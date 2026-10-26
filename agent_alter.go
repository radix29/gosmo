package gosmo

// agent_alter.go is the batched form of the SQL Server Agent property
// writes: one Changes struct and one Alter method per family, each emitting
// a single sp_update_* call naming only the parameters the caller set.
//
// Alter is the one way to change these properties. Rename, Enable, Disable
// and SetEnabled stay beside it — they read better than a struct literal —
// and each is a one-line wrapper over Alter, so there is one statement
// builder, one binding path and one receiver update per family. The other
// per-property setters (Job.SetOwner, Alert.SetTrigger, Schedule.SetFrequency
// and the rest) were removed on 2026-10-26: each was a second spelling of one
// Changes field, and five of them for a Properties dialog's five edits were
// four round trips more than msdb's sp_update_* procedures, which take every
// updatable parameter in one call, asked for. (They had begun as hand-rolled
// copies, and the job copy and Alter both missed the empty-category mapping
// the alert and operator paths had: a cleared job category was mirrored as ""
// while msdb reported [Uncategorized (Local)].)
//
// A nil field means "leave this alone" — the distinction a zero value
// cannot make, since 0, "" and false are all values these parameters
// accept. An empty Changes emits nothing at all rather than a parameterless
// sp_update_*, which msdb rejects.

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// agentParams accumulates the "@param = value" fragments of one sp_update_*
// call, in the order they were added. A string parameter is bound (args): msdb
// takes names, not identifiers, so there is nothing to bracket-quote, and a
// bound value needs no escaping. The key the caller renders binds through the
// same args, and the call is sent with args.vals.
type agentParams struct {
	parts []string
	args  boundArgs
}

// str appends a bound string parameter.
func (p *agentParams) str(name, v string) {
	p.parts = append(p.parts, name+" = "+p.args.str(v))
}

// num appends a numeric parameter.
func (p *agentParams) num(name string, v int) {
	p.parts = append(p.parts, fmt.Sprintf("%s = %d", name, v))
}

// flag appends a BIT parameter.
func (p *agentParams) flag(name string, v bool) {
	p.num(name, boolToInt(v))
}

func (p *agentParams) empty() bool { return len(p.parts) == 0 }

// statement renders the whole call. key is the already-rendered parameter
// that identifies the object — "@job_name = N'...'" or "@schedule_id = 7".
func (p *agentParams) statement(proc, key string) string {
	return "EXEC msdb.dbo." + proc + " " + key + ", " + strings.Join(p.parts, ", ")
}

// ============================================================
// Job
// ============================================================

// JobChanges is a batched update to a job's msdb properties, applied by
// Job.Alter. A nil field is left unchanged; a non-nil one is sent even when
// it holds the zero value.
//
// Name is sp_update_job's @new_name, as Job.Rename sends it.
type JobChanges struct {
	Name        *string
	Description *string
	// Category set to "" moves the job back to the default category, sent
	// as msdb's [DEFAULT] — see agentCategoryTarget.
	Category    *string
	OwnerLogin  *string
	Enabled     *bool
	StartStepID *int
	// NotifyLevelEmail and NotifyEmailOperatorName are set together: which
	// operator is emailed on completion, and when. SQL Server has no
	// documented "clear to none" value for @notify_email_operator_name, so
	// leave the operator nil to keep the configured one and send NotifyNever
	// to stop the mail.
	//
	// A level set on a job with no operator does not stick: msdb stores
	// notify_level_email as 0 and reports no error, since a level with nobody
	// to mail is meaningless to it (verified on SQL Server 17.0.1135.8,
	// 2026-09-17).
	NotifyLevelEmail        *NotifyLevel
	NotifyEmailOperatorName *string
	DeleteLevel             *NotifyLevel
}

func (ch JobChanges) params() *agentParams {
	p := &agentParams{}
	if ch.Name != nil {
		p.str("@new_name", *ch.Name)
	}
	if ch.Description != nil {
		p.str("@description", *ch.Description)
	}
	if ch.Category != nil {
		send, _ := agentCategoryTarget(CategoryClassJob, *ch.Category)
		p.str("@category_name", send)
	}
	if ch.OwnerLogin != nil {
		p.str("@owner_login_name", *ch.OwnerLogin)
	}
	if ch.Enabled != nil {
		p.flag("@enabled", *ch.Enabled)
	}
	if ch.StartStepID != nil {
		p.num("@start_step_id", *ch.StartStepID)
	}
	if ch.NotifyLevelEmail != nil {
		p.num("@notify_level_email", int(*ch.NotifyLevelEmail))
	}
	if ch.NotifyEmailOperatorName != nil {
		p.str("@notify_email_operator_name", *ch.NotifyEmailOperatorName)
	}
	if ch.DeleteLevel != nil {
		p.num("@delete_level", int(*ch.DeleteLevel))
	}
	return p
}

