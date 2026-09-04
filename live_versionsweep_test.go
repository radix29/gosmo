//go:build livedb

// Live version sweep: call every read gosmo exposes against the connected
// instance and report the ones the server rejects.
//
// This exists because a query that names a column the instance does not have
// fails the *whole* read — the object it serves is simply dead on that
// version, with nothing in a unit test to say so. Nine such defects were found
// the first time gosmo met a SQL Server 2017 instance, every one of them
// invisible to `go test ./...`. The sweep is the standing check: run it on the
// oldest instance available after any query changes.
//
//	go test -tags livedb . -run TestLiveVersionSweep -v -timeout 45m \
//	  -livedb 'sqlserver://sa:PASS@host?TrustServerCertificate=true'
//
// It creates one throwaway database holding one of every object kind it knows
// how to make, sweeps that, and drops it. Server-level reads run against the
// instance as it is; the logins and jobs it reads are whatever already exist,
// and it only ever reads them.
//
// The failures it reports are not all defects. On an instance without a
// database master key the certificate and asymmetric-key reads fail for that
// reason, and a read gated behind a permission the connecting login lacks
// fails for that one. Read the error, not just the count.
package gosmo

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

// sweepSkip names the methods the sweep must not call because they write.
// The list is a heuristic kept here, in the test, rather than in a shared
// helper: a method classified wrongly has to be visible at the point it is
// skipped. Keys are "<type>.<method>".
//
// Every entry is checked against a real method below, so a renamed or deleted
// method fails the test instead of silently un-skipping — or silently
// skipping nothing.
var sweepSkip = map[string]string{
	"Server.CycleErrorLogContext":     "rolls the error log over",
	"Database.ClearQueryStoreContext": "discards the query store's contents",
	"Database.FlushQueryStoreContext": "forces a query store write",
	"Database.SetOfflineContext":      "takes the database offline",
	"Database.SetOnlineContext":       "brings the database online",
	"Table.TruncateTableContext":      "deletes every row",
	"Statistic.DropContext":           "drops the statistic",
	"Login.DisableContext":            "disables the login",
	"Login.DropContext":               "drops the login",
	"Login.EnableContext":             "enables the login",
	"Job.DisableContext":              "disables the job",
	"Job.DropContext":                 "drops the job",
	"Job.EnableContext":               "enables the job",
	"Job.StopContext":                 "stops a running job",
}

// sweep records what ran and what failed, so the result is one report rather
// than a stream of failures in call order.
type sweep struct {
	t       *testing.T
	ctx     context.Context
	calls   int
	failed  []string
	refused []string // gosmo's own version refusals; expected, not failures
}

// call runs one read and records its outcome. A panic is recorded like an
// error: the sweep's value is in reaching the end of the list.
func (sw *sweep) call(label string, fn func() error) {
	sw.calls++
	err := func() (err error) {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("panic: %v", r)
			}
		}()
		return fn()
	}()
	switch {
	case err == nil:
	case errors.Is(err, ErrUnsupportedVersion):
		// gosmo declining a feature the instance is too old for is the gate
		// working, not a defect — the whole point of running the sweep on the
		// oldest instance available. Counted separately so an old major can
		// still be 0 failures.
		sw.refused = append(sw.refused, fmt.Sprintf("%s: %v", label, err))
	default:
		sw.failed = append(sw.failed, fmt.Sprintf("%s: %v", label, err))
	}
}

// reflectSweep calls every exported XxxContext(ctx) (…, error) method on recv
// that sweepSkip does not name. Methods taking arguments beyond the context
// are driven by hand further down; this half is what keeps a newly added
// listing covered without anyone remembering to add it here.
func (sw *sweep) reflectSweep(typeName, instance string, recv any) {
	ctxT := reflect.TypeOf((*context.Context)(nil)).Elem()
	errT := reflect.TypeOf((*error)(nil)).Elem()
	rv := reflect.ValueOf(recv)
	rt := rv.Type()
	ctxV := reflect.ValueOf(sw.ctx)

	for i := 0; i < rt.NumMethod(); i++ {
		m := rt.Method(i)
		ft := m.Type
		if ft.NumIn() != 2 || ft.In(1) != ctxT {
			continue
		}
		if ft.NumOut() == 0 || ft.Out(ft.NumOut()-1) != errT {
			continue
		}
		if _, skip := sweepSkip[typeName+"."+m.Name]; skip {
			continue
		}
		label := fmt.Sprintf("%s.%s [%s]", typeName, m.Name, instance)
		sw.call(label, func() error {
			out := m.Func.Call([]reflect.Value{rv, ctxV})
			if e := out[len(out)-1].Interface(); e != nil {
				return e.(error)
			}
			return nil
		})
	}
}

