//go:build livedb

// Live verification that this package never leaves a database stranded in
// SINGLE_USER.
//
// A database it sets to SINGLE_USER is unreachable by every other login until
// someone notices and puts it back — the application looks like it did nothing
// while having locked the database to one connection. Three ways in, one per
// test below: a forced drop whose DROP fails, a repair issued on a context that
// has already expired, and the ordinary release after a forced rename.
//
// None of this can be established without a server. The statement text is
// right in every case; what is under test is whether the repair *runs*.
//
//	go test -tags livedb . -run TestLiveSingleUser -v \
//	  -livedb 'sqlserver://sa:PASS@host?TrustServerCertificate=true'
//
// Creates and drops its own databases; touches nothing else.
package gosmo

import (
	"context"
	"database/sql"
	"testing"
	"time"
)

// liveSingleUserDB creates a throwaway database and returns its name plus a
// dropper that copes with the database having been left in SINGLE_USER — which
// is the failure these tests exist to catch, and would otherwise strand the
// scratch database on the instance when one of them fails.
func liveSingleUserDB(t *testing.T, srv *Server, ctx context.Context, name string) func() {
	t.Helper()
	drop := func() {
		srv.execContext(context.Background(),
			"IF DB_ID('"+name+"') IS NOT NULL BEGIN "+
				"ALTER DATABASE "+quoteIdent(name)+" SET MULTI_USER; "+
				"DROP DATABASE "+quoteIdent(name)+"; END")
	}
	drop()
	if err := srv.execContext(ctx, "CREATE DATABASE "+quoteIdent(name)); err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
	return drop
}

// userAccess reads sys.databases.user_access_desc, the column that says
// whether a database is still locked to one login.
func userAccess(t *testing.T, srv *Server, ctx context.Context, name string) string {
	t.Helper()
	var access string
	err := srv.db.QueryRowContext(ctx,
		"SELECT user_access_desc FROM sys.databases WHERE name = @p1", name).Scan(&access)
	if err == sql.ErrNoRows {
		return "(no such database)"
	}
	if err != nil {
		t.Fatalf("read user_access_desc for %s: %v", name, err)
	}
	return access
}

// TestLiveSingleUserForcedDropThatFailsLeavesTheDatabaseUsable.
//
// DropDatabaseContext(force) sets SINGLE_USER WITH ROLLBACK IMMEDIATE and then
// drops. The drop can genuinely fail after the alter succeeded, and it used to
// return that failure with the database still there and still single-user.
//
// Staged with a database snapshot, which SQL Server refuses to let its source
// be dropped out from under (Msg 3709) — a deterministic failure that happens
// *after* the access mode has already been changed, which is the shape that
// matters. A test that made the drop fail earlier would pass with the repair
// deleted.
func TestLiveSingleUserForcedDropThatFailsLeavesTheDatabaseUsable(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()
	srv := &Server{db: db}

	const name, snap = "gossms_live_su_drop", "gossms_live_su_drop_snap"
	dropSnap := func() {
		srv.execContext(context.Background(), "IF DB_ID('"+snap+"') IS NOT NULL DROP DATABASE "+quoteIdent(snap))
	}
	dropSnap()
	drop := liveSingleUserDB(t, srv, ctx, name)
	defer drop()
	defer dropSnap()

	var file string
	if err := srv.db.QueryRowContext(ctx,
		"SELECT REPLACE(physical_name, '.mdf', '_snap.ss') FROM sys.master_files "+
			"WHERE database_id = DB_ID(@p1) AND type = 0", name).Scan(&file); err != nil {
		t.Fatalf("locate the data file: %v", err)
	}
	if err := srv.execContext(ctx, "CREATE DATABASE "+quoteIdent(snap)+
		" ON (NAME = "+quoteIdent(name)+", FILENAME = '"+escapeSingle(file)+"') AS SNAPSHOT OF "+quoteIdent(name)); err != nil {
		t.Fatalf("create snapshot: %v", err)
	}

	err := srv.DropDatabaseContext(ctx, name, true)
	if err == nil {
		t.Fatal("the drop succeeded; the snapshot was supposed to make it fail")
	}
	t.Logf("drop failed as staged: %v", err)

	if got := userAccess(t, srv, ctx, name); got != "MULTI_USER" {
		t.Errorf("after a failed forced drop the database is %s, want MULTI_USER — "+
			"it is locked to one login and nothing said so", got)
	}
}

