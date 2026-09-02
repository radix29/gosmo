package gosmo

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// credential.go covers server-level credentials — sys.credentials, SSMS's
// Security > Credentials folder, and the identity a Login can be mapped to.
//
// # The secret is write-only
//
// sys.credentials never exposes the stored secret, and there is no read that
// does. A caller can set one and can clear one; it can never read one back,
// which is what shapes both AlterContext's signature and what a generated
// script can honestly emit.

// -- Credentials -----------------------------------------------------------------

// Credential mirrors a row from sys.credentials — a server-level credential,
// listed under Security > Credentials and offered in a Login's "Map to
// credential" dropdown.
//
// Get one from Server.Credential(name) or one of the reads below. A Credential
// built as a struct literal has no server behind it and will panic on Alter or
// Drop.
type Credential struct {
	server *Server

	CredentialID int
	Name         string
	Identity     string
	CreateDate   time.Time
	ModifyDate   time.Time

	// TargetType is what the credential is bound to: empty for an ordinary
	// credential (sys.credentials.target_type is NULL there) and
	// "CRYPTOGRAPHIC PROVIDER" for one created FOR CRYPTOGRAPHIC PROVIDER.
	TargetType string

	// CryptographicProvider is the provider's name, resolved through
	// sys.cryptographic_providers, and empty for an ordinary credential.
	// It can also be empty for a provider credential the connected login
	// cannot see the provider row for — TargetType is the reliable test of
	// which kind of credential this is.
	CryptographicProvider string
}

const credentialSelect = `
SELECT c.credential_id, c.name, c.credential_identity, c.create_date, c.modify_date,
       c.target_type, p.name
FROM   sys.credentials c
LEFT   JOIN sys.cryptographic_providers p
       ON  c.target_type = 'CRYPTOGRAPHIC PROVIDER' AND p.provider_id = c.target_id`

// Credentials returns every server-level credential.
func (s *Server) Credentials() ([]*Credential, error) {
	return s.CredentialsContext(context.Background())
}

// CredentialsContext is the context-aware variant of Credentials.
func (s *Server) CredentialsContext(ctx context.Context) ([]*Credential, error) {
	rows, err := s.query(ctx, credentialSelect+`
ORDER  BY c.name`)
	if err != nil {
		return nil, fmt.Errorf("gosmo: list credentials: %w", err)
	}
	defer rows.Close()

	var creds []*Credential
	for rows.Next() {
		c, err := scanCredential(s, rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("gosmo: list credentials: %w", err)
		}
		creds = append(creds, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("gosmo: list credentials: %w", err)
	}
	return creds, nil
}

// CredentialByName returns one credential with every field populated, or a
// not-found error (errors.Is ErrNotFound) when the server has none by that
// name.
func (s *Server) CredentialByName(name string) (*Credential, error) {
	return s.CredentialByNameContext(context.Background(), name)
}

// CredentialByNameContext is the context-aware variant of CredentialByName.
func (s *Server) CredentialByNameContext(ctx context.Context, name string) (*Credential, error) {
	var c *Credential
	err := s.queryRow(ctx, func(row *sql.Row) error {
		var err error
		c, err = scanCredential(s, row.Scan)
		return err
	}, credentialSelect+`
WHERE  c.name = @p1`, name)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, notFoundf("gosmo: credential %q not found", name)
	}
	if err != nil {
		return nil, fmt.Errorf("gosmo: read credential %q: %w", name, err)
	}
	return c, nil
}

// Credential returns a lightweight handle for a credential by name, without
// querying sys.credentials — the credential-side counterpart of
// Server.Database. Identity, CredentialID and every other cached field stay
// at their zero value; CredentialByName is what populates them.
//
// Every write method on *Credential addresses the credential by name, so this
// handle is enough to go on operating on one the caller already knows exists —
// and is the only usable form under a WithScript context, where
// CredentialByNameContext's lookup is a real read and a credential whose
// CREATE CREDENTIAL was merely collected is not there to find.
func (s *Server) Credential(name string) *Credential {
	return &Credential{server: s, Name: name}
}

func scanCredential(s *Server, scan func(...any) error) (*Credential, error) {
	c := &Credential{server: s}
	var identity, targetType, provider sql.NullString
	if err := scan(&c.CredentialID, &c.Name, &identity, &c.CreateDate, &c.ModifyDate,
		&targetType, &provider); err != nil {
		return nil, err
	}
	c.Identity = identity.String
	c.TargetType = targetType.String
	c.CryptographicProvider = provider.String
	return c, nil
}

// -- Writes ----------------------------------------------------------------------

// CredentialSpec describes a credential to create.
type CredentialSpec struct {
	Name string

	// Identity is the account the credential presents when connecting
	// outside the server. CREATE CREDENTIAL requires it.
	Identity string

	// Secret is the password half. Empty omits the SECRET clause, creating a
	// credential with a NULL secret — which is legitimate for an identity
	// that needs no password.
	Secret string

	// CryptographicProvider binds the credential to an EKM provider
	// (FOR CRYPTOGRAPHIC PROVIDER). Empty creates an ordinary credential.
	CryptographicProvider string
}

