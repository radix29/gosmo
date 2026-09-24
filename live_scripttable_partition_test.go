//go:build livedb

// Live verification that a scripted table keeps its storage: the partition
// scheme it is on, and the filegroup when that is not the default one.
//
// Only a live server settles this. ScriptTable emitted no ON clause at all,
// so a partitioned table's script recreated it on the target's default
// filegroup — a table that is silently unpartitioned, which the script itself
// runs cleanly to produce. The test scripts from one database and replays into
// another, then reads sys.data_spaces back: the assertion is on where the rows
// actually landed, not on the text.
//
//	go test -tags livedb . -run TestLiveScriptTablePartition -v \
//	  -livedb 'sqlserver://sa:PASS@host?TrustServerCertificate=true'
//
// Creates and drops two throwaway databases; touches nothing else.
package gosmo

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

// liveStorage returns the data space name and type ('FG'/'PS') the table's
// heap or clustered index is on, read straight from the catalog rather than
// through gosmo — the scripter is what is under test, so the check must not
// share code with it.
func liveStorage(t *testing.T, d *Database, ctx context.Context, schema, name string) (string, string) {
	t.Helper()
	const q = `
SELECT ds.name, ds.type
FROM   sys.indexes i
JOIN   sys.data_spaces ds ON ds.data_space_id = i.data_space_id
WHERE  i.object_id = OBJECT_ID(@p1) AND i.index_id IN (0, 1)`
	var dsName, dsType string
	if err := d.queryRow(ctx, func(row *sql.Row) error {
		return row.Scan(&dsName, &dsType)
	}, q, schema+"."+name); err != nil {
		t.Fatalf("storage of %s.%s in %s: %v", schema, name, d.Name, err)
	}
	return dsName, strings.TrimSpace(dsType)
}

// livePartitionSetup creates the filegroup, partition function and partition
// scheme both databases need — the source to put tables on, the target to
// have somewhere for the replayed script to land.
func livePartitionSetup(t *testing.T, d *Database, ctx context.Context) {
	t.Helper()
	liveExecIn(t, d, ctx,
		`ALTER DATABASE [`+d.Name+`] ADD FILEGROUP FG_Archive`,
		`ALTER DATABASE [`+d.Name+`] ADD FILE (NAME = N'`+d.Name+`_arch', FILENAME = N'`+
			liveDatabaseFileDir(t, d, ctx)+d.Name+`_arch.ndf', SIZE = 8MB) TO FILEGROUP FG_Archive`,
		`CREATE PARTITION FUNCTION pf_year (INT) AS RANGE RIGHT FOR VALUES (2000, 2010)`,
		`CREATE PARTITION SCHEME ps_year AS PARTITION pf_year ALL TO ([PRIMARY])`,
	)
}

// liveDatabaseFileDir returns the directory the database's own primary file
// is in, so anything a test writes on the server — an added file, a backup —
// lands beside it whatever the server's OS and default data path are.
func liveDatabaseFileDir(t *testing.T, d *Database, ctx context.Context) string {
	t.Helper()
	var path string
	if err := d.queryRow(ctx, func(row *sql.Row) error {
		return row.Scan(&path)
	}, `SELECT TOP 1 physical_name FROM sys.database_files WHERE type = 0`); err != nil {
		t.Fatalf("primary file path of %s: %v", d.Name, err)
	}
	sep := "\\"
	if !strings.Contains(path, sep) {
		sep = "/"
	}
	return path[:strings.LastIndex(path, sep)+1]
}

