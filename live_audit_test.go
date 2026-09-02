//go:build livedb

// Live verification of the audit family: that the catalog reads back the
// columns audit.go and audit_specification.go scan, that the off/apply/on
// dance really is required and really works, and that the generated scripts
// run as generated.
//
//	go test -tags livedb . -run 'TestLive.*Audit' -v \
//	  -livedb 'sqlserver://sa:PASS@host?TrustServerCertificate=true'
//
// Everything this test creates it drops. The audit writes into auditDir, which
// must exist on the server host.
package gosmo

import (
	"context"
	"database/sql"
	"slices"
	"strings"
	"testing"
)

const auditDir = `C:\gossms_audit`

// dropAudit removes a specification and its audit, ignoring what is not there.
// Order matters: the specification references the audit.
// dropAudit takes an empty spec name for an audit that has no specification.
func dropAudit(t *testing.T, db *sql.DB, ctx context.Context, audit, spec string) {
	t.Helper()
	stmts := []string{
		"IF EXISTS (SELECT 1 FROM sys.server_audit_specifications WHERE name = N'" + spec + "')\n" +
			"BEGIN ALTER SERVER AUDIT SPECIFICATION [" + spec + "] WITH (STATE = OFF);" +
			" DROP SERVER AUDIT SPECIFICATION [" + spec + "]; END",
		"IF EXISTS (SELECT 1 FROM sys.server_audits WHERE name = N'" + audit + "')\n" +
			"BEGIN ALTER SERVER AUDIT [" + audit + "] WITH (STATE = OFF);" +
			" DROP SERVER AUDIT [" + audit + "]; END",
	}
	if spec == "" {
		stmts = stmts[1:]
	}
	for _, stmt := range stmts {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Errorf("cleanup: %v", err)
		}
	}
}

