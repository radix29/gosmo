package gosmo

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// -- recording driver: keeps the parameters as well as the statement -------
//
// The shared capture driver in identifier_quoting_test.go drops the argument
// list, and the argument list is exactly what these reports get wrong: every
// query here is concatenated from fragments and numbers its own @pN as it
// goes, so a fragment added in the wrong order binds a time range to TOP.

type qsRecDriver struct{}

func (qsRecDriver) Open(string) (driver.Conn, error) { return &qsRecConn{}, nil }

type qsRecConn struct{}

func (c *qsRecConn) Prepare(string) (driver.Stmt, error) { return nil, driver.ErrSkip }
func (c *qsRecConn) Close() error                        { return nil }
func (c *qsRecConn) Begin() (driver.Tx, error)           { return nil, driver.ErrSkip }

func (c *qsRecConn) ExecContext(_ context.Context, q string, args []driver.NamedValue) (driver.Result, error) {
	qsRec.add(q, args)
	return driver.ResultNoRows, nil
}

func (c *qsRecConn) QueryContext(_ context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	qsRec.add(q, args)
	return qsRec.reply(), nil
}

// qsRecCall is one statement the driver was handed, with its parameters in
// the order the driver received them.
type qsRecCall struct {
	sql  string
	args []any
}

type qsRecLog struct {
	mu    sync.Mutex
	calls []qsRecCall
	cols  []string
	rows  [][]driver.Value
}

func (l *qsRecLog) add(q string, args []driver.NamedValue) {
	l.mu.Lock()
	defer l.mu.Unlock()
	// The USE that Database.query issues to reach the database is plumbing,
	// not a statement under test, and recording it would offset every index a
	// test asserts on.
	if strings.HasPrefix(q, "USE ") {
		return
	}
	vals := make([]any, len(args))
	for i, a := range args {
		vals[i] = a.Value
	}
	l.calls = append(l.calls, qsRecCall{sql: q, args: vals})
}

func (l *qsRecLog) reply() *qsRecRows {
	l.mu.Lock()
	defer l.mu.Unlock()
	return &qsRecRows{cols: l.cols, rows: l.rows}
}

func (l *qsRecLog) reset(cols []string, rows [][]driver.Value) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls, l.cols, l.rows = nil, cols, rows
}

// last returns the most recent recorded statement, failing the test if
// nothing was recorded at all — a report that never reached the driver would
// otherwise pass every assertion about its SQL vacuously.
func (l *qsRecLog) last(t *testing.T) qsRecCall {
	t.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.calls) == 0 {
		t.Fatal("no statement reached the driver")
	}
	return l.calls[len(l.calls)-1]
}

type qsRecRows struct {
	cols []string
	rows [][]driver.Value
	next int
}

func (r *qsRecRows) Columns() []string {
	if r.cols == nil {
		return []string{"c"}
	}
	return r.cols
}
func (r *qsRecRows) Close() error { return nil }
func (r *qsRecRows) Next(dest []driver.Value) error {
	if r.next >= len(r.rows) {
		return io.EOF
	}
	copy(dest, r.rows[r.next])
	r.next++
	return nil
}

var qsRec qsRecLog

func init() { sql.Register("qsrec", qsRecDriver{}) }

// qsRecDB returns a Database over the recording driver, reporting the given
// major version. cols/rows are what every query answers with.
func qsRecDB(t *testing.T, major int, cols []string, rows [][]driver.Value) *Database {
	t.Helper()
	pool, err := sql.Open("qsrec", "")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { pool.Close() })
	qsRec.reset(cols, rows)
	return &Database{name: "appdb", server: &Server{db: pool, info: &ServerInfo{VersionMajor: major}}}
}

// -- the two lookup tables -----------------------------------------------------

