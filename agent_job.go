package gosmo

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// ============================================================
// SQL Server Agent -- Status
// ============================================================

// AgentStatus reports whether SQL Server Agent is currently running, based
// only on SQL-visible state (sys.dm_server_services, or the Agent's own
// sessions on Azure) — no Windows Service Control, WMI, or registry access,
// matching the SQL-only scope of the rest of this file.
type AgentStatus struct {
	Running    bool
	StatusText string
	// LastStartupTime is the zero Time if the DMV has no matching row (the
	// Agent service isn't registered under this instance, or the DMV isn't
	// queryable in this edition/deployment).
	LastStartupTime time.Time
}

// AgentInfo reports SQL Server Agent's current run state.
func (s *Server) AgentInfo() (*AgentStatus, error) {
	return s.AgentInfoContext(context.Background())
}

// AgentInfoContext is the context-aware variant of AgentInfo.
func (s *Server) AgentInfoContext(ctx context.Context) (*AgentStatus, error) {
	if s.info.IsAzure() {
		return s.agentInfoAzure(ctx)
	}

	const q = `
SELECT status_desc, last_startup_time
FROM   sys.dm_server_services
WHERE  servicename LIKE N'SQL Server Agent%'`

	st := &AgentStatus{}
	var startup sql.NullTime
	if err := s.queryRowScan(ctx, q, nil, &st.StatusText, &startup); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return &AgentStatus{StatusText: "Unknown"}, nil
		}
		return nil, fmt.Errorf("gosmo: agent status: %w", err)
	}
	st.Running = strings.EqualFold(st.StatusText, "Running")
	if startup.Valid {
		st.LastStartupTime = startup.Time
	}
	return st, nil
}

// agentInfoAzure answers AgentInfoContext on an Azure engine edition, where
// sys.dm_server_services returns no rows at all — there is no Windows service
// to report, and xp_servicecontrol fails with "The specified service does not
// exist as an installed service" — so the on-prem read falls to its "Unknown"
// branch on a Managed Instance whose Agent is demonstrably running.
//
// Two SQL-visible facts stand in, keeping the no-WMI contract:
//
//   - Agent connects to the engine under program_name 'SQLAgent - ...' (a
//     Managed Instance shows "Generic Refresher" and "Email Logger"), so a
//     matching row in sys.dm_exec_sessions means it is up right now. That
//     read needs VIEW SERVER STATE; without it the count comes back 0 rather
//     than failing, which reads as stopped — see the fallback below.
//   - msdb.dbo.syssessions gets a row per Agent start, so its newest
//     agent_start_date is the last startup time. It survives an Agent stop,
//     which is why it cannot decide Running on its own.
func (s *Server) agentInfoAzure(ctx context.Context) (*AgentStatus, error) {
	const q = `
SELECT (SELECT MAX(agent_start_date) FROM msdb.dbo.syssessions),
       (SELECT COUNT(*) FROM sys.dm_exec_sessions
        WHERE program_name LIKE N'SQLAgent%')`

	var startup sql.NullTime
	var sessions int
	if err := s.queryRowScan(ctx, q, nil, &startup, &sessions); err != nil {
		return nil, fmt.Errorf("gosmo: agent status: %w", err)
	}

	st := &AgentStatus{Running: sessions > 0}
	if startup.Valid {
		st.LastStartupTime = startup.Time
	}
	switch {
	case st.Running:
		st.StatusText = "Running"
	case startup.Valid:
		// Agent has run at some point but holds no session now. Stopped is
		// the honest reading; a login without VIEW SERVER STATE lands here
		// too, which is why the startup time is still reported alongside.
		st.StatusText = "Stopped"
	default:
		st.StatusText = "Unknown"
	}
	return st, nil
}

// ============================================================
// SQL Server Agent -- Jobs
// ============================================================

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

// Jobs returns all SQL Server Agent jobs from msdb.
func (s *Server) Jobs() ([]*Job, error) {
	return s.JobsContext(context.Background())
}

// JobsContext is the context-aware variant of Jobs.
func (s *Server) JobsContext(ctx context.Context) ([]*Job, error) {
	const q = `
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
       AND js.server_id = 0
ORDER  BY j.name`

	rows, err := s.query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("gosmo: list agent jobs: %w", err)
	}
	defer rows.Close()

	var jobs []*Job
	for rows.Next() {
		j := &Job{server: s}
		var lastRun, nextRun sql.NullTime
		var lastOutcome, jobState, lastDuration sql.NullInt64
		if err := rows.Scan(
			&j.JobID, &j.Name, &j.Description,
			&j.IsEnabled, &j.Category, &j.OwnerLoginName,
			&j.DateCreated, &j.DateModified, &j.StartStepID,
			&j.DeleteLevel, &j.NotifyLevelEmail, &j.NotifyEmailOperatorName,
			&lastRun, &lastOutcome, &lastDuration, &nextRun, &jobState,
		); err != nil {
			return nil, fmt.Errorf("gosmo: list agent jobs: %w", err)
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
		jobs = append(jobs, j)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("gosmo: list agent jobs: %w", err)
	}
	s.applyJobStates(ctx, jobs...)
	return jobs, nil
}

// Job returns a lightweight handle for a job by name, without querying
// msdb — the job-side counterpart of Server.Database. JobID, Category,
// LastRunOutcome and every other cached field stay at their zero value;
// JobByName is what populates them.
//
// Every write method on *Job builds its statement from Name alone
// (AddStep, AttachSchedule, Start, Rename, ...), so this handle is enough
// to keep operating on a job the caller already knows exists — and is the
// only usable form under a WithScript context, where JobByNameContext's
// lookup is a real read and a job whose sp_add_job was merely collected is
// not there to find.
func (s *Server) Job(name string) *Job {
	return &Job{server: s, Name: name}
}

// JobByName returns a single job by name using a direct parameterised query.
func (s *Server) JobByName(name string) (*Job, error) {
	return s.JobByNameContext(context.Background(), name)
}

