package gosmo

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	mssql "github.com/microsoft/go-mssqldb"
)

// The fixtures in testdata/xevents were captured from a live instance (major
// 17, 2026-09-29): ring_buffer_batches.xml is a throwaway session's whole
// target_data, the event_file_* files one fn_xe_file_target_read_file row's
// event_data each, taken from system_health.

func readXEFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "xevents", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestDecodeRingBuffer(t *testing.T) {
	rb, err := DecodeRingBuffer(readXEFixture(t, "ring_buffer_batches.xml"))
	if err != nil {
		t.Fatal(err)
	}
	if rb.TotalEventsProcessed != 4 || rb.EventCount != 4 || rb.MemoryUsed != 757 || rb.Truncated {
		t.Errorf("header = %+v", *rb)
	}
	if len(rb.Events) != 4 {
		t.Fatalf("got %d events, want 4", len(rb.Events))
	}
	for i, ev := range rb.Events {
		if ev.Seq != uint64(i+1) {
			t.Errorf("event %d: Seq = %d, want %d", i, ev.Seq, i+1)
		}
	}

	last := rb.Events[3]
	if last.Name != "sql_batch_completed" || last.Package != "sqlserver" {
		t.Errorf("last event = %s.%s", last.Package, last.Name)
	}
	if want := time.Date(2026, 9, 29, 17, 46, 26, 868_000_000, time.UTC); !last.Timestamp.Equal(want) || last.Timestamp.Location() != time.UTC {
		t.Errorf("Timestamp = %v, want %v UTC", last.Timestamp, want)
	}
	// CDATA text keeps the characters XML would otherwise escape.
	if v, _ := last.Field("batch_text"); v.Value != `SELECT 1 AS [a<&>'b]; WAITFOR DELAY '00:00:00.050'` || v.IsXML || v.Type != "unicode_string" {
		t.Errorf("batch_text = %+v", v)
	}
	if v, _ := last.Field("result"); v.Value != "0" || v.Text != "OK" || v.Type != "rpc_return_result" {
		t.Errorf("result = %+v", v)
	}
	if v, ok := last.Action("session_id"); !ok || v.Value != "64" {
		t.Errorf("session_id action = %+v, %v", v, ok)
	}

	// A map-typed field: the key in Value, its text in Text.
	wait := rb.Events[2]
	if v, _ := wait.Field("wait_type"); v.Value != "308" || v.Text != "WAITFOR" || v.Type != "wait_types" {
		t.Errorf("wait_type = %+v", v)
	}
	if v, ok := wait.Field("wait_resource"); !ok || v.Value != "" {
		t.Errorf("empty CDATA wait_resource = %+v, %v", v, ok)
	}
}

