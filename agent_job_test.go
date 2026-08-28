package gosmo

import (
	"database/sql"
	"strings"
	"testing"
	"time"
)

func TestParseSQLAgentDate(t *testing.T) {
	got := parseSQLAgentDate(20240315, 143059)
	want := time.Date(2024, time.March, 15, 14, 30, 59, 0, time.Local)
	if !got.Equal(want) {
		t.Errorf("parseSQLAgentDate(20240315, 143059) = %v, want %v", got, want)
	}
}

func TestParseSQLAgentDateMidnight(t *testing.T) {
	got := parseSQLAgentDate(20200101, 0)
	want := time.Date(2020, time.January, 1, 0, 0, 0, 0, time.Local)
	if !got.Equal(want) {
		t.Errorf("parseSQLAgentDate(20200101, 0) = %v, want %v", got, want)
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
