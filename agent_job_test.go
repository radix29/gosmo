package gosmo

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"io"
	"strings"
	"testing"
	"time"
)

func TestParseSQLAgentDate(t *testing.T) {
	got := parseSQLAgentDate(20240315, 143059)
	want := time.Date(2024, time.March, 15, 14, 30, 59, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("parseSQLAgentDate(20240315, 143059) = %v, want %v", got, want)
	}
}

func TestParseSQLAgentDateMidnight(t *testing.T) {
	got := parseSQLAgentDate(20200101, 0)
	want := time.Date(2020, time.January, 1, 0, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("parseSQLAgentDate(20200101, 0) = %v, want %v", got, want)
	}
}

// A decoded msdb integer pair and the same instant read from a real datetime
// column must land on the same time.Time. go-mssqldb hands a datetime back in
// UTC, so parseSQLAgentDate has to as well — otherwise Job.LastRunDate (from
// ja.last_executed_step_date, a datetime) and JobStep.LastRunDate (from the
// integer columns) differ by the client's UTC offset while showing the same
// digits.
func TestParseSQLAgentDateMatchesDatetimeColumn(t *testing.T) {
	// What the driver produces for datetime '2024-03-15 14:30:59'.
	fromDatetime := time.Date(2024, time.March, 15, 14, 30, 59, 0, time.UTC)
	fromIntegers := parseSQLAgentDate(20240315, 143059)
	if !fromIntegers.Equal(fromDatetime) {
		t.Errorf("parseSQLAgentDate(20240315, 143059) = %v, datetime column = %v; the two must be the same instant",
			fromIntegers, fromDatetime)
	}
	if loc := fromIntegers.Location(); loc != time.UTC {
		t.Errorf("parseSQLAgentDate location = %v, want UTC", loc)
	}
}

func TestParseSQLAgentDuration(t *testing.T) {
	cases := []struct {
		dur  int
		want time.Duration
	}{
		{0, 0},
		{10203, 1*time.Hour + 2*time.Minute + 3*time.Second},
		{130245, 13*time.Hour + 2*time.Minute + 45*time.Second},
		{959, 9*time.Minute + 59*time.Second},
		{5, 5 * time.Second},
	}
	for _, c := range cases {
		if got := parseSQLAgentDuration(c.dur); got != c.want {
			t.Errorf("parseSQLAgentDuration(%d) = %v, want %v", c.dur, got, c.want)
		}
	}
}

// TestJobStepNameRequired pins the empty-name guard on both job-step writers.
// Beyond rejecting a request sp_add_jobstep/sp_update_jobstep would refuse
// anyway, the guard has to run before anything else: JobStep.UpdateContext
// copies req over the receiver's own fields once the statement succeeds, so an
// empty name that got that far would blank out JobStep.Name locally. Both
// receivers here are deliberately zero-valued — no job, no server — so a guard
// that moved below the statement-building code would panic instead of
// returning, and this test would catch that too.
func TestJobStepNameRequired(t *testing.T) {
	cases := []struct {
		name string
		call func() error
	}{
		{"Job.AddStepContext", func() error {
			return (&Job{}).AddStepContext(t.Context(), JobStepRequest{Command: "SELECT 1"})
		}},
		{"JobStep.UpdateContext", func() error {
			return (&JobStep{}).UpdateContext(t.Context(), JobStepRequest{Command: "SELECT 1"})
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.call()
			if err == nil {
				t.Fatalf("%s with an empty Name = nil, want an error", c.name)
			}
			if !strings.Contains(err.Error(), "name is required") {
				t.Errorf("%s error = %q, want it to say the name is required", c.name, err)
			}
		})
	}
}

// TestJobStepUpdateLeavesFieldsAloneOnRejection pins the consequence the guard
// exists for: a rejected update must not have already overwritten the step's
// in-memory fields.
func TestJobStepUpdateLeavesFieldsAloneOnRejection(t *testing.T) {
	s := &JobStep{Name: "Load staging", Subsystem: "TSQL", Command: "EXEC dbo.Load"}
	if err := s.UpdateContext(t.Context(), JobStepRequest{Command: "SELECT 1"}); err == nil {
		t.Fatal("UpdateContext with an empty Name = nil, want an error")
	}
	if s.Name != "Load staging" || s.Subsystem != "TSQL" || s.Command != "EXEC dbo.Load" {
		t.Errorf("after a rejected update, step = %+v, want its original field values", s)
	}
}

