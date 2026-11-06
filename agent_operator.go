package gosmo

import (
	"context"
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

// operatorFrom is a sysadmin's operator read (see agentRead):
// sysoperators, which msdb grants SQLAgentOperatorRole only.
const operatorFrom = `FROM   msdb.dbo.sysoperators o
LEFT   JOIN msdb.dbo.syscategories c ON c.category_id = o.category_id`

// operatorProcTable declares @op and fills it from sp_help_operator, which
// msdb grants SQLAgentUserRole (and through it Reader and Operator): every
// operator, the procedure applying no per-login filter. jobProcTables
// reads the e-mail operator's name from the same table.
//
// The columns are sp_help_operator's result set, the same 20 on 13.0 and
// 17.0 (sys.dm_exec_describe_first_result_set, 2026-11-05). Every text column
// is max-sized: INSERT ... EXECUTE fails Msg 8152 on a value longer than its
// target, where a select would have returned it.
const operatorProcTable = `
DECLARE @op TABLE (id int, name sysname, enabled tinyint, email_address nvarchar(max),
  last_email_date int, last_email_time int, pager_address nvarchar(max), last_pager_date int,
  last_pager_time int, weekday_pager_start_time int, weekday_pager_end_time int,
  saturday_pager_start_time int, saturday_pager_end_time int, sunday_pager_start_time int,
  sunday_pager_end_time int, pager_days tinyint, netsend_address nvarchar(max),
  last_netsend_date int, last_netsend_time int, category_name sysname NULL);
INSERT INTO @op EXECUTE msdb.dbo.sp_help_operator;`

// operatorProcFrom is everyone else's: operatorColumns over @op, the
// category name the procedure already resolved standing in for the
// syscategories join.
const operatorProcFrom = `FROM   @op o
OUTER  APPLY (SELECT o.category_name AS name) c`

// operatorRead is the batch Operators and OperatorByName send, tail being
// the WHERE or ORDER BY each appends to both reads.
func operatorRead(tail string) string {
	return agentRead("\nSELECT "+operatorColumns+" "+operatorFrom+tail,
		operatorProcTable+"\nSELECT "+operatorColumns+" "+operatorProcFrom+tail)
}

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

// Operators returns every SQL Server Agent operator defined on the server,
// to sysadmin and to every msdb Agent role (see operatorProcTable).
func (s *Server) Operators(ctx context.Context) ([]*Operator, error) {
	q := operatorRead(`
ORDER  BY o.name`)

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

// OperatorByName returns a single operator by name, read as Operators reads
// it.
func (s *Server) OperatorByName(ctx context.Context, name string) (*Operator, error) {
	q := operatorRead(`
WHERE  o.name = @p1`)

	return readByName(ctx, s, scanOperator, q, []any{name},
		notFoundf("gosmo: operator %q not found", name), fmt.Sprintf("read operator %q", name))
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
		return nil, invalidf("gosmo: create operator: name is required")
	}
	var args boundArgs
	q := fmt.Sprintf("EXEC msdb.dbo.sp_add_operator @name = %s, @enabled = %d",
		args.str(req.Name), boolToInt(req.Enabled))
	if req.EmailAddress != "" {
		q += ", @email_address = " + args.str(req.EmailAddress)
	}
	if req.Category != "" {
		q += ", @category_name = " + args.str(req.Category)
	}
	if _, err := s.exec(ctx, q, args.vals...); err != nil {
		return nil, fmt.Errorf("gosmo: create operator %q: %w", req.Name, err)
	}
	return createdObject(ctx, s.OperatorRef(req.Name), func() (*Operator, error) {
		return s.OperatorByName(ctx, req.Name)
	})
}

// Rename changes the operator's name.
func (o *Operator) Rename(ctx context.Context, newName string) error {
	return o.Alter(ctx, OperatorChanges{Name: &newName})
}

// Enable enables the operator.
func (o *Operator) Enable(ctx context.Context) error { return o.SetEnabled(ctx, true) }

// Disable disables the operator.
func (o *Operator) Disable(ctx context.Context) error { return o.SetEnabled(ctx, false) }

// SetEnabled is Enable when on is true and Disable otherwise.
func (o *Operator) SetEnabled(ctx context.Context, on bool) error {
	return o.Alter(ctx, OperatorChanges{Enabled: &on})
}

// Drop deletes the operator via sp_delete_operator.
func (o *Operator) Drop(ctx context.Context) error {
	if _, err := o.server.exec(ctx, "EXEC msdb.dbo.sp_delete_operator @name = @p1", o.Name); err != nil {
		return fmt.Errorf("gosmo: drop operator %q: %w", o.Name, err)
	}
	return nil
}

// OperatorAlert is one alert configured to notify an operator, as
// NotifyingAlerts lists it.
type OperatorAlert struct {
	AlertName string
	Method    NotificationMethod
}

// NotifyingAlerts returns every alert configured to notify this operator.
// It needs SQLAgentOperatorRole, as Alert.Notifications does.
func (o *Operator) NotifyingAlerts(ctx context.Context) ([]*OperatorAlert, error) {
	const q = `
SELECT a.name, n.notification_method
FROM   msdb.dbo.sysnotifications n
JOIN   msdb.dbo.sysalerts a ON a.id = n.alert_id
WHERE  n.operator_id = @p1
ORDER  BY a.name`

	return notificationRows(ctx, o.server, fmt.Sprintf("notifying alerts for operator %q", o.Name), q, o.ID,
		func(name string, code int) *OperatorAlert {
			return &OperatorAlert{AlertName: name, Method: NotificationMethod(code)}
		})
}

// OperatorJob is one job configured to email an operator on completion, as
// NotifyingJobs lists it.
type OperatorJob struct {
	JobName string
	Level   NotifyLevel
}

// NotifyingJobs returns every job configured to email this operator on
// completion (notify_email_operator_id) — distinct from NotifyingAlerts,
// which covers alert-triggered notifications.
//
// It reads sysjobs_view, granted to SQLAgentUserRole, not sysjobs, granted to
// TargetServersRole only — which refused it to every Agent role. The view
// applies msdb's job visibility, as Jobs does: sysadmin and
// SQLAgentReaderRole (so Operator) see every job, anyone else the jobs it
// owns (its definition, the same on 13.0 and 17.0, read 2026-11-05).
func (o *Operator) NotifyingJobs(ctx context.Context) ([]*OperatorJob, error) {
	const q = `
SELECT name, notify_level_email
FROM   msdb.dbo.sysjobs_view
WHERE  notify_email_operator_id = @p1
ORDER  BY name`

	return notificationRows(ctx, o.server, fmt.Sprintf("notifying jobs for operator %q", o.Name), q, o.ID,
		func(name string, code int) *OperatorJob {
			return &OperatorJob{JobName: name, Level: NotifyLevel(code)}
		})
}
