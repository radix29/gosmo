package gosmo

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"strings"
	"sync/atomic"
	"testing"
)

// countingConn is a pooled connection that only records being closed.
type countingConn struct{ closed *atomic.Int32 }

func (c countingConn) Prepare(string) (driver.Stmt, error) { return nil, driver.ErrSkip }
func (c countingConn) Close() error                        { c.closed.Add(1); return nil }
func (c countingConn) Begin() (driver.Tx, error)           { return nil, driver.ErrSkip }

type countingConnector struct{ dialed, closed atomic.Int32 }

func (f *countingConnector) Connect(context.Context) (driver.Conn, error) {
	f.dialed.Add(1)
	return countingConn{closed: &f.closed}, nil
}
func (f *countingConnector) Driver() driver.Driver { return fakeAcquireDriver{} }

// ReleaseIdleConnections must close what is idle — the sessions left parked
// inside a database by the reads before an exclusive-access statement — and
// leave the pool's configuration alone: a pool passed to NewServer carries its
// owner's idle limit, which nothing here can read back to restore.
func TestReleaseIdleConnectionsClosesEveryIdleConnection(t *testing.T) {
	fc := &countingConnector{}
	db := sql.OpenDB(fc)
	defer db.Close()
	db.SetMaxIdleConns(5)
	ctx := t.Context()

	// Three connections out at once, then all three back: three idle.
	var held []*sql.Conn
	for range 3 {
		c, err := db.Conn(ctx)
		if err != nil {
			t.Fatalf("Conn: %v", err)
		}
		held = append(held, c)
	}
	for _, c := range held {
		c.Close()
	}
	if got := db.Stats().Idle; got != 3 {
		t.Fatalf("idle before = %d, want 3", got)
	}

	s := &Server{db: db}
	if err := s.ReleaseIdleConnections(ctx); err != nil {
		t.Fatalf("ReleaseIdleConnections: %v", err)
	}
	if got := db.Stats().Idle; got != 0 {
		t.Errorf("idle after = %d, want 0", got)
	}
	if got := fc.closed.Load(); got != 3 {
		t.Errorf("closed %d driver connections, want 3", got)
	}
	if got := fc.dialed.Load(); got != 3 {
		t.Errorf("dialed %d times, want 3 — releasing must reuse the idle connections, not dial", got)
	}

	// The idle limit is untouched: a connection used and returned is kept.
	c, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("Conn after release: %v", err)
	}
	c.Close()
	if got := db.Stats().Idle; got != 1 {
		t.Errorf("idle after a later use = %d, want 1 — the pool's idle limit changed", got)
	}
}

// Under WithScript nothing runs, so there is nothing to release — and a
// scripting caller's Server may have no pool at all.
func TestReleaseIdleConnectionsIsANoOpWhenScripting(t *testing.T) {
	ctx, _ := WithScript(context.Background())
	if err := (&Server{}).ReleaseIdleConnections(ctx); err != nil {
		t.Fatalf("ReleaseIdleConnections under WithScript: %v", err)
	}
}

// Termination renders as the SET statement's WITH clause, and an unknown value
// is refused rather than silently meaning "wait".
func TestWithScriptTerminationClause(t *testing.T) {
	d := &Database{server: &Server{}, Name: "AppDB"}
	cases := []struct {
		name string
		set  func(context.Context) error
		want string
	}{
		{"RCSI rollback immediate", func(c context.Context) error {
			return d.SetDatabaseOption(c, DBOptReadCommittedSnapshot, "ON", TerminationRollbackImmediate)
		}, "ALTER DATABASE [AppDB] SET READ_COMMITTED_SNAPSHOT ON WITH ROLLBACK IMMEDIATE"},
		{"RCSI none", func(c context.Context) error {
			return d.SetDatabaseOption(c, DBOptReadCommittedSnapshot, "OFF", TerminationNone)
		}, "ALTER DATABASE [AppDB] SET READ_COMMITTED_SNAPSHOT OFF"},
		{"containment keeps its = before the clause", func(c context.Context) error {
			return d.SetDatabaseOption(c, DBOptContainment, "NONE", TerminationRollbackImmediate)
		}, "ALTER DATABASE [AppDB] SET CONTAINMENT = NONE WITH ROLLBACK IMMEDIATE"},
		{"read-only rollback immediate", func(c context.Context) error {
			return d.SetReadOnly(c, true, TerminationRollbackImmediate)
		}, "ALTER DATABASE [AppDB] SET READ_ONLY WITH ROLLBACK IMMEDIATE"},
		{"read-write none", func(c context.Context) error {
			return d.SetReadOnly(c, false, TerminationNone)
		}, "ALTER DATABASE [AppDB] SET READ_WRITE"},
	}
	for _, c := range cases {
		ctx, script := WithScript(context.Background())
		if err := c.set(ctx); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got := script.Statements(); len(got) != 1 || got[0] != c.want {
			t.Errorf("%s = %q, want [%q]", c.name, got, c.want)
		}
	}

	ctx, script := WithScript(context.Background())
	bad := Termination(7)
	for name, err := range map[string]error{
		"SetDatabaseOption":    d.SetDatabaseOption(ctx, DBOptAutoClose, "ON", bad),
		"SetReadOnly":          d.SetReadOnly(ctx, true, bad),
		"SetFileGroupReadOnly": d.SetFileGroupReadOnly(ctx, "FG2", true, bad),
	} {
		if err == nil {
			t.Errorf("%s accepted Termination(7)", name)
		}
	}
	if got := script.Statements(); len(got) != 0 {
		t.Errorf("a refused termination still emitted %q", got)
	}
}

// MODIFY FILEGROUP parses WITH ROLLBACK IMMEDIATE and ignores it, so the
// filegroup's form of TerminationRollbackImmediate is the session-kill batch
// ahead of the ALTER — one statement, so nothing reconnects in between.
func TestWithScriptFileGroupReadOnlyRollbackImmediateKillsFirst(t *testing.T) {
	d := &Database{server: &Server{}, Name: "App'DB"}
	ctx, script := WithScript(context.Background())
	if err := d.SetFileGroupReadOnly(ctx, "FG]2", true, TerminationRollbackImmediate); err != nil {
		t.Fatalf("SetFileGroupReadOnly: %v", err)
	}
	got := script.Statements()
	if len(got) != 1 {
		t.Fatalf("got %d statements, want the kill and the ALTER as one: %q", len(got), got)
	}
	want := killDatabaseSessionsBatch("App'DB") + ";\nALTER DATABASE [App'DB] MODIFY FILEGROUP [FG]]2] READ_ONLY;"
	if got[0] != want {
		t.Errorf("statement =\n%s\nwant\n%s", got[0], want)
	}
	if strings.Contains(got[0], "ROLLBACK IMMEDIATE") {
		t.Error("the filegroup statement carries a WITH ROLLBACK IMMEDIATE the server ignores")
	}
}
