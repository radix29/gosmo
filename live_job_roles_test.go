//go:build livedb

package gosmo

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

// TestLiveJobReadsWithoutSysadmin: a member of each msdb Agent role reads
// jobs, steps, history and schedules through the sp_help_* procedures (see
// agentRead) and gets what sysadmin's direct read gets, limited to msdb's own
// visibility — SQLAgentUserRole its own jobs and schedules, Reader and
// Operator every one. Before agentRead every one of these reads was Msg 229
// on sysjobservers, sysjobsteps or sysjobhistory for all three roles.
//
// It creates three logins, an operator, a schedule and two jobs — one sa's,
// one the SQLAgentUserRole login's — runs both once, and drops it all. It
// needs SQL Server Agent running, and skips otherwise.
func TestLiveJobReadsWithoutSysadmin(t *testing.T) {
	skipSlowInShort(t)
	db, ctx, done := liveDBTimeout(t, 3*time.Minute)
	t.Cleanup(done)
	sa := &Server{db: db}
	c := context.Background()

	const (
		pass     = "Pw!gosmo_live_jobroles1"
		prefix   = "gosmo_live_jobroles_"
		saJob    = prefix + "sa_job"
		userJob  = prefix + "user_job"
		schedule = prefix + "sched"
		operator = prefix + "op"
	)
	roles := map[string]string{
		prefix + "reader":   "SQLAgentReaderRole",
		prefix + "user":     "SQLAgentUserRole",
		prefix + "operator": "SQLAgentOperatorRole",
	}
	userLogin := prefix + "user"

	cleanup := func() {
		for _, j := range []string{saJob, userJob} {
			db.ExecContext(c, "IF EXISTS (SELECT 1 FROM msdb.dbo.sysjobs WHERE name = N'"+j+"') "+
				"EXEC msdb.dbo.sp_delete_job @job_name = N'"+j+"'")
		}
		db.ExecContext(c, "IF EXISTS (SELECT 1 FROM msdb.dbo.sysschedules WHERE name = N'"+schedule+"') "+
			"EXEC msdb.dbo.sp_delete_schedule @schedule_name = N'"+schedule+"', @force_delete = 1")
		db.ExecContext(c, "IF EXISTS (SELECT 1 FROM msdb.dbo.sysoperators WHERE name = N'"+operator+"') "+
			"EXEC msdb.dbo.sp_delete_operator @name = N'"+operator+"'")
		for login := range roles {
			db.ExecContext(c, "USE msdb; IF USER_ID(N'"+login+"') IS NOT NULL DROP USER ["+login+"]")
			db.ExecContext(c, "IF SUSER_ID(N'"+login+"') IS NOT NULL DROP LOGIN ["+login+"]")
		}
	}
	cleanup()
	t.Cleanup(cleanup)

	for login, role := range roles {
		for _, q := range []string{
			"CREATE LOGIN [" + login + "] WITH PASSWORD = N'" + pass + "', CHECK_POLICY = OFF",
			"USE msdb; CREATE USER [" + login + "] FOR LOGIN [" + login + "]; ALTER ROLE [" + role + "] ADD MEMBER [" + login + "]",
		} {
			if _, err := db.ExecContext(ctx, q); err != nil {
				t.Fatalf("%s: %v", q, err)
			}
		}
	}

	if _, err := sa.CreateOperator(ctx, CreateOperatorRequest{Name: operator, Enabled: true, EmailAddress: "jobroles@example.invalid"}); err != nil {
		t.Fatalf("create operator: %v", err)
	}
	makeJob := func(name, owner string, steps ...string) *Job {
		t.Helper()
		j, err := sa.CreateJob(ctx, CreateJobRequest{Name: name, Description: "gosmo live", OwnerLogin: owner, Enabled: true})
		if err != nil {
			t.Fatalf("create job %s: %v", name, err)
		}
		for i, step := range steps {
			onSuccess := 3 // go to the next step
			if i == len(steps)-1 {
				onSuccess = 1
			}
			if _, err := j.AddStep(ctx, JobStepDefinition{Name: step, Subsystem: "TSQL", Command: "SELECT 1", Database: "master",
				OnSuccessAction: onSuccess, OnFailAction: 2, RetryAttempts: 1, RetryInterval: 2}); err != nil {
				t.Fatalf("add step %s to %s: %v", step, name, err)
			}
		}
		return j
	}
	sj := makeJob(saJob, "sa", "first", "second")
	op, level := operator, NotifyOnComplete
	if err := sj.Alter(ctx, JobChanges{NotifyEmailOperatorName: &op, NotifyLevelEmail: &level}); err != nil {
		t.Fatalf("set the operator: %v", err)
	}
	if _, err := sj.AddSchedule(ctx, CreateScheduleRequest{Name: schedule, Enabled: true, FreqType: 4, FreqInterval: 1}); err != nil {
		t.Fatalf("add schedule: %v", err)
	}
	makeJob(userJob, userLogin, "only")

	for _, name := range []string{saJob, userJob} {
		if err := sa.JobRef(name).Start(ctx, ""); err != nil {
			t.Skipf("start %s (is SQL Server Agent running?): %v", name, err)
		}
	}
	// Wait for both runs' outcome rows, and for Agent to close the run in
	// sysjobactivity, so no state moves between the reads compared.
	for deadline := time.Now().Add(60 * time.Second); ; {
		var n int
		db.QueryRowContext(ctx, `
SELECT COUNT(*) FROM msdb.dbo.sysjobhistory h JOIN msdb.dbo.sysjobs j ON j.job_id = h.job_id
WHERE  j.name IN (N'`+saJob+`', N'`+userJob+`') AND h.step_id = 0`).Scan(&n)
		if n == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the two runs did not finish in 60 s (%d outcome rows)", n)
		}
		time.Sleep(time.Second)
	}
	time.Sleep(2 * time.Second)

	ours := func(name string) bool { return strings.HasPrefix(name, prefix) }
	want := readJobFixture(t, ctx, sa, ours, saJob, userJob, schedule)
	if len(want.jobs) != 2 || len(want.steps[saJob]) != 2 || len(want.history[saJob]) != 3 ||
		want.jobs[0].NotifyEmailOperatorName != operator || len(want.schedules) != 1 || len(want.schedJobs) != 1 {
		t.Fatalf("sysadmin's read is not the fixture: %+v", want)
	}

	for login, role := range roles {
		t.Run(role, func(t *testing.T) {
			pool, err := sql.Open("sqlserver", liveRestrictedDSN(t, login, pass))
			if err != nil {
				t.Fatalf("open as %s: %v", login, err)
			}
			t.Cleanup(func() { pool.Close() })
			srv := &Server{db: pool}

			if role != "SQLAgentUserRole" {
				got := readJobFixture(t, ctx, srv, ours, saJob, userJob, schedule)
				if !reflect.DeepEqual(got, want) {
					t.Errorf("%s read\n%+v\nsysadmin read\n%+v", role, got, want)
				}
				return
			}

			// SQLAgentUserRole: its own job, read as sysadmin reads it, and
			// nothing of sa's.
			jobs, err := srv.Jobs(ctx)
			if err != nil {
				t.Fatalf("Jobs: %v", err)
			}
			jobs = slices.DeleteFunc(jobs, func(j *Job) bool { return !ours(j.Name) })
			if len(jobs) != 1 || jobs[0].Name != userJob {
				t.Fatalf("Jobs = %v, want only %s", jobNames(jobs), userJob)
			}
			if got, w := *bare(jobs[0]), want.jobs[1]; got != w {
				t.Errorf("Jobs gave\n%+v\nsysadmin\n%+v", got, w)
			}
			if _, err := srv.JobByName(ctx, saJob); !errors.Is(err, ErrNotFound) {
				t.Errorf("JobByName(%s) = %v, want ErrNotFound", saJob, err)
			}
			steps, err := srv.JobRef(userJob).Steps(ctx)
			if err != nil || !reflect.DeepEqual(bareSteps(steps), want.steps[userJob]) {
				t.Errorf("Steps = %+v, %v; sysadmin %+v", bareSteps(steps), err, want.steps[userJob])
			}
			hist, err := srv.SearchJobHistory(ctx, JobHistoryFilter{IncludeSteps: true})
			hist = slices.DeleteFunc(hist, func(h *JobHistoryEntry) bool { return !ours(h.JobName) })
			if err != nil || !reflect.DeepEqual(derefAll(hist), want.history[userJob]) {
				t.Errorf("SearchJobHistory = %+v, %v; want only %s's %+v", derefAll(hist), err, userJob, want.history[userJob])
			}
			scheds, err := srv.Schedules(ctx)
			if err != nil || slices.ContainsFunc(scheds, func(s *Schedule) bool { return s.Name == schedule }) {
				t.Errorf("Schedules shows sa's %s (%v)", schedule, err)
			}
		})
	}
}

