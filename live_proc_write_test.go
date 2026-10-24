//go:build livedb

// Live proof for execIn (OPEN-THREADS.md § Idle pooled connections camp in
// the last database written to): a database-scoped write sent as an RPC call
// of [db].sys.sp_executesql, which every pooled write is, against the USE it
// replaced (a pooled connection switched by hand, then the statement) — the
// five rules live_proc_read_test.go proves for reads, each run both ways so a
// test that passes for execIn and fails for USE shows it can see the
// difference it claims. Then the shapes a write takes that a read does not:
// statements that must open their batch, atomic batches, ExecProc, writes
// that return a row, plans and bulk copy.
//
//	go test -tags livedb . -run TestLiveProcWrite -v \
//	  -livedb 'sqlserver://sa:PASS@host?TrustServerCertificate=true'
//
// Creates, backs up, restores and drops gosmo_a1w_* databases, and deletes
// the backup file it wrote.
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

// writeVia runs a database-scoped write by either path: d.exec (execIn), or,
// with rpc false, a pooled connection switched with USE and q run on it, as
// every pooled write went before 2026-10-24 — the control.
func writeVia(ctx context.Context, rpc bool, d *Database, q string, args ...any) error {
	if rpc {
		_, err := d.exec(ctx, q, args...)
		return err
	}
	conn, err := d.server.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if err := d.use(ctx, conn); err != nil {
		return err
	}
	_, err = conn.ExecContext(ctx, q, args...)
	return err
}

func writePath(rpc bool) string {
	if rpc {
		return "execIn"
	}
	return "USE"
}

// The write runs in d, and the pooled connection it used is left in its login
// database afterwards — under USE it is left in d.
func TestLiveProcWriteLeavesSessionInLoginDatabase(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()
	other := liveSecondDB(t)

	d, drop := liveScratchDB(t, db, ctx, "gosmo_a1w_home")
	defer drop()

	for _, rpc := range []bool{true, false} {
		table := "t_" + writePath(rpc)
		if err := writeVia(ctx, rpc, d, `CREATE TABLE dbo.`+table+` (db sysname DEFAULT DB_NAME())`); err != nil {
			t.Fatalf("%s: create: %v", writePath(rpc), err)
		}
		// Counted before the check below, whose read would take the parked
		// connection and reset it out of d.
		n := sessionsIn(t, ctx, other, d.Name)
		t.Logf("%s: %d idle session(s) left in %s", writePath(rpc), n, d.Name)
		if rpc && n != 0 {
			t.Errorf("execIn left %d session(s) in %s, want 0", n, d.Name)
		}
		if !rpc && n == 0 {
			t.Errorf("USE left no session in %s: the test cannot see camping", d.Name)
		}
		var in string
		if err := d.queryRow(ctx, func(r *sql.Row) error { return r.Scan(&in) }, `SELECT OBJECT_NAME(OBJECT_ID(@p1))`, "dbo."+table); err != nil || in != table {
			t.Errorf("%s: the table is not in %s: %q, %v", writePath(rpc), d.Name, in, err)
		}
		// Let the control's session go, so the next round starts clean.
		d.server.db.SetMaxIdleConns(0)
		d.server.db.SetMaxIdleConns(2)
	}
}

