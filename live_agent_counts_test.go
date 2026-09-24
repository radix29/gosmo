//go:build livedb

package gosmo

import (
	"context"
	"strings"
	"testing"
)

// TestLiveAgentCounts pins AgentCounts against the list reads it replaces:
// each count must equal the length of the corresponding listing, and the
// EventAlerts count — a SQL restatement of Alert.IsEventAlert — must agree
// with the Go filter. A performance-condition alert is created so the two
// alert counts differ and the filter is exercised, not merely equal to
// Alerts on a server with no such alert.
//
//	go test -tags livedb . -run TestLiveAgentCounts -v -livedb '...'
func TestLiveAgentCounts(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()
	srv := &Server{db: db}

	const perfAlert = "gosmo_live_agentcounts_perf"
	drop := func() {
		srv.exec(context.Background(),
			"IF EXISTS (SELECT 1 FROM msdb.dbo.sysalerts WHERE name = N'"+perfAlert+"') "+
				"EXEC msdb.dbo.sp_delete_alert @name = N'"+perfAlert+"'")
	}
	drop()
	// Deferred, not t.Cleanup: a cleanup runs after the deferred done() has
	// closed the pool, and the drop then fails silently.
	defer drop()
	perfCreated := true
	// The object name carries the instance ("MSSQL$X:General Statistics" on
	// a named instance), so read it from the DMV rather than spell it.
	if err := srv.exec(ctx, `
DECLARE @obj nvarchar(128) = (SELECT TOP (1) RTRIM(object_name)
                              FROM sys.dm_os_performance_counters
                              WHERE counter_name = N'User Connections');
DECLARE @cond nvarchar(512) = @obj + N'|User Connections||>|1000000';
EXEC msdb.dbo.sp_add_alert @name = N'`+perfAlert+`', @enabled = 0,
     @performance_condition = @cond`); err != nil {
		// Managed Instance has no Agent alerting at all (Msg 41914); the
		// counts are still compared, just without the filter exercised.
		if !strings.Contains(err.Error(), "not supported") {
			t.Fatalf("create performance alert: %v", err)
		}
		t.Logf("no performance alert, so the EventAlerts filter is not exercised: %v", err)
		perfCreated = false
	}

	got, err := srv.AgentCounts(ctx)
	if err != nil {
		t.Fatalf("AgentCounts: %v", err)
	}
	jobs, err := srv.Jobs(ctx)
	if err != nil {
		t.Fatalf("Jobs: %v", err)
	}
	schedules, err := srv.Schedules(ctx)
	if err != nil {
		t.Fatalf("Schedules: %v", err)
	}
	alerts, err := srv.Alerts(ctx)
	if err != nil {
		t.Fatalf("Alerts: %v", err)
	}
	eventAlerts, err := srv.EventAlerts(ctx)
	if err != nil {
		t.Fatalf("EventAlerts: %v", err)
	}
	operators, err := srv.Operators(ctx)
	if err != nil {
		t.Fatalf("Operators: %v", err)
	}
	want := AgentCounts{
		Jobs:        len(jobs),
		Schedules:   len(schedules),
		Alerts:      len(alerts),
		EventAlerts: len(eventAlerts),
		Operators:   len(operators),
	}
	if *got != want {
		t.Errorf("AgentCounts = %+v, the listings give %+v", *got, want)
	}
	if perfCreated && got.EventAlerts >= got.Alerts {
		t.Errorf("EventAlerts = %d, Alerts = %d: the performance alert should be counted only in Alerts",
			got.EventAlerts, got.Alerts)
	}
}
