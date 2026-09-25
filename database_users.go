package gosmo

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// -- Database users ------------------------------------------------------------

// userTypes is every sys.database_principals type that is a user rather than
// a role: SQL, Windows user and group, Entra user and group ('E','X'), and
// the certificate- and asymmetric-key-mapped users ('C','K'). Listing only
// the first three left the rest out of the Users folder and made Script as
// report them not found — every FROM EXTERNAL PROVIDER user on Managed
// Instance among them.
const userTypes = `('S','U','G','E','X','C','K')`

// Users returns all database users.
func (d *Database) Users(ctx context.Context) ([]*User, error) {
	const q = `
SELECT name, principal_id, type_desc, default_schema_name,
       create_date, modify_date, authentication_type_desc
FROM   sys.database_principals
WHERE  type IN ` + userTypes + `
ORDER  BY name`

	rows, err := d.query(ctx, q)
	return scanRows(rows, err, fmt.Sprintf("list users in %q", d.Name), func(scan func(...any) error) (*User, error) {
		u := &User{db: d}
		var defSchema, authType sql.NullString
		if err := scan(&u.Name, &u.ID, &u.UserType, &defSchema,
			&u.CreateDate, &u.ModifyDate, &authType); err != nil {
			return nil, err
		}
		u.DefaultSchema = defSchema.String
		u.AuthType = authType.String
		return u, nil
	})
}

// UserByName returns a single database user by name, with its SID, matching
// server login (if any) and mapped certificate or asymmetric key filled in —
// Users leaves these out since Object Explorer's tree listing never needs
// them.
func (d *Database) UserByName(ctx context.Context, name string) (*User, error) {
	const q = `
SELECT dp.principal_id, dp.type_desc, dp.default_schema_name,
       dp.create_date, dp.modify_date, dp.authentication_type_desc, dp.sid,
       sp.name, sp.is_disabled,
       CASE dp.type WHEN 'C' THEN (SELECT TOP 1 c.name  FROM sys.certificates    c  WHERE c.sid  = dp.sid)
                    WHEN 'K' THEN (SELECT TOP 1 ak.name FROM sys.asymmetric_keys ak WHERE ak.sid = dp.sid)
       END
FROM   sys.database_principals dp
LEFT   JOIN sys.server_principals sp ON sp.sid = dp.sid
WHERE  dp.type IN ` + userTypes + ` AND dp.name = @p1`

	u := &User{db: d, Name: name}
	var defSchema, authType, loginName, mapped sql.NullString
	var loginDisabled sql.NullBool
	err := d.queryRow(ctx, func(row *sql.Row) error {
		return row.Scan(&u.ID, &u.UserType, &defSchema, &u.CreateDate, &u.ModifyDate,
			&authType, &u.SID, &loginName, &loginDisabled, &mapped)
	}, q, name)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, notFoundf("gosmo: database user %q not found in %q", name, d.Name)
		}
		return nil, fmt.Errorf("gosmo: find database user %q in %q: %w", name, d.Name, err)
	}
	u.DefaultSchema = defSchema.String
	u.AuthType = authType.String
	u.LoginName = loginName.String
	u.LoginDisabled = loginDisabled.Bool
	u.MappedObject = mapped.String
	return u, nil
}

// UserRef returns a lightweight handle for name without querying the server
// at all — unlike UserByName, it doesn't verify the user
// exists or populate ID/UserType/DefaultSchema/AuthType/SID/LoginName/etc.
// (they stay at their zero value). Every write method on *User (Drop,
// Rename, SetDefaultSchema, SetLogin, AddToRole,
// Grant, ...) only ever needs the user's name, never those cached
// fields, so this is sufficient for issuing further ALTER-style calls against
// a user the caller already knows exists — most commonly one it just created
// in the same operation. See Server.DatabaseRef's doc comment for why this
// also matters under a WithScript-derived context.
func (d *Database) UserRef(name string) *User {
	return &User{db: d, Name: name}
}

