package gosmo

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Login represents a SQL Server server-level login.
type Login struct {
	server *Server
	Name   string
	SID    []byte
	// LoginType is the login's type_desc: "SQL_LOGIN", "WINDOWS_LOGIN",
	// "WINDOWS_GROUP", "EXTERNAL_LOGIN", "EXTERNAL_GROUP",
	// "CERTIFICATE_MAPPED_LOGIN" or "ASYMMETRIC_KEY_MAPPED_LOGIN".
	LoginType       string
	IsDisabled      bool
	DefaultDatabase string
	CreateDate      time.Time
	ModifyDate      time.Time

	// MappedObject is the master certificate or asymmetric key a
	// CERTIFICATE_MAPPED_LOGIN / ASYMMETRIC_KEY_MAPPED_LOGIN maps to. It is
	// not read with the login — the name lives in master, not in
	// sys.server_principals — so it is empty until ResolveMapping fills it.
	MappedObject string
}

// Server returns the server the login belongs to.
func (l *Login) Server() *Server { return l.server }

// ResolveMapping looks up the certificate or asymmetric key this login maps
// to and stores its name in MappedObject.
func (l *Login) ResolveMapping() error {
	return l.ResolveMappingContext(context.Background())
}

// ResolveMappingContext is the context-aware variant of ResolveMapping.
//
// It is a no-op for every login type but CERTIFICATE_MAPPED_LOGIN and
// ASYMMETRIC_KEY_MAPPED_LOGIN. The lookup is by SID against master, where a
// login-mapped certificate or asymmetric key must live, and names master
// explicitly because the connection may be in any database. MappedObject is
// left empty, without an error, when nothing matches — the mapped object can
// have been dropped out from under the login.
func (l *Login) ResolveMappingContext(ctx context.Context) error {
	switch l.LoginType {
	case "CERTIFICATE_MAPPED_LOGIN", "ASYMMETRIC_KEY_MAPPED_LOGIN":
	default:
		return nil
	}
	const q = `
SELECT ISNULL((SELECT TOP 1 name FROM master.sys.certificates    WHERE sid = @p1),
       ISNULL((SELECT TOP 1 name FROM master.sys.asymmetric_keys WHERE sid = @p1), ''))`

	var name string
	if err := l.server.queryRowScan(ctx, q, []any{l.SID}, &name); err != nil {
		return fmt.Errorf("gosmo: resolve mapping for login %q: %w", l.Name, err)
	}
	l.MappedObject = name
	return nil
}

// Disable disables the login.
func (l *Login) Disable() error {
	return l.DisableContext(context.Background())
}

// DisableContext is the context-aware variant of Disable.
func (l *Login) DisableContext(ctx context.Context) error {
	if err := l.server.execContext(ctx, "ALTER LOGIN "+quoteIdent(l.Name)+" DISABLE"); err != nil {
		return fmt.Errorf("gosmo: disable login %q: %w", l.Name, err)
	}
	setIfApplied(ctx, &l.IsDisabled, true)
	return nil
}

// Enable enables the login.
func (l *Login) Enable() error {
	return l.EnableContext(context.Background())
}

// EnableContext is the context-aware variant of Enable.
func (l *Login) EnableContext(ctx context.Context) error {
	if err := l.server.execContext(ctx, "ALTER LOGIN "+quoteIdent(l.Name)+" ENABLE"); err != nil {
		return fmt.Errorf("gosmo: enable login %q: %w", l.Name, err)
	}
	setIfApplied(ctx, &l.IsDisabled, false)
	return nil
}

// ChangePassword changes the login's password.
func (l *Login) ChangePassword(newPassword string) error {
	return l.ChangePasswordContext(context.Background(), newPassword)
}

// ChangePasswordContext changes the login's password.
//
// Security: the password is quoted via nStringLiteral (N'...', doubling
// any embedded quote) rather than interpolated raw. HASHED is
// deliberately not used — it tells SQL Server the value is already one of
// its own password-hash formats, not cleartext, so passing a hex encoding
// of the cleartext under HASHED either fails outright or creates a login
// nothing can ever authenticate as.
func (l *Login) ChangePasswordContext(ctx context.Context, newPassword string) error {
	q := fmt.Sprintf("ALTER LOGIN %s WITH PASSWORD = %s", quoteIdent(l.Name), nStringLiteral(newPassword))
	if err := l.server.execContext(ctx, q); err != nil {
		return fmt.Errorf("gosmo: change password for login %q: %w", l.Name, err)
	}
	return nil
}

