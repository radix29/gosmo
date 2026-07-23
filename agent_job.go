package gosmo

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ============================================================
// SQL Server Agent -- Status
// ============================================================

// AgentStatus reports whether SQL Server Agent is currently running, based
// only on SQL-visible state (sys.dm_server_services) — no Windows Service
// Control, WMI, or registry access, matching the SQL-only scope of the rest
// of this file.
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
	const q = `
SELECT status_desc, last_startup_time
FROM   sys.dm_server_services
WHERE  servicename LIKE N'SQL Server Agent%'`

	row := s.db.QueryRowContext(ctx, q)
	st := &AgentStatus{}
	var startup sql.NullTime
	if err := row.Scan(&st.StatusText, &startup); err != nil {
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

// ============================================================
// SQL Server Agent -- Jobs
// ============================================================

// JobState mirrors the current_execution_status values in msdb.dbo.sysjobactivity.
type JobState int

const (
	JobStateIdle                        JobState = 1
	JobStateSuspended                   JobState = 2
	JobStateExecuting                   JobState = 4
	JobStateWaitingForWorker            JobState = 5
	JobStateBetweenRetries              JobState = 6
	JobStateCancelling                  JobState = 7
	JobStatePerformingCompletionActions JobState = 8
	JobStateRunning                     JobState = 10
)

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
            THEN 4 ELSE 1 END
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

	rows, err := s.db.QueryContext(ctx, q)
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
		jobs = append(jobs, j)
	}
	return jobs, rows.Err()
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
            THEN 4 ELSE 1 END
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

	row := s.db.QueryRowContext(ctx, q, name)
	j := &Job{server: s}
	var lastRun, nextRun sql.NullTime
	var lastOutcome, jobState, lastDuration sql.NullInt64
	if err := row.Scan(
		&j.JobID, &j.Name, &j.Description,
		&j.IsEnabled, &j.Category, &j.OwnerLoginName,
		&j.DateCreated, &j.DateModified, &j.StartStepID,
		&j.DeleteLevel, &j.NotifyLevelEmail, &j.NotifyEmailOperatorName,
		&lastRun, &lastOutcome, &lastDuration, &nextRun, &jobState,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("gosmo: agent job %q not found", name)
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
	return j, nil
}

// Start starts the job, optionally from a specific step name.
func (j *Job) Start(stepName string) error {
	return j.StartContext(context.Background(), stepName)
}

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
	j.IsEnabled = on
	return nil
}

// Drop drops the agent job.
func (j *Job) Drop() error {
	return j.DropContext(context.Background())
}

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
	j.Name = newName
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
	j.Description = desc
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
	j.Category = category
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
	j.OwnerLoginName = loginName
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
	j.StartStepID = stepID
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
	j.NotifyLevelEmail = level
	if operatorName != "" {
		j.NotifyEmailOperatorName = operatorName
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
	j.DeleteLevel = level
	return nil
}

// Steps returns all steps defined for the job, ordered by step_id.
func (j *Job) Steps() ([]*JobStep, error) {
	return j.StepsContext(context.Background())
}

func (j *Job) StepsContext(ctx context.Context) ([]*JobStep, error) {
	const q = `
SELECT step_id, step_name, subsystem, command, ISNULL(database_name, ''),
       on_success_action, on_success_step_id, on_fail_action, on_fail_step_id,
       last_run_outcome, last_run_date, last_run_duration,
       retry_attempts, retry_interval, ISNULL(output_file_name, ''), flags
FROM   msdb.dbo.sysjobsteps
WHERE  job_id = @p1
ORDER  BY step_id`

	rows, err := j.server.db.QueryContext(ctx, q, j.JobID)
	if err != nil {
		return nil, fmt.Errorf("gosmo: steps for job %q: %w", j.Name, err)
	}
	defer rows.Close()

	var steps []*JobStep
	for rows.Next() {
		s := &JobStep{job: j}
		var lastRun sql.NullInt64
		if err := rows.Scan(
			&s.StepID, &s.Name, &s.Subsystem, &s.Command, &s.Database,
			&s.OnSuccessAction, &s.OnSuccessStepID, &s.OnFailAction, &s.OnFailStepID,
			&s.LastRunOutcome, &lastRun, &s.LastRunDuration,
			&s.RetryAttempts, &s.RetryInterval, &s.OutputFileName, &s.Flags,
		); err != nil {
			return nil, err
		}
		steps = append(steps, s)
	}
	return steps, rows.Err()
}

// History returns the execution history (most recent first).
// Pass limit=0 to use the default of 100 rows.
func (j *Job) History(limit int) ([]*JobHistoryEntry, error) {
	return j.HistoryContext(context.Background(), limit)
}

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

	rows, err := j.server.db.QueryContext(ctx, q, j.JobID)
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
			return nil, err
		}
		h.RunDate = parseSQLAgentDate(runDate, runTime)
		h.Duration = parseSQLAgentDuration(runDur)
		history = append(history, h)
	}
	return history, rows.Err()
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

	rows, err := s.db.QueryContext(ctx, q)
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
			return nil, err
		}
		h.RunDate = parseSQLAgentDate(runDate, runTime)
		h.Duration = parseSQLAgentDuration(runDur)
		history = append(history, h)
	}
	return history, rows.Err()
}

