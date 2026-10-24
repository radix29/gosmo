package gosmo

import (
	"database/sql/driver"
	"reflect"
	"strings"
	"testing"
	"time"
)

var userCols = strings.Fields("name id type schema created modified auth sid login disabled mapped")

// userRow is one userSelect row with every column populated.
func userRow() []driver.Value {
	created := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	return []driver.Value{"cert_user", int64(5), "CERTIFICATE_MAPPED_USER", "dbo",
		created, created.Add(time.Hour), "INSTANCE", []byte{1, 2, 3},
		"cert_login", true, "signing_cert"}
}

// TestUsersFillsWhatUserByNameFills pins the rule in CLAUDE.md § Conventions:
// a field a type declares is filled by every read returning it. Users used to
// leave SID, LoginName, LoginDisabled and MappedObject zero — values that
// read as "no SID", "orphaned" and "key dropped".
func TestUsersFillsWhatUserByNameFills(t *testing.T) {
	d := qsRecDB(t, 17, userCols, [][]driver.Value{userRow()})
	users, err := d.Users(t.Context())
	if err != nil || len(users) != 1 {
		t.Fatalf("Users = %v, %v; want one user", users, err)
	}
	byName, err := d.UserByName(t.Context(), "cert_user")
	if err != nil {
		t.Fatalf("UserByName: %v", err)
	}
	if !reflect.DeepEqual(users[0], byName) {
		t.Errorf("Users =\n%+v\nUserByName =\n%+v", *users[0], *byName)
	}
	v := reflect.ValueOf(*users[0])
	for i := range v.NumField() {
		if f := v.Type().Field(i); f.IsExported() && v.Field(i).IsZero() {
			t.Errorf("Users leaves User.%s zero on a row that has it", f.Name)
		}
	}
}

// TestUsersJoinsLoginsExceptOnAzureSQLDatabase: a user database on Azure SQL
// Database cannot see sys.server_principals, so the listing must not name it
// there — and must everywhere else.
func TestUsersJoinsLoginsExceptOnAzureSQLDatabase(t *testing.T) {
	for _, tc := range []struct {
		edition EngineEdition
		join    bool
	}{
		{EngineEnterprise, true},
		{EngineAzureManagedInst, true},
		{EngineAzureSQLDatabase, false},
	} {
		row := userRow()
		row[8], row[9] = nil, nil // the CAST(NULL …) columns
		d := qsRecDB(t, 17, userCols, [][]driver.Value{row})
		d.server.info.EngineEdition = int(tc.edition)
		users, err := d.Users(t.Context())
		if err != nil || len(users) != 1 {
			t.Fatalf("edition %d: Users = %v, %v", tc.edition, users, err)
		}
		if got := strings.Contains(qsRec.last(t).sql, "sys.server_principals"); got != tc.join {
			t.Errorf("edition %d: query joins sys.server_principals = %v, want %v", tc.edition, got, tc.join)
		}
		if users[0].LoginName != "" || users[0].MappedObject != "signing_cert" {
			t.Errorf("edition %d: LoginName %q, MappedObject %q", tc.edition, users[0].LoginName, users[0].MappedObject)
		}
	}
}
