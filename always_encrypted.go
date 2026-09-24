package gosmo

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"
)

// always_encrypted.go is Always Encrypted's two key objects: the column master
// keys that live outside the database and the column encryption keys they
// protect, each with the values that reseat one onto another master key.
// Row-level security is in security_policy.go and the GRANT/DENY/REVOKE
// surface in security.go.

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

// Database returns the database the column master key belongs to.
func (cmk *ColumnMasterKey) Database() *Database { return cmk.db }

// columnMasterKeySelect is the column list every column master key read
// shares; the listing adds ORDER BY, the by-name lookup a WHERE.
//
// allow_enclave_computations and signature are the metadata of Always
// Encrypted with secure enclaves, which is "SQL Server 2019 (15.x) and later
// versions on Windows" — sys.column_master_keys has neither column before
// then, and naming one fails the whole read rather than the field.
// https://learn.microsoft.com/sql/relational-databases/security/encryption/always-encrypted-enclaves
func (d *Database) columnMasterKeySelect() string {
	major := d.serverMajorVersion()
	return `
SELECT name, column_master_key_id,
       key_store_provider_name, key_path,
       ` + colSince(major, SQLServer2019, "allow_enclave_computations", "CAST(0 AS bit)") + `,
       ` + colSince(major, SQLServer2019, "signature", "CAST(NULL AS varbinary(max))") + `
FROM   sys.column_master_keys`
}

// ColumnMasterKeys returns all column master keys in the database.
func (d *Database) ColumnMasterKeys(ctx context.Context) ([]*ColumnMasterKey, error) {
	rows, err := d.query(ctx, d.columnMasterKeySelect()+`
ORDER  BY name`)
	return scanRows(rows, err, "list column master keys", func(scan func(...any) error) (*ColumnMasterKey, error) {
		return scanColumnMasterKey(d, scan)
	})
}

// ColumnMasterKeyByName returns one column master key by name.
func (d *Database) ColumnMasterKeyByName(ctx context.Context, name string) (*ColumnMasterKey, error) {
	var k *ColumnMasterKey
	err := d.queryRow(ctx, func(row *sql.Row) error {
		var err error
		k, err = scanColumnMasterKey(d, row.Scan)
		return err
	}, d.columnMasterKeySelect()+`
WHERE  name = @p1`, name)
	return foundRow(k, err, notFoundf("gosmo: column master key %q not found in %q", name, d.Name), fmt.Sprintf("find column master key %q in %q", name, d.Name))
}