// CreateJob creates a new SQL Server Agent job.
func (s *Server) CreateJob(req CreateJobRequest) (*Job, error) {
	return s.CreateJobContext(context.Background(), req)
}

// CreateJobContext is the context-aware variant of CreateJob. It also
// enlists the job to run on the local server via sp_add_jobserver —
// without that, SQL Server Agent refuses to start the job (sp_start_job:
// "does not have any job server or servers defined") or let an alert
// target it (sp_update_alert/sp_add_alert: "cannot be used by an alert"),
// confirmed live against a real server. SSMS's own New Job dialog does
// this same enlistment implicitly; multi-server (MSX/TSX) target-server
// selection is out of scope here, so "(local)" is the only target.
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
	return s.JobByNameContext(ctx, req.Name)
}

// AddStep adds a T-SQL or other subsystem step to the job.
func (j *Job) AddStep(req JobStepRequest) error {
	return j.AddStepContext(context.Background(), req)
}

func (j *Job) AddStepContext(ctx context.Context, req JobStepRequest) error {
	if req.Name == "" {
		return fmt.Errorf("gosmo: add step: name is required")
	}
	q := fmt.Sprintf(
		"EXEC msdb.dbo.sp_add_jobstep @job_name = N'%s', @step_name = N'%s', "+
			"@subsystem = N'%s', @command = N'%s', "+
			"@on_success_action = %d, @on_success_step_id = %d, "+
			"@on_fail_action = %d, @on_fail_step_id = %d, "+
			"@retry_attempts = %d, @retry_interval = %d",
		escapeSingle(j.Name), escapeSingle(req.Name),
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
	if err := j.server.execContext(ctx, q); err != nil {
		return fmt.Errorf("gosmo: add step %q to job %q: %w", req.Name, j.Name, err)
	}
	return nil
}

// Update replaces the step's definition via sp_update_jobstep.
func (s *JobStep) Update(req JobStepRequest) error {
	return s.UpdateContext(context.Background(), req)
}

// UpdateContext is the context-aware variant of Update.
func (s *JobStep) UpdateContext(ctx context.Context, req JobStepRequest) error {
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
	if req.Database != "" {
		q += fmt.Sprintf(", @database_name = N'%s'", escapeSingle(req.Database))
	}
	if req.OutputFileName != "" {
		q += fmt.Sprintf(", @output_file_name = N'%s'", escapeSingle(req.OutputFileName))
	}
	if err := s.job.server.execContext(ctx, q); err != nil {
		return fmt.Errorf("gosmo: update step %q of job %q: %w", req.Name, s.job.Name, err)
	}
	s.Name, s.Subsystem, s.Command, s.Database = req.Name, req.Subsystem, req.Command, req.Database
	s.OnSuccessAction, s.OnSuccessStepID = req.OnSuccessAction, req.OnSuccessStepID
	s.OnFailAction, s.OnFailStepID = req.OnFailAction, req.OnFailStepID
	s.RetryAttempts, s.RetryInterval = req.RetryAttempts, req.RetryInterval
	s.OutputFileName = req.OutputFileName
	return nil
}

// Delete removes the job step via sp_delete_jobstep.
func (s *JobStep) Delete() error {
	return s.DeleteContext(context.Background())
}

// DeleteContext is the context-aware variant of Delete.
func (s *JobStep) DeleteContext(ctx context.Context) error {
	q := fmt.Sprintf("EXEC msdb.dbo.sp_delete_jobstep @job_name = N'%s', @step_id = %d",
		escapeSingle(s.job.Name), s.StepID)
	if err := s.job.server.execContext(ctx, q); err != nil {
		return fmt.Errorf("gosmo: delete step %d of job %q: %w", s.StepID, s.job.Name, err)
	}
	return nil
}

// AddSchedule attaches a schedule to the job.
func (j *Job) AddSchedule(req JobScheduleRequest) error {
	return j.AddScheduleContext(context.Background(), req)
}

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
	OnFailStepID    int
	LastRunOutcome  JobOutcome
	LastRunDuration int
	RetryAttempts   int
	RetryInterval   int
	OutputFileName  string
	// Flags is the raw sysjobsteps.flags bitmask (append-to-output-file,
	// log-to-table, include-step-output-in-history, ...). See Microsoft's
	// sp_add_jobstep documentation for bit meanings; gosmo round-trips it
	// as-is rather than decoding it into named booleans, since the exact
	// bit assignments are worth confirming against a live server before
	// being exposed that way.
	Flags int
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
	RetryInterval  int
	OutputFileName string
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