// checkSkipList fails on an entry naming a method that no longer exists, so
// the exclusions cannot rot into a list of names that skip nothing.
func checkSkipList(t *testing.T) {
	t.Helper()
	types := map[string]reflect.Type{
		"Server":    reflect.TypeOf(&Server{}),
		"Database":  reflect.TypeOf(&Database{}),
		"Table":     reflect.TypeOf(&Table{}),
		"Index":     reflect.TypeOf(&Index{}),
		"Statistic": reflect.TypeOf(&Statistic{}),
		"Login":     reflect.TypeOf(&Login{}),
		"Job":       reflect.TypeOf(&Job{}),
	}
	for key := range sweepSkip {
		typeName, method, ok := strings.Cut(key, ".")
		rt := types[typeName]
		if !ok || rt == nil {
			t.Errorf("sweepSkip key %q names no swept type", key)
			continue
		}
		if _, found := rt.MethodByName(method); !found {
			t.Errorf("sweepSkip names %s, which %s does not have — stale exclusion", key, rt)
		}
	}
}

// sweepSchema is one of every object kind the sweep knows how to make. Each
// statement exists to give some read a row to return: a read that finds
// nothing can succeed on a version its query cannot even run on.
var sweepSchema = []string{
	`CREATE SCHEMA app`,
	`CREATE TABLE dbo.sweep_parent (id INT NOT NULL PRIMARY KEY, name NVARCHAR(100) NOT NULL, amount DECIMAL(18,2) NULL)`,
	`CREATE TABLE dbo.sweep_child (id INT NOT NULL PRIMARY KEY, parent_id INT NOT NULL, note NVARCHAR(200) NULL,
	   CONSTRAINT FK_sweep_child_parent FOREIGN KEY (parent_id) REFERENCES dbo.sweep_parent (id),
	   CONSTRAINT CK_sweep_child_id CHECK (id > 0))`,
	`CREATE TABLE app.sweep_app_table (id INT NOT NULL PRIMARY KEY, payload XML NULL)`,
	`CREATE INDEX IX_sweep_child_parent ON dbo.sweep_child (parent_id) INCLUDE (note)`,
	`CREATE PRIMARY XML INDEX PXI_sweep_app_payload ON app.sweep_app_table (payload)`,
	`CREATE STATISTICS ST_sweep_parent_name ON dbo.sweep_parent (name)`,
	`INSERT dbo.sweep_parent (id, name, amount) VALUES (1, N'one', 1.00), (2, N'two', 2.00)`,
	`INSERT dbo.sweep_child (id, parent_id, note) VALUES (1, 1, N'a'), (2, 2, N'b')`,
	`UPDATE STATISTICS dbo.sweep_parent`,
	`CREATE VIEW dbo.sweep_view AS SELECT id, name FROM dbo.sweep_parent`,
	`CREATE PROCEDURE dbo.usp_sweep @id INT AS SELECT id FROM dbo.sweep_parent WHERE id = @id`,
	`CREATE FUNCTION dbo.sweep_fn (@id INT) RETURNS INT AS BEGIN RETURN @id * 2 END`,
	`CREATE TRIGGER trg_sweep_child ON dbo.sweep_child AFTER INSERT AS SET NOCOUNT ON`,
	`CREATE TRIGGER trg_sweep_ddl ON DATABASE FOR CREATE_TABLE AS SET NOCOUNT ON`,
	`CREATE SEQUENCE dbo.sweep_seq AS INT START WITH 1 INCREMENT BY 1`,
	`CREATE SYNONYM dbo.sweep_syn FOR dbo.sweep_parent`,
	`CREATE PARTITION FUNCTION sweep_pf (INT) AS RANGE RIGHT FOR VALUES (100, 200)`,
	`CREATE PARTITION SCHEME sweep_ps AS PARTITION sweep_pf ALL TO ([PRIMARY])`,
	`CREATE COLUMN MASTER KEY sweep_cmk WITH (KEY_STORE_PROVIDER_NAME = 'MSSQL_CERTIFICATE_STORE', KEY_PATH = 'CurrentUser/My/DEADBEEF')`,
	`CREATE COLUMN ENCRYPTION KEY sweep_cek WITH VALUES (COLUMN_MASTER_KEY = sweep_cmk, ALGORITHM = 'RSA_OAEP', ENCRYPTED_VALUE = 0x0123456789ABCDEF)`,
	`CREATE FUNCTION app.sweep_pred (@name AS SYSNAME) RETURNS TABLE WITH SCHEMABINDING
	   AS RETURN SELECT 1 AS ok WHERE @name = USER_NAME()`,
	`CREATE SECURITY POLICY app.sweep_policy
	   ADD FILTER PREDICATE app.sweep_pred(name) ON dbo.sweep_parent WITH (STATE = ON)`,
	`CREATE ROLE sweep_role`,
	`CREATE USER sweep_user WITHOUT LOGIN`,
	`ALTER ROLE sweep_role ADD MEMBER sweep_user`,
	`EXEC sp_addextendedproperty @name = N'sweep_prop', @value = N'sweep'`,
	`ALTER DATABASE CURRENT SET CHANGE_TRACKING = ON (CHANGE_RETENTION = 2 DAYS)`,
	`ALTER TABLE dbo.sweep_parent ENABLE CHANGE_TRACKING`,
	`ALTER DATABASE CURRENT SET QUERY_STORE = ON`,
	`ALTER DATABASE CURRENT SET QUERY_STORE (OPERATION_MODE = READ_WRITE)`,
	`SELECT COUNT(*) FROM dbo.sweep_parent WHERE name = N'one'`,
}

