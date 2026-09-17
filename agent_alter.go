package gosmo

// agent_alter.go is the batched form of the SQL Server Agent property
// writes: one Changes struct and one Alter method per family, each emitting
// a single sp_update_* call naming only the parameters the caller set.
//
// The per-property setters in agent_job.go, agent_alert.go,
// agent_operator.go and agent_schedule.go stay — they are the right shape
// for a single change, and Enable/Disable/Rename read better than a struct
// literal. What they are not is the right shape for a Properties dialog
// applying five edits at once: msdb's sp_update_* procedures take every
// updatable parameter in one call, and SMO's own shape is
// set-properties-then-Alter(), so five setters is four round trips more
// than the server asked for.
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
// call, in the order they were added. Every sp_* parameter here is a
// literal — msdb takes names, not identifiers, so escapeSingle is the whole
// defense and there is nothing to bracket-quote.
type agentParams struct {
	parts []string
}

// str appends a string parameter as N'...', with its apostrophes doubled.
func (p *agentParams) str(name, v string) {
	p.parts = append(p.parts, fmt.Sprintf("%s = N'%s'", name, escapeSingle(v)))
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
// The fields mirror the per-property setters one for one —
// Name is sp_update_job's @new_name, as Job.Rename sends it.
type JobChanges struct {
	Name        *string
	Description *string
	Category    *string
	OwnerLogin  *string
	Enabled     *bool
	StartStepID *int
	// NotifyLevelEmail and NotifyEmailOperatorName are what
	// Job.SetEmailNotify sets together. SQL Server has no documented "clear
	// to none" value for @notify_email_operator_name, so leave the operator
	// nil to keep the configured one and pair NotifyNever with it to stop
	// the mail — see Job.SetEmailNotify, which also records that a level
	// set on a job with no operator is silently stored as 0.
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
		p.str("@category_name", *ch.Category)
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
func (j *Job) Alter(ch JobChanges) error { return j.AlterContext(context.Background(), ch) }

// AlterContext is the context-aware variant of Alter.
func (j *Job) AlterContext(ctx context.Context, ch JobChanges) error {
	p := ch.params()
	if p.empty() {
		return nil
	}
	q := p.statement("sp_update_job", fmt.Sprintf("@job_name = N'%s'", escapeSingle(j.Name)))
	if err := j.server.execContext(ctx, q); err != nil {
		return fmt.Errorf("gosmo: alter job %q: %w", j.Name, err)
	}
	setPtrIfApplied(ctx, &j.Description, ch.Description)
	setPtrIfApplied(ctx, &j.Category, ch.Category)
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
	// ErrorNumber and Severity are the mutually exclusive triggers
	// Alert.SetTrigger sets together — setting one without sending 0 for
	// the other leaves the other in place, which is how msdb behaves and
	// not what SetTrigger does.
	ErrorNumber           *int
	Severity              *int
	DatabaseName          *string
	DelayBetweenResponses *time.Duration
	NotificationMessage   *string
	// IncludeEventDescriptionIn is msdb's channel bitmask — see the field
	// of the same name on Alert.
	IncludeEventDescriptionIn *int
	// Category set to "" clears the category, sent as the real
	// [Uncategorized] — see Alert.SetCategory for why an empty name will
	// not do.
	Category *string
	// JobName set to "" clears the job response — see Alert.SetJobResponse.
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
		p.str("@category_name", agentCategoryTarget(*ch.Category))
	}
	if ch.JobName != nil {
		p.str("@job_name", *ch.JobName)
	}
	return p
}

// Alter applies every property set on ch in one sp_update_alert call. An
// empty AlertChanges is a no-op and issues nothing.
func (a *Alert) Alter(ch AlertChanges) error { return a.AlterContext(context.Background(), ch) }

// AlterContext is the context-aware variant of Alter.
func (a *Alert) AlterContext(ctx context.Context, ch AlertChanges) error {
	p := ch.params()
	if p.empty() {
		return nil
	}
	q := p.statement("sp_update_alert", fmt.Sprintf("@name = N'%s'", escapeSingle(a.Name)))
	if err := a.server.execContext(ctx, q); err != nil {
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
		setIfApplied(ctx, &a.Category, agentCategoryTarget(*ch.Category))
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
	// [Uncategorized] — see Operator.SetCategory.
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
		p.str("@category_name", agentCategoryTarget(*ch.Category))
	}
	return p
}

// Alter applies every property set on ch in one sp_update_operator call. An
// empty OperatorChanges is a no-op and issues nothing.
func (o *Operator) Alter(ch OperatorChanges) error {
	return o.AlterContext(context.Background(), ch)
}

// AlterContext is the context-aware variant of Alter.
func (o *Operator) AlterContext(ctx context.Context, ch OperatorChanges) error {
	p := ch.params()
	if p.empty() {
		return nil
	}
	q := p.statement("sp_update_operator", fmt.Sprintf("@name = N'%s'", escapeSingle(o.Name)))
	if err := o.server.execContext(ctx, q); err != nil {
		return fmt.Errorf("gosmo: alter operator %q: %w", o.Name, err)
	}
	setPtrIfApplied(ctx, &o.Enabled, ch.Enabled)
	setPtrIfApplied(ctx, &o.EmailAddress, ch.EmailAddress)
	setPtrIfApplied(ctx, &o.PagerAddress, ch.PagerAddress)
	setPtrIfApplied(ctx, &o.NetSendAddress, ch.NetSendAddress)
	if ch.Category != nil {
		setIfApplied(ctx, &o.Category, agentCategoryTarget(*ch.Category))
	}
	setPtrIfApplied(ctx, &o.Name, ch.Name)
	return nil
}

// ============================================================
// Schedule
// ============================================================

// ScheduleActiveRange is the Duration section of a schedule: the date range
// it is active over, plus the daily HHMMSS time-of-day window it may fire
// within. It is what Schedule.SetActiveRange takes as four arguments, in
// the form ScheduleChanges carries. A zero EndDate means "no end date".
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
func (sch *Schedule) Alter(ch ScheduleChanges) error {
	return sch.AlterContext(context.Background(), ch)
}

// AlterContext is the context-aware variant of Alter.
//
// The schedule is addressed by @schedule_id where the receiver has one,
// because msdb allows two schedules to share a name and sp_update_schedule
// refuses a @name that matches more than one. A handle from
// Server.ScheduleRef has no ID, so it is addressed by @name instead — which
// is what makes the batched form usable under WithScript, where the
// per-property setters, keyed on an ID a Ref does not carry, are not.
func (sch *Schedule) AlterContext(ctx context.Context, ch ScheduleChanges) error {
	p := ch.params()
	if p.empty() {
		return nil
	}
	key := fmt.Sprintf("@schedule_id = %d", sch.ID)
	if sch.ID == 0 {
		key = fmt.Sprintf("@name = N'%s'", escapeSingle(sch.Name))
	}
	q := p.statement("sp_update_schedule", key)
	if err := sch.server.execContext(ctx, q); err != nil {
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
