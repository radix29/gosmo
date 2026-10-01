//go:build livedb

// Live verification that Script as CREATE keeps the five table properties it
// once dropped without a word (review W1): a typed xml column's schema
// collection, a vector column's dimensions (whose loss made the script fail
// outright, Msg 2715), an ordered columnstore index's ORDER, a finite
// temporal HISTORY_RETENTION_PERIOD and a non-default LOCK_ESCALATION — and
// the selective XML indexes it once left as a comment.
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
	// Selective XML indexes (2012 SP1), on an untyped column with every path
	// form and a secondary index, and on a typed one, whose paths' XSD types
	// are inferred from the collection and must not be scripted (Msg 6368).
	tables = append(tables, "SelXml", "SelXmlTyped")
	liveExecIn(t, src, ctx,
		`CREATE TABLE dbo.SelXml (id int NOT NULL PRIMARY KEY, x xml NULL)`,
		`CREATE SELECTIVE XML INDEX sxi ON dbo.SelXml (x)
		 WITH XMLNAMESPACES (DEFAULT 'urn:d', N'urn:ä''q' AS a)
		 FOR (
		   item = '/a:root/a:item' AS XQUERY 'node()',
		   id   = '/a:root/a:item/@id' AS XQUERY 'xs:string' MAXLENGTH(20) SINGLETON,
		   s    = '/a:root/a:s' AS XQUERY 'xs:string',
		   d    = '/a:root/a:d' AS XQUERY 'xs:double',
		   n    = '/a:root/n' AS SQL nvarchar(30) SINGLETON,
		   m    = '/a:root/m' AS SQL nvarchar(max),
		   dec  = '/a:root/a:dec' AS SQL decimal(10, 3),
		   [ü p] = N'/root/ü'
		 ) WITH (PAD_INDEX = ON, FILLFACTOR = 80, ALLOW_ROW_LOCKS = OFF)`,
		`CREATE XML INDEX sxi_n ON dbo.SelXml (x) USING XML INDEX sxi FOR (n) WITH (FILLFACTOR = 70)`,
		`CREATE TABLE dbo.SelXmlTyped (id int NOT NULL PRIMARY KEY, x xml(CONTENT dbo.XC) NULL)`,
		`CREATE SELECTIVE XML INDEX sxi ON dbo.SelXmlTyped (x) FOR (a = '/a' AS XQUERY SINGLETON, an = '/a' AS XQUERY 'node()' SINGLETON)`,
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

	// Neither selective index is a primary XML index a PATH/VALUE/PROPERTY
	// one could be built over.
	selTable, err := src.TableByName(ctx, "dbo", "SelXml")
	if err != nil {
		t.Fatalf("TableByName(SelXml): %v", err)
	}
	xis, err := selTable.XMLIndexes(ctx)
	if err != nil {
		t.Fatalf("XMLIndexes: %v", err)
	}
	if len(xis) != 2 {
		t.Fatalf("XMLIndexes(SelXml) = %d indexes, want 2", len(xis))
	}
	for _, x := range xis {
		if x.IsPrimary || !x.IsSelective {
			t.Errorf("XMLIndexes(SelXml): %s read as primary %v, selective %v", x.Name, x.IsPrimary, x.IsSelective)
		}
	}

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
	xmlIndexes := `
SELECT i.name, xi.xml_index_type, ISNULL(u.name, '') AS using_index, ISNULL(sp.name, '') AS path,
       i.is_padded, i.fill_factor, i.allow_row_locks
FROM   sys.xml_indexes xi
JOIN   sys.indexes i ON i.object_id = xi.object_id AND i.index_id = xi.index_id
LEFT   JOIN sys.xml_indexes u ON u.object_id = xi.object_id AND u.index_id = xi.using_xml_index_id
LEFT   JOIN sys.selective_xml_index_paths sp ON sp.object_id = xi.object_id
            AND sp.index_id = xi.using_xml_index_id AND sp.path_id = xi.path_id
WHERE  xi.object_id = OBJECT_ID(@p1)
ORDER  BY i.name`
	selectivePaths := `
SELECT i.name, p.path_id, p.name, p.path, p.path_type, ISNULL(p.xquery_type_description, '') AS xtype,
       ISNULL(p.is_xquery_type_inferred, 0) AS inferred, ISNULL(p.xquery_max_length, 0) AS xmax,
       ISNULL(p.is_xquery_max_length_inferred, 0) AS xmax_inferred, p.is_node,
       ISNULL(TYPE_NAME(p.user_type_id), '') AS sqltype, ISNULL(p.max_length, 0) AS len,
       ISNULL(p.precision, 0) AS prec, ISNULL(p.scale, 0) AS scale, p.is_singleton
FROM   sys.selective_xml_index_paths p
JOIN   sys.indexes i ON i.object_id = p.object_id AND i.index_id = p.index_id
WHERE  p.object_id = OBJECT_ID(@p1)
ORDER  BY i.name, p.path_id`
	selectiveNamespaces := `
SELECT i.name, n.is_default_uri, n.uri, ISNULL(n.prefix, '') AS prefix
FROM   sys.selective_xml_index_namespaces n
JOIN   sys.indexes i ON i.object_id = n.object_id AND i.index_id = n.index_id
WHERE  n.object_id = OBJECT_ID(@p1)
ORDER  BY i.name, n.is_default_uri DESC, n.prefix`

	for _, tbl := range tables {
		compare(tbl+" columns", columns, "dbo."+tbl)
		if strings.HasPrefix(tbl, "SelXml") {
			compare(tbl+" xml indexes", xmlIndexes, "dbo."+tbl)
			compare(tbl+" selective paths", selectivePaths, "dbo."+tbl)
		}
		if tbl == "SelXml" {
			compare(tbl+" selective namespaces", selectiveNamespaces, "dbo."+tbl)
		}
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