// JobByNameContext is the context-aware variant of JobByName.
func (s *Server) JobByNameContext(ctx context.Context, name string) (*Job, error) {
	const q = `
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
       AND js.server_id = 0
WHERE  j.name = @p1`

	j := &Job{server: s}
	var lastRun, nextRun sql.NullTime
	var lastOutcome, jobState, lastDuration sql.NullInt64
	if err := s.queryRowScan(ctx, q, []any{name},
		&j.JobID, &j.Name, &j.Description,
		&j.IsEnabled, &j.Category, &j.OwnerLoginName,
		&j.DateCreated, &j.DateModified, &j.StartStepID,
		&j.DeleteLevel, &j.NotifyLevelEmail, &j.NotifyEmailOperatorName,
		&lastRun, &lastOutcome, &lastDuration, &nextRun, &jobState,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, notFoundf("gosmo: agent job %q not found", name)
		}
		return nil, fmt.Errorf("gosmo: job by name: %w", err)
	}
	if lastRun.Valid {
		j.LastRunDate = lastRun.Time
	}
	if nextRun.Valid {
		j.NextRunDate = nextRun.Time
	}
	j.LastRunOutcome = JobOutcome(lastOutcome.Int64)
	j.CurrentState = JobState(jobState.Int64)
	d := lastDuration.Int64
	j.LastRunDuration = time.Duration(d/10000)*time.Hour +
		time.Duration((d%10000)/100)*time.Minute +
		time.Duration(d%100)*time.Second
	s.applyJobStates(ctx, j)
	return j, nil
}

// Start starts the job, optionally from a specific step name.
func (j *Job) Start(stepName string) error {
	return j.StartContext(context.Background(), stepName)
}

// StartContext is the context-aware variant of Start.
func (j *Job) StartContext(ctx context.Context, stepName string) error {
	q := fmt.Sprintf("EXEC msdb.dbo.sp_start_job @job_name = N'%s'", escapeSingle(j.Name))
	if stepName != "" {
		q += fmt.Sprintf(", @step_name = N'%s'", escapeSingle(stepName))
	}
	if err := j.server.execContext(ctx, q); err != nil {
		return fmt.Errorf("gosmo: start job %q: %w", j.Name, err)
	}
	return nil
}

// Stop stops a running job.
func (j *Job) Stop() error {
	return j.StopContext(context.Background())
}

// StopContext is the context-aware variant of Stop.
func (j *Job) StopContext(ctx context.Context) error {
	q := fmt.Sprintf("EXEC msdb.dbo.sp_stop_job @job_name = N'%s'", escapeSingle(j.Name))
	if err := j.server.execContext(ctx, q); err != nil {
		return fmt.Errorf("gosmo: stop job %q: %w", j.Name, err)
	}
	return nil
}

// Enable enables the job.
func (j *Job) Enable() error { return j.EnableContext(context.Background()) }

// EnableContext is the context-aware variant of Enable.
func (j *Job) EnableContext(ctx context.Context) error { return j.setEnabled(ctx, true) }

// Disable disables the job.
func (j *Job) Disable() error { return j.DisableContext(context.Background()) }

// DisableContext is the context-aware variant of Disable.
func (j *Job) DisableContext(ctx context.Context) error { return j.setEnabled(ctx, false) }

func (j *Job) setEnabled(ctx context.Context, on bool) error {
	q := fmt.Sprintf("EXEC msdb.dbo.sp_update_job @job_name = N'%s', @enabled = %d",
		escapeSingle(j.Name), boolToInt(on))
	if err := j.server.execContext(ctx, q); err != nil {
		return fmt.Errorf("gosmo: set enabled=%v for job %q: %w", on, j.Name, err)
	}
	setIfApplied(ctx, &j.IsEnabled, on)
	return nil
}

// Drop drops the agent job.
func (j *Job) Drop() error {
	return j.DropContext(context.Background())
}

// DropContext is the context-aware variant of Drop.
func (j *Job) DropContext(ctx context.Context) error {
	q := fmt.Sprintf("EXEC msdb.dbo.sp_delete_job @job_name = N'%s'", escapeSingle(j.Name))
	if err := j.server.execContext(ctx, q); err != nil {
		return fmt.Errorf("gosmo: drop job %q: %w", j.Name, err)
	}
	return nil
}

// Rename changes the job's name.
func (j *Job) Rename(newName string) error { return j.RenameContext(context.Background(), newName) }

// RenameContext is the context-aware variant of Rename.
func (j *Job) RenameContext(ctx context.Context, newName string) error {
	q := fmt.Sprintf("EXEC msdb.dbo.sp_update_job @job_name = N'%s', @new_name = N'%s'",
		escapeSingle(j.Name), escapeSingle(newName))
	if err := j.server.execContext(ctx, q); err != nil {
		return fmt.Errorf("gosmo: rename job %q to %q: %w", j.Name, newName, err)
	}
	setIfApplied(ctx, &j.Name, newName)
	return nil
}

// SetDescription changes the job's description.
func (j *Job) SetDescription(desc string) error {
	return j.SetDescriptionContext(context.Background(), desc)
}

// SetDescriptionContext is the context-aware variant of SetDescription.
func (j *Job) SetDescriptionContext(ctx context.Context, desc string) error {
	q := fmt.Sprintf("EXEC msdb.dbo.sp_update_job @job_name = N'%s', @description = N'%s'",
		escapeSingle(j.Name), escapeSingle(desc))
	if err := j.server.execContext(ctx, q); err != nil {
		return fmt.Errorf("gosmo: set description for job %q: %w", j.Name, err)
	}
	setIfApplied(ctx, &j.Description, desc)
	return nil
}

// SetCategory reassigns the job's category.
func (j *Job) SetCategory(category string) error {
	return j.SetCategoryContext(context.Background(), category)
}

// SetCategoryContext is the context-aware variant of SetCategory.
func (j *Job) SetCategoryContext(ctx context.Context, category string) error {
	q := fmt.Sprintf("EXEC msdb.dbo.sp_update_job @job_name = N'%s', @category_name = N'%s'",
		escapeSingle(j.Name), escapeSingle(category))
	if err := j.server.execContext(ctx, q); err != nil {
		return fmt.Errorf("gosmo: set category for job %q: %w", j.Name, err)
	}
	setIfApplied(ctx, &j.Category, category)
	return nil
}

// SetOwner reassigns the job's owner login.
func (j *Job) SetOwner(loginName string) error {
	return j.SetOwnerContext(context.Background(), loginName)
}

