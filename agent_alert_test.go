package gosmo

import (
	"context"
	"strings"
	"testing"
)

func TestNotificationMethodString(t *testing.T) {
	cases := []struct {
		m    NotificationMethod
		want string
	}{
		{0, ""},
		{NotifyMethodEmail, "Email"},
		{NotifyMethodPager, "Pager"},
		{NotifyMethodNetSend, "Net Send"},
		{NotifyMethodEmail | NotifyMethodPager, "Email, Pager"},
		{NotifyMethodEmail | NotifyMethodPager | NotifyMethodNetSend, "Email, Pager, Net Send"},
	}
	for _, c := range cases {
		if got := c.m.String(); got != c.want {
			t.Errorf("NotificationMethod(%d).String() = %q, want %q", c.m, got, c.want)
		}
	}
}

func TestAlertIsEventAlert(t *testing.T) {
	cases := []struct {
		name string
		a    *Alert
		want bool
	}{
		{"plain SQL Server event alert", &Alert{EventSource: "MSSQLSERVER"}, true},
		{"WMI alert", &Alert{EventSource: "WMI"}, false},
		{"performance condition alert", &Alert{EventSource: "MSSQLSERVER", PerformanceCondition: "Buffer Manager|Page life expectancy|<|300"}, false},
		{"lowercase wmi event source", &Alert{EventSource: "wmi"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.a.IsEventAlert(); got != c.want {
				t.Errorf("IsEventAlert() = %v, want %v", got, c.want)
			}
		})
	}
}

func TestParseSQLAgentDateOrZero(t *testing.T) {
	if !parseSQLAgentDateOrZero(0, 0).IsZero() {
		t.Errorf("parseSQLAgentDateOrZero(0, 0) should be zero Time")
	}
	if !parseSQLAgentDateOrZero(0, 123456).IsZero() {
		t.Errorf("parseSQLAgentDateOrZero(0, 123456) should be zero Time regardless of time component")
	}
	got := parseSQLAgentDateOrZero(20260722, 143059)
	if got.IsZero() {
		t.Errorf("parseSQLAgentDateOrZero(20260722, 143059) should not be zero Time")
	}
}

// TestAlertSetJobResponseClearsWithAnEmptyName pins the sentinel that clears
// an alert's job response. sp_update_alert maps an empty @job_name to a
// job_id of 0x00; any placeholder name — [UNSPECIFIED] was the one shipped
// here — is looked up as a real job and fails with "The specified @job_name
// does not exist".
func TestAlertSetJobResponseClearsWithAnEmptyName(t *testing.T) {
	a := &Alert{server: &Server{}, Name: "Disk full", JobName: "Nightly"}
	ctx, script := WithScript(context.Background())

	if err := a.SetJobResponseContext(ctx, ""); err != nil {
		t.Fatalf("SetJobResponseContext under WithScript: %v", err)
	}
	if len(script.Statements) != 1 {
		t.Fatalf("Statements = %d, want 1", len(script.Statements))
	}
	got := script.Statements[0]
	want := "EXEC msdb.dbo.sp_update_alert @name = N'Disk full', @job_name = N''"
	if got != want {
		t.Errorf("statement = %q, want %q", got, want)
	}
	if strings.Contains(got, "UNSPECIFIED") {
		t.Errorf("cleared job response still names a placeholder job:\n%s", got)
	}
}

// TestAlertSetJobResponseNamesTheJob pins the ordinary case alongside it.
func TestAlertSetJobResponseNamesTheJob(t *testing.T) {
	a := &Alert{server: &Server{}, Name: "Disk full"}
	ctx, script := WithScript(context.Background())

	if err := a.SetJobResponseContext(ctx, "Nightly O'Brien"); err != nil {
		t.Fatalf("SetJobResponseContext under WithScript: %v", err)
	}
	want := "EXEC msdb.dbo.sp_update_alert @name = N'Disk full', @job_name = N'Nightly O''Brien'"
	if len(script.Statements) != 1 || script.Statements[0] != want {
		t.Errorf("Statements = %q, want [%q]", script.Statements, want)
	}
}
