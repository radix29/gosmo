package gosmo

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// ============================================================
// Azure instance resource statistics
// ============================================================

// ServerResourceStat is one row of sys.server_resource_stats: an Azure SQL
// Managed Instance's own resource accounting for a fixed 15-second window.
//
// Retention is documented as ~14 days but is not what a live instance holds:
// t-qmi-01 on 2026-09-09 had 927 rows over three days with two multi-hour
// holes in them. The 15-second cadence is exact *within* a run, so a caller
// must plot against EndTime and must not assume row n follows row n-1 by 15
// seconds.
//
// It is a pre-aggregated history, not a counter to be sampled — every value
// is already an average or a total over [StartTime, EndTime), so a caller
// plots it directly and must not run it through a per-second delta.
//
// SKU, HardwareGeneration, VirtualCoreCount, ReservedStorageMB and
// StorageSpaceUsedMB repeat on every row; the newest row is the instance's
// current shape, which is what LatestServerResourceStats returns.
type ServerResourceStat struct {
	StartTime time.Time
	EndTime   time.Time
	// ResourceType is "SQL managed instance"; ResourceName is the instance's
	// short name, e.g. "t-qmi-01".
	ResourceType string
	ResourceName string
	// SKU is the service tier ("GeneralPurpose", "BusinessCritical"), and
	// HardwareGeneration the compute generation ("Gen5").
	SKU                string
	HardwareGeneration string
	VirtualCoreCount   int
	AvgCPUPercent      float64
	// ReservedStorageMB is the storage the instance is provisioned for and
	// StorageSpaceUsedMB what it has used — the pair that actually governs a
	// Managed Instance, and the one to display in place of
	// sys.dm_os_volume_stats, whose total_bytes there is the container's
	// 192 MB system volume rather than the instance's quota.
	ReservedStorageMB  int64
	StorageSpaceUsedMB float64
	IORequests         int64
	IOBytesRead        int64
	IOBytesWritten     int64
}

// serverResourceStatsQuery reads sys.server_resource_stats oldest-first, the
// order a chart wants. The TOP is a guard, not a filter: the view is
// documented to hold ~14 days at one row per 15 seconds, ~80k rows, and a
// caller charting it wants the recent end.
const serverResourceStatsQuery = `
SELECT start_time, end_time, resource_type, resource_name, sku,
       hardware_generation, virtual_core_count, avg_cpu_percent,
       reserved_storage_mb, storage_space_used_mb,
       io_requests, io_bytes_read, io_bytes_written
FROM   (SELECT TOP (@p1) * FROM sys.server_resource_stats
        ORDER BY end_time DESC) r
ORDER BY end_time`

// ServerResourceStats returns the most recent max rows of
// sys.server_resource_stats, oldest first.
func (s *Server) ServerResourceStats(max int) ([]*ServerResourceStat, error) {
	return s.ServerResourceStatsContext(context.Background(), max)
}

// ServerResourceStatsContext is the context-aware variant of
// ServerResourceStats. max caps how far back the read reaches; a max of 0 or
// less means the whole retained history.
//
// The view exists only on an Azure engine edition, so this refuses anywhere
// else with an ErrUnsupportedVersion error rather than letting the server
// answer with an "invalid object name".
func (s *Server) ServerResourceStatsContext(ctx context.Context, max int) ([]*ServerResourceStat, error) {
	if !s.info.IsAzure() {
		return nil, unsupportedVersionf("gosmo: server resource stats: sys.server_resource_stats requires an Azure SQL Managed Instance")
	}
	if max <= 0 {
		max = serverResourceStatsAll
	}

	rows, err := s.query(ctx, serverResourceStatsQuery, max)
	if err != nil {
		return nil, fmt.Errorf("gosmo: server resource stats: %w", err)
	}
	defer rows.Close()

	var out []*ServerResourceStat
	for rows.Next() {
		st := &ServerResourceStat{}
		// Every column but the two timestamps is nullable in practice: a
		// window the instance was restarting through reports its shape and
		// nothing else.
		var resType, resName, sku, hw sql.NullString
		var cores sql.NullInt64
		var cpu, used sql.NullFloat64
		var reserved, ioReq, ioRead, ioWrite sql.NullInt64
		if err := rows.Scan(&st.StartTime, &st.EndTime, &resType, &resName, &sku, &hw,
			&cores, &cpu, &reserved, &used, &ioReq, &ioRead, &ioWrite); err != nil {
			return nil, fmt.Errorf("gosmo: server resource stats: %w", err)
		}
		st.ResourceType, st.ResourceName = resType.String, resName.String
		st.SKU, st.HardwareGeneration = sku.String, hw.String
		st.VirtualCoreCount = int(cores.Int64)
		st.AvgCPUPercent, st.StorageSpaceUsedMB = cpu.Float64, used.Float64
		st.ReservedStorageMB = reserved.Int64
		st.IORequests, st.IOBytesRead, st.IOBytesWritten = ioReq.Int64, ioRead.Int64, ioWrite.Int64
		out = append(out, st)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("gosmo: server resource stats: %w", err)
	}
	return out, nil
}

// serverResourceStatsAll is the row cap standing in for "everything retained":
// 14 days at four rows a minute is ~80,640, so this cannot silently truncate
// even at the documented retention, which is more than any observed instance
// actually keeps.
const serverResourceStatsAll = 100000

