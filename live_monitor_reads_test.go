//go:build livedb

// Live verification of the reads a server monitor builds on (gossms phase
// 7.a): the ActiveSession fields an idle head blocker is diagnosed by,
// TopQueries/CachedPlan over the plan cache, and system_health's deadlocks
// read through ReadFileEvents and decoded by DecodeDeadlockReport.
//
//	go test -tags livedb . -run TestLiveMonitor -v \
//	  -livedb 'sqlserver://sa:PASS@host?TrustServerCertificate=true'
//
// -livedeadlock-fixtures <dir> also writes each deadlock graph the test
// provokes to <dir>/deadlock_<major>_<kind>.xml (testdata/deadlocks holds
// the committed ones). Creates global temporary tables, which go when their
// connections close, and provokes two deadlocks, which system_health keeps.
package gosmo

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

var liveDeadlockFixtures = flag.String("livedeadlock-fixtures", "", "directory TestLiveMonitorDeadlocks writes the deadlock graphs it provokes to")

// liveConn pins one pooled connection and returns it with its session id.
func liveConn(t *testing.T, ctx context.Context, db *sql.DB) (*sql.Conn, int) {
	t.Helper()
	c, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("connection: %v", err)
	}
	t.Cleanup(func() {
		c.ExecContext(context.Background(), "IF @@TRANCOUNT > 0 ROLLBACK")
		c.Close()
	})
	var spid int
	if err := c.QueryRowContext(ctx, "SELECT @@SPID").Scan(&spid); err != nil {
		t.Fatalf("@@SPID: %v", err)
	}
	return c, spid
}

func liveExec(t *testing.T, ctx context.Context, c *sql.Conn, stmts ...string) {
	t.Helper()
	for _, q := range stmts {
		if _, err := c.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
}

func TestLiveMonitorIdleHeadBlocker(t *testing.T) {
	db, ctx, done := liveDB(t)
	t.Cleanup(done)
	srv, err := NewServer(ctx, db)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	tag := fmt.Sprintf("gosmo_live_head_%x", time.Now().UnixNano()&0xffffff)

	head, headID := liveConn(t, ctx, db)
	liveExec(t, ctx, head,
		"CREATE TABLE ##"+tag+" (id INT NOT NULL PRIMARY KEY, v INT NOT NULL)",
		"INSERT ##"+tag+" VALUES (1, 0), (2, 0)",
		"BEGIN TRANSACTION",
		"UPDATE ##"+tag+" SET v = 1 WHERE id = 1 /* "+tag+" head */")

	waiter, waiterID := liveConn(t, ctx, db)
	wctx, cancel := context.WithCancel(ctx)
	finished := make(chan error, 1)
	go func() {
		_, err := waiter.ExecContext(wctx, "UPDATE ##"+tag+" SET v = 2 WHERE id = 1")
		finished <- err
	}()
	t.Cleanup(func() { cancel(); <-finished })

	var h, w *ActiveSession
	for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
		sessions, err := srv.ActiveSessions(ctx, UserSessionsOnly)
		if err != nil {
			t.Fatalf("ActiveSessions: %v", err)
		}
		h, w = nil, nil
		for _, s := range sessions {
			switch s.SessionID {
			case headID:
				h = s
			case waiterID:
				w = s
			}
		}
		if w != nil && w.BlockingSessionID == headID {
			break
		}
	}
	if h == nil || w == nil {
		t.Fatalf("head %d or waiter %d not listed", headID, waiterID)
	}
	if w.BlockingSessionID != headID {
		t.Fatalf("waiter blocked by %d, want %d", w.BlockingSessionID, headID)
	}

	// The head is idle: no request, so no Command and no CommandText, but its
	// open transaction and the batch that opened it are there.
	if h.Command != "" || h.CommandText != "" {
		t.Errorf("idle head: Command %q, CommandText %q, want both empty", h.Command, h.CommandText)
	}
	if h.OpenTransactionCount < 1 {
		t.Errorf("idle head: OpenTransactionCount %d, want >= 1", h.OpenTransactionCount)
	}
	if !strings.Contains(h.RecentText, tag+" head") {
		t.Errorf("idle head: RecentText %q lacks the UPDATE that opened the transaction", h.RecentText)
	}
	if h.LogicalReads == 0 {
		t.Errorf("idle head: LogicalReads 0 after a CREATE, INSERT and UPDATE")
	}

	// The waiter runs an UPDATE, waits on the key lock and names it.
	if w.Command != "UPDATE" {
		t.Errorf("waiter: Command %q, want UPDATE", w.Command)
	}
	if !strings.HasPrefix(w.WaitResource, "KEY:") {
		t.Errorf("waiter: WaitResource %q, want a KEY: lock", w.WaitResource)
	}
	// The server may auto-parameterize the batch, so match the table only.
	if !strings.Contains(w.RecentText, tag) {
		t.Errorf("waiter: RecentText %q, want its own batch", w.RecentText)
	}
	// The waiter's request has been running (blocked) for a while; the
	// session's own counters leave a running request out, so only the
	// request's elapsed time shows it. The idle head has no request.
	if w.RequestElapsedMS <= 0 {
		t.Errorf("waiter: RequestElapsedMS %d, want the blocked request's running time", w.RequestElapsedMS)
	}
	if h.RequestElapsedMS != 0 || h.RequestCPUTime != 0 {
		t.Errorf("idle head: request counters %d ms / %d ms cpu, want 0", h.RequestElapsedMS, h.RequestCPUTime)
	}

	// SessionByID shares the read and fills the same fields.
	byID, err := srv.SessionByID(ctx, waiterID)
	if err != nil {
		t.Fatalf("SessionByID: %v", err)
	}
	if byID.WaitResource != w.WaitResource || byID.Command != w.Command {
		t.Errorf("SessionByID: %q/%q, ActiveSessions %q/%q", byID.Command, byID.WaitResource, w.Command, w.WaitResource)
	}
}

