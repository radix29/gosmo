//go:build livedb

// Live verification of the maintenance reads and writes gossms phase 8.a
// added: Database.CheckDB against a clean and a deliberately corrupted
// database, Database.FragmentationStats against Table.FragmentationStats,
// the ONLINE/MAXDOP rebuild, sp_updatestats, Server.LastBackups and
// Server.SearchJobHistory.
//
//	go test -tags livedb . -run TestLiveMaintenance -v \
//	  -livedb 'sqlserver://sa:PASS@host?TrustServerCertificate=true'
//
// Creates and drops gosmo_p8a_maint and gosmo_p8a_never, and the job
// gosmo_p8a_failing; deletes msdb's backup history for those two names only.
// The backup file is left beside the server's data files (overwritten by the
// next run), as live_recoverystatus_test.go leaves its own.
package gosmo

import (
	"context"
	"database/sql"
	"slices"
	"strings"
	"testing"
	"time"
)

const liveMaintDB = "gosmo_p8a_maint"

// liveMaintFixture is a FULL-recovery scratch database holding dbo.t, whose
// nonclustered index ix_v is keyed on random values, half of them inserted
// after it was built, so it is left fragmented.
func liveMaintFixture(t *testing.T, db *sql.DB, ctx context.Context) (*Database, *Table) {
	t.Helper()
	d, drop := liveScratchDB(t, db, ctx, liveMaintDB)
	t.Cleanup(drop)
	liveExecIn(t, d, ctx,
		"ALTER DATABASE CURRENT SET RECOVERY FULL",
		"CREATE TABLE dbo.t (id int IDENTITY PRIMARY KEY, v char(300) NOT NULL)",
		// Half the rows before the index, half after: the second half's
		// keys land between the first's, splitting the full pages.
		`INSERT dbo.t (v) SELECT TOP 3000 CONVERT(char(36), NEWID())
		 FROM sys.all_objects a CROSS JOIN sys.all_objects b`,
		"CREATE INDEX ix_v ON dbo.t (v)",
		`INSERT dbo.t (v) SELECT TOP 3000 CONVERT(char(36), NEWID())
		 FROM sys.all_objects a CROSS JOIN sys.all_objects b`)
	tbl, err := d.TableByName(ctx, "dbo", "t")
	if err != nil {
		t.Fatalf("TableByName: %v", err)
	}
	return d, tbl
}

func TestLiveMaintenanceFragmentationAndRebuild(t *testing.T) {
	db, ctx, done := liveDBTimeout(t, 3*time.Minute)
	t.Cleanup(done)
	d, tbl := liveMaintFixture(t, db, ctx)

	ixFrag := func(t *testing.T) *IndexFragmentation {
		t.Helper()
		rows, err := tbl.FragmentationStats(ctx, FragmentationSampled)
		if err != nil {
			t.Fatalf("Table.FragmentationStats: %v", err)
		}
		for _, r := range rows {
			if r.IndexName == "ix_v" {
				return r
			}
		}
		t.Fatal("Table.FragmentationStats has no ix_v row")
		return nil
	}
	want := ixFrag(t)
	if want.AvgFragmentationPct < 30 || want.PageCount < 100 {
		t.Fatalf("fixture not fragmented enough to prove anything: %+v", *want)
	}

	all, err := d.FragmentationStats(ctx, FragmentationSampled, 0, 0)
	if err != nil {
		t.Fatalf("Database.FragmentationStats: %v", err)
	}
	i := slices.IndexFunc(all, func(r *DatabaseIndexFragmentation) bool { return r.IndexName == "ix_v" })
	if i < 0 {
		t.Fatalf("Database.FragmentationStats has no ix_v row among %d", len(all))
	}
	got := all[i]
	if got.IndexFragmentation != *want {
		t.Errorf("database-wide row differs from the table's:\n got %+v\nwant %+v", got.IndexFragmentation, *want)
	}
	if got.Schema != "dbo" || got.ObjectName != "t" || got.ObjectID != tbl.ObjectID || got.IsView ||
		got.IndexType != IndexTypeNonClustered || got.PartitionCount != 1 {
		t.Errorf("ix_v row = %+v", *got)
	}
	if !slices.IsSortedFunc(all, func(a, b *DatabaseIndexFragmentation) int {
		switch {
		case a.AvgFragmentationPct > b.AvgFragmentationPct:
			return -1
		case a.AvgFragmentationPct < b.AvgFragmentationPct:
			return 1
		}
		return 0
	}) {
		t.Error("rows are not most fragmented first")
	}
	// The thresholds filter on the server.
	if rows, err := d.FragmentationStats(ctx, FragmentationSampled, 0, want.PageCount+1); err != nil ||
		slices.ContainsFunc(rows, func(r *DatabaseIndexFragmentation) bool { return r.IndexName == "ix_v" }) {
		t.Errorf("minPages above ix_v's pages still returns it (err %v)", err)
	}
	if rows, err := d.FragmentationStats(ctx, FragmentationLimited, 100, 0); err != nil || len(rows) != 0 {
		t.Errorf("minPct 100 returns %d rows (err %v)", len(rows), err)
	}

	// ONLINE needs Enterprise or Developer — every instance this runs on.
	if err := tbl.IndexRef("ix_v").Rebuild(ctx, IndexRebuildOptions{Online: true, MaxDOP: 1}); err != nil {
		t.Fatalf("online rebuild of ix_v: %v", err)
	}
	if after := ixFrag(t); after.AvgFragmentationPct >= want.AvgFragmentationPct/2 {
		t.Errorf("ix_v still %.1f%% fragmented after a rebuild (was %.1f%%)", after.AvgFragmentationPct, want.AvgFragmentationPct)
	}
	if err := tbl.RebuildAllIndexesWith(ctx, IndexRebuildOptions{Online: true, MaxDOP: 2, FillFactor: 90, DataCompression: DataCompressionPage}); err != nil {
		t.Fatalf("RebuildAllIndexesWith: %v", err)
	}
	var fill int
	var comp string
	if err := d.queryRow(ctx, func(r *sql.Row) error { return r.Scan(&fill, &comp) }, `
SELECT i.fill_factor, p.data_compression_desc FROM sys.indexes i
JOIN sys.partitions p ON p.object_id = i.object_id AND p.index_id = i.index_id
WHERE i.object_id = OBJECT_ID(N'dbo.t') AND i.name = N'ix_v'`); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if fill != 90 || comp != "PAGE" {
		t.Errorf("after RebuildAllIndexesWith ix_v has fill factor %d, compression %s; want 90, PAGE", fill, comp)
	}
}

