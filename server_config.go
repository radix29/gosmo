package gosmo

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// ============================================================
// Server Configuration  (sp_configure / sys.configurations)
// ============================================================

// ConfigurationOption mirrors a row from sys.configurations.
type ConfigurationOption struct {
	server      *Server
	ConfigID    int
	Name        string
	Value       int64
	ValueInUse  int64
	Minimum     int64
	Maximum     int64
	IsDynamic   bool // true = change takes effect without a restart
	IsAdvanced  bool
	Description string

	// Default is the option's engine default on this server's major version
	// — the value a bare installation of the engine carries, not Setup's
	// recommendation (max degree of parallelism 0, max server memory
	// 2147483647; see configDefaults for the source). HasDefault is false,
	// and Default 0, where gosmo has no default to give: Azure, a major
	// newer than the table, and an option it does not list (one a CU added,
	// or the engine-written version parts). Configurations and
	// ConfigurationByName fill both; a ConfigurationRef handle leaves them
	// zero.
	//
	// A self-configuring option's default is 0 ("the server decides"),
	// which lies below the Minimum sys.configurations reports for it (locks
	// 5000, max worker threads 128, index create memory (KB) 704) and which
	// sp_configure accepts all the same. A range check must let 0 through.
	Default    int64
	HasDefault bool
}

// Server returns the server the configuration option belongs to.
func (c *ConfigurationOption) Server() *Server { return c.server }

// Configurations returns all server configuration options.
func (s *Server) Configurations(ctx context.Context) ([]*ConfigurationOption, error) {
	const q = `
SELECT configuration_id, name, value, value_in_use,
       minimum, maximum, is_dynamic, is_advanced, description
FROM   sys.configurations
ORDER  BY name`

	rows, err := s.query(ctx, q)
	return scanRows(rows, err, "list configurations", func(scan func(...any) error) (*ConfigurationOption, error) {
		c := &ConfigurationOption{server: s}
		var desc sql.NullString
		if err := scan(
			&c.ConfigID, &c.Name, &c.Value, &c.ValueInUse,
			&c.Minimum, &c.Maximum, &c.IsDynamic, &c.IsAdvanced, &desc,
		); err != nil {
			return nil, err
		}
		c.Description = desc.String
		c.fillDefault()
		return c, nil
	})
}

// ConfigurationByName returns a single option using a direct parameterised query.
func (s *Server) ConfigurationByName(ctx context.Context, name string) (*ConfigurationOption, error) {
	const q = `
SELECT configuration_id, name, value, value_in_use,
       minimum, maximum, is_dynamic, is_advanced, description
FROM   sys.configurations
WHERE  name = @p1`

	c := &ConfigurationOption{server: s}
	var desc sql.NullString
	if err := s.queryRowScan(ctx, q, []any{name},
		&c.ConfigID, &c.Name, &c.Value, &c.ValueInUse,
		&c.Minimum, &c.Maximum, &c.IsDynamic, &c.IsAdvanced, &desc,
	); err != nil {
		return nil, rowErr(err, notFoundf("gosmo: configuration option %q not found", name), "configuration by name")
	}
	c.Description = desc.String
	c.fillDefault()
	return c, nil
}

// ConfigurationRef returns a lightweight handle for name without querying the
// server at all — unlike ConfigurationByName, it
// doesn't verify the option exists or populate ConfigID/Value/ValueInUse/
// Minimum/Maximum/IsDynamic/IsAdvanced/Description (they stay at their zero
// value). SetValue only ever needs the option's name, never those
// cached fields, so this is sufficient for setting an option whose name the
// caller already knows. Note that IsDynamic stays false on a handle, so the
// caller decides on its own whether Server.Reconfigure is needed. See
// Server.DatabaseRef's doc comment for why this also matters under a
// WithScript-derived context.
func (s *Server) ConfigurationRef(name string) *ConfigurationOption {
	return &ConfigurationOption{server: s, Name: name}
}