func TestLiveMonitorTopQueriesAndCachedPlan(t *testing.T) {
	db, ctx, done := liveDB(t)
	t.Cleanup(done)
	srv, err := NewServer(ctx, db)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	tag := fmt.Sprintf("gosmo_live_topq_%x", time.Now().UnixNano()&0xffffff)
	c, _ := liveConn(t, ctx, db)
	liveExec(t, ctx, c, "USE master")
	stmt := "SELECT COUNT(*) AS " + tag + " FROM sys.objects AS o CROSS JOIN sys.objects AS p WHERE o.object_id > p.object_id"
	for range 5 {
		liveExec(t, ctx, c, stmt)
	}

	find := func(opts TopQueriesOptions) *CachedQueryStat {
		t.Helper()
		qs, err := srv.TopQueries(ctx, opts)
		if err != nil {
			t.Fatalf("TopQueries(%+v): %v", opts, err)
		}
		if len(qs) > opts.Top {
			t.Fatalf("TopQueries(%+v): %d rows", opts, len(qs))
		}
		for _, q := range qs {
			if strings.Contains(q.Text, tag) && !strings.Contains(q.Text, "dm_exec") {
				return q
			}
		}
		return nil
	}
	q := find(TopQueriesOptions{By: TopByExecutions, Top: 5000})
	if q == nil {
		t.Fatal("the test's statement is not in TopQueries by executions")
	}
	if q.ExecutionCount < 5 || q.Database != "master" || q.TotalLogicalReads == 0 || q.TotalCPUMicros == 0 ||
		len(q.PlanHandle) == 0 || len(q.QueryHash) != 8 || q.LastExecutionTime.IsZero() || q.CreationTime.IsZero() {
		t.Errorf("statement row incomplete: %+v", q)
	}
	if !strings.HasPrefix(q.Text, "SELECT COUNT(*)") {
		t.Errorf("Text %q is not the statement", q.Text)
	}
	if q.AvgLogicalReads() != float64(q.TotalLogicalReads)/float64(q.ExecutionCount) {
		t.Errorf("AvgLogicalReads %v", q.AvgLogicalReads())
	}
	if find(TopQueriesOptions{By: TopByCPU, Average: true, Top: 5000, Database: "master"}) == nil {
		t.Error("the statement is not in TopQueries by average CPU in master")
	}
	if find(TopQueriesOptions{By: TopByExecutions, Top: 5000, Database: "tempdb"}) != nil {
		t.Error("a master statement is listed for tempdb")
	}
	if _, err := srv.TopQueries(ctx, TopQueriesOptions{Database: "gosmo_no_such_db"}); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown database: %v, want ErrNotFound", err)
	}

	plan, err := srv.CachedPlan(ctx, q.PlanHandle, q.StatementStart, q.StatementEnd)
	if err != nil {
		t.Fatalf("CachedPlan: %v", err)
	}
	if !strings.Contains(plan, "<ShowPlanXML") || !strings.Contains(plan, tag) {
		t.Errorf("CachedPlan: not the statement's showplan: %.200s", plan)
	}
	if _, err := srv.CachedPlan(ctx, make([]byte, 64), 0, -1); !errors.Is(err, ErrNotFound) {
		t.Errorf("CachedPlan of a zero handle: %v, want ErrNotFound", err)
	}
}

