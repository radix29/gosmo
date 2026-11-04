package gosmo

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// TestTopQueriesRanksByTheNamedColumn pins every ranking to the
// dm_exec_query_stats column it orders by, by name — a swapped pair in
// topQueriesColumn would otherwise rank "CPU" by duration with every caller
// agreeing.
func TestTopQueriesRanksByTheNamedColumn(t *testing.T) {
	want := map[TopQueriesBy]string{
		TopByCPU:           "total_worker_time",
		TopByLogicalReads:  "total_logical_reads",
		TopByLogicalWrites: "total_logical_writes",
		TopByDuration:      "total_elapsed_time",
		TopByExecutions:    "execution_count",
	}
	if len(want) != len(topQueriesColumn) {
		t.Fatalf("%d rankings pinned, %d defined", len(want), len(topQueriesColumn))
	}
	for by, col := range want {
		for _, avg := range []bool{false, true} {
			if avg && by == TopByExecutions {
				continue
			}
			d := qsRecDB(t, 17, nil, nil)
			if _, err := d.server.TopQueries(context.Background(), TopQueriesOptions{By: by, Average: avg}); err != nil {
				t.Fatalf("TopQueries(%d, avg %v): %v", by, avg, err)
			}
			call := qsRec.last(t)
			order := "ORDER  BY qs." + col + " DESC"
			if avg {
				order = "ORDER  BY CAST(qs." + col + " AS float) / NULLIF(qs.execution_count, 0) DESC"
			}
			if !strings.Contains(call.sql, order) {
				t.Errorf("TopQueries(%d, avg %v) does not %s:\n%s", by, avg, order, call.sql)
			}
			if !slices.Equal(call.args, []any{int64(20)}) {
				t.Errorf("TopQueries(%d) bound %v, want the default top 20", by, call.args)
			}
			if strings.Contains(call.sql, "WHERE  d.dbid") {
				t.Errorf("TopQueries with no database filters on one:\n%s", call.sql)
			}
		}
	}
}

func TestTopQueriesRefusals(t *testing.T) {
	for _, opts := range []TopQueriesOptions{
		{By: TopQueriesBy(99)},
		{By: TopByExecutions, Average: true},
		{Top: -1},
	} {
		d := qsRecDB(t, 17, nil, nil)
		if _, err := d.server.TopQueries(context.Background(), opts); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("TopQueries(%+v): %v, want ErrInvalidRequest", opts, err)
		}
		if n := len(qsRec.recorded()); n != 0 {
			t.Errorf("TopQueries(%+v) sent %d statements before refusing", opts, n)
		}
	}
}