// LatestServerResourceStats returns the newest sys.server_resource_stats row.
func (s *Server) LatestServerResourceStats() (*ServerResourceStat, error) {
	return s.LatestServerResourceStatsContext(context.Background())
}

// LatestServerResourceStatsContext is the context-aware variant of
// LatestServerResourceStats — the instance's current SKU, core count and
// storage quota in one row, for a caller that wants the shape rather than the
// history.
//
// It returns ErrNotFound when the view is empty, which a freshly created
// instance is until its first 15-second window closes.
func (s *Server) LatestServerResourceStatsContext(ctx context.Context) (*ServerResourceStat, error) {
	stats, err := s.ServerResourceStatsContext(ctx, 1)
	if err != nil {
		return nil, err
	}
	if len(stats) == 0 {
		return nil, notFoundf("gosmo: server resource stats: sys.server_resource_stats is empty")
	}
	return stats[0], nil
}

// ============================================================
// Azure instance resource governance
// ============================================================

// InstanceResourceGovernance is the single row of
// sys.dm_instance_resource_governance: the fixed limits the Azure SQL Managed
// Instance's resource governor enforces on the whole instance.
//
// These are ceilings, not readings — they change only when the instance is
// resized, which is what makes them the scale a ServerResourceStat history is
// read against. Every field is nullable in the view; an unset one reads as
// zero, and the empty string for ServerName.
type InstanceResourceGovernance struct {
	// ServerName is the instance's name as the governor knows it.
	ServerName string

	// CapCPU is instance_cap_cpu, the percentage of a vCore's throughput the
	// instance may use — 100 on a General Purpose instance that is not
	// throttled below its purchased cores.
	CapCPU int
	// MaxLogRate is instance_max_log_rate in bytes per second, the transaction
	// log throughput ceiling. It is the limit a bulk load hits first on
	// General Purpose.
	MaxLogRate int64
	// MaxWorkerThreads is instance_max_worker_threads.
	MaxWorkerThreads int

	// LocalIOPS, ManagedXStoreIOPS and ExternalXStoreIOPS are
	// volume_local_iops / volume_managed_xstore_iops /
	// volume_external_xstore_iops: the IOPS ceiling of each storage class the
	// instance can place a file on.
	LocalIOPS          int
	ManagedXStoreIOPS  int
	ExternalXStoreIOPS int

	// LocalMaxOutstandingIO, ManagedXStoreMaxOutstandingIO and
	// ExternalXStoreMaxOutstandingIO are the queue depth allowed against each
	// of those. The view spells the columns `..._max_oustanding_io`, missing
	// the first `t` — Microsoft's typo, kept in the query because the column
	// is named that on the server.
	LocalMaxOutstandingIO          int
	ManagedXStoreMaxOutstandingIO  int
	ExternalXStoreMaxOutstandingIO int

	// TempDBLogFileNumber is tempdb_log_file_number.
	TempDBLogFileNumber int
	// DataDirectoryQuotaMB and DataDirectoryUsageMB are
	// user_data_directory_space_quota_mb / _usage_mb — the *file directory's*
	// limit, which on General Purpose is far larger than the storage the
	// instance is billed for. ServerResourceStat's ReservedStorageMB /
	// StorageSpaceUsedMB is the pair that actually governs; these two say how
	// much room the directory itself has.
	DataDirectoryQuotaMB int
	DataDirectoryUsageMB int
	// BufferPoolExtensionSizeGB is bufferpool_extension_size_gb, 0 when the
	// extension is off.
	BufferPoolExtensionSizeGB int
}

const instanceResourceGovernanceQuery = `
SELECT server_name, instance_cap_cpu, instance_max_log_rate,
       instance_max_worker_threads,
       volume_local_iops, volume_managed_xstore_iops, volume_external_xstore_iops,
       volume_local_max_oustanding_io, volume_managed_xstore_max_oustanding_io,
       volume_external_xstore_max_oustanding_io,
       tempdb_log_file_number,
       user_data_directory_space_quota_mb, user_data_directory_space_usage_mb,
       bufferpool_extension_size_gb
FROM   sys.dm_instance_resource_governance`

// InstanceResourceGovernance returns the instance's resource-governor limits.
func (s *Server) InstanceResourceGovernance() (*InstanceResourceGovernance, error) {
	return s.InstanceResourceGovernanceContext(context.Background())
}

