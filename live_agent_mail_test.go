//go:build livedb

// Live Agent mail settings: write both registry values through
// SetAgentMailSettings, read them back through AgentMailSettings, and put back
// what was there.
//
//	go test -tags livedb . -run TestLiveAgentMailSettings -v \
//	  -livedb 'sqlserver://sa:PASS@host?TrustServerCertificate=true'
//
// The settings are instance-wide registry values, not msdb rows. A value that
// was never written is deleted again rather than restored as "" — the one
// state sp_set_sqlagent_properties cannot return to.
package gosmo

import (
	"context"
	"errors"
	"testing"
)

func TestLiveAgentMailSettings(t *testing.T) {
	db, ctx, done := liveDB(t)
	t.Cleanup(done) // registered first, so it runs after the restore below

	srv, err := NewServer(ctx, db)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	if srv.Info().Platform == "Linux" {
		// The registry there keeps nothing written to it; both halves refuse.
		if _, err := srv.AgentMailSettings(ctx); !errors.Is(err, ErrAgentSettingsInMssqlConf) {
			t.Errorf("AgentMailSettings on Linux: %v, want ErrAgentSettingsInMssqlConf", err)
		}
		return
	}
	before, err := srv.AgentMailSettings(ctx)
	if err != nil {
		t.Fatalf("AgentMailSettings: %v", err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		if err := srv.SetAgentMailSettings(ctx, AgentMailChanges{Enabled: &before.Enabled, Profile: &before.Profile}); err != nil {
			t.Errorf("restore: %v", err)
		}
		if before.Profile == "" {
			if _, err := db.ExecContext(ctx, `EXEC master.dbo.xp_instance_regdeletevalue N'HKEY_LOCAL_MACHINE', N'`+agentRegistryKey+`', N'DatabaseMailProfile'`); err != nil {
				t.Errorf("delete DatabaseMailProfile: %v", err)
			}
		}
	})

	// The profile need not exist: Agent stores the name unchecked.
	want := AgentMailSettings{Enabled: !before.Enabled, Profile: "gosmo_live_agent_mail'q"}
	if err := srv.SetAgentMailSettings(ctx, AgentMailChanges{Enabled: &want.Enabled, Profile: &want.Profile}); err != nil {
		t.Fatalf("SetAgentMailSettings: %v", err)
	}
	got, err := srv.AgentMailSettings(ctx)
	if err != nil {
		t.Fatalf("AgentMailSettings after write: %v", err)
	}
	if *got != want {
		t.Errorf("read back %+v, want %+v", *got, want)
	}

	// A nil field is left alone.
	if err := srv.SetAgentMailSettings(ctx, AgentMailChanges{Enabled: &before.Enabled}); err != nil {
		t.Fatalf("SetAgentMailSettings (enabled only): %v", err)
	}
	if got, err = srv.AgentMailSettings(ctx); err != nil {
		t.Fatalf("AgentMailSettings after second write: %v", err)
	}
	if got.Enabled != before.Enabled || got.Profile != want.Profile {
		t.Errorf("after an Enabled-only write: %+v, want enabled %v and profile %q kept", *got, before.Enabled, want.Profile)
	}
}
