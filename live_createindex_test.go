//go:build livedb

// Live verification of CreateIndex and CreateStatisticWithOptions against a
// real server. The unit tests pin the statement text; what only a server can
// answer is whether that text parses and produces the index that was asked
// for — clause order, which options each index family accepts, and whether a
// refusal gosmo makes is one SQL Server would have made too.
//
// Every case reads the catalog back rather than trusting the exec: an index
// that is created as the wrong type, or on the wrong data space, reports
// success just as loudly as one that isn't.
//
//	go test -tags livedb . -run TestLiveCreateIndex -v \
//	  -livedb 'sqlserver://sa:PASS@host?TrustServerCertificate=true'
//
// Creates and drops its own throwaway database; touches nothing else.
package gosmo

import (
	"context"
	"database/sql"
	"strings"
	"testing"
)

// liveIndexFacts is what the catalog says about one created index.
type liveIndexFacts struct {
	TypeDesc  string
	IsUnique  bool
	Filter    string
	DataSpace string
	SpaceType string
}

func liveIndexOf(t *testing.T, d *Database, ctx context.Context, table, index string) liveIndexFacts {
	t.Helper()
	const q = `
SELECT i.type_desc, i.is_unique, ISNULL(i.filter_definition, ''), ISNULL(ds.name, ''), ISNULL(ds.type, '')
FROM   sys.indexes i
LEFT   JOIN sys.data_spaces ds ON ds.data_space_id = i.data_space_id
WHERE  i.object_id = OBJECT_ID(@p1) AND i.name = @p2`
	var f liveIndexFacts
	if err := d.queryRow(ctx, func(row *sql.Row) error {
		return row.Scan(&f.TypeDesc, &f.IsUnique, &f.Filter, &f.DataSpace, &f.SpaceType)
	}, q, "dbo."+table, index); err != nil {
		t.Fatalf("catalog read of index %s on %s: %v", index, table, err)
	}
	f.SpaceType = strings.TrimSpace(f.SpaceType)
	return f
}

func liveScalar(t *testing.T, d *Database, ctx context.Context, q string, args ...any) string {
	t.Helper()
	var v string
	if err := d.queryRow(ctx, func(row *sql.Row) error { return row.Scan(&v) }, q, args...); err != nil {
		t.Fatalf("query %.60q: %v", q, err)
	}
	return v
}

