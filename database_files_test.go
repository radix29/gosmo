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

// TestFileGroupsCarryTheirType pins sys.filegroups.type_desc through to
// FileGroup.Type and IsFileStream.
//
// The type is what decides whether a file added to the group becomes a
// FILESTREAM file: ALTER DATABASE ADD FILE has no file-type keyword, so a
// caller that cannot tell the groups apart builds SIZE and FILEGROWTH clauses
// SQL Server refuses with error 5509 — measured against a real FILESTREAM
// database, 2026-09-05.
func TestFileGroupsCarryTheirType(t *testing.T) {
	cols := []string{
		"name", "type_desc", "is_default", "is_read_only",
		"file_id", "file_name", "physical_name", "file_type", "state", "size", "max_size", "growth",
		"is_percent_growth",
	}
	rows := [][]driver.Value{
		{"PRIMARY", RowsFileGroup, true, false,
			int64(1), "appdb", `C:\data\appdb.mdf`, "ROWS", "ONLINE", int64(8192), int64(-1), int64(65536), false},
		{"fsdata", FileStreamFileGroup, true, false,
			int64(65537), "appdb_fs", `C:\data\appdb_fs`, "FILESTREAM", "ONLINE", int64(0), int64(-1), int64(0), false},
	}
	d := qsRecDB(t, 17, cols, rows)

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
	if sql := qsRec.last(t).sql; !strings.Contains(sql, "type_desc") {
		t.Errorf("FileGroups query does not read type_desc:\n%s", sql)
	}
}

// TestFileGroupsListAnEmptyFilegroup pins B18 (gossms): a filegroup with no
// files is listed, with no Files.
//
// FileGroups once inner-joined sys.database_files, so ALTER DATABASE ADD
// FILEGROUP's result had no row — and a filegroup only REMOVE FILEGROUP can
// act on (it must be empty) never reached a caller that could remove it.
// The fake answers what the LEFT JOIN returns for one: a filegroup row whose
// file columns are all NULL.
func TestFileGroupsListAnEmptyFilegroup(t *testing.T) {
	cols := []string{
		"name", "type_desc", "is_default", "is_read_only",
		"file_id", "file_name", "physical_name", "file_type", "state", "size", "max_size", "growth",
		"is_percent_growth",
	}
	rows := [][]driver.Value{
		{"archive", RowsFileGroup, false, false,
			nil, nil, nil, nil, nil, nil, nil, nil, nil},
		{"PRIMARY", RowsFileGroup, true, false,
			int64(1), "appdb", `C:\data\appdb.mdf`, "ROWS", "ONLINE", int64(8192), int64(-1), int64(10), true},
	}
	d := qsRecDB(t, 17, cols, rows)

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
	if sql := qsRec.last(t).sql; !strings.Contains(sql, "LEFT   JOIN sys.database_files") {
		t.Errorf("FileGroups query does not outer-join the files:\n%s", sql)
	}
}

// TestFileGroupsReportMaxSizeAndGrowthInKB pins the units of a filegroup's
// files: sys.database_files.max_size and growth are 8 KB pages, and
// FileGroups once passed them through under the "KB" label, so every max
// size and fixed growth read eight times too small. A percentage growth
// stays a percentage, and -1 stays "unlimited".
func TestFileGroupsReportMaxSizeAndGrowthInKB(t *testing.T) {
	cols := []string{
		"name", "type_desc", "is_default", "is_read_only",
		"file_id", "file_name", "physical_name", "file_type", "state", "size", "max_size", "growth",
		"is_percent_growth",
	}
	rows := [][]driver.Value{
		{"PRIMARY", RowsFileGroup, true, false,
			int64(1), "appdb", `C:\data\appdb.mdf`, "ROWS", "ONLINE", int64(8192), int64(1280), int64(8192), false},
		{"PRIMARY", RowsFileGroup, true, false,
			int64(3), "appdb2", `C:\data\appdb2.ndf`, "ROWS", "ONLINE", int64(8192), int64(-1), int64(10), true},
	}
	d := qsRecDB(t, 17, cols, rows)

	fgs, err := d.FileGroups(context.Background())
	if err != nil {
		t.Fatalf("FileGroups: %v", err)
	}
	if len(fgs) != 1 || len(fgs[0].Files) != 2 {
		t.Fatalf("got %+v, want PRIMARY with two files", fgs)
	}
	if f := fgs[0].Files[0]; f.MaxSizeKB != 10240 || f.IsPercentGrowth || f.GrowthKB != 65536 || f.SizeKB != 8192 {
		t.Errorf("appdb = %+v, want MaxSize 10240 KB, Growth 65536 KB", f)
	}
	if f := fgs[0].Files[1]; f.MaxSizeKB != -1 || !f.IsPercentGrowth || f.GrowthPercent != 10 || f.GrowthKB != 0 || f.IsPrimaryFile() {
		t.Errorf("appdb2 = %+v, want MaxSize -1 (unlimited), Growth 10 PERCENT", f)
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
