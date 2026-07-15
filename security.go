package gosmo

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
)

// ============================================================
// Always Encrypted - Column Master Keys & Column Encryption Keys
// ============================================================

// ColumnMasterKey mirrors sys.column_master_keys.
type ColumnMasterKey struct {
	db                       *Database
	Name                     string
	ID                       int
	KeyStoreProviderName     string
	KeyPath                  string
	AllowEnclaveComputations bool
}

// ColumnMasterKeys returns all column master keys in the database.
func (d *Database) ColumnMasterKeys() ([]*ColumnMasterKey, error) {
	const q = `
SELECT name, column_master_key_id,
       key_store_provider_name, key_path,
       allow_enclave_computations
FROM   sys.column_master_keys
ORDER  BY name`

	rows, err := d.query(context.Background(), q)
	if err != nil {
		return nil, fmt.Errorf("gosmo: list column master keys: %w", err)
	}
	defer rows.Close()

	var keys []*ColumnMasterKey
	for rows.Next() {
		k := &ColumnMasterKey{db: d}
		if err := rows.Scan(&k.Name, &k.ID,
			&k.KeyStoreProviderName, &k.KeyPath,
			&k.AllowEnclaveComputations); err != nil {
			return nil, err
		}
		keys = append(keys, k)
	}
	return keys, rows.Err()
}

// CreateColumnMasterKey creates a column master key metadata entry.
// Note: the actual key must already exist in the key store.
func (d *Database) CreateColumnMasterKey(name, keyStoreProvider, keyPath string, enclaveComputations bool) error {
	enclave := "YES"
	if !enclaveComputations {
		enclave = "NO"
	}
	q := fmt.Sprintf(`
CREATE COLUMN MASTER KEY %s
WITH (
    KEY_STORE_PROVIDER_NAME = N'%s',
    KEY_PATH = N'%s',
    ENCLAVE_COMPUTATIONS = %s
)`, name, escapeSingle(keyStoreProvider), escapeSingle(keyPath), enclave)
	_, err := d.exec(context.Background(), q)
	if err != nil {
		return fmt.Errorf("gosmo: create column master key [%s]: %w", name, err)
	}
	return nil
}

// Drop drops the column master key.
func (cmk *ColumnMasterKey) Drop() error {
	_, err := cmk.db.exec(context.Background(),
		fmt.Sprintf("DROP COLUMN MASTER KEY %s", quoteIdent(cmk.Name)))
	if err != nil {
		return fmt.Errorf("gosmo: drop column master key [%s]: %w", cmk.Name, err)
	}
	return nil
}

// -- Column Encryption Keys ----------------------------------------------------

// ColumnEncryptionKey mirrors sys.column_encryption_keys.
type ColumnEncryptionKey struct {
	db                  *Database
	Name                string
	ID                  int
	MasterKeyName       string
	EncryptionAlgorithm string
}

// ColumnEncryptionKeys returns all column encryption keys in the database.
func (d *Database) ColumnEncryptionKeys() ([]*ColumnEncryptionKey, error) {
	const q = `
SELECT cek.name, cek.column_encryption_key_id,
       cmk.name AS master_key_name,
       cekv.encryption_algorithm_name
FROM   sys.column_encryption_keys cek
JOIN   sys.column_encryption_key_values cekv ON cekv.column_encryption_key_id = cek.column_encryption_key_id
JOIN   sys.column_master_keys cmk ON cmk.column_master_key_id = cekv.column_master_key_id
ORDER  BY cek.name`

	rows, err := d.query(context.Background(), q)
	if err != nil {
		return nil, fmt.Errorf("gosmo: list column encryption keys: %w", err)
	}
	defer rows.Close()

	var keys []*ColumnEncryptionKey
	for rows.Next() {
		k := &ColumnEncryptionKey{db: d}
		var algo sql.NullString
		if err := rows.Scan(&k.Name, &k.ID, &k.MasterKeyName, &algo); err != nil {
			return nil, err
		}
		k.EncryptionAlgorithm = algo.String
		keys = append(keys, k)
	}
	return keys, rows.Err()
}