func TestLiveCreateIndexEveryType(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()

	d, drop := liveScratchDB(t, db, ctx, "gosmo_idxlive")
	defer drop()

	liveExecIn(t, d, ctx,
		// XML and spatial indexes both require a clustered primary key on the
		// table they index.
		`CREATE TABLE dbo.Docs (
		    ID INT NOT NULL CONSTRAINT PK_Docs PRIMARY KEY CLUSTERED,
		    Name NVARCHAR(50) NULL,
		    Note NVARCHAR(100) NULL,
		    Amount INT NULL,
		    Doc XML NULL,
		    GeomShape GEOMETRY NULL,
		    GeogShape GEOGRAPHY NULL)`,
		`CREATE TABLE dbo.Facts (a INT NOT NULL, b INT NULL)`,
		`CREATE TABLE dbo.Cold (a INT NOT NULL, b NVARCHAR(20) NULL)`,
		`CREATE TABLE dbo.Heap (a INT NOT NULL, b INT NULL)`,
		`CREATE PARTITION FUNCTION pf_year (INT) AS RANGE RIGHT FOR VALUES (2000, 2010)`,
		`CREATE PARTITION SCHEME ps_year AS PARTITION pf_year ALL TO ([PRIMARY])`,
		`CREATE TABLE dbo.Parted (ID INT NOT NULL, Yr INT NOT NULL, Note NVARCHAR(40) NULL) ON ps_year(Yr)`,
	)

	docs, err := d.TableByNameContext(ctx, "dbo", "Docs")
	if err != nil {
		t.Fatalf("TableByNameContext Docs: %v", err)
	}
	tableOf := func(name string) *Table {
		tbl, err := d.TableByNameContext(ctx, "dbo", name)
		if err != nil {
			t.Fatalf("TableByNameContext %s: %v", name, err)
		}
		return tbl
	}

	// The rowstore case carries every shared option at once: if any of them is
	// in the wrong place in the clause order, the statement does not parse.
	t.Run("rowstore nonclustered", func(t *testing.T) {
		if err := docs.CreateIndexContext(ctx, CreateIndexRequest{
			Name:             "IX_Docs_Name",
			Type:             IndexTypeNonClustered,
			IsUnique:         true,
			KeyColumns:       []IndexColumnDef{{Name: "Name"}, {Name: "Amount", Descending: true}},
			IncludedColumns:  []string{"Note"},
			FilterDefinition: "[Name] IS NOT NULL",
			FillFactor:       80,
			PadIndex:         true,
			Online:           true,
			SortInTempDB:     true,
			DataCompression:  "PAGE",
			FileGroup:        "PRIMARY",
		}); err != nil {
			t.Fatalf("CreateIndex: %v", err)
		}
		f := liveIndexOf(t, d, ctx, "Docs", "IX_Docs_Name")
		if f.TypeDesc != "NONCLUSTERED" || !f.IsUnique {
			t.Errorf("index = %+v, want a unique NONCLUSTERED index", f)
		}
		if !strings.Contains(f.Filter, "Name") {
			t.Errorf("filter_definition = %q, want the Name predicate", f.Filter)
		}
		if f.DataSpace != "PRIMARY" || f.SpaceType != "FG" {
			t.Errorf("data space = %q/%q, want PRIMARY/FG", f.DataSpace, f.SpaceType)
		}
	})

	t.Run("rowstore clustered", func(t *testing.T) {
		if err := tableOf("Heap").CreateIndexContext(ctx, CreateIndexRequest{
			Name:       "CIX_Heap",
			Type:       IndexTypeClustered,
			KeyColumns: []IndexColumnDef{{Name: "a"}},
		}); err != nil {
			t.Fatalf("CreateIndex: %v", err)
		}
		if f := liveIndexOf(t, d, ctx, "Heap", "CIX_Heap"); f.TypeDesc != "CLUSTERED" {
			t.Errorf("type_desc = %q, want CLUSTERED", f.TypeDesc)
		}
	})

	t.Run("nonclustered columnstore", func(t *testing.T) {
		if err := tableOf("Facts").CreateIndexContext(ctx, CreateIndexRequest{
			Name:             "NCCI_Facts",
			Type:             IndexTypeColumnStore,
			KeyColumns:       []IndexColumnDef{{Name: "a"}, {Name: "b"}},
			FilterDefinition: "[a] > 0",
			DataCompression:  "COLUMNSTORE_ARCHIVE",
			CompressionDelay: 60,
		}); err != nil {
			t.Fatalf("CreateIndex: %v", err)
		}
		f := liveIndexOf(t, d, ctx, "Facts", "NCCI_Facts")
		if f.TypeDesc != "NONCLUSTERED COLUMNSTORE" {
			t.Errorf("type_desc = %q, want NONCLUSTERED COLUMNSTORE", f.TypeDesc)
		}
		if !strings.Contains(f.Filter, "a") {
			t.Errorf("filter_definition = %q, want the a > 0 predicate", f.Filter)
		}
		delay := liveScalar(t, d, ctx,
			`SELECT CAST(compression_delay AS NVARCHAR(10)) FROM sys.indexes
			 WHERE object_id = OBJECT_ID('dbo.Facts') AND name = 'NCCI_Facts'`)
		if delay != "60" {
			t.Errorf("compression_delay = %q, want 60", delay)
		}
	})

	t.Run("clustered columnstore", func(t *testing.T) {
		if err := tableOf("Cold").CreateIndexContext(ctx, CreateIndexRequest{
			Name: "CCI_Cold",
			Type: IndexTypeClusteredColumnStore,
		}); err != nil {
			t.Fatalf("CreateIndex: %v", err)
		}
		if f := liveIndexOf(t, d, ctx, "Cold", "CCI_Cold"); f.TypeDesc != "CLUSTERED COLUMNSTORE" {
			t.Errorf("type_desc = %q, want CLUSTERED COLUMNSTORE", f.TypeDesc)
		}
	})

	t.Run("primary and secondary XML", func(t *testing.T) {
		if err := docs.CreateIndexContext(ctx, CreateIndexRequest{
			Name:         "PXML_Docs",
			Type:         IndexTypeXML,
			IsPrimaryXML: true,
			KeyColumns:   []IndexColumnDef{{Name: "Doc"}},
		}); err != nil {
			t.Fatalf("CreateIndex (primary): %v", err)
		}
		if err := docs.CreateIndexContext(ctx, CreateIndexRequest{
			Name:             "SXML_Docs_Path",
			Type:             IndexTypeXML,
			KeyColumns:       []IndexColumnDef{{Name: "Doc"}},
			PrimaryXMLIndex:  "PXML_Docs",
			SecondaryXMLType: XMLSecondaryPath,
		}); err != nil {
			t.Fatalf("CreateIndex (secondary): %v", err)
		}
		got := liveScalar(t, d, ctx,
			`SELECT ISNULL(secondary_type_desc, 'PRIMARY') FROM sys.xml_indexes
			 WHERE object_id = OBJECT_ID('dbo.Docs') AND name = @p1`, "SXML_Docs_Path")
		if got != "PATH" {
			t.Errorf("secondary_type_desc = %q, want PATH", got)
		}
	})

	t.Run("spatial geometry grid", func(t *testing.T) {
		if err := docs.CreateIndexContext(ctx, CreateIndexRequest{
			Name:           "SI_Docs_Geom",
			Type:           IndexTypeSpatial,
			KeyColumns:     []IndexColumnDef{{Name: "GeomShape"}},
			Tessellation:   SpatialGeometryGrid,
			BoundingBox:    &SpatialBoundingBox{XMin: -180.5, YMin: -90, XMax: 180.5, YMax: 90},
			GridLevels:     SpatialGridLevels{Level1: SpatialGridLow, Level2: SpatialGridMedium, Level3: SpatialGridHigh, Level4: SpatialGridHigh},
			CellsPerObject: 16,
			FillFactor:     90,
		}); err != nil {
			t.Fatalf("CreateIndex: %v", err)
		}
		got := liveScalar(t, d, ctx,
			`SELECT tessellation_scheme FROM sys.spatial_indexes
			 WHERE object_id = OBJECT_ID('dbo.Docs') AND name = @p1`, "SI_Docs_Geom")
		if got != "GEOMETRY_GRID" {
			t.Errorf("tessellation_scheme = %q, want GEOMETRY_GRID", got)
		}
		cells := liveScalar(t, d, ctx,
			`SELECT CAST(cells_per_object AS NVARCHAR(10)) FROM sys.spatial_index_tessellations
			 WHERE object_id = OBJECT_ID('dbo.Docs')
			   AND index_id = (SELECT index_id FROM sys.indexes WHERE object_id = OBJECT_ID('dbo.Docs') AND name = @p1)`,
			"SI_Docs_Geom")
		if cells != "16" {
			t.Errorf("cells_per_object = %q, want 16", cells)
		}
	})

	t.Run("spatial geography auto grid", func(t *testing.T) {
		if err := docs.CreateIndexContext(ctx, CreateIndexRequest{
			Name:           "SI_Docs_Geog",
			Type:           IndexTypeSpatial,
			KeyColumns:     []IndexColumnDef{{Name: "GeogShape"}},
			Tessellation:   SpatialGeographyAutoGrid,
			CellsPerObject: 12,
		}); err != nil {
			t.Fatalf("CreateIndex: %v", err)
		}
		got := liveScalar(t, d, ctx,
			`SELECT tessellation_scheme FROM sys.spatial_indexes
			 WHERE object_id = OBJECT_ID('dbo.Docs') AND name = @p1`, "SI_Docs_Geog")
		if got != "GEOGRAPHY_AUTO_GRID" {
			t.Errorf("tessellation_scheme = %q, want GEOGRAPHY_AUTO_GRID", got)
		}
	})

	// The partition-scheme ON clause is the one that has to come last and
	// carry its partitioning column with it.
	t.Run("on a partition scheme", func(t *testing.T) {
		if err := tableOf("Parted").CreateIndexContext(ctx, CreateIndexRequest{
			Name:             "IX_Parted_Note",
			Type:             IndexTypeNonClustered,
			KeyColumns:       []IndexColumnDef{{Name: "Note"}},
			PartitionScheme:  "ps_year",
			PartitionColumns: []string{"Yr"},
		}); err != nil {
			t.Fatalf("CreateIndex: %v", err)
		}
		f := liveIndexOf(t, d, ctx, "Parted", "IX_Parted_Note")
		if f.DataSpace != "ps_year" || f.SpaceType != "PS" {
			t.Errorf("data space = %q/%q, want ps_year/PS", f.DataSpace, f.SpaceType)
		}
	})

	// DROP_EXISTING recreates an index that is already there; it needs one to
	// exist, so it runs after the rowstore case above.
	t.Run("drop existing", func(t *testing.T) {
		if err := docs.CreateIndexContext(ctx, CreateIndexRequest{
			Name:         "IX_Docs_Name",
			Type:         IndexTypeNonClustered,
			KeyColumns:   []IndexColumnDef{{Name: "Note"}},
			DropExisting: true,
		}); err != nil {
			t.Fatalf("CreateIndex: %v", err)
		}
		f := liveIndexOf(t, d, ctx, "Docs", "IX_Docs_Name")
		if f.IsUnique || f.Filter != "" {
			t.Errorf("index = %+v, want the recreated non-unique unfiltered form", f)
		}
	})
}

