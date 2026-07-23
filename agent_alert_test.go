package gosmo

import "testing"

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
