//go:build livedb

// Live verification of the Query Store reports. Every query in
// query_store_reports.go is assembled by concatenation from a metric table, a
// statistic table and a window predicate, and a unit test can only pin the
// text it produced — the server is the only authority on whether that text
// parses, whether the aggregates are legal where they are used, and whether
// TOP (@p1) binds. A syntax error in any one of the eleven metric × five
// statistic combinations is invisible until something runs it.
//
//	go test -tags livedb . -run TestLiveQueryStore -v \
//	  -livedb 'sqlserver://sa:PASS@host?TrustServerCertificate=true'
//
// Creates and drops its own throwaway database; touches nothing else.
// Skipped entirely without -livedb.
package gosmo

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"
)

// qsLiveDBName is the throwaway database every test here works in. Query
// Store cannot be turned on in tempdb, so a real database is unavoidable.
const qsLiveDBName = "zz_gossms_qs_probe"

// qsLiveSetup creates the throwaway database, turns Query Store on, runs a
// workload that produces two plans for one query, and waits for Query Store
// to flush. It returns the server, the database handle and a dropper.
func qsLiveSetup(t *testing.T, db *sql.DB, ctx context.Context) (*Server, *Database, func()) {
	t.Helper()

	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", strings.SplitN(q, "\n", 2)[0], err)
		}
	}
	drop := func() {
		c := context.Background()
		db.ExecContext(c, "ALTER DATABASE ["+qsLiveDBName+"] SET SINGLE_USER WITH ROLLBACK IMMEDIATE")
		db.ExecContext(c, "DROP DATABASE ["+qsLiveDBName+"]")
	}
	drop()
	exec("CREATE DATABASE [" + qsLiveDBName + "]")

	srv, err := NewServer(ctx, db)
	if err != nil {
		drop()
		t.Fatalf("NewServer: %v", err)
	}
	// WAIT_STATS_CAPTURE_MODE is SQL Server 2017 syntax, and an option the
	// parser does not know fails the whole ALTER — so on 2016 the setup asks
	// for everything but it, rather than not running at all.
	waitStats := ", WAIT_STATS_CAPTURE_MODE = ON"
	if !hasColumnSince(srv.serverMajorVersion(), SQLServer2017) {
		waitStats = ""
	}
	exec("ALTER DATABASE [" + qsLiveDBName + "] SET QUERY_STORE = ON " +
		"(OPERATION_MODE = READ_WRITE, QUERY_CAPTURE_MODE = ALL, " +
		"INTERVAL_LENGTH_MINUTES = 1, DATA_FLUSH_INTERVAL_SECONDS = 60" +
		waitStats + ")")
	d := srv.Database(qsLiveDBName)

	// A workload: a table, a procedure over it, and enough executions with
	// different plans that every report has something to rank. The index is
	// created between the two batches so the second compile picks a different
	// plan — that is what makes Regressed Queries and Tracked Queries
	// non-empty rather than merely syntactically exercised.
	conn, err := db.Conn(ctx)
	if err != nil {
		drop()
		t.Fatalf("conn: %v", err)
	}
	defer conn.Close()
	run := func(q string) {
		t.Helper()
		if _, err := conn.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", strings.SplitN(q, "\n", 2)[0], err)
		}
	}
	run("USE [" + qsLiveDBName + "]")
	run(`CREATE TABLE dbo.qs_probe (id int IDENTITY PRIMARY KEY, pad char(200) NOT NULL, val int NOT NULL)`)
	run(`INSERT INTO dbo.qs_probe (pad, val)
	     SELECT TOP (20000) 'x', ABS(CHECKSUM(NEWID())) % 1000
	     FROM sys.all_columns a CROSS JOIN sys.all_columns b`)
	run(`CREATE OR ALTER PROCEDURE dbo.qs_probe_read @v int AS
	     SELECT COUNT(*) FROM dbo.qs_probe WHERE val = @v`)
	for i := range 20 {
		run(fmt.Sprintf("EXEC dbo.qs_probe_read %d", i))
	}
	run("CREATE INDEX ix_qs_probe_val ON dbo.qs_probe (val)")
	for i := range 20 {
		run(fmt.Sprintf("EXEC dbo.qs_probe_read %d", i))
	}
	// Query Store writes asynchronously; nothing is readable until it flushes.
	if err := d.FlushQueryStoreContext(ctx); err != nil {
		drop()
		t.Fatalf("FlushQueryStore: %v", err)
	}
	return srv, d, drop
}

// qsLiveWindow is the report window the tests use — wide enough to contain
// the workload whatever the interval boundaries fell on.
func qsLiveWindow() (time.Time, time.Time) {
	now := time.Now()
	return now.Add(-24 * time.Hour), now.Add(time.Hour)
}

