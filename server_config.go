package gosmo

import (
	"context"
	"database/sql"
	"fmt"
)

// ============================================================
// Server Configuration  (sp_configure)
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
	IsDynamic   bool // no restart needed
	IsAdvanced  bool
	Description string
}

// Configurations returns all SQL Server configuration options.
func (s *Server) Configurations() ([]*ConfigurationOption, error) {
	const q = `
SELECT configuration_id, name, value, value_in_use,
       minimum, maximum, is_dynamic, is_advanced, description
FROM   sys.configurations
ORDER  BY name`

	rows, err := s.db.QueryContext(context.Background(), q)
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

// ConfigurationByName returns a single configuration option by name.
func (s *Server) ConfigurationByName(name string) (*ConfigurationOption, error) {
	cfgs, err := s.Configurations()
	if err != nil {
		return nil, err
	}
	for _, c := range cfgs {
		if c.Name == name {
			return c, nil
		}
	}
	return nil, fmt.Errorf("gosmo: configuration option %q not found", name)
}

// SetValue changes the value of the configuration option using sp_configure.
// Call Reconfigure() after setting non-dynamic options.
func (c *ConfigurationOption) SetValue(value int64) error {
	_, err := c.server.db.ExecContext(context.Background(),
		fmt.Sprintf("EXEC sp_configure N'%s', %d", escapeSingle(c.Name), value))
	if err != nil {
		return fmt.Errorf("gosmo: set configuration %q = %d: %w", c.Name, value, err)
	}
	c.Value = value
	return nil
}

// Reconfigure applies pending configuration changes (RECONFIGURE WITH OVERRIDE).
func (s *Server) Reconfigure(override bool) error {
	q := "RECONFIGURE"
	if override {
		q += " WITH OVERRIDE"
	}
	_, err := s.db.ExecContext(context.Background(), q)
	if err != nil {
		return fmt.Errorf("gosmo: reconfigure: %w", err)
	}
	return nil
}

// ============================================================
// Active Sessions / Connections
// ============================================================

// ActiveSession holds information about a running session.
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

// ActiveSessions returns all non-sleeping active sessions.
// Pass includeSystem=true to also include system sessions.
func (s *Server) ActiveSessions(includeSystem bool) ([]*ActiveSession, error) {
	sysFilter := "AND s.is_user_process = 1"
	if includeSystem {
		sysFilter = ""
	}
	q := fmt.Sprintf(`
SELECT s.session_id, s.login_name, s.host_name, s.program_name,
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

	rows, err := s.db.QueryContext(context.Background(), q)
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

// KillSession terminates a session by ID.
func (s *Server) KillSession(sessionID int) error {
	_, err := s.db.ExecContext(context.Background(),
		fmt.Sprintf("KILL %d", sessionID))
	if err != nil {
		return fmt.Errorf("gosmo: kill session %d: %w", sessionID, err)
	}
	return nil
}

// ============================================================
// Audit / Error Log
// ============================================================

// ErrorLogEntry represents one row from the SQL Server error log.
type ErrorLogEntry struct {
	LogDate string
	Process string
	Text    string
}

// ReadErrorLog reads the current SQL Server error log.
// Pass logNumber=0 for the current log, 1 for the first archive, etc.
func (s *Server) ReadErrorLog(logNumber int) ([]*ErrorLogEntry, error) {
	rows, err := s.db.QueryContext(context.Background(),
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

// CycleErrorLog closes the current error log and opens a new one (sp_cycle_errorlog).
func (s *Server) CycleErrorLog() error {
	_, err := s.db.ExecContext(context.Background(), "EXEC sp_cycle_errorlog")
	if err != nil {
		return fmt.Errorf("gosmo: cycle error log: %w", err)
	}
	return nil
}

// ============================================================
// Database Mail  (no WMI required)
// ============================================================

// MailProfile represents an msdb Database Mail profile.
type MailProfile struct {
	ProfileID   int
	Name        string
	Description string
	IsDefault   bool
}

// MailProfiles returns all Database Mail profiles.
func (s *Server) MailProfiles() ([]*MailProfile, error) {
	const q = `
SELECT p.profile_id, p.name, ISNULL(p.description,''),
       ISNULL(pp.is_default, 0)
FROM   msdb.dbo.sysmail_profile p
LEFT   JOIN msdb.dbo.sysmail_principalprofile pp
       ON pp.profile_id = p.profile_id AND pp.principal_sid = 0x00
ORDER  BY p.name`

	rows, err := s.db.QueryContext(context.Background(), q)
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

// SendMail sends an e-mail using Database Mail (sp_send_dbmail).
func (s *Server) SendMail(profile, recipients, subject, body string) error {
	q := fmt.Sprintf(`
EXEC msdb.dbo.sp_send_dbmail
    @profile_name  = N'%s',
    @recipients    = N'%s',
    @subject       = N'%s',
    @body          = N'%s'`,
		escapeSingle(profile),
		escapeSingle(recipients),
		escapeSingle(subject),
		escapeSingle(body),
	)
	_, err := s.db.ExecContext(context.Background(), q)
	if err != nil {
		return fmt.Errorf("gosmo: send mail: %w", err)
	}
	return nil
}
