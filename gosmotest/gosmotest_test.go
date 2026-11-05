package gosmotest_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	mssql "github.com/microsoft/go-mssqldb"
	"github.com/radix29/gosmo"
	"github.com/radix29/gosmo/gosmotest"
)

var created = time.Date(2026, 3, 14, 9, 12, 44, 0, time.UTC)

func objectRow(id int, schema, name, typ, desc string) []any {
	return []any{id, schema, name, typ, desc, "", "", created, created, false}
}

func TestConnectReadsInfo(t *testing.T) {
	f := gosmotest.New(t)
	f.Info.ProductVersion = "13.0.6300.2"
	f.Info.HADREnabled = true
	s := f.Server()
	info := s.Info()
	if info.VersionMajor != 13 || info.Login != "sa" || info.Platform != "Linux" || info.LogicalCPUCount != 8 || !info.IsHADREnabled {
		t.Errorf("info %+v", info)
	}
	if f.Server() != s {
		t.Error("Server made a second server")
	}
}

// A database read goes as [db].sys.sp_executesql; the rule sees the inner
// query, the database and the inner arguments.
func TestDatabaseReadIsUnwrapped(t *testing.T) {
	f := gosmotest.New(t)
	f.On("FROM   sys.objects o", func(q gosmotest.Query) gosmotest.Result {
		if q.Database != "a]pp" || q.Arg(0) != "Sales" || q.Arg(1) != "Customer" {
			return gosmotest.Result{}
		}
		return gosmotest.Result{Sets: []gosmotest.Set{{Rows: [][]any{objectRow(1, "Sales", "Customer", "U", "USER_TABLE")}}}}
	})
	o, err := f.Server().DatabaseRef("a]pp").ObjectByName(context.Background(), "Sales", "Customer")
	if err != nil || o.ObjectID != 1 || o.TypeDesc != "USER_TABLE" || !o.Created.Equal(created) {
		t.Fatalf("object %+v, %v", o, err)
	}
	last := f.Queries()[len(f.Queries())-1]
	if last.Database != "a]pp" || len(last.Args) != 2 || last.Exec {
		t.Errorf("recorded %+v", last)
	}
	// No row is gosmo's not found, as from a server.
	if _, err := f.Server().DatabaseRef("other").ObjectByName(context.Background(), "Sales", "Customer"); !errors.Is(err, gosmo.ErrNotFound) {
		t.Errorf("no row: %v", err)
	}
}

