package gosmo

import (
	"context"
	"testing"
	"time"
)

// TestScriptAgentWrites pins the msdb stored-procedure calls behind the SQL
// Agent writes. See script_write_common_test.go.
//
// Everything here reaches msdb through sp_* parameters, which are literals —
// a job or operator name goes into the statement as N'...' with no brackets
// anywhere, so escapeSingle is the whole defense and an apostrophe in a name
// is the ordinary case, not an exotic one.
func TestScriptAgentWrites(t *testing.T) {
	job := func() *Job { return &Job{server: &Server{}, Name: "Nightly'Run"} }
	alert := func() *Alert { return &Alert{server: &Server{}, Name: "Disk'Full"} }
	operator := func() *Operator { return &Operator{server: &Server{}, Name: "On'Call"} }
	schedule := func() *Schedule { return &Schedule{server: &Server{}, ID: 7, Name: "Daily'2am"} }

	runScriptCases(t, []scriptCase{
		// --- Job
		{"Job SetDescription", func(c context.Context) error {
			return job().SetDescriptionContext(c, "runs at 2'am")
		}, "EXEC msdb.dbo.sp_update_job @job_name = N'Nightly''Run', @description = N'runs at 2''am'"},
		{"Job SetStartStep", func(c context.Context) error {
			return job().SetStartStepContext(c, 2)
		}, "EXEC msdb.dbo.sp_update_job @job_name = N'Nightly''Run', @start_step_id = 2"},
		{"Job SetEmailNotify", func(c context.Context) error {
			return job().SetEmailNotifyContext(c, "On'Call", NotifyOnFailure)
		}, "EXEC msdb.dbo.sp_update_job @job_name = N'Nightly''Run', @notify_level_email = 2, @notify_email_operator_name = N'On''Call'"},
		{"Job SetDeleteLevel", func(c context.Context) error {
			return job().SetDeleteLevelContext(c, NotifyOnFailure)
		}, "EXEC msdb.dbo.sp_update_job @job_name = N'Nightly''Run', @delete_level = 2"},
		{"Job AddSchedule", func(c context.Context) error {
			return job().AddScheduleContext(c, JobScheduleRequest{
				Name: "Daily'2am", Enabled: true,
				FreqType: 4, FreqInterval: 1, FreqSubdayType: 1,
				ActiveStartTime: 20000,
			})
		}, "EXEC msdb.dbo.sp_add_jobschedule @job_name = N'Nightly''Run', @name = N'Daily''2am', @enabled = 1, @freq_type = 4, @freq_interval = 1, @freq_subday_type = 1, @freq_subday_interval = 0, @active_start_time = 20000, @active_end_time = 0"},
		{"Job DetachSchedule", func(c context.Context) error {
			return job().DetachScheduleContext(c, "Daily'2am")
		}, "EXEC msdb.dbo.sp_detach_schedule @job_name = N'Nightly''Run', @schedule_name = N'Daily''2am'"},
		{"JobStep Delete", func(c context.Context) error {
			return (&JobStep{job: job(), StepID: 3}).DeleteContext(c)
		}, "EXEC msdb.dbo.sp_delete_jobstep @job_name = N'Nightly''Run', @step_id = 3"},

		// --- Alert
		{"Alert SetTrigger on an error number", func(c context.Context) error {
			return alert().SetTriggerContext(c, 823, 0)
		}, "EXEC msdb.dbo.sp_update_alert @name = N'Disk''Full', @message_id = 823, @severity = 0"},
		{"Alert SetTrigger on a severity", func(c context.Context) error {
			return alert().SetTriggerContext(c, 0, 17)
		}, "EXEC msdb.dbo.sp_update_alert @name = N'Disk''Full', @message_id = 0, @severity = 17"},
		{"Alert SetDatabase", func(c context.Context) error {
			return alert().SetDatabaseContext(c, "App'DB")
		}, "EXEC msdb.dbo.sp_update_alert @name = N'Disk''Full', @database_name = N'App''DB'"},
		{"Alert SetDelay renders seconds", func(c context.Context) error {
			return alert().SetDelayContext(c, 90*time.Second)
		}, "EXEC msdb.dbo.sp_update_alert @name = N'Disk''Full', @delay_between_responses = 90"},
		{"Alert SetNotificationMessage", func(c context.Context) error {
			return alert().SetNotificationMessageContext(c, "call o'brien")
		}, "EXEC msdb.dbo.sp_update_alert @name = N'Disk''Full', @notification_message = N'call o''brien'"},
		{"Alert SetCategory", func(c context.Context) error {
			return alert().SetCategoryContext(c, "Cat'1")
		}, "EXEC msdb.dbo.sp_update_alert @name = N'Disk''Full', @category_name = N'Cat''1'"},
		{"Alert RemoveNotify", func(c context.Context) error {
			return alert().RemoveNotifyContext(c, "On'Call")
		}, "EXEC msdb.dbo.sp_delete_notification @alert_name = N'Disk''Full', @operator_name = N'On''Call'"},

		// --- Operator
		{"Operator SetEmailAddress", func(c context.Context) error {
			return operator().SetEmailAddressContext(c, "o'brien@example.com")
		}, "EXEC msdb.dbo.sp_update_operator @name = N'On''Call', @email_address = N'o''brien@example.com'"},

		// --- Schedule. Addressed by schedule_id, not by name: msdb allows two
		// schedules to share a name, and sp_update_schedule then refuses a
		// @name that matches more than one.
		{"Schedule SetFrequency", func(c context.Context) error {
			return schedule().SetFrequencyContext(c, ScheduleFrequency{
				FreqType: 8, FreqInterval: 2, FreqSubdayType: 4,
				FreqSubdayInterval: 30, FreqRecurrenceFactor: 1,
			})
		}, "EXEC msdb.dbo.sp_update_schedule @schedule_id = 7, @freq_type = 8, @freq_interval = 2, @freq_subday_type = 4, @freq_subday_interval = 30, @freq_relative_interval = 0, @freq_recurrence_factor = 1"},
		{"Schedule SetActiveRange", func(c context.Context) error {
			return schedule().SetActiveRangeContext(c,
				time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC),
				time.Date(2026, 12, 31, 0, 0, 0, 0, time.UTC),
				20000, 235959)
		}, "EXEC msdb.dbo.sp_update_schedule @schedule_id = 7, @active_start_date = 20260801, @active_end_date = 20261231, @active_start_time = 20000, @active_end_time = 235959"},
	})
}

