package gosmo

import (
	"strings"
	"testing"
)

// TestBuildDatabaseSecurityScript pins the whole script for one database of
// each kind of principal: what the server makes itself is left out, roles
// are created owned by dbo and handed over after the users, and every
// statement is guarded or idempotent.
func TestBuildDatabaseSecurityScript(t *testing.T) {
	sec := &databaseSecurity{
		owner: "sa",
		roles: []*DatabaseRole{
			{Name: "public", ID: 0},
			{Name: "db_owner", ID: 16384, IsFixedRole: true, Members: []string{"dbo", "app"}},
			{Name: "readers", ID: 5, Owner: "dbo", Members: []string{"app", "nobody"}},
			{Name: "writers", ID: 6, Owner: "app"},
		},
		appRoles: []*ApplicationRole{{Name: "ar", DefaultSchema: "sales"}},
		users: []*User{
			{Name: "dbo", ID: 1, AuthType: "INSTANCE", LoginName: "sa"},
			{Name: "guest", ID: 2, AuthType: "NONE"},
			{Name: "sys", ID: 4, AuthType: "NONE"},
			{Name: "##MS_PolicyEventProcessingLogin##", ID: 7, AuthType: "INSTANCE", LoginName: "x"},
			{Name: "app", ID: 8, AuthType: "INSTANCE", LoginName: "app_login", DefaultSchema: "sales"},
			{Name: "cu", ID: 9, AuthType: "DATABASE"},
		},
		schemas: []*Schema{
			{Name: "dbo", ID: 1, Owner: "dbo"},
			{Name: "sales", ID: 5, Owner: "app"},
			{Name: "db_owner", ID: 16384, Owner: "db_owner"},
		},
		grants: []securityGrant{
			{grantee: "app", state: "G", permission: "CONNECT"},
			{grantee: "app", state: "W", permission: "SELECT", class: 1, schema: "dbo", name: "t"},
			{grantee: "app", state: "D", permission: "UPDATE", class: 1, schema: "dbo", name: "t", column: "c"},
			{grantee: "public", state: "R", permission: "SELECT", class: 3, name: "sales"},
			{grantee: "readers", state: "G", permission: "IMPERSONATE", class: 4, kind: "S", name: "app"},
			{grantee: "readers", state: "G", permission: "SEND", class: 17, name: "//svc"},
			{grantee: "readers", state: "G", permission: "REFERENCES", class: 6, schema: "dbo", name: "ty"},
			{grantee: "readers", state: "G", permission: "ALTER", class: 99, classDesc: "NEW_CLASS", majorID: 7},
		},
	}
	want := `/* Security of database [d]. Every password below is a placeholder: the
   server does not expose them. Set the real one on each line marked
   "set the real password" before running this. */

-- Database owner
DECLARE @db nvarchar(300) = QUOTENAME(DB_NAME());
EXEC (N'ALTER AUTHORIZATION ON DATABASE::' + @db + N' TO [sa];');
GO

-- Database roles
IF DATABASE_PRINCIPAL_ID(N'readers') IS NULL
CREATE ROLE [readers] AUTHORIZATION [dbo];
GO
IF DATABASE_PRINCIPAL_ID(N'writers') IS NULL
CREATE ROLE [writers] AUTHORIZATION [dbo];
GO

-- Application roles
IF DATABASE_PRINCIPAL_ID(N'ar') IS NULL
CREATE APPLICATION ROLE [ar] WITH PASSWORD = N'<insert password here>', DEFAULT_SCHEMA = [sales];  -- set the real password
GO

-- Database users
IF DATABASE_PRINCIPAL_ID(N'app') IS NULL
CREATE USER [app] FOR LOGIN [app_login] WITH DEFAULT_SCHEMA = [sales];
GO
IF DATABASE_PRINCIPAL_ID(N'cu') IS NULL
CREATE USER [cu] WITH PASSWORD = N'<password, sysname, >';  -- set the real password
GO

-- Schemas
IF SCHEMA_ID(N'sales') IS NULL
    EXEC(N'CREATE SCHEMA [sales] AUTHORIZATION [app]');
GO

-- Ownership
ALTER AUTHORIZATION ON SCHEMA::[sales] TO [app];
ALTER AUTHORIZATION ON ROLE::[writers] TO [app];
GO

-- Role memberships
ALTER ROLE [db_owner] ADD MEMBER [app];
ALTER ROLE [readers] ADD MEMBER [app];
GO

-- Permissions
GRANT CONNECT TO [app];
GRANT SELECT ON OBJECT::[dbo].[t] TO [app] WITH GRANT OPTION;
DENY UPDATE ON OBJECT::[dbo].[t] ([c]) TO [app];
REVOKE SELECT ON SCHEMA::[sales] FROM [public];
GRANT IMPERSONATE ON USER::[app] TO [readers];
GRANT SEND ON SERVICE::[//svc] TO [readers];
GRANT REFERENCES ON TYPE::[dbo].[ty] TO [readers];
-- not scripted: GRANT ALTER on NEW_CLASS id 7 to [readers]
GO
`
	if got := buildDatabaseSecurityScript("d", sec); got != want {
		t.Errorf("script:\n%s\nwant:\n%s", got, want)
	}
}

