package gosmo

import (
	"strings"
	"testing"
	"time"
)

func TestParsePercent(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"10 percent processed.", 10},
		{"100 percent processed.", 100},
		{"0 percent processed.", 0},
		{"Processed 128 pages for database 'x'.", -1},
		{"", -1},
		{"percent processed.", -1}, // no leading digits
	}
	for _, c := range cases {
		if got := parsePercent(c.in); got != c.want {
			t.Errorf("parsePercent(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestBackupRequiresDatabaseAndDevices(t *testing.T) {
	s := &Server{}
	if err := s.Backup(BackupOptions{}); err == nil {
		t.Error("Backup with no database = nil error, want error")
	}
	if err := s.Backup(BackupOptions{Database: "AdventureWorks"}); err == nil {
		t.Error("Backup with no devices = nil error, want error")
	}
}

func TestBuildBackupStatementDifferential(t *testing.T) {
	got, err := BuildBackupStatement(BackupOptions{
		Database: "AdventureWorks",
		Action:   BackupActionDifferential,
		Devices:  []string{`/var/backups/aw_diff.bak`},
	})
	if err != nil {
		t.Fatalf("BuildBackupStatement: %v", err)
	}
	want := "BACKUP DATABASE [AdventureWorks] TO DISK = N'/var/backups/aw_diff.bak' WITH DIFFERENTIAL"
	if got != want {
		t.Errorf("BuildBackupStatement =\n%q\nwant\n%q", got, want)
	}
}

// TestBuildBackupStatementDefaultAction confirms an empty Action defaults to
// a plain BACKUP DATABASE, with no WITH clause when no options are set.
func TestBuildBackupStatementDefaultAction(t *testing.T) {
	got, err := BuildBackupStatement(BackupOptions{
		Database: "AdventureWorks",
		Devices:  []string{`/var/backups/aw.bak`},
	})
	if err != nil {
		t.Fatalf("BuildBackupStatement: %v", err)
	}
	want := "BACKUP DATABASE [AdventureWorks] TO DISK = N'/var/backups/aw.bak'"
	if got != want {
		t.Errorf("BuildBackupStatement =\n%q\nwant\n%q", got, want)
	}
}

// TestBuildBackupStatementLogMultiDevice covers BackupActionLog and multiple
// striped backup devices.
func TestBuildBackupStatementLogMultiDevice(t *testing.T) {
	got, err := BuildBackupStatement(BackupOptions{
		Database: "AdventureWorks",
		Action:   BackupActionLog,
		Devices:  []string{`/var/backups/aw1.trn`, `/var/backups/aw2.trn`},
	})
	if err != nil {
		t.Fatalf("BuildBackupStatement: %v", err)
	}
	want := "BACKUP LOG [AdventureWorks] TO DISK = N'/var/backups/aw1.trn', DISK = N'/var/backups/aw2.trn'"
	if got != want {
		t.Errorf("BuildBackupStatement =\n%q\nwant\n%q", got, want)
	}
}

// TestBuildBackupStatementAllOptions exercises every WITH-clause option
// together: name/description/media, copy-only, compression on, checksum,
// format, init, stats.
func TestBuildBackupStatementAllOptions(t *testing.T) {
	compressionOn := true
	got, err := BuildBackupStatement(BackupOptions{
		Database:         "AdventureWorks",
		Devices:          []string{`/var/backups/aw.bak`},
		BackupSetName:    "AW Full",
		Description:      "Weekly full backup",
		MediaDescription: "Backup media",
		CopyOnly:         true,
		Compression:      &compressionOn,
		Checksum:         true,
		Format:           true,
		Init:             true,
		Stats:            25,
	})
	if err != nil {
		t.Fatalf("BuildBackupStatement: %v", err)
	}
	want := "BACKUP DATABASE [AdventureWorks] TO DISK = N'/var/backups/aw.bak' WITH " +
		"NAME = N'AW Full', DESCRIPTION = N'Weekly full backup', MEDIADESCRIPTION = N'Backup media', " +
		"COPY_ONLY, COMPRESSION, CHECKSUM, FORMAT, INIT, STATS = 25"
	if got != want {
		t.Errorf("BuildBackupStatement =\n%q\nwant\n%q", got, want)
	}
}

// TestBuildBackupStatementCompressionOff confirms Compression=new(false)
// emits NO_COMPRESSION rather than being treated as unset.
func TestBuildBackupStatementCompressionOff(t *testing.T) {
	compressionOff := false
	got, err := BuildBackupStatement(BackupOptions{
		Database:    "AdventureWorks",
		Devices:     []string{`/var/backups/aw.bak`},
		Compression: &compressionOff,
	})
	if err != nil {
		t.Fatalf("BuildBackupStatement: %v", err)
	}
	want := "BACKUP DATABASE [AdventureWorks] TO DISK = N'/var/backups/aw.bak' WITH NO_COMPRESSION"
	if got != want {
		t.Errorf("BuildBackupStatement =\n%q\nwant\n%q", got, want)
	}
}

func TestBackupTypeFromHeader(t *testing.T) {
	cases := []struct {
		n    int
		want BackupAction
	}{
		{1, BackupActionDatabase},
		{2, BackupActionLog},
		{4, BackupActionFiles},
		{5, BackupActionDifferential},
		{6, BackupActionFiles},
		{7, BackupActionDatabase}, // partial — no closer mapping, falls to default
		{8, BackupActionDatabase},
	}
	for _, c := range cases {
		if got := backupTypeFromHeader(c.n); got != c.want {
			t.Errorf("backupTypeFromHeader(%d) = %q, want %q", c.n, got, c.want)
		}
	}
}

func TestBuildRestoreStatement(t *testing.T) {
	got, err := BuildRestoreStatement(RestoreOptions{
		Database: "AW_Restore",
		Devices:  []string{`/var/backups/aw.bak`},
		RelocateFiles: []RelocateFile{
			{LogicalName: "AW_Data", PhysicalName: "/data/AW_Restore_Data.mdf"},
		},
		Recovery: true,
		Replace:  true,
		Stats:    10,
	})
	if err != nil {
		t.Fatalf("BuildRestoreStatement: %v", err)
	}
	want := "RESTORE DATABASE [AW_Restore] FROM DISK = N'/var/backups/aw.bak'" +
		" WITH MOVE N'AW_Data' TO N'/data/AW_Restore_Data.mdf', RECOVERY, REPLACE, STATS = 10"
	if got != want {
		t.Errorf("BuildRestoreStatement =\n%q\nwant\n%q", got, want)
	}
}

// TestBuildRestoreStatementNoRecovery covers the NORECOVERY branch (log
// shipping / tail-log restores), which is mutually exclusive with Recovery.
func TestBuildRestoreStatementNoRecovery(t *testing.T) {
	got, err := BuildRestoreStatement(RestoreOptions{
		Database:   "AW_Restore",
		Devices:    []string{`/var/backups/aw.bak`},
		NoRecovery: true,
	})
	if err != nil {
		t.Fatalf("BuildRestoreStatement: %v", err)
	}
	want := "RESTORE DATABASE [AW_Restore] FROM DISK = N'/var/backups/aw.bak' WITH NORECOVERY"
	if got != want {
		t.Errorf("BuildRestoreStatement =\n%q\nwant\n%q", got, want)
	}
}

// TestBuildRestoreStatementStandbyChecksumStopAt covers STANDBY, CHECKSUM,
// and a point-in-time STOPAT restore together.
func TestBuildRestoreStatementStandbyChecksumStopAt(t *testing.T) {
	stopAt := time.Date(2026, 7, 18, 12, 30, 0, 0, time.UTC)
	got, err := BuildRestoreStatement(RestoreOptions{
		Database: "AW_Restore",
		Devices:  []string{`/var/backups/aw.bak`},
		StandBy:  "/var/backups/aw_undo.bak",
		Checksum: true,
		StopAt:   &stopAt,
	})
	if err != nil {
		t.Fatalf("BuildRestoreStatement: %v", err)
	}
	want := "RESTORE DATABASE [AW_Restore] FROM DISK = N'/var/backups/aw.bak' WITH " +
		"STANDBY = N'/var/backups/aw_undo.bak', CHECKSUM, STOPAT = '2026-07-18T12:30:00'"
	if got != want {
		t.Errorf("BuildRestoreStatement =\n%q\nwant\n%q", got, want)
	}
}

// TestBuildRestoreStatementNoOptions confirms the bare minimum — just
// Database and Devices — produces no WITH clause at all.
func TestBuildRestoreStatementNoOptions(t *testing.T) {
	got, err := BuildRestoreStatement(RestoreOptions{
		Database: "AW_Restore",
		Devices:  []string{`/var/backups/aw.bak`},
	})
	if err != nil {
		t.Fatalf("BuildRestoreStatement: %v", err)
	}
	want := "RESTORE DATABASE [AW_Restore] FROM DISK = N'/var/backups/aw.bak'"
	if got != want {
		t.Errorf("BuildRestoreStatement =\n%q\nwant\n%q", got, want)
	}
}

func TestBuildRestoreStatementRequiresDatabaseAndDevices(t *testing.T) {
	if _, err := BuildRestoreStatement(RestoreOptions{}); err == nil {
		t.Error("BuildRestoreStatement with no database = nil error, want error")
	}
	if _, err := BuildRestoreStatement(RestoreOptions{Database: "x"}); err == nil {
		t.Error("BuildRestoreStatement with no devices = nil error, want error")
	}
}

func TestParseInt64(t *testing.T) {
	cases := []struct {
		in   string
		want int64
	}{
		{"5800000000", 5800000000},
		{"123.0", 123},
		{"", 0},
		{"abc", 0},
	}
	for _, c := range cases {
		if got := parseInt64(c.in); got != c.want {
			t.Errorf("parseInt64(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

// TestBuildBackupStatementFiles pins that BackupActionFiles renders as a
// BACKUP DATABASE carrying FILE = / FILEGROUP = clauses. There is no
// "BACKUP FILES" verb: the action name used to be pasted in literally,
// producing "BACKUP FILES [db] TO ..." — which the allowlist accepted and
// SQL Server rejects.
func TestBuildBackupStatementFiles(t *testing.T) {
	got, err := BuildBackupStatement(BackupOptions{
		Database:   "AppDB",
		Action:     BackupActionFiles,
		Files:      []string{"AppDB_dat2"},
		FileGroups: []string{"FG_Archive"},
		Devices:    []string{`C:\B\p.bak`},
	})
	if err != nil {
		t.Fatalf("BuildBackupStatement: %v", err)
	}
	want := `BACKUP DATABASE [AppDB] FILE = N'AppDB_dat2', FILEGROUP = N'FG_Archive' TO DISK = N'C:\B\p.bak'`
	if got != want {
		t.Errorf("BuildBackupStatement()\n got %q\nwant %q", got, want)
	}
	if strings.Contains(got, "BACKUP FILES") {
		t.Errorf("emitted the non-existent BACKUP FILES verb: %q", got)
	}
}

// TestBuildBackupStatementFilesNeedsATarget pins that the FILES action fails
// loudly with neither a file nor a filegroup, rather than degrading into a
// full BACKUP DATABASE that does far more work than asked.
func TestBuildBackupStatementFilesNeedsATarget(t *testing.T) {
	_, err := BuildBackupStatement(BackupOptions{
		Database: "AppDB", Action: BackupActionFiles, Devices: []string{"d.bak"},
	})
	if err == nil {
		t.Fatal("BuildBackupStatement(FILES with no file/filegroup) = nil error, want one")
	}
}

// TestBuildRestoreStatementFileNumber pins WITH FILE = n, without which a
// device holding several backup sets always restores the first.
func TestBuildRestoreStatementFileNumber(t *testing.T) {
	got, err := BuildRestoreStatement(RestoreOptions{
		Database: "AppDB", Devices: []string{"d.bak"}, FileNumber: 3, NoRecovery: true,
	})
	if err != nil {
		t.Fatalf("BuildRestoreStatement: %v", err)
	}
	if !strings.Contains(got, "WITH FILE = 3, NORECOVERY") {
		t.Errorf("BuildRestoreStatement() = %q, want it to carry WITH FILE = 3", got)
	}

	// Zero leaves the clause off entirely — SQL Server's own default is the
	// first set, so emitting "FILE = 0" would be an error rather than a no-op.
	got, err = BuildRestoreStatement(RestoreOptions{
		Database: "AppDB", Devices: []string{"d.bak"}, Recovery: true,
	})
	if err != nil {
		t.Fatalf("BuildRestoreStatement: %v", err)
	}
	if strings.Contains(got, "FILE =") {
		t.Errorf("BuildRestoreStatement() = %q, want no FILE clause for FileNumber 0", got)
	}
}

// TestBuildRestoreStatementFiles is the RESTORE counterpart of
// TestBuildBackupStatementFiles.
func TestBuildRestoreStatementFiles(t *testing.T) {
	got, err := BuildRestoreStatement(RestoreOptions{
		Database: "AppDB", Action: BackupActionFiles,
		Files: []string{"AppDB_dat2"}, Devices: []string{"d.bak"},
	})
	if err != nil {
		t.Fatalf("BuildRestoreStatement: %v", err)
	}
	if !strings.HasPrefix(got, `RESTORE DATABASE [AppDB] FILE = N'AppDB_dat2' FROM `) {
		t.Errorf("BuildRestoreStatement() = %q", got)
	}
	if strings.Contains(got, "RESTORE FILES") {
		t.Errorf("emitted the non-existent RESTORE FILES verb: %q", got)
	}
}