// The gate the plan names: a RESTORE … WITH REPLACE from another session
// succeeds straight after a gosmo write in the database. Under USE it fails
// Msg 3101, "Exclusive access could not be obtained".
func TestLiveProcWriteDoesNotBlockRestore(t *testing.T) {
	db, ctx, done := liveDBTimeout(t, 3*time.Minute)
	defer done()
	other := liveSecondDB(t)

	d, drop := liveScratchDB(t, db, ctx, "gosmo_a1w_restore")
	defer drop()
	liveExecIn(t, d, ctx, `CREATE TABLE dbo.w (id int)`)

	const bak = "gosmo_a1w_restore.bak" // the instance's default backup directory
	if _, err := other.ExecContext(ctx, `BACKUP DATABASE [gosmo_a1w_restore] TO DISK = N'`+bak+`' WITH INIT, COPY_ONLY`); err != nil {
		t.Fatalf("backup: %v", err)
	}
	var file string
	if err := other.QueryRowContext(ctx, `
SELECT TOP (1) f.physical_device_name
FROM msdb.dbo.backupset s JOIN msdb.dbo.backupmediafamily f ON f.media_set_id = s.media_set_id
WHERE s.database_name = N'gosmo_a1w_restore' ORDER BY s.backup_finish_date DESC`).Scan(&file); err != nil {
		t.Fatalf("find backup file: %v", err)
	}
	t.Cleanup(func() {
		if _, err := other.ExecContext(context.Background(), `EXEC master.sys.xp_delete_file 0, @p1`, file); err != nil {
			t.Logf("delete %s: %v (remove it by hand)", file, err)
		}
	})

	for _, rpc := range []bool{true, false} {
		if err := writeVia(ctx, rpc, d, `INSERT dbo.w VALUES (@p1)`, int64(1)); err != nil {
			t.Fatalf("%s: write: %v", writePath(rpc), err)
		}
		_, err := other.ExecContext(ctx, `RESTORE DATABASE [gosmo_a1w_restore] FROM DISK = @p1 WITH REPLACE`, file)
		t.Logf("%s: restore straight after a write: %v", writePath(rpc), err)
		if rpc && err != nil {
			t.Errorf("execIn: restore failed: %v", err)
		}
		if !rpc {
			me, ok := errors.AsType[mssql.Error](err)
			if !ok || !slices.ContainsFunc(me.All, func(e mssql.Error) bool { return e.Number == 3101 }) {
				t.Errorf("USE: restore err = %v, want Msg 3101: the test cannot see camping", err)
			}
		}
	}
}

