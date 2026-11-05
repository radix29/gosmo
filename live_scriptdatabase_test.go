//go:build livedb

// Live verification that ScriptDatabase produces a real script from a
// bare Server.DatabaseRef(name) handle.
//
// The handle carries no metadata by design, and ScriptDatabase used to render
// it anyway — "SET RECOVERY ;" and "COMPATIBILITY_LEVEL = 0", neither of them
// valid T-SQL. Only a live server can settle both halves: that the refresh
// returns the same metadata DatabaseByName would have, and that the
// script the two paths produce is byte-for-byte the same.
//
//	go test -tags livedb . -run TestLiveScriptDatabase -v \
//	  -livedb 'sqlserver://sa:PASS@host?TrustServerCertificate=true'
//
// Creates and drops its own throwaway database; touches nothing else.
package gosmo

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestLiveScriptDatabaseFromABareHandle(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()

	const name = "gosmo_scriptdb_live"
	full, drop := liveScratchDB(t, db, ctx, name)
	defer drop()

	srv := full.server
	if _, err := db.ExecContext(ctx, "ALTER DATABASE ["+name+"] SET RECOVERY BULK_LOGGED"); err != nil {
		t.Fatalf("set recovery: %v", err)
	}
	// Re-read: full was fetched before the ALTER, so its cached recovery
	// model is now stale — which is itself the reason the bare handle has to
	// read rather than guess.
	var err error
	full, err = srv.DatabaseByName(ctx, name)
	if err != nil {
		t.Fatalf("DatabaseByName: %v", err)
	}

	opts := DefaultScriptOptions()
	opts.IncludeHeaders = false

	wantScript, err := NewScripter(full, opts).ScriptDatabase(ctx)
	if err != nil {
		t.Fatalf("script from a fetched database: %v", err)
	}
	bare, err := NewScripter(srv.DatabaseRef(name), opts).ScriptDatabase(ctx)
	if err != nil {
		t.Fatalf("script from a bare handle: %v", err)
	}
	if bare != wantScript {
		t.Errorf("bare handle script differs:\n--- bare ---\n%s\n--- fetched ---\n%s", bare, wantScript)
	}
	for _, want := range []string{
		"CREATE DATABASE [" + name + "]",
		"SET RECOVERY BULK_LOGGED;",
		"SET COMPATIBILITY_LEVEL = ",
	} {
		if !strings.Contains(bare, want) {
			t.Errorf("script missing %q:\n%s", want, bare)
		}
	}
	// The shapes the bug produced. Neither parses.
	for _, bad := range []string{"SET RECOVERY ;", "COMPATIBILITY_LEVEL = 0;"} {
		if strings.Contains(bare, bad) {
			t.Errorf("script contains %q:\n%s", bad, bare)
		}
	}

	// And it has to actually run. The script drops straight back in against a
	// server that no longer has the database.
	if _, err := db.ExecContext(ctx, "ALTER DATABASE ["+name+"] SET SINGLE_USER WITH ROLLBACK IMMEDIATE"); err != nil {
		t.Fatalf("pre-drop prep: %v", err)
	}
	if _, err := db.ExecContext(ctx, "DROP DATABASE ["+name+"]"); err != nil {
		t.Fatalf("drop before replay: %v", err)
	}
	for _, batch := range strings.Split(bare, "\nGO") {
		if strings.TrimSpace(batch) == "" {
			continue
		}
		if _, err := db.ExecContext(ctx, batch); err != nil {
			t.Fatalf("replaying %.60q: %v", strings.TrimSpace(batch), err)
		}
	}
	replayed, err := srv.DatabaseByName(ctx, name)
	if err != nil {
		t.Fatalf("database after replay: %v", err)
	}
	if replayed.RecoveryModel != RecoveryModelBulkLogged {
		t.Errorf("replayed recovery model = %q, want BULK_LOGGED", replayed.RecoveryModel)
	}
}

// A DROP AND CREATE script of a database runs: the guarded DROP removes it
// (from master, which the script switches to), and the CREATE puts it back
// with its recovery model. Run twice, so the guards are exercised both ways.
func TestLiveScriptDatabaseDropAndCreateRuns(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()

	const name = "gosmo_scriptdb_drop_live"
	d, drop := liveScratchDB(t, db, ctx, name)
	defer drop()
	if _, err := db.ExecContext(ctx, "ALTER DATABASE ["+name+"] SET RECOVERY SIMPLE"); err != nil {
		t.Fatalf("set recovery: %v", err)
	}

	opts := DefaultScriptOptions()
	opts.IncludeHeaders = false
	opts.Verb = ScriptDropAndCreate
	script, err := NewScripter(d.server.DatabaseRef(name), opts).ScriptDatabase(ctx)
	if err != nil {
		t.Fatalf("ScriptDatabase: %v", err)
	}
	for range 2 {
		for _, batch := range strings.Split(script, "\nGO") {
			if strings.TrimSpace(batch) == "" {
				continue
			}
			if _, err := db.ExecContext(ctx, batch); err != nil {
				t.Fatalf("running %.60q: %v", strings.TrimSpace(batch), err)
			}
		}
	}
	got, err := d.server.DatabaseByName(ctx, name)
	if err != nil {
		t.Fatalf("database after the script: %v", err)
	}
	if got.RecoveryModel != RecoveryModelSimple {
		t.Errorf("recovery model = %q, want SIMPLE", got.RecoveryModel)
	}

	opts.Verb = ScriptDrop
	dropScript, err := NewScripter(d.server.DatabaseRef(name), opts).ScriptDatabase(ctx)
	if err != nil {
		t.Fatalf("ScriptDatabase DROP: %v", err)
	}
	for _, batch := range strings.Split(dropScript, "\nGO") {
		if strings.TrimSpace(batch) == "" {
			continue
		}
		if _, err := db.ExecContext(ctx, batch); err != nil {
			t.Fatalf("running %.60q: %v", strings.TrimSpace(batch), err)
		}
	}
	if _, err := d.server.DatabaseByName(ctx, name); !errors.Is(err, ErrNotFound) {
		t.Errorf("after the DROP script, DatabaseByName = %v, want ErrNotFound", err)
	}
}

