package gosmo

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// -- Database roles ------------------------------------------------------------

// DatabaseRole represents a database-level role.
type DatabaseRole struct {
	db          *Database
	Name        string
	ID          int
	IsFixedRole bool
	Owner       string
	Members     []string
	SID         []byte
	CreateDate  time.Time
	ModifyDate  time.Time
}

// Database returns the database the role belongs to.
func (r *DatabaseRole) Database() *Database { return r.db }

// DatabaseRoles returns all roles defined in the database.
func (d *Database) DatabaseRoles(ctx context.Context) ([]*DatabaseRole, error) {
	q := `
SELECT r.name, r.principal_id, r.is_fixed_role, p.name AS owner,
       ` + jsonList("m.name", `
        FROM   sys.database_role_members rm
        JOIN   sys.database_principals m ON m.principal_id = rm.member_principal_id
        WHERE  rm.role_principal_id = r.principal_id`, "m.name") + ` AS members
FROM   sys.database_principals r
JOIN   sys.database_principals p ON p.principal_id = r.owning_principal_id
WHERE  r.type = 'R'
ORDER  BY r.name`

	rows, err := d.query(ctx, q)
	return scanRows(rows, err, fmt.Sprintf("list database roles in %q", d.Name), func(scan func(...any) error) (*DatabaseRole, error) {
		r := &DatabaseRole{db: d}
		var members sql.NullString
		if err := scan(&r.Name, &r.ID, &r.IsFixedRole, &r.Owner, &members); err != nil {
			return nil, err
		}
		var err error
		if r.Members, err = decodeJSONList(members); err != nil {
			return nil, err
		}
		return r, nil
	})
}

// RoleByName returns a single database role by name, with its principal
// detail (SID, create/modify dates) filled in — DatabaseRoles
// leaves these out since Object Explorer's tree listing never needs them.
func (d *Database) RoleByName(ctx context.Context, name string) (*DatabaseRole, error) {
	q := `
SELECT r.name, r.principal_id, r.is_fixed_role, p.name AS owner,
       r.sid, r.create_date, r.modify_date,
       ` + jsonList("m.name", `
        FROM   sys.database_role_members rm
        JOIN   sys.database_principals m ON m.principal_id = rm.member_principal_id
        WHERE  rm.role_principal_id = r.principal_id`, "m.name") + ` AS members
FROM   sys.database_principals r
JOIN   sys.database_principals p ON p.principal_id = r.owning_principal_id
WHERE  r.type = 'R' AND r.name = @p1`

	r := &DatabaseRole{db: d}
	var members sql.NullString
	err := d.queryRow(ctx, func(row *sql.Row) error {
		return row.Scan(&r.Name, &r.ID, &r.IsFixedRole, &r.Owner, &r.SID, &r.CreateDate, &r.ModifyDate, &members)
	}, q, name)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, notFoundf("gosmo: database role %q not found in %q", name, d.Name)
		}
		return nil, fmt.Errorf("gosmo: find database role %q in %q: %w", name, d.Name, err)
	}
	if r.Members, err = decodeJSONList(members); err != nil {
		return nil, err
	}
	return r, nil
}

// RoleRef returns a lightweight handle for a database role by name, without
// querying the catalog — the counterpart of Server.DatabaseRef. Every field
// but the name stays at its zero value; RoleByName is what populates them.
//
// Every write on *DatabaseRole addresses it by name, so this handle is enough to
// drop one the caller already knows exists — and is the form to use when
// there is nothing to read yet, such as a script of a CREATE that was only
// collected.
func (d *Database) RoleRef(name string) *DatabaseRole {
	return &DatabaseRole{db: d, Name: name}
}

// Rename changes the database role's name.
func (r *DatabaseRole) Rename(ctx context.Context, newName string) error {
	q := fmt.Sprintf("ALTER ROLE %s WITH NAME = %s", quoteIdent(r.Name), quoteIdent(newName))
	if _, err := r.db.exec(ctx, q); err != nil {
		return fmt.Errorf("gosmo: rename database role %q to %q: %w", r.Name, newName, err)
	}
	setIfApplied(ctx, &r.Name, newName)
	return nil
}

// SetOwner transfers ownership of the database role to a new principal.
func (r *DatabaseRole) SetOwner(ctx context.Context, newOwner string) error {
	q := fmt.Sprintf("ALTER AUTHORIZATION ON ROLE::%s TO %s", quoteIdent(r.Name), quoteIdent(newOwner))
	if _, err := r.db.exec(ctx, q); err != nil {
		return fmt.Errorf("gosmo: change database role %q owner to %q: %w", r.Name, newOwner, err)
	}
	setIfApplied(ctx, &r.Owner, newOwner)
	return nil
}

// RoleMember is one direct member of a database role.
type RoleMember struct {
	Name string
	Type string // e.g. "SQL_USER", "WINDOWS_USER", "DATABASE_ROLE"
}

// RoleMembers returns the direct members of a database role, with each
// member's principal type — DatabaseRoles/RoleByName only
// return member names, concatenated, with no type.
func (d *Database) RoleMembers(ctx context.Context, roleName string) ([]*RoleMember, error) {
	const q = `
SELECT m.name, m.type_desc
FROM   sys.database_role_members rm
JOIN   sys.database_principals r ON r.principal_id = rm.role_principal_id
JOIN   sys.database_principals m ON m.principal_id = rm.member_principal_id
WHERE  r.name = @p1
ORDER  BY m.name`

	rows, err := d.query(ctx, q, roleName)
	return scanRows(rows, err, fmt.Sprintf("members of role %q in %q", roleName, d.Name), func(scan func(...any) error) (*RoleMember, error) {
		m := &RoleMember{}
		if err := scan(&m.Name, &m.Type); err != nil {
			return nil, err
		}
		return m, nil
	})
}

// AddRoleMember adds a user to a database role.
func (d *Database) AddRoleMember(ctx context.Context, roleName, memberName string) error {
	if _, err := d.exec(ctx,
		fmt.Sprintf("ALTER ROLE %s ADD MEMBER %s", quoteIdent(roleName), quoteIdent(memberName)),
	); err != nil {
		return fmt.Errorf("gosmo: add %q to role %q: %w", memberName, roleName, err)
	}
	return nil
}

// RemoveRoleMember removes a user from a database role.
func (d *Database) RemoveRoleMember(ctx context.Context, roleName, memberName string) error {
	if _, err := d.exec(ctx,
		fmt.Sprintf("ALTER ROLE %s DROP MEMBER %s", quoteIdent(roleName), quoteIdent(memberName)),
	); err != nil {
		return fmt.Errorf("gosmo: remove %q from role %q: %w", memberName, roleName, err)
	}
	return nil
}

// Drop drops this database role. A role that still owns a schema or has
// members is refused by the server, not here.
func (r *DatabaseRole) Drop(ctx context.Context) error {
	if _, err := r.db.exec(ctx, "DROP ROLE "+quoteIdent(r.Name)); err != nil {
		return fmt.Errorf("gosmo: drop database role %q: %w", r.Name, err)
	}
	return nil
}
