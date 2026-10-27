package gosmo

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/radix29/gosmo/tsql"
)

// ============================================================
// Scripter — a database's whole security
// ============================================================

// ScriptDatabaseSecurity generates the T-SQL that recreates the database's
// security: its owner, roles, application roles, users, schemas and their
// owners, role memberships and explicit permissions — SSMS's "Script
// Database Security" shape, as one script.
//
// The script is the CREATE form whatever the options' Verb, and every
// statement is guarded or idempotent (IF DATABASE_PRINCIPAL_ID(…) IS NULL,
// IF SCHEMA_ID(…) IS NULL, ALTER AUTHORIZATION, ALTER ROLE … ADD MEMBER), so
// it can be replayed into a database that already holds part of it — the
// usual case after a restore onto another server. It names no database: it
// applies to the one it runs in, the owner's ALTER AUTHORIZATION included
// (DB_NAME()). DatabaseContext prefixes it for a reader.
//
// It leaves out what the server makes itself: public, dbo, guest,
// INFORMATION_SCHEMA and sys, the fixed roles, the ##MS_…## principals, the
// system schemas, and grants on system objects — though a grant to public or
// guest is kept, being a deliberate opening. Extended properties are not
// scripted.
//
// The order is the one replay needs. Roles are created owned by dbo —
// without AUTHORIZATION the owner would be whoever runs the script — and
// handed to their owner only once every principal exists; schemas likewise,
// after the users. Users are buildUserScript's forms (ScriptUser's), so an
// orphaned user carries its note. Contained users and application roles get
// a password placeholder that fails Windows complexity (no upper case, no
// digit), each marked "set the real password": their passwords cannot be
// read back. A permission on a class the script cannot name is listed as a
// comment rather than dropped.
func (sc *Scripter) ScriptDatabaseSecurity(ctx context.Context) (string, error) {
	d := sc.db
	var sec databaseSecurity
	err := d.queryRow(ctx, func(row *sql.Row) error {
		var owner sql.NullString
		if err := row.Scan(&owner, &sec.ownerSID); err != nil {
			return err
		}
		sec.owner = owner.String
		return nil
	}, `SELECT SUSER_SNAME(owner_sid), owner_sid FROM sys.databases WHERE database_id = DB_ID()`)
	if err != nil {
		return "", fmt.Errorf("gosmo: read the owner of %q: %w", d.Name, err)
	}
	if sec.roles, err = d.Roles(ctx); err != nil {
		return "", err
	}
	if sec.appRoles, err = d.applicationRoles(ctx); err != nil {
		return "", err
	}
	if sec.users, err = d.Users(ctx); err != nil {
		return "", err
	}
	if sec.schemas, err = d.Schemas(ctx); err != nil {
		return "", err
	}
	if sec.grants, err = d.securityGrants(ctx); err != nil {
		return "", err
	}
	return buildDatabaseSecurityScript(d.Name, &sec), nil
}

// databaseSecurity is what ScriptDatabaseSecurity reads, unfiltered:
// buildDatabaseSecurityScript decides what is the server's own.
type databaseSecurity struct {
	owner    string // "" when the owner's login is not visible here
	ownerSID []byte
	roles    []*DatabaseRole
	appRoles []applicationRole
	users    []*User
	schemas  []*Schema
	grants   []securityGrant
}

// applicationRole is one sys.database_principals row of type 'A'.
type applicationRole struct {
	name, defaultSchema string
}

// applicationRoles lists the database's application roles.
func (d *Database) applicationRoles(ctx context.Context) ([]applicationRole, error) {
	rows, err := d.query(ctx, `
SELECT name, default_schema_name
FROM   sys.database_principals
WHERE  type = 'A'
ORDER  BY name`)
	return scanRows(rows, err, fmt.Sprintf("list application roles in %q", d.Name), func(scan func(...any) error) (applicationRole, error) {
		var r applicationRole
		var schema sql.NullString
		if err := scan(&r.name, &schema); err != nil {
			return r, err
		}
		r.defaultSchema = schema.String
		return r, nil
	})
}

// securityGrant is one explicit sys.database_permissions row with its
// securable resolved to its name parts. name is "" when the securable could
// not be resolved (a class not listed in securityGrantsQuery, or a row whose
// object is gone); kind is the grantee type of a class-4 (principal)
// securable.
type securityGrant struct {
	grantee, state, permission string
	class                      int
	classDesc                  string
	majorID                    int
	kind, schema, name, column string
}

