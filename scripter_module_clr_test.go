package gosmo

import (
	"database/sql"
	"testing"
)

// TestRenderCLRModule pins the CREATE each CLR module kind is rebuilt as.
// TestLiveCLRModulesScriptRoundTrip replays every one of these shapes.
func TestRenderCLRModule(t *testing.T) {
	method := func(m clrModule) clrModule {
		m.assembly, m.class, m.method = "w7clr", "W7Clr", "Twice"
		return m
	}
	cases := []struct {
		name string
		m    clrModule
		want string
	}{
		{"procedure", method(clrModule{keyword: "PROCEDURE", name: "[dbo].[p]", executeAs: "N'runner'",
			params: []clrParam{{name: "@x", typ: "int", def: "NULL"}, {name: "@y", typ: "int", output: true}}}),
			"CREATE PROCEDURE [dbo].[p]\n    @x int = NULL,\n    @y int OUTPUT\nWITH EXECUTE AS N'runner'\nAS EXTERNAL NAME [w7clr].[W7Clr].[Twice];"},
		{"procedure without parameters", method(clrModule{keyword: "PROCEDURE", name: "[dbo].[p]"}),
			"CREATE PROCEDURE [dbo].[p]\nAS EXTERNAL NAME [w7clr].[W7Clr].[Twice];"},
		{"scalar function", method(clrModule{keyword: "FUNCTION", name: "[dbo].[f]", returns: "int",
			nullOnNull: true, executeAs: "OWNER", params: []clrParam{{name: "@x", typ: "int", def: "21"}}}),
			"CREATE FUNCTION [dbo].[f](@x int = 21)\nRETURNS int\nWITH RETURNS NULL ON NULL INPUT, EXECUTE AS OWNER\nAS EXTERNAL NAME [w7clr].[W7Clr].[Twice];"},
		{"table function", method(clrModule{keyword: "FUNCTION", name: "[dbo].[f]",
			table: []string{"[n] int", "[s] nvarchar(10)"}, order: []string{"[n] DESC"}}),
			"CREATE FUNCTION [dbo].[f]()\nRETURNS TABLE (\n    [n] int,\n    [s] nvarchar(10)\n)\nORDER ([n] DESC)\nAS EXTERNAL NAME [w7clr].[W7Clr].[Twice];"},
		{"DML trigger", method(clrModule{keyword: "TRIGGER", name: "[dbo].[t]", on: "[dbo].[tbl]",
			events: []string{"INSERT", "DELETE"}, notForReplication: true}),
			"CREATE TRIGGER [dbo].[t] ON [dbo].[tbl]\nAFTER INSERT, DELETE\nNOT FOR REPLICATION\nAS EXTERNAL NAME [w7clr].[W7Clr].[Twice];"},
		{"INSTEAD OF trigger", method(clrModule{keyword: "TRIGGER", name: "[dbo].[t]", on: "[dbo].[v]",
			events: []string{"UPDATE"}, insteadOf: true}),
			"CREATE TRIGGER [dbo].[t] ON [dbo].[v]\nINSTEAD OF UPDATE\nAS EXTERNAL NAME [w7clr].[W7Clr].[Twice];"},
		{"database trigger", method(clrModule{keyword: "TRIGGER", name: "[ddl]", on: "DATABASE",
			executeAs: "N'runner'", events: []string{"CREATE_TABLE"}}),
			"CREATE TRIGGER [ddl] ON DATABASE\nWITH EXECUTE AS N'runner'\nAFTER CREATE_TABLE\nAS EXTERNAL NAME [w7clr].[W7Clr].[Twice];"},
		{"trigger on an event group", method(clrModule{keyword: "TRIGGER", name: "[ddl]", on: "ALL SERVER",
			events: []string{"DDL_LOGIN_EVENTS", "CREATE_DATABASE"}}),
			"CREATE TRIGGER [ddl] ON ALL SERVER\nAFTER DDL_LOGIN_EVENTS, CREATE_DATABASE\nAS EXTERNAL NAME [w7clr].[W7Clr].[Twice];"},
		{"class in a namespace", clrModule{keyword: "PROCEDURE", name: "[dbo].[p]", assembly: "a]b", class: "Ns.Cls", method: "M"},
			"CREATE PROCEDURE [dbo].[p]\nAS EXTERNAL NAME [a]]b].[Ns.Cls].[M];"},
	}
	for _, c := range cases {
		if got := renderCLRModule(c.m); got != c.want {
			t.Errorf("%s:\ngot:\n%s\nwant:\n%s", c.name, got, c.want)
		}
	}
	// The rebuilt CREATE is what Script as ALTER rewrites.
	if got := alterModuleDefinition(renderCLRModule(cases[0].m)); got[:16] != "ALTER PROCEDURE " {
		t.Errorf("ALTER of a CLR procedure:\n%s", got)
	}
}

