package gosmo

import (
	"context"
	"database/sql/driver"
	"strings"
	"testing"
	"time"
)

// addJobSchedule is the Job AddSchedule case's statement.
const addJobSchedule = "EXEC msdb.dbo.sp_add_jobschedule @job_name = N'Nightly''Run', @name = N'Weekly''2am', @enabled = 1, " +
	"@freq_type = 8, @freq_interval = 34, @freq_subday_type = 1, @freq_subday_interval = 0, " +
	"@freq_relative_interval = 0, @freq_recurrence_factor = 1, " +
	"@active_start_date = 20261002, @active_end_date = 99991231, " +
	"@active_start_time = 20000, @active_end_time = 0"

// addSchedule is the CreateSchedule case's statement.
const addSchedule = "EXEC msdb.dbo.sp_add_schedule @schedule_name = N'Daily''2am', @enabled = 1, " +
	"@freq_type = 4, @freq_interval = 1, @freq_subday_type = 1, @freq_subday_interval = 0, " +
	"@freq_relative_interval = 0, @freq_recurrence_factor = 0, @active_end_date = 99991231, " +
	"@active_start_time = 20000, @active_end_time = 0, @owner_login_name = N'CONTOSO\\o''brien'"

// TestScriptAgentWrites pins the msdb stored-procedure calls behind the SQL
// Agent writes. See script_write_common_test.go.
//
// Everything here reaches msdb through sp_* parameters, which take names, not
// identifiers: a job or operator name has no brackets anywhere, and an
// apostrophe in one is the ordinary case, not an exotic one. Each string is
// bound (boundArgs), and a script shows it as N'...' — so every case runs
// twice: under WithScript, for the text a person is shown, and against the
// capture driver, for what is sent.
func TestScriptAgentWrites(t *testing.T) {
	// On the capture driver, which WithScript never reaches: runBoundCases
	// runs the same cases for real.
	srv := captureServer(t, 17)
	job := func() *Job { return &Job{server: srv, Name: "Nightly'Run"} }
	alert := func() *Alert { return &Alert{server: srv, Name: "Disk'Full"} }
	operator := func() *Operator { return &Operator{server: srv, Name: "On'Call"} }
	schedule := func() *Schedule { return &Schedule{server: srv, ID: 7, Name: "Daily'2am"} }

	cases := []scriptCase{
		// --- Job
		{"Job Rename", func(c context.Context) error {
			return job().Rename(c, "Nightly'Run 2")
		}, "EXEC msdb.dbo.sp_update_job @job_name = N'Nightly''Run', @new_name = N'Nightly''Run 2'"},
		{"Job Enable", func(c context.Context) error {
			return job().Enable(c)
		}, "EXEC msdb.dbo.sp_update_job @job_name = N'Nightly''Run', @enabled = 1"},
		{"Job Disable", func(c context.Context) error {
			return job().Disable(c)
		}, "EXEC msdb.dbo.sp_update_job @job_name = N'Nightly''Run', @enabled = 0"},
		{"Job Alter category", func(c context.Context) error {
			return job().Alter(c, JobChanges{Category: new("Data'Loads")})
		}, "EXEC msdb.dbo.sp_update_job @job_name = N'Nightly''Run', @category_name = N'Data''Loads'"},
		{"Job Alter owner", func(c context.Context) error {
			return job().Alter(c, JobChanges{OwnerLogin: new("CONTOSO\\o'brien")})
		}, "EXEC msdb.dbo.sp_update_job @job_name = N'Nightly''Run', @owner_login_name = N'CONTOSO\\o''brien'"},
		{"Job Drop", func(c context.Context) error {
			return job().Drop(c)
		}, "EXEC msdb.dbo.sp_delete_job @job_name = N'Nightly''Run'"},
		{"Job Alter description", func(c context.Context) error {
			return job().Alter(c, JobChanges{Description: new("runs at 2'am")})
		}, "EXEC msdb.dbo.sp_update_job @job_name = N'Nightly''Run', @description = N'runs at 2''am'"},
		{"Job Alter start step", func(c context.Context) error {
			return job().Alter(c, JobChanges{StartStepID: new(2)})
		}, "EXEC msdb.dbo.sp_update_job @job_name = N'Nightly''Run', @start_step_id = 2"},
		{"Job Alter email notification", func(c context.Context) error {
			return job().Alter(c, JobChanges{NotifyLevelEmail: new(NotifyOnFailure), NotifyEmailOperatorName: new("On'Call")})
		}, "EXEC msdb.dbo.sp_update_job @job_name = N'Nightly''Run', @notify_level_email = 2, @notify_email_operator_name = N'On''Call'"},
		{"Job Alter delete level", func(c context.Context) error {
			return job().Alter(c, JobChanges{DeleteLevel: new(NotifyOnFailure)})
		}, "EXEC msdb.dbo.sp_update_job @job_name = N'Nightly''Run', @delete_level = 2"},
		{"Job AddSchedule", func(c context.Context) error {
			_, err := job().AddSchedule(c, CreateScheduleRequest{
				Name: "Weekly'2am", Enabled: true,
				FreqType: FreqWeekly, FreqInterval: WeekdayMonday | WeekdayFriday, FreqRecurrenceFactor: 1,
				FreqSubdayType: SubdayOnce, ActiveStartTime: 20000,
				ActiveStartDate: time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC),
			})
			return err
		}, addJobSchedule},
		{"Job Start at a step", func(c context.Context) error {
			return job().Start(c, "Load'Step")
		}, "EXEC msdb.dbo.sp_start_job @job_name = N'Nightly''Run', @step_name = N'Load''Step'"},
		{"Job Stop", func(c context.Context) error {
			return job().Stop(c)
		}, "EXEC msdb.dbo.sp_stop_job @job_name = N'Nightly''Run'"},
		{"CreateJob", func(c context.Context) error {
			_, err := srv.CreateJob(c, CreateJobRequest{
				Name: "Nightly'Run", Description: "runs at 2'am", Category: "Data'Loads",
				Enabled: true, OwnerLogin: "CONTOSO\\o'brien",
			})
			return err
		}, atomicBatchText(
			"EXEC msdb.dbo.sp_add_job @job_name = N'Nightly''Run', @description = N'runs at 2''am', "+
				"@category_name = N'Data''Loads', @enabled = 1, @owner_login_name = N'CONTOSO\\o''brien'",
			"EXEC msdb.dbo.sp_add_jobserver @job_name = N'Nightly''Run', @server_name = N'(local)'")},
		{"Job AddStep", func(c context.Context) error {
			_, err := job().AddStep(c, JobStepDefinition{
				Name: "Load'Step", Subsystem: "TSQL", Command: "SELECT 'x'", Database: "App'DB",
				OnSuccessAction: 3, OnFailAction: 2, OutputFileName: `C:\o'log.txt`, ProxyName: "Px'1",
			})
			return err
		}, "EXEC msdb.dbo.sp_add_jobstep @job_name = N'Nightly''Run', @step_name = N'Load''Step', " +
			"@subsystem = N'TSQL', @command = N'SELECT ''x''', " +
			"@on_success_action = 3, @on_success_step_id = 0, @on_fail_action = 2, @on_fail_step_id = 0, " +
			"@retry_attempts = 0, @retry_interval = 0, @database_name = N'App''DB', " +
			"@output_file_name = N'C:\\o''log.txt', @flags = 0, @cmdexec_success_code = 0, " +
			"@os_run_priority = 0, @proxy_name = N'Px''1'"},
		{"JobStep Alter", func(c context.Context) error {
			return (&JobStep{job: job(), StepID: 2, Name: "Load"}).Alter(c, JobStepDefinition{
				Name: "Load'Step", Subsystem: "TSQL", Command: "SELECT 'x'", Database: "App'DB",
				OnSuccessAction: 1, OnFailAction: 2,
			})
		}, "EXEC msdb.dbo.sp_update_jobstep @job_name = N'Nightly''Run', @step_id = 2, " +
			"@step_name = N'Load''Step', @subsystem = N'TSQL', @command = N'SELECT ''x''', " +
			"@on_success_action = 1, @on_success_step_id = 0, @on_fail_action = 2, @on_fail_step_id = 0, " +
			"@retry_attempts = 0, @retry_interval = 0, @database_name = N'App''DB', @output_file_name = N''"},
		// Schedule names are not unique (J1), so a schedule with an ID is
		// addressed by it; only a ScheduleRef, which has none, by name.
		{"Job AttachSchedule by id", func(c context.Context) error {
			return job().AttachSchedule(c, schedule())
		}, "EXEC msdb.dbo.sp_attach_schedule @job_name = N'Nightly''Run', @schedule_id = 7"},
		{"Job AttachSchedule from a Ref", func(c context.Context) error {
			return job().AttachSchedule(c, (&Server{}).ScheduleRef("Daily'2am"))
		}, "EXEC msdb.dbo.sp_attach_schedule @job_name = N'Nightly''Run', @schedule_name = N'Daily''2am'"},
		{"Job DetachSchedule by id", func(c context.Context) error {
			return job().DetachSchedule(c, schedule())
		}, "EXEC msdb.dbo.sp_detach_schedule @job_name = N'Nightly''Run', @schedule_id = 7"},
		{"Job DetachSchedule from a Ref", func(c context.Context) error {
			return job().DetachSchedule(c, (&Server{}).ScheduleRef("Daily'2am"))
		}, "EXEC msdb.dbo.sp_detach_schedule @job_name = N'Nightly''Run', @schedule_name = N'Daily''2am'"},
		{
			// sp_update_jobstep names only the four flow parameters; every
			// other one omitted is "leave alone", which is what makes this a
			// flow change rather than a rewrite of the step.
			"JobStep SetFlow", func(c context.Context) error {
				return (&JobStep{job: job(), StepID: 3, Name: "Load"}).SetFlow(c, 4, 2, 2, 0)
			}, "EXEC msdb.dbo.sp_update_jobstep @job_name = N'Nightly''Run', @step_id = 3, " +
				"@on_success_action = 4, @on_success_step_id = 2, " +
				"@on_fail_action = 2, @on_fail_step_id = 0"},
		{"JobStep Drop", func(c context.Context) error {
			return (&JobStep{job: job(), StepID: 3}).Drop(c)
		}, "EXEC msdb.dbo.sp_delete_jobstep @job_name = N'Nightly''Run', @step_id = 3"},

		// --- Alert
		{"Alert Rename", func(c context.Context) error {
			return alert().Rename(c, "Disk'Full 2")
		}, "EXEC msdb.dbo.sp_update_alert @name = N'Disk''Full', @new_name = N'Disk''Full 2'"},
		{"Alert Enable", func(c context.Context) error {
			return alert().Enable(c)
		}, "EXEC msdb.dbo.sp_update_alert @name = N'Disk''Full', @enabled = 1"},
		{"Alert Disable", func(c context.Context) error {
			return alert().Disable(c)
		}, "EXEC msdb.dbo.sp_update_alert @name = N'Disk''Full', @enabled = 0"},
		{"Alert Drop", func(c context.Context) error {
			return alert().Drop(c)
		}, "EXEC msdb.dbo.sp_delete_alert @name = N'Disk''Full'"},
		{"Alert Alter trigger to an error number", func(c context.Context) error {
			return alert().Alter(c, AlertChanges{ErrorNumber: new(823), Severity: new(0)})
		}, "EXEC msdb.dbo.sp_update_alert @name = N'Disk''Full', @message_id = 823, @severity = 0"},
		{"Alert Alter trigger to a severity", func(c context.Context) error {
			return alert().Alter(c, AlertChanges{ErrorNumber: new(0), Severity: new(17)})
		}, "EXEC msdb.dbo.sp_update_alert @name = N'Disk''Full', @message_id = 0, @severity = 17"},
		{"Alert Alter database", func(c context.Context) error {
			return alert().Alter(c, AlertChanges{DatabaseName: new("App'DB")})
		}, "EXEC msdb.dbo.sp_update_alert @name = N'Disk''Full', @database_name = N'App''DB'"},
		{"Alert Alter delay renders seconds", func(c context.Context) error {
			return alert().Alter(c, AlertChanges{DelayBetweenResponses: new(90 * time.Second)})
		}, "EXEC msdb.dbo.sp_update_alert @name = N'Disk''Full', @delay_between_responses = 90"},
		{"Alert Alter notification message", func(c context.Context) error {
			return alert().Alter(c, AlertChanges{NotificationMessage: new("call o'brien")})
		}, "EXEC msdb.dbo.sp_update_alert @name = N'Disk''Full', @notification_message = N'call o''brien'"},
		{"Alert Alter category", func(c context.Context) error {
			return alert().Alter(c, AlertChanges{Category: new("Cat'1")})
		}, "EXEC msdb.dbo.sp_update_alert @name = N'Disk''Full', @category_name = N'Cat''1'"},
		{"CreateAlert", func(c context.Context) error {
			_, err := srv.CreateAlert(c, CreateAlertRequest{
				Name: "Disk'Full", Severity: 17, Enabled: true, DelayBetweenResponses: time.Minute,
				DatabaseName: "App'DB", NotificationMessage: "call o'brien", Category: "Cat'1",
			})
			return err
		}, "EXEC msdb.dbo.sp_add_alert @name = N'Disk''Full', @message_id = 0, @severity = 17, " +
			"@enabled = 1, @delay_between_responses = 60, @include_event_description_in = 0, " +
			"@database_name = N'App''DB', @notification_message = N'call o''brien', @category_name = N'Cat''1'"},
		{"Alert Notify", func(c context.Context) error {
			return alert().Notify(c, "On'Call", NotifyMethodEmail)
		}, "EXEC msdb.dbo.sp_add_notification @alert_name = N'Disk''Full', @operator_name = N'On''Call', @notification_method = 1"},
		{"CreateCategory", func(c context.Context) error {
			_, err := srv.CreateCategory(c, CreateCategoryRequest{Class: CategoryClassAlert, Name: "Disk'Space"})
			return err
		}, "EXEC msdb.dbo.sp_add_category @class = N'ALERT', @type = N'NONE', @name = N'Disk''Space'"},
		{"Category Drop", func(c context.Context) error {
			return srv.CategoryRef(CategoryClassAlert, "Disk'Space").Drop(c)
		}, "EXEC msdb.dbo.sp_delete_category @class = N'ALERT', @name = N'Disk''Space'"},
		{"Alert RemoveNotification", func(c context.Context) error {
			return alert().RemoveNotification(c, "On'Call")
		}, "EXEC msdb.dbo.sp_delete_notification @alert_name = N'Disk''Full', @operator_name = N'On''Call'"},

		// --- Operator
		{"Operator Rename", func(c context.Context) error {
			return operator().Rename(c, "On'Call 2")
		}, "EXEC msdb.dbo.sp_update_operator @name = N'On''Call', @new_name = N'On''Call 2'"},
		{"Operator Enable", func(c context.Context) error {
			return operator().Enable(c)
		}, "EXEC msdb.dbo.sp_update_operator @name = N'On''Call', @enabled = 1"},
		{"Operator Disable", func(c context.Context) error {
			return operator().Disable(c)
		}, "EXEC msdb.dbo.sp_update_operator @name = N'On''Call', @enabled = 0"},
		{"Operator Alter category", func(c context.Context) error {
			return operator().Alter(c, OperatorChanges{Category: new("Cat'1")})
		}, "EXEC msdb.dbo.sp_update_operator @name = N'On''Call', @category_name = N'Cat''1'"},
		{
			// An empty category is not an omitted parameter: sp_update_operator
			// refuses N'', so the library sends msdb's own default category
			// name instead.
			"Operator Alter sends the default category for an empty one", func(c context.Context) error {
				return operator().Alter(c, OperatorChanges{Category: new("")})
			}, "EXEC msdb.dbo.sp_update_operator @name = N'On''Call', @category_name = N'[Uncategorized]'"},
		{"CreateOperator", func(c context.Context) error {
			_, err := srv.CreateOperator(c, CreateOperatorRequest{
				Name: "On'Call", Enabled: true, EmailAddress: "o'brien@example.com", Category: "Cat'1",
			})
			return err
		}, "EXEC msdb.dbo.sp_add_operator @name = N'On''Call', @enabled = 1, " +
			"@email_address = N'o''brien@example.com', @category_name = N'Cat''1'"},
		{"Operator Drop", func(c context.Context) error {
			return operator().Drop(c)
		}, "EXEC msdb.dbo.sp_delete_operator @name = N'On''Call'"},
		{"Operator Alter email address", func(c context.Context) error {
			return operator().Alter(c, OperatorChanges{EmailAddress: new("o'brien@example.com")})
		}, "EXEC msdb.dbo.sp_update_operator @name = N'On''Call', @email_address = N'o''brien@example.com'"},

		// --- Schedule. Addressed by schedule_id, not by name: msdb allows two
		// schedules to share a name, and sp_update_schedule then refuses a
		// @name that matches more than one.
		{"CreateSchedule", func(c context.Context) error {
			_, err := srv.CreateSchedule(c, CreateScheduleRequest{
				Name: "Daily'2am", Enabled: true, FreqType: FreqDaily, FreqInterval: 1,
				FreqSubdayType: SubdayOnce, ActiveStartTime: 20000, OwnerLoginName: "CONTOSO\\o'brien",
			})
			return err
		}, addSchedule},
		{"Schedule Rename", func(c context.Context) error {
			return schedule().Rename(c, "Daily'3am")
		}, "EXEC msdb.dbo.sp_update_schedule @schedule_id = 7, @new_name = N'Daily''3am'"},
		{"Schedule Enable", func(c context.Context) error {
			return schedule().Enable(c)
		}, "EXEC msdb.dbo.sp_update_schedule @schedule_id = 7, @enabled = 1"},
		{"Schedule Disable", func(c context.Context) error {
			return schedule().Disable(c)
		}, "EXEC msdb.dbo.sp_update_schedule @schedule_id = 7, @enabled = 0"},
		{"Schedule Alter owner", func(c context.Context) error {
			return schedule().Alter(c, ScheduleChanges{OwnerLogin: new("CONTOSO\\o'brien")})
		}, "EXEC msdb.dbo.sp_update_schedule @schedule_id = 7, @owner_login_name = N'CONTOSO\\o''brien'"},
		{"Schedule Drop", func(c context.Context) error {
			return schedule().Drop(c)
		}, "EXEC msdb.dbo.sp_delete_schedule @schedule_id = 7"},
		// @schedule_id = 0 names no schedule; a Ref drops by name, as Alter
		// addresses it.
		{"Schedule Drop from a Ref addresses by name", func(c context.Context) error {
			return srv.ScheduleRef("Daily'2am").Drop(c)
		}, "EXEC msdb.dbo.sp_delete_schedule @schedule_name = N'Daily''2am'"},
		{"Schedule Alter frequency", func(c context.Context) error {
			return schedule().Alter(c, ScheduleChanges{Frequency: &ScheduleFrequency{
				FreqType: 8, FreqInterval: 2, FreqSubdayType: 4,
				FreqSubdayInterval: 30, FreqRecurrenceFactor: 1,
			}})
		}, "EXEC msdb.dbo.sp_update_schedule @schedule_id = 7, @freq_type = 8, @freq_interval = 2, @freq_subday_type = 4, @freq_subday_interval = 30, @freq_relative_interval = 0, @freq_recurrence_factor = 1"},
		{"Schedule Alter active range", func(c context.Context) error {
			return schedule().Alter(c, ScheduleChanges{Range: &ScheduleActiveRange{
				StartDate: time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC),
				EndDate:   time.Date(2026, 12, 31, 0, 0, 0, 0, time.UTC),
				StartTime: 20000, EndTime: 235959,
			}})
		}, "EXEC msdb.dbo.sp_update_schedule @schedule_id = 7, @active_start_date = 20260801, @active_end_date = 20261231, @active_start_time = 20000, @active_end_time = 235959"},
	}
	runScriptCases(t, cases)
	// AddSchedule runs its EXEC inside a batch that reads the new
	// schedule_id back, which the script leaves out.
	runBoundCases(t, cases, map[string]string{
		"Job AddSchedule": "DECLARE @schedule_id int;\n" + addJobSchedule + ", @schedule_id = @schedule_id OUTPUT;\nSELECT @schedule_id;",
		"CreateSchedule":  "DECLARE @schedule_id int;\n" + addSchedule + ", @schedule_id = @schedule_id OUTPUT;\nSELECT @schedule_id;",
	},
		cannedRow{match: "sp_add_jobschedule", cols: []string{"id"}, row: []driver.Value{int64(9)}},
		cannedRow{match: "sp_add_schedule", cols: []string{"id"}, row: []driver.Value{int64(9)}})
}