// InstanceResourceGovernanceContext is the context-aware variant of
// InstanceResourceGovernance.
//
// The view exists only on an Azure engine edition, so this refuses anywhere
// else with an ErrUnsupportedVersion error rather than letting the server
// answer with an "invalid object name", and returns ErrNotFound in the
// (unobserved) case of an empty view.
func (s *Server) InstanceResourceGovernanceContext(ctx context.Context) (*InstanceResourceGovernance, error) {
	if !s.info.IsAzure() {
		return nil, unsupportedVersionf("gosmo: instance resource governance: sys.dm_instance_resource_governance requires an Azure SQL Managed Instance")
	}
	rows, err := s.query(ctx, instanceResourceGovernanceQuery)
	if err != nil {
		return nil, fmt.Errorf("gosmo: instance resource governance: %w", err)
	}
	defer rows.Close()

	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("gosmo: instance resource governance: %w", err)
		}
		return nil, notFoundf("gosmo: instance resource governance: sys.dm_instance_resource_governance is empty")
	}
	g := &InstanceResourceGovernance{}
	var name sql.NullString
	var capCPU, maxWorkers, localIOPS, mgdIOPS, extIOPS sql.NullInt64
	var localIO, mgdIO, extIO, tempdbLogs, quota, usage, bpe sql.NullInt64
	var maxLogRate sql.NullInt64
	if err := rows.Scan(&name, &capCPU, &maxLogRate, &maxWorkers,
		&localIOPS, &mgdIOPS, &extIOPS,
		&localIO, &mgdIO, &extIO,
		&tempdbLogs, &quota, &usage, &bpe); err != nil {
		return nil, fmt.Errorf("gosmo: instance resource governance: %w", err)
	}
	g.ServerName = name.String
	g.CapCPU, g.MaxLogRate = int(capCPU.Int64), maxLogRate.Int64
	g.MaxWorkerThreads = int(maxWorkers.Int64)
	g.LocalIOPS, g.ManagedXStoreIOPS, g.ExternalXStoreIOPS = int(localIOPS.Int64), int(mgdIOPS.Int64), int(extIOPS.Int64)
	g.LocalMaxOutstandingIO, g.ManagedXStoreMaxOutstandingIO, g.ExternalXStoreMaxOutstandingIO =
		int(localIO.Int64), int(mgdIO.Int64), int(extIO.Int64)
	g.TempDBLogFileNumber = int(tempdbLogs.Int64)
	g.DataDirectoryQuotaMB, g.DataDirectoryUsageMB = int(quota.Int64), int(usage.Int64)
	g.BufferPoolExtensionSizeGB = int(bpe.Int64)
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("gosmo: instance resource governance: %w", err)
	}
	return g, nil
}

// ============================================================
// The engine's Windows job object
// ============================================================

// OSJobObject is the single row of sys.dm_os_job_object: the Windows job
// object the SQL Server process runs inside on an Azure SQL Managed Instance,
// and the memory and CPU limits that job object imposes.
//
// It is the layer *below* the resource governor — the governor's limits are
// what SQL Server enforces on itself, these are what the host enforces on
// SQL Server — which is why an instance can be under its governor limits and
// still be squeezed.
//
// Every field is nullable in the view (WorkingSetLimitMB is NULL on a live
// General Purpose instance), and an unset one reads as zero.
type OSJobObject struct {
	// CPURate is cpu_rate, the job object's CPU allocation in units of
	// 1/10000 of a single processor's capacity — 400 on a 4 vCore instance.
	CPURate int
	// CPUAffinityMask and CPUAffinityGroup are the processors the job object
	// may run on.
	CPUAffinityMask  int64
	CPUAffinityGroup int

	// MemoryLimitMB, ProcessMemoryLimitMB and WorkingSetLimitMB are the job
	// object's three memory ceilings; LowMemorySignalThresholdMB is where the
	// host starts signalling memory pressure.
	MemoryLimitMB           int64
	ProcessMemoryLimitMB    int64
	WorkingSetLimitMB       int64
	LowMemSignalThresholdMB int64
	NonSOSMemGapMB          int64
	PeakProcessMemoryUsedMB int64
	PeakJobMemoryUsedMB     int64

	// TotalUserTime and TotalKernelTime are cumulative CPU time in
	// 100-nanosecond units, the Windows FILETIME tick — divide by 10,000,000
	// for seconds.
	TotalUserTime   int64
	TotalKernelTime int64

	// ReadOperationCount and WriteOperationCount are cumulative IO operation
	// counts for the whole job object.
	ReadOperationCount  int64
	WriteOperationCount int64
}

const osJobObjectQuery = `
SELECT cpu_rate, cpu_affinity_mask, cpu_affinity_group,
       memory_limit_mb, process_memory_limit_mb, workingset_limit_mb,
       low_mem_signal_threshold_mb, non_sos_mem_gap_mb,
       peak_process_memory_used_mb, peak_job_memory_used_mb,
       total_user_time, total_kernel_time,
       read_operation_count, write_operation_count
FROM   sys.dm_os_job_object`

// OSJobObject returns the job object the engine process runs inside.
func (s *Server) OSJobObject() (*OSJobObject, error) {
	return s.OSJobObjectContext(context.Background())
}

