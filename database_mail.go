package gosmo

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// database_mail.go covers Database Mail — SSMS's Management > Database Mail.
// All of its state lives in msdb and every write is an msdb.dbo.sysmail_*
// procedure; there is no DDL.
//
// # Permissions
//
// Probed on 13 and 17 (2026-10-01). Nothing here checks sysadmin; access is
// ordinary msdb permission:
//
//   - msdb db_owner or CONTROL SERVER reads everything: accounts, profiles,
//     principal profiles, configuration, status, every item and event (the
//     last two through the base tables — see "Items and events" below).
//   - DatabaseMailUserRole alone reads MailStatus, its own MailItems and the
//     MailEvents of those items only. The account, profile, principal-profile
//     and configuration reads fail Msg 229 for it — and the reads here never
//     join an item or event to a profile, so the role's reads stay readable.
//   - msdb db_datareader reads every item and event, and nothing else here.
//   - Any other login, guest included, gets Msg 229 from every read.
//   - MailQueues needs VIEW SERVER STATE besides (VIEW SERVER PERFORMANCE
//     STATE on 2022 and later), Msg 300 without it.
//
// Errors pass through unchanged, so a caller tells "not visible" from a
// failure by AsSQLError's Number.
//
// # Database Mail XPs
//
// With the 'Database Mail XPs' option at 0 every read here still works
// except the status procedure, which is refused (Msg 15281). MailStatus
// therefore reads the option first and answers MailDisabled rather than an
// error.
//
// # Items and events
//
// The documented views, sysmail_allitems and sysmail_event_log, filter on
// IS_SRVROLEMEMBER('sysadmin'): every other login sees only the items it
// sent and those items' events — not even the log's own start/stop events,
// which name no item. So CONTROL SERVER and msdb db_owner, who configure
// everything, would see almost nothing. MailItems, MailItemByID, MailEvents
// and the ErrorLogDatabaseMail family therefore read the undocumented base
// tables, sysmail_mailitems and sysmail_log, when the caller is not
// sysadmin but may SELECT them (HAS_PERMS_BY_NAME, so a DENY counts), and
// the views otherwise — sysadmin, whom the views already show everything,
// and a DatabaseMailUserRole member, who may not read the tables. The
// choice is made server-side in one batch: a statement SQL Server does not
// run is not permission-checked, so the role member's batch naming the
// table does not fail. The base-table read decodes sent_status and
// event_type as the views do (probed on 17, 2026-10-01). MailVisibility
// says which the caller got.
//
// # Versions
//
// The sysmail_* tables, views and procedures are one implementation from 13
// through 17, Windows and Linux; nothing here is gated.

// ============================================================
// Accounts
// ============================================================

// MailAuthentication is how a Database Mail account authenticates to its
// SMTP server.
type MailAuthentication int

const (
	// MailAuthAnonymous sends no credentials.
	MailAuthAnonymous MailAuthentication = iota
	// MailAuthBasic sends a user name and a password stored as a server
	// credential.
	MailAuthBasic
	// MailAuthWindows sends the Database Mail process's own Windows
	// credentials (use_default_credentials).
	MailAuthWindows
)

func (a MailAuthentication) String() string {
	switch a {
	case MailAuthAnonymous:
		return "Anonymous"
	case MailAuthBasic:
		return "Basic"
	case MailAuthWindows:
		return "Windows"
	}
	return fmt.Sprintf("MailAuthentication(%d)", int(a))
}

// MailAccount is a Database Mail account — msdb.dbo.sysmail_account and its
// SMTP server row in sysmail_server.
//
// The password is never readable: the server keeps it in a server credential
// (CredentialID), so the type has no password field.
type MailAccount struct {
	server *Server

	AccountID      int
	Name           string
	Description    string
	EmailAddress   string
	DisplayName    string
	ReplyToAddress string

	// ServerType is always "SMTP" in practice; sysmail_servertype has no
	// other outgoing type.
	ServerType string
	ServerName string
	Port       int
	EnableSSL  bool

	// UserName is the Basic-auth user, empty otherwise. CredentialID is the
	// server credential holding the password, 0 when there is none.
	UserName              string
	CredentialID          int
	UseDefaultCredentials bool

	// Timeout is the SMTP timeout in seconds, 0 when unset.
	Timeout int

	LastModified time.Time
}

