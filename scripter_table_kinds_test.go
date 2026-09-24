package gosmo

import (
	"database/sql"
	"errors"
	"slices"
	"strings"
	"testing"
)

// TestScriptTableRefusesKindsItCannotExpress: a ledger history table and a
// dropped ledger table are made by the ledger, never by a CREATE TABLE, and
// would otherwise come out as a plain table under the same name — which
// under DROP AND CREATE drops the original and creates something different.
func TestScriptTableRefusesKindsItCannotExpress(t *testing.T) {
	for _, c := range []struct {
		name string
		o    tableScriptOptions
		want string
	}{
		{"ledger history", tableScriptOptions{LedgerType: 1}, "a ledger history table"},
		{"dropped ledger", tableScriptOptions{LedgerType: 3, IsDroppedLedgerTable: true}, "a dropped ledger table"},
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
	for _, o := range []tableScriptOptions{{}, {IsNode: true}, {IsEdge: true}, {IsMemoryOptimized: true},
		{LedgerType: 2}, {LedgerType: 3}, {IsFileTable: true}, {IsExternal: true}} {
		if err := o.refusal("[dbo].[T]"); err != nil {
			t.Errorf("refusal(%+v) = %v, want nil", o, err)
		}
	}
}

// ledgerTestColumns is a ledger table's catalog columns: its own, then the
// generated ones (START only for append-only), then one dropped column.
func ledgerTestColumns(updatable bool) []*Column {
	cols := []*Column{
		{Name: "id", DataType: DataTypeInt},
		{Name: "v", DataType: DataTypeNVarChar, MaxLength: 20, IsNullable: true},
		{Name: "ledger_start_transaction_id", DataType: DataTypeBigInt, GeneratedAlwaysType: 7, IsHidden: true},
	}
	if updatable {
		cols = append(cols, &Column{Name: "ledger_end_transaction_id", DataType: DataTypeBigInt, GeneratedAlwaysType: 8, IsHidden: true, IsNullable: true})
	}
	cols = append(cols, &Column{Name: "ledger_start_sequence_number", DataType: DataTypeBigInt, GeneratedAlwaysType: 9, IsHidden: true})
	if updatable {
		cols = append(cols, &Column{Name: "ledger_end_sequence_number", DataType: DataTypeBigInt, GeneratedAlwaysType: 10, IsHidden: true, IsNullable: true})
	}
	return append(cols, &Column{Name: "MSSQL_DroppedLedgerColumn_x_1", DataType: DataTypeInt, IsNullable: true, IsDroppedLedgerColumn: true})
}

// TestBuildTableScriptLedger: both ledger kinds keep their LEDGER option,
// the ledger view with its renamed columns, the generated columns and — for
// the updatable kind — its history table; a dropped ledger column is not
// recreated as an ordinary one.
func TestBuildTableScriptLedger(t *testing.T) {
	view := tableScriptOptions{LedgerViewSchema: "lv", LedgerViewName: "T_V",
		LedgerViewColumns: []string{"tx", "sq", "op", "op desc"}}

	upd := view
	upd.LedgerType, upd.HistorySchema, upd.HistoryTable = 2, "dbo", "T_H"
	s := buildTableScript("dbo", "T", "G", tableScriptParts{cols: ledgerTestColumns(true), table: upd}, DefaultScriptOptions())
	for _, want := range []string{
		"[ledger_start_transaction_id] bigint GENERATED ALWAYS AS TRANSACTION_ID START HIDDEN NOT NULL",
		"[ledger_end_transaction_id] bigint GENERATED ALWAYS AS TRANSACTION_ID END HIDDEN NULL",
		"[ledger_start_sequence_number] bigint GENERATED ALWAYS AS SEQUENCE_NUMBER START HIDDEN NOT NULL",
		"[ledger_end_sequence_number] bigint GENERATED ALWAYS AS SEQUENCE_NUMBER END HIDDEN NULL",
		"SYSTEM_VERSIONING = ON (HISTORY_TABLE = [dbo].[T_H]), LEDGER = ON (LEDGER_VIEW = [lv].[T_V] " +
			"(TRANSACTION_ID_COLUMN_NAME = [tx], SEQUENCE_NUMBER_COLUMN_NAME = [sq], " +
			"OPERATION_TYPE_COLUMN_NAME = [op], OPERATION_TYPE_DESC_COLUMN_NAME = [op desc])))",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("updatable ledger script is missing %q:\n%s", want, s)
		}
	}
	// sys.tables reports an updatable ledger table as non-temporal, and
	// versioning cannot be switched off on one: the drop is a plain DROP.
	opts := DefaultScriptOptions()
	opts.Verb = ScriptDrop
	if d := buildTableScript("dbo", "T", "G", tableScriptParts{cols: ledgerTestColumns(true), table: upd}, opts); strings.Contains(d, "SYSTEM_VERSIONING") {
		t.Errorf("ledger drop switches versioning off:\n%s", d)
	}

	app := view
	app.LedgerType = 3
	s = buildTableScript("dbo", "T", "G", tableScriptParts{cols: ledgerTestColumns(false), table: app}, DefaultScriptOptions())
	if want := "LEDGER = ON (LEDGER_VIEW = [lv].[T_V] (TRANSACTION_ID_COLUMN_NAME = [tx], " +
		"SEQUENCE_NUMBER_COLUMN_NAME = [sq], OPERATION_TYPE_COLUMN_NAME = [op], OPERATION_TYPE_DESC_COLUMN_NAME = [op desc]), " +
		"APPEND_ONLY = ON))"; !strings.Contains(s, want) {
		t.Errorf("append-only ledger script is missing %q:\n%s", want, s)
	}
	for _, bad := range []string{"SYSTEM_VERSIONING", "MSSQL_DroppedLedgerColumn", "_END"} {
		if strings.Contains(s, bad) {
			t.Errorf("append-only ledger script contains %q:\n%s", bad, s)
		}
	}

	// No view read: the bare option still makes it a ledger table.
	if got := ledgerOptions(tableScriptOptions{LedgerType: 3}); !slices.Equal(got, []string{"LEDGER = ON (APPEND_ONLY = ON)"}) {
		t.Errorf("ledgerOptions without a view = %q", got)
	}
	if got := ledgerOptions(tableScriptOptions{}); got != nil {
		t.Errorf("ledgerOptions for a non-ledger table = %q, want nil", got)
	}
}

