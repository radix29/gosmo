package gosmo

import (
	"context"
	"strings"
	"testing"
)

// dbTriggerFor builds a handle over a database named the same as the audit
// tests' one, so the `use` prefix const applies here unchanged.
func dbTriggerFor(name string) *DatabaseTrigger {
	return (&Database{name: "app]db"}).DatabaseTrigger(name)
}

func TestDatabaseTriggerWriteStatements(t *testing.T) {
	cases := []struct {
		name string
		act  func(*DatabaseTrigger, context.Context) error
		want string
	}{
		{"enable", func(tr *DatabaseTrigger, ctx context.Context) error { return tr.EnableContext(ctx) },
			use + "ENABLE TRIGGER [ddl_audit] ON DATABASE"},
		{"disable", func(tr *DatabaseTrigger, ctx context.Context) error { return tr.DisableContext(ctx) },
			use + "DISABLE TRIGGER [ddl_audit] ON DATABASE"},
		{"drop", func(tr *DatabaseTrigger, ctx context.Context) error { return tr.DropContext(ctx) },
			use + "DROP TRIGGER [ddl_audit] ON DATABASE"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, col := WithScript(context.Background())
			if err := tc.act(dbTriggerFor("ddl_audit"), ctx); err != nil {
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

// A DDL trigger has no schema, so its drop must not be schema-qualified —
// Database.DropTrigger's form, which would address a different (nonexistent)
// object and omit the ON DATABASE clause the server requires.
func TestDatabaseTriggerDropIsNotSchemaQualified(t *testing.T) {
	ctx, col := WithScript(context.Background())
	if err := dbTriggerFor("ddl_audit").DropContext(ctx); err != nil {
		t.Fatalf("DropContext: %v", err)
	}
	if strings.Contains(col.Statements[0], "[dbo].") {
		t.Errorf("the drop schema-qualified a database-scope trigger:\n%s", col.Statements[0])
	}
	if !strings.HasSuffix(col.Statements[0], " ON DATABASE") {
		t.Errorf("the drop is missing its ON DATABASE clause:\n%s", col.Statements[0])
	}
}

// A trigger name is an identifier, so a bracket in it must be doubled or the
// statement addresses a different (or no) trigger.
func TestDatabaseTriggerNameIsQuoted(t *testing.T) {
	ctx, col := WithScript(context.Background())
	if err := dbTriggerFor("odd]name").DropContext(ctx); err != nil {
		t.Fatalf("DropContext: %v", err)
	}
	if want := use + "DROP TRIGGER [odd]]name] ON DATABASE"; col.Statements[0] != want {
		t.Errorf("got %q, want %q", col.Statements[0], want)
	}
}

// Under WithScript nothing ran, so the receiver must not be updated to claim
// a state the server was never told about.
func TestDatabaseTriggerEnabledFlagIsNotSetWhileScripting(t *testing.T) {
	tr := dbTriggerFor("ddl_audit")
	ctx, _ := WithScript(context.Background())
	if err := tr.EnableContext(ctx); err != nil {
		t.Fatalf("EnableContext: %v", err)
	}
	if tr.IsEnabled {
		t.Error("IsEnabled was set from a scripted (not executed) ENABLE")
	}
}

// databaseTriggerSelect must read the DDL family and only it: parent_class = 0
// is what separates it from Database.triggersWhere's DML rows, and there is no
// sys.objects join because parent_id is 0 for every row here.
func TestDatabaseTriggerSelectReadsTheDDLFamily(t *testing.T) {
	if !strings.Contains(databaseTriggerSelect, "tr.parent_class = 0") {
		t.Error("databaseTriggerSelect does not restrict to parent_class = 0")
	}
	if !strings.Contains(databaseTriggerSelect, "LEFT   JOIN sys.sql_modules") {
		t.Error("sys.sql_modules is not LEFT JOINed — a CLR trigger would vanish from the folder")
	}
	if strings.Contains(databaseTriggerSelect, "sys.objects") {
		t.Error("databaseTriggerSelect joins sys.objects, which has no row for a parent_id of 0")
	}
}

func TestBuildDatabaseTriggerScript(t *testing.T) {
	const def = "CREATE TRIGGER [ddl_audit] ON DATABASE\nFOR CREATE_TABLE\nAS PRINT 'x';"

	t.Run("create", func(t *testing.T) {
		got, err := buildDatabaseTriggerScript(&DatabaseTrigger{Name: "ddl_audit", IsEnabled: true, Definition: def}, ScriptOptions{})
		if err != nil {
			t.Fatalf("buildDatabaseTriggerScript: %v", err)
		}
		if !strings.Contains(got, def) {
			t.Errorf("definition missing from script:\n%s", got)
		}
		if strings.Contains(got, "DISABLE TRIGGER") {
			t.Errorf("an enabled trigger scripted a DISABLE:\n%s", got)
		}
	})

	t.Run("a disabled trigger scripts its DISABLE", func(t *testing.T) {
		got, err := buildDatabaseTriggerScript(&DatabaseTrigger{Name: "ddl_audit", Definition: def}, ScriptOptions{})
		if err != nil {
			t.Fatalf("buildDatabaseTriggerScript: %v", err)
		}
		if !strings.Contains(got, "DISABLE TRIGGER [ddl_audit] ON DATABASE;") {
			t.Errorf("disabled state not scripted:\n%s", got)
		}
	})

	t.Run("drop and create", func(t *testing.T) {
		got, err := buildDatabaseTriggerScript(&DatabaseTrigger{Name: "ddl_audit", IsEnabled: true, Definition: def},
			ScriptOptions{Verb: ScriptDropAndCreate})
		if err != nil {
			t.Fatalf("buildDatabaseTriggerScript: %v", err)
		}
		if !strings.Contains(got, "DROP TRIGGER IF EXISTS [ddl_audit] ON DATABASE;") {
			t.Errorf("drop half missing:\n%s", got)
		}
		if !strings.Contains(got, def) {
			t.Errorf("create half missing:\n%s", got)
		}
	})

	t.Run("drop only reads no definition", func(t *testing.T) {
		got, err := buildDatabaseTriggerScript(&DatabaseTrigger{Name: "ddl_audit"}, ScriptOptions{Verb: ScriptDrop})
		if err != nil {
			t.Fatalf("buildDatabaseTriggerScript: %v", err)
		}
		if strings.Contains(got, "CREATE") || strings.Contains(got, "DISABLE") {
			t.Errorf("a drop-only script carried more than the drop:\n%s", got)
		}
	})

	t.Run("alter rewrites the create", func(t *testing.T) {
		got, err := buildDatabaseTriggerScript(&DatabaseTrigger{Name: "ddl_audit", IsEnabled: true, Definition: def},
			ScriptOptions{Verb: ScriptAlter})
		if err != nil {
			t.Fatalf("buildDatabaseTriggerScript: %v", err)
		}
		if !strings.HasPrefix(got, "ALTER TRIGGER [ddl_audit] ON DATABASE") {
			t.Errorf("CREATE was not rewritten to ALTER:\n%s", got)
		}
	})

	// An unreadable definition must fail rather than emit a script whose drop
	// half runs and whose create half is empty.
	t.Run("an unreadable definition is an error", func(t *testing.T) {
		if _, err := buildDatabaseTriggerScript(&DatabaseTrigger{Name: "clr_trig"}, ScriptOptions{Verb: ScriptDropAndCreate}); err == nil {
			t.Error("want an error for a trigger with no definition, got nil")
		}
	})
}
