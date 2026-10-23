package gosmo

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"testing"

	mssql "github.com/microsoft/go-mssqldb"
)

func TestIsRetryable(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"plain", errors.New("boom"), false},
		{"bad conn", driver.ErrBadConn, true},
		{"wrapped bad conn", fmt.Errorf("query: %w", driver.ErrBadConn), true},
		{"io.EOF", io.EOF, true},
		{"wrapped io.EOF", fmt.Errorf("read: %w", io.EOF), true},
		{"net error", &net.OpError{Op: "read", Err: errors.New("connection reset")}, true},
		{"wrapped net error", fmt.Errorf("query: %w", &net.OpError{Op: "read", Err: errors.New("reset")}), true},
		{"stream error", mssql.StreamError{InnerError: errors.New("corrupt")}, true},
		{"server error", mssql.ServerError{}, true},
		// context.DeadlineExceeded implements net.Error, so without an
		// explicit rejection it reports as a transient failure and a caller
		// driving its own retry loop waits out three attempts on a deadline
		// that has already passed.
		{"deadline exceeded", context.DeadlineExceeded, false},
		{"wrapped deadline exceeded", fmt.Errorf("query: %w", context.DeadlineExceeded), false},
		{"canceled", context.Canceled, false},
		{"wrapped canceled", fmt.Errorf("query: %w", context.Canceled), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := IsRetryable(c.err); got != c.want {
				t.Errorf("IsRetryable(%v) = %v, want %v", c.err, got, c.want)
			}
		})
	}
}

func TestWithRetrySucceedsAfterTransient(t *testing.T) {
	calls := 0
	v, err := withRetry(context.Background(), func() (int, error) {
		calls++
		if calls < 2 {
			return 0, driver.ErrBadConn
		}
		return 99, nil
	})
	if err != nil {
		t.Fatalf("withRetry: %v", err)
	}
	if v != 99 {
		t.Errorf("value = %d, want 99", v)
	}
	if calls != 2 {
		t.Errorf("calls = %d, want 2", calls)
	}
}

func TestWithRetryExhausts(t *testing.T) {
	calls := 0
	_, err := withRetry(context.Background(), func() (int, error) {
		calls++
		return 0, driver.ErrBadConn
	})
	if err == nil {
		t.Fatal("want error after exhausting retries")
	}
	if calls != readRetryAttempts {
		t.Errorf("calls = %d, want %d", calls, readRetryAttempts)
	}
}

func TestWithRetryStopsOnNonRetryable(t *testing.T) {
	calls := 0
	_, err := withRetry(context.Background(), func() (int, error) {
		calls++
		return 0, errors.New("fatal")
	})
	if err == nil {
		t.Fatal("want the non-retryable error back")
	}
	if calls != 1 {
		t.Errorf("calls = %d, want 1 (no retry on non-retryable error)", calls)
	}
}

func TestWithRetryHonoursContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled before the backoff sleep
	calls := 0
	_, err := withRetry(ctx, func() (int, error) {
		calls++
		return 0, driver.ErrBadConn
	})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if calls != 1 {
		t.Errorf("calls = %d, want 1 (cancelled before retry)", calls)
	}
}

func TestIsTransient(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"plain", errors.New("deadlocked"), false},
		{"bad conn", driver.ErrBadConn, true},
		{"deadlock victim", mssql.Error{Number: 1205, Message: "chosen as the deadlock victim"}, true},
		{"wrapped deadlock victim", fmt.Errorf("gosmo: list tables: %w", mssql.Error{Number: 1205}), true},
		{"azure unavailable", mssql.Error{Number: 40613}, true},
		{"azure busy", mssql.Error{Number: 40501}, true},
		{"azure resource limit", mssql.Error{Number: 10928}, true},
		{"read-secondary wait", mssql.Error{Number: 4221}, true},
		{"in a later message", mssql.Error{Number: 3621, All: []mssql.Error{{Number: 3621}, {Number: 1205}}}, true},
		{"lock timeout", mssql.Error{Number: 1222}, false},
		{"permission", mssql.Error{Number: 229}, false},
		{"canceled", context.Canceled, false},
		{"deadline exceeded", context.DeadlineExceeded, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := IsTransient(c.err); got != c.want {
				t.Errorf("IsTransient(%v) = %v, want %v", c.err, got, c.want)
			}
		})
	}
	// IsRetryable keeps its meaning: the connection failed.
	if IsRetryable(mssql.Error{Number: 1205}) {
		t.Error("IsRetryable(1205) = true; it must still mean a connection failure only")
	}
}