// TestBuildTableScriptAlwaysEncrypted: an encrypted column keeps its key,
// encryption type and algorithm, and a deterministic string column its
// _BIN2 collation, without which the CREATE fails.
func TestBuildTableScriptAlwaysEncrypted(t *testing.T) {
	cols := []*Column{
		{Name: "id", DataType: DataTypeInt, IsNullable: true},
		{Name: "ssn", DataType: DataTypeChar, MaxLength: 11, Collation: "Latin1_General_BIN2",
			ColumnEncryptionKey: "CEK 1", EncryptionType: "DETERMINISTIC", EncryptionAlgorithm: "AEAD_AES_256_CBC_HMAC_SHA_256"},
		{Name: "r", DataType: DataTypeNVarChar, MaxLength: 100, IsNullable: true, Collation: "SQL_Latin1_General_CP1_CI_AS",
			ColumnEncryptionKey: "CEK 1", EncryptionType: "RANDOMIZED", EncryptionAlgorithm: "AEAD_AES_256_CBC_HMAC_SHA_256"},
	}
	s := buildTableScript("dbo", "AE", "G", tableScriptParts{cols: cols,
		table: tableScriptOptions{DatabaseCollation: "SQL_Latin1_General_CP1_CI_AS"}}, DefaultScriptOptions())
	for _, want := range []string{
		"    [id] int NULL,\n",
		"[ssn] char(11) COLLATE Latin1_General_BIN2 ENCRYPTED WITH (COLUMN_ENCRYPTION_KEY = [CEK 1], " +
			"ENCRYPTION_TYPE = DETERMINISTIC, ALGORITHM = 'AEAD_AES_256_CBC_HMAC_SHA_256') NOT NULL",
		"[r] nvarchar(50) ENCRYPTED WITH (COLUMN_ENCRYPTION_KEY = [CEK 1], " +
			"ENCRYPTION_TYPE = RANDOMIZED, ALGORITHM = 'AEAD_AES_256_CBC_HMAC_SHA_256') NULL",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("script is missing %q:\n%s", want, s)
		}
	}
}

