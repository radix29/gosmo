package gosmo

import (
	"context"
	"database/sql/driver"
	"errors"
	"strings"
	"testing"
)

// alterTestDB is scriptTestDB on a capture server whose column read answers
// for a plain column (no collation, not sparse, not masked): AlterColumn
// reads the column before it writes, under WithScript too.
func alterTestDB(t *testing.T) *Database {
	t.Helper()
	s := captureServer(t, 17)
	captured.reset(columnAttrsRow(nil, false, nil))
	return &Database{server: s, Name: scriptTestDB().Name}
}

// columnAttrsRow answers AlterColumn's read of the column's current
// collation, sparse flag and masking function (nil for NULL).
func columnAttrsRow(collation driver.Value, sparse bool, mask driver.Value) cannedRow {
	return cannedRow{match: "FROM   sys.columns c",
		cols: []string{"collation_name", "is_sparse", "masking_function"},
		row:  []driver.Value{collation, sparse, mask}}
}

// TestScriptTypedXMLAndVectorColumns pins the column types CreateTable and
// AlterColumn could not declare before: a typed xml (CONTENT and DOCUMENT)
// and a vector, with and without its base type. The spelling is the one
// ScriptTable gives the column read back.
func TestScriptTypedXMLAndVectorColumns(t *testing.T) {
	create := func(col ColumnDefinition) func(context.Context) error {
		return func(c context.Context) error {
			return errOnly(scriptTestDB().CreateTable(c, CreateTableRequest{Schema: "dbo", Name: "o'brien", Columns: []ColumnDefinition{col}}))
		}
	}
	db := alterTestDB(t)
	alter := func(col ColumnDefinition) func(context.Context) error {
		return func(c context.Context) error {
			return db.TableRef("dbo", "o'brien").AlterColumn(c, col)
		}
	}
	const head = scriptUsePrefix + "CREATE TABLE [dbo].[o'brien] (\n    "
	runScriptCases(t, []scriptCase{
		{"CreateTable typed xml CONTENT", create(ColumnDefinition{Name: "x", DataType: DataTypeXML,
			XMLSchemaCollectionSchema: "Sales.Archive", XMLSchemaCollection: "a]b", IsNullable: true}),
			head + "[x] xml(CONTENT [Sales.Archive].[a]]b]) NULL\n)"},
		{"CreateTable typed xml DOCUMENT", create(ColumnDefinition{Name: "x", DataType: DataTypeXML,
			XMLSchemaCollectionSchema: "dbo", XMLSchemaCollection: "o'brien", IsXMLDocument: true}),
			head + "[x] xml(DOCUMENT [dbo].[o'brien]) NOT NULL\n)"},
		{"CreateTable untyped xml", create(ColumnDefinition{Name: "x", DataType: DataTypeXML, IsNullable: true}),
			head + "[x] xml NULL\n)"},
		{"CreateTable vector", create(ColumnDefinition{Name: "v", DataType: DataTypeVector, VectorDimensions: 3}),
			head + "[v] vector(3) NOT NULL\n)"},
		{"CreateTable vector float32 is the default", create(ColumnDefinition{Name: "v", DataType: DataTypeVector, VectorDimensions: 3, VectorBaseType: "float32"}),
			head + "[v] vector(3) NOT NULL\n)"},
		{"CreateTable vector float16", create(ColumnDefinition{Name: "v", DataType: DataTypeVector, VectorDimensions: 1998, VectorBaseType: "float16", IsNullable: true}),
			head + "[v] vector(1998, float16) NULL\n)"},
		{"AlterColumn typed xml", alter(ColumnDefinition{Name: "a]b", DataType: DataTypeXML,
			XMLSchemaCollectionSchema: "dbo", XMLSchemaCollection: "Sales.Archive", IsXMLDocument: true, IsNullable: true}),
			scriptUsePrefix + "ALTER TABLE [dbo].[o'brien] ALTER COLUMN [a]]b] xml(DOCUMENT [dbo].[Sales.Archive]) NULL"},
		{"AlterColumn vector", alter(ColumnDefinition{Name: "v", DataType: DataTypeVector, VectorDimensions: 5}),
			scriptUsePrefix + "ALTER TABLE [dbo].[o'brien] ALTER COLUMN [v] vector(5) NOT NULL"},
	})
}

