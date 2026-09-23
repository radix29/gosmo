package gosmo

import (
	"context"
	"fmt"
	"slices"
)

// ============================================================
// Server security  (authentication mode, server-level GRANT/DENY)
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
func (s *Server) SecurityInfo(ctx context.Context) (*ServerSecurityInfo, error) {
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
	Permission    ServerPermission // e.g. "CONNECT SQL", "ALTER ANY LOGIN", "CONTROL SERVER"
	State         string           // "GRANT", "GRANT_WITH_GRANT_OPTION", "DENY"
}

// ServerPermissions returns every server-level GRANT/DENY entry.
func (s *Server) ServerPermissions(ctx context.Context) ([]*ServerPermissionEntry, error) {
	const q = `
SELECT pr.name, pr.type_desc, grantor.name, sp.permission_name, sp.state_desc
FROM   sys.server_permissions sp
JOIN   sys.server_principals pr      ON pr.principal_id      = sp.grantee_principal_id
JOIN   sys.server_principals grantor ON grantor.principal_id = sp.grantor_principal_id
WHERE  sp.class_desc = 'SERVER'
ORDER  BY pr.name, sp.permission_name`

	rows, err := s.query(ctx, q)
	return scanRows(rows, err, "server permissions", func(scan func(...any) error) (*ServerPermissionEntry, error) {
		e := &ServerPermissionEntry{}
		if err := scan(&e.Principal, &e.PrincipalType, &e.Grantor, &e.Permission, &e.State); err != nil {
			return nil, err
		}
		return e, nil
	})
}

// serverPermissionNames allowlists every server-scoped permission name
// SQL Server accepts in a GRANT/DENY/REVOKE ... statement. Permission
// names can't be identifier-quoted (QuoteName would wrap them in brackets
// SQL Server doesn't expect here) or passed as query parameters (GRANT is
// DDL), so Grant/Deny/RevokeServerPermission reject anything not in this
// list rather than splicing caller input directly into the statement.
var serverPermissionNames = map[ServerPermission]bool{
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
func validServerPermission(name ServerPermission) bool { return serverPermissionNames[name] }

// ServerPermissionNames returns every server-scoped permission name
// GRANT/DENY/REVOKE accepts, sorted — the catalog SSMS's Server Properties
// > Permissions page enumerates for a principal regardless of whether it
// already has an explicit GRANT/DENY entry.
func ServerPermissionNames() []string {
	names := make([]string, 0, len(serverPermissionNames))
	for name := range serverPermissionNames {
		names = append(names, string(name))
	}
	slices.Sort(names)
	return names
}
