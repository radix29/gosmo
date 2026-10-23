//go:build livedb

// Live proof for the A1 spike (OPEN-THREADS.md § Idle pooled connections camp
// in the last database read): a database-scoped read sent as an RPC call of
// [db].sys.sp_executesql (procRead) against the USE batch it would replace
// (useBatch). Each test runs both paths, so a test that passes for procRead
// and fails for useBatch shows it can see the difference it claims.
//
//	go test -tags livedb . -run TestLiveProcRead -v \
//	  -livedb 'sqlserver://sa:PASS@host?TrustServerCertificate=true'
//
// -procreads runs the rest of the live suite (TestLiveVersionSweep, the
// collation tests) on procRead too.
//
// Creates, backs up, restores and drops gosmo_a1_* databases, and deletes the
// backup file it wrote.
package gosmo

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"slices"
	"testing"
	"time"

	mssql "github.com/microsoft/go-mssqldb"
)

var liveProcReads = flag.Bool("procreads", false, "run live reads through procRead (the A1 spike) instead of useBatch")

// setProcReads sets procReads for the rest of t.
func setProcReads(t *testing.T, on bool) {
	t.Helper()
	was := procReads
	procReads = on
	t.Cleanup(func() { procReads = was })
}

// liveSecondDB is a second pool on the live DSN: another session, which
// neither shares nor resets gosmo's pooled connections.
func liveSecondDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlserver", *liveDSN)
	if err != nil {
		t.Fatalf("open second pool: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func pathName(proc bool) string {
	if proc {
		return "procRead"
	}
	return "useBatch"
}

// sessionsIn counts the sessions other than other's own whose current
// database is name.
func sessionsIn(t *testing.T, ctx context.Context, other *sql.DB, name string) int {
	t.Helper()
	var n int
	if err := other.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sys.dm_exec_sessions WHERE database_id = DB_ID(@p1) AND session_id <> @@SPID`,
		name).Scan(&n); err != nil {
		t.Fatalf("count sessions in %s: %v", name, err)
	}
	return n
}

// The read runs in d, and the pooled connection it used is left in its login
// database afterwards — under useBatch it is left in d.
func TestLiveProcReadLeavesSessionInLoginDatabase(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()
	other := liveSecondDB(t)

	d, drop := liveScratchDB(t, db, ctx, "gosmo_a1_home")
	t.Cleanup(drop)

	for _, proc := range []bool{true, false} {
		setProcReads(t, proc)
		var in string
		if err := d.queryRow(ctx, func(r *sql.Row) error { return r.Scan(&in) }, `SELECT DB_NAME()`); err != nil {
			t.Fatalf("%s: read: %v", pathName(proc), err)
		}
		if in != d.Name {
			t.Errorf("%s: read ran in %q, want %q", pathName(proc), in, d.Name)
		}
		// query too, whose connection is released by rows.Close.
		rows, err := d.query(ctx, `SELECT name FROM sys.objects WHERE object_id = @p1`, 3)
		if err != nil {
			t.Fatalf("%s: query: %v", pathName(proc), err)
		}
		for rows.Next() {
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("%s: rows: %v", pathName(proc), err)
		}
		rows.Close()

		n := sessionsIn(t, ctx, other, d.Name)
		t.Logf("%s: %d idle session(s) left in %s", pathName(proc), n, d.Name)
		if proc && n != 0 {
			t.Errorf("procRead left %d session(s) in %s, want 0", n, d.Name)
		}
		if !proc && n == 0 {
			t.Errorf("useBatch left no session in %s: the test cannot see camping", d.Name)
		}
	}
}

// The gate the plan names: a RESTORE … WITH REPLACE from another session
// succeeds straight after a gosmo read of the database. Under useBatch it fails
// Msg 3101, "Exclusive access could not be obtained".
func TestLiveProcReadDoesNotBlockRestore(t *testing.T) {
	db, ctx, done := liveDBTimeout(t, 3*time.Minute)
	defer done()
	other := liveSecondDB(t)

	d, drop := liveScratchDB(t, db, ctx, "gosmo_a1_restore")
	t.Cleanup(drop)

	const bak = "gosmo_a1_restore.bak" // the instance's default backup directory
	if _, err := other.ExecContext(ctx, `BACKUP DATABASE [gosmo_a1_restore] TO DISK = N'`+bak+`' WITH INIT, COPY_ONLY`); err != nil {
		t.Fatalf("backup: %v", err)
	}
	var file string
	if err := other.QueryRowContext(ctx, `
SELECT TOP (1) f.physical_device_name
FROM msdb.dbo.backupset s JOIN msdb.dbo.backupmediafamily f ON f.media_set_id = s.media_set_id
WHERE s.database_name = N'gosmo_a1_restore' ORDER BY s.backup_finish_date DESC`).Scan(&file); err != nil {
		t.Fatalf("find backup file: %v", err)
	}
	t.Cleanup(func() {
		if _, err := other.ExecContext(context.Background(), `EXEC master.sys.xp_delete_file 0, @p1`, file); err != nil {
			t.Logf("delete %s: %v (remove it by hand)", file, err)
		}
	})

	for _, proc := range []bool{true, false} {
		setProcReads(t, proc)
		var n int
		if err := d.queryRow(ctx, func(r *sql.Row) error { return r.Scan(&n) }, `SELECT COUNT(*) FROM sys.objects`); err != nil {
			t.Fatalf("%s: read: %v", pathName(proc), err)
		}
		_, err := other.ExecContext(ctx, `RESTORE DATABASE [gosmo_a1_restore] FROM DISK = @p1 WITH REPLACE`, file)
		t.Logf("%s: restore straight after a read: %v", pathName(proc), err)
		if proc && err != nil {
			t.Errorf("procRead: restore failed: %v", err)
		}
		if !proc {
			// Msg 3101 comes first and 3013 ("terminating abnormally") last,
			// and the driver's Number is the last one's.
			me, ok := errors.AsType[mssql.Error](err)
			if !ok || !slices.ContainsFunc(me.All, func(e mssql.Error) bool { return e.Number == 3101 }) {
				t.Errorf("useBatch: restore err = %v, want Msg 3101: the test cannot see camping", err)
			}
		}
	}
}

// A database that cannot be entered fails the read, with no rows from
// anywhere else. procRead has no compiled batch, so the SINGLE_USER case (Msg
// 924) must fail at the call, not at a compile perDatabaseExec avoids.
func TestLiveProcReadRefusesADatabaseItCannotEnter(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()
	other := liveSecondDB(t)

	d, drop := liveScratchDB(t, db, ctx, "gosmo_a1_enter")
	t.Cleanup(drop)
	missing := d.server.DatabaseRef("gosmo_a1_missing")

	read := func(target *Database) error {
		var in string
		return target.queryRow(ctx, func(r *sql.Row) error { return r.Scan(&in) }, `SELECT DB_NAME()`)
	}
	report := func(what string, target *Database) {
		for _, proc := range []bool{true, false} {
			setProcReads(t, proc)
			err := read(target)
			num := 0
			if me, ok := errors.AsType[mssql.Error](err); ok {
				num = int(me.Number)
			}
			t.Logf("%s, %s: Msg %d: %v", what, pathName(proc), num, err)
			if err == nil {
				t.Errorf("%s, %s: read succeeded", what, pathName(proc))
			}
		}
	}

	report("missing", missing)

	// SINGLE_USER, held by a pinned session of the second pool.
	holder, err := other.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := holder.ExecContext(ctx, `ALTER DATABASE [gosmo_a1_enter] SET SINGLE_USER WITH ROLLBACK IMMEDIATE; USE [gosmo_a1_enter]`); err != nil {
		t.Fatalf("single user: %v", err)
	}
	report("single user, held", d)
	if _, err := holder.ExecContext(ctx, `USE master; ALTER DATABASE [gosmo_a1_enter] SET MULTI_USER`); err != nil {
		t.Fatalf("multi user: %v", err)
	}
	holder.Close()

	if _, err := other.ExecContext(ctx, `ALTER DATABASE [gosmo_a1_enter] SET OFFLINE WITH ROLLBACK IMMEDIATE`); err != nil {
		t.Fatalf("offline: %v", err)
	}
	report("offline", d)
	if _, err := other.ExecContext(ctx, `ALTER DATABASE [gosmo_a1_enter] SET ONLINE`); err != nil {
		t.Fatalf("online: %v", err)
	}
}

// The plan-cache trap of CLAUDE.md § Read another database's catalog under
// USE [db]: an ad-hoc [db].sys.… plan keeps the database id it compiled
// against. Two databases are read, dropped and created again in the other
// order, so they swap ids, as a rerun's drops and creates did on 17: each read
// must still find its own database's marker. The literal [db].sys.… form from
// master runs alongside as the control.
func TestLiveProcReadSurvivesAnIdSwap(t *testing.T) {
	db, ctx, done := liveDBTimeout(t, 3*time.Minute)
	defer done()
	setProcReads(t, true)

	names := []string{"gosmo_a1_swap_x", "gosmo_a1_swap_y"}
	const q = `SELECT DB_NAME(), (SELECT TOP (1) name FROM sys.tables WHERE name LIKE N'marker%')`
	literal := func(n string) string {
		return `SELECT TOP (1) name FROM [` + n + `].sys.tables WHERE name LIKE N'marker%'`
	}
	ids := func() map[string]int {
		m := map[string]int{}
		for _, n := range names {
			var id int
			if err := db.QueryRowContext(ctx, `SELECT DB_ID(@p1)`, n).Scan(&id); err != nil {
				t.Fatalf("DB_ID(%s): %v", n, err)
			}
			m[n] = id
		}
		return m
	}
	create := func(order []string, round string) map[string]*Database {
		ds := map[string]*Database{}
		for _, n := range order {
			d, drop := liveScratchDB(t, db, ctx, n)
			t.Cleanup(drop)
			liveExecIn(t, d, ctx, `CREATE TABLE dbo.marker_`+round+` (id int)`)
			ds[n] = d
		}
		return ds
	}
	read := func(ds map[string]*Database, round string) {
		t.Helper()
		for _, n := range names {
			for range 2 { // the second from cache
				var in, marker, lit string
				if err := ds[n].queryRow(ctx, func(r *sql.Row) error { return r.Scan(&in, &marker) }, q); err != nil {
					t.Fatalf("read %s: %v", n, err)
				}
				if in != n || marker != "marker_"+round {
					t.Errorf("round %s: procRead of %s ran in %q and read %q, want marker_%s", round, n, in, marker, round)
				}
				if err := db.QueryRowContext(ctx, literal(n)).Scan(&lit); err != nil {
					t.Fatalf("literal read %s: %v", n, err)
				}
				if lit != "marker_"+round {
					t.Logf("round %s: control, literal [%s].sys.tables read %q — the trap", round, n, lit)
				}
			}
		}
	}

	ds := create(names, "first")
	before := ids()
	read(ds, "first")
	for _, d := range ds {
		db.ExecContext(ctx, "ALTER DATABASE ["+d.Name+"] SET SINGLE_USER WITH ROLLBACK IMMEDIATE; DROP DATABASE ["+d.Name+"]")
	}
	ds = create([]string{names[1], names[0]}, "second")
	after := ids()
	t.Logf("ids before %v, after %v", before, after)
	if after[names[0]] != before[names[1]] || after[names[1]] != before[names[0]] {
		t.Logf("the ids did not swap: this run proves nothing about the trap")
	}
	read(ds, "second")
}

// Parameters inside [db].sys.sp_executesql bind in db's collation, so a name
// built-in compared with one needs no COLLATE DATABASE_DEFAULT there — and the
// collated form still works, so the rule can stay as it is.
func TestLiveProcReadParameterCollation(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()

	var server string
	if err := db.QueryRowContext(ctx, `SELECT CAST(DATABASEPROPERTYEX(N'master', 'Collation') AS nvarchar(128))`).Scan(&server); err != nil {
		t.Fatal(err)
	}
	coll := "Latin1_General_100_CS_AS"
	if server == coll {
		coll = "Latin1_General_100_CI_AI"
	}
	d, drop := liveScratchDBCollated(t, db, ctx, "gosmo_a1_coll", coll)
	t.Cleanup(drop)
	liveExecIn(t, d, ctx, `CREATE TABLE dbo.Mixed (id int)`)

	for _, proc := range []bool{true, false} {
		setProcReads(t, proc)
		var pc string
		if err := d.queryRow(ctx, func(r *sql.Row) error { return r.Scan(&pc) },
			`SELECT CAST(SQL_VARIANT_PROPERTY(CAST(@p1 AS sql_variant), 'Collation') AS nvarchar(128))`, "x"); err != nil {
			t.Fatalf("%s: parameter collation: %v", pathName(proc), err)
		}
		t.Logf("%s: parameter binds in %s (master %s, %s %s)", pathName(proc), pc, server, d.Name, coll)
		if proc && pc != coll {
			t.Errorf("procRead: parameter collation %s, want the database's %s", pc, coll)
		}
	}

	// The raw call, with no recheckUse to rerun it: uncollated fails Msg 468
	// under useBatch and binds under procRead; the collated form works in both.
	const bare = `SELECT COUNT(*) FROM sys.tables t WHERE SCHEMA_NAME(t.schema_id) = @p1 AND t.name = @p2`
	const collated = `SELECT COUNT(*) FROM sys.tables t WHERE SCHEMA_NAME(t.schema_id) COLLATE DATABASE_DEFAULT = @p1 AND t.name = @p2`
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	for _, proc := range []bool{true, false} {
		for _, q := range []string{bare, collated} {
			text, args := d.useBatch(q), []any{"dbo", "Mixed"}
			if proc {
				var ok bool
				if text, args, ok = d.procRead(q, args); !ok {
					t.Fatal("procRead declined string arguments")
				}
			}
			var n int
			err := conn.QueryRowContext(ctx, text, args...).Scan(&n)
			collatedQ := q == collated
			t.Logf("%s, collated=%v: n=%d err=%v", pathName(proc), collatedQ, n, err)
			switch {
			case proc || collatedQ:
				if err != nil || n != 1 {
					t.Errorf("%s, collated=%v: n=%d err=%v, want 1", pathName(proc), collatedQ, n, err)
				}
			default:
				if me, ok := errors.AsType[mssql.Error](err); !ok || me.Number != msgCollationConflict {
					t.Errorf("useBatch, bare: err = %v, want Msg 468: the test cannot see the difference", err)
				}
			}
		}
	}
}
