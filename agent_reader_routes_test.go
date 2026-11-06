package gosmo_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	mssql "github.com/microsoft/go-mssqldb"
	"github.com/radix29/gosmo"
	"github.com/radix29/gosmo/gosmotest"
)

// The Agent reads a SQLAgentReaderRole or SQLAgentUserRole login has a route
// for — operators through sp_help_operator, a job's alerts through
// sp_get_job_alerts — and the ones it has none for (alerts), which must
// refuse with a classified permission error.

// operatorRow is one row shaped like operatorColumns: id, name, enabled, the
// three addresses, the category, then the three last-notified date/time
// pairs.
func operatorRow(id int64, name, category string) []any {
	return []any{id, name, true, name + "@example.invalid", "", "", category,
		int64(20261105), int64(93000), int64(0), int64(0), int64(0), int64(0)}
}

func TestOperatorsReadThroughSpHelpOperatorBelowSysadmin(t *testing.T) {
	f := gosmotest.New(t)
	f.On("sp_help_operator", gosmotest.Rows(
		operatorRow(1, "alpha", "[Uncategorized]"),
		operatorRow(2, "beta", ""),
	))
	ops, err := f.Server().Operators(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(ops) != 2 || ops[0].Name != "alpha" || ops[0].Category != "[Uncategorized]" ||
		ops[0].EmailAddress != "alpha@example.invalid" || !ops[0].Enabled || ops[0].LastEmailDate.IsZero() ||
		ops[1].ID != 2 || !ops[1].LastPagerDate.IsZero() {
		t.Errorf("operators = %+v %+v", ops[0], ops[1])
	}

	q := f.Queries()[len(f.Queries())-1].Text
	// The batch decides on the server: sysoperators for sysadmin, the
	// procedure for everyone else, each with the caller's ORDER BY.
	for _, want := range []string{"IS_SRVROLEMEMBER(N'sysadmin')", "FROM   msdb.dbo.sysoperators o",
		"INSERT INTO @op EXECUTE msdb.dbo.sp_help_operator", "FROM   @op o"} {
		if !strings.Contains(q, want) {
			t.Errorf("query lacks %q:\n%s", want, q)
		}
	}
	if n := strings.Count(q, "ORDER  BY o.name"); n != 2 {
		t.Errorf("ORDER BY in %d branches, want 2:\n%s", n, q)
	}
}

func TestOperatorByNameFiltersBothBranches(t *testing.T) {
	f := gosmotest.New(t)
	f.On("sp_help_operator", gosmotest.Rows(operatorRow(7, "ops", "")))
	o, err := f.Server().OperatorByName(context.Background(), "ops")
	if err != nil {
		t.Fatal(err)
	}
	if o.ID != 7 || o.Name != "ops" {
		t.Errorf("operator = %+v", o)
	}
	q := f.Queries()[len(f.Queries())-1]
	if n := strings.Count(q.Text, "WHERE  o.name = @p1"); n != 2 {
		t.Errorf("name filter in %d branches, want 2:\n%s", n, q.Text)
	}
	if q.Arg(0) != "ops" {
		t.Errorf("@p1 = %v, want ops", q.Arg(0))
	}

	f2 := gosmotest.New(t)
	f2.On("sp_help_operator", gosmotest.Rows())
	if _, err := f2.Server().OperatorByName(context.Background(), "gone"); !errors.Is(err, gosmo.ErrNotFound) {
		t.Errorf("missing operator: err = %v, want ErrNotFound", err)
	}
}

func TestJobAlertsReadThroughSpGetJobAlerts(t *testing.T) {
	const id = "4678FC6B-E692-4FF3-BBD5-108EB31300D4"
	f := gosmotest.New(t)
	f.On("FROM msdb.dbo.sysjobs_view WHERE name = @p1", gosmotest.Rows([]any{id}))
	// The procedure's columns: id, name, enabled (tinyint), type — 1 event
	// (WMI included, see JobAlert), 2 performance condition, 3 WMI with a
	// performance condition. Unordered, as the procedure returns them.
	f.On("sp_get_job_alerts", gosmotest.Rows(
		[]any{int64(9), "zeta", int64(1), int64(1)},
		[]any{int64(4), "alpha", int64(0), int64(2)},
		[]any{int64(5), "mid", int64(1), int64(3)},
	))
	alerts, err := f.Server().JobRef("nightly").Alerts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	type got struct {
		id      int
		name    string
		enabled bool
		perf    bool
	}
	want := []got{{4, "alpha", false, true}, {5, "mid", true, true}, {9, "zeta", true, false}}
	if len(alerts) != len(want) {
		t.Fatalf("alerts = %d, want %d", len(alerts), len(want))
	}
	for i, a := range alerts {
		if g := (got{a.ID, a.Name, a.Enabled, a.IsPerformanceAlert}); g != want[i] {
			t.Errorf("alert %d = %+v, want %+v", i, g, want[i])
		}
	}
	q := f.Queries()[len(f.Queries())-1]
	if !strings.Contains(q.Text, "EXECUTE msdb.dbo.sp_get_job_alerts @job_id = @job") || q.Arg(0) != id {
		t.Errorf("query %q, @p1 %v; want sp_get_job_alerts keyed on the job's id", q.Text, q.Arg(0))
	}
	// Nothing reads sysalerts: that is SQLAgentOperatorRole's.
	for _, q := range f.Queries() {
		if strings.Contains(q.Text, "sysalerts") {
			t.Errorf("Job.Alerts read sysalerts: %s", q.Text)
		}
	}
}

func TestJobAlertsOfAMissingJobIsNotFound(t *testing.T) {
	f := gosmotest.New(t)
	f.On("FROM msdb.dbo.sysjobs_view WHERE name = @p1", gosmotest.Rows())
	if _, err := f.Server().JobRef("gone").Alerts(context.Background()); !errors.Is(err, gosmo.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

// TestAlertReadsBelowOperatorRoleAreClassifiedRefusals: a Reader's sysalerts
// and sysnotifications reads are Msg 229, and every alert read must hand that
// back as a permission refusal a caller can test for, not a generic failure.
func TestAlertReadsBelowOperatorRoleAreClassifiedRefusals(t *testing.T) {
	denied := func(obj string) gosmotest.Responder {
		return gosmotest.Fail(mssql.Error{Number: 229, Class: 14,
			Message: "The SELECT permission was denied on the object '" + obj + "', database 'msdb', schema 'dbo'."})
	}
	f := gosmotest.New(t)
	f.On("msdb.dbo.sysalerts", denied("sysalerts"))
	f.On("msdb.dbo.sysnotifications", denied("sysnotifications"))
	s := f.Server()
	ctx := context.Background()

	_, errAlerts := s.Alerts(ctx)
	_, errEvent := s.EventAlerts(ctx)
	_, errByName := s.AlertByName(ctx, "a")
	al := s.AlertRef("a")
	al.ID = 3 // as Alerts would have read it
	_, errNotif := al.Notifications(ctx)
	for name, err := range map[string]error{
		"Alerts": errAlerts, "EventAlerts": errEvent, "AlertByName": errByName, "Alert.Notifications": errNotif,
	} {
		if !gosmo.IsPermissionDenied(err) {
			t.Errorf("%s: err = %v, want IsPermissionDenied", name, err)
		}
		if errors.Is(err, gosmo.ErrNotFound) {
			t.Errorf("%s: a refusal must not read as not found: %v", name, err)
		}
	}
}
