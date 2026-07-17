package gosmo

import "testing"

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
