//go:build livedb

// Live verification that Script as CREATE keeps the five table properties it
// once dropped without a word (review W1): a typed xml column's schema
// collection, a vector column's dimensions (whose loss made the script fail
// outright, Msg 2715), an ordered columnstore index's ORDER, a finite
// temporal HISTORY_RETENTION_PERIOD and a non-default LOCK_ESCALATION.
//
// As in live_script_fidelity_test.go, the assertion is on the catalog after a
// replay into a second database, never on the script's text. Each shape is
// created only on a major that has it, so the test also runs, narrower, on
// gosmo's older supported instances.
//
//	go test -tags livedb . -run TestLiveScriptFacets -v \
//	  -livedb 'sqlserver://sa:PASS@host?TrustServerCertificate=true'
//
// Creates and drops two throwaway databases; touches nothing else.
package gosmo

import (
	"slices"
	"strings"
	"testing"
)

func TestLiveScriptFacets(t *testing.T) {
	db, ctx, done := liveDB(t)
	t.Cleanup(done)

	major := liveServer(t, db, ctx).serverMajorVersion()
	has := func(v ServerVersion) bool { return hasColumnSince(major, v) }

	src, dropSrc := liveScratchDB(t, db, ctx, "gosmo_facets_src")
	t.Cleanup(dropSrc)
	dst, dropDst := liveScratchDB(t, db, ctx, "gosmo_facets_dst")
	t.Cleanup(dropDst)

	// The collection is a dependency the table script names, not part of it;
	// float16 vectors are a 2025 preview feature, off by default per database.
	for _, d := range []*Database{src, dst} {
		liveExecIn(t, d, ctx, `CREATE XML SCHEMA COLLECTION dbo.XC AS N'<xsd:schema xmlns:xsd="http://www.w3.org/2001/XMLSchema"><xsd:element name="a" type="xsd:string"/></xsd:schema>'`)
		if has(SQLServer2025) {
			liveExecIn(t, d, ctx, `ALTER DATABASE SCOPED CONFIGURATION SET PREVIEW_FEATURES = ON`)
		}
	}

	tables := []string{"Xmls"}
	liveExecIn(t, src, ctx,
		`CREATE TABLE dbo.Xmls (id int NOT NULL PRIMARY KEY, d xml(DOCUMENT dbo.XC) NULL, c xml(CONTENT dbo.XC) NULL, u xml NULL)`,
		`ALTER TABLE dbo.Xmls SET (LOCK_ESCALATION = DISABLE)`,
	)
	if has(SQLServer2017) {
		tables = append(tables, "Temporal")
		liveExecIn(t, src, ctx,
			`CREATE TABLE dbo.Temporal (id int NOT NULL PRIMARY KEY,
			    vf datetime2 GENERATED ALWAYS AS ROW START NOT NULL,
			    vt datetime2 GENERATED ALWAYS AS ROW END NOT NULL,
			    PERIOD FOR SYSTEM_TIME (vf, vt))
			 WITH (SYSTEM_VERSIONING = ON (HISTORY_TABLE = dbo.TemporalHistory, HISTORY_RETENTION_PERIOD = 6 MONTHS))`,
			`ALTER TABLE dbo.Temporal SET (LOCK_ESCALATION = AUTO)`,
		)
	}
	if has(SQLServer2022) {
		tables = append(tables, "OrderedCCI", "OrderedNCCI")
		liveExecIn(t, src, ctx,
			`CREATE TABLE dbo.OrderedCCI (a int NULL, b int NULL, c int NULL)`,
			`CREATE CLUSTERED COLUMNSTORE INDEX cci ON dbo.OrderedCCI ORDER (c, a)`,
			`CREATE TABLE dbo.OrderedNCCI (id int NOT NULL PRIMARY KEY, a int NULL, b int NULL)`,
			`CREATE NONCLUSTERED COLUMNSTORE INDEX ncci ON dbo.OrderedNCCI (a, b) ORDER (b) WHERE a > 0`,
		)
	}
	if has(SQLServer2025) {
		tables = append(tables, "Vectors")
		liveExecIn(t, src, ctx,
			`CREATE TABLE dbo.Vectors (id int NOT NULL PRIMARY KEY, v vector(3) NULL, h vector(4, float16) NULL)`,
		)
	}
	t.Logf("major %d: %v", major, tables)

	opts := DefaultScriptOptions()
	opts.IncludeHeaders = false
	sc := NewScripter(src, opts)
	for _, tbl := range tables {
		script, err := sc.ScriptTable(ctx, "dbo", tbl)
		if err != nil {
			t.Fatalf("ScriptTable(%s): %v", tbl, err)
		}
		livePinnedRun(t, db, ctx, dst.Name, script)
	}

	// The oracle, written apart from the scripter's own reads.
	vector := "CAST(NULL AS int) AS dims, CAST(NULL AS nvarchar(60)) AS base"
	if has(SQLServer2025) {
		vector = "c.vector_dimensions AS dims, c.vector_base_type_desc AS base"
	}
	columns := `
SELECT c.name, TYPE_NAME(c.user_type_id) AS type, ISNULL(x.name, '') AS xml_collection,
       c.is_xml_document, ` + vector + `
FROM   sys.columns c LEFT JOIN sys.xml_schema_collections x ON x.xml_collection_id = c.xml_collection_id
WHERE  c.object_id = OBJECT_ID(@p1)`
	retention := "CAST(NULL AS int) AS retention, CAST(NULL AS nvarchar(60)) AS unit"
	if has(SQLServer2017) {
		retention = "t.history_retention_period AS retention, t.history_retention_period_unit_desc AS unit"
	}
	table := `
SELECT t.lock_escalation_desc, t.temporal_type, ` + retention + `
FROM   sys.tables t WHERE t.object_id = OBJECT_ID(@p1)`
	order := "CAST(0 AS tinyint) AS ord"
	if has(SQLServer2022) {
		order = "ic.column_store_order_ordinal AS ord"
	}
	indexColumns := `
SELECT i.type_desc, COL_NAME(ic.object_id, ic.column_id) AS col, ` + order + `
FROM   sys.index_columns ic JOIN sys.indexes i ON i.object_id = ic.object_id AND i.index_id = ic.index_id
WHERE  ic.object_id = OBJECT_ID(@p1) AND i.type IN (5, 6)`

	compare := func(what, q string, args ...any) {
		t.Helper()
		want := liveRowsAsStrings(t, src, ctx, q, args...)
		got := liveRowsAsStrings(t, dst, ctx, q, args...)
		if len(want) == 0 {
			t.Fatalf("%s: the source has nothing to compare — the oracle query is wrong", what)
		}
		if !slices.Equal(want, got) {
			t.Errorf("%s differs after the round trip\nsource:\n  %s\nreplayed:\n  %s",
				what, strings.Join(want, "\n  "), strings.Join(got, "\n  "))
		}
	}
	for _, tbl := range tables {
		compare(tbl+" columns", columns, "dbo."+tbl)
		compare(tbl+" table options", table, "dbo."+tbl)
		if strings.HasPrefix(tbl, "Ordered") {
			compare(tbl+" columnstore order", indexColumns, "dbo."+tbl)
		}
	}

	// A procedure's parameters carry the same two facets.
	proc := `CREATE PROCEDURE dbo.P @x xml(DOCUMENT dbo.XC) AS SELECT 1`
	wantParams := []string{"xml(DOCUMENT [dbo].[XC])"}
	if has(SQLServer2025) {
		proc = `CREATE PROCEDURE dbo.P @x xml(DOCUMENT dbo.XC), @v vector(3), @h vector(4, float16) AS SELECT 1`
		wantParams = append(wantParams, "vector(3)", "vector(4, float16)")
	}
	liveExecIn(t, src, ctx, proc)
	params, err := src.Parameters(ctx, "dbo", "P")
	if err != nil {
		t.Fatalf("Parameters: %v", err)
	}
	var gotParams []string
	for _, p := range params {
		gotParams = append(gotParams, p.TypeString())
	}
	if !slices.Equal(gotParams, wantParams) {
		t.Errorf("parameter types = %q, want %q", gotParams, wantParams)
	}
}
