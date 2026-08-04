// Command maintain demonstrates the routine-maintenance side of gosmo:
// index fragmentation and rebuilds, statistics with their header, histogram
// and density vector, database files and filegroups, space usage, database
// options and scoped configurations, change tracking, and Query Store.
//
// It works on a throwaway database it creates and drops.
//
//	MSSQL_SERVER=localhost:1433 MSSQL_USER=sa MSSQL_PASSWORD=YourPw go run ./examples/maintain
package main

import (
	"fmt"
	"math/rand/v2"

	gosmo "github.com/radix29/gosmo"
	"github.com/radix29/gosmo/examples/internal/demo"
)

const dbName = "GoSMOMaintainDemo"

func main() {
	// First, so it runs after the cleanup deferred below it.
	defer demo.Exit()

	srv := demo.Connect()
	defer srv.Close()

	db, drop := demo.TempDatabase(srv, dbName)
	defer drop()

	// -- Files and filegroups ---------------------------------------------
	//
	// Files() reports every file including the log; FileGroups() only sees
	// files that belong to a filegroup, so the log is absent from it.
	demo.Section("Files as created")
	for _, f := range demo.Value(db.Files()) {
		fmt.Printf("  %-20s %-5s %-10s %6d MB  growth=%s\n",
			f.Name, f.Type, f.FileGroup, f.SizeKB/1024, growth(f))
	}

	demo.Section("Adding a filegroup and a file")
	demo.Must(db.AddFileGroup("ARCHIVE"))
	dataDir := srv.Info().DefaultDataPath
	demo.Must(db.AddFile(gosmo.DatabaseFileSpec{
		Name:      dbName + "_archive",
		FileGroup: "ARCHIVE",
		Path:      demo.ServerPath(dataDir, dbName+"_archive.ndf"),
		SizeKB:    8 * 1024,
		GrowthKB:  4 * 1024,
		MaxSizeKB: -1, // UNLIMITED
	}))
	// Zero-valued FileModify fields are left alone; this only resizes.
	demo.Must(db.AlterFile(dbName+"_archive", gosmo.FileModify{SizeKB: 16 * 1024}))
	for _, fg := range demo.Value(db.FileGroups()) {
		fmt.Printf("  filegroup %-10s default=%-5t readonly=%-5t files=%d\n",
			fg.Name, fg.IsDefault, fg.IsReadOnly, len(fg.Files))
	}

	// -- A table with enough rows to fragment ------------------------------
	demo.Must(db.CreateTable(gosmo.CreateTableRequest{
		Schema: "dbo",
		Name:   "Ledger",
		Columns: []gosmo.ColumnDefinition{
			{Name: "LedgerID", DataType: gosmo.DataTypeInt, IsIdentity: true, IdentitySeed: 1, IdentityIncr: 1, IsPrimaryKey: true},
			{Name: "Account", DataType: gosmo.DataTypeNVarChar, MaxLength: 40, IsNullable: false},
			{Name: "Amount", DataType: gosmo.DataTypeDecimal, Precision: 18, Scale: 2, IsNullable: false},
			{Name: "Memo", DataType: gosmo.DataTypeNVarChar, MaxLength: 400, IsNullable: true},
		},
	}))
	tbl := demo.Value(db.TableByName("dbo", "Ledger"))

	// Random account names in random order is what actually fragments the
	// nonclustered index below — sequential data would fill pages neatly.
	accounts := []string{"cash", "receivable", "payable", "equity", "revenue", "expense"}
	rows := func(yield func([]any, error) bool) {
		for i := range 50_000 {
			row := []any{
				fmt.Sprintf("%s-%04d", accounts[rand.IntN(len(accounts))], rand.IntN(1000)),
				float64(rand.IntN(1_000_000)) / 100,
				fmt.Sprintf("entry %d padding padding padding", i),
			}
			if !yield(row, nil) {
				return
			}
		}
	}
	loaded := demo.Value(db.BulkInsert(gosmo.BulkCopy{
		Schema:  "dbo",
		Table:   "Ledger",
		Columns: []string{"Account", "Amount", "Memo"},
		Options: gosmo.BulkOptions{TableLock: true, RowsPerBatch: 10_000},
	}, rows))
	fmt.Printf("\nLoaded %d rows into dbo.Ledger\n", loaded)

	demo.Must(tbl.CreateIndex(gosmo.CreateIndexRequest{
		Name:            "IX_Ledger_Account",
		Type:            gosmo.IndexTypeNonClustered,
		KeyColumns:      []gosmo.IndexColumnDef{{Name: "Account"}},
		IncludedColumns: []string{"Amount"},
		FillFactor:      70,
	}))

	// -- Fragmentation -----------------------------------------------------
	//
	// The mode is the DMV's: "LIMITED" (cheap, index leaf only), "SAMPLED",
	// or "DETAILED". AvgPageSpaceUsedPct is only populated by the latter two
	// — Table.FragmentationStats runs LIMITED and leaves it zero.
	demo.Section("Fragmentation (DETAILED)")
	for _, idx := range demo.Value(tbl.Indexes()) {
		f := demo.Value(idx.Fragmentation(tbl, "DETAILED"))
		fmt.Printf("  %-24s frag=%5.2f%%  pages=%-6d fragments=%-5d page_fullness=%5.2f%%\n",
			f.IndexName, f.AvgFragmentationPct, f.PageCount, f.FragmentCount, f.AvgPageSpaceUsedPct)
	}

	demo.Section("Reorganize, then rebuild")
	idx := demo.Value(tbl.Indexes())[0]
	demo.Must(idx.Reorganize(tbl))
	fmt.Printf("  reorganized %s\n", idx.Name)
	// RebuildWithOptions is the same rebuild plus PAD_INDEX and
	// DATA_COMPRESSION; the compression keyword is allowlisted, not spliced.
	demo.Must(idx.RebuildWithOptions(tbl, 90, true, "PAGE"))
	fmt.Printf("  rebuilt %s at fill factor 90 with PAGE compression\n", idx.Name)
	demo.Must(tbl.RebuildAllIndexes(90))
	fmt.Println("  rebuilt every index on the table")

	after := demo.Value(tbl.FragmentationStats("LIMITED"))
	for _, f := range after {
		fmt.Printf("  after: %-24s frag=%5.2f%% pages=%d\n",
			f.IndexName, f.AvgFragmentationPct, f.PageCount)
	}

	// -- Index options -----------------------------------------------------
	demo.Section("Index options")
	demo.Must(idx.SetLockOptions(tbl, true, false))
	demo.Must(idx.Disable(tbl))
	fmt.Printf("  %s disabled — it now costs nothing to maintain and cannot be used\n", idx.Name)
	demo.Must(idx.Enable(tbl)) // ENABLE is a rebuild; there is no cheaper way back
	storage := demo.Value(idx.StorageInfo(tbl))
	fmt.Printf("  %s: %d rows, %d KB reserved (%d used) on %s, avg record %.1f bytes\n",
		idx.Name, storage.RowCount, storage.ReservedKB, storage.UsedKB,
		storage.FileGroup, storage.AvgRecordSize)

	// -- Statistics --------------------------------------------------------
	demo.Section("Statistics")
	demo.Must(tbl.CreateStatistic("ST_Ledger_Amount", []string{"Amount"}, 100))
	demo.Must(tbl.UpdateAllStatistics(50))
	for _, st := range demo.Value(tbl.Statistics()) {
		origin := "auto"
		if st.IsUserCreated {
			origin = "user"
		}
		cols := demo.Value(st.Columns())
		fmt.Printf("  %-28s %-5s cols=%v rows=%d sampled=%d steps=%d modified=%d\n",
			st.Name, origin, cols, st.TotalRows, st.RowsSampled, st.Steps, st.ModificationCounter)
	}

	// DBCC SHOW_STATISTICS, split into its three result sets.
	st := demo.Value(tbl.Statistics())[0]
	hdr := demo.Value(st.Header())
	fmt.Printf("\n  %s header: updated=%q rows=%d density=%.6f avg_key_len=%.1f\n",
		st.Name, hdr.Updated, hdr.Rows, hdr.Density, hdr.AverageKeyLength)
	for _, d := range demo.Value(st.DensityVector()) {
		fmt.Printf("    density %.8f  avg_len=%5.1f  {%s}\n", d.AllDensity, d.AverageLength, d.Columns)
	}
	hist := demo.Value(st.Histogram())
	fmt.Printf("    histogram: %d steps; first three:\n", len(hist))
	for _, step := range hist[:min(3, len(hist))] {
		fmt.Printf("      high_key=%-20s eq_rows=%-8.0f range_rows=%-8.0f avg_range=%.2f\n",
			step.RangeHighKey, step.EqRows, step.RangeRows, step.AvgRangeRows)
	}

	// -- Space -------------------------------------------------------------
	demo.Section("Space")
	space := demo.Value(db.SpaceUsed())
	fmt.Printf("  database: total=%.1f MB data=%.1f MB log=%.1f MB unallocated=%.1f MB avail_log=%.1f MB\n",
		space.TotalMB, space.DataMB, space.LogMB, space.UnallocatedMB, space.AvailLogMB)
	ts := demo.Value(tbl.SpaceUsed())
	fmt.Printf("  dbo.Ledger: reserved=%d KB data=%d KB index=%d KB lob=%d KB unused=%d KB on %s\n",
		ts.ReservedKB, ts.DataKB, ts.IndexKB, ts.LOBKB, ts.UnusedKB, ts.FileGroup)

	// One round trip for every table, for a tree or grid that needs them all.
	counts := demo.Value(db.TableRowCounts())
	sizes := demo.Value(db.TableSpaceUsedAll())
	for _, t := range demo.Value(db.Tables()) {
		fmt.Printf("  %-20s rows=%-8d reserved=%d KB\n",
			t.FullName(), counts[t.ObjectID], sizes[t.ObjectID].ReservedKB)
	}

	// -- Database options --------------------------------------------------
	demo.Section("Database options")
	demo.Must(db.SetDatabaseOption(gosmo.DBOptAutoCreateStatistics, "ON"))
	demo.Must(db.SetDatabaseOption(gosmo.DBOptAutoUpdateStatisticsAsync, "ON"))
	demo.Must(db.SetDatabaseOption(gosmo.DBOptPageVerify, "CHECKSUM"))
	opts := demo.Value(db.Options())
	fmt.Printf("  owner=%s page_verify=%s user_access=%s auto_create_stats=%t rcsi=%t\n",
		opts.Owner, opts.PageVerify, opts.UserAccess, opts.AutoCreateStats, opts.ReadCommittedSnapshot)

	demo.Section("Database scoped configurations (non-default only)")
	demo.Must(db.SetDatabaseScopedConfig("MAXDOP", "2", false))
	for _, c := range demo.Value(db.DatabaseScopedConfigs()) {
		if c.IsValueDefault {
			continue
		}
		// Boolean-style options render as "0"/"1" here, not "OFF"/"ON".
		fmt.Printf("  %-40s = %-8s (secondary: %s)\n", c.Name, c.Value, c.ValueForSecondary)
	}

	// -- Change tracking ---------------------------------------------------
	demo.Section("Change tracking")
	demo.Must(db.SetChangeTracking(gosmo.ChangeTrackingInfo{
		Enabled:         true,
		AutoCleanup:     true,
		RetentionPeriod: 2,
		RetentionUnit:   "DAYS",
	}))
	demo.Must(db.SetTableChangeTracking("dbo", "Ledger", true, true))
	ct := demo.Value(db.ChangeTracking())
	fmt.Printf("  database: enabled=%t auto_cleanup=%t retention=%d %s\n",
		ct.Enabled, ct.AutoCleanup, ct.RetentionPeriod, ct.RetentionUnit)
	for _, t := range demo.Value(db.TableChangeTracking()) {
		fmt.Printf("  table %s.%s: track_columns=%t\n", t.Schema, t.Name, t.TrackColumnsUpdated)
	}

	// -- Query Store -------------------------------------------------------
	demo.Section("Query Store")
	demo.Must(db.SetQueryStoreOptions(gosmo.QueryStoreOptions{
		DesiredState:         "READ_WRITE",
		MaxStorageMB:         256,
		CaptureMode:          "AUTO",
		SizeCleanupMode:      "AUTO",
		StaleThresholdDays:   7,
		FlushIntervalSec:     900,
		IntervalMinutes:      15,
		MaxPlansPerQuery:     200,
		WaitStatsCaptureMode: "ON",
	}))
	qs := demo.Value(db.QueryStore())
	fmt.Printf("  desired=%s actual=%s storage=%d/%d MB capture=%s wait_stats=%s\n",
		qs.DesiredState, qs.ActualState, qs.CurrentStorageMB, qs.MaxStorageMB,
		qs.CaptureMode, qs.WaitStatsCaptureMode)
	demo.Must(db.FlushQueryStore())
	demo.Must(db.ClearQueryStore())
	fmt.Println("  flushed and cleared")

	// -- Server-wide storage ------------------------------------------------
	demo.Section("Volumes backing this instance's files")
	for _, v := range demo.Value(srv.DiskVolumes()) {
		name := v.MountPoint
		if name == "" {
			name = v.SamplePath // containers often report no mount point
		}
		fmt.Printf("  %-24s %.0f MB free of %.0f MB\n", name, v.AvailableMB, v.TotalMB)
	}
}

func growth(f *gosmo.DatabaseFileInfo) string {
	if f.IsPercentGrowth {
		return fmt.Sprintf("%d%%", f.GrowthPercent)
	}
	return fmt.Sprintf("%d MB", f.GrowthKB/1024)
}