// A database that cannot be entered fails the write, reported as the USE
// failure it always was, and nothing is written anywhere else — above all not
// in the login database the session stays in.
func TestLiveProcWriteRefusesADatabaseItCannotEnter(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()
	other := liveSecondDB(t)

	d, drop := liveScratchDB(t, db, ctx, "gosmo_a1w_enter")
	defer drop()
	missing := d.server.DatabaseRef("gosmo_a1w_missing")

	const probe = "gosmo_a1w_probe"
	strays := func() int {
		var n int
		if err := other.QueryRowContext(ctx, `SELECT COUNT(*) FROM master.sys.tables WHERE name = @p1`, probe).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	report := func(what string, target *Database, want int32) {
		for _, rpc := range []bool{true, false} {
			err := writeVia(ctx, rpc, target, `CREATE TABLE dbo.`+probe+` (id int)`)
			num := int32(0)
			if me, ok := errors.AsType[mssql.Error](err); ok {
				num = me.Number
			}
			t.Logf("%s, %s: Msg %d: %v", what, writePath(rpc), num, err)
			if err == nil {
				t.Errorf("%s, %s: write succeeded", what, writePath(rpc))
				continue
			}
			if rpc && (num != want || !strings.HasPrefix(err.Error(), "gosmo: USE "+target.Name+": ")) {
				t.Errorf("%s, execIn: err = %v, want Msg %d reported as gosmo: USE %s", what, err, want, target.Name)
			}
		}
		if n := strays(); n != 0 {
			t.Errorf("%s: %d table(s) %s written in master", what, n, probe)
			other.ExecContext(ctx, `DROP TABLE master.dbo.`+probe)
		}
	}

	report("missing", missing, 911)

	holder, err := other.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := holder.ExecContext(ctx, `ALTER DATABASE [gosmo_a1w_enter] SET SINGLE_USER WITH ROLLBACK IMMEDIATE; USE [gosmo_a1w_enter]`); err != nil {
		t.Fatalf("single user: %v", err)
	}
	report("single user, held", d, 924)
	if _, err := holder.ExecContext(ctx, `USE master; ALTER DATABASE [gosmo_a1w_enter] SET MULTI_USER`); err != nil {
		t.Fatalf("multi user: %v", err)
	}
	holder.Close()

	if _, err := other.ExecContext(ctx, `ALTER DATABASE [gosmo_a1w_enter] SET OFFLINE WITH ROLLBACK IMMEDIATE`); err != nil {
		t.Fatalf("offline: %v", err)
	}
	report("offline", d, 942)
	if _, err := other.ExecContext(ctx, `ALTER DATABASE [gosmo_a1w_enter] SET ONLINE`); err != nil {
		t.Fatalf("online: %v", err)
	}
}

// Two databases are written, dropped and created again in the other order,
// so they swap ids: each write must still land in its own database, the
// cached plan of the call notwithstanding.
func TestLiveProcWriteSurvivesAnIdSwap(t *testing.T) {
	db, ctx, done := liveDBTimeout(t, 3*time.Minute)
	defer done()
	var drops []func()
	defer func() {
		for _, drop := range drops {
			drop()
		}
	}()

	names := []string{"gosmo_a1w_swap_x", "gosmo_a1w_swap_y"}
	const w = `INSERT dbo.marker (db, round) VALUES (DB_NAME(), @p1)`
	create := func(order []string) map[string]*Database {
		ds := map[string]*Database{}
		for _, n := range order {
			d, drop := liveScratchDB(t, db, ctx, n)
			drops = append(drops, drop)
			liveExecIn(t, d, ctx, `CREATE TABLE dbo.marker (db sysname, round nvarchar(10))`)
			ds[n] = d
		}
		return ds
	}
	write := func(ds map[string]*Database, round string) {
		t.Helper()
		for _, n := range names {
			for range 2 { // the second from cache
				if _, err := ds[n].exec(ctx, w, round); err != nil {
					t.Fatalf("write %s: %v", n, err)
				}
			}
			var in string
			var rows int
			if err := ds[n].queryRow(ctx, func(r *sql.Row) error { return r.Scan(&rows, &in) },
				`SELECT COUNT(*), MIN(db) FROM dbo.marker WHERE round = @p1 AND db = DB_NAME()`, round); err != nil {
				t.Fatalf("read %s: %v", n, err)
			}
			if rows != 2 || in != n {
				t.Errorf("round %s: %s holds %d of its own rows (%q), want 2", round, n, rows, in)
			}
		}
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

	ds := create(names)
	before := ids()
	write(ds, "first")
	for _, d := range ds {
		db.ExecContext(ctx, "ALTER DATABASE ["+d.Name+"] SET SINGLE_USER WITH ROLLBACK IMMEDIATE; DROP DATABASE ["+d.Name+"]")
	}
	ds = create([]string{names[1], names[0]})
	after := ids()
	t.Logf("ids before %v, after %v", before, after)
	if after[names[0]] != before[names[1]] || after[names[1]] != before[names[0]] {
		t.Errorf("the ids did not swap (a leftover database?): the test cannot see the trap")
	}
	write(ds, "second")
}

// A write's parameters bind in d's collation too: an uncollated name
// built-in compared with one fails Msg 468 under USE from a database of
// another collation and runs under execIn.
func TestLiveProcWriteParameterCollation(t *testing.T) {
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
	d, drop := liveScratchDBCollated(t, db, ctx, "gosmo_a1w_coll", coll)
	defer drop()
	liveExecIn(t, d, ctx, `CREATE TABLE dbo.Mixed (n int)`)

	const bare = `INSERT dbo.Mixed (n) SELECT COUNT(*) FROM sys.tables t WHERE SCHEMA_NAME(t.schema_id) = @p1 AND t.name = @p2`
	// The control's USE goes in the same batch, so the parameters bind in the
	// login database's collation, as a useBatch read's do; a USE on its own
	// first would change the session's database before the call.
	for _, rpc := range []bool{true, false} {
		var err error
		if rpc {
			_, err = d.exec(ctx, bare, "dbo", "Mixed")
		} else {
			_, err = db.ExecContext(ctx, "USE "+quoteIdent(d.Name)+"; "+bare, "dbo", "Mixed")
		}
		t.Logf("%s: %v", writePath(rpc), err)
		if rpc && err != nil {
			t.Errorf("execIn: %v", err)
		}
		if !rpc {
			if me, ok := errors.AsType[mssql.Error](err); !ok || me.Number != msgCollationConflict {
				t.Errorf("USE batch: err = %v, want Msg 468: the test cannot see the difference", err)
			}
		}
	}
}

// What a write sends that a read never did, each through the real path and
// each leaving no session in the database.
func TestLiveProcWriteShapes(t *testing.T) {
	db, ctx, done := liveDBTimeout(t, 2*time.Minute)
	defer done()
	other := liveSecondDB(t)

	d, drop := liveScratchDB(t, db, ctx, "gosmo_a1w_shapes")
	defer drop()
	// home is called straight after the writes it checks, before any read:
	// a read would take a parked connection and reset it out of d.
	home := func(what string) {
		t.Helper()
		if n := sessionsIn(t, ctx, other, d.Name); n != 0 {
			t.Errorf("%s left %d session(s) in %s", what, n, d.Name)
		}
	}
	exists := func(what, name string) {
		t.Helper()
		var id sql.NullInt64
		if err := d.queryRow(ctx, func(r *sql.Row) error { return r.Scan(&id) }, `SELECT OBJECT_ID(@p1)`, name); err != nil || !id.Valid {
			t.Errorf("%s: %s not created in %s (%v)", what, name, d.Name, err)
		}
	}

	// The control: a write under USE parks a session home can see.
	if err := writeVia(ctx, false, d, `DECLARE @parked int`); err != nil {
		t.Fatal(err)
	}
	if n := sessionsIn(t, ctx, other, d.Name); n == 0 {
		t.Fatalf("USE left no session in %s: home cannot see camping", d.Name)
	}
	d.server.db.SetMaxIdleConns(0)
	d.server.db.SetMaxIdleConns(2)

	// Statements that must open their batch: the call's statement is one.
	for _, s := range []struct{ what, stmt, name string }{
		{"CREATE SCHEMA", `CREATE SCHEMA app AUTHORIZATION dbo`, ""},
		{"CREATE TABLE", `CREATE TABLE app.t (id int IDENTITY PRIMARY KEY, v nvarchar(20))`, "app.t"},
		{"CREATE VIEW", `CREATE VIEW app.v AS SELECT id, v FROM app.t`, "app.v"},
		{"CREATE PROCEDURE", "CREATE PROCEDURE app.whereami @db sysname OUTPUT, @n int AS\nBEGIN\n  SET @db = DB_NAME();\n  RETURN @n + 1;\nEND", "app.whereami"},
		{"CREATE FUNCTION", `CREATE FUNCTION app.f() RETURNS int AS BEGIN RETURN 1 END`, "app.f"},
		{"CREATE TRIGGER", `CREATE TRIGGER app.tr ON app.t AFTER INSERT AS SET NOCOUNT ON`, "app.tr"},
		{"CREATE SEQUENCE", `CREATE SEQUENCE app.s START WITH 41`, "app.s"},
	} {
		if _, err := d.exec(ctx, s.stmt); err != nil {
			t.Fatalf("%s: %v", s.what, err)
		}
		home(s.what)
		if s.name != "" {
			exists(s.what, s.name)
		}
	}

	// Rows affected come back from the call.
	res, err := d.exec(ctx, `INSERT app.t (v) VALUES (@p1), (@p2)`, "a", "b")
	if err != nil {
		t.Fatal(err)
	}
	if n, err := res.RowsAffected(); err != nil || n != 2 {
		t.Errorf("RowsAffected = %d, %v, want 2", n, err)
	}

	// An atomic batch's statements all apply, or none: its TRY/CATCH and
	// transaction balance inside the call, so a failure is the statement's
	// own error, not Msg 266.
	if err := d.execAtomic(ctx, []string{`INSERT app.t (v) VALUES (N'c')`, `INSERT app.t (v) VALUES (N'd')`}); err != nil {
		t.Errorf("execAtomic: %v", err)
	}
	err = d.execAtomic(ctx, []string{`INSERT app.t (v) VALUES (N'e')`, `INSERT app.t (id, v) VALUES (1, N'f')`})
	if me, ok := errors.AsType[mssql.Error](err); !ok || me.Number != 544 {
		t.Errorf("execAtomic, second statement failing: %v, want Msg 544", err)
	}
	home("execAtomic")
	var rows int
	if err := d.queryRow(ctx, func(r *sql.Row) error { return r.Scan(&rows) }, `SELECT COUNT(*) FROM app.t`); err != nil || rows != 4 {
		t.Errorf("app.t holds %d rows (%v), want 4: the failed batch's first insert stayed", rows, err)
	}

	// ExecProc: by three-part name, OUTPUT parameters and the return status.
	var in string
	r, err := d.ExecProc(ctx, "app", "whereami", Out("db", &in), In("n", 6))
	if err != nil || in != d.Name || r.ReturnStatus != 7 {
		t.Errorf("ExecProc: db %q, status %d, %v; want %q, 7", in, r.ReturnStatus, err, d.Name)
	}
	// A sys procedure named with the database runs in it.
	if _, err := d.ExecProc(ctx, "sys", "sp_rename", In("objname", "app.v"), In("newname", "v2")); err != nil {
		t.Errorf("ExecProc sys.sp_rename: %v", err)
	}
	home("ExecProc")
	exists("sys.sp_rename", "app.v2")

	// Writes that return a row.
	if v, err := d.SequenceRef("app", "s").NextValue(ctx); err != nil || v != 41 {
		t.Errorf("NextValue = %d, %v, want 41", v, err)
	}
	home("NextValue")
	files, err := d.Files(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if _, err := f.Shrink(ctx, ShrinkFileOptions{}); err != nil {
			t.Errorf("Shrink %s: %v", f.Name, err)
		}
	}
	home("Shrink")

	// A SET in a write ends with the call: the session's own option is
	// unchanged afterwards.
	conn, err := d.server.db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	call, cargs, _ := d.procRead(`SET XACT_ABORT ON`, nil)
	var opts int
	if _, err := conn.ExecContext(ctx, call, cargs...); err != nil {
		t.Fatal(err)
	}
	if err := conn.QueryRowContext(ctx, `SELECT @@OPTIONS & 16384`).Scan(&opts); err != nil || opts != 0 {
		t.Errorf("XACT_ABORT after a write that set it: %d, %v, want off", opts, err)
	}
	conn.Close()

	// The two that switch a session (withSessionIn) switch it back.
	plan, err := d.EstimatedPlan(ctx, `SELECT v FROM app.t WHERE id = 1`)
	if err != nil || !strings.Contains(plan.XML, `Table="[t]"`) {
		t.Errorf("EstimatedPlan: %v, or no plan of app.t", err)
	}
	if _, err := d.ActualPlan(ctx, `SELECT v FROM app.t WHERE id = 1`); err != nil {
		t.Errorf("ActualPlan: %v", err)
	}
	n, err := d.BulkInsert(ctx, BulkCopy{Schema: "app", Table: "t", Columns: []string{"v"}},
		SliceRows([][]any{{"x"}, {"y"}}))
	if err != nil || n != 2 {
		t.Errorf("BulkInsert = %d, %v, want 2", n, err)
	}
	home("plans, BulkInsert")

	// A transaction's session is switched to d for its statements, and back
	// before COMMIT or ROLLBACK returns it to the pool.
	for _, commit := range []bool{true, false} {
		err := d.server.InTransaction(ctx, func(ctx context.Context) error {
			if _, err := d.exec(ctx, `INSERT app.t (v) VALUES (N'tx')`); err != nil {
				return err
			}
			if !commit {
				return errors.New("roll back")
			}
			return nil
		})
		if commit != (err == nil) {
			t.Fatalf("InTransaction, commit=%v: %v", commit, err)
		}
		if commit {
			home("InTransaction, committed")
		} else {
			home("InTransaction, rolled back")
		}
	}
}
