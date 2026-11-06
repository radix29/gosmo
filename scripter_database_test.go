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

func layoutFile(id int, name, path string, typ FileType, sizeKB, maxKB, growthKB int64, pct int) *DatabaseFileInfo {
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
		Owner: "sa", PageVerify: "CHECKSUM", UserAccess: UserAccessMulti, Containment: "NONE",
		DefaultCursor: "GLOBAL", SnapshotIsolation: "OFF",
		AutoCreateStats: true, AutoUpdateStats: true, IsBrokerEnabled: true, NonTransactedAccess: "OFF",
		DelayedDurability: "DISABLED", TargetRecoveryTimeSeconds: 60,
	}
}

// A database at every default still scripts every option, in SSMS's order:
// left out, each would take the replaying instance's model value instead.
// Query Store, the owner, then user access and READ_WRITE close it.
func TestScriptDatabaseScriptsEveryOptionExplicitly(t *testing.T) {
	layout := &databaseLayout{opts: shippedDefaults(), ct: &ChangeTrackingInfo{},
		qs: &QueryStoreInfo{DesiredState: QueryStoreOff}, major: 16}
	got := render(t, scripterOverDatabase("Sales"), layout)
	var want strings.Builder
	for _, c := range []string{
		"ANSI_NULL_DEFAULT OFF", "ANSI_NULLS OFF", "ANSI_PADDING OFF", "ANSI_WARNINGS OFF",
		"ARITHABORT OFF", "AUTO_CLOSE OFF", "AUTO_SHRINK OFF", "AUTO_CREATE_STATISTICS ON",
		"AUTO_UPDATE_STATISTICS ON", "AUTO_UPDATE_STATISTICS_ASYNC OFF", "CURSOR_CLOSE_ON_COMMIT OFF",
		"CURSOR_DEFAULT GLOBAL", "CONCAT_NULL_YIELDS_NULL OFF", "NUMERIC_ROUNDABORT OFF",
		"QUOTED_IDENTIFIER OFF", "RECURSIVE_TRIGGERS OFF", "ENABLE_BROKER",
		"DATE_CORRELATION_OPTIMIZATION OFF", "TRUSTWORTHY OFF", "ALLOW_SNAPSHOT_ISOLATION OFF",
		"PARAMETERIZATION SIMPLE", "READ_COMMITTED_SNAPSHOT OFF", "HONOR_BROKER_PRIORITY OFF",
		"PAGE_VERIFY CHECKSUM", "DB_CHAINING OFF", "TARGET_RECOVERY_TIME = 60 SECONDS",
		"DELAYED_DURABILITY = DISABLED", "ACCELERATED_DATABASE_RECOVERY = OFF", "QUERY_STORE = OFF",
	} {
		want.WriteString("ALTER DATABASE [Sales] SET " + c + ";\nGO\n")
	}
	want.WriteString("ALTER AUTHORIZATION ON DATABASE::[Sales] TO [sa];\nGO\n")
	want.WriteString("ALTER DATABASE [Sales] SET MULTI_USER;\nGO\n")
	want.WriteString("ALTER DATABASE [Sales] SET READ_WRITE;\nGO\n")
	if !strings.HasSuffix(got, want.String()) {
		t.Errorf("settings wrong.\n--- got ---\n%s\n--- want suffix ---\n%s", got, want.String())
	}
	for _, bad := range []string{"CONTAINMENT", "WITH FILESTREAM", "CHANGE_TRACKING"} {
		if strings.Contains(got, bad) {
			t.Errorf("default database scripts %q:\n%s", bad, got)
		}
	}
}

// ACCELERATED_DATABASE_RECOVERY is SQL Server 2019's: a 2017 source has no
// value to restate, and the line would fail on a 2017 replay. An unknown
// major (0) is treated as newest, as every version gate here does.
func TestScriptDatabaseGatesAcceleratedDatabaseRecovery(t *testing.T) {
	for _, tc := range []struct {
		major int
		want  bool
	}{{14, false}, {15, true}, {0, true}} {
		o := shippedDefaults()
		o.AcceleratedDatabaseRecovery = true
		got := render(t, scripterOverDatabase("Sales"), &databaseLayout{opts: o, major: tc.major})
		if has := strings.Contains(got, "SET ACCELERATED_DATABASE_RECOVERY = ON;"); has != tc.want {
			t.Errorf("major %d: ACCELERATED_DATABASE_RECOVERY scripted = %v, want %v:\n%s", tc.major, has, tc.want, got)
		}
	}
}

