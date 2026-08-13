package gosmo

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"time"
)

// ============================================================
// Server security  (authentication mode, server-level GRANT/DENY,
// credentials)
// ============================================================

// ServerSecurityInfo holds server-wide authentication settings — SSMS's
// Server Properties > Security page. Login-audit level and the server
// proxy account live in the registry (xp_instance_regread), which gosmo
// deliberately does not touch (see README "Features intentionally
// excluded"); only what SERVERPROPERTY exposes is included here.
type ServerSecurityInfo struct {
	// AuthenticationMode is "WINDOWS" (Windows Authentication only) or
	// "MIXED" (SQL Server and Windows Authentication).
	AuthenticationMode string
}

// SecurityInfo returns server-wide authentication settings.
func (s *Server) SecurityInfo() (*ServerSecurityInfo, error) {
	return s.SecurityInfoContext(context.Background())
}

// SecurityInfoContext is the context-aware variant of SecurityInfo.
func (s *Server) SecurityInfoContext(ctx context.Context) (*ServerSecurityInfo, error) {
	const q = `SELECT CASE CAST(SERVERPROPERTY('IsIntegratedSecurityOnly') AS INT)
	                   WHEN 1 THEN 'WINDOWS' ELSE 'MIXED' END`

	info := &ServerSecurityInfo{}
	if err := s.queryRowScan(ctx, q, nil, &info.AuthenticationMode); err != nil {
		return nil, fmt.Errorf("gosmo: server security info: %w", err)
	}
	return info, nil
}

// -- Server-level permissions ---------------------------------------------------

// ServerPermissionEntry is one GRANT/DENY entry recorded at server scope,
// as reported by sys.server_permissions — SSMS's Server Properties >
// Permissions page and a Login's Securables page.
type ServerPermissionEntry struct {
	Principal     string
	PrincipalType string // e.g. "SQL_LOGIN", "SERVER_ROLE"
	Grantor       string
	Permission    string // e.g. "CONNECT SQL", "ALTER ANY LOGIN", "CONTROL SERVER"
	State         string // "GRANT", "GRANT_WITH_GRANT_OPTION", "DENY"
}

// ServerPermissions returns every server-level GRANT/DENY entry.
func (s *Server) ServerPermissions() ([]*ServerPermissionEntry, error) {
	return s.ServerPermissionsContext(context.Background())
}

// ServerPermissionsContext is the context-aware variant of ServerPermissions.
func (s *Server) ServerPermissionsContext(ctx context.Context) ([]*ServerPermissionEntry, error) {
	const q = `
SELECT pr.name, pr.type_desc, grantor.name, sp.permission_name, sp.state_desc
FROM   sys.server_permissions sp
JOIN   sys.server_principals pr      ON pr.principal_id      = sp.grantee_principal_id
JOIN   sys.server_principals grantor ON grantor.principal_id = sp.grantor_principal_id
WHERE  sp.class_desc = 'SERVER'
ORDER  BY pr.name, sp.permission_name`

	rows, err := s.query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("gosmo: server permissions: %w", err)
	}
	defer rows.Close()

	var perms []*ServerPermissionEntry
	for rows.Next() {
		e := &ServerPermissionEntry{}
		if err := rows.Scan(&e.Principal, &e.PrincipalType, &e.Grantor, &e.Permission, &e.State); err != nil {
			return nil, fmt.Errorf("gosmo: server permissions: %w", err)
		}
		perms = append(perms, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("gosmo: server permissions: %w", err)
	}
	return perms, nil
}

// serverPermissionNames allowlists every server-scoped permission name
// SQL Server accepts in a GRANT/DENY/REVOKE ... statement. Permission
// names can't be identifier-quoted (QuoteName would wrap them in brackets
// SQL Server doesn't expect here) or passed as query parameters (GRANT is
// DDL), so Grant/Deny/RevokeServerPermission reject anything not in this
// list rather than splicing caller input directly into the statement.
var serverPermissionNames = map[string]bool{
	"ADMINISTER BULK OPERATIONS":      true,
	"ALTER ANY AVAILABILITY GROUP":    true,
	"ALTER ANY CONNECTION":            true,
	"ALTER ANY CREDENTIAL":            true,
	"ALTER ANY DATABASE":              true,
	"ALTER ANY ENDPOINT":              true,
	"ALTER ANY EVENT NOTIFICATION":    true,
	"ALTER ANY EVENT SESSION":         true,
	"ALTER ANY LINKED SERVER":         true,
	"ALTER ANY LOGIN":                 true,
	"ALTER ANY SERVER AUDIT":          true,
	"ALTER ANY SERVER ROLE":           true,
	"ALTER RESOURCES":                 true,
	"ALTER SERVER STATE":              true,
	"ALTER SETTINGS":                  true,
	"ALTER TRACE":                     true,
	"AUTHENTICATE SERVER":             true,
	"CONNECT ANY DATABASE":            true,
	"CONNECT SQL":                     true,
	"CONTROL SERVER":                  true,
	"CREATE ANY DATABASE":             true,
	"CREATE AVAILABILITY GROUP":       true,
	"CREATE DDL EVENT NOTIFICATION":   true,
	"CREATE ENDPOINT":                 true,
	"CREATE SERVER ROLE":              true,
	"CREATE TRACE EVENT NOTIFICATION": true,
	"EXTERNAL ACCESS ASSEMBLY":        true,
	"IMPERSONATE ANY LOGIN":           true,
	"SELECT ALL USER SECURABLES":      true,
	"SHUTDOWN":                        true,
	"UNSAFE ASSEMBLY":                 true,
	"VIEW ANY DATABASE":               true,
	"VIEW ANY DEFINITION":             true,
	"VIEW SERVER STATE":               true,
	"VIEW SERVER SECURITY AUDIT":      true,
}

