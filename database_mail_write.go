package gosmo

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// database_mail_write.go is the write half of database_mail.go: accounts,
// profiles, their accounts and grants, the system parameters, start/stop,
// sending, and the item and log purges. Every write is one or more
// msdb.dbo.sysmail_* procedure calls.
//
// # Permissions
//
// Probed on 13 and 17 (2026-10-01): msdb db_owner or CONTROL SERVER runs
// every write here, with one exception — an account with Basic
// authentication stores its password as a server credential, so creating
// one, or changing its user name or password, also needs ALTER ANY
// CREDENTIAL (Msg 15247), which CONTROL SERVER implies and db_owner does not.
// A DatabaseMailUserRole member can SendMail through a profile granted to
// it and DeleteMailItems its own items; nothing else.
//
// # 'Database Mail XPs'
//
// With the option at 0 every configuration write here still works; only
// StartDatabaseMail, StopDatabaseMail and SendMail are refused (Msg
// 15281).
//
// # What the procedures do that a caller would not guess
//
//   - sysmail_update_account_sp keeps the stored value of every NULL
//     parameter except @username: an omitted @username drops the account's
//     credential and turns Basic authentication into anonymous. Only
//     @no_credential_change = 1 keeps the stored user name and password, so
//     an Alter that does not set Credentials sends it.
//   - @username without @password sets an empty password.
//   - The procedures trim user names and passwords and treat '' as NULL.
//   - NULL means "keep" for every string, so a display name, reply-to address
//     or description is cleared by sending '', which reads back as empty.
//   - Deleting an account removes it from every profile; deleting a profile
//     removes its account links and grants and marks its unsent mail failed.
//     The server refuses neither.
//   - Sequence numbers within a profile may repeat and have gaps; gosmo
//     writes 1..n so the order it reads back is unambiguous.
//   - sysmail_add_principalprofile_sp takes an msdb database user name, or
//     "public"; a login or a role is Msg 14607. A principal's new default
//     profile clears its previous one.
//
// # The password in a script
//
// Under WithScript, and to a statement observer, a password is
// PasswordPlaceholder — the statement that runs carries the real one, the
// captured and observed copies never do (unless WithScriptSecrets asks for
// it). The scripter writes the same placeholder, since the stored password
// cannot be read.

// mailPasswordNote precedes a statement carrying PasswordPlaceholder.
const mailPasswordNote = "-- The account's password cannot be scripted. Replace " + PasswordPlaceholder + " before running.\n"

// procArgs accumulates the "@name = value" arguments of one EXEC.
type procArgs struct{ parts []string }

func (a *procArgs) raw(name, v string) { a.parts = append(a.parts, name+" = "+v) }

func (a *procArgs) str(name, v string) { a.raw(name, QuoteLiteral(v)) }

func (a *procArgs) strPtr(name string, v *string) {
	if v != nil {
		a.str(name, *v)
	}
}

// strSet sends v only when it is not empty — the procedure's own default
// (NULL) otherwise.
func (a *procArgs) strSet(name, v string) {
	if v != "" {
		a.str(name, v)
	}
}

func (a *procArgs) int(name string, v int) { a.raw(name, fmt.Sprint(v)) }

func (a *procArgs) intPtr(name string, v *int) {
	if v != nil {
		a.int(name, *v)
	}
}

func (a *procArgs) bit(name string, v bool) { a.int(name, boolToInt(v)) }

func (a *procArgs) bitPtr(name string, v *bool) {
	if v != nil {
		a.bit(name, *v)
	}
}

// exec renders "EXEC msdb.dbo.<proc> <args>".
func (a *procArgs) exec(proc string) string {
	stmt := "EXEC msdb.dbo." + proc
	if len(a.parts) > 0 {
		stmt += " " + strings.Join(a.parts, ", ")
	}
	return stmt
}

// mailDateLiteral renders t as a datetime literal the server reads the same
// whatever its language setting (ISO 8601 with the T).
func mailDateLiteral(t time.Time) string {
	return "'" + t.Format("2006-01-02T15:04:05.000") + "'"
}

// ============================================================
// Accounts
// ============================================================

// MailCredentials is how an account authenticates to its SMTP server.
// UserName and Password apply to MailAuthBasic only and are ignored
// otherwise.
type MailCredentials struct {
	Authentication MailAuthentication
	UserName       string
	// Password is written, never read: the server keeps it in a credential.
	// Leading and trailing spaces are trimmed by the server.
	Password string
}

