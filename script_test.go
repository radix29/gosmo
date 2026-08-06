package gosmo

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	mssql "github.com/microsoft/go-mssqldb"
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

// The sql.Null* family, UniqueIdentifier and NullUniqueIdentifier are the
// destination types whose Go *kind* cannot give their T-SQL type away —
// sql.Null* and NullUniqueIdentifier are structs, UniqueIdentifier is a
// [16]byte array — so scriptDeclType's kind switch sent every one of them to
// SQL_VARIANT. SQL Server then refused the scripted EXEC outright ("Implicit
// conversion from data type sql_variant to int is not allowed"), which meant
// pointing an OUTPUT parameter at a nullable destination — the ordinary way
// to receive one — produced a script the user could not run. Each mapping
// here was checked against a real procedure with the matching parameter
// type; see live_execproc_script_test.go.
func TestScriptDeclTypeMapsNullableAndUUIDDestinations(t *testing.T) {
	var (
		ni  sql.NullInt64
		n32 sql.NullInt32
		n16 sql.NullInt16
		nby sql.NullByte
		ns  sql.NullString
		nb  sql.NullBool
		nf  sql.NullFloat64
		nt  sql.NullTime
		tt  time.Time
		g   mssql.UniqueIdentifier
		ng  mssql.NullUniqueIdentifier
	)
	for _, tc := range []struct {
		name string
		dest any
		want string
	}{
		{"*sql.NullInt64", &ni, "BIGINT"},
		{"*sql.NullInt32", &n32, "INT"},
		{"*sql.NullInt16", &n16, "SMALLINT"},
		{"*sql.NullByte", &nby, "TINYINT"},
		{"*sql.NullString", &ns, "NVARCHAR(MAX)"},
		{"*sql.NullBool", &nb, "BIT"},
		{"*sql.NullFloat64", &nf, "FLOAT"},
		{"*sql.NullTime", &nt, "DATETIME2"},
		{"*time.Time", &tt, "DATETIME2"},
		{"*mssql.UniqueIdentifier", &g, "UNIQUEIDENTIFIER"},
		{"*mssql.NullUniqueIdentifier", &ng, "UNIQUEIDENTIFIER"},
	} {
		if got := scriptDeclType(tc.dest); got != tc.want {
			t.Errorf("scriptDeclType(%s) = %s, want %s", tc.name, got, tc.want)
		}
	}
}