func TestLiveServerAuditLifecycle(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()
	s, err := NewServer(ctx, db)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	const name = "gossms_live_audit"
	const specName = "gossms_live_audit_spec"
	dropAudit(t, db, ctx, name, specName)
	defer dropAudit(t, db, ctx, name, specName)

	a, err := s.CreateServerAuditContext(ctx, ServerAuditSpec{
		Name: name, Type: AuditToFile, FilePath: auditDir,
		MaxFileSize: 10, MaxRolloverFiles: 3, QueueDelay: 1000,
		OnFailure: AuditFailureContinue,
		Predicate: "server_principal_name <> N'nobody'",
	})
	if err != nil {
		t.Fatalf("CreateServerAuditContext: %v", err)
	}
	if a.Type != AuditToFile || a.MaxFileSize != 10 || a.MaxRolloverFiles != 3 {
		t.Errorf("read back %+v", a)
	}
	if !strings.Contains(a.LogFilePath, "gossms_audit") || a.LogFileName == "" {
		t.Errorf("file target read back as path %q name %q", a.LogFilePath, a.LogFileName)
	}
	if a.Predicate == "" {
		t.Error("predicate read back empty")
	}
	if a.IsEnabled {
		t.Error("a new audit is created disabled")
	}

	if err := a.SetStateContext(ctx, true); err != nil {
		t.Fatalf("SetStateContext(on): %v", err)
	}
	st, err := a.StatusContext(ctx)
	if err != nil {
		t.Fatalf("StatusContext: %v", err)
	}
	if st.Status != "STARTED" || st.AuditFilePath == "" {
		t.Errorf("status = %+v", st)
	}

	// The point of the whole exercise: an ALTER on an enabled audit is
	// refused by the server, so AlterContext has to disable it and put it
	// back. Doing it on the enabled audit is what proves the dance.
	if err := a.AlterContext(ctx, ServerAuditSpec{
		Name: name, Type: AuditToFile, FilePath: auditDir,
		MaxFileSize: 20, MaxRolloverFiles: 5, QueueDelay: 2000,
		OnFailure: AuditFailureContinue,
		Predicate: "server_principal_name <> N'nobody'",
	}); err != nil {
		t.Fatalf("AlterContext on an enabled audit: %v", err)
	}
	again, err := s.ServerAuditByNameContext(ctx, name)
	if err != nil {
		t.Fatalf("ServerAuditByNameContext: %v", err)
	}
	if again.QueueDelay != 2000 || again.MaxFileSize != 20 || again.MaxRolloverFiles != 5 {
		t.Errorf("alter did not land: %+v", again)
	}
	if !again.IsEnabled {
		t.Error("the audit was left disabled after an alter")
	}

	// Clearing the predicate is the case a unit test cannot settle: REMOVE
	// WHERE combined with WITH(...) is a syntax error the server only reports
	// at execution, and every statement-shape test passed while a Properties
	// page Apply failed on it.
	if err := again.AlterContext(ctx, ServerAuditSpec{
		Name: name, Type: AuditToFile, FilePath: auditDir, QueueDelay: 2000,
		OnFailure: AuditFailureContinue,
	}); err != nil {
		t.Fatalf("AlterContext clearing the predicate: %v", err)
	}
	cleared, err := s.ServerAuditByNameContext(ctx, name)
	if err != nil {
		t.Fatalf("ServerAuditByNameContext: %v", err)
	}
	if cleared.Predicate != "" {
		t.Errorf("the predicate survived REMOVE WHERE: %q", cleared.Predicate)
	}
	if !cleared.IsEnabled {
		t.Error("the audit was left disabled after clearing the predicate")
	}

	// The specification half, on the enabled audit.
	spec, err := s.CreateServerAuditSpecificationContext(ctx, ServerAuditSpecificationSpec{
		Name: specName, AuditName: name,
		ActionGroups: []string{"BACKUP_RESTORE_GROUP", "LOGIN_CHANGE_PASSWORD_GROUP"},
		Enabled:      true,
	})
	if err != nil {
		t.Fatalf("CreateServerAuditSpecificationContext: %v", err)
	}
	if spec.AuditName != name || !spec.IsEnabled {
		t.Errorf("specification read back %+v", spec)
	}
	want := []string{"BACKUP_RESTORE_GROUP", "LOGIN_CHANGE_PASSWORD_GROUP"}
	if !slices.Equal(spec.ActionGroups, want) {
		t.Errorf("action groups = %v, want %v", spec.ActionGroups, want)
	}

	// Same dance, on the specification: the server refuses this while it is
	// enabled.
	if err := spec.AddActionGroupsContext(ctx, "DATABASE_CHANGE_GROUP"); err != nil {
		t.Fatalf("AddActionGroupsContext on an enabled specification: %v", err)
	}
	if err := spec.DropActionGroupsContext(ctx, "BACKUP_RESTORE_GROUP"); err != nil {
		t.Fatalf("DropActionGroupsContext: %v", err)
	}
	spec, err = s.ServerAuditSpecificationByNameContext(ctx, specName)
	if err != nil {
		t.Fatalf("ServerAuditSpecificationByNameContext: %v", err)
	}
	want = []string{"DATABASE_CHANGE_GROUP", "LOGIN_CHANGE_PASSWORD_GROUP"}
	if !slices.Equal(spec.ActionGroups, want) {
		t.Errorf("action groups = %v, want %v", spec.ActionGroups, want)
	}
	if !spec.IsEnabled {
		t.Error("the specification was left disabled after a change")
	}

	// The action-group pick list must contain what was just used.
	groups, err := s.AuditActionGroupsContext(ctx)
	if err != nil {
		t.Fatalf("AuditActionGroupsContext: %v", err)
	}
	for _, g := range want {
		if !slices.Contains(groups, g) {
			t.Errorf("%q missing from the pick list of %d groups", g, len(groups))
		}
	}

	// Both drops must work on enabled objects.
	if err := spec.DropContext(ctx); err != nil {
		t.Fatalf("DropContext(specification): %v", err)
	}
	if err := again.DropContext(ctx); err != nil {
		t.Fatalf("DropContext(audit): %v", err)
	}
	if _, err := s.ServerAuditByNameContext(ctx, name); err == nil {
		t.Error("the audit survived its drop")
	}
}

