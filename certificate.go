package gosmo

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// certificate.go covers certificates and the database master key that protects
// their private keys.
//
// # Moving a certificate between instances without files
//
// The documented way to give one instance another's certificate is BACKUP
// CERTIFICATE to a file, copy the file, then CREATE CERTIFICATE FROM FILE. That
// needs filesystem access on both hosts, which a client library does not have.
// Encoded and CreateCertificate's FromBinary are the pair that avoids it:
// CERTENCODED returns the ASN.1-encoded *public* certificate, and CREATE
// CERTIFICATE ... FROM BINARY takes it back. No private key crosses the wire,
// which is what makes this safe to do over an ordinary connection — and enough
// for database mirroring endpoints, where each instance keeps its own key pair
// and holds only the public certificate of its peers.
//
// FROM BINARY works on every supported version — probed on 2016, 2017 and
// 2025 and on Managed Instance, with and without WITH PRIVATE KEY (BINARY =
// ...). It fails with Msg 15232 only when the target database already holds
// a certificate with the same thumbprint, which is what importing into the
// database the certificate came from does.

// Certificate mirrors a row of sys.certificates.
type Certificate struct {
	db *Database

	Name          string
	CertificateID int
	PrincipalID   int

	// Subject is the certificate's subject as SQL Server decoded it.
	Subject string

	// PvtKeyEncryptionType is how the private key is protected —
	// "ENCRYPTED_BY_MASTER_KEY", "ENCRYPTED_BY_PASSWORD", or "NO_PRIVATE_KEY"
	// for one imported from a public certificate alone. An endpoint's own
	// certificate must be ENCRYPTED_BY_MASTER_KEY, because the private key has
	// to be openable without anyone typing a password.
	PvtKeyEncryptionType string

	StartDate  time.Time
	ExpiryDate time.Time

	Thumbprint []byte

	// Owner is the name of the database principal that owns the certificate
	// (principal_id resolved through sys.database_principals).
	Owner string

	// IssuerName and SerialNumber are the certificate's issuer and serial
	// number as SQL Server decoded them (issuer_name, cert_serial_number).
	IssuerName   string
	SerialNumber string

	// KeyLength is the key size in bits. It is read, never assumed: a
	// certificate SQL Server generates is 2048 bits on 2016 and 3072 on 2025.
	KeyLength int

	// IsActiveForBeginDialog is whether Service Broker may use the
	// certificate to initiate a dialog (ACTIVE FOR BEGIN_DIALOG).
	IsActiveForBeginDialog bool

	// PvtKeyLastBackupDate is when the private key was last exported with
	// BACKUP CERTIFICATE, or the zero time if it never was.
	PvtKeyLastBackupDate time.Time

	// AttestedBy is set only for a certificate SQL Server created for itself
	// from a signed file; empty otherwise.
	AttestedBy string
}

// Database returns the database the certificate belongs to.
func (c *Certificate) Database() *Database { return c.db }

// HasPrivateKey reports whether this instance holds the certificate's private
// key, i.e. can present it rather than only verify against it.
func (c *Certificate) HasPrivateKey() bool {
	return c.PvtKeyEncryptionType != "" && c.PvtKeyEncryptionType != "NO_PRIVATE_KEY"
}

// certificateSelect is aliased c, so a caller's predicate names c.name — the
// owner join brings a second name column into scope.
const certificateSelect = `
SELECT c.name, c.certificate_id, c.principal_id,
       ISNULL(c.subject, N''), ISNULL(c.pvt_key_encryption_type_desc, N''),
       c.start_date, c.expiry_date, ISNULL(c.thumbprint, 0x),
       ISNULL(p.name, N''), ISNULL(c.issuer_name, N''), ISNULL(c.cert_serial_number, N''),
       ISNULL(c.key_length, 0), c.is_active_for_begin_dialog,
       c.pvt_key_last_backup_date, ISNULL(c.attested_by, N'')
FROM   sys.certificates c
LEFT   JOIN sys.database_principals p ON p.principal_id = c.principal_id`

