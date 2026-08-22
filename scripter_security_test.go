package gosmo

import (
	"strings"
	"testing"
)

func TestBuildUserScriptPicksTheFormFromAuthType(t *testing.T) {
	cases := []struct {
		name string
		user *User
		want string
	}{
		{"login-backed", &User{Name: "u", AuthType: "INSTANCE", LoginName: "sqluser", DefaultSchema: "dbo"},
			"CREATE USER [u] FOR LOGIN [sqluser] WITH DEFAULT_SCHEMA = [dbo];"},
		// Orphaned: AuthType still says the user was made for a login, but the
		// login is gone. Scripting it as a contained or login-less user would
		// create a different kind of principal.
		{"orphaned", &User{Name: "u", AuthType: "INSTANCE", DefaultSchema: "dbo"},
			"CREATE USER [u] FOR LOGIN [u] WITH DEFAULT_SCHEMA = [dbo];"},
		{"contained", &User{Name: "u", AuthType: "DATABASE", DefaultSchema: "dbo"},
			"CREATE USER [u] WITH PASSWORD = N'<password, sysname, >', DEFAULT_SCHEMA = [dbo];"},
		{"without login", &User{Name: "u", AuthType: "NONE", DefaultSchema: "dbo"},
			"CREATE USER [u] WITHOUT LOGIN WITH DEFAULT_SCHEMA = [dbo];"},
		{"external", &User{Name: "u@contoso.com", AuthType: "EXTERNAL"},
			"CREATE USER [u@contoso.com] FROM EXTERNAL PROVIDER;"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := buildUserScript(c.user, DefaultScriptOptions())
			if !strings.Contains(got, c.want) {
				t.Errorf("user script:\n%s\nwant it to contain:\n%s", got, c.want)
			}
		})
	}
}

func TestBuildDatabaseRoleScriptRestoresMembership(t *testing.T) {
	r := &DatabaseRole{Name: "app_rw", Owner: "dbo", Members: []string{"alice", "bob"}}
	got := buildDatabaseRoleScript(r, DefaultScriptOptions())

	if !strings.Contains(got, "CREATE ROLE [app_rw] AUTHORIZATION [dbo];") {
		t.Errorf("role script wrong:\n%s", got)
	}
	for _, m := range r.Members {
		if !strings.Contains(got, "ALTER ROLE [app_rw] ADD MEMBER ["+m+"];") {
			t.Errorf("member %q not restored by the script:\n%s", m, got)
		}
	}
}

func TestBuildSchemaScriptGuardsWithDynamicExec(t *testing.T) {
	// CREATE SCHEMA must be the first statement in its batch, so it cannot sit
	// inside an IF — the guard has to run it through EXEC instead.
	got := buildSchemaScript(&Schema{Name: "sales", Owner: "dbo"}, DefaultScriptOptions())
	if !strings.Contains(got, "IF SCHEMA_ID(N'sales') IS NULL") || !strings.Contains(got, "EXEC(N'CREATE SCHEMA [sales] AUTHORIZATION [dbo]');") {
		t.Errorf("guarded schema script wrong:\n%s", got)
	}

	opts := DefaultScriptOptions()
	opts.IncludeIfNotExists = false
	if got := buildSchemaScript(&Schema{Name: "sales"}, opts); !strings.Contains(got, "CREATE SCHEMA [sales];") || strings.Contains(got, "EXEC") {
		t.Errorf("unguarded schema script should be the bare statement:\n%s", got)
	}
}

func TestBuildLoginScript(t *testing.T) {
	sql := &Login{Name: "app", LoginType: "SQL_LOGIN", DefaultDatabase: "master", IsDisabled: true}
	got := buildLoginScript(sql, DefaultScriptOptions())
	if !strings.Contains(got, "CREATE LOGIN [app] WITH PASSWORD = N'<password, sysname, >', DEFAULT_DATABASE = [master];") {
		t.Errorf("SQL login script wrong:\n%s", got)
	}
	if !strings.Contains(got, "ALTER LOGIN [app] DISABLE;") {
		t.Errorf("a disabled login must be scripted disabled:\n%s", got)
	}

	win := &Login{Name: `CONTOSO\svc`, LoginType: "WINDOWS_LOGIN", DefaultDatabase: "master"}
	if got := buildLoginScript(win, DefaultScriptOptions()); !strings.Contains(got, `CREATE LOGIN [CONTOSO\svc] FROM WINDOWS WITH DEFAULT_DATABASE = [master];`) {
		t.Errorf("Windows login script wrong:\n%s", got)
	}

	// DROP LOGIN has no IF EXISTS form; the guard is what makes the drop
	// re-runnable.
	opts := DefaultScriptOptions()
	opts.Verb = ScriptDrop
	got = buildLoginScript(sql, opts)
	if strings.Contains(got, "DROP LOGIN IF EXISTS") {
		t.Errorf("DROP LOGIN IF EXISTS is not valid T-SQL:\n%s", got)
	}
	if !strings.Contains(got, "IF SUSER_ID(N'app') IS NOT NULL") || !strings.Contains(got, "DROP LOGIN [app];") {
		t.Errorf("guarded login drop wrong:\n%s", got)
	}
}