func TestTopQueriesDatabaseAndScan(t *testing.T) {
	cols := make([]string, 13)
	for i := range cols {
		cols[i] = fmt.Sprintf("c%d", i)
	}
	last := time.Date(2026, 10, 30, 9, 0, 0, 0, time.UTC)
	row := []driver.Value{"appdb", []byte{1, 2, 3, 4, 5, 6, 7, 8}, []byte{9, 9}, int64(10), int64(-1), "SELECT 1",
		int64(4), int64(400), int64(800), int64(40), int64(8), last, last.Add(-time.Hour)}
	d := qsRecDB(t, 17, cols, [][]driver.Value{row})
	qsRec.route("DB_ID(@p1)", []string{"id"}, [][]driver.Value{{int64(7)}})
	qs, err := d.server.TopQueries(context.Background(), TopQueriesOptions{By: TopByLogicalReads, Top: 5, Database: "appdb"})
	if err != nil {
		t.Fatalf("TopQueries: %v", err)
	}
	calls := qsRec.recorded()
	if len(calls) != 2 || !slices.Equal(calls[0].args, []any{"appdb"}) {
		t.Fatalf("calls %+v, want the DB_ID lookup of appdb, then the read", calls)
	}
	if !slices.Equal(calls[1].args, []any{int64(5), int64(7)}) || !strings.Contains(calls[1].sql, "WHERE  d.dbid = @p2") {
		t.Errorf("read bound %v, want top 5 and database id 7:\n%s", calls[1].args, calls[1].sql)
	}
	if len(qs) != 1 {
		t.Fatalf("%d rows", len(qs))
	}
	q := qs[0]
	if q.Database != "appdb" || len(q.QueryHash) != 8 || !slices.Equal(q.PlanHandle, []byte{9, 9}) ||
		q.StatementStart != 10 || q.StatementEnd != -1 || q.Text != "SELECT 1" ||
		q.ExecutionCount != 4 || q.TotalCPUMicros != 400 || q.TotalDurationMicros != 800 ||
		q.TotalLogicalReads != 40 || q.TotalLogicalWrites != 8 ||
		!q.LastExecutionTime.Equal(last) || !q.CreationTime.Equal(last.Add(-time.Hour)) {
		t.Errorf("scanned %+v", *q)
	}
	if q.AvgCPUMicros() != 100 || q.AvgDurationMicros() != 200 || q.AvgLogicalReads() != 10 || q.AvgLogicalWrites() != 2 {
		t.Errorf("averages %v %v %v %v", q.AvgCPUMicros(), q.AvgDurationMicros(), q.AvgLogicalReads(), q.AvgLogicalWrites())
	}

	d = qsRecDB(t, 17, cols, [][]driver.Value{row})
	qsRec.route("DB_ID(@p1)", []string{"id"}, [][]driver.Value{{nil}})
	if _, err := d.server.TopQueries(context.Background(), TopQueriesOptions{Database: "nope"}); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown database: %v, want ErrNotFound", err)
	}
	if n := len(qsRec.recorded()); n != 1 {
		t.Errorf("unknown database: %d statements, want only the lookup", n)
	}
}

func TestCachedPlan(t *testing.T) {
	for _, tc := range []struct {
		handle     []byte
		start, end int
	}{
		{nil, 0, -1},
		{[]byte{1}, -1, -1},
		{[]byte{1}, 0, -2},
		{[]byte{1}, 10, 4},
	} {
		d := qsRecDB(t, 17, nil, nil)
		if _, err := d.server.CachedPlan(context.Background(), tc.handle, tc.start, tc.end); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("CachedPlan(%v, %d, %d): %v, want ErrInvalidRequest", tc.handle, tc.start, tc.end, err)
		}
		if n := len(qsRec.recorded()); n != 0 {
			t.Errorf("CachedPlan(%v, %d, %d) sent %d statements before refusing", tc.handle, tc.start, tc.end, n)
		}
	}

	d := qsRecDB(t, 17, []string{"query_plan"}, [][]driver.Value{{"<ShowPlanXML/>"}})
	plan, err := d.server.CachedPlan(context.Background(), []byte{0xAB}, 4, -1)
	if err != nil || plan != "<ShowPlanXML/>" {
		t.Fatalf("CachedPlan = %q, %v", plan, err)
	}
	call := qsRec.last(t)
	if !strings.Contains(call.sql, "sys.dm_exec_text_query_plan(@p1, @p2, @p3)") ||
		!slices.EqualFunc(call.args, []any{[]byte{0xAB}, int64(4), int64(-1)}, func(a, b any) bool { return fmt.Sprint(a) == fmt.Sprint(b) }) {
		t.Errorf("CachedPlan sent %v:\n%s", call.args, call.sql)
	}

	// A plan that left the cache: a NULL query_plan, or no row.
	for _, rows := range [][][]driver.Value{{{nil}}, nil} {
		d = qsRecDB(t, 17, []string{"query_plan"}, rows)
		if _, err := d.server.CachedPlan(context.Background(), []byte{1}, 0, -1); !errors.Is(err, ErrNotFound) {
			t.Errorf("CachedPlan with rows %v: %v, want ErrNotFound", rows, err)
		}
	}
}

