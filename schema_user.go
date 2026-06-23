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
func (s *Schema) Drop() error {
	return s.db.DropSchema(s.Name)
}

// ChangeOwner changes the schema owner.
func (s *Schema) ChangeOwner(newOwner string) error {
	_, err := s.db.exec(context.Background(),
		fmt.Sprintf("ALTER AUTHORIZATION ON SCHEMA::[%s] TO [%s]", s.Name, newOwner))
	if err != nil {
		return fmt.Errorf("gosmo: change schema owner: %w", err)
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
func (u *User) Drop() error {
	return u.db.DropUser(u.Name)
}

// AddToRole adds the user to a database role.
func (u *User) AddToRole(roleName string) error {
	return u.db.AddRoleMember(roleName, u.Name)
}

// RemoveFromRole removes the user from a database role.
func (u *User) RemoveFromRole(roleName string) error {
	return u.db.RemoveRoleMember(roleName, u.Name)
}

// Grant grants a permission on an object to the user.
func (u *User) Grant(permission ObjectPermission, objectSchema, objectName string) error {
	q := fmt.Sprintf("GRANT %s ON [%s].[%s] TO [%s]",
		permission, objectSchema, objectName, u.Name)
	_, err := u.db.exec(context.Background(), q)
	if err != nil {
		return fmt.Errorf("gosmo: grant %s to user %q: %w", permission, u.Name, err)
	}
	return nil
}

// Deny denies a permission on an object to the user.
func (u *User) Deny(permission ObjectPermission, objectSchema, objectName string) error {
	q := fmt.Sprintf("DENY %s ON [%s].[%s] TO [%s]",
		permission, objectSchema, objectName, u.Name)
	_, err := u.db.exec(context.Background(), q)
	if err != nil {
		return fmt.Errorf("gosmo: deny %s to user %q: %w", permission, u.Name, err)
	}
	return nil
}

// Revoke revokes a permission on an object from the user.
func (u *User) Revoke(permission ObjectPermission, objectSchema, objectName string) error {
	q := fmt.Sprintf("REVOKE %s ON [%s].[%s] FROM [%s]",
		permission, objectSchema, objectName, u.Name)
	_, err := u.db.exec(context.Background(), q)
	if err != nil {
		return fmt.Errorf("gosmo: revoke %s from user %q: %w", permission, u.Name, err)
	}
	return nil
}
