//go:build livedb

// Live verification of DatabaseCapabilities' DATABASE_PRINCIPAL (class 4)
// block: which class-4 DENYs SQL Server actually enforces, and which it
// records and then ignores.
//
//	go test -tags livedb . -run TestLivePrincipalCapabilities -v \
//	  -livedb 'sqlserver://sa:PASS@host?TrustServerCertificate=true'
//
// Creates and drops its own throwaway database and login; touches nothing else.
package gosmo

import (
	"context"
	"database/sql"
	"strings"
	"testing"
)

// TestLivePrincipalCapabilitiesMatchWhatTheServerEnforces pins the two results
// that shaped the class-4 block, both of which read the other way round if
// assumed rather than measured — and both of which were identical on majors
// 13, 14 and 17 when this was written (2026-09-04), so a difference here is a
// real behaviour change and not a version gap.
//
//   - On a *user*, a class-4 DENY beats the database-wide ALTER ANY USER: both
//     ALTER USER ... WITH NAME and DROP USER are refused. That is the whole
//     reason ExplicitPrincipalPermissions exists — HAS_PERMS_BY_NAME keeps
//     answering 1 for ALTER ANY USER throughout, so nothing but the catalog
//     can say the write will fail.
//   - On a *role*, the same DENY is recorded and enforces nothing: DROP ROLE
//     and ALTER ROLE ... WITH NAME check ALTER ANY ROLE at database scope and
//     go through. gosmo records the row because the catalog does; a caller
//     that gates a role rename or drop on it withholds an action the server
//     allows. gossms's rightAlterAnyDBRole deliberately declares no
//     deniedOnPrincipal for this reason.
//
// The grant direction is asserted too, and it is the reason the block reads
// DENY rows only: GRANT ALTER ON USER::x reads back 1 and permits neither
// statement, so unlike object and schema scope there is no narrow grant here
// for a wider map to miss.
func TestLivePrincipalCapabilitiesMatchWhatTheServerEnforces(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()

	const login = "gosmo_live_principalcaps"
	// The grant direction needs a login of its own, and the first draft of this
	// test proved why: asserted through the login above, which holds
	// ALTER ANY USER for the denial half, the rename succeeds — through the
	// database-wide right, not through the per-user grant. Two logins is what
	// makes the two questions independent.
	const grantee = "gosmo_live_principalcaps_g"
	const pass = "P@ssw0rd_gosmo_live"

	d, drop := liveScratchDB(t, db, ctx, "gosmo_principalcaps_live")
	defer drop()

	for _, l := range []string{login, grantee} {
		db.ExecContext(ctx, "IF SUSER_ID('"+l+"') IS NOT NULL DROP LOGIN ["+l+"]")
		if _, err := db.ExecContext(ctx, "CREATE LOGIN ["+l+"] WITH PASSWORD = '"+pass+"', CHECK_POLICY = OFF"); err != nil {
			t.Fatalf("create login %s: %v", l, err)
		}
		defer db.ExecContext(context.Background(), "DROP LOGIN ["+l+"]")
	}

	liveExecIn(t, d, ctx,
		`CREATE USER [`+login+`] FOR LOGIN [`+login+`]`,
		// The database-wide rights the class-4 DENY has to beat.
		`GRANT ALTER ANY USER TO [`+login+`]`,
		`GRANT ALTER ANY ROLE TO [`+login+`]`,
		`CREATE USER denied_user WITHOUT LOGIN`,
		`CREATE USER free_user WITHOUT LOGIN`,
		`CREATE ROLE denied_role`,
		// The grant direction's fixture: a login whose only right in this
		// database is ALTER on one user.
		`CREATE USER granted_only WITHOUT LOGIN`,
		`CREATE USER [`+grantee+`] FOR LOGIN [`+grantee+`]`,
		`DENY ALTER ON USER::denied_user TO [`+login+`]`,
		`DENY ALTER ON ROLE::denied_role TO [`+login+`]`,
		`GRANT ALTER ON USER::granted_only TO [`+grantee+`]`,
	)

	pool, err := sql.Open("sqlserver", liveRestrictedDSN(t, login, pass))
	if err != nil {
		t.Fatalf("open as %s: %v", login, err)
	}
	defer pool.Close()

	// Not NewServer, for live_schemacaps_test.go's reason: loadInfo reads
	// server-scope DMVs this login has no rights to.
	restricted := &Server{db: pool}
	caps, err := restricted.Database(d.Name()).CapabilitiesContext(ctx)
	if err != nil {
		t.Fatalf("CapabilitiesContext as %s: %v", login, err)
	}
	if !caps.Accessible {
		t.Fatal("the login cannot open the database it was given a user in")
	}

	// The database-wide grant is intact throughout — the fixture is worthless
	// without this, since a denial that only agreed with a right the login had
	// lost would prove nothing.
	if !caps.Permits("ALTER ANY USER") {
		t.Fatal("ALTER ANY USER did not read back; this fixture no longer isolates the class-4 DENY")
	}

	if !caps.DeniedOnPrincipal("denied_user", "ALTER") {
		t.Error("the class-4 DENY on a user did not read back")
	}
	if caps.DeniedOnPrincipal("free_user", "ALTER") {
		t.Error("a user carrying no DENY read as denied — the map is sparse, so silence is not a deny")
	}
	if !caps.DeniedOnPrincipal("denied_role", "ALTER") {
		t.Error("the class-4 DENY on a role did not read back; the block should record it even though nothing may gate on it")
	}
	// The grant row belongs to the other login, so its absence here pins the
	// block's *principal* filter — and only that. Asserting the state filter
	// from this capability set would be asserting a negative loosely: the row
	// is not in this session's principal set to begin with, and dropping
	// `p.state = 'D'` from the query leaves this passing. The state filter is
	// asserted through gcaps below, where the row really is.
	if caps.DeniedOnPrincipal("granted_only", "ALTER") {
		t.Error("a row granted to another login read back — the block's principal filter is gone")
	}

	// The oracle: what the server actually accepts from this login. A probe
	// that agreed with itself and not with the server would pass everything
	// above.
	exec := func(stmt string) error {
		_, err := pool.ExecContext(ctx, "USE ["+d.Name()+"]; "+stmt)
		return err
	}
	if err := exec("DROP USER denied_user"); err == nil {
		t.Error("the server dropped a user carrying a class-4 DENY — DeniedOnPrincipal would be withholding nothing real")
	} else if !strings.Contains(err.Error(), "denied_user") {
		t.Errorf("the drop failed for some other reason than the denial: %v", err)
	}
	if err := exec("DROP USER free_user"); err != nil {
		t.Errorf("the server refused a user the login was denied nothing on: %v", err)
	}
	// The asymmetry, and the assertion this file exists for: the role's DENY
	// reads back above and enforces nothing here.
	if err := exec("ALTER ROLE denied_role WITH NAME = denied_role_x"); err != nil {
		t.Errorf("the server refused a rename of a role carrying a class-4 DENY, which it permitted on majors 13, 14 and 17: %v", err)
	}
	if err := exec("DROP ROLE denied_role_x"); err != nil {
		t.Errorf("the server refused a drop of a role carrying a class-4 DENY, which it permitted on majors 13, 14 and 17: %v", err)
	}
	// The grant direction, asked of the login that holds the per-user grant and
	// nothing else: HAS_PERMS_BY_NAME says 1 on the user, and neither statement
	// goes through, because both need ALTER ANY USER at database scope. That is
	// what makes the block DENY-only — there is no narrow grant here for a
	// wider map to miss, unlike object and schema scope.
	gpool, err := sql.Open("sqlserver", liveRestrictedDSN(t, grantee, pass))
	if err != nil {
		t.Fatalf("open as %s: %v", grantee, err)
	}
	defer gpool.Close()
	gcaps, err := (&Server{db: gpool}).Database(d.Name()).CapabilitiesContext(ctx)
	if err != nil {
		t.Fatalf("CapabilitiesContext as %s: %v", grantee, err)
	}
	if gcaps.Permits("ALTER ANY USER") {
		t.Fatal("the grantee holds ALTER ANY USER; this fixture no longer isolates the per-user grant")
	}
	// The state filter, asserted where the GRANT row actually is. The block
	// selects a literal 0 as its state column, so a row that gets past
	// `p.state = 'D'` reads back as a denial whatever the catalog says it is —
	// which would withhold every write on a user the login was *granted*
	// ALTER on. Verified as a real mutation check: removing the state filter
	// fails here and nowhere else.
	if gcaps.DeniedOnPrincipal("granted_only", "ALTER") {
		t.Error("a GRANT row read back as a denial — the block's state filter is gone")
	}
	gexec := func(stmt string) error {
		_, err := gpool.ExecContext(ctx, "USE ["+d.Name()+"]; "+stmt)
		return err
	}
	if err := gexec("ALTER USER granted_only WITH NAME = granted_only_x"); err == nil {
		t.Error("GRANT ALTER ON USER::x alone permitted a rename; the block would then need a grant direction too")
	}
	if err := gexec("DROP USER granted_only"); err == nil {
		t.Error("GRANT ALTER ON USER::x alone permitted a drop; the block would then need a grant direction too")
	}
}