// TestLiveMonitorDeadlocks provokes a two-session deadlock on rows and one
// between two parallel plans, then finds both in system_health.
func TestLiveMonitorDeadlocks(t *testing.T) {
	db, ctx, done := liveDBTimeout(t, 5*time.Minute)
	t.Cleanup(done)
	srv, err := NewServer(ctx, db)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	sh, err := srv.XEvents().SessionByName(ctx, "system_health")
	if err != nil {
		t.Fatalf("system_health: %v", err)
	}
	pattern, err := sh.EventFilePattern()
	if err != nil {
		t.Skipf("system_health has no event_file: %v", err)
	}

	tag := fmt.Sprintf("gosmo_live_dl_%x", time.Now().UnixNano()&0xffffff)
	kinds := []struct {
		name     string
		rows     int
		lockID   int
		cross    string // the statement each session runs on the other's table
		parallel bool
	}{
		{"rows", 2, 1, "UPDATE ##%s SET v = 9 WHERE id = 1", false},
		{"parallel", 300000, 150000,
			"SELECT COUNT(*) FROM ##%s WHERE pad <> 'z' OPTION (MAXDOP 4, USE HINT('ENABLE_PARALLEL_PLAN_PREFERENCE'))", true},
	}
	for _, k := range kinds {
		a, b := tag+"_"+k.name+"_a", tag+"_"+k.name+"_b"
		ca, _ := liveConn(t, ctx, db)
		cb, _ := liveConn(t, ctx, db)
		for _, tbl := range []string{a, b} {
			liveExec(t, ctx, ca,
				"CREATE TABLE ##"+tbl+" (id INT NOT NULL PRIMARY KEY, v INT NOT NULL DEFAULT 0, pad CHAR(100) NOT NULL DEFAULT 'x')",
				fmt.Sprintf(`INSERT ##%s (id) SELECT TOP (%d) ROW_NUMBER() OVER (ORDER BY (SELECT NULL))
				 FROM sys.all_objects x CROSS JOIN sys.all_objects y`, tbl, k.rows))
		}
		liveExec(t, ctx, ca, "BEGIN TRANSACTION", fmt.Sprintf("UPDATE ##%s SET v = 1 WHERE id = %d", a, k.lockID))
		liveExec(t, ctx, cb, "BEGIN TRANSACTION", fmt.Sprintf("UPDATE ##%s SET v = 1 WHERE id = %d", b, k.lockID))
		var wg sync.WaitGroup
		errs := make([]error, 2)
		for i, side := range []struct {
			c     *sql.Conn
			other string
		}{{ca, b}, {cb, a}} {
			wg.Go(func() {
				_, errs[i] = side.c.ExecContext(ctx, fmt.Sprintf(k.cross, side.other))
			})
			time.Sleep(time.Second) // the first blocks before the second closes the cycle
		}
		wg.Wait()
		victims := 0
		for _, err := range errs {
			if se, ok := AsSQLError(err); ok && se.Number == 1205 {
				victims++
			} else if err != nil {
				t.Fatalf("%s: %v", k.name, err)
			}
		}
		if victims == 0 {
			t.Fatalf("%s: no session was chosen as deadlock victim", k.name)
		}
	}

	// system_health dispatches within its MAX_DISPATCH_LATENCY; poll.
	found := map[string]*DeadlockReport{}
	var raw = map[string]string{}
	for deadline := time.Now().Add(4 * time.Minute); len(found) < len(kinds) && time.Now().Before(deadline); time.Sleep(5 * time.Second) {
		events, _, err := srv.XEvents().ReadFileEvents(ctx, pattern, EventFileCursor{}, 0, []string{"xml_deadlock_report"})
		if err != nil {
			t.Fatalf("ReadFileEvents: %v", err)
		}
		for _, e := range events {
			if e.Name != "xml_deadlock_report" {
				t.Fatalf("ReadFileEvents returned a %s event", e.Name)
			}
			x, ok := e.Field("xml_report")
			if !ok || !x.IsXML {
				t.Fatalf("xml_deadlock_report without an XML xml_report: %+v", e.Fields)
			}
			for _, k := range kinds {
				if strings.Contains(x.Value, tag+"_"+k.name+"_") {
					r, err := DecodeDeadlockReport([]byte(x.Value))
					if err != nil {
						t.Fatalf("%s: DecodeDeadlockReport: %v", k.name, err)
					}
					found[k.name], raw[k.name] = r, x.Value
				}
			}
		}
	}
	for _, k := range kinds {
		r := found[k.name]
		if r == nil {
			t.Errorf("%s: deadlock not found in system_health", k.name)
			continue
		}
		t.Logf("%s: %d victims, %d processes, %d resources", k.name, len(r.Victims), len(r.Processes), len(r.Resources))
		if len(r.Victims) == 0 || len(r.Processes) < 2 || len(r.Resources) < 2 {
			t.Errorf("%s: incomplete report: %+v", k.name, r)
		}
		for _, v := range r.Victims {
			if p, ok := r.Process(v); !ok || !p.Victim {
				t.Errorf("%s: victim %s not a victim process", k.name, v)
			}
		}
		// A parallel worker (ecid > 0) carries no login, and a coordinator
		// waiting on its workers no lock: only a lock waiter has both.
		lockWaiters := 0
		for _, p := range r.Processes {
			if p.SessionID == 0 || p.Statement() == "" || (p.ExecutionContextID == 0 && p.Login == "") {
				t.Errorf("%s: process incomplete: %+v", k.name, p)
			}
			if p.LockMode != "" && p.WaitResource != "" {
				lockWaiters++
			}
		}
		if lockWaiters < 2 {
			t.Errorf("%s: %d processes wait on a lock, want 2 at least", k.name, lockWaiters)
		}
		if k.parallel && !strings.Contains(raw[k.name], "exchangeEvent") && !hasWorker(r) {
			t.Errorf("%s: no exchangeEvent or parallel worker in the report", k.name)
		}
		if *liveDeadlockFixtures != "" {
			name := filepath.Join(*liveDeadlockFixtures, fmt.Sprintf("deadlock_%d_%s.xml", srv.Info().VersionMajor, k.name))
			if err := os.WriteFile(name, []byte(raw[k.name]), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func hasWorker(r *DeadlockReport) bool {
	for _, p := range r.Processes {
		if p.ExecutionContextID > 0 {
			return true
		}
	}
	return false
}
