package gosmo

import (
	"strings"
	"testing"
)

func TestScriptOptionsVerbFoldsScriptDrops(t *testing.T) {
	if got := (ScriptOptions{ScriptDrops: true}).verb(); got != ScriptDrop {
		t.Errorf("ScriptDrops=true resolved to %v, want ScriptDrop — the older spelling must keep working", got)
	}
	// Verb is the general form and wins: a caller that sets it explicitly is
	// not silently given the drop instead.
	if got := (ScriptOptions{ScriptDrops: true, Verb: ScriptAlter}).verb(); got != ScriptAlter {
		t.Errorf("Verb=ScriptAlter with ScriptDrops=true resolved to %v, want ScriptAlter", got)
	}
	if got := (ScriptOptions{}).verb(); got != ScriptCreate {
		t.Errorf("zero ScriptOptions resolved to %v, want ScriptCreate", got)
	}
}

func TestAlterModuleDefinition(t *testing.T) {
	cases := []struct {
		name string
		def  string
		want string
	}{
		{"plain create", "CREATE VIEW dbo.v AS SELECT 1 AS x", "ALTER VIEW dbo.v AS SELECT 1 AS x"},
		{"lowercase", "create   view dbo.v as select 1", "ALTER   view dbo.v as select 1"},
		{"leading whitespace kept", "\n\nCREATE VIEW dbo.v AS SELECT 1", "\n\nALTER VIEW dbo.v AS SELECT 1"},
		{"leading line comment kept", "-- header\nCREATE VIEW dbo.v AS SELECT 1", "-- header\nALTER VIEW dbo.v AS SELECT 1"},
		{"leading block comment kept", "/* header */ CREATE VIEW dbo.v AS SELECT 1", "/* header */ ALTER VIEW dbo.v AS SELECT 1"},
		// Already re-runnable; rewriting would produce "ALTER OR ALTER".
		{"create or alter untouched", "CREATE OR ALTER VIEW dbo.v AS SELECT 1", "CREATE OR ALTER VIEW dbo.v AS SELECT 1"},
		// Only the CREATE that opens the definition may be rewritten.
		{"create in body untouched", "CREATE PROCEDURE dbo.p AS CREATE TABLE #t (a int)", "ALTER PROCEDURE dbo.p AS CREATE TABLE #t (a int)"},
		{"unrecognized left alone", "SELECT 1", "SELECT 1"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := alterModuleDefinition(c.def); got != c.want {
				t.Errorf("alterModuleDefinition(%q) = %q, want %q", c.def, got, c.want)
			}
		})
	}
}

func TestBuildTableScriptDropAndCreateEmitsBoth(t *testing.T) {
	cols := []*Column{{Name: "id", DataType: DataTypeInt, OrdinalPosition: 1}}
	opts := DefaultScriptOptions()
	opts.Verb = ScriptDropAndCreate
	got := buildTableScript("dbo", "T", "db", cols, nil, nil, DataSpace{Name: "PRIMARY", IsDefaultFileGroup: true}, opts)

	drop := strings.Index(got, "DROP TABLE")
	create := strings.Index(got, "CREATE TABLE")
	if drop < 0 || create < 0 {
		t.Fatalf("DROP-and-CREATE emitted only one half:\n%s", got)
	}
	if drop > create {
		t.Errorf("CREATE TABLE came before DROP TABLE; the script is not re-runnable:\n%s", got)
	}
}
