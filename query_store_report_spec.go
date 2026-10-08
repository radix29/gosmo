package gosmo

import (
	"fmt"
	"strings"
	"time"
)

// ============================================================
// Query Store report vocabulary: the metrics, statistics and
// options a report takes, and the SQL fragments the reports in
// query_store_reports.go are assembled from
// ============================================================

// QSUnit is what a metric's values are measured in. Query Store reports raw
// engine units, not display ones: durations are microseconds, I/O and memory
// are 8-KB pages. A caller formatting a value needs to know which.
type QSUnit int

const (
	QSUnitCount        QSUnit = iota // dimensionless (DOP, row count)
	QSUnitMicroseconds               // duration, CPU time, CLR time
	QSUnitPages                      // 8-KB pages
	QSUnitBytes
	QSUnitMilliseconds // wait times, which Query Store reports in ms
)

// QSMetric names one resource dimension a Query Store report can rank by —
// the "Metric" selector in SSMS's Query Store views.
type QSMetric string

const (
	QSMetricDuration      QSMetric = "Duration"
	QSMetricCPUTime       QSMetric = "CPU time"
	QSMetricLogicalReads  QSMetric = "Logical reads"
	QSMetricLogicalWrites QSMetric = "Logical writes"
	QSMetricPhysicalReads QSMetric = "Physical reads"
	QSMetricCLRTime       QSMetric = "CLR time"
	QSMetricDOP           QSMetric = "DOP"
	QSMetricMemory        QSMetric = "Memory consumption"
	QSMetricRowCount      QSMetric = "Row count"
	QSMetricLogMemory     QSMetric = "Log memory used"
	QSMetricTempDBMemory  QSMetric = "Tempdb memory used"
)

// qsMetricDef ties a metric to the sys.query_store_runtime_stats column
// family it aggregates. column is the stem the avg_/min_/max_/stdev_ prefixes
// attach to, so one entry describes all four columns at once.
//
// This is deliberately one table rather than a metric list beside a column
// list: a pair of parallel slices cancels its own faults out, and a metric
// silently reading another metric's column is invisible in any round-trip
// test. minVersion is the first release carrying the column — below it the
// metric is not offered rather than producing "Invalid column name".
type qsMetricDef struct {
	Metric     QSMetric
	column     string
	unit       QSUnit
	minVersion ServerVersion
}