// TestTypedXMLAndVectorDefinitionsRefusedWhenUnsayable pins that a facet
// with no place in the type is refused before anything is scripted, rather
// than dropped: the column would be created untyped, or not parse.
func TestTypedXMLAndVectorDefinitionsRefusedWhenUnsayable(t *testing.T) {
	for name, col := range map[string]ColumnDefinition{
		"collection with no schema":   {DataType: DataTypeXML, XMLSchemaCollection: "c"},
		"schema with no collection":   {DataType: DataTypeXML, XMLSchemaCollectionSchema: "dbo"},
		"DOCUMENT with no collection": {DataType: DataTypeXML, IsXMLDocument: true},
		"collection on nvarchar":      {DataType: DataTypeNVarChar, XMLSchemaCollectionSchema: "dbo", XMLSchemaCollection: "c"},
		"vector with no dimensions":   {DataType: DataTypeVector},
		"vector negative dimensions":  {DataType: DataTypeVector, VectorDimensions: -1},
		"vector unknown base type":    {DataType: DataTypeVector, VectorDimensions: 3, VectorBaseType: "float64) --"},
		"dimensions on varbinary":     {DataType: DataTypeVarBinary, VectorDimensions: 3},
		"base type on xml":            {DataType: DataTypeXML, VectorBaseType: "float16"},
		"precision on vector":         {DataType: DataTypeVector, VectorDimensions: 3, Precision: new(3)},
	} {
		col.Name = "c"
		ctx, script := WithScript(context.Background())
		if _, err := scriptTestDB().CreateTable(ctx, CreateTableRequest{Schema: "dbo", Name: "t", Columns: []ColumnDefinition{col}}); err == nil {
			t.Errorf("CreateTable with %s = nil error, want a refusal", name)
		}
		if err := scriptTestDB().TableRef("dbo", "t").AlterColumn(ctx, col); err == nil {
			t.Errorf("AlterColumn with %s = nil error, want a refusal", name)
		}
		if n := len(script.Statements()); n != 0 {
			t.Errorf("%s scripted %d statements, want none", name, n)
		}
	}
}

// P2 (review plan 2026-10-15): a string column's collation is declared with
// COLLATE after its type, in CreateTable and in AlterColumn when the caller
// names one — no catalog read then.
func TestScriptColumnCollation(t *testing.T) {
	create := func(col ColumnDefinition) func(context.Context) error {
		return func(c context.Context) error {
			return errOnly(scriptTestDB().CreateTable(c, CreateTableRequest{Schema: "dbo", Name: "t", Columns: []ColumnDefinition{col}}))
		}
	}
	db := alterTestDB(t)
	alter := func(col ColumnDefinition) func(context.Context) error {
		return func(c context.Context) error { return db.TableRef("dbo", "t").AlterColumn(c, col) }
	}
	runScriptCases(t, []scriptCase{
		{"CreateTable collated varchar", create(ColumnDefinition{Name: "c", DataType: DataTypeVarChar, MaxLength: 20,
			Collation: "Latin1_General_CS_AS", IsNullable: true}),
			scriptUsePrefix + "CREATE TABLE [dbo].[t] (\n    [c] varchar(20) COLLATE Latin1_General_CS_AS NULL\n)"},
		{"CreateTable collated ntext", create(ColumnDefinition{Name: "c", DataType: DataTypeNText, Collation: "SQL_Latin1_General_CP1_CS_AS"}),
			scriptUsePrefix + "CREATE TABLE [dbo].[t] (\n    [c] ntext COLLATE SQL_Latin1_General_CP1_CS_AS NOT NULL\n)"},
		{"AlterColumn explicit collation", alter(ColumnDefinition{Name: "c", DataType: DataTypeNVarChar, MaxLength: -1,
			Collation: "Latin1_General_100_CI_AS_SC_UTF8"}),
			scriptUsePrefix + "ALTER TABLE [dbo].[t] ALTER COLUMN [c] nvarchar(MAX) COLLATE Latin1_General_100_CI_AS_SC_UTF8 NOT NULL"},
	})
}