// SQL_VARIANT stays the fallthrough for a type with no mapping, and it is a
// working one: a procedure whose parameter really is sql_variant takes a
// DECLARE @v SQL_VARIANT without complaint (verified live). So the
// fallthrough must not be turned into an error — that would break the case
// it is correct for.
func TestScriptDeclTypeKeepsSQLVariantForUnmappedDestinations(t *testing.T) {
	var unmapped struct{ X int }
	if got := scriptDeclType(&unmapped); got != "SQL_VARIANT" {
		t.Errorf("scriptDeclType(*struct{X int}) = %s, want SQL_VARIANT", got)
	}
	// A non-pointer has nothing to read the value back into at all.
	if got := scriptDeclType(42); got != "SQL_VARIANT" {
		t.Errorf("scriptDeclType(non-pointer) = %s, want SQL_VARIANT", got)
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

// TestServerScopePermissionsScriptTheUsePrefix pins that a captured
// server-scope grant carries "USE master" — the statement SQL Server needs
// before it will accept one at all. See GrantServerPermissionContext on why
// the prefix form is safe against the connection pool.
func TestServerScopePermissionsScriptTheUsePrefix(t *testing.T) {
	s := &Server{}
	for _, c := range []struct {
		name string
		run  func(ctx context.Context) error
		want string
	}{
		{"grant", func(ctx context.Context) error {
			return s.GrantServerPermissionContext(ctx, "CONNECT SQL", "app_user")
		}, "GRANT CONNECT SQL TO [app_user]"},
		{"deny", func(ctx context.Context) error {
			return s.DenyServerPermissionContext(ctx, "CONNECT SQL", "app_user")
		}, "DENY CONNECT SQL TO [app_user]"},
		{"revoke", func(ctx context.Context) error {
			return s.RevokeServerPermissionContext(ctx, "CONNECT SQL", "app_user")
		}, "REVOKE CONNECT SQL FROM [app_user]"},
	} {
		t.Run(c.name, func(t *testing.T) {
			ctx, script := WithScript(context.Background())
			if err := c.run(ctx); err != nil {
				t.Fatalf("%s under WithScript: %v", c.name, err)
			}
			if len(script.Statements) != 1 {
				t.Fatalf("Statements = %d, want 1", len(script.Statements))
			}
			got := script.Statements[0]
			if !strings.HasPrefix(got, "USE master; ") {
				t.Errorf("Statements[0] = %q, want it to open with USE master", got)
			}
			if !strings.Contains(got, c.want) {
				t.Errorf("Statements[0] = %q, want it to contain %q", got, c.want)
			}
		})
	}
}

// TestServerPermissionRejectedBeforeConnecting pins that the allowlist runs
// before anything touches the pool — validation must not depend on being
// connected.
func TestServerPermissionRejectedBeforeConnecting(t *testing.T) {
	s := &Server{}
	if err := s.GrantServerPermissionContext(context.Background(), "DROP TABLE x", "app_user"); err == nil {
		t.Error("GrantServerPermissionContext accepted an unrecognized permission")
	}
}

// The four Agent create methods end with a read-back that populates the
// returned object's fields. That read is a real query, so under WithScript —
// where the CREATE itself was only collected — it used to fail with
// "not found", which is what broke Script Changes on every create dialog
// whose later page acts on what an earlier page created (New Schedule's
// Jobs page, New Job's Schedules page, New Alert's Response page). Each must
// now come back with a name-only handle and no error.
func TestScriptedAgentCreatesReturnNameOnlyHandles(t *testing.T) {
	s := &Server{}

	t.Run("schedule", func(t *testing.T) {
		ctx, script := WithScript(context.Background())
		sch, err := s.CreateScheduleContext(ctx, CreateScheduleRequest{Name: "Nightly", FreqType: FreqDaily})
		if err != nil {
			t.Fatalf("CreateScheduleContext under WithScript: %v", err)
		}
		if sch == nil || sch.Name != "Nightly" {
			t.Fatalf("returned schedule = %+v, want a handle named \"Nightly\"", sch)
		}
		if len(script.Statements) != 1 || !strings.Contains(script.Statements[0], "sp_add_schedule") {
			t.Errorf("Statements = %v, want one sp_add_schedule", script.Statements)
		}
		// The handle has to be usable for the dependent statement the next
		// page scripts — that is the whole point of returning one.
		if err := s.Job("nightly reindex").AttachScheduleContext(ctx, sch.Name); err != nil {
			t.Fatalf("AttachScheduleContext under WithScript: %v", err)
		}
		if len(script.Statements) != 2 || !strings.Contains(script.Statements[1], "sp_attach_schedule") {
			t.Errorf("Statements = %v, want sp_add_schedule then sp_attach_schedule", script.Statements)
		}
	})

	t.Run("job", func(t *testing.T) {
		ctx, script := WithScript(context.Background())
		j, err := s.CreateJobContext(ctx, CreateJobRequest{Name: "nightly reindex"})
		if err != nil {
			t.Fatalf("CreateJobContext under WithScript: %v", err)
		}
		if j == nil || j.Name != "nightly reindex" {
			t.Fatalf("returned job = %+v, want a handle named \"nightly reindex\"", j)
		}
		// sp_add_job and sp_add_jobserver, then the dependent step.
		if err := j.AddStepContext(ctx, JobStepRequest{Name: "step 1", Subsystem: "TSQL", Command: "SELECT 1"}); err != nil {
			t.Fatalf("AddStepContext under WithScript: %v", err)
		}
		if len(script.Statements) != 3 || !strings.Contains(script.Statements[2], "sp_add_jobstep") {
			t.Errorf("Statements = %v, want sp_add_job, sp_add_jobserver, sp_add_jobstep", script.Statements)
		}
	})

	t.Run("alert", func(t *testing.T) {
		ctx, script := WithScript(context.Background())
		a, err := s.CreateAlertContext(ctx, CreateAlertRequest{Name: "sev 19", Severity: 19})
		if err != nil {
			t.Fatalf("CreateAlertContext under WithScript: %v", err)
		}
		if a == nil || a.Name != "sev 19" {
			t.Fatalf("returned alert = %+v, want a handle named \"sev 19\"", a)
		}
		if err := a.NotifyContext(ctx, "dba", NotifyMethodEmail); err != nil {
			t.Fatalf("NotifyContext under WithScript: %v", err)
		}
		if len(script.Statements) != 2 || !strings.Contains(script.Statements[1], "sp_add_notification") {
			t.Errorf("Statements = %v, want sp_add_alert then sp_add_notification", script.Statements)
		}
	})

	t.Run("operator", func(t *testing.T) {
		ctx, script := WithScript(context.Background())
		o, err := s.CreateOperatorContext(ctx, CreateOperatorRequest{Name: "dba", Enabled: true})
		if err != nil {
			t.Fatalf("CreateOperatorContext under WithScript: %v", err)
		}
		if o == nil || o.Name != "dba" {
			t.Fatalf("returned operator = %+v, want a handle named \"dba\"", o)
		}
		if len(script.Statements) != 1 || !strings.Contains(script.Statements[0], "sp_add_operator") {
			t.Errorf("Statements = %v, want one sp_add_operator", script.Statements)
		}
	})
}
