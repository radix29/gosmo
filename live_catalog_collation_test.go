//go:build livedb

// Live verification of Database.CatalogCollation: a case-sensitive database
// compares names under its own collation until it is made partially
// contained, after which its catalog — and so CatalogCollation — is
// Latin1_General_100_CI_AS_KS_WS_SC whatever the data collation says.
//
//	go test -tags livedb . -run TestLiveCatalogCollation -v \
//	  -livedb 'sqlserver://sa:PASS@host?TrustServerCertificate=true'
//
// Creates and drops its own throwaway database. Containment needs
// 'contained database authentication' = 1; it is switched on for the test
// when it is off and put back afterwards.
package gosmo

import (
	"context"
	"testing"
)

func TestLiveCatalogCollation(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()

	srv, err := NewServer(ctx, db)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	var was int
	if err := db.QueryRowContext(ctx, `SELECT CAST(value_in_use AS int) FROM sys.configurations
WHERE name = 'contained database authentication'`).Scan(&was); err != nil {
		t.Fatalf("read contained database authentication: %v", err)
	}
	// Before the scratch database, so its drop runs first: the option cannot go
	// back to 0 while a contained database exists (Msg 12818).
	if was == 0 {
		if _, err := db.ExecContext(ctx, "EXEC sp_configure 'contained database authentication', 1; RECONFIGURE"); err != nil {
			t.Fatalf("enable contained database authentication: %v", err)
		}
		defer func() {
			if _, err := db.ExecContext(context.Background(),
				"EXEC sp_configure 'contained database authentication', 0; RECONFIGURE"); err != nil {
				t.Errorf("RESTORE contained database authentication to 0 by hand: %v", err)
			}
		}()
	}
	const name = "gosmo_catalog_collation_live"
	const cs = "Latin1_General_100_CS_AS"
	_, drop := liveScratchDB(t, db, ctx, name)
	defer drop()
	if _, err := db.ExecContext(ctx, "ALTER DATABASE "+name+" COLLATE "+cs); err != nil {
		t.Fatalf("collate: %v", err)
	}

	read := func(stage string) *Database {
		t.Helper()
		d, err := srv.DatabaseByName(ctx, name)
		if err != nil {
			t.Fatalf("%s: DatabaseByName: %v", stage, err)
		}
		all, err := srv.Databases(ctx)
		if err != nil {
			t.Fatalf("%s: Databases: %v", stage, err)
		}
		for _, row := range all {
			if row.Name == name && row.CatalogCollation != d.CatalogCollation {
				t.Errorf("%s: Databases says CatalogCollation %q, DatabaseByName %q", stage, row.CatalogCollation, d.CatalogCollation)
			}
		}
		return d
	}

	if d := read("uncontained"); d.Collation != cs || d.CatalogCollation != cs {
		t.Errorf("uncontained: Collation %q, CatalogCollation %q, want both %q", d.Collation, d.CatalogCollation, cs)
	}

	if _, err := db.ExecContext(ctx, "ALTER DATABASE "+name+" SET CONTAINMENT = PARTIAL WITH ROLLBACK IMMEDIATE"); err != nil {
		t.Fatalf("set containment: %v", err)
	}
	d := read("contained")
	if d.Collation != cs || d.CatalogCollation != containedCatalogCollation {
		t.Errorf("contained: Collation %q, CatalogCollation %q, want %q and %q", d.Collation, d.CatalogCollation, cs, containedCatalogCollation)
	}

	// The server's own answer: a wrong-case name resolves in the contained
	// catalog, which is what CatalogCollation is for.
	var id any
	if err := db.QueryRowContext(ctx, "EXEC('USE "+name+"; CREATE TABLE dbo.Orders (i int); SELECT OBJECT_ID(''DBO.ORDERS'')')").Scan(&id); err != nil {
		t.Fatalf("wrong-case lookup: %v", err)
	}
	if id == nil {
		t.Errorf("DBO.ORDERS did not resolve in the contained database; CatalogCollation's premise is wrong")
	}
}
