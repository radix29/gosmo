package gosmo

// agent_job.go is a SQL Server Agent job: what one is, how the jobs are read
// and their running state resolved, and the job-level writes — start, stop,
// enable, rename, the msdb properties, CreateJob and AddSchedule. A job's steps
// are in agent_job_step.go and its run history in agent_job_history.go.

import (
	"context"
	"database/sql"
	"errors"
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
func (s *Server) Jobs() ([]*Job, error) {
	return s.JobsContext(context.Background())
}

// JobsContext is the context-aware variant of Jobs.
func (s *Server) JobsContext(ctx context.Context) ([]*Job, error) {
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
// context, JobByNameContext's lookup is a real read and a job whose
// sp_add_job was merely collected is not there to find.
func (s *Server) JobRef(name string) *Job {
	return &Job{server: s, Name: name}
}

// JobByName returns a single job by name using a direct parameterised query.
func (s *Server) JobByName(name string) (*Job, error) {
	return s.JobByNameContext(context.Background(), name)
}

// JobByNameContext is the context-aware variant of JobByName.
func (s *Server) JobByNameContext(ctx context.Context, name string) (*Job, error) {
	const q = jobSelect + `
WHERE  j.name = @p1`

	var j *Job
	err := s.queryRow(ctx, func(row *sql.Row) error {
		var err error
		j, err = scanJob(s, row.Scan)
		return err
	}, q, name)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, notFoundf("gosmo: agent job %q not found", name)
		}
		return nil, fmt.Errorf("gosmo: job by name: %w", err)
	}
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
//
// A level set on a job with no operator does not stick: msdb stores
// notify_level_email as 0 and reports no error, since a level with nobody to
// mail is meaningless to it. Verified on SQL Server 17.0.1135.8, 2026-09-17,
// and the batched Job.Alter behaves the same way.
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
		return s.JobRef(req.Name), nil
	}
	return s.JobByNameContext(ctx, req.Name)
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

// CreateJobRequest describes a new SQL Server Agent job.
type CreateJobRequest struct {
	Name        string
	Description string
	// Category defaults to [Uncategorized (Local)] when empty.
	Category   string
	OwnerLogin string
	Enabled    bool
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