// TestQSMetricDefsArePinnedByName pins every metric to the
// sys.query_store_runtime_stats column stem it reads, by name.
//
// This is the fault a round-trip test cannot see: the metric list and the
// column list are consulted through the same table, so swapping two entries
// leaves every caller self-consistent — "CPU time" would rank by duration and
// nothing in the library would disagree with itself. The pairs below are the
// only place that says which is which, and the length check is what makes a
// newly added metric fail here until it is pinned too.
func TestQSMetricDefsArePinnedByName(t *testing.T) {
	want := []struct {
		metric  QSMetric
		column  string
		unit    QSUnit
		version ServerVersion
	}{
		{QSMetricDuration, "duration", QSUnitMicroseconds, SQLServer2016},
		{QSMetricCPUTime, "cpu_time", QSUnitMicroseconds, SQLServer2016},
		{QSMetricLogicalReads, "logical_io_reads", QSUnitPages, SQLServer2016},
		{QSMetricLogicalWrites, "logical_io_writes", QSUnitPages, SQLServer2016},
		{QSMetricPhysicalReads, "physical_io_reads", QSUnitPages, SQLServer2016},
		{QSMetricCLRTime, "clr_time", QSUnitMicroseconds, SQLServer2016},
		{QSMetricDOP, "dop", QSUnitCount, SQLServer2016},
		{QSMetricMemory, "query_max_used_memory", QSUnitPages, SQLServer2016},
		{QSMetricRowCount, "rowcount", QSUnitCount, SQLServer2016},
		{QSMetricLogMemory, "log_bytes_used", QSUnitBytes, SQLServer2017},
		{QSMetricTempDBMemory, "tempdb_space_used", QSUnitPages, SQLServer2017},
	}
	if len(want) != len(qsMetricDefs) {
		t.Fatalf("qsMetricDefs has %d entries, this test pins %d — pin the new metric here too",
			len(qsMetricDefs), len(want))
	}
	seen := map[string]QSMetric{}
	for _, w := range want {
		def, ok := qsMetric(w.metric)
		if !ok {
			t.Errorf("metric %q is not in qsMetricDefs", w.metric)
			continue
		}
		if def.column != w.column {
			t.Errorf("metric %q reads column stem %q, want %q", w.metric, def.column, w.column)
		}
		if def.unit != w.unit {
			t.Errorf("metric %q has unit %v, want %v", w.metric, def.unit, w.unit)
		}
		if def.minVersion != w.version {
			t.Errorf("metric %q needs version %v, want %v", w.metric, def.minVersion, w.version)
		}
		if prev, dup := seen[w.column]; dup {
			t.Errorf("metrics %q and %q both read column stem %q", prev, w.metric, w.column)
		}
		seen[w.column] = w.metric
	}
}

// TestQSStatisticDefsArePinnedByName pins the SQL each statistic generates.
//
// The averages and totals are execution-weighted, which is the part a
// plausible simplification undoes: AVG(rs.avg_cpu_time) reads correctly and
// is wrong, because it counts an interval with one execution as heavily as
// one with ten thousand.
func TestQSStatisticDefsArePinnedByName(t *testing.T) {
	want := []struct {
		stat QSStatistic
		expr string
	}{
		{QSStatAvg, "SUM(rs.avg_cpu_time * rs.count_executions) / NULLIF(SUM(rs.count_executions), 0)"},
		{QSStatMin, "CAST(MIN(rs.min_cpu_time) AS float)"},
		{QSStatMax, "CAST(MAX(rs.max_cpu_time) AS float)"},
		{QSStatTotal, "SUM(rs.avg_cpu_time * rs.count_executions)"},
		{QSStatStdDev, "SQRT(ABS(SUM((POWER(rs.stdev_cpu_time, 2) + POWER(rs.avg_cpu_time, 2)) * rs.count_executions) / NULLIF(SUM(rs.count_executions), 0) - POWER(SUM(rs.avg_cpu_time * rs.count_executions) / NULLIF(SUM(rs.count_executions), 0), 2)))"},
	}
	if len(want) != len(qsStatisticDefs) {
		t.Fatalf("qsStatisticDefs has %d entries, this test pins %d — pin the new statistic here too",
			len(qsStatisticDefs), len(want))
	}
	for _, w := range want {
		def, ok := qsStatistic(w.stat)
		if !ok {
			t.Errorf("statistic %q is not in qsStatisticDefs", w.stat)
			continue
		}
		if got := def.runtime("rs", "cpu_time"); got != w.expr {
			t.Errorf("statistic %q renders\n  %s\nwant\n  %s", w.stat, got, w.expr)
		}
	}
}

// TestQSWaitStatisticDefsArePinnedByName pins the SQL each statistic
// generates over sys.query_store_wait_stats, the way
// TestQSStatisticDefsArePinnedByName pins the runtime-stats half.
//
// Two faults it exists to catch, both of which read correctly. The averages
// divide by the execution count from the *joined* runtime-stats row, because
// sys.query_store_wait_stats carries no count of its own — so an average
// built from it alone is an average of per-interval averages, the same
// weighting fault as above in the one place the column is not even available
// to get right by accident. And every expression is float-typed; see
// TestQSWaitAveragesDoNotTruncateToWholeMilliseconds.
func TestQSWaitStatisticDefsArePinnedByName(t *testing.T) {
	want := []struct {
		stat QSStatistic
		expr string
	}{
		{QSStatAvg, "CAST(SUM(ws.total_query_wait_time_ms) AS float) / NULLIF(SUM(rs.count_executions), 0)"},
		{QSStatMin, "CAST(MIN(ws.min_query_wait_time_ms) AS float)"},
		{QSStatMax, "CAST(MAX(ws.max_query_wait_time_ms) AS float)"},
		{QSStatTotal, "CAST(SUM(ws.total_query_wait_time_ms) AS float)"},
		{QSStatStdDev, "SQRT(ABS(SUM((POWER(ws.stdev_query_wait_time_ms, 2) + POWER(ws.avg_query_wait_time_ms, 2)) * rs.count_executions) / NULLIF(SUM(rs.count_executions), 0) - POWER(SUM(ws.avg_query_wait_time_ms * rs.count_executions) / NULLIF(SUM(rs.count_executions), 0), 2)))"},
	}
	if len(want) != len(qsStatisticDefs) {
		t.Fatalf("qsStatisticDefs has %d entries, this test pins %d — pin the new statistic here too",
			len(qsStatisticDefs), len(want))
	}
	for _, w := range want {
		def, ok := qsStatistic(w.stat)
		if !ok {
			t.Errorf("statistic %q is not in qsStatisticDefs", w.stat)
			continue
		}
		if got := def.wait("ws", "rs"); got != w.expr {
			t.Errorf("statistic %q renders\n  %s\nwant\n  %s", w.stat, got, w.expr)
		}
	}
}

