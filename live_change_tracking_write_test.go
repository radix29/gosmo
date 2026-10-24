//go:build livedb

// Live verification of Database.SetChangeTracking's server-side branch:
// SET CHANGE_TRACKING = ON on a database already tracked is Msg 5088, so a
// reconfigure must take the other form, and only the server can say the IF
// around ALTER DATABASE is accepted.
//
//	go test -tags livedb . -run TestLiveChangeTrackingReconfigure -v \
//	  -livedb 'sqlserver://sa:PASS@host?TrustServerCertificate=true'
//
// Creates and drops its own throwaway database; touches nothing else.
package gosmo

import (
	"database/sql"
	"fmt"
	"testing"
)

func TestLiveChangeTrackingReconfigure(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()
	d, drop := liveScratchDB(t, db, ctx, "gosmo_ct_reconfig")
	defer drop()

	steps := []ChangeTrackingInfo{
		{Enabled: true, RetentionPeriod: 2, RetentionUnit: ChangeTrackingDays, AutoCleanup: true},
		{Enabled: true, RetentionPeriod: 5, RetentionUnit: ChangeTrackingHours, AutoCleanup: false},
		{Enabled: true, RetentionPeriod: 30, RetentionUnit: ChangeTrackingMinutes, AutoCleanup: true},
		{},
		{Enabled: true, RetentionPeriod: 3, RetentionUnit: ChangeTrackingDays, AutoCleanup: false},
	}
	for i, want := range steps {
		if err := d.SetChangeTracking(ctx, want); err != nil {
			t.Fatalf("step %d SetChangeTracking(%+v): %v", i, want, err)
		}
		got, err := d.ChangeTracking(ctx)
		if err != nil {
			t.Fatalf("step %d ChangeTracking: %v", i, err)
		}
		if *got != want {
			t.Errorf("step %d: read back %+v, want %+v", i, *got, want)
		}
	}
}

// M4 (review plan 2026-10-13): flipping TRACK_COLUMNS_UPDATED on a tracked
// table was Msg 4996; it now disables and re-enables in one atomic batch,
// which resets the table's tracking. Enabling with the same setting is a
// no-op that leaves the minimum valid version where it was.
func TestLiveTableChangeTrackingTrackColumnsFlip(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()
	d, drop := liveScratchDB(t, db, ctx, "gosmo_ct_table_flip")
	defer drop()

	if err := d.SetChangeTracking(ctx, ChangeTrackingInfo{Enabled: true, RetentionPeriod: 2, AutoCleanup: true}); err != nil {
		t.Fatalf("database SetChangeTracking: %v", err)
	}
	if _, err := d.exec(ctx, "CREATE TABLE dbo.t (id int PRIMARY KEY)"); err != nil {
		t.Fatalf("create table: %v", err)
	}
	tbl := d.TableRef("dbo", "t")
	minValid := func() int64 {
		t.Helper()
		var v int64
		if err := d.queryRow(ctx, func(r *sql.Row) error { return r.Scan(&v) },
			"SELECT ISNULL(CHANGE_TRACKING_MIN_VALID_VERSION(OBJECT_ID(N'dbo.t')), -1)"); err != nil {
			t.Fatalf("min valid version: %v", err)
		}
		return v
	}
	insert := func(id int) {
		t.Helper()
		if _, err := d.exec(ctx, fmt.Sprintf("INSERT dbo.t VALUES (%d)", id)); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}

	for i, step := range []struct{ enable, cols bool }{
		{true, false}, {true, true}, {true, true}, {true, false}, {false, false}, {true, true},
	} {
		insert(i)
		before := minValid()
		if err := tbl.SetChangeTracking(ctx, TableChangeTrackingOptions{Enabled: step.enable, TrackColumnsUpdated: step.cols}); err != nil {
			t.Fatalf("step %d SetChangeTracking(%v, %v): %v", i, step.enable, step.cols, err)
		}
		got, err := tbl.ChangeTracking(ctx)
		if err != nil {
			t.Fatalf("step %d ChangeTracking: %v", i, err)
		}
		if got.Enabled != step.enable || got.TrackColumnsUpdated != (step.enable && step.cols) {
			t.Errorf("step %d: read back %+v, want enabled %v cols %v", i, *got, step.enable, step.cols)
		}
		if i == 2 && minValid() != before {
			t.Errorf("step 2 (same setting again) reset tracking: min valid version %d -> %d", before, minValid())
		}
		if (i == 1 || i == 3) && minValid() == before {
			t.Errorf("step %d (flip) left min valid version at %d; tracking was not reset", i, before)
		}
	}
}
