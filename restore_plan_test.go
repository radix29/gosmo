package gosmo

import (
	"slices"
	"strings"
	"testing"
)

// The Files Included preview, the MOVE clauses and the RESTORE itself all
// name a backup set, and they agree only because every one of them derives
// the number from SetNumber. Position, not the header's index: the two
// coincide only on a device whose sets run contiguously from 1.
func TestBackupSetNumber(t *testing.T) {
	for _, tc := range []struct{ position, want int }{
		// No clause for set 1: SQL Server reads its absence as set 1, so the
		// common single-backup device restores without WITH FILE = 1.
		{1, 0},
		{0, 0}, // a header that reported no position
		{2, 2},
		{7, 7},
	} {
		if got := (&BackupHeader{Position: tc.position}).SetNumber(); got != tc.want {
			t.Errorf("BackupHeader{Position: %d}.SetNumber() = %d, want %d", tc.position, got, tc.want)
		}
		if got := (&BackupInfo{Position: tc.position}).SetNumber(); got != tc.want {
			t.Errorf("BackupInfo{Position: %d}.SetNumber() = %d, want %d", tc.position, got, tc.want)
		}
	}
}

func TestBackupSetAt(t *testing.T) {
	hdr := func(pos int, db string) *BackupHeader { return &BackupHeader{Position: pos, DatabaseName: db} }
	three := []*BackupHeader{hdr(1, "AppDB"), hdr(2, "AppDB"), hdr(3, "Other")}

	if got := BackupSetAt(three, 0); got != three[0] {
		t.Errorf("set 0 = %+v, want the first set", got)
	}
	if got := BackupSetAt(three, 3); got != three[2] {
		t.Errorf("set 3 = %+v, want Other's", got)
	}
	// A position the device no longer has — its file was overwritten WITH
	// INIT since the history row was written — is no set, not set 1.
	if got := BackupSetAt(three, 5); got != nil {
		t.Errorf("missing set 5 = %+v, want nil", got)
	}
	if got := BackupSetAt(nil, 0); got != nil {
		t.Errorf("no headers = %+v, want nil", got)
	}
}

// A Managed Instance's automated backups carry no device, and on t-qmi-01
// they were the whole history: as restore sources they restore from nothing.
func TestBackupInfoRestorable(t *testing.T) {
	for dev, want := range map[string]bool{
		"":                false,
		"  ":              false,
		`E:\hist\one.bak`: true,
		"https://acct.blob.core.windows.net/c/GoTest01.bak": true,
	} {
		if got := (&BackupInfo{DeviceName: dev}).Restorable(); got != want {
			t.Errorf("Restorable(%q) = %v, want %v", dev, got, want)
		}
	}
}

// backupSetFiles is a data and a log file recorded under the source
// database's own Windows paths.
func backupSetFiles() []*BackupFile {
	return []*BackupFile{
		{LogicalName: "AppDB", PhysicalName: `D:\SQL\DATA\AppDB.mdf`, Type: "D"},
		{LogicalName: "AppDB_log", PhysicalName: `E:\SQL\LOG\AppDB_log.ldf`, Type: "L"},
	}
}

func assertMoves(t *testing.T, got, want []RelocateFile) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Errorf("moves = %+v\nwant    %+v", got, want)
	}
}