// TestBuildFileTableScript: a FileTable is AS FILETABLE with its directory,
// collation and constraint names; none of its fixed columns or the objects
// AS FILETABLE creates are scripted, and what was added afterwards is.
func TestBuildFileTableScript(t *testing.T) {
	sys := []string{"PK__FT__1", "UQ__FT__2", "UQ__FT__3", "CK__FT__4", "FK__FT__parent_5"}
	p := tableScriptParts{
		cols: []*Column{{Name: "stream_id", DataType: DataTypeUniqueIdentifier}, {Name: "name", DataType: DataTypeNVarChar, MaxLength: 510}},
		indexes: []*Index{
			{Name: "PK__FT__1", IsPrimaryKey: true, IsUnique: true, Type: IndexTypeNonClustered, KeyColumns: []IndexColumn{{Name: "path_locator"}}},
			{Name: "UQ__FT__2", IsUniqueConstraint: true, IsUnique: true, Type: IndexTypeNonClustered, KeyColumns: []IndexColumn{{Name: "stream_id"}}},
			{Name: "UQ__FT__3", IsUniqueConstraint: true, IsUnique: true, Type: IndexTypeNonClustered,
				KeyColumns: []IndexColumn{{Name: "parent_path_locator"}, {Name: "name"}}},
			{Name: "IX_FT_name", Type: IndexTypeNonClustered, AllowRowLocks: true, AllowPageLocks: true, KeyColumns: []IndexColumn{{Name: "name"}}},
		},
		fks: []*ForeignKey{{Name: "FK__FT__parent_5", Columns: []string{"parent_path_locator"},
			ReferencedSchema: "dbo", ReferencedTable: "FT", ReferencedColumns: []string{"path_locator"}}},
		checks: []*CheckConstraint{{Name: "CK__FT__4", Definition: "(1=1)"}, {Name: "CK_user", Definition: "([is_offline]=(0))"}},
		ds:     DataSpace{Name: "FG2"},
		table: tableScriptOptions{IsFileTable: true, FileTableDirectory: "ft 'dir'", FileTableCollation: "Latin1_General_CI_AS",
			FileStreamDataSpace: "FSG", FileTableNamespaceEnabled: true, FileTableSystemObjects: sys},
	}
	s := buildFileTableScript("dbo", "FT", "G", p, DefaultScriptOptions())
	for _, want := range []string{
		"CREATE TABLE [dbo].[FT] AS FILETABLE ON [FG2] FILESTREAM_ON [FSG]\nWITH (\n" +
			"    FILETABLE_DIRECTORY = N'ft ''dir''',\n" +
			"    FILETABLE_COLLATE_FILENAME = Latin1_General_CI_AS,\n" +
			"    FILETABLE_PRIMARY_KEY_CONSTRAINT_NAME = [PK__FT__1],\n" +
			"    FILETABLE_STREAMID_UNIQUE_CONSTRAINT_NAME = [UQ__FT__2],\n" +
			"    FILETABLE_FULLPATH_UNIQUE_CONSTRAINT_NAME = [UQ__FT__3]\n);",
		"CREATE NONCLUSTERED INDEX [IX_FT_name]",
		"ADD CONSTRAINT [CK_user]",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("script is missing %q:\n%s", want, s)
		}
	}
	for _, bad := range []string{"[stream_id] uniqueidentifier", "ADD CONSTRAINT [UQ__FT", "CK__FT__4", "FK__FT__parent_5", "DISABLE FILETABLE_NAMESPACE"} {
		if strings.Contains(s, bad) {
			t.Errorf("script contains %q:\n%s", bad, s)
		}
	}
	p.table.FileTableNamespaceEnabled = false
	if s := buildFileTableScript("dbo", "FT", "G", p, DefaultScriptOptions()); !strings.Contains(s, "ALTER TABLE [dbo].[FT] DISABLE FILETABLE_NAMESPACE;") {
		t.Errorf("a disabled namespace is not disabled again:\n%s", s)
	}
}

