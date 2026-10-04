package gosmo

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestMailAccountStatementShapes(t *testing.T) {
	s := &Server{}
	acct := s.MailAccountRef("ops]x")
	cases := []struct {
		name  string
		write func(ctx context.Context) error
		want  []string
	}{
		{"create anonymous", func(ctx context.Context) error {
			_, err := s.CreateMailAccount(ctx, CreateMailAccountRequest{
				Name: "ops", EmailAddress: "ops@example.com", ServerName: "smtp.example.com"})
			return err
		}, []string{"EXEC msdb.dbo.sysmail_add_account_sp @account_name = N'ops', @email_address = N'ops@example.com', " +
			"@mailserver_name = N'smtp.example.com', @enable_ssl = 0, @use_default_credentials = 0"}},
		// No @timeout on the add procedure: a timeout is a second statement,
		// which keeps the credential — sent with the add as one atomic batch,
		// so a failed update leaves no account behind.
		{"create windows with timeout", func(ctx context.Context) error {
			_, err := s.CreateMailAccount(ctx, CreateMailAccountRequest{
				Name: "o'ps", EmailAddress: "ops@example.com", DisplayName: "Ops", ReplyToAddress: "r@example.com",
				Description: "d", ServerName: "smtp", Port: 587, EnableSSL: true, Timeout: 30,
				Credentials: MailCredentials{Authentication: MailAuthWindows, UserName: "ignored"}})
			return err
		}, []string{atomicBatch([]string{
			"EXEC msdb.dbo.sysmail_add_account_sp @account_name = N'o''ps', @email_address = N'ops@example.com', " +
				"@display_name = N'Ops', @replyto_address = N'r@example.com', @description = N'd', " +
				"@mailserver_name = N'smtp', @port = 587, @enable_ssl = 1, @use_default_credentials = 1",
			"EXEC msdb.dbo.sysmail_update_account_sp @account_name = N'o''ps', @timeout = 30, @no_credential_change = 1",
		})}},
		// Alter with no Credentials must keep the stored ones: an omitted
		// @username alone would drop them.
		{"alter keeps credentials", func(ctx context.Context) error {
			return acct.Alter(ctx, MailAccountOptions{Port: new(25), DisplayName: new("")})
		}, []string{"EXEC msdb.dbo.sysmail_update_account_sp @account_name = N'ops]x', @display_name = N'', " +
			"@port = 25, @no_credential_change = 1"}},
		{"alter to anonymous", func(ctx context.Context) error {
			return acct.Alter(ctx, MailAccountOptions{Credentials: &MailCredentials{Authentication: MailAuthAnonymous}})
		}, []string{"EXEC msdb.dbo.sysmail_update_account_sp @account_name = N'ops]x', @use_default_credentials = 0"}},
		{"alter rename", func(ctx context.Context) error {
			return acct.Alter(ctx, MailAccountOptions{Name: new("ops2")})
		}, []string{"DECLARE @account_id int = (SELECT account_id FROM msdb.dbo.sysmail_account WHERE name = N'ops]x');\n" +
			"IF @account_id IS NULL RAISERROR(14607, 16, 1, 'account') ELSE " +
			"EXEC msdb.dbo.sysmail_update_account_sp @account_id = @account_id, @account_name = N'ops2', @no_credential_change = 1"}},
		{"alter nothing", func(ctx context.Context) error { return acct.Alter(ctx, MailAccountOptions{}) }, nil},
		{"drop", acct.Drop, []string{"EXEC msdb.dbo.sysmail_delete_account_sp @account_name = N'ops]x'"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := rgStatements(t, c.write); !slices.Equal(got, c.want) {
				t.Errorf("got  %q\nwant %q", got, c.want)
			}
		})
	}
}

func TestMailAccountRefusals(t *testing.T) {
	s := &Server{}
	for name, req := range map[string]CreateMailAccountRequest{
		"no name":            {EmailAddress: "a@b", ServerName: "x"},
		"no address":         {Name: "a", ServerName: "x"},
		"no server":          {Name: "a", EmailAddress: "a@b"},
		"basic without user": {Name: "a", EmailAddress: "a@b", ServerName: "x", Credentials: MailCredentials{Authentication: MailAuthBasic, Password: "p"}},
	} {
		ctx, col := WithScript(context.Background())
		if _, err := s.CreateMailAccount(ctx, req); err == nil || len(col.Statements()) != 0 {
			t.Errorf("%s: err %v, statements %q; want a refusal before any statement", name, err, col.Statements())
		}
	}
	ctx, col := WithScript(context.Background())
	if err := s.MailAccountRef("a").Alter(ctx, MailAccountOptions{Name: new(" ")}); err == nil || len(col.Statements()) != 0 {
		t.Errorf("rename to blank: err %v, statements %q", err, col.Statements())
	}
}

