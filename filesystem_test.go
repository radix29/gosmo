package gosmo

import (
	"database/sql"
	"database/sql/driver"
	"testing"
)

// captureServer returns a Server wired to the capture driver, reporting the
// given major version so the version-gated fallbacks in filesystem.go can be
// exercised without a real instance.
func captureServer(t *testing.T, versionMajor int) *Server {
	t.Helper()
	db, err := sql.Open("capture", "")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	captured.reset()
	return &Server{db: db, info: &ServerInfo{VersionMajor: versionMajor}}
}

// EnumFileSystem must read the DMF on 2017+ and fall back to xp_dirtree
// below it — sys.dm_os_enumerate_filesystem does not exist before 2017, and
// sending it there fails the listing outright rather than degrading.
func TestEnumFileSystemPicksSourceByVersion(t *testing.T) {
	s := captureServer(t, 17)
	if _, err := s.EnumFileSystem(`C:\Backup`); err != nil {
		t.Fatalf("EnumFileSystem (2025): %v", err)
	}
	if captured.find("sys.dm_os_enumerate_filesystem") == "" {
		t.Fatalf("2017+ did not query sys.dm_os_enumerate_filesystem, got %v", captured.qs)
	}

	s = captureServer(t, 13)
	if _, err := s.EnumFileSystem(`C:\Backup`); err != nil {
		t.Fatalf("EnumFileSystem (2016): %v", err)
	}
	if captured.find("xp_dirtree") == "" {
		t.Fatalf("pre-2017 did not fall back to xp_dirtree, got %v", captured.qs)
	}
	if captured.find("sys.dm_os_enumerate_filesystem") != "" {
		t.Fatalf("pre-2017 queried sys.dm_os_enumerate_filesystem")
	}
}

// Same split for the drive list: sys.dm_os_enumerate_fixed_drives is 2019+.
func TestFixedDrivesPicksSourceByVersion(t *testing.T) {
	s := captureServer(t, 15)
	if _, err := s.FixedDrives(); err != nil {
		t.Fatalf("FixedDrives (2019): %v", err)
	}
	if captured.find("sys.dm_os_enumerate_fixed_drives") == "" {
		t.Fatalf("2019+ did not query sys.dm_os_enumerate_fixed_drives, got %v", captured.qs)
	}

	s = captureServer(t, 14)
	if _, err := s.FixedDrives(); err != nil {
		t.Fatalf("FixedDrives (2017): %v", err)
	}
	if captured.find("xp_fixeddrives") == "" {
		t.Fatalf("pre-2019 did not fall back to xp_fixeddrives, got %v", captured.qs)
	}
}

// xp_fileexist reports a directory as "File Exists" = 0 with "File is a
// Directory" = 1. Reading only the first column would report an existing
// directory as missing — which, in a Save dialog, silently skips the
// overwrite prompt for a path that is really there.
func TestFileSystemExistsTreatsDirectoryAsExisting(t *testing.T) {
	db, err := sql.Open("capture", "")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	captured.reset(cannedRow{
		match: "xp_fileexist",
		cols:  []string{"File Exists", "File is a Directory", "Parent Directory Exists"},
		row:   []driver.Value{int64(0), int64(1), int64(1)},
	})
	s := &Server{db: db, info: &ServerInfo{VersionMajor: 17}}

	exists, isDir, err := s.FileSystemExists(`C:\Backup`)
	if err != nil {
		t.Fatalf("FileSystemExists: %v", err)
	}
	if !exists || !isDir {
		t.Fatalf("FileSystemExists = (%v, %v), want (true, true)", exists, isDir)
	}
}

// A path that doesn't exist is a normal answer, not an error — the Save
// dialog asks about every candidate destination, most of which are new.
func TestFileSystemExistsMissingPathIsNotAnError(t *testing.T) {
	s := captureServer(t, 17) // capture driver replies with no rows
	exists, isDir, err := s.FileSystemExists(`C:\nope.bak`)
	if err != nil {
		t.Fatalf("FileSystemExists: %v", err)
	}
	if exists || isDir {
		t.Fatalf("FileSystemExists = (%v, %v), want (false, false)", exists, isDir)
	}
}

func TestServerPathSeparator(t *testing.T) {
	if got := serverPathSeparator(`C:\Program Files\Backup`); got != `\` {
		t.Errorf("serverPathSeparator(windows) = %q, want %q", got, `\`)
	}
	if got := serverPathSeparator("/var/opt/mssql/data"); got != "/" {
		t.Errorf("serverPathSeparator(posix) = %q, want %q", got, "/")
	}
}
