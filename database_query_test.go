package gosmo

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
)

// -- fake driver: just enough to exercise Database.query's acquire/USE/query
// path without a real network connection --------------------------------

type fakeQueryDriver struct{}

func (fakeQueryDriver) Open(name string) (driver.Conn, error) { return &fakeQueryConn{}, nil }

type fakeQueryConn struct{}

func (c *fakeQueryConn) Prepare(query string) (driver.Stmt, error) { return nil, driver.ErrSkip }
func (c *fakeQueryConn) Close() error                              { return nil }
func (c *fakeQueryConn) Begin() (driver.Tx, error)                 { return nil, driver.ErrSkip }

func (c *fakeQueryConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	return driver.ResultNoRows, nil
}

func (c *fakeQueryConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	return &fakeQueryRows{}, nil
}

// fakeQueryRows yields a single ("x") row, then EOF.
type fakeQueryRows struct{ done bool }

func (r *fakeQueryRows) Columns() []string { return []string{"name"} }
func (r *fakeQueryRows) Close() error      { return nil }
func (r *fakeQueryRows) Next(dest []driver.Value) error {
	if r.done {
		return io.EOF
	}
	r.done = true
	dest[0] = "x"
	return nil
}

func init() {
	sql.Register("fakequery", fakeQueryDriver{})
}

// TestDatabaseQueryReleasesConnection guards against a connection leak in
// Database.query: closing the *dbRows it returns must also release the
// *sql.Conn pinned for the query back to the pool (sql.DB.Stats().InUse
// back to 0), not just the query's own driver-level resources. Returning a
// bare *sql.Rows instead leaves every acquired *sql.Conn checked out
// forever, exhausting the pool after as few as maxOpenConns reads.
func TestDatabaseQueryReleasesConnection(t *testing.T) {
	db, err := sql.Open("fakequery", "")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(2)

	srv := &Server{db: db}
	d := &Database{server: srv, name: "test"}

	ctx := context.Background()
	for i := range 5 {
		rows, err := d.query(ctx, "SELECT name FROM sys.tables")
		if err != nil {
			t.Fatalf("query %d: %v", i, err)
		}
		for rows.Next() {
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("query %d: rows.Err: %v", i, err)
		}
		if err := rows.Close(); err != nil {
			t.Fatalf("query %d: rows.Close: %v", i, err)
		}
		if inUse := db.Stats().InUse; inUse != 0 {
			t.Fatalf("query %d: db.Stats().InUse = %d, want 0 (pinned connection was never released)", i, inUse)
		}
	}
}

// splitUseBatch undoes Database.useBatch for a fake driver: the USE statement
// and the query behind it. ok is false for a statement that is not one.
func splitUseBatch(q string) (use, rest string, ok bool) {
	const guard = "; IF @@ERROR <> 0 RETURN; "
	if !strings.HasPrefix(q, "USE ") {
		return "", q, false
	}
	i := strings.Index(q, guard)
	if i < 0 {
		return "", q, false
	}
	return q[:i], q[i+len(guard):], true
}

// -- useDriver: records every statement and fails the ones a test names ------

type useDriverState struct {
	mu        sync.Mutex
	stmts     []string
	failBatch error // returned for a useBatch statement
	failUse   error // returned for a bare USE
	failQuery error // returned for the bare query
}

var useState *useDriverState

type useDriver struct{}

func (useDriver) Open(string) (driver.Conn, error) { return &useConn{}, nil }

type useConn struct{}

func (c *useConn) Prepare(string) (driver.Stmt, error) { return nil, driver.ErrSkip }
func (c *useConn) Close() error                        { return nil }
func (c *useConn) Begin() (driver.Tx, error)           { return nil, driver.ErrSkip }