// The file layout round-trips: a database with two PRIMARY files, a second
// ROWS filegroup that is the default and read-only, an empty filegroup, a
// memory-optimized container, a FILESTREAM container where the instance has
// FILESTREAM on, and two logs is scripted, the script replayed under a new
// name (which, the files being named after the database, also rewrites
// every logical name and path), and the two databases' files and
// filegroups compared row for row.
func TestLiveScriptDatabaseFileLayoutRoundTrips(t *testing.T) {
	db, ctx, done := liveDBTimeout(t, 3*time.Minute)
	t.Cleanup(done) // after the drops below, which t.Cleanup runs first

	const name, copyName = "gosmo_scriptdb_files", "gosmo_scriptdb_files_copy"
	dropDB := func(n string) {
		c := context.Background()
		db.ExecContext(c, "IF DB_ID('"+n+"') IS NOT NULL ALTER DATABASE ["+n+"] SET SINGLE_USER WITH ROLLBACK IMMEDIATE")
		db.ExecContext(c, "IF DB_ID('"+n+"') IS NOT NULL DROP DATABASE ["+n+"]")
	}
	dropDB(copyName)
	dropDB(name)
	t.Cleanup(func() { dropDB(copyName); dropDB(name) })

	srv, err := NewServer(ctx, db)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	dir := srv.Info().DefaultDataPath
	if dir == "" {
		t.Skip("the instance reports no default data path")
	}
	path := func(suffix string) string { return QuoteLiteral(dir + name + suffix) }
	var fsLevel int
	if err := db.QueryRowContext(ctx, "SELECT CAST(SERVERPROPERTY('FilestreamEffectiveLevel') AS int)").Scan(&fsLevel); err != nil {
		t.Fatalf("FilestreamEffectiveLevel: %v", err)
	}
	fs := ""
	if fsLevel > 0 {
		fs = `,
 FILEGROUP [` + name + `_fs] CONTAINS FILESTREAM ( NAME = ` + name + `_fs1, FILENAME = ` + path("_fs1") + ` )`
	} else {
		t.Log("FILESTREAM is off here: the FILESTREAM filegroup is not covered")
	}
	create := `CREATE DATABASE [` + name + `] ON PRIMARY
 ( NAME = ` + name + `, FILENAME = ` + path(".mdf") + `, SIZE = 10MB, MAXSIZE = UNLIMITED, FILEGROWTH = 5MB ),
 ( NAME = ` + name + `_p2, FILENAME = ` + path("_p2.ndf") + `, SIZE = 9MB, MAXSIZE = 50MB, FILEGROWTH = 0 ),
 FILEGROUP [` + name + `_fg2] ( NAME = ` + name + `_fg2a, FILENAME = ` + path("_fg2a.ndf") + `, SIZE = 8MB, FILEGROWTH = 10% ),
 FILEGROUP [` + name + `_mo] CONTAINS MEMORY_OPTIMIZED_DATA ( NAME = ` + name + `_mo1, FILENAME = ` + path("_mo1") + ` )` + fs + `
 LOG ON ( NAME = ` + name + `_log, FILENAME = ` + path(".ldf") + `, SIZE = 8MB, MAXSIZE = 1GB, FILEGROWTH = 15% ),
 ( NAME = ` + name + `_log2, FILENAME = ` + path("_log2.ldf") + `, SIZE = 4MB, FILEGROWTH = 1MB )`
	for _, q := range []string{
		create,
		"ALTER DATABASE [" + name + "] ADD FILEGROUP [" + name + "_empty]",
		"ALTER DATABASE [" + name + "] MODIFY FILEGROUP [" + name + "_fg2] DEFAULT",
		"ALTER DATABASE [" + name + "] MODIFY FILEGROUP [" + name + "_fg2] READ_ONLY",
	} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatalf("setup %.60q: %v", q, err)
		}
	}

	opts := DefaultScriptOptions()
	opts.IncludeHeaders = false
	script, err := NewScripter(srv.DatabaseRef(name), opts).ScriptDatabase(ctx)
	if err != nil {
		t.Fatalf("ScriptDatabase: %v", err)
	}
	t.Logf("script:\n%s", script)
	replay := strings.ReplaceAll(script, name, copyName)
	for _, batch := range strings.Split(replay, "\nGO") {
		if strings.TrimSpace(batch) == "" {
			continue
		}
		if _, err := db.ExecContext(ctx, batch); err != nil {
			t.Fatalf("replaying %.200q: %v", strings.TrimSpace(batch), err)
		}
	}

	// Each database's layout as text, its own name taken out of every
	// logical name, path and filegroup.
	layout := func(n string) []string {
		// Under USE, never [n].sys… from master: see the scoped
		// configurations read in the in-database round trip.
		q := `USE [` + n + `];
SELECT CONCAT(mf.type_desc COLLATE DATABASE_DEFAULT, '|', mf.name COLLATE DATABASE_DEFAULT, '|',
              mf.physical_name COLLATE DATABASE_DEFAULT, '|', mf.size, '|',
              mf.max_size, '|', mf.growth, '|', mf.is_percent_growth)
FROM   sys.master_files mf WHERE mf.database_id = DB_ID(@p1)
UNION ALL
SELECT CONCAT('FG|', fg.name COLLATE DATABASE_DEFAULT, '|', fg.type_desc COLLATE DATABASE_DEFAULT, '|', fg.is_default, '|', fg.is_read_only)
FROM   sys.filegroups fg`
		rows, err := db.QueryContext(ctx, q, n)
		if err != nil {
			t.Fatalf("layout of %s: %v", n, err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				t.Fatalf("layout of %s: %v", n, err)
			}
			out = append(out, strings.ReplaceAll(s, n, "DB"))
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("layout of %s: %v", n, err)
		}
		slices.Sort(out)
		return out
	}
	want, got := layout(name), layout(copyName)
	if !slices.Equal(got, want) {
		t.Errorf("replayed layout differs:\n--- replay ---\n%s\n--- source ---\n%s",
			strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// replayScript runs script batch by batch on one connection, as a query
// editor would: a USE in one batch holds for the next, which a pooled
// connection, reset on reuse, would not keep.
func replayScript(t *testing.T, ctx context.Context, db *sql.DB, script string) {
	t.Helper()
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("replay connection: %v", err)
	}
	defer conn.Close()
	for _, batch := range strings.Split(script, "\nGO") {
		if strings.TrimSpace(batch) == "" {
			continue
		}
		if _, err := conn.ExecContext(ctx, batch); err != nil {
			t.Fatalf("replaying %.200q: %v", strings.TrimSpace(batch), err)
		}
	}
}

// The database options round-trip: a database with every scripted option
// away from its default — the ANSI family, AUTO_*, cursors, triggers, broker,
// PAGE_VERIFY, TRUSTWORTHY, DB_CHAINING, PARAMETERIZATION, both snapshot
// options, TARGET_RECOVERY_TIME, DELAYED_DURABILITY, ADR from 2019, change
// tracking, Query Store with non-default settings, a throwaway owner,
// RESTRICTED_USER, READ_ONLY, and where the instance allows it WITH
// FILESTREAM — is scripted, replayed under a
// new name, and the two databases' settings compared.
func TestLiveScriptDatabaseOptionsRoundTrip(t *testing.T) {
	db, ctx, done := liveDBTimeout(t, 3*time.Minute)
	t.Cleanup(done) // after the drops below, which t.Cleanup runs first

	const name, copyName, owner = "gosmo_scriptdb_opts", "gosmo_scriptdb_opts_copy", "gosmo_scriptdb_owner"
	dropAll := func() {
		c := context.Background()
		for _, n := range []string{copyName, name} {
			db.ExecContext(c, "IF DB_ID('"+n+"') IS NOT NULL ALTER DATABASE ["+n+"] SET SINGLE_USER WITH ROLLBACK IMMEDIATE")
			db.ExecContext(c, "IF DB_ID('"+n+"') IS NOT NULL DROP DATABASE ["+n+"]")
		}
		db.ExecContext(c, "IF SUSER_ID('"+owner+"') IS NOT NULL DROP LOGIN ["+owner+"]")
	}
	dropAll()
	t.Cleanup(dropAll)

	srv, err := NewServer(ctx, db)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	major := srv.Info().VersionMajor
	var fsLevel int
	if err := db.QueryRowContext(ctx, "SELECT CAST(SERVERPROPERTY('FilestreamEffectiveLevel') AS int)").Scan(&fsLevel); err != nil {
		t.Fatalf("FilestreamEffectiveLevel: %v", err)
	}
	create := "CREATE DATABASE [" + name + "]"
	if fsLevel >= 2 {
		create += " WITH FILESTREAM (NON_TRANSACTED_ACCESS = READ_ONLY, DIRECTORY_NAME = N'" + name + "_dir')"
	} else {
		t.Log("FILESTREAM non-transacted access is off here: WITH FILESTREAM is not covered")
	}
	waitStats := ""
	if major >= 14 {
		waitStats = ", WAIT_STATS_CAPTURE_MODE = OFF"
	}
	set := "ALTER DATABASE [" + name + "] SET "
	// Broker's state on a new database follows the instance's model; flip
	// whatever it gave, so the replay cannot match by inheriting it too.
	var broker bool
	if err := db.QueryRowContext(ctx, "SELECT is_broker_enabled FROM sys.databases WHERE name = 'model'").Scan(&broker); err != nil {
		t.Fatalf("model broker: %v", err)
	}
	flipBroker := "ENABLE_BROKER"
	if broker {
		flipBroker = "DISABLE_BROKER"
	}
	adr := ""
	if major >= 15 {
		adr = "ON"
	} else {
		t.Log("pre-2019 instance: ACCELERATED_DATABASE_RECOVERY is not covered")
	}
	for _, q := range []string{
		create,
		"CREATE LOGIN [" + owner + "] WITH PASSWORD = N'Gosmo#Owner1', CHECK_POLICY = OFF",
		"ALTER AUTHORIZATION ON DATABASE::[" + name + "] TO [" + owner + "]",
		set + "AUTO_CLOSE ON", set + "AUTO_SHRINK ON", set + "AUTO_CREATE_STATISTICS OFF",
		set + "AUTO_UPDATE_STATISTICS_ASYNC ON", set + "PAGE_VERIFY TORN_PAGE_DETECTION",
		set + "TRUSTWORTHY ON", set + "READ_COMMITTED_SNAPSHOT ON WITH ROLLBACK IMMEDIATE",
		set + "ALLOW_SNAPSHOT_ISOLATION ON",
		set + "CHANGE_TRACKING = ON (CHANGE_RETENTION = 5 HOURS, AUTO_CLEANUP = OFF)",
		set + "QUERY_STORE = ON (OPERATION_MODE = READ_WRITE, MAX_STORAGE_SIZE_MB = 321, INTERVAL_LENGTH_MINUTES = 15, QUERY_CAPTURE_MODE = ALL, CLEANUP_POLICY = (STALE_QUERY_THRESHOLD_DAYS = 12)" + waitStats + ")",
		set + "ANSI_NULL_DEFAULT ON", set + "ANSI_NULLS ON", set + "ANSI_PADDING ON",
		set + "ANSI_WARNINGS ON", set + "ARITHABORT ON", set + "CONCAT_NULL_YIELDS_NULL ON",
		set + "NUMERIC_ROUNDABORT ON", set + "QUOTED_IDENTIFIER ON", set + "RECURSIVE_TRIGGERS ON",
		set + "CURSOR_CLOSE_ON_COMMIT ON", set + "CURSOR_DEFAULT LOCAL", set + flipBroker,
		set + "DATE_CORRELATION_OPTIMIZATION ON", set + "HONOR_BROKER_PRIORITY ON",
		set + "DB_CHAINING ON", set + "PARAMETERIZATION FORCED",
		set + "TARGET_RECOVERY_TIME = 90 SECONDS", set + "DELAYED_DURABILITY = ALLOWED",
		set + "ACCELERATED_DATABASE_RECOVERY = " + adr,
		// Last: READ_ONLY refuses every ALTER after it.
		set + "RESTRICTED_USER", set + "READ_ONLY",
	} {
		if strings.HasSuffix(q, "RECOVERY = ") {
			continue
		}
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatalf("setup %.80q: %v", q, err)
		}
	}

	opts := DefaultScriptOptions()
	opts.IncludeHeaders = false
	script, err := NewScripter(srv.DatabaseRef(name), opts).ScriptDatabase(ctx)
	if err != nil {
		t.Fatalf("ScriptDatabase: %v", err)
	}
	t.Logf("script:\n%s", script)
	// Once in 30 runs on 14 the script lacked CHANGE_TRACKING although the
	// source had it on (OPEN-THREADS.md). Re-read it at once, so the next
	// miss shows whether the read was momentary or the scripter dropped it.
	if !strings.Contains(script, "SET CHANGE_TRACKING") {
		for i := range 5 {
			ct, err := srv.DatabaseRef(name).ChangeTracking(ctx)
			var st string
			db.QueryRowContext(ctx, `SELECT CONCAT(state_desc, ' collation=', ISNULL(collation_name, 'NULL'),
       ' ct_rows=', (SELECT COUNT(*) FROM sys.change_tracking_databases WHERE database_id = DB_ID(@p1)),
       ' sessions=', (SELECT COUNT(*) FROM sys.dm_exec_sessions WHERE database_id = DB_ID(@p1)))
FROM sys.databases WHERE name = @p1`, name).Scan(&st)
			t.Errorf("script has no SET CHANGE_TRACKING; re-read %d: %+v, err %v, %s", i, ct, err, st)
			time.Sleep(20 * time.Millisecond)
		}
	}
	replayScript(t, ctx, db, strings.ReplaceAll(script, name, copyName))

	// Read from inside the database: AUTO_CLOSE is on, and a closed
	// database reports NULL for several of these at server scope.
	settings := func(n string) string {
		q := `USE [` + n + `];
SELECT CONCAT(SUSER_SNAME(d.owner_sid) COLLATE DATABASE_DEFAULT, '|', d.page_verify_option_desc COLLATE DATABASE_DEFAULT, '|', d.containment_desc COLLATE DATABASE_DEFAULT, '|',
       d.snapshot_isolation_state_desc COLLATE DATABASE_DEFAULT, '|', d.is_read_committed_snapshot_on, '|',
       d.is_auto_close_on, d.is_auto_shrink_on, d.is_auto_create_stats_on,
       d.is_auto_update_stats_on, d.is_auto_update_stats_async_on, '|', d.is_trustworthy_on, '|',
       d.recovery_model_desc COLLATE DATABASE_DEFAULT, '|', d.compatibility_level, '|', d.collation_name COLLATE DATABASE_DEFAULT, '|',
       ctd.retention_period, ctd.retention_period_units_desc COLLATE DATABASE_DEFAULT, ctd.is_auto_cleanup_on, '|',
       fo.non_transacted_access_desc COLLATE DATABASE_DEFAULT, '|', fo.directory_name COLLATE DATABASE_DEFAULT, '|',
       qs.desired_state_desc COLLATE DATABASE_DEFAULT, qs.max_storage_size_mb, '/', qs.flush_interval_seconds, '/',
       qs.interval_length_minutes, '/', qs.max_plans_per_query, '/', qs.query_capture_mode_desc COLLATE DATABASE_DEFAULT, '/',
       qs.size_based_cleanup_mode_desc COLLATE DATABASE_DEFAULT, '/', qs.stale_query_threshold_days, '|',
       d.is_ansi_null_default_on, d.is_ansi_nulls_on, d.is_ansi_padding_on, d.is_ansi_warnings_on,
       d.is_arithabort_on, d.is_concat_null_yields_null_on, d.is_numeric_roundabort_on,
       d.is_quoted_identifier_on, d.is_recursive_triggers_on, d.is_cursor_close_on_commit_on,
       d.is_local_cursor_default, d.is_broker_enabled, d.is_date_correlation_on,
       d.is_honor_broker_priority_on, d.is_db_chaining_on, d.is_parameterization_forced, '|',
       d.target_recovery_time_in_seconds, '|', d.delayed_durability_desc COLLATE DATABASE_DEFAULT, '|',
       ` + colSince(major, SQLServer2019, "d.is_accelerated_database_recovery_on", "''") + `, '|',
       d.user_access_desc COLLATE DATABASE_DEFAULT, '|', d.is_read_only)
FROM   sys.databases d
LEFT   JOIN sys.change_tracking_databases ctd ON ctd.database_id = d.database_id
LEFT   JOIN sys.database_filestream_options fo ON fo.database_id = d.database_id
CROSS  JOIN [` + n + `].sys.database_query_store_options qs
WHERE  d.name = @p1`
		var s string
		if err := db.QueryRowContext(ctx, q, n).Scan(&s); err != nil {
			t.Fatalf("settings of %s: %v", n, err)
		}
		return strings.ReplaceAll(s, n, "DB")
	}
	want, got := settings(name), settings(copyName)
	t.Logf("source settings: %s", want)
	if got != want {
		t.Errorf("replayed settings differ:\n%s replay %s\n source %s", settingsFieldDiff(got, want), got, want)
	}

	// Once AUTO_CLOSE has closed the copy, sys.databases reports NULL for
	// its collation, TARGET_RECOVERY_TIME, DELAYED_DURABILITY and ADR. The
	// script must not change, and Options must still read. Each waits for
	// the close on its own: the server-scope Options read reopens it.
	waitClosed := func() bool {
		for range 40 {
			var coll sql.NullString
			if err := db.QueryRowContext(ctx, "SELECT collation_name FROM sys.databases WHERE name = @p1", copyName).Scan(&coll); err != nil {
				t.Fatalf("collation of %s: %v", copyName, err)
			}
			if !coll.Valid {
				return true
			}
			time.Sleep(250 * time.Millisecond)
		}
		t.Log("the copy never auto-closed: the closed-database read is not covered")
		return false
	}
	if !waitClosed() {
		return
	}
	again, err := NewScripter(srv.DatabaseRef(copyName), opts).ScriptDatabase(ctx)
	if err != nil {
		t.Fatalf("ScriptDatabase of the auto-closed copy: %v", err)
	}
	if again = strings.ReplaceAll(again, copyName, name); again != script {
		t.Errorf("script of the auto-closed copy differs:\n--- closed copy ---\n%s\n--- source ---\n%s", again, script)
	}
	if !waitClosed() {
		return
	}
	if _, err := srv.DatabaseRef(copyName).Options(ctx); err != nil {
		t.Errorf("Options on an auto-closed database: %v", err)
	}
}