func (c MailCredentials) validate() error {
	switch c.Authentication {
	case MailAuthAnonymous, MailAuthWindows:
		return nil
	case MailAuthBasic:
		// The procedure trims the user name and treats '' as NULL, which
		// would store an anonymous account instead.
		if strings.TrimSpace(c.UserName) == "" {
			return fmt.Errorf("basic authentication needs a user name")
		}
		return nil
	}
	return fmt.Errorf("unknown authentication %v", c.Authentication)
}

// render adds the credential arguments twice over: to run, with the
// password, and to show, with PasswordPlaceholder.
func (c MailCredentials) render(run, shown *procArgs) {
	for _, a := range []*procArgs{run, shown} {
		a.bit("@use_default_credentials", c.Authentication == MailAuthWindows)
	}
	if c.Authentication == MailAuthBasic {
		run.str("@username", c.UserName)
		shown.str("@username", c.UserName)
		run.str("@password", c.Password)
		shown.str("@password", PasswordPlaceholder)
	}
}

// CreateMailAccountRequest describes a new Database Mail account. Empty
// optional strings are left at the procedure's default (NULL).
type CreateMailAccountRequest struct {
	Name           string
	EmailAddress   string
	DisplayName    string
	ReplyToAddress string
	Description    string

	// ServerName is the SMTP server's host name or address.
	ServerName string
	// Port is the SMTP port; 0 means the procedure's default, 25.
	Port      int
	EnableSSL bool
	// Timeout is the SMTP timeout in seconds; 0 leaves it unset.
	// sysmail_add_account_sp has no @timeout, so a non-zero one is a second
	// statement, sysmail_update_account_sp.
	Timeout int

	Credentials MailCredentials
}

// MailAccountRef returns a handle for a Database Mail account by name,
// without reading msdb. Every field but Name is zero; MailAccountByName is
// what populates them. Alter and Drop address the account by name, so the
// handle is enough for both.
func (s *Server) MailAccountRef(name string) *MailAccount {
	return &MailAccount{server: s, Name: name}
}

// mailAccountCreateStmts renders CreateMailAccount's statements: each to
// run, and as shown (password replaced).
func mailAccountCreateStmts(req CreateMailAccountRequest) (run, shown []string) {
	var r, sh procArgs
	for _, a := range []*procArgs{&r, &sh} {
		a.str("@account_name", req.Name)
		a.str("@email_address", req.EmailAddress)
		a.strSet("@display_name", req.DisplayName)
		a.strSet("@replyto_address", req.ReplyToAddress)
		a.strSet("@description", req.Description)
		a.str("@mailserver_name", req.ServerName)
		if req.Port != 0 {
			a.int("@port", req.Port)
		}
		a.bit("@enable_ssl", req.EnableSSL)
	}
	req.Credentials.render(&r, &sh)
	run, shown = []string{r.exec("sysmail_add_account_sp")}, []string{sh.exec("sysmail_add_account_sp")}
	if req.Timeout != 0 {
		var t procArgs
		t.str("@account_name", req.Name)
		t.int("@timeout", req.Timeout)
		t.bit("@no_credential_change", true)
		stmt := t.exec("sysmail_update_account_sp")
		run, shown = append(run, stmt), append(shown, stmt)
	}
	return run, shown
}

// CreateMailAccount creates a Database Mail account and returns it read
// back, or, under Scripting(ctx), the MailAccountRef handle. With Basic
// authentication the password is stored as a server credential, which needs
// ALTER ANY CREDENTIAL besides the msdb rights.
func (s *Server) CreateMailAccount(ctx context.Context, req CreateMailAccountRequest) (*MailAccount, error) {
	switch {
	case strings.TrimSpace(req.Name) == "":
		return nil, fmt.Errorf("gosmo: create mail account: account has no name")
	case strings.TrimSpace(req.EmailAddress) == "":
		return nil, fmt.Errorf("gosmo: create mail account %q: account has no e-mail address", req.Name)
	case strings.TrimSpace(req.ServerName) == "":
		return nil, fmt.Errorf("gosmo: create mail account %q: account has no mail server", req.Name)
	}
	if err := req.Credentials.validate(); err != nil {
		return nil, fmt.Errorf("gosmo: create mail account %q: %w", req.Name, err)
	}
	// With a timeout the account is two statements — the add procedure takes
	// no @timeout — sent as one atomic batch: one at a time, a failed second
	// left the account created behind an error, and the caller's retry then
	// failed "already exists".
	run, shown := mailAccountCreateStmts(req)
	var err error
	if len(run) == 1 {
		err = s.execSecret(ctx, run[0], shown[0])
	} else {
		err = s.execSecretAtomic(ctx, run, shown)
	}
	if err != nil {
		return nil, fmt.Errorf("gosmo: create mail account %q: %w", req.Name, err)
	}
	return createdObject(ctx, s.MailAccountRef(req.Name), func() (*MailAccount, error) {
		return s.MailAccountByName(ctx, req.Name)
	})
}

