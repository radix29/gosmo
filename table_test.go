package gosmo

import (
	"context"
	"database/sql/driver"
	"slices"
	"testing"
)

func TestColTypeSQL(t *testing.T) {
	cases := []struct {
		name string
		col  ColumnDefinition
		want string
	}{
		{"varchar with length", ColumnDefinition{DataType: DataTypeVarChar, MaxLength: 50}, "varchar(50)"},
		{"varchar MAX", ColumnDefinition{DataType: DataTypeVarChar, MaxLength: -1}, "varchar(MAX)"},
		{"varchar no length", ColumnDefinition{DataType: DataTypeVarChar, MaxLength: 0}, "varchar"},
		{"nvarchar with length", ColumnDefinition{DataType: DataTypeNVarChar, MaxLength: 100}, "nvarchar(100)"},
		{"decimal with precision", ColumnDefinition{DataType: DataTypeDecimal, Precision: new(18), Scale: new(2)}, "decimal(18,2)"},
		{"decimal no precision", ColumnDefinition{DataType: DataTypeDecimal}, "decimal"},
		{"datetime2 with scale", ColumnDefinition{DataType: DataTypeDatetime2, Scale: new(3)}, "datetime2(3)"},
		{"datetime2 no scale", ColumnDefinition{DataType: DataTypeDatetime2}, "datetime2"},
		{"plain int", ColumnDefinition{DataType: DataTypeInt}, "int"},
		{"bit", ColumnDefinition{DataType: DataTypeBit}, "bit"},
		{"varbinary with length", ColumnDefinition{DataType: DataTypeVarBinary, MaxLength: 16}, "varbinary(16)"},
		// Zero is a scale, not "unspecified": each of these became the
		// 7-digit or (18,0) default while Scale and Precision were ints.
		{"datetime2(0)", ColumnDefinition{DataType: DataTypeDatetime2, Scale: new(0)}, "datetime2(0)"},
		{"time(0)", ColumnDefinition{DataType: DataTypeTime, Scale: new(0)}, "time(0)"},
		{"datetimeoffset(0)", ColumnDefinition{DataType: DataTypeDatetimeOffset, Scale: new(0)}, "datetimeoffset(0)"},
		{"decimal(p,0)", ColumnDefinition{DataType: DataTypeDecimal, Precision: new(10), Scale: new(0)}, "decimal(10,0)"},
		{"decimal precision only", ColumnDefinition{DataType: DataTypeNumeric, Precision: new(10)}, "numeric(10)"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := colTypeSQL(c.col); got != c.want {
				t.Errorf("colTypeSQL(%+v) = %q, want %q", c.col, got, c.want)
			}
		})
	}
}

// A precision or scale colTypeSQL has no spelling for is refused, never
// rendered as something else.
func TestCheckColumnDefinitionRefusesWhatCannotBeSaid(t *testing.T) {
	for _, col := range []ColumnDefinition{
		{Name: "a", DataType: DataTypeDecimal, Scale: new(2)},
		{Name: "b", DataType: DataTypeDatetime2, Precision: new(3)},
		{Name: "c", DataType: DataTypeInt, Scale: new(0)},
	} {
		if err := checkColumnDefinition(col); err == nil {
			t.Errorf("checkColumnDefinition(%s %s) = nil, want an error", col.Name, col.DataType)
		}
	}
	if err := checkColumnDefinition(ColumnDefinition{Name: "d", DataType: DataTypeDecimal, Precision: new(9), Scale: new(0)}); err != nil {
		t.Errorf("decimal(9,0) refused: %v", err)
	}
}

// TestAlterColumnRequiresName pins the early-return validation that runs
// before AlterColumn ever touches t.db — the only part of the DDL flow
// testable without a live server.
func TestAlterColumnRequiresName(t *testing.T) {
	tbl := &Table{Schema: "dbo", Name: "T"}
	if err := tbl.AlterColumn(t.Context(), ColumnDefinition{DataType: DataTypeInt}); err == nil {
		t.Error("AlterColumn with empty column name = nil error, want error")
	}
}

