package gosmo

// agent_job.go is a SQL Server Agent job: what one is, how the jobs are read
// and their running state resolved, and the job-level writes — start, stop,
// enable, rename, the msdb properties, CreateJob and AddSchedule. A job's steps
// are in agent_job_step.go and its run history in agent_job_history.go.

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// JobState is SQL Server Agent's job_state encoding for a job — the value
// xp_sqlagent_enum_jobs reports, which sp_help_job passes through as
// current_execution_status and SSMS's Job Activity Monitor displays.
//
// Agent keeps this in memory for the jobs it runs itself; msdb has no column
// for it. Jobs and JobByName read it through jobStates and fall back to a
// start/stop_execution_date derivation over msdb.dbo.sysjobactivity for every
// job that read does not cover — Agent stopped, or a login with neither
// sysadmin nor SQLAgentReaderRole — in which case only JobStateExecuting and
// JobStateIdle can be told apart. JobStateUnknown is what a multi-server job
// Agent does not run itself reports.
type JobState int

const (
	JobStateUnknown                     JobState = 0
	JobStateExecuting                   JobState = 1
	JobStateWaitingForWorker            JobState = 2
	JobStateBetweenRetries              JobState = 3
	JobStateIdle                        JobState = 4
	JobStateSuspended                   JobState = 5
	JobStateWaitingForStepToFinish      JobState = 6
	JobStatePerformingCompletionActions JobState = 7
)

// jobStateColumns is the result set of master.dbo.xp_sqlagent_enum_jobs, as
// a table-variable declaration. INSERT ... EXECUTE requires the shape to
// match the procedure's exactly, so this is copied from
// msdb.dbo.sp_get_composite_job_info, which is the only documentation of it.
const jobStateColumns = `(job_id                UNIQUEIDENTIFIER NOT NULL,
                          last_run_date         INT              NOT NULL,
                          last_run_time         INT              NOT NULL,
                          next_run_date         INT              NOT NULL,
                          next_run_time         INT              NOT NULL,
                          next_run_schedule_id  INT              NOT NULL,
                          requested_to_run      INT              NOT NULL,
                          request_source        INT              NOT NULL,
                          request_source_id     sysname          NULL,
                          running               INT              NOT NULL,
                          current_step          INT              NOT NULL,
                          current_retry_attempt INT              NOT NULL,
                          job_state             INT              NOT NULL)`

// jobStates returns each job's live execution state keyed by lower-cased
// job_id, in one read for the whole instance.
//
// The two arguments are the ones sp_get_composite_job_info passes: whether
// the caller may see every job's state — sysadmin or SQLAgentReaderRole;
// anyone else is shown only the jobs they own — and the login to judge that
// ownership by. Callers treat an error as "no states available" and keep the
// derived fallback rather than failing the listing: a job listing must survive
// an Agent outage.
//
// With Agent stopped the extended procedure does not fail — it runs and
// returns no rows, so this returns an empty map and no error, and
// applyJobStates overlays nothing (measured on SQL Server 2025 for Linux,
// 2026-09-03, by live_jobstate_test.go). The error return is for the other
// case, a caller the procedure refuses.
func (s *Server) jobStates(ctx context.Context) (map[string]JobState, error) {
	q := `
SET NOCOUNT ON;
DECLARE @states TABLE ` + jobStateColumns + `;
DECLARE @all INT = ISNULL(IS_SRVROLEMEMBER(N'sysadmin'), 0);
IF (@all = 0) SET @all = ISNULL(IS_MEMBER(N'SQLAgentReaderRole'), 0);
DECLARE @owner sysname = SUSER_SNAME();
INSERT INTO @states EXECUTE master.dbo.xp_sqlagent_enum_jobs @all, @owner;
SELECT LOWER(CONVERT(varchar(36), job_id)), job_state FROM @states`

	rows, err := s.query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("gosmo: agent job states: %w", err)
	}
	defer rows.Close()

	states := make(map[string]JobState)
	for rows.Next() {
		var id string
		var state int
		if err := rows.Scan(&id, &state); err != nil {
			return nil, fmt.Errorf("gosmo: agent job states: %w", err)
		}
		states[id] = JobState(state)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("gosmo: agent job states: %w", err)
	}
	return states, nil
}

