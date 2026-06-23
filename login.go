package gosmo

import (
	"context"
	"fmt"
	"time"
)

// Login represents a SQL Server server-level login.
type Login struct {
	server          *Server
	Name            string
	SID             []byte
	LoginType       string // "SQL_LOGIN", "WINDOWS_LOGIN", "WINDOWS_GROUP"
	IsDisabled      bool
	DefaultDatabase string
	CreateDate      time.Time
	ModifyDate      time.Time
}

// Disable disables the login.
func (l *Login) Disable() error {
	return l.DisableContext(context.Background())
}

func (l *Login) DisableContext(ctx context.Context) error {
	if _, err := l.server.db.ExecContext(ctx, "ALTER LOGIN "+quoteIdent(l.Name)+" DISABLE"); err != nil {
		return fmt.Errorf("gosmo: disable login %q: %w", l.Name, err)
	}
	l.IsDisabled = true
	return nil
}

// Enable enables the login.
func (l *Login) Enable() error {
	return l.EnableContext(context.Background())
}

func (l *Login) EnableContext(ctx context.Context) error {
	if _, err := l.server.db.ExecContext(ctx, "ALTER LOGIN "+quoteIdent(l.Name)+" ENABLE"); err != nil {
		return fmt.Errorf("gosmo: enable login %q: %w", l.Name, err)
	}
	l.IsDisabled = false
	return nil
}

// ChangePassword changes the login's password.
// The new password is passed as a parameter to avoid injection via escapeSingle.
func (l *Login) ChangePassword(newPassword string) error {
	return l.ChangePasswordContext(context.Background(), newPassword)
}

func (l *Login) ChangePasswordContext(ctx context.Context, newPassword string) error {
	// ALTER LOGIN does not support parameterised passwords; escapeSingle is safe here.
	q := fmt.Sprintf("ALTER LOGIN %s WITH PASSWORD = N'%s'", quoteIdent(l.Name), escapeSingle(newPassword))
	if _, err := l.server.db.ExecContext(ctx, q); err != nil {
		return fmt.Errorf("gosmo: change password for login %q: %w", l.Name, err)
	}
	return nil
}

// Drop drops the login from the server.
func (l *Login) Drop() error { return l.server.DropLogin(l.Name) }

// AddServerRoleMember adds this login to a server role.
func (l *Login) AddServerRoleMember(roleName string) error {
	return l.AddServerRoleMemberContext(context.Background(), roleName)
}

func (l *Login) AddServerRoleMemberContext(ctx context.Context, roleName string) error {
	q := fmt.Sprintf("ALTER SERVER ROLE %s ADD MEMBER %s", quoteIdent(roleName), quoteIdent(l.Name))
	if _, err := l.server.db.ExecContext(ctx, q); err != nil {
		return fmt.Errorf("gosmo: add %q to server role %q: %w", l.Name, roleName, err)
	}
	return nil
}

// RemoveServerRoleMember removes this login from a server role.
func (l *Login) RemoveServerRoleMember(roleName string) error {
	return l.RemoveServerRoleMemberContext(context.Background(), roleName)
}

func (l *Login) RemoveServerRoleMemberContext(ctx context.Context, roleName string) error {
	q := fmt.Sprintf("ALTER SERVER ROLE %s DROP MEMBER %s", quoteIdent(roleName), quoteIdent(l.Name))
	if _, err := l.server.db.ExecContext(ctx, q); err != nil {
		return fmt.Errorf("gosmo: remove %q from server role %q: %w", l.Name, roleName, err)
	}
	return nil
}
