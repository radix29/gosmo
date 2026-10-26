package gosmo

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// ============================================================
// SQL Server Agent -- Alerts
// ============================================================

// NotificationMethod is msdb's bitmask for how an operator is notified
// (sysnotifications.notification_method, and reused by
// sysalerts.include_event_description — see Alert.IncludeEventDescriptionIn).
type NotificationMethod int

const (
	NotifyMethodEmail   NotificationMethod = 1
	NotifyMethodPager   NotificationMethod = 2
	NotifyMethodNetSend NotificationMethod = 4
)

// String renders the method bitmask as e.g. "Email, Pager".
func (m NotificationMethod) String() string {
	var parts []string
	if m&NotifyMethodEmail != 0 {
		parts = append(parts, "Email")
	}
	if m&NotifyMethodPager != 0 {
		parts = append(parts, "Pager")
	}
	if m&NotifyMethodNetSend != 0 {
		parts = append(parts, "Net Send")
	}
	return strings.Join(parts, ", ")
}

// Alert represents a SQL Server Agent alert (msdb.dbo.sysalerts).
type Alert struct {
	server  *Server
	ID      int
	Name    string
	Enabled bool
	// EventSource is "MSSQLSERVER" for a plain SQL Server event alert, or
	// "WMI" for a WMI alert — see IsEventAlert.
	EventSource string
	// ErrorNumber is sysalerts.message_id — mutually exclusive in practice
	// with Severity (SQL Server Agent triggers on whichever is nonzero).
	ErrorNumber           int
	Severity              int
	DatabaseName          string
	DelayBetweenResponses time.Duration
	NotificationMessage   string
	// IncludeEventDescriptionIn is sysalerts.include_event_description —
	// msdb's own bitmask for which notification channel(s) get the event
	// description text appended (0=None, 1=Email, 2=Pager, 4=NetSend,
	// 7=All). Named "...In" after sp_add_alert/sp_update_alert's
	// @include_event_description_in parameter; the column itself has no
	// "_in" suffix, the two names genuinely diverge.
	IncludeEventDescriptionIn int
	EventDescriptionKeyword   string
	Category                  string
	// JobName is the job executed in response to this alert, or "" if none.
	JobName              string
	PerformanceCondition string
	OccurrenceCount      int
	LastOccurrence       time.Time
	LastResponse         time.Time
}

// Server returns the server the alert belongs to.
func (a *Alert) Server() *Server { return a.server }

// IsEventAlert reports whether this is a plain SQL Server event alert (an
// error-number-or-severity trigger) rather than a WMI alert or a
// performance-condition alert — the SQL-only-implementable subset of SQL
// Server Agent alerts (no WMI provider, no performance counter access).
// See Server.EventAlerts. Only EventSource and PerformanceCondition are
// checked — sysalerts.wmi_query/wmi_namespace, WMI's own supplementary
// columns, don't exist on every build (they're absent on SQL Server on
// Linux, where WMI doesn't apply at all), so gosmo doesn't select them;
// event_source = 'WMI' is the reliable, always-present discriminator SQL
// Server itself sets for a WMI alert.
func (a *Alert) IsEventAlert() bool {
	return a.PerformanceCondition == "" && !strings.EqualFold(a.EventSource, "WMI")
}

const alertColumns = `a.id, a.name, a.enabled, ISNULL(a.event_source,''),
       ISNULL(a.message_id,0), ISNULL(a.severity,0),
       ISNULL(a.database_name,''), ISNULL(a.delay_between_responses,0),
       ISNULL(a.notification_message,''), ISNULL(a.include_event_description,0),
       ISNULL(a.event_description_keyword,''), ISNULL(c.name,''),
       ISNULL(j.name,''), ISNULL(a.performance_condition,''),
       ISNULL(a.occurrence_count,0),
       ISNULL(a.last_occurrence_date,0), ISNULL(a.last_occurrence_time,0),
       ISNULL(a.last_response_date,0), ISNULL(a.last_response_time,0)`

const alertFrom = `FROM   msdb.dbo.sysalerts a
LEFT   JOIN msdb.dbo.syscategories c ON c.category_id = a.category_id
LEFT   JOIN msdb.dbo.sysjobs j ON j.job_id = a.job_id`

