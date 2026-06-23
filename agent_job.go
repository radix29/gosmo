package gosmo

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

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

// Job mirrors a row in msdb.dbo.sysjobs together with its latest activity.
type Job struct {
	server          *Server
	JobID           string
	Name            string
	Description     string
	IsEnabled       bool
	Category        string
	OwnerLoginName  string
	DateCreated     time.Time
	DateModified    time.Time
	StartStepID     int
	LastRunDate     time.Time
	LastRunOutcome  JobOutcome
	LastRunDuration time.Duration
	NextRunDate     time.Time
	CurrentState    JobState
}

// Jobs returns all SQL Server Agent jobs from msdb.
func (s *Server) Jobs() ([]*Job, error) {
	return s.JobsContext(context.Background())
}

// JobsContext is the context-aware variant of Jobs.
func (s *Server) JobsContext(ctx context.Context) ([]*Job, error) {
	const q = `
SELECT j.job_id, j.name, ISNULL(j.description,''),
       j.enabled, ISNULL(c.name,''), ISNULL(l.name,''),
       j.date_created, j.date_modified, j.start_step_id,
       ja.last_executed_step_date,
       ISNULL(ja.last_run_outcome, 5),
       ISNULL(ja.last_run_duration, 0),
       ISNULL(ja.next_scheduled_run_date, '19000101'),
       ISNULL(ja.job_state, 1)
FROM   msdb.dbo.sysjobs j
LEFT   JOIN msdb.dbo.syscategories c ON c.category_id = j.category_id
LEFT   JOIN master.sys.server_principals l ON l.sid = j.owner_sid
LEFT   JOIN msdb.dbo.sysjobactivity ja
       ON  ja.job_id = j.job_id
       AND ja.session_id = (SELECT MAX(session_id) FROM msdb.dbo.sysjobactivity)
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
SELECT j.job_id, j.name, ISNULL(j.description,''),
       j.enabled, ISNULL(c.name,''), ISNULL(l.name,''),
       j.date_created, j.date_modified, j.start_step_id,
       ja.last_executed_step_date,
       ISNULL(ja.last_run_outcome, 5),
       ISNULL(ja.last_run_duration, 0),
       ISNULL(ja.next_scheduled_run_date, '19000101'),
       ISNULL(ja.job_state, 1)
FROM   msdb.dbo.sysjobs j
LEFT   JOIN msdb.dbo.syscategories c ON c.category_id = j.category_id
LEFT   JOIN master.sys.server_principals l ON l.sid = j.owner_sid
LEFT   JOIN msdb.dbo.sysjobactivity ja
       ON  ja.job_id = j.job_id
       AND ja.session_id = (SELECT MAX(session_id) FROM msdb.dbo.sysjobactivity)
WHERE  j.name = @p1`

	row := s.db.QueryRowContext(ctx, q, name)
	j := &Job{server: s}
	var lastRun, nextRun sql.NullTime
	var lastOutcome, jobState, lastDuration sql.NullInt64
	if err := row.Scan(
		&j.JobID, &j.Name, &j.Description,
		&j.IsEnabled, &j.Category, &j.OwnerLoginName,
		&j.DateCreated, &j.DateModified, &j.StartStepID,
		&lastRun, &lastOutcome, &lastDuration, &nextRun, &jobState,
	); err != nil {
		if err == sql.ErrNoRows {
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
	if _, err := j.server.db.ExecContext(ctx, q); err != nil {
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
	if _, err := j.server.db.ExecContext(ctx, q); err != nil {
		return fmt.Errorf("gosmo: stop job %q: %w", j.Name, err)
	}
	return nil
}

// Enable enables the job.
func (j *Job) Enable() error { return j.setEnabled(context.Background(), true) }

// Disable disables the job.
func (j *Job) Disable() error { return j.setEnabled(context.Background(), false) }

func (j *Job) setEnabled(ctx context.Context, on bool) error {
	q := fmt.Sprintf("EXEC msdb.dbo.sp_update_job @job_name = N'%s', @enabled = %d",
		escapeSingle(j.Name), boolToInt(on))
	if _, err := j.server.db.ExecContext(ctx, q); err != nil {
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
	if _, err := j.server.db.ExecContext(ctx, q); err != nil {
		return fmt.Errorf("gosmo: drop job %q: %w", j.Name, err)
	}
	return nil
}

// Steps returns all steps defined for the job, ordered by step_id.
func (j *Job) Steps() ([]*JobStep, error) {
	return j.StepsContext(context.Background())
}

func (j *Job) StepsContext(ctx context.Context) ([]*JobStep, error) {
	const q = `
SELECT step_id, step_name, subsystem, command,
       on_success_action, on_fail_action,
       last_run_outcome, last_run_date, last_run_duration,
       retry_attempts, retry_interval
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
			&s.StepID, &s.Name, &s.Subsystem, &s.Command,
			&s.OnSuccessAction, &s.OnFailAction,
			&s.LastRunOutcome, &lastRun, &s.LastRunDuration,
			&s.RetryAttempts, &s.RetryInterval,
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
       run_status, message, step_id, step_name
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

// CreateJob creates a new SQL Server Agent job.
func (s *Server) CreateJob(req CreateJobRequest) (*Job, error) {
	return s.CreateJobContext(context.Background(), req)
}

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
	if _, err := s.db.ExecContext(ctx, q); err != nil {
		return nil, fmt.Errorf("gosmo: create job %q: %w", req.Name, err)
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
			"@on_success_action = %d, @on_fail_action = %d, "+
			"@retry_attempts = %d, @retry_interval = %d",
		escapeSingle(j.Name), escapeSingle(req.Name),
		escapeSingle(req.Subsystem), escapeSingle(req.Command),
		req.OnSuccessAction, req.OnFailAction,
		req.RetryAttempts, req.RetryInterval,
	)
	if req.Database != "" {
		q += fmt.Sprintf(", @database_name = N'%s'", escapeSingle(req.Database))
	}
	if _, err := j.server.db.ExecContext(ctx, q); err != nil {
		return fmt.Errorf("gosmo: add step %q to job %q: %w", req.Name, j.Name, err)
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
	if _, err := j.server.db.ExecContext(ctx, q); err != nil {
		return fmt.Errorf("gosmo: add schedule %q to job %q: %w", req.Name, j.Name, err)
	}
	return nil
}

// ============================================================
// Supporting types
// ============================================================

// JobStep represents one step of an agent job.
type JobStep struct {
	job             *Job
	StepID          int
	Name            string
	Subsystem       string // "TSQL", "CmdExec", "SSIS", etc.
	Command         string
	OnSuccessAction int
	OnFailAction    int
	LastRunOutcome  JobOutcome
	LastRunDuration int
	RetryAttempts   int
	RetryInterval   int
}

// JobHistoryEntry represents one row from msdb.dbo.sysjobhistory.
type JobHistoryEntry struct {
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

// JobStepRequest describes a step to add to a job.
type JobStepRequest struct {
	Name      string
	Subsystem string // "TSQL" is the most common value
	Command   string
	// Database is only used for TSQL steps.
	Database string
	// OnSuccessAction: 1=quit success, 2=quit fail, 3=go to next step, 4=go to step N.
	OnSuccessAction int
	OnFailAction    int
	RetryAttempts   int
	// RetryInterval is in minutes.
	RetryInterval int
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