// TestReadFileEventsFiltersOnTheServer: the event names are bound into a
// WHERE on object_name, and ReadFile sends none.
func TestReadFileEventsFiltersOnTheServer(t *testing.T) {
	d := qsRecDB(t, 17, []string{"file_name", "file_offset", "event_data"}, nil)
	x := d.server.XEvents()
	if _, _, err := x.ReadFileEvents(context.Background(), "sh*.xel", EventFileCursor{}, 0,
		[]string{"xml_deadlock_report", "error_reported"}); err != nil {
		t.Fatal(err)
	}
	call := qsRec.last(t)
	if !strings.Contains(call.sql, "WHERE  object_name COLLATE DATABASE_DEFAULT IN (@p4, @p5)") ||
		!slices.Equal(call.args, []any{"sh*.xel", nil, nil, "xml_deadlock_report", "error_reported"}) {
		t.Errorf("ReadFileEvents sent %v:\n%s", call.args, call.sql)
	}
	for _, read := range []func() error{
		func() error {
			_, _, err := x.ReadFile(context.Background(), "sh*.xel", EventFileCursor{}, 0)
			return err
		},
		func() error {
			_, _, err := x.ReadFileEvents(context.Background(), "sh*.xel", EventFileCursor{}, 0, nil)
			return err
		},
	} {
		if err := read(); err != nil {
			t.Fatal(err)
		}
		if call := qsRec.last(t); strings.Contains(call.sql, "WHERE") || len(call.args) != 3 {
			t.Errorf("an unfiltered read sent %v:\n%s", call.args, call.sql)
		}
	}
}

// The fixtures in testdata/deadlocks are xml_report values system_health
// recorded for deadlocks TestLiveMonitorDeadlocks provoked (2026-10-30), one
// per major: "rows" two sessions updating each other's row, "parallel" two
// parallel scans each blocked on the other's lock.
func readDeadlockFixture(t *testing.T, name string) *DeadlockReport {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "deadlocks", name))
	if err != nil {
		t.Fatal(err)
	}
	r, err := DecodeDeadlockReport(b)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return r
}

func TestDecodeDeadlockReportRows(t *testing.T) {
	for _, major := range []int{13, 14, 17} {
		name := fmt.Sprintf("deadlock_%d_rows.xml", major)
		r := readDeadlockFixture(t, name)
		if len(r.Victims) != 1 || len(r.Processes) != 2 || len(r.Resources) != 2 {
			t.Fatalf("%s: %d victims, %d processes, %d resources", name, len(r.Victims), len(r.Processes), len(r.Resources))
		}
		v, ok := r.Process(r.Victims[0])
		if !ok || !v.Victim {
			t.Fatalf("%s: victim %q is not a victim process", name, r.Victims[0])
		}
		for _, p := range r.Processes {
			if p.SessionID == 0 || p.ExecutionContextID != 0 || p.Login != "sa" || p.ClientApp == "" ||
				p.LockMode != "X" || !strings.HasPrefix(p.WaitResource, "KEY: 2:") || p.DatabaseID != 1 ||
				p.IsolationLevel != "read committed (2)" || p.TranCount != 2 || p.TransactionName != "user_transaction" ||
				p.LastTranStarted.IsZero() || len(p.Frames) == 0 || p.Frames[0].ProcName != "adhoc" ||
				!strings.Contains(p.Statement(), "UPDATE ##gosmo_live_dl_") {
				t.Errorf("%s: process %+v", name, p)
			}
			// currentdbname is a servicing addition: 2016 SP3 has it, 2017
			// RTM does not.
			if wantDB := map[bool]string{true: "master", false: ""}[major != 14]; p.Database != wantDB {
				t.Errorf("%s: Database %q, want %q", name, p.Database, wantDB)
			}
			if p.Victim != (p.ID == v.ID) {
				t.Errorf("%s: process %s Victim %v", name, p.ID, p.Victim)
			}
		}
		for _, res := range r.Resources {
			if res.Kind != "keylock" || res.Mode != "X" || res.DatabaseID != 2 ||
				!strings.Contains(res.ObjectName, "##gosmo_live_dl_") || res.Attributes["hobtid"] == "" ||
				len(res.Owners) != 1 || len(res.Waiters) != 1 || res.Waiters[0].Mode != "X" || res.Waiters[0].RequestType != "wait" {
				t.Errorf("%s: resource %+v", name, res)
			}
			if _, ok := r.Process(res.Owners[0].ProcessID); !ok {
				t.Errorf("%s: owner %s is no process", name, res.Owners[0].ProcessID)
			}
		}
	}
}