// Server returns the server the account belongs to.
func (a *MailAccount) Server() *Server { return a.server }

// Authentication derives how the account authenticates: Windows when it uses
// the default credentials, Basic when it has a user name or credential,
// anonymous otherwise.
func (a *MailAccount) Authentication() MailAuthentication {
	switch {
	case a.UseDefaultCredentials:
		return MailAuthWindows
	case a.UserName != "" || a.CredentialID != 0:
		return MailAuthBasic
	}
	return MailAuthAnonymous
}

// mailAccountQuery lists accounts with their SMTP server. The LEFT JOIN keeps
// an account whose server row is missing — the procedures never leave one,
// but a hand-edited msdb can.
const mailAccountQuery = `
SELECT a.account_id, a.name, ISNULL(a.description, N''), a.email_address,
       ISNULL(a.display_name, N''), ISNULL(a.replyto_address, N''),
       ISNULL(s.servertype, N''), ISNULL(s.servername, N''), ISNULL(s.port, 0),
       ISNULL(s.enable_ssl, 0), ISNULL(s.username, N''), ISNULL(s.credential_id, 0),
       ISNULL(s.use_default_credentials, 0), ISNULL(s.timeout, 0),
       a.last_mod_datetime
FROM   msdb.dbo.sysmail_account a
LEFT   JOIN msdb.dbo.sysmail_server s ON s.account_id = a.account_id`

func scanMailAccount(s *Server, scan func(...any) error) (*MailAccount, error) {
	a := &MailAccount{server: s}
	err := scan(&a.AccountID, &a.Name, &a.Description, &a.EmailAddress,
		&a.DisplayName, &a.ReplyToAddress,
		&a.ServerType, &a.ServerName, &a.Port,
		&a.EnableSSL, &a.UserName, &a.CredentialID,
		&a.UseDefaultCredentials, &a.Timeout,
		&a.LastModified)
	return a, err
}

// MailAccounts returns every Database Mail account, by name.
func (s *Server) MailAccounts(ctx context.Context) ([]*MailAccount, error) {
	rows, err := s.query(ctx, mailAccountQuery+" ORDER BY a.name")
	return scanRows(rows, err, "list mail accounts", func(scan func(...any) error) (*MailAccount, error) {
		return scanMailAccount(s, scan)
	})
}

// MailAccountByName returns one Database Mail account; a missing one is an
// error wrapping ErrNotFound.
func (s *Server) MailAccountByName(ctx context.Context, name string) (*MailAccount, error) {
	return readByName(ctx, s, scanMailAccount, mailAccountQuery+" WHERE a.name = @p1", []any{name},
		notFoundf("gosmo: mail account %q not found", name),
		fmt.Sprintf("read mail account %q", name))
}

// ============================================================
// Profiles
// ============================================================

// MailProfile represents an msdb Database Mail profile — sysmail_profile,
// with its public grant and its ordered accounts.
type MailProfile struct {
	server *Server

	ProfileID   int
	Name        string
	Description string

	// IsPublic reports a grant to public (principal_sid 0x00); IsDefault
	// reports that grant is the public default profile. Private grants are
	// MailPrincipalProfiles.
	IsPublic  bool
	IsDefault bool

	// Accounts are the profile's accounts in failover order — ascending
	// sequence number, then account name, since the server accepts gaps and
	// duplicates in the sequence.
	Accounts []*MailProfileAccount

	LastModified time.Time
}

// Server returns the server the profile belongs to.
func (p *MailProfile) Server() *Server { return p.server }

// MailProfileAccount is one account of a profile — a sysmail_profileaccount
// row.
type MailProfileAccount struct {
	AccountID      int
	AccountName    string
	SequenceNumber int
}

