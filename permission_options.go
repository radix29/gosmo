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

// PermissionOptions carries the GRANT/DENY/REVOKE modifiers every
// Grant/Deny/Revoke method at every scope takes as its last argument. The
// zero value renders the plain statement. There is one renderer
// (permissionStmt) and one set of error strings for all five scopes.
//
// Until 2026-09-23 each method came as a pair, a plain Foo delegating to a
// FooWithOptions passing PermissionOptions{} — eighteen twins whose zero
// options equalled the plain form, the same shape the Foo/FooContext pairs
// had before them.
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

// GrantPermission grants permission on schema.name to principal. opts adds
// WITH GRANT OPTION; the zero value is the plain GRANT.
func (d *Database) GrantPermission(ctx context.Context, schema, name string, permission ObjectPermission, principal string, opts PermissionOptions) error {
	if err := requireSchema("grant permission", schema, name); err != nil {
		return err
	}
	return d.objectPermission(ctx, "GRANT", schema, name, permission, nil, principal, opts)
}

// DenyPermission denies permission on schema.name to principal. opts adds
// CASCADE; the zero value is the plain DENY.
func (d *Database) DenyPermission(ctx context.Context, schema, name string, permission ObjectPermission, principal string, opts PermissionOptions) error {
	if err := requireSchema("deny permission", schema, name); err != nil {
		return err
	}
	return d.objectPermission(ctx, "DENY", schema, name, permission, nil, principal, opts)
}

// RevokePermission revokes permission on schema.name from principal. opts
// adds CASCADE or GRANT OPTION FOR; the zero value is the plain REVOKE.
func (d *Database) RevokePermission(ctx context.Context, schema, name string, permission ObjectPermission, principal string, opts PermissionOptions) error {
	if err := requireSchema("revoke permission", schema, name); err != nil {
		return err
	}
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

// GrantSchemaPermission grants permission on a schema to principal,
// honouring opts.
func (d *Database) GrantSchemaPermission(ctx context.Context, schemaName string, permission ObjectPermission, principal string, opts PermissionOptions) error {
	return d.schemaPermission(ctx, "GRANT", schemaName, permission, principal, opts)
}

// DenySchemaPermission denies permission on a schema to principal,
// honouring opts.
func (d *Database) DenySchemaPermission(ctx context.Context, schemaName string, permission ObjectPermission, principal string, opts PermissionOptions) error {
	return d.schemaPermission(ctx, "DENY", schemaName, permission, principal, opts)
}

// RevokeSchemaPermission revokes permission on a schema from principal,
// honouring opts.
func (d *Database) RevokeSchemaPermission(ctx context.Context, schemaName string, permission ObjectPermission, principal string, opts PermissionOptions) error {
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

// GrantDatabasePermission grants a database-level permission to principal,
// honouring opts.
func (d *Database) GrantDatabasePermission(ctx context.Context, permission DatabasePermission, principal string, opts PermissionOptions) error {
	return d.databasePermission(ctx, "GRANT", permission, principal, opts)
}

// DenyDatabasePermission denies a database-level permission to principal,
// honouring opts.
func (d *Database) DenyDatabasePermission(ctx context.Context, permission DatabasePermission, principal string, opts PermissionOptions) error {
	return d.databasePermission(ctx, "DENY", permission, principal, opts)
}

// RevokeDatabasePermission revokes a database-level permission from
// principal, honouring opts.
func (d *Database) RevokeDatabasePermission(ctx context.Context, permission DatabasePermission, principal string, opts PermissionOptions) error {
	return d.databasePermission(ctx, "REVOKE", permission, principal, opts)
}

func (d *Database) databasePermission(ctx context.Context, verb string, permission DatabasePermission, principal string, opts PermissionOptions) error {
	lower := strings.ToLower(verb)
	if !validDatabasePermission(permission) {
		return fmt.Errorf("gosmo: %s database permission: unrecognized permission %q", lower, permission)
	}
	q, err := permissionStmt{verb: verb, permission: string(permission), principal: principal, opts: opts}.render()
	if err != nil {
		return err
	}
	if _, err := d.exec(ctx, q); err != nil {
		return fmt.Errorf("gosmo: %s %s %s %q in %q: %w", lower, permission, fromOrTo(verb), principal, d.Name, err)
	}
	return nil
}

// -- Server scope ----------------------------------------------------------

// GrantServerPermission grants a server-level permission to principal,
// honouring opts.
//
// SQL Server rejects GRANT/DENY/REVOKE at server scope outright unless the
// session's current database is master ("Permissions at the server scope can
// only be granted when the current database is master") — its own
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
func (s *Server) GrantServerPermission(ctx context.Context, permission ServerPermission, principal string, opts PermissionOptions) error {
	return s.serverPermission(ctx, "GRANT", permission, principal, opts)
}

// DenyServerPermission denies a server-level permission to principal,
// honouring opts.
//
// See GrantServerPermission for the USE master prefix every
// server-scoped statement carries.
func (s *Server) DenyServerPermission(ctx context.Context, permission ServerPermission, principal string, opts PermissionOptions) error {
	return s.serverPermission(ctx, "DENY", permission, principal, opts)
}

// RevokeServerPermission revokes a server-level permission from principal,
// honouring opts.
//
// See GrantServerPermission for the USE master prefix.
func (s *Server) RevokeServerPermission(ctx context.Context, permission ServerPermission, principal string, opts PermissionOptions) error {
	return s.serverPermission(ctx, "REVOKE", permission, principal, opts)
}

func (s *Server) serverPermission(ctx context.Context, verb string, permission ServerPermission, principal string, opts PermissionOptions) error {
	lower := strings.ToLower(verb)
	if !validServerPermission(permission) {
		return fmt.Errorf("gosmo: %s server permission: unrecognized permission %q", lower, permission)
	}
	stmt, err := permissionStmt{verb: verb, permission: string(permission), principal: principal, opts: opts}.render()
	if err != nil {
		return err
	}
	if err := s.exec(ctx, "USE master; "+stmt); err != nil {
		return fmt.Errorf("gosmo: %s %s %s %q: %w", lower, permission, fromOrTo(verb), principal, err)
	}
	return nil
}
