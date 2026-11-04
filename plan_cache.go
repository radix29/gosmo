package gosmo

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// ============================================================
// Plan cache
// ============================================================
//
// The cached statements' runtime totals (sys.dm_exec_query_stats) and their
// plans (sys.dm_exec_text_query_plan). Both need VIEW SERVER STATE, and both
// see only what is still cached: a statement's totals go with its plan, at
// eviction, recompilation or a restart, so a ranking covers "since the plan
// was cached" (CachedQueryStat.CreationTime), not a fixed window — Query
// Store is the durable history.

// TopQueriesBy is the figure Server.TopQueries ranks statements by.
type TopQueriesBy int

const (
	// TopByCPU ranks by worker (CPU) time.
	TopByCPU TopQueriesBy = iota
	// TopByLogicalReads ranks by logical reads.
	TopByLogicalReads
	// TopByLogicalWrites ranks by logical writes.
	TopByLogicalWrites
	// TopByDuration ranks by elapsed time.
	TopByDuration
	// TopByExecutions ranks by execution count. It has no average.
	TopByExecutions
)

// topQueriesColumn is each TopQueriesBy's dm_exec_query_stats column.
var topQueriesColumn = map[TopQueriesBy]string{
	TopByCPU:           "total_worker_time",
	TopByLogicalReads:  "total_logical_reads",
	TopByLogicalWrites: "total_logical_writes",
	TopByDuration:      "total_elapsed_time",
	TopByExecutions:    "execution_count",
}

// TopQueriesOptions says what Server.TopQueries returns.
type TopQueriesOptions struct {
	By TopQueriesBy
	// Average ranks by the figure per execution instead of its total. It is
	// refused with TopByExecutions.
	Average bool
	// Top caps the statements returned; 0 means 20.
	Top int
	// Database, when set, keeps the statements whose plan was compiled in
	// that database (the plan's dbid attribute: the connection's database
	// for an ad hoc batch, the module's for a procedure). A name no database
	// has is an ErrNotFound error.
	Database string
}

// CachedQueryStat is one cached statement's totals since its plan was
// cached, from sys.dm_exec_query_stats. Times are in microseconds, as the
// DMV keeps them.
type CachedQueryStat struct {
	// Database is the database the plan was compiled in, empty when the
	// plan names none the login can see.
	Database string
	// QueryHash groups statements that differ only in their literals.
	QueryHash []byte
	// PlanHandle and the statement offsets (bytes into the batch, End -1 for
	// its end) address the statement's plan: Server.CachedPlan takes them.
	PlanHandle     []byte
	StatementStart int
	StatementEnd   int
	// Text is the statement, cut from its batch by the offsets; empty when
	// the batch text has left the cache or is an encrypted module's.
	Text string

	ExecutionCount      int64
	TotalCPUMicros      int64
	TotalDurationMicros int64
	TotalLogicalReads   int64
	TotalLogicalWrites  int64

	// LastExecutionTime and CreationTime (when the plan was cached) are the
	// server's local clock, carried in time.UTC.
	LastExecutionTime time.Time
	CreationTime      time.Time
}

// AvgCPUMicros, AvgDurationMicros, AvgLogicalReads and AvgLogicalWrites are
// the totals per execution.
func (q *CachedQueryStat) AvgCPUMicros() float64      { return q.avg(q.TotalCPUMicros) }
func (q *CachedQueryStat) AvgDurationMicros() float64 { return q.avg(q.TotalDurationMicros) }
func (q *CachedQueryStat) AvgLogicalReads() float64   { return q.avg(q.TotalLogicalReads) }
func (q *CachedQueryStat) AvgLogicalWrites() float64  { return q.avg(q.TotalLogicalWrites) }

func (q *CachedQueryStat) avg(total int64) float64 {
	if q.ExecutionCount == 0 {
		return 0
	}
	return float64(total) / float64(q.ExecutionCount)
}

