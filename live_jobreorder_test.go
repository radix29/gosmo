//go:build livedb

// Live verification of Job.MoveStep / Job.ReorderSteps against a real msdb.
//
// Two things here can only be established against a server, and both were
// (2026-08-23, SQL Server 2025 on win10cli): sp_add_jobstep with @step_id
// inserts at that position, renumbering the later steps and following their
// "go to step N" references; sp_delete_jobstep does not follow them, it
// silently resets a reference to a step at or after the deleted one to "quit
// with success". A move is a delete plus an insert, so the second is what the
// reference-repair pass exists for — and a test that only checked the step
// order would pass with that pass deleted.
//
//	go test -tags livedb . -run TestLiveJobReorder -v \
//	  -livedb 'sqlserver://sa:PASS@host?TrustServerCertificate=true'
//
// Creates and drops its own job; touches nothing else.
package gosmo

import (
	"context"
	"testing"
)

const reorderJobName = "gossms_live_reorder"

// liveReorderJob creates a four-step job whose steps differ in the columns a
// move must carry — flags, an output file, a database, retries — and whose
// first and third steps use "go to step N" references, the thing a move
// renumbers.
func liveReorderJob(t *testing.T, srv *Server, ctx context.Context) (*Job, func()) {
	t.Helper()
	drop := func() {
		srv.execContext(context.Background(),
			"IF EXISTS (SELECT 1 FROM msdb.dbo.sysjobs WHERE name = N'"+reorderJobName+"') "+
				"EXEC msdb.dbo.sp_delete_job @job_name = N'"+reorderJobName+"'")
	}
	drop()
	if _, err := srv.CreateJobContext(ctx, CreateJobRequest{Name: reorderJobName, Enabled: false}); err != nil {
		t.Fatalf("create job: %v", err)
	}
	j, err := srv.JobByNameContext(ctx, reorderJobName)
	if err != nil {
		drop()
		t.Fatalf("job by name: %v", err)
	}
	steps := []JobStepRequest{
		// Step 1 sends the run on to step 3, which is what has to still be
		// true — pointing at the same *step* — after the reorder.
		{Name: "one", Subsystem: "TSQL", Command: "SELECT 1", OnSuccessAction: goToStepAction, OnSuccessStepID: 3, OnFailAction: 2},
		{Name: "two", Subsystem: "TSQL", Command: "SELECT 2", Database: "tempdb", OnSuccessAction: 3, OnFailAction: 2,
			RetryAttempts: 3, RetryInterval: 5, Flags: 2, OutputFileName: `C:\Temp\two.txt`},
		{Name: "three", Subsystem: "TSQL", Command: "SELECT 3", OnSuccessAction: 3, OnFailAction: goToStepAction, OnFailStepID: 1},
		{Name: "four", Subsystem: "TSQL", Command: "SELECT 4", OnSuccessAction: 1, OnFailAction: 2},
	}
	for _, req := range steps {
		if err := j.AddStepContext(ctx, req); err != nil {
			drop()
			t.Fatalf("add step %q: %v", req.Name, err)
		}
	}
	return j, drop
}

func stepNames(steps []*JobStep) []string {
	out := make([]string, len(steps))
	for i, s := range steps {
		out[i] = s.Name
	}
	return out
}

// byName is the step with that name, or a failure — the tests address steps
// by name because the numbers are the thing under test.
func byName(t *testing.T, steps []*JobStep, name string) *JobStep {
	t.Helper()
	for _, s := range steps {
		if s.Name == name {
			return s
		}
	}
	t.Fatalf("step %q is gone; steps are %v", name, stepNames(steps))
	return nil
}

func TestLiveJobReorderMovesTheStepAndKeepsItsDefinition(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()
	srv := &Server{db: db}
	j, drop := liveReorderJob(t, srv, ctx)
	defer drop()

	// Step 2 is the one carrying the columns a re-add would default away, so
	// it has to be the step that moves: a move only rewrites the step it
	// moves, and asserting on a step that merely shifted passes with the
	// definition dropped (found by deleting Flags from stepRequestFrom,
	// 2026-08-23).
	// one, two, three, four -> two, one, three, four.
	if err := j.MoveStepContext(ctx, 2, 1); err != nil {
		t.Fatalf("move step 2 to 1: %v", err)
	}
	steps, err := j.StepsContext(ctx)
	if err != nil {
		t.Fatalf("steps: %v", err)
	}
	want := []string{"two", "one", "three", "four"}
	if got := stepNames(steps); !equalStrings(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
	for i, s := range steps {
		if s.StepID != i+1 {
			t.Errorf("step %q has id %d at position %d; msdb numbers steps without gaps", s.Name, s.StepID, i+1)
		}
	}

	// The moved step keeps everything about itself.
	two := byName(t, steps, "two")
	if two.Flags != 2 {
		t.Errorf("flags = %d, want the 2 it was created with — a move must not clear them", two.Flags)
	}
	if two.OutputFileName == "" {
		t.Errorf("output file was lost by the move")
	}
	if two.Database != "tempdb" || two.RetryAttempts != 3 || two.RetryInterval != 5 {
		t.Errorf("database/retries = %q/%d/%d, want tempdb/3/5", two.Database, two.RetryAttempts, two.RetryInterval)
	}
}

// The half a move can silently get wrong: "one" pointed at the step named
// "three", not at the number 3, and "three" fails back to the one named
// "one". Both have to still name those steps afterwards.
//
// The move is deliberately step 2 to the end, not step 4 to the front. The
// step deleted has to sit *before* a reference for the repair pass to matter:
// sp_delete_jobstep leaves a reference to an earlier step alone, and
// sp_add_jobstep's insert remaps what it shifts — so a move of the last step
// passes with the repair pass deleted, and this one does not. Established by
// deleting it (2026-08-23).
func TestLiveJobReorderFollowsGoToStepReferences(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()
	srv := &Server{db: db}
	j, drop := liveReorderJob(t, srv, ctx)
	defer drop()

	// one, two, three, four -> one, three, four, two
	if err := j.MoveStepContext(ctx, 2, 4); err != nil {
		t.Fatalf("move step 2 to 4: %v", err)
	}
	steps, err := j.StepsContext(ctx)
	if err != nil {
		t.Fatalf("steps: %v", err)
	}
	if got, want := stepNames(steps), []string{"one", "three", "four", "two"}; !equalStrings(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
	one, three := byName(t, steps, "one"), byName(t, steps, "three")

	if one.OnSuccessAction != goToStepAction {
		t.Fatalf("step %q on success = action %d, want it still going to a step (4)", one.Name, one.OnSuccessAction)
	}
	if one.OnSuccessStepID != three.StepID {
		t.Errorf("step %q goes to step %d, want %d — the step named %q", one.Name, one.OnSuccessStepID, three.StepID, three.Name)
	}
	// The reference the other way: "three" fails to the step named "one".
	if three.OnFailAction != goToStepAction || three.OnFailStepID != one.StepID {
		t.Errorf("step %q on failure = action %d step %d, want action 4 step %d (%q)",
			three.Name, three.OnFailAction, three.OnFailStepID, one.StepID, one.Name)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
