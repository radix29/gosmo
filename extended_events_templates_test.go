package gosmo

import (
	"context"
	"strings"
	"testing"
)

// The XEvent Profiler templates are pinned as the CREATE they script to, so a
// change to an event, an action or the predicate shows as a diff of the
// statement SSMS would run — which is what the templates copy.

func TestXEProfilerStandardStatement(t *testing.T) {
	got, err := XEProfilerStandard().createStatement(xeServerScope)
	if err != nil {
		t.Fatal(err)
	}
	const conn = "ACTION(package0.event_sequence,sqlserver.client_app_name,sqlserver.client_pid,sqlserver.nt_username,sqlserver.server_principal_name,sqlserver.session_id)"
	const stmt = "ACTION(package0.event_sequence,sqlserver.client_app_name,sqlserver.client_pid,sqlserver.database_id,sqlserver.database_name,sqlserver.nt_username,sqlserver.query_hash,sqlserver.server_principal_name,sqlserver.session_id)"
	const where = "WHERE ([package0].[equal_boolean]([sqlserver].[is_system],(0)))"
	want := "CREATE EVENT SESSION [QuickSessionStandard] ON SERVER" +
		"\nADD EVENT sqlserver.attention(" + stmt + "\n    " + where + ")," +
		"\nADD EVENT sqlserver.existing_connection(SET collect_options_text=(1)\n    " + conn + ")," +
		"\nADD EVENT sqlserver.login(SET collect_options_text=(1)\n    " + conn + ")," +
		"\nADD EVENT sqlserver.logout(" + conn + ")," +
		"\nADD EVENT sqlserver.rpc_completed(" + stmt + "\n    " + where + ")," +
		"\nADD EVENT sqlserver.sql_batch_completed(" + stmt + "\n    " + where + ")," +
		"\nADD EVENT sqlserver.sql_batch_starting(" + stmt + "\n    " + where + ")" +
		"\nWITH (MAX_MEMORY=8192 KB,EVENT_RETENTION_MODE=ALLOW_SINGLE_EVENT_LOSS,MAX_DISPATCH_LATENCY=5 SECONDS,MAX_EVENT_SIZE=0 KB,MEMORY_PARTITION_MODE=PER_CPU,TRACK_CAUSALITY=ON,STARTUP_STATE=OFF)"
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestXEProfilerTSQLStatement(t *testing.T) {
	got, err := XEProfilerTSQL().createStatement(xeServerScope)
	if err != nil {
		t.Fatal(err)
	}
	const acts = "ACTION(package0.event_sequence,sqlserver.client_app_name,sqlserver.client_pid,sqlserver.database_id,sqlserver.database_name,sqlserver.nt_username,sqlserver.session_id)"
	const where = "WHERE ([package0].[equal_boolean]([sqlserver].[is_system],(0)))"
	want := "CREATE EVENT SESSION [QuickSessionTSQL] ON SERVER" +
		"\nADD EVENT sqlserver.existing_connection(" + acts + ")," +
		"\nADD EVENT sqlserver.login(SET collect_options_text=(1)\n    " + acts + ")," +
		"\nADD EVENT sqlserver.logout(" + acts + ")," +
		"\nADD EVENT sqlserver.rpc_starting(" + acts + "\n    " + where + ")," +
		"\nADD EVENT sqlserver.sql_batch_starting(" + acts + "\n    " + where + ")" +
		"\nWITH (MAX_MEMORY=8192 KB,EVENT_RETENTION_MODE=ALLOW_SINGLE_EVENT_LOSS,MAX_DISPATCH_LATENCY=5 SECONDS,MAX_EVENT_SIZE=0 KB,MEMORY_PARTITION_MODE=PER_CPU,TRACK_CAUSALITY=ON,STARTUP_STATE=OFF)"
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

// Each call is a fresh spec, and each event its own action slice: a caller
// adding an action to one event, or to one copy, reaches nothing else.
func TestXEProfilerSpecsShareNothing(t *testing.T) {
	a, b := XEProfilerStandard(), XEProfilerStandard()
	a.Events[0].Actions[0] = "mutated"
	a.Events[4].Actions = append(a.Events[4].Actions, "sqlserver.sql_text")
	if b.Events[0].Actions[0] != "package0.event_sequence" {
		t.Error("a second call saw the first one's edit")
	}
	if a.Events[5].Actions[0] != "package0.event_sequence" || len(a.Events[5].Actions) != 9 {
		t.Errorf("an edit to one event reached another: %v", a.Events[5].Actions)
	}
}

func TestTargetBuilders(t *testing.T) {
	for _, tc := range []struct {
		t    SessionTarget
		want string
	}{
		{EventFileTarget("it's", 20, 4), "package0.event_file(SET filename=N'it''s',max_file_size=(20),max_rollover_files=(4))"},
		{EventFileTarget(`C:\xe\x.xel`, 0, 0), `package0.event_file(SET filename=N'C:\xe\x.xel')`},
		{RingBufferTarget(0), "package0.ring_buffer"},
		{RingBufferTarget(8192), "package0.ring_buffer(SET max_memory=(8192))"},
	} {
		if got := tc.t.clause(); got != tc.want {
			t.Errorf("got %s, want %s", got, tc.want)
		}
	}
}

func TestAddTargetStatement(t *testing.T) {
	ctx, col := WithScript(context.Background())
	es := (&Server{}).EventSessionRef("s]1")
	if err := es.AddTarget(ctx, EventFileTarget("s1", 0, 0)); err != nil {
		t.Fatal(err)
	}
	if err := (&Server{}).DatabaseRef("AppDB").EventSessionRef("d").AddTarget(ctx, RingBufferTarget(0)); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"ALTER EVENT SESSION [s]]1] ON SERVER\nADD TARGET package0.event_file(SET filename=N's1')",
		useAppDB + "ALTER EVENT SESSION [d] ON DATABASE\nADD TARGET package0.ring_buffer",
	}
	if got := col.Statements(); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("got %q\nwant %q", got, want)
	}
	if len(es.Targets) != 0 {
		t.Error("Targets mirrored while scripting")
	}
	if err := es.AddTarget(ctx, SessionTarget{Name: "ring_buffer"}); err == nil {
		t.Error("a target with no package was sent")
	}
}