// SetValue changes the option value using sp_configure.
// For non-dynamic options, call Server.Reconfigure() afterwards.
//
// It is the raw single-statement primitive: a bare sp_configure. On a server
// where "show advanced options" is 0 — the installation default — every
// advanced option (max degree of parallelism, max server memory, fill
// factor, …) fails with Msg 15123 "does not exist, or it may be an advanced
// option". Server.ApplyConfiguration handles that, and applies a set of
// changes with one RECONFIGURE; prefer it for anything a user edits.
func (c *ConfigurationOption) SetValue(ctx context.Context, value int64) error {
	q := fmt.Sprintf("EXEC sp_configure N'%s', %d", escapeSingle(c.Name), value)
	if _, err := c.server.exec(ctx, q); err != nil {
		return fmt.Errorf("gosmo: set configuration %q = %d: %w", c.Name, value, err)
	}
	setIfApplied(ctx, &c.Value, value)
	return nil
}

// ReconfigureMode says whether Server.Reconfigure checks the pending values
// against their recommended ranges.
type ReconfigureMode int

const (
	// ReconfigureChecked is the bare RECONFIGURE, which refuses a value
	// outside its option's recommended range.
	ReconfigureChecked ReconfigureMode = iota

	// ReconfigureOverride is RECONFIGURE WITH OVERRIDE, which installs it
	// anyway.
	ReconfigureOverride
)

// Reconfigure applies pending sp_configure changes, with the range checks
// mode says.
func (s *Server) Reconfigure(ctx context.Context, mode ReconfigureMode) error {
	q := "RECONFIGURE"
	switch mode {
	case ReconfigureChecked:
	case ReconfigureOverride:
		q += " WITH OVERRIDE"
	default:
		return invalidf("gosmo: reconfigure: unrecognized mode %d", mode)
	}
	if _, err := s.exec(ctx, q); err != nil {
		return fmt.Errorf("gosmo: reconfigure: %w", err)
	}
	return nil
}

// ConfigChange is one sp_configure change for Server.ApplyConfiguration:
// the option's sys.configurations name and its new value.
type ConfigChange struct {
	Name  string
	Value int64
}

// ConfigApplyOptions controls Server.ApplyConfiguration.
type ConfigApplyOptions struct {
	// Override issues RECONFIGURE WITH OVERRIDE, which installs values the
	// plain form refuses as out of the recommended range.
	Override bool
}

// showAdvancedOptions is the sp_configure option that hides every advanced
// option from sp_configure while it is 0.
const showAdvancedOptions = "show advanced options"

// ApplyConfiguration applies changes in one batch, the way SSMS scripts a
// Server Properties change:
//
//  1. record "show advanced options" and, when it is off and any change
//     names an advanced option, turn it on (with its own RECONFIGURE, which
//     sp_configure needs before it will accept an advanced name);
//  2. sp_configure every change;
//  3. one RECONFIGURE [WITH OVERRIDE];
//  4. put "show advanced options" back to its configured value if step 1
//     turned it on.
//
// A bare sp_configure of an advanced option fails Msg 15123 while "show
// advanced options" is 0, which is how every stock installation ships — see
// ConfigurationOption.SetValue. Whether an option is advanced is decided by
// the server (sys.configurations.is_advanced), not by gosmo, so the batch is
// the same scripted as executed.
//
// Step 4 restores the configured value read before step 1, not 0: an
// sp_configure of 1 still waiting for its RECONFIGURE has value_in_use 0, so
// step 1 runs, and writing 0 back would silently drop that pending 1.
//
// A change to "show advanced options" itself wins over step 4: it is applied
// after every other change and nothing is restored afterwards.
//
// An sp_configure that fails (an unknown name, a value out of range, an
// option the edition lacks) does not stop the rest, so the RECONFIGURE still
// installs every change that was accepted and step 4 still runs. The error
// returned names every failure. A refused RECONFIGURE (Msg 5807, a value
// outside the recommended range without Override) is different: it ends the
// batch, leaving every change configured but not running. So each RECONFIGURE
// that follows step 1 sits in TRY/CATCH, and the CATCH puts "show advanced
// options" back to its configured value before rethrowing — not running
// either, since its own RECONFIGURE would be refused the same way, but
// pending with the rest rather than left on for good (found live on 13: the
// option stayed 1/1). An empty changes is a no-op.
func (s *Server) ApplyConfiguration(ctx context.Context, changes []ConfigChange, opts ConfigApplyOptions) error {
	if len(changes) == 0 {
		return nil
	}
	if _, err := s.exec(ctx, buildApplyConfiguration(changes, opts)); err != nil {
		return fmt.Errorf("gosmo: apply configuration: %w", err)
	}
	return nil
}

