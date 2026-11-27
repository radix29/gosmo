package gosmo

import "context"

// BuiltinPermission is one class/permission pair from sys.fn_builtin_permissions.
// The catalog describes the engine's permission vocabulary, not the caller's
// grants; some entries may be unavailable on the current service or securable.
type BuiltinPermission struct {
	Class string
	Name  string
}

// BuiltinPermissions returns the engine's permission vocabulary, including
// names introduced after this library's write allowlists. SQL Server and Azure
// SQL support this catalog; Fabric Warehouse does not.
func (s *Server) BuiltinPermissions(ctx context.Context) ([]BuiltinPermission, error) {
	const q = `SELECT class_desc, permission_name
FROM sys.fn_builtin_permissions(DEFAULT)
ORDER BY class_desc, permission_name`
	rows, err := s.query(ctx, q)
	return scanRows(rows, err, "builtin permissions", func(scan func(...any) error) (BuiltinPermission, error) {
		var p BuiltinPermission
		err := scan(&p.Class, &p.Name)
		return p, err
	})
}