// Drop drops the login from the server.
func (l *Login) Drop() error { return l.DropContext(context.Background()) }

// DropContext is the context-aware variant of Drop.
func (l *Login) DropContext(ctx context.Context) error {
	return l.server.DropLoginContext(ctx, l.Name)
}

// AddServerRoleMember adds this login to a server role.
func (l *Login) AddServerRoleMember(roleName string) error {
	return l.AddServerRoleMemberContext(context.Background(), roleName)
}

// AddServerRoleMemberContext is the context-aware variant of AddServerRoleMember.
func (l *Login) AddServerRoleMemberContext(ctx context.Context, roleName string) error {
	return l.server.AddServerRoleMemberContext(ctx, roleName, l.Name)
}

// RemoveServerRoleMember removes this login from a server role.
func (l *Login) RemoveServerRoleMember(roleName string) error {
	return l.RemoveServerRoleMemberContext(context.Background(), roleName)
}

// RemoveServerRoleMemberContext is the context-aware variant of RemoveServerRoleMember.
func (l *Login) RemoveServerRoleMemberContext(ctx context.Context, roleName string) error {
	return l.server.RemoveServerRoleMemberContext(ctx, roleName, l.Name)
}

// -- Status / details --------------------------------------------------------

// LoginDetails holds the login's password-policy and status fields —
// SSMS's Login Properties > Status page (plus the policy checkboxes and
// credential mapping shown on the General page). Windows logins have no
// password policy; those fields read as their zero value rather than
// erroring, since LOGINPROPERTY simply returns NULL for them.
type LoginDetails struct {
	IsLocked            bool
	IsExpired           bool
	MustChangePassword  bool
	IsPolicyChecked     bool
	IsExpirationChecked bool
	PasswordLastSet     time.Time
	// LastLogin is best-effort: it reflects the most recent session found
	// in sys.dm_exec_sessions, which only holds currently-connected (or
	// very recently disconnected) sessions, not full login history. It is
	// the zero Time if no matching session is currently visible.
	LastLogin        time.Time
	BadPasswordCount int
	// BadPasswordTime is the last failed-login attempt time, or the zero
	// Time if none is recorded.
	BadPasswordTime time.Time
	DefaultLanguage string
	CredentialName  string
	// ConnectSQLState is "GRANT", "DENY", or "" (default/unset) for the
	// login's explicit CONNECT SQL server permission.
	ConnectSQLState string
}

// Details returns the login's password-policy and status information.
func (l *Login) Details() (*LoginDetails, error) {
	return l.DetailsContext(context.Background())
}

// DetailsContext is the context-aware variant of Details.
func (l *Login) DetailsContext(ctx context.Context) (*LoginDetails, error) {
	const q = `
SELECT
    ISNULL(CAST(LOGINPROPERTY(@p1, 'IsLocked')     AS INT), 0),
    ISNULL(CAST(LOGINPROPERTY(@p1, 'IsExpired')    AS INT), 0),
    ISNULL(CAST(LOGINPROPERTY(@p1, 'IsMustChange') AS INT), 0),
    ISNULL(sl.is_policy_checked, 0),
    ISNULL(sl.is_expiration_checked, 0),
    CAST(LOGINPROPERTY(@p1, 'PasswordLastSetTime') AS DATETIME2),
    (SELECT MAX(login_time) FROM sys.dm_exec_sessions WHERE login_name = @p1),
    ISNULL(CAST(LOGINPROPERTY(@p1, 'BadPasswordCount') AS INT), 0),
    CAST(LOGINPROPERTY(@p1, 'BadPasswordTime') AS DATETIME2),
    ISNULL(sl.default_language_name, ''),
    ISNULL(cr.name, ''),
    ISNULL((SELECT TOP 1 perm.state_desc FROM sys.server_permissions perm
            WHERE perm.grantee_principal_id = sp.principal_id AND perm.permission_name = 'CONNECT SQL'), '')
FROM   sys.server_principals sp
LEFT   JOIN sys.sql_logins sl ON sl.principal_id = sp.principal_id
LEFT   JOIN sys.credentials cr ON cr.credential_id = sl.credential_id
WHERE  sp.name = @p1`

	det := &LoginDetails{}
	var isLocked, isExpired, isMustChange int
	var pwdLastSet, lastLogin, badPasswordTime sql.NullTime

	err := l.server.queryRow(ctx, func(row *sql.Row) error {
		return row.Scan(
			&isLocked, &isExpired, &isMustChange,
			&det.IsPolicyChecked, &det.IsExpirationChecked,
			&pwdLastSet, &lastLogin, &det.BadPasswordCount, &badPasswordTime,
			&det.DefaultLanguage, &det.CredentialName, &det.ConnectSQLState,
		)
	}, q, l.Name)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, notFoundf("gosmo: login %q not found", l.Name)
		}
		return nil, fmt.Errorf("gosmo: login details for %q: %w", l.Name, err)
	}
	det.IsLocked = isLocked != 0
	det.IsExpired = isExpired != 0
	det.MustChangePassword = isMustChange != 0
	det.PasswordLastSet = pwdLastSet.Time
	det.LastLogin = lastLogin.Time
	det.BadPasswordTime = badPasswordTime.Time
	return det, nil
}

