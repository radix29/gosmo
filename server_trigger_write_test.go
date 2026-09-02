package gosmo

import (
	"context"
	"strings"
	"testing"
)

func TestServerTriggerWriteStatements(t *testing.T) {
	cases := []struct {
		name string
		act  func(*ServerTrigger, context.Context) error
		want string
	}{
		{"enable", func(tr *ServerTrigger, ctx context.Context) error { return tr.EnableContext(ctx) },
			"ENABLE TRIGGER [ddl_audit] ON ALL SERVER"},
		{"disable", func(tr *ServerTrigger, ctx context.Context) error { return tr.DisableContext(ctx) },
			"DISABLE TRIGGER [ddl_audit] ON ALL SERVER"},
		{"drop", func(tr *ServerTrigger, ctx context.Context) error { return tr.DropContext(ctx) },
			"DROP TRIGGER [ddl_audit] ON ALL SERVER"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, col := WithScript(context.Background())
			tr := (&Server{}).ServerTrigger("ddl_audit")
			if err := tc.act(tr, ctx); err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			if len(col.Statements) != 1 {
				t.Fatalf("got %d statements, want 1: %v", len(col.Statements), col.Statements)
			}
			if col.Statements[0] != tc.want {
				t.Errorf("got:\n%s\nwant:\n%s", col.Statements[0], tc.want)
			}
		})
	}
}

// A trigger name is an identifier, so a bracket in it must be doubled or the
// statement addresses a different (or no) trigger.
func TestServerTriggerNameIsQuoted(t *testing.T) {
	ctx, col := WithScript(context.Background())
	if err := (&Server{}).ServerTrigger("odd]name").DropContext(ctx); err != nil {
		t.Fatalf("DropContext: %v", err)
	}
	if want := "DROP TRIGGER [odd]]name] ON ALL SERVER"; col.Statements[0] != want {
		t.Errorf("got %q, want %q", col.Statements[0], want)
	}
}

// Under WithScript nothing ran, so the receiver must not be updated to claim
// a state the server was never told about.
func TestServerTriggerEnabledFlagIsNotSetWhileScripting(t *testing.T) {
	tr := (&Server{}).ServerTrigger("ddl_audit")
	ctx, _ := WithScript(context.Background())
	if err := tr.EnableContext(ctx); err != nil {
		t.Fatalf("EnableContext: %v", err)
	}
	if tr.IsEnabled {
		t.Error("IsEnabled was set from a scripted (not executed) ENABLE")
	}
}

func TestBuildServerTriggerScript(t *testing.T) {
	const def = "CREATE TRIGGER [ddl_audit] ON ALL SERVER\nFOR CREATE_DATABASE\nAS PRINT 'x';"

	t.Run("create", func(t *testing.T) {
		got, err := buildServerTriggerScript(&ServerTrigger{Name: "ddl_audit", IsEnabled: true, Definition: def}, ScriptOptions{})
		if err != nil {
			t.Fatalf("buildServerTriggerScript: %v", err)
		}
		if !strings.Contains(got, def) {
			t.Errorf("definition missing from script:\n%s", got)
		}
		if strings.Contains(got, "DISABLE TRIGGER") {
			t.Errorf("an enabled trigger scripted a DISABLE:\n%s", got)
		}
	})

	t.Run("a disabled trigger scripts its DISABLE", func(t *testing.T) {
		got, err := buildServerTriggerScript(&ServerTrigger{Name: "ddl_audit", Definition: def}, ScriptOptions{})
		if err != nil {
			t.Fatalf("buildServerTriggerScript: %v", err)
		}
		if !strings.Contains(got, "DISABLE TRIGGER [ddl_audit] ON ALL SERVER;") {
			t.Errorf("disabled state not scripted:\n%s", got)
		}
	})

	t.Run("drop and create", func(t *testing.T) {
		got, err := buildServerTriggerScript(&ServerTrigger{Name: "ddl_audit", IsEnabled: true, Definition: def},
			ScriptOptions{Verb: ScriptDropAndCreate})
		if err != nil {
			t.Fatalf("buildServerTriggerScript: %v", err)
		}
		if !strings.Contains(got, "DROP TRIGGER IF EXISTS [ddl_audit] ON ALL SERVER;") {
			t.Errorf("drop half missing:\n%s", got)
		}
		if !strings.Contains(got, def) {
			t.Errorf("create half missing:\n%s", got)
		}
	})

	t.Run("drop only reads no definition", func(t *testing.T) {
		got, err := buildServerTriggerScript(&ServerTrigger{Name: "ddl_audit"}, ScriptOptions{Verb: ScriptDrop})
		if err != nil {
			t.Fatalf("buildServerTriggerScript: %v", err)
		}
		if strings.Contains(got, "CREATE") || strings.Contains(got, "DISABLE") {
			t.Errorf("a drop-only script carried more than the drop:\n%s", got)
		}
	})

	t.Run("alter rewrites the create", func(t *testing.T) {
		got, err := buildServerTriggerScript(&ServerTrigger{Name: "ddl_audit", IsEnabled: true, Definition: def},
			ScriptOptions{Verb: ScriptAlter})
		if err != nil {
			t.Fatalf("buildServerTriggerScript: %v", err)
		}
		if !strings.HasPrefix(got, "ALTER TRIGGER [ddl_audit] ON ALL SERVER") {
			t.Errorf("CREATE was not rewritten to ALTER:\n%s", got)
		}
	})

	// An unreadable definition must fail rather than emit a script whose drop
	// half runs and whose create half is empty.
	t.Run("an unreadable definition is an error", func(t *testing.T) {
		if _, err := buildServerTriggerScript(&ServerTrigger{Name: "clr_trig"}, ScriptOptions{Verb: ScriptDropAndCreate}); err == nil {
			t.Error("want an error for a trigger with no definition, got nil")
		}
	})
}