func TestBuildServerRoleScriptRestoresMembership(t *testing.T) {
	got := buildServerRoleScript(&ServerRole{Name: "ops", Owner: "sa", Members: []string{"app"}}, DefaultScriptOptions())
	if !strings.Contains(got, "CREATE SERVER ROLE [ops] AUTHORIZATION [sa];") {
		t.Errorf("server role script wrong:\n%s", got)
	}
	if !strings.Contains(got, "ALTER SERVER ROLE [ops] ADD MEMBER [app];") {
		t.Errorf("server role membership not restored:\n%s", got)
	}
}

func TestBuildLoginScriptCarriesTheSID(t *testing.T) {
	// A login recreated with a fresh SID orphans every database user mapped
	// to it, which is the failure scripting a login exists to avoid.
	sid := []byte{0x01, 0x06, 0x00, 0xAB}
	sql := &Login{Name: "app", LoginType: "SQL_LOGIN", SID: sid}
	got := buildLoginScript(sql, DefaultScriptOptions())
	if !strings.Contains(got, "SID = 0x010600AB") {
		t.Errorf("a SQL login's SID must be scripted:\n%s", got)
	}
	if !strings.Contains(got, "CREATE LOGIN [app] WITH PASSWORD = N'<password, sysname, >', SID = 0x010600AB;") {
		t.Errorf("SID must continue the WITH list, not open a second one:\n%s", got)
	}

	// SID is a SQL-login clause. A Windows or external login's SID comes from
	// the directory and naming one does not parse, so it must not be emitted
	// even though the field is populated for both.
	for _, typ := range []string{"WINDOWS_LOGIN", "WINDOWS_GROUP", "EXTERNAL_LOGIN", "EXTERNAL_GROUP"} {
		got := buildLoginScript(&Login{Name: "svc", LoginType: typ, SID: sid}, DefaultScriptOptions())
		if strings.Contains(got, "SID =") {
			t.Errorf("%s must not carry a SID clause:\n%s", typ, got)
		}
	}

	// A login read from a lightweight handle has no SID; the clause is then
	// absent rather than an empty 0x.
	if got := buildLoginScript(&Login{Name: "app", LoginType: "SQL_LOGIN"}, DefaultScriptOptions()); strings.Contains(got, "SID") {
		t.Errorf("a login with no SID must not get a SID clause:\n%s", got)
	}
}

func TestBuildLoginScriptOpensWithFromTheBranchNotTheText(t *testing.T) {
	// " WITH " is legal inside a bracketed identifier. Deciding whether the
	// WITH keyword has been emitted by searching the statement built so far
	// found the one in the name and continued a list that was never opened,
	// producing "FROM WINDOWS, DEFAULT_DATABASE = [master]".
	win := &Login{Name: `svc WITH rights`, LoginType: "WINDOWS_LOGIN", DefaultDatabase: "master"}
	got := buildLoginScript(win, DefaultScriptOptions())
	if !strings.Contains(got, "CREATE LOGIN [svc WITH rights] FROM WINDOWS WITH DEFAULT_DATABASE = [master];") {
		t.Errorf("a login name containing WITH must not change the clause list:\n%s", got)
	}
	if strings.Contains(got, "FROM WINDOWS,") {
		t.Errorf("DEFAULT_DATABASE continued a WITH list that was never opened:\n%s", got)
	}

	// The same trap in the other direction: a SQL login has opened a WITH,
	// so its DEFAULT_DATABASE must continue it with a comma.
	sqlLogin := &Login{Name: `app WITH rights`, LoginType: "SQL_LOGIN", DefaultDatabase: "master"}
	got = buildLoginScript(sqlLogin, DefaultScriptOptions())
	if !strings.Contains(got, "N'<password, sysname, >', DEFAULT_DATABASE = [master];") {
		t.Errorf("a SQL login's DEFAULT_DATABASE must continue the open WITH:\n%s", got)
	}
	if strings.Contains(got, "WITH DEFAULT_DATABASE") {
		t.Errorf("a second WITH keyword is a syntax error:\n%s", got)
	}
}