// RelocateIfRenamed moves files to the default folders — under names derived
// from the target — only when the restore renames the database: a same-name
// restore is meant to land on the original's own files.
func TestRestoreRelocationIfRenamed(t *testing.T) {
	r := RestoreRelocation{DefaultDataDir: `C:\Data`, DefaultLogDir: `C:\Log`}

	if got := r.Moves(backupSetFiles(), "AppDB", "AppDB"); got != nil {
		t.Errorf("same-name restore moved files: %+v", got)
	}
	// Case-insensitively the same name is the same database.
	if got := r.Moves(backupSetFiles(), "AppDB", "appdb"); got != nil {
		t.Errorf("case-different name moved files: %+v", got)
	}
	// On a case-sensitive instance it is a different database, which needs
	// files of its own: without MOVE clauses it collides with AppDB's.
	cs := r
	cs.Collation = "Latin1_General_CS_AS"
	assertMoves(t, cs.Moves(backupSetFiles(), "AppDB", "appdb"), []RelocateFile{
		{LogicalName: "AppDB", PhysicalName: `C:\Data\appdb_AppDB.mdf`},
		{LogicalName: "AppDB_log", PhysicalName: `C:\Log\appdb_AppDB_log.ldf`},
	})
	if !cs.NeedsFileList("AppDB", "appdb") {
		t.Error("NeedsFileList skipped the file list for a case-only rename on a CS instance")
	}
	// Token-matched: "CS" inside a word is not a case-sensitive collation.
	if !(RestoreRelocation{Collation: "SQL_Latin1_General_CP1_CI_AS"}).NeedsFileList("AppDB", "AppDB_Copy") ||
		(RestoreRelocation{Collation: "SQL_Latin1_General_CP1_CI_AS"}).NeedsFileList("AppDB", "appdb") {
		t.Error("a CI collation compared database names case-sensitively")
	}

	assertMoves(t, r.Moves(backupSetFiles(), "AppDB", "AppDB_Copy"), []RelocateFile{
		{LogicalName: "AppDB", PhysicalName: `C:\Data\AppDB_Copy_AppDB.mdf`},
		{LogicalName: "AppDB_log", PhysicalName: `C:\Log\AppDB_Copy_AppDB_log.ldf`},
	})
}

// RelocateNone emits no MOVE at all, renamed target included.
func TestRestoreRelocationNone(t *testing.T) {
	r := RestoreRelocation{Mode: RelocateNone, DefaultDataDir: `C:\Data`, DefaultLogDir: `C:\Log`}
	for _, target := range []string{"AppDB", "AppDB_Copy"} {
		if got := r.Moves(backupSetFiles(), "AppDB", target); got != nil {
			t.Errorf("target %q: moved files: %+v", target, got)
		}
	}
}

// RelocateToFolders moves every file whether or not the database is renamed;
// the names follow the same rule as RelocateIfRenamed, so a copy restored into
// the original's folder cannot collide with it.
func TestRestoreRelocationToFolders(t *testing.T) {
	r := RestoreRelocation{Mode: RelocateToFolders, DataDir: `F:\NewData`, LogDir: `G:\NewLog`,
		DefaultDataDir: `C:\Data`, DefaultLogDir: `C:\Log`}
	assertMoves(t, r.Moves(backupSetFiles(), "AppDB", "AppDB"), []RelocateFile{
		{LogicalName: "AppDB", PhysicalName: `F:\NewData\AppDB.mdf`},
		{LogicalName: "AppDB_log", PhysicalName: `G:\NewLog\AppDB_log.ldf`},
	})
	assertMoves(t, r.Moves(backupSetFiles(), "AppDB", "AppDB_Copy"), []RelocateFile{
		{LogicalName: "AppDB", PhysicalName: `F:\NewData\AppDB_Copy_AppDB.mdf`},
		{LogicalName: "AppDB_log", PhysicalName: `G:\NewLog\AppDB_Copy_AppDB_log.ldf`},
	})

	// An empty folder means the server's default, not a bare file name —
	// which RESTORE would reject as a relative path.
	r.DataDir = ""
	assertMoves(t, r.Moves(backupSetFiles(), "AppDB", "AppDB"), []RelocateFile{
		{LogicalName: "AppDB", PhysicalName: `C:\Data\AppDB.mdf`},
		{LogicalName: "AppDB_log", PhysicalName: `G:\NewLog\AppDB_log.ldf`},
	})
}

// A renamed file is minted from the logical name, so a physical name with no
// extension needs one, and data and log files get different ones.
func TestRestoreRelocationSuppliesAnExtension(t *testing.T) {
	files := []*BackupFile{
		{LogicalName: "AppDB", PhysicalName: `D:\SQL\DATA\AppDB`, Type: "D"},
		{LogicalName: "AppDB_log", PhysicalName: "/var/opt/mssql/log/AppDB_log", Type: "L"},
	}
	r := RestoreRelocation{DefaultDataDir: "/data/", DefaultLogDir: "/log"}
	assertMoves(t, r.Moves(files, "AppDB", "Copy"), []RelocateFile{
		{LogicalName: "AppDB", PhysicalName: "/data/Copy_AppDB.ndf"},
		{LogicalName: "AppDB_log", PhysicalName: "/log/Copy_AppDB_log.ldf"},
	})
}