// Indexes costs two queries however many indexes the table has: one
// for the indexes, one for every index column on the object. Fetching each
// index's columns inside the loop over the indexes made it N+1, and
// Database.query pins its own pooled connection and issues its own USE, so
// the count below is round trips and connection acquisitions both.
//
// The grouping the single column query needs is what the rest asserts, since
// nothing else now separates one index's columns from another's: rows arrive
// ordered by index_id and land on the index that claims that id, keys and
// included columns split by is_included_column, each in its own arrival
// order. The heap row (index_id 0) is in the reply on purpose — the columns
// query has no index_id predicate, so the server really does return it, and
// it must reach no index at all.
func TestIndexesUsesOneQueryForEveryIndexColumn(t *testing.T) {
	tbl := captureTable(t)
	captured.reset(
		cannedRow{
			match: "FROM   sys.indexes i",
			cols: []string{"name", "index_id", "type_desc", "is_unique", "is_primary_key",
				"is_unique_constraint", "is_disabled", "fill_factor", "filter_definition",
				"is_padded", "ignore_dup_key", "allow_row_locks", "allow_page_locks",
				"data_compression_desc",
				"data_space", "is_partition_scheme", "is_default_filegroup", "partition_column",
				"no_recompute", "optimize_for_sequential_key", "bucket_count", "compression_delay",
				"is_primary_xml", "primary_xml_index", "secondary_type_desc", "is_selective_xml", "selective_xml_path",
				"tessellation_scheme",
				"xmin", "ymin", "xmax", "ymax", "level_1", "level_2", "level_3", "level_4", "cells_per_object"},
			rows: [][]driver.Value{
				withNoXMLOrSpatial("PK_T", int64(1), "CLUSTERED", true, true, false, false, int64(0), "", false, false, true, true, `[{"n":1,"c":"NONE"}]`, "PRIMARY", false, true, "", false, false, int64(0), int64(0)),
				withNoXMLOrSpatial("IX_covering", int64(2), "NONCLUSTERED", false, false, false, false, int64(0), "", false, false, true, true, `[{"n":1,"c":"PAGE"},{"n":2,"c":"NONE"}]`, "ps_year", true, false, "created", true, true, int64(0), int64(0)),
				withNoXMLOrSpatial("IX_empty", int64(3), "NONCLUSTERED", false, false, false, false, int64(0), "", false, false, true, true, `[{"n":1,"c":"NONE"}]`, "FG_archive", false, false, "", false, false, int64(0), int64(45)),
			},
		},
		cannedRow{
			match: "FROM   sys.index_columns ic",
			cols:  []string{"index_id", "name", "is_descending_key", "is_included_column", "column_store_order_ordinal"},
			rows: [][]driver.Value{
				{int64(0), "heap_col", false, false, int64(0)},
				{int64(1), "id", false, false, int64(0)},
				{int64(2), "a", true, false, int64(0)},
				{int64(2), "b", false, false, int64(0)},
				{int64(2), "note", false, true, int64(0)},
				{int64(2), "total", false, true, int64(0)},
			},
		},
	)

	indexes, err := tbl.Indexes(context.Background())
	if err != nil {
		t.Fatalf("Indexes: %v", err)
	}

	if n := captured.count("sys.index_columns ic"); n != 1 {
		t.Errorf("sys.index_columns queried %d times for 3 indexes, want 1", n)
	}
	if n := captured.count("sys.indexes i"); n != 1 {
		t.Errorf("sys.indexes queried %d times, want 1", n)
	}
	if n := captured.count("FROM   sys.selective_xml_index"); n != 0 {
		t.Errorf("selective XML paths/namespaces queried %d times with no selective index, want 0", n)
	}

	if len(indexes) != 3 {
		t.Fatalf("got %d indexes, want 3", len(indexes))
	}
	want := []struct {
		name string
		keys []IndexColumn
		incl []IndexColumn
	}{
		{"PK_T", []IndexColumn{{Name: "id"}}, nil},
		{"IX_covering",
			[]IndexColumn{{Name: "a", Descending: true}, {Name: "b"}},
			[]IndexColumn{{Name: "note", IsIncluded: true}, {Name: "total", IsIncluded: true}}},
		{"IX_empty", nil, nil},
	}
	for i, w := range want {
		got := indexes[i]
		if got.Name != w.name {
			t.Errorf("index %d name = %q, want %q", i, got.Name, w.name)
		}
		if !slices.Equal(got.KeyColumns, w.keys) {
			t.Errorf("%s key columns = %+v, want %+v", w.name, got.KeyColumns, w.keys)
		}
		if !slices.Equal(got.IncludedColumns, w.incl) {
			t.Errorf("%s included columns = %+v, want %+v", w.name, got.IncludedColumns, w.incl)
		}
		// Only IX_covering's row sets the two trailing options; a scan that
		// shifted them onto the wrong destinations flips one of these.
		if wantOn := w.name == "IX_covering"; got.StatisticsNoRecompute != wantOn || got.OptimizeForSequentialKey != wantOn {
			t.Errorf("%s NoRecompute/OptimizeForSequentialKey = %v/%v, want %v/%v",
				w.name, got.StatisticsNoRecompute, got.OptimizeForSequentialKey, wantOn, wantOn)
		}
		// The compression column is a per-partition JSON list; DataCompression
		// is its first entry. Only IX_covering's is partitioned and mixed.
		wantParts := []DataCompression{DataCompressionNone}
		if w.name == "IX_covering" {
			wantParts = []DataCompression{DataCompressionPage, DataCompressionNone}
		}
		if !slices.Equal(got.PartitionCompression, wantParts) || got.DataCompression != wantParts[0] {
			t.Errorf("%s compression = %q / %q, want %q / %q",
				w.name, got.DataCompression, got.PartitionCompression, wantParts[0], wantParts)
		}
		// compression_delay is the last column, and only IX_empty's is set.
		if want := map[bool]int{true: 45}[w.name == "IX_empty"]; got.CompressionDelay != want {
			t.Errorf("%s CompressionDelay = %d, want %d", w.name, got.CompressionDelay, want)
		}
	}
}

