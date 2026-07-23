package gosmo

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ============================================================
// SQL Server Agent -- Operators
// ============================================================

// Operator represents a SQL Server Agent operator (msdb.dbo.sysoperators) —
// a notification target for job and alert email.
type Operator struct {
	server          *Server
	ID              int
	Name            string
	Enabled         bool
	EmailAddress    string
	PagerAddress    string
	NetSendAddress  string
	Category        string
	LastEmailDate   time.Time
	LastPagerDate   time.Time
	LastNetSendDate time.Time
}

const operatorColumns = `o.id, o.name, o.enabled,
       ISNULL(o.email_address,''), ISNULL(o.pager_address,''), ISNULL(o.netsend_address,''),
       ISNULL(c.name,''),
       ISNULL(o.last_email_date,0), ISNULL(o.last_email_time,0),
       ISNULL(o.last_pager_date,0), ISNULL(o.last_pager_time,0),
       ISNULL(o.last_netsend_date,0), ISNULL(o.last_netsend_time,0)`

const operatorFrom = `FROM   msdb.dbo.sysoperators o
LEFT   JOIN msdb.dbo.syscategories c ON c.category_id = o.category_id`

// scanOperator scans one row shaped like operatorColumns into a new Operator.
func scanOperator(s *Server, scan func(dest ...any) error) (*Operator, error) {
	o := &Operator{server: s}
	var emailD, emailT, pagerD, pagerT, netD, netT int
	if err := scan(
		&o.ID, &o.Name, &o.Enabled,
		&o.EmailAddress, &o.PagerAddress, &o.NetSendAddress,
		&o.Category,
		&emailD, &emailT, &pagerD, &pagerT, &netD, &netT,
	); err != nil {
		return nil, err
	}
	o.LastEmailDate = parseSQLAgentDateOrZero(emailD, emailT)
	o.LastPagerDate = parseSQLAgentDateOrZero(pagerD, pagerT)
	o.LastNetSendDate = parseSQLAgentDateOrZero(netD, netT)
	return o, nil
}

// Operators returns every SQL Server Agent operator defined on the server.
func (s *Server) Operators() ([]*Operator, error) { return s.OperatorsContext(context.Background()) }

// OperatorsContext is the context-aware variant of Operators.
func (s *Server) OperatorsContext(ctx context.Context) ([]*Operator, error) {
	q := "SELECT " + operatorColumns + " " + operatorFrom + " ORDER BY o.name"

	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("gosmo: list operators: %w", err)
	}
	defer rows.Close()

	var out []*Operator
	for rows.Next() {
		o, err := scanOperator(s, rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// OperatorByName returns a single operator by name.
func (s *Server) OperatorByName(name string) (*Operator, error) {
	return s.OperatorByNameContext(context.Background(), name)
}

// OperatorByNameContext is the context-aware variant of OperatorByName.
func (s *Server) OperatorByNameContext(ctx context.Context, name string) (*Operator, error) {
	q := "SELECT " + operatorColumns + " " + operatorFrom + " WHERE o.name = @p1"

	row := s.db.QueryRowContext(ctx, q, name)
	o, err := scanOperator(s, row.Scan)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("gosmo: operator %q not found", name)
		}
		return nil, fmt.Errorf("gosmo: operator by name: %w", err)
	}
	return o, nil
}

// CreateOperatorRequest describes a new SQL Server Agent operator.
type CreateOperatorRequest struct {
	Name         string
	Enabled      bool
	EmailAddress string
	Category     string
}

// CreateOperator creates a new operator via sp_add_operator.
func (s *Server) CreateOperator(req CreateOperatorRequest) (*Operator, error) {
	return s.CreateOperatorContext(context.Background(), req)
}

// CreateOperatorContext is the context-aware variant of CreateOperator.
func (s *Server) CreateOperatorContext(ctx context.Context, req CreateOperatorRequest) (*Operator, error) {
	if req.Name == "" {
		return nil, fmt.Errorf("gosmo: create operator: name is required")
	}
	q := fmt.Sprintf("EXEC msdb.dbo.sp_add_operator @name = N'%s', @enabled = %d",
		escapeSingle(req.Name), boolToInt(req.Enabled))
	if req.EmailAddress != "" {
		q += fmt.Sprintf(", @email_address = N'%s'", escapeSingle(req.EmailAddress))
	}
	if req.Category != "" {
		q += fmt.Sprintf(", @category_name = N'%s'", escapeSingle(req.Category))
	}
	if err := s.execContext(ctx, q); err != nil {
		return nil, fmt.Errorf("gosmo: create operator %q: %w", req.Name, err)
	}
	return s.OperatorByNameContext(ctx, req.Name)
}

// Rename changes the operator's name.
func (o *Operator) Rename(newName string) error {
	return o.RenameContext(context.Background(), newName)
}

// RenameContext is the context-aware variant of Rename.
func (o *Operator) RenameContext(ctx context.Context, newName string) error {
	q := fmt.Sprintf("EXEC msdb.dbo.sp_update_operator @name = N'%s', @new_name = N'%s'",
		escapeSingle(o.Name), escapeSingle(newName))
	if err := o.server.execContext(ctx, q); err != nil {
		return fmt.Errorf("gosmo: rename operator %q to %q: %w", o.Name, newName, err)
	}
	o.Name = newName
	return nil
}

// Enable enables the operator.
func (o *Operator) Enable() error { return o.EnableContext(context.Background()) }

// EnableContext is the context-aware variant of Enable.
func (o *Operator) EnableContext(ctx context.Context) error { return o.setEnabled(ctx, true) }

// Disable disables the operator.
func (o *Operator) Disable() error { return o.DisableContext(context.Background()) }