// The password reaches the server and nothing else: a WithScript capture and
// a statement observer both see the placeholder.
func TestMailPasswordNeverCapturedOrObserved(t *testing.T) {
	const secret = "s3cr3t'pw"
	basic := MailCredentials{Authentication: MailAuthBasic, UserName: " smtp_user ", Password: secret}
	writes := map[string]func(ctx context.Context, s *Server) error{
		"create": func(ctx context.Context, s *Server) error {
			_, err := s.CreateMailAccount(ctx, CreateMailAccountRequest{
				Name: "a", EmailAddress: "a@b", ServerName: "x", Credentials: basic})
			return err
		},
		"alter": func(ctx context.Context, s *Server) error {
			return s.MailAccountRef("a").Alter(ctx, MailAccountOptions{Credentials: &basic})
		},
	}
	for name, write := range writes {
		ctx, col := WithScript(context.Background())
		if err := write(ctx, &Server{}); err != nil {
			t.Fatalf("%s under WithScript: %v", name, err)
		}
		script := col.String()
		if strings.Contains(script, "s3cr3t") || !strings.Contains(script, "@password = N'<insert password here>'") {
			t.Errorf("%s: captured script %q; want the placeholder and not the password", name, script)
		}

		s := captureServer(t, 17)
		octx, got := observed(context.Background())
		if err := write(octx, s); err != nil && name == "alter" {
			t.Fatalf("%s: %v", name, err)
		}
		if ran := captured.find("@password"); !strings.Contains(ran, "N's3cr3t''pw'") {
			t.Errorf("%s: the statement run was %q; want the real password", name, ran)
		}
		for _, e := range *got {
			if strings.Contains(e.SQL, "s3cr3t") {
				t.Errorf("%s: observer saw the password: %q", name, e.SQL)
			}
		}
		if len(*got) == 0 || !strings.Contains((*got)[0].SQL, PasswordPlaceholder) {
			t.Errorf("%s: observed %q; want the placeholder form", name, observedSQL(*got))
		}
	}
}

func TestMailProfileStatementShapes(t *testing.T) {
	s := &Server{}
	p := s.MailProfileRef("alerts")
	cases := []struct {
		name  string
		write func(ctx context.Context) error
		want  []string
	}{
		{"create", func(ctx context.Context) error {
			_, err := s.CreateMailProfile(ctx, CreateMailProfileRequest{Name: "alerts", Description: "d"})
			return err
		}, []string{"EXEC msdb.dbo.sysmail_add_profile_sp @profile_name = N'alerts', @description = N'd'"}},
		{"describe", func(ctx context.Context) error {
			return p.Alter(ctx, MailProfileOptions{Description: new("")})
		}, []string{"EXEC msdb.dbo.sysmail_update_profile_sp @profile_name = N'alerts', @description = N''"}},
		{"rename", func(ctx context.Context) error {
			return p.Alter(ctx, MailProfileOptions{Name: new("pages")})
		}, []string{"DECLARE @profile_id int = (SELECT profile_id FROM msdb.dbo.sysmail_profile WHERE name = N'alerts');\n" +
			"IF @profile_id IS NULL RAISERROR(14607, 16, 1, 'profile') ELSE " +
			"EXEC msdb.dbo.sysmail_update_profile_sp @profile_id = @profile_id, @profile_name = N'pages'"}},
		{"drop", p.Drop, []string{"EXEC msdb.dbo.sysmail_delete_profile_sp @profile_name = N'alerts'"}},
		{"add account", func(ctx context.Context) error { return p.AddAccount(ctx, "ops", 2) },
			[]string{"EXEC msdb.dbo.sysmail_add_profileaccount_sp @profile_name = N'alerts', @account_name = N'ops', @sequence_number = 2"}},
		{"remove account", func(ctx context.Context) error { return p.RemoveAccount(ctx, "ops") },
			[]string{"EXEC msdb.dbo.sysmail_delete_profileaccount_sp @profile_name = N'alerts', @account_name = N'ops'"}},
		{"resequence", func(ctx context.Context) error { return p.SetAccountSequence(ctx, "ops", 1) },
			[]string{"EXEC msdb.dbo.sysmail_update_profileaccount_sp @profile_name = N'alerts', @account_name = N'ops', @sequence_number = 1"}},
		{"grant public default", func(ctx context.Context) error { return p.Grant(ctx, MailPublicPrincipal, true) },
			[]string{"EXEC msdb.dbo.sysmail_add_principalprofile_sp @principal_name = N'public', @profile_name = N'alerts', @is_default = 1"}},
		{"set default", func(ctx context.Context) error { return p.SetGrantDefault(ctx, "app", false) },
			[]string{"EXEC msdb.dbo.sysmail_update_principalprofile_sp @principal_name = N'app', @profile_name = N'alerts', @is_default = 0"}},
		{"revoke", func(ctx context.Context) error { return p.Revoke(ctx, "app") },
			[]string{"EXEC msdb.dbo.sysmail_delete_principalprofile_sp @principal_name = N'app', @profile_name = N'alerts'"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := rgStatements(t, c.write); !slices.Equal(got, c.want) {
				t.Errorf("got  %q\nwant %q", got, c.want)
			}
		})
	}
}

