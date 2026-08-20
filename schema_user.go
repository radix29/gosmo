package gosmo

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// ============================================================
// Schema
// ============================================================

// Schema mirrors Microsoft.SqlServer.Management.Smo.Schema.
type Schema struct {
	db    *Database
	ID    int
	Name  string
	Owner string
}

// Drop drops the schema.
func (s *Schema) Drop() error { return s.DropContext(context.Background()) }

// DropContext is the context-aware variant of Drop.
func (s *Schema) DropContext(ctx context.Context) error {
	return s.db.DropSchemaContext(ctx, s.Name)
}

// ChangeOwner transfers schema ownership to a new principal.
func (s *Schema) ChangeOwner(newOwner string) error {
	return s.ChangeOwnerContext(context.Background(), newOwner)
}

// ChangeOwnerContext is the context-aware variant of ChangeOwner.
func (s *Schema) ChangeOwnerContext(ctx context.Context, newOwner string) error {
	q := fmt.Sprintf("ALTER AUTHORIZATION ON SCHEMA::%s TO %s",
		quoteIdent(s.Name), quoteIdent(newOwner))
	if _, err := s.db.exec(ctx, q); err != nil {
		return fmt.Errorf("gosmo: change schema %q owner to %q: %w", s.Name, newOwner, err)
	}
	setIfApplied(ctx, &s.Owner, newOwner)
	return nil
}

// ObjectCount returns the number of objects (tables, views, procedures,
// functions, ...) contained in the schema — SSMS's Owned Schemas "Object
// count" field, fetched lazily only when a schema is selected rather than
// folded into Schema/SchemasContext (used by every tree-list call).
func (s *Schema) ObjectCount() (int, error) {
	return s.ObjectCountContext(context.Background())
}

// ObjectCountContext is the context-aware variant of ObjectCount.
func (s *Schema) ObjectCountContext(ctx context.Context) (int, error) {
	const q = `SELECT COUNT(*) FROM sys.objects o WHERE o.schema_id = SCHEMA_ID(@p1) AND o.is_ms_shipped = 0`

	var count int
	if err := s.db.queryRow(ctx, func(row *sql.Row) error { return row.Scan(&count) }, q, s.Name); err != nil {
		return 0, fmt.Errorf("gosmo: object count for schema %q: %w", s.Name, err)
	}
	return count, nil
}

// SchemaObjectCounts is a per-category breakdown of what a schema contains,
// as Schema Properties' Object summary shows it.
type SchemaObjectCounts struct {
	Tables           int
	Views            int
	StoredProcedures int
	Functions        int
	Synonyms         int
	Sequences        int
}

// ObjectCountsByType returns the schema's contents broken down by category —
// ObjectCount's total, itemized. It is a separate method rather than a
// widening of ObjectCount because the two do not agree: ObjectCount is one
// COUNT over sys.objects, while each count here reproduces the predicate of
// the listing it stands in for, down to the sys.sql_modules join that keeps
// a CLR or extended procedure out of the stored-procedure count.
func (s *Schema) ObjectCountsByType() (SchemaObjectCounts, error) {
	return s.ObjectCountsByTypeContext(context.Background())
}