// deadlockDriver fails every statement naming deadlockProbe with Msg 1205 and
// counts them; anything else (USE, SELECT DB_NAME(), BEGIN) succeeds, so the
// count is the number of times the read itself was tried.
type deadlockDriver struct {
	mu    sync.Mutex
	tries int
}

const deadlockProbe = "deadlock_probe"

func (d *deadlockDriver) Open(string) (driver.Conn, error)             { return deadlockConn{d}, nil }
func (d *deadlockDriver) Connect(context.Context) (driver.Conn, error) { return deadlockConn{d}, nil }
func (d *deadlockDriver) Driver() driver.Driver                        { return d }

func (d *deadlockDriver) try(q string) error {
	if !strings.Contains(q, deadlockProbe) {
		return nil
	}
	d.mu.Lock()
	d.tries++
	d.mu.Unlock()
	return mssql.Error{Number: 1205, Class: 13, Message: "Transaction was deadlocked on lock resources with another process and has been chosen as the deadlock victim. Rerun the transaction."}
}

type deadlockConn struct{ d *deadlockDriver }

func (c deadlockConn) Prepare(string) (driver.Stmt, error) { return nil, driver.ErrSkip }
func (c deadlockConn) Close() error                        { return nil }
func (c deadlockConn) Begin() (driver.Tx, error)           { return deadlockTx{}, nil }
func (c deadlockConn) ExecContext(_ context.Context, q string, _ []driver.NamedValue) (driver.Result, error) {
	if err := c.d.try(q); err != nil {
		return nil, err
	}
	return driver.ResultNoRows, nil
}
func (c deadlockConn) QueryContext(_ context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	if _, inner, _, ok := unwrapProcRead(q, args); ok {
		q = inner
	}
	if err := c.d.try(q); err != nil {
		return nil, err
	}
	return &captureRows{rows: [][]driver.Value{{"master"}}}, nil
}

type deadlockTx struct{}

func (deadlockTx) Commit() error   { return nil }
func (deadlockTx) Rollback() error { return nil }

// A read chosen as a deadlock victim is re-run, readRetryAttempts times in
// all, on the server and database paths and in AcquireConn's prologue; inside
// InTransaction the 1205 has already rolled the transaction back, so the read
// fails at once.
func TestDeadlockVictimReadIsRetriedOutsideTransactionOnly(t *testing.T) {
	q := "SELECT name FROM sys.tables /* " + deadlockProbe + " */"
	for _, c := range []struct {
		name  string
		inTx  bool
		want  int
		reads func(ctx context.Context, s *Server) error
	}{
		{"Server.query", false, readRetryAttempts, func(ctx context.Context, s *Server) error { return errOnly(s.query(ctx, q)) }},
		{"Server.queryRow", false, readRetryAttempts, func(ctx context.Context, s *Server) error {
			var v string
			return s.queryRowScan(ctx, q, nil, &v)
		}},
		{"Database.query", false, readRetryAttempts, func(ctx context.Context, s *Server) error { return errOnly(s.DatabaseRef("db").query(ctx, q)) }},
		{"AcquireConn", false, readRetryAttempts, func(ctx context.Context, s *Server) error {
			return errOnly(AcquireConn(ctx, s.db, deadlockProbe))
		}},
		{"Server.query in a transaction", true, 1, func(ctx context.Context, s *Server) error { return errOnly(s.query(ctx, q)) }},
		{"Database.query in a transaction", true, 1, func(ctx context.Context, s *Server) error { return errOnly(s.DatabaseRef("db").query(ctx, q)) }},
	} {
		t.Run(c.name, func(t *testing.T) {
			d := &deadlockDriver{}
			s := &Server{db: sql.OpenDB(d)}
			t.Cleanup(func() { s.db.Close() })
			var err error
			if c.inTx {
				err = s.InTransaction(t.Context(), func(ctx context.Context) error { return c.reads(ctx, s) })
			} else {
				err = c.reads(t.Context(), s)
			}
			if se, ok := AsSQLError(err); !ok || se.Number != 1205 {
				t.Errorf("err = %v, want the 1205 back", err)
			}
			if d.tries != c.want {
				t.Errorf("tries = %d, want %d", d.tries, c.want)
			}
		})
	}
}
