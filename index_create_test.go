package gosmo

import (
	"context"
	"strings"
	"testing"
)

// TestScriptCreateIndexWrites pins the statement each index type produces.
// See script_write_common_test.go for why the whole statement is compared
// and why every name carries a bracket, an apostrophe, or a dot.
func TestScriptCreateIndexWrites(t *testing.T) {
	table := func() *Table { return &Table{db: scriptTestDB(), Schema: "dbo", Name: "Sales.Archive"} }
	create := func(req CreateIndexRequest) func(context.Context) error {
		return func(c context.Context) error { return table().CreateIndexContext(c, req) }
	}

	runScriptCases(t, []scriptCase{
		{"rowstore nonclustered, every option", create(CreateIndexRequest{
			Name:             "IX_A]B",
			Type:             IndexTypeNonClustered,
			IsUnique:         true,
			KeyColumns:       []IndexColumnDef{{Name: "a]b"}, {Name: "c'd", Descending: true}},
			IncludedColumns:  []string{"e]f"},
			FilterDefinition: "[a]]b] > 0",
			FillFactor:       80,
			PadIndex:         true,
			Online:           true,
			SortInTempDB:     true,
			DropExisting:     true,
			DataCompression:  "PAGE",
			FileGroup:        "FG]1",
		}), scriptUsePrefix + "CREATE UNIQUE NONCLUSTERED INDEX [IX_A]]B] ON [dbo].[Sales.Archive] " +
			"([a]]b] ASC, [c'd] DESC) INCLUDE ([e]]f]) WHERE [a]]b] > 0 " +
			"WITH (PAD_INDEX = ON, FILLFACTOR = 80, ONLINE = ON, SORT_IN_TEMPDB = ON, DROP_EXISTING = ON, DATA_COMPRESSION = PAGE) " +
			"ON [FG]]1]"},

		{"rowstore clustered", create(CreateIndexRequest{
			Name:       "CIX",
			Type:       IndexTypeClustered,
			KeyColumns: []IndexColumnDef{{Name: "a"}},
		}), scriptUsePrefix + "CREATE CLUSTERED INDEX [CIX] ON [dbo].[Sales.Archive] ([a] ASC)"},

		// The zero Type is a nonclustered rowstore index, the form callers
		// written before the type existed pass.
		{"rowstore, no type given", create(CreateIndexRequest{
			Name:       "IX",
			KeyColumns: []IndexColumnDef{{Name: "a"}},
		}), scriptUsePrefix + "CREATE NONCLUSTERED INDEX [IX] ON [dbo].[Sales.Archive] ([a] ASC)"},

		{"nonclustered columnstore, filtered and partitioned", create(CreateIndexRequest{
			Name:             "nc]ci",
			Type:             IndexTypeColumnStore,
			KeyColumns:       []IndexColumnDef{{Name: "a]b"}, {Name: "c'd"}},
			FilterDefinition: "[x] > 0",
			DataCompression:  "COLUMNSTORE_ARCHIVE",
			CompressionDelay: 60,
			PartitionScheme:  "PS]1",
			PartitionColumns: []string{"a]b"},
		}), scriptUsePrefix + "CREATE NONCLUSTERED COLUMNSTORE INDEX [nc]]ci] ON [dbo].[Sales.Archive] ([a]]b], [c'd]) " +
			"WHERE [x] > 0 WITH (DATA_COMPRESSION = COLUMNSTORE_ARCHIVE, COMPRESSION_DELAY = 60 MINUTES) ON [PS]]1] ([a]]b])"},

		{"clustered columnstore", create(CreateIndexRequest{
			Name:         "cci",
			Type:         IndexTypeClusteredColumnStore,
			DropExisting: true,
		}), scriptUsePrefix + "CREATE CLUSTERED COLUMNSTORE INDEX [cci] ON [dbo].[Sales.Archive] WITH (DROP_EXISTING = ON)"},

		{"primary XML", create(CreateIndexRequest{
			Name:         "PXML]1",
			Type:         IndexTypeXML,
			IsPrimaryXML: true,
			KeyColumns:   []IndexColumnDef{{Name: "doc]x"}},
		}), scriptUsePrefix + "CREATE PRIMARY XML INDEX [PXML]]1] ON [dbo].[Sales.Archive] ([doc]]x])"},

		{"secondary XML", create(CreateIndexRequest{
			Name:             "SX",
			Type:             IndexTypeXML,
			KeyColumns:       []IndexColumnDef{{Name: "doc"}},
			PrimaryXMLIndex:  "PXML]1",
			SecondaryXMLType: XMLSecondaryPath,
		}), scriptUsePrefix + "CREATE XML INDEX [SX] ON [dbo].[Sales.Archive] ([doc]) USING XML INDEX [PXML]]1] FOR PATH"},

		{"spatial geometry grid", create(CreateIndexRequest{
			Name:           "SI",
			Type:           IndexTypeSpatial,
			KeyColumns:     []IndexColumnDef{{Name: "geo"}},
			Tessellation:   SpatialGeometryGrid,
			BoundingBox:    &SpatialBoundingBox{XMin: -180.5, YMin: -90, XMax: 180.5, YMax: 90},
			GridLevels:     SpatialGridLevels{Level1: SpatialGridLow, Level2: SpatialGridMedium, Level3: SpatialGridHigh, Level4: SpatialGridHigh},
			CellsPerObject: 16,
			FillFactor:     90,
		}), scriptUsePrefix + "CREATE SPATIAL INDEX [SI] ON [dbo].[Sales.Archive] ([geo]) USING GEOMETRY_GRID " +
			"WITH (BOUNDING_BOX = (-180.5, -90, 180.5, 90), GRIDS = (LEVEL_1 = LOW, LEVEL_2 = MEDIUM, LEVEL_3 = HIGH, LEVEL_4 = HIGH), " +
			"CELLS_PER_OBJECT = 16, FILLFACTOR = 90)"},

		{"spatial geography auto grid", create(CreateIndexRequest{
			Name:           "SI",
			Type:           IndexTypeSpatial,
			KeyColumns:     []IndexColumnDef{{Name: "geo"}},
			Tessellation:   SpatialGeographyAutoGrid,
			CellsPerObject: 12,
		}), scriptUsePrefix + "CREATE SPATIAL INDEX [SI] ON [dbo].[Sales.Archive] ([geo]) USING GEOGRAPHY_AUTO_GRID WITH (CELLS_PER_OBJECT = 12)"},
	})
}

