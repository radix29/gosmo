//go:build livedb

// Live verification that PermissionsForPrincipal, ColumnPermissionsForPrincipal
// and FindSecurables report every class ApplyPermission takes, under the class
// ApplyPermission takes back — so a grant read from one is revocable through
// the other (review plan 2026-10-15 § P3).
//
//	go test -tags livedb . -run TestLivePermissionsForPrincipal -v \
//	  -livedb 'sqlserver://sa:PASS@host?TrustServerCertificate=true'
//
// Creates and drops its own throwaway database; touches nothing else.
package gosmo

import (
	"slices"
	"testing"
)

func TestLivePermissionsForPrincipalEveryClass(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()

	d, drop := liveScratchDB(t, db, ctx, "gosmo_pfp_live")
	defer drop()

	// A WITHOUT LOGIN user, never dbo: a grant to the owner is a silent
	// no-op that writes no row.
	liveExecIn(t, d, ctx,
		`CREATE USER pfp_user WITHOUT LOGIN`,
		`CREATE TABLE dbo.pfp_t (c INT)`,
		`CREATE VIEW dbo.pfp_v AS SELECT c FROM dbo.pfp_t`,
		`CREATE PROCEDURE dbo.pfp_p AS SELECT 1`,
		`CREATE FUNCTION dbo.pfp_sf() RETURNS INT AS BEGIN RETURN 1 END`,
		`CREATE FUNCTION dbo.pfp_if() RETURNS TABLE AS RETURN SELECT 1 AS x, 2 AS y`,
		`CREATE FUNCTION dbo.pfp_tf() RETURNS @r TABLE (x INT, y INT) AS BEGIN INSERT @r VALUES (1, 2); RETURN END`,
		`CREATE SEQUENCE dbo.pfp_sq`,
		`CREATE SYNONYM dbo.pfp_syn FOR dbo.pfp_p`,
		`CREATE TYPE dbo.pfp_ty FROM INT`,
		`CREATE TYPE dbo.pfp_tt AS TABLE (x INT)`,
		`CREATE CERTIFICATE pfp_ce ENCRYPTION BY PASSWORD = 'Pfp-Pa55-word!' WITH SUBJECT = 'pfp'`,
	)

	grants := []struct {
		sec  Securable
		perm ObjectPermission
	}{
		{Securable{Class: SecurableSchema, Name: "dbo"}, PermExecute},
		{Securable{Class: SecurableTable, Schema: "dbo", Name: "pfp_t"}, PermSelect},
		{Securable{Class: SecurableView, Schema: "dbo", Name: "pfp_v"}, PermSelect},
		{Securable{Class: SecurableProcedure, Schema: "dbo", Name: "pfp_p"}, PermExecute},
		{Securable{Class: SecurableScalarFunction, Schema: "dbo", Name: "pfp_sf"}, PermExecute},
		{Securable{Class: SecurableInlineFunction, Schema: "dbo", Name: "pfp_if"}, PermSelect},
		{Securable{Class: SecurableTableFunction, Schema: "dbo", Name: "pfp_tf"}, PermSelect},
		{Securable{Class: SecurableSequence, Schema: "dbo", Name: "pfp_sq"}, PermUpdate},
		{Securable{Class: SecurableSynonym, Schema: "dbo", Name: "pfp_syn"}, PermExecute},
		{Securable{Class: SecurableUserType, Schema: "dbo", Name: "pfp_ty"}, PermReferences},
		{Securable{Class: SecurableUserType, Schema: "dbo", Name: "pfp_tt"}, PermExecute},
		{Securable{Class: SecurableCertificate, Name: "pfp_ce"}, PermControl},
	}
	for _, g := range grants {
		if err := d.ApplyPermission(ctx, VerbGrant, g.sec, g.perm, "pfp_user", PermissionOptions{}); err != nil {
			t.Fatalf("grant %s on %s %s: %v", g.perm, g.sec.Class, g.sec.Name, err)
		}
	}

	t.Run("PermissionsForPrincipal", func(t *testing.T) {
		got, err := d.PermissionsForPrincipal(ctx, "pfp_user")
		if err != nil {
			t.Fatal(err)
		}
		for _, g := range grants {
			if !slices.ContainsFunc(got, func(e *PrincipalSecurable) bool {
				return e.SecurableType == g.sec.Class && e.Schema == g.sec.Schema && e.Name == g.sec.Name &&
					e.Permission == string(g.perm) && e.State == PermissionGrant
			}) {
				t.Errorf("%s %s on %s.%s not reported", g.sec.Class, g.perm, g.sec.Schema, g.sec.Name)
			}
		}
		for _, e := range got {
			if e.SecurableType == "" {
				t.Errorf("entry with no class: %+v", e)
			}
		}
	})

	t.Run("columns of table-valued functions", func(t *testing.T) {
		for _, sec := range []Securable{
			{Class: SecurableInlineFunction, Schema: "dbo", Name: "pfp_if", Columns: []string{"x"}},
			{Class: SecurableTableFunction, Schema: "dbo", Name: "pfp_tf", Columns: []string{"x"}},
		} {
			for _, name := range sec.Class.ColumnPermissionNames() {
				if err := d.ApplyPermission(ctx, VerbDeny, sec, ObjectPermission(name), "pfp_user", PermissionOptions{}); err != nil {
					t.Errorf("deny %s (x) on %s: %v", name, sec.Name, err)
				}
			}
		}
		got, err := d.ColumnPermissionsForPrincipal(ctx, "pfp_user")
		if err != nil {
			t.Fatal(err)
		}
		classes := map[string]SecurableClass{}
		for _, e := range got {
			classes[e.Object] = e.ObjectType
		}
		if classes["pfp_if"] != SecurableInlineFunction || classes["pfp_tf"] != SecurableTableFunction {
			t.Errorf("column entries' classes = %v", classes)
		}
		if n := len(got); n != 5 {
			t.Errorf("%d column entries, want 5 (3 on the inline function, 2 on the table function): %+v", n, got)
		}
	})

	t.Run("FindSecurables", func(t *testing.T) {
		got, err := d.FindSecurables(ctx, SecurableSearch{Name: "pfp_"})
		if err != nil {
			t.Fatal(err)
		}
		for _, g := range grants[1:] { // the schema is dbo, not a pfp_ name
			want := Securable{Class: g.sec.Class, Schema: g.sec.Schema, Name: g.sec.Name}
			if !slices.ContainsFunc(got, func(s Securable) bool {
				return s.Class == want.Class && s.Schema == want.Schema && s.Name == want.Name && s.Columns == nil
			}) {
				t.Errorf("%+v not found in %+v", want, got)
			}
		}
		for i := 1; i < len(got); i++ {
			if slices.Index(securableSearchOrder, got[i-1].Class) > slices.Index(securableSearchOrder, got[i].Class) {
				t.Errorf("%+v listed before %+v", got[i-1], got[i])
			}
		}
	})

	// Every entry read back is revocable as read: its class, schema and name
	// are what ApplyPermission takes.
	t.Run("revoke as read", func(t *testing.T) {
		got, err := d.PermissionsForPrincipal(ctx, "pfp_user")
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range got {
			if e.SecurableType == SecurableDatabase {
				continue // CONNECT, which the user needs
			}
			sec := Securable{Class: e.SecurableType, Schema: e.Schema, Name: e.Name}
			if err := d.ApplyPermission(ctx, VerbRevoke, sec, ObjectPermission(e.Permission), "pfp_user", PermissionOptions{}); err != nil {
				t.Errorf("revoke %+v: %v", e, err)
			}
		}
		cols, err := d.ColumnPermissionsForPrincipal(ctx, "pfp_user")
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range cols {
			sec := Securable{Class: e.ObjectType, Schema: e.Schema, Name: e.Object, Columns: []string{e.Column}}
			if err := d.ApplyPermission(ctx, VerbRevoke, sec, e.Permission, "pfp_user", PermissionOptions{}); err != nil {
				t.Errorf("revoke %+v: %v", e, err)
			}
		}
		left, err := d.PermissionsForPrincipal(ctx, "pfp_user")
		if err != nil {
			t.Fatal(err)
		}
		cols, err = d.ColumnPermissionsForPrincipal(ctx, "pfp_user")
		if err != nil {
			t.Fatal(err)
		}
		if len(left) != 1 || left[0].SecurableType != SecurableDatabase || len(cols) != 0 {
			t.Errorf("after revoking everything read: %+v, columns %+v", left, cols)
		}
	})
}
