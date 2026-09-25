package gosmo

import (
	"slices"
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
		// A zero scale is datetime2(0)/time(0), not the bare type, which means 7.
		{"time zero scale", &Column{DataType: DataTypeTime}, "time(0)"},
		{"datetime2 zero scale", &Column{DataType: DataTypeDatetime2}, "datetime2(0)"},
		{"datetimeoffset zero scale", &Column{DataType: DataTypeDatetimeOffset}, "datetimeoffset(0)"},
		{"alias type is qualified, no length", &Column{DataType: "Phone", TypeSchema: "app", IsUserDefinedType: true, MaxLength: 20}, "[app].[Phone]"},
		{"plain bigint", &Column{DataType: DataTypeBigInt}, "bigint"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.col.TypeString(); got != c.want {
				t.Errorf("Column.TypeString(%+v) = %q, want %q", c.col, got, c.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// buildTableScript — the assembly ScriptTable hands its catalog reads
// to. Kept separate from those reads precisely so this can be asserted
// without a server.
// ---------------------------------------------------------------------------

// scriptTestTable is a small table with a PK, a unique constraint, a
// filtered nonclustered index and a foreign key — enough shape to exercise
// every branch buildTableScript has.
func scriptTestTable() (cols []*Column, indexes []*Index, fks []*ForeignKey) {
	cols = []*Column{
		{Name: "ID", DataType: DataTypeInt, IsIdentity: true, IdentitySeed: "1", IdentityIncrement: "1"},
		{Name: "Code", DataType: DataTypeNVarChar, MaxLength: 40},
		{Name: "OwnerID", DataType: DataTypeInt, IsNullable: true},
	}
	indexes = []*Index{
		{AllowRowLocks: true, AllowPageLocks: true, Name: "PK_Widget", IsPrimaryKey: true, IsClustered: true, IsUnique: true,
			Type: IndexTypeClustered, KeyColumns: []IndexColumn{{Name: "ID"}}},
		{AllowRowLocks: true, AllowPageLocks: true, Name: "UQ_Widget_Code", IsUniqueConstraint: true, IsUnique: true,
			Type: IndexTypeNonClustered, KeyColumns: []IndexColumn{{Name: "Code"}}},
		{AllowRowLocks: true, AllowPageLocks: true, Name: "IX_Widget_Owner", Type: IndexTypeNonClustered,
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
	script := buildTableScript("dbo", "Widget", "AppDB", tableScriptParts{cols: cols, indexes: indexes, fks: fks, ds: DataSpace{Name: "PRIMARY", IsDefaultFileGroup: true}}, DefaultScriptOptions())

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
	script := buildTableScript("dbo", "Widget", "AppDB", tableScriptParts{cols: cols, indexes: indexes, fks: fks, ds: DataSpace{Name: "PRIMARY", IsDefaultFileGroup: true}}, DefaultScriptOptions())

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
			idx: &Index{AllowRowLocks: true, AllowPageLocks: true, Name: "CCI", Type: IndexTypeClusteredColumnStore,
				KeyColumns: []IndexColumn{{Name: "A"}}},
			want:      "CREATE CLUSTERED COLUMNSTORE INDEX [CCI] ON [dbo].[T];",
			notWanted: []string{"ASC", "([A]"},
		},
		{
			name: "nonclustered columnstore takes columns without a direction",
			idx: &Index{AllowRowLocks: true, AllowPageLocks: true, Name: "NCCI", Type: IndexTypeColumnStore,
				KeyColumns: []IndexColumn{{Name: "A"}, {Name: "B"}}},
			want:      "CREATE NONCLUSTERED COLUMNSTORE INDEX [NCCI]\n    ON [dbo].[T] ([A], [B]);",
			notWanted: []string{"ASC", "DESC"},
		},
		{
			// How Table.Indexes reads one: sys.index_columns marks every
			// NCCI column included, so KeyColumns is empty.
			name: "nonclustered columnstore as read lists its included columns",
			idx: &Index{AllowRowLocks: true, AllowPageLocks: true, Name: "NCCI", Type: IndexTypeColumnStore,
				IncludedColumns: []IndexColumn{{Name: "A", IsIncluded: true}, {Name: "B", IsIncluded: true}}},
			want:      "CREATE NONCLUSTERED COLUMNSTORE INDEX [NCCI]\n    ON [dbo].[T] ([A], [B]);",
			notWanted: []string{"()"},
		},
		{
			// Recreated without its WHERE, a filtered NCCI covers every row.
			name: "filtered nonclustered columnstore keeps its filter",
			idx: &Index{AllowRowLocks: true, AllowPageLocks: true, Name: "NCCI", Type: IndexTypeColumnStore,
				KeyColumns: []IndexColumn{{Name: "A"}}, FilterDefinition: "([A]>(0))"},
			want: "CREATE NONCLUSTERED COLUMNSTORE INDEX [NCCI]\n    ON [dbo].[T] ([A])\n    WHERE ([A]>(0));",
		},
		{
			// G5: recreated without it, rows compress into the columnstore
			// at once instead of after the delay.
			name: "clustered columnstore keeps its compression delay",
			idx: &Index{AllowRowLocks: true, AllowPageLocks: true, Name: "CCI", Type: IndexTypeClusteredColumnStore,
				CompressionDelay: 30},
			want: "CREATE CLUSTERED COLUMNSTORE INDEX [CCI] ON [dbo].[T] WITH (COMPRESSION_DELAY = 30 MINUTES);",
		},
		{
			name: "nonclustered columnstore keeps archive compression and delay together",
			idx: &Index{AllowRowLocks: true, AllowPageLocks: true, Name: "NCCI", Type: IndexTypeColumnStore,
				KeyColumns: []IndexColumn{{Name: "A"}}, DataCompression: "COLUMNSTORE_ARCHIVE", CompressionDelay: 10},
			want: "CREATE NONCLUSTERED COLUMNSTORE INDEX [NCCI]\n    ON [dbo].[T] ([A]) WITH (DATA_COMPRESSION = COLUMNSTORE_ARCHIVE, COMPRESSION_DELAY = 10 MINUTES);",
		},
		{
			name:      "xml index is skipped with a note, not mis-scripted",
			idx:       &Index{AllowRowLocks: true, AllowPageLocks: true, Name: "XI", Type: IndexTypeXML, KeyColumns: []IndexColumn{{Name: "Doc"}}},
			want:      "-- XML index [XI] on [dbo].[T] is not scripted",
			notWanted: []string{"CREATE XML INDEX", "CREATE  INDEX"},
		},
		{
			name:      "spatial index is skipped with a note",
			idx:       &Index{AllowRowLocks: true, AllowPageLocks: true, Name: "SI", Type: IndexTypeSpatial, KeyColumns: []IndexColumn{{Name: "Geo"}}},
			want:      "-- SPATIAL index [SI] on [dbo].[T] is not scripted",
			notWanted: []string{"CREATE SPATIAL INDEX"},
		},
		{
			name: "ordinary nonclustered keeps the b-tree form",
			idx: &Index{AllowRowLocks: true, AllowPageLocks: true, Name: "IX", Type: IndexTypeNonClustered, IsUnique: true,
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
	got := buildTableScript("dbo", "Widget", "AppDB", tableScriptParts{cols: cols, indexes: indexes, fks: fks, ds: DataSpace{Name: "PRIMARY", IsDefaultFileGroup: true}}, opts)
	want := "IF OBJECT_ID(N'[dbo].[Widget]', N'U') IS NOT NULL\n    DROP TABLE [dbo].[Widget];\nGO\n"
	if got != want {
		t.Errorf("buildTableScript(drop) = %q, want %q", got, want)
	}
}

// TestBuildTableScriptEmitsThePartitionScheme is the regression test for a
// partitioned table scripting as an unpartitioned one: with no ON clause the
// script recreates the table on the target database's default filegroup, and
// nothing — not the server, not the script — says so. Every object that has a
// partition scheme must name it, and name the partitioning column with it.
func TestBuildTableScriptEmitsThePartitionScheme(t *testing.T) {
	ps := DataSpace{Name: "ps_year", IsPartitionScheme: true, PartitionColumn: "Created"}
	cols, indexes, fks := scriptTestTable()
	for _, idx := range indexes {
		idx.DataSpace = ps
	}
	script := buildTableScript("dbo", "Widget", "AppDB", tableScriptParts{cols: cols, indexes: indexes, fks: fks, ds: ps}, DefaultScriptOptions())

	for _, want := range []string{
		") ON [ps_year]([Created]);",
		"CONSTRAINT [PK_Widget] PRIMARY KEY CLUSTERED ([ID] ASC) ON [ps_year]([Created])",
		"ADD CONSTRAINT [UQ_Widget_Code] UNIQUE NONCLUSTERED ([Code] ASC) ON [ps_year]([Created]);",
		"WHERE ([OwnerID] IS NOT NULL) ON [ps_year]([Created]);",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("script is missing %q:\n%s", want, script)
		}
	}
}

// TestBuildTableScriptEmitsANonDefaultFileGroup: a table on a filegroup other
// than the default one is the same silent relocation as the partition case,
// and the default filegroup is left unsaid because ON [PRIMARY] is what the
// server does anyway — naming a filegroup the target database may not have
// would break a script that otherwise runs.
func TestBuildTableScriptEmitsANonDefaultFileGroup(t *testing.T) {
	cols, indexes, fks := scriptTestTable()
	archive := DataSpace{Name: "FG_Archive"}
	script := buildTableScript("dbo", "Widget", "AppDB", tableScriptParts{cols: cols, indexes: indexes, fks: fks, ds: archive}, DefaultScriptOptions())
	if !strings.Contains(script, ") ON [FG_Archive];") {
		t.Errorf("script does not put the table on its filegroup:\n%s", script)
	}

	primary := DataSpace{Name: "PRIMARY", IsDefaultFileGroup: true}
	script = buildTableScript("dbo", "Widget", "AppDB", tableScriptParts{cols: cols, indexes: indexes, fks: fks, ds: primary}, DefaultScriptOptions())
	if strings.Contains(script, "ON [PRIMARY]") {
		t.Errorf("script names the default filegroup, which says nothing:\n%s", script)
	}
}

// TestDataSpaceClause covers what the two tests above do not reach: an index
// with no data space at all (a memory-optimized table's), and a partition
// scheme whose partitioning column did not come back — neither of which can
// be written as a clause, so neither may produce half of one.
func TestDataSpaceClause(t *testing.T) {
	cases := []struct {
		name string
		ds   DataSpace
		want string
	}{
		{"scheme", DataSpace{Name: "ps", IsPartitionScheme: true, PartitionColumn: "d"}, " ON [ps]([d])"},
		{"non-default filegroup", DataSpace{Name: "FG2"}, " ON [FG2]"},
		{"default filegroup", DataSpace{Name: "PRIMARY", IsDefaultFileGroup: true}, ""},
		{"no data space", DataSpace{}, ""},
		{"scheme with no column", DataSpace{Name: "ps", IsPartitionScheme: true}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := dataSpaceClause(c.ds); got != c.want {
				t.Errorf("dataSpaceClause(%+v) = %q, want %q", c.ds, got, c.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Table-script fidelity: each case is a table feature ScriptTable used to
// drop, so CREATE To recreated a different table without saying so.
// ---------------------------------------------------------------------------

func TestBuildTableScriptKeepsColumnFeatures(t *testing.T) {
	cols := []*Column{
		{Name: "ID", DataType: DataTypeInt, IsIdentity: true, IdentitySeed: "1", IdentityIncrement: "1", IdentityNotForReplication: true},
		{Name: "Stamp", DataType: DataTypeDatetime2, Scale: 0},
		{Name: "Guid", DataType: DataTypeUniqueIdentifier, IsRowGUID: true},
		{Name: "Total", DataType: DataTypeInt, IsComputed: true, ComputedText: "([ID]*(2))", IsPersisted: true},
		{Name: "Loose", DataType: DataTypeInt, IsComputed: true, ComputedText: "([ID]+(1))", IsNullable: true},
		{Name: "Phone", DataType: "Phone", TypeSchema: "app", IsUserDefinedType: true, MaxLength: 40, IsNullable: true},
		{Name: "Rare", DataType: DataTypeInt, IsSparse: true, IsNullable: true},
		{Name: "Props", DataType: DataTypeXML, IsColumnSet: true, IsNullable: true},
		{Name: "Email", DataType: DataTypeNVarChar, MaxLength: 200, IsNullable: true, MaskingFunction: `partial(1,"XXX",0)`},
		{Name: "CS", DataType: DataTypeVarChar, MaxLength: 10, IsNullable: true, Collation: "Latin1_General_CS_AS"},
		{Name: "Same", DataType: DataTypeVarChar, MaxLength: 10, IsNullable: true, Collation: "SQL_Latin1_General_CP1_CI_AS"},
	}
	p := tableScriptParts{cols: cols, table: tableScriptOptions{DatabaseCollation: "SQL_Latin1_General_CP1_CI_AS"}}
	got := buildTableScript("dbo", "T", "db", p, ScriptOptions{})
	for _, want := range []string{
		"[ID] int IDENTITY(1,1) NOT FOR REPLICATION NOT NULL",
		"[Stamp] datetime2(0) NOT NULL",
		"[Guid] uniqueidentifier ROWGUIDCOL NOT NULL",
		"[Total] AS ([ID]*(2)) PERSISTED NOT NULL",
		"[Loose] AS ([ID]+(1)),",
		"[Phone] [app].[Phone] NULL",
		"[Rare] int SPARSE NULL",
		"[Props] xml COLUMN_SET FOR ALL_SPARSE_COLUMNS,",
		`[Email] nvarchar(100) MASKED WITH (FUNCTION = 'partial(1,"XXX",0)') NULL`,
		"[CS] varchar(10) COLLATE Latin1_General_CS_AS NULL",
		"[Same] varchar(10) NULL",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("script is missing %q:\n%s", want, got)
		}
	}
}

func TestBuildTableScriptKeepsCheckConstraints(t *testing.T) {
	p := tableScriptParts{
		cols: []*Column{{Name: "a", DataType: DataTypeInt}},
		checks: []*CheckConstraint{
			{Name: "CK_on", Definition: "([a]>(0))"},
			{Name: "CK_off", Definition: "([a]<(9))", IsDisabled: true, IsNotTrusted: true},
			{Name: "CK_untrusted", Definition: "([a]<>(5))", IsNotTrusted: true},
			{Name: "CK_nfr", Definition: "([a]<>(6))", IsNotForReplication: true, IsNotTrusted: true},
		},
	}
	got := buildTableScript("dbo", "T", "db", p, ScriptOptions{})
	for _, want := range []string{
		"ALTER TABLE [dbo].[T] WITH CHECK\n    ADD CONSTRAINT [CK_on] CHECK ([a]>(0));",
		"ALTER TABLE [dbo].[T] WITH NOCHECK\n    ADD CONSTRAINT [CK_off] CHECK ([a]<(9));",
		"ALTER TABLE [dbo].[T] NOCHECK CONSTRAINT [CK_off];",
		"ALTER TABLE [dbo].[T] WITH NOCHECK\n    ADD CONSTRAINT [CK_untrusted] CHECK ([a]<>(5));",
		"ADD CONSTRAINT [CK_nfr] CHECK NOT FOR REPLICATION ([a]<>(6));",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("script is missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "NOCHECK CONSTRAINT [CK_untrusted]") {
		t.Errorf("an enabled untrusted constraint was disabled:\n%s", got)
	}
}

func TestBuildTableScriptTemporal(t *testing.T) {
	p := tableScriptParts{
		cols: []*Column{
			{Name: "ID", DataType: DataTypeInt},
			{Name: "ValidFrom", DataType: DataTypeDatetime2, Scale: 7, GeneratedAlwaysType: 1, IsHidden: true},
			{Name: "ValidTo", DataType: DataTypeDatetime2, Scale: 7, GeneratedAlwaysType: 2},
		},
		indexes: []*Index{{AllowRowLocks: true, AllowPageLocks: true, Name: "PK_T", IsPrimaryKey: true,
			IsClustered: true, KeyColumns: []IndexColumn{{Name: "ID"}}}},
		table: tableScriptOptions{SystemVersioned: true, HistorySchema: "hist", HistoryTable: "T_History",
			PeriodStart: "ValidFrom", PeriodEnd: "ValidTo"},
	}
	opts := ScriptOptions{Verb: ScriptDropAndCreate}
	got := buildTableScript("dbo", "T", "db", p, opts)
	for _, want := range []string{
		"[ValidFrom] datetime2(7) GENERATED ALWAYS AS ROW START HIDDEN NOT NULL,",
		"[ValidTo] datetime2(7) GENERATED ALWAYS AS ROW END NOT NULL,",
		"    PERIOD FOR SYSTEM_TIME ([ValidFrom], [ValidTo]),\n    CONSTRAINT [PK_T] PRIMARY KEY CLUSTERED",
		")\nWITH (SYSTEM_VERSIONING = ON (HISTORY_TABLE = [hist].[T_History]));",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("script is missing %q:\n%s", want, got)
		}
	}
	off := strings.Index(got, "SET (SYSTEM_VERSIONING = OFF)")
	drop := strings.Index(got, "DROP TABLE")
	if off < 0 || off > drop {
		t.Errorf("DROP TABLE of a system-versioned table needs versioning switched off first:\n%s", got)
	}
}

func TestBuildTableScriptIndexOptions(t *testing.T) {
	p := tableScriptParts{
		cols: []*Column{{Name: "a", DataType: DataTypeInt}, {Name: "b", DataType: DataTypeInt}},
		indexes: []*Index{
			{Name: "PK_T", IsPrimaryKey: true, IsClustered: true, AllowRowLocks: true, AllowPageLocks: false,
				OptimizeForSequentialKey: true,
				DataCompression:          "PAGE", KeyColumns: []IndexColumn{{Name: "a"}}},
			{Name: "IX_b", Type: IndexTypeNonClustered, IsUnique: true, IsPadded: true, FillFactor: 80,
				IgnoreDupKey: true, StatisticsNoRecompute: true, AllowRowLocks: true, AllowPageLocks: true, DataCompression: "ROW",
				IsDisabled: true, KeyColumns: []IndexColumn{{Name: "b"}}},
		},
		table: tableScriptOptions{HeapCompression: ""},
	}
	got := buildTableScript("dbo", "T", "db", p, ScriptOptions{})
	for _, want := range []string{
		"PRIMARY KEY CLUSTERED ([a] ASC) WITH (ALLOW_PAGE_LOCKS = OFF, OPTIMIZE_FOR_SEQUENTIAL_KEY = ON, DATA_COMPRESSION = PAGE)",
		"WITH (PAD_INDEX = ON, FILLFACTOR = 80, IGNORE_DUP_KEY = ON, STATISTICS_NORECOMPUTE = ON, DATA_COMPRESSION = ROW);",
		"ALTER INDEX [IX_b] ON [dbo].[T] DISABLE;",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("script is missing %q:\n%s", want, got)
		}
	}
	if strings.Index(got, "DISABLE;") < strings.Index(got, "CREATE UNIQUE NONCLUSTERED INDEX [IX_b]") {
		t.Errorf("an index must be disabled after it is created:\n%s", got)
	}

	heap := tableScriptParts{cols: p.cols, table: tableScriptOptions{HeapCompression: "PAGE"}}
	if got := buildTableScript("dbo", "T", "db", heap, ScriptOptions{}); !strings.Contains(got, ")\nWITH (DATA_COMPRESSION = PAGE);") {
		t.Errorf("a compressed heap lost its compression:\n%s", got)
	}
}

// TestCompressionOptionsPerPartition (G3): a partitioned heap or index whose
// partitions mix compressions scripts one DATA_COMPRESSION per value with
// its partitions, contiguous ones collapsed; a uniform one keeps the
// single-value form.
func TestCompressionOptionsPerPartition(t *testing.T) {
	const (
		N = DataCompressionNone
		R = DataCompressionRow
		P = DataCompressionPage
		A = DataCompressionColumnstoreArchive
		C = DataCompressionColumnstore
	)
	archive := func(c DataCompression) bool { return c == A }
	cases := []struct {
		name   string
		single DataCompression
		parts  []DataCompression
		named  func(DataCompression) bool
		want   []string
	}{
		{"uniform", P, []DataCompression{P, P, P}, rowstoreCompressed, []string{"DATA_COMPRESSION = PAGE"}},
		{"uniform none", N, []DataCompression{N, N}, rowstoreCompressed, nil},
		{"single partition", R, []DataCompression{R}, rowstoreCompressed, []string{"DATA_COMPRESSION = ROW"}},
		{"not read", P, nil, rowstoreCompressed, []string{"DATA_COMPRESSION = PAGE"}},
		{"mixed", P, []DataCompression{P, N, N}, rowstoreCompressed,
			[]string{"DATA_COMPRESSION = PAGE ON PARTITIONS (1)"}},
		{"range plus one", P, []DataCompression{P, P, P, N, P}, rowstoreCompressed,
			[]string{"DATA_COMPRESSION = PAGE ON PARTITIONS (1 TO 3, 5)"}},
		{"two compressions", N, []DataCompression{N, R, R, P}, rowstoreCompressed,
			[]string{"DATA_COMPRESSION = ROW ON PARTITIONS (2 TO 3)", "DATA_COMPRESSION = PAGE ON PARTITIONS (4)"}},
		{"columnstore", C, []DataCompression{C, A, A}, archive,
			[]string{"DATA_COMPRESSION = COLUMNSTORE_ARCHIVE ON PARTITIONS (2 TO 3)"}},
	}
	for _, c := range cases {
		if got := compressionOptions(c.single, c.parts, c.named); !slices.Equal(got, c.want) {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}

	// Through the whole script: the clustered PK, a nonclustered index, a
	// columnstore index and a heap each carry their own partitions.
	p := tableScriptParts{
		cols: []*Column{{Name: "a", DataType: DataTypeInt}, {Name: "b", DataType: DataTypeInt}},
		indexes: []*Index{
			{Name: "PK_T", IsPrimaryKey: true, IsClustered: true, AllowRowLocks: true, AllowPageLocks: true,
				DataCompression: P, PartitionCompression: []DataCompression{P, N, N},
				KeyColumns: []IndexColumn{{Name: "a"}}},
			{Name: "IX_b", Type: IndexTypeNonClustered, AllowRowLocks: true, AllowPageLocks: true,
				DataCompression: N, PartitionCompression: []DataCompression{N, R},
				KeyColumns: []IndexColumn{{Name: "b"}}},
			{Name: "NCCI", Type: IndexTypeColumnStore,
				DataCompression: A, PartitionCompression: []DataCompression{A, C},
				IncludedColumns: []IndexColumn{{Name: "b", IsIncluded: true}}},
		},
	}
	got := buildTableScript("dbo", "T", "db", p, ScriptOptions{})
	for _, want := range []string{
		"PRIMARY KEY CLUSTERED ([a] ASC) WITH (DATA_COMPRESSION = PAGE ON PARTITIONS (1))",
		"WITH (DATA_COMPRESSION = ROW ON PARTITIONS (2));",
		"ON [dbo].[T] ([b]) WITH (DATA_COMPRESSION = COLUMNSTORE_ARCHIVE ON PARTITIONS (1));",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("script is missing %q:\n%s", want, got)
		}
	}
	heap := tableScriptParts{cols: p.cols, table: tableScriptOptions{
		HeapCompression: "NONE", HeapPartitionCompression: []DataCompression{N, P, P}}}
	if got := buildTableScript("dbo", "T", "db", heap, ScriptOptions{}); !strings.Contains(got, ")\nWITH (DATA_COMPRESSION = PAGE ON PARTITIONS (2 TO 3));") {
		t.Errorf("a heap with mixed compression lost it:\n%s", got)
	}
}

// TestScriptXMLAndSpatialIndexes (G4): XML and spatial indexes script as
// their own CREATE forms, not as a comment, and a secondary XML index comes
// after the primary it is built over whatever order the list had.
func TestScriptXMLAndSpatialIndexes(t *testing.T) {
	locks := func(idx *Index) *Index { idx.AllowRowLocks, idx.AllowPageLocks = true, true; return idx }
	doc := []IndexColumn{{Name: "Doc"}}
	secondary := func(name string, kind XMLSecondaryIndexType) *Index {
		return locks(&Index{Name: name, Type: IndexTypeXML, KeyColumns: doc, PrimaryXMLIndex: "PX", SecondaryXMLType: kind})
	}
	p := tableScriptParts{
		cols: []*Column{{Name: "ID", DataType: DataTypeInt}, {Name: "Doc", DataType: DataTypeXML}},
		indexes: []*Index{
			locks(&Index{Name: "PK_X", IsPrimaryKey: true, IsClustered: true, KeyColumns: []IndexColumn{{Name: "ID"}}}),
			// Listed ahead of its primary: the script must still put it after.
			secondary("SX_Path", XMLSecondaryPath),
			locks(&Index{Name: "PX", Type: IndexTypeXML, KeyColumns: doc, IsPrimaryXML: true, FillFactor: 90}),
			secondary("SX_Value", XMLSecondaryValue),
			secondary("SX_Prop", XMLSecondaryProperty),
			locks(&Index{Name: "SP_G", Type: IndexTypeSpatial, KeyColumns: []IndexColumn{{Name: "G"}},
				Tessellation: SpatialGeometryGrid, BoundingBox: &SpatialBoundingBox{XMin: -1.5, YMin: 0, XMax: 500, YMax: 200},
				GridLevels:     SpatialGridLevels{Level1: SpatialGridLow, Level2: SpatialGridMedium, Level3: SpatialGridHigh, Level4: SpatialGridLow},
				CellsPerObject: 64, DataCompression: DataCompressionPage,
				DataSpace: DataSpace{Name: "FG_Archive"}}),
			locks(&Index{Name: "SP_Gg", Type: IndexTypeSpatial, KeyColumns: []IndexColumn{{Name: "Gg"}},
				Tessellation: SpatialGeographyAutoGrid, CellsPerObject: 12,
				// A partitioned table's spatial index is aligned by the server
				// and refuses an ON naming the scheme.
				DataSpace: DataSpace{Name: "ps_year", IsPartitionScheme: true, PartitionColumn: "Yr"}}),
			// A selective XML index is neither form: still a comment.
			locks(&Index{Name: "SXI", Type: IndexTypeXML, KeyColumns: doc}),
		},
	}
	got := buildTableScript("dbo", "X", "db", p, ScriptOptions{})
	for _, want := range []string{
		"CREATE PRIMARY XML INDEX [PX]\n    ON [dbo].[X] ([Doc])\n    WITH (FILLFACTOR = 90);\nGO",
		"CREATE XML INDEX [SX_Path]\n    ON [dbo].[X] ([Doc])\n    USING XML INDEX [PX] FOR PATH;\nGO",
		"CREATE XML INDEX [SX_Value]\n    ON [dbo].[X] ([Doc])\n    USING XML INDEX [PX] FOR VALUE;\nGO",
		"CREATE XML INDEX [SX_Prop]\n    ON [dbo].[X] ([Doc])\n    USING XML INDEX [PX] FOR PROPERTY;\nGO",
		"CREATE SPATIAL INDEX [SP_G]\n    ON [dbo].[X] ([G])\n    USING GEOMETRY_GRID\n" +
			"    WITH (BOUNDING_BOX = (-1.5, 0, 500, 200), GRIDS = (LEVEL_1 = LOW, LEVEL_2 = MEDIUM, LEVEL_3 = HIGH, LEVEL_4 = LOW), " +
			"CELLS_PER_OBJECT = 64, DATA_COMPRESSION = PAGE) ON [FG_Archive];\nGO",
		"CREATE SPATIAL INDEX [SP_Gg]\n    ON [dbo].[X] ([Gg])\n    USING GEOGRAPHY_AUTO_GRID\n    WITH (CELLS_PER_OBJECT = 12);\nGO",
		"-- XML index [SXI] on [dbo].[X] is not scripted",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("script is missing %q:\n%s", want, got)
		}
	}
	primary := strings.Index(got, "CREATE PRIMARY XML INDEX [PX]")
	for _, name := range []string{"SX_Path", "SX_Value", "SX_Prop"} {
		if i := strings.Index(got, "CREATE XML INDEX ["+name+"]"); i < primary {
			t.Errorf("secondary XML index %s is created before its primary:\n%s", name, got)
		}
	}

	// A lock option off, and no GRIDS on an automatic grid even if the
	// levels were somehow read.
	odd := &Index{Name: "SP", Type: IndexTypeSpatial, KeyColumns: []IndexColumn{{Name: "G"}},
		Tessellation: SpatialGeometryAutoGrid, BoundingBox: &SpatialBoundingBox{XMax: 1, YMax: 1},
		GridLevels: SpatialGridLevels{Level1: SpatialGridLow}, AllowRowLocks: false, AllowPageLocks: true}
	if got, want := xmlOrSpatialIndexCreate(odd, "[t]"),
		"CREATE SPATIAL INDEX [SP]\n    ON [t] ([G])\n    USING GEOMETRY_AUTO_GRID\n    WITH (BOUNDING_BOX = (0, 0, 1, 1), ALLOW_ROW_LOCKS = OFF)"; got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
}

func TestModuleSetOptions(t *testing.T) {
	got := moduleSetOptions(false, true)
	want := "SET ANSI_NULLS OFF;\nGO\nSET QUOTED_IDENTIFIER ON;\nGO\n"
	if got != want {
		t.Errorf("moduleSetOptions(false, true) = %q, want %q", got, want)
	}
}