// Certificates returns the database's certificates, excluding the internal
// ones SQL Server creates for itself (named ##...##).
func (d *Database) Certificates(ctx context.Context) ([]*Certificate, error) {
	rows, err := d.query(ctx, certificateSelect+`
WHERE  c.name NOT LIKE '##%'
ORDER  BY c.name`)
	return scanRows(rows, err, fmt.Sprintf("list certificates in %q", d.Name), func(scan func(...any) error) (*Certificate, error) {
		return scanCertificate(d, scan)
	})
}

// CertificateByName returns one certificate, or an error wrapping
// ErrNotFound when the database has none by that name. Until 2026-09-22 it
// answered absence with (nil, nil); a caller about to create a certificate
// branches on errors.Is(err, ErrNotFound) instead.
func (d *Database) CertificateByName(ctx context.Context, name string) (*Certificate, error) {
	var c *Certificate
	err := d.queryRow(ctx, func(row *sql.Row) error {
		var err error
		c, err = scanCertificate(d, row.Scan)
		return err
	}, certificateSelect+`
WHERE  c.name = @p1`, name)
	return foundRow(c, err, notFoundf("gosmo: certificate %s not found in %q", quoteIdent(name), d.Name),
		fmt.Sprintf("read certificate %q in %q", name, d.Name))
}

func scanCertificate(d *Database, scan func(...any) error) (*Certificate, error) {
	c := &Certificate{db: d}
	var backup sql.NullTime
	if err := scan(&c.Name, &c.CertificateID, &c.PrincipalID, &c.Subject,
		&c.PvtKeyEncryptionType, &c.StartDate, &c.ExpiryDate, &c.Thumbprint,
		&c.Owner, &c.IssuerName, &c.SerialNumber, &c.KeyLength,
		&c.IsActiveForBeginDialog, &backup, &c.AttestedBy); err != nil {
		return nil, err
	}
	c.PvtKeyLastBackupDate = backup.Time
	return c, nil
}

// CertificateRef returns a lightweight handle for a certificate by name,
// without querying the catalog — the counterpart of Server.DatabaseRef.
// CertificateID, Subject and every other cached field stay at their zero
// value (HasPrivateKey answers false); CertificateByName is what populates
// them.
//
// Every write on *Certificate addresses it by name, so this handle is enough
// to drop one the caller already knows exists — and is the form to use when
// there is nothing to read yet, such as a New Certificate dialog scripting a
// certificate whose CREATE was only collected. Encoded does read the server,
// by name, and works from a handle to an existing certificate.
func (d *Database) CertificateRef(name string) *Certificate {
	return &Certificate{db: d, Name: name}
}

// Encoded returns the ASN.1-encoded public certificate — what
// CreateCertificate's FromBinary takes, and the whole of what one instance
// needs to give another to authenticate it. The private key is not included
// and cannot be obtained this way.
func (c *Certificate) Encoded(ctx context.Context) ([]byte, error) {
	var raw []byte
	err := c.db.queryRow(ctx, func(row *sql.Row) error {
		return row.Scan(&raw)
	}, "SELECT CERTENCODED(CERT_ID(@p1))", c.Name)
	if err != nil {
		return nil, fmt.Errorf("gosmo: encode certificate %q in %q: %w", c.Name, c.db.Name, err)
	}
	if len(raw) == 0 {
		return nil, fmt.Errorf("gosmo: encode certificate %q in %q: CERTENCODED returned nothing", c.Name, c.db.Name)
	}
	return raw, nil
}

// CreateCertificateRequest describes a certificate to create.
//
// Exactly one origin: either FromBinary, which imports an existing public
// certificate, or a Subject, which has SQL Server generate a new key pair.
type CreateCertificateRequest struct {
	Name string

	// Authorization is the database user that will own the certificate. Empty
	// leaves it owned by the caller. An imported peer certificate is normally
	// owned by a user created for it, so that CONNECT on the endpoint can be
	// granted to that user's login.
	Authorization string

	// Subject is the certificate subject for a newly generated certificate.
	Subject string

	// StartDate and ExpiryDate bound a newly generated certificate. Zero
	// values omit the clause, which SQL Server defaults to one year from now.
	StartDate  time.Time
	ExpiryDate time.Time

	// EncryptionPassword protects the new certificate's private key with a
	// password instead of the database master key. Leave it empty for an
	// endpoint certificate: the private key has to open without a password.
	EncryptionPassword string

	// FromBinary is an ASN.1-encoded public certificate, as returned by
	// Certificate.Encoded. Mutually exclusive with Subject.
	FromBinary []byte
}