// createCredentialStatement builds CREATE CREDENTIAL, validating the spec.
func (spec CredentialSpec) createCredentialStatement() (string, error) {
	if strings.TrimSpace(spec.Name) == "" {
		return "", fmt.Errorf("credential has no name")
	}
	if spec.Identity == "" {
		return "", fmt.Errorf("credential %q has no identity", spec.Name)
	}

	stmt := fmt.Sprintf("CREATE CREDENTIAL %s WITH IDENTITY = N'%s'",
		quoteIdent(spec.Name), escapeSingle(spec.Identity))
	if spec.Secret != "" {
		stmt += fmt.Sprintf(", SECRET = N'%s'", escapeSingle(spec.Secret))
	}
	// FOR CRYPTOGRAPHIC PROVIDER follows the whole WITH clause; it is not
	// another comma-separated option inside it.
	if spec.CryptographicProvider != "" {
		stmt += " FOR CRYPTOGRAPHIC PROVIDER " + quoteIdent(spec.CryptographicProvider)
	}
	return stmt, nil
}

// CreateCredential creates a server-level credential.
func (s *Server) CreateCredential(spec CredentialSpec) (*Credential, error) {
	return s.CreateCredentialContext(context.Background(), spec)
}

// CreateCredentialContext is the context-aware variant of CreateCredential.
func (s *Server) CreateCredentialContext(ctx context.Context, spec CredentialSpec) (*Credential, error) {
	stmt, err := spec.createCredentialStatement()
	if err != nil {
		return nil, fmt.Errorf("gosmo: create credential: %w", err)
	}
	if err := s.execContext(ctx, stmt); err != nil {
		return nil, fmt.Errorf("gosmo: create credential %q: %w", spec.Name, err)
	}
	if Scripting(ctx) {
		// The CREATE was only collected, so there is nothing to read back.
		return s.Credential(spec.Name), nil
	}
	return s.CredentialByNameContext(ctx, spec.Name)
}

// Alter changes the credential's identity, and its secret.
func (c *Credential) Alter(identity string, secret *string) error {
	return c.AlterContext(context.Background(), identity, secret)
}

// AlterContext is the context-aware variant of Alter.
//
// A nil secret does not leave the stored secret alone — it clears it.
// ALTER CREDENTIAL resets both halves every time, and SQL Server documents
// omitting SECRET as setting the stored secret to NULL; there is no T-SQL
// form that changes the identity while keeping the secret. Since the secret
// can never be read back, a caller that wants to keep one has to ask the user
// for it again and pass it here. Both branches are deliberate: pass a pointer
// to the new secret to set it, and nil only when clearing it is the intent.
func (c *Credential) AlterContext(ctx context.Context, identity string, secret *string) error {
	if identity == "" {
		return fmt.Errorf("gosmo: alter credential %q: identity is required", c.Name)
	}
	stmt := fmt.Sprintf("ALTER CREDENTIAL %s WITH IDENTITY = N'%s'",
		quoteIdent(c.Name), escapeSingle(identity))
	if secret != nil {
		stmt += fmt.Sprintf(", SECRET = N'%s'", escapeSingle(*secret))
	}
	if err := c.server.execContext(ctx, stmt); err != nil {
		return fmt.Errorf("gosmo: alter credential %q: %w", c.Name, err)
	}
	setIfApplied(ctx, &c.Identity, identity)
	return nil
}

// Drop deletes the credential.
func (c *Credential) Drop() error { return c.DropContext(context.Background()) }

// DropContext is the context-aware variant of Drop.
func (c *Credential) DropContext(ctx context.Context) error {
	if err := c.server.execContext(ctx, "DROP CREDENTIAL "+quoteIdent(c.Name)); err != nil {
		return fmt.Errorf("gosmo: drop credential %q: %w", c.Name, err)
	}
	return nil
}

// -- Cryptographic providers -----------------------------------------------------

// CryptographicProvider mirrors a row of sys.cryptographic_providers — an
// Extensible Key Management provider registered with CREATE CRYPTOGRAPHIC
// PROVIDER. It lives here rather than in a file of its own because a
// credential's FOR CRYPTOGRAPHIC PROVIDER binding is the only thing in gosmo
// that refers to one.
type CryptographicProvider struct {
	ProviderID int
	Name       string
	GUID       string
	Version    string
	DLLPath    string
	IsEnabled  bool
}

// CryptographicProviders returns every registered EKM provider.
func (s *Server) CryptographicProviders() ([]*CryptographicProvider, error) {
	return s.CryptographicProvidersContext(context.Background())
}

// CryptographicProvidersContext is the context-aware variant of
// CryptographicProviders. A server with no provider registered — the ordinary
// case — returns no rows, not an error.
func (s *Server) CryptographicProvidersContext(ctx context.Context) ([]*CryptographicProvider, error) {
	const q = `
SELECT provider_id, name,
       -- A uniqueidentifier reaches the driver as 16 raw bytes; converted here
       -- so it scans as the text form callers expect. Same shape as
       -- availability_group.go's group_id reads.
       CONVERT(varchar(36), guid), version, dll_path, is_enabled
FROM   sys.cryptographic_providers
ORDER  BY name`

	rows, err := s.query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("gosmo: list cryptographic providers: %w", err)
	}
	defer rows.Close()

	var out []*CryptographicProvider
	for rows.Next() {
		p := &CryptographicProvider{}
		var guid, version, dllPath sql.NullString
		if err := rows.Scan(&p.ProviderID, &p.Name, &guid, &version, &dllPath, &p.IsEnabled); err != nil {
			return nil, fmt.Errorf("gosmo: list cryptographic providers: %w", err)
		}
		p.GUID, p.Version, p.DLLPath = guid.String, version.String, dllPath.String
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("gosmo: list cryptographic providers: %w", err)
	}
	return out, nil
}
