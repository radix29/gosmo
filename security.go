package gosmo

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
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
	// Signature is the digital signature over the key's metadata, required
	// verbatim by CREATE COLUMN MASTER KEY's ENCLAVE_COMPUTATIONS clause —
	// it can't be recomputed from the other fields, so a key that allows
	// enclave computations cannot be scripted without it. Empty for a key
	// that doesn't.
	Signature []byte
}

// columnMasterKeySelect is the column list every column master key read
// shares; the listing adds ORDER BY, the by-name lookup a WHERE.
const columnMasterKeySelect = `
SELECT name, column_master_key_id,
       key_store_provider_name, key_path,
       allow_enclave_computations, signature
FROM   sys.column_master_keys`

// ColumnMasterKeys returns all column master keys in the database.
func (d *Database) ColumnMasterKeys() ([]*ColumnMasterKey, error) {
	return d.ColumnMasterKeysContext(context.Background())
}

// ColumnMasterKeysContext is the context-aware variant of ColumnMasterKeys.
func (d *Database) ColumnMasterKeysContext(ctx context.Context) ([]*ColumnMasterKey, error) {
	rows, err := d.query(ctx, columnMasterKeySelect+`
ORDER  BY name`)
	if err != nil {
		return nil, fmt.Errorf("gosmo: list column master keys: %w", err)
	}
	defer rows.Close()

	var keys []*ColumnMasterKey
	for rows.Next() {
		k, err := scanColumnMasterKey(d, rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("gosmo: list column master keys: %w", err)
		}
		keys = append(keys, k)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("gosmo: list column master keys: %w", err)
	}
	return keys, nil
}

// ColumnMasterKeyByName returns one column master key by name.
func (d *Database) ColumnMasterKeyByName(name string) (*ColumnMasterKey, error) {
	return d.ColumnMasterKeyByNameContext(context.Background(), name)
}

// ColumnMasterKeyByNameContext is the context-aware variant of
// ColumnMasterKeyByName.
func (d *Database) ColumnMasterKeyByNameContext(ctx context.Context, name string) (*ColumnMasterKey, error) {
	var k *ColumnMasterKey
	err := d.queryRow(ctx, func(row *sql.Row) error {
		var err error
		k, err = scanColumnMasterKey(d, row.Scan)
		return err
	}, columnMasterKeySelect+`
WHERE  name = @p1`, name)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, notFoundf("gosmo: column master key %q not found in %q", name, d.name)
		}
		return nil, fmt.Errorf("gosmo: find column master key %q in %q: %w", name, d.name, err)
	}
	return k, nil
}

func scanColumnMasterKey(d *Database, scan func(...any) error) (*ColumnMasterKey, error) {
	k := &ColumnMasterKey{db: d}
	if err := scan(&k.Name, &k.ID,
		&k.KeyStoreProviderName, &k.KeyPath,
		&k.AllowEnclaveComputations, &k.Signature); err != nil {
		return nil, err
	}
	return k, nil
}

// CreateColumnMasterKey creates a column master key metadata entry.
// Note: the actual key must already exist in the key store.
//
// enclaveComputations can only be false here. The ENCLAVE_COMPUTATIONS clause
// takes a signature over the key's metadata — CREATE COLUMN MASTER KEY spells
// it ENCLAVE_COMPUTATIONS (SIGNATURE = 0x...) and has no boolean form — and
// that signature is computed by the client from the master key's private key,
// so nothing here can supply it. Passing true returns an error naming
// CreateColumnMasterKeyWithSignature rather than emitting a statement the
// server will reject.
func (d *Database) CreateColumnMasterKey(name, keyStoreProvider, keyPath string, enclaveComputations bool) error {
	return d.CreateColumnMasterKeyContext(context.Background(), name, keyStoreProvider, keyPath, enclaveComputations)
}

