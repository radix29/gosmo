package gosmo

import (
	"context"
	"strings"
	"testing"
)

// TestScriptIndexAndStatisticsWrites pins the index and statistics
// maintenance statements. See script_write_common_test.go.
//
// The table these run against is named Sales.Archive on purpose: an index
// statement names its table in the ON clause, and a table name carrying a dot
// that reaches the statement unbracketed parses as schema.object and rebuilds
// an index on something else — or on nothing, which reports success.
func TestScriptIndexAndStatisticsWrites(t *testing.T) {
	table := func() *Table { return &Table{db: scriptTestDB(), Schema: "dbo", Name: "Sales.Archive"} }
	index := func() *Index { return &Index{Name: "IX_A]B", IndexID: 3} }

	runScriptCases(t, []scriptCase{
		{"Index Rebuild", func(c context.Context) error {
			return index().RebuildContext(c, table(), 80)
		}, scriptUsePrefix + "ALTER INDEX [IX_A]]B] ON [dbo].[Sales.Archive] REBUILD WITH (FILLFACTOR = 80)"},
		{"Index RebuildWithOptions", func(c context.Context) error {
			return index().RebuildWithOptionsContext(c, table(), 90, true, "PAGE")
		}, scriptUsePrefix + "ALTER INDEX [IX_A]]B] ON [dbo].[Sales.Archive] REBUILD WITH (PAD_INDEX = ON, FILLFACTOR = 90, DATA_COMPRESSION = PAGE)"},
		{"Index Reorganize", func(c context.Context) error {
			return index().ReorganizeContext(c, table())
		}, scriptUsePrefix + "ALTER INDEX [IX_A]]B] ON [dbo].[Sales.Archive] REORGANIZE"},
		{"Index SetOptions", func(c context.Context) error {
			return index().SetOptionsContext(c, table(), true, false, true)
		}, scriptUsePrefix + "ALTER INDEX [IX_A]]B] ON [dbo].[Sales.Archive] SET (IGNORE_DUP_KEY = ON, ALLOW_ROW_LOCKS = OFF, ALLOW_PAGE_LOCKS = ON)"},
		{"Index SetLockOptions", func(c context.Context) error {
			return index().SetLockOptionsContext(c, table(), false, true)
		}, scriptUsePrefix + "ALTER INDEX [IX_A]]B] ON [dbo].[Sales.Archive] SET (ALLOW_ROW_LOCKS = OFF, ALLOW_PAGE_LOCKS = ON)"},
		{"Index UpdateStatistics", func(c context.Context) error {
			return index().UpdateStatisticsContext(c, table())
		}, scriptUsePrefix + "UPDATE STATISTICS [dbo].[Sales.Archive] ([IX_A]]B])"},
		{"Table RebuildAllIndexes", func(c context.Context) error {
			return table().RebuildAllIndexesContext(c, 70)
		}, scriptUsePrefix + "ALTER INDEX ALL ON [dbo].[Sales.Archive] REBUILD WITH (FILLFACTOR = 70)"},
		{"Table TruncateTable", func(c context.Context) error {
			return table().TruncateTableContext(c)
		}, scriptUsePrefix + "TRUNCATE TABLE [dbo].[Sales.Archive]"},
		{"Table CreateStatistic", func(c context.Context) error {
			return table().CreateStatisticContext(c, "st]1", []string{"a]b", "c'd"}, 50)
		}, scriptUsePrefix + "CREATE STATISTICS [st]]1] ON [dbo].[Sales.Archive] ([a]]b], [c'd]) WITH SAMPLE 50 PERCENT"},
		{"Table CreateStatistic without a sample", func(c context.Context) error {
			return table().CreateStatisticContext(c, "st1", []string{"ab"}, 0)
		}, scriptUsePrefix + "CREATE STATISTICS [st1] ON [dbo].[Sales.Archive] ([ab])"},
		{"Table UpdateAllStatistics", func(c context.Context) error {
			return table().UpdateAllStatisticsContext(c, 25)
		}, scriptUsePrefix + "UPDATE STATISTICS [dbo].[Sales.Archive] WITH SAMPLE 25 PERCENT"},
	})
}

// TestCreateStatisticRefusesAnEmptySpec pins the two guards that stop a
// statement being built at all: CREATE STATISTICS with no name, and with no
// column list — the second of which is a syntax error rather than a no-op.
func TestCreateStatisticRefusesAnEmptySpec(t *testing.T) {
	table := &Table{db: scriptTestDB(), Schema: "dbo", Name: "Sales"}
	cases := []struct {
		name    string
		stat    string
		columns []string
		want    string
	}{
		{"no name", "", []string{"a"}, "name is required"},
		{"no columns", "st1", nil, "at least one column"},
	}
	for _, c := range cases {
		ctx, script := WithScript(context.Background())
		err := table.CreateStatisticContext(ctx, c.stat, c.columns, 0)
		if err == nil {
			t.Errorf("CreateStatisticContext(%s) returned nil, want an error", c.name)
		} else if !strings.Contains(err.Error(), c.want) {
			t.Errorf("CreateStatisticContext(%s) error = %v, want it to mention %q", c.name, err, c.want)
		}
		if len(script.Statements) != 0 {
			t.Errorf("CreateStatisticContext(%s) scripted %q, want nothing", c.name, script.Statements)
		}
	}
}