// runBoundCases runs each case against the capture driver and checks what was
// sent: the one msdb EXEC it issued carries no string literal of its own —
// every apostrophe-bearing value went as a bound argument — and, with those
// arguments bound back in, is the case's scripted statement — or sent[name],
// for a case whose real run sends more than its script. A string spliced back
// in by hand fails the first check; a value bound to the wrong parameter, the
// second. canned answers the reads a real run makes.
func runBoundCases(t *testing.T, cases []scriptCase, sent map[string]string, canned ...cannedRow) {
	t.Helper()
	for _, c := range cases {
		t.Run(c.name+" bound", func(t *testing.T) {
			want := c.want
			if w, ok := sent[c.name]; ok {
				want = w
			}
			captured.reset(canned...)
			if err := c.call(t.Context()); err != nil {
				t.Fatalf("%s: %v", c.name, err)
			}
			if n := captured.count("EXEC msdb."); n != 1 {
				t.Fatalf("%d msdb EXECs sent, want 1", n)
			}
			q, args := captured.findArgs("EXEC msdb.")
			if strings.Contains(q, "''") {
				t.Errorf("a hand-escaped literal was sent:\n%s", q)
			}
			if got := captured.findBound("EXEC msdb."); got != want {
				t.Errorf("sent, with %q bound:\n got: %q\nwant: %q", args, got, want)
			}
		})
	}
}

