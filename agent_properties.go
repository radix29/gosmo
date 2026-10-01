package gosmo

// agent_properties.go reads and writes SQL Server Agent's own settings — what
// SSMS shows under SQL Server Agent Properties. Only the Alert System page's
// mail profile so far: which Database Mail profile Agent sends operator and
// job notifications through.
//
// Agent keeps these in the registry, not msdb. msdb.dbo.sp_get_sqlagent_properties
// returns neither value (its email_profile and email_save_in_sent_folder
// columns are the obsolete SQL Mail settings), so the read goes to
// xp_instance_regread as SSMS's does; the write goes through
// msdb.dbo.sp_set_sqlagent_properties, which writes the same two values and
// then tells a running Agent to reload them.
//
// SQL Server on Linux takes these settings from mssql.conf instead
// (mssql-conf set sqlagent.databasemailprofile). Its emulated registry
// accepts the write and keeps nothing, and the read answers "on, no profile"
// whatever was written (SQL Server 2025 on Ubuntu) — so both refuse there
// with ErrAgentSettingsInMssqlConf rather than report a success that is not
// one.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// ErrAgentSettingsInMssqlConf reports an Agent settings read or write refused
// because the server runs on Linux, where Agent keeps them in mssql.conf,
// which no SQL connection reaches.
var ErrAgentSettingsInMssqlConf = errors.New("SQL Server Agent on Linux keeps this setting in mssql.conf (mssql-conf set sqlagent.databasemailprofile)")

// agentSettingsInRegistry refuses with ErrAgentSettingsInMssqlConf on Linux.
// An unknown platform is let through: the registry is where every other host
// keeps them.
func (s *Server) agentSettingsInRegistry(op string) error {
	if s.info != nil && s.info.Platform == "Linux" {
		return fmt.Errorf("gosmo: %s: %w", op, ErrAgentSettingsInMssqlConf)
	}
	return nil
}

// agentRegistryKey is the instance-relative key Agent's settings live under;
// xp_instance_regread/xp_instance_regwrite map it to the instance's own hive
// (on Linux, to the emulated registry).
const agentRegistryKey = `SOFTWARE\Microsoft\MSSQLServer\SQLServerAgent`

// AgentMailSettings is how SQL Server Agent sends notification mail: the
// Alert System page's "Enable mail profile" and its profile.
type AgentMailSettings struct {
	// Enabled is UseDatabaseMail. Off, Agent sends no e-mail to operators
	// whatever Profile holds.
	Enabled bool
	// Profile is DatabaseMailProfile — a Database Mail profile name, or ""
	// when none was ever set. It is not checked against msdb's profiles: one
	// renamed or deleted since is still what Agent tries.
	Profile string
}

// AgentMailSettings reads Agent's mail profile settings from the registry.
// A value never written reads as its zero — off, no profile.
//
// xp_instance_regread is executable by public and answers for this key
// without any further right (probed on major 17), so the read needs no Agent
// role; writing needs more — see SetAgentMailSettings. Refused on Linux —
// see the file comment.
func (s *Server) AgentMailSettings(ctx context.Context) (*AgentMailSettings, error) {
	if err := s.agentSettingsInRegistry("agent mail settings"); err != nil {
		return nil, err
	}
	const q = `
SET NOCOUNT ON;
DECLARE @use int, @profile nvarchar(256);
EXEC master.dbo.xp_instance_regread N'HKEY_LOCAL_MACHINE', N'` + agentRegistryKey + `', N'UseDatabaseMail', @use OUTPUT, N'no_output';
EXEC master.dbo.xp_instance_regread N'HKEY_LOCAL_MACHINE', N'` + agentRegistryKey + `', N'DatabaseMailProfile', @profile OUTPUT, N'no_output';
SELECT @use, @profile;`

	var use sql.NullInt64
	var profile sql.NullString
	if err := s.queryRowScan(ctx, q, nil, &use, &profile); err != nil {
		return nil, fmt.Errorf("gosmo: agent mail settings: %w", err)
	}
	return &AgentMailSettings{Enabled: use.Int64 != 0, Profile: profile.String}, nil
}

// AgentMailChanges is a write to Agent's mail profile settings, applied by
// SetAgentMailSettings. A nil field is left unchanged.
type AgentMailChanges struct {
	Enabled *bool
	// Profile set to "" stores an empty name: sp_set_sqlagent_properties
	// treats NULL as "leave alone", so there is no way back to the value
	// never having been written, and "" is what stands for none.
	Profile *string
}

// SetAgentMailSettings writes every field set on ch in one
// sp_set_sqlagent_properties call. An empty AgentMailChanges is a no-op and
// issues nothing.
//
// Permitted to sysadmin and to msdb db_owner. CONTROL SERVER without sysadmin
// is not enough, and fails late: the registry write goes through and the
// reload notification that follows (sp_sqlagent_notify) is refused, Msg 14260
// — so the error comes back with the change already made, and a running Agent
// picks it up only at its next start (probed on major 17). Refused on Linux
// before anything is sent — see the file comment.
func (s *Server) SetAgentMailSettings(ctx context.Context, ch AgentMailChanges) error {
	if err := s.agentSettingsInRegistry("set agent mail settings"); err != nil {
		return err
	}
	p := &agentParams{}
	if ch.Enabled != nil {
		p.flag("@use_databasemail", *ch.Enabled)
	}
	if ch.Profile != nil {
		p.str("@databasemail_profile", *ch.Profile)
	}
	if p.empty() {
		return nil
	}
	if err := s.exec(ctx, "EXEC msdb.dbo.sp_set_sqlagent_properties "+strings.Join(p.parts, ", ")); err != nil {
		return fmt.Errorf("gosmo: set agent mail settings: %w", err)
	}
	return nil
}