// The refusals gosmo makes must be refusals SQL Server would have made too —
// otherwise validate() is turning a legal statement into an error. Each case
// is executed raw against the server to confirm the server rejects it as
// well.
func TestLiveCreateIndexRefusalsMatchTheServer(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()

	d, drop := liveScratchDB(t, db, ctx, "gosmo_idxrefuse")
	defer drop()
	liveExecIn(t, d, ctx, `CREATE TABLE dbo.T (a INT NOT NULL, b INT NULL)`)

	for _, c := range []struct {
		name string
		stmt string
	}{
		{"unique columnstore", "CREATE UNIQUE NONCLUSTERED COLUMNSTORE INDEX ix ON dbo.T (a)"},
		{"include on a clustered index", "CREATE CLUSTERED INDEX ix ON dbo.T (a) INCLUDE (b)"},
		{"ordered columnstore column", "CREATE NONCLUSTERED COLUMNSTORE INDEX ix ON dbo.T (a DESC)"},
		{"fill factor on a columnstore index", "CREATE NONCLUSTERED COLUMNSTORE INDEX ix ON dbo.T (a) WITH (FILLFACTOR = 80)"},
		{"key columns on a clustered columnstore index", "CREATE CLUSTERED COLUMNSTORE INDEX ix ON dbo.T (a)"},
		{"rowstore compression on a columnstore index", "CREATE NONCLUSTERED COLUMNSTORE INDEX ix ON dbo.T (a) WITH (DATA_COMPRESSION = PAGE)"},
		{"filtered clustered index", "CREATE CLUSTERED INDEX ix ON dbo.T (a) WHERE a > 0"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if _, err := d.exec(ctx, c.stmt); err == nil {
				t.Errorf("the server accepted %q — gosmo refuses it, so the refusal is wrong", c.stmt)
			}
		})
	}
}