func TestDecodeDeadlockReportParallel(t *testing.T) {
	for _, major := range []int{13, 14, 17} {
		name := fmt.Sprintf("deadlock_%d_parallel.xml", major)
		r := readDeadlockFixture(t, name)
		workers, exchanges, lockWaiters := 0, 0, 0
		for _, p := range r.Processes {
			if p.ExecutionContextID > 0 {
				workers++
				if p.Login != "" {
					t.Errorf("%s: worker %s has a login %q; the fixture changed", name, p.ID, p.Login)
				}
			}
			if p.LockMode != "" {
				lockWaiters++
			}
			if p.InputBuffer == "" || p.Statement() == "" {
				t.Errorf("%s: process %s has no statement", name, p.ID)
			}
		}
		for _, res := range r.Resources {
			if res.Kind == "exchangeEvent" {
				exchanges++
				if res.WaitType == "" || res.Mode != "" {
					t.Errorf("%s: exchange %+v", name, res)
				}
			}
		}
		if len(r.Victims) == 0 || workers == 0 || exchanges == 0 || lockWaiters < 2 {
			t.Errorf("%s: %d victims, %d workers, %d exchanges, %d lock waiters", name, len(r.Victims), workers, exchanges, lockWaiters)
		}
	}
}

func TestDecodeDeadlockReportForms(t *testing.T) {
	// The .xdl form: a deadlock-list, the victim an attribute, a frame with
	// its statement text.
	xdl := `<deadlock-list><deadlock victim="p2">
<process-list>
 <process id="p1" spid="51" ecid="0" lockMode="S" waitresource="RID: 5:1:200:0" lasttranstarted="garbage">
  <executionStack><frame procname="db.dbo.proc" line="7" stmtstart="20">SELECT * FROM t</frame></executionStack>
  <inputbuf>EXEC dbo.proc</inputbuf>
 </process>
 <process id="p2" spid="52"><inputbuf>  UPDATE t SET a = 1  </inputbuf></process>
</process-list>
<resource-list>
 <ridlock fileid="1" pageid="200" dbid="5" objectname="db.dbo.t" id="lock1" mode="X">
  <owner-list><owner id="p2" mode="X"/></owner-list>
  <waiter-list><waiter id="p1" mode="S" requestType="wait"/></waiter-list>
 </ridlock>
</resource-list></deadlock></deadlock-list>`
	r, err := DecodeDeadlockReport([]byte(xdl))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(r.Victims, []string{"p2"}) || r.Processes[0].Victim || !r.Processes[1].Victim {
		t.Errorf("victims %v, flags %v %v", r.Victims, r.Processes[0].Victim, r.Processes[1].Victim)
	}
	p1 := r.Processes[0]
	if f := p1.Frames[0]; f.ProcName != "db.dbo.proc" || f.Line != 7 || f.StmtStart != 20 || f.StmtEnd != -1 || f.Text != "SELECT * FROM t" {
		t.Errorf("frame %+v", f)
	}
	if p1.Statement() != "SELECT * FROM t" || r.Processes[1].Statement() != "UPDATE t SET a = 1" {
		t.Errorf("statements %q, %q", p1.Statement(), r.Processes[1].Statement())
	}
	if !p1.LastTranStarted.IsZero() {
		t.Errorf("an unreadable time decoded to %v", p1.LastTranStarted)
	}
	if res := r.Resources[0]; res.Kind != "ridlock" || res.Attributes["pageid"] != "200" ||
		res.Owners[0] != (DeadlockLockHolder{ProcessID: "p2", Mode: "X"}) ||
		res.Waiters[0] != (DeadlockLockHolder{ProcessID: "p1", Mode: "S", RequestType: "wait"}) {
		t.Errorf("resource %+v", res)
	}

	if _, err := DecodeDeadlockReport([]byte(`<event name="x"/>`)); !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("no deadlock: %v, want ErrInvalidRequest", err)
	}
	if _, err := DecodeDeadlockReport([]byte(`<deadlock><process-list>`)); err == nil {
		t.Error("truncated XML decoded")
	}
}
