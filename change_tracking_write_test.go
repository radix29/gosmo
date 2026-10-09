package gosmo

import "testing"

func TestSetChangeTrackingRejectsUnknownRetentionUnit(t *testing.T) {
	d := &Database{Name: "appdb", server: &Server{}}
	err := d.SetChangeTracking(t.Context(), ChangeTrackingInfo{
		Enabled: true, RetentionPeriod: 2, RetentionUnit: "FORTNIGHTS",
	})
	if err == nil {
		t.Fatal("SetChangeTracking accepted an unrecognized retention unit, want an error")
	}
}

func TestChangeTrackingRetentionUnitsAllowlist(t *testing.T) {
	for _, unit := range []string{"DAYS", "HOURS", "MINUTES"} {
		if !changeTrackingRetentionUnits[ChangeTrackingUnit(unit)] {
			t.Errorf("%q should be a recognized retention unit", unit)
		}
	}
	if changeTrackingRetentionUnits["FORTNIGHTS"] {
		t.Error("FORTNIGHTS should not be a recognized retention unit")
	}
}

// SET CHANGE_TRACKING = ON on a database already tracked is Msg 5088, so
// enabling must branch to the reconfigure form on the server.
func TestBuildSetChangeTrackingStatement(t *testing.T) {
	cases := []struct {
		name string
		info ChangeTrackingInfo
		want string
	}{
		{
			name: "enable or reconfigure",
			info: ChangeTrackingInfo{Enabled: true, RetentionPeriod: 5, RetentionUnit: ChangeTrackingHours},
			want: `IF EXISTS (SELECT 1 FROM sys.change_tracking_databases WHERE database_id = DB_ID(N'app''db'))
    ALTER DATABASE [app'db] SET CHANGE_TRACKING (CHANGE_RETENTION = 5 HOURS, AUTO_CLEANUP = OFF);
ELSE
    ALTER DATABASE [app'db] SET CHANGE_TRACKING = ON (CHANGE_RETENTION = 5 HOURS, AUTO_CLEANUP = OFF);`,
		},
		{
			name: "unit defaults to days",
			info: ChangeTrackingInfo{Enabled: true, RetentionPeriod: 2, AutoCleanup: true},
			want: `IF EXISTS (SELECT 1 FROM sys.change_tracking_databases WHERE database_id = DB_ID(N'app''db'))
    ALTER DATABASE [app'db] SET CHANGE_TRACKING (CHANGE_RETENTION = 2 DAYS, AUTO_CLEANUP = ON);
ELSE
    ALTER DATABASE [app'db] SET CHANGE_TRACKING = ON (CHANGE_RETENTION = 2 DAYS, AUTO_CLEANUP = ON);`,
		},
		{
			name: "disable",
			info: ChangeTrackingInfo{},
			want: "ALTER DATABASE [app'db] SET CHANGE_TRACKING = OFF",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := buildSetChangeTrackingStatement("app'db", c.info)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != c.want {
				t.Errorf("got:\n%s\nwant:\n%s", got, c.want)
			}
		})
	}
}
