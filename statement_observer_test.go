package gosmo

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
)

// observed collects what WithStatementObserver reports.
func observed(ctx context.Context) (context.Context, *[]ScriptEntry) {
	var got []ScriptEntry
	return WithStatementObserver(ctx, func(e ScriptEntry) { got = append(got, e) }), &got
}

func observedSQL(entries []ScriptEntry) []string {
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.SQL
	}
	return out
}

// The observer fires once per statement the server accepted, in order, and
// never for the one that failed — the caller uses the count to tell "nothing
// reached the server" from "part of it did".
func TestStatementObserverReportsExecutedStatementsOnly(t *testing.T) {
	refused := errors.New("refused")
	srv := auditServer(t, &auditScript{onExec: func(q string) error {
		if strings.Contains(q, "fails") {
			return refused
		}
		return nil
	}})
	ctx, got := observed(context.Background())

	for _, stmt := range []string{"SELECT 'one'", "SELECT 'two'"} {
		if err := srv.exec(ctx, stmt); err != nil {
			t.Fatalf("exec %q: %v", stmt, err)
		}
	}
	if err := srv.exec(ctx, "SELECT 'fails'"); !errors.Is(err, refused) {
		t.Fatalf("exec of the failing statement = %v, want %v", err, refused)
	}

	want := []string{"SELECT 'one'", "SELECT 'two'"}
	if sql := observedSQL(*got); !slices.Equal(sql, want) {
		t.Errorf("observed %v, want %v", sql, want)
	}
}

// A database-scoped write is reported with its database and with bound
// parameters substituted, and the USE that pins the connection is not a
// statement of the caller's.
func TestStatementObserverDatabaseScope(t *testing.T) {
	srv := auditServer(t, &auditScript{})
	d := srv.DatabaseRef("sales")
	ctx, got := observed(context.Background())

	if _, err := d.exec(ctx, "EXEC sp_x @p1", "it's"); err != nil {
		t.Fatalf("exec: %v", err)
	}
	if len(*got) != 1 {
		t.Fatalf("observed %d entries, want 1: %v", len(*got), *got)
	}
	e := (*got)[0]
	if e.Database != "sales" || e.SQL != "EXEC sp_x N'it''s'" {
		t.Errorf("entry = %+v, want Database sales and the parameter bound", e)
	}
}

// Under WithScript nothing is executed, so nothing is observed — Script
// Changes must never look like a partly applied change.
func TestStatementObserverSilentUnderWithScript(t *testing.T) {
	srv := auditServer(t, &auditScript{})
	ctx, got := observed(context.Background())
	ctx, col := WithScript(ctx)

	if err := srv.exec(ctx, "SELECT 1"); err != nil {
		t.Fatalf("exec: %v", err)
	}
	if _, err := srv.DatabaseRef("sales").exec(ctx, "SELECT 2"); err != nil {
		t.Fatalf("db exec: %v", err)
	}
	if len(col.Entries) != 2 {
		t.Fatalf("collected %d, want 2", len(col.Entries))
	}
	if len(*got) != 0 {
		t.Errorf("observed %v under WithScript, want nothing", observedSQL(*got))
	}
}

// A disable window's own STATE = OFF / STATE = ON are not reported. When the
// ALTER inside the window is refused and the window closes cleanly, the server
// is as it was — so nothing may be observed, or the caller would discard edits
// that never landed.
func TestStatementObserverSkipsWindowBrackets(t *testing.T) {
	for _, tc := range []struct {
		name      string
		failAlter bool
		wantAlter int
	}{
		{"alter succeeds", false, 2},
		{"alter refused", true, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := auditServer(t, &auditScript{enabled: true, onExec: func(q string) error {
				if tc.failAlter && !strings.Contains(q, "STATE =") {
					return errors.New("refused")
				}
				return nil
			}})
			ctx, got := observed(context.Background())
			err := srv.ServerAuditRef("a").Alter(ctx, ServerAuditSpec{Name: "a", QueueDelay: 2000})
			if (err != nil) != tc.failAlter {
				t.Fatalf("Alter: %v", err)
			}
			for _, e := range *got {
				if strings.Contains(e.SQL, "STATE =") {
					t.Errorf("observed the window bracket %q", e.SQL)
				}
			}
			if len(*got) != tc.wantAlter {
				t.Errorf("observed %d statements, want %d: %v", len(*got), tc.wantAlter, observedSQL(*got))
			}
			// The bracket went to the server all the same.
			if n := countStatements(auditCurrent.execs, "STATE = ON"); n != 1 {
				t.Errorf("re-enable ran %d times, want 1: %v", n, auditCurrent.execs)
			}
		})
	}
}

// An observer already on the context keeps firing when another is added.
func TestStatementObserverChains(t *testing.T) {
	srv := auditServer(t, &auditScript{})
	ctx, outer := observed(context.Background())
	ctx, inner := observed(ctx)

	if err := srv.exec(ctx, "SELECT 1"); err != nil {
		t.Fatalf("exec: %v", err)
	}
	if len(*outer) != 1 || len(*inner) != 1 {
		t.Errorf("outer saw %d, inner saw %d, want 1 each", len(*outer), len(*inner))
	}
}
