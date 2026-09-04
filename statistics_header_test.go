package gosmo

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"io"
	"testing"
)

// DBCC SHOW_STATISTICS ... WITH STAT_HEADER returns 10 columns before SQL
// Server 2019 and 11 from it, and its shape is not a documented contract, so
// HeaderContext binds by column name. These fakes serve whichever shape the
// test asks for.

type statHeaderDriver struct{}

// statHeaderShape is the result the next Open'd connection serves. The driver
// interface gives a query no other channel, and only one test runs at a time
// against each registered name.
var statHeaderShape struct {
	cols []string
	vals []driver.Value
}

func (statHeaderDriver) Open(string) (driver.Conn, error) { return statHeaderConn{}, nil }

type statHeaderConn struct{}

func (statHeaderConn) Prepare(string) (driver.Stmt, error) { return nil, driver.ErrSkip }
func (statHeaderConn) Close() error                        { return nil }
func (statHeaderConn) Begin() (driver.Tx, error)           { return nil, driver.ErrSkip }

func (statHeaderConn) ExecContext(context.Context, string, []driver.NamedValue) (driver.Result, error) {
	return driver.ResultNoRows, nil
}

func (statHeaderConn) QueryContext(_ context.Context, q string, _ []driver.NamedValue) (driver.Rows, error) {
	return &statHeaderRows{cols: statHeaderShape.cols, vals: statHeaderShape.vals}, nil
}

type statHeaderRows struct {
	cols []string
	vals []driver.Value
	done bool
}

func (r *statHeaderRows) Columns() []string { return r.cols }
func (r *statHeaderRows) Close() error      { return nil }
func (r *statHeaderRows) Next(dest []driver.Value) error {
	if r.done {
		return io.EOF
	}
	r.done = true
	copy(dest, r.vals)
	return nil
}

func init() { sql.Register("statheader", statHeaderDriver{}) }

func statHeaderFor(t *testing.T, cols []string, vals []driver.Value) *StatisticHeader {
	t.Helper()
	statHeaderShape.cols, statHeaderShape.vals = cols, vals
	db, err := sql.Open("statheader", "")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	st := &Statistic{
		Name:  "IX_t",
		table: &Table{db: &Database{server: &Server{db: db}, name: "d"}, Schema: "dbo", Name: "t"},
	}
	h, err := st.HeaderContext(context.Background())
	if err != nil {
		t.Fatalf("HeaderContext: %v", err)
	}
	return h
}

// The 2019+ shape: every field is filled from its own column.
func TestStatisticHeaderReadsTheElevenColumnShape(t *testing.T) {
	h := statHeaderFor(t,
		[]string{"Name", "Updated", "Rows", "Rows Sampled", "Steps", "Density",
			"Average key length", "String Index", "Filter Expression",
			"Unfiltered Rows", "Persisted Sample Percent"},
		[]driver.Value{"IX_t", "Jan 1 2026", int64(100), int64(90), int64(7), 0.25,
			8.0, "NO", "([id]>(0))", int64(120), 55.0})

	want := StatisticHeader{
		Updated: "Jan 1 2026", Rows: 100, RowsSampled: 90, Steps: 7,
		Density: 0.25, AverageKeyLength: 8, StringIndex: "NO",
		FilterExpression: "([id]>(0))", UnfilteredRows: 120,
		PersistedSamplePercent: 55,
	}
	if *h != want {
		t.Errorf("header = %+v, want %+v", *h, want)
	}
}

// The pre-2019 shape. A fixed 11-destination Scan failed here with
// "expected 10 destination arguments in Scan, not 11", which killed
// Statistics Properties outright on 2016 and 2017.
func TestStatisticHeaderReadsTheTenColumnShape(t *testing.T) {
	h := statHeaderFor(t,
		[]string{"Name", "Updated", "Rows", "Rows Sampled", "Steps", "Density",
			"Average key length", "String Index", "Filter Expression", "Unfiltered Rows"},
		[]driver.Value{"IX_t", "Jan 1 2026", int64(100), int64(90), int64(7), 0.25,
			8.0, "NO", "([id]>(0))", int64(120)})

	want := StatisticHeader{
		Updated: "Jan 1 2026", Rows: 100, RowsSampled: 90, Steps: 7,
		Density: 0.25, AverageKeyLength: 8, StringIndex: "NO",
		FilterExpression: "([id]>(0))", UnfilteredRows: 120,
	}
	if *h != want {
		t.Errorf("header = %+v, want %+v", *h, want)
	}
	if h.PersistedSamplePercent != 0 {
		t.Errorf("PersistedSamplePercent = %v on a header that has no such column, want 0", h.PersistedSamplePercent)
	}
}

// Binding is by name, not by position: a reordered header with a column gosmo
// does not know still lands every value in the right field. A positional read
// passes the ten-column test and fails this one.
func TestStatisticHeaderBindsByNameNotPosition(t *testing.T) {
	h := statHeaderFor(t,
		[]string{"Unfiltered Rows", "Some Future Column", "Name", "Steps", "Updated",
			"Rows Sampled", "Density", "Average key length", "String Index",
			"Filter Expression", "Rows", "Persisted Sample Percent"},
		[]driver.Value{int64(120), "ignored", "IX_t", int64(7), "Jan 1 2026",
			int64(90), 0.25, 8.0, "NO", "([id]>(0))", int64(100), 55.0})

	want := StatisticHeader{
		Updated: "Jan 1 2026", Rows: 100, RowsSampled: 90, Steps: 7,
		Density: 0.25, AverageKeyLength: 8, StringIndex: "NO",
		FilterExpression: "([id]>(0))", UnfilteredRows: 120,
		PersistedSamplePercent: 55,
	}
	if *h != want {
		t.Errorf("header = %+v, want %+v", *h, want)
	}
}

// A header row of NULLs is what a statistic that has never been populated
// returns; every field stays zero and it is not an error.
func TestStatisticHeaderAcceptsAnAllNullRow(t *testing.T) {
	h := statHeaderFor(t,
		[]string{"Name", "Updated", "Rows", "Rows Sampled", "Steps", "Density",
			"Average key length", "String Index", "Filter Expression",
			"Unfiltered Rows", "Persisted Sample Percent"},
		[]driver.Value{"IX_t", nil, nil, nil, nil, nil, nil, nil, nil, nil, nil})

	if *h != (StatisticHeader{}) {
		t.Errorf("header = %+v, want the zero value", *h)
	}
}
