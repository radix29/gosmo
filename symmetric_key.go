package gosmo

import (
	"bytes"
	"cmp"
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"
	"time"
)

// symmetric_key.go covers symmetric keys — sys.symmetric_keys and the
// encryptions sys.key_encryptions records for each. The database master key
// lives in the same catalog view as ##MS_DatabaseMasterKey## and is kept out
// of this type: HasMasterKey and CreateMasterKey (certificate.go) are its
// surface, and every read here skips the ##...## keys, as Certificates and
// AsymmetricKeys do.

// SymmetricKey mirrors a row of sys.symmetric_keys, with the encryptions that
// protect it.
type SymmetricKey struct {
	db *Database

	Name        string
	KeyID       int
	PrincipalID int

	// Owner is the name of the database principal that owns the key
	// (principal_id resolved through sys.database_principals).
	Owner string

	// Algorithm is the key algorithm's description — "AES_256" and the like.
	Algorithm string

	// KeyLength is the key length in bits.
	KeyLength int

	// KeyGUID is the key's GUID in its canonical string form. A key created
	// with the same IDENTITY_VALUE has the same GUID in every database, which
	// is how data encrypted in one decrypts in another.
	KeyGUID string

	CreateDate time.Time
	ModifyDate time.Time

	// ProviderType is "CRYPTOGRAPHIC PROVIDER" for a key held by an EKM
	// provider, empty for one SQL Server holds itself.
	ProviderType string

	// Encryptions lists what protects the key — the ENCRYPTION BY clauses it
	// was created with, plus any added since — ordered by kind, then name.
	Encryptions []SymmetricKeyEncryption
}

// Database returns the database the symmetric key belongs to.
func (k *SymmetricKey) Database() *Database { return k.db }

// SymmetricKeyEncryptionKind names what encrypts a symmetric key, as
// CREATE SYMMETRIC KEY ... ENCRYPTION BY spells it.
type SymmetricKeyEncryptionKind string

const (
	SymmetricKeyByCertificate   SymmetricKeyEncryptionKind = "CERTIFICATE"
	SymmetricKeyByAsymmetricKey SymmetricKeyEncryptionKind = "ASYMMETRIC KEY"
	SymmetricKeyBySymmetricKey  SymmetricKeyEncryptionKind = "SYMMETRIC KEY"
	SymmetricKeyByPassword      SymmetricKeyEncryptionKind = "PASSWORD"

	// SymmetricKeyByMasterKey protects only the database master key, which
	// this type excludes; it is here so a row carrying it is classified
	// rather than left unknown.
	SymmetricKeyByMasterKey SymmetricKeyEncryptionKind = "MASTER KEY"
)

// SymmetricKeyEncryption is one row of sys.key_encryptions: one thing that
// can open the key.
type SymmetricKeyEncryption struct {
	// Kind is what encrypts the key; empty for a crypt_type_desc this
	// version of gosmo does not recognise (CryptTypeDesc still says).
	Kind SymmetricKeyEncryptionKind

	// Name is the certificate, asymmetric key or symmetric key the
	// encryption's thumbprint resolves to. Empty for a password, and for an
	// encryptor the caller cannot see — metadata visibility hides a key or
	// certificate the principal has no right on, and the join then finds no
	// row.
	Name string

	// CryptTypeDesc is sys.key_encryptions.crypt_type_desc as the server
	// reports it — "ENCRYPTION BY PASSWORD V2", "ENCRYPTION BY CERTIFICATE
	// OAEP256" and the like.
	CryptTypeDesc string

	// Thumbprint identifies the encryptor: a certificate's or asymmetric
	// key's thumbprint, a symmetric key's GUID as 16 bytes; empty for a
	// password.
	Thumbprint []byte
}

// symmetricKeyEncryptionKind classifies a crypt_type_desc by its prefix. The
// crypt_type code differs by major and platform for the same encryption — a
// certificate is EPUC on 13 and 14 and on Managed Instance, C256 on 17; a
// password ESKP on 13, ESP2 on 14 and later — so the code is never used.
func symmetricKeyEncryptionKind(desc string) SymmetricKeyEncryptionKind {
	rest, ok := strings.CutPrefix(desc, "ENCRYPTION BY ")
	if !ok {
		return ""
	}
	for _, k := range []SymmetricKeyEncryptionKind{
		SymmetricKeyByCertificate, SymmetricKeyByAsymmetricKey, SymmetricKeyBySymmetricKey,
		SymmetricKeyByPassword, SymmetricKeyByMasterKey,
	} {
		if strings.HasPrefix(rest, string(k)) {
			return k
		}
	}
	return ""
}

