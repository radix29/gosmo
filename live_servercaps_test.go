//go:build livedb

// Live verification of Capabilities' server-scope catalog block: which
// server-class DENYs SQL Server actually enforces, and which it records and
// then ignores.
//
//	go test -tags livedb . -run TestLiveServerCapabilities -v \
//	  -livedb 'sqlserver://sa:PASS@host?TrustServerCertificate=true'
//
// Creates and drops its own throwaway logins, server role and endpoints;
// touches nothing that was already there.
package gosmo

import (
	"context"
	"database/sql"
	"strings"
	"testing"
)

// TestLiveServerCapabilitiesMatchWhatTheServerEnforces pins the three results
// that shaped the server-scope block, each of which reads the other way round
// if assumed rather than measured — and each of which was identical on majors
// 13 and 17 when this was written (2026-09-04), so a difference here is a real
// behaviour change and not a version gap.
//
//   - On a *login*, a class-101 DENY beats the server-wide ALTER ANY LOGIN,
//     all-or-nothing: ALTER LOGIN (rename and password alike) and DROP LOGIN
//     are refused, Msg 15151.
//   - On a *server role*, the same class and the same DENY split: the
//     membership edits are refused and the rename and the drop go through.
//     That is the database role's shape, not the login's, and a gate reading
//     one answer for both withholds an action the server allows.
//   - On an *endpoint* (class 105), ALTER ENDPOINT is refused, and with a
//     different number — Msg 6004 rather than 15151.
//   - The *member* of a membership edit is not checked, which is where the
//     server scope parts company with the database scope: adding a user
//     carrying a class-4 DENY to an undenied database role is refused, and
//     adding a login carrying a class-101 DENY to an undenied server role goes
//     through (major 17, 2026-09-05). A server-scope membership gate therefore
//     asks about the role and nothing else.
//
// HAS_PERMS_BY_NAME reads 0 for the denied ALTER in every one of these rows,
// including the two the server goes on to allow, which is why the block is a
// catalog read and why the gate has to be asked per action rather than per
// object.
func TestLiveServerCapabilitiesMatchWhatTheServerEnforces(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()

	const login = "gosmo_live_servercaps"
	const pass = "P@ssw0rd_gosmo_live"
	const deniedLogin = "gosmo_live_denied_login"
	const freeLogin = "gosmo_live_free_login"
	const deniedRole = "gosmo_live_denied_role"
	const freeRole = "gosmo_live_free_role"
	const deniedEP = "gosmo_live_denied_ep"

	exec := func(stmt string) {
		t.Helper()
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	cleanup := func(stmt string) { db.ExecContext(context.Background(), stmt) }

	for _, l := range []string{login, deniedLogin, freeLogin} {
		db.ExecContext(ctx, "IF SUSER_ID('"+l+"') IS NOT NULL DROP LOGIN ["+l+"]")
		exec("CREATE LOGIN [" + l + "] WITH PASSWORD = '" + pass + "', CHECK_POLICY = OFF")
		defer cleanup("DROP LOGIN [" + l + "]")
	}
	for _, r := range []string{deniedRole, freeRole} {
		db.ExecContext(ctx, "IF SUSER_ID('"+r+"') IS NOT NULL DROP SERVER ROLE ["+r+"]")
		exec("CREATE SERVER ROLE [" + r + "]")
		defer cleanup("DROP SERVER ROLE [" + r + "]")
	}
	// SERVICE_BROKER, and the choice is not arbitrary — **do not make this a
	// TSQL endpoint.** Creating the first user-defined TSQL endpoint revokes
	// public's CONNECT on TSQL Default TCP, permanently and instance-wide, and
	// dropping the endpoint again does not put it back: the first draft of
	// this test did exactly that and every SQL login on the instance stopped
	// being able to connect until the grant was restored by hand. A mirroring
	// endpoint is no good either, since an instance may hold only one and on
	// the instance this was written against that one is real.
	//
	// STOPPED means nothing listens on the port, so the endpoint is inert for
	// as long as it exists.
	db.ExecContext(ctx, "IF EXISTS (SELECT 1 FROM sys.endpoints WHERE name = '"+deniedEP+"') DROP ENDPOINT ["+deniedEP+"]")
	exec("CREATE ENDPOINT [" + deniedEP + "] STATE = STOPPED AS TCP (LISTENER_PORT = 45123) FOR SERVICE_BROKER (AUTHENTICATION = WINDOWS)")
	defer cleanup("DROP ENDPOINT [" + deniedEP + "]")
	// The undenied control is whatever user endpoint the instance already has,
	// because a second disposable one cannot be created: SERVICE_BROKER and
	// DATABASE_MIRRORING are both one per instance and TSQL is barred above.
	// ALTER ENDPOINT ... STATE = <its current state> mutates nothing, so
	// borrowing a real endpoint for the positive half is safe; where there is
	// none, that half is skipped rather than faked.
	var freeEP, freeEPState string
	db.QueryRowContext(ctx,
		"SELECT TOP 1 name, state_desc FROM sys.endpoints WHERE endpoint_id > 65535 AND name <> @p1 ORDER BY endpoint_id",
		deniedEP).Scan(&freeEP, &freeEPState)
	exec("GRANT ALTER ANY LOGIN TO [" + login + "]")
	exec("GRANT ALTER ANY SERVER ROLE TO [" + login + "]")
	exec("GRANT ALTER ANY ENDPOINT TO [" + login + "]")
	exec("DENY ALTER ON LOGIN::[" + deniedLogin + "] TO [" + login + "]")
	exec("DENY ALTER ON SERVER ROLE::[" + deniedRole + "] TO [" + login + "]")
	exec("DENY ALTER ON ENDPOINT::[" + deniedEP + "] TO [" + login + "]")
	// Revoked before anything is dropped, and that ordering is load-bearing:
	// a login named as the *securable* of a permission row cannot be dropped
	// while the row stands, and the deferred drops run in reverse order — so
	// without this the denied login outlives the test, every time.
	defer cleanup("REVOKE ALTER ON LOGIN::[" + deniedLogin + "] TO [" + login + "]")
	defer cleanup("REVOKE ALTER ON SERVER ROLE::[" + deniedRole + "] TO [" + login + "]")
	defer cleanup("REVOKE ALTER ON ENDPOINT::[" + deniedEP + "] TO [" + login + "]")

	pool, err := sql.Open("sqlserver", liveRestrictedDSN(t, login, pass))
	if err != nil {
		t.Fatalf("open as %s: %v", login, err)
	}
	defer pool.Close()

	// Not NewServer, for live_schemacaps_test.go's reason: loadInfo reads
	// server-scope DMVs this login has no rights to.
	restricted := &Server{db: pool}
	caps, err := restricted.CapabilitiesContext(ctx)
	if err != nil {
		t.Fatalf("CapabilitiesContext as %s: %v", login, err)
	}

	// The server-wide grants are intact throughout — the fixture is worthless
	// without this, since a denial that only agreed with a right the login had
	// lost would prove nothing.
	for _, p := range []string{"ALTER ANY LOGIN", "ALTER ANY SERVER ROLE", "ALTER ANY ENDPOINT"} {
		if !caps.Allows(p) {
			t.Fatalf("%s did not read back; this fixture no longer isolates the server-class DENY", p)
		}
	}

	if !caps.DeniedOnLogin(deniedLogin, "ALTER") {
		t.Error("the class-101 DENY on a login did not read back")
	}
	if caps.DeniedOnLogin(freeLogin, "ALTER") {
		t.Error("a login carrying no DENY read as denied — the map is sparse, so silence is not a deny")
	}
	if !caps.DeniedOnServerRole(deniedRole, "ALTER") {
		t.Error("the class-101 DENY on a server role did not read back")
	}
	if caps.DeniedOnServerRole(freeRole, "ALTER") {
		t.Error("a server role carrying no DENY read as denied")
	}
	if !caps.DeniedOnEndpoint(deniedEP, "ALTER") {
		t.Error("the class-105 DENY on an endpoint did not read back")
	}
	if freeEP != "" && caps.DeniedOnEndpoint(freeEP, "ALTER") {
		t.Error("an endpoint carrying no DENY read as denied")
	}
	// The kind is part of the key, and only a live read can show the query
	// puts the right one there: a login answered for under SERVER ROLE would
	// make the whole split unreachable.
	if caps.DeniedOnServerRole(deniedLogin, "ALTER") {
		t.Error("a denied login read back under the SERVER ROLE kind — type_desc is not being read")
	}
	if caps.DeniedOnLogin(deniedRole, "ALTER") {
		t.Error("a denied server role read back under the LOGIN kind — type_desc is not being read")
	}

	// The oracle: what the server actually accepts from this login. A probe
	// that agreed with itself and not with the server would pass everything
	// above.
	as := func(stmt string) error {
		_, err := pool.ExecContext(ctx, stmt)
		return err
	}
	if err := as("ALTER LOGIN [" + deniedLogin + "] WITH NAME = [" + deniedLogin + "_x]"); err == nil {
		db.ExecContext(ctx, "ALTER LOGIN ["+deniedLogin+"_x] WITH NAME = ["+deniedLogin+"]")
		t.Error("the server renamed a login carrying a class-101 DENY — DeniedOnLogin would be withholding nothing real")
	} else if !strings.Contains(err.Error(), deniedLogin) {
		t.Errorf("the rename failed for some other reason than the denial: %v", err)
	}
	if err := as("DROP LOGIN [" + deniedLogin + "]"); err == nil {
		t.Error("the server dropped a login carrying a class-101 DENY; the login's DENY is not all-or-nothing after all")
	}
	if err := as("ALTER LOGIN [" + freeLogin + "] WITH NAME = [" + freeLogin + "_x]"); err != nil {
		t.Errorf("the server refused a rename of a login the gate was denied nothing on: %v", err)
	} else {
		db.ExecContext(ctx, "ALTER LOGIN ["+freeLogin+"_x] WITH NAME = ["+freeLogin+"]")
	}

	// The server role's split, and the assertion this file exists for: the
	// membership edit is refused and the rename beside it goes through.
	if err := as("ALTER SERVER ROLE [" + deniedRole + "] ADD MEMBER [" + freeLogin + "]"); err == nil {
		db.ExecContext(ctx, "ALTER SERVER ROLE ["+deniedRole+"] DROP MEMBER ["+freeLogin+"]")
		t.Error("the server added a member to a role carrying a class-101 DENY — the membership gate would be withholding nothing real")
	}
	if err := as("ALTER SERVER ROLE [" + deniedRole + "] WITH NAME = [" + deniedRole + "_x]"); err != nil {
		t.Errorf("the server refused a rename of a server role carrying a class-101 DENY, which it permitted on majors 13 and 17: %v", err)
	} else {
		db.ExecContext(ctx, "ALTER SERVER ROLE ["+deniedRole+"_x] WITH NAME = ["+deniedRole+"]")
	}
	// The member side, and the *third* asymmetry: at database scope, adding a
	// user carrying a class-4 DENY to an undenied role is refused. At server
	// scope it is not — measured on major 17, 2026-09-05, and reproduced
	// outside this test. So the membership question is answered by the role
	// alone here, and a gate that also asked about the login being added would
	// withhold an edit the server performs.
	defer cleanup("ALTER SERVER ROLE [" + freeRole + "] DROP MEMBER [" + deniedLogin + "]")
	if err := as("ALTER SERVER ROLE [" + freeRole + "] ADD MEMBER [" + deniedLogin + "]"); err != nil {
		t.Errorf("the server refused a denied login as a member of an undenied role, which it permitted on major 17: %v", err)
	} else {
		db.ExecContext(ctx, "ALTER SERVER ROLE ["+freeRole+"] DROP MEMBER ["+deniedLogin+"]")
	}
	if err := as("ALTER SERVER ROLE [" + freeRole + "] ADD MEMBER [" + freeLogin + "]"); err != nil {
		t.Errorf("the server refused a membership edit nothing was denied on: %v", err)
	} else {
		db.ExecContext(ctx, "ALTER SERVER ROLE ["+freeRole+"] DROP MEMBER ["+freeLogin+"]")
	}

	// The endpoint, whose refusal carries its own number. STATE = STOPPED is
	// what both endpoints are already in, so the statement mutates nothing
	// either way and the permission check is the whole of the answer.
	if err := as("ALTER ENDPOINT [" + deniedEP + "] STATE = STOPPED"); err == nil {
		t.Error("the server altered an endpoint carrying a class-105 DENY — DeniedOnEndpoint would be withholding nothing real")
	} else if !strings.Contains(err.Error(), "6004") && !strings.Contains(err.Error(), deniedEP) {
		t.Logf("the endpoint refusal did not name Msg 6004 or the endpoint; recorded for the next audit: %v", err)
	}
	if freeEP == "" {
		t.Log("the instance has no second user endpoint; the undenied endpoint half was not asked")
	} else if err := as("ALTER ENDPOINT [" + freeEP + "] STATE = " + freeEPState); err != nil {
		t.Errorf("the server refused an ALTER on an endpoint nothing was denied on: %v", err)
	}
}
