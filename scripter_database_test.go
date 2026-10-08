package gosmo

import (
	"strings"
	"testing"
)

// scripterOverDatabase builds a Scripter over a Database with the metadata
// DatabaseByName would have filled in, so the renderer
// needs no refresh.
func scripterOverDatabase(name string) *Scripter {
	d := &Database{
		server:             &Server{},
		Name:               name,
		RecoveryModel:      RecoveryModelFull,
		CompatibilityLevel: CompatLevel2022,
		Collation:          "SQL_Latin1_General_CP1_CI_AS",
	}
	opts := DefaultScriptOptions()
	opts.IncludeHeaders = false
	return NewScripter(d, opts)
}

// render is ScriptDatabase without its reads: the metadata is already on
// the Database, and the layout is what the test supplies (nil for none).
func render(t *testing.T, sc *Scripter, layout *databaseLayout) string {
	t.Helper()
	got, err := sc.scriptDatabaseFrom(sc.db, layout)
	if err != nil {
		t.Fatalf("scriptDatabaseFrom: %v", err)
	}
	return got
}

func TestScriptDatabaseRendersFromCachedMetadata(t *testing.T) {
	got := render(t, scripterOverDatabase("Sales"), nil)
	for _, want := range []string{
		"CREATE DATABASE [Sales] COLLATE SQL_Latin1_General_CP1_CI_AS;",
		"ALTER DATABASE [Sales] SET RECOVERY FULL;",
		"ALTER DATABASE [Sales] SET COMPATIBILITY_LEVEL = 160;",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("script missing %q:\n%s", want, got)
		}
	}
}

// A database whose recovery model or compatibility level is unknown — what
// sys.databases reports for one that is OFFLINE or otherwise inaccessible,
// where both columns come back NULL — must omit those lines rather than emit
// "SET RECOVERY ;" and "COMPATIBILITY_LEVEL = 0", which are not valid T-SQL.
func TestScriptDatabaseOmitsSettingsItDoesNotKnow(t *testing.T) {
	sc := scripterOverDatabase("Offline")
	sc.db.RecoveryModel = ""
	sc.db.CompatibilityLevel = 0
	// Rendered directly: ScriptDatabase would try to refresh these
	// from the server first, and there is no server here.
	got := render(t, sc, nil)
	if strings.Contains(got, "SET RECOVERY") {
		t.Errorf("script emits a RECOVERY line with no recovery model:\n%s", got)
	}
	if strings.Contains(got, "COMPATIBILITY_LEVEL") {
		t.Errorf("script emits a COMPATIBILITY_LEVEL line with no level:\n%s", got)
	}
	if !strings.Contains(got, "CREATE DATABASE [Offline]") {
		t.Errorf("script lost its CREATE DATABASE:\n%s", got)
	}
}

func TestScriptDatabaseIfNotExistsWrapsTheCreate(t *testing.T) {
	sc := scripterOverDatabase("O'Brien")
	sc.opts.IncludeIfNotExists = true
	got := render(t, sc, nil)
	// The name reaches the script twice, quoted differently each time: a
	// string literal inside DB_ID, an identifier in CREATE DATABASE.
	if !strings.Contains(got, "IF DB_ID(N'O''Brien') IS NULL") {
		t.Errorf("existence check not single-quote escaped:\n%s", got)
	}
	if !strings.Contains(got, "CREATE DATABASE [O'Brien]") {
		t.Errorf("CREATE not bracket-quoted:\n%s", got)
	}
}

// A Server built as &Server{db: db} — anything but NewServer — has no
// ServerInfo, and the header read it unguarded.
func TestScriptDatabaseHeaderSurvivesAServerWithNoInfo(t *testing.T) {
	sc := scripterOverDatabase("Sales")
	sc.opts.IncludeHeaders = true
	got := render(t, sc, nil)
	if !strings.Contains(got, "/* Database: Sales  Version:  */") {
		t.Errorf("header missing or malformed with a nil ServerInfo:\n%s", got)
	}
}

// With a ServerInfo present the header still names the version.
func TestScriptDatabaseHeaderNamesTheVersion(t *testing.T) {
	sc := scripterOverDatabase("Sales")
	sc.db.server.info = &ServerInfo{ProductVersion: "14.0.3480.0"}
	sc.opts.IncludeHeaders = true
	got := render(t, sc, nil)
	if !strings.Contains(got, "Version: 14.0.3480.0") {
		t.Errorf("header lost the product version:\n%s", got)
	}
}