// OSJobObjectContext is the context-aware variant of OSJobObject.
//
// The view exists only on an Azure engine edition, so this refuses anywhere
// else with an ErrUnsupportedVersion error, and returns ErrNotFound when the
// view is empty — which is what a hosted engine that is not inside a job
// object reports.
func (s *Server) OSJobObjectContext(ctx context.Context) (*OSJobObject, error) {
	if !s.info.IsAzure() {
		return nil, unsupportedVersionf("gosmo: os job object: sys.dm_os_job_object requires an Azure SQL Managed Instance")
	}
	rows, err := s.query(ctx, osJobObjectQuery)
	if err != nil {
		return nil, fmt.Errorf("gosmo: os job object: %w", err)
	}
	defer rows.Close()

	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("gosmo: os job object: %w", err)
		}
		return nil, notFoundf("gosmo: os job object: sys.dm_os_job_object is empty")
	}
	j := &OSJobObject{}
	var rate, affGroup sql.NullInt64
	var affMask, memLimit, procMem, wsLimit, lowMem, gap, peakProc, peakJob sql.NullInt64
	var userTime, kernelTime, reads, writes sql.NullInt64
	if err := rows.Scan(&rate, &affMask, &affGroup,
		&memLimit, &procMem, &wsLimit, &lowMem, &gap,
		&peakProc, &peakJob, &userTime, &kernelTime, &reads, &writes); err != nil {
		return nil, fmt.Errorf("gosmo: os job object: %w", err)
	}
	j.CPURate, j.CPUAffinityGroup = int(rate.Int64), int(affGroup.Int64)
	j.CPUAffinityMask = affMask.Int64
	j.MemoryLimitMB, j.ProcessMemoryLimitMB, j.WorkingSetLimitMB = memLimit.Int64, procMem.Int64, wsLimit.Int64
	j.LowMemSignalThresholdMB, j.NonSOSMemGapMB = lowMem.Int64, gap.Int64
	j.PeakProcessMemoryUsedMB, j.PeakJobMemoryUsedMB = peakProc.Int64, peakJob.Int64
	j.TotalUserTime, j.TotalKernelTime = userTime.Int64, kernelTime.Int64
	j.ReadOperationCount, j.WriteOperationCount = reads.Int64, writes.Int64
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("gosmo: os job object: %w", err)
	}
	return j, nil
}

// ============================================================
// Azure per-database resource statistics
// ============================================================

// DatabaseResourceStat is one row of sys.dm_db_resource_stats: a single
// database's resource consumption over a fixed 15-second window, retained
// about an hour.
//
// It is the database-scoped counterpart of ServerResourceStat and has the same
// contract — every value is already an average or a maximum over the window,
// so a caller plots it directly rather than differencing it. The view is
// scoped to the database the connection is in, which is why this hangs off
// Database rather than Server.
//
// The row carries no start_time: the window is the 15 seconds ending at
// EndTime. Percentages are of the database's own governed limit, not of the
// instance — AvgInstanceCPUPercent and AvgInstanceMemoryPercent are the two
// that are instance-wide, and are what says whether a quiet database is
// sharing a busy instance.
type DatabaseResourceStat struct {
	EndTime time.Time
	// AvgCPUPercent, AvgDataIOPercent, AvgLogWritePercent and
	// AvgMemoryUsagePercent are averages over the window, as a percentage of
	// the database's limit.
	AvgCPUPercent         float64
	AvgDataIOPercent      float64
	AvgLogWritePercent    float64
	AvgMemoryUsagePercent float64
	// XTPStoragePercent is In-Memory OLTP storage used, as a percentage.
	XTPStoragePercent float64
	// MaxWorkerPercent and MaxSessionPercent are the window's peaks, not
	// averages: the highest concurrent workers and sessions reached, against
	// the database's limits.
	MaxWorkerPercent  float64
	MaxSessionPercent float64
	// DTULimit is the database's DTU allocation. It is NULL on a Managed
	// Instance, which is vCore-based, and so reads as zero there.
	DTULimit int
	// AvgLoginRatePercent is logins against the limit, as a percentage.
	AvgLoginRatePercent float64
	// AvgInstanceCPUPercent and AvgInstanceMemoryPercent are the *instance's*
	// consumption over the same window, which lets a caller tell "this
	// database is busy" from "this instance is busy".
	AvgInstanceCPUPercent    float64
	AvgInstanceMemoryPercent float64
	// CPULimit is the database's vCore allocation.
	CPULimit float64
	// UsedStorageMB and AllocatedStorageMB are the database's data size and
	// the space allocated to hold it.
	UsedStorageMB      int64
	AllocatedStorageMB int64
	// ReplicaRole is the role the replica answering held during the window:
	// 0 primary, 1 secondary, 2 named secondary, 3 geo-replication forwarder.
	ReplicaRole int
}

// databaseResourceStatsQuery reads sys.dm_db_resource_stats oldest-first, the
// order a chart wants. The TOP is a guard, not a filter: the view holds about
// an hour at one row per 15 seconds, ~240 rows.
const databaseResourceStatsQuery = `
SELECT end_time, avg_cpu_percent, avg_data_io_percent, avg_log_write_percent,
       avg_memory_usage_percent, xtp_storage_percent, max_worker_percent,
       max_session_percent, dtu_limit, avg_login_rate_percent,
       avg_instance_cpu_percent, avg_instance_memory_percent, cpu_limit,
       used_storage_mb, allocated_storage_mb, replica_role
FROM   (SELECT TOP (@p1) * FROM sys.dm_db_resource_stats
        ORDER BY end_time DESC) r
ORDER BY end_time`

// databaseResourceStatsAll is the row cap standing in for "everything
// retained": an hour at four rows a minute is ~240, so this cannot silently
// truncate.
const databaseResourceStatsAll = 10000

// ResourceStats returns the most recent max rows of sys.dm_db_resource_stats
// for this database, oldest first.
func (d *Database) ResourceStats(max int) ([]*DatabaseResourceStat, error) {
	return d.ResourceStatsContext(context.Background(), max)
}

