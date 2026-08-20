package gosmo

import (
	"context"
	"testing"
)

// Sequence.RestartContext mirrors the new value onto the handle, so it has to
// honour WithScript: a scripted restart is recorded, not run, and the server
// still hands out values from where it was. Mirroring anyway left the handle
// claiming a current value nothing on the server had ever produced.
func TestScriptedSequenceRestartDoesNotMoveCurrentValue(t *testing.T) {
	seq := &Sequence{db: scriptTestDB(), Schema: "Sales.Archive", Name: "o'brien", CurrentValue: 42}

	ctx, script := WithScript(context.Background())
	if err := seq.RestartContext(ctx, 1000); err != nil {
		t.Fatalf("scripted restart: %v", err)
	}
	if seq.CurrentValue != 42 {
		t.Errorf("CurrentValue = %d after a scripted restart, want 42 — the "+
			"statement was only captured, so the server is still at 42",
			seq.CurrentValue)
	}
	want := scriptUsePrefix + "ALTER SEQUENCE [Sales.Archive].[o'brien] RESTART WITH 1000"
	if len(script.Statements) != 1 || script.Statements[0] != want {
		t.Errorf("captured %q, want %q", script.Statements, want)
	}
}
