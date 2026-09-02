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

// ScriptCredential generates the CREATE (or DROP) script for one server-level
// credential.
func (sc *ServerScripter) ScriptCredential(name string) (string, error) {
	return sc.ScriptCredentialContext(context.Background(), name)
}

// ScriptCredentialContext is the context-aware variant of ScriptCredential.
func (sc *ServerScripter) ScriptCredentialContext(ctx context.Context, name string) (string, error) {
	c, err := sc.server.CredentialByNameContext(ctx, name)
	if err != nil {
		return "", err
	}
	return buildCredentialScript(c, sc.opts), nil
}

// ScriptBackupDevice generates the CREATE (or DROP) script for one logical
// backup device.
func (sc *ServerScripter) ScriptBackupDevice(name string) (string, error) {
	return sc.ScriptBackupDeviceContext(context.Background(), name)
}

// ScriptBackupDeviceContext is the context-aware variant of
// ScriptBackupDevice.
func (sc *ServerScripter) ScriptBackupDeviceContext(ctx context.Context, name string) (string, error) {
	d, err := sc.server.BackupDeviceByNameContext(ctx, name)
	if err != nil {
		return "", err
	}
	return buildBackupDeviceScript(d, sc.opts), nil
}

// buildBackupDeviceScript assembles one backup device's script.
//
// sp_dropdevice has no IF EXISTS of its own, so the drop is guarded with a
// sys.backup_devices lookup the way a credential's is. The device's type_desc
// is the catalog's spelling ("VIRTUAL_DEVICE"), not the keyword
// sp_addumpdevice takes, so it is mapped back — a script emitting
// @devtype = N'VIRTUAL_DEVICE' is one the server refuses.
func buildBackupDeviceScript(d *BackupDevice, opts ScriptOptions) string {
	var sb strings.Builder
	if v := opts.verb(); v == ScriptDrop || v == ScriptDropAndCreate {
		fmt.Fprintf(&sb, "IF EXISTS (SELECT 1 FROM sys.backup_devices WHERE name = N'%s')\n    EXEC sp_dropdevice @logicalname = N'%s';\nGO\n",
			escapeSingle(d.Name), escapeSingle(d.Name))
		if v == ScriptDrop {
			return sb.String()
		}
		sb.WriteString("\n")
	}
	if opts.IncludeIfNotExists {
		fmt.Fprintf(&sb, "IF NOT EXISTS (SELECT 1 FROM sys.backup_devices WHERE name = N'%s')\n",
			escapeSingle(d.Name))
	}
	fmt.Fprintf(&sb, "EXEC sp_addumpdevice @devtype = N'%s', @logicalname = N'%s', @physicalname = N'%s';\nGO\n",
		escapeSingle(string(backupDeviceKeyword(d.Type))), escapeSingle(d.Name), escapeSingle(d.PhysicalName))
	return sb.String()
}

// ScriptServerTrigger generates the CREATE (or DROP) script for one
// server-scope DDL or logon trigger.
func (sc *ServerScripter) ScriptServerTrigger(name string) (string, error) {
	return sc.ScriptServerTriggerContext(context.Background(), name)
}

// ScriptServerTriggerContext is the context-aware variant of
// ScriptServerTrigger.
func (sc *ServerScripter) ScriptServerTriggerContext(ctx context.Context, name string) (string, error) {
	t, err := sc.server.ServerTriggerByNameContext(ctx, name)
	if err != nil {
		return "", err
	}
	return buildServerTriggerScript(t, sc.opts)
}

// buildServerTriggerScript assembles one server trigger's script from the
// definition sys.server_sql_modules stores.
//
// A trigger with no readable definition — encrypted, or CLR, which has no row
// in that view at all — is an error rather than an empty CREATE half: emitting
// nothing produces a script that drops the trigger and does not put it back.
// IncludeIfNotExists is not honoured because CREATE TRIGGER must be the first
// statement in its batch, the same reason scriptModule ignores it.
func buildServerTriggerScript(t *ServerTrigger, opts ScriptOptions) (string, error) {
	var sb strings.Builder
	if v := opts.verb(); v == ScriptDrop || v == ScriptDropAndCreate {
		fmt.Fprintf(&sb, "DROP TRIGGER IF EXISTS %s ON ALL SERVER;\nGO\n", quoteIdent(t.Name))
		if v == ScriptDrop {
			return sb.String(), nil
		}
		sb.WriteString("\n")
	}
	if strings.TrimSpace(t.Definition) == "" {
		return "", fmt.Errorf("gosmo: script server trigger %q: definition is not readable (encrypted or CLR)", t.Name)
	}
	def := t.Definition
	if opts.verb() == ScriptAlter {
		def = alterModuleDefinition(def)
	}
	sb.WriteString(def + "\nGO\n")
	if !t.IsEnabled {
		fmt.Fprintf(&sb, "DISABLE TRIGGER %s ON ALL SERVER;\nGO\n", quoteIdent(t.Name))
	}
	return sb.String(), nil
}

