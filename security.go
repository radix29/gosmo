package gosmo

// security.go is the GRANT/DENY/REVOKE surface at the three scopes a database
// has: one object, one schema, and the database itself — each with the
// allowlist of permission names that scope accepts, and the read of what is
// currently granted. Always Encrypted's keys are in always_encrypted.go and
// row-level security in security_policy.go.

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
)

// ============================================================
// Object permissions (GRANT / DENY / REVOKE)
// ============================================================

// PermissionEntry is one GRANT/DENY entry recorded for a securable, as
// reported by sys.database_permissions. The permission-name and state enums
// live in types.go (ObjectPermission, PermissionState).
type PermissionEntry struct {
	Principal     string
	PrincipalType string // e.g. "DATABASE_ROLE", "SQL_USER"
	Grantor       string
	Permission    ObjectPermission
	State         PermissionState
}

// permissionEntrySelect is the select list and the two grantee/grantor joins
// every sys.database_permissions read in this file shares; each caller
// appends its own WHERE and ORDER BY, because the three scopes identify their
// securable differently — OBJECT_ID for an object, SCHEMA_ID for a schema,
// and nothing at all for the database itself.
const permissionEntrySelect = `
SELECT pr.name, pr.type_desc, grantor.name, dp.permission_name, dp.state_desc
FROM   sys.database_permissions dp
JOIN   sys.database_principals pr      ON pr.principal_id      = dp.grantee_principal_id
JOIN   sys.database_principals grantor ON grantor.principal_id = dp.grantor_principal_id`

