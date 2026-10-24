package gosmo

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// Login represents a SQL Server server-level login.
type Login struct {
	server *Server
	Name   string
	SID    []byte
	// LoginType is the login's type_desc: SQL_LOGIN, WINDOWS_LOGIN,
	// WINDOWS_GROUP, EXTERNAL_LOGIN, EXTERNAL_GROUP, CERTIFICATE_MAPPED_LOGIN
	// or ASYMMETRIC_KEY_MAPPED_LOGIN.
	LoginType       PrincipalType
	IsDisabled      bool
	DefaultDatabase string
	CreateDate      time.Time
	ModifyDate      time.Time

	// DefaultLanguage is sys.server_principals.default_language_name: set for
	// every SQL and Windows login, empty for the kinds that take none.
	DefaultLanguage string
	// IsPolicyChecked and IsExpirationChecked are a SQL login's CHECK_POLICY
	// and CHECK_EXPIRATION. Every other type has neither and reads false.
	IsPolicyChecked     bool
	IsExpirationChecked bool

	// MappedObject is the master certificate or asymmetric key a
	// CERTIFICATE_MAPPED_LOGIN / ASYMMETRIC_KEY_MAPPED_LOGIN maps to. It is
	// not read with the login — the name lives in master, not in
	// sys.server_principals — so it is empty until ResolveMapping fills it.
	MappedObject string
}

// Server returns the server the login belongs to.
func (l *Login) Server() *Server { return l.server }

// IsSystem reports whether l is one SQL Server creates and manages for itself:
// a name in ## (##MS_PolicyEventProcessingLogin##, ##MS_PolicyTsqlExecutionLogin##
// and the like). Derived from the name alone, so it answers on a LoginRef too.
//
// The server lets most of these be dropped, and that is the reason to ask:
// dropping [##MS_PolicyEventProcessingLogin##] succeeds and takes Policy-Based
// Management's execution identity with it, orphaning the matching users in
// master and msdb, and nothing fails until a policy runs.
//
// Deliberately narrow: sa and the NT SERVICE\* logins are not system logins
// here. Renaming sa is a documented hardening step, and the service logins
// are ordinary Windows logins an administrator may want gone.
func (l *Login) IsSystem() bool { return strings.HasPrefix(l.Name, "##") }

// IsSQLLogin reports whether l authenticates with a SQL Server password — the
// only kind with a password, CHECK_POLICY or CHECK_EXPIRATION.
func (l *Login) IsSQLLogin() bool { return l.LoginType == PrincipalSQLLogin }

// ResolveMapping looks up the certificate or asymmetric key this login maps
// to and stores its name in MappedObject.
//
// It is a no-op for every login type but CERTIFICATE_MAPPED_LOGIN and
// ASYMMETRIC_KEY_MAPPED_LOGIN. The lookup is by SID against master, where a
// login-mapped certificate or asymmetric key must live, and names master
// explicitly because the connection may be in any database. MappedObject is
// left empty, without an error, when nothing matches — the mapped object can
// have been dropped out from under the login.
//
// On a LoginRef the SID and type are looked up by name first.
func (l *Login) ResolveMapping(ctx context.Context) error {
	what := fmt.Sprintf("resolve mapping for login %q", l.Name)
	sid, loginType, err := l.identity(ctx, what)
	if err != nil {
		return err
	}
	switch loginType {
	case PrincipalCertificateMappedLogin, PrincipalAsymmetricKeyMappedLogin:
	default:
		return nil
	}
	const q = `
SELECT ISNULL((SELECT TOP 1 name FROM master.sys.certificates    WHERE sid = @p1),
       ISNULL((SELECT TOP 1 name FROM master.sys.asymmetric_keys WHERE sid = @p1), ''))`

	var name string
	if err := l.server.queryRowScan(ctx, q, []any{sid}, &name); err != nil {
		return fmt.Errorf("gosmo: %s: %w", what, err)
	}
	l.MappedObject = name
	return nil
}

// Disable disables the login.
func (l *Login) Disable(ctx context.Context) error {
	if _, err := l.server.exec(ctx, "ALTER LOGIN "+quoteIdent(l.Name)+" DISABLE"); err != nil {
		return fmt.Errorf("gosmo: disable login %q: %w", l.Name, err)
	}
	setIfApplied(ctx, &l.IsDisabled, true)
	return nil
}