// Grant mirrors the public grant onto the profile, and a scripted one does
// not.
func TestMailProfileGrantMirrorsPublic(t *testing.T) {
	p := (&Server{}).MailProfileRef("alerts")
	ctx, _ := WithScript(context.Background())
	if err := p.Grant(ctx, MailPublicPrincipal, true); err != nil || p.IsPublic || p.IsDefault {
		t.Errorf("scripted Grant: err %v, IsPublic %v, IsDefault %v; want nothing mirrored", err, p.IsPublic, p.IsDefault)
	}
	s := captureServer(t, 17)
	p = s.MailProfileRef("alerts")
	if err := p.Grant(context.Background(), MailPublicPrincipal, true); err != nil || !p.IsPublic || !p.IsDefault {
		t.Errorf("Grant: err %v, IsPublic %v, IsDefault %v; want both set", err, p.IsPublic, p.IsDefault)
	}
	if err := p.Revoke(context.Background(), MailPublicPrincipal); err != nil || p.IsPublic || p.IsDefault {
		t.Errorf("Revoke: err %v, IsPublic %v, IsDefault %v; want both cleared", err, p.IsPublic, p.IsDefault)
	}
}

func TestMailSetAccountsDiff(t *testing.T) {
	cur := []*MailProfileAccount{
		{AccountName: "a", SequenceNumber: 1},
		{AccountName: "b", SequenceNumber: 2},
		{AccountName: "c", SequenceNumber: 7},
	}
	got := mailSetAccountsStmts("", "p", cur, []string{"c", "a", "d"})
	want := []string{
		"EXEC msdb.dbo.sysmail_delete_profileaccount_sp @profile_name = N'p', @account_name = N'b'",
		"EXEC msdb.dbo.sysmail_update_profileaccount_sp @profile_name = N'p', @account_name = N'c', @sequence_number = 1",
		"EXEC msdb.dbo.sysmail_update_profileaccount_sp @profile_name = N'p', @account_name = N'a', @sequence_number = 2",
		"EXEC msdb.dbo.sysmail_add_profileaccount_sp @profile_name = N'p', @account_name = N'd', @sequence_number = 3",
	}
	if !slices.Equal(got, want) {
		t.Errorf("got  %q\nwant %q", got, want)
	}
	if got := mailSetAccountsStmts("", "p", cur[:2], []string{"a", "b"}); len(got) != 0 {
		t.Errorf("an unchanged list issued %q", got)
	}
}

