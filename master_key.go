package gosmo

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// master_key.go covers the database master key: whether it exists
// (HasMasterKey), creating it (CreateMasterKey, EnsureMasterKey), reading it,
// and ALTER MASTER KEY's regenerate and encryption changes, BACKUP MASTER KEY
// and DROP MASTER KEY.
//
// Every write after the CREATE needs the master key open in the session. SQL Server
// opens it on its own when the service master key encrypts it — the default —
// and otherwise refuses with Msg 15581; OPEN MASTER KEY DECRYPTION BY PASSWORD
// is the way in then. Each write here takes that password as openPassword,
// and wraps the statement in the OPEN and CLOSE as one batch, since gosmo
// hands each exec its own pooled connection and an open key belongs to the
// session. Probed 2026-09-22 on 13 and 17, identical.

// masterKeyName is the database master key's row in sys.symmetric_keys.
const masterKeyName = "##MS_DatabaseMasterKey##"

// MasterKey mirrors the database master key's row of sys.symmetric_keys,
// with its encryptions.
type MasterKey struct {
	db *Database

	Algorithm  string
	KeyLength  int
	KeyGUID    string
	CreateDate time.Time
	ModifyDate time.Time

	// EncryptedByServer is sys.databases.is_master_key_encrypted_by_server:
	// the service master key encrypts it, so SQL Server opens it unasked.
	EncryptedByServer bool

	// Encryptions lists what protects the key, as sys.key_encryptions
	// records it: SymmetricKeyByMasterKey is the service master key's
	// encryption, SymmetricKeyByPassword each password's.
	Encryptions []SymmetricKeyEncryption
}

// Database returns the database the master key belongs to.
func (m *MasterKey) Database() *Database { return m.db }

// MasterKey reads the database master key, or returns an error wrapping
// ErrNotFound when the database has none. Its sys.symmetric_keys row is
// visible only to a principal with a right on it: for one without, the answer
// is ErrNotFound though a key exists — HasMasterKey is the check for that
// case.
func (d *Database) MasterKey(ctx context.Context) (*MasterKey, error) {
	var byServer bool
	keys, err := d.readSymmetricKeys(ctx, `,
       (SELECT is_master_key_encrypted_by_server FROM sys.databases WHERE database_id = DB_ID())`, &byServer, `
WHERE  k.name = N'`+masterKeyName+`'`)
	if err != nil {
		return nil, fmt.Errorf("gosmo: read the master key in %q: %w", d.Name, err)
	}
	if len(keys) == 0 {
		return nil, notFoundf("gosmo: database %q has no master key this principal can see", d.Name)
	}
	k := keys[0]
	return &MasterKey{
		db: d, Algorithm: k.Algorithm, KeyLength: k.KeyLength, KeyGUID: k.KeyGUID,
		CreateDate: k.CreateDate, ModifyDate: k.ModifyDate, Encryptions: k.Encryptions,
		EncryptedByServer: byServer,
	}, nil
}

// MasterKeyRef returns a lightweight handle for the database master key,
// without querying the catalog — every field at its zero value. It is the
// form for a write that has nothing to read first, such as a scripted
// change; MasterKey is what populates the fields.
func (d *Database) MasterKeyRef() *MasterKey {
	return &MasterKey{db: d}
}

// MasterKeyEncryptor is one thing ALTER MASTER KEY ... ADD or DROP ENCRYPTION
// BY names: the service master key, or a password.
type MasterKeyEncryptor struct {
	// ServiceMasterKey names the service master key's encryption. Password
	// must then be empty.
	ServiceMasterKey bool

	// Password names a password encryption. DROP needs it too: the server
	// finds the encryption by value.
	Password string
}

func (e MasterKeyEncryptor) clause() (string, error) {
	switch {
	case e.ServiceMasterKey && e.Password != "":
		return "", invalidf("an encryption is by the service master key or by a password, not both")
	case e.ServiceMasterKey:
		return "SERVICE MASTER KEY", nil
	case e.Password != "":
		return "PASSWORD = " + QuoteLiteral(e.Password), nil
	}
	return "", invalidf("no encryption named")
}

