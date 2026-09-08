package gosmo

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// ============================================================
// Azure instance resource statistics
// ============================================================

// ServerResourceStat is one row of sys.server_resource_stats: an Azure SQL
// Managed Instance's own resource accounting for a fixed 15-second window,
// retained about 14 days.
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
// order a chart wants. The TOP is a guard, not a filter: the view holds
// ~14 days at one row per 15 seconds, ~80k rows, and a caller charting it
// wants the recent end.
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
// 14 days at four rows a minute is ~80,640, so this cannot silently truncate.
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
