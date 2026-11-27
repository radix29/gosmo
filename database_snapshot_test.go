package gosmo

import "testing"

// TestSnapshotFilePathsAreDistinct: the default sparse-file path drops the
// source file's extension, so two data files in one folder differing only by
// it mapped onto one path and the CREATE DATABASE failed "Cannot create file
// … because it already exists". The first file keeps the conventional name;
// each later one that would collide — case-insensitively, as a Windows
// server's paths do — falls back to keeping the extension, then the logical
// name, then a number.
func TestSnapshotFilePathsAreDistinct(t *testing.T) {
	type file struct{ physical, logical string }
	cases := []struct {
		name  string
		files []file
		want  []string
	}{
		{"distinct stems are unchanged",
			[]file{{`D:\data\db.mdf`, "db"}, {`D:\data\db_2.ndf`, "db_2"}},
			[]string{`D:\data\db_snap.ss`, `D:\data\db_2_snap.ss`}},
		{"same stem, different extension",
			[]file{{`D:\data\gz_dst.mdf`, "gz_dst"}, {`D:\data\gz_dst.ndf`, "gz_dst_2"}},
			[]string{`D:\data\gz_dst_snap.ss`, `D:\data\gz_dst_ndf_snap.ss`}},
		{"the same stem in different folders does not collide",
			[]file{{`D:\a\db.mdf`, "db"}, {`D:\b\db.ndf`, "db_2"}},
			[]string{`D:\a\db_snap.ss`, `D:\b\db_snap.ss`}},
		{"case differences collide",
			[]file{{`D:\Data\DB.mdf`, "db"}, {`d:\data\db.NDF`, "db_2"}},
			[]string{`D:\Data\DB_snap.ss`, `d:\data\db_NDF_snap.ss`}},
		{"the extension kept still collides: the logical name, made safe",
			[]file{{`/var/opt/x.mdf`, "x"}, {`/var/opt/x_ndf`, "x2"}, {`/var/opt/x.ndf`, `x:3`}},
			[]string{`/var/opt/x_snap.ss`, `/var/opt/x_ndf_snap.ss`, `/var/opt/x_ndf_x_3_snap.ss`}},
		{"then a number, when making the logical name safe collides too",
			[]file{{`/d/x.mdf`, "a"}, {`/d/x.ndf`, "b"}, {`/d/x_ndf_c_`, "q"}, {`/d/X.NDF`, "c?"}},
			[]string{`/d/x_snap.ss`, `/d/x_ndf_snap.ss`, `/d/x_ndf_c__snap.ss`, `/d/X_NDF_c__2_snap.ss`}},
	}
	for _, c := range cases {
		taken := map[string]bool{}
		for i, f := range c.files {
			if got := snapshotFilePath(f.physical, f.logical, "snap", taken); got != c.want[i] {
				t.Errorf("%s: file %d (%s) → %q, want %q", c.name, i, f.physical, got, c.want[i])
			}
		}
	}
}

// TestSnapshotFilePathIsFileNameSafe: the snapshot name is a sysname and was
// put into the sparse-file path unchanged, so a time typed into it ("10:30")
// made the path an NTFS stream and a "/" named a missing directory. Two
// names differing only by an unsafe character map to one safe name; the
// taken fallback still keeps their paths apart.
func TestSnapshotFilePathIsFileNameSafe(t *testing.T) {
	taken := map[string]bool{}
	if got, want := snapshotFilePath(`D:\data\Sales.mdf`, "Sales", "Sales/2026:1", taken), `D:\data\Sales_Sales_2026_1.ss`; got != want {
		t.Errorf("Sales/2026:1 → %q, want %q", got, want)
	}
	taken = map[string]bool{}
	a := snapshotFilePath(`/d/x.mdf`, "x", "a:b", taken)
	b := snapshotFilePath(`/d/x.mdf`, "x", "a/b", taken)
	if a != `/d/x_a_b.ss` || b == a || ServerPathDir(b) != "/d" {
		t.Errorf("a:b → %q, a/b → %q: want /d/x_a_b.ss and a distinct path in /d", a, b)
	}
}
