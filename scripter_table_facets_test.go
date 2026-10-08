package gosmo

import (
	"strings"
	"testing"
)

// The table properties Script as CREATE once dropped without a word (review
// W1): a typed xml column's collection, a vector column's dimensions, an
// ordered columnstore index's ORDER, a finite temporal retention and a
// non-default LOCK_ESCALATION. Each case here is one of them.

func TestTypedXMLColumnKeepsItsCollection(t *testing.T) {
	cases := []struct {
		col  Column
		want string
	}{
		{Column{DataType: DataTypeXML}, "xml"},
		{Column{DataType: DataTypeXML, XMLSchemaCollectionSchema: "dbo", XMLSchemaCollection: "XC", IsXMLDocument: true},
			"xml(DOCUMENT [dbo].[XC])"},
		{Column{DataType: DataTypeXML, XMLSchemaCollectionSchema: "my.schema", XMLSchemaCollection: "X]C"},
			"xml(CONTENT [my.schema].[X]]C])"},
	}
	for _, c := range cases {
		if got := c.col.TypeString(); got != c.want {
			t.Errorf("Column.TypeString(%+v) = %q, want %q", c.col, got, c.want)
		}
	}
	p := Parameter{DataType: DataTypeXML, XMLSchemaCollectionSchema: "dbo", XMLSchemaCollection: "XC", IsXMLDocument: true}
	if got := p.TypeString(); got != "xml(DOCUMENT [dbo].[XC])" {
		t.Errorf("Parameter.TypeString = %q, want xml(DOCUMENT [dbo].[XC])", got)
	}
}

func TestVectorColumnKeepsItsDimensions(t *testing.T) {
	cases := []struct {
		col  Column
		want string
	}{
		{Column{DataType: "vector", VectorDimensions: 3, VectorBaseType: "float32"}, "vector(3)"},
		{Column{DataType: "vector", VectorDimensions: 3}, "vector(3)"},
		{Column{DataType: "vector", VectorDimensions: 4, VectorBaseType: "float16"}, "vector(4, float16)"},
	}
	for _, c := range cases {
		if got := c.col.TypeString(); got != c.want {
			t.Errorf("Column.TypeString(%+v) = %q, want %q", c.col, got, c.want)
		}
	}
	p := Parameter{DataType: "vector", VectorDimensions: 3, VectorBaseType: "float32"}
	if got := p.TypeString(); got != "vector(3)" {
		t.Errorf("Parameter.TypeString = %q, want vector(3)", got)
	}
}

func TestOrderedColumnstoreKeepsItsOrder(t *testing.T) {
	cci := &Index{Name: "cci", Type: IndexTypeClusteredColumnStore, IsClustered: true,
		IncludedColumns:  []IndexColumn{{Name: "a", IsIncluded: true}, {Name: "b", IsIncluded: true}, {Name: "c", IsIncluded: true}},
		ColumnstoreOrder: []string{"c", "a"}}
	if got, want := scriptIndex(cci, "[dbo].[T]", ScriptOptions{}),
		"CREATE CLUSTERED COLUMNSTORE INDEX [cci] ON [dbo].[T] ORDER ([c], [a]);"; !strings.Contains(got, want) {
		t.Errorf("clustered:\n%s\nwant %q", got, want)
	}
	ncci := &Index{Name: "ncci", Type: IndexTypeColumnStore,
		IncludedColumns:  []IndexColumn{{Name: "a", IsIncluded: true}, {Name: "b", IsIncluded: true}},
		ColumnstoreOrder: []string{"b"}, FilterDefinition: "([a]>(0))"}
	if got, want := scriptIndex(ncci, "[dbo].[T]", ScriptOptions{}),
		"CREATE NONCLUSTERED COLUMNSTORE INDEX [ncci]\n    ON [dbo].[T] ([a], [b]) ORDER ([b])\n    WHERE ([a]>(0));"; !strings.Contains(got, want) {
		t.Errorf("nonclustered:\n%s\nwant %q", got, want)
	}
}

