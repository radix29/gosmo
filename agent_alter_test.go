package gosmo

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"
)

// TestScriptAgentAlterWrites pins the batched sp_update_* calls the Alter
// methods emit. See script_write_common_test.go for why each case asserts
// the whole statement and feeds an apostrophe through every parameter that
// reaches it.
//
// The property these cases exist for is the one the per-property setters
// cannot have: a Changes struct setting three fields produces exactly one
// statement naming exactly those three parameters, and no others.
func TestScriptAgentAlterWrites(t *testing.T) {
	job := func() *Job { return &Job{server: &Server{}, Name: "Nightly'Run"} }
	alert := func() *Alert { return &Alert{server: &Server{}, Name: "Disk'Full"} }
	operator := func() *Operator { return &Operator{server: &Server{}, Name: "On'Call"} }
	schedule := func() *Schedule { return &Schedule{server: &Server{}, ID: 7, Name: "Daily'2am"} }

	runScriptCases(t, []scriptCase{
		{"Job Alter, three fields", func(c context.Context) error {
			return job().Alter(c, JobChanges{
				Description: Ptr("runs at 2'am"),
				OwnerLogin:  Ptr("sa'm"),
				Enabled:     Ptr(true),
			})
		}, "EXEC msdb.dbo.sp_update_job @job_name = N'Nightly''Run', @description = N'runs at 2''am', @owner_login_name = N'sa''m', @enabled = 1"},
		{"Job Alter, every field", func(c context.Context) error {
			return job().Alter(c, JobChanges{
				Name:                    Ptr("New'Name"),
				Description:             Ptr("d'esc"),
				Category:                Ptr("Cat'1"),
				OwnerLogin:              Ptr("sa'm"),
				Enabled:                 Ptr(false),
				StartStepID:             Ptr(2),
				NotifyLevelEmail:        Ptr(NotifyOnFailure),
				NotifyEmailOperatorName: Ptr("On'Call"),
				DeleteLevel:             Ptr(NotifyNever),
			})
		}, "EXEC msdb.dbo.sp_update_job @job_name = N'Nightly''Run', @new_name = N'New''Name', @description = N'd''esc', @category_name = N'Cat''1', @owner_login_name = N'sa''m', @enabled = 0, @start_step_id = 2, @notify_level_email = 2, @notify_email_operator_name = N'On''Call', @delete_level = 0"},
		{"Job Alter sends a zero value it was given", func(c context.Context) error {
			return job().Alter(c, JobChanges{Description: Ptr(""), StartStepID: Ptr(0)})
		}, "EXEC msdb.dbo.sp_update_job @job_name = N'Nightly''Run', @description = N'', @start_step_id = 0"},

		{"Alert Alter, trigger and delay", func(c context.Context) error {
			return alert().Alter(c, AlertChanges{
				ErrorNumber:           Ptr(823),
				Severity:              Ptr(0),
				DelayBetweenResponses: Ptr(90 * time.Second),
			})
		}, "EXEC msdb.dbo.sp_update_alert @name = N'Disk''Full', @message_id = 823, @severity = 0, @delay_between_responses = 90"},
		{"Alert Alter, every field", func(c context.Context) error {
			return alert().Alter(c, AlertChanges{
				Name:                      Ptr("New'Alert"),
				Enabled:                   Ptr(true),
				ErrorNumber:               Ptr(0),
				Severity:                  Ptr(17),
				DatabaseName:              Ptr("App'DB"),
				DelayBetweenResponses:     Ptr(2 * time.Minute),
				NotificationMessage:       Ptr("call o'brien"),
				IncludeEventDescriptionIn: Ptr(1),
				Category:                  Ptr("Cat'1"),
				JobName:                   Ptr("Nightly'Run"),
			})
		}, "EXEC msdb.dbo.sp_update_alert @name = N'Disk''Full', @new_name = N'New''Alert', @enabled = 1, @message_id = 0, @severity = 17, @database_name = N'App''DB', @delay_between_responses = 120, @notification_message = N'call o''brien', @include_event_description_in = 1, @category_name = N'Cat''1', @job_name = N'Nightly''Run'"},
		{"Alert Alter clears the category as [Uncategorized]", func(c context.Context) error {
			return alert().Alter(c, AlertChanges{Category: Ptr("")})
		}, "EXEC msdb.dbo.sp_update_alert @name = N'Disk''Full', @category_name = N'[Uncategorized]'"},
		{"Alert Alter clears the job response with an empty name", func(c context.Context) error {
			return alert().Alter(c, AlertChanges{JobName: Ptr("")})
		}, "EXEC msdb.dbo.sp_update_alert @name = N'Disk''Full', @job_name = N''"},

		{"Operator Alter, every field", func(c context.Context) error {
			return operator().Alter(c, OperatorChanges{
				Name:           Ptr("New'Op"),
				Enabled:        Ptr(false),
				EmailAddress:   Ptr("o'brien@example.com"),
				PagerAddress:   Ptr("pager'1"),
				NetSendAddress: Ptr("host'1"),
				Category:       Ptr("Cat'1"),
			})
		}, "EXEC msdb.dbo.sp_update_operator @name = N'On''Call', @new_name = N'New''Op', @enabled = 0, @email_address = N'o''brien@example.com', @pager_address = N'pager''1', @netsend_address = N'host''1', @category_name = N'Cat''1'"},

		{"Schedule Alter, frequency and range in one call", func(c context.Context) error {
			return schedule().Alter(c, ScheduleChanges{
				Enabled: Ptr(true),
				Frequency: &ScheduleFrequency{
					FreqType: FreqWeekly, FreqInterval: 2, FreqSubdayType: SubdayMinutes,
					FreqSubdayInterval: 30, FreqRecurrenceFactor: 1,
				},
				Range: &ScheduleActiveRange{
					StartDate: time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC),
					EndDate:   time.Date(2026, 12, 31, 0, 0, 0, 0, time.UTC),
					StartTime: 20000, EndTime: 235959,
				},
				OwnerLogin: Ptr("sa'm"),
			})
		}, "EXEC msdb.dbo.sp_update_schedule @schedule_id = 7, @enabled = 1, @freq_type = 8, @freq_interval = 2, @freq_subday_type = 4, @freq_subday_interval = 30, @freq_relative_interval = 0, @freq_recurrence_factor = 1, @active_start_date = 20260801, @active_end_date = 20261231, @active_start_time = 20000, @active_end_time = 235959, @owner_login_name = N'sa''m'"},
		{"Schedule Alter renders no end date as the sentinel", func(c context.Context) error {
			return schedule().Alter(c, ScheduleChanges{
				Range: &ScheduleActiveRange{StartDate: time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)},
			})
		}, "EXEC msdb.dbo.sp_update_schedule @schedule_id = 7, @active_start_date = 20260801, @active_end_date = 99991231, @active_start_time = 0, @active_end_time = 0"},
		// A ScheduleRef carries no ID, and the per-property setters key on
		// one — @schedule_id = 0 names no schedule. The batched form falls
		// back to @name, which is what makes it usable from a Ref.
		{"Schedule Alter from a Ref addresses by name", func(c context.Context) error {
			return (&Server{}).ScheduleRef("Daily'2am").Alter(c, ScheduleChanges{Enabled: Ptr(false)})
		}, "EXEC msdb.dbo.sp_update_schedule @name = N'Daily''2am', @enabled = 0"},
	})
}