// CreateColumnMasterKeyContext is the context-aware variant of CreateColumnMasterKey.
func (d *Database) CreateColumnMasterKeyContext(ctx context.Context, name, keyStoreProvider, keyPath string, enclaveComputations bool) error {
	if enclaveComputations {
		return fmt.Errorf("gosmo: create column master key [%s]: enclave computations need the key's signature, which only the client can compute: use CreateColumnMasterKeyWithSignature", name)
	}
	return d.createColumnMasterKey(ctx, name, keyStoreProvider, keyPath, nil)
}

// CreateColumnMasterKeyWithSignature creates a column master key that allows
// enclave computations. signature is the digital signature over the key's
// metadata, the same value ColumnMasterKey.Signature reads back and the
// scripter writes out verbatim; it is produced client-side by whatever holds
// the master key's private key (SSMS and the SqlColumnMasterKey PowerShell
// cmdlets both do), and the server verifies it against the rest of the
// metadata, so an empty or wrong one is rejected. Use CreateColumnMasterKey
// for a key that does not allow enclave computations.
func (d *Database) CreateColumnMasterKeyWithSignature(name, keyStoreProvider, keyPath string, signature []byte) error {
	return d.CreateColumnMasterKeyWithSignatureContext(context.Background(), name, keyStoreProvider, keyPath, signature)
}

// CreateColumnMasterKeyWithSignatureContext is the context-aware variant of
// CreateColumnMasterKeyWithSignature.
func (d *Database) CreateColumnMasterKeyWithSignatureContext(ctx context.Context, name, keyStoreProvider, keyPath string, signature []byte) error {
	if len(signature) == 0 {
		return fmt.Errorf("gosmo: create column master key [%s]: signature is empty", name)
	}
	return d.createColumnMasterKey(ctx, name, keyStoreProvider, keyPath, signature)
}

// createColumnMasterKey emits the CREATE, with the ENCLAVE_COMPUTATIONS clause
// only when a signature was given. The clause is written the way the scripter
// writes it (buildColumnMasterKeyScript), so a key created here and one scripted
// from the server read back the same.
func (d *Database) createColumnMasterKey(ctx context.Context, name, keyStoreProvider, keyPath string, signature []byte) error {
	enclave := ""
	if len(signature) > 0 {
		enclave = fmt.Sprintf(",\n    ENCLAVE_COMPUTATIONS (SIGNATURE = %s)", hexLiteral(signature))
	}
	q := fmt.Sprintf(`
CREATE COLUMN MASTER KEY %s
WITH (
    KEY_STORE_PROVIDER_NAME = N'%s',
    KEY_PATH = N'%s'%s
)`, quoteIdent(name), escapeSingle(keyStoreProvider), escapeSingle(keyPath), enclave)
	_, err := d.exec(ctx, q)
	if err != nil {
		return fmt.Errorf("gosmo: create column master key [%s]: %w", name, err)
	}
	return nil
}

// Drop drops the column master key.
func (cmk *ColumnMasterKey) Drop() error {
	return cmk.DropContext(context.Background())
}

// DropContext is the context-aware variant of Drop.
func (cmk *ColumnMasterKey) DropContext(ctx context.Context) error {
	_, err := cmk.db.exec(ctx,
		fmt.Sprintf("DROP COLUMN MASTER KEY %s", quoteIdent(cmk.Name)))
	if err != nil {
		return fmt.Errorf("gosmo: drop column master key [%s]: %w", cmk.Name, err)
	}
	return nil
}

// -- Column Encryption Keys ----------------------------------------------------

// ColumnEncryptionKey mirrors sys.column_encryption_keys.
type ColumnEncryptionKey struct {
	db   *Database
	Name string
	ID   int
	// MasterKeyName and EncryptionAlgorithm describe the key's first
	// encrypted value — the common case, where a key has exactly one.
	MasterKeyName       string
	EncryptionAlgorithm string
	// Values holds every encrypted value of the key, one per column master
	// key it is encrypted under. A key has two while its master key is being
	// rotated, and CREATE COLUMN ENCRYPTION KEY has to restate all of them.
	Values []*ColumnEncryptionKeyValue
}

