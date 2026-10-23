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
	liveMailReader   = "gosmo_live_mail_reader"
	liveMailPassword = "P@ssw0rd_gosmo_live_mail"
)

func TestLiveDatabaseMailReads(t *testing.T) {
	db, ctx, done := liveDBTimeout(t, 4*time.Minute)
	t.Cleanup(done) // registered first, so it runs after every restore below

	srv, err := NewServer(ctx, db)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	if ps, err := srv.Mail().Profiles(ctx); err != nil {
		t.Fatalf("Mail.Profiles: %v", err)
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

	// msdb db_datareader: no sysadmin, so the views would show it nothing,
	// but SELECT on the base tables.
	db.ExecContext(ctx, "IF SUSER_ID(@p1) IS NOT NULL DROP LOGIN ["+liveMailReader+"]", liveMailReader)
	exec("CREATE LOGIN [" + liveMailReader + "] WITH PASSWORD = '" + liveMailPassword + "', CHECK_POLICY = OFF")
	cleanup("DROP LOGIN [" + liveMailReader + "]")
	exec("EXEC msdb.sys.sp_executesql N'CREATE USER [" + liveMailReader + "] FOR LOGIN [" + liveMailReader + "]; " +
		"ALTER ROLE db_datareader ADD MEMBER [" + liveMailReader + "]'")
	cleanup("EXEC msdb.sys.sp_executesql N'DROP USER [" + liveMailReader + "]'")

	// -- Accounts ------------------------------------------------------------------
	accts, err := srv.Mail().Accounts(ctx)
	if err != nil {
		t.Fatalf("Mail.Accounts: %v", err)
	}
	byName := map[string]*MailAccount{}
	for _, a := range accts {
		byName[a.Name] = a
		if a.Server() != srv {
			t.Errorf("MailAccount %s: Server() is not the server it was read from", a.Name)
		}
	}
	if b := byName[liveMailBasic]; b == nil {
		t.Errorf("Mail.Accounts has no %s", liveMailBasic)
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
		t.Errorf("Mail.Accounts has no %s", liveMailAnon)
	} else {
		if a.Port != 1 || a.EnableSSL || a.UserName != "" || a.CredentialID != 0 || a.DisplayName != "" {
			t.Errorf("anonymous account read back as %+v (want port 1, no SSL, no credential)", *a)
		}
		if got := a.Authentication(); got != MailAuthAnonymous {
			t.Errorf("anonymous account Authentication = %v", got)
		}
	}
	if w := byName[liveMailWindows]; w == nil {
		t.Errorf("Mail.Accounts has no %s", liveMailWindows)
	} else if got := w.Authentication(); got != MailAuthWindows {
		t.Errorf("windows account Authentication = %v (%+v)", got, *w)
	}
	if a, err := srv.Mail().AccountByName(ctx, liveMailBasic); err != nil || a.UserName != "gosmo_smtp_user" {
		t.Errorf("Mail.AccountByName = %+v, %v", a, err)
	}
	if _, err := srv.Mail().AccountByName(ctx, "gosmo_live_mail_absent"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Mail.AccountByName(absent) = %v, want ErrNotFound", err)
	}

	// -- Profiles and grants -------------------------------------------------------
	p, err := srv.Mail().ProfileByName(ctx, liveMailProfile)
	if err != nil {
		t.Fatalf("Mail.ProfileByName: %v", err)
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
	ps, err := srv.Mail().Profiles(ctx)
	if err != nil || len(ps) != 1 || len(ps[0].Accounts) != 2 {
		t.Errorf("Mail.Profiles = %d profiles, %v; want the one fixture with two accounts", len(ps), err)
	}
	if _, err := srv.Mail().ProfileByName(ctx, "gosmo_live_mail_absent"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Mail.ProfileByName(absent) = %v, want ErrNotFound", err)
	}

	grants, err := srv.Mail().PrincipalProfiles(ctx)
	if err != nil {
		t.Fatalf("Mail.PrincipalProfiles: %v", err)
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
		t.Errorf("Mail.PrincipalProfiles = %v, want %v (public named public, not guest)", got, want)
	}

	// -- Configuration ---------------------------------------------------------------
	cfg, err := srv.Mail().Configuration(ctx)
	if err != nil {
		t.Fatalf("Mail.Configuration: %v", err)
	}
	if cfg.AccountRetryDelay <= 0 || cfg.DatabaseMailExeMinimumLifeTime <= 0 || cfg.MaxFileSize <= 0 ||
		cfg.LoggingLevel < MailLoggingNormal || cfg.LoggingLevel > MailLoggingVerbose ||
		cfg.DefaultAttachmentEncoding == "" || !strings.Contains(cfg.ProhibitedExtensions, "exe") {
		t.Errorf("Mail.Configuration = %+v", *cfg)
	}

	// -- Status, with the option off and on ------------------------------------------
	if xps == 0 {
		if st, err := srv.Mail().Status(ctx); err != nil || st != MailDisabled {
			t.Errorf("Mail.Status with Database Mail XPs 0 = %v, %v; want Disabled and no error", st, err)
		}
		exec(`EXEC sp_configure 'show advanced options', 1; RECONFIGURE;
EXEC sp_configure 'Database Mail XPs', 1; RECONFIGURE;`)
	}
	st, err := srv.Mail().Status(ctx)
	if err != nil || st == MailDisabled {
		t.Fatalf("Mail.Status with Database Mail XPs 1 = %v, %v", st, err)
	}
	if st == MailStopped {
		exec("EXEC msdb.dbo.sysmail_start_sp")
		cleanup("EXEC msdb.dbo.sysmail_stop_sp")
		if st, err := srv.Mail().Status(ctx); err != nil || st != MailStarted {
			t.Fatalf("Mail.Status after sysmail_start_sp = %v, %v", st, err)
		}
	}
	queues, err := srv.Mail().Queues(ctx)
	if err != nil {
		t.Fatalf("Mail.Queues: %v", err)
	}
	var qtypes []string
	for _, q := range queues {
		qtypes = append(qtypes, q.Type)
	}
	if slices.Sort(qtypes); !slices.Equal(qtypes, []string{"mail", "status"}) {
		t.Errorf("Mail.Queues types = %v, want mail and status", qtypes)
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
		item, err = srv.Mail().ItemByID(ctx, id)
		if err != nil {
			t.Fatalf("Mail.ItemByID(%d): %v", id, err)
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

	failed, err := srv.Mail().Items(ctx, MailItemFilter{Status: MailFailed, Max: 5})
	if err != nil || len(failed) == 0 || failed[0].MailItemID != id {
		t.Errorf("Mail.Items(failed) = %d items, %v; want item %d first", len(failed), err, id)
	}
	if none, err := srv.Mail().Items(ctx, MailItemFilter{Status: MailSent}); err != nil || slices.ContainsFunc(none, func(m *MailItem) bool { return m.MailItemID == id }) {
		t.Errorf("Mail.Items(sent) = %v; the failed item is not sent", err)
	}
	if before, err := srv.Mail().Items(ctx, MailItemFilter{Before: item.SendRequestDate}); err != nil ||
		slices.ContainsFunc(before, func(m *MailItem) bool { return m.MailItemID == id }) {
		t.Errorf("Mail.Items(Before = its own request date) = %v; want the item left out", err)
	}
	if after, err := srv.Mail().Items(ctx, MailItemFilter{Before: item.SendRequestDate.Add(time.Second)}); err != nil ||
		!slices.ContainsFunc(after, func(m *MailItem) bool { return m.MailItemID == id }) {
		t.Errorf("Mail.Items(Before = a second later) = %v; want the item kept", err)
	}

	// DatabaseMail.exe marks the item failed and logs the error event as two
	// writes, and not always in that order: one run on 17 read the item failed
	// with no event logged yet. So the event gets its own short wait.
	var events []*MailEvent
	var sawError bool
	for deadline := time.Now().Add(30 * time.Second); ; {
		events, err = srv.Mail().Events(ctx, MailEventFilter{MailItemID: id})
		if err != nil {
			t.Fatalf("Mail.Events: %v", err)
		}
		for _, e := range events {
			if e.MailItemID != id {
				t.Errorf("Mail.Events(item %d) returned event %d of item %d", id, e.LogID, e.MailItemID)
			}
			if !sawError && e.EventType == MailEventError && e.Description != "" && e.LogDate.After(time.Time{}) {
				sawError = true
				t.Logf("error event: %.200s", e.Description)
			}
		}
		if sawError || time.Now().After(deadline) {
			break
		}
		time.Sleep(time.Second)
	}
	if !sawError {
		t.Errorf("Mail.Events(item %d) = %d events, none an error with a description", id, len(events))
	}
	if errs, err := srv.Mail().Events(ctx, MailEventFilter{EventType: MailEventError, Max: 1}); err != nil || len(errs) != 1 || errs[0].EventType != MailEventError {
		t.Errorf("Mail.Events(error, Max 1) = %d, %v", len(errs), err)
	}

	if v, err := srv.Mail().Visibility(ctx); err != nil || !v.AllItems || !v.AllEvents {
		t.Errorf("sysadmin Mail.Visibility = %+v, %v; want everything", v, err)
	}

	// login opens a pool as one of the fixture logins. Not NewServer:
	// loadInfo's DMV half needs VIEW SERVER STATE.
	login := func(name string) *Server {
		t.Helper()
		pool, err := sql.Open("sqlserver", liveRestrictedDSN(t, name, liveMailPassword))
		if err != nil {
			t.Fatalf("open as %s: %v", name, err)
		}
		t.Cleanup(func() { pool.Close() })
		return &Server{db: pool}
	}

	// -- msdb db_datareader: the base tables ----------------------------------------
	// The item is sa's, and the log holds events naming no item (Database
	// Mail starting): the views would show this login neither.
	reader := login(liveMailReader)
	if v, err := reader.Mail().Visibility(ctx); err != nil || !v.AllItems || !v.AllEvents {
		t.Errorf("db_datareader Mail.Visibility = %+v, %v; want everything", v, err)
	}
	if v, err := reader.Mail().Items(ctx, MailItemFilter{Status: MailFailed}); err != nil ||
		!slices.ContainsFunc(v, func(m *MailItem) bool { return m.MailItemID == id }) {
		t.Errorf("db_datareader Mail.Items(failed) = %d, %v; want sa's item %d", len(v), err, id)
	}
	// Compared with a sysadmin read taken just after, not with item: Database
	// Mail stamps sent_date a moment after it marks the item failed (seen on
	// 13), so the two reads are retried until the item has settled.
	for try := 0; ; try++ {
		m, err := reader.Mail().ItemByID(ctx, id)
		want, werr := srv.Mail().ItemByID(ctx, id)
		if err == nil && werr == nil && *m == *want {
			break
		}
		if try == 5 {
			t.Errorf("db_datareader Mail.ItemByID(%d) = %+v, %v; want what sysadmin read, %+v, %v", id, m, err, want, werr)
			break
		}
		time.Sleep(time.Second)
	}
	allEvents, err := srv.Mail().Events(ctx, MailEventFilter{Max: 50})
	if err != nil {
		t.Fatalf("Mail.Events: %v", err)
	}
	if v, err := reader.Mail().Events(ctx, MailEventFilter{Max: 50}); err != nil || len(v) != len(allEvents) {
		t.Errorf("db_datareader Mail.Events = %d, %v; want sysadmin's %d", len(v), err, len(allEvents))
	} else {
		for i := range v {
			if *v[i] != *allEvents[i] {
				t.Errorf("db_datareader event %d = %+v; sysadmin read %+v", i, *v[i], *allEvents[i])
			}
		}
		// Present only when DatabaseMail.exe started during the run, not when
		// a previous run left it up.
		noItem := func(e *MailEvent) bool { return e.MailItemID == 0 }
		if slices.ContainsFunc(allEvents, noItem) && !slices.ContainsFunc(v, noItem) {
			t.Errorf("db_datareader Mail.Events has no event naming no item")
		}
	}
	if v, err := reader.ReadLog(ctx, ErrorLogDatabaseMail, 0, LogSearch{}); err != nil || len(v) == 0 {
		t.Errorf("db_datareader ReadLog(Database Mail) = %d, %v; want the whole log", len(v), err)
	}
	// A DENY on the table sends the read back to the view: HAS_PERMS_BY_NAME
	// sees the DENY, so the base-table branch is never tried and refused.
	exec("EXEC msdb.sys.sp_executesql N'DENY SELECT ON dbo.sysmail_mailitems TO [" + liveMailReader + "]'")
	if v, err := reader.Mail().Visibility(ctx); err != nil || v.AllItems || !v.AllEvents {
		t.Errorf("db_datareader denied the items table: Mail.Visibility = %+v, %v; want items own-only, events all", v, err)
	}
	if v, err := reader.Mail().Items(ctx, MailItemFilter{}); err != nil || len(v) != 0 {
		t.Errorf("db_datareader denied the items table: Mail.Items = %d, %v; want none and no error", len(v), err)
	}
	exec("EXEC msdb.sys.sp_executesql N'REVOKE SELECT ON dbo.sysmail_mailitems FROM [" + liveMailReader + "]'")

	// -- DatabaseMailUserRole only ---------------------------------------------------
	restricted := login(liveMailLogin)
	if v, err := restricted.Mail().Visibility(ctx); err != nil || v.AllItems || v.AllEvents {
		t.Errorf("role member Mail.Visibility = %+v, %v; want own-only", v, err)
	}
	if st, err := restricted.Mail().Status(ctx); err != nil || st != MailStarted {
		t.Errorf("role member Mail.Status = %v, %v; the role is granted the status proc", st, err)
	}
	if v, err := restricted.Mail().Items(ctx, MailItemFilter{}); err != nil || len(v) != 0 {
		t.Errorf("role member Mail.Items = %d, %v; want none of someone else's items and no error", len(v), err)
	}
	if v, err := restricted.Mail().Events(ctx, MailEventFilter{}); err != nil || len(v) != 0 {
		t.Errorf("role member Mail.Events = %d, %v; want none and no error", len(v), err)
	}
	for name, read := range map[string]func() error{
		"Mail.Accounts":          func() error { _, err := restricted.Mail().Accounts(ctx); return err },
		"Mail.Profiles":          func() error { _, err := restricted.Mail().Profiles(ctx); return err },
		"Mail.PrincipalProfiles": func() error { _, err := restricted.Mail().PrincipalProfiles(ctx); return err },
		"MailConfiguration":      func() error { _, err := restricted.Mail().Configuration(ctx); return err },
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
	if ps, err := srv.Mail().Profiles(ctx); err != nil {
		t.Fatalf("Mail.Profiles: %v", err)
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
		a, err := srv.Mail().AccountByName(ctx, name)
		if err != nil {
			t.Fatalf("Mail.AccountByName(%s): %v", name, err)
		}
		return a
	}
	profileAccounts := func(name string) []string {
		t.Helper()
		p, err := srv.Mail().ProfileByName(ctx, name)
		if err != nil {
			t.Fatalf("Mail.ProfileByName(%s): %v", name, err)
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
	cfg, err := srv.Mail().Configuration(ctx)
	if err != nil {
		t.Fatalf("Mail.Configuration: %v", err)
	}
	t.Cleanup(func() {
		if err := srv.Mail().SetConfiguration(context.Background(), cfg.options()); err != nil {
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
	b, err := srv.Mail().CreateAccount(ctx, basicReq)
	if err != nil {
		t.Fatalf("Mail.CreateAccount(basic): %v", err)
	}
	if b.AccountID == 0 || b.UserName != "gosmo_smtp_user" || b.CredentialID == 0 || b.Timeout != 30 ||
		!b.EnableSSL || b.Port != 1 || b.DisplayName != "Basic" || b.Authentication() != MailAuthBasic {
		t.Errorf("basic account created as %+v", *b)
	}
	if intOf("SELECT COUNT(*) FROM sys.credentials") != startCreds+1 {
		t.Errorf("a Basic account did not store one credential")
	}
	if _, err := srv.Mail().CreateAccount(ctx, CreateMailAccountRequest{Name: liveMailAnon,
		EmailAddress: "anon@example.com", ServerName: "127.0.0.1", Port: 1}); err != nil {
		t.Fatalf("Mail.CreateAccount(anon): %v", err)
	}
	if w, err := srv.Mail().CreateAccount(ctx, CreateMailAccountRequest{Name: liveMailWindows,
		EmailAddress: "win@example.com", ServerName: "127.0.0.1", Port: 1,
		Credentials: MailCredentials{Authentication: MailAuthWindows}}); err != nil || w.Authentication() != MailAuthWindows {
		t.Fatalf("Mail.CreateAccount(windows) = %+v, %v", w, err)
	}

	// Alter without Credentials keeps the user name and credential — the
	// trap: an omitted @username alone drops them.
	credID := b.CredentialID
	if err := srv.Mail().AccountRef(liveMailBasic).Alter(ctx, MailAccountChanges{Port: new(2), DisplayName: new("")}); err != nil {
		t.Fatalf("Alter(port, display name): %v", err)
	}
	if got := account(liveMailBasic); got.Port != 2 || got.DisplayName != "" || got.UserName != "gosmo_smtp_user" ||
		got.CredentialID != credID || got.Timeout != 30 || !got.EnableSSL || got.EmailAddress != "basic@example.com" {
		t.Errorf("after Alter(port, display name) the account reads %+v; want the credential and the rest kept", *got)
	}
	// Rename through a name-only handle, then new credentials on it.
	if err := srv.Mail().AccountRef(liveMailBasic).Alter(ctx, MailAccountChanges{Name: new(renamed),
		Credentials: &MailCredentials{Authentication: MailAuthBasic, UserName: "gosmo_smtp_user2", Password: "other"}}); err != nil {
		t.Fatalf("Alter(rename, credentials): %v", err)
	}
	if got := account(renamed); got.UserName != "gosmo_smtp_user2" || got.CredentialID != credID || got.Port != 2 {
		t.Errorf("after rename + new credentials the account reads %+v", *got)
	}
	if _, err := srv.Mail().AccountByName(ctx, liveMailBasic); !errors.Is(err, ErrNotFound) {
		t.Errorf("the old name still resolves: %v", err)
	}
	if err := srv.Mail().AccountRef("gosmo_live_mail_absent").Alter(ctx, MailAccountChanges{Name: new("x")}); err == nil {
		t.Error("renaming a missing account returned no error")
	}

	// -- Profiles ---------------------------------------------------------------------------
	p, err := srv.Mail().CreateProfile(ctx, CreateMailProfileRequest{Name: liveMailProfile, Description: "gosmo live profile"})
	if err != nil || p.ProfileID == 0 || p.Description != "gosmo live profile" {
		t.Fatalf("Mail.CreateProfile = %+v, %v", p, err)
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
	if fresh, err := srv.Mail().ProfileByName(ctx, liveMailProfile); err != nil {
		t.Errorf("Mail.ProfileByName: %v", err)
	} else if len(p.Accounts) != len(fresh.Accounts) {
		t.Errorf("the handle's accounts = %d, want the %d read back", len(p.Accounts), len(fresh.Accounts))
	} else {
		for i, a := range p.Accounts {
			if *a != *fresh.Accounts[i] {
				t.Errorf("the handle's account %d = %+v, want %+v", i, *a, *fresh.Accounts[i])
			}
		}
	}
	// All or nothing: a missing account fails the batch and leaves the list.
	if err := p.SetAccounts(ctx, []string{liveMailAnon, "gosmo_live_mail_absent"}); err == nil {
		t.Error("SetAccounts with a missing account returned no error")
	}
	if got, want := profileAccounts(liveMailProfile), []string{renamed + "@1", liveMailWindows + "@2"}; !slices.Equal(got, want) {
		t.Errorf("a failed SetAccounts changed the list to %v, want %v", got, want)
	}
	// sysmail matches names under the server's collation: on a case-insensitive
	// one a differently cased spelling is the existing link (nothing to send),
	// and two spellings of one account are refused before the server is asked.
	if CollationIgnoresCase(srv.Info().Collation) {
		sctx, col := WithScript(ctx)
		if err := p.SetAccounts(sctx, []string{strings.ToUpper(renamed), strings.ToUpper(liveMailWindows)}); err != nil {
			t.Errorf("SetAccounts (recased): %v", err)
		} else if stmts := col.Statements(); len(stmts) != 0 {
			t.Errorf("SetAccounts with the current list recased would send %q, want nothing", stmts)
		}
		err := p.SetAccounts(ctx, []string{renamed, strings.ToUpper(renamed)})
		if err == nil || !strings.Contains(err.Error(), "is listed twice") {
			t.Errorf("SetAccounts with one account in two casings: err %v, want the listed-twice refusal", err)
		}
		if got, want := profileAccounts(liveMailProfile), []string{renamed + "@1", liveMailWindows + "@2"}; !slices.Equal(got, want) {
			t.Errorf("the recased SetAccounts calls changed the list to %v, want %v", got, want)
		}
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

	if err := p.Grant(ctx, MailPublicPrincipal, MailDefaultProfile); err != nil || !p.IsPublic || !p.IsDefault {
		t.Fatalf("Grant(public, default) = %v; mirrored IsPublic %v IsDefault %v", err, p.IsPublic, p.IsDefault)
	}
	if err := p.Grant(ctx, liveMailLogin, MailNotDefaultProfile); err != nil {
		t.Fatalf("Grant(user): %v", err)
	}
	if err := p.SetGrantDefault(ctx, liveMailLogin, MailDefaultProfile); err != nil {
		t.Fatalf("SetGrantDefault: %v", err)
	}
	grantsOf := func() []string {
		t.Helper()
		gs, err := srv.Mail().PrincipalProfiles(ctx)
		if err != nil {
			t.Fatalf("Mail.PrincipalProfiles: %v", err)
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
	if err := p.Grant(ctx, liveMailLogin+"_absent", MailNotDefaultProfile); err == nil {
		t.Error("Grant to a principal with no msdb user returned no error")
	}

	if err := p.Alter(ctx, MailProfileChanges{Name: new(renamedProfile), Description: new("")}); err != nil {
		t.Fatalf("profile Alter(rename): %v", err)
	}
	if got, err := srv.Mail().ProfileByName(ctx, renamedProfile); err != nil || got.Description != "" || !got.IsDefault || len(got.Accounts) != 2 {
		t.Errorf("renamed profile reads %+v, %v; want its links and grants kept", got, err)
	}
	if err := p.Alter(ctx, MailProfileChanges{Name: new(liveMailProfile), Description: new("gosmo live profile")}); err != nil {
		t.Fatalf("profile Alter(rename back): %v", err)
	}

	// -- System parameters -----------------------------------------------------------------
	if err := srv.Mail().SetConfiguration(ctx, MailConfigurationChanges{
		AccountRetryAttempts: new(0), LoggingLevel: new(MailLoggingVerbose)}); err != nil {
		t.Fatalf("Mail.SetConfiguration: %v", err)
	}
	if got, err := srv.Mail().Configuration(ctx); err != nil || got.AccountRetryAttempts != 0 ||
		got.LoggingLevel != MailLoggingVerbose || got.MaxFileSize != cfg.MaxFileSize {
		t.Errorf("configuration after Mail.SetConfiguration = %+v, %v", got, err)
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
	if strings.Contains(acctScript, "other") || !strings.Contains(acctScript, "@password = N'<insert password here>'") {
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
	if before.accts, err = srv.Mail().Accounts(ctx); err != nil {
		t.Fatalf("Mail.Accounts: %v", err)
	}
	before.prof, before.grants = profileAccounts(liveMailProfile), grantsOf()
	if before.cfg, err = srv.Mail().Configuration(ctx); err != nil {
		t.Fatalf("Mail.Configuration: %v", err)
	}

	// -- Start and a send that fails at once -------------------------------------------------
	if xps == 0 {
		if err := srv.Mail().Start(ctx); err == nil {
			t.Error("Mail.Start with Database Mail XPs 0 returned no error")
		}
		exec(`EXEC sp_configure 'show advanced options', 1; RECONFIGURE;
EXEC sp_configure 'Database Mail XPs', 1; RECONFIGURE;`)
	}
	if st, err := srv.Mail().Status(ctx); err != nil {
		t.Fatalf("Mail.Status: %v", err)
	} else if st == MailStopped {
		cleanup("EXEC msdb.dbo.sysmail_stop_sp")
	}
	if err := srv.Mail().Start(ctx); err != nil {
		t.Fatalf("Mail.Start: %v", err)
	}
	if st, err := srv.Mail().Status(ctx); err != nil || st != MailStarted {
		t.Fatalf("Mail.Status after Mail.Start = %v, %v", st, err)
	}
	if err := srv.Mail().Stop(ctx); err != nil {
		t.Fatalf("Mail.Stop: %v", err)
	}
	if st, err := srv.Mail().Status(ctx); err != nil || st != MailStopped {
		t.Fatalf("Mail.Status after Mail.Stop = %v, %v", st, err)
	}
	if err := srv.Mail().Start(ctx); err != nil {
		t.Fatalf("Mail.Start: %v", err)
	}

	// No profile: the caller's default, here public's.
	id, err := srv.Mail().Send(ctx, MailMessage{To: "nobody@example.com", Subject: "gosmo live write", Body: "gosmo live write body"})
	if err != nil || id == 0 {
		t.Fatalf("Mail.Send = %d, %v", id, err)
	}
	var item *MailItem
	for deadline := time.Now().Add(2 * time.Minute); ; {
		if item, err = srv.Mail().ItemByID(ctx, id); err != nil {
			t.Fatalf("Mail.ItemByID(%d): %v", id, err)
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
	entries, err := srv.ReadLog(ctx, ErrorLogDatabaseMail, 0, LogSearch{
		Text1: "COULD NOT CONNECT", From: item.SendRequestDate.Add(-time.Second)})
	if err != nil {
		t.Fatalf("ReadLog(Database Mail): %v", err)
	}
	if len(entries) == 0 || entries[0].Process != "error" || entries[0].Source() != "error" ||
		!strings.Contains(strings.ToLower(entries[0].Text), "could not connect") || entries[0].Date.IsZero() {
		t.Errorf("Database Mail log search = %d entries (first %+v); want the send's error", len(entries), entries)
	}
	// An entry's own Date bounds it inclusively both ways — a datetime read
	// back rounded to the millisecond must not fall outside its own range.
	for _, e := range entries {
		if got, err := srv.ReadLog(ctx, ErrorLogDatabaseMail, 0, LogSearch{From: e.Date, To: e.Date}); err != nil ||
			!slices.ContainsFunc(got, func(g *ErrorLogEntry) bool { return g.Text == e.Text }) {
			t.Errorf("ReadLog(From = To = %s) = %d entries, %v; want the entry itself", e.LogDate, len(got), err)
		}
	}
	if all, err := srv.ReadLog(ctx, ErrorLogDatabaseMail, 0, LogSearch{}); err != nil || len(all) < len(entries) ||
		(len(all) > 1 && all[0].Date.After(all[len(all)-1].Date)) {
		t.Errorf("ReadLog(Database Mail) = %d entries, %v; want at least the search's, oldest first", len(all), err)
	}
	if _, err := srv.ReadLog(ctx, ErrorLogDatabaseMail, 1, LogSearch{}); err == nil {
		t.Error("ReadLog(Database Mail, 1) returned no error")
	}

	// -- Purges -------------------------------------------------------------------------------
	// sysmail_delete_mailitems_sp and sysmail_delete_log_sp take no lower
	// bound, so they run only when nothing older than the test is in reach.
	if intOf("SELECT COUNT(*) FROM msdb.dbo.sysmail_mailitems WHERE mailitem_id <= @p1 AND sent_status = 2", startItem) == 0 {
		if err := srv.Mail().DeleteItems(ctx, time.Time{}, MailFailed); err != nil {
			t.Errorf("Mail.DeleteItems: %v", err)
		}
		if _, err := srv.Mail().ItemByID(ctx, id); !errors.Is(err, ErrNotFound) {
			t.Errorf("item %d after Mail.DeleteItems: %v; want not found", id, err)
		}
	} else {
		t.Log("older failed mail items exist; Mail.DeleteItems not run")
	}
	if err := srv.Mail().DeleteItems(ctx, time.Time{}, ""); err == nil {
		t.Error("Mail.DeleteItems with neither bound returned no error")
	}
	// Earlier runs' DatabaseMail.exe start/exit rows are fair game; anything
	// else is not this test's to delete.
	if intOf(`SELECT COUNT(*) FROM msdb.dbo.sysmail_log WHERE log_id <= @p1
AND description NOT LIKE N'DatabaseMail process is %'`, startLog) == 0 {
		if err := srv.Mail().DeleteLog(ctx, time.Time{}, MailEventError); err != nil {
			t.Errorf("Mail.DeleteLog(error): %v", err)
		}
		if n := intOf("SELECT COUNT(*) FROM msdb.dbo.sysmail_log WHERE event_type = 3"); n != 0 {
			t.Errorf("%d error rows after Mail.DeleteLog(error)", n)
		}
	} else {
		t.Log("older Database Mail log rows exist; Mail.DeleteLog not run")
	}

	// -- Drop, then run the scripts back --------------------------------------------------------
	if err := srv.Mail().ProfileRef(liveMailProfile).Drop(ctx); err != nil {
		t.Fatalf("profile Drop: %v", err)
	}
	for _, name := range []string{renamed, liveMailAnon, liveMailWindows} {
		if err := srv.Mail().AccountRef(name).Drop(ctx); err != nil {
			t.Fatalf("account Drop(%s): %v", name, err)
		}
	}
	if n := intOf("SELECT COUNT(*) FROM sys.credentials"); n != startCreds {
		t.Errorf("%d credentials after dropping the accounts, want %d", n, startCreds)
	}
	if gs := grantsOf(); len(gs) != 0 {
		t.Errorf("grants survived the profile's drop: %v", gs)
	}

	runScript(t, srv, strings.ReplaceAll(allScript, PasswordPlaceholder, "other"))
	// Run again: every statement is guarded, so a second run is a no-op.
	runScript(t, srv, strings.ReplaceAll(allScript, PasswordPlaceholder, "other"))
	after, err := srv.Mail().Accounts(ctx)
	if err != nil {
		t.Fatalf("Mail.Accounts after the script: %v", err)
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
	if got, err := srv.Mail().Configuration(ctx); err != nil || *got != *before.cfg {
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