// symmetricKeySelect returns one row per (key, encryption), keys in name
// order, so a key's encryptions arrive together and are grouped in Go — one
// query for the whole list, never one per key.
//
// A symmetric-key encryptor's thumbprint is its key_guid, so that join
// converts it — guarded to 16-byte values, the only length CONVERT to
// uniqueidentifier reads whole. The kind predicates keep each join to its own
// rows. The caller appends a WHERE on k and the ORDER BY.
const symmetricKeySelect = `
SELECT k.name, k.symmetric_key_id, k.principal_id, ISNULL(p.name, N''),
       ISNULL(k.algorithm_desc, N''), ISNULL(k.key_length, 0),
       ISNULL(CONVERT(nvarchar(36), k.key_guid), N''),
       k.create_date, k.modify_date, ISNULL(k.provider_type, N''),
       e.crypt_type_desc, e.thumbprint,
       COALESCE(c.name, a.name, s.name)
FROM   sys.symmetric_keys k
LEFT   JOIN sys.database_principals p ON p.principal_id = k.principal_id
LEFT   JOIN sys.key_encryptions e ON e.key_id = k.symmetric_key_id
LEFT   JOIN sys.certificates c
       ON e.crypt_type_desc LIKE N'ENCRYPTION BY CERTIFICATE%' AND c.thumbprint = e.thumbprint
LEFT   JOIN sys.asymmetric_keys a
       ON e.crypt_type_desc LIKE N'ENCRYPTION BY ASYMMETRIC KEY%' AND a.thumbprint = e.thumbprint
LEFT   JOIN sys.symmetric_keys s
       ON e.crypt_type_desc LIKE N'ENCRYPTION BY SYMMETRIC KEY%' AND DATALENGTH(e.thumbprint) = 16
      AND s.key_guid = CONVERT(uniqueidentifier, e.thumbprint)`

const symmetricKeyOrder = `
ORDER  BY k.name, k.symmetric_key_id`

// SymmetricKeys returns the database's symmetric keys with their encryptions,
// excluding the database master key and the other internal ##...## keys.
func (d *Database) SymmetricKeys(ctx context.Context) ([]*SymmetricKey, error) {
	out, err := d.symmetricKeys(ctx, `
WHERE  k.name NOT LIKE '##%'`)
	if err != nil {
		return nil, fmt.Errorf("gosmo: list symmetric keys in %q: %w", d.Name, err)
	}
	return out, nil
}

// SymmetricKeyByName returns one symmetric key with its encryptions, or an
// error wrapping ErrNotFound when the database has none by that name. The
// database
// master key is not found by its ##MS_DatabaseMasterKey## name: it is not a
// SymmetricKey (see HasMasterKey).
func (d *Database) SymmetricKeyByName(ctx context.Context, name string) (*SymmetricKey, error) {
	out, err := d.symmetricKeys(ctx, `
WHERE  k.name = @p1 AND k.name NOT LIKE '##%'`, name)
	if err != nil {
		return nil, fmt.Errorf("gosmo: read symmetric key %q in %q: %w", name, d.Name, err)
	}
	if len(out) == 0 {
		return nil, notFoundf("gosmo: symmetric key %s not found in %q", quoteIdent(name), d.Name)
	}
	return out[0], nil
}

