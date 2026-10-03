package gosmo

import (
	"context"
	"database/sql"
	"fmt"
)

// ============================================================
// tempdb usage
// ============================================================
//
// What is using tempdb right now: its space by allocation kind, its files,
// the objects in it grouped by who made them, and the sessions holding space
// in it. Each reads tempdb's own catalog by three-part name, not USE:
// dm_db_file_space_usage is database-scoped and would report the pooled
// connection's current database. Like server_activity.go's reads they need
// VIEW SERVER STATE.

// pagesPerMB converts 8 KB page counts to MB.
const pagesPerMB = 128

// TempDBSpace is tempdb's data-file space in MB. The four allocated parts and
// FreeMB sum to TotalMB. Log files are not included: dm_db_file_space_usage
// covers data files only.
type TempDBSpace struct {
	VersionStoreMB   float64
	UserObjectMB     float64
	InternalObjectMB float64
	MixedExtentMB    float64
	FreeMB           float64
	TotalMB          float64
}

// TempDBSpace reads tempdb's data-file space by allocation kind.
func (s *Server) TempDBSpace(ctx context.Context) (TempDBSpace, error) {
	var total, free, version, user, internal, mixed sql.NullInt64
	err := s.queryRowScan(ctx, `
SELECT SUM(CAST(total_page_count AS bigint)),
       SUM(CAST(unallocated_extent_page_count AS bigint)),
       SUM(CAST(version_store_reserved_page_count AS bigint)),
       SUM(CAST(user_object_reserved_page_count AS bigint)),
       SUM(CAST(internal_object_reserved_page_count AS bigint)),
       SUM(CAST(mixed_extent_page_count AS bigint))
FROM   tempdb.sys.dm_db_file_space_usage`, nil, &total, &free, &version, &user, &internal, &mixed)
	if err != nil {
		return TempDBSpace{}, fmt.Errorf("gosmo: read tempdb space: %w", err)
	}
	mb := func(v sql.NullInt64) float64 { return float64(v.Int64) / pagesPerMB }
	return TempDBSpace{
		TotalMB:          mb(total),
		FreeMB:           mb(free),
		VersionStoreMB:   mb(version),
		UserObjectMB:     mb(user),
		InternalObjectMB: mb(internal),
		MixedExtentMB:    mb(mixed),
	}, nil
}

// TempDBFile is one tempdb file. A log file's UsedMB is zero: its allocation
// is not in dm_db_file_space_usage.
type TempDBFile struct {
	FileID int
	Name   string
	Type   string // "ROWS" or "LOG"
	SizeMB float64
	UsedMB float64
	// GrowthMB is the autogrowth increment, or the percentage when
	// PercentGrowth is set; zero means growth is disabled.
	GrowthMB      float64
	PercentGrowth bool
}

// TempDBFiles lists tempdb's files, data files first, each in file_id order.
//
// It reads tempdb.sys.database_files, not sys.master_files: master_files has
// the configured size tempdb is recreated at on restart, database_files the
// size it has actually grown to.
func (s *Server) TempDBFiles(ctx context.Context) ([]TempDBFile, error) {
	rows, err := s.query(ctx, `
SELECT df.file_id, df.name, df.type_desc,
       CAST(df.size AS bigint),
       CAST(ISNULL(fsu.total_page_count - fsu.unallocated_extent_page_count, 0) AS bigint),
       df.growth, df.is_percent_growth
FROM   tempdb.sys.database_files df
LEFT   JOIN tempdb.sys.dm_db_file_space_usage fsu ON fsu.file_id = df.file_id
ORDER  BY df.type_desc DESC, df.file_id`)
	return scanRows(rows, err, "list tempdb files", func(scan func(...any) error) (TempDBFile, error) {
		var f TempDBFile
		var sizePages, usedPages, growth int64
		if err := scan(&f.FileID, &f.Name, &f.Type, &sizePages, &usedPages, &growth, &f.PercentGrowth); err != nil {
			return f, err
		}
		f.SizeMB = float64(sizePages) / pagesPerMB
		f.UsedMB = float64(usedPages) / pagesPerMB
		if f.PercentGrowth {
			f.GrowthMB = float64(growth) // already a percentage
		} else {
			f.GrowthMB = float64(growth) / pagesPerMB
		}
		return f, nil
	})
}

// TempDBObjectKind groups tempdb's objects by who made them, which decides
// whom to talk to about one.
type TempDBObjectKind int

const (
	// TempDBLocalTemp is #tables: one session's.
	TempDBLocalTemp TempDBObjectKind = iota
	// TempDBGlobalTemp is ##tables: shared until their creator disconnects.
	TempDBGlobalTemp
	// TempDBUserTable is a permanent table someone created in tempdb itself.
	TempDBUserTable
	// TempDBInternal is the engine's internal tables (type IT).
	TempDBInternal
	// TempDBSystem is what ships with SQL Server (is_ms_shipped).
	TempDBSystem
)

// TempDBObjects is one kind's footprint in tempdb.
type TempDBObjects struct {
	Kind       TempDBObjectKind
	Count      int
	ReservedMB float64
	UsedMB     float64
	Rows       int64
}

