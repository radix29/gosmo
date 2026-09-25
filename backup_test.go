package gosmo

import (
	"context"
	"strings"
	"testing"
	"time"

	mssql "github.com/microsoft/go-mssqldb"
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

// TestNoticePercentLocalised pins that a WITH STATS notice is recognised by
// its message number, not its English text: sys.messages holds 3211 in 22
// languages, and several put the number mid-sentence.
func TestNoticePercentLocalised(t *testing.T) {
	cases := []struct {
		msg  string
		want int
	}{
		{"50 percent processed.", 50},
		{"50 Prozent verarbeitet.", 50},
		{"Bylo zpracováno 50 procent.", 50},
		{"Yüzde 50 işlendi.", 50},
		{"50 パーセント処理されました。", 50},
		{"已处理百分之 100。", 100},
		{"Przetworzono: 10 procent.", 10},
		{"30por ciento procesado.", 30},
	}
	for _, c := range cases {
		n := mssql.Error{Number: msgPercentProcessed, Message: c.msg}
		if got := noticePercent(n); got != c.want {
			t.Errorf("noticePercent(3211 %q) = %d, want %d", c.msg, got, c.want)
		}
	}
	// Another numbered notice carrying digits is not progress.
	if got := noticePercent(mssql.Error{Number: 4035, Message: "Processed 128 pages for database 'x', file 'x' on file 1."}); got != -1 {
		t.Errorf("noticePercent(4035) = %d, want -1", got)
	}
	// A notice that is not an mssql.Error falls back to the English text.
	if got := noticePercent(plainNotice("20 percent processed.")); got != 20 {
		t.Errorf("noticePercent(plain English) = %d, want 20", got)
	}
	if got := noticePercent(plainNotice("20 Prozent verarbeitet.")); got != -1 {
		t.Errorf("noticePercent(plain German) = %d, want -1", got)
	}
}

type plainNotice string

func (p plainNotice) String() string { return string(p) }

func TestBackupRequiresDatabaseAndDevices(t *testing.T) {
	s := &Server{}
	if err := s.Backup(t.Context(), BackupOptions{}); err == nil {
		t.Error("Backup with no database = nil error, want error")
	}
	if err := s.Backup(t.Context(), BackupOptions{Database: "AdventureWorks"}); err == nil {
		t.Error("Backup with no devices = nil error, want error")
	}
}

func TestBuildBackupStatementDifferential(t *testing.T) {
	got, err := BuildBackupStatement(BackupOptions{
		Database: "AdventureWorks",
		Action:   BackupActionDifferential,
		Devices:  []BackupTarget{DiskTarget(`/var/backups/aw_diff.bak`)},
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
		Devices:  []BackupTarget{DiskTarget(`/var/backups/aw.bak`)},
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
		Devices:  []BackupTarget{DiskTarget(`/var/backups/aw1.trn`), DiskTarget(`/var/backups/aw2.trn`)},
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
		Devices:          []BackupTarget{DiskTarget(`/var/backups/aw.bak`)},
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
		Devices:     []BackupTarget{DiskTarget(`/var/backups/aw.bak`)},
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
	got, err := (&Server{}).BuildRestoreStatement(RestoreOptions{
		Database: "AW_Restore",
		Devices:  []BackupTarget{DiskTarget(`/var/backups/aw.bak`)},
		RelocateFiles: []RelocateFile{
			{LogicalName: "AW_Data", PhysicalName: "/data/AW_Restore_Data.mdf"},
		},
		Recovery: RestoreWithRecovery,
		Replace:  true,
		Stats:    10,
	})
	if err != nil {
		t.Fatalf("BuildRestoreStatement: %v", err)
	}
	want := "RESTORE DATABASE [AW_Restore]\n" +
		"FROM DISK = N'/var/backups/aw.bak'\n" +
		"WITH MOVE N'AW_Data' TO N'/data/AW_Restore_Data.mdf',\n" +
		"     RECOVERY,\n" +
		"     REPLACE,\n" +
		"     STATS = 10"
	if got != want {
		t.Errorf("BuildRestoreStatement =\n%q\nwant\n%q", got, want)
	}
}

// TestBuildRestoreStatementNoRecovery covers the NORECOVERY branch (log
// shipping / tail-log restores).
func TestBuildRestoreStatementNoRecovery(t *testing.T) {
	got, err := (&Server{}).BuildRestoreStatement(RestoreOptions{
		Database: "AW_Restore",
		Devices:  []BackupTarget{DiskTarget(`/var/backups/aw.bak`)},
		Recovery: RestoreWithNoRecovery,
	})
	if err != nil {
		t.Fatalf("BuildRestoreStatement: %v", err)
	}
	want := "RESTORE DATABASE [AW_Restore]\nFROM DISK = N'/var/backups/aw.bak'\nWITH NORECOVERY"
	if got != want {
		t.Errorf("BuildRestoreStatement =\n%q\nwant\n%q", got, want)
	}
}

// TestBuildRestoreStatementStandbyChecksumStopAt covers STANDBY, CHECKSUM,
// and a point-in-time STOPAT restore together.
func TestBuildRestoreStatementStandbyChecksumStopAt(t *testing.T) {
	stopAt := time.Date(2026, 7, 18, 12, 30, 0, 0, time.UTC)
	got, err := (&Server{}).BuildRestoreStatement(RestoreOptions{
		Database:    "AW_Restore",
		Devices:     []BackupTarget{DiskTarget(`/var/backups/aw.bak`)},
		Recovery:    RestoreWithStandBy,
		StandByFile: "/var/backups/aw_undo.bak",
		Checksum:    true,
		StopAt:      &stopAt,
	})
	if err != nil {
		t.Fatalf("BuildRestoreStatement: %v", err)
	}
	want := "RESTORE DATABASE [AW_Restore]\n" +
		"FROM DISK = N'/var/backups/aw.bak'\n" +
		"WITH STANDBY = N'/var/backups/aw_undo.bak',\n" +
		"     CHECKSUM,\n" +
		"     STOPAT = '2026-07-18T12:30:00.000'"
	if got != want {
		t.Errorf("BuildRestoreStatement =\n%q\nwant\n%q", got, want)
	}
}

// STOPAT is read as server-local time and takes milliseconds. The rendering
// keeps them — a log restore stopped a second early can miss the very
// transaction the point-in-time restore was for — and makes no zone
// conversion: the wall-clock fields go out as written whatever Location the
// time carries, which is what a caller passing server-local times relies on.
func TestBuildRestoreStatementStopAtKeepsMillisecondsAndWallClock(t *testing.T) {
	east := time.FixedZone("UTC+3", 3*60*60)
	for _, c := range []struct {
		name string
		at   time.Time
		want string
	}{
		{"milliseconds", time.Date(2026, 7, 18, 12, 30, 5, 123_456_789, time.UTC), "STOPAT = '2026-07-18T12:30:05.123'"},
		{"non-UTC zone, no conversion", time.Date(2026, 7, 18, 12, 30, 5, 7_000_000, east), "STOPAT = '2026-07-18T12:30:05.007'"},
	} {
		got, err := (&Server{}).BuildRestoreStatement(RestoreOptions{
			Database: "AW_Restore",
			Devices:  []BackupTarget{DiskTarget(`/var/backups/aw.bak`)},
			StopAt:   &c.at,
		})
		if err != nil {
			t.Fatalf("%s: BuildRestoreStatement: %v", c.name, err)
		}
		if !strings.HasSuffix(got, c.want) {
			t.Errorf("%s: statement = %q, want it to end %q", c.name, got, c.want)
		}
	}
}

// TestBuildRestoreStatementNoOptions confirms the bare minimum — just
// Database and Devices — produces no WITH clause at all.
func TestBuildRestoreStatementNoOptions(t *testing.T) {
	got, err := (&Server{}).BuildRestoreStatement(RestoreOptions{
		Database: "AW_Restore",
		Devices:  []BackupTarget{DiskTarget(`/var/backups/aw.bak`)},
	})
	if err != nil {
		t.Fatalf("BuildRestoreStatement: %v", err)
	}
	want := "RESTORE DATABASE [AW_Restore]\nFROM DISK = N'/var/backups/aw.bak'"
	if got != want {
		t.Errorf("BuildRestoreStatement =\n%q\nwant\n%q", got, want)
	}
}

func TestBuildRestoreStatementRequiresDatabaseAndDevices(t *testing.T) {
	if _, err := (&Server{}).BuildRestoreStatement(RestoreOptions{}); err == nil {
		t.Error("BuildRestoreStatement with no database = nil error, want error")
	}
	if _, err := (&Server{}).BuildRestoreStatement(RestoreOptions{Database: "x"}); err == nil {
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
		Devices:    []BackupTarget{DiskTarget(`C:\B\p.bak`)},
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
		Database: "AppDB", Action: BackupActionFiles, Devices: []BackupTarget{DiskTarget("d.bak")},
	})
	if err == nil {
		t.Fatal("BuildBackupStatement(FILES with no file/filegroup) = nil error, want one")
	}
}

// TestBuildRestoreStatementFileNumber pins WITH FILE = n, without which a
// device holding several backup sets always restores the first.
func TestBuildRestoreStatementFileNumber(t *testing.T) {
	got, err := (&Server{}).BuildRestoreStatement(RestoreOptions{
		Database: "AppDB", Devices: []BackupTarget{DiskTarget("d.bak")}, FileNumber: 3, Recovery: RestoreWithNoRecovery,
	})
	if err != nil {
		t.Fatalf("BuildRestoreStatement: %v", err)
	}
	if !strings.Contains(got, "WITH FILE = 3,\n     NORECOVERY") {
		t.Errorf("(&Server{}).BuildRestoreStatement() = %q, want it to carry WITH FILE = 3", got)
	}

	// Zero leaves the clause off entirely — SQL Server's own default is the
	// first set, so emitting "FILE = 0" would be an error rather than a no-op.
	got, err = (&Server{}).BuildRestoreStatement(RestoreOptions{
		Database: "AppDB", Devices: []BackupTarget{DiskTarget("d.bak")}, Recovery: RestoreWithRecovery,
	})
	if err != nil {
		t.Fatalf("BuildRestoreStatement: %v", err)
	}
	if strings.Contains(got, "FILE =") {
		t.Errorf("(&Server{}).BuildRestoreStatement() = %q, want no FILE clause for FileNumber 0", got)
	}
}

// TestBuildRestoreStatementFiles is the RESTORE counterpart of
// TestBuildBackupStatementFiles.
func TestBuildRestoreStatementFiles(t *testing.T) {
	got, err := (&Server{}).BuildRestoreStatement(RestoreOptions{
		Database: "AppDB", Action: BackupActionFiles,
		Files: []string{"AppDB_dat2"}, Devices: []BackupTarget{DiskTarget("d.bak")},
	})
	if err != nil {
		t.Fatalf("BuildRestoreStatement: %v", err)
	}
	if !strings.HasPrefix(got, "RESTORE DATABASE [AppDB] FILE = N'AppDB_dat2'\nFROM ") {
		t.Errorf("(&Server{}).BuildRestoreStatement() = %q", got)
	}
	if strings.Contains(got, "RESTORE FILES") {
		t.Errorf("emitted the non-existent RESTORE FILES verb: %q", got)
	}
}

// The file list must name the same backup set the restore names, or the
// MOVE clauses built from it describe files the restored set doesn't
// contain. Position is 1-based; 0 means "no clause", which SQL Server reads
// as the first set.
func TestBackupFileListQuerySelectsTheSet(t *testing.T) {
	cases := []struct {
		device BackupTarget
		file   int
		want   string
	}{
		{DiskTarget(`C:\bk\db.bak`), 0, `RESTORE FILELISTONLY FROM DISK = N'C:\bk\db.bak'`},
		{DiskTarget(`C:\bk\db.bak`), 1, `RESTORE FILELISTONLY FROM DISK = N'C:\bk\db.bak' WITH FILE = 1`},
		{DiskTarget(`/var/opt/mssql/data/db.bak`), 3, `RESTORE FILELISTONLY FROM DISK = N'/var/opt/mssql/data/db.bak' WITH FILE = 3`},
		// A negative number is not a set, so it must not reach the server.
		{DiskTarget(`/tmp/db.bak`), -2, `RESTORE FILELISTONLY FROM DISK = N'/tmp/db.bak'`},
		// A device path carrying an apostrophe stays quoted.
		{DiskTarget(`/tmp/o'brien.bak`), 2, `RESTORE FILELISTONLY FROM DISK = N'/tmp/o''brien.bak' WITH FILE = 2`},
		// A logical backup device is named bare, never as DISK = N'name' —
		// that reads the name as a file path in the default backup directory.
		{DeviceTarget("NightlyDev"), 0, `RESTORE FILELISTONLY FROM [NightlyDev]`},
		{DeviceTarget("Nightly]Dev"), 2, `RESTORE FILELISTONLY FROM [Nightly]]Dev] WITH FILE = 2`},
	}
	for _, c := range cases {
		if got := backupFileListQuery(c.file, []BackupTarget{c.device}); got != c.want {
			t.Errorf("backupFileListQuery(%q, %d) =\n  %s\nwant\n  %s", c.device, c.file, got, c.want)
		}
	}
}

// The URL device tests below are Azure SQL Managed Instance's requirement:
// it answers any TO DISK / FROM DISK with Msg 41902, "supports database
// restore from URI backup device only". See docs/plan-azure-managed-instance.md
// in goSSMS § 3 for the live capture.

func TestIsBackupURL(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"https://acct.blob.core.windows.net/c/db.bak", true},
		{"http://acct.blob.core.windows.net/c/db.bak", true},
		{"HTTPS://acct.blob.core.windows.net/c/db.bak", true},
		{"  https://acct.blob.core.windows.net/c/db.bak  ", true},
		{`C:\Backups\db.bak`, false},
		{"/var/opt/mssql/backup/db.bak", false},
		{`\\fileserver\share\db.bak`, false}, // a UNC path is a DISK device
		{"db.bak", false},
		{"", false},
		{"http:/", false}, // too short to carry a scheme
		// A user typing a URL into the destination field passes through every
		// prefix of it, so every prefix has to answer without panicking.
		{"http://", true},
		{"https:/", false},
		{"https://", true},
		{"h", false},
	}
	for _, c := range cases {
		if got := IsBackupURL(c.in); got != c.want {
			t.Errorf("IsBackupURL(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestBuildBackupStatementToURL(t *testing.T) {
	got, err := BuildBackupStatement(BackupOptions{
		Database: "GoTest01",
		Devices:  []BackupTarget{DiskTarget("https://acct.blob.core.windows.net/backups/GoTest01_full.bak")},
		CopyOnly: true,
	})
	if err != nil {
		t.Fatalf("BuildBackupStatement: %v", err)
	}
	want := "BACKUP DATABASE [GoTest01] TO URL = N'https://acct.blob.core.windows.net/backups/GoTest01_full.bak' WITH COPY_ONLY"
	if got != want {
		t.Errorf("BuildBackupStatement =\n%q\nwant\n%q", got, want)
	}
}

func TestBuildRestoreStatementFromURL(t *testing.T) {
	got, err := (&Server{}).BuildRestoreStatement(RestoreOptions{
		Database: "GoTest01",
		Devices:  []BackupTarget{DiskTarget("https://acct.blob.core.windows.net/backups/GoTest01_full.bak")},
	})
	if err != nil {
		t.Fatalf("BuildRestoreStatement: %v", err)
	}
	if !strings.Contains(got, "FROM URL = N'https://acct.blob.core.windows.net/backups/GoTest01_full.bak'") {
		t.Errorf("BuildRestoreStatement =\n%s\nwant a FROM URL device", got)
	}
	if strings.Contains(got, "DISK") {
		t.Errorf("BuildRestoreStatement =\n%s\nstill names a DISK device", got)
	}
}

// A stripe set mixing a blob and a path is nonsense to SQL Server, but each
// device is still classified on its own — the alternative, letting the first
// device decide for the rest, hides which half of the pair is wrong.
func TestBuildBackupStatementClassifiesEachDevice(t *testing.T) {
	got, err := BuildBackupStatement(BackupOptions{
		Database: "GoTest01",
		Devices: []BackupTarget{
			DiskTarget("https://acct.blob.core.windows.net/backups/a.bak"),
			DiskTarget(`C:\Backups\b.bak`),
		},
	})
	if err != nil {
		t.Fatalf("BuildBackupStatement: %v", err)
	}
	want := `BACKUP DATABASE [GoTest01] TO URL = N'https://acct.blob.core.windows.net/backups/a.bak', DISK = N'C:\Backups\b.bak'`
	if got != want {
		t.Errorf("BuildBackupStatement =\n%q\nwant\n%q", got, want)
	}
}

// DiskTarget classifying a URL for itself is what makes the three RESTORE-side
// reads work against a blob a caller named with DiskTarget: VerifyBackup,
// BackupHeaders and BackupFileList read it as a URL. TestBackupTargetClause in
// backup_device_write_test.go covers the path and logical-device cases.
func TestBackupTargetClauseURL(t *testing.T) {
	const blob = "https://acct.blob.core.windows.net/c/db.bak"
	want := "URL = N'" + blob + "'"
	if got := DiskTarget(blob).clause(); got != want {
		t.Errorf("DiskTarget(URL).clause() = %q, want %q", got, want)
	}
	if got := URLTarget(blob).clause(); got != want {
		t.Errorf("URLTarget.clause() = %q, want %q", got, want)
	}
}

// The RESTORE-side reads reach a blob as a URL device without any caller
// having to say so — a Managed Instance refuses all three as DISK.
func TestRestoreSideReadsUseURLForABlob(t *testing.T) {
	const blob = "https://acct.blob.core.windows.net/c/db.bak"
	ctx, col := WithScript(context.Background())
	if err := (&Server{}).VerifyBackup(ctx, DiskTarget(blob)); err != nil {
		t.Fatalf("VerifyBackup: %v", err)
	}
	want := "RESTORE VERIFYONLY FROM URL = N'" + blob + "'"
	if len(col.Statements()) != 1 || col.Statements()[0] != want {
		t.Errorf("got %v, want [%s]", col.Statements(), want)
	}
}

// A striped backup is one media set, and each RESTORE-side read has to name
// every family of it — the server refuses one stripe of two. An empty list is
// refused before it reaches the server as a statement with no FROM operand.
func TestRestoreSideReadsNameEveryFamily(t *testing.T) {
	ctx, col := WithScript(context.Background())
	s := &Server{}
	if err := s.VerifyBackup(ctx, DiskTarget(`/b/s1.bak`), DiskTarget(`/b/s2.bak`)); err != nil {
		t.Fatalf("VerifyBackup: %v", err)
	}
	want := `RESTORE VERIFYONLY FROM DISK = N'/b/s1.bak', DISK = N'/b/s2.bak'`
	if got := col.Statements(); len(got) != 1 || got[0] != want {
		t.Errorf("VerifyBackup = %v, want [%s]", got, want)
	}
	two := []BackupTarget{DiskTarget(`/b/s1.bak`), DiskTarget(`/b/s2.bak`)}
	if got, want := backupFileListQuery(3, two),
		`RESTORE FILELISTONLY FROM DISK = N'/b/s1.bak', DISK = N'/b/s2.bak' WITH FILE = 3`; got != want {
		t.Errorf("backupFileListQuery = %s, want %s", got, want)
	}
	if err := s.VerifyBackup(ctx); err == nil {
		t.Error("VerifyBackup with no device: want an error")
	}
	if _, err := s.BackupHeaders(ctx); err == nil {
		t.Error("BackupHeaders with no device: want an error")
	}
	if _, err := s.BackupFileList(ctx, 1); err == nil {
		t.Error("BackupFileList with no device: want an error")
	}
}

func TestBuildStatementsWithCredential(t *testing.T) {
	b, err := BuildBackupStatement(BackupOptions{
		Database:   "GoTest01",
		Devices:    []BackupTarget{DiskTarget("https://acct.blob.core.windows.net/backups/a.bak")},
		Credential: "AzureStorage",
	})
	if err != nil {
		t.Fatalf("BuildBackupStatement: %v", err)
	}
	if !strings.Contains(b, "CREDENTIAL = N'AzureStorage'") {
		t.Errorf("BuildBackupStatement =\n%s\nwant a CREDENTIAL clause", b)
	}
	r, err := (&Server{}).BuildRestoreStatement(RestoreOptions{
		Database:   "GoTest01",
		Devices:    []BackupTarget{DiskTarget("https://acct.blob.core.windows.net/backups/a.bak")},
		Credential: "AzureStorage",
	})
	if err != nil {
		t.Fatalf("BuildRestoreStatement: %v", err)
	}
	if !strings.Contains(r, "CREDENTIAL = N'AzureStorage'") {
		t.Errorf("BuildRestoreStatement =\n%s\nwant a CREDENTIAL clause", r)
	}
}

// Recovery is one choice. As three fields, NoRecovery silently won when both
// booleans were set, and a STANDBY path rode alongside either.
func TestBuildRestoreStatementRecoveryIsOneChoice(t *testing.T) {
	base := RestoreOptions{Database: "AppDB", Devices: []BackupTarget{DiskTarget("d.bak")}}
	for _, c := range []struct {
		name     string
		recovery RestoreRecovery
		standBy  string
		want     string // the WITH clause; "" for none, "error" for a refusal
	}{
		{"default", RestoreRecoveryDefault, "", ""},
		{"recovery", RestoreWithRecovery, "", "\nWITH RECOVERY"},
		{"norecovery", RestoreWithNoRecovery, "", "\nWITH NORECOVERY"},
		{"standby", RestoreWithStandBy, `C:\u'ndo.bak`, `\nWITH STANDBY = N'C:\u''ndo.bak'`},
		{"standby without a file", RestoreWithStandBy, "", "error"},
		{"a file without standby", RestoreWithNoRecovery, "u.bak", "error"},
		{"a file with the default", RestoreRecoveryDefault, "u.bak", "error"},
		{"unknown", RestoreRecovery("RECOVERY; DROP DATABASE x"), "", "error"},
	} {
		opts := base
		opts.Recovery, opts.StandByFile = c.recovery, c.standBy
		got, err := (&Server{}).BuildRestoreStatement(opts)
		if c.want == "error" {
			if err == nil {
				t.Errorf("%s: built %q, want an error", c.name, got)
			}
			continue
		}
		want := strings.ReplaceAll("RESTORE DATABASE [AppDB]\nFROM DISK = N'd.bak'"+c.want, `\n`, "\n")
		if err != nil || got != want {
			t.Errorf("%s: got %q, %v; want %q", c.name, got, err, want)
		}
	}
}

// Q4 of the 2026-09-22 review: closing connections as its own round trip left
// the single-user slot free until the RESTORE arrived, so SINGLE_USER, the
// RESTORE and the release are one batch. The ALTERs are guarded — a database
// that does not exist yet, is RESTORING or is in STANDBY refuses them, and a
// refusal would abort the RESTORE after it — and only an access mode the batch
// itself set is released. A STANDBY database's readers are killed instead
// (G11, 2026-09-24): left alone, they failed a log-shipping secondary's next
// log restore with "Exclusive access could not be obtained".
func TestBuildRestoreStatementClosesConnectionsInTheSameBatch(t *testing.T) {
	got, err := (&Server{}).BuildRestoreStatement(RestoreOptions{
		Database: "App'DB", Devices: []BackupTarget{DiskTarget("d.bak")}, Replace: true,
		CloseExistingConnections: true,
	})
	if err != nil {
		t.Fatalf("BuildRestoreStatement: %v", err)
	}
	online := "EXISTS (SELECT 1 FROM sys.databases WHERE name = N'App''DB' AND state = 0 AND is_in_standby = 0)"
	standby := "EXISTS (SELECT 1 FROM sys.databases WHERE name = N'App''DB' AND state = 0 AND is_in_standby = 1)"
	want := "DECLARE @closed bit = 0;\n" +
		"IF " + online + "\n" +
		"BEGIN\n" +
		"    ALTER DATABASE [App'DB] SET SINGLE_USER WITH ROLLBACK IMMEDIATE;\n" +
		"    SET @closed = 1;\n" +
		"END\n" +
		"ELSE IF " + standby + "\n" +
		"BEGIN\n" +
		killDatabaseSessionsBatch("App'DB") + ";\n" +
		"END;\n" +
		"RESTORE DATABASE [App'DB]\nFROM DISK = N'd.bak'\nWITH REPLACE;\n" +
		"IF @closed = 1 AND " + online + "\n" +
		"    ALTER DATABASE [App'DB] SET MULTI_USER;"
	if got != want {
		t.Errorf("BuildRestoreStatement =\n%s\nwant\n%s", got, want)
	}
}

// A Managed Instance refuses SET SINGLE_USER (Msg 5008), so the dialog failed
// on the preparation step there. The instance-aware builder kills the sessions
// instead, in the same batch, and touches no access mode.
func TestServerBuildRestoreStatementKillsSessionsOnAManagedInstance(t *testing.T) {
	opts := RestoreOptions{Database: "AppDB", Devices: []BackupTarget{DiskTarget("d.bak")}, CloseExistingConnections: true}
	mi := &Server{info: &ServerInfo{EngineEdition: int(EngineAzureManagedInst)}}
	got, err := mi.BuildRestoreStatement(opts)
	if err != nil {
		t.Fatalf("BuildRestoreStatement: %v", err)
	}
	if !strings.HasPrefix(got, killDatabaseSessionsBatch("AppDB")+";\n") ||
		!strings.HasSuffix(got, "RESTORE DATABASE [AppDB]\nFROM DISK = N'd.bak';") {
		t.Errorf("Managed Instance statement =\n%s\nwant the KILL batch then the RESTORE", got)
	}
	if strings.Contains(got, "_USER") {
		t.Errorf("Managed Instance statement sets an access mode it refuses:\n%s", got)
	}

	onPrem := &Server{info: &ServerInfo{EngineEdition: int(EngineEnterprise)}}
	got, err = onPrem.BuildRestoreStatement(opts)
	want, _ := (&Server{}).BuildRestoreStatement(opts)
	if err != nil || got != want {
		t.Errorf("on-premises statement = %q, %v; want the SINGLE_USER form a server of unknown edition writes, %q", got, err, want)
	}
	if !strings.Contains(got, "SET SINGLE_USER") {
		t.Errorf("on-premises statement does not set SINGLE_USER:\n%s", got)
	}

	opts.CloseExistingConnections = false
	got, _ = mi.BuildRestoreStatement(opts)
	if strings.Contains(got, "KILL") {
		t.Errorf("KILLed sessions without CloseExistingConnections:\n%s", got)
	}
}

// The batch's own MULTI_USER does not run when the batch is cut short, and a
// cancel is the likeliest way for a long restore to fail — so Restore
// releases the database again, off the caller's cancellation.
func TestARestoreCutShortIsPutBackToMultiUser(t *testing.T) {
	s := detServer(t)
	ctx := detCancelOn(t, "RESTORE DATABASE")
	err := s.Restore(ctx, RestoreOptions{
		Database: "appdb", Devices: []BackupTarget{DiskTarget("d.bak")}, CloseExistingConnections: true,
	})
	if err == nil {
		t.Fatal("a restore whose context was cancelled returned no error")
	}
	stmts := detLog.statements()
	if last := stmts[len(stmts)-1]; last != "ALTER DATABASE [appdb] SET MULTI_USER" {
		t.Errorf("last statement after a cancelled restore is %q, want the MULTI_USER repair: %v", last, stmts)
	}
}

// Without CloseExistingConnections nothing set the access mode, so a failed
// restore must not touch it — nor on a Managed Instance, where none was set.
func TestAFailedRestoreLeavesAnAccessModeItDidNotSet(t *testing.T) {
	for _, c := range []struct {
		name  string
		mi    bool
		close bool
	}{{"no close", false, false}, {"managed instance", true, true}} {
		s := detServer(t)
		if c.mi {
			s.info = &ServerInfo{EngineEdition: int(EngineAzureManagedInst)}
		}
		detLog.mu.Lock()
		detLog.failOn = "RESTORE DATABASE"
		detLog.mu.Unlock()
		err := s.Restore(context.Background(), RestoreOptions{
			Database: "appdb", Devices: []BackupTarget{DiskTarget("d.bak")}, CloseExistingConnections: c.close,
		})
		if err == nil {
			t.Fatalf("%s: a failing restore returned no error", c.name)
		}
		if stmts := detLog.statements(); len(stmts) != 1 {
			t.Errorf("%s: statements %v, want only the restore", c.name, stmts)
		}
	}
}

// A logical backup device is named bare on both sides. With Devices as plain
// strings it could not be said at all: the name became DISK = N'devname', a
// file of that name in the default backup directory.
func TestBackupAndRestoreToALogicalDevice(t *testing.T) {
	got, err := BuildBackupStatement(BackupOptions{
		Database: "AppDB",
		Devices:  []BackupTarget{DeviceTarget("Nightly]Dev")},
	})
	if err != nil {
		t.Fatalf("BuildBackupStatement: %v", err)
	}
	if want := "BACKUP DATABASE [AppDB] TO [Nightly]]Dev]"; got != want {
		t.Errorf("backup = %q, want %q", got, want)
	}
	got, err = (&Server{}).BuildRestoreStatement(RestoreOptions{
		Database: "AppDB",
		Devices:  []BackupTarget{DeviceTarget("Nightly]Dev")},
	})
	if err != nil {
		t.Fatalf("BuildRestoreStatement: %v", err)
	}
	if want := "RESTORE DATABASE [AppDB]\nFROM [Nightly]]Dev]"; got != want {
		t.Errorf("restore = %q, want %q", got, want)
	}
}

// The zero BackupTarget has no name and would render as DISK = N”.
func TestZeroBackupTargetIsRefused(t *testing.T) {
	if _, err := BuildBackupStatement(BackupOptions{Database: "AppDB", Devices: []BackupTarget{{}}}); err == nil {
		t.Error("backup to a zero BackupTarget built a statement, want an error")
	}
	if _, err := (&Server{}).BuildRestoreStatement(RestoreOptions{Database: "AppDB", Devices: []BackupTarget{DiskTarget("a.bak"), {}}}); err == nil {
		t.Error("restore from a zero BackupTarget built a statement, want an error")
	}
}

// A failed BACKUP sends the cause first and "terminating abnormally" after
// it. The progress path returned at the first message and the plain path at
// the last; both must report both, as exec's withAllMessages does.
func TestProgressErrorKeepsEveryMessage(t *testing.T) {
	msgs := []mssql.Error{
		{Number: 3201, Class: 16, Message: "Cannot open backup device 'X:\\nope.bak'."},
		{Number: 3013, Class: 16, Message: "BACKUP DATABASE is terminating abnormally."},
	}
	err := progressError(msgs, nil, nil)
	for _, m := range msgs {
		if !strings.Contains(err.Error(), m.Message) {
			t.Errorf("error %q does not carry %q", err, m.Message)
		}
	}
	if se, ok := AsSQLError(err); !ok || se.Number != 3013 {
		t.Errorf("AsSQLError = %+v, %v; want the last message, 3013, as the headline", se, ok)
	}
	if err := progressError(nil, nil, nil); err != nil {
		t.Errorf("no messages: %v, want nil", err)
	}
}

// The progress path runs its statement on a connection of its own, which
// WithScript cannot intercept: a Backup or Restore with Progress set under a
// scripting context ran for real. Both take exec's path there.
func TestProgressIsIgnoredUnderWithScript(t *testing.T) {
	ctx, col := WithScript(context.Background())
	progress := func(int, string) {}
	s := &Server{}
	if err := s.Backup(ctx, BackupOptions{Database: "AppDB", Devices: []BackupTarget{DiskTarget("a.bak")}, Progress: progress}); err != nil {
		t.Fatalf("Backup: %v", err)
	}
	if err := s.Restore(ctx, RestoreOptions{Database: "AppDB", Devices: []BackupTarget{DiskTarget("a.bak")}, Progress: progress}); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if n := len(col.Statements()); n != 2 {
		t.Errorf("collected %d statements, want the BACKUP and the RESTORE: %v", n, col.Statements())
	}
}