// symmetricKeys runs symmetricKeySelect with the given WHERE and groups the
// rows into keys. The error is bare; each caller names its operation.
func (d *Database) symmetricKeys(ctx context.Context, where string, args ...any) ([]*SymmetricKey, error) {
	rows, err := d.query(ctx, symmetricKeySelect+where+symmetricKeyOrder, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*SymmetricKey
	for rows.Next() {
		var (
			k          = &SymmetricKey{db: d}
			desc, name sql.NullString
			thumbprint []byte
		)
		if err := rows.Scan(&k.Name, &k.KeyID, &k.PrincipalID, &k.Owner,
			&k.Algorithm, &k.KeyLength, &k.KeyGUID,
			&k.CreateDate, &k.ModifyDate, &k.ProviderType,
			&desc, &thumbprint, &name); err != nil {
			return nil, err
		}
		// Rows arrive ordered by key, so a key's later rows follow its first.
		if n := len(out); n > 0 && out[n-1].KeyID == k.KeyID {
			k = out[n-1]
		} else {
			out = append(out, k)
		}
		if desc.Valid { // NULL: a key with no encryption row (EKM)
			k.Encryptions = append(k.Encryptions, SymmetricKeyEncryption{
				Kind:          symmetricKeyEncryptionKind(desc.String),
				Name:          name.String,
				CryptTypeDesc: desc.String,
				Thumbprint:    thumbprint,
			})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, k := range out {
		sortSymmetricKeyEncryptions(k.Encryptions)
	}
	return out, nil
}

// sortSymmetricKeyEncryptions orders encryptions by kind (the order CREATE
// SYMMETRIC KEY's grammar lists them), then name, then thumbprint — the
// catalog keeps no creation order, and a stable one is what makes a script
// or a Properties page diff cleanly.
func sortSymmetricKeyEncryptions(es []SymmetricKeyEncryption) {
	rank := func(k SymmetricKeyEncryptionKind) int {
		switch k {
		case SymmetricKeyByCertificate:
			return 0
		case SymmetricKeyByPassword:
			return 1
		case SymmetricKeyBySymmetricKey:
			return 2
		case SymmetricKeyByAsymmetricKey:
			return 3
		case SymmetricKeyByMasterKey:
			return 4
		}
		return 5
	}
	slices.SortStableFunc(es, func(a, b SymmetricKeyEncryption) int {
		if c := cmp.Compare(rank(a.Kind), rank(b.Kind)); c != 0 {
			return c
		}
		if c := strings.Compare(a.Name, b.Name); c != 0 {
			return c
		}
		return bytes.Compare(a.Thumbprint, b.Thumbprint)
	})
}

// SymmetricKeyRef returns a lightweight handle for a symmetric key by name,
// without querying the catalog — the counterpart of Server.DatabaseRef.
// KeyID, Algorithm, Encryptions and every other cached field stay at their
// zero value; SymmetricKeyByName is what populates them.
//
// It is the form to use when there is nothing to read yet, such as a New
// Symmetric Key dialog scripting a key whose CREATE was only collected, and
// for any write that addresses the key by name.
func (d *Database) SymmetricKeyRef(name string) *SymmetricKey {
	return &SymmetricKey{db: d, Name: name}
}

// SymmetricKeyAlgorithm names a symmetric key's algorithm, as CREATE
// SYMMETRIC KEY ... WITH ALGORITHM spells it.
type SymmetricKeyAlgorithm string

// The algorithms CREATE SYMMETRIC KEY accepts. Only the AES ones work in a
// database at compatibility level 130 or higher — every other is a syntax
// error there (Msg 102) — which is every database created on a supported
// version unless it was lowered; they are listed for the databases that were,
// and for keys created before.
const (
	SymmetricKeyAES128     SymmetricKeyAlgorithm = "AES_128"
	SymmetricKeyAES192     SymmetricKeyAlgorithm = "AES_192"
	SymmetricKeyAES256     SymmetricKeyAlgorithm = "AES_256"
	SymmetricKeyDES        SymmetricKeyAlgorithm = "DES"
	SymmetricKeyTripleDES  SymmetricKeyAlgorithm = "TRIPLE_DES"
	SymmetricKeyTripleDES3 SymmetricKeyAlgorithm = "TRIPLE_DES_3KEY"
	SymmetricKeyRC2        SymmetricKeyAlgorithm = "RC2"
	SymmetricKeyRC4        SymmetricKeyAlgorithm = "RC4"
	SymmetricKeyRC4128     SymmetricKeyAlgorithm = "RC4_128"
	SymmetricKeyDESX       SymmetricKeyAlgorithm = "DESX"
)

// valid reports whether a is one of the algorithms above. The algorithm is
// written into a statement unquoted, so nothing else may reach it.
func (a SymmetricKeyAlgorithm) valid() bool {
	switch a {
	case SymmetricKeyAES128, SymmetricKeyAES192, SymmetricKeyAES256,
		SymmetricKeyDES, SymmetricKeyTripleDES, SymmetricKeyTripleDES3,
		SymmetricKeyRC2, SymmetricKeyRC4, SymmetricKeyRC4128, SymmetricKeyDESX:
		return true
	}
	return false
}

// -- Writes -------------------------------------------------------------------

// SymmetricKeyEncryptor is one ENCRYPTION BY item: what a new key is created
// with (CreateSymmetricKeyRequest.Encryptions), or what ADD / DROP ENCRYPTION adds to
// or removes from an existing one.
type SymmetricKeyEncryptor struct {
	// Kind is SymmetricKeyByCertificate, SymmetricKeyByAsymmetricKey,
	// SymmetricKeyBySymmetricKey or SymmetricKeyByPassword. The master key
	// never encrypts a symmetric key and is refused.
	Kind SymmetricKeyEncryptionKind

	// Name is the certificate, asymmetric key or symmetric key; unused for a
	// password.
	Name string

	// Password is the password, for SymmetricKeyByPassword only. DROP
	// ENCRYPTION needs it too: the server matches it against the encryption
	// to remove (Msg 15313 when none matches).
	Password string

	// Open says how to open the symmetric key Name. It is required for
	// SymmetricKeyBySymmetricKey and ignored otherwise: the server encrypts
	// with a symmetric key, and removes an encryption by one, only while that
	// key is open (Msg 15315).
	Open *SymmetricKeyDecryptor
}

// SymmetricKeyDecryptor says how to open a symmetric key — OPEN SYMMETRIC
// KEY ... DECRYPTION BY. It names one of the key's encryptions.
type SymmetricKeyDecryptor struct {
	// Kind is SymmetricKeyByCertificate, SymmetricKeyByAsymmetricKey,
	// SymmetricKeyBySymmetricKey or SymmetricKeyByPassword.
	Kind SymmetricKeyEncryptionKind

	// Name is the certificate, asymmetric key or symmetric key; unused for a
	// password.
	Name string

	// Password is the key's password for SymmetricKeyByPassword. For a
	// certificate or asymmetric key it is the password protecting that
	// object's private key, and is empty when the database master key
	// protects it instead — such a key opens with no password at all, while
	// a password-protected one opened without it fails with Msg 15334.
	Password string

	// Open says how to open the symmetric key Name first, for
	// SymmetricKeyBySymmetricKey; required for that kind, ignored otherwise.
	// A key encrypted by another symmetric key cannot be opened until that
	// one is (Msg 15315 naming the parent).
	Open *SymmetricKeyDecryptor
}

// maxKeyChain bounds how deep Open may nest. Real chains are one or two keys
// deep; the bound is what stops a cyclic Open from recursing forever.
const maxKeyChain = 8

// clause renders the encryptor as an ENCRYPTION BY item.
func (e SymmetricKeyEncryptor) clause() (string, error) {
	switch e.Kind {
	case SymmetricKeyByPassword:
		if e.Password == "" {
			return "", fmt.Errorf("encryption by password has no password")
		}
		return "PASSWORD = " + QuoteLiteral(e.Password), nil
	case SymmetricKeyByCertificate, SymmetricKeyByAsymmetricKey, SymmetricKeyBySymmetricKey:
		if strings.TrimSpace(e.Name) == "" {
			return "", fmt.Errorf("encryption by %s has no name", strings.ToLower(string(e.Kind)))
		}
		return string(e.Kind) + " " + quoteIdent(e.Name), nil
	}
	return "", fmt.Errorf("a symmetric key cannot be encrypted by %q", e.Kind)
}

// clause renders the decryptor as an OPEN SYMMETRIC KEY ... DECRYPTION BY
// item.
func (dec SymmetricKeyDecryptor) clause() (string, error) {
	switch dec.Kind {
	case SymmetricKeyByPassword:
		if dec.Password == "" {
			return "", fmt.Errorf("decryption by password has no password")
		}
		return "PASSWORD = " + QuoteLiteral(dec.Password), nil
	case SymmetricKeyByCertificate, SymmetricKeyByAsymmetricKey, SymmetricKeyBySymmetricKey:
		if strings.TrimSpace(dec.Name) == "" {
			return "", fmt.Errorf("decryption by %s has no name", strings.ToLower(string(dec.Kind)))
		}
		s := string(dec.Kind) + " " + quoteIdent(dec.Name)
		if dec.Password != "" && dec.Kind != SymmetricKeyBySymmetricKey {
			s += " WITH PASSWORD = " + QuoteLiteral(dec.Password)
		}
		return s, nil
	}
	return "", fmt.Errorf("a symmetric key cannot be opened by %q", dec.Kind)
}

// keyOpens collects the OPEN SYMMETRIC KEY statements a write needs, each
// key's parent before the key, each key once. Opening a key twice is harmless
// but closing it twice is Msg 15315, so the dedupe is what keeps the CLOSEs
// right.
type keyOpens struct {
	names []string // open order
	opens []string
}

// add records how to open the symmetric key name, after whatever dec's own
// chain needs opened first.
func (o *keyOpens) add(name string, dec *SymmetricKeyDecryptor, depth int) error {
	if slices.Contains(o.names, name) {
		return nil
	}
	if dec == nil {
		return fmt.Errorf("symmetric key %q has no decryptor to open it with", name)
	}
	if depth >= maxKeyChain {
		return fmt.Errorf("symmetric key %q: decryptor chain deeper than %d", name, maxKeyChain)
	}
	if dec.Kind == SymmetricKeyBySymmetricKey {
		if err := o.add(dec.Name, dec.Open, depth+1); err != nil {
			return err
		}
	}
	c, err := dec.clause()
	if err != nil {
		return fmt.Errorf("symmetric key %q: %w", name, err)
	}
	o.names = append(o.names, name)
	o.opens = append(o.opens, "OPEN SYMMETRIC KEY "+quoteIdent(name)+" DECRYPTION BY "+c)
	return nil
}

// addEncryptor records the open an encryptor needs: a symmetric key encrypts,
// or has an encryption by it removed, only while open.
func (o *keyOpens) addEncryptor(e SymmetricKeyEncryptor) error {
	if e.Kind != SymmetricKeyBySymmetricKey {
		return nil
	}
	return o.add(e.Name, e.Open, 0)
}

// wrap renders stmt between the OPENs and their CLOSEs as one batch — gosmo
// hands each exec its own pooled connection, and an open key is scoped to the
// session that opened it, so OPEN, the write and CLOSE only work together as
// one batch on one connection. With nothing to open, stmt is returned as is.
//
// The CLOSEs are reached on failure too, through the CATCH, so a failed write
// never leaves a key open — on gosmo's pooled connection that would be reset
// anyway, but a Script Changes script runs in the user's own session. Each
// CATCH-side CLOSE is guarded by sys.openkeys because closing a key that is
// not open is itself Msg 15315, which would replace the error THROW is there
// to report: an OPEN that failed leaves its own key, and every one after it,
// closed. THROW re-raises the original error.
func (o *keyOpens) wrap(stmt string) string {
	if len(o.names) == 0 {
		return stmt
	}
	var b strings.Builder
	b.WriteString("BEGIN TRY\n")
	for _, s := range o.opens {
		b.WriteString(s + ";\n")
	}
	b.WriteString(stmt + ";\n")
	for i := len(o.names) - 1; i >= 0; i-- {
		b.WriteString("CLOSE SYMMETRIC KEY " + quoteIdent(o.names[i]) + ";\n")
	}
	b.WriteString("END TRY\nBEGIN CATCH\n")
	for i := len(o.names) - 1; i >= 0; i-- {
		fmt.Fprintf(&b, "IF EXISTS (SELECT 1 FROM sys.openkeys WHERE database_id = DB_ID() AND key_name = N'%s')\n"+
			"    CLOSE SYMMETRIC KEY %s;\n", escapeSingle(o.names[i]), quoteIdent(o.names[i]))
	}
	b.WriteString("THROW;\nEND CATCH;")
	return b.String()
}

// CreateSymmetricKeyRequest describes a symmetric key for CREATE SYMMETRIC KEY.
type CreateSymmetricKeyRequest struct {
	Name string

	// Authorization is the database user or role that will own the key.
	// Empty leaves it owned by the caller.
	Authorization string

	// Algorithm is the key's algorithm; required. Only the AES ones work at
	// compatibility level 130 or higher (see SymmetricKeyAlgorithm).
	Algorithm SymmetricKeyAlgorithm

	// KeySource and IdentityValue, both optional, make the key re-creatable:
	// the key material derives from KeySource and the GUID from
	// IdentityValue, so the same pair creates the same key in another
	// database — the one way to decrypt data there without a backup. Both are
	// secrets, and neither can be read back from the server.
	KeySource     string
	IdentityValue string

	// Encryptions is what protects the key; at least one is required, except
	// with FromProvider, which takes none. An encryptor that is a symmetric
	// key must say how to open it.
	Encryptions []SymmetricKeyEncryptor

	// FromProvider has an EKM provider hold the key (FROM PROVIDER). The
	// provider protects it, so Encryptions must be empty, and KEY_SOURCE /
	// IDENTITY_VALUE are the provider's business and refused. Algorithm is
	// optional with ProviderOpenExisting.
	FromProvider *ProviderKey
}

// createSymmetricKeyStatement builds CREATE SYMMETRIC KEY, wrapped in the
// OPEN / CLOSE of every symmetric key it is encrypted by, validating the
// spec.
func (spec CreateSymmetricKeyRequest) createSymmetricKeyStatement() (string, error) {
	if strings.TrimSpace(spec.Name) == "" {
		return "", fmt.Errorf("symmetric key has no name")
	}
	if spec.FromProvider != nil {
		return spec.createProviderSymmetricKeyStatement()
	}
	if !spec.Algorithm.valid() {
		return "", fmt.Errorf("symmetric key %q: unknown algorithm %q", spec.Name, spec.Algorithm)
	}
	if len(spec.Encryptions) == 0 {
		return "", fmt.Errorf("symmetric key %q has no encryption", spec.Name)
	}
	var (
		opens keyOpens
		items []string
	)
	for _, e := range spec.Encryptions {
		c, err := e.clause()
		if err != nil {
			return "", fmt.Errorf("symmetric key %q: %w", spec.Name, err)
		}
		if err := opens.addEncryptor(e); err != nil {
			return "", fmt.Errorf("symmetric key %q: %w", spec.Name, err)
		}
		items = append(items, c)
	}
	stmt := "CREATE SYMMETRIC KEY " + quoteIdent(spec.Name)
	if spec.Authorization != "" {
		stmt += " AUTHORIZATION " + quoteIdent(spec.Authorization)
	}
	stmt += " WITH ALGORITHM = " + string(spec.Algorithm)
	if spec.KeySource != "" {
		stmt += ", KEY_SOURCE = " + QuoteLiteral(spec.KeySource)
	}
	if spec.IdentityValue != "" {
		stmt += ", IDENTITY_VALUE = " + QuoteLiteral(spec.IdentityValue)
	}
	stmt += " ENCRYPTION BY " + strings.Join(items, ", ")
	return opens.wrap(stmt), nil
}

// createProviderSymmetricKeyStatement builds CREATE SYMMETRIC KEY ... FROM
// PROVIDER.
func (spec CreateSymmetricKeyRequest) createProviderSymmetricKeyStatement() (string, error) {
	p := spec.FromProvider
	if len(spec.Encryptions) > 0 {
		return "", fmt.Errorf("symmetric key %q is held by a provider, so it takes no encryption", spec.Name)
	}
	if spec.KeySource != "" || spec.IdentityValue != "" {
		return "", fmt.Errorf("symmetric key %q is held by a provider, so it takes no KEY_SOURCE or IDENTITY_VALUE", spec.Name)
	}
	stmt := "CREATE SYMMETRIC KEY " + quoteIdent(spec.Name)
	if spec.Authorization != "" {
		stmt += " AUTHORIZATION " + quoteIdent(spec.Authorization)
	}
	var opts []string
	if spec.Algorithm != "" || p.Disposition != ProviderOpenExisting {
		if !spec.Algorithm.valid() {
			return "", fmt.Errorf("symmetric key %q: unknown algorithm %q", spec.Name, spec.Algorithm)
		}
		opts = append(opts, "ALGORITHM = "+string(spec.Algorithm))
	}
	from, popts, err := p.clauses()
	if err != nil {
		return "", fmt.Errorf("symmetric key %q: %w", spec.Name, err)
	}
	return stmt + from + " WITH " + strings.Join(append(opts, popts...), ", "), nil
}

// CreateSymmetricKey creates a symmetric key in the database. A key
// encrypted by a certificate or asymmetric key needs CONTROL on it (Msg
// 15151 without); one encrypted by another symmetric key opens that key
// first, in the same batch.
//
// It returns the key read back from the catalog — or, under Scripting(ctx),
// the SymmetricKeyRef handle, since nothing ran.
func (d *Database) CreateSymmetricKey(ctx context.Context, spec CreateSymmetricKeyRequest) (*SymmetricKey, error) {
	stmt, err := spec.createSymmetricKeyStatement()
	if err != nil {
		return nil, fmt.Errorf("gosmo: create symmetric key in %q: %w", d.Name, err)
	}
	if _, err := d.exec(ctx, stmt); err != nil {
		return nil, fmt.Errorf("gosmo: create symmetric key %q in %q: %w", spec.Name, d.Name, err)
	}
	return createdObject(ctx, d.SymmetricKeyRef(spec.Name), func() (*SymmetricKey, error) {
		return d.SymmetricKeyByName(ctx, spec.Name)
	})
}

// Drop deletes the symmetric key. Data encrypted with it cannot be decrypted
// again, unless the key was created with KEY_SOURCE and IDENTITY_VALUE and is
// re-created from them.
func (k *SymmetricKey) Drop(ctx context.Context) error {
	if _, err := k.db.exec(ctx, "DROP SYMMETRIC KEY "+quoteIdent(k.Name)); err != nil {
		return fmt.Errorf("gosmo: drop symmetric key %q in %q: %w", k.Name, k.db.Name, err)
	}
	return nil
}

// SetOwner transfers the key to another database principal with ALTER
// AUTHORIZATION. SQL Server drops every explicit permission on the key as it
// does so.
func (k *SymmetricKey) SetOwner(ctx context.Context, newOwner string) error {
	q := "ALTER AUTHORIZATION ON SYMMETRIC KEY::" + quoteIdent(k.Name) + " TO " + quoteIdent(newOwner)
	if _, err := k.db.exec(ctx, q); err != nil {
		return fmt.Errorf("gosmo: change symmetric key %q owner to %q in %q: %w", k.Name, newOwner, k.db.Name, err)
	}
	setIfApplied(ctx, &k.Owner, newOwner)
	return nil
}

// alterEncryptionStatement builds ALTER SYMMETRIC KEY ... ADD or DROP
// ENCRYPTION BY enc, wrapped in the OPEN / CLOSE of the key itself (by dec)
// and of enc when it is a symmetric key.
func (k *SymmetricKey) alterEncryptionStatement(verb string, enc SymmetricKeyEncryptor, dec SymmetricKeyDecryptor) (string, error) {
	c, err := enc.clause()
	if err != nil {
		return "", err
	}
	var opens keyOpens
	if err := opens.add(k.Name, &dec, 0); err != nil {
		return "", err
	}
	if err := opens.addEncryptor(enc); err != nil {
		return "", err
	}
	return opens.wrap("ALTER SYMMETRIC KEY " + quoteIdent(k.Name) + " " + verb + " ENCRYPTION BY " + c), nil
}

// AddEncryption adds enc to what protects the key. The server alters a key
// only while it is open in the same session (Msg 15315), so dec says how to
// open it; the OPEN, the ALTER and the CLOSE go as one batch on one
// connection. Encrypting by a certificate or asymmetric key needs a right on
// it — VIEW DEFINITION is enough to add, CONTROL is needed to use it as dec.
//
// The receiver's Encryptions are not updated; SymmetricKeyByName reads them
// afresh.
func (k *SymmetricKey) AddEncryption(ctx context.Context, enc SymmetricKeyEncryptor, dec SymmetricKeyDecryptor) error {
	stmt, err := k.alterEncryptionStatement("ADD", enc, dec)
	if err != nil {
		return fmt.Errorf("gosmo: add encryption to symmetric key %q in %q: %w", k.Name, k.db.Name, err)
	}
	if _, err := k.db.exec(ctx, stmt); err != nil {
		return fmt.Errorf("gosmo: add encryption to symmetric key %q in %q: %w", k.Name, k.db.Name, err)
	}
	return nil
}

// DropEncryption removes enc from what protects the key, opening it by dec as
// AddEncryption does. dec may name the very encryption being removed. The
// server refuses to remove the last one (Msg 15558), and removing an
// encryption by a certificate or asymmetric key needs CONTROL on it.
//
// The receiver's Encryptions are not updated; SymmetricKeyByName reads them
// afresh.
func (k *SymmetricKey) DropEncryption(ctx context.Context, enc SymmetricKeyEncryptor, dec SymmetricKeyDecryptor) error {
	stmt, err := k.alterEncryptionStatement("DROP", enc, dec)
	if err != nil {
		return fmt.Errorf("gosmo: drop encryption from symmetric key %q in %q: %w", k.Name, k.db.Name, err)
	}
	if _, err := k.db.exec(ctx, stmt); err != nil {
		return fmt.Errorf("gosmo: drop encryption from symmetric key %q in %q: %w", k.Name, k.db.Name, err)
	}
	return nil
}