// TestBuildDatabaseSecurityScriptOwnerNotVisible pins the owner whose login
// this server cannot name: a comment with its SID, not an ALTER naming no one.
func TestBuildDatabaseSecurityScriptOwnerNotVisible(t *testing.T) {
	got := buildDatabaseSecurityScript("d", &databaseSecurity{ownerSID: []byte{0x01, 0xAB}})
	if !strings.Contains(got, "-- the owner's login is not visible here (SID 0x01AB)\n") || strings.Contains(got, "ALTER AUTHORIZATION ON DATABASE") {
		t.Errorf("script:\n%s", got)
	}
}

// TestSecurityGrantSecurable pins the ON clause of every class the script
// names, and the refusal of a row whose securable is gone.
func TestSecurityGrantSecurable(t *testing.T) {
	cases := []struct {
		g    securityGrant
		want string
		ok   bool
	}{
		{securityGrant{class: 0}, "", true},
		{securityGrant{class: 1}, "", false}, // its object is gone
		{securityGrant{class: 4, kind: "R", name: "r"}, "ROLE::[r]", true},
		{securityGrant{class: 4, kind: "A", name: "a"}, "APPLICATION ROLE::[a]", true},
		{securityGrant{class: 5, name: "asm"}, "ASSEMBLY::[asm]", true},
		{securityGrant{class: 10, schema: "s", name: "x"}, "XML SCHEMA COLLECTION::[s].[x]", true},
		{securityGrant{class: 15, name: "m]t"}, "MESSAGE TYPE::[m]]t]", true},
		{securityGrant{class: 16, name: "c"}, "CONTRACT::[c]", true},
		{securityGrant{class: 18, name: "b"}, "REMOTE SERVICE BINDING::[b]", true},
		{securityGrant{class: 19, name: "r"}, "ROUTE::[r]", true},
		{securityGrant{class: 23, name: "f"}, "FULLTEXT CATALOG::[f]", true},
		{securityGrant{class: 24, name: "k"}, "SYMMETRIC KEY::[k]", true},
		{securityGrant{class: 25, name: "c"}, "CERTIFICATE::[c]", true},
		{securityGrant{class: 26, name: "k"}, "ASYMMETRIC KEY::[k]", true},
		{securityGrant{class: 29, name: "s"}, "FULLTEXT STOPLIST::[s]", true},
		{securityGrant{class: 31, name: "p"}, "SEARCH PROPERTY LIST::[p]", true},
	}
	for _, c := range cases {
		if got, ok := c.g.securable(); got != c.want || ok != c.ok {
			t.Errorf("%+v: securable() = %q, %v; want %q, %v", c.g, got, ok, c.want, c.ok)
		}
	}
}