const mailProfileQuery = `
SELECT p.profile_id, p.name, ISNULL(p.description, N''),
       CAST(CASE WHEN pp.profile_id IS NULL THEN 0 ELSE 1 END AS bit),
       ISNULL(pp.is_default, 0), p.last_mod_datetime
FROM   msdb.dbo.sysmail_profile p
LEFT   JOIN msdb.dbo.sysmail_principalprofile pp
       ON  pp.profile_id = p.profile_id AND pp.principal_sid = 0x00`

const mailProfileAccountQuery = `
SELECT pa.profile_id, pa.account_id, a.name, ISNULL(pa.sequence_number, 0)
FROM   msdb.dbo.sysmail_profileaccount pa
JOIN   msdb.dbo.sysmail_account a ON a.account_id = pa.account_id`

// MailProfiles returns all Database Mail profiles from msdb, by name, each
// with its accounts. Two round trips: the profiles, then every
// profile-account row grouped in Go.
func (s *Server) MailProfiles(ctx context.Context) ([]*MailProfile, error) {
	return s.mailProfiles(ctx, "list mail profiles", "", nil)
}

// MailProfileByName returns one Database Mail profile with its accounts; a
// missing one is an error wrapping ErrNotFound.
func (s *Server) MailProfileByName(ctx context.Context, name string) (*MailProfile, error) {
	ps, err := s.mailProfiles(ctx, fmt.Sprintf("read mail profile %q", name), " WHERE p.name = @p1", []any{name})
	if err != nil {
		return nil, err
	}
	if len(ps) == 0 {
		return nil, notFoundf("gosmo: mail profile %q not found", name)
	}
	return ps[0], nil
}

// mailProfiles reads the profiles matching where, then their accounts — every
// profile's, or with a where, the one profile's. Both reads wrap with what.
func (s *Server) mailProfiles(ctx context.Context, what, where string, args []any) ([]*MailProfile, error) {
	rows, err := s.query(ctx, mailProfileQuery+where+" ORDER BY p.name", args...)
	profiles, err := scanRows(rows, err, what, func(scan func(...any) error) (*MailProfile, error) {
		p := &MailProfile{server: s}
		if err := scan(&p.ProfileID, &p.Name, &p.Description, &p.IsPublic, &p.IsDefault, &p.LastModified); err != nil {
			return nil, err
		}
		return p, nil
	})
	if err != nil || len(profiles) == 0 {
		return profiles, err
	}

	byID := make(map[int]*MailProfile, len(profiles))
	for _, p := range profiles {
		byID[p.ProfileID] = p
	}
	paWhere := ""
	if where != "" {
		paWhere = " WHERE pa.profile_id = @p1"
		args = []any{profiles[0].ProfileID}
	}
	rows, err = s.query(ctx, mailProfileAccountQuery+paWhere+
		" ORDER BY pa.profile_id, ISNULL(pa.sequence_number, 0), a.name", args...)
	type link struct {
		profileID int
		acct      *MailProfileAccount
	}
	links, err := scanRows(rows, err, what, func(scan func(...any) error) (link, error) {
		l := link{acct: &MailProfileAccount{}}
		err := scan(&l.profileID, &l.acct.AccountID, &l.acct.AccountName, &l.acct.SequenceNumber)
		return l, err
	})
	if err != nil {
		return nil, err
	}
	for _, l := range links {
		if p := byID[l.profileID]; p != nil {
			p.Accounts = append(p.Accounts, l.acct)
		}
	}
	return profiles, nil
}

// ============================================================
// Profile security
// ============================================================

// MailPrincipalProfile is one grant of a profile to an msdb database
// principal — a sysmail_principalprofile row.
type MailPrincipalProfile struct {
	ProfileID   int
	ProfileName string

	// PrincipalSID is the msdb principal's SID; 0x00 is public.
	// PrincipalName is "public" for that row and the msdb principal's name
	// otherwise, empty when the SID no longer resolves (a dropped user).
	//
	// public is resolved here, not by sysmail_help_principalprofile_sp or a
	// join to msdb.sys.database_principals: 0x00 is also msdb guest's SID,
	// and both of those name the row "guest".
	PrincipalSID  []byte
	PrincipalName string

	IsDefault bool
}

