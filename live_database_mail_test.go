//go:build livedb

// Live Database Mail reads: build disposable accounts, a profile and its grants
// with the raw msdb.dbo.sysmail_* procedures, send one message that is refused
// at once, and read everything back through gosmo — then what a login holding
// only DatabaseMailUserRole is allowed to see.
//
//	go test -tags livedb . -run TestLiveDatabaseMail -v \
//	  -livedb 'sqlserver://sa:PASS@host?TrustServerCertificate=true'
//
// Database Mail is server-wide state on a shared instance. The test runs only
// on one with no mail profile (it sets a public default profile, which would
// displace another), and restores what it changes: 'Database Mail XPs' and
// 'show advanced options', AccountRetryAttempts, the queue's started state,
// and the fixtures. The message's item and the log rows logged during the
// test are deleted directly rather than through sysmail_delete_*_sp, which
// log a row of their own. DatabaseMail.exe, started by the send, logs its
// own exit once DatabaseMailExeMinimumLifeTime runs out — after the test.
package gosmo

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

const (
	liveMailBasic    = "gosmo_live_mail_basic"
	liveMailAnon     = "gosmo_live_mail_anon"
	liveMailWindows  = "gosmo_live_mail_win"
	liveMailProfile  = "gosmo_live_mail_profile"
	liveMailLogin    = "gosmo_live_mail_login"
	liveMailPassword = "P@ssw0rd_gosmo_live_mail"
)