// ColumnEncryptionKeyValue is one encrypted value of a column encryption
// key, from sys.column_encryption_key_values.
type ColumnEncryptionKeyValue struct {
	MasterKeyName       string
	EncryptionAlgorithm string
	// EncryptedValue is the key material encrypted under the master key.
	// Scripting the key means reproducing these bytes exactly; nothing can
	// regenerate them.
	EncryptedValue []byte
}

// columnEncryptionKeySelect is the column list and joins every column
// encryption key read shares. It returns one row per encrypted value, so
// every caller folds the rows with scanColumnEncryptionKeys.
const columnEncryptionKeySelect = `
SELECT cek.name, cek.column_encryption_key_id,
       cmk.name AS master_key_name,
       cekv.encryption_algorithm_name, cekv.encrypted_value
FROM   sys.column_encryption_keys cek
JOIN   sys.column_encryption_key_values cekv ON cekv.column_encryption_key_id = cek.column_encryption_key_id
JOIN   sys.column_master_keys cmk ON cmk.column_master_key_id = cekv.column_master_key_id`

// ColumnEncryptionKeys returns all column encryption keys in the database.
func (d *Database) ColumnEncryptionKeys() ([]*ColumnEncryptionKey, error) {
	return d.ColumnEncryptionKeysContext(context.Background())
}

// ColumnEncryptionKeysContext is the context-aware variant of ColumnEncryptionKeys.
func (d *Database) ColumnEncryptionKeysContext(ctx context.Context) ([]*ColumnEncryptionKey, error) {
	rows, err := d.query(ctx, columnEncryptionKeySelect+`
ORDER  BY cek.name, cekv.column_master_key_id`)
	if err != nil {
		return nil, fmt.Errorf("gosmo: list column encryption keys: %w", err)
	}
	defer rows.Close()

	keys, err := scanColumnEncryptionKeys(d, rows)
	if err != nil {
		return nil, fmt.Errorf("gosmo: list column encryption keys: %w", err)
	}
	return keys, nil
}

// ColumnEncryptionKeyByName returns one column encryption key by name.
func (d *Database) ColumnEncryptionKeyByName(name string) (*ColumnEncryptionKey, error) {
	return d.ColumnEncryptionKeyByNameContext(context.Background(), name)
}

// ColumnEncryptionKeyByNameContext is the context-aware variant of
// ColumnEncryptionKeyByName.
func (d *Database) ColumnEncryptionKeyByNameContext(ctx context.Context, name string) (*ColumnEncryptionKey, error) {
	// query, not queryRow: a key encrypted under two master keys is two rows
	// and both have to be read, so the not-found answer is an empty fold
	// rather than sql.ErrNoRows.
	rows, err := d.query(ctx, columnEncryptionKeySelect+`
WHERE  cek.name = @p1
ORDER  BY cekv.column_master_key_id`, name)
	if err != nil {
		return nil, fmt.Errorf("gosmo: find column encryption key %q in %q: %w", name, d.name, err)
	}
	defer rows.Close()

	keys, err := scanColumnEncryptionKeys(d, rows)
	if err != nil {
		return nil, fmt.Errorf("gosmo: find column encryption key %q in %q: %w", name, d.name, err)
	}
	if len(keys) == 0 {
		return nil, notFoundf("gosmo: column encryption key %q not found in %q", name, d.name)
	}
	return keys[0], nil
}

// scanColumnEncryptionKeys folds the one-row-per-encrypted-value result into
// one key per column_encryption_key_id. A key encrypted under two master
// keys — what a master-key rotation leaves behind — arrives as two rows of
// the same key, and CREATE COLUMN ENCRYPTION KEY has to restate both.
func scanColumnEncryptionKeys(d *Database, rows *dbRows) ([]*ColumnEncryptionKey, error) {
	var keys []*ColumnEncryptionKey
	byID := map[int]*ColumnEncryptionKey{}
	for rows.Next() {
		var name, masterKey string
		var id int
		var algo sql.NullString
		var encrypted []byte
		if err := rows.Scan(&name, &id, &masterKey, &algo, &encrypted); err != nil {
			return nil, err
		}
		k := byID[id]
		if k == nil {
			k = &ColumnEncryptionKey{db: d, Name: name, ID: id,
				MasterKeyName: masterKey, EncryptionAlgorithm: algo.String}
			byID[id] = k
			keys = append(keys, k)
		}
		k.Values = append(k.Values, &ColumnEncryptionKeyValue{
			MasterKeyName:       masterKey,
			EncryptionAlgorithm: algo.String,
			EncryptedValue:      encrypted,
		})
	}
	return keys, rows.Err()
}

