//go:build livedb

// Live verification of H4 (review plan 2026-10-03b). UserByName,
// ServerRoleByName and CategoryByName built their result from the caller's
// spelling and never read the name column, so on a case-insensitive server a
// wrong-case lookup came back named as typed — a name the catalog doesn't
// hold, which then mismatched a listing's spelling or went into a script.
//
//	go test -tags livedb . -run TestLiveByNameReturnsCatalogSpelling -v \
//	  -livedb 'sqlserver://sa:PASS@host?TrustServerCertificate=true'
//
// Creates and drops one throwaway database and one Agent alert category.
package gosmo

import (
	"context"
	"testing"
)

func TestLiveByNameReturnsCatalogSpelling(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()

	d, drop := liveScratchDB(t, db, ctx, "gosmo_byname_case_live")
	defer drop()
	srv := d.Server()
	if !CollationIgnoresCase(srv.Info().Collation) || !CollationIgnoresCase(d.Collation) {
		t.Skipf("server %q / database %q collation is case-sensitive; a wrong-case lookup finds nothing",
			srv.Info().Collation, d.Collation)
	}

	liveExecIn(t, d, ctx, "CREATE USER [gosmo_Case_User] WITHOUT LOGIN")
	u, err := d.UserByName(ctx, "GOSMO_CASE_USER")
	if err != nil {
		t.Fatalf("UserByName wrong case: %v", err)
	}
	if u.Name != "gosmo_Case_User" {
		t.Errorf("UserByName(GOSMO_CASE_USER).Name = %q, want the catalog's gosmo_Case_User", u.Name)
	}

	r, err := srv.ServerRoleByName(ctx, "SYSADMIN")
	if err != nil {
		t.Fatalf("ServerRoleByName wrong case: %v", err)
	}
	if r.Name != "sysadmin" {
		t.Errorf("ServerRoleByName(SYSADMIN).Name = %q, want the catalog's sysadmin", r.Name)
	}

	const cat = "gosmo_Case_Cat"
	dropCat := func() {
		db.ExecContext(context.Background(), `IF EXISTS (SELECT 1 FROM msdb.dbo.syscategories WHERE category_class = 2 AND name = N'`+cat+`')
	EXEC msdb.dbo.sp_delete_category @class = N'ALERT', @name = N'`+cat+`'`)
	}
	dropCat()
	defer dropCat()
	if _, err := db.ExecContext(ctx, "EXEC msdb.dbo.sp_add_category @class = N'ALERT', @type = N'NONE', @name = N'"+cat+"'"); err != nil {
		t.Fatalf("sp_add_category: %v", err)
	}
	c, err := srv.CategoryByName(ctx, CategoryClassAlert, "GOSMO_CASE_CAT")
	if err != nil {
		t.Fatalf("CategoryByName wrong case: %v", err)
	}
	if c.Name != cat || c.Class != CategoryClassAlert {
		t.Errorf("CategoryByName(GOSMO_CASE_CAT) = {Name: %q, Class: %q}, want {%q, %q}", c.Name, c.Class, cat, CategoryClassAlert)
	}
}