// A collation on a type that has none, or a name COLLATE cannot take bare,
// is refused before anything is scripted.
func TestColumnCollationRefusedWhenUnsayable(t *testing.T) {
	for name, col := range map[string]ColumnDefinition{
		"collation on int":       {DataType: DataTypeInt, Collation: "Latin1_General_CS_AS"},
		"collation on varbinary": {DataType: DataTypeVarBinary, MaxLength: 10, Collation: "Latin1_General_CS_AS"},
		"collation on xml":       {DataType: DataTypeXML, Collation: "Latin1_General_CS_AS"},
		"injected collation":     {DataType: DataTypeVarChar, Collation: "Latin1_General_CS_AS NULL; DROP TABLE x --"},
		"bracketed collation":    {DataType: DataTypeVarChar, Collation: "[Latin1_General_CS_AS]"},
	} {
		col.Name = "c"
		ctx, script := WithScript(context.Background())
		if _, err := scriptTestDB().CreateTable(ctx, CreateTableRequest{Schema: "dbo", Name: "t", Columns: []ColumnDefinition{col}}); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("CreateTable with %s = %v, want ErrInvalidRequest", name, err)
		}
		if err := scriptTestDB().TableRef("dbo", "t").AlterColumn(ctx, col); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("AlterColumn with %s = %v, want ErrInvalidRequest", name, err)
		}
		if n := len(script.Statements()); n != 0 {
			t.Errorf("%s scripted %d statements, want none", name, n)
		}
	}
}

// AlterColumn restates what ALTER COLUMN would otherwise reset without a
// word (B4, review plan 2026-10-21). With no Collation, a string column keeps
// the one it has; a column with none (NULL, a non-string column becoming a
// string one) gets no clause. A SPARSE column stays SPARSE, written before
// NULL ("NULL SPARSE" is Msg 102), and asking it to be NOT NULL is refused. A
// masked column gets its mask back in the same atomic batch, the function
// unescaped from the catalog and quoted once. The column is read whatever the
// target type; one the read does not find is ErrNotFound and nothing is
// altered. On 2016, where ADD MASKED clears SPARSE (checked live on 13; 14
// and 17 keep both), a column with both is refused instead.
func TestAlterColumnRestatesCurrentAttributes(t *testing.T) {
	const read = "FROM   sys.columns c"
	plain := columnAttrsRow(nil, false, nil)
	cases := []struct {
		name    string
		major   int // 0 is 17
		canned  []cannedRow
		col     ColumnDefinition
		want    string // the ALTER sent; "" for none
		notFnd  bool
		invalid bool
		unsupp  bool
	}{
		{"keeps CS collation", 0, []cannedRow{columnAttrsRow("Latin1_General_CS_AS", false, nil)},
			ColumnDefinition{Name: "c", DataType: DataTypeVarChar, MaxLength: 50, IsNullable: true},
			"ALTER TABLE [dbo].[t] ALTER COLUMN [c] varchar(50) COLLATE Latin1_General_CS_AS NULL", false, false, false},
		{"none to restate", 0, []cannedRow{plain},
			ColumnDefinition{Name: "c", DataType: DataTypeNVarChar, MaxLength: 10},
			"ALTER TABLE [dbo].[t] ALTER COLUMN [c] nvarchar(10) NOT NULL", false, false, false},
		{"explicit wins", 0, []cannedRow{columnAttrsRow("Latin1_General_CS_AS", false, nil)},
			ColumnDefinition{Name: "c", DataType: DataTypeChar, MaxLength: 3, Collation: "Latin1_General_CI_AS"},
			"ALTER TABLE [dbo].[t] ALTER COLUMN [c] char(3) COLLATE Latin1_General_CI_AS NOT NULL", false, false, false},
		{"non-string drops collation", 0, []cannedRow{columnAttrsRow("Latin1_General_CS_AS", false, nil)},
			ColumnDefinition{Name: "c", DataType: DataTypeInt},
			"ALTER TABLE [dbo].[t] ALTER COLUMN [c] int NOT NULL", false, false, false},
		{"keeps SPARSE", 0, []cannedRow{columnAttrsRow("Latin1_General_CS_AS", true, nil)},
			ColumnDefinition{Name: "c", DataType: DataTypeVarChar, MaxLength: 20, IsNullable: true},
			"ALTER TABLE [dbo].[t] ALTER COLUMN [c] varchar(20) COLLATE Latin1_General_CS_AS SPARSE NULL", false, false, false},
		{"SPARSE NOT NULL refused", 0, []cannedRow{columnAttrsRow(nil, true, nil)},
			ColumnDefinition{Name: "c", DataType: DataTypeInt},
			"", false, true, false},
		{"keeps mask", 0, []cannedRow{columnAttrsRow(nil, false, "email()")},
			ColumnDefinition{Name: "c]d", DataType: DataTypeVarChar, MaxLength: 20, IsNullable: true},
			atomicBatchText(
				"ALTER TABLE [dbo].[t] ALTER COLUMN [c]]d] varchar(20) NULL",
				"ALTER TABLE [dbo].[t] ALTER COLUMN [c]]d] ADD MASKED WITH (FUNCTION = N'email()')"), false, false, false},
		{"keeps mask with a quote, and SPARSE", 0, []cannedRow{columnAttrsRow(nil, true, `partial(1,"X''X",0)`)},
			ColumnDefinition{Name: "c", DataType: DataTypeNVarChar, MaxLength: 30, IsNullable: true},
			atomicBatchText(
				"ALTER TABLE [dbo].[t] ALTER COLUMN [c] nvarchar(30) SPARSE NULL",
				`ALTER TABLE [dbo].[t] ALTER COLUMN [c] ADD MASKED WITH (FUNCTION = N'partial(1,"X''X",0)')`), false, false, false},
		{"keeps SPARSE on 2016", 13, []cannedRow{columnAttrsRow(nil, true, nil)},
			ColumnDefinition{Name: "c", DataType: DataTypeInt, IsNullable: true},
			"ALTER TABLE [dbo].[t] ALTER COLUMN [c] int SPARSE NULL", false, false, false},
		{"keeps mask on 2016", 13, []cannedRow{columnAttrsRow(nil, false, "default()")},
			ColumnDefinition{Name: "c", DataType: DataTypeInt, IsNullable: true},
			atomicBatchText(
				"ALTER TABLE [dbo].[t] ALTER COLUMN [c] int NULL",
				"ALTER TABLE [dbo].[t] ALTER COLUMN [c] ADD MASKED WITH (FUNCTION = N'default()')"), false, false, false},
		{"SPARSE and masked refused on 2016", 13, []cannedRow{columnAttrsRow(nil, true, "default()")},
			ColumnDefinition{Name: "c", DataType: DataTypeInt, IsNullable: true},
			"", false, false, true},
		{"no such column", 0, nil,
			ColumnDefinition{Name: "c", DataType: DataTypeVarChar, MaxLength: 5},
			"", true, false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			major := c.major
			if major == 0 {
				major = 17
			}
			s := captureServer(t, major)
			captured.reset(c.canned...)
			err := (&Database{server: s, Name: "app"}).TableRef("dbo", "t").AlterColumn(t.Context(), c.col)
			switch {
			case c.unsupp:
				if !errors.Is(err, ErrUnsupported) {
					t.Fatalf("err = %v, want ErrUnsupported", err)
				}
			case c.notFnd:
				if !errors.Is(err, ErrNotFound) {
					t.Fatalf("err = %v, want ErrNotFound", err)
				}
			case c.invalid:
				if !errors.Is(err, ErrInvalidRequest) {
					t.Fatalf("err = %v, want ErrInvalidRequest", err)
				}
			case err != nil:
				t.Fatalf("AlterColumn: %v", err)
			}
			if q := captured.find(read); q == "" {
				t.Error("column read not sent")
			} else if !strings.Contains(q, "c.name = @p2") {
				t.Errorf("column read %q does not match the column by name", q)
			}
			got := captured.find("ALTER TABLE")
			if _, rest, ok := splitUseBatch(got); ok {
				got = rest
			}
			if got != c.want {
				t.Errorf("ALTER sent\n got %q\nwant %q", got, c.want)
			}
		})
	}
}

