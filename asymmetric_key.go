package gosmo

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// asymmetric_key.go covers asymmetric keys, the asymmetric-key counterpart of
// certificate.go's read side. Creation is deliberately not here yet: nothing
// asks for it, and unlike CREATE CERTIFICATE ... FROM BINARY there is no way
// to move an existing key between instances over a connection — CREATE
// ASYMMETRIC KEY imports only FROM FILE, FROM ASSEMBLY or FROM PROVIDER, all
// of which read the server's own filesystem.

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
}

// HasPrivateKey reports whether this instance holds the key's private half,
// i.e. can sign with it rather than only verify against it.
func (k *AsymmetricKey) HasPrivateKey() bool {
	return k.PvtKeyEncryptionType != "" && k.PvtKeyEncryptionType != "NO_PRIVATE_KEY"
}

const asymmetricKeySelect = `
SELECT name, asymmetric_key_id, principal_id,
       ISNULL(algorithm_desc, ''), ISNULL(key_length, 0),
       ISNULL(pvt_key_encryption_type_desc, ''), ISNULL(thumbprint, 0x)
FROM   sys.asymmetric_keys`

// AsymmetricKeys returns the database's asymmetric keys, excluding the
// internal ones SQL Server creates for itself (named ##...##) — the same
// exclusion Certificates makes.
func (d *Database) AsymmetricKeys() ([]*AsymmetricKey, error) {
	return d.AsymmetricKeysContext(context.Background())
}

// AsymmetricKeysContext is the context-aware variant of AsymmetricKeys.
func (d *Database) AsymmetricKeysContext(ctx context.Context) ([]*AsymmetricKey, error) {
	rows, err := d.query(ctx, asymmetricKeySelect+`
WHERE  name NOT LIKE '##%'
ORDER  BY name`)
	if err != nil {
		return nil, fmt.Errorf("gosmo: list asymmetric keys in %q: %w", d.name, err)
	}
	defer rows.Close()

	var out []*AsymmetricKey
	for rows.Next() {
		k, err := scanAsymmetricKey(d, rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("gosmo: list asymmetric keys in %q: %w", d.name, err)
		}
		out = append(out, k)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("gosmo: list asymmetric keys in %q: %w", d.name, err)
	}
	return out, nil
}

// AsymmetricKeyByName returns one asymmetric key, or (nil, nil) when the
// database has none by that name — matching CertificateByName, whose absent
// answer this family's callers already branch on.
func (d *Database) AsymmetricKeyByName(name string) (*AsymmetricKey, error) {
	return d.AsymmetricKeyByNameContext(context.Background(), name)
}

// AsymmetricKeyByNameContext is the context-aware variant of
// AsymmetricKeyByName.
func (d *Database) AsymmetricKeyByNameContext(ctx context.Context, name string) (*AsymmetricKey, error) {
	var k *AsymmetricKey
	err := d.queryRow(ctx, func(row *sql.Row) error {
		var err error
		k, err = scanAsymmetricKey(d, row.Scan)
		return err
	}, asymmetricKeySelect+`
WHERE  name = @p1`, name)
	// errors.Is rather than ==, for the reason spelled out on
	// CertificateByNameContext: Database.queryRow wraps some of its failures,
	// and a bare comparison that stopped matching would turn "no such key"
	// into an error for callers that branch on k == nil.
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("gosmo: read asymmetric key %q in %q: %w", name, d.name, err)
	}
	return k, nil
}

func scanAsymmetricKey(d *Database, scan func(...any) error) (*AsymmetricKey, error) {
	k := &AsymmetricKey{db: d}
	if err := scan(&k.Name, &k.KeyID, &k.PrincipalID, &k.Algorithm,
		&k.KeyLength, &k.PvtKeyEncryptionType, &k.Thumbprint); err != nil {
		return nil, err
	}
	return k, nil
}