// ADR's version store off PRIMARY is restated with the ON, which is the
// only form that sets it; on PRIMARY, with ADR off, or unread the line is
// the bare one. A refused read says so above the bare ON.
func TestScriptDatabaseVersionStoreFileGroup(t *testing.T) {
	const refused = "-- Reading ADR's version store filegroup needs VIEW DATABASE PERFORMANCE STATE (VIEW SERVER STATE before 2022): PERSISTENT_VERSION_STORE_FILEGROUP not scripted.\n"
	for _, tc := range []struct {
		adr     bool
		fg      string
		refused bool
		want    string
	}{
		{true, "PVS]fg", false, "ALTER DATABASE [Sales] SET ACCELERATED_DATABASE_RECOVERY = ON (PERSISTENT_VERSION_STORE_FILEGROUP = [PVS]]fg]);\nGO\n"},
		{true, "PRIMARY", false, "ALTER DATABASE [Sales] SET ACCELERATED_DATABASE_RECOVERY = ON;\nGO\n"},
		{true, "", false, "ALTER DATABASE [Sales] SET ACCELERATED_DATABASE_RECOVERY = ON;\nGO\n"},
		{false, "PVS", false, "ALTER DATABASE [Sales] SET ACCELERATED_DATABASE_RECOVERY = OFF;\nGO\n"},
		{true, "", true, refused + "ALTER DATABASE [Sales] SET ACCELERATED_DATABASE_RECOVERY = ON;\nGO\n"},
		{false, "", true, "ALTER DATABASE [Sales] SET ACCELERATED_DATABASE_RECOVERY = OFF;\nGO\n"},
	} {
		o := shippedDefaults()
		o.AcceleratedDatabaseRecovery = tc.adr
		got := render(t, scripterOverDatabase("Sales"), &databaseLayout{opts: o, major: 17, pvsFileGroup: tc.fg, pvsRefused: tc.refused})
		if !strings.Contains(got, tc.want) || strings.Count(got, "ACCELERATED_DATABASE_RECOVERY") != 1 {
			t.Errorf("%+v: want %q once:\n%s", tc, tc.want, got)
		}
		if !tc.refused && strings.Contains(got, refused) || !tc.adr && strings.Contains(got, "--") {
			t.Errorf("%+v: refusal noted wrongly:\n%s", tc, got)
		}
	}
}

// A read-only, restricted database restates both last — after the owner and
// Query Store, which READ_ONLY would refuse.
func TestScriptDatabaseReadOnlyAndUserAccessComeLast(t *testing.T) {
	sc := scripterOverDatabase("Sales")
	sc.db.IsReadOnly = true
	o := shippedDefaults()
	o.UserAccess = UserAccessRestricted
	got := render(t, sc, &databaseLayout{opts: o, qs: &QueryStoreInfo{DesiredState: QueryStoreOff}})
	want := "ALTER AUTHORIZATION ON DATABASE::[Sales] TO [sa];\nGO\n" +
		"ALTER DATABASE [Sales] SET RESTRICTED_USER;\nGO\n" +
		"ALTER DATABASE [Sales] SET READ_ONLY;\nGO\n"
	if !strings.HasSuffix(got, want) {
		t.Errorf("tail wrong.\n--- got ---\n%s\n--- want suffix ---\n%s", got, want)
	}
	// A user-access value the catalog never reports is not spliced in.
	o.UserAccess = "MULTI_USER; DROP DATABASE x"
	if got := render(t, sc, &databaseLayout{opts: o}); strings.Contains(got, "DROP DATABASE x") {
		t.Errorf("unknown user access spliced in:\n%s", got)
	}
}

