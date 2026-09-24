//go:build livedb

// Live verification of the user kinds beyond SQL/Windows — certificate- and
// asymmetric-key-mapped, contained, login-less — through all three of the
// listing, Script as, and CreateUserRequest; and of a login script carrying
// its language and password policy.
//
// Each assertion is on the catalog after a replay, not on a script's text.
//
//	go test -tags livedb . -run 'TestLiveUserKinds|TestLiveLoginScriptKeepsPolicy' -v \
//	  -livedb 'sqlserver://sa:PASS@host?TrustServerCertificate=true'
//
// Creates and drops two throwaway databases and one throwaway login. The
// contained-user case needs 'contained database authentication' = 1; it is
// switched on for the test when it is off and put back afterwards.
package gosmo

import (
	"context"
	"slices"
	"strings"
	"testing"
)

func TestLiveUserKindsListScriptAndCreate(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()

	src, dropSrc := liveScratchDB(t, db, ctx, "gosmo_userkinds_src")
	defer dropSrc()
	dst, dropDst := liveScratchDB(t, db, ctx, "gosmo_userkinds_dst")
	defer dropDst()

	for _, d := range []*Database{src, dst} {
		liveExecIn(t, d, ctx,
			"CREATE MASTER KEY ENCRYPTION BY PASSWORD = 'Str0ng!Passw0rd#1'",
			"CREATE CERTIFICATE [c 1] WITH SUBJECT = 'c1'",
			"CREATE ASYMMETRIC KEY [ak 1] WITH ALGORITHM = RSA_2048",
		)
	}
	liveExecIn(t, src, ctx,
		"CREATE USER certUser FROM CERTIFICATE [c 1]",
		"CREATE USER akUser FROM ASYMMETRIC KEY [ak 1]",
		"CREATE USER [x WITH y] WITHOUT LOGIN WITH DEFAULT_SCHEMA = dbo",
	)

	t.Run("listed", func(t *testing.T) {
		users, err := src.Users(ctx)
		if err != nil {
			t.Fatalf("Users: %v", err)
		}
		got := map[string]string{}
		for _, u := range users {
			got[u.Name] = u.UserType
		}
		for name, typ := range map[string]string{
			"certUser": "CERTIFICATE_MAPPED_USER",
			"akUser":   "ASYMMETRIC_KEY_MAPPED_USER",
		} {
			if got[name] != typ {
				t.Errorf("Users()[%s] type = %q, want %q (listed: %v)", name, got[name], typ, got)
			}
		}
		u, err := src.UserByName(ctx, "certUser")
		if err != nil {
			t.Fatalf("UserByName certUser: %v", err)
		}
		if u.MappedObject != "c 1" {
			t.Errorf("certUser MappedObject = %q, want %q", u.MappedObject, "c 1")
		}
	})

	const principals = `SELECT name, type_desc, authentication_type_desc, ISNULL(default_schema_name, '')
FROM sys.database_principals WHERE name IN (N'certUser', N'akUser', N'x WITH y') ORDER BY name`

	t.Run("scripts replay", func(t *testing.T) {
		sc := NewScripter(src, DefaultScriptOptions())
		for _, name := range []string{"certUser", "akUser", "x WITH y"} {
			s, err := sc.ScriptUser(ctx, name)
			if err != nil {
				t.Fatalf("ScriptUser %s: %v", name, err)
			}
			liveRunScript(t, dst, ctx, s)
		}
		if a, b := liveRowsAsStrings(t, src, ctx, principals), liveRowsAsStrings(t, dst, ctx, principals); !slices.Equal(a, b) {
			t.Errorf("users differ after replay:\nsource: %v\nreplay: %v", a, b)
		}
	})

	t.Run("CreateUser", func(t *testing.T) {
		const login = "gosmo_userkinds_login"
		dropLoginIfPresent(t, src.server, login)
		defer dropLoginIfPresent(t, src.server, login)
		if _, err := src.server.CreateLogin(ctx, CreateLoginRequest{Name: login, Password: "Sc0pe!Test#2026"}); err != nil {
			t.Fatalf("create login: %v", err)
		}

		fresh, dropFresh := liveScratchDB(t, db, ctx, "gosmo_userkinds_new")
		defer dropFresh()
		liveExecIn(t, fresh, ctx,
			"CREATE MASTER KEY ENCRYPTION BY PASSWORD = 'Str0ng!Passw0rd#1'",
			"CREATE CERTIFICATE [c 1] WITH SUBJECT = 'c1'",
			"CREATE ASYMMETRIC KEY [ak 1] WITH ALGORITHM = RSA_2048",
		)
		for _, req := range []CreateUserRequest{
			{Name: "certUser", Kind: UserFromCertificate, Certificate: "c 1"},
			{Name: "akUser", Kind: UserFromAsymmetricKey, AsymmetricKey: "ak 1"},
			{Name: "x WITH y", Kind: UserWithoutLogin, DefaultSchema: "dbo"},
			{Name: "forLogin", Login: login, DefaultSchema: "dbo"},
		} {
			if _, err := fresh.CreateUser(ctx, req); err != nil {
				t.Fatalf("CreateUser %+v: %v", req, err)
			}
		}
		if a, b := liveRowsAsStrings(t, src, ctx, principals), liveRowsAsStrings(t, fresh, ctx, principals); !slices.Equal(a, b) {
			t.Errorf("CreateUser made different users:\nwant: %v\ngot:  %v", a, b)
		}
		u, err := fresh.UserByName(ctx, "forLogin")
		if err != nil || u.LoginName != login {
			t.Errorf("forLogin: LoginName = %v, %v; want %q", u, err, login)
		}

		// A Windows user for an existing Windows login, when the instance
		// has one to borrow.
		var win string
		if err := db.QueryRowContext(ctx, `SELECT TOP 1 name FROM sys.server_principals
WHERE type = 'U' AND name NOT LIKE N'NT %' ORDER BY name`).Scan(&win); err == nil {
			if _, err := fresh.CreateUser(ctx, CreateUserRequest{Name: win, Kind: UserWindows, Login: win}); err != nil {
				t.Errorf("CreateUser Windows %s: %v", win, err)
			} else if u, err := fresh.UserByName(ctx, win); err != nil || u.UserType != "WINDOWS_USER" {
				t.Errorf("Windows user read back as %v, %v", u, err)
			}
		} else {
			t.Logf("no Windows login to borrow, Windows kind not run: %v", err)
		}

		// Refused in a database that is not contained, before anything is
		// sent — the server's own refusal names neither.
		_, err = fresh.CreateUser(ctx, CreateUserRequest{Name: "cu", Kind: UserWithPassword, Password: "Sc0pe!Test#2026"})
		if err == nil || !strings.Contains(err.Error(), "CONTAINMENT = NONE") {
			t.Errorf("contained user in a non-contained database: err = %v, want the containment refusal", err)
		}
	})

	t.Run("contained", func(t *testing.T) {
		var was int
		if err := db.QueryRowContext(ctx, `SELECT CAST(value_in_use AS int) FROM sys.configurations
WHERE name = 'contained database authentication'`).Scan(&was); err != nil {
			t.Fatalf("read contained database authentication: %v", err)
		}
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
		cdb, dropC := liveScratchDB(t, db, ctx, "gosmo_userkinds_contained")
		defer dropC()
		if _, err := db.ExecContext(ctx, "ALTER DATABASE gosmo_userkinds_contained SET CONTAINMENT = PARTIAL WITH ROLLBACK IMMEDIATE"); err != nil {
			t.Fatalf("set containment: %v", err)
		}
		if _, err := cdb.CreateUser(ctx, CreateUserRequest{Name: "cu", Kind: UserWithPassword,
			Password: "Sc0pe!'Test#2026", DefaultSchema: "dbo"}); err != nil {
			t.Fatalf("CreateUser contained: %v", err)
		}
		u, err := cdb.UserByName(ctx, "cu")
		if err != nil || u.AuthType != "DATABASE" {
			t.Fatalf("contained user read back as %+v, %v", u, err)
		}

		// Its script replays once the password placeholder is filled in.
		s, err := NewScripter(cdb, DefaultScriptOptions()).ScriptUser(ctx, "cu")
		if err != nil {
			t.Fatalf("ScriptUser cu: %v", err)
		}
		liveExecIn(t, cdb, ctx, "DROP USER cu")
		liveRunScript(t, cdb, ctx, strings.ReplaceAll(s, "N'<password, sysname, >'", "N'Sc0pe!Test#2026'"))
		if u, err := cdb.UserByName(ctx, "cu"); err != nil || u.AuthType != "DATABASE" || u.DefaultSchema != "dbo" {
			t.Errorf("replayed contained user is %+v, %v", u, err)
		}
	})
}