// Rename changes the login's name.
func (l *Login) Rename(newName string) error {
	return l.RenameContext(context.Background(), newName)
}

// RenameContext is the context-aware variant of Rename.
func (l *Login) RenameContext(ctx context.Context, newName string) error {
	q := fmt.Sprintf("ALTER LOGIN %s WITH NAME = %s", quoteIdent(l.Name), quoteIdent(newName))
	if err := l.server.execContext(ctx, q); err != nil {
		return fmt.Errorf("gosmo: rename login %q to %q: %w", l.Name, newName, err)
	}
	setIfApplied(ctx, &l.Name, newName)
	return nil
}

// SetDefaultDatabase changes the login's default database.
func (l *Login) SetDefaultDatabase(name string) error {
	return l.SetDefaultDatabaseContext(context.Background(), name)
}

// SetDefaultDatabaseContext is the context-aware variant of SetDefaultDatabase.
func (l *Login) SetDefaultDatabaseContext(ctx context.Context, name string) error {
	q := fmt.Sprintf("ALTER LOGIN %s WITH DEFAULT_DATABASE = %s", quoteIdent(l.Name), quoteIdent(name))
	if err := l.server.execContext(ctx, q); err != nil {
		return fmt.Errorf("gosmo: set default database for login %q to %q: %w", l.Name, name, err)
	}
	setIfApplied(ctx, &l.DefaultDatabase, name)
	return nil
}

// SetDefaultLanguage changes the login's default language.
func (l *Login) SetDefaultLanguage(lang string) error {
	return l.SetDefaultLanguageContext(context.Background(), lang)
}

// SetDefaultLanguageContext is the context-aware variant of SetDefaultLanguage.
func (l *Login) SetDefaultLanguageContext(ctx context.Context, lang string) error {
	q := fmt.Sprintf("ALTER LOGIN %s WITH DEFAULT_LANGUAGE = %s", quoteIdent(l.Name), quoteIdent(lang))
	if err := l.server.execContext(ctx, q); err != nil {
		return fmt.Errorf("gosmo: set default language for login %q to %q: %w", l.Name, lang, err)
	}
	return nil
}

// SetPasswordPolicy sets the login's CHECK_POLICY and CHECK_EXPIRATION
// flags. SQL Server rejects checkExpiration=true with checkPolicy=false —
// surfaced as the returned error, not pre-validated here.
func (l *Login) SetPasswordPolicy(checkPolicy, checkExpiration bool) error {
	return l.SetPasswordPolicyContext(context.Background(), checkPolicy, checkExpiration)
}

// SetPasswordPolicyContext is the context-aware variant of SetPasswordPolicy.
func (l *Login) SetPasswordPolicyContext(ctx context.Context, checkPolicy, checkExpiration bool) error {
	policy, expiration := "OFF", "OFF"
	if checkPolicy {
		policy = "ON"
	}
	if checkExpiration {
		expiration = "ON"
	}
	q := fmt.Sprintf("ALTER LOGIN %s WITH CHECK_POLICY = %s, CHECK_EXPIRATION = %s",
		quoteIdent(l.Name), policy, expiration)
	if err := l.server.execContext(ctx, q); err != nil {
		return fmt.Errorf("gosmo: set password policy for login %q: %w", l.Name, err)
	}
	return nil
}

