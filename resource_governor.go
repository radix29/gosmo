package gosmo

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// resource_governor.go covers the user-configurable Resource Governor — the
// sys.resource_governor_* catalog views and their sys.dm_resource_governor_*
// counterparts, SSMS's Management > Resource Governor node. It is not the
// Azure platform's governor that azure_resources.go reads
// (sys.dm_instance_resource_governance): that one is fixed by the service
// tier and nothing about it is configurable.
//
// # Stored versus effective
//
// The catalog views hold the stored configuration; the DMVs hold what is in
// force. The two differ after any change until ALTER RESOURCE GOVERNOR
// RECONFIGURE, which is what ResourceGovernorStatus.IsReconfigurationPending
// reports. The catalog is what an editor shows and what a script is built
// from, so the types here read it; the DMV reads are separate calls.
//
// # Permissions
//
// Probed on 13 and 17 (2026-09-30), and the reason the reads are split the
// way they are:
//
//   - The catalog views are metadata, visible with VIEW ANY DEFINITION. A
//     login without it gets **no rows and no error** — not even from
//     sys.resource_governor_configuration, which otherwise always has one.
//     The internal and default pools always exist, so an empty ResourcePools
//     means "not visible", never "no pools"; ResourceGovernor reports the
//     missing row as a not-found error.
//   - The DMVs need VIEW SERVER STATE (or VIEW SERVER PERFORMANCE STATE on
//     2022 and later) and fail with Msg 300 without it. They are the
//     ResourceGovernorStatus, ResourcePoolStats and WorkloadGroupStats reads,
//     kept apart so a login that can see the configuration but not the
//     runtime still gets the configuration — the ServerAudit.Status split.
//
// # Versions
//
// Everything read here exists on gosmo's 2016 floor, including the IOPS
// columns, cap_cpu_percent, max_outstanding_io_per_volume and the external
// pools. Two workload-group columns are newer and gated:
// request_max_memory_grant_percent_numeric (2019, fractional grant percent)
// and the tempdb-governance pair group_max_tempdb_data_percent /
// group_max_tempdb_data_mb (2025).

// systemResourceGovernorMaxID is the highest id a built-in pool or workload
// group takes: 1 is internal, 2 is default, in both catalogs. The external
// pool catalog has only default, at 2.
const systemResourceGovernorMaxID = 2

// ResourceGovernor is the server's stored Resource Governor configuration, a
// singleton — sys.resource_governor_configuration.
type ResourceGovernor struct {
	server *Server

	IsEnabled bool

	// ClassifierFunctionID is the classifier's object_id in master, 0 when
	// there is none. ClassifierSchema and ClassifierName are its name,
	// resolved in master; both are empty with no classifier.
	ClassifierFunctionID int
	ClassifierSchema     string
	ClassifierName       string

	// MaxOutstandingIOPerVolume is the stored setting; 0 means the server
	// chooses (ResourceGovernorStatus reports the value in force).
	MaxOutstandingIOPerVolume int
}

// Server returns the server the configuration belongs to.
func (rg *ResourceGovernor) Server() *Server { return rg.server }

// ResourceGovernor reads the stored Resource Governor configuration. A login
// without VIEW ANY DEFINITION sees no configuration row, which is reported as
// a not-found error (errors.Is ErrNotFound) — the server always has one.
func (s *Server) ResourceGovernor(ctx context.Context) (*ResourceGovernor, error) {
	rg := &ResourceGovernor{server: s}
	var schema, name sql.NullString
	err := s.queryRowScan(ctx, `
SELECT is_enabled, classifier_function_id,
       OBJECT_SCHEMA_NAME(NULLIF(classifier_function_id, 0), 1),
       OBJECT_NAME(NULLIF(classifier_function_id, 0), 1),
       max_outstanding_io_per_volume
FROM   sys.resource_governor_configuration`, nil,
		&rg.IsEnabled, &rg.ClassifierFunctionID, &schema, &name, &rg.MaxOutstandingIOPerVolume)
	rg.ClassifierSchema, rg.ClassifierName = schema.String, name.String
	return foundRow(rg, err,
		notFoundf("gosmo: resource governor configuration is not visible (VIEW ANY DEFINITION is needed)"),
		"read resource governor configuration")
}