// SetEnabled is Enable when on is true and Disable otherwise.
func (l *Login) SetEnabled(ctx context.Context, on bool) error {
	if on {
		return l.Enable(ctx)
	}
	return l.Disable(ctx)
}

// Enable enables the login.
func (l *Login) Enable(ctx context.Context) error {
	if _, err := l.server.exec(ctx, "ALTER LOGIN "+quoteIdent(l.Name)+" ENABLE"); err != nil {
		return fmt.Errorf("gosmo: enable login %q: %w", l.Name, err)
	}
	setIfApplied(ctx, &l.IsDisabled, false)
	return nil
}

// Drop drops the login from the server.
func (l *Login) Drop(ctx context.Context) error {
	if l.Name == "" {
		return invalidf("gosmo: drop login: name is required")
	}
	if _, err := l.server.exec(ctx, fmt.Sprintf("DROP LOGIN %s", quoteIdent(l.Name))); err != nil {
		return fmt.Errorf("gosmo: drop login %q: %w", l.Name, err)
	}
	return nil
}

// AddToServerRole adds this login to a server role.
func (l *Login) AddToServerRole(ctx context.Context, roleName string) error {
	return l.server.ServerRoleRef(roleName).AddMember(ctx, l.Name)
}

// RemoveFromServerRole removes this login from a server role.
func (l *Login) RemoveFromServerRole(ctx context.Context, roleName string) error {
	return l.server.ServerRoleRef(roleName).RemoveMember(ctx, l.Name)
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
	// PasswordLastSet is when the password was last set, or the zero Time
	// if none is recorded (see loginPropertyTime).
	PasswordLastSet time.Time
	// LastLogin is best-effort: it reflects the most recent session found
	// in sys.dm_exec_sessions, which only holds currently-connected (or
	// very recently disconnected) sessions, not full login history. It is
	// the zero Time if no matching session is currently visible.
	LastLogin        time.Time
	BadPasswordCount int
	// BadPasswordTime is the last failed-login attempt time, or the zero
	// Time if none is recorded — never the server's 1900-01-01 sentinel
	// (see loginPropertyTime).
	BadPasswordTime time.Time
	DefaultLanguage string
	CredentialName  string
	// ConnectSQLState is GRANT, GRANT_WITH_GRANT_OPTION, DENY, or ""
	// (default/unset) for the login's explicit CONNECT SQL server permission
	// — sys.server_permissions.state_desc as read.
	ConnectSQLState PermissionState
}

// Details returns the login's password-policy and status information.
func (l *Login) Details(ctx context.Context) (*LoginDetails, error) {
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
    -- sys.server_principals, not sys.sql_logins: the latter holds SQL
    -- logins only, so a Windows login's language read as unset.
    ISNULL(sp.default_language_name, ''),
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
		return nil, rowErr(err, notFoundf("gosmo: login %q not found", l.Name),
			fmt.Sprintf("login details for %q", l.Name))
	}
	det.IsLocked = isLocked != 0
	det.IsExpired = isExpired != 0
	det.MustChangePassword = isMustChange != 0
	det.PasswordLastSet = loginPropertyTime(pwdLastSet)
	det.LastLogin = lastLogin.Time
	det.BadPasswordTime = loginPropertyTime(badPasswordTime)
	return det, nil
}

// loginPropertyTime decodes a LOGINPROPERTY date. For "never" the server
// answers not NULL but its 1900-01-01 sentinel, shifted by the server's UTC
// offset (1900-01-01 02:00 on a UTC+2 host, the day before west of UTC) — so
// anything before 1900-01-02 is the zero Time, as NULL is. Passed through, it
// reached the Status page as a real "last bad password" date.
func loginPropertyTime(t sql.NullTime) time.Time {
	if !t.Valid || t.Time.Before(time.Date(1900, 1, 2, 0, 0, 0, 0, time.UTC)) {
		return time.Time{}
	}
	return t.Time
}

// Rename changes the login's name.
func (l *Login) Rename(ctx context.Context, newName string) error {
	q := fmt.Sprintf("ALTER LOGIN %s WITH NAME = %s", quoteIdent(l.Name), quoteIdent(newName))
	if _, err := l.server.exec(ctx, q); err != nil {
		return fmt.Errorf("gosmo: rename login %q to %q: %w", l.Name, newName, err)
	}
	setIfApplied(ctx, &l.Name, newName)
	return nil
}