// ResourceStatsContext is the context-aware variant of ResourceStats. max caps
// how far back the read reaches; a max of 0 or less means the whole retained
// history, which is about an hour.
//
// The view exists only on an Azure engine edition, so this refuses anywhere
// else with an ErrUnsupportedVersion error rather than letting the server
// answer with an "invalid object name".
func (d *Database) ResourceStatsContext(ctx context.Context, max int) ([]*DatabaseResourceStat, error) {
	if !d.serverInfo().IsAzure() {
		return nil, unsupportedVersionf("gosmo: database resource stats: sys.dm_db_resource_stats requires an Azure SQL Database or Managed Instance")
	}
	if max <= 0 {
		max = databaseResourceStatsAll
	}

	rows, err := d.query(ctx, databaseResourceStatsQuery, max)
	if err != nil {
		return nil, fmt.Errorf("gosmo: database resource stats: %w", err)
	}
	defer rows.Close()

	var out []*DatabaseResourceStat
	for rows.Next() {
		st := &DatabaseResourceStat{}
		// Every column is nullable in the view, including end_time.
		var end sql.NullTime
		var cpu, dataIO, logWrite, mem, xtp, worker, session sql.NullFloat64
		var loginRate, instCPU, instMem, cpuLimit sql.NullFloat64
		var dtu, role sql.NullInt64
		var used, alloc sql.NullInt64
		if err := rows.Scan(&end, &cpu, &dataIO, &logWrite, &mem, &xtp, &worker,
			&session, &dtu, &loginRate, &instCPU, &instMem, &cpuLimit,
			&used, &alloc, &role); err != nil {
			return nil, fmt.Errorf("gosmo: database resource stats: %w", err)
		}
		st.EndTime = end.Time
		st.AvgCPUPercent, st.AvgDataIOPercent = cpu.Float64, dataIO.Float64
		st.AvgLogWritePercent, st.AvgMemoryUsagePercent = logWrite.Float64, mem.Float64
		st.XTPStoragePercent = xtp.Float64
		st.MaxWorkerPercent, st.MaxSessionPercent = worker.Float64, session.Float64
		st.DTULimit = int(dtu.Int64)
		st.AvgLoginRatePercent = loginRate.Float64
		st.AvgInstanceCPUPercent, st.AvgInstanceMemoryPercent = instCPU.Float64, instMem.Float64
		st.CPULimit = cpuLimit.Float64
		st.UsedStorageMB, st.AllocatedStorageMB = used.Int64, alloc.Int64
		st.ReplicaRole = int(role.Int64)
		out = append(out, st)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("gosmo: database resource stats: %w", err)
	}
	return out, nil
}

// LatestResourceStats returns the newest sys.dm_db_resource_stats row for this
// database.
func (d *Database) LatestResourceStats() (*DatabaseResourceStat, error) {
	return d.LatestResourceStatsContext(context.Background())
}

// LatestResourceStatsContext is the context-aware variant of
// LatestResourceStats, for a caller that wants the database's current
// consumption rather than its history.
//
// It returns ErrNotFound when the view is empty, which a database that has
// been idle since the instance last restarted is.
func (d *Database) LatestResourceStatsContext(ctx context.Context) (*DatabaseResourceStat, error) {
	stats, err := d.ResourceStatsContext(ctx, 1)
	if err != nil {
		return nil, err
	}
	if len(stats) == 0 {
		return nil, notFoundf("gosmo: database resource stats: sys.dm_db_resource_stats is empty for %s", d.name)
	}
	return stats[0], nil
}

// serverInfo is the ServerInfo behind this database, or nil when the Database
// was built without one. IsAzure is nil-safe, so callers gate on
// d.serverInfo().IsAzure() directly.
func (d *Database) serverInfo() *ServerInfo {
	if d.server == nil {
		return nil
	}
	return d.server.info
}

// ============================================================
// Azure per-database resource governance
// ============================================================