func TestDecodeEventXMLFromEventFile(t *testing.T) {
	evs, err := DecodeEventXML(readXEFixture(t, "event_file_diagnostics.xml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 {
		t.Fatalf("got %d events", len(evs))
	}
	ev := evs[0]
	// An event_file read carries no <type>, and no event_sequence here.
	if v, _ := ev.Field("component"); v.Type != "" || v.Value != "3" || v.Text != "IO_SUBSYSTEM" {
		t.Errorf("component = %+v", v)
	}
	if ev.Seq != 0 {
		t.Errorf("Seq = %d without an event_sequence action", ev.Seq)
	}
	// An xml-typed field keeps its fragment as markup.
	v, _ := ev.Field("data")
	if !v.IsXML || !strings.HasPrefix(v.Value, "<ioSubsystem ") || !strings.HasSuffix(v.Value, "</ioSubsystem>") {
		t.Errorf("data = %+v", v)
	}

	evs, err = DecodeEventXML(readXEFixture(t, "event_file_error_reported.xml"))
	if err != nil {
		t.Fatal(err)
	}
	// callstack_rva is CDATA that looks like markup; it is text.
	cs, ok := evs[0].Action("callstack_rva")
	if !ok || cs.IsXML || !strings.HasPrefix(cs.Value, "<frame ") {
		t.Errorf("callstack_rva = %+v", cs)
	}
	if v, _ := evs[0].Field("message"); v.Value == "" || v.IsXML {
		t.Errorf("message = %+v", v)
	}
}

func TestDecodeEventXMLRejectsMalformed(t *testing.T) {
	if _, err := DecodeEventXML([]byte(`<event name="x" timestamp="yesterday"></event>`)); err == nil {
		t.Error("bad timestamp decoded")
	}
	if _, err := DecodeEventXML([]byte(`<event name="x"><data>`)); err == nil {
		t.Error("truncated XML decoded")
	}
}

func TestEventFilePattern(t *testing.T) {
	for _, tc := range []struct{ filename, want string }{
		{"system_health.xel", "system_health*.xel"},
		{`C:\xe\trace.XEL`, `C:\xe\trace*.xel`},
		{"/var/opt/mssql/log/s", "/var/opt/mssql/log/s*.xel"},
	} {
		es := &EventSession{Name: "s", Targets: []SessionTarget{{Package: "package0", Name: XETargetEventFile,
			Fields: []SessionField{{Name: "filename", Value: tc.filename, IsString: true}}}}}
		got, err := es.EventFilePattern()
		if err != nil || got != tc.want {
			t.Errorf("%q: got %q, %v; want %q", tc.filename, got, err, tc.want)
		}
	}
	if _, err := (&EventSession{Name: "s"}).EventFilePattern(); err == nil {
		t.Error("a session with no event_file target returned a pattern")
	}
}

// -- DDL ---------------------------------------------------------------------------

var batchEvent = SessionEvent{
	Package: "sqlserver", Name: "sql_batch_completed",
	Predicate: "([sqlserver].[database_name]=N'master')",
	Actions:   []string{"package0.event_sequence", "sqlserver.sql_text"},
	Fields:    []SessionField{{Name: "collect_batch_text", Value: "1"}},
}

var fileTarget = SessionTarget{Package: "package0", Name: "event_file", Fields: []SessionField{
	{Name: "filename", Value: "it's.xel", IsString: true},
	{Name: "max_file_size", Value: "20"},
}}

func TestCreateEventSessionStatement(t *testing.T) {
	spec := EventSessionSpec{
		Name:               "odd]name",
		Events:             []SessionEvent{batchEvent, {Package: "sqlserver", Name: "rpc_completed"}},
		Targets:            []SessionTarget{fileTarget, {Package: "package0", Name: "ring_buffer"}},
		MaxDispatchLatency: 3 * time.Second,
		RetentionMode:      XERetentionAllowSingleEventLoss,
	}
	got, err := spec.createStatement(xeServerScope)
	if err != nil {
		t.Fatal(err)
	}
	want := `CREATE EVENT SESSION [odd]]name] ON SERVER
ADD EVENT sqlserver.sql_batch_completed(SET collect_batch_text=(1)
    ACTION(package0.event_sequence,sqlserver.sql_text)
    WHERE ([sqlserver].[database_name]=N'master')),
ADD EVENT sqlserver.rpc_completed
ADD TARGET package0.event_file(SET filename=N'it''s.xel',max_file_size=(20)),
ADD TARGET package0.ring_buffer
WITH (EVENT_RETENTION_MODE=ALLOW_SINGLE_EVENT_LOSS,MAX_DISPATCH_LATENCY=3 SECONDS,MAX_EVENT_SIZE=0 KB,TRACK_CAUSALITY=OFF,STARTUP_STATE=OFF)`
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}

	// INFINITE, MAX_DURATION, and the database scope's ON clause.
	spec.MaxDispatchLatency, spec.MaxDuration = XEInfinite, 2*time.Hour
	got, _ = spec.createStatement(xeDatabaseScope)
	for _, w := range []string{"] ON DATABASE\n", "MAX_DISPATCH_LATENCY=INFINITE", "MAX_DURATION=7200 SECONDS"} {
		if !strings.Contains(got, w) {
			t.Errorf("missing %q in:\n%s", w, got)
		}
	}

	for _, bad := range []EventSessionSpec{
		{Events: []SessionEvent{batchEvent}},
		{Name: "no events"},
		{Name: "x", Events: []SessionEvent{{Name: "no_package"}}},
		{Name: "x", Events: []SessionEvent{batchEvent}, Targets: []SessionTarget{{Name: "no_package"}}},
	} {
		if _, err := bad.createStatement(xeServerScope); err == nil {
			t.Errorf("%+v: want an error", bad)
		}
	}
}