// TestLiveQueryStoreEveryMetricAndStatisticParses runs every metric against
// every statistic through the report that uses the most of the expression
// machinery. Fifty-five combinations, each of which builds a different
// aggregate — the pooled standard deviation in particular, whose SQRT/ABS/
// NULLIF nesting no unit test can validate.
func TestLiveQueryStoreEveryMetricAndStatisticParses(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()
	_, d, drop := qsLiveSetup(t, db, ctx)
	defer drop()

	from, to := qsLiveWindow()
	for _, m := range d.QueryStoreMetrics() {
		for _, s := range QSStatistics() {
			t.Run(string(m)+"/"+string(s), func(t *testing.T) {
				opts := QueryStoreReportOptions{Metric: m, Statistic: s, From: from, To: to}
				if _, err := d.QueryStoreTopResourceQueriesContext(ctx, opts); err != nil {
					t.Errorf("top resource queries: %v", err)
				}
				if _, err := d.QueryStoreHighVariationQueriesContext(ctx, opts); err != nil {
					t.Errorf("high variation queries: %v", err)
				}
				if _, err := d.QueryStoreOverallConsumptionContext(ctx, opts); err != nil {
					t.Errorf("overall consumption: %v", err)
				}
			})
		}
	}
}

// TestLiveQueryStoreEveryReportReturnsTheWorkload checks each of the seven
// views against a database whose contents the test put there: every report
// that should see the probe procedure must actually return it. A report that
// parses but silently returns nothing is the failure mode this catches, and
// it is the one an empty test database hides.
func TestLiveQueryStoreEveryReportReturnsTheWorkload(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()
	_, d, drop := qsLiveSetup(t, db, ctx)
	defer drop()

	from, to := qsLiveWindow()
	opts := QueryStoreReportOptions{Metric: QSMetricCPUTime, Statistic: QSStatAvg, From: from, To: to}

	// Match on the module name, not the query text: the setup batch's own
	// INSERT and CREATE INDEX also mention dbo.qs_probe, and matching text
	// picks up whichever of them sorts highest instead of the procedure.
	findProbe := func(stats []*QSQueryStat) *QSQueryStat {
		for _, s := range stats {
			if s.ObjectName == "dbo.qs_probe_read" {
				return s
			}
		}
		return nil
	}

	top, err := d.QueryStoreTopResourceQueriesContext(ctx, opts)
	if err != nil {
		t.Fatalf("top resource queries: %v", err)
	}
	probe := findProbe(top)
	if probe == nil {
		t.Fatalf("Top Resource Consuming Queries returned %d rows, none of them the probe workload", len(top))
	}
	if probe.ExecCount < 40 {
		t.Errorf("probe ExecCount = %d, want at least the 40 executions the workload ran", probe.ExecCount)
	}
	if probe.ObjectName != "dbo.qs_probe_read" {
		t.Errorf("probe ObjectName = %q, want %q", probe.ObjectName, "dbo.qs_probe_read")
	}
	if probe.Value <= 0 {
		t.Errorf("probe Value = %v, want a positive average CPU time", probe.Value)
	}
	if probe.PlanCount < 2 {
		t.Errorf("probe PlanCount = %d, want the 2 plans the workload forced by adding an index", probe.PlanCount)
	}

	if _, err := d.QueryStoreHighVariationQueriesContext(ctx, opts); err != nil {
		t.Errorf("high variation queries: %v", err)
	}
	if _, err := d.QueryStoreRegressedQueriesContext(ctx, opts); err != nil {
		t.Errorf("regressed queries: %v", err)
	}

	intervals, err := d.QueryStoreOverallConsumptionContext(ctx, opts)
	if err != nil {
		t.Fatalf("overall consumption: %v", err)
	}
	if len(intervals) == 0 {
		t.Error("Overall Resource Consumption returned no intervals for a database that was just worked")
	}
	for i, iv := range intervals {
		if !iv.EndTime.After(iv.StartTime) {
			t.Errorf("interval %d: EndTime %v is not after StartTime %v", i, iv.EndTime, iv.StartTime)
		}
		if i > 0 && iv.StartTime.Before(intervals[i-1].StartTime) {
			t.Errorf("interval %d starts before its predecessor — rows are not in time order", i)
		}
	}

	tracked, err := d.QueryStoreTrackedQueryContext(ctx, probe.QueryID, opts)
	if err != nil {
		t.Fatalf("tracked query: %v", err)
	}
	if len(tracked) == 0 {
		t.Error("Tracked Queries returned no series for the probe query")
	}

	if !d.QueryStoreWaitStatsSupported() {
		t.Log("wait statistics unsupported on this instance — skipping both wait reports")
		return
	}
	cats, err := d.QueryStoreWaitCategoriesContext(ctx, opts)
	if err != nil {
		t.Fatalf("wait categories: %v", err)
	}
	if len(cats) == 0 {
		t.Log("no wait categories recorded for the workload — the query parsed but had nothing to report")
	}
	for _, c := range cats {
		if _, err := d.QueryStoreWaitingQueriesContext(ctx, c.Category, opts); err != nil {
			t.Errorf("waiting queries in %q: %v", c.Category, err)
		}
	}
	if _, err := d.QueryStoreWaitingQueriesContext(ctx, "", opts); err != nil {
		t.Errorf("waiting queries across every category: %v", err)
	}
}