// A snapshot is a CREATE … AS SNAPSHOT OF over its sparse files and nothing
// else — not a plain database, which is what it used to script as.
func TestScriptDatabaseSnapshot(t *testing.T) {
	sc := scripterOverDatabase("Sales_snap")
	sc.db.SourceDatabaseID = 5
	sc.opts.IncludeIfNotExists = false
	layout := &databaseLayout{snapshotOf: "Sal]es", snapshotFiles: []*DatabaseFileInfo{
		layoutFile(1, "Sales", `D:\data\Sales_snap.ss`, "ROWS", 0, 0, 0, 0),
		layoutFile(3, "arch", `D:\data\O'Neil_snap.ss`, "ROWS", 0, 0, 0, 0)}}
	got := render(t, sc, layout)
	want := "CREATE DATABASE [Sales_snap] ON\n" +
		"( NAME = [Sales], FILENAME = N'D:\\data\\Sales_snap.ss' ),\n" +
		"( NAME = [arch], FILENAME = N'D:\\data\\O''Neil_snap.ss' )\n" +
		"AS SNAPSHOT OF [Sal]]es];\nGO\n"
	if got != want {
		t.Errorf("snapshot script wrong.\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
	sc.opts.IncludeIfNotExists = true
	got = render(t, sc, layout)
	if !strings.HasPrefix(got, "IF DB_ID(N'Sales_snap') IS NULL\nBEGIN\n    CREATE DATABASE [Sales_snap] ON\n") ||
		!strings.HasSuffix(got, "AS SNAPSHOT OF [Sal]]es];\nEND\nGO\n") {
		t.Errorf("guarded snapshot script wrong:\n%s", got)
	}
}

// Every option away from its default, in the form a replay needs.
func TestScriptDatabaseSettingsAwayFromDefault(t *testing.T) {
	o := &DatabaseOptions{
		Owner: `CORP\o'neil`, PageVerify: "TORN_PAGE_DETECTION", Containment: "PARTIAL",
		SnapshotIsolation: "IN_TRANSITION_TO_ON", AutoClose: true, AutoShrink: true,
		AutoUpdateStatsAsync: true, IsTrustworthy: true, ReadCommittedSnapshot: true,
		NonTransactedAccess: "FULL", DirectoryName: "Sales'docs",
		ANSINullDefault: true, ANSINulls: true, ANSIPadding: true, ANSIWarnings: true, ArithAbort: true,
		ConcatNullYieldsNull: true, NumericRoundAbort: true, QuotedIdentifier: true,
		RecursiveTriggers: true, CursorCloseOnCommit: true, DefaultCursor: "LOCAL",
		DBChaining: true, ForcedParameterization: true, DateCorrelation: true, HonorBrokerPriority: true,
		DelayedDurability: "FORCED", TargetRecoveryTimeSeconds: 0, AcceleratedDatabaseRecovery: true,
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
		"SET ANSI_NULL_DEFAULT ON;", "SET ANSI_NULLS ON;", "SET ANSI_PADDING ON;", "SET ANSI_WARNINGS ON;",
		"SET ARITHABORT ON;", "SET CONCAT_NULL_YIELDS_NULL ON;", "SET NUMERIC_ROUNDABORT ON;",
		"SET QUOTED_IDENTIFIER ON;", "SET RECURSIVE_TRIGGERS ON;", "SET CURSOR_CLOSE_ON_COMMIT ON;",
		"SET CURSOR_DEFAULT LOCAL;", "SET DISABLE_BROKER;", "SET DB_CHAINING ON;",
		"SET PARAMETERIZATION FORCED;", "SET DATE_CORRELATION_OPTIMIZATION ON;",
		"SET HONOR_BROKER_PRIORITY ON;", "SET DELAYED_DURABILITY = FORCED;",
		"SET TARGET_RECOVERY_TIME = 0 SECONDS;", "SET ACCELERATED_DATABASE_RECOVERY = ON;",
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

// LEDGER = ON exists only on the CREATE, and shares its WITH list with
// FILESTREAM's options.
func TestScriptDatabaseLedger(t *testing.T) {
	o := shippedDefaults()
	o.IsLedger = true
	got := render(t, scripterOverDatabase("Sales"), &databaseLayout{opts: o})
	if !strings.Contains(got, "CREATE DATABASE [Sales] COLLATE SQL_Latin1_General_CP1_CI_AS WITH LEDGER = ON;\n") {
		t.Errorf("ledger CREATE wrong:\n%s", got)
	}
	o.NonTransactedAccess, o.DirectoryName = "READ_ONLY", "d"
	got = render(t, scripterOverDatabase("Sales"), &databaseLayout{opts: o})
	if !strings.Contains(got, " WITH FILESTREAM (NON_TRANSACTED_ACCESS = READ_ONLY, DIRECTORY_NAME = N'd'), LEDGER = ON;\n") {
		t.Errorf("FILESTREAM and LEDGER not one WITH list:\n%s", got)
	}
}

// A TDE database restates its key from inside the database, then turns
// encryption on — both before the owner and READ_ONLY, which would refuse
// the key.
func TestScriptDatabaseEncryption(t *testing.T) {
	sc := scripterOverDatabase("Sales")
	sc.db.IsReadOnly = true
	got := render(t, sc, &databaseLayout{opts: shippedDefaults(),
		tde: &DatabaseEncryptionKey{KeyAlgorithm: "AES", KeyLength: 256, EncryptionState: 3, ServerCertificate: "TDE]Cert"}})
	want := "USE [Sales];\nGO\n" +
		"CREATE DATABASE ENCRYPTION KEY WITH ALGORITHM = AES_256 ENCRYPTION BY SERVER CERTIFICATE [TDE]]Cert];\nGO\n" +
		"USE [master];\nGO\n" +
		"ALTER DATABASE [Sales] SET ENCRYPTION ON;\nGO\n" +
		"ALTER AUTHORIZATION ON DATABASE::[Sales] TO [sa];\nGO\n" +
		"ALTER DATABASE [Sales] SET MULTI_USER;\nGO\n" +
		"ALTER DATABASE [Sales] SET READ_ONLY;\nGO\n"
	if !strings.HasSuffix(got, want) {
		t.Errorf("TDE wrong.\n--- got ---\n%s\n--- want suffix ---\n%s", got, want)
	}
	for _, tc := range []struct {
		key  DatabaseEncryptionKey
		want string
	}{
		{DatabaseEncryptionKey{KeyAlgorithm: "AES", KeyLength: 128, ServerAsymmetricKey: "ekm"},
			"WITH ALGORITHM = AES_128 ENCRYPTION BY SERVER ASYMMETRIC KEY [ekm];"},
		{DatabaseEncryptionKey{KeyAlgorithm: "TRIPLE_DES_3KEY", KeyLength: 192, ServerCertificate: "c"},
			"WITH ALGORITHM = TRIPLE_DES_3KEY ENCRYPTION BY SERVER CERTIFICATE [c];"},
		{DatabaseEncryptionKey{KeyAlgorithm: "AES", KeyLength: 256},
			"-- The database encryption key's protector is not visible in master: encryption not scripted."},
		{DatabaseEncryptionKey{KeyAlgorithm: "AES", KeyLength: 512, ServerCertificate: "c"},
			"-- The database encryption key reports an algorithm with no CREATE form: encryption not scripted."},
	} {
		got := render(t, scripterOverDatabase("Sales"), &databaseLayout{opts: shippedDefaults(), tde: &tc.key})
		if !strings.Contains(got, tc.want) {
			t.Errorf("%+v: missing %q:\n%s", tc.key, tc.want, got)
		}
		if strings.HasPrefix(tc.want, "--") == strings.Contains(got, "SET ENCRYPTION ON") {
			t.Errorf("%+v: SET ENCRYPTION ON written iff a key is:\n%s", tc.key, got)
		}
	}
	// A refused key read is noted whether encryption is on or not: for an
	// unencrypted database a key SET ENCRYPTION OFF left behind could hide.
	encrypted := shippedDefaults()
	encrypted.IsEncrypted = true
	for _, tc := range []struct {
		opts      *DatabaseOptions
		want, not string
	}{
		{encrypted, "-- Encryption is on, and reading its key needs VIEW SERVER STATE: encryption not scripted.\n", "leftover"},
		{shippedDefaults(), "USE [Sales];\nGO\n-- Reading a leftover database encryption key needs VIEW SERVER STATE: not checked.\nUSE [master];\nGO\n", "Encryption is on"},
	} {
		got = render(t, scripterOverDatabase("Sales"), &databaseLayout{opts: tc.opts, tdeRefused: true})
		if !strings.Contains(got, tc.want) || strings.Contains(got, tc.not) || strings.Contains(got, "ENCRYPTION ON") ||
			strings.Contains(got, "CREATE DATABASE ENCRYPTION KEY") {
			t.Errorf("refused key read (IsEncrypted=%v) not noted as %q:\n%s", tc.opts.IsEncrypted, tc.want, got)
		}
	}
	// A key SET ENCRYPTION OFF left behind is created, and encryption is
	// left off.
	got = render(t, scripterOverDatabase("Sales"), &databaseLayout{opts: shippedDefaults(), tdeOff: true,
		tde: &DatabaseEncryptionKey{KeyAlgorithm: "AES", KeyLength: 192, EncryptionState: 1, ServerCertificate: "c"}})
	if !strings.Contains(got, "USE [Sales];\nGO\nCREATE DATABASE ENCRYPTION KEY WITH ALGORITHM = AES_192 ENCRYPTION BY SERVER CERTIFICATE [c];\nGO\nUSE [master];\nGO\n") ||
		strings.Contains(got, "ENCRYPTION ON") {
		t.Errorf("key left by SET ENCRYPTION OFF wrong:\n%s", got)
	}
	if got := render(t, scripterOverDatabase("Sales"), &databaseLayout{opts: shippedDefaults()}); strings.Contains(got, "USE [Sales]") {
		t.Errorf("unencrypted database with default scoped configurations enters the database:\n%s", got)
	}
}

// The database's own extended properties are added from inside it, before
// READ_ONLY would refuse them; with nothing else to set there, they alone
// enter the database.
func TestScriptDatabaseExtendedProperties(t *testing.T) {
	sc := scripterOverDatabase("Sales")
	sc.db.IsReadOnly = true
	prop := "EXEC sys.sp_addextendedproperty @name = N'MS_Description', @value = N'db';\nGO\n"
	got := render(t, sc, &databaseLayout{opts: shippedDefaults(), extProps: prop})
	want := "USE [Sales];\nGO\n" + prop + "USE [master];\nGO\n" +
		"ALTER AUTHORIZATION ON DATABASE::[Sales] TO [sa];\nGO\n" +
		"ALTER DATABASE [Sales] SET MULTI_USER;\nGO\n" +
		"ALTER DATABASE [Sales] SET READ_ONLY;\nGO\n"
	if !strings.HasSuffix(got, want) {
		t.Errorf("database properties wrong.\n--- got ---\n%s\n--- want suffix ---\n%s", got, want)
	}
}

// scopedConfigValue spells a value by its base type; a string that is not a
// bare keyword becomes an N'…' literal with its quotes doubled, and the
// statement built from it carries the literal unchanged.
func TestScopedConfigValue(t *testing.T) {
	for _, tc := range []struct {
		raw, typ, want string
		literal, ok    bool
	}{
		{"https://acct.blob.core.windows.net/ledger", "nvarchar", "N'https://acct.blob.core.windows.net/ledger'", true, true},
		{"it's", "varchar", "N'it''s'", true, true},
		{"a b", "nvarchar", "N'a b'", true, true},
		{"9LIVES", "nvarchar", "N'9LIVES'", true, true},
		{"WHEN_SUPPORTED", "nvarchar", "WHEN_SUPPORTED", false, true},
		{"", "nvarchar", "", false, false},
		{"8", "int", "8", false, true},
		{"x", "int", "", false, false},
		{"1", "bit", "ON", false, true},
		{"0", "bit", "OFF", false, true},
		{"1", "", "", false, false},
	} {
		v, literal, ok := scopedConfigValue(tc.raw, tc.typ)
		if v != tc.want || literal != tc.literal || ok != tc.ok {
			t.Errorf("scopedConfigValue(%q, %q) = %q, %v, %v; want %q, %v, %v",
				tc.raw, tc.typ, v, literal, ok, tc.want, tc.literal, tc.ok)
		}
	}
	got, err := scopedConfigStatement("LEDGER_DIGEST_STORAGE_ENDPOINT", "N'it''s'", ScopedConfigPrimary)
	if want := "ALTER DATABASE SCOPED CONFIGURATION SET LEDGER_DIGEST_STORAGE_ENDPOINT = N'it''s'"; err != nil || got != want {
		t.Errorf("scopedConfigStatement = %q, %v; want %q", got, err, want)
	}
	if _, err := scopedConfigStatement("X; DROP TABLE t --", "N'a'", ScopedConfigPrimary); err == nil {
		t.Error("scopedConfigStatement accepted an injected name")
	}
}

// Scoped configurations: only what is away from its default, by
// is_value_default from 2017 and by the documented defaults on 2016, and a
// secondary value whenever one is set. The value's sql_variant base type
// decides its spelling: a bit is OFF/ON, an integer a number — so a numeric
// option no list names (NEW_NUMERIC, reporting 1) is written as 1, not ON,
// and a string that is no keyword (a URL) as a quoted literal.
func TestScriptDatabaseScopedConfigurations(t *testing.T) {
	typ := func(v string) string {
		switch v {
		case "":
			return ""
		case "0", "1":
			return "bit"
		case "WHEN_SUPPORTED", "PRIMARY", "https://x/y":
			return "nvarchar"
		}
		return "int"
	}
	cfg := func(name, v, sec string, def bool) *DatabaseScopedConfig {
		return &DatabaseScopedConfig{Name: name, Value: v, ValueForSecondary: sec, IsValueDefault: def,
			ValueType: typ(v), ValueForSecondaryType: typ(sec)}
	}
	num := func(c *DatabaseScopedConfig) *DatabaseScopedConfig {
		c.ValueType = "int"
		if c.ValueForSecondary != "" {
			c.ValueForSecondaryType = "int"
		}
		return c
	}
	scoped := []*DatabaseScopedConfig{
		cfg("ELEVATE_ONLINE", "WHEN_SUPPORTED", "", false),
		cfg("FULLTEXT_INDEX_VERSION", "1", "", false),
		cfg("IDENTITY_CACHE", "1", "", true),
		cfg("LEGACY_CARDINALITY_ESTIMATION", "1", "PRIMARY", false),
		num(cfg("MAXDOP", "1", "0", false)),
		num(cfg("NEW_NUMERIC", "1", "", false)),
		cfg("OPTIMIZE_FOR_AD_HOC_WORKLOADS", "1", "", false),
		cfg("PARAMETER_SNIFFING", "1", "", true),
		num(cfg("PAUSED_RESUMABLE_INDEX_ABORT_DURATION_MINUTES", "0", "", false)),
		cfg("LEDGER_DIGEST_STORAGE_ENDPOINT", "https://x/y", "", false),
		{Name: "UNTYPED", Value: "1"},
	}
	scoped[1].ValueType = "smallint"
	got := render(t, scripterOverDatabase("Sales"), &databaseLayout{opts: shippedDefaults(), scoped: scoped, major: 17})
	stmt := func(s string) string { return "ALTER DATABASE SCOPED CONFIGURATION " + s + ";\nGO\n" }
	want := "USE [Sales];\nGO\n" +
		stmt("SET ELEVATE_ONLINE = WHEN_SUPPORTED") +
		stmt("SET FULLTEXT_INDEX_VERSION = 1") +
		stmt("SET LEGACY_CARDINALITY_ESTIMATION = ON") +
		stmt("FOR SECONDARY SET LEGACY_CARDINALITY_ESTIMATION = PRIMARY") +
		stmt("SET MAXDOP = 1") +
		stmt("FOR SECONDARY SET MAXDOP = 0") +
		stmt("SET NEW_NUMERIC = 1") +
		stmt("SET OPTIMIZE_FOR_AD_HOC_WORKLOADS = ON") +
		stmt("SET PAUSED_RESUMABLE_INDEX_ABORT_DURATION_MINUTES = 0") +
		stmt("SET LEDGER_DIGEST_STORAGE_ENDPOINT = N'https://x/y'") +
		"-- Database scoped configuration UNTYPED reports a value with no SET form: not scripted.\n" +
		"USE [master];\nGO\n" +
		"ALTER AUTHORIZATION ON DATABASE::[Sales]"
	if !strings.Contains(got, want) {
		t.Errorf("scoped configurations wrong.\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}

	// 2016 has no is_value_default: every row reads false, and the
	// documented defaults decide.
	scoped16 := []*DatabaseScopedConfig{
		cfg("LEGACY_CARDINALITY_ESTIMATION", "0", "", false),
		num(cfg("MAXDOP", "2", "", false)),
		cfg("PARAMETER_SNIFFING", "0", "", false),
		cfg("QUERY_OPTIMIZER_HOTFIXES", "0", "", false),
		cfg("SOMETHING_NEWER", "1", "", false),
	}
	got = render(t, scripterOverDatabase("Sales"), &databaseLayout{opts: shippedDefaults(), scoped: scoped16, major: 13})
	want = "USE [Sales];\nGO\n" + stmt("SET MAXDOP = 2") + stmt("SET PARAMETER_SNIFFING = OFF") + "USE [master];\nGO\n"
	if !strings.Contains(got, want) {
		t.Errorf("2016 scoped configurations wrong.\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}