// SetDefaultDatabase changes the login's default database.
func (l *Login) SetDefaultDatabase(ctx context.Context, name string) error {
	q := fmt.Sprintf("ALTER LOGIN %s WITH DEFAULT_DATABASE = %s", quoteIdent(l.Name), quoteIdent(name))
	if _, err := l.server.exec(ctx, q); err != nil {
		return fmt.Errorf("gosmo: set default database for login %q to %q: %w", l.Name, name, err)
	}
	setIfApplied(ctx, &l.DefaultDatabase, name)
	return nil
}

// SetDefaultLanguage changes the login's default language. lang is mirrored
// onto DefaultLanguage as given, so pass the language's name rather than an
// alias for the handle to read as the catalog does.
func (l *Login) SetDefaultLanguage(ctx context.Context, lang string) error {
	q := fmt.Sprintf("ALTER LOGIN %s WITH DEFAULT_LANGUAGE = %s", quoteIdent(l.Name), quoteIdent(lang))
	if _, err := l.server.exec(ctx, q); err != nil {
		return fmt.Errorf("gosmo: set default language for login %q to %q: %w", l.Name, lang, err)
	}
	setIfApplied(ctx, &l.DefaultLanguage, lang)
	return nil
}

// PasswordPolicy is what Login.SetPasswordPolicy sets: CHECK_POLICY and
// CHECK_EXPIRATION.
type PasswordPolicy struct {
	CheckPolicy     bool
	CheckExpiration bool
}

// SetPasswordPolicy sets the login's CHECK_POLICY and CHECK_EXPIRATION
// flags. CheckExpiration with CheckPolicy off is refused before anything is
// sent, as SQL Server refuses it (Msg 15122). Turning either off on a login
// whose password must change at next login fails Msg 15128 on the server —
// that state needs a read, so it is not pre-validated here.
func (l *Login) SetPasswordPolicy(ctx context.Context, p PasswordPolicy) error {
	if p.CheckExpiration && !p.CheckPolicy {
		return invalidf("gosmo: set password policy for login %q: CheckExpiration cannot be on when CheckPolicy is off", l.Name)
	}
	policy, expiration := "OFF", "OFF"
	if p.CheckPolicy {
		policy = "ON"
	}
	if p.CheckExpiration {
		expiration = "ON"
	}
	q := fmt.Sprintf("ALTER LOGIN %s WITH CHECK_POLICY = %s, CHECK_EXPIRATION = %s",
		quoteIdent(l.Name), policy, expiration)
	if _, err := l.server.exec(ctx, q); err != nil {
		return fmt.Errorf("gosmo: set password policy for login %q: %w", l.Name, err)
	}
	setIfApplied(ctx, &l.IsPolicyChecked, p.CheckPolicy)
	setIfApplied(ctx, &l.IsExpirationChecked, p.CheckExpiration)
	return nil
}

// ChangePassword changes the login's password.
//
// Security: the password is quoted via QuoteLiteral (N'...', doubling
// any embedded quote) rather than interpolated raw. HASHED is
// deliberately not used — it tells SQL Server the value is already one of
// its own password-hash formats, not cleartext, so passing a hex encoding
// of the cleartext under HASHED either fails outright or creates a login
// nothing can ever authenticate as.
//
// opts adds MUST_CHANGE (force a password change at next login), UNLOCK
// (clear a lockout) and the login's CHECK_POLICY/CHECK_EXPIRATION, all in the
// one write; the zero value changes the password alone. Carrying the
// policy in the same write is what lets MUST_CHANGE apply to a login whose
// policy is off, and the policy be turned off on a login whose password must
// change — a separate SetPasswordPolicy before or after fails one of the two
// (Msg 15122/15128). The combinations SQL Server refuses are refused before
// anything is sent.
func (l *Login) ChangePassword(ctx context.Context, newPassword string, opts ChangePasswordOptions) error {
	stmt, err := buildChangePasswordStatement(l.Name, newPassword, opts)
	if err != nil {
		return fmt.Errorf("gosmo: change password for login %q: %w", l.Name, err)
	}
	if opts.turnsPolicyOff() {
		// Two ALTERs on one branch of the batch: all or nothing.
		err = l.server.execPasswordsAtomic(ctx, []string{stmt}, newPassword)
	} else {
		_, err = l.server.execPasswords(ctx, stmt, newPassword)
	}
	if err != nil {
		return fmt.Errorf("gosmo: change password for login %q: %w", l.Name, err)
	}
	policy, expiration := opts.effectivePolicy()
	if policy != nil {
		setIfApplied(ctx, &l.IsPolicyChecked, *policy)
	}
	if expiration != nil {
		setIfApplied(ctx, &l.IsExpirationChecked, *expiration)
	}
	return nil
}