// Drop drops the column encryption key.
func (cek *ColumnEncryptionKey) Drop() error {
	_, err := cek.db.exec(context.Background(),
		fmt.Sprintf("DROP COLUMN ENCRYPTION KEY %s", quoteIdent(cek.Name)))
	if err != nil {
		return fmt.Errorf("gosmo: drop column encryption key [%s]: %w", cek.Name, err)
	}
	return nil
}

// ============================================================
// Row-Level Security - Security Policies
// ============================================================

// SecurityPolicy mirrors sys.security_policies.
type SecurityPolicy struct {
	db                  *Database
	Name                string
	Schema              string
	ObjectID            int
	IsEnabled           bool
	IsNotForReplication bool
	Predicates          []*SecurityPredicate
}

// SecurityPredicate represents one predicate in a security policy.
type SecurityPredicate struct {
	PredicateType       string // "FILTER" or "BLOCK"
	PredicateDefinition string
	TargetSchema        string
	TargetTable         string
	Operation           string // for BLOCK: AFTER INSERT, AFTER UPDATE, etc.
}

// SecurityPolicies returns all security policies in the database.
func (d *Database) SecurityPolicies() ([]*SecurityPolicy, error) {
	const q = `
SELECT sp.name, SCHEMA_NAME(sp.schema_id), sp.object_id,
       sp.is_enabled, sp.is_not_for_replication
FROM   sys.security_policies sp
ORDER  BY sp.name`

	rows, err := d.query(context.Background(), q)
	if err != nil {
		return nil, fmt.Errorf("gosmo: list security policies: %w", err)
	}
	defer rows.Close()

	var policies []*SecurityPolicy
	for rows.Next() {
		p := &SecurityPolicy{db: d}
		if err := rows.Scan(&p.Name, &p.Schema, &p.ObjectID,
			&p.IsEnabled, &p.IsNotForReplication); err != nil {
			return nil, err
		}

		// Load predicates
		preds, err := d.securityPredicates(p.ObjectID)
		if err != nil {
			return nil, err
		}
		p.Predicates = preds
		policies = append(policies, p)
	}
	return policies, rows.Err()
}

