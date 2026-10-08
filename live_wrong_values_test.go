//go:build livedb

// Live checks for a batch of wrong-value reads: file sizes widened to bigint,
// filegroup files reported in KB, a job's last run read from sysjobservers,
// an index left enabled after Enable, and the history tiebreak.
//
//	go test -tags livedb . -run TestLiveWrongValues -v \
//	  -livedb 'sqlserver://sa:PASS@host?TrustServerCertificate=true'
//
// Creates and drops its own databases and job; touches nothing else. The job
// check needs SQL Server Agent running.
package gosmo

import (
	"context"
	"testing"
	"time"
)

// Files, FileGroups, DatabaseFiles, SpaceUsed and DiskUsage all run with the
// bigint casts, and FileGroups reports max size and growth in the same KB as
// Files.
func TestLiveWrongValuesFileUnits(t *testing.T) {
	db, ctx, done := liveDB(t)
	t.Cleanup(done)
	d, drop := liveScratchDB(t, db, ctx, "gosmo_wrong_values_files")
	t.Cleanup(drop)

	if _, err := db.ExecContext(ctx, `ALTER DATABASE [gosmo_wrong_values_files]
MODIFY FILE (NAME = N'gosmo_wrong_values_files', MAXSIZE = 100MB, FILEGROWTH = 64MB)`); err != nil {
		t.Fatalf("modify file: %v", err)
	}

	files, err := d.Files(ctx)
	if err != nil {
		t.Fatalf("Files: %v", err)
	}
	if _, err := d.server.DatabaseFiles(ctx, d.Name); err != nil {
		t.Fatalf("DatabaseFiles: %v", err)
	}
	if _, err := d.SpaceUsed(ctx); err != nil {
		t.Fatalf("SpaceUsed: %v", err)
	}
	if _, err := d.DiskUsage(ctx); err != nil {
		t.Fatalf("DiskUsage: %v", err)
	}
	fgs, err := d.FileGroups(ctx)
	if err != nil {
		t.Fatalf("FileGroups: %v", err)
	}

	var info *DatabaseFileInfo
	for _, f := range files {
		if f.Name == "gosmo_wrong_values_files" {
			info = f
		}
	}
	var file *DatabaseFileInfo
	for _, fg := range fgs {
		for _, f := range fg.Files {
			if f.Name == "gosmo_wrong_values_files" {
				file = f
			}
		}
	}
	if info == nil || file == nil {
		t.Fatalf("data file missing: Files %v, FileGroups %v", info, file)
	}
	if info.MaxSizeKB != 100*1024 || info.GrowthKB != 64*1024 {
		t.Errorf("Files: MaxSizeKB %d, GrowthKB %d; want 102400, 65536", info.MaxSizeKB, info.GrowthKB)
	}
	// One type now, so the file FileGroups returns must equal the one Files
	// returns field for field.
	if *file != *info {
		t.Errorf("FileGroups file = %+v, want the Files values %+v", *file, *info)
	}
}