// ChangePasswordWithOptions changes the login's password with the same
// quoted-literal encoding ChangePassword uses, plus MUST_CHANGE (force a
// password change at next login) and UNLOCK (clear a lockout).
func (l *Login) ChangePasswordWithOptions(newPassword string, mustChange, unlock bool) error {
	return l.ChangePasswordWithOptionsContext(context.Background(), newPassword, mustChange, unlock)
}

// ChangePasswordWithOptionsContext is the context-aware variant of
// ChangePasswordWithOptions.
func (l *Login) ChangePasswordWithOptionsContext(ctx context.Context, newPassword string, mustChange, unlock bool) error {
	stmt := buildChangePasswordStatement(l.Name, newPassword, mustChange, unlock)
	if err := l.server.execContext(ctx, stmt); err != nil {
		return fmt.Errorf("gosmo: change password (with options) for login %q: %w", l.Name, err)
	}
	return nil
}

// buildChangePasswordStatement builds the ALTER LOGIN ... WITH PASSWORD
// statement for ChangePasswordWithOptions. Unexported and side-effect-free
// so it's unit-testable without a server.
//
// MUST_CHANGE and UNLOCK are password-clause modifiers, not comma-separated
// <set_option> items — SQL Server rejects "PASSWORD = '...', UNLOCK" and
// "..., MUST_CHANGE" outright ("Incorrect syntax near 'UNLOCK'"). Both must
// instead follow PASSWORD = '...' space-separated, in either order;
// CHECK_EXPIRATION = ON is the one that belongs after a comma, as its own
// <set_option>.
func buildChangePasswordStatement(loginName, newPassword string, mustChange, unlock bool) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("ALTER LOGIN %s WITH PASSWORD = %s", quoteIdent(loginName), nStringLiteral(newPassword)))
	if mustChange {
		sb.WriteString(" MUST_CHANGE")
	}
	if unlock {
		sb.WriteString(" UNLOCK")
	}
	if mustChange {
		// MUST_CHANGE requires CHECK_EXPIRATION = ON (and CHECK_POLICY =
		// ON, already the server default) — SQL Server rejects MUST_CHANGE
		// otherwise.
		sb.WriteString(", CHECK_EXPIRATION = ON")
	}
	return sb.String()
}

// MapCredential maps a server credential to the login.
func (l *Login) MapCredential(credential string) error {
	return l.MapCredentialContext(context.Background(), credential)
}

// MapCredentialContext is the context-aware variant of MapCredential.
func (l *Login) MapCredentialContext(ctx context.Context, credential string) error {
	q := fmt.Sprintf("ALTER LOGIN %s ADD CREDENTIAL %s", quoteIdent(l.Name), quoteIdent(credential))
	if err := l.server.execContext(ctx, q); err != nil {
		return fmt.Errorf("gosmo: map credential %q to login %q: %w", credential, l.Name, err)
	}
	return nil
}

// UnmapCredential removes a credential mapping from the login.
func (l *Login) UnmapCredential(credential string) error {
	return l.UnmapCredentialContext(context.Background(), credential)
}

// UnmapCredentialContext is the context-aware variant of UnmapCredential.
func (l *Login) UnmapCredentialContext(ctx context.Context, credential string) error {
	q := fmt.Sprintf("ALTER LOGIN %s DROP CREDENTIAL %s", quoteIdent(l.Name), quoteIdent(credential))
	if err := l.server.execContext(ctx, q); err != nil {
		return fmt.Errorf("gosmo: unmap credential %q from login %q: %w", credential, l.Name, err)
	}
	return nil
}

// -- User mapping --------------------------------------------------------------

// LoginUserMapping describes one database this login is mapped into —
// SSMS's Login Properties > User Mapping page.
type LoginUserMapping struct {
	Database      string
	User          string
	DefaultSchema string
	Roles         []string
}

// UserMappings returns every database this login has a mapped user in.
// Only mapped databases are included — combine with Server.Databases to
// build a full "all databases, mapped or not" view. Databases that are
// offline, or that the login can't currently reach, are skipped rather
// than failing the whole scan (SSMS's own User Mapping page behaves the
// same way). A cancelled context is not skipped — it ends the scan and is
// returned.
//
// The skip covers a database whose query never opened. Once its rows are
// being read, a failure ends the scan with an error instead: those rows are
// already in the result, so skipping would return a short list and call it
// success.
func (l *Login) UserMappings() ([]*LoginUserMapping, error) {
	return l.UserMappingsContext(context.Background())
}

