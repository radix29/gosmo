package gosmo

import (
	"context"
	"database/sql"
	"fmt"
)

// ============================================================
// Effective permissions (SSMS's "Effective" tab — what a principal can
// actually do, after role membership, inherited scopes and DENY are
// resolved)
// ============================================================

// EffectivePermission is one permission a principal effectively holds on a
// securable, as reported by fn_my_permissions. Unlike a PermissionEntry it
// is not an explicit GRANT/DENY row: role membership, ownership, and
// permissions inherited from a wider scope (a schema grant covering a table,
// CONTROL implying everything below it) are all already resolved into it,
// and anything DENY takes away is simply absent.
type EffectivePermission struct {
	// Entity is the securable the permission applies to, as
	// fn_my_permissions names it — "database", "server", or the
	// schema-qualified object name.
	Entity string

	// Subentity is the column name for a column-level permission, empty
	// otherwise. Asking about an object reports both: the object-level rows
	// with an empty Subentity, then one row per column that carries a
	// column-level permission of its own.
	Subentity string

	// Permission is the permission name, e.g. "SELECT", "VIEW DEFINITION".
	Permission string
}

// EffectivePermissions returns every permission principal effectively holds
// on the database itself — SSMS's Effective tab on a database user's
// Securables page, with the database row selected.
//
// principal must be a database *user* that exists in this database. A
// database role is not accepted and cannot be made to work: every function
// here resolves permissions by impersonating the principal, and SQL Server
// refuses to impersonate a role — "Cannot execute as the database principal
// because the principal %q does not exist, this type of principal cannot be
// impersonated, or you do not have permission" (Msg 15517), verified live
// 2026-08-05 against a role that plainly did exist. There is no principal
// argument to fn_my_permissions to use instead; it always answers for the
// current execution context.
func (d *Database) EffectivePermissions(principal string) ([]*EffectivePermission, error) {
	return d.EffectivePermissionsContext(context.Background(), principal)
}

// EffectivePermissionsContext is the context-aware variant of
// EffectivePermissions.
func (d *Database) EffectivePermissionsContext(ctx context.Context, principal string) ([]*EffectivePermission, error) {
	return d.effectivePermissions(ctx, principal, nil, "DATABASE",
		fmt.Sprintf("effective database permissions for %q in %q", principal, d.name))
}

// EffectiveObjectPermissions returns every permission principal effectively
// holds on the table or view schema.name, column-level entries included (see
// EffectivePermission.Subentity). principal must be a database user — see
// EffectivePermissions for why a role cannot be one.
func (d *Database) EffectiveObjectPermissions(schema, name, principal string) ([]*EffectivePermission, error) {
	return d.EffectiveObjectPermissionsContext(context.Background(), schema, name, principal)
}

// EffectiveObjectPermissionsContext is the context-aware variant of
// EffectiveObjectPermissions.
func (d *Database) EffectiveObjectPermissionsContext(ctx context.Context, schema, name, principal string) ([]*EffectivePermission, error) {
	// fn_my_permissions parses its first argument as a securable *name*, so
	// this is an identifier inside a string value — bracket-quote it first,
	// or a schema or table containing a dot resolves to something else.
	ref := qualifiedName(schema, name)
	return d.effectivePermissions(ctx, principal, ref, "OBJECT",
		fmt.Sprintf("effective permissions on %s for %q in %q", ref, principal, d.name))
}

// EffectiveSchemaPermissions returns every permission principal effectively
// holds on a schema. principal must be a database user — see
// EffectivePermissions for why a role cannot be one.
func (d *Database) EffectiveSchemaPermissions(schemaName, principal string) ([]*EffectivePermission, error) {
	return d.EffectiveSchemaPermissionsContext(context.Background(), schemaName, principal)
}

// EffectiveSchemaPermissionsContext is the context-aware variant of
// EffectiveSchemaPermissions.
func (d *Database) EffectiveSchemaPermissionsContext(ctx context.Context, schemaName, principal string) ([]*EffectivePermission, error) {
	ref := quoteIdent(schemaName)
	return d.effectivePermissions(ctx, principal, ref, "SCHEMA",
		fmt.Sprintf("effective permissions on schema %q for %q in %q", schemaName, principal, d.name))
}

