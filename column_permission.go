package gosmo

import (
	"context"
	"fmt"
	"slices"
)

// ============================================================
// Column-level permissions (GRANT SELECT (col) ON [s].[t] — SSMS's
// "Column Permissions..." button on a Securables page)
// ============================================================

// ColumnPermissionEntry is one GRANT/DENY entry recorded on a single column
// of a table or view — what sys.database_permissions stores as an
// OBJECT_OR_COLUMN row with a non-zero minor_id. Object-level entries
// (minor_id 0) are reported by Permissions instead, and the two are
// genuinely separate grants: a column DENY overrides an object GRANT.
type ColumnPermissionEntry struct {
	Principal     string
	PrincipalType string // e.g. "DATABASE_ROLE", "SQL_USER"
	Grantor       string
	Schema        string
	Object        string

	// ObjectType is what the column's parent is — "TABLE" or "VIEW", the
	// same values PrincipalSecurable.SecurableType uses, so a caller can key
	// both against one securable identity. Both types carry column
	// permissions and sys.database_permissions records them identically, so
	// without this a view's grants get mistaken for a table's.
	ObjectType string

	Column     string
	Permission ObjectPermission
	State      PermissionState // "GRANT", "GRANT_WITH_GRANT_OPTION", "DENY"
}

// columnPermissionNames allowlists the permissions SQL Server accepts on a
// column. It is much shorter than objectPermissionNames because most
// permissions have no column-level form — GRANT DELETE (col) fails with
// "Incorrect syntax near '('". See serverPermissionNames (server_security.go)
// for why an allowlist rather than quoting.
var columnPermissionNames = map[ObjectPermission]bool{
	PermSelect: true, PermUpdate: true, PermReferences: true,
}

// validColumnPermission reports whether permission has a column-level form.
func validColumnPermission(permission ObjectPermission) bool {
	return columnPermissionNames[permission]
}

// ColumnPermissionNames returns every permission name GRANT/DENY/REVOKE
// accepts on a column, sorted — the catalog a column-permissions grid
// enumerates, the way ObjectPermissionNames is the catalog for the whole
// object.
func ColumnPermissionNames() []string {
	names := make([]string, 0, len(columnPermissionNames))
	for name := range columnPermissionNames {
		names = append(names, string(name))
	}
	slices.Sort(names)
	return names
}

// ColumnPermissions returns every column-level GRANT/DENY entry recorded on
// schema.name, for all principals and all columns.
func (d *Database) ColumnPermissions(schema, name string) ([]*ColumnPermissionEntry, error) {
	return d.ColumnPermissionsContext(context.Background(), schema, name)
}

// ColumnPermissionsContext is the context-aware variant of ColumnPermissions.
func (d *Database) ColumnPermissionsContext(ctx context.Context, schema, name string) ([]*ColumnPermissionEntry, error) {
	const q = `
SELECT pr.name, pr.type_desc, grantor.name, sch.name, obj.name, obj.type_desc,
       col.name, dp.permission_name, dp.state_desc
FROM   sys.database_permissions dp
JOIN   sys.objects obj                 ON obj.object_id = dp.major_id
JOIN   sys.schemas sch                 ON sch.schema_id = obj.schema_id
JOIN   sys.columns col                 ON col.object_id = dp.major_id
                                      AND col.column_id = dp.minor_id
JOIN   sys.database_principals pr      ON pr.principal_id      = dp.grantee_principal_id
JOIN   sys.database_principals grantor ON grantor.principal_id = dp.grantor_principal_id
WHERE  dp.class_desc = 'OBJECT_OR_COLUMN' AND dp.minor_id > 0
AND    dp.major_id = OBJECT_ID(@p1)
ORDER  BY pr.name, col.name, dp.permission_name`

	ref := qualifiedName(schema, name)
	return d.scanColumnPermissions(ctx, q, fmt.Sprintf("column permissions on %s in %q", ref, d.name), ref)
}