// DisableContext is the context-aware variant of Disable.
func (o *Operator) DisableContext(ctx context.Context) error { return o.setEnabled(ctx, false) }

func (o *Operator) setEnabled(ctx context.Context, on bool) error {
	q := fmt.Sprintf("EXEC msdb.dbo.sp_update_operator @name = N'%s', @enabled = %d", escapeSingle(o.Name), boolToInt(on))
	if err := o.server.execContext(ctx, q); err != nil {
		return fmt.Errorf("gosmo: set enabled=%v for operator %q: %w", on, o.Name, err)
	}
	o.Enabled = on
	return nil
}

// SetEmailAddress changes the operator's email address.
func (o *Operator) SetEmailAddress(addr string) error {
	return o.SetEmailAddressContext(context.Background(), addr)
}

// SetEmailAddressContext is the context-aware variant of SetEmailAddress.
func (o *Operator) SetEmailAddressContext(ctx context.Context, addr string) error {
	q := fmt.Sprintf("EXEC msdb.dbo.sp_update_operator @name = N'%s', @email_address = N'%s'",
		escapeSingle(o.Name), escapeSingle(addr))
	if err := o.server.execContext(ctx, q); err != nil {
		return fmt.Errorf("gosmo: set email address for operator %q: %w", o.Name, err)
	}
	o.EmailAddress = addr
	return nil
}

// SetCategory reassigns the operator's category. category == "" clears it —
// sent as the real [Uncategorized] category, for the same reason
// Alert.SetCategory does: sp_update_operator's category check
// (sp_verify_category, shared with sp_update_alert) rejects an empty name
// outright.
func (o *Operator) SetCategory(category string) error {
	return o.SetCategoryContext(context.Background(), category)
}

// SetCategoryContext is the context-aware variant of SetCategory.
func (o *Operator) SetCategoryContext(ctx context.Context, category string) error {
	target := category
	if target == "" {
		target = "[Uncategorized]"
	}
	q := fmt.Sprintf("EXEC msdb.dbo.sp_update_operator @name = N'%s', @category_name = N'%s'",
		escapeSingle(o.Name), escapeSingle(target))
	if err := o.server.execContext(ctx, q); err != nil {
		return fmt.Errorf("gosmo: set category for operator %q: %w", o.Name, err)
	}
	o.Category = target
	return nil
}

// Drop deletes the operator via sp_delete_operator.
func (o *Operator) Drop() error { return o.DropContext(context.Background()) }

// DropContext is the context-aware variant of Drop.
func (o *Operator) DropContext(ctx context.Context) error {
	q := fmt.Sprintf("EXEC msdb.dbo.sp_delete_operator @name = N'%s'", escapeSingle(o.Name))
	if err := o.server.execContext(ctx, q); err != nil {
		return fmt.Errorf("gosmo: drop operator %q: %w", o.Name, err)
	}
	return nil
}

// AlertNotificationRef describes one alert configured to notify an operator.
type AlertNotificationRef struct {
	AlertName string
	Method    NotificationMethod
}

// NotifyingAlerts returns every alert configured to notify this operator.
func (o *Operator) NotifyingAlerts() ([]*AlertNotificationRef, error) {
	return o.NotifyingAlertsContext(context.Background())
}

// NotifyingAlertsContext is the context-aware variant of NotifyingAlerts.
func (o *Operator) NotifyingAlertsContext(ctx context.Context) ([]*AlertNotificationRef, error) {
	const q = `
SELECT a.name, n.notification_method
FROM   msdb.dbo.sysnotifications n
JOIN   msdb.dbo.sysalerts a ON a.id = n.alert_id
WHERE  n.operator_id = @p1
ORDER  BY a.name`

	rows, err := o.server.db.QueryContext(ctx, q, o.ID)
	if err != nil {
		return nil, fmt.Errorf("gosmo: notifying alerts for operator %q: %w", o.Name, err)
	}
	defer rows.Close()

	var out []*AlertNotificationRef
	for rows.Next() {
		r := &AlertNotificationRef{}
		var method int
		if err := rows.Scan(&r.AlertName, &method); err != nil {
			return nil, err
		}
		r.Method = NotificationMethod(method)
		out = append(out, r)
	}
	return out, rows.Err()
}

// JobNotificationRef describes one job configured to email an operator on
// completion.
type JobNotificationRef struct {
	JobName string
	Level   NotifyLevel
}

// NotifyingJobs returns every job configured to email this operator on
// completion (sysjobs.notify_email_operator_id) — distinct from
// NotifyingAlerts, which covers alert-triggered notifications.
func (o *Operator) NotifyingJobs() ([]*JobNotificationRef, error) {
	return o.NotifyingJobsContext(context.Background())
}

// NotifyingJobsContext is the context-aware variant of NotifyingJobs.
func (o *Operator) NotifyingJobsContext(ctx context.Context) ([]*JobNotificationRef, error) {
	const q = `
SELECT name, notify_level_email
FROM   msdb.dbo.sysjobs
WHERE  notify_email_operator_id = @p1
ORDER  BY name`

	rows, err := o.server.db.QueryContext(ctx, q, o.ID)
	if err != nil {
		return nil, fmt.Errorf("gosmo: notifying jobs for operator %q: %w", o.Name, err)
	}
	defer rows.Close()

	var out []*JobNotificationRef
	for rows.Next() {
		r := &JobNotificationRef{}
		var level int
		if err := rows.Scan(&r.JobName, &level); err != nil {
			return nil, err
		}
		r.Level = NotifyLevel(level)
		out = append(out, r)
	}
	return out, rows.Err()
}