// ResourceGovernorStatus is the Resource Governor configuration in force,
// from sys.dm_resource_governor_configuration.
type ResourceGovernorStatus struct {
	// ClassifierFunctionID is the classifier in force, 0 for none.
	// ClassifierSchema/ClassifierName resolve it in master and are empty when
	// the login cannot see the function.
	ClassifierFunctionID int
	ClassifierSchema     string
	ClassifierName       string

	// IsReconfigurationPending reports stored changes not yet applied by
	// ALTER RESOURCE GOVERNOR RECONFIGURE. It is set by any metadata change,
	// even with the governor disabled, and cleared by DISABLE as well as by
	// RECONFIGURE — DISABLE clears it without applying anything.
	IsReconfigurationPending bool

	// MaxOutstandingIOPerVolume is the effective value — the server's own
	// choice when the stored setting is 0.
	MaxOutstandingIOPerVolume int
}

// ResourceGovernorStatus reads the Resource Governor configuration in force.
// It needs VIEW SERVER STATE (VIEW SERVER PERFORMANCE STATE on 2022 and later)
// and fails without it; ResourceGovernor, the stored configuration, does not.
func (s *Server) ResourceGovernorStatus(ctx context.Context) (*ResourceGovernorStatus, error) {
	st := &ResourceGovernorStatus{}
	var schema, name sql.NullString
	err := s.queryRowScan(ctx, `
SELECT classifier_function_id,
       OBJECT_SCHEMA_NAME(NULLIF(classifier_function_id, 0), 1),
       OBJECT_NAME(NULLIF(classifier_function_id, 0), 1),
       CAST(is_reconfiguration_pending AS bit),
       max_outstanding_io_per_volume
FROM   sys.dm_resource_governor_configuration`, nil,
		&st.ClassifierFunctionID, &schema, &name, &st.IsReconfigurationPending, &st.MaxOutstandingIOPerVolume)
	st.ClassifierSchema, st.ClassifierName = schema.String, name.String
	return foundRow(st, err,
		notFoundf("gosmo: resource governor status is not visible"),
		"read resource governor status")
}

// -- Pool kinds ------------------------------------------------------------------

// poolKind is what a resource pool and an external resource pool differ by in
// the code they share — the DDL noun, the catalog views and their columns,
// the affinity word — so that one implementation reads, creates, alters,
// drops and scripts both. P is the pool type the kind builds.
type poolKind[P rgPool] struct {
	noun         string // "resource pool", in errors and script comments
	ddl          string // "RESOURCE POOL", after CREATE/ALTER/DROP
	view         string // the catalog view, for the script's existence guard
	selectSQL    string // the pool read, view included, no WHERE
	scan         func(*Server, func(...any) error) (P, error)
	affinitySQL  string // pool id, processor group and mask, ordered
	affinityWord string // SCHEDULER or CPU, in AFFINITY ... = (ids)
}

// rgPool is what the shared pool code needs of either pool type.
type rgPool interface {
	*ResourcePool | *ExternalResourcePool
	poolID() int
	poolName() string
	IsSystem() bool
	// affinity returns the stored affinity as parallel group/mask slices.
	affinity() (groups []int, masks []int64)
	addAffinity(group int, mask int64)
}

var resourcePoolKind = &poolKind[*ResourcePool]{
	noun:      "resource pool",
	ddl:       "RESOURCE POOL",
	view:      "sys.resource_governor_resource_pools",
	selectSQL: resourcePoolSelect,
	scan:      scanResourcePool,
	affinitySQL: `
SELECT pool_id, processor_group, scheduler_mask
FROM   sys.resource_governor_resource_pool_affinity
ORDER  BY pool_id, processor_group`,
	affinityWord: "SCHEDULER",
}

