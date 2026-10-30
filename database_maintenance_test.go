package gosmo

import (
	"context"
	"database/sql/driver"
	"errors"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestScriptMaintenanceWrites pins the statements gossms phase 8.a added: the
// rebuild's ONLINE and MAXDOP, the whole-table rebuild with every option,
// sp_updatestats and DBCC CHECKDB. See script_write_common_test.go.
func TestScriptMaintenanceWrites(t *testing.T) {
	table := func() *Table { return &Table{db: scriptTestDB(), Schema: "dbo", Name: "Sales.Archive"} }
	d := scriptTestDB

	runScriptCases(t, []scriptCase{
		{"Index Rebuild online", func(c context.Context) error {
			return table().IndexRef("IX").Rebuild(c, IndexRebuildOptions{FillFactor: 90, Online: true, MaxDOP: 2})
		}, scriptUsePrefix + "ALTER INDEX [IX] ON [dbo].[Sales.Archive] REBUILD WITH (FILLFACTOR = 90, ONLINE = ON, MAXDOP = 2)"},
		{"Table RebuildAllIndexesWith", func(c context.Context) error {
			return table().RebuildAllIndexesWith(c, IndexRebuildOptions{PadIndex: new(false), DataCompression: DataCompressionRow, Online: true, MaxDOP: 1})
		}, scriptUsePrefix + "ALTER INDEX ALL ON [dbo].[Sales.Archive] REBUILD WITH (PAD_INDEX = OFF, DATA_COMPRESSION = ROW, ONLINE = ON, MAXDOP = 1)"},
		{"Table RebuildAllIndexesWith no options", func(c context.Context) error {
			return table().RebuildAllIndexesWith(c, IndexRebuildOptions{})
		}, scriptUsePrefix + "ALTER INDEX ALL ON [dbo].[Sales.Archive] REBUILD"},
		{"Database UpdateStatistics", func(c context.Context) error {
			return d().UpdateStatistics(c, false)
		}, scriptUsePrefix + "EXEC sys.sp_updatestats"},
		{"Database UpdateStatistics resample", func(c context.Context) error {
			return d().UpdateStatistics(c, true)
		}, scriptUsePrefix + "EXEC sys.sp_updatestats @resample = 'resample'"},
		{"Database CheckDB", func(c context.Context) error {
			return errOnly(d().CheckDB(c, CheckDBOptions{}))
		}, scriptUsePrefix + "DBCC CHECKDB (N'App''DB') WITH NO_INFOMSGS, ALL_ERRORMSGS, TABLERESULTS"},
		{"Database CheckDB physical only", func(c context.Context) error {
			return errOnly(d().CheckDB(c, CheckDBOptions{PhysicalOnly: true, NoIndex: true, MaxDOP: 4}))
		}, scriptUsePrefix + "DBCC CHECKDB (N'App''DB', NOINDEX) WITH NO_INFOMSGS, ALL_ERRORMSGS, TABLERESULTS, PHYSICAL_ONLY, MAXDOP = 4"},
		{"Database CheckDB logical", func(c context.Context) error {
			return errOnly(d().CheckDB(c, CheckDBOptions{ExtendedLogicalChecks: true, DataPurity: true}))
		}, scriptUsePrefix + "DBCC CHECKDB (N'App''DB') WITH NO_INFOMSGS, ALL_ERRORMSGS, TABLERESULTS, EXTENDED_LOGICAL_CHECKS, DATA_PURITY"},
	})
}

// The refusals are made before anything is sent, and read as the caller's
// mistake.
func TestMaintenanceRefusals(t *testing.T) {
	ctx, script := WithScript(context.Background())
	d := scriptTestDB()
	tbl := &Table{db: d, Schema: "dbo", Name: "t"}
	cases := map[string]error{
		"physical only + data purity":     errOnly(d.CheckDB(ctx, CheckDBOptions{PhysicalOnly: true, DataPurity: true})),
		"physical only + extended checks": errOnly(d.CheckDB(ctx, CheckDBOptions{PhysicalOnly: true, ExtendedLogicalChecks: true})),
		"checkdb negative maxdop":         errOnly(d.CheckDB(ctx, CheckDBOptions{MaxDOP: -1})),
		"rebuild maxdop over 64":          tbl.IndexRef("IX").Rebuild(ctx, IndexRebuildOptions{MaxDOP: 65}),
		"rebuild all negative maxdop":     tbl.RebuildAllIndexesWith(ctx, IndexRebuildOptions{MaxDOP: -1}),
		"rebuild all fill factor 101":     tbl.RebuildAllIndexesWith(ctx, IndexRebuildOptions{FillFactor: 101}),
		"rebuild all bad compression":     tbl.RebuildAllIndexesWith(ctx, IndexRebuildOptions{DataCompression: "ZIP"}),
		"fragmentation bad mode":          errOnly(d.FragmentationStats(ctx, "FAST", 0, 0)),
		"fragmentation pct over 100":      errOnly(d.FragmentationStats(ctx, "", 101, 0)),
		"fragmentation negative pages":    errOnly(d.FragmentationStats(ctx, "", 0, -1)),
	}
	for name, err := range cases {
		if !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("%s: %v, want ErrInvalidRequest", name, err)
		}
		checkOnePrefix(t, name, err)
	}
	if n := len(script.Statements()); n != 0 {
		t.Errorf("a refused call captured %d statements: %q", n, script.Statements())
	}
}

