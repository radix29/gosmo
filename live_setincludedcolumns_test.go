//go:build livedb

// Live verification of Index.SetIncludedColumns against the S4 probe: an
// index with every option DROP_EXISTING resets, on a filegroup that is not
// the table's. The hand-built statement this replaced came back with fill
// factor 0, pad off, page locks on, NORECOMPUTE off, IGNORE_DUP_KEY off and
// on PRIMARY — none of which a unit test of the statement text can prove
// the server honours.
//
//	go test -tags livedb . -run TestLiveSetIncludedColumns -v \
//	  -livedb 'sqlserver://sa:PASS@host?TrustServerCertificate=true'
//
// Creates and drops its own throwaway database; touches nothing else.
package gosmo

import (
	"slices"
	"strings"
	"testing"
)

// indexOptions is the part of an Index SetIncludedColumns must not change.
type indexOptions struct {
	Type                                   IndexType
	Unique, Padded, IgnoreDup, NoRecompute bool
	RowLocks, PageLocks, SeqKey, Disabled  bool
	FillFactor                             int
	Compression, Filter, DataSpace         string
	Keys                                   string
}

func optionsOf(idx *Index) indexOptions {
	return indexOptions{
		Type: idx.Type, Unique: idx.IsUnique, Padded: idx.IsPadded, IgnoreDup: idx.IgnoreDupKey,
		NoRecompute: idx.StatisticsNoRecompute, RowLocks: idx.AllowRowLocks, PageLocks: idx.AllowPageLocks,
		SeqKey: idx.OptimizeForSequentialKey, Disabled: idx.IsDisabled, FillFactor: idx.FillFactor,
		Compression: string(idx.DataCompression), Filter: idx.FilterDefinition, DataSpace: idx.DataSpace.Name,
		Keys: indexColumnList(idx.KeyColumns),
	}
}

func includedNames(idx *Index) []string {
	var out []string
	for _, c := range idx.IncludedColumns {
		out = append(out, c.Name)
	}
	return out
}

