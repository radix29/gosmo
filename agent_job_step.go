package gosmo

// agent_job_step.go is one step of an agent job: the steps as read from
// msdb.dbo.sysjobsteps, the add/update/delete that change them, and the control
// flow and step order between them. The job itself is in agent_job.go.

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"time"
)

// Steps returns all steps defined for the job, ordered by step_id.
func (j *Job) Steps(ctx context.Context) ([]*JobStep, error) {
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
	return scanRows(rows, err, fmt.Sprintf("steps for job %q", j.Name), func(scan func(...any) error) (*JobStep, error) {
		s := &JobStep{job: j}
		var lastRunDate, lastRunTime sql.NullInt64
		if err := scan(
			&s.StepID, &s.Name, &s.Subsystem, &s.Command, &s.Database,
			&s.OnSuccessAction, &s.OnSuccessStepID, &s.OnFailAction, &s.OnFailStepID,
			&s.LastRunOutcome, &lastRunDate, &lastRunTime, &s.LastRunDuration,
			&s.RetryAttempts, &s.RetryInterval, &s.OutputFileName, &s.Flags,
			&s.ProxyName, &s.AdditionalParameters, &s.CmdExecSuccessCode,
			&s.Server, &s.DatabaseUserName, &s.OSRunPriority,
		); err != nil {
			return nil, err
		}
		// last_run_date is 0 for a step that has never run, which
		// parseSQLAgentDate would turn into a year-zero date rather than a
		// zero Time. Leave LastRunDate zero so IsZero() is the "never ran"
		// test callers expect.
		if lastRunDate.Valid && lastRunDate.Int64 != 0 {
			s.LastRunDate = parseSQLAgentDate(int(lastRunDate.Int64), int(lastRunTime.Int64))
		}
		s.LastRunElapsed = parseSQLAgentDuration(s.LastRunDuration)
		return s, nil
	})
}

// AddStep adds a T-SQL or other subsystem step to the job.
func (j *Job) AddStep(ctx context.Context, req JobStepRequest) error {
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
func (j *Job) InsertStep(ctx context.Context, req JobStepRequest, stepID int) error {
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
	if err := j.server.exec(ctx, addStepStmt(j.Name, req, stepID)); err != nil {
		return fmt.Errorf("gosmo: add step %q to job %q: %w", req.Name, j.Name, err)
	}
	return nil
}

// addStepStmt renders the sp_add_jobstep call. stepID > 0 inserts at that
// position; 0 appends. Split out from addStepAt so ReorderSteps can
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

// Alter replaces the step's definition via sp_update_jobstep.
func (s *JobStep) Alter(ctx context.Context, req JobStepRequest) error {
	// Same guard as Job.AddStep: sp_update_jobstep rejects an empty
	// @step_name with a server-side error, and the local field writes at the
	// end of this method would otherwise blank out s.Name on the way past.
	if req.Name == "" {
		return fmt.Errorf("gosmo: alter step: name is required")
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
	if err := s.job.server.exec(ctx, q); err != nil {
		return fmt.Errorf("gosmo: alter step %q of job %q: %w", req.Name, s.job.Name, err)
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
//
// The step is addressed by its number, which is what sp_delete_jobstep takes:
// a *JobStep is a snapshot, and its StepID is only current until something
// renumbers the job.
func (s *JobStep) Delete(ctx context.Context) error {
	return s.job.deleteStepAt(ctx, s.StepID)
}

// deleteStepAt removes the step currently numbered stepID, without needing a
// *JobStep for it, for a caller holding a step number rather than the step.
// JobStep.Delete is this with the number taken off the step.
func (j *Job) deleteStepAt(ctx context.Context, stepID int) error {
	if err := j.server.exec(ctx, deleteStepStmt(j.Name, stepID)); err != nil {
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

// SetFlow changes only the step's control flow — what happens after it
// succeeds or fails — leaving its command, proxy, flags and everything else
// untouched.
//
// Every other parameter is omitted, which sp_update_jobstep reads as "leave
// alone". That is what makes this usable for repairing references after a
// reorder, where rewriting the whole definition would be both wasteful and a
// chance to lose a column the request does not model.
func (s *JobStep) SetFlow(ctx context.Context, onSuccessAction, onSuccessStepID, onFailAction, onFailStepID int) error {
	q := setFlowStmt(s.job.Name, s.StepID, onSuccessAction, onSuccessStepID, onFailAction, onFailStepID)
	if err := s.job.server.exec(ctx, q); err != nil {
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

// MoveStep moves one step to another position, which is what "move up"
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
func (j *Job) MoveStep(ctx context.Context, stepID, newStepID int) error {
	return j.ReorderSteps(ctx, moveOrder(stepID, newStepID))
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

// ReorderSteps rewrites the job's step order. order is given the
// current number of steps and returns the current step ids in the sequence
// they should end up in — every id exactly once.
//
// The reorder is realised as delete-and-insert per step that has to move,
// fewest first, and every "go to step N" reference is rewritten afterwards
// through the composed mapping. See MoveStep for why both halves are
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
// which a bare Server.JobRef handle does not carry.
func (j *Job) ReorderSteps(ctx context.Context, order func(n int) []int) error {
	steps, err := j.Steps(ctx)
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
	if err := j.server.exec(ctx, atomicBatch(stmts)); err != nil {
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

// JobStepRequest describes a step to add to, or replace the definition of
// (see JobStep.Alter), a job.
type JobStepRequest struct {
	Name      string
	Subsystem string // "TSQL" is the most common value
	Command   string
	// Database is only used for TSQL steps.
	//
	// Empty means "leave the step's own database alone" on an update, not
	// "clear it": sp_update_jobstep accepts N'' without error and changes
	// nothing, so JobStep.Alter omits @database_name entirely rather
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
	// which create a row and so decide every column of it. Update
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