// Alter applies every property set on ch in one sp_update_job call. An
// empty JobChanges is a no-op and issues nothing.
func (j *Job) Alter(ctx context.Context, ch JobChanges) error {
	p := ch.params()
	if p.empty() {
		return nil
	}
	q := p.statement("sp_update_job", "@job_name = "+p.args.str(j.Name))
	if _, err := j.server.exec(ctx, q, p.args.vals...); err != nil {
		return fmt.Errorf("gosmo: alter job %q: %w", j.Name, err)
	}
	setPtrIfApplied(ctx, &j.Description, ch.Description)
	if ch.Category != nil {
		_, stored := agentCategoryTarget(CategoryClassJob, *ch.Category)
		setIfApplied(ctx, &j.Category, stored)
	}
	setPtrIfApplied(ctx, &j.OwnerLoginName, ch.OwnerLogin)
	setPtrIfApplied(ctx, &j.IsEnabled, ch.Enabled)
	setPtrIfApplied(ctx, &j.StartStepID, ch.StartStepID)
	setPtrIfApplied(ctx, &j.NotifyLevelEmail, ch.NotifyLevelEmail)
	setPtrIfApplied(ctx, &j.NotifyEmailOperatorName, ch.NotifyEmailOperatorName)
	setPtrIfApplied(ctx, &j.DeleteLevel, ch.DeleteLevel)
	// Last: every fragment above was built from the old name.
	setPtrIfApplied(ctx, &j.Name, ch.Name)
	return nil
}

// ============================================================
// Alert
// ============================================================

// AlertChanges is a batched update to an alert's msdb properties, applied
// by Alert.Alter. A nil field is left unchanged.
type AlertChanges struct {
	Name    *string
	Enabled *bool
	// ErrorNumber and Severity are the mutually exclusive triggers: what
	// raises the alert, a specific error number or a severity level. Set both,
	// with 0 for whichever is not in use — setting one alone leaves the other
	// in place, which is how msdb behaves.
	ErrorNumber *int
	Severity    *int
	// DatabaseName scopes the alert to one database, "" to all of them.
	DatabaseName *string
	// DelayBetweenResponses is the minimum delay between repeated
	// responses, sent in whole seconds.
	DelayBetweenResponses *time.Duration
	// NotificationMessage is the extra text appended to the notification.
	NotificationMessage *string
	// IncludeEventDescriptionIn is msdb's channel bitmask — see the field
	// of the same name on Alert.
	IncludeEventDescriptionIn *int
	// Category set to "" clears the category, sent as the real
	// [Uncategorized]: sp_update_alert rejects an empty name outright ("The
	// specified @category_name ('') does not exist"), and [Uncategorized] is
	// what an alert created with no category holds in
	// msdb.dbo.syscategories.
	Category *string
	// JobName is the job run in response to the alert; "" clears it. An
	// empty @job_name is sp_update_alert's own sentinel for "no job
	// response" — it maps N'' to a job_id of 0x00 before sp_verify_alert
	// would otherwise reject the name for not matching a job. Anything else,
	// a placeholder like [UNSPECIFIED] included, fails as a missing job.
	JobName *string
}

func (ch AlertChanges) params() *agentParams {
	p := &agentParams{}
	if ch.Name != nil {
		p.str("@new_name", *ch.Name)
	}
	if ch.Enabled != nil {
		p.flag("@enabled", *ch.Enabled)
	}
	if ch.ErrorNumber != nil {
		p.num("@message_id", *ch.ErrorNumber)
	}
	if ch.Severity != nil {
		p.num("@severity", *ch.Severity)
	}
	if ch.DatabaseName != nil {
		p.str("@database_name", *ch.DatabaseName)
	}
	if ch.DelayBetweenResponses != nil {
		p.num("@delay_between_responses", int(ch.DelayBetweenResponses.Seconds()))
	}
	if ch.NotificationMessage != nil {
		p.str("@notification_message", *ch.NotificationMessage)
	}
	if ch.IncludeEventDescriptionIn != nil {
		p.num("@include_event_description_in", *ch.IncludeEventDescriptionIn)
	}
	if ch.Category != nil {
		send, _ := agentCategoryTarget(CategoryClassAlert, *ch.Category)
		p.str("@category_name", send)
	}
	if ch.JobName != nil {
		p.str("@job_name", *ch.JobName)
	}
	return p
}