// UserMappingsContext is the context-aware variant of UserMappings.
func (l *Login) UserMappingsContext(ctx context.Context) ([]*LoginUserMapping, error) {
	dbs, err := l.server.DatabasesContext(ctx)
	if err != nil {
		return nil, err
	}

	const q = `
SELECT dp.name, ISNULL(dp.default_schema_name, ''),
       ISNULL(STUFF((SELECT ', ' + r.name
              FROM   sys.database_role_members rm
              JOIN   sys.database_principals r ON r.principal_id = rm.role_principal_id
              WHERE  rm.member_principal_id = dp.principal_id
              FOR XML PATH(''), TYPE).value('.','NVARCHAR(MAX)'), 1, 2, ''), '')
FROM   sys.database_principals dp
WHERE  dp.sid = @p1`

	// One query per database, serially. Fanning these across a worker pool
	// was tried and measured slower against a 46-database instance
	// (2026-08-14): Database.query pins a pooled connection of its own, and
	// on a pool with nothing idle each worker pays a full TCP+TLS+login
	// handshake, which costs far more than the query latency it overlaps.
	var out []*LoginUserMapping
	for _, db := range dbs {
		if db.State != "ONLINE" {
			continue
		}
		ms, err := l.userMappingsIn(ctx, db, q)
		if err != nil {
			return nil, err
		}
		out = append(out, ms...)
	}
	return out, nil
}

// userMappingsIn reads one database's mapping for l, or (nil, nil) if that
// database's query would not open — the skip UserMappings documents.
func (l *Login) userMappingsIn(ctx context.Context, db *Database, q string) ([]*LoginUserMapping, error) {
	rows, err := db.query(ctx, q, l.SID)
	if err != nil {
		// Skipping an unreachable database is the point of this scan, but a
		// cancelled context is not one of those — every remaining database
		// would fail the same way, so the scan stops instead of issuing a
		// doomed query per database.
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, nil
	}
	defer rows.Close()

	var out []*LoginUserMapping
	for rows.Next() {
		m := &LoginUserMapping{Database: db.Name}
		var roles string
		if err := rows.Scan(&m.User, &m.DefaultSchema, &roles); err != nil {
			return nil, fmt.Errorf("gosmo: user mappings for login %q in %q: %w", l.Name, db.Name, err)
		}
		if roles != "" {
			m.Roles = strings.Split(roles, ", ")
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		// Not skipped, unlike a failure to *open* the query above. By here
		// this database's rows are already built, so continuing would return a
		// silently short list and report success — the same failure the Scan
		// arm above aborts on, and the reason the skip stops at the query
		// boundary rather than covering iteration too.
		return nil, fmt.Errorf("gosmo: user mappings for login %q in %q: %w", l.Name, db.Name, err)
	}
	return out, nil
}

// MapToDatabase creates a user for this login in the named database
// (CREATE USER ... FOR LOGIN).
func (l *Login) MapToDatabase(dbName, userName, defaultSchema string) error {
	return l.MapToDatabaseContext(context.Background(), dbName, userName, defaultSchema)
}

// MapToDatabaseContext is the context-aware variant of MapToDatabase.
func (l *Login) MapToDatabaseContext(ctx context.Context, dbName, userName, defaultSchema string) error {
	d, err := l.server.DatabaseByNameContext(ctx, dbName)
	if err != nil {
		return err
	}
	return d.CreateUserContext(ctx, userName, l.Name, defaultSchema)
}

// UnmapFromDatabase drops this login's mapped user in the named database.
func (l *Login) UnmapFromDatabase(dbName string) error {
	return l.UnmapFromDatabaseContext(context.Background(), dbName)
}

// UnmapFromDatabaseContext is the context-aware variant of UnmapFromDatabase.
func (l *Login) UnmapFromDatabaseContext(ctx context.Context, dbName string) error {
	d, err := l.server.DatabaseByNameContext(ctx, dbName)
	if err != nil {
		return err
	}
	mappings, err := l.UserMappingsContext(ctx)
	if err != nil {
		return err
	}
	for _, m := range mappings {
		if m.Database == dbName {
			return d.DropUserContext(ctx, m.User)
		}
	}
	return fmt.Errorf("gosmo: login %q is not mapped to database %q", l.Name, dbName)
}

// -- Logins --------------------------------------------------------------------

// Logins returns all server-level logins.
func (s *Server) Logins() ([]*Login, error) {
	return s.LoginsContext(context.Background())
}

// LoginsContext is the context-aware variant of Logins.
//
// Every server-level login is listed, not just the SQL/Windows ones: the
// type filter also admits Entra ('E','X') and the certificate- and
// asymmetric-key-mapped logins ('C','K') that hold permissions for signed
// code, which is what SSMS's Logins folder shows.
func (s *Server) LoginsContext(ctx context.Context) ([]*Login, error) {
	const q = `
	SELECT name, sid, type_desc, is_disabled, default_database_name,
	       create_date, modify_date
	FROM sys.server_principals
	WHERE type IN ('S','U','G','E','X','C','K')
	ORDER BY name`

	rows, err := s.query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("gosmo: list logins: %w", err)
	}
	defer rows.Close()

	var logins []*Login
	for rows.Next() {
		l := &Login{server: s}
		var defDB sql.NullString
		if err := rows.Scan(&l.Name, &l.SID, &l.LoginType, &l.IsDisabled,
			&defDB, &l.CreateDate, &l.ModifyDate); err != nil {
			return nil, fmt.Errorf("gosmo: list logins: %w", err)
		}
		l.DefaultDatabase = defDB.String
		logins = append(logins, l)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("gosmo: list logins: %w", err)
	}
	return logins, nil
}

