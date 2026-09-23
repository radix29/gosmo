package gosmo

import (
	"strings"
	"testing"
)

// onTable puts a hand-built index on tbl, as Table.Indexes would have.
func onTable(tbl *Table, idx *Index) *Index {
	idx.table = tbl
	return idx
}

// fullyOptionedIndex is the S4 probe's index: every option DROP_EXISTING
// would reset if the statement did not restate it, on a filegroup that is not
// the table's.
func fullyOptionedIndex() *Index {
	return &Index{
		Name: "IX_probe", Type: IndexTypeNonClustered, IsUnique: true,
		FillFactor: 70, IsPadded: true, IgnoreDupKey: true, StatisticsNoRecompute: true,
		AllowRowLocks: true, AllowPageLocks: false, OptimizeForSequentialKey: true,
		DataCompression:  "PAGE",
		KeyColumns:       []IndexColumn{{Name: "a"}, {Name: "b", Descending: true}},
		IncludedColumns:  []IndexColumn{{Name: "c", IsIncluded: true}},
		FilterDefinition: "([a]>(0))",
		DataSpace:        DataSpace{Name: "FG2"},
	}
}

// SetIncludedColumns reissues the index with DROP_EXISTING, which builds it
// from what the statement says. The hand-built statement this replaced said
// nothing but the columns: the probe's index came back with fill factor 0,
// pad off, page locks on, NORECOMPUTE off and on PRIMARY instead of FG2.
func TestSetIncludedColumnsRestatesEveryOption(t *testing.T) {
	tbl := captureTable(t)
	if err := onTable(tbl, fullyOptionedIndex()).SetIncludedColumns(t.Context(), []string{"c", "d"}); err != nil {
		t.Fatalf("SetIncludedColumns: %v", err)
	}
	q := captured.find("DROP_EXISTING")
	if q == "" {
		t.Fatal("no DROP_EXISTING statement was sent")
	}
	for _, want := range []string{
		"CREATE UNIQUE NONCLUSTERED INDEX [IX_probe]\n    ON [my.schema].[Sales.Archive] ([a] ASC, [b] DESC)",
		"INCLUDE ([c], [d])",
		"WHERE ([a]>(0))",
		"PAD_INDEX = ON", "FILLFACTOR = 70", "IGNORE_DUP_KEY = ON",
		"STATISTICS_NORECOMPUTE = ON", "ALLOW_PAGE_LOCKS = OFF",
		"OPTIMIZE_FOR_SEQUENTIAL_KEY = ON", "DATA_COMPRESSION = PAGE",
		"DROP_EXISTING = ON)",
		" ON [FG2]",
	} {
		if !strings.Contains(q, want) {
			t.Errorf("statement is missing %q:\n%s", want, q)
		}
	}
	if strings.Contains(q, "DISABLE") {
		t.Errorf("an enabled index was disabled:\n%s", q)
	}
}

// With DROP_EXISTING an omitted ON clause means the table's data space, not
// the default filegroup, so the scripter's "default filegroup needs no
// clause" rule is exactly wrong here: the clause is always written.
func TestSetIncludedColumnsNamesTheDataSpaceExplicitly(t *testing.T) {
	for _, c := range []struct {
		name string
		ds   DataSpace
		want string
	}{
		{"default filegroup", DataSpace{Name: "PRIMARY", IsDefaultFileGroup: true}, " ON [PRIMARY]"},
		{"partition scheme", DataSpace{Name: "ps_year", IsPartitionScheme: true, PartitionColumn: "created"}, " ON [ps_year]([created])"},
	} {
		t.Run(c.name, func(t *testing.T) {
			tbl := captureTable(t)
			idx := fullyOptionedIndex()
			idx.DataSpace = c.ds
			if err := onTable(tbl, idx).SetIncludedColumns(t.Context(), []string{"c"}); err != nil {
				t.Fatalf("SetIncludedColumns: %v", err)
			}
			if q := captured.find("DROP_EXISTING"); !strings.HasSuffix(q, c.want) {
				t.Errorf("statement does not end with %q:\n%s", c.want, q)
			}
		})
	}
}

// The rebuild enables a disabled index; the same batch puts it back.
func TestSetIncludedColumnsKeepsADisabledIndexDisabled(t *testing.T) {
	tbl := captureTable(t)
	idx := fullyOptionedIndex()
	idx.IsDisabled = true
	if err := onTable(tbl, idx).SetIncludedColumns(t.Context(), []string{"c"}); err != nil {
		t.Fatalf("SetIncludedColumns: %v", err)
	}
	q := captured.find("DROP_EXISTING")
	create, disable := strings.Index(q, "CREATE"), strings.Index(q, "ALTER INDEX [IX_probe] ON [my.schema].[Sales.Archive] DISABLE")
	if disable < 0 || disable < create {
		t.Errorf("want the CREATE followed by a DISABLE of the same index:\n%s", q)
	}
}

