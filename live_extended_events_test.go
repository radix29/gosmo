//go:build livedb

// Live verification of the Extended Events family: that the catalog reads back
// what the DDL wrote, that the scripter's output runs and reproduces the
// session, that the stop window really is needed and works, and that both
// readers see events a query just caused.
//
//	go test -tags livedb . -run 'TestLive.*EventSession|TestLiveXE' -v \
//	  -livedb 'sqlserver://sa:PASS@host?TrustServerCertificate=true'
//
// Every session is named gossms_test_xe_* and dropped in t.Cleanup. The
// event_file targets use relative file names, which land in the error-log
// directory; their .xel files are deleted after the drop where the server
// offers a way (deleteXEFiles) and logged by name where it doesn't.
// system_health is read, never altered.
package gosmo

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

// liveXEServer opens the live server, closing it in t.Cleanup so that drops
// registered afterwards run first.
func liveXEServer(t *testing.T) (*Server, *sql.DB, context.Context) {
	t.Helper()
	db, ctx, done := liveDBTimeout(t, 2*time.Minute)
	t.Cleanup(done)
	s, err := NewServer(ctx, db)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	return s, db, ctx
}

// dropXESessionAfter drops the session now (a leftover from an aborted run)
// and again in t.Cleanup, and deletes the .xel files its event_file target
// wrote both times — name*.xel in the error-log directory, where every
// event_file here writes (relative filenames named after the session).
func dropXESessionAfter(t *testing.T, s *Server, db *sql.DB, name string) {
	t.Helper()
	drop := func() {
		if _, err := db.ExecContext(context.Background(), fmt.Sprintf(
			"IF EXISTS (SELECT 1 FROM sys.server_event_sessions WHERE name = N'%s') DROP EVENT SESSION %s ON SERVER",
			escapeSingle(name), quoteIdent(name))); err != nil {
			t.Errorf("cleanup: %v", err)
			return
		}
		deleteXEFiles(t, s, db, name)
	}
	drop()
	t.Cleanup(drop)
}

// deleteXEFiles deletes the files a dropped session's event_file wrote. T-SQL
// has no file delete of its own, so it takes what the server offers without
// changing its configuration: sys.xp_delete_files (2019 and later, Linux
// included), else xp_cmdshell's del where it is already enabled. Anything
// left is logged by name — a server offering neither (2016 and 2017 with
// xp_cmdshell off) keeps the files, and the log says which. Runs after the
// DROP, which closes the file the session was writing.
func deleteXEFiles(t *testing.T, s *Server, db *sql.DB, name string) {
	t.Helper()
	ctx := context.Background()
	files, err := s.EventFiles(ctx, name+"*.xel")
	if err != nil || len(files) == 0 {
		if err != nil && !errors.Is(err, ErrUnsupported) {
			t.Logf("cleanup: listing %s*.xel: %v", name, err)
		}
		return
	}
	var deleteFiles, cmdshell bool
	if err := db.QueryRowContext(ctx, `
SELECT CAST(CASE WHEN OBJECT_ID(N'master.sys.xp_delete_files') IS NULL THEN 0 ELSE 1 END AS bit),
       CAST(ISNULL((SELECT value_in_use FROM sys.configurations WHERE name = N'xp_cmdshell'), 0) AS bit)`).
		Scan(&deleteFiles, &cmdshell); err != nil {
		t.Logf("cleanup: %v", err)
		return
	}
	for _, f := range files {
		switch {
		case deleteFiles:
			_, err = db.ExecContext(ctx, "EXEC master.sys.xp_delete_files @p1", f)
		case cmdshell:
			_, err = db.ExecContext(ctx, "DECLARE @c nvarchar(4000) = N'del /q \"' + @p1 + N'\"'; EXEC master.sys.xp_cmdshell @c, no_output", f)
		}
		if err != nil {
			t.Logf("cleanup: delete %s: %v", f, err)
		}
	}
	if left, err := s.EventFiles(ctx, name+"*.xel"); err == nil && len(left) > 0 {
		t.Logf("cleanup: left behind (no xp_delete_files, xp_cmdshell off): %q", left)
	}
}

