package gosmo

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"testing"
	"time"
)

// fakeAcquireConn simulates one pooled connection: its first query
// (AcquireConn's prologue) returns execErr, if set, and flips valid false as
// go-mssqldb's connectionGood does on a connection-level failure; otherwise
// it answers home. Later queries (Release's DB_NAME() read) answer current,
// or fail with readErr. sql.DB.putConn checks driver.Validator.IsValid to
// decide whether to keep a released connection — the mechanism AcquireConn's
// eviction relies on — and closed records a discard.
type fakeAcquireConn struct {
	execErr error
	valid   bool
	used    bool
	closed  bool

	home, current string
	readErr       error
}

func (c *fakeAcquireConn) Prepare(string) (driver.Stmt, error) { return nil, errFakeAcquireUnsupported }
func (c *fakeAcquireConn) Close() error                        { c.closed = true; return nil }
func (c *fakeAcquireConn) Begin() (driver.Tx, error)           { return nil, errFakeAcquireUnsupported }
func (c *fakeAcquireConn) IsValid() bool                       { return c.valid }

func (c *fakeAcquireConn) QueryContext(_ context.Context, _ string, _ []driver.NamedValue) (driver.Rows, error) {
	if !c.used {
		c.used = true
		if c.execErr != nil {
			c.valid = false
			return nil, c.execErr
		}
		return &captureRows{rows: [][]driver.Value{{c.home}}}, nil
	}
	if c.readErr != nil {
		return nil, c.readErr
	}
	return &captureRows{rows: [][]driver.Value{{c.current}}}, nil
}

var (
	_ driver.Conn           = (*fakeAcquireConn)(nil)
	_ driver.Validator      = (*fakeAcquireConn)(nil)
	_ driver.QueryerContext = (*fakeAcquireConn)(nil)
)

var errFakeAcquireUnsupported = errors.New("fakeAcquireConn: unsupported")

// fakeAcquireConnector hands out conns from a queue, one per dial, as sql.DB
// dials when no idle connection remains (e.g. after an eviction).
type fakeAcquireConnector struct {
	conns []*fakeAcquireConn
	next  int
}

func (f *fakeAcquireConnector) Connect(context.Context) (driver.Conn, error) {
	if f.next >= len(f.conns) {
		return nil, errors.New("fakeAcquireConnector: exhausted")
	}
	c := f.conns[f.next]
	f.next++
	return c, nil
}
func (f *fakeAcquireConnector) Driver() driver.Driver { return fakeAcquireDriver{} }

type fakeAcquireDriver struct{}

func (fakeAcquireDriver) Open(string) (driver.Conn, error) { return nil, errFakeAcquireUnsupported }

func TestAcquireConnRetriesDeadPooledConnection(t *testing.T) {
	dead := &fakeAcquireConn{valid: true, execErr: driver.ErrBadConn}
	healthy := &fakeAcquireConn{valid: true}
	db := sql.OpenDB(&fakeAcquireConnector{conns: []*fakeAcquireConn{dead, healthy}})
	defer db.Close()

	conn, err := AcquireConn(context.Background(), db, "")
	if err != nil {
		t.Fatalf("AcquireConn: %v", err)
	}
	defer conn.Release(context.Background())

	if !dead.used {
		t.Error("dead conn was never tried")
	}
	if !healthy.used {
		t.Error("healthy conn was never tried — AcquireConn didn't retry after the dead one")
	}
}

func TestAcquireConnUsesDatabase(t *testing.T) {
	healthy := &fakeAcquireConn{valid: true, home: "mydb"}
	db := sql.OpenDB(&fakeAcquireConnector{conns: []*fakeAcquireConn{healthy}})
	defer db.Close()

	conn, err := AcquireConn(context.Background(), db, "mydb")
	if err != nil {
		t.Fatalf("AcquireConn: %v", err)
	}
	defer conn.Release(context.Background())

	if !healthy.used {
		t.Error("connection's prologue was never run")
	}
	if conn.Home() != "mydb" {
		t.Errorf("Home() = %q, want mydb", conn.Home())
	}
}

