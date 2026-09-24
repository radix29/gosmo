//go:build livedb

// Live verification of ScriptTable for the table kinds beyond a plain
// disk-based table: graph node and edge tables with their edge constraints,
// memory-optimized tables, TEXTIMAGE_ON and FILESTREAM, Always Encrypted
// columns, ledger tables and FileTables — and of the refusal for the ledger
// tables it cannot express.
//
// Each kind is replayed into a second database and the two catalogs are
// compared, since a script is only proven by running it.
//
//	go test -tags livedb . -run TestLiveScriptTableKinds -v \
//	  -livedb 'sqlserver://sa:PASS@host?TrustServerCertificate=true'
//
// Creates and drops two throwaway databases; touches nothing else. The
// FILESTREAM and FileTable parts are skipped on an instance with FILESTREAM
// disabled, and the ledger part before SQL Server 2022.
package gosmo

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"strings"
	"testing"
)

// liveAddFileGroups gives d the filegroups the table kinds need: FG2 (rows),
// MOG (memory-optimized data) and, when withFS, FSG (FILESTREAM).
func liveAddFileGroups(t *testing.T, db *sql.DB, ctx context.Context, d *Database, dataPath string, withFS bool) {
	t.Helper()
	n := d.Name
	stmts := []string{
		"ALTER DATABASE [" + n + "] ADD FILEGROUP FG2",
		"ALTER DATABASE [" + n + "] ADD FILE (NAME = " + n + "_fg2, FILENAME = '" + dataPath + n + "_fg2.ndf') TO FILEGROUP FG2",
		"ALTER DATABASE [" + n + "] ADD FILEGROUP MOG CONTAINS MEMORY_OPTIMIZED_DATA",
		"ALTER DATABASE [" + n + "] ADD FILE (NAME = " + n + "_mo, FILENAME = '" + dataPath + n + "_mo') TO FILEGROUP MOG",
	}
	if withFS {
		stmts = append(stmts,
			"ALTER DATABASE ["+n+"] ADD FILEGROUP FSG CONTAINS FILESTREAM",
			"ALTER DATABASE ["+n+"] ADD FILE (NAME = "+n+"_fs, FILENAME = '"+dataPath+n+"_fs') TO FILEGROUP FSG")
	}
	for _, s := range stmts {
		if _, err := db.ExecContext(ctx, s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
}

// The catalog, read so that two databases holding the same tables read the
// same: a graph table's internal columns and automatic index carry a
// per-table GUID in their names, so those are named by their kind instead.
const (
	liveKindsTablesQuery = `
SELECT t.name, t.is_node, t.is_edge, t.is_memory_optimized, t.durability_desc,
       ISNULL(lds.name, '-'), ISNULL(fds.name, '-'),
       ISNULL((SELECT ds.name FROM sys.indexes i JOIN sys.data_spaces ds ON ds.data_space_id = i.data_space_id
               WHERE i.object_id = t.object_id AND i.index_id IN (0, 1)), '-')
FROM   sys.tables t
LEFT   JOIN sys.data_spaces lds ON lds.data_space_id = NULLIF(t.lob_data_space_id, 0)
LEFT   JOIN sys.data_spaces fds ON fds.data_space_id = t.filestream_data_space_id
ORDER  BY t.name`
	liveKindsColumnsQuery = `
SELECT OBJECT_NAME(c.object_id), ISNULL(c.graph_type_desc, c.name), TYPE_NAME(c.user_type_id),
       c.max_length, c.is_nullable, c.is_filestream, c.is_rowguidcol, c.is_hidden
FROM   sys.columns c JOIN sys.tables t ON t.object_id = c.object_id
ORDER  BY OBJECT_NAME(c.object_id), c.column_id`
	liveKindsIndexesQuery = `
SELECT OBJECT_NAME(i.object_id),
       CASE WHEN i.name LIKE 'GRAPH[_]UNIQUE[_]INDEX[_]%' THEN 'GRAPH_UNIQUE_INDEX' ELSE i.name END AS n,
       i.type_desc, i.is_unique, i.is_primary_key, i.is_unique_constraint,
       ISNULL(h.bucket_count, 0), ISNULL(i.compression_delay, 0) AS delay, ISNULL(ds.name, '-'),
       (SELECT ISNULL(c.graph_type_desc, c.name) AS c, ic.is_descending_key AS d, ic.is_included_column AS inc
        FROM   sys.index_columns ic JOIN sys.columns c ON c.object_id = ic.object_id AND c.column_id = ic.column_id
        WHERE  ic.object_id = i.object_id AND ic.index_id = i.index_id
        ORDER  BY ic.key_ordinal, ic.index_column_id FOR JSON PATH)
FROM   sys.indexes i JOIN sys.tables t ON t.object_id = i.object_id
LEFT   JOIN sys.hash_indexes h ON h.object_id = i.object_id AND h.index_id = i.index_id
LEFT   JOIN sys.data_spaces ds ON ds.data_space_id = i.data_space_id
WHERE  i.type > 0
ORDER  BY OBJECT_NAME(i.object_id), n`
	liveKindsEdgeConstraintsQuery = `
SELECT OBJECT_NAME(ec.parent_object_id), ec.name, ec.delete_referential_action_desc, ec.is_disabled, ec.is_not_trusted,
       (SELECT OBJECT_NAME(c.from_object_id) AS f, OBJECT_NAME(c.to_object_id) AS t
        FROM sys.edge_constraint_clauses c WHERE c.object_id = ec.object_id
        ORDER BY c.clause_number FOR JSON PATH)
FROM   sys.edge_constraints ec
ORDER  BY ec.name`
)

func TestLiveScriptTableKinds(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()
	// Two databases with three filegroups each outlast liveDB's minute.
	ctx, cancel := context.WithTimeout(context.Background(), 5*60e9)
	defer cancel()

	var dataPath string
	var fsLevel int
	if err := db.QueryRowContext(ctx, `SELECT CONVERT(nvarchar(260), SERVERPROPERTY('InstanceDefaultDataPath')),
       CONVERT(int, ISNULL(SERVERPROPERTY('FilestreamEffectiveLevel'), 0))`).Scan(&dataPath, &fsLevel); err != nil {
		t.Fatalf("server properties: %v", err)
	}
	withFS := fsLevel >= 2

	src, dropSrc := liveScratchDB(t, db, ctx, "gosmo_kinds_src")
	defer dropSrc()
	dst, dropDst := liveScratchDB(t, db, ctx, "gosmo_kinds_dst")
	defer dropDst()
	for _, d := range []*Database{src, dst} {
		liveAddFileGroups(t, db, ctx, d, dataPath, withFS)
	}
	major := src.serverMajorVersion()
	graph := major == 0 || major >= int(SQLServer2017)
	edgeConstraints := major == 0 || major >= int(SQLServer2019)

	var tables []string // in replay order: node tables before the edges that reference them
	if graph {
		liveExecIn(t, src, ctx,
			"CREATE TABLE dbo.Person (ID int CONSTRAINT PK_Person PRIMARY KEY, name nvarchar(100)) AS NODE ON FG2",
			"CREATE INDEX IX_Person_node ON dbo.Person ($node_id DESC)",
			"CREATE TABLE dbo.City (ID int PRIMARY KEY NONCLUSTERED, name nvarchar(100) NOT NULL) AS NODE",
			"CREATE TABLE dbo.Likes (rating int) AS EDGE",
			"CREATE INDEX IX_Likes_ends ON dbo.Likes ($from_id, $to_id)",
		)
		tables = append(tables, "Person", "City", "Likes")
	}
	if edgeConstraints {
		liveExecIn(t, src, ctx,
			"CREATE TABLE dbo.LivesIn (since date, CONSTRAINT EC_LivesIn CONNECTION (dbo.Person TO dbo.City, dbo.City TO dbo.City) ON DELETE CASCADE) AS EDGE",
			"CREATE TABLE dbo.Knows (a int) AS EDGE",
			"ALTER TABLE dbo.Knows WITH NOCHECK ADD CONSTRAINT EC_Knows CONNECTION (dbo.Person TO dbo.Person)",
			"ALTER TABLE dbo.Knows NOCHECK CONSTRAINT EC_Knows",
		)
		tables = append(tables, "LivesIn", "Knows")
	}
	liveExecIn(t, src, ctx,
		`CREATE TABLE dbo.MO1 (id int NOT NULL CONSTRAINT PK_MO1 PRIMARY KEY NONCLUSTERED HASH WITH (BUCKET_COUNT = 1000),
			v nvarchar(50) NOT NULL, w int NULL,
			INDEX ix_v NONCLUSTERED (v DESC),
			INDEX ix_w HASH (w) WITH (BUCKET_COUNT = 64),
			CONSTRAINT UQ_MO1 UNIQUE NONCLUSTERED (w)
		) WITH (MEMORY_OPTIMIZED = ON, DURABILITY = SCHEMA_ONLY)`,
		`CREATE TABLE dbo.MO2 (id int NOT NULL CONSTRAINT PK_MO2 PRIMARY KEY NONCLUSTERED (id), n nvarchar(max) NULL,
			INDEX ix_u UNIQUE NONCLUSTERED (id)) WITH (MEMORY_OPTIMIZED = ON)`,
		// G5: the inline columnstore index keeps its COMPRESSION_DELAY, which
		// is 0 or at least 60 minutes on a memory-optimized table.
		`CREATE TABLE dbo.MO3 (id int NOT NULL CONSTRAINT PK_MO3 PRIMARY KEY NONCLUSTERED (id), v int NULL,
			INDEX cci_MO3 CLUSTERED COLUMNSTORE WITH (COMPRESSION_DELAY = 90 MINUTES)) WITH (MEMORY_OPTIMIZED = ON)`,
		"CREATE TABLE dbo.LOB1 (id int, notes nvarchar(max)) ON [PRIMARY] TEXTIMAGE_ON FG2",
		"CREATE TABLE dbo.LOB2 (id int PRIMARY KEY, x xml) ON FG2 TEXTIMAGE_ON [PRIMARY]",
		"CREATE TABLE dbo.LOB3 (id int, notes nvarchar(max)) ON FG2",
	)
	tables = append(tables, "MO1", "MO2", "MO3", "LOB1", "LOB2", "LOB3")
	if withFS {
		liveExecIn(t, src, ctx,
			`CREATE TABLE dbo.FS1 (id uniqueidentifier ROWGUIDCOL NOT NULL CONSTRAINT UQ_FS1 UNIQUE,
				doc varbinary(max) FILESTREAM NULL, notes nvarchar(max) NULL) ON FG2 TEXTIMAGE_ON [PRIMARY] FILESTREAM_ON FSG`,
			// The ROWGUIDCOL's constraint added after the table, and the
			// FILESTREAM column after that; filestream alone is not LOB data.
			"CREATE TABLE dbo.FS2 (id uniqueidentifier ROWGUIDCOL NOT NULL, k int)",
			"ALTER TABLE dbo.FS2 ADD CONSTRAINT UQ_FS2 UNIQUE (id)",
			"ALTER TABLE dbo.FS2 ADD doc varbinary(max) FILESTREAM NULL",
		)
		tables = append(tables, "FS1", "FS2")
	} else {
		t.Log("FILESTREAM is disabled on this instance; its tables are not replayed")
	}

	for _, verb := range []ScriptVerb{ScriptCreate, ScriptDropAndCreate} {
		opts := DefaultScriptOptions()
		opts.Verb = verb
		sc := NewScripter(src, opts)
		for _, name := range tables {
			// A node table referenced by an edge constraint cannot be
			// dropped, so DROP AND CREATE replays only the rest.
			if verb == ScriptDropAndCreate && (name == "Person" || name == "City") {
				continue
			}
			s, err := sc.ScriptTable(ctx, "dbo", name)
			if err != nil {
				t.Fatalf("ScriptTable %s: %v", name, err)
			}
			liveRunScript(t, dst, ctx, s)
		}
	}

	queries := map[string]string{
		"tables": liveKindsTablesQuery, "columns": liveKindsColumnsQuery, "indexes": liveKindsIndexesQuery,
	}
	if edgeConstraints {
		queries["edge constraints"] = liveKindsEdgeConstraintsQuery
	}
	for what, q := range queries {
		if !graph { // sys.tables and sys.columns have no graph columns before 2017
			q = strings.NewReplacer("t.is_node, t.is_edge, ", "", "ISNULL(c.graph_type_desc, c.name)", "c.name").Replace(q)
		}
		if a, b := liveRowsAsStrings(t, src, ctx, q), liveRowsAsStrings(t, dst, ctx, q); !slices.Equal(a, b) {
			t.Errorf("%s differ after replay:\nsource: %v\nreplay: %v", what, a, b)
		}
	}

	// replay scripts each named table of src under both CREATE verbs, runs
	// the scripts in dst, and compares the rows each query returns in the
	// two databases.
	replay := func(t *testing.T, names []string, queries map[string]string) {
		t.Helper()
		for _, verb := range []ScriptVerb{ScriptCreate, ScriptDropAndCreate} {
			opts := DefaultScriptOptions()
			opts.Verb = verb
			for _, name := range names {
				s, err := NewScripter(src, opts).ScriptTable(ctx, "dbo", name)
				if err != nil {
					t.Fatalf("ScriptTable %s (verb %d): %v", name, verb, err)
				}
				liveRunScript(t, dst, ctx, s)
			}
		}
		for what, q := range queries {
			if a, b := liveRowsAsStrings(t, src, ctx, q), liveRowsAsStrings(t, dst, ctx, q); !slices.Equal(a, b) {
				t.Errorf("%s differ after replay:\nsource: %v\nreplay: %v", what, a, b)
			}
		}
	}
	refused := func(t *testing.T, name string) {
		t.Helper()
		for _, verb := range []ScriptVerb{ScriptCreate, ScriptDrop, ScriptDropAndCreate} {
			opts := DefaultScriptOptions()
			opts.Verb = verb
			s, err := NewScripter(src, opts).ScriptTable(ctx, "dbo", name)
			if !errors.Is(err, ErrUnsupported) || s != "" {
				t.Errorf("ScriptTable %s (verb %d) = %q, %v; want no script and an ErrUnsupported error", name, verb, s, err)
			}
		}
	}

	// G8: the column encryption key is referenced by name, so both
	// databases have one. Neither key is usable — nothing here encrypts a
	// value — but the DDL is all a script carries.
	t.Run("always encrypted", func(t *testing.T) {
		for _, d := range []*Database{src, dst} {
			liveExecIn(t, d, ctx,
				"CREATE COLUMN MASTER KEY CMK_probe WITH (KEY_STORE_PROVIDER_NAME = 'MSSQL_CERTIFICATE_STORE', KEY_PATH = 'CurrentUser/My/0000000000000000000000000000000000000000')",
				"CREATE COLUMN ENCRYPTION KEY [CEK probe] WITH VALUES (COLUMN_MASTER_KEY = CMK_probe, ALGORITHM = 'RSA_OAEP', ENCRYPTED_VALUE = 0x01)")
		}
		liveExecIn(t, src, ctx,
			`CREATE TABLE dbo.AE (id int NOT NULL PRIMARY KEY,
				ssn char(11) COLLATE Latin1_General_BIN2
					ENCRYPTED WITH (COLUMN_ENCRYPTION_KEY = [CEK probe], ENCRYPTION_TYPE = DETERMINISTIC, ALGORITHM = 'AEAD_AES_256_CBC_HMAC_SHA_256') NOT NULL,
				note nvarchar(50)
					ENCRYPTED WITH (COLUMN_ENCRYPTION_KEY = [CEK probe], ENCRYPTION_TYPE = RANDOMIZED, ALGORITHM = 'AEAD_AES_256_CBC_HMAC_SHA_256') NULL,
				amount decimal(10,2)
					ENCRYPTED WITH (COLUMN_ENCRYPTION_KEY = [CEK probe], ENCRYPTION_TYPE = RANDOMIZED, ALGORITHM = 'AEAD_AES_256_CBC_HMAC_SHA_256'))`)
		replay(t, []string{"AE"}, map[string]string{"encrypted columns": `
SELECT c.name, TYPE_NAME(c.user_type_id), c.max_length, c.precision, c.scale, ISNULL(c.collation_name, '-'),
       c.is_nullable, ISNULL(k.name, '-'), ISNULL(c.encryption_type_desc, '-'), ISNULL(c.encryption_algorithm_name, '-')
FROM   sys.columns c LEFT JOIN sys.column_encryption_keys k ON k.column_encryption_key_id = c.column_encryption_key_id
WHERE  c.object_id = OBJECT_ID('dbo.AE')
ORDER  BY c.column_id`})
	})

	// G7: both ledger kinds, with a ledger view in its own schema and
	// renamed columns, and a dropped column the ledger keeps. A dropped
	// ledger table and a history table are made only by the ledger, so
	// those two are still refused.
	t.Run("ledger", func(t *testing.T) {
		if major != 0 && major < int(SQLServer2022) {
			t.Skip("ledger tables are SQL Server 2022 and later")
		}
		for _, d := range []*Database{src, dst} {
			liveExecIn(t, d, ctx, "CREATE SCHEMA lv")
		}
		liveExecIn(t, src, ctx,
			`CREATE TABLE dbo.LA (id int NOT NULL, v nvarchar(10) NULL)
				WITH (LEDGER = ON (LEDGER_VIEW = lv.LA_V (TRANSACTION_ID_COLUMN_NAME = tx, SEQUENCE_NUMBER_COLUMN_NAME = [seq no],
					OPERATION_TYPE_COLUMN_NAME = op, OPERATION_TYPE_DESC_COLUMN_NAME = op_desc), APPEND_ONLY = ON))`,
			`CREATE TABLE dbo.LU (id int NOT NULL CONSTRAINT PK_LU PRIMARY KEY, v nvarchar(10) NULL)
				WITH (SYSTEM_VERSIONING = ON (HISTORY_TABLE = dbo.LU_Hist), LEDGER = ON)`,
			"ALTER TABLE dbo.LU ADD w int NULL",
			"ALTER TABLE dbo.LU DROP COLUMN w",
			// Added after the drop: the view's ledger columns stay its last four.
			"ALTER TABLE dbo.LU ADD z int NULL",
			"CREATE TABLE dbo.LX (id int) WITH (LEDGER = ON (APPEND_ONLY = ON))",
			"DROP TABLE dbo.LX",
		)
		var dropped string
		if err := src.queryRow(ctx, func(r *sql.Row) error { return r.Scan(&dropped) },
			"SELECT name FROM sys.tables WHERE is_dropped_ledger_table = 1"); err != nil {
			t.Fatalf("dropped ledger table: %v", err)
		}
		// A DROP AND CREATE leaves the replayed copy's first incarnation
		// behind as a dropped ledger table, so those are left out, as is the
		// ledger's record of the dropped column: the history table and the
		// ledger view keep it, and no CREATE can.
		replay(t, []string{"LA", "LU"}, map[string]string{
			"ledger tables": `
SELECT t.name, t.ledger_type_desc, ISNULL(OBJECT_SCHEMA_NAME(t.ledger_view_id) + '.' + OBJECT_NAME(t.ledger_view_id), '-'),
       ISNULL(OBJECT_NAME(t.history_table_id), '-'), t.temporal_type
FROM   sys.tables t WHERE t.is_dropped_ledger_table = 0 AND t.name IN ('LA', 'LU', 'LU_Hist')
ORDER  BY t.name`,
			"ledger columns": `
SELECT OBJECT_NAME(c.object_id), c.name, TYPE_NAME(c.user_type_id), c.is_nullable, c.generated_always_type_desc, c.is_hidden
FROM   sys.columns c JOIN sys.tables t ON t.object_id = c.object_id
WHERE  t.is_dropped_ledger_table = 0 AND t.name IN ('LA', 'LU', 'LU_Hist') AND c.name NOT LIKE 'MSSQL[_]DroppedLedgerColumn[_]%'
ORDER  BY OBJECT_NAME(c.object_id), c.column_id`,
			"ledger views": `
SELECT OBJECT_NAME(c.object_id), c.name
FROM   sys.columns c JOIN sys.tables t ON c.object_id = t.ledger_view_id
WHERE  t.is_dropped_ledger_table = 0 AND t.name IN ('LA', 'LU') AND c.name NOT LIKE 'MSSQL[_]DroppedLedgerColumn[_]%'
ORDER  BY OBJECT_NAME(c.object_id), c.column_id`,
		})
		refused(t, "LU_Hist")
		refused(t, dropped)
	})

	// G9: FileTables need non-transacted FILESTREAM access, and a directory
	// name that is unique on the instance.
	t.Run("filetable", func(t *testing.T) {
		if !withFS {
			t.Skip("FILESTREAM is disabled on this instance")
		}
		for _, d := range []*Database{src, dst} {
			if _, err := db.ExecContext(ctx, "ALTER DATABASE ["+d.Name+"] SET FILESTREAM (NON_TRANSACTED_ACCESS = FULL, DIRECTORY_NAME = N'"+d.Name+"')"); err != nil {
				t.Fatalf("non-transacted access on %s: %v", d.Name, err)
			}
		}
		liveExecIn(t, src, ctx,
			"CREATE TABLE dbo.FT AS FILETABLE ON FG2 FILESTREAM_ON FSG WITH (FILETABLE_DIRECTORY = N'ft ''dir''', FILETABLE_COLLATE_FILENAME = Latin1_General_CI_AS)",
			"CREATE INDEX IX_FT_name ON dbo.FT (name)",
			"ALTER TABLE dbo.FT ADD CONSTRAINT CK_FT_small CHECK (cached_file_size < 1000000)",
			"CREATE TABLE dbo.FT2 AS FILETABLE",
			"ALTER TABLE dbo.FT2 DISABLE FILETABLE_NAMESPACE",
		)
		replay(t, []string{"FT", "FT2"}, map[string]string{
			"filetables": `
SELECT OBJECT_NAME(f.object_id), f.is_enabled, f.directory_name, f.filename_collation_name,
       ISNULL(ds.name, '-'), ISNULL(fds.name, '-'),
       (SELECT COUNT(*) FROM sys.filetable_system_defined_objects so WHERE so.parent_object_id = f.object_id)
FROM   sys.filetables f JOIN sys.tables t ON t.object_id = f.object_id
LEFT   JOIN sys.indexes i ON i.object_id = t.object_id AND i.index_id IN (0, 1)
LEFT   JOIN sys.data_spaces ds ON ds.data_space_id = i.data_space_id
LEFT   JOIN sys.data_spaces fds ON fds.data_space_id = t.filestream_data_space_id
ORDER  BY 1`,
			"filetable indexes": `
SELECT OBJECT_NAME(i.object_id), i.name, i.type_desc, i.is_unique, i.is_primary_key, i.is_unique_constraint,
       (SELECT c.name AS c FROM sys.index_columns ic JOIN sys.columns c ON c.object_id = ic.object_id AND c.column_id = ic.column_id
        WHERE ic.object_id = i.object_id AND ic.index_id = i.index_id ORDER BY ic.key_ordinal FOR JSON PATH)
FROM   sys.indexes i JOIN sys.filetables f ON f.object_id = i.object_id
WHERE  i.type > 0
ORDER  BY 1, 2`,
			"user check constraints": `
SELECT OBJECT_NAME(ck.parent_object_id), ck.name, ck.definition
FROM   sys.check_constraints ck JOIN sys.filetables f ON f.object_id = ck.parent_object_id
WHERE  ck.object_id NOT IN (SELECT object_id FROM sys.filetable_system_defined_objects)
ORDER  BY 1, 2`,
		})
	})
}
