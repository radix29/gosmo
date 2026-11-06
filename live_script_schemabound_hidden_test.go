//go:build livedb

// Live verification that a table's DROP script says when the login could not
// read what is schema-bound to it, rather than leaving the Msg 3729 note out
// (gossms review plan 2026-11-05 item F2). sys.sql_expression_dependencies is
// all-or-nothing on VIEW DEFINITION on the database: a db_datareader login
// reads it and gets no rows, so without the probe its DROP script looked
// complete.
//
//	go test -tags livedb . -run TestLiveTableDropSaysWhenDependentsAreHidden -v \
//	  -livedb 'sqlserver://sa:PASS@host?TrustServerCertificate=true'
//
// Creates and drops one throwaway database and one login; touches nothing
// else.
package gosmo

import (
	"database/sql"
	"net/url"
	"strings"
	"testing"
)

func TestLiveTableDropSaysWhenDependentsAreHidden(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()

	d, drop := liveScratchDB(t, db, ctx, "gosmo_sb_hidden_live")
	defer drop()
	liveExecIn(t, d, ctx,
		"CREATE TABLE dbo.T (id int NOT NULL PRIMARY KEY, v int NULL)",
		"CREATE VIEW dbo.V WITH SCHEMABINDING AS SELECT id, v FROM dbo.T",
		"CREATE TABLE dbo.Free (id int NOT NULL PRIMARY KEY)",
	)
	const login, pass = "gosmo_sb_hidden_lp", "Sb!4hQ#9zKw2"
	if _, err := db.ExecContext(ctx, "CREATE LOGIN "+login+" WITH PASSWORD = N'"+pass+"', CHECK_POLICY = OFF"); err != nil {
		t.Fatalf("create login: %v", err)
	}
	defer db.ExecContext(ctx, "DROP LOGIN "+login)
	liveExecIn(t, d, ctx, "CREATE USER "+login+" FOR LOGIN "+login,
		"ALTER ROLE db_datareader ADD MEMBER "+login)
	defer liveExecIn(t, d, ctx, "DROP USER "+login)

	u, err := url.Parse(*liveDSN)
	if err != nil {
		t.Fatalf("parse DSN: %v", err)
	}
	u.User = url.UserPassword(login, pass)
	ldb, err := sql.Open("sqlserver", u.String())
	if err != nil {
		t.Fatalf("open as %s: %v", login, err)
	}
	defer ldb.Close()
	ld, err := liveServer(t, ldb, ctx).DatabaseByName(ctx, d.Name)
	if err != nil {
		t.Fatalf("DatabaseByName as %s: %v", login, err)
	}
	script := func(db *Database, v ScriptVerb, table string) string {
		t.Helper()
		o := DefaultScriptOptions()
		o.Verb = v
		s, err := NewScripter(db, o).ScriptTable(ctx, "dbo", table)
		if err != nil {
			t.Fatalf("ScriptTable %s verb %v: %v", table, v, err)
		}
		return s
	}
	const hiddenNote = "-- Could not check what is schema-bound to [dbo].[T]: without VIEW DEFINITION on\n" +
		"-- the database, sys.sql_expression_dependencies lists nothing. A module\n" +
		"-- schema-bound to it makes this DROP fail with Msg 3729.\n"
	const boundNote = "-- This DROP fails with Msg 3729 while these are schema-bound to [dbo].[T]:\n" +
		"--   [dbo].[V] (VIEW)\n" +
		"-- Drop them first, or ALTER them without SCHEMABINDING.\n"

	for _, v := range []ScriptVerb{ScriptDrop, ScriptDropAndCreate} {
		asSa := script(d, v, "T")
		if !strings.HasPrefix(asSa, boundNote) {
			t.Fatalf("verb %v as sa: want the dependents named:\n%s", v, asSa)
		}
		asReader := script(ld, v, "T")
		if !strings.HasPrefix(asReader, hiddenNote) {
			t.Errorf("verb %v as %s: want the could-not-check note:\n%s", v, login, asReader)
		}
		// The note is the only difference: the script itself is unchanged.
		if got, want := strings.TrimPrefix(asReader, hiddenNote), strings.TrimPrefix(asSa, boundNote); got != want {
			t.Errorf("verb %v: the scripts differ beyond the note:\nas %s:\n%s\nas sa:\n%s", v, login, got, want)
		}
	}
	// A CREATE reads no dependents, and says nothing about them.
	if s := script(ld, ScriptCreate, "T"); strings.Contains(s, "schema-bound") {
		t.Errorf("CREATE as %s carries a schema-bound note:\n%s", login, s)
	}
	// A table nothing is bound to: the reader still cannot tell, so it is
	// still told; sa gets no note.
	if s := script(ld, ScriptDrop, "Free"); !strings.HasPrefix(s, "-- Could not check what is schema-bound to [dbo].[Free]") {
		t.Errorf("DROP of an unbound table as %s: want the could-not-check note:\n%s", login, s)
	}
	if s := script(d, ScriptDrop, "Free"); strings.Contains(s, "schema-bound") {
		t.Errorf("DROP of an unbound table as sa carries a note:\n%s", s)
	}

	// VIEW DEFINITION on the database is what lifts it: the reader then
	// gets the dependents named, as sa does.
	liveExecIn(t, d, ctx, "GRANT VIEW DEFINITION TO "+login)
	if s := script(ld, ScriptDrop, "T"); !strings.HasPrefix(s, boundNote) {
		t.Errorf("DROP with VIEW DEFINITION as %s: want the dependents named:\n%s", login, s)
	}
}
