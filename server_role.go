package gosmo

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
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

// ServerRoles returns all fixed and user-defined server roles.
func (s *Server) ServerRoles() ([]*ServerRole, error) {
	return s.ServerRolesContext(context.Background())
}

// ServerRolesContext is the context-aware variant of ServerRoles.
func (s *Server) ServerRolesContext(ctx context.Context) ([]*ServerRole, error) {
	const q = `
	SELECT r.name, r.principal_id, r.is_fixed_role, ISNULL(p.name, ''),
	       STUFF((SELECT ', ' + m.name
	              FROM sys.server_role_members rm
	              JOIN sys.server_principals m ON m.principal_id = rm.member_principal_id
	              WHERE rm.role_principal_id = r.principal_id
	              FOR XML PATH(''), TYPE).value('.','NVARCHAR(MAX)'), 1, 2, '') AS members
	FROM sys.server_principals r
	LEFT JOIN sys.server_principals p ON p.principal_id = r.owning_principal_id
	WHERE r.type = 'R'
	ORDER BY r.name`

	rows, err := s.query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("gosmo: list server roles: %w", err)
	}
	defer rows.Close()

	var roles []*ServerRole
	for rows.Next() {
		r := &ServerRole{server: s}
		var members sql.NullString
		if err := rows.Scan(&r.Name, &r.ID, &r.IsFixedRole, &r.Owner, &members); err != nil {
			return nil, fmt.Errorf("gosmo: list server roles: %w", err)
		}
		if members.Valid && members.String != "" {
			r.Members = strings.Split(members.String, ", ")
		}
		roles = append(roles, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("gosmo: list server roles: %w", err)
	}
	return roles, nil
}

// ServerRoleByName returns a single server role by name, with its
// principal detail (SID, create/modify dates) filled in —
// ServerRolesContext leaves these out since Object Explorer's tree listing
// never needs them.
func (s *Server) ServerRoleByName(name string) (*ServerRole, error) {
	return s.ServerRoleByNameContext(context.Background(), name)
}