// checkDBColumns is TABLERESULTS' header as SQL Server 2016, 2017 and 2025
// all return it (read live, 2026-10-30).
var checkDBColumns = []string{"Error", "Level", "State", "MessageText", "RepairLevel", "Status", "DbId", "DbFragId",
	"ObjectId", "IndexId", "PartitionId", "AllocUnitId", "RidDbId", "RidPruId", "File", "Page", "Slot",
	"RefDbId", "RefPruId", "RefFile", "RefPage", "RefSlot", "Allocation"}

// CheckDB reads its rows by column name, so the fake serves them in a
// shuffled order with an extra column, and the values must still land in
// their fields. The rows are a real 2025 run's against a page corrupted with
// DBCC WRITEPAGE, trimmed.
func TestCheckDBScansRowsByColumnName(t *testing.T) {
	cols := slices.Clone(checkDBColumns)
	slices.Reverse(cols)
	cols = append(cols, "SomeFutureColumn")
	row := func(vals map[string]driver.Value) []driver.Value {
		out := make([]driver.Value, len(cols))
		for i, c := range cols {
			out[i] = vals[c]
		}
		return out
	}
	d := qsRecDB(t, 17, cols, [][]driver.Value{
		row(map[string]driver.Value{"Error": int64(8939), "Level": int64(16), "State": int64(98),
			"MessageText": "Table error: Object ID 1221579390, index ID 2 ...", "RepairLevel": "repair_allow_data_loss",
			"ObjectId": int64(1221579390), "IndexId": int64(2), "PartitionId": int64(72057594047299584),
			"AllocUnitId": int64(72057594054049792), "File": int64(1), "Page": int64(376), "Slot": int64(0),
			"SomeFutureColumn": "x"}),
		row(map[string]driver.Value{"Error": int64(8989), "Level": int64(10), "State": int64(1),
			"MessageText": "CHECKDB found 0 allocation errors and 4 consistency errors in database 'appdb'.",
			"RepairLevel": nil, "ObjectId": int64(0)}),
	})
	got, err := d.CheckDB(context.Background(), CheckDBOptions{})
	if err != nil {
		t.Fatalf("CheckDB: %v", err)
	}
	want := []*CheckDBError{
		{Error: 8939, Level: 16, State: 98, Message: "Table error: Object ID 1221579390, index ID 2 ...",
			RepairLevel: "repair_allow_data_loss", ObjectID: 1221579390, IndexID: 2,
			PartitionID: 72057594047299584, AllocUnitID: 72057594054049792, File: 1, Page: 376},
		{Error: 8989, Level: 10, State: 1, Message: "CHECKDB found 0 allocation errors and 4 consistency errors in database 'appdb'."},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d rows, want %d", len(got), len(want))
	}
	for i := range want {
		if *got[i] != *want[i] {
			t.Errorf("row %d:\n got %+v\nwant %+v", i, *got[i], *want[i])
		}
	}
	if got[0].IsSummary() || !got[1].IsSummary() {
		t.Errorf("IsSummary = %v, %v; want false, true", got[0].IsSummary(), got[1].IsSummary())
	}
	if c := qsRec.last(t); !strings.HasPrefix(c.sql, "DBCC CHECKDB (N'appdb') WITH NO_INFOMSGS") {
		t.Errorf("sent %q", c.sql)
	}
}

