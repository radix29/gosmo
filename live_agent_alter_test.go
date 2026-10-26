//go:build livedb

package gosmo

import (
	"context"
	"testing"
	"time"
)

// TestLiveAgentSettersClearCategory pins what the per-property setters
// send, now that each wraps Alter, against msdb itself: an emptied category
// is accepted for every class and reads back as the name the receiver
// mirrors. Jobs are the case this exists for — they are sent [DEFAULT],
// not the [Uncategorized] alerts and operators take, and must read back as
// [Uncategorized (Local)] on the receiver as well as on the server.
//
//	go test -tags livedb . -run TestLiveAgentSettersClearCategory -v -livedb '...'
func TestLiveAgentSettersClearCategory(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()
	srv := &Server{db: db}

	const (
		jobName     = "gosmo_live_k2_job"
		jobRenamed  = "gosmo_live_k2_job2"
		alertName   = "gosmo_live_k2_alert"
		operatorNam = "gosmo_live_k2_op"
	)
	drop := func() {
		bg := context.Background()
		for _, n := range []string{jobName, jobRenamed} {
			srv.exec(bg, "IF EXISTS (SELECT 1 FROM msdb.dbo.sysjobs WHERE name = N'"+n+"') "+
				"EXEC msdb.dbo.sp_delete_job @job_name = N'"+n+"'")
		}
		srv.exec(bg, "IF EXISTS (SELECT 1 FROM msdb.dbo.sysalerts WHERE name = N'"+alertName+"') "+
			"EXEC msdb.dbo.sp_delete_alert @name = N'"+alertName+"'")
		srv.exec(bg, "IF EXISTS (SELECT 1 FROM msdb.dbo.sysoperators WHERE name = N'"+operatorNam+"') "+
			"EXEC msdb.dbo.sp_delete_operator @name = N'"+operatorNam+"'")
	}
	drop()
	// Deferred, not t.Cleanup: a cleanup runs after the deferred done() has
	// closed the pool, and the drop then fails silently.
	defer drop()

	// Job: a real category, then back to none, then a rename and a disable
	// through the wrappers.
	j, err := srv.CreateJob(ctx, CreateJobRequest{Name: jobName, Enabled: true})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	if err := j.SetCategory(ctx, "Database Maintenance"); err != nil {
		t.Fatalf("Job.SetCategory(Database Maintenance): %v", err)
	}
	if err := j.SetCategory(ctx, ""); err != nil {
		t.Fatalf(`Job.SetCategory(""): %v`, err)
	}
	if err := j.Rename(ctx, jobRenamed); err != nil {
		t.Fatalf("Job.Rename: %v", err)
	}
	if err := j.Disable(ctx); err != nil {
		t.Fatalf("Job.Disable: %v", err)
	}
	got, err := srv.JobByName(ctx, jobRenamed)
	if err != nil {
		t.Fatalf("JobByName: %v", err)
	}
	if got.Category != j.Category || got.Category != "[Uncategorized (Local)]" {
		t.Errorf("job category: server %q, receiver %q, want [Uncategorized (Local)]", got.Category, j.Category)
	}
	if got.IsEnabled || j.IsEnabled {
		t.Errorf("job enabled: server %v, receiver %v, want false", got.IsEnabled, j.IsEnabled)
	}

	// Alert and operator: "" is sent as [Uncategorized].
	a, err := srv.CreateAlert(ctx, CreateAlertRequest{Name: alertName, Severity: 17})
	if err != nil {
		t.Fatalf("CreateAlert: %v", err)
	}
	if err := a.SetCategory(ctx, ""); err != nil {
		t.Fatalf(`Alert.SetCategory(""): %v`, err)
	}
	if err := a.SetDelay(ctx, 90*time.Second); err != nil {
		t.Fatalf("Alert.SetDelay: %v", err)
	}
	gotA, err := srv.AlertByName(ctx, alertName)
	if err != nil {
		t.Fatalf("AlertByName: %v", err)
	}
	if gotA.Category != a.Category || gotA.DelayBetweenResponses != 90*time.Second {
		t.Errorf("alert: server category %q delay %v, receiver category %q", gotA.Category, gotA.DelayBetweenResponses, a.Category)
	}

	o, err := srv.CreateOperator(ctx, CreateOperatorRequest{Name: operatorNam, Enabled: true})
	if err != nil {
		t.Fatalf("CreateOperator: %v", err)
	}
	if err := o.SetCategory(ctx, ""); err != nil {
		t.Fatalf(`Operator.SetCategory(""): %v`, err)
	}
	if err := o.SetEmailAddress(ctx, "k2@example.com"); err != nil {
		t.Fatalf("Operator.SetEmailAddress: %v", err)
	}
	gotO, err := srv.OperatorByName(ctx, operatorNam)
	if err != nil {
		t.Fatalf("OperatorByName: %v", err)
	}
	if gotO.Category != o.Category || gotO.EmailAddress != "k2@example.com" {
		t.Errorf("operator: server category %q email %q, receiver category %q", gotO.Category, gotO.EmailAddress, o.Category)
	}
}
