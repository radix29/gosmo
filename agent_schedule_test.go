package gosmo

import (
	"testing"
	"time"
)

func TestYYYYMMDDToTime(t *testing.T) {
	got := yyyymmddToTime(20260115)
	want := time.Date(2026, time.January, 15, 0, 0, 0, 0, time.Local)
	if !got.Equal(want) {
		t.Errorf("yyyymmddToTime(20260115) = %v, want %v", got, want)
	}
	if !yyyymmddToTime(0).IsZero() {
		t.Errorf("yyyymmddToTime(0) should be zero Time")
	}
}

func TestTimeToYYYYMMDD(t *testing.T) {
	d := time.Date(2026, time.July, 22, 14, 0, 0, 0, time.UTC)
	if got := timeToYYYYMMDD(d); got != 20260722 {
		t.Errorf("timeToYYYYMMDD(%v) = %d, want 20260722", d, got)
	}
	if got := timeToYYYYMMDD(time.Time{}); got != 0 {
		t.Errorf("timeToYYYYMMDD(zero) = %d, want 0", got)
	}
}

func TestScheduleEndDate(t *testing.T) {
	if !scheduleEndDate(0).IsZero() {
		t.Errorf("scheduleEndDate(0) should be zero Time")
	}
	if !scheduleEndDate(noEndDateYYYYMMDD).IsZero() {
		t.Errorf("scheduleEndDate(99991231) should be zero Time")
	}
	want := time.Date(2026, time.December, 31, 0, 0, 0, 0, time.Local)
	if got := scheduleEndDate(20261231); !got.Equal(want) {
		t.Errorf("scheduleEndDate(20261231) = %v, want %v", got, want)
	}
}

func TestScheduleEndDateRaw(t *testing.T) {
	if got := scheduleEndDateRaw(time.Time{}); got != noEndDateYYYYMMDD {
		t.Errorf("scheduleEndDateRaw(zero) = %d, want %d", got, noEndDateYYYYMMDD)
	}
	d := time.Date(2026, time.March, 1, 0, 0, 0, 0, time.UTC)
	if got := scheduleEndDateRaw(d); got != 20260301 {
		t.Errorf("scheduleEndDateRaw(%v) = %d, want 20260301", d, got)
	}
}

func TestFormatHHMMSS(t *testing.T) {
	cases := []struct {
		n    int
		want string
	}{
		{0, "00:00:00"},
		{10000, "01:00:00"},
		{93015, "09:30:15"},
		{235959, "23:59:59"},
	}
	for _, c := range cases {
		if got := formatHHMMSS(c.n); got != c.want {
			t.Errorf("formatHHMMSS(%d) = %q, want %q", c.n, got, c.want)
		}
	}
}