// SetOwnerContext is the context-aware variant of SetOwner.
func (j *Job) SetOwnerContext(ctx context.Context, loginName string) error {
	q := fmt.Sprintf("EXEC msdb.dbo.sp_update_job @job_name = N'%s', @owner_login_name = N'%s'",
		escapeSingle(j.Name), escapeSingle(loginName))
	if err := j.server.execContext(ctx, q); err != nil {
		return fmt.Errorf("gosmo: set owner for job %q: %w", j.Name, err)
	}
	setIfApplied(ctx, &j.OwnerLoginName, loginName)
	return nil
}

// SetStartStep changes which step the job begins execution from.
func (j *Job) SetStartStep(stepID int) error {
	return j.SetStartStepContext(context.Background(), stepID)
}

// SetStartStepContext is the context-aware variant of SetStartStep.
func (j *Job) SetStartStepContext(ctx context.Context, stepID int) error {
	q := fmt.Sprintf("EXEC msdb.dbo.sp_update_job @job_name = N'%s', @start_step_id = %d",
		escapeSingle(j.Name), stepID)
	if err := j.server.execContext(ctx, q); err != nil {
		return fmt.Errorf("gosmo: set start step for job %q: %w", j.Name, err)
	}
	setIfApplied(ctx, &j.StartStepID, stepID)
	return nil
}

// SetEmailNotify sets which operator is emailed on job completion, and
// under what condition. operatorName == "" leaves the currently configured
// operator unchanged (SQL Server has no documented "clear to none" value
// for sp_update_job's @notify_email_operator_name; pair with
// NotifyNever to stop emailing without needing to clear it).
func (j *Job) SetEmailNotify(operatorName string, level NotifyLevel) error {
	return j.SetEmailNotifyContext(context.Background(), operatorName, level)
}

// SetEmailNotifyContext is the context-aware variant of SetEmailNotify.
func (j *Job) SetEmailNotifyContext(ctx context.Context, operatorName string, level NotifyLevel) error {
	q := fmt.Sprintf("EXEC msdb.dbo.sp_update_job @job_name = N'%s', @notify_level_email = %d",
		escapeSingle(j.Name), int(level))
	if operatorName != "" {
		q += fmt.Sprintf(", @notify_email_operator_name = N'%s'", escapeSingle(operatorName))
	}
	if err := j.server.execContext(ctx, q); err != nil {
		return fmt.Errorf("gosmo: set email notification for job %q: %w", j.Name, err)
	}
	setIfApplied(ctx, &j.NotifyLevelEmail, level)
	if operatorName != "" {
		setIfApplied(ctx, &j.NotifyEmailOperatorName, operatorName)
	}
	return nil
}

// SetDeleteLevel sets the job's automatic-delete condition.
func (j *Job) SetDeleteLevel(level NotifyLevel) error {
	return j.SetDeleteLevelContext(context.Background(), level)
}

// SetDeleteLevelContext is the context-aware variant of SetDeleteLevel.
func (j *Job) SetDeleteLevelContext(ctx context.Context, level NotifyLevel) error {
	q := fmt.Sprintf("EXEC msdb.dbo.sp_update_job @job_name = N'%s', @delete_level = %d",
		escapeSingle(j.Name), int(level))
	if err := j.server.execContext(ctx, q); err != nil {
		return fmt.Errorf("gosmo: set delete level for job %q: %w", j.Name, err)
	}
	setIfApplied(ctx, &j.DeleteLevel, level)
	return nil
}

// Steps returns all steps defined for the job, ordered by step_id.
func (j *Job) Steps() ([]*JobStep, error) {
	return j.StepsContext(context.Background())
}

