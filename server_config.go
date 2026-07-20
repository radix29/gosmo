package gosmo

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
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

// Configurations returns all server configuration options.
func (s *Server) Configurations() ([]*ConfigurationOption, error) {
	return s.ConfigurationsContext(context.Background())
}

// ConfigurationsContext is the context-aware variant of Configurations.
func (s *Server) ConfigurationsContext(ctx context.Context) ([]*ConfigurationOption, error) {
	const q = `
SELECT configuration_id, name, value, value_in_use,
       minimum, maximum, is_dynamic, is_advanced, description
FROM   sys.configurations
ORDER  BY name`

	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("gosmo: list configurations: %w", err)
	}
	defer rows.Close()

	var opts []*ConfigurationOption
	for rows.Next() {
		c := &ConfigurationOption{server: s}
		var desc sql.NullString
		if err := rows.Scan(
			&c.ConfigID, &c.Name, &c.Value, &c.ValueInUse,
			&c.Minimum, &c.Maximum, &c.IsDynamic, &c.IsAdvanced, &desc,
		); err != nil {
			return nil, err
		}
		c.Description = desc.String
		opts = append(opts, c)
	}
	return opts, rows.Err()
}

// ConfigurationByName returns a single option using a direct parameterised query.
func (s *Server) ConfigurationByName(name string) (*ConfigurationOption, error) {
	return s.ConfigurationByNameContext(context.Background(), name)
}

// ConfigurationByNameContext is the context-aware variant.
func (s *Server) ConfigurationByNameContext(ctx context.Context, name string) (*ConfigurationOption, error) {
	const q = `
SELECT configuration_id, name, value, value_in_use,
       minimum, maximum, is_dynamic, is_advanced, description
FROM   sys.configurations
WHERE  name = @p1`

	c := &ConfigurationOption{server: s}
	var desc sql.NullString
	row := s.db.QueryRowContext(ctx, q, name)
	if err := row.Scan(
		&c.ConfigID, &c.Name, &c.Value, &c.ValueInUse,
		&c.Minimum, &c.Maximum, &c.IsDynamic, &c.IsAdvanced, &desc,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("gosmo: configuration option %q not found", name)
		}
		return nil, fmt.Errorf("gosmo: configuration by name: %w", err)
	}
	c.Description = desc.String
	return c, nil
}

// SetValue changes the option value using sp_configure.
// For non-dynamic options, call Server.Reconfigure() afterwards.
func (c *ConfigurationOption) SetValue(value int64) error {
	return c.SetValueContext(context.Background(), value)
}

func (c *ConfigurationOption) SetValueContext(ctx context.Context, value int64) error {
	q := fmt.Sprintf("EXEC sp_configure N'%s', %d", escapeSingle(c.Name), value)
	if err := c.server.execContext(ctx, q); err != nil {
		return fmt.Errorf("gosmo: set configuration %q = %d: %w", c.Name, value, err)
	}
	c.Value = value
	return nil
}

// Reconfigure applies pending sp_configure changes.
// Pass override=true to use RECONFIGURE WITH OVERRIDE (bypasses range checks).
func (s *Server) Reconfigure(override bool) error {
	return s.ReconfigureContext(context.Background(), override)
}