func TestLiveVersionSweep(t *testing.T) {
	checkSkipList(t)

	db, _, done := liveDB(t)
	defer done()

	// liveDB's own context is a minute; the sweep is several hundred round
	// trips and needs its own budget.
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Minute)
	defer cancel()

	srv, err := NewServer(ctx, db)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	info := srv.Info()
	if info == nil {
		t.Fatal("Server.Info() is nil — the sweep's whole point is the version it reports")
	}
	t.Logf("instance %s, major %d, version %s, platform %q",
		srv.Name(), info.VersionMajor, info.ProductVersion, info.Platform)

	scratch, drop := liveScratchDB(t, db, ctx, "gosmo_versionsweep_live")
	defer drop()

	// Re-read the scratch database through the Server NewServer built.
	// liveScratchDB's own Server carries no ServerInfo, and a read gated on
	// the version behaves differently — or, in ScriptDatabaseContext's case,
	// panics — against one that has none.
	d, err := srv.DatabaseByNameContext(ctx, scratch.Name())
	if err != nil {
		t.Fatalf("DatabaseByNameContext %s: %v", scratch.Name(), err)
	}
	for _, stmt := range sweepSchema {
		if _, err := d.exec(ctx, stmt); err != nil {
			// A setup statement the instance rejects is itself a version
			// finding, but the sweep still has to run: report and continue.
			t.Errorf("scratch schema %.70q: %v", strings.Join(strings.Fields(stmt), " "), err)
		}
	}

	sw := &sweep{t: t, ctx: ctx}

	sw.reflectSweep("Server", srv.Name(), srv)
	sw.reflectSweep("Database", d.Name(), d)

	// Tables, and everything reached through one. Both scratch tables are
	// swept rather than the first: a read that ignores the object it was
	// given passes on a single-table database.
	tables, err := d.TablesContext(ctx)
	if err != nil {
		t.Errorf("TablesContext: %v — every table-level read is unreachable", err)
	}
	if len(tables) == 0 {
		t.Error("scratch database has no tables — the table, index and statistic sweeps below prove nothing")
	}
	for _, tbl := range tables {
		name := tbl.Schema + "." + tbl.Name
		sw.reflectSweep("Table", name, tbl)

		// Index has no context-only reads today. It is swept anyway so that
		// one added later is covered without anyone editing this list.
		idxs, err := tbl.IndexesContext(ctx)
		if err == nil {
			for _, ix := range idxs {
				sw.reflectSweep("Index", name+"."+ix.Name, ix)
			}
		}
		stats, err := tbl.StatisticsContext(ctx)
		if err == nil {
			for _, st := range stats {
				sw.reflectSweep("Statistic", name+"."+st.Name, st)
			}
		}
	}

	// Logins and jobs as the instance already has them — read only, never
	// created or modified here.
	logins, err := srv.LoginsContext(ctx)
	if err != nil {
		t.Errorf("LoginsContext: %v", err)
	}
	for _, l := range logins {
		sw.reflectSweep("Login", l.Name, l)
	}
	jobs, err := srv.JobsContext(ctx)
	if err != nil {
		t.Errorf("JobsContext: %v", err)
	}
	for _, j := range jobs {
		sw.reflectSweep("Job", j.Name, j)
	}

	sweepQueryStoreReports(sw, d)
	sweepScripter(sw, d)
	sweepServerCalls(sw, srv, info)

	sort.Strings(sw.failed)
	sort.Strings(sw.refused)
	t.Logf("%d calls made, %d failed, %d refused as too old for this instance", sw.calls, len(sw.failed), len(sw.refused))
	for _, r := range sw.refused {
		t.Logf("refused (expected): %s", r)
	}
	for _, f := range sw.failed {
		t.Errorf("%s", f)
	}
}