var externalPoolKind = &poolKind[*ExternalResourcePool]{
	noun:      "external resource pool",
	ddl:       "EXTERNAL RESOURCE POOL",
	view:      "sys.resource_governor_external_resource_pools",
	selectSQL: externalResourcePoolSelect,
	scan:      scanExternalResourcePool,
	affinitySQL: `
SELECT external_pool_id, processor_group, cpu_mask
FROM   sys.resource_governor_external_resource_pool_affinity
ORDER  BY external_pool_id, processor_group`,
	affinityWord: "CPU",
}

// list reads every pool of the kind, ordered by name, affinity attached.
func (k *poolKind[P]) list(ctx context.Context, s *Server) ([]P, error) {
	what := "list " + k.noun + "s"
	rows, err := s.query(ctx, k.selectSQL+`
ORDER  BY name`)
	pools, err := scanRows(rows, err, what, func(scan func(...any) error) (P, error) {
		return k.scan(s, scan)
	})
	if err != nil {
		return nil, err
	}
	if err := k.attachAffinity(ctx, s, pools, what); err != nil {
		return nil, err
	}
	return pools, nil
}

// byName reads one pool of the kind, affinity attached, or a not-found error.
func (k *poolKind[P]) byName(ctx context.Context, s *Server, name string) (P, error) {
	what := fmt.Sprintf("read %s %q", k.noun, name)
	p, err := readByName(ctx, s, k.scan, k.selectSQL+`
WHERE  name = @p1`, []any{name}, notFoundf("gosmo: %s %q not found", k.noun, name), what)
	if err != nil {
		return nil, err
	}
	if err := k.attachAffinity(ctx, s, []P{p}, what); err != nil {
		return nil, err
	}
	return p, nil
}

// attachAffinity fills each pool's Affinity from one read of the kind's
// affinity view, grouped here rather than queried per pool.
func (k *poolKind[P]) attachAffinity(ctx context.Context, s *Server, pools []P, what string) error {
	if len(pools) == 0 {
		return nil
	}
	byID := make(map[int]P, len(pools))
	for _, p := range pools {
		byID[p.poolID()] = p
	}
	rows, err := s.query(ctx, k.affinitySQL)
	_, err = scanRows(rows, err, what, func(scan func(...any) error) (struct{}, error) {
		var id, group int
		var mask int64
		if err := scan(&id, &group, &mask); err != nil {
			return struct{}{}, err
		}
		if p, ok := byID[id]; ok {
			p.addAffinity(group, mask)
		}
		return struct{}{}, nil
	})
	return err
}

// -- Resource pools --------------------------------------------------------------

// ResourcePool mirrors a row of sys.resource_governor_resource_pools, the
// stored configuration of one pool.
type ResourcePool struct {
	server *Server

	ID   int
	Name string

	MinCPUPercent    int
	MaxCPUPercent    int
	CapCPUPercent    int
	MinMemoryPercent int
	MaxMemoryPercent int
	MinIOPSPerVolume int // 0 is no minimum
	MaxIOPSPerVolume int // 0 is unlimited

	// Affinity is the pool's scheduler affinity, one entry per processor
	// group, from sys.resource_governor_resource_pool_affinity. Empty means
	// AFFINITY SCHEDULER = AUTO — the view has no rows for such a pool.
	Affinity []ResourcePoolAffinity
}

// ResourcePoolAffinity is one processor group's scheduler mask for a pool.
type ResourcePoolAffinity struct {
	ProcessorGroup int
	SchedulerMask  int64
}

// Server returns the server the pool belongs to.
func (p *ResourcePool) Server() *Server { return p.server }

// IsSystem reports whether the pool is one of the two built-in pools,
// internal and default. Neither can be dropped, and internal cannot be
// altered either.
func (p *ResourcePool) IsSystem() bool { return p.ID > 0 && p.ID <= systemResourceGovernorMaxID }

