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
			return scriptTestDB().AddFileContext(c, DatabaseFileSpec{
				Name: "App]Dat2", FileGroup: "FG'1", Path: `C:\d\a'2.ndf`,
				SizeKB: 8192, GrowthKB: 4096, MaxSizeKB: -1,
			})
		}, `ALTER DATABASE [App'DB] ADD FILE (NAME = [App]]Dat2], FILENAME = 'C:\d\a''2.ndf', SIZE = 8192KB, MAXSIZE = UNLIMITED, FILEGROWTH = 4096KB) TO FILEGROUP [FG'1]`},
		{"AddFile log with percentage growth", func(c context.Context) error {
			return scriptTestDB().AddFileContext(c, DatabaseFileSpec{
				Name: "AppLog2", Type: "LOG", Path: `C:\d\l2.ldf`,
				SizeKB: 1024, GrowthPercent: 10, MaxSizeKB: 102400,
			})
		}, `ALTER DATABASE [App'DB] ADD LOG FILE (NAME = [AppLog2], FILENAME = 'C:\d\l2.ldf', SIZE = 1024KB, MAXSIZE = 102400KB, FILEGROWTH = 10%)`},
		{"AlterFile", func(c context.Context) error {
			return scriptTestDB().AlterFileContext(c, "App]Dat2", FileModify{
				NewName: "New'Name", SizeKB: 16384, GrowthPercent: 25, MaxSizeKB: -1,
			})
		}, `ALTER DATABASE [App'DB] MODIFY FILE (NAME = [App]]Dat2], NEWNAME = [New'Name], SIZE = 16384KB, MAXSIZE = UNLIMITED, FILEGROWTH = 25%)`},
		{"RemoveFile", func(c context.Context) error {
			return scriptTestDB().RemoveFileContext(c, "App]Dat2")
		}, "ALTER DATABASE [App'DB] REMOVE FILE [App]]Dat2]"},
		{"AddFileGroup", func(c context.Context) error {
			return scriptTestDB().AddFileGroupContext(c, "FG]2")
		}, "ALTER DATABASE [App'DB] ADD FILEGROUP [FG]]2]"},
		{"RemoveFileGroup", func(c context.Context) error {
			return scriptTestDB().RemoveFileGroupContext(c, "FG]2")
		}, "ALTER DATABASE [App'DB] REMOVE FILEGROUP [FG]]2]"},
		{"SetDefaultFileGroup", func(c context.Context) error {
			return scriptTestDB().SetDefaultFileGroupContext(c, "FG]2")
		}, "ALTER DATABASE [App'DB] MODIFY FILEGROUP [FG]]2] DEFAULT"},
		{"SetFileGroupReadOnly", func(c context.Context) error {
			return scriptTestDB().SetFileGroupReadOnlyContext(c, "FG]2", true)
		}, "ALTER DATABASE [App'DB] MODIFY FILEGROUP [FG]]2] READONLY"},
		{"SetFileGroupReadOnly false is READWRITE", func(c context.Context) error {
			return scriptTestDB().SetFileGroupReadOnlyContext(c, "FG]2", false)
		}, "ALTER DATABASE [App'DB] MODIFY FILEGROUP [FG]]2] READWRITE"},
	})
}

// TestAlterFileWithNoChangeScriptsNothing pins the empty-modification case:
// buildAlterFileStatement returns "" rather than an ALTER DATABASE ... MODIFY
// FILE with no clauses, which is a syntax error, and AlterFileContext then
// returns without executing anything.
func TestAlterFileWithNoChangeScriptsNothing(t *testing.T) {
	ctx, script := WithScript(context.Background())
	if err := scriptTestDB().AlterFileContext(ctx, "AppDat", FileModify{}); err != nil {
		t.Fatalf("AlterFileContext with an empty FileModify: %v", err)
	}
	if len(script.Statements) != 0 {
		t.Errorf("Statements = %q, want none", script.Statements)
	}
}