// applyJobStates overlays Agent's live state onto jobs read from msdb,
// leaving the sysjobactivity-derived value in place for any job the read
// did not cover.
func (s *Server) applyJobStates(ctx context.Context, jobs ...*Job) {
	states, err := s.jobStates(ctx)
	if err != nil {
		return
	}
	for _, j := range jobs {
		if state, ok := states[strings.ToLower(j.JobID)]; ok {
			j.CurrentState = state
		}
	}
}

// JobOutcome represents the last run outcome for a job or step.
type JobOutcome int

const (
	JobOutcomeFailed    JobOutcome = 0
	JobOutcomeSucceeded JobOutcome = 1
	JobOutcomeRetried   JobOutcome = 2
	JobOutcomeCancelled JobOutcome = 3
	JobOutcomeUnknown   JobOutcome = 5
)

// NotifyLevel is the "when" condition for a job's email notification and
// its automatic-delete behavior — msdb's shared 0-3 encoding used by both
// sysjobs.notify_level_email and sysjobs.delete_level.
type NotifyLevel int

const (
	NotifyNever      NotifyLevel = 0
	NotifyOnSuccess  NotifyLevel = 1
	NotifyOnFailure  NotifyLevel = 2
	NotifyOnComplete NotifyLevel = 3
)

// Job mirrors a row in msdb.dbo.sysjobs together with its latest activity.
type Job struct {
	server           *Server
	JobID            string
	Name             string
	Description      string
	IsEnabled        bool
	Category         string
	OwnerLoginName   string
	DateCreated      time.Time
	DateModified     time.Time
	StartStepID      int
	DeleteLevel      NotifyLevel
	NotifyLevelEmail NotifyLevel
	// NotifyEmailOperatorName is "" if no operator is configured to be
	// emailed on job completion.
	NotifyEmailOperatorName string
	LastRunDate             time.Time
	LastRunOutcome          JobOutcome
	LastRunDuration         time.Duration
	NextRunDate             time.Time
	CurrentState            JobState
}

// Server returns the server the job belongs to.
func (j *Job) Server() *Server { return j.server }

// IsSystem reports whether j is one SQL Server creates for itself
// (syspolicy_purge_history). msdb has no flag for it, so it is the
// "syspolicy_" name prefix, and it answers on a JobRef too.
//
// msdb lets sp_delete_job drop any job, so a caller that gates Delete on this
// is the only thing protecting the job: widening the prefix hides a permitted
// operation, narrowing it exposes one.
//
// Deliberately narrow: sysutility_*, mdw_purge_data* and "SSIS Server
// Maintenance Job" come with optionally installed features, and removing them
// is ordinary administration.
func (j *Job) IsSystem() bool { return strings.HasPrefix(j.Name, "syspolicy_") }

// jobSelect is the 17-column select list and the five joins both job reads
// share; each caller appends its own ORDER BY or WHERE. Written once because
// the select list *is* what makes a *Job complete — a column added, or an
// ISNULL corrected, in one copy and not the other made Jobs and JobByName
// return differently-populated jobs for the same job, and the two copies had
// already started to drift.
const jobSelect = `
SELECT CONVERT(varchar(36), j.job_id), j.name, ISNULL(j.description,''),
       j.enabled, ISNULL(c.name,''), ISNULL(l.name,''),
       j.date_created, j.date_modified, j.start_step_id,
       j.delete_level, j.notify_level_email, ISNULL(no.name,''),
       ja.last_executed_step_date,
       ISNULL(js.last_run_outcome, 5),
       ISNULL(js.last_run_duration, 0),
       ja.next_scheduled_run_date,
       CASE WHEN ja.start_execution_date IS NOT NULL AND ja.stop_execution_date IS NULL
            THEN 1 ELSE 4 END
FROM   msdb.dbo.sysjobs j
LEFT   JOIN msdb.dbo.syscategories c ON c.category_id = j.category_id
LEFT   JOIN master.sys.server_principals l ON l.sid = j.owner_sid
LEFT   JOIN msdb.dbo.sysoperators no ON no.id = j.notify_email_operator_id
LEFT   JOIN msdb.dbo.sysjobactivity ja
       ON  ja.job_id = j.job_id
       AND ja.session_id = (SELECT MAX(session_id) FROM msdb.dbo.sysjobactivity)
LEFT   JOIN msdb.dbo.sysjobservers js
       ON  js.job_id = j.job_id
       AND js.server_id = 0`