// MailAccountOptions is a change to an account. A nil field is left as
// stored; a non-nil one is sent even when it holds the stored value. An
// empty string clears a display name, reply-to address or description.
type MailAccountOptions struct {
	// Name renames the account.
	Name           *string
	EmailAddress   *string
	DisplayName    *string
	ReplyToAddress *string
	Description    *string
	ServerName     *string
	Port           *int
	EnableSSL      *bool
	Timeout        *int

	// Credentials replaces the authentication, user name and password
	// together. nil keeps all three (sent as @no_credential_change = 1);
	// there is no way to change the user name and keep the password, since
	// the server cannot read the password back to reuse it. Anonymous or
	// Windows drops a stored credential.
	Credentials *MailCredentials
}

func (o MailAccountOptions) empty() bool {
	return o.Name == nil && o.EmailAddress == nil && o.DisplayName == nil &&
		o.ReplyToAddress == nil && o.Description == nil && o.ServerName == nil &&
		o.Port == nil && o.EnableSSL == nil && o.Timeout == nil && o.Credentials == nil
}

// mailAccountAlterStmt renders Alter's statement, to run and as shown. A
// rename identifies the account by id — the procedure takes @account_name as
// the new name when @account_id is given — read in the same batch, so a
// name-only handle can rename too.
func mailAccountAlterStmt(name string, o MailAccountOptions) (run, shown string) {
	var r, sh procArgs
	for _, a := range []*procArgs{&r, &sh} {
		if o.Name != nil {
			a.raw("@account_id", "@account_id")
			a.str("@account_name", *o.Name)
		} else {
			a.str("@account_name", name)
		}
		a.strPtr("@email_address", o.EmailAddress)
		a.strPtr("@display_name", o.DisplayName)
		a.strPtr("@replyto_address", o.ReplyToAddress)
		a.strPtr("@description", o.Description)
		a.strPtr("@mailserver_name", o.ServerName)
		a.intPtr("@port", o.Port)
		a.bitPtr("@enable_ssl", o.EnableSSL)
		a.intPtr("@timeout", o.Timeout)
	}
	if o.Credentials != nil {
		o.Credentials.render(&r, &sh)
	} else {
		r.bit("@no_credential_change", true)
		sh.bit("@no_credential_change", true)
	}
	run, shown = r.exec("sysmail_update_account_sp"), sh.exec("sysmail_update_account_sp")
	if o.Name != nil {
		pre := "DECLARE @account_id int = (SELECT account_id FROM msdb.dbo.sysmail_account WHERE name = " +
			QuoteLiteral(name) + ");\nIF @account_id IS NULL RAISERROR(14607, 16, 1, 'account') ELSE "
		run, shown = pre+run, pre+shown
	}
	return run, shown
}

