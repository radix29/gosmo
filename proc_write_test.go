package gosmo

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"

	mssql "github.com/microsoft/go-mssqldb"
)

// -- writeDriver: records each call as the server sees it -------------------
//
// A pooled write is an RPC call of [db].sys.sp_executesql (execIn), ExecProc
// a call of [db].[schema].[name], and withSessionIn switches a session to the
// database and back. These tests pin what each sends; live_proc_write_test.go
// shows what the server makes of it.

type writeCall struct {
	query string
	args  []any
}

type writeDriverState struct {
	mu        sync.Mutex
	calls     []writeCall
	closed    int   // driver connections closed
	failCall  error // a write's call of sp_executesql, or ExecProc's
	failEntry error // recheckEntry's empty call
	failUse   error // a bare USE
}

var writeState *writeDriverState

type writeDriver struct{}

func (writeDriver) Open(string) (driver.Conn, error) { return &writeConn{}, nil }

type writeConn struct{}

func (*writeConn) Prepare(string) (driver.Stmt, error) { return nil, driver.ErrSkip }
func (*writeConn) Begin() (driver.Tx, error)           { return useTx{}, nil }
func (*writeConn) Close() error {
	writeState.mu.Lock()
	defer writeState.mu.Unlock()
	writeState.closed++
	return nil
}

func (*writeConn) record(q string, args []driver.NamedValue) error {
	writeState.mu.Lock()
	defer writeState.mu.Unlock()
	vals := make([]any, len(args))
	for i, a := range args {
		vals[i] = a.Value
	}
	writeState.calls = append(writeState.calls, writeCall{q, vals})
	switch {
	case strings.HasSuffix(q, ".sys.sp_executesql") && len(args) == 1 && args[0].Value == "SELECT NULL":
		return writeState.failEntry
	case strings.HasPrefix(q, "USE "):
		return writeState.failUse
	case strings.HasPrefix(q, "DECLARE @home"):
		return nil
	}
	return writeState.failCall
}

func (c *writeConn) ExecContext(_ context.Context, q string, args []driver.NamedValue) (driver.Result, error) {
	if err := c.record(q, args); err != nil {
		return nil, err
	}
	return driver.ResultNoRows, nil
}

func (c *writeConn) QueryContext(_ context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	if err := c.record(q, args); err != nil {
		return nil, err
	}
	return &writeRows{}, nil
}

// writeRows is one row of one column, "home" — withSessionIn's login
// database, or a NEXT VALUE FOR the scan converts.
type writeRows struct{ done bool }

func (*writeRows) Columns() []string { return []string{"c"} }
func (*writeRows) Close() error      { return nil }
func (r *writeRows) Next(dest []driver.Value) error {
	if r.done {
		return io.EOF
	}
	r.done = true
	dest[0] = "7"
	return nil
}

func init() { sql.Register("gosmowrite", writeDriver{}) }

func writeTestDB(t *testing.T) (*Database, *sql.DB) {
	t.Helper()
	writeState = &writeDriverState{}
	db, err := sql.Open("gosmowrite", "")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return &Database{server: &Server{db: db}, Name: "App]DB"}, db
}

func (s *writeDriverState) queries() []string {
	var qs []string
	for _, c := range s.calls {
		qs = append(qs, c.query)
	}
	return qs
}

// A pooled write is one call of the database's own sp_executesql — the
// statement, its parameters declared as procRead declares a read's, and the
// values — with no USE before or after it.
func TestPooledWriteIsOneProcCall(t *testing.T) {
	d, _ := writeTestDB(t)
	const q = "EXEC sp_rename @p1, @p2"
	if _, err := d.exec(context.Background(), q, "dbo.a", "b"); err != nil {
		t.Fatalf("exec: %v", err)
	}
	want := writeCall{"[App]]DB].sys.sp_executesql", []any{q, "@p1 nvarchar(4000), @p2 nvarchar(4000)", "dbo.a", "b"}}
	if len(writeState.calls) != 1 || fmt.Sprint(writeState.calls[0]) != fmt.Sprint(want) {
		t.Errorf("calls = %v, want exactly %v", writeState.calls, want)
	}
}

// NextValue and Shrink read the row their write returns, through the same
// call.
func TestPooledWriteReturningARowIsOneProcCall(t *testing.T) {
	d, _ := writeTestDB(t)
	if _, err := d.SequenceRef("dbo", "s").NextValue(context.Background()); err != nil {
		t.Fatalf("next value: %v", err)
	}
	want := writeCall{"[App]]DB].sys.sp_executesql", []any{"SELECT NEXT VALUE FOR [dbo].[s]"}}
	if len(writeState.calls) != 1 || fmt.Sprint(writeState.calls[0]) != fmt.Sprint(want) {
		t.Errorf("calls = %v, want exactly %v", writeState.calls, want)
	}
}

