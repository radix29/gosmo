package gosmo

import (
	"database/sql/driver"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

var principalCols = append(append([]string(nil), userCols...), "fixed", "owner", "members")

// principalRows is one Principals row per family, every column populated
// that the family reads.
func principalRows() [][]driver.Value {
	created := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	user := append(userRow(), false, nil, nil)
	role := []driver.Value{"app_readers", int64(7), "DATABASE_ROLE", nil,
		created, created.Add(time.Hour), "NONE", []byte{9, 8},
		nil, nil, nil, true, "dbo", `[{"v":"alice"},{"v":"bob"}]`}
	app := []driver.Value{"reporting_app", int64(9), "APPLICATION_ROLE", "rpt",
		created, created.Add(2 * time.Hour), "NONE", []byte{7},
		nil, nil, nil, false, "dbo", nil}
	return [][]driver.Value{user, role, app}
}

// sameFields fails on an exported field of got that is zero or differs from
// want's — the CLAUDE.md rule that every read of a type fills what it
// declares, here between Principals and the single-family read.
func sameFields(t *testing.T, what string, got, want any) {
	t.Helper()
	v, w := reflect.ValueOf(got).Elem(), reflect.ValueOf(want).Elem()
	for i := range v.NumField() {
		f := v.Type().Field(i)
		if !f.IsExported() {
			continue
		}
		if v.Field(i).IsZero() && f.Name != "IsFixedRole" {
			t.Errorf("Principals leaves %s.%s zero on a row that has it", what, f.Name)
		}
		if g, w := v.Field(i).Interface(), w.Field(i).Interface(); !reflect.DeepEqual(g, w) {
			t.Errorf("%s.%s: Principals = %+v, single read = %+v", what, f.Name, g, w)
		}
	}
}

// TestPrincipalsFillsWhatEachFamilyReadFills: one Principals row per family
// lands in the right slice, with every field the family's own read fills.
func TestPrincipalsFillsWhatEachFamilyReadFills(t *testing.T) {
	d := qsRecDB(t, 17, principalCols, principalRows())
	p, err := d.Principals(t.Context())
	if err != nil {
		t.Fatalf("Principals: %v", err)
	}
	if len(p.Users) != 1 || len(p.Roles) != 1 || len(p.ApplicationRoles) != 1 {
		t.Fatalf("Principals = %d users, %d roles, %d app roles; want 1 each",
			len(p.Users), len(p.Roles), len(p.ApplicationRoles))
	}
	if q := qsRec.last(t).sql; !strings.Contains(q, "dp.type IN ('S','U','G','E','X','C','K','R','A')") {
		t.Errorf("Principals with no kinds does not read all three families:\n%s", q)
	}
	if !p.Roles[0].IsFixedRole {
		t.Error("Role.IsFixedRole not read")
	}

	qsRec.reset(userCols, [][]driver.Value{userRow()})
	user, err := d.UserByName(t.Context(), "cert_user")
	if err != nil {
		t.Fatalf("UserByName: %v", err)
	}
	sameFields(t, "User", p.Users[0], user)

	created := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	qsRec.reset(strings.Fields("name id fixed owner sid created modified members"), [][]driver.Value{{
		"app_readers", int64(7), true, "dbo", []byte{9, 8}, created, created.Add(time.Hour),
		`[{"v":"alice"},{"v":"bob"}]`}})
	role, err := d.RoleByName(t.Context(), "app_readers")
	if err != nil {
		t.Fatalf("RoleByName: %v", err)
	}
	sameFields(t, "DatabaseRole", p.Roles[0], role)

	qsRec.reset(strings.Fields("name schema id created modified"), [][]driver.Value{{
		"reporting_app", "rpt", int64(9), created, created.Add(2 * time.Hour)}})
	app, err := d.ApplicationRoleByName(t.Context(), "reporting_app")
	if err != nil {
		t.Fatalf("ApplicationRoleByName: %v", err)
	}
	sameFields(t, "ApplicationRole", p.ApplicationRoles[0], app)
}

// TestPrincipalsReadsOnlyTheKindsAsked: kinds narrow the WHERE, so a dialog
// wanting roles does not pay for every user's login join.
func TestPrincipalsReadsOnlyTheKindsAsked(t *testing.T) {
	for _, tc := range []struct {
		kinds []PrincipalKind
		in    string
	}{
		{[]PrincipalKind{PrincipalKindRole}, "('R')"},
		{[]PrincipalKind{PrincipalKindApplicationRole, PrincipalKindRole}, "('R','A')"},
		{[]PrincipalKind{PrincipalKindUser}, "('S','U','G','E','X','C','K')"},
		{[]PrincipalKind{PrincipalKindUser, PrincipalKindUser}, "('S','U','G','E','X','C','K')"},
	} {
		d := qsRecDB(t, 17, principalCols, nil)
		if _, err := d.Principals(t.Context(), tc.kinds...); err != nil {
			t.Fatalf("%v: %v", tc.kinds, err)
		}
		if q := qsRec.last(t).sql; !strings.Contains(q, "dp.type IN "+tc.in+"\n") {
			t.Errorf("%v: want dp.type IN %s in\n%s", tc.kinds, tc.in, q)
		}
	}
}

// TestPrincipalsRefusesAnUnknownKind: the zero value and anything past the
// last constant are the caller's mistake, refused before any query.
func TestPrincipalsRefusesAnUnknownKind(t *testing.T) {
	for _, k := range []PrincipalKind{0, PrincipalKindApplicationRole + 1} {
		d := qsRecDB(t, 17, principalCols, nil)
		_, err := d.Principals(t.Context(), k)
		if !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("kind %d: err = %v, want ErrInvalidRequest", k, err)
		}
		if n := len(qsRec.recorded()); n != 0 {
			t.Errorf("kind %d: %d statements reached the driver", k, n)
		}
	}
}

// TestPrincipalsJoinsLoginsExceptOnAzureSQLDatabase mirrors Users' rule.
func TestPrincipalsJoinsLoginsExceptOnAzureSQLDatabase(t *testing.T) {
	for _, tc := range []struct {
		edition EngineEdition
		join    bool
	}{
		{EngineEnterprise, true},
		{EngineAzureSQLDatabase, false},
	} {
		d := qsRecDB(t, 17, principalCols, nil)
		d.server.info.EngineEdition = int(tc.edition)
		if _, err := d.Principals(t.Context()); err != nil {
			t.Fatalf("edition %d: %v", tc.edition, err)
		}
		if got := strings.Contains(qsRec.last(t).sql, "sys.server_principals"); got != tc.join {
			t.Errorf("edition %d: query joins sys.server_principals = %v, want %v", tc.edition, got, tc.join)
		}
	}
}
