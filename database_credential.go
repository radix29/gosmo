package gosmo

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// database_credential.go covers database-scoped credentials —
// sys.database_scoped_credentials, SSMS's <database> > Security > Database
// Scoped Credentials folder, and the credential an external data source or a
// BULK INSERT ... FROM URL binds to. It is the database-scope counterpart of
// credential.go's server-level Credential.
//
// # The secret is write-only
//
// sys.database_scoped_credentials never exposes the stored secret, and there
// is no read that does — exactly as sys.credentials does not. A caller can set
// one and can clear one; it can never read one back, which is what shapes both
// AlterContext's signature and what a generated script can honestly emit: the
// script carries a placeholder, not the secret.
//
// # It is not a credential with a different WHERE clause
//
// The two families are separate securables with separate DDL. There is no
// FOR CRYPTOGRAPHIC PROVIDER form of a database-scoped credential, so nothing
// here mirrors Credential.TargetType, and the statements are
// CREATE/ALTER/DROP DATABASE SCOPED CREDENTIAL, which the server accepts only
// in the database the credential lives in — every write here goes through
// Database.exec, which issues that USE.

// -- Database-scoped credentials --------------------------------------------------

// DatabaseScopedCredential mirrors a row from sys.database_scoped_credentials.
//
// Get one from Database.DatabaseScopedCredential(name) or one of the reads
// below. A DatabaseScopedCredential built as a struct literal has no database
// behind it and will panic on Alter or Drop.
type DatabaseScopedCredential struct {
	db *Database

	CredentialID int
	Name         string
	Identity     string
	CreateDate   time.Time
	ModifyDate   time.Time
}

const databaseScopedCredentialSelect = `
SELECT c.credential_id, c.name, c.credential_identity, c.create_date, c.modify_date
FROM   sys.database_scoped_credentials c`

// DatabaseScopedCredentials returns every database-scoped credential in the
// database.
func (d *Database) DatabaseScopedCredentials() ([]*DatabaseScopedCredential, error) {
	return d.DatabaseScopedCredentialsContext(context.Background())
}

// DatabaseScopedCredentialsContext is the context-aware variant of
// DatabaseScopedCredentials.
func (d *Database) DatabaseScopedCredentialsContext(ctx context.Context) ([]*DatabaseScopedCredential, error) {
	rows, err := d.query(ctx, databaseScopedCredentialSelect+`
ORDER  BY c.name`)
	if err != nil {
		return nil, fmt.Errorf("gosmo: list database scoped credentials in %q: %w", d.name, err)
	}
	defer rows.Close()

	var creds []*DatabaseScopedCredential
	for rows.Next() {
		c, err := scanDatabaseScopedCredential(d, rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("gosmo: list database scoped credentials in %q: %w", d.name, err)
		}
		creds = append(creds, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("gosmo: list database scoped credentials in %q: %w", d.name, err)
	}
	return creds, nil
}

// DatabaseScopedCredentialByName returns one credential with every field
// populated, or a not-found error (errors.Is ErrNotFound) when the database
// has none by that name.
func (d *Database) DatabaseScopedCredentialByName(name string) (*DatabaseScopedCredential, error) {
	return d.DatabaseScopedCredentialByNameContext(context.Background(), name)
}

// DatabaseScopedCredentialByNameContext is the context-aware variant of
// DatabaseScopedCredentialByName.
func (d *Database) DatabaseScopedCredentialByNameContext(ctx context.Context, name string) (*DatabaseScopedCredential, error) {
	var c *DatabaseScopedCredential
	err := d.queryRow(ctx, func(row *sql.Row) error {
		var err error
		c, err = scanDatabaseScopedCredential(d, row.Scan)
		return err
	}, databaseScopedCredentialSelect+`
WHERE  c.name = @p1`, name)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, notFoundf("gosmo: database scoped credential %q not found in %q", name, d.name)
	}
	if err != nil {
		return nil, fmt.Errorf("gosmo: read database scoped credential %q in %q: %w", name, d.name, err)
	}
	return c, nil
}

// DatabaseScopedCredential returns a lightweight handle for a credential by
// name, without querying the catalog — the counterpart of Server.Database.
// Identity, CredentialID and every other cached field stay at their zero
// value; DatabaseScopedCredentialByName is what populates them.
//
// Every write method on *DatabaseScopedCredential addresses the credential by
// name, so this handle is enough to go on operating on one the caller already
// knows exists — and is the only usable form under a WithScript-derived
// context, where DatabaseScopedCredentialByNameContext's lookup is a real read
// and a credential whose CREATE was merely collected is not there to find.
func (d *Database) DatabaseScopedCredential(name string) *DatabaseScopedCredential {
	return &DatabaseScopedCredential{db: d, Name: name}
}

// Database returns the database the credential lives in.
func (c *DatabaseScopedCredential) Database() *Database { return c.db }

func scanDatabaseScopedCredential(d *Database, scan func(...any) error) (*DatabaseScopedCredential, error) {
	c := &DatabaseScopedCredential{db: d}
	var identity sql.NullString
	if err := scan(&c.CredentialID, &c.Name, &identity, &c.CreateDate, &c.ModifyDate); err != nil {
		return nil, err
	}
	c.Identity = identity.String
	return c, nil
}

