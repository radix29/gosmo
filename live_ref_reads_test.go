//go:build livedb

// Live checks that a JobRef's and a LoginRef's id-keyed reads look the id up
// by name and return what the read handle returns.
//
//	go test -tags livedb . -run TestLiveRefReads -v \
//	  -livedb 'sqlserver://sa:PASS@host?TrustServerCertificate=true'
//
// Creates and drops its own job, login and database; touches nothing else.
package gosmo

import (
	"context"
	"errors"
	"testing"
)

func TestLiveRefReadsJob(t *testing.T) {
	db, ctx, done := liveDB(t)
	t.Cleanup(done)
	s, err := NewServer(ctx, db)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	const jobName = "gosmo_ref_reads_job"
	if old, err := s.JobByName(ctx, jobName); err == nil {
		_ = old.Drop(ctx)
	}
	if _, err := s.CreateJob(ctx, CreateJobRequest{Name: jobName, Enabled: false, OwnerLogin: "sa"}); err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	t.Cleanup(func() { _ = s.JobRef(jobName).Drop(context.Background()) })
	ref := s.JobRef(jobName)
	for _, n := range []string{"a", "b"} {
		if _, err := ref.AddStep(ctx, JobStepRequest{Name: n, Subsystem: "TSQL", Command: "SELECT 1",
			OnSuccessAction: 3, OnFailAction: 2}); err != nil {
			t.Fatalf("AddStep %s: %v", n, err)
		}
	}
	if _, err := ref.AddSchedule(ctx, CreateScheduleRequest{Name: "gosmo_ref_reads_sched", FreqType: FreqDaily, FreqInterval: 1, Enabled: true}); err != nil {
		t.Fatalf("AddSchedule: %v", err)
	}
	t.Cleanup(func() { _ = s.ScheduleRef("gosmo_ref_reads_sched").Drop(context.Background()) })

	steps, err := ref.Steps(ctx)
	if err != nil || len(steps) != 2 || steps[0].Name != "a" {
		t.Fatalf("JobRef.Steps = %d steps, %v; want a, b", len(steps), err)
	}
	if sch, err := ref.Schedules(ctx); err != nil || len(sch) != 1 {
		t.Fatalf("JobRef.Schedules = %d, %v; want 1", len(sch), err)
	}
	if _, err := ref.History(ctx, 0); err != nil {
		t.Fatalf("JobRef.History: %v", err)
	}
	if err := ref.ReorderSteps(ctx, func(int) []int { return []int{2, 1} }); err != nil {
		t.Fatalf("JobRef.ReorderSteps: %v", err)
	}
	if steps, err := ref.Steps(ctx); err != nil || steps[0].Name != "b" {
		t.Fatalf("after reorder Steps = %v, %v; want b first", steps, err)
	}
	if _, err := s.JobRef("gosmo_ref_reads_no_such_job").Steps(ctx); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing job: err = %v, want ErrNotFound", err)
	}
}

func TestLiveRefReadsLogin(t *testing.T) {
	db, ctx, done := liveDB(t)
	t.Cleanup(done)
	d, drop := liveScratchDB(t, db, ctx, "gosmo_ref_reads_db")
	t.Cleanup(drop)
	s := d.server
	const loginName = "gosmo_ref_reads_login"
	_, _ = db.ExecContext(ctx, "IF SUSER_ID('"+loginName+"') IS NOT NULL DROP LOGIN ["+loginName+"]")
	if _, err := s.CreateLogin(ctx, CreateLoginRequest{Name: loginName, Password: "Gosmo#Ref#Reads#9", CheckPolicy: new(false)}); err != nil {
		t.Fatalf("CreateLogin: %v", err)
	}
	t.Cleanup(func() { _, _ = db.ExecContext(context.Background(), "DROP LOGIN ["+loginName+"]") })
	ref := s.LoginRef(loginName)
	if err := ref.MapToDatabase(ctx, d.Name, "u_ref", ""); err != nil {
		t.Fatalf("MapToDatabase: %v", err)
	}

	ms, err := ref.UserMappings(ctx)
	if err != nil || len(ms) != 1 || ms[0].Database != d.Name || ms[0].User != "u_ref" {
		t.Fatalf("LoginRef.UserMappings = %+v, %v; want u_ref in %s", ms, err, d.Name)
	}
	if err := ref.ResolveMapping(ctx); err != nil {
		t.Fatalf("LoginRef.ResolveMapping: %v", err)
	}
	if err := ref.UnmapFromDatabase(ctx, d.Name); err != nil {
		t.Fatalf("LoginRef.UnmapFromDatabase: %v", err)
	}
	if ms, err := ref.UserMappings(ctx); err != nil || len(ms) != 0 {
		t.Fatalf("after unmap UserMappings = %+v, %v; want none", ms, err)
	}
	if _, err := s.LoginRef("gosmo_ref_reads_no_such_login").UserMappings(ctx); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing login: err = %v, want ErrNotFound", err)
	}
}