func (p *ResourcePool) poolID() int      { return p.ID }
func (p *ResourcePool) poolName() string { return p.Name }

func (p *ResourcePool) affinity() ([]int, []int64) {
	groups, masks := make([]int, len(p.Affinity)), make([]int64, len(p.Affinity))
	for i, a := range p.Affinity {
		groups[i], masks[i] = a.ProcessorGroup, a.SchedulerMask
	}
	return groups, masks
}

func (p *ResourcePool) addAffinity(group int, mask int64) {
	p.Affinity = append(p.Affinity, ResourcePoolAffinity{ProcessorGroup: group, SchedulerMask: mask})
}

const resourcePoolSelect = `
SELECT pool_id, name, min_cpu_percent, max_cpu_percent, cap_cpu_percent,
       min_memory_percent, max_memory_percent,
       min_iops_per_volume, max_iops_per_volume
FROM   sys.resource_governor_resource_pools`

// ResourcePools returns every resource pool, built-in ones included. An empty
// result means the login cannot see the catalog (VIEW ANY DEFINITION), since
// internal and default always exist.
func (s *Server) ResourcePools(ctx context.Context) ([]*ResourcePool, error) {
	return resourcePoolKind.list(ctx, s)
}

// ResourcePoolByName returns one resource pool with every field populated, or
// a not-found error (errors.Is ErrNotFound) when there is none by that name
// or the login cannot see it.
func (s *Server) ResourcePoolByName(ctx context.Context, name string) (*ResourcePool, error) {
	return resourcePoolKind.byName(ctx, s, name)
}

func scanResourcePool(s *Server, scan func(...any) error) (*ResourcePool, error) {
	p := &ResourcePool{server: s}
	if err := scan(&p.ID, &p.Name, &p.MinCPUPercent, &p.MaxCPUPercent, &p.CapCPUPercent,
		&p.MinMemoryPercent, &p.MaxMemoryPercent,
		&p.MinIOPSPerVolume, &p.MaxIOPSPerVolume); err != nil {
		return nil, err
	}
	return p, nil
}

// ResourcePoolStats is one pool's runtime state, from
// sys.dm_resource_governor_resource_pools. The cumulative counters run from
// StatisticsStartTime, which ALTER RESOURCE GOVERNOR RESET STATISTICS resets.
type ResourcePoolStats struct {
	PoolID              int
	Name                string
	StatisticsStartTime time.Time

	TotalCPUUsageMS int64

	CacheMemoryKB  int64
	UsedMemoryKB   int64
	TargetMemoryKB int64
	MaxMemoryKB    int64

	ActiveMemgrantCount int
	ActiveMemgrantKB    int64
	MemgrantWaiterCount int
	OutOfMemoryCount    int64

	ReadBytesTotal  int64
	WriteBytesTotal int64
}

// ResourcePoolStats returns every pool's runtime state, ordered by pool id.
// It needs VIEW SERVER STATE (VIEW SERVER PERFORMANCE STATE on 2022
// and later); ResourcePools does not. A pool created but not yet applied by
// RECONFIGURE has no row here.
func (s *Server) ResourcePoolStats(ctx context.Context) ([]*ResourcePoolStats, error) {
	rows, err := s.query(ctx, `
SELECT pool_id, name, statistics_start_time, total_cpu_usage_ms,
       cache_memory_kb, used_memory_kb, target_memory_kb, max_memory_kb,
       active_memgrant_count, active_memgrant_kb, memgrant_waiter_count,
       out_of_memory_count, read_bytes_total, write_bytes_total
FROM   sys.dm_resource_governor_resource_pools
ORDER  BY pool_id`)
	return scanRows(rows, err, "list resource pool statistics", func(scan func(...any) error) (*ResourcePoolStats, error) {
		st := &ResourcePoolStats{}
		if err := scan(&st.PoolID, &st.Name, &st.StatisticsStartTime, &st.TotalCPUUsageMS,
			&st.CacheMemoryKB, &st.UsedMemoryKB, &st.TargetMemoryKB, &st.MaxMemoryKB,
			&st.ActiveMemgrantCount, &st.ActiveMemgrantKB, &st.MemgrantWaiterCount,
			&st.OutOfMemoryCount, &st.ReadBytesTotal, &st.WriteBytesTotal); err != nil {
			return nil, err
		}
		return st, nil
	})
}

