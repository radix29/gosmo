package gosmo

import (
	"context"
	"fmt"
	"strings"
)

// ============================================================
// GRANT/DENY/REVOKE modifiers — WITH GRANT OPTION, CASCADE, and
// GRANT OPTION FOR
// ============================================================

// PermissionOptions carries the GRANT/DENY/REVOKE modifiers the plain
// Grant/Deny/Revoke trios do not expose. The zero value renders exactly the
// statement those trios render, at every scope — true by construction since
// 2026-08-05: each plain method is a one-line delegation to its
// WithOptions counterpart passing PermissionOptions{}, so there is one
// renderer (permissionStmt) and one set of error strings rather than two
// that have to be kept in step.
//
// The three fields are not independent of each other in practice, because
// SQL Server refuses some sequences outright:
//
//   - A permission granted WITH GRANT OPTION cannot be revoked or denied
//     without CASCADE — "the permission was granted WITH GRANT OPTION" is a
//     hard error, not a warning. Anything that takes such a grant away
//     therefore needs Cascade set.
//   - GrantOptionOnly (REVOKE GRANT OPTION FOR) takes away only the right to
//     re-grant and leaves the underlying GRANT in place. It is the
//     "WITH GRANT OPTION -> plain GRANT" downgrade, and SQL Server requires
//     CASCADE with it as well, so Cascade is implied and need not be set.
//
// CASCADE reaches every principal the grantee granted the permission on to,
// which is the point of it and also why it is opt-in rather than always sent.
type PermissionOptions struct {
	// WithGrantOption appends WITH GRANT OPTION to a GRANT, letting the
	// grantee grant the same permission on to others. GRANT only.
	WithGrantOption bool

	// Cascade appends CASCADE to a DENY or REVOKE, applying it to every
	// principal the grantee passed the permission on to. Required whenever
	// the permission being taken away was granted WITH GRANT OPTION.
	Cascade bool

	// GrantOptionOnly turns a REVOKE into REVOKE GRANT OPTION FOR: the
	// grantee keeps the permission but loses the right to grant it onward.
	// REVOKE only, and always CASCADE.
	GrantOptionOnly bool
}

// permissionStmt is one rendered GRANT/DENY/REVOKE, shared by every scope so
// the modifier placement is decided in one place. on is the full "ON ..."
// clause ("ON [dbo].[t]", "ON SCHEMA::[s]") or empty for database- and
// server-scoped permissions, which name no securable.
type permissionStmt struct {
	verb       string // "GRANT", "DENY", "REVOKE"
	permission string
	columns    []string // column-level permission; object scope only
	on         string
	principal  string
	opts       PermissionOptions
}

// render builds the statement, rejecting a modifier the verb has no form
// for rather than quietly dropping it — a caller that asks for WITH GRANT
// OPTION on a DENY has a bug, and a silently plain DENY hides it.
func (p permissionStmt) render() (string, error) {
	o := p.opts
	if o.WithGrantOption && p.verb != "GRANT" {
		return "", fmt.Errorf("gosmo: %s: WITH GRANT OPTION applies to GRANT only", strings.ToLower(p.verb))
	}
	if o.GrantOptionOnly && p.verb != "REVOKE" {
		return "", fmt.Errorf("gosmo: %s: GRANT OPTION FOR applies to REVOKE only", strings.ToLower(p.verb))
	}
	if o.Cascade && p.verb == "GRANT" {
		return "", fmt.Errorf("gosmo: grant: CASCADE applies to DENY and REVOKE only")
	}

	var b strings.Builder
	b.WriteString(p.verb)
	b.WriteByte(' ')
	if o.GrantOptionOnly {
		b.WriteString("GRANT OPTION FOR ")
	}
	b.WriteString(p.permission)
	if len(p.columns) > 0 {
		b.WriteString(" (")
		for i, c := range p.columns {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(quoteIdent(c))
		}
		b.WriteByte(')')
	}
	if p.on != "" {
		b.WriteByte(' ')
		b.WriteString(p.on)
	}
	if p.verb == "REVOKE" {
		b.WriteString(" FROM ")
	} else {
		b.WriteString(" TO ")
	}
	b.WriteString(quoteIdent(p.principal))
	switch {
	case o.WithGrantOption:
		b.WriteString(" WITH GRANT OPTION")
	case o.Cascade || o.GrantOptionOnly:
		b.WriteString(" CASCADE")
	}
	return b.String(), nil
}

