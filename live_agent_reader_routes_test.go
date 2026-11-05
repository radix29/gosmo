//go:build livedb

package gosmo

import (
	"context"
	"database/sql"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// TestLiveAgentReaderOperatorsAndJobAlerts: a SQLAgentReaderRole-only login
// lists operators through sp_help_operator and a job's alerts through
// sp_get_job_alerts, and an operator's notifying jobs through sysjobs_view,
// and gets what sysadmin's direct read gets; the reads
// with no route below SQLAgentOperatorRole — Alerts, EventAlerts,
// AlertByName, Alert.Notifications, Operator.NotifyingAlerts — refuse with an
// error IsPermissionDenied classifies. Before the operatorRead route
// Operators was Msg 229 on sysoperators for this login.
//
// It creates the login, an operator, a job and two alerts responding with
// that job, and drops them all. Agent need not be running.
func TestLiveAgentReaderOperatorsAndJobAlerts(t *testing.T) {
	db, ctx, done := liveDB(t)
	t.Cleanup(done)
	sa := &Server{db: db}
	c := context.Background()

	const (
		login    = "gosmo_live_k1_reader"
		pass     = "Pw!gosmo_live_k1reader1"
		prefix   = "gosmo_live_k1_"
		job      = prefix + "job"
		operator = prefix + "op"
		alertA   = prefix + "alert_a"
		alertB   = prefix + "alert_b"
	)

	cleanup := func() {
		for _, a := range []string{alertA, alertB} {
			db.ExecContext(c, "IF EXISTS (SELECT 1 FROM msdb.dbo.sysalerts WHERE name = N'"+a+"') "+
				"EXEC msdb.dbo.sp_delete_alert @name = N'"+a+"'")
		}
		db.ExecContext(c, "IF EXISTS (SELECT 1 FROM msdb.dbo.sysjobs WHERE name = N'"+job+"') "+
			"EXEC msdb.dbo.sp_delete_job @job_name = N'"+job+"'")
		db.ExecContext(c, "IF EXISTS (SELECT 1 FROM msdb.dbo.sysoperators WHERE name = N'"+operator+"') "+
			"EXEC msdb.dbo.sp_delete_operator @name = N'"+operator+"'")
		db.ExecContext(c, "USE msdb; IF USER_ID(N'"+login+"') IS NOT NULL DROP USER ["+login+"]")
		db.ExecContext(c, "IF SUSER_ID(N'"+login+"') IS NOT NULL DROP LOGIN ["+login+"]")
	}
	cleanup()
	t.Cleanup(cleanup)

	for _, q := range []string{
		"CREATE LOGIN [" + login + "] WITH PASSWORD = N'" + pass + "', CHECK_POLICY = OFF",
		"USE msdb; CREATE USER [" + login + "] FOR LOGIN [" + login + "]; ALTER ROLE [SQLAgentReaderRole] ADD MEMBER [" + login + "]",
	} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	if _, err := sa.CreateOperator(ctx, CreateOperatorRequest{Name: operator, Enabled: true, EmailAddress: "k1@example.invalid"}); err != nil {
		t.Fatalf("create operator: %v", err)
	}
	if _, err := sa.CreateJob(ctx, CreateJobRequest{Name: job, Description: "gosmo live", OwnerLogin: "sa", Enabled: true}); err != nil {
		t.Fatalf("create job: %v", err)
	}
	// alertB is created first and disabled, so the name sort and the
	// enabled flag are both exercised.
	for _, a := range []struct {
		name, database string
		enabled        bool
	}{{alertB, "model", false}, {alertA, "msdb", true}} {
		// sp_add_alert needs a trigger, one alert per (message, severity,
		// database); severity 25 in model or msdb is never raised here.
		if _, err := sa.CreateAlert(ctx, CreateAlertRequest{Name: a.name, Enabled: a.enabled, Severity: 25, DatabaseName: a.database}); err != nil {
			t.Fatalf("create alert %s: %v", a.name, err)
		}
		if _, err := db.ExecContext(ctx, "EXEC msdb.dbo.sp_update_alert @name = N'"+a.name+"', @job_name = N'"+job+"'"); err != nil {
			t.Fatalf("attach %s to %s: %v", a.name, job, err)
		}
		if err := sa.AlertRef(a.name).Notify(ctx, operator, NotifyMethodEmail); err != nil {
			t.Fatalf("notify %s: %v", a.name, err)
		}
	}

	if _, err := db.ExecContext(ctx, "EXEC msdb.dbo.sp_update_job @job_name = N'"+job+"', "+
		"@notify_level_email = 2, @notify_email_operator_name = N'"+operator+"'"); err != nil {
		t.Fatalf("set %s's e-mail operator: %v", job, err)
	}

	ours := func(name string) bool { return strings.HasPrefix(name, prefix) }
	readOperators := func(s *Server) []Operator {
		t.Helper()
		ops, err := s.Operators(ctx)
		if err != nil {
			t.Fatalf("Operators: %v", err)
		}
		var out []Operator
		for _, o := range ops {
			if ours(o.Name) {
				v := *o
				v.server = nil
				out = append(out, v)
			}
		}
		return out
	}
	readJobAlerts := func(s *Server) []JobAlert {
		t.Helper()
		as, err := s.JobRef(job).Alerts(ctx)
		if err != nil {
			t.Fatalf("Job.Alerts: %v", err)
		}
		var out []JobAlert
		for _, a := range as {
			out = append(out, *a)
		}
		return out
	}

	wantOps, wantAlerts := readOperators(sa), readJobAlerts(sa)
	if len(wantOps) != 1 || wantOps[0].EmailAddress != "k1@example.invalid" {
		t.Fatalf("sysadmin's operators = %+v", wantOps)
	}
	if len(wantAlerts) != 2 || wantAlerts[0].Name != alertA || !wantAlerts[0].Enabled ||
		wantAlerts[1].Name != alertB || wantAlerts[1].Enabled || wantAlerts[0].IsPerformanceAlert {
		t.Fatalf("sysadmin's job alerts = %+v", wantAlerts)
	}

	pool, err := sql.Open("sqlserver", liveRestrictedDSN(t, login, pass))
	if err != nil {
		t.Fatalf("open as %s: %v", login, err)
	}
	t.Cleanup(func() { pool.Close() })
	rd := &Server{db: pool}

	if got := readOperators(rd); !reflect.DeepEqual(got, wantOps) {
		t.Errorf("reader's Operators\n%+v\nsysadmin's\n%+v", got, wantOps)
	}
	if o, err := rd.OperatorByName(ctx, operator); err != nil {
		t.Errorf("reader's OperatorByName: %v", err)
	} else {
		v := *o
		v.server = nil
		if !reflect.DeepEqual(v, wantOps[0]) {
			t.Errorf("reader's OperatorByName\n%+v\nsysadmin's\n%+v", v, wantOps[0])
		}
	}
	if got := readJobAlerts(rd); !reflect.DeepEqual(got, wantAlerts) {
		t.Errorf("reader's Job.Alerts\n%+v\nsysadmin's\n%+v", got, wantAlerts)
	}

	// NotifyingJobs reads sysjobs_view: before, sysjobs refused it to every
	// Agent role.
	readNotifyingJobs := func(s *Server) []OperatorJob {
		t.Helper()
		op, err := s.OperatorByName(ctx, operator)
		if err != nil {
			t.Fatalf("OperatorByName: %v", err)
		}
		js, err := op.NotifyingJobs(ctx)
		if err != nil {
			t.Fatalf("NotifyingJobs: %v", err)
		}
		var out []OperatorJob
		for _, j := range js {
			out = append(out, *j)
		}
		return out
	}
	wantJobs := readNotifyingJobs(sa)
	if len(wantJobs) != 1 || wantJobs[0].JobName != job || wantJobs[0].Level != NotifyLevel(2) {
		t.Fatalf("sysadmin's NotifyingJobs = %+v", wantJobs)
	}
	if got := readNotifyingJobs(rd); !reflect.DeepEqual(got, wantJobs) {
		t.Errorf("reader's NotifyingJobs\n%+v\nsysadmin's\n%+v", got, wantJobs)
	}

	// No route below SQLAgentOperatorRole: each refuses, classified.
	refused := map[string]error{}
	_, refused["Alerts"] = rd.Alerts(ctx)
	_, refused["EventAlerts"] = rd.EventAlerts(ctx)
	_, refused["AlertByName"] = rd.AlertByName(ctx, alertA)
	// Both need a loaded handle (they read by id). The reader can load the
	// operator, but no alert at all, so its alert is sa's re-pointed.
	saAlert, err := sa.AlertByName(ctx, alertA)
	if err != nil {
		t.Fatalf("sysadmin's AlertByName: %v", err)
	}
	rdAlert := *saAlert
	rdAlert.server = rd
	_, refused["Alert.Notifications"] = rdAlert.Notifications(ctx)
	if op, err := rd.OperatorByName(ctx, operator); err != nil {
		t.Fatalf("reader's OperatorByName: %v", err)
	} else {
		_, refused["Operator.NotifyingAlerts"] = op.NotifyingAlerts(ctx)
	}
	names := slices.Sorted(func(yield func(string) bool) {
		for k := range refused {
			if !yield(k) {
				return
			}
		}
	})
	for _, name := range names {
		if err := refused[name]; !IsPermissionDenied(err) {
			t.Errorf("reader's %s: err = %v, want a permission refusal", name, err)
		}
	}
}