// securityGrantsQuery reads every explicit permission but those to dbo,
// INFORMATION_SCHEMA, sys and the ##MS_…## principals, and those on system
// objects. Every name is collated to the database's: the Service Broker
// catalogs hold theirs in a binary collation, and the UNION would not
// resolve.
const securityGrantsQuery = `
SELECT g.name, perm.state, perm.permission_name, perm.class, perm.class_desc, perm.major_id,
       s.kind, s.sch, s.name, s.col
FROM   sys.database_permissions perm
JOIN   sys.database_principals g ON g.principal_id = perm.grantee_principal_id
OUTER  APPLY (
    SELECT CAST(NULL AS char(1)) AS kind,
           OBJECT_SCHEMA_NAME(perm.major_id) COLLATE DATABASE_DEFAULT AS sch,
           OBJECT_NAME(perm.major_id) COLLATE DATABASE_DEFAULT AS name,
           CASE WHEN perm.minor_id > 0 THEN COL_NAME(perm.major_id, perm.minor_id) END COLLATE DATABASE_DEFAULT AS col
    WHERE  perm.class = 1
    UNION ALL SELECT NULL, NULL, SCHEMA_NAME(perm.major_id) COLLATE DATABASE_DEFAULT, NULL WHERE perm.class = 3
    UNION ALL SELECT x.type, NULL, x.name COLLATE DATABASE_DEFAULT, NULL FROM sys.database_principals x WHERE perm.class = 4 AND x.principal_id = perm.major_id
    UNION ALL SELECT NULL, NULL, x.name COLLATE DATABASE_DEFAULT, NULL FROM sys.assemblies x WHERE perm.class = 5 AND x.assembly_id = perm.major_id
    UNION ALL SELECT NULL, SCHEMA_NAME(x.schema_id) COLLATE DATABASE_DEFAULT, x.name COLLATE DATABASE_DEFAULT, NULL FROM sys.types x WHERE perm.class = 6 AND x.user_type_id = perm.major_id
    UNION ALL SELECT NULL, SCHEMA_NAME(x.schema_id) COLLATE DATABASE_DEFAULT, x.name COLLATE DATABASE_DEFAULT, NULL FROM sys.xml_schema_collections x WHERE perm.class = 10 AND x.xml_collection_id = perm.major_id
    UNION ALL SELECT NULL, NULL, x.name COLLATE DATABASE_DEFAULT, NULL FROM sys.service_message_types x WHERE perm.class = 15 AND x.message_type_id = perm.major_id
    UNION ALL SELECT NULL, NULL, x.name COLLATE DATABASE_DEFAULT, NULL FROM sys.service_contracts x WHERE perm.class = 16 AND x.service_contract_id = perm.major_id
    UNION ALL SELECT NULL, NULL, x.name COLLATE DATABASE_DEFAULT, NULL FROM sys.services x WHERE perm.class = 17 AND x.service_id = perm.major_id
    UNION ALL SELECT NULL, NULL, x.name COLLATE DATABASE_DEFAULT, NULL FROM sys.remote_service_bindings x WHERE perm.class = 18 AND x.remote_service_binding_id = perm.major_id
    UNION ALL SELECT NULL, NULL, x.name COLLATE DATABASE_DEFAULT, NULL FROM sys.routes x WHERE perm.class = 19 AND x.route_id = perm.major_id
    UNION ALL SELECT NULL, NULL, x.name COLLATE DATABASE_DEFAULT, NULL FROM sys.fulltext_catalogs x WHERE perm.class = 23 AND x.fulltext_catalog_id = perm.major_id
    UNION ALL SELECT NULL, NULL, x.name COLLATE DATABASE_DEFAULT, NULL FROM sys.symmetric_keys x WHERE perm.class = 24 AND x.symmetric_key_id = perm.major_id
    UNION ALL SELECT NULL, NULL, x.name COLLATE DATABASE_DEFAULT, NULL FROM sys.certificates x WHERE perm.class = 25 AND x.certificate_id = perm.major_id
    UNION ALL SELECT NULL, NULL, x.name COLLATE DATABASE_DEFAULT, NULL FROM sys.asymmetric_keys x WHERE perm.class = 26 AND x.asymmetric_key_id = perm.major_id
    UNION ALL SELECT NULL, NULL, x.name COLLATE DATABASE_DEFAULT, NULL FROM sys.fulltext_stoplists x WHERE perm.class = 29 AND x.stoplist_id = perm.major_id
    UNION ALL SELECT NULL, NULL, x.name COLLATE DATABASE_DEFAULT, NULL FROM sys.registered_search_property_lists x WHERE perm.class = 31 AND x.property_list_id = perm.major_id
) s
WHERE  perm.grantee_principal_id NOT IN (1, 3, 4)
  AND  g.name NOT LIKE N'##%'
  AND  NOT (perm.class = 1 AND (perm.major_id < 0 OR OBJECTPROPERTY(perm.major_id, 'IsMSShipped') = 1))
ORDER  BY g.name, perm.class, s.sch, s.name, s.col, perm.permission_name`