func TestLiveSetIncludedColumnsKeepsEveryOption(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()

	d, drop := liveScratchDB(t, db, ctx, "gosmo_setincluded_live")
	defer drop()

	// OPTIMIZE_FOR_SEQUENTIAL_KEY is 2019+; on an older instance the index
	// is created without it and the read must report it off.
	seqKey := d.serverMajorVersion() == 0 || d.serverMajorVersion() >= int(SQLServer2019)
	with := "FILLFACTOR = 70, PAD_INDEX = ON, ALLOW_PAGE_LOCKS = OFF, STATISTICS_NORECOMPUTE = ON, " +
		"IGNORE_DUP_KEY = ON, DATA_COMPRESSION = PAGE"
	if seqKey {
		with += ", OPTIMIZE_FOR_SEQUENTIAL_KEY = ON"
	}
	liveExecIn(t, d, ctx,
		`ALTER DATABASE [`+d.Name+`] ADD FILEGROUP FG2`,
		`ALTER DATABASE [`+d.Name+`] ADD FILE (NAME = N'`+d.Name+`_fg2', FILENAME = N'`+
			liveDatabaseFileDir(t, d, ctx)+d.Name+`_fg2.ndf', SIZE = 8MB) TO FILEGROUP FG2`,
		`CREATE TABLE dbo.T (ID INT NOT NULL CONSTRAINT PK_T PRIMARY KEY CLUSTERED,
		    A INT NOT NULL, B INT NULL, C INT NULL, D NVARCHAR(20) NULL)`,
		`CREATE UNIQUE NONCLUSTERED INDEX IX_probe ON dbo.T (A, B DESC) INCLUDE (C)
		    WITH (`+with+`) ON FG2`,
	)

	tbl, err := d.TableByName(ctx, "dbo", "T")
	if err != nil {
		t.Fatalf("TableByName: %v", err)
	}
	read := func() *Index {
		t.Helper()
		idx, err := tbl.IndexByName(ctx, "IX_probe")
		if err != nil {
			t.Fatalf("IndexByName: %v", err)
		}
		return idx
	}

	before := read()
	// No filter: SQL Server refuses IGNORE_DUP_KEY on a filtered index
	// (10618), and the filter is pinned by the unit tests.
	// The read path first: an option read as its default would make the
	// before/after comparison below vacuous for it.
	if !before.IsPadded || before.FillFactor != 70 || before.AllowPageLocks || !before.StatisticsNoRecompute ||
		!before.IgnoreDupKey || before.DataCompression != "PAGE" || before.DataSpace.Name != "FG2" ||
		before.OptimizeForSequentialKey != seqKey {
		t.Fatalf("index read back as %+v — the options were not read, so nothing below is tested", optionsOf(before))
	}

	t.Run("options and filegroup survive", func(t *testing.T) {
		if err := before.SetIncludedColumns(ctx, []string{"C", "D"}); err != nil {
			t.Fatalf("SetIncludedColumns: %v", err)
		}
		after := read()
		if optionsOf(after) != optionsOf(before) {
			t.Errorf("options changed:\nbefore %+v\nafter  %+v", optionsOf(before), optionsOf(after))
		}
		if got := includedNames(after); !slices.Equal(got, []string{"C", "D"}) {
			t.Errorf("included = %v, want [C D]", got)
		}
	})

	t.Run("a disabled index stays disabled", func(t *testing.T) {
		liveExecIn(t, d, ctx, `ALTER INDEX IX_probe ON dbo.T DISABLE`)
		idx := read()
		if err := idx.SetIncludedColumns(ctx, []string{"C"}); err != nil {
			t.Fatalf("SetIncludedColumns: %v", err)
		}
		after := read()
		if !after.IsDisabled {
			t.Error("the rebuild left the index enabled")
		}
		if got := includedNames(after); !slices.Equal(got, []string{"C"}) {
			t.Errorf("included = %v, want [C]", got)
		}
		liveExecIn(t, d, ctx, `ALTER INDEX IX_probe ON dbo.T REBUILD`)
	})

	t.Run("script as create round-trips the options", func(t *testing.T) {
		src := read()
		script := strings.TrimSuffix(scriptIndex(src, tbl.FullName(), ScriptOptions{}), ";\nGO\n\n")
		liveExecIn(t, d, ctx, `DROP INDEX IX_probe ON dbo.T`, script)
		if got := read(); optionsOf(got) != optionsOf(src) {
			t.Errorf("replayed script changed the index:\nscript %s\nbefore %+v\nafter  %+v", script, optionsOf(src), optionsOf(got))
		}
	})

	t.Run("a constraint-backed index is refused before the server", func(t *testing.T) {
		pk, err := tbl.IndexByName(ctx, "PK_T")
		if err != nil {
			t.Fatalf("IndexByName PK_T: %v", err)
		}
		err = pk.SetIncludedColumns(ctx, []string{"C"})
		if err == nil || !strings.Contains(err.Error(), "not supported") {
			t.Errorf("SetIncludedColumns on PK_T = %v, want a refusal", err)
		}
	})
}

// A filtered nonclustered columnstore index scripted without its WHERE is
// recreated over every row. Replayed here so the server, not the test,
// decides whether the scripted filter is valid DDL.
func TestLiveScriptFilteredColumnstoreKeepsItsFilter(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()

	d, drop := liveScratchDB(t, db, ctx, "gosmo_ncci_filter_live")
	defer drop()

	liveExecIn(t, d, ctx,
		`CREATE TABLE dbo.F (ID INT NOT NULL PRIMARY KEY, A INT NULL, B INT NULL)`,
		`CREATE NONCLUSTERED COLUMNSTORE INDEX NCCI_F ON dbo.F (A, B) WHERE A > 10`,
	)
	tbl, err := d.TableByName(ctx, "dbo", "F")
	if err != nil {
		t.Fatalf("TableByName: %v", err)
	}
	idx, err := tbl.IndexByName(ctx, "NCCI_F")
	if err != nil {
		t.Fatalf("IndexByName: %v", err)
	}
	script := strings.TrimSuffix(scriptIndex(idx, tbl.FullName(), ScriptOptions{}), ";\nGO\n\n")
	if !strings.Contains(script, "WHERE") {
		t.Fatalf("script has no filter:\n%s", script)
	}
	liveExecIn(t, d, ctx, `DROP INDEX NCCI_F ON dbo.F`, script)
	again, err := tbl.IndexByName(ctx, "NCCI_F")
	if err != nil {
		t.Fatalf("IndexByName after replay: %v", err)
	}
	if again.FilterDefinition != idx.FilterDefinition {
		t.Errorf("filter after replay = %q, want %q", again.FilterDefinition, idx.FilterDefinition)
	}
}
