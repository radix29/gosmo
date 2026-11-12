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
		// SQL Server ends a -- comment at a lone CR: the CREATE after it is code.
		{"leading line comment ended by CR", "-- header\rCREATE PROCEDURE dbo.p AS SELECT 1", "-- header\rALTER PROCEDURE dbo.p AS SELECT 1"},
		{"leading block comment kept", "/* header */ CREATE VIEW dbo.v AS SELECT 1", "/* header */ ALTER VIEW dbo.v AS SELECT 1"},
		// Already re-runnable; rewriting would produce "ALTER OR ALTER".
		{"create or alter untouched", "CREATE OR ALTER VIEW dbo.v AS SELECT 1", "CREATE OR ALTER VIEW dbo.v AS SELECT 1"},
		// Only the CREATE that opens the definition may be rewritten.
		{"create in body untouched", "CREATE PROCEDURE dbo.p AS CREATE TABLE #t (a int)", "ALTER PROCEDURE dbo.p AS CREATE TABLE #t (a int)"},
		{"unrecognized left alone", "SELECT 1", "SELECT 1"},
		// T-SQL block comments nest; a lazy regex stopped at the first */.
		{"nested leading comment", "/* a /* b */ c */\nCREATE PROCEDURE dbo.p AS SELECT 1", "/* a /* b */ c */\nALTER PROCEDURE dbo.p AS SELECT 1"},
		{"comments mixed", "-- x /*\n/* y */ -- z\n  CREATE VIEW v AS SELECT 1", "-- x /*\n/* y */ -- z\n  ALTER VIEW v AS SELECT 1"},
		{"create inside the comment is not the keyword", "/* CREATE /* x */ */ CREATE VIEW v AS SELECT 1", "/* CREATE /* x */ */ ALTER VIEW v AS SELECT 1"},
		{"unterminated comment left alone", "/* CREATE VIEW v AS SELECT 1", "/* CREATE VIEW v AS SELECT 1"},
		{"quoted text first left alone", "[CREATE] VIEW v", "[CREATE] VIEW v"},
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
	got := buildTableScript("dbo", "T", "db", tableScriptParts{cols: cols, indexes: nil, fks: nil, ds: DataSpace{Name: "PRIMARY", IsDefaultFileGroup: true}}, opts)

	drop := strings.Index(got, "DROP TABLE")
	create := strings.Index(got, "CREATE TABLE")
	if drop < 0 || create < 0 {
		t.Fatalf("DROP-and-CREATE emitted only one half:\n%s", got)
	}
	if drop > create {
		t.Errorf("CREATE TABLE came before DROP TABLE; the script is not re-runnable:\n%s", got)
	}
}

