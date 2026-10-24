package gosmo

import (
	"context"
	"database/sql/driver"
	"strings"
	"testing"
)

func TestNormalizeFileGrowth(t *testing.T) {
	cases := []struct {
		name                    string
		maxSizePages, growthRaw int64
		isPercentGrowth         bool
		wantMaxKB, wantGrowthKB int64
		wantGrowthPercent       int
	}{
		{
			name:         "unlimited max size, KB growth",
			maxSizePages: -1, growthRaw: 1024, isPercentGrowth: false,
			wantMaxKB: -1, wantGrowthKB: 8192, wantGrowthPercent: 0,
		},
		{
			name:         "bounded max size, percent growth",
			maxSizePages: 4096, growthRaw: 10, isPercentGrowth: true,
			wantMaxKB: 32768, wantGrowthKB: 0, wantGrowthPercent: 10,
		},
		{
			name:         "zero max size (log file with no explicit cap) stays zero, not unlimited",
			maxSizePages: 0, growthRaw: 512, isPercentGrowth: false,
			wantMaxKB: 0, wantGrowthKB: 4096, wantGrowthPercent: 0,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gotMax, gotGrowth, gotPct := normalizeFileGrowth(c.maxSizePages, c.growthRaw, c.isPercentGrowth)
			if gotMax != c.wantMaxKB {
				t.Errorf("maxSizeKB = %d, want %d", gotMax, c.wantMaxKB)
			}
			if gotGrowth != c.wantGrowthKB {
				t.Errorf("growthKB = %d, want %d", gotGrowth, c.wantGrowthKB)
			}
			if gotPct != c.wantGrowthPercent {
				t.Errorf("growthPercent = %d, want %d", gotPct, c.wantGrowthPercent)
			}
		})
	}
}

// fileGroupsDB returns a Database over the recording driver whose
// sys.filegroups read answers groups (name, type_desc, is_default,
// is_read_only) and whose Files read answers files, in Files' column order.
func fileGroupsDB(t *testing.T, groups, files [][]driver.Value) *Database {
	t.Helper()
	d := qsRecDB(t, 17, []string{"name", "type_desc", "is_default", "is_read_only"}, groups)
	qsRec.route("sys.database_files df", []string{
		"file_id", "name", "physical_name", "type_desc", "filegroup", "state_desc",
		"size", "max_size", "growth", "is_percent_growth", "space_used",
	}, files)
	return d
}

// TestFileGroupsCarryTheirType pins sys.filegroups.type_desc through to
// FileGroup.Type and IsFileStream.
//
// The type is what decides whether a file added to the group becomes a
// FILESTREAM file: ALTER DATABASE ADD FILE has no file-type keyword, so a
// caller that cannot tell the groups apart builds SIZE and FILEGROWTH clauses
// SQL Server refuses with error 5509 — measured against a real FILESTREAM
// database, 2026-09-05.
func TestFileGroupsCarryTheirType(t *testing.T) {
	d := fileGroupsDB(t, [][]driver.Value{
		{"PRIMARY", RowsFileGroup, true, false},
		{"fsdata", FileStreamFileGroup, true, false},
	}, [][]driver.Value{
		{int64(65537), "appdb_fs", `C:\data\appdb_fs`, "FILESTREAM", "fsdata", "ONLINE", int64(0), int64(-1), int64(0), false, int64(0)},
		{int64(1), "appdb", `C:\data\appdb.mdf`, "ROWS", "PRIMARY", "ONLINE", int64(65536), int64(-1), int64(8192), false, int64(2048)},
	})

	fgs, err := d.FileGroups(context.Background())
	if err != nil {
		t.Fatalf("FileGroups: %v", err)
	}
	if len(fgs) != 2 {
		t.Fatalf("got %d filegroups, want 2", len(fgs))
	}
	byName := map[string]*FileGroup{}
	for _, fg := range fgs {
		byName[fg.Name] = fg
	}
	if got := byName["PRIMARY"]; got == nil || got.Type != RowsFileGroup || got.IsFileStream() {
		t.Errorf("PRIMARY = %+v, want a %s that is not FILESTREAM", got, RowsFileGroup)
	}
	if got := byName["fsdata"]; got == nil || got.Type != FileStreamFileGroup || !got.IsFileStream() {
		t.Errorf("fsdata = %+v, want a %s that reports IsFileStream", got, FileStreamFileGroup)
	}
	// The column has to be selected, not merely scanned into: a query that
	// stopped reading type_desc would leave every group's Type empty and
	// IsFileStream false, which reads as "no FILESTREAM filegroup here".
	if sql := qsRec.recorded()[0].sql; !strings.Contains(sql, "fg.type_desc") {
		t.Errorf("FileGroups query does not read type_desc:\n%s", sql)
	}
}

