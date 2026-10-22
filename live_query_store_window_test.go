//go:build livedb

// Live verification of the Query Store report window: an interval that
// started before From but runs into the window belongs to the report. With
// the default 60-minute interval and a "last hour" window, that interval is
// most of the hour — a start_time-only filter dropped it and reported only
// the minutes since the top of the hour.
//
//	go test -tags livedb . -run TestLiveQueryStoreWindow -v \
//	  -livedb 'sqlserver://sa:PASS@host?TrustServerCertificate=true'
//
// Uses qsLiveSetup's throwaway database; touches nothing else.
package gosmo

import (
	"database/sql"
	"testing"
	"time"
)

// TestLiveQueryStoreWindowKeepsTheIntervalStraddlingFrom starts the window a
// millisecond after the workload's first interval began, so every execution
// sits in an interval that overlaps the window but started before it.
func TestLiveQueryStoreWindowKeepsTheIntervalStraddlingFrom(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()
	_, d, drop := qsLiveSetup(t, db, ctx)
	defer drop()

	var first time.Time
	if err := d.queryRow(ctx, func(r *sql.Row) error { return r.Scan(&first) },
		`SELECT MIN(rsi.start_time)`+qsRuntimeFrom+`
WHERE OBJECT_NAME(q.object_id) = 'qs_probe_read'`); err != nil {
		t.Fatalf("first probe interval: %v", err)
	}
	opts := QueryStoreReportOptions{
		Metric: QSMetricCPUTime, From: first.Add(time.Millisecond), To: time.Now().Add(time.Hour),
	}

	top, err := d.QueryStore().TopResourceQueries(ctx, opts)
	if err != nil {
		t.Fatalf("top resource queries: %v", err)
	}
	var execs int64
	for _, s := range top {
		if s.ObjectName == "dbo.qs_probe_read" {
			execs = s.ExecCount
		}
	}
	if execs != 40 {
		t.Errorf("probe ExecCount = %d with From inside its first interval, want all 40 executions", execs)
	}

	intervals, err := d.QueryStore().OverallConsumption(ctx, opts)
	if err != nil {
		t.Fatalf("overall consumption: %v", err)
	}
	if len(intervals) == 0 || !intervals[0].StartTime.Equal(first) {
		t.Errorf("Overall Resource Consumption does not start at the straddling interval %v: %d intervals", first, len(intervals))
	}
}

// TestLiveQueryStoreWindowBoundsThePlanListsCost asks for the plan list over
// a window a day before the workload: both plans still list (the LEFT JOIN's
// job), but neither may carry executions from outside the window.
func TestLiveQueryStoreWindowBoundsThePlanListsCost(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()
	_, d, drop := qsLiveSetup(t, db, ctx)
	defer drop()

	var queryID int64
	if err := d.queryRow(ctx, func(r *sql.Row) error { return r.Scan(&queryID) },
		`SELECT TOP (1) q.query_id FROM sys.query_store_query AS q
WHERE OBJECT_NAME(q.object_id) = 'qs_probe_read'`); err != nil {
		t.Fatalf("probe query id: %v", err)
	}
	now := time.Now()
	plans, err := d.QueryStore().Plans(ctx, queryID, QueryStoreReportOptions{
		Metric: QSMetricCPUTime, From: now.Add(-48 * time.Hour), To: now.Add(-24 * time.Hour),
	})
	if err != nil {
		t.Fatalf("query store plans: %v", err)
	}
	if len(plans) < 2 {
		t.Errorf("listed %d plans for a window the workload is outside, want both plans", len(plans))
	}
	for _, p := range plans {
		if p.ExecCount != 0 || p.Value != 0 {
			t.Errorf("plan %d: ExecCount %d, Value %v for a window it never ran in, want 0", p.PlanID, p.ExecCount, p.Value)
		}
	}
}
