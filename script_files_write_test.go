package gosmo

import (
	"context"
	"strings"
	"testing"
)

// TestScriptFileAndFileGroupWrites pins the ALTER DATABASE statements behind
// the file and filegroup writes. See script_write_common_test.go.
//
// The size clauses are the part worth pinning beyond quoting: every one of
// them is a KB figure the caller passes and the statement renders with a unit
// suffix, and the sentinels differ per field — MaxSizeKB of 0 omits MAXSIZE
// while -1 is UNLIMITED, and GrowthPercent wins over GrowthKB when both are
// set.
func TestScriptFileAndFileGroupWrites(t *testing.T) {
	runScriptCases(t, []scriptCase{
		{"AddFile data", func(c context.Context) error {
			return scriptTestDB().AddFile(c, DatabaseFileSpec{
				Name: "App]Dat2", FileGroup: "FG'1", Path: `C:\d\a'2.ndf`,
				SizeKB: 8192, GrowthKB: 4096, MaxSizeKB: -1,
			})
		}, `ALTER DATABASE [App'DB] ADD FILE (NAME = [App]]Dat2], FILENAME = N'C:\d\a''2.ndf', SIZE = 8192KB, MAXSIZE = UNLIMITED, FILEGROWTH = 4096KB) TO FILEGROUP [FG'1]`},
		// M5 (review plan 2026-10-13): a smaller size is a shrink, not MODIFY
		// FILE (Msg 5039).
		{"File Shrink", func(c context.Context) error {
			return shrinkFile(c, scriptTestDB().FileRef("App'Dat]2"), ShrinkFileOptions{TargetMB: new(int64(17))})
		}, scriptUsePrefix + "DBCC SHRINKFILE (N'App''Dat]2', 17)"},
		// N2 (review plan 2026-10-13): SHRINKFILE's target 0 stops at the
		// file's creation size, so "as small as the data allows" is 1 MB.
		{"File Shrink to the minimum", func(c context.Context) error {
			return shrinkFile(c, scriptTestDB().FileRef("AppLog"), ShrinkFileOptions{TargetMB: new(int64(0))})
		}, scriptUsePrefix + "DBCC SHRINKFILE (N'AppLog', 1)"},
		{"AddFile log with percentage growth", func(c context.Context) error {
			return scriptTestDB().AddFile(c, DatabaseFileSpec{
				Name: "AppLog2", Type: "LOG", Path: `C:\d\l2.ldf`,
				SizeKB: 1024, GrowthPercent: 10, MaxSizeKB: 102400,
			})
		}, `ALTER DATABASE [App'DB] ADD LOG FILE (NAME = [AppLog2], FILENAME = N'C:\d\l2.ldf', SIZE = 1024KB, MAXSIZE = 102400KB, FILEGROWTH = 10%)`},
		{"File Alter", func(c context.Context) error {
			return scriptTestDB().FileRef("App]Dat2").Alter(c, DatabaseFileChanges{
				NewName: "New'Name", SizeKB: 16384, GrowthPercent: 25, MaxSizeKB: -1,
			})
		}, `ALTER DATABASE [App'DB] MODIFY FILE (NAME = [App]]Dat2], NEWNAME = [New'Name], SIZE = 16384KB, MAXSIZE = UNLIMITED, FILEGROWTH = 25%)`},
		{"File Drop", func(c context.Context) error {
			return scriptTestDB().FileRef("App]Dat2").Drop(c)
		}, "ALTER DATABASE [App'DB] REMOVE FILE [App]]Dat2]"},
		{"AddFileGroup", func(c context.Context) error {
			return scriptTestDB().AddFileGroup(c, "FG]2")
		}, "ALTER DATABASE [App'DB] ADD FILEGROUP [FG]]2]"},
		{"FileGroup Drop", func(c context.Context) error {
			return scriptTestDB().FileGroupRef("FG]2").Drop(c)
		}, "ALTER DATABASE [App'DB] REMOVE FILEGROUP [FG]]2]"},
		{"FileGroup SetDefault", func(c context.Context) error {
			return scriptTestDB().FileGroupRef("FG]2").SetDefault(c)
		}, "ALTER DATABASE [App'DB] MODIFY FILEGROUP [FG]]2] DEFAULT"},
		{"FileGroup SetReadOnly", func(c context.Context) error {
			return scriptTestDB().FileGroupRef("FG]2").SetReadOnly(c, true, TerminationNone)
		}, "ALTER DATABASE [App'DB] MODIFY FILEGROUP [FG]]2] READ_ONLY"},
		{"FileGroup SetReadOnly false is READ_WRITE", func(c context.Context) error {
			return scriptTestDB().FileGroupRef("FG]2").SetReadOnly(c, false, TerminationNone)
		}, "ALTER DATABASE [App'DB] MODIFY FILEGROUP [FG]]2] READ_WRITE"},
	})
}

