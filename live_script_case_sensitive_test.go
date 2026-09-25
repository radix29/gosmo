//go:build livedb

// Live verification of T13 (gossms docs/review-plan-2026-09-23b.md): the
// Script* methods look their object up under the database's collation, not
// case-blind in Go. In a case-sensitive database, [Sales] and [sales] are two
// objects, and a case-blind pick scripted whichever the listing returned
// first. ALTER SCHEMA TRANSFER between two schemas that differ only in case
// is legitimate there, and still refused where the collation makes them one.
//
//	go test -tags livedb . -run TestLiveScriptLookupsHonourCollation -v \
//	  -livedb 'sqlserver://sa:PASS@host?TrustServerCertificate=true'
//
// Creates and drops its own throwaway databases; touches nothing else.
package gosmo

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
)

// liveScratchDBCollated is liveScratchDB with an explicit database
// collation.
func liveScratchDBCollated(t *testing.T, db *sql.DB, ctx context.Context, name, collation string) (*Database, func()) {
	t.Helper()
	drop := func() {
		c := context.Background()
		db.ExecContext(c, "IF DB_ID('"+name+"') IS NOT NULL ALTER DATABASE ["+name+"] SET SINGLE_USER WITH ROLLBACK IMMEDIATE")
		db.ExecContext(c, "IF DB_ID('"+name+"') IS NOT NULL DROP DATABASE ["+name+"]")
	}
	drop()
	if _, err := db.ExecContext(ctx, "CREATE DATABASE ["+name+"] COLLATE "+collation); err != nil {
		t.Fatalf("create database %s: %v", name, err)
	}
	srv, err := NewServer(ctx, db)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	d, err := srv.DatabaseByName(ctx, name)
	if err != nil {
		t.Fatalf("DatabaseByName %s: %v", name, err)
	}
	return d, drop
}

