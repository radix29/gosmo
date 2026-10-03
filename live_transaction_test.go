//go:build livedb

// Live InTransaction: a resource pool, a workload group in it and a schema in
// a throwaway database, created in one transaction, rolled back and then
// committed. The group's create reads the pool back, the schema's switches
// the session to the database, and a server-scoped read follows it — so the
// transaction's reads, its USE and its return to the home database all run.
//
//	go test -tags livedb . -run TestLiveInTransaction -v \
//	  -livedb 'sqlserver://sa:PASS@host?TrustServerCertificate=true'
//
// The governor is server-wide state. The test runs only on one with nothing
// pending, and puts that back: the fixtures are dropped, then RECONFIGURE on
// an enabled governor (which, with nothing changed, changes nothing) or
// DISABLE on a disabled one (which clears the flag without enabling it).
package gosmo

import (
	"context"
	"errors"
	"slices"
	"testing"
)

const (
	liveTxPool   = "gosmo_live_tx_pool"
	liveTxGroup  = "gosmo_live_tx_group"
	liveTxDB     = "gosmo_live_tx_db"
	liveTxSchema = "gosmo_live_tx_schema"
)

func TestLiveInTransaction(t *testing.T) {
	db, ctx, done := liveDB(t)
	t.Cleanup(done) // registered first, so it runs after every drop below

	srv, err := NewServer(ctx, db)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	t.Cleanup(func() { srv.Close() })
	rg, err := srv.ResourceGovernor(ctx)
	if err != nil {
		t.Fatalf("ResourceGovernor: %v", err)
	}
	st, err := srv.ResourceGovernorStatus(ctx)
	if err != nil {
		t.Fatalf("ResourceGovernorStatus: %v", err)
	}
	if st.IsReconfigurationPending {
		t.Skip("resource governor has a pending change — this test only runs where it can restore the state exactly")
	}

	cleanup := func(stmt string) {
		t.Cleanup(func() {
			if _, err := db.ExecContext(context.Background(), stmt); err != nil {
				t.Errorf("cleanup %s: %v", stmt, err)
			}
		})
	}
	if rg.IsEnabled {
		cleanup("ALTER RESOURCE GOVERNOR RECONFIGURE")
	} else {
		cleanup("ALTER RESOURCE GOVERNOR DISABLE")
	}
	if _, err := db.ExecContext(ctx, "CREATE DATABASE ["+liveTxDB+"]"); err != nil {
		t.Fatalf("create database: %v", err)
	}
	cleanup("DROP DATABASE IF EXISTS [" + liveTxDB + "]")
	cleanup("IF EXISTS (SELECT 1 FROM sys.resource_governor_resource_pools WHERE name = N'" + liveTxPool + "') DROP RESOURCE POOL [" + liveTxPool + "]")
	cleanup("IF EXISTS (SELECT 1 FROM sys.resource_governor_workload_groups WHERE name = N'" + liveTxGroup + "') DROP WORKLOAD GROUP [" + liveTxGroup + "]")

	var seen []string
	octx := WithStatementObserver(ctx, func(e ScriptEntry) { seen = append(seen, e.SQL) })
	steps := func(ctx context.Context) error {
		if _, err := srv.CreateResourcePool(ctx, CreateResourcePoolRequest{Name: liveTxPool}); err != nil {
			return err
		}
		g, err := srv.CreateWorkloadGroup(ctx, CreateWorkloadGroupRequest{Name: liveTxGroup, Options: WorkloadGroupOptions{Pool: new(liveTxPool)}})
		if err != nil {
			return err
		}
		if g.PoolName != liveTxPool {
			t.Errorf("group read back inside the transaction is in pool %q, want %q", g.PoolName, liveTxPool)
		}
		if _, err := srv.DatabaseRef(liveTxDB).CreateSchema(ctx, CreateSchemaRequest{Name: liveTxSchema}); err != nil {
			return err
		}
		// A server-scoped read after the database-scoped write: it runs back
		// in the home database, and sees the uncommitted pool.
		var home string
		if err := srv.queryRowScan(ctx, "SELECT DB_NAME()", nil, &home); err != nil {
			return err
		}
		if home == liveTxDB {
			t.Errorf("server-scoped read ran in %s, the database a database-scoped write switched to", home)
		}
		if _, err := srv.ResourcePoolByName(ctx, liveTxPool); err != nil {
			t.Errorf("ResourcePoolByName inside the transaction: %v", err)
		}
		// A nested call joins.
		return srv.InTransaction(ctx, func(ctx context.Context) error { return nil })
	}

	exists := func() (pool, group, schema bool) {
		t.Helper()
		_, err := srv.ResourcePoolByName(ctx, liveTxPool)
		pool = err == nil
		_, err = srv.WorkloadGroupByName(ctx, liveTxGroup)
		group = err == nil
		_, err = srv.DatabaseRef(liveTxDB).SchemaByName(ctx, liveTxSchema)
		schema = err == nil
		return
	}

	t.Run("rollback", func(t *testing.T) {
		seen = nil
		boom := errors.New("boom")
		err := srv.InTransaction(octx, func(ctx context.Context) error {
			if err := steps(ctx); err != nil {
				return err
			}
			return boom
		})
		if !errors.Is(err, boom) {
			t.Fatalf("InTransaction: %v, want boom", err)
		}
		if p, g, s := exists(); p || g || s {
			t.Errorf("after rollback: pool %v, group %v, schema %v — want none", p, g, s)
		}
		if len(seen) != 0 {
			t.Errorf("observer heard of rolled-back statements: %q", seen)
		}
	})

	t.Run("refused inside", func(t *testing.T) {
		err := srv.InTransaction(ctx, func(ctx context.Context) error {
			if err := srv.ResourceGovernorRef().Reconfigure(ctx); err == nil {
				t.Error("RECONFIGURE inside a transaction succeeded, want Msg 574")
			}
			_, err := srv.DatabaseRef(liveTxDB).BulkInsert(ctx, BulkCopy{Schema: "dbo", Table: "t", Columns: []string{"a"}}, nil)
			if !errors.Is(err, ErrUnsupported) {
				t.Errorf("BulkInsert inside a transaction: %v, want ErrUnsupported", err)
			}
			return nil
		})
		// Msg 574 does not doom the transaction, so the empty one commits.
		if err != nil {
			t.Errorf("InTransaction: %v", err)
		}
	})

	t.Run("commit", func(t *testing.T) {
		seen = nil
		if err := srv.InTransaction(octx, steps); err != nil {
			t.Fatalf("InTransaction: %v", err)
		}
		if p, g, s := exists(); !p || !g || !s {
			t.Errorf("after commit: pool %v, group %v, schema %v — want all", p, g, s)
		}
		want := []string{
			"CREATE RESOURCE POOL [" + liveTxPool + "]",
			"CREATE WORKLOAD GROUP [" + liveTxGroup + "] USING [" + liveTxPool + "]",
			"CREATE SCHEMA [" + liveTxSchema + "]",
		}
		if !slices.Equal(seen, want) {
			t.Errorf("observer after commit:\n got %q\nwant %q", seen, want)
		}
	})
}