// TestQSWaitAveragesDoNotTruncateToWholeMilliseconds pins the CAST on the
// numerator of every wait expression that divides.
//
// sys.query_store_wait_stats reports total_/min_/max_query_wait_time_ms as
// bigint, and count_executions is bigint too, so dividing one sum by the
// other is integer division: SQL Server truncates it and reports nothing.
// Every wait category averaging under a millisecond per execution then comes
// back as 0, and QueryStoreWaitCategoriesContext's ORDER BY value DESC ties
// all of them — the ranking the report exists for becomes arbitrary. Avg
// shipped that way.
//
// Asserted on the text either side of the "/" rather than on the whole
// expression, so the pin survives a rename of the aliases and still fails if
// a CAST moves to the outside of the division, where it no longer helps.
func TestQSWaitAveragesDoNotTruncateToWholeMilliseconds(t *testing.T) {
	checked := 0
	for _, def := range qsStatisticDefs {
		expr := def.wait("ws", "rs")
		for _, numerator := range integerDividends(expr) {
			checked++
			if !strings.Contains(numerator, "AS float)") {
				t.Errorf("statistic %q divides an uncast bigint sum — %s truncates in\n  %s",
					def.Statistic, numerator, expr)
			}
		}
	}
	// Avg is the one statistic that divides a bigint wait column; Std dev
	// divides too, but by way of avg_/stdev_, which are float already. A
	// changed expression that this test no longer recognises would pass it by
	// finding nothing to check.
	if checked != 1 {
		t.Errorf("integerDividends found %d bigint numerator(s) across the wait expressions, want 1 (Avg) — "+
			"the expressions changed shape and this test is no longer reading them", checked)
	}
}

// integerDividends returns, for each "/" in expr, the innermost aggregate
// immediately to its left — the numerator whose type decides whether the
// division truncates. A numerator over a float column is safe as it stands;
// one over a bigint column has to be CAST.
func integerDividends(expr string) []string {
	var out []string
	for i, r := range expr {
		if r != '/' {
			continue
		}
		left := strings.TrimSpace(expr[:i])
		start := strings.LastIndexAny(left, "(+-*")
		term := strings.TrimSpace(left[start+1:])
		// A closing ")" ends the aggregate the "/" divides; walk back to its
		// own opening rather than to the nearest "(" to its left, which
		// belongs to something nested inside it.
		if strings.HasSuffix(term, ")") {
			depth := 0
			for j := len(left) - 1; j >= 0; j-- {
				switch left[j] {
				case ')':
					depth++
				case '(':
					depth--
				}
				if depth == 0 {
					start = strings.LastIndexAny(left[:j], "(+-*, ")
					term = strings.TrimSpace(left[start+1:])
					j = -1
				}
			}
		}
		if !mentionsBigintWaitColumn(term) {
			continue
		}
		out = append(out, term)
	}
	return out
}

// mentionsBigintWaitColumn reports whether term reads one of the wait
// columns SQL Server types as bigint. The float ones — avg_ and stdev_ —
// need no CAST, and demanding one everywhere would pin noise.
func mentionsBigintWaitColumn(term string) bool {
	for _, col := range []string{
		"total_query_wait_time_ms", "min_query_wait_time_ms",
		"max_query_wait_time_ms", "last_query_wait_time_ms",
	} {
		if strings.Contains(term, col) {
			return true
		}
	}
	return false
}