// Inside a transaction a database read is a USE batch, and the session stays
// in the database for the next statement.
func TestTransactionReadIsUnwrapped(t *testing.T) {
	f := gosmotest.New(t)
	f.On("WHERE  o.name = @p1", gosmotest.Rows(objectRow(2, "dbo", "t", "U", "USER_TABLE")))
	f.On("SELECT DB_NAME()", func(q gosmotest.Query) gosmotest.Result {
		return gosmotest.Result{Sets: []gosmotest.Set{{Rows: [][]any{{q.Database}}}}}
	})
	s := f.Server()
	err := s.InTransaction(context.Background(), func(ctx context.Context) error {
		got, err := s.DatabaseRef("app").ObjectsNamed(ctx, "t")
		if err != nil || len(got) != 1 || got[0].Schema != "dbo" {
			return fmt.Errorf("objects %v, %v", got, err)
		}
		if cur, err := s.CurrentDatabase(ctx); err != nil || cur != "app" {
			return fmt.Errorf("current database %q, %v", cur, err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range f.Queries() {
		if strings.Contains(q.Text, "USE ") {
			t.Errorf("transport left in %q", q.Text)
		}
	}
}

func TestFailIsTheDriversError(t *testing.T) {
	f := gosmotest.New(t)
	f.On("WHERE  o.name = @p1", gosmotest.Fail(mssql.Error{Number: 229, Class: 14, Message: "The SELECT permission was denied"}))
	_, err := f.Server().DatabaseRef("app").ObjectsNamed(context.Background(), "t")
	if !gosmo.IsPermissionDenied(err) {
		t.Errorf("err %v", err)
	}
}

func TestSets(t *testing.T) {
	f := gosmotest.New(t)
	f.On("q", gosmotest.Sets(gosmotest.Set{Columns: []string{"a"}, Rows: [][]any{{1}, {2}}}, gosmotest.Set{Rows: [][]any{{"x", "y"}}}))
	rows, err := f.Server().DB().QueryContext(context.Background(), "q")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for {
		cols, _ := rows.Columns()
		for rows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				t.Fatal(err)
			}
			got = append(got, fmt.Sprint(vals))
		}
		if !rows.NextResultSet() {
			break
		}
	}
	if strings.Join(got, ",") != "[1],[2],[x y]" {
		t.Errorf("rows %q", got)
	}
}

// recordingTB keeps what a Fake reports instead of failing the test.
type recordingTB struct {
	testing.TB
	errs []string
}

func (r *recordingTB) Errorf(format string, args ...any) {
	r.errs = append(r.errs, fmt.Sprintf(format, args...))
}

func TestUnansweredQueryFailsTheTest(t *testing.T) {
	tb := &recordingTB{TB: t}
	f := gosmotest.New(tb)
	_, err := f.Server().DatabaseRef("app").ObjectsNamed(context.Background(), "t")
	if !errors.Is(err, gosmotest.ErrNoRule) {
		t.Errorf("err %v", err)
	}
	if len(tb.errs) != 1 || !strings.Contains(tb.errs[0], "in [app]: SELECT o.object_id") || !strings.Contains(tb.errs[0], "[t]") {
		t.Errorf("reported %q", tb.errs)
	}
	// An unanswered write succeeds and is recorded.
	if _, err := f.Server().DB().ExecContext(context.Background(), "DROP TABLE x"); err != nil || len(tb.errs) != 1 {
		t.Errorf("exec: %v, %q", err, tb.errs)
	}
	if q := f.Queries(); !q[len(q)-1].Exec {
		t.Errorf("exec recorded as %+v", q[len(q)-1])
	}
}

func TestRowWidthIsChecked(t *testing.T) {
	f := gosmotest.New(t)
	f.On("q", gosmotest.Sets(gosmotest.Set{Columns: []string{"a", "b"}, Rows: [][]any{{1}}}))
	if _, err := f.Server().DB().QueryContext(context.Background(), "q"); err == nil || !strings.Contains(err.Error(), "row 1 has 1 values") {
		t.Errorf("err %v", err)
	}
}

// A bulk load is one recorded Exec per row in the table's database; the
// flush reports the rows sent, and a rule failing a row stops the load there.
func TestBulkInsert(t *testing.T) {
	f := gosmotest.New(t)
	f.On("WHERE c.object_id = OBJECT_ID(@p1)", func(q gosmotest.Query) gosmotest.Result {
		if q.Database != "app" {
			return gosmotest.Result{}
		}
		return gosmotest.Result{Sets: []gosmotest.Set{{Rows: [][]any{
			{"id", "int", 4, 10, 0, 0, 1252}, {"name", "nvarchar", 40, 0, 0, 1252, 1252},
		}}}}
	})
	rows := func(n int) func(func([]any, error) bool) {
		return func(yield func([]any, error) bool) {
			for i := range n {
				if !yield([]any{int64(i + 1), fmt.Sprint("r", i+1)}, nil) {
					return
				}
			}
		}
	}
	bc := gosmo.BulkCopy{Schema: "dbo", Table: "t", Columns: []string{"id", "name"}}
	n, err := f.Server().DatabaseRef("app").BulkInsert(context.Background(), bc, rows(3))
	if err != nil || n != 3 {
		t.Fatalf("loaded %d, %v", n, err)
	}
	var loaded []gosmotest.Query
	for _, q := range f.Queries() {
		if strings.HasPrefix(q.Text, "INSERTBULK ") {
			loaded = append(loaded, q)
		}
	}
	if len(loaded) != 3 || loaded[2].Database != "app" || !loaded[2].Exec || loaded[2].Arg(1) != "r3" {
		t.Errorf("rows %+v", loaded)
	}

	f.OnFunc(func(q gosmotest.Query) bool { return strings.HasPrefix(q.Text, "INSERTBULK ") && q.Arg(0) == int64(2) },
		gosmotest.Fail(mssql.Error{Number: 515, Class: 16, Message: "Cannot insert the value NULL"}))
	n, err = f.Server().DatabaseRef("app").BulkInsert(context.Background(), bc, rows(3))
	if se, ok := gosmo.AsSQLError(err); !ok || se.Number != 515 || n != 1 {
		t.Errorf("failed load: %d, %v", n, err)
	}
}

// A write that reads the server's progress messages (a BACKUP with
// Progress) gets its answer as that stream: success, or the rule's error as
// the server's — the cause and the closing Msg 3013 both.
func TestProgressMessages(t *testing.T) {
	f := gosmotest.New(t)
	opts := gosmo.BackupOptions{Database: "app", Devices: []gosmo.BackupTarget{gosmo.DiskTarget("/b/app.bak")},
		Progress: func(int, string) {}}
	if err := f.Server().Backup(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	last := f.Queries()[len(f.Queries())-1]
	if last.Text != "BACKUP DATABASE [app] TO DISK = N'/b/app.bak' WITH STATS = 10" || last.Exec || !last.Messages || len(last.Args) != 0 {
		t.Errorf("recorded %+v", last)
	}

	f.On("BACKUP DATABASE", gosmotest.Fail(mssql.Error{Number: 3013, Class: 16, Message: "BACKUP DATABASE is terminating abnormally.",
		All: []mssql.Error{{Number: 3201, Class: 16, Message: "Cannot open backup device"}, {Number: 3013, Class: 16}}}))
	err := f.Server().Backup(context.Background(), opts)
	if se, ok := gosmo.AsSQLError(err); !ok || se.Number != 3013 || len(se.All) != 2 || se.All[0].Number != 3201 {
		t.Errorf("refused backup: %v", err)
	}
}