func TestEventSessionStateAndDropStatements(t *testing.T) {
	ctx, col := WithScript(context.Background())
	es := (&Server{}).EventSessionRef("s]1")
	if err := es.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if err := es.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if err := es.Drop(ctx); err != nil {
		t.Fatal(err)
	}
	azure := &Server{info: &ServerInfo{EngineEdition: int(EngineAzureSQLDatabase)}}
	db := azure.DatabaseRef("AppDB").EventSessionRef("d")
	if err := db.Start(ctx); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"ALTER EVENT SESSION [s]]1] ON SERVER STATE = START",
		"ALTER EVENT SESSION [s]]1] ON SERVER STATE = STOP",
		"DROP EVENT SESSION [s]]1] ON SERVER",
		useAppDB + "ALTER EVENT SESSION [d] ON DATABASE STATE = START",
	}
	if got := col.Statements(); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("got %q\nwant %q", got, want)
	}
	if es.IsRunning {
		t.Error("IsRunning mirrored while scripting")
	}
}

// TestDatabaseScopedEventSessionsRefusedOffAzure pins the refusal every
// database-scoped verb gives on a box product, where the catalog views and
// ON DATABASE DDL do not exist (Msg 208 on 13, 14 and 17 before it). Script
// mode is refused too: the captured statement would not replay. The server
// has no connection, so a verb that slipped past the guard panics.
func TestDatabaseScopedEventSessionsRefusedOffAzure(t *testing.T) {
	ctx, col := WithScript(context.Background())
	for _, edition := range []EngineEdition{EngineEnterprise, EngineStandard, 0} {
		s := &Server{info: &ServerInfo{EngineEdition: int(edition), VersionMajor: 17}}
		d := s.DatabaseRef("AppDB")
		es := d.EventSessionRef("d")
		spec := EventSessionSpec{Name: "d", Events: []SessionEvent{{Package: "sqlserver", Name: "sql_batch_completed"}}}
		for name, call := range map[string]func() error{
			"EventSessions":      func() error { _, err := d.EventSessions(ctx); return err },
			"EventSessionByName": func() error { _, err := d.EventSessionByName(ctx, "d"); return err },
			"CreateEventSession": func() error { _, err := d.CreateEventSession(ctx, spec); return err },
			"Status":             func() error { _, err := es.Status(ctx); return err },
			"ReadRingBuffer":     func() error { _, err := es.ReadRingBuffer(ctx); return err },
			"Start":              func() error { return es.Start(ctx) },
			"Stop":               func() error { return es.Stop(ctx) },
			"AddTarget":          func() error { return es.AddTarget(ctx, RingBufferTarget(0)) },
			"Drop":               func() error { return es.Drop(ctx) },
			"Alter":              func() error { return es.Alter(ctx, spec) },
			"ScriptEventSession": func() error { _, err := NewScripter(d, ScriptOptions{}).ScriptEventSession(ctx, "d"); return err },
		} {
			if err := call(); !errors.Is(err, ErrUnsupportedVersion) {
				t.Errorf("edition %d: %s: err = %v, want ErrUnsupportedVersion", edition, name, err)
			}
		}
	}
	if got := col.Statements(); len(got) != 0 {
		t.Errorf("refused writes were captured: %q", got)
	}
}

