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
	"Server.CycleErrorLog":     "rolls the error log over",
	"Database.ClearQueryStore": "discards the query store's contents",
	"Database.FlushQueryStore": "forces a query store write",
	"Database.SetOffline":      "takes the database offline",
	"Database.SetOnline":       "brings the database online",
	"Table.TruncateTable":      "deletes every row",
	"Statistic.Drop":           "drops the statistic",
	"Index.Disable":            "disables the index",
	"Index.Drop":               "drops the index",
	"Index.Enable":             "rebuilds the index",
	"Index.Reorganize":         "reorganizes the index",
	"Login.Disable":            "disables the login",
	"Login.Drop":               "drops the login",
	"Login.Enable":             "enables the login",
	"Job.Disable":              "disables the job",
	"Job.Drop":                 "drops the job",
	"Job.Enable":               "enables the job",
	"Job.Stop":                 "stops a running job",
}

// sweep records what ran and what failed, so the result is one report rather
// than a stream of failures in call order.
type sweep struct {
	t       *testing.T
	ctx     context.Context
	calls   int
	failed  []string
	refused []string        // gosmo's own version refusals; expected, not failures
	called  map[string]bool // every label reached, for the coverage check below
}

// call runs one read and records its outcome. A panic is recorded like an
// error: the sweep's value is in reaching the end of the list.
func (sw *sweep) call(label string, fn func() error) {
	sw.calls++
	if sw.called == nil {
		sw.called = map[string]bool{}
	}
	sw.called[label] = true
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
	case strings.HasPrefix(label, "Database.LatestResourceStats") && errors.Is(err, ErrNotFound):
		// sys.dm_db_resource_stats gains its first row a few seconds after a
		// database is created (3 s on a Managed Instance, 2026-09-10), and the
		// sweep's database is seconds old, so an empty view is a race rather
		// than a defect. The query ran, which is what is being swept.
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

// sweepMustCall names the reads that Phase 3's eight new object families
// added. Every one is listed by hand, because the thing this guards against
// is precisely the read that quietly stops being swept: the reflective half
// covers a listing only for as long as it stays an exported
// XxxContext(ctx) (…, error) on a type reflectSweep is pointed at, and a
// listing given a filter argument, moved to another receiver or renamed
// leaves no trace when it drops out. A sweep that reports "0 failures"
// because it made the call and a sweep that reports it because it never made
// the call read identically otherwise.
//
// Names are matched against the labels sw.call recorded, allowing for the
// two decorations the labels carry: the reflective half appends
// " [<instance>]" and the hand-driven half sometimes appends "(<arg>)".
var sweepMustCall = []string{
	// Types (Stage A/B): five listings, four by-name finders.
	"Database.SystemDataTypes",
	"Database.UserDefinedDataTypes",
	"Database.UserDefinedTableTypes",
	"Database.ClrTypes",
	"Database.XMLSchemaCollections",
	"Database.UserDefinedDataTypeByName",
	"Database.ClrTypeByName",
	"Database.XMLSchemaCollectionByName",
	"UserDefinedTableType.Columns",
	"XMLSchemaCollection.Definition",

	// The capability probe, which grew a class 5/6/10 block reading
	// sys.assemblies, sys.types and sys.xml_schema_collections — the same
	// version exposure as the listings above.
	"Database.Capabilities",

	// Assemblies.
	"Database.Assemblies",
	"Database.AssemblyByName",
	"Assembly.Files/Modules",

	// Rules and defaults.
	"Database.Rules",
	"Database.RuleByName",
	"Database.Defaults",
	"Database.DefaultByName",

	// Plan guides.
	"Database.PlanGuides",
	"Database.PlanGuideByName",

	// Service Broker: seven listings, seven by-name finders, and the two
	// DMV-backed reads, which are the ones a caller without VIEW DATABASE
	// STATE loses — they are separate calls precisely so that losing them
	// does not cost the queue listing as well.
	"Database.MessageTypes",
	"Database.MessageTypeByName",
	"Database.Contracts",
	"Database.ContractByName",
	"Database.BrokerQueues",
	"Database.BrokerQueueByName",
	"Database.BrokerServices",
	"Database.BrokerServiceByName",
	"Database.Routes",
	"Database.RouteByName",
	"Database.RemoteServiceBindings",
	"Database.RemoteServiceBindingByName",
	"Database.BrokerPriorities",
	"Database.BrokerPriorityByName",
	"Database.QueueMessageCounts",
	"Database.QueueMonitors",
	"BrokerQueue.MessageCount",

	// External resources.
	"Database.ExternalDataSources",
	"Database.ExternalDataSourceByName",
	"Database.ExternalFileFormats",
	"Database.ExternalFileFormatByName",
	"Database.ExternalLibraries",
	"Database.ExternalLibraryByName",

	// Tables sub-folders (Stage E).
	"Database.TableKindsPresent",
	"Database.TablesOfKind(user)",
	"Database.TablesOfKind(system)",
	"Database.TablesOfKind(filetable)",
	"Database.TablesOfKind(external)",
	"Database.TablesOfKind(graph)",

	// Database snapshots (Stage E).
	"Server.DatabaseSnapshots",
	"Server.SnapshotsOf",
	"Server.SnapshotFileDefaults",

	// The eleven scripters the new families added. Each opens with a by-name
	// catalog read, so each carries the same version exposure as a listing.
	"Scripter.ScriptUserDefinedDataType",
	"Scripter.ScriptUserDefinedTableType",
	"Scripter.ScriptClrType",
	"Scripter.ScriptXMLSchemaCollection",
	"Scripter.ScriptRule",
	"Scripter.ScriptDefault",
	"Scripter.ScriptAssembly",
	"Scripter.ScriptPlanGuide",

	// The seven Service Broker scripters, each of which opens with the
	// family's by-name read.
	"Scripter.ScriptMessageType",
	"Scripter.ScriptContract",
	"Scripter.ScriptBrokerQueue",
	"Scripter.ScriptBrokerService",
	"Scripter.ScriptRoute",
	"Scripter.ScriptRemoteServiceBinding",
	"Scripter.ScriptBrokerPriority",
	"Scripter.ScriptExternalDataSource",
	"Scripter.ScriptExternalFileFormat",
	"Scripter.ScriptExternalLibrary",

	// Phase 3 item 15: certificates, asymmetric keys, symmetric keys.
	"Database.Certificates",
	"Database.CertificateByName",
	"Certificate.Encoded",
	"Scripter.ScriptCertificate",
	"Database.AsymmetricKeys",
	"Database.AsymmetricKeyByName",
	"Scripter.ScriptAsymmetricKey",
	"Database.SymmetricKeys",
	"Database.SymmetricKeyByName",
	"Scripter.ScriptSymmetricKey",
}

// checkCoverage fails on any sweepMustCall entry no label matched. It runs
// after the sweep rather than instead of it: a read can be both called and
// broken, and both answers are wanted.
func (sw *sweep) checkCoverage() {
	sw.t.Helper()
	for _, want := range sweepMustCall {
		found := false
		for label := range sw.called {
			if label == want ||
				strings.HasPrefix(label, want+" [") ||
				strings.HasPrefix(label, want+"(") {
				found = true
				break
			}
		}
		if !found {
			sw.t.Errorf("sweep never called %s — it is in sweepMustCall but no label matched, "+
				"so this instance's answer for it is unknown, not clean", want)
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
	// Types, rules, defaults and a plan guide: one member each so the
	// Programmability reads have a row to return rather than only proving
	// their statement parses.
	//
	// CREATE RULE and CREATE DEFAULT must be the first statement in their
	// batch — the server rejects them outright otherwise, with a parse error
	// naming the statement rather than anything about the object. Each entry
	// here goes through its own Database.exec, which is its own batch, so
	// that holds; a caller batching them together would fail on every major.
	`CREATE TYPE dbo.sweep_alias FROM VARCHAR(20) NOT NULL`,
	`CREATE TYPE dbo.sweep_tabletype AS TABLE (id INT NOT NULL PRIMARY KEY, note NVARCHAR(50) NULL)`,
	`CREATE XML SCHEMA COLLECTION dbo.sweep_xsd AS N'<xsd:schema xmlns:xsd="http://www.w3.org/2001/XMLSchema"><xsd:element name="sweep" type="xsd:string"/></xsd:schema>'`,
	`CREATE RULE dbo.sweep_rule AS @value > 0`,
	`CREATE DEFAULT dbo.sweep_default AS 0`,
	`EXEC sp_create_plan_guide @name = N'sweep_pg',
	   @stmt = N'SELECT COUNT(*) FROM dbo.sweep_parent WHERE name = @name',
	   @type = N'SQL', @module_or_batch = NULL,
	   @params = N'@name nvarchar(100)',
	   @hints = N'OPTION (OPTIMIZE FOR (@name = N''one''))'`,
	// Service Broker: one object of each family, in dependency order — a
	// contract needs its message types, a service its queue and contract, a
	// broker priority both. The activation procedure takes no parameters
	// because an activated one is called with none.
	//
	// The remote service binding is missing from this list on purpose: it is
	// the one statement Azure SQL Managed Instance refuses (Msg 41906, at
	// compile time, which would take the rest of its batch with it), so
	// sweepServiceBroker creates it where the refusal can be tolerated.
	`CREATE MESSAGE TYPE [//gosmo/sweep/mt] VALIDATION = VALID_XML WITH SCHEMA COLLECTION dbo.sweep_xsd`,
	`CREATE CONTRACT [//gosmo/sweep/contract] ([//gosmo/sweep/mt] SENT BY ANY)`,
	`CREATE PROCEDURE dbo.usp_sweep_activate AS SET NOCOUNT ON`,
	`CREATE QUEUE dbo.sweep_queue WITH STATUS = ON, RETENTION = OFF,
	   ACTIVATION (STATUS = ON, PROCEDURE_NAME = dbo.usp_sweep_activate,
	               MAX_QUEUE_READERS = 2, EXECUTE AS OWNER),
	   POISON_MESSAGE_HANDLING (STATUS = ON) ON [PRIMARY]`,
	`CREATE SERVICE [//gosmo/sweep/service] ON QUEUE dbo.sweep_queue ([//gosmo/sweep/contract])`,
	`CREATE ROUTE sweep_route WITH SERVICE_NAME = N'//gosmo/sweep/service',
	   LIFETIME = 6000, ADDRESS = N'TCP://sweep.invalid:4022'`,
	`CREATE BROKER PRIORITY sweep_priority FOR CONVERSATION
	   SET (CONTRACT_NAME = [//gosmo/sweep/contract],
	        LOCAL_SERVICE_NAME = [//gosmo/sweep/service],
	        REMOTE_SERVICE_NAME = N'//gosmo/sweep/remote', PRIORITY_LEVEL = 7)`,
	`CREATE SYNONYM dbo.sweep_syn FOR dbo.sweep_parent`,
	`CREATE PARTITION FUNCTION sweep_pf (INT) AS RANGE RIGHT FOR VALUES (100, 200)`,
	`CREATE PARTITION SCHEME sweep_ps AS PARTITION sweep_pf ALL TO ([PRIMARY])`,
	`CREATE COLUMN MASTER KEY sweep_cmk WITH (KEY_STORE_PROVIDER_NAME = 'MSSQL_CERTIFICATE_STORE', KEY_PATH = 'CurrentUser/My/DEADBEEF')`,
	`CREATE COLUMN ENCRYPTION KEY sweep_cek WITH VALUES (COLUMN_MASTER_KEY = sweep_cmk, ALGORITHM = 'RSA_OAEP', ENCRYPTED_VALUE = 0x0123456789ABCDEF)`,
	`CREATE FUNCTION app.sweep_pred (@name AS SYSNAME) RETURNS TABLE WITH SCHEMABINDING
	   AS RETURN SELECT 1 AS ok WHERE @name = USER_NAME()`,
	`CREATE SECURITY POLICY app.sweep_policy
	   ADD FILTER PREDICATE app.sweep_pred(name) ON dbo.sweep_parent WITH (STATE = ON)`,
	// Password-protected, so the scratch database needs no master key. The
	// Certificate reads' columns are the ones a missing column would kill.
	`CREATE CERTIFICATE sweep_cert ENCRYPTION BY PASSWORD = N'Sw33p!Cert#Pass' WITH SUBJECT = N'gosmo sweep'`,
	`CREATE ASYMMETRIC KEY sweep_asymkey WITH ALGORITHM = RSA_2048 ENCRYPTION BY PASSWORD = N'Sw33p!Asym#Pass'`,
	`CREATE SYMMETRIC KEY sweep_symkey WITH ALGORITHM = AES_256
	   ENCRYPTION BY CERTIFICATE sweep_cert, PASSWORD = N'Sw33p!Sym#Pass'`,
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
	// the version behaves differently — or, in ScriptDatabase's case,
	// panics — against one that has none.
	d, err := srv.DatabaseByName(ctx, scratch.Name)
	if err != nil {
		t.Fatalf("DatabaseByName %s: %v", scratch.Name, err)
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
	sw.reflectSweep("Database", d.Name, d)

	// Tables, and everything reached through one. Both scratch tables are
	// swept rather than the first: a read that ignores the object it was
	// given passes on a single-table database.
	tables, err := d.Tables(ctx)
	if err != nil {
		t.Errorf("Tables: %v — every table-level read is unreachable", err)
	}
	if len(tables) == 0 {
		t.Error("scratch database has no tables — the table, index and statistic sweeps below prove nothing")
	}
	for _, tbl := range tables {
		name := tbl.Schema + "." + tbl.Name
		sw.reflectSweep("Table", name, tbl)

		// Index's context-only read is StorageInfo; its context-only writes
		// (Drop, Disable, Enable, Reorganize — context-only since the table
		// parameter went, 2026-09-23) are in sweepSkip. A read added later is
		// covered without anyone editing this list.
		idxs, err := tbl.Indexes(ctx)
		if err == nil {
			for _, ix := range idxs {
				sw.reflectSweep("Index", name+"."+ix.Name, ix)
				sw.call(fmt.Sprintf("Index.Fragmentation [%s.%s]", name, ix.Name), func() error {
					_, err := ix.Fragmentation(ctx, FragmentationSampled)
					return err
				})
			}
		}
		stats, err := tbl.Statistics(ctx)
		if err == nil {
			for _, st := range stats {
				sw.reflectSweep("Statistic", name+"."+st.Name, st)
			}
		}
	}

	// Logins and jobs as the instance already has them — read only, never
	// created or modified here.
	logins, err := srv.Logins(ctx)
	if err != nil {
		t.Errorf("Logins: %v", err)
	}
	for _, l := range logins {
		sw.reflectSweep("Login", l.Name, l)
	}
	jobs, err := srv.Jobs(ctx)
	if err != nil {
		t.Errorf("Jobs: %v", err)
	}
	for _, j := range jobs {
		sw.reflectSweep("Job", j.Name, j)
	}

	sweepTableKinds(sw, d)
	sweepProgrammability(sw, d)
	sweepServiceBroker(sw, d)
	sweepQueryStoreReports(sw, d)
	sweepKeys(sw, d)
	sweepScripter(sw, d)
	sweepServerCalls(sw, srv, info)

	sw.checkCoverage()

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

// sweepTableKinds drives the five table-family listings. Each takes a
// TableKind argument, so the reflective half cannot reach any of them — and
// the graph one is the only read in the sweep that names a column absent from
// the 2016 floor, which is what its refusal there proves.
func sweepTableKinds(sw *sweep, d *Database) {
	for _, kind := range []TableKind{
		TableKindUser, TableKindSystem, TableKindFileTable,
		TableKindExternal, TableKindGraph,
	} {
		sw.call("Database.TablesOfKind("+kind.String()+")", func() error {
			_, err := d.TablesOfKind(sw.ctx, kind)
			return err
		})
	}
}

// sweepServiceBroker drives the Service Broker by-name finders and the two
// DMV-backed reads, all of which the reflective half cannot reach: the
// finders take a name, and BrokerQueue.MessageCount hangs off a queue
// the listing returned.
//
// The seven listings themselves come from the reflective half, against the
// fixture objects sweepSchema created — except the remote service binding,
// which is created here because Azure SQL Managed Instance refuses the
// statement (Msg 41906) and the listing must still be swept there, with no
// row and no failure.
func sweepServiceBroker(sw *sweep, d *Database) {
	bindingCreated := true
	if _, err := d.exec(sw.ctx, `CREATE REMOTE SERVICE BINDING sweep_rsb
	   TO SERVICE N'//gosmo/sweep/remote' WITH USER = sweep_user, ANONYMOUS = OFF`); err != nil {
		bindingCreated = false
		sw.t.Logf("CREATE REMOTE SERVICE BINDING refused (expected on Managed Instance, "+
			"Msg 41906): %v", err)
	}

	sw.call("Database.MessageTypeByName", func() error {
		_, err := d.MessageTypeByName(sw.ctx, "//gosmo/sweep/mt")
		return err
	})
	sw.call("Database.ContractByName", func() error {
		c, err := d.ContractByName(sw.ctx, "//gosmo/sweep/contract")
		if err != nil {
			return err
		}
		// The second query, which a listing that returned a bare contract
		// would pass without: the fixture contract carries one message type.
		if len(c.Messages) != 1 {
			return fmt.Errorf("contract has %d messages, want 1 — the usage read found nothing",
				len(c.Messages))
		}
		return nil
	})
	sw.call("Database.BrokerQueueByName", func() error {
		q, err := d.BrokerQueueByName(sw.ctx, "dbo", "sweep_queue")
		if err != nil {
			return err
		}
		// The OUTER APPLY onto the queue's internal table — the part of the
		// read most likely to answer differently on another major.
		if q.FileGroup == "" {
			return errors.New("the queue's filegroup came back empty; the internal-table lookup found nothing")
		}
		return nil
	})
	sw.call("BrokerQueue.MessageCount", func() error {
		q, err := d.BrokerQueueByName(sw.ctx, "dbo", "sweep_queue")
		if err != nil {
			return err
		}
		_, err = q.MessageCount(sw.ctx)
		return err
	})
	sw.call("Database.BrokerServiceByName", func() error {
		s, err := d.BrokerServiceByName(sw.ctx, "//gosmo/sweep/service")
		if err != nil {
			return err
		}
		if len(s.Contracts) != 1 {
			return fmt.Errorf("service has %d contracts, want 1 — the usage read found nothing",
				len(s.Contracts))
		}
		return nil
	})
	sw.call("Database.RouteByName", func() error {
		_, err := d.RouteByName(sw.ctx, "sweep_route")
		return err
	})
	sw.call("Database.RemoteServiceBindingByName", func() error {
		_, err := d.RemoteServiceBindingByName(sw.ctx, "sweep_rsb")
		if !bindingCreated && errors.Is(err, ErrNotFound) {
			return nil
		}
		return err
	})
	sw.call("Database.BrokerPriorityByName", func() error {
		_, err := d.BrokerPriorityByName(sw.ctx, "sweep_priority")
		return err
	})
	sw.call("Database.QueueMessageCounts", func() error {
		counts, err := d.QueueMessageCounts(sw.ctx)
		if err != nil {
			return err
		}
		if len(counts) == 0 {
			return errors.New("no queue reported a message count; the sys.internal_tables join found nothing")
		}
		return nil
	})
}

// sweepProgrammability drives the reads the reflective half cannot reach:
// the by-name finders, which take a name, and the per-object reads that hang
// off an object the listing returned.
//
// The fixture objects sweepSchema creates are named here rather than looked
// up, so a listing that silently returned nothing cannot make the finders
// pass by never being called with anything.
//
// Assemblies, external data sources, external file formats and external
// libraries get their listings from the reflective half and no fixture: each
// needs a facility the sweep cannot turn on from a connection — CLR enabled
// plus a signed binary, or PolyBase, or Machine Learning Services. The
// listings still run, which is what the version exposure is about; only the
// by-name finders below go unexercised against a real row.
func sweepProgrammability(sw *sweep, d *Database) {
	sw.call("Database.Parameters", func() error {
		ps, err := d.Parameters(sw.ctx, "dbo", "usp_sweep")
		if err == nil && len(ps) != 1 {
			err = fmt.Errorf("usp_sweep has %d parameters, want 1", len(ps))
		}
		return err
	})
	sw.call("Database.UserDefinedDataTypeByName", func() error {
		_, err := d.UserDefinedDataTypeByName(sw.ctx, "dbo", "sweep_alias")
		return err
	})
	sw.call("Database.ClrTypeByName", func() error {
		// No CLR type exists here, so a not-found is the right answer and
		// not a failure: what is being swept is whether the query runs.
		_, err := d.ClrTypeByName(sw.ctx, "dbo", "sweep_clr_absent")
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		return err
	})
	sw.call("Database.XMLSchemaCollectionByName", func() error {
		_, err := d.XMLSchemaCollectionByName(sw.ctx, "dbo", "sweep_xsd")
		return err
	})
	sw.call("Database.RuleByName", func() error {
		_, err := d.RuleByName(sw.ctx, "dbo", "sweep_rule")
		return err
	})
	sw.call("Database.DefaultByName", func() error {
		_, err := d.DefaultByName(sw.ctx, "dbo", "sweep_default")
		return err
	})
	sw.call("Database.PlanGuideByName", func() error {
		_, err := d.PlanGuideByName(sw.ctx, "sweep_pg")
		return err
	})
	sw.call("Database.AssemblyByName", func() error {
		_, err := d.AssemblyByName(sw.ctx, "sweep_assembly_absent")
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		return err
	})
	sw.call("Database.ExternalDataSourceByName", func() error {
		_, err := d.ExternalDataSourceByName(sw.ctx, "sweep_eds_absent")
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		return err
	})
	sw.call("Database.ExternalFileFormatByName", func() error {
		_, err := d.ExternalFileFormatByName(sw.ctx, "sweep_eff_absent")
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		return err
	})
	sw.call("Database.ExternalLibraryByName", func() error {
		_, err := d.ExternalLibraryByName(sw.ctx, "sweep_lib_absent")
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		return err
	})

	// The table type's columns come off the internal table sys.table_types
	// points at, which is the read most likely to be wrong and the one a
	// listing alone would never exercise.
	sw.call("UserDefinedTableType.Columns", func() error {
		tt, err := d.UserDefinedTableTypeByName(sw.ctx, "dbo", "sweep_tabletype")
		if err != nil {
			return err
		}
		cols, err := tt.Columns(sw.ctx)
		if err != nil {
			return err
		}
		if len(cols) != 2 {
			return fmt.Errorf("table type has %d columns, want 2 — the internal-table lookup found the wrong object", len(cols))
		}
		return nil
	})

	sw.call("XMLSchemaCollection.Definition", func() error {
		c, err := d.XMLSchemaCollectionByName(sw.ctx, "dbo", "sweep_xsd")
		if err != nil {
			return err
		}
		def, err := c.Definition(sw.ctx)
		if err != nil {
			return err
		}
		if def == "" {
			return errors.New("XML_SCHEMA_NAMESPACE returned nothing for a collection that exists")
		}
		return nil
	})

	// Assembly's own reads, against whatever the instance already has —
	// Microsoft.SqlServer.Types is registered in every database, so this
	// reaches a real row without the sweep creating one.
	sw.call("Assembly.Files/Modules", func() error {
		asms, err := d.Assemblies(sw.ctx)
		if err != nil {
			return err
		}
		if len(asms) == 0 {
			return nil
		}
		if _, err := asms[0].Files(sw.ctx); err != nil {
			return err
		}
		_, err = asms[0].Modules(sw.ctx)
		return err
	})
}

// sweepQueryStoreReports drives the ten report methods, which take options
// and so are invisible to the reflective half.
func sweepQueryStoreReports(sw *sweep, d *Database) {
	opts := QueryStoreReportOptions{From: time.Now().Add(-24 * time.Hour)}

	// A query id from the store when it has one. Where it does not, 1 still
	// exercises the statement — the sweep asks whether the query runs on this
	// version, not whether the row exists.
	queryID := int64(1)
	if top, err := d.QueryStoreTopResourceQueries(sw.ctx, opts); err == nil && len(top) > 0 {
		queryID = top[0].QueryID
	}

	sw.call("Database.QueryStoreTopResourceQueries", func() error {
		_, err := d.QueryStoreTopResourceQueries(sw.ctx, opts)
		return err
	})
	sw.call("Database.QueryStoreForcedPlanQueries", func() error {
		_, err := d.QueryStoreForcedPlanQueries(sw.ctx, opts)
		return err
	})
	sw.call("Database.QueryStoreHighVariationQueries", func() error {
		_, err := d.QueryStoreHighVariationQueries(sw.ctx, opts)
		return err
	})
	sw.call("Database.QueryStoreRegressedQueries", func() error {
		_, err := d.QueryStoreRegressedQueries(sw.ctx, opts)
		return err
	})
	sw.call("Database.QueryStoreOverallConsumption", func() error {
		_, err := d.QueryStoreOverallConsumption(sw.ctx, opts)
		return err
	})
	sw.call("Database.QueryStoreTrackedQuery", func() error {
		_, err := d.QueryStoreTrackedQuery(sw.ctx, queryID, opts)
		return err
	})
	sw.call("Database.QueryStorePlans", func() error {
		_, err := d.QueryStorePlans(sw.ctx, queryID, opts)
		return err
	})
	sw.call("Database.QueryStoreQueryText", func() error {
		_, _, err := d.QueryStoreQueryText(sw.ctx, queryID)
		return err
	})
	sw.call("Database.QueryStoreWaitCategories", func() error {
		_, err := d.QueryStoreWaitCategories(sw.ctx, opts)
		return err
	})
	sw.call("Database.QueryStoreWaitingQueries", func() error {
		_, err := d.QueryStoreWaitingQueries(sw.ctx, "CPU", opts)
		return err
	})
}

// sweepKeys drives the certificate and key reads that take a name, which
// the reflective half cannot reach.
func sweepKeys(sw *sweep, d *Database) {
	sw.call("Database.CertificateByName", func() error {
		c, err := d.CertificateByName(sw.ctx, "sweep_cert")
		if err == nil && (c.KeyLength == 0 || c.Owner == "") {
			return fmt.Errorf("sweep_cert read back with KeyLength %d, Owner %q", c.KeyLength, c.Owner)
		}
		return err
	})
	sw.call("Certificate.Encoded", func() error {
		_, err := d.CertificateRef("sweep_cert").Encoded(sw.ctx)
		return err
	})
	sw.call("Scripter.ScriptCertificate", func() error {
		_, err := NewScripter(d, ScriptOptions{Verb: ScriptDropAndCreate}).ScriptCertificate(sw.ctx, "sweep_cert")
		return err
	})
	sw.call("Database.AsymmetricKeyByName", func() error {
		k, err := d.AsymmetricKeyByName(sw.ctx, "sweep_asymkey")
		if err == nil && (k.KeyLength != 2048 || k.Owner == "") {
			return fmt.Errorf("sweep_asymkey read back with KeyLength %d, Owner %q", k.KeyLength, k.Owner)
		}
		return err
	})
	sw.call("Scripter.ScriptAsymmetricKey", func() error {
		_, err := NewScripter(d, ScriptOptions{Verb: ScriptDropAndCreate}).ScriptAsymmetricKey(sw.ctx, "sweep_asymkey")
		return err
	})
	sw.call("Database.SymmetricKeyByName", func() error {
		k, err := d.SymmetricKeyByName(sw.ctx, "sweep_symkey")
		if err == nil && (k.KeyLength != 256 || k.Owner == "" || len(k.Encryptions) != 2 ||
			k.Encryptions[0].Name != "sweep_cert" || k.Encryptions[1].Kind != SymmetricKeyByPassword) {
			return fmt.Errorf("sweep_symkey read back as %+v", k)
		}
		return err
	})
	sw.call("Scripter.ScriptSymmetricKey", func() error {
		_, err := NewScripter(d, ScriptOptions{Verb: ScriptDropAndCreate}).ScriptSymmetricKey(sw.ctx, "sweep_symkey")
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

	str("ScriptDatabase", func() (string, error) { return sc.ScriptDatabase(sw.ctx) })
	str("ScriptTable", func() (string, error) { return sc.ScriptTable(sw.ctx, "dbo", "sweep_child") })
	str("ScriptView", func() (string, error) { return sc.ScriptView(sw.ctx, "dbo", "sweep_view") })
	str("ScriptStoredProcedure", func() (string, error) {
		return sc.ScriptStoredProcedure(sw.ctx, "dbo", "usp_sweep")
	})
	str("ScriptFunction", func() (string, error) { return sc.ScriptFunction(sw.ctx, "dbo", "sweep_fn") })
	str("ScriptTrigger", func() (string, error) {
		return sc.ScriptTrigger(sw.ctx, "dbo", "trg_sweep_child")
	})
	str("ScriptSelect", func() (string, error) { return sc.ScriptSelect(sw.ctx, "dbo", "sweep_parent") })
	str("ScriptInsert", func() (string, error) { return sc.ScriptInsert(sw.ctx, "dbo", "sweep_parent") })
	str("ScriptUpdate", func() (string, error) { return sc.ScriptUpdate(sw.ctx, "dbo", "sweep_parent") })
	str("ScriptDelete", func() (string, error) { return sc.ScriptDelete(sw.ctx, "dbo", "sweep_parent") })
	str("ScriptExecute", func() (string, error) { return sc.ScriptExecute(sw.ctx, "dbo", "usp_sweep") })
	str("ScriptFunctionCall", func() (string, error) {
		return sc.ScriptFunctionCall(sw.ctx, "dbo", "sweep_fn", "FN")
	})
	str("ScriptSchema", func() (string, error) { return sc.ScriptSchema(sw.ctx, "app") })
	str("ScriptUser", func() (string, error) { return sc.ScriptUser(sw.ctx, "sweep_user") })
	str("ScriptDatabaseRole", func() (string, error) { return sc.ScriptDatabaseRole(sw.ctx, "sweep_role") })
	str("ScriptSecurityPolicy", func() (string, error) {
		return sc.ScriptSecurityPolicy(sw.ctx, "app", "sweep_policy")
	})
	str("ScriptColumnMasterKey", func() (string, error) {
		return sc.ScriptColumnMasterKey(sw.ctx, "sweep_cmk")
	})
	str("ScriptColumnEncryptionKey", func() (string, error) {
		return sc.ScriptColumnEncryptionKey(sw.ctx, "sweep_cek")
	})
	str("ScriptIndex", func() (string, error) {
		return sc.ScriptIndex(sw.ctx, "dbo", "sweep_child", "IX_sweep_child_parent")
	})
	str("ScriptCheckConstraint", func() (string, error) {
		return sc.ScriptCheckConstraint(sw.ctx, "dbo", "sweep_child", "CK_sweep_child_id")
	})
	str("ScriptForeignKey", func() (string, error) {
		return sc.ScriptForeignKey(sw.ctx, "dbo", "sweep_child", "FK_sweep_child_parent")
	})
	str("ScriptSequence", func() (string, error) { return sc.ScriptSequence(sw.ctx, "dbo", "sweep_seq") })
	str("ScriptSynonym", func() (string, error) { return sc.ScriptSynonym(sw.ctx, "dbo", "sweep_syn") })
	str("ScriptPartitionFunction", func() (string, error) {
		return sc.ScriptPartitionFunction(sw.ctx, "sweep_pf")
	})
	str("ScriptPartitionScheme", func() (string, error) {
		return sc.ScriptPartitionScheme(sw.ctx, "sweep_ps")
	})

	// The new families' scripters. Each opens with a by-name catalog read, so
	// each is version-exposed exactly like a listing is, and none of them was
	// reachable from the sweep before Stage F.
	str("ScriptUserDefinedDataType", func() (string, error) {
		return sc.ScriptUserDefinedDataType(sw.ctx, "dbo", "sweep_alias")
	})
	str("ScriptUserDefinedTableType", func() (string, error) {
		return sc.ScriptUserDefinedTableType(sw.ctx, "dbo", "sweep_tabletype")
	})
	str("ScriptXMLSchemaCollection", func() (string, error) {
		return sc.ScriptXMLSchemaCollection(sw.ctx, "dbo", "sweep_xsd")
	})
	str("ScriptRule", func() (string, error) { return sc.ScriptRule(sw.ctx, "dbo", "sweep_rule") })
	str("ScriptDefault", func() (string, error) {
		return sc.ScriptDefault(sw.ctx, "dbo", "sweep_default")
	})
	str("ScriptPlanGuide", func() (string, error) { return sc.ScriptPlanGuide(sw.ctx, "sweep_pg") })

	// The seven Service Broker scripters. Each opens with its family's
	// by-name read, so each is version-exposed exactly like a listing.
	str("ScriptMessageType", func() (string, error) {
		return sc.ScriptMessageType(sw.ctx, "//gosmo/sweep/mt")
	})
	str("ScriptContract", func() (string, error) {
		return sc.ScriptContract(sw.ctx, "//gosmo/sweep/contract")
	})
	str("ScriptBrokerQueue", func() (string, error) {
		return sc.ScriptBrokerQueue(sw.ctx, "dbo", "sweep_queue")
	})
	str("ScriptBrokerService", func() (string, error) {
		return sc.ScriptBrokerService(sw.ctx, "//gosmo/sweep/service")
	})
	str("ScriptRoute", func() (string, error) { return sc.ScriptRoute(sw.ctx, "sweep_route") })
	str("ScriptBrokerPriority", func() (string, error) {
		return sc.ScriptBrokerPriority(sw.ctx, "sweep_priority")
	})

	// The remaining five script objects the sweep cannot create — a CLR type
	// and an assembly need CLR enabled and a signed binary, the three
	// external ones PolyBase or Machine Learning Services. A not-found is
	// therefore the right answer and not a failure: what is being swept is
	// whether the by-name read underneath runs on this version at all, which
	// is the half that touches the catalog.
	absent := func(label string, fn func() (string, error)) {
		sw.call("Scripter."+label, func() error {
			_, err := fn()
			if errors.Is(err, ErrNotFound) {
				return nil
			}
			return err
		})
	}
	// The remote service binding's scripter goes with them: the binding
	// exists on every box product and on none of Managed Instance, where
	// CREATE REMOTE SERVICE BINDING is refused outright.
	absent("ScriptRemoteServiceBinding", func() (string, error) {
		return sc.ScriptRemoteServiceBinding(sw.ctx, "sweep_rsb")
	})
	absent("ScriptClrType", func() (string, error) {
		return sc.ScriptClrType(sw.ctx, "dbo", "sweep_clr_absent")
	})
	absent("ScriptAssembly", func() (string, error) {
		return sc.ScriptAssembly(sw.ctx, "sweep_assembly_absent")
	})
	absent("ScriptExternalDataSource", func() (string, error) {
		return sc.ScriptExternalDataSource(sw.ctx, "sweep_eds_absent")
	})
	absent("ScriptExternalFileFormat", func() (string, error) {
		return sc.ScriptExternalFileFormat(sw.ctx, "sweep_eff_absent")
	})
	absent("ScriptExternalLibrary", func() (string, error) {
		return sc.ScriptExternalLibrary(sw.ctx, "sweep_lib_absent")
	})
}

// sweepServerCalls drives the server reads that take arguments: the error log
// and the filesystem enumeration.
func sweepServerCalls(sw *sweep, srv *Server, info *ServerInfo) {
	for _, lt := range []ErrorLogType{ErrorLogSQLServer, ErrorLogAgent} {
		sw.call(fmt.Sprintf("Server.EnumErrorLogs(%v)", lt), func() error {
			_, err := srv.EnumErrorLogs(sw.ctx, lt)
			return err
		})
		sw.call(fmt.Sprintf("Server.ReadLog(%v)", lt), func() error {
			_, err := srv.ReadLog(sw.ctx, lt, 0)
			return err
		})
		sw.call(fmt.Sprintf("Server.ReadLogFiltered(%v)", lt), func() error {
			_, err := srv.ReadLogFiltered(sw.ctx, lt, 0, LogSearch{Text1: "server"})
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
	sw.call("Server.EnumFileSystem", func() error {
		_, err := srv.EnumFileSystem(sw.ctx, path)
		return err
	})
	sw.call("Server.FileSystemExists", func() error {
		_, _, err := srv.FileSystemExists(sw.ctx, path)
		return err
	})

	// Database snapshots. The listing is reflective; these two take a name.
	// SnapshotFileDefaults is a read, not the create: it only asks
	// sys.master_files what the source's data files are, which is where a
	// snapshot statement most often goes wrong.
	sw.call("Server.SnapshotsOf", func() error {
		_, err := srv.SnapshotsOf(sw.ctx, "master")
		return err
	})
	sw.call("Server.SnapshotFileDefaults", func() error {
		specs, err := srv.SnapshotFileDefaults(sw.ctx, "master", "sweep_snap")
		if err != nil {
			return err
		}
		for _, sp := range specs {
			if strings.HasSuffix(strings.ToLower(sp.FileName), ".ldf") {
				return fmt.Errorf("snapshot file defaults include the log file %q — CREATE DATABASE ... AS SNAPSHOT OF rejects that", sp.FileName)
			}
		}
		return nil
	})
}
