//go:build livedb

// Live verification that a scripted table, statistic or module recreates the
// object it was scripted from — not merely a script that runs.
//
// Every feature below was once dropped by the scripter without a word: the
// script ran cleanly and produced a different object. So the assertion is on
// the catalog after a replay, compared row for row with the source's, never
// on the script's text.
//
//	go test -tags livedb . -run TestLiveScriptFidelity -v \
//	  -livedb 'sqlserver://sa:PASS@host?TrustServerCertificate=true'
//
// Creates and drops two throwaway databases; touches nothing else.
package gosmo

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"
	"testing"
)

// livePinnedRun runs script batch by batch on one connection, in database
// dbName. A query window is one session, and a script's SET QUOTED_IDENTIFIER
// OFF batch only governs the CREATE after it on that same session — replaying
// batch by batch over a pool, as liveRunScript does, would lose it.
func livePinnedRun(t *testing.T, db *sql.DB, ctx context.Context, dbName, script string) {
	t.Helper()
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("pin a connection: %v", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "USE "+quoteIdent(dbName)); err != nil {
		t.Fatalf("USE %s: %v", dbName, err)
	}
	for _, batch := range splitGoBatches(script) {
		if _, err := conn.ExecContext(ctx, batch); err != nil {
			t.Fatalf("the generated script does not run in %s:\n%s\n\nfailing batch:\n%s\n\n%v", dbName, script, batch, err)
		}
	}
}

// liveRowsAsStrings runs q in d and renders each row as one string, so two
// databases' catalogs can be compared as sorted lists.
func liveRowsAsStrings(t *testing.T, d *Database, ctx context.Context, q string, args ...any) []string {
	t.Helper()
	rows, err := d.query(ctx, q, args...)
	if err != nil {
		t.Fatalf("catalog read in %s: %v\n%s", d.Name, err, q)
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatal(err)
		}
		parts := make([]string, len(vals))
		for i, v := range vals {
			if b, ok := v.([]byte); ok {
				v = string(b)
			}
			parts[i] = fmt.Sprintf("%s=%v", cols[i], v)
		}
		out = append(out, strings.Join(parts, " "))
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	slices.Sort(out)
	return out
}

