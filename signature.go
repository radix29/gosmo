package gosmo

import (
	"context"
	"fmt"
	"strings"
)

// signature.go covers module signing: ADD / DROP [COUNTER] SIGNATURE, and
// sys.crypt_properties, which records each signature against the module
// (class 1) with the signer's thumbprint.
//
// Probed 2026-09-22 on 13 and 17, identical: procedures, scalar and
// multi-statement functions and DML triggers sign; an inline table-valued
// function is refused (Msg 15560, "only modules can be signed"), and a
// database DDL trigger is not reachable by either ADD SIGNATURE TO name or
// DATABASE::name (Msg 15151). A second signature by the same signer is Msg
// 15557. A certificate signature's thumbprint is 20 bytes and an asymmetric
// key's 8, each matching its own catalog view's thumbprint column. DROP
// SIGNATURE takes no password, even for a signer whose private key has one.

// SignerKind names what signs a module, as ADD SIGNATURE ... BY spells it.
type SignerKind string

const (
	SignerCertificate   SignerKind = "CERTIFICATE"
	SignerAsymmetricKey SignerKind = "ASYMMETRIC KEY"
)

// ModuleSignature is one row of sys.crypt_properties on a module.
type ModuleSignature struct {
	db *Database

	ObjectID int
	Schema   string
	Module   string

	// ModuleType is the module's sys.objects.type_desc — "SQL_STORED_PROCEDURE"
	// and the like.
	ModuleType string

	// Kind is the signer's kind; empty for a crypt_type_desc this version of
	// gosmo does not recognise (CryptTypeDesc still says).
	Kind SignerKind

	// Signer is the certificate or asymmetric key the thumbprint resolves to.
	// Empty for one the caller cannot see: metadata visibility hides a
	// certificate or key the principal has no right on.
	Signer string

	// Counter is a counter signature (ADD COUNTER SIGNATURE), which lets the
	// module be called from a signed one without granting its permissions
	// on its own.
	Counter bool

	CryptTypeDesc string
	Thumbprint    []byte
}

// Database returns the database the signed module lives in.
func (s *ModuleSignature) Database() *Database { return s.db }

// signatureKind classifies a crypt_type_desc — "SIGNATURE BY CERTIFICATE",
// "COUNTER SIGNATURE BY ASYMMETRIC KEY" — by its text, never the crypt_type
// code, for symmetricKeyEncryptionKind's reason.
func signatureKind(desc string) (SignerKind, bool) {
	counter := strings.HasPrefix(desc, "COUNTER ")
	rest, ok := strings.CutPrefix(strings.TrimPrefix(desc, "COUNTER "), "SIGNATURE BY ")
	if !ok {
		return "", counter
	}
	for _, k := range []SignerKind{SignerCertificate, SignerAsymmetricKey} {
		if strings.HasPrefix(rest, string(k)) {
			return k, counter
		}
	}
	return "", counter
}

// moduleSignatureSelect lists module signatures with their modules and
// signers. The caller appends predicates starting with AND, and the order.
const moduleSignatureSelect = `
SELECT cp.major_id, SCHEMA_NAME(o.schema_id), o.name, o.type_desc,
       cp.crypt_type_desc, cp.thumbprint, COALESCE(c.name, a.name, N'')
FROM   sys.crypt_properties cp
JOIN   sys.objects o ON o.object_id = cp.major_id
LEFT   JOIN sys.certificates c
       ON cp.crypt_type_desc LIKE N'%SIGNATURE BY CERTIFICATE%' AND c.thumbprint = cp.thumbprint
LEFT   JOIN sys.asymmetric_keys a
       ON cp.crypt_type_desc LIKE N'%SIGNATURE BY ASYMMETRIC KEY%' AND a.thumbprint = cp.thumbprint
WHERE  cp.class = 1`

const moduleSignatureOrder = `
ORDER  BY SCHEMA_NAME(o.schema_id), o.name, cp.crypt_type_desc, COALESCE(c.name, a.name, N'')`

// moduleSignatures runs moduleSignatureSelect with the given predicate. The
// error is bare; each caller names its operation.
func (d *Database) moduleSignatures(ctx context.Context, and string, args ...any) ([]*ModuleSignature, error) {
	rows, err := d.query(ctx, moduleSignatureSelect+and+moduleSignatureOrder, args...)
	return scanRows(rows, err, "", func(scan func(...any) error) (*ModuleSignature, error) {
		s := &ModuleSignature{db: d}
		if err := scan(&s.ObjectID, &s.Schema, &s.Module, &s.ModuleType,
			&s.CryptTypeDesc, &s.Thumbprint, &s.Signer); err != nil {
			return nil, err
		}
		s.Kind, s.Counter = signatureKind(s.CryptTypeDesc)
		return s, nil
	})
}

// ModuleSignatures returns every signature on every module in the database.
func (d *Database) ModuleSignatures(ctx context.Context) ([]*ModuleSignature, error) {
	out, err := d.moduleSignatures(ctx, "")
	if err != nil {
		return nil, fmt.Errorf("gosmo: list module signatures in %q: %w", d.Name, err)
	}
	return out, nil
}

