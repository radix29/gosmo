package gosmo

import (
	"context"
	"fmt"
	"strings"
)

// ============================================================
// ServerScripter — server-level principals
// ============================================================

// ServerScripter generates T-SQL scripts for server-level objects, the ones
// that belong to no database: logins and server roles. Scripter's objects all
// live inside a Database, which is why these are not on it.
type ServerScripter struct {
	server *Server
	opts   ScriptOptions
}

// NewServerScripter creates a ServerScripter for the given server.
func NewServerScripter(s *Server, opts ScriptOptions) *ServerScripter {
	return &ServerScripter{server: s, opts: opts}
}

// ScriptLogin generates the CREATE (or DROP) script for one login.
func (sc *ServerScripter) ScriptLogin(name string) (string, error) {
	return sc.ScriptLoginContext(context.Background(), name)
}

// ScriptLoginContext is the context-aware variant of ScriptLogin.
func (sc *ServerScripter) ScriptLoginContext(ctx context.Context, name string) (string, error) {
	l, err := sc.server.LoginByNameContext(ctx, name)
	if err != nil {
		return "", err
	}
	return buildLoginScript(l, sc.opts), nil
}

// buildLoginScript assembles one login's script.
//
// A SQL login's password is stored only as a hash and is not scripted, so the
// statement carries a placeholder for the operator to fill in. Note DROP
// LOGIN has no IF EXISTS form — unlike DROP USER or DROP ROLE — so the drop
// is guarded by SUSER_ID instead.
func buildLoginScript(l *Login, opts ScriptOptions) string {
	var sb strings.Builder
	if v := opts.verb(); v == ScriptDrop || v == ScriptDropAndCreate {
		fmt.Fprintf(&sb, "IF SUSER_ID(N'%s') IS NOT NULL\n    DROP LOGIN %s;\nGO\n",
			escapeSingle(l.Name), quoteIdent(l.Name))
		if v == ScriptDrop {
			return sb.String()
		}
		sb.WriteString("\n")
	}
	if opts.IncludeIfNotExists {
		fmt.Fprintf(&sb, "IF SUSER_ID(N'%s') IS NULL\n", escapeSingle(l.Name))
	}
	stmt := "CREATE LOGIN " + quoteIdent(l.Name)
	switch {
	case strings.HasPrefix(l.LoginType, "WINDOWS"):
		stmt += " FROM WINDOWS"
	case strings.HasPrefix(l.LoginType, "EXTERNAL"):
		stmt += " FROM EXTERNAL PROVIDER"
	default:
		stmt += " WITH PASSWORD = N'<password, sysname, >'"
	}
	if l.DefaultDatabase != "" {
		if strings.Contains(stmt, " WITH ") {
			stmt += ", DEFAULT_DATABASE = " + quoteIdent(l.DefaultDatabase)
		} else {
			stmt += " WITH DEFAULT_DATABASE = " + quoteIdent(l.DefaultDatabase)
		}
	}
	fmt.Fprintf(&sb, "%s;\nGO\n", stmt)
	if l.IsDisabled {
		fmt.Fprintf(&sb, "ALTER LOGIN %s DISABLE;\nGO\n", quoteIdent(l.Name))
	}
	return sb.String()
}

// ScriptServerRole generates the CREATE (or DROP) script for one server role,
// including the ALTER SERVER ROLE statements that restore its membership.
func (sc *ServerScripter) ScriptServerRole(name string) (string, error) {
	return sc.ScriptServerRoleContext(context.Background(), name)
}

// ScriptServerRoleContext is the context-aware variant of ScriptServerRole.
func (sc *ServerScripter) ScriptServerRoleContext(ctx context.Context, name string) (string, error) {
	r, err := sc.server.ServerRoleByNameContext(ctx, name)
	if err != nil {
		return "", err
	}
	return buildServerRoleScript(r, sc.opts), nil
}

// buildServerRoleScript assembles one server role's script. DROP SERVER ROLE
// has no IF EXISTS form, so the drop is guarded the same way a login's is.
func buildServerRoleScript(r *ServerRole, opts ScriptOptions) string {
	var sb strings.Builder
	if v := opts.verb(); v == ScriptDrop || v == ScriptDropAndCreate {
		fmt.Fprintf(&sb, "IF SUSER_ID(N'%s') IS NOT NULL\n    DROP SERVER ROLE %s;\nGO\n",
			escapeSingle(r.Name), quoteIdent(r.Name))
		if v == ScriptDrop {
			return sb.String()
		}
		sb.WriteString("\n")
	}
	if opts.IncludeIfNotExists {
		fmt.Fprintf(&sb, "IF SUSER_ID(N'%s') IS NULL\n", escapeSingle(r.Name))
	}
	fmt.Fprintf(&sb, "CREATE SERVER ROLE %s", quoteIdent(r.Name))
	if r.Owner != "" {
		fmt.Fprintf(&sb, " AUTHORIZATION %s", quoteIdent(r.Owner))
	}
	sb.WriteString(";\nGO\n")
	for _, m := range r.Members {
		fmt.Fprintf(&sb, "ALTER SERVER ROLE %s ADD MEMBER %s;\nGO\n", quoteIdent(r.Name), quoteIdent(m))
	}
	return sb.String()
}
