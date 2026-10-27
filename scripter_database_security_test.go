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
		appRoles: []applicationRole{{name: "ar", defaultSchema: "sales"}},
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