// IsPublic reports the grant to public.
func (pp *MailPrincipalProfile) IsPublic() bool {
	return len(pp.PrincipalSID) == 1 && pp.PrincipalSID[0] == 0
}

// MailPrincipalProfiles returns every profile grant, public's included, by
// profile then principal name.
func (s *Server) MailPrincipalProfiles(ctx context.Context) ([]*MailPrincipalProfile, error) {
	const q = `
SELECT pp.profile_id, p.name, pp.principal_sid,
       CASE WHEN pp.principal_sid = 0x00 THEN N'public' ELSE ISNULL(dp.name, N'') END,
       pp.is_default
FROM   msdb.dbo.sysmail_principalprofile pp
JOIN   msdb.dbo.sysmail_profile p ON p.profile_id = pp.profile_id
LEFT   JOIN msdb.sys.database_principals dp
       ON  dp.sid = pp.principal_sid AND pp.principal_sid <> 0x00
ORDER  BY p.name, CASE WHEN pp.principal_sid = 0x00 THEN 0 ELSE 1 END, dp.name`

	rows, err := s.query(ctx, q)
	return scanRows(rows, err, "list mail principal profiles", func(scan func(...any) error) (*MailPrincipalProfile, error) {
		pp := &MailPrincipalProfile{}
		err := scan(&pp.ProfileID, &pp.ProfileName, &pp.PrincipalSID, &pp.PrincipalName, &pp.IsDefault)
		return pp, err
	})
}

// ============================================================
// System parameters
// ============================================================

// MailLoggingLevel is the LoggingLevel system parameter.
type MailLoggingLevel int

const (
	MailLoggingNormal   MailLoggingLevel = 1
	MailLoggingExtended MailLoggingLevel = 2 // the server's default
	MailLoggingVerbose  MailLoggingLevel = 3
)

func (l MailLoggingLevel) String() string {
	switch l {
	case MailLoggingNormal:
		return "Normal"
	case MailLoggingExtended:
		return "Extended"
	case MailLoggingVerbose:
		return "Verbose"
	}
	return fmt.Sprintf("MailLoggingLevel(%d)", int(l))
}

// MailConfiguration is Database Mail's system parameters —
// msdb.dbo.sysmail_configuration, one row per parameter. A parameter missing
// from the table reads as its zero value.
type MailConfiguration struct {
	AccountRetryAttempts int
	// AccountRetryDelay is in seconds.
	AccountRetryDelay int
	// DatabaseMailExeMinimumLifeTime is in seconds.
	DatabaseMailExeMinimumLifeTime int
	DefaultAttachmentEncoding      string
	LoggingLevel                   MailLoggingLevel
	// MaxFileSize is the attachment limit in bytes.
	MaxFileSize int
	// ProhibitedExtensions is the comma-separated list as stored.
	ProhibitedExtensions string
}

// MailConfiguration reads Database Mail's system parameters.
func (s *Server) MailConfiguration(ctx context.Context) (*MailConfiguration, error) {
	type param struct{ name, value string }
	rows, err := s.query(ctx, `
SELECT paramname, ISNULL(paramvalue, N'') FROM msdb.dbo.sysmail_configuration`)
	params, err := scanRows(rows, err, "read mail configuration", func(scan func(...any) error) (param, error) {
		var p param
		err := scan(&p.name, &p.value)
		return p, err
	})
	if err != nil {
		return nil, err
	}

	c := &MailConfiguration{}
	for _, p := range params {
		var n *int
		switch p.name {
		case "AccountRetryAttempts":
			n = &c.AccountRetryAttempts
		case "AccountRetryDelay":
			n = &c.AccountRetryDelay
		case "DatabaseMailExeMinimumLifeTime":
			n = &c.DatabaseMailExeMinimumLifeTime
		case "MaxFileSize":
			n = &c.MaxFileSize
		case "LoggingLevel":
			v, err := strconv.Atoi(strings.TrimSpace(p.value))
			if err != nil {
				return nil, fmt.Errorf("gosmo: read mail configuration: %s: %w", p.name, err)
			}
			c.LoggingLevel = MailLoggingLevel(v)
		case "DefaultAttachmentEncoding":
			c.DefaultAttachmentEncoding = p.value
		case "ProhibitedExtensions":
			c.ProhibitedExtensions = p.value
		}
		if n != nil {
			v, err := strconv.Atoi(strings.TrimSpace(p.value))
			if err != nil {
				return nil, fmt.Errorf("gosmo: read mail configuration: %s: %w", p.name, err)
			}
			*n = v
		}
	}
	return c, nil
}

