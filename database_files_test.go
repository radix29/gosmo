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
		"file_name", "physical_name", "size", "max_size", "growth",
		"is_percent_growth", "is_primary",
	}
	rows := [][]driver.Value{
		{"PRIMARY", RowsFileGroup, true, false,
			"appdb", `C:\data\appdb.mdf`, int64(8192), int64(-1), int64(65536), false, true},
		{"fsdata", FileStreamFileGroup, true, false,
			"appdb_fs", `C:\data\appdb_fs`, int64(0), int64(-1), int64(0), false, false},
	}
	d := qsRecDB(t, 17, cols, rows)

	fgs, err := d.FileGroupsContext(context.Background())
	if err != nil {
		t.Fatalf("FileGroupsContext: %v", err)
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
		t.Errorf("FileGroupsContext query does not read type_desc:\n%s", sql)
	}
}
