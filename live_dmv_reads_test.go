//go:build livedb

// Live verification of the DMV reads fixed in the 2026-10-02 review (T5,
// T11, T25 in gossms's docs/review-plan-2026-10-02.md). Only the server shows
// which rows sys.dm_db_index_physical_stats returns for a partitioned index
// with a LOB column, and that sys.dm_db_stats_properties returns nothing to a
// principal holding VIEW DEFINITION alone.
//
//	go test -tags livedb . -run TestLiveDMVReads -v \
//	  -livedb 'sqlserver://sa:PASS@host?TrustServerCertificate=true'
//
// Creates and drops its own throwaway database and login; touches nothing
// else.
package gosmo

import (
	"database/sql"
	"net/url"
	"testing"
)

func TestLiveDMVReads(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()

	const dbName = "gosmo_dmvreads_live"
	d, drop := liveScratchDB(t, db, ctx, dbName)
	defer drop()

	// Two partitions, each with an IN_ROW_DATA and a LOB_DATA unit: the DMV
	// returns four leaf rows for PK_W, and the LOB ones report 0%
	// fragmentation over ~170x the pages.
	liveExecIn(t, d, ctx,
		`CREATE PARTITION FUNCTION pf_w (INT) AS RANGE RIGHT FOR VALUES (500)`,
		`CREATE PARTITION SCHEME ps_w AS PARTITION pf_w ALL TO ([PRIMARY])`,
		`CREATE TABLE dbo.W (ID INT NOT NULL, Big NVARCHAR(MAX) NULL,
		    CONSTRAINT PK_W PRIMARY KEY CLUSTERED (ID) ON ps_w (ID))`,
		`INSERT dbo.W (ID, Big)
		 SELECT TOP (1000) ROW_NUMBER() OVER (ORDER BY (SELECT NULL)),
		        REPLICATE(CONVERT(NVARCHAR(MAX), N'x'), 5000)
		 FROM sys.all_objects a CROSS JOIN sys.all_objects b`,
		`CREATE INDEX IX_W_Big ON dbo.W (ID) INCLUDE (Big)`,
		`CREATE TABLE dbo.CS (ID INT NOT NULL, V INT NULL)`,
		`INSERT dbo.CS (ID, V) SELECT TOP (2000) ROW_NUMBER() OVER (ORDER BY (SELECT NULL)), 1
		 FROM sys.all_objects a CROSS JOIN sys.all_objects b`,
		`CREATE CLUSTERED COLUMNSTORE INDEX CCI ON dbo.CS`,
	)

	var inRowPages int64
	if err := db.QueryRowContext(ctx, `SELECT SUM(page_count) FROM sys.dm_db_index_physical_stats(
		DB_ID(N'`+dbName+`'), OBJECT_ID(N'`+dbName+`.dbo.W'), 1, NULL, 'SAMPLED')
		WHERE alloc_unit_type_desc = N'IN_ROW_DATA' AND index_level = 0`).Scan(&inRowPages); err != nil {
		t.Fatalf("in-row page count: %v", err)
	}

	t.Run("Index.Fragmentation folds partitions and skips LOB", func(t *testing.T) {
		f, err := d.TableRef("dbo", "W").IndexRef("PK_W").Fragmentation(ctx, FragmentationSampled)
		if err != nil {
			t.Fatalf("Fragmentation: %v", err)
		}
		if f.PageCount != inRowPages {
			t.Errorf("PageCount = %d, want the leaf in-row pages summed over both partitions, %d", f.PageCount, inRowPages)
		}
		if f.AvgPageSpaceUsedPct <= 0 {
			t.Errorf("AvgPageSpaceUsedPct = %v, want a SAMPLED density", f.AvgPageSpaceUsedPct)
		}
	})

	t.Run("Index.Fragmentation of a columnstore reports zeros", func(t *testing.T) {
		f, err := d.TableRef("dbo", "CS").IndexRef("CCI").Fragmentation(ctx, FragmentationSampled)
		if err != nil {
			t.Fatalf("Fragmentation: %v", err)
		}
		if f.IndexName != "CCI" {
			t.Errorf("IndexName = %q, want CCI", f.IndexName)
		}
	})

	t.Run("Table.FragmentationStats is one row per index", func(t *testing.T) {
		for _, mode := range []FragmentationMode{FragmentationLimited, FragmentationSampled, FragmentationDetailed} {
			stats, err := d.TableRef("dbo", "W").FragmentationStats(ctx, mode)
			if err != nil {
				t.Fatalf("%s: FragmentationStats: %v", mode, err)
			}
			seen := map[string]int{}
			for _, f := range stats {
				seen[f.IndexName]++
				if f.IndexName == "PK_W" && f.PageCount != inRowPages {
					t.Errorf("%s: PK_W PageCount = %d, want %d", mode, f.PageCount, inRowPages)
				}
			}
			if len(stats) != 2 || seen["PK_W"] != 1 || seen["IX_W_Big"] != 1 {
				t.Errorf("%s: FragmentationStats = %v, want PK_W and IX_W_Big once each", mode, seen)
			}
		}
	})

	t.Run("Table.FragmentationStats of a missing table is empty", func(t *testing.T) {
		stats, err := d.TableRef("dbo", "NoSuchTable").FragmentationStats(ctx, FragmentationLimited)
		if err != nil {
			t.Fatalf("FragmentationStats: %v", err)
		}
		if len(stats) != 0 {
			t.Errorf("FragmentationStats of a missing table returned %d rows — other tables' indexes", len(stats))
		}
	})

	t.Run("Index.StorageInfo's record size is the in-row one", func(t *testing.T) {
		info, err := d.TableRef("dbo", "W").IndexRef("PK_W").StorageInfo(ctx)
		if err != nil {
			t.Fatalf("StorageInfo: %v", err)
		}
		// In-row records here are ~51 bytes (an int plus a LOB pointer); a
		// LOB unit's record reports ~5000.
		if info.AvgRecordSize <= 0 || info.AvgRecordSize > 200 {
			t.Errorf("AvgRecordSize = %v, want the in-row record size", info.AvgRecordSize)
		}
	})

	t.Run("Table.Statistics with VIEW DEFINITION only", func(t *testing.T) {
		const login, pass = "gosmo_dmvreads_viewdef", "Vd!9xQ#2pLk7"
		if _, err := db.ExecContext(ctx, "CREATE LOGIN "+login+" WITH PASSWORD = N'"+pass+"', CHECK_POLICY = OFF"); err != nil {
			t.Fatalf("create login: %v", err)
		}
		defer db.ExecContext(ctx, "DROP LOGIN "+login)
		liveExecIn(t, d, ctx,
			"CREATE USER "+login+" FOR LOGIN "+login,
			"GRANT VIEW DEFINITION ON dbo.W TO "+login,
		)
		defer liveExecIn(t, d, ctx, "DROP USER "+login)

		admin, err := d.TableByName(ctx, "dbo", "W")
		if err != nil {
			t.Fatalf("TableByName: %v", err)
		}
		want, err := admin.Statistics(ctx)
		if err != nil {
			t.Fatalf("Statistics as sa: %v", err)
		}

		u, err := url.Parse(*liveDSN)
		if err != nil {
			t.Fatalf("parse DSN: %v", err)
		}
		u.User = url.UserPassword(login, pass)
		ldb, err := sql.Open("sqlserver", u.String())
		if err != nil {
			t.Fatalf("open as %s: %v", login, err)
		}
		// The login must be gone from the server before DROP LOGIN runs.
		defer ldb.Close()
		lsrv := liveServer(t, ldb, ctx)
		tbl := lsrv.DatabaseRef(dbName).TableRef("dbo", "W")
		tbl.ObjectID = admin.ObjectID
		got, err := tbl.Statistics(ctx)
		if err != nil {
			t.Fatalf("Statistics as %s: %v", login, err)
		}
		if len(got) != len(want) || len(want) == 0 {
			t.Errorf("Statistics as %s returned %d, sa sees %d — CROSS APPLY hid the unreadable ones", login, len(got), len(want))
		}
	})
}