// jobFixture is everything TestLiveJobReadsWithoutSysadmin compares, with the
// server and job back-pointers cleared so two logins' reads compare equal.
type jobFixture struct {
	jobs      []Job
	steps     map[string][]JobStep
	history   map[string][]JobHistoryEntry
	search    []JobHistoryEntry
	schedules []Schedule
	schedByID Schedule
	schedJobs []Job
}

func readJobFixture(t *testing.T, ctx context.Context, s *Server, ours func(string) bool, saJob, userJob, schedule string) jobFixture {
	t.Helper()
	f := jobFixture{steps: map[string][]JobStep{}, history: map[string][]JobHistoryEntry{}}
	jobs, err := s.Jobs(ctx)
	if err != nil {
		t.Fatalf("Jobs: %v", err)
	}
	for _, j := range jobs {
		if ours(j.Name) {
			f.jobs = append(f.jobs, *bare(j))
		}
	}
	byName, err := s.JobByName(ctx, saJob)
	if err != nil || len(f.jobs) == 0 || *bare(byName) != f.jobs[0] {
		t.Fatalf("JobByName(%s) = %+v, %v; Jobs gave %+v", saJob, byName, err, f.jobs)
	}
	for _, name := range []string{saJob, userJob} {
		// A JobRef: the id lookup is one of the reads under test.
		steps, err := s.JobRef(name).Steps(ctx)
		if err != nil {
			t.Fatalf("Steps(%s): %v", name, err)
		}
		f.steps[name] = bareSteps(steps)
		hist, err := s.JobRef(name).History(ctx, 0)
		if err != nil {
			t.Fatalf("History(%s): %v", name, err)
		}
		f.history[name] = derefAll(hist)
	}
	search, err := s.SearchJobHistory(ctx, JobHistoryFilter{JobName: saJob, Outcomes: []JobOutcome{JobOutcomeSucceeded}})
	if err != nil {
		t.Fatalf("SearchJobHistory: %v", err)
	}
	f.search = derefAll(search)
	scheds, err := s.JobRef(saJob).Schedules(ctx)
	if err != nil {
		t.Fatalf("Job.Schedules: %v", err)
	}
	for _, sch := range scheds {
		f.schedules = append(f.schedules, *bareSchedule(sch))
	}
	all, err := s.Schedules(ctx)
	if err != nil {
		t.Fatalf("Server.Schedules: %v", err)
	}
	if !slices.ContainsFunc(all, func(sch *Schedule) bool { return sch.Name == schedule }) {
		t.Errorf("Server.Schedules has no %s", schedule)
	}
	if len(f.schedules) == 1 {
		byID, err := s.ScheduleByID(ctx, f.schedules[0].ID)
		if err != nil {
			t.Fatalf("ScheduleByID: %v", err)
		}
		f.schedByID = *bareSchedule(byID)
		sjobs, err := s.ScheduleRef(schedule).Jobs(ctx)
		if err != nil {
			t.Fatalf("Schedule.Jobs: %v", err)
		}
		for _, j := range sjobs {
			f.schedJobs = append(f.schedJobs, *bare(j))
		}
	}
	return f
}

func bare(j *Job) *Job {
	c := *j
	c.server = nil
	return &c
}

func bareSteps(steps []*JobStep) []JobStep {
	out := make([]JobStep, len(steps))
	for i, st := range steps {
		out[i] = *st
		out[i].job = nil
	}
	return out
}

func bareSchedule(sch *Schedule) *Schedule {
	c := *sch
	c.server = nil
	return &c
}

func derefAll[T any](ps []*T) []T {
	out := make([]T, len(ps))
	for i, p := range ps {
		out[i] = *p
	}
	return out
}

func jobNames(jobs []*Job) []string {
	out := make([]string, len(jobs))
	for i, j := range jobs {
		out[i] = j.Name
	}
	return out
}
