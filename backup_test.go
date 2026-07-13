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
