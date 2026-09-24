package gosmo

import (
	"errors"
	"strings"
	"testing"
)

// TestScriptTableRefusesKindsItCannotExpress: each of these would otherwise
// come out as a plain table under the same name, and under DROP AND CREATE
// the original would be dropped and something different created.
func TestScriptTableRefusesKindsItCannotExpress(t *testing.T) {
	for _, c := range []struct {
		name string
		o    tableScriptOptions
		want string
	}{
		{"external", tableScriptOptions{IsExternal: true}, "an external table"},
		{"filetable", tableScriptOptions{IsFileTable: true}, "a FileTable"},
		{"ledger", tableScriptOptions{LedgerType: 3}, "a ledger table"},
		{"ledger history", tableScriptOptions{LedgerType: 1}, "a ledger table"},
		{"always encrypted", tableScriptOptions{HasEncryptedColumns: true}, "Always Encrypted"},
	} {
		err := c.o.refusal("[dbo].[T]")
		if !errors.Is(err, ErrUnsupported) {
			t.Errorf("%s: refusal = %v, want an ErrUnsupported error", c.name, err)
			continue
		}
		if errors.Is(err, ErrUnsupportedVersion) {
			t.Errorf("%s: refusal also satisfies ErrUnsupportedVersion; the server is not too old", c.name)
		}
		if !strings.Contains(err.Error(), c.want) || !strings.Contains(err.Error(), "[dbo].[T]") {
			t.Errorf("%s: refusal = %q, want it to name %q and the table", c.name, err, c.want)
		}
	}
	for _, o := range []tableScriptOptions{{}, {IsNode: true}, {IsEdge: true}, {IsMemoryOptimized: true}} {
		if err := o.refusal("[dbo].[T]"); err != nil {
			t.Errorf("refusal(%+v) = %v, want nil", o, err)
		}
	}
}

// graphTestColumns is a node or edge table's catalog columns as sys.columns
// reports them: the internal ones first, with their per-table GUID names.
func graphTestColumns(edge bool) []*Column {
	cols := []*Column{
		{Name: "graph_id_AAAA", DataType: DataTypeBigInt, GraphType: GraphColumnID, IsHidden: true},
	}
	if !edge {
		cols = append(cols,
			&Column{Name: "$node_id_BBBB", DataType: DataTypeNVarChar, MaxLength: 2000, GraphType: GraphColumnIDComputed},
			&Column{Name: "ID", DataType: DataTypeInt},
			&Column{Name: "name", DataType: DataTypeNVarChar, MaxLength: 200, IsNullable: true})
		return cols
	}
	return append(cols,
		&Column{Name: "$edge_id_BBBB", DataType: DataTypeNVarChar, MaxLength: 2000, GraphType: GraphColumnIDComputed},
		&Column{Name: "from_obj_id_CCCC", DataType: DataTypeInt, GraphType: GraphColumnFromObjID, IsHidden: true},
		&Column{Name: "from_id_DDDD", DataType: DataTypeBigInt, GraphType: GraphColumnFromID, IsHidden: true},
		&Column{Name: "$from_id_EEEE", DataType: DataTypeNVarChar, MaxLength: 2000, GraphType: GraphColumnFromIDComputed},
		&Column{Name: "to_obj_id_FFFF", DataType: DataTypeInt, GraphType: GraphColumnToObjID, IsHidden: true},
		&Column{Name: "to_id_GGGG", DataType: DataTypeBigInt, GraphType: GraphColumnToID, IsHidden: true},
		&Column{Name: "$to_id_HHHH", DataType: DataTypeNVarChar, MaxLength: 2000, GraphType: GraphColumnToIDComputed},
		&Column{Name: "rating", DataType: DataTypeInt, IsNullable: true})
}

