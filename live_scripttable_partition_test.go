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
		t.Fatalf("storage of %s.%s in %s: %v", schema, name, d.name, err)
	}
	return dsName, strings.TrimSpace(dsType)
}

// livePartitionSetup creates the filegroup, partition function and partition
// scheme both databases need — the source to put tables on, the target to
// have somewhere for the replayed script to land.
func livePartitionSetup(t *testing.T, d *Database, ctx context.Context) {
	t.Helper()
	liveExecIn(t, d, ctx,
		`ALTER DATABASE [`+d.name+`] ADD FILEGROUP FG_Archive`,
		`ALTER DATABASE [`+d.name+`] ADD FILE (NAME = N'`+d.name+`_arch', FILENAME = N'`+
			liveDatabaseFileDir(t, d, ctx)+d.name+`_arch.ndf', SIZE = 8MB) TO FILEGROUP FG_Archive`,
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
		t.Fatalf("primary file path of %s: %v", d.name, err)
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
		// only reachable through Table.DataSpaceContext.
		`CREATE TABLE dbo.PartedHeap (ID INT NOT NULL, Yr INT NOT NULL) ON ps_year(Yr)`,
		`CREATE TABLE dbo.OnArchive (ID INT NOT NULL) ON FG_Archive`,
	)

	cases := []struct {
		table    string
		wantName string
		wantType string
	}{
		{"Parted", "ps_year", "PS"},
		{"PartedHeap", "ps_year", "PS"},
		{"OnArchive", "FG_Archive", "FG"},
	}

	opts := DefaultScriptOptions()
	opts.IncludeHeaders = false
	sc := NewScripter(src, opts)

	for _, c := range cases {
		t.Run(c.table, func(t *testing.T) {
			script, err := sc.ScriptTableContext(ctx, "dbo", c.table)
			if err != nil {
				t.Fatalf("ScriptTableContext: %v", err)
			}
			for _, batch := range splitBatches(script) {
				if strings.TrimSpace(batch) == "" {
					continue
				}
				if _, err := dst.exec(ctx, batch); err != nil {
					t.Fatalf("replaying into %s: %v\n--- batch ---\n%s\n--- whole script ---\n%s",
						dst.name, err, batch, script)
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
}