// -- Workload groups -------------------------------------------------------------

// WorkloadGroup mirrors a row of sys.resource_governor_workload_groups, the
// stored configuration of one group.
type WorkloadGroup struct {
	server *Server

	ID   int
	Name string

	// PoolID/PoolName is the resource pool the group uses;
	// ExternalPoolID/ExternalPoolName the external resource pool.
	PoolID           int
	PoolName         string
	ExternalPoolID   int
	ExternalPoolName string

	// Importance is Low, Medium or High, as the catalog spells it.
	Importance string

	// RequestMaxMemoryGrantPercent is fractional from SQL Server 2019, where
	// the catalog gained request_max_memory_grant_percent_numeric; before
	// that it is the whole-number column.
	RequestMaxMemoryGrantPercent float64
	RequestMaxCPUTimeSec         int // 0 is unlimited
	RequestMemoryGrantTimeoutSec int // 0 is the server's own calculation
	MaxDOP                       int // 0 defers to the server/database setting
	GroupMaxRequests             int // 0 is unlimited

	// GroupMaxTempdbDataPercent and GroupMaxTempdbDataMB are SQL Server
	// 2025's tempdb space governance; nil means unset, and both nil means
	// tempdb use is not limited. Always nil before 2025.
	GroupMaxTempdbDataPercent *float64
	GroupMaxTempdbDataMB      *float64
}

// Server returns the server the workload group belongs to.
func (g *WorkloadGroup) Server() *Server { return g.server }

// IsSystem reports whether the group is one of the two built-in groups,
// internal and default. Neither can be dropped, and internal cannot be
// altered either.
func (g *WorkloadGroup) IsSystem() bool { return g.ID > 0 && g.ID <= systemResourceGovernorMaxID }

// workloadGroupSelect builds the group read for the instance's major. The
// joins resolve pool names in the same round trip; the external pool is a
// LEFT JOIN so a group is never lost to a catalog the login sees only part of.
func (s *Server) workloadGroupSelect() string {
	major := s.serverMajorVersion()
	// request_max_memory_grant_percent_numeric: SQL Server 2019 and later, per
	// the catalog view's documentation. Confirmed absent on 13.0.6500.1 and
	// 14.0.2130.4 and present on 17.0.1135.8; no 2019 or 2022 instance was
	// available to confirm the floor itself. Before it, the whole-number
	// column is the same value.
	return `
SELECT g.group_id, g.name, g.pool_id, p.name, g.external_pool_id, ISNULL(ep.name, N''),
       g.importance,
       ` + colSince(major, SQLServer2019, "g.request_max_memory_grant_percent_numeric", "CAST(g.request_max_memory_grant_percent AS float)") + `,
       g.request_max_cpu_time_sec, g.request_memory_grant_timeout_sec,
       g.max_dop, g.group_max_requests,
       ` + colSince(major, SQLServer2025, "g.group_max_tempdb_data_percent", "CAST(NULL AS float)") + `,
       ` + colSince(major, SQLServer2025, "g.group_max_tempdb_data_mb", "CAST(NULL AS float)") + `
FROM   sys.resource_governor_workload_groups g
JOIN   sys.resource_governor_resource_pools p ON p.pool_id = g.pool_id
LEFT   JOIN sys.resource_governor_external_resource_pools ep
       ON ep.external_pool_id = g.external_pool_id`
}

