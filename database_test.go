package gosmo

import "testing"

func TestDatabaseIsSystem(t *testing.T) {
	cases := []struct {
		id   int
		want bool
	}{
		{1, true},  // master
		{2, true},  // tempdb
		{3, true},  // model
		{4, true},  // msdb
		{5, false}, // first user database id
		{0, false},
		{100, false},
	}
	for _, c := range cases {
		d := &Database{id: c.id}
		if got := d.IsSystem(); got != c.want {
			t.Errorf("Database{id: %d}.IsSystem() = %v, want %v", c.id, got, c.want)
		}
	}
}