// captureStepJob is a job wired to the capture driver, holding one step at
// stepID. No canned rows: deleting a step reads nothing.
func captureStepJob(t *testing.T, jobName string, stepID int) *JobStep {
	t.Helper()
	db, err := sql.Open("capture", "")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	captured.reset()
	j := &Job{server: &Server{db: db}, JobID: "job-id", Name: jobName}
	return &JobStep{job: j, StepID: stepID, Name: "Load staging"}
}

// TestDeleteStepAddressesTheStepItWasCalledOn. sp_delete_jobstep takes a
// number, and msdb renumbers the steps after it — so a delete that sent the
// wrong number removes a step the caller never named, and succeeds while doing
// it. There is no error to notice and no second statement to compare against.
func TestDeleteStepAddressesTheStepItWasCalledOn(t *testing.T) {
	// The third step, not the first: a delete that ignored StepID and sent 1
	// would pass against a job whose step is step 1.
	s := captureStepJob(t, "nightly", 3)

	if err := s.DeleteContext(t.Context()); err != nil {
		t.Fatalf("DeleteContext: %v", err)
	}

	got := captured.find("sp_delete_jobstep")
	want := "EXEC msdb.dbo.sp_delete_jobstep @job_name = N'nightly', @step_id = 3"
	if got != want {
		t.Errorf("statement =\n%s\nwant\n%s", got, want)
	}
}

// A job name carrying an apostrophe is escaped, not concatenated. Job names are
// user text and reach this statement as a literal.
func TestDeleteStepEscapesTheJobName(t *testing.T) {
	s := captureStepJob(t, "Bob's nightly", 1)

	if err := s.DeleteContext(t.Context()); err != nil {
		t.Fatalf("DeleteContext: %v", err)
	}

	if got, want := captured.find("sp_delete_jobstep"),
		"EXEC msdb.dbo.sp_delete_jobstep @job_name = N'Bob''s nightly', @step_id = 1"; got != want {
		t.Errorf("statement =\n%s\nwant\n%s", got, want)
	}
}

// JobStep.DeleteContext and Job.deleteStepAt are one call now, the step's
// number being the only difference between them, and ReorderStepsContext
// collects the same text into its batch through deleteStepStmt. The three
// agreeing is what makes a fix to the statement reach every path that deletes a
// step; they were two renderings of the same procedure call before.
func TestEveryStepDeleteRendersTheSameCall(t *testing.T) {
	s := captureStepJob(t, "nightly", 2)

	if err := s.DeleteContext(t.Context()); err != nil {
		t.Fatalf("DeleteContext: %v", err)
	}
	viaStep := captured.find("sp_delete_jobstep")

	captured.reset()
	if err := s.job.deleteStepAt(t.Context(), 2); err != nil {
		t.Fatalf("deleteStepAt: %v", err)
	}
	viaNumber := captured.find("sp_delete_jobstep")

	if viaStep != viaNumber {
		t.Errorf("JobStep.DeleteContext sends\n%s\nand Job.deleteStepAt sends\n%s", viaStep, viaNumber)
	}
	if got := deleteStepStmt("nightly", 2); got != viaStep {
		t.Errorf("the reorder batch collects\n%s\nand a delete sends\n%s", got, viaStep)
	}
}

// Jobs and JobByName build their statements from one jobSelect and decode
// through one scanJob. Before that they were two 895-character copies of the
// same SELECT with two copies of the 17 scan targets below them, and had
// already drifted by a comment; the failure that shape invites is a column
// added or an ISNULL corrected in one copy only, so the same job comes back
// populated one way from the listing and another from the by-name read. This
// answers both paths from one row and requires the two *Job values to match.
type jobRowDriver struct{}

func (jobRowDriver) Open(string) (driver.Conn, error) { return &jobRowConn{}, nil }

// jobQueries collects the job SELECTs the two reads send, so the test can
// compare the statements as well as the decode.
var jobQueries []string

type jobRowConn struct{}

func (*jobRowConn) Prepare(string) (driver.Stmt, error) { return nil, driver.ErrSkip }
func (*jobRowConn) Close() error                        { return nil }
func (*jobRowConn) Begin() (driver.Tx, error)           { return nil, driver.ErrSkip }

func (*jobRowConn) QueryContext(_ context.Context, q string, _ []driver.NamedValue) (driver.Rows, error) {
	// applyJobStates reads Agent's live state separately; an empty answer is
	// the Agent-stopped case, which leaves the sysjobactivity-derived
	// CurrentState in place — exactly what this test wants to compare.
	if strings.Contains(q, "xp_sqlagent_enum_jobs") {
		return &jobStateRows{}, nil
	}
	jobQueries = append(jobQueries, q)
	return &jobRows{}, nil
}

