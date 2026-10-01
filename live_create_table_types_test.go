//go:build livedb

// Live verification that CreateTable and AlterColumn declare a typed xml
// column and, on SQL Server 2025, a vector column — asserted on the catalog
// read back, not on the statement's text.
//
//	go test -tags livedb . -run TestLiveCreateTableTypedXMLAndVector -v \
//	  -livedb 'sqlserver://sa:PASS@host?TrustServerCertificate=true'
//
// Creates and drops one throwaway database; touches nothing else.
package gosmo

import "testing"

func TestLiveCreateTableTypedXMLAndVector(t *testing.T) {
	db, ctx, done := liveDB(t)
	t.Cleanup(done)

	major := liveServer(t, db, ctx).serverMajorVersion()
	vectors := hasColumnSince(major, SQLServer2025)

	d, drop := liveScratchDB(t, db, ctx, "gosmo_create_types")
	t.Cleanup(drop)
	liveExecIn(t, d, ctx, `CREATE XML SCHEMA COLLECTION dbo.XC AS N'<xsd:schema xmlns:xsd="http://www.w3.org/2001/XMLSchema"><xsd:element name="a" type="xsd:string"/></xsd:schema>'`)
	if vectors {
		// float16 is a 2025 preview feature, off by default per database.
		liveExecIn(t, d, ctx, `ALTER DATABASE SCOPED CONFIGURATION SET PREVIEW_FEATURES = ON`)
	}

	cols := []ColumnDefinition{
		{Name: "id", DataType: DataTypeInt, IsPrimaryKey: true},
		{Name: "c", DataType: DataTypeXML, XMLSchemaCollectionSchema: "dbo", XMLSchemaCollection: "XC", IsNullable: true},
		{Name: "d", DataType: DataTypeXML, XMLSchemaCollectionSchema: "dbo", XMLSchemaCollection: "XC", IsXMLDocument: true, IsNullable: true},
		{Name: "u", DataType: DataTypeXML, IsNullable: true},
	}
	want := map[string]string{
		"id": "int",
		"c":  "xml(CONTENT [dbo].[XC])",
		"d":  "xml(DOCUMENT [dbo].[XC])",
		"u":  "xml(DOCUMENT [dbo].[XC])", // after the AlterColumn below
	}
	if vectors {
		cols = append(cols,
			ColumnDefinition{Name: "v", DataType: DataTypeVector, VectorDimensions: 3, IsNullable: true},
			ColumnDefinition{Name: "h", DataType: DataTypeVector, VectorDimensions: 4, VectorBaseType: "float16", IsNullable: true})
		want["v"] = "vector(3)"
		want["h"] = "vector(4, float16)"
	}
	tbl, err := d.CreateTable(ctx, CreateTableRequest{Schema: "dbo", Name: "Typed", Columns: cols})
	if err != nil {
		t.Fatalf("CreateTable: %v", err)
	}
	if err := tbl.AlterColumn(ctx, ColumnDefinition{Name: "u", DataType: DataTypeXML,
		XMLSchemaCollectionSchema: "dbo", XMLSchemaCollection: "XC", IsXMLDocument: true, IsNullable: true}); err != nil {
		t.Fatalf("AlterColumn untyped xml to typed: %v", err)
	}

	got, err := tbl.Columns(ctx)
	if err != nil {
		t.Fatalf("Columns: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("Columns = %d, want %d", len(got), len(want))
	}
	for _, c := range got {
		if s := c.TypeString(); s != want[c.Name] {
			t.Errorf("column %s reads back as %s, want %s", c.Name, s, want[c.Name])
		}
	}
	// The collection is enforced, not merely recorded: DOCUMENT refuses a
	// fragment, and the collection refuses an element it does not declare.
	if _, err := d.exec(ctx, `INSERT dbo.Typed (id, d) VALUES (1, N'<b/>')`); err == nil {
		t.Errorf("an element outside the collection was accepted by the typed column")
	}
	if !vectors {
		t.Logf("major %d: vector columns not exercised (2025 and later)", major)
	}
}
