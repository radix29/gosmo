package gosmo

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
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

// DatabaseRoles returns all roles defined in the database.
func (d *Database) DatabaseRoles() ([]*DatabaseRole, error) {
	return d.DatabaseRolesContext(context.Background())
}

// DatabaseRolesContext is the context-aware variant of DatabaseRoles.
func (d *Database) DatabaseRolesContext(ctx context.Context) ([]*DatabaseRole, error) {
	const q = `
SELECT r.name, r.principal_id, r.is_fixed_role, p.name AS owner,
       STUFF((SELECT ', ' + m.name
              FROM   sys.database_role_members rm
              JOIN   sys.database_principals m ON m.principal_id = rm.member_principal_id
              WHERE  rm.role_principal_id = r.principal_id
              FOR XML PATH(''), TYPE).value('.','NVARCHAR(MAX)'), 1, 2, '') AS members
FROM   sys.database_principals r
JOIN   sys.database_principals p ON p.principal_id = r.owning_principal_id
WHERE  r.type = 'R'
ORDER  BY r.name`

	rows, err := d.query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("gosmo: list database roles in %q: %w", d.name, err)
	}
	defer rows.Close()

	var roles []*DatabaseRole
	for rows.Next() {
		r := &DatabaseRole{db: d}
		var members sql.NullString
		if err := rows.Scan(&r.Name, &r.ID, &r.IsFixedRole, &r.Owner, &members); err != nil {
			return nil, err
		}
		if members.Valid && members.String != "" {
			r.Members = strings.Split(members.String, ", ")
		}
		roles = append(roles, r)
	}
	return roles, rows.Err()
}

// RoleByName returns a single database role by name, with its principal
// detail (SID, create/modify dates) filled in — DatabaseRolesContext
// leaves these out since Object Explorer's tree listing never needs them.
func (d *Database) RoleByName(name string) (*DatabaseRole, error) {
	return d.RoleByNameContext(context.Background(), name)
}

// RoleByNameContext is the context-aware variant of RoleByName.
func (d *Database) RoleByNameContext(ctx context.Context, name string) (*DatabaseRole, error) {
	const q = `
SELECT r.principal_id, r.is_fixed_role, p.name AS owner,
       r.sid, r.create_date, r.modify_date,
       STUFF((SELECT ', ' + m.name
              FROM   sys.database_role_members rm
              JOIN   sys.database_principals m ON m.principal_id = rm.member_principal_id
              WHERE  rm.role_principal_id = r.principal_id
              FOR XML PATH(''), TYPE).value('.','NVARCHAR(MAX)'), 1, 2, '') AS members
FROM   sys.database_principals r
JOIN   sys.database_principals p ON p.principal_id = r.owning_principal_id
WHERE  r.type = 'R' AND r.name = @p1`

	r := &DatabaseRole{db: d, Name: name}
	var members sql.NullString
	err := d.queryRow(ctx, func(row *sql.Row) error {
		return row.Scan(&r.ID, &r.IsFixedRole, &r.Owner, &r.SID, &r.CreateDate, &r.ModifyDate, &members)
	}, q, name)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, notFoundf("gosmo: database role %q not found in %q", name, d.name)
		}
		return nil, fmt.Errorf("gosmo: find database role %q in %q: %w", name, d.name, err)
	}
	if members.Valid && members.String != "" {
		r.Members = strings.Split(members.String, ", ")
	}
	return r, nil
}

// Rename changes the database role's name.
func (r *DatabaseRole) Rename(newName string) error {
	return r.RenameContext(context.Background(), newName)
}

// RenameContext is the context-aware variant of Rename.
func (r *DatabaseRole) RenameContext(ctx context.Context, newName string) error {
	q := fmt.Sprintf("ALTER ROLE %s WITH NAME = %s", quoteIdent(r.Name), quoteIdent(newName))
	if _, err := r.db.exec(ctx, q); err != nil {
		return fmt.Errorf("gosmo: rename database role %q to %q: %w", r.Name, newName, err)
	}
	setIfApplied(ctx, &r.Name, newName)
	return nil
}

