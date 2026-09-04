//go:build livedb

// Live verification of the two paths a job's CurrentState can come from:
// Agent's own in-memory state, read through master.dbo.xp_sqlagent_enum_jobs
// by Server.jobStates, and the msdb.dbo.sysjobactivity derivation Jobs /
// JobByName fall back to when that read fails.
//
// The fallback is the half no unit test can reach: it exists for an instance
// whose Agent is stopped, and the extended procedure is only unavailable when
// Agent really is down. Both paths emit the same encoding (1 Executing,
// 4 Idle), so getting the fallback wrong is invisible until an Agent outage.
//
// Three steps, each its own flag so none can pass silently:
//
//  1. Agent running — the overlay path, and the sysjobactivity row shapes the
//     fallback reads:
//     go test -tags livedb . -run TestLiveJobStateFromAgent -v \
//     -livedb 'sqlserver://sa:PASS@ubusql1.fritz.box?TrustServerCertificate=true'
//
//  2. Arm the fallback: leave a job mid-run, then stop Agent out of band
//     (Linux: mssql-conf set sqlagent.enabled false; systemctl restart
//     mssql-server. Windows: net stop SQLSERVERAGENT):
//     go test -tags livedb . -run TestLiveJobStateArm -v -livedb '...' -live-agent-arm
//
//  3. Agent stopped — the fallback itself:
//     go test -tags livedb . -run TestLiveJobStateFallback -v -livedb '...' -live-agent-stopped
//
// Step 3 drops what step 2 created. Each step creates and drops its own jobs
// and touches nothing else.
package gosmo

import (
	"context"
	"flag"
	"strings"
	"testing"
	"time"
)

var (
	liveAgentArm = flag.Bool("live-agent-arm", false,
		"create a long-running job and leave it running, for the -live-agent-stopped step")
	liveAgentStopped = flag.Bool("live-agent-stopped", false,
		"SQL Server Agent on -livedb is stopped (see TestLiveJobStateFallbackWhenAgentIsStopped)")
)

const (
	// Three names, not two: the armed job outlives its process and step 1 is
	// re-runnable, so they must not collide — step 1's own drop deleted the
	// armed fixture the first time this was run.
	jobStateAgentJob   = "gossms_live_jobstate_agent"
	jobStateRunningJob = "gossms_live_jobstate_running"
	jobStateIdleJob    = "gossms_live_jobstate_idle"
)

// liveStateJob creates a single-step job and returns it with a dropper. The
// step is a WAITFOR of the given duration, so the job is long-running or
// instant depending on it.
func liveStateJob(t *testing.T, srv *Server, ctx context.Context, name, wait string) (*Job, func()) {
	t.Helper()
	drop := func() {
		srv.execContext(context.Background(),
			"IF EXISTS (SELECT 1 FROM msdb.dbo.sysjobs WHERE name = N'"+name+"') "+
				"EXEC msdb.dbo.sp_delete_job @job_name = N'"+name+"'")
	}
	drop()
	if _, err := srv.CreateJobContext(ctx, CreateJobRequest{Name: name, Enabled: true}); err != nil {
		t.Fatalf("create job %s: %v", name, err)
	}
	j, err := srv.JobByNameContext(ctx, name)
	if err != nil {
		drop()
		t.Fatalf("job by name %s: %v", name, err)
	}
	if err := j.AddStepContext(ctx, JobStepRequest{
		Name: "wait", Subsystem: "TSQL", Command: "WAITFOR DELAY '" + wait + "'",
		OnSuccessAction: 1, OnFailAction: 2,
	}); err != nil {
		drop()
		t.Fatalf("add step to %s: %v", name, err)
	}
	return j, drop
}