func TestLiveMaintenanceUpdateStatistics(t *testing.T) {
	db, ctx, done := liveDBTimeout(t, 3*time.Minute)
	t.Cleanup(done)
	d, _ := liveMaintFixture(t, db, ctx)

	statsDate := func(t *testing.T) time.Time {
		t.Helper()
		var at sql.NullTime
		if err := d.queryRow(ctx, func(r *sql.Row) error { return r.Scan(&at) },
			`SELECT STATS_DATE(OBJECT_ID(N'dbo.t'), INDEXPROPERTY(OBJECT_ID(N'dbo.t'), N'ix_v', 'IndexID'))`); err != nil {
			t.Fatalf("STATS_DATE: %v", err)
		}
		return at.Time
	}
	for _, resample := range []bool{false, true} {
		// A modification since the last update is what sp_updatestats
		// looks for; a statistic without one is skipped.
		liveExecIn(t, d, ctx, "UPDATE TOP (500) dbo.t SET v = CONVERT(char(36), NEWID())")
		before := statsDate(t)
		time.Sleep(1100 * time.Millisecond)
		if err := d.UpdateStatistics(ctx, resample); err != nil {
			t.Fatalf("UpdateStatistics(%v): %v", resample, err)
		}
		if after := statsDate(t); !after.After(before) {
			t.Errorf("UpdateStatistics(%v): STATS_DATE %v did not move from %v", resample, after, before)
		}
	}
}