// Every New Session template is nameless, targetless unless it needs one to
// mean anything, valid as a spec once named, and fresh per call.
func TestXESessionTemplates(t *testing.T) {
	seen := map[string]bool{}
	for _, tpl := range XESessionTemplates() {
		if seen[tpl.Name] || tpl.Category == "" || tpl.Description == "" {
			t.Errorf("template %+v: duplicate or unlabelled", tpl)
		}
		seen[tpl.Name] = true
		a := tpl.Spec()
		if a.Name != "" {
			t.Errorf("%s: spec named %q", tpl.Name, a.Name)
		}
		a.Name = "x"
		if err := a.validate(); err != nil {
			t.Errorf("%s: %v", tpl.Name, err)
		}
		a.Events[0].Actions = append(a.Events[0].Actions[:0], "mutated")
		if b := tpl.Spec(); len(b.Events[0].Actions) > 0 && b.Events[0].Actions[0] == "mutated" {
			t.Errorf("%s: two calls share an action slice", tpl.Name)
		}
	}
}

// A conjunction is written as the catalog stores one: one pair of
// parentheses round the whole, a term's own outer pair dropped, and a bare
// function call left intact.
func TestXEAnd(t *testing.T) {
	got := xeAnd(xeNotSystem, xeUserDatabase)
	want := "([package0].[equal_boolean]([sqlserver].[is_system],(0)) AND [package0].[greater_than_uint64]([sqlserver].[database_id],(4)))"
	if got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}