// waitForState polls JobByName until the job reports want, and reports what it
// last saw if it never does.
func waitForState(t *testing.T, srv *Server, ctx context.Context, name string, want JobState) JobState {
	t.Helper()
	var got JobState
	deadline := time.Now().Add(20 * time.Second)
	for {
		j, err := srv.JobByNameContext(ctx, name)
		if err != nil {
			t.Fatalf("job by name %s: %v", name, err)
		}
		got = j.CurrentState
		if got == want || time.Now().After(deadline) {
			return got
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// TestLiveJobStateFromAgent is the overlay path, and it also pins the two
// sysjobactivity columns the fallback derives from: a job Agent reports as
// executing has start_execution_date set and stop_execution_date NULL, which
// is the whole content of the CASE. Without that second half the fallback is
// asserted against nothing but its own arithmetic.
func TestLiveJobStateFromAgent(t *testing.T) {
	if *liveAgentStopped {
		t.Skip("-live-agent-stopped: this step needs a running Agent")
	}
	db, ctx, done := liveDB(t)
	defer done()
	srv := &Server{db: db}

	st, err := srv.AgentInfoContext(ctx)
	if err != nil {
		t.Fatalf("agent info: %v", err)
	}
	if !st.Running {
		t.Skipf("Agent is %s on this instance; this step needs it running", st.StatusText)
	}

	j, drop := liveStateJob(t, srv, ctx, jobStateAgentJob, "00:00:30")
	defer drop()

	// A job that has never run: msdb has no activity row at all, so both the
	// overlay and the derivation have to answer Idle.
	if got := waitForState(t, srv, ctx, j.Name, JobStateIdle); got != JobStateIdle {
		t.Fatalf("a job that has never run reports state %d, want %d (idle)", got, JobStateIdle)
	}

	if err := j.StartContext(ctx, ""); err != nil {
		t.Fatalf("start job: %v", err)
	}
	defer j.StopContext(context.Background())

	if got := waitForState(t, srv, ctx, j.Name, JobStateExecuting); got != JobStateExecuting {
		t.Fatalf("a running job reports state %d, want %d (executing)", got, JobStateExecuting)
	}
	// The state really came from Agent, not from the derivation: jobStates
	// covers this job.
	states, err := srv.jobStates(ctx)
	if err != nil {
		t.Fatalf("jobStates with Agent running: %v — the overlay is unavailable, so the assertion above was the fallback's answer", err)
	}
	if got, ok := states[strings.ToLower(j.JobID)]; !ok || got != JobStateExecuting {
		t.Errorf("xp_sqlagent_enum_jobs reports state %d (present %v) for the running job, want %d", got, ok, JobStateExecuting)
	}

	// The fallback's two inputs, read while Agent says the job is executing.
	var started, stopped any
	row := db.QueryRowContext(ctx, `
SELECT ja.start_execution_date, ja.stop_execution_date
FROM   msdb.dbo.sysjobactivity ja
WHERE  ja.job_id = @p1
       AND ja.session_id = (SELECT MAX(session_id) FROM msdb.dbo.sysjobactivity)`, j.JobID)
	if err := row.Scan(&started, &stopped); err != nil {
		t.Fatalf("reading the activity row the fallback derives from: %v", err)
	}
	if started == nil || stopped != nil {
		t.Errorf("activity row for a running job = started %v stopped %v, want a start and no stop — the fallback's CASE reads exactly these", started, stopped)
	}

	if err := j.StopContext(ctx); err != nil {
		t.Fatalf("stop job: %v", err)
	}
	if got := waitForState(t, srv, ctx, j.Name, JobStateIdle); got != JobStateIdle {
		t.Errorf("a stopped job reports state %d, want %d (idle)", got, JobStateIdle)
	}

	// Which of the two paths JobByName actually reports is invisible while
	// they agree, and on a healthy instance they always do. Make them
	// disagree on this throwaway job alone — clear the stop date its finished
	// run left, so the derivation reads it as executing while Agent reports
	// idle — and the overlay has to win. Without it the assertions above pass
	// with applyJobStates deleted.
	if _, err := db.ExecContext(ctx, `
UPDATE msdb.dbo.sysjobactivity SET stop_execution_date = NULL
WHERE  job_id = @p1 AND stop_execution_date IS NOT NULL`, j.JobID); err != nil {
		t.Fatalf("clearing the stop date on the throwaway job's activity row: %v", err)
	}
	stale, err := srv.JobByNameContext(ctx, j.Name)
	if err != nil {
		t.Fatalf("job by name: %v", err)
	}
	if stale.CurrentState != JobStateIdle {
		t.Errorf("with a stale activity row the job reports state %d, want %d (idle) — Agent's answer must win over the derivation",
			stale.CurrentState, JobStateIdle)
	}
}

// TestLiveJobStateArmsARunningJob leaves jobStateRunningJob mid-run so Agent
// can be stopped underneath it. It deliberately does not drop the job —
// TestLiveJobStateFallbackWhenAgentIsStopped does, after reading it.
func TestLiveJobStateArmsARunningJob(t *testing.T) {
	if !*liveAgentArm {
		t.Skip("no -live-agent-arm")
	}
	db, ctx, done := liveDB(t)
	defer done()
	srv := &Server{db: db}

	j, _ := liveStateJob(t, srv, ctx, jobStateRunningJob, "04:00:00")
	if err := j.StartContext(ctx, ""); err != nil {
		t.Fatalf("start job: %v", err)
	}
	if got := waitForState(t, srv, ctx, j.Name, JobStateExecuting); got != JobStateExecuting {
		t.Fatalf("the armed job reports state %d, want %d (executing) before Agent is stopped", got, JobStateExecuting)
	}
	t.Logf("%s is running; stop Agent now, then run -run TestLiveJobStateFallback -live-agent-stopped", j.Name)
}

// TestLiveJobStateFallbackWhenAgentIsStopped is the item this file exists for.
// With Agent down, xp_sqlagent_enum_jobs is unreachable, so applyJobStates
// swallows its error and the listing keeps the sysjobactivity derivation —
// which must still list every job and must still tell a job interrupted
// mid-run from an idle one.
func TestLiveJobStateFallbackWhenAgentIsStopped(t *testing.T) {
	if !*liveAgentStopped {
		t.Skip("no -live-agent-stopped; Agent is running")
	}
	db, ctx, done := liveDB(t)
	defer done()
	srv := &Server{db: db}

	// The guard: with Agent up this test would pass on the overlay and prove
	// nothing about the fallback.
	st, err := srv.AgentInfoContext(ctx)
	if err != nil {
		t.Fatalf("agent info: %v", err)
	}
	if st.Running {
		t.Fatalf("Agent is running (%s); -live-agent-stopped was given against a live Agent", st.StatusText)
	}
	// How the overlay actually fails with Agent down, measured here rather
	// than assumed: xp_sqlagent_enum_jobs still runs and returns *no rows*,
	// so jobStates hands back an empty map with no error and applyJobStates
	// overlays nothing. The error path stays for the other reason it exists
	// (a login with neither sysadmin nor SQLAgentReaderRole), but this is the
	// one an Agent outage takes.
	states, err := srv.jobStates(ctx)
	if err != nil {
		t.Logf("jobStates with Agent stopped: %v", err)
	} else if len(states) != 0 {
		t.Fatalf("jobStates returned %d states with Agent stopped; the overlay is still answering, so nothing below tests the fallback", len(states))
	}

	// A job created now has never run, and Agent is not there to run it: the
	// derivation's ELSE arm.
	idle, dropIdle := liveStateJob(t, srv, ctx, jobStateIdleJob, "00:00:05")
	defer dropIdle()
	got, err := srv.JobByNameContext(ctx, idle.Name)
	if err != nil {
		t.Fatalf("job by name %s with Agent stopped: %v", idle.Name, err)
	}
	if got.CurrentState != JobStateIdle {
		t.Errorf("%s reports state %d with Agent stopped, want %d (idle)", idle.Name, got.CurrentState, JobStateIdle)
	}

	// And the job Agent was running when it stopped: its activity row still
	// has a start and no stop, which is the derivation's THEN arm.
	running, err := srv.JobByNameContext(ctx, jobStateRunningJob)
	if err != nil {
		t.Fatalf("job by name %s: %v — run -live-agent-arm before stopping Agent", jobStateRunningJob, err)
	}
	defer srv.execContext(context.Background(),
		"IF EXISTS (SELECT 1 FROM msdb.dbo.sysjobs WHERE name = N'"+jobStateRunningJob+"') "+
			"EXEC msdb.dbo.sp_delete_job @job_name = N'"+jobStateRunningJob+"'")
	if running.CurrentState != JobStateExecuting {
		t.Errorf("%s reports state %d with Agent stopped, want %d (executing) — it was mid-run when Agent went down",
			jobStateRunningJob, running.CurrentState, JobStateExecuting)
	}

	// The listing as a whole still works: a failed state read is not a failed
	// listing, and the two jobs above must both be in it with the same states.
	jobs, err := srv.JobsContext(ctx)
	if err != nil {
		t.Fatalf("Jobs with Agent stopped: %v — applyJobStates turned a failed overlay into a failed listing", err)
	}
	seen := map[string]JobState{}
	for _, j := range jobs {
		seen[j.Name] = j.CurrentState
	}
	if s, ok := seen[jobStateRunningJob]; !ok || s != JobStateExecuting {
		t.Errorf("Jobs reports %s as state %d (present %v), want %d", jobStateRunningJob, s, ok, JobStateExecuting)
	}
	if s, ok := seen[jobStateIdleJob]; !ok || s != JobStateIdle {
		t.Errorf("Jobs reports %s as state %d (present %v), want %d", jobStateIdleJob, s, ok, JobStateIdle)
	}
}
