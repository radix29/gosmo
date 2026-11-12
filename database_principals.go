package gosmo

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// -- Database principals (users, roles, application roles in one read) --------

// PrincipalKind selects a family of database principals for
// Database.Principals.
type PrincipalKind int

const (
	// PrincipalKindUser is every database user, as Users lists them.
	PrincipalKindUser PrincipalKind = iota + 1
	// PrincipalKindRole is every database role, fixed roles and public
	// included, as Roles lists them.
	PrincipalKindRole
	// PrincipalKindApplicationRole is every application role, as
	// ApplicationRoles lists them.
	PrincipalKindApplicationRole
)

// DatabasePrincipals is what Principals returns: each family ordered by name,
// nil when it has none or was not asked for.
type DatabasePrincipals struct {
	Users            []*User
	Roles            []*DatabaseRole
	ApplicationRoles []*ApplicationRole
}

// Principals reads the database's users, database roles and application
// roles in one round trip, instead of Users, Roles and ApplicationRoles'
// three. kinds limits the read to those families; none means all three.
//
// Each element carries what its single-family read fills: a User what Users
// fills (LoginName and LoginDisabled zero on Azure SQL Database), an
// ApplicationRole what ApplicationRoles fills, and a DatabaseRole what
// RoleByName fills — SID and dates included, which Roles leaves out.
func (d *Database) Principals(ctx context.Context, kinds ...PrincipalKind) (*DatabasePrincipals, error) {
	want := map[PrincipalKind]bool{}
	for _, k := range kinds {
		if k < PrincipalKindUser || k > PrincipalKindApplicationRole {
			return nil, invalidf("gosmo: list principals in %q: unknown principal kind %d", d.Name, k)
		}
		want[k] = true
	}
	all := len(want) == 0
	var types []string
	if all || want[PrincipalKindUser] {
		types = append(types, strings.Trim(userTypes, "()"))
	}
	if all || want[PrincipalKindRole] {
		types = append(types, "'R'")
	}
	if all || want[PrincipalKindApplicationRole] {
		types = append(types, "'A'")
	}

	// The role columns: NULL owner and no members for every other kind.
	roleCols := `,
       dp.is_fixed_role,
       (SELECT o.name FROM sys.database_principals o WHERE o.principal_id = dp.owning_principal_id),
       ` + jsonList("m.name", `
        FROM   sys.database_role_members rm
        JOIN   sys.database_principals m ON m.principal_id = rm.member_principal_id
        WHERE  rm.role_principal_id = dp.principal_id`, "m.name")
	q := userSelect(!d.serverIsAzureSQLDatabase(), roleCols) + `
WHERE  dp.type IN (` + strings.Join(types, ",") + `)
ORDER  BY dp.name`

	out := &DatabasePrincipals{}
	what := fmt.Sprintf("list principals in %q", d.Name)
	rows, err := d.query(ctx, q)
	_, err = scanRows(rows, err, what, func(scan func(...any) error) (struct{}, error) {
		u := &User{db: d}
		var fixed sql.NullBool
		var owner, members sql.NullString
		if err := scanUser(func(dest ...any) error {
			return scan(append(dest, &fixed, &owner, &members)...)
		}, u); err != nil {
			return struct{}{}, err
		}
		switch u.UserType {
		case PrincipalDatabaseRole:
			r := &DatabaseRole{db: d, Name: u.Name, ID: u.ID, IsFixedRole: fixed.Bool,
				Owner: owner.String, SID: u.SID, CreateDate: u.CreateDate, ModifyDate: u.ModifyDate}
			var err error
			if r.Members, err = decodeJSONList(members); err != nil {
				return struct{}{}, err
			}
			out.Roles = append(out.Roles, r)
		case PrincipalApplicationRole:
			out.ApplicationRoles = append(out.ApplicationRoles, &ApplicationRole{db: d, Name: u.Name,
				DefaultSchema: u.DefaultSchema, PrincipalID: u.ID, CreateDate: u.CreateDate, ModifyDate: u.ModifyDate})
		default:
			out.Users = append(out.Users, u)
		}
		return struct{}{}, nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