// LoginByName returns a single server-level login by name.
func (s *Server) LoginByName(name string) (*Login, error) {
	return s.LoginByNameContext(context.Background(), name)
}

// LoginByNameContext is the context-aware variant of LoginByName.
func (s *Server) LoginByNameContext(ctx context.Context, name string) (*Login, error) {
	const q = `
	SELECT name, sid, type_desc, is_disabled, default_database_name,
	       create_date, modify_date
	FROM sys.server_principals
	WHERE type IN ('S','U','G','E','X','C','K') AND name = @p1`

	l := &Login{server: s}
	var defDB sql.NullString

	if err := s.queryRowScan(ctx, q, []any{name},
		&l.Name, &l.SID, &l.LoginType, &l.IsDisabled, &defDB, &l.CreateDate, &l.ModifyDate,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, notFoundf("gosmo: login %q not found", name)
		}
		return nil, fmt.Errorf("gosmo: find login %q: %w", name, err)
	}
	l.DefaultDatabase = defDB.String
	return l, nil
}

// LoginRef returns a lightweight handle for name without querying the server
// at all — unlike LoginByName/LoginByNameContext, it doesn't verify the
// login exists or populate SID/LoginType/IsDisabled/etc. (they stay at
// their zero value). Every write method on *Login (AddServerRoleMemberContext,
// DisableContext, ChangePasswordContext, ...) only ever needs the login's
// name, never those cached fields, so this is sufficient for issuing
// further ALTER-style calls against a login the caller already knows
// exists — most commonly one it just created in the same operation. See
// Server.DatabaseRef's doc comment for why this also matters under a
// WithScript-derived context.
func (s *Server) LoginRef(name string) *Login {
	return &Login{server: s, Name: name}
}

// CreateLogin creates a login. With no CreateLoginOptions.Source, an empty
// password means a Windows login (FROM WINDOWS) and a non-empty one a SQL
// login; set Source to create any of the other kinds.
func (s *Server) CreateLogin(name, password string, opts *CreateLoginOptions) error {
	return s.CreateLoginContext(context.Background(), name, password, opts)
}

