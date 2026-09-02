//go:build livedb

// Live verification of the server-scope DDL trigger family: that
// sys.server_triggers reads back the columns server_trigger.go scans, that
// ENABLE/DISABLE/DROP ... ON ALL SERVER as gosmo builds them are accepted, and
// that a generated script recreates the trigger.
//
// The unit tests pin the statement text; only a live run settles the catalog
// shape — the LEFT JOIN onto sys.server_sql_modules and the STRING_AGG over
// sys.server_trigger_events in particular.
//
//	go test -tags livedb . -run TestLiveServerTrigger -v \
//	  -livedb 'sqlserver://sa:PASS@host?TrustServerCertificate=true'
//
// Creates and drops its own throwaway trigger; touches nothing else.
package gosmo

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

const liveServerTriggerName = "gossms_plan_ddl_trig"

func TestLiveServerTriggerReadEnableDisableDrop(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()
	s, err := NewServer(ctx, db)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	drop := func() {
		if _, err := db.ExecContext(ctx, "DROP TRIGGER IF EXISTS ["+liveServerTriggerName+"] ON ALL SERVER"); err != nil {
			t.Logf("cleanup of server trigger: %v", err)
		}
	}
	drop()
	defer drop()

	const body = `CREATE TRIGGER [` + liveServerTriggerName + `] ON ALL SERVER
FOR CREATE_DATABASE, ALTER_DATABASE
AS
    PRINT 'gossms plan trigger';`
	if _, err := db.ExecContext(ctx, body); err != nil {
		t.Fatalf("create trigger: %v", err)
	}

	tr, err := s.ServerTriggerByNameContext(ctx, liveServerTriggerName)
	if err != nil {
		t.Fatalf("ServerTriggerByNameContext: %v", err)
	}
	if !tr.IsEnabled {
		t.Error("a freshly created trigger read back as disabled")
	}
	if tr.CreateDate.IsZero() || tr.ModifyDate.IsZero() {
		t.Errorf("dates not populated: create=%v modify=%v", tr.CreateDate, tr.ModifyDate)
	}
	for _, want := range []string{"CREATE_DATABASE", "ALTER_DATABASE"} {
		if !slices.Contains(tr.Events, want) {
			t.Errorf("event %q missing from %v", want, tr.Events)
		}
	}
	if !strings.Contains(tr.Definition, "gossms plan trigger") {
		t.Errorf("definition not read back: %q", tr.Definition)
	}

	list, err := s.ServerTriggersContext(ctx)
	if err != nil {
		t.Fatalf("ServerTriggersContext: %v", err)
	}
	idx := slices.IndexFunc(list, func(x *ServerTrigger) bool { return x.Name == liveServerTriggerName })
	if idx < 0 {
		t.Fatalf("trigger missing from the list of %d", len(list))
	}
	if got := list[idx]; got.IsEnabled != tr.IsEnabled || len(got.Events) != len(tr.Events) {
		t.Errorf("list row disagrees with the by-name read: %+v vs %+v", got, tr)
	}

	if err := tr.DisableContext(ctx); err != nil {
		t.Fatalf("DisableContext: %v", err)
	}
	after, err := s.ServerTriggerByNameContext(ctx, liveServerTriggerName)
	if err != nil {
		t.Fatalf("re-read after disable: %v", err)
	}
	if after.IsEnabled {
		t.Error("trigger still reads as enabled after DisableContext")
	}

	// A disabled trigger's script must carry the DISABLE, or running it puts
	// the trigger back in a state the source server was not in.
	script, err := NewServerScripter(s, ScriptOptions{Verb: ScriptDropAndCreate}).ScriptServerTriggerContext(ctx, liveServerTriggerName)
	if err != nil {
		t.Fatalf("ScriptServerTriggerContext: %v", err)
	}
	if !strings.Contains(script, "DROP TRIGGER IF EXISTS") || !strings.Contains(script, "gossms plan trigger") ||
		!strings.Contains(script, "DISABLE TRIGGER") {
		t.Errorf("script is missing a half:\n%s", script)
	}

	if err := after.EnableContext(ctx); err != nil {
		t.Fatalf("EnableContext: %v", err)
	}
	if back, err := s.ServerTriggerByNameContext(ctx, liveServerTriggerName); err != nil || !back.IsEnabled {
		t.Errorf("trigger did not come back enabled: %v %+v", err, back)
	}

	if err := s.ServerTrigger(liveServerTriggerName).DropContext(ctx); err != nil {
		t.Fatalf("DropContext: %v", err)
	}
	if _, err := s.ServerTriggerByNameContext(ctx, liveServerTriggerName); !errors.Is(err, ErrNotFound) {
		t.Errorf("after the drop, want ErrNotFound, got %v", err)
	}
}
