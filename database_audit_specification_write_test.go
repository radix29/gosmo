package gosmo

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
)

// auditDatabase is a database handle over the audit_write_test.go fake, whose
// QueryContext answers the is_state_enabled probe the disable window makes.
func auditDatabase(t *testing.T, s *auditScript) *Database {
	t.Helper()
	return &Database{name: "app]db", server: auditServer(t, s)}
}

// use is the prefix Database.exec puts in front of every captured statement,
// since a collected script may be pasted into a session scoped elsewhere.
const use = "USE [app]]db];\n"

func TestDatabaseAuditSpecificationStateStatements(t *testing.T) {
	for _, tc := range []struct {
		on   bool
		want string
	}{
		{true, use + "ALTER DATABASE AUDIT SPECIFICATION [odd]]name] WITH ( STATE = ON )"},
		{false, use + "ALTER DATABASE AUDIT SPECIFICATION [odd]]name] WITH ( STATE = OFF )"},
	} {
		ctx, col := WithScript(context.Background())
		spec := &DatabaseAuditSpecification{db: &Database{name: "app]db"}, Name: "odd]name"}
		if err := spec.SetStateContext(ctx, tc.on); err != nil {
			t.Fatalf("SetStateContext: %v", err)
		}
		if len(col.Statements) != 1 || col.Statements[0] != tc.want {
			t.Errorf("got %v, want [%s]", col.Statements, tc.want)
		}
		if spec.IsEnabled {
			t.Error("IsEnabled mirrored while scripting")
		}
	}
}