// TestLiveQueryStoreForceAndUnforcePlan drives the write half end to end and
// reads the result back from sys.query_store_plan. Forcing is the one
// operation here that changes how the server executes anything, and its
// parameter names are the sort of thing a plausible-looking EXEC gets wrong
// silently — sp_query_store_force_plan is an extended stored procedure, so
// its signature is not in sys.all_parameters to check against.
func TestLiveQueryStoreForceAndUnforcePlan(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()
	_, d, drop := qsLiveSetup(t, db, ctx)
	defer drop()

	from, to := qsLiveWindow()
	opts := QueryStoreReportOptions{Metric: QSMetricCPUTime, From: from, To: to}

	top, err := d.QueryStoreTopResourceQueriesContext(ctx, opts)
	if err != nil {
		t.Fatalf("top resource queries: %v", err)
	}
	var queryID int64
	for _, s := range top {
		if strings.Contains(s.ObjectName, "qs_probe_read") {
			queryID = s.QueryID
			break
		}
	}
	if queryID == 0 {
		t.Fatal("the probe query is not in Query Store — nothing to force")
	}

	plans, err := d.QueryStorePlansContext(ctx, queryID, opts)
	if err != nil {
		t.Fatalf("query store plans: %v", err)
	}
	if len(plans) < 2 {
		t.Fatalf("query %d has %d plans, want the 2 the workload produced", queryID, len(plans))
	}
	for _, p := range plans {
		if p.IsForced {
			t.Fatalf("plan %d is already forced before the test forced anything", p.PlanID)
		}
		if !strings.Contains(p.QueryPlanXML, "<ShowPlanXML") {
			t.Errorf("plan %d: QueryPlanXML does not look like a showplan document", p.PlanID)
		}
	}

	// Force the *second* plan, not the first: forcing plans[0] would pass
	// against an implementation that ignores the plan id it was handed.
	target := plans[1]
	if err := d.QueryStoreForcePlanContext(ctx, queryID, target.PlanID); err != nil {
		t.Fatalf("force plan %d: %v", target.PlanID, err)
	}

	after, err := d.QueryStorePlansContext(ctx, queryID, opts)
	if err != nil {
		t.Fatalf("query store plans after forcing: %v", err)
	}
	for _, p := range after {
		if p.PlanID == target.PlanID && !p.IsForced {
			t.Errorf("plan %d was forced but reads back as not forced", p.PlanID)
		}
		if p.PlanID != target.PlanID && p.IsForced {
			t.Errorf("plan %d reads back as forced, but plan %d was the one forced", p.PlanID, target.PlanID)
		}
	}

	forced, err := d.QueryStoreForcedPlanQueriesContext(ctx, opts)
	if err != nil {
		t.Fatalf("forced plan queries: %v", err)
	}
	var seen bool
	for _, s := range forced {
		if s.QueryID == queryID {
			seen = true
			if s.ForcedPlanID != target.PlanID {
				t.Errorf("Queries With Forced Plans reports plan %d forced, want %d", s.ForcedPlanID, target.PlanID)
			}
		}
	}
	if !seen {
		t.Errorf("Queries With Forced Plans did not list query %d after its plan was forced", queryID)
	}

	if err := d.QueryStoreUnforcePlanContext(ctx, queryID, target.PlanID); err != nil {
		t.Fatalf("unforce plan %d: %v", target.PlanID, err)
	}
	final, err := d.QueryStorePlansContext(ctx, queryID, opts)
	if err != nil {
		t.Fatalf("query store plans after unforcing: %v", err)
	}
	for _, p := range final {
		if p.IsForced {
			t.Errorf("plan %d is still forced after Unforce", p.PlanID)
		}
	}
}

// TestLiveQueryStoreQueryTextRoundTrips pins the by-id text read, including
// the not-found classification a caller branches on.
func TestLiveQueryStoreQueryTextRoundTrips(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()
	_, d, drop := qsLiveSetup(t, db, ctx)
	defer drop()

	from, to := qsLiveWindow()
	opts := QueryStoreReportOptions{From: from, To: to}
	top, err := d.QueryStoreTopResourceQueriesContext(ctx, opts)
	if err != nil {
		t.Fatalf("top resource queries: %v", err)
	}
	if len(top) == 0 {
		t.Fatal("no queries in Query Store")
	}
	text, object, err := d.QueryStoreQueryTextContext(ctx, top[0].QueryID)
	if err != nil {
		t.Fatalf("query text: %v", err)
	}
	if text != top[0].QueryText {
		t.Errorf("QueryStoreQueryText = %q, but the report reported %q", text, top[0].QueryText)
	}
	if object != top[0].ObjectName {
		t.Errorf("object name = %q, but the report reported %q", object, top[0].ObjectName)
	}

	if _, _, err := d.QueryStoreQueryTextContext(ctx, 999999999); err == nil {
		t.Error("a query id Query Store does not hold returned no error")
	}
}
