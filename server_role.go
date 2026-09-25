package gosmo

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// -- Server roles --------------------------------------------------------------

// ServerRole represents a server-level role.
type ServerRole struct {
	server      *Server
	Name        string
	ID          int
	IsFixedRole bool
	Owner       string
	Members     []string
	SID         []byte
	CreateDate  time.Time
	ModifyDate  time.Time
}

// Server returns the server the role belongs to.
func (r *ServerRole) Server() *Server { return r.server }

// ServerRoles returns all fixed and user-defined server roles.
func (s *Server) ServerRoles(ctx context.Context) ([]*ServerRole, error) {
	q := `
	SELECT r.name, r.principal_id, r.is_fixed_role, ISNULL(p.name, ''),
	       ` + jsonList("m.name", `
        FROM   sys.server_role_members rm
        JOIN   sys.server_principals m ON m.principal_id = rm.member_principal_id
        WHERE  rm.role_principal_id = r.principal_id`, "m.name") + ` AS members
	FROM sys.server_principals r
	LEFT JOIN sys.server_principals p ON p.principal_id = r.owning_principal_id
	WHERE r.type = 'R'
	ORDER BY r.name`

	rows, err := s.query(ctx, q)
	return scanRows(rows, err, "list server roles", func(scan func(...any) error) (*ServerRole, error) {
		r := &ServerRole{server: s}
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

// ServerRoleByName returns a single server role by name, with its
// principal detail (SID, create/modify dates) filled in —
// ServerRoles leaves these out since Object Explorer's tree listing
// never needs them.
func (s *Server) ServerRoleByName(ctx context.Context, name string) (*ServerRole, error) {
	q := `
	SELECT r.principal_id, r.is_fixed_role, ISNULL(p.name, ''),
	       r.sid, r.create_date, r.modify_date,
	       ` + jsonList("m.name", `
        FROM   sys.server_role_members rm
        JOIN   sys.server_principals m ON m.principal_id = rm.member_principal_id
        WHERE  rm.role_principal_id = r.principal_id`, "m.name") + ` AS members
	FROM sys.server_principals r
	LEFT JOIN sys.server_principals p ON p.principal_id = r.owning_principal_id
	WHERE r.type = 'R' AND r.name = @p1`

	r := &ServerRole{server: s, Name: name}
	var members sql.NullString
	if err := s.queryRowScan(ctx, q, []any{name},
		&r.ID, &r.IsFixedRole, &r.Owner, &r.SID, &r.CreateDate, &r.ModifyDate, &members,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, notFoundf("gosmo: server role %q not found", name)
		}
		return nil, fmt.Errorf("gosmo: find server role %q: %w", name, err)
	}
	var err error
	if r.Members, err = decodeJSONList(members); err != nil {
		return nil, err
	}
	return r, nil
}

// ServerRoleRef returns a lightweight handle for name without querying the
// server at all — unlike ServerRoleByName, it doesn't
// verify the role exists or populate ID/IsFixedRole/Owner/Members/SID/
// CreateDate/ModifyDate (they stay at their zero value). Every write method
// on *ServerRole (Drop, Rename, SetOwner) only ever
// needs the role's name, never those cached fields, so this is sufficient for
// issuing further ALTER-style calls against a role the caller already knows
// exists — most commonly one it just created in the same operation. See
// Server.DatabaseRef's doc comment for why this also matters under a
// WithScript-derived context.
func (s *Server) ServerRoleRef(name string) *ServerRole {
	return &ServerRole{server: s, Name: name}
}

// Drop drops this server role. A fixed role, or one that still owns another
// role, is refused by the server, not here.
func (r *ServerRole) Drop(ctx context.Context) error {
	if r.Name == "" {
		return fmt.Errorf("gosmo: drop server role: name is required")
	}
	if err := r.server.exec(ctx, "DROP SERVER ROLE "+quoteIdent(r.Name)); err != nil {
		return fmt.Errorf("gosmo: drop server role %q: %w", r.Name, err)
	}
	return nil
}

// Rename changes the server role's name.
func (r *ServerRole) Rename(ctx context.Context, newName string) error {
	q := fmt.Sprintf("ALTER SERVER ROLE %s WITH NAME = %s", quoteIdent(r.Name), quoteIdent(newName))
	if err := r.server.exec(ctx, q); err != nil {
		return fmt.Errorf("gosmo: rename server role %q to %q: %w", r.Name, newName, err)
	}
	setIfApplied(ctx, &r.Name, newName)
	return nil
}

// SetOwner transfers ownership of the server role to a new principal.
func (r *ServerRole) SetOwner(ctx context.Context, newOwner string) error {
	q := fmt.Sprintf("ALTER AUTHORIZATION ON SERVER ROLE::%s TO %s", quoteIdent(r.Name), quoteIdent(newOwner))
	if err := r.server.exec(ctx, q); err != nil {
		return fmt.Errorf("gosmo: change server role %q owner to %q: %w", r.Name, newOwner, err)
	}
	setIfApplied(ctx, &r.Owner, newOwner)
	return nil
}

// ServerRoleMembers returns the direct members of a server role (logins or
// other server roles), with each member's principal type —
// ServerRoles/ServerRoleByName only return member names,
// concatenated, with no type.
func (s *Server) ServerRoleMembers(ctx context.Context, roleName string) ([]*RoleMember, error) {
	const q = `
SELECT m.name, m.type_desc
FROM   sys.server_role_members rm
JOIN   sys.server_principals r ON r.principal_id = rm.role_principal_id
JOIN   sys.server_principals m ON m.principal_id = rm.member_principal_id
WHERE  r.name = @p1
ORDER  BY m.name`

	rows, err := s.query(ctx, q, roleName)
	return scanRows(rows, err, fmt.Sprintf("members of server role %q", roleName), func(scan func(...any) error) (*RoleMember, error) {
		m := &RoleMember{}
		if err := scan(&m.Name, &m.Type); err != nil {
			return nil, err
		}
		return m, nil
	})
}

// AddServerRoleMember adds member (a login or another server role, by
// name) to a server role.
func (s *Server) AddServerRoleMember(ctx context.Context, roleName, memberName string) error {
	q := fmt.Sprintf("ALTER SERVER ROLE %s ADD MEMBER %s", quoteIdent(roleName), quoteIdent(memberName))
	if err := s.exec(ctx, q); err != nil {
		return fmt.Errorf("gosmo: add %q to server role %q: %w", memberName, roleName, err)
	}
	return nil
}

// RemoveServerRoleMember removes member from a server role.
func (s *Server) RemoveServerRoleMember(ctx context.Context, roleName, memberName string) error {
	q := fmt.Sprintf("ALTER SERVER ROLE %s DROP MEMBER %s", quoteIdent(roleName), quoteIdent(memberName))
	if err := s.exec(ctx, q); err != nil {
		return fmt.Errorf("gosmo: remove %q from server role %q: %w", memberName, roleName, err)
	}
	return nil
}
