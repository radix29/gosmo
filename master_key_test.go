package gosmo

import (
	"context"
	"strings"
	"testing"
)

// EnsureMasterKey reads before it writes, so WithScript alone cannot test it:
// the recording driver (login_map_test.go) answers HasMasterKey, with a key
// in "Keyed" and none in "Bare".
func TestEnsureMasterKey(t *testing.T) {
	const pw = "p'wd"
	create := "CREATE MASTER KEY ENCRYPTION BY PASSWORD = N'p''wd'"
	cases := []struct {
		db      string
		created bool
	}{
		{"Bare", true},
		{"Keyed", false},
	}
	for _, c := range cases {
		t.Run(c.db, func(t *testing.T) {
			// Run: the CREATE goes to the database, or nothing is written.
			s := mapServer(t)
			created, err := s.DatabaseRef(c.db).EnsureMasterKey(context.Background(), pw)
			if err != nil || created != c.created {
				t.Fatalf("EnsureMasterKey = %v, %v; want %v, nil", created, err, c.created)
			}
			got := mapStatements("CREATE MASTER KEY")
			if !c.created {
				if len(got) != 0 {
					t.Fatalf("an existing key was created again: %v", got)
				}
			} else {
				if len(got) != 1 || got[0].q != create || strings.Trim(got[0].db, "[]") != c.db {
					t.Fatalf("CREATE = %v; want %q in %s", got, create, c.db)
				}
			}

			// Scripted: the read still asks the server, only the CREATE is
			// captured — and its password is a placeholder.
			s = mapServer(t)
			ctx, col := WithScript(context.Background())
			created, err = s.DatabaseRef(c.db).EnsureMasterKey(ctx, pw)
			if err != nil || created != c.created {
				t.Fatalf("scripted EnsureMasterKey = %v, %v; want %v, nil", created, err, c.created)
			}
			if got := mapStatements("CREATE MASTER KEY"); len(got) != 0 {
				t.Errorf("a scripted CREATE reached the server: %v", got)
			}
			if len(mapStatements("THEN 1 ELSE 0 END")) != 1 {
				t.Errorf("the HasMasterKey read did not reach the server")
			}
			script := col.String()
			if c.created != strings.Contains(script, "CREATE MASTER KEY ENCRYPTION BY PASSWORD = N'<insert ") {
				t.Errorf("script = %q; want a CREATE with a placeholder: %v", script, c.created)
			}
			if strings.Contains(script, "p''wd") {
				t.Errorf("script carries the password: %q", script)
			}
		})
	}
}

func TestEnsureMasterKeyRefusesAnEmptyPasswordOnlyWhenItWouldCreate(t *testing.T) {
	s := mapServer(t)
	if _, err := s.DatabaseRef("Keyed").EnsureMasterKey(context.Background(), ""); err != nil {
		t.Errorf("an existing key needs no password: %v", err)
	}
	if _, err := s.DatabaseRef("Bare").EnsureMasterKey(context.Background(), ""); err == nil {
		t.Error("creating a key with no password was not refused")
	}
}
