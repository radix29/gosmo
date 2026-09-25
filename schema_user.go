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

// Database returns the database the schema belongs to.
func (s *Schema) Database() *Database { return s.db }

// Drop drops the schema.
func (s *Schema) Drop(ctx context.Context) error {
	if _, err := s.db.exec(ctx, "DROP SCHEMA "+quoteIdent(s.Name)); err != nil {
		return fmt.Errorf("gosmo: drop schema %q: %w", s.Name, err)
	}
	return nil
}

// SetOwner transfers schema ownership to a new principal.
func (s *Schema) SetOwner(ctx context.Context, newOwner string) error {
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
// folded into Schema/Schemas (used by every tree-list call).
func (s *Schema) ObjectCount(ctx context.Context) (int, error) {
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
//
// One round trip of six scalar subqueries rather than a single GROUP BY over
// sys.objects: the counts have to match what Views,
// StoredProcedures, UserDefinedFunctions, Synonyms,
// Sequences and TablesBySchema would each have returned, and
// those differ in more than the type code — three join sys.sql_modules, two
// do not filter is_ms_shipped, and synonyms and sequences have catalog views
// of their own. A schema that does not exist yields zeros, not an error,
// because SCHEMA_ID returns NULL for it.
func (s *Schema) ObjectCountsByType(ctx context.Context) (SchemaObjectCounts, error) {
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
	db   *Database
	Name string
	ID   int
	// UserType is type_desc: "SQL_USER", "WINDOWS_USER", "WINDOWS_GROUP",
	// "EXTERNAL_USER", "EXTERNAL_GROUPS", "CERTIFICATE_MAPPED_USER" or
	// "ASYMMETRIC_KEY_MAPPED_USER".
	UserType      string
	DefaultSchema string
	AuthType      string
	CreateDate    time.Time
	ModifyDate    time.Time
	SID           []byte
	// LoginName is the server login this user's SID matches, or empty if
	// none does — only populated by UserByName (Users's
	// tree-listing query doesn't join sys.server_principals). A blank
	// LoginName is ambiguous by itself: check AuthType too. A genuine
	// CREATE USER ... WITHOUT LOGIN reports AuthType "NONE", while a user
	// created FOR LOGIN whose login was later dropped keeps AuthType
	// "INSTANCE" with no matching login, i.e. orphaned.
	LoginName string
	// LoginDisabled is only meaningful when LoginName is non-empty.
	LoginDisabled bool
	// MappedObject is the certificate or asymmetric key in this database a
	// CERTIFICATE_MAPPED_USER / ASYMMETRIC_KEY_MAPPED_USER maps to, matched
	// by SID. Populated by UserByName only, and empty when the object has
	// been dropped out from under the user.
	MappedObject string
}

// Database returns the database the user belongs to.
func (u *User) Database() *Database { return u.db }

// Drop drops the database user.
func (u *User) Drop(ctx context.Context) error {
	if _, err := u.db.exec(ctx, "DROP USER "+quoteIdent(u.Name)); err != nil {
		return fmt.Errorf("gosmo: drop user %q: %w", u.Name, err)
	}
	return nil
}

// Rename changes the database user's name.
func (u *User) Rename(ctx context.Context, newName string) error {
	q := fmt.Sprintf("ALTER USER %s WITH NAME = %s", quoteIdent(u.Name), quoteIdent(newName))
	if _, err := u.db.exec(ctx, q); err != nil {
		return fmt.Errorf("gosmo: rename database user %q to %q: %w", u.Name, newName, err)
	}
	setIfApplied(ctx, &u.Name, newName)
	return nil
}

// SetDefaultSchema changes the user's default schema.
func (u *User) SetDefaultSchema(ctx context.Context, schemaName string) error {
	q := fmt.Sprintf("ALTER USER %s WITH DEFAULT_SCHEMA = %s", quoteIdent(u.Name), quoteIdent(schemaName))
	if _, err := u.db.exec(ctx, q); err != nil {
		return fmt.Errorf("gosmo: set default schema for user %q to %q: %w", u.Name, schemaName, err)
	}
	setIfApplied(ctx, &u.DefaultSchema, schemaName)
	return nil
}

// SetLogin remaps the user to a different server login.
func (u *User) SetLogin(ctx context.Context, loginName string) error {
	q := fmt.Sprintf("ALTER USER %s WITH LOGIN = %s", quoteIdent(u.Name), quoteIdent(loginName))
	if _, err := u.db.exec(ctx, q); err != nil {
		return fmt.Errorf("gosmo: map user %q to login %q: %w", u.Name, loginName, err)
	}
	setIfApplied(ctx, &u.LoginName, loginName)
	return nil
}

// AddToRole adds the user to a database role.
func (u *User) AddToRole(ctx context.Context, roleName string) error {
	return u.db.AddRoleMember(ctx, roleName, u.Name)
}

// RemoveFromRole removes the user from a database role.
func (u *User) RemoveFromRole(ctx context.Context, roleName string) error {
	return u.db.RemoveRoleMember(ctx, roleName, u.Name)
}

// Grant grants a permission on a schema-qualified object to the user.
func (u *User) Grant(ctx context.Context, permission ObjectPermission, objectSchema, objectName string) error {
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
func (u *User) Deny(ctx context.Context, permission ObjectPermission, objectSchema, objectName string) error {
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
func (u *User) Revoke(ctx context.Context, permission ObjectPermission, objectSchema, objectName string) error {
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