// optionsSettingsFields names the '|'-separated fields of the options round
// trip's settings line, in order; a run of digits is one field per flag.
var optionsSettingsFields = []string{
	"owner", "PAGE_VERIFY", "containment", "ALLOW_SNAPSHOT_ISOLATION", "READ_COMMITTED_SNAPSHOT",
	"AUTO_CLOSE/SHRINK/CREATE_STATS/UPDATE_STATS/UPDATE_STATS_ASYNC", "TRUSTWORTHY",
	"recovery model", "compatibility level", "collation", "change tracking",
	"FILESTREAM access", "FILESTREAM directory", "Query Store",
	"ANSI_NULL_DEFAULT/ANSI_NULLS/ANSI_PADDING/ANSI_WARNINGS/ARITHABORT/CONCAT_NULL_YIELDS_NULL/" +
		"NUMERIC_ROUNDABORT/QUOTED_IDENTIFIER/RECURSIVE_TRIGGERS/CURSOR_CLOSE_ON_COMMIT/" +
		"CURSOR_DEFAULT LOCAL/broker/DATE_CORRELATION/HONOR_BROKER_PRIORITY/DB_CHAINING/PARAMETERIZATION FORCED",
	"TARGET_RECOVERY_TIME", "DELAYED_DURABILITY", "ADR", "user access", "READ_ONLY",
}

