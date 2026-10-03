package gosmo

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// ============================================================
// Server activity
// ============================================================
//
// The instance-wide DMV readings an activity monitor samples: performance
// counters, waits, file I/O, memory clerks, request counts and host CPU
// (scheduler load is on Schedulers, server_config.go; tempdb usage is in
// tempdb_usage.go). Each is one round trip returning the server's own
// cumulative or current figures. None derives a rate: a per-second figure
// needs two readings and the time between them, which is the caller's to keep.
//
// Every read here needs VIEW SERVER STATE. Without it a read is either
// refused or narrowed to the caller's own session, and a narrowed one looks
// like an idle server — ask HasViewServerState first.

// HasViewServerState reports whether the connected login holds VIEW SERVER
// STATE, the right every read in this file and tempdb_usage.go needs. It is
// asked rather than inferred from a read, because some reads without it come
// back narrowed rather than refused.
func (s *Server) HasViewServerState(ctx context.Context) (bool, error) {
	var ok int
	err := s.queryRowScan(ctx,
		`SELECT CASE WHEN HAS_PERMS_BY_NAME(NULL, NULL, 'VIEW SERVER STATE') = 1 THEN 1 ELSE 0 END`,
		nil, &ok)
	if err != nil {
		return false, fmt.Errorf("gosmo: check VIEW SERVER STATE: %w", err)
	}
	return ok == 1, nil
}

// CounterType is a performance counter's cntr_type: the Windows PERF_* type
// that says how its cntr_value is read. Most values are wrong taken raw — a
// cumulative counter only grows, and a fraction needs its base row.
type CounterType int

const (
	// CounterRawCount (PERF_COUNTER_LARGE_RAWCOUNT) is a gauge: the value is
	// the reading.
	CounterRawCount CounterType = 65792
	// CounterBulkCount (PERF_COUNTER_BULK_COUNT) is cumulative: a per-second
	// rate is the change between two readings over the seconds between them.
	CounterBulkCount CounterType = 272696576
	// CounterCounter (PERF_COUNTER_COUNTER) is read as CounterBulkCount.
	CounterCounter CounterType = 272696320
	// CounterRawFraction (PERF_LARGE_RAW_FRACTION) is a ratio: the value over
	// its CounterRawBase row.
	CounterRawFraction CounterType = 537003264
	// CounterAverageBulk (PERF_AVERAGE_BULK) is an average: the change in the
	// value over the change in its CounterRawBase row.
	CounterAverageBulk CounterType = 1073874176
	// CounterRawBase (PERF_LARGE_RAW_BASE) is the divisor of a
	// CounterRawFraction or CounterAverageBulk, published as a row of its own
	// named after it plus " base" (spelling and case vary).
	CounterRawBase CounterType = 1073939712
)

// PerformanceCounter is one row of sys.dm_os_performance_counters.
type PerformanceCounter struct {
	// Object is object_name without its instance prefix: a default instance
	// publishes "SQLServer:Buffer Manager" and a named one
	// "MSSQL$INST:Buffer Manager", and both are "Buffer Manager" here, so a
	// caller matching on it works on either.
	Object string
	// Counter is counter_name and Instance instance_name (a database name,
	// "_Total", or empty), both with SQL Server's blank padding trimmed.
	Counter  string
	Instance string
	Value    int64
	Type     CounterType
}

// PerformanceCounters reads the performance counters named in counters,
// limited to the instance names in instances. Either list empty means no
// limit on that column.
//
// The instance limit is worth giving: the Databases object publishes a row
// per database and Plan Cache one per cache type, so an unlimited read on a
// 200-database server returns about a thousand rows where a caller reading
// only "" and "_Total" uses forty.
func (s *Server) PerformanceCounters(ctx context.Context, counters, instances []string) ([]PerformanceCounter, error) {
	var b strings.Builder
	b.WriteString(`
SELECT RTRIM(object_name), RTRIM(counter_name), RTRIM(instance_name), cntr_value, cntr_type
FROM   sys.dm_os_performance_counters
WHERE  1 = 1`)
	var args []any
	in := func(column string, values []string) {
		if len(values) == 0 {
			return
		}
		b.WriteString("\n  AND  RTRIM(" + column + ") IN (")
		for i, v := range values {
			if i > 0 {
				b.WriteString(", ")
			}
			args = append(args, v)
			fmt.Fprintf(&b, "@p%d", len(args))
		}
		b.WriteString(")")
	}
	in("counter_name", counters)
	in("instance_name", instances)

	rows, err := s.query(ctx, b.String(), args...)
	return scanRows(rows, err, "read performance counters", func(scan func(...any) error) (PerformanceCounter, error) {
		var c PerformanceCounter
		err := scan(&c.Object, &c.Counter, &c.Instance, &c.Value, &c.Type)
		c.Object = counterObject(c.Object)
		return c, err
	})
}