// TestQueryStoreMetricsOmitsMetricsTheInstanceLacks checks the version gate
// on the metric selector: offering "Log memory used" against SQL Server 2016
// produces "Invalid column name avg_log_bytes_used", which reaches the user
// as a failed report rather than a missing menu entry.
func TestQueryStoreMetricsOmitsMetricsTheInstanceLacks(t *testing.T) {
	has := func(ms []QSMetric, m QSMetric) bool {
		for _, x := range ms {
			if x == m {
				return true
			}
		}
		return false
	}

	d2016 := &Database{name: "appdb", server: &Server{info: &ServerInfo{VersionMajor: int(SQLServer2016)}}}
	got := d2016.QueryStoreMetrics()
	if has(got, QSMetricLogMemory) || has(got, QSMetricTempDBMemory) {
		t.Errorf("a 2016 instance was offered a 2017-only metric: %v", got)
	}
	if !has(got, QSMetricDuration) {
		t.Errorf("a 2016 instance was not offered Duration: %v", got)
	}

	d2017 := &Database{name: "appdb", server: &Server{info: &ServerInfo{VersionMajor: int(SQLServer2017)}}}
	if got := d2017.QueryStoreMetrics(); !has(got, QSMetricLogMemory) {
		t.Errorf("a 2017 instance was not offered Log memory used: %v", got)
	}

	// A Server whose version was never read must not be narrowed to nothing:
	// the query is a better authority than a version we do not have.
	unknown := &Database{name: "appdb", server: &Server{}}
	if got := unknown.QueryStoreMetrics(); len(got) != len(qsMetricDefs) {
		t.Errorf("an instance of unknown version was offered %d of %d metrics", len(got), len(qsMetricDefs))
	}
}

// -- options ------------------------------------------------------------------

// TestQueryStoreReportOptionsResolveFillsDefaults pins the zero value: the
// last hour, by average duration, top 25 — and a baseline window of the same
// length immediately before the reported one, which is what makes a zero
// Options meaningful for Regressed Queries rather than comparing a window
// against itself.
func TestQueryStoreReportOptionsResolveFillsDefaults(t *testing.T) {
	before := time.Now()
	sp, err := QueryStoreReportOptions{}.resolve()
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if sp.metric.Metric != QSMetricDuration {
		t.Errorf("default metric = %q, want %q", sp.metric.Metric, QSMetricDuration)
	}
	if sp.stat.Statistic != QSStatAvg {
		t.Errorf("default statistic = %q, want %q", sp.stat.Statistic, QSStatAvg)
	}
	if sp.opts.Top != QSDefaultTop {
		t.Errorf("default Top = %d, want %d", sp.opts.Top, QSDefaultTop)
	}
	if sp.opts.To.Before(before) {
		t.Errorf("default To = %v, want no earlier than now", sp.opts.To)
	}
	if d := sp.opts.To.Sub(sp.opts.From); d != time.Hour {
		t.Errorf("default window is %v long, want 1h", d)
	}
	if !sp.opts.BaselineTo.Equal(sp.opts.From) {
		t.Errorf("baseline ends at %v, want it to end where the report begins (%v)", sp.opts.BaselineTo, sp.opts.From)
	}
	if d := sp.opts.BaselineTo.Sub(sp.opts.BaselineFrom); d != time.Hour {
		t.Errorf("default baseline window is %v long, want the reported window's 1h", d)
	}

	// An explicit window must survive resolve untouched, and set the baseline
	// to the equally long window before it.
	to := time.Date(2026, 3, 4, 12, 0, 0, 0, time.UTC)
	from := to.Add(-6 * time.Hour)
	sp, err = QueryStoreReportOptions{From: from, To: to}.resolve()
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if !sp.opts.From.Equal(from) || !sp.opts.To.Equal(to) {
		t.Errorf("window = [%v, %v), want [%v, %v)", sp.opts.From, sp.opts.To, from, to)
	}
	if !sp.opts.BaselineTo.Equal(from) || !sp.opts.BaselineFrom.Equal(from.Add(-6*time.Hour)) {
		t.Errorf("baseline = [%v, %v), want [%v, %v)",
			sp.opts.BaselineFrom, sp.opts.BaselineTo, from.Add(-6*time.Hour), from)
	}
}

