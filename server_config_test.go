package gosmo

import (
	"context"
	"strings"
	"testing"
)

// TestWithScriptApplyConfigurationBatchShape pins ApplyConfiguration's batch:
// "show advanced options" turned on (conditionally, by the server's own
// is_advanced) before any change, every change, one RECONFIGURE, and the
// restore — in that order, as one captured statement. A bare sp_configure of
// an advanced option fails Msg 15123 on a stock server, which is what this
// shape exists to avoid.
func TestWithScriptApplyConfigurationBatchShape(t *testing.T) {
	s := &Server{}
	ctx, script := WithScript(context.Background())
	err := s.ApplyConfiguration(ctx, []ConfigChange{
		{Name: "max degree of parallelism", Value: 4},
		{Name: "cost threshold for parallelism", Value: 50},
	}, ConfigApplyOptions{})
	if err != nil {
		t.Fatalf("ApplyConfiguration under WithScript: %v", err)
	}
	stmts := script.Statements()
	if len(stmts) != 1 {
		t.Fatalf("captured %d statements, want 1 batch: %q", len(stmts), stmts)
	}
	got := stmts[0]
	inOrder(t, got,
		"DECLARE @show_advanced_enabled bit = 0;",
		"name = N'show advanced options' AND value_in_use = 0",
		"is_advanced = 1 AND name IN (N'max degree of parallelism', N'cost threshold for parallelism')",
		"EXEC sys.sp_configure N'show advanced options', 1;",
		"RECONFIGURE;",
		"SET @show_advanced_enabled = 1;",
		"EXEC sys.sp_configure N'max degree of parallelism', 4;",
		"EXEC sys.sp_configure N'cost threshold for parallelism', 50;",
		"RECONFIGURE;",
		"IF @show_advanced_enabled = 1",
		"EXEC sys.sp_configure N'show advanced options', 0;",
		"RECONFIGURE;",
	)
	if strings.Contains(got, "OVERRIDE") {
		t.Errorf("Override false, but the batch says OVERRIDE:\n%s", got)
	}
}

// TestWithScriptApplyConfigurationOverride: every RECONFIGURE in the batch
// takes the option, not only the one after the changes.
func TestWithScriptApplyConfigurationOverride(t *testing.T) {
	ctx, script := WithScript(context.Background())
	if err := (&Server{}).ApplyConfiguration(ctx, []ConfigChange{{Name: "recovery interval (min)", Value: 70}},
		ConfigApplyOptions{Override: true}); err != nil {
		t.Fatal(err)
	}
	got := script.Statements()[0]
	if n := strings.Count(got, "RECONFIGURE WITH OVERRIDE;"); n != 3 {
		t.Errorf("%d RECONFIGURE WITH OVERRIDE, want 3:\n%s", n, got)
	}
	if n := strings.Count(got, "RECONFIGURE"); n != 3 {
		t.Errorf("%d RECONFIGURE in all, want 3:\n%s", n, got)
	}
}

// TestWithScriptApplyConfigurationShowAdvancedWins: a change to "show advanced
// options" itself is applied after every other change, and nothing restores
// it afterwards — the user's value is final.
func TestWithScriptApplyConfigurationShowAdvancedWins(t *testing.T) {
	ctx, script := WithScript(context.Background())
	if err := (&Server{}).ApplyConfiguration(ctx, []ConfigChange{
		{Name: "Show Advanced Options", Value: 0},
		{Name: "max server memory (MB)", Value: 4096},
	}, ConfigApplyOptions{}); err != nil {
		t.Fatal(err)
	}
	got := script.Statements()[0]
	inOrder(t, got,
		"EXEC sys.sp_configure N'show advanced options', 1;",
		"EXEC sys.sp_configure N'max server memory (MB)', 4096;",
		"EXEC sys.sp_configure N'Show Advanced Options', 0;",
		"RECONFIGURE;",
	)
	if strings.Contains(got, "IF @show_advanced_enabled = 1") {
		t.Errorf("a set changing show advanced options must not restore it:\n%s", got)
	}
	if !strings.Contains(got, "is_advanced = 1 AND name IN (N'max server memory (MB)'))") {
		t.Errorf("show advanced options must not count toward the advanced check:\n%s", got)
	}
}

// TestWithScriptApplyConfigurationOnlyShowAdvanced: nothing else changes, so
// there is no enable/restore scaffolding — just the change.
func TestWithScriptApplyConfigurationOnlyShowAdvanced(t *testing.T) {
	ctx, script := WithScript(context.Background())
	if err := (&Server{}).ApplyConfiguration(ctx, []ConfigChange{{Name: "show advanced options", Value: 1}},
		ConfigApplyOptions{}); err != nil {
		t.Fatal(err)
	}
	want := "EXEC sys.sp_configure N'show advanced options', 1;\nRECONFIGURE;"
	if got := script.Statements(); len(got) != 1 || got[0] != want {
		t.Errorf("got %q, want [%q]", got, want)
	}
}

// TestApplyConfigurationEscapesNames: a name reaches the batch in two
// places, each inside N'…'.
func TestApplyConfigurationEscapesNames(t *testing.T) {
	got := buildApplyConfiguration([]ConfigChange{{Name: "it's", Value: 1}}, ConfigApplyOptions{})
	if strings.Count(got, "N'it''s'") != 2 || strings.Contains(got, "N'it's'") {
		t.Errorf("name not escaped everywhere:\n%s", got)
	}
}

func TestApplyConfigurationEmptyIsNoOp(t *testing.T) {
	ctx, script := WithScript(context.Background())
	if err := (&Server{}).ApplyConfiguration(ctx, nil, ConfigApplyOptions{}); err != nil {
		t.Fatal(err)
	}
	if got := script.Statements(); len(got) != 0 {
		t.Errorf("empty changes captured %q", got)
	}
}

// inOrder fails unless each of parts occurs in s after the previous one.
func inOrder(t *testing.T, s string, parts ...string) {
	t.Helper()
	rest := s
	for _, p := range parts {
		i := strings.Index(rest, p)
		if i < 0 {
			t.Fatalf("%q missing (or out of order) in:\n%s", p, s)
		}
		rest = rest[i+len(p):]
	}
}