// Catalog reads written independently of the scripter's own queries: they
// are the oracle, so they must not share its mistakes.
const (
	liveFidelityColumns = `
SELECT c.name, TYPE_NAME(c.user_type_id) AS type, SCHEMA_NAME(tp.schema_id) AS type_schema,
       c.max_length, c.precision, c.scale, c.is_nullable, c.is_identity,
       c.is_rowguidcol, c.is_sparse, c.is_column_set, c.is_masked,
       ISNULL(mc.masking_function, '') AS mask, c.generated_always_type, c.is_hidden,
       ISNULL(c.collation_name, '') AS collation,
       ISNULL(cc.is_persisted, 0) AS persisted, ISNULL(ic.is_not_for_replication, 0) AS nfr,
       ISNULL(dc.name, '') AS default_name, ISNULL(dc.definition, '') AS default_def
FROM   sys.columns c
JOIN   sys.types tp ON tp.user_type_id = c.user_type_id
LEFT   JOIN sys.masked_columns mc ON mc.object_id = c.object_id AND mc.column_id = c.column_id
LEFT   JOIN sys.computed_columns cc ON cc.object_id = c.object_id AND cc.column_id = c.column_id
LEFT   JOIN sys.identity_columns ic ON ic.object_id = c.object_id AND ic.column_id = c.column_id
LEFT   JOIN sys.default_constraints dc ON dc.parent_object_id = c.object_id AND dc.parent_column_id = c.column_id
WHERE  c.object_id = OBJECT_ID(@p1)`

	liveFidelityChecks = `
SELECT name, definition, is_disabled, is_not_trusted, is_not_for_replication
FROM   sys.check_constraints WHERE parent_object_id = OBJECT_ID(@p1)`

	liveFidelityIndexes = `
SELECT i.name, i.type_desc, i.is_unique, i.is_primary_key, i.is_disabled, i.fill_factor,
       i.is_padded, i.ignore_dup_key, i.allow_row_locks, i.allow_page_locks, i.compression_delay,
       (SELECT TOP 1 p.data_compression_desc FROM sys.partitions p
        WHERE p.object_id = i.object_id AND p.index_id = i.index_id) AS compression
FROM   sys.indexes i WHERE i.object_id = OBJECT_ID(@p1)`

	liveFidelityTable = `
SELECT t.temporal_type, ISNULL(OBJECT_SCHEMA_NAME(t.history_table_id) + '.' + OBJECT_NAME(t.history_table_id), '') AS history,
       ISNULL(COL_NAME(p.object_id, p.start_column_id), '') AS period_start,
       ISNULL(COL_NAME(p.object_id, p.end_column_id), '') AS period_end
FROM   sys.tables t LEFT JOIN sys.periods p ON p.object_id = t.object_id
WHERE  t.object_id = OBJECT_ID(@p1)`

	liveFidelityStats = `
SELECT s.name, s.has_filter, ISNULL(s.filter_definition, '') AS filter, s.no_recompute, s.is_incremental,
       STUFF((SELECT ',' + COL_NAME(sc.object_id, sc.column_id) FROM sys.stats_columns sc
              WHERE sc.object_id = s.object_id AND sc.stats_id = s.stats_id
              ORDER BY sc.stats_column_id FOR XML PATH('')), 1, 1, '') AS cols
FROM   sys.stats s WHERE s.object_id = OBJECT_ID(@p1) AND s.name = @p2`

	// G4: the XML index's form and the primary it is built over, and every
	// tessellation setting of a spatial index, by index name.
	liveFidelityXMLIndexes = `
SELECT xi.name, xi.xml_index_type, ISNULL(xi.secondary_type_desc, '') AS secondary,
       ISNULL(p.name, '') AS primary_index, COL_NAME(ic.object_id, ic.column_id) AS col
FROM   sys.xml_indexes xi
JOIN   sys.index_columns ic ON ic.object_id = xi.object_id AND ic.index_id = xi.index_id
LEFT   JOIN sys.xml_indexes p ON p.object_id = xi.object_id AND p.index_id = xi.using_xml_index_id
WHERE  xi.object_id = OBJECT_ID(@p1)`

	liveFidelitySpatial = `
SELECT i.name, st.tessellation_scheme, st.bounding_box_xmin, st.bounding_box_ymin,
       st.bounding_box_xmax, st.bounding_box_ymax, st.level_1_grid_desc, st.level_2_grid_desc,
       st.level_3_grid_desc, st.level_4_grid_desc, st.cells_per_object,
       COL_NAME(ic.object_id, ic.column_id) AS col
FROM   sys.spatial_index_tessellations st
JOIN   sys.indexes i ON i.object_id = st.object_id AND i.index_id = st.index_id
JOIN   sys.index_columns ic ON ic.object_id = st.object_id AND ic.index_id = st.index_id
WHERE  st.object_id = OBJECT_ID(@p1)`

	liveFidelityModule = `
SELECT uses_ansi_nulls, uses_quoted_identifier
FROM   sys.sql_modules WHERE object_id = OBJECT_ID(@p1)`
)

func liveFidelitySetup(t *testing.T, d *Database, ctx context.Context) {
	t.Helper()
	liveExecIn(t, d, ctx,
		`CREATE SCHEMA app`,
		`CREATE SCHEMA hist`,
		`CREATE TYPE app.Phone FROM varchar(20) NULL`,
		`CREATE PARTITION FUNCTION pf_fid (INT) AS RANGE RIGHT FOR VALUES (100, 200)`,
		`CREATE PARTITION SCHEME ps_fid AS PARTITION pf_fid ALL TO ([PRIMARY])`,
	)
}

