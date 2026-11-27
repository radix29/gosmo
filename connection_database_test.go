package gosmo

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
)

type databasePoolScript struct {
	mu      sync.Mutex
	built   []ConnectionOptions
	queries []string
	refuse  string
	started chan struct{}
	block   bool
}

type databasePoolConnector struct {
	script   *databasePoolScript
	database string
}

func (c *databasePoolConnector) Driver() driver.Driver { return fakeQueryDriver{} }
func (c *databasePoolConnector) Connect(ctx context.Context) (driver.Conn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	c.script.mu.Lock()
	refuse := c.script.refuse
	c.script.mu.Unlock()
	if c.database == refuse {
		return nil, errors.New("login refused target database")
	}
	return &databasePoolConn{connector: c}, nil
}

type databasePoolConn struct{ connector *databasePoolConnector }

func (c *databasePoolConn) Prepare(string) (driver.Stmt, error) { return nil, driver.ErrSkip }
func (c *databasePoolConn) Close() error                        { return nil }
func (c *databasePoolConn) Begin() (driver.Tx, error)           { return databasePoolTx{}, nil }

type databasePoolTx struct{}

func (databasePoolTx) Commit() error   { return nil }
func (databasePoolTx) Rollback() error { return nil }
func (c *databasePoolConn) ExecContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Result, error) {
	rows, err := c.QueryContext(ctx, q, args)
	if rows != nil {
		rows.Close()
	}
	return driver.ResultNoRows, err
}
func (c *databasePoolConn) QueryContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	s := c.connector.script
	s.mu.Lock()
	s.queries = append(s.queries, c.connector.database+": "+q)
	s.mu.Unlock()
	if strings.Contains(q, "USE [target]") && c.connector.database != "target" {
		return nil, errors.New("cross-database USE is unavailable")
	}
	if s.block && c.connector.database != "master" {
		select {
		case s.started <- struct{}{}:
		default:
		}
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if db, inner, _, ok := unwrapProcRead(q, args); ok {
		if db != QuoteName(c.connector.database) {
			return nil, errors.New("cross-database RPC is unavailable")
		}
		q = inner
	}
	switch {
	case strings.Contains(q, "HAS_DBACCESS"):
		return &capRows{cols: 1, rows: [][]driver.Value{{nil}}}, nil
	case strings.Contains(q, "IS_ROLEMEMBER"):
		return &capRows{cols: 3, rows: [][]driver.Value{{"P", "SELECT", int64(1)}}}, nil
	default:
		return &capRows{cols: 1, rows: [][]driver.Value{{c.connector.database}}}, nil
	}
}

