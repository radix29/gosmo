//go:build livedb

// Live round trip of Scripter.ScriptDatabaseSecurity: a throwaway database
// holding each kind of principal, ownership, membership and grant is
// scripted, the script replayed into a second throwaway database — twice,
// since every statement claims to be re-runnable — and the two catalogs
// compared.
//
//	go test -tags livedb . -run TestLiveScriptDatabaseSecurity -v \
//	  -livedb 'sqlserver://sa:PASS@host?TrustServerCertificate=true'
//
// Creates and drops two throwaway databases and one throwaway login.
package gosmo

import (
	"slices"
	"strings"
	"testing"
)

func TestLiveScriptDatabaseSecurityRoundTrips(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()

	const login = "gosmo_secscript_login"
	srv, err := NewServer(ctx, db)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	dropLoginIfPresent(t, srv, login)
	defer dropLoginIfPresent(t, srv, login)
	if _, err := srv.CreateLogin(ctx, CreateLoginRequest{Name: login, Password: "Sc0pe!Test#2026"}); err != nil {
		t.Fatalf("create login: %v", err)
	}

	src, dropSrc := liveScratchDB(t, db, ctx, "gosmo_secscript_src")
	defer dropSrc()
	dst, dropDst := liveScratchDB(t, db, ctx, "gosmo_secscript_dst")
	defer dropDst()

	liveExecIn(t, src, ctx,
		"CREATE USER [app user] FOR LOGIN "+login+" WITH DEFAULT_SCHEMA = [sales]",
		"CREATE USER nologin WITHOUT LOGIN",
		"CREATE ROLE readers",
		"CREATE ROLE [owned role] AUTHORIZATION [app user]",
		"CREATE ROLE nested",
		"ALTER ROLE readers ADD MEMBER [app user]",
		"ALTER ROLE readers ADD MEMBER nested",
		"ALTER ROLE db_datareader ADD MEMBER nologin",
		"CREATE APPLICATION ROLE ar WITH PASSWORD = N'Sc0pe!Test#2026', DEFAULT_SCHEMA = dbo",
		"CREATE SCHEMA sales AUTHORIZATION nologin",
		"CREATE TABLE sales.t (id int, secret int)",
		"GRANT SELECT ON sales.t TO readers WITH GRANT OPTION",
		"DENY SELECT ON sales.t (secret) TO [app user]",
		"GRANT EXECUTE ON SCHEMA::sales TO nested",
		"GRANT CREATE TABLE TO [app user]",
		"GRANT IMPERSONATE ON USER::nologin TO [app user]",
		"GRANT SELECT ON SCHEMA::dbo TO ar",
		"CREATE CERTIFICATE c1 ENCRYPTION BY PASSWORD = 'Sc0pe!Test#2026' WITH SUBJECT = 'c1'",
		"GRANT CONTROL ON CERTIFICATE::c1 TO readers",
	)
	// The table and certificate a grant names are objects, not security: the
	// replay target needs them, as it would after a schema-only copy.
	liveExecIn(t, dst, ctx,
		"EXEC (N'CREATE SCHEMA sales')",
		"CREATE TABLE sales.t (id int, secret int)",
		"CREATE CERTIFICATE c1 ENCRYPTION BY PASSWORD = 'Sc0pe!Test#2026' WITH SUBJECT = 'c1'",
	)

	script, err := NewScripter(src, DefaultScriptOptions()).ScriptDatabaseSecurity(ctx)
	if err != nil {
		t.Fatalf("ScriptDatabaseSecurity: %v", err)
	}
	if strings.Contains(script, "not scripted") {
		t.Errorf("a permission was left unscripted:\n%s", script)
	}
	t.Logf("script:\n%s", script)
	replay := strings.ReplaceAll(script, "N'"+PasswordPlaceholder+"'", "N'Sc0pe!Test#2026'")
	liveRunScript(t, dst, ctx, replay)
	liveRunScript(t, dst, ctx, replay)

	for what, q := range map[string]string{
		"principals": `SELECT dp.name, dp.type, dp.authentication_type, ISNULL(dp.default_schema_name, N''),
       o.name AS owner, SUSER_SNAME(dp.sid) AS login
FROM sys.database_principals dp
LEFT JOIN sys.database_principals o ON o.principal_id = dp.owning_principal_id
WHERE dp.principal_id > 4 AND dp.is_fixed_role = 0 AND dp.name NOT LIKE N'##%'
ORDER BY dp.name`,
		"schemas": `SELECT s.name, p.name FROM sys.schemas s JOIN sys.database_principals p ON p.principal_id = s.principal_id
WHERE s.schema_id > 4 AND s.schema_id < 16384 ORDER BY s.name`,
		"memberships": `SELECT r.name, m.name FROM sys.database_role_members rm
JOIN sys.database_principals r ON r.principal_id = rm.role_principal_id
JOIN sys.database_principals m ON m.principal_id = rm.member_principal_id
WHERE m.principal_id > 4 ORDER BY r.name, m.name`,
		"permissions": `SELECT g.name, p.class, p.state, p.permission_name,
       CASE p.class WHEN 1 THEN OBJECT_SCHEMA_NAME(p.major_id) + N'.' + OBJECT_NAME(p.major_id)
                    WHEN 3 THEN SCHEMA_NAME(p.major_id)
                    WHEN 4 THEN USER_NAME(p.major_id)
                    WHEN 25 THEN (SELECT name FROM sys.certificates WHERE certificate_id = p.major_id) END AS securable,
       ISNULL(COL_NAME(p.major_id, p.minor_id), N'') AS col
FROM sys.database_permissions p JOIN sys.database_principals g ON g.principal_id = p.grantee_principal_id
WHERE p.grantee_principal_id > 4 AND g.name NOT LIKE N'##%'
ORDER BY 1, 2, 5, 6, 4`,
	} {
		if a, b := liveRowsAsStrings(t, src, ctx, q), liveRowsAsStrings(t, dst, ctx, q); !slices.Equal(a, b) {
			t.Errorf("%s differ after replay:\nsource: %v\nreplay: %v\n\n%s", what, a, b, script)
		}
	}
}