// buildApplyConfiguration renders ApplyConfiguration's batch.
func buildApplyConfiguration(changes []ConfigChange, opts ConfigApplyOptions) string {
	reconfigure := "RECONFIGURE"
	if opts.Override {
		reconfigure += " WITH OVERRIDE"
	}
	var names []string
	var ordered []ConfigChange
	var showAdvanced *ConfigChange
	for _, c := range changes {
		if strings.EqualFold(c.Name, showAdvancedOptions) {
			showAdvanced = &c
			continue
		}
		ordered = append(ordered, c)
		names = append(names, "N'"+escapeSingle(c.Name)+"'")
	}
	if showAdvanced != nil {
		ordered = append(ordered, *showAdvanced)
	}

	var b strings.Builder
	enable := len(names) > 0
	if enable {
		fmt.Fprintf(&b, `DECLARE @show_advanced_enabled bit = 0;
DECLARE @show_advanced_value int = (SELECT CONVERT(int, value) FROM sys.configurations WHERE name = N'%[1]s');
IF EXISTS (SELECT 1 FROM sys.configurations WHERE name = N'%[1]s' AND value_in_use = 0)
   AND EXISTS (SELECT 1 FROM sys.configurations WHERE is_advanced = 1 AND name IN (%[2]s))
BEGIN
    EXEC sys.sp_configure N'%[1]s', 1;
    BEGIN TRY
        %[3]s;
    END TRY
    BEGIN CATCH
        EXEC sys.sp_configure N'%[1]s', @show_advanced_value;
        THROW;
    END CATCH;
    SET @show_advanced_enabled = 1;
END;
`, showAdvancedOptions, strings.Join(names, ", "), reconfigure)
	}
	for _, c := range ordered {
		fmt.Fprintf(&b, "EXEC sys.sp_configure N'%s', %d;\n", escapeSingle(c.Name), c.Value)
	}
	if enable && showAdvanced == nil {
		fmt.Fprintf(&b, `BEGIN TRY
    %[2]s;
END TRY
BEGIN CATCH
    IF @show_advanced_enabled = 1
        EXEC sys.sp_configure N'%[1]s', @show_advanced_value;
    THROW;
END CATCH;
IF @show_advanced_enabled = 1
BEGIN
    EXEC sys.sp_configure N'%[1]s', @show_advanced_value;
    %[2]s;
END;`, showAdvancedOptions, reconfigure)
	} else {
		b.WriteString(reconfigure)
		b.WriteString(";")
	}
	return b.String()
}

// ============================================================
// Memory  (live DMV counters, distinct from the sp_configure-backed
// min/max server memory options above)
// ============================================================

// ServerMemoryStats holds live memory figures for the Server Properties >
// Memory page's "Current values" section — unlike the configured min/max
// server memory (an sp_configure option, see ConfigurationOption), these
// reflect the server's actual memory state right now.
type ServerMemoryStats struct {
	PhysicalMemoryMB     int64
	AvailableMemoryMB    int64
	TargetServerMemoryMB int64
	TotalServerMemoryMB  int64
}

// MemoryStats returns live server memory figures.
func (s *Server) MemoryStats(ctx context.Context) (*ServerMemoryStats, error) {
	const q = `
SELECT
    (SELECT total_physical_memory_kb / 1024 FROM sys.dm_os_sys_memory),
    (SELECT available_physical_memory_kb / 1024 FROM sys.dm_os_sys_memory),
    (SELECT cntr_value / 1024 FROM sys.dm_os_performance_counters
      WHERE object_name LIKE '%Memory Manager%' AND counter_name = 'Target Server Memory (KB)'),
    (SELECT cntr_value / 1024 FROM sys.dm_os_performance_counters
      WHERE object_name LIKE '%Memory Manager%' AND counter_name = 'Total Server Memory (KB)')`

	m := &ServerMemoryStats{}
	if err := s.queryRowScan(ctx, q, nil,
		&m.PhysicalMemoryMB, &m.AvailableMemoryMB, &m.TargetServerMemoryMB, &m.TotalServerMemoryMB,
	); err != nil {
		return nil, fmt.Errorf("gosmo: server memory stats: %w", err)
	}
	return m, nil
}

// ============================================================
// Processors  (CPU/NUMA topology — Server Properties > Processors page)
// ============================================================

