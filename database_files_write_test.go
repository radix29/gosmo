package gosmo

import (
	"strings"
	"testing"
)

func TestBuildAddFileStatement(t *testing.T) {
	cases := []struct {
		name string
		spec DatabaseFileSpec
		want string
	}{
		{
			name: "data file with filegroup, size, growth, max size",
			spec: DatabaseFileSpec{
				Name: "appdb_idx", FileGroup: "INDEXES", Path: `D:\SQLData\appdb_idx.ndf`,
				SizeKB: 4096 * 1024, GrowthKB: 1024 * 1024, MaxSizeKB: -1,
			},
			want: "ALTER DATABASE [appdb] ADD FILE (NAME = [appdb_idx], FILENAME = 'D:\\SQLData\\appdb_idx.ndf', " +
				"SIZE = 4194304KB, MAXSIZE = UNLIMITED, FILEGROWTH = 1048576KB) TO FILEGROUP [INDEXES]",
		},
		{
			name: "log file — no filegroup clause even if one is set",
			spec: DatabaseFileSpec{
				Name: "appdb_log2", Type: "LOG", FileGroup: "PRIMARY", Path: `D:\SQLLog\appdb_log2.ldf`,
				SizeKB: 512 * 1024, GrowthPercent: 10,
			},
			want: "ALTER DATABASE [appdb] ADD LOG FILE (NAME = [appdb_log2], FILENAME = 'D:\\SQLLog\\appdb_log2.ldf', " +
				"SIZE = 524288KB, FILEGROWTH = 10%)",
		},
		{
			name: "minimal spec — no optional clauses",
			spec: DatabaseFileSpec{Name: "f1", Path: "/var/opt/mssql/data/f1.ndf"},
			want: "ALTER DATABASE [appdb] ADD FILE (NAME = [f1], FILENAME = '/var/opt/mssql/data/f1.ndf')",
		},
		{
			name: "bounded (positive) max size, not unlimited",
			spec: DatabaseFileSpec{Name: "f2", Path: "/var/opt/mssql/data/f2.ndf", MaxSizeKB: 102400},
			want: "ALTER DATABASE [appdb] ADD FILE (NAME = [f2], FILENAME = '/var/opt/mssql/data/f2.ndf', MAXSIZE = 102400KB)",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := buildAddFileStatement("appdb", c.spec)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != c.want {
				t.Errorf("got:\n%s\nwant:\n%s", got, c.want)
			}
		})
	}
}

func TestBuildAddFileStatementRequiresNameAndPath(t *testing.T) {
	if _, err := buildAddFileStatement("appdb", DatabaseFileSpec{Path: "x"}); err == nil {
		t.Error("missing Name: want an error, got nil")
	}
	if _, err := buildAddFileStatement("appdb", DatabaseFileSpec{Name: "x"}); err == nil {
		t.Error("missing Path: want an error, got nil")
	}
}

func TestBuildAlterFileStatement(t *testing.T) {
	cases := []struct {
		name string
		m    FileModify
		want string
	}{
		{
			name: "rename and resize",
			m:    FileModify{NewName: "appdb2", SizeKB: 65536, MaxSizeKB: 131072},
			want: "ALTER DATABASE [appdb] MODIFY FILE (NAME = [appdb], NEWNAME = [appdb2], SIZE = 65536KB, MAXSIZE = 131072KB)",
		},
		{
			name: "unlimited max size, percent growth",
			m:    FileModify{MaxSizeKB: -1, GrowthPercent: 15},
			want: "ALTER DATABASE [appdb] MODIFY FILE (NAME = [appdb], MAXSIZE = UNLIMITED, FILEGROWTH = 15%)",
		},
		{
			name: "growth in KB rather than percent",
			m:    FileModify{GrowthKB: 4096},
			want: "ALTER DATABASE [appdb] MODIFY FILE (NAME = [appdb], FILEGROWTH = 4096KB)",
		},
		{
			name: "autogrowth off",
			m:    FileModify{DisableGrowth: true},
			want: "ALTER DATABASE [appdb] MODIFY FILE (NAME = [appdb], FILEGROWTH = 0)",
		},
		{
			name: "autogrowth off beats a growth amount left set",
			m:    FileModify{DisableGrowth: true, GrowthKB: 4096, GrowthPercent: 10},
			want: "ALTER DATABASE [appdb] MODIFY FILE (NAME = [appdb], FILEGROWTH = 0)",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := buildAlterFileStatement("appdb", "appdb", c.m)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != c.want {
				t.Errorf("got:\n%s\nwant:\n%s", got, c.want)
			}
		})
	}
}

func TestBuildAlterFileStatementNoOpWhenNothingChanges(t *testing.T) {
	got, err := buildAlterFileStatement("appdb", "appdb", FileModify{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "" {
		t.Errorf("no-op FileModify produced a statement: %q, want \"\"", got)
	}
}

func TestBuildAlterFileStatementRequiresName(t *testing.T) {
	if _, err := buildAlterFileStatement("appdb", "", FileModify{SizeKB: 1}); err == nil {
		t.Error("missing name: want an error, got nil")
	}
}

// TestDisableGrowthIsNotTheZeroValue pins the distinction the DisableGrowth
// field exists for: a zero GrowthKB means "don't touch FILEGROWTH", and only
// DisableGrowth means "set it to 0". Collapsing the two — by emitting
// FILEGROWTH = 0 whenever growth is zero — would write autogrowth off on
// every ALTER that only meant to resize.
func TestDisableGrowthIsNotTheZeroValue(t *testing.T) {
	resize, err := buildAlterFileStatement("appdb", "appdb", FileModify{SizeKB: 65536})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(resize, "FILEGROWTH") {
		t.Errorf("a zero GrowthKB emitted a FILEGROWTH clause: %q", resize)
	}
	off, err := buildAlterFileStatement("appdb", "appdb", FileModify{DisableGrowth: true})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(off, "FILEGROWTH = 0") {
		t.Errorf("DisableGrowth did not emit FILEGROWTH = 0: %q", off)
	}
}

// TestDisableGrowthOnANewFile covers the ADD FILE half. writeFileSizeClauses
// is shared with CREATE DATABASE's file definitions, so this pins both.
func TestDisableGrowthOnANewFile(t *testing.T) {
	got, err := buildAddFileStatement("appdb", DatabaseFileSpec{
		Name: "appdb_2", Path: `C:\data\appdb_2.ndf`, SizeKB: 8192, DisableGrowth: true,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(got, "FILEGROWTH = 0") {
		t.Errorf("DisableGrowth did not reach ADD FILE: %q", got)
	}
}