func TestLiveScriptFidelity(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()

	src, dropSrc := liveScratchDB(t, db, ctx, "gosmo_fidelity_src")
	defer dropSrc()
	dst, dropDst := liveScratchDB(t, db, ctx, "gosmo_fidelity_dst")
	defer dropDst()
	liveFidelitySetup(t, src, ctx)
	liveFidelitySetup(t, dst, ctx)

	liveExecIn(t, src, ctx,
		`CREATE TABLE dbo.Fidelity (
		    ID     int IDENTITY(1,1) NOT FOR REPLICATION NOT NULL,
		    Stamp  datetime2(0) NOT NULL,
		    T0     time(0) NULL,
		    DTO    datetimeoffset(0) NULL,
		    Guid   uniqueidentifier ROWGUIDCOL NOT NULL CONSTRAINT DF_Fidelity_Guid DEFAULT NEWID(),
		    Total  AS (ID * 2) PERSISTED NOT NULL,
		    Loose  AS (ID + 1),
		    Phone  app.Phone NULL,
		    Email  nvarchar(100) MASKED WITH (FUNCTION = 'partial(1,"XXX",0)') NULL,
		    CS     varchar(10) COLLATE Latin1_General_CS_AS NULL,
		    ValidFrom datetime2(7) GENERATED ALWAYS AS ROW START HIDDEN NOT NULL,
		    ValidTo   datetime2(7) GENERATED ALWAYS AS ROW END HIDDEN NOT NULL,
		    PERIOD FOR SYSTEM_TIME (ValidFrom, ValidTo),
		    CONSTRAINT PK_Fidelity PRIMARY KEY CLUSTERED (ID) WITH (DATA_COMPRESSION = PAGE, ALLOW_ROW_LOCKS = OFF)
		 ) WITH (SYSTEM_VERSIONING = ON (HISTORY_TABLE = hist.Fidelity_History))`,
		`CREATE UNIQUE INDEX IX_Fidelity_Total ON dbo.Fidelity (Total)
		 WITH (IGNORE_DUP_KEY = ON, PAD_INDEX = ON, FILLFACTOR = 80, ALLOW_PAGE_LOCKS = OFF, DATA_COMPRESSION = ROW)`,
		`CREATE INDEX IX_Fidelity_Stamp ON dbo.Fidelity (Stamp)`,
		`ALTER INDEX IX_Fidelity_Stamp ON dbo.Fidelity DISABLE`,
		`ALTER TABLE dbo.Fidelity WITH NOCHECK ADD CONSTRAINT CK_Fidelity_Off CHECK (ID > 0)`,
		`ALTER TABLE dbo.Fidelity NOCHECK CONSTRAINT CK_Fidelity_Off`,
		`ALTER TABLE dbo.Fidelity ADD CONSTRAINT CK_Fidelity_On CHECK (Stamp > '2000-01-01')`,
		`ALTER TABLE dbo.Fidelity WITH NOCHECK ADD CONSTRAINT CK_Fidelity_Untrusted CHECK (ID <> 5)`,
		`ALTER TABLE dbo.Fidelity ADD CONSTRAINT CK_Fidelity_Nfr CHECK NOT FOR REPLICATION (ID <> 6)`,
		`CREATE STATISTICS st_Fidelity_Filtered ON dbo.Fidelity (Stamp, ID) WHERE Stamp > '2010-01-01' WITH NORECOMPUTE`,
		// Sparse columns cannot share a table with data compression.
		`CREATE TABLE dbo.Sparse (ID int NOT NULL, Rare int SPARSE NULL, Props xml COLUMN_SET FOR ALL_SPARSE_COLUMNS)`,
		`CREATE TABLE dbo.Heap (a int NULL) WITH (DATA_COMPRESSION = ROW)`,
		`CREATE TABLE dbo.Parted (ID int NOT NULL, Yr int NOT NULL) ON ps_fid(Yr)`,
		`CREATE STATISTICS st_Parted_Inc ON dbo.Parted (ID) WITH INCREMENTAL = ON`,
		`CREATE TABLE dbo.CCI (a int NOT NULL, b int NULL)`,
		`CREATE CLUSTERED COLUMNSTORE INDEX CCI_CCI ON dbo.CCI WITH (COMPRESSION_DELAY = 30 MINUTES)`,
		`CREATE TABLE dbo.NCCI (a int NOT NULL, b int NULL)`,
		`CREATE NONCLUSTERED COLUMNSTORE INDEX NCCI_NCCI ON dbo.NCCI (a, b)
		 WITH (DATA_COMPRESSION = COLUMNSTORE_ARCHIVE, COMPRESSION_DELAY = 15)`,
		// G4: every XML index form and spatial tessellation scheme on one
		// table. They were skipped with a comment, so the copy had none.
		`CREATE TABLE dbo.XmlGeo (ID int NOT NULL CONSTRAINT PK_XmlGeo PRIMARY KEY CLUSTERED,
		    Doc xml NULL, G geometry NULL, Gg geography NULL)`,
		`CREATE PRIMARY XML INDEX PX_XmlGeo ON dbo.XmlGeo (Doc) WITH (FILLFACTOR = 90)`,
		`CREATE XML INDEX SX_XmlGeo_Path ON dbo.XmlGeo (Doc) USING XML INDEX PX_XmlGeo FOR PATH`,
		`CREATE XML INDEX SX_XmlGeo_Value ON dbo.XmlGeo (Doc) USING XML INDEX PX_XmlGeo FOR VALUE`,
		`CREATE XML INDEX SX_XmlGeo_Prop ON dbo.XmlGeo (Doc) USING XML INDEX PX_XmlGeo FOR PROPERTY`,
		`CREATE SPATIAL INDEX SP_XmlGeo_G ON dbo.XmlGeo (G) USING GEOMETRY_GRID
		 WITH (BOUNDING_BOX = (-1.5, 0, 500, 200), GRIDS = (LEVEL_1 = LOW, LEVEL_2 = MEDIUM, LEVEL_3 = HIGH, LEVEL_4 = LOW),
		       CELLS_PER_OBJECT = 64, DATA_COMPRESSION = PAGE, ALLOW_ROW_LOCKS = OFF)`,
		`CREATE SPATIAL INDEX SP_XmlGeo_GA ON dbo.XmlGeo (G) USING GEOMETRY_AUTO_GRID
		 WITH (BOUNDING_BOX = (0, 0, 1000, 1000))`,
		`CREATE SPATIAL INDEX SP_XmlGeo_Gg ON dbo.XmlGeo (Gg) USING GEOGRAPHY_GRID
		 WITH (GRIDS = (HIGH, HIGH, MEDIUM, LOW), CELLS_PER_OBJECT = 32)`,
		`CREATE SPATIAL INDEX SP_XmlGeo_GgA ON dbo.XmlGeo (Gg) USING GEOGRAPHY_AUTO_GRID`,
	)

	opts := DefaultScriptOptions()
	opts.IncludeHeaders = false
	sc := NewScripter(src, opts)

	tables := []string{"Fidelity", "Sparse", "Heap", "Parted", "CCI", "NCCI", "XmlGeo"}
	for _, tbl := range tables {
		script, err := sc.ScriptTable(ctx, "dbo", tbl)
		if err != nil {
			t.Fatalf("ScriptTable(%s): %v", tbl, err)
		}
		livePinnedRun(t, db, ctx, dst.Name, script)
	}
	for _, st := range [][2]string{{"Fidelity", "st_Fidelity_Filtered"}, {"Parted", "st_Parted_Inc"}} {
		script, err := sc.ScriptStatistic(ctx, "dbo", st[0], st[1])
		if err != nil {
			t.Fatalf("ScriptStatistic(%s): %v", st[1], err)
		}
		livePinnedRun(t, db, ctx, dst.Name, script)
	}

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
		compare(tbl+" columns", liveFidelityColumns, "dbo."+tbl)
		compare(tbl+" indexes", liveFidelityIndexes, "dbo."+tbl)
		compare(tbl+" table options", liveFidelityTable, "dbo."+tbl)
	}
	compare("XmlGeo XML indexes", liveFidelityXMLIndexes, "dbo.XmlGeo")
	compare("XmlGeo spatial indexes", liveFidelitySpatial, "dbo.XmlGeo")
	compare("Fidelity check constraints", liveFidelityChecks, "dbo.Fidelity")
	compare("filtered statistic", liveFidelityStats, "dbo.Fidelity", "st_Fidelity_Filtered")
	compare("incremental statistic", liveFidelityStats, "dbo.Parted", "st_Parted_Inc")

	// DROP And CREATE of the temporal table must itself run: DROP TABLE is
	// refused on a system-versioned table until versioning is off.
	dc := opts
	dc.Verb = ScriptDropAndCreate
	script, err := NewScripter(dst, dc).ScriptTable(ctx, "dbo", "Fidelity")
	if err != nil {
		t.Fatalf("ScriptTable(drop and create): %v", err)
	}
	livePinnedRun(t, db, ctx, dst.Name, script)
	compare("Fidelity columns after DROP And CREATE", liveFidelityColumns, "dbo.Fidelity")
}

