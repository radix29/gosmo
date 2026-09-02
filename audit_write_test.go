package gosmo

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"slices"
	"strings"
	"testing"
)

// -- a driver that answers only the enabled-state read -------------------------
//
// The off/apply/on dance is the whole point of these writes, and it turns on a
// catalog read the ScriptCollector does not intercept: WithScript collects
// writes, reads still execute. So the statement order can only be pinned with
// a connection behind the Server, however small.

type auditScript struct {
	enabled bool // what is_state_enabled answers
	missing bool // the audit / specification is not there at all
}

var auditCurrent *auditScript

type auditDriver struct{}

func (auditDriver) Open(string) (driver.Conn, error) { return &auditConn{}, nil }

type auditConn struct{}

func (*auditConn) Prepare(string) (driver.Stmt, error) { return nil, driver.ErrSkip }
func (*auditConn) Close() error                        { return nil }
func (*auditConn) Begin() (driver.Tx, error)           { return nil, driver.ErrSkip }

func (*auditConn) ExecContext(context.Context, string, []driver.NamedValue) (driver.Result, error) {
	return driver.ResultNoRows, nil
}

func (*auditConn) QueryContext(_ context.Context, q string, _ []driver.NamedValue) (driver.Rows, error) {
	if strings.Contains(q, "is_state_enabled") {
		rows := [][]driver.Value{{auditCurrent.enabled}}
		if auditCurrent.missing {
			rows = nil
		}
		return &capRows{cols: 1, rows: rows}, nil
	}
	return fakeInfoAnswer(q, nil)
}

func init() { sql.Register("auditdb", auditDriver{}) }

func auditServer(t *testing.T, s *auditScript) *Server {
	t.Helper()
	auditCurrent = s
	pool, err := sql.Open("auditdb", "")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { pool.Close() })
	srv, err := NewServer(context.Background(), pool)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	return srv
}

// -- statement shapes ----------------------------------------------------------

func TestServerAuditStateStatements(t *testing.T) {
	for _, tc := range []struct {
		on   bool
		want string
	}{
		{true, "ALTER SERVER AUDIT [odd]]name] WITH ( STATE = ON )"},
		{false, "ALTER SERVER AUDIT [odd]]name] WITH ( STATE = OFF )"},
	} {
		ctx, col := WithScript(context.Background())
		a := &ServerAudit{server: &Server{}, Name: "odd]name"}
		if err := a.SetStateContext(ctx, tc.on); err != nil {
			t.Fatalf("SetStateContext: %v", err)
		}
		if len(col.Statements) != 1 || col.Statements[0] != tc.want {
			t.Errorf("got %v, want [%s]", col.Statements, tc.want)
		}
		// setIfApplied must not mirror a state that was only collected.
		if a.IsEnabled {
			t.Error("IsEnabled mirrored while scripting")
		}
	}
}