// CreateColumnEncryptionKey creates a column encryption key from one or more
// already-encrypted values.
//
// Each value's key material is the CEK encrypted under a column master key,
// which is done client-side by whatever can reach that master key's private
// key (SSMS and the SqlColumnEncryptionKey PowerShell cmdlets both do) —
// nothing here can generate or verify it, and the server rejects a value it
// cannot decrypt on first use. Pass two values only to reproduce a key
// mid-rotation; one is the ordinary case.
func (d *Database) CreateColumnEncryptionKey(name string, values []ColumnEncryptionKeyValue) error {
	return d.CreateColumnEncryptionKeyContext(context.Background(), name, values)
}

// CreateColumnEncryptionKeyContext is the context-aware variant of
// CreateColumnEncryptionKey. The statement is written the way the scripter
// writes it (buildColumnEncryptionKeyScript), so a key created here and one
// scripted from the server read back the same.
func (d *Database) CreateColumnEncryptionKeyContext(ctx context.Context, name string, values []ColumnEncryptionKeyValue) error {
	if name == "" {
		return fmt.Errorf("gosmo: create column encryption key: name is required")
	}
	if len(values) == 0 {
		return fmt.Errorf("gosmo: create column encryption key [%s]: at least one encrypted value is required", name)
	}
	for i, v := range values {
		if v.MasterKeyName == "" {
			return fmt.Errorf("gosmo: create column encryption key [%s]: value %d has no column master key", name, i+1)
		}
		if v.EncryptionAlgorithm == "" {
			return fmt.Errorf("gosmo: create column encryption key [%s]: value %d has no encryption algorithm", name, i+1)
		}
		if len(v.EncryptedValue) == 0 {
			return fmt.Errorf("gosmo: create column encryption key [%s]: value %d has no encrypted value, which only the client holding the master key can produce", name, i+1)
		}
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "CREATE COLUMN ENCRYPTION KEY %s\nWITH VALUES", quoteIdent(name))
	for i, v := range values {
		if i > 0 {
			sb.WriteString(",")
		}
		fmt.Fprintf(&sb, "\n(\n    COLUMN_MASTER_KEY = %s,\n    ALGORITHM = '%s',\n    ENCRYPTED_VALUE = %s\n)",
			quoteIdent(v.MasterKeyName), escapeSingle(v.EncryptionAlgorithm), hexLiteral(v.EncryptedValue))
	}
	if _, err := d.exec(ctx, sb.String()); err != nil {
		return fmt.Errorf("gosmo: create column encryption key [%s]: %w", name, err)
	}
	return nil
}

// Drop drops the column encryption key.
func (cek *ColumnEncryptionKey) Drop() error {
	return cek.DropContext(context.Background())
}