// scanJob scans one row shaped like jobSelect into a new Job, decoding the
// nullable activity columns the outer joins may not supply. CurrentState is
// the sysjobactivity-derived fallback; applyJobStates overlays Agent's live
// value on top where it has one.
func scanJob(s *Server, scan func(dest ...any) error) (*Job, error) {
	j := &Job{server: s}
	var lastRun, nextRun sql.NullTime
	var lastOutcome, jobState, lastDuration sql.NullInt64
	if err := scan(
		&j.JobID, &j.Name, &j.Description,
		&j.IsEnabled, &j.Category, &j.OwnerLoginName,
		&j.DateCreated, &j.DateModified, &j.StartStepID,
		&j.DeleteLevel, &j.NotifyLevelEmail, &j.NotifyEmailOperatorName,
		&lastRun, &lastOutcome, &lastDuration, &nextRun, &jobState,
	); err != nil {
		return nil, err
	}
	if lastRun.Valid {
		j.LastRunDate = lastRun.Time
	}
	if nextRun.Valid {
		j.NextRunDate = nextRun.Time
	}
	j.LastRunOutcome = JobOutcome(lastOutcome.Int64)
	j.CurrentState = JobState(jobState.Int64)
	// Duration is encoded as HHMMSS integer, e.g. 10230 = 1h 2m 30s.
	d := lastDuration.Int64
	j.LastRunDuration = time.Duration(d/10000)*time.Hour +
		time.Duration((d%10000)/100)*time.Minute +
		time.Duration(d%100)*time.Second
	return j, nil
}

// Jobs returns all SQL Server Agent jobs from msdb.
func (s *Server) Jobs(ctx context.Context) ([]*Job, error) {
	const q = jobSelect + `
ORDER  BY j.name`

	rows, err := s.query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("gosmo: list agent jobs: %w", err)
	}
	defer rows.Close()

	var jobs []*Job
	for rows.Next() {
		j, err := scanJob(s, rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("gosmo: list agent jobs: %w", err)
		}
		jobs = append(jobs, j)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("gosmo: list agent jobs: %w", err)
	}
	s.applyJobStates(ctx, jobs...)
	return jobs, nil
}

// JobRef returns a lightweight handle for a job by name, without querying
// msdb — the job-side counterpart of Server.DatabaseRef. JobID, Category,
// LastRunOutcome and every other cached field stay at their zero value;
// JobByName is what populates them.
//
// Every write method on *Job builds its statement from Name alone
// (AddStep, AttachSchedule, Start, Rename, ...), so this handle is enough
// to keep operating on a job the caller already knows exists — and is the
// form to use when there is nothing to read yet: under a WithScript-derived
// context, JobByName's lookup is a real read and a job whose
// sp_add_job was merely collected is not there to find.
func (s *Server) JobRef(name string) *Job {
	return &Job{server: s, Name: name}
}

// JobByName returns a single job by name using a direct parameterised query.
func (s *Server) JobByName(ctx context.Context, name string) (*Job, error) {
	const q = jobSelect + `
WHERE  j.name = @p1`

	j, err := readByName(ctx, s, scanJob, q, []any{name},
		notFoundf("gosmo: agent job %q not found", name), "job by name")
	if err != nil {
		return nil, err
	}
	s.applyJobStates(ctx, j)
	return j, nil
}

// Start starts the job, optionally from a specific step name.
func (j *Job) Start(ctx context.Context, stepName string) error {
	q := fmt.Sprintf("EXEC msdb.dbo.sp_start_job @job_name = N'%s'", escapeSingle(j.Name))
	if stepName != "" {
		q += fmt.Sprintf(", @step_name = N'%s'", escapeSingle(stepName))
	}
	if err := j.server.exec(ctx, q); err != nil {
		return fmt.Errorf("gosmo: start job %q: %w", j.Name, err)
	}
	return nil
}

