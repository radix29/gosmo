package gosmo

import "testing"

func TestValidServerPermission(t *testing.T) {
	if !validServerPermission("CONTROL SERVER") {
		t.Error("CONTROL SERVER should be a recognized server permission")
	}
	if !validServerPermission("CONNECT SQL") {
		t.Error("CONNECT SQL should be a recognized server permission")
	}
	if validServerPermission("DROP TABLE students; --") {
		t.Error("an injection attempt was accepted as a valid server permission")
	}
	if validServerPermission("") {
		t.Error("empty string should not be a recognized server permission")
	}
}

func TestGrantServerPermissionRejectsUnknownPermission(t *testing.T) {
	s := &Server{}
	err := s.GrantServerPermission("CONTROL SERVER; DROP DATABASE master; --", "attacker")
	if err == nil {
		t.Fatal("GrantServerPermission accepted an unrecognized permission name, want an error")
	}
}

func TestDenyAndRevokeServerPermissionRejectUnknownPermission(t *testing.T) {
	s := &Server{}
	if err := s.DenyServerPermission("NOT A REAL PERMISSION", "sa"); err == nil {
		t.Error("DenyServerPermission accepted an unrecognized permission, want an error")
	}
	if err := s.RevokeServerPermission("NOT A REAL PERMISSION", "sa"); err == nil {
		t.Error("RevokeServerPermission accepted an unrecognized permission, want an error")
	}
}

func TestValidDatabasePermission(t *testing.T) {
	if !validDatabasePermission("CONNECT") {
		t.Error("CONNECT should be a recognized database permission")
	}
	if !validDatabasePermission("CREATE TABLE") {
		t.Error("CREATE TABLE should be a recognized database permission")
	}
	if validDatabasePermission("SELECT * FROM users; --") {
		t.Error("an injection attempt was accepted as a valid database permission")
	}
}

func TestGrantDatabasePermissionRejectsUnknownPermission(t *testing.T) {
	d := &Database{name: "appdb", server: &Server{}}
	err := d.GrantDatabasePermission("CONTROL; DROP TABLE Users; --", "attacker")
	if err == nil {
		t.Fatal("GrantDatabasePermission accepted an unrecognized permission name, want an error")
	}
}

func TestValidDatabaseOption(t *testing.T) {
	if !validDatabaseOption(DBOptAutoClose) {
		t.Error("DBOptAutoClose should be a recognized database option")
	}
	if validDatabaseOption(DatabaseOption("DROP DATABASE appdb")) {
		t.Error("an injection attempt was accepted as a valid database option")
	}
}

func TestIsSimpleSetValue(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"ON", true},
		{"OFF", true},
		{"CHECKSUM", true},
		{"SNAPSHOT_ISOLATION", true},
		{"", false},
		{"ON; DROP TABLE Users; --", false},
		{"ON'", false},
		{"ON/*", false},
	}
	for _, c := range cases {
		if got := isSimpleSetValue(c.in); got != c.want {
			t.Errorf("isSimpleSetValue(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestSetDatabaseOptionRejectsUnknownOptionAndUnsafeValue(t *testing.T) {
	d := &Database{name: "appdb", server: &Server{}}
	if err := d.SetDatabaseOption(DatabaseOption("EVIL_OPTION"), "ON"); err == nil {
		t.Error("SetDatabaseOption accepted an unrecognized option, want an error")
	}
	if err := d.SetDatabaseOption(DBOptAutoClose, "ON; DROP DATABASE appdb; --"); err == nil {
		t.Error("SetDatabaseOption accepted an unsafe value, want an error")
	}
}
