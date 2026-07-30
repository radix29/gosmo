package gosmo

import (
	"strings"
	"testing"
)

// Table.IndexesContext maps sys.indexes.type_desc onto IndexType. The two
// columnstore spellings are distinct types, and a clustered columnstore index
// is clustered — a caller labelling indexes from IsClustered otherwise calls
// it nonclustered.
func TestIndexTypeIsColumnStore(t *testing.T) {
	for _, tc := range []struct {
		typ  IndexType
		want bool
	}{
		{IndexTypeColumnStore, true},
		{IndexTypeClusteredColumnStore, true},
		{IndexTypeClustered, false},
		{IndexTypeNonClustered, false},
		{IndexTypeXML, false},
		{IndexTypeSpatial, false},
		{IndexType("NONCLUSTERED HASH"), false},
		{IndexType(""), false},
	} {
		if got := tc.typ.IsColumnStore(); got != tc.want {
			t.Errorf("IndexType(%q).IsColumnStore() = %v, want %v", tc.typ, got, tc.want)
		}
	}
}

// The two columnstore constants must not collide: IndexTypeColumnStore keeps
// its original spelling for the nonclustered form, so code that switches on
// one does not silently match the other.
func TestColumnStoreIndexTypesAreDistinct(t *testing.T) {
	if IndexTypeColumnStore == IndexTypeClusteredColumnStore {
		t.Fatal("the two columnstore index types must be distinct values")
	}
}

// SetIncludedColumns recreates the index with CREATE INDEX, choosing
// CLUSTERED/NONCLUSTERED from IsClustered. On a columnstore index that would
// replace it with a rowstore index of the same name, so it must refuse rather
// than execute anything.
func TestSetIncludedColumnsRejectsColumnStore(t *testing.T) {
	for _, typ := range []IndexType{IndexTypeColumnStore, IndexTypeClusteredColumnStore} {
		tbl := captureTable(t)
		idx := &Index{Name: "cci", Type: typ, IsClustered: typ == IndexTypeClusteredColumnStore,
			KeyColumns: []IndexColumn{{Name: "a"}}}

		err := idx.SetIncludedColumns(tbl, []string{"b"})
		if err == nil {
			t.Fatalf("%s: SetIncludedColumns returned nil, want an error", typ)
		}
		if !strings.Contains(err.Error(), "not supported") {
			t.Errorf("%s: error = %v, want it to say the operation is unsupported", typ, err)
		}
		if q := captured.find("CREATE"); q != "" {
			t.Errorf("%s: refused but still executed: %s", typ, q)
		}
	}
}

// CREATE CLUSTERED COLUMNSTORE INDEX takes no key columns, so it doesn't fit
// the statement CreateIndex builds. It must be refused, not quietly turned
// into a plain NONCLUSTERED index — the behavior before the type existed.
func TestCreateIndexRejectsClusteredColumnStore(t *testing.T) {
	tbl := captureTable(t)
	err := tbl.CreateIndex(CreateIndexRequest{
		Name:       "cci",
		Type:       IndexTypeClusteredColumnStore,
		KeyColumns: []IndexColumnDef{{Name: "a"}},
	})
	if err == nil {
		t.Fatal("CreateIndex returned nil for a clustered columnstore index, want an error")
	}
	if q := captured.find("CREATE"); q != "" {
		t.Errorf("refused but still executed: %s", q)
	}
}

// The nonclustered columnstore path is unchanged by the split.
func TestCreateIndexNonClusteredColumnStoreUnchanged(t *testing.T) {
	tbl := captureTable(t)
	if err := tbl.CreateIndex(CreateIndexRequest{
		Name:       "ncci",
		Type:       IndexTypeColumnStore,
		KeyColumns: []IndexColumnDef{{Name: "a"}},
	}); err != nil {
		t.Fatalf("CreateIndex: %v", err)
	}
	q := captured.find("CREATE")
	if !strings.Contains(q, "NONCLUSTERED COLUMNSTORE INDEX") {
		t.Errorf("statement = %q, want it to create a NONCLUSTERED COLUMNSTORE index", q)
	}
}