// securityGrants reads securityGrantsQuery.
func (d *Database) securityGrants(ctx context.Context) ([]securityGrant, error) {
	rows, err := d.query(ctx, securityGrantsQuery)
	return scanRows(rows, err, fmt.Sprintf("list permissions in %q", d.Name), func(scan func(...any) error) (securityGrant, error) {
		var g securityGrant
		var kind, schema, name, column sql.NullString
		if err := scan(&g.grantee, &g.state, &g.permission, &g.class, &g.classDesc, &g.majorID,
			&kind, &schema, &name, &column); err != nil {
			return g, err
		}
		g.kind, g.schema, g.name, g.column = kind.String, schema.String, name.String, column.String
		return g, nil
	})
}

// securablePrefixes is the ON clause's class keyword for each class whose
// securable a bare name (or schema.name) identifies.
var securablePrefixes = map[int]string{
	5: "ASSEMBLY", 6: "TYPE", 10: "XML SCHEMA COLLECTION",
	15: "MESSAGE TYPE", 16: "CONTRACT", 17: "SERVICE", 18: "REMOTE SERVICE BINDING", 19: "ROUTE",
	23: "FULLTEXT CATALOG", 24: "SYMMETRIC KEY", 25: "CERTIFICATE", 26: "ASYMMETRIC KEY",
	29: "FULLTEXT STOPLIST", 31: "SEARCH PROPERTY LIST",
}

// securable renders g's ON target — "" for the database itself — and
// whether it could.
func (g securityGrant) securable() (string, bool) {
	if g.class == 0 {
		return "", true
	}
	if g.name == "" {
		return "", false
	}
	switch g.class {
	case 1:
		on := "OBJECT::" + qualifiedName(g.schema, g.name)
		if g.column != "" {
			on += " (" + quoteIdent(g.column) + ")"
		}
		return on, true
	case 3:
		return "SCHEMA::" + quoteIdent(g.name), true
	case 4:
		switch g.kind {
		case "R":
			return "ROLE::" + quoteIdent(g.name), true
		case "A":
			return "APPLICATION ROLE::" + quoteIdent(g.name), true
		}
		return "USER::" + quoteIdent(g.name), true
	}
	p, ok := securablePrefixes[g.class]
	if !ok {
		return "", false
	}
	if g.schema != "" {
		return p + "::" + qualifiedName(g.schema, g.name), true
	}
	return p + "::" + quoteIdent(g.name), true
}

// statement renders g as its GRANT, DENY or REVOKE, or the comment listing
// one it cannot name.
func (g securityGrant) statement() string {
	verb, to := "GRANT", " TO "
	switch g.state {
	case "D":
		verb = "DENY"
	case "R":
		verb, to = "REVOKE", " FROM "
	}
	on, ok := g.securable()
	if !ok {
		return "-- not scripted: " + tsql.CommentSafe(fmt.Sprintf("%s %s on %s id %d%s%s",
			verb, g.permission, g.classDesc, g.majorID, strings.ToLower(to), quoteIdent(g.grantee))) + "\n"
	}
	stmt := verb + " " + g.permission
	if on != "" {
		stmt += " ON " + on
	}
	stmt += to + quoteIdent(g.grantee)
	if g.state == "W" {
		stmt += " WITH GRANT OPTION"
	}
	return stmt + ";\n"
}

// isServerPrincipal reports a principal the server makes itself: public (0),
// dbo, guest, INFORMATION_SCHEMA and sys (1–4), or a ##MS_…## one.
func isServerPrincipal(id int, name string) bool {
	return id <= 4 || strings.HasPrefix(name, "##")
}