func TestScheduleDescription(t *testing.T) {
	start := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)

	cases := []struct {
		name string
		sch  *Schedule
		want string
	}{
		{
			name: "daily once no end date",
			sch: &Schedule{
				FreqType:        FreqDaily,
				FreqInterval:    1,
				FreqSubdayType:  SubdayOnce,
				ActiveStartTime: 10000,
				ActiveStartDate: start,
			},
			want: "Occurs every day at 01:00:00. Schedule is active from 2026-01-01.",
		},
		{
			name: "daily every N days",
			sch: &Schedule{
				FreqType:        FreqDaily,
				FreqInterval:    3,
				FreqSubdayType:  SubdayOnce,
				ActiveStartTime: 30000,
				ActiveStartDate: start,
			},
			want: "Occurs every 3 days at 03:00:00. Schedule is active from 2026-01-01.",
		},
		{
			name: "daily with subday minutes window and end date",
			sch: &Schedule{
				FreqType:           FreqDaily,
				FreqInterval:       1,
				FreqSubdayType:     SubdayMinutes,
				FreqSubdayInterval: 15,
				ActiveStartTime:    0,
				ActiveEndTime:      235900,
				ActiveStartDate:    start,
				ActiveEndDate:      time.Date(2026, time.December, 31, 0, 0, 0, 0, time.UTC),
			},
			want: "Occurs every day every 15 minute(s) between 00:00:00 and 23:59:00. Schedule is active from 2026-01-01 to 2026-12-31.",
		},
		{
			name: "weekly single day recurrence factor 1",
			sch: &Schedule{
				FreqType:             FreqWeekly,
				FreqInterval:         WeekdayMonday,
				FreqRecurrenceFactor: 1,
				FreqSubdayType:       SubdayOnce,
				ActiveStartTime:      93000,
				ActiveStartDate:      start,
			},
			want: "Occurs every week on Monday at 09:30:00. Schedule is active from 2026-01-01.",
		},
		{
			name: "weekly multiple days every 2 weeks",
			sch: &Schedule{
				FreqType:             FreqWeekly,
				FreqInterval:         WeekdayMonday | WeekdayWednesday | WeekdayFriday,
				FreqRecurrenceFactor: 2,
				FreqSubdayType:       SubdayOnce,
				ActiveStartTime:      10000,
				ActiveStartDate:      start,
			},
			want: "Occurs every 2 weeks on Monday, Wednesday, Friday at 01:00:00. Schedule is active from 2026-01-01.",
		},
		{
			name: "monthly day of month",
			sch: &Schedule{
				FreqType:             FreqMonthly,
				FreqInterval:         15,
				FreqRecurrenceFactor: 1,
				FreqSubdayType:       SubdayOnce,
				ActiveStartTime:      10000,
				ActiveStartDate:      start,
			},
			want: "Occurs on day 15 of every month at 01:00:00. Schedule is active from 2026-01-01.",
		},
		{
			name: "monthly relative first Sunday",
			sch: &Schedule{
				FreqType:             FreqMonthlyRelative,
				FreqInterval:         RelativeDaySunday,
				FreqRelativeInterval: RelativeFirst,
				FreqRecurrenceFactor: 1,
				FreqSubdayType:       SubdayOnce,
				ActiveStartTime:      10000,
				ActiveStartDate:      start,
			},
			want: "Occurs the first Sunday of every month at 01:00:00. Schedule is active from 2026-01-01.",
		},
		{
			name: "monthly relative last day",
			sch: &Schedule{
				FreqType:             FreqMonthlyRelative,
				FreqInterval:         RelativeDayDay,
				FreqRelativeInterval: RelativeLast,
				FreqRecurrenceFactor: 1,
				FreqSubdayType:       SubdayOnce,
				ActiveStartTime:      10000,
				ActiveStartDate:      start,
			},
			want: "Occurs the last day of every month at 01:00:00. Schedule is active from 2026-01-01.",
		},
		{
			name: "monthly relative every 2 months, weekday",
			sch: &Schedule{
				FreqType:             FreqMonthlyRelative,
				FreqInterval:         RelativeDayWeekday,
				FreqRelativeInterval: RelativeFourth,
				FreqRecurrenceFactor: 2,
				FreqSubdayType:       SubdayOnce,
				ActiveStartTime:      10000,
				ActiveStartDate:      start,
			},
			want: "Occurs the fourth weekday of every 2 months at 01:00:00. Schedule is active from 2026-01-01.",
		},
		{
			name: "once",
			sch: &Schedule{
				FreqType:        FreqOnce,
				ActiveStartDate: start,
				ActiveStartTime: 143000,
			},
			want: "Occurs once on 2026-01-01 at 14:30:00.",
		},
		{
			name: "auto start",
			sch: &Schedule{
				FreqType: FreqAutoStart,
			},
			want: "Starts automatically when SQL Server Agent starts.",
		},
		{
			name: "on idle",
			sch: &Schedule{
				FreqType: FreqOnIdle,
			},
			want: "Starts whenever the CPU becomes idle.",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.sch.Description(); got != c.want {
				t.Errorf("Description() = %q, want %q", got, c.want)
			}
		})
	}
}

func TestWeekdayListText(t *testing.T) {
	cases := []struct {
		mask int
		want string
	}{
		{0, "no days"},
		{WeekdaySunday, "Sunday"},
		{WeekdayMonday | WeekdayWednesday | WeekdayFriday, "Monday, Wednesday, Friday"},
		{WeekdaySunday | WeekdayMonday | WeekdayTuesday | WeekdayWednesday | WeekdayThursday | WeekdayFriday | WeekdaySaturday,
			"Sunday, Monday, Tuesday, Wednesday, Thursday, Friday, Saturday"},
	}
	for _, c := range cases {
		if got := weekdayListText(c.mask); got != c.want {
			t.Errorf("weekdayListText(%d) = %q, want %q", c.mask, got, c.want)
		}
	}
}

func TestWeekdayName(t *testing.T) {
	cases := []struct {
		code int
		want string
	}{
		{RelativeDaySunday, "Sunday"},
		{RelativeDaySaturday, "Saturday"},
		{RelativeDayDay, "day"},
		{RelativeDayWeekday, "weekday"},
		{RelativeDayWeekendDay, "weekend day"},
	}
	for _, c := range cases {
		if got := weekdayName(c.code); got != c.want {
			t.Errorf("weekdayName(%d) = %q, want %q", c.code, got, c.want)
		}
	}
}

func TestRelativeText(t *testing.T) {
	cases := []struct {
		n    int
		want string
	}{
		{RelativeFirst, "first"},
		{RelativeSecond, "second"},
		{RelativeThird, "third"},
		{RelativeFourth, "fourth"},
		{RelativeLast, "last"},
	}
	for _, c := range cases {
		if got := relativeText(c.n); got != c.want {
			t.Errorf("relativeText(%d) = %q, want %q", c.n, got, c.want)
		}
	}
}

func TestEveryNText(t *testing.T) {
	cases := []struct {
		n    int
		unit string
		want string
	}{
		{0, "day", "every day"},
		{1, "day", "every day"},
		{2, "day", "every 2 days"},
		{3, "week", "every 3 weeks"},
	}
	for _, c := range cases {
		if got := everyNText(c.n, c.unit); got != c.want {
			t.Errorf("everyNText(%d, %q) = %q, want %q", c.n, c.unit, got, c.want)
		}
	}
}