// sweepQueryStoreReports drives the ten report methods, which take options
// and so are invisible to the reflective half.
func sweepQueryStoreReports(sw *sweep, d *Database) {
	opts := QueryStoreReportOptions{From: time.Now().Add(-24 * time.Hour)}

	// A query id from the store when it has one. Where it does not, 1 still
	// exercises the statement — the sweep asks whether the query runs on this
	// version, not whether the row exists.
	queryID := int64(1)
	if top, err := d.QueryStoreTopResourceQueriesContext(sw.ctx, opts); err == nil && len(top) > 0 {
		queryID = top[0].QueryID
	}

	sw.call("Database.QueryStoreTopResourceQueriesContext", func() error {
		_, err := d.QueryStoreTopResourceQueriesContext(sw.ctx, opts)
		return err
	})
	sw.call("Database.QueryStoreForcedPlanQueriesContext", func() error {
		_, err := d.QueryStoreForcedPlanQueriesContext(sw.ctx, opts)
		return err
	})
	sw.call("Database.QueryStoreHighVariationQueriesContext", func() error {
		_, err := d.QueryStoreHighVariationQueriesContext(sw.ctx, opts)
		return err
	})
	sw.call("Database.QueryStoreRegressedQueriesContext", func() error {
		_, err := d.QueryStoreRegressedQueriesContext(sw.ctx, opts)
		return err
	})
	sw.call("Database.QueryStoreOverallConsumptionContext", func() error {
		_, err := d.QueryStoreOverallConsumptionContext(sw.ctx, opts)
		return err
	})
	sw.call("Database.QueryStoreTrackedQueryContext", func() error {
		_, err := d.QueryStoreTrackedQueryContext(sw.ctx, queryID, opts)
		return err
	})
	sw.call("Database.QueryStorePlansContext", func() error {
		_, err := d.QueryStorePlansContext(sw.ctx, queryID, opts)
		return err
	})
	sw.call("Database.QueryStoreQueryTextContext", func() error {
		_, _, err := d.QueryStoreQueryTextContext(sw.ctx, queryID)
		return err
	})
	sw.call("Database.QueryStoreWaitCategoriesContext", func() error {
		_, err := d.QueryStoreWaitCategoriesContext(sw.ctx, opts)
		return err
	})
	sw.call("Database.QueryStoreWaitingQueriesContext", func() error {
		_, err := d.QueryStoreWaitingQueriesContext(sw.ctx, "CPU", opts)
		return err
	})
}