// -- Writes ----------------------------------------------------------------------

// DatabaseScopedCredentialSpec describes a database-scoped credential to
// create.
type DatabaseScopedCredentialSpec struct {
	Name string

	// Identity is the account the credential presents when the database
	// reaches outside itself. CREATE DATABASE SCOPED CREDENTIAL requires it.
	Identity string

	// Secret is the password half — a shared access signature, a storage key
	// or a password, depending on what the identity means. Empty omits the
	// SECRET clause, creating a credential with a NULL secret, which is
	// legitimate for an identity that needs no password (Managed Identity is
	// the usual case).
	Secret string
}

// createDatabaseScopedCredentialStatement builds CREATE DATABASE SCOPED
// CREDENTIAL, validating the spec.
func (spec DatabaseScopedCredentialSpec) createDatabaseScopedCredentialStatement() (string, error) {
	if strings.TrimSpace(spec.Name) == "" {
		return "", fmt.Errorf("database scoped credential has no name")
	}
	if spec.Identity == "" {
		return "", fmt.Errorf("database scoped credential %q has no identity", spec.Name)
	}

	stmt := fmt.Sprintf("CREATE DATABASE SCOPED CREDENTIAL %s WITH IDENTITY = N'%s'",
		quoteIdent(spec.Name), escapeSingle(spec.Identity))
	if spec.Secret != "" {
		stmt += fmt.Sprintf(", SECRET = N'%s'", escapeSingle(spec.Secret))
	}
	return stmt, nil
}

// CreateDatabaseScopedCredential creates a database-scoped credential.
func (d *Database) CreateDatabaseScopedCredential(spec DatabaseScopedCredentialSpec) (*DatabaseScopedCredential, error) {
	return d.CreateDatabaseScopedCredentialContext(context.Background(), spec)
}

// CreateDatabaseScopedCredentialContext is the context-aware variant of
// CreateDatabaseScopedCredential.
func (d *Database) CreateDatabaseScopedCredentialContext(ctx context.Context, spec DatabaseScopedCredentialSpec) (*DatabaseScopedCredential, error) {
	stmt, err := spec.createDatabaseScopedCredentialStatement()
	if err != nil {
		return nil, fmt.Errorf("gosmo: create database scoped credential: %w", err)
	}
	if _, err := d.exec(ctx, stmt); err != nil {
		return nil, fmt.Errorf("gosmo: create database scoped credential %q in %q: %w", spec.Name, d.name, err)
	}
	if Scripting(ctx) {
		// The CREATE was only collected, so there is nothing to read back.
		return d.DatabaseScopedCredential(spec.Name), nil
	}
	return d.DatabaseScopedCredentialByNameContext(ctx, spec.Name)
}

// Alter changes the credential's identity, and its secret.
func (c *DatabaseScopedCredential) Alter(identity string, secret *string) error {
	return c.AlterContext(context.Background(), identity, secret)
}

// AlterContext is the context-aware variant of Alter.
//
// A nil secret does not leave the stored secret alone — it clears it, for the
// same reason Credential.AlterContext's does. ALTER DATABASE SCOPED CREDENTIAL
// resets both halves every time and an omitted SECRET sets the stored secret
// to NULL; there is no T-SQL form that changes the identity while keeping the
// secret. Since the secret can never be read back, a caller that wants to keep
// one has to ask the user for it again and pass it here. Both branches are
// deliberate: pass a pointer to the new secret to set it, and nil only when
// clearing it is the intent.
func (c *DatabaseScopedCredential) AlterContext(ctx context.Context, identity string, secret *string) error {
	if identity == "" {
		return fmt.Errorf("gosmo: alter database scoped credential %q: identity is required", c.Name)
	}
	stmt := fmt.Sprintf("ALTER DATABASE SCOPED CREDENTIAL %s WITH IDENTITY = N'%s'",
		quoteIdent(c.Name), escapeSingle(identity))
	if secret != nil {
		stmt += fmt.Sprintf(", SECRET = N'%s'", escapeSingle(*secret))
	}
	if _, err := c.db.exec(ctx, stmt); err != nil {
		return fmt.Errorf("gosmo: alter database scoped credential %q in %q: %w", c.Name, c.db.name, err)
	}
	setIfApplied(ctx, &c.Identity, identity)
	return nil
}

// Drop deletes the credential.
func (c *DatabaseScopedCredential) Drop() error { return c.DropContext(context.Background()) }

// DropContext is the context-aware variant of Drop. No IF EXISTS: dropping
// one that isn't there reaches the caller as the server's error, the way every
// other Drop* in this package does.
func (c *DatabaseScopedCredential) DropContext(ctx context.Context) error {
	if _, err := c.db.exec(ctx, "DROP DATABASE SCOPED CREDENTIAL "+quoteIdent(c.Name)); err != nil {
		return fmt.Errorf("gosmo: drop database scoped credential %q in %q: %w", c.Name, c.db.name, err)
	}
	return nil
}