// A table with no indexes must not query for columns at all — there is
// nothing for the rows to be grouped onto, and the query is a round trip
// against every heap in an Object Explorer folder otherwise.
func TestIndexesSkipsTheColumnQueryWhenThereAreNoIndexes(t *testing.T) {
	tbl := captureTable(t)
	captured.reset()

	indexes, err := tbl.Indexes(context.Background())
	if err != nil {
		t.Fatalf("Indexes: %v", err)
	}
	if len(indexes) != 0 {
		t.Errorf("got %d indexes, want none", len(indexes))
	}
	if n := captured.count("sys.index_columns ic"); n != 0 {
		t.Errorf("sys.index_columns queried %d times for a table with no indexes, want 0", n)
	}
}

// TestIndexListReadsEachIndexDataSpace pins the ON clause onto the index it
// belongs to: an index can be on a different filegroup, or a different
// partition scheme, from the table and from every other index, so reading one
// data space for the whole object would be wrong on exactly the tables this
// exists for. The scheme's partitioning column comes with it — the clause is
// ON [scheme]([column]) and half of it is not a clause.
func TestIndexListReadsEachIndexDataSpace(t *testing.T) {
	tbl := captureTable(t)
	captured.reset(
		cannedRow{
			match: "FROM   sys.indexes i",
			cols: []string{"name", "index_id", "type_desc", "is_unique", "is_primary_key",
				"is_unique_constraint", "is_disabled", "fill_factor", "filter_definition",
				"is_padded", "ignore_dup_key", "allow_row_locks", "allow_page_locks",
				"data_compression_desc",
				"data_space", "is_partition_scheme", "is_default_filegroup", "partition_column",
				"no_recompute", "optimize_for_sequential_key", "bucket_count", "compression_delay",
				"is_primary_xml", "primary_xml_index", "secondary_type_desc", "is_selective_xml", "selective_xml_path",
				"tessellation_scheme",
				"xmin", "ymin", "xmax", "ymax", "level_1", "level_2", "level_3", "level_4", "cells_per_object"},
			rows: [][]driver.Value{
				withNoXMLOrSpatial("PK_T", int64(1), "CLUSTERED", true, true, false, false, int64(0), "", false, false, true, true, `[{"n":1,"c":"NONE"}]`, "ps_year", true, false, "Created", false, false, int64(0), int64(0)),
				withNoXMLOrSpatial("IX_archive", int64(2), "NONCLUSTERED", false, false, false, false, int64(0), "", false, false, true, true, `[{"n":1,"c":"NONE"}]`, "FG_Archive", false, false, "", false, false, int64(0), int64(0)),
				withNoXMLOrSpatial("IX_default", int64(3), "NONCLUSTERED", false, false, false, false, int64(0), "", false, false, true, true, `[{"n":1,"c":"NONE"}]`, "PRIMARY", false, true, "", false, false, int64(0), int64(0)),
			},
		},
		cannedRow{
			match: "FROM   sys.index_columns ic",
			cols:  []string{"index_id", "name", "is_descending_key", "is_included_column", "column_store_order_ordinal"},
			rows:  [][]driver.Value{{int64(1), "id", false, false, int64(0)}},
		},
	)

	indexes, err := tbl.Indexes(context.Background())
	if err != nil {
		t.Fatalf("Indexes: %v", err)
	}
	want := []DataSpace{
		{Name: "ps_year", IsPartitionScheme: true, PartitionColumn: "Created"},
		{Name: "FG_Archive"},
		{Name: "PRIMARY", IsDefaultFileGroup: true},
	}
	if len(indexes) != len(want) {
		t.Fatalf("got %d indexes, want %d", len(indexes), len(want))
	}
	for i, w := range want {
		if indexes[i].DataSpace != w {
			t.Errorf("%s data space = %+v, want %+v", indexes[i].Name, indexes[i].DataSpace, w)
		}
	}
}