func TestCreateDatabaseAuditSpecificationStatement(t *testing.T) {
	for _, tc := range []struct {
		name string
		spec DatabaseAuditSpecificationSpec
		want []string
		bad  bool
	}{
		{name: "group and action",
			spec: DatabaseAuditSpecificationSpec{Name: "s]p", AuditName: "a]ud",
				ActionGroups: []string{"SCHEMA_OBJECT_ACCESS_GROUP"},
				Actions: []DatabaseAuditAction{
					{ActionName: "SELECT", ClassDesc: "OBJECT", SchemaName: "dbo", ObjectName: "T", Principal: "public"},
				},
				Enabled: true},
			want: []string{"CREATE DATABASE AUDIT SPECIFICATION [s]]p]", "FOR SERVER AUDIT [a]]ud]",
				"ADD (SCHEMA_OBJECT_ACCESS_GROUP)", "ADD (SELECT ON OBJECT::[dbo].[T] BY [public])",
				"WITH ( STATE = ON )"}},
		// A securable is an identifier, and a ] in its name must be doubled —
		// the opposite treatment to the action keyword beside it.
		{name: "securable with a bracket",
			spec: DatabaseAuditSpecificationSpec{Name: "s", AuditName: "a",
				Actions: []DatabaseAuditAction{
					{ActionName: "INSERT", ClassDesc: "OBJECT", SchemaName: "od]d", ObjectName: "t]bl", Principal: "us]r"},
				}},
			want: []string{"ADD (INSERT ON OBJECT::[od]]d].[t]]bl] BY [us]]r])"}},
		{name: "schema and database classes",
			spec: DatabaseAuditSpecificationSpec{Name: "s", AuditName: "a",
				Actions: []DatabaseAuditAction{
					{ActionName: "EXECUTE", ClassDesc: "SCHEMA", ObjectName: "dbo"},
					{ActionName: "SELECT", ClassDesc: "DATABASE", ObjectName: "appdb", Principal: "dbo"},
				}},
			// An empty principal defaults to public rather than emitting a
			// BY clause that does not parse.
			want: []string{"ADD (EXECUTE ON SCHEMA::[dbo] BY [public])",
				"ADD (SELECT ON DATABASE::[appdb] BY [dbo])"}},
		{name: "no actions disabled",
			spec: DatabaseAuditSpecificationSpec{Name: "s", AuditName: "a"},
			want: []string{"WITH ( STATE = OFF )"}},
		{name: "no name", spec: DatabaseAuditSpecificationSpec{AuditName: "a"}, bad: true},
		{name: "no audit", spec: DatabaseAuditSpecificationSpec{Name: "s"}, bad: true},
		{name: "injected group",
			spec: DatabaseAuditSpecificationSpec{Name: "s", AuditName: "a",
				ActionGroups: []string{"X), ADD (Y"}}, bad: true},
		// The action name and the class are keywords: neither can be quoted,
		// so anything that is not a bare word has to be refused.
		{name: "injected action",
			spec: DatabaseAuditSpecificationSpec{Name: "s", AuditName: "a",
				Actions: []DatabaseAuditAction{{ActionName: "SELECT), ADD (DELETE", ObjectName: "T"}}}, bad: true},
		{name: "bogus class",
			spec: DatabaseAuditSpecificationSpec{Name: "s", AuditName: "a",
				Actions: []DatabaseAuditAction{{ActionName: "SELECT", ClassDesc: "SERVER", ObjectName: "T"}}}, bad: true},
		{name: "action with no securable",
			spec: DatabaseAuditSpecificationSpec{Name: "s", AuditName: "a",
				Actions: []DatabaseAuditAction{{ActionName: "SELECT"}}}, bad: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.spec.createStatement()
			if tc.bad {
				if err == nil {
					t.Fatalf("want an error, got %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("createStatement: %v", err)
			}
			for _, w := range tc.want {
				if !strings.Contains(got, w) {
					t.Errorf("missing %q in:\n%s", w, got)
				}
			}
		})
	}
}

// Same rule as the server half: the server refuses every change while the
// specification is enabled, and a disabled one must not be switched on by an
// edit.
func TestChangingADatabaseSpecificationTurnsItOffAndBackOn(t *testing.T) {
	for _, tc := range []struct {
		name    string
		enabled bool
		act     func(*DatabaseAuditSpecification, context.Context) error
		want    []string
	}{
		{"add while enabled", true,
			func(s *DatabaseAuditSpecification, ctx context.Context) error {
				return s.AddActionsContext(ctx, []string{"SCHEMA_OBJECT_ACCESS_GROUP"},
					[]DatabaseAuditAction{{ActionName: "SELECT", ObjectName: "T", SchemaName: "dbo"}})
			},
			[]string{
				use + "ALTER DATABASE AUDIT SPECIFICATION [s] WITH ( STATE = OFF )",
				use + "ALTER DATABASE AUDIT SPECIFICATION [s]\n    ADD (SCHEMA_OBJECT_ACCESS_GROUP),\n    ADD (SELECT ON OBJECT::[dbo].[T] BY [public])",
				use + "ALTER DATABASE AUDIT SPECIFICATION [s] WITH ( STATE = ON )",
			}},
		{"drop while disabled", false,
			func(s *DatabaseAuditSpecification, ctx context.Context) error {
				return s.DropActionsContext(ctx, []string{"SCHEMA_OBJECT_ACCESS_GROUP"}, nil)
			},
			[]string{use + "ALTER DATABASE AUDIT SPECIFICATION [s]\n    DROP (SCHEMA_OBJECT_ACCESS_GROUP)"}},
		{"reparent while enabled", true,
			func(s *DatabaseAuditSpecification, ctx context.Context) error {
				return s.SetAuditContext(ctx, "other")
			},
			[]string{
				use + "ALTER DATABASE AUDIT SPECIFICATION [s] WITH ( STATE = OFF )",
				use + "ALTER DATABASE AUDIT SPECIFICATION [s]\nFOR SERVER AUDIT [other]",
				use + "ALTER DATABASE AUDIT SPECIFICATION [s] WITH ( STATE = ON )",
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := auditDatabase(t, &auditScript{enabled: tc.enabled})
			ctx, col := WithScript(context.Background())
			if err := tc.act(db.DatabaseAuditSpecification("s"), ctx); err != nil {
				t.Fatalf("act: %v", err)
			}
			if !slices.Equal(col.Statements, tc.want) {
				t.Errorf("statements =\n%#v\nwant\n%#v", col.Statements, tc.want)
			}
		})
	}
}

// One window for a batch, not one per statement — and the nested writes must
// read the context they are handed to see it.
func TestWithDisabledSharesOneWindow(t *testing.T) {
	db := auditDatabase(t, &auditScript{enabled: true})
	ctx, col := WithScript(context.Background())
	spec := db.DatabaseAuditSpecification("s")
	err := spec.WithDisabled(ctx, func(ctx context.Context) error {
		if err := spec.AddActionsContext(ctx, []string{"SCHEMA_OBJECT_ACCESS_GROUP"}, nil); err != nil {
			return err
		}
		return spec.SetAuditContext(ctx, "other")
	})
	if err != nil {
		t.Fatalf("WithDisabled: %v", err)
	}
	want := []string{
		use + "ALTER DATABASE AUDIT SPECIFICATION [s] WITH ( STATE = OFF )",
		use + "ALTER DATABASE AUDIT SPECIFICATION [s]\n    ADD (SCHEMA_OBJECT_ACCESS_GROUP)",
		use + "ALTER DATABASE AUDIT SPECIFICATION [s]\nFOR SERVER AUDIT [other]",
		use + "ALTER DATABASE AUDIT SPECIFICATION [s] WITH ( STATE = ON )",
	}
	if !slices.Equal(col.Statements, want) {
		t.Errorf("statements =\n%#v\nwant\n%#v", col.Statements, want)
	}
}

// The window marker is shared with the server half, so a database
// specification must not mistake a server one's open window for its own.
func TestADatabaseWindowDoesNotMatchAServerWindow(t *testing.T) {
	db := auditDatabase(t, &auditScript{enabled: true})
	ctx := context.WithValue(context.Background(), specificationDisabledKey{}, "s")
	if db.DatabaseAuditSpecification("s").inSpecificationWindow(ctx) {
		t.Error("a server specification's window was taken for a database one's")
	}
}

func TestDroppingAnEnabledDatabaseSpecificationDisablesItFirst(t *testing.T) {
	db := auditDatabase(t, &auditScript{enabled: true})
	ctx, col := WithScript(context.Background())
	if err := db.DatabaseAuditSpecification("s").DropContext(ctx); err != nil {
		t.Fatalf("DropContext: %v", err)
	}
	want := []string{
		use + "ALTER DATABASE AUDIT SPECIFICATION [s] WITH ( STATE = OFF )",
		use + "DROP DATABASE AUDIT SPECIFICATION [s]",
	}
	if !slices.Equal(col.Statements, want) {
		t.Errorf("statements = %v, want %v", col.Statements, want)
	}
}

func TestChangingAMissingDatabaseSpecificationIsNotFound(t *testing.T) {
	db := auditDatabase(t, &auditScript{missing: true})
	ctx, col := WithScript(context.Background())
	err := db.DatabaseAuditSpecification("gone").AddActionsContext(ctx, []string{"SCHEMA_OBJECT_ACCESS_GROUP"}, nil)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("got %v, want a not-found error", err)
	}
	if len(col.Statements) != 0 {
		t.Errorf("a refused change still built %v", col.Statements)
	}
}

