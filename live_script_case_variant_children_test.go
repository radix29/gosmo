//go:build livedb

// Live verification of H1 and H2 (gossms docs/review-plan-2026-10-04.md): in
// a case-sensitive database a table's children may differ only in case, and
// the Scripter scripts the one named exactly; a table whose LOB filegroup
// differs from its data filegroup only in case keeps its TEXTIMAGE_ON.
//
//	go test -tags livedb . -run TestLiveScriptCaseVariantTableChildren -v \
//	  -livedb 'sqlserver://sa:PASS@host?TrustServerCertificate=true'
//
// Creates and drops its own throwaway database; touches nothing else.
package gosmo

import (
	"errors"
	"strings"
	"testing"
)

func TestLiveScriptCaseVariantTableChildren(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()

	const name = "gosmo_h1_cs"
	d, drop := liveScratchDBCollated(t, db, ctx, name, "Latin1_General_100_CS_AS")
	defer drop()

	// Two filegroups differing only in case, each with a file in the
	// instance's default data directory.
	for _, fg := range []string{"fg", "FG"} {
		file := name + "_" + map[string]string{"fg": "lower", "FG": "upper"}[fg]
		liveExecIn(t, d, ctx,
			"ALTER DATABASE ["+name+"] ADD FILEGROUP ["+fg+"]",
			"DECLARE @p nvarchar(400) = CAST(SERVERPROPERTY('InstanceDefaultDataPath') AS nvarchar(400));\n"+
				"EXEC (N'ALTER DATABASE ["+name+"] ADD FILE (NAME = N''"+file+"'', FILENAME = N''' + @p + N'"+file+".ndf'') TO FILEGROUP ["+fg+"]')")
	}
	liveExecIn(t, d, ctx,
		`CREATE TABLE dbo.P (id int NOT NULL PRIMARY KEY)`,
		`CREATE TABLE dbo.T (id int NOT NULL PRIMARY KEY, colA int, colB int, colP int)`,
		`CREATE INDEX [IX_a] ON dbo.T (colA)`,
		`CREATE INDEX [ix_A] ON dbo.T (colB)`,
		`ALTER TABLE dbo.T ADD CONSTRAINT [CK_x] CHECK (colA > 111)`,
		`ALTER TABLE dbo.T ADD CONSTRAINT [ck_X] CHECK (colB > 222)`,
		`ALTER TABLE dbo.T ADD CONSTRAINT [FK_p] FOREIGN KEY (colP) REFERENCES dbo.P (id)`,
		`ALTER TABLE dbo.T ADD CONSTRAINT [fk_P] FOREIGN KEY (colA) REFERENCES dbo.P (id)`,
		`CREATE TABLE dbo.L (id int NOT NULL, notes nvarchar(max)) ON [fg] TEXTIMAGE_ON [FG]`,
	)

	sc := NewScripter(d, ScriptOptions{})
	for _, c := range []struct {
		kind   string
		script func(string) (string, error)
		names  [2]string
		marks  [2]string // marks[i] appears in names[i]'s script only
	}{
		{"index", func(n string) (string, error) { return sc.ScriptIndex(ctx, "dbo", "T", n) },
			[2]string{"IX_a", "ix_A"}, [2]string{"[colA]", "[colB]"}},
		{"check constraint", func(n string) (string, error) { return sc.ScriptCheckConstraint(ctx, "dbo", "T", n) },
			[2]string{"CK_x", "ck_X"}, [2]string{"111", "222"}},
		{"foreign key", func(n string) (string, error) { return sc.ScriptForeignKey(ctx, "dbo", "T", n) },
			[2]string{"FK_p", "fk_P"}, [2]string{"[colP]", "[colA]"}},
	} {
		for i, n := range c.names {
			got, err := c.script(n)
			if err != nil {
				t.Errorf("%s %s: %v", c.kind, n, err)
				continue
			}
			if !strings.Contains(got, "["+n+"]") || !strings.Contains(got, c.marks[i]) || strings.Contains(got, c.marks[1-i]) {
				t.Errorf("%s %s scripted the wrong object:\n%s", c.kind, n, got)
			}
		}
		if _, err := c.script(strings.ToLower(c.names[0])); !errors.Is(err, ErrAmbiguous) {
			t.Errorf("%s %s (matching both only case-blindly): err %v, want ErrAmbiguous", c.kind, strings.ToLower(c.names[0]), err)
		}
	}

	got, err := sc.ScriptTable(ctx, "dbo", "L")
	if err != nil {
		t.Fatalf("ScriptTable L: %v", err)
	}
	if !strings.Contains(got, "ON [fg] TEXTIMAGE_ON [FG]") {
		t.Errorf("table on [fg] with LOB data on [FG] lost its TEXTIMAGE_ON:\n%s", got)
	}
}