func TestCreateServerAuditStatement(t *testing.T) {
	for _, tc := range []struct {
		name string
		spec ServerAuditSpec
		want []string
		bad  bool
	}{
		{name: "file target",
			spec: ServerAuditSpec{Name: "a'b", Type: AuditToFile, FilePath: `C:\it's`,
				MaxFileSize: 10, MaxRolloverFiles: 3, QueueDelay: 1000, OnFailure: AuditFailureContinue},
			want: []string{"CREATE SERVER AUDIT [a'b]", "TO FILE", `FILEPATH = N'C:\it''s'`,
				"MAXSIZE = 10 MB", "MAX_ROLLOVER_FILES = 3", "RESERVE_DISK_SPACE = OFF",
				"QUEUE_DELAY = 1000", "ON_FAILURE = CONTINUE"}},
		{name: "unlimited size and rollover",
			spec: ServerAuditSpec{Name: "a", Type: AuditToFile, FilePath: `C:\x`,
				MaxRolloverFiles: AuditUnlimited},
			want: []string{"MAXSIZE = UNLIMITED", "MAX_ROLLOVER_FILES = UNLIMITED"}},
		{name: "max files wins over rollover",
			spec: ServerAuditSpec{Name: "a", Type: AuditToFile, FilePath: `C:\x`,
				MaxFiles: 7, MaxRolloverFiles: AuditUnlimited},
			want: []string{"MAX_FILES = 7"}},
		{name: "application log",
			spec: ServerAuditSpec{Name: "a", Type: AuditToApplicationLog, OnFailure: AuditFailureShutdown},
			want: []string{"TO APPLICATION_LOG", "ON_FAILURE = SHUTDOWN"}},
		{name: "security log and fail operation",
			spec: ServerAuditSpec{Name: "a", Type: AuditToSecurityLog, OnFailure: AuditFailureFailOp},
			want: []string{"TO SECURITY_LOG", "ON_FAILURE = FAIL_OPERATION"}},
		{name: "predicate",
			spec: ServerAuditSpec{Name: "a", Type: AuditToSecurityLog,
				Predicate: "server_principal_name <> N'sa'"},
			want: []string{"WHERE server_principal_name <> N'sa'"}},
		{name: "no name", spec: ServerAuditSpec{Type: AuditToSecurityLog}, bad: true},
		{name: "no destination", spec: ServerAuditSpec{Name: "a"}, bad: true},
		{name: "file with no path", spec: ServerAuditSpec{Name: "a", Type: AuditToFile}, bad: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.spec.createServerAuditStatement()
			if tc.bad {
				if err == nil {
					t.Fatalf("want an error, got %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("createServerAuditStatement: %v", err)
			}
			for _, w := range tc.want {
				if !strings.Contains(got, w) {
					t.Errorf("missing %q in:\n%s", w, got)
				}
			}
			// MAX_FILES and MAX_ROLLOVER_FILES together is a syntax error.
			if strings.Contains(got, "MAX_FILES") && strings.Contains(got, "MAX_ROLLOVER_FILES") {
				t.Errorf("both file-count options in one statement:\n%s", got)
			}
		})
	}
}

// TestAlteringAnAuditTurnsItOffAndBackOn is the rule the plan says must live in
// the library: SQL Server refuses every ALTER but the state toggle, and the
// DROP, while the audit is enabled. A caller doing it by hand gets the failure
// path wrong, and a "simplification" that drops the dance produces a write the
// server refuses with a message naming neither the audit nor the fix.
func TestAlteringAnAuditTurnsItOffAndBackOn(t *testing.T) {
	for _, tc := range []struct {
		name    string
		enabled bool
		want    []string
	}{
		// The spec carries no predicate, so the settings ALTER is followed by
		// its own REMOVE WHERE — two "alter"s, not one.
		{"enabled", true, []string{
			"ALTER SERVER AUDIT [a] WITH ( STATE = OFF )",
			"alter", "alter",
			"ALTER SERVER AUDIT [a] WITH ( STATE = ON )",
		}},
		// A disabled audit must not be switched on as a side effect of an edit.
		{"already disabled", false, []string{"alter", "alter"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := auditServer(t, &auditScript{enabled: tc.enabled})
			ctx, col := WithScript(context.Background())
			a := srv.ServerAudit("a")
			if err := a.AlterContext(ctx, ServerAuditSpec{Name: "a", QueueDelay: 2000}); err != nil {
				t.Fatalf("AlterContext: %v", err)
			}
			got := make([]string, 0, len(col.Statements))
			for _, s := range col.Statements {
				if strings.Contains(s, "STATE =") {
					got = append(got, s)
					continue
				}
				got = append(got, "alter")
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("statements = %v, want %v (raw: %v)", got, tc.want, col.Statements)
			}
		})
	}
}

func TestDroppingAnEnabledAuditDisablesItFirst(t *testing.T) {
	for _, tc := range []struct {
		name    string
		enabled bool
		want    []string
	}{
		{"enabled", true, []string{
			"ALTER SERVER AUDIT [a] WITH ( STATE = OFF )",
			"DROP SERVER AUDIT [a]",
		}},
		{"already disabled", false, []string{"DROP SERVER AUDIT [a]"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := auditServer(t, &auditScript{enabled: tc.enabled})
			ctx, col := WithScript(context.Background())
			if err := srv.ServerAudit("a").DropContext(ctx); err != nil {
				t.Fatalf("DropContext: %v", err)
			}
			if !slices.Equal(col.Statements, tc.want) {
				t.Errorf("statements = %v, want %v", col.Statements, tc.want)
			}
		})
	}
}

// An alter that never reaches the audit must not report success, and must not
// leave a disable behind it either.
func TestAlteringAMissingAuditIsNotFound(t *testing.T) {
	srv := auditServer(t, &auditScript{missing: true})
	ctx, col := WithScript(context.Background())
	err := srv.ServerAudit("gone").AlterContext(ctx, ServerAuditSpec{Name: "gone"})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("got %v, want a not-found error", err)
	}
	if len(col.Statements) != 0 {
		t.Errorf("a refused alter still built %v", col.Statements)
	}
}