// TestTableDataSpaceReadsTheHeapOrClusteredIndex pins the table's own ON
// clause to index_id 0 or 1. A heap has no row the index list would return —
// it filters on i.type > 0 — so a partitioned heap's scheme is only reachable
// through this query, and it is the ordinary case for a staging table.
func TestTableDataSpaceReadsTheHeapOrClusteredIndex(t *testing.T) {
	tbl := captureTable(t)
	captured.reset(cannedRow{
		match: "FROM   sys.indexes i",
		cols:  []string{"data_space", "is_partition_scheme", "is_default_filegroup", "partition_column"},
		row:   []driver.Value{"ps_year", true, false, "Created"},
	})

	ds, err := tbl.DataSpace(context.Background())
	if err != nil {
		t.Fatalf("DataSpace: %v", err)
	}
	want := DataSpace{Name: "ps_year", IsPartitionScheme: true, PartitionColumn: "Created"}
	if ds != want {
		t.Errorf("DataSpace = %+v, want %+v", ds, want)
	}
	if n := captured.count("i.index_id IN (0, 1)"); n != 1 {
		t.Errorf("index_id IN (0, 1) appeared in %d queries, want 1 — a heap is only reachable that way", n)
	}
}

// A table with no row in sys.indexes at all — a Database.TableRef handle, whose
// ObjectID is zero — must read as "no data space", not as an error: the
// scripter asks for it on every table and an error here would fail the whole
// script over a clause that has nothing to say.
func TestTableDataSpaceIsEmptyWhenThereIsNoRow(t *testing.T) {
	tbl := captureTable(t)
	captured.reset()

	ds, err := tbl.DataSpace(context.Background())
	if err != nil {
		t.Fatalf("DataSpace: %v", err)
	}
	if (ds != DataSpace{}) {
		t.Errorf("DataSpace = %+v, want the zero DataSpace", ds)
	}
}