func TestLiveScriptTablePartitionSurvivesTheRoundTrip(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()

	src, dropSrc := liveScratchDB(t, db, ctx, "gosmo_partscript_src")
	defer dropSrc()
	dst, dropDst := liveScratchDB(t, db, ctx, "gosmo_partscript_dst")
	defer dropDst()

	livePartitionSetup(t, src, ctx)
	livePartitionSetup(t, dst, ctx)

	liveExecIn(t, src, ctx,
		`CREATE TABLE dbo.Parted (ID INT NOT NULL, Yr INT NOT NULL, Note NVARCHAR(40) NULL,
		    CONSTRAINT PK_Parted PRIMARY KEY CLUSTERED (ID, Yr) ON ps_year(Yr))
		 ON ps_year(Yr)`,
		`CREATE NONCLUSTERED INDEX IX_Parted_Note ON dbo.Parted (Note) ON ps_year(Yr)`,
		// A heap: it has no row in the index list at all, so its scheme is
		// only reachable through Table.DataSpace.
		`CREATE TABLE dbo.PartedHeap (ID INT NOT NULL, Yr INT NOT NULL) ON ps_year(Yr)`,
		`CREATE TABLE dbo.OnArchive (ID INT NOT NULL) ON FG_Archive`,
		// G3: PAGE on partition 1 and NONE on the others, on a clustered
		// index, a nonclustered one and a heap — each carries its own
		// per-partition compression, and the script wrote only the first's.
		`CREATE TABLE dbo.MixedComp (ID INT NOT NULL, Yr INT NOT NULL, Note NVARCHAR(40) NULL,
		    CONSTRAINT PK_MixedComp PRIMARY KEY CLUSTERED (ID, Yr)
		    WITH (DATA_COMPRESSION = PAGE ON PARTITIONS (1)) ON ps_year(Yr))
		 ON ps_year(Yr)`,
		`CREATE NONCLUSTERED INDEX IX_MixedComp_Note ON dbo.MixedComp (Note)
		 WITH (DATA_COMPRESSION = ROW ON PARTITIONS (2 TO 3)) ON ps_year(Yr)`,
		`CREATE TABLE dbo.MixedHeap (ID INT NOT NULL, Yr INT NOT NULL)
		 ON ps_year(Yr) WITH (DATA_COMPRESSION = PAGE ON PARTITIONS (1))`,
		// Neither index names the partitioning column, so the server adds it
		// to each: sys.index_columns lists Yr with key_ordinal 0 and
		// partition_ordinal 1, and the script wrote it as the first key
		// column — CX (Yr, ID), a different index.
		`CREATE TABLE dbo.ImplicitPart (ID INT NOT NULL, Yr INT NOT NULL, Note NVARCHAR(40) NULL) ON ps_year(Yr)`,
		`CREATE CLUSTERED INDEX CX_ImplicitPart ON dbo.ImplicitPart (ID) ON ps_year(Yr)`,
		`CREATE NONCLUSTERED INDEX IX_ImplicitPart_Note ON dbo.ImplicitPart (Note) INCLUDE (ID) ON ps_year(Yr)`,
	)

	cases := []struct {
		table    string
		wantName string
		wantType string
	}{
		{"Parted", "ps_year", "PS"},
		{"PartedHeap", "ps_year", "PS"},
		{"OnArchive", "FG_Archive", "FG"},
		{"MixedComp", "ps_year", "PS"},
		{"MixedHeap", "ps_year", "PS"},
		{"ImplicitPart", "ps_year", "PS"},
	}

	opts := DefaultScriptOptions()
	opts.IncludeHeaders = false
	sc := NewScripter(src, opts)

	for _, c := range cases {
		t.Run(c.table, func(t *testing.T) {
			script, err := sc.ScriptTable(ctx, "dbo", c.table)
			if err != nil {
				t.Fatalf("ScriptTable: %v", err)
			}
			for _, batch := range splitBatches(script) {
				if strings.TrimSpace(batch) == "" {
					continue
				}
				if _, err := dst.exec(ctx, batch); err != nil {
					t.Fatalf("replaying into %s: %v\n--- batch ---\n%s\n--- whole script ---\n%s",
						dst.Name, err, batch, script)
				}
			}
			gotName, gotType := liveStorage(t, dst, ctx, "dbo", c.table)
			if gotName != c.wantName || gotType != c.wantType {
				t.Errorf("replayed dbo.%s is on %s (%s), want %s (%s)\n%s",
					c.table, gotName, gotType, c.wantName, c.wantType, script)
			}
		})
	}

	// The nonclustered index carries its own ON clause, and it is not the
	// table's: an index can be on a different data space entirely.
	var idxSpace string
	if err := dst.queryRow(ctx, func(row *sql.Row) error {
		return row.Scan(&idxSpace)
	}, `SELECT ds.name FROM sys.indexes i
	    JOIN sys.data_spaces ds ON ds.data_space_id = i.data_space_id
	    WHERE i.object_id = OBJECT_ID('dbo.Parted') AND i.name = 'IX_Parted_Note'`); err != nil {
		t.Fatalf("storage of IX_Parted_Note: %v", err)
	}
	if idxSpace != "ps_year" {
		t.Errorf("replayed IX_Parted_Note is on %q, want ps_year", idxSpace)
	}

	// Every index keeps its declared key and included columns, and no more.
	for _, table := range []string{"Parted", "MixedComp", "ImplicitPart"} {
		want := liveIndexKeys(t, src, ctx, table)
		got := liveIndexKeys(t, dst, ctx, table)
		if want != got {
			t.Errorf("replayed dbo.%s index columns = %s, want %s", table, got, want)
		}
	}

	// G3: every partition of every heap and index keeps its compression.
	// Read from sys.partitions directly, for the same reason liveStorage is.
	for _, table := range []string{"MixedComp", "MixedHeap"} {
		want := livePartitionCompression(t, src, ctx, table)
		got := livePartitionCompression(t, dst, ctx, table)
		if want != got {
			t.Errorf("replayed dbo.%s compression per index/partition = %s, want %s", table, got, want)
		}
		if !strings.Contains(want, "PAGE") || !strings.Contains(want, "NONE") {
			t.Errorf("dbo.%s source is not mixed (%s): the case tests nothing", table, want)
		}
	}
}

