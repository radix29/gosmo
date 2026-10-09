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

import "testing"

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