// Alter applies every field set on o in one sysmail_update_account_sp. An
// empty o issues nothing. Setting Credentials to Basic needs ALTER ANY
// CREDENTIAL besides the msdb rights.
//
// So does any Alter of an account that has Basic authentication, even one
// leaving Credentials nil. The procedure finds the stored credential through
// sys.credentials, which shows a login without ALTER ANY CREDENTIAL (or VIEW
// ANY DEFINITION) nothing, and writes back the NULL it read as the account's
// credential_id: the call succeeds and the account keeps its user name but
// loses its password (gossms W14, msdb db_owner on 17).
func (a *MailAccount) Alter(ctx context.Context, o MailAccountOptions) error {
	if o.empty() {
		return nil
	}
	if o.Name != nil && strings.TrimSpace(*o.Name) == "" {
		return fmt.Errorf("gosmo: alter mail account %q: new name is empty", a.Name)
	}
	if o.Credentials != nil {
		if err := o.Credentials.validate(); err != nil {
			return fmt.Errorf("gosmo: alter mail account %q: %w", a.Name, err)
		}
	}
	run, shown := mailAccountAlterStmt(a.Name, o)
	if err := a.server.execSecret(ctx, run, shown); err != nil {
		return fmt.Errorf("gosmo: alter mail account %q: %w", a.Name, err)
	}
	setPtrIfApplied(ctx, &a.Name, o.Name)
	setPtrIfApplied(ctx, &a.EmailAddress, o.EmailAddress)
	setPtrIfApplied(ctx, &a.DisplayName, o.DisplayName)
	setPtrIfApplied(ctx, &a.ReplyToAddress, o.ReplyToAddress)
	setPtrIfApplied(ctx, &a.Description, o.Description)
	setPtrIfApplied(ctx, &a.ServerName, o.ServerName)
	setPtrIfApplied(ctx, &a.Port, o.Port)
	setPtrIfApplied(ctx, &a.EnableSSL, o.EnableSSL)
	setPtrIfApplied(ctx, &a.Timeout, o.Timeout)
	if c := o.Credentials; c != nil {
		setIfApplied(ctx, &a.UseDefaultCredentials, c.Authentication == MailAuthWindows)
		if c.Authentication == MailAuthBasic {
			setIfApplied(ctx, &a.UserName, strings.TrimSpace(c.UserName))
		} else {
			setIfApplied(ctx, &a.UserName, "")
			setIfApplied(ctx, &a.CredentialID, 0)
		}
	}
	return nil
}

// Drop deletes the account and its credential. The account leaves every
// profile it was in; the server does not refuse that. A caller without ALTER
// ANY CREDENTIAL (or VIEW ANY DEFINITION) is not refused either: the
// procedure cannot see the credential, so it deletes the account and leaves
// the credential behind (see Alter).
func (a *MailAccount) Drop(ctx context.Context) error {
	var p procArgs
	p.str("@account_name", a.Name)
	if err := a.server.exec(ctx, p.exec("sysmail_delete_account_sp")); err != nil {
		return fmt.Errorf("gosmo: drop mail account %q: %w", a.Name, err)
	}
	return nil
}

// ============================================================
// Profiles
// ============================================================

// CreateMailProfileRequest describes a new Database Mail profile. Its
// accounts and grants are added afterwards (SetAccounts, Grant).
type CreateMailProfileRequest struct {
	Name        string
	Description string
}

// MailProfileRef returns a handle for a Database Mail profile by name,
// without reading msdb. Every field but Name is zero; MailProfileByName is
// what populates them. Every write addresses the profile by name, so the
// handle is enough for all of them.
func (s *Server) MailProfileRef(name string) *MailProfile {
	return &MailProfile{server: s, Name: name}
}

// CreateMailProfile creates a Database Mail profile and returns it read
// back, or, under Scripting(ctx), the MailProfileRef handle.
func (s *Server) CreateMailProfile(ctx context.Context, req CreateMailProfileRequest) (*MailProfile, error) {
	if strings.TrimSpace(req.Name) == "" {
		return nil, fmt.Errorf("gosmo: create mail profile: profile has no name")
	}
	if err := s.exec(ctx, mailProfileCreateStmt(req.Name, req.Description)); err != nil {
		return nil, fmt.Errorf("gosmo: create mail profile %q: %w", req.Name, err)
	}
	return createdObject(ctx, s.MailProfileRef(req.Name), func() (*MailProfile, error) {
		return s.MailProfileByName(ctx, req.Name)
	})
}

func mailProfileCreateStmt(name, description string) string {
	var p procArgs
	p.str("@profile_name", name)
	p.strSet("@description", description)
	return p.exec("sysmail_add_profile_sp")
}

// MailProfileOptions is a change to a profile. A nil field is left as
// stored; an empty Description clears it.
type MailProfileOptions struct {
	// Name renames the profile.
	Name        *string
	Description *string
}

// Alter applies every field set on o in one sysmail_update_profile_sp. An
// empty o issues nothing. A rename identifies the profile by id, read in the
// same batch, as MailAccount.Alter does.
func (p *MailProfile) Alter(ctx context.Context, o MailProfileOptions) error {
	if o.Name == nil && o.Description == nil {
		return nil
	}
	if o.Name != nil && strings.TrimSpace(*o.Name) == "" {
		return fmt.Errorf("gosmo: alter mail profile %q: new name is empty", p.Name)
	}
	var a procArgs
	if o.Name != nil {
		a.raw("@profile_id", "@profile_id")
		a.str("@profile_name", *o.Name)
	} else {
		a.str("@profile_name", p.Name)
	}
	a.strPtr("@description", o.Description)
	stmt := a.exec("sysmail_update_profile_sp")
	if o.Name != nil {
		stmt = "DECLARE @profile_id int = (SELECT profile_id FROM msdb.dbo.sysmail_profile WHERE name = " +
			QuoteLiteral(p.Name) + ");\nIF @profile_id IS NULL RAISERROR(14607, 16, 1, 'profile') ELSE " + stmt
	}
	if err := p.server.exec(ctx, stmt); err != nil {
		return fmt.Errorf("gosmo: alter mail profile %q: %w", p.Name, err)
	}
	setPtrIfApplied(ctx, &p.Name, o.Name)
	setPtrIfApplied(ctx, &p.Description, o.Description)
	return nil
}

