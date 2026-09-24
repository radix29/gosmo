package gosmo

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// asymmetric_key.go covers asymmetric keys, the asymmetric-key counterpart of
// certificate.go. CreateAsymmetricKey generates a new key pair only: unlike
// CREATE CERTIFICATE ... FROM BINARY there is no way to move an existing key
// between instances over a connection — CREATE ASYMMETRIC KEY imports only
// FROM FILE, EXECUTABLE FILE, ASSEMBLY or PROVIDER, the first three of which
// read the server's own filesystem and the last of which needs an EKM
// provider set up on the instance.

// AsymmetricKey mirrors a row of sys.asymmetric_keys.
type AsymmetricKey struct {
	db *Database

	Name        string
	KeyID       int
	PrincipalID int

	// Algorithm is the key algorithm's description — "RSA_2048" and the like.
	Algorithm string

	// KeyLength is the key length in bits.
	KeyLength int

	// PvtKeyEncryptionType is how the private key is protected —
	// "ENCRYPTED_BY_MASTER_KEY", "ENCRYPTED_BY_PASSWORD", or "NO_PRIVATE_KEY"
	// for a key imported from its public half alone.
	PvtKeyEncryptionType string

	// Thumbprint is the SHA-1 hash of the public key.
	Thumbprint []byte

	// Owner is the name of the database principal that owns the key
	// (principal_id resolved through sys.database_principals).
	Owner string

	// ProviderType is "CRYPTOGRAPHIC PROVIDER" for a key held by an EKM
	// provider, empty for one SQL Server holds itself.
	ProviderType string

	// AttestedBy is set only for a key SQL Server created for itself from a
	// signed file; empty otherwise.
	AttestedBy string
}

// Database returns the database the asymmetric key belongs to.
func (k *AsymmetricKey) Database() *Database { return k.db }

// HasPrivateKey reports whether this instance holds the key's private half,
// i.e. can sign with it rather than only verify against it.
func (k *AsymmetricKey) HasPrivateKey() bool {
	return k.PvtKeyEncryptionType != "" && k.PvtKeyEncryptionType != "NO_PRIVATE_KEY"
}

// asymmetricKeySelect is aliased k, so a caller's predicate names k.name — the
// owner join brings a second name column into scope. sys.asymmetric_keys has
// no pvt_key_last_backup_date and no create_date on any supported version.
const asymmetricKeySelect = `
SELECT k.name, k.asymmetric_key_id, k.principal_id,
       ISNULL(k.algorithm_desc, N''), ISNULL(k.key_length, 0),
       ISNULL(k.pvt_key_encryption_type_desc, N''), ISNULL(k.thumbprint, 0x),
       ISNULL(p.name, N''), ISNULL(k.provider_type, N''), ISNULL(k.attested_by, N'')
FROM   sys.asymmetric_keys k
LEFT   JOIN sys.database_principals p ON p.principal_id = k.principal_id`

// AsymmetricKeys returns the database's asymmetric keys, excluding the
// internal ones SQL Server creates for itself (named ##...##) — the same
// exclusion Certificates makes.
func (d *Database) AsymmetricKeys(ctx context.Context) ([]*AsymmetricKey, error) {
	rows, err := d.query(ctx, asymmetricKeySelect+`
WHERE  k.name NOT LIKE '##%'
ORDER  BY k.name`)
	return scanRows(rows, err, fmt.Sprintf("list asymmetric keys in %q", d.Name), func(scan func(...any) error) (*AsymmetricKey, error) {
		return scanAsymmetricKey(d, scan)
	})
}

// AsymmetricKeyByName returns one asymmetric key, or an error wrapping
// ErrNotFound when the database has none by that name. Until 2026-09-22 it
// answered absence with (nil, nil), as CertificateByName did.
func (d *Database) AsymmetricKeyByName(ctx context.Context, name string) (*AsymmetricKey, error) {
	var k *AsymmetricKey
	err := d.queryRow(ctx, func(row *sql.Row) error {
		var err error
		k, err = scanAsymmetricKey(d, row.Scan)
		return err
	}, asymmetricKeySelect+`
WHERE  k.name = @p1`, name)
	return foundRow(k, err, notFoundf("gosmo: asymmetric key %s not found in %q", quoteIdent(name), d.Name),
		fmt.Sprintf("read asymmetric key %q in %q", name, d.Name))
}