// WorkloadGroups returns every workload group on the server, ordered by pool
// and then group name. An empty result means the login cannot see the
// catalog, since internal and default always exist.
func (s *Server) WorkloadGroups(ctx context.Context) ([]*WorkloadGroup, error) {
	rows, err := s.query(ctx, s.workloadGroupSelect()+`
ORDER  BY p.name, g.name`)
	return scanRows(rows, err, "list workload groups", func(scan func(...any) error) (*WorkloadGroup, error) {
		return scanWorkloadGroup(s, scan)
	})
}

// WorkloadGroups returns the workload groups that use this pool, ordered by
// name.
func (p *ResourcePool) WorkloadGroups(ctx context.Context) ([]*WorkloadGroup, error) {
	rows, err := p.server.query(ctx, p.server.workloadGroupSelect()+`
WHERE  g.pool_id = @p1
ORDER  BY g.name`, p.ID)
	return scanRows(rows, err, fmt.Sprintf("list workload groups of resource pool %q", p.Name),
		func(scan func(...any) error) (*WorkloadGroup, error) {
			return scanWorkloadGroup(p.server, scan)
		})
}

// WorkloadGroupByName returns one workload group with every field populated,
// or a not-found error (errors.Is ErrNotFound) when there is none by that
// name or the login cannot see it. Group names are unique server-wide, not
// per pool.
func (s *Server) WorkloadGroupByName(ctx context.Context, name string) (*WorkloadGroup, error) {
	return readByName(ctx, s, scanWorkloadGroup, s.workloadGroupSelect()+`
WHERE  g.name = @p1`, []any{name},
		notFoundf("gosmo: workload group %q not found", name), fmt.Sprintf("read workload group %q", name))
}

func scanWorkloadGroup(s *Server, scan func(...any) error) (*WorkloadGroup, error) {
	g := &WorkloadGroup{server: s}
	var tempdbPct, tempdbMB sql.NullFloat64
	if err := scan(&g.ID, &g.Name, &g.PoolID, &g.PoolName, &g.ExternalPoolID, &g.ExternalPoolName,
		&g.Importance, &g.RequestMaxMemoryGrantPercent,
		&g.RequestMaxCPUTimeSec, &g.RequestMemoryGrantTimeoutSec,
		&g.MaxDOP, &g.GroupMaxRequests, &tempdbPct, &tempdbMB); err != nil {
		return nil, err
	}
	if tempdbPct.Valid {
		g.GroupMaxTempdbDataPercent = new(tempdbPct.Float64)
	}
	if tempdbMB.Valid {
		g.GroupMaxTempdbDataMB = new(tempdbMB.Float64)
	}
	return g, nil
}

// WorkloadGroupStats is one workload group's runtime state, from
// sys.dm_resource_governor_workload_groups. The cumulative counters run from
// StatisticsStartTime.
type WorkloadGroupStats struct {
	GroupID             int
	Name                string
	PoolID              int
	StatisticsStartTime time.Time

	TotalRequestCount       int64
	TotalQueuedRequestCount int64
	ActiveRequestCount      int
	QueuedRequestCount      int

	TotalCPUUsageMS             int64
	MaxRequestCPUTimeMS         int64
	TotalCPULimitViolationCount int64
	BlockedTaskCount            int
	ActiveParallelThreadCount   int64

	// EffectiveMaxDOP is the degree of parallelism actually in force for
	// the group's requests.
	EffectiveMaxDOP int
}

