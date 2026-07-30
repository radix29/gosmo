package gosmo

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"
)

func TestWithScriptCapturesServerWriteWithoutExecuting(t *testing.T) {
	s := &Server{}
	ctx, script := WithScript(context.Background())

	if err := s.GrantServerPermissionContext(ctx, "CONNECT SQL", "app_user"); err != nil {
		t.Fatalf("GrantServerPermissionContext under WithScript: %v", err)
	}

	if len(script.Statements) != 1 {
		t.Fatalf("Statements = %d, want 1", len(script.Statements))
	}
	if !strings.Contains(script.Statements[0], "GRANT CONNECT SQL TO") {
		t.Errorf("Statements[0] = %q, want a GRANT CONNECT SQL statement", script.Statements[0])
	}
}

func TestWithScriptCapturesDatabaseWriteWithoutExecuting(t *testing.T) {
	d := &Database{server: &Server{}, name: "AppDB"}
	ctx, script := WithScript(context.Background())

	if err := d.GrantDatabasePermissionContext(ctx, "SELECT", "app_user"); err != nil {
		t.Fatalf("GrantDatabasePermissionContext under WithScript: %v", err)
	}

	if len(script.Statements) != 1 {
		t.Fatalf("Statements = %d, want 1", len(script.Statements))
	}
	if !strings.Contains(script.Statements[0], "GRANT SELECT TO") {
		t.Errorf("Statements[0] = %q, want a GRANT SELECT statement", script.Statements[0])
	}
	if !strings.HasPrefix(script.Statements[0], "USE [AppDB]") {
		t.Errorf("Statements[0] = %q, want a USE [AppDB] prefix (the real path always runs after USE)", script.Statements[0])
	}
}

func TestWithScriptCollectorsAreIndependent(t *testing.T) {
	s := &Server{}
	ctx1, script1 := WithScript(context.Background())
	ctx2, script2 := WithScript(context.Background())

	if err := s.GrantServerPermissionContext(ctx1, "CONNECT SQL", "a"); err != nil {
		t.Fatalf("grant under ctx1: %v", err)
	}
	if err := s.GrantServerPermissionContext(ctx2, "CONNECT SQL", "b"); err != nil {
		t.Fatalf("grant under ctx2: %v", err)
	}

	if len(script1.Statements) != 1 || !strings.Contains(script1.Statements[0], "TO [a]") {
		t.Errorf("script1.Statements = %v, want exactly the grant to \"a\"", script1.Statements)
	}
	if len(script2.Statements) != 1 || !strings.Contains(script2.Statements[0], "TO [b]") {
		t.Errorf("script2.Statements = %v, want exactly the grant to \"b\"", script2.Statements)
	}
}

// Scripting is what lets a caller tell a recorded write from one that
// really ran, so it can hold back state (a renamed object's new name, say)
// that the server doesn't actually have yet.
func TestScriptingReportsWhetherWritesAreRecorded(t *testing.T) {
	plain := context.Background()
	if Scripting(plain) {
		t.Error("Scripting(context.Background()) = true, want false")
	}

	ctx, _ := WithScript(plain)
	if !Scripting(ctx) {
		t.Error("Scripting(WithScript(...)) = false, want true")
	}

	// The marker rides on the context, so it survives further derivation
	// the way a caller adding its own timeout would leave it.
	derived, cancel := context.WithCancel(ctx)
	defer cancel()
	if !Scripting(derived) {
		t.Error("Scripting lost the collector across context.WithCancel")
	}
}

// -- parameter binding ------------------------------------------------------

// TestWithScriptBindsParametersIntoTheStatement covers the four parameterised
// write methods. A captured statement is run by hand in a query editor, where
// nothing binds @p1 — before this, each of these produced a script that failed
// with "Must declare the scalar variable '@p1'".
func TestWithScriptBindsParametersIntoTheStatement(t *testing.T) {
	cases := []struct {
		name   string
		write  func(context.Context, *Database) error
		want   []string
		absent string
	}{
		{
			name:  "RenameTable",
			write: func(ctx context.Context, d *Database) error { return d.RenameTableContext(ctx, "dbo", "Old", "New") },
			want:  []string{"EXEC sp_rename", "N'[dbo].[Old]'", "N'New'", "N'OBJECT'"},
		},
		{
			name: "Index.Rename",
			write: func(ctx context.Context, d *Database) error {
				t := &Table{db: d, Schema: "dbo", Name: "Orders"}
				idx := &Index{Name: "IX_Old"}
				return idx.RenameContext(ctx, t, "IX_New")
			},
			want: []string{"EXEC sp_rename", "N'[dbo].[Orders].[IX_Old]'", "N'IX_New'", "N'INDEX'"},
		},
		{
			name: "DropColumn",
			write: func(ctx context.Context, d *Database) error {
				return (&Table{db: d, Schema: "dbo", Name: "Orders", ObjectID: 1234}).DropColumnContext(ctx, "Notes")
			},
			want: []string{"dc.parent_object_id = 1234", "c.name = N'Notes'", "DROP COLUMN [Notes]"},
		},
		{
			name:  "DropTable cascade",
			write: func(ctx context.Context, d *Database) error { return d.DropTableContext(ctx, "dbo", "Orders", true) },
			want:  []string{"OBJECT_ID(N'[dbo].[Orders]')", "DROP TABLE IF EXISTS [dbo].[Orders]"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := &Database{server: &Server{}, name: "AppDB"}
			ctx, script := WithScript(context.Background())
			if err := tc.write(ctx, d); err != nil {
				t.Fatalf("%s under WithScript: %v", tc.name, err)
			}
			all := strings.Join(script.Statements, "\n")
			for _, want := range tc.want {
				if !strings.Contains(all, want) {
					t.Errorf("captured script missing %q:\n%s", want, all)
				}
			}
			if strings.Contains(all, "@p1") || strings.Contains(all, "@p2") {
				t.Errorf("captured script still has an unbound placeholder:\n%s", all)
			}
		})
	}
}