// StepsContext is the context-aware variant of Steps.
func (j *Job) StepsContext(ctx context.Context) ([]*JobStep, error) {
	// The proxy is joined by name rather than reported as an id: an id means
	// nothing to a caller, and a move that re-adds a step has to pass
	// @proxy_name back.
	const q = `
SELECT s.step_id, s.step_name, s.subsystem, s.command, ISNULL(s.database_name, ''),
       s.on_success_action, s.on_success_step_id, s.on_fail_action, s.on_fail_step_id,
       s.last_run_outcome, s.last_run_date, s.last_run_time, s.last_run_duration,
       s.retry_attempts, s.retry_interval, ISNULL(s.output_file_name, ''), s.flags,
       ISNULL(p.name, ''), ISNULL(s.additional_parameters, ''),
       ISNULL(s.cmdexec_success_code, 0), ISNULL(s.server, ''),
       ISNULL(s.database_user_name, ''), ISNULL(s.os_run_priority, 0)
FROM   msdb.dbo.sysjobsteps s
LEFT   JOIN msdb.dbo.sysproxies p ON p.proxy_id = s.proxy_id
WHERE  s.job_id = @p1
ORDER  BY s.step_id`

	rows, err := j.server.query(ctx, q, j.JobID)
	if err != nil {
		return nil, fmt.Errorf("gosmo: steps for job %q: %w", j.Name, err)
	}
	defer rows.Close()

	var steps []*JobStep
	for rows.Next() {
		s := &JobStep{job: j}
		var lastRunDate, lastRunTime sql.NullInt64
		if err := rows.Scan(
			&s.StepID, &s.Name, &s.Subsystem, &s.Command, &s.Database,
			&s.OnSuccessAction, &s.OnSuccessStepID, &s.OnFailAction, &s.OnFailStepID,
			&s.LastRunOutcome, &lastRunDate, &lastRunTime, &s.LastRunDuration,
			&s.RetryAttempts, &s.RetryInterval, &s.OutputFileName, &s.Flags,
			&s.ProxyName, &s.AdditionalParameters, &s.CmdExecSuccessCode,
			&s.Server, &s.DatabaseUserName, &s.OSRunPriority,
		); err != nil {
			return nil, fmt.Errorf("gosmo: steps for job %q: %w", j.Name, err)
		}
		// last_run_date is 0 for a step that has never run, which
		// parseSQLAgentDate would turn into a year-zero date rather than a
		// zero Time. Leave LastRunDate zero so IsZero() is the "never ran"
		// test callers expect.
		if lastRunDate.Valid && lastRunDate.Int64 != 0 {
			s.LastRunDate = parseSQLAgentDate(int(lastRunDate.Int64), int(lastRunTime.Int64))
		}
		s.LastRunElapsed = parseSQLAgentDuration(s.LastRunDuration)
		steps = append(steps, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("gosmo: steps for job %q: %w", j.Name, err)
	}
	return steps, nil
}

// History returns the execution history (most recent first).
// Pass limit=0 to use the default of 100 rows.
func (j *Job) History(limit int) ([]*JobHistoryEntry, error) {
	return j.HistoryContext(context.Background(), limit)
}

// HistoryContext is the context-aware variant of History.
func (j *Job) HistoryContext(ctx context.Context, limit int) ([]*JobHistoryEntry, error) {
	if limit <= 0 {
		limit = 100
	}
	q := fmt.Sprintf(`
SELECT TOP %d
       run_date, run_time, run_duration,
       run_status, ISNULL(message, ''), step_id, step_name
FROM   msdb.dbo.sysjobhistory
WHERE  job_id = @p1
ORDER  BY run_date DESC, run_time DESC`, limit)

	rows, err := j.server.query(ctx, q, j.JobID)
	if err != nil {
		return nil, fmt.Errorf("gosmo: history for job %q: %w", j.Name, err)
	}
	defer rows.Close()

	var history []*JobHistoryEntry
	for rows.Next() {
		h := &JobHistoryEntry{}
		var runDate, runTime, runDur int
		if err := rows.Scan(&runDate, &runTime, &runDur,
			&h.Outcome, &h.Message, &h.StepID, &h.StepName); err != nil {
			return nil, fmt.Errorf("gosmo: history for job %q: %w", j.Name, err)
		}
		h.RunDate = parseSQLAgentDate(runDate, runTime)
		h.Duration = parseSQLAgentDuration(runDur)
		history = append(history, h)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("gosmo: history for job %q: %w", j.Name, err)
	}
	return history, nil
}

// JobHistory returns the most recent job-level history entries (step_id =
// 0, i.e. the overall outcome of each run rather than a single step's)
// across every job, most recent first. Pass limit=0 for the default of 100
// rows.
func (s *Server) JobHistory(limit int) ([]*JobHistoryEntry, error) {
	return s.JobHistoryContext(context.Background(), limit)
}

// JobHistoryContext is the context-aware variant of JobHistory.
func (s *Server) JobHistoryContext(ctx context.Context, limit int) ([]*JobHistoryEntry, error) {
	if limit <= 0 {
		limit = 100
	}
	q := fmt.Sprintf(`
SELECT TOP %d
       j.name, h.run_date, h.run_time, h.run_duration,
       h.run_status, ISNULL(h.message, ''), h.step_id, h.step_name
FROM   msdb.dbo.sysjobhistory h
JOIN   msdb.dbo.sysjobs j ON j.job_id = h.job_id
WHERE  h.step_id = 0
ORDER  BY h.run_date DESC, h.run_time DESC`, limit)

	rows, err := s.query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("gosmo: job history: %w", err)
	}
	defer rows.Close()

	var history []*JobHistoryEntry
	for rows.Next() {
		h := &JobHistoryEntry{}
		var runDate, runTime, runDur int
		if err := rows.Scan(&h.JobName, &runDate, &runTime, &runDur,
			&h.Outcome, &h.Message, &h.StepID, &h.StepName); err != nil {
			return nil, fmt.Errorf("gosmo: job history: %w", err)
		}
		h.RunDate = parseSQLAgentDate(runDate, runTime)
		h.Duration = parseSQLAgentDuration(runDur)
		history = append(history, h)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("gosmo: job history: %w", err)
	}
	return history, nil
}

// CreateJob creates a new SQL Server Agent job.
func (s *Server) CreateJob(req CreateJobRequest) (*Job, error) {
	return s.CreateJobContext(context.Background(), req)
}

// CreateJobContext is the context-aware variant of CreateJob. It also
// enlists the job to run on the local server via sp_add_jobserver —
// without that, SQL Server Agent refuses to start the job (sp_start_job:
// "does not have any job server or servers defined") or let an alert
// target it (sp_update_alert/sp_add_alert: "cannot be used by an alert").
// Multi-server (MSX/TSX) target-server selection is out of scope here, so
// "(local)" is the only target.
func (s *Server) CreateJobContext(ctx context.Context, req CreateJobRequest) (*Job, error) {
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
	if err := s.execContext(ctx, q); err != nil {
		return nil, fmt.Errorf("gosmo: create job %q: %w", req.Name, err)
	}
	enlistQ := fmt.Sprintf("EXEC msdb.dbo.sp_add_jobserver @job_name = N'%s', @server_name = N'(local)'", escapeSingle(req.Name))
	if err := s.execContext(ctx, enlistQ); err != nil {
		return nil, fmt.Errorf("gosmo: enlist job %q on local server: %w", req.Name, err)
	}
	if Scripting(ctx) {
		// See CreateScheduleContext: the read-back is a real query, and the
		// two EXECs above were only collected, so it would fail with "job not
		// found" rather than yielding the script that was asked for.
		return s.Job(req.Name), nil
	}
	return s.JobByNameContext(ctx, req.Name)
}

// AddStep adds a T-SQL or other subsystem step to the job.
func (j *Job) AddStep(req JobStepRequest) error {
	return j.AddStepContext(context.Background(), req)
}

// AddStepContext is the context-aware variant of AddStep.
func (j *Job) AddStepContext(ctx context.Context, req JobStepRequest) error {
	return j.addStepAt(ctx, req, 0)
}

// InsertStep adds a step at position stepID, renumbering the steps at and
// after it, rather than appending.
//
// The renumbering is msdb's, and it carries every other step's "go to step N"
// reference with it — verified against SQL Server 2025. sp_delete_jobstep is
// not symmetrical about this: it clears a reference to a step at or after the
// one deleted instead of following it, which is why ReorderSteps repairs
// references itself.
func (j *Job) InsertStep(req JobStepRequest, stepID int) error {
	return j.InsertStepContext(context.Background(), req, stepID)
}

// InsertStepContext is the context-aware variant of InsertStep.
func (j *Job) InsertStepContext(ctx context.Context, req JobStepRequest, stepID int) error {
	if stepID < 1 {
		return fmt.Errorf("gosmo: insert step %q into job %q: step id must be 1 or more", req.Name, j.Name)
	}
	return j.addStepAt(ctx, req, stepID)
}