// DropContext is the context-aware variant of Drop.
func (cek *ColumnEncryptionKey) DropContext(ctx context.Context) error {
	_, err := cek.db.exec(ctx,
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
	// IsSchemaBound reports whether the policy binds the schema of the
	// tables and predicate functions it names, which blocks any change to
	// them while it exists. Part of the CREATE statement, so scripting a
	// policy without it produces one that behaves differently.
	IsSchemaBound bool
	Predicates    []*SecurityPredicate
}

// SecurityPredicate represents one predicate in a security policy.
type SecurityPredicate struct {
	PredicateType       string // "FILTER" or "BLOCK"
	PredicateDefinition string
	TargetSchema        string
	TargetTable         string
	Operation           string // for BLOCK: AFTER INSERT, AFTER UPDATE, etc.
}

// securityPolicySelect is the column list every security policy read
// shares; the listing adds ORDER BY, the by-name lookup a WHERE.
const securityPolicySelect = `
SELECT sp.name, SCHEMA_NAME(sp.schema_id), sp.object_id,
       sp.is_enabled, sp.is_not_for_replication, sp.is_schema_bound
FROM   sys.security_policies sp`

// SecurityPolicies returns all security policies in the database.
func (d *Database) SecurityPolicies() ([]*SecurityPolicy, error) {
	return d.SecurityPoliciesContext(context.Background())
}

// SecurityPoliciesContext is the context-aware variant of SecurityPolicies.
func (d *Database) SecurityPoliciesContext(ctx context.Context) ([]*SecurityPolicy, error) {
	rows, err := d.query(ctx, securityPolicySelect+`
ORDER  BY sp.name`)
	if err != nil {
		return nil, fmt.Errorf("gosmo: list security policies: %w", err)
	}
	defer rows.Close()

	var policies []*SecurityPolicy
	for rows.Next() {
		p, err := scanSecurityPolicy(d, rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("gosmo: list security policies: %w", err)
		}
		policies = append(policies, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("gosmo: list security policies: %w", err)
	}
	// The predicates are loaded after the row scan, not inside it: each
	// policy needs its own query, and running one while the outer rows are
	// still open would hold two statements on the same connection.
	for _, p := range policies {
		if err := d.loadSecurityPredicates(ctx, p); err != nil {
			return nil, err
		}
	}
	return policies, nil
}

// SecurityPolicyByName returns one security policy by schema-qualified name.
func (d *Database) SecurityPolicyByName(schema, name string) (*SecurityPolicy, error) {
	return d.SecurityPolicyByNameContext(context.Background(), schema, name)
}

// SecurityPolicyByNameContext is the context-aware variant of
// SecurityPolicyByName.
func (d *Database) SecurityPolicyByNameContext(ctx context.Context, schema, name string) (*SecurityPolicy, error) {
	var p *SecurityPolicy
	err := d.queryRow(ctx, func(row *sql.Row) error {
		var err error
		p, err = scanSecurityPolicy(d, row.Scan)
		return err
	}, securityPolicySelect+`
WHERE  SCHEMA_NAME(sp.schema_id) = @p1
  AND  sp.name                   = @p2`, schema, name)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, notFoundf("gosmo: security policy [%s].[%s] not found in %q", schema, name, d.name)
		}
		return nil, fmt.Errorf("gosmo: find security policy [%s].[%s] in %q: %w", schema, name, d.name, err)
	}
	if err := d.loadSecurityPredicates(ctx, p); err != nil {
		return nil, err
	}
	return p, nil
}

func scanSecurityPolicy(d *Database, scan func(...any) error) (*SecurityPolicy, error) {
	p := &SecurityPolicy{db: d}
	if err := scan(&p.Name, &p.Schema, &p.ObjectID,
		&p.IsEnabled, &p.IsNotForReplication, &p.IsSchemaBound); err != nil {
		return nil, err
	}
	return p, nil
}

func (d *Database) loadSecurityPredicates(ctx context.Context, p *SecurityPolicy) error {
	preds, err := d.securityPredicates(ctx, p.ObjectID)
	if err != nil {
		return fmt.Errorf("gosmo: predicates of security policy %q in %q: %w", p.Name, d.name, err)
	}
	p.Predicates = preds
	return nil
}

func (d *Database) securityPredicates(ctx context.Context, policyObjectID int) ([]*SecurityPredicate, error) {
	const q = `
SELECT spr.predicate_type_desc, spr.predicate_definition,
       SCHEMA_NAME(t.schema_id), t.name, spr.operation_desc
FROM   sys.security_predicates spr
JOIN   sys.tables t ON t.object_id = spr.target_object_id
WHERE  spr.object_id = @p1
ORDER  BY spr.predicate_type_desc`

	rows, err := d.query(ctx, q, policyObjectID)
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
	return p.EnableContext(context.Background())
}