// -- Object scope (tables and views) ---------------------------------------

// GrantPermissionWithOptions grants permission on schema.name to principal,
// honouring opts — the WITH GRANT OPTION form of GrantPermission.
func (d *Database) GrantPermissionWithOptions(schema, name string, permission ObjectPermission, principal string, opts PermissionOptions) error {
	return d.GrantPermissionWithOptionsContext(context.Background(), schema, name, permission, principal, opts)
}

// GrantPermissionWithOptionsContext is the context-aware variant of
// GrantPermissionWithOptions.
func (d *Database) GrantPermissionWithOptionsContext(ctx context.Context, schema, name string, permission ObjectPermission, principal string, opts PermissionOptions) error {
	return d.objectPermission(ctx, "GRANT", schema, name, permission, nil, principal, opts)
}

// DenyPermissionWithOptions denies permission on schema.name to principal,
// honouring opts — the CASCADE form of DenyPermission.
func (d *Database) DenyPermissionWithOptions(schema, name string, permission ObjectPermission, principal string, opts PermissionOptions) error {
	return d.DenyPermissionWithOptionsContext(context.Background(), schema, name, permission, principal, opts)
}

// DenyPermissionWithOptionsContext is the context-aware variant of
// DenyPermissionWithOptions.
func (d *Database) DenyPermissionWithOptionsContext(ctx context.Context, schema, name string, permission ObjectPermission, principal string, opts PermissionOptions) error {
	return d.objectPermission(ctx, "DENY", schema, name, permission, nil, principal, opts)
}

// RevokePermissionWithOptions revokes permission on schema.name from
// principal, honouring opts — the CASCADE and GRANT OPTION FOR forms of
// RevokePermission.
func (d *Database) RevokePermissionWithOptions(schema, name string, permission ObjectPermission, principal string, opts PermissionOptions) error {
	return d.RevokePermissionWithOptionsContext(context.Background(), schema, name, permission, principal, opts)
}

// RevokePermissionWithOptionsContext is the context-aware variant of
// RevokePermissionWithOptions.
func (d *Database) RevokePermissionWithOptionsContext(ctx context.Context, schema, name string, permission ObjectPermission, principal string, opts PermissionOptions) error {
	return d.objectPermission(ctx, "REVOKE", schema, name, permission, nil, principal, opts)
}

// objectPermission is the shared body of every object-scoped
// GRANT/DENY/REVOKE, column-level included (columns nil means the whole
// object).
func (d *Database) objectPermission(ctx context.Context, verb, schema, name string, permission ObjectPermission, columns []string, principal string, opts PermissionOptions) error {
	lower := strings.ToLower(verb)
	if len(columns) > 0 {
		if !validColumnPermission(permission) {
			return fmt.Errorf("gosmo: %s column permission: %q cannot be granted on a column", lower, permission)
		}
	} else if !validObjectPermission(permission) {
		return fmt.Errorf("gosmo: %s permission: unrecognized permission %q", lower, permission)
	}
	ref := qualifiedName(schema, name)
	q, err := permissionStmt{
		verb: verb, permission: string(permission), columns: columns,
		on: "ON " + ref, principal: principal, opts: opts,
	}.render()
	if err != nil {
		return err
	}
	if _, err := d.exec(ctx, q); err != nil {
		return fmt.Errorf("gosmo: %s %s on %s %s %q: %w", lower, permission, ref, fromOrTo(verb), principal, err)
	}
	return nil
}

// fromOrTo picks the preposition a verb's error message reads with.
func fromOrTo(verb string) string {
	if verb == "REVOKE" {
		return "from"
	}
	return "to"
}

// -- Schema scope ----------------------------------------------------------

// GrantSchemaPermissionWithOptions grants permission on a schema to
// principal, honouring opts.
func (d *Database) GrantSchemaPermissionWithOptions(schemaName string, permission ObjectPermission, principal string, opts PermissionOptions) error {
	return d.GrantSchemaPermissionWithOptionsContext(context.Background(), schemaName, permission, principal, opts)
}