// scanPermissionEntries reads the rows permissionEntrySelect returns into
// PermissionEntry values. It returns bare errors; the caller names the scope
// it was reading. DatabasePermissions does its own scan because its
// DatabasePermissionEntry keeps permission and state as plain strings.
func scanPermissionEntries(rows *sql.Rows) ([]*PermissionEntry, error) {
	var grants []*PermissionEntry
	for rows.Next() {
		g := &PermissionEntry{}
		var perm, state string
		if err := rows.Scan(&g.Principal, &g.PrincipalType, &g.Grantor, &perm, &state); err != nil {
			return nil, err
		}
		g.Permission = ObjectPermission(perm)
		g.State = PermissionState(state)
		grants = append(grants, g)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return grants, nil
}

// Permissions returns the GRANT/DENY entries recorded for schema.name —
// SSMS's object Properties > Permissions page.
func (d *Database) Permissions(ctx context.Context, schema, name string) ([]*PermissionEntry, error) {
	const q = permissionEntrySelect + `
WHERE  dp.major_id = OBJECT_ID(@p1) AND dp.minor_id = 0
ORDER  BY pr.name, dp.permission_name`

	ref := qualifiedName(schema, name)
	rows, err := d.query(ctx, q, ref)
	if err != nil {
		return nil, fmt.Errorf("gosmo: permissions for %s: %w", ref, err)
	}
	defer rows.Close()

	grants, err := scanPermissionEntries(rows.Rows)
	if err != nil {
		return nil, fmt.Errorf("gosmo: permissions for %s: %w", ref, err)
	}
	return grants, nil
}

// PrincipalSecurable is one GRANT/DENY entry for a securable that a
// principal (typically a database role) has an explicit permission on —
// the inverse of Permissions, which is "one securable, every principal."
// This is "one principal, every securable" — SSMS's Database Role
// Properties > Securables page. SecurableType is "TABLE", "VIEW",
// "SCHEMA", or "DATABASE"; Schema and Name are empty for "DATABASE".
type PrincipalSecurable struct {
	SecurableType string
	Schema        string
	Name          string
	Permission    string
	State         string
}

// securableObjectTypeNames maps sys.objects.type_desc to the SecurableType
// string PrincipalSecurable reports.
var securableObjectTypeNames = map[string]string{
	"USER_TABLE": "TABLE",
	"VIEW":       "VIEW",
}

// PermissionsForPrincipal returns every explicit GRANT/DENY entry recorded
// for principal across database-, schema-, and table/view-scoped
// securables. Stored procedure and function securables are deliberately
// excluded — they need their own permission catalog (EXECUTE-centric,
// distinct from the table/view one) not built yet; see SchemaPermissionNames/
// ObjectPermissionNames for the catalogs this DOES cover.
func (d *Database) PermissionsForPrincipal(ctx context.Context, principal string) ([]*PrincipalSecurable, error) {
	const q = `
SELECT dp.class_desc, dp.permission_name, dp.state_desc,
       COALESCE(objSchema.name, sch.name, N'') AS schema_name,
       COALESCE(obj.name, N'') AS object_name,
       COALESCE(obj.type_desc, N'') AS object_type
FROM   sys.database_permissions dp
JOIN   sys.database_principals pr ON pr.principal_id = dp.grantee_principal_id
LEFT   JOIN sys.schemas sch ON dp.class_desc = 'SCHEMA' AND sch.schema_id = dp.major_id
LEFT   JOIN sys.objects obj ON dp.class_desc = 'OBJECT_OR_COLUMN' AND obj.object_id = dp.major_id
                            AND dp.minor_id = 0 AND obj.type IN ('U','V')
LEFT   JOIN sys.schemas objSchema ON objSchema.schema_id = obj.schema_id
WHERE  pr.name = @p1
AND    dp.class_desc IN ('DATABASE','SCHEMA','OBJECT_OR_COLUMN')
AND    (dp.class_desc <> 'OBJECT_OR_COLUMN' OR obj.object_id IS NOT NULL)
ORDER  BY dp.class_desc, schema_name, object_name, dp.permission_name`

	rows, err := d.query(ctx, q, principal)
	return scanRows(rows, err, fmt.Sprintf("permissions for principal %q in %q", principal, d.Name), func(scan func(...any) error) (*PrincipalSecurable, error) {
		e := &PrincipalSecurable{}
		var class, objType string
		if err := scan(&class, &e.Permission, &e.State, &e.Schema, &e.Name, &objType); err != nil {
			return nil, err
		}
		switch class {
		case "DATABASE":
			e.SecurableType = "DATABASE"
		case "SCHEMA":
			// The query's schema_name column lands in e.Schema for every
			// class (it's what resolves an OBJECT_OR_COLUMN row's
			// containing schema) — but for a SCHEMA row itself, that value
			// *is* the securable's own name, not a containing schema.
			// Normalize so Name is always "the securable's own name" and
			// Schema is always "containing schema, empty if none", matching
			// every other securable-type row (and what callers building a
			// display label/key from Type+Schema+Name expect).
			e.SecurableType = "SCHEMA"
			e.Name = e.Schema
			e.Schema = ""
		default:
			e.SecurableType = securableObjectTypeNames[objType]
		}
		return e, nil
	})
}

// objectPermissionNames allowlists every object-scoped permission name SQL
// Server accepts in a GRANT/DENY/REVOKE ... statement — see
// serverPermissionNames (server_security.go) for why an allowlist rather
// than quoting. This is the set valid on tables/views specifically — GRANT
// EXECUTE on a table fails with "Granted or revoked privilege EXECUTE is
// not compatible with object". A future stored-procedure/function
// securable would need its own set (EXECUTE applies there, REFERENCES does
// not).
var objectPermissionNames = map[ObjectPermission]bool{
	PermAlter: true, PermControl: true, PermDelete: true,
	PermInsert: true, PermReferences: true, PermSelect: true, PermTakeOwnership: true,
	PermUpdate: true, PermView: true, PermViewChangeTracking: true,
}

// validObjectPermission reports whether name is a recognized object-scoped
// permission name.
func validObjectPermission(name ObjectPermission) bool { return objectPermissionNames[name] }

// ObjectPermissionNames returns every object-scoped permission name
// GRANT/DENY/REVOKE accepts on a table or view, sorted — see
// ServerPermissionNames for what it's used for.
func ObjectPermissionNames() []string {
	names := make([]string, 0, len(objectPermissionNames))
	for name := range objectPermissionNames {
		names = append(names, string(name))
	}
	slices.Sort(names)
	return names
}

// ============================================================
// Schema-scoped permissions (GRANT/DENY ON SCHEMA::x — grants every
// current and future object in the schema at once)
// ============================================================

// schemaPermissionNames allowlists every schema-scoped permission name SQL
// Server accepts in a GRANT/DENY/REVOKE ... ON SCHEMA::x statement — the
// same set as objectPermissionNames plus EXECUTE, which tables/views
// reject but schemas accept (it grants EXECUTE on every routine in the
// schema).
var schemaPermissionNames = map[ObjectPermission]bool{
	PermAlter: true, PermControl: true, PermDelete: true, PermExecute: true,
	PermInsert: true, PermReferences: true, PermSelect: true, PermTakeOwnership: true,
	PermUpdate: true, PermView: true, PermViewChangeTracking: true,
}

// validSchemaPermission reports whether name is a recognized schema-scoped
// permission name.
func validSchemaPermission(name ObjectPermission) bool { return schemaPermissionNames[name] }

// SchemaPermissionNames returns every schema-scoped permission name
// GRANT/DENY/REVOKE accepts ON SCHEMA::x, sorted — see ObjectPermissionNames
// for what it's used for.
func SchemaPermissionNames() []string {
	names := make([]string, 0, len(schemaPermissionNames))
	for name := range schemaPermissionNames {
		names = append(names, string(name))
	}
	slices.Sort(names)
	return names
}

// SchemaPermissions returns the GRANT/DENY entries recorded on
// SCHEMA::schemaName — SSMS's Schema Properties > Permissions page. This
// is the schema-scoped analog of Permissions: that one resolves its
// securable via OBJECT_ID(schema.name), which only works for table/view
// securables — a schema has no OBJECT_ID, so it needs its own query
// keyed on SCHEMA_ID instead.
func (d *Database) SchemaPermissions(ctx context.Context, schemaName string) ([]*PermissionEntry, error) {
	const q = permissionEntrySelect + `
WHERE  dp.class_desc = 'SCHEMA' AND dp.major_id = SCHEMA_ID(@p1)
ORDER  BY pr.name, dp.permission_name`

	rows, err := d.query(ctx, q, schemaName)
	if err != nil {
		return nil, fmt.Errorf("gosmo: schema permissions for %q in %q: %w", schemaName, d.Name, err)
	}
	defer rows.Close()

	grants, err := scanPermissionEntries(rows.Rows)
	if err != nil {
		return nil, fmt.Errorf("gosmo: schema permissions for %q in %q: %w", schemaName, d.Name, err)
	}
	return grants, nil
}

// ============================================================
// Database-scoped permissions (GRANT/DENY not tied to a specific object —
// e.g. CONNECT, CREATE TABLE, ALTER ANY USER)
// ============================================================

// DatabasePermissionEntry is one GRANT/DENY entry recorded at database
// scope, as reported by sys.database_permissions — SSMS's Database
// Properties > Permissions page.
type DatabasePermissionEntry struct {
	Principal     string
	PrincipalType string // e.g. "DATABASE_ROLE", "SQL_USER"
	Grantor       string
	Permission    DatabasePermission // e.g. "CONNECT", "CREATE TABLE", "ALTER"
	State         string             // "GRANT", "GRANT_WITH_GRANT_OPTION", "DENY"
}

// DatabasePermissions returns every database-scoped GRANT/DENY entry —
// permissions granted on the database itself, not on a specific object
// within it (see Permissions for that).
func (d *Database) DatabasePermissions(ctx context.Context) ([]*DatabasePermissionEntry, error) {
	const q = permissionEntrySelect + `
WHERE  dp.class_desc = 'DATABASE'
ORDER  BY pr.name, dp.permission_name`

	rows, err := d.query(ctx, q)
	return scanRows(rows, err, fmt.Sprintf("database permissions in %q", d.Name), func(scan func(...any) error) (*DatabasePermissionEntry, error) {
		e := &DatabasePermissionEntry{}
		if err := scan(&e.Principal, &e.PrincipalType, &e.Grantor, &e.Permission, &e.State); err != nil {
			return nil, err
		}
		return e, nil
	})
}

// databasePermissionNames allowlists every database-scoped permission name
// SQL Server accepts in a GRANT/DENY/REVOKE ... statement — see
// serverPermissionNames (server_security.go) for why an allowlist rather
// than quoting. Deliberately excludes "ADMINISTER DATABASE BULK
// OPERATIONS": granting it fails with "The permission 'ADMINISTER DATABASE
// BULK OPERATIONS' is not supported in this version of SQL Server.
// Alternatively, use the server level 'ADMINISTER BULK OPERATIONS'
// permission." — which serverPermissionNames already has.
var databasePermissionNames = map[DatabasePermission]bool{
	"ALTER":                                  true,
	"ALTER ANY APPLICATION ROLE":             true,
	"ALTER ANY ASSEMBLY":                     true,
	"ALTER ANY ASYMMETRIC KEY":               true,
	"ALTER ANY CERTIFICATE":                  true,
	"ALTER ANY CONTRACT":                     true,
	"ALTER ANY DATABASE AUDIT":               true,
	"ALTER ANY DATABASE DDL TRIGGER":         true,
	"ALTER ANY DATABASE EVENT NOTIFICATION":  true,
	"ALTER ANY DATASPACE":                    true,
	"ALTER ANY FULLTEXT CATALOG":             true,
	"ALTER ANY MESSAGE TYPE":                 true,
	"ALTER ANY REMOTE SERVICE BINDING":       true,
	"ALTER ANY ROLE":                         true,
	"ALTER ANY ROUTE":                        true,
	"ALTER ANY SCHEMA":                       true,
	"ALTER ANY SECURITY POLICY":              true,
	"ALTER ANY SERVICE":                      true,
	"ALTER ANY SYMMETRIC KEY":                true,
	"ALTER ANY USER":                         true,
	"AUTHENTICATE":                           true,
	"BACKUP DATABASE":                        true,
	"BACKUP LOG":                             true,
	"CHECKPOINT":                             true,
	"CONNECT":                                true,
	"CONNECT REPLICATION":                    true,
	"CONTROL":                                true,
	"CREATE AGGREGATE":                       true,
	"CREATE ASSEMBLY":                        true,
	"CREATE ASYMMETRIC KEY":                  true,
	"CREATE CERTIFICATE":                     true,
	"CREATE CONTRACT":                        true,
	"CREATE DATABASE":                        true,
	"CREATE DATABASE DDL EVENT NOTIFICATION": true,
	"CREATE DEFAULT":                         true,
	"CREATE FULLTEXT CATALOG":                true,
	"CREATE FUNCTION":                        true,
	"CREATE MESSAGE TYPE":                    true,
	"CREATE PROCEDURE":                       true,
	"CREATE QUEUE":                           true,
	"CREATE REMOTE SERVICE BINDING":          true,
	"CREATE ROLE":                            true,
	"CREATE ROUTE":                           true,
	"CREATE RULE":                            true,
	"CREATE SCHEMA":                          true,
	"CREATE SERVICE":                         true,
	"CREATE SYMMETRIC KEY":                   true,
	"CREATE SYNONYM":                         true,
	"CREATE TABLE":                           true,
	"CREATE TYPE":                            true,
	"CREATE VIEW":                            true,
	"CREATE XML SCHEMA COLLECTION":           true,
	"DELETE":                                 true,
	"EXECUTE":                                true,
	"EXECUTE ANY EXTERNAL SCRIPT":            true,
	"INSERT":                                 true,
	"KILL DATABASE CONNECTION":               true,
	"REFERENCES":                             true,
	"SELECT":                                 true,
	"SHOWPLAN":                               true,
	"SUBSCRIBE QUERY NOTIFICATIONS":          true,
	"TAKE OWNERSHIP":                         true,
	"UNMASK":                                 true,
	"UPDATE":                                 true,
	"VIEW DATABASE STATE":                    true,
	"VIEW DEFINITION":                        true,
}

// validDatabasePermission reports whether name is a recognized
// database-scoped permission name.
func validDatabasePermission(name DatabasePermission) bool { return databasePermissionNames[name] }

// DatabasePermissionNames returns every database-scoped permission name
// GRANT/DENY/REVOKE accepts, sorted — see ServerPermissionNames for what
// it's used for.
func DatabasePermissionNames() []string {
	names := make([]string, 0, len(databasePermissionNames))
	for name := range databasePermissionNames {
		names = append(names, string(name))
	}
	slices.Sort(names)
	return names
}