// sysmail matches account names under the server's collation, so the diff
// must too: on a case-insensitive server "smtp1" is the existing "SMTP1"
// link, not a remove and re-add; on a case-sensitive one it is another
// account.
func TestMailSetAccountsDiffFollowsCollation(t *testing.T) {
	cur := []*MailProfileAccount{{AccountName: "SMTP1", SequenceNumber: 1}}
	if got := mailSetAccountsStmts("SQL_Latin1_General_CP1_CI_AS", "p", cur, []string{"smtp1"}); len(got) != 0 {
		t.Errorf("CI: a differently cased existing link issued %q", got)
	}
	got := mailSetAccountsStmts("Latin1_General_CS_AS", "p", cur, []string{"smtp1"})
	want := []string{
		"EXEC msdb.dbo.sysmail_delete_profileaccount_sp @profile_name = N'p', @account_name = N'SMTP1'",
		"EXEC msdb.dbo.sysmail_add_profileaccount_sp @profile_name = N'p', @account_name = N'smtp1', @sequence_number = 1",
	}
	if !slices.Equal(got, want) {
		t.Errorf("CS: got  %q\nwant %q", got, want)
	}
}

// A list naming one account twice in two casings is refused before anything
// is read or sent on a case-insensitive server, and allowed on a
// case-sensitive one.
func TestMailSetAccountsRefusesCaseDuplicate(t *testing.T) {
	ctx, col := WithScript(context.Background())
	s := captureServer(t, 17)
	s.info.Collation = "SQL_Latin1_General_CP1_CI_AS"
	err := s.MailProfileRef("p").SetAccounts(ctx, []string{"a", "A"})
	if err == nil || !strings.Contains(err.Error(), `account "A" is listed twice`) {
		t.Fatalf("CI: err %v, want the listed-twice refusal", err)
	}
	if len(col.Statements()) != 0 || captured.count("") != 0 {
		t.Errorf("CI: the refusal sent %q / %q", col.Statements(), captured.find(""))
	}
	s.info.Collation = "Latin1_General_CS_AS"
	if err := s.MailProfileRef("p").SetAccounts(ctx, []string{"a", "A"}); err != nil {
		t.Errorf("CS: %v", err)
	}
}

// SetAccounts sends the whole diff as one all-or-nothing batch.
func TestMailSetAccountsIsAtomic(t *testing.T) {
	ctx, col := WithScript(context.Background())
	s := captureServer(t, 17)
	if err := s.MailProfileRef("p").SetAccounts(ctx, []string{"a", "b"}); err != nil {
		t.Fatalf("SetAccounts: %v", err)
	}
	stmts := col.Statements()
	if len(stmts) != 1 || !strings.HasPrefix(stmts[0], "SET XACT_ABORT ON;") ||
		strings.Count(stmts[0], "sysmail_add_profileaccount_sp") != 2 {
		t.Errorf("SetAccounts on a profile with no links = %q; want one atomic batch adding both", stmts)
	}
	if err := s.MailProfileRef("p").SetAccounts(ctx, []string{"a", "a"}); err == nil {
		t.Error("SetAccounts with a duplicate returned no error")
	}
}

func TestMailServerStatementShapes(t *testing.T) {
	s := &Server{}
	before := time.Date(2026, 10, 1, 13, 4, 5, 0, time.UTC)
	cases := []struct {
		name  string
		write func(ctx context.Context) error
		want  []string
	}{
		{"configure", func(ctx context.Context) error {
			return s.SetMailConfiguration(ctx, MailConfigurationOptions{
				AccountRetryAttempts: new(0), LoggingLevel: new(MailLoggingVerbose), ProhibitedExtensions: new("exe,dll")})
		}, []string{
			"EXEC msdb.dbo.sysmail_configure_sp @parameter_name = N'AccountRetryAttempts', @parameter_value = N'0'",
			"EXEC msdb.dbo.sysmail_configure_sp @parameter_name = N'LoggingLevel', @parameter_value = N'3'",
			"EXEC msdb.dbo.sysmail_configure_sp @parameter_name = N'ProhibitedExtensions', @parameter_value = N'exe,dll'",
		}},
		{"start", s.StartDatabaseMail, []string{"EXEC msdb.dbo.sysmail_start_sp"}},
		{"stop", s.StopDatabaseMail, []string{"EXEC msdb.dbo.sysmail_stop_sp"}},
		{"send", func(ctx context.Context) error {
			_, err := s.SendMail(ctx, MailMessage{To: "a@b", Subject: "Database Mail Test", Body: "it's a test"})
			return err
		}, []string{"DECLARE @mailitem_id int;\nEXEC msdb.dbo.sp_send_dbmail @recipients = N'a@b', " +
			"@subject = N'Database Mail Test', @body = N'it''s a test', @mailitem_id = @mailitem_id OUTPUT;\n" +
			"SELECT @mailitem_id AS mailitem_id;"}},
		{"purge items", func(ctx context.Context) error { return s.DeleteMailItems(ctx, before, MailFailed) },
			[]string{"EXEC msdb.dbo.sysmail_delete_mailitems_sp @sent_before = '2026-10-01T13:04:05.000', @sent_status = N'failed'"}},
		{"purge log", func(ctx context.Context) error { return s.DeleteMailLog(ctx, time.Time{}, "") },
			[]string{"EXEC msdb.dbo.sysmail_delete_log_sp"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := rgStatements(t, c.write); !slices.Equal(got, c.want) {
				t.Errorf("got  %q\nwant %q", got, c.want)
			}
		})
	}
	ctx, col := WithScript(context.Background())
	if err := s.SetMailConfiguration(ctx, MailConfigurationOptions{LoggingLevel: new(MailLoggingLevel(4))}); err == nil || len(col.Statements()) != 0 {
		t.Errorf("logging level 4: err %v, statements %q; want a refusal", err, col.Statements())
	}
}