// UserDBResourceGovernance is one row of sys.dm_user_db_resource_governance:
// the limits the resource governor enforces on a single database, one row per
// database on the instance.
//
// It is the database-scoped counterpart of InstanceResourceGovernance and has
// the same contract — these are ceilings, not readings, and change only when
// the database or the instance is resized. It is the scale a
// DatabaseResourceStat history is read against: the stats view reports
// percentages, and this says of what.
//
// Unlike sys.dm_db_resource_stats, the view is *not* scoped to the connection's
// database: it returns every database on the instance from wherever it is
// read, which is why Server.UserDBResourceGovernance returns them all and
// Database.ResourceGovernance picks one out.
//
// Fields are in view column order. A nullable column reads as zero, or the
// empty string.
type UserDBResourceGovernance struct {
	DatabaseID int
	// LogicalDatabaseGUID and PhysicalDatabaseGUID identify the database
	// across a resize or a failover, which rewrites the physical one. Read as
	// strings: the view types them uniqueidentifier.
	LogicalDatabaseGUID  string
	PhysicalDatabaseGUID string
	ServerName           string
	DatabaseName         string
	// SLOName is the service-level objective the database runs under, e.g.
	// "MIWCOWHE4G5_INTERNAL_NPG5C_4D" on a General Purpose Gen5 4 vCore
	// Managed Instance. It is an internal identifier, not the SKU a user
	// picked — ServerResourceStat.SKU is that.
	SLOName string
	// DTULimit is the DTU allocation, and is meaningless on a vCore-based
	// Managed Instance. CPULimit is the vCore allocation.
	DTULimit int
	CPULimit int
	// MinCPU, MaxCPU and CapCPU are the resource governor's CPU percentages,
	// and MinCores the reserved core count.
	MinCPU    int
	MaxCPU    int
	CapCPU    int
	MinCores  int
	MaxDOP    int
	MinMemory int
	MaxMemory int
	// MaxSessions is the concurrent session ceiling MaxSessionPercent in
	// DatabaseResourceStat is a percentage of.
	MaxSessions        int
	MaxMemoryGrant     int
	MaxDBMemory        int
	GovernBackgroundIO bool
	// MinDBMaxSizeMB, MaxDBMaxSizeMB and DefaultDBMaxSizeMB are all zero on
	// the "Shared" SLO, which is what the instance's internal databases
	// (model_msdb, model_replicatedmaster) run under — not a failed read.
	//
	// MinDBMaxSizeMB, MaxDBMaxSizeMB and DefaultDBMaxSizeMB bound how large
	// the database may be set to grow; DBFileGrowthMB and
	// InitialDBFileSizeMB are the growth increment and starting size the
	// instance places files with, which is why New Database's file fields are
	// withheld on an Azure edition. LogSizeMB is the log's ceiling.
	MinDBMaxSizeMB      int64
	MaxDBMaxSizeMB      int64
	DefaultDBMaxSizeMB  int64
	DBFileGrowthMB      int64
	InitialDBFileSizeMB int64
	LogSizeMB           int64
	// InstanceCapCPU, InstanceMaxLogRate and InstanceMaxWorkerThreads repeat
	// the instance-wide figures from InstanceResourceGovernance, so a
	// per-database read does not need a second query to say what share of the
	// instance a database has.
	InstanceCapCPU           int
	InstanceMaxLogRate       int64
	InstanceMaxWorkerThreads int
	// ReplicaType and ReplicaRole describe the replica this row is for.
	ReplicaType        int
	MaxTransactionSize int64
	CheckpointRateMBps int
	CheckpointRateIO   int
	LastUpdatedUTC     time.Time
	// The primary_* group is the resource pool the primary replica's workload
	// group draws from.
	PrimaryGroupID          int
	PrimaryGroupMaxWorkers  int
	PrimaryMinLogRate       int64
	PrimaryMaxLogRate       int64
	PrimaryGroupMinIO       int
	PrimaryGroupMaxIO       int
	PrimaryGroupMinCPU      float64
	PrimaryGroupMaxCPU      float64
	PrimaryLogCommitFee     int
	PrimaryPoolMaxWorkers   int
	PoolMaxIO               int
	GovernDBMemoryInPool    bool
	LocalIOPS               int
	ManagedXStoreIOPS       int
	ExternalXStoreIOPS      int
	TypeLocalIOPS           int
	TypeManagedXStoreIOPS   int
	TypeExternalXStoreIOPS  int
	PFSIOPS                 int
	TypePFSIOPS             int
	DataDirectoryQuotaMB    int
	DataDirectoryUsageMB    int
	BufferPoolExtensionGB   int
	PoolMaxLogRate          int64
	PrimaryGroupMaxOutbound int
	PrimaryPoolMaxOutbound  int
	// ReplicaRole is 0 primary, 1 secondary, 2 named secondary, 3 forwarder.
	ReplicaRole      int
	TypeRBIODataIOPS int
}

// userDBResourceGovernanceQuery reads every column of
// sys.dm_user_db_resource_governance in view order. The two uniqueidentifier
// columns are cast to nvarchar rather than scanned raw: the driver hands back
// a byte slice in its own field order otherwise, which is not the string
// anyone reading a GUID expects.
const userDBResourceGovernanceQuery = `
SELECT database_id,
       CAST(logical_database_guid AS NVARCHAR(36)),
       CAST(physical_database_guid AS NVARCHAR(36)),
       server_name, database_name, slo_name, dtu_limit, cpu_limit,
       min_cpu, max_cpu, cap_cpu, min_cores, max_dop, min_memory, max_memory,
       max_sessions, max_memory_grant, max_db_memory, govern_background_io,
       min_db_max_size_in_mb, max_db_max_size_in_mb, default_db_max_size_in_mb,
       db_file_growth_in_mb, initial_db_file_size_in_mb, log_size_in_mb,
       instance_cap_cpu, instance_max_log_rate, instance_max_worker_threads,
       replica_type, max_transaction_size, checkpoint_rate_mbps,
       checkpoint_rate_io, last_updated_date_utc,
       primary_group_id, primary_group_max_workers, primary_min_log_rate,
       primary_max_log_rate, primary_group_min_io, primary_group_max_io,
       primary_group_min_cpu, primary_group_max_cpu, primary_log_commit_fee,
       primary_pool_max_workers, pool_max_io,
       govern_db_memory_in_resource_pool,
       volume_local_iops, volume_managed_xstore_iops,
       volume_external_xstore_iops, volume_type_local_iops,
       volume_type_managed_xstore_iops, volume_type_external_xstore_iops,
       volume_pfs_iops, volume_type_pfs_iops,
       user_data_directory_space_quota_mb, user_data_directory_space_usage_mb,
       bufferpool_extension_size_gb, pool_max_log_rate,
       primary_group_max_outbound_connection_workers,
       primary_pool_max_outbound_connection_workers,
       replica_role, volume_type_rbio_data_iops
FROM   sys.dm_user_db_resource_governance
ORDER BY database_name`