// ChangePasswordOptions are the modifiers ChangePassword can add.
type ChangePasswordOptions struct {
	// MustChange forces a password change at the next login (MUST_CHANGE),
	// which also turns CHECK_POLICY and CHECK_EXPIRATION on — SQL Server
	// refuses it otherwise. With either set to false it is refused.
	MustChange bool
	// Unlock clears a lockout (UNLOCK).
	Unlock bool
	// CheckPolicy and CheckExpiration set CHECK_POLICY/CHECK_EXPIRATION in
	// the same statement; nil leaves each as it is. CheckExpiration on with
	// CheckPolicy off is refused, as SQL Server refuses it.
	CheckPolicy     *bool
	CheckExpiration *bool
}

// turnsPolicyOff reports whether o turns CHECK_POLICY or CHECK_EXPIRATION
// off — what a login whose password must change refuses (Msg 15128) until a
// password change clears that flag.
func (o ChangePasswordOptions) turnsPolicyOff() bool {
	return !o.MustChange && ((o.CheckPolicy != nil && !*o.CheckPolicy) || (o.CheckExpiration != nil && !*o.CheckExpiration))
}

// effectivePolicy is the CHECK_POLICY/CHECK_EXPIRATION the statement sets:
// the caller's, with MustChange forcing both on.
func (o ChangePasswordOptions) effectivePolicy() (policy, expiration *bool) {
	if o.MustChange {
		return new(true), new(true)
	}
	return o.CheckPolicy, o.CheckExpiration
}

// buildChangePasswordStatement builds the ALTER LOGIN ... WITH PASSWORD
// statement for ChangePassword. Unexported and side-effect-free
// so it's unit-testable without a server.
//
// MUST_CHANGE and UNLOCK are password-clause modifiers, not comma-separated
// <set_option> items — SQL Server rejects "PASSWORD = '...', UNLOCK" and
// "..., MUST_CHANGE" outright ("Incorrect syntax near 'UNLOCK'"). Both must
// instead follow PASSWORD = '...' space-separated, in either order;
// CHECK_POLICY and CHECK_EXPIRATION belong after a comma, as their own
// <set_option>s.
func buildChangePasswordStatement(loginName, newPassword string, opts ChangePasswordOptions) (string, error) {
	policyOff := opts.CheckPolicy != nil && !*opts.CheckPolicy
	switch {
	case opts.MustChange && (policyOff || (opts.CheckExpiration != nil && !*opts.CheckExpiration)):
		return "", invalidf("MustChange requires CheckPolicy and CheckExpiration on")
	case policyOff && opts.CheckExpiration != nil && *opts.CheckExpiration:
		return "", invalidf("CheckExpiration cannot be on when CheckPolicy is off")
	}
	login := quoteIdent(loginName)
	var pw strings.Builder
	fmt.Fprintf(&pw, "ALTER LOGIN %s WITH PASSWORD = %s", login, QuoteLiteral(newPassword))
	if opts.MustChange {
		pw.WriteString(" MUST_CHANGE")
	}
	if opts.Unlock {
		pw.WriteString(" UNLOCK")
	}
	var policies []string
	policy, expiration := opts.effectivePolicy()
	if policy != nil {
		policies = append(policies, "CHECK_POLICY = "+onOff(*policy))
	}
	if expiration != nil {
		policies = append(policies, "CHECK_EXPIRATION = "+onOff(*expiration))
	}
	if len(policies) == 0 {
		return pw.String(), nil
	}
	combined := pw.String() + ", " + strings.Join(policies, ", ")
	if !opts.turnsPolicyOff() {
		return combined, nil
	}
	// A login whose password must change refuses either option OFF, in the
	// password's own statement too (15128); a password change alone clears
	// the flag, so there the password goes first and the policy after
	// (verified live on 17.0). Decided on the server, so no read is needed
	// and a WithScript capture is what runs. Only that branch splits: a weak
	// password is accepted only with the policy turned off in its own
	// statement.
	return fmt.Sprintf(`IF CAST(LOGINPROPERTY(%s, 'IsMustChange') AS int) = 1
BEGIN
    %s;
    ALTER LOGIN %s WITH %s;
END
ELSE
    %s`, QuoteLiteral(loginName), pw.String(), login, strings.Join(policies, ", "), combined), nil
}