// scanAlert scans one row shaped like alertColumns into a new Alert.
func scanAlert(s *Server, scan func(dest ...any) error) (*Alert, error) {
	a := &Alert{server: s}
	var delaySec, lastOccDate, lastOccTime, lastRespDate, lastRespTime int
	if err := scan(
		&a.ID, &a.Name, &a.Enabled, &a.EventSource,
		&a.ErrorNumber, &a.Severity,
		&a.DatabaseName, &delaySec,
		&a.NotificationMessage, &a.IncludeEventDescriptionIn,
		&a.EventDescriptionKeyword, &a.Category,
		&a.JobName, &a.PerformanceCondition,
		&a.OccurrenceCount,
		&lastOccDate, &lastOccTime, &lastRespDate, &lastRespTime,
	); err != nil {
		return nil, err
	}
	a.DelayBetweenResponses = time.Duration(delaySec) * time.Second
	a.LastOccurrence = parseSQLAgentDateOrZero(lastOccDate, lastOccTime)
	a.LastResponse = parseSQLAgentDateOrZero(lastRespDate, lastRespTime)
	return a, nil
}

// Alerts returns every SQL Server Agent alert defined on the server.
func (s *Server) Alerts(ctx context.Context) ([]*Alert, error) {
	q := "SELECT " + alertColumns + " " + alertFrom + " ORDER BY a.name"

	rows, err := s.query(ctx, q)
	return scanRows(rows, err, "list alerts", func(scan func(...any) error) (*Alert, error) {
		return scanAlert(s, scan)
	})
}

// EventAlerts returns only plain SQL Server event alerts — the SQL-only
// implementable subset (see Alert.IsEventAlert). WMI alerts and
// performance-condition alerts are excluded, since they depend on non-SQL
// subsystems.
func (s *Server) EventAlerts(ctx context.Context) ([]*Alert, error) {
	all, err := s.Alerts(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*Alert, 0, len(all))
	for _, a := range all {
		if a.IsEventAlert() {
			out = append(out, a)
		}
	}
	return out, nil
}

// AlertRef returns a lightweight handle for an alert by name, without
// querying msdb — the alert-side counterpart of Server.DatabaseRef. ID,
// Severity, JobName and every other cached field stay at their zero value;
// AlertByName is what populates them.
//
// Every write method on *Alert addresses the alert by name (Notify,
// RemoveNotify, Update, ...), so this handle is enough to keep operating on
// an alert the caller already knows exists — and is the form to use when
// there is nothing to read yet: under a WithScript-derived context,
// AlertByName's lookup is a real read and an alert whose sp_add_alert
// was merely collected is not there to find.
func (s *Server) AlertRef(name string) *Alert {
	return &Alert{server: s, Name: name}
}

// AlertByName returns a single alert by name.
func (s *Server) AlertByName(ctx context.Context, name string) (*Alert, error) {
	q := "SELECT " + alertColumns + " " + alertFrom + " WHERE a.name = @p1"

	return readByName(ctx, s, scanAlert, q, []any{name},
		notFoundf("gosmo: alert %q not found", name), fmt.Sprintf("read alert %q", name))
}

// CreateAlertRequest describes a new SQL Server event alert.
type CreateAlertRequest struct {
	Name    string
	Enabled bool
	// ErrorNumber and Severity are mutually exclusive triggers — leave
	// whichever isn't in use at 0.
	ErrorNumber               int
	Severity                  int
	DatabaseName              string
	DelayBetweenResponses     time.Duration
	NotificationMessage       string
	IncludeEventDescriptionIn int
	Category                  string
}

// CreateAlert creates a new SQL Server event alert via sp_add_alert.
func (s *Server) CreateAlert(ctx context.Context, req CreateAlertRequest) (*Alert, error) {
	if req.Name == "" {
		return nil, invalidf("gosmo: create alert: name is required")
	}
	var args boundArgs
	q := fmt.Sprintf(
		"EXEC msdb.dbo.sp_add_alert @name = %s, @message_id = %d, @severity = %d, "+
			"@enabled = %d, @delay_between_responses = %d, @include_event_description_in = %d",
		args.str(req.Name), req.ErrorNumber, req.Severity,
		boolToInt(req.Enabled), int(req.DelayBetweenResponses.Seconds()), req.IncludeEventDescriptionIn,
	)
	if req.DatabaseName != "" {
		q += ", @database_name = " + args.str(req.DatabaseName)
	}
	if req.NotificationMessage != "" {
		q += ", @notification_message = " + args.str(req.NotificationMessage)
	}
	if req.Category != "" {
		q += ", @category_name = " + args.str(req.Category)
	}
	if _, err := s.exec(ctx, q, args.vals...); err != nil {
		return nil, fmt.Errorf("gosmo: create alert %q: %w", req.Name, err)
	}
	return createdObject(ctx, s.AlertRef(req.Name), func() (*Alert, error) {
		return s.AlertByName(ctx, req.Name)
	})
}