// CreateLoginContext is the context-aware variant of CreateLogin.
//
// Security: the password is never string-concatenated raw into the SQL
// text — it's quoted via nStringLiteral (N'...', doubling any embedded
// quote), the same escaping every other literal in this package uses.
// HASHED is deliberately not used here: it tells SQL Server the value is
// already one of its own password-hash formats, not a cleartext password,
// so passing an arbitrary hex encoding of the cleartext under HASHED
// either fails outright or creates a login nothing can ever authenticate
// as.
//
// DefaultDatabase reaches an external-provider login through a following
// ALTER LOGIN: OBJECT_ID is the only WITH option FROM EXTERNAL PROVIDER
// accepts, and DEFAULT_DATABASE alongside it does not parse. A
// certificate- or asymmetric-key-mapped login cannot have one at all —
// SQL Server rejects DEFAULT_DATABASE for those in both CREATE and ALTER
// ("Cannot use the parameter DEFAULT_DATABASE for a certificate or
// asymmetric key login", verified live) — so asking for one is an error
// rather than a statement the server will refuse.
func (s *Server) CreateLoginContext(ctx context.Context, name, password string, opts *CreateLoginOptions) error {
	if name == "" {
		return fmt.Errorf("gosmo: create login: name is required")
	}
	if opts == nil {
		opts = &CreateLoginOptions{}
	}

	src := opts.Source
	if src == LoginSourceAuto {
		if password == "" {
			src = LoginSourceWindows
		} else {
			src = LoginSourceSQL
		}
	}
	stmt, alterDefaultDB, err := createLoginStatement(name, password, src, opts)
	if err != nil {
		return fmt.Errorf("gosmo: create login %q: %w", name, err)
	}
	if err := s.execContext(ctx, stmt); err != nil {
		return fmt.Errorf("gosmo: create login %q: %w", name, err)
	}
	if alterDefaultDB {
		q := fmt.Sprintf("ALTER LOGIN %s WITH DEFAULT_DATABASE = %s",
			quoteIdent(name), quoteIdent(opts.DefaultDatabase))
		if err := s.execContext(ctx, q); err != nil {
			return fmt.Errorf("gosmo: create login %q: set default database: %w", name, err)
		}
	}
	return nil
}

// createLoginStatement builds the CREATE LOGIN statement for one resolved
// source, and reports whether DefaultDatabase still has to be applied by a
// following ALTER LOGIN — CERTIFICATE and ASYMMETRIC KEY take no WITH option
// list in CREATE LOGIN and EXTERNAL PROVIDER takes only OBJECT_ID, so naming
// DEFAULT_DATABASE there is a syntax error. A mapped login has no default database at all; see
// CreateLoginContext.
func createLoginStatement(name, password string, src LoginSource, opts *CreateLoginOptions) (string, bool, error) {
	if src != LoginSourceSQL && password != "" {
		return "", false, fmt.Errorf("a %s login takes no password", src)
	}
	if opts.MustChange && src != LoginSourceSQL {
		return "", false, fmt.Errorf("MustChange applies to a SQL login only, not a %s login", src)
	}
	if opts.DefaultDatabase != "" && (src == LoginSourceCertificate || src == LoginSourceAsymmetricKey) {
		return "", false, fmt.Errorf("a %s login cannot have a default database", src)
	}
	if opts.ObjectID != "" && src != LoginSourceExternalProvider {
		return "", false, fmt.Errorf("ObjectID applies to an external provider login only, not a %s login", src)
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "CREATE LOGIN %s", quoteIdent(name))

	switch src {
	case LoginSourceSQL:
		if password == "" {
			return "", false, fmt.Errorf("a SQL login requires a password")
		}
		fmt.Fprintf(&sb, " WITH PASSWORD = %s", nStringLiteral(password))
		if opts.MustChange {
			// MUST_CHANGE requires CHECK_EXPIRATION = ON (and CHECK_POLICY =
			// ON, already the server default) — SQL Server rejects
			// MUST_CHANGE otherwise.
			sb.WriteString(" MUST_CHANGE, CHECK_EXPIRATION = ON")
		}
		if opts.DefaultDatabase != "" {
			fmt.Fprintf(&sb, ", DEFAULT_DATABASE = %s", quoteIdent(opts.DefaultDatabase))
		}
	case LoginSourceWindows:
		sb.WriteString(" FROM WINDOWS")
		if opts.DefaultDatabase != "" {
			fmt.Fprintf(&sb, " WITH DEFAULT_DATABASE = %s", quoteIdent(opts.DefaultDatabase))
		}
	case LoginSourceExternalProvider:
		sb.WriteString(" FROM EXTERNAL PROVIDER")
		if opts.ObjectID != "" {
			// The one WITH option FROM EXTERNAL PROVIDER does take, and it is
			// not part of the general option list: OBJECT_ID names the Entra
			// principal directly, so DEFAULT_DATABASE still cannot join it
			// here and stays on the following ALTER LOGIN.
			fmt.Fprintf(&sb, " WITH OBJECT_ID = %s", nStringLiteral(opts.ObjectID))
		}
		return sb.String(), opts.DefaultDatabase != "", nil
	case LoginSourceCertificate:
		if opts.CertificateName == "" {
			return "", false, fmt.Errorf("a certificate login requires CertificateName")
		}
		fmt.Fprintf(&sb, " FROM CERTIFICATE %s", quoteIdent(opts.CertificateName))
		return sb.String(), false, nil
	case LoginSourceAsymmetricKey:
		if opts.AsymmetricKeyName == "" {
			return "", false, fmt.Errorf("an asymmetric key login requires AsymmetricKeyName")
		}
		fmt.Fprintf(&sb, " FROM ASYMMETRIC KEY %s", quoteIdent(opts.AsymmetricKeyName))
		return sb.String(), false, nil
	default:
		return "", false, fmt.Errorf("unknown login source %d", int(src))
	}
	return sb.String(), false, nil
}

