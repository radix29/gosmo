package gosmo

import (
	"context"
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
func (s *Schema) Drop() error { return s.db.DropSchema(s.Name) }

// ChangeOwner transfers schema ownership to a new principal.
func (s *Schema) ChangeOwner(newOwner string) error {
	return s.ChangeOwnerContext(context.Background(), newOwner)
}

func (s *Schema) ChangeOwnerContext(ctx context.Context, newOwner string) error {
	q := fmt.Sprintf("ALTER AUTHORIZATION ON SCHEMA::%s TO %s",
		quoteIdent(s.Name), quoteIdent(newOwner))
	if _, err := s.db.exec(ctx, q); err != nil {
		return fmt.Errorf("gosmo: change schema %q owner to %q: %w", s.Name, newOwner, err)
	}
	s.Owner = newOwner
	return nil
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
}

// Drop drops the database user.
func (u *User) Drop() error { return u.db.DropUser(u.Name) }

// AddToRole adds the user to a database role.
func (u *User) AddToRole(roleName string) error {
	return u.db.AddRoleMember(roleName, u.Name)
}

// RemoveFromRole removes the user from a database role.
func (u *User) RemoveFromRole(roleName string) error {
	return u.db.RemoveRoleMember(roleName, u.Name)
}

// Grant grants a permission on a schema-qualified object to the user.
func (u *User) Grant(permission ObjectPermission, objectSchema, objectName string) error {
	return u.GrantContext(context.Background(), permission, objectSchema, objectName)
}

func (u *User) GrantContext(ctx context.Context, permission ObjectPermission, objectSchema, objectName string) error {
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

func (u *User) DenyContext(ctx context.Context, permission ObjectPermission, objectSchema, objectName string) error {
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

func (u *User) RevokeContext(ctx context.Context, permission ObjectPermission, objectSchema, objectName string) error {
	q := fmt.Sprintf("REVOKE %s ON %s FROM %s",
		permission, qualifiedName(objectSchema, objectName), quoteIdent(u.Name))
	if _, err := u.db.exec(ctx, q); err != nil {
		return fmt.Errorf("gosmo: revoke %s from user %q: %w", permission, u.Name, err)
	}
	return nil
}