// Alter applies every property set on ch in one sp_update_alert call. An
// empty AlertChanges is a no-op and issues nothing.
func (a *Alert) Alter(ctx context.Context, ch AlertChanges) error {
	p := ch.params()
	if p.empty() {
		return nil
	}
	q := p.statement("sp_update_alert", "@name = "+p.args.str(a.Name))
	if _, err := a.server.exec(ctx, q, p.args.vals...); err != nil {
		return fmt.Errorf("gosmo: alter alert %q: %w", a.Name, err)
	}
	setPtrIfApplied(ctx, &a.Enabled, ch.Enabled)
	setPtrIfApplied(ctx, &a.ErrorNumber, ch.ErrorNumber)
	setPtrIfApplied(ctx, &a.Severity, ch.Severity)
	setPtrIfApplied(ctx, &a.DatabaseName, ch.DatabaseName)
	setPtrIfApplied(ctx, &a.DelayBetweenResponses, ch.DelayBetweenResponses)
	setPtrIfApplied(ctx, &a.NotificationMessage, ch.NotificationMessage)
	setPtrIfApplied(ctx, &a.IncludeEventDescriptionIn, ch.IncludeEventDescriptionIn)
	if ch.Category != nil {
		_, stored := agentCategoryTarget(CategoryClassAlert, *ch.Category)
		setIfApplied(ctx, &a.Category, stored)
	}
	setPtrIfApplied(ctx, &a.JobName, ch.JobName)
	setPtrIfApplied(ctx, &a.Name, ch.Name)
	return nil
}

// ============================================================
// Operator
// ============================================================

// OperatorChanges is a batched update to an operator's msdb properties,
// applied by Operator.Alter. A nil field is left unchanged.
type OperatorChanges struct {
	Name           *string
	Enabled        *bool
	EmailAddress   *string
	PagerAddress   *string
	NetSendAddress *string
	// Category set to "" clears the category, sent as the real
	// [Uncategorized] for the reason AlertChanges.Category is:
	// sp_update_operator's category check (sp_verify_category, shared with
	// sp_update_alert) rejects an empty name outright.
	Category *string
}

func (ch OperatorChanges) params() *agentParams {
	p := &agentParams{}
	if ch.Name != nil {
		p.str("@new_name", *ch.Name)
	}
	if ch.Enabled != nil {
		p.flag("@enabled", *ch.Enabled)
	}
	if ch.EmailAddress != nil {
		p.str("@email_address", *ch.EmailAddress)
	}
	if ch.PagerAddress != nil {
		p.str("@pager_address", *ch.PagerAddress)
	}
	if ch.NetSendAddress != nil {
		p.str("@netsend_address", *ch.NetSendAddress)
	}
	if ch.Category != nil {
		send, _ := agentCategoryTarget(CategoryClassOperator, *ch.Category)
		p.str("@category_name", send)
	}
	return p
}

// Alter applies every property set on ch in one sp_update_operator call. An
// empty OperatorChanges is a no-op and issues nothing.
func (o *Operator) Alter(ctx context.Context, ch OperatorChanges) error {
	p := ch.params()
	if p.empty() {
		return nil
	}
	q := p.statement("sp_update_operator", "@name = "+p.args.str(o.Name))
	if _, err := o.server.exec(ctx, q, p.args.vals...); err != nil {
		return fmt.Errorf("gosmo: alter operator %q: %w", o.Name, err)
	}
	setPtrIfApplied(ctx, &o.Enabled, ch.Enabled)
	setPtrIfApplied(ctx, &o.EmailAddress, ch.EmailAddress)
	setPtrIfApplied(ctx, &o.PagerAddress, ch.PagerAddress)
	setPtrIfApplied(ctx, &o.NetSendAddress, ch.NetSendAddress)
	if ch.Category != nil {
		_, stored := agentCategoryTarget(CategoryClassOperator, *ch.Category)
		setIfApplied(ctx, &o.Category, stored)
	}
	setPtrIfApplied(ctx, &o.Name, ch.Name)
	return nil
}

// ============================================================
// Schedule
// ============================================================