// TestWithScriptExecProcRendersAnExecStatement pins the one write whose real
// path is an RPC rather than SQL text: the statement handed to the driver is
// the bare procedure name, so capturing it verbatim recorded an object name
// with no EXEC and no parameters at all.
func TestWithScriptExecProcRendersAnExecStatement(t *testing.T) {
	d := &Database{server: &Server{}, name: "AppDB"}
	ctx, script := WithScript(context.Background())

	var out int64
	inOut := "seed"
	if _, err := d.ExecProcContext(ctx, "dbo", "DoWork",
		In("mode", 3), In("label", "it's fine"), Out("total", &out), InOut("tag", &inOut),
	); err != nil {
		t.Fatalf("ExecProcContext under WithScript: %v", err)
	}
	if len(script.Statements) != 1 {
		t.Fatalf("Statements = %d, want 1", len(script.Statements))
	}
	got := script.Statements[0]
	for _, want := range []string{
		"DECLARE @total BIGINT;",
		"DECLARE @tag NVARCHAR(MAX) = N'seed';",
		"EXEC [dbo].[DoWork] @mode = 3, @label = N'it''s fine', @total = @total OUTPUT, @tag = @tag OUTPUT",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("captured statement missing %q:\n%s", want, got)
		}
	}
}

// TestWithScriptRefusesAnUnscriptableArgument checks that an argument with no
// literal form is an error rather than a %v guess — a silently wrong literal
// in a script the user is about to run by hand is worse than a refusal.
func TestWithScriptRefusesAnUnscriptableArgument(t *testing.T) {
	if _, err := bindScriptArgs("SELECT @p1", []any{struct{ X int }{1}}); err == nil {
		t.Error("bindScriptArgs accepted an unscriptable argument, want an error")
	}
	if _, err := bindScriptArgs("SELECT @p1, @p2", []any{1}); err == nil {
		t.Error("bindScriptArgs accepted a statement with more placeholders than arguments, want an error")
	}
}

// TestBindScriptArgsRefusesNamedArguments pins the guard that a named
// argument is rejected even when nothing in the statement matches @pN. The
// scan for placeholders never sees "@name", so without an up-front check the
// statement scripts with every parameter silently unbound.
func TestBindScriptArgsRefusesNamedArguments(t *testing.T) {
	q := "EXEC dbo.p @name = @name"
	if _, err := bindScriptArgs(q, []any{sql.Named("name", 1)}); err == nil {
		t.Error("bindScriptArgs accepted a named argument with no @pN placeholder, want an error")
	}
	// And still rejected on the path that does find a placeholder.
	if _, err := bindScriptArgs("SELECT @p1", []any{sql.Named("name", 1)}); err == nil {
		t.Error("bindScriptArgs accepted a named argument for an @pN placeholder, want an error")
	}
}

// TestBindScriptArgsMatchesWholePlaceholders guards the greedy-digit match:
// substituting "@p1" into "@p10" would corrupt every statement with ten or
// more parameters.
func TestBindScriptArgsMatchesWholePlaceholders(t *testing.T) {
	args := make([]any, 10)
	for i := range args {
		args[i] = i + 1
	}
	got, err := bindScriptArgs("VALUES (@p1, @p10)", args)
	if err != nil {
		t.Fatalf("bindScriptArgs: %v", err)
	}
	if want := "VALUES (1, 10)"; got != want {
		t.Errorf("bindScriptArgs = %q, want %q", got, want)
	}
}

func TestScriptLiteral(t *testing.T) {
	cases := []struct {
		in   any
		want string
	}{
		{nil, "NULL"},
		{"plain", "N'plain'"},
		{"it's", "N'it''s'"},
		{true, "1"},
		{false, "0"},
		{42, "42"},
		{int64(-7), "-7"},
		{1.5, "1.5"},
		{[]byte{0xDE, 0xAD}, "0xDEAD"},
		// "0x" alone is not a valid T-SQL binary literal.
		{[]byte{}, "0x00"},
		{time.Date(2026, 7, 30, 14, 5, 6, 0, time.UTC), "'2026-07-30T14:05:06'"},
	}
	for _, tc := range cases {
		got, err := scriptLiteral(tc.in)
		if err != nil {
			t.Errorf("scriptLiteral(%#v): %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("scriptLiteral(%#v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
