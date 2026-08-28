package gosmo

import (
	"database/sql"
	"database/sql/driver"
	"strings"
	"testing"
)

// captureJob returns a Job wired to the capture driver, with steps canned to
// the definitions given. Step ids are 1..n in the order listed, which is what
// sysjobsteps guarantees and what ReorderStepsContext's ORDER BY relies on.
func captureJob(t *testing.T, steps ...*JobStep) *Job {
	t.Helper()
	db, err := sql.Open("capture", "")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	rows := make([][]driver.Value, len(steps))
	for i, s := range steps {
		rows[i] = []driver.Value{
			int64(i + 1), s.Name, "TSQL", s.Command, "",
			int64(s.OnSuccessAction), int64(s.OnSuccessStepID),
			int64(s.OnFailAction), int64(s.OnFailStepID),
			int64(0), int64(0), int64(0), int64(0),
			int64(0), int64(0), "", int64(0),
			"", "", int64(0), "", "", int64(0),
		}
	}
	// 23 named columns, because captureRows defaults to a single one and Scan
	// then fails before any of this is reached — which the reorder reports as
	// an error, and a test asserting "nothing was written" passes on.
	cols := make([]string, 23)
	for i := range cols {
		cols[i] = "c" + string(rune('a'+i))
	}
	captured.reset(cannedRow{match: "FROM   msdb.dbo.sysjobsteps s", cols: cols, rows: rows})
	return &Job{server: &Server{db: db}, JobID: "job-id", Name: "nightly"}
}

// plainSteps are n steps that name each other in no way at all — every
// on_success/on_fail action is something other than "go to step N", so a
// reorder of them emits moves and no reference repair.
func plainSteps(n int) []*JobStep {
	steps := make([]*JobStep, n)
	for i := range steps {
		steps[i] = &JobStep{
			Name: string(rune('a' + i)), Command: "SELECT 1",
			OnSuccessAction: 3, OnFailAction: 2,
		}
	}
	return steps
}

// reverseOrder is the reorder that moves every step, so no case below passes
// by accident on a job that barely changed.
func reverseOrder(n int) []int {
	order := make([]int, n)
	for i := range order {
		order[i] = n - i
	}
	return order
}

// TestReorderStepsIsOneAtomicBatch is the whole point of the transactional
// form. msdb cannot renumber a step in place, so every move is
// sp_delete_jobstep followed by sp_add_jobstep, and between the two the step's
// definition exists nowhere but in gosmo's memory: a failed insert, a dropped
// connection or a context that ran out of budget used to lose the step for
// good, with the job left one step shorter than the user asked to reorder.
//
// Asserted as "one statement reached the server, and it carries both halves",
// because the mutant worth killing is the pre-fix code — which produced
// exactly the same procedure calls with exactly the same arguments, just
// issued one at a time.
func TestReorderStepsIsOneAtomicBatch(t *testing.T) {
	j := captureJob(t, plainSteps(3)...)
	if err := j.ReorderStepsContext(t.Context(), reverseOrder); err != nil {
		t.Fatalf("ReorderStepsContext: %v", err)
	}

	if n := captured.count("sp_delete_jobstep"); n != 1 {
		t.Fatalf("%d statements mention sp_delete_jobstep, want 1 — the moves were issued separately", n)
	}
	batch := captured.find("sp_delete_jobstep")
	for _, want := range []string{
		"SET XACT_ABORT ON",
		"BEGIN TRY",
		"BEGIN TRANSACTION",
		"sp_add_jobstep",
		"COMMIT TRANSACTION",
		"BEGIN CATCH",
		"ROLLBACK TRANSACTION",
		"THROW",
	} {
		if !strings.Contains(batch, want) {
			t.Errorf("the reorder batch does not contain %q:\n%s", want, batch)
		}
	}
	// Both halves of every move are in the one batch. Two steps have to move
	// to reverse three, so that is two deletes and two inserts.
	if got := strings.Count(batch, "sp_delete_jobstep"); got != 2 {
		t.Errorf("batch has %d deletes, want 2:\n%s", got, batch)
	}
	if got := strings.Count(batch, "sp_add_jobstep"); got != 2 {
		t.Errorf("batch has %d inserts, want 2:\n%s", got, batch)
	}
}

