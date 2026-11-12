//go:build livedb

// Live check of Database.Principals against the three single-family reads it
// replaces: same principals, same fields, in one round trip.
//
//	go test -tags livedb . -run TestLivePrincipals -v \
//	  -livedb 'sqlserver://sa:PASS@host?TrustServerCertificate=true'
//
// Creates and drops its own throwaway database; touches nothing else.
package gosmo

import (
	"reflect"
	"testing"
)

func TestLivePrincipalsMatchesTheFamilyReads(t *testing.T) {
	db, ctx, done := liveDB(t)
	t.Cleanup(done)

	d, drop := liveScratchDB(t, db, ctx, "gosmo_principals_live")
	t.Cleanup(drop)

	for _, stmt := range []string{
		"CREATE USER [pu]]1] WITHOUT LOGIN",
		"CREATE ROLE [pr'1] AUTHORIZATION [pu]]1]",
		"ALTER ROLE [pr'1] ADD MEMBER [pu]]1]",
		"CREATE APPLICATION ROLE [pa1] WITH PASSWORD = 'Pa1!" + "x9x9x9x9x9', DEFAULT_SCHEMA = [dbo]",
	} {
		if _, err := d.exec(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}

	p, err := d.Principals(ctx)
	if err != nil {
		t.Fatalf("Principals: %v", err)
	}
	users, err := d.Users(ctx)
	if err != nil {
		t.Fatalf("Users: %v", err)
	}
	if !reflect.DeepEqual(p.Users, users) {
		t.Errorf("Principals.Users differs from Users:\n%+v\n%+v", p.Users, users)
	}
	apps, err := d.ApplicationRoles(ctx)
	if err != nil {
		t.Fatalf("ApplicationRoles: %v", err)
	}
	if !reflect.DeepEqual(p.ApplicationRoles, apps) || len(apps) != 1 {
		t.Errorf("Principals.ApplicationRoles = %+v, ApplicationRoles = %+v", p.ApplicationRoles, apps)
	}
	roles, err := d.Roles(ctx)
	if err != nil {
		t.Fatalf("Roles: %v", err)
	}
	if len(p.Roles) != len(roles) {
		t.Fatalf("Principals has %d roles, Roles %d", len(p.Roles), len(roles))
	}
	for i, r := range roles {
		byName, err := d.RoleByName(ctx, r.Name)
		if err != nil {
			t.Fatalf("RoleByName(%q): %v", r.Name, err)
		}
		if !reflect.DeepEqual(p.Roles[i], byName) {
			t.Errorf("role %q: Principals = %+v, RoleByName = %+v", r.Name, p.Roles[i], byName)
		}
	}

	only, err := d.Principals(ctx, PrincipalKindApplicationRole)
	if err != nil {
		t.Fatalf("Principals(app roles): %v", err)
	}
	if only.Users != nil || only.Roles != nil || len(only.ApplicationRoles) != 1 {
		t.Errorf("Principals(app roles) = %+v, want only the one application role", only)
	}
}