// TestScriptDatabaseOptionWrites pins the database-scoped option writes,
// where the statement shape itself changes with the arguments: a
// FOR SECONDARY clause, and CHANGE_TRACKING's ENABLE/DISABLE split (a
// disable takes no WITH clause at all).
func TestScriptDatabaseOptionWrites(t *testing.T) {
	runScriptCases(t, []scriptCase{
		{"SetUserAccess", func(c context.Context) error {
			return scriptTestDB().SetUserAccessContext(c, "SINGLE_USER")
		}, "ALTER DATABASE [App'DB] SET SINGLE_USER WITH ROLLBACK IMMEDIATE"},
		{"SetDatabaseScopedConfig", func(c context.Context) error {
			return scriptTestDB().SetDatabaseScopedConfigContext(c, "MAXDOP", "4", false)
		}, scriptUsePrefix + "ALTER DATABASE SCOPED CONFIGURATION SET MAXDOP = 4"},
		{"SetDatabaseScopedConfig for secondary", func(c context.Context) error {
			return scriptTestDB().SetDatabaseScopedConfigContext(c, "LEGACY_CARDINALITY_ESTIMATION", "ON", true)
		}, scriptUsePrefix + "ALTER DATABASE SCOPED CONFIGURATION FOR SECONDARY SET LEGACY_CARDINALITY_ESTIMATION = ON"},
		{"SetTableChangeTracking on with columns", func(c context.Context) error {
			return scriptTestDB().SetTableChangeTrackingContext(c, "dbo", "Sales.Archive", true, true)
		}, scriptUsePrefix + "ALTER TABLE [dbo].[Sales.Archive] ENABLE CHANGE_TRACKING WITH (TRACK_COLUMNS_UPDATED = ON)"},
		{"SetTableChangeTracking off", func(c context.Context) error {
			return scriptTestDB().SetTableChangeTrackingContext(c, "dbo", "Sales.Archive", false, false)
		}, scriptUsePrefix + "ALTER TABLE [dbo].[Sales.Archive] DISABLE CHANGE_TRACKING"},
	})
}

// TestScriptDatabaseObjectCreates pins the remaining CREATE statements a
// database issues for objects of its own.
func TestScriptDatabaseObjectCreates(t *testing.T) {
	runScriptCases(t, []scriptCase{
		{"CreateSynonym", func(c context.Context) error {
			return scriptTestDB().CreateSynonymContext(c, "dbo", "S]yn", "[Other'DB].[dbo].[T]]bl]")
		}, scriptUsePrefix + "CREATE SYNONYM [dbo].[S]]yn] FOR [Other'DB].[dbo].[T]]bl]"},
		{"CreateSynonym defaults the schema to dbo", func(c context.Context) error {
			return scriptTestDB().CreateSynonymContext(c, "", "Syn", "[OtherDB].[dbo].[Tbl]")
		}, scriptUsePrefix + "CREATE SYNONYM [dbo].[Syn] FOR [OtherDB].[dbo].[Tbl]"},
		{"CreateStoredProcedure", func(c context.Context) error {
			return scriptTestDB().CreateStoredProcedureContext(c, "dbo", "usp_x", "SELECT 1")
		}, scriptUsePrefix + "CREATE OR ALTER PROCEDURE [dbo].[usp_x]\nAS\nSELECT 1"},
		{"CreatePartitionScheme", func(c context.Context) error {
			return scriptTestDB().CreatePartitionSchemeContext(c, "ps]1", "pf'1", []string{"FG1", "FG]2"})
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
		err := scriptTestDB().CreateSynonymContext(ctx, "dbo", "Syn", bad)
		if err == nil {
			t.Errorf("CreateSynonymContext(%q) returned nil, want an error", bad)
		} else if !strings.Contains(err.Error(), "invalid base object") {
			t.Errorf("CreateSynonymContext(%q) error = %v, want it to name the base object", bad, err)
		}
		if len(script.Statements) != 0 {
			t.Errorf("CreateSynonymContext(%q) scripted %q, want nothing", bad, script.Statements)
		}
	}
}