// TestBuildTableScriptGraphNode is the regression test for a node table
// scripting as DDL that cannot run: the internal graph_id column came out as
// `bigint HIDDEN NOT NULL` (HIDDEN without GENERATED ALWAYS does not parse),
// the automatic GRAPH_UNIQUE_INDEX_… was recreated, and AS NODE was missing.
func TestBuildTableScriptGraphNode(t *testing.T) {
	fg2 := DataSpace{Name: "FG2"}
	p := tableScriptParts{
		cols: graphTestColumns(false),
		indexes: []*Index{
			{Name: "PK_Person", IsPrimaryKey: true, IsClustered: true, IsUnique: true, Type: IndexTypeClustered,
				AllowRowLocks: true, AllowPageLocks: true, KeyColumns: []IndexColumn{{Name: "ID"}}, DataSpace: fg2},
			{Name: "GRAPH_UNIQUE_INDEX_0A29", IsUnique: true, Type: IndexTypeNonClustered,
				AllowRowLocks: true, AllowPageLocks: true, KeyColumns: []IndexColumn{{Name: "graph_id_AAAA"}}, DataSpace: fg2},
			{Name: "IX_node", Type: IndexTypeNonClustered,
				AllowRowLocks: true, AllowPageLocks: true, KeyColumns: []IndexColumn{{Name: "graph_id_AAAA", Descending: true}}, DataSpace: fg2},
		},
		ds:    fg2,
		table: tableScriptOptions{IsNode: true},
	}
	script := buildTableScript("dbo", "Person", "G", p, DefaultScriptOptions())

	for _, want := range []string{
		") AS NODE ON [FG2];",
		"CREATE NONCLUSTERED INDEX [IX_node]\n    ON [dbo].[Person] ($node_id DESC)",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("script is missing %q:\n%s", want, script)
		}
	}
	for _, bad := range []string{"graph_id_", "$node_id_BBBB", "GRAPH_UNIQUE_INDEX", "HIDDEN"} {
		if strings.Contains(script, bad) {
			t.Errorf("script contains %q:\n%s", bad, script)
		}
	}
}

// TestBuildTableScriptGraphEdge: an index on ($from_id, $to_id) is stored on
// four internal columns, two per pseudo-column, and has to come back as the
// two it was written with. Edge constraints are scripted with their trust.
func TestBuildTableScriptGraphEdge(t *testing.T) {
	p := tableScriptParts{
		cols: graphTestColumns(true),
		indexes: []*Index{
			{Name: "GRAPH_UNIQUE_INDEX_30C3", IsUnique: true, Type: IndexTypeNonClustered,
				AllowRowLocks: true, AllowPageLocks: true, KeyColumns: []IndexColumn{{Name: "graph_id_AAAA"}}},
			{Name: "IX_ends", Type: IndexTypeNonClustered, AllowRowLocks: true, AllowPageLocks: true,
				KeyColumns: []IndexColumn{{Name: "from_obj_id_CCCC"}, {Name: "from_id_DDDD"}, {Name: "to_obj_id_FFFF"}, {Name: "to_id_GGGG"}}},
		},
		ecs: []*EdgeConstraint{
			{Name: "EC_Likes", DeleteAction: "CASCADE", Clauses: []EdgeConnection{
				{"dbo", "Person", "dbo", "City"}, {"dbo", "City", "dbo", "City"}}},
			{Name: "EC_off", DeleteAction: "NO_ACTION", IsDisabled: true, IsNotTrusted: true,
				Clauses: []EdgeConnection{{"dbo", "Person", "dbo", "Person"}}},
		},
		table: tableScriptOptions{IsEdge: true},
	}
	script := buildTableScript("dbo", "Likes", "G", p, DefaultScriptOptions())

	for _, want := range []string{
		"    [rating] int NULL\n) AS EDGE;",
		"ON [dbo].[Likes] ($from_id ASC, $to_id ASC)",
		"ALTER TABLE [dbo].[Likes] WITH CHECK\n    ADD CONSTRAINT [EC_Likes]\n    CONNECTION ([dbo].[Person] TO [dbo].[City], [dbo].[City] TO [dbo].[City])\n    ON DELETE CASCADE;\nGO\n",
		"ALTER TABLE [dbo].[Likes] WITH NOCHECK\n    ADD CONSTRAINT [EC_off]\n    CONNECTION ([dbo].[Person] TO [dbo].[Person]);\nGO\nALTER TABLE [dbo].[Likes] NOCHECK CONSTRAINT [EC_off];",
		"IF NOT EXISTS (SELECT 1 FROM sys.objects WHERE name = N'EC_Likes' AND parent_object_id = OBJECT_ID(N'[dbo].[Likes]'))",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("script is missing %q:\n%s", want, script)
		}
	}
	for _, bad := range []string{"from_obj_id", "to_id_GGGG", "$edge_id", "GRAPH_UNIQUE_INDEX"} {
		if strings.Contains(script, bad) {
			t.Errorf("script contains %q:\n%s", bad, script)
		}
	}
}