func TestAlterEventSessionDiff(t *testing.T) {
	cur := &EventSession{
		server: &Server{}, Name: "s",
		RetentionMode: XERetentionAllowSingleEventLoss, MaxDispatchLatency: 30 * time.Second,
		MaxMemory: 4096, MemoryPartitionMode: XEPartitionNone,
		Events: []SessionEvent{
			batchEvent,
			{Package: "sqlserver", Name: "rpc_completed"},
			{Package: "sqlos", Name: "wait_info", Actions: []string{"sqlserver.session_id"}},
		},
		Targets: []SessionTarget{fileTarget, {Package: "package0", Name: "ring_buffer"}},
	}

	// Nothing changed: actions in another order and case are the same set.
	same := cur.Spec()
	same.Events[0].Actions = []string{"SQLSERVER.sql_text", "package0.event_sequence"}
	if d := same.alterStatements(cur); len(d.statements) != 0 || d.stoppedOnly != "" {
		t.Errorf("no-op diff = %+v", d)
	}

	want := cur.Spec()
	want.Events = []SessionEvent{
		batchEvent, // unchanged
		{Package: "sqlos", Name: "wait_info", Actions: []string{"sqlserver.sql_text"}}, // changed
		{Package: "sqlserver", Name: "error_reported"},                                 // new
	} // rpc_completed dropped
	want.Targets = []SessionTarget{{Package: "package0", Name: "event_file", Fields: []SessionField{
		{Name: "filename", Value: "it's.xel", IsString: true}, {Name: "max_file_size", Value: "50"}}}}
	want.MaxMemory = 8192
	want.StartupState = true
	d := want.alterStatements(cur)
	head := "ALTER EVENT SESSION [s] ON SERVER\n"
	wantStmts := []string{
		head + "DROP EVENT sqlos.wait_info,\nDROP EVENT sqlserver.rpc_completed",
		head + "DROP TARGET package0.event_file,\nDROP TARGET package0.ring_buffer",
		head + "ADD EVENT sqlos.wait_info(ACTION(sqlserver.sql_text)),\nADD EVENT sqlserver.error_reported",
		head + "ADD TARGET package0.event_file(SET filename=N'it''s.xel',max_file_size=(50))",
		head + "WITH (STARTUP_STATE=ON)",
	}
	if strings.Join(d.statements, "|") != strings.Join(wantStmts, "|") {
		t.Errorf("statements:\n%s\nwant:\n%s", strings.Join(d.statements, "\n--\n"), strings.Join(wantStmts, "\n--\n"))
	}
	if d.stoppedOnly != head+"WITH (MAX_MEMORY=8192 KB)" {
		t.Errorf("stoppedOnly = %q", d.stoppedOnly)
	}

	// The spec's "server default" zeros leave the session's values alone.
	zero := cur.Spec()
	zero.RetentionMode, zero.MaxDispatchLatency, zero.MaxMemory, zero.MemoryPartitionMode = "", 0, 0, ""
	if d := zero.alterStatements(cur); d.stoppedOnly != "" {
		t.Errorf("zero options altered: %q", d.stoppedOnly)
	}
}

// xeCanned is a canned reply of one or more rows, its column count taken
// from the first.
func xeCanned(match string, rows ...[]driver.Value) cannedRow {
	return cannedRow{match: match, cols: make([]string, len(rows[0])), rows: rows}
}