// TestLiveInTransactionAtomicWrite: a multi-statement write (execAtomic)
// inside InTransaction. Its failure leaves the caller's transaction open,
// XACT_ABORT off and the statements before it uncommitted; an fn that
// tolerates the failure gets a refused COMMIT, not the half-done write.
func TestLiveInTransactionAtomicWrite(t *testing.T) {
	db, ctx, done := liveDB(t)
	t.Cleanup(done) // registered first, so it runs after every drop below

	srv, err := NewServer(ctx, db)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	t.Cleanup(func() { srv.Close() })

	const table, job, other = "tempdb.dbo.gosmo_live_tx_atomic", "gosmo_live_tx_atomic_job", "gosmo_live_tx_atomic_job2"
	dropJob := func(name string) string {
		return "IF EXISTS (SELECT 1 FROM msdb.dbo.sysjobs WHERE name = N'" + name + "') EXEC msdb.dbo.sp_delete_job @job_name = N'" + name + "'"
	}
	for _, stmt := range []string{
		"DROP TABLE IF EXISTS " + table, dropJob(job), dropJob(other),
	} {
		t.Cleanup(func() {
			if _, err := db.ExecContext(context.Background(), stmt); err != nil {
				t.Errorf("cleanup %s: %v", stmt, err)
			}
		})
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	if _, err := db.ExecContext(ctx, "CREATE TABLE "+table+" (n int NOT NULL)"); err != nil {
		t.Fatalf("create table: %v", err)
	}
	j, err := srv.CreateJob(ctx, CreateJobRequest{Name: job})
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	insert := func(ctx context.Context, n string) {
		t.Helper()
		if _, err := srv.DatabaseRef("tempdb").exec(ctx, "INSERT INTO "+table+" VALUES ("+n+")"); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	rows := func() int {
		t.Helper()
		var n int
		if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table).Scan(&n); err != nil {
			t.Fatalf("count: %v", err)
		}
		return n
	}
	schedules := func() int {
		t.Helper()
		var n int
		if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM msdb.dbo.sysjobschedules js
			JOIN msdb.dbo.sysjobs j ON j.job_id = js.job_id WHERE j.name = @p1`, job).Scan(&n); err != nil {
			t.Fatalf("count schedules: %v", err)
		}
		return n
	}
	// sp_add_jobschedule succeeds, then sp_update_schedule refuses the owner:
	// the failure comes after a statement of the write has already run.
	badSchedule := CreateScheduleRequest{Name: "gosmo_live_tx_atomic_sched", FreqType: 1, OwnerLoginName: "gosmo_no_such_login"}

	t.Run("failure keeps the caller's transaction", func(t *testing.T) {
		err := srv.InTransaction(ctx, func(ctx context.Context) error {
			insert(ctx, "1")
			_, err := srv.CreateJob(ctx, CreateJobRequest{Name: job}) // a duplicate
			if err == nil {
				t.Error("CreateJob of a duplicate name succeeded")
			}
			var trancount, options int
			if err := srv.queryRowScan(ctx, "SELECT @@TRANCOUNT, @@OPTIONS", nil, &trancount, &options); err != nil {
				t.Fatalf("read session state: %v", err)
			}
			if trancount != 1 {
				t.Errorf("@@TRANCOUNT after the failed write = %d, want 1: the write ended the caller's transaction", trancount)
			}
			if options&16384 != 0 {
				t.Error("XACT_ABORT is on after the failed write")
			}
			return err
		})
		if err == nil {
			t.Fatal("InTransaction returned nil")
		}
		if n := rows(); n != 0 {
			t.Errorf("%d rows committed, want the insert before the failure rolled back", n)
		}
	})

	t.Run("a tolerated failure refuses COMMIT", func(t *testing.T) {
		err := srv.InTransaction(ctx, func(ctx context.Context) error {
			insert(ctx, "1")
			if _, err := j.AddSchedule(ctx, badSchedule); err == nil {
				t.Error("AddSchedule with an unknown owner succeeded")
			}
			insert(ctx, "2")
			return nil
		})
		if err == nil {
			t.Fatal("InTransaction committed after a failed atomic write")
		}
		if n := rows(); n != 0 {
			t.Errorf("%d rows committed, want none", n)
		}
		if n := schedules(); n != 0 {
			t.Errorf("%d schedules committed, want the half-done AddSchedule rolled back", n)
		}
	})

	t.Run("success commits", func(t *testing.T) {
		if err := srv.InTransaction(ctx, func(ctx context.Context) error {
			insert(ctx, "1")
			_, err := srv.CreateJob(ctx, CreateJobRequest{Name: other})
			return err
		}); err != nil {
			t.Fatalf("InTransaction: %v", err)
		}
		if n := rows(); n != 1 {
			t.Errorf("%d rows committed, want 1", n)
		}
		if _, err := srv.JobByName(ctx, other); err != nil {
			t.Errorf("job created inside the transaction: %v", err)
		}
	})

	t.Run("outside a transaction the failure still rolls back", func(t *testing.T) {
		if _, err := j.AddSchedule(ctx, badSchedule); err == nil {
			t.Error("AddSchedule with an unknown owner succeeded")
		}
		if n := schedules(); n != 0 {
			t.Errorf("%d schedules left behind, want none", n)
		}
	})
}