// SignaturesOn returns the signatures on one module — who signed it.
func (d *Database) SignaturesOn(ctx context.Context, schema, module string) ([]*ModuleSignature, error) {
	if err := requireSchema("signatures on", schema, module); err != nil {
		return nil, err
	}
	out, err := d.moduleSignatures(ctx, `
  AND  o.schema_id = SCHEMA_ID(@p1) AND o.name = @p2`, schema, module)
	if err != nil {
		return nil, fmt.Errorf("gosmo: read signatures on %s in %q: %w", qualifiedName(schema, module), d.Name, err)
	}
	return out, nil
}

// SignedModules returns the modules the certificate signs or counter-signs.
func (c *Certificate) SignedModules(ctx context.Context) ([]*ModuleSignature, error) {
	out, err := c.db.moduleSignatures(ctx, `
  AND  c.name = @p1`, c.Name)
	if err != nil {
		return nil, fmt.Errorf("gosmo: list modules signed by certificate %q in %q: %w", c.Name, c.db.Name, err)
	}
	return out, nil
}

// SignedModules returns the modules the asymmetric key signs or
// counter-signs.
func (k *AsymmetricKey) SignedModules(ctx context.Context) ([]*ModuleSignature, error) {
	out, err := k.db.moduleSignatures(ctx, `
  AND  a.name = @p1`, k.Name)
	if err != nil {
		return nil, fmt.Errorf("gosmo: list modules signed by asymmetric key %q in %q: %w", k.Name, k.db.Name, err)
	}
	return out, nil
}

// SignableModule is one module ADD SIGNATURE accepts.
type SignableModule struct {
	Schema string
	Name   string

	// Type is the module's sys.objects.type_desc.
	Type string
}

// SignableModules returns the modules in the database that can be signed:
// stored procedures, scalar and multi-statement table-valued functions and
// DML triggers, in schema and name order. An inline table-valued function is
// refused by the server (Msg 15560), and a database DDL trigger cannot be
// named by ADD SIGNATURE at all, so neither is listed. CLR modules are not
// listed either: signing one has not been tried.
func (d *Database) SignableModules(ctx context.Context) ([]*SignableModule, error) {
	rows, err := d.query(ctx, `
SELECT SCHEMA_NAME(o.schema_id), o.name, o.type_desc
FROM   sys.objects o
WHERE  o.type IN ('P', 'FN', 'TF', 'TR') AND o.is_ms_shipped = 0
ORDER  BY SCHEMA_NAME(o.schema_id), o.name`)
	return scanRows(rows, err, fmt.Sprintf("list signable modules in %q", d.Name), func(scan func(...any) error) (*SignableModule, error) {
		m := &SignableModule{}
		if err := scan(&m.Schema, &m.Name, &m.Type); err != nil {
			return nil, err
		}
		return m, nil
	})
}

// Signer names who signs: a certificate or asymmetric key, and the password
// on its private key when one protects it — empty when the database master
// key does, and ignored by DROP, which takes none.
type Signer struct {
	Kind     SignerKind
	Name     string
	Password string
}

func (s Signer) clause(withPassword bool) (string, error) {
	if s.Kind != SignerCertificate && s.Kind != SignerAsymmetricKey {
		return "", fmt.Errorf("a module cannot be signed by %q", s.Kind)
	}
	if strings.TrimSpace(s.Name) == "" {
		return "", fmt.Errorf("signature by %s has no name", strings.ToLower(string(s.Kind)))
	}
	c := string(s.Kind) + " " + quoteIdent(s.Name)
	if withPassword && s.Password != "" {
		c += " WITH PASSWORD = " + QuoteLiteral(s.Password)
	}
	return c, nil
}

// signatureStatement builds ADD or DROP [COUNTER] SIGNATURE.
func signatureStatement(add bool, schema, module string, by Signer, counter bool) (string, error) {
	if strings.TrimSpace(module) == "" {
		return "", fmt.Errorf("no module named")
	}
	c, err := by.clause(add)
	if err != nil {
		return "", err
	}
	verb, prep := "DROP", " FROM "
	if add {
		verb, prep = "ADD", " TO "
	}
	if counter {
		verb += " COUNTER"
	}
	return verb + " SIGNATURE" + prep + qualifiedName(schema, module) + " BY " + c, nil
}

// AddSignature signs a module — a procedure, a scalar or multi-statement
// function, or a DML trigger — with ADD [COUNTER] SIGNATURE. Signing needs
// the signer's private key, and CONTROL on it; altering the module later
// drops the signature.
func (d *Database) AddSignature(ctx context.Context, schema, module string, by Signer, counter bool) error {
	if err := requireSchema("add signature", schema, module); err != nil {
		return err
	}
	stmt, err := signatureStatement(true, schema, module, by, counter)
	if err == nil {
		_, err = d.exec(ctx, stmt)
	}
	if err != nil {
		return fmt.Errorf("gosmo: sign %s in %q: %w", qualifiedName(schema, module), d.Name, err)
	}
	return nil
}

// DropSignature removes a signature with DROP [COUNTER] SIGNATURE. by's
// Password is not used.
func (d *Database) DropSignature(ctx context.Context, schema, module string, by Signer, counter bool) error {
	if err := requireSchema("drop signature", schema, module); err != nil {
		return err
	}
	stmt, err := signatureStatement(false, schema, module, by, counter)
	if err == nil {
		_, err = d.exec(ctx, stmt)
	}
	if err != nil {
		return fmt.Errorf("gosmo: drop signature from %s in %q: %w", qualifiedName(schema, module), d.Name, err)
	}
	return nil
}