// Stop stops a running job.
func (j *Job) Stop(ctx context.Context) error {
	q := fmt.Sprintf("EXEC msdb.dbo.sp_stop_job @job_name = N'%s'", escapeSingle(j.Name))
	if err := j.server.exec(ctx, q); err != nil {
		return fmt.Errorf("gosmo: stop job %q: %w", j.Name, err)
	}
	return nil
}

// Enable enables the job.
func (j *Job) Enable(ctx context.Context) error { return j.setEnabled(ctx, true) }

// Disable disables the job.
func (j *Job) Disable(ctx context.Context) error { return j.setEnabled(ctx, false) }

func (j *Job) setEnabled(ctx context.Context, on bool) error {
	return j.Alter(ctx, JobChanges{Enabled: &on})
}

// Drop drops the agent job.
func (j *Job) Drop(ctx context.Context) error {
	q := fmt.Sprintf("EXEC msdb.dbo.sp_delete_job @job_name = N'%s'", escapeSingle(j.Name))
	if err := j.server.exec(ctx, q); err != nil {
		return fmt.Errorf("gosmo: drop job %q: %w", j.Name, err)
	}
	return nil
}

// Rename changes the job's name.
func (j *Job) Rename(ctx context.Context, newName string) error {
	return j.Alter(ctx, JobChanges{Name: &newName})
}

// SetDescription changes the job's description.
func (j *Job) SetDescription(ctx context.Context, desc string) error {
	return j.Alter(ctx, JobChanges{Description: &desc})
}

// SetCategory reassigns the job's category. category == "" moves it back to
// the default category, sent as msdb's [DEFAULT] sentinel and mirrored as
// the [Uncategorized (Local)] the catalog then reports — see
// agentCategoryTarget.
func (j *Job) SetCategory(ctx context.Context, category string) error {
	return j.Alter(ctx, JobChanges{Category: &category})
}

// SetOwner reassigns the job's owner login.
func (j *Job) SetOwner(ctx context.Context, loginName string) error {
	return j.Alter(ctx, JobChanges{OwnerLogin: &loginName})
}

// SetStartStep changes which step the job begins execution from.
func (j *Job) SetStartStep(ctx context.Context, stepID int) error {
	return j.Alter(ctx, JobChanges{StartStepID: &stepID})
}

// SetEmailNotify sets which operator is emailed on job completion, and
// under what condition. operatorName == "" leaves the currently configured
// operator unchanged (SQL Server has no documented "clear to none" value
// for sp_update_job's @notify_email_operator_name; pair with
// NotifyNever to stop emailing without needing to clear it).
//
// A level set on a job with no operator does not stick: msdb stores
// notify_level_email as 0 and reports no error, since a level with nobody to
// mail is meaningless to it. Verified on SQL Server 17.0.1135.8, 2026-09-17,
// and the batched Job.Alter behaves the same way.
func (j *Job) SetEmailNotify(ctx context.Context, operatorName string, level NotifyLevel) error {
	ch := JobChanges{NotifyLevelEmail: &level}
	if operatorName != "" {
		ch.NotifyEmailOperatorName = &operatorName
	}
	return j.Alter(ctx, ch)
}

// SetDeleteLevel sets the job's automatic-delete condition.
func (j *Job) SetDeleteLevel(ctx context.Context, level NotifyLevel) error {
	return j.Alter(ctx, JobChanges{DeleteLevel: &level})
}

// CreateJob creates a new SQL Server Agent job.
//
// It also enlists the job to run on the local server via sp_add_jobserver —
// without that, SQL Server Agent refuses to start the job (sp_start_job: "does
// not have any job server or servers defined") or let an alert target it
// (sp_update_alert/sp_add_alert: "cannot be used by an alert"). Multi-server
// (MSX/TSX) target-server selection is out of scope here, so "(local)" is the
// only target. The two procedures run as one execAtomic write, so a refused
// sp_add_jobserver leaves no half-made job behind under the requested name.
func (s *Server) CreateJob(ctx context.Context, req CreateJobRequest) (*Job, error) {
	if req.Name == "" {
		return nil, fmt.Errorf("gosmo: create job: name is required")
	}
	category := req.Category
	if category == "" {
		category = "[Uncategorized (Local)]"
	}
	q := fmt.Sprintf(
		"EXEC msdb.dbo.sp_add_job @job_name = N'%s', @description = N'%s', @category_name = N'%s', @enabled = %d",
		escapeSingle(req.Name), escapeSingle(req.Description),
		escapeSingle(category), boolToInt(req.Enabled),
	)
	if req.OwnerLogin != "" {
		q += fmt.Sprintf(", @owner_login_name = N'%s'", escapeSingle(req.OwnerLogin))
	}
	enlistQ := fmt.Sprintf("EXEC msdb.dbo.sp_add_jobserver @job_name = N'%s', @server_name = N'(local)'", escapeSingle(req.Name))
	if err := s.execAtomic(ctx, []string{q, enlistQ}); err != nil {
		return nil, fmt.Errorf("gosmo: create job %q: %w", req.Name, err)
	}
	return createdObject(ctx, s.JobRef(req.Name), func() (*Job, error) {
		return s.JobByName(ctx, req.Name)
	})
}