func databasePoolServer(t *testing.T, edition EngineEdition, script *databasePoolScript) *Server {
	t.Helper()
	opts := ConnectionOptions{Server: "test.database.windows.net", Database: "master",
		User: "test", Password: "test", Encrypt: "true", ApplicationName: "goSSMS",
		EntraCache: NewEntraCache(), MaxOpenConns: 3}
	applyDefaults(&opts)
	login, err := newRetargetingConnectorWith(opts, func(o ConnectionOptions) (driver.Connector, error) {
		script.mu.Lock()
		defer script.mu.Unlock()
		script.built = append(script.built, o)
		return &databasePoolConnector{script: script, database: o.Database}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	s := newServer(sql.OpenDB(login))
	s.login = login
	s.info = &ServerInfo{EngineEdition: edition, Collation: "SQL_Latin1_General_CP1_CI_AS"}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestDatabaseScopedReadsUseTargetLoginDatabase(t *testing.T) {
	for _, edition := range []EngineEdition{EngineAzureSQLDatabase, EngineFabricSQLDatabase} {
		t.Run(edition.String(), func(t *testing.T) {
			script := &databasePoolScript{}
			s := databasePoolServer(t, edition, script)
			d := s.DatabaseRef("target")
			caps, err := d.Capabilities(t.Context())
			if err != nil || !caps.Accessible || !caps.Has("SELECT") {
				t.Fatalf("direct target capabilities = %v, %v", caps, err)
			}
			var database string
			if err := d.queryRow(t.Context(), func(row *sql.Row) error { return row.Scan(&database) }, "SELECT DB_NAME()"); err != nil || database != "target" {
				t.Fatalf("target read = %q, %v", database, err)
			}
			if _, err := d.exec(t.Context(), "PRINT N'target'"); err != nil {
				t.Fatalf("target write = %v", err)
			}
			if err := d.withSessionIn(t.Context(), func(ctx context.Context, conn *sql.Conn) error {
				return conn.QueryRowContext(ctx, "SELECT DB_NAME()").Scan(&database)
			}); err != nil || database != "target" {
				t.Fatalf("target session = %q, %v", database, err)
			}
			if len(script.built) != 2 {
				t.Fatalf("built %d connectors, want root and one reused target", len(script.built))
			}
			before, after := script.built[0], script.built[1]
			after.Database = before.Database
			if !reflect.DeepEqual(after, before) {
				t.Fatal("routing changed credentials, destination, auth cache or transport")
			}
			for _, q := range script.queries {
				if strings.HasPrefix(q, "master:") || strings.Contains(q, "HAS_DBACCESS") {
					t.Errorf("target metadata used root context: %s", q)
				}
			}
		})
	}
}

func TestDatabaseScopedTargetRefusalIsAnErrorNotCachedDenial(t *testing.T) {
	script := &databasePoolScript{refuse: "target"}
	s := databasePoolServer(t, EngineAzureSQLDatabase, script)
	caps, err := s.DatabaseRef("target").Capabilities(t.Context())
	if err == nil || caps != nil {
		t.Fatalf("refused target = %v, %v, want the actual login error", caps, err)
	}
	script.mu.Lock()
	script.refuse = ""
	script.mu.Unlock()
	caps, err = s.DatabaseRef("target").Capabilities(t.Context())
	if err != nil || !caps.Accessible {
		t.Fatalf("target did not recover after refusal: %v, %v", caps, err)
	}
}

func TestInstanceDatabaseCapabilitiesKeepServerAccessProbe(t *testing.T) {
	for _, edition := range []EngineEdition{EngineEnterprise, EngineAzureManagedInst} {
		script := &databasePoolScript{}
		s := databasePoolServer(t, edition, script)
		caps, err := s.DatabaseRef("target").Capabilities(t.Context())
		if err != nil || caps.Accessible || len(script.built) != 1 {
			t.Fatalf("edition %s lost server access gating: %v, %v", edition, caps, err)
		}
	}

}

func TestDatabaseScopedPoolsShareConcurrentLoadsButIsolateDatabases(t *testing.T) {
	script := &databasePoolScript{}
	s := databasePoolServer(t, EngineAzureSQLDatabase, script)
	var wg sync.WaitGroup
	for range 8 {
		for _, name := range []string{"one", "two"} {
			wg.Go(func() {
				var got string
				err := s.DatabaseRef(name).queryRow(t.Context(),
					func(row *sql.Row) error { return row.Scan(&got) }, "SELECT DB_NAME()")
				if err != nil || got != name {
					t.Errorf("database %q read %q, %v", name, got, err)
				}
			})
		}
	}
	wg.Wait()
	if len(script.built) != 3 {
		t.Fatalf("built %d connectors, want root and two target pools", len(script.built))
	}
	for _, name := range []string{"master", "one", "two"} {
		pool, err := s.databasePool(name)
		if err != nil {
			t.Fatal(err)
		}
		conn, err := pool.Conn(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		conn.Close()
		if pool.Stats().Idle == 0 {
			t.Fatalf("database %q has no idle session to release", name)
		}
	}
	if err := s.ReleaseIdleConnections(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"master", "one", "two"} {
		pool, err := s.databasePool(name)
		if err != nil || pool.Stats().OpenConnections != 0 {
			t.Errorf("database %q retained sessions after release: %v", name, err)
		}
	}
}

func TestDatabaseScopedPoolCloseCancelsTargetWork(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		script := &databasePoolScript{block: true, started: make(chan struct{}, 1)}
		s := databasePoolServer(t, EngineAzureSQLDatabase, script)
		done := make(chan error, 1)
		go func() {
			rows, err := s.DatabaseRef("target").query(context.Background(), "SELECT DB_NAME()")
			if rows != nil {
				rows.Close()
			}
			done <- err
		}()
		<-script.started
		pool, err := s.databasePool("target")
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatalf("target read after Close = %v, want cancellation", err)
		}
		if pool.Stats().OpenConnections != 0 {
			t.Fatal("Close left a target connection open")
		}
		if _, err := s.databasePool("another"); !errors.Is(err, context.Canceled) {
			t.Fatalf("new database pool after Close = %v", err)
		}
	})
}

func TestDatabaseScopedRoutingDoesNotEscapeTransactionOrScript(t *testing.T) {
	script := &databasePoolScript{}
	s := databasePoolServer(t, EngineAzureSQLDatabase, script)
	err := s.InTransaction(t.Context(), func(ctx context.Context) error {
		var got string
		return s.DatabaseRef("target").queryRow(ctx, func(row *sql.Row) error {
			return row.Scan(&got)
		}, "SELECT DB_NAME()")
	})
	if err == nil || !strings.Contains(err.Error(), "cross-database USE is unavailable") {
		t.Fatalf("cross-database transaction = %v, want the original session's refusal", err)
	}
	ctx, _ := WithScript(t.Context())
	if _, err := s.DatabaseRef("target").exec(ctx, "PRINT N'test'"); err != nil {
		t.Fatal(err)
	}
	if len(script.built) != 1 {
		t.Fatal("transaction or script opened a separate target connection")
	}
}

func TestDatabaseScopedPoolKeysFollowServerCollation(t *testing.T) {
	for _, collation := range []string{"SQL_Latin1_General_CP1_CI_AS", "Latin1_General_CS_AS"} {
		t.Run(collation, func(t *testing.T) {
			s := databasePoolServer(t, EngineAzureSQLDatabase, &databasePoolScript{})
			s.info.Collation = collation
			home, err := s.databasePool("master")
			if err != nil || home != s.db {
				t.Fatalf("home pool was replaced: %v", err)
			}
			upper, err := s.databasePool("Sales")
			if err != nil {
				t.Fatal(err)
			}
			lower, err := s.databasePool("sales")
			if err != nil {
				t.Fatal(err)
			}
			wantSame := collation == "SQL_Latin1_General_CP1_CI_AS"
			if (upper == lower) != wantSame {
				t.Fatalf("case-variant names share pool = %v, want %v", upper == lower, wantSame)
			}
		})
	}
}