// REMOVE WHERE has to be its own statement: combined with WITH(...) the server
// answers "Incorrect syntax near 'REMOVE'", which is how a Properties page
// Apply failed live after every unit test here passed.
func TestClearingAPredicateIsItsOwnStatement(t *testing.T) {
	stmts := ServerAuditSpec{Name: "a", QueueDelay: 1000}.alterServerAuditStatements("a")
	if len(stmts) != 2 {
		t.Fatalf("got %d statements, want the settings ALTER and a separate REMOVE WHERE: %v", len(stmts), stmts)
	}
	if strings.Contains(stmts[0], "REMOVE") {
		t.Errorf("REMOVE WHERE shares a statement with the settings:\n%s", stmts[0])
	}
	if stmts[1] != "ALTER SERVER AUDIT [a] REMOVE WHERE" {
		t.Errorf("the clearing statement is %q", stmts[1])
	}

	stmts = ServerAuditSpec{Name: "a", Predicate: "1 = 1"}.alterServerAuditStatements("a")
	if len(stmts) != 1 || !strings.Contains(stmts[0], "WHERE 1 = 1") || strings.Contains(stmts[0], "REMOVE") {
		t.Errorf("a predicate must be written in one statement, not removed: %v", stmts)
	}
}

func TestRenamingAnAuditUsesModifyName(t *testing.T) {
	srv := auditServer(t, &auditScript{enabled: false})
	ctx, col := WithScript(context.Background())
	a := srv.ServerAudit("old")
	if err := a.RenameContext(ctx, "new]er"); err != nil {
		t.Fatalf("RenameContext: %v", err)
	}
	want := "ALTER SERVER AUDIT [old] MODIFY NAME = [new]]er]"
	if len(col.Statements) != 1 || col.Statements[0] != want {
		t.Errorf("got %v, want [%s]", col.Statements, want)
	}
	if a.Name != "old" {
		t.Errorf("Name mirrored to %q while scripting", a.Name)
	}
}

// TestRenamingAnEnabledAuditReEnablesUnderTheNewName pins the half the
// disabled-audit case cannot reach: the restore runs after MODIFY NAME has
// committed, so an ON addressed to the old name fails with the rename already
// applied and leaves auditing off.
func TestRenamingAnEnabledAuditReEnablesUnderTheNewName(t *testing.T) {
	srv := auditServer(t, &auditScript{enabled: true})
	ctx, col := WithScript(context.Background())
	if err := srv.ServerAudit("old").RenameContext(ctx, "new"); err != nil {
		t.Fatalf("RenameContext: %v", err)
	}
	want := []string{
		"ALTER SERVER AUDIT [old] WITH ( STATE = OFF )",
		"ALTER SERVER AUDIT [old] MODIFY NAME = [new]",
		"ALTER SERVER AUDIT [new] WITH ( STATE = ON )",
	}
	if !slices.Equal(col.Statements, want) {
		t.Errorf("got %v, want %v", col.Statements, want)
	}
}