func TestTemporalRetentionIsScripted(t *testing.T) {
	cases := []struct {
		o    tableScriptOptions
		want string
	}{
		{tableScriptOptions{HistorySchema: "dbo", HistoryTable: "TH"},
			"SYSTEM_VERSIONING = ON (HISTORY_TABLE = [dbo].[TH])"},
		{tableScriptOptions{HistorySchema: "dbo", HistoryTable: "TH", HistoryRetentionPeriod: 6, HistoryRetentionUnit: "MONTH"},
			"SYSTEM_VERSIONING = ON (HISTORY_TABLE = [dbo].[TH], HISTORY_RETENTION_PERIOD = 6 MONTHS)"},
		{tableScriptOptions{HistorySchema: "dbo", HistoryTable: "TH", HistoryRetentionPeriod: 1, HistoryRetentionUnit: "DAY"},
			"SYSTEM_VERSIONING = ON (HISTORY_TABLE = [dbo].[TH], HISTORY_RETENTION_PERIOD = 1 DAY)"},
		{tableScriptOptions{}, "SYSTEM_VERSIONING = ON"},
	}
	for _, c := range cases {
		if got := systemVersioningOption(c.o); got != c.want {
			t.Errorf("systemVersioningOption(%+v) = %q, want %q", c.o, got, c.want)
		}
	}
}

func TestLockEscalationIsScripted(t *testing.T) {
	p := tableScriptParts{
		cols:  []*Column{{Name: "id", DataType: DataTypeInt}},
		table: tableScriptOptions{LockEscalation: "DISABLE"},
	}
	got := buildTableScript("dbo", "T", "db", p, ScriptOptions{})
	create := strings.Index(got, "CREATE TABLE")
	alter := strings.Index(got, "ALTER TABLE [dbo].[T] SET (LOCK_ESCALATION = DISABLE);\nGO")
	if alter < 0 || alter < create {
		t.Errorf("want LOCK_ESCALATION = DISABLE after the CREATE:\n%s", got)
	}
	for _, o := range []tableScriptOptions{
		{LockEscalation: "TABLE"},
		{LockEscalation: ""},
		{LockEscalation: "DISABLE", IsMemoryOptimized: true},
	} {
		if s := lockEscalationStatement(o, "[dbo].[T]"); s != "" {
			t.Errorf("lockEscalationStatement(%+v) = %q, want none", o, s)
		}
	}
	if s := lockEscalationStatement(tableScriptOptions{LockEscalation: "AUTO"}, "[dbo].[T]"); !strings.Contains(s, "LOCK_ESCALATION = AUTO") {
		t.Errorf("AUTO: %q", s)
	}
}

