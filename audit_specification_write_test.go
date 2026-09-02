package gosmo

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
)

func TestServerAuditSpecificationStateStatements(t *testing.T) {
	for _, tc := range []struct {
		on   bool
		want string
	}{
		{true, "ALTER SERVER AUDIT SPECIFICATION [odd]]name] WITH ( STATE = ON )"},
		{false, "ALTER SERVER AUDIT SPECIFICATION [odd]]name] WITH ( STATE = OFF )"},
	} {
		ctx, col := WithScript(context.Background())
		spec := &ServerAuditSpecification{server: &Server{}, Name: "odd]name"}
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

func TestCreateServerAuditSpecificationStatement(t *testing.T) {
	for _, tc := range []struct {
		name string
		spec ServerAuditSpecificationSpec
		want []string
		bad  bool
	}{
		{name: "two groups enabled",
			spec: ServerAuditSpecificationSpec{Name: "s]p", AuditName: "a]ud",
				ActionGroups: []string{"BACKUP_RESTORE_GROUP", "DATABASE_CHANGE_GROUP"}, Enabled: true},
			want: []string{"CREATE SERVER AUDIT SPECIFICATION [s]]p]", "FOR SERVER AUDIT [a]]ud]",
				"ADD (BACKUP_RESTORE_GROUP)", "ADD (DATABASE_CHANGE_GROUP)", "WITH ( STATE = ON )"}},
		{name: "no groups disabled",
			spec: ServerAuditSpecificationSpec{Name: "s", AuditName: "a"},
			want: []string{"WITH ( STATE = OFF )"}},
		{name: "no name", spec: ServerAuditSpecificationSpec{AuditName: "a"}, bad: true},
		{name: "no audit", spec: ServerAuditSpecificationSpec{Name: "s"}, bad: true},
		// A group name is a keyword inside ADD (...), not an identifier: it
		// cannot be bracket-quoted, so anything that is not a bare name has to
		// be refused rather than interpolated.
		{name: "injected group",
			spec: ServerAuditSpecificationSpec{Name: "s", AuditName: "a",
				ActionGroups: []string{"X), ADD (Y"}}, bad: true},
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

// Same rule as the audit: the server refuses every change while the
// specification is enabled, and a disabled one must not be switched on by an
// edit.
func TestChangingASpecificationTurnsItOffAndBackOn(t *testing.T) {
	for _, tc := range []struct {
		name    string
		enabled bool
		act     func(*ServerAuditSpecification, context.Context) error
		want    []string
	}{
		{"add groups while enabled", true,
			func(s *ServerAuditSpecification, ctx context.Context) error {
				return s.AddActionGroupsContext(ctx, "BACKUP_RESTORE_GROUP")
			},
			[]string{
				"ALTER SERVER AUDIT SPECIFICATION [s] WITH ( STATE = OFF )",
				"ALTER SERVER AUDIT SPECIFICATION [s]\n    ADD (BACKUP_RESTORE_GROUP)",
				"ALTER SERVER AUDIT SPECIFICATION [s] WITH ( STATE = ON )",
			}},
		{"drop groups while disabled", false,
			func(s *ServerAuditSpecification, ctx context.Context) error {
				return s.DropActionGroupsContext(ctx, "BACKUP_RESTORE_GROUP", "DATABASE_CHANGE_GROUP")
			},
			[]string{"ALTER SERVER AUDIT SPECIFICATION [s]\n    DROP (BACKUP_RESTORE_GROUP),\n    DROP (DATABASE_CHANGE_GROUP)"}},
		{"reparent while enabled", true,
			func(s *ServerAuditSpecification, ctx context.Context) error {
				return s.SetAuditContext(ctx, "other")
			},
			[]string{
				"ALTER SERVER AUDIT SPECIFICATION [s] WITH ( STATE = OFF )",
				"ALTER SERVER AUDIT SPECIFICATION [s]\nFOR SERVER AUDIT [other]",
				"ALTER SERVER AUDIT SPECIFICATION [s] WITH ( STATE = ON )",
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := auditServer(t, &auditScript{enabled: tc.enabled})
			ctx, col := WithScript(context.Background())
			if err := tc.act(srv.ServerAuditSpecification("s"), ctx); err != nil {
				t.Fatalf("act: %v", err)
			}
			if !slices.Equal(col.Statements, tc.want) {
				t.Errorf("statements =\n%#v\nwant\n%#v", col.Statements, tc.want)
			}
		})
	}
}

func TestDroppingAnEnabledSpecificationDisablesItFirst(t *testing.T) {
	srv := auditServer(t, &auditScript{enabled: true})
	ctx, col := WithScript(context.Background())
	if err := srv.ServerAuditSpecification("s").DropContext(ctx); err != nil {
		t.Fatalf("DropContext: %v", err)
	}
	want := []string{
		"ALTER SERVER AUDIT SPECIFICATION [s] WITH ( STATE = OFF )",
		"DROP SERVER AUDIT SPECIFICATION [s]",
	}
	if !slices.Equal(col.Statements, want) {
		t.Errorf("statements = %v, want %v", col.Statements, want)
	}
}

func TestChangingAMissingSpecificationIsNotFound(t *testing.T) {
	srv := auditServer(t, &auditScript{missing: true})
	ctx, col := WithScript(context.Background())
	err := srv.ServerAuditSpecification("gone").AddActionGroupsContext(ctx, "BACKUP_RESTORE_GROUP")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("got %v, want a not-found error", err)
	}
	if len(col.Statements) != 0 {
		t.Errorf("a refused change still built %v", col.Statements)
	}
}

// No groups is not an error and must not produce a bare ALTER with an empty
// clause list, which does not parse.
func TestChangingNoActionGroupsWritesNothing(t *testing.T) {
	srv := auditServer(t, &auditScript{enabled: true})
	ctx, col := WithScript(context.Background())
	if err := srv.ServerAuditSpecification("s").AddActionGroupsContext(ctx); err != nil {
		t.Fatalf("AddActionGroupsContext: %v", err)
	}
	if len(col.Statements) != 0 {
		t.Errorf("an empty change wrote %v", col.Statements)
	}
}

func TestServerAuditSpecificationScriptShapes(t *testing.T) {
	spec := &ServerAuditSpecification{
		Name: "sp'ec", AuditName: "aud'it", IsEnabled: true,
		ActionGroups: []string{"BACKUP_RESTORE_GROUP"},
	}
	for _, tc := range []struct {
		name string
		opts ScriptOptions
		want []string
		deny []string
	}{
		{name: "drop", opts: ScriptOptions{Verb: ScriptDrop},
			want: []string{"IF EXISTS (SELECT 1 FROM sys.server_audit_specifications WHERE name = N'sp''ec')",
				"ALTER SERVER AUDIT SPECIFICATION [sp'ec] WITH ( STATE = OFF );",
				"DROP SERVER AUDIT SPECIFICATION [sp'ec];"},
			deny: []string{"CREATE SERVER AUDIT SPECIFICATION"}},
		{name: "create", opts: ScriptOptions{Verb: ScriptCreate},
			want: []string{"CREATE SERVER AUDIT SPECIFICATION [sp'ec]", "FOR SERVER AUDIT [aud'it]",
				"ADD (BACKUP_RESTORE_GROUP)", "WITH ( STATE = ON )"},
			deny: []string{"DROP SERVER AUDIT SPECIFICATION"}},
		{name: "if not exists", opts: ScriptOptions{Verb: ScriptCreate, IncludeIfNotExists: true},
			want: []string{"IF NOT EXISTS (SELECT 1 FROM sys.server_audit_specifications WHERE name = N'sp''ec')"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := buildServerAuditSpecificationScript(spec, tc.opts)
			if err != nil {
				t.Fatalf("buildServerAuditSpecificationScript: %v", err)
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

// Dropping an audit a specification still references succeeds and orphans the
// specification — verified live. Scripting one would emit FOR SERVER AUDIT
// with nothing after it, which does not parse, so it is refused instead.
func TestScriptingAnOrphanedSpecificationIsRefused(t *testing.T) {
	spec := &ServerAuditSpecification{Name: "sp", ActionGroups: []string{"BACKUP_RESTORE_GROUP"}}
	if _, err := buildServerAuditSpecificationScript(spec, ScriptOptions{Verb: ScriptCreate}); err == nil {
		t.Error("an orphaned specification scripted without error")
	}
	// The drop half needs no audit and must still work.
	if _, err := buildServerAuditSpecificationScript(spec, ScriptOptions{Verb: ScriptDrop}); err != nil {
		t.Errorf("drop-only script refused: %v", err)
	}
}