// sweepScripter drives every Scripter entry point against the scratch
// database's objects. A scripter method reads the catalog like any other, so
// it carries the same version exposure.
func sweepScripter(sw *sweep, d *Database) {
	sc := NewScripter(d, DefaultScriptOptions())

	str := func(label string, fn func() (string, error)) {
		sw.call("Scripter."+label, func() error { _, err := fn(); return err })
	}

	str("ScriptDatabaseContext", func() (string, error) { return sc.ScriptDatabaseContext(sw.ctx) })
	str("ScriptTableContext", func() (string, error) { return sc.ScriptTableContext(sw.ctx, "dbo", "sweep_child") })
	str("ScriptViewContext", func() (string, error) { return sc.ScriptViewContext(sw.ctx, "dbo", "sweep_view") })
	str("ScriptStoredProcedureContext", func() (string, error) {
		return sc.ScriptStoredProcedureContext(sw.ctx, "dbo", "usp_sweep")
	})
	str("ScriptFunctionContext", func() (string, error) { return sc.ScriptFunctionContext(sw.ctx, "dbo", "sweep_fn") })
	str("ScriptTriggerContext", func() (string, error) {
		return sc.ScriptTriggerContext(sw.ctx, "dbo", "trg_sweep_child")
	})
	str("ScriptSelectContext", func() (string, error) { return sc.ScriptSelectContext(sw.ctx, "dbo", "sweep_parent") })
	str("ScriptInsertContext", func() (string, error) { return sc.ScriptInsertContext(sw.ctx, "dbo", "sweep_parent") })
	str("ScriptUpdateContext", func() (string, error) { return sc.ScriptUpdateContext(sw.ctx, "dbo", "sweep_parent") })
	str("ScriptDeleteContext", func() (string, error) { return sc.ScriptDeleteContext(sw.ctx, "dbo", "sweep_parent") })
	str("ScriptExecuteContext", func() (string, error) { return sc.ScriptExecuteContext(sw.ctx, "dbo", "usp_sweep") })
	str("ScriptFunctionCallContext", func() (string, error) {
		return sc.ScriptFunctionCallContext(sw.ctx, "dbo", "sweep_fn", "FN")
	})
	str("ScriptSchemaContext", func() (string, error) { return sc.ScriptSchemaContext(sw.ctx, "app") })
	str("ScriptUserContext", func() (string, error) { return sc.ScriptUserContext(sw.ctx, "sweep_user") })
	str("ScriptDatabaseRoleContext", func() (string, error) { return sc.ScriptDatabaseRoleContext(sw.ctx, "sweep_role") })
	str("ScriptSecurityPolicyContext", func() (string, error) {
		return sc.ScriptSecurityPolicyContext(sw.ctx, "app", "sweep_policy")
	})
	str("ScriptColumnMasterKeyContext", func() (string, error) {
		return sc.ScriptColumnMasterKeyContext(sw.ctx, "sweep_cmk")
	})
	str("ScriptColumnEncryptionKeyContext", func() (string, error) {
		return sc.ScriptColumnEncryptionKeyContext(sw.ctx, "sweep_cek")
	})
	str("ScriptIndexContext", func() (string, error) {
		return sc.ScriptIndexContext(sw.ctx, "dbo", "sweep_child", "IX_sweep_child_parent")
	})
	str("ScriptCheckConstraintContext", func() (string, error) {
		return sc.ScriptCheckConstraintContext(sw.ctx, "dbo", "sweep_child", "CK_sweep_child_id")
	})
	str("ScriptForeignKeyContext", func() (string, error) {
		return sc.ScriptForeignKeyContext(sw.ctx, "dbo", "sweep_child", "FK_sweep_child_parent")
	})
	str("ScriptSequenceContext", func() (string, error) { return sc.ScriptSequenceContext(sw.ctx, "dbo", "sweep_seq") })
	str("ScriptSynonymContext", func() (string, error) { return sc.ScriptSynonymContext(sw.ctx, "dbo", "sweep_syn") })
	str("ScriptPartitionFunctionContext", func() (string, error) {
		return sc.ScriptPartitionFunctionContext(sw.ctx, "sweep_pf")
	})
	str("ScriptPartitionSchemeContext", func() (string, error) {
		return sc.ScriptPartitionSchemeContext(sw.ctx, "sweep_ps")
	})
}

// sweepServerCalls drives the server reads that take arguments: the error log
// and the filesystem enumeration.
func sweepServerCalls(sw *sweep, srv *Server, info *ServerInfo) {
	for _, lt := range []ErrorLogType{ErrorLogSQLServer, ErrorLogAgent} {
		sw.call(fmt.Sprintf("Server.EnumErrorLogsContext(%v)", lt), func() error {
			_, err := srv.EnumErrorLogsContext(sw.ctx, lt)
			return err
		})
		sw.call(fmt.Sprintf("Server.ReadLogContext(%v)", lt), func() error {
			_, err := srv.ReadLogContext(sw.ctx, lt, 0)
			return err
		})
		sw.call(fmt.Sprintf("Server.ReadLogFilteredContext(%v)", lt), func() error {
			_, err := srv.ReadLogFilteredContext(sw.ctx, lt, 0, LogSearch{Text1: "server"})
			return err
		})
	}

	path := info.DefaultDataPath
	if path == "" {
		sw.t.Log("no default data path — the filesystem enumeration is swept against the root")
		path = "C:\\"
		if !strings.Contains(info.Platform, "Windows") {
			path = "/"
		}
	}
	sw.call("Server.EnumFileSystemContext", func() error {
		_, err := srv.EnumFileSystemContext(sw.ctx, path)
		return err
	})
	sw.call("Server.FileSystemExistsContext", func() error {
		_, _, err := srv.FileSystemExistsContext(sw.ctx, path)
		return err
	})
}