// TestAuditWithDisabledOpensOneWindow pins that nested writes share the
// caller's window rather than each stopping auditing for themselves, and that
// a rename inside it moves the restore onto the new name.
func TestAuditWithDisabledOpensOneWindow(t *testing.T) {
	srv := auditServer(t, &auditScript{enabled: true})
	ctx, col := WithScript(context.Background())
	a := srv.ServerAudit("old")
	err := a.WithDisabled(ctx, func(ctx context.Context) error {
		if err := a.AlterContext(ctx, ServerAuditSpec{
			Name: "old", Type: AuditToApplicationLog, QueueDelay: 1000,
			OnFailure: AuditFailureContinue,
		}); err != nil {
			return err
		}
		return a.RenameContext(ctx, "new")
	})
	if err != nil {
		t.Fatalf("WithDisabled: %v", err)
	}
	if n := countStatements(col.Statements, "STATE = OFF"); n != 1 {
		t.Errorf("got %d disables, want 1: %v", n, col.Statements)
	}
	if n := countStatements(col.Statements, "STATE = ON"); n != 1 {
		t.Errorf("got %d enables, want 1: %v", n, col.Statements)
	}
	last := col.Statements[len(col.Statements)-1]
	if last != "ALTER SERVER AUDIT [new] WITH ( STATE = ON )" {
		t.Errorf("restore = %q, want it under the new name", last)
	}
}

func countStatements(stmts []string, sub string) int {
	n := 0
	for _, s := range stmts {
		if strings.Contains(s, sub) {
			n++
		}
	}
	return n
}

// -- the scripter ---------------------------------------------------------------

func TestServerAuditScriptShapes(t *testing.T) {
	a := &ServerAudit{
		Name: "aud'it", Type: AuditToFile, OnFailure: AuditFailureShutdown,
		QueueDelay: 1000, IsEnabled: true,
		// The catalog stores the path with a trailing separator; FILEPATH
		// must not be emitted with one doubled up.
		LogFilePath: `C:\audit\`, MaxRolloverFiles: AuditUnlimited,
	}
	for _, tc := range []struct {
		name string
		opts ScriptOptions
		want []string
		deny []string
	}{
		{name: "drop", opts: ScriptOptions{Verb: ScriptDrop},
			want: []string{"IF EXISTS (SELECT 1 FROM sys.server_audits WHERE name = N'aud''it')",
				"ALTER SERVER AUDIT [aud'it] WITH ( STATE = OFF );", "DROP SERVER AUDIT [aud'it];"},
			deny: []string{"CREATE SERVER AUDIT"}},
		{name: "create", opts: ScriptOptions{Verb: ScriptCreate},
			want: []string{"CREATE SERVER AUDIT [aud'it]", `FILEPATH = N'C:\audit'`,
				"ON_FAILURE = SHUTDOWN", "ALTER SERVER AUDIT [aud'it] WITH ( STATE = ON );"},
			deny: []string{"DROP SERVER AUDIT", `N'C:\audit\'`}},
		{name: "drop and create", opts: ScriptOptions{Verb: ScriptDropAndCreate},
			want: []string{"DROP SERVER AUDIT", "CREATE SERVER AUDIT"}},
		{name: "if not exists", opts: ScriptOptions{Verb: ScriptCreate, IncludeIfNotExists: true},
			want: []string{"IF NOT EXISTS (SELECT 1 FROM sys.server_audits WHERE name = N'aud''it')",
				"BEGIN", "END"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := buildServerAuditScript(a, tc.opts)
			for _, w := range tc.want {
				if !strings.Contains(got, w) {
					t.Errorf("missing %q in:\n%s", w, got)
				}
			}
			for _, d := range tc.deny {
				if strings.Contains(got, d) {
					t.Errorf("unwanted %q in:\n%s", d, got)
				}
			}
		})
	}
}

// A disabled audit's script must not switch auditing on when it is run.
func TestADisabledAuditIsNotScriptedEnabled(t *testing.T) {
	a := &ServerAudit{Name: "a", Type: AuditToSecurityLog}
	got := buildServerAuditScript(a, ScriptOptions{Verb: ScriptCreate})
	if strings.Contains(got, "STATE = ON") {
		t.Errorf("a disabled audit scripted as enabled:\n%s", got)
	}
}
