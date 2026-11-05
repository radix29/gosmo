package gosmo

// agent_job_history.go is what a job did: msdb.dbo.sysjobhistory for one job or
// for the whole instance, and the decoding of its YYYYMMDD / HHMMSS integer
// columns. The job itself is in agent_job.go.

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// historyProcTable captures sp_help_jobhistory's FULL result set in @h, under
// sysjobhistory's own column names, for a login without sysadmin (see
// agentRead); the caller appends the procedure's filter arguments. The
// procedure joins sysjobs_view, so it applies the Agent roles' visibility.
// It is handed only filters it applies exactly as the direct read's WHERE
// does — its run_time bounds are per day, not a point in time — and the
// outer select applies the rest, so both reads return the same rows.
const historyProcTable = `
DECLARE @h TABLE (instance_id int, job_id uniqueidentifier, job_name sysname, step_id int,
  step_name sysname, sql_message_id int, sql_severity int, message nvarchar(max),
  run_status int, run_date int, run_time int, run_duration int,
  operator_emailed nvarchar(max), operator_netsent nvarchar(max), operator_paged nvarchar(max),
  retries_attempted int, server nvarchar(max));
INSERT INTO @h EXECUTE msdb.dbo.sp_help_jobhistory @mode = 'FULL'`

// History returns the execution history (most recent first).
// Pass limit=0 to use the default of 100 rows. On a JobRef the job is looked
// up by name first (see Server.JobRef).
//
// instance_id breaks ties: a run's step-0 (job outcome) row carries the run's
// start time, the same run_date/run_time as its first step, so without it
// the two came back in either order. Agent writes the outcome row last, so
// it sorts first, above the steps it summarises.
func (j *Job) History(ctx context.Context, limit int) ([]*JobHistoryEntry, error) {
	if limit <= 0 {
		limit = 100
	}
	sel := `
SELECT TOP %d
       run_date, run_time, run_duration,
       run_status, ISNULL(message, ''), step_id, step_name
FROM   %s
WHERE  job_id = @p1
ORDER  BY run_date DESC, run_time DESC, instance_id DESC`
	q := agentRead(fmt.Sprintf(sel, limit, "msdb.dbo.sysjobhistory"),
		historyProcTable+", @job_id = @p1;"+fmt.Sprintf(sel, limit, "@h"))

	what := fmt.Sprintf("history for job %q", j.Name)
	id, err := j.id(ctx, what)
	if err != nil {
		return nil, err
	}
	rows, err := j.server.query(ctx, q, id)
	return scanRows(rows, err, what, func(scan func(...any) error) (*JobHistoryEntry, error) {
		h := &JobHistoryEntry{JobName: j.Name}
		var runDate, runTime, runDur int
		if err := scan(&runDate, &runTime, &runDur,
			&h.Outcome, &h.Message, &h.StepID, &h.StepName); err != nil {
			return nil, err
		}
		h.RunDate = parseSQLAgentDate(runDate, runTime)
		h.Duration = parseSQLAgentDuration(runDur)
		return h, nil
	})
}

// JobHistory returns the most recent job-level history entries (step_id =
// 0, i.e. the overall outcome of each run rather than a single step's)
// across every job, most recent first. Pass limit=0 for the default of 100
// rows. SearchJobHistory narrows the same read by job, time and outcome.
func (s *Server) JobHistory(ctx context.Context, limit int) ([]*JobHistoryEntry, error) {
	return s.SearchJobHistory(ctx, JobHistoryFilter{Max: limit})
}

// JobHistoryFilter narrows Server.SearchJobHistory. The zero value reads the
// newest 100 job outcome rows of every job.
type JobHistoryFilter struct {
	// JobName keeps one job's rows, matched exactly as msdb compares names;
	// empty keeps every job's. A name no job has returns no rows.
	JobName string
	// Since and Until bound the run's start time, inclusive: Since keeps
	// runs that started at or after it, Until runs that started at or before
	// it; zero leaves that end open. msdb records the server's wall clock
	// with no zone, and JobHistoryEntry.RunDate carries it stamped UTC (see
	// parseSQLAgentDate): a bound is compared the same way, by its UTC
	// fields, so pass a value built that way — a RunDate, or a server-local
	// time put in time.UTC.
	Since, Until time.Time
	// Outcomes keeps rows with one of these outcomes; empty keeps all.
	// {JobOutcomeFailed} is "the runs that failed".
	Outcomes []JobOutcome
	// IncludeSteps adds each run's step rows to its job outcome row
	// (step_id 0), which is all that is read without it.
	IncludeSteps bool
	// Max caps the rows read, newest first; 0 or less means 100.
	Max int
}

