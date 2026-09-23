package gosmo

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
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
		if errors.Is(err, sql.ErrNoRows) {
			return nil, notFoundf("gosmo: configuration option %q not found", name)
		}
		return nil, fmt.Errorf("gosmo: configuration by name: %w", err)
	}
	c.Description = desc.String
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
	if err := c.server.exec(ctx, q); err != nil {
		return fmt.Errorf("gosmo: set configuration %q = %d: %w", c.Name, value, err)
	}
	setIfApplied(ctx, &c.Value, value)
	return nil
}

// Reconfigure applies pending sp_configure changes.
// Pass override=true to use RECONFIGURE WITH OVERRIDE (bypasses range checks).
func (s *Server) Reconfigure(ctx context.Context, override bool) error {
	q := "RECONFIGURE"
	if override {
		q += " WITH OVERRIDE"
	}
	if err := s.exec(ctx, q); err != nil {
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
//  4. put "show advanced options" back if step 1 turned it on.
//
// A bare sp_configure of an advanced option fails Msg 15123 while "show
// advanced options" is 0, which is how every stock installation ships — see
// ConfigurationOption.SetValue. Whether an option is advanced is decided by
// the server (sys.configurations.is_advanced), not by gosmo, so the batch is
// the same scripted as executed.
//
// A change to "show advanced options" itself wins over step 4: it is applied
// after every other change and nothing is restored afterwards.
//
// The batch has no TRY/CATCH: an sp_configure that fails (an unknown name, a
// value out of range) does not stop the rest, so the RECONFIGURE still
// installs every change that was accepted and step 4 still runs. The error
// returned names every failure. An empty changes is a no-op.
func (s *Server) ApplyConfiguration(ctx context.Context, changes []ConfigChange, opts ConfigApplyOptions) error {
	if len(changes) == 0 {
		return nil
	}
	if err := s.exec(ctx, buildApplyConfiguration(changes, opts)); err != nil {
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
IF EXISTS (SELECT 1 FROM sys.configurations WHERE name = N'%[1]s' AND value_in_use = 0)
   AND EXISTS (SELECT 1 FROM sys.configurations WHERE is_advanced = 1 AND name IN (%[2]s))
BEGIN
    EXEC sys.sp_configure N'%[1]s', 1;
    %[3]s;
    SET @show_advanced_enabled = 1;
END;
`, showAdvancedOptions, strings.Join(names, ", "), reconfigure)
	}
	for _, c := range ordered {
		fmt.Fprintf(&b, "EXEC sys.sp_configure N'%s', %d;\n", escapeSingle(c.Name), c.Value)
	}
	b.WriteString(reconfigure + ";")
	if enable && showAdvanced == nil {
		fmt.Fprintf(&b, `
IF @show_advanced_enabled = 1
BEGIN
    EXEC sys.sp_configure N'%s', 0;
    %s;
END;`, showAdvancedOptions, reconfigure)
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
	SessionID         int
	LoginName         string
	HostName          string
	ProgramName       string
	DatabaseName      string
	Status            string
	CPUTime           int64
	MemoryUsage       int64
	TotalElapsedMS    int64
	LastRequestStart  string
	CommandText       string
	BlockingSessionID int
	WaitType          string
	WaitTimeMS        int64
}

// ActiveSessions returns running sessions.
// Set includeSystem=true to include SQL Server internal sessions.
func (s *Server) ActiveSessions(ctx context.Context, includeSystem bool) ([]*ActiveSession, error) {
	sysFilter := "AND s.is_user_process = 1"
	if includeSystem {
		sysFilter = ""
	}
	q := fmt.Sprintf(`
SELECT s.session_id, ISNULL(s.login_name, ''), ISNULL(s.host_name, ''), ISNULL(s.program_name, ''),
       DB_NAME(s.database_id), s.status,
       s.cpu_time, s.memory_usage, s.total_elapsed_time,
       CONVERT(VARCHAR(30), s.last_request_start_time, 121),
       ISNULL(SUBSTRING(t.text, 1, 512), ''),
       ISNULL(r.blocking_session_id, 0),
       ISNULL(r.wait_type, ''),
       ISNULL(r.wait_time, 0)
FROM   sys.dm_exec_sessions s
LEFT   JOIN sys.dm_exec_requests r ON r.session_id = s.session_id
OUTER  APPLY sys.dm_exec_sql_text(r.sql_handle) t
WHERE  s.session_id != @@SPID %s
ORDER  BY s.session_id`, sysFilter)

	rows, err := s.query(ctx, q)
	return scanRows(rows, err, "active sessions", func(scan func(...any) error) (*ActiveSession, error) {
		as := &ActiveSession{}
		var dbName, waitType, lastReq, cmd, status sql.NullString
		if err := scan(
			&as.SessionID, &as.LoginName, &as.HostName, &as.ProgramName,
			&dbName, &status,
			&as.CPUTime, &as.MemoryUsage, &as.TotalElapsedMS,
			&lastReq, &cmd,
			&as.BlockingSessionID, &waitType, &as.WaitTimeMS,
		); err != nil {
			return nil, err
		}
		as.DatabaseName = dbName.String
		as.Status = status.String
		as.LastRequestStart = lastReq.String
		as.CommandText = cmd.String
		as.WaitType = waitType.String
		return as, nil
	})
}

// KillSession terminates a session by session ID.
func (s *Server) KillSession(ctx context.Context, sessionID int) error {
	if err := s.exec(ctx, fmt.Sprintf("KILL %d", sessionID)); err != nil {
		return fmt.Errorf("gosmo: kill session %d: %w", sessionID, err)
	}
	return nil
}

// ============================================================
// Database Mail  (no WMI/COM required, pure T-SQL)
// ============================================================

// MailProfile represents an msdb Database Mail profile.
type MailProfile struct {
	ProfileID   int
	Name        string
	Description string
	IsDefault   bool
}

// MailProfiles returns all Database Mail profiles from msdb.
func (s *Server) MailProfiles(ctx context.Context) ([]*MailProfile, error) {
	const q = `
SELECT p.profile_id, p.name, ISNULL(p.description,''),
       ISNULL(pp.is_default, 0)
FROM   msdb.dbo.sysmail_profile p
LEFT   JOIN msdb.dbo.sysmail_principalprofile pp
       ON  pp.profile_id = p.profile_id AND pp.principal_sid = 0x00
ORDER  BY p.name`

	rows, err := s.query(ctx, q)
	return scanRows(rows, err, "list mail profiles", func(scan func(...any) error) (*MailProfile, error) {
		p := &MailProfile{}
		if err := scan(&p.ProfileID, &p.Name, &p.Description, &p.IsDefault); err != nil {
			return nil, err
		}
		return p, nil
	})
}

// SendMail sends an email via Database Mail (sp_send_dbmail).
func (s *Server) SendMail(ctx context.Context, profile, recipients, subject, body string) error {
	q := fmt.Sprintf(
		"EXEC msdb.dbo.sp_send_dbmail @profile_name = N'%s', @recipients = N'%s', "+
			"@subject = N'%s', @body = N'%s'",
		escapeSingle(profile), escapeSingle(recipients),
		escapeSingle(subject), escapeSingle(body),
	)
	if err := s.exec(ctx, q); err != nil {
		return fmt.Errorf("gosmo: send mail: %w", err)
	}
	return nil
}
