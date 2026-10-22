package gosmo

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"io"
	"slices"
	"strings"
	"testing"
)

// notifDriver answers any query with notifRows and records the statement and
// its arguments, so one fake covers the three readers notificationRows backs.
type notifDriver struct{}

func (notifDriver) Open(string) (driver.Conn, error) { return notifConn{}, nil }

func init() { sql.Register("notifrows", notifDriver{}) }

var (
	notifQuery string
	notifArgs  []any
)

type notifConn struct{}

func (notifConn) Prepare(string) (driver.Stmt, error) { return nil, driver.ErrSkip }
func (notifConn) Close() error                        { return nil }
func (notifConn) Begin() (driver.Tx, error)           { return nil, driver.ErrSkip }
func (notifConn) QueryContext(_ context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	notifQuery = q
	notifArgs = nil
	for _, a := range args {
		notifArgs = append(notifArgs, a.Value)
	}
	return &notifRows{rows: [][]driver.Value{{"first", int64(1)}, {"second", int64(4)}}}, nil
}

type notifRows struct{ rows [][]driver.Value }

func (r *notifRows) Columns() []string { return []string{"name", "code"} }
func (r *notifRows) Close() error      { return nil }
func (r *notifRows) Next(dest []driver.Value) error {
	if len(r.rows) == 0 {
		return io.EOF
	}
	copy(dest, r.rows[0])
	r.rows = r.rows[1:]
	return nil
}

// TestNotificationReadersMapRows pins the three "who notifies whom" readers
// that share notificationRows: each keys its own msdb table on the handle's
// id, and maps the name and code columns into its own row type.
func TestNotificationReadersMapRows(t *testing.T) {
	db, err := sql.Open("notifrows", "")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()
	s := &Server{db: db}
	ctx := context.Background()

	check := func(t *testing.T, table, key string) {
		t.Helper()
		if !strings.Contains(notifQuery, table) || !strings.Contains(notifQuery, key+" = @p1") {
			t.Errorf("query = %q; want %s keyed on %s = @p1", notifQuery, table, key)
		}
		if !slices.Equal(notifArgs, []any{int64(7)}) {
			t.Errorf("args = %v; want [7]", notifArgs)
		}
	}

	t.Run("Alert.Notifications", func(t *testing.T) {
		got, err := (&Alert{server: s, ID: 7, Name: "a"}).Notifications(ctx)
		if err != nil {
			t.Fatal(err)
		}
		check(t, "sysoperators", "n.alert_id")
		want := []AlertNotification{{"first", NotificationMethod(1)}, {"second", NotificationMethod(4)}}
		if len(got) != 2 || *got[0] != want[0] || *got[1] != want[1] {
			t.Errorf("got %v; want %v", got, want)
		}
	})
	t.Run("Operator.NotifyingAlerts", func(t *testing.T) {
		got, err := (&Operator{server: s, ID: 7, Name: "o"}).NotifyingAlerts(ctx)
		if err != nil {
			t.Fatal(err)
		}
		check(t, "sysalerts", "n.operator_id")
		want := []OperatorAlert{{"first", NotificationMethod(1)}, {"second", NotificationMethod(4)}}
		if len(got) != 2 || *got[0] != want[0] || *got[1] != want[1] {
			t.Errorf("got %v; want %v", got, want)
		}
	})
	t.Run("Operator.NotifyingJobs", func(t *testing.T) {
		got, err := (&Operator{server: s, ID: 7, Name: "o"}).NotifyingJobs(ctx)
		if err != nil {
			t.Fatal(err)
		}
		check(t, "sysjobs", "notify_email_operator_id")
		want := []OperatorJob{{"first", NotifyLevel(1)}, {"second", NotifyLevel(4)}}
		if len(got) != 2 || *got[0] != want[0] || *got[1] != want[1] {
			t.Errorf("got %v; want %v", got, want)
		}
	})
}