// A float16 vector parses only with PREVIEW_FEATURES on in the database the
// script runs in (Msg 195 otherwise); the script names the setting, before
// the CREATE TABLE, and never changes it itself.
func TestFloat16VectorScriptNamesPreviewFeatures(t *testing.T) {
	const setting = "--   ALTER DATABASE SCOPED CONFIGURATION SET PREVIEW_FEATURES = ON;\n"
	script := func(cols ...*Column) string {
		opts := DefaultScriptOptions()
		opts.Verb = ScriptDropAndCreate
		return buildTableScript("dbo", "T", "db", tableScriptParts{cols: cols}, opts)
	}

	got := script(
		&Column{Name: "id", DataType: DataTypeInt},
		&Column{Name: "v", DataType: DataTypeVector, VectorDimensions: 3, VectorBaseType: "float32"},
		&Column{Name: "h]1", DataType: DataTypeVector, VectorDimensions: 4, VectorBaseType: "float16"},
		&Column{Name: "H2", DataType: DataTypeVector, VectorDimensions: 2, VectorBaseType: "FLOAT16"},
	)
	note := strings.Index(got, "-- [h]]1], [H2]: vector(n, float16) is a SQL Server 2025 preview feature.")
	set := strings.Index(got, setting)
	create := strings.Index(got, "CREATE TABLE")
	if note < 0 || set < note || create < set || note < strings.Index(got, "DROP TABLE") {
		t.Errorf("want the note naming [h]]1] and [H2], then the setting, between the DROP and the CREATE:\n%s", got)
	}
	if strings.Count(got, "PREVIEW_FEATURES") != 1 {
		t.Errorf("want PREVIEW_FEATURES only in the comment:\n%s", got)
	}

	for _, cols := range [][]*Column{
		{{Name: "v", DataType: DataTypeVector, VectorDimensions: 3}},
		{{Name: "v", DataType: DataTypeVector, VectorDimensions: 3, VectorBaseType: "float32"}},
		{{Name: "f", DataType: DataTypeVarChar, VectorBaseType: "float16"}},
		{{Name: "h", DataType: DataTypeVector, VectorDimensions: 4, VectorBaseType: "float16", IsDroppedLedgerColumn: true}},
	} {
		if got := script(cols...); strings.Contains(got, "PREVIEW_FEATURES") {
			t.Errorf("%+v: want no note:\n%s", *cols[0], got)
		}
	}
}

// A module's float16 parameters get the table script's note, by parameter
// name; none, no note.
func TestFloat16ParameterNote(t *testing.T) {
	got := float16Note([]string{"@h", "@q"})
	if !strings.HasPrefix(got, "-- @h, @q: vector(n, float16) is a SQL Server 2025 preview feature.") ||
		!strings.HasSuffix(got, "--   ALTER DATABASE SCOPED CONFIGURATION SET PREVIEW_FEATURES = ON;\n") {
		t.Errorf("note = %q", got)
	}
	if got := float16Note(nil); got != "" {
		t.Errorf("no parameters: note = %q", got)
	}
}

// A float16 vector the parameter catalog cannot see — a local, a table
// variable's or RETURNS table's column, a CAST — is found in the definition's
// code, and only there: not in a comment, a string or a quoted identifier.
func TestUsesFloat16Vector(t *testing.T) {
	for _, c := range []struct {
		name, def string
		want      bool
	}{
		{"declare", "CREATE PROCEDURE p AS DECLARE @v vector(4, float16); SELECT 1", true},
		{"table variable", "CREATE PROCEDURE p AS DECLARE @t TABLE (id int, v VECTOR ( 3 , FLOAT16 )); SELECT 1", true},
		{"returns table", "CREATE FUNCTION f() RETURNS @t TABLE (v vector(2,float16)) AS BEGIN RETURN END", true},
		{"cast in a view", "CREATE VIEW v AS SELECT CAST(x AS vector(2, float16)) AS h FROM t", true},
		{"comment between tokens", "CREATE PROCEDURE p AS DECLARE @v vector/* c */(4, -- c\nfloat16); SELECT 1", true},
		{"after a string", "CREATE PROCEDURE p AS DECLARE @s nvarchar(9) = N'x'; DECLARE @v vector(4, float16)", true},
		{"start of definition", "vector(4, float16)", true},

		{"float32", "CREATE PROCEDURE p AS DECLARE @v vector(4, float32), @w vector(3); SELECT 1", false},
		{"line comment", "CREATE PROCEDURE p AS -- DECLARE @v vector(4, float16)\nSELECT 1", false},
		{"nested block comment", "CREATE PROCEDURE p AS /* a /* b */ vector(4, float16) */ SELECT 1", false},
		{"string", "CREATE PROCEDURE p AS EXEC (N'DECLARE @v vector(4, float16)')", false},
		{"bracketed identifier", "CREATE PROCEDURE p AS SELECT 1 AS [vector(4, float16)]", false},
		{"quoted identifier", `CREATE PROCEDURE p AS SELECT 1 AS "vector(4, float16)"`, false},
		{"longer identifier", "CREATE PROCEDURE p AS SELECT my_vector(4, float16), dbo.x$vector(1, float16)", false},
	} {
		if got := usesFloat16Vector(c.def); got != c.want {
			t.Errorf("%s: usesFloat16Vector(%q) = %v, want %v", c.name, c.def, got, c.want)
		}
	}
	if !strings.HasPrefix(float16DefinitionNote, "-- The definition uses vector(n, float16), a SQL Server 2025 preview feature.") ||
		!strings.HasSuffix(float16DefinitionNote, "--   ALTER DATABASE SCOPED CONFIGURATION SET PREVIEW_FEATURES = ON;\n") {
		t.Errorf("note = %q", float16DefinitionNote)
	}
}

