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
// SQL Server Agent -- Schedules
// ============================================================

// ScheduleFreqType mirrors sysschedules.freq_type.
type ScheduleFreqType int

const (
	FreqOnce            ScheduleFreqType = 1
	FreqDaily           ScheduleFreqType = 4
	FreqWeekly          ScheduleFreqType = 8
	FreqMonthly         ScheduleFreqType = 16
	FreqMonthlyRelative ScheduleFreqType = 32
	FreqAutoStart       ScheduleFreqType = 64
	FreqOnIdle          ScheduleFreqType = 128
)

// ScheduleSubdayType mirrors sysschedules.freq_subday_type.
type ScheduleSubdayType int

const (
	SubdayOnce    ScheduleSubdayType = 1
	SubdaySeconds ScheduleSubdayType = 2
	SubdayMinutes ScheduleSubdayType = 4
	SubdayHours   ScheduleSubdayType = 8
)

// Weekday bitmask values for a FreqWeekly schedule's FreqInterval.
const (
	WeekdaySunday    = 1
	WeekdayMonday    = 2
	WeekdayTuesday   = 4
	WeekdayWednesday = 8
	WeekdayThursday  = 16
	WeekdayFriday    = 32
	WeekdaySaturday  = 64
)

// Single-value day codes for a FreqMonthlyRelative schedule's FreqInterval.
// Unlike FreqWeekly's bitmask above, these are sequential (1=Sunday through
// 7=Saturday), not powers of two, plus three special values.
const (
	RelativeDaySunday     = 1
	RelativeDayMonday     = 2
	RelativeDayTuesday    = 3
	RelativeDayWednesday  = 4
	RelativeDayThursday   = 5
	RelativeDayFriday     = 6
	RelativeDaySaturday   = 7
	RelativeDayDay        = 8
	RelativeDayWeekday    = 9
	RelativeDayWeekendDay = 10
)

// FreqRelativeInterval values for a FreqMonthlyRelative schedule.
const (
	RelativeFirst  = 1
	RelativeSecond = 2
	RelativeThird  = 4
	RelativeFourth = 8
	RelativeLast   = 16
)

// noEndDateYYYYMMDD is the sysschedules.active_end_date sentinel SQL Server
// Agent uses to mean "no end date".
const noEndDateYYYYMMDD = 99991231

// Schedule represents a shared SQL Server Agent schedule
// (msdb.dbo.sysschedules), which may be attached to more than one job.
type Schedule struct {
	server               *Server
	ID                   int
	Name                 string
	Enabled              bool
	FreqType             ScheduleFreqType
	FreqInterval         int
	FreqSubdayType       ScheduleSubdayType
	FreqSubdayInterval   int
	FreqRelativeInterval int
	FreqRecurrenceFactor int
	ActiveStartDate      time.Time
	// ActiveEndDate is the zero Time when the schedule has no end date.
	ActiveEndDate time.Time
	// ActiveStartTime and ActiveEndTime are HHMMSS integers, e.g. 10000 = 01:00:00.
	ActiveStartTime int
	ActiveEndTime   int
	OwnerLoginName  string
	CreateDate      time.Time
	ModifyDate      time.Time
}

const scheduleColumns = `sch.schedule_id, sch.name, sch.enabled, sch.freq_type, sch.freq_interval,
       sch.freq_subday_type, sch.freq_subday_interval, sch.freq_relative_interval,
       sch.freq_recurrence_factor, sch.active_start_date, sch.active_end_date,
       sch.active_start_time, sch.active_end_time,
       sch.date_created, sch.date_modified, ISNULL(l.name, '')`

const scheduleFrom = `FROM   msdb.dbo.sysschedules sch
LEFT   JOIN master.sys.server_principals l ON l.sid = sch.owner_sid`

// scanSchedule scans one row shaped like scheduleColumns into a new Schedule.
func scanSchedule(s *Server, scan func(dest ...any) error) (*Schedule, error) {
	sch := &Schedule{server: s}
	var startDate, endDate int
	if err := scan(
		&sch.ID, &sch.Name, &sch.Enabled, &sch.FreqType, &sch.FreqInterval,
		&sch.FreqSubdayType, &sch.FreqSubdayInterval, &sch.FreqRelativeInterval,
		&sch.FreqRecurrenceFactor, &startDate, &endDate,
		&sch.ActiveStartTime, &sch.ActiveEndTime,
		&sch.CreateDate, &sch.ModifyDate, &sch.OwnerLoginName,
	); err != nil {
		return nil, err
	}
	sch.ActiveStartDate = yyyymmddToTime(startDate)
	sch.ActiveEndDate = scheduleEndDate(endDate)
	return sch, nil
}