// ============================================================
// Status and queues
// ============================================================

// MailState is whether Database Mail can run.
type MailState int

const (
	// MailDisabled: the 'Database Mail XPs' option is 0 in use, so nothing
	// sends and the status procedure is refused.
	MailDisabled MailState = iota
	// MailStopped: the external mail queue is not receiving
	// (sysmail_stop_sp). sp_send_dbmail refuses mail meanwhile (Msg 14641,
	// "Mail not queued. Database Mail is stopped") — nothing is queued.
	MailStopped
	// MailStarted: the queue is receiving. DatabaseMail.exe itself starts on
	// the first message, not here.
	MailStarted
)

func (st MailState) String() string {
	switch st {
	case MailDisabled:
		return "Disabled"
	case MailStopped:
		return "Stopped"
	case MailStarted:
		return "Started"
	}
	return fmt.Sprintf("MailState(%d)", int(st))
}

// MailStatus reports whether Database Mail is disabled, stopped or started.
// The 'Database Mail XPs' check and sysmail_help_status_sp run in one batch,
// and the procedure only when the option is on, so a disabled server answers
// MailDisabled rather than Msg 15281.
func (s *Server) MailStatus(ctx context.Context) (MailState, error) {
	const q = `
IF EXISTS (SELECT 1 FROM sys.configurations
           WHERE name = N'Database Mail XPs' AND CAST(value_in_use AS int) = 1)
    EXEC msdb.dbo.sysmail_help_status_sp
ELSE
    SELECT 'DISABLED' AS Status`

	var status string
	if err := s.queryRowScan(ctx, q, nil, &status); err != nil {
		return MailDisabled, fmt.Errorf("gosmo: read mail status: %w", err)
	}
	switch strings.ToUpper(strings.TrimSpace(status)) {
	case "DISABLED":
		return MailDisabled, nil
	case "STOPPED":
		return MailStopped, nil
	case "STARTED":
		return MailStarted, nil
	}
	return MailDisabled, fmt.Errorf("gosmo: read mail status: unexpected status %q", status)
}

// MailQueue is one of Database Mail's two Service Broker queues, from
// sysmail_help_queue_sp: "mail" (ExternalMailQueue, messages waiting to be
// sent) and "status" (InternalMailQueue, send results waiting to be logged).
type MailQueue struct {
	Type   string
	Length int
	// State is the queue monitor's state (INACTIVE, NOTIFIED, RECEIVES_OCCURRING
	// …); a queue with no monitor is not listed at all.
	State           string
	LastEmptyRowset time.Time
	LastActivated   time.Time
}

// MailQueues reads the two mail queues' lengths and monitor state. It needs
// VIEW SERVER STATE (the procedure reads sys.dm_broker_queue_monitors) and
// fails Msg 300 without it.
func (s *Server) MailQueues(ctx context.Context) ([]*MailQueue, error) {
	rows, err := s.query(ctx, "EXEC msdb.dbo.sysmail_help_queue_sp")
	return scanRows(rows, err, "read mail queues", func(scan func(...any) error) (*MailQueue, error) {
		q := &MailQueue{}
		var state sql.NullString
		var empty, activated sql.NullTime
		if err := scan(&q.Type, &q.Length, &state, &empty, &activated); err != nil {
			return nil, err
		}
		q.State, q.LastEmptyRowset, q.LastActivated = state.String, empty.Time, activated.Time
		return q, nil
	})
}

// ============================================================
// Mail items and the event log
// ============================================================

