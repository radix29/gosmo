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
//
// A certificate- or asymmetric-key-mapped login needs one more read than the
// others: the object it maps to is named in master, not in the login's own
// row. ResolveMappingContext is a no-op for every other type.
func (sc *ServerScripter) ScriptLoginContext(ctx context.Context, name string) (string, error) {
	l, err := sc.server.LoginByNameContext(ctx, name)
	if err != nil {
		return "", err
	}
	if err := l.ResolveMappingContext(ctx); err != nil {
		return "", err
	}
	return buildLoginScript(l, sc.opts), nil
}

// buildLoginScript assembles one login's script.
//
// Only a SQL or Windows login can carry DEFAULT_DATABASE in CREATE LOGIN;
// FROM EXTERNAL PROVIDER takes no WITH option list, so an external login's
// goes out as a following ALTER LOGIN. A certificate- or asymmetric-key-
// mapped login gets neither: SQL Server refuses DEFAULT_DATABASE for those in
// CREATE *and* ALTER ("Cannot use the parameter DEFAULT_DATABASE for a
// certificate or asymmetric key login", verified live), while still reporting
// one in sys.server_principals — so scripting the value back is a script that
// fails on the login it came from.
//
// A SQL login's password is stored only as a hash and is not scripted, so the
// statement carries a placeholder for the operator to fill in. Its SID is,
// and that is not decoration: a login recreated on another server with a
// fresh SID leaves every database user that was mapped to it orphaned, which
// is the main reason to script a login at all. SID is a SQL-login clause
// only — a Windows or external login's SID comes from the directory, and
// naming one is a syntax error.
//
// Note DROP LOGIN has no IF EXISTS form — unlike DROP USER or DROP ROLE — so
// the drop is guarded by SUSER_ID instead.
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

	// withOpen tracks whether the WITH keyword has already been emitted, so
	// the next clause knows to continue the list with a comma. Read from the
	// branch taken, never sniffed back out of stmt with strings.Contains: a
	// login legitimately named [svc WITH rights] made a Windows login's
	// DEFAULT_DATABASE continue a WITH list that was never opened.
	withOpen := false
	// takesWithOptions is the other half of the same rule: CREATE LOGIN's
	// WITH list exists for SQL and Windows logins only, so DEFAULT_DATABASE
	// goes out as an ALTER for the rest rather than as a clause that does
	// not parse.
	takesWithOptions := true
	// alterDefaultDB is the third state: the login takes a default database,
	// but only through a separate ALTER LOGIN.
	alterDefaultDB := false
	switch {
	case strings.HasPrefix(l.LoginType, "WINDOWS"):
		stmt += " FROM WINDOWS"
	case strings.HasPrefix(l.LoginType, "EXTERNAL"):
		stmt += " FROM EXTERNAL PROVIDER"
		takesWithOptions = false
		alterDefaultDB = true
	case l.LoginType == "CERTIFICATE_MAPPED_LOGIN":
		stmt += " FROM CERTIFICATE " + mappedObjectName(l, "certificate")
		takesWithOptions = false
	case l.LoginType == "ASYMMETRIC_KEY_MAPPED_LOGIN":
		stmt += " FROM ASYMMETRIC KEY " + mappedObjectName(l, "asymmetric key")
		takesWithOptions = false
	default:
		stmt += " WITH PASSWORD = N'<password, sysname, >'"
		withOpen = true
		if len(l.SID) > 0 {
			stmt += ", SID = " + hexLiteral(l.SID)
		}
	}
	if l.DefaultDatabase != "" && takesWithOptions {
		if withOpen {
			stmt += ", "
		} else {
			stmt += " WITH "
			withOpen = true
		}
		stmt += "DEFAULT_DATABASE = " + quoteIdent(l.DefaultDatabase)
	}
	fmt.Fprintf(&sb, "%s;\nGO\n", stmt)
	if l.DefaultDatabase != "" && alterDefaultDB {
		fmt.Fprintf(&sb, "ALTER LOGIN %s WITH DEFAULT_DATABASE = %s;\nGO\n",
			quoteIdent(l.Name), quoteIdent(l.DefaultDatabase))
	}
	if l.IsDisabled {
		fmt.Fprintf(&sb, "ALTER LOGIN %s DISABLE;\nGO\n", quoteIdent(l.Name))
	}
	return sb.String()
}

// mappedObjectName renders the certificate or asymmetric key a mapped login
// points at. ResolveMapping leaves MappedObject empty when the object has
// been dropped or the caller never resolved it, and a script that silently
// named nothing would not parse — an SSMS-style placeholder says what the
// operator has to fill in.
func mappedObjectName(l *Login, kind string) string {
	if l.MappedObject == "" {
		return fmt.Sprintf("[<%s name, sysname, >]", kind)
	}
	return quoteIdent(l.MappedObject)
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