// WorkloadGroupStats returns every workload group's runtime state, ordered by
// pool and group id. It needs VIEW SERVER STATE (VIEW SERVER PERFORMANCE STATE
// on 2022 and later); WorkloadGroups does not.
func (s *Server) WorkloadGroupStats(ctx context.Context) ([]*WorkloadGroupStats, error) {
	rows, err := s.query(ctx, `
SELECT group_id, name, pool_id, statistics_start_time,
       total_request_count, total_queued_request_count,
       active_request_count, queued_request_count,
       total_cpu_usage_ms, max_request_cpu_time_ms, total_cpu_limit_violation_count,
       blocked_task_count, active_parallel_thread_count, effective_max_dop
FROM   sys.dm_resource_governor_workload_groups
ORDER  BY pool_id, group_id`)
	return scanRows(rows, err, "list workload group statistics", func(scan func(...any) error) (*WorkloadGroupStats, error) {
		st := &WorkloadGroupStats{}
		if err := scan(&st.GroupID, &st.Name, &st.PoolID, &st.StatisticsStartTime,
			&st.TotalRequestCount, &st.TotalQueuedRequestCount,
			&st.ActiveRequestCount, &st.QueuedRequestCount,
			&st.TotalCPUUsageMS, &st.MaxRequestCPUTimeMS, &st.TotalCPULimitViolationCount,
			&st.BlockedTaskCount, &st.ActiveParallelThreadCount, &st.EffectiveMaxDOP); err != nil {
			return nil, err
		}
		return st, nil
	})
}

// -- External resource pools -----------------------------------------------------

// ExternalResourcePool mirrors a row of
// sys.resource_governor_external_resource_pools — a pool governing the
// external runtimes (Machine Learning Services) rather than the engine. The
// catalog has only the default external pool (id 2) until one is created;
// there is no internal one.
type ExternalResourcePool struct {
	server *Server

	ID   int
	Name string

	MaxCPUPercent    int
	MaxMemoryPercent int
	MaxProcesses     int // 0 is unlimited

	// Version is the catalog's internal version counter for the pool's
	// configuration.
	Version int64

	// Affinity is the pool's CPU affinity, one entry per processor group,
	// from sys.resource_governor_external_resource_pool_affinity. Empty
	// means AFFINITY CPU = AUTO.
	Affinity []ExternalResourcePoolAffinity
}

// ExternalResourcePoolAffinity is one processor group's CPU mask for an
// external pool.
type ExternalResourcePoolAffinity struct {
	ProcessorGroup int
	CPUMask        int64
}

// Server returns the server the external pool belongs to.
func (p *ExternalResourcePool) Server() *Server { return p.server }

// IsSystem reports whether the pool is the built-in default external pool,
// which can be altered but not dropped.
func (p *ExternalResourcePool) IsSystem() bool {
	return p.ID > 0 && p.ID <= systemResourceGovernorMaxID
}

func (p *ExternalResourcePool) poolID() int      { return p.ID }
func (p *ExternalResourcePool) poolName() string { return p.Name }

func (p *ExternalResourcePool) affinity() ([]int, []int64) {
	groups, masks := make([]int, len(p.Affinity)), make([]int64, len(p.Affinity))
	for i, a := range p.Affinity {
		groups[i], masks[i] = a.ProcessorGroup, a.CPUMask
	}
	return groups, masks
}

func (p *ExternalResourcePool) addAffinity(group int, mask int64) {
	p.Affinity = append(p.Affinity, ExternalResourcePoolAffinity{ProcessorGroup: group, CPUMask: mask})
}

const externalResourcePoolSelect = `
SELECT external_pool_id, name, max_cpu_percent, max_memory_percent, max_processes, version
FROM   sys.resource_governor_external_resource_pools`

// ExternalResourcePools returns every external resource pool. An empty result
// means the login cannot see the catalog, since default always exists.
func (s *Server) ExternalResourcePools(ctx context.Context) ([]*ExternalResourcePool, error) {
	return externalPoolKind.list(ctx, s)
}

// ExternalResourcePoolByName returns one external resource pool with every
// field populated, or a not-found error (errors.Is ErrNotFound).
func (s *Server) ExternalResourcePoolByName(ctx context.Context, name string) (*ExternalResourcePool, error) {
	return externalPoolKind.byName(ctx, s, name)
}

func scanExternalResourcePool(s *Server, scan func(...any) error) (*ExternalResourcePool, error) {
	p := &ExternalResourcePool{server: s}
	if err := scan(&p.ID, &p.Name, &p.MaxCPUPercent, &p.MaxMemoryPercent, &p.MaxProcesses, &p.Version); err != nil {
		return nil, err
	}
	return p, nil
}