// TestIndexListReadsXMLAndSpatialForms (G4): the index list carries an XML
// index's primary/secondary form and a spatial index's tessellation, which
// is what lets the scripter write them instead of a comment. A geography
// index has no bounding box, and its NULL coordinates must read as none.
func TestIndexListReadsXMLAndSpatialForms(t *testing.T) {
	tbl := captureTable(t)
	cols := []string{"name", "index_id", "type_desc", "is_unique", "is_primary_key",
		"is_unique_constraint", "is_disabled", "fill_factor", "filter_definition",
		"is_padded", "ignore_dup_key", "allow_row_locks", "allow_page_locks",
		"data_compression_desc",
		"data_space", "is_partition_scheme", "is_default_filegroup", "partition_column",
		"no_recompute", "optimize_for_sequential_key", "bucket_count", "compression_delay",
		"is_primary_xml", "primary_xml_index", "secondary_type_desc", "is_selective_xml", "selective_xml_path",
		"tessellation_scheme",
		"xmin", "ymin", "xmax", "ymax", "level_1", "level_2", "level_3", "level_4", "cells_per_object"}
	head := func(name string, id int64, typ string) []driver.Value {
		return []driver.Value{name, id, typ, false, false, false, false, int64(0), "", false, false, true, true,
			nil, "", false, false, "", false, false, int64(0), int64(0)}
	}
	captured.reset(
		cannedRow{
			match: "FROM   sys.indexes i",
			cols:  cols,
			rows: [][]driver.Value{
				append(head("PX", 256000, "XML"), true, "", "", false, "", "", nil, nil, nil, nil, "", "", "", "", int64(0)),
				append(head("SX", 256001, "XML"), false, "PX", "VALUE", false, "", "", nil, nil, nil, nil, "", "", "", "", int64(0)),
				append(head("SP_G", 384000, "SPATIAL"), false, "", "", false, "", "GEOMETRY_GRID",
					float64(-1.5), float64(0), float64(500), float64(200), "LOW", "MEDIUM", "HIGH", "LOW", int64(64)),
				append(head("SP_Gg", 384001, "SPATIAL"), false, "", "", false, "", "GEOGRAPHY_AUTO_GRID",
					nil, nil, nil, nil, "", "", "", "", int64(12)),
			},
		},
		cannedRow{
			match: "FROM   sys.index_columns ic",
			cols:  []string{"index_id", "name", "is_descending_key", "is_included_column", "column_store_order_ordinal"},
		},
	)
	indexes, err := tbl.Indexes(context.Background())
	if err != nil {
		t.Fatalf("Indexes: %v", err)
	}
	if len(indexes) != 4 {
		t.Fatalf("got %d indexes, want 4", len(indexes))
	}
	px, sx, g, gg := indexes[0], indexes[1], indexes[2], indexes[3]
	if !px.IsPrimaryXML || px.PrimaryXMLIndex != "" || px.SecondaryXMLType != "" {
		t.Errorf("primary XML read as %v/%q/%q", px.IsPrimaryXML, px.PrimaryXMLIndex, px.SecondaryXMLType)
	}
	if sx.IsPrimaryXML || sx.PrimaryXMLIndex != "PX" || sx.SecondaryXMLType != XMLSecondaryValue {
		t.Errorf("secondary XML read as %v/%q/%q", sx.IsPrimaryXML, sx.PrimaryXMLIndex, sx.SecondaryXMLType)
	}
	wantGrids := SpatialGridLevels{Level1: SpatialGridLow, Level2: SpatialGridMedium, Level3: SpatialGridHigh, Level4: SpatialGridLow}
	if g.Tessellation != SpatialGeometryGrid || g.BoundingBox == nil ||
		*g.BoundingBox != (SpatialBoundingBox{XMin: -1.5, YMin: 0, XMax: 500, YMax: 200}) ||
		g.GridLevels != wantGrids || g.CellsPerObject != 64 {
		t.Errorf("geometry index read as %q %+v %+v %d", g.Tessellation, g.BoundingBox, g.GridLevels, g.CellsPerObject)
	}
	if gg.Tessellation != SpatialGeographyAutoGrid || gg.BoundingBox != nil || gg.GridLevels != (SpatialGridLevels{}) || gg.CellsPerObject != 12 {
		t.Errorf("geography index read as %q %+v %+v %d", gg.Tessellation, gg.BoundingBox, gg.GridLevels, gg.CellsPerObject)
	}
}