// stepExtraArgs renders the parameters both sp_add_jobstep and
// sp_update_jobstep take and that a plain edit leaves alone: the ones a move
// has to carry so a re-added step is the step it was.
func stepExtraArgs(req JobStepRequest) string {
	q := fmt.Sprintf(", @flags = %d, @cmdexec_success_code = %d, @os_run_priority = %d",
		req.Flags, req.CmdExecSuccessCode, req.OSRunPriority)
	if req.ProxyName != "" {
		q += fmt.Sprintf(", @proxy_name = N'%s'", escapeSingle(req.ProxyName))
	}
	if req.AdditionalParameters != "" {
		q += fmt.Sprintf(", @additional_parameters = N'%s'", escapeSingle(req.AdditionalParameters))
	}
	if req.Server != "" {
		q += fmt.Sprintf(", @server = N'%s'", escapeSingle(req.Server))
	}
	if req.DatabaseUserName != "" {
		q += fmt.Sprintf(", @database_user_name = N'%s'", escapeSingle(req.DatabaseUserName))
	}
	return q
}

func (j *Job) addStepAt(ctx context.Context, req JobStepRequest, stepID int) error {
	if req.Name == "" {
		return fmt.Errorf("gosmo: add step: name is required")
	}
	if err := j.server.execContext(ctx, addStepStmt(j.Name, req, stepID)); err != nil {
		return fmt.Errorf("gosmo: add step %q to job %q: %w", req.Name, j.Name, err)
	}
	return nil
}

// addStepStmt renders the sp_add_jobstep call. stepID > 0 inserts at that
// position; 0 appends. Split out from addStepAt so ReorderStepsContext can
// collect the statement into its transactional batch instead of issuing it —
// see atomicBatch.
func addStepStmt(jobName string, req JobStepRequest, stepID int) string {
	q := fmt.Sprintf(
		"EXEC msdb.dbo.sp_add_jobstep @job_name = N'%s', @step_name = N'%s', "+
			"@subsystem = N'%s', @command = N'%s', "+
			"@on_success_action = %d, @on_success_step_id = %d, "+
			"@on_fail_action = %d, @on_fail_step_id = %d, "+
			"@retry_attempts = %d, @retry_interval = %d",
		escapeSingle(jobName), escapeSingle(req.Name),
		escapeSingle(req.Subsystem), escapeSingle(req.Command),
		req.OnSuccessAction, req.OnSuccessStepID,
		req.OnFailAction, req.OnFailStepID,
		req.RetryAttempts, req.RetryInterval,
	)
	if req.Database != "" {
		q += fmt.Sprintf(", @database_name = N'%s'", escapeSingle(req.Database))
	}
	if req.OutputFileName != "" {
		q += fmt.Sprintf(", @output_file_name = N'%s'", escapeSingle(req.OutputFileName))
	}
	q += stepExtraArgs(req)
	if stepID > 0 {
		// Insertion, not append: sp_add_jobstep renumbers every later step
		// and follows their "go to step N" references, which is what makes a
		// move possible at all. Appending is @step_id omitted, not 0 — msdb
		// rejects a zero.
		q += fmt.Sprintf(", @step_id = %d", stepID)
	}
	return q
}

// Update replaces the step's definition via sp_update_jobstep.
func (s *JobStep) Update(req JobStepRequest) error {
	return s.UpdateContext(context.Background(), req)
}

// UpdateContext is the context-aware variant of Update.
func (s *JobStep) UpdateContext(ctx context.Context, req JobStepRequest) error {
	// Same guard as Job.AddStepContext: sp_update_jobstep rejects an empty
	// @step_name with a server-side error, and the local field writes at the
	// end of this method would otherwise blank out s.Name on the way past.
	if req.Name == "" {
		return fmt.Errorf("gosmo: update step: name is required")
	}
	q := fmt.Sprintf(
		"EXEC msdb.dbo.sp_update_jobstep @job_name = N'%s', @step_id = %d, "+
			"@step_name = N'%s', @subsystem = N'%s', @command = N'%s', "+
			"@on_success_action = %d, @on_success_step_id = %d, "+
			"@on_fail_action = %d, @on_fail_step_id = %d, "+
			"@retry_attempts = %d, @retry_interval = %d",
		escapeSingle(s.job.Name), s.StepID,
		escapeSingle(req.Name), escapeSingle(req.Subsystem), escapeSingle(req.Command),
		req.OnSuccessAction, req.OnSuccessStepID,
		req.OnFailAction, req.OnFailStepID,
		req.RetryAttempts, req.RetryInterval,
	)
	// sp_update_jobstep leaves a parameter it was not passed exactly as it
	// was, so an omitted @database_name is "keep", not "clear". An empty
	// req.Database therefore cannot be sent through: N'' is accepted without
	// error and changes nothing (verified against SQL Server 2025), so the
	// only honest reading of an empty value is "leave it alone" — and the
	// local mirror below has to skip the field for the same reason, or the
	// JobStep would report a database the server never stopped using.
	sendDatabase := req.Database != ""
	if sendDatabase {
		q += fmt.Sprintf(", @database_name = N'%s'", escapeSingle(req.Database))
	}
	// @output_file_name, unlike @database_name, does honour N'': it nulls the
	// column. It is therefore always sent, so blanking the field in a caller's
	// form actually clears the step's output file instead of silently keeping
	// the old path while the JobStep claimed it was gone.
	q += fmt.Sprintf(", @output_file_name = N'%s'", escapeSingle(req.OutputFileName))
	if err := s.job.server.execContext(ctx, q); err != nil {
		return fmt.Errorf("gosmo: update step %q of job %q: %w", req.Name, s.job.Name, err)
	}
	// Not mirrored under WithScript — nothing reached the server, so the step
	// must keep describing what is actually there. See setIfApplied, which is
	// the single-field form of this guard.
	if !Scripting(ctx) {
		s.Name, s.Subsystem, s.Command = req.Name, req.Subsystem, req.Command
		if sendDatabase {
			s.Database = req.Database
		}
		s.OnSuccessAction, s.OnSuccessStepID = req.OnSuccessAction, req.OnSuccessStepID
		s.OnFailAction, s.OnFailStepID = req.OnFailAction, req.OnFailStepID
		s.RetryAttempts, s.RetryInterval = req.RetryAttempts, req.RetryInterval
		s.OutputFileName = req.OutputFileName
	}
	return nil
}