// -- eofDriver: every query "runs", then the connection breaks ----------------

// eofQueries counts the statements eofDriver was handed.
var eofQueries atomic.Int32

type eofDriver struct{}

func (eofDriver) Open(string) (driver.Conn, error) { return eofConn{}, nil }

type eofConn struct{}

func (eofConn) Prepare(string) (driver.Stmt, error) { return nil, driver.ErrSkip }
func (eofConn) Close() error                        { return nil }
func (eofConn) Begin() (driver.Tx, error)           { return nil, driver.ErrSkip }

// QueryContext fails with io.EOF — what a connection broken after the server
// ran the statement, before the result came back, surfaces as. IsRetryable
// accepts it.
func (eofConn) QueryContext(context.Context, string, []driver.NamedValue) (driver.Rows, error) {
	eofQueries.Add(1)
	return nil, io.EOF
}

func init() { sql.Register("eofafterrun", eofDriver{}) }

// A send is a write, so a connection lost after the server queued the message
// must not be retried: SendMail ran through the retrying read helper and
// a broken connection queued the message up to three times.
func TestSendMailIsNotRetried(t *testing.T) {
	db, err := sql.Open("eofafterrun", "")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()
	if !IsRetryable(io.EOF) {
		t.Fatal("io.EOF is no longer retryable; the test no longer exercises the retry path")
	}
	eofQueries.Store(0)
	s := &Server{db: db}
	_, err = s.SendMail(context.Background(), MailMessage{To: "a@b", Subject: "s", Body: "b"})
	if !errors.Is(err, io.EOF) {
		t.Errorf("err = %v, want the io.EOF surfaced", err)
	}
	if n := eofQueries.Load(); n != 1 {
		t.Errorf("sp_send_dbmail sent %d times, want 1", n)
	}
}

// With a timeout an account is two statements, sent as one atomic batch that
// runs with the password and is observed with the placeholder: one at a
// time, a failed update left the account behind an error.
func TestMailAccountWithTimeoutIsOneAtomicBatch(t *testing.T) {
	s := captureServer(t, 17)
	ctx, got := observed(t.Context())
	_, err := s.CreateMailAccount(ctx, CreateMailAccountRequest{Name: "a", EmailAddress: "a@b", ServerName: "x", Timeout: 30,
		Credentials: MailCredentials{Authentication: MailAuthBasic, UserName: "u", Password: "s3cr3t"}})
	if err != nil {
		t.Fatalf("CreateMailAccount: %v", err)
	}
	ran := captured.find("sysmail_add_account_sp")
	if !strings.HasPrefix(ran, "SET XACT_ABORT ON;") || !strings.Contains(ran, "sysmail_update_account_sp") ||
		!strings.Contains(ran, "N's3cr3t'") || captured.count("sysmail_update_account_sp") != 1 {
		t.Errorf("ran %q; want one atomic batch with the add, the update and the password", ran)
	}
	if len(*got) != 1 || strings.Contains((*got)[0].SQL, "s3cr3t") || !strings.Contains((*got)[0].SQL, PasswordPlaceholder) ||
		!strings.HasPrefix((*got)[0].SQL, "SET XACT_ABORT ON;") {
		t.Errorf("observed %q; want the one batch with the placeholder", observedSQL(*got))
	}
}