// TestIndexListReadsSelectiveXML: a selective XML index is read with its
// paths and namespaces — fetched once for the table, and landing on the
// index whose id they carry — and a secondary selective one with the
// selective index and path it is built over. Before, both read as an XML
// index of neither form and ScriptTable left a comment in their place.
func TestIndexListReadsSelectiveXML(t *testing.T) {
	tbl := captureTable(t)
	cols := []string{"name", "index_id", "type_desc", "is_unique", "is_primary_key",
		"is_unique_constraint", "is_disabled", "fill_factor", "filter_definition",
		"is_padded", "ignore_dup_key", "allow_row_locks", "allow_page_locks",
		"data_compression_desc",
		"data_space", "is_partition_scheme", "is_default_filegroup", "partition_column",
		"no_recompute", "optimize_for_sequential_key", "bucket_count", "compression_delay",
		"is_primary_xml", "primary_xml_index", "secondary_type_desc", "is_selective_xml", "selective_xml_path",
		"tessellation_scheme",
		"xmin", "ymin", "xmax", "ymax", "level_1", "level_2", "level_3", "level_4", "cells_per_object"}
	head := func(name string, id int64) []driver.Value {
		return []driver.Value{name, id, "XML", false, false, false, false, int64(0), "", false, false, true, true,
			nil, "", false, false, "", false, false, int64(0), int64(0)}
	}
	spatial := []driver.Value{"", nil, nil, nil, nil, "", "", "", "", int64(0)}
	captured.reset(
		cannedRow{
			match: "FROM   sys.indexes i",
			cols:  cols,
			rows: [][]driver.Value{
				append(append(head("SXI", 256000), false, "", "", true, ""), spatial...),
				append(append(head("SXI_n", 256001), false, "SXI", "", false, "n"), spatial...),
			},
		},
		cannedRow{
			match: "FROM   sys.index_columns ic",
			cols:  []string{"index_id", "name", "is_descending_key", "is_included_column", "column_store_order_ordinal"},
		},
		cannedRow{
			match: "FROM   sys.selective_xml_index_paths p",
			cols: []string{"index_id", "name", "path", "is_sql", "type", "max_length", "precision", "scale",
				"xquery_type", "xquery_max_length", "is_node", "is_singleton"},
			rows: [][]driver.Value{
				{int64(256000), "item", "/a:item", false, "", int64(0), int64(0), int64(0), "", int64(0), true, false},
				{int64(256000), "n", "/a:n", true, "nvarchar", int64(60), int64(0), int64(0), "", int64(0), false, true},
				{int64(256000), "d", "/a:d", true, "decimal", int64(9), int64(10), int64(3), "", int64(0), false, false},
				{int64(256000), "id", "/a:item/@id", false, "", int64(0), int64(0), int64(0), "xs:string", int64(20), false, true},
			},
		},
		cannedRow{
			match: "FROM   sys.selective_xml_index_namespaces n",
			cols:  []string{"index_id", "prefix", "uri"},
			rows: [][]driver.Value{
				{int64(256000), "", "urn:d"},
				{int64(256000), "a", "urn:a"},
			},
		},
	)
	indexes, err := tbl.Indexes(context.Background())
	if err != nil {
		t.Fatalf("Indexes: %v", err)
	}
	if len(indexes) != 2 {
		t.Fatalf("got %d indexes, want 2", len(indexes))
	}
	sxi, sec := indexes[0], indexes[1]
	if !sxi.IsSelectiveXML || sxi.PrimaryXMLIndex != "" || sxi.SelectiveXMLPath != "" {
		t.Errorf("selective index read as %v/%q/%q", sxi.IsSelectiveXML, sxi.PrimaryXMLIndex, sxi.SelectiveXMLPath)
	}
	wantPaths := []SelectiveXMLPath{
		{Name: "item", Path: "/a:item", IsNode: true},
		{Name: "n", Path: "/a:n", IsSQL: true, SQLType: "nvarchar(30)", IsSingleton: true},
		{Name: "d", Path: "/a:d", IsSQL: true, SQLType: "decimal(10,3)"},
		{Name: "id", Path: "/a:item/@id", XQueryType: "xs:string", MaxLength: 20, IsSingleton: true},
	}
	if !slices.Equal(sxi.SelectiveXMLPaths, wantPaths) {
		t.Errorf("paths = %+v\nwant %+v", sxi.SelectiveXMLPaths, wantPaths)
	}
	wantNS := []XMLNamespace{{URI: "urn:d"}, {Prefix: "a", URI: "urn:a"}}
	if !slices.Equal(sxi.SelectiveXMLNamespaces, wantNS) {
		t.Errorf("namespaces = %+v, want %+v", sxi.SelectiveXMLNamespaces, wantNS)
	}
	if sec.IsSelectiveXML || sec.PrimaryXMLIndex != "SXI" || sec.SelectiveXMLPath != "n" ||
		sec.SecondaryXMLType != "" || sec.SelectiveXMLPaths != nil {
		t.Errorf("secondary selective index read as %+v", sec)
	}
	for _, q := range []string{"FROM   sys.selective_xml_index_paths p", "FROM   sys.selective_xml_index_namespaces n"} {
		if n := captured.count(q); n != 1 {
			t.Errorf("%q queried %d times, want 1", q, n)
		}
	}
}