// settingsFieldDiff lists, one per line, each field of the options round
// trip's settings line that differs — a whole-line diff of ~200 characters
// is where the one flake on 14 went unread.
func settingsFieldDiff(got, want string) string {
	g, w := strings.Split(got, "|"), strings.Split(want, "|")
	var sb strings.Builder
	for i := range max(len(g), len(w)) {
		var gv, wv, label string
		if i < len(g) {
			gv = g[i]
		}
		if i < len(w) {
			wv = w[i]
		}
		if i < len(optionsSettingsFields) {
			label = optionsSettingsFields[i]
		} else {
			label = fmt.Sprintf("field %d", i)
		}
		if gv != wv {
			fmt.Fprintf(&sb, " %s: replay %q, source %q\n", label, gv, wv)
		}
	}
	return sb.String()
}

// A snapshot scripts as CREATE … AS SNAPSHOT OF and replays as one: the
// snapshot is scripted, dropped, the script run, and what comes back is a
// snapshot of the same source over the same sparse files.
func TestLiveScriptDatabaseSnapshotRoundTrips(t *testing.T) {
	db, ctx, done := liveDB(t)
	t.Cleanup(done) // after the drops below, which t.Cleanup runs first

	const name, snapName = "gosmo_scriptdb_snapsrc", "gosmo_scriptdb_snap"
	dropSnap := func() {
		db.ExecContext(context.Background(), "IF DB_ID('"+snapName+"') IS NOT NULL DROP DATABASE ["+snapName+"]")
	}
	dropSnap()
	src, drop := liveScratchDB(t, db, ctx, name)
	t.Cleanup(drop)
	t.Cleanup(dropSnap)

	srv := src.server
	if _, err := srv.CreateDatabaseSnapshot(ctx, CreateDatabaseSnapshotRequest{Name: snapName, SourceDatabase: name}); err != nil {
		t.Fatalf("CreateDatabaseSnapshot: %v", err)
	}
	files := func() []string {
		fs, err := srv.DatabaseFiles(ctx, snapName)
		if err != nil {
			t.Fatalf("DatabaseFiles: %v", err)
		}
		var out []string
		for _, f := range fs {
			out = append(out, string(f.Type)+"|"+f.Name+"|"+f.PhysicalName)
		}
		return out
	}
	wantFiles := files()

	opts := DefaultScriptOptions()
	opts.IncludeHeaders = false
	script, err := NewScripter(srv.DatabaseRef(snapName), opts).ScriptDatabase(ctx)
	if err != nil {
		t.Fatalf("ScriptDatabase: %v", err)
	}
	t.Logf("script:\n%s", script)
	if !strings.Contains(script, "AS SNAPSHOT OF ["+name+"]") || strings.Contains(script, "ALTER DATABASE") {
		t.Errorf("not a snapshot script:\n%s", script)
	}
	dropSnap()
	replayScript(t, ctx, db, script)

	snap, err := srv.DatabaseSnapshotByName(ctx, snapName)
	if err != nil {
		t.Fatalf("snapshot after replay: %v", err)
	}
	if snap.SourceDatabase != name {
		t.Errorf("replayed snapshot's source = %q, want %q", snap.SourceDatabase, name)
	}
	if got := files(); !slices.Equal(got, wantFiles) {
		t.Errorf("replayed snapshot files differ:\n got %v\nwant %v", got, wantFiles)
	}
}

