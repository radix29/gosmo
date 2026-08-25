package gosmo

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"io"
	"strings"
	"testing"
)

// -- fake driver returning scripted showplan result sets ---------------------

// fakePlanSet is one result set the fake connection hands back: its column
// names and the single-column rows under them.
type fakePlanSet struct {
	cols []string
	rows []string
}

// fakePlanScript is the answer the next QueryContext will give. It is package
// state because database/sql owns connection creation; every test that uses
// the driver sets it before opening a pool.
var fakePlanScript []fakePlanSet

type fakePlanDriver struct{}

func (fakePlanDriver) Open(string) (driver.Conn, error) { return &fakePlanConn{}, nil }

type fakePlanConn struct{}

func (c *fakePlanConn) Prepare(string) (driver.Stmt, error) { return nil, driver.ErrSkip }
func (c *fakePlanConn) Close() error                        { return nil }
func (c *fakePlanConn) Begin() (driver.Tx, error)           { return nil, driver.ErrSkip }

func (c *fakePlanConn) ExecContext(context.Context, string, []driver.NamedValue) (driver.Result, error) {
	return driver.RowsAffected(0), nil
}

func (c *fakePlanConn) QueryContext(_ context.Context, q string, _ []driver.NamedValue) (driver.Rows, error) {
	// loadInfo's two queries run once when NewServer builds the *Server;
	// only the batch under test gets the scripted plan sets.
	if strings.Contains(q, "SERVERPROPERTY") || strings.Contains(q, "dm_os_sys_info") {
		return fakeInfoAnswer(q, nil)
	}
	return &fakePlanRows{sets: fakePlanScript}, nil
}

type fakePlanRows struct {
	sets []fakePlanSet
	set  int
	row  int
}

func (r *fakePlanRows) Columns() []string { return r.sets[r.set].cols }
func (r *fakePlanRows) Close() error      { return nil }

func (r *fakePlanRows) Next(dest []driver.Value) error {
	s := r.sets[r.set]
	if r.row >= len(s.rows) {
		return io.EOF
	}
	dest[0] = s.rows[r.row]
	r.row++
	return nil
}

func (r *fakePlanRows) HasNextResultSet() bool { return r.set+1 < len(r.sets) }

func (r *fakePlanRows) NextResultSet() error {
	if !r.HasNextResultSet() {
		return io.EOF
	}
	r.set++
	r.row = 0
	return nil
}

func init() { sql.Register("fakeplan", fakePlanDriver{}) }

// planTestServer opens a *Server over the fake driver with script installed.
func planTestServer(t *testing.T, script []fakePlanSet) *Server {
	t.Helper()
	fakePlanScript = script
	pool, err := sql.Open("fakeplan", "")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { pool.Close() })
	s, err := NewServer(context.Background(), pool)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	return s
}

const planCol = showplanColumn

// TestCapturePlanKeepsEveryRowOfOneShowplanSet pins the SHOWPLAN_XML shape: a
// three-statement batch comes back as three rows of one result set, and every
// one of them is a statement's plan. Keeping only the last was the defect.
func TestCapturePlanKeepsEveryRowOfOneShowplanSet(t *testing.T) {
	s := planTestServer(t, []fakePlanSet{
		{cols: []string{planCol}, rows: []string{"<p1/>", "<p2/>", "<p3/>"}},
	})
	plan, err := s.Database("tempdb").EstimatedPlanContext(context.Background(), "batch")
	if err != nil {
		t.Fatalf("EstimatedPlanContext: %v", err)
	}
	want := []string{"<p1/>", "<p2/>", "<p3/>"}
	if len(plan.All) != len(want) {
		t.Fatalf("All = %v, want %v", plan.All, want)
	}
	for i := range want {
		if plan.All[i] != want[i] {
			t.Errorf("All[%d] = %q, want %q", i, plan.All[i], want[i])
		}
	}
	if plan.XML != "<p3/>" {
		t.Errorf("XML = %q, want the last plan %q", plan.XML, "<p3/>")
	}
}

// TestCapturePlanKeepsEveryShowplanResultSet pins the STATISTICS XML shape:
// each statement's own result set, then its plan in a set of its own.
func TestCapturePlanKeepsEveryShowplanResultSet(t *testing.T) {
	s := planTestServer(t, []fakePlanSet{
		{cols: []string{"id"}, rows: []string{"row"}},
		{cols: []string{planCol}, rows: []string{"<p1/>"}},
		{cols: []string{"id"}, rows: []string{"row"}},
		{cols: []string{planCol}, rows: []string{"<p2/>"}},
	})
	plan, err := s.Database("tempdb").ActualPlanContext(context.Background(), "batch")
	if err != nil {
		t.Fatalf("ActualPlanContext: %v", err)
	}
	if len(plan.All) != 2 || plan.All[0] != "<p1/>" || plan.All[1] != "<p2/>" {
		t.Fatalf("All = %v, want [<p1/> <p2/>]", plan.All)
	}
	if plan.XML != "<p2/>" {
		t.Errorf("XML = %q, want %q", plan.XML, "<p2/>")
	}
}

// TestCapturePlanIgnoresNonPlanResultSets checks the column-name gate still
// decides what counts: a single-column set that is not the showplan column
// must not be collected as a plan.
func TestCapturePlanIgnoresNonPlanResultSets(t *testing.T) {
	s := planTestServer(t, []fakePlanSet{
		{cols: []string{"name"}, rows: []string{"not a plan"}},
		{cols: []string{planCol}, rows: []string{"<p1/>"}},
	})
	plan, err := s.Database("tempdb").ActualPlanContext(context.Background(), "batch")
	if err != nil {
		t.Fatalf("ActualPlanContext: %v", err)
	}
	if len(plan.All) != 1 || plan.All[0] != "<p1/>" {
		t.Fatalf("All = %v, want [<p1/>]", plan.All)
	}
}

// TestCapturePlanErrorsWhenNoPlanCameBack keeps the empty case an error
// rather than an ExecutionPlan holding nothing.
func TestCapturePlanErrorsWhenNoPlanCameBack(t *testing.T) {
	s := planTestServer(t, []fakePlanSet{
		{cols: []string{"name"}, rows: []string{"not a plan"}},
	})
	if _, err := s.Database("tempdb").EstimatedPlanContext(context.Background(), "batch"); err == nil {
		t.Fatal("want an error when no showplan set came back, got nil")
	}
}
