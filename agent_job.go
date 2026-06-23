package gosmo

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// ============================================================
// SQL Server Agent – Jobs
// ============================================================

// JobState mirrors the msdb.dbo.sysjobs current_execution_status.
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

// Job mirrors msdb.dbo.sysjobs.
type Job struct {
	server         *Server
	JobID          string
	Name           string
	Description    string
	IsEnabled      bool
	Category       string
	OwnerLoginName string
	DateCreated    time.Time
	DateModified   time.Time
	StartStepID    int
	// Last run info
	LastRunDate     time.Time
	LastRunOutcome  JobOutcome
	LastRunDuration time.Duration
	NextRunDate     time.Time
	CurrentState    JobState
}

// Jobs returns all SQL Server Agent jobs from msdb.
func (s *Server) Jobs() ([]*Job, error) {
	const q = `
SELECT j.job_id, j.name, ISNULL(j.description,''),
       j.enabled, c.name AS category, ISNULL(l.name,'') AS owner,
       j.date_created, j.date_modified, j.start_step_id,
       ja.last_executed_step_date,
       ISNULL(ja.last_run_outcome, 5),
       ISNULL(ja.last_run_duration, 0),
       ISNULL(ja.next_scheduled_run_date, '19000101'),
       ISNULL(ja.job_state, 1)
FROM   msdb.dbo.sysjobs j
LEFT   JOIN msdb.dbo.syscategories c ON c.category_id = j.category_id
LEFT   JOIN master.sys.server_principals l ON l.sid = j.owner_sid
LEFT   JOIN msdb.dbo.sysjobactivity ja ON ja.job_id = j.job_id
       AND ja.session_id = (SELECT MAX(session_id) FROM msdb.dbo.sysjobactivity)
ORDER  BY j.name`

	rows, err := s.db.QueryContext(context.Background(), q)
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
		// duration is stored as HHMMSS integer e.g. 10230 = 1h 2m 30s
		d := lastDuration.Int64
		h, m, sc := d/10000, (d%10000)/100, d%100
		j.LastRunDuration = time.Duration(h)*time.Hour +
			time.Duration(m)*time.Minute +
			time.Duration(sc)*time.Second
		jobs = append(jobs, j)
	}
	return jobs, rows.Err()
}

// JobByName returns a single job by name.
func (s *Server) JobByName(name string) (*Job, error) {
	jobs, err := s.Jobs()
	if err != nil {
		return nil, err
	}
	for _, j := range jobs {
		if strings.EqualFold(j.Name, name) {
			return j, nil
		}
	}
	return nil, fmt.Errorf("gosmo: agent job %q not found", name)
}

// Start starts the agent job.
func (j *Job) Start(stepName string) error {
	q := fmt.Sprintf("EXEC msdb.dbo.sp_start_job @job_name = N'%s'", escapeSingle(j.Name))
	if stepName != "" {
		q += fmt.Sprintf(", @step_name = N'%s'", escapeSingle(stepName))
	}
	_, err := j.server.db.ExecContext(context.Background(), q)
	if err != nil {
		return fmt.Errorf("gosmo: start job %q: %w", j.Name, err)
	}
	return nil
}

// Stop stops a running agent job.
func (j *Job) Stop() error {
	_, err := j.server.db.ExecContext(context.Background(),
		fmt.Sprintf("EXEC msdb.dbo.sp_stop_job @job_name = N'%s'", escapeSingle(j.Name)))
	if err != nil {
		return fmt.Errorf("gosmo: stop job %q: %w", j.Name, err)
	}
	return nil
}

// Enable enables the job.
func (j *Job) Enable() error {
	_, err := j.server.db.ExecContext(context.Background(),
		fmt.Sprintf("EXEC msdb.dbo.sp_update_job @job_name = N'%s', @enabled = 1", escapeSingle(j.Name)))
	if err != nil {
		return fmt.Errorf("gosmo: enable job %q: %w", j.Name, err)
	}
	j.IsEnabled = true
	return nil
}

// Disable disables the job.
func (j *Job) Disable() error {
	_, err := j.server.db.ExecContext(context.Background(),
		fmt.Sprintf("EXEC msdb.dbo.sp_update_job @job_name = N'%s', @enabled = 0", escapeSingle(j.Name)))
	if err != nil {
		return fmt.Errorf("gosmo: disable job %q: %w", j.Name, err)
	}
	j.IsEnabled = false
	return nil
}

// Drop drops the agent job.
func (j *Job) Drop() error {
	_, err := j.server.db.ExecContext(context.Background(),
		fmt.Sprintf("EXEC msdb.dbo.sp_delete_job @job_name = N'%s'", escapeSingle(j.Name)))
	if err != nil {
		return fmt.Errorf("gosmo: drop job %q: %w", j.Name, err)
	}
	return nil
}