// mailRowLimit bounds an item or event read whose filter sets no Max.
const mailRowLimit = 1000

// MailSentStatus is a mail item's sent_status in sysmail_allitems.
type MailSentStatus string

const (
	MailSent     MailSentStatus = "sent"
	MailUnsent   MailSentStatus = "unsent"
	MailRetrying MailSentStatus = "retrying"
	MailFailed   MailSentStatus = "failed"
)

// MailItem is a message Database Mail was asked to send —
// msdb.dbo.sysmail_allitems. Dates are server-local wall clock, carried as
// UTC like every zoneless server time in gosmo.
type MailItem struct {
	MailItemID          int
	ProfileID           int
	Recipients          string
	CopyRecipients      string
	BlindCopyRecipients string
	Subject             string
	Body                string
	BodyFormat          string
	Importance          string
	Sensitivity         string
	FileAttachments     string

	SendRequestDate time.Time
	SendRequestUser string

	// SentAccountID is the account that sent it, 0 until it is sent.
	SentAccountID int
	SentStatus    MailSentStatus
	// SentDate is zero until the item is sent or finally fails.
	SentDate     time.Time
	LastModified time.Time
}

// MailItemFilter narrows MailItems. The zero value reads the newest
// mailRowLimit items of any status.
type MailItemFilter struct {
	// Status keeps only items in this state; empty keeps all.
	Status MailSentStatus
	// Before keeps items requested strictly before this wall-clock time
	// (compared as server-local, like SendRequestDate); zero keeps all.
	Before time.Time
	// Max caps the rows read, newest first; 0 or less means 1000.
	Max int
}

const mailItemColumns = `mailitem_id, profile_id,
       ISNULL(recipients, ''), ISNULL(copy_recipients, ''), ISNULL(blind_copy_recipients, ''),
       ISNULL(subject, N''), ISNULL(body, N''), ISNULL(body_format, ''),
       ISNULL(importance, ''), ISNULL(sensitivity, ''), ISNULL(file_attachments, N''),
       send_request_date, send_request_user, ISNULL(sent_account_id, 0),
       sent_status, sent_date, last_mod_date`

// The base-table sources: a derived table carrying the view's own column
// names and decoding, so one query text reads either. Only the columns the
// reads here select are listed.
const (
	mailItemsView = "msdb.dbo.sysmail_allitems"
	mailItemsBase = `(SELECT mailitem_id, profile_id, recipients, copy_recipients, blind_copy_recipients,
       subject, body, body_format, importance, sensitivity, file_attachments,
       send_request_date, send_request_user, sent_account_id,
       CASE sent_status WHEN 0 THEN 'unsent' WHEN 1 THEN 'sent' WHEN 3 THEN 'retrying' ELSE 'failed' END AS sent_status,
       sent_date, last_mod_date
FROM   msdb.dbo.sysmail_mailitems) AS ai`
	mailEventsView = "msdb.dbo.sysmail_event_log"
	mailEventsBase = `(SELECT log_id,
       CASE event_type WHEN 0 THEN 'success' WHEN 1 THEN 'information' WHEN 2 THEN 'warning' ELSE 'error' END AS event_type,
       log_date, description, process_id, mailitem_id, account_id, last_mod_date, last_mod_user
FROM   msdb.dbo.sysmail_log) AS sl`
)

// mailReadsBase is the predicate under which a read takes the base table
// (msdb.dbo.<table>) over its view — see "Items and events" above.
func mailReadsBase(table string) string {
	return "ISNULL(IS_SRVROLEMEMBER(N'sysadmin'), 0) = 0 AND " +
		"ISNULL(HAS_PERMS_BY_NAME(N'msdb.dbo." + table + "', N'OBJECT', N'SELECT'), 0) = 1"
}

// mailItemsQuery is q, called with the item source, as a batch reading
// sysmail_mailitems when the caller may and sysmail_allitems otherwise. q is a
// builder, not a format string with a %s, so a condition holding a % cannot
// corrupt the query and vet's printf check sees every Sprintf.
func mailItemsQuery(q func(source string) string) string {
	return mailSourceBatch("sysmail_mailitems", q, mailItemsBase, mailItemsView)
}

