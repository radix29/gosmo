package gosmo

import "testing"

func TestSetChangeTrackingRejectsUnknownRetentionUnit(t *testing.T) {
	d := &Database{name: "appdb", server: &Server{}}
	err := d.SetChangeTracking(ChangeTrackingInfo{
		Enabled: true, RetentionPeriod: 2, RetentionUnit: "FORTNIGHTS",
	})
	if err == nil {
		t.Fatal("SetChangeTracking accepted an unrecognized retention unit, want an error")
	}
}

func TestChangeTrackingRetentionUnitsAllowlist(t *testing.T) {
	for _, unit := range []string{"DAYS", "HOURS", "MINUTES"} {
		if !changeTrackingRetentionUnits[unit] {
			t.Errorf("%q should be a recognized retention unit", unit)
		}
	}
	if changeTrackingRetentionUnits["FORTNIGHTS"] {
		t.Error("FORTNIGHTS should not be a recognized retention unit")
	}
}