// ProcessorInfo holds server-wide CPU/NUMA topology: the header counts on
// Server Properties > Processors (CPU count, NUMA nodes, hyperthread
// ratio) and the NUMA column in its per-CPU affinity grid.
// CPUNUMANode[i] is the NUMA node hosting logical CPU i.
type ProcessorInfo struct {
	CPUCount         int
	HyperthreadRatio int
	NUMANodeCount    int
	CPUNUMANode      []int
}

// ProcessorInfo returns server-wide CPU/NUMA topology.
func (s *Server) ProcessorInfo(ctx context.Context) (*ProcessorInfo, error) {
	info := &ProcessorInfo{}
	const q = `SELECT cpu_count, hyperthread_ratio FROM sys.dm_os_sys_info`
	if err := s.queryRowScan(ctx, q, nil, &info.CPUCount, &info.HyperthreadRatio); err != nil {
		return nil, fmt.Errorf("gosmo: processor info: %w", err)
	}

	const nq = `
SELECT cpu_id, parent_node_id
FROM   sys.dm_os_schedulers
WHERE  status = 'VISIBLE ONLINE'
GROUP  BY cpu_id, parent_node_id
ORDER  BY cpu_id`

	rows, err := s.query(ctx, nq)
	if err != nil {
		return nil, fmt.Errorf("gosmo: processor NUMA map: %w", err)
	}
	defer rows.Close()

	nodes := make(map[int]bool)
	cpuNode := make(map[int]int)
	maxCPU := -1
	for rows.Next() {
		var cpu, node int
		if err := rows.Scan(&cpu, &node); err != nil {
			return nil, fmt.Errorf("gosmo: processor info: %w", err)
		}
		cpuNode[cpu] = node
		nodes[node] = true
		if cpu > maxCPU {
			maxCPU = cpu
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("gosmo: processor info: %w", err)
	}

	info.CPUNUMANode = make([]int, maxCPU+1)
	for cpu, node := range cpuNode {
		info.CPUNUMANode[cpu] = node
	}
	info.NUMANodeCount = len(nodes)
	return info, nil
}

// Scheduler is one scheduler user work can run on — a VISIBLE row of
// sys.dm_os_schedulers — with the NUMA node and processor group hosting it.
// It is what a pool's AFFINITY names (PoolAffinity): a resource pool takes
// ID, an external pool CPUID, either NUMANode.
type Scheduler struct {
	ID             int
	CPUID          int
	NUMANode       int
	ProcessorGroup int

	// IsOnline is false for a scheduler the server's own affinity mask
	// leaves out (VISIBLE OFFLINE). A pool can still name it.
	IsOnline bool

	// The scheduler's load at the moment of the read. RunnableTasks are
	// waiting for it — CPU pressure — and WorkQueue are queued for a worker
	// it does not have yet. LoadFactor is the engine's own measure, the one
	// it places new tasks by.
	RunnableTasks int
	CurrentTasks  int
	ActiveWorkers int
	WorkQueue     int
	LoadFactor    int
}

// Schedulers returns the instance's visible schedulers in id order — hidden
// (system) ones and the dedicated admin connection's are left out — each with
// its current load. Only the online ones run user work. Needs
// VIEW SERVER STATE (VIEW SERVER PERFORMANCE STATE from SQL Server 2022);
// without it the server refuses the read.
func (s *Server) Schedulers(ctx context.Context) ([]Scheduler, error) {
	rows, err := s.query(ctx, `
SELECT s.scheduler_id, s.cpu_id, s.parent_node_id, n.processor_group, s.is_online,
       s.runnable_tasks_count, s.current_tasks_count, s.active_workers_count,
       s.work_queue_count, s.load_factor
FROM   sys.dm_os_schedulers s
JOIN   sys.dm_os_nodes n ON n.node_id = s.parent_node_id
WHERE  s.status IN (N'VISIBLE ONLINE', N'VISIBLE OFFLINE')
ORDER  BY s.scheduler_id`)
	return scanRows(rows, err, "list schedulers", func(scan func(...any) error) (Scheduler, error) {
		var sc Scheduler
		err := scan(&sc.ID, &sc.CPUID, &sc.NUMANode, &sc.ProcessorGroup, &sc.IsOnline,
			&sc.RunnableTasks, &sc.CurrentTasks, &sc.ActiveWorkers, &sc.WorkQueue, &sc.LoadFactor)
		return sc, err
	})
}

// ============================================================
// Start time
// ============================================================

// StartTime returns when the instance last started
// (sys.dm_os_sys_info.sqlserver_start_time) — the server's local wall clock
// carried in UTC, as every zoneless server time is — and how long ago that
// was by the server's own clock. Uptime comes from the server because the
// start time is not a real UTC instant: subtracting it from the client's
// clock is off by the server's UTC offset and any clock skew.
//
// Needs VIEW SERVER STATE (VIEW SERVER PERFORMANCE STATE from SQL Server
// 2022); without it the server refuses the read. tempdb's create_date needs
// no permission but trails the start by however long recovery took, so it is
// not used.
func (s *Server) StartTime(ctx context.Context) (start time.Time, uptime time.Duration, err error) {
	const q = `
SELECT sqlserver_start_time,
       DATEDIFF_BIG(millisecond, sqlserver_start_time, SYSDATETIME())
FROM   sys.dm_os_sys_info`
	var ms int64
	if err := s.queryRowScan(ctx, q, nil, &start, &ms); err != nil {
		return time.Time{}, 0, fmt.Errorf("gosmo: server start time: %w", err)
	}
	return start, time.Duration(ms) * time.Millisecond, nil
}

// CurrentTime returns the server's clock now (SYSDATETIMEOFFSET), in a fixed
// zone at the server's UTC offset — unlike the zoneless server times, a real
// instant. Its Location is how a caller turns a server-local wall clock into
// an instant, and back: the offset is today's, so a time on the other side of
// a daylight-saving change is an hour out. Needs no permission.
func (s *Server) CurrentTime(ctx context.Context) (time.Time, error) {
	var now time.Time
	if err := s.queryRowScan(ctx, `SELECT SYSDATETIMEOFFSET()`, nil, &now); err != nil {
		return time.Time{}, fmt.Errorf("gosmo: server current time: %w", err)
	}
	return now, nil
}

// ============================================================
// Disk volumes
// ============================================================

// DiskVolumeInfo describes free/total space for one storage volume backing
// at least one of the server's database files, as reported by
// sys.dm_os_volume_stats — a DMV SQL Server exposes identically on Windows
// and Linux, so this is usable regardless of the host OS.
type DiskVolumeInfo struct {
	// MountPoint is the drive letter (Windows) or mount path (Linux). Some
	// hosts — e.g. a containerized Linux instance without a distinct OS
	// volume — report this as empty.
	MountPoint string
	// VolumeName is the OS volume label, also sometimes empty.
	VolumeName string
	// SamplePath is one database file's path stored on this volume, for
	// display when MountPoint and VolumeName are both empty.
	SamplePath  string
	TotalMB     float64
	AvailableMB float64
}

// DiskVolumes returns free/total space for every storage volume backing a
// database file on the server.
func (s *Server) DiskVolumes(ctx context.Context) ([]DiskVolumeInfo, error) {
	const q = `
SELECT
    vs.volume_mount_point,
    vs.logical_volume_name,
    MIN(mf.physical_name)          AS sample_path,
    vs.total_bytes / 1048576.0     AS total_mb,
    vs.available_bytes / 1048576.0 AS available_mb
FROM sys.master_files mf
CROSS APPLY sys.dm_os_volume_stats(mf.database_id, mf.file_id) vs
GROUP BY vs.volume_mount_point, vs.logical_volume_name, vs.total_bytes, vs.available_bytes
ORDER BY vs.volume_mount_point`

	rows, err := s.query(ctx, q)
	return scanRows(rows, err, "disk volumes", func(scan func(...any) error) (DiskVolumeInfo, error) {
		var v DiskVolumeInfo
		var mount, name, path sql.NullString
		if err := scan(&mount, &name, &path, &v.TotalMB, &v.AvailableMB); err != nil {
			return DiskVolumeInfo{}, err
		}
		v.MountPoint, v.VolumeName, v.SamplePath = mount.String, name.String, path.String
		return v, nil
	})
}

// Plausible reports whether the volume's figures can be shown: a total above
// zero and no more available than the total. A host reporting a volume it
// does not own answers otherwise — an Azure Managed Instance gives C:\ a
// total of 192 MB and 65,344 MB available — and "65,344 MB free of 192 MB"
// is worse than saying nothing.
func (v DiskVolumeInfo) Plausible() bool { return plausibleVolume(v.TotalMB, v.AvailableMB) }

// FileVolume is the storage volume one database file sits on, as
// sys.dm_os_volume_stats reports it for that file — the file-to-volume
// mapping DiskVolumes groups away.
type FileVolume struct {
	DatabaseID   int
	FileID       int
	PhysicalName string // the file's path (sys.master_files)
	// MountPoint and VolumeName are as DiskVolumeInfo's: either or both
	// can be empty on a containerized host.
	MountPoint     string
	VolumeName     string
	TotalBytes     int64
	AvailableBytes int64
}

// Plausible is DiskVolumeInfo.Plausible for one file's volume figures.
func (v FileVolume) Plausible() bool {
	return plausibleVolume(float64(v.TotalBytes), float64(v.AvailableBytes))
}

// plausibleVolume is the one rule both volume shapes follow.
func plausibleVolume(total, available float64) bool {
	return total > 0 && available <= total
}

// FileVolumes returns the volume behind every database file on the server,
// one row per file (sys.master_files × sys.dm_os_volume_stats), ordered by
// database_id and file_id.
//
// The DMV needs VIEW SERVER STATE (VIEW SERVER PERFORMANCE STATE from 2022)
// and is refused outright without it (Msg 300). FileVolumes asks first, as
// DatabaseFiles does, and answers visible=false with no rows instead of an
// error, so a caller can say which permission is missing. Rows are returned
// as the host reports them; filter with Plausible before showing figures.
func (s *Server) FileVolumes(ctx context.Context) (vols []FileVolume, visible bool, err error) {
	// The permission is the first column, so the ELSE branch's single row
	// says "hidden" — no rows at all from the first branch is a host whose
	// DMV answered nothing, which is visible and empty.
	const q = `
IF HAS_PERMS_BY_NAME(NULL, NULL, 'VIEW SERVER STATE') = 1
   OR HAS_PERMS_BY_NAME(NULL, NULL, 'VIEW SERVER PERFORMANCE STATE') = 1
    SELECT CAST(1 AS bit), mf.database_id, mf.file_id, mf.physical_name,
           vs.volume_mount_point, vs.logical_volume_name, vs.total_bytes, vs.available_bytes
    FROM   sys.master_files mf
    CROSS  APPLY sys.dm_os_volume_stats(mf.database_id, mf.file_id) vs
    ORDER  BY mf.database_id, mf.file_id
ELSE
    SELECT CAST(0 AS bit), NULL, NULL, NULL, NULL, NULL, NULL, NULL`

	rows, err := s.query(ctx, q)
	visible = true
	list, err := scanRows(rows, err, "file volumes", func(scan func(...any) error) (*FileVolume, error) {
		var shown bool
		var dbID, fileID sql.NullInt32
		var path, mount, name sql.NullString
		var total, avail sql.NullInt64
		if err := scan(&shown, &dbID, &fileID, &path, &mount, &name, &total, &avail); err != nil {
			return nil, err
		}
		if !shown {
			visible = false
			return nil, nil
		}
		return &FileVolume{
			DatabaseID: int(dbID.Int32), FileID: int(fileID.Int32), PhysicalName: path.String,
			MountPoint: mount.String, VolumeName: name.String,
			TotalBytes: total.Int64, AvailableBytes: avail.Int64,
		}, nil
	})
	if err != nil {
		return nil, false, err
	}
	if !visible {
		return nil, false, nil
	}
	vols = make([]FileVolume, len(list))
	for i, v := range list {
		vols[i] = *v
	}
	return vols, true, nil
}

// ============================================================
// Languages
// ============================================================

// Language mirrors a row from sys.syslanguages — used to populate the
// server's "Default language" and a Login's "Default language" dropdowns.
type Language struct {
	LangID int
	Name   string
	Alias  string
}

// Languages returns every language installed on the server.
func (s *Server) Languages(ctx context.Context) ([]*Language, error) {
	const q = `SELECT langid, name, alias FROM sys.syslanguages ORDER BY name`

	rows, err := s.query(ctx, q)
	return scanRows(rows, err, "list languages", func(scan func(...any) error) (*Language, error) {
		l := &Language{}
		if err := scan(&l.LangID, &l.Name, &l.Alias); err != nil {
			return nil, err
		}
		return l, nil
	})
}

// ============================================================
// Active Sessions
// ============================================================

// ActiveSession holds information about one session from sys.dm_exec_sessions.
type ActiveSession struct {
	SessionID        int
	LoginName        string
	HostName         string
	ProgramName      string
	DatabaseName     string
	Status           string
	CPUTime          int64
	MemoryUsage      int64
	TotalElapsedMS   int64
	LastRequestStart string
	// CommandText is the batch of the request the session is running now, its
	// first 512 characters; empty for a session running nothing.
	CommandText string
	// RecentText is the batch the session's connection sent last
	// (dm_exec_connections.most_recent_sql_handle), its first 512
	// characters — in the server's auto-parameterized form when it
	// parameterized it ((@1 int)UPDATE [t] set [v] = @1 …): for a running
	// session usually CommandText's batch, and for
	// an idle one the only text there is — an idle head blocker holding an
	// open transaction shows here what opened it. Empty for a session with no
	// connection (a system session) or whose batch has left the cache.
	RecentText string
	// Command is the running request's command (SELECT, UPDATE, AWAITING
	// COMMAND…), empty for a session running nothing.
	Command string
	// OpenTransactionCount is the session's open transactions — nonzero for an
	// idle session is a transaction left open, the usual head blocker.
	OpenTransactionCount int
	// Reads, Writes and LogicalReads are the session's totals since it
	// logged in, as CPUTime is: physical reads, writes and logical reads.
	Reads        int64
	Writes       int64
	LogicalReads int64
	// BlockingSessionID is the session a request of this one is blocked by,
	// 0 for none — a parallel request's blocked worker counts
	// (sessionBlockerJoin). WaitType, WaitTimeMS and WaitResource are that
	// worker's wait when it is one, else the request's; WaitResource is the
	// lock or page waited on (KEY: 5:72057594043236352 (8194443284a0)), in
	// dm_os_waiting_tasks' wording for a parallel worker's wait
	// (sessionBlockerWaitResource), empty for none.
	BlockingSessionID int
	WaitType          string
	WaitTimeMS        int64
	WaitResource      string
	// HostProcessID is the client's process id as reported at login
	// (go-mssqldb sends os.Getpid()), 0 for a system session.
	// FromCallerHost reports that the session's client named the reading
	// connection's own host (host_name = HOST_NAME()). Together they pick out
	// one client process's sessions, as BlockedRequests does.
	HostProcessID  int
	FromCallerHost bool
	// IsUserProcess is dm_exec_sessions.is_user_process: false for SQL
	// Server's internal sessions, which KILL refuses (Msg 6107).
	IsUserProcess bool
	// RequestCPUTime, RequestElapsedMS, RequestReads, RequestWrites and
	// RequestLogicalReads are the running request's (dm_exec_requests), 0
	// for a session running nothing. The session's own counters (CPUTime,
	// TotalElapsedMS, Reads, Writes, LogicalReads) are updated only when a
	// request ends, so they leave the running one out — a query blocked for
	// minutes shows 0 there (found live): the session's figure including
	// the running request is the sum of the two.
	RequestCPUTime      int64
	RequestElapsedMS    int64
	RequestReads        int64
	RequestWrites       int64
	RequestLogicalReads int64
}

// SessionScope says which sessions Server.ActiveSessions returns.
type SessionScope int

const (
	// UserSessionsOnly is the sessions of user connections
	// (is_user_process = 1).
	UserSessionsOnly SessionScope = iota

	// AllSessions adds SQL Server's internal sessions.
	AllSessions
)

// ActiveSessions returns the sessions scope names, other than the caller's
// own.
func (s *Server) ActiveSessions(ctx context.Context, scope SessionScope) ([]*ActiveSession, error) {
	var sysFilter string
	switch scope {
	case UserSessionsOnly:
		sysFilter = "AND s.is_user_process = 1"
	case AllSessions:
	default:
		return nil, invalidf("gosmo: active sessions: unrecognized session scope %d", scope)
	}
	q := `
SELECT ` + activeSessionColumns + `
` + activeSessionFrom + `
WHERE  s.session_id != @@SPID ` + sysFilter + `
ORDER  BY s.session_id`

	rows, err := s.query(ctx, q)
	return scanRows(rows, err, "active sessions", func(scan func(...any) error) (*ActiveSession, error) {
		return scanActiveSession(s, scan)
	})
}

// SessionByID reads one session — the caller's own included, unlike
// ActiveSessions — with ErrNotFound when no session has that id: it has
// ended, or the login lacks VIEW SERVER STATE and sees only its own.
func (s *Server) SessionByID(ctx context.Context, sessionID int) (*ActiveSession, error) {
	q := "SELECT " + activeSessionColumns + "\n" + activeSessionFrom + "\nWHERE  s.session_id = @p1"
	return readByName(ctx, s, scanActiveSession, q, []any{sessionID},
		notFoundf("gosmo: session %d not found", sessionID), fmt.Sprintf("read session %d", sessionID))
}

// activeSessionColumns and activeSessionFrom are the ActiveSession read,
// scanned by scanActiveSession.
const activeSessionColumns = `s.session_id, ISNULL(s.login_name, ''), ISNULL(s.host_name, ''), ISNULL(s.program_name, ''),
       DB_NAME(s.database_id), s.status,
       s.cpu_time, s.memory_usage, s.total_elapsed_time,
       CONVERT(VARCHAR(30), s.last_request_start_time, 121),
       ISNULL(SUBSTRING(t.text, 1, 512), ''),
       ISNULL(SUBSTRING(rt.text, 1, 512), ''),
       ISNULL(r.command, N''),
       s.open_transaction_count, s.reads, s.writes, s.logical_reads,
       ` + sessionBlockerID + `,
       ` + sessionBlockerWaitType + `,
       ` + sessionBlockerWaitMS + `,
       ` + sessionBlockerWaitResource + `,
       ISNULL(s.host_process_id, 0),
       CAST(CASE WHEN s.host_name = HOST_NAME() THEN 1 ELSE 0 END AS bit),
       s.is_user_process,
       ISNULL(r.cpu_time, 0), ISNULL(r.total_elapsed_time, 0),
       ISNULL(r.reads, 0), ISNULL(r.writes, 0), ISNULL(r.logical_reads, 0)`

// activeSessionFrom takes a MARS session's most recently read connection for
// RecentText: each logical connection has its own row and handle.
const activeSessionFrom = `FROM   sys.dm_exec_sessions s
LEFT   JOIN sys.dm_exec_requests r ON r.session_id = s.session_id` + sessionBlockerJoin + `
OUTER  APPLY sys.dm_exec_sql_text(r.sql_handle) t
OUTER  APPLY (SELECT TOP (1) c.most_recent_sql_handle
              FROM   sys.dm_exec_connections c
              WHERE  c.session_id = s.session_id
              ORDER  BY c.last_read DESC) AS rc
OUTER  APPLY sys.dm_exec_sql_text(rc.most_recent_sql_handle) rt`

func scanActiveSession(_ *Server, scan func(...any) error) (*ActiveSession, error) {
	as := &ActiveSession{}
	var dbName, waitType, lastReq, cmd, status sql.NullString
	if err := scan(
		&as.SessionID, &as.LoginName, &as.HostName, &as.ProgramName,
		&dbName, &status,
		&as.CPUTime, &as.MemoryUsage, &as.TotalElapsedMS,
		&lastReq, &cmd, &as.RecentText, &as.Command,
		&as.OpenTransactionCount, &as.Reads, &as.Writes, &as.LogicalReads,
		&as.BlockingSessionID, &waitType, &as.WaitTimeMS, &as.WaitResource,
		&as.HostProcessID, &as.FromCallerHost, &as.IsUserProcess,
		&as.RequestCPUTime, &as.RequestElapsedMS,
		&as.RequestReads, &as.RequestWrites, &as.RequestLogicalReads,
	); err != nil {
		return nil, err
	}
	as.DatabaseName = dbName.String
	as.Status = status.String
	as.LastRequestStart = lastReq.String
	as.CommandText = cmd.String
	as.WaitType = waitType.String
	return as, nil
}

// KillSession terminates a session by session ID. A sessionID below 1 is
// refused with ErrInvalidRequest before anything is sent.
func (s *Server) KillSession(ctx context.Context, sessionID int) error {
	if sessionID <= 0 {
		return invalidf("gosmo: kill session %d: a session ID is positive", sessionID)
	}
	if _, err := s.exec(ctx, fmt.Sprintf("KILL %d", sessionID)); err != nil {
		return fmt.Errorf("gosmo: kill session %d: %w", sessionID, err)
	}
	return nil
}
