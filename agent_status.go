package gosmo

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// agent_status.go answers whether SQL Server Agent is running, from SQL-visible
// state only (sys.dm_server_services, or the Agent's own sessions on Azure) —
// no Windows Service Control, WMI or registry access. The jobs it runs are in
// agent_job.go.

// AgentStatus reports whether SQL Server Agent is currently running, based
// only on SQL-visible state (sys.dm_server_services, or the Agent's own
// sessions on Azure) — no Windows Service Control, WMI, or registry access,
// matching the SQL-only scope of the rest of this file.
type AgentStatus struct {
	Running    bool
	StatusText string
	// LastStartupTime is the zero Time if the DMV has no matching row (the
	// Agent service isn't registered under this instance, or the DMV isn't
	// queryable in this edition/deployment).
	LastStartupTime time.Time
}

// AgentInfo reports SQL Server Agent's current run state.
func (s *Server) AgentInfo(ctx context.Context) (*AgentStatus, error) {
	if s.info.IsAzure() {
		return s.agentInfoAzure(ctx)
	}

	const q = `
SELECT status_desc, last_startup_time
FROM   sys.dm_server_services
WHERE  servicename LIKE N'SQL Server Agent%'`

	st := &AgentStatus{}
	var startup sql.NullTime
	if err := s.queryRowScan(ctx, q, nil, &st.StatusText, &startup); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return &AgentStatus{StatusText: "Unknown"}, nil
		}
		return nil, fmt.Errorf("gosmo: agent status: %w", err)
	}
	st.Running = strings.EqualFold(st.StatusText, "Running")
	if startup.Valid {
		st.LastStartupTime = startup.Time
	}
	return st, nil
}

// agentInfoAzure answers AgentInfo on an Azure engine edition, where
// sys.dm_server_services returns no rows at all — there is no Windows service
// to report, and xp_servicecontrol fails with "The specified service does not
// exist as an installed service" — so the on-prem read falls to its "Unknown"
// branch on a Managed Instance whose Agent is demonstrably running.
//
// Two SQL-visible facts stand in, keeping the no-WMI contract:
//
//   - Agent connects to the engine under program_name 'SQLAgent - ...' (a
//     Managed Instance shows "Generic Refresher" and "Email Logger"), so a
//     matching row in sys.dm_exec_sessions means it is up right now. That
//     read needs VIEW SERVER STATE; without it the count comes back 0 rather
//     than failing, which reads as stopped — see the fallback below.
//   - msdb.dbo.syssessions gets a row per Agent start, so its newest
//     agent_start_date is the last startup time. It survives an Agent stop,
//     which is why it cannot decide Running on its own.
func (s *Server) agentInfoAzure(ctx context.Context) (*AgentStatus, error) {
	const q = `
SELECT (SELECT MAX(agent_start_date) FROM msdb.dbo.syssessions),
       (SELECT COUNT(*) FROM sys.dm_exec_sessions
        WHERE program_name LIKE N'SQLAgent%')`

	var startup sql.NullTime
	var sessions int
	if err := s.queryRowScan(ctx, q, nil, &startup, &sessions); err != nil {
		return nil, fmt.Errorf("gosmo: agent status: %w", err)
	}

	st := &AgentStatus{Running: sessions > 0}
	if startup.Valid {
		st.LastStartupTime = startup.Time
	}
	switch {
	case st.Running:
		st.StatusText = "Running"
	case startup.Valid:
		// Agent has run at some point but holds no session now. Stopped is
		// the honest reading; a login without VIEW SERVER STATE lands here
		// too, which is why the startup time is still reported alongside.
		st.StatusText = "Stopped"
	default:
		st.StatusText = "Unknown"
	}
	return st, nil
}

// AgentCounts is how many of each Agent object msdb holds — the census an
// Agent overview shows beside its run state.
type AgentCounts struct {
	Jobs      int
	Schedules int
	// Alerts counts every alert; EventAlerts only the subset EventAlerts
	// returns (Alert.IsEventAlert).
	Alerts      int
	EventAlerts int
	Operators   int
}

// AgentCounts counts msdb's Agent objects in one round trip. Counting through
// Jobs/Schedules/EventAlerts/Operators instead costs four full catalog reads
// plus Jobs' xp_sqlagent_enum_jobs call, in series, to produce four numbers.
//
// EventAlerts mirrors Alert.IsEventAlert in SQL: no performance condition and
// an event source other than WMI. DATALENGTH rather than an equality test, because the Go
// predicate tests for the empty string and T-SQL's = ignores trailing spaces.
func (s *Server) AgentCounts(ctx context.Context) (*AgentCounts, error) {
	const q = `
SELECT (SELECT COUNT(*) FROM msdb.dbo.sysjobs),
       (SELECT COUNT(*) FROM msdb.dbo.sysschedules),
       (SELECT COUNT(*) FROM msdb.dbo.sysalerts),
       (SELECT COUNT(*) FROM msdb.dbo.sysalerts
        WHERE  DATALENGTH(ISNULL(performance_condition, N'')) = 0
        AND    UPPER(ISNULL(event_source, N'')) <> N'WMI'),
       (SELECT COUNT(*) FROM msdb.dbo.sysoperators)`

	c := &AgentCounts{}
	if err := s.queryRowScan(ctx, q, nil, &c.Jobs, &c.Schedules, &c.Alerts, &c.EventAlerts, &c.Operators); err != nil {
		return nil, fmt.Errorf("gosmo: agent counts: %w", err)
	}
	return c, nil
}
