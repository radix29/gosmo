//go:build livedb

// Live verification that DatabaseCapabilities' schema block answers for a
// principal whose only right is ALTER on one schema — the case the
// database-scope probe cannot see at all, and the reason the block exists.
//
//	go test -tags livedb . -run TestLiveSchemaCapabilities -v \
//	  -livedb 'sqlserver://sa:PASS@host?TrustServerCertificate=true'
//
// Creates and drops its own throwaway database and login; touches nothing else.
package gosmo

import (
	"context"
	"database/sql"
	"net/url"
	"testing"
)

// liveRestrictedDSN is the -livedb DSN with its credentials swapped for the
// throwaway login's. The probe has to run *as* that login: HAS_PERMS_BY_NAME
// answers for the session's own principal, so sa's connection reports sa's
// rights whatever database it is pointed at.
func liveRestrictedDSN(t *testing.T, user, pass string) string {
	t.Helper()
	u, err := url.Parse(*liveDSN)
	if err != nil {
		t.Fatalf("parse -livedb DSN: %v", err)
	}
	u.User = url.UserPassword(user, pass)
	return u.String()
}

func TestLiveSchemaCapabilitiesSeeAGrantTheDatabaseScopeCannot(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()

	const login = "gosmo_live_schemacaps"
	const pass = "P@ssw0rd_gosmo_live"

	d, drop := liveScratchDB(t, db, ctx, "gosmo_schemacaps_live")
	defer drop()

	db.ExecContext(ctx, "IF SUSER_ID('"+login+"') IS NOT NULL DROP LOGIN ["+login+"]")
	if _, err := db.ExecContext(ctx, "CREATE LOGIN ["+login+"] WITH PASSWORD = '"+pass+"', CHECK_POLICY = OFF"); err != nil {
		t.Fatalf("create login: %v", err)
	}
	defer db.ExecContext(context.Background(), "DROP LOGIN ["+login+"]")

	liveExecIn(t, d, ctx,
		`CREATE SCHEMA app`,
		`CREATE SCHEMA other`,
		`CREATE TABLE app.t1 (id INT NOT NULL)`,
		`CREATE TABLE other.t3 (id INT NOT NULL)`,
		`CREATE USER [`+login+`] FOR LOGIN [`+login+`]`,
		// The whole grant: ALTER on one schema, nothing database-wide.
		`GRANT ALTER ON SCHEMA::app TO [`+login+`]`,
	)

	pool, err := sql.Open("sqlserver", liveRestrictedDSN(t, login, pass))
	if err != nil {
		t.Fatalf("open as %s: %v", login, err)
	}
	defer pool.Close()

	// Not NewServer: loadInfo reads server-scope DMVs this login has no rights
	// to, and the probe is what is under test.
	restricted := &Server{db: pool}
	caps, err := restricted.Database(d.Name()).CapabilitiesContext(ctx)
	if err != nil {
		t.Fatalf("CapabilitiesContext as %s: %v", login, err)
	}

	if !caps.Accessible {
		t.Fatal("the login cannot open the database it was given a user in")
	}
	if !caps.HasOnSchema("app", "ALTER") {
		t.Error("ALTER on schema app did not read back for the login that holds it")
	}
	if caps.AllowsOnSchema("other", "ALTER") {
		t.Error("ALTER on schema other read as allowed for a login that was never granted it")
	}
	// The point of the whole block: none of the database-wide rights gossms
	// used to gate on can see this grant.
	for _, n := range []string{"ALTER", "CONTROL", "ALTER ANY SCHEMA"} {
		if caps.Allows(n) {
			t.Errorf("database-scope %s read as allowed — this fixture no longer isolates the schema grant", n)
		}
	}

	// The oracle: what the server actually accepts from this login. A probe
	// that agreed with itself and not with the server would pass everything
	// above.
	if _, err := pool.ExecContext(ctx, "USE ["+d.Name()+"]; EXEC sp_rename 'app.t1', 't1x'"); err != nil {
		t.Errorf("the server refused a rename inside the granted schema: %v", err)
	}
	if _, err := pool.ExecContext(ctx, "USE ["+d.Name()+"]; EXEC sp_rename 'other.t3', 't3x'"); err == nil {
		t.Error("the server accepted a rename inside a schema the login has no ALTER on")
	}
}
