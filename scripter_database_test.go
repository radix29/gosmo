package gosmo

import (
	"strings"
	"testing"
)

// scripterOverDatabase builds a Scripter over a Database with the metadata
// DatabaseByName would have filled in, so ScriptDatabase
// renders without needing to refresh.
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

func TestScriptDatabaseRendersFromCachedMetadata(t *testing.T) {
	got, err := scripterOverDatabase("Sales").ScriptDatabase(t.Context())
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
	sc.db.RecoveryModel = ""
	sc.db.CompatibilityLevel = 0
	// Rendered directly: ScriptDatabase would try to refresh these
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
	got, err := sc.ScriptDatabase(t.Context())
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

// A Server built as &Server{db: db} — anything but NewServer — has no
// ServerInfo, and the header read it unguarded.
func TestScriptDatabaseHeaderSurvivesAServerWithNoInfo(t *testing.T) {
	sc := scripterOverDatabase("Sales")
	sc.opts.IncludeHeaders = true
	got, err := sc.ScriptDatabase(t.Context())
	if err != nil {
		t.Fatalf("ScriptDatabase: %v", err)
	}
	if !strings.Contains(got, "/* Database: Sales  Version:  */") {
		t.Errorf("header missing or malformed with a nil ServerInfo:\n%s", got)
	}
}

// With a ServerInfo present the header still names the version.
func TestScriptDatabaseHeaderNamesTheVersion(t *testing.T) {
	sc := scripterOverDatabase("Sales")
	sc.db.server.info = &ServerInfo{ProductVersion: "14.0.3480.0"}
	sc.opts.IncludeHeaders = true
	got, err := sc.ScriptDatabase(t.Context())
	if err != nil {
		t.Fatalf("ScriptDatabase: %v", err)
	}
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
		got, err := sc.ScriptDatabase(t.Context())
		if err != nil {
			t.Fatalf("verb %d: %v", tc.verb, err)
		}
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