// Verb used to be ignored here: a DROP came back as a CREATE, which run
// as a "drop this" script fails on an existing database at best.
func TestScriptDatabaseHonoursVerb(t *testing.T) {
	cases := []struct {
		verb     ScriptVerb
		guard    bool
		want     []string
		wantNone []string
	}{
		{ScriptDrop, false,
			[]string{"USE [master];\nGO\nDROP DATABASE [O'Brien];\nGO\n"},
			[]string{"CREATE DATABASE", "SET RECOVERY"}},
		{ScriptDrop, true,
			[]string{"IF DB_ID(N'O''Brien') IS NOT NULL\n    DROP DATABASE [O'Brien];"},
			[]string{"CREATE DATABASE"}},
		{ScriptDropAndCreate, true,
			[]string{"DROP DATABASE [O'Brien];", "IF DB_ID(N'O''Brien') IS NULL\nBEGIN\n    CREATE DATABASE [O'Brien]", "SET RECOVERY FULL"},
			nil},
		{ScriptAlter, false,
			[]string{"CREATE DATABASE [O'Brien]"},
			[]string{"DROP DATABASE"}},
	}
	for _, tc := range cases {
		sc := scripterOverDatabase("O'Brien")
		sc.opts.Verb = tc.verb
		sc.opts.IncludeIfNotExists = tc.guard
		got := render(t, sc, nil)
		for _, w := range tc.want {
			if !strings.Contains(got, w) {
				t.Errorf("verb %d guard %v: missing %q in:\n%s", tc.verb, tc.guard, w, got)
			}
		}
		for _, w := range tc.wantNone {
			if strings.Contains(got, w) {
				t.Errorf("verb %d guard %v: unexpected %q in:\n%s", tc.verb, tc.guard, w, got)
			}
		}
		if tc.verb == ScriptDropAndCreate && strings.Index(got, "DROP DATABASE") > strings.Index(got, "CREATE DATABASE") {
			t.Errorf("DROP AND CREATE puts the CREATE first:\n%s", got)
		}
	}
}

// A DROP needs only the name, so a bare DatabaseRef handle scripts one with
// no server to refresh from.
func TestScriptDatabaseDropNeedsNoRefresh(t *testing.T) {
	opts := DefaultScriptOptions()
	opts.Verb = ScriptDrop
	opts.IncludeHeaders = false
	got, err := NewScripter((&Server{}).DatabaseRef("Sales"), opts).ScriptDatabase(t.Context())
	if err != nil {
		t.Fatalf("ScriptDatabase: %v", err)
	}
	if !strings.Contains(got, "DROP DATABASE [Sales];") {
		t.Errorf("no DROP:\n%s", got)
	}
}

func layoutFile(id int, name, path, typ string, sizeKB, maxKB, growthKB int64, pct int) *DatabaseFileInfo {
	return &DatabaseFileInfo{FileID: id, Name: name, PhysicalName: path, Type: typ,
		SizeKB: sizeKB, MaxSizeKB: maxKB, GrowthKB: growthKB, GrowthPercent: pct, IsPercentGrowth: pct > 0}
}