// ColumnPermissionsForPrincipal returns every column-level GRANT/DENY entry
// principal holds anywhere in the database — the column-level counterpart of
// PermissionsForPrincipal, which reports object-, schema- and
// database-scoped entries and deliberately leaves column entries out (a
// Securables page lists securables, and a column is not one of them; SSMS
// puts them behind a per-securable "Column Permissions..." button).
func (d *Database) ColumnPermissionsForPrincipal(principal string) ([]*ColumnPermissionEntry, error) {
	return d.ColumnPermissionsForPrincipalContext(context.Background(), principal)
}

// ColumnPermissionsForPrincipalContext is the context-aware variant of
// ColumnPermissionsForPrincipal.
func (d *Database) ColumnPermissionsForPrincipalContext(ctx context.Context, principal string) ([]*ColumnPermissionEntry, error) {
	const q = `
SELECT pr.name, pr.type_desc, grantor.name, sch.name, obj.name, obj.type_desc,
       col.name, dp.permission_name, dp.state_desc
FROM   sys.database_permissions dp
JOIN   sys.objects obj                 ON obj.object_id = dp.major_id
JOIN   sys.schemas sch                 ON sch.schema_id = obj.schema_id
JOIN   sys.columns col                 ON col.object_id = dp.major_id
                                      AND col.column_id = dp.minor_id
JOIN   sys.database_principals pr      ON pr.principal_id      = dp.grantee_principal_id
JOIN   sys.database_principals grantor ON grantor.principal_id = dp.grantor_principal_id
WHERE  dp.class_desc = 'OBJECT_OR_COLUMN' AND dp.minor_id > 0
AND    pr.name = @p1
ORDER  BY sch.name, obj.name, col.name, dp.permission_name`

	return d.scanColumnPermissions(ctx, q,
		fmt.Sprintf("column permissions for principal %q in %q", principal, d.name), principal)
}