// SearchJobHistory returns msdb.dbo.sysjobhistory rows across every job (or
// the one f names), newest first, narrowed by f — the instance-wide read that
// finds a failure even when a later run of the same job succeeded, which
// Job.LastRunOutcome cannot.
func (s *Server) SearchJobHistory(ctx context.Context, f JobHistoryFilter) ([]*JobHistoryEntry, error) {
	limit := f.Max
	if limit <= 0 {
		limit = 100
	}
	var where []string
	var args []any
	arg := func(v any) string {
		args = append(args, v)
		return fmt.Sprintf("@p%d", len(args))
	}
	if !f.IncludeSteps {
		where = append(where, "h.step_id = 0")
	}
	if f.JobName != "" {
		where = append(where, "j.name = "+arg(f.JobName))
	}
	if !f.Since.IsZero() {
		d, t := sqlAgentDateTime(f.Since)
		pd, pt := arg(d), arg(t)
		where = append(where, fmt.Sprintf("(h.run_date > %s OR (h.run_date = %s AND h.run_time >= %s))", pd, pd, pt))
	}
	if !f.Until.IsZero() {
		d, t := sqlAgentDateTime(f.Until)
		pd, pt := arg(d), arg(t)
		where = append(where, fmt.Sprintf("(h.run_date < %s OR (h.run_date = %s AND h.run_time <= %s))", pd, pd, pt))
	}
	if len(f.Outcomes) > 0 {
		in := make([]string, len(f.Outcomes))
		for i, o := range f.Outcomes {
			in[i] = arg(int(o))
		}
		where = append(where, "h.run_status IN ("+strings.Join(in, ", ")+")")
	}
	cond := ""
	if len(where) > 0 {
		cond = "\nWHERE  " + strings.Join(where, "\n  AND  ")
	}
	sel := `
SELECT TOP %d
       j.name, h.run_date, h.run_time, h.run_duration,
       h.run_status, ISNULL(h.message, ''), h.step_id, h.step_name
FROM   %s h
JOIN   %s j ON j.job_id = h.job_id%s
ORDER  BY h.run_date DESC, h.run_time DESC, h.instance_id DESC`
	procArgs := ";"
	if !f.IncludeSteps {
		procArgs = ", @step_id = 0;"
	}
	q := agentRead(fmt.Sprintf(sel, limit, "msdb.dbo.sysjobhistory", "msdb.dbo.sysjobs", cond),
		historyProcTable+procArgs+fmt.Sprintf(sel, limit, "@h", "msdb.dbo.sysjobs_view", cond))

	rows, err := s.query(ctx, q, args...)
	return scanRows(rows, err, "job history", func(scan func(...any) error) (*JobHistoryEntry, error) {
		h := &JobHistoryEntry{}
		var runDate, runTime, runDur int
		if err := scan(&h.JobName, &runDate, &runTime, &runDur,
			&h.Outcome, &h.Message, &h.StepID, &h.StepName); err != nil {
			return nil, err
		}
		h.RunDate = parseSQLAgentDate(runDate, runTime)
		h.Duration = parseSQLAgentDuration(runDur)
		return h, nil
	})
}

// sqlAgentDateTime is parseSQLAgentDate's inverse: t's UTC fields as msdb's
// YYYYMMDD and HHMMSS integers.
func sqlAgentDateTime(t time.Time) (date, clock int) {
	t = t.UTC()
	return t.Year()*10000 + int(t.Month())*100 + t.Day(), t.Hour()*10000 + t.Minute()*100 + t.Second()
}

// JobHistoryEntry represents one row from msdb.dbo.sysjobhistory.
type JobHistoryEntry struct {
	JobName  string
	RunDate  time.Time
	Duration time.Duration
	Outcome  JobOutcome
	Message  string
	StepID   int
	StepName string
}

// parseSQLAgentDate decodes msdb's YYYYMMDD / HHMMSS integer column pairs.
//
// The columns carry the server's wall clock with no time zone, so the result
// is stamped UTC — the convention this library holds everywhere a zoneless
// server clock is decoded (see ErrorLogEntry.Date), and the one go-mssqldb
// already uses for real datetime columns. Stamping time.Local instead would
// make these values silently uncomparable with the datetime-derived ones
// beside them, such as Job.NextRunDate, by the client's UTC offset.
func parseSQLAgentDate(runDate, runTime int) time.Time {
	y := runDate / 10000
	m := (runDate % 10000) / 100
	d := runDate % 100
	h := runTime / 10000
	min := (runTime % 10000) / 100
	s := runTime % 100
	return time.Date(y, time.Month(m), d, h, min, s, 0, time.UTC)
}

func parseSQLAgentDuration(dur int) time.Duration {
	return time.Duration(dur/10000)*time.Hour +
		time.Duration((dur%10000)/100)*time.Minute +
		time.Duration(dur%100)*time.Second
}