// An identity column renders IDENTITY(seed,incr), and the zero value (both
// 0) the bare IDENTITY — SQL Server's (1,1) — rather than IDENTITY(0,0),
// which the server refuses (Msg 2753).
func TestScriptIdentityColumn(t *testing.T) {
	create := func(col ColumnDefinition) func(context.Context) error {
		return func(c context.Context) error {
			return errOnly(scriptTestDB().CreateTable(c, CreateTableRequest{Schema: "dbo", Name: "t", Columns: []ColumnDefinition{col}}))
		}
	}
	scale0 := 0
	prec := 18
	runScriptCases(t, []scriptCase{
		{"zero value", create(ColumnDefinition{Name: "id", DataType: DataTypeInt, IsIdentity: true}),
			scriptUsePrefix + "CREATE TABLE [dbo].[t] (\n    [id] int IDENTITY NOT NULL\n)"},
		{"explicit", create(ColumnDefinition{Name: "id", DataType: DataTypeBigInt, IsIdentity: true, IdentitySeed: 100, IdentityIncr: -5}),
			scriptUsePrefix + "CREATE TABLE [dbo].[t] (\n    [id] bigint IDENTITY(100,-5) NOT NULL\n)"},
		{"seed 0", create(ColumnDefinition{Name: "id", DataType: DataTypeTinyInt, IsIdentity: true, IdentityIncr: 1}),
			scriptUsePrefix + "CREATE TABLE [dbo].[t] (\n    [id] tinyint IDENTITY(0,1) NOT NULL\n)"},
		{"decimal scale 0", create(ColumnDefinition{Name: "id", DataType: DataTypeDecimal, Precision: &prec, Scale: &scale0,
			IsIdentity: true, IdentitySeed: 1, IdentityIncr: 1}),
			scriptUsePrefix + "CREATE TABLE [dbo].[t] (\n    [id] decimal(18,0) IDENTITY(1,1) NOT NULL\n)"},
	})
}

