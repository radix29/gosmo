//go:build livedb

// Live verification of atomicBatch — the three things about it that no unit
// test can establish, because they are facts about SQL Server rather than
// about the string gosmo builds:
//
//   - the batch is valid T-SQL at all (a unit test asserts its shape, never
//     that the server accepts it);
//   - a failure inside it rolls back the statements that already succeeded,
//     rather than the CATCH swallowing it and the COMMIT going ahead;
//   - the error still reaches the Go caller, and it is msdb's own message
//     about what went wrong, not a generic one from a RAISERROR of ours.
//
// The third is what makes the first two safe to rely on: a batch that rolls
// back and reports success is worse than one that never rolled back, because
// the caller then treats the write as applied.
//
//	go test -tags livedb . -run TestLiveAtomicBatch -v \
//	  -livedb 'sqlserver://sa:PASS@host?TrustServerCertificate=true'
//
// Creates and drops one table in tempdb; touches nothing else.
package gosmo

import (
	"strings"
	"testing"
)

const atomicBatchTable = "tempdb.dbo.gossms_live_atomic"

func TestLiveAtomicBatchRollsBackTheStatementsBeforeTheFailure(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()
	srv := &Server{db: db}

	drop := func() {
		srv.execContext(ctx, "IF OBJECT_ID('"+atomicBatchTable+"') IS NOT NULL DROP TABLE "+atomicBatchTable)
	}
	drop()
	defer drop()
	if err := srv.execContext(ctx, "CREATE TABLE "+atomicBatchTable+" (n INT NOT NULL)"); err != nil {
		t.Fatalf("create table: %v", err)
	}

	count := func() int {
		var n int
		if err := srv.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+atomicBatchTable).Scan(&n); err != nil {
			t.Fatalf("count: %v", err)
		}
		return n
	}

	// The happy path first, so a batch that silently does nothing at all
	// cannot pass the rollback assertion below.
	if err := srv.execContext(ctx, atomicBatch([]string{
		"INSERT INTO " + atomicBatchTable + " VALUES (1)",
		"INSERT INTO " + atomicBatchTable + " VALUES (2)",
	})); err != nil {
		t.Fatalf("a batch that should succeed failed: %v", err)
	}
	if n := count(); n != 2 {
		t.Fatalf("after a successful batch the table has %d rows, want 2", n)
	}

	// Now one that fails on its second statement. The first has already
	// succeeded when it does, which is the delete-then-failed-insert shape a
	// job step reorder gets into.
	err := srv.execContext(ctx, atomicBatch([]string{
		"INSERT INTO " + atomicBatchTable + " VALUES (3)",
		"INSERT INTO " + atomicBatchTable + " VALUES ('not an int')",
	}))
	if err == nil {
		t.Fatal("a batch whose second statement fails returned nil — the caller would " +
			"treat a rolled-back write as applied")
	}
	if n := count(); n != 2 {
		t.Errorf("after a failed batch the table has %d rows, want the 2 it started with — "+
			"the statements before the failure were not rolled back", n)
	}
	// msdb's message, not one of ours: THROW re-raises the original.
	if !strings.Contains(strings.ToLower(err.Error()), "convert") {
		t.Errorf("error = %q, want SQL Server's own conversion error", err)
	}
}

// TestLiveAtomicBatchRunsMsdbJobProcedures. The reorder batch is not arbitrary
// T-SQL — it is msdb's sp_add_jobstep and sp_delete_jobstep inside an explicit
// transaction, and those procedures manage transactions of their own. If they
// refused to nest, every reorder would fail rather than merely fail unsafely,
// which is the one regression this change could introduce.
//
// Covered by TestLiveJobReorderMovesTheStepAndKeepsItsDefinition running at
// all, so this only names the dependency; run them together.
func TestLiveAtomicBatchRunsMsdbJobProcedures(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()
	srv := &Server{db: db}
	j, drop := liveReorderJob(t, srv, ctx)
	defer drop()

	if err := j.MoveStepContext(ctx, 4, 1); err != nil {
		t.Fatalf("msdb's job procedures rejected the transactional batch: %v", err)
	}
	steps, err := j.StepsContext(ctx)
	if err != nil {
		t.Fatalf("steps: %v", err)
	}
	want := []string{"four", "one", "two", "three"}
	if got := stepNames(steps); !equalStrings(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
}