// Every shape the file clauses take, in one database: two PRIMARY files, a
// second ROWS filegroup that is the default and read-only, FILESTREAM and
// memory-optimized containers, an empty filegroup, and two logs.
func TestScriptDatabaseFileLayout(t *testing.T) {
	layout := &databaseLayout{
		groups: []*FileGroup{
			{Name: "Archive", Type: "ROWS_FILEGROUP", IsDefault: true, IsReadOnly: true, Files: []*DatabaseFileInfo{
				layoutFile(3, "arch", `D:\data\arch.ndf`, "ROWS", 16384, 1048576, 0, 10)}},
			{Name: "Docs", Type: "FILESTREAM_DATA_FILEGROUP", IsDefault: true, Files: []*DatabaseFileInfo{
				layoutFile(5, "docs", `D:\data\docs`, "FILESTREAM", 0, -1, 0, 0)}},
			{Name: "Empty", Type: "ROWS_FILEGROUP"},
			{Name: "Mem", Type: "MEMORY_OPTIMIZED_DATA_FILEGROUP", Files: []*DatabaseFileInfo{
				layoutFile(6, "mem", `D:\data\mem`, "FILESTREAM", 0, 524288, 0, 0)}},
			{Name: "PRIMARY", Type: "ROWS_FILEGROUP", Files: []*DatabaseFileInfo{
				layoutFile(1, "Sales", `D:\data\Sales.mdf`, "ROWS", 8192, -1, 65536, 0),
				layoutFile(4, "Sales2", `D:\data\O'Neil.ndf`, "ROWS", 8192, 102400, 0, 0)}},
		},
		logs: []*DatabaseFileInfo{
			layoutFile(2, "Sales_log", `E:\log\Sales.ldf`, "LOG", 8192, 2147483648, 65536, 0),
			layoutFile(7, "Sales_log2", `E:\log\Sales2.ldf`, "LOG", 1024, -1, 0, 10)},
	}
	got := render(t, scripterOverDatabase("Sales"), layout)
	want := "CREATE DATABASE [Sales]\n ON PRIMARY\n" +
		"( NAME = [Sales], FILENAME = N'D:\\data\\Sales.mdf', SIZE = 8192KB, MAXSIZE = UNLIMITED, FILEGROWTH = 65536KB ),\n" +
		"( NAME = [Sales2], FILENAME = N'D:\\data\\O''Neil.ndf', SIZE = 8192KB, MAXSIZE = 102400KB, FILEGROWTH = 0 ),\n" +
		" FILEGROUP [Archive] DEFAULT\n" +
		"( NAME = [arch], FILENAME = N'D:\\data\\arch.ndf', SIZE = 16384KB, MAXSIZE = 1048576KB, FILEGROWTH = 10% ),\n" +
		" FILEGROUP [Docs] CONTAINS FILESTREAM DEFAULT\n" +
		"( NAME = [docs], FILENAME = N'D:\\data\\docs' ),\n" +
		" FILEGROUP [Mem] CONTAINS MEMORY_OPTIMIZED_DATA\n" +
		"( NAME = [mem], FILENAME = N'D:\\data\\mem', MAXSIZE = 524288KB )\n" +
		" LOG ON\n" +
		"( NAME = [Sales_log], FILENAME = N'E:\\log\\Sales.ldf', SIZE = 8192KB, MAXSIZE = 2097152MB, FILEGROWTH = 65536KB ),\n" +
		"( NAME = [Sales_log2], FILENAME = N'E:\\log\\Sales2.ldf', SIZE = 1024KB, MAXSIZE = UNLIMITED, FILEGROWTH = 10% )" +
		" COLLATE SQL_Latin1_General_CP1_CI_AS;\n"
	if !strings.Contains(got, want) {
		t.Errorf("file clauses wrong.\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
	for _, w := range []string{
		"ALTER DATABASE [Sales] ADD FILEGROUP [Empty];",
		"ALTER DATABASE [Sales] MODIFY FILEGROUP [Archive] READ_ONLY;",
	} {
		if !strings.Contains(got, w) {
			t.Errorf("script missing %q:\n%s", w, got)
		}
	}
	// Both follow the CREATE they modify.
	if strings.Index(got, "READ_ONLY") < strings.Index(got, "CREATE DATABASE") {
		t.Errorf("READ_ONLY precedes the CREATE:\n%s", got)
	}
}

// A layout with no primary file (nothing read, or a database mid-restore)
// leaves the file clauses out rather than writing a LOG ON with no ON.
func TestScriptDatabaseNoPrimaryFileWritesNoFileClauses(t *testing.T) {
	layout := &databaseLayout{logs: []*DatabaseFileInfo{layoutFile(2, "l", `E:\l.ldf`, "LOG", 8192, -1, 65536, 0)}}
	got := render(t, scripterOverDatabase("Sales"), layout)
	if strings.Contains(got, "LOG ON") || strings.Contains(got, " ON PRIMARY") {
		t.Errorf("file clauses without a primary file:\n%s", got)
	}
}

// shippedDefaults is what DatabaseOptions reads for a database CREATE
// DATABASE has just made on an instance whose model is as shipped.
func shippedDefaults() *DatabaseOptions {
	return &DatabaseOptions{
		Owner: "sa", PageVerify: "CHECKSUM", Containment: "NONE", SnapshotIsolation: "OFF",
		AutoCreateStats: true, AutoUpdateStats: true, NonTransactedAccess: "OFF",
	}
}

// A database at every default scripts no SET beyond the three always
// written: Query Store (its default depends on the replaying instance) and
// the owner (else the replaying login).
func TestScriptDatabaseDefaultSettingsStayShort(t *testing.T) {
	layout := &databaseLayout{opts: shippedDefaults(), ct: &ChangeTrackingInfo{}, qs: &QueryStoreInfo{DesiredState: QueryStoreOff}}
	got := render(t, scripterOverDatabase("Sales"), layout)
	sets := strings.Count(got, " SET ")
	// RECOVERY, COMPATIBILITY_LEVEL, QUERY_STORE.
	if sets != 3 {
		t.Errorf("%d SET statements for a database at its defaults, want 3:\n%s", sets, got)
	}
	for _, w := range []string{
		"ALTER DATABASE [Sales] SET QUERY_STORE = OFF;",
		"ALTER AUTHORIZATION ON DATABASE::[Sales] TO [sa];",
	} {
		if !strings.Contains(got, w) {
			t.Errorf("script missing %q:\n%s", w, got)
		}
	}
	for _, bad := range []string{"CONTAINMENT", "WITH FILESTREAM"} {
		if strings.Contains(got, bad) {
			t.Errorf("default database scripts %q:\n%s", bad, got)
		}
	}
}

// Every option away from its default, in the form a replay needs.
func TestScriptDatabaseSettingsAwayFromDefault(t *testing.T) {
	o := &DatabaseOptions{
		Owner: `CORP\o'neil`, PageVerify: "TORN_PAGE_DETECTION", Containment: "PARTIAL",
		SnapshotIsolation: "IN_TRANSITION_TO_ON", AutoClose: true, AutoShrink: true,
		AutoUpdateStatsAsync: true, IsTrustworthy: true, ReadCommittedSnapshot: true,
		NonTransactedAccess: "FULL", DirectoryName: "Sales'docs",
	}
	layout := &databaseLayout{
		opts: o,
		ct:   &ChangeTrackingInfo{Enabled: true, AutoCleanup: true, RetentionPeriod: 3, RetentionUnit: ChangeTrackingHours},
		qs: &QueryStoreInfo{DesiredState: QueryStoreReadWrite, MaxStorageMB: 500, FlushIntervalSec: 900,
			IntervalMinutes: 15, MaxPlansPerQuery: 200, CaptureMode: QueryStoreCaptureAuto,
			SizeCleanupMode: QueryStoreCleanupAuto, StaleThresholdDays: 30, WaitStatsCaptureMode: QueryStoreWaitStatsOn},
	}
	got := render(t, scripterOverDatabase("Sales"), layout)
	for _, w := range []string{
		"CREATE DATABASE [Sales] CONTAINMENT = PARTIAL COLLATE SQL_Latin1_General_CP1_CI_AS WITH FILESTREAM (NON_TRANSACTED_ACCESS = FULL, DIRECTORY_NAME = N'Sales''docs');",
		"SET AUTO_CLOSE ON;", "SET AUTO_SHRINK ON;", "SET AUTO_CREATE_STATISTICS OFF;",
		"SET AUTO_UPDATE_STATISTICS OFF;", "SET AUTO_UPDATE_STATISTICS_ASYNC ON;",
		"SET PAGE_VERIFY TORN_PAGE_DETECTION;", "SET TRUSTWORTHY ON;",
		"SET READ_COMMITTED_SNAPSHOT ON;", "SET ALLOW_SNAPSHOT_ISOLATION ON;",
		"SET CHANGE_TRACKING = ON (CHANGE_RETENTION = 3 HOURS, AUTO_CLEANUP = ON);",
		"SET QUERY_STORE = ON (OPERATION_MODE = READ_WRITE, MAX_STORAGE_SIZE_MB = 500, DATA_FLUSH_INTERVAL_SECONDS = 900, INTERVAL_LENGTH_MINUTES = 15, MAX_PLANS_PER_QUERY = 200, SIZE_BASED_CLEANUP_MODE = AUTO, QUERY_CAPTURE_MODE = AUTO, CLEANUP_POLICY = (STALE_QUERY_THRESHOLD_DAYS = 30), WAIT_STATS_CAPTURE_MODE = ON);",
		`ALTER AUTHORIZATION ON DATABASE::[Sales] TO [CORP\o'neil];`,
	} {
		if !strings.Contains(got, w) {
			t.Errorf("script missing %q:\n%s", w, got)
		}
	}
}

// SQL Server 2016 reports no wait-stats mode; the clause is left out, not
// written as "WAIT_STATS_CAPTURE_MODE = ". A value with no SET form leaves
// Query Store out with a note, and an unresolvable owner is noted too.
func TestScriptDatabaseSettingsEdgeCases(t *testing.T) {
	o := shippedDefaults()
	o.Owner = ""
	qs := &QueryStoreInfo{DesiredState: QueryStoreReadOnly, MaxStorageMB: 100, FlushIntervalSec: 900,
		IntervalMinutes: 60, MaxPlansPerQuery: 200, CaptureMode: QueryStoreCaptureAll,
		SizeCleanupMode: QueryStoreCleanupOff, StaleThresholdDays: 367}
	got := render(t, scripterOverDatabase("Sales"), &databaseLayout{opts: o, qs: qs})
	if !strings.Contains(got, "OPERATION_MODE = READ_ONLY") || strings.Contains(got, "WAIT_STATS") {
		t.Errorf("2016 Query Store scripted wrongly:\n%s", got)
	}
	if strings.Contains(got, "ALTER AUTHORIZATION ON") || !strings.Contains(got, "-- The owner's SID maps to no login") {
		t.Errorf("orphaned owner not noted:\n%s", got)
	}
	qs.CaptureMode = "SOMETHING_NEW"
	got = render(t, scripterOverDatabase("Sales"), &databaseLayout{opts: o, qs: qs})
	if strings.Contains(got, "QUERY_STORE = ON") || !strings.Contains(got, "QUERY_STORE not scripted") {
		t.Errorf("unknown capture mode spliced in or not noted:\n%s", got)
	}
}
