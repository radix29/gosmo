//go:build livedb

// Live coverage for CycleLogContext, which no unit test can reach: WithScript
// proves the right statement is *built*, never that SQL Server accepts it or
// that it does what the name says. What settles it is the archive count
// before and after.
//
//	go test -tags livedb . -run TestLiveCycleLog -v \
//	  -livedb 'sqlserver://sa:PASS@host?TrustServerCertificate=true'
//
// This one mutates the instance rather than a throwaway object of its own —
// cycling is a server-wide action with nothing smaller to do it to. It is
// benign (the archives are rotating storage and the server renumbers them on
// every restart anyway), but it does discard the oldest archive once the
// instance holds its configured maximum, so don't point it at a server whose
// error log history matters.
package gosmo

import (
	"testing"
)

// TestLiveCycleLogSQLServer cycles the SQL Server error log and asserts the
// archive it created is really there — the current log is renumbered to 1, so
// whatever was last written before the cycle now sits one number higher.
func TestLiveCycleLogSQLServer(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()

	s, err := NewServer(ctx, db)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	before, err := s.EnumErrorLogsContext(ctx, ErrorLogSQLServer)
	if err != nil {
		t.Fatalf("EnumErrorLogsContext before: %v", err)
	}
	if len(before) == 0 {
		t.Fatal("no error logs reported before the cycle")
	}

	if err := s.CycleLogContext(ctx, ErrorLogSQLServer); err != nil {
		t.Fatalf("CycleLogContext(SQL Server): %v", err)
	}

	after, err := s.EnumErrorLogsContext(ctx, ErrorLogSQLServer)
	if err != nil {
		t.Fatalf("EnumErrorLogsContext after: %v", err)
	}
	// The count only grows until the instance reaches its configured maximum,
	// after which cycling drops the oldest and the count holds steady. What is
	// true either way is that a new current log exists and the previous one
	// became archive 1 — so archive 1's last-written time must now be at least
	// the old current log's.
	if len(after) < len(before) {
		t.Errorf("archive count fell from %d to %d", len(before), len(after))
	}
	if len(after) < 2 {
		t.Fatalf("after cycling there are %d logs, want at least the current one plus an archive", len(after))
	}
	if after[0].Number != 0 || after[1].Number != 1 {
		t.Fatalf("logs after the cycle are numbered %d, %d — want 0 (current) then 1", after[0].Number, after[1].Number)
	}
	oldCurrent := before[0].LastWritten
	if !oldCurrent.IsZero() && !after[1].LastWritten.IsZero() &&
		after[1].LastWritten.Before(oldCurrent) {
		t.Errorf("archive 1 was last written %v, before the pre-cycle current log's %v — the cycle did not archive it",
			after[1].LastWritten, oldCurrent)
	}
}

// TestLiveCycleLogAgent is the Agent half. It needs SQL Server Agent running:
// sp_cycle_agent_errorlog fails outright when it is not, which is itself worth
// seeing rather than skipping past.
func TestLiveCycleLogAgent(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()

	s, err := NewServer(ctx, db)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	before, err := s.EnumErrorLogsContext(ctx, ErrorLogAgent)
	if err != nil {
		t.Skipf("cannot enumerate Agent logs (Agent not running?): %v", err)
	}
	if len(before) == 0 {
		t.Skip("no Agent error logs reported — Agent has never run on this instance")
	}

	if err := s.CycleLogContext(ctx, ErrorLogAgent); err != nil {
		t.Fatalf("CycleLogContext(SQL Server Agent): %v", err)
	}

	after, err := s.EnumErrorLogsContext(ctx, ErrorLogAgent)
	if err != nil {
		t.Fatalf("EnumErrorLogsContext after: %v", err)
	}
	if len(after) < len(before) {
		t.Errorf("Agent archive count fell from %d to %d", len(before), len(after))
	}
	if len(after) < 2 {
		t.Fatalf("after cycling there are %d Agent logs, want at least the current one plus an archive", len(after))
	}
	if after[0].Number != 0 || after[1].Number != 1 {
		t.Fatalf("Agent logs after the cycle are numbered %d, %d — want 0 (current) then 1", after[0].Number, after[1].Number)
	}
}