// TestLiveSingleUserRepairSurvivesAnExpiredContext is the reason
// restoreMultiUser derives its context with context.WithoutCancel.
//
// The statement it repairs after — SET SINGLE_USER WITH ROLLBACK IMMEDIATE —
// waits out the rollback of every transaction it killed, so it is precisely the
// statement likely to have used up the caller's whole deadline. On the caller's
// own context the repair then fails without reaching the server at all, and the
// database stays single-user for a reason nobody asked for.
//
// The A/B is in the test: the same ALTER on the same dead context is issued
// both ways, and only the WithoutCancel one arrives.
func TestLiveSingleUserRepairSurvivesAnExpiredContext(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()
	srv := &Server{db: db}

	const name = "gossms_live_su_expired"
	drop := liveSingleUserDB(t, srv, ctx, name)
	defer drop()

	setSingle := func() {
		if err := srv.execContext(ctx, "ALTER DATABASE "+quoteIdent(name)+
			" SET SINGLE_USER WITH ROLLBACK IMMEDIATE"); err != nil {
			t.Fatalf("set single user: %v", err)
		}
		if got := userAccess(t, srv, ctx, name); got != "SINGLE_USER" {
			t.Fatalf("staging left the database %s, want SINGLE_USER — the test would prove nothing", got)
		}
	}

	dead, cancel := context.WithCancel(ctx)
	cancel()

	// First the pre-fix form, to show the context really is dead and that this
	// is what used to strand the database.
	setSingle()
	if err := srv.execContext(dead, "ALTER DATABASE "+quoteIdent(name)+" SET MULTI_USER"); err == nil {
		t.Fatal("the ALTER on a cancelled context succeeded; the A/B below proves nothing")
	}
	if got := userAccess(t, srv, ctx, name); got != "SINGLE_USER" {
		t.Fatalf("the cancelled ALTER reached the server after all (%s)", got)
	}

	// Now the repair, on the same dead context.
	if err := srv.restoreMultiUser(dead, name); err != nil {
		t.Fatalf("restoreMultiUser on an expired context: %v", err)
	}
	if got := userAccess(t, srv, ctx, name); got != "MULTI_USER" {
		t.Errorf("after restoreMultiUser the database is %s, want MULTI_USER", got)
	}
}

// TestLiveSingleUserForcedRenameReleasesTheDatabase covers the ordinary path,
// with a connection held open inside the database so that force has something
// to roll back — a rename against an idle database never exercises the wait
// that makes the release matter.
func TestLiveSingleUserForcedRenameReleasesTheDatabase(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()
	srv := &Server{db: db}

	const name, renamed = "gossms_live_su_rename", "gossms_live_su_renamed"
	dropRenamed := func() {
		srv.execContext(context.Background(),
			"IF DB_ID('"+renamed+"') IS NOT NULL BEGIN "+
				"ALTER DATABASE "+quoteIdent(renamed)+" SET MULTI_USER; "+
				"DROP DATABASE "+quoteIdent(renamed)+"; END")
	}
	dropRenamed()
	drop := liveSingleUserDB(t, srv, ctx, name)
	defer drop()
	defer dropRenamed()

	// A second session sitting inside the database, in an open transaction, so
	// the rename has a real rollback to wait out rather than an idle database.
	holder, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("open a second connection: %v", err)
	}
	if _, err := holder.ExecContext(ctx, "USE "+quoteIdent(name)+"; BEGIN TRANSACTION; SELECT 1"); err != nil {
		holder.Close()
		t.Fatalf("occupy the database: %v", err)
	}
	defer holder.Close()

	start := time.Now()
	if err := srv.RenameDatabaseContext(ctx, name, renamed, true); err != nil {
		t.Fatalf("forced rename: %v", err)
	}
	t.Logf("forced rename took %v", time.Since(start))

	if got := userAccess(t, srv, ctx, renamed); got != "MULTI_USER" {
		t.Errorf("after a forced rename the database is %s, want MULTI_USER — "+
			"the rename worked and left it unusable", got)
	}
	if got := userAccess(t, srv, ctx, name); got != "(no such database)" {
		t.Errorf("the old name still resolves (%s)", got)
	}
}