func TestLiveScriptLookupsHonourCollation(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()

	d, drop := liveScratchDBCollated(t, db, ctx, "gosmo_t13_cs", "Latin1_General_100_CS_AS")
	defer drop()

	// Each pair differs only in case, and carries a marker that shows up in
	// its script, so a script of the wrong one is visible.
	liveExecIn(t, d, ctx,
		`CREATE SCHEMA [Sales]`,
		`CREATE SCHEMA [sales]`,
		`CREATE ROLE [Clerk]`,
		`CREATE ROLE [clerk]`,
		`CREATE SEQUENCE [Sales].[seq] AS int START WITH 111`,
		`CREATE SEQUENCE [sales].[seq] AS int START WITH 222`,
		`CREATE TABLE [dbo].[TargetUpper] (id int NOT NULL, owner_name sysname NOT NULL)`,
		`CREATE TABLE [dbo].[TargetLower] (id int NOT NULL, owner_name sysname NOT NULL)`,
		`CREATE SYNONYM [dbo].[Syn] FOR [dbo].[TargetUpper]`,
		`CREATE SYNONYM [dbo].[syn] FOR [dbo].[TargetLower]`,
		`CREATE PARTITION FUNCTION [Pf] (int) AS RANGE RIGHT FOR VALUES (111)`,
		`CREATE PARTITION FUNCTION [pf] (int) AS RANGE RIGHT FOR VALUES (222)`,
		`CREATE PARTITION SCHEME [Ps] AS PARTITION [Pf] ALL TO ([PRIMARY])`,
		`CREATE PARTITION SCHEME [ps] AS PARTITION [pf] ALL TO ([PRIMARY])`,
		`CREATE FUNCTION [dbo].[pred](@owner AS sysname) RETURNS TABLE WITH SCHEMABINDING
		 AS RETURN SELECT 1 AS ok WHERE @owner = USER_NAME()`,
		`CREATE SECURITY POLICY [Sales].[pol] ADD FILTER PREDICATE [dbo].[pred](owner_name) ON [dbo].[TargetUpper] WITH (STATE = ON)`,
		`CREATE SECURITY POLICY [sales].[pol] ADD FILTER PREDICATE [dbo].[pred](owner_name) ON [dbo].[TargetLower] WITH (STATE = ON)`,
		`CREATE COLUMN MASTER KEY [Cmk] WITH (KEY_STORE_PROVIDER_NAME = 'MSSQL_CERTIFICATE_STORE', KEY_PATH = 'CurrentUser/My/111')`,
		`CREATE COLUMN MASTER KEY [cmk] WITH (KEY_STORE_PROVIDER_NAME = 'MSSQL_CERTIFICATE_STORE', KEY_PATH = 'CurrentUser/My/222')`,
		`CREATE COLUMN ENCRYPTION KEY [Cek] WITH VALUES (COLUMN_MASTER_KEY = [Cmk], ALGORITHM = 'RSA_OAEP', ENCRYPTED_VALUE = 0x0111)`,
		`CREATE COLUMN ENCRYPTION KEY [cek] WITH VALUES (COLUMN_MASTER_KEY = [cmk], ALGORITHM = 'RSA_OAEP', ENCRYPTED_VALUE = 0x0222)`,
		`CREATE TABLE [sales].[Orders] (id int NOT NULL)`,
	)

	sc := NewScripter(d, ScriptOptions{})
	cases := []struct {
		name   string
		script func(upper bool) (string, error)
		// want[0] must appear in the upper-case object's script and not the
		// lower-case one's; want[1] the other way round.
		want [2]string
	}{
		{"schema", func(u bool) (string, error) { return sc.ScriptSchema(ctx, pick(u, "Sales", "sales")) },
			[2]string{"[Sales]", "[sales]"}},
		{"database role", func(u bool) (string, error) { return sc.ScriptDatabaseRole(ctx, pick(u, "Clerk", "clerk")) },
			[2]string{"[Clerk]", "[clerk]"}},
		{"sequence", func(u bool) (string, error) { return sc.ScriptSequence(ctx, pick(u, "Sales", "sales"), "seq") },
			[2]string{"[Sales].[seq]", "[sales].[seq]"}},
		{"synonym", func(u bool) (string, error) { return sc.ScriptSynonym(ctx, "dbo", pick(u, "Syn", "syn")) },
			[2]string{"[TargetUpper]", "[TargetLower]"}},
		{"partition function", func(u bool) (string, error) { return sc.ScriptPartitionFunction(ctx, pick(u, "Pf", "pf")) },
			[2]string{"(111)", "(222)"}},
		{"partition scheme", func(u bool) (string, error) { return sc.ScriptPartitionScheme(ctx, pick(u, "Ps", "ps")) },
			[2]string{"PARTITION [Pf]", "PARTITION [pf]"}},
		{"security policy", func(u bool) (string, error) { return sc.ScriptSecurityPolicy(ctx, pick(u, "Sales", "sales"), "pol") },
			[2]string{"[TargetUpper]", "[TargetLower]"}},
		{"column master key", func(u bool) (string, error) { return sc.ScriptColumnMasterKey(ctx, pick(u, "Cmk", "cmk")) },
			[2]string{"My/111", "My/222"}},
		{"column encryption key", func(u bool) (string, error) { return sc.ScriptColumnEncryptionKey(ctx, pick(u, "Cek", "cek")) },
			[2]string{"0x0111", "0x0222"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			for i, upper := range []bool{true, false} {
				s, err := c.script(upper)
				if err != nil {
					t.Fatalf("upper=%v: %v", upper, err)
				}
				if !strings.Contains(s, c.want[i]) || strings.Contains(s, c.want[1-i]) {
					t.Errorf("upper=%v: script is of the wrong object, want %q and not %q:\n%s", upper, c.want[i], c.want[1-i], s)
				}
			}
		})
	}

	t.Run("a name the collation does not match is not found", func(t *testing.T) {
		if _, err := sc.ScriptPartitionFunction(ctx, "PF"); !errors.Is(err, ErrNotFound) {
			t.Errorf("ScriptPartitionFunction(PF) err = %v, want not-found", err)
		}
	})

	t.Run("transfer between schemas that differ only in case", func(t *testing.T) {
		if err := d.TableRef("sales", "Orders").Transfer(ctx, "Sales"); err != nil {
			t.Fatalf("Transfer sales → Sales: %v", err)
		}
		if _, err := d.TableByName(ctx, "Sales", "Orders"); err != nil {
			t.Errorf("after the transfer, [Sales].[Orders]: %v", err)
		}
	})

	t.Run("a case-insensitive database still refuses a case-only transfer", func(t *testing.T) {
		ci, dropCI := liveScratchDBCollated(t, db, ctx, "gosmo_t13_ci", "Latin1_General_100_CI_AS")
		defer dropCI()
		liveExecIn(t, ci, ctx, `CREATE SCHEMA [sales]`, `CREATE TABLE [sales].[Orders] (id int NOT NULL)`)
		err := ci.TableRef("sales", "Orders").Transfer(ctx, "SALES")
		if err == nil || !strings.Contains(err.Error(), "already in schema") {
			t.Errorf("Transfer SALES ← sales err = %v, want an already-in-schema refusal", err)
		}
	})
}

func pick(upper bool, u, l string) string {
	if upper {
		return u
	}
	return l
}
