package gosmo

import (
	"context"
	"database/sql"
	"fmt"
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
SELECT pr.name, pr.type_desc, dp.permission_name, dp.state_desc
FROM   sys.database_permissions dp
JOIN   sys.database_principals pr ON pr.principal_id = dp.grantee_principal_id
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
		if err := rows.Scan(&g.Principal, &g.PrincipalType, &perm, &state); err != nil {
			return nil, err
		}
		g.Permission = ObjectPermission(perm)
		g.State = PermissionState(state)
		grants = append(grants, g)
	}
	return grants, rows.Err()
}

// GrantPermission grants permission on schema.name to principal.
func (d *Database) GrantPermission(schema, name string, permission ObjectPermission, principal string) error {
	return d.GrantPermissionContext(context.Background(), schema, name, permission, principal)
}

// GrantPermissionContext is the context-aware variant of GrantPermission.
func (d *Database) GrantPermissionContext(ctx context.Context, schema, name string, permission ObjectPermission, principal string) error {
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
	ref := qualifiedName(schema, name)
	q := fmt.Sprintf("REVOKE %s ON %s FROM %s", permission, ref, quoteIdent(principal))
	if _, err := d.exec(ctx, q); err != nil {
		return fmt.Errorf("gosmo: revoke %s on %s from %q: %w", permission, ref, principal, err)
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
// than quoting.
var databasePermissionNames = map[string]bool{
	"ADMINISTER DATABASE BULK OPERATIONS":    true,
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