// TestBuildTableScriptMemoryOptimized: a memory-optimized table scripted as
// a disk table plus CREATE INDEX statements is refused by the server on
// every index, and loses MEMORY_OPTIMIZED and DURABILITY besides. Its
// indexes go inside the CREATE TABLE, with none of a disk index's options —
// the catalog reports row and page locks off for them.
func TestBuildTableScriptMemoryOptimized(t *testing.T) {
	p := tableScriptParts{
		cols: []*Column{
			{Name: "id", DataType: DataTypeInt},
			{Name: "v", DataType: DataTypeNVarChar, MaxLength: 100},
			{Name: "w", DataType: DataTypeInt, IsNullable: true},
		},
		indexes: []*Index{
			{Name: "UQ_MO", IsUniqueConstraint: true, IsUnique: true, Type: IndexTypeNonClustered,
				KeyColumns: []IndexColumn{{Name: "w"}}},
			{Name: "ix_w", Type: IndexTypeNonClusteredHash, BucketCount: 64,
				KeyColumns: []IndexColumn{{Name: "w"}}},
			{Name: "ix_v", Type: IndexTypeNonClustered, KeyColumns: []IndexColumn{{Name: "v", Descending: true}}},
			{Name: "PK_MO", IsPrimaryKey: true, IsUnique: true, Type: IndexTypeNonClusteredHash, BucketCount: 1024,
				KeyColumns: []IndexColumn{{Name: "id"}}},
		},
		table: tableScriptOptions{IsMemoryOptimized: true, Durability: "SCHEMA_ONLY", HeapCompression: "NONE"},
	}
	script := buildTableScript("dbo", "MO", "M", p, DefaultScriptOptions())

	want := "CREATE TABLE [dbo].[MO] (\n" +
		"    [id] int NOT NULL,\n" +
		"    [v] nvarchar(50) NOT NULL,\n" +
		"    [w] int NULL,\n" +
		"    CONSTRAINT [PK_MO] PRIMARY KEY NONCLUSTERED HASH ([id]) WITH (BUCKET_COUNT = 1024),\n" +
		"    CONSTRAINT [UQ_MO] UNIQUE NONCLUSTERED ([w] ASC),\n" +
		"    INDEX [ix_w] NONCLUSTERED HASH ([w]) WITH (BUCKET_COUNT = 64),\n" +
		"    INDEX [ix_v] NONCLUSTERED ([v] DESC)\n" +
		")\nWITH (MEMORY_OPTIMIZED = ON, DURABILITY = SCHEMA_ONLY);\nGO\n\n"
	if !strings.HasSuffix(script, want) {
		t.Errorf("script =\n%s\nwant it to end with\n%s", script, want)
	}
}