// eventSessionRows cans the five catalog reads behind EventSessionByName for
// one session named s.
func eventSessionRows(running bool) []cannedRow {
	return []cannedRow{
		xeCanned("s.event_retention_mode_desc", []driver.Value{int64(7), "s", false, running,
			XERetentionAllowSingleEventLoss, int64(30000), int64(4096), int64(0), XEPartitionNone, false, int64(0), int64(3)}),
		xeCanned("e.predicate", []driver.Value{int64(7), int64(1), "sqlserver", "rpc_completed", nil}),
		xeCanned("FROM   sys.server_event_session_actions", []driver.Value{int64(7), int64(1), "sqlserver", "sql_text"}),
		xeCanned("FROM   sys.server_event_session_targets", []driver.Value{int64(7), int64(2), "package0", "ring_buffer"}),
		xeCanned("FROM   sys.server_event_session_fields",
			[]driver.Value{int64(7), int64(2), "max_events_limit", "100", "int"},
			[]driver.Value{int64(7), int64(1), "collect_statement", "1", "int"}),
	}
}

func TestEventSessionByNameAssemblesChildren(t *testing.T) {
	s := captureServer(t, 17)
	captured.reset(eventSessionRows(true)...)
	es, err := s.EventSessionByName(t.Context(), "s")
	if err != nil {
		t.Fatal(err)
	}
	if !es.IsRunning || es.MaxDispatchLatency != 30*time.Second || es.MaxMemory != 4096 || es.DroppedEvents != 3 {
		t.Errorf("session = %+v", es)
	}
	if len(es.Events) != 1 || es.Events[0].Actions[0] != "sqlserver.sql_text" ||
		len(es.Events[0].Fields) != 1 || es.Events[0].Fields[0].Name != "collect_statement" {
		t.Errorf("events = %+v", es.Events)
	}
	if len(es.Targets) != 1 || len(es.Targets[0].Fields) != 1 || es.Targets[0].Fields[0].Value != "100" {
		t.Errorf("targets = %+v", es.Targets)
	}
	if es.Server() != s || es.Database() != nil {
		t.Error("back-pointers not set")
	}

	captured.reset()
	if _, err := s.EventSessionByName(t.Context(), "missing"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing session: %v", err)
	}
}

// TestAlterStopsARunningSessionOnlyForOptions pins the stop window: options a
// running session refuses are applied between a STOP and a START, and nothing
// else is.
func TestAlterStopsARunningSessionOnlyForOptions(t *testing.T) {
	for _, tc := range []struct {
		name    string
		running bool
		spec    func(EventSessionSpec) EventSessionSpec
		want    []string
	}{
		{"running, option", true, func(s EventSessionSpec) EventSessionSpec { s.MaxMemory = 8192; return s }, []string{
			"ALTER EVENT SESSION [s] ON SERVER STATE = STOP",
			"ALTER EVENT SESSION [s] ON SERVER\nWITH (MAX_MEMORY=8192 KB)",
			"ALTER EVENT SESSION [s] ON SERVER STATE = START",
		}},
		{"stopped, option", false, func(s EventSessionSpec) EventSessionSpec { s.MaxMemory = 8192; return s }, []string{
			"ALTER EVENT SESSION [s] ON SERVER\nWITH (MAX_MEMORY=8192 KB)",
		}},
		{"running, startup state only", true, func(s EventSessionSpec) EventSessionSpec { s.StartupState = true; return s }, []string{
			"ALTER EVENT SESSION [s] ON SERVER\nWITH (STARTUP_STATE=ON)",
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := captureServer(t, 17)
			captured.reset(eventSessionRows(tc.running)...)
			cur, err := s.EventSessionByName(t.Context(), "s")
			if err != nil {
				t.Fatal(err)
			}
			ctx, col := WithScript(t.Context())
			if err := s.EventSessionRef("s").Alter(ctx, tc.spec(cur.Spec())); err != nil {
				t.Fatal(err)
			}
			if got := col.Statements(); strings.Join(got, "|") != strings.Join(tc.want, "|") {
				t.Errorf("got %q\nwant %q", got, tc.want)
			}
		})
	}
}

