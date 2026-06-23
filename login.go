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
	_, err := l.server.db.ExecContext(context.Background(),
		fmt.Sprintf("ALTER LOGIN [%s] DISABLE", l.Name))
	if err != nil {
		return fmt.Errorf("gosmo: disable login %q: %w", l.Name, err)
	}
	l.IsDisabled = true
	return nil
}

// Enable enables the login.
func (l *Login) Enable() error {
	_, err := l.server.db.ExecContext(context.Background(),
		fmt.Sprintf("ALTER LOGIN [%s] ENABLE", l.Name))
	if err != nil {
		return fmt.Errorf("gosmo: enable login %q: %w", l.Name, err)
	}
	l.IsDisabled = false
	return nil
}

// ChangePassword changes the login password.
func (l *Login) ChangePassword(newPassword string) error {
	_, err := l.server.db.ExecContext(context.Background(),
		fmt.Sprintf("ALTER LOGIN [%s] WITH PASSWORD = N'%s'", l.Name, escapeSingle(newPassword)))
	if err != nil {
		return fmt.Errorf("gosmo: change password for login %q: %w", l.Name, err)
	}
	return nil
}

// Drop drops the login from the server.
func (l *Login) Drop() error {
	return l.server.DropLogin(l.Name)
}

// AddServerRoleMember adds this login to a fixed server role.
func (l *Login) AddServerRoleMember(roleName string) error {
	_, err := l.server.db.ExecContext(context.Background(),
		fmt.Sprintf("ALTER SERVER ROLE [%s] ADD MEMBER [%s]", roleName, l.Name))
	if err != nil {
		return fmt.Errorf("gosmo: add %q to server role %q: %w", l.Name, roleName, err)
	}
	return nil
}

// RemoveServerRoleMember removes this login from a fixed server role.
func (l *Login) RemoveServerRoleMember(roleName string) error {
	_, err := l.server.db.ExecContext(context.Background(),
		fmt.Sprintf("ALTER SERVER ROLE [%s] DROP MEMBER [%s]", roleName, l.Name))
	if err != nil {
		return fmt.Errorf("gosmo: remove %q from server role %q: %w", l.Name, roleName, err)
	}
	return nil
}