// TestBuildTableScriptFileStream: a FILESTREAM column needs its attribute,
// FILESTREAM_ON, and — because CREATE TABLE refuses a FILESTREAM column on a
// table without one (Msg 5505) — the ROWGUIDCOL's unique constraint inside
// the CREATE TABLE rather than added after it. TEXTIMAGE_ON is emitted when
// the LOB data is on a filegroup other than the table's.
func TestBuildTableScriptFileStream(t *testing.T) {
	fg2 := DataSpace{Name: "FG2"}
	p := tableScriptParts{
		cols: []*Column{
			{Name: "id", DataType: DataTypeUniqueIdentifier, IsRowGUID: true},
			{Name: "doc", DataType: DataTypeVarBinary, MaxLength: -1, IsFileStream: true, IsNullable: true},
			{Name: "notes", DataType: DataTypeNVarChar, MaxLength: -1, IsNullable: true},
		},
		indexes: []*Index{
			{Name: "UQ_FS", IsUniqueConstraint: true, IsUnique: true, Type: IndexTypeNonClustered,
				AllowRowLocks: true, AllowPageLocks: true, KeyColumns: []IndexColumn{{Name: "id"}}, DataSpace: fg2},
		},
		ds: fg2,
		table: tableScriptOptions{LobDataSpace: "PRIMARY", LobIsFileGroup: true,
			FileStreamDataSpace: "FSG"},
	}
	script := buildTableScript("dbo", "FS", "F", p, DefaultScriptOptions())

	for _, want := range []string{
		"[doc] varbinary(MAX) FILESTREAM NULL,",
		"[id] uniqueidentifier ROWGUIDCOL NOT NULL,",
		"    CONSTRAINT [UQ_FS] UNIQUE NONCLUSTERED ([id] ASC) ON [FG2]\n) ON [FG2] TEXTIMAGE_ON [PRIMARY] FILESTREAM_ON [FSG];",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("script is missing %q:\n%s", want, script)
		}
	}
	if strings.Contains(script, "ADD CONSTRAINT [UQ_FS]") {
		t.Errorf("the unique constraint is also added after the table:\n%s", script)
	}
}

// TestTableStorageClauses: each clause appears only where it says something
// the ON clause does not, and never where the server would refuse it.
func TestTableStorageClauses(t *testing.T) {
	lob := []*Column{{Name: "n", DataType: DataTypeNVarChar, MaxLength: -1}}
	noLob := []*Column{{Name: "n", DataType: DataTypeInt}}
	fs := []*Column{{Name: "d", DataType: DataTypeVarBinary, MaxLength: -1, IsFileStream: true}}
	for _, c := range []struct {
		name string
		p    tableScriptParts
		want string
	}{
		{"lob on the table's filegroup", tableScriptParts{cols: lob, ds: DataSpace{Name: "PRIMARY"},
			table: tableScriptOptions{LobDataSpace: "PRIMARY", LobIsFileGroup: true}}, ""},
		{"lob elsewhere", tableScriptParts{cols: lob, ds: DataSpace{Name: "PRIMARY", IsDefaultFileGroup: true},
			table: tableScriptOptions{LobDataSpace: "FG2", LobIsFileGroup: true}}, " TEXTIMAGE_ON [FG2]"},
		// lob_data_space_id outlives the last LOB column (Msg 1709 otherwise).
		{"no lob column left", tableScriptParts{cols: noLob, ds: DataSpace{Name: "PRIMARY"},
			table: tableScriptOptions{LobDataSpace: "FG2", LobIsFileGroup: true}}, ""},
		{"partitioned", tableScriptParts{cols: lob, ds: DataSpace{Name: "ps", IsPartitionScheme: true, PartitionColumn: "c"},
			table: tableScriptOptions{LobDataSpace: "ps"}}, ""},
		{"filestream alone is not lob", tableScriptParts{cols: fs, ds: DataSpace{Name: "PRIMARY"},
			table: tableScriptOptions{LobDataSpace: "FG2", LobIsFileGroup: true, FileStreamDataSpace: "FSG"}}, " FILESTREAM_ON [FSG]"},
		{"filestream space with no filestream column", tableScriptParts{cols: noLob, ds: DataSpace{Name: "PRIMARY"},
			table: tableScriptOptions{FileStreamDataSpace: "FSG"}}, ""},
	} {
		if got := tableStorageClauses(c.p); got != c.want {
			t.Errorf("%s: tableStorageClauses = %q, want %q", c.name, got, c.want)
		}
	}
}