// counterObject strips object_name's instance prefix, up to the first colon.
func counterObject(object string) string {
	if _, rest, ok := strings.Cut(object, ":"); ok {
		object = rest
	}
	return strings.TrimSpace(object)
}

// WaitStat is one row of sys.dm_os_wait_stats: a wait type's totals since the
// instance started or the statistics were last cleared.
type WaitStat struct {
	WaitType     string
	WaitingTasks int64
	WaitTimeMs   int64
	// SignalWaitTimeMs is the part of WaitTimeMs spent runnable, after the
	// resource was signalled and before a scheduler picked the task up — CPU
	// pressure, whatever the wait type.
	SignalWaitTimeMs int64
	MaxWaitTimeMs    int64
}

// WaitStats returns every wait type's cumulative totals, background and idle
// waits included: which of them to leave out of a picture is the caller's
// judgement, and the list differs between versions.
func (s *Server) WaitStats(ctx context.Context) ([]WaitStat, error) {
	rows, err := s.query(ctx, `
SELECT wait_type, waiting_tasks_count, wait_time_ms, signal_wait_time_ms, max_wait_time_ms
FROM   sys.dm_os_wait_stats`)
	return scanRows(rows, err, "read wait statistics", func(scan func(...any) error) (WaitStat, error) {
		var w WaitStat
		err := scan(&w.WaitType, &w.WaitingTasks, &w.WaitTimeMs, &w.SignalWaitTimeMs, &w.MaxWaitTimeMs)
		return w, err
	})
}

// FileIOStat is one database file's cumulative I/O since the database came
// online, from sys.dm_io_virtual_file_stats.
type FileIOStat struct {
	DatabaseID int
	FileID     int
	// Database is empty for a database the login cannot see; its I/O is still
	// reported.
	Database string
	IsLog    bool

	Reads          int64
	BytesRead      int64
	IOStallReadMs  int64
	Writes         int64
	BytesWritten   int64
	IOStallWriteMs int64
}

// FileIOStats returns every database file's cumulative I/O. A file's counters
// restart when its database is detached and reattached, or the instance
// restarts, so a caller differencing two readings must expect them to go
// backwards.
func (s *Server) FileIOStats(ctx context.Context) ([]FileIOStat, error) {
	rows, err := s.query(ctx, `
SELECT vfs.database_id, vfs.file_id, ISNULL(DB_NAME(vfs.database_id), N''),
       CASE WHEN mf.type = 1 THEN 1 ELSE 0 END,
       vfs.num_of_reads, vfs.num_of_bytes_read, vfs.io_stall_read_ms,
       vfs.num_of_writes, vfs.num_of_bytes_written, vfs.io_stall_write_ms
FROM   sys.dm_io_virtual_file_stats(NULL, NULL) AS vfs
LEFT   JOIN sys.master_files AS mf
         ON mf.database_id = vfs.database_id AND mf.file_id = vfs.file_id`)
	return scanRows(rows, err, "read file I/O statistics", func(scan func(...any) error) (FileIOStat, error) {
		var f FileIOStat
		err := scan(&f.DatabaseID, &f.FileID, &f.Database, &f.IsLog,
			&f.Reads, &f.BytesRead, &f.IOStallReadMs,
			&f.Writes, &f.BytesWritten, &f.IOStallWriteMs)
		return f, err
	})
}

// MemoryClerk is one memory clerk type's total, summed over the NUMA nodes
// sys.dm_os_memory_clerks lists it under.
type MemoryClerk struct {
	// Type is the clerk type, e.g. "MEMORYCLERK_SQLBUFFERPOOL" or
	// "CACHESTORE_SQLCP". New releases add types.
	Type string
	MB   float64
}