// Drop deletes the profile with its account links and grants. Mail still
// queued for it is marked failed — the procedure's @force_delete default.
func (p *MailProfile) Drop(ctx context.Context) error {
	var a procArgs
	a.str("@profile_name", p.Name)
	if err := p.server.exec(ctx, a.exec("sysmail_delete_profile_sp")); err != nil {
		return fmt.Errorf("gosmo: drop mail profile %q: %w", p.Name, err)
	}
	return nil
}

// -- Profile accounts ------------------------------------------------------------

func mailProfileAccountStmt(proc, profile, account string, seq *int) string {
	var a procArgs
	a.str("@profile_name", profile)
	a.str("@account_name", account)
	a.intPtr("@sequence_number", seq)
	return a.exec(proc)
}

// AddAccount adds an account to the profile at failover position seq.
func (p *MailProfile) AddAccount(ctx context.Context, account string, seq int) error {
	if err := p.server.exec(ctx, mailProfileAccountStmt("sysmail_add_profileaccount_sp", p.Name, account, &seq)); err != nil {
		return fmt.Errorf("gosmo: add account %q to mail profile %q: %w", account, p.Name, err)
	}
	return nil
}

// RemoveAccount removes an account from the profile; the account itself is
// kept.
func (p *MailProfile) RemoveAccount(ctx context.Context, account string) error {
	if err := p.server.exec(ctx, mailProfileAccountStmt("sysmail_delete_profileaccount_sp", p.Name, account, nil)); err != nil {
		return fmt.Errorf("gosmo: remove account %q from mail profile %q: %w", account, p.Name, err)
	}
	return nil
}

// SetAccountSequence moves an account of the profile to failover position
// seq, in place.
func (p *MailProfile) SetAccountSequence(ctx context.Context, account string, seq int) error {
	if err := p.server.exec(ctx, mailProfileAccountStmt("sysmail_update_profileaccount_sp", p.Name, account, &seq)); err != nil {
		return fmt.Errorf("gosmo: set sequence of account %q in mail profile %q: %w", account, p.Name, err)
	}
	return nil
}

// SetAccounts makes accounts the profile's account list, in failover order
// (sequence 1..n): it reads the profile's current links, then removes,
// renumbers and adds in one all-or-nothing batch. A list that already
// matches issues nothing. Accounts is then read back, since a newly linked
// account's id is the server's to tell.
//
// The read goes to the server under WithScript too. For a profile the same
// script is about to create it finds no links, so the script adds every
// account — which is what that script needs.
func (p *MailProfile) SetAccounts(ctx context.Context, accounts []string) error {
	what := fmt.Sprintf("set accounts of mail profile %q", p.Name)
	collation := p.server.collation()
	seen := map[string]bool{}
	for _, name := range accounts {
		k := NameKey(collation, name)
		if seen[k] {
			return fmt.Errorf("gosmo: %s: account %q is listed twice", what, name)
		}
		seen[k] = true
	}
	current, err := p.links(ctx, what)
	if err != nil {
		return err
	}
	stmts := mailSetAccountsStmts(collation, p.Name, current, accounts)
	if len(stmts) > 0 {
		if err := p.server.execAtomic(ctx, stmts); err != nil {
			return fmt.Errorf("gosmo: %s: %w", what, err)
		}
		if Scripting(ctx) {
			return nil
		}
		if current, err = p.links(ctx, what); err != nil {
			return err
		}
	}
	setIfApplied(ctx, &p.Accounts, current)
	return nil
}

