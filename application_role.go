package gosmo

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// -- Application roles ---------------------------------------------------------

// ApplicationRole is a database application role: a principal (type 'A')
// that a session takes on with sp_setapprole and a password, rather than one
// a user is a member of. It shares the database principal namespace with
// users and database roles.
type ApplicationRole struct {
	db            *Database
	Name          string
	DefaultSchema string // "" when none is set
	PrincipalID   int
	CreateDate    time.Time
	ModifyDate    time.Time
}

// Database returns the database the application role belongs to.
func (r *ApplicationRole) Database() *Database { return r.db }

// applicationRoleSelect is the one select ApplicationRoles and
// ApplicationRoleByName share, so both fill every field.
const applicationRoleSelect = `
SELECT name, default_schema_name, principal_id, create_date, modify_date
FROM   sys.database_principals
WHERE  type = 'A'`

// scanApplicationRole builds an ApplicationRole from one
// applicationRoleSelect row.
func scanApplicationRole(d *Database, scan func(...any) error) (*ApplicationRole, error) {
	r := &ApplicationRole{db: d}
	var schema sql.NullString
	if err := scan(&r.Name, &schema, &r.PrincipalID, &r.CreateDate, &r.ModifyDate); err != nil {
		return nil, err
	}
	r.DefaultSchema = schema.String
	return r, nil
}

// ApplicationRoles returns the database's application roles, by name.
func (d *Database) ApplicationRoles(ctx context.Context) ([]*ApplicationRole, error) {
	rows, err := d.query(ctx, applicationRoleSelect+`
ORDER  BY name`)
	return scanRows(rows, err, fmt.Sprintf("list application roles in %q", d.Name), func(scan func(...any) error) (*ApplicationRole, error) {
		return scanApplicationRole(d, scan)
	})
}

// ApplicationRoleByName returns one application role, or a not-found error
// (errors.Is ErrNotFound) when the database has none by that name.
func (d *Database) ApplicationRoleByName(ctx context.Context, name string) (*ApplicationRole, error) {
	return readByName(ctx, d, scanApplicationRole, applicationRoleSelect+`
   AND name = @p1`, []any{name},
		notFoundf("gosmo: application role %q not found in %q", name, d.Name),
		fmt.Sprintf("read application role %q in %q", name, d.Name))
}

// ApplicationRoleRef returns a lightweight handle for an application role by
// name, without querying the catalog — the counterpart of
// Server.DatabaseRef. Every field but the name stays at its zero value;
// ApplicationRoleByName is what populates them.
//
// Every write on *ApplicationRole addresses it by name, so this handle is
// enough to change or drop one the caller already knows exists — and is the
// form to use when there is nothing to read yet, such as a script of a
// CREATE that was only collected.
func (d *Database) ApplicationRoleRef(name string) *ApplicationRole {
	return &ApplicationRole{db: d, Name: name}
}

// CreateApplicationRoleRequest describes a new application role.
type CreateApplicationRoleRequest struct {
	Name string
	// Password is what sp_setapprole must present. Required: CREATE
	// APPLICATION ROLE has no form without one.
	Password string
	// DefaultSchema is the schema unqualified names resolve in while the role
	// is active; empty leaves it to the server (dbo).
	DefaultSchema string
}

// CreateApplicationRole creates a new application role, and returns it read
// back from the catalog — or, under Scripting(ctx), the ApplicationRoleRef
// handle, since nothing ran. The password goes through QuoteLiteral, and a
// capture or observer sees a placeholder in its place.
func (d *Database) CreateApplicationRole(ctx context.Context, req CreateApplicationRoleRequest) (*ApplicationRole, error) {
	if req.Name == "" {
		return nil, invalidf("gosmo: create application role: name is required")
	}
	if req.Password == "" {
		return nil, invalidf("gosmo: create application role %q: a password is required", req.Name)
	}
	stmt := "CREATE APPLICATION ROLE " + quoteIdent(req.Name) + " WITH PASSWORD = " + QuoteLiteral(req.Password)
	if req.DefaultSchema != "" {
		stmt += ", DEFAULT_SCHEMA = " + quoteIdent(req.DefaultSchema)
	}
	if _, err := d.execPasswords(ctx, stmt, req.Password); err != nil {
		return nil, fmt.Errorf("gosmo: create application role %q: %w", req.Name, err)
	}
	return createdObject(ctx, d.ApplicationRoleRef(req.Name), func() (*ApplicationRole, error) {
		return d.ApplicationRoleByName(ctx, req.Name)
	})
}

// Rename changes the application role's name.
func (r *ApplicationRole) Rename(ctx context.Context, newName string) error {
	q := fmt.Sprintf("ALTER APPLICATION ROLE %s WITH NAME = %s", quoteIdent(r.Name), quoteIdent(newName))
	if _, err := r.db.exec(ctx, q); err != nil {
		return fmt.Errorf("gosmo: rename application role %q to %q: %w", r.Name, newName, err)
	}
	setIfApplied(ctx, &r.Name, newName)
	return nil
}

// SetDefaultSchema changes the application role's default schema.
func (r *ApplicationRole) SetDefaultSchema(ctx context.Context, schemaName string) error {
	q := fmt.Sprintf("ALTER APPLICATION ROLE %s WITH DEFAULT_SCHEMA = %s", quoteIdent(r.Name), quoteIdent(schemaName))
	if _, err := r.db.exec(ctx, q); err != nil {
		return fmt.Errorf("gosmo: set default schema for application role %q to %q: %w", r.Name, schemaName, err)
	}
	setIfApplied(ctx, &r.DefaultSchema, schemaName)
	return nil
}

// ChangePassword sets the application role's password. It goes through
// QuoteLiteral, and a capture or observer sees a placeholder in its place.
// An empty password is refused here, as CreateApplicationRole refuses one.
func (r *ApplicationRole) ChangePassword(ctx context.Context, newPassword string) error {
	if newPassword == "" {
		return invalidf("gosmo: change password of application role %q: a password is required", r.Name)
	}
	stmt := "ALTER APPLICATION ROLE " + quoteIdent(r.Name) + " WITH PASSWORD = " + QuoteLiteral(newPassword)
	if _, err := r.db.execPasswords(ctx, stmt, newPassword); err != nil {
		return fmt.Errorf("gosmo: change password of application role %q: %w", r.Name, err)
	}
	return nil
}

// Drop drops the application role. One that still owns a schema is refused
// by the server, not here.
func (r *ApplicationRole) Drop(ctx context.Context) error {
	if _, err := r.db.exec(ctx, "DROP APPLICATION ROLE "+quoteIdent(r.Name)); err != nil {
		return fmt.Errorf("gosmo: drop application role %q: %w", r.Name, err)
	}
	return nil
}
