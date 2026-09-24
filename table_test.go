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
				"no_recompute", "optimize_for_sequential_key", "bucket_count"},
			rows: [][]driver.Value{
				{"PK_T", int64(1), "CLUSTERED", true, true, false, false, int64(0), "", false, false, true, true, "NONE", "PRIMARY", false, true, "", false, false, int64(0)},
				{"IX_covering", int64(2), "NONCLUSTERED", false, false, false, false, int64(0), "", false, false, true, true, "NONE", "ps_year", true, false, "created", true, true, int64(0)},
				{"IX_empty", int64(3), "NONCLUSTERED", false, false, false, false, int64(0), "", false, false, true, true, "NONE", "FG_archive", false, false, "", false, false, int64(0)},
			},
		},
		cannedRow{
			match: "FROM   sys.index_columns ic",
			cols:  []string{"index_id", "name", "is_descending_key", "is_included_column"},
			rows: [][]driver.Value{
				{int64(0), "heap_col", false, false},
				{int64(1), "id", false, false},
				{int64(2), "a", true, false},
				{int64(2), "b", false, false},
				{int64(2), "note", false, true},
				{int64(2), "total", false, true},
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
				"no_recompute", "optimize_for_sequential_key", "bucket_count"},
			rows: [][]driver.Value{
				{"PK_T", int64(1), "CLUSTERED", true, true, false, false, int64(0), "", false, false, true, true, "NONE", "ps_year", true, false, "Created", false, false, int64(0)},
				{"IX_archive", int64(2), "NONCLUSTERED", false, false, false, false, int64(0), "", false, false, true, true, "NONE", "FG_Archive", false, false, "", false, false, int64(0)},
				{"IX_default", int64(3), "NONCLUSTERED", false, false, false, false, int64(0), "", false, false, true, true, "NONE", "PRIMARY", false, true, "", false, false, int64(0)},
			},
		},
		cannedRow{
			match: "FROM   sys.index_columns ic",
			cols:  []string{"index_id", "name", "is_descending_key", "is_included_column"},
			rows:  [][]driver.Value{{int64(1), "id", false, false}},
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