func TestLiveLoginScriptKeepsPolicyAndLanguage(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()
	s, err := NewServer(ctx, db)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	const name = "gosmo_live_policy_off"
	dropLoginIfPresent(t, s, name)
	defer dropLoginIfPresent(t, s, name)
	// A password that fails the Windows policy: the replay only works if the
	// script really carries CHECK_POLICY = OFF.
	if _, err := db.ExecContext(ctx, "CREATE LOGIN "+name+
		" WITH PASSWORD = N'a', CHECK_POLICY = OFF, CHECK_EXPIRATION = OFF, DEFAULT_LANGUAGE = Deutsch"); err != nil {
		t.Fatalf("create login: %v", err)
	}
	const q = `SELECT sp.default_language_name, sl.is_policy_checked, sl.is_expiration_checked
FROM sys.server_principals sp JOIN sys.sql_logins sl ON sl.principal_id = sp.principal_id WHERE sp.name = @p1`
	read := func() string {
		var lang string
		var pol, exp bool
		if err := db.QueryRowContext(ctx, q, name).Scan(&lang, &pol, &exp); err != nil {
			t.Fatalf("read login: %v", err)
		}
		return strings.Join([]string{lang, boolStr(pol), boolStr(exp)}, "/")
	}
	before := read()

	script, err := NewServerScripter(s, DefaultScriptOptions()).ScriptLogin(ctx, name)
	if err != nil {
		t.Fatalf("ScriptLogin: %v", err)
	}
	if _, err := db.ExecContext(ctx, "DROP LOGIN "+name); err != nil {
		t.Fatalf("drop before replay: %v", err)
	}
	runScript(t, s, strings.ReplaceAll(script, "N'<password, sysname, >'", "N'a'"))
	if after := read(); after != before {
		t.Errorf("login after replay = %s, want %s\n%s", after, before, script)
	}

	// A Windows login's language lives in sys.server_principals only.
	var win string
	if err := db.QueryRowContext(ctx, `SELECT TOP 1 name FROM sys.server_principals WHERE type = 'U' ORDER BY name`).Scan(&win); err != nil {
		t.Skipf("no Windows login: %v", err)
	}
	l, err := s.LoginByName(ctx, win)
	if err != nil {
		t.Fatalf("LoginByName %s: %v", win, err)
	}
	det, err := l.Details(ctx)
	if err != nil {
		t.Fatalf("Details %s: %v", win, err)
	}
	if det.DefaultLanguage == "" || det.DefaultLanguage != l.DefaultLanguage {
		t.Errorf("Windows login %s: Details language %q, Login language %q — want both set and equal",
			win, det.DefaultLanguage, l.DefaultLanguage)
	}
}

func boolStr(b bool) string {
	if b {
		return "1"
	}
	return "0"
}