// TestQueryStoreReportRejectsUnknownMetricOrStatistic confirms the two names
// that reach a query as raw SQL are checked against their tables before any
// statement is built — the same end-to-end check
// TestSetQueryStoreOptionsRejectsUnknownValues makes for the writer.
func TestQueryStoreReportRejectsUnknownMetricOrStatistic(t *testing.T) {
	d := &Database{name: "appdb", server: &Server{}}
	inject := "cpu_time) FROM sys.query_store_query; DROP TABLE dbo.Secrets; --"

	if _, err := d.QueryStoreTopResourceQueries(QueryStoreReportOptions{Metric: QSMetric(inject)}); err == nil {
		t.Error("an unknown metric was accepted")
	}
	if _, err := d.QueryStoreTopResourceQueries(QueryStoreReportOptions{Statistic: QSStatistic(inject)}); err == nil {
		t.Error("an unknown statistic was accepted")
	}
	// Every report resolves through the same path; check one of each shape so
	// a new report that forgot to call resolve is caught.
	if _, err := d.QueryStoreOverallConsumption(QueryStoreReportOptions{Metric: QSMetric(inject)}); err == nil {
		t.Error("an unknown metric was accepted by Overall Resource Consumption")
	}
	if _, err := d.QueryStoreRegressedQueries(QueryStoreReportOptions{Metric: QSMetric(inject)}); err == nil {
		t.Error("an unknown metric was accepted by Regressed Queries")
	}
	if _, err := d.QueryStorePlans(1, QueryStoreReportOptions{Metric: QSMetric(inject)}); err == nil {
		t.Error("an unknown metric was accepted by QueryStorePlans")
	}
}

// -- parameter binding ---------------------------------------------------------

// qsWindow is a fixed report window for the binding tests.
var (
	qsTo   = time.Date(2026, 3, 4, 12, 0, 0, 0, time.UTC)
	qsFrom = qsTo.Add(-2 * time.Hour)
)

// TestQueryStoreTopResourceQueriesBindsItsParametersInOrder pins the
// parameter numbering. These queries are concatenated from fragments, each of
// which appends its own parameters as it renders, so a fragment moved in the
// statement without being moved in the build order silently binds a timestamp
// to TOP — which the server accepts on some type combinations rather than
// rejecting.
func TestQueryStoreTopResourceQueriesBindsItsParametersInOrder(t *testing.T) {
	d := qsRecDB(t, 17, nil, nil)
	_, err := d.QueryStoreTopResourceQueriesContext(context.Background(), QueryStoreReportOptions{
		Metric: QSMetricCPUTime, From: qsFrom, To: qsTo, Top: 10, MinExecCount: 5,
	})
	if err != nil {
		t.Fatalf("top resource queries: %v", err)
	}
	call := qsRec.last(t)
	want := []any{int64(10), qsFrom, qsTo, int64(5)}
	if len(call.args) != len(want) {
		t.Fatalf("bound %d parameters (%v), want %d", len(call.args), call.args, len(want))
	}
	for i := range want {
		if call.args[i] != want[i] {
			t.Errorf("@p%d = %v, want %v", i+1, call.args[i], want[i])
		}
	}
	for _, frag := range []string{
		"TOP (@p1)",
		"rsi.start_time >= @p2 AND rsi.start_time < @p3",
		"HAVING SUM(rs.count_executions) >= @p4",
		"q.is_internal_query = 0",
	} {
		if !strings.Contains(call.sql, frag) {
			t.Errorf("statement is missing %q:\n%s", frag, call.sql)
		}
	}
}

// TestQueryStoreRegressedQueriesBindsBothWindowsSeparately is the same check
// where it matters most: two windows are rendered by the same closure, one
// after the other, and a build order that rendered the baseline CTE's text
// before the reported one's would leave both CTEs reading the same range and
// every regression exactly zero — a report that looks like a quiet server.
func TestQueryStoreRegressedQueriesBindsBothWindowsSeparately(t *testing.T) {
	baseTo := qsFrom
	baseFrom := baseTo.Add(-2 * time.Hour)

	d := qsRecDB(t, 17, nil, nil)
	_, err := d.QueryStoreRegressedQueriesContext(context.Background(), QueryStoreReportOptions{
		Metric: QSMetricCPUTime, From: qsFrom, To: qsTo,
		BaselineFrom: baseFrom, BaselineTo: baseTo, Top: 10,
	})
	if err != nil {
		t.Fatalf("regressed queries: %v", err)
	}
	call := qsRec.last(t)
	want := []any{int64(10), qsFrom, qsTo, baseFrom, baseTo}
	if len(call.args) != len(want) {
		t.Fatalf("bound %d parameters (%v), want %d", len(call.args), call.args, len(want))
	}
	for i := range want {
		if call.args[i] != want[i] {
			t.Errorf("@p%d = %v, want %v", i+1, call.args[i], want[i])
		}
	}
	if !strings.Contains(call.sql, "rsi.start_time >= @p2 AND rsi.start_time < @p3") {
		t.Errorf("the reported window is not bound to @p2/@p3:\n%s", call.sql)
	}
	if !strings.Contains(call.sql, "rsi.start_time >= @p4 AND rsi.start_time < @p5") {
		t.Errorf("the baseline window is not bound to @p4/@p5:\n%s", call.sql)
	}
	if !strings.Contains(call.sql, "r.value - b.value") {
		t.Errorf("regression is not the reported value less the baseline:\n%s", call.sql)
	}
}

