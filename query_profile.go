package gosmo

import (
	"context"
	"fmt"
)

// ============================================================
// Live query profiles
// ============================================================
//
// The two readings SSMS's Live Query Statistics polls while another session
// runs a statement: the operator counters of sys.dm_exec_query_profiles and
// the in-flight showplan of sys.dm_exec_query_statistics_xml. Both describe a
// *different* session from the one reading them, so a caller polls on a
// connection of its own, never the session it is watching.
//
// Both are empty unless the watched session is being profiled. On 2016 and
// 2017 that means the session switched profiling on itself — SET STATISTICS
// XML ON (StartPlanCapture with PlanActual) or SET STATISTICS PROFILE ON —
// before the statement started; 2019 and later also profile every session
// lightly by default (LIGHTWEIGHT_QUERY_PROFILING), which carries the row
// counts but not the CPU, elapsed or I/O figures.
//
// Rights: VIEW SERVER STATE (VIEW SERVER PERFORMANCE STATE on 2022 and
// later), VIEW DATABASE STATE on Azure SQL Database. Without it both reads are
// refused outright (Msg 300, IsPermissionDenied) rather than narrowed — seen
// on 2016 SP3 and 2025.
//
// No version gate: every column read here exists on 2016 SP1, gosmo's floor
// (MinimumServerVersion), which is also the release that added
// sys.dm_exec_query_statistics_xml. The columns later releases added to
// sys.dm_exec_query_profiles (the page-server reads and
// row_requalification_count) are left out on purpose, and so are
// statistics_xml's own statement offsets, which 2016 lacks — the offsets come
// from sys.dm_exec_requests instead, which has them on every version.

// QueryProfile is one row of sys.dm_exec_query_profiles: one plan operator on
// one thread of a running statement. A parallel operator has a row per
// thread (thread 0 is the coordinator), so a per-operator figure is the sum
// over the rows sharing a NodeID — that merge is the caller's.
//
// The *Time fields are the server's millisecond tick count
// (sys.dm_os_sys_info.ms_ticks) when the event happened, 0 while it has not:
// CloseTime non-zero means the operator has finished. They are not
// timestamps and are only comparable with each other.
type QueryProfile struct {
	RequestID int
	// PlanHandle and StatementStart/StatementEnd (byte offsets into the
	// batch text, StatementEnd -1 for "to the end") identify the statement
	// these counters belong to; they change when a batch moves on to its
	// next statement, which is when an InFlightPlan read goes stale.
	PlanHandle     []byte
	StatementStart int
	StatementEnd   int

	NodeID           int
	ThreadID         int
	PhysicalOperator string

	RowCount         int64
	EstimateRowCount int64
	RewindCount      int64
	RebindCount      int64
	EndOfScanCount   int64

	FirstActiveTime int64
	LastActiveTime  int64
	OpenTime        int64
	FirstRowTime    int64
	LastRowTime     int64
	CloseTime       int64

	// ElapsedMs and CPUMs are zero under lightweight profiling, which does
	// not collect them.
	ElapsedMs int64
	CPUMs     int64

	// The object an access operator reads; all three are zero for an
	// operator that reads none (a join, a filter, an aggregate).
	DatabaseID int
	ObjectID   int
	IndexID    int

	// The I/O counters are zero for an operator that does no I/O of its own.
	ScanCount             int64
	LogicalReads          int64
	PhysicalReads         int64
	ReadAheads            int64
	WritePages            int64
	LobLogicalReads       int64
	LobPhysicalReads      int64
	LobReadAheads         int64
	SegmentReads          int64
	SegmentSkips          int64
	ActualReadRowCount    int64
	EstimatedReadRowCount int64
}