// A certificate- or asymmetric-key-mapped login used to fall through to the
// password branch, which scripted it as a SQL login — the wrong kind of login
// entirely, and one carrying a SID clause that does not parse for it.
func TestBuildLoginScriptMappedLogins(t *testing.T) {
	sid := []byte{0x01, 0x06, 0x00, 0xAB}
	for _, tc := range []struct {
		typ  string
		want string
	}{
		{"CERTIFICATE_MAPPED_LOGIN", "CREATE LOGIN [signer] FROM CERTIFICATE [sig_cert];"},
		{"ASYMMETRIC_KEY_MAPPED_LOGIN", "CREATE LOGIN [signer] FROM ASYMMETRIC KEY [sig_cert];"},
	} {
		l := &Login{Name: "signer", LoginType: tc.typ, SID: sid, MappedObject: "sig_cert"}
		got := buildLoginScript(l, DefaultScriptOptions())
		if !strings.Contains(got, tc.want) {
			t.Errorf("%s script wrong:\n%s\nwant it to contain:\n%s", tc.typ, got, tc.want)
		}
		if strings.Contains(got, "PASSWORD") {
			t.Errorf("%s was scripted as a SQL login:\n%s", tc.typ, got)
		}
		if strings.Contains(got, "SID =") {
			t.Errorf("%s must not carry a SID clause:\n%s", tc.typ, got)
		}
	}
}

// FROM EXTERNAL PROVIDER takes no WITH option list, so a DEFAULT_DATABASE
// named there is a syntax error; it goes out as an ALTER LOGIN instead.
func TestBuildLoginScriptMovesAnExternalLoginsDefaultDatabaseToAnAlter(t *testing.T) {
	for _, typ := range []string{"EXTERNAL_LOGIN", "EXTERNAL_GROUP"} {
		got := buildLoginScript(&Login{Name: "svc", LoginType: typ, DefaultDatabase: "sales"}, DefaultScriptOptions())
		create, _, _ := strings.Cut(got, ";")
		if strings.Contains(create, "DEFAULT_DATABASE") {
			t.Errorf("%s: CREATE LOGIN cannot carry DEFAULT_DATABASE:\n%s", typ, got)
		}
		if !strings.Contains(got, "ALTER LOGIN [svc] WITH DEFAULT_DATABASE = [sales];") {
			t.Errorf("%s: default database dropped instead of moved to an ALTER:\n%s", typ, got)
		}
	}

	// The two types whose CREATE does carry it must not also get an ALTER.
	for _, typ := range []string{"SQL_LOGIN", "WINDOWS_LOGIN"} {
		got := buildLoginScript(&Login{Name: "svc", LoginType: typ, DefaultDatabase: "sales"}, DefaultScriptOptions())
		if strings.Contains(got, "ALTER LOGIN [svc] WITH DEFAULT_DATABASE") {
			t.Errorf("%s: DEFAULT_DATABASE scripted twice:\n%s", typ, got)
		}
		if !strings.Contains(got, "DEFAULT_DATABASE = [sales]") {
			t.Errorf("%s: default database not scripted:\n%s", typ, got)
		}
	}
}

// sys.server_principals reports a default database for a certificate- or
// asymmetric-key-mapped login (master), but SQL Server refuses to set one:
// both CREATE and ALTER answer "Cannot use the parameter DEFAULT_DATABASE for
// a certificate or asymmetric key login" (verified live on win10cli). A
// script that carried the value back would fail on the very login it was
// taken from.
func TestBuildLoginScriptOmitsAMappedLoginsDefaultDatabase(t *testing.T) {
	for _, typ := range []string{"CERTIFICATE_MAPPED_LOGIN", "ASYMMETRIC_KEY_MAPPED_LOGIN"} {
		got := buildLoginScript(&Login{
			Name: "signer", LoginType: typ, DefaultDatabase: "master", MappedObject: "sig_cert",
		}, DefaultScriptOptions())
		if strings.Contains(got, "DEFAULT_DATABASE") {
			t.Errorf("%s must not be scripted with a default database:\n%s", typ, got)
		}
	}
}

// ResolveMapping leaves MappedObject empty when the certificate has been
// dropped or the caller never resolved it. Naming nothing would produce
// "FROM CERTIFICATE ;", so the script carries a placeholder like the one a
// SQL login's password gets.
func TestBuildLoginScriptPlaceholdersAnUnresolvedMapping(t *testing.T) {
	got := buildLoginScript(&Login{Name: "signer", LoginType: "CERTIFICATE_MAPPED_LOGIN"}, DefaultScriptOptions())
	if !strings.Contains(got, "FROM CERTIFICATE [<certificate name, sysname, >];") {
		t.Errorf("unresolved certificate mapping wrong:\n%s", got)
	}
}