// ColumnMasterKeyRef returns a lightweight handle for a column master key by name, without
// querying the catalog — the counterpart of Server.DatabaseRef. Every field
// but the name stays at its zero value; ColumnMasterKeyByName is what populates them.
//
// Every write on *ColumnMasterKey addresses it by name, so this handle is enough to
// drop one the caller already knows exists — and is the form to use when
// there is nothing to read yet, such as a script of a CREATE that was only
// collected.
func (d *Database) ColumnMasterKeyRef(name string) *ColumnMasterKey {
	return &ColumnMasterKey{db: d, Name: name}
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

// CreateColumnMasterKeyRequest describes a column master key's metadata.
// The key itself must already exist in the key store.
type CreateColumnMasterKeyRequest struct {
	Name             string
	KeyStoreProvider string // KEY_STORE_PROVIDER_NAME, e.g. MSSQL_CERTIFICATE_STORE
	KeyPath          string
	// Signature, when set, makes the key allow enclave computations
	// (ENCLAVE_COMPUTATIONS (SIGNATURE = 0x...)). It is the digital signature
	// over the key's metadata — the same value ColumnMasterKey.Signature
	// reads back and the scripter writes out verbatim — and is produced
	// client-side by whatever holds the master key's private key (SSMS and
	// the SqlColumnMasterKey PowerShell cmdlets both do); the server verifies
	// it against the rest of the metadata, so a wrong one is rejected. There
	// is no boolean form of the clause, which is why this is the signature
	// and not a flag. Leave it nil for a key that does not allow enclave
	// computations.
	Signature []byte
}

// EnclaveComputationsSupported reports whether this instance understands
// CREATE COLUMN MASTER KEY's ENCLAVE_COMPUTATIONS clause, which SQL Server
// 2019 added. Below it the clause is not "ignored" — the parser rejects the
// whole statement with "Incorrect syntax near ','", so a caller offering an
// enclave option should hide it rather than let it fail on submit.
//
// An unread version (0) is treated as supported, the convention every version
// gate here follows.
func (d *Database) EnclaveComputationsSupported() bool {
	major := d.serverMajorVersion()
	return major == 0 || major >= int(SQLServer2019)
}

// CreateColumnMasterKey creates a column master key metadata entry, and
// returns it read back from the catalog — or, under Scripting(ctx), the
// ColumnMasterKeyRef handle, since nothing ran.
//
// A Signature needs SQL Server 2019 or later (the ENCLAVE_COMPUTATIONS
// clause); below that this refuses rather than sending a statement the
// parser rejects. Ask EnclaveComputationsSupported first.
func (d *Database) CreateColumnMasterKey(ctx context.Context, req CreateColumnMasterKeyRequest) (*ColumnMasterKey, error) {
	if req.Name == "" {
		return nil, fmt.Errorf("gosmo: create column master key: name is required")
	}
	enclave := ""
	if len(req.Signature) > 0 {
		if !d.EnclaveComputationsSupported() {
			return nil, unsupportedVersionf("gosmo: create column master key [%s]: enclave computations require SQL Server 2019 or later", req.Name)
		}
		enclave = fmt.Sprintf(",\n    ENCLAVE_COMPUTATIONS (SIGNATURE = %s)", binaryLiteral(req.Signature))
	}
	// Written the way the scripter writes it (buildColumnMasterKeyScript), so
	// a key created here and one scripted from the server read back the same.
	q := fmt.Sprintf(`
CREATE COLUMN MASTER KEY %s
WITH (
    KEY_STORE_PROVIDER_NAME = N'%s',
    KEY_PATH = N'%s'%s
)`, quoteIdent(req.Name), escapeSingle(req.KeyStoreProvider), escapeSingle(req.KeyPath), enclave)
	if _, err := d.exec(ctx, q); err != nil {
		return nil, fmt.Errorf("gosmo: create column master key [%s]: %w", req.Name, err)
	}
	return createdObject(ctx, d.ColumnMasterKeyRef(req.Name), func() (*ColumnMasterKey, error) {
		return d.ColumnMasterKeyByName(ctx, req.Name)
	})
}

// Drop drops the column master key.
func (cmk *ColumnMasterKey) Drop(ctx context.Context) error {
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
	// MasterKeyName and EncryptionAlgorithm mirror Values[0] — the key's first
	// encrypted value, which is the whole of it in the common case where a key
	// has exactly one. AddValue and DropValue re-seat them, so a caller
	// rendering a summary from the handle it already holds never names a
	// master key the rotation has dropped. Both are empty when Values is.
	MasterKeyName       string
	EncryptionAlgorithm string
	// Values holds every encrypted value of the key, one per column master
	// key it is encrypted under. A key has two while its master key is being
	// rotated, and CREATE COLUMN ENCRYPTION KEY has to restate all of them.
	Values []*ColumnEncryptionKeyValue
}

// Database returns the database the column encryption key belongs to.
func (cek *ColumnEncryptionKey) Database() *Database { return cek.db }

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

// missing names the first required part this value has not been given, or
// "" when it is complete. The parts are the same for CREATE and for ALTER ...
// ADD VALUE, and the server's own error for an omitted one is a syntax error
// pointing at the closing paren.
func (v ColumnEncryptionKeyValue) missing() string {
	switch {
	case v.MasterKeyName == "":
		return "no column master key"
	case v.EncryptionAlgorithm == "":
		return "no encryption algorithm"
	case len(v.EncryptedValue) == 0:
		return "no encrypted value, which only the client holding the master key can produce"
	}
	return ""
}

// valueClause renders the parenthesised value block CREATE COLUMN ENCRYPTION
// KEY and ALTER ... ADD VALUE both take.
func (v ColumnEncryptionKeyValue) valueClause() string {
	return fmt.Sprintf("(\n    COLUMN_MASTER_KEY = %s,\n    ALGORITHM = '%s',\n    ENCRYPTED_VALUE = %s\n)",
		quoteIdent(v.MasterKeyName), escapeSingle(v.EncryptionAlgorithm), binaryLiteral(v.EncryptedValue))
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
func (d *Database) ColumnEncryptionKeys(ctx context.Context) ([]*ColumnEncryptionKey, error) {
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
func (d *Database) ColumnEncryptionKeyByName(ctx context.Context, name string) (*ColumnEncryptionKey, error) {
	// query, not queryRow: a key encrypted under two master keys is two rows
	// and both have to be read, so the not-found answer is an empty fold
	// rather than sql.ErrNoRows.
	rows, err := d.query(ctx, columnEncryptionKeySelect+`
WHERE  cek.name = @p1
ORDER  BY cekv.column_master_key_id`, name)
	if err != nil {
		return nil, fmt.Errorf("gosmo: find column encryption key %q in %q: %w", name, d.Name, err)
	}
	defer rows.Close()

	keys, err := scanColumnEncryptionKeys(d, rows)
	if err != nil {
		return nil, fmt.Errorf("gosmo: find column encryption key %q in %q: %w", name, d.Name, err)
	}
	if len(keys) == 0 {
		return nil, notFoundf("gosmo: column encryption key %q not found in %q", name, d.Name)
	}
	return keys[0], nil
}

// ColumnEncryptionKeyRef returns a lightweight handle for a column encryption key by name, without
// querying the catalog — the counterpart of Server.DatabaseRef. Every field
// but the name stays at its zero value; ColumnEncryptionKeyByName is what populates them.
//
// Every write on *ColumnEncryptionKey addresses it by name, so this handle is enough to
// drop one the caller already knows exists — and is the form to use when
// there is nothing to read yet, such as a script of a CREATE that was only
// collected.
func (d *Database) ColumnEncryptionKeyRef(name string) *ColumnEncryptionKey {
	return &ColumnEncryptionKey{db: d, Name: name}
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

// CreateColumnEncryptionKeyRequest describes a column encryption key.
type CreateColumnEncryptionKeyRequest struct {
	Name   string
	Values []ColumnEncryptionKeyValue
}

// CreateColumnEncryptionKey creates a column encryption key from one or more
// already-encrypted values, and returns it read back from the catalog — or,
// under Scripting(ctx), the ColumnEncryptionKeyRef handle, since nothing ran.
//
// Each value's key material is the CEK encrypted under a column master key,
// which is done client-side by whatever can reach that master key's private
// key (SSMS and the SqlColumnEncryptionKey PowerShell cmdlets both do) —
// nothing here can generate or verify it, and the server rejects a value it
// cannot decrypt on first use. Pass two values only to reproduce a key
// mid-rotation; one is the ordinary case.
//
// The statement is written the way the scripter writes it
// (buildColumnEncryptionKeyScript), so a key created here and one scripted
// from the server read back the same.
func (d *Database) CreateColumnEncryptionKey(ctx context.Context, req CreateColumnEncryptionKeyRequest) (*ColumnEncryptionKey, error) {
	name, values := req.Name, req.Values
	if name == "" {
		return nil, fmt.Errorf("gosmo: create column encryption key: name is required")
	}
	if len(values) == 0 {
		return nil, fmt.Errorf("gosmo: create column encryption key [%s]: at least one encrypted value is required", name)
	}
	for i, v := range values {
		if missing := v.missing(); missing != "" {
			return nil, fmt.Errorf("gosmo: create column encryption key [%s]: value %d has %s", name, i+1, missing)
		}
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "CREATE COLUMN ENCRYPTION KEY %s\nWITH VALUES", quoteIdent(name))
	for i, v := range values {
		if i > 0 {
			sb.WriteString(",")
		}
		fmt.Fprintf(&sb, "\n%s", v.valueClause())
	}
	if _, err := d.exec(ctx, sb.String()); err != nil {
		return nil, fmt.Errorf("gosmo: create column encryption key [%s]: %w", name, err)
	}
	return createdObject(ctx, d.ColumnEncryptionKeyRef(name), func() (*ColumnEncryptionKey, error) {
		return d.ColumnEncryptionKeyByName(ctx, name)
	})
}

// AddValue encrypts the key under one more column master key, the first half
// of a master-key rotation: both values coexist so clients holding either
// master key can still decrypt, and the old one is dropped with DropValue
// once every client has the new master key.
//
// As with CreateColumnEncryptionKey, the encrypted value is produced
// client-side by something that can reach the new master key — nothing here
// can generate it, and the server stores it without checking it.
func (cek *ColumnEncryptionKey) AddValue(ctx context.Context, value ColumnEncryptionKeyValue) error {
	if missing := value.missing(); missing != "" {
		return fmt.Errorf("gosmo: add value to column encryption key [%s]: the value has %s", cek.Name, missing)
	}
	stmt := fmt.Sprintf("ALTER COLUMN ENCRYPTION KEY %s\nADD VALUE\n%s",
		quoteIdent(cek.Name), value.valueClause())
	if _, err := cek.db.exec(ctx, stmt); err != nil {
		return fmt.Errorf("gosmo: add value to column encryption key [%s]: %w", cek.Name, err)
	}
	// Not mirrored under WithScript — nothing reached the server, so the
	// handle must keep describing what is actually there. See setIfApplied,
	// which is the single-field form of this guard.
	if !Scripting(ctx) {
		cek.Values = append(cek.Values, &value)
		cek.reseatSummary()
	}
	return nil
}

// DropValue removes the value encrypted under one column master key — the
// second half of a rotation.
//
// The data encrypted with this key becomes unreadable to any client that can
// reach only the dropped master key, so the new value must be in place and
// distributed first. DROP VALUE names the master key alone; the ciphertext is
// not restated.
func (cek *ColumnEncryptionKey) DropValue(ctx context.Context, masterKeyName string) error {
	if masterKeyName == "" {
		return fmt.Errorf("gosmo: drop value from column encryption key [%s]: the column master key name is required", cek.Name)
	}
	stmt := fmt.Sprintf("ALTER COLUMN ENCRYPTION KEY %s\nDROP VALUE\n(\n    COLUMN_MASTER_KEY = %s\n)",
		quoteIdent(cek.Name), quoteIdent(masterKeyName))
	if _, err := cek.db.exec(ctx, stmt); err != nil {
		return fmt.Errorf("gosmo: drop value from column encryption key [%s]: %w", cek.Name, err)
	}
	// Not mirrored under WithScript, as in AddValue above.
	if !Scripting(ctx) {
		cek.Values = slices.DeleteFunc(cek.Values, func(v *ColumnEncryptionKeyValue) bool {
			return strings.EqualFold(v.MasterKeyName, masterKeyName)
		})
		cek.reseatSummary()
	}
	return nil
}

// reseatSummary points MasterKeyName and EncryptionAlgorithm back at Values[0]
// after the slice has changed. Dropping the first value otherwise leaves the
// key naming the master key that no longer encrypts it.
func (cek *ColumnEncryptionKey) reseatSummary() {
	if len(cek.Values) == 0 {
		cek.MasterKeyName, cek.EncryptionAlgorithm = "", ""
		return
	}
	cek.MasterKeyName = cek.Values[0].MasterKeyName
	cek.EncryptionAlgorithm = cek.Values[0].EncryptionAlgorithm
}

// Drop drops the column encryption key.
func (cek *ColumnEncryptionKey) Drop(ctx context.Context) error {
	_, err := cek.db.exec(ctx,
		fmt.Sprintf("DROP COLUMN ENCRYPTION KEY %s", quoteIdent(cek.Name)))
	if err != nil {
		return fmt.Errorf("gosmo: drop column encryption key [%s]: %w", cek.Name, err)
	}
	return nil
}
