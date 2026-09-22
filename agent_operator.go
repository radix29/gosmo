package gosmo

import (
	"context"
	"database/sql"
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

// Server returns the server the operator belongs to.
func (o *Operator) Server() *Server { return o.server }

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
func (s *Server) Operators(ctx context.Context) ([]*Operator, error) {
	q := "SELECT " + operatorColumns + " " + operatorFrom + " ORDER BY o.name"

	rows, err := s.query(ctx, q)
	return scanRows(rows, err, "list operators", func(scan func(...any) error) (*Operator, error) {
		return scanOperator(s, scan)
	})
}

// OperatorRef returns a lightweight handle for an operator by name, without
// querying msdb — the operator-side counterpart of Server.DatabaseRef. ID,
// EmailAddress, Category and every other cached field stay at their zero
// value; OperatorByName is what populates them.
//
// Every write method on *Operator addresses the operator by name, so this
// handle is enough to keep operating on an operator the caller already
// knows exists — and is the form to use when there is nothing to read yet:
// under a WithScript-derived context, OperatorByName's lookup is a
// real read and an operator whose sp_add_operator was merely collected is
// not there to find.
func (s *Server) OperatorRef(name string) *Operator {
	return &Operator{server: s, Name: name}
}

// OperatorByName returns a single operator by name.
func (s *Server) OperatorByName(ctx context.Context, name string) (*Operator, error) {
	q := "SELECT " + operatorColumns + " " + operatorFrom + " WHERE o.name = @p1"

	var o *Operator
	err := s.queryRow(ctx, func(row *sql.Row) error {
		var scanErr error
		o, scanErr = scanOperator(s, row.Scan)
		return scanErr
	}, q, name)
	return foundRow(o, err, notFoundf("gosmo: operator %q not found", name), "operator by name")
}

// CreateOperatorRequest describes a new SQL Server Agent operator.
type CreateOperatorRequest struct {
	Name         string
	Enabled      bool
	EmailAddress string
	Category     string
}

// CreateOperator creates a new operator via sp_add_operator.
func (s *Server) CreateOperator(ctx context.Context, req CreateOperatorRequest) (*Operator, error) {
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
	if err := s.exec(ctx, q); err != nil {
		return nil, fmt.Errorf("gosmo: create operator %q: %w", req.Name, err)
	}
	if Scripting(ctx) {
		// See CreateSchedule.
		return s.OperatorRef(req.Name), nil
	}
	return s.OperatorByName(ctx, req.Name)
}

// Rename changes the operator's name.
func (o *Operator) Rename(ctx context.Context, newName string) error {
	q := fmt.Sprintf("EXEC msdb.dbo.sp_update_operator @name = N'%s', @new_name = N'%s'",
		escapeSingle(o.Name), escapeSingle(newName))
	if err := o.server.exec(ctx, q); err != nil {
		return fmt.Errorf("gosmo: rename operator %q to %q: %w", o.Name, newName, err)
	}
	setIfApplied(ctx, &o.Name, newName)
	return nil
}

// Enable enables the operator.
func (o *Operator) Enable(ctx context.Context) error { return o.setEnabled(ctx, true) }

// Disable disables the operator.
func (o *Operator) Disable(ctx context.Context) error { return o.setEnabled(ctx, false) }

func (o *Operator) setEnabled(ctx context.Context, on bool) error {
	q := fmt.Sprintf("EXEC msdb.dbo.sp_update_operator @name = N'%s', @enabled = %d", escapeSingle(o.Name), boolToInt(on))
	if err := o.server.exec(ctx, q); err != nil {
		return fmt.Errorf("gosmo: set enabled=%v for operator %q: %w", on, o.Name, err)
	}
	setIfApplied(ctx, &o.Enabled, on)
	return nil
}

// SetEmailAddress changes the operator's email address.
func (o *Operator) SetEmailAddress(ctx context.Context, addr string) error {
	q := fmt.Sprintf("EXEC msdb.dbo.sp_update_operator @name = N'%s', @email_address = N'%s'",
		escapeSingle(o.Name), escapeSingle(addr))
	if err := o.server.exec(ctx, q); err != nil {
		return fmt.Errorf("gosmo: set email address for operator %q: %w", o.Name, err)
	}
	setIfApplied(ctx, &o.EmailAddress, addr)
	return nil
}

// SetCategory reassigns the operator's category. category == "" clears it —
// sent as the real [Uncategorized] category, for the same reason
// Alert.SetCategory does: sp_update_operator's category check
// (sp_verify_category, shared with sp_update_alert) rejects an empty name
// outright.
func (o *Operator) SetCategory(ctx context.Context, category string) error {
	target := agentCategoryTarget(category)
	q := fmt.Sprintf("EXEC msdb.dbo.sp_update_operator @name = N'%s', @category_name = N'%s'",
		escapeSingle(o.Name), escapeSingle(target))
	if err := o.server.exec(ctx, q); err != nil {
		return fmt.Errorf("gosmo: set category for operator %q: %w", o.Name, err)
	}
	setIfApplied(ctx, &o.Category, target)
	return nil
}

// Drop deletes the operator via sp_delete_operator.
func (o *Operator) Drop(ctx context.Context) error {
	q := fmt.Sprintf("EXEC msdb.dbo.sp_delete_operator @name = N'%s'", escapeSingle(o.Name))
	if err := o.server.exec(ctx, q); err != nil {
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
func (o *Operator) NotifyingAlerts(ctx context.Context) ([]*AlertNotificationRef, error) {
	const q = `
SELECT a.name, n.notification_method
FROM   msdb.dbo.sysnotifications n
JOIN   msdb.dbo.sysalerts a ON a.id = n.alert_id
WHERE  n.operator_id = @p1
ORDER  BY a.name`

	rows, err := o.server.query(ctx, q, o.ID)
	return scanRows(rows, err, fmt.Sprintf("notifying alerts for operator %q", o.Name), func(scan func(...any) error) (*AlertNotificationRef, error) {
		r := &AlertNotificationRef{}
		var method int
		if err := scan(&r.AlertName, &method); err != nil {
			return nil, err
		}
		r.Method = NotificationMethod(method)
		return r, nil
	})
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
func (o *Operator) NotifyingJobs(ctx context.Context) ([]*JobNotificationRef, error) {
	const q = `
SELECT name, notify_level_email
FROM   msdb.dbo.sysjobs
WHERE  notify_email_operator_id = @p1
ORDER  BY name`

	rows, err := o.server.query(ctx, q, o.ID)
	return scanRows(rows, err, fmt.Sprintf("notifying jobs for operator %q", o.Name), func(scan func(...any) error) (*JobNotificationRef, error) {
		r := &JobNotificationRef{}
		var level int
		if err := scan(&r.JobName, &level); err != nil {
			return nil, err
		}
		r.Level = NotifyLevel(level)
		return r, nil
	})
}