// UserKind selects the CREATE USER form a CreateUserRequest issues.
type UserKind int

const (
	// UserForLogin is a user mapped to a server login (FOR LOGIN). It is the
	// zero value.
	UserForLogin UserKind = iota
	// UserWithoutLogin is a user nobody connects as (WITHOUT LOGIN) — for
	// impersonation, or to own objects and hold permissions.
	UserWithoutLogin
	// UserWithPassword is a contained database user that authenticates with
	// its own password (WITH PASSWORD). The database must be partially
	// contained, except on Azure SQL Database, where every database is.
	UserWithPassword
	// UserWindows is a Windows user or group, named DOMAIN\name. With a
	// Login it is FOR LOGIN; without one it is the bare statement, a
	// contained database's Windows user.
	UserWindows
	// UserFromCertificate maps the user to a certificate in the database
	// (FROM CERTIFICATE), to hold permissions for code signed by it.
	UserFromCertificate
	// UserFromAsymmetricKey is UserFromCertificate for an asymmetric key.
	UserFromAsymmetricKey
	// UserFromExternalProvider is a Microsoft Entra user or group
	// (FROM EXTERNAL PROVIDER) — Azure SQL Database, Managed Instance and
	// SQL Server 2022 and later.
	UserFromExternalProvider
)

// String renders the kind as the words used in error messages.
func (k UserKind) String() string {
	switch k {
	case UserForLogin:
		return "login-mapped"
	case UserWithoutLogin:
		return "login-less"
	case UserWithPassword:
		return "contained"
	case UserWindows:
		return "Windows"
	case UserFromCertificate:
		return "certificate-mapped"
	case UserFromAsymmetricKey:
		return "asymmetric-key-mapped"
	case UserFromExternalProvider:
		return "external provider"
	}
	return fmt.Sprintf("UserKind(%d)", int(k))
}

// CreateUserRequest describes a new database user. Kind selects the form, and
// each of the other fields is accepted only by the kinds that use it — a field
// set for a kind that has no clause for it is refused rather than dropped.
type CreateUserRequest struct {
	Name string
	Kind UserKind
	// Login is required for UserForLogin and optional for UserWindows.
	Login string
	// Password is required for UserWithPassword, and refused otherwise.
	Password string
	// Certificate / AsymmetricKey name the object a mapped user maps to.
	Certificate   string
	AsymmetricKey string
	// ObjectID is the Entra object id, for UserFromExternalProvider only.
	ObjectID string
	// DefaultSchema is refused for the certificate- and key-mapped kinds,
	// which SQL Server refuses it for too.
	DefaultSchema string
}

// CreateUser creates a database user of any kind — see UserKind.
//
// A contained user's password goes through QuoteLiteral, as CreateLogin's
// does. Asking for one in a database that is not partially contained is
// refused here, after a read of sys.databases: SQL Server's own refusal
// (Msg 33233) says "only in a contained database" without naming the setting
// or the database.
func (d *Database) CreateUser(ctx context.Context, req CreateUserRequest) (*User, error) {
	stmt, err := createUserStatement(req)
	if err != nil {
		if req.Name == "" {
			return nil, fmt.Errorf("gosmo: create user: %w", err)
		}
		return nil, fmt.Errorf("gosmo: create user %q: %w", req.Name, err)
	}
	if req.Kind == UserWithPassword && !d.server.everyDatabaseContained() {
		var containment string
		if err := d.server.queryRowScan(ctx,
			"SELECT containment_desc FROM sys.databases WHERE name = @p1",
			[]any{d.Name}, &containment); err != nil {
			return nil, fmt.Errorf("gosmo: create user %q: read containment of %q: %w", req.Name, d.Name, err)
		}
		if containment != "PARTIAL" {
			return nil, fmt.Errorf("gosmo: create user %q: a user with a password needs a contained database, and %q has CONTAINMENT = %s",
				req.Name, d.Name, containment)
		}
	}
	if _, err := d.exec(ctx, stmt); err != nil {
		return nil, fmt.Errorf("gosmo: create user %q: %w", req.Name, err)
	}
	return createdObject(ctx, d.UserRef(req.Name), func() (*User, error) {
		return d.UserByName(ctx, req.Name)
	})
}