// UserDBResourceGovernance returns one row per database on the instance.
func (s *Server) UserDBResourceGovernance() ([]*UserDBResourceGovernance, error) {
	return s.UserDBResourceGovernanceContext(context.Background())
}

// UserDBResourceGovernanceContext is the context-aware variant of
// UserDBResourceGovernance, ordered by database name.
//
// The view exists only on an Azure engine edition, so this refuses anywhere
// else with an ErrUnsupportedVersion error rather than letting the server
// answer with an "invalid object name". It lists the system databases the
// instance governs (master, model, model_msdb, model_replicatedmaster)
// alongside the user ones, because the view does.
func (s *Server) UserDBResourceGovernanceContext(ctx context.Context) ([]*UserDBResourceGovernance, error) {
	if !s.info.IsAzure() {
		return nil, unsupportedVersionf("gosmo: user db resource governance: sys.dm_user_db_resource_governance requires an Azure SQL Database or Managed Instance")
	}
	rows, err := s.query(ctx, userDBResourceGovernanceQuery)
	if err != nil {
		return nil, fmt.Errorf("gosmo: user db resource governance: %w", err)
	}
	defer rows.Close()

	var out []*UserDBResourceGovernance
	for rows.Next() {
		g, err := scanUserDBResourceGovernance(rows)
		if err != nil {
			return nil, fmt.Errorf("gosmo: user db resource governance: %w", err)
		}
		out = append(out, g)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("gosmo: user db resource governance: %w", err)
	}
	return out, nil
}

// ResourceGovernance returns this database's row of
// sys.dm_user_db_resource_governance.
func (d *Database) ResourceGovernance() (*UserDBResourceGovernance, error) {
	return d.ResourceGovernanceContext(context.Background())
}

// ResourceGovernanceContext is the context-aware variant of
// ResourceGovernance: the limits governing this database, the scale its
// ResourceStats percentages are of.
//
// It returns ErrNotFound when the instance governs no row for the database,
// and an ErrUnsupportedVersion error off an Azure engine edition.
func (d *Database) ResourceGovernanceContext(ctx context.Context) (*UserDBResourceGovernance, error) {
	if !d.serverInfo().IsAzure() {
		return nil, unsupportedVersionf("gosmo: database resource governance: sys.dm_user_db_resource_governance requires an Azure SQL Database or Managed Instance")
	}
	// The view ignores the connection's database, so this filters by name
	// rather than relying on Database.query's USE.
	rows, err := d.query(ctx, userDBResourceGovernanceRowQuery, d.name)
	if err != nil {
		return nil, fmt.Errorf("gosmo: database resource governance: %w", err)
	}
	defer rows.Close()

	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("gosmo: database resource governance: %w", err)
		}
		return nil, notFoundf("gosmo: database resource governance: no row for %s", d.name)
	}
	g, err := scanUserDBResourceGovernance(rows)
	if err != nil {
		return nil, fmt.Errorf("gosmo: database resource governance: %w", err)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("gosmo: database resource governance: %w", err)
	}
	return g, nil
}

// userDBResourceGovernanceRowQuery is the same read narrowed to one database.
var userDBResourceGovernanceRowQuery = strings.Replace(userDBResourceGovernanceQuery,
	"ORDER BY database_name", "WHERE database_name = @p1", 1)

