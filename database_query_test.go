package gosmo

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"slices"
	"strings"
	"sync"
	"testing"

	mssql "github.com/microsoft/go-mssqldb"
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
	d := &Database{server: srv, Name: "test"}

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
	before, after, ok := strings.Cut(q, guard)
	if !ok {
		return "", q, false
	}
	return before, after, true
}

// unwrapProcRead undoes Database.procRead for a fake driver: the database the
// call runs in (bracketed, as a USE names it), the query it carries and that
// query's own arguments, numbered from 1 again. ok is false for a statement
// that is not one, and for recheckEntry's empty call, which carries no query
// worth recording.
func unwrapProcRead(q string, args []driver.NamedValue) (db, inner string, innerArgs []driver.NamedValue, ok bool) {
	db, found := strings.CutSuffix(q, ".sys.sp_executesql")
	if !found || len(args) == 0 {
		return "", q, args, false
	}
	inner, _ = args[0].Value.(string)
	if len(args) > 2 {
		innerArgs = make([]driver.NamedValue, len(args)-2)
		for i, a := range args[2:] {
			a.Ordinal = i + 1
			innerArgs[i] = a
		}
	}
	return db, inner, innerArgs, true
}

// -- useDriver: records every statement and fails the ones a test names ------

type useDriverState struct {
	mu        sync.Mutex
	stmts     []string
	failBatch error // returned for a useBatch statement
	failUse   error // returned for a bare USE
	failQuery error // returned for the bare query, and for procRead's call
	failEntry error // returned for recheckEntry's empty call of sp_executesql
	// batchErr, when set, decides a useBatch statement's error from its text
	// before failBatch does — a model of the server rather than a fixed reply.
	batchErr func(q string) error
}

var useState *useDriverState

type useDriver struct{}

func (useDriver) Open(string) (driver.Conn, error) { return &useConn{}, nil }

type useConn struct{}

func (c *useConn) Prepare(string) (driver.Stmt, error) { return nil, driver.ErrSkip }
func (c *useConn) Close() error                        { return nil }
func (c *useConn) Begin() (driver.Tx, error)           { return useTx{}, nil }

// useTx is a transaction that does nothing, so a read can run inside
// InTransaction — the one place useBatch still goes.
type useTx struct{}

func (useTx) Commit() error   { return nil }
func (useTx) Rollback() error { return nil }

func (c *useConn) record(q string) error {
	useState.mu.Lock()
	defer useState.mu.Unlock()
	useState.stmts = append(useState.stmts, q)
	if _, rest, ok := splitUseBatch(q); ok {
		if useState.batchErr != nil {
			if err := useState.batchErr(rest); err != nil {
				return err
			}
		}
		return useState.failBatch
	}
	if strings.HasPrefix(q, "USE ") {
		return useState.failUse
	}
	return useState.failQuery
}

func (c *useConn) ExecContext(_ context.Context, q string, _ []driver.NamedValue) (driver.Result, error) {
	if strings.HasSuffix(q, ".sys.sp_executesql") {
		useState.mu.Lock()
		useState.stmts = append(useState.stmts, q)
		useState.mu.Unlock()
		if useState.failEntry != nil {
			return nil, useState.failEntry
		}
		return driver.ResultNoRows, nil
	}
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
	return &Database{server: &Server{db: db}, Name: "App]DB"}
}

// readBoth runs one read through query and one through queryRow, returning
// their errors.
func readBoth(d *Database) (qErr, rowErr error) {
	return readBothCtx(context.Background(), d)
}

func readBothCtx(ctx context.Context, d *Database) (qErr, rowErr error) {
	rows, err := d.query(ctx, "SELECT name FROM sys.tables")
	if err == nil {
		rows.Close()
	}
	var name string
	return err, d.queryRow(ctx, func(r *sql.Row) error { return r.Scan(&name) }, "SELECT name FROM sys.tables")
}

// readBothInTx is readBoth inside InTransaction, the one place a read still
// goes as a useBatch. set arms the failures once the transaction has begun,
// so its own SELECT DB_NAME() is not failed, and the statements are recorded
// from there. InTransaction's own result is not the test's: a failed read
// leaves it refusing to commit.
func readBothInTx(d *Database, set func()) (qErr, rowErr error) {
	_ = d.server.InTransaction(context.Background(), func(ctx context.Context) error {
		useState.stmts = nil
		if set != nil {
			set()
		}
		qErr, rowErr = readBothCtx(ctx, d)
		return nil
	})
	return qErr, rowErr
}