// CONTAINMENT = PARTIAL round-trips. It has a database of its own: a
// contained database refuses change tracking (Msg 12839), which the options
// round trip sets. "contained database authentication" is turned on for the
// test where it is off, and back off in a cleanup registered before the
// drop's, so it runs after the database is gone.
func TestLiveScriptDatabaseContainmentRoundTrips(t *testing.T) {
	db, ctx, done := liveDB(t)
	t.Cleanup(done)

	const name, copyName = "gosmo_scriptdb_contained", "gosmo_scriptdb_contained_copy"
	var contained int
	if err := db.QueryRowContext(ctx, `SELECT CAST(value_in_use AS int) FROM sys.configurations
WHERE name = 'contained database authentication'`).Scan(&contained); err != nil {
		t.Fatalf("contained database authentication: %v", err)
	}
	if contained == 0 {
		if _, err := db.ExecContext(ctx, "EXEC sp_configure 'contained database authentication', 1; RECONFIGURE"); err != nil {
			t.Fatalf("enable contained database authentication: %v", err)
		}
		t.Cleanup(func() {
			if _, err := db.ExecContext(context.Background(), "EXEC sp_configure 'contained database authentication', 0; RECONFIGURE"); err != nil {
				t.Errorf("restoring contained database authentication to 0: %v — restore it by hand", err)
			}
		})
	}
	dropAll := func() {
		for _, n := range []string{copyName, name} {
			db.ExecContext(context.Background(), "IF DB_ID('"+n+"') IS NOT NULL DROP DATABASE ["+n+"]")
		}
	}
	dropAll()
	t.Cleanup(dropAll)
	if _, err := db.ExecContext(ctx, "CREATE DATABASE ["+name+"] CONTAINMENT = PARTIAL"); err != nil {
		t.Fatalf("create: %v", err)
	}
	srv, err := NewServer(ctx, db)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	opts := DefaultScriptOptions()
	opts.IncludeHeaders = false
	script, err := NewScripter(srv.DatabaseRef(name), opts).ScriptDatabase(ctx)
	if err != nil {
		t.Fatalf("ScriptDatabase: %v", err)
	}
	if !strings.Contains(script, "CREATE DATABASE ["+name+"] CONTAINMENT = PARTIAL") {
		t.Errorf("script lost CONTAINMENT = PARTIAL:\n%s", script)
	}
	replayScript(t, ctx, db, strings.ReplaceAll(script, name, copyName))
	var got string
	if err := db.QueryRowContext(ctx, "SELECT containment_desc FROM sys.databases WHERE name = @p1", copyName).Scan(&got); err != nil {
		t.Fatalf("containment of the copy: %v", err)
	}
	if got != "PARTIAL" {
		t.Errorf("replayed containment = %q, want PARTIAL", got)
	}
}

