package gosmo

import (
	"context"
	"testing"
)

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
	alter := func(col ColumnDefinition) func(context.Context) error {
		return func(c context.Context) error {
			return scriptTestDB().TableRef("dbo", "o'brien").AlterColumn(c, col)
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