// A read inside InTransaction is one round trip: the switch to the database
// and the query go as one batch, guarded so a failed USE stops it, with the
// query on the batch's first line so its error line numbers are unchanged.
func TestDatabaseReadIsOneBatch(t *testing.T) {
	d := useTestDB(t)
	if qErr, rowErr := readBothInTx(d, nil); qErr != nil || rowErr != nil {
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
// the query. So a failing batch runs the USE alone to tell them apart.
func TestDatabaseReadReportsAFailedUseAsBefore(t *testing.T) {
	d := useTestDB(t)
	serverErr := errors.New("Database 'App]DB' does not exist. (911)")
	qErr, rowErr := readBothInTx(d, func() { useState.failBatch, useState.failUse = serverErr, serverErr })
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
	qErr, rowErr := readBothInTx(d, func() { useState.failBatch, useState.failQuery = serverErr, serverErr })
	for name, err := range map[string]error{"query": qErr, "queryRow": rowErr} {
		if err == nil || err.Error() != serverErr.Error() {
			t.Errorf("%s error = %v, want the query's own %q", name, err, serverErr)
		}
	}
}

// A failing read inside InTransaction costs two round trips, never three, and
// the query runs once: the batch, then the USE alone to tell which half
// failed. A read that succeeds is the batch alone.
func TestDatabaseReadFailureRunsQueryOnce(t *testing.T) {
	const bare = "SELECT name FROM sys.tables"
	cases := []struct {
		name               string
		failUse, failQuery bool
	}{
		{"success", false, false},
		{"USE fails", true, false},
		{"query fails", false, true},
		{"both fail", true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := useTestDB(t)
			serverErr := errors.New("refused")
			arm := func() {
				if tc.failUse {
					useState.failUse = serverErr
				}
				if tc.failUse || tc.failQuery {
					useState.failBatch = serverErr
				}
			}
			for name, read := range map[string]func(ctx context.Context) error{
				"query": func(ctx context.Context) error {
					rows, err := d.query(ctx, bare)
					if err == nil {
						rows.Close()
					}
					return err
				},
				"queryRow": func(ctx context.Context) error {
					var s string
					return d.queryRow(ctx, func(r *sql.Row) error { return r.Scan(&s) }, bare)
				},
			} {
				useState.failUse, useState.failBatch = nil, nil
				var err error
				_ = d.server.InTransaction(context.Background(), func(ctx context.Context) error {
					useState.stmts = nil
					arm()
					err = read(ctx)
					return nil
				})
				want := 2
				if !tc.failUse && !tc.failQuery {
					want = 1
				}
				if len(useState.stmts) != want {
					t.Errorf("%s sent %d statements %q, want %d", name, len(useState.stmts), useState.stmts, want)
				}
				if slices.Contains(useState.stmts, bare) {
					t.Errorf("%s ran the query again on its own: %q", name, useState.stmts)
				}
				if (err != nil) != (tc.failUse || tc.failQuery) {
					t.Errorf("%s error = %v", name, err)
				}
			}
		})
	}
}

// A collation conflict can be the batch's own doing: its parameters take the
// collation of the database the batch started in, not d's. So inside
// InTransaction, after the USE alone succeeds, a 468 runs the query again on
// its own — once — and that read's result is the read's. A pooled read
// (procRead) binds its parameters in d and never sees it.
func TestDatabaseReadReRunsACollationConflictAlone(t *testing.T) {
	const bare = "SELECT name FROM sys.tables"
	d := useTestDB(t)
	conflict := mssql.Error{Number: 468, Message: "Cannot resolve the collation conflict"}
	qErr, rowErr := readBothInTx(d, func() { useState.failBatch = conflict })
	for name, err := range map[string]error{"query": qErr, "queryRow": rowErr} {
		if err != nil {
			t.Errorf("%s error = %v, want the bare query's success", name, err)
		}
	}
	want := []string{d.useBatch(bare), "USE [App]]DB]", bare}
	if got := useState.stmts; !slices.Equal(got, slices.Concat(want, want)) {
		t.Errorf("statements = %q, want %q twice", got, want)
	}

	// The same conflict from the bare query is the read's error, not a loop.
	qErr, rowErr = readBothInTx(d, func() { useState.failBatch, useState.failQuery = conflict, conflict })
	for name, err := range map[string]error{"query": qErr, "queryRow": rowErr} {
		if me, ok := errors.AsType[mssql.Error](err); !ok || me.Number != 468 {
			t.Errorf("%s error = %v, want the bare query's 468", name, err)
		}
	}
	if len(useState.stmts) != 6 {
		t.Errorf("statements = %q, want three per read", useState.stmts)
	}
}

// A refusal inside InTransaction has already ended the transaction, so the
// USE recheckUse sends never reaches the server: the driver answers it with
// its own Number 0. The read then reports the server's refusal, not that.
func TestDatabaseReadInAnAbortedTransactionKeepsTheServersError(t *testing.T) {
	d := useTestDB(t)
	refused := mssql.Error{Number: 911, Message: "Database 'App]DB' does not exist."}
	aborted := mssql.Error{Number: 0, Message: "server does not have an active transaction"}
	qErr, rowErr := readBothInTx(d, func() { useState.failBatch, useState.failUse = refused, aborted })
	for name, err := range map[string]error{"query": qErr, "queryRow": rowErr} {
		if me, ok := errors.AsType[mssql.Error](err); !ok || me.Number != 911 {
			t.Errorf("%s error = %v, want the server's 911", name, err)
		}
	}
}