// Only a rowstore nonclustered index backing no constraint has an INCLUDE
// list that CREATE ... DROP_EXISTING can change. Every other kind is refused
// before anything is sent, with an error naming what the index is.
func TestSetIncludedColumnsRefusesWhatItCannotRecreate(t *testing.T) {
	for _, c := range []struct {
		name  string
		edit  func(*Index)
		label string
	}{
		{"clustered", func(i *Index) { i.Type, i.IsClustered = IndexTypeClustered, true }, "CLUSTERED index"},
		{"xml", func(i *Index) { i.Type = IndexTypeXML }, "XML index"},
		{"spatial", func(i *Index) { i.Type = IndexTypeSpatial }, "SPATIAL index"},
		{"hash", func(i *Index) { i.Type = IndexType("NONCLUSTERED HASH") }, "NONCLUSTERED HASH index"},
		{"columnstore", func(i *Index) { i.Type = IndexTypeColumnStore }, "COLUMNSTORE index"},
		{"primary key", func(i *Index) { i.IsPrimaryKey = true }, "PRIMARY KEY constraint"},
		{"unique constraint", func(i *Index) { i.IsUniqueConstraint = true }, "UNIQUE constraint"},
		{"memory-optimized", func(i *Index) { i.DataSpace = DataSpace{} }, "memory-optimized table"},
	} {
		t.Run(c.name, func(t *testing.T) {
			tbl := captureTable(t)
			idx := fullyOptionedIndex()
			c.edit(idx)
			if err := idx.IncludedColumnsSupported(); err == nil || !strings.Contains(err.Error(), c.label) {
				t.Errorf("IncludedColumnsSupported() = %v, want an error naming %q", err, c.label)
			}
			err := onTable(tbl, idx).SetIncludedColumns(t.Context(), []string{"c"})
			if err == nil || !strings.Contains(err.Error(), c.label) {
				t.Errorf("SetIncludedColumns() = %v, want an error naming %q", err, c.label)
			}
			if q := captured.find("CREATE"); q != "" {
				t.Errorf("refused but still executed: %s", q)
			}
		})
	}
	if err := fullyOptionedIndex().IncludedColumnsSupported(); err != nil {
		t.Errorf("a plain nonclustered index is refused: %v", err)
	}
}

// SetIncludedColumns and Script as CREATE share rowstoreIndexCreate; apart
// from the WITH additions and the ON clause the two must say the same thing.
func TestSetIncludedColumnsAndScriptAgree(t *testing.T) {
	tbl := captureTable(t)
	idx := fullyOptionedIndex()
	if err := onTable(tbl, idx).SetIncludedColumns(t.Context(), []string{"c"}); err != nil {
		t.Fatalf("SetIncludedColumns: %v", err)
	}
	sent := captured.find("DROP_EXISTING")
	script := scriptIndex(idx, tbl.FullName(), ScriptOptions{})
	head := script[:strings.Index(script, "WITH (")]
	if !strings.HasPrefix(sent, head) {
		t.Errorf("the rebuild and the script differ before WITH:\nsent:   %s\nscript: %s", sent, head)
	}
}

// An IndexRef handle carries no type and no options, so there is nothing for
// the DROP_EXISTING rebuild to restate. It must say so, not fall through to
// "not supported for a  index" with the empty type spliced in.
func TestIncludedColumnsRefusesAHandle(t *testing.T) {
	tbl := captureTable(t)
	err := tbl.IndexRef("IX").SetIncludedColumns(t.Context(), []string{"c"})
	if err == nil || !strings.Contains(err.Error(), "IndexByName") {
		t.Errorf("SetIncludedColumns on a handle = %v, want an error pointing at IndexByName", err)
	}
	if q := captured.find("CREATE"); q != "" {
		t.Errorf("refused but still executed: %s", q)
	}
}

// ALTER INDEX ... SET () is a syntax error; an IndexSetOptions naming nothing
// is refused before anything is sent.
func TestIndexSetOptionsRefusesNoOptions(t *testing.T) {
	tbl := captureTable(t)
	if err := tbl.IndexRef("IX").SetOptions(t.Context(), IndexSetOptions{}); err == nil {
		t.Error("SetOptions with no option returned nil, want an error")
	}
	if q := captured.find("ALTER INDEX"); q != "" {
		t.Errorf("refused but still executed: %s", q)
	}
}