// A float16 vector spelled in dynamic SQL is found in a definition that
// executes some: in the literal's code, nested a level down, in a batch
// assigned to a variable first. Not in a plain string with no EXEC, nor in the
// dynamic batch's own comment or string.
func TestBuildsFloat16Vector(t *testing.T) {
	for _, c := range []struct {
		name, def string
		want      bool
	}{
		{"EXEC literal", "CREATE PROCEDURE p AS EXEC (N'DECLARE @v vector(4, float16)')", true},
		{"EXECUTE literal", "CREATE PROCEDURE p AS EXECUTE ('DECLARE @v VECTOR(4,FLOAT16)')", true},
		{"sp_executesql", "CREATE PROCEDURE p AS EXEC sys.sp_executesql N'SELECT CAST(@x AS vector(2, float16))', N'@x nvarchar(20)', @x = N'[1,2]'", true},
		{"via a variable", "CREATE PROCEDURE p AS DECLARE @s nvarchar(max) = N'DECLARE @v vector(4, float16);'; EXEC (@s)", true},
		{"after a doubled quote", "CREATE PROCEDURE p AS EXEC (N'SELECT N''it''''s''; DECLARE @v vector(4, float16)')", true},
		{"nested dynamic SQL", "CREATE PROCEDURE p AS EXEC (N'EXEC (N''DECLARE @v vector(4, float16)'')')", true},
		{"comment inside the batch's code span", "CREATE PROCEDURE p AS EXEC (N'DECLARE @v vector/* c */(4, float16)')", true},

		{"plain string, no EXEC", "CREATE PROCEDURE p AS SELECT N'DECLARE @v vector(4, float16)'", false},
		{"EXEC in a comment only", "CREATE PROCEDURE p AS /* EXEC */ SELECT N'vector(4, float16)'", false},
		{"executed batch's comment", "CREATE PROCEDURE p AS EXEC (N'SELECT 1 -- vector(4, float16)')", false},
		{"executed batch's string", "CREATE PROCEDURE p AS EXEC (N'SELECT N''vector(4, float16)''')", false},
		{"float32", "CREATE PROCEDURE p AS EXEC (N'DECLARE @v vector(4, float32)')", false},
		{"longer word than EXEC", "CREATE PROCEDURE p AS SELECT executed, @exec FROM t WHERE x = N'vector(4, float16)'", false},
		{"split across literals", "CREATE PROCEDURE p AS EXEC (N'DECLARE @v vector(4, ' + N'float16)')", false},
	} {
		if got := buildsFloat16Vector(c.def); got != c.want {
			t.Errorf("%s: buildsFloat16Vector(%q) = %v, want %v", c.name, c.def, got, c.want)
		}
	}
	if !strings.HasPrefix(float16DynamicNote, "-- The definition's dynamic SQL uses vector(n, float16)") ||
		!strings.HasSuffix(float16DynamicNote, "--   ALTER DATABASE SCOPED CONFIGURATION SET PREVIEW_FEATURES = ON;\n") {
		t.Errorf("note = %q", float16DynamicNote)
	}
}