// TestScriptServerLevelWrites pins the server-scoped writes that reach
// neither a database nor msdb's agent tables, plus the endpoint statements.
func TestScriptServerLevelWrites(t *testing.T) {
	endpoint := func() *DatabaseMirroringEndpoint {
		return &DatabaseMirroringEndpoint{server: &Server{}, Name: "Hadr]Endpoint"}
	}

	runScriptCases(t, []scriptCase{
		{"KillSession", func(c context.Context) error {
			return (&Server{}).KillSession(c, 57)
		}, "KILL 57"},
		{"SetTraceFlag on", func(c context.Context) error {
			return (&Server{}).SetTraceFlag(c, 7745, true)
		}, "DBCC TRACEON(7745, -1) WITH NO_INFOMSGS"},
		{"SetTraceFlag off", func(c context.Context) error {
			return (&Server{}).SetTraceFlag(c, 7745, false)
		}, "DBCC TRACEOFF(7745, -1) WITH NO_INFOMSGS"},
		{"Mail.Send", func(c context.Context) error {
			_, err := (&Server{}).Mail().Send(c, MailMessage{Profile: "Prof'ile", To: "o'brien@example.com", Subject: "Sub'ject", Body: "Bo'dy"})
			return err
		}, "DECLARE @mailitem_id int;\nEXEC msdb.dbo.sp_send_dbmail @profile_name = N'Prof''ile', @recipients = N'o''brien@example.com', " +
			"@subject = N'Sub''ject', @body = N'Bo''dy', @mailitem_id = @mailitem_id OUTPUT;\nSELECT @mailitem_id AS mailitem_id;"},
		{"CreateDatabaseMirroringEndpoint", func(c context.Context) error {
			_, err := (&Server{}).CreateDatabaseMirroringEndpoint(c, CreateDatabaseMirroringEndpointRequest{
				Name: "Hadr]Endpoint", Port: 5022,
				Authentication: "CERTIFICATE [c'1]", Encryption: "REQUIRED", EncryptionAlgorithm: "AES",
			})
			return err
		}, "CREATE ENDPOINT [Hadr]]Endpoint] STATE = STARTED AS TCP (LISTENER_PORT = 5022, LISTENER_IP = ALL) FOR DATABASE_MIRRORING (AUTHENTICATION = CERTIFICATE [c'1], ENCRYPTION = REQUIRED ALGORITHM AES, ROLE = ALL)"},
		{"CreateDatabaseMirroringEndpoint defaults", func(c context.Context) error {
			_, err := (&Server{}).CreateDatabaseMirroringEndpoint(c, CreateDatabaseMirroringEndpointRequest{Name: "Hadr_Endpoint"})
			return err
		}, "CREATE ENDPOINT [Hadr_Endpoint] STATE = STARTED AS TCP (LISTENER_PORT = 5022, LISTENER_IP = ALL) FOR DATABASE_MIRRORING (AUTHENTICATION = WINDOWS NEGOTIATE, ENCRYPTION = REQUIRED, ROLE = ALL)"},
		{"Endpoint Start", func(c context.Context) error {
			return endpoint().Start(c)
		}, "ALTER ENDPOINT [Hadr]]Endpoint] STATE = STARTED"},
		{"Endpoint Stop", func(c context.Context) error {
			return endpoint().Stop(c)
		}, "ALTER ENDPOINT [Hadr]]Endpoint] STATE = STOPPED"},

		// BuildRestoreStatement's own output is pinned in backup_test.go; what
		// this adds is that the scripted path emits that statement and nothing
		// else. Restore has a second, non-scripting branch when
		// Progress is set (execWithProgress reads the driver's message
		// stream), so a change that routed every restore through it would
		// silently stop Script Changes producing anything.
		{"Restore", func(c context.Context) error {
			return (&Server{}).Restore(c, RestoreOptions{
				Database: "App'DB",
				Devices:  []BackupTarget{DiskTarget(`C:\bak\a'1.bak`)},
				Replace:  true,
			})
		}, "USE [master];\nRESTORE DATABASE [App'DB]\nFROM DISK = N'C:\\bak\\a''1.bak'\nWITH REPLACE"},
	})
}