// EnableContext is the context-aware variant of Enable.
func (p *SecurityPolicy) EnableContext(ctx context.Context) error {
	_, err := p.db.exec(ctx,
		fmt.Sprintf("ALTER SECURITY POLICY %s WITH (STATE = ON)", qualifiedName(p.Schema, p.Name)))
	if err != nil {
		return fmt.Errorf("gosmo: enable security policy [%s]: %w", p.Name, err)
	}
	setIfApplied(ctx, &p.IsEnabled, true)
	return nil
}

// Disable disables the security policy.
func (p *SecurityPolicy) Disable() error {
	return p.DisableContext(context.Background())
}

// DisableContext is the context-aware variant of Disable.
func (p *SecurityPolicy) DisableContext(ctx context.Context) error {
	_, err := p.db.exec(ctx,
		fmt.Sprintf("ALTER SECURITY POLICY %s WITH (STATE = OFF)", qualifiedName(p.Schema, p.Name)))
	if err != nil {
		return fmt.Errorf("gosmo: disable security policy [%s]: %w", p.Name, err)
	}
	setIfApplied(ctx, &p.IsEnabled, false)
	return nil
}

// Drop drops the security policy. A policy that isn't there is the server's
// error, not a silent success — see the note on Database.DropTable.
func (p *SecurityPolicy) Drop() error {
	return p.DropContext(context.Background())
}

// DropContext is the context-aware variant of Drop.
func (p *SecurityPolicy) DropContext(ctx context.Context) error {
	_, err := p.db.exec(ctx,
		fmt.Sprintf("DROP SECURITY POLICY %s", qualifiedName(p.Schema, p.Name)))
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
			return nil, fmt.Errorf("gosmo: permissions for %s: %w", ref, err)
		}
		g.Permission = ObjectPermission(perm)
		g.State = PermissionState(state)
		grants = append(grants, g)
	}
	if err := rows.Err(); err != nil {
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
			return nil, fmt.Errorf("gosmo: permissions for principal %q in %q: %w", principal, d.name, err)
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
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("gosmo: permissions for principal %q in %q: %w", principal, d.name, err)
	}
	return entries, nil
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

// GrantPermission grants permission on schema.name to principal.
func (d *Database) GrantPermission(schema, name string, permission ObjectPermission, principal string) error {
	return d.GrantPermissionContext(context.Background(), schema, name, permission, principal)
}

// GrantPermissionContext is the context-aware variant of GrantPermission.
func (d *Database) GrantPermissionContext(ctx context.Context, schema, name string, permission ObjectPermission, principal string) error {
	return d.GrantPermissionWithOptionsContext(ctx, schema, name, permission, principal, PermissionOptions{})
}

// DenyPermission denies permission on schema.name to principal.
func (d *Database) DenyPermission(schema, name string, permission ObjectPermission, principal string) error {
	return d.DenyPermissionContext(context.Background(), schema, name, permission, principal)
}

// DenyPermissionContext is the context-aware variant of DenyPermission.
func (d *Database) DenyPermissionContext(ctx context.Context, schema, name string, permission ObjectPermission, principal string) error {
	return d.DenyPermissionWithOptionsContext(ctx, schema, name, permission, principal, PermissionOptions{})
}

// RevokePermission revokes permission on schema.name from principal.
func (d *Database) RevokePermission(schema, name string, permission ObjectPermission, principal string) error {
	return d.RevokePermissionContext(context.Background(), schema, name, permission, principal)
}

// RevokePermissionContext is the context-aware variant of RevokePermission.
func (d *Database) RevokePermissionContext(ctx context.Context, schema, name string, permission ObjectPermission, principal string) error {
	return d.RevokePermissionWithOptionsContext(ctx, schema, name, permission, principal, PermissionOptions{})
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
			return nil, fmt.Errorf("gosmo: schema permissions for %q in %q: %w", schemaName, d.name, err)
		}
		g.Permission = ObjectPermission(perm)
		g.State = PermissionState(state)
		grants = append(grants, g)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("gosmo: schema permissions for %q in %q: %w", schemaName, d.name, err)
	}
	return grants, nil
}