// TestScriptServerLevelWrites pins the two server-scoped writes that reach
// neither a database nor msdb's agent tables, plus the endpoint statements.
func TestScriptServerLevelWrites(t *testing.T) {
	endpoint := func() *DatabaseMirroringEndpoint {
		return &DatabaseMirroringEndpoint{server: &Server{}, Name: "Hadr]Endpoint"}
	}

	runScriptCases(t, []scriptCase{
		{"KillSession", func(c context.Context) error {
			return (&Server{}).KillSessionContext(c, 57)
		}, "KILL 57"},
		{"SendMail", func(c context.Context) error {
			return (&Server{}).SendMailContext(c, "Prof'ile", "o'brien@example.com", "Sub'ject", "Bo'dy")
		}, "EXEC msdb.dbo.sp_send_dbmail @profile_name = N'Prof''ile', @recipients = N'o''brien@example.com', @subject = N'Sub''ject', @body = N'Bo''dy'"},
		{"CreateDatabaseMirroringEndpoint", func(c context.Context) error {
			_, err := (&Server{}).CreateDatabaseMirroringEndpointContext(c, EndpointSpec{
				Name: "Hadr]Endpoint", Port: 5022,
				Authentication: "CERTIFICATE [c'1]", Encryption: "REQUIRED", EncryptionAlgorithm: "AES",
			})
			return err
		}, "CREATE ENDPOINT [Hadr]]Endpoint] STATE = STARTED AS TCP (LISTENER_PORT = 5022, LISTENER_IP = ALL) FOR DATABASE_MIRRORING (AUTHENTICATION = CERTIFICATE [c'1], ENCRYPTION = REQUIRED ALGORITHM AES, ROLE = ALL)"},
		{"CreateDatabaseMirroringEndpoint defaults", func(c context.Context) error {
			_, err := (&Server{}).CreateDatabaseMirroringEndpointContext(c, EndpointSpec{Name: "Hadr_Endpoint"})
			return err
		}, "CREATE ENDPOINT [Hadr_Endpoint] STATE = STARTED AS TCP (LISTENER_PORT = 5022, LISTENER_IP = ALL) FOR DATABASE_MIRRORING (AUTHENTICATION = WINDOWS NEGOTIATE, ENCRYPTION = REQUIRED, ROLE = ALL)"},
		{"Endpoint Start", func(c context.Context) error {
			return endpoint().StartContext(c)
		}, "ALTER ENDPOINT [Hadr]]Endpoint] STATE = STARTED"},
		{"Endpoint Stop", func(c context.Context) error {
			return endpoint().StopContext(c)
		}, "ALTER ENDPOINT [Hadr]]Endpoint] STATE = STOPPED"},

		// BuildRestoreStatement's own output is pinned in backup_test.go; what
		// this adds is that the scripted path emits that statement and nothing
		// else. RestoreContext has a second, non-scripting branch when
		// Progress is set (execWithProgress reads the driver's message
		// stream), so a change that routed every restore through it would
		// silently stop Script Changes producing anything.
		{"Restore", func(c context.Context) error {
			return (&Server{}).RestoreContext(c, RestoreOptions{
				Database: "App'DB",
				Devices:  []string{`C:\bak\a'1.bak`},
				Replace:  true,
			})
		}, "RESTORE DATABASE [App'DB]\nFROM DISK = N'C:\\bak\\a''1.bak'\nWITH REPLACE"},
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
// library hands back a name-only handle (see CreateScheduleContext); this one
// was the outlier.
//
// The handle carries what the CREATE statement itself determines and nothing
// else — no ConnectionAuth, no Owner.
func TestScriptedEndpointCreateReturnsAHandle(t *testing.T) {
	ctx, script := WithScript(context.Background())
	srv := &Server{}
	ep, err := srv.CreateDatabaseMirroringEndpointContext(ctx, EndpointSpec{
		Name:                "Hadr_Endpoint",
		EncryptionAlgorithm: "aes",
	})
	if err != nil {
		t.Fatalf("CreateDatabaseMirroringEndpointContext under WithScript: %v", err)
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
	if len(script.Statements) != 1 {
		t.Fatalf("Statements = %d, want 1", len(script.Statements))
	}

	// The point of the handle: the next step scripts against it instead of
	// being skipped.
	if err := ep.GrantConnectContext(ctx, "ubusql2_login"); err != nil {
		t.Fatalf("GrantConnectContext under WithScript: %v", err)
	}
	if len(script.Statements) != 2 {
		t.Fatalf("Statements = %d after the grant, want 2", len(script.Statements))
	}
	if want := "GRANT CONNECT ON ENDPOINT::[Hadr_Endpoint] TO [ubusql2_login]"; script.Statements[1] != want {
		t.Errorf("grant = %q, want %q", script.Statements[1], want)
	}
}

// A DISABLED spec is the one case where the endpoint is not encrypted, and the
// handle has to say so rather than inheriting the REQUIRED default's answer.
func TestScriptedEndpointHandleReflectsDisabledEncryption(t *testing.T) {
	ctx, _ := WithScript(context.Background())
	ep, err := (&Server{}).CreateDatabaseMirroringEndpointContext(ctx, EndpointSpec{
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