// validServerPermission reports whether name is a recognized server-scoped
// permission name.
func validServerPermission(name string) bool { return serverPermissionNames[name] }

// ServerPermissionNames returns every server-scoped permission name
// GRANT/DENY/REVOKE accepts, sorted — the catalog SSMS's Server Properties
// > Permissions page enumerates for a principal regardless of whether it
// already has an explicit GRANT/DENY entry.
func ServerPermissionNames() []string {
	names := make([]string, 0, len(serverPermissionNames))
	for name := range serverPermissionNames {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// GrantServerPermission grants a server-level permission to principal.
func (s *Server) GrantServerPermission(permission, principal string) error {
	return s.GrantServerPermissionContext(context.Background(), permission, principal)
}

// GrantServerPermissionContext is the context-aware variant of GrantServerPermission.
//
// SQL Server rejects GRANT/DENY/REVOKE at server scope outright unless the
// session's current database is master ("Permissions at the server scope
// can only be granted when the current database is master") — its own
// restriction, not one gosmo imposes — so every statement here is prefixed
// with USE master in the same batch.
//
// That USE does not leak into whatever borrows the connection next, and the
// reason is the driver, not this package: USE is session state and would
// otherwise survive the connection's return to the pool. database/sql calls
// driver.SessionResetter.ResetSession before handing a pooled connection to
// its next user, and go-mssqldb implements it by flagging the next TDS batch
// as a connection reset (Conn.ResetSession -> sendSqlBatch72's resetSession),
// which restores the session's database to the connection string's.
//
// Verified live 2026-08-01, A/B against a connection opened with
// Database set: eight pooled connections all still reported that database
// after a GRANT. Recorded because the shape of this code invites the
// opposite conclusion — a review that session proposed replacing it with a
// pinned connection that reads DB_NAME(), switches, and switches back, which
// is three extra round trips per grant to re-solve what the driver already
// handles.
func (s *Server) GrantServerPermissionContext(ctx context.Context, permission, principal string) error {
	return s.GrantServerPermissionWithOptionsContext(ctx, permission, principal, PermissionOptions{})
}

// DenyServerPermission denies a server-level permission to principal.
func (s *Server) DenyServerPermission(permission, principal string) error {
	return s.DenyServerPermissionContext(context.Background(), permission, principal)
}

// DenyServerPermissionContext is the context-aware variant of DenyServerPermission.
// See GrantServerPermissionContext's doc comment for the USE master prefix.
func (s *Server) DenyServerPermissionContext(ctx context.Context, permission, principal string) error {
	return s.DenyServerPermissionWithOptionsContext(ctx, permission, principal, PermissionOptions{})
}

// RevokeServerPermission revokes a server-level permission from principal.
func (s *Server) RevokeServerPermission(permission, principal string) error {
	return s.RevokeServerPermissionContext(context.Background(), permission, principal)
}

// RevokeServerPermissionContext is the context-aware variant of RevokeServerPermission.
// See GrantServerPermissionContext's doc comment for the USE master prefix.
func (s *Server) RevokeServerPermissionContext(ctx context.Context, permission, principal string) error {
	return s.RevokeServerPermissionWithOptionsContext(ctx, permission, principal, PermissionOptions{})
}

// -- Credentials -----------------------------------------------------------------

// Credential mirrors a row from sys.credentials — used to populate a
// Login's "Map to credential" dropdown.
type Credential struct {
	Name       string
	Identity   string
	CreateDate time.Time
	ModifyDate time.Time
}

// Credentials returns every server-level credential.
func (s *Server) Credentials() ([]*Credential, error) {
	return s.CredentialsContext(context.Background())
}

// CredentialsContext is the context-aware variant of Credentials.
func (s *Server) CredentialsContext(ctx context.Context) ([]*Credential, error) {
	const q = `
SELECT name, credential_identity, create_date, modify_date
FROM   sys.credentials
ORDER  BY name`

	rows, err := s.query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("gosmo: list credentials: %w", err)
	}
	defer rows.Close()

	var creds []*Credential
	for rows.Next() {
		c := &Credential{}
		var identity sql.NullString
		if err := rows.Scan(&c.Name, &identity, &c.CreateDate, &c.ModifyDate); err != nil {
			return nil, fmt.Errorf("gosmo: list credentials: %w", err)
		}
		c.Identity = identity.String
		creds = append(creds, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("gosmo: list credentials: %w", err)
	}
	return creds, nil
}
