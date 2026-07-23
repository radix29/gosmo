package gosmo

import (
	"context"
	"database/sql"
	"errors"
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
	// 7=All). Named "...In" (matching sp_add_alert/sp_update_alert's own
	// @include_event_description_in parameter, not the column, which has
	// no "_in" suffix — confirmed live against SQL Server 2025: the column
	// and the stored-procedure parameter names genuinely diverge here).
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

// IsEventAlert reports whether this is a plain SQL Server event alert (an
// error-number-or-severity trigger) rather than a WMI alert or a
// performance-condition alert — the SQL-only-implementable subset of SQL
// Server Agent alerts (no WMI provider, no performance counter access).
// See Server.EventAlerts. Only EventSource and PerformanceCondition are
// checked — sysalerts.wmi_query/wmi_namespace, WMI's own supplementary
// columns, don't exist on every build (confirmed absent against a live
// SQL Server 2025 on Linux instance, where WMI doesn't apply at all), so
// gosmo doesn't select them; event_source = 'WMI' is the reliable, always-
// present discriminator SQL Server itself sets for a WMI alert.
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
func (s *Server) Alerts() ([]*Alert, error) { return s.AlertsContext(context.Background()) }

// AlertsContext is the context-aware variant of Alerts.
func (s *Server) AlertsContext(ctx context.Context) ([]*Alert, error) {
	q := "SELECT " + alertColumns + " " + alertFrom + " ORDER BY a.name"

	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("gosmo: list alerts: %w", err)
	}
	defer rows.Close()

	var out []*Alert
	for rows.Next() {
		a, err := scanAlert(s, rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// EventAlerts returns only plain SQL Server event alerts — the SQL-only
// implementable subset (see Alert.IsEventAlert). WMI alerts and
// performance-condition alerts are excluded, since they depend on non-SQL
// subsystems.
func (s *Server) EventAlerts() ([]*Alert, error) { return s.EventAlertsContext(context.Background()) }

// EventAlertsContext is the context-aware variant of EventAlerts.
func (s *Server) EventAlertsContext(ctx context.Context) ([]*Alert, error) {
	all, err := s.AlertsContext(ctx)
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

// AlertByName returns a single alert by name.
func (s *Server) AlertByName(name string) (*Alert, error) {
	return s.AlertByNameContext(context.Background(), name)
}

// AlertByNameContext is the context-aware variant of AlertByName.
func (s *Server) AlertByNameContext(ctx context.Context, name string) (*Alert, error) {
	q := "SELECT " + alertColumns + " " + alertFrom + " WHERE a.name = @p1"

	row := s.db.QueryRowContext(ctx, q, name)
	a, err := scanAlert(s, row.Scan)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("gosmo: alert %q not found", name)
		}
		return nil, fmt.Errorf("gosmo: alert by name: %w", err)
	}
	return a, nil
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
func (s *Server) CreateAlert(req CreateAlertRequest) (*Alert, error) {
	return s.CreateAlertContext(context.Background(), req)
}

// CreateAlertContext is the context-aware variant of CreateAlert.
func (s *Server) CreateAlertContext(ctx context.Context, req CreateAlertRequest) (*Alert, error) {
	if req.Name == "" {
		return nil, fmt.Errorf("gosmo: create alert: name is required")
	}
	q := fmt.Sprintf(
		"EXEC msdb.dbo.sp_add_alert @name = N'%s', @message_id = %d, @severity = %d, "+
			"@enabled = %d, @delay_between_responses = %d, @include_event_description_in = %d",
		escapeSingle(req.Name), req.ErrorNumber, req.Severity,
		boolToInt(req.Enabled), int(req.DelayBetweenResponses.Seconds()), req.IncludeEventDescriptionIn,
	)
	if req.DatabaseName != "" {
		q += fmt.Sprintf(", @database_name = N'%s'", escapeSingle(req.DatabaseName))
	}
	if req.NotificationMessage != "" {
		q += fmt.Sprintf(", @notification_message = N'%s'", escapeSingle(req.NotificationMessage))
	}
	if req.Category != "" {
		q += fmt.Sprintf(", @category_name = N'%s'", escapeSingle(req.Category))
	}
	if err := s.execContext(ctx, q); err != nil {
		return nil, fmt.Errorf("gosmo: create alert %q: %w", req.Name, err)
	}
	return s.AlertByNameContext(ctx, req.Name)
}

// Rename changes the alert's name.
func (a *Alert) Rename(newName string) error { return a.RenameContext(context.Background(), newName) }

// RenameContext is the context-aware variant of Rename.
func (a *Alert) RenameContext(ctx context.Context, newName string) error {
	q := fmt.Sprintf("EXEC msdb.dbo.sp_update_alert @name = N'%s', @new_name = N'%s'",
		escapeSingle(a.Name), escapeSingle(newName))
	if err := a.server.execContext(ctx, q); err != nil {
		return fmt.Errorf("gosmo: rename alert %q to %q: %w", a.Name, newName, err)
	}
	a.Name = newName
	return nil
}

// Enable enables the alert.
func (a *Alert) Enable() error { return a.EnableContext(context.Background()) }

// EnableContext is the context-aware variant of Enable.
func (a *Alert) EnableContext(ctx context.Context) error { return a.setEnabled(ctx, true) }

// Disable disables the alert.
func (a *Alert) Disable() error { return a.DisableContext(context.Background()) }

// DisableContext is the context-aware variant of Disable.
func (a *Alert) DisableContext(ctx context.Context) error { return a.setEnabled(ctx, false) }

func (a *Alert) setEnabled(ctx context.Context, on bool) error {
	q := fmt.Sprintf("EXEC msdb.dbo.sp_update_alert @name = N'%s', @enabled = %d", escapeSingle(a.Name), boolToInt(on))
	if err := a.server.execContext(ctx, q); err != nil {
		return fmt.Errorf("gosmo: set enabled=%v for alert %q: %w", on, a.Name, err)
	}
	a.Enabled = on
	return nil
}

// SetTrigger sets what raises the alert: a specific SQL Server error
// number, or a severity level. SQL Server treats these as mutually
// exclusive — pass 0 for whichever one isn't in use.
func (a *Alert) SetTrigger(errorNumber, severity int) error {
	return a.SetTriggerContext(context.Background(), errorNumber, severity)
}

// SetTriggerContext is the context-aware variant of SetTrigger.
func (a *Alert) SetTriggerContext(ctx context.Context, errorNumber, severity int) error {
	q := fmt.Sprintf("EXEC msdb.dbo.sp_update_alert @name = N'%s', @message_id = %d, @severity = %d",
		escapeSingle(a.Name), errorNumber, severity)
	if err := a.server.execContext(ctx, q); err != nil {
		return fmt.Errorf("gosmo: set trigger for alert %q: %w", a.Name, err)
	}
	a.ErrorNumber, a.Severity = errorNumber, severity
	return nil
}

// SetDatabase scopes the alert to a single database, or "" for all databases.
func (a *Alert) SetDatabase(dbName string) error {
	return a.SetDatabaseContext(context.Background(), dbName)
}

// SetDatabaseContext is the context-aware variant of SetDatabase.
func (a *Alert) SetDatabaseContext(ctx context.Context, dbName string) error {
	q := fmt.Sprintf("EXEC msdb.dbo.sp_update_alert @name = N'%s', @database_name = N'%s'",
		escapeSingle(a.Name), escapeSingle(dbName))
	if err := a.server.execContext(ctx, q); err != nil {
		return fmt.Errorf("gosmo: set database for alert %q: %w", a.Name, err)
	}
	a.DatabaseName = dbName
	return nil
}

// SetDelay sets the minimum delay between repeated responses to the alert.
func (a *Alert) SetDelay(d time.Duration) error { return a.SetDelayContext(context.Background(), d) }

// SetDelayContext is the context-aware variant of SetDelay.
func (a *Alert) SetDelayContext(ctx context.Context, d time.Duration) error {
	q := fmt.Sprintf("EXEC msdb.dbo.sp_update_alert @name = N'%s', @delay_between_responses = %d",
		escapeSingle(a.Name), int(d.Seconds()))
	if err := a.server.execContext(ctx, q); err != nil {
		return fmt.Errorf("gosmo: set delay for alert %q: %w", a.Name, err)
	}
	a.DelayBetweenResponses = d
	return nil
}

// SetNotificationMessage sets the extra text appended to the alert's
// notification.
func (a *Alert) SetNotificationMessage(msg string) error {
	return a.SetNotificationMessageContext(context.Background(), msg)
}

// SetNotificationMessageContext is the context-aware variant of SetNotificationMessage.
func (a *Alert) SetNotificationMessageContext(ctx context.Context, msg string) error {
	q := fmt.Sprintf("EXEC msdb.dbo.sp_update_alert @name = N'%s', @notification_message = N'%s'",
		escapeSingle(a.Name), escapeSingle(msg))
	if err := a.server.execContext(ctx, q); err != nil {
		return fmt.Errorf("gosmo: set notification message for alert %q: %w", a.Name, err)
	}
	a.NotificationMessage = msg
	return nil
}

// SetCategory reassigns the alert's category. category == "" clears it —
// sent as the real [Uncategorized] category, since sp_update_alert rejects
// an empty name outright ("The specified @category_name (”) does not
// exist", live-verified) and [Uncategorized] is what an alert created with
// no category actually holds in msdb.dbo.syscategories.
func (a *Alert) SetCategory(category string) error {
	return a.SetCategoryContext(context.Background(), category)
}

// SetCategoryContext is the context-aware variant of SetCategory.
func (a *Alert) SetCategoryContext(ctx context.Context, category string) error {
	target := category
	if target == "" {
		target = "[Uncategorized]"
	}
	q := fmt.Sprintf("EXEC msdb.dbo.sp_update_alert @name = N'%s', @category_name = N'%s'",
		escapeSingle(a.Name), escapeSingle(target))
	if err := a.server.execContext(ctx, q); err != nil {
		return fmt.Errorf("gosmo: set category for alert %q: %w", a.Name, err)
	}
	a.Category = target
	return nil
}

// SetJobResponse sets the job executed in response to this alert, or ""
// to clear it.
func (a *Alert) SetJobResponse(jobName string) error {
	return a.SetJobResponseContext(context.Background(), jobName)
}

// SetJobResponseContext is the context-aware variant of SetJobResponse.
func (a *Alert) SetJobResponseContext(ctx context.Context, jobName string) error {
	target := jobName
	if target == "" {
		// sp_update_alert's documented sentinel for "no job response".
		target = "[UNSPECIFIED]"
	}
	q := fmt.Sprintf("EXEC msdb.dbo.sp_update_alert @name = N'%s', @job_name = N'%s'",
		escapeSingle(a.Name), escapeSingle(target))
	if err := a.server.execContext(ctx, q); err != nil {
		return fmt.Errorf("gosmo: set job response for alert %q: %w", a.Name, err)
	}
	a.JobName = jobName
	return nil
}

// Drop deletes the alert via sp_delete_alert.
func (a *Alert) Drop() error { return a.DropContext(context.Background()) }

// DropContext is the context-aware variant of Drop.
func (a *Alert) DropContext(ctx context.Context) error {
	q := fmt.Sprintf("EXEC msdb.dbo.sp_delete_alert @name = N'%s'", escapeSingle(a.Name))
	if err := a.server.execContext(ctx, q); err != nil {
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
func (a *Alert) Notify(operatorName string, method NotificationMethod) error {
	return a.NotifyContext(context.Background(), operatorName, method)
}

// NotifyContext is the context-aware variant of Notify.
func (a *Alert) NotifyContext(ctx context.Context, operatorName string, method NotificationMethod) error {
	q := fmt.Sprintf("EXEC msdb.dbo.sp_add_notification @alert_name = N'%s', @operator_name = N'%s', @notification_method = %d",
		escapeSingle(a.Name), escapeSingle(operatorName), int(method))
	if err := a.server.execContext(ctx, q); err != nil {
		return fmt.Errorf("gosmo: add notification for alert %q to operator %q: %w", a.Name, operatorName, err)
	}
	return nil
}

// RemoveNotify removes an operator's notification link from the alert.
func (a *Alert) RemoveNotify(operatorName string) error {
	return a.RemoveNotifyContext(context.Background(), operatorName)
}

// RemoveNotifyContext is the context-aware variant of RemoveNotify.
func (a *Alert) RemoveNotifyContext(ctx context.Context, operatorName string) error {
	q := fmt.Sprintf("EXEC msdb.dbo.sp_delete_notification @alert_name = N'%s', @operator_name = N'%s'",
		escapeSingle(a.Name), escapeSingle(operatorName))
	if err := a.server.execContext(ctx, q); err != nil {
		return fmt.Errorf("gosmo: remove notification for alert %q from operator %q: %w", a.Name, operatorName, err)
	}
	return nil
}

// Notifications returns every operator notified by this alert.
func (a *Alert) Notifications() ([]*AlertNotification, error) {
	return a.NotificationsContext(context.Background())
}

// NotificationsContext is the context-aware variant of Notifications.
func (a *Alert) NotificationsContext(ctx context.Context) ([]*AlertNotification, error) {
	const q = `
SELECT o.name, n.notification_method
FROM   msdb.dbo.sysnotifications n
JOIN   msdb.dbo.sysoperators o ON o.id = n.operator_id
WHERE  n.alert_id = @p1
ORDER  BY o.name`

	rows, err := a.server.db.QueryContext(ctx, q, a.ID)
	if err != nil {
		return nil, fmt.Errorf("gosmo: notifications for alert %q: %w", a.Name, err)
	}
	defer rows.Close()

	var out []*AlertNotification
	for rows.Next() {
		n := &AlertNotification{}
		var method int
		if err := rows.Scan(&n.OperatorName, &method); err != nil {
			return nil, err
		}
		n.Method = NotificationMethod(method)
		out = append(out, n)
	}
	return out, rows.Err()
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