func TestLiveCreateStatisticWithOptions(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()

	d, drop := liveScratchDB(t, db, ctx, "gosmo_statlive")
	defer drop()
	liveExecIn(t, d, ctx,
		`CREATE TABLE dbo.S (a INT NOT NULL, b NVARCHAR(40) NULL)`,
		`INSERT INTO dbo.S (a, b) VALUES (1, N'x'), (2, N'y'), (3, NULL)`,
		`CREATE PARTITION FUNCTION pf_a (INT) AS RANGE RIGHT FOR VALUES (2, 4)`,
		`CREATE PARTITION SCHEME ps_a AS PARTITION pf_a ALL TO ([PRIMARY])`,
		`CREATE TABLE dbo.P (a INT NOT NULL, b INT NULL) ON ps_a(a)`,
		`CREATE CLUSTERED INDEX CIX_P ON dbo.P (a) ON ps_a(a)`,
	)

	tbl, err := d.TableByNameContext(ctx, "dbo", "S")
	if err != nil {
		t.Fatalf("TableByNameContext: %v", err)
	}
	if err := tbl.CreateStatisticWithOptionsContext(ctx, CreateStatisticRequest{
		Name:             "ST_S_ab",
		Columns:          []string{"a", "b"},
		FullScan:         true,
		FilterDefinition: "[b] IS NOT NULL",
		NoRecompute:      true,
	}); err != nil {
		t.Fatalf("CreateStatisticWithOptions: %v", err)
	}
	facts := liveScalar(t, d, ctx,
		`SELECT CONCAT(CAST(s.has_filter AS INT), '/', CAST(s.no_recompute AS INT), '/',
		        (SELECT COUNT(*) FROM sys.stats_columns sc WHERE sc.object_id = s.object_id AND sc.stats_id = s.stats_id))
		 FROM sys.stats s WHERE s.object_id = OBJECT_ID('dbo.S') AND s.name = @p1`, "ST_S_ab")
	if facts != "1/1/2" {
		t.Errorf("has_filter/no_recompute/columns = %q, want 1/1/2", facts)
	}

	parted, err := d.TableByNameContext(ctx, "dbo", "P")
	if err != nil {
		t.Fatalf("TableByNameContext P: %v", err)
	}
	if err := parted.CreateStatisticWithOptionsContext(ctx, CreateStatisticRequest{
		Name:          "ST_P_b",
		Columns:       []string{"b"},
		SamplePercent: 50,
		Incremental:   true,
	}); err != nil {
		t.Fatalf("CreateStatisticWithOptions (incremental): %v", err)
	}
	if got := liveScalar(t, d, ctx,
		`SELECT CAST(is_incremental AS NVARCHAR(2)) FROM sys.stats
		 WHERE object_id = OBJECT_ID('dbo.P') AND name = @p1`, "ST_P_b"); got != "1" {
		t.Errorf("is_incremental = %q, want 1", got)
	}
}