// buildDatabaseSecurityScript assembles ScriptDatabaseSecurity's script for
// dbName (named in the header comment only) from what it read.
func buildDatabaseSecurityScript(dbName string, sec *databaseSecurity) string {
	opts := ScriptOptions{IncludeIfNotExists: true}
	var sb strings.Builder
	fmt.Fprintf(&sb, "/* Security of database %s. Every password below is a placeholder: the\n"+
		"   server does not expose them. Set the real one on each line marked\n"+
		"   \"set the real password\" before running this. */\n",
		tsql.BlockCommentSafe(quoteIdent(dbName)))

	section := func(title string) { fmt.Fprintf(&sb, "\n-- %s\n", title) }

	section("Database owner")
	if sec.owner == "" {
		fmt.Fprintf(&sb, "-- the owner's login is not visible here (SID %s)\n", binaryLiteral(sec.ownerSID))
	} else {
		fmt.Fprintf(&sb, "DECLARE @db nvarchar(300) = QUOTENAME(DB_NAME());\n"+
			"EXEC (N'ALTER AUTHORIZATION ON DATABASE::' + @db + N' TO %s;');\nGO\n",
			escapeSingle(quoteIdent(sec.owner)))
	}

	// principals is every name a membership may name: a scripted role or user.
	principals := map[string]bool{}
	var roles []*DatabaseRole
	for _, r := range sec.roles {
		if !r.IsFixedRole && !isServerPrincipal(r.ID, r.Name) {
			roles = append(roles, r)
			principals[r.Name] = true
		}
	}
	section("Database roles")
	for _, r := range roles {
		sb.WriteString(buildDatabaseRoleScript(&DatabaseRole{Name: r.Name, Owner: "dbo"}, opts))
	}

	section("Application roles")
	for _, r := range sec.appRoles {
		fmt.Fprintf(&sb, "IF DATABASE_PRINCIPAL_ID(N'%s') IS NULL\nCREATE APPLICATION ROLE %s WITH PASSWORD = N'%s'",
			escapeSingle(r.name), quoteIdent(r.name), PasswordPlaceholder)
		if r.defaultSchema != "" {
			sb.WriteString(", DEFAULT_SCHEMA = " + quoteIdent(r.defaultSchema))
		}
		sb.WriteString(";  -- set the real password\nGO\n")
	}

	section("Database users")
	for _, u := range sec.users {
		if isServerPrincipal(u.ID, u.Name) {
			continue
		}
		principals[u.Name] = true
		s := buildUserScript(u, opts)
		if strings.EqualFold(u.AuthType, "DATABASE") {
			s = strings.Replace(s, ";\nGO\n", ";  -- set the real password\nGO\n", 1)
		}
		sb.WriteString(s)
	}

	var schemas []*Schema
	for _, s := range sec.schemas {
		if s.ID > 4 && s.ID < 16384 {
			schemas = append(schemas, s)
		}
	}
	section("Schemas")
	for _, s := range schemas {
		sb.WriteString(buildSchemaScript(s, opts))
	}

	// A schema that already existed keeps its owner through the guarded
	// CREATE above; this hands it over either way.
	section("Ownership")
	owned := false
	for _, s := range schemas {
		if s.Owner != "" {
			fmt.Fprintf(&sb, "ALTER AUTHORIZATION ON SCHEMA::%s TO %s;\n", quoteIdent(s.Name), quoteIdent(s.Owner))
			owned = true
		}
	}
	for _, r := range roles {
		if r.Owner != "" && r.Owner != "dbo" {
			fmt.Fprintf(&sb, "ALTER AUTHORIZATION ON ROLE::%s TO %s;\n", quoteIdent(r.Name), quoteIdent(r.Owner))
			owned = true
		}
	}
	if owned {
		sb.WriteString("GO\n")
	}

	section("Role memberships")
	members := false
	for _, r := range sec.roles {
		if strings.HasPrefix(r.Name, "##") {
			continue
		}
		for _, m := range r.Members {
			if principals[m] {
				fmt.Fprintf(&sb, "ALTER ROLE %s ADD MEMBER %s;\n", quoteIdent(r.Name), quoteIdent(m))
				members = true
			}
		}
	}
	if members {
		sb.WriteString("GO\n")
	}

	section("Permissions")
	for _, g := range sec.grants {
		sb.WriteString(g.statement())
	}
	if len(sec.grants) > 0 {
		sb.WriteString("GO\n")
	}
	return sb.String()
}
