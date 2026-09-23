//go:build livedb

// Live verification that SetDatabaseOption's CONTAINMENT statement parses.
// The grammar is "SET CONTAINMENT = PARTIAL"; the bare-keyword form every
// other option uses is "Incorrect syntax near 'PARTIAL'" (Msg 102). Setting
// PARTIAL also needs the server's "contained database authentication"
// option — where that is off the server refuses with Msg 12824 instead,
// which still proves the statement parsed. The server option is read, never
// changed.
//
//	go test -tags livedb . -run TestLiveSetDatabaseOptionContainment -v \
//	  -livedb 'sqlserver://sa:PASS@host?TrustServerCertificate=true'
//
// Creates and drops its own throwaway database; touches nothing else.
package gosmo

import (
	"database/sql"
	"errors"
	"slices"
	"testing"

	mssql "github.com/microsoft/go-mssqldb"
)

func TestLiveSetDatabaseOptionContainment(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()

	d, drop := liveScratchDB(t, db, ctx, "gosmo_containment_live")
	defer drop()

	var enabled int
	if err := db.QueryRowContext(ctx,
		`SELECT CONVERT(INT, value_in_use) FROM sys.configurations WHERE name = 'contained database authentication'`,
	).Scan(&enabled); err != nil {
		t.Fatalf("read contained database authentication: %v", err)
	}

	err := d.SetDatabaseOption(ctx, DBOptContainment, "PARTIAL", TerminationNone)
	if enabled == 0 {
		// 12824 comes first; the error's own Number is the trailing Msg 5069,
		// "ALTER DATABASE statement failed".
		msErr, ok := errors.AsType[mssql.Error](err)
		if !ok || !slices.ContainsFunc(msErr.All, func(e mssql.Error) bool { return e.Number == 12824 }) {
			t.Fatalf("SetDatabaseOption(CONTAINMENT, PARTIAL) with contained auth off = %v, want Msg 12824 (not a syntax error)", err)
		}
		return
	}
	if err != nil {
		t.Fatalf("SetDatabaseOption(CONTAINMENT, PARTIAL): %v", err)
	}
	containment := func() int {
		var c int
		if err := db.QueryRowContext(ctx, `SELECT containment FROM sys.databases WHERE name = @p1`,
			sql.Named("p1", d.Name)).Scan(&c); err != nil {
			t.Fatalf("read containment: %v", err)
		}
		return c
	}
	if got := containment(); got != 1 {
		t.Errorf("containment after PARTIAL = %d, want 1", got)
	}
	if err := d.SetDatabaseOption(ctx, DBOptContainment, "NONE", TerminationNone); err != nil {
		t.Fatalf("SetDatabaseOption(CONTAINMENT, NONE): %v", err)
	}
	if got := containment(); got != 0 {
		t.Errorf("containment after NONE = %d, want 0", got)
	}
}