// Schedules returns every SQL Server Agent schedule defined on the server.
func (s *Server) Schedules() ([]*Schedule, error) { return s.SchedulesContext(context.Background()) }

// SchedulesContext is the context-aware variant of Schedules.
func (s *Server) SchedulesContext(ctx context.Context) ([]*Schedule, error) {
	q := "SELECT " + scheduleColumns + " " + scheduleFrom + " ORDER BY sch.name"

	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("gosmo: list schedules: %w", err)
	}
	defer rows.Close()

	var out []*Schedule
	for rows.Next() {
		sch, err := scanSchedule(s, rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, sch)
	}
	return out, rows.Err()
}

// ScheduleByName returns a single schedule by name.
func (s *Server) ScheduleByName(name string) (*Schedule, error) {
	return s.ScheduleByNameContext(context.Background(), name)
}

// ScheduleByNameContext is the context-aware variant of ScheduleByName.
func (s *Server) ScheduleByNameContext(ctx context.Context, name string) (*Schedule, error) {
	q := "SELECT " + scheduleColumns + " " + scheduleFrom + " WHERE sch.name = @p1"

	row := s.db.QueryRowContext(ctx, q, name)
	sch, err := scanSchedule(s, row.Scan)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("gosmo: schedule %q not found", name)
		}
		return nil, fmt.Errorf("gosmo: schedule by name: %w", err)
	}
	return sch, nil
}

// CreateScheduleRequest describes a new shared schedule.
type CreateScheduleRequest struct {
	Name                 string
	Enabled              bool
	FreqType             ScheduleFreqType
	FreqInterval         int
	FreqSubdayType       ScheduleSubdayType
	FreqSubdayInterval   int
	FreqRelativeInterval int
	FreqRecurrenceFactor int
	// ActiveStartDate defaults to today if the zero Time.
	ActiveStartDate time.Time
	// ActiveEndDate means "no end date" if the zero Time.
	ActiveEndDate time.Time
	// ActiveStartTime and ActiveEndTime are HHMMSS integers.
	ActiveStartTime int
	ActiveEndTime   int
	OwnerLoginName  string
}

// CreateSchedule creates a new shared schedule via sp_add_schedule. The
// returned Schedule is not yet attached to any job — see Job.AttachSchedule.
func (s *Server) CreateSchedule(req CreateScheduleRequest) (*Schedule, error) {
	return s.CreateScheduleContext(context.Background(), req)
}

// CreateScheduleContext is the context-aware variant of CreateSchedule.
func (s *Server) CreateScheduleContext(ctx context.Context, req CreateScheduleRequest) (*Schedule, error) {
	if req.Name == "" {
		return nil, fmt.Errorf("gosmo: create schedule: name is required")
	}
	startDate := timeToYYYYMMDD(req.ActiveStartDate)
	if startDate == 0 {
		startDate = timeToYYYYMMDD(time.Now())
	}
	q := fmt.Sprintf(
		"EXEC msdb.dbo.sp_add_schedule @schedule_name = N'%s', @enabled = %d, "+
			"@freq_type = %d, @freq_interval = %d, "+
			"@freq_subday_type = %d, @freq_subday_interval = %d, "+
			"@freq_relative_interval = %d, @freq_recurrence_factor = %d, "+
			"@active_start_date = %d, @active_end_date = %d, "+
			"@active_start_time = %d, @active_end_time = %d",
		escapeSingle(req.Name), boolToInt(req.Enabled),
		int(req.FreqType), req.FreqInterval,
		int(req.FreqSubdayType), req.FreqSubdayInterval,
		req.FreqRelativeInterval, req.FreqRecurrenceFactor,
		startDate, scheduleEndDateRaw(req.ActiveEndDate),
		req.ActiveStartTime, req.ActiveEndTime,
	)
	if req.OwnerLoginName != "" {
		q += fmt.Sprintf(", @owner_login_name = N'%s'", escapeSingle(req.OwnerLoginName))
	}
	if err := s.execContext(ctx, q); err != nil {
		return nil, fmt.Errorf("gosmo: create schedule %q: %w", req.Name, err)
	}
	return s.ScheduleByNameContext(ctx, req.Name)
}

