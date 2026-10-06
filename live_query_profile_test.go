//go:build livedb

// Live check of the Live Query Statistics reads: a long cross join runs on a
// connection of its own under SET STATISTICS XML ON, and QueryProfiles and
// InFlightPlan poll it from the pool — the shape a Live Query Statistics view
// has. Only a server shows what the DMVs return mid-statement, and that both
// go quiet once the session is idle.
//
//	go test -tags livedb . -run TestLiveQueryProfiles -v \
//	  -livedb 'sqlserver://sa:PASS@host?TrustServerCertificate=true'
//
// Run on the 2016 floor as well as a current major. Creates and drops one
// throwaway login; touches nothing else.
package gosmo

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"
)

// liveProfiledQuery is slow on purpose and in row mode: CHECKSUM over three
// name columns cannot be batch-mode aggregated, and MAXDOP 1 keeps it to one
// thread so the per-node checks below need no merge. It runs for minutes; the
// test cancels it.
const liveProfiledQuery = `SELECT SUM(CAST(CHECKSUM(a.name, b.name, c.name) AS bigint))
FROM sys.all_objects AS a CROSS JOIN sys.all_objects AS b CROSS JOIN sys.all_objects AS c
OPTION (MAXDOP 1)`

func TestLiveQueryProfiles(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()
	srv := liveServer(t, db, ctx)

	// The watched session: one pinned connection, profiled the way a query
	// window with Include Actual Execution Plan is.
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("conn: %v", err)
	}
	defer conn.Close()
	var spid int
	if err := conn.QueryRowContext(ctx, "SELECT @@SPID").Scan(&spid); err != nil {
		t.Fatalf("spid: %v", err)
	}

	t.Run("idle session", func(t *testing.T) {
		got, err := srv.QueryProfiles(ctx, spid)
		if err != nil || len(got) != 0 {
			t.Errorf("QueryProfiles of an idle session = %d rows, %v; want none", len(got), err)
		}
		if _, err := srv.InFlightPlan(ctx, spid); !errors.Is(err, ErrNotFound) {
			t.Errorf("InFlightPlan of an idle session: %v, want ErrNotFound", err)
		}
	})

	stop, err := StartPlanCapture(ctx, conn, PlanActual)
	if err != nil {
		t.Fatalf("StartPlanCapture: %v", err)
	}
	runCtx, cancelRun := context.WithCancel(ctx)
	ran := make(chan error, 1)
	go func() {
		rows, err := conn.QueryContext(runCtx, liveProfiledQuery)
		if err == nil {
			for rows.NextResultSet() {
			}
			err = rows.Err()
			rows.Close()
		}
		ran <- err
	}()
	finish := func() {
		cancelRun()
		select {
		case <-ran:
		case <-time.After(30 * time.Second):
			t.Fatal("the profiled query did not stop on cancel")
		}
		stop()
	}

	// Wait until the cross join has moved rows through its inner loop.
	var profiles []QueryProfile
	deadline := time.Now().Add(20 * time.Second)
	for {
		profiles, err = srv.QueryProfiles(ctx, spid)
		if err != nil {
			finish()
			t.Fatalf("QueryProfiles: %v", err)
		}
		if moved(profiles) {
			break
		}
		if time.Now().After(deadline) {
			finish()
			t.Fatalf("no profile row with rows after 20s: %+v", profiles)
		}
		time.Sleep(250 * time.Millisecond)
	}

	t.Run("profiles mid-statement", func(t *testing.T) {
		nodes := map[int]bool{}
		var sawScan bool
		for _, p := range profiles {
			nodes[p.NodeID] = true
			if p.ThreadID != 0 {
				t.Errorf("node %d on thread %d under MAXDOP 1", p.NodeID, p.ThreadID)
			}
			if p.PhysicalOperator == "" || len(p.PlanHandle) == 0 {
				t.Errorf("node %d: operator %q, plan handle %x — both should be filled in", p.NodeID, p.PhysicalOperator, p.PlanHandle)
			}
			if p.StatementStart != profiles[0].StatementStart || !bytes.Equal(p.PlanHandle, profiles[0].PlanHandle) {
				t.Errorf("node %d names another statement than node %d", p.NodeID, profiles[0].NodeID)
			}
			if p.OpenTime != 0 && p.FirstActiveTime == 0 {
				t.Errorf("node %d open at tick %d but never active", p.NodeID, p.OpenTime)
			}
			if p.ObjectID != 0 {
				sawScan = true
				if p.DatabaseID == 0 {
					t.Errorf("node %d reads object %d in database 0", p.NodeID, p.ObjectID)
				}
			}
		}
		// Three scans, two loops joins and an aggregate at least, whatever
		// shape the optimizer picks for this version.
		if len(nodes) < 6 {
			t.Errorf("profiles cover %d nodes, want the whole plan: %+v", len(nodes), profiles)
		}
		if !sawScan {
			t.Error("no node names the object it reads — the ISNULLed columns came back zero throughout")
		}
		// The root aggregate cannot close while its input is still running.
		for _, p := range profiles {
			if p.NodeID == minNode(profiles) && p.CloseTime != 0 {
				t.Errorf("root node %d closed at tick %d mid-statement", p.NodeID, p.CloseTime)
			}
		}
	})

	t.Run("in-flight plan", func(t *testing.T) {
		plan, err := srv.InFlightPlan(ctx, spid)
		if err != nil {
			t.Fatalf("InFlightPlan: %v", err)
		}
		if !strings.Contains(plan.XML, "<ShowPlanXML") || !strings.Contains(plan.XML, "RunTimeInformation") {
			t.Errorf("in-flight plan is not a showplan with runtime counters: %.300s", plan.XML)
		}
		if !bytes.Equal(plan.PlanHandle, profiles[0].PlanHandle) ||
			plan.StatementStart != profiles[0].StatementStart || plan.StatementEnd != profiles[0].StatementEnd {
			t.Errorf("plan (%x, %d..%d) and profiles (%x, %d..%d) name different statements",
				plan.PlanHandle, plan.StatementStart, plan.StatementEnd,
				profiles[0].PlanHandle, profiles[0].StatementStart, profiles[0].StatementEnd)
		}
	})

	t.Run("without VIEW SERVER STATE", func(t *testing.T) {
		const login, pass = "gosmo_queryprofiles_lowpriv", "Qp!9xQ#2pLk7"
		if _, err := db.ExecContext(ctx, "CREATE LOGIN "+login+" WITH PASSWORD = N'"+pass+"', CHECK_POLICY = OFF"); err != nil {
			t.Fatalf("create login: %v", err)
		}
		t.Cleanup(func() { db.ExecContext(context.Background(), "DROP LOGIN "+login) })

		u, err := url.Parse(*liveDSN)
		if err != nil {
			t.Fatalf("parse DSN: %v", err)
		}
		u.User = url.UserPassword(login, pass)
		ldb, err := sql.Open("sqlserver", u.String())
		if err != nil {
			t.Fatalf("open as %s: %v", login, err)
		}
		// Closed before the DROP LOGIN cleanup, which runs after this.
		defer ldb.Close()
		lsrv := liveServer(t, ldb, ctx)
		if _, err := lsrv.QueryProfiles(ctx, spid); !IsPermissionDenied(err) {
			t.Errorf("QueryProfiles without the right: %v, want a permission refusal", err)
		}
		if _, err := lsrv.InFlightPlan(ctx, spid); !IsPermissionDenied(err) {
			t.Errorf("InFlightPlan without the right: %v, want a permission refusal", err)
		}
	})

	finish()

	t.Run("after the statement", func(t *testing.T) {
		// The session is idle again; its connection may be broken by the
		// cancel, which is no matter — nothing runs on it.
		if got, err := srv.QueryProfiles(ctx, spid); err != nil || len(got) != 0 {
			t.Errorf("QueryProfiles after cancel = %d rows, %v; want none", len(got), err)
		}
		if _, err := srv.InFlightPlan(ctx, spid); !errors.Is(err, ErrNotFound) {
			t.Errorf("InFlightPlan after cancel: %v, want ErrNotFound", err)
		}
	})
}

// moved reports whether some operator has produced rows yet.
func moved(profiles []QueryProfile) bool {
	for _, p := range profiles {
		if p.RowCount > 0 {
			return true
		}
	}
	return false
}

func minNode(profiles []QueryProfile) int {
	m := profiles[0].NodeID
	for _, p := range profiles {
		m = min(m, p.NodeID)
	}
	return m
}
