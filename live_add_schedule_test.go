//go:build livedb

package gosmo

import (
	"context"
	"errors"
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

// TestLiveSharedScheduleNames pins J1 against msdb: two schedules may share a
// name, and every by-name procedure but sp_detach_schedule (which resolves
// within the job) then refuses it with Msg 14371. ScheduleByName reports the
// ambiguity, ScheduleByID reads the right one, CreateSchedule reads back the
// schedule it made rather than the older namesake, and attach, detach,
// disable and drop by handle each touch only their own schedule.
//
//	go test -tags livedb . -run TestLiveSharedScheduleNames -v -livedb '...'
func TestLiveSharedScheduleNames(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()
	srv := &Server{db: db}

	const (
		jobName = "gossms_j1_job"
		dup     = "gossms_j1_dup"
	)
	drop := func() {
		bg := context.Background()
		srv.exec(bg, "IF EXISTS (SELECT 1 FROM msdb.dbo.sysjobs WHERE name = N'"+jobName+"') "+
			"EXEC msdb.dbo.sp_delete_job @job_name = N'"+jobName+"', @delete_unused_schedule = 0")
		srv.exec(bg, "DECLARE @i int; WHILE 1 = 1 BEGIN SET @i = NULL; "+
			"SELECT TOP (1) @i = schedule_id FROM msdb.dbo.sysschedules WHERE name = N'"+dup+"'; "+
			"IF @i IS NULL BREAK; EXEC msdb.dbo.sp_delete_schedule @schedule_id = @i, @force_delete = 1; END")
	}
	drop()
	// Deferred, not t.Cleanup: a cleanup runs after the deferred done() has
	// closed the pool, and the drop then fails silently.
	defer drop()

	req := CreateScheduleRequest{Name: dup, Enabled: true, FreqType: FreqDaily, FreqInterval: 1, FreqSubdayType: SubdayOnce}
	first, err := srv.CreateSchedule(ctx, req)
	if err != nil {
		t.Fatalf("CreateSchedule first: %v", err)
	}
	second, err := srv.CreateSchedule(ctx, req)
	if err != nil {
		t.Fatalf("CreateSchedule second: %v", err)
	}
	if first.ID == 0 || second.ID == 0 || first.ID == second.ID {
		t.Fatalf("CreateSchedule ids = %d, %d; want two distinct ids", first.ID, second.ID)
	}
	// L6: a zero ActiveStartDate is left to sp_add_schedule, which stores
	// the server's today, not the client's.
	if today := liveServerToday(t, ctx, srv); !first.ActiveStartDate.Equal(today) {
		t.Errorf("CreateSchedule with no start date: active_start_date = %v, want the server's today %v", first.ActiveStartDate, today)
	}

	if _, err := srv.ScheduleByName(ctx, dup); !errors.Is(err, ErrAmbiguous) {
		t.Errorf("ScheduleByName(shared) err = %v, want ErrAmbiguous", err)
	}
	got, err := srv.ScheduleByID(ctx, second.ID)
	if err != nil || got.ID != second.ID || got.Name != dup {
		t.Errorf("ScheduleByID(%d) = %+v, %v", second.ID, got, err)
	}
	if _, err := srv.ScheduleByID(ctx, -1); !errors.Is(err, ErrNotFound) {
		t.Errorf("ScheduleByID(-1) err = %v, want ErrNotFound", err)
	}

	// The by-name form is what msdb refuses; pin that the reason for all of
	// this is real on this server.
	if err := srv.JobRef(jobName).AttachSchedule(ctx, srv.ScheduleRef(dup)); err == nil {
		t.Error("attach by a shared name succeeded; msdb was expected to refuse it (Msg 14371)")
	}

	j, err := srv.CreateJob(ctx, CreateJobRequest{Name: jobName})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	if err := j.AttachSchedule(ctx, second); err != nil {
		t.Fatalf("AttachSchedule by handle: %v", err)
	}
	attached, err := j.Schedules(ctx)
	if err != nil || len(attached) != 1 || attached[0].ID != second.ID {
		t.Fatalf("job schedules after attach = %v, %v; want only %d", attached, err, second.ID)
	}
	// L4: Jobs on a handle reads by its id; on a ScheduleRef it looks the id
	// up by name, which a shared name makes ambiguous rather than "no jobs".
	if jobs, err := second.Jobs(ctx); err != nil || len(jobs) != 1 || jobs[0].Name != jobName {
		t.Errorf("second.Jobs = %v, %v; want only %s", jobs, err, jobName)
	}
	if _, err := srv.ScheduleRef(dup).Jobs(ctx); !errors.Is(err, ErrAmbiguous) {
		t.Errorf("ScheduleRef(shared).Jobs err = %v, want ErrAmbiguous", err)
	}
	if err := j.DetachSchedule(ctx, second); err != nil {
		t.Fatalf("DetachSchedule by handle: %v", err)
	}
	if attached, err := j.Schedules(ctx); err != nil || len(attached) != 0 {
		t.Errorf("job schedules after detach = %v, %v; want none", attached, err)
	}

	if err := second.Disable(ctx); err != nil {
		t.Fatalf("Disable second: %v", err)
	}
	if f, err := srv.ScheduleByID(ctx, first.ID); err != nil || !f.Enabled {
		t.Errorf("first schedule after disabling the second = %+v, %v; want still enabled", f, err)
	}

	if err := second.Drop(ctx); err != nil {
		t.Fatalf("Drop second: %v", err)
	}
	if _, err := srv.ScheduleByID(ctx, second.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("second after Drop: err = %v, want ErrNotFound", err)
	}
	only, err := srv.ScheduleByName(ctx, dup)
	if err != nil || only.ID != first.ID {
		t.Errorf("ScheduleByName after dropping the second = %+v, %v; want %d", only, err, first.ID)
	}

	if err := j.AttachSchedule(ctx, first); err != nil {
		t.Fatalf("AttachSchedule first: %v", err)
	}
	if jobs, err := srv.ScheduleRef(dup).Jobs(ctx); err != nil || len(jobs) != 1 || jobs[0].Name != jobName {
		t.Errorf("ScheduleRef(now unique).Jobs = %v, %v; want only %s", jobs, err, jobName)
	}
	if _, err := srv.ScheduleRef("gossms_j1_no_such_schedule").Jobs(ctx); !errors.Is(err, ErrNotFound) {
		t.Errorf("ScheduleRef(missing).Jobs err = %v, want ErrNotFound", err)
	}
}

// liveServerToday is the server's GETDATE() date, as ActiveStartDate decodes
// a sysschedules date (UTC midnight).
func liveServerToday(t *testing.T, ctx context.Context, srv *Server) time.Time {
	t.Helper()
	var today int
	if err := srv.queryRowScan(ctx, "SELECT CONVERT(int, CONVERT(char(8), GETDATE(), 112))", nil, &today); err != nil {
		t.Fatalf("read the server's date: %v", err)
	}
	return yyyymmddToTime(today)
}

// TestLiveAddScheduleSharedName pins H5 (gossms
// docs/review-plan-2026-10-04.md): AddSchedule reads back the schedule it
// made by the id sp_add_jobschedule returns, so two schedules of one name on
// one job come back as two handles, and detaching and dropping the first by
// its handle leaves the second. Before, the handle was the job's newest schedule of that
// name, or a name-only handle msdb refuses (Msg 14371) once the name is shared.
//
//	go test -tags livedb . -run TestLiveAddScheduleSharedName -v -livedb '...'
func TestLiveAddScheduleSharedName(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()
	srv := &Server{db: db}

	const (
		jobName = "gosmo_live_h5_job"
		dup     = "gosmo_live_h5_dup"
	)
	drop := func() {
		srv.exec(context.Background(), "IF EXISTS (SELECT 1 FROM msdb.dbo.sysjobs WHERE name = N'"+jobName+"') "+
			"EXEC msdb.dbo.sp_delete_job @job_name = N'"+jobName+"', @delete_unused_schedule = 1")
	}
	drop()
	// Deferred, not t.Cleanup: see TestLiveJobAddScheduleRecurrences.
	defer drop()

	j, err := srv.CreateJob(ctx, CreateJobRequest{Name: jobName})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	req := CreateScheduleRequest{Name: dup, Enabled: true, FreqType: FreqDaily, FreqInterval: 1, FreqSubdayType: SubdayOnce}
	first, err := j.AddSchedule(ctx, req)
	if err != nil {
		t.Fatalf("AddSchedule first: %v", err)
	}
	req.OwnerLoginName = "sa" // the atomic branch
	second, err := j.AddSchedule(ctx, req)
	if err != nil {
		t.Fatalf("AddSchedule second (with owner): %v", err)
	}
	if first.ID == 0 || second.ID == 0 || first.ID == second.ID {
		t.Fatalf("AddSchedule ids = %d, %d; want two distinct ids", first.ID, second.ID)
	}
	// L6: sp_add_jobschedule, on both branches, defaults the start date to
	// the server's today.
	today := liveServerToday(t, ctx, srv)
	for _, sch := range []*Schedule{first, second} {
		if !sch.ActiveStartDate.Equal(today) {
			t.Errorf("AddSchedule %d with no start date: active_start_date = %v, want the server's today %v", sch.ID, sch.ActiveStartDate, today)
		}
	}
	// msdb refuses to drop an attached schedule (Msg 14372), and detaching
	// by a name two of the job's schedules share is ambiguous too.
	if err := j.DetachSchedule(ctx, first); err != nil {
		t.Fatalf("DetachSchedule first by its handle: %v", err)
	}
	if err := first.Drop(ctx); err != nil {
		t.Fatalf("Drop first by its handle: %v", err)
	}
	left, err := j.Schedules(ctx)
	if err != nil || len(left) != 1 || left[0].ID != second.ID {
		t.Errorf("job schedules after dropping the first = %v, %v; want only %d", left, err, second.ID)
	}
}