// links reads the profile's account links from the server, in failover
// order.
func (p *MailProfile) links(ctx context.Context, what string) ([]*MailProfileAccount, error) {
	rows, err := p.server.query(ctx, mailProfileAccountQuery+`
JOIN   msdb.dbo.sysmail_profile p ON p.profile_id = pa.profile_id
WHERE  p.name = @p1
ORDER  BY pa.sequence_number`, p.Name)
	return scanRows(rows, err, what, func(scan func(...any) error) (*MailProfileAccount, error) {
		var profileID int
		pa := &MailProfileAccount{}
		err := scan(&profileID, &pa.AccountID, &pa.AccountName, &pa.SequenceNumber)
		return pa, err
	})
}

// mailSetAccountsStmts is the diff from current to want: removals first,
// then renumbering in place, then additions. Names are matched under msdb's
// collation — the server's — as sysmail does, so a want spelled "smtp1" for
// a link to "SMTP1" on a case-insensitive server is that link, not a remove
// and re-add.
func mailSetAccountsStmts(collation, profile string, current []*MailProfileAccount, want []string) []string {
	wanted := map[string]bool{}
	for _, name := range want {
		wanted[NameKey(collation, name)] = true
	}
	var stmts []string
	have := map[string]int{}
	for _, pa := range current {
		k := NameKey(collation, pa.AccountName)
		have[k] = pa.SequenceNumber
		if !wanted[k] {
			stmts = append(stmts, mailProfileAccountStmt("sysmail_delete_profileaccount_sp", profile, pa.AccountName, nil))
		}
	}
	var adds []string
	for i, name := range want {
		seq := i + 1
		old, ok := have[NameKey(collation, name)]
		switch {
		case !ok:
			adds = append(adds, mailProfileAccountStmt("sysmail_add_profileaccount_sp", profile, name, &seq))
		case old != seq:
			stmts = append(stmts, mailProfileAccountStmt("sysmail_update_profileaccount_sp", profile, name, &seq))
		}
	}
	return append(stmts, adds...)
}

// -- Profile security --------------------------------------------------------------

// MailPublicPrincipal is the principal name that grants a profile to every
// msdb user.
const MailPublicPrincipal = "public"

func mailPrincipalProfileStmt(proc, profile, principal string, isDefault *bool) string {
	var a procArgs
	a.str("@principal_name", principal)
	a.str("@profile_name", profile)
	a.bitPtr("@is_default", isDefault)
	return a.exec(proc)
}

// Grant lets principal — an msdb database user, or MailPublicPrincipal —
// send through the profile, and makes it that principal's default profile
// when isDefault is set (clearing its previous default).
func (p *MailProfile) Grant(ctx context.Context, principal string, isDefault bool) error {
	if err := p.server.exec(ctx, mailPrincipalProfileStmt("sysmail_add_principalprofile_sp", p.Name, principal, &isDefault)); err != nil {
		return fmt.Errorf("gosmo: grant mail profile %q to %q: %w", p.Name, principal, err)
	}
	if principal == MailPublicPrincipal {
		setIfApplied(ctx, &p.IsPublic, true)
		setIfApplied(ctx, &p.IsDefault, isDefault)
	}
	return nil
}

// SetGrantDefault changes whether the profile is principal's default; the
// grant must exist. Setting it clears the principal's previous default.
func (p *MailProfile) SetGrantDefault(ctx context.Context, principal string, isDefault bool) error {
	if err := p.server.exec(ctx, mailPrincipalProfileStmt("sysmail_update_principalprofile_sp", p.Name, principal, &isDefault)); err != nil {
		return fmt.Errorf("gosmo: set default of mail profile %q for %q: %w", p.Name, principal, err)
	}
	if principal == MailPublicPrincipal {
		setIfApplied(ctx, &p.IsDefault, isDefault)
	}
	return nil
}

// Revoke removes principal's grant of the profile.
func (p *MailProfile) Revoke(ctx context.Context, principal string) error {
	if err := p.server.exec(ctx, mailPrincipalProfileStmt("sysmail_delete_principalprofile_sp", p.Name, principal, nil)); err != nil {
		return fmt.Errorf("gosmo: revoke mail profile %q from %q: %w", p.Name, principal, err)
	}
	if principal == MailPublicPrincipal {
		setIfApplied(ctx, &p.IsPublic, false)
		setIfApplied(ctx, &p.IsDefault, false)
	}
	return nil
}

// ============================================================
// System parameters
// ============================================================

// MailConfigurationOptions is a change to the system parameters. A nil
// field is left as stored.
type MailConfigurationOptions struct {
	AccountRetryAttempts           *int
	AccountRetryDelay              *int
	DatabaseMailExeMinimumLifeTime *int
	DefaultAttachmentEncoding      *string
	LoggingLevel                   *MailLoggingLevel
	MaxFileSize                    *int
	ProhibitedExtensions           *string
}