func scanAsymmetricKey(d *Database, scan func(...any) error) (*AsymmetricKey, error) {
	k := &AsymmetricKey{db: d}
	if err := scan(&k.Name, &k.KeyID, &k.PrincipalID, &k.Algorithm,
		&k.KeyLength, &k.PvtKeyEncryptionType, &k.Thumbprint,
		&k.Owner, &k.ProviderType, &k.AttestedBy); err != nil {
		return nil, err
	}
	return k, nil
}

// AsymmetricKeyRef returns a lightweight handle for an asymmetric key by
// name, without querying the catalog — the counterpart of
// Server.DatabaseRef. KeyID, Algorithm and every other cached field stay at
// their zero value (HasPrivateKey answers false); AsymmetricKeyByName is what
// populates them.
//
// Every write on *AsymmetricKey addresses it by name, so this handle is
// enough to drop one the caller already knows exists — and is the form to
// use when there is nothing to read yet, such as a New Asymmetric Key dialog
// scripting a key whose CREATE was only collected.
func (d *Database) AsymmetricKeyRef(name string) *AsymmetricKey {
	return &AsymmetricKey{db: d, Name: name}
}

// AsymmetricKeyAlgorithm names the algorithm of a generated asymmetric key,
// as CREATE ASYMMETRIC KEY ... WITH ALGORITHM spells it.
type AsymmetricKeyAlgorithm string

// The algorithms CREATE ASYMMETRIC KEY accepts. RSA_512 and RSA_1024 are a
// syntax error (Msg 102) in a database at compatibility level 130 or higher,
// which is every database created on a supported version unless it was
// lowered; they are accepted here for the databases that were.
const (
	AsymmetricKeyRSA512  AsymmetricKeyAlgorithm = "RSA_512"
	AsymmetricKeyRSA1024 AsymmetricKeyAlgorithm = "RSA_1024"
	AsymmetricKeyRSA2048 AsymmetricKeyAlgorithm = "RSA_2048"
	AsymmetricKeyRSA3072 AsymmetricKeyAlgorithm = "RSA_3072"
	AsymmetricKeyRSA4096 AsymmetricKeyAlgorithm = "RSA_4096"
)

// valid reports whether a is one of the algorithms above. The algorithm is
// written into the statement unquoted, so nothing else may reach it.
func (a AsymmetricKeyAlgorithm) valid() bool {
	switch a {
	case AsymmetricKeyRSA512, AsymmetricKeyRSA1024, AsymmetricKeyRSA2048, AsymmetricKeyRSA3072, AsymmetricKeyRSA4096:
		return true
	}
	return false
}

// AsymmetricKeySpec describes an asymmetric key for SQL Server to generate.
type AsymmetricKeySpec struct {
	Name string

	// Authorization is the database user or role that will own the key.
	// Empty leaves it owned by the caller.
	Authorization string

	// Algorithm is the key's algorithm; required.
	Algorithm AsymmetricKeyAlgorithm

	// EncryptionPassword protects the private key with a password instead of
	// the database master key. Empty needs the database to have a master key,
	// or CREATE fails with Msg 15581.
	EncryptionPassword string

	// FromProvider has an EKM provider hold the key instead of SQL Server
	// generating it (FROM PROVIDER). It takes no EncryptionPassword — the
	// provider protects the key — and Algorithm is optional with
	// ProviderOpenExisting, where the provider already knows it.
	FromProvider *ProviderKey
}

