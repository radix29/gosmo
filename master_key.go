package gosmo

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// master_key.go covers the database master key beyond its existence
// (HasMasterKey and CreateMasterKey, in certificate.go): reading it, and
// ALTER MASTER KEY's regenerate and encryption changes, BACKUP MASTER KEY and
// DROP MASTER KEY.
//
// Every one of these needs the master key open in the session. SQL Server
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

// MasterKey reads the database master key, or returns (nil, nil) when the
// database has none. Its sys.symmetric_keys row is visible only to a
// principal with a right on it: for one without, the answer is (nil, nil)
// though a key exists — HasMasterKey is the check for that case.
func (d *Database) MasterKey() (*MasterKey, error) {
	return d.MasterKeyContext(context.Background())
}

// MasterKeyContext is the context-aware variant of MasterKey.
func (d *Database) MasterKeyContext(ctx context.Context) (*MasterKey, error) {
	keys, err := d.symmetricKeys(ctx, `
WHERE  k.name = N'`+masterKeyName+`'`)
	if err != nil {
		return nil, fmt.Errorf("gosmo: read the master key in %q: %w", d.Name, err)
	}
	if len(keys) == 0 {
		return nil, nil
	}
	k := keys[0]
	m := &MasterKey{
		db: d, Algorithm: k.Algorithm, KeyLength: k.KeyLength, KeyGUID: k.KeyGUID,
		CreateDate: k.CreateDate, ModifyDate: k.ModifyDate, Encryptions: k.Encryptions,
	}
	err = d.queryRow(ctx, func(row *sql.Row) error {
		return row.Scan(&m.EncryptedByServer)
	}, `SELECT is_master_key_encrypted_by_server FROM sys.databases WHERE database_id = DB_ID()`)
	if err != nil {
		return nil, fmt.Errorf("gosmo: read the master key in %q: %w", d.Name, err)
	}
	return m, nil
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
		return "", fmt.Errorf("an encryption is by the service master key or by a password, not both")
	case e.ServiceMasterKey:
		return "SERVICE MASTER KEY", nil
	case e.Password != "":
		return "PASSWORD = " + nStringLiteral(e.Password), nil
	}
	return "", fmt.Errorf("no encryption named")
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
	b.WriteString("OPEN MASTER KEY DECRYPTION BY PASSWORD = " + nStringLiteral(openPassword) + ";\n")
	b.WriteString(stmt + ";\n")
	b.WriteString("CLOSE MASTER KEY;\n")
	b.WriteString("END TRY\nBEGIN CATCH\n")
	b.WriteString("IF EXISTS (SELECT 1 FROM sys.openkeys WHERE database_id = DB_ID() AND key_name = N'" + masterKeyName + "')\n")
	b.WriteString("    CLOSE MASTER KEY;\n")
	b.WriteString("THROW;\nEND CATCH;")
	return b.String()
}

// exec runs one master key statement, opened by openPassword when set.
func (m *MasterKey) exec(ctx context.Context, what, stmt, openPassword string) error {
	if _, err := m.db.exec(ctx, withOpen(stmt, openPassword)); err != nil {
		return fmt.Errorf("gosmo: %s the master key in %q: %w", what, m.db.Name, err)
	}
	return nil
}

// Regenerate replaces the key material with ALTER MASTER KEY REGENERATE,
// re-encrypting everything the key protects, and leaves it encrypted by
// password and — where it was before — the service master key. force is
// FORCE REGENERATE, which goes ahead even when something it protects cannot
// be decrypted, losing that thing; use it only to recover from a damaged
// key.
func (m *MasterKey) Regenerate(password string, force bool, openPassword string) error {
	return m.RegenerateContext(context.Background(), password, force, openPassword)
}

// RegenerateContext is the context-aware variant of Regenerate.
func (m *MasterKey) RegenerateContext(ctx context.Context, password string, force bool, openPassword string) error {
	if password == "" {
		return fmt.Errorf("gosmo: regenerate the master key in %q: empty password", m.db.Name)
	}
	stmt := "ALTER MASTER KEY "
	if force {
		stmt += "FORCE "
	}
	stmt += "REGENERATE WITH ENCRYPTION BY PASSWORD = " + nStringLiteral(password)
	return m.exec(ctx, "regenerate", stmt, openPassword)
}

// AddEncryption adds an encryption by the service master key or by a
// password. Adding the service master key's needs the key open, so a key not
// already encrypted by it needs openPassword (Msg 15581 without).
func (m *MasterKey) AddEncryption(enc MasterKeyEncryptor, openPassword string) error {
	return m.AddEncryptionContext(context.Background(), enc, openPassword)
}

// AddEncryptionContext is the context-aware variant of AddEncryption.
func (m *MasterKey) AddEncryptionContext(ctx context.Context, enc MasterKeyEncryptor, openPassword string) error {
	c, err := enc.clause()
	if err != nil {
		return fmt.Errorf("gosmo: add encryption to the master key in %q: %w", m.db.Name, err)
	}
	return m.exec(ctx, "add encryption to", "ALTER MASTER KEY ADD ENCRYPTION BY "+c, openPassword)
}

// DropEncryption removes an encryption. The server refuses to remove the
// last password (Msg 15558); dropping the service master key's leaves the
// key to be opened by password before every use.
func (m *MasterKey) DropEncryption(enc MasterKeyEncryptor, openPassword string) error {
	return m.DropEncryptionContext(context.Background(), enc, openPassword)
}

// DropEncryptionContext is the context-aware variant of DropEncryption.
func (m *MasterKey) DropEncryptionContext(ctx context.Context, enc MasterKeyEncryptor, openPassword string) error {
	c, err := enc.clause()
	if err != nil {
		return fmt.Errorf("gosmo: drop encryption from the master key in %q: %w", m.db.Name, err)
	}
	return m.exec(ctx, "drop encryption from", "ALTER MASTER KEY DROP ENCRYPTION BY "+c, openPassword)
}

// Backup exports the key to a file on the *server's* filesystem, encrypted by
// encryptionPassword — BACKUP MASTER KEY. The file is readable only by the
// SQL Server service account.
func (m *MasterKey) Backup(file, encryptionPassword, openPassword string) error {
	return m.BackupContext(context.Background(), file, encryptionPassword, openPassword)
}

// BackupContext is the context-aware variant of Backup.
func (m *MasterKey) BackupContext(ctx context.Context, file, encryptionPassword, openPassword string) error {
	if strings.TrimSpace(file) == "" {
		return fmt.Errorf("gosmo: back up the master key in %q: no file", m.db.Name)
	}
	if encryptionPassword == "" {
		return fmt.Errorf("gosmo: back up the master key in %q: empty password", m.db.Name)
	}
	stmt := "BACKUP MASTER KEY TO FILE = " + nStringLiteral(file) +
		" ENCRYPTION BY PASSWORD = " + nStringLiteral(encryptionPassword)
	return m.exec(ctx, "back up", stmt, openPassword)
}

// Drop deletes the master key. The server refuses while any certificate or
// key is encrypted by it (Msg 15580).
func (m *MasterKey) Drop() error { return m.DropContext(context.Background()) }

// DropContext is the context-aware variant of Drop.
func (m *MasterKey) DropContext(ctx context.Context) error {
	return m.exec(ctx, "drop", "DROP MASTER KEY", "")
}