// GrantSchemaPermissionWithOptionsContext is the context-aware variant of
// GrantSchemaPermissionWithOptions.
func (d *Database) GrantSchemaPermissionWithOptionsContext(ctx context.Context, schemaName string, permission ObjectPermission, principal string, opts PermissionOptions) error {
	return d.schemaPermission(ctx, "GRANT", schemaName, permission, principal, opts)
}

// DenySchemaPermissionWithOptions denies permission on a schema to
// principal, honouring opts.
func (d *Database) DenySchemaPermissionWithOptions(schemaName string, permission ObjectPermission, principal string, opts PermissionOptions) error {
	return d.DenySchemaPermissionWithOptionsContext(context.Background(), schemaName, permission, principal, opts)
}

// DenySchemaPermissionWithOptionsContext is the context-aware variant of
// DenySchemaPermissionWithOptions.
func (d *Database) DenySchemaPermissionWithOptionsContext(ctx context.Context, schemaName string, permission ObjectPermission, principal string, opts PermissionOptions) error {
	return d.schemaPermission(ctx, "DENY", schemaName, permission, principal, opts)
}

// RevokeSchemaPermissionWithOptions revokes permission on a schema from
// principal, honouring opts.
func (d *Database) RevokeSchemaPermissionWithOptions(schemaName string, permission ObjectPermission, principal string, opts PermissionOptions) error {
	return d.RevokeSchemaPermissionWithOptionsContext(context.Background(), schemaName, permission, principal, opts)
}

// RevokeSchemaPermissionWithOptionsContext is the context-aware variant of
// RevokeSchemaPermissionWithOptions.
func (d *Database) RevokeSchemaPermissionWithOptionsContext(ctx context.Context, schemaName string, permission ObjectPermission, principal string, opts PermissionOptions) error {
	return d.schemaPermission(ctx, "REVOKE", schemaName, permission, principal, opts)
}

func (d *Database) schemaPermission(ctx context.Context, verb, schemaName string, permission ObjectPermission, principal string, opts PermissionOptions) error {
	lower := strings.ToLower(verb)
	if !validSchemaPermission(permission) {
		return fmt.Errorf("gosmo: %s schema permission: unrecognized permission %q", lower, permission)
	}
	q, err := permissionStmt{
		verb: verb, permission: string(permission),
		on: "ON SCHEMA::" + quoteIdent(schemaName), principal: principal, opts: opts,
	}.render()
	if err != nil {
		return err
	}
	if _, err := d.exec(ctx, q); err != nil {
		return fmt.Errorf("gosmo: %s %s on schema %q %s %q: %w", lower, permission, schemaName, fromOrTo(verb), principal, err)
	}
	return nil
}

// -- Database scope --------------------------------------------------------

// GrantDatabasePermissionWithOptions grants a database-level permission to
// principal, honouring opts.
func (d *Database) GrantDatabasePermissionWithOptions(permission, principal string, opts PermissionOptions) error {
	return d.GrantDatabasePermissionWithOptionsContext(context.Background(), permission, principal, opts)
}

// GrantDatabasePermissionWithOptionsContext is the context-aware variant of
// GrantDatabasePermissionWithOptions.
func (d *Database) GrantDatabasePermissionWithOptionsContext(ctx context.Context, permission, principal string, opts PermissionOptions) error {
	return d.databasePermission(ctx, "GRANT", permission, principal, opts)
}

// DenyDatabasePermissionWithOptions denies a database-level permission to
// principal, honouring opts.
func (d *Database) DenyDatabasePermissionWithOptions(permission, principal string, opts PermissionOptions) error {
	return d.DenyDatabasePermissionWithOptionsContext(context.Background(), permission, principal, opts)
}

// DenyDatabasePermissionWithOptionsContext is the context-aware variant of
// DenyDatabasePermissionWithOptions.
func (d *Database) DenyDatabasePermissionWithOptionsContext(ctx context.Context, permission, principal string, opts PermissionOptions) error {
	return d.databasePermission(ctx, "DENY", permission, principal, opts)
}