// TestRenameModuleHeader pins H1: sp_rename and ALTER SCHEMA … TRANSFER leave
// sys.sql_modules' text naming the old object, so the header is rewritten to
// the current name — and only the header, and only when it does not match.
func TestRenameModuleHeader(t *testing.T) {
	cases := []struct {
		name                          string
		def, keyword, schema, objName string
		want                          string
	}{
		{"renamed", "CREATE PROCEDURE dbo.oldp AS SELECT 1", "PROCEDURE", "dbo", "newp",
			"CREATE PROCEDURE [dbo].[newp] AS SELECT 1"},
		{"transferred", "CREATE VIEW dbo.v AS SELECT 1 AS x", "VIEW", "hist", "v",
			"CREATE VIEW [hist].[v] AS SELECT 1 AS x"},
		{"unchanged two-part kept as written", "create view DBO_x.v as select 1", "VIEW", "DBO_x", "v",
			"create view DBO_x.v as select 1"},
		{"unchanged bracketed kept", "CREATE PROC [dbo].[p q] AS SELECT 1", "PROCEDURE", "dbo", "p q",
			"CREATE PROC [dbo].[p q] AS SELECT 1"},
		{"PROC abbreviation renamed", "CREATE PROC [dbo].[p] AS SELECT 1", "PROCEDURE", "dbo", "p2",
			"CREATE PROC [dbo].[p2] AS SELECT 1"},
		{"bracket escape compared unescaped", "CREATE VIEW [dbo].[a]]b] AS SELECT 1", "VIEW", "dbo", "a]b",
			"CREATE VIEW [dbo].[a]]b] AS SELECT 1"},
		{"quoted identifier", `CREATE FUNCTION "dbo"."f"() RETURNS int AS BEGIN RETURN 1 END`, "FUNCTION", "dbo", "g",
			`CREATE FUNCTION [dbo].[g]() RETURNS int AS BEGIN RETURN 1 END`},
		{"comment between keyword and name", "CREATE PROCEDURE /* x */ -- y\n dbo.oldp AS SELECT 1", "PROCEDURE", "dbo", "newp",
			"CREATE PROCEDURE /* x */ -- y\n [dbo].[newp] AS SELECT 1"},
		{"comments inside the name", "CREATE VIEW dbo /* a */ . /* b */ oldv AS SELECT 1", "VIEW", "dbo", "v",
			"CREATE VIEW [dbo].[v] AS SELECT 1"},
		{"create or alter", "CREATE OR ALTER VIEW dbo.oldv AS SELECT 1", "VIEW", "dbo", "v",
			"CREATE OR ALTER VIEW [dbo].[v] AS SELECT 1"},
		{"leading comment kept", "/* hdr /* nested */ */\nCREATE VIEW dbo.oldv AS SELECT 1", "VIEW", "dbo", "v",
			"/* hdr /* nested */ */\nCREATE VIEW [dbo].[v] AS SELECT 1"},
		{"one-part dbo unchanged", "CREATE PROCEDURE p AS SELECT 1", "PROCEDURE", "dbo", "p",
			"CREATE PROCEDURE p AS SELECT 1"},
		{"one-part renamed", "CREATE PROCEDURE oldp AS SELECT 1", "PROCEDURE", "dbo", "newp",
			"CREATE PROCEDURE [dbo].[newp] AS SELECT 1"},
		{"one-part transferred", "CREATE PROCEDURE p AS SELECT 1", "PROCEDURE", "hist", "p",
			"CREATE PROCEDURE [hist].[p] AS SELECT 1"},
		// A trigger lives in its table's schema; its one-part name resolves there.
		{"one-part trigger in its table's schema", "CREATE TRIGGER tr ON hist.t AFTER INSERT AS SELECT 1", "TRIGGER", "hist", "tr",
			"CREATE TRIGGER tr ON hist.t AFTER INSERT AS SELECT 1"},
		{"trigger renamed, ON untouched", "CREATE TRIGGER dbo.oldtr ON dbo.oldtr AFTER INSERT AS SELECT 1", "TRIGGER", "dbo", "tr",
			"CREATE TRIGGER [dbo].[tr] ON dbo.oldtr AFTER INSERT AS SELECT 1"},
		{"three-part", "CREATE VIEW db.dbo.oldv AS SELECT 1", "VIEW", "dbo", "v",
			"CREATE VIEW [dbo].[v] AS SELECT 1"},
		{"empty schema part", "CREATE VIEW db..oldv AS SELECT 1", "VIEW", "dbo", "v",
			"CREATE VIEW [dbo].[v] AS SELECT 1"},
		{"numbered procedure keeps its number", "CREATE PROCEDURE dbo.oldp;2 AS SELECT 1", "PROCEDURE", "dbo", "newp",
			"CREATE PROCEDURE [dbo].[newp];2 AS SELECT 1"},
		{"name in body untouched", "CREATE PROCEDURE dbo.p AS EXEC dbo.oldp", "PROCEDURE", "dbo", "p",
			"CREATE PROCEDURE dbo.p AS EXEC dbo.oldp"},
		{"wrong keyword left alone", "CREATE VIEW dbo.v AS SELECT 1", "PROCEDURE", "dbo", "p",
			"CREATE VIEW dbo.v AS SELECT 1"},
		{"unterminated comment left alone", "CREATE VIEW /* dbo.v AS SELECT 1", "VIEW", "dbo", "w",
			"CREATE VIEW /* dbo.v AS SELECT 1"},
		{"unterminated bracket left alone", "CREATE VIEW [dbo.v AS SELECT 1", "VIEW", "dbo", "w",
			"CREATE VIEW [dbo.v AS SELECT 1"},
		// A -- comment with no newline runs to the end of the text
		// (tsql.Spans): the name before it is still read.
		{"trailing line comment with no newline", "CREATE VIEW dbo.oldv -- x", "VIEW", "dbo", "v",
			"CREATE VIEW [dbo].[v] -- x"},
		{"line comment ending the text left alone", "CREATE VIEW -- dbo.v", "VIEW", "dbo", "w",
			"CREATE VIEW -- dbo.v"},
		{"bracket escape ends where ]] does not", "CREATE VIEW [dbo].[a]]'b] AS SELECT 1", "VIEW", "dbo", "w",
			"CREATE VIEW [dbo].[w] AS SELECT 1"},
		{"four parts left alone", "CREATE VIEW a.b.c.d AS SELECT 1", "VIEW", "dbo", "w",
			"CREATE VIEW a.b.c.d AS SELECT 1"},
		{"not a header left alone", "SELECT 1", "VIEW", "dbo", "w", "SELECT 1"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := renameModuleHeader(c.def, c.keyword, c.schema, c.objName); got != c.want {
				t.Errorf("renameModuleHeader(%q, %s, %s, %s) =\n%q\nwant\n%q", c.def, c.keyword, c.schema, c.objName, got, c.want)
			}
		})
	}
	// Composed with the ALTER rewrite, as writeModuleDefinition does.
	if got := alterModuleDefinition(renameModuleHeader("CREATE PROCEDURE dbo.oldp AS SELECT 1", "PROCEDURE", "dbo", "newp")); got != "ALTER PROCEDURE [dbo].[newp] AS SELECT 1" {
		t.Errorf("rename then ALTER = %q", got)
	}
}
