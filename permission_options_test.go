package gosmo

import (
	"context"
	"testing"
)

// scriptedDB returns a Database handle plus a script context, so a write
// method's exact statement text can be asserted without a server.
func scriptedDB(t *testing.T) (*Database, context.Context, *ScriptCollector) {
	t.Helper()
	ctx, script := WithScript(context.Background())
	return &Database{Name: "appdb", server: &Server{}}, ctx, script
}

// onlyStatement returns the single statement collected, without the USE
// Database.exec records it under.
func onlyStatement(t *testing.T, script *ScriptCollector) string {
	t.Helper()
	if len(script.Entries) != 1 {
		t.Fatalf("collected %d statements, want 1: %v", len(script.Entries), script.Statements())
	}
	return script.Entries[0].SQL
}

func TestPermissionOptionsRenderWithGrantOption(t *testing.T) {
	d, ctx, script := scriptedDB(t)
	err := d.GrantPermissionWithOptions(ctx, "dbo", "Orders", PermSelect, "app_reader",
		PermissionOptions{WithGrantOption: true})
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	want := "GRANT SELECT ON [dbo].[Orders] TO [app_reader] WITH GRANT OPTION"
	if got := onlyStatement(t, script); got != want {
		t.Errorf("statement =\n%q\nwant\n%q", got, want)
	}
}

func TestPermissionOptionsRenderRevokeGrantOptionFor(t *testing.T) {
	d, ctx, script := scriptedDB(t)
	// GRANT OPTION FOR always carries CASCADE — SQL Server rejects it
	// without one, so it must not depend on the caller also setting Cascade.
	err := d.RevokePermissionWithOptions(ctx, "dbo", "Orders", PermSelect, "app_reader",
		PermissionOptions{GrantOptionOnly: true})
	if err != nil {
		t.Fatalf("revoke: %v", err)
	}
	want := "REVOKE GRANT OPTION FOR SELECT ON [dbo].[Orders] FROM [app_reader] CASCADE"
	if got := onlyStatement(t, script); got != want {
		t.Errorf("statement =\n%q\nwant\n%q", got, want)
	}
}

func TestPermissionOptionsRenderDenyCascade(t *testing.T) {
	d, ctx, script := scriptedDB(t)
	err := d.DenySchemaPermissionWithOptions(ctx, "sales", PermSelect, "app_reader",
		PermissionOptions{Cascade: true})
	if err != nil {
		t.Fatalf("deny: %v", err)
	}
	want := "DENY SELECT ON SCHEMA::[sales] TO [app_reader] CASCADE"
	if got := onlyStatement(t, script); got != want {
		t.Errorf("statement =\n%q\nwant\n%q", got, want)
	}
}

// The zero PermissionOptions must render exactly what the plain trio renders
// — that equivalence is what lets the UI call one path for every state.
// Kept, but note what it can and cannot catch now: since the plain methods
// delegate to the WithOptions ones, both sides of each pair run the same
// code, so this can no longer detect two renderers drifting apart — it
// asserts the delegation is still in place. The statements and error strings
// themselves are pinned literally in
// TestLegacyPermissionMethodsRenderAndReject.
func TestZeroPermissionOptionsMatchesPlainStatement(t *testing.T) {
	cases := []struct {
		name        string
		plain, opts func(d *Database, ctx context.Context) error
	}{
		{"object grant",
			func(d *Database, ctx context.Context) error {
				return d.GrantPermission(ctx, "dbo", "Orders", PermSelect, "app_reader")
			},
			func(d *Database, ctx context.Context) error {
				return d.GrantPermissionWithOptions(ctx, "dbo", "Orders", PermSelect, "app_reader", PermissionOptions{})
			}},
		{"object revoke",
			func(d *Database, ctx context.Context) error {
				return d.RevokePermission(ctx, "dbo", "Orders", PermSelect, "app_reader")
			},
			func(d *Database, ctx context.Context) error {
				return d.RevokePermissionWithOptions(ctx, "dbo", "Orders", PermSelect, "app_reader", PermissionOptions{})
			}},
		{"schema deny",
			func(d *Database, ctx context.Context) error {
				return d.DenySchemaPermission(ctx, "sales", PermUpdate, "app_reader")
			},
			func(d *Database, ctx context.Context) error {
				return d.DenySchemaPermissionWithOptions(ctx, "sales", PermUpdate, "app_reader", PermissionOptions{})
			}},
		{"database grant",
			func(d *Database, ctx context.Context) error {
				return d.GrantDatabasePermission(ctx, "CREATE TABLE", "app_reader")
			},
			func(d *Database, ctx context.Context) error {
				return d.GrantDatabasePermissionWithOptions(ctx, "CREATE TABLE", "app_reader", PermissionOptions{})
			}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, ctx, script := scriptedDB(t)
			if err := tc.plain(d, ctx); err != nil {
				t.Fatalf("plain: %v", err)
			}
			plain := onlyStatement(t, script)

			d, ctx, script = scriptedDB(t)
			if err := tc.opts(d, ctx); err != nil {
				t.Fatalf("with options: %v", err)
			}
			if got := onlyStatement(t, script); got != plain {
				t.Errorf("zero-options statement =\n%q\nplain statement =\n%q", got, plain)
			}
		})
	}
}