// TestCreateIndexRefusesAWrongCombination covers the requests that cannot
// become a statement. Each is a combination SQL Server itself rejects; the
// point of refusing here is that nothing is executed, so a request built by
// a form with the wrong page showing never reaches the server at all.
func TestCreateIndexRefusesAWrongCombination(t *testing.T) {
	keyA := []IndexColumnDef{{Name: "a"}}
	for _, c := range []struct {
		name string
		req  CreateIndexRequest
		want string
	}{
		{"no name", CreateIndexRequest{KeyColumns: keyA}, "name is required"},
		{"no key columns", CreateIndexRequest{Name: "ix"}, "at least one key column"},
		{"unknown type", CreateIndexRequest{Name: "ix", Type: IndexType("HASH"), KeyColumns: keyA}, "cannot be created here"},
		{"unique columnstore", CreateIndexRequest{Name: "ix", Type: IndexTypeColumnStore, IsUnique: true, KeyColumns: keyA},
			"only a rowstore index can be unique"},
		{"include on a clustered index", CreateIndexRequest{Name: "ix", Type: IndexTypeClustered, KeyColumns: keyA, IncludedColumns: []string{"b"}},
			"only a nonclustered rowstore index has included columns"},
		{"filtered clustered index", CreateIndexRequest{Name: "ix", Type: IndexTypeClustered, KeyColumns: keyA, FilterDefinition: "[a] > 0"},
			"can be filtered"},
		{"ordered columnstore column", CreateIndexRequest{Name: "ix", Type: IndexTypeColumnStore, KeyColumns: []IndexColumnDef{{Name: "a", Descending: true}}},
			"orders its key columns"},
		{"fill factor on a columnstore index", CreateIndexRequest{Name: "ix", Type: IndexTypeColumnStore, KeyColumns: keyA, FillFactor: 80},
			"no fill factor"},
		{"fill factor out of range", CreateIndexRequest{Name: "ix", KeyColumns: keyA, FillFactor: 101}, "out of range"},
		{"rowstore compression on a columnstore index", CreateIndexRequest{Name: "ix", Type: IndexTypeColumnStore, KeyColumns: keyA, DataCompression: "PAGE"},
			"rowstore setting"},
		{"columnstore compression on a rowstore index", CreateIndexRequest{Name: "ix", KeyColumns: keyA, DataCompression: "COLUMNSTORE"},
			"columnstore index only"},
		{"compression delay off a columnstore index", CreateIndexRequest{Name: "ix", KeyColumns: keyA, CompressionDelay: 60},
			"compression delay applies"},
		{"XML options off an XML index", CreateIndexRequest{Name: "ix", KeyColumns: keyA, IsPrimaryXML: true},
			"XML index only"},
		{"two XML columns", CreateIndexRequest{Name: "ix", Type: IndexTypeXML, IsPrimaryXML: true, KeyColumns: []IndexColumnDef{{Name: "a"}, {Name: "b"}}},
			"exactly one key column"},
		{"secondary XML with no primary", CreateIndexRequest{Name: "ix", Type: IndexTypeXML, KeyColumns: keyA, SecondaryXMLType: XMLSecondaryPath},
			"names the primary XML index"},
		{"secondary XML with no FOR clause", CreateIndexRequest{Name: "ix", Type: IndexTypeXML, KeyColumns: keyA, PrimaryXMLIndex: "px"},
			"PATH, VALUE, or PROPERTY"},
		{"primary XML built over another", CreateIndexRequest{Name: "ix", Type: IndexTypeXML, IsPrimaryXML: true, KeyColumns: keyA, PrimaryXMLIndex: "px"},
			"not built over another index"},
		{"spatial options off a spatial index", CreateIndexRequest{Name: "ix", KeyColumns: keyA, CellsPerObject: 8},
			"spatial index only"},
		{"spatial with no tessellation", CreateIndexRequest{Name: "ix", Type: IndexTypeSpatial, KeyColumns: keyA},
			"tessellation scheme"},
		{"geometry with no bounding box", CreateIndexRequest{Name: "ix", Type: IndexTypeSpatial, KeyColumns: keyA, Tessellation: SpatialGeometryGrid},
			"requires a bounding box"},
		{"geography with a bounding box", CreateIndexRequest{Name: "ix", Type: IndexTypeSpatial, KeyColumns: keyA, Tessellation: SpatialGeographyGrid,
			BoundingBox: &SpatialBoundingBox{XMax: 1, YMax: 1}}, "whole globe"},
		{"empty bounding box", CreateIndexRequest{Name: "ix", Type: IndexTypeSpatial, KeyColumns: keyA, Tessellation: SpatialGeometryGrid,
			BoundingBox: &SpatialBoundingBox{}}, "bounding box is empty"},
		{"grids on an auto grid", CreateIndexRequest{Name: "ix", Type: IndexTypeSpatial, KeyColumns: keyA, Tessellation: SpatialGeographyAutoGrid,
			GridLevels: SpatialGridLevels{Level1: SpatialGridLow}}, "chooses its own grid densities"},
		{"unknown grid density", CreateIndexRequest{Name: "ix", Type: IndexTypeSpatial, KeyColumns: keyA, Tessellation: SpatialGeographyGrid,
			GridLevels: SpatialGridLevels{Level1: SpatialGridDensity("TINY")}}, "LOW, MEDIUM, or HIGH"},
		{"filegroup and partition scheme together", CreateIndexRequest{Name: "ix", KeyColumns: keyA, FileGroup: "fg",
			PartitionScheme: "ps", PartitionColumns: []string{"a"}}, "not both"},
		{"partition scheme with no column", CreateIndexRequest{Name: "ix", KeyColumns: keyA, PartitionScheme: "ps"},
			"go together"},
		{"filegroup on an XML index", CreateIndexRequest{Name: "ix", Type: IndexTypeXML, IsPrimaryXML: true, KeyColumns: keyA, FileGroup: "fg"},
			"takes no filegroup"},
	} {
		t.Run(c.name, func(t *testing.T) {
			tbl := captureTable(t)
			err := tbl.CreateIndex(c.req)
			if err == nil {
				t.Fatalf("CreateIndex(%s) returned nil, want an error", c.name)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error = %v, want it to mention %q", err, c.want)
			}
			if q := captured.find("CREATE"); q != "" {
				t.Errorf("refused but still executed: %s", q)
			}
		})
	}
}