// effectivePermissions impersonates a database principal for the length of
// one batch and asks fn_my_permissions what it can do. securable is nil for
// a scope that names none (the database itself).
//
// The impersonation has to be part of the same batch as the SELECT:
// fn_my_permissions always answers for the *current* execution context, and
// there is no form of it that takes a principal. Database.query pins one
// connection for the rows it returns, so EXECUTE AS, the SELECT, and REVERT
// all land on that connection in order.
//
// The REVERT is belt-and-braces rather than load-bearing: an impersonated
// context ends with the batch, and go-mssqldb resets session state before a
// pooled connection is handed to its next user (see
// Server.GrantServerPermissionContext for that mechanism, verified live).
// It is written anyway so the statement reads correctly if it is ever
// scripted or run by hand.
//
// EXECUTE AS takes a literal, not a parameter, so the principal name is
// escaped into the batch text; the securable name a parameter can carry.
func (d *Database) effectivePermissions(ctx context.Context, principal string, securable any, class, what string) ([]*EffectivePermission, error) {
	q := fmt.Sprintf(`
EXECUTE AS USER = N'%s';
SELECT entity_name, COALESCE(subentity_name, N''), permission_name
FROM   fn_my_permissions(@p1, '%s')
ORDER  BY entity_name, subentity_name, permission_name;
REVERT;`, escapeSingle(principal), class)

	// A nil securable binds @p1 as NULL, which is what fn_my_permissions
	// wants for a scope that names none.
	rows, err := d.query(ctx, q, securable)
	if err != nil {
		return nil, fmt.Errorf("gosmo: %s: %w", what, err)
	}
	defer rows.Close()

	return scanEffectivePermissions(rows.Rows)
}

// EffectiveServerPermissions returns every server-level permission login
// effectively holds — SSMS's Effective tab on a Login Properties >
// Securables page, with the server row selected.
//
// login must be a *login*. A server role is not accepted, for the same
// reason a database role isn't (see EffectivePermissions): SQL Server
// refuses to impersonate one, with Msg 15406, the server-principal wording
// of the same error. The impersonation is EXECUTE AS LOGIN rather than
// EXECUTE AS USER, and it needs IMPERSONATE on that login (CONTROL SERVER
// covers it).
func (s *Server) EffectiveServerPermissions(login string) ([]*EffectivePermission, error) {
	return s.EffectiveServerPermissionsContext(context.Background(), login)
}

// EffectiveServerPermissionsContext is the context-aware variant of
// EffectiveServerPermissions.
func (s *Server) EffectiveServerPermissionsContext(ctx context.Context, login string) ([]*EffectivePermission, error) {
	// The USE master is load-bearing, not tidiness. EXECUTE AS LOGIN keeps
	// the session's current database, so it fails outright — "The server
	// principal %q is not able to access the database %q under the current
	// security context" (Msg 916) — whenever the pooled connection happens
	// to sit in a database that login has no user in. That is the normal
	// case for the restricted logins this call is most useful on. Verified
	// live 2026-08-05: identical statement, master vs. such a database,
	// succeeds and fails respectively. Same prefix and same reasoning as
	// Server.GrantServerPermissionContext.
	q := fmt.Sprintf(`
USE master;
EXECUTE AS LOGIN = N'%s';
SELECT entity_name, COALESCE(subentity_name, N''), permission_name
FROM   fn_my_permissions(NULL, 'SERVER')
ORDER  BY permission_name;
REVERT;`, escapeSingle(login))

	rows, err := s.query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("gosmo: effective server permissions for %q: %w", login, err)
	}
	defer rows.Close()

	return scanEffectivePermissions(rows)
}

// scanEffectivePermissions reads the three-column shape every
// fn_my_permissions query above returns.
func scanEffectivePermissions(rows *sql.Rows) ([]*EffectivePermission, error) {
	var perms []*EffectivePermission
	for rows.Next() {
		p := &EffectivePermission{}
		if err := rows.Scan(&p.Entity, &p.Subentity, &p.Permission); err != nil {
			return nil, err
		}
		perms = append(perms, p)
	}
	return perms, rows.Err()
}