// TestAlterFileWithNoChangeScriptsNothing pins the empty-modification case:
// buildAlterFileStatement returns "" rather than an ALTER DATABASE ... MODIFY
// FILE with no clauses, which is a syntax error, and Alter then
// returns without executing anything.
func TestAlterFileWithNoChangeScriptsNothing(t *testing.T) {
	ctx, script := WithScript(context.Background())
	if err := scriptTestDB().FileRef("AppDat").Alter(ctx, DatabaseFileChanges{}); err != nil {
		t.Fatalf("Alter with an empty DatabaseFileChanges: %v", err)
	}
	if len(script.Statements()) != 0 {
		t.Errorf("Statements = %q, want none", script.Statements())
	}
}

// TestScriptDatabaseOptionWrites pins the database-scoped option writes,
// where the statement shape itself changes with the arguments: a
// FOR SECONDARY clause, and CHANGE_TRACKING's ENABLE/DISABLE split (a
// disable takes no WITH clause at all).
func TestScriptDatabaseOptionWrites(t *testing.T) {
	runScriptCases(t, []scriptCase{
		{"SetUserAccess", func(c context.Context) error {
			return scriptTestDB().SetUserAccess(c, "SINGLE_USER")
		}, "ALTER DATABASE [App'DB] SET SINGLE_USER WITH ROLLBACK IMMEDIATE"},
		{"SetOnline", func(c context.Context) error {
			return scriptTestDB().SetOnline(c)
		}, "ALTER DATABASE [App'DB] SET ONLINE"},
		// K3 (review plan 2026-10-16): a RESTORING database is refused every
		// ALTER DATABASE; recovery is a RESTORE with no FROM clause.
		{"RecoverFromRestoring", func(c context.Context) error {
			return scriptTestDB().RecoverFromRestoring(c)
		}, "RESTORE DATABASE [App'DB] WITH RECOVERY"},
		{"SetDatabaseScopedConfig", func(c context.Context) error {
			return scriptTestDB().SetDatabaseScopedConfig(c, "MAXDOP", "4", ScopedConfigPrimary)
		}, scriptUsePrefix + "ALTER DATABASE SCOPED CONFIGURATION SET MAXDOP = 4"},
		{"SetDatabaseScopedConfig for secondary", func(c context.Context) error {
			return scriptTestDB().SetDatabaseScopedConfig(c, "LEGACY_CARDINALITY_ESTIMATION", "ON", ScopedConfigSecondary)
		}, scriptUsePrefix + "ALTER DATABASE SCOPED CONFIGURATION FOR SECONDARY SET LEGACY_CARDINALITY_ESTIMATION = ON"},
		{"Table.SetChangeTracking on with columns", func(c context.Context) error {
			return scriptTestDB().TableRef("dbo", "Sales.Archive").SetChangeTracking(c, true, true)
		}, scriptUsePrefix + atomicBatchText(
			"IF EXISTS (SELECT 1 FROM sys.change_tracking_tables WHERE object_id = OBJECT_ID(N'[dbo].[Sales.Archive]') AND is_track_columns_updated_on = 0)\n"+
				"    ALTER TABLE [dbo].[Sales.Archive] DISABLE CHANGE_TRACKING",
			"IF NOT EXISTS (SELECT 1 FROM sys.change_tracking_tables WHERE object_id = OBJECT_ID(N'[dbo].[Sales.Archive]'))\n"+
				"    ALTER TABLE [dbo].[Sales.Archive] ENABLE CHANGE_TRACKING WITH (TRACK_COLUMNS_UPDATED = ON)")},
		// M4 (review plan 2026-10-13): ENABLE on a tracked table is Msg 4996,
		// so a TRACK_COLUMNS_UPDATED change disables and re-enables, all or
		// nothing; the name is a literal inside OBJECT_ID.
		{"Table.SetChangeTracking on without columns", func(c context.Context) error {
			return scriptTestDB().TableRef("dbo", "o'b").SetChangeTracking(c, true, false)
		}, scriptUsePrefix + atomicBatchText(
			"IF EXISTS (SELECT 1 FROM sys.change_tracking_tables WHERE object_id = OBJECT_ID(N'[dbo].[o''b]') AND is_track_columns_updated_on = 1)\n"+
				"    ALTER TABLE [dbo].[o'b] DISABLE CHANGE_TRACKING",
			"IF NOT EXISTS (SELECT 1 FROM sys.change_tracking_tables WHERE object_id = OBJECT_ID(N'[dbo].[o''b]'))\n"+
				"    ALTER TABLE [dbo].[o'b] ENABLE CHANGE_TRACKING WITH (TRACK_COLUMNS_UPDATED = OFF)")},
		{"Table.SetChangeTracking off", func(c context.Context) error {
			return scriptTestDB().TableRef("dbo", "Sales.Archive").SetChangeTracking(c, false, false)
		}, scriptUsePrefix + "ALTER TABLE [dbo].[Sales.Archive] DISABLE CHANGE_TRACKING"},
	})
}