// livePartitionCompression renders each (index, partition)'s compression of
// dbo.table as one comparable string, index 0 or 1 standing for the table
// itself and every other index named.
func livePartitionCompression(t *testing.T, d *Database, ctx context.Context, table string) string {
	t.Helper()
	const q = `
SELECT ISNULL(i.name, '(heap)'), p.partition_number, p.data_compression_desc
FROM   sys.partitions p
JOIN   sys.indexes i ON i.object_id = p.object_id AND i.index_id = p.index_id
WHERE  p.object_id = OBJECT_ID(@p1)
ORDER  BY p.index_id, p.partition_number`
	rows, err := d.query(ctx, q, "dbo."+table)
	if err != nil {
		t.Fatalf("compression of dbo.%s in %s: %v", table, d.Name, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name, comp string
		var n int
		if err := rows.Scan(&name, &n, &comp); err != nil {
			t.Fatalf("compression of dbo.%s in %s: %v", table, d.Name, err)
		}
		out = append(out, fmt.Sprintf("%s:%d=%s", name, n, comp))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("compression of dbo.%s in %s: %v", table, d.Name, err)
	}
	return strings.Join(out, " ")
}

// liveIndexKeys renders the declared columns of every index on dbo.table —
// key columns in key order, then included ones — as one comparable string.
// The partitioning column the server adds on its own (key_ordinal 0, not
// included) is left out, as a CREATE INDEX never names it.
func liveIndexKeys(t *testing.T, d *Database, ctx context.Context, table string) string {
	t.Helper()
	const q = `
SELECT i.name, c.name, ic.key_ordinal, ic.is_included_column, ic.is_descending_key
FROM   sys.indexes i
JOIN   sys.index_columns ic ON ic.object_id = i.object_id AND ic.index_id = i.index_id
JOIN   sys.columns c ON c.object_id = ic.object_id AND c.column_id = ic.column_id
WHERE  i.object_id = OBJECT_ID(@p1) AND (ic.key_ordinal > 0 OR ic.is_included_column = 1)
ORDER  BY i.name, ic.is_included_column, ic.key_ordinal, c.name`
	rows, err := d.query(ctx, q, "dbo."+table)
	if err != nil {
		t.Fatalf("index columns of dbo.%s in %s: %v", table, d.Name, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var idx, col string
		var ord int
		var inc, desc bool
		if err := rows.Scan(&idx, &col, &ord, &inc, &desc); err != nil {
			t.Fatalf("index columns of dbo.%s in %s: %v", table, d.Name, err)
		}
		out = append(out, fmt.Sprintf("%s:%s/%d/%t/%t", idx, col, ord, inc, desc))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("index columns of dbo.%s in %s: %v", table, d.Name, err)
	}
	return strings.Join(out, " ")
}
