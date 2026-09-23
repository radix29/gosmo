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
	index := func() *Index { return table().IndexRef("IX_A]B") }

	runScriptCases(t, []scriptCase{
		{"Index Rebuild", func(c context.Context) error {
			return index().Rebuild(c, IndexRebuildOptions{FillFactor: 80})
		}, scriptUsePrefix + "ALTER INDEX [IX_A]]B] ON [dbo].[Sales.Archive] REBUILD WITH (FILLFACTOR = 80)"},
		{"Index Rebuild with options", func(c context.Context) error {
			return index().Rebuild(c, IndexRebuildOptions{FillFactor: 90, PadIndex: new(true), DataCompression: DataCompressionPage})
		}, scriptUsePrefix + "ALTER INDEX [IX_A]]B] ON [dbo].[Sales.Archive] REBUILD WITH (PAD_INDEX = ON, FILLFACTOR = 90, DATA_COMPRESSION = PAGE)"},
		{"Index Reorganize", func(c context.Context) error {
			return index().Reorganize(c)
		}, scriptUsePrefix + "ALTER INDEX [IX_A]]B] ON [dbo].[Sales.Archive] REORGANIZE"},
		{"Index SetOptions", func(c context.Context) error {
			return index().SetOptions(c, IndexSetOptions{IgnoreDupKey: new(true), AllowRowLocks: new(false), AllowPageLocks: new(true)})
		}, scriptUsePrefix + "ALTER INDEX [IX_A]]B] ON [dbo].[Sales.Archive] SET (IGNORE_DUP_KEY = ON, ALLOW_ROW_LOCKS = OFF, ALLOW_PAGE_LOCKS = ON)"},
		{
			// A nil option is not sent: IGNORE_DUP_KEY stays off the
			// statement, which is the form a constraint-backing index needs.
			"Index SetOptions leaves nil options out", func(c context.Context) error {
				return index().SetOptions(c, IndexSetOptions{AllowRowLocks: new(false), AllowPageLocks: new(true)})
			}, scriptUsePrefix + "ALTER INDEX [IX_A]]B] ON [dbo].[Sales.Archive] SET (ALLOW_ROW_LOCKS = OFF, ALLOW_PAGE_LOCKS = ON)"},
		{
			// 0 is a FULLSCAN, as it is to Statistic.Update — not the
			// server's default sample, which it was until 2026-09-23.
			"Index UpdateStatistics", func(c context.Context) error {
				return index().UpdateStatistics(c, 0)
			}, scriptUsePrefix + "UPDATE STATISTICS [dbo].[Sales.Archive] ([IX_A]]B]) WITH FULLSCAN"},
		{"Index UpdateStatistics sampled", func(c context.Context) error {
			return index().UpdateStatistics(c, 25)
		}, scriptUsePrefix + "UPDATE STATISTICS [dbo].[Sales.Archive] ([IX_A]]B]) WITH SAMPLE 25 PERCENT"},
		{"Index Disable", func(c context.Context) error {
			return index().Disable(c)
		}, scriptUsePrefix + "ALTER INDEX [IX_A]]B] ON [dbo].[Sales.Archive] DISABLE"},
		{
			// A disabled index is re-enabled by rebuilding it; there is no
			// ALTER INDEX ... ENABLE. The rebuild carries no FILLFACTOR,
			// which would otherwise change the index's stored setting as a
			// side effect of turning it back on.
			"Index Enable rebuilds", func(c context.Context) error {
				return index().Enable(c)
			}, scriptUsePrefix + "ALTER INDEX [IX_A]]B] ON [dbo].[Sales.Archive] REBUILD"},
		{"Index Drop", func(c context.Context) error {
			return index().Drop(c)
		}, scriptUsePrefix + "DROP INDEX [IX_A]]B] ON [dbo].[Sales.Archive]"},
		{"Table RebuildAllIndexes", func(c context.Context) error {
			return table().RebuildAllIndexes(c, 70)
		}, scriptUsePrefix + "ALTER INDEX ALL ON [dbo].[Sales.Archive] REBUILD WITH (FILLFACTOR = 70)"},
		{"Table TruncateTable", func(c context.Context) error {
			return table().TruncateTable(c)
		}, scriptUsePrefix + "TRUNCATE TABLE [dbo].[Sales.Archive]"},
		{"Table CreateStatistic", func(c context.Context) error {
			return table().CreateStatistic(c, CreateStatisticRequest{Name: "st]1", Columns: []string{"a]b", "c'd"}, SamplePercent: 50})
		}, scriptUsePrefix + "CREATE STATISTICS [st]]1] ON [dbo].[Sales.Archive] ([a]]b], [c'd]) WITH SAMPLE 50 PERCENT"},
		{"Table CreateStatistic without a sample", func(c context.Context) error {
			return table().CreateStatistic(c, CreateStatisticRequest{Name: "st1", Columns: []string{"ab"}})
		}, scriptUsePrefix + "CREATE STATISTICS [st1] ON [dbo].[Sales.Archive] ([ab])"},
		{"Table CreateStatistic with options", func(c context.Context) error {
			return table().CreateStatistic(c, CreateStatisticRequest{
				Name:             "st]1",
				Columns:          []string{"a]b", "c'd"},
				FullScan:         true,
				FilterDefinition: "[a]]b] IS NOT NULL",
				NoRecompute:      true,
				Incremental:      true,
			})
		}, scriptUsePrefix + "CREATE STATISTICS [st]]1] ON [dbo].[Sales.Archive] ([a]]b], [c'd]) WHERE [a]]b] IS NOT NULL WITH FULLSCAN, NORECOMPUTE, INCREMENTAL = ON"},
		{"Table CreateStatistic with options, sampled", func(c context.Context) error {
			return table().CreateStatistic(c, CreateStatisticRequest{
				Name: "st1", Columns: []string{"ab"}, SamplePercent: 25,
			})
		}, scriptUsePrefix + "CREATE STATISTICS [st1] ON [dbo].[Sales.Archive] ([ab]) WITH SAMPLE 25 PERCENT"},
		{"Table UpdateAllStatistics", func(c context.Context) error {
			return table().UpdateAllStatistics(c, 25)
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
		err := table.CreateStatistic(ctx, CreateStatisticRequest{Name: c.stat, Columns: c.columns})
		if err == nil {
			t.Errorf("CreateStatistic(%s) returned nil, want an error", c.name)
		} else if !strings.Contains(err.Error(), c.want) {
			t.Errorf("CreateStatistic(%s) error = %v, want it to mention %q", c.name, err, c.want)
		}
		if len(script.Statements()) != 0 {
			t.Errorf("CreateStatistic(%s) scripted %q, want nothing", c.name, script.Statements())
		}
	}
}

// A full scan and a sample percentage describe two different reads of the
// table, and CREATE STATISTICS takes one WITH clause — a request carrying
// both is refused rather than silently resolved to one of them.
func TestCreateStatisticRefusesAFullScanAndASample(t *testing.T) {
	ctx, script := WithScript(context.Background())
	table := &Table{db: scriptTestDB(), Schema: "dbo", Name: "Sales.Archive"}
	err := table.CreateStatistic(ctx, CreateStatisticRequest{
		Name: "st1", Columns: []string{"a"}, FullScan: true, SamplePercent: 50,
	})
	if err == nil {
		t.Fatal("CreateStatistic returned nil, want an error")
	}
	if !strings.Contains(err.Error(), "alternatives") {
		t.Errorf("error = %v, want it to say the two are alternatives", err)
	}
	if len(script.Statements()) != 0 {
		t.Errorf("refused but still scripted %q", script.Statements())
	}
}

// The plan guide controls. All three are the same sp_control_plan_guide call
// with a different @operation, and they reach it through bound parameters —
// so what a captured statement shows is the substituted form bindScriptArgs
// produces, not @p1/@p2.
func TestScriptPlanGuideControls(t *testing.T) {
	guide := func() *PlanGuide { return &PlanGuide{db: scriptTestDB(), Name: "PG_o'brien"} }

	runScriptCases(t, []scriptCase{
		{"PlanGuide Enable", func(c context.Context) error {
			return guide().Enable(c)
		}, scriptUsePrefix + "EXEC sp_control_plan_guide @operation = N'ENABLE', @name = N'PG_o''brien'"},
		{"PlanGuide Disable", func(c context.Context) error {
			return guide().Disable(c)
		}, scriptUsePrefix + "EXEC sp_control_plan_guide @operation = N'DISABLE', @name = N'PG_o''brien'"},
		{"PlanGuide Drop", func(c context.Context) error {
			return guide().Drop(c)
		}, scriptUsePrefix + "EXEC sp_control_plan_guide @operation = N'DROP', @name = N'PG_o''brien'"},
		{"DropPlanGuide by name", func(c context.Context) error {
			return scriptTestDB().DropPlanGuide(c, "PG_o'brien")
		}, scriptUsePrefix + "EXEC sp_control_plan_guide @operation = N'DROP', @name = N'PG_o''brien'"},
	})
}

// A scripted enable or disable must not move the receiver's IsDisabled:
// nothing ran, so the guide on the server still has the state it had.
func TestScriptedPlanGuideControlDoesNotMirrorOntoTheReceiver(t *testing.T) {
	g := &PlanGuide{db: scriptTestDB(), Name: "PG", IsDisabled: true}

	ctx, script := WithScript(context.Background())
	if err := g.Enable(ctx); err != nil {
		t.Fatalf("scripted enable: %v", err)
	}
	if !g.IsDisabled {
		t.Error("a scripted enable cleared IsDisabled on the receiver; the statement " +
			"was only captured, so the server's guide is still disabled")
	}
	if len(script.Statements()) != 1 {
		t.Fatalf("captured %q, want one statement", script.Statements())
	}
}