// withOpen wraps stmt in OPEN MASTER KEY / CLOSE MASTER KEY when
// openPassword is set, as one batch. The CATCH closes the key only if it
// was opened, then re-raises: a failed OPEN leaves it closed, and CLOSE of a
// key that is not open would replace the error THROW is there to report.
func withOpen(stmt, openPassword string) string {
	if openPassword == "" {
		return stmt
	}
	var b strings.Builder
	b.WriteString("BEGIN TRY\n")
	b.WriteString("OPEN MASTER KEY DECRYPTION BY PASSWORD = ")
	b.WriteString(QuoteLiteral(openPassword))
	b.WriteString(";\n")
	b.WriteString(stmt)
	b.WriteString(";\n")
	b.WriteString("CLOSE MASTER KEY;\n")
	b.WriteString("END TRY\nBEGIN CATCH\n")
	b.WriteString("IF EXISTS (SELECT 1 FROM sys.openkeys WHERE database_id = DB_ID() AND key_name = N'" + masterKeyName + "')\n")
	b.WriteString("    CLOSE MASTER KEY;\n")
	b.WriteString("THROW;\nEND CATCH;")
	return b.String()
}

// exec runs one master key statement, opened by openPassword when set.
// passwords are the ones stmt itself carries, redacted with openPassword from
// what is captured or observed.
func (m *MasterKey) exec(ctx context.Context, what, stmt, openPassword string, passwords ...string) error {
	if _, err := m.db.execPasswords(ctx, withOpen(stmt, openPassword), append(passwords, openPassword)...); err != nil {
		return fmt.Errorf("gosmo: %s the master key in %q: %w", what, m.db.Name, err)
	}
	return nil
}

// MasterKeyRegenerateMode says whether MasterKey.Regenerate goes ahead
// when something the key protects cannot be decrypted.
type MasterKeyRegenerateMode int

const (
	// MasterKeyRegenerate is ALTER MASTER KEY REGENERATE, which fails if
	// anything the key protects cannot be decrypted.
	MasterKeyRegenerate MasterKeyRegenerateMode = iota

	// MasterKeyForceRegenerate is FORCE REGENERATE, which goes ahead even
	// then, losing whatever could not be decrypted; use it only to recover
	// from a damaged key.
	MasterKeyForceRegenerate
)

// Regenerate replaces the key material with ALTER MASTER KEY [FORCE]
// REGENERATE, as mode says, re-encrypting everything the key protects, and
// leaves it encrypted by password and — where it was before — the service
// master key.
func (m *MasterKey) Regenerate(ctx context.Context, password string, mode MasterKeyRegenerateMode, openPassword string) error {
	if password == "" {
		return invalidf("gosmo: regenerate the master key in %q: empty password", m.db.Name)
	}
	stmt := "ALTER MASTER KEY "
	switch mode {
	case MasterKeyRegenerate:
	case MasterKeyForceRegenerate:
		stmt += "FORCE "
	default:
		return invalidf("gosmo: regenerate the master key in %q: unrecognized mode %d", m.db.Name, mode)
	}
	stmt += "REGENERATE WITH ENCRYPTION BY PASSWORD = " + QuoteLiteral(password)
	return m.exec(ctx, "regenerate", stmt, openPassword, password)
}

// AddEncryption adds an encryption by the service master key or by a
// password. Adding the service master key's needs the key open, so a key not
// already encrypted by it needs openPassword (Msg 15581 without).
func (m *MasterKey) AddEncryption(ctx context.Context, enc MasterKeyEncryptor, openPassword string) error {
	c, err := enc.clause()
	if err != nil {
		return fmt.Errorf("gosmo: add encryption to the master key in %q: %w", m.db.Name, err)
	}
	return m.exec(ctx, "add encryption to", "ALTER MASTER KEY ADD ENCRYPTION BY "+c, openPassword, enc.Password)
}

// DropEncryption removes an encryption. The server refuses to remove the
// last password (Msg 15558); dropping the service master key's leaves the
// key to be opened by password before every use.
func (m *MasterKey) DropEncryption(ctx context.Context, enc MasterKeyEncryptor, openPassword string) error {
	c, err := enc.clause()
	if err != nil {
		return fmt.Errorf("gosmo: drop encryption from the master key in %q: %w", m.db.Name, err)
	}
	return m.exec(ctx, "drop encryption from", "ALTER MASTER KEY DROP ENCRYPTION BY "+c, openPassword, enc.Password)
}

