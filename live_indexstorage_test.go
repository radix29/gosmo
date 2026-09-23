//go:build livedb

// Live verification of Index.StorageInfo's row count on a table whose rows
// span several allocation units. Summing sys.partitions.rows over the
// allocation-unit join counted each partition once per unit, so a table with
// LOB and row-overflow columns reported three times its rows — a unit test of
// the query text cannot see that; only the server's own catalog can.
//
//	go test -tags livedb . -run TestLiveIndexStorageInfo -v \
//	  -livedb 'sqlserver://sa:PASS@host?TrustServerCertificate=true'
//
// Creates and drops its own throwaway database; touches nothing else.
package gosmo

import "testing"

func TestLiveIndexStorageInfoCountsRowsOncePerPartition(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()

	d, drop := liveScratchDB(t, db, ctx, "gosmo_idxstorage_live")
	defer drop()

	// IN_ROW_DATA, LOB_DATA (nvarchar(max)) and ROW_OVERFLOW_DATA (two
	// varchar(8000) that cannot both fit in-row) — three allocation units.
	liveExecIn(t, d, ctx,
		`CREATE TABLE dbo.Wide (
		    ID INT NOT NULL CONSTRAINT PK_Wide PRIMARY KEY CLUSTERED,
		    Big NVARCHAR(MAX) NULL,
		    V1 VARCHAR(8000) NULL,
		    V2 VARCHAR(8000) NULL)`,
		`INSERT dbo.Wide (ID, Big, V1, V2)
		 SELECT TOP (1000) ROW_NUMBER() OVER (ORDER BY (SELECT NULL)),
		        REPLICATE(CONVERT(NVARCHAR(MAX), N'x'), 5000),
		        REPLICATE('a', 5000), REPLICATE('b', 5000)
		 FROM sys.all_objects a CROSS JOIN sys.all_objects b`,
	)

	tbl, err := d.TableByName(ctx, "dbo", "Wide")
	if err != nil {
		t.Fatalf("TableByName Wide: %v", err)
	}
	idx, err := tbl.IndexByName(ctx, "PK_Wide")
	if err != nil {
		t.Fatalf("IndexByName PK_Wide: %v", err)
	}
	info, err := idx.StorageInfo(ctx)
	if err != nil {
		t.Fatalf("StorageInfo: %v", err)
	}
	if len(info.Allocations) < 3 {
		t.Fatalf("allocation units = %+v, want IN_ROW, LOB and ROW_OVERFLOW — the case this test exists for", info.Allocations)
	}
	if info.RowCount != 1000 {
		t.Errorf("RowCount = %d, want 1000", info.RowCount)
	}
	if info.UsedKB <= 0 || info.ReservedKB < info.UsedKB {
		t.Errorf("UsedKB/ReservedKB = %d/%d, want positive with reserved >= used", info.UsedKB, info.ReservedKB)
	}
}
