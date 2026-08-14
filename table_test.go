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
		{"decimal with precision", ColumnDefinition{DataType: DataTypeDecimal, Precision: 18, Scale: 2}, "decimal(18,2)"},
		{"decimal no precision", ColumnDefinition{DataType: DataTypeDecimal}, "decimal"},
		{"datetime2 with scale", ColumnDefinition{DataType: DataTypeDatetime2, Scale: 3}, "datetime2(3)"},
		{"datetime2 no scale", ColumnDefinition{DataType: DataTypeDatetime2}, "datetime2"},
		{"plain int", ColumnDefinition{DataType: DataTypeInt}, "int"},
		{"bit", ColumnDefinition{DataType: DataTypeBit}, "bit"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := colTypeSQL(c.col); got != c.want {
				t.Errorf("colTypeSQL(%+v) = %q, want %q", c.col, got, c.want)
			}
		})
	}
}

// TestAlterColumnRequiresName pins the early-return validation that runs
// before AlterColumn ever touches t.db — the only part of the DDL flow
// testable without a live server.
func TestAlterColumnRequiresName(t *testing.T) {
	tbl := &Table{Schema: "dbo", Name: "T"}
	if err := tbl.AlterColumn(ColumnDefinition{DataType: DataTypeInt}); err == nil {
		t.Error("AlterColumn with empty column name = nil error, want error")
	}
}

// IndexesContext costs two queries however many indexes the table has: one
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
				"data_compression_desc"},
			rows: [][]driver.Value{
				{"PK_T", int64(1), "CLUSTERED", true, true, false, false, int64(0), "", false, false, true, true, "NONE"},
				{"IX_covering", int64(2), "NONCLUSTERED", false, false, false, false, int64(0), "", false, false, true, true, "NONE"},
				{"IX_empty", int64(3), "NONCLUSTERED", false, false, false, false, int64(0), "", false, false, true, true, "NONE"},
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

	indexes, err := tbl.IndexesContext(context.Background())
	if err != nil {
		t.Fatalf("IndexesContext: %v", err)
	}

	if n := captured.count("sys.index_columns"); n != 1 {
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
	}
}

// A table with no indexes must not query for columns at all — there is
// nothing for the rows to be grouped onto, and the query is a round trip
// against every heap in an Object Explorer folder otherwise.
func TestIndexesSkipsTheColumnQueryWhenThereAreNoIndexes(t *testing.T) {
	tbl := captureTable(t)
	captured.reset()

	indexes, err := tbl.IndexesContext(context.Background())
	if err != nil {
		t.Fatalf("IndexesContext: %v", err)
	}
	if len(indexes) != 0 {
		t.Errorf("got %d indexes, want none", len(indexes))
	}
	if n := captured.count("sys.index_columns"); n != 0 {
		t.Errorf("sys.index_columns queried %d times for a table with no indexes, want 0", n)
	}
}