// ScriptEndpoint generates the CREATE (or DROP) script for one endpoint.
func (sc *ServerScripter) ScriptEndpoint(name string) (string, error) {
	return sc.ScriptEndpointContext(context.Background(), name)
}

// ScriptEndpointContext is the context-aware variant of ScriptEndpoint.
//
// A built-in endpoint is refused with ErrSystemEndpoint: neither half of its
// script would run, since it can be neither dropped nor created.
func (sc *ServerScripter) ScriptEndpointContext(ctx context.Context, name string) (string, error) {
	e, err := sc.server.EndpointByNameContext(ctx, name)
	if err != nil {
		return "", err
	}
	if e.IsSystem {
		return "", fmt.Errorf("gosmo: script endpoint %q: %w", e.Name, ErrSystemEndpoint)
	}

	var sb strings.Builder
	if v := sc.opts.verb(); v == ScriptDrop || v == ScriptDropAndCreate {
		fmt.Fprintf(&sb, "IF EXISTS (SELECT 1 FROM sys.endpoints WHERE name = N'%s')\n    DROP ENDPOINT %s;\nGO\n",
			escapeSingle(e.Name), quoteIdent(e.Name))
		if v == ScriptDrop {
			return sb.String(), nil
		}
		sb.WriteString("\n")
	}

	payload, err := sc.endpointPayloadClause(ctx, e)
	if err != nil {
		return "", err
	}
	if sc.opts.IncludeIfNotExists {
		fmt.Fprintf(&sb, "IF NOT EXISTS (SELECT 1 FROM sys.endpoints WHERE name = N'%s')\n",
			escapeSingle(e.Name))
	}
	fmt.Fprintf(&sb, "CREATE ENDPOINT %s", quoteIdent(e.Name))
	if e.Owner != "" {
		fmt.Fprintf(&sb, "\n    AUTHORIZATION %s", quoteIdent(e.Owner))
	}
	if e.State != "" {
		fmt.Fprintf(&sb, "\n    STATE = %s", e.State)
	}
	fmt.Fprintf(&sb, "\n    AS TCP (LISTENER_PORT = %d, LISTENER_IP = ALL)\n    FOR %s;\nGO\n", e.Port, payload)
	return sb.String(), nil
}

// endpointPayloadClause builds the FOR <payload> half of CREATE ENDPOINT,
// reading the type-specific catalog view the payload needs.
//
// A payload this cannot reproduce is an error rather than an omitted clause:
// CREATE ENDPOINT requires one, so a script without it does not run, and a
// script with the wrong one creates a different endpoint than the original.
//
// One value is genuinely unrecoverable and is written as REQUIRED: the catalog
// records only is_encryption_enabled, which does not separate ENCRYPTION =
// REQUIRED from = SUPPORTED. SSMS's own script makes the same substitution.
func (sc *ServerScripter) endpointPayloadClause(ctx context.Context, e *Endpoint) (string, error) {
	switch e.Type {
	case "TSQL":
		return "TSQL ()", nil

	case "DATABASE_MIRRORING":
		d, err := e.MirroringDetailContext(ctx)
		if err != nil {
			return "", err
		}
		if d == nil {
			return "", fmt.Errorf("gosmo: script endpoint %q: its database mirroring detail could not be read", e.Name)
		}
		cert, err := e.mirroringCertificateName(ctx)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("DATABASE_MIRRORING (AUTHENTICATION = %s, ENCRYPTION = %s, ROLE = %s)",
			endpointAuthClause(d.ConnectionAuth, cert),
			endpointEncryptionClause(d.IsEncryptionEnabled, d.EncryptionAlgorithm),
			orElse(d.Role, "ALL")), nil

	case "SERVICE_BROKER":
		d, err := e.ServiceBrokerDetailContext(ctx)
		if err != nil {
			return "", err
		}
		if d == nil {
			return "", fmt.Errorf("gosmo: script endpoint %q: its service broker detail could not be read", e.Name)
		}
		forwarding := "DISABLED"
		if d.IsMessageForwardingEnabled {
			forwarding = "ENABLED"
		}
		return fmt.Sprintf("SERVICE_BROKER (AUTHENTICATION = %s, ENCRYPTION = %s, MESSAGE_FORWARDING = %s, MESSAGE_FORWARD_SIZE = %d)",
			endpointAuthClause(d.ConnectionAuth, d.CertificateName),
			endpointEncryptionClause(d.EncryptionAlgorithm != "" && d.EncryptionAlgorithm != "NONE", d.EncryptionAlgorithm),
			forwarding, d.MessageForwardingSize), nil

	default:
		return "", fmt.Errorf("gosmo: script endpoint %q: goSMO cannot script a %s endpoint", e.Name, orElse(e.Type, "typeless"))
	}
}

