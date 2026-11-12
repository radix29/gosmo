package gosmo_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	mssql "github.com/microsoft/go-mssqldb"
	"github.com/radix29/gosmo"
	"github.com/radix29/gosmo/gosmotest"
)

// -- what this file pins ------------------------------------------------------
//
// The Script* methods below were reached by no test, offline or live
// (`deadcode -test -tags livedb`): their builders are tested, but not the
// read each method makes first nor its wiring of the result into the
// builder. A wrong select, a scan out of column order or the wrong builder
// went unnoticed. Each case runs the method over gosmotest's canned rows, so
// a change in the read's shape fails here. schedulerGroupSizes and
// clrServerTrigger carry logic of their own and are pinned through the
// public method that uses them.

var scriptWhen = time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC)

func TestScripterAccessorsReturnTheirParent(t *testing.T) {
	srv := gosmotest.New(t).Server()
	db := srv.DatabaseRef("app")
	if got := gosmo.NewScripter(db, gosmo.DefaultScriptOptions()).Database(); got != db {
		t.Errorf("Scripter.Database() = %p, want %p", got, db)
	}
	if got := gosmo.NewServerScripter(srv, gosmo.DefaultScriptOptions()).Server(); got != srv {
		t.Errorf("ServerScripter.Server() = %p, want %p", got, srv)
	}
}