// Rename changes the schedule's name.
func (sch *Schedule) Rename(newName string) error {
	return sch.RenameContext(context.Background(), newName)
}

// RenameContext is the context-aware variant of Rename.
func (sch *Schedule) RenameContext(ctx context.Context, newName string) error {
	q := fmt.Sprintf("EXEC msdb.dbo.sp_update_schedule @schedule_id = %d, @new_name = N'%s'",
		sch.ID, escapeSingle(newName))
	if err := sch.server.execContext(ctx, q); err != nil {
		return fmt.Errorf("gosmo: rename schedule %q to %q: %w", sch.Name, newName, err)
	}
	sch.Name = newName
	return nil
}

// Enable enables the schedule.
func (sch *Schedule) Enable() error { return sch.EnableContext(context.Background()) }

// EnableContext is the context-aware variant of Enable.
func (sch *Schedule) EnableContext(ctx context.Context) error { return sch.setEnabled(ctx, true) }

// Disable disables the schedule.
func (sch *Schedule) Disable() error { return sch.DisableContext(context.Background()) }

// DisableContext is the context-aware variant of Disable.
func (sch *Schedule) DisableContext(ctx context.Context) error { return sch.setEnabled(ctx, false) }

func (sch *Schedule) setEnabled(ctx context.Context, on bool) error {
	q := fmt.Sprintf("EXEC msdb.dbo.sp_update_schedule @schedule_id = %d, @enabled = %d",
		sch.ID, boolToInt(on))
	if err := sch.server.execContext(ctx, q); err != nil {
		return fmt.Errorf("gosmo: set enabled=%v for schedule %q: %w", on, sch.Name, err)
	}
	sch.Enabled = on
	return nil
}

// ScheduleFrequency bundles the freq_type cluster of sp_update_schedule's
// parameters — freq_interval's meaning depends on freq_type, so these are
// only meaningful set together, matching how SSMS's own Schedule Properties
// dialog submits its whole Frequency section as one unit.
type ScheduleFrequency struct {
	FreqType             ScheduleFreqType
	FreqInterval         int
	FreqSubdayType       ScheduleSubdayType
	FreqSubdayInterval   int
	FreqRelativeInterval int
	FreqRecurrenceFactor int
}

// SetFrequency replaces the schedule's frequency definition.
func (sch *Schedule) SetFrequency(f ScheduleFrequency) error {
	return sch.SetFrequencyContext(context.Background(), f)
}

// SetFrequencyContext is the context-aware variant of SetFrequency.
func (sch *Schedule) SetFrequencyContext(ctx context.Context, f ScheduleFrequency) error {
	q := fmt.Sprintf(
		"EXEC msdb.dbo.sp_update_schedule @schedule_id = %d, "+
			"@freq_type = %d, @freq_interval = %d, "+
			"@freq_subday_type = %d, @freq_subday_interval = %d, "+
			"@freq_relative_interval = %d, @freq_recurrence_factor = %d",
		sch.ID,
		int(f.FreqType), f.FreqInterval,
		int(f.FreqSubdayType), f.FreqSubdayInterval,
		f.FreqRelativeInterval, f.FreqRecurrenceFactor,
	)
	if err := sch.server.execContext(ctx, q); err != nil {
		return fmt.Errorf("gosmo: set frequency for schedule %q: %w", sch.Name, err)
	}
	sch.FreqType, sch.FreqInterval = f.FreqType, f.FreqInterval
	sch.FreqSubdayType, sch.FreqSubdayInterval = f.FreqSubdayType, f.FreqSubdayInterval
	sch.FreqRelativeInterval, sch.FreqRecurrenceFactor = f.FreqRelativeInterval, f.FreqRecurrenceFactor
	return nil
}

// SetActiveRange changes the schedule's Duration section: the date range
// it's active over, plus the daily time-of-day window (HHMMSS) it can fire
// within. A zero endDate means "no end date".
func (sch *Schedule) SetActiveRange(startDate, endDate time.Time, startTime, endTime int) error {
	return sch.SetActiveRangeContext(context.Background(), startDate, endDate, startTime, endTime)
}