// TestCLRTableColumn pins when a CLR table function's column gets COLLATE:
// only a collation other than the database default, as a table's column.
func TestCLRTableColumn(t *testing.T) {
	const db = "SQL_Latin1_General_CP1_CI_AS"
	for _, c := range []struct {
		name, typ, collation, want string
	}{
		{"n", "int", "", "[n] int"},
		{"s", "nvarchar(20)", db, "[s] nvarchar(20)"},
		{"s", "nvarchar(20)", "sql_latin1_general_cp1_ci_as", "[s] nvarchar(20)"},
		{"s", "nvarchar(20)", "Latin1_General_BIN2", "[s] nvarchar(20) COLLATE Latin1_General_BIN2"},
		{"a]b", "varchar(5)", "Japanese_CI_AS", "[a]]b] varchar(5) COLLATE Japanese_CI_AS"},
	} {
		if got := clrTableColumn(c.name, c.typ, c.collation, db); got != c.want {
			t.Errorf("clrTableColumn(%q, %q, %q) = %q, want %q", c.name, c.typ, c.collation, got, c.want)
		}
	}
	// No database collation read (an empty answer): nothing to compare
	// against, so no clause rather than a spurious one.
	if got := clrTableColumn("s", "nvarchar(20)", "Latin1_General_BIN2", ""); got != "[s] nvarchar(20)" {
		t.Errorf("with no database collation: %q", got)
	}
}

func TestCLRDefaultLiteral(t *testing.T) {
	for _, c := range []struct {
		base string
		text sql.NullString
		want string
	}{
		{"int", sql.NullString{String: "21", Valid: true}, "21"},
		{"", sql.NullString{}, "NULL"},
		{"nvarchar", sql.NullString{String: "it's", Valid: true}, "N'it''s'"},
		{"datetime2", sql.NullString{String: "2026-10-03T12:00:00", Valid: true}, "N'2026-10-03T12:00:00'"},
		{"varbinary", sql.NullString{String: "0x0A0B", Valid: true}, "0x0A0B"},
		{"binary", sql.NullString{String: "0x0A0B0000", Valid: true}, "0x0A0B0000"},
		{"float", sql.NullString{String: "1.0000000000000001e-001", Valid: true}, "1.0000000000000001e-001"},
		{"real", sql.NullString{String: "1.0000000149011612e-001", Valid: true}, "1.0000000149011612e-001"},
		{"money", sql.NullString{String: "-922337203685477.5808", Valid: true}, "-922337203685477.5808"},
		{"smallmoney", sql.NullString{String: "1.2345", Valid: true}, "1.2345"},
		{"decimal", sql.NullString{String: "12.345", Valid: true}, "12.345"},
		{"bit", sql.NullString{String: "1", Valid: true}, "1"},
		{"bigint", sql.NullString{String: "9007199254740993", Valid: true}, "9007199254740993"},
		{"smallint", sql.NullString{String: "-32768", Valid: true}, "-32768"},
		{"date", sql.NullString{String: "2026-10-03", Valid: true}, "N'2026-10-03'"},
		{"time", sql.NullString{String: "12:34:56.1234567", Valid: true}, "N'12:34:56.1234567'"},
		{"datetime", sql.NullString{String: "2026-10-03T12:34:56.997", Valid: true}, "N'2026-10-03T12:34:56.997'"},
		{"smalldatetime", sql.NullString{String: "2026-10-03T12:34:00", Valid: true}, "N'2026-10-03T12:34:00'"},
		{"datetimeoffset", sql.NullString{String: "2026-10-03T12:34:56.1234567+02:00", Valid: true}, "N'2026-10-03T12:34:56.1234567+02:00'"},
		{"nchar", sql.NullString{String: "ab", Valid: true}, "N'ab'"},
		{"uniqueidentifier", sql.NullString{String: "6F9619FF-8B86-D011-B42D-00C04FC964FF", Valid: true}, "N'6F9619FF-8B86-D011-B42D-00C04FC964FF'"},
	} {
		if got := clrDefaultLiteral(c.base, c.text); got != c.want {
			t.Errorf("clrDefaultLiteral(%q, %v) = %q, want %q", c.base, c.text, got, c.want)
		}
	}
}