// TestQueryStorePlansFiltersTheWindowInTheJoinNotTheWhere pins the one
// structural decision in QueryStorePlansContext. Moving the interval
// predicate into the WHERE turns the LEFT JOIN back into an inner one, and
// the plan that did not run in the window — very often the good plan a user
// opened the report to force back — disappears from the list.
func TestQueryStorePlansFiltersTheWindowInTheJoinNotTheWhere(t *testing.T) {
	d := qsRecDB(t, 17, nil, nil)
	if _, err := d.QueryStorePlansContext(context.Background(), 42, QueryStoreReportOptions{
		Metric: QSMetricCPUTime, From: qsFrom, To: qsTo,
	}); err != nil {
		t.Fatalf("query store plans: %v", err)
	}
	call := qsRec.last(t)
	if !strings.Contains(call.sql, "LEFT JOIN sys.query_store_runtime_stats ") {
		t.Errorf("runtime stats are not left-joined:\n%s", call.sql)
	}
	where := call.sql[strings.Index(call.sql, "\nWHERE "):]
	if strings.Contains(where, "rsi.start_time") {
		t.Errorf("the interval window is in the WHERE clause, which undoes the LEFT JOIN:\n%s", where)
	}
	if !strings.Contains(call.sql, "WHERE  p.query_id = @p3") {
		t.Errorf("the query id is not bound as @p3:\n%s", call.sql)
	}
	if got, want := call.args, []any{qsFrom, qsTo, int64(42)}; len(got) != len(want) {
		t.Fatalf("bound %v, want %v", got, want)
	}
}

// TestQueryStoreForcedPlanQueriesFiltersInHaving pins the other structural
// decision. is_forced_plan belongs in a HAVING over the grouped query, not a
// WHERE over its rows: a query's non-forced plans still ran in the window,
// and excluding their rows would report the forced plan's cost as the whole
// query's.
func TestQueryStoreForcedPlanQueriesFiltersInHaving(t *testing.T) {
	d := qsRecDB(t, 17, nil, nil)
	if _, err := d.QueryStoreForcedPlanQueriesContext(context.Background(), QueryStoreReportOptions{
		Metric: QSMetricCPUTime, From: qsFrom, To: qsTo,
	}); err != nil {
		t.Fatalf("forced plan queries: %v", err)
	}
	call := qsRec.last(t)
	where := call.sql[strings.Index(call.sql, "\nWHERE "):strings.Index(call.sql, "\nGROUP BY")]
	if strings.Contains(where, "is_forced_plan") {
		t.Errorf("is_forced_plan is filtered in the WHERE, which drops the query's other plans:\n%s", where)
	}
	if !strings.Contains(call.sql, "HAVING MAX(CAST(p.is_forced_plan AS int)) = 1") {
		t.Errorf("the forced-plan filter is not a HAVING:\n%s", call.sql)
	}
}

// TestQueryStoreHighVariationRanksByCoefficientOfVariation pins that the
// High Variation report orders by stdev/avg rather than by stdev. Ordering by
// stdev alone makes the report a second copy of Top Resource Consuming
// Queries: the most expensive query has the largest absolute deviation
// whether or not it is unstable.
func TestQueryStoreHighVariationRanksByCoefficientOfVariation(t *testing.T) {
	d := qsRecDB(t, 17, nil, nil)
	if _, err := d.QueryStoreHighVariationQueriesContext(context.Background(), QueryStoreReportOptions{
		Metric: QSMetricCPUTime, Statistic: QSStatAvg, From: qsFrom, To: qsTo,
	}); err != nil {
		t.Fatalf("high variation queries: %v", err)
	}
	call := qsRec.last(t)
	if !strings.HasSuffix(strings.TrimSpace(call.sql), "ORDER BY variation DESC") {
		t.Errorf("high variation does not order by variation:\n%s", call.sql)
	}
	if !strings.Contains(call.sql, "SQRT(ABS(") {
		t.Errorf("the variation column is not built from the pooled standard deviation:\n%s", call.sql)
	}
	// Value must still be the statistic the caller asked for, not the stdev.
	if !strings.Contains(call.sql, "SUM(rs.avg_cpu_time * rs.count_executions) / NULLIF(SUM(rs.count_executions), 0) AS value") {
		t.Errorf("Value is not the requested statistic:\n%s", call.sql)
	}
}

// -- scanning ------------------------------------------------------------------

