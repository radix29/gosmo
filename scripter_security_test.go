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
