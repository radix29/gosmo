//go:build livedb

// Live verification of T26: capability keys keep a dotted schema and a dotted
// object apart. [a.b].[c] and [a].[b.c] were both keyed "a.b.c", so a DENY on
// one read back as a DENY on the other — for objects, and for types through
// DatabaseSecurableKey.
//
//	go test -tags livedb . -run TestLiveDottedCapabilityKeys -v \
//	  -livedb 'sqlserver://sa:PASS@host?TrustServerCertificate=true'
//
// Creates and drops one throwaway database and one login.
package gosmo

import (
	"context"
	"database/sql"
	"testing"
)

func TestLiveDottedCapabilityKeys(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()

	const login = "gosmo_live_dottedkeys"
	const pass = "P@ssw0rd_gosmo_live"

	d, drop := liveScratchDB(t, db, ctx, "gosmo_dottedkeys_live")
	defer drop()

	db.ExecContext(ctx, "IF SUSER_ID('"+login+"') IS NOT NULL DROP LOGIN ["+login+"]")
	if _, err := db.ExecContext(ctx, "CREATE LOGIN ["+login+"] WITH PASSWORD = '"+pass+"', CHECK_POLICY = OFF"); err != nil {
		t.Fatalf("create login: %v", err)
	}
	defer db.ExecContext(context.Background(), "DROP LOGIN ["+login+"]")

	liveExecIn(t, d, ctx,
		`CREATE SCHEMA [a.b]`,
		`CREATE SCHEMA a`,
		`CREATE TABLE [a.b].c (i int)`,
		`CREATE TABLE a.[b.c] (i int)`,
		`CREATE TYPE [a.b].c FROM int`,
		`CREATE TYPE a.[b.c] FROM int`,
		`CREATE USER [`+login+`] FOR LOGIN [`+login+`]`,
		// SELECT keeps [a.b].[c] visible: a DENY alone leaves the login no
		// permission on it, and metadata visibility then hides the row.
		`GRANT SELECT ON [a.b].c TO [`+login+`]`,
		`DENY ALTER ON [a.b].c TO [`+login+`]`,
		`GRANT ALTER ON a.[b.c] TO [`+login+`]`,
		`GRANT CONTROL ON TYPE::a.[b.c] TO [`+login+`]`,
		`GRANT VIEW DEFINITION ON TYPE::[a.b].c TO [`+login+`]`,
	)

	pool, err := sql.Open("sqlserver", liveRestrictedDSN(t, login, pass))
	if err != nil {
		t.Fatalf("open as %s: %v", login, err)
	}
	defer pool.Close()
	caps, err := (&Server{db: pool}).DatabaseRef(d.Name).Capabilities(ctx)
	if err != nil {
		t.Fatalf("Capabilities as %s: %v", login, err)
	}

	if !caps.DeniedOnObject("a.b", "c", "ALTER") {
		t.Error("[a.b].[c]: the DENY ALTER did not read back")
	}
	if caps.DeniedOnObject("a", "b.c", "ALTER") || !caps.HasOnObject("a", "b.c", "ALTER") {
		t.Errorf("[a].[b.c] reads %v, want granted — it read [a.b].[c]'s DENY", caps.ObjectPermission("a", "b.c", "ALTER"))
	}
	if got := caps.SecurablePermission(DatabaseSecurableType, "a", "b.c", "CONTROL"); got != CapabilityGranted {
		t.Errorf("TYPE [a].[b.c] CONTROL = %v, want granted", got)
	}
	if got := caps.SecurablePermission(DatabaseSecurableType, "a.b", "c", "CONTROL"); got != CapabilityDenied {
		t.Errorf("TYPE [a.b].[c] CONTROL = %v, want denied — it read [a].[b.c]'s grant", got)
	}
}