func TestLiveDatabaseMailReads(t *testing.T) {
	db, ctx, done := liveDBTimeout(t, 4*time.Minute)
	t.Cleanup(done) // registered first, so it runs after every restore below

	srv, err := NewServer(ctx, db)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	if ps, err := srv.MailProfiles(ctx); err != nil {
		t.Fatalf("MailProfiles: %v", err)
	} else if len(ps) > 0 {
		t.Skipf("%d mail profile(s) exist — this test sets a public default profile and runs only where none exists", len(ps))
	}

	exec := func(stmt string, args ...any) {
		t.Helper()
		if _, err := db.ExecContext(ctx, stmt, args...); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	cleanup := func(stmt string, args ...any) {
		t.Cleanup(func() {
			if _, err := db.ExecContext(context.Background(), stmt, args...); err != nil {
				t.Errorf("cleanup %s: %v", stmt, err)
			}
		})
	}
	intOf := func(q string) int {
		t.Helper()
		var v int
		if err := db.QueryRowContext(ctx, q).Scan(&v); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		return v
	}

	// -- Fixtures ---------------------------------------------------------------
	// Cleanups run last-registered first, so they are registered in the
	// reverse of the order they must run in.
	xps := intOf("SELECT CAST(value_in_use AS int) FROM sys.configurations WHERE name = N'Database Mail XPs'")
	adv := intOf("SELECT CAST(value_in_use AS int) FROM sys.configurations WHERE name = N'show advanced options'")
	if xps == 0 {
		cleanup(fmt.Sprintf(`EXEC sp_configure 'show advanced options', 1; RECONFIGURE;
EXEC sp_configure 'Database Mail XPs', 0; RECONFIGURE;
EXEC sp_configure 'show advanced options', %d; RECONFIGURE;`, adv))
	}
	startLog := intOf("SELECT ISNULL(MAX(log_id), 0) FROM msdb.dbo.sysmail_log")
	cleanup("DELETE FROM msdb.dbo.sysmail_log WHERE log_id > @p1", startLog)

	cleanup("EXEC msdb.dbo.sysmail_delete_account_sp @account_name = @p1", liveMailBasic)
	cleanup("EXEC msdb.dbo.sysmail_delete_account_sp @account_name = @p1", liveMailAnon)
	cleanup("EXEC msdb.dbo.sysmail_delete_account_sp @account_name = @p1", liveMailWindows)
	exec(`EXEC msdb.dbo.sysmail_add_account_sp @account_name = @p1, @email_address = N'basic@example.com',
     @display_name = N'Basic', @replyto_address = N'reply@example.com', @description = N'gosmo live basic',
     @mailserver_name = N'127.0.0.1', @port = 1, @username = N'gosmo_smtp_user', @password = N'secret',
     @enable_ssl = 1`, liveMailBasic)
	exec(`EXEC msdb.dbo.sysmail_add_account_sp @account_name = @p1, @email_address = N'anon@example.com',
     @mailserver_name = N'127.0.0.1', @port = 1`, liveMailAnon)
	exec(`EXEC msdb.dbo.sysmail_add_account_sp @account_name = @p1, @email_address = N'win@example.com',
     @mailserver_name = N'127.0.0.1', @use_default_credentials = 1`, liveMailWindows)

	// sysmail_delete_profile_sp removes the profile's account links and
	// grants with it.
	cleanup("EXEC msdb.dbo.sysmail_delete_profile_sp @profile_name = @p1", liveMailProfile)
	exec("EXEC msdb.dbo.sysmail_add_profile_sp @profile_name = @p1, @description = N'gosmo live profile'", liveMailProfile)
	// Sequence puts anon first, though basic sorts first by name.
	exec("EXEC msdb.dbo.sysmail_add_profileaccount_sp @profile_name = @p1, @account_name = @p2, @sequence_number = 1",
		liveMailProfile, liveMailAnon)
	exec("EXEC msdb.dbo.sysmail_add_profileaccount_sp @profile_name = @p1, @account_name = @p2, @sequence_number = 5",
		liveMailProfile, liveMailBasic)
	exec("EXEC msdb.dbo.sysmail_add_principalprofile_sp @principal_name = N'public', @profile_name = @p1, @is_default = 1",
		liveMailProfile)

	db.ExecContext(ctx, "IF SUSER_ID(@p1) IS NOT NULL DROP LOGIN ["+liveMailLogin+"]", liveMailLogin)
	exec("CREATE LOGIN [" + liveMailLogin + "] WITH PASSWORD = '" + liveMailPassword + "', CHECK_POLICY = OFF")
	cleanup("DROP LOGIN [" + liveMailLogin + "]")
	exec("EXEC msdb.sys.sp_executesql N'CREATE USER [" + liveMailLogin + "] FOR LOGIN [" + liveMailLogin + "]; " +
		"ALTER ROLE DatabaseMailUserRole ADD MEMBER [" + liveMailLogin + "]'")
	cleanup("EXEC msdb.sys.sp_executesql N'DROP USER [" + liveMailLogin + "]'")
	exec("EXEC msdb.dbo.sysmail_add_principalprofile_sp @principal_name = @p1, @profile_name = @p2, @is_default = 0",
		liveMailLogin, liveMailProfile)

	// -- Accounts ------------------------------------------------------------------
	accts, err := srv.MailAccounts(ctx)
	if err != nil {
		t.Fatalf("MailAccounts: %v", err)
	}
	byName := map[string]*MailAccount{}
	for _, a := range accts {
		byName[a.Name] = a
		if a.Server() != srv {
			t.Errorf("MailAccount %s: Server() is not the server it was read from", a.Name)
		}
	}
	if b := byName[liveMailBasic]; b == nil {
		t.Errorf("MailAccounts has no %s", liveMailBasic)
	} else {
		if b.EmailAddress != "basic@example.com" || b.DisplayName != "Basic" ||
			b.ReplyToAddress != "reply@example.com" || b.Description != "gosmo live basic" ||
			b.ServerType != "SMTP" || b.ServerName != "127.0.0.1" || b.Port != 1 || !b.EnableSSL ||
			b.UserName != "gosmo_smtp_user" || b.CredentialID == 0 || b.UseDefaultCredentials ||
			b.LastModified.IsZero() {
			t.Errorf("basic account read back as %+v", *b)
		}
		if got := b.Authentication(); got != MailAuthBasic {
			t.Errorf("basic account Authentication = %v", got)
		}
	}
	if a := byName[liveMailAnon]; a == nil {
		t.Errorf("MailAccounts has no %s", liveMailAnon)
	} else {
		if a.Port != 1 || a.EnableSSL || a.UserName != "" || a.CredentialID != 0 || a.DisplayName != "" {
			t.Errorf("anonymous account read back as %+v (want port 1, no SSL, no credential)", *a)
		}
		if got := a.Authentication(); got != MailAuthAnonymous {
			t.Errorf("anonymous account Authentication = %v", got)
		}
	}
	if w := byName[liveMailWindows]; w == nil {
		t.Errorf("MailAccounts has no %s", liveMailWindows)
	} else if got := w.Authentication(); got != MailAuthWindows {
		t.Errorf("windows account Authentication = %v (%+v)", got, *w)
	}
	if a, err := srv.MailAccountByName(ctx, liveMailBasic); err != nil || a.UserName != "gosmo_smtp_user" {
		t.Errorf("MailAccountByName = %+v, %v", a, err)
	}
	if _, err := srv.MailAccountByName(ctx, "gosmo_live_mail_absent"); !errors.Is(err, ErrNotFound) {
		t.Errorf("MailAccountByName(absent) = %v, want ErrNotFound", err)
	}

	// -- Profiles and grants -------------------------------------------------------
	p, err := srv.MailProfileByName(ctx, liveMailProfile)
	if err != nil {
		t.Fatalf("MailProfileByName: %v", err)
	}
	if !p.IsPublic || !p.IsDefault || p.Description != "gosmo live profile" || p.Server() != srv {
		t.Errorf("profile read back as %+v", *p)
	}
	var order []string
	for _, pa := range p.Accounts {
		order = append(order, fmt.Sprintf("%s@%d", pa.AccountName, pa.SequenceNumber))
	}
	if want := []string{liveMailAnon + "@1", liveMailBasic + "@5"}; !slices.Equal(order, want) {
		t.Errorf("profile accounts = %v, want %v (sequence order, not name order)", order, want)
	}
	ps, err := srv.MailProfiles(ctx)
	if err != nil || len(ps) != 1 || len(ps[0].Accounts) != 2 {
		t.Errorf("MailProfiles = %d profiles, %v; want the one fixture with two accounts", len(ps), err)
	}
	if _, err := srv.MailProfileByName(ctx, "gosmo_live_mail_absent"); !errors.Is(err, ErrNotFound) {
		t.Errorf("MailProfileByName(absent) = %v, want ErrNotFound", err)
	}

	grants, err := srv.MailPrincipalProfiles(ctx)
	if err != nil {
		t.Fatalf("MailPrincipalProfiles: %v", err)
	}
	var got []string
	for _, g := range grants {
		got = append(got, fmt.Sprintf("%s/%s/public=%v/default=%v", g.ProfileName, g.PrincipalName, g.IsPublic(), g.IsDefault))
	}
	want := []string{
		liveMailProfile + "/public/public=true/default=true",
		liveMailProfile + "/" + liveMailLogin + "/public=false/default=false",
	}
	if !slices.Equal(got, want) {
		t.Errorf("MailPrincipalProfiles = %v, want %v (public named public, not guest)", got, want)
	}

	// -- Configuration ---------------------------------------------------------------
	cfg, err := srv.MailConfiguration(ctx)
	if err != nil {
		t.Fatalf("MailConfiguration: %v", err)
	}
	if cfg.AccountRetryDelay <= 0 || cfg.DatabaseMailExeMinimumLifeTime <= 0 || cfg.MaxFileSize <= 0 ||
		cfg.LoggingLevel < MailLoggingNormal || cfg.LoggingLevel > MailLoggingVerbose ||
		cfg.DefaultAttachmentEncoding == "" || !strings.Contains(cfg.ProhibitedExtensions, "exe") {
		t.Errorf("MailConfiguration = %+v", *cfg)
	}

	// -- Status, with the option off and on ------------------------------------------
	if xps == 0 {
		if st, err := srv.MailStatus(ctx); err != nil || st != MailDisabled {
			t.Errorf("MailStatus with Database Mail XPs 0 = %v, %v; want Disabled and no error", st, err)
		}
		exec(`EXEC sp_configure 'show advanced options', 1; RECONFIGURE;
EXEC sp_configure 'Database Mail XPs', 1; RECONFIGURE;`)
	}
	st, err := srv.MailStatus(ctx)
	if err != nil || st == MailDisabled {
		t.Fatalf("MailStatus with Database Mail XPs 1 = %v, %v", st, err)
	}
	if st == MailStopped {
		exec("EXEC msdb.dbo.sysmail_start_sp")
		cleanup("EXEC msdb.dbo.sysmail_stop_sp")
		if st, err := srv.MailStatus(ctx); err != nil || st != MailStarted {
			t.Fatalf("MailStatus after sysmail_start_sp = %v, %v", st, err)
		}
	}
	queues, err := srv.MailQueues(ctx)
	if err != nil {
		t.Fatalf("MailQueues: %v", err)
	}
	var qtypes []string
	for _, q := range queues {
		qtypes = append(qtypes, q.Type)
	}
	if slices.Sort(qtypes); !slices.Equal(qtypes, []string{"mail", "status"}) {
		t.Errorf("MailQueues types = %v, want mail and status", qtypes)
	}

	// -- A send that fails at once --------------------------------------------------
	// Nothing listens on 127.0.0.1:1, so the connect is refused rather than
	// timing out, and with no retries the item fails within seconds.
	retries := cfg.AccountRetryAttempts
	exec("EXEC msdb.dbo.sysmail_configure_sp @parameter_name = N'AccountRetryAttempts', @parameter_value = N'0'")
	cleanup("EXEC msdb.dbo.sysmail_configure_sp @parameter_name = N'AccountRetryAttempts', @parameter_value = @p1",
		fmt.Sprint(retries))

	var id int
	if err := db.QueryRowContext(ctx, `
DECLARE @id int;
EXEC msdb.dbo.sp_send_dbmail @profile_name = @p1, @recipients = N'nobody@example.com',
     @subject = N'gosmo live', @body = N'gosmo live body', @mailitem_id = @id OUTPUT;
SELECT @id`, liveMailProfile).Scan(&id); err != nil {
		t.Fatalf("sp_send_dbmail: %v", err)
	}
	cleanup("DELETE FROM msdb.dbo.sysmail_mailitems WHERE mailitem_id = @p1", id)

	var item *MailItem
	for deadline := time.Now().Add(2 * time.Minute); ; {
		item, err = srv.MailItemByID(ctx, id)
		if err != nil {
			t.Fatalf("MailItemByID(%d): %v", id, err)
		}
		if item.SentStatus == MailFailed || item.SentStatus == MailSent || time.Now().After(deadline) {
			break
		}
		time.Sleep(time.Second)
	}
	if item.SentStatus != MailFailed || item.Subject != "gosmo live" || item.Body != "gosmo live body" ||
		item.Recipients != "nobody@example.com" || item.ProfileID != p.ProfileID || item.SendRequestDate.IsZero() {
		t.Fatalf("mail item %d = %+v; want failed with the sent fields", id, *item)
	}
	t.Logf("item %d %s after %v", id, item.SentStatus, item.LastModified.Sub(item.SendRequestDate))

	failed, err := srv.MailItems(ctx, MailItemFilter{Status: MailFailed, Max: 5})
	if err != nil || len(failed) == 0 || failed[0].MailItemID != id {
		t.Errorf("MailItems(failed) = %d items, %v; want item %d first", len(failed), err, id)
	}
	if none, err := srv.MailItems(ctx, MailItemFilter{Status: MailSent}); err != nil || slices.ContainsFunc(none, func(m *MailItem) bool { return m.MailItemID == id }) {
		t.Errorf("MailItems(sent) = %v; the failed item is not sent", err)
	}
	if before, err := srv.MailItems(ctx, MailItemFilter{Before: item.SendRequestDate}); err != nil ||
		slices.ContainsFunc(before, func(m *MailItem) bool { return m.MailItemID == id }) {
		t.Errorf("MailItems(Before = its own request date) = %v; want the item left out", err)
	}
	if after, err := srv.MailItems(ctx, MailItemFilter{Before: item.SendRequestDate.Add(time.Second)}); err != nil ||
		!slices.ContainsFunc(after, func(m *MailItem) bool { return m.MailItemID == id }) {
		t.Errorf("MailItems(Before = a second later) = %v; want the item kept", err)
	}

	events, err := srv.MailEvents(ctx, MailEventFilter{MailItemID: id})
	if err != nil {
		t.Fatalf("MailEvents: %v", err)
	}
	var sawError bool
	for _, e := range events {
		if e.MailItemID != id {
			t.Errorf("MailEvents(item %d) returned event %d of item %d", id, e.LogID, e.MailItemID)
		}
		if e.EventType == MailEventError && e.Description != "" && e.LogDate.After(time.Time{}) {
			sawError = true
			t.Logf("error event: %.200s", e.Description)
		}
	}
	if !sawError {
		t.Errorf("MailEvents(item %d) = %d events, none an error with a description", id, len(events))
	}
	if errs, err := srv.MailEvents(ctx, MailEventFilter{EventType: MailEventError, Max: 1}); err != nil || len(errs) != 1 || errs[0].EventType != MailEventError {
		t.Errorf("MailEvents(error, Max 1) = %d, %v", len(errs), err)
	}

	// -- DatabaseMailUserRole only ---------------------------------------------------
	pool, err := sql.Open("sqlserver", liveRestrictedDSN(t, liveMailLogin, liveMailPassword))
	if err != nil {
		t.Fatalf("open as %s: %v", liveMailLogin, err)
	}
	t.Cleanup(func() { pool.Close() })
	// Not NewServer: loadInfo's DMV half needs VIEW SERVER STATE.
	restricted := &Server{db: pool}
	if st, err := restricted.MailStatus(ctx); err != nil || st != MailStarted {
		t.Errorf("role member MailStatus = %v, %v; the role is granted the status proc", st, err)
	}
	if v, err := restricted.MailItems(ctx, MailItemFilter{}); err != nil || len(v) != 0 {
		t.Errorf("role member MailItems = %d, %v; want none of someone else's items and no error", len(v), err)
	}
	if v, err := restricted.MailEvents(ctx, MailEventFilter{}); err != nil || len(v) != 0 {
		t.Errorf("role member MailEvents = %d, %v; want none and no error", len(v), err)
	}
	for name, read := range map[string]func() error{
		"MailAccounts":          func() error { _, err := restricted.MailAccounts(ctx); return err },
		"MailProfiles":          func() error { _, err := restricted.MailProfiles(ctx); return err },
		"MailPrincipalProfiles": func() error { _, err := restricted.MailPrincipalProfiles(ctx); return err },
		"MailConfiguration":     func() error { _, err := restricted.MailConfiguration(ctx); return err },
	} {
		if se, ok := AsSQLError(read()); !ok || se.Number != 229 {
			t.Errorf("role member %s: want Msg 229, got %v", name, read())
		}
	}
}

// TestLiveDatabaseMailWrites drives every Database Mail write through gosmo:
// accounts (the credential kept or dropped as Alter's Credentials says),
// profiles with their account lists and grants, the system parameters,
// start, a test mail that fails at once, the Database Mail log read as an
// error-log family, the scripts run back, and the purges. Same preconditions
// and restoring teardown as TestLiveDatabaseMailReads.
func TestLiveDatabaseMailWrites(t *testing.T) {
	db, ctx, done := liveDBTimeout(t, 4*time.Minute)
	t.Cleanup(done)

	srv, err := NewServer(ctx, db)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	if ps, err := srv.MailProfiles(ctx); err != nil {
		t.Fatalf("MailProfiles: %v", err)
	} else if len(ps) > 0 {
		t.Skipf("%d mail profile(s) exist — this test sets a public default profile and runs only where none exists", len(ps))
	}

	exec := func(stmt string, args ...any) {
		t.Helper()
		if _, err := db.ExecContext(ctx, stmt, args...); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	cleanup := func(stmt string, args ...any) {
		t.Cleanup(func() {
			if _, err := db.ExecContext(context.Background(), stmt, args...); err != nil {
				t.Errorf("cleanup %s: %v", stmt, err)
			}
		})
	}
	intOf := func(q string, args ...any) int {
		t.Helper()
		var v int
		if err := db.QueryRowContext(ctx, q, args...).Scan(&v); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		return v
	}
	account := func(name string) *MailAccount {
		t.Helper()
		a, err := srv.MailAccountByName(ctx, name)
		if err != nil {
			t.Fatalf("MailAccountByName(%s): %v", name, err)
		}
		return a
	}
	profileAccounts := func(name string) []string {
		t.Helper()
		p, err := srv.MailProfileByName(ctx, name)
		if err != nil {
			t.Fatalf("MailProfileByName(%s): %v", name, err)
		}
		var out []string
		for _, pa := range p.Accounts {
			out = append(out, fmt.Sprintf("%s@%d", pa.AccountName, pa.SequenceNumber))
		}
		return out
	}

	// -- Saved state, and fixtures' cleanups (run last-registered first) ---------------
	xps := intOf("SELECT CAST(value_in_use AS int) FROM sys.configurations WHERE name = N'Database Mail XPs'")
	adv := intOf("SELECT CAST(value_in_use AS int) FROM sys.configurations WHERE name = N'show advanced options'")
	if xps == 0 {
		cleanup(fmt.Sprintf(`EXEC sp_configure 'show advanced options', 1; RECONFIGURE;
EXEC sp_configure 'Database Mail XPs', 0; RECONFIGURE;
EXEC sp_configure 'show advanced options', %d; RECONFIGURE;`, adv))
	}
	startLog := intOf("SELECT ISNULL(MAX(log_id), 0) FROM msdb.dbo.sysmail_log")
	cleanup("DELETE FROM msdb.dbo.sysmail_log WHERE log_id > @p1", startLog)
	startItem := intOf("SELECT ISNULL(MAX(mailitem_id), 0) FROM msdb.dbo.sysmail_mailitems")
	cleanup("DELETE FROM msdb.dbo.sysmail_mailitems WHERE mailitem_id > @p1", startItem)
	cfg, err := srv.MailConfiguration(ctx)
	if err != nil {
		t.Fatalf("MailConfiguration: %v", err)
	}
	t.Cleanup(func() {
		if err := srv.SetMailConfiguration(context.Background(), cfg.options()); err != nil {
			t.Errorf("restore mail configuration: %v", err)
		}
	})
	startCreds := intOf("SELECT COUNT(*) FROM sys.credentials")

	renamed := liveMailBasic + "_renamed"
	for _, name := range []string{liveMailBasic, renamed, liveMailAnon, liveMailWindows} {
		cleanup("IF EXISTS (SELECT 1 FROM msdb.dbo.sysmail_account WHERE name = @p1) EXEC msdb.dbo.sysmail_delete_account_sp @account_name = @p1", name)
	}
	renamedProfile := liveMailProfile + "_renamed"
	for _, name := range []string{liveMailProfile, renamedProfile} {
		cleanup("IF EXISTS (SELECT 1 FROM msdb.dbo.sysmail_profile WHERE name = @p1) EXEC msdb.dbo.sysmail_delete_profile_sp @profile_name = @p1", name)
	}

	// -- Accounts -------------------------------------------------------------------------
	basicReq := CreateMailAccountRequest{
		Name: liveMailBasic, EmailAddress: "basic@example.com", DisplayName: "Basic",
		ReplyToAddress: "reply@example.com", Description: "gosmo live basic",
		ServerName: "127.0.0.1", Port: 1, EnableSSL: true, Timeout: 30,
		Credentials: MailCredentials{Authentication: MailAuthBasic, UserName: "gosmo_smtp_user", Password: liveMailPassword},
	}
	b, err := srv.CreateMailAccount(ctx, basicReq)
	if err != nil {
		t.Fatalf("CreateMailAccount(basic): %v", err)
	}
	if b.AccountID == 0 || b.UserName != "gosmo_smtp_user" || b.CredentialID == 0 || b.Timeout != 30 ||
		!b.EnableSSL || b.Port != 1 || b.DisplayName != "Basic" || b.Authentication() != MailAuthBasic {
		t.Errorf("basic account created as %+v", *b)
	}
	if intOf("SELECT COUNT(*) FROM sys.credentials") != startCreds+1 {
		t.Errorf("a Basic account did not store one credential")
	}
	if _, err := srv.CreateMailAccount(ctx, CreateMailAccountRequest{Name: liveMailAnon,
		EmailAddress: "anon@example.com", ServerName: "127.0.0.1", Port: 1}); err != nil {
		t.Fatalf("CreateMailAccount(anon): %v", err)
	}
	if w, err := srv.CreateMailAccount(ctx, CreateMailAccountRequest{Name: liveMailWindows,
		EmailAddress: "win@example.com", ServerName: "127.0.0.1", Port: 1,
		Credentials: MailCredentials{Authentication: MailAuthWindows}}); err != nil || w.Authentication() != MailAuthWindows {
		t.Fatalf("CreateMailAccount(windows) = %+v, %v", w, err)
	}

	// Alter without Credentials keeps the user name and credential — the
	// trap: an omitted @username alone drops them.
	credID := b.CredentialID
	if err := srv.MailAccountRef(liveMailBasic).Alter(ctx, MailAccountOptions{Port: new(2), DisplayName: new("")}); err != nil {
		t.Fatalf("Alter(port, display name): %v", err)
	}
	if got := account(liveMailBasic); got.Port != 2 || got.DisplayName != "" || got.UserName != "gosmo_smtp_user" ||
		got.CredentialID != credID || got.Timeout != 30 || !got.EnableSSL || got.EmailAddress != "basic@example.com" {
		t.Errorf("after Alter(port, display name) the account reads %+v; want the credential and the rest kept", *got)
	}
	// Rename through a name-only handle, then new credentials on it.
	if err := srv.MailAccountRef(liveMailBasic).Alter(ctx, MailAccountOptions{Name: new(renamed),
		Credentials: &MailCredentials{Authentication: MailAuthBasic, UserName: "gosmo_smtp_user2", Password: "other"}}); err != nil {
		t.Fatalf("Alter(rename, credentials): %v", err)
	}
	if got := account(renamed); got.UserName != "gosmo_smtp_user2" || got.CredentialID != credID || got.Port != 2 {
		t.Errorf("after rename + new credentials the account reads %+v", *got)
	}
	if _, err := srv.MailAccountByName(ctx, liveMailBasic); !errors.Is(err, ErrNotFound) {
		t.Errorf("the old name still resolves: %v", err)
	}
	if err := srv.MailAccountRef("gosmo_live_mail_absent").Alter(ctx, MailAccountOptions{Name: new("x")}); err == nil {
		t.Error("renaming a missing account returned no error")
	}

	// -- Profiles ---------------------------------------------------------------------------
	p, err := srv.CreateMailProfile(ctx, CreateMailProfileRequest{Name: liveMailProfile, Description: "gosmo live profile"})
	if err != nil || p.ProfileID == 0 || p.Description != "gosmo live profile" {
		t.Fatalf("CreateMailProfile = %+v, %v", p, err)
	}
	if err := p.SetAccounts(ctx, []string{liveMailAnon, renamed}); err != nil {
		t.Fatalf("SetAccounts: %v", err)
	}
	if got, want := profileAccounts(liveMailProfile), []string{liveMailAnon + "@1", renamed + "@2"}; !slices.Equal(got, want) {
		t.Errorf("profile accounts = %v, want %v", got, want)
	}
	if err := p.SetAccounts(ctx, []string{renamed, liveMailWindows}); err != nil {
		t.Fatalf("SetAccounts (reorder): %v", err)
	}
	if got, want := profileAccounts(liveMailProfile), []string{renamed + "@1", liveMailWindows + "@2"}; !slices.Equal(got, want) {
		t.Errorf("profile accounts after reorder = %v, want %v", got, want)
	}
	// All or nothing: a missing account fails the batch and leaves the list.
	if err := p.SetAccounts(ctx, []string{liveMailAnon, "gosmo_live_mail_absent"}); err == nil {
		t.Error("SetAccounts with a missing account returned no error")
	}
	if got, want := profileAccounts(liveMailProfile), []string{renamed + "@1", liveMailWindows + "@2"}; !slices.Equal(got, want) {
		t.Errorf("a failed SetAccounts changed the list to %v, want %v", got, want)
	}
	if err := p.AddAccount(ctx, liveMailAnon, 9); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}
	if err := p.SetAccountSequence(ctx, liveMailAnon, 3); err != nil {
		t.Fatalf("SetAccountSequence: %v", err)
	}
	if err := p.RemoveAccount(ctx, liveMailWindows); err != nil {
		t.Fatalf("RemoveAccount: %v", err)
	}
	if got, want := profileAccounts(liveMailProfile), []string{renamed + "@1", liveMailAnon + "@3"}; !slices.Equal(got, want) {
		t.Errorf("profile accounts after add/resequence/remove = %v, want %v", got, want)
	}

	// Grants: public default, and a private one to an msdb user.
	db.ExecContext(ctx, "IF SUSER_ID(@p1) IS NOT NULL DROP LOGIN ["+liveMailLogin+"]", liveMailLogin)
	exec("CREATE LOGIN [" + liveMailLogin + "] WITH PASSWORD = '" + liveMailPassword + "', CHECK_POLICY = OFF")
	cleanup("DROP LOGIN [" + liveMailLogin + "]")
	exec("EXEC msdb.sys.sp_executesql N'CREATE USER [" + liveMailLogin + "] FOR LOGIN [" + liveMailLogin + "]'")
	cleanup("EXEC msdb.sys.sp_executesql N'DROP USER [" + liveMailLogin + "]'")

	if err := p.Grant(ctx, MailPublicPrincipal, true); err != nil || !p.IsPublic || !p.IsDefault {
		t.Fatalf("Grant(public, default) = %v; mirrored IsPublic %v IsDefault %v", err, p.IsPublic, p.IsDefault)
	}
	if err := p.Grant(ctx, liveMailLogin, false); err != nil {
		t.Fatalf("Grant(user): %v", err)
	}
	if err := p.SetGrantDefault(ctx, liveMailLogin, true); err != nil {
		t.Fatalf("SetGrantDefault: %v", err)
	}
	grantsOf := func() []string {
		t.Helper()
		gs, err := srv.MailPrincipalProfiles(ctx)
		if err != nil {
			t.Fatalf("MailPrincipalProfiles: %v", err)
		}
		var out []string
		for _, g := range gs {
			out = append(out, fmt.Sprintf("%s/%s/default=%v", g.ProfileName, g.PrincipalName, g.IsDefault))
		}
		return out
	}
	if got, want := grantsOf(), []string{
		liveMailProfile + "/public/default=true", liveMailProfile + "/" + liveMailLogin + "/default=true",
	}; !slices.Equal(got, want) {
		t.Errorf("grants = %v, want %v", got, want)
	}
	if err := p.Grant(ctx, liveMailLogin+"_absent", false); err == nil {
		t.Error("Grant to a principal with no msdb user returned no error")
	}

	if err := p.Alter(ctx, MailProfileOptions{Name: new(renamedProfile), Description: new("")}); err != nil {
		t.Fatalf("profile Alter(rename): %v", err)
	}
	if got, err := srv.MailProfileByName(ctx, renamedProfile); err != nil || got.Description != "" || !got.IsDefault || len(got.Accounts) != 2 {
		t.Errorf("renamed profile reads %+v, %v; want its links and grants kept", got, err)
	}
	if err := p.Alter(ctx, MailProfileOptions{Name: new(liveMailProfile), Description: new("gosmo live profile")}); err != nil {
		t.Fatalf("profile Alter(rename back): %v", err)
	}

	// -- System parameters -----------------------------------------------------------------
	if err := srv.SetMailConfiguration(ctx, MailConfigurationOptions{
		AccountRetryAttempts: new(0), LoggingLevel: new(MailLoggingVerbose)}); err != nil {
		t.Fatalf("SetMailConfiguration: %v", err)
	}
	if got, err := srv.MailConfiguration(ctx); err != nil || got.AccountRetryAttempts != 0 ||
		got.LoggingLevel != MailLoggingVerbose || got.MaxFileSize != cfg.MaxFileSize {
		t.Errorf("configuration after SetMailConfiguration = %+v, %v", got, err)
	}

	// -- Scripts, run back --------------------------------------------------------------------
	// Taken now, while the configuration is at its fullest; run back at the
	// end, after everything has been dropped.
	opts := DefaultScriptOptions()
	sc := NewServerScripter(srv, opts)
	acctScript, err := sc.ScriptMailAccount(ctx, renamed)
	if err != nil {
		t.Fatalf("ScriptMailAccount: %v", err)
	}
	if strings.Contains(acctScript, "other") || !strings.Contains(acctScript, "@password = N'<password>'") {
		t.Errorf("account script carries a password or lacks the placeholder:\n%s", acctScript)
	}
	profScript, err := sc.ScriptMailProfile(ctx, liveMailProfile)
	if err != nil {
		t.Fatalf("ScriptMailProfile: %v", err)
	}
	if !strings.Contains(profScript, "@principal_name = N'public'") || !strings.Contains(profScript, "@principal_name = N'"+liveMailLogin+"'") {
		t.Errorf("profile script lacks a grant:\n%s", profScript)
	}
	allScript, err := sc.ScriptDatabaseMail(ctx)
	if err != nil {
		t.Fatalf("ScriptDatabaseMail: %v", err)
	}
	before := struct {
		accts  []*MailAccount
		prof   []string
		grants []string
		cfg    *MailConfiguration
	}{}
	if before.accts, err = srv.MailAccounts(ctx); err != nil {
		t.Fatalf("MailAccounts: %v", err)
	}
	before.prof, before.grants = profileAccounts(liveMailProfile), grantsOf()
	if before.cfg, err = srv.MailConfiguration(ctx); err != nil {
		t.Fatalf("MailConfiguration: %v", err)
	}

	// -- Start and a send that fails at once -------------------------------------------------
	if xps == 0 {
		if err := srv.StartDatabaseMail(ctx); err == nil {
			t.Error("StartDatabaseMail with Database Mail XPs 0 returned no error")
		}
		exec(`EXEC sp_configure 'show advanced options', 1; RECONFIGURE;
EXEC sp_configure 'Database Mail XPs', 1; RECONFIGURE;`)
	}
	if st, err := srv.MailStatus(ctx); err != nil {
		t.Fatalf("MailStatus: %v", err)
	} else if st == MailStopped {
		cleanup("EXEC msdb.dbo.sysmail_stop_sp")
	}
	if err := srv.StartDatabaseMail(ctx); err != nil {
		t.Fatalf("StartDatabaseMail: %v", err)
	}
	if st, err := srv.MailStatus(ctx); err != nil || st != MailStarted {
		t.Fatalf("MailStatus after StartDatabaseMail = %v, %v", st, err)
	}
	if err := srv.StopDatabaseMail(ctx); err != nil {
		t.Fatalf("StopDatabaseMail: %v", err)
	}
	if st, err := srv.MailStatus(ctx); err != nil || st != MailStopped {
		t.Fatalf("MailStatus after StopDatabaseMail = %v, %v", st, err)
	}
	if err := srv.StartDatabaseMail(ctx); err != nil {
		t.Fatalf("StartDatabaseMail: %v", err)
	}

	// No profile: the caller's default, here public's.
	id, err := srv.SendTestMail(ctx, "", "nobody@example.com", "gosmo live write", "gosmo live write body")
	if err != nil || id == 0 {
		t.Fatalf("SendTestMail = %d, %v", id, err)
	}
	var item *MailItem
	for deadline := time.Now().Add(2 * time.Minute); ; {
		if item, err = srv.MailItemByID(ctx, id); err != nil {
			t.Fatalf("MailItemByID(%d): %v", id, err)
		}
		if item.SentStatus == MailFailed || item.SentStatus == MailSent || time.Now().After(deadline) {
			break
		}
		time.Sleep(time.Second)
	}
	if item.SentStatus != MailFailed || item.ProfileID != p.ProfileID || item.Subject != "gosmo live write" {
		t.Fatalf("mail item %d = %+v; want failed through the public default profile", id, *item)
	}

	// -- The log as an error-log family -------------------------------------------------------
	files, err := srv.EnumErrorLogs(ctx, ErrorLogDatabaseMail)
	if err != nil || len(files) != 1 || files[0].Number != 0 || files[0].LastWritten.IsZero() {
		t.Errorf("EnumErrorLogs(Database Mail) = %+v, %v", files, err)
	}
	entries, err := srv.ReadLogFiltered(ctx, ErrorLogDatabaseMail, 0, LogSearch{
		Text1: "COULD NOT CONNECT", From: item.SendRequestDate.Add(-time.Second)})
	if err != nil {
		t.Fatalf("ReadLogFiltered(Database Mail): %v", err)
	}
	if len(entries) == 0 || entries[0].Process != "error" || entries[0].Source() != "error" ||
		!strings.Contains(strings.ToLower(entries[0].Text), "could not connect") || entries[0].Date.IsZero() {
		t.Errorf("Database Mail log search = %d entries (first %+v); want the send's error", len(entries), entries)
	}
	// An entry's own Date bounds it inclusively both ways — a datetime read
	// back rounded to the millisecond must not fall outside its own range.
	for _, e := range entries {
		if got, err := srv.ReadLogFiltered(ctx, ErrorLogDatabaseMail, 0, LogSearch{From: e.Date, To: e.Date}); err != nil ||
			!slices.ContainsFunc(got, func(g *ErrorLogEntry) bool { return g.Text == e.Text }) {
			t.Errorf("ReadLogFiltered(From = To = %s) = %d entries, %v; want the entry itself", e.LogDate, len(got), err)
		}
	}
	if all, err := srv.ReadLog(ctx, ErrorLogDatabaseMail, 0); err != nil || len(all) < len(entries) ||
		(len(all) > 1 && all[0].Date.After(all[len(all)-1].Date)) {
		t.Errorf("ReadLog(Database Mail) = %d entries, %v; want at least the search's, oldest first", len(all), err)
	}
	if _, err := srv.ReadLog(ctx, ErrorLogDatabaseMail, 1); err == nil {
		t.Error("ReadLog(Database Mail, 1) returned no error")
	}

	// -- Purges -------------------------------------------------------------------------------
	// sysmail_delete_mailitems_sp and sysmail_delete_log_sp take no lower
	// bound, so they run only when nothing older than the test is in reach.
	if intOf("SELECT COUNT(*) FROM msdb.dbo.sysmail_mailitems WHERE mailitem_id <= @p1 AND sent_status = 2", startItem) == 0 {
		if err := srv.DeleteMailItems(ctx, time.Time{}, MailFailed); err != nil {
			t.Errorf("DeleteMailItems: %v", err)
		}
		if _, err := srv.MailItemByID(ctx, id); !errors.Is(err, ErrNotFound) {
			t.Errorf("item %d after DeleteMailItems: %v; want not found", id, err)
		}
	} else {
		t.Log("older failed mail items exist; DeleteMailItems not run")
	}
	if err := srv.DeleteMailItems(ctx, time.Time{}, ""); err == nil {
		t.Error("DeleteMailItems with neither bound returned no error")
	}
	// Earlier runs' DatabaseMail.exe start/exit rows are fair game; anything
	// else is not this test's to delete.
	if intOf(`SELECT COUNT(*) FROM msdb.dbo.sysmail_log WHERE log_id <= @p1
AND description NOT LIKE N'DatabaseMail process is %'`, startLog) == 0 {
		if err := srv.DeleteMailLog(ctx, time.Time{}, MailEventError); err != nil {
			t.Errorf("DeleteMailLog(error): %v", err)
		}
		if n := intOf("SELECT COUNT(*) FROM msdb.dbo.sysmail_log WHERE event_type = 3"); n != 0 {
			t.Errorf("%d error rows after DeleteMailLog(error)", n)
		}
	} else {
		t.Log("older Database Mail log rows exist; DeleteMailLog not run")
	}

	// -- Drop, then run the scripts back --------------------------------------------------------
	if err := srv.MailProfileRef(liveMailProfile).Drop(ctx); err != nil {
		t.Fatalf("profile Drop: %v", err)
	}
	for _, name := range []string{renamed, liveMailAnon, liveMailWindows} {
		if err := srv.MailAccountRef(name).Drop(ctx); err != nil {
			t.Fatalf("account Drop(%s): %v", name, err)
		}
	}
	if n := intOf("SELECT COUNT(*) FROM sys.credentials"); n != startCreds {
		t.Errorf("%d credentials after dropping the accounts, want %d", n, startCreds)
	}
	if gs := grantsOf(); len(gs) != 0 {
		t.Errorf("grants survived the profile's drop: %v", gs)
	}

	runScript(t, srv, strings.ReplaceAll(allScript, mailPasswordPlaceholder, "other"))
	// Run again: every statement is guarded, so a second run is a no-op.
	runScript(t, srv, strings.ReplaceAll(allScript, mailPasswordPlaceholder, "other"))
	after, err := srv.MailAccounts(ctx)
	if err != nil {
		t.Fatalf("MailAccounts after the script: %v", err)
	}
	if len(after) != len(before.accts) {
		t.Fatalf("the script recreated %d accounts, want %d", len(after), len(before.accts))
	}
	for i, a := range after {
		w := before.accts[i]
		a.AccountID, a.CredentialID, a.LastModified = 0, 0, time.Time{}
		w2 := *w
		w2.AccountID, w2.CredentialID, w2.LastModified = 0, 0, time.Time{}
		if *a != w2 {
			t.Errorf("account recreated as %+v\nwant %+v", *a, w2)
		}
	}
	if got := profileAccounts(liveMailProfile); !slices.Equal(got, before.prof) {
		t.Errorf("profile accounts recreated as %v, want %v", got, before.prof)
	}
	if got := grantsOf(); !slices.Equal(got, before.grants) {
		t.Errorf("grants recreated as %v, want %v", got, before.grants)
	}
	if got, err := srv.MailConfiguration(ctx); err != nil || *got != *before.cfg {
		t.Errorf("configuration recreated as %+v, %v; want %+v", got, err, *before.cfg)
	}

	// The per-object scripts run back the same way, as DROP AND CREATE.
	dc := NewServerScripter(srv, ScriptOptions{Verb: ScriptDropAndCreate, IncludeIfNotExists: true})
	if s, err := dc.ScriptMailProfile(ctx, liveMailProfile); err != nil {
		t.Errorf("ScriptMailProfile(drop and create): %v", err)
	} else {
		runScript(t, srv, s)
	}
	if s, err := dc.ScriptMailAccount(ctx, liveMailAnon); err != nil {
		t.Errorf("ScriptMailAccount(drop and create): %v", err)
	} else {
		runScript(t, srv, s)
	}
	if got := profileAccounts(liveMailProfile); len(got) != 1 || !strings.HasPrefix(got[0], renamed+"@") {
		// The drop-and-create of the anonymous account dropped its link.
		t.Errorf("profile accounts after the per-object scripts = %v; want only %s", got, renamed)
	}
}