// stmts renders one sysmail_configure_sp per set parameter, in the order
// MailConfiguration lists them.
func (o MailConfigurationOptions) stmts() []string {
	var stmts []string
	param := func(name, v string) {
		var a procArgs
		a.str("@parameter_name", name)
		a.str("@parameter_value", v)
		stmts = append(stmts, a.exec("sysmail_configure_sp"))
	}
	intParam := func(name string, v *int) {
		if v != nil {
			param(name, fmt.Sprint(*v))
		}
	}
	intParam("AccountRetryAttempts", o.AccountRetryAttempts)
	intParam("AccountRetryDelay", o.AccountRetryDelay)
	intParam("DatabaseMailExeMinimumLifeTime", o.DatabaseMailExeMinimumLifeTime)
	if o.DefaultAttachmentEncoding != nil {
		param("DefaultAttachmentEncoding", *o.DefaultAttachmentEncoding)
	}
	if o.LoggingLevel != nil {
		param("LoggingLevel", fmt.Sprint(int(*o.LoggingLevel)))
	}
	intParam("MaxFileSize", o.MaxFileSize)
	if o.ProhibitedExtensions != nil {
		param("ProhibitedExtensions", *o.ProhibitedExtensions)
	}
	return stmts
}

// SetMailConfiguration writes every parameter set on o, one
// sysmail_configure_sp each. The procedure validates nothing and updates
// only a parameter msdb already has a row for.
func (s *Server) SetMailConfiguration(ctx context.Context, o MailConfigurationOptions) error {
	if o.LoggingLevel != nil && (*o.LoggingLevel < MailLoggingNormal || *o.LoggingLevel > MailLoggingVerbose) {
		return fmt.Errorf("gosmo: set mail configuration: logging level %d is not 1, 2 or 3", int(*o.LoggingLevel))
	}
	for _, stmt := range o.stmts() {
		if err := s.exec(ctx, stmt); err != nil {
			return fmt.Errorf("gosmo: set mail configuration: %w", err)
		}
	}
	return nil
}

// ============================================================
// Start, stop, send
// ============================================================

// StartDatabaseMail starts the mail queue (sysmail_start_sp). It does not
// start DatabaseMail.exe — the first queued message does. Refused while
// 'Database Mail XPs' is 0.
func (s *Server) StartDatabaseMail(ctx context.Context) error {
	if err := s.exec(ctx, "EXEC msdb.dbo.sysmail_start_sp"); err != nil {
		return fmt.Errorf("gosmo: start database mail: %w", err)
	}
	return nil
}

// StopDatabaseMail stops the mail queue (sysmail_stop_sp). Mail is not
// queued meanwhile: sp_send_dbmail refuses it, Msg 14641 (probed on 17).
// Refused while 'Database Mail XPs' is 0.
func (s *Server) StopDatabaseMail(ctx context.Context) error {
	if err := s.exec(ctx, "EXEC msdb.dbo.sysmail_stop_sp"); err != nil {
		return fmt.Errorf("gosmo: stop database mail: %w", err)
	}
	return nil
}

// MailMessage is one plain-text message for SendMail. An empty Profile
// sends through the caller's default profile, or the public default — what a
// DatabaseMailUserRole member, who cannot list profiles, passes. To is
// sp_send_dbmail's @recipients: one or more addresses separated by ';'.
type MailMessage struct {
	Profile string
	To      string
	Subject string
	Body    string
}

// The refusals SendMail reports through errors.Is, each the server error
// sp_send_dbmail raises for it, captured live on 17 (2026-10-01). The server's
// own text — "profile name is not valid" — names nothing a user can act on;
// these let a caller say what to do instead. The SQL Server error stays
// reachable through AsSQLError, and the message text is unchanged.
var (
	// ErrMailProfileInvalid: the profile does not exist, or is not granted to
	// the caller (Msg 14607).
	ErrMailProfileInvalid = errors.New("database mail profile not valid")
	// ErrMailNoDefaultProfile: no profile was named and the caller has no
	// default to fall back on (Msg 14636).
	ErrMailNoDefaultProfile = errors.New("no default database mail profile")
	// ErrMailStopped: Database Mail is stopped, and the message was not
	// queued (Msg 14641).
	ErrMailStopped = errors.New("database mail is stopped")
	// ErrMailXPsDisabled: the 'Database Mail XPs' option is 0 (Msg 15281).
	ErrMailXPsDisabled = errors.New("database mail XPs are disabled")
)