// GrantSchemaPermission grants permission on a schema to principal.
func (d *Database) GrantSchemaPermission(schemaName string, permission ObjectPermission, principal string) error {
	return d.GrantSchemaPermissionContext(context.Background(), schemaName, permission, principal)
}

// GrantSchemaPermissionContext is the context-aware variant of GrantSchemaPermission.
func (d *Database) GrantSchemaPermissionContext(ctx context.Context, schemaName string, permission ObjectPermission, principal string) error {
	return d.GrantSchemaPermissionWithOptionsContext(ctx, schemaName, permission, principal, PermissionOptions{})
}

// DenySchemaPermission denies permission on a schema to principal.
func (d *Database) DenySchemaPermission(schemaName string, permission ObjectPermission, principal string) error {
	return d.DenySchemaPermissionContext(context.Background(), schemaName, permission, principal)
}

// DenySchemaPermissionContext is the context-aware variant of DenySchemaPermission.
func (d *Database) DenySchemaPermissionContext(ctx context.Context, schemaName string, permission ObjectPermission, principal string) error {
	return d.DenySchemaPermissionWithOptionsContext(ctx, schemaName, permission, principal, PermissionOptions{})
}

// RevokeSchemaPermission revokes permission on a schema from principal.
func (d *Database) RevokeSchemaPermission(schemaName string, permission ObjectPermission, principal string) error {
	return d.RevokeSchemaPermissionContext(context.Background(), schemaName, permission, principal)
}

// RevokeSchemaPermissionContext is the context-aware variant of RevokeSchemaPermission.
func (d *Database) RevokeSchemaPermissionContext(ctx context.Context, schemaName string, permission ObjectPermission, principal string) error {
	return d.RevokeSchemaPermissionWithOptionsContext(ctx, schemaName, permission, principal, PermissionOptions{})
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
			return nil, fmt.Errorf("gosmo: database permissions in %q: %w", d.name, err)
		}
		perms = append(perms, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("gosmo: database permissions in %q: %w", d.name, err)
	}
	return perms, nil
}

// databasePermissionNames allowlists every database-scoped permission name
// SQL Server accepts in a GRANT/DENY/REVOKE ... statement — see
// serverPermissionNames (server_security.go) for why an allowlist rather
// than quoting. Deliberately excludes "ADMINISTER DATABASE BULK
// OPERATIONS": granting it fails with "The permission 'ADMINISTER DATABASE
// BULK OPERATIONS' is not supported in this version of SQL Server.
// Alternatively, use the server level 'ADMINISTER BULK OPERATIONS'
// permission." — which serverPermissionNames already has.
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
	return d.GrantDatabasePermissionWithOptionsContext(ctx, permission, principal, PermissionOptions{})
}

// DenyDatabasePermission denies a database-level permission to principal.
func (d *Database) DenyDatabasePermission(permission, principal string) error {
	return d.DenyDatabasePermissionContext(context.Background(), permission, principal)
}

// DenyDatabasePermissionContext is the context-aware variant of DenyDatabasePermission.
func (d *Database) DenyDatabasePermissionContext(ctx context.Context, permission, principal string) error {
	return d.DenyDatabasePermissionWithOptionsContext(ctx, permission, principal, PermissionOptions{})
}

// RevokeDatabasePermission revokes a database-level permission from principal.
func (d *Database) RevokeDatabasePermission(permission, principal string) error {
	return d.RevokeDatabasePermissionContext(context.Background(), permission, principal)
}

// RevokeDatabasePermissionContext is the context-aware variant of RevokeDatabasePermission.
func (d *Database) RevokeDatabasePermissionContext(ctx context.Context, permission, principal string) error {
	return d.RevokeDatabasePermissionWithOptionsContext(ctx, permission, principal, PermissionOptions{})
}