// ChangeOwner transfers ownership of the database role to a new principal.
func (r *DatabaseRole) ChangeOwner(newOwner string) error {
	return r.ChangeOwnerContext(context.Background(), newOwner)
}

// ChangeOwnerContext is the context-aware variant of ChangeOwner.
func (r *DatabaseRole) ChangeOwnerContext(ctx context.Context, newOwner string) error {
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
// member's principal type — DatabaseRolesContext/RoleByNameContext only
// return member names, concatenated, with no type.
func (d *Database) RoleMembers(roleName string) ([]*RoleMember, error) {
	return d.RoleMembersContext(context.Background(), roleName)
}

// RoleMembersContext is the context-aware variant of RoleMembers.
func (d *Database) RoleMembersContext(ctx context.Context, roleName string) ([]*RoleMember, error) {
	const q = `
SELECT m.name, m.type_desc
FROM   sys.database_role_members rm
JOIN   sys.database_principals r ON r.principal_id = rm.role_principal_id
JOIN   sys.database_principals m ON m.principal_id = rm.member_principal_id
WHERE  r.name = @p1
ORDER  BY m.name`

	rows, err := d.query(ctx, q, roleName)
	if err != nil {
		return nil, fmt.Errorf("gosmo: members of role %q in %q: %w", roleName, d.name, err)
	}
	defer rows.Close()

	var members []*RoleMember
	for rows.Next() {
		m := &RoleMember{}
		if err := rows.Scan(&m.Name, &m.Type); err != nil {
			return nil, err
		}
		members = append(members, m)
	}
	return members, rows.Err()
}

// AddRoleMember adds a user to a database role.
func (d *Database) AddRoleMember(roleName, memberName string) error {
	return d.AddRoleMemberContext(context.Background(), roleName, memberName)
}

// AddRoleMemberContext is the context-aware variant.
func (d *Database) AddRoleMemberContext(ctx context.Context, roleName, memberName string) error {
	if _, err := d.exec(ctx,
		fmt.Sprintf("ALTER ROLE %s ADD MEMBER %s", quoteIdent(roleName), quoteIdent(memberName)),
	); err != nil {
		return fmt.Errorf("gosmo: add %q to role %q: %w", memberName, roleName, err)
	}
	return nil
}

// RemoveRoleMember removes a user from a database role.
func (d *Database) RemoveRoleMember(roleName, memberName string) error {
	return d.RemoveRoleMemberContext(context.Background(), roleName, memberName)
}

// RemoveRoleMemberContext is the context-aware variant.
func (d *Database) RemoveRoleMemberContext(ctx context.Context, roleName, memberName string) error {
	if _, err := d.exec(ctx,
		fmt.Sprintf("ALTER ROLE %s DROP MEMBER %s", quoteIdent(roleName), quoteIdent(memberName)),
	); err != nil {
		return fmt.Errorf("gosmo: remove %q from role %q: %w", memberName, roleName, err)
	}
	return nil
}

// DropDatabaseRole drops a database role. A role that still owns a schema
// or has members is refused by the server, not here.
func (d *Database) DropDatabaseRole(name string) error {
	return d.DropDatabaseRoleContext(context.Background(), name)
}

// DropDatabaseRoleContext is the context-aware variant of DropDatabaseRole.
func (d *Database) DropDatabaseRoleContext(ctx context.Context, name string) error {
	if _, err := d.exec(ctx, "DROP ROLE "+quoteIdent(name)); err != nil {
		return fmt.Errorf("gosmo: drop database role %q: %w", name, err)
	}
	return nil
}

// Drop drops this database role.
func (r *DatabaseRole) Drop() error { return r.DropContext(context.Background()) }

// DropContext is the context-aware variant of Drop.
func (r *DatabaseRole) DropContext(ctx context.Context) error {
	return r.db.DropDatabaseRoleContext(ctx, r.Name)
}
