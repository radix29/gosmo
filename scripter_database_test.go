package gosmo

import (
	"strings"
	"testing"
)

// scripterOverDatabase builds a Scripter over a Database with the metadata
// DatabaseByNameContext would have filled in, so ScriptDatabaseContext
// renders without needing to refresh.
func scripterOverDatabase(name string) *Scripter {
	d := &Database{
		server:        &Server{},
		name:          name,
		recoveryModel: RecoveryModelFull,
		compatLevel:   CompatLevel2022,
		collation:     "SQL_Latin1_General_CP1_CI_AS",
	}
	opts := DefaultScriptOptions()
	opts.IncludeHeaders = false
	return NewScripter(d, opts)
}

func TestScriptDatabaseRendersFromCachedMetadata(t *testing.T) {
	got, err := scripterOverDatabase("Sales").ScriptDatabase()
	if err != nil {
		t.Fatalf("ScriptDatabase: %v", err)
	}
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
	sc.db.recoveryModel = ""
	sc.db.compatLevel = 0
	// Rendered directly: ScriptDatabaseContext would try to refresh these
	// from the server first, and there is no server here.
	got, err := sc.scriptDatabaseFrom(sc.db)
	if err != nil {
		t.Fatalf("scriptDatabaseFrom: %v", err)
	}
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
	got, err := sc.ScriptDatabase()
	if err != nil {
		t.Fatalf("ScriptDatabase: %v", err)
	}
	// The name reaches the script twice, quoted differently each time: a
	// string literal inside DB_ID, an identifier in CREATE DATABASE.
	if !strings.Contains(got, "IF DB_ID(N'O''Brien') IS NULL") {
		t.Errorf("existence check not single-quote escaped:\n%s", got)
	}
	if !strings.Contains(got, "CREATE DATABASE [O'Brien]") {
		t.Errorf("CREATE not bracket-quoted:\n%s", got)
	}
}
