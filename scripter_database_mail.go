package gosmo

import (
	"context"
	"fmt"
	"strings"
)

// ============================================================
// ServerScripter — Database Mail
// ============================================================

// scripter_database_mail.go scripts Database Mail accounts, profiles and
// the whole configuration as the sysmail_* procedure calls that create them —
// the same statements CreateMailAccount, CreateMailProfile, AddAccount, Grant
// and SetMailConfiguration send, built by the same functions.
//
// An account's password cannot be read, so an account with Basic
// authentication scripts @password = N'<insert password here>' under a comment saying
// so, as SSMS does.
//
// IncludeIfNotExists guards each procedure call with its own existence test:
// unlike DDL, every sysmail_add_* fails outright on a duplicate.

// mailExistsGuard is "IF NOT EXISTS (SELECT 1 FROM <from> WHERE <where>)\n".
func mailExistsGuard(from, where string) string {
	return "IF NOT EXISTS (SELECT 1 FROM " + from + " WHERE " + where + ")\n"
}

func mailAccountWhere(name string) string { return "name = " + QuoteLiteral(name) }

// mailStmt writes one procedure call as its own batch, behind guard when
// opts asks for guards.
func mailStmt(sb *strings.Builder, opts ScriptOptions, guard, stmt string) {
	if opts.IncludeIfNotExists && guard != "" {
		sb.WriteString(guard)
	}
	sb.WriteString(stmt)
	sb.WriteString(";\nGO\n")
}

// -- Accounts ----------------------------------------------------------------------

// ScriptMailAccount generates the CREATE (or DROP) script for one Database
// Mail account.
func (sc *ServerScripter) ScriptMailAccount(ctx context.Context, name string) (string, error) {
	a, err := sc.server.MailAccountByName(ctx, name)
	if err != nil {
		return "", err
	}
	return buildMailAccountScript(a, sc.opts), nil
}

// createRequest is the request that recreates the account; the password is
// filled in by mailAccountCreateStmts' shown form.
func (a *MailAccount) createRequest() CreateMailAccountRequest {
	return CreateMailAccountRequest{
		Name:           a.Name,
		EmailAddress:   a.EmailAddress,
		DisplayName:    a.DisplayName,
		ReplyToAddress: a.ReplyToAddress,
		Description:    a.Description,
		ServerName:     a.ServerName,
		Port:           a.Port,
		EnableSSL:      a.EnableSSL,
		Timeout:        a.Timeout,
		Credentials:    MailCredentials{Authentication: a.Authentication(), UserName: a.UserName},
	}
}

func buildMailAccountScript(a *MailAccount, opts ScriptOptions) string {
	drop := "IF EXISTS (SELECT 1 FROM msdb.dbo.sysmail_account WHERE " + mailAccountWhere(a.Name) + ")\n" +
		"    EXEC msdb.dbo.sysmail_delete_account_sp @account_name = " + QuoteLiteral(a.Name) + ";\nGO\n"
	return opts.envelope(drop, "", func(sb *strings.Builder) {
		writeMailAccountCreate(sb, a, opts)
	})
}

func writeMailAccountCreate(sb *strings.Builder, a *MailAccount, opts ScriptOptions) {
	if a.Authentication() == MailAuthBasic {
		sb.WriteString(mailPasswordNote)
	}
	_, shown := mailAccountCreateStmts(a.createRequest())
	for i, stmt := range shown {
		guard := ""
		if i == 0 {
			guard = mailExistsGuard("msdb.dbo.sysmail_account", mailAccountWhere(a.Name))
		}
		mailStmt(sb, opts, guard, stmt)
	}
}

// -- Profiles ----------------------------------------------------------------------

// ScriptMailProfile generates the CREATE (or DROP) script for one Database
// Mail profile: the profile, its accounts in their stored sequence, and its
// grants. The accounts themselves are not scripted with it and must exist.
func (sc *ServerScripter) ScriptMailProfile(ctx context.Context, name string) (string, error) {
	p, err := sc.server.MailProfileByName(ctx, name)
	if err != nil {
		return "", err
	}
	grants, err := sc.server.MailPrincipalProfiles(ctx)
	if err != nil {
		return "", err
	}
	return buildMailProfileScript(p, grants, sc.opts), nil
}

func buildMailProfileScript(p *MailProfile, grants []*MailPrincipalProfile, opts ScriptOptions) string {
	drop := "IF EXISTS (SELECT 1 FROM msdb.dbo.sysmail_profile WHERE name = " + QuoteLiteral(p.Name) + ")\n" +
		"    EXEC msdb.dbo.sysmail_delete_profile_sp @profile_name = " + QuoteLiteral(p.Name) + ";\nGO\n"
	return opts.envelope(drop, "", func(sb *strings.Builder) {
		writeMailProfileCreate(sb, p, grants, opts)
	})
}

