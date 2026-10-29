//go:build livedb

// Live check that changing only Query Store's state through Options +
// SetOptions leaves every other option as it was read — the round trip a
// "set the state, keep the rest" caller (gossms `db set QUERY_STORE=…`)
// relies on instead of a state-only setter. Runs in a throwaway database.
//
//	go test -tags livedb . -run TestLiveQueryStoreStateRoundTrip -v -livedb '…'
package gosmo

import "testing"

func TestLiveQueryStoreStateRoundTrip(t *testing.T) {
	db, ctx, done := liveDB(t)
	t.Cleanup(done)
	d, drop := liveScratchDB(t, db, ctx, "gosmo_qs_roundtrip")
	t.Cleanup(drop)

	// Away from every default, so a value the round trip resets shows.
	withs := "OPERATION_MODE = READ_WRITE, MAX_STORAGE_SIZE_MB = 123, DATA_FLUSH_INTERVAL_SECONDS = 300, " +
		"INTERVAL_LENGTH_MINUTES = 15, MAX_PLANS_PER_QUERY = 77, SIZE_BASED_CLEANUP_MODE = OFF, " +
		"QUERY_CAPTURE_MODE = ALL, CLEANUP_POLICY = (STALE_QUERY_THRESHOLD_DAYS = 11)"
	if d.atLeast(SQLServer2017) {
		withs += ", WAIT_STATS_CAPTURE_MODE = OFF"
	}
	if _, err := db.ExecContext(ctx, "ALTER DATABASE [gosmo_qs_roundtrip] SET QUERY_STORE = ON ("+withs+")"); err != nil {
		t.Fatalf("set up Query Store: %v", err)
	}

	qs := d.QueryStore()
	for _, to := range []QueryStoreState{QueryStoreReadOnly, QueryStoreReadWrite, QueryStoreOff, QueryStoreReadWrite} {
		before, err := qs.Options(ctx)
		if err != nil {
			t.Fatal(err)
		}
		opts := QueryStoreOptions{
			DesiredState: to, MaxStorageMB: before.MaxStorageMB, CaptureMode: before.CaptureMode,
			SizeCleanupMode: before.SizeCleanupMode, StaleThresholdDays: before.StaleThresholdDays,
			FlushIntervalSec: before.FlushIntervalSec, IntervalMinutes: before.IntervalMinutes,
			MaxPlansPerQuery: before.MaxPlansPerQuery, WaitStatsCaptureMode: before.WaitStatsCaptureMode,
			CapturePolicyExecCount: before.CapturePolicyExecCount, CapturePolicyCompileCPUMs: before.CapturePolicyCompileCPUMs,
			CapturePolicyExecCPUMs: before.CapturePolicyExecCPUMs, CapturePolicyStaleHours: before.CapturePolicyStaleHours,
		}
		if err := qs.SetOptions(ctx, opts); err != nil {
			t.Fatalf("%s → %s: %v", before.DesiredState, to, err)
		}
		after, err := qs.Options(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if after.DesiredState != to {
			t.Errorf("%s → %s: desired state is %s", before.DesiredState, to, after.DesiredState)
		}
		b, a := *before, *after
		b.DesiredState, b.ActualState, b.ReadOnlyReason, b.CurrentStorageMB = "", "", 0, 0
		a.DesiredState, a.ActualState, a.ReadOnlyReason, a.CurrentStorageMB = "", "", 0, 0
		if a != b {
			t.Errorf("%s → %s changed more than the state:\n before %+v\n after  %+v", before.DesiredState, to, b, a)
		}
		t.Logf("%s → %s: options kept (%+v)", before.DesiredState, to, a)
	}
}