// MapCredential maps a server credential to the login.
func (l *Login) MapCredential(ctx context.Context, credential string) error {
	q := fmt.Sprintf("ALTER LOGIN %s ADD CREDENTIAL %s", quoteIdent(l.Name), quoteIdent(credential))
	if _, err := l.server.exec(ctx, q); err != nil {
		return fmt.Errorf("gosmo: map credential %q to login %q: %w", credential, l.Name, err)
	}
	return nil
}

// UnmapCredential removes a credential mapping from the login.
func (l *Login) UnmapCredential(ctx context.Context, credential string) error {
	q := fmt.Sprintf("ALTER LOGIN %s DROP CREDENTIAL %s", quoteIdent(l.Name), quoteIdent(credential))
	if _, err := l.server.exec(ctx, q); err != nil {
		return fmt.Errorf("gosmo: unmap credential %q from login %q: %w", credential, l.Name, err)
	}
	return nil
}

// -- Logins --------------------------------------------------------------------

// loginSelect is the column list Logins and LoginByName share, in
// scanLogin's order. sys.sql_logins is joined for the password-policy flags a
// script has to carry; every other login type has no row there.
const loginSelect = `
	SELECT sp.name, sp.sid, sp.type_desc, sp.is_disabled, sp.default_database_name,
	       sp.create_date, sp.modify_date, sp.default_language_name,
	       ISNULL(sl.is_policy_checked, 0), ISNULL(sl.is_expiration_checked, 0)
	FROM sys.server_principals sp
	LEFT JOIN sys.sql_logins sl ON sl.principal_id = sp.principal_id
	WHERE sp.type IN ('S','U','G','E','X','C','K')`

func scanLogin(s *Server, scan func(...any) error) (*Login, error) {
	l := &Login{server: s}
	var defDB, defLang sql.NullString
	if err := scan(&l.Name, &l.SID, &l.LoginType, &l.IsDisabled,
		&defDB, &l.CreateDate, &l.ModifyDate, &defLang,
		&l.IsPolicyChecked, &l.IsExpirationChecked); err != nil {
		return nil, err
	}
	l.DefaultDatabase = defDB.String
	l.DefaultLanguage = defLang.String
	return l, nil
}

// Logins returns all server-level logins.
//
// Every server-level login is listed, not just the SQL/Windows ones: the type
// filter also admits Entra ('E','X') and the certificate- and
// asymmetric-key-mapped logins ('C','K') that hold permissions for signed
// code, which is what SSMS's Logins folder shows.
func (s *Server) Logins(ctx context.Context) ([]*Login, error) {
	rows, err := s.query(ctx, loginSelect+"\n\tORDER BY sp.name")
	return scanRows(rows, err, "list logins", func(scan func(...any) error) (*Login, error) {
		return scanLogin(s, scan)
	})
}

// LoginByName returns a single server-level login by name.
func (s *Server) LoginByName(ctx context.Context, name string) (*Login, error) {
	return readByName(ctx, s, scanLogin, loginSelect+" AND sp.name = @p1", []any{name},
		notFoundf("gosmo: login %q not found", name), fmt.Sprintf("find login %q", name))
}

// LoginRef returns a lightweight handle for name without querying the server
// at all — unlike LoginByName, it doesn't verify the
// login exists or populate SID/LoginType/IsDisabled/etc. (they stay at
// their zero value). The ALTER-style writes on *Login (AddToServerRole,
// Disable, ChangePassword, ...) name the login and need nothing else, so
// this is sufficient for issuing them against a login the caller already
// knows exists — most commonly one it just created in the same operation.
// See Server.DatabaseRef's doc comment for why this also matters under a
// WithScript-derived context.
//
// The reads keyed by SID or login type (UserMappings, ResolveMapping, and
// UnmapFromDatabase, a write that reads the mapping first) look both up by
// name on this handle — one extra round trip, and a not-found error for a
// login that does not exist, rather than an empty result.
func (s *Server) LoginRef(name string) *Login {
	return &Login{server: s, Name: name}
}

// identity returns the login's SID and type_desc: the fields when the login
// was read (SID is never empty on a read login), else looked up by name. The
// handle is not updated — a read must not write to a receiver another
// goroutine may be reading.
func (l *Login) identity(ctx context.Context, what string) ([]byte, PrincipalType, error) {
	if l.SID != nil {
		return l.SID, l.LoginType, nil
	}
	var sid []byte
	var loginType PrincipalType
	err := l.server.queryRowScan(ctx, `
SELECT sid, type_desc FROM sys.server_principals
WHERE  name = @p1 AND type IN ('S','U','G','E','X','C','K')`, []any{l.Name}, &sid, &loginType)
	if err != nil {
		return nil, "", rowErr(err, notFoundf("gosmo: login %q not found", l.Name), what)
	}
	return sid, loginType, nil
}

