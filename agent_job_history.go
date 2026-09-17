package gosmo

// agent_job_history.go is what a job did: msdb.dbo.sysjobhistory for one job or
// for the whole instance, and the decoding of its YYYYMMDD / HHMMSS integer
// columns. The job itself is in agent_job.go.

import (
	"context"
	"fmt"
	"time"
)

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

// parseSQLAgentDate decodes msdb's YYYYMMDD / HHMMSS integer column pairs.
//
// The columns carry the server's wall clock with no time zone, so the result
// is stamped UTC — the convention this library holds everywhere a zoneless
// server clock is decoded (see ErrorLogEntry.Date), and the one go-mssqldb
// already uses for real datetime columns. Stamping time.Local instead would
// make these values silently uncomparable with the datetime-derived ones
// beside them, such as Job.LastRunDate, by the client's UTC offset.
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