// everyDatabaseContained reports whether the engine takes contained users in
// any database without CONTAINMENT = PARTIAL — Azure SQL Database does, and
// reports containment NONE for all of them.
func (s *Server) everyDatabaseContained() bool {
	return s.info != nil && EngineEdition(s.info.EngineEdition) == EngineAzureSQLDatabase
}

// createUserStatement validates req and builds its CREATE USER statement.
func createUserStatement(req CreateUserRequest) (string, error) {
	if req.Name == "" {
		return "", fmt.Errorf("user name is required")
	}
	k := req.Kind
	// Each optional field against the kinds that have a clause for it.
	switch {
	case req.Login != "" && k != UserForLogin && k != UserWindows:
		return "", fmt.Errorf("a %s user takes no login", k)
	case req.Password != "" && k != UserWithPassword:
		return "", fmt.Errorf("a %s user takes no password", k)
	case req.Certificate != "" && k != UserFromCertificate:
		return "", fmt.Errorf("a %s user maps to no certificate", k)
	case req.AsymmetricKey != "" && k != UserFromAsymmetricKey:
		return "", fmt.Errorf("a %s user maps to no asymmetric key", k)
	case req.ObjectID != "" && k != UserFromExternalProvider:
		return "", fmt.Errorf("ObjectID applies to an external provider user only, not a %s user", k)
	case req.DefaultSchema != "" && (k == UserFromCertificate || k == UserFromAsymmetricKey):
		return "", fmt.Errorf("a %s user cannot have a default schema", k)
	}

	var opts []string
	stmt := "CREATE USER " + quoteIdent(req.Name)
	switch k {
	case UserForLogin:
		// Without this, quoteIdent("") turns an empty login into "FOR LOGIN
		// []" — a statement the server rejects with a message naming an empty
		// login the caller never typed.
		if req.Login == "" {
			return "", fmt.Errorf("login name is required")
		}
		stmt += " FOR LOGIN " + quoteIdent(req.Login)
	case UserWithoutLogin:
		stmt += " WITHOUT LOGIN"
	case UserWithPassword:
		if req.Password == "" {
			return "", fmt.Errorf("a contained user requires a password")
		}
		opts = append(opts, "PASSWORD = "+QuoteLiteral(req.Password))
	case UserWindows:
		if req.Login != "" {
			stmt += " FOR LOGIN " + quoteIdent(req.Login)
		}
	case UserFromCertificate:
		if req.Certificate == "" {
			return "", fmt.Errorf("a certificate-mapped user requires Certificate")
		}
		stmt += " FROM CERTIFICATE " + quoteIdent(req.Certificate)
	case UserFromAsymmetricKey:
		if req.AsymmetricKey == "" {
			return "", fmt.Errorf("an asymmetric-key-mapped user requires AsymmetricKey")
		}
		stmt += " FROM ASYMMETRIC KEY " + quoteIdent(req.AsymmetricKey)
	case UserFromExternalProvider:
		stmt += " FROM EXTERNAL PROVIDER"
		if req.ObjectID != "" {
			opts = append(opts, "OBJECT_ID = "+QuoteLiteral(req.ObjectID))
		}
	default:
		return "", fmt.Errorf("unknown user kind %s", k)
	}
	if req.DefaultSchema != "" {
		opts = append(opts, "DEFAULT_SCHEMA = "+quoteIdent(req.DefaultSchema))
	}
	if len(opts) > 0 {
		stmt += " WITH " + strings.Join(opts, ", ")
	}
	return stmt, nil
}