// ObjectCountsByTypeContext is the context-aware variant of
// ObjectCountsByType.
//
// One round trip of six scalar subqueries rather than a single GROUP BY over
// sys.objects: the counts have to match what ViewsContext,
// StoredProceduresContext, UserDefinedFunctionsContext, SynonymsContext,
// SequencesContext and TablesBySchemaContext would each have returned, and
// those differ in more than the type code — three join sys.sql_modules, two
// do not filter is_ms_shipped, and synonyms and sequences have catalog views
// of their own. A schema that does not exist yields zeros, not an error,
// because SCHEMA_ID returns NULL for it.
func (s *Schema) ObjectCountsByTypeContext(ctx context.Context) (SchemaObjectCounts, error) {
	const q = `
DECLARE @sid INT = SCHEMA_ID(@p1);
SELECT
  (SELECT COUNT(*) FROM sys.tables t
   WHERE  t.schema_id = @sid AND t.is_ms_shipped = 0),
  (SELECT COUNT(*) FROM sys.views v
   JOIN   sys.sql_modules m ON m.object_id = v.object_id
   WHERE  v.schema_id = @sid AND v.is_ms_shipped = 0),
  (SELECT COUNT(*) FROM sys.procedures p
   JOIN   sys.sql_modules m ON m.object_id = p.object_id
   WHERE  p.schema_id = @sid AND p.is_ms_shipped = 0),
  (SELECT COUNT(*) FROM sys.objects o
   JOIN   sys.sql_modules m ON m.object_id = o.object_id
   WHERE  o.schema_id = @sid AND o.type IN ('FN','TF','IF') AND o.is_ms_shipped = 0),
  (SELECT COUNT(*) FROM sys.synonyms sy WHERE sy.schema_id = @sid),
  (SELECT COUNT(*) FROM sys.sequences sq WHERE sq.schema_id = @sid)`

	var c SchemaObjectCounts
	if err := s.db.queryRow(ctx, func(row *sql.Row) error {
		return row.Scan(&c.Tables, &c.Views, &c.StoredProcedures,
			&c.Functions, &c.Synonyms, &c.Sequences)
	}, q, s.Name); err != nil {
		return SchemaObjectCounts{}, fmt.Errorf("gosmo: object counts for schema %q: %w", s.Name, err)
	}
	return c, nil
}

// ============================================================
// User
// ============================================================

// User mirrors Microsoft.SqlServer.Management.Smo.User.
type User struct {
	db            *Database
	Name          string
	ID            int
	UserType      string // "SQL_USER", "WINDOWS_USER", "WINDOWS_GROUP", etc.
	DefaultSchema string
	AuthType      string
	CreateDate    time.Time
	ModifyDate    time.Time
	SID           []byte
	// LoginName is the server login this user's SID matches, or empty if
	// none does — only populated by UserByNameContext (UsersContext's
	// tree-listing query doesn't join sys.server_principals). A blank
	// LoginName is ambiguous by itself: check AuthType too. A genuine
	// CREATE USER ... WITHOUT LOGIN reports AuthType "NONE", while a user
	// created FOR LOGIN whose login was later dropped keeps AuthType
	// "INSTANCE" with no matching login, i.e. orphaned.
	LoginName string
	// LoginDisabled is only meaningful when LoginName is non-empty.
	LoginDisabled bool
}

// Drop drops the database user.
func (u *User) Drop() error { return u.DropContext(context.Background()) }

// DropContext is the context-aware variant of Drop.
func (u *User) DropContext(ctx context.Context) error {
	return u.db.DropUserContext(ctx, u.Name)
}

// Rename changes the database user's name.
func (u *User) Rename(newName string) error {
	return u.RenameContext(context.Background(), newName)
}

// RenameContext is the context-aware variant of Rename.
func (u *User) RenameContext(ctx context.Context, newName string) error {
	q := fmt.Sprintf("ALTER USER %s WITH NAME = %s", quoteIdent(u.Name), quoteIdent(newName))
	if _, err := u.db.exec(ctx, q); err != nil {
		return fmt.Errorf("gosmo: rename database user %q to %q: %w", u.Name, newName, err)
	}
	setIfApplied(ctx, &u.Name, newName)
	return nil
}

// SetDefaultSchema changes the user's default schema.
func (u *User) SetDefaultSchema(schemaName string) error {
	return u.SetDefaultSchemaContext(context.Background(), schemaName)
}

// SetDefaultSchemaContext is the context-aware variant of SetDefaultSchema.
func (u *User) SetDefaultSchemaContext(ctx context.Context, schemaName string) error {
	q := fmt.Sprintf("ALTER USER %s WITH DEFAULT_SCHEMA = %s", quoteIdent(u.Name), quoteIdent(schemaName))
	if _, err := u.db.exec(ctx, q); err != nil {
		return fmt.Errorf("gosmo: set default schema for user %q to %q: %w", u.Name, schemaName, err)
	}
	setIfApplied(ctx, &u.DefaultSchema, schemaName)
	return nil
}

