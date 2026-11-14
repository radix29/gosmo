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
		srv.exec(context.Background(),
			"IF DB_ID('"+name+"') IS NOT NULL BEGIN "+
				"ALTER DATABASE "+QuoteName(name)+" SET MULTI_USER; "+
				"DROP DATABASE "+QuoteName(name)+"; END")
	}
	drop()
	if _, err := srv.exec(ctx, "CREATE DATABASE "+QuoteName(name)); err != nil {
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
// Database.Drop(force) sets SINGLE_USER WITH ROLLBACK IMMEDIATE and then
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
		srv.exec(context.Background(), "IF DB_ID('"+snap+"') IS NOT NULL DROP DATABASE "+QuoteName(snap))
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
	if _, err := srv.exec(ctx, "CREATE DATABASE "+QuoteName(snap)+
		" ON (NAME = "+QuoteName(name)+", FILENAME = '"+escapeSingle(file)+"') AS SNAPSHOT OF "+QuoteName(name)); err != nil {
		t.Fatalf("create snapshot: %v", err)
	}

	err := srv.DatabaseRef(name).Drop(ctx, TerminationRollbackImmediate)
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
// restoreAccessMode derives its context with context.WithoutCancel.
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
		if _, err := srv.exec(ctx, "ALTER DATABASE "+QuoteName(name)+
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
	if _, err := srv.exec(dead, "ALTER DATABASE "+QuoteName(name)+" SET MULTI_USER"); err == nil {
		t.Fatal("the ALTER on a cancelled context succeeded; the A/B below proves nothing")
	}
	if got := userAccess(t, srv, ctx, name); got != "SINGLE_USER" {
		t.Fatalf("the cancelled ALTER reached the server after all (%s)", got)
	}

	// Now the repair, on the same dead context.
	if err := srv.restoreAccessMode(dead, name, UserAccessMulti); err != nil {
		t.Fatalf("restoreAccessMode on an expired context: %v", err)
	}
	if got := userAccess(t, srv, ctx, name); got != "MULTI_USER" {
		t.Errorf("after restoreAccessMode the database is %s, want MULTI_USER", got)
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
		srv.exec(context.Background(),
			"IF DB_ID('"+renamed+"') IS NOT NULL BEGIN "+
				"ALTER DATABASE "+QuoteName(renamed)+" SET MULTI_USER; "+
				"DROP DATABASE "+QuoteName(renamed)+"; END")
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
	if _, err := holder.ExecContext(ctx, "USE "+QuoteName(name)+"; BEGIN TRANSACTION; SELECT 1"); err != nil {
		holder.Close()
		t.Fatalf("occupy the database: %v", err)
	}
	defer holder.Close()

	start := time.Now()
	if err := srv.DatabaseRef(name).Rename(ctx, renamed, TerminationRollbackImmediate); err != nil {
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

// TestLiveSingleUserForcedWritesKeepTheAccessMode: a forced rename, drop or
// detach takes the database SINGLE_USER and releases it afterwards — to the
// mode it had, not to a hard-coded MULTI_USER, which opened a database the
// DBA had left RESTRICTED_USER to everyone (gossms renames with force
// always). Each path is run from RESTRICTED_USER and from SINGLE_USER: a
// successful rename, a rename refused because the name is taken, a forced
// drop refused by a snapshot, and the client-side repair of a batch cut
// short, on an expired context, under both names.
func TestLiveSingleUserForcedWritesKeepTheAccessMode(t *testing.T) {
	db, ctx, done := liveDBTimeout(t, 3*time.Minute)
	defer done()
	srv := &Server{db: db}

	const name, renamed, taken, snap = "gossms_live_su_mode", "gossms_live_su_mode2",
		"gossms_live_su_mode_taken", "gossms_live_su_mode_snap"
	dropSnap := func() {
		srv.exec(context.Background(), "IF DB_ID('"+snap+"') IS NOT NULL DROP DATABASE "+QuoteName(snap))
	}
	dropRenamed := liveSingleUserDB(t, srv, ctx, renamed) // created only so the dropper exists
	dropRenamed()
	defer dropRenamed()
	dropTaken := liveSingleUserDB(t, srv, ctx, taken)
	defer dropTaken()

	for _, mode := range []string{"RESTRICTED_USER", "SINGLE_USER"} {
		dropSnap()
		dropRenamed()
		drop := liveSingleUserDB(t, srv, ctx, name)
		set := func(db string) {
			t.Helper()
			if _, err := srv.exec(ctx, "ALTER DATABASE "+QuoteName(db)+" SET "+mode+" WITH ROLLBACK IMMEDIATE"); err != nil {
				t.Fatalf("set %s %s: %v", db, mode, err)
			}
		}
		check := func(what, db string) {
			t.Helper()
			if got := userAccess(t, srv, ctx, db); got != mode {
				t.Errorf("from %s, after %s the database is %s, want %s kept", mode, what, got, mode)
			}
		}
		set(name)

		if err := srv.DatabaseRef(name).Rename(ctx, taken, TerminationRollbackImmediate); err == nil {
			t.Fatalf("from %s: a rename onto a taken name succeeded", mode)
		}
		check("a refused forced rename", name)
		if got := userAccess(t, srv, ctx, taken); got != "MULTI_USER" {
			t.Errorf("from %s: a refused rename touched the database holding the name (%s)", mode, got)
		}

		if err := srv.DatabaseRef(name).Rename(ctx, renamed, TerminationRollbackImmediate); err != nil {
			t.Fatalf("from %s: forced rename: %v", mode, err)
		}
		check("a forced rename", renamed)
		if err := srv.DatabaseRef(renamed).Rename(ctx, name, TerminationRollbackImmediate); err != nil {
			t.Fatalf("from %s: forced rename back: %v", mode, err)
		}

		var file string
		if err := srv.db.QueryRowContext(ctx,
			"SELECT REPLACE(physical_name, '.mdf', '_snap.ss') FROM sys.master_files "+
				"WHERE database_id = DB_ID(@p1) AND type = 0", name).Scan(&file); err != nil {
			t.Fatalf("locate the data file: %v", err)
		}
		if _, err := srv.exec(ctx, "CREATE DATABASE "+QuoteName(snap)+
			" ON (NAME = "+QuoteName(name)+", FILENAME = '"+escapeSingle(file)+"') AS SNAPSHOT OF "+QuoteName(name)); err != nil {
			t.Fatalf("create snapshot: %v", err)
		}
		if err := srv.DatabaseRef(name).Drop(ctx, TerminationRollbackImmediate); err == nil {
			t.Fatalf("from %s: the drop succeeded; the snapshot was supposed to refuse it", mode)
		}
		check("a refused forced drop", name)
		dropSnap()

		// The client-side repair, as after a batch cut short: the database is
		// SINGLE_USER by then, and the repair runs on a dead context.
		dead, cancel := context.WithCancel(ctx)
		cancel()
		before := srv.accessModeBefore(ctx, name)
		if string(before) != mode {
			t.Errorf("accessModeBefore read %s, want %s", before, mode)
		}
		if _, err := srv.exec(ctx, "ALTER DATABASE "+QuoteName(name)+" SET SINGLE_USER WITH ROLLBACK IMMEDIATE"); err != nil {
			t.Fatalf("set single user: %v", err)
		}
		if err := srv.restoreAccessMode(dead, name, before); err != nil {
			t.Fatalf("restoreAccessMode: %v", err)
		}
		check("the repair of a cut-short drop", name)
		if _, err := srv.exec(ctx, "ALTER DATABASE "+QuoteName(name)+" SET SINGLE_USER WITH ROLLBACK IMMEDIATE; "+
			"ALTER DATABASE "+QuoteName(name)+" MODIFY NAME = "+QuoteName(renamed)); err != nil {
			t.Fatalf("stage a rename that happened: %v", err)
		}
		if err := srv.restoreAccessModeAfterRename(dead, name, renamed, before); err != nil {
			t.Fatalf("restoreAccessModeAfterRename: %v", err)
		}
		check("the repair of a cut-short rename that happened", renamed)

		dropRenamed()
		drop()
	}
}