// Backup exports the key to a file on the *server's* filesystem, encrypted by
// encryptionPassword — BACKUP MASTER KEY. The file is readable only by the
// SQL Server service account.
func (m *MasterKey) Backup(ctx context.Context, file, encryptionPassword, openPassword string) error {
	if strings.TrimSpace(file) == "" {
		return invalidf("gosmo: back up the master key in %q: no file", m.db.Name)
	}
	if encryptionPassword == "" {
		return invalidf("gosmo: back up the master key in %q: empty password", m.db.Name)
	}
	stmt := "BACKUP MASTER KEY TO FILE = " + QuoteLiteral(file) +
		" ENCRYPTION BY PASSWORD = " + QuoteLiteral(encryptionPassword)
	return m.exec(ctx, "back up", stmt, openPassword, encryptionPassword)
}

// Drop deletes the master key. The server refuses while any certificate or
// key is encrypted by it (Msg 15580).
func (m *MasterKey) Drop(ctx context.Context) error {
	return m.exec(ctx, "drop", "DROP MASTER KEY", "")
}

// -- Existence and creation -------------------------------------------------

// HasMasterKey reports whether the database has a master key. A certificate
// whose private key is to open without a password needs one.
//
// sys.symmetric_keys alone is not enough: it shows a principal only the keys
// it holds a right on, so for a user with CREATE CERTIFICATE and nothing else
// the master key's row is absent and the answer would be a confident false —
// and CREATE MASTER KEY then fails Msg 15247, not "already exists". The
// database's is_master_key_encrypted_by_server flag, in sys.databases and
// visible to anyone who can see the database, answers for the usual key,
// which SQL Server encrypts by the service master key on creation. What stays
// invisible to such a principal is a master key whose service-master-key
// encryption was dropped; that one still reads false.
func (d *Database) HasMasterKey(ctx context.Context) (bool, error) {
	var n int
	err := d.queryRow(ctx, func(row *sql.Row) error {
		return row.Scan(&n)
	}, `SELECT CASE WHEN EXISTS (SELECT 1 FROM sys.symmetric_keys WHERE name = '##MS_DatabaseMasterKey##')
	                  OR EXISTS (SELECT 1 FROM sys.databases WHERE database_id = DB_ID() AND is_master_key_encrypted_by_server = 1)
	             THEN 1 ELSE 0 END`)
	if err != nil {
		return false, fmt.Errorf("gosmo: check for a master key in %q: %w", d.Name, err)
	}
	return n > 0, nil
}

// CreateMasterKeyRequest describes a database master key.
type CreateMasterKeyRequest struct {
	// Password protects the key (ENCRYPTION BY PASSWORD); required.
	Password string
}

// CreateMasterKey creates the database master key, protected by a password.
//
// The key is also encrypted by the service master key automatically, which is
// what lets SQL Server open it without the password at startup. Losing that —
// a restore onto another instance, or a service master key that no longer
// decrypts — leaves the password as the only way in, so it is worth keeping.
//
// It returns the key read back — or, under Scripting(ctx), the MasterKeyRef
// handle, since nothing ran.
func (d *Database) CreateMasterKey(ctx context.Context, req CreateMasterKeyRequest) (*MasterKey, error) {
	if req.Password == "" {
		return nil, invalidf("gosmo: create master key in %q: empty password", d.Name)
	}
	if _, err := d.execPasswords(ctx, "CREATE MASTER KEY ENCRYPTION BY PASSWORD = "+QuoteLiteral(req.Password), req.Password); err != nil {
		return nil, fmt.Errorf("gosmo: create master key in %q: %w", d.Name, err)
	}
	if Scripting(ctx) {
		return d.MasterKeyRef(), nil
	}
	m, err := d.MasterKey(ctx)
	if errors.Is(err, ErrNotFound) {
		// Created, but its row is not visible to this principal — see
		// MasterKey and createdObject. The handle still addresses it.
		return d.MasterKeyRef(), nil
	}
	return m, err
}

// EnsureMasterKey creates the database master key, protected by password,
// when the database has none, and reports whether it did; an existing key is
// left alone and password is unused. It is the precondition for a certificate
// or asymmetric key whose private key is protected by the master key — SQL
// Server refuses one in a database without (Msg 15581).
//
// The check is HasMasterKey, a read, so under Scripting(ctx) it still asks
// the real server and only the CREATE is captured: a script for a database
// that already has a master key does not try to make a second, and created
// then means "the script creates it".
func (d *Database) EnsureMasterKey(ctx context.Context, password string) (created bool, err error) {
	has, err := d.HasMasterKey(ctx)
	if err != nil {
		return false, err
	}
	if has {
		return false, nil
	}
	if _, err := d.CreateMasterKey(ctx, CreateMasterKeyRequest{Password: password}); err != nil {
		return false, err
	}
	return true, nil
}