// Steps returns all steps for the job.
func (j *Job) Steps() ([]*JobStep, error) {
	const q = `
SELECT step_id, step_name, subsystem, command,
       on_success_action, on_fail_action,
       last_run_outcome, last_run_date, last_run_duration,
       retry_attempts, retry_interval
FROM   msdb.dbo.sysjobsteps
WHERE  job_id = ?
ORDER  BY step_id`

	rows, err := j.server.db.QueryContext(context.Background(), q, j.JobID)
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

// JobHistory returns execution history for the job (most recent first, limit n rows; 0 = 100).
func (j *Job) History(limit int) ([]*JobHistoryEntry, error) {
	if limit <= 0 {
		limit = 100
	}
	q := fmt.Sprintf(`
SELECT TOP %d
       run_date, run_time, run_duration,
       run_status, message, step_id, step_name
FROM   msdb.dbo.sysjobhistory
WHERE  job_id = ?
ORDER  BY run_date DESC, run_time DESC`, limit)

	rows, err := j.server.db.QueryContext(context.Background(), q, j.JobID)
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

// JobHistoryEntry represents one row from sysjobhistory.
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
	Category    string // leave blank for [Uncategorized (Local)]
	OwnerLogin  string
	Enabled     bool
}

// CreateJob creates a new SQL Server Agent job.
func (s *Server) CreateJob(req CreateJobRequest) (*Job, error) {
	category := req.Category
	if category == "" {
		category = "[Uncategorized (Local)]"
	}
	enabled := 0
	if req.Enabled {
		enabled = 1
	}
	q := fmt.Sprintf(`
EXEC msdb.dbo.sp_add_job
    @job_name       = N'%s',
    @description    = N'%s',
    @category_name  = N'%s',
    @enabled        = %d`,
		escapeSingle(req.Name),
		escapeSingle(req.Description),
		escapeSingle(category),
		enabled,
	)
	if req.OwnerLogin != "" {
		q += fmt.Sprintf(", @owner_login_name = N'%s'", escapeSingle(req.OwnerLogin))
	}
	_, err := s.db.ExecContext(context.Background(), q)
	if err != nil {
		return nil, fmt.Errorf("gosmo: create job %q: %w", req.Name, err)
	}
	return s.JobByName(req.Name)
}

// AddStep adds a step to the job.
func (j *Job) AddStep(req JobStepRequest) error {
	q := fmt.Sprintf(`
EXEC msdb.dbo.sp_add_jobstep
    @job_name           = N'%s',
    @step_name          = N'%s',
    @subsystem          = N'%s',
    @command            = N'%s',
    @on_success_action  = %d,
    @on_fail_action     = %d,
    @retry_attempts     = %d,
    @retry_interval     = %d`,
		escapeSingle(j.Name),
		escapeSingle(req.Name),
		escapeSingle(req.Subsystem),
		escapeSingle(req.Command),
		req.OnSuccessAction,
		req.OnFailAction,
		req.RetryAttempts,
		req.RetryInterval,
	)
	if req.Database != "" {
		q += fmt.Sprintf(", @database_name = N'%s'", escapeSingle(req.Database))
	}
	_, err := j.server.db.ExecContext(context.Background(), q)
	if err != nil {
		return fmt.Errorf("gosmo: add step %q to job %q: %w", req.Name, j.Name, err)
	}
	return nil
}

// AddSchedule creates and attaches a schedule to the job.
func (j *Job) AddSchedule(req JobScheduleRequest) error {
	q := fmt.Sprintf(`
EXEC msdb.dbo.sp_add_jobschedule
    @job_name        = N'%s',
    @name            = N'%s',
    @enabled         = %d,
    @freq_type       = %d,
    @freq_interval   = %d,
    @freq_subday_type = %d,
    @freq_subday_interval = %d,
    @active_start_time = %d,
    @active_end_time   = %d`,
		escapeSingle(j.Name),
		escapeSingle(req.Name),
		boolToInt(req.Enabled),
		req.FreqType,
		req.FreqInterval,
		req.FreqSubdayType,
		req.FreqSubdayInterval,
		req.ActiveStartTime,
		req.ActiveEndTime,
	)
	_, err := j.server.db.ExecContext(context.Background(), q)
	if err != nil {
		return fmt.Errorf("gosmo: add schedule %q to job %q: %w", req.Name, j.Name, err)
	}
	return nil
}

// ── Supporting types ──────────────────────────────────────────────────────────

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

// JobStepRequest describes a new step to add to a job.
type JobStepRequest struct {
	Name            string
	Subsystem       string // "TSQL" most common
	Command         string
	Database        string // for T-SQL steps
	OnSuccessAction int    // 1=quit with success, 2=quit with fail, 3=goto next, 4=goto step
	OnFailAction    int
	RetryAttempts   int
	RetryInterval   int // minutes
}

// JobScheduleRequest describes a schedule to attach to a job.
type JobScheduleRequest struct {
	Name               string
	Enabled            bool
	FreqType           int // 1=once, 4=daily, 8=weekly, 16=monthly, 64=when agent starts
	FreqInterval       int
	FreqSubdayType     int // 1=once, 2=seconds, 4=minutes, 8=hours
	FreqSubdayInterval int
	ActiveStartTime    int // HHMMSS as integer
	ActiveEndTime      int
}

// ── Date/time helpers for sysjobhistory ──────────────────────────────────────

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
	h := dur / 10000
	m := (dur % 10000) / 100
	s := dur % 100
	return time.Duration(h)*time.Hour +
		time.Duration(m)*time.Minute +
		time.Duration(s)*time.Second
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