// What is set from inside the database round-trips: a TDE key under a
// throwaway server certificate with encryption on, scoped configurations
// away from their default (one FOR SECONDARY), LEDGER = ON from 2022, ADR's
// version store on its own filegroup from 2019 (read from a DMV), and
// READ_ONLY after all of them. Encryption also encrypts tempdb until the
// instance restarts — accepted for the test instances (2026-10-13).
func TestLiveScriptDatabaseInDatabaseSettingsRoundTrip(t *testing.T) {
	db, ctx, done := liveDBTimeout(t, 3*time.Minute)
	t.Cleanup(done)

	const name, copyName, cert = "gosmo_scriptdb_tde", "gosmo_scriptdb_tde_copy", "gosmo_tdecert_scriptdb"
	srv, err := NewServer(ctx, db)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	major := srv.Info().VersionMajor
	t.Cleanup(ensureMasterKey(t, srv, ctx))
	dropCert := func() {
		db.ExecContext(context.Background(), "USE [master]; IF CERT_ID('"+cert+"') IS NOT NULL DROP CERTIFICATE ["+cert+"]")
	}
	dropDBs := func() {
		c := context.Background()
		for _, n := range []string{copyName, name} {
			db.ExecContext(c, "IF DB_ID('"+n+"') IS NOT NULL ALTER DATABASE ["+n+"] SET SINGLE_USER WITH ROLLBACK IMMEDIATE")
			db.ExecContext(c, "IF DB_ID('"+n+"') IS NOT NULL DROP DATABASE ["+n+"]")
		}
	}
	dropDBs()
	dropCert()
	t.Cleanup(dropCert) // after the databases, whose keys it protects
	t.Cleanup(dropDBs)

	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("conn: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	create := "CREATE DATABASE [" + name + "]"
	if major >= 16 {
		create += " WITH LEDGER = ON"
	} else {
		t.Log("pre-2022 instance: LEDGER is not covered")
	}
	scoped := []string{"MAXDOP = 3", "LEGACY_CARDINALITY_ESTIMATION = ON", "PARAMETER_SNIFFING = OFF"}
	if major >= 14 {
		scoped = append(scoped, "IDENTITY_CACHE = OFF")
	}
	if major >= 15 {
		scoped = append(scoped, "ELEVATE_ONLINE = WHEN_SUPPORTED")
	}
	setup := []string{
		"USE [master]; CREATE CERTIFICATE [" + cert + "] WITH SUBJECT = 'gosmo ScriptDatabase TDE test'",
		create,
		"USE [" + name + "]; CREATE DATABASE ENCRYPTION KEY WITH ALGORITHM = AES_128 ENCRYPTION BY SERVER CERTIFICATE [" + cert + "]",
		"USE [" + name + "]; ALTER DATABASE SCOPED CONFIGURATION FOR SECONDARY SET MAXDOP = 1",
	}
	for _, sc := range scoped {
		setup = append(setup, "USE ["+name+"]; ALTER DATABASE SCOPED CONFIGURATION SET "+sc)
	}
	if major >= 15 {
		setup = append(setup,
			"ALTER DATABASE ["+name+"] ADD FILEGROUP [pvs]",
			"DECLARE @p nvarchar(400) = CAST(SERVERPROPERTY('InstanceDefaultDataPath') AS nvarchar(400)); "+
				"EXEC (N'ALTER DATABASE ["+name+"] ADD FILE (NAME = N''"+name+"_pvs'', FILENAME = N''' + @p + N'"+name+"_pvs.ndf'') TO FILEGROUP [pvs]')",
			"ALTER DATABASE ["+name+"] SET ACCELERATED_DATABASE_RECOVERY = ON (PERSISTENT_VERSION_STORE_FILEGROUP = [pvs])")
	} else {
		t.Log("pre-2019 instance: ADR's version store filegroup is not covered")
	}
	setup = append(setup, "USE [master]; ALTER DATABASE ["+name+"] SET ENCRYPTION ON")
	for _, q := range setup {
		if _, err := conn.ExecContext(ctx, q); err != nil {
			t.Fatalf("setup %.100q: %v", q, err)
		}
	}
	// READ_ONLY waits for the encryption scan: it is refused while one runs.
	waitEncrypted := func(n string) {
		for range 120 {
			var state int
			if err := db.QueryRowContext(ctx, "SELECT encryption_state FROM sys.dm_database_encryption_keys WHERE database_id = DB_ID(@p1)", n).Scan(&state); err != nil {
				t.Fatalf("encryption state of %s: %v", n, err)
			}
			if state == 3 {
				return
			}
			time.Sleep(250 * time.Millisecond)
		}
		t.Fatalf("%s never finished encrypting", n)
	}
	waitEncrypted(name)
	if _, err := db.ExecContext(ctx, "ALTER DATABASE ["+name+"] SET READ_ONLY"); err != nil {
		t.Fatalf("READ_ONLY: %v", err)
	}

	opts := DefaultScriptOptions()
	opts.IncludeHeaders = false
	script, err := NewScripter(srv.DatabaseRef(name), opts).ScriptDatabase(ctx)
	if err != nil {
		t.Fatalf("ScriptDatabase: %v", err)
	}
	t.Logf("script:\n%s", script)
	wants := []string{
		"CREATE DATABASE ENCRYPTION KEY WITH ALGORITHM = AES_128 ENCRYPTION BY SERVER CERTIFICATE [" + cert + "];",
		"ALTER DATABASE SCOPED CONFIGURATION FOR SECONDARY SET MAXDOP = 1;",
		"ALTER DATABASE SCOPED CONFIGURATION SET MAXDOP = 3;",
	}
	if major >= 15 {
		wants = append(wants, "SET ACCELERATED_DATABASE_RECOVERY = ON (PERSISTENT_VERSION_STORE_FILEGROUP = [pvs]);")
	}
	for _, w := range wants {
		if !strings.Contains(script, w) {
			t.Errorf("script missing %q", w)
		}
	}
	replayScript(t, ctx, db, strings.ReplaceAll(script, name, copyName))

	settings := func(n string) string {
		var head string
		if err := db.QueryRowContext(ctx, `
SELECT CONCAT(d.is_encrypted, '|', k.key_algorithm, k.key_length, '|', c.name, '|', d.is_read_only, '|',
       `+colSince(major, SQLServer2022, "d.is_ledger_on", "''")+`)
FROM   sys.databases d
LEFT   JOIN sys.dm_database_encryption_keys k ON k.database_id = d.database_id
LEFT   JOIN master.sys.certificates c ON c.thumbprint = k.encryptor_thumbprint
WHERE  d.name = @p1`, n).Scan(&head); err != nil {
			t.Fatalf("settings of %s: %v", n, err)
		}
		// Under USE, never [n].sys… from master: an ad-hoc plan keeps the
		// database id it was compiled against, so once a rerun's drops and
		// creates reassign the ids, the same text reads the other database
		// — source and replay swapped, the reported failure on 17.
		rows, err := db.QueryContext(ctx, `USE [`+n+`];
SELECT CONCAT(name, '=', CAST(value AS nvarchar(256)), '/', CAST(value_for_secondary AS nvarchar(256)))
FROM sys.database_scoped_configurations ORDER BY name`)
		if err != nil {
			t.Fatalf("scoped configurations of %s: %v", n, err)
		}
		defer rows.Close()
		out := []string{head}
		if major >= 15 {
			var pvs string
			if err := db.QueryRowContext(ctx, `USE [`+n+`];
SELECT CONCAT('pvs=', fg.name) FROM sys.dm_tran_persistent_version_store_stats s
JOIN sys.filegroups fg ON fg.data_space_id = s.pvs_filegroup_id WHERE s.database_id = DB_ID()`).Scan(&pvs); err != nil {
				t.Fatalf("version store filegroup of %s: %v", n, err)
			}
			out = append(out, pvs)
		}
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				t.Fatalf("scan: %v", err)
			}
			out = append(out, s)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("scoped configurations of %s: %v", n, err)
		}
		return strings.Join(out, "\n")
	}
	want, got := settings(name), settings(copyName)
	t.Logf("source settings:\n%s", want)
	if got != want {
		t.Errorf("replayed settings differ:\n--- replay ---\n%s\n--- source ---\n%s", got, want)
	}
}