func TestScriptDatabaseObjectsPrefixesTheDatabaseContext(t *testing.T) {
	f := gosmotest.New(t)
	f.On("tr.parent_class = 0", gosmotest.Rows([]any{"no_ddl", false, scriptWhen, scriptWhen,
		`[{"v":"DDL_DATABASE_LEVEL_EVENTS"}]`, "CREATE TRIGGER no_ddl ON DATABASE FOR DDL_DATABASE_LEVEL_EVENTS AS ROLLBACK",
		true, true, false, int64(1)}))
	f.On("HAS_PERMS_BY_NAME(DB_NAME(), 'DATABASE'", gosmotest.Rows([]any{true}))
	srv := f.Server()
	f.On("", gosmotest.Rows())
	sc := gosmo.NewScripter(srv.DatabaseRef("app"), gosmo.DefaultScriptOptions())
	ctx := context.Background()

	got, err := sc.ScriptDatabaseObjects(ctx, gosmo.ObjectScriptOptions{})
	if err != nil {
		t.Fatal(err)
	}
	list, err := sc.ScriptDatabaseObjectList(ctx, gosmo.ObjectScriptOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if want := sc.DatabaseContext() + list.String(); got != want {
		t.Errorf("ScriptDatabaseObjects =\n%s\nwant the context then the list:\n%s", got, want)
	}
	if !strings.HasPrefix(got, "USE [app];") || !strings.Contains(got, "CREATE TRIGGER no_ddl") {
		t.Errorf("ScriptDatabaseObjects =\n%s\nwant USE [app] first and the trigger scripted", got)
	}

	drop := gosmo.DefaultScriptOptions()
	drop.Verb = gosmo.ScriptDrop
	if _, err := gosmo.NewScripter(srv.DatabaseRef("app"), drop).ScriptDatabaseObjects(ctx, gosmo.ObjectScriptOptions{}); !errors.Is(err, gosmo.ErrInvalidRequest) {
		t.Errorf("DROP verb: err = %v, want ErrInvalidRequest", err)
	}
}

func TestScriptDatabaseAuditSpecificationReadsSpecAndDetails(t *testing.T) {
	f := gosmotest.New(t)
	f.On("FROM   sys.database_audit_specifications s", gosmotest.Rows(
		[]any{int64(65536), "spec_app", "8f0c2a4e-0000-0000-0000-000000000001", "audit_main", true, scriptWhen, scriptWhen}))
	f.OnFunc(func(q gosmotest.Query) bool {
		return strings.Contains(q.Text, "FROM   sys.database_audit_specification_details d") && q.Arg(0) == int64(65536)
	}, gosmotest.Rows(
		[]any{int64(65536), "SCHEMA_OBJECT_ACCESS_GROUP", "DATABASE", true, "SUCCESS AND FAILURE", nil, nil, nil}))
	sc := gosmo.NewScripter(f.Server().DatabaseRef("app"), gosmo.DefaultScriptOptions())

	got, err := sc.ScriptDatabaseAuditSpecification(context.Background(), "spec_app")
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{
		"WHERE name = N'spec_app'",
		"CREATE DATABASE AUDIT SPECIFICATION [spec_app]\nFOR SERVER AUDIT [audit_main]",
		"ADD (SCHEMA_OBJECT_ACCESS_GROUP)",
		"STATE = ON",
	} {
		if !strings.Contains(got, w) {
			t.Errorf("missing %q in:\n%s", w, got)
		}
	}
}

func TestScriptDatabaseScopedCredentialCarriesThePlaceholder(t *testing.T) {
	f := gosmotest.New(t)
	f.On("FROM   sys.database_scoped_credentials c", gosmotest.Rows(
		[]any{int64(3), "blob_cred", "SHARED ACCESS SIGNATURE", scriptWhen, scriptWhen}))
	sc := gosmo.NewScripter(f.Server().DatabaseRef("app"), gosmo.DefaultScriptOptions())

	got, err := sc.ScriptDatabaseScopedCredential(context.Background(), "blob_cred")
	if err != nil {
		t.Fatal(err)
	}
	want := "CREATE DATABASE SCOPED CREDENTIAL [blob_cred] WITH IDENTITY = N'SHARED ACCESS SIGNATURE', SECRET = N'" +
		gosmo.SecretPlaceholder + "';"
	if !strings.Contains(got, want) {
		t.Errorf("missing %q in:\n%s", want, got)
	}
}

func TestScriptServerRoleRestoresOwnerAndMembers(t *testing.T) {
	f := gosmotest.New(t)
	f.On("WHERE r.type = 'R' AND r.name = @p1", gosmotest.Rows(
		[]any{"ops", int64(300), false, "sa", []byte{1, 2}, scriptWhen, scriptWhen, `[{"v":"alice"},{"v":"bob"}]`}))
	sc := gosmo.NewServerScripter(f.Server(), gosmo.DefaultScriptOptions())

	got, err := sc.ScriptServerRole(context.Background(), "ops")
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{
		"CREATE SERVER ROLE [ops] AUTHORIZATION [sa];",
		"ALTER SERVER ROLE [ops] ADD MEMBER [alice];\nGO\nALTER SERVER ROLE [ops] ADD MEMBER [bob];",
	} {
		if !strings.Contains(got, w) {
			t.Errorf("missing %q in:\n%s", w, got)
		}
	}
}

func TestScriptBackupDeviceMapsTheCatalogType(t *testing.T) {
	f := gosmotest.New(t)
	f.On("FROM   sys.backup_devices", gosmotest.Rows([]any{"nightly", "DISK", "/var/opt/mssql/backup/nightly.bak"}))
	sc := gosmo.NewServerScripter(f.Server(), gosmo.DefaultScriptOptions())

	got, err := sc.ScriptBackupDevice(context.Background(), "nightly")
	if err != nil {
		t.Fatal(err)
	}
	want := "EXEC sp_addumpdevice @devtype = N'disk', @logicalname = N'nightly', @physicalname = N'/var/opt/mssql/backup/nightly.bak';"
	if !strings.Contains(got, want) {
		t.Errorf("missing %q in:\n%s", want, got)
	}
}

// serverTriggerRow is the ServerTriggerByName row of a trigger with no
// readable T-SQL definition: a CLR trigger, or an encrypted one.
func serverTriggerRow(isCLR bool) gosmotest.Responder {
	return gosmotest.Rows([]any{"audit_logon", false, scriptWhen, scriptWhen, `[{"v":"LOGON"}]`, nil, false, false, isCLR})
}

// A CLR server trigger's CREATE is rebuilt from master's catalog: the
// assembly method and EXECUTE AS from sys.server_assembly_modules, the events
// from sys.server_trigger_events, each read by the object id
// clrServerTrigger found.
func TestScriptServerTriggerRebuildsACLRTrigger(t *testing.T) {
	f := gosmotest.New(t)
	f.On("LEFT   JOIN sys.server_sql_modules m", serverTriggerRow(true))
	f.On("JOIN   sys.server_assembly_modules am ON am.object_id = tr.object_id", gosmotest.Rows([]any{int64(7)}))
	f.OnFunc(func(q gosmotest.Query) bool {
		// SUSER_NAME: a server trigger's EXECUTE AS names a login, not a user.
		return strings.Contains(q.Text, "JOIN   master.sys.assemblies a") &&
			strings.Contains(q.Text, "SUSER_NAME(NULLIF(am.execute_as_principal_id") && q.Arg(0) == int64(7)
	}, gosmotest.Rows([]any{"AuditAsm", "Audit.Triggers", "OnLogon", false, false, "audit_login"}))
	f.OnFunc(func(q gosmotest.Query) bool {
		return strings.Contains(q.Text, "FROM   sys.server_trigger_events te\nWHERE  te.object_id = @p1") && q.Arg(0) == int64(7)
	}, gosmotest.Rows([]any{"LOGON"}))
	sc := gosmo.NewServerScripter(f.Server(), gosmo.DefaultScriptOptions())

	got, err := sc.ScriptServerTrigger(context.Background(), "audit_logon")
	if err != nil {
		t.Fatal(err)
	}
	want := "CREATE TRIGGER [audit_logon] ON ALL SERVER\nWITH EXECUTE AS N'audit_login'\nAFTER LOGON\n" +
		"AS EXTERNAL NAME [AuditAsm].[Audit.Triggers].[OnLogon];"
	if !strings.Contains(got, want) {
		t.Errorf("missing %q in:\n%s", want, got)
	}
	if strings.Contains(got, "SET ANSI_NULLS") {
		t.Errorf("a CLR trigger got T-SQL SET options:\n%s", got)
	}
}

// No CLR row: the definition is unreadable (encrypted), which is refused —
// not scripted as an empty CREATE.
func TestScriptServerTriggerRefusesAnUnreadableNonCLRTrigger(t *testing.T) {
	f := gosmotest.New(t)
	f.On("LEFT   JOIN sys.server_sql_modules m", serverTriggerRow(false))
	f.On("JOIN   sys.server_assembly_modules am ON am.object_id = tr.object_id", gosmotest.Rows())
	sc := gosmo.NewServerScripter(f.Server(), gosmo.DefaultScriptOptions())

	if got, err := sc.ScriptServerTrigger(context.Background(), "audit_logon"); !errors.Is(err, gosmo.ErrUnsupported) {
		t.Errorf("ScriptServerTrigger = %q, %v; want ErrUnsupported", got, err)
	}
}

// poolFake serves one default-option resource pool, id 256, with affinity
// rows (group, mask), and the schedulers rule sched.
func poolFake(t *testing.T, affinity [][2]int64, sched gosmotest.Responder) *gosmotest.Fake {
	f := gosmotest.New(t)
	f.On("FROM   sys.resource_governor_resource_pools", gosmotest.Rows(
		[]any{int64(256), "pool_g", int64(0), int64(100), int64(100), int64(0), int64(100), int64(0), int64(0)}))
	var rows [][]any
	for _, a := range affinity {
		rows = append(rows, []any{int64(256), a[0], a[1]})
	}
	f.On("FROM   sys.resource_governor_resource_pool_affinity", gosmotest.Rows(rows...))
	f.On("FROM   sys.dm_os_schedulers", sched)
	return f
}

// schedulers is a sys.dm_os_schedulers read of n schedulers per group, in
// group order.
func schedulers(perGroup ...int) gosmotest.Responder {
	var rows [][]any
	id := int64(0)
	for g, n := range perGroup {
		for range n {
			rows = append(rows, []any{id, id, int64(g), int64(g), true, int64(0), int64(0), int64(0), int64(0), int64(0)})
			id++
		}
	}
	return gosmotest.Rows(rows...)
}

func countQueries(f *gosmotest.Fake, needle string) int {
	n := 0
	for _, q := range f.Queries() {
		if strings.Contains(q.Text, needle) {
			n++
		}
	}
	return n
}

// Group 0 maps bit n to scheduler n with no read; a later group's ids start
// after every scheduler of the groups below it, counted from one read of
// sys.dm_os_schedulers however many groups need it.
func TestScriptResourcePoolMapsAffinityPastGroupZero(t *testing.T) {
	f := poolFake(t, [][2]int64{{0, 0b11}, {1, 0b1}, {2, 0b10}}, schedulers(4, 2, 2))
	sc := gosmo.NewServerScripter(f.Server(), gosmo.DefaultScriptOptions())

	got, err := sc.ScriptResourcePool(context.Background(), "pool_g")
	if err != nil {
		t.Fatal(err)
	}
	if want := "AFFINITY SCHEDULER = (0 TO 1, 4, 7)"; !strings.Contains(got, want) {
		t.Errorf("missing %q in:\n%s", want, got)
	}
	if n := countQueries(f, "FROM   sys.dm_os_schedulers"); n != 1 {
		t.Errorf("schedulers read %d times, want once", n)
	}
}

func TestScriptResourcePoolGroupZeroReadsNoSchedulers(t *testing.T) {
	f := poolFake(t, [][2]int64{{0, 0b101}}, gosmotest.Fail(errors.New("must not be read")))
	sc := gosmo.NewServerScripter(f.Server(), gosmo.DefaultScriptOptions())

	got, err := sc.ScriptResourcePool(context.Background(), "pool_g")
	if err != nil {
		t.Fatal(err)
	}
	if want := "AFFINITY SCHEDULER = (0, 2)"; !strings.Contains(got, want) {
		t.Errorf("missing %q in:\n%s", want, got)
	}
}

// Msg 297 and 300 are the permission refusals of sys.dm_os_schedulers: the
// script is ErrUnsupported naming the permission, the server's error still
// reachable. Any other failure stays what it was.
func TestScriptResourcePoolSchedulerReadRefusals(t *testing.T) {
	for _, tc := range []struct {
		number      int32
		unsupported bool
	}{
		{297, true},
		{300, true},
		{208, false},
	} {
		cause := mssql.Error{Number: tc.number, Class: 14, Message: "schedulers refused"}
		f := poolFake(t, [][2]int64{{1, 0b1}}, gosmotest.Fail(cause))
		sc := gosmo.NewServerScripter(f.Server(), gosmo.DefaultScriptOptions())

		_, err := sc.ScriptResourcePool(context.Background(), "pool_g")
		if err == nil {
			t.Fatalf("Msg %d: want an error", tc.number)
		}
		if got := errors.Is(err, gosmo.ErrUnsupported); got != tc.unsupported {
			t.Errorf("Msg %d: errors.Is(ErrUnsupported) = %v, want %v (%v)", tc.number, got, tc.unsupported, err)
		}
		if tc.unsupported && !strings.Contains(err.Error(), "VIEW SERVER STATE") {
			t.Errorf("Msg %d: %v does not name the permission", tc.number, err)
		}
		if se, ok := gosmo.AsSQLError(err); !ok || se.Number != tc.number {
			t.Errorf("Msg %d: AsSQLError = %v, %v; want the server's error", tc.number, se, ok)
		}
	}
}

func TestQSMetricUnit(t *testing.T) {
	for _, tc := range []struct {
		m    gosmo.QSMetric
		unit gosmo.QSUnit
	}{
		{gosmo.QSMetricDuration, gosmo.QSUnitMicroseconds},
		{gosmo.QSMetricLogicalReads, gosmo.QSUnitPages},
		{gosmo.QSMetricRowCount, gosmo.QSUnitCount},
		{gosmo.QSMetricLogMemory, gosmo.QSUnitBytes},
	} {
		if u, ok := gosmo.QSMetricUnit(tc.m); !ok || u != tc.unit {
			t.Errorf("QSMetricUnit(%s) = %v, %v; want %v, true", tc.m, u, ok, tc.unit)
		}
	}
	if _, ok := gosmo.QSMetricUnit("no such metric"); ok {
		t.Error("QSMetricUnit knows an unknown metric")
	}
}
