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