// normalizedSpec compares two sessions' definitions ignoring order, which the
// catalog does not promise to keep.
func normalizedSpec(es *EventSession) EventSessionSpec {
	spec := es.Spec()
	spec.Name = ""
	for i := range spec.Events {
		spec.Events[i].Actions = sortedFold(spec.Events[i].Actions)
		slices.SortFunc(spec.Events[i].Fields, func(a, b SessionField) int { return strings.Compare(a.Name, b.Name) })
	}
	slices.SortFunc(spec.Events, func(a, b SessionEvent) int { return strings.Compare(a.QualifiedName(), b.QualifiedName()) })
	for i := range spec.Targets {
		slices.SortFunc(spec.Targets[i].Fields, func(a, b SessionField) int { return strings.Compare(a.Name, b.Name) })
	}
	slices.SortFunc(spec.Targets, func(a, b SessionTarget) int { return strings.Compare(a.QualifiedName(), b.QualifiedName()) })
	return spec
}

func specString(s EventSessionSpec) string { return fmt.Sprintf("%+v", s) }

func TestLiveEventSessionLifecycle(t *testing.T) {
	s, db, ctx := liveXEServer(t)
	const name = "gossms_test_xe_life"
	dropXESessionAfter(t, s, db, name)

	// A dedicated connection whose spid the predicate names, so the session
	// sees this test's batches and nobody else's.
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	var spid int
	if err := conn.QueryRowContext(ctx, "SELECT @@SPID").Scan(&spid); err != nil {
		t.Fatal(err)
	}

	spec := EventSessionSpec{
		Name: name,
		Events: []SessionEvent{{
			Package: "sqlserver", Name: "sql_batch_completed",
			Predicate: fmt.Sprintf("([sqlserver].[session_id]=(%d))", spid),
			Actions:   []string{"package0.event_sequence", "sqlserver.session_id"},
			Fields:    []SessionField{{Name: "collect_batch_text", Value: "1"}},
		}},
		Targets: []SessionTarget{
			{Package: "package0", Name: XETargetRingBuffer, Fields: []SessionField{{Name: "max_events_limit", Value: "1000"}}},
			{Package: "package0", Name: XETargetEventFile, Fields: []SessionField{
				{Name: "filename", Value: name + ".xel", IsString: true},
				{Name: "max_file_size", Value: "5"},
				{Name: "max_rollover_files", Value: "2"},
			}},
		},
		RetentionMode:      XERetentionAllowSingleEventLoss,
		MaxDispatchLatency: time.Second,
		MaxMemory:          4096,
	}
	// A Managed Instance refuses a file target without a blob URL (Msg
	// 40538); Phase G of gossms's plan wires that. The ring_buffer half of
	// the test still runs there.
	azure := s.Info().IsAzure()
	if azure {
		spec.Targets = spec.Targets[:1]
	}
	es, err := s.CreateEventSession(ctx, spec)
	if err != nil {
		t.Fatalf("CreateEventSession: %v", err)
	}
	if es.IsRunning || es.MaxDispatchLatency != time.Second || es.MaxMemory != 4096 ||
		es.RetentionMode != XERetentionAllowSingleEventLoss || es.MemoryPartitionMode != XEPartitionNone {
		t.Errorf("read back %+v", es)
	}
	if got, want := specString(normalizedSpec(es)), specString(normalizedSpec(&EventSession{
		Name: name, Events: spec.Events, Targets: spec.Targets, RetentionMode: spec.RetentionMode,
		MaxDispatchLatency: time.Second, MaxMemory: 4096, MemoryPartitionMode: XEPartitionNone,
	})); got != want {
		t.Errorf("definition read back as\n%s\nwant\n%s", got, want)
	}
	if _, err := es.Status(ctx); !errors.Is(err, ErrNotFound) {
		t.Errorf("Status of a stopped session: %v", err)
	}

	if err := es.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if !es.IsRunning {
		t.Error("Start did not mirror IsRunning")
	}

	marker := fmt.Sprintf("SELECT 'gossms_xe_marker_%d'", time.Now().UnixNano())
	if _, err := conn.ExecContext(ctx, marker); err != nil {
		t.Fatal(err)
	}
	hasMarker := func(evs []XEvent) bool {
		for _, ev := range evs {
			if v, ok := ev.Field("batch_text"); ok && v.Value == marker {
				return true
			}
		}
		return false
	}

	// Ring buffer: dispatch latency is a second, so poll for a few.
	var rb *RingBufferData
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(500 * time.Millisecond) {
		if rb, err = es.ReadRingBuffer(ctx); err != nil {
			t.Fatalf("ReadRingBuffer: %v", err)
		}
		if hasMarker(rb.Events) {
			break
		}
	}
	if !hasMarker(rb.Events) {
		t.Fatalf("marker batch never reached the ring buffer: %+v", rb)
	}
	for _, ev := range rb.Events {
		if ev.Seq == 0 || ev.Timestamp.IsZero() || ev.Timestamp.Location() != time.UTC {
			t.Errorf("event %s: Seq %d, Timestamp %v", ev.Name, ev.Seq, ev.Timestamp)
		}
	}

	st, err := es.Status(ctx)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	var sawFile, sawRing bool
	for _, tg := range st.Targets {
		switch tg.Name {
		case XETargetEventFile:
			sawFile = azure || strings.Contains(tg.CurrentFile, name) && strings.HasSuffix(strings.ToLower(tg.CurrentFile), ".xel")
		case XETargetRingBuffer:
			sawRing = tg.TotalEventsProcessed > 0 && tg.Package == "package0"
		}
	}
	if azure {
		sawFile = true
	}
	if !sawFile || !sawRing {
		t.Errorf("Status targets = %+v", st.Targets)
	}

	if !azure {
		checkLiveEventFile(t, s, ctx, es, hasMarker)
	}

	// Alter while running: an event added in place, an option through the
	// stop window. The session must come out running.
	want := es.Spec()
	want.Events = append(want.Events, SessionEvent{Package: "sqlserver", Name: "error_reported",
		Predicate: fmt.Sprintf("([sqlserver].[session_id]=(%d))", spid)})
	want.MaxDispatchLatency = 2 * time.Second
	if err := s.EventSessionRef(name).Alter(ctx, want); err != nil {
		t.Fatalf("Alter: %v", err)
	}
	got, err := s.EventSessionByName(ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	if !got.IsRunning || got.MaxDispatchLatency != 2*time.Second || len(got.Events) != 2 {
		t.Errorf("after Alter: running %v, latency %v, %d events", got.IsRunning, got.MaxDispatchLatency, len(got.Events))
	}

	// The stop window is needed: the same option change by hand, on the
	// running session, is refused.
	if _, err := db.ExecContext(ctx, "ALTER EVENT SESSION "+quoteIdent(name)+" ON SERVER WITH (MAX_DISPATCH_LATENCY = 3 SECONDS)"); err == nil ||
		!strings.Contains(err.Error(), "running") {
		t.Errorf("changing an option on a running session was not refused: %v", err)
	}

	if err := got.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if _, err := got.ReadRingBuffer(ctx); !errors.Is(err, ErrNotFound) {
		t.Errorf("ReadRingBuffer of a stopped session: %v", err)
	}
	if err := got.Drop(ctx); err != nil {
		t.Fatalf("Drop: %v", err)
	}
	if _, err := s.EventSessionByName(ctx, name); !errors.Is(err, ErrNotFound) {
		t.Errorf("session still there after Drop: %v", err)
	}
}

// checkLiveEventFile: a read from the start sees the marker; a read from the
// returned cursor sees only what came after it.
func checkLiveEventFile(t *testing.T, s *Server, ctx context.Context, es *EventSession, hasMarker func([]XEvent) bool) {
	t.Helper()
	pattern, err := es.EventFilePattern()
	if err != nil {
		t.Fatal(err)
	}
	var evs []XEvent
	var cur EventFileCursor
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(500 * time.Millisecond) {
		if evs, cur, err = s.ReadEventFile(ctx, pattern, EventFileCursor{}, 0); err != nil {
			t.Fatalf("ReadEventFile: %v", err)
		}
		if hasMarker(evs) {
			break
		}
	}
	if !hasMarker(evs) || cur.IsZero() {
		t.Fatalf("marker never reached the event file (%d events, cursor %+v)", len(evs), cur)
	}
	again, cur2, err := s.ReadEventFile(ctx, pattern, cur, 0)
	if err != nil {
		t.Fatalf("ReadEventFile from cursor: %v", err)
	}
	if hasMarker(again) {
		t.Errorf("a read from the cursor returned the marker again (%d events)", len(again))
	}
	if len(again) == 0 && cur2 != cur {
		t.Errorf("an empty read moved the cursor from %+v to %+v", cur, cur2)
	}
}

// TestLiveEventSessionScriptRoundTrip: script → drop → run the script →
// script again, and the two scripts are equal. Then the same against a copy of
// system_health, the richest definition on every instance (predicates with
// string literals, SET fields on events and targets), created under another
// name and never started.
func TestLiveEventSessionScriptRoundTrip(t *testing.T) {
	s, db, ctx := liveXEServer(t)
	sc := NewServerScripter(s, ScriptOptions{})

	health, err := s.EventSessionByName(ctx, "system_health")
	if err != nil {
		t.Fatalf("system_health: %v", err)
	}
	const name = "gossms_test_xe_health_copy"
	dropXESessionAfter(t, s, db, name)
	spec := health.Spec()
	spec.Name = name
	spec.StartupState = false
	// The copy writes its own file, so a later read of system_health's never
	// picks it up.
	for i, tg := range spec.Targets {
		if tg.Name == XETargetEventFile {
			spec.Targets[i].Fields = slices.Clone(tg.Fields)
			for j, f := range spec.Targets[i].Fields {
				if f.Name == "filename" {
					spec.Targets[i].Fields[j].Value = name + ".xel"
				}
			}
		}
	}
	if _, err := s.CreateEventSession(ctx, spec); err != nil {
		t.Fatalf("create system_health copy: %v", err)
	}

	first, err := sc.ScriptEventSession(ctx, name)
	if err != nil {
		t.Fatalf("ScriptEventSession: %v", err)
	}
	if err := s.EventSessionRef(name).Drop(ctx); err != nil {
		t.Fatal(err)
	}
	for _, batch := range strings.Split(first, "\nGO\n") {
		if strings.TrimSpace(batch) == "" {
			continue
		}
		if _, err := db.ExecContext(ctx, batch); err != nil {
			t.Fatalf("running the script: %v\n%s", err, batch)
		}
	}
	second, err := sc.ScriptEventSession(ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Errorf("script did not round-trip:\n%s\n---\n%s", first, second)
	}
	copied, err := s.EventSessionByName(ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	h, c := normalizedSpec(health), normalizedSpec(copied)
	h.StartupState, h.Targets, c.Targets = false, nil, nil // renamed file, startup off
	if specString(h) != specString(c) {
		t.Errorf("copy differs from system_health:\n%s\n---\n%s", specString(h), specString(c))
	}

	// The drop-and-create form runs twice in a row.
	both, err := NewServerScripter(s, ScriptOptions{Verb: ScriptDropAndCreate, IncludeIfNotExists: true}).ScriptEventSession(ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		for _, batch := range strings.Split(both, "\nGO\n") {
			if strings.TrimSpace(batch) == "" {
				continue
			}
			if _, err := db.ExecContext(ctx, batch); err != nil {
				t.Fatalf("running the drop-and-create script: %v\n%s", err, batch)
			}
		}
	}
}

func TestLiveXECatalog(t *testing.T) {
	s, _, ctx := liveXEServer(t)

	pkgs, err := s.XEPackages(ctx)
	if err != nil || !slices.ContainsFunc(pkgs, func(p XEPackage) bool { return p.Name == "package0" }) {
		t.Errorf("XEPackages: %d, %v", len(pkgs), err)
	}
	if slices.ContainsFunc(pkgs, func(p XEPackage) bool { return p.Name == "SecAudit" }) {
		t.Error("a private package was listed")
	}

	events, err := s.XEObjects(ctx, XEObjectEvent)
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(events, func(o XEObject) bool { return o.QualifiedName() == "sqlserver.sql_batch_completed" })
	if i < 0 || events[i].Channel == "" || events[i].Keyword == "" || events[i].Description == "" {
		t.Errorf("sql_batch_completed = %+v (of %d events)", events, len(events))
	}

	cols, err := s.XEObjectColumns(ctx, "package0", "event_file")
	if err != nil {
		t.Fatal(err)
	}
	fi := slices.IndexFunc(cols, func(c XEObjectColumn) bool { return c.Name == "filename" })
	ri := slices.IndexFunc(cols, func(c XEObjectColumn) bool { return c.Name == "max_rollover_files" })
	if fi < 0 || !cols[fi].Mandatory || ri < 0 || cols[ri].Value == "" || cols[ri].ColumnType != XEColumnCustomizable {
		t.Errorf("event_file columns = %+v", cols)
	}

	waits, err := s.XEMapValues(ctx, "sqlos", "wait_types")
	if err != nil || !slices.ContainsFunc(waits, func(v XEMapValue) bool { return v.Value == "WAITFOR" }) {
		t.Errorf("wait_types: %d values, %v", len(waits), err)
	}

	// Every session on the instance reads, with its children.
	all, err := s.EventSessions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	hi := slices.IndexFunc(all, func(es *EventSession) bool { return es.Name == "system_health" })
	if hi < 0 || len(all[hi].Events) == 0 || len(all[hi].Targets) == 0 || !all[hi].StartupState {
		t.Errorf("system_health in the list = %+v", all)
	}
}

// TestLiveXEProfilerTemplates creates each XEvent Profiler template under a
// throwaway name with a target added, checks the catalog reads it back as
// written, starts it and sees a batch arrive — the templates' events, actions
// and predicate are ones this server accepts. Then AddTarget on the running
// session, EventFiles listing its file, and ReadEventFile refusing a cursor
// into a file that is not there as ErrEventFileGone.
func TestLiveXEProfilerTemplates(t *testing.T) {
	s, db, ctx := liveXEServer(t)
	azure := s.Info().IsAzure()
	for _, tc := range []struct {
		name string
		spec EventSessionSpec
	}{
		{"gossms_test_xe_prof_std", XEProfilerStandard()},
		{"gossms_test_xe_prof_tsql", XEProfilerTSQL()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dropXESessionAfter(t, s, db, tc.name)
			spec := tc.spec
			spec.Name = tc.name
			spec.MaxDispatchLatency = time.Second
			spec.Targets = []SessionTarget{EventFileTarget(tc.name, 5, 2)}
			if azure {
				spec.Targets = []SessionTarget{RingBufferTarget(0)}
			}
			es, err := s.CreateEventSession(ctx, spec)
			if err != nil {
				t.Fatalf("CreateEventSession: %v", err)
			}
			want := &EventSession{Name: spec.Name, Events: spec.Events, Targets: spec.Targets,
				RetentionMode: spec.RetentionMode, MaxDispatchLatency: time.Second, MaxMemory: spec.MaxMemory,
				MemoryPartitionMode: spec.MemoryPartitionMode, TrackCausality: true}
			if got, w := specString(normalizedSpec(es)), specString(normalizedSpec(want)); got != w {
				t.Errorf("definition read back as\n%s\nwant\n%s", got, w)
			}
			if err := es.Start(ctx); err != nil {
				t.Fatalf("Start: %v", err)
			}

			marker := fmt.Sprintf("SELECT 'gossms_xe_prof_%d'", time.Now().UnixNano())
			if _, err := db.ExecContext(ctx, marker); err != nil {
				t.Fatal(err)
			}
			hasMarker := func(evs []XEvent) bool {
				for _, ev := range evs {
					if v, ok := ev.Field("batch_text"); ok && v.Value == marker && ev.Name == "sql_batch_starting" {
						return true
					}
				}
				return false
			}
			if azure {
				var rb *RingBufferData
				for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); time.Sleep(time.Second) {
					if rb, err = es.ReadRingBuffer(ctx); err != nil {
						t.Fatal(err)
					}
					if hasMarker(rb.Events) {
						break
					}
				}
				if !hasMarker(rb.Events) {
					t.Fatalf("the marker batch never reached the ring buffer (%d events)", len(rb.Events))
				}
				return
			}
			checkLiveEventFile(t, s, ctx, es, hasMarker)

			pattern, _ := es.EventFilePattern()
			files, err := s.EventFiles(ctx, pattern)
			if err != nil {
				t.Fatalf("EventFiles: %v", err)
			}
			st, err := es.Status(ctx)
			if err != nil {
				t.Fatal(err)
			}
			var current string
			for _, tg := range st.Targets {
				if tg.Name == XETargetEventFile {
					current = tg.CurrentFile
				}
			}
			if len(files) == 0 || !strings.EqualFold(files[len(files)-1], current) {
				t.Errorf("EventFiles = %q, want the current file %q last", files, current)
			}

			gone := EventFileCursor{File: current[:len(current)-4] + "9.xel", Offset: 84480}
			if _, _, err := s.ReadEventFile(ctx, pattern, gone, 0); !errors.Is(err, ErrEventFileGone) {
				t.Errorf("a cursor into a missing file: %v, want ErrEventFileGone", err)
			}

			if err := es.AddTarget(ctx, RingBufferTarget(1024)); err != nil {
				t.Fatalf("AddTarget on a running session: %v", err)
			}
			got, err := s.EventSessionByName(ctx, tc.name)
			if err != nil {
				t.Fatal(err)
			}
			if _, ok := got.Target(XETargetRingBuffer); !ok || !got.IsRunning || len(got.Targets) != 2 {
				t.Errorf("after AddTarget: running %v, targets %+v", got.IsRunning, got.Targets)
			}
		})
	}
}

