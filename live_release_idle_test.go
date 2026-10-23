//go:build livedb

// Live verification of the two halves of exclusive access (review plan S1 and
// S11): a Server's own idle sessions, left inside a database by the writes just
// before, no longer block a statement that needs the database to itself; and
// TerminationRollbackImmediate gets past somebody else's.
//
//	go test -tags livedb . -run TestLiveExclusiveAccess -v \
//	  -livedb 'sqlserver://sa:PASS@host?TrustServerCertificate=true'
//
// Creates and drops its own throwaway databases; touches nothing else. Every
// exclusive statement runs on its own short deadline, so a regression fails
// in seconds rather than waiting out the idle session.
package gosmo

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	mssql "github.com/microsoft/go-mssqldb"
)

// exclusiveDeadline bounds each statement under test. The failure it guards
// against is a wait of up to ConnMaxIdleTime (5 minutes) or forever.
const exclusiveDeadline = 15 * time.Second

// ownSessionsIn counts the user sessions whose current database is name,
// observed from a pool other than the one under test — asking through the
// same pool would hand the question to one of the idle sessions being counted.
func ownSessionsIn(t *testing.T, observer *sql.DB, ctx context.Context, name string) int {
	t.Helper()
	var n int
	if err := observer.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sys.dm_exec_sessions WHERE is_user_process = 1 AND database_id = DB_ID(@p1)`, name,
	).Scan(&n); err != nil {
		t.Fatalf("count sessions in %s: %v", name, err)
	}
	return n
}

// liveObserver opens a second pool on the same DSN, for watching and for
// parking a session the Server under test knows nothing about.
func liveObserver(t *testing.T, ctx context.Context) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlserver", *liveDSN)
	if err != nil {
		t.Fatalf("open observer: %v", err)
	}
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		t.Fatalf("ping observer: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// leaveTwoIdleIn runs a database-scoped statement on two connections at once,
// so the pool ends up with two idle sessions inside d. One is not enough to
// show the bug: the pool hands the next statement that same connection, whose
// session reset takes it out of the database first. The failure needs the
// statement to get a different connection from the one parked in the
// database — which is what happens as soon as statements overlap, as
// gossms's do. A read no longer parks a session (procRead); a write still
// does, through withConn's USE, so that is what this overlaps.
func leaveTwoIdleIn(t *testing.T, observer *sql.DB, d *Database, ctx context.Context) {
	t.Helper()
	err := d.withConn(ctx, func(ctx context.Context, _ sqlConn) error {
		_, err := d.exec(ctx, "DECLARE @parked int") // a second connection, while the first is held
		return err
	})
	if err != nil {
		t.Fatalf("overlapping statements: %v", err)
	}
	if n := ownSessionsIn(t, observer, ctx, d.Name); n < 2 {
		t.Fatalf("%d idle sessions left in the database after two overlapping statements, want 2 — the precondition this test exists for did not hold", n)
	}
}

// The probe that found S1: a read left an idle session in the database, and
// a detach without DropConnections through the same pool then failed Msg 3703
// "currently in use" — against a database nobody else was using. Reads no
// longer park one (procRead); writes still do (leaveTwoIdleIn).
func TestLiveExclusiveAccessDetachAfterRead(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()
	srv, _, drop := detLiveSetup(t, db, ctx)
	defer drop()
	observer := liveObserver(t, ctx)

	leaveTwoIdleIn(t, observer, srv.DatabaseRef(detLiveName), ctx)

	dctx, cancel := context.WithTimeout(ctx, exclusiveDeadline)
	defer cancel()
	if err := srv.DatabaseRef(detLiveName).Detach(dctx, DetachOptions{}); err != nil {
		t.Fatalf("Detach without DropConnections with own sessions parked in it: %v", err)
	}
	if databaseExists(t, db, ctx, detLiveName) {
		t.Fatal("the database is still attached after a detach that reported success")
	}
}

// liveFileGroupDB is a scratch database with a second filegroup holding one
// file, so the filegroup's read-only flag can be set.
func liveFileGroupDB(t *testing.T, db *sql.DB, ctx context.Context, name string) (*Database, func()) {
	t.Helper()
	d, drop := liveScratchDB(t, db, ctx, name)
	dir := d.Server().Info().DefaultDataPath
	sep := "\\"
	if strings.HasPrefix(dir, "/") {
		sep = "/"
	}
	path := strings.TrimRight(dir, "\\/") + sep + name + "_fg2.ndf"
	if err := d.AddFileGroup(ctx, "FG2"); err != nil {
		drop()
		t.Fatalf("AddFileGroup: %v", err)
	}
	if err := d.AddFile(ctx, DatabaseFileSpec{Name: name + "_fg2", FileGroup: "FG2", Path: path, SizeKB: 8192}); err != nil {
		drop()
		t.Fatalf("AddFile: %v", err)
	}
	return d, drop
}

// The same shape for a filegroup: MODIFY FILEGROUP ... READ_ONLY after a read
// of the database waited behind the reading pool's own idle session. Reads no
// longer park one (procRead); writes still do (leaveTwoIdleIn).
func TestLiveExclusiveAccessFileGroupReadOnlyAfterRead(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()
	d, drop := liveFileGroupDB(t, db, ctx, "gosmo_s1_fg_live")
	defer drop()
	observer := liveObserver(t, ctx)

	for _, readOnly := range []bool{true, false} {
		leaveTwoIdleIn(t, observer, d, ctx)
		fctx, cancel := context.WithTimeout(ctx, exclusiveDeadline)
		err := d.FileGroupRef("FG2").SetReadOnly(fctx, readOnly, TerminationNone)
		cancel()
		if err != nil {
			t.Fatalf("SetFileGroupReadOnly(%v) with own sessions parked: %v", readOnly, err)
		}
	}
}

// park holds an open transaction inside name on a session of observer's, and
// returns its connection. The transaction created table parked, so whether it
// was rolled back shows in whether that table exists.
func park(t *testing.T, observer *sql.DB, ctx context.Context, name string) *sql.Conn {
	t.Helper()
	conn, err := observer.Conn(ctx)
	if err != nil {
		t.Fatalf("observer Conn: %v", err)
	}
	if _, err := conn.ExecContext(ctx, "USE ["+name+"]; BEGIN TRAN; CREATE TABLE dbo.parked (i int);"); err != nil {
		conn.Close()
		t.Fatalf("park a transaction in %s: %v", name, err)
	}
	return conn
}

func parkedTableExists(t *testing.T, d *Database, ctx context.Context) bool {
	t.Helper()
	var n int
	if err := d.queryRow(ctx, func(r *sql.Row) error { return r.Scan(&n) },
		"SELECT COUNT(*) FROM sys.tables WHERE name = N'parked'"); err != nil {
		t.Fatalf("look for the parked table: %v", err)
	}
	return n > 0
}

// S11: READ_COMMITTED_SNAPSHOT behind another session's open transaction
// waits for as long as that transaction stays open. TerminationRollbackImmediate
// is what finishes it, rolling the other session's work back.
func TestLiveExclusiveAccessRCSIRollsBackAParkedSession(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()
	d, drop := liveScratchDB(t, db, ctx, "gosmo_s11_rcsi_live")
	defer drop()
	observer := liveObserver(t, ctx)

	conn := park(t, observer, ctx, d.Name)
	defer conn.Close()

	// Without termination the statement waits; a short deadline shows it.
	wctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	err := d.SetDatabaseOption(wctx, DBOptReadCommittedSnapshot, "ON", TerminationNone)
	cancel()
	if err == nil {
		t.Fatal("RCSI ON with TerminationNone finished despite an open transaction in the database — the precondition did not hold")
	}

	rctx, cancel := context.WithTimeout(ctx, exclusiveDeadline)
	defer cancel()
	if err := d.SetDatabaseOption(rctx, DBOptReadCommittedSnapshot, "ON", TerminationRollbackImmediate); err != nil {
		t.Fatalf("RCSI ON with TerminationRollbackImmediate: %v", err)
	}
	o, err := d.Options(ctx)
	if err != nil {
		t.Fatalf("Options: %v", err)
	}
	if !o.ReadCommittedSnapshot {
		t.Error("READ_COMMITTED_SNAPSHOT still off after a statement that reported success")
	}
	if parkedTableExists(t, d, ctx) {
		t.Error("the parked session's transaction survived ROLLBACK IMMEDIATE")
	}
}

// S11 for a filegroup, where WITH ROLLBACK IMMEDIATE is parsed and ignored:
// the kill batch is what gets the statement past the parked session.
func TestLiveExclusiveAccessFileGroupRollsBackAParkedSession(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()
	d, drop := liveFileGroupDB(t, db, ctx, "gosmo_s11_fg_live")
	defer drop()
	observer := liveObserver(t, ctx)

	conn := park(t, observer, ctx, d.Name)
	defer conn.Close()

	fctx, cancel := context.WithTimeout(ctx, exclusiveDeadline)
	defer cancel()
	if err := d.FileGroupRef("FG2").SetReadOnly(fctx, true, TerminationRollbackImmediate); err != nil {
		t.Fatalf("SetFileGroupReadOnly with TerminationRollbackImmediate: %v", err)
	}
	fgs, err := d.FileGroups(ctx)
	if err != nil {
		t.Fatalf("FileGroups: %v", err)
	}
	for _, fg := range fgs {
		if fg.Name == "FG2" && !fg.IsReadOnly {
			t.Error("FG2 is not read-only after a statement that reported success")
		}
	}
	if parkedTableExists(t, d, ctx) {
		t.Error("the parked session's transaction survived the kill")
	}
}

// containmentApplied reports whether a SET CONTAINMENT = PARTIAL got past
// the lock: success, or Msg 12824 where "contained database authentication"
// is off (win10cli) — raised only once the exclusive lock was taken.
func containmentApplied(err error) bool {
	if err == nil {
		return true
	}
	msErr, ok := errors.AsType[mssql.Error](err)
	return ok && slices.ContainsFunc(msErr.All, func(e mssql.Error) bool { return e.Number == 12824 })
}

// M2 (review plan 2026-10-13): CONTAINMENT needs the database to itself, like
// READ_COMMITTED_SNAPSHOT. The Server's own idle sessions in it are released
// first, and TerminationRollbackImmediate gets past somebody else's; before,
// either waited without end.
func TestLiveExclusiveAccessContainment(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()
	d, drop := liveScratchDB(t, db, ctx, "gosmo_m2_containment_live")
	defer drop()
	observer := liveObserver(t, ctx)

	leaveTwoIdleIn(t, observer, d, ctx)
	ictx, cancel := context.WithTimeout(ctx, exclusiveDeadline)
	err := d.SetDatabaseOption(ictx, DBOptContainment, "PARTIAL", TerminationNone)
	cancel()
	if !containmentApplied(err) {
		t.Fatalf("CONTAINMENT with own sessions parked, TerminationNone: %v — an own idle session blocked it", err)
	}

	conn := park(t, observer, ctx, d.Name)
	defer conn.Close()
	wctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	err = d.SetDatabaseOption(wctx, DBOptContainment, "PARTIAL", TerminationNone)
	cancel()
	if containmentApplied(err) {
		t.Fatal("CONTAINMENT with TerminationNone got past a session parked in the database — the precondition did not hold")
	}
	rctx, cancel := context.WithTimeout(ctx, exclusiveDeadline)
	defer cancel()
	if err := d.SetDatabaseOption(rctx, DBOptContainment, "PARTIAL", TerminationRollbackImmediate); !containmentApplied(err) {
		t.Fatalf("CONTAINMENT with TerminationRollbackImmediate: %v", err)
	}
}