// A FILESTREAM container (S) and a legacy full-text catalog (F) are
// directories: renamed, they get no extension — not .ndf, and not whatever
// follows a dot in the original directory name.
func TestRestoreRelocationDirectoriesGetNoExtension(t *testing.T) {
	files := []*BackupFile{
		{LogicalName: "AppDB_fs", PhysicalName: `D:\SQL\DATA\AppDB_fs`, Type: "S"},
		{LogicalName: "AppDB_ft", PhysicalName: `D:\SQL\FTData\AppDB.ft`, Type: "F"},
	}
	r := RestoreRelocation{DefaultDataDir: `D:\SQL\DATA`, DefaultLogDir: `D:\SQL\LOG`}
	assertMoves(t, r.Moves(files, "AppDB", "Copy"), []RelocateFile{
		{LogicalName: "AppDB_fs", PhysicalName: `D:\SQL\DATA\Copy_AppDB_fs`},
		{LogicalName: "AppDB_ft", PhysicalName: `D:\SQL\DATA\Copy_AppDB_ft`},
	})
	// A same-name restore keeps the directory's own name.
	r.Mode = RelocateToFolders
	assertMoves(t, r.Moves(files, "AppDB", "AppDB"), []RelocateFile{
		{LogicalName: "AppDB_fs", PhysicalName: `D:\SQL\DATA\AppDB_fs`},
		{LogicalName: "AppDB_ft", PhysicalName: `D:\SQL\DATA\AppDB.ft`},
	})
}

// NeedsFileList decides whether a caller runs RESTORE FILELISTONLY at all, so
// it has to agree with Moves: a plan that would move files must not have its
// file list skipped.
func TestNeedsFileListAgreesWithMoves(t *testing.T) {
	for _, mode := range []RelocationMode{RelocateIfRenamed, RelocateNone, RelocateToFolders} {
		for _, target := range []string{"AppDB", "AppDB_Copy"} {
			r := RestoreRelocation{Mode: mode, DataDir: `F:\D`, LogDir: `F:\L`}
			moves := r.Moves(backupSetFiles(), "AppDB", target)
			if want := len(moves) > 0; r.NeedsFileList("AppDB", target) != want {
				t.Errorf("mode %d target %q: NeedsFileList = %v but Moves made %d", mode, target, !want, len(moves))
			}
		}
	}
}

// FromHeader sets both halves from the one set, so WITH FILE and MOVE cannot
// name different ones — and plans the moves for the source database the set
// itself records, not the device's first.
func TestRestoreOptionsFromHeader(t *testing.T) {
	o := RestoreOptions{Database: "AppDB"}
	o.FromHeader(&BackupHeader{Position: 3, DatabaseName: "Other"}, backupSetFiles(),
		RestoreRelocation{DefaultDataDir: `C:\Data`, DefaultLogDir: `C:\Log`})
	if o.FileNumber != 3 {
		t.Errorf("FileNumber = %d, want 3", o.FileNumber)
	}
	// Other restored as AppDB is a rename.
	assertMoves(t, o.RelocateFiles, []RelocateFile{
		{LogicalName: "AppDB", PhysicalName: `C:\Data\AppDB_AppDB.mdf`},
		{LogicalName: "AppDB_log", PhysicalName: `C:\Log\AppDB_AppDB_log.ldf`},
	})
	o.Devices = []BackupTarget{DiskTarget(`E:\b\all.bak`)}
	stmt, err := buildRestoreStatement(o, false)
	if err != nil {
		t.Fatalf("buildRestoreStatement: %v", err)
	}
	for _, want := range []string{"FILE = 3", `MOVE N'AppDB' TO N'C:\Data\AppDB_AppDB.mdf'`} {
		if !strings.Contains(stmt, want) {
			t.Errorf("statement lacks %s:\n%s", want, stmt)
		}
	}

	o = RestoreOptions{Database: "AppDB"}
	o.FromHeader(&BackupHeader{Position: 1, DatabaseName: "AppDB"}, nil, RestoreRelocation{})
	if o.FileNumber != 0 || o.RelocateFiles != nil {
		t.Errorf("set 1 under its own name = FILE %d, %d moves; want no clause and none", o.FileNumber, len(o.RelocateFiles))
	}
}