// endpointAuthClause turns a connection_auth_desc back into the AUTHENTICATION
// clause that produces it. The desc names the methods in the order the clause
// listed them ("NTLM, CERTIFICATE"), and each Windows method is spelled
// "WINDOWS <method>" in the clause but bare in the desc.
func endpointAuthClause(desc, certificate string) string {
	if desc == "" {
		return "WINDOWS NEGOTIATE"
	}
	parts := strings.Split(desc, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		switch p = strings.TrimSpace(p); p {
		case "":
		case "CERTIFICATE":
			if certificate == "" {
				// Naming no certificate would be a clause the server refuses,
				// which beats one that silently authenticates differently.
				out = append(out, "CERTIFICATE <certificate name>")
			} else {
				out = append(out, "CERTIFICATE "+quoteIdent(certificate))
			}
		default:
			out = append(out, "WINDOWS "+p)
		}
	}
	if len(out) == 0 {
		return "WINDOWS NEGOTIATE"
	}
	return strings.Join(out, " ")
}

// endpointEncryptionClause builds the ENCRYPTION clause. See
// endpointPayloadClause for why an enabled one is always REQUIRED.
func endpointEncryptionClause(enabled bool, algorithm string) string {
	if !enabled {
		return "DISABLED"
	}
	if algorithm == "" || algorithm == "NONE" {
		return "REQUIRED"
	}
	return "REQUIRED ALGORITHM " + algorithm
}

// backupDeviceKeyword maps sys.backup_devices.type_desc to the keyword
// sp_addumpdevice takes.
func backupDeviceKeyword(typeDesc string) BackupDeviceType {
	switch strings.ToUpper(typeDesc) {
	case "TAPE":
		return BackupDeviceTape
	default:
		return BackupDeviceDisk
	}
}

// credentialSecretPlaceholder stands in for the secret in a generated script.
// The stored secret is not readable through any catalog view, and emitting no
// SECRET clause at all would produce a script that silently creates the
// credential without one — so the script carries a placeholder that cannot be
// mistaken for a real value, and says so.
const credentialSecretPlaceholder = "<insert secret here>"

// buildCredentialScript assembles one credential's script. DROP CREDENTIAL has
// no IF EXISTS form, so the drop is guarded with a sys.credentials lookup the
// way a server role's is guarded with SUSER_ID.
func buildCredentialScript(c *Credential, opts ScriptOptions) string {
	var sb strings.Builder
	if v := opts.verb(); v == ScriptDrop || v == ScriptDropAndCreate {
		fmt.Fprintf(&sb, "IF EXISTS (SELECT 1 FROM sys.credentials WHERE name = N'%s')\n    DROP CREDENTIAL %s;\nGO\n",
			escapeSingle(c.Name), quoteIdent(c.Name))
		if v == ScriptDrop {
			return sb.String()
		}
		sb.WriteString("\n")
	}
	sb.WriteString("/* The credential's secret cannot be read from the server. Replace the\n" +
		"   placeholder below, or remove the SECRET clause if it has none. */\n")
	if opts.IncludeIfNotExists {
		fmt.Fprintf(&sb, "IF NOT EXISTS (SELECT 1 FROM sys.credentials WHERE name = N'%s')\n",
			escapeSingle(c.Name))
	}
	fmt.Fprintf(&sb, "CREATE CREDENTIAL %s WITH IDENTITY = N'%s', SECRET = N'%s'",
		quoteIdent(c.Name), escapeSingle(c.Identity), credentialSecretPlaceholder)
	if c.CryptographicProvider != "" {
		fmt.Fprintf(&sb, " FOR CRYPTOGRAPHIC PROVIDER %s", quoteIdent(c.CryptographicProvider))
	}
	sb.WriteString(";\nGO\n")
	return sb.String()
}

// ScriptServerAudit generates the CREATE (or DROP) script for one server audit.
func (sc *ServerScripter) ScriptServerAudit(name string) (string, error) {
	return sc.ScriptServerAuditContext(context.Background(), name)
}

// ScriptServerAuditContext is the context-aware variant of ScriptServerAudit.
func (sc *ServerScripter) ScriptServerAuditContext(ctx context.Context, name string) (string, error) {
	a, err := sc.server.ServerAuditByNameContext(ctx, name)
	if err != nil {
		return "", err
	}
	return buildServerAuditScript(a, sc.opts), nil
}