// Every New Session template creates on the instance and reads back as it was
// written — events, actions, fields, predicates in the catalog's own form, and
// targets — so a Properties dialog opened on a session made from one diffs to
// nothing. A template whose event the instance lacks is reported, not failed:
// the dialog leaves such events out.
func TestLiveXESessionTemplates(t *testing.T) {
	s, db, ctx := liveXEServer(t)
	events, err := s.XEObjects(ctx, XEObjectEvent)
	if err != nil {
		t.Fatal(err)
	}
	for i, tpl := range XESessionTemplates() {
		name := fmt.Sprintf("gossms_test_xe_tpl_%d", i)
		t.Run(tpl.Name, func(t *testing.T) {
			spec := tpl.Spec()
			for _, e := range spec.Events {
				if !slices.ContainsFunc(events, func(o XEObject) bool { return o.QualifiedName() == e.QualifiedName() }) {
					t.Skipf("%s is not on this instance", e.QualifiedName())
				}
			}
			dropXESessionAfter(t, s, db, name)
			spec.Name = name
			es, err := s.CreateEventSession(ctx, spec)
			if err != nil {
				t.Fatalf("CreateEventSession: %v", err)
			}
			want := &EventSession{Name: name, Events: spec.Events, Targets: spec.Targets,
				RetentionMode: spec.RetentionMode, MaxDispatchLatency: spec.MaxDispatchLatency, MaxMemory: spec.MaxMemory,
				MemoryPartitionMode: spec.MemoryPartitionMode, TrackCausality: spec.TrackCausality}
			if got, w := specString(normalizedSpec(es)), specString(normalizedSpec(want)); got != w {
				t.Errorf("definition read back as\n%s\nwant\n%s", got, w)
			}
			if st := spec.alterStatements(es); len(st.statements) > 0 || st.stoppedOnly != "" {
				t.Errorf("the template diffs against its own session: %+v", st)
			}
		})
	}
}