// SetActiveRangeContext is the context-aware variant of SetActiveRange.
func (sch *Schedule) SetActiveRangeContext(ctx context.Context, startDate, endDate time.Time, startTime, endTime int) error {
	q := fmt.Sprintf(
		"EXEC msdb.dbo.sp_update_schedule @schedule_id = %d, "+
			"@active_start_date = %d, @active_end_date = %d, "+
			"@active_start_time = %d, @active_end_time = %d",
		sch.ID,
		timeToYYYYMMDD(startDate), scheduleEndDateRaw(endDate),
		startTime, endTime,
	)
	if err := sch.server.execContext(ctx, q); err != nil {
		return fmt.Errorf("gosmo: set active range for schedule %q: %w", sch.Name, err)
	}
	sch.ActiveStartDate, sch.ActiveEndDate = startDate, endDate
	sch.ActiveStartTime, sch.ActiveEndTime = startTime, endTime
	return nil
}

// SetOwner reassigns the schedule's owner login.
func (sch *Schedule) SetOwner(loginName string) error {
	return sch.SetOwnerContext(context.Background(), loginName)
}

// SetOwnerContext is the context-aware variant of SetOwner.
func (sch *Schedule) SetOwnerContext(ctx context.Context, loginName string) error {
	q := fmt.Sprintf("EXEC msdb.dbo.sp_update_schedule @schedule_id = %d, @owner_login_name = N'%s'",
		sch.ID, escapeSingle(loginName))
	if err := sch.server.execContext(ctx, q); err != nil {
		return fmt.Errorf("gosmo: set owner for schedule %q: %w", sch.Name, err)
	}
	sch.OwnerLoginName = loginName
	return nil
}

// Drop deletes the schedule via sp_delete_schedule. SQL Server refuses the
// call (returning a wrapped SQLError) if the schedule is still attached to
// one or more jobs — detach it first via Job.DetachSchedule.
func (sch *Schedule) Drop() error { return sch.DropContext(context.Background()) }

// DropContext is the context-aware variant of Drop.
func (sch *Schedule) DropContext(ctx context.Context) error {
	q := fmt.Sprintf("EXEC msdb.dbo.sp_delete_schedule @schedule_id = %d", sch.ID)
	if err := sch.server.execContext(ctx, q); err != nil {
		return fmt.Errorf("gosmo: drop schedule %q: %w", sch.Name, err)
	}
	return nil
}

// Jobs returns every job this schedule is attached to — a "referenced by"
// list. Only JobID/Name/IsEnabled are populated on each returned Job (not
// its activity/history joins), enough for a reference list without the
// extra round trips Server.Jobs pays for.
func (sch *Schedule) Jobs() ([]*Job, error) { return sch.JobsContext(context.Background()) }

