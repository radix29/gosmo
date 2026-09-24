//go:build livedb

// Live verification of ScriptTable for the table kinds beyond a plain
// disk-based table: graph node and edge tables with their edge constraints,
// memory-optimized tables, TEXTIMAGE_ON and FILESTREAM — and of the refusal
// for the kinds it cannot express.
//
// Each kind is replayed into a second database and the two catalogs are
// compared, since a script is only proven by running it.
//
//	go test -tags livedb . -run TestLiveScriptTableKinds -v \
//	  -livedb 'sqlserver://sa:PASS@host?TrustServerCertificate=true'
//
// Creates and drops two throwaway databases; touches nothing else. The
// FILESTREAM part is skipped on an instance with FILESTREAM disabled, and
// the ledger refusal before SQL Server 2022.
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
       ISNULL(h.bucket_count, 0), ISNULL(ds.name, '-'),
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
		"CREATE TABLE dbo.LOB1 (id int, notes nvarchar(max)) ON [PRIMARY] TEXTIMAGE_ON FG2",
		"CREATE TABLE dbo.LOB2 (id int PRIMARY KEY, x xml) ON FG2 TEXTIMAGE_ON [PRIMARY]",
		"CREATE TABLE dbo.LOB3 (id int, notes nvarchar(max)) ON FG2",
	)
	tables = append(tables, "MO1", "MO2", "LOB1", "LOB2", "LOB3")
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

	t.Run("refused kinds", func(t *testing.T) {
		liveExecIn(t, src, ctx,
			"CREATE COLUMN MASTER KEY CMK_probe WITH (KEY_STORE_PROVIDER_NAME = 'MSSQL_CERTIFICATE_STORE', KEY_PATH = 'CurrentUser/My/0000000000000000000000000000000000000000')",
			"CREATE COLUMN ENCRYPTION KEY CEK_probe WITH VALUES (COLUMN_MASTER_KEY = CMK_probe, ALGORITHM = 'RSA_OAEP', ENCRYPTED_VALUE = 0x01)",
			`CREATE TABLE dbo.AE (id int, ssn char(11) COLLATE Latin1_General_BIN2
				ENCRYPTED WITH (COLUMN_ENCRYPTION_KEY = CEK_probe, ENCRYPTION_TYPE = DETERMINISTIC, ALGORITHM = 'AEAD_AES_256_CBC_HMAC_SHA_256'))`,
		)
		refused := []string{"AE"}
		if major == 0 || major >= int(SQLServer2022) {
			liveExecIn(t, src, ctx, "CREATE TABLE dbo.Ledger (id int) WITH (LEDGER = ON (APPEND_ONLY = ON))")
			refused = append(refused, "Ledger")
		}
		for _, verb := range []ScriptVerb{ScriptCreate, ScriptDrop, ScriptDropAndCreate} {
			opts := DefaultScriptOptions()
			opts.Verb = verb
			for _, name := range refused {
				s, err := NewScripter(src, opts).ScriptTable(ctx, "dbo", name)
				if !errors.Is(err, ErrUnsupported) || s != "" {
					t.Errorf("ScriptTable %s (verb %d) = %q, %v; want no script and an ErrUnsupported error", name, verb, s, err)
				}
			}
		}
	})
}
