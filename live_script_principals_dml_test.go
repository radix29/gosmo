//go:build livedb

// Live checks of three scripts that ran but recreated something different or
// could not run once filled in: a login's server role memberships, an
// orphaned user's FOR LOGIN, and the INSERT/UPDATE templates of a table with
// a column set.
//
//	go test -tags livedb . -run 'TestLiveScript(LoginServerRoles|OrphanedUser|DMLColumnSet)' -v \
//	  -livedb 'sqlserver://sa:PASS@host?TrustServerCertificate=true'
//
// Creates and drops its own throwaway logins, server role and database.
package gosmo

import (
	"regexp"
	"slices"
	"strings"
	"testing"
)

func TestLiveScriptLoginServerRoles(t *testing.T) {
	db, ctx, done := liveDB(t)
	t.Cleanup(done)
	s, err := NewServer(ctx, db)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	const login, role = "gosmo_live_rolemember", "gosmo live role]x"
	dropLoginIfPresent(t, s, login)
	s.db.Exec("IF SUSER_ID(N'" + escapeSingle(role) + "') IS NOT NULL DROP SERVER ROLE " + QuoteName(role))
	t.Cleanup(func() {
		dropLoginIfPresent(t, s, login)
		s.db.Exec("IF SUSER_ID(N'" + escapeSingle(role) + "') IS NOT NULL DROP SERVER ROLE " + QuoteName(role))
	})
	for _, q := range []string{
		"CREATE LOGIN " + QuoteName(login) + " WITH PASSWORD = N'Sc0pe!Test#2026', CHECK_POLICY = OFF",
		"ALTER LOGIN " + QuoteName(login) + " DISABLE",
		"CREATE SERVER ROLE " + QuoteName(role),
		"ALTER SERVER ROLE [dbcreator] ADD MEMBER " + QuoteName(login),
		"ALTER SERVER ROLE " + QuoteName(role) + " ADD MEMBER " + QuoteName(login),
	} {
		if _, err := s.db.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	memberships := func() []string {
		t.Helper()
		rows, err := s.db.QueryContext(ctx, `
SELECT r.name FROM sys.server_role_members rm
JOIN sys.server_principals r ON r.principal_id = rm.role_principal_id
WHERE rm.member_principal_id = SUSER_ID(@p1)`, login)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var n string
			if err := rows.Scan(&n); err != nil {
				t.Fatal(err)
			}
			out = append(out, n)
		}
		slices.Sort(out)
		return out
	}
	want := memberships()
	if len(want) != 2 {
		t.Fatalf("setup: memberships %v", want)
	}
	script, err := NewServerScripter(s, DefaultScriptOptions()).ScriptLogin(ctx, login)
	if err != nil {
		t.Fatal(err)
	}
	if i, j := strings.Index(script, "DISABLE"), strings.Index(script, "ADD MEMBER"); i < 0 || j < i {
		t.Errorf("the memberships do not follow the DISABLE:\n%s", script)
	}
	dropLoginIfPresent(t, s, login)
	runScript(t, s, strings.ReplaceAll(script, "<password, sysname, >", "Sc0pe!Test#2026"))
	if got := memberships(); !slices.Equal(got, want) {
		t.Errorf("memberships after the replay %v, want %v\n%s", got, want, script)
	}
}

func TestLiveScriptOrphanedUser(t *testing.T) {
	db, ctx, done := liveDB(t)
	t.Cleanup(done)
	d, drop := liveScratchDB(t, db, ctx, "gosmo_orphan_live")
	defer drop()
	const login = "gosmo_live_orphan"
	dropLoginIfPresent(t, d.server, login)
	t.Cleanup(func() { dropLoginIfPresent(t, d.server, login) })
	if _, err := db.ExecContext(ctx, "CREATE LOGIN "+QuoteName(login)+" WITH PASSWORD = N'Sc0pe!Test#2026', CHECK_POLICY = OFF"); err != nil {
		t.Fatal(err)
	}
	liveExecIn(t, d, ctx, "CREATE USER u_orph FOR LOGIN "+QuoteName(login))
	dropLoginIfPresent(t, d.server, login)

	u, err := d.UserByName(ctx, "u_orph")
	if err != nil {
		t.Fatal(err)
	}
	script, err := NewScripter(d, DefaultScriptOptions()).ScriptUser(ctx, "u_orph")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(script, "-- [u_orph] is orphaned: no login matches its SID "+binaryLiteral(u.SID)+".\n") ||
		!strings.Contains(script, "CREATE USER [u_orph] FOR LOGIN [u_orph]") {
		t.Errorf("orphaned user script:\n%s", script)
	}
	// The script still runs as it did: the note is a comment, the guard
	// skips the existing user.
	liveRunScript(t, d, ctx, script)
}

func TestLiveScriptDMLColumnSet(t *testing.T) {
	db, ctx, done := liveDB(t)
	t.Cleanup(done)
	d, drop := liveScratchDB(t, db, ctx, "gosmo_colset_live")
	defer drop()
	liveExecIn(t, d, ctx, `CREATE TABLE dbo.Sp (Id int IDENTITY NOT NULL, A int NULL, S1 int SPARSE NULL,
	    CS xml COLUMN_SET FOR ALL_SPARSE_COLUMNS)`)
	sc := NewScripter(d, DefaultScriptOptions())
	placeholder := regexp.MustCompile(`<[^<>]*>`)
	for name, f := range map[string]func() (string, error){
		"INSERT": func() (string, error) { return sc.ScriptInsert(ctx, "dbo", "Sp") },
		"UPDATE": func() (string, error) { return sc.ScriptUpdate(ctx, "dbo", "Sp") },
	} {
		s, err := f()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		filled := strings.ReplaceAll(s, "<Search Conditions,,>", "1 = 0")
		liveRunScript(t, d, ctx, placeholder.ReplaceAllString(filled, "NULL"))
	}
}