// A clean database answers with no row, which is no error.
func TestCheckDBCleanIsNoRows(t *testing.T) {
	d := qsRecDB(t, 17, checkDBColumns, nil)
	got, err := d.CheckDB(context.Background(), CheckDBOptions{})
	if err != nil || len(got) != 0 {
		t.Fatalf("CheckDB = %v, %v; want no rows, no error", got, err)
	}
}

// The thresholds bind as parameters, in order, after the mode is formatted
// into the DMV call; the IndexType comes from type_desc.
func TestDatabaseFragmentationStatsQuery(t *testing.T) {
	d := qsRecDB(t, 17, anonCols(12), [][]driver.Value{
		{"IX_a", int64(2), 42.5, int64(5000), int64(300), 71.0, "dbo", "t", int64(99), false, "NONCLUSTERED", int64(1)},
		{"CI_v", int64(1), 12.0, int64(1200), int64(40), 0.0, "s", "v", int64(100), true, "CLUSTERED", int64(3)},
	})
	got, err := d.FragmentationStats(context.Background(), FragmentationSampled, 10, 1000)
	if err != nil {
		t.Fatalf("FragmentationStats: %v", err)
	}
	c := qsRec.last(t)
	if !strings.Contains(c.sql, "sys.dm_db_index_physical_stats(DB_ID(), NULL, NULL, NULL, N'SAMPLED')") {
		t.Errorf("query does not scan the database in SAMPLED mode:\n%s", c.sql)
	}
	if !slices.Equal(c.args, []any{10.0, int64(1000)}) {
		t.Errorf("args = %v, want [10 1000]", c.args)
	}
	if len(got) != 2 {
		t.Fatalf("got %d rows, want 2", len(got))
	}
	want := DatabaseIndexFragmentation{
		IndexFragmentation: IndexFragmentation{IndexName: "CI_v", IndexID: 1, AvgFragmentationPct: 12, PageCount: 1200, FragmentCount: 40},
		Schema:             "s", ObjectName: "v", ObjectID: 100, IsView: true, IndexType: IndexTypeClustered, PartitionCount: 3,
	}
	if *got[1] != want {
		t.Errorf("row 1:\n got %+v\nwant %+v", *got[1], want)
	}
	if got[0].IndexType != IndexTypeNonClustered || got[0].IsView {
		t.Errorf("row 0 = %+v", *got[0])
	}
}

// LastBackups lists every database, the never-backed-up ones with nil
// backups, folds a striped set's families, and drops a dropped database's
// history.
func TestLastBackupsGroupsByDatabaseAndKind(t *testing.T) {
	at := time.Date(2026, 10, 30, 1, 0, 0, 0, time.UTC)
	set := func(db, typ string, id int64, device string) []driver.Value {
		return []driver.Value{db, "", "", typ, at, at, int64(1024), device, "sa", "srv",
			int64(957), int64(160), int64(1), id, id, int64(1), false, "1", "2", "", ""}
	}
	d := qsRecDB(t, 17, nil, nil)
	qsRec.route("FROM   sys.databases", []string{"name", "recovery_model_desc"}, [][]driver.Value{
		{"app", "FULL"}, {"master", "SIMPLE"}, {"never", nil},
	})
	qsRec.route("msdb.dbo.backupset", anonCols(21), [][]driver.Value{
		set("app", "D", 10, "/b/app_1.bak"),
		set("app", "D", 10, "/b/app_2.bak"),
		set("app", "L", 12, "/b/app.trn"),
		set("gone", "D", 13, "/b/gone.bak"),
		set("master", "I", 14, "/b/master.dif"),
	})
	got, err := d.server.LastBackups(context.Background())
	if err != nil {
		t.Fatalf("LastBackups: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d databases, want 3", len(got))
	}
	app, master, never := got[0], got[1], got[2]
	if app.DatabaseName != "app" || app.RecoveryModel != RecoveryModelFull || app.Full == nil || app.Log == nil || app.Differential != nil {
		t.Errorf("app = %+v", *app)
	}
	if app.Full != nil && !slices.Equal(app.Full.Devices, []string{"/b/app_1.bak", "/b/app_2.bak"}) {
		t.Errorf("app full devices = %v, want both stripes", app.Full.Devices)
	}
	if master.Differential == nil || master.Full != nil || master.Differential.BackupSetID != 14 {
		t.Errorf("master = %+v", *master)
	}
	if never.DatabaseName != "never" || never.Full != nil || never.Differential != nil || never.Log != nil || never.RecoveryModel != "" {
		t.Errorf("never = %+v", *never)
	}
}