func writeMailProfileCreate(sb *strings.Builder, p *MailProfile, grants []*MailPrincipalProfile, opts ScriptOptions) {
	profile := QuoteLiteral(p.Name)
	mailStmt(sb, opts, mailExistsGuard("msdb.dbo.sysmail_profile", "name = "+profile),
		mailProfileCreateStmt(p.Name, p.Description))

	for _, pa := range p.Accounts {
		seq := pa.SequenceNumber
		guard := mailExistsGuard("msdb.dbo.sysmail_profileaccount pa\n"+
			"              JOIN msdb.dbo.sysmail_profile p ON p.profile_id = pa.profile_id\n"+
			"              JOIN msdb.dbo.sysmail_account a ON a.account_id = pa.account_id",
			"p.name = "+profile+" AND a.name = "+QuoteLiteral(pa.AccountName))
		mailStmt(sb, opts, guard, mailProfileAccountStmt("sysmail_add_profileaccount_sp", p.Name, pa.AccountName, &seq))
	}

	for _, g := range grants {
		if g.ProfileID != p.ProfileID {
			continue
		}
		if g.PrincipalName == "" {
			fmt.Fprintf(sb, "-- A grant to a principal no longer in msdb (SID 0x%X) is not scripted.\n", g.PrincipalSID)
			continue
		}
		sid := "0x00"
		if !g.IsPublic() {
			sid = "(SELECT sid FROM msdb.sys.database_principals WHERE name = " + QuoteLiteral(g.PrincipalName) + ")"
		}
		guard := mailExistsGuard("msdb.dbo.sysmail_principalprofile pp\n"+
			"              JOIN msdb.dbo.sysmail_profile p ON p.profile_id = pp.profile_id",
			"p.name = "+profile+" AND pp.principal_sid = "+sid)
		isDefault := g.IsDefault
		mailStmt(sb, opts, guard, mailPrincipalProfileStmt("sysmail_add_principalprofile_sp", p.Name, g.PrincipalName, &isDefault))
	}
}

// -- The whole configuration ---------------------------------------------------------

// ScriptDatabaseMail scripts the whole Database Mail configuration: every
// account, every profile with its accounts and grants, then all seven system
// parameters. 'Database Mail XPs' is a server option and is not part of it.
// DROP is refused with ErrUnsupported — there is no single statement to
// write, and "drop every account" is not what a configuration script is for.
func (sc *ServerScripter) ScriptDatabaseMail(ctx context.Context) (string, error) {
	if v := sc.opts.verb(); v == ScriptDrop || v == ScriptDropAndCreate {
		return "", unsupportedf("gosmo: script database mail: the configuration cannot be dropped")
	}
	accts, err := sc.server.MailAccounts(ctx)
	if err != nil {
		return "", err
	}
	profiles, err := sc.server.MailProfiles(ctx)
	if err != nil {
		return "", err
	}
	grants, err := sc.server.MailPrincipalProfiles(ctx)
	if err != nil {
		return "", err
	}
	cfg, err := sc.server.MailConfiguration(ctx)
	if err != nil {
		return "", err
	}
	return buildDatabaseMailScript(accts, profiles, grants, cfg, sc.opts), nil
}

func buildDatabaseMailScript(accts []*MailAccount, profiles []*MailProfile, grants []*MailPrincipalProfile,
	cfg *MailConfiguration, opts ScriptOptions) string {
	var sb strings.Builder
	sb.WriteString("-- Database Mail accounts\n")
	if len(accts) == 0 {
		sb.WriteString("-- (none)\n")
	}
	for _, a := range accts {
		writeMailAccountCreate(&sb, a, opts)
	}
	sb.WriteString("\n-- Database Mail profiles\n")
	if len(profiles) == 0 {
		sb.WriteString("-- (none)\n")
	}
	for _, p := range profiles {
		writeMailProfileCreate(&sb, p, grants, opts)
	}
	sb.WriteString("\n-- Database Mail system parameters\n")
	for _, stmt := range cfg.options().stmts() {
		mailStmt(&sb, opts, "", stmt)
	}
	return sb.String()
}

// options is every parameter of c, set — what restores it in full.
func (c *MailConfiguration) options() MailConfigurationOptions {
	return MailConfigurationOptions{
		AccountRetryAttempts:           new(c.AccountRetryAttempts),
		AccountRetryDelay:              new(c.AccountRetryDelay),
		DatabaseMailExeMinimumLifeTime: new(c.DatabaseMailExeMinimumLifeTime),
		DefaultAttachmentEncoding:      new(c.DefaultAttachmentEncoding),
		LoggingLevel:                   new(c.LoggingLevel),
		MaxFileSize:                    new(c.MaxFileSize),
		ProhibitedExtensions:           new(c.ProhibitedExtensions),
	}
}