// A modifier the verb has no form for is an error, not a silently dropped
// field: a DENY that quietly loses its WITH GRANT OPTION would look like it
// worked.
func TestPermissionOptionsRejectMismatchedModifier(t *testing.T) {
	d, ctx, script := scriptedDB(t)

	if err := d.DenyPermissionWithOptions(ctx, "dbo", "Orders", PermSelect, "app_reader",
		PermissionOptions{WithGrantOption: true}); err == nil {
		t.Error("DENY accepted WITH GRANT OPTION, want an error")
	}
	if err := d.GrantPermissionWithOptions(ctx, "dbo", "Orders", PermSelect, "app_reader",
		PermissionOptions{Cascade: true}); err == nil {
		t.Error("GRANT accepted CASCADE, want an error")
	}
	if err := d.GrantPermissionWithOptions(ctx, "dbo", "Orders", PermSelect, "app_reader",
		PermissionOptions{GrantOptionOnly: true}); err == nil {
		t.Error("GRANT accepted GRANT OPTION FOR, want an error")
	}
	if len(script.Statements()) != 0 {
		t.Errorf("a rejected statement was still collected: %v", script.Statements())
	}
}

func TestServerPermissionWithOptionsKeepsUseMasterPrefix(t *testing.T) {
	ctx, script := WithScript(context.Background())
	s := &Server{}
	if err := s.GrantServerPermissionWithOptions(ctx, "VIEW SERVER STATE", "app_login",
		PermissionOptions{WithGrantOption: true}); err != nil {
		t.Fatalf("grant: %v", err)
	}
	if len(script.Statements()) != 1 {
		t.Fatalf("collected %d statements, want 1: %v", len(script.Statements()), script.Statements())
	}
	want := "USE master; GRANT VIEW SERVER STATE TO [app_login] WITH GRANT OPTION"
	if got := script.Statements()[0]; got != want {
		t.Errorf("statement =\n%q\nwant\n%q", got, want)
	}
}

func TestPermissionWithOptionsStillRejectsUnknownPermission(t *testing.T) {
	d, ctx, _ := scriptedDB(t)
	if err := d.GrantPermissionWithOptions(ctx, "dbo", "Orders",
		ObjectPermission("SELECT; DROP TABLE Orders; --"), "attacker", PermissionOptions{}); err == nil {
		t.Error("an unrecognized permission name was accepted, want an error")
	}
	if err := d.GrantDatabasePermissionWithOptions(ctx, "CONTROL; DROP DATABASE appdb; --",
		"attacker", PermissionOptions{}); err == nil {
		t.Error("an unrecognized database permission name was accepted, want an error")
	}
	s := &Server{}
	if err := s.RevokeServerPermissionWithOptions(ctx, "NOT REAL", "sa", PermissionOptions{}); err == nil {
		t.Error("an unrecognized server permission name was accepted, want an error")
	}
}