// CreateLogin creates a login. With no CreateLoginRequest.Source, an empty
// password (and no PasswordHash) means a Windows login (FROM WINDOWS) and a
// non-empty one a SQL login; set Source to create any of the other kinds. It returns the login
// read back from the catalog — or, under Scripting(ctx), the LoginRef handle,
// since nothing ran.
//
// Security: the password is never string-concatenated raw into the SQL text
// — it's quoted via QuoteLiteral (N'...', doubling any embedded quote),
// the same escaping every other literal in this package uses. HASHED is used
// only for CreateLoginRequest.PasswordHash, never for Password: it tells SQL
// Server the value is already one of its own password-hash formats, not a
// cleartext password, so passing an arbitrary hex encoding of the cleartext
// under HASHED either fails outright or creates a login nothing can ever
// authenticate as.
//
// Every option a source's CREATE LOGIN accepts goes in its one WITH list, so
// a login is never created first under the server's defaults and corrected
// afterwards: CHECK_POLICY defaults to ON, so a weak password with the policy
// off has to be refused-or-accepted by the CREATE itself (Msg 15118
// otherwise, on Linux too).
//
// DefaultDatabase and DefaultLanguage reach an external-provider login
// through a following ALTER LOGIN: OBJECT_ID is the only WITH option FROM
// EXTERNAL PROVIDER accepts, and DEFAULT_DATABASE alongside it does not
// parse. A certificate- or asymmetric-key-mapped login cannot have either at
// all —
// SQL Server rejects DEFAULT_DATABASE for those in both CREATE and ALTER
// ("Cannot use the parameter DEFAULT_DATABASE for a certificate or
// asymmetric key login", verified live) — so asking for one is an error
// rather than a statement the server will refuse. The CREATE and that ALTER
// run as one execAtomic write, so a refused ALTER leaves no login behind —
// except on Azure SQL Database, where CREATE LOGIN must be alone in its
// batch, so there they stay two statements.
func (s *Server) CreateLogin(ctx context.Context, req CreateLoginRequest) (*Login, error) {
	name, password, opts := req.Name, req.Password, &req
	if name == "" {
		return nil, invalidf("gosmo: create login: name is required")
	}

	src := opts.Source
	if src == LoginSourceAuto {
		if password == "" && opts.PasswordHash == nil {
			src = LoginSourceWindows
		} else {
			src = LoginSourceSQL
		}
	}
	stmt, alterDefaults, err := createLoginStatement(name, password, src, opts)
	if err != nil {
		return nil, fmt.Errorf("gosmo: create login %q: %w", name, err)
	}
	stmts := []string{stmt}
	if alterDefaults {
		var set []string
		if opts.DefaultDatabase != "" {
			set = append(set, "DEFAULT_DATABASE = "+quoteIdent(opts.DefaultDatabase))
		}
		if opts.DefaultLanguage != "" {
			set = append(set, "DEFAULT_LANGUAGE = "+quoteIdent(opts.DefaultLanguage))
		}
		stmts = append(stmts, fmt.Sprintf("ALTER LOGIN %s WITH %s", quoteIdent(name), strings.Join(set, ", ")))
	}
	switch {
	case len(stmts) == 1:
		_, err = s.execPasswords(ctx, stmt, password)
	case s.info != nil && EngineEdition(s.info.EngineEdition) == EngineAzureSQLDatabase:
		for _, q := range stmts {
			if _, err = s.execPasswords(ctx, q, password); err != nil {
				break
			}
		}
	default:
		err = s.execPasswordsAtomic(ctx, stmts, password)
	}
	if err != nil {
		return nil, fmt.Errorf("gosmo: create login %q: %w", name, err)
	}
	return createdObject(ctx, s.LoginRef(name), func() (*Login, error) {
		return s.LoginByName(ctx, name)
	})
}