func TestLiveScriptFidelityModuleSetOptions(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()

	src, dropSrc := liveScratchDB(t, db, ctx, "gosmo_modset_src")
	defer dropSrc()
	dst, dropDst := liveScratchDB(t, db, ctx, "gosmo_modset_dst")
	defer dropDst()

	// Created under both options OFF: "literal" is a string only because
	// QUOTED_IDENTIFIER is off, and NULL = NULL is true only because
	// ANSI_NULLS is. A nested comment opens it, which Script as ALTER once
	// failed to see past.
	livePinnedRun(t, db, ctx, src.Name, "SET ANSI_NULLS OFF;\nGO\nSET QUOTED_IDENTIFIER OFF;\nGO\n"+
		"/* legacy /* nested */ header */\nCREATE PROCEDURE dbo.Legacy AS SELECT \"literal\" AS x WHERE NULL = NULL\nGO\n")

	sc := NewScripter(src, DefaultScriptOptions())
	script, err := sc.ScriptStoredProcedure(ctx, "dbo", "Legacy")
	if err != nil {
		t.Fatalf("ScriptStoredProcedure: %v", err)
	}
	livePinnedRun(t, db, ctx, dst.Name, script)

	checkModule := func(d *Database) {
		t.Helper()
		if got := liveRowsAsStrings(t, d, ctx, liveFidelityModule, "dbo.Legacy"); len(got) != 1 ||
			got[0] != "uses_ansi_nulls=false uses_quoted_identifier=false" {
			t.Errorf("dbo.Legacy in %s is compiled with %v, want both options OFF\n%s", d.Name, got, script)
		}
		if got := liveRowsAsStrings(t, d, ctx, "EXEC dbo.Legacy"); len(got) != 1 || got[0] != "x=literal" {
			t.Errorf("EXEC dbo.Legacy in %s returned %v, want the one row x=literal", d.Name, got)
		}
	}
	checkModule(dst)

	alter := DefaultScriptOptions()
	alter.Verb = ScriptAlter
	script, err = NewScripter(src, alter).ScriptStoredProcedure(ctx, "dbo", "Legacy")
	if err != nil {
		t.Fatalf("ScriptStoredProcedure(alter): %v", err)
	}
	if !strings.Contains(script, "*/\nALTER PROCEDURE") {
		t.Fatalf("Script as ALTER did not rewrite the CREATE behind a nested comment:\n%s", script)
	}
	livePinnedRun(t, db, ctx, src.Name, script)
	checkModule(src)
}