// mailEventsQuery is mailItemsQuery for the log: sysmail_log or
// sysmail_event_log.
func mailEventsQuery(q func(source string) string) string {
	return mailSourceBatch("sysmail_log", q, mailEventsBase, mailEventsView)
}

func mailSourceBatch(table string, q func(source string) string, base, view string) string {
	return "IF " + mailReadsBase(table) + "\n" + q(base) + "\nELSE\n" + q(view)
}

// MailVisibility is how much of Database Mail's items and log the caller's
// reads return: everything, or only its own items and their events (see
// "Items and events" above).
type MailVisibility struct {
	// AllItems is true when MailItems and MailItemByID see every login's
	// items.
	AllItems bool
	// AllEvents is true when MailEvents and the ErrorLogDatabaseMail family
	// see the whole log.
	AllEvents bool
}

// MailVisibility reports what the caller's item and event reads return — the
// same test those reads branch on, so a caller saying "only your items"
// says what the reads did.
func (s *Server) MailVisibility(ctx context.Context) (MailVisibility, error) {
	var v MailVisibility
	q := "SELECT CAST(CASE WHEN ISNULL(IS_SRVROLEMEMBER(N'sysadmin'), 0) = 1 OR (" + mailReadsBase("sysmail_mailitems") + ") THEN 1 ELSE 0 END AS bit), " +
		"CAST(CASE WHEN ISNULL(IS_SRVROLEMEMBER(N'sysadmin'), 0) = 1 OR (" + mailReadsBase("sysmail_log") + ") THEN 1 ELSE 0 END AS bit)"
	if err := s.queryRowScan(ctx, q, nil, &v.AllItems, &v.AllEvents); err != nil {
		return MailVisibility{}, fmt.Errorf("gosmo: read mail visibility: %w", err)
	}
	return v, nil
}

func scanMailItem(scan func(...any) error) (*MailItem, error) {
	m := &MailItem{}
	var status string
	var sent sql.NullTime
	if err := scan(&m.MailItemID, &m.ProfileID,
		&m.Recipients, &m.CopyRecipients, &m.BlindCopyRecipients,
		&m.Subject, &m.Body, &m.BodyFormat,
		&m.Importance, &m.Sensitivity, &m.FileAttachments,
		&m.SendRequestDate, &m.SendRequestUser, &m.SentAccountID,
		&status, &sent, &m.LastModified); err != nil {
		return nil, err
	}
	m.SentStatus, m.SentDate = MailSentStatus(status), sent.Time
	return m, nil
}

// MailItems reads mail items, newest first. A login that may read neither
// sysmail_mailitems nor every row of sysmail_allitems — a
// DatabaseMailUserRole member — sees only the items it sent (see "Items and
// events" above; MailVisibility).
func (s *Server) MailItems(ctx context.Context, f MailItemFilter) ([]*MailItem, error) {
	var where []string
	var args []any
	if f.Status != "" {
		args = append(args, string(f.Status))
		where = append(where, fmt.Sprintf("sent_status = @p%d", len(args)))
	}
	if !f.Before.IsZero() {
		// The bound is compared as datetime, the column's own type, not
		// datetime2: go-mssqldb reads a datetime rounded to the millisecond
		// (.00666… arrives as .007), so an item's own SendRequestDate passed
		// back as datetime2 is above the stored value in 4 of 10 cases, and
		// "before its own date" kept the item (probed on 17, 2026-10-01).
		// CAST to datetime rounds it back onto the stored tick.
		args = append(args, f.Before)
		where = append(where, fmt.Sprintf("send_request_date < CAST(@p%d AS datetime)", len(args)))
	}
	q := mailItemsQuery(func(source string) string {
		return fmt.Sprintf("SELECT TOP (%d) %s FROM %s%s ORDER BY mailitem_id DESC",
			rowLimit(f.Max), mailItemColumns, source, whereClause(where))
	})

	rows, err := s.query(ctx, q, args...)
	return scanRows(rows, err, "list mail items", func(scan func(...any) error) (*MailItem, error) {
		return scanMailItem(scan)
	})
}

