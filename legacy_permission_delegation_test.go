package gosmo

import (
	"context"
	"testing"
)

// The twelve Grant/Deny/Revoke methods at object, schema, database and
// server scope, called with the zero PermissionOptions. This pins the exact
// statement and the exact validation error each produces — the plain form
// every caller without a modifier gets. It began as the pin for moving each
// plain method onto the shared permissionStmt renderer (2026-08-05), and
// stayed when the plain/WithOptions pairs were merged into one method per
// verb (2026-09-23).
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
				return d.GrantPermission(ctx, "dbo", "Orders", PermSelect, "app_reader", PermissionOptions{})
			},
			badPermission: func(d *Database, _ *Server, ctx context.Context) error {
				return d.GrantPermission(ctx, "dbo", "Orders", ObjectPermission("NOPE"), "app_reader", PermissionOptions{})
			},
			wantStmt: "GRANT SELECT ON [dbo].[Orders] TO [app_reader]",
			wantErr:  `gosmo: grant permission: unrecognized permission "NOPE"`,
		},
		{
			name: "object deny",
			call: func(d *Database, _ *Server, ctx context.Context) error {
				return d.DenyPermission(ctx, "dbo", "Orders", PermSelect, "app_reader", PermissionOptions{})
			},
			badPermission: func(d *Database, _ *Server, ctx context.Context) error {
				return d.DenyPermission(ctx, "dbo", "Orders", ObjectPermission("NOPE"), "app_reader", PermissionOptions{})
			},
			wantStmt: "DENY SELECT ON [dbo].[Orders] TO [app_reader]",
			wantErr:  `gosmo: deny permission: unrecognized permission "NOPE"`,
		},
		{
			name: "object revoke",
			call: func(d *Database, _ *Server, ctx context.Context) error {
				return d.RevokePermission(ctx, "dbo", "Orders", PermSelect, "app_reader", PermissionOptions{})
			},
			badPermission: func(d *Database, _ *Server, ctx context.Context) error {
				return d.RevokePermission(ctx, "dbo", "Orders", ObjectPermission("NOPE"), "app_reader", PermissionOptions{})
			},
			wantStmt: "REVOKE SELECT ON [dbo].[Orders] FROM [app_reader]",
			wantErr:  `gosmo: revoke permission: unrecognized permission "NOPE"`,
		},
		{
			name: "schema grant",
			call: func(d *Database, _ *Server, ctx context.Context) error {
				return d.GrantSchemaPermission(ctx, "sales", PermSelect, "app_reader", PermissionOptions{})
			},
			badPermission: func(d *Database, _ *Server, ctx context.Context) error {
				return d.GrantSchemaPermission(ctx, "sales", ObjectPermission("NOPE"), "app_reader", PermissionOptions{})
			},
			wantStmt: "GRANT SELECT ON SCHEMA::[sales] TO [app_reader]",
			wantErr:  `gosmo: grant schema permission: unrecognized permission "NOPE"`,
		},
		{
			name: "schema deny",
			call: func(d *Database, _ *Server, ctx context.Context) error {
				return d.DenySchemaPermission(ctx, "sales", PermUpdate, "app_reader", PermissionOptions{})
			},
			badPermission: func(d *Database, _ *Server, ctx context.Context) error {
				return d.DenySchemaPermission(ctx, "sales", ObjectPermission("NOPE"), "app_reader", PermissionOptions{})
			},
			wantStmt: "DENY UPDATE ON SCHEMA::[sales] TO [app_reader]",
			wantErr:  `gosmo: deny schema permission: unrecognized permission "NOPE"`,
		},
		{
			name: "schema revoke",
			call: func(d *Database, _ *Server, ctx context.Context) error {
				return d.RevokeSchemaPermission(ctx, "sales", PermExecute, "app_reader", PermissionOptions{})
			},
			badPermission: func(d *Database, _ *Server, ctx context.Context) error {
				return d.RevokeSchemaPermission(ctx, "sales", ObjectPermission("NOPE"), "app_reader", PermissionOptions{})
			},
			wantStmt: "REVOKE EXECUTE ON SCHEMA::[sales] FROM [app_reader]",
			wantErr:  `gosmo: revoke schema permission: unrecognized permission "NOPE"`,
		},
		{
			name: "database grant",
			call: func(d *Database, _ *Server, ctx context.Context) error {
				return d.GrantDatabasePermission(ctx, "CREATE TABLE", "app_reader", PermissionOptions{})
			},
			badPermission: func(d *Database, _ *Server, ctx context.Context) error {
				return d.GrantDatabasePermission(ctx, "NOPE", "app_reader", PermissionOptions{})
			},
			wantStmt: "GRANT CREATE TABLE TO [app_reader]",
			wantErr:  `gosmo: grant database permission: unrecognized permission "NOPE"`,
		},
		{
			name: "database deny",
			call: func(d *Database, _ *Server, ctx context.Context) error {
				return d.DenyDatabasePermission(ctx, "CREATE TABLE", "app_reader", PermissionOptions{})
			},
			badPermission: func(d *Database, _ *Server, ctx context.Context) error {
				return d.DenyDatabasePermission(ctx, "NOPE", "app_reader", PermissionOptions{})
			},
			wantStmt: "DENY CREATE TABLE TO [app_reader]",
			wantErr:  `gosmo: deny database permission: unrecognized permission "NOPE"`,
		},
		{
			name: "database revoke",
			call: func(d *Database, _ *Server, ctx context.Context) error {
				return d.RevokeDatabasePermission(ctx, "CREATE TABLE", "app_reader", PermissionOptions{})
			},
			badPermission: func(d *Database, _ *Server, ctx context.Context) error {
				return d.RevokeDatabasePermission(ctx, "NOPE", "app_reader", PermissionOptions{})
			},
			wantStmt: "REVOKE CREATE TABLE FROM [app_reader]",
			wantErr:  `gosmo: revoke database permission: unrecognized permission "NOPE"`,
		},
		{
			name: "server grant",
			call: func(_ *Database, s *Server, ctx context.Context) error {
				return s.GrantServerPermission(ctx, "VIEW SERVER STATE", "app_login", PermissionOptions{})
			},
			badPermission: func(_ *Database, s *Server, ctx context.Context) error {
				return s.GrantServerPermission(ctx, "NOPE", "app_login", PermissionOptions{})
			},
			wantStmt: "USE master; GRANT VIEW SERVER STATE TO [app_login]",
			wantErr:  `gosmo: grant server permission: unrecognized permission "NOPE"`,
		},
		{
			name: "server deny",
			call: func(_ *Database, s *Server, ctx context.Context) error {
				return s.DenyServerPermission(ctx, "VIEW SERVER STATE", "app_login", PermissionOptions{})
			},
			badPermission: func(_ *Database, s *Server, ctx context.Context) error {
				return s.DenyServerPermission(ctx, "NOPE", "app_login", PermissionOptions{})
			},
			wantStmt: "USE master; DENY VIEW SERVER STATE TO [app_login]",
			wantErr:  `gosmo: deny server permission: unrecognized permission "NOPE"`,
		},
		{
			name: "server revoke",
			call: func(_ *Database, s *Server, ctx context.Context) error {
				return s.RevokeServerPermission(ctx, "VIEW SERVER STATE", "app_login", PermissionOptions{})
			},
			badPermission: func(_ *Database, s *Server, ctx context.Context) error {
				return s.RevokeServerPermission(ctx, "NOPE", "app_login", PermissionOptions{})
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
			if len(script.Statements()) != 0 {
				t.Errorf("a rejected call still issued %v", script.Statements())
			}
		})
	}
}