// qsMetricDefs is every metric, in the order SSMS lists them. The slice is
// the display order as well as the lookup table, so the two cannot diverge.
var qsMetricDefs = []qsMetricDef{
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

// qsMetric looks a metric up in qsMetricDefs.
func qsMetric(m QSMetric) (qsMetricDef, bool) {
	for _, d := range qsMetricDefs {
		if d.Metric == m {
			return d, true
		}
	}
	return qsMetricDef{}, false
}

// QueryStoreMetrics returns every metric a Query Store report can rank by, in SSMS's
// display order. Metrics the instance is too old to have are left out, so a
// caller building a selector from this never offers one that cannot be read.
func (d *Database) QueryStoreMetrics() []QSMetric {
	major := d.serverMajorVersion()
	out := make([]QSMetric, 0, len(qsMetricDefs))
	for _, def := range qsMetricDefs {
		// major == 0 means the version was never read (a Server built without
		// loadInfo); offer everything rather than nothing, and let the query
		// itself be the authority.
		if major == 0 || major >= int(def.minVersion) {
			out = append(out, def.Metric)
		}
	}
	return out
}

// QSMetricUnit reports the unit a metric's values carry, and whether the
// metric is one this library knows at all.
func QSMetricUnit(m QSMetric) (QSUnit, bool) {
	def, ok := qsMetric(m)
	return def.unit, ok
}

// QSStatistic is how a metric is aggregated across the intervals in a
// report's time range — the "Statistic" selector in SSMS's Query Store views.
type QSStatistic string

const (
	QSStatAvg    QSStatistic = "Avg"
	QSStatMin    QSStatistic = "Min"
	QSStatMax    QSStatistic = "Max"
	QSStatTotal  QSStatistic = "Total"
	QSStatStdDev QSStatistic = "Std dev"
)

// qsStatisticDef ties a statistic to the SQL aggregate that computes it.
// runtime renders the aggregate over a sys.query_store_runtime_stats alias
// and a metric column stem; wait renders it over sys.query_store_wait_stats,
// whose columns are named differently and which has no execution count of its
// own (see QueryStoreWaitCategories).
//
// Both are functions rather than format strings so the substitution order is
// the compiler's problem, not a reviewer's.
//
// Every expression either reads a float column or CASTs to float before it
// divides. The wait columns are the ones that matter: total_/min_/max_
// query_wait_time_ms and count_executions are all bigint, so an expression
// that divides two of them truncates silently rather than failing. See
// TestQSWaitAveragesDoNotTruncateToWholeMilliseconds.
type qsStatisticDef struct {
	Statistic QSStatistic
	runtime   func(rs, stem string) string
	wait      func(ws, rs string) string
}

// qsStatisticDefs is every statistic, in SSMS's display order.
//
// The averages are execution-weighted rather than an AVG of the per-interval
// averages: an interval where the query ran once must not count as much as
// one where it ran ten thousand times. Every "total" is likewise
// SUM(avg * count_executions), because Query Store stores no total column —
// only the per-interval average and the count behind it.
var qsStatisticDefs = []qsStatisticDef{
	{QSStatAvg,
		func(rs, c string) string {
			return fmt.Sprintf("SUM(%[1]s.avg_%[2]s * %[1]s.count_executions) / NULLIF(SUM(%[1]s.count_executions), 0)", rs, c)
		},
		func(ws, rs string) string {
			// CAST before the division, not after: total_query_wait_time_ms
			// and count_executions are both bigint, so an uncast quotient is
			// integer division. Every category averaging under a millisecond
			// per execution then reports 0 — and ORDER BY value DESC ties all
			// of them, so the ranking the report exists for is arbitrary.
			return fmt.Sprintf("CAST(SUM(%s.total_query_wait_time_ms) AS float) / NULLIF(SUM(%s.count_executions), 0)", ws, rs)
		}},
	{QSStatMin,
		func(rs, c string) string { return fmt.Sprintf("CAST(MIN(%s.min_%s) AS float)", rs, c) },
		func(ws, _ string) string {
			return fmt.Sprintf("CAST(MIN(%s.min_query_wait_time_ms) AS float)", ws)
		}},
	{QSStatMax,
		func(rs, c string) string { return fmt.Sprintf("CAST(MAX(%s.max_%s) AS float)", rs, c) },
		func(ws, _ string) string {
			return fmt.Sprintf("CAST(MAX(%s.max_query_wait_time_ms) AS float)", ws)
		}},
	{QSStatTotal,
		func(rs, c string) string {
			return fmt.Sprintf("SUM(%[1]s.avg_%[2]s * %[1]s.count_executions)", rs, c)
		},
		func(ws, _ string) string { return fmt.Sprintf("CAST(SUM(%s.total_query_wait_time_ms) AS float)", ws) }},
	{QSStatStdDev,
		func(rs, c string) string {
			return qsPooledStdev(fmt.Sprintf("%s.stdev_%s", rs, c), fmt.Sprintf("%s.avg_%s", rs, c), rs+".count_executions")
		},
		func(ws, rs string) string {
			return qsPooledStdev(ws+".stdev_query_wait_time_ms", ws+".avg_query_wait_time_ms", rs+".count_executions")
		}},
}

// qsStatistic looks a statistic up in qsStatisticDefs.
func qsStatistic(s QSStatistic) (qsStatisticDef, bool) {
	for _, d := range qsStatisticDefs {
		if d.Statistic == s {
			return d, true
		}
	}
	return qsStatisticDef{}, false
}

// QSStatistics returns every statistic a Query Store report can aggregate
// with, in SSMS's display order.
func QSStatistics() []QSStatistic {
	out := make([]QSStatistic, 0, len(qsStatisticDefs))
	for _, d := range qsStatisticDefs {
		out = append(out, d.Statistic)
	}
	return out
}

// qsPooledStdev renders the standard deviation of a metric pooled across
// every interval in range, from each interval's own stdev, mean and
// execution count: sqrt(E[X²] - E[X]²), with both expectations weighted by
// the execution count.
//
// A plain AVG(stdev_x) would be wrong twice over — it ignores how many
// executions each interval's deviation was measured over, and it ignores the
// variance *between* intervals, which is the whole point of the High
// Variation report.
//
// ABS is not papering over a sign error: the quantity under the root is a
// variance and cannot be negative mathematically, so a negative here is float
// rounding at the 1e-10 scale, and SQRT of it would fail the batch outright.
func qsPooledStdev(stdevCol, avgCol, countCol string) string {
	return fmt.Sprintf("SQRT(ABS("+
		"SUM((POWER(%[1]s, 2) + POWER(%[2]s, 2)) * %[3]s) / NULLIF(SUM(%[3]s), 0)"+
		" - POWER(SUM(%[2]s * %[3]s) / NULLIF(SUM(%[3]s), 0), 2)))",
		stdevCol, avgCol, countCol)
}

// QueryStoreReportOptions selects what one report covers and how it ranks.
// The zero value is usable: it reports the last hour by average duration,
// which is what SSMS's views open on.
type QueryStoreReportOptions struct {
	// Metric and Statistic pick the value every row is ranked by. Empty means
	// QSMetricDuration and QSStatAvg.
	Metric    QSMetric
	Statistic QSStatistic

	// From and To bound the report by runtime-stats interval, half-open
	// [From, To). A zero To means now; a zero From means an hour before To.
	From, To time.Time

	// BaselineFrom and BaselineTo bound the comparison window
	// QueryStoreRegressedQueries measures regression against, and are
	// ignored by every other report. Zero means the window of the same length
	// immediately before From.
	BaselineFrom, BaselineTo time.Time

	// Top caps how many rows come back. Zero means QSDefaultTop.
	Top int

	// MinExecCount drops queries that ran fewer than this many times in the
	// window — the noise floor for a regression or variation report, where one
	// execution has no meaningful average. Zero keeps everything.
	MinExecCount int64

	// QueryIDs restricts a per-query report to these queries, in place of
	// ranking the whole database — what the Tracked Queries view reads, where
	// the caller already knows which queries it is following. Empty means
	// every query. Honoured by the four per-query reports; ignored by the
	// reports whose rows are not queries (QueryStoreOverallConsumption,
	// QueryStoreWaitCategories) and by QueryStorePlans, which
	// names the one query it is about.
	//
	// Top still applies: a caller asking for more ids than Top gets the
	// costliest of them, so pass Top alongside a long list.
	QueryIDs []int64

	// MinRegressionPct drops queries whose metric has grown by less than this
	// percentage of its baseline value — the threshold SSMS's Regressed
	// Queries view offers, and ignored by every other report. Zero keeps
	// everything.
	//
	// A percentage rather than an absolute amount, because the same report is
	// read under eleven metrics measured in four different units: a threshold
	// of "100" would mean 100 microseconds under Duration and 100 8-KB pages
	// under Logical reads. A query whose baseline value is zero is dropped
	// when a threshold is set — growth from nothing has no percentage.
	MinRegressionPct float64

	// IncludeInternal keeps queries Query Store flags as internal (statistics
	// updates and the like). They are excluded by default, as SSMS excludes
	// them.
	IncludeInternal bool
}

// QSDefaultTop is how many rows a report returns when Options.Top is zero.
const QSDefaultTop = 25

// qsReportSpec is a validated QueryStoreReportOptions: every free-text field
// resolved to the table entry it names, every zero field filled in. Building
// one is the single point where a bad metric or statistic is rejected, so no
// query is ever assembled from an unrecognised name.
type qsReportSpec struct {
	metric qsMetricDef
	stat   qsStatisticDef
	opts   QueryStoreReportOptions
}

// resolve validates o and fills in its defaults.
func (o QueryStoreReportOptions) resolve() (qsReportSpec, error) {
	if o.Metric == "" {
		o.Metric = QSMetricDuration
	}
	if o.Statistic == "" {
		o.Statistic = QSStatAvg
	}
	m, ok := qsMetric(o.Metric)
	if !ok {
		return qsReportSpec{}, invalidf("gosmo: query store report: unknown metric %q", o.Metric)
	}
	s, ok := qsStatistic(o.Statistic)
	if !ok {
		return qsReportSpec{}, invalidf("gosmo: query store report: unknown statistic %q", o.Statistic)
	}
	if o.To.IsZero() {
		o.To = time.Now()
	}
	if o.From.IsZero() {
		o.From = o.To.Add(-time.Hour)
	}
	if o.BaselineTo.IsZero() {
		o.BaselineTo = o.From
	}
	if o.BaselineFrom.IsZero() {
		o.BaselineFrom = o.BaselineTo.Add(-o.To.Sub(o.From))
	}
	if o.Top <= 0 {
		o.Top = QSDefaultTop
	}
	return qsReportSpec{metric: m, stat: s, opts: o}, nil
}

// value renders the ranked expression over the runtime-stats alias rs.
func (sp qsReportSpec) value(rs string) string { return sp.stat.runtime(rs, sp.metric.column) }

// qsArgs accumulates positional query parameters. Queries here are built by
// concatenation, so the parameter numbers have to follow the text rather than
// be written into it by hand.
type qsArgs struct{ args []any }

func (a *qsArgs) add(v any) string {
	a.args = append(a.args, v)
	return fmt.Sprintf("@p%d", len(a.args))
}

// qsRuntimeFrom is the join every runtime-stats report reads: one row per
// plan per interval, carrying its query and that query's text.
const qsRuntimeFrom = `
FROM   sys.query_store_query                  AS q
JOIN   sys.query_store_query_text             AS qt  ON qt.query_text_id = q.query_text_id
JOIN   sys.query_store_plan                   AS p   ON p.query_id = q.query_id
JOIN   sys.query_store_runtime_stats          AS rs  ON rs.plan_id = p.plan_id
JOIN   sys.query_store_runtime_stats_interval AS rsi ON rsi.runtime_stats_interval_id = rs.runtime_stats_interval_id`

// qsObjectName renders the schema-qualified name of the module a query came
// from, empty for an ad-hoc batch.
const qsObjectName = `COALESCE(OBJECT_SCHEMA_NAME(q.object_id) + '.' + OBJECT_NAME(q.object_id), '')`

// qsQueryGroupBy groups a runtime-stats report to one row per query.
const qsQueryGroupBy = `GROUP BY q.query_id, qt.query_sql_text, q.object_id`

// window renders the interval-range and internal-query predicates over the
// half-open range [from, to), as a WHERE clause.
func (sp qsReportSpec) window(a *qsArgs, from, to time.Time) string {
	w := fmt.Sprintf("WHERE rsi.start_time >= %s AND rsi.start_time < %s", a.add(from), a.add(to))
	if !sp.opts.IncludeInternal {
		w += "\n  AND q.is_internal_query = 0"
	}
	return w
}

// queryFilter renders the Options.QueryIDs restriction as further predicates
// for the window's WHERE, or nothing when there is no list. Appended by the
// per-query reports only: QueryStorePlans names its own query, and
// adding a second id predicate there would answer nothing for any query the
// caller had not also listed.
func (sp qsReportSpec) queryFilter(a *qsArgs) string {
	if len(sp.opts.QueryIDs) == 0 {
		return ""
	}
	ids := make([]string, len(sp.opts.QueryIDs))
	for i, id := range sp.opts.QueryIDs {
		// Bound, not interpolated: these ids reach the library from wherever
		// the caller persisted them, and a report is not the place to find out
		// that the file was editable.
		ids[i] = a.add(id)
	}
	return "\n  AND q.query_id IN (" + strings.Join(ids, ", ") + ")"
}

// having renders the execution-count floor, or nothing when there is none.
func (sp qsReportSpec) having(a *qsArgs) string {
	if sp.opts.MinExecCount <= 0 {
		return ""
	}
	return "\nHAVING SUM(rs.count_executions) >= " + a.add(sp.opts.MinExecCount)
}

// regressionFloor renders the regression threshold as a WHERE clause over the
// two windows the Regressed Queries report joins, or nothing when there is no
// threshold.
//
// The comparison is written as a multiplication rather than a division so a
// zero baseline cannot divide: b.value > 0 already excludes it, but SQL Server
// is free to evaluate the two predicates in either order.
func (sp qsReportSpec) regressionFloor(a *qsArgs) string {
	if sp.opts.MinRegressionPct <= 0 {
		return ""
	}
	return "\nWHERE  b.value > 0 AND (r.value - b.value) >= b.value * " +
		a.add(sp.opts.MinRegressionPct) + " / 100"
}