// SetLogin remaps the user to a different server login.
func (u *User) SetLogin(loginName string) error {
	return u.SetLoginContext(context.Background(), loginName)
}

// SetLoginContext is the context-aware variant of SetLogin.
func (u *User) SetLoginContext(ctx context.Context, loginName string) error {
	q := fmt.Sprintf("ALTER USER %s WITH LOGIN = %s", quoteIdent(u.Name), quoteIdent(loginName))
	if _, err := u.db.exec(ctx, q); err != nil {
		return fmt.Errorf("gosmo: map user %q to login %q: %w", u.Name, loginName, err)
	}
	setIfApplied(ctx, &u.LoginName, loginName)
	return nil
}

// AddToRole adds the user to a database role.
func (u *User) AddToRole(roleName string) error {
	return u.AddToRoleContext(context.Background(), roleName)
}

// AddToRoleContext is the context-aware variant of AddToRole.
func (u *User) AddToRoleContext(ctx context.Context, roleName string) error {
	return u.db.AddRoleMemberContext(ctx, roleName, u.Name)
}

// RemoveFromRole removes the user from a database role.
func (u *User) RemoveFromRole(roleName string) error {
	return u.RemoveFromRoleContext(context.Background(), roleName)
}

// RemoveFromRoleContext is the context-aware variant of RemoveFromRole.
func (u *User) RemoveFromRoleContext(ctx context.Context, roleName string) error {
	return u.db.RemoveRoleMemberContext(ctx, roleName, u.Name)
}

// Grant grants a permission on a schema-qualified object to the user.
func (u *User) Grant(permission ObjectPermission, objectSchema, objectName string) error {
	return u.GrantContext(context.Background(), permission, objectSchema, objectName)
}

// GrantContext is the context-aware variant of Grant.
func (u *User) GrantContext(ctx context.Context, permission ObjectPermission, objectSchema, objectName string) error {
	if !validObjectPermission(permission) {
		return fmt.Errorf("gosmo: grant permission: unrecognized permission %q", permission)
	}
	q := fmt.Sprintf("GRANT %s ON %s TO %s",
		permission, qualifiedName(objectSchema, objectName), quoteIdent(u.Name))
	if _, err := u.db.exec(ctx, q); err != nil {
		return fmt.Errorf("gosmo: grant %s to user %q: %w", permission, u.Name, err)
	}
	return nil
}

// Deny denies a permission on a schema-qualified object to the user.
func (u *User) Deny(permission ObjectPermission, objectSchema, objectName string) error {
	return u.DenyContext(context.Background(), permission, objectSchema, objectName)
}

// DenyContext is the context-aware variant of Deny.
func (u *User) DenyContext(ctx context.Context, permission ObjectPermission, objectSchema, objectName string) error {
	if !validObjectPermission(permission) {
		return fmt.Errorf("gosmo: deny permission: unrecognized permission %q", permission)
	}
	q := fmt.Sprintf("DENY %s ON %s TO %s",
		permission, qualifiedName(objectSchema, objectName), quoteIdent(u.Name))
	if _, err := u.db.exec(ctx, q); err != nil {
		return fmt.Errorf("gosmo: deny %s to user %q: %w", permission, u.Name, err)
	}
	return nil
}

// Revoke revokes a permission on a schema-qualified object from the user.
func (u *User) Revoke(permission ObjectPermission, objectSchema, objectName string) error {
	return u.RevokeContext(context.Background(), permission, objectSchema, objectName)
}

// RevokeContext is the context-aware variant of Revoke.
func (u *User) RevokeContext(ctx context.Context, permission ObjectPermission, objectSchema, objectName string) error {
	if !validObjectPermission(permission) {
		return fmt.Errorf("gosmo: revoke permission: unrecognized permission %q", permission)
	}
	q := fmt.Sprintf("REVOKE %s ON %s FROM %s",
		permission, qualifiedName(objectSchema, objectName), quoteIdent(u.Name))
	if _, err := u.db.exec(ctx, q); err != nil {
		return fmt.Errorf("gosmo: revoke %s from user %q: %w", permission, u.Name, err)
	}
	return nil
}