// A two-step job: the first step waits, so the last step starts seconds after
// the job did. LastRunDate is the job's start (sysjobservers), not the last
// step's (sysjobactivity.last_executed_step_date), and History puts the
// step-0 outcome row above step 1, whose start time it shares.
func TestLiveWrongValuesAgentJob(t *testing.T) {
	db, ctx, done := liveDBTimeout(t, 3*time.Minute)
	t.Cleanup(done)
	s, err := NewServer(ctx, db)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	const jobName = "gosmo_wrong_values_job"
	if old, err := s.JobByName(ctx, jobName); err == nil {
		_ = old.Drop(ctx)
	}
	j, err := s.CreateJob(ctx, CreateJobRequest{Name: jobName, Enabled: true, OwnerLogin: "sa"})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	t.Cleanup(func() { _ = s.JobRef(jobName).Drop(context.Background()) })
	if _, err := j.AddStep(ctx, JobStepRequest{Name: "wait", Subsystem: "TSQL",
		Command: "WAITFOR DELAY '00:00:03'", OnSuccessAction: 3, OnFailAction: 2}); err != nil {
		t.Fatalf("AddStep wait: %v", err)
	}
	if _, err := j.AddStep(ctx, JobStepRequest{Name: "done", Subsystem: "TSQL",
		Command: "SELECT 1", OnSuccessAction: 1, OnFailAction: 2}); err != nil {
		t.Fatalf("AddStep done: %v", err)
	}

	if err := j.Start(ctx, ""); err != nil {
		t.Fatalf("Start: %v", err)
	}

	var got *Job
	for deadline := time.Now().Add(60 * time.Second); time.Now().Before(deadline); time.Sleep(time.Second) {
		got, err = s.JobByName(ctx, jobName)
		if err != nil {
			t.Fatalf("JobByName: %v", err)
		}
		if got.LastRunOutcome != JobOutcomeUnknown && got.CurrentState != JobStateExecuting {
			break
		}
	}
	if got.LastRunOutcome != JobOutcomeSucceeded {
		t.Fatalf("job outcome %v, want Succeeded", got.LastRunOutcome)
	}

	var start, lastStep time.Time
	if err := db.QueryRowContext(ctx, `
SELECT ja.start_execution_date, ja.last_executed_step_date
FROM   msdb.dbo.sysjobactivity ja JOIN msdb.dbo.sysjobs j ON j.job_id = ja.job_id
WHERE  j.name = @p1 AND ja.session_id = (SELECT MAX(session_id) FROM msdb.dbo.sysjobactivity)`,
		jobName).Scan(&start, &lastStep); err != nil {
		t.Fatalf("read sysjobactivity: %v", err)
	}
	if lastStep.Sub(start) < 2*time.Second {
		t.Fatalf("fixture: last step started %v after the job, want the 3 s wait between them", lastStep.Sub(start))
	}
	if d := got.LastRunDate.Sub(start.Truncate(time.Second)); d < 0 || d > time.Second {
		t.Errorf("LastRunDate %v, want the job's start %v (last step started %v)", got.LastRunDate, start, lastStep)
	}

	hist, err := got.History(ctx, 0)
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	// Step 2 started three seconds after the job, so it is newest; the
	// outcome row and step 1 share the job's start, and instance_id puts the
	// outcome (written last) above step 1.
	if len(hist) != 3 || hist[0].StepID != 2 || hist[1].StepID != 0 || hist[2].StepID != 1 {
		ids := []int{}
		for _, h := range hist {
			ids = append(ids, h.StepID)
		}
		t.Errorf("History step ids %v, want [2 0 1]", ids)
	}
}

// Disable, Enable, then SetIncludedColumns on the same handle: the index ends
// enabled.
func TestLiveWrongValuesIndexEnableThenInclude(t *testing.T) {
	db, ctx, done := liveDB(t)
	t.Cleanup(done)
	d, drop := liveScratchDB(t, db, ctx, "gosmo_wrong_values_index")
	t.Cleanup(drop)
	liveExecIn(t, d, ctx, "CREATE TABLE dbo.t (id int PRIMARY KEY, a int, b int, c int)")
	tbl, err := d.TableByName(ctx, "dbo", "t")
	if err != nil {
		t.Fatalf("TableByName: %v", err)
	}
	if _, err := tbl.CreateIndex(ctx, CreateIndexRequest{Name: "IX_a", Type: IndexTypeNonClustered,
		KeyColumns: []IndexColumnDef{{Name: "a"}}, IncludedColumns: []string{"b"}}); err != nil {
		t.Fatalf("CreateIndex: %v", err)
	}
	idx, err := tbl.IndexByName(ctx, "IX_a")
	if err != nil {
		t.Fatalf("IndexByName: %v", err)
	}
	if err := idx.Disable(ctx); err != nil {
		t.Fatalf("Disable: %v", err)
	}
	if err := idx.Enable(ctx); err != nil {
		t.Fatalf("Enable: %v", err)
	}
	if err := idx.SetIncludedColumns(ctx, []string{"b", "c"}); err != nil {
		t.Fatalf("SetIncludedColumns: %v", err)
	}
	var disabled bool
	if err := db.QueryRowContext(ctx, `SELECT is_disabled FROM gosmo_wrong_values_index.sys.indexes
WHERE object_id = OBJECT_ID('gosmo_wrong_values_index.dbo.t') AND name = 'IX_a'`).Scan(&disabled); err != nil {
		t.Fatalf("read is_disabled: %v", err)
	}
	if disabled {
		t.Error("IX_a is disabled after Enable then SetIncludedColumns")
	}
}
