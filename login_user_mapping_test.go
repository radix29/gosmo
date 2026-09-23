package gosmo

import (
	"database/sql/driver"
	"slices"
	"testing"
)

// A role whose name holds ", " came back as two roles while the roles were
// read as one comma-joined string and split. They are now one row each,
// grouped in Go, and a user in no role reads as no roles rather than one
// empty-named one.
func TestUserMappingsGroupRolesWithoutSplittingNames(t *testing.T) {
	tbl := captureTable(t)
	db := tbl.db
	captured.reset(cannedRow{
		match: "sys.database_role_members rm",
		cols:  []string{"principal_id", "name", "default_schema_name", "role"},
		rows: [][]driver.Value{
			{int64(5), "app", "dbo", "db_datareader"},
			{int64(5), "app", "dbo", "readers, and writers"},
			{int64(9), "app_alias", "", nil},
		},
	})
	l := &Login{server: db.server, Name: "app", SID: []byte{1}}
	got, err := l.userMappingsIn(t.Context(), db, false)
	if err != nil {
		t.Fatalf("userMappingsIn: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d mappings, want 2: %+v", len(got), got)
	}
	if want := []string{"db_datareader", "readers, and writers"}; !slices.Equal(got[0].Roles, want) {
		t.Errorf("roles = %q, want %q", got[0].Roles, want)
	}
	if got[0].User != "app" || got[0].DefaultSchema != "dbo" || got[0].Database != db.Name {
		t.Errorf("mapping = %+v", got[0])
	}
	if got[1].User != "app_alias" || got[1].Roles != nil {
		t.Errorf("second mapping = %+v, want app_alias with no roles", got[1])
	}
}