// Rename changes the alert's name.
func (a *Alert) Rename(ctx context.Context, newName string) error {
	return a.Alter(ctx, AlertChanges{Name: &newName})
}

// Enable enables the alert.
func (a *Alert) Enable(ctx context.Context) error { return a.SetEnabled(ctx, true) }

// Disable disables the alert.
func (a *Alert) Disable(ctx context.Context) error { return a.SetEnabled(ctx, false) }

// SetEnabled is Enable when on is true and Disable otherwise.
func (a *Alert) SetEnabled(ctx context.Context, on bool) error {
	return a.Alter(ctx, AlertChanges{Enabled: &on})
}

// Drop deletes the alert via sp_delete_alert.
func (a *Alert) Drop(ctx context.Context) error {
	if _, err := a.server.exec(ctx, "EXEC msdb.dbo.sp_delete_alert @name = @p1", a.Name); err != nil {
		return fmt.Errorf("gosmo: drop alert %q: %w", a.Name, err)
	}
	return nil
}

// AlertNotification describes one operator notified by an alert.
type AlertNotification struct {
	OperatorName string
	Method       NotificationMethod
}

// Notify configures the alert to notify an operator via sp_add_notification.
func (a *Alert) Notify(ctx context.Context, operatorName string, method NotificationMethod) error {
	q := fmt.Sprintf("EXEC msdb.dbo.sp_add_notification @alert_name = @p1, @operator_name = @p2, @notification_method = %d",
		int(method))
	if _, err := a.server.exec(ctx, q, a.Name, operatorName); err != nil {
		return fmt.Errorf("gosmo: add notification for alert %q to operator %q: %w", a.Name, operatorName, err)
	}
	return nil
}

// RemoveNotification removes an operator's notification link from the alert
// — the inverse of Notify.
func (a *Alert) RemoveNotification(ctx context.Context, operatorName string) error {
	const q = "EXEC msdb.dbo.sp_delete_notification @alert_name = @p1, @operator_name = @p2"
	if _, err := a.server.exec(ctx, q, a.Name, operatorName); err != nil {
		return fmt.Errorf("gosmo: remove notification for alert %q from operator %q: %w", a.Name, operatorName, err)
	}
	return nil
}

// Notifications returns every operator notified by this alert.
func (a *Alert) Notifications(ctx context.Context) ([]*AlertNotification, error) {
	const q = `
SELECT o.name, n.notification_method
FROM   msdb.dbo.sysnotifications n
JOIN   msdb.dbo.sysoperators o ON o.id = n.operator_id
WHERE  n.alert_id = @p1
ORDER  BY o.name`

	return notificationRows(ctx, a.server, fmt.Sprintf("notifications for alert %q", a.Name), q, a.ID,
		func(name string, code int) *AlertNotification {
			return &AlertNotification{OperatorName: name, Method: NotificationMethod(code)}
		})
}

// notificationRows is the shared body of the three "who notifies whom" reads
// (Alert.Notifications, Operator.NotifyingAlerts, Operator.NotifyingJobs):
// q selects a name and an integer code for one msdb id (@p1), and build turns
// each pair into a row. id 0 — a handle that was never read — is refused with
// requireID. what names the operation, as the read's errors carry it.
func notificationRows[T any](ctx context.Context, s *Server, what, q string, id int, build func(name string, code int) T) ([]T, error) {
	if err := requireID(what, id != 0); err != nil {
		return nil, err
	}
	rows, err := s.query(ctx, q, id)
	return scanRows(rows, err, what, func(scan func(...any) error) (T, error) {
		var name string
		var code int
		if err := scan(&name, &code); err != nil {
			var zero T
			return zero, err
		}
		return build(name, code), nil
	})
}

// parseSQLAgentDateOrZero is parseSQLAgentDate, but treats a 0 date (msdb's
// "never happened" sentinel for columns like sysalerts.last_occurrence_date)
// as the zero Time instead of trying to parse it.
func parseSQLAgentDateOrZero(date, t int) time.Time {
	if date == 0 {
		return time.Time{}
	}
	return parseSQLAgentDate(date, t)
}