// A write that fails because the database refused the call reports it as the
// USE failure every database-scoped call always has; one that fails on its
// own reports its own error — the same split recheckEntry makes for a read.
func TestPooledWriteTellsARefusalFromItsOwnError(t *testing.T) {
	refused := mssql.Error{Number: 911, Message: "Database 'App]DB' does not exist."}
	other := mssql.Error{Number: 942, Message: "Database 'Other' cannot be opened because it is offline."}
	own := mssql.Error{Number: 3701, Message: "Cannot drop the table 't'."}
	for _, tc := range []struct {
		name            string
		callErr, entErr error
		want            string
	}{
		{"database refuses", refused, refused, "gosmo: USE App]DB: " + refused.Error()},
		{"statement reaches a refusing database", other, nil, other.Error()},
		{"statement fails", own, nil, own.Error()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, _ := writeTestDB(t)
			writeState.failCall, writeState.failEntry = tc.callErr, tc.entErr
			_, err := d.exec(context.Background(), "DROP TABLE t")
			if err == nil || err.Error() != tc.want {
				t.Errorf("exec error = %v, want %q", err, tc.want)
			}
		})
	}
}

// Inside InTransaction the session is the transaction's, switched to the
// database (serverTx.at), and the statement goes as itself. The session is
// switched back to where the transaction began before COMMIT or ROLLBACK
// returns it to the pool.
func TestWriteInTransactionRunsInTheSwitchedSession(t *testing.T) {
	for _, commit := range []bool{true, false} {
		t.Run(fmt.Sprintf("commit=%v", commit), func(t *testing.T) {
			d, _ := writeTestDB(t)
			err := d.server.InTransaction(context.Background(), func(ctx context.Context) error {
				writeState.calls = nil
				if _, err := d.exec(ctx, "DROP TABLE t"); err != nil {
					return err
				}
				if !commit {
					return errors.New("roll back")
				}
				return nil
			})
			if commit != (err == nil) {
				t.Fatalf("InTransaction: %v", err)
			}
			// writeRows answers SELECT DB_NAME() with "7": the home database.
			if got, want := writeState.queries(), []string{"USE [App]]DB]", "DROP TABLE t", "USE [7]"}; fmt.Sprint(got) != fmt.Sprint(want) {
				t.Errorf("statements = %q, want %q", got, want)
			}
		})
	}
}

// ExecProc names the procedure with the database's name, so the procedure
// runs there with no USE, and a refusal is reported as one.
func TestExecProcCallsTheProcedureByThreePartName(t *testing.T) {
	d, _ := writeTestDB(t)
	if _, err := d.ExecProc(context.Background(), "dbo", "p", In("a", 1)); err != nil {
		t.Fatalf("ExecProc: %v", err)
	}
	if got, want := writeState.queries(), []string{"[App]]DB].[dbo].[p]"}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("calls = %q, want %q", got, want)
	}

	refused := mssql.Error{Number: 942, Message: "Database 'App]DB' cannot be opened because it is offline."}
	writeState.calls, writeState.failCall, writeState.failEntry = nil, refused, refused
	_, err := d.ExecProc(context.Background(), "dbo", "p")
	if want := "gosmo: exec proc [dbo].[p]: gosmo: USE App]DB: " + refused.Error(); err == nil || err.Error() != want {
		t.Errorf("ExecProc error = %v, want %q", err, want)
	}
}

// An argument procRead cannot declare refuses the write before anything is
// sent.
func TestPooledWriteRefusesAnUndeclarableArgument(t *testing.T) {
	d, _ := writeTestDB(t)
	_, err := d.exec(context.Background(), "UPDATE t SET a = @p1", struct{}{})
	if !errors.Is(err, ErrUnsupported) {
		t.Errorf("exec error = %v, want ErrUnsupported", err)
	}
	if len(writeState.calls) != 0 {
		t.Errorf("calls = %v, want none", writeState.calls)
	}
}

// withSessionIn switches the session to the database, runs fn there, and
// switches it back to the database it came from, so it goes back to the pool
// where it started. A session that cannot be switched back is discarded.
func TestSessionInSwitchesBack(t *testing.T) {
	for _, failBack := range []bool{false, true} {
		t.Run(fmt.Sprintf("switch back fails=%v", failBack), func(t *testing.T) {
			d, db := writeTestDB(t)
			if failBack {
				writeState.failUse = errors.New("Database '7' does not exist. (911)")
			}
			err := d.withSessionIn(context.Background(), func(ctx context.Context, c *sql.Conn) error {
				_, err := c.ExecContext(ctx, "INSERT BULK [dbo].[t]")
				return err
			})
			if err != nil {
				t.Fatalf("withSessionIn: %v", err)
			}
			want := []string{
				"DECLARE @home sysname = DB_NAME(); USE [App]]DB]; IF @@ERROR <> 0 RETURN; SELECT @home",
				"INSERT BULK [dbo].[t]",
				"USE [7]",
			}
			if got := writeState.queries(); fmt.Sprint(got) != fmt.Sprint(want) {
				t.Errorf("statements = %q, want %q", got, want)
			}
			if open := db.Stats().OpenConnections; failBack != (open == 0) {
				t.Errorf("%d connection(s) open after the session, want the session discarded=%v", open, failBack)
			}
		})
	}
}