// TopQueries returns the cached statements that rank highest by opts.By,
// highest first. The ranking is taken before each statement's text and
// database are looked up, so its cost does not grow with the plan cache's
// size beyond one scan of it — unless opts.Database is set, which needs
// every plan's attributes to filter on.
func (s *Server) TopQueries(ctx context.Context, opts TopQueriesOptions) ([]*CachedQueryStat, error) {
	col, ok := topQueriesColumn[opts.By]
	switch {
	case !ok:
		return nil, invalidf("gosmo: top queries: unrecognized ranking %d", opts.By)
	case opts.Average && opts.By == TopByExecutions:
		return nil, invalidf("gosmo: top queries: execution count has no average")
	case opts.Top < 0:
		return nil, invalidf("gosmo: top queries: top %d is negative", opts.Top)
	}
	top := opts.Top
	if top == 0 {
		top = 20
	}
	rank := func(alias string) string {
		if opts.Average {
			return "CAST(" + alias + "." + col + " AS float) / NULLIF(" + alias + ".execution_count, 0)"
		}
		return alias + "." + col
	}
	const dbidApply = `(SELECT CONVERT(int, pa.value) AS dbid
              FROM   sys.dm_exec_plan_attributes(%s.plan_handle) AS pa
              WHERE  pa.attribute = N'dbid') AS d`

	args := []any{top}
	filter := ""
	if opts.Database != "" {
		var id sql.NullInt64
		if err := s.queryRowScan(ctx, "SELECT DB_ID(@p1)", []any{opts.Database}, &id); err != nil {
			return nil, fmt.Errorf("gosmo: top queries in %q: %w", opts.Database, err)
		}
		if !id.Valid {
			return nil, notFoundf("gosmo: top queries: database %q not found", opts.Database)
		}
		args = append(args, id.Int64)
		filter = "\n       CROSS  APPLY " + fmt.Sprintf(dbidApply, "qs") + "\n       WHERE  d.dbid = @p2"
	}

	q := `
WITH ranked AS (
       SELECT TOP (@p1) qs.sql_handle, qs.plan_handle, qs.query_hash,
              qs.statement_start_offset, qs.statement_end_offset,
              qs.execution_count, qs.total_worker_time, qs.total_elapsed_time,
              qs.total_logical_reads, qs.total_logical_writes,
              qs.last_execution_time, qs.creation_time
       FROM   sys.dm_exec_query_stats AS qs` + filter + `
       ORDER  BY ` + rank("qs") + ` DESC, qs.plan_handle, qs.statement_start_offset
)
SELECT ISNULL(DB_NAME(d.dbid), N''), q.query_hash, q.plan_handle,
       q.statement_start_offset, q.statement_end_offset,
       ISNULL(SUBSTRING(st.text, q.statement_start_offset / 2 + 1,
              (CASE q.statement_end_offset WHEN -1 THEN DATALENGTH(st.text)
                    ELSE q.statement_end_offset END - q.statement_start_offset) / 2 + 1), N''),
       q.execution_count, q.total_worker_time, q.total_elapsed_time,
       q.total_logical_reads, q.total_logical_writes,
       q.last_execution_time, q.creation_time
FROM   ranked AS q
OUTER  APPLY ` + fmt.Sprintf(dbidApply, "q") + `
OUTER  APPLY sys.dm_exec_sql_text(q.sql_handle) AS st
ORDER  BY ` + rank("q") + ` DESC, q.plan_handle, q.statement_start_offset`

	rows, err := s.query(ctx, q, args...)
	return scanRows(rows, err, "top queries", func(scan func(...any) error) (*CachedQueryStat, error) {
		c := &CachedQueryStat{}
		err := scan(&c.Database, &c.QueryHash, &c.PlanHandle,
			&c.StatementStart, &c.StatementEnd, &c.Text,
			&c.ExecutionCount, &c.TotalCPUMicros, &c.TotalDurationMicros,
			&c.TotalLogicalReads, &c.TotalLogicalWrites,
			&c.LastExecutionTime, &c.CreationTime)
		return c, err
	})
}

// CachedPlan returns the cached Showplan XML of one statement of a plan:
// the estimated plan the server compiled, with no runtime figures. planHandle
// and the offsets are a CachedQueryStat's; statement offsets 0 and -1 give
// the plan of the whole batch.
//
// It reads sys.dm_exec_text_query_plan, the text form, because the xml form
// (dm_exec_query_plan) fails on a plan nested more than 128 levels deep.
// A plan that has left the cache, or a statement not compiled yet (a
// deferred compile, such as one touching a temporary table its batch
// creates), is an ErrNotFound error.
func (s *Server) CachedPlan(ctx context.Context, planHandle []byte, stmtStart, stmtEnd int) (string, error) {
	what := fmt.Sprintf("read cached plan 0x%X", planHandle)
	switch {
	case len(planHandle) == 0:
		return "", invalidf("gosmo: read cached plan: no plan handle")
	case stmtStart < 0 || stmtEnd < -1 || (stmtEnd != -1 && stmtEnd < stmtStart):
		return "", invalidf("gosmo: %s: statement offsets %d..%d are not a statement", what, stmtStart, stmtEnd)
	}
	var plan sql.NullString
	err := s.queryRowScan(ctx, `
SELECT query_plan FROM sys.dm_exec_text_query_plan(@p1, @p2, @p3)`,
		[]any{planHandle, stmtStart, stmtEnd}, &plan)
	if err == nil && !plan.Valid {
		err = sql.ErrNoRows
	}
	return foundRow(plan.String, err,
		notFoundf("gosmo: %s: no plan in cache for that statement", what), what)
}