// JobsContext is the context-aware variant of Jobs.
func (sch *Schedule) JobsContext(ctx context.Context) ([]*Job, error) {
	const q = `
SELECT CONVERT(varchar(36), j.job_id), j.name, j.enabled
FROM   msdb.dbo.sysjobs j
JOIN   msdb.dbo.sysjobschedules js ON js.job_id = j.job_id
WHERE  js.schedule_id = @p1
ORDER  BY j.name`

	rows, err := sch.server.db.QueryContext(ctx, q, sch.ID)
	if err != nil {
		return nil, fmt.Errorf("gosmo: jobs for schedule %q: %w", sch.Name, err)
	}
	defer rows.Close()

	var out []*Job
	for rows.Next() {
		j := &Job{server: sch.server}
		if err := rows.Scan(&j.JobID, &j.Name, &j.IsEnabled); err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// Schedules returns every schedule attached to the job.
func (j *Job) Schedules() ([]*Schedule, error) { return j.SchedulesContext(context.Background()) }

// SchedulesContext is the context-aware variant of Schedules.
func (j *Job) SchedulesContext(ctx context.Context) ([]*Schedule, error) {
	q := "SELECT " + scheduleColumns + " " + scheduleFrom + `
JOIN   msdb.dbo.sysjobschedules js ON js.schedule_id = sch.schedule_id
WHERE  js.job_id = @p1
ORDER  BY sch.name`

	rows, err := j.server.db.QueryContext(ctx, q, j.JobID)
	if err != nil {
		return nil, fmt.Errorf("gosmo: schedules for job %q: %w", j.Name, err)
	}
	defer rows.Close()

	var out []*Schedule
	for rows.Next() {
		sch, err := scanSchedule(j.server, rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, sch)
	}
	return out, rows.Err()
}

// AttachSchedule attaches an existing shared schedule to the job — as
// opposed to AddSchedule, which creates a brand-new schedule and attaches
// it in one step.
func (j *Job) AttachSchedule(scheduleName string) error {
	return j.AttachScheduleContext(context.Background(), scheduleName)
}

// AttachScheduleContext is the context-aware variant of AttachSchedule.
func (j *Job) AttachScheduleContext(ctx context.Context, scheduleName string) error {
	q := fmt.Sprintf("EXEC msdb.dbo.sp_attach_schedule @job_name = N'%s', @schedule_name = N'%s'",
		escapeSingle(j.Name), escapeSingle(scheduleName))
	if err := j.server.execContext(ctx, q); err != nil {
		return fmt.Errorf("gosmo: attach schedule %q to job %q: %w", scheduleName, j.Name, err)
	}
	return nil
}

// DetachSchedule detaches a schedule from the job without deleting the
// schedule itself (it may still be shared by other jobs).
func (j *Job) DetachSchedule(scheduleName string) error {
	return j.DetachScheduleContext(context.Background(), scheduleName)
}

// DetachScheduleContext is the context-aware variant of DetachSchedule.
func (j *Job) DetachScheduleContext(ctx context.Context, scheduleName string) error {
	q := fmt.Sprintf("EXEC msdb.dbo.sp_detach_schedule @job_name = N'%s', @schedule_name = N'%s'",
		escapeSingle(j.Name), escapeSingle(scheduleName))
	if err := j.server.execContext(ctx, q); err != nil {
		return fmt.Errorf("gosmo: detach schedule %q from job %q: %w", scheduleName, j.Name, err)
	}
	return nil
}

// Description renders the schedule's frequency and active-date range as a
// human-readable summary, the same shape SSMS shows at the bottom of its
// own Schedule Properties dialog, e.g. "Occurs every day at 01:00:00.
// Schedule is active from 2026-01-01."
func (sch *Schedule) Description() string {
	freq := sch.frequencyText()
	rangeText := sch.activeRangeText()
	if rangeText == "" {
		return freq + "."
	}
	return freq + ". " + rangeText + "."
}

func (sch *Schedule) frequencyText() string {
	switch sch.FreqType {
	case FreqOnce:
		return fmt.Sprintf("Occurs once on %s at %s", sch.ActiveStartDate.Format("2006-01-02"), formatHHMMSS(sch.ActiveStartTime))
	case FreqAutoStart:
		return "Starts automatically when SQL Server Agent starts"
	case FreqOnIdle:
		return "Starts whenever the CPU becomes idle"
	case FreqDaily:
		return fmt.Sprintf("Occurs %s %s", everyNText(sch.FreqInterval, "day"), sch.subdayText())
	case FreqWeekly:
		days := weekdayListText(sch.FreqInterval)
		return fmt.Sprintf("Occurs %s on %s %s", everyNText(sch.FreqRecurrenceFactor, "week"), days, sch.subdayText())
	case FreqMonthly:
		return fmt.Sprintf("Occurs on day %d of %s %s", sch.FreqInterval, everyNText(sch.FreqRecurrenceFactor, "month"), sch.subdayText())
	case FreqMonthlyRelative:
		rel := relativeText(sch.FreqRelativeInterval)
		day := weekdayName(sch.FreqInterval)
		return fmt.Sprintf("Occurs the %s %s of %s %s", rel, day, everyNText(sch.FreqRecurrenceFactor, "month"), sch.subdayText())
	default:
		return "Occurs on an unspecified schedule"
	}
}

func (sch *Schedule) subdayText() string {
	switch sch.FreqSubdayType {
	case SubdayMinutes:
		return fmt.Sprintf("every %d minute(s) between %s and %s", sch.FreqSubdayInterval, formatHHMMSS(sch.ActiveStartTime), formatHHMMSS(sch.ActiveEndTime))
	case SubdayHours:
		return fmt.Sprintf("every %d hour(s) between %s and %s", sch.FreqSubdayInterval, formatHHMMSS(sch.ActiveStartTime), formatHHMMSS(sch.ActiveEndTime))
	case SubdaySeconds:
		return fmt.Sprintf("every %d second(s) between %s and %s", sch.FreqSubdayInterval, formatHHMMSS(sch.ActiveStartTime), formatHHMMSS(sch.ActiveEndTime))
	default: // SubdayOnce, or unset
		return "at " + formatHHMMSS(sch.ActiveStartTime)
	}
}

func (sch *Schedule) activeRangeText() string {
	// FreqOnce's single trigger date is already stated in frequencyText;
	// FreqAutoStart/FreqOnIdle have no date range at all — SSMS's own
	// summary line omits the "active from" clause for all three.
	if sch.FreqType == FreqOnce || sch.FreqType == FreqAutoStart || sch.FreqType == FreqOnIdle {
		return ""
	}
	start := sch.ActiveStartDate.Format("2006-01-02")
	if sch.ActiveEndDate.IsZero() {
		return fmt.Sprintf("Schedule is active from %s", start)
	}
	return fmt.Sprintf("Schedule is active from %s to %s", start, sch.ActiveEndDate.Format("2006-01-02"))
}

// everyNText renders a recurrence factor as "every day"/"every 3 days".
func everyNText(n int, unit string) string {
	if n <= 1 {
		return "every " + unit
	}
	return fmt.Sprintf("every %d %ss", n, unit)
}

// weekdayListText renders a FreqWeekly weekday bitmask as "Monday,
// Wednesday, Friday".
func weekdayListText(mask int) string {
	names := []struct {
		bit  int
		name string
	}{
		{WeekdaySunday, "Sunday"}, {WeekdayMonday, "Monday"}, {WeekdayTuesday, "Tuesday"},
		{WeekdayWednesday, "Wednesday"}, {WeekdayThursday, "Thursday"}, {WeekdayFriday, "Friday"},
		{WeekdaySaturday, "Saturday"},
	}
	var days []string
	for _, wd := range names {
		if mask&wd.bit != 0 {
			days = append(days, wd.name)
		}
	}
	if len(days) == 0 {
		return "no days"
	}
	return strings.Join(days, ", ")
}

// weekdayName renders a single FreqMonthlyRelative day code (sequential
// 1=Sunday..7=Saturday, plus the three RelativeDayDay/Weekday/WeekendDay
// special values — not FreqWeekly's bitmask).
func weekdayName(code int) string {
	switch code {
	case RelativeDaySunday:
		return "Sunday"
	case RelativeDayMonday:
		return "Monday"
	case RelativeDayTuesday:
		return "Tuesday"
	case RelativeDayWednesday:
		return "Wednesday"
	case RelativeDayThursday:
		return "Thursday"
	case RelativeDayFriday:
		return "Friday"
	case RelativeDaySaturday:
		return "Saturday"
	case RelativeDayDay:
		return "day"
	case RelativeDayWeekday:
		return "weekday"
	case RelativeDayWeekendDay:
		return "weekend day"
	default:
		return "day"
	}
}

// relativeText renders a FreqMonthlyRelative FreqRelativeInterval value.
func relativeText(n int) string {
	switch n {
	case RelativeFirst:
		return "first"
	case RelativeSecond:
		return "second"
	case RelativeThird:
		return "third"
	case RelativeFourth:
		return "fourth"
	case RelativeLast:
		return "last"
	default:
		return "first"
	}
}

// ============================================================
// Date/time helpers for sysschedules integer columns
// ============================================================

// yyyymmddToTime converts msdb's YYYYMMDD-encoded integer date columns to
// time.Time. 0 maps to the zero Time.
func yyyymmddToTime(n int) time.Time {
	if n == 0 {
		return time.Time{}
	}
	return parseSQLAgentDate(n, 0)
}

// timeToYYYYMMDD is the inverse of yyyymmddToTime.
func timeToYYYYMMDD(t time.Time) int {
	if t.IsZero() {
		return 0
	}
	return t.Year()*10000 + int(t.Month())*100 + t.Day()
}

// scheduleEndDate converts a raw sysschedules.active_end_date value to
// time.Time, treating both 0 and the noEndDateYYYYMMDD sentinel as "no end
// date" (the zero Time).
func scheduleEndDate(raw int) time.Time {
	if raw == 0 || raw == noEndDateYYYYMMDD {
		return time.Time{}
	}
	return yyyymmddToTime(raw)
}

// scheduleEndDateRaw is the inverse of scheduleEndDate: the zero Time
// becomes the noEndDateYYYYMMDD sentinel.
func scheduleEndDateRaw(t time.Time) int {
	if t.IsZero() {
		return noEndDateYYYYMMDD
	}
	return timeToYYYYMMDD(t)
}

// formatHHMMSS renders an msdb HHMMSS-encoded integer time-of-day column
// (e.g. sysschedules.active_start_time) as "HH:MM:SS".
func formatHHMMSS(n int) string {
	h, m, s := n/10000, (n%10000)/100, n%100
	return fmt.Sprintf("%02d:%02d:%02d", h, m, s)
}
