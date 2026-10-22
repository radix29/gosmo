package gosmo

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// The writes and reads that took a positional bool take a named mode type
// instead, so x.Drop(ctx, true) no longer compiles and a call site says what
// it asks for. A mode type is an int, so a value outside its constants still
// compiles: each must be refused as an invalid request before anything
// reaches the server, never read as the zero (or the other) arm.
func TestUnrecognizedModeIsRefused(t *testing.T) {
	const bad = 9
	writes := map[string]func(ctx context.Context, d *Database) error{
		"Database.Drop":   func(ctx context.Context, d *Database) error { return d.Drop(ctx, bad) },
		"Database.Rename": func(ctx context.Context, d *Database) error { return d.Rename(ctx, "appdb2", bad) },
		"Table.Drop":      func(ctx context.Context, d *Database) error { return d.TableRef("dbo", "t").Drop(ctx, bad) },
		"BackupDevice.Drop": func(ctx context.Context, d *Database) error {
			return d.server.BackupDeviceRef("dev").Drop(ctx, bad)
		},
		"FullTextIndex.AddColumn": func(ctx context.Context, d *Database) error {
			return d.TableRef("dbo", "t").FullTextIndexRef().AddColumn(ctx, FullTextIndexColumnSpec{Name: "c"}, bad)
		},
		"FullTextIndex.DropColumn": func(ctx context.Context, d *Database) error {
			return d.TableRef("dbo", "t").FullTextIndexRef().DropColumn(ctx, "c", bad)
		},
		"StoredProcedure.AddSignature": func(ctx context.Context, d *Database) error {
			return d.StoredProcedureRef("dbo", "p").AddSignature(ctx, Signer{Kind: SignerCertificate, Name: "c"}, bad)
		},
		"Trigger.DropSignature": func(ctx context.Context, d *Database) error {
			return d.TriggerRef("dbo", "tr").DropSignature(ctx, Signer{Kind: SignerCertificate, Name: "c"}, bad)
		},
		"Server.Reconfigure": func(ctx context.Context, d *Database) error { return d.server.Reconfigure(ctx, bad) },
		"Database.SetDatabaseScopedConfig": func(ctx context.Context, d *Database) error {
			return d.SetDatabaseScopedConfig(ctx, "MAXDOP", "4", bad)
		},
		"MailProfile.Grant": func(ctx context.Context, d *Database) error {
			return d.server.Mail().ProfileRef("p").Grant(ctx, "app", bad)
		},
		"MailProfile.SetGrantDefault": func(ctx context.Context, d *Database) error {
			return d.server.Mail().ProfileRef("p").SetGrantDefault(ctx, "app", bad)
		},
		"MasterKey.Regenerate": func(ctx context.Context, d *Database) error {
			return d.MasterKeyRef().Regenerate(ctx, "pw", bad, "")
		},
	}
	for name, write := range writes {
		d, ctx, script := scriptedDB(t)
		err := write(ctx, d)
		if !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("%s: error %v, want an invalid request", name, err)
		}
		if script.Len() != 0 {
			t.Errorf("%s: collected %v, want nothing", name, script.Statements())
		}
	}

	reads := map[string]func(d *Database) error{
		"Server.ActiveSessions": func(d *Database) error {
			_, err := d.server.ActiveSessions(context.Background(), bad)
			return err
		},
		"ReplicationAgent.Sessions": func(d *Database) error {
			a := &ReplicationAgent{dd: &DistributionDatabase{srv: d.server, Name: "distribution"}, Name: "a"}
			_, err := a.Sessions(context.Background(), 0, bad)
			return err
		},
	}
	for name, read := range reads {
		err := read(qsRecDB(t, 17, nil, nil))
		if !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("%s: error %v, want an invalid request", name, err)
		}
		if calls := qsRec.recorded(); len(calls) != 0 {
			t.Errorf("%s: sent %v, want nothing", name, calls)
		}
	}
}

// TestReconfigureStatement pins the two forms of Server.Reconfigure.
func TestReconfigureStatement(t *testing.T) {
	for mode, want := range map[ReconfigureMode]string{
		ReconfigureChecked:  "RECONFIGURE",
		ReconfigureOverride: "RECONFIGURE WITH OVERRIDE",
	} {
		ctx, script := WithScript(context.Background())
		if err := (&Server{}).Reconfigure(ctx, mode); err != nil {
			t.Fatalf("Reconfigure(%d): %v", mode, err)
		}
		if got := script.Statements(); len(got) != 1 || got[0] != want {
			t.Errorf("Reconfigure(%d) = %v, want [%s]", mode, got, want)
		}
	}
}

// TestActiveSessionsScope: UserSessionsOnly filters to user processes,
// AllSessions does not.
func TestActiveSessionsScope(t *testing.T) {
	for scope, wantFilter := range map[SessionScope]bool{UserSessionsOnly: true, AllSessions: false} {
		d := qsRecDB(t, 17, nil, nil)
		if _, err := d.server.ActiveSessions(context.Background(), scope); err != nil {
			t.Fatalf("ActiveSessions(%d): %v", scope, err)
		}
		if got := strings.Contains(qsRec.last(t).sql, "is_user_process = 1"); got != wantFilter {
			t.Errorf("ActiveSessions(%d): user-process filter %v, want %v", scope, got, wantFilter)
		}
	}
}

// TestReplicationAgentSessionsFilter pins the @session_type each filter
// binds: 1 is every session, 2 only the failed ones.
func TestReplicationAgentSessionsFilter(t *testing.T) {
	for filter, want := range map[ReplSessionFilter]int{AllReplSessions: 1, FailedReplSessions: 2} {
		d := qsRecDB(t, 17, nil, nil)
		a := &ReplicationAgent{dd: &DistributionDatabase{srv: d.server, Name: "distribution"}, Kind: ReplSnapshotAgent, Name: "a"}
		if _, err := a.Sessions(context.Background(), 0, filter); err != nil {
			t.Fatalf("Sessions(%d): %v", filter, err)
		}
		call := qsRec.last(t)
		if len(call.args) != 3 || call.args[2] != int64(want) {
			t.Errorf("Sessions(%d) bound %v, want @session_type %d", filter, call.args, want)
		}
	}
}