// scanColumnPermissions runs one of the two column-permission queries above,
// which return identical column lists.
func (d *Database) scanColumnPermissions(ctx context.Context, q, what string, arg any) ([]*ColumnPermissionEntry, error) {
	rows, err := d.query(ctx, q, arg)
	if err != nil {
		return nil, fmt.Errorf("gosmo: %s: %w", what, err)
	}
	defer rows.Close()

	var entries []*ColumnPermissionEntry
	for rows.Next() {
		e := &ColumnPermissionEntry{}
		var perm, state, objType string
		if err := rows.Scan(&e.Principal, &e.PrincipalType, &e.Grantor,
			&e.Schema, &e.Object, &objType, &e.Column, &perm, &state); err != nil {
			return nil, fmt.Errorf("gosmo: %s: %w", what, err)
		}
		e.ObjectType = securableObjectTypeNames[objType]
		e.Permission = ObjectPermission(perm)
		e.State = PermissionState(state)
		entries = append(entries, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("gosmo: %s: %w", what, err)
	}
	return entries, nil
}

// GrantColumnPermission grants permission on the named columns of
// schema.name to principal. Passing several columns renders the one
// statement SQL Server accepts for them — GRANT SELECT (a, b) ON ... — not
// one statement per column.
func (d *Database) GrantColumnPermission(schema, name string, permission ObjectPermission, columns []string, principal string) error {
	return d.GrantColumnPermissionContext(context.Background(), schema, name, permission, columns, principal)
}

// GrantColumnPermissionContext is the context-aware variant of
// GrantColumnPermission.
func (d *Database) GrantColumnPermissionContext(ctx context.Context, schema, name string, permission ObjectPermission, columns []string, principal string) error {
	return d.GrantColumnPermissionWithOptionsContext(ctx, schema, name, permission, columns, principal, PermissionOptions{})
}

// GrantColumnPermissionWithOptions grants a column-level permission
// honouring opts — the WITH GRANT OPTION form of GrantColumnPermission.
func (d *Database) GrantColumnPermissionWithOptions(schema, name string, permission ObjectPermission, columns []string, principal string, opts PermissionOptions) error {
	return d.GrantColumnPermissionWithOptionsContext(context.Background(), schema, name, permission, columns, principal, opts)
}

// GrantColumnPermissionWithOptionsContext is the context-aware variant of
// GrantColumnPermissionWithOptions.
func (d *Database) GrantColumnPermissionWithOptionsContext(ctx context.Context, schema, name string, permission ObjectPermission, columns []string, principal string, opts PermissionOptions) error {
	if err := requireColumns("grant", columns); err != nil {
		return err
	}
	return d.objectPermission(ctx, "GRANT", schema, name, permission, columns, principal, opts)
}

// DenyColumnPermission denies permission on the named columns of
// schema.name to principal.
func (d *Database) DenyColumnPermission(schema, name string, permission ObjectPermission, columns []string, principal string) error {
	return d.DenyColumnPermissionContext(context.Background(), schema, name, permission, columns, principal)
}

// DenyColumnPermissionContext is the context-aware variant of
// DenyColumnPermission.
func (d *Database) DenyColumnPermissionContext(ctx context.Context, schema, name string, permission ObjectPermission, columns []string, principal string) error {
	return d.DenyColumnPermissionWithOptionsContext(ctx, schema, name, permission, columns, principal, PermissionOptions{})
}

// DenyColumnPermissionWithOptions denies a column-level permission honouring
// opts — the CASCADE form of DenyColumnPermission.
func (d *Database) DenyColumnPermissionWithOptions(schema, name string, permission ObjectPermission, columns []string, principal string, opts PermissionOptions) error {
	return d.DenyColumnPermissionWithOptionsContext(context.Background(), schema, name, permission, columns, principal, opts)
}

// DenyColumnPermissionWithOptionsContext is the context-aware variant of
// DenyColumnPermissionWithOptions.
func (d *Database) DenyColumnPermissionWithOptionsContext(ctx context.Context, schema, name string, permission ObjectPermission, columns []string, principal string, opts PermissionOptions) error {
	if err := requireColumns("deny", columns); err != nil {
		return err
	}
	return d.objectPermission(ctx, "DENY", schema, name, permission, columns, principal, opts)
}

// RevokeColumnPermission revokes permission on the named columns of
// schema.name from principal.
func (d *Database) RevokeColumnPermission(schema, name string, permission ObjectPermission, columns []string, principal string) error {
	return d.RevokeColumnPermissionContext(context.Background(), schema, name, permission, columns, principal)
}

// RevokeColumnPermissionContext is the context-aware variant of
// RevokeColumnPermission.
func (d *Database) RevokeColumnPermissionContext(ctx context.Context, schema, name string, permission ObjectPermission, columns []string, principal string) error {
	return d.RevokeColumnPermissionWithOptionsContext(ctx, schema, name, permission, columns, principal, PermissionOptions{})
}

// RevokeColumnPermissionWithOptions revokes a column-level permission
// honouring opts — the CASCADE and GRANT OPTION FOR forms of
// RevokeColumnPermission.
func (d *Database) RevokeColumnPermissionWithOptions(schema, name string, permission ObjectPermission, columns []string, principal string, opts PermissionOptions) error {
	return d.RevokeColumnPermissionWithOptionsContext(context.Background(), schema, name, permission, columns, principal, opts)
}

// RevokeColumnPermissionWithOptionsContext is the context-aware variant of
// RevokeColumnPermissionWithOptions.
func (d *Database) RevokeColumnPermissionWithOptionsContext(ctx context.Context, schema, name string, permission ObjectPermission, columns []string, principal string, opts PermissionOptions) error {
	if err := requireColumns("revoke", columns); err != nil {
		return err
	}
	return d.objectPermission(ctx, "REVOKE", schema, name, permission, columns, principal, opts)
}

// requireColumns rejects an empty column list rather than letting it render
// as an object-level statement — a caller that meant "the whole table" has
// the plain Grant/Deny/RevokePermission trio to say so, and silently
// widening a column grant to the object is the wrong direction to guess in.
func requireColumns(verb string, columns []string) error {
	if len(columns) == 0 {
		return fmt.Errorf("gosmo: %s column permission: no columns named", verb)
	}
	return nil
}