func TestScriptEventSessionRoundTripsTheSpec(t *testing.T) {
	es := &EventSession{
		server: &Server{}, Name: "s", StartupState: true,
		RetentionMode: XERetentionNoEventLoss, MaxMemory: 8192, MaxEventSize: 2048,
		MemoryPartitionMode: XEPartitionPerCPU, TrackCausality: true,
		Events:  []SessionEvent{batchEvent},
		Targets: []SessionTarget{fileTarget},
	}
	got, err := buildEventSessionScript(es, ScriptOptions{Verb: ScriptDropAndCreate, IncludeIfNotExists: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{
		"IF EXISTS (SELECT 1 FROM sys.server_event_sessions WHERE name = N's')\n    DROP EVENT SESSION [s] ON SERVER;\nGO\n",
		"IF NOT EXISTS (SELECT 1 FROM sys.server_event_sessions WHERE name = N's')\nCREATE EVENT SESSION [s] ON SERVER",
		// A catalog latency of 0 is INFINITE, not "server default".
		"WITH (MAX_MEMORY=8192 KB,EVENT_RETENTION_MODE=NO_EVENT_LOSS,MAX_DISPATCH_LATENCY=INFINITE,MAX_EVENT_SIZE=2048 KB,MEMORY_PARTITION_MODE=PER_CPU,TRACK_CAUSALITY=ON,STARTUP_STATE=ON);\nGO\n",
	} {
		if !strings.Contains(got, w) {
			t.Errorf("missing %q in:\n%s", w, got)
		}
	}
	if strings.Contains(got, "STATE = START") {
		t.Error("the running state was scripted")
	}
}

func TestXECatalogIsCachedPerServer(t *testing.T) {
	s := captureServer(t, 17)
	captured.reset(xeCanned("FROM   sys.dm_xe_objects o", []driver.Value{
		"sqlserver", "rpc_completed", "event", "desc", "", "Analytic", "execution", ""}))
	for range 3 {
		objs, err := s.XEObjects(t.Context(), XEObjectEvent)
		if err != nil || len(objs) != 1 || objs[0].Channel != "Analytic" {
			t.Fatalf("XEObjects = %+v, %v", objs, err)
		}
		objs[0].Name = "mutated"
	}
	if n := captured.count("sys.dm_xe_objects o"); n != 1 {
		t.Errorf("catalog read %d times, want 1", n)
	}
	if _, err := s.XEObjects(t.Context(), XEObjectTarget); err != nil {
		t.Fatal(err)
	}
	if n := captured.count("sys.dm_xe_objects o"); n != 2 {
		t.Errorf("another kind shared the cache entry (%d reads)", n)
	}
}

// EventFiles lists a relative pattern in the error-log directory, keeps only
// the files the pattern matches (case aside), and sorts them oldest first
// whatever order the listing came in.
func TestEventFilesListsTheMatchingFilesInReadOrder(t *testing.T) {
	s := captureServer(t, 17)
	dmf := func(name string, dir bool) []driver.Value {
		return []driver.Value{`C:\log\` + name, name, map[bool]int64{true: 1}[dir], int64(1), time.Time{}}
	}
	captured.reset(
		xeCanned("ErrorLogFileName", []driver.Value{`C:\log\ERRORLOG`}),
		xeCanned("sys.dm_os_enumerate_filesystem",
			dmf("zz_0_300.xel", false), dmf("ZZ_0_100.XEL", false), dmf("zz_0_200.xel", false),
			dmf("zz_sub.xel", true), dmf("zz_0_150.txt", false), dmf("other_0_1.xel", false)),
	)
	got, err := s.EventFiles(t.Context(), "zz*.xel")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{`C:\log\ZZ_0_100.XEL`, `C:\log\zz_0_200.xel`, `C:\log\zz_0_300.xel`}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("got %q, want %q", got, want)
	}
	if q := captured.find("sys.dm_os_enumerate_filesystem"); q == "" {
		t.Fatal("the directory was not listed")
	}

	// A pattern with a directory lists that directory, and asks nothing of
	// the error log.
	captured.reset(xeCanned("sys.dm_os_enumerate_filesystem", dmf("zz_0_1.xel", false)))
	if _, err := s.EventFiles(t.Context(), `/var/opt/mssql/log/zz*.xel`); err != nil {
		t.Fatal(err)
	}
	if captured.count("ErrorLogFileName") != 0 {
		t.Error("an absolute pattern read the error-log directory")
	}

	if _, err := s.EventFiles(t.Context(), "https://acct.blob.core.windows.net/xe/zz*.xel"); !errors.Is(err, ErrUnsupported) {
		t.Errorf("a URL: %v, want ErrUnsupported", err)
	}
}

func TestMatchWildcardFold(t *testing.T) {
	for _, tc := range []struct {
		pattern, name string
		want          bool
	}{
		{"system_health*.xel", "system_health_0_134.xel", true},
		{"system_health*.xel", "SYSTEM_HEALTH_0_134.XEL", true},
		{"system_health*.xel", "system_health.xel", true},
		{"system_health*.xel", "system_health_0_134.xel.bak", false},
		{"system_health*.xel", "xsystem_health_0.xel", false},
		{"a*b*c", "abc", true},
		{"a*b*c", "ac", false},
		{"abc*c", "abc", false},
		{"exact.xel", "exact.xel", true},
		{"exact.xel", "exact.xelx", false},
	} {
		if got := matchWildcardFold(tc.pattern, tc.name); got != tc.want {
			t.Errorf("matchWildcardFold(%q, %q) = %v", tc.pattern, tc.name, got)
		}
	}
}

// xeFailDriver fails every query with one SQL Server error.
type xeFailDriver struct{}
type xeFailConn struct{ captureConn }

var xeFailWith error

func (xeFailDriver) Open(string) (driver.Conn, error) { return &xeFailConn{}, nil }
func (c *xeFailConn) QueryContext(context.Context, string, []driver.NamedValue) (driver.Rows, error) {
	return nil, xeFailWith
}

func init() { sql.Register("xefail", xeFailDriver{}) }

// A cursor into a file rollover has deleted is refused with Msg 25722, which
// ReadEventFile names ErrEventFileGone; the same refusal of a read from the
// start is not that — there was no cursor to lose.
func TestReadEventFileNamesAFileRolloverDeleted(t *testing.T) {
	db, err := sql.Open("xefail", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	s := &Server{db: db, info: &ServerInfo{VersionMajor: 17}}
	xeFailWith = mssql.Error{Number: 25722, Class: 16, Message: `The offset 84480 is invalid for log file "C:\log\zz_0_1.xel".`}

	_, cur, err := s.ReadEventFile(t.Context(), "zz*.xel", EventFileCursor{File: `C:\log\zz_0_1.xel`, Offset: 84480}, 0)
	if !errors.Is(err, ErrEventFileGone) {
		t.Fatalf("err = %v, want ErrEventFileGone", err)
	}
	if se, ok := AsSQLError(err); !ok || se.Number != 25722 {
		t.Error("the server's error is no longer reachable")
	}
	if cur.File != `C:\log\zz_0_1.xel` {
		t.Errorf("cursor moved to %+v on a failed read", cur)
	}
	if _, _, err := s.ReadEventFile(t.Context(), "zz*.xel", EventFileCursor{}, 0); err == nil || errors.Is(err, ErrEventFileGone) {
		t.Errorf("from the start: %v", err)
	}
	xeFailWith = mssql.Error{Number: 208, Class: 16, Message: "Invalid object name"}
	if _, _, err := s.ReadEventFile(t.Context(), "zz*.xel", EventFileCursor{File: "f", Offset: 1}, 0); errors.Is(err, ErrEventFileGone) {
		t.Error("another error read as a rolled-over file")
	}
}