// The layout reads a database's options, collation and change tracking from
// inside it: one AUTO_CLOSE has closed reports NULL for its collation,
// TARGET_RECOVERY_TIME, DELAYED_DURABILITY, ADR and change tracking retention
// at server scope. The options round trip covers the whole script, but its
// file reads open the database first, so this calls the three in-database
// reads directly on a closed database. Read at server scope (Options,
// ChangeTracking), the options and change tracking halves fail here.
func TestLiveScriptDatabaseReadsAClosedDatabaseFromInside(t *testing.T) {
	db, ctx, done := liveDB(t)
	t.Cleanup(done)

	const name = "gosmo_scriptdb_closed"
	srv, err := NewServer(ctx, db)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	major := srv.Info().VersionMajor
	_, drop := liveScratchDB(t, db, ctx, name)
	t.Cleanup(drop)
	set := "ALTER DATABASE [" + name + "] SET "
	setup := []string{set + "TARGET_RECOVERY_TIME = 90 SECONDS", set + "DELAYED_DURABILITY = ALLOWED",
		set + "CHANGE_TRACKING = ON (CHANGE_RETENTION = 5 HOURS, AUTO_CLEANUP = OFF)", set + "AUTO_CLOSE ON"}
	if major >= 15 {
		setup = append(setup, set+"ACCELERATED_DATABASE_RECOVERY = ON")
	}
	for _, q := range setup {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatalf("setup %q: %v", q, err)
		}
	}
	// Each read below opens the database, so each waits for it to close.
	waitClosed := func() {
		t.Helper()
		for range 80 {
			var coll sql.NullString
			if err := db.QueryRowContext(ctx, "SELECT collation_name FROM sys.databases WHERE name = @p1", name).Scan(&coll); err != nil {
				t.Fatalf("collation of %s: %v", name, err)
			}
			if !coll.Valid {
				return
			}
			time.Sleep(250 * time.Millisecond)
		}
		t.Fatalf("%s never auto-closed: nothing here would be covered", name)
	}

	d := srv.DatabaseRef(name)
	waitClosed()
	o, err := d.readOptions(ctx, d.queryRow)
	if err != nil {
		t.Fatalf("options of a closed database: %v", err)
	}
	if o.TargetRecoveryTimeSeconds != 90 || o.DelayedDurability != "ALLOWED" || major >= 15 && !o.AcceleratedDatabaseRecovery {
		t.Errorf("options of a closed database read as unknown: TARGET_RECOVERY_TIME %d, DELAYED_DURABILITY %q, ADR %v",
			o.TargetRecoveryTimeSeconds, o.DelayedDurability, o.AcceleratedDatabaseRecovery)
	}
	waitClosed()
	if c, err := d.inDatabaseCollation(ctx); err != nil || c == "" {
		t.Errorf("collation of a closed database: %q, %v", c, err)
	}
	waitClosed()
	want := ChangeTrackingInfo{Enabled: true, RetentionPeriod: 5, RetentionUnit: ChangeTrackingHours}
	if ct, err := d.readChangeTracking(ctx, d.queryRow); err != nil || *ct != want {
		t.Errorf("change tracking of a closed database: %+v, %v; want %+v", ct, err, want)
	}
}