// Delete removes the job step via sp_delete_jobstep.
func (s *JobStep) Delete() error {
	return s.DeleteContext(context.Background())
}

// DeleteContext is the context-aware variant of Delete.
//
// The step is addressed by its number, which is what sp_delete_jobstep takes:
// a *JobStep is a snapshot, and its StepID is only current until something
// renumbers the job.
func (s *JobStep) DeleteContext(ctx context.Context) error {
	return s.job.deleteStepAt(ctx, s.StepID)
}

// deleteStepAt removes the step currently numbered stepID, without needing a
// *JobStep for it, for a caller holding a step number rather than the step.
// JobStep.DeleteContext is this with the number taken off the step.
func (j *Job) deleteStepAt(ctx context.Context, stepID int) error {
	if err := j.server.execContext(ctx, deleteStepStmt(j.Name, stepID)); err != nil {
		return fmt.Errorf("gosmo: delete step %d of job %q: %w", stepID, j.Name, err)
	}
	return nil
}

// deleteStepStmt renders the sp_delete_jobstep call. See addStepStmt for why
// it is separable from the method that issues it.
func deleteStepStmt(jobName string, stepID int) string {
	return fmt.Sprintf("EXEC msdb.dbo.sp_delete_jobstep @job_name = N'%s', @step_id = %d",
		escapeSingle(jobName), stepID)
}

// AddSchedule attaches a schedule to the job.
func (j *Job) AddSchedule(req JobScheduleRequest) error {
	return j.AddScheduleContext(context.Background(), req)
}

// AddScheduleContext is the context-aware variant of AddSchedule.
func (j *Job) AddScheduleContext(ctx context.Context, req JobScheduleRequest) error {
	q := fmt.Sprintf(
		"EXEC msdb.dbo.sp_add_jobschedule @job_name = N'%s', @name = N'%s', "+
			"@enabled = %d, @freq_type = %d, @freq_interval = %d, "+
			"@freq_subday_type = %d, @freq_subday_interval = %d, "+
			"@active_start_time = %d, @active_end_time = %d",
		escapeSingle(j.Name), escapeSingle(req.Name),
		boolToInt(req.Enabled), req.FreqType, req.FreqInterval,
		req.FreqSubdayType, req.FreqSubdayInterval,
		req.ActiveStartTime, req.ActiveEndTime,
	)
	if err := j.server.execContext(ctx, q); err != nil {
		return fmt.Errorf("gosmo: add schedule %q to job %q: %w", req.Name, j.Name, err)
	}
	return nil
}

// ============================================================
// Supporting types
// ============================================================

// JobStep represents one step of an agent job.
type JobStep struct {
	job       *Job
	StepID    int
	Name      string
	Subsystem string // "TSQL", "CmdExec", "SSIS", etc.
	Command   string
	// Database is only meaningful for TSQL steps.
	Database        string
	OnSuccessAction int
	// OnSuccessStepID is the target step_id when OnSuccessAction is
	// "go to step N" (4); otherwise unused.
	OnSuccessStepID int
	OnFailAction    int
	// OnFailStepID is the target step_id when OnFailAction is "go to
	// step N" (4); otherwise unused.
	OnFailStepID   int
	LastRunOutcome JobOutcome
	// LastRunDate is when the step last ran, from sysjobsteps'
	// last_run_date/last_run_time integer pair. Zero when the step has never
	// run — test it with IsZero(), not against a sentinel date.
	LastRunDate time.Time
	// LastRunDuration is sysjobsteps.last_run_duration verbatim: an HHMMSS
	// integer, not a count of seconds (10230 is 1h 02m 30s). LastRunElapsed
	// is the same value decoded, and is what display code should use.
	LastRunDuration int
	LastRunElapsed  time.Duration
	RetryAttempts   int
	RetryInterval   int
	OutputFileName  string
	// Flags is the raw sysjobsteps.flags bitmask (append-to-output-file,
	// log-to-table, include-step-output-in-history, ...). See Microsoft's
	// sp_add_jobstep documentation for bit meanings; gosmo round-trips it
	// as-is rather than decoding it into named booleans.
	Flags int
	// ProxyName is the proxy account the step runs under, or "" for none
	// (the Agent service account). Resolved from sysjobsteps.proxy_id.
	ProxyName string
	// AdditionalParameters is sysjobsteps.additional_parameters, used by
	// some subsystems and left empty by TSQL steps.
	AdditionalParameters string
	// CmdExecSuccessCode is the process exit code a CmdExec step treats as
	// success. Zero for every other subsystem, and also the CmdExec default.
	CmdExecSuccessCode int
	// Server is sysjobsteps.server, the target server for a replication or
	// analysis-services step; "" for the common case.
	Server string
	// DatabaseUserName is the user a TSQL step impersonates, or "" to run as
	// the job owner's mapping.
	DatabaseUserName string
	// OSRunPriority is the process priority for a CmdExec step.
	OSRunPriority int
}

// JobHistoryEntry represents one row from msdb.dbo.sysjobhistory.
type JobHistoryEntry struct {
	// JobName is only populated by Server.JobHistory (the cross-job
	// history query); it's left "" by Job.History, whose caller already
	// knows which job it asked about.
	JobName  string
	RunDate  time.Time
	Duration time.Duration
	Outcome  JobOutcome
	Message  string
	StepID   int
	StepName string
}

// SetFlow changes only the step's control flow — what happens after it
// succeeds or fails — leaving its command, proxy, flags and everything else
// untouched.
//
// Every other parameter is omitted, which sp_update_jobstep reads as "leave
// alone". That is what makes this usable for repairing references after a
// reorder, where rewriting the whole definition would be both wasteful and a
// chance to lose a column the request does not model.
func (s *JobStep) SetFlow(onSuccessAction, onSuccessStepID, onFailAction, onFailStepID int) error {
	return s.SetFlowContext(context.Background(), onSuccessAction, onSuccessStepID, onFailAction, onFailStepID)
}