// createCertificateStatement builds CREATE CERTIFICATE, validating the spec.
func (spec CreateCertificateRequest) createCertificateStatement() (string, error) {
	if strings.TrimSpace(spec.Name) == "" {
		return "", fmt.Errorf("certificate has no name")
	}
	if len(spec.FromBinary) > 0 && spec.Subject != "" {
		return "", fmt.Errorf("certificate %q gives both a subject and an encoded certificate to import", spec.Name)
	}
	if len(spec.FromBinary) == 0 && spec.Subject == "" {
		return "", fmt.Errorf("certificate %q has neither a subject nor an encoded certificate to import", spec.Name)
	}

	stmt := "CREATE CERTIFICATE " + quoteIdent(spec.Name)
	if spec.Authorization != "" {
		stmt += " AUTHORIZATION " + quoteIdent(spec.Authorization)
	}
	if len(spec.FromBinary) > 0 {
		if spec.EncryptionPassword != "" || !spec.StartDate.IsZero() || !spec.ExpiryDate.IsZero() {
			return "", fmt.Errorf("certificate %q is imported, so it takes no password or validity dates of its own", spec.Name)
		}
		return stmt + " FROM BINARY = " + binaryLiteral(spec.FromBinary), nil
	}

	if spec.EncryptionPassword != "" {
		stmt += " ENCRYPTION BY PASSWORD = " + QuoteLiteral(spec.EncryptionPassword)
	}
	stmt += " WITH SUBJECT = " + QuoteLiteral(spec.Subject)
	if !spec.StartDate.IsZero() {
		stmt += ", START_DATE = " + QuoteLiteral(spec.StartDate.Format("20060102"))
	}
	if !spec.ExpiryDate.IsZero() {
		stmt += ", EXPIRY_DATE = " + QuoteLiteral(spec.ExpiryDate.Format("20060102"))
	}
	return stmt, nil
}

// CreateCertificate creates a certificate in the database, and returns it
// read back from the catalog — or, under Scripting(ctx), the CertificateRef
// handle, since nothing ran.
func (d *Database) CreateCertificate(ctx context.Context, spec CreateCertificateRequest) (*Certificate, error) {
	stmt, err := spec.createCertificateStatement()
	if err != nil {
		return nil, fmt.Errorf("gosmo: create certificate in %q: %w", d.Name, err)
	}
	if _, err := d.exec(ctx, stmt); err != nil {
		return nil, fmt.Errorf("gosmo: create certificate %q in %q: %w", spec.Name, d.Name, err)
	}
	return createdObject(ctx, d.CertificateRef(spec.Name), func() (*Certificate, error) {
		return d.CertificateByName(ctx, spec.Name)
	})
}

// Drop deletes the certificate.
func (c *Certificate) Drop(ctx context.Context) error {
	if _, err := c.db.exec(ctx, "DROP CERTIFICATE "+quoteIdent(c.Name)); err != nil {
		return fmt.Errorf("gosmo: drop certificate %q in %q: %w", c.Name, c.db.Name, err)
	}
	return nil
}

// CertificateBackupSpec says where BACKUP CERTIFICATE writes, and whether
// the private key goes with it. Every path is on the *server's* filesystem,
// resolved by the SQL Server service account — not the caller's machine.
type CertificateBackupSpec struct {
	// File receives the public certificate; required.
	File string

	// PrivateKeyFile, when set, also exports the private key, encrypted by
	// EncryptionPassword (required with it). A certificate whose key is
	// protected by a password also needs DecryptionPassword; one protected
	// by the database master key does not.
	PrivateKeyFile     string
	EncryptionPassword string
	DecryptionPassword string
}