// A key SET ENCRYPTION OFF left behind is restated, and encryption is not
// turned on: the replay has the same key, algorithm and protector, and is
// not encrypted.
func TestLiveScriptDatabaseKeyLeftByEncryptionOff(t *testing.T) {
	db, ctx, done := liveDBTimeout(t, 3*time.Minute)
	t.Cleanup(done)

	const name, copyName, cert = "gosmo_scriptdb_tdeoff", "gosmo_scriptdb_tdeoff_copy", "gosmo_tdecert_tdeoff"
	srv, err := NewServer(ctx, db)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	t.Cleanup(ensureMasterKey(t, srv, ctx))
	dropCert := func() {
		db.ExecContext(context.Background(), "USE [master]; IF CERT_ID('"+cert+"') IS NOT NULL DROP CERTIFICATE ["+cert+"]")
	}
	dropDBs := func() {
		c := context.Background()
		for _, n := range []string{copyName, name} {
			db.ExecContext(c, "IF DB_ID('"+n+"') IS NOT NULL ALTER DATABASE ["+n+"] SET SINGLE_USER WITH ROLLBACK IMMEDIATE")
			db.ExecContext(c, "IF DB_ID('"+n+"') IS NOT NULL DROP DATABASE ["+n+"]")
		}
	}
	dropDBs()
	dropCert()
	t.Cleanup(dropCert) // after the databases, whose keys it protects
	t.Cleanup(dropDBs)

	waitState := func(n string, want int) {
		t.Helper()
		for range 120 {
			var state int
			if err := db.QueryRowContext(ctx, "SELECT encryption_state FROM sys.dm_database_encryption_keys WHERE database_id = DB_ID(@p1)", n).Scan(&state); err != nil {
				t.Fatalf("encryption state of %s: %v", n, err)
			}
			if state == want {
				return
			}
			time.Sleep(250 * time.Millisecond)
		}
		t.Fatalf("%s never reached encryption state %d", n, want)
	}
	for _, q := range []string{
		"USE [master]; CREATE CERTIFICATE [" + cert + "] WITH SUBJECT = 'gosmo ScriptDatabase TDE off test'",
		"CREATE DATABASE [" + name + "]",
		"USE [" + name + "]; CREATE DATABASE ENCRYPTION KEY WITH ALGORITHM = AES_192 ENCRYPTION BY SERVER CERTIFICATE [" + cert + "]",
		"USE [master]; ALTER DATABASE [" + name + "] SET ENCRYPTION ON",
	} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatalf("setup %.100q: %v", q, err)
		}
	}
	waitState(name, 3)
	if _, err := db.ExecContext(ctx, "ALTER DATABASE ["+name+"] SET ENCRYPTION OFF"); err != nil {
		t.Fatalf("ENCRYPTION OFF: %v", err)
	}
	waitState(name, 1)

	opts := DefaultScriptOptions()
	opts.IncludeHeaders = false
	script, err := NewScripter(srv.DatabaseRef(name), opts).ScriptDatabase(ctx)
	if err != nil {
		t.Fatalf("ScriptDatabase: %v", err)
	}
	t.Logf("script:\n%s", script)
	if !strings.Contains(script, "CREATE DATABASE ENCRYPTION KEY WITH ALGORITHM = AES_192 ENCRYPTION BY SERVER CERTIFICATE ["+cert+"];") ||
		strings.Contains(script, "ENCRYPTION ON") {
		t.Fatalf("script does not restate the key alone")
	}
	replayScript(t, ctx, db, strings.ReplaceAll(script, name, copyName))

	key := func(n string) string {
		var s string
		if err := db.QueryRowContext(ctx, `
SELECT CONCAT(d.is_encrypted, '|', k.encryption_state, '|', k.key_algorithm, k.key_length, '|', c.name)
FROM   sys.databases d
LEFT   JOIN sys.dm_database_encryption_keys k ON k.database_id = d.database_id
LEFT   JOIN master.sys.certificates c ON c.thumbprint = k.encryptor_thumbprint
WHERE  d.name = @p1`, n).Scan(&s); err != nil {
			t.Fatalf("key of %s: %v", n, err)
		}
		return s
	}
	if want, got := key(name), key(copyName); got != want || want != "0|1|AES192|"+cert {
		t.Errorf("replayed key %q, source %q", got, want)
	}
}
