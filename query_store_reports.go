package gosmo

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ============================================================
// Query Store reports (SSMS's seven Query Store views, and the
// plan forcing behind Force/Unforce Plan)
// ============================================================
//
// Everything here reads the sys.query_store_* catalog views, which are
// database-scoped: each call goes through Database.query, which pins a
// connection and USEs d before running. A database with Query Store turned
// off is not an error — the views exist and are empty, so every report
// returns no rows.

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
// own (see QueryStoreWaitCategoriesContext).
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
	// QueryStoreRegressedQueriesContext measures regression against, and are
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
	// reports whose rows are not queries (QueryStoreOverallConsumptionContext,
	// QueryStoreWaitCategoriesContext) and by QueryStorePlansContext, which
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
		return qsReportSpec{}, fmt.Errorf("gosmo: query store report: unknown metric %q", o.Metric)
	}
	s, ok := qsStatistic(o.Statistic)
	if !ok {
		return qsReportSpec{}, fmt.Errorf("gosmo: query store report: unknown statistic %q", o.Statistic)
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
// per-query reports only: QueryStorePlansContext names its own query, and
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

// QSQueryStat is one query's line in a Query Store report.
//
// Which fields carry a value depends on the report: BaselineValue and
// BaselineExecCount are populated only by QueryStoreRegressedQueriesContext,
// and Variation only by QueryStoreHighVariationQueriesContext. Both are zero
// elsewhere.
type QSQueryStat struct {
	QueryID    int64
	QueryText  string
	ObjectName string // schema-qualified module the query is in, empty if ad hoc

	ExecCount int64
	PlanCount int
	// ForcedPlanID is the plan forced for this query, or 0 if none is.
	ForcedPlanID      int64
	LastExecutionTime time.Time

	// Value is the metric under the report's statistic, in the metric's unit.
	Value float64

	// BaselineValue is Value over the report's baseline window, and Regression
	// the amount Value has grown since — the ranking of the Regressed Queries
	// report.
	BaselineValue     float64
	BaselineExecCount int64
	Regression        float64

	// Variation is the metric's coefficient of variation, stdev/avg — the
	// ranking of the High Variation report. Dimensionless, so it compares
	// across queries of very different absolute cost.
	Variation float64
}

// QSIntervalStat is one runtime-stats interval's line in the Overall Resource
// Consumption report.
type QSIntervalStat struct {
	StartTime, EndTime time.Time
	ExecCount          int64
	Value              float64
}

// QSPlanIntervalStat is one plan's value in one interval — the per-plan time
// series behind the Tracked Queries report.
type QSPlanIntervalStat struct {
	PlanID             int64
	StartTime, EndTime time.Time
	ExecCount          int64
	Value              float64
}

// QSWaitStat is one wait category's line in the Query Wait Statistics report.
type QSWaitStat struct {
	Category  string
	ExecCount int64
	Value     float64 // milliseconds
}

// QSPlan is one plan of one query, with the plan XML SSMS renders below its
// Query Store views.
type QSPlan struct {
	PlanID  int64
	QueryID int64

	IsForced               bool
	ForcingType            string
	ForceFailureCount      int64
	LastForceFailureReason string

	CompatibilityLevel int
	IsTrivialPlan      bool
	IsParallelPlan     bool

	LastCompileStartTime time.Time
	LastExecutionTime    time.Time

	QueryPlanXML string

	// ExecCount and Value cover the report's window, so the caller can rank a
	// query's plans the same way its queries were ranked.
	ExecCount int64
	Value     float64
}

// scanQueryStats reads the column list every per-query report selects.
func scanQueryStats(rows *dbRows) ([]*QSQueryStat, error) {
	var out []*QSQueryStat
	for rows.Next() {
		st := &QSQueryStat{}
		var value, baseline, regression, variation sql.NullFloat64
		var last sql.NullTime
		var forced sql.NullInt64
		if err := rows.Scan(&st.QueryID, &st.QueryText, &st.ObjectName, &st.ExecCount,
			&st.PlanCount, &forced, &last, &value, &baseline, &st.BaselineExecCount,
			&regression, &variation); err != nil {
			return nil, err
		}
		st.ForcedPlanID = forced.Int64
		st.LastExecutionTime = last.Time
		st.Value = value.Float64
		st.BaselineValue = baseline.Float64
		st.Regression = regression.Float64
		st.Variation = variation.Float64
		out = append(out, st)
	}
	return out, rows.Err()
}

// qsQueryColumns is the per-query report column list, in scanQueryStats's
// order. Only value and variation vary between reports; the baseline and
// regression columns are zero here because the one report that fills them —
// Regressed Queries — reads two windows and builds its own column list.
func qsQueryColumns(value, variation string) string {
	return fmt.Sprintf(`q.query_id,
       qt.query_sql_text,
       %s AS object_name,
       SUM(rs.count_executions)                                   AS exec_count,
       COUNT(DISTINCT p.plan_id)                                  AS plan_count,
       MAX(CASE WHEN p.is_forced_plan = 1 THEN p.plan_id END)     AS forced_plan_id,
       MAX(rs.last_execution_time)                                AS last_execution_time,
       %s AS value,
       CAST(0 AS float) AS baseline_value,
       0                AS baseline_exec_count,
       CAST(0 AS float) AS regression,
       %s AS variation`, qsObjectName, value, variation)
}

// QueryStoreTopResourceQueries ranks the database's queries by one metric —
// SSMS's Top Resource Consuming Queries view.
func (d *Database) QueryStoreTopResourceQueries(opts QueryStoreReportOptions) ([]*QSQueryStat, error) {
	return d.QueryStoreTopResourceQueriesContext(context.Background(), opts)
}

// QueryStoreTopResourceQueriesContext is the context-aware variant of
// QueryStoreTopResourceQueries.
func (d *Database) QueryStoreTopResourceQueriesContext(ctx context.Context, opts QueryStoreReportOptions) ([]*QSQueryStat, error) {
	sp, err := opts.resolve()
	if err != nil {
		return nil, err
	}
	var a qsArgs
	top := a.add(sp.opts.Top)
	q := "SELECT TOP (" + top + ") " + qsQueryColumns(sp.value("rs"), "CAST(0 AS float)") +
		qsRuntimeFrom + "\n" + sp.window(&a, sp.opts.From, sp.opts.To) + sp.queryFilter(&a) + "\n" + qsQueryGroupBy + sp.having(&a) +
		"\nORDER BY value DESC"

	rows, err := d.query(ctx, q, a.args...)
	if err != nil {
		return nil, fmt.Errorf("gosmo: top resource queries in %q: %w", d.name, err)
	}
	defer rows.Close()
	stats, err := scanQueryStats(rows)
	if err != nil {
		return nil, fmt.Errorf("gosmo: top resource queries in %q: %w", d.name, err)
	}
	return stats, nil
}

// QueryStoreForcedPlanQueries lists the queries that have a forced plan —
// SSMS's Queries With Forced Plans view.
func (d *Database) QueryStoreForcedPlanQueries(opts QueryStoreReportOptions) ([]*QSQueryStat, error) {
	return d.QueryStoreForcedPlanQueriesContext(context.Background(), opts)
}

// QueryStoreForcedPlanQueriesContext is the context-aware variant of
// QueryStoreForcedPlanQueries.
//
// The forced-plan predicate is applied as a HAVING over the whole query
// rather than a WHERE on the plan: a query's *other* plans still have runtime
// stats in the window, and filtering them out at the row level would report
// the forced plan's cost as the query's total.
func (d *Database) QueryStoreForcedPlanQueriesContext(ctx context.Context, opts QueryStoreReportOptions) ([]*QSQueryStat, error) {
	sp, err := opts.resolve()
	if err != nil {
		return nil, err
	}
	var a qsArgs
	top := a.add(sp.opts.Top)
	having := sp.having(&a)
	if having == "" {
		having = "\nHAVING MAX(CAST(p.is_forced_plan AS int)) = 1"
	} else {
		having += "\n   AND MAX(CAST(p.is_forced_plan AS int)) = 1"
	}
	q := "SELECT TOP (" + top + ") " + qsQueryColumns(sp.value("rs"), "CAST(0 AS float)") +
		qsRuntimeFrom + "\n" + sp.window(&a, sp.opts.From, sp.opts.To) + sp.queryFilter(&a) + "\n" + qsQueryGroupBy + having +
		"\nORDER BY value DESC"

	rows, err := d.query(ctx, q, a.args...)
	if err != nil {
		return nil, fmt.Errorf("gosmo: queries with forced plans in %q: %w", d.name, err)
	}
	defer rows.Close()
	stats, err := scanQueryStats(rows)
	if err != nil {
		return nil, fmt.Errorf("gosmo: queries with forced plans in %q: %w", d.name, err)
	}
	return stats, nil
}

// QueryStoreHighVariationQueries ranks queries by how unstable one metric is
// — SSMS's Queries With High Variation view.
func (d *Database) QueryStoreHighVariationQueries(opts QueryStoreReportOptions) ([]*QSQueryStat, error) {
	return d.QueryStoreHighVariationQueriesContext(context.Background(), opts)
}

// QueryStoreHighVariationQueriesContext is the context-aware variant of
// QueryStoreHighVariationQueries.
//
// Ranking is by coefficient of variation (stdev/avg), not by stdev: the most
// expensive query in the database otherwise tops a variation report merely
// for being expensive. Value still carries the statistic the caller asked
// for, so the report can show the cost beside the instability.
func (d *Database) QueryStoreHighVariationQueriesContext(ctx context.Context, opts QueryStoreReportOptions) ([]*QSQueryStat, error) {
	sp, err := opts.resolve()
	if err != nil {
		return nil, err
	}
	stdev, _ := qsStatistic(QSStatStdDev)
	avg, _ := qsStatistic(QSStatAvg)
	variation := fmt.Sprintf("(%s) / NULLIF(%s, 0)",
		stdev.runtime("rs", sp.metric.column), avg.runtime("rs", sp.metric.column))

	var a qsArgs
	top := a.add(sp.opts.Top)
	q := "SELECT TOP (" + top + ") " + qsQueryColumns(sp.value("rs"), variation) +
		qsRuntimeFrom + "\n" + sp.window(&a, sp.opts.From, sp.opts.To) + sp.queryFilter(&a) + "\n" + qsQueryGroupBy + sp.having(&a) +
		"\nORDER BY variation DESC"

	rows, err := d.query(ctx, q, a.args...)
	if err != nil {
		return nil, fmt.Errorf("gosmo: high variation queries in %q: %w", d.name, err)
	}
	defer rows.Close()
	stats, err := scanQueryStats(rows)
	if err != nil {
		return nil, fmt.Errorf("gosmo: high variation queries in %q: %w", d.name, err)
	}
	return stats, nil
}

// QueryStoreRegressedQueries ranks queries by how much one metric has grown
// between two windows — SSMS's Regressed Queries view.
func (d *Database) QueryStoreRegressedQueries(opts QueryStoreReportOptions) ([]*QSQueryStat, error) {
	return d.QueryStoreRegressedQueriesContext(context.Background(), opts)
}

// QueryStoreRegressedQueriesContext is the context-aware variant of
// QueryStoreRegressedQueries. It compares [From, To) against
// [BaselineFrom, BaselineTo), which default to the equally long window
// immediately before From.
//
// The join between the two windows is an inner one on purpose: a query with
// no executions in the baseline window has not regressed, it is new, and
// ranking it by "growth from zero" would fill the report with first-time
// queries and hide the actual regressions.
func (d *Database) QueryStoreRegressedQueriesContext(ctx context.Context, opts QueryStoreReportOptions) ([]*QSQueryStat, error) {
	sp, err := opts.resolve()
	if err != nil {
		return nil, err
	}
	var a qsArgs
	top := a.add(sp.opts.Top)

	// Both windows aggregate identically; only the interval predicate differs.
	windowCTE := func(name string, from, to time.Time) string {
		return fmt.Sprintf(`%s AS (
    SELECT q.query_id,
           qt.query_sql_text                                       AS query_sql_text,
           %s                                                      AS object_name,
           SUM(rs.count_executions)                                AS exec_count,
           COUNT(DISTINCT p.plan_id)                               AS plan_count,
           MAX(CASE WHEN p.is_forced_plan = 1 THEN p.plan_id END)  AS forced_plan_id,
           MAX(rs.last_execution_time)                             AS last_execution_time,
           %s                                                      AS value
    %s
    %s
    %s%s
)`, name, qsObjectName, sp.value("rs"), strings.TrimPrefix(qsRuntimeFrom, "\n"),
			sp.window(&a, from, to)+sp.queryFilter(&a), qsQueryGroupBy, sp.having(&a))
	}

	// The CTEs are rendered in parameter order — recent first, then baseline —
	// because qsArgs numbers each parameter as it is appended.
	recent := windowCTE("recent", sp.opts.From, sp.opts.To)
	baseline := windowCTE("baseline", sp.opts.BaselineFrom, sp.opts.BaselineTo)

	q := fmt.Sprintf(`WITH %s,
%s
SELECT TOP (%s)
       r.query_id,
       r.query_sql_text,
       r.object_name,
       r.exec_count,
       r.plan_count,
       r.forced_plan_id,
       r.last_execution_time,
       r.value,
       b.value                AS baseline_value,
       b.exec_count           AS baseline_exec_count,
       r.value - b.value      AS regression,
       CAST(0 AS float)       AS variation
FROM   recent   AS r
JOIN   baseline AS b ON b.query_id = r.query_id%s
ORDER BY regression DESC`, recent, baseline, top, sp.regressionFloor(&a))

	rows, err := d.query(ctx, q, a.args...)
	if err != nil {
		return nil, fmt.Errorf("gosmo: regressed queries in %q: %w", d.name, err)
	}
	defer rows.Close()
	stats, err := scanQueryStats(rows)
	if err != nil {
		return nil, fmt.Errorf("gosmo: regressed queries in %q: %w", d.name, err)
	}
	return stats, nil
}

// QueryStoreOverallConsumption totals one metric per runtime-stats interval
// across the whole database — SSMS's Overall Resource Consumption view.
func (d *Database) QueryStoreOverallConsumption(opts QueryStoreReportOptions) ([]*QSIntervalStat, error) {
	return d.QueryStoreOverallConsumptionContext(context.Background(), opts)
}

// QueryStoreOverallConsumptionContext is the context-aware variant of
// QueryStoreOverallConsumption. Rows come back oldest first, ready to plot,
// and Options.Top does not apply — the caller asked for a time range, and
// dropping intervals out of the middle of it would misdraw the chart.
func (d *Database) QueryStoreOverallConsumptionContext(ctx context.Context, opts QueryStoreReportOptions) ([]*QSIntervalStat, error) {
	sp, err := opts.resolve()
	if err != nil {
		return nil, err
	}
	var a qsArgs
	q := fmt.Sprintf(`SELECT rsi.start_time,
       rsi.end_time,
       SUM(rs.count_executions) AS exec_count,
       %s AS value
%s
%s
GROUP BY rsi.runtime_stats_interval_id, rsi.start_time, rsi.end_time
ORDER BY rsi.start_time`, sp.value("rs"), qsRuntimeFrom, sp.window(&a, sp.opts.From, sp.opts.To))

	rows, err := d.query(ctx, q, a.args...)
	if err != nil {
		return nil, fmt.Errorf("gosmo: overall resource consumption in %q: %w", d.name, err)
	}
	defer rows.Close()
	var out []*QSIntervalStat
	for rows.Next() {
		st := &QSIntervalStat{}
		var value sql.NullFloat64
		if err := rows.Scan(&st.StartTime, &st.EndTime, &st.ExecCount, &value); err != nil {
			return nil, fmt.Errorf("gosmo: overall resource consumption in %q: %w", d.name, err)
		}
		st.Value = value.Float64
		out = append(out, st)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("gosmo: overall resource consumption in %q: %w", d.name, err)
	}
	return out, nil
}

// QueryStoreTrackedQuery returns one query's per-plan time series — SSMS's
// Tracked Queries view.
func (d *Database) QueryStoreTrackedQuery(queryID int64, opts QueryStoreReportOptions) ([]*QSPlanIntervalStat, error) {
	return d.QueryStoreTrackedQueryContext(context.Background(), queryID, opts)
}

// QueryStoreTrackedQueryContext is the context-aware variant of
// QueryStoreTrackedQuery. Rows come back plan by plan, oldest interval first,
// which is the order a per-plan series is plotted in.
func (d *Database) QueryStoreTrackedQueryContext(ctx context.Context, queryID int64, opts QueryStoreReportOptions) ([]*QSPlanIntervalStat, error) {
	sp, err := opts.resolve()
	if err != nil {
		return nil, err
	}
	var a qsArgs
	q := fmt.Sprintf(`SELECT p.plan_id,
       rsi.start_time,
       rsi.end_time,
       SUM(rs.count_executions) AS exec_count,
       %s AS value
%s
%s
  AND q.query_id = %s
GROUP BY p.plan_id, rsi.runtime_stats_interval_id, rsi.start_time, rsi.end_time
ORDER BY p.plan_id, rsi.start_time`,
		sp.value("rs"), qsRuntimeFrom, sp.window(&a, sp.opts.From, sp.opts.To), a.add(queryID))

	rows, err := d.query(ctx, q, a.args...)
	if err != nil {
		return nil, fmt.Errorf("gosmo: tracked query %d in %q: %w", queryID, d.name, err)
	}
	defer rows.Close()
	var out []*QSPlanIntervalStat
	for rows.Next() {
		st := &QSPlanIntervalStat{}
		var value sql.NullFloat64
		if err := rows.Scan(&st.PlanID, &st.StartTime, &st.EndTime, &st.ExecCount, &value); err != nil {
			return nil, fmt.Errorf("gosmo: tracked query %d in %q: %w", queryID, d.name, err)
		}
		st.Value = value.Float64
		out = append(out, st)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("gosmo: tracked query %d in %q: %w", queryID, d.name, err)
	}
	return out, nil
}

// QueryStorePlans returns every plan Query Store holds for one query, with
// its cost over the report's window — the plan list under SSMS's Query Store
// views, and what Force Plan picks from.
func (d *Database) QueryStorePlans(queryID int64, opts QueryStoreReportOptions) ([]*QSPlan, error) {
	return d.QueryStorePlansContext(context.Background(), queryID, opts)
}

// QueryStorePlansContext is the context-aware variant of QueryStorePlans.
//
// The runtime-stats join is a LEFT one: a plan that did not run inside the
// window still exists, is still forceable, and still has plan XML worth
// showing — dropping it would hide the very plan a user opened the report to
// force back.
func (d *Database) QueryStorePlansContext(ctx context.Context, queryID int64, opts QueryStoreReportOptions) ([]*QSPlan, error) {
	sp, err := opts.resolve()
	if err != nil {
		return nil, err
	}
	var a qsArgs
	q := fmt.Sprintf(`SELECT p.plan_id,
       p.query_id,
       p.is_forced_plan,
       COALESCE(p.plan_forcing_type_desc, ''),
       p.force_failure_count,
       COALESCE(p.last_force_failure_reason_desc, ''),
       p.compatibility_level,
       p.is_trivial_plan,
       p.is_parallel_plan,
       p.last_compile_start_time,
       p.last_execution_time,
       COALESCE(p.query_plan, ''),
       COALESCE(SUM(rs.count_executions), 0) AS exec_count,
       %s AS value
FROM   sys.query_store_plan AS p
LEFT JOIN sys.query_store_runtime_stats AS rs
       ON rs.plan_id = p.plan_id
LEFT JOIN sys.query_store_runtime_stats_interval AS rsi
       ON rsi.runtime_stats_interval_id = rs.runtime_stats_interval_id
      AND rsi.start_time >= %s AND rsi.start_time < %s
WHERE  p.query_id = %s
GROUP BY p.plan_id, p.query_id, p.is_forced_plan, p.plan_forcing_type_desc,
         p.force_failure_count, p.last_force_failure_reason_desc,
         p.compatibility_level, p.is_trivial_plan, p.is_parallel_plan,
         p.last_compile_start_time, p.last_execution_time, p.query_plan
ORDER BY p.plan_id`,
		sp.value("rs"), a.add(sp.opts.From), a.add(sp.opts.To), a.add(queryID))

	rows, err := d.query(ctx, q, a.args...)
	if err != nil {
		return nil, fmt.Errorf("gosmo: query store plans for query %d in %q: %w", queryID, d.name, err)
	}
	defer rows.Close()
	var out []*QSPlan
	for rows.Next() {
		pl := &QSPlan{}
		var value sql.NullFloat64
		var compiled, executed sql.NullTime
		if err := rows.Scan(&pl.PlanID, &pl.QueryID, &pl.IsForced, &pl.ForcingType,
			&pl.ForceFailureCount, &pl.LastForceFailureReason, &pl.CompatibilityLevel,
			&pl.IsTrivialPlan, &pl.IsParallelPlan, &compiled, &executed,
			&pl.QueryPlanXML, &pl.ExecCount, &value); err != nil {
			return nil, fmt.Errorf("gosmo: query store plans for query %d in %q: %w", queryID, d.name, err)
		}
		pl.LastCompileStartTime = compiled.Time
		pl.LastExecutionTime = executed.Time
		pl.Value = value.Float64
		out = append(out, pl)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("gosmo: query store plans for query %d in %q: %w", queryID, d.name, err)
	}
	return out, nil
}

// QueryStoreQueryText returns one query's SQL text and the schema-qualified
// module it belongs to, empty if it is ad hoc.
func (d *Database) QueryStoreQueryText(queryID int64) (text, objectName string, err error) {
	return d.QueryStoreQueryTextContext(context.Background(), queryID)
}

// QueryStoreQueryTextContext is the context-aware variant of
// QueryStoreQueryText. A query id Query Store no longer holds — cleanup
// removes them — comes back as an error wrapping ErrNotFound.
func (d *Database) QueryStoreQueryTextContext(ctx context.Context, queryID int64) (text, objectName string, err error) {
	q := fmt.Sprintf(`SELECT qt.query_sql_text, %s
FROM   sys.query_store_query      AS q
JOIN   sys.query_store_query_text AS qt ON qt.query_text_id = q.query_text_id
WHERE  q.query_id = @p1`, qsObjectName)

	err = d.queryRow(ctx, func(row *sql.Row) error {
		return row.Scan(&text, &objectName)
	}, q, queryID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", notFoundf("gosmo: query store query %d in %q: not found", queryID, d.name)
	}
	if err != nil {
		return "", "", fmt.Errorf("gosmo: query store query %d in %q: %w", queryID, d.name, err)
	}
	return text, objectName, nil
}

// -- Wait statistics -----------------------------------------------------------

// QueryStoreWaitStatsSupported reports whether this instance has
// sys.query_store_wait_stats, which SQL Server 2017 added. Below it the two
// wait reports return an error rather than an empty result, so a caller
// building a report list should leave them out.
func (d *Database) QueryStoreWaitStatsSupported() bool {
	major := d.serverMajorVersion()
	// An unread version is treated as supported for the same reason QueryStoreMetrics
	// treats it as supporting everything: the query is a better authority than
	// a version we never learned.
	return major == 0 || major >= int(SQLServer2017)
}

// errWaitStatsUnsupported explains the version gate in the terms a caller can
// show a user.
func (d *Database) errWaitStatsUnsupported() error {
	return fmt.Errorf("gosmo: query store wait statistics in %q: requires SQL Server 2017 or later", d.name)
}

// qsWaitFrom is the join both wait reports read. The runtime-stats join is
// what supplies count_executions: sys.query_store_wait_stats has no execution
// count of its own, so an average wait per execution cannot be computed from
// it alone. Joining on all three of plan, interval and execution type is what
// keeps that a one-to-one match rather than a fan-out.
const qsWaitFrom = `
FROM   sys.query_store_wait_stats             AS ws
JOIN   sys.query_store_plan                   AS p   ON p.plan_id = ws.plan_id
JOIN   sys.query_store_query                  AS q   ON q.query_id = p.query_id
JOIN   sys.query_store_query_text             AS qt  ON qt.query_text_id = q.query_text_id
JOIN   sys.query_store_runtime_stats_interval AS rsi ON rsi.runtime_stats_interval_id = ws.runtime_stats_interval_id
LEFT JOIN sys.query_store_runtime_stats       AS rs  ON rs.plan_id = ws.plan_id
                                                    AND rs.runtime_stats_interval_id = ws.runtime_stats_interval_id
                                                    AND rs.execution_type = ws.execution_type`

// QueryStoreWaitCategories totals wait time by category — the top half of
// SSMS's Query Wait Statistics view.
func (d *Database) QueryStoreWaitCategories(opts QueryStoreReportOptions) ([]*QSWaitStat, error) {
	return d.QueryStoreWaitCategoriesContext(context.Background(), opts)
}

// QueryStoreWaitCategoriesContext is the context-aware variant of
// QueryStoreWaitCategories. Values are milliseconds whatever Options.Metric
// says — Query Store records wait time and nothing else per category — but
// Options.Statistic still applies.
func (d *Database) QueryStoreWaitCategoriesContext(ctx context.Context, opts QueryStoreReportOptions) ([]*QSWaitStat, error) {
	if !d.QueryStoreWaitStatsSupported() {
		return nil, d.errWaitStatsUnsupported()
	}
	sp, err := opts.resolve()
	if err != nil {
		return nil, err
	}
	var a qsArgs
	top := a.add(sp.opts.Top)
	q := fmt.Sprintf(`SELECT TOP (%s) ws.wait_category_desc,
       COALESCE(SUM(rs.count_executions), 0) AS exec_count,
       %s AS value
%s
%s
GROUP BY ws.wait_category_desc
ORDER BY value DESC`, top, sp.stat.wait("ws", "rs"), qsWaitFrom, sp.window(&a, sp.opts.From, sp.opts.To))

	rows, err := d.query(ctx, q, a.args...)
	if err != nil {
		return nil, fmt.Errorf("gosmo: query store wait categories in %q: %w", d.name, err)
	}
	defer rows.Close()
	var out []*QSWaitStat
	for rows.Next() {
		st := &QSWaitStat{}
		var value sql.NullFloat64
		if err := rows.Scan(&st.Category, &st.ExecCount, &value); err != nil {
			return nil, fmt.Errorf("gosmo: query store wait categories in %q: %w", d.name, err)
		}
		st.Value = value.Float64
		out = append(out, st)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("gosmo: query store wait categories in %q: %w", d.name, err)
	}
	return out, nil
}

// QueryStoreWaitingQueries ranks the queries waiting in one category — the
// drill-down half of SSMS's Query Wait Statistics view.
func (d *Database) QueryStoreWaitingQueries(category string, opts QueryStoreReportOptions) ([]*QSQueryStat, error) {
	return d.QueryStoreWaitingQueriesContext(context.Background(), category, opts)
}

// QueryStoreWaitingQueriesContext is the context-aware variant of
// QueryStoreWaitingQueries. category is a sys.query_store_wait_stats
// wait_category_desc value — one of the Category strings
// QueryStoreWaitCategoriesContext returned. An empty category covers every
// one of them. Values are milliseconds.
func (d *Database) QueryStoreWaitingQueriesContext(ctx context.Context, category string, opts QueryStoreReportOptions) ([]*QSQueryStat, error) {
	if !d.QueryStoreWaitStatsSupported() {
		return nil, d.errWaitStatsUnsupported()
	}
	sp, err := opts.resolve()
	if err != nil {
		return nil, err
	}
	var a qsArgs
	top := a.add(sp.opts.Top)
	where := sp.window(&a, sp.opts.From, sp.opts.To)
	if category != "" {
		where += "\n  AND ws.wait_category_desc = " + a.add(category)
	}
	q := fmt.Sprintf(`SELECT TOP (%s) q.query_id,
       qt.query_sql_text,
       %s AS object_name,
       COALESCE(SUM(rs.count_executions), 0)                  AS exec_count,
       COUNT(DISTINCT p.plan_id)                              AS plan_count,
       MAX(CASE WHEN p.is_forced_plan = 1 THEN p.plan_id END) AS forced_plan_id,
       MAX(rsi.end_time)                                      AS last_execution_time,
       %s AS value,
       CAST(0 AS float) AS baseline_value,
       0                AS baseline_exec_count,
       CAST(0 AS float) AS regression,
       CAST(0 AS float) AS variation
%s
%s
%s
ORDER BY value DESC`, top, qsObjectName, sp.stat.wait("ws", "rs"), qsWaitFrom, where, qsQueryGroupBy)

	rows, err := d.query(ctx, q, a.args...)
	if err != nil {
		return nil, fmt.Errorf("gosmo: query store waiting queries in %q: %w", d.name, err)
	}
	defer rows.Close()
	stats, err := scanQueryStats(rows)
	if err != nil {
		return nil, fmt.Errorf("gosmo: query store waiting queries in %q: %w", d.name, err)
	}
	return stats, nil
}

// -- Plan forcing --------------------------------------------------------------

// QueryStoreForcePlan pins one plan as the only plan the optimizer may use
// for a query — SSMS's Force Plan.
func (d *Database) QueryStoreForcePlan(queryID, planID int64) error {
	return d.QueryStoreForcePlanContext(context.Background(), queryID, planID)
}

// QueryStoreForcePlanContext is the context-aware variant of
// QueryStoreForcePlan. It needs ALTER on the database.
//
// Forcing does not guarantee the plan is used: the engine records a failure
// on sys.query_store_plan.last_force_failure_reason_desc and silently
// recompiles when the plan can no longer be produced (a dropped index, say).
// Read the plan back with QueryStorePlansContext to see whether it took.
func (d *Database) QueryStoreForcePlanContext(ctx context.Context, queryID, planID int64) error {
	const q = `EXEC sys.sp_query_store_force_plan @query_id = @p1, @plan_id = @p2`
	if _, err := d.exec(ctx, q, queryID, planID); err != nil {
		return fmt.Errorf("gosmo: force plan %d for query %d in %q: %w", planID, queryID, d.name, err)
	}
	return nil
}

// QueryStoreUnforcePlan releases a plan forced by QueryStoreForcePlan,
// returning the query to normal optimization — SSMS's Unforce Plan.
func (d *Database) QueryStoreUnforcePlan(queryID, planID int64) error {
	return d.QueryStoreUnforcePlanContext(context.Background(), queryID, planID)
}

// QueryStoreUnforcePlanContext is the context-aware variant of
// QueryStoreUnforcePlan. It needs ALTER on the database.
func (d *Database) QueryStoreUnforcePlanContext(ctx context.Context, queryID, planID int64) error {
	const q = `EXEC sys.sp_query_store_unforce_plan @query_id = @p1, @plan_id = @p2`
	if _, err := d.exec(ctx, q, queryID, planID); err != nil {
		return fmt.Errorf("gosmo: unforce plan %d for query %d in %q: %w", planID, queryID, d.name, err)
	}
	return nil
}

// serverMajorVersion is the instance's major version, or 0 when it was never
// read — a Database built from a Server that skipped loadInfo.
func (d *Database) serverMajorVersion() int {
	if d.server == nil || d.server.info == nil {
		return 0
	}
	return d.server.info.VersionMajor
}
