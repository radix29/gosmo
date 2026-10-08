//go:build livedb

// Live verification of ApplyPermission on the classes the 2026-10-08 API
// added: every class's allowlist entry is granted for real and read back from
// sys.database_permissions, and a server permission round-trips through
// Server.ApplyPermission.
//
//	go test -tags livedb . -run TestLiveApplyPermission -v \
//	  -livedb 'sqlserver://sa:PASS@host?TrustServerCertificate=true'
//
// Creates and drops its own throwaway database and login; touches nothing
// else.
package gosmo

import (
	"context"
	"testing"
)

func TestLiveApplyPermissionEveryClassAndName(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()

	d, drop := liveScratchDB(t, db, ctx, "gosmo_apply_perm_live")
	defer drop()

	// A WITHOUT LOGIN user, never dbo: a grant to the owner is a silent
	// no-op that writes no row.
	liveExecIn(t, d, ctx,
		`CREATE USER ap_user WITHOUT LOGIN`,
		`CREATE TABLE dbo.ap_t (c INT)`,
		`CREATE VIEW dbo.ap_v AS SELECT c FROM dbo.ap_t`,
		`CREATE PROCEDURE dbo.ap_p AS SELECT 1`,
		`CREATE FUNCTION dbo.ap_sf() RETURNS INT AS BEGIN RETURN 1 END`,
		`CREATE FUNCTION dbo.ap_if() RETURNS TABLE AS RETURN SELECT 1 AS x`,
		`CREATE FUNCTION dbo.ap_tf() RETURNS @r TABLE (x INT) AS BEGIN INSERT @r VALUES (1); RETURN END`,
		`CREATE SEQUENCE dbo.ap_sq`,
		`CREATE SYNONYM dbo.ap_syn FOR dbo.ap_p`,
		`CREATE TYPE dbo.ap_ty FROM INT`,
		`CREATE CERTIFICATE ap_ce ENCRYPTION BY PASSWORD = 'Ap-Pa55-word!' WITH SUBJECT = 'ap'`,
	)

	for _, sec := range []Securable{
		{Class: SecurableDatabase},
		{Class: SecurableSchema, Name: "dbo"},
		{Class: SecurableTable, Schema: "dbo", Name: "ap_t"},
		{Class: SecurableView, Schema: "dbo", Name: "ap_v"},
		{Class: SecurableProcedure, Schema: "dbo", Name: "ap_p"},
		{Class: SecurableScalarFunction, Schema: "dbo", Name: "ap_sf"},
		{Class: SecurableInlineFunction, Schema: "dbo", Name: "ap_if"},
		{Class: SecurableTableFunction, Schema: "dbo", Name: "ap_tf"},
		{Class: SecurableSequence, Schema: "dbo", Name: "ap_sq"},
		{Class: SecurableSynonym, Schema: "dbo", Name: "ap_syn"},
		{Class: SecurableUserType, Schema: "dbo", Name: "ap_ty"},
		{Class: SecurableCertificate, Name: "ap_ce"},
	} {
		t.Run(string(sec.Class), func(t *testing.T) {
			for _, name := range sec.Class.PermissionNames() {
				if sec.Class == SecurableDatabase && name != "CONNECT" && name != "SHOWPLAN" {
					continue // the database list is long and pinned elsewhere
				}
				perm := ObjectPermission(name)
				if err := d.ApplyPermission(ctx, VerbGrant, sec, perm, "ap_user", PermissionOptions{WithGrantOption: true}); err != nil {
					t.Errorf("grant %s: %v", name, err)
					continue
				}
				if err := d.ApplyPermission(ctx, VerbRevoke, sec, perm, "ap_user", PermissionOptions{GrantOptionOnly: true}); err != nil {
					t.Errorf("revoke grant option for %s: %v", name, err)
				}
				if err := d.ApplyPermission(ctx, VerbDeny, sec, perm, "ap_user", PermissionOptions{Cascade: true}); err != nil {
					t.Errorf("deny %s: %v", name, err)
				}
				var state string
				row := db.QueryRowContext(ctx, `
SELECT dp.state_desc
FROM   `+quoteIdent(d.Name)+`.sys.database_permissions dp
JOIN   `+quoteIdent(d.Name)+`.sys.database_principals pr ON pr.principal_id = dp.grantee_principal_id
WHERE  pr.name = N'ap_user' AND dp.permission_name = @p1 AND dp.class_desc = @p2`, name, classDesc(sec.Class))
				if err := row.Scan(&state); err != nil {
					t.Errorf("%s: no permission row after DENY: %v", name, err)
				} else if state != "DENY" {
					t.Errorf("%s: state %s after DENY", name, state)
				}
				if err := d.ApplyPermission(ctx, VerbRevoke, sec, perm, "ap_user", PermissionOptions{Cascade: true}); err != nil {
					t.Errorf("revoke %s: %v", name, err)
				}
			}
		})
	}

	t.Run("column", func(t *testing.T) {
		sec := Securable{Class: SecurableView, Schema: "dbo", Name: "ap_v", Columns: []string{"c"}}
		if err := d.ApplyPermission(ctx, VerbGrant, sec, PermSelect, "ap_user", PermissionOptions{}); err != nil {
			t.Fatal(err)
		}
		got, err := d.ColumnPermissions(ctx, "dbo", "ap_v")
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0].Column != "c" || got[0].ObjectType != "VIEW" {
			t.Errorf("column permissions = %+v", got)
		}
	})
}

// classDesc is sys.database_permissions.class_desc for a class.
func classDesc(c SecurableClass) string {
	switch c {
	case SecurableDatabase, SecurableSchema, SecurableCertificate:
		return string(c)
	case SecurableUserType:
		return "TYPE"
	}
	return "OBJECT_OR_COLUMN"
}

func TestLiveApplyPermissionServer(t *testing.T) {
	db, ctx, done := liveDB(t)
	t.Cleanup(done) // after the login's drop, which is a Cleanup too

	const login = "gosmo_apply_perm_login"
	srv := liveServer(t, db, ctx)
	if _, err := db.ExecContext(ctx, `CREATE LOGIN `+quoteIdent(login)+` WITH PASSWORD = 'Ap-Pa55-word!', CHECK_POLICY = OFF`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.ExecContext(context.Background(), `DROP LOGIN `+quoteIdent(login)) })

	sec := Securable{Class: SecurableServer}
	if err := srv.ApplyPermission(ctx, VerbGrant, sec, ServerPermission("VIEW SERVER STATE"), login, PermissionOptions{}); err != nil {
		t.Fatal(err)
	}
	perms, err := srv.ServerPermissions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, p := range perms {
		found = found || (p.Principal == login && p.Permission == "VIEW SERVER STATE" && p.State == "GRANT")
	}
	if !found {
		t.Error("VIEW SERVER STATE grant not read back")
	}
	if err := srv.ApplyPermission(ctx, VerbRevoke, sec, ServerPermission("VIEW SERVER STATE"), login, PermissionOptions{}); err != nil {
		t.Fatal(err)
	}
}
