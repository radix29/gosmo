//go:build livedb

package gosmo

import (
	"context"
	"testing"
	"time"
)

// TestLiveJobAddScheduleRecurrences pins T23 against msdb: Job.AddSchedule
// takes a CreateScheduleRequest, so a weekly and a relative-monthly schedule
// — which sp_verify_schedule refuses without a recurrence factor (Msg 14278),
// and the old request could not carry one — are created, read back with
// every field, and an owner is applied in the same batch. A refused owner
// leaves no schedule behind.
//
//	go test -tags livedb . -run TestLiveJobAddScheduleRecurrences -v -livedb '...'
func TestLiveJobAddScheduleRecurrences(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()
	srv := &Server{db: db}

	const (
		jobName = "gosmo_live_t23_job"
		owner   = "gosmo_live_t23_owner"
	)
	drop := func() {
		bg := context.Background()
		srv.exec(bg, "IF EXISTS (SELECT 1 FROM msdb.dbo.sysjobs WHERE name = N'"+jobName+"') "+
			"EXEC msdb.dbo.sp_delete_job @job_name = N'"+jobName+"', @delete_unused_schedule = 1")
		srv.exec(bg, "IF SUSER_ID(N'"+owner+"') IS NOT NULL DROP LOGIN ["+owner+"]")
	}
	drop()
	// Deferred, not t.Cleanup: a cleanup runs after the deferred done() has
	// closed the pool, and the drop then fails silently.
	defer drop()

	if err := srv.exec(ctx, "CREATE LOGIN ["+owner+"] WITH PASSWORD = N'Gosmo!T23-"+time.Now().Format("150405")+"', CHECK_POLICY = OFF"); err != nil {
		t.Fatalf("create owner login: %v", err)
	}
	j, err := srv.CreateJob(ctx, CreateJobRequest{Name: jobName})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	start := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	end := time.Date(2027, 3, 31, 0, 0, 0, 0, time.UTC)

	weekly, err := j.AddSchedule(ctx, CreateScheduleRequest{
		Name: "t23 weekly", Enabled: true,
		FreqType: FreqWeekly, FreqInterval: WeekdayMonday | WeekdayThursday, FreqRecurrenceFactor: 2,
		FreqSubdayType: SubdayOnce, ActiveStartTime: 23000,
		ActiveStartDate: start, ActiveEndDate: end,
	})
	if err != nil {
		t.Fatalf("AddSchedule weekly: %v", err)
	}
	if weekly.ID == 0 || weekly.FreqType != FreqWeekly || weekly.FreqInterval != WeekdayMonday|WeekdayThursday ||
		weekly.FreqRecurrenceFactor != 2 || weekly.ActiveStartTime != 23000 ||
		!weekly.ActiveStartDate.Equal(start) || !weekly.ActiveEndDate.Equal(end) {
		t.Errorf("weekly schedule read back as %+v", weekly)
	}

	monthly, err := j.AddSchedule(ctx, CreateScheduleRequest{
		Name: "t23 monthly", Enabled: true,
		FreqType: FreqMonthlyRelative, FreqInterval: RelativeDayFriday, FreqRelativeInterval: 16, // last
		FreqRecurrenceFactor: 1, FreqSubdayType: SubdayHours, FreqSubdayInterval: 6,
		OwnerLoginName: owner,
	})
	if err != nil {
		t.Fatalf("AddSchedule monthly relative with owner: %v", err)
	}
	if monthly.FreqType != FreqMonthlyRelative || monthly.FreqRelativeInterval != 16 ||
		monthly.FreqSubdayType != SubdayHours || monthly.FreqSubdayInterval != 6 {
		t.Errorf("monthly schedule read back as %+v", monthly)
	}
	if monthly.OwnerLoginName != owner {
		t.Errorf("owner = %q, want %q", monthly.OwnerLoginName, owner)
	}

	if _, err := j.AddSchedule(ctx, CreateScheduleRequest{
		Name: "t23 refused", Enabled: true, FreqType: FreqDaily, FreqInterval: 1, FreqSubdayType: SubdayOnce,
		OwnerLoginName: "gosmo_live_t23_no_such_login",
	}); err == nil {
		t.Error("AddSchedule with an unknown owner succeeded")
	}
	scheds, err := j.Schedules(ctx)
	if err != nil {
		t.Fatalf("Job.Schedules: %v", err)
	}
	var names []string
	for _, s := range scheds {
		names = append(names, s.Name)
	}
	if len(scheds) != 2 {
		t.Errorf("job schedules = %v, want the weekly and monthly ones only — a refused owner left one behind", names)
	}
}
