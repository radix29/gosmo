// Command jobs demonstrates gosmo's SQL Server Agent surface: categories,
// operators, a job with several steps and branching, a shared schedule
// attached to it, running it, reading its history, and alerts.
//
// It creates a disposable job, operator, category and schedule, then removes
// all four. Existing Agent objects are only read.
//
//	MSSQL_SERVER=localhost:1433 MSSQL_USER=sa MSSQL_PASSWORD=YourPw go run ./examples/jobs
package main

import (
	"fmt"
	"time"

	gosmo "github.com/radix29/gosmo"
	"github.com/radix29/gosmo/examples/internal/demo"
)

const (
	jobName      = "gosmo demo job"
	categoryName = "gosmo demo category"
	operatorName = "gosmo demo operator"
	scheduleName = "gosmo demo nightly"
	alertName    = "gosmo demo severity 17 alert"
)

func main() {
	// First, so it runs after the cleanup deferred below it.
	defer demo.Exit()

	srv := demo.Connect()
	defer srv.Close()

	// -- Is the Agent even running? ---------------------------------------
	//
	// Read from sys.dm_server_services only: no WMI, no Service Control
	// Manager, so it works the same on Linux. Azure SQL Database has no
	// Agent at all and everything below will fail there.
	demo.Section("Agent status")
	agent := demo.Value(srv.AgentInfo())
	fmt.Printf("  running=%t  status=%q  since=%s\n",
		agent.Running, agent.StatusText, formatTime(agent.LastStartupTime))

	// -- Category ----------------------------------------------------------
	demo.Section("Category")
	_ = srv.DeleteCategory(gosmo.CategoryClassJob, categoryName)
	demo.Must(srv.CreateCategory(gosmo.CategoryClassJob, categoryName))
	defer func() { _ = srv.DeleteCategory(gosmo.CategoryClassJob, categoryName) }()
	for _, c := range demo.Value(srv.Categories(gosmo.CategoryClassJob)) {
		fmt.Printf("  [%d] %s\n", c.ID, c.Name)
	}

	// -- Operator ----------------------------------------------------------
	demo.Section("Operator")
	if op, err := srv.OperatorByName(operatorName); err == nil {
		demo.Must(op.Drop())
	}
	op := demo.Value(srv.CreateOperator(gosmo.CreateOperatorRequest{
		Name:         operatorName,
		Enabled:      true,
		EmailAddress: "dba@example.com",
	}))
	defer func() { _ = op.Drop() }()
	fmt.Printf("  %s  email=%s  enabled=%t\n", op.Name, op.EmailAddress, op.Enabled)

	// -- Job ---------------------------------------------------------------
	demo.Section("Job")
	if existing, err := srv.JobByName(jobName); err == nil {
		demo.Must(existing.Drop())
	}
	job := demo.Value(srv.CreateJob(gosmo.CreateJobRequest{
		Name:        jobName,
		Description: "Created by the gosmo jobs example",
		Category:    categoryName,
		Enabled:     true,
	}))
	defer func() {
		if err := job.Drop(); err == nil {
			fmt.Printf("\nDropped job [%s]\n", jobName)
		}
	}()
	fmt.Printf("  %s  id=%s  category=%s\n", job.Name, job.JobID, job.Category)

	// -- Steps -------------------------------------------------------------
	//
	// OnSuccessAction/OnFailAction: 1=quit reporting success,
	// 2=quit reporting failure, 3=go to the next step, 4=go to step N (in
	// which case set OnSuccessStepID/OnFailStepID).
	demo.Section("Steps")
	demo.Must(job.AddStep(gosmo.JobStepRequest{
		Name:            "Check free space",
		Subsystem:       "TSQL",
		Database:        "master",
		Command:         "SELECT DB_NAME(database_id), SUM(size) * 8 / 1024 AS MB FROM sys.master_files GROUP BY database_id;",
		OnSuccessAction: 3, // next step
		OnFailAction:    4, // jump to the handler
		OnFailStepID:    3,
		RetryAttempts:   2,
		RetryInterval:   1, // minutes
	}))
	demo.Must(job.AddStep(gosmo.JobStepRequest{
		Name:            "Cycle the error log",
		Subsystem:       "TSQL",
		Database:        "master",
		Command:         "EXEC sp_cycle_errorlog;",
		OnSuccessAction: 1, // quit, success
		OnFailAction:    4,
		OnFailStepID:    3,
	}))
	demo.Must(job.AddStep(gosmo.JobStepRequest{
		Name:            "Failure handler",
		Subsystem:       "TSQL",
		Database:        "master",
		Command:         "RAISERROR('gosmo demo job failed', 16, 1);",
		OnSuccessAction: 2, // quit, failure
		OnFailAction:    2,
	}))
	for _, s := range demo.Value(job.Steps()) {
		fmt.Printf("  %d. %-20s subsystem=%-6s on_success=%d on_fail=%d->%d\n",
			s.StepID, s.Name, s.Subsystem, s.OnSuccessAction, s.OnFailAction, s.OnFailStepID)
	}

	// -- Notification ------------------------------------------------------
	demo.Must(job.SetEmailNotify(operatorName, gosmo.NotifyOnFailure))
	demo.Must(job.SetDeleteLevel(gosmo.NotifyNever))

	// -- Schedules ---------------------------------------------------------
	//
	// Two shapes exist and they are not the same thing. Job.AddSchedule
	// creates a schedule owned by that job; Server.CreateSchedule creates a
	// shared schedule that Job.AttachSchedule can bind to several jobs.
	demo.Section("Schedules")
	demo.Must(job.AddSchedule(gosmo.JobScheduleRequest{
		Name:               "every 15 minutes",
		Enabled:            true,
		FreqType:           4, // daily
		FreqInterval:       1,
		FreqSubdayType:     4, // minutes
		FreqSubdayInterval: 15,
		ActiveStartTime:    0,      // 00:00:00
		ActiveEndTime:      235959, // 23:59:59
	}))

	if existing, err := srv.ScheduleByName(scheduleName); err == nil {
		demo.Must(existing.Drop())
	}
	shared := demo.Value(srv.CreateSchedule(gosmo.CreateScheduleRequest{
		Name:                 scheduleName,
		Enabled:              true,
		FreqType:             gosmo.FreqWeekly,
		FreqInterval:         gosmo.WeekdaySaturday | gosmo.WeekdaySunday,
		FreqRecurrenceFactor: 1, // every week
		FreqSubdayType:       gosmo.SubdayOnce,
		ActiveStartTime:      23000, // 02:30:00 as HHMMSS
	}))
	defer func() { _ = shared.Drop() }()
	demo.Must(job.AttachSchedule(scheduleName))

	for _, s := range demo.Value(job.Schedules()) {
		// Description renders the freq_* cluster the way SSMS's schedule
		// list does, which is the only readable form of those columns.
		fmt.Printf("  %-20s %s\n", s.Name, s.Description())
	}

	// -- Run it ------------------------------------------------------------
	//
	// Everything above is msdb metadata, which is writable whether or not
	// the Agent service is up. Starting a job is not: sp_start_job fails
	// outright when the Agent is stopped, so gate on AgentInfo first.
	demo.Section("Run")
	if !agent.Running {
		fmt.Println("  SQL Server Agent is not running — skipping the run and its history")
	} else {
		demo.Must(job.Start("")) // "" starts at the job's first step
		fmt.Println("  started; waiting for it to finish")
		for range 20 {
			time.Sleep(500 * time.Millisecond)
			current := demo.Value(srv.JobByName(jobName))
			if current.CurrentState == gosmo.JobStateIdle {
				fmt.Printf("  last outcome: %s (%s)\n",
					outcomeName(current.LastRunOutcome), current.LastRunDuration)
				break
			}
		}

		demo.Section("History")
		for _, h := range demo.Value(job.History(10)) {
			step := h.StepName
			if h.StepID == 0 {
				step = "(job outcome)"
			}
			fmt.Printf("  %s  %-20s %-10s %s\n",
				formatTime(h.RunDate), step, outcomeName(h.Outcome), firstLine(h.Message))
		}
	}

	// -- Alerts ------------------------------------------------------------
	//
	// Only plain SQL Server event alerts are creatable: WMI and
	// performance-condition alerts need a Windows provider, so they are
	// listed but not manageable. IsEventAlert / EventAlerts identify the
	// manageable subset.
	demo.Section("Alerts")
	if existing, err := srv.AlertByName(alertName); err == nil {
		demo.Must(existing.Drop())
	}
	alert := demo.Value(srv.CreateAlert(gosmo.CreateAlertRequest{
		Name:                  alertName,
		Enabled:               true,
		Severity:              17, // mutually exclusive with ErrorNumber
		DelayBetweenResponses: 5 * time.Minute,
		NotificationMessage:   "Severity 17 raised — see the error log.",
	}))
	defer func() { _ = alert.Drop() }()
	demo.Must(alert.Notify(operatorName, gosmo.NotifyMethodEmail))
	demo.Must(alert.SetJobResponse(jobName))

	for _, n := range demo.Value(alert.Notifications()) {
		fmt.Printf("  notifies %s via %s\n", n.OperatorName, n.Method)
	}
	for _, a := range demo.Value(srv.EventAlerts()) {
		fmt.Printf("  %-32s severity=%d error=%d job=%s\n",
			a.Name, a.Severity, a.ErrorNumber, a.JobName)
	}

	// The alert has to stop pointing at the job before the job can be
	// dropped; deferred cleanup runs in reverse order, so undo it here.
	demo.Must(alert.SetJobResponse(""))

	// -- Everything on the instance ---------------------------------------
	demo.Section("All jobs")
	for _, j := range demo.Value(srv.Jobs()) {
		fmt.Printf("  %-40s enabled=%-5t last=%s next=%s\n",
			j.Name, j.IsEnabled, outcomeName(j.LastRunOutcome), formatTime(j.NextRunDate))
	}
}

func outcomeName(o gosmo.JobOutcome) string {
	switch o {
	case gosmo.JobOutcomeFailed:
		return "failed"
	case gosmo.JobOutcomeSucceeded:
		return "succeeded"
	case gosmo.JobOutcomeRetried:
		return "retried"
	case gosmo.JobOutcomeCancelled:
		return "cancelled"
	default:
		return "unknown"
	}
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	return t.Format("2006-01-02 15:04:05")
}

func firstLine(s string) string {
	if i := len(s); i > 60 {
		s = s[:60] + "..."
	}
	for i, r := range s {
		if r == '\n' {
			return s[:i]
		}
	}
	return s
}