// createAsymmetricKeyStatement builds CREATE ASYMMETRIC KEY, validating the
// spec.
func (spec AsymmetricKeySpec) createAsymmetricKeyStatement() (string, error) {
	if strings.TrimSpace(spec.Name) == "" {
		return "", fmt.Errorf("asymmetric key has no name")
	}
	stmt := "CREATE ASYMMETRIC KEY " + quoteIdent(spec.Name)
	if spec.Authorization != "" {
		stmt += " AUTHORIZATION " + quoteIdent(spec.Authorization)
	}
	if p := spec.FromProvider; p != nil {
		if spec.EncryptionPassword != "" {
			return "", fmt.Errorf("asymmetric key %q is held by a provider, so it takes no password", spec.Name)
		}
		var opts []string
		if spec.Algorithm != "" || p.Disposition != ProviderOpenExisting {
			if !spec.Algorithm.valid() {
				return "", fmt.Errorf("asymmetric key %q: unknown algorithm %q", spec.Name, spec.Algorithm)
			}
			opts = append(opts, "ALGORITHM = "+string(spec.Algorithm))
		}
		from, popts, err := p.clauses()
		if err != nil {
			return "", fmt.Errorf("asymmetric key %q: %w", spec.Name, err)
		}
		return stmt + from + " WITH " + strings.Join(append(opts, popts...), ", "), nil
	}
	if !spec.Algorithm.valid() {
		return "", fmt.Errorf("asymmetric key %q: unknown algorithm %q", spec.Name, spec.Algorithm)
	}
	stmt += " WITH ALGORITHM = " + string(spec.Algorithm)
	if spec.EncryptionPassword != "" {
		stmt += " ENCRYPTION BY PASSWORD = " + QuoteLiteral(spec.EncryptionPassword)
	}
	return stmt, nil
}

// CreateAsymmetricKey has SQL Server generate a new asymmetric key pair in
// the database.
func (d *Database) CreateAsymmetricKey(ctx context.Context, spec AsymmetricKeySpec) error {
	stmt, err := spec.createAsymmetricKeyStatement()
	if err != nil {
		return fmt.Errorf("gosmo: create asymmetric key in %q: %w", d.Name, err)
	}
	if _, err := d.exec(ctx, stmt); err != nil {
		return fmt.Errorf("gosmo: create asymmetric key %q in %q: %w", spec.Name, d.Name, err)
	}
	return nil
}

// Drop deletes the asymmetric key. SQL Server refuses (Msg 15559) while a
// login or user is mapped to it.
func (k *AsymmetricKey) Drop(ctx context.Context) error {
	if _, err := k.db.exec(ctx, "DROP ASYMMETRIC KEY "+quoteIdent(k.Name)); err != nil {
		return fmt.Errorf("gosmo: drop asymmetric key %q in %q: %w", k.Name, k.db.Name, err)
	}
	return nil
}

// RemovePrivateKey deletes the key's private half, leaving the public key —
// ALTER ASYMMETRIC KEY ... REMOVE PRIVATE KEY. It cannot be undone: SQL
// Server has no BACKUP ASYMMETRIC KEY (a syntax error on 17), so unlike a
// certificate's the private key cannot have been exported first.
func (k *AsymmetricKey) RemovePrivateKey(ctx context.Context) error {
	if _, err := k.db.exec(ctx, "ALTER ASYMMETRIC KEY "+quoteIdent(k.Name)+" REMOVE PRIVATE KEY"); err != nil {
		return fmt.Errorf("gosmo: remove private key of asymmetric key %q in %q: %w", k.Name, k.db.Name, err)
	}
	setIfApplied(ctx, &k.PvtKeyEncryptionType, "NO_PRIVATE_KEY")
	return nil
}

// ChangeOwner transfers the key to another database principal with ALTER
// AUTHORIZATION. SQL Server drops every explicit permission on the key as it
// does so.
func (k *AsymmetricKey) ChangeOwner(ctx context.Context, newOwner string) error {
	q := "ALTER AUTHORIZATION ON ASYMMETRIC KEY::" + quoteIdent(k.Name) + " TO " + quoteIdent(newOwner)
	if _, err := k.db.exec(ctx, q); err != nil {
		return fmt.Errorf("gosmo: change asymmetric key %q owner to %q in %q: %w", k.Name, newOwner, k.db.Name, err)
	}
	setIfApplied(ctx, &k.Owner, newOwner)
	return nil
}