// MailItemByID reads one mail item — what a caller polls after a send to
// learn whether it went. A missing or invisible item is an error wrapping
// ErrNotFound.
func (s *Server) MailItemByID(ctx context.Context, id int) (*MailItem, error) {
	var m *MailItem
	err := s.queryRow(ctx, func(row *sql.Row) error {
		var err error
		m, err = scanMailItem(row.Scan)
		return err
	}, mailItemsQuery(func(source string) string {
		return "SELECT " + mailItemColumns + " FROM " + source + " WHERE mailitem_id = @p1"
	}), id)
	return foundRow(m, err,
		notFoundf("gosmo: mail item %d not found", id),
		fmt.Sprintf("read mail item %d", id))
}

// MailEventType is a Database Mail log row's event_type.
type MailEventType string

const (
	MailEventSuccess     MailEventType = "success"
	MailEventInformation MailEventType = "information"
	MailEventWarning     MailEventType = "warning"
	MailEventError       MailEventType = "error"
)

// MailEvent is a Database Mail log row — msdb.dbo.sysmail_event_log.
type MailEvent struct {
	LogID     int
	EventType MailEventType
	LogDate   time.Time
	// Description can be several KB: a summary sentence, then the .NET
	// exception text, for a send failure.
	Description string
	ProcessID   int
	// MailItemID and AccountID are 0 for an event about neither.
	MailItemID   int
	AccountID    int
	LastModified time.Time
	LastModUser  string
}

// MailEventFilter narrows MailEvents. The zero value reads the newest
// mailRowLimit events.
type MailEventFilter struct {
	// MailItemID keeps the events of one item; 0 keeps all.
	MailItemID int
	// EventType keeps one type; empty keeps all.
	EventType MailEventType
	// Before keeps events logged strictly before this wall-clock time; zero
	// keeps all.
	Before time.Time
	// Max caps the rows read, newest first; 0 or less means 1000.
	Max int
}

// MailEvents reads the Database Mail log, newest first. A
// DatabaseMailUserRole member sees only the events of its own items (see
// "Items and events" above; MailVisibility).
func (s *Server) MailEvents(ctx context.Context, f MailEventFilter) ([]*MailEvent, error) {
	var where []string
	var args []any
	if f.MailItemID != 0 {
		args = append(args, f.MailItemID)
		where = append(where, fmt.Sprintf("mailitem_id = @p%d", len(args)))
	}
	if f.EventType != "" {
		args = append(args, string(f.EventType))
		where = append(where, fmt.Sprintf("event_type = @p%d", len(args)))
	}
	if !f.Before.IsZero() {
		args = append(args, f.Before)
		where = append(where, fmt.Sprintf("log_date < CAST(@p%d AS datetime)", len(args))) // as MailItems' Before
	}
	q := mailEventsQuery(func(source string) string {
		return fmt.Sprintf(`SELECT TOP (%d) log_id, event_type, log_date, ISNULL(description, N''),
       ISNULL(process_id, 0), ISNULL(mailitem_id, 0), ISNULL(account_id, 0),
       last_mod_date, last_mod_user
FROM   %s%s
ORDER  BY log_id DESC`, rowLimit(f.Max), source, whereClause(where))
	})

	rows, err := s.query(ctx, q, args...)
	return scanRows(rows, err, "read mail log", func(scan func(...any) error) (*MailEvent, error) {
		e := &MailEvent{}
		var typ string
		if err := scan(&e.LogID, &typ, &e.LogDate, &e.Description,
			&e.ProcessID, &e.MailItemID, &e.AccountID,
			&e.LastModified, &e.LastModUser); err != nil {
			return nil, err
		}
		e.EventType = MailEventType(typ)
		return e, nil
	})
}

func rowLimit(max int) int {
	if max <= 0 {
		return mailRowLimit
	}
	return max
}

func whereClause(conds []string) string {
	if len(conds) == 0 {
		return ""
	}
	return " WHERE " + strings.Join(conds, " AND ")
}