func (d *Database) securityPredicates(policyObjectID int) ([]*SecurityPredicate, error) {
	const q = `
SELECT spr.predicate_type_desc, spr.predicate_definition,
       SCHEMA_NAME(t.schema_id), t.name, spr.operation_desc
FROM   sys.security_predicates spr
JOIN   sys.tables t ON t.object_id = spr.target_object_id
WHERE  spr.object_id = @p1
ORDER  BY spr.predicate_type_desc`

	rows, err := d.query(context.Background(), q, policyObjectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var preds []*SecurityPredicate
	for rows.Next() {
		p := &SecurityPredicate{}
		var op sql.NullString
		if err := rows.Scan(&p.PredicateType, &p.PredicateDefinition,
			&p.TargetSchema, &p.TargetTable, &op); err != nil {
			return nil, err
		}
		p.Operation = op.String
		preds = append(preds, p)
	}
	return preds, rows.Err()
}

// Enable enables the security policy.
func (p *SecurityPolicy) Enable() error {
	_, err := p.db.exec(context.Background(),
		fmt.Sprintf("ALTER SECURITY POLICY %s WITH (STATE = ON)", qualifiedName(p.Schema, p.Name)))
	if err != nil {
		return fmt.Errorf("gosmo: enable security policy [%s]: %w", p.Name, err)
	}
	p.IsEnabled = true
	return nil
}

// Disable disables the security policy.
func (p *SecurityPolicy) Disable() error {
	_, err := p.db.exec(context.Background(),
		fmt.Sprintf("ALTER SECURITY POLICY %s WITH (STATE = OFF)", qualifiedName(p.Schema, p.Name)))
	if err != nil {
		return fmt.Errorf("gosmo: disable security policy [%s]: %w", p.Name, err)
	}
	p.IsEnabled = false
	return nil
}

// Drop drops the security policy.
func (p *SecurityPolicy) Drop() error {
	_, err := p.db.exec(context.Background(),
		fmt.Sprintf("DROP SECURITY POLICY IF EXISTS %s", qualifiedName(p.Schema, p.Name)))
	if err != nil {
		return fmt.Errorf("gosmo: drop security policy [%s]: %w", p.Name, err)
	}
	return nil
}

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

// Permissions returns the GRANT/DENY entries recorded for schema.name —
// SSMS's object Properties > Permissions page.
func (d *Database) Permissions(schema, name string) ([]*PermissionEntry, error) {
	return d.PermissionsContext(context.Background(), schema, name)
}

// PermissionsContext is the context-aware variant of Permissions.
func (d *Database) PermissionsContext(ctx context.Context, schema, name string) ([]*PermissionEntry, error) {
	const q = `
SELECT pr.name, pr.type_desc, grantor.name, dp.permission_name, dp.state_desc
FROM   sys.database_permissions dp
JOIN   sys.database_principals pr ON pr.principal_id = dp.grantee_principal_id
JOIN   sys.database_principals grantor ON grantor.principal_id = dp.grantor_principal_id
WHERE  dp.major_id = OBJECT_ID(@p1) AND dp.minor_id = 0
ORDER  BY pr.name, dp.permission_name`

	ref := qualifiedName(schema, name)
	rows, err := d.query(ctx, q, ref)
	if err != nil {
		return nil, fmt.Errorf("gosmo: permissions for %s: %w", ref, err)
	}
	defer rows.Close()

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
	return grants, rows.Err()
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
func (d *Database) PermissionsForPrincipal(principal string) ([]*PrincipalSecurable, error) {
	return d.PermissionsForPrincipalContext(context.Background(), principal)
}

// PermissionsForPrincipalContext is the context-aware variant of
// PermissionsForPrincipal.
func (d *Database) PermissionsForPrincipalContext(ctx context.Context, principal string) ([]*PrincipalSecurable, error) {
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
	if err != nil {
		return nil, fmt.Errorf("gosmo: permissions for principal %q in %q: %w", principal, d.name, err)
	}
	defer rows.Close()

	var entries []*PrincipalSecurable
	for rows.Next() {
		e := &PrincipalSecurable{}
		var class, objType string
		if err := rows.Scan(&class, &e.Permission, &e.State, &e.Schema, &e.Name, &objType); err != nil {
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
		entries = append(entries, e)
	}
	return entries, rows.Err()
}

// objectPermissionNames allowlists every object-scoped permission name SQL
// Server accepts in a GRANT/DENY/REVOKE ... statement — see
// serverPermissionNames (server_security.go) for why an allowlist rather
// than quoting. This is the set valid on tables/views specifically —
// verified live (GRANT EXECUTE ON a table fails with "Granted or revoked
// privilege EXECUTE is not compatible with object"); a future
// stored-procedure/function securable would need its own (EXECUTE
// applies there, REFERENCES does not).
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

// GrantPermission grants permission on schema.name to principal.
func (d *Database) GrantPermission(schema, name string, permission ObjectPermission, principal string) error {
	return d.GrantPermissionContext(context.Background(), schema, name, permission, principal)
}

// GrantPermissionContext is the context-aware variant of GrantPermission.
func (d *Database) GrantPermissionContext(ctx context.Context, schema, name string, permission ObjectPermission, principal string) error {
	if !validObjectPermission(permission) {
		return fmt.Errorf("gosmo: grant permission: unrecognized permission %q", permission)
	}
	ref := qualifiedName(schema, name)
	q := fmt.Sprintf("GRANT %s ON %s TO %s", permission, ref, quoteIdent(principal))
	if _, err := d.exec(ctx, q); err != nil {
		return fmt.Errorf("gosmo: grant %s on %s to %q: %w", permission, ref, principal, err)
	}
	return nil
}

// DenyPermission denies permission on schema.name to principal.
func (d *Database) DenyPermission(schema, name string, permission ObjectPermission, principal string) error {
	return d.DenyPermissionContext(context.Background(), schema, name, permission, principal)
}

// DenyPermissionContext is the context-aware variant of DenyPermission.
func (d *Database) DenyPermissionContext(ctx context.Context, schema, name string, permission ObjectPermission, principal string) error {
	if !validObjectPermission(permission) {
		return fmt.Errorf("gosmo: deny permission: unrecognized permission %q", permission)
	}
	ref := qualifiedName(schema, name)
	q := fmt.Sprintf("DENY %s ON %s TO %s", permission, ref, quoteIdent(principal))
	if _, err := d.exec(ctx, q); err != nil {
		return fmt.Errorf("gosmo: deny %s on %s to %q: %w", permission, ref, principal, err)
	}
	return nil
}

// RevokePermission revokes permission on schema.name from principal.
func (d *Database) RevokePermission(schema, name string, permission ObjectPermission, principal string) error {
	return d.RevokePermissionContext(context.Background(), schema, name, permission, principal)
}

// RevokePermissionContext is the context-aware variant of RevokePermission.
func (d *Database) RevokePermissionContext(ctx context.Context, schema, name string, permission ObjectPermission, principal string) error {
	if !validObjectPermission(permission) {
		return fmt.Errorf("gosmo: revoke permission: unrecognized permission %q", permission)
	}
	ref := qualifiedName(schema, name)
	q := fmt.Sprintf("REVOKE %s ON %s FROM %s", permission, ref, quoteIdent(principal))
	if _, err := d.exec(ctx, q); err != nil {
		return fmt.Errorf("gosmo: revoke %s on %s from %q: %w", permission, ref, principal, err)
	}
	return nil
}

// ============================================================
// Schema-scoped permissions (GRANT/DENY ON SCHEMA::x — grants every
// current and future object in the schema at once)
// ============================================================

// schemaPermissionNames allowlists every schema-scoped permission name SQL
// Server accepts in a GRANT/DENY/REVOKE ... ON SCHEMA::x statement — the
// same set as objectPermissionNames plus EXECUTE, which tables/views
// reject but schemas accept (grants EXECUTE on every routine in the
// schema). Verified live: GRANT EXECUTE/ALTER/SELECT/VIEW CHANGE
// TRACKING/TAKE OWNERSHIP ON SCHEMA::x all succeed.
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
func (d *Database) SchemaPermissions(schemaName string) ([]*PermissionEntry, error) {
	return d.SchemaPermissionsContext(context.Background(), schemaName)
}

// SchemaPermissionsContext is the context-aware variant of SchemaPermissions.
func (d *Database) SchemaPermissionsContext(ctx context.Context, schemaName string) ([]*PermissionEntry, error) {
	const q = `
SELECT pr.name, pr.type_desc, grantor.name, dp.permission_name, dp.state_desc
FROM   sys.database_permissions dp
JOIN   sys.database_principals pr      ON pr.principal_id = dp.grantee_principal_id
JOIN   sys.database_principals grantor ON grantor.principal_id = dp.grantor_principal_id
WHERE  dp.class_desc = 'SCHEMA' AND dp.major_id = SCHEMA_ID(@p1)
ORDER  BY pr.name, dp.permission_name`

	rows, err := d.query(ctx, q, schemaName)
	if err != nil {
		return nil, fmt.Errorf("gosmo: schema permissions for %q in %q: %w", schemaName, d.name, err)
	}
	defer rows.Close()

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
	return grants, rows.Err()
}

// GrantSchemaPermission grants permission on a schema to principal.
func (d *Database) GrantSchemaPermission(schemaName string, permission ObjectPermission, principal string) error {
	return d.GrantSchemaPermissionContext(context.Background(), schemaName, permission, principal)
}

// GrantSchemaPermissionContext is the context-aware variant of GrantSchemaPermission.
func (d *Database) GrantSchemaPermissionContext(ctx context.Context, schemaName string, permission ObjectPermission, principal string) error {
	if !validSchemaPermission(permission) {
		return fmt.Errorf("gosmo: grant schema permission: unrecognized permission %q", permission)
	}
	q := fmt.Sprintf("GRANT %s ON SCHEMA::%s TO %s", permission, quoteIdent(schemaName), quoteIdent(principal))
	if _, err := d.exec(ctx, q); err != nil {
		return fmt.Errorf("gosmo: grant %s on schema %q to %q: %w", permission, schemaName, principal, err)
	}
	return nil
}

// DenySchemaPermission denies permission on a schema to principal.
func (d *Database) DenySchemaPermission(schemaName string, permission ObjectPermission, principal string) error {
	return d.DenySchemaPermissionContext(context.Background(), schemaName, permission, principal)
}

// DenySchemaPermissionContext is the context-aware variant of DenySchemaPermission.
func (d *Database) DenySchemaPermissionContext(ctx context.Context, schemaName string, permission ObjectPermission, principal string) error {
	if !validSchemaPermission(permission) {
		return fmt.Errorf("gosmo: deny schema permission: unrecognized permission %q", permission)
	}
	q := fmt.Sprintf("DENY %s ON SCHEMA::%s TO %s", permission, quoteIdent(schemaName), quoteIdent(principal))
	if _, err := d.exec(ctx, q); err != nil {
		return fmt.Errorf("gosmo: deny %s on schema %q to %q: %w", permission, schemaName, principal, err)
	}
	return nil
}

// RevokeSchemaPermission revokes permission on a schema from principal.
func (d *Database) RevokeSchemaPermission(schemaName string, permission ObjectPermission, principal string) error {
	return d.RevokeSchemaPermissionContext(context.Background(), schemaName, permission, principal)
}

// RevokeSchemaPermissionContext is the context-aware variant of RevokeSchemaPermission.
func (d *Database) RevokeSchemaPermissionContext(ctx context.Context, schemaName string, permission ObjectPermission, principal string) error {
	if !validSchemaPermission(permission) {
		return fmt.Errorf("gosmo: revoke schema permission: unrecognized permission %q", permission)
	}
	q := fmt.Sprintf("REVOKE %s ON SCHEMA::%s FROM %s", permission, quoteIdent(schemaName), quoteIdent(principal))
	if _, err := d.exec(ctx, q); err != nil {
		return fmt.Errorf("gosmo: revoke %s on schema %q from %q: %w", permission, schemaName, principal, err)
	}
	return nil
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
	Permission    string // e.g. "CONNECT", "CREATE TABLE", "ALTER"
	State         string // "GRANT", "GRANT_WITH_GRANT_OPTION", "DENY"
}

// DatabasePermissions returns every database-scoped GRANT/DENY entry —
// permissions granted on the database itself, not on a specific object
// within it (see Permissions for that).
func (d *Database) DatabasePermissions() ([]*DatabasePermissionEntry, error) {
	return d.DatabasePermissionsContext(context.Background())
}

// DatabasePermissionsContext is the context-aware variant of
// DatabasePermissions.
func (d *Database) DatabasePermissionsContext(ctx context.Context) ([]*DatabasePermissionEntry, error) {
	const q = `
SELECT pr.name, pr.type_desc, grantor.name, dp.permission_name, dp.state_desc
FROM   sys.database_permissions dp
JOIN   sys.database_principals pr      ON pr.principal_id      = dp.grantee_principal_id
JOIN   sys.database_principals grantor ON grantor.principal_id = dp.grantor_principal_id
WHERE  dp.class_desc = 'DATABASE'
ORDER  BY pr.name, dp.permission_name`

	rows, err := d.query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("gosmo: database permissions in %q: %w", d.name, err)
	}
	defer rows.Close()

	var perms []*DatabasePermissionEntry
	for rows.Next() {
		e := &DatabasePermissionEntry{}
		if err := rows.Scan(&e.Principal, &e.PrincipalType, &e.Grantor, &e.Permission, &e.State); err != nil {
			return nil, err
		}
		perms = append(perms, e)
	}
	return perms, rows.Err()
}

// databasePermissionNames allowlists every database-scoped permission name
// SQL Server accepts in a GRANT/DENY/REVOKE ... statement — see
// serverPermissionNames (server_security.go) for why an allowlist rather
// than quoting. Deliberately excludes "ADMINISTER DATABASE BULK
// OPERATIONS" — verified live that granting it fails with "The permission
// 'ADMINISTER DATABASE BULK OPERATIONS' is not supported in this version
// of SQL Server. Alternatively, use the server level 'ADMINISTER BULK
// OPERATIONS' permission." (which serverPermissionNames already has).
var databasePermissionNames = map[string]bool{
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
func validDatabasePermission(name string) bool { return databasePermissionNames[name] }

// DatabasePermissionNames returns every database-scoped permission name
// GRANT/DENY/REVOKE accepts, sorted — see ServerPermissionNames for what
// it's used for.
func DatabasePermissionNames() []string {
	names := make([]string, 0, len(databasePermissionNames))
	for name := range databasePermissionNames {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// GrantDatabasePermission grants a database-level permission to principal.
func (d *Database) GrantDatabasePermission(permission, principal string) error {
	return d.GrantDatabasePermissionContext(context.Background(), permission, principal)
}

// GrantDatabasePermissionContext is the context-aware variant of GrantDatabasePermission.
func (d *Database) GrantDatabasePermissionContext(ctx context.Context, permission, principal string) error {
	if !validDatabasePermission(permission) {
		return fmt.Errorf("gosmo: grant database permission: unrecognized permission %q", permission)
	}
	if _, err := d.exec(ctx, fmt.Sprintf("GRANT %s TO %s", permission, quoteIdent(principal))); err != nil {
		return fmt.Errorf("gosmo: grant %s to %q in %q: %w", permission, principal, d.name, err)
	}
	return nil
}

// DenyDatabasePermission denies a database-level permission to principal.
func (d *Database) DenyDatabasePermission(permission, principal string) error {
	return d.DenyDatabasePermissionContext(context.Background(), permission, principal)
}

// DenyDatabasePermissionContext is the context-aware variant of DenyDatabasePermission.
func (d *Database) DenyDatabasePermissionContext(ctx context.Context, permission, principal string) error {
	if !validDatabasePermission(permission) {
		return fmt.Errorf("gosmo: deny database permission: unrecognized permission %q", permission)
	}
	if _, err := d.exec(ctx, fmt.Sprintf("DENY %s TO %s", permission, quoteIdent(principal))); err != nil {
		return fmt.Errorf("gosmo: deny %s to %q in %q: %w", permission, principal, d.name, err)
	}
	return nil
}

// RevokeDatabasePermission revokes a database-level permission from principal.
func (d *Database) RevokeDatabasePermission(permission, principal string) error {
	return d.RevokeDatabasePermissionContext(context.Background(), permission, principal)
}

// RevokeDatabasePermissionContext is the context-aware variant of RevokeDatabasePermission.
func (d *Database) RevokeDatabasePermissionContext(ctx context.Context, permission, principal string) error {
	if !validDatabasePermission(permission) {
		return fmt.Errorf("gosmo: revoke database permission: unrecognized permission %q", permission)
	}
	if _, err := d.exec(ctx, fmt.Sprintf("REVOKE %s FROM %s", permission, quoteIdent(principal))); err != nil {
		return fmt.Errorf("gosmo: revoke %s from %q in %q: %w", permission, principal, d.name, err)
	}
	return nil
}