// TestQueryStoreQueryStatScanMapsEveryColumn drives one fully populated row
// through the scan and checks each field against a value unique to it. Twelve
// consecutive Scan destinations is exactly the shape where two adjacent
// columns swap unnoticed — both are numbers, both are plausible, and the
// report still renders.
func TestQueryStoreQueryStatScanMapsEveryColumn(t *testing.T) {
	last := time.Date(2026, 3, 4, 11, 30, 0, 0, time.UTC)
	cols := []string{"query_id", "query_sql_text", "object_name", "exec_count", "plan_count",
		"forced_plan_id", "last_execution_time", "value", "baseline_value",
		"baseline_exec_count", "regression", "variation"}
	row := []driver.Value{
		int64(11), "SELECT 1", "dbo.p", int64(22), int64(3),
		int64(44), last, 55.5, 66.5, int64(77), 88.5, 99.5,
	}
	d := qsRecDB(t, 17, cols, [][]driver.Value{row})

	stats, err := d.QueryStoreTopResourceQueriesContext(context.Background(),
		QueryStoreReportOptions{From: qsFrom, To: qsTo})
	if err != nil {
		t.Fatalf("top resource queries: %v", err)
	}
	if len(stats) != 1 {
		t.Fatalf("got %d rows, want 1", len(stats))
	}
	s := stats[0]
	checks := []struct {
		field string
		got   any
		want  any
	}{
		{"QueryID", s.QueryID, int64(11)},
		{"QueryText", s.QueryText, "SELECT 1"},
		{"ObjectName", s.ObjectName, "dbo.p"},
		{"ExecCount", s.ExecCount, int64(22)},
		{"PlanCount", s.PlanCount, 3},
		{"ForcedPlanID", s.ForcedPlanID, int64(44)},
		{"LastExecutionTime", s.LastExecutionTime, last},
		{"Value", s.Value, 55.5},
		{"BaselineValue", s.BaselineValue, 66.5},
		{"BaselineExecCount", s.BaselineExecCount, int64(77)},
		{"Regression", s.Regression, 88.5},
		{"Variation", s.Variation, 99.5},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.field, c.got, c.want)
		}
	}
}

// TestQueryStoreQueryStatScanTakesNullsAsZero covers the NULL a real report
// returns: MAX(CASE WHEN is_forced_plan ...) is NULL for a query with no
// forced plan, and every aggregate is NULL where NULLIF divided by a zero
// execution count. Scanning those into the plain numeric fields would fail
// the whole report rather than reporting a query with no forced plan.
func TestQueryStoreQueryStatScanTakesNullsAsZero(t *testing.T) {
	cols := []string{"query_id", "query_sql_text", "object_name", "exec_count", "plan_count",
		"forced_plan_id", "last_execution_time", "value", "baseline_value",
		"baseline_exec_count", "regression", "variation"}
	row := []driver.Value{int64(11), "SELECT 1", "", int64(0), int64(1),
		nil, nil, nil, nil, int64(0), nil, nil}
	d := qsRecDB(t, 17, cols, [][]driver.Value{row})

	stats, err := d.QueryStoreTopResourceQueriesContext(context.Background(),
		QueryStoreReportOptions{From: qsFrom, To: qsTo})
	if err != nil {
		t.Fatalf("a row with no forced plan and no executions failed to scan: %v", err)
	}
	if len(stats) != 1 {
		t.Fatalf("got %d rows, want 1", len(stats))
	}
	if stats[0].ForcedPlanID != 0 {
		t.Errorf("ForcedPlanID = %d, want 0 for a query with no forced plan", stats[0].ForcedPlanID)
	}
	if !stats[0].LastExecutionTime.IsZero() {
		t.Errorf("LastExecutionTime = %v, want the zero time", stats[0].LastExecutionTime)
	}
	if stats[0].Value != 0 {
		t.Errorf("Value = %v, want 0", stats[0].Value)
	}
}

// -- plan forcing ---------------------------------------------------------------

// TestQueryStoreForcePlanScriptsTheProcedureAndItsArguments pins both
// statements through WithScript, which substitutes the bound parameters into
// the text — so this checks the argument *values* and their order, not just
// the procedure name. Passing the two ids the wrong way round produces a
// statement that runs and forces nothing recognisable.
func TestQueryStoreForcePlanScriptsTheProcedureAndItsArguments(t *testing.T) {
	tests := []struct {
		name string
		call func(*Database, context.Context) error
		want string
	}{
		{"force", func(d *Database, ctx context.Context) error {
			return d.QueryStoreForcePlanContext(ctx, 42, 7)
		}, "EXEC sys.sp_query_store_force_plan @query_id = 42, @plan_id = 7"},
		{"unforce", func(d *Database, ctx context.Context) error {
			return d.QueryStoreUnforcePlanContext(ctx, 42, 7)
		}, "EXEC sys.sp_query_store_unforce_plan @query_id = 42, @plan_id = 7"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := &Database{name: "app db", server: &Server{}}
			ctx, script := WithScript(context.Background())
			if err := tt.call(d, ctx); err != nil {
				t.Fatalf("%s: %v", tt.name, err)
			}
			got := strings.Join(script.Statements, "\n")
			if !strings.Contains(got, tt.want) {
				t.Errorf("scripted:\n%s\nwant it to contain:\n%s", got, tt.want)
			}
			// The script may be pasted into a session pointed anywhere, so it
			// has to name the database itself — and bracket-quote a name with
			// a space in it.
			if !strings.Contains(got, "USE [app db]") {
				t.Errorf("scripted statement does not USE the database it applies to:\n%s", got)
			}
		})
	}
}

