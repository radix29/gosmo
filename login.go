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
	server          *Server
	Name            string
	SID             []byte
	LoginType       string // "SQL_LOGIN", "WINDOWS_LOGIN", "WINDOWS_GROUP"
	IsDisabled      bool
	DefaultDatabase string
	CreateDate      time.Time
	ModifyDate      time.Time
}

// Disable disables the login.
func (l *Login) Disable() error {
	return l.DisableContext(context.Background())
}

func (l *Login) DisableContext(ctx context.Context) error {
	if err := l.server.execContext(ctx, "ALTER LOGIN "+quoteIdent(l.Name)+" DISABLE"); err != nil {
		return fmt.Errorf("gosmo: disable login %q: %w", l.Name, err)
	}
	l.IsDisabled = true
	return nil
}

// Enable enables the login.
func (l *Login) Enable() error {
	return l.EnableContext(context.Background())
}

func (l *Login) EnableContext(ctx context.Context) error {
	if err := l.server.execContext(ctx, "ALTER LOGIN "+quoteIdent(l.Name)+" ENABLE"); err != nil {
		return fmt.Errorf("gosmo: enable login %q: %w", l.Name, err)
	}
	l.IsDisabled = false
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
func (l *Login) Drop() error { return l.server.DropLogin(l.Name) }

// AddServerRoleMember adds this login to a server role.
func (l *Login) AddServerRoleMember(roleName string) error {
	return l.AddServerRoleMemberContext(context.Background(), roleName)
}

func (l *Login) AddServerRoleMemberContext(ctx context.Context, roleName string) error {
	q := fmt.Sprintf("ALTER SERVER ROLE %s ADD MEMBER %s", quoteIdent(roleName), quoteIdent(l.Name))
	if err := l.server.execContext(ctx, q); err != nil {
		return fmt.Errorf("gosmo: add %q to server role %q: %w", l.Name, roleName, err)
	}
	return nil
}

// RemoveServerRoleMember removes this login from a server role.
func (l *Login) RemoveServerRoleMember(roleName string) error {
	return l.RemoveServerRoleMemberContext(context.Background(), roleName)
}

func (l *Login) RemoveServerRoleMemberContext(ctx context.Context, roleName string) error {
	q := fmt.Sprintf("ALTER SERVER ROLE %s DROP MEMBER %s", quoteIdent(roleName), quoteIdent(l.Name))
	if err := l.server.execContext(ctx, q); err != nil {
		return fmt.Errorf("gosmo: remove %q from server role %q: %w", l.Name, roleName, err)
	}
	return nil
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
	DefaultLanguage  string
	CredentialName   string
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
	var pwdLastSet, lastLogin sql.NullTime

	row := l.server.db.QueryRowContext(ctx, q, l.Name)
	if err := row.Scan(
		&isLocked, &isExpired, &isMustChange,
		&det.IsPolicyChecked, &det.IsExpirationChecked,
		&pwdLastSet, &lastLogin, &det.BadPasswordCount,
		&det.DefaultLanguage, &det.CredentialName, &det.ConnectSQLState,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("gosmo: login %q not found", l.Name)
		}
		return nil, fmt.Errorf("gosmo: login details for %q: %w", l.Name, err)
	}
	det.IsLocked = isLocked != 0
	det.IsExpired = isExpired != 0
	det.MustChangePassword = isMustChange != 0
	det.PasswordLastSet = pwdLastSet.Time
	det.LastLogin = lastLogin.Time
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
	l.Name = newName
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
	l.DefaultDatabase = name
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
// "..., MUST_CHANGE" outright ("Incorrect syntax near 'UNLOCK'"), confirmed
// against a live server. Both must instead follow PASSWORD = '...'
// space-separated, in either order; CHECK_EXPIRATION = ON is the one that
// belongs after a comma, as its own <set_option>.
func buildChangePasswordStatement(loginName, newPassword string, mustChange, unlock bool) string {
	var sb strings.Builder
	sb.WriteString("ALTER LOGIN " + quoteIdent(loginName) + " WITH PASSWORD = " + nStringLiteral(newPassword))
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
// same way).
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

	var out []*LoginUserMapping
	for _, db := range dbs {
		if db.State() != "ONLINE" {
			continue
		}
		rows, err := db.query(ctx, q, l.SID)
		if err != nil {
			continue
		}
		for rows.Next() {
			m := &LoginUserMapping{Database: db.Name()}
			var roles string
			if err := rows.Scan(&m.User, &m.DefaultSchema, &roles); err != nil {
				rows.Close()
				return nil, err
			}
			if roles != "" {
				m.Roles = strings.Split(roles, ", ")
			}
			out = append(out, m)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			continue
		}
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