// TestFileGroupsListAnEmptyFilegroup pins B18 (gossms): a filegroup with no
// files is listed, with no Files.
//
// FileGroups once inner-joined sys.database_files, so ALTER DATABASE ADD
// FILEGROUP's result had no row — and a filegroup only REMOVE FILEGROUP can
// act on (it must be empty) never reached a caller that could remove it. The
// log file, in no filegroup, is attached to none.
func TestFileGroupsListAnEmptyFilegroup(t *testing.T) {
	d := fileGroupsDB(t, [][]driver.Value{
		{"archive", RowsFileGroup, false, false},
		{"PRIMARY", RowsFileGroup, true, false},
	}, [][]driver.Value{
		{int64(2), "appdb_log", `C:\data\appdb.ldf`, "LOG", "", "ONLINE", int64(8192), int64(268435456), int64(10), true, int64(512)},
		{int64(1), "appdb", `C:\data\appdb.mdf`, "ROWS", "PRIMARY", "ONLINE", int64(65536), int64(-1), int64(10), true, int64(2048)},
	})

	fgs, err := d.FileGroups(context.Background())
	if err != nil {
		t.Fatalf("FileGroups: %v", err)
	}
	if len(fgs) != 2 {
		t.Fatalf("got %d filegroups, want 2 (the empty one included)", len(fgs))
	}
	if fg := fgs[0]; fg.Name != "archive" || len(fg.Files) != 0 {
		t.Errorf("fgs[0] = %q with %d files, want archive with none", fg.Name, len(fg.Files))
	}
	if fg := fgs[1]; fg.Name != "PRIMARY" || len(fg.Files) != 1 ||
		fg.Files[0].Name != "appdb" || !fg.Files[0].IsPercentGrowth || !fg.Files[0].IsPrimaryFile() ||
		fg.Files[0].FileGroup != "PRIMARY" || fg.Files[0].Database() != d {
		t.Errorf("fgs[1] = %+v, want PRIMARY with its one percent-growth primary file", fg)
	}
}

// TestFileGroupsFilesAreFilesOwn pins B2 of the 2026-10-24 review: a file
// FileGroups returns equals the same file from Files, field for field.
//
// FileGroups once read the files itself and never read FILEPROPERTY, so
// every file's SpaceUsedKB was 0 here and not in Files — the zero looked like
// data (TestLiveWrongValuesFileUnits). The units ride along: max_size and
// growth are 8 KB pages, once passed through under the "KB" label, so every
// max size and fixed growth read eight times too small.
func TestFileGroupsFilesAreFilesOwn(t *testing.T) {
	d := fileGroupsDB(t, [][]driver.Value{
		{"PRIMARY", RowsFileGroup, true, false},
	}, [][]driver.Value{
		{int64(1), "appdb", `C:\data\appdb.mdf`, "ROWS", "PRIMARY", "ONLINE", int64(65536), int64(1280), int64(8192), false, int64(3072)},
		{int64(3), "appdb2", `C:\data\appdb2.ndf`, "ROWS", "PRIMARY", "ONLINE", int64(65536), int64(-1), int64(10), true, int64(1024)},
	})
	ctx := context.Background()

	fgs, err := d.FileGroups(ctx)
	if err != nil {
		t.Fatalf("FileGroups: %v", err)
	}
	if len(fgs) != 1 || len(fgs[0].Files) != 2 {
		t.Fatalf("got %+v, want PRIMARY with two files", fgs)
	}
	if f := fgs[0].Files[0]; f.MaxSizeKB != 10240 || f.IsPercentGrowth || f.GrowthKB != 65536 || f.SizeKB != 65536 || f.SpaceUsedKB != 3072 {
		t.Errorf("appdb = %+v, want MaxSize 10240 KB, Growth 65536 KB, SpaceUsed 3072 KB", f)
	}
	if f := fgs[0].Files[1]; f.MaxSizeKB != -1 || !f.IsPercentGrowth || f.GrowthPercent != 10 || f.GrowthKB != 0 || f.IsPrimaryFile() {
		t.Errorf("appdb2 = %+v, want MaxSize -1 (unlimited), Growth 10 PERCENT", f)
	}
	files, err := d.Files(ctx)
	if err != nil {
		t.Fatalf("Files: %v", err)
	}
	for i, f := range fgs[0].Files {
		if *f != *files[i] {
			t.Errorf("FileGroups file %d = %+v, want the Files values %+v", i, *f, *files[i])
		}
	}
}