// createLoginStatement builds the CREATE LOGIN statement for one resolved
// source, and reports whether DefaultDatabase or DefaultLanguage still has to
// be applied by a following ALTER LOGIN — CERTIFICATE and ASYMMETRIC KEY take
// no WITH option list in CREATE LOGIN and EXTERNAL PROVIDER takes only
// OBJECT_ID, so naming DEFAULT_DATABASE there is a syntax error. A mapped
// login has no default database or language at all; see CreateLogin.
//
// The combinations SQL Server refuses are refused here first, so a caller
// learns of them before anything runs (each verified live on 17,
// 2026-10-02): CHECK_EXPIRATION = ON with CHECK_POLICY = OFF (Msg 15122,
// which MUST_CHANGE's implied CHECK_EXPIRATION = ON hits too), MUST_CHANGE
// with HASHED (Msg 33010), and SID or CREDENTIAL on a Windows login (a
// syntax error: <windows_options> is DEFAULT_DATABASE and DEFAULT_LANGUAGE
// only).
func createLoginStatement(name, password string, src LoginSource, opts *CreateLoginRequest) (string, bool, error) {
	isSQL := src == LoginSourceSQL
	if !isSQL && (password != "" || opts.PasswordHash != nil) {
		return "", false, invalidf("a %s login takes no password", src)
	}
	if opts.MustChange && !isSQL {
		return "", false, invalidf("MustChange applies to a SQL login only, not a %s login", src)
	}
	if !isSQL && (opts.CheckPolicy != nil || opts.CheckExpiration != nil) {
		return "", false, invalidf("CheckPolicy and CheckExpiration apply to a SQL login only, not a %s login", src)
	}
	if !isSQL && opts.SID != nil {
		return "", false, invalidf("SID applies to a SQL login only, not a %s login", src)
	}
	if !isSQL && opts.Credential != "" {
		return "", false, invalidf("a credential maps to a SQL login only, not a %s login", src)
	}
	if (opts.DefaultDatabase != "" || opts.DefaultLanguage != "") && (src == LoginSourceCertificate || src == LoginSourceAsymmetricKey) {
		return "", false, invalidf("a %s login cannot have a default database or language", src)
	}
	if opts.ObjectID != "" && src != LoginSourceExternalProvider {
		return "", false, invalidf("ObjectID applies to an external provider login only, not a %s login", src)
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "CREATE LOGIN %s", quoteIdent(name))

	switch src {
	case LoginSourceSQL:
		policyOff := opts.CheckPolicy != nil && !*opts.CheckPolicy
		switch {
		case password == "" && opts.PasswordHash == nil:
			return "", false, invalidf("a SQL login requires a password")
		case password != "" && opts.PasswordHash != nil:
			return "", false, invalidf("a SQL login takes Password or PasswordHash, not both")
		case opts.MustChange && opts.PasswordHash != nil:
			return "", false, invalidf("MustChange cannot be combined with PasswordHash")
		case opts.MustChange && (policyOff || (opts.CheckExpiration != nil && !*opts.CheckExpiration)):
			return "", false, invalidf("MustChange requires CheckPolicy and CheckExpiration on")
		case policyOff && opts.CheckExpiration != nil && *opts.CheckExpiration:
			return "", false, invalidf("CheckExpiration cannot be on when CheckPolicy is off")
		}
		if opts.PasswordHash != nil {
			fmt.Fprintf(&sb, " WITH PASSWORD = %s HASHED", binaryLiteral(opts.PasswordHash))
		} else {
			fmt.Fprintf(&sb, " WITH PASSWORD = %s", QuoteLiteral(password))
		}
		checkExpiration := opts.CheckExpiration
		if opts.MustChange {
			// MUST_CHANGE requires CHECK_EXPIRATION = ON (and CHECK_POLICY =
			// ON, already the server default) — SQL Server rejects
			// MUST_CHANGE otherwise.
			sb.WriteString(" MUST_CHANGE")
			checkExpiration = new(true)
		}
		if opts.SID != nil {
			fmt.Fprintf(&sb, ", SID = %s", binaryLiteral(opts.SID))
		}
		writeLoginDefaults(&sb, ", ", opts)
		if checkExpiration != nil {
			sb.WriteString(", CHECK_EXPIRATION = " + onOff(*checkExpiration))
		}
		if opts.CheckPolicy != nil {
			sb.WriteString(", CHECK_POLICY = " + onOff(*opts.CheckPolicy))
		}
		if opts.Credential != "" {
			fmt.Fprintf(&sb, ", CREDENTIAL = %s", quoteIdent(opts.Credential))
		}
	case LoginSourceWindows:
		sb.WriteString(" FROM WINDOWS")
		writeLoginDefaults(&sb, " WITH ", opts)
	case LoginSourceExternalProvider:
		sb.WriteString(" FROM EXTERNAL PROVIDER")
		if opts.ObjectID != "" {
			// The one WITH option FROM EXTERNAL PROVIDER does take, and it is
			// not part of the general option list: OBJECT_ID names the Entra
			// principal directly, so DEFAULT_DATABASE still cannot join it
			// here and stays on the following ALTER LOGIN.
			fmt.Fprintf(&sb, " WITH OBJECT_ID = %s", QuoteLiteral(opts.ObjectID))
		}
		return sb.String(), opts.DefaultDatabase != "" || opts.DefaultLanguage != "", nil
	case LoginSourceCertificate:
		if opts.CertificateName == "" {
			return "", false, invalidf("a certificate login requires CertificateName")
		}
		fmt.Fprintf(&sb, " FROM CERTIFICATE %s", quoteIdent(opts.CertificateName))
		return sb.String(), false, nil
	case LoginSourceAsymmetricKey:
		if opts.AsymmetricKeyName == "" {
			return "", false, invalidf("an asymmetric key login requires AsymmetricKeyName")
		}
		fmt.Fprintf(&sb, " FROM ASYMMETRIC KEY %s", quoteIdent(opts.AsymmetricKeyName))
		return sb.String(), false, nil
	default:
		return "", false, invalidf("unknown login source %d", int(src))
	}
	return sb.String(), false, nil
}