type jobStateRows struct{}

func (*jobStateRows) Columns() []string              { return []string{"job_id", "job_state"} }
func (*jobStateRows) Close() error                   { return nil }
func (*jobStateRows) Next(dest []driver.Value) error { return io.EOF }

type jobRows struct{ done bool }

func (r *jobRows) Columns() []string {
	return []string{"job_id", "name", "description", "enabled", "category", "owner",
		"date_created", "date_modified", "start_step_id", "delete_level",
		"notify_level_email", "notify_operator", "last_run", "last_outcome",
		"last_duration", "next_run", "job_state"}
}
func (*jobRows) Close() error { return nil }
func (r *jobRows) Next(dest []driver.Value) error {
	if r.done {
		return io.EOF
	}
	r.done = true
	created := time.Date(2026, time.March, 1, 9, 0, 0, 0, time.UTC)
	modified := time.Date(2026, time.April, 2, 10, 30, 0, 0, time.UTC)
	lastRun := time.Date(2026, time.April, 3, 1, 0, 0, 0, time.UTC)
	nextRun := time.Date(2026, time.April, 4, 1, 0, 0, 0, time.UTC)
	for i, v := range []driver.Value{
		"7F1E0C2A-0000-0000-0000-000000000001", "Nightly reindex", "rebuilds every index",
		true, "Database Maintenance", "sa",
		created, modified, int64(3),
		int64(NotifyOnFailure), int64(NotifyOnComplete), "dba_pager",
		lastRun, int64(JobOutcomeSucceeded), int64(10230), nextRun, int64(4),
	} {
		dest[i] = v
	}
	return nil
}

func init() { sql.Register("fakejobrow", jobRowDriver{}) }

func TestJobsAndJobByNameDecodeTheSameRowIdentically(t *testing.T) {
	db, err := sql.Open("fakejobrow", "")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()
	s := &Server{db: db}

	jobQueries = nil
	jobs, err := s.JobsContext(context.Background())
	if err != nil {
		t.Fatalf("JobsContext: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("JobsContext returned %d jobs, want 1", len(jobs))
	}
	byName, err := s.JobByNameContext(context.Background(), "Nightly reindex")
	if err != nil {
		t.Fatalf("JobByNameContext: %v", err)
	}

	if *jobs[0] != *byName {
		t.Errorf("Jobs gave\n%+v\nJobByName gave\n%+v", *jobs[0], *byName)
	}

	// The decode check above cannot see the select list — this driver answers
	// positionally, whatever was asked for. So compare the statements too:
	// everything up to the tail each read appends must be byte-identical, which
	// is what fails if either literal is written out again by hand.
	if len(jobQueries) != 2 {
		t.Fatalf("captured %d job queries, want 2", len(jobQueries))
	}
	listHead, _, _ := strings.Cut(jobQueries[0], "\nORDER  BY j.name")
	nameHead, _, _ := strings.Cut(jobQueries[1], "\nWHERE  j.name = @p1")
	if listHead != nameHead {
		t.Errorf("Jobs and JobByName no longer share a select head:\n%s\n---\n%s", listHead, nameHead)
	}
	if listHead != jobSelect {
		t.Errorf("the shared head is not jobSelect:\n%s", listHead)
	}

	// Every field the shared select list populates, so a column dropped from
	// jobSelect fails here rather than passing as "both agree on zero".
	want := Job{
		server:                  s,
		JobID:                   "7F1E0C2A-0000-0000-0000-000000000001",
		Name:                    "Nightly reindex",
		Description:             "rebuilds every index",
		IsEnabled:               true,
		Category:                "Database Maintenance",
		OwnerLoginName:          "sa",
		DateCreated:             time.Date(2026, time.March, 1, 9, 0, 0, 0, time.UTC),
		DateModified:            time.Date(2026, time.April, 2, 10, 30, 0, 0, time.UTC),
		StartStepID:             3,
		DeleteLevel:             NotifyOnFailure,
		NotifyLevelEmail:        NotifyOnComplete,
		NotifyEmailOperatorName: "dba_pager",
		LastRunDate:             time.Date(2026, time.April, 3, 1, 0, 0, 0, time.UTC),
		LastRunOutcome:          JobOutcomeSucceeded,
		LastRunDuration:         time.Hour + 2*time.Minute + 30*time.Second,
		NextRunDate:             time.Date(2026, time.April, 4, 1, 0, 0, 0, time.UTC),
		CurrentState:            JobStateIdle,
	}
	if *byName != want {
		t.Errorf("JobByName =\n%+v\nwant\n%+v", *byName, want)
	}
}
