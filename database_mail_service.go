package gosmo

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// database_mail_service.go is the service half of database_mail_write.go:
// the system parameters, start/stop, sending, and the item and log purges.
// That file's header covers both — the permissions, the 'Database Mail
// XPs' option and the procedures' surprises.

// ============================================================
// System parameters
// ============================================================

// MailConfigurationChanges is a change to the system parameters. A nil
// field is left as stored.
type MailConfigurationChanges struct {
	AccountRetryAttempts           *int
	AccountRetryDelay              *int
	DatabaseMailExeMinimumLifeTime *int
	DefaultAttachmentEncoding      *string
	LoggingLevel                   *MailLoggingLevel
	MaxFileSize                    *int
	ProhibitedExtensions           *string
}

// stmts renders one sysmail_configure_sp per set parameter, in the order
// Mail.Configuration lists them.
func (o MailConfigurationChanges) stmts() []string {
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

// SetConfiguration writes every parameter set on o, one
// sysmail_configure_sp each. The procedure validates nothing and updates
// only a parameter msdb already has a row for.
func (dm Mail) SetConfiguration(ctx context.Context, o MailConfigurationChanges) error {
	s := dm.server
	if o.LoggingLevel != nil && (*o.LoggingLevel < MailLoggingNormal || *o.LoggingLevel > MailLoggingVerbose) {
		return invalidf("gosmo: set mail configuration: logging level %d is not 1, 2 or 3", int(*o.LoggingLevel))
	}
	for _, stmt := range o.stmts() {
		if _, err := s.exec(ctx, stmt); err != nil {
			return fmt.Errorf("gosmo: set mail configuration: %w", err)
		}
	}
	return nil
}

// ============================================================
// Start, stop, send
// ============================================================

// Start starts the mail queue (sysmail_start_sp). It does not
// start DatabaseMail.exe — the first queued message does. Refused while
// 'Database Mail XPs' is 0.
func (dm Mail) Start(ctx context.Context) error {
	s := dm.server
	if _, err := s.exec(ctx, "EXEC msdb.dbo.sysmail_start_sp"); err != nil {
		return fmt.Errorf("gosmo: start database mail: %w", err)
	}
	return nil
}

// Stop stops the mail queue (sysmail_stop_sp). Mail is not
// queued meanwhile: sp_send_dbmail refuses it, Msg 14641 (probed on 17).
// Refused while 'Database Mail XPs' is 0.
func (dm Mail) Stop(ctx context.Context) error {
	s := dm.server
	if _, err := s.exec(ctx, "EXEC msdb.dbo.sysmail_stop_sp"); err != nil {
		return fmt.Errorf("gosmo: stop database mail: %w", err)
	}
	return nil
}

// MailMessage is one plain-text message for Mail.Send. An empty Profile
// sends through the caller's default profile, or the public default — what a
// DatabaseMailUserRole member, who cannot list profiles, passes. To is
// sp_send_dbmail's @recipients: one or more addresses separated by ';'.
type MailMessage struct {
	Profile string
	To      string
	Subject string
	Body    string
}

// The refusals Mail.Send reports through errors.Is, each the server error
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

// Send queues m through sp_send_dbmail and returns its mailitem_id, for
// Mail.ItemByID and Mail.Events to follow.
//
// A refusal sp_send_dbmail is known to raise wraps ErrMailProfileInvalid,
// ErrMailNoDefaultProfile, ErrMailStopped or ErrMailXPsDisabled.
//
// It returns once the message is queued; whether it was sent is learned by
// polling. It is never retried (execScan): a connection lost after the
// server queued the message would queue it again. Under Scripting(ctx) the
// statement is recorded and the id is 0.
func (dm Mail) Send(ctx context.Context, m MailMessage) (int, error) {
	s := dm.server
	if strings.TrimSpace(m.To) == "" {
		return 0, invalidf("gosmo: send mail: no recipient")
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

// DeleteItems deletes mail items requested before before and, when
// status is set, in that state — sysmail_delete_mailitems_sp, which needs at
// least one of the two (Msg 14608 otherwise) and logs an information row
// counting what it deleted. A DatabaseMailUserRole member deletes only their
// own items.
func (dm Mail) DeleteItems(ctx context.Context, before time.Time, status MailSentStatus) error {
	s := dm.server
	var a procArgs
	if !before.IsZero() {
		a.raw("@sent_before", mailDateLiteral(before))
	}
	if status != "" {
		a.str("@sent_status", string(status))
	}
	if _, err := s.exec(ctx, a.exec("sysmail_delete_mailitems_sp")); err != nil {
		return fmt.Errorf("gosmo: delete mail items: %w", err)
	}
	return nil
}

// DeleteLog deletes Database Mail log rows logged before before and,
// when eventType is set, of that type — sysmail_delete_log_sp. Both zero
// deletes the whole log. Purge items first and the log last: the item purge
// logs a row of its own.
func (dm Mail) DeleteLog(ctx context.Context, before time.Time, eventType MailEventType) error {
	s := dm.server
	var a procArgs
	if !before.IsZero() {
		a.raw("@logged_before", mailDateLiteral(before))
	}
	if eventType != "" {
		a.str("@event_type", string(eventType))
	}
	if _, err := s.exec(ctx, a.exec("sysmail_delete_log_sp")); err != nil {
		return fmt.Errorf("gosmo: delete mail log: %w", err)
	}
	return nil
}