// TestDatabaseSecurityPartsLeavesOutMappedUsers: with a scope (a
// whole-database script, which creates no certificate or key) a certificate-
// or key-mapped user is a comment, and so is every statement naming it — its
// schema's ownership (the schema is created under dbo), a role's ownership,
// its membership, a grant to it or on it. Everything else is unchanged, and
// without a scope the user is scripted.
func TestDatabaseSecurityPartsLeavesOutMappedUsers(t *testing.T) {
	sec := &databaseSecurity{
		owner: "sa",
		roles: []*DatabaseRole{
			{Name: "db_owner", ID: 16384, IsFixedRole: true, Members: []string{"cu"}},
			{Name: "readers", ID: 5, Owner: "dbo", Members: []string{"app", "cu", "ku"}},
			{Name: "writers", ID: 6, Owner: "cu"},
		},
		users: []*User{
			{Name: "app", ID: 7, AuthType: "INSTANCE", LoginName: "app_login"},
			{Name: "cu", ID: 8, AuthType: "NONE", UserType: PrincipalCertificateMappedUser, MappedObject: "cert1"},
			{Name: "ku", ID: 9, AuthType: "NONE", UserType: PrincipalAsymmetricKeyMappedUser, MappedObject: "key1"},
		},
		schemas: []*Schema{{Name: "cs", ID: 5, Owner: "cu"}, {Name: "as", ID: 6, Owner: "app"}},
		grants: []securityGrant{
			{grantee: "app", state: "G", permission: "CONNECT"},
			{grantee: "app", state: "G", permission: "IMPERSONATE", class: 4, kind: "S", name: "cu"},
			{grantee: "cu", state: "G", permission: "SELECT", class: 1, schema: "dbo", name: "t"},
			{grantee: "ku", state: "D", permission: "CONNECT"},
			{grantee: "readers", state: "G", permission: "SELECT", class: 1, schema: "dbo", name: "t"},
		},
	}
	scope := &grantScope{listed: map[string]bool{"o:[dbo].[t]": true}}
	head, tail := buildDatabaseSecurityParts("d", sec, scope)
	all := head + tail
	for _, want := range []string{
		"-- not scripted: CREATE USER [cu] FROM CERTIFICATE [cert1]: CERTIFICATE::[cert1] is not scripted\n",
		"-- not scripted: CREATE USER [ku] FROM ASYMMETRIC KEY [key1]: ASYMMETRIC KEY::[key1] is not scripted\n",
		"EXEC(N'CREATE SCHEMA [cs]');",
		"-- not scripted: ALTER AUTHORIZATION ON SCHEMA::[cs] TO [cu]: USER::[cu] is not scripted\n",
		"-- not scripted: ALTER AUTHORIZATION ON ROLE::[writers] TO [cu]: USER::[cu] is not scripted\n",
		"-- not scripted: ALTER ROLE [db_owner] ADD MEMBER [cu]: USER::[cu] is not scripted\n",
		"-- not scripted: ALTER ROLE [readers] ADD MEMBER [cu]: USER::[cu] is not scripted\n",
		"-- not scripted: ALTER ROLE [readers] ADD MEMBER [ku]: USER::[ku] is not scripted\n",
		"-- not scripted: GRANT IMPERSONATE ON USER::[cu] TO [app]: USER::[cu] is not scripted\n",
		"-- not scripted: GRANT SELECT ON OBJECT::[dbo].[t] TO [cu]: USER::[cu] is not scripted\n",
		"-- not scripted: DENY CONNECT TO [ku]: USER::[ku] is not scripted\n",
		// Unchanged:
		"CREATE USER [app] FOR LOGIN [app_login];",
		"EXEC(N'CREATE SCHEMA [as] AUTHORIZATION [app]');",
		"\nALTER AUTHORIZATION ON SCHEMA::[as] TO [app];\n",
		"\nALTER ROLE [readers] ADD MEMBER [app];\n",
		"\nGRANT CONNECT TO [app];\n",
		"\nGRANT SELECT ON OBJECT::[dbo].[t] TO [readers];\n",
	} {
		if !strings.Contains(all, want) {
			t.Errorf("script lacks %q:\n%s", want, all)
		}
	}
	// Every line naming a mapped user is a comment.
	for line := range strings.SplitSeq(all, "\n") {
		if (strings.Contains(line, "[cu]") || strings.Contains(line, "[ku]")) && !strings.HasPrefix(line, "--") {
			t.Errorf("live statement names a mapped user: %q", line)
		}
	}

	all = buildDatabaseSecurityScript("d", sec)
	for _, want := range []string{
		"CREATE USER [cu] FROM CERTIFICATE [cert1];",
		"CREATE USER [ku] FROM ASYMMETRIC KEY [key1];",
		"\nALTER ROLE [readers] ADD MEMBER [cu];\n",
		"\nGRANT SELECT ON OBJECT::[dbo].[t] TO [cu];\n",
	} {
		if !strings.Contains(all, want) {
			t.Errorf("unscoped script lacks %q:\n%s", want, all)
		}
	}
	if strings.Contains(all, "not scripted") {
		t.Errorf("unscoped script left something out:\n%s", all)
	}
}