// No groups and no actions is not an error and must not produce a bare ALTER
// with an empty clause list, which does not parse.
func TestChangingNoDatabaseActionsWritesNothing(t *testing.T) {
	db := auditDatabase(t, &auditScript{enabled: true})
	ctx, col := WithScript(context.Background())
	if err := db.DatabaseAuditSpecification("s").AddActionsContext(ctx, nil, nil); err != nil {
		t.Fatalf("AddActionsContext: %v", err)
	}
	if len(col.Statements) != 0 {
		t.Errorf("an empty change wrote %v", col.Statements)
	}
}

func TestDatabaseAuditSpecificationScriptShapes(t *testing.T) {
	spec := &DatabaseAuditSpecification{
		Name: "sp'ec", AuditName: "aud'it", IsEnabled: true,
		ActionGroups: []string{"SCHEMA_OBJECT_ACCESS_GROUP"},
		Actions: []DatabaseAuditAction{
			{ActionName: "SELECT", ClassDesc: "OBJECT", SchemaName: "dbo", ObjectName: "T", Principal: "public"},
		},
	}
	for _, tc := range []struct {
		name string
		opts ScriptOptions
		want []string
		deny []string
	}{
		{name: "drop", opts: ScriptOptions{Verb: ScriptDrop},
			want: []string{"IF EXISTS (SELECT 1 FROM sys.database_audit_specifications WHERE name = N'sp''ec')",
				"ALTER DATABASE AUDIT SPECIFICATION [sp'ec] WITH ( STATE = OFF );",
				"DROP DATABASE AUDIT SPECIFICATION [sp'ec];"},
			deny: []string{"CREATE DATABASE AUDIT SPECIFICATION"}},
		{name: "create", opts: ScriptOptions{Verb: ScriptCreate},
			want: []string{"CREATE DATABASE AUDIT SPECIFICATION [sp'ec]", "FOR SERVER AUDIT [aud'it]",
				"ADD (SCHEMA_OBJECT_ACCESS_GROUP)", "ADD (SELECT ON OBJECT::[dbo].[T] BY [public])",
				"WITH ( STATE = ON )"},
			deny: []string{"DROP DATABASE AUDIT SPECIFICATION"}},
		{name: "if not exists", opts: ScriptOptions{Verb: ScriptCreate, IncludeIfNotExists: true},
			want: []string{"IF NOT EXISTS (SELECT 1 FROM sys.database_audit_specifications WHERE name = N'sp''ec')"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := buildDatabaseAuditSpecificationScript(spec, tc.opts)
			if err != nil {
				t.Fatalf("buildDatabaseAuditSpecificationScript: %v", err)
			}
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

// An orphaned specification cannot be scripted: FOR SERVER AUDIT would come
// out empty and the script would not parse.
func TestScriptingAnOrphanedDatabaseSpecificationFails(t *testing.T) {
	spec := &DatabaseAuditSpecification{Name: "s"}
	if got, err := buildDatabaseAuditSpecificationScript(spec, ScriptOptions{Verb: ScriptCreate}); err == nil {
		t.Fatalf("want an error, got %q", got)
	}
}