// RevokeDatabasePermissionWithOptions revokes a database-level permission
// from principal, honouring opts.
func (d *Database) RevokeDatabasePermissionWithOptions(permission, principal string, opts PermissionOptions) error {
	return d.RevokeDatabasePermissionWithOptionsContext(context.Background(), permission, principal, opts)
}

// RevokeDatabasePermissionWithOptionsContext is the context-aware variant of
// RevokeDatabasePermissionWithOptions.
func (d *Database) RevokeDatabasePermissionWithOptionsContext(ctx context.Context, permission, principal string, opts PermissionOptions) error {
	return d.databasePermission(ctx, "REVOKE", permission, principal, opts)
}

func (d *Database) databasePermission(ctx context.Context, verb, permission, principal string, opts PermissionOptions) error {
	lower := strings.ToLower(verb)
	if !validDatabasePermission(permission) {
		return fmt.Errorf("gosmo: %s database permission: unrecognized permission %q", lower, permission)
	}
	q, err := permissionStmt{verb: verb, permission: permission, principal: principal, opts: opts}.render()
	if err != nil {
		return err
	}
	if _, err := d.exec(ctx, q); err != nil {
		return fmt.Errorf("gosmo: %s %s %s %q in %q: %w", lower, permission, fromOrTo(verb), principal, d.name, err)
	}
	return nil
}

// -- Server scope ----------------------------------------------------------

// GrantServerPermissionWithOptions grants a server-level permission to
// principal, honouring opts.
func (s *Server) GrantServerPermissionWithOptions(permission, principal string, opts PermissionOptions) error {
	return s.GrantServerPermissionWithOptionsContext(context.Background(), permission, principal, opts)
}

// GrantServerPermissionWithOptionsContext is the context-aware variant of
// GrantServerPermissionWithOptions. See GrantServerPermissionContext for the
// USE master prefix every server-scoped statement carries.
func (s *Server) GrantServerPermissionWithOptionsContext(ctx context.Context, permission, principal string, opts PermissionOptions) error {
	return s.serverPermission(ctx, "GRANT", permission, principal, opts)
}

// DenyServerPermissionWithOptions denies a server-level permission to
// principal, honouring opts.
func (s *Server) DenyServerPermissionWithOptions(permission, principal string, opts PermissionOptions) error {
	return s.DenyServerPermissionWithOptionsContext(context.Background(), permission, principal, opts)
}

// DenyServerPermissionWithOptionsContext is the context-aware variant of
// DenyServerPermissionWithOptions. See GrantServerPermissionContext for the
// USE master prefix.
func (s *Server) DenyServerPermissionWithOptionsContext(ctx context.Context, permission, principal string, opts PermissionOptions) error {
	return s.serverPermission(ctx, "DENY", permission, principal, opts)
}

// RevokeServerPermissionWithOptions revokes a server-level permission from
// principal, honouring opts.
func (s *Server) RevokeServerPermissionWithOptions(permission, principal string, opts PermissionOptions) error {
	return s.RevokeServerPermissionWithOptionsContext(context.Background(), permission, principal, opts)
}

// RevokeServerPermissionWithOptionsContext is the context-aware variant of
// RevokeServerPermissionWithOptions. See GrantServerPermissionContext for the
// USE master prefix.
func (s *Server) RevokeServerPermissionWithOptionsContext(ctx context.Context, permission, principal string, opts PermissionOptions) error {
	return s.serverPermission(ctx, "REVOKE", permission, principal, opts)
}

func (s *Server) serverPermission(ctx context.Context, verb, permission, principal string, opts PermissionOptions) error {
	lower := strings.ToLower(verb)
	if !validServerPermission(permission) {
		return fmt.Errorf("gosmo: %s server permission: unrecognized permission %q", lower, permission)
	}
	stmt, err := permissionStmt{verb: verb, permission: permission, principal: principal, opts: opts}.render()
	if err != nil {
		return err
	}
	if err := s.execContext(ctx, "USE master; "+stmt); err != nil {
		return fmt.Errorf("gosmo: %s %s %s %q: %w", lower, permission, fromOrTo(verb), principal, err)
	}
	return nil
}
