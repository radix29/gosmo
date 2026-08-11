package gosmo

import (
	"context"
	"database/sql"
	"encoding/hex"
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
// FROM BINARY requires SQL Server 2022 or later. Against an older instance the
// file route is the only one, and CREATE CERTIFICATE fails with a syntax error.

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
}

// HasPrivateKey reports whether this instance holds the certificate's private
// key, i.e. can present it rather than only verify against it.
func (c *Certificate) HasPrivateKey() bool {
	return c.PvtKeyEncryptionType != "" && c.PvtKeyEncryptionType != "NO_PRIVATE_KEY"
}

const certificateSelect = `
SELECT name, certificate_id, principal_id,
       ISNULL(subject, ''), ISNULL(pvt_key_encryption_type_desc, ''),
       start_date, expiry_date, ISNULL(thumbprint, 0x)
FROM   sys.certificates`

// Certificates returns the database's certificates, excluding the internal
// ones SQL Server creates for itself (named ##...##).
func (d *Database) Certificates() ([]*Certificate, error) {
	return d.CertificatesContext(context.Background())
}

// CertificatesContext is the context-aware variant of Certificates.
func (d *Database) CertificatesContext(ctx context.Context) ([]*Certificate, error) {
	rows, err := d.query(ctx, certificateSelect+`
WHERE  name NOT LIKE '##%'
ORDER  BY name`)
	if err != nil {
		return nil, fmt.Errorf("gosmo: list certificates in %q: %w", d.name, err)
	}
	defer rows.Close()

	var out []*Certificate
	for rows.Next() {
		c, err := scanCertificate(d, rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("gosmo: list certificates in %q: %w", d.name, err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("gosmo: list certificates in %q: %w", d.name, err)
	}
	return out, nil
}

// CertificateByName returns one certificate, or (nil, nil) when the database
// has none by that name — an absent certificate is the ordinary case for a
// caller about to create one, not an error.
func (d *Database) CertificateByName(name string) (*Certificate, error) {
	return d.CertificateByNameContext(context.Background(), name)
}

// CertificateByNameContext is the context-aware variant of CertificateByName.
func (d *Database) CertificateByNameContext(ctx context.Context, name string) (*Certificate, error) {
	var c *Certificate
	err := d.queryRow(ctx, func(row *sql.Row) error {
		var err error
		c, err = scanCertificate(d, row.Scan)
		return err
	}, certificateSelect+`
WHERE  name = @p1`, name)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("gosmo: read certificate %q in %q: %w", name, d.name, err)
	}
	return c, nil
}

func scanCertificate(d *Database, scan func(...any) error) (*Certificate, error) {
	c := &Certificate{db: d}
	if err := scan(&c.Name, &c.CertificateID, &c.PrincipalID, &c.Subject,
		&c.PvtKeyEncryptionType, &c.StartDate, &c.ExpiryDate, &c.Thumbprint); err != nil {
		return nil, err
	}
	return c, nil
}

// Encoded returns the ASN.1-encoded public certificate — what
// CreateCertificate's FromBinary takes, and the whole of what one instance
// needs to give another to authenticate it. The private key is not included
// and cannot be obtained this way.
func (c *Certificate) Encoded() ([]byte, error) {
	return c.EncodedContext(context.Background())
}

// EncodedContext is the context-aware variant of Encoded.
func (c *Certificate) EncodedContext(ctx context.Context) ([]byte, error) {
	var raw []byte
	err := c.db.queryRow(ctx, func(row *sql.Row) error {
		return row.Scan(&raw)
	}, "SELECT CERTENCODED(CERT_ID(@p1))", c.Name)
	if err != nil {
		return nil, fmt.Errorf("gosmo: encode certificate %q in %q: %w", c.Name, c.db.name, err)
	}
	if len(raw) == 0 {
		return nil, fmt.Errorf("gosmo: encode certificate %q in %q: CERTENCODED returned nothing", c.Name, c.db.name)
	}
	return raw, nil
}

// CertificateSpec describes a certificate to create.
//
// Exactly one origin: either FromBinary, which imports an existing public
// certificate, or a Subject, which has SQL Server generate a new key pair.
type CertificateSpec struct {
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
func (spec CertificateSpec) createCertificateStatement() (string, error) {
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
		// Uppercase hex with the 0x prefix, which is the only literal form
		// T-SQL accepts for varbinary.
		return stmt + " FROM BINARY = 0x" + strings.ToUpper(hex.EncodeToString(spec.FromBinary)), nil
	}

	if spec.EncryptionPassword != "" {
		stmt += " ENCRYPTION BY PASSWORD = " + nStringLiteral(spec.EncryptionPassword)
	}
	stmt += " WITH SUBJECT = " + nStringLiteral(spec.Subject)
	if !spec.StartDate.IsZero() {
		stmt += ", START_DATE = " + nStringLiteral(spec.StartDate.Format("20060102"))
	}
	if !spec.ExpiryDate.IsZero() {
		stmt += ", EXPIRY_DATE = " + nStringLiteral(spec.ExpiryDate.Format("20060102"))
	}
	return stmt, nil
}

// CreateCertificate creates a certificate in the database.
func (d *Database) CreateCertificate(spec CertificateSpec) error {
	return d.CreateCertificateContext(context.Background(), spec)
}

// CreateCertificateContext is the context-aware variant of CreateCertificate.
func (d *Database) CreateCertificateContext(ctx context.Context, spec CertificateSpec) error {
	stmt, err := spec.createCertificateStatement()
	if err != nil {
		return fmt.Errorf("gosmo: create certificate in %q: %w", d.name, err)
	}
	if _, err := d.exec(ctx, stmt); err != nil {
		return fmt.Errorf("gosmo: create certificate %q in %q: %w", spec.Name, d.name, err)
	}
	return nil
}

// Drop deletes the certificate.
func (c *Certificate) Drop() error { return c.DropContext(context.Background()) }

// DropContext is the context-aware variant of Drop.
func (c *Certificate) DropContext(ctx context.Context) error {
	if _, err := c.db.exec(ctx, "DROP CERTIFICATE "+quoteIdent(c.Name)); err != nil {
		return fmt.Errorf("gosmo: drop certificate %q in %q: %w", c.Name, c.db.name, err)
	}
	return nil
}

// -- The database master key -----------------------------------------------

// HasMasterKey reports whether the database has a master key. A certificate
// whose private key is to open without a password needs one.
func (d *Database) HasMasterKey() (bool, error) {
	return d.HasMasterKeyContext(context.Background())
}

// HasMasterKeyContext is the context-aware variant of HasMasterKey.
func (d *Database) HasMasterKeyContext(ctx context.Context) (bool, error) {
	var n int
	err := d.queryRow(ctx, func(row *sql.Row) error {
		return row.Scan(&n)
	}, "SELECT COUNT(*) FROM sys.symmetric_keys WHERE name = '##MS_DatabaseMasterKey##'")
	if err != nil {
		return false, fmt.Errorf("gosmo: check for a master key in %q: %w", d.name, err)
	}
	return n > 0, nil
}

// CreateMasterKey creates the database master key, protected by password.
//
// The key is also encrypted by the service master key automatically, which is
// what lets SQL Server open it without the password at startup. Losing that —
// a restore onto another instance, or a service master key that no longer
// decrypts — leaves the password as the only way in, so it is worth keeping.
func (d *Database) CreateMasterKey(password string) error {
	return d.CreateMasterKeyContext(context.Background(), password)
}

// CreateMasterKeyContext is the context-aware variant of CreateMasterKey.
func (d *Database) CreateMasterKeyContext(ctx context.Context, password string) error {
	if password == "" {
		return fmt.Errorf("gosmo: create master key in %q: empty password", d.name)
	}
	if _, err := d.exec(ctx, "CREATE MASTER KEY ENCRYPTION BY PASSWORD = "+nStringLiteral(password)); err != nil {
		return fmt.Errorf("gosmo: create master key in %q: %w", d.name, err)
	}
	return nil
}