// LoginSource names what a new login authenticates from — the FROM clause of
// CREATE LOGIN, or WITH PASSWORD for a SQL login.
type LoginSource int

const (
	// LoginSourceAuto resolves from the password CreateLogin is given: empty
	// means a Windows login, non-empty a SQL login. It is the zero value, so
	// a CreateLoginOptions written before LoginSource existed behaves exactly
	// as it did.
	LoginSourceAuto LoginSource = iota
	// LoginSourceSQL is a SQL Server login (WITH PASSWORD).
	LoginSourceSQL
	// LoginSourceWindows is a Windows user or group login (FROM WINDOWS).
	LoginSourceWindows
	// LoginSourceExternalProvider is a Microsoft Entra ID (Azure AD) login
	// (FROM EXTERNAL PROVIDER) — SQL Server 2022 and later, Azure SQL
	// Managed Instance, and Azure SQL Database.
	LoginSourceExternalProvider
	// LoginSourceCertificate maps the login to a certificate in master
	// (FROM CERTIFICATE). Nothing authenticates as such a login; it exists
	// to hold permissions for code signed by the certificate.
	LoginSourceCertificate
	// LoginSourceAsymmetricKey maps the login to an asymmetric key in master
	// (FROM ASYMMETRIC KEY), the asymmetric-key counterpart of
	// LoginSourceCertificate.
	LoginSourceAsymmetricKey
)

// String renders the source as the words used in error messages.
func (src LoginSource) String() string {
	switch src {
	case LoginSourceAuto:
		return "auto"
	case LoginSourceSQL:
		return "SQL"
	case LoginSourceWindows:
		return "Windows"
	case LoginSourceExternalProvider:
		return "external provider"
	case LoginSourceCertificate:
		return "certificate"
	case LoginSourceAsymmetricKey:
		return "asymmetric key"
	}
	return fmt.Sprintf("LoginSource(%d)", int(src))
}

// CreateLoginOptions holds optional parameters for CreateLogin.
type CreateLoginOptions struct {
	DefaultDatabase string
	MustChange      bool

	// Source selects what the login authenticates from. The zero value
	// (LoginSourceAuto) keeps CreateLogin's original behaviour: a SQL login
	// when a password is given, a Windows login when it is empty.
	Source LoginSource

	// CertificateName is the master certificate a LoginSourceCertificate
	// login maps to; required for that source and ignored otherwise.
	CertificateName string

	// AsymmetricKeyName is the master asymmetric key a
	// LoginSourceAsymmetricKey login maps to; required for that source and
	// ignored otherwise.
	AsymmetricKeyName string

	// ObjectID is the Microsoft Entra ID object id (a GUID) a
	// LoginSourceExternalProvider login names explicitly, emitted as
	// CREATE LOGIN ... FROM EXTERNAL PROVIDER WITH OBJECT_ID = '...'.
	// SQL Server 2022 and later. It resolves a display name that is
	// ambiguous in the directory — with no object id the server looks the
	// login name up itself, which is the ordinary case. Naming it for any
	// other source is an error rather than a silently ignored field.
	ObjectID string
}

// DropLogin drops a server login.
func (s *Server) DropLogin(name string) error {
	return s.DropLoginContext(context.Background(), name)
}

// DropLoginContext is the context-aware variant of DropLogin.
func (s *Server) DropLoginContext(ctx context.Context, name string) error {
	if name == "" {
		return fmt.Errorf("gosmo: drop login: name is required")
	}
	if err := s.execContext(ctx, fmt.Sprintf("DROP LOGIN %s", quoteIdent(name))); err != nil {
		return fmt.Errorf("gosmo: drop login %q: %w", name, err)
	}
	return nil
}