// scanUserDBResourceGovernance reads one row of userDBResourceGovernanceQuery.
// Every column is taken through a sql.Null* destination, in view order:
// nineteen of them are nullable in the view, and the ones that are not today
// are a resize away from being so.
func scanUserDBResourceGovernance(rows interface{ Scan(...any) error }) (*UserDBResourceGovernance, error) {
	g := &UserDBResourceGovernance{}
	var (
		dbID, dtu, cpuLimit                      sql.NullInt64
		logicalGUID, physicalGUID                sql.NullString
		serverName, dbName, slo                  sql.NullString
		minCPU, maxCPU, capCPU, minCores, maxDOP sql.NullInt64
		minMem, maxMem, maxSess, maxGrant, maxDB sql.NullInt64
		governBgIO                               sql.NullBool
		minSize, maxSize, defSize                sql.NullInt64
		growth, initSize, logSize                sql.NullInt64
		instCap, instLogRate, instWorkers        sql.NullInt64
		replType, maxTxn, ckptMBps, ckptIO       sql.NullInt64
		updated                                  sql.NullTime
		pGroupID, pGroupWorkers                  sql.NullInt64
		pMinLog, pMaxLog                         sql.NullInt64
		pMinIO, pMaxIO                           sql.NullInt64
		pMinCPU, pMaxCPU                         sql.NullFloat64
		pCommitFee, pPoolWorkers, poolMaxIO      sql.NullInt64
		governPoolMem                            sql.NullBool
		vLocal, vMgd, vExt                       sql.NullInt64
		vtLocal, vtMgd, vtExt                    sql.NullInt64
		vPFS, vtPFS                              sql.NullInt64
		quota, usage, bpe                        sql.NullInt64
		poolLogRate                              sql.NullInt64
		pGroupOutbound, pPoolOutbound            sql.NullInt64
		replRole, vtRBIO                         sql.NullInt64
	)
	if err := rows.Scan(&dbID, &logicalGUID, &physicalGUID, &serverName, &dbName,
		&slo, &dtu, &cpuLimit,
		&minCPU, &maxCPU, &capCPU, &minCores, &maxDOP, &minMem, &maxMem,
		&maxSess, &maxGrant, &maxDB, &governBgIO,
		&minSize, &maxSize, &defSize, &growth, &initSize, &logSize,
		&instCap, &instLogRate, &instWorkers,
		&replType, &maxTxn, &ckptMBps, &ckptIO, &updated,
		&pGroupID, &pGroupWorkers, &pMinLog, &pMaxLog, &pMinIO, &pMaxIO,
		&pMinCPU, &pMaxCPU, &pCommitFee, &pPoolWorkers, &poolMaxIO,
		&governPoolMem,
		&vLocal, &vMgd, &vExt, &vtLocal, &vtMgd, &vtExt, &vPFS, &vtPFS,
		&quota, &usage, &bpe, &poolLogRate,
		&pGroupOutbound, &pPoolOutbound, &replRole, &vtRBIO); err != nil {
		return nil, err
	}
	g.DatabaseID = int(dbID.Int64)
	g.LogicalDatabaseGUID, g.PhysicalDatabaseGUID = logicalGUID.String, physicalGUID.String
	g.ServerName, g.DatabaseName, g.SLOName = serverName.String, dbName.String, slo.String
	g.DTULimit, g.CPULimit = int(dtu.Int64), int(cpuLimit.Int64)
	g.MinCPU, g.MaxCPU, g.CapCPU = int(minCPU.Int64), int(maxCPU.Int64), int(capCPU.Int64)
	g.MinCores, g.MaxDOP = int(minCores.Int64), int(maxDOP.Int64)
	g.MinMemory, g.MaxMemory = int(minMem.Int64), int(maxMem.Int64)
	g.MaxSessions, g.MaxMemoryGrant, g.MaxDBMemory = int(maxSess.Int64), int(maxGrant.Int64), int(maxDB.Int64)
	g.GovernBackgroundIO = governBgIO.Bool
	g.MinDBMaxSizeMB, g.MaxDBMaxSizeMB, g.DefaultDBMaxSizeMB = minSize.Int64, maxSize.Int64, defSize.Int64
	g.DBFileGrowthMB, g.InitialDBFileSizeMB, g.LogSizeMB = growth.Int64, initSize.Int64, logSize.Int64
	g.InstanceCapCPU, g.InstanceMaxLogRate = int(instCap.Int64), instLogRate.Int64
	g.InstanceMaxWorkerThreads = int(instWorkers.Int64)
	g.ReplicaType, g.MaxTransactionSize = int(replType.Int64), maxTxn.Int64
	g.CheckpointRateMBps, g.CheckpointRateIO = int(ckptMBps.Int64), int(ckptIO.Int64)
	g.LastUpdatedUTC = updated.Time
	g.PrimaryGroupID, g.PrimaryGroupMaxWorkers = int(pGroupID.Int64), int(pGroupWorkers.Int64)
	g.PrimaryMinLogRate, g.PrimaryMaxLogRate = pMinLog.Int64, pMaxLog.Int64
	g.PrimaryGroupMinIO, g.PrimaryGroupMaxIO = int(pMinIO.Int64), int(pMaxIO.Int64)
	g.PrimaryGroupMinCPU, g.PrimaryGroupMaxCPU = pMinCPU.Float64, pMaxCPU.Float64
	g.PrimaryLogCommitFee, g.PrimaryPoolMaxWorkers = int(pCommitFee.Int64), int(pPoolWorkers.Int64)
	g.PoolMaxIO, g.GovernDBMemoryInPool = int(poolMaxIO.Int64), governPoolMem.Bool
	g.LocalIOPS, g.ManagedXStoreIOPS, g.ExternalXStoreIOPS = int(vLocal.Int64), int(vMgd.Int64), int(vExt.Int64)
	g.TypeLocalIOPS, g.TypeManagedXStoreIOPS, g.TypeExternalXStoreIOPS = int(vtLocal.Int64), int(vtMgd.Int64), int(vtExt.Int64)
	g.PFSIOPS, g.TypePFSIOPS = int(vPFS.Int64), int(vtPFS.Int64)
	g.DataDirectoryQuotaMB, g.DataDirectoryUsageMB = int(quota.Int64), int(usage.Int64)
	g.BufferPoolExtensionGB, g.PoolMaxLogRate = int(bpe.Int64), poolLogRate.Int64
	g.PrimaryGroupMaxOutbound, g.PrimaryPoolMaxOutbound = int(pGroupOutbound.Int64), int(pPoolOutbound.Int64)
	g.ReplicaRole, g.TypeRBIODataIOPS = int(replRole.Int64), int(vtRBIO.Int64)
	return g, nil
}