// TestReorderStepsRepairsReferencesInsideTheSameTransaction.
//
// sp_delete_jobstep silently resets any "go to step N" reference that names
// the step being deleted, so the repair pass is not a tidy-up after the
// reorder — it is part of it. Committing the moves and then repairing would
// leave a window, and a failure in it, with the job's control flow rewritten
// to "quit with success" and nothing to say so.
//
// Pinned by position: the repair has to be *before* the COMMIT.
func TestReorderStepsRepairsReferencesInsideTheSameTransaction(t *testing.T) {
	steps := plainSteps(3)
	// Step 1 hands off to step 3 — the reference the reverse below has to
	// carry with the step it names rather than lose.
	steps[0].OnSuccessAction, steps[0].OnSuccessStepID = goToStepAction, 3

	j := captureJob(t, steps...)
	if err := j.ReorderStepsContext(t.Context(), reverseOrder); err != nil {
		t.Fatalf("ReorderStepsContext: %v", err)
	}

	batch := captured.find("sp_delete_jobstep")
	repair := strings.Index(batch, "sp_update_jobstep")
	if repair < 0 {
		t.Fatalf("no reference repair in the batch:\n%s", batch)
	}
	commit := strings.Index(batch, "COMMIT TRANSACTION")
	if commit < 0 {
		t.Fatalf("no COMMIT in the batch:\n%s", batch)
	}
	if repair > commit {
		t.Errorf("the reference repair is after the COMMIT, so a failure in it "+
			"leaves the references reset:\n%s", batch)
	}
	if n := captured.count("sp_update_jobstep"); n != 1 {
		t.Errorf("%d statements mention sp_update_jobstep, want 1 (inside the batch)", n)
	}
}

// TestReorderStepsWritesNothingWhenNothingMoves. An empty batch is still a
// BEGIN TRANSACTION and a COMMIT — two round trips and a write lock on msdb's
// tables for a reorder that asked for the order the job is already in, which
// is what a step list re-saved without a drag produces.
func TestReorderStepsWritesNothingWhenNothingMoves(t *testing.T) {
	j := captureJob(t, plainSteps(3)...)
	identity := func(n int) []int {
		order := make([]int, n)
		for i := range order {
			order[i] = i + 1
		}
		return order
	}
	if err := j.ReorderStepsContext(t.Context(), identity); err != nil {
		t.Fatalf("ReorderStepsContext: %v", err)
	}
	if n := captured.count("BEGIN TRANSACTION"); n != 0 {
		t.Errorf("a no-op reorder still opened %d transactions", n)
	}
}

// TestReorderStepsRejectsAnOrderThatIsNotAPermutation, pinned here rather than
// only in checkReorder's own terms: a duplicate or a missing id deletes a step
// and never puts it back, and the check has to happen before anything is
// built, not just before it is sent.
func TestReorderStepsRejectsAnOrderThatIsNotAPermutation(t *testing.T) {
	cases := map[string]func(n int) []int{
		"a step named twice": func(int) []int { return []int{1, 1, 3} },
		"a step left out":    func(int) []int { return []int{1, 2} },
		"an id out of range": func(int) []int { return []int{1, 2, 9} },
	}
	for name, order := range cases {
		t.Run(name, func(t *testing.T) {
			j := captureJob(t, plainSteps(3)...)
			if err := j.ReorderStepsContext(t.Context(), order); err == nil {
				t.Fatal("ReorderStepsContext accepted an order that is not a permutation")
			}
			if n := captured.count("sp_delete_jobstep"); n != 0 {
				t.Errorf("%d delete statements were sent for a rejected order", n)
			}
		})
	}
}

// TestAtomicBatchShape pins the batch's own text, which is the part no unit
// test of the reorder can check for meaning — only a live run can. Each
// assertion is a way of getting it wrong that still looks right.
func TestAtomicBatchShape(t *testing.T) {
	got := atomicBatch([]string{"EXEC dbo.one", "EXEC dbo.two"})

	// Every statement terminated. THROW in particular is documented as
	// requiring the preceding statement to end in a semicolon, and a batch
	// that runs everything but fails to roll back is the worst outcome here.
	if strings.Count(got, ";") != 8 {
		t.Errorf("batch has %d semicolons, want one per statement:\n%s", strings.Count(got, ";"), got)
	}

	order := []string{
		"SET XACT_ABORT ON",
		"BEGIN TRY",
		"BEGIN TRANSACTION",
		"EXEC dbo.one",
		"EXEC dbo.two",
		"COMMIT TRANSACTION",
		"END TRY",
		"BEGIN CATCH",
		"ROLLBACK TRANSACTION",
		"THROW",
		"END CATCH",
	}
	at := -1
	for _, want := range order {
		i := strings.Index(got, want)
		if i < 0 {
			t.Fatalf("batch is missing %q:\n%s", want, got)
		}
		if i <= at {
			t.Errorf("%q comes out of order:\n%s", want, got)
		}
		at = i
	}

	// The rollback is guarded: with XACT_ABORT on, an error can have doomed
	// the transaction already, and an unguarded ROLLBACK in the CATCH then
	// raises an error of its own that replaces the one THROW was to re-raise.
	if !strings.Contains(got, "IF @@TRANCOUNT > 0 ROLLBACK TRANSACTION") {
		t.Errorf("the rollback is not guarded by @@TRANCOUNT:\n%s", got)
	}

	// No DECLARE: a ScriptCollector concatenates its statements into one
	// batch, and a batch-scoped variable declared twice is an error — the
	// same collision bindScriptArgs describes for @p1.
	if strings.Contains(got, "DECLARE") {
		t.Errorf("the batch declares a variable, so two of them cannot be scripted together:\n%s", got)
	}
}