// ScheduleActiveRange is the Duration section of a schedule: the date range
// it is active over, plus the daily HHMMSS time-of-day window it may fire
// within, as ScheduleChanges.Range carries it. A zero EndDate means "no end
// date".
//
// A zero StartDate or EndTime is sent as 0, which msdb treats as "unchanged"
// and "end of day (235959)" respectively (probed on SQL Server 2025); after
// Alter the cached Schedule fields hold the zero values, not what msdb kept,
// so reload the schedule to read them back.
type ScheduleActiveRange struct {
	StartDate time.Time
	EndDate   time.Time
	// StartTime and EndTime are HHMMSS integers, e.g. 10000 = 01:00:00.
	StartTime int
	EndTime   int
}

// ScheduleChanges is a batched update to a shared schedule's msdb
// properties, applied by Schedule.Alter. A nil field is left unchanged.
//
// Frequency and Range are set as whole units rather than field by field,
// for the reason ScheduleFrequency itself states: freq_interval's meaning
// depends on freq_type, so half a frequency is not a frequency.
type ScheduleChanges struct {
	Name       *string
	Enabled    *bool
	Frequency  *ScheduleFrequency
	Range      *ScheduleActiveRange
	OwnerLogin *string
}

func (ch ScheduleChanges) params() *agentParams {
	p := &agentParams{}
	if ch.Name != nil {
		p.str("@new_name", *ch.Name)
	}
	if ch.Enabled != nil {
		p.flag("@enabled", *ch.Enabled)
	}
	if f := ch.Frequency; f != nil {
		p.num("@freq_type", int(f.FreqType))
		p.num("@freq_interval", f.FreqInterval)
		p.num("@freq_subday_type", int(f.FreqSubdayType))
		p.num("@freq_subday_interval", f.FreqSubdayInterval)
		p.num("@freq_relative_interval", f.FreqRelativeInterval)
		p.num("@freq_recurrence_factor", f.FreqRecurrenceFactor)
	}
	if r := ch.Range; r != nil {
		p.num("@active_start_date", timeToYYYYMMDD(r.StartDate))
		p.num("@active_end_date", scheduleEndDateRaw(r.EndDate))
		p.num("@active_start_time", r.StartTime)
		p.num("@active_end_time", r.EndTime)
	}
	if ch.OwnerLogin != nil {
		p.str("@owner_login_name", *ch.OwnerLogin)
	}
	return p
}

// Alter applies every property set on ch in one sp_update_schedule call. An
// empty ScheduleChanges is a no-op and issues nothing.
//
// The schedule is addressed by @schedule_id where the receiver has one,
// because msdb allows two schedules to share a name and sp_update_schedule
// refuses a @name that matches more than one. A handle from Server.ScheduleRef
// has no ID, so it is addressed by @name instead — which is what makes it
// usable from a ScheduleRef under WithScript, with no read to find the ID.
func (sch *Schedule) Alter(ctx context.Context, ch ScheduleChanges) error {
	p := ch.params()
	if p.empty() {
		return nil
	}
	q := p.statement("sp_update_schedule", sch.key(&p.args, "@name"))
	if _, err := sch.server.exec(ctx, q, p.args.vals...); err != nil {
		return fmt.Errorf("gosmo: alter schedule %q: %w", sch.Name, err)
	}
	setPtrIfApplied(ctx, &sch.Enabled, ch.Enabled)
	if f := ch.Frequency; f != nil {
		setIfApplied(ctx, &sch.FreqType, f.FreqType)
		setIfApplied(ctx, &sch.FreqInterval, f.FreqInterval)
		setIfApplied(ctx, &sch.FreqSubdayType, f.FreqSubdayType)
		setIfApplied(ctx, &sch.FreqSubdayInterval, f.FreqSubdayInterval)
		setIfApplied(ctx, &sch.FreqRelativeInterval, f.FreqRelativeInterval)
		setIfApplied(ctx, &sch.FreqRecurrenceFactor, f.FreqRecurrenceFactor)
	}
	if r := ch.Range; r != nil {
		setIfApplied(ctx, &sch.ActiveStartDate, r.StartDate)
		setIfApplied(ctx, &sch.ActiveEndDate, r.EndDate)
		setIfApplied(ctx, &sch.ActiveStartTime, r.StartTime)
		setIfApplied(ctx, &sch.ActiveEndTime, r.EndTime)
	}
	setPtrIfApplied(ctx, &sch.OwnerLoginName, ch.OwnerLogin)
	setPtrIfApplied(ctx, &sch.Name, ch.Name)
	return nil
}
