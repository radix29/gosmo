package gosmo

import (
	"context"
	"strings"
	"testing"
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