// TestBuildExternalTableScript: CREATE EXTERNAL TABLE with its WITH clause,
// the data source and file format ahead of it, and DROP EXTERNAL TABLE — a
// DROP TABLE on an external table fails.
func TestBuildExternalTableScript(t *testing.T) {
	p := tableScriptParts{
		cols: []*Column{{Name: "id", DataType: DataTypeInt}, {Name: "name", DataType: DataTypeNVarChar, MaxLength: 100,
			IsNullable: true, Collation: "Latin1_General_BIN2"}},
		table: tableScriptOptions{IsExternal: true, DatabaseCollation: "SQL_Latin1_General_CP1_CI_AS",
			External: externalTableOptions{DataSource: "ds", FileFormat: "csv", Location: "y/'q'/",
				RejectType: "PERCENTAGE", RejectValue: sql.NullFloat64{Float64: 10, Valid: true},
				RejectSampleValue: sql.NullFloat64{Float64: 1000, Valid: true}}},
	}
	deps := []string{"-- data source\n", "-- file format\n"}
	opts := DefaultScriptOptions()
	opts.Verb = ScriptDropAndCreate
	s := buildExternalTableScript("dbo", "ET", "G", p, deps, opts)
	want := "IF OBJECT_ID(N'[dbo].[ET]') IS NOT NULL\n    DROP EXTERNAL TABLE [dbo].[ET];\nGO\n\n" +
		"/* External table: [dbo].[ET]  Database: G */\n" +
		"-- data source\n\n-- file format\n\n" +
		"IF OBJECT_ID(N'[dbo].[ET]') IS NULL\n" +
		"CREATE EXTERNAL TABLE [dbo].[ET] (\n" +
		"    [id] int NOT NULL,\n" +
		"    [name] nvarchar(50) COLLATE Latin1_General_BIN2 NULL\n" +
		")\nWITH (\n" +
		"    LOCATION = N'y/''q''/',\n" +
		"    DATA_SOURCE = [ds],\n" +
		"    FILE_FORMAT = [csv],\n" +
		"    REJECT_TYPE = PERCENTAGE,\n" +
		"    REJECT_VALUE = 10,\n" +
		"    REJECT_SAMPLE_VALUE = 1000\n" +
		");\nGO\n"
	if s != want {
		t.Errorf("script =\n%s\nwant\n%s", s, want)
	}

	// An elastic-query table: no file format, so no reject options, and its
	// remote names and distribution.
	p.table.External = externalTableOptions{DataSource: "rdb",
		RejectType: "VALUE", RejectValue: sql.NullFloat64{Valid: true},
		RemoteSchema: "rs", RemoteObject: "ro", Distribution: "SHARDED", ShardingColumn: "id"}
	s = buildExternalTableScript("dbo", "ET", "G", p, nil, DefaultScriptOptions())
	if want := "WITH (\n    DATA_SOURCE = [rdb],\n    SCHEMA_NAME = N'rs',\n    OBJECT_NAME = N'ro',\n    DISTRIBUTION = SHARDED([id])\n);"; !strings.Contains(s, want) {
		t.Errorf("elastic-query script is missing %q:\n%s", want, s)
	}
	if strings.Contains(s, "REJECT") {
		t.Errorf("elastic-query script has reject options:\n%s", s)
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

// TestMemoryOptimizedColumnstoreKeepsItsCompressionDelay (G5): the inline
// form of a memory-optimized table's columnstore index carries the delay too.
func TestMemoryOptimizedColumnstoreKeepsItsCompressionDelay(t *testing.T) {
	for delay, want := range map[int]string{
		0:  "CLUSTERED COLUMNSTORE",
		60: "CLUSTERED COLUMNSTORE WITH (COMPRESSION_DELAY = 60 MINUTES)",
	} {
		idx := &Index{Name: "cci", Type: IndexTypeClusteredColumnStore, CompressionDelay: delay}
		if got := memoryOptimizedIndexSpec(idx); got != want {
			t.Errorf("delay %d: memoryOptimizedIndexSpec = %q, want %q", delay, got, want)
		}
	}
}