func (c *useConn) record(q string) error {
	useState.mu.Lock()
	defer useState.mu.Unlock()
	useState.stmts = append(useState.stmts, q)
	if _, _, ok := splitUseBatch(q); ok {
		return useState.failBatch
	}
	if strings.HasPrefix(q, "USE ") {
		return useState.failUse
	}
	return useState.failQuery
}

func (c *useConn) ExecContext(_ context.Context, q string, _ []driver.NamedValue) (driver.Result, error) {
	if err := c.record(q); err != nil {
		return nil, err
	}
	return driver.ResultNoRows, nil
}

func (c *useConn) QueryContext(_ context.Context, q string, _ []driver.NamedValue) (driver.Rows, error) {
	if err := c.record(q); err != nil {
		return nil, err
	}
	return &fakeQueryRows{}, nil
}

func init() { sql.Register("gosmousebatch", useDriver{}) }

func useTestDB(t *testing.T) *Database {
	t.Helper()
	useState = &useDriverState{}
	db, err := sql.Open("gosmousebatch", "")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return &Database{server: &Server{db: db}, name: "App]DB"}
}

// readBoth runs one read through query and one through queryRow, returning
// their errors.
func readBoth(d *Database) (qErr, rowErr error) {
	ctx := context.Background()
	rows, err := d.query(ctx, "SELECT name FROM sys.tables")
	if err == nil {
		rows.Close()
	}
	var name string
	return err, d.queryRow(ctx, func(r *sql.Row) error { return r.Scan(&name) }, "SELECT name FROM sys.tables")
}

// A read is one round trip: the switch to the database and the query go as
// one batch, guarded so a failed USE stops it, with the query on the batch's
// first line so its error line numbers are unchanged.
func TestDatabaseReadIsOneBatch(t *testing.T) {
	d := useTestDB(t)
	if qErr, rowErr := readBoth(d); qErr != nil || rowErr != nil {
		t.Fatalf("query: %v, queryRow: %v", qErr, rowErr)
	}
	want := "USE [App]]DB]; IF @@ERROR <> 0 RETURN; SELECT name FROM sys.tables"
	if len(useState.stmts) != 2 || useState.stmts[0] != want || useState.stmts[1] != want {
		t.Errorf("statements = %q, want exactly two of %q", useState.stmts, want)
	}
}

// A batch whose USE fails must still report the error every database-scoped
// call always has — wrapped as a USE failure, with the server's own error
// reachable underneath — which the batch cannot tell apart from a failure in
// the query. So a failing batch is replayed as the two statements.
func TestDatabaseReadReportsAFailedUseAsBefore(t *testing.T) {
	d := useTestDB(t)
	serverErr := errors.New("Database 'App]DB' does not exist. (911)")
	useState.failBatch, useState.failUse = serverErr, serverErr

	qErr, rowErr := readBoth(d)
	for name, err := range map[string]error{"query": qErr, "queryRow": rowErr} {
		if err == nil || err.Error() != "gosmo: USE App]DB: "+serverErr.Error() || !errors.Is(err, serverErr) {
			t.Errorf("%s error = %v, want %q wrapping the server's", name, err, "gosmo: USE App]DB: "+serverErr.Error())
		}
	}
	for _, s := range useState.stmts {
		if s == "SELECT name FROM sys.tables" {
			t.Errorf("the bare query ran after its USE failed: %q", useState.stmts)
		}
	}
}

// A batch that fails in the query half reports the query's own error,
// unwrapped, exactly as the query alone did.
func TestDatabaseReadReportsAFailedQueryAsBefore(t *testing.T) {
	d := useTestDB(t)
	serverErr := errors.New("Invalid column name 'x'. (207)")
	useState.failBatch, useState.failQuery = serverErr, serverErr

	qErr, rowErr := readBoth(d)
	for name, err := range map[string]error{"query": qErr, "queryRow": rowErr} {
		if err == nil || err.Error() != serverErr.Error() {
			t.Errorf("%s error = %v, want the query's own %q", name, err, serverErr)
		}
	}
}
