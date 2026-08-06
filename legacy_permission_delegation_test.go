package gosmo

import (
	"context"
	"testing"
)

// The twelve plain Grant/Deny/RevokePermission*Context methods each rendered
// their own fmt.Sprintf before delegating to the shared permissionStmt
// renderer. This pins the exact statement and the exact validation error
// every one of them produces, so the delegation can be shown to change
// neither. Written against the pre-delegation code first, then re-run
// against it — a table here rather than in permission_options_test.go
// because it is about the legacy surface, not about the modifiers.
func TestLegacyPermissionMethodsRenderAndReject(t *testing.T) {
	cases := []struct {
		name string
		// call issues the statement; badPermission issues the same call with
		// a permission name no allowlist contains.
		call          func(d *Database, s *Server, ctx context.Context) error
		badPermission func(d *Database, s *Server, ctx context.Context) error
		wantStmt      string
		wantErr       string
	}{
		{
			name: "object grant",
			call: func(d *Database, _ *Server, ctx context.Context) error {
				return d.GrantPermissionContext(ctx, "dbo", "Orders", PermSelect, "app_reader")
			},
			badPermission: func(d *Database, _ *Server, ctx context.Context) error {
				return d.GrantPermissionContext(ctx, "dbo", "Orders", ObjectPermission("NOPE"), "app_reader")
			},
			wantStmt: "GRANT SELECT ON [dbo].[Orders] TO [app_reader]",
			wantErr:  `gosmo: grant permission: unrecognized permission "NOPE"`,
		},
		{
			name: "object deny",
			call: func(d *Database, _ *Server, ctx context.Context) error {
				return d.DenyPermissionContext(ctx, "dbo", "Orders", PermSelect, "app_reader")
			},
			badPermission: func(d *Database, _ *Server, ctx context.Context) error {
				return d.DenyPermissionContext(ctx, "dbo", "Orders", ObjectPermission("NOPE"), "app_reader")
			},
			wantStmt: "DENY SELECT ON [dbo].[Orders] TO [app_reader]",
			wantErr:  `gosmo: deny permission: unrecognized permission "NOPE"`,
		},
		{
			name: "object revoke",
			call: func(d *Database, _ *Server, ctx context.Context) error {
				return d.RevokePermissionContext(ctx, "dbo", "Orders", PermSelect, "app_reader")
			},
			badPermission: func(d *Database, _ *Server, ctx context.Context) error {
				return d.RevokePermissionContext(ctx, "dbo", "Orders", ObjectPermission("NOPE"), "app_reader")
			},
			wantStmt: "REVOKE SELECT ON [dbo].[Orders] FROM [app_reader]",
			wantErr:  `gosmo: revoke permission: unrecognized permission "NOPE"`,
		},
		{
			name: "schema grant",
			call: func(d *Database, _ *Server, ctx context.Context) error {
				return d.GrantSchemaPermissionContext(ctx, "sales", PermSelect, "app_reader")
			},
			badPermission: func(d *Database, _ *Server, ctx context.Context) error {
				return d.GrantSchemaPermissionContext(ctx, "sales", ObjectPermission("NOPE"), "app_reader")
			},
			wantStmt: "GRANT SELECT ON SCHEMA::[sales] TO [app_reader]",
			wantErr:  `gosmo: grant schema permission: unrecognized permission "NOPE"`,
		},
		{
			name: "schema deny",
			call: func(d *Database, _ *Server, ctx context.Context) error {
				return d.DenySchemaPermissionContext(ctx, "sales", PermUpdate, "app_reader")
			},
			badPermission: func(d *Database, _ *Server, ctx context.Context) error {
				return d.DenySchemaPermissionContext(ctx, "sales", ObjectPermission("NOPE"), "app_reader")
			},
			wantStmt: "DENY UPDATE ON SCHEMA::[sales] TO [app_reader]",
			wantErr:  `gosmo: deny schema permission: unrecognized permission "NOPE"`,
		},
		{
			name: "schema revoke",
			call: func(d *Database, _ *Server, ctx context.Context) error {
				return d.RevokeSchemaPermissionContext(ctx, "sales", PermExecute, "app_reader")
			},
			badPermission: func(d *Database, _ *Server, ctx context.Context) error {
				return d.RevokeSchemaPermissionContext(ctx, "sales", ObjectPermission("NOPE"), "app_reader")
			},
			wantStmt: "REVOKE EXECUTE ON SCHEMA::[sales] FROM [app_reader]",
			wantErr:  `gosmo: revoke schema permission: unrecognized permission "NOPE"`,
		},
		{
			name: "database grant",
			call: func(d *Database, _ *Server, ctx context.Context) error {
				return d.GrantDatabasePermissionContext(ctx, "CREATE TABLE", "app_reader")
			},
			badPermission: func(d *Database, _ *Server, ctx context.Context) error {
				return d.GrantDatabasePermissionContext(ctx, "NOPE", "app_reader")
			},
			wantStmt: "GRANT CREATE TABLE TO [app_reader]",
			wantErr:  `gosmo: grant database permission: unrecognized permission "NOPE"`,
		},
		{
			name: "database deny",
			call: func(d *Database, _ *Server, ctx context.Context) error {
				return d.DenyDatabasePermissionContext(ctx, "CREATE TABLE", "app_reader")
			},
			badPermission: func(d *Database, _ *Server, ctx context.Context) error {
				return d.DenyDatabasePermissionContext(ctx, "NOPE", "app_reader")
			},
			wantStmt: "DENY CREATE TABLE TO [app_reader]",
			wantErr:  `gosmo: deny database permission: unrecognized permission "NOPE"`,
		},
		{
			name: "database revoke",
			call: func(d *Database, _ *Server, ctx context.Context) error {
				return d.RevokeDatabasePermissionContext(ctx, "CREATE TABLE", "app_reader")
			},
			badPermission: func(d *Database, _ *Server, ctx context.Context) error {
				return d.RevokeDatabasePermissionContext(ctx, "NOPE", "app_reader")
			},
			wantStmt: "REVOKE CREATE TABLE FROM [app_reader]",
			wantErr:  `gosmo: revoke database permission: unrecognized permission "NOPE"`,
		},
		{
			name: "server grant",
			call: func(_ *Database, s *Server, ctx context.Context) error {
				return s.GrantServerPermissionContext(ctx, "VIEW SERVER STATE", "app_login")
			},
			badPermission: func(_ *Database, s *Server, ctx context.Context) error {
				return s.GrantServerPermissionContext(ctx, "NOPE", "app_login")
			},
			wantStmt: "USE master; GRANT VIEW SERVER STATE TO [app_login]",
			wantErr:  `gosmo: grant server permission: unrecognized permission "NOPE"`,
		},
		{
			name: "server deny",
			call: func(_ *Database, s *Server, ctx context.Context) error {
				return s.DenyServerPermissionContext(ctx, "VIEW SERVER STATE", "app_login")
			},
			badPermission: func(_ *Database, s *Server, ctx context.Context) error {
				return s.DenyServerPermissionContext(ctx, "NOPE", "app_login")
			},
			wantStmt: "USE master; DENY VIEW SERVER STATE TO [app_login]",
			wantErr:  `gosmo: deny server permission: unrecognized permission "NOPE"`,
		},
		{
			name: "server revoke",
			call: func(_ *Database, s *Server, ctx context.Context) error {
				return s.RevokeServerPermissionContext(ctx, "VIEW SERVER STATE", "app_login")
			},
			badPermission: func(_ *Database, s *Server, ctx context.Context) error {
				return s.RevokeServerPermissionContext(ctx, "NOPE", "app_login")
			},
			wantStmt: "USE master; REVOKE VIEW SERVER STATE FROM [app_login]",
			wantErr:  `gosmo: revoke server permission: unrecognized permission "NOPE"`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, ctx, script := scriptedDB(t)
			if err := tc.call(d, d.server, ctx); err != nil {
				t.Fatalf("call: %v", err)
			}
			if got := onlyStatement(t, script); got != tc.wantStmt {
				t.Errorf("statement =\n%q\nwant\n%q", got, tc.wantStmt)
			}

			d, ctx, script = scriptedDB(t)
			err := tc.badPermission(d, d.server, ctx)
			if err == nil {
				t.Fatal("an unrecognized permission name was accepted")
			}
			if err.Error() != tc.wantErr {
				t.Errorf("error =\n%q\nwant\n%q", err.Error(), tc.wantErr)
			}
			if len(script.Statements) != 0 {
				t.Errorf("a rejected call still issued %v", script.Statements)
			}
		})
	}
}
