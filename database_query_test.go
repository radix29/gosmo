package gosmo

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"io"
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

// TestDatabaseQueryReleasesConnection guards against the connection leak in
// Database.query: closing the *dbRows it returns must also release the
// *sql.Conn pinned for the query back to the pool (sql.DB.Stats().InUse back
// to 0), not just the query's own driver-level resources. Before dbRows
// existed, Database.query returned a bare *sql.Rows and every acquired
// *sql.Conn stayed checked out forever — verified live against a real SQL
// Server: the pool was exhausted (and every further read timed out) after as
// few as maxOpenConns reads.
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