// withNoXMLOrSpatial completes a canned index-list row with the trailing XML
// and spatial columns every other index type reads as zero.
func withNoXMLOrSpatial(vals ...driver.Value) []driver.Value {
	return append(vals, false, "", "", false, "", "", nil, nil, nil, nil, "", "", "", "", int64(0))
}

// TestXMLIndexesTellsSelectiveFromPrimary: a selective XML index and its
// secondary have no secondary_type_desc, and IsPrimary once derived from
// that alone read both as primary XML indexes — so a New Index dialog
// offered them as the parent of a PATH/VALUE/PROPERTY index, which the
// server refuses. IsPrimary now comes from xml_index_type.
func TestXMLIndexesTellsSelectiveFromPrimary(t *testing.T) {
	tbl := captureTable(t)
	captured.reset(cannedRow{
		match: "FROM   sys.xml_indexes xi",
		cols:  []string{"name", "index_id", "secondary_type_desc", "column", "primary", "is_primary", "is_selective"},
		rows: [][]driver.Value{
			{"PX", int64(256000), "", "x", "", true, false},
			{"SX", int64(256001), "PATH", "x", "PX", false, false},
			{"SXI", int64(256002), "", "y", "", false, true},
			{"SXI_n", int64(256003), "", "y", "SXI", false, true},
		},
	})
	got, err := tbl.XMLIndexes(context.Background())
	if err != nil {
		t.Fatalf("XMLIndexes: %v", err)
	}
	want := []XMLIndex{
		{Name: "PX", IndexID: 256000, IsPrimary: true, ColumnName: "x"},
		{Name: "SX", IndexID: 256001, SecondaryType: XMLSecondaryPath, ColumnName: "x", PrimaryIndexName: "PX"},
		{Name: "SXI", IndexID: 256002, IsSelective: true, ColumnName: "y"},
		{Name: "SXI_n", IndexID: 256003, IsSelective: true, ColumnName: "y", PrimaryIndexName: "SXI"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d XML indexes, want %d", len(got), len(want))
	}
	for i := range want {
		if *got[i] != want[i] {
			t.Errorf("XML index %d = %+v, want %+v", i, *got[i], want[i])
		}
	}
}
