package gosmo

import (
	"strings"
	"testing"
)

func TestBuildIndexScriptConstraintBackedIndexIsAConstraint(t *testing.T) {
	opts := DefaultScriptOptions()
	pk := &Index{Name: "PK_T", IsPrimaryKey: true, IsUnique: true, IsClustered: true,
		KeyColumns: []IndexColumn{{Name: "id"}}}

	got := buildIndexScript(pk, "[dbo].[T]", opts)
	if !strings.Contains(got, "ADD CONSTRAINT [PK_T] PRIMARY KEY CLUSTERED ([id] ASC)") {
		t.Errorf("primary key not scripted as a constraint — CREATE INDEX cannot recreate one:\n%s", got)
	}
	if strings.Contains(got, "CREATE ") && strings.Contains(got, "INDEX [PK_T]") {
		t.Errorf("primary key scripted as CREATE INDEX:\n%s", got)
	}

	opts.Verb = ScriptDrop
	if got := buildIndexScript(pk, "[dbo].[T]", opts); !strings.Contains(got, "ALTER TABLE [dbo].[T] DROP CONSTRAINT IF EXISTS [PK_T]") {
		t.Errorf("dropping a key-backed index must drop the constraint:\n%s", got)
	}
}

func TestBuildIndexScriptOrdinaryIndex(t *testing.T) {
	idx := &Index{Name: "IX_T_a", KeyColumns: []IndexColumn{{Name: "a"}}}
	opts := DefaultScriptOptions()

	got := buildIndexScript(idx, "[dbo].[T]", opts)
	if !strings.Contains(got, "CREATE NONCLUSTERED INDEX [IX_T_a]") {
		t.Errorf("index not scripted as CREATE INDEX:\n%s", got)
	}

	opts.Verb = ScriptDrop
	if got := buildIndexScript(idx, "[dbo].[T]", opts); !strings.Contains(got, "DROP INDEX IF EXISTS [IX_T_a] ON [dbo].[T]") {
		t.Errorf("index drop wrong:\n%s", got)
	}

	opts.Verb = ScriptDropAndCreate
	got = buildIndexScript(idx, "[dbo].[T]", opts)
	if drop, create := strings.Index(got, "DROP INDEX"), strings.Index(got, "CREATE NONCLUSTERED"); drop < 0 || create < 0 || drop > create {
		t.Errorf("DROP-and-CREATE out of order or incomplete:\n%s", got)
	}
}

func TestBuildCheckConstraintScriptKeepsADisabledConstraintDisabled(t *testing.T) {
	ck := &CheckConstraint{Name: "CK_T_a", Definition: "([a]>(0))", IsDisabled: true}
	got := buildCheckConstraintScript(ck, "[dbo].[T]", DefaultScriptOptions())

	if !strings.Contains(got, "WITH NOCHECK") {
		t.Errorf("a disabled constraint must be added WITH NOCHECK, or the script fails on rows it was disabled for:\n%s", got)
	}
	if !strings.Contains(got, "NOCHECK CONSTRAINT [CK_T_a]") {
		t.Errorf("a disabled constraint must be left disabled after it is added:\n%s", got)
	}

	enabled := &CheckConstraint{Name: "CK_T_a", Definition: "([a]>(0))"}
	got = buildCheckConstraintScript(enabled, "[dbo].[T]", DefaultScriptOptions())
	if strings.Contains(got, "NOCHECK") {
		t.Errorf("an enabled constraint must not be scripted with NOCHECK:\n%s", got)
	}
}

func TestBuildSequenceScriptStartsAtTheCurrentValue(t *testing.T) {
	seq := &Sequence{Schema: "dbo", Name: "S", DataType: DataTypeBigInt,
		StartValue: 1, CurrentValue: 4321, Increment: 1, MinValue: 1, MaxValue: 9999}
	got := buildSequenceScript(seq, DefaultScriptOptions())

	// Restarting at StartValue would hand out numbers the original sequence
	// has already given away.
	if !strings.Contains(got, "START WITH 4321") {
		t.Errorf("sequence scripted from its start value rather than its current one:\n%s", got)
	}
	if !strings.Contains(got, "NO CYCLE") || !strings.Contains(got, "NO CACHE") {
		t.Errorf("non-cycling, non-cached sequence not scripted as such:\n%s", got)
	}

	seq.IsCached, seq.CacheSize, seq.IsCycling = true, 50, true
	got = buildSequenceScript(seq, DefaultScriptOptions())
	if !strings.Contains(got, "CACHE 50") || strings.Contains(got, "NO CYCLE") {
		t.Errorf("cached, cycling sequence not scripted as such:\n%s", got)
	}
}

func TestBuildSynonymScript(t *testing.T) {
	syn := &Synonym{Schema: "dbo", Name: "S", BaseObject: "[other].[dbo].[T]"}
	got := buildSynonymScript(syn, DefaultScriptOptions())
	if !strings.Contains(got, "CREATE SYNONYM [dbo].[S] FOR [other].[dbo].[T]") {
		t.Errorf("synonym script wrong:\n%s", got)
	}

	opts := DefaultScriptOptions()
	opts.Verb = ScriptDrop
	if got := buildSynonymScript(syn, opts); !strings.Contains(got, "DROP SYNONYM IF EXISTS [dbo].[S]") {
		t.Errorf("synonym drop wrong:\n%s", got)
	}
}