// An identity the server would refuse is refused before anything is
// scripted: an increment of 0 with a seed, a type that cannot be an
// identity, a nullable identity and an identity with a default.
func TestIdentityColumnRefusedWhenInvalid(t *testing.T) {
	scale2 := 2
	prec := 9
	for name, col := range map[string]ColumnDefinition{
		"increment 0":       {DataType: DataTypeInt, IsIdentity: true, IdentitySeed: 5},
		"varchar identity":  {DataType: DataTypeVarChar, MaxLength: 5, IsIdentity: true},
		"float identity":    {DataType: DataTypeFloat, IsIdentity: true},
		"decimal scale 2":   {DataType: DataTypeDecimal, Precision: &prec, Scale: &scale2, IsIdentity: true},
		"nullable identity": {DataType: DataTypeInt, IsIdentity: true, IsNullable: true},
		"identity default":  {DataType: DataTypeInt, IsIdentity: true, DefaultValue: "1"},
	} {
		col.Name = "id"
		ctx, script := WithScript(context.Background())
		if _, err := scriptTestDB().CreateTable(ctx, CreateTableRequest{Schema: "dbo", Name: "t", Columns: []ColumnDefinition{col}}); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("CreateTable with %s = %v, want ErrInvalidRequest", name, err)
		}
		if n := len(script.Statements()); n != 0 {
			t.Errorf("%s scripted %d statements, want none", name, n)
		}
	}
}

// TestScriptTriggerEnableDisable pins the DML trigger toggle: the trigger and
// the table or view it is on, both in the trigger's schema.
func TestScriptTriggerEnableDisable(t *testing.T) {
	tr := func() *Trigger {
		return &Trigger{db: scriptTestDB(), Schema: "Sales.Archive", Name: "tr]1", TableName: "o'brien"}
	}
	runScriptCases(t, []scriptCase{
		{"Enable", func(c context.Context) error { return tr().Enable(c) },
			scriptUsePrefix + "ENABLE TRIGGER [Sales.Archive].[tr]]1] ON [Sales.Archive].[o'brien]"},
		{"Disable", func(c context.Context) error { return tr().Disable(c) },
			scriptUsePrefix + "DISABLE TRIGGER [Sales.Archive].[tr]]1] ON [Sales.Archive].[o'brien]"},
	})
}

// A TriggerRef has no TableName, and the toggle statement must name the
// parent. It looks the parent up by schema and name — never caching it on
// the handle — and a trigger that is not there is ErrNotFound with no
// write sent.
func TestTriggerRefSetEnabledLooksUpTheParent(t *testing.T) {
	s := captureServer(t, 17)
	captured.reset(cannedRow{match: "FROM   sys.triggers tr", cols: []string{"name"}, row: []driver.Value{"o'brien"}})
	d := &Database{server: s, Name: "appdb"}
	h := d.TriggerRef("Sales.Archive", "tr1")
	if err := h.Disable(t.Context()); err != nil {
		t.Fatalf("Disable: %v", err)
	}
	q, args := captured.findArgs("FROM   sys.triggers tr")
	if q == "" || len(args) != 2 || args[0] != "Sales.Archive" || args[1] != "tr1" {
		t.Errorf("parent lookup = %q %v, want one by schema and name", q, args)
	}
	if got := captured.find("DISABLE TRIGGER"); !strings.HasSuffix(got, "DISABLE TRIGGER [Sales.Archive].[tr1] ON [Sales.Archive].[o'brien]") {
		t.Errorf("write = %q, want it ON the looked-up parent", got)
	}
	if h.TableName != "" {
		t.Errorf("TableName = %q, want the lookup left uncached", h.TableName)
	}
	if h.IsEnabled {
		t.Error("IsEnabled = true after Disable")
	}

	captured.reset()
	if err := d.TriggerRef("dbo", "gone").Enable(t.Context()); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing trigger: err = %v, want ErrNotFound", err)
	}
	if captured.count("ENABLE TRIGGER") != 0 {
		t.Error("a write was sent for a trigger the lookup did not find")
	}
}
