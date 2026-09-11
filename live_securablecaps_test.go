//go:build livedb

// Live verification of DatabaseCapabilities' class 5/6/10 block: that the
// effective CONTROL it reads for a user-defined type and an XML schema
// collection is the answer the server enforces for ALTER SCHEMA ... TRANSFER.
//
//	go test -tags livedb . -run TestLiveSecurableCapabilities -v \
//	  -livedb 'sqlserver://sa:PASS@host?TrustServerCertificate=true'
//
// Creates and drops its own throwaway database and login; touches nothing else.
package gosmo

import (
	"context"
	"database/sql"
	"testing"
)

// TestLiveSecurableCapabilitiesMatchWhatTheServerEnforces pins the results
// recorded on ProbedSecurablePermissions, identical on majors 13, 14 and 17
// when this was written (2026-09-11):
//
//   - CONTROL reads 1 through a grant on the securable and through its
//     ownership, and the transfer goes through for both.
//   - db_ddladmin, which sees every type and drops it, reads 0 — and the
//     transfer is refused. This is the reading gossms's Move to Schema gate
//     rests on, so it is asserted against the server rather than assumed.
//   - DENY CONTROL hides the securable, so it has no row at all. That is why
//     the block has no DENY half; a server that stopped hiding it would need
//     one, and this is where that shows.
//
// Assemblies are not covered here: CREATE ASSEMBLY needs a compiled binary and
// the test carries none. The assembly rows were probed by hand on the same
// three majors, with the same results as the other two classes.
func TestLiveSecurableCapabilitiesMatchWhatTheServerEnforces(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()

	const login = "gosmo_live_securablecaps"
	const pass = "P@ssw0rd_gosmo_live"
	const xsd = `N'<xsd:schema xmlns:xsd="http://www.w3.org/2001/XMLSchema"><xsd:element name="e" type="xsd:string"/></xsd:schema>'`

	d, drop := liveScratchDB(t, db, ctx, "gosmo_securablecaps_live")
	defer drop()

	db.ExecContext(ctx, "IF SUSER_ID('"+login+"') IS NOT NULL DROP LOGIN ["+login+"]")
	if _, err := db.ExecContext(ctx, "CREATE LOGIN ["+login+"] WITH PASSWORD = '"+pass+"', CHECK_POLICY = OFF"); err != nil {
		t.Fatalf("create login: %v", err)
	}
	defer db.ExecContext(context.Background(), "DROP LOGIN ["+login+"]")

	liveExecIn(t, d, ctx,
		`CREATE SCHEMA s1`,
		`CREATE SCHEMA s2`,
		`CREATE TYPE s1.t_ctl FROM int`,
		`CREATE TYPE s1.t_none FROM int`,
		`CREATE TYPE s1.t_hidden FROM int`,
		// A name that only reads back if the block QUOTENAMEs each part:
		// asked about bare, "s1.t.dot" names a securable that does not exist.
		`CREATE TYPE s1.[t.dot] FROM int`,
		`CREATE XML SCHEMA COLLECTION s1.x_own AS `+xsd,
		`CREATE USER [`+login+`] FOR LOGIN [`+login+`]`,
		// db_ddladmin makes every type visible, so t_none is asked about and
		// reads 0 rather than being absent.
		`ALTER ROLE db_ddladmin ADD MEMBER [`+login+`]`,
		`GRANT ALTER ON SCHEMA::s2 TO [`+login+`]`,
		`GRANT CONTROL ON TYPE::s1.t_ctl TO [`+login+`]`,
		`GRANT CONTROL ON TYPE::s1.[t.dot] TO [`+login+`]`,
		`ALTER AUTHORIZATION ON XML SCHEMA COLLECTION::s1.x_own TO [`+login+`]`,
		`DENY CONTROL ON TYPE::s1.t_hidden TO [`+login+`]`,
	)

	pool, err := sql.Open("sqlserver", liveRestrictedDSN(t, login, pass))
	if err != nil {
		t.Fatalf("open as %s: %v", login, err)
	}
	defer pool.Close()
	// Not NewServer, for live_schemacaps_test.go's reason: loadInfo reads
	// server-scope DMVs this login has no rights to.
	caps, err := (&Server{db: pool}).Database(d.Name()).CapabilitiesContext(ctx)
	if err != nil {
		t.Fatalf("CapabilitiesContext as %s: %v", login, err)
	}

	for _, tc := range []struct {
		kind         DatabaseSecurableKind
		schema, name string
		want         CapabilityState
		why          string
	}{
		{DatabaseSecurableType, "s1", "t_ctl", CapabilityGranted, "GRANT CONTROL on the type"},
		{DatabaseSecurableType, "s1", "t.dot", CapabilityGranted, "GRANT CONTROL on a type whose name needs quoting"},
		{DatabaseSecurableXmlSchemaCollection, "s1", "x_own", CapabilityGranted, "ownership of the collection"},
		{DatabaseSecurableType, "s1", "t_none", CapabilityDenied, "db_ddladmin alone"},
		{DatabaseSecurableType, "s1", "t_hidden", CapabilityUnknown, "DENY CONTROL, which hides the type"},
	} {
		if got := caps.SecurablePermission(tc.kind, tc.schema, tc.name, "CONTROL"); got != tc.want {
			t.Errorf("%s %s.%s CONTROL = %v, want %v (%s)", tc.kind, tc.schema, tc.name, got, tc.want, tc.why)
		}
	}

	// The oracle: what the server actually accepts from this login.
	exec := func(stmt string) error {
		_, err := pool.ExecContext(ctx, "USE ["+d.Name()+"]; "+stmt)
		return err
	}
	if err := exec("ALTER SCHEMA s2 TRANSFER TYPE::s1.t_ctl"); err != nil {
		t.Errorf("the server refused a transfer CONTROL read 1 for: %v", err)
	}
	if err := exec("ALTER SCHEMA s2 TRANSFER XML SCHEMA COLLECTION::s1.x_own"); err != nil {
		t.Errorf("the server refused the owner's transfer: %v", err)
	}
	if err := exec("ALTER SCHEMA s2 TRANSFER TYPE::s1.t_none"); err == nil {
		t.Error("db_ddladmin transferred a type CONTROL read 0 for — the Move to Schema gate would be withholding a move the server allows")
	}
	// And the drop, which the wider right *does* permit: why the block is only
	// an additional reason to permit a drop, never the whole test.
	if err := exec("DROP TYPE s1.t_none"); err != nil {
		t.Errorf("db_ddladmin was refused a type's drop, which it permitted on majors 13, 14 and 17: %v", err)
	}
	if err := exec("DROP TYPE s1.t_hidden"); err == nil {
		t.Error("the server dropped a type carrying DENY CONTROL")
	}
}
