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
		q := `
SELECT CONCAT(mf.type_desc COLLATE DATABASE_DEFAULT, '|', mf.name COLLATE DATABASE_DEFAULT, '|',
              mf.physical_name COLLATE DATABASE_DEFAULT, '|', mf.size, '|',
              mf.max_size, '|', mf.growth, '|', mf.is_percent_growth)
FROM   sys.master_files mf WHERE mf.database_id = DB_ID(@p1)
UNION ALL
SELECT CONCAT('FG|', fg.name COLLATE DATABASE_DEFAULT, '|', fg.type_desc COLLATE DATABASE_DEFAULT, '|', fg.is_default, '|', fg.is_read_only)
FROM   [` + n + `].sys.filegroups fg`
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

// replayScript runs script batch by batch, as a query editor would.
func replayScript(t *testing.T, ctx context.Context, db *sql.DB, script string) {
	t.Helper()
	for _, batch := range strings.Split(script, "\nGO") {
		if strings.TrimSpace(batch) == "" {
			continue
		}
		if _, err := db.ExecContext(ctx, batch); err != nil {
			t.Fatalf("replaying %.200q: %v", strings.TrimSpace(batch), err)
		}
	}
}

// The database options round-trip: a database with every scripted option
// away from its default — AUTO_*, PAGE_VERIFY, TRUSTWORTHY, both snapshot
// options, change tracking, Query Store with non-default settings, a
// throwaway owner, and where the instance allows them CONTAINMENT = PARTIAL
// and WITH FILESTREAM — is scripted, replayed under a new name, and the two
// databases' settings compared.
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
	var contained, fsLevel int
	if err := db.QueryRowContext(ctx, `SELECT CAST(value_in_use AS int) FROM sys.configurations
WHERE name = 'contained database authentication'`).Scan(&contained); err != nil {
		t.Fatalf("contained database authentication: %v", err)
	}
	if err := db.QueryRowContext(ctx, "SELECT CAST(SERVERPROPERTY('FilestreamEffectiveLevel') AS int)").Scan(&fsLevel); err != nil {
		t.Fatalf("FilestreamEffectiveLevel: %v", err)
	}
	create := "CREATE DATABASE [" + name + "]"
	if contained == 1 {
		create += " CONTAINMENT = PARTIAL"
	} else {
		t.Log("contained database authentication is off here: CONTAINMENT is not covered")
	}
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
	} {
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
	replayScript(t, ctx, db, strings.ReplaceAll(script, name, copyName))

	settings := func(n string) string {
		q := `
SELECT CONCAT(SUSER_SNAME(d.owner_sid) COLLATE DATABASE_DEFAULT, '|', d.page_verify_option_desc COLLATE DATABASE_DEFAULT, '|', d.containment_desc COLLATE DATABASE_DEFAULT, '|',
       d.snapshot_isolation_state_desc COLLATE DATABASE_DEFAULT, '|', d.is_read_committed_snapshot_on, '|',
       d.is_auto_close_on, d.is_auto_shrink_on, d.is_auto_create_stats_on,
       d.is_auto_update_stats_on, d.is_auto_update_stats_async_on, '|', d.is_trustworthy_on, '|',
       d.recovery_model_desc COLLATE DATABASE_DEFAULT, '|', d.compatibility_level, '|', d.collation_name COLLATE DATABASE_DEFAULT, '|',
       ctd.retention_period, ctd.retention_period_units_desc COLLATE DATABASE_DEFAULT, ctd.is_auto_cleanup_on, '|',
       fo.non_transacted_access_desc COLLATE DATABASE_DEFAULT, '|', fo.directory_name COLLATE DATABASE_DEFAULT, '|',
       qs.desired_state_desc COLLATE DATABASE_DEFAULT, qs.max_storage_size_mb, '/', qs.flush_interval_seconds, '/',
       qs.interval_length_minutes, '/', qs.max_plans_per_query, '/', qs.query_capture_mode_desc COLLATE DATABASE_DEFAULT, '/',
       qs.size_based_cleanup_mode_desc COLLATE DATABASE_DEFAULT, '/', qs.stale_query_threshold_days)
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
		t.Errorf("replayed settings differ:\n replay %s\n source %s", got, want)
	}
}
