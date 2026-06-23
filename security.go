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
	db                *Database
	Name              string
	ID                int
	KeyStoreProviderName string
	KeyPath           string
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
	db               *Database
	Name             string
	ID               int
	MasterKeyName    string
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
	db          *Database
	Name        string
	Schema      string
	ObjectID    int
	IsEnabled   bool
	IsNotForReplication bool
	Predicates  []*SecurityPredicate
}

// SecurityPredicate represents one predicate in a security policy.
type SecurityPredicate struct {
	PredicateType  string // "FILTER" or "BLOCK"
	PredicateDefinition string
	TargetSchema   string
	TargetTable    string
	Operation      string // for BLOCK: AFTER INSERT, AFTER UPDATE, etc.
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