// ServerRoleByNameContext is the context-aware variant of ServerRoleByName.
func (s *Server) ServerRoleByNameContext(ctx context.Context, name string) (*ServerRole, error) {
	const q = `
	SELECT r.principal_id, r.is_fixed_role, ISNULL(p.name, ''),
	       r.sid, r.create_date, r.modify_date,
	       STUFF((SELECT ', ' + m.name
	              FROM sys.server_role_members rm
	              JOIN sys.server_principals m ON m.principal_id = rm.member_principal_id
	              WHERE rm.role_principal_id = r.principal_id
	              FOR XML PATH(''), TYPE).value('.','NVARCHAR(MAX)'), 1, 2, '') AS members
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
	if members.Valid && members.String != "" {
		r.Members = strings.Split(members.String, ", ")
	}
	return r, nil
}

// ServerRoleRef returns a lightweight handle for name without querying the
// server at all — unlike ServerRoleByName/ServerRoleByNameContext, it doesn't
// verify the role exists or populate ID/IsFixedRole/Owner/Members/SID/
// CreateDate/ModifyDate (they stay at their zero value). Every write method
// on *ServerRole (DropContext, RenameContext, ChangeOwnerContext) only ever
// needs the role's name, never those cached fields, so this is sufficient for
// issuing further ALTER-style calls against a role the caller already knows
// exists — most commonly one it just created in the same operation. See
// Server.DatabaseRef's doc comment for why this also matters under a
// WithScript-derived context.
func (s *Server) ServerRoleRef(name string) *ServerRole {
	return &ServerRole{server: s, Name: name}
}

// DropServerRole drops a user-defined server role. A fixed role, or one
// that still owns another role, is refused by the server, not here.
func (s *Server) DropServerRole(name string) error {
	return s.DropServerRoleContext(context.Background(), name)
}

// DropServerRoleContext is the context-aware variant of DropServerRole.
func (s *Server) DropServerRoleContext(ctx context.Context, name string) error {
	if name == "" {
		return fmt.Errorf("gosmo: drop server role: name is required")
	}
	if err := s.execContext(ctx, "DROP SERVER ROLE "+quoteIdent(name)); err != nil {
		return fmt.Errorf("gosmo: drop server role %q: %w", name, err)
	}
	return nil
}

// Drop drops this server role.
func (r *ServerRole) Drop() error { return r.DropContext(context.Background()) }

// DropContext is the context-aware variant of Drop.
func (r *ServerRole) DropContext(ctx context.Context) error {
	return r.server.DropServerRoleContext(ctx, r.Name)
}

// Rename changes the server role's name.
func (r *ServerRole) Rename(newName string) error {
	return r.RenameContext(context.Background(), newName)
}

// RenameContext is the context-aware variant of Rename.
func (r *ServerRole) RenameContext(ctx context.Context, newName string) error {
	q := fmt.Sprintf("ALTER SERVER ROLE %s WITH NAME = %s", quoteIdent(r.Name), quoteIdent(newName))
	if err := r.server.execContext(ctx, q); err != nil {
		return fmt.Errorf("gosmo: rename server role %q to %q: %w", r.Name, newName, err)
	}
	setIfApplied(ctx, &r.Name, newName)
	return nil
}

// ChangeOwner transfers ownership of the server role to a new principal.
func (r *ServerRole) ChangeOwner(newOwner string) error {
	return r.ChangeOwnerContext(context.Background(), newOwner)
}

// ChangeOwnerContext is the context-aware variant of ChangeOwner.
func (r *ServerRole) ChangeOwnerContext(ctx context.Context, newOwner string) error {
	q := fmt.Sprintf("ALTER AUTHORIZATION ON SERVER ROLE::%s TO %s", quoteIdent(r.Name), quoteIdent(newOwner))
	if err := r.server.execContext(ctx, q); err != nil {
		return fmt.Errorf("gosmo: change server role %q owner to %q: %w", r.Name, newOwner, err)
	}
	setIfApplied(ctx, &r.Owner, newOwner)
	return nil
}

// ServerRoleMembers returns the direct members of a server role (logins or
// other server roles), with each member's principal type —
// ServerRolesContext/ServerRoleByNameContext only return member names,
// concatenated, with no type.
func (s *Server) ServerRoleMembers(roleName string) ([]*RoleMember, error) {
	return s.ServerRoleMembersContext(context.Background(), roleName)
}

// ServerRoleMembersContext is the context-aware variant of ServerRoleMembers.
func (s *Server) ServerRoleMembersContext(ctx context.Context, roleName string) ([]*RoleMember, error) {
	const q = `
SELECT m.name, m.type_desc
FROM   sys.server_role_members rm
JOIN   sys.server_principals r ON r.principal_id = rm.role_principal_id
JOIN   sys.server_principals m ON m.principal_id = rm.member_principal_id
WHERE  r.name = @p1
ORDER  BY m.name`

	rows, err := s.query(ctx, q, roleName)
	if err != nil {
		return nil, fmt.Errorf("gosmo: members of server role %q: %w", roleName, err)
	}
	defer rows.Close()

	var members []*RoleMember
	for rows.Next() {
		m := &RoleMember{}
		if err := rows.Scan(&m.Name, &m.Type); err != nil {
			return nil, fmt.Errorf("gosmo: members of server role %q: %w", roleName, err)
		}
		members = append(members, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("gosmo: members of server role %q: %w", roleName, err)
	}
	return members, nil
}

// AddServerRoleMember adds member (a login or another server role, by
// name) to a server role.
func (s *Server) AddServerRoleMember(roleName, memberName string) error {
	return s.AddServerRoleMemberContext(context.Background(), roleName, memberName)
}

// AddServerRoleMemberContext is the context-aware variant of AddServerRoleMember.
func (s *Server) AddServerRoleMemberContext(ctx context.Context, roleName, memberName string) error {
	q := fmt.Sprintf("ALTER SERVER ROLE %s ADD MEMBER %s", quoteIdent(roleName), quoteIdent(memberName))
	if err := s.execContext(ctx, q); err != nil {
		return fmt.Errorf("gosmo: add %q to server role %q: %w", memberName, roleName, err)
	}
	return nil
}

// RemoveServerRoleMember removes member from a server role.
func (s *Server) RemoveServerRoleMember(roleName, memberName string) error {
	return s.RemoveServerRoleMemberContext(context.Background(), roleName, memberName)
}

// RemoveServerRoleMemberContext is the context-aware variant of RemoveServerRoleMember.
func (s *Server) RemoveServerRoleMemberContext(ctx context.Context, roleName, memberName string) error {
	q := fmt.Sprintf("ALTER SERVER ROLE %s DROP MEMBER %s", quoteIdent(roleName), quoteIdent(memberName))
	if err := s.execContext(ctx, q); err != nil {
		return fmt.Errorf("gosmo: remove %q from server role %q: %w", memberName, roleName, err)
	}
	return nil
}
