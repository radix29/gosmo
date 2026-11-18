package gosmo

import (
	"context"
	"database/sql/driver"
	"strings"
	"testing"
	"time"
)

// agentStatusServer returns a Server over the recording driver, reporting the
// given engine edition, whose every query answers with rows.
func agentStatusServer(t *testing.T, edition EngineEdition, cols []string, rows [][]driver.Value) *Server {
	t.Helper()
	s := qsRecDB(t, 17, cols, rows).server
	s.info.EngineEdition = edition
	return s
}

// TestAgentInfoReadsTheServicesDMV pins the on-premises read: no row is
// "Unknown" rather than an error (the Agent service is not registered, or the
// DMV is not queryable here), and the status text decides Running whatever
// its case.
func TestAgentInfoReadsTheServicesDMV(t *testing.T) {
	started := time.Date(2026, 10, 1, 8, 30, 0, 0, time.UTC)
	cols := []string{"status_desc", "last_startup_time"}
	cases := []struct {
		name        string
		rows        [][]driver.Value
		wantRunning bool
		wantText    string
		wantStart   time.Time
	}{
		{"no row", nil, false, "Unknown", time.Time{}},
		{"running", [][]driver.Value{{"Running", started}}, true, "Running", started},
		{"running, other case", [][]driver.Value{{"RUNNING", started}}, true, "RUNNING", started},
		{"stopped, no startup time", [][]driver.Value{{"Stopped", nil}}, false, "Stopped", time.Time{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := agentStatusServer(t, EngineEnterprise, cols, c.rows)
			st, err := s.AgentInfo(context.Background())
			if err != nil {
				t.Fatalf("AgentInfo: %v", err)
			}
			if st.Running != c.wantRunning || st.StatusText != c.wantText || !st.LastStartupTime.Equal(c.wantStart) {
				t.Errorf("AgentInfo = %+v, want Running=%v StatusText=%q LastStartupTime=%v",
					*st, c.wantRunning, c.wantText, c.wantStart)
			}
			if q := qsRec.last(t).sql; !strings.Contains(q, "sys.dm_server_services") {
				t.Errorf("on-premises read did not query sys.dm_server_services:\n%s", q)
			}
		})
	}
}

// TestAgentInfoOnAzureReadsSessions pins the Azure branch, where
// sys.dm_server_services is empty: a live SQLAgent session is Running, a
// start date alone is Stopped (it survives an Agent stop, and is what a login
// without VIEW SERVER STATE sees), and neither is Unknown.
func TestAgentInfoOnAzureReadsSessions(t *testing.T) {
	started := time.Date(2026, 10, 1, 8, 30, 0, 0, time.UTC)
	cols := []string{"agent_start_date", "sessions"}
	cases := []struct {
		name        string
		row         []driver.Value
		wantRunning bool
		wantText    string
		wantStart   time.Time
	}{
		{"sessions", []driver.Value{started, int64(2)}, true, "Running", started},
		{"sessions, no start date", []driver.Value{nil, int64(1)}, true, "Running", time.Time{}},
		{"start date only", []driver.Value{started, int64(0)}, false, "Stopped", started},
		{"neither", []driver.Value{nil, int64(0)}, false, "Unknown", time.Time{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := agentStatusServer(t, EngineAzureManagedInst, cols, [][]driver.Value{c.row})
			st, err := s.AgentInfo(context.Background())
			if err != nil {
				t.Fatalf("AgentInfo: %v", err)
			}
			if st.Running != c.wantRunning || st.StatusText != c.wantText || !st.LastStartupTime.Equal(c.wantStart) {
				t.Errorf("AgentInfo = %+v, want Running=%v StatusText=%q LastStartupTime=%v",
					*st, c.wantRunning, c.wantText, c.wantStart)
			}
			q := qsRec.last(t).sql
			if strings.Contains(q, "sys.dm_server_services") || !strings.Contains(q, "msdb.dbo.syssessions") {
				t.Errorf("Azure read did not take the sessions branch:\n%s", q)
			}
		})
	}
}
