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