// TestLiveAuditScriptsRunAsGenerated is the check a statement-shape unit test
// cannot make: that what the scripter emits is accepted by the server.
func TestLiveAuditScriptsRunAsGenerated(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()
	s, err := NewServer(ctx, db)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	const name = "gossms_live_audit_s"
	const specName = "gossms_live_audit_s_spec"
	dropAudit(t, db, ctx, name, specName)
	defer dropAudit(t, db, ctx, name, specName)

	a, err := s.CreateServerAuditContext(ctx, ServerAuditSpec{
		Name: name, Type: AuditToFile, FilePath: auditDir, QueueDelay: 1000,
		OnFailure: AuditFailureContinue, Predicate: "server_principal_name <> N'nobody'",
	})
	if err != nil {
		t.Fatalf("CreateServerAuditContext: %v", err)
	}
	if err := a.SetStateContext(ctx, true); err != nil {
		t.Fatalf("SetStateContext: %v", err)
	}
	if _, err := s.CreateServerAuditSpecificationContext(ctx, ServerAuditSpecificationSpec{
		Name: specName, AuditName: name,
		ActionGroups: []string{"BACKUP_RESTORE_GROUP"}, Enabled: true,
	}); err != nil {
		t.Fatalf("CreateServerAuditSpecificationContext: %v", err)
	}

	sc := NewServerScripter(s, ScriptOptions{Verb: ScriptDropAndCreate})
	specScript, err := sc.ScriptServerAuditSpecificationContext(ctx, specName)
	if err != nil {
		t.Fatalf("ScriptServerAuditSpecificationContext: %v", err)
	}
	auditScriptText, err := sc.ScriptServerAuditContext(ctx, name)
	if err != nil {
		t.Fatalf("ScriptServerAuditContext: %v", err)
	}

	// The specification is dropped and recreated first, then the audit — an
	// audit cannot be dropped out from under nothing, but a specification
	// bound to it must go first for the audit's own drop half to be reachable.
	for _, script := range []string{specScript, auditScriptText, specScript} {
		for _, batch := range strings.Split(script, "\nGO\n") {
			if strings.TrimSpace(batch) == "" {
				continue
			}
			if _, err := db.ExecContext(ctx, batch); err != nil {
				t.Fatalf("generated batch failed: %v\n%s", err, batch)
			}
		}
	}

	back, err := s.ServerAuditByNameContext(ctx, name)
	if err != nil {
		t.Fatalf("the scripted audit is not there: %v", err)
	}
	if !back.IsEnabled {
		t.Error("the script recreated the audit disabled")
	}
	if back.Predicate == "" {
		t.Error("the script dropped the predicate")
	}
	backSpec, err := s.ServerAuditSpecificationByNameContext(ctx, specName)
	if err != nil {
		t.Fatalf("the scripted specification is not there: %v", err)
	}
	if !backSpec.IsEnabled || !slices.Contains(backSpec.ActionGroups, "BACKUP_RESTORE_GROUP") {
		t.Errorf("specification came back as %+v", backSpec)
	}
}

// TestLiveRenamingAnEnabledAudit is the acceptance for the 2026-09-02 review's
// §1. The restore ran under the receiver's name, which MODIFY NAME had just
// invalidated: the rename committed, the re-enable failed against a name the
// server no longer had, and RenameContext returned an error with auditing left
// switched off — the exact failure the disable/restore dance exists to prevent.
//
// The application log target is used rather than a file so the test needs no
// directory on the server host.
func TestLiveRenamingAnEnabledAudit(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()
	s, err := NewServer(ctx, db)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	const from = "gossms_live_rename_from"
	const to = "gossms_live_rename_to"
	cleanup := func() {
		dropAudit(t, db, ctx, from, "")
		dropAudit(t, db, ctx, to, "")
	}
	cleanup()
	defer cleanup()

	a, err := s.CreateServerAuditContext(ctx, ServerAuditSpec{
		Name: from, Type: AuditToApplicationLog,
		QueueDelay: 1000, OnFailure: AuditFailureContinue,
	})
	if err != nil {
		t.Fatalf("CreateServerAuditContext: %v", err)
	}
	if err := a.SetStateContext(ctx, true); err != nil {
		t.Fatalf("SetStateContext(on): %v", err)
	}

	if err := a.RenameContext(ctx, to); err != nil {
		t.Fatalf("RenameContext on an enabled audit: %v", err)
	}
	if a.Name != to {
		t.Errorf("receiver name = %q, want %q", a.Name, to)
	}

	renamed, err := s.ServerAuditByNameContext(ctx, to)
	if err != nil {
		t.Fatalf("ServerAuditByNameContext(%q): %v", to, err)
	}
	if !renamed.IsEnabled {
		t.Error("the audit was left disabled after the rename")
	}
	if _, err := s.ServerAuditByNameContext(ctx, from); err == nil {
		t.Errorf("the old name %q still resolves", from)
	}
}