// TempDBObjects returns tempdb's objects summed by kind, one entry per kind
// present, in kind order.
func (s *Server) TempDBObjects(ctx context.Context) ([]TempDBObjects, error) {
	rows, err := s.query(ctx, `
SELECT kind, COUNT(DISTINCT object_id),
       SUM(reserved_page_count), SUM(used_page_count), SUM(row_count)
FROM (
    SELECT o.object_id,
           CASE WHEN o.type = 'IT' THEN 3
                WHEN o.is_ms_shipped = 1 THEN 4
                WHEN o.name LIKE '##%' THEN 1
                WHEN o.name LIKE '#%' THEN 0
                ELSE 2 END AS kind,
           ps.reserved_page_count, ps.used_page_count, ps.row_count
    FROM   tempdb.sys.objects o
    JOIN   tempdb.sys.dm_db_partition_stats ps ON ps.object_id = o.object_id
) x
GROUP BY kind
ORDER BY kind`)
	return scanRows(rows, err, "read tempdb objects", func(scan func(...any) error) (TempDBObjects, error) {
		var o TempDBObjects
		var reserved, used, rowCount sql.NullInt64
		if err := scan(&o.Kind, &o.Count, &reserved, &used, &rowCount); err != nil {
			return o, err
		}
		o.ReservedMB = float64(reserved.Int64) / pagesPerMB
		o.UsedMB = float64(used.Int64) / pagesPerMB
		o.Rows = rowCount.Int64
		return o, nil
	})
}

// TempDBSession is one session's tempdb footprint, net of what it has
// deallocated, including what its running tasks hold and have not yet
// handed to the session's totals.
type TempDBSession struct {
	SessionID  int
	Host       string
	Program    string
	Login      string
	UserMB     float64
	InternalMB float64
	TotalMB    float64
}

// TempDBSessions lists the sessions holding tempdb space, largest first.
// Sessions holding nothing are left out.
//
// What a session's running tasks hold is added in: a batch's allocations stay
// in dm_db_task_space_usage until it finishes, so the session totals alone
// show nothing for a long-running query filling tempdb right now.
func (s *Server) TempDBSessions(ctx context.Context) ([]TempDBSession, error) {
	rows, err := s.query(ctx, `
SELECT su.session_id,
       ISNULL(s.host_name, ''), ISNULL(s.program_name, ''), ISNULL(s.login_name, ''),
       CAST(su.user_objects_alloc_page_count - su.user_objects_dealloc_page_count
            + ISNULL(tu.user_alloc, 0) - ISNULL(tu.user_dealloc, 0) AS bigint),
       CAST(su.internal_objects_alloc_page_count - su.internal_objects_dealloc_page_count
            + ISNULL(tu.internal_alloc, 0) - ISNULL(tu.internal_dealloc, 0) AS bigint)
FROM   sys.dm_db_session_space_usage su
JOIN   sys.dm_exec_sessions s ON s.session_id = su.session_id
LEFT   JOIN (
    SELECT session_id,
           SUM(user_objects_alloc_page_count) AS user_alloc,
           SUM(user_objects_dealloc_page_count) AS user_dealloc,
           SUM(internal_objects_alloc_page_count) AS internal_alloc,
           SUM(internal_objects_dealloc_page_count) AS internal_dealloc
    FROM   sys.dm_db_task_space_usage
    GROUP  BY session_id
) tu ON tu.session_id = su.session_id
WHERE  su.user_objects_alloc_page_count - su.user_objects_dealloc_page_count
     + su.internal_objects_alloc_page_count - su.internal_objects_dealloc_page_count
     + ISNULL(tu.user_alloc, 0) - ISNULL(tu.user_dealloc, 0)
     + ISNULL(tu.internal_alloc, 0) - ISNULL(tu.internal_dealloc, 0) > 0
ORDER  BY su.user_objects_alloc_page_count - su.user_objects_dealloc_page_count
     + su.internal_objects_alloc_page_count - su.internal_objects_dealloc_page_count
     + ISNULL(tu.user_alloc, 0) - ISNULL(tu.user_dealloc, 0)
     + ISNULL(tu.internal_alloc, 0) - ISNULL(tu.internal_dealloc, 0) DESC`)
	return scanRows(rows, err, "list tempdb sessions", func(scan func(...any) error) (TempDBSession, error) {
		var t TempDBSession
		var userPages, internalPages int64
		if err := scan(&t.SessionID, &t.Host, &t.Program, &t.Login, &userPages, &internalPages); err != nil {
			return t, err
		}
		// Clamped at zero: summing session and task usage can briefly go
		// negative when a task releases pages its session already counted.
		t.UserMB = nonNegativeMB(userPages)
		t.InternalMB = nonNegativeMB(internalPages)
		t.TotalMB = t.UserMB + t.InternalMB
		return t, nil
	})
}

func nonNegativeMB(pages int64) float64 {
	if pages <= 0 {
		return 0
	}
	return float64(pages) / pagesPerMB
}
