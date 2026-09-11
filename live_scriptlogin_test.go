//go:build livedb

// Live verification that buildLoginScript's output is T-SQL SQL Server
// accepts, and that the SID it carries is the SID the login comes back with.
//
// The unit tests in scripter_security_test.go pin the statement text. They
// cannot settle either half of what the SID clause exists for: that
// "WITH PASSWORD = ..., SID = 0x..., DEFAULT_DATABASE = ..." parses at all in
// that order, and that recreating a login from the script reproduces its SID
// rather than getting a fresh one — which is the whole point, since a fresh
// SID orphans every database user mapped to the login.
//
//	go test -tags livedb . -run TestLiveScriptLogin -v \
//	  -livedb 'sqlserver://sa:PASS@host?TrustServerCertificate=true'
//
// Creates and drops its own throwaway logins and database; touches nothing
// else.
package gosmo

import (
	"bytes"
	"strings"
	"testing"
)

// dropLoginIfPresent removes a throwaway login, ignoring the error from one
// that was never created. DROP LOGIN has no IF EXISTS form.
func dropLoginIfPresent(t *testing.T, s *Server, name string) {
	t.Helper()
	if _, err := s.db.Exec("IF SUSER_ID(N'" + escapeSingle(name) + "') IS NOT NULL DROP LOGIN " + quoteIdent(name)); err != nil {
		t.Logf("cleanup of login %q: %v", name, err)
	}
}

// runScript executes a generated script the way a user would: batch by batch,
// splitting on the GO lines the scripter emits, which are a client directive
// and not a statement the server accepts.
func runScript(t *testing.T, s *Server, script string) {
	t.Helper()
	for _, batch := range strings.Split(script, "\nGO\n") {
		batch = strings.TrimSpace(batch)
		if batch == "" {
			continue
		}
		if _, err := s.db.Exec(batch); err != nil {
			t.Fatalf("the generated script was rejected by the server:\n%s\n\nerror: %v", batch, err)
		}
	}
}

func TestLiveScriptLoginRoundTripsTheSID(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()
	s, err := NewServer(ctx, db)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	// A name containing " WITH ", which is the other half of this change:
	// deciding whether the WITH keyword had been emitted by searching the
	// statement text found the one in the name.
	const name = `gosmo live WITH rights`
	dropLoginIfPresent(t, s, name)
	defer dropLoginIfPresent(t, s, name)

	if err := s.CreateLoginContext(ctx, name, "Sc0pe!Test#2026", &CreateLoginOptions{DefaultDatabase: "master"}); err != nil {
		t.Fatalf("create the login to be scripted: %v", err)
	}
	before, err := s.LoginByNameContext(ctx, name)
	if err != nil {
		t.Fatalf("read the login back: %v", err)
	}
	if len(before.SID) == 0 {
		t.Fatal("the login came back with no SID; the rest of this test proves nothing")
	}

	opts := DefaultScriptOptions()
	opts.IncludeHeaders = false
	script := buildLoginScript(before, opts)
	t.Logf("generated script:\n%s", script)

	// The placeholder is a template parameter for a human, not something the
	// server will take. Substitute a real password, exactly as the operator
	// this script is written for would.
	script = strings.ReplaceAll(script, "N'<password, sysname, >'", "N'Sc0pe!Test#2026'")

	// Drop it and rebuild it from its own script — the move the SID exists
	// for, on the server it would really be done on.
	if _, err := db.ExecContext(ctx, "DROP LOGIN "+quoteIdent(name)); err != nil {
		t.Fatalf("drop before replay: %v", err)
	}
	runScript(t, s, script)

	after, err := s.LoginByNameContext(ctx, name)
	if err != nil {
		t.Fatalf("read the recreated login: %v", err)
	}
	if !bytes.Equal(before.SID, after.SID) {
		t.Errorf("SID changed across a script round trip: was %s, now %s\n"+
			"a login recreated with a fresh SID orphans every database user mapped to it",
			binaryLiteral(before.SID), binaryLiteral(after.SID))
	}
	if after.DefaultDatabase != "master" {
		t.Errorf("default database = %q after the round trip, want master "+
			"(the DEFAULT_DATABASE clause did not survive the WITH list)", after.DefaultDatabase)
	}
}

// TestLiveScriptedLoginKeepsAUserMapped is the consequence the SID clause
// exists to prevent, exercised end to end: a database user bound to a login is
// orphaned when the login comes back with a different SID, and stays bound
// when it comes back with the same one.
func TestLiveScriptedLoginKeepsAUserMapped(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()
	s, err := NewServer(ctx, db)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	const login = "gosmo_sidmap_login"
	const dbName = "gosmo_sidmap_live"
	dropLoginIfPresent(t, s, login)
	defer dropLoginIfPresent(t, s, login)

	if err := s.CreateLoginContext(ctx, login, "Sc0pe!Test#2026", nil); err != nil {
		t.Fatalf("create login: %v", err)
	}
	_, drop := liveScratchDB(t, db, ctx, dbName)
	defer drop()
	if _, err := db.ExecContext(ctx, "USE ["+dbName+"]; CREATE USER "+quoteIdent(login)+" FOR LOGIN "+quoteIdent(login)); err != nil {
		t.Fatalf("create the mapped user: %v", err)
	}

	orphaned := func() bool {
		t.Helper()
		var n int
		q := "SELECT COUNT(*) FROM [" + dbName + "].sys.database_principals dp " +
			"WHERE dp.name = @p1 AND dp.type = 'S' " +
			"AND NOT EXISTS (SELECT 1 FROM sys.server_principals sp WHERE sp.sid = dp.sid)"
		if err := db.QueryRowContext(ctx, q, login).Scan(&n); err != nil {
			t.Fatalf("orphan check: %v", err)
		}
		return n > 0
	}
	if orphaned() {
		t.Fatal("the user was orphaned before anything was dropped; the fixture is wrong")
	}

	l, err := s.LoginByNameContext(ctx, login)
	if err != nil {
		t.Fatalf("read login: %v", err)
	}
	opts := DefaultScriptOptions()
	opts.IncludeHeaders = false
	script := strings.ReplaceAll(buildLoginScript(l, opts),
		"N'<password, sysname, >'", "N'Sc0pe!Test#2026'")

	if _, err := db.ExecContext(ctx, "DROP LOGIN "+quoteIdent(login)); err != nil {
		t.Fatalf("drop login: %v", err)
	}
	if !orphaned() {
		t.Fatal("dropping the login did not orphan its user; this server does not " +
			"behave the way the SID clause assumes, and the assertion below would be vacuous")
	}

	runScript(t, s, script)
	if orphaned() {
		t.Error("the user is still orphaned after replaying the login's script; " +
			"the scripted SID did not reattach it, which is the one thing it is for")
	}
}