// SetFlowContext is the context-aware variant of SetFlow.
func (s *JobStep) SetFlowContext(ctx context.Context, onSuccessAction, onSuccessStepID, onFailAction, onFailStepID int) error {
	q := setFlowStmt(s.job.Name, s.StepID, onSuccessAction, onSuccessStepID, onFailAction, onFailStepID)
	if err := s.job.server.execContext(ctx, q); err != nil {
		return fmt.Errorf("gosmo: set flow of step %q of job %q: %w", s.Name, s.job.Name, err)
	}
	if !Scripting(ctx) {
		s.OnSuccessAction, s.OnSuccessStepID = onSuccessAction, onSuccessStepID
		s.OnFailAction, s.OnFailStepID = onFailAction, onFailStepID
	}
	return nil
}

// setFlowStmt renders the sp_update_jobstep call SetFlow issues. Every other
// parameter is omitted, which sp_update_jobstep reads as "leave alone". See
// addStepStmt for why it is separable from the method that issues it.
func setFlowStmt(jobName string, stepID, onSuccessAction, onSuccessStepID, onFailAction, onFailStepID int) string {
	return fmt.Sprintf(
		"EXEC msdb.dbo.sp_update_jobstep @job_name = N'%s', @step_id = %d, "+
			"@on_success_action = %d, @on_success_step_id = %d, "+
			"@on_fail_action = %d, @on_fail_step_id = %d",
		escapeSingle(jobName), stepID,
		onSuccessAction, onSuccessStepID, onFailAction, onFailStepID,
	)
}

// goToStepAction is on_success_action / on_fail_action's "go to step N".
// Every other value ignores the accompanying step id.
const goToStepAction = 4

// MoveStep moves the step at position stepID to position newStepID,
// renumbering the steps in between. See MoveStepContext.
func (j *Job) MoveStep(stepID, newStepID int) error {
	return j.MoveStepContext(context.Background(), stepID, newStepID)
}

// MoveStepContext moves one step to another position, which is what "move up"
// and "move down" in a job's step list amount to.
//
// msdb has no procedure that renumbers a step in place, so the move is a
// delete followed by an insert at the target position — which is why the
// step's whole definition has to survive the round trip, and why JobStep and
// JobStepRequest model every sysjobsteps column that sp_add_jobstep can set
// rather than the handful a step form edits.
//
// The delete and the insert are one transactional batch, because a failure
// between them would leave the step deleted and its definition nowhere but in
// gosmo's memory. See atomicBatch.
//
// "Go to step N" references follow the steps they name. sp_add_jobstep
// remaps them on insert, but sp_delete_jobstep does not — it resets a
// reference to a step at or after the deleted one to "quit with success",
// silently (verified against SQL Server 2025). So every reference is written
// back afterwards from the pre-move reading, mapped through the move. A
// reference that pointed at the moved step still points at it; one that
// pointed at a step the move shifted follows that step.
func (j *Job) MoveStepContext(ctx context.Context, stepID, newStepID int) error {
	return j.ReorderStepsContext(ctx, moveOrder(stepID, newStepID))
}

// moveOrder expresses a single move as the reorder ReorderSteps takes: the
// current step ids in the order they should end up in. It is written against
// a step count it does not know, so it returns a function of it.
func moveOrder(stepID, newStepID int) func(n int) []int {
	return func(n int) []int {
		order := make([]int, 0, n)
		for id := 1; id <= n; id++ {
			if id != stepID {
				order = append(order, id)
			}
		}
		at := min(max(newStepID-1, 0), len(order))
		return slices.Insert(order, at, stepID)
	}
}

// ReorderSteps puts the job's steps into the given order. See
// ReorderStepsContext.
func (j *Job) ReorderSteps(order func(n int) []int) error {
	return j.ReorderStepsContext(context.Background(), order)
}

// ReorderStepsContext rewrites the job's step order. order is given the
// current number of steps and returns the current step ids in the sequence
// they should end up in — every id exactly once.
//
// The reorder is realised as delete-and-insert per step that has to move,
// fewest first, and every "go to step N" reference is rewritten afterwards
// through the composed mapping. See MoveStepContext for why both halves are
// necessary.
//
// All of it goes to the server as a single transactional batch, so the job is
// either in the requested order or in the order it started in, and never in
// the state between a step's delete and its re-insert — where the step exists
// nowhere but in this function. See atomicBatch.
//
// The step listing that decides all this is read outside the transaction, so
// a concurrent edit of the same job is still last-writer-wins; the batch
// makes the reorder atomic, not serializable.
//
// The job must have been read with JobByName: the step listing is by job_id,
// which a bare Server.Job handle does not carry.
func (j *Job) ReorderStepsContext(ctx context.Context, order func(n int) []int) error {
	steps, err := j.StepsContext(ctx)
	if err != nil {
		return err
	}
	want := order(len(steps))
	if err := checkReorder(want, len(steps)); err != nil {
		return fmt.Errorf("gosmo: reorder steps of job %q: %w", j.Name, err)
	}

	byID := make(map[int]*JobStep, len(steps))
	for _, s := range steps {
		byID[s.StepID] = s
	}
	// current holds the original step ids in their present order, and is kept
	// in step with the server as each move is applied.
	current := make([]int, len(steps))
	for i, s := range steps {
		current[i] = s.StepID
	}

	// Every statement is built first and issued as one transactional batch.
	// Nothing here may be applied on its own: a move is a delete and an insert,
	// and between them the step exists only in this function's memory, so a
	// failure there loses it for good. The reference-repair pass below is just
	// as unskippable — sp_delete_jobstep resets a reference to the step it
	// deleted, so a reorder that stops before the repair leaves the job's
	// control flow silently rewritten to "quit with success". See atomicBatch.
	var stmts []string

	for target := 0; target < len(want); target++ {
		if current[target] == want[target] {
			continue
		}
		from := slices.Index(current, want[target])
		s := byID[want[target]]
		stmts = append(stmts, deleteStepStmt(j.Name, from+1))
		current = slices.Delete(current, from, from+1)
		// References are repaired in one pass at the end, so the step goes
		// back with the flow it had; only its position is being decided here.
		stmts = append(stmts, addStepStmt(j.Name, stepRequestFrom(s, s.OnSuccessStepID, s.OnFailStepID), target+1))
		current = slices.Insert(current, target, want[target])
	}

	// position[originalID] is where that step ended up, which is what a
	// reference naming the original id has to become.
	position := make(map[int]int, len(current))
	for i, id := range current {
		position[id] = i + 1
	}
	for _, id := range current {
		s := byID[id]
		if s.OnSuccessAction != goToStepAction && s.OnFailAction != goToStepAction {
			continue
		}
		stmts = append(stmts, setFlowStmt(j.Name, position[id],
			s.OnSuccessAction, position[s.OnSuccessStepID],
			s.OnFailAction, position[s.OnFailStepID]))
	}

	if len(stmts) == 0 {
		return nil
	}
	if err := j.server.execContext(ctx, atomicBatch(stmts)); err != nil {
		return fmt.Errorf("gosmo: reorder steps of job %q: %w", j.Name, err)
	}
	return nil
}