// MemoryClerks returns the memory each clerk type holds, in no particular
// order.
func (s *Server) MemoryClerks(ctx context.Context) ([]MemoryClerk, error) {
	rows, err := s.query(ctx, `
SELECT type, SUM(pages_kb) / 1024.0
FROM   sys.dm_os_memory_clerks
GROUP  BY type`)
	return scanRows(rows, err, "read memory clerks", func(scan func(...any) error) (MemoryClerk, error) {
		var c MemoryClerk
		err := scan(&c.Type, &c.MB)
		return c, err
	})
}

// RequestActivity is the instance's current user sessions and requests.
// "User" is is_user_process = 1 throughout; a session_id > 50 cut-off
// disagrees at the edges and would make the request counts inconsistent with
// the session count.
type RequestActivity struct {
	UserSessions int
	// The request counts leave out the connection that read them, which would
	// otherwise always look busy.
	ActiveRequests    int
	RunnableRequests  int
	SuspendedRequests int
	// BlockedRequests are requests waiting on another session
	// (blocking_session_id set).
	BlockedRequests int
}

// RequestActivity counts the user sessions and the requests they are running.
func (s *Server) RequestActivity(ctx context.Context) (RequestActivity, error) {
	var a RequestActivity
	err := s.queryRowScan(ctx, `
SELECT COUNT(DISTINCT s.session_id),
       COUNT(r.session_id),
       ISNULL(SUM(CASE WHEN r.status = 'runnable' THEN 1 ELSE 0 END), 0),
       ISNULL(SUM(CASE WHEN r.status = 'suspended' THEN 1 ELSE 0 END), 0),
       ISNULL(SUM(CASE WHEN r.blocking_session_id <> 0 THEN 1 ELSE 0 END), 0)
FROM   sys.dm_exec_sessions AS s
LEFT   JOIN sys.dm_exec_requests AS r
         ON r.session_id = s.session_id AND r.session_id <> @@SPID
WHERE  s.is_user_process = 1`, nil,
		&a.UserSessions, &a.ActiveRequests, &a.RunnableRequests, &a.SuspendedRequests, &a.BlockedRequests)
	if err != nil {
		return RequestActivity{}, fmt.Errorf("gosmo: read request activity: %w", err)
	}
	return a, nil
}

// HostCPU is the host's busy CPU at the newest scheduler-monitor record,
// split between SQL Server and every other process. It is host-wide, unlike
// a scheduler's load: a server pinned by another process shows only here.
type HostCPU struct {
	SQLServerPercent int
	OtherPercent     int
}

// HostCPU reads the newest RING_BUFFER_SCHEDULER_MONITOR record. SQL Server
// writes one a minute, so readings closer together than that repeat. A
// freshly started instance has none yet, and reads as the zero HostCPU.
func (s *Server) HostCPU(ctx context.Context) (HostCPU, error) {
	// The LIKE narrows to health records before the XML cast, which keeps
	// this cheap enough to sample every few seconds.
	var c HostCPU
	err := s.queryRowScan(ctx, `
WITH CpuUsage AS
(
    SELECT DATEADD(ms, -1 * (osi.ms_ticks - rb.[timestamp]), SYSDATETIME()) AS EventTime,
           x.value('(./Record/SchedulerMonitorEvent/SystemHealth/ProcessUtilization)[1]', 'int') AS SQLServerCPUPercent,
           x.value('(./Record/SchedulerMonitorEvent/SystemHealth/SystemIdle)[1]', 'int') AS SystemIdlePercent
    FROM   sys.dm_os_ring_buffers rb
    CROSS  JOIN sys.dm_os_sys_info osi
    CROSS  APPLY (SELECT CAST(rb.record AS xml)) AS r(x)
    WHERE  rb.ring_buffer_type = N'RING_BUFFER_SCHEDULER_MONITOR'
      AND  rb.record LIKE '%<SystemHealth>%'
)
SELECT TOP (1) SQLServerCPUPercent, 100 - SystemIdlePercent - SQLServerCPUPercent
FROM   CpuUsage
ORDER  BY EventTime DESC`, nil, &c.SQLServerPercent, &c.OtherPercent)
	if errors.Is(err, sql.ErrNoRows) {
		return HostCPU{}, nil
	}
	if err != nil {
		return HostCPU{}, fmt.Errorf("gosmo: read host CPU: %w", err)
	}
	return c, nil
}