func TestLiveMaintenanceCheckDB(t *testing.T) {
	db, ctx, done := liveDBTimeout(t, 5*time.Minute)
	t.Cleanup(done)
	d, tbl := liveMaintFixture(t, db, ctx)

	// Every option form the server accepts, on a clean database: no rows.
	for _, opts := range []CheckDBOptions{
		{},
		{PhysicalOnly: true, NoIndex: true, MaxDOP: 1},
		{ExtendedLogicalChecks: true, DataPurity: true, MaxDOP: 2},
	} {
		rows, err := d.CheckDB(ctx, opts)
		if err != nil {
			t.Fatalf("CheckDB(%+v) on a clean database: %v", opts, err)
		}
		if len(rows) != 0 {
			t.Errorf("CheckDB(%+v) on a clean database: %d rows, first %+v", opts, len(rows), *rows[0])
		}
	}
	// The two combinations CheckDB refuses itself are the server's refusals
	// too — so the refusal narrows nothing.
	for _, with := range []string{"PHYSICAL_ONLY, DATA_PURITY", "PHYSICAL_ONLY, EXTENDED_LOGICAL_CHECKS"} {
		if _, err := db.ExecContext(ctx, "DBCC CHECKDB (N'"+liveMaintDB+"') WITH NO_INFOMSGS, "+with); err == nil {
			t.Errorf("the server accepts WITH %s, which CheckDB refuses", with)
		}
	}

	// Corrupt one leaf page of ix_v: DBCC WRITEPAGE, with the database in
	// SINGLE_USER, on the one session that holds it.
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("conn: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	var file, page int
	if err := conn.QueryRowContext(ctx, `USE `+QuoteName(liveMaintDB)+`;
SELECT TOP 1 allocated_page_file_id, allocated_page_page_id
FROM sys.dm_db_database_page_allocations(DB_ID(), OBJECT_ID(N'dbo.t'), INDEXPROPERTY(OBJECT_ID(N'dbo.t'), N'ix_v', 'IndexID'), NULL, 'DETAILED')
WHERE page_type = 2 AND is_allocated = 1 ORDER BY allocated_page_page_id`).Scan(&file, &page); err != nil {
		t.Fatalf("find an index page: %v", err)
	}
	for _, q := range []string{
		"USE master",
		"ALTER DATABASE " + QuoteName(liveMaintDB) + " SET SINGLE_USER WITH ROLLBACK IMMEDIATE",
		"DBCC WRITEPAGE (" + QuoteLiteral(liveMaintDB) + ", @p1, @p2, 100, 4, 0x41424344, 1)",
		"ALTER DATABASE " + QuoteName(liveMaintDB) + " SET MULTI_USER",
	} {
		args := []any{}
		if strings.HasPrefix(q, "DBCC") {
			args = []any{file, page}
		}
		if _, err := conn.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}

	rows, err := d.CheckDB(ctx, CheckDBOptions{})
	if err != nil {
		t.Fatalf("CheckDB on the corrupted database: %v", err)
	}
	var errs, summaries int
	for _, r := range rows {
		t.Logf("%d level %d %s: %.100s", r.Error, r.Level, r.RepairLevel, r.Message)
		if r.IsSummary() {
			summaries++
			continue
		}
		errs++
		if r.ObjectID != tbl.ObjectID || r.RepairLevel == "" {
			t.Errorf("error row %d names object %d (want %d), repair %q", r.Error, r.ObjectID, tbl.ObjectID, r.RepairLevel)
		}
	}
	if errs == 0 || summaries == 0 {
		t.Fatalf("CheckDB on the corrupted database: %d errors, %d summaries; want both", errs, summaries)
	}
	if !slices.ContainsFunc(rows, func(r *CheckDBError) bool { return r.File == file && r.Page == page }) {
		t.Errorf("no row names the corrupted page (%d:%d)", file, page)
	}
	if !slices.ContainsFunc(rows, func(r *CheckDBError) bool { return r.Error == 8989 }) {
		t.Error("no 8989 database summary")
	}
}

func TestLiveMaintenanceLastBackups(t *testing.T) {
	db, ctx, done := liveDBTimeout(t, 3*time.Minute)
	t.Cleanup(done)
	d, _ := liveMaintFixture(t, db, ctx)
	_, dropNever := liveScratchDB(t, db, ctx, "gosmo_p8a_never")
	t.Cleanup(dropNever)
	srv := d.Server()
	forget := func() {
		for _, n := range []string{liveMaintDB, "gosmo_p8a_never"} {
			db.ExecContext(context.Background(), "EXEC msdb.dbo.sp_delete_database_backuphistory @database_name = @p1", n)
		}
	}
	forget()
	t.Cleanup(forget)

	device := liveBackupPath(t, srv, ctx, liveMaintDB+".bak")
	if err := srv.Backup(ctx, BackupOptions{Database: liveMaintDB, Devices: []BackupTarget{DiskTarget(device)}, Init: true}); err != nil {
		t.Fatalf("full backup: %v", err)
	}
	if err := srv.Backup(ctx, BackupOptions{Database: liveMaintDB, Action: BackupActionLog, Devices: []BackupTarget{DiskTarget(device)}}); err != nil {
		t.Fatalf("log backup: %v", err)
	}
	hist, err := srv.BackupHistory(ctx, liveMaintDB)
	if err != nil || len(hist) != 2 {
		t.Fatalf("BackupHistory: %d sets, %v", len(hist), err)
	}

	last, err := srv.LastBackups(ctx)
	if err != nil {
		t.Fatalf("LastBackups: %v", err)
	}
	byName := map[string]*DatabaseLastBackups{}
	for _, l := range last {
		byName[l.DatabaseName] = l
	}
	m := byName[liveMaintDB]
	if m == nil {
		t.Fatalf("LastBackups has no %s row", liveMaintDB)
	}
	if m.RecoveryModel != RecoveryModelFull || m.Full == nil || m.Log == nil || m.Differential != nil {
		t.Fatalf("%s = %+v", liveMaintDB, *m)
	}
	// The same sets BackupHistory reads, field for field.
	for _, h := range hist {
		got := m.Full
		if h.SetType == BackupSetLog {
			got = m.Log
		}
		if got.BackupSetID != h.BackupSetID || got.DeviceName != device || !got.BackupFinish.Equal(h.BackupFinish) || got.LastLSN != h.LastLSN {
			t.Errorf("%s backup: LastBackups %+v, BackupHistory %+v", h.SetType, *got, *h)
		}
	}
	n := byName["gosmo_p8a_never"]
	if n == nil || n.Full != nil || n.Differential != nil || n.Log != nil || n.RecoveryModel == "" {
		t.Errorf("never-backed-up database: %+v", n)
	}
	if byName["tempdb"] != nil {
		t.Error("tempdb is listed")
	}
}

func TestLiveMaintenanceSearchJobHistory(t *testing.T) {
	db, ctx, done := liveDBTimeout(t, 2*time.Minute)
	t.Cleanup(done)
	srv, err := NewServer(ctx, db)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	if st, err := srv.AgentInfo(ctx); err != nil || !st.Running {
		t.Skipf("Agent not running here (%v)", err)
	}
	const name = "gosmo_p8a_failing"
	drop := func() {
		srv.exec(context.Background(), "IF EXISTS (SELECT 1 FROM msdb.dbo.sysjobs WHERE name = @p1) EXEC msdb.dbo.sp_delete_job @job_name = @p1", name)
	}
	drop()
	t.Cleanup(drop)
	if _, err := srv.CreateJob(ctx, CreateJobRequest{Name: name, Enabled: true}); err != nil {
		t.Fatalf("create job: %v", err)
	}
	j, err := srv.JobByName(ctx, name)
	if err != nil {
		t.Fatalf("JobByName: %v", err)
	}
	if _, err := j.AddStep(ctx, JobStepDefinition{
		Name: "fail", Subsystem: "TSQL", Command: "RAISERROR(N'gosmo p8a failure', 16, 1)",
		OnSuccessAction: 1, OnFailAction: 2,
	}); err != nil {
		t.Fatalf("add step: %v", err)
	}
	now, err := srv.CurrentTime(ctx)
	if err != nil {
		t.Fatalf("CurrentTime: %v", err)
	}
	// The server's wall clock, stamped UTC as RunDate is.
	since := time.Date(now.Year(), now.Month(), now.Day(), now.Hour(), now.Minute(), now.Second(), 0, time.UTC).Add(-time.Second)
	if err := j.Start(ctx, ""); err != nil {
		t.Fatalf("start: %v", err)
	}

	failed := JobHistoryFilter{JobName: name, Since: since, Outcomes: []JobOutcome{JobOutcomeFailed}}
	var got []*JobHistoryEntry
	for deadline := time.Now().Add(45 * time.Second); time.Now().Before(deadline); time.Sleep(time.Second) {
		if got, err = srv.SearchJobHistory(ctx, failed); err != nil {
			t.Fatalf("SearchJobHistory: %v", err)
		}
		if len(got) > 0 {
			break
		}
	}
	if len(got) != 1 || got[0].StepID != 0 || got[0].JobName != name || got[0].Outcome != JobOutcomeFailed {
		t.Fatalf("failed outcome rows = %+v", got)
	}
	if got[0].RunDate.Before(since) {
		t.Errorf("RunDate %v is before Since %v", got[0].RunDate, since)
	}

	// The outcome row and the step row, in either order: the outcome row
	// carries the run's start, and a step that started a second later sorts
	// above it.
	steps, err := srv.SearchJobHistory(ctx, JobHistoryFilter{JobName: name, IncludeSteps: true})
	if err != nil || len(steps) != 2 ||
		!slices.ContainsFunc(steps, func(h *JobHistoryEntry) bool { return h.StepID == 0 }) ||
		!slices.ContainsFunc(steps, func(h *JobHistoryEntry) bool {
			return h.StepID == 1 && strings.Contains(h.Message, "gosmo p8a failure")
		}) {
		for _, s := range steps {
			t.Logf("%+v", *s)
		}
		t.Errorf("IncludeSteps: %d rows (err %v)", len(steps), err)
	}
	for name, f := range map[string]JobHistoryFilter{
		"succeeded only":     {JobName: name, Outcomes: []JobOutcome{JobOutcomeSucceeded}},
		"since after run":    {JobName: name, Since: got[0].RunDate.Add(time.Hour)},
		"until before run":   {JobName: name, Until: got[0].RunDate.Add(-time.Second)},
		"another job's name": {JobName: name + "_x"},
	} {
		if rows, err := srv.SearchJobHistory(ctx, f); err != nil || len(rows) != 0 {
			t.Errorf("%s: %d rows (err %v), want none", name, len(rows), err)
		}
	}
	// Until at the run's own start keeps it: the bound is inclusive.
	if rows, err := srv.SearchJobHistory(ctx, JobHistoryFilter{JobName: name, Since: got[0].RunDate, Until: got[0].RunDate}); err != nil || len(rows) != 1 {
		t.Errorf("Since = Until = RunDate: %d rows (err %v), want 1", len(rows), err)
	}
}
