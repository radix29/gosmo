package gosmo

import (
	"errors"
	"strings"
	"testing"
)

func TestBuildMailAccountScript(t *testing.T) {
	a := &MailAccount{Name: "ops", EmailAddress: "ops@example.com", ServerName: "smtp", Port: 25,
		UserName: "u", CredentialID: 65536, Timeout: 30}
	got := buildMailAccountScript(a, ScriptOptions{IncludeIfNotExists: true})
	want := mailPasswordNote +
		"IF NOT EXISTS (SELECT 1 FROM msdb.dbo.sysmail_account WHERE name = N'ops')\n" +
		"EXEC msdb.dbo.sysmail_add_account_sp @account_name = N'ops', @email_address = N'ops@example.com', " +
		"@mailserver_name = N'smtp', @port = 25, @enable_ssl = 0, @use_default_credentials = 0, " +
		"@username = N'u', @password = N'<password>';\nGO\n" +
		"EXEC msdb.dbo.sysmail_update_account_sp @account_name = N'ops', @timeout = 30, @no_credential_change = 1;\nGO\n"
	if got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}

	anon := &MailAccount{Name: "anon", EmailAddress: "a@b", ServerName: "smtp"}
	if got := buildMailAccountScript(anon, ScriptOptions{}); strings.Contains(got, "password") || strings.Contains(got, "IF NOT EXISTS") {
		t.Errorf("anonymous, unguarded script = %q; want no password and no guard", got)
	}
	drop := buildMailAccountScript(anon, ScriptOptions{Verb: ScriptDrop})
	if drop != "IF EXISTS (SELECT 1 FROM msdb.dbo.sysmail_account WHERE name = N'anon')\n"+
		"    EXEC msdb.dbo.sysmail_delete_account_sp @account_name = N'anon';\nGO\n" {
		t.Errorf("drop script = %q", drop)
	}
}

func TestBuildMailProfileScript(t *testing.T) {
	p := &MailProfile{ProfileID: 4, Name: "alerts", Description: "d", Accounts: []*MailProfileAccount{
		{AccountName: "b", SequenceNumber: 1}, {AccountName: "a", SequenceNumber: 5}}}
	grants := []*MailPrincipalProfile{
		{ProfileID: 4, ProfileName: "alerts", PrincipalSID: []byte{0}, PrincipalName: "public", IsDefault: true},
		{ProfileID: 4, ProfileName: "alerts", PrincipalSID: []byte{1, 2}, PrincipalName: "app"},
		{ProfileID: 4, ProfileName: "alerts", PrincipalSID: []byte{9}, PrincipalName: ""},
		{ProfileID: 5, ProfileName: "other", PrincipalSID: []byte{0}, PrincipalName: "public"},
	}
	got := buildMailProfileScript(p, grants, ScriptOptions{})
	for _, want := range []string{
		"EXEC msdb.dbo.sysmail_add_profile_sp @profile_name = N'alerts', @description = N'd';\nGO\n",
		"@account_name = N'b', @sequence_number = 1;\nGO\n",
		"@account_name = N'a', @sequence_number = 5;\nGO\n",
		"@principal_name = N'public', @profile_name = N'alerts', @is_default = 1;\nGO\n",
		"@principal_name = N'app', @profile_name = N'alerts', @is_default = 0;\nGO\n",
		"-- A grant to a principal no longer in msdb (SID 0x09) is not scripted.\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("profile script lacks %q:\n%s", want, got)
		}
	}
	if strings.Index(got, "N'b'") > strings.Index(got, "N'a'") || strings.Contains(got, "other") {
		t.Errorf("profile script out of sequence order or holding another profile's grant:\n%s", got)
	}
	guarded := buildMailProfileScript(p, grants, ScriptOptions{IncludeIfNotExists: true})
	if n := strings.Count(guarded, "IF NOT EXISTS"); n != 5 {
		t.Errorf("guarded profile script has %d guards, want 5 (profile, two accounts, two grants):\n%s", n, guarded)
	}
}

func TestBuildDatabaseMailScript(t *testing.T) {
	cfg := &MailConfiguration{AccountRetryAttempts: 1, AccountRetryDelay: 60, DatabaseMailExeMinimumLifeTime: 600,
		DefaultAttachmentEncoding: "MIME", LoggingLevel: MailLoggingExtended, MaxFileSize: 1000000,
		ProhibitedExtensions: "exe,dll"}
	got := buildDatabaseMailScript(nil, nil, nil, cfg, ScriptOptions{})
	if n := strings.Count(got, "sysmail_configure_sp"); n != 7 {
		t.Errorf("configuration script has %d parameters, want 7:\n%s", n, got)
	}
	if !strings.Contains(got, "@parameter_name = N'LoggingLevel', @parameter_value = N'2'") {
		t.Errorf("logging level not scripted as its number:\n%s", got)
	}
	sc := NewServerScripter(&Server{}, ScriptOptions{Verb: ScriptDropAndCreate})
	if _, err := sc.ScriptDatabaseMail(t.Context()); !errors.Is(err, ErrUnsupported) {
		t.Errorf("DROP of the configuration = %v, want ErrUnsupported", err)
	}
}
