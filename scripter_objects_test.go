package gosmo

import (
	"strings"
	"testing"
)

func TestBuildIndexScriptConstraintBackedIndexIsAConstraint(t *testing.T) {
	opts := DefaultScriptOptions()
	pk := &Index{AllowRowLocks: true, AllowPageLocks: true, Name: "PK_T", IsPrimaryKey: true, IsUnique: true, IsClustered: true,
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
	idx := &Index{AllowRowLocks: true, AllowPageLocks: true, Name: "IX_T_a", KeyColumns: []IndexColumn{{Name: "a"}}}
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

func TestBuildSequenceScriptStartsAfterTheLastUsedValue(t *testing.T) {
	// Three NEXT VALUE FORs on a sequence starting at 1 leave current_value
	// and last_used_value at 3. Starting the copy at 3 re-issues it — a
	// duplicate key on the first insert — and restarting at StartValue
	// re-issues all three.
	seq := &Sequence{Schema: "dbo", Name: "S", DataType: DataTypeBigInt,
		StartValue: "1", CurrentValue: "3", LastUsedValue: "3", Increment: "1", MinValue: "1", MaxValue: "9999"}
	got := buildSequenceScript(seq, DefaultScriptOptions())
	if !strings.Contains(got, "START WITH 4\n") {
		t.Errorf("sequence not scripted to start after its last used value:\n%s", got)
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

func TestSequenceStartWith(t *testing.T) {
	const big38 = "99999999999999999999999999999999999999"
	for _, tc := range []struct {
		name          string
		seq           Sequence
		want          string
		wantExhausted bool
	}{
		{"never used starts at start_value",
			Sequence{StartValue: "5", CurrentValue: "5", Increment: "2", MinValue: "1", MaxValue: "100"}, "5", false},
		{"restarted starts at the restart value",
			Sequence{StartValue: "50", CurrentValue: "50", Increment: "1", MinValue: "1", MaxValue: "100"}, "50", false},
		{"used starts one increment on",
			Sequence{StartValue: "1", CurrentValue: "3", LastUsedValue: "3", Increment: "1", MinValue: "1", MaxValue: "100"}, "4", false},
		{"negative increment",
			Sequence{StartValue: "0", CurrentValue: "-10", LastUsedValue: "-10", Increment: "-5", MinValue: "-100", MaxValue: "0"}, "-15", false},
		{"past int64",
			Sequence{StartValue: "100000000000000000000", CurrentValue: "100000000000000000000", LastUsedValue: "100000000000000000000",
				Increment: "10", MinValue: "-" + big38, MaxValue: big38}, "100000000000000000010", false},
		{"cycling wraps to MINVALUE",
			Sequence{StartValue: "5", CurrentValue: "9", LastUsedValue: "9", Increment: "2", MinValue: "-7", MaxValue: "9", IsCycling: true}, "-7", false},
		{"cycling negative wraps to MAXVALUE",
			Sequence{StartValue: "5", CurrentValue: "1", LastUsedValue: "1", Increment: "-1", MinValue: "1", MaxValue: "9", IsCycling: true}, "9", false},
		{"non-cycling at the end is exhausted",
			Sequence{StartValue: "5", CurrentValue: "9", LastUsedValue: "9", Increment: "2", MinValue: "1", MaxValue: "9"}, "9", true},
		{"pre-2017 falls back to current_value",
			Sequence{StartValue: "1", CurrentValue: "3", Increment: "1", MinValue: "1", MaxValue: "100", noLastUsed: true}, "4", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, exhausted := sequenceStartWith(&tc.seq)
			if got != tc.want || exhausted != tc.wantExhausted {
				t.Errorf("sequenceStartWith = %s, %v; want %s, %v", got, exhausted, tc.want, tc.wantExhausted)
			}
		})
	}
}

func TestBuildSequenceScriptExhaustedSaysSo(t *testing.T) {
	seq := &Sequence{Schema: "dbo", Name: "S", DataType: DataTypeInt,
		StartValue: "1", CurrentValue: "9", LastUsedValue: "9", Increment: "1", MinValue: "1", MaxValue: "9"}
	got := buildSequenceScript(seq, DefaultScriptOptions())
	if !strings.Contains(got, "-- This sequence is exhausted") || !strings.Contains(got, "START WITH 9\n") {
		t.Errorf("exhausted sequence not flagged:\n%s", got)
	}
}

func TestBuildSequenceScriptCarriesDecimalPrecision(t *testing.T) {
	// The bare numeric type means numeric(18,0), so a numeric(12,0)
	// sequence came back wider than it went in.
	seq := &Sequence{Schema: "dbo", Name: "S", DataType: DataTypeNumeric, DataTypeSchema: "sys",
		Precision: 12, Scale: 0, StartValue: "1", CurrentValue: "1", Increment: "1",
		MinValue: "-999999999999", MaxValue: "999999999999"}
	got := buildSequenceScript(seq, DefaultScriptOptions())
	if !strings.Contains(got, "AS [numeric](12,0)\n") {
		t.Errorf("numeric precision not scripted:\n%s", got)
	}
	seq.DataType, seq.Precision = DataTypeInt, 10
	got = buildSequenceScript(seq, DefaultScriptOptions())
	if !strings.Contains(got, "AS [int]\n") {
		t.Errorf("int must not carry a precision:\n%s", got)
	}
}

func TestBuildSequenceScriptQualifiesAnAliasDataType(t *testing.T) {
	// A sequence over a user-defined alias type scripts the type's name
	// alone unless its schema is carried too — and an unqualified alias
	// resolves against whoever runs the script, not whoever owns it.
	seq := &Sequence{Schema: "dbo", Name: "S", DataType: "bigid", DataTypeSchema: "app",
		StartValue: "1", CurrentValue: "1", Increment: "1", MinValue: "1", MaxValue: "9999"}
	got := buildSequenceScript(seq, DefaultScriptOptions())
	if !strings.Contains(got, "AS [app].[bigid]") {
		t.Errorf("alias data type not schema-qualified:\n%s", got)
	}

	builtin := &Sequence{Schema: "dbo", Name: "S", DataType: DataTypeBigInt, DataTypeSchema: "sys",
		StartValue: "1", CurrentValue: "1", Increment: "1", MinValue: "1", MaxValue: "9999"}
	got = buildSequenceScript(builtin, DefaultScriptOptions())
	if !strings.Contains(got, "AS [bigint]") || strings.Contains(got, "[sys].[bigint]") {
		t.Errorf("built-in data type must stay unqualified:\n%s", got)
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

func TestBuildStatisticScriptKeepsFilterAndOptions(t *testing.T) {
	st := &Statistic{Name: "st_a", IsUserCreated: true, HasFilter: true, FilterDef: "([a]>(0))",
		NoRecompute: true, IsIncremental: true}
	opts := ScriptOptions{Verb: ScriptDropAndCreate}
	got, err := buildStatisticScript(st, []string{"a", "b"}, "[dbo].[T]", opts)
	if err != nil {
		t.Fatal(err)
	}
	want := "IF EXISTS (SELECT 1 FROM sys.stats WHERE name = N'st_a' AND object_id = OBJECT_ID(N'[dbo].[T]'))\n" +
		"    DROP STATISTICS [dbo].[T].[st_a];\nGO\n\n" +
		"CREATE STATISTICS [st_a] ON [dbo].[T] ([a], [b]) WHERE ([a]>(0)) WITH NORECOMPUTE, INCREMENTAL = ON;\nGO\n"
	if got != want {
		t.Errorf("buildStatisticScript =\n%s\nwant\n%s", got, want)
	}

	opts = ScriptOptions{Verb: ScriptDrop}
	got, err = buildStatisticScript(st, nil, "[dbo].[T]", opts)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got, "CREATE") {
		t.Errorf("DROP script carries a CREATE:\n%s", got)
	}
}