func (s *Server) ReconfigureContext(ctx context.Context, override bool) error {
	q := "RECONFIGURE"
	if override {
		q += " WITH OVERRIDE"
	}
	if err := s.execContext(ctx, q); err != nil {
		return fmt.Errorf("gosmo: reconfigure: %w", err)
	}
	return nil
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
func (s *Server) MemoryStats() (*ServerMemoryStats, error) {
	return s.MemoryStatsContext(context.Background())
}

// MemoryStatsContext is the context-aware variant of MemoryStats.
func (s *Server) MemoryStatsContext(ctx context.Context) (*ServerMemoryStats, error) {
	const q = `
SELECT
    (SELECT total_physical_memory_kb / 1024 FROM sys.dm_os_sys_memory),
    (SELECT available_physical_memory_kb / 1024 FROM sys.dm_os_sys_memory),
    (SELECT cntr_value / 1024 FROM sys.dm_os_performance_counters
      WHERE object_name LIKE '%Memory Manager%' AND counter_name = 'Target Server Memory (KB)'),
    (SELECT cntr_value / 1024 FROM sys.dm_os_performance_counters
      WHERE object_name LIKE '%Memory Manager%' AND counter_name = 'Total Server Memory (KB)')`

	m := &ServerMemoryStats{}
	row := s.db.QueryRowContext(ctx, q)
	if err := row.Scan(&m.PhysicalMemoryMB, &m.AvailableMemoryMB, &m.TargetServerMemoryMB, &m.TotalServerMemoryMB); err != nil {
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
func (s *Server) ProcessorInfo() (*ProcessorInfo, error) {
	return s.ProcessorInfoContext(context.Background())
}

// ProcessorInfoContext is the context-aware variant of ProcessorInfo.
func (s *Server) ProcessorInfoContext(ctx context.Context) (*ProcessorInfo, error) {
	info := &ProcessorInfo{}
	const q = `SELECT cpu_count, hyperthread_ratio FROM sys.dm_os_sys_info`
	if err := s.db.QueryRowContext(ctx, q).Scan(&info.CPUCount, &info.HyperthreadRatio); err != nil {
		return nil, fmt.Errorf("gosmo: processor info: %w", err)
	}

	const nq = `
SELECT cpu_id, parent_node_id
FROM   sys.dm_os_schedulers
WHERE  status = 'VISIBLE ONLINE'
GROUP  BY cpu_id, parent_node_id
ORDER  BY cpu_id`

	rows, err := s.db.QueryContext(ctx, nq)
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
			return nil, err
		}
		cpuNode[cpu] = node
		nodes[node] = true
		if cpu > maxCPU {
			maxCPU = cpu
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
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
func (s *Server) DiskVolumes() ([]DiskVolumeInfo, error) {
	return s.DiskVolumesContext(context.Background())
}

// DiskVolumesContext is the context-aware variant of DiskVolumes.
func (s *Server) DiskVolumesContext(ctx context.Context) ([]DiskVolumeInfo, error) {
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

	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("gosmo: disk volumes: %w", err)
	}
	defer rows.Close()

	var out []DiskVolumeInfo
	for rows.Next() {
		var v DiskVolumeInfo
		var mount, name, path sql.NullString
		if err := rows.Scan(&mount, &name, &path, &v.TotalMB, &v.AvailableMB); err != nil {
			return nil, fmt.Errorf("gosmo: disk volumes: %w", err)
		}
		v.MountPoint, v.VolumeName, v.SamplePath = mount.String, name.String, path.String
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("gosmo: disk volumes: %w", err)
	}
	return out, nil
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
func (s *Server) Languages() ([]*Language, error) {
	return s.LanguagesContext(context.Background())
}

// LanguagesContext is the context-aware variant of Languages.
func (s *Server) LanguagesContext(ctx context.Context) ([]*Language, error) {
	const q = `SELECT langid, name, alias FROM sys.syslanguages ORDER BY name`

	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("gosmo: list languages: %w", err)
	}
	defer rows.Close()

	var langs []*Language
	for rows.Next() {
		l := &Language{}
		if err := rows.Scan(&l.LangID, &l.Name, &l.Alias); err != nil {
			return nil, err
		}
		langs = append(langs, l)
	}
	return langs, rows.Err()
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
func (s *Server) ActiveSessions(includeSystem bool) ([]*ActiveSession, error) {
	return s.ActiveSessionsContext(context.Background(), includeSystem)
}

// ActiveSessionsContext is the context-aware variant of ActiveSessions.
func (s *Server) ActiveSessionsContext(ctx context.Context, includeSystem bool) ([]*ActiveSession, error) {
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

	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("gosmo: active sessions: %w", err)
	}
	defer rows.Close()

	var sessions []*ActiveSession
	for rows.Next() {
		as := &ActiveSession{}
		var dbName, waitType, lastReq, cmd, status sql.NullString
		if err := rows.Scan(
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
		sessions = append(sessions, as)
	}
	return sessions, rows.Err()
}

// KillSession terminates a session by session ID.
func (s *Server) KillSession(sessionID int) error {
	return s.KillSessionContext(context.Background(), sessionID)
}

func (s *Server) KillSessionContext(ctx context.Context, sessionID int) error {
	if err := s.execContext(ctx, fmt.Sprintf("KILL %d", sessionID)); err != nil {
		return fmt.Errorf("gosmo: kill session %d: %w", sessionID, err)
	}
	return nil
}

// ============================================================
// Error Log
// ============================================================

// ErrorLogEntry represents one row returned by xp_readerrorlog.
type ErrorLogEntry struct {
	LogDate string
	Process string
	Text    string
}

// ReadErrorLog reads a SQL Server error log file.
// Pass logNumber=0 for the current log, 1 for the first archived log, etc.
func (s *Server) ReadErrorLog(logNumber int) ([]*ErrorLogEntry, error) {
	return s.ReadErrorLogContext(context.Background(), logNumber)
}

func (s *Server) ReadErrorLogContext(ctx context.Context, logNumber int) ([]*ErrorLogEntry, error) {
	rows, err := s.db.QueryContext(ctx,
		fmt.Sprintf("EXEC xp_readerrorlog %d, 1", logNumber))
	if err != nil {
		return nil, fmt.Errorf("gosmo: read error log: %w", err)
	}
	defer rows.Close()

	var entries []*ErrorLogEntry
	for rows.Next() {
		e := &ErrorLogEntry{}
		if err := rows.Scan(&e.LogDate, &e.Process, &e.Text); err != nil {
			return nil, err
		}
		entries = append(entries, e)
	}
	return entries, rows.Err()
}

// CycleErrorLog closes the current error log and opens a new one.
// Equivalent to sp_cycle_errorlog.
func (s *Server) CycleErrorLog() error {
	return s.CycleErrorLogContext(context.Background())
}

func (s *Server) CycleErrorLogContext(ctx context.Context) error {
	if err := s.execContext(ctx, "EXEC sp_cycle_errorlog"); err != nil {
		return fmt.Errorf("gosmo: cycle error log: %w", err)
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
func (s *Server) MailProfiles() ([]*MailProfile, error) {
	return s.MailProfilesContext(context.Background())
}

func (s *Server) MailProfilesContext(ctx context.Context) ([]*MailProfile, error) {
	const q = `
SELECT p.profile_id, p.name, ISNULL(p.description,''),
       ISNULL(pp.is_default, 0)
FROM   msdb.dbo.sysmail_profile p
LEFT   JOIN msdb.dbo.sysmail_principalprofile pp
       ON  pp.profile_id = p.profile_id AND pp.principal_sid = 0x00
ORDER  BY p.name`

	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("gosmo: list mail profiles: %w", err)
	}
	defer rows.Close()

	var profiles []*MailProfile
	for rows.Next() {
		p := &MailProfile{}
		if err := rows.Scan(&p.ProfileID, &p.Name, &p.Description, &p.IsDefault); err != nil {
			return nil, err
		}
		profiles = append(profiles, p)
	}
	return profiles, rows.Err()
}

// SendMail sends an email via Database Mail (sp_send_dbmail).
func (s *Server) SendMail(profile, recipients, subject, body string) error {
	return s.SendMailContext(context.Background(), profile, recipients, subject, body)
}

func (s *Server) SendMailContext(ctx context.Context, profile, recipients, subject, body string) error {
	q := fmt.Sprintf(
		"EXEC msdb.dbo.sp_send_dbmail @profile_name = N'%s', @recipients = N'%s', "+
			"@subject = N'%s', @body = N'%s'",
		escapeSingle(profile), escapeSingle(recipients),
		escapeSingle(subject), escapeSingle(body),
	)
	if err := s.execContext(ctx, q); err != nil {
		return fmt.Errorf("gosmo: send mail: %w", err)
	}
	return nil
}