// Release pools a connection still in its home database and discards one the
// caller's statements left elsewhere, or whose database it cannot read.
func TestPinnedConnRelease(t *testing.T) {
	for _, c := range []struct {
		name        string
		current     string
		readErr     error
		wantDiscard bool
		wantName    string
	}{
		{"still home", "master", nil, false, "master"},
		{"left in another database", "otherdb", nil, true, "otherdb"},
		{"read failed", "", errors.New("read failed"), true, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			fc := &fakeAcquireConn{valid: true, home: "master", current: c.current, readErr: c.readErr}
			db := sql.OpenDB(&fakeAcquireConnector{conns: []*fakeAcquireConn{fc}})
			defer db.Close()

			pc, err := AcquireConn(t.Context(), db, "")
			if err != nil {
				t.Fatalf("AcquireConn: %v", err)
			}
			name, err := pc.Release(t.Context())
			if (err != nil) != (c.readErr != nil) {
				t.Errorf("Release err = %v, want error %v", err, c.readErr != nil)
			}
			if name != c.wantName {
				t.Errorf("Release name = %q, want %q", name, c.wantName)
			}
			if fc.closed != c.wantDiscard {
				t.Errorf("discarded = %v, want %v", fc.closed, c.wantDiscard)
			}
			if _, err := pc.Release(t.Context()); !errors.Is(err, sql.ErrConnDone) {
				t.Errorf("second Release err = %v, want sql.ErrConnDone", err)
			}
		})
	}
}

// A ctx cancelled during the backoff keeps the transient error that caused
// the retry, joined with the cancel.
func TestAcquireConnCancelDuringBackoffKeepsCause(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	dead := &fakeAcquireConn{valid: true, execErr: driver.ErrBadConn}
	db := sql.OpenDB(&fakeAcquireConnector{conns: []*fakeAcquireConn{dead}})
	defer db.Close()

	go func() { <-time.After(readRetryDelay(1) / 2); cancel() }()
	_, err := AcquireConn(ctx, db, "")
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if !errors.Is(err, driver.ErrBadConn) {
		t.Errorf("err = %v, want the transient cause (driver.ErrBadConn) joined", err)
	}
}

func TestAcquireConnExhaustsRetries(t *testing.T) {
	conns := make([]*fakeAcquireConn, readRetryAttempts)
	for i := range conns {
		conns[i] = &fakeAcquireConn{valid: true, execErr: driver.ErrBadConn}
	}
	db := sql.OpenDB(&fakeAcquireConnector{conns: conns})
	defer db.Close()

	_, err := AcquireConn(context.Background(), db, "")
	if err == nil {
		t.Fatal("want an error after exhausting retries, got nil")
	}
	for i, c := range conns {
		if !c.used {
			t.Errorf("conn %d was never tried", i)
		}
	}
}

func TestAcquireConnExhaustsRetriesWrapsDatabaseError(t *testing.T) {
	conns := make([]*fakeAcquireConn, readRetryAttempts)
	for i := range conns {
		conns[i] = &fakeAcquireConn{valid: true, execErr: driver.ErrBadConn}
	}
	db := sql.OpenDB(&fakeAcquireConnector{conns: conns})
	defer db.Close()

	_, err := AcquireConn(context.Background(), db, "mydb")
	if err == nil {
		t.Fatal("want an error after exhausting retries, got nil")
	}
	if want := "gosmo: switch to database mydb"; err.Error()[:len(want)] != want {
		t.Errorf("err = %q, want prefix %q", err.Error(), want)
	}
}

func TestAcquireConnStopsOnNonRetryableError(t *testing.T) {
	fatal := &fakeAcquireConn{valid: true, execErr: errors.New("permission denied")}
	db := sql.OpenDB(&fakeAcquireConnector{conns: []*fakeAcquireConn{fatal}})
	defer db.Close()

	_, err := AcquireConn(context.Background(), db, "")
	if err == nil {
		t.Fatal("want the non-retryable error back, got nil")
	}
	if !fatal.used {
		t.Error("conn was never tried")
	}
}