// AddSchedule creates a schedule and attaches it to the job in one step
// (sp_add_jobschedule), as opposed to AttachSchedule, which attaches an
// existing shared one. It takes the same request as Server.CreateSchedule,
// so every recurrence the catalog can hold is expressible: a weekly or
// monthly schedule needs FreqRecurrenceFactor ≥ 1 (Msg 14278 otherwise), and
// a FreqMonthlyRelative one FreqRelativeInterval too.
//
// sp_add_jobschedule has no owner parameter — it gives the schedule the job's
// owner — so an OwnerLoginName is applied by sp_update_schedule in the same
// execAtomic write, and a refused owner leaves no schedule behind.
//
// The new schedule_id comes back through sp_add_jobschedule's OUTPUT
// parameter and the schedule is read back by it, as CreateSchedule does:
// schedule names are not unique, and a handle without the id addresses the
// schedule by name, which fails Msg 14371 once another shares it. When the
// read finds nothing (a creator who cannot see msdb's row) the ScheduleRef
// handle still carries the id. The write is never retried. Under
// Scripting(ctx) the plain statements are collected and the result is the
// ScheduleRef handle, with no id.
func (j *Job) AddSchedule(ctx context.Context, req CreateScheduleRequest) (*Schedule, error) {
	if req.Name == "" {
		return nil, fmt.Errorf("gosmo: add schedule to job %q: name is required", j.Name)
	}
	add := fmt.Sprintf("EXEC msdb.dbo.sp_add_jobschedule @job_name = N'%s', @name = N'%s', %s",
		escapeSingle(j.Name), escapeSingle(req.Name), req.frequencyArgs())
	stmts := []string{"DECLARE @schedule_id int", add + ", @schedule_id = @schedule_id OUTPUT"}
	if req.OwnerLoginName != "" {
		stmts = append(stmts, fmt.Sprintf("EXEC msdb.dbo.sp_update_schedule @schedule_id = @schedule_id, @owner_login_name = N'%s'",
			escapeSingle(req.OwnerLoginName)))
	}
	const read = "SELECT @schedule_id;"
	var err error
	var id int
	switch {
	case Scripting(ctx) && req.OwnerLoginName == "":
		err = j.server.exec(ctx, add)
	case Scripting(ctx):
		err = j.server.execAtomic(ctx, stmts)
	case req.OwnerLoginName == "":
		stmt := strings.Join(stmts, ";\n") + ";\n" + read
		if err = j.server.execScan(ctx, stmt, &id); err == nil {
			observe(ctx, j.server, ScriptEntry{Server: scriptServerName(ctx, j.server), SQL: stmt})
		}
	default:
		err = j.server.execAtomicScan(ctx, stmts, read, &id)
	}
	if err != nil {
		return nil, fmt.Errorf("gosmo: add schedule %q to job %q: %w", req.Name, j.Name, err)
	}
	handle := j.server.ScheduleRef(req.Name)
	handle.ID = id
	return createdObject(ctx, handle, func() (*Schedule, error) {
		return j.server.ScheduleByID(ctx, id)
	})
}

// CreateJobRequest describes a new SQL Server Agent job.
type CreateJobRequest struct {
	Name        string
	Description string
	// Category defaults to [Uncategorized (Local)] when empty.
	Category   string
	OwnerLogin string
	Enabled    bool
}
