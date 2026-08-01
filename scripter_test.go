package gosmo

import (
	"strings"
	"testing"
)

func TestColumnTypeString(t *testing.T) {
	cases := []struct {
		name string
		col  *Column
		want string
	}{
		{"varchar with length", &Column{DataType: DataTypeVarChar, MaxLength: 50}, "varchar(50)"},
		{"varchar MAX", &Column{DataType: DataTypeVarChar, MaxLength: -1}, "varchar(MAX)"},
		{"varchar zero length", &Column{DataType: DataTypeVarChar, MaxLength: 0}, "varchar"},
		// nvarchar/nchar store max_length in bytes (2 per char) in sys.columns.
		{"nvarchar halves byte length", &Column{DataType: DataTypeNVarChar, MaxLength: 100}, "nvarchar(50)"},
		{"nvarchar MAX", &Column{DataType: DataTypeNVarChar, MaxLength: -1}, "nvarchar(MAX)"},
		{"nchar halves byte length", &Column{DataType: DataTypeNChar, MaxLength: 20}, "nchar(10)"},
		{"decimal with precision", &Column{DataType: DataTypeDecimal, Precision: 10, Scale: 4}, "decimal(10,4)"},
		{"decimal no precision", &Column{DataType: DataTypeDecimal}, "decimal"},
		{"time with scale", &Column{DataType: DataTypeTime, Scale: 7}, "time(7)"},
		{"time no scale", &Column{DataType: DataTypeTime}, "time"},
		{"plain bigint", &Column{DataType: DataTypeBigInt}, "bigint"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ColumnTypeString(c.col); got != c.want {
				t.Errorf("ColumnTypeString(%+v) = %q, want %q", c.col, got, c.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// buildTableScript — the assembly ScriptTableContext hands its catalog reads
// to. Kept separate from those reads precisely so this can be asserted
// without a server.
// ---------------------------------------------------------------------------

// scriptTestTable is a small table with a PK, a unique constraint, a
// filtered nonclustered index and a foreign key — enough shape to exercise
// every branch buildTableScript has.
func scriptTestTable() (cols []*Column, indexes []*Index, fks []*ForeignKey) {
	cols = []*Column{
		{Name: "ID", DataType: DataTypeInt, IsIdentity: true, IdentitySeed: 1, IdentityIncrement: 1},
		{Name: "Code", DataType: DataTypeNVarChar, MaxLength: 40},
		{Name: "OwnerID", DataType: DataTypeInt, IsNullable: true},
	}
	indexes = []*Index{
		{Name: "PK_Widget", IsPrimaryKey: true, IsClustered: true, IsUnique: true,
			Type: IndexTypeClustered, KeyColumns: []IndexColumn{{Name: "ID"}}},
		{Name: "UQ_Widget_Code", IsUniqueConstraint: true, IsUnique: true,
			Type: IndexTypeNonClustered, KeyColumns: []IndexColumn{{Name: "Code"}}},
		{Name: "IX_Widget_Owner", Type: IndexTypeNonClustered,
			KeyColumns:       []IndexColumn{{Name: "OwnerID", Descending: true}},
			IncludedColumns:  []IndexColumn{{Name: "Code"}},
			FilterDefinition: "([OwnerID] IS NOT NULL)"},
	}
	fks = []*ForeignKey{
		{Name: "FK_Widget_Owner", Columns: []string{"OwnerID"},
			ReferencedSchema: "dbo", ReferencedTable: "Owner",
			ReferencedColumns: []string{"ID"}, DeleteAction: "SET_NULL"},
	}
	return cols, indexes, fks
}

// countBeginEnd counts BEGIN/END keywords appearing as whole lines, and GO
// batch separators, in each batch. Used to pin the invariant below.
func splitBatches(script string) []string {
	var batches []string
	var cur []string
	for _, line := range strings.Split(script, "\n") {
		if strings.TrimSpace(line) == "GO" {
			batches = append(batches, strings.Join(cur, "\n"))
			cur = nil
			continue
		}
		cur = append(cur, line)
	}
	return append(batches, strings.Join(cur, "\n"))
}

// TestBuildTableScriptKeepsBlocksInsideOneBatch is the regression test for
// the shipped bug: with IncludeIfNotExists the CREATE/index/FK statements
// were wrapped in a single IF ... BEGIN ... END whose body contained GO
// separators. GO is a client-side batch break, so batch one had an unclosed
// BEGIN and the last batch was a bare END — the whole script failed to
// parse. Every batch must balance its own BEGIN/END.
func TestBuildTableScriptKeepsBlocksInsideOneBatch(t *testing.T) {
	cols, indexes, fks := scriptTestTable()
	script := buildTableScript("dbo", "Widget", "AppDB", cols, indexes, fks, DefaultScriptOptions())

	for i, batch := range splitBatches(script) {
		begins, ends := 0, 0
		for _, line := range strings.Split(batch, "\n") {
			switch strings.TrimSpace(line) {
			case "BEGIN":
				begins++
			case "END":
				ends++
			}
		}
		if begins != ends {
			t.Errorf("batch %d has %d BEGIN and %d END — unbalanced across a GO:\n%s", i, begins, ends, batch)
		}
	}
	if strings.Contains(script, "\nEND\nGO") {
		t.Errorf("script ends a block in its own batch:\n%s", script)
	}
}

// TestBuildTableScriptGuardsEachStatementSeparately pins that the existence
// check applies per statement rather than wrapping the whole script — the
// shape that replaced the single BEGIN block.
func TestBuildTableScriptGuardsEachStatementSeparately(t *testing.T) {
	cols, indexes, fks := scriptTestTable()
	script := buildTableScript("dbo", "Widget", "AppDB", cols, indexes, fks, DefaultScriptOptions())

	for _, want := range []string{
		"IF OBJECT_ID(N'[dbo].[Widget]', N'U') IS NULL\nCREATE TABLE [dbo].[Widget] (",
		"IF NOT EXISTS (SELECT 1 FROM sys.indexes WHERE name = N'IX_Widget_Owner' AND object_id = OBJECT_ID(N'[dbo].[Widget]'))",
		"ALTER TABLE [dbo].[Widget]\n    ADD CONSTRAINT [UQ_Widget_Code] UNIQUE NONCLUSTERED ([Code] ASC);",
		"CONSTRAINT [PK_Widget] PRIMARY KEY CLUSTERED ([ID] ASC)",
		"[ID] int IDENTITY(1,1) NOT NULL,",
		"ON DELETE SET NULL",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("script missing %q:\n%s", want, script)
		}
	}
	// The unique constraint must not also be emitted as a CREATE INDEX.
	if strings.Contains(script, "INDEX [UQ_Widget_Code]") {
		t.Errorf("unique constraint scripted as an index:\n%s", script)
	}
}

// TestScriptIndexByType pins that the index type decides the grammar. The
// B-tree form was previously used for every type, with type_desc pasted in
// as the keyword — which emits DDL SQL Server rejects for columnstore (no
// ASC/DESC; clustered takes no column list) and for XML/spatial (a different
// grammar entirely).
func TestScriptIndexByType(t *testing.T) {
	plain := ScriptOptions{}
	cases := []struct {
		name      string
		idx       *Index
		want      string
		notWanted []string
	}{
		{
			name: "clustered columnstore takes no column list",
			idx: &Index{Name: "CCI", Type: IndexTypeClusteredColumnStore,
				KeyColumns: []IndexColumn{{Name: "A"}}},
			want:      "CREATE CLUSTERED COLUMNSTORE INDEX [CCI] ON [dbo].[T];",
			notWanted: []string{"ASC", "([A]"},
		},
		{
			name: "nonclustered columnstore takes columns without a direction",
			idx: &Index{Name: "NCCI", Type: IndexTypeColumnStore,
				KeyColumns: []IndexColumn{{Name: "A"}, {Name: "B"}}},
			want:      "CREATE NONCLUSTERED COLUMNSTORE INDEX [NCCI]\n    ON [dbo].[T] ([A], [B]);",
			notWanted: []string{"ASC", "DESC"},
		},
		{
			name:      "xml index is skipped with a note, not mis-scripted",
			idx:       &Index{Name: "XI", Type: IndexTypeXML, KeyColumns: []IndexColumn{{Name: "Doc"}}},
			want:      "-- XML index [XI] on [dbo].[T] is not scripted",
			notWanted: []string{"CREATE XML INDEX", "CREATE  INDEX"},
		},
		{
			name:      "spatial index is skipped with a note",
			idx:       &Index{Name: "SI", Type: IndexTypeSpatial, KeyColumns: []IndexColumn{{Name: "Geo"}}},
			want:      "-- SPATIAL index [SI] on [dbo].[T] is not scripted",
			notWanted: []string{"CREATE SPATIAL INDEX"},
		},
		{
			name: "ordinary nonclustered keeps the b-tree form",
			idx: &Index{Name: "IX", Type: IndexTypeNonClustered, IsUnique: true,
				KeyColumns: []IndexColumn{{Name: "A", Descending: true}}},
			want:      "CREATE UNIQUE NONCLUSTERED INDEX [IX]\n    ON [dbo].[T] ([A] DESC);",
			notWanted: nil,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := scriptIndex(c.idx, "[dbo].[T]", plain)
			if !strings.Contains(got, c.want) {
				t.Errorf("scriptIndex() = %q, want it to contain %q", got, c.want)
			}
			for _, nw := range c.notWanted {
				if strings.Contains(got, nw) {
					t.Errorf("scriptIndex() = %q, must not contain %q", got, nw)
				}
			}
		})
	}
}

// TestBuildTableScriptDrops pins that the DROP path is unaffected — it has
// no block to break, and its guard is a single-statement IF.
func TestBuildTableScriptDrops(t *testing.T) {
	cols, indexes, fks := scriptTestTable()
	opts := DefaultScriptOptions()
	opts.ScriptDrops = true
	got := buildTableScript("dbo", "Widget", "AppDB", cols, indexes, fks, opts)
	want := "IF OBJECT_ID(N'[dbo].[Widget]', N'U') IS NOT NULL\n    DROP TABLE [dbo].[Widget];\nGO\n"
	if got != want {
		t.Errorf("buildTableScript(drop) = %q, want %q", got, want)
	}
}