// checkReorder insists the requested order is a permutation of 1..n. A
// duplicate or a missing id would delete a step and never put it back.
func checkReorder(want []int, n int) error {
	if len(want) != n {
		return fmt.Errorf("order has %d steps, the job has %d", len(want), n)
	}
	seen := make(map[int]bool, n)
	for _, id := range want {
		if id < 1 || id > n {
			return fmt.Errorf("step id %d is outside 1..%d", id, n)
		}
		if seen[id] {
			return fmt.Errorf("step id %d appears twice", id)
		}
		seen[id] = true
	}
	return nil
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

// JobStepRequest describes a step to add to, or replace the definition of
// (see JobStep.Update), a job.
type JobStepRequest struct {
	Name      string
	Subsystem string // "TSQL" is the most common value
	Command   string
	// Database is only used for TSQL steps.
	//
	// Empty means "leave the step's own database alone" on an update, not
	// "clear it": sp_update_jobstep accepts N'' without error and changes
	// nothing, so JobStep.UpdateContext omits @database_name entirely rather
	// than sending a value that would be silently ignored. On AddStep an
	// empty value likewise sends no @database_name, and the server applies
	// its default. There is no way to null the column through this type,
	// because msdb offers none.
	Database string
	// OnSuccessAction: 1=quit success, 2=quit fail, 3=go to next step, 4=go to step N.
	OnSuccessAction int
	// OnSuccessStepID is the target step_id when OnSuccessAction is 4.
	OnSuccessStepID int
	OnFailAction    int
	// OnFailStepID is the target step_id when OnFailAction is 4.
	OnFailStepID  int
	RetryAttempts int
	// RetryInterval is in minutes.
	RetryInterval int
	// OutputFileName is sent on every update, empty or not — unlike
	// Database, whose empty value means "keep". @output_file_name does
	// honour N'': it nulls the column, so blanking this field is how a
	// caller's form clears a step's output file. Two string fields of this
	// struct therefore read an empty value differently, because msdb does.
	OutputFileName string
	// Flags is the raw sysjobsteps.flags bitmask.
	//
	// This field and the six below it are sent by AddStep and InsertStep,
	// which create a row and so decide every column of it. UpdateContext
	// deliberately does not send them: an omitted sp_update_jobstep
	// parameter means "leave alone", which is what an edit of the fields a
	// step form owns should do to a step's proxy, flags and run-as user.
	Flags int
	// ProxyName, AdditionalParameters, Server and DatabaseUserName are sent
	// only when non-empty: msdb reads an omitted parameter as "leave alone"
	// on an update and "use the default" on an add, while N'' for a proxy or
	// a user name is an error rather than a clear.
	ProxyName            string
	AdditionalParameters string
	Server               string
	DatabaseUserName     string
	// CmdExecSuccessCode and OSRunPriority are sent verbatim, zero included:
	// zero is msdb's own default for both, so it cannot be told from unset
	// and there is nothing to lose by sending it.
	CmdExecSuccessCode int
	OSRunPriority      int
}

// stepRequestFrom builds the request that recreates s exactly, which is what
// a move has to pass to sp_add_jobstep: every column msdb would otherwise
// default away. onSuccessStepID and onFailStepID are given rather than taken
// from s, because a move renumbers what a "go to step N" reference means.
func stepRequestFrom(s *JobStep, onSuccessStepID, onFailStepID int) JobStepRequest {
	return JobStepRequest{
		Name: s.Name, Subsystem: s.Subsystem, Command: s.Command, Database: s.Database,
		OnSuccessAction: s.OnSuccessAction, OnSuccessStepID: onSuccessStepID,
		OnFailAction: s.OnFailAction, OnFailStepID: onFailStepID,
		RetryAttempts: s.RetryAttempts, RetryInterval: s.RetryInterval,
		OutputFileName: s.OutputFileName, Flags: s.Flags,
		ProxyName: s.ProxyName, AdditionalParameters: s.AdditionalParameters,
		Server: s.Server, DatabaseUserName: s.DatabaseUserName,
		CmdExecSuccessCode: s.CmdExecSuccessCode, OSRunPriority: s.OSRunPriority,
	}
}

// JobScheduleRequest describes a schedule to attach to a job.
type JobScheduleRequest struct {
	Name    string
	Enabled bool
	// FreqType: 1=once, 4=daily, 8=weekly, 16=monthly, 64=when agent starts.
	FreqType     int
	FreqInterval int
	// FreqSubdayType: 1=once, 2=seconds, 4=minutes, 8=hours.
	FreqSubdayType     int
	FreqSubdayInterval int
	// ActiveStartTime and ActiveEndTime are HHMMSS integers, e.g. 23000 = 02:30:00.
	ActiveStartTime int
	ActiveEndTime   int
}

// ============================================================
// Date/time helpers for sysjobhistory integer columns
// ============================================================

func parseSQLAgentDate(runDate, runTime int) time.Time {
	y := runDate / 10000
	m := (runDate % 10000) / 100
	d := runDate % 100
	h := runTime / 10000
	min := (runTime % 10000) / 100
	s := runTime % 100
	return time.Date(y, time.Month(m), d, h, min, s, 0, time.Local)
}

func parseSQLAgentDuration(dur int) time.Duration {
	return time.Duration(dur/10000)*time.Hour +
		time.Duration((dur%10000)/100)*time.Minute +
		time.Duration(dur%100)*time.Second
}