// TestQueryStoreWaitReportsAreVersionGated checks that both wait reports
// refuse a pre-2017 instance rather than issuing a query against a view that
// does not exist there — the difference between a caller being able to hide
// the report and a user meeting "Invalid object name
// sys.query_store_wait_stats".
func TestQueryStoreWaitReportsAreVersionGated(t *testing.T) {
	old := qsRecDB(t, int(SQLServer2016), nil, nil)
	if old.QueryStoreWaitStatsSupported() {
		t.Error("wait statistics reported as supported on SQL Server 2016")
	}
	if _, err := old.QueryStoreWaitCategoriesContext(context.Background(), QueryStoreReportOptions{}); err == nil {
		t.Error("wait categories ran against SQL Server 2016")
	}
	if _, err := old.QueryStoreWaitingQueriesContext(context.Background(), "CPU", QueryStoreReportOptions{}); err == nil {
		t.Error("waiting queries ran against SQL Server 2016")
	}
	if len(qsRec.calls) != 0 {
		t.Errorf("a gated report still sent %d statements to the server", len(qsRec.calls))
	}

	newer := qsRecDB(t, int(SQLServer2017), nil, nil)
	if !newer.QueryStoreWaitStatsSupported() {
		t.Error("wait statistics reported as unsupported on SQL Server 2017")
	}
	if _, err := newer.QueryStoreWaitCategoriesContext(context.Background(), QueryStoreReportOptions{}); err != nil {
		t.Errorf("wait categories on SQL Server 2017: %v", err)
	}
}

// TestQueryStoreWaitingQueriesFiltersByCategoryOnlyWhenGivenOne pins the
// empty-category case as "every category" rather than "a category named the
// empty string", which would silently return nothing.
func TestQueryStoreWaitingQueriesFiltersByCategoryOnlyWhenGivenOne(t *testing.T) {
	d := qsRecDB(t, 17, nil, nil)
	if _, err := d.QueryStoreWaitingQueriesContext(context.Background(), "CPU",
		QueryStoreReportOptions{From: qsFrom, To: qsTo, Top: 10}); err != nil {
		t.Fatalf("waiting queries: %v", err)
	}
	call := qsRec.last(t)
	if !strings.Contains(call.sql, "ws.wait_category_desc = @p4") {
		t.Errorf("the category is not bound as a parameter:\n%s", call.sql)
	}
	if got, want := call.args[3], any("CPU"); got != want {
		t.Errorf("@p4 = %v, want %v", got, want)
	}

	d = qsRecDB(t, 17, nil, nil)
	if _, err := d.QueryStoreWaitingQueriesContext(context.Background(), "",
		QueryStoreReportOptions{From: qsFrom, To: qsTo, Top: 10}); err != nil {
		t.Fatalf("waiting queries across every category: %v", err)
	}
	if call := qsRec.last(t); strings.Contains(call.sql, "wait_category_desc =") {
		t.Errorf("an empty category still produced a category predicate:\n%s", call.sql)
	}
}

// TestQueryStoreReportsExcludeInternalQueriesUnlessAsked pins the default.
// Query Store records the engine's own statistics-maintenance queries, and
// leaving them in puts them at the top of a Top Resource Consuming report on
// an otherwise quiet database.
func TestQueryStoreReportsExcludeInternalQueriesUnlessAsked(t *testing.T) {
	d := qsRecDB(t, 17, nil, nil)
	if _, err := d.QueryStoreTopResourceQueriesContext(context.Background(),
		QueryStoreReportOptions{From: qsFrom, To: qsTo}); err != nil {
		t.Fatalf("top resource queries: %v", err)
	}
	if !strings.Contains(qsRec.last(t).sql, "q.is_internal_query = 0") {
		t.Error("internal queries are not excluded by default")
	}

	d = qsRecDB(t, 17, nil, nil)
	if _, err := d.QueryStoreTopResourceQueriesContext(context.Background(),
		QueryStoreReportOptions{From: qsFrom, To: qsTo, IncludeInternal: true}); err != nil {
		t.Fatalf("top resource queries: %v", err)
	}
	if strings.Contains(qsRec.last(t).sql, "is_internal_query") {
		t.Error("IncludeInternal still filtered internal queries out")
	}
}