// writeLoginDefaults appends DEFAULT_DATABASE and DEFAULT_LANGUAGE, the first
// of them introduced by lead (", " after a PASSWORD clause, " WITH " for a
// Windows login, whose CREATE has no other option).
func writeLoginDefaults(sb *strings.Builder, lead string, opts *CreateLoginRequest) {
	if opts.DefaultDatabase != "" {
		fmt.Fprintf(sb, "%sDEFAULT_DATABASE = %s", lead, quoteIdent(opts.DefaultDatabase))
		lead = ", "
	}
	if opts.DefaultLanguage != "" {
		fmt.Fprintf(sb, "%sDEFAULT_LANGUAGE = %s", lead, quoteIdent(opts.DefaultLanguage))
	}
}

// LoginSource names what a new login authenticates from — the FROM clause of
// CREATE LOGIN, or WITH PASSWORD for a SQL login.
type LoginSource int

const (
	// LoginSourceAuto resolves from the password CreateLogin is given: empty
	// means a Windows login, non-empty a SQL login. It is the zero value, so
	// a CreateLoginRequest that does not set it behaves as the kind its
	// password implies.
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

// CreateLoginRequest describes a new login.
type CreateLoginRequest struct {
	Name string
	// Password is the SQL login's password. With Source left at
	// LoginSourceAuto, an empty one (and no PasswordHash) means a Windows
	// login.
	Password string

	// PasswordHash is a SQL login's password as SQL Server's own hash — the
	// varbinary LOGINPROPERTY(name, 'PasswordHash') returns — emitted as
	// PASSWORD = 0x... HASHED. It re-creates a login elsewhere (an AG
	// secondary, a migration target) able to authenticate with the same
	// password, and implies a SQL login under LoginSourceAuto. Mutually
	// exclusive with Password and with MustChange.
	PasswordHash []byte

	DefaultDatabase string
	// DefaultLanguage is the login's DEFAULT_LANGUAGE, by name or alias;
	// empty leaves the server default. A certificate- or asymmetric-key
	// login cannot have one.
	DefaultLanguage string
	MustChange      bool

	// CheckPolicy and CheckExpiration are a SQL login's CHECK_POLICY and
	// CHECK_EXPIRATION; nil omits the option, so the server default applies
	// (policy ON, expiration OFF). CheckExpiration on with CheckPolicy off is
	// refused, as SQL Server refuses it; MustChange implies CheckExpiration
	// on and is refused with either one explicitly off.
	CheckPolicy     *bool
	CheckExpiration *bool

	// SID is the SQL login's security identifier, for re-creating a login
	// whose database users must keep mapping to it (an AG secondary, a
	// restored database); nil lets the server generate one. SQL logins
	// only — a Windows login's SID is its Windows one.
	SID []byte

	// Credential is a server credential to map to the SQL login
	// (CREDENTIAL = ...). SQL logins only: CREATE LOGIN ... FROM WINDOWS
	// does not take it, though Login.MapCredential can add one afterwards.
	Credential string

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