// TestAgentAlterEmptyChangesIssuesNothing pins the other half of the
// contract: an empty Changes emits no statement at all, rather than a
// parameterless sp_update_* that msdb rejects. A Properties dialog that
// applies with nothing edited takes this path.
func TestAgentAlterEmptyChangesIssuesNothing(t *testing.T) {
	cases := []struct {
		name string
		call func(context.Context) error
	}{
		{"Job", func(c context.Context) error {
			return (&Job{server: &Server{}, Name: "J"}).Alter(c, JobChanges{})
		}},
		{"Alert", func(c context.Context) error {
			return (&Alert{server: &Server{}, Name: "A"}).Alter(c, AlertChanges{})
		}},
		{"Operator", func(c context.Context) error {
			return (&Operator{server: &Server{}, Name: "O"}).Alter(c, OperatorChanges{})
		}},
		{"Schedule", func(c context.Context) error {
			return (&Schedule{server: &Server{}, ID: 7, Name: "S"}).Alter(c, ScheduleChanges{})
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ctx, script := WithScript(context.Background())
			if err := c.call(ctx); err != nil {
				t.Fatalf("empty Alter: %v", err)
			}
			if len(script.Statements()) != 0 {
				t.Errorf("Statements = %d, want 0:\n%s", len(script.Statements()),
					strings.Join(script.Statements(), "\n---\n"))
			}
		})
	}
}

// TestAgentAlterMirrorsOnlyWhatItSet pins the setIfApplied discipline for
// the batched form: a field left nil must not be written back onto the
// receiver, and under WithScript nothing at all must be, since nothing ran.
func TestAgentAlterMirrorsOnlyWhatItSet(t *testing.T) {
	// Under WithScript no statement executed, so no field may move —
	// including the name every subsequent statement would be built from.
	j := &Job{server: &Server{}, Name: "Nightly'Run", Description: "old", Category: "Cat"}
	ctx, _ := WithScript(context.Background())
	if err := j.Alter(ctx, JobChanges{Name: Ptr("New"), Description: Ptr("new")}); err != nil {
		t.Fatalf("Alter under WithScript: %v", err)
	}
	if j.Name != "Nightly'Run" || j.Description != "old" {
		t.Errorf("scripted Alter mutated the receiver: Name=%q Description=%q", j.Name, j.Description)
	}

	// Executed for real against the capture driver, only the fields that
	// were set move. The statement itself is pinned above; what is asserted
	// here is the mirroring.
	db, err := sql.Open("capture", "")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	captured.reset()
	a := &Alert{server: &Server{db: db}, Name: "Disk'Full", DatabaseName: "keep", Severity: 17}
	if err := a.Alter(context.Background(), AlertChanges{NotificationMessage: Ptr("msg")}); err != nil {
		t.Fatalf("Alter: %v", err)
	}
	if a.NotificationMessage != "msg" {
		t.Errorf("NotificationMessage = %q, want %q", a.NotificationMessage, "msg")
	}
	if a.DatabaseName != "keep" || a.Severity != 17 {
		t.Errorf("Alter touched a field it was not given: DatabaseName=%q Severity=%d",
			a.DatabaseName, a.Severity)
	}
}