// mailSendErrors maps the error numbers sp_send_dbmail refuses with to the
// sentinels above.
var mailSendErrors = map[int32]error{
	14607: ErrMailProfileInvalid,
	14636: ErrMailNoDefaultProfile,
	14641: ErrMailStopped,
	15281: ErrMailXPsDisabled,
}

// classifiedError keeps err's text and chain and adds kind to it, so
// errors.Is reaches both.
type classifiedError struct {
	err  error
	kind error
}

func (e *classifiedError) Error() string   { return e.err.Error() }
func (e *classifiedError) Unwrap() []error { return []error{e.kind, e.err} }

// classifyMailSendError returns err carrying the mail sentinel its first
// error-severity message maps to, or err unchanged.
func classifyMailSendError(err error) error {
	se, ok := AsSQLError(err)
	if !ok {
		return err
	}
	for _, m := range se.messages() {
		if kind, ok := mailSendErrors[m.Number]; ok && m.IsError() {
			return &classifiedError{err: err, kind: kind}
		}
	}
	return err
}

// SendMail queues m through sp_send_dbmail and returns its mailitem_id, for
// MailItemByID and MailEvents to follow.
//
// A refusal sp_send_dbmail is known to raise wraps ErrMailProfileInvalid,
// ErrMailNoDefaultProfile, ErrMailStopped or ErrMailXPsDisabled.
//
// It returns once the message is queued; whether it was sent is learned by
// polling. It is never retried (execScan): a connection lost after the
// server queued the message would queue it again. Under Scripting(ctx) the
// statement is recorded and the id is 0.
func (s *Server) SendMail(ctx context.Context, m MailMessage) (int, error) {
	if strings.TrimSpace(m.To) == "" {
		return 0, fmt.Errorf("gosmo: send mail: no recipient")
	}
	var a procArgs
	a.strSet("@profile_name", m.Profile)
	a.str("@recipients", m.To)
	a.str("@subject", m.Subject)
	a.str("@body", m.Body)
	a.raw("@mailitem_id", "@mailitem_id OUTPUT")
	stmt := "DECLARE @mailitem_id int;\n" + a.exec("sp_send_dbmail")
	if c, ok := scriptFrom(ctx); ok {
		c.append(ScriptEntry{Server: scriptServerName(ctx, s), SQL: stmt + ";\nSELECT @mailitem_id AS mailitem_id;"})
		return 0, nil
	}
	var id int
	if err := s.execScan(ctx, stmt+";\nSELECT @mailitem_id;", &id); err != nil {
		return 0, fmt.Errorf("gosmo: send mail: %w", classifyMailSendError(err))
	}
	observe(ctx, s, ScriptEntry{Server: scriptServerName(ctx, s), SQL: stmt + ";\nSELECT @mailitem_id AS mailitem_id;"})
	return id, nil
}

// ============================================================
// Purging items and the log
// ============================================================

// DeleteMailItems deletes mail items requested before before and, when
// status is set, in that state — sysmail_delete_mailitems_sp, which needs at
// least one of the two (Msg 14608 otherwise) and logs an information row
// counting what it deleted. A DatabaseMailUserRole member deletes only their
// own items.
func (s *Server) DeleteMailItems(ctx context.Context, before time.Time, status MailSentStatus) error {
	var a procArgs
	if !before.IsZero() {
		a.raw("@sent_before", mailDateLiteral(before))
	}
	if status != "" {
		a.str("@sent_status", string(status))
	}
	if err := s.exec(ctx, a.exec("sysmail_delete_mailitems_sp")); err != nil {
		return fmt.Errorf("gosmo: delete mail items: %w", err)
	}
	return nil
}

// DeleteMailLog deletes Database Mail log rows logged before before and,
// when eventType is set, of that type — sysmail_delete_log_sp. Both zero
// deletes the whole log. Purge items first and the log last: the item purge
// logs a row of its own.
func (s *Server) DeleteMailLog(ctx context.Context, before time.Time, eventType MailEventType) error {
	var a procArgs
	if !before.IsZero() {
		a.raw("@logged_before", mailDateLiteral(before))
	}
	if eventType != "" {
		a.str("@event_type", string(eventType))
	}
	if err := s.exec(ctx, a.exec("sysmail_delete_log_sp")); err != nil {
		return fmt.Errorf("gosmo: delete mail log: %w", err)
	}
	return nil
}