// TestFileSizesAreSummedInBigint pins the casts that keep a large file from
// failing every file read with Msg 8115 (arithmetic overflow converting
// expression to data type int): size is an int count of 8 KB pages, so
// size * 8 overflows at 2 TiB in one file and SUM(size) at 16 TiB in all. A
// file that large is not practical to create on a test server, so this
// asserts the emitted SQL instead.
func TestFileSizesAreSummedInBigint(t *testing.T) {
	cols := []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j", "k"}
	d := qsRecDB(t, 17, cols, nil)
	ctx := context.Background()

	reads := []struct {
		name string
		run  func() error
	}{
		{"Files", func() error { _, err := d.Files(ctx); return err }},
		{"DatabaseFiles", func() error { _, err := d.server.DatabaseFiles(ctx, "appdb"); return err }},
		{"FileGroups", func() error { _, err := d.FileGroups(ctx); return err }},
		{"SpaceUsed", func() error { _, err := d.SpaceUsed(ctx); return err }},
		{"DiskUsage", func() error { _, err := d.DiskUsage(ctx); return err }},
	}
	for _, r := range reads {
		_ = r.run() // the fake has no rows for the single-row reads; only the SQL matters
		sql := qsRec.last(t).sql
		norm := strings.Join(strings.Fields(sql), " ")
		for _, bad := range []string{"size * 8", "SUM(size)", "THEN size ", "THEN size-"} {
			if strings.Contains(norm, bad) {
				t.Errorf("%s still computes on the int column (%q):\n%s", r.name, bad, sql)
			}
		}
		if !strings.Contains(sql, "CAST(") || !strings.Contains(sql, "size AS bigint)") {
			t.Errorf("%s does not widen size to bigint:\n%s", r.name, sql)
		}
	}
}

// TestDatabaseFilesReadsSizeOnDisk pins the column a snapshot's real size
// comes from: sys.master_files.size is the sparse file's ceiling, so the
// footprint must come from dm_io_virtual_file_stats — and without the right
// to read it, the batch must fall back to NULL (read as 0, "unknown") rather
// than let Msg 300 fail the whole file list.
func TestDatabaseFilesReadsSizeOnDisk(t *testing.T) {
	cols := []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j"}
	rows := [][]driver.Value{
		{int64(1), "snap", `C:\data\snap.ss`, "ROWS", "ONLINE", int64(65536), int64(-1), int64(8192), false, int64(128)},
		{int64(3), "snap2", `C:\data\snap2.ss`, "ROWS", "ONLINE", int64(8192), int64(-1), int64(8192), false, nil},
	}
	d := qsRecDB(t, 17, cols, rows)

	files, err := d.server.DatabaseFiles(context.Background(), "snap")
	if err != nil {
		t.Fatalf("DatabaseFiles: %v", err)
	}
	if len(files) != 2 {
		t.Fatalf("got %d files, want 2", len(files))
	}
	if f := files[0]; f.SizeKB != 65536 || f.SizeOnDiskKB != 128 {
		t.Errorf("snap: SizeKB %d, SizeOnDiskKB %d; want 65536 and 128", f.SizeKB, f.SizeOnDiskKB)
	}
	if f := files[1]; f.SizeOnDiskKB != 0 {
		t.Errorf("snap2: SizeOnDiskKB = %d for a NULL reading, want 0 (unknown)", f.SizeOnDiskKB)
	}

	sql := strings.Join(strings.Fields(qsRec.last(t).sql), " ")
	for _, want := range []string{
		"OUTER APPLY sys.dm_io_virtual_file_stats(mf.database_id, mf.file_id)",
		"size_on_disk_bytes / 1024",
		"HAS_PERMS_BY_NAME(NULL, NULL, 'VIEW SERVER STATE') = 1",
		"HAS_PERMS_BY_NAME(NULL, NULL, 'VIEW SERVER PERFORMANCE STATE') = 1",
		"ELSE",
		"CAST(NULL AS bigint)",
	} {
		if !strings.Contains(sql, want) {
			t.Errorf("DatabaseFiles SQL lacks %q:\n%s", want, sql)
		}
	}
	if strings.Count(sql, "WHERE mf.database_id = DB_ID(@p1)") != 2 {
		t.Errorf("both branches must filter to the one database:\n%s", sql)
	}
}