// buildServerAuditScript assembles one server audit's script.
//
// Both halves are wrapped in BEGIN/END blocks rather than the bare one-statement
// guard the other server scripters use, because each half is two statements: the
// server refuses DROP SERVER AUDIT while the audit is enabled, so the drop has to
// disable it first, and an enabled audit has to be switched back on after the
// create. DROP SERVER AUDIT has no IF EXISTS form of its own, hence the catalog
// guard — the same shape as the credential's.
func buildServerAuditScript(a *ServerAudit, opts ScriptOptions) string {
	var sb strings.Builder
	if v := opts.verb(); v == ScriptDrop || v == ScriptDropAndCreate {
		fmt.Fprintf(&sb, "IF EXISTS (SELECT 1 FROM sys.server_audits WHERE name = N'%s')\nBEGIN\n"+
			"    ALTER SERVER AUDIT %s WITH ( STATE = OFF );\n    DROP SERVER AUDIT %s;\nEND\nGO\n",
			escapeSingle(a.Name), quoteIdent(a.Name), quoteIdent(a.Name))
		if v == ScriptDrop {
			return sb.String()
		}
		sb.WriteString("\n")
	}

	spec := ServerAuditSpec{
		Name:             a.Name,
		Type:             a.Type,
		QueueDelay:       a.QueueDelay,
		OnFailure:        a.OnFailure,
		Predicate:        a.Predicate,
		FilePath:         strings.TrimRight(a.LogFilePath, `\/`),
		MaxFileSize:      a.MaxFileSize,
		MaxRolloverFiles: a.MaxRolloverFiles,
		MaxFiles:         a.MaxFiles,
		ReserveDiskSpace: a.ReserveDiskSpace,
	}
	// The spec builder validates a caller-supplied spec; this one is built
	// from a row that already exists on the server, so the error cannot fire.
	create, _ := spec.createServerAuditStatement()

	if opts.IncludeIfNotExists {
		fmt.Fprintf(&sb, "IF NOT EXISTS (SELECT 1 FROM sys.server_audits WHERE name = N'%s')\nBEGIN\n%s\nEND\nGO\n",
			escapeSingle(a.Name), create)
	} else {
		sb.WriteString(create + "\nGO\n")
	}
	if a.IsEnabled {
		fmt.Fprintf(&sb, "\nALTER SERVER AUDIT %s WITH ( STATE = ON );\nGO\n", quoteIdent(a.Name))
	}
	return sb.String()
}

// ScriptServerAuditSpecification generates the CREATE (or DROP) script for one
// server audit specification.
func (sc *ServerScripter) ScriptServerAuditSpecification(name string) (string, error) {
	return sc.ScriptServerAuditSpecificationContext(context.Background(), name)
}

// ScriptServerAuditSpecificationContext is the context-aware variant of
// ScriptServerAuditSpecification.
func (sc *ServerScripter) ScriptServerAuditSpecificationContext(ctx context.Context, name string) (string, error) {
	spec, err := sc.server.ServerAuditSpecificationByNameContext(ctx, name)
	if err != nil {
		return "", err
	}
	return buildServerAuditSpecificationScript(spec, sc.opts)
}

// buildServerAuditSpecificationScript assembles one specification's script.
//
// An orphaned specification — one whose audit has been dropped out from under
// it, which SQL Server allows — is refused rather than scripted with an empty
// FOR SERVER AUDIT clause, which would not parse.
func buildServerAuditSpecificationScript(s *ServerAuditSpecification, opts ScriptOptions) (string, error) {
	var sb strings.Builder
	if v := opts.verb(); v == ScriptDrop || v == ScriptDropAndCreate {
		fmt.Fprintf(&sb, "IF EXISTS (SELECT 1 FROM sys.server_audit_specifications WHERE name = N'%s')\nBEGIN\n"+
			"    ALTER SERVER AUDIT SPECIFICATION %s WITH ( STATE = OFF );\n"+
			"    DROP SERVER AUDIT SPECIFICATION %s;\nEND\nGO\n",
			escapeSingle(s.Name), quoteIdent(s.Name), quoteIdent(s.Name))
		if v == ScriptDrop {
			return sb.String(), nil
		}
		sb.WriteString("\n")
	}

	if s.AuditName == "" {
		return "", fmt.Errorf("gosmo: script server audit specification %q: it names no audit", s.Name)
	}
	create, err := ServerAuditSpecificationSpec{
		Name:         s.Name,
		AuditName:    s.AuditName,
		ActionGroups: s.ActionGroups,
		Enabled:      s.IsEnabled,
	}.createStatement()
	if err != nil {
		return "", fmt.Errorf("gosmo: script server audit specification %q: %w", s.Name, err)
	}

	if opts.IncludeIfNotExists {
		fmt.Fprintf(&sb, "IF NOT EXISTS (SELECT 1 FROM sys.server_audit_specifications WHERE name = N'%s')\nBEGIN\n%s\nEND\nGO\n",
			escapeSingle(s.Name), create)
	} else {
		sb.WriteString(create + "\nGO\n")
	}
	return sb.String(), nil
}