// QueryProfiles returns the operator counters of whatever sessionID is
// running now, ordered by request, node and thread. A session that is idle,
// or running unprofiled, returns none and no error.
func (s *Server) QueryProfiles(ctx context.Context, sessionID int) ([]QueryProfile, error) {
	// The I/O and object columns are NULL for an operator that has none,
	// observed on 2025; the rest have always been filled in.
	rows, err := s.query(ctx, `
SELECT p.request_id, p.plan_handle,
       ISNULL(r.statement_start_offset, 0), ISNULL(r.statement_end_offset, -1),
       p.node_id, p.thread_id, p.physical_operator_name,
       p.row_count, p.estimate_row_count, p.rewind_count, p.rebind_count, p.end_of_scan_count,
       p.first_active_time, p.last_active_time, p.open_time,
       p.first_row_time, p.last_row_time, p.close_time,
       p.elapsed_time_ms, p.cpu_time_ms,
       ISNULL(p.database_id, 0), ISNULL(p.object_id, 0), ISNULL(p.index_id, 0),
       ISNULL(p.scan_count, 0), ISNULL(p.logical_read_count, 0), ISNULL(p.physical_read_count, 0),
       ISNULL(p.read_ahead_count, 0), ISNULL(p.write_page_count, 0),
       ISNULL(p.lob_logical_read_count, 0), ISNULL(p.lob_physical_read_count, 0),
       ISNULL(p.lob_read_ahead_count, 0),
       ISNULL(p.segment_read_count, 0), ISNULL(p.segment_skip_count, 0),
       ISNULL(p.actual_read_row_count, 0), ISNULL(p.estimated_read_row_count, 0)
FROM   sys.dm_exec_query_profiles AS p
LEFT   JOIN sys.dm_exec_requests AS r
         ON r.session_id = p.session_id AND r.request_id = p.request_id
WHERE  p.session_id = @p1
ORDER  BY p.request_id, p.node_id, p.thread_id`, sessionID)
	return scanRows(rows, err, fmt.Sprintf("read query profiles of session %d", sessionID), func(scan func(...any) error) (QueryProfile, error) {
		var p QueryProfile
		err := scan(&p.RequestID, &p.PlanHandle, &p.StatementStart, &p.StatementEnd,
			&p.NodeID, &p.ThreadID, &p.PhysicalOperator,
			&p.RowCount, &p.EstimateRowCount, &p.RewindCount, &p.RebindCount, &p.EndOfScanCount,
			&p.FirstActiveTime, &p.LastActiveTime, &p.OpenTime,
			&p.FirstRowTime, &p.LastRowTime, &p.CloseTime,
			&p.ElapsedMs, &p.CPUMs,
			&p.DatabaseID, &p.ObjectID, &p.IndexID,
			&p.ScanCount, &p.LogicalReads, &p.PhysicalReads,
			&p.ReadAheads, &p.WritePages,
			&p.LobLogicalReads, &p.LobPhysicalReads, &p.LobReadAheads,
			&p.SegmentReads, &p.SegmentSkips,
			&p.ActualReadRowCount, &p.EstimatedReadRowCount)
		return p, err
	})
}

// InFlightPlan is the showplan of the statement a session is running now,
// from sys.dm_exec_query_statistics_xml: the same Showplan XML document an
// actual plan is, with each operator's RunTimeInformation holding the
// counters so far rather than the final ones.
type InFlightPlan struct {
	RequestID int
	// PlanHandle and the statement offsets are QueryProfile's: a profile row
	// whose three match belongs to this plan.
	PlanHandle     []byte
	StatementStart int
	StatementEnd   int
	XML            string
}

// InFlightPlan reads the showplan of the statement sessionID is running now.
// It returns an ErrNotFound error when the session is running nothing, or
// nothing profiled. A session running several requests (MARS) reports its
// lowest request_id.
//
// The document is tens of kilobytes and costs the server a plan
// serialisation each time, so a poller re-reads it only when a QueryProfiles
// row names a different statement, not on every tick.
func (s *Server) InFlightPlan(ctx context.Context, sessionID int) (*InFlightPlan, error) {
	var p InFlightPlan
	err := s.queryRowScan(ctx, `
SELECT TOP (1) x.request_id, x.plan_handle,
       r.statement_start_offset, r.statement_end_offset,
       CONVERT(nvarchar(max), x.query_plan)
FROM   sys.dm_exec_query_statistics_xml(@p1) AS x
JOIN   sys.dm_exec_requests AS r
         ON r.session_id = x.session_id AND r.request_id = x.request_id
WHERE  x.query_plan IS NOT NULL
ORDER  BY x.request_id`, []any{sessionID},
		&p.RequestID, &p.PlanHandle, &p.StatementStart, &p.StatementEnd, &p.XML)
	return foundRow(&p, err,
		notFoundf("gosmo: session %d is running no profiled statement", sessionID),
		fmt.Sprintf("read in-flight plan of session %d", sessionID))
}
