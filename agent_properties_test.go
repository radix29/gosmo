package gosmo

import (
	"context"
	"errors"
	"testing"
)

// The two registry values go through sp_set_sqlagent_properties by name, a
// field left nil is not sent at all (NULL there means "leave alone", so
// sending one would be harmless but sending the zero value would not be), and
// a profile name is a literal with its apostrophes doubled.
func TestSetAgentMailSettingsStatementShapes(t *testing.T) {
	s := &Server{}
	cases := []struct {
		name string
		ch   AgentMailChanges
		want string
	}{
		{"both", AgentMailChanges{Enabled: new(true), Profile: new("Ops")},
			"EXEC msdb.dbo.sp_set_sqlagent_properties @use_databasemail = 1, @databasemail_profile = N'Ops'"},
		{"disable only", AgentMailChanges{Enabled: new(false)},
			"EXEC msdb.dbo.sp_set_sqlagent_properties @use_databasemail = 0"},
		{"profile only, quoted", AgentMailChanges{Profile: new("O'Brien")},
			"EXEC msdb.dbo.sp_set_sqlagent_properties @databasemail_profile = N'O''Brien'"},
		{"cleared profile", AgentMailChanges{Profile: new("")},
			"EXEC msdb.dbo.sp_set_sqlagent_properties @databasemail_profile = N''"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			wantOne(t, rgStatements(t, func(ctx context.Context) error { return s.SetAgentMailSettings(ctx, c.ch) }), c.want)
		})
	}
	if got := rgStatements(t, func(ctx context.Context) error { return s.SetAgentMailSettings(ctx, AgentMailChanges{}) }); len(got) != 0 {
		t.Errorf("empty changes issued %q, want nothing", got)
	}
}

// On Linux the registry write is silently dropped, so both halves refuse
// before sending anything — under WithScript too, since a captured
// statement that would do nothing is no better than an executed one.
func TestAgentMailSettingsRefusedOnLinux(t *testing.T) {
	s := &Server{info: &ServerInfo{Platform: "Linux"}}
	ctx, col := WithScript(context.Background())
	if err := s.SetAgentMailSettings(ctx, AgentMailChanges{Enabled: new(true)}); !errors.Is(err, ErrAgentSettingsInMssqlConf) {
		t.Errorf("SetAgentMailSettings on Linux: %v, want ErrAgentSettingsInMssqlConf", err)
	}
	if got := col.Statements(); len(got) != 0 {
		t.Errorf("SetAgentMailSettings on Linux captured %q, want nothing", got)
	}
	if _, err := s.AgentMailSettings(ctx); !errors.Is(err, ErrAgentSettingsInMssqlConf) {
		t.Errorf("AgentMailSettings on Linux: %v, want ErrAgentSettingsInMssqlConf", err)
	}
}