// TestScriptedEndpointCreateReturnsAHandle pins what a scripted create hands
// back. The real path re-reads the endpoint it just made; under WithScript
// nothing was created, so there is nothing to read.
//
// It used to answer (nil, nil) there, on the grounds that a handle would have
// the caller address an endpoint the server does not have — which is exactly
// what a scripted caller is for: the GRANT CONNECTs and ALTERs that follow the
// CREATE have to be collected too, and a nil handle leaves nothing to collect
// them against. goSSMS's New Endpoint wizard bailed out on the nil and emitted
// its CREATEs without a single GRANT. Every other scripted create in this
// library hands back a name-only handle (see createdObject); this one
// was the outlier.
//
// The handle carries what the CREATE statement itself determines and nothing
// else — no ConnectionAuth, no Owner.
func TestScriptedEndpointCreateReturnsAHandle(t *testing.T) {
	ctx, script := WithScript(context.Background())
	srv := &Server{}
	ep, err := srv.CreateDatabaseMirroringEndpoint(ctx, CreateDatabaseMirroringEndpointRequest{
		Name:                "Hadr_Endpoint",
		EncryptionAlgorithm: "aes",
	})
	if err != nil {
		t.Fatalf("CreateDatabaseMirroringEndpoint under WithScript: %v", err)
	}
	if ep == nil {
		t.Fatal("endpoint = nil under WithScript, want a handle to script the next step against")
	}
	if ep.Server() != srv {
		t.Error("the handle does not carry the server it was created on")
	}
	want := DatabaseMirroringEndpoint{
		Name: "Hadr_Endpoint", Port: 5022, State: "STARTED", Role: "ALL",
		IsEncryptionEnabled: true, EncryptionAlgorithm: "AES",
	}
	got := *ep
	got.server = nil
	if got != want {
		t.Errorf("handle = %+v\nwant       %+v", got, want)
	}
	if len(script.Statements()) != 1 {
		t.Fatalf("Statements = %d, want 1", len(script.Statements()))
	}

	// The point of the handle: the next step scripts against it instead of
	// being skipped.
	if err := ep.GrantConnect(ctx, "ubusql2_login"); err != nil {
		t.Fatalf("GrantConnect under WithScript: %v", err)
	}
	if len(script.Statements()) != 2 {
		t.Fatalf("Statements = %d after the grant, want 2", len(script.Statements()))
	}
	if want := "GRANT CONNECT ON ENDPOINT::[Hadr_Endpoint] TO [ubusql2_login]"; script.Statements()[1] != want {
		t.Errorf("grant = %q, want %q", script.Statements()[1], want)
	}
}

// A DISABLED spec is the one case where the endpoint is not encrypted, and the
// handle has to say so rather than inheriting the REQUIRED default's answer.
func TestScriptedEndpointHandleReflectsDisabledEncryption(t *testing.T) {
	ctx, _ := WithScript(context.Background())
	ep, err := (&Server{}).CreateDatabaseMirroringEndpoint(ctx, CreateDatabaseMirroringEndpointRequest{
		Name: "EP", Port: 7022, Role: "partner", Encryption: "disabled",
	})
	if err != nil {
		t.Fatalf("under WithScript: %v", err)
	}
	if ep.IsEncryptionEnabled {
		t.Error("IsEncryptionEnabled = true for ENCRYPTION = DISABLED")
	}
	if ep.Port != 7022 || ep.Role != "PARTNER" {
		t.Errorf("handle = %+v, want port 7022 and role PARTNER", *ep)
	}
}