// backupStatement builds BACKUP CERTIFICATE, validating the spec.
func (c *Certificate) backupStatement(spec CertificateBackupSpec) (string, error) {
	if strings.TrimSpace(spec.File) == "" {
		return "", fmt.Errorf("no file to back up to")
	}
	stmt := "BACKUP CERTIFICATE " + quoteIdent(c.Name) + " TO FILE = " + QuoteLiteral(spec.File)
	if spec.PrivateKeyFile == "" {
		if spec.EncryptionPassword != "" || spec.DecryptionPassword != "" {
			return "", fmt.Errorf("a password was given but no private key file")
		}
		return stmt, nil
	}
	if spec.EncryptionPassword == "" {
		return "", fmt.Errorf("the private key file needs a password to encrypt it with")
	}
	stmt += " WITH PRIVATE KEY (FILE = " + QuoteLiteral(spec.PrivateKeyFile) +
		", ENCRYPTION BY PASSWORD = " + QuoteLiteral(spec.EncryptionPassword)
	if spec.DecryptionPassword != "" {
		stmt += ", DECRYPTION BY PASSWORD = " + QuoteLiteral(spec.DecryptionPassword)
	}
	return stmt + ")", nil
}

// Backup writes the certificate, and optionally its private key, to files on
// the server with BACKUP CERTIFICATE. The files it writes are readable only
// by its own service account. A private-key backup sets PvtKeyLastBackupDate; the
// receiver is not updated.
func (c *Certificate) Backup(ctx context.Context, spec CertificateBackupSpec) error {
	stmt, err := c.backupStatement(spec)
	if err != nil {
		return fmt.Errorf("gosmo: back up certificate %q in %q: %w", c.Name, c.db.Name, err)
	}
	if _, err := c.db.exec(ctx, stmt); err != nil {
		return fmt.Errorf("gosmo: back up certificate %q in %q: %w", c.Name, c.db.Name, err)
	}
	return nil
}

// RemovePrivateKey deletes the certificate's private key, leaving the public
// certificate — ALTER CERTIFICATE ... REMOVE PRIVATE KEY. It cannot be undone
// short of re-importing the key from a backup: nothing the certificate signed
// or encrypts can be signed or decrypted by it afterwards.
func (c *Certificate) RemovePrivateKey(ctx context.Context) error {
	if _, err := c.db.exec(ctx, "ALTER CERTIFICATE "+quoteIdent(c.Name)+" REMOVE PRIVATE KEY"); err != nil {
		return fmt.Errorf("gosmo: remove private key of certificate %q in %q: %w", c.Name, c.db.Name, err)
	}
	setIfApplied(ctx, &c.PvtKeyEncryptionType, "NO_PRIVATE_KEY")
	return nil
}

// SetOwner transfers the certificate to another database principal with
// ALTER AUTHORIZATION. SQL Server drops every explicit permission on the
// certificate as it does so (verified 2026-09-22 on 13 and 17).
func (c *Certificate) SetOwner(ctx context.Context, newOwner string) error {
	q := "ALTER AUTHORIZATION ON CERTIFICATE::" + quoteIdent(c.Name) + " TO " + quoteIdent(newOwner)
	if _, err := c.db.exec(ctx, q); err != nil {
		return fmt.Errorf("gosmo: change certificate %q owner to %q in %q: %w", c.Name, newOwner, c.db.Name, err)
	}
	setIfApplied(ctx, &c.Owner, newOwner)
	return nil
}

// -- The database master key -----------------------------------------------

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
		return nil, fmt.Errorf("gosmo: create master key in %q: empty password", d.Name)
	}
	if _, err := d.exec(ctx, "CREATE MASTER KEY ENCRYPTION BY PASSWORD = "+QuoteLiteral(req.Password)); err != nil {
		return nil, fmt.Errorf("gosmo: create master key in %q: %w", d.Name, err)
	}
	if Scripting(ctx) {
		return d.MasterKeyRef(), nil
	}
	m, err := d.MasterKey(ctx)
	if err != nil {
		return nil, err
	}
	if m == nil {
		// Created, but its row is not visible to this principal — see
		// MasterKey and createdObject. The handle still addresses it.
		return d.MasterKeyRef(), nil
	}
	return m, nil
}