// TestScriptDatabaseObjectCreates pins the remaining CREATE statements a
// database issues for objects of its own.
func TestScriptDatabaseObjectCreates(t *testing.T) {
	runScriptCases(t, []scriptCase{
		{"CreateSynonym", func(c context.Context) error {
			return errOnly(scriptTestDB().CreateSynonym(c, CreateSynonymRequest{Schema: "dbo", Name: "S]yn", BaseObject: "[Other'DB].[dbo].[T]]bl]"}))
		}, scriptUsePrefix + "CREATE SYNONYM [dbo].[S]]yn] FOR [Other'DB].[dbo].[T]]bl]"},
		{"CreateStoredProcedure", func(c context.Context) error {
			return errOnly(scriptTestDB().CreateStoredProcedure(c, CreateStoredProcedureRequest{Schema: "dbo", Name: "usp_x", Body: "SELECT 1"}))
		}, scriptUsePrefix + "CREATE OR ALTER PROCEDURE [dbo].[usp_x]\nAS\nSELECT 1"},
		{"CreatePartitionScheme", func(c context.Context) error {
			return errOnly(scriptTestDB().CreatePartitionScheme(c, CreatePartitionSchemeRequest{Name: "ps]1", Function: "pf'1", FileGroups: []string{"FG1", "FG]2"}}))
		}, scriptUsePrefix + "CREATE PARTITION SCHEME [ps]]1] AS PARTITION [pf'1] TO ([FG1], [FG]]2])"},
	})
}

// TestCreateSynonymRefusesAnUnquotedBaseObject pins the injection defense on
// the one parameter that cannot be bracket-quoted as a whole, because it
// spans server/database/schema/object. A base object carrying an apostrophe
// outside a quoted part is refused rather than emitted.
func TestCreateSynonymRefusesAnUnquotedBaseObject(t *testing.T) {
	for _, bad := range []string{
		"other.dbo.T'bl",
		"dbo.T; DROP TABLE X--",
		"[unclosed",
		"a.b.c.d.e",
	} {
		ctx, script := WithScript(context.Background())
		_, err := scriptTestDB().CreateSynonym(ctx, CreateSynonymRequest{Schema: "dbo", Name: "Syn", BaseObject: bad})
		if err == nil {
			t.Errorf("CreateSynonym(%q) returned nil, want an error", bad)
		} else if !strings.Contains(err.Error(), "invalid base object") {
			t.Errorf("CreateSynonym(%q) error = %v, want it to name the base object", bad, err)
		}
		if len(script.Statements()) != 0 {
			t.Errorf("CreateSynonym(%q) scripted %q, want nothing", bad, script.Statements())
		}
	}
}

// atomicBatchText is atomicBatch's frame written out, so a case pins the
// frame too rather than reusing the function under test.
func atomicBatchText(stmts ...string) string {
	return "SET XACT_ABORT ON;\nBEGIN TRY\nBEGIN TRANSACTION;\n" + strings.Join(stmts, ";\n") + ";\n" +
		"COMMIT TRANSACTION;\nEND TRY\nBEGIN CATCH\nIF @@TRANCOUNT > 0 ROLLBACK TRANSACTION;\nTHROW;\nEND CATCH;"
}