// Each filter field becomes its predicate, and the parameters bind in the
// order the predicates number them.
func TestSearchJobHistoryFilter(t *testing.T) {
	d := qsRecDB(t, 17, anonCols(8), [][]driver.Value{
		{"nightly", int64(20261029), int64(230501), int64(10203), int64(0), "failed", int64(0), "(Job outcome)"},
	})
	since := time.Date(2026, 10, 29, 23, 0, 0, 0, time.UTC)
	until := time.Date(2026, 10, 30, 6, 30, 15, 0, time.UTC)
	got, err := d.server.SearchJobHistory(context.Background(), JobHistoryFilter{
		JobName: "nightly", Since: since, Until: until,
		Outcomes: []JobOutcome{JobOutcomeFailed, JobOutcomeCancelled}, Max: 7,
	})
	if err != nil {
		t.Fatalf("SearchJobHistory: %v", err)
	}
	c := qsRec.last(t)
	for _, frag := range []string{
		"SELECT TOP 7",
		"h.step_id = 0",
		"j.name = @p1",
		"(h.run_date > @p2 OR (h.run_date = @p2 AND h.run_time >= @p3))",
		"(h.run_date < @p4 OR (h.run_date = @p4 AND h.run_time <= @p5))",
		"h.run_status IN (@p6, @p7)",
	} {
		if !strings.Contains(c.sql, frag) {
			t.Errorf("query lacks %q:\n%s", frag, c.sql)
		}
	}
	if want := []any{"nightly", int64(20261029), int64(230000), int64(20261030), int64(63015), int64(0), int64(3)}; !slices.Equal(c.args, want) {
		t.Errorf("args = %v, want %v", c.args, want)
	}
	if len(got) != 1 || got[0].JobName != "nightly" || got[0].Outcome != JobOutcomeFailed ||
		!got[0].RunDate.Equal(time.Date(2026, 10, 29, 23, 5, 1, 0, time.UTC)) || got[0].Duration != time.Hour+2*time.Minute+3*time.Second {
		t.Errorf("row = %+v", got)
	}

	// The zero filter is JobHistory's read: outcome rows only, 100 of them,
	// no parameter.
	if _, err := d.server.JobHistory(context.Background(), 0); err != nil {
		t.Fatalf("JobHistory: %v", err)
	}
	c = qsRec.last(t)
	if !strings.Contains(c.sql, "SELECT TOP 100") || !strings.Contains(c.sql, "WHERE  h.step_id = 0\n") || len(c.args) != 0 {
		t.Errorf("JobHistory query:\n%s\nargs %v", c.sql, c.args)
	}
	// IncludeSteps drops the step filter.
	if _, err := d.server.SearchJobHistory(context.Background(), JobHistoryFilter{IncludeSteps: true}); err != nil {
		t.Fatalf("SearchJobHistory: %v", err)
	}
	if c = qsRec.last(t); strings.Contains(c.sql, "step_id = 0") || strings.Contains(c.sql, "WHERE") {
		t.Errorf("IncludeSteps query still filters:\n%s", c.sql)
	}
}

func TestSQLAgentDateTimeInvertsParse(t *testing.T) {
	for _, tm := range []time.Time{
		time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		time.Date(1999, 12, 31, 23, 59, 59, 0, time.UTC),
	} {
		d, c := sqlAgentDateTime(tm)
		if back := parseSQLAgentDate(d, c); !back.Equal(tm) {
			t.Errorf("%v → %d %d → %v", tm, d, c, back)
		}
	}
}

// anonCols names n columns for a fake result read by position.
func anonCols(n int) []string {
	cols := make([]string, n)
	for i := range cols {
		cols[i] = "c" + strconv.Itoa(i)
	}
	return cols
}
