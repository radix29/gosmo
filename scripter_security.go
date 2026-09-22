package gosmo

import (
	"context"
	"fmt"
	"strings"
)

// ============================================================
// Scripter — database principals (schema, user, role)
// ============================================================

// ScriptSchema generates the CREATE (or DROP) script for one schema.
func (sc *Scripter) ScriptSchema(ctx context.Context, name string) (string, error) {
	schemas, err := sc.db.Schemas(ctx)
	if err != nil {
		return "", err
	}
	for _, s := range schemas {
		if strings.EqualFold(s.Name, name) {
			return buildSchemaScript(s, sc.opts), nil
		}
	}
	return "", notFoundf("gosmo: schema %s not found", quoteIdent(name))
}

// buildSchemaScript assembles one schema's script. CREATE SCHEMA has to be
// the first statement of its batch, so its existence guard is a dynamic EXEC
// rather than an IF wrapping the statement itself.
func buildSchemaScript(s *Schema, opts ScriptOptions) string {
	var sb strings.Builder
	if v := opts.verb(); v == ScriptDrop || v == ScriptDropAndCreate {
		fmt.Fprintf(&sb, "DROP SCHEMA IF EXISTS %s;\nGO\n", quoteIdent(s.Name))
		if v == ScriptDrop {
			return sb.String()
		}
		sb.WriteString("\n")
	}
	stmt := "CREATE SCHEMA " + quoteIdent(s.Name)
	if s.Owner != "" {
		stmt += " AUTHORIZATION " + quoteIdent(s.Owner)
	}
	if opts.IncludeIfNotExists {
		fmt.Fprintf(&sb, "IF SCHEMA_ID(N'%s') IS NULL\n    EXEC(N'%s');\nGO\n",
			escapeSingle(s.Name), escapeSingle(stmt))
		return sb.String()
	}
	fmt.Fprintf(&sb, "%s;\nGO\n", stmt)
	return sb.String()
}

// ScriptUser generates the CREATE (or DROP) script for one database user.
func (sc *Scripter) ScriptUser(ctx context.Context, name string) (string, error) {
	u, err := sc.db.UserByName(ctx, name)
	if err != nil {
		return "", err
	}
	return buildUserScript(u, sc.opts), nil
}

// buildUserScript assembles one database user's script.
//
// Which CREATE USER form applies is decided by AuthType, not by UserType: a
// user created FOR LOGIN whose login was later dropped still reports
// AuthType "INSTANCE" with an empty LoginName, and scripting that as a
// contained user would silently produce a different kind of principal. A
// contained user's password is not in the catalog and cannot be scripted —
// the placeholder is left for the operator to fill in, exactly as SSMS does.
func buildUserScript(u *User, opts ScriptOptions) string {
	var sb strings.Builder
	if v := opts.verb(); v == ScriptDrop || v == ScriptDropAndCreate {
		fmt.Fprintf(&sb, "DROP USER IF EXISTS %s;\nGO\n", quoteIdent(u.Name))
		if v == ScriptDrop {
			return sb.String()
		}
		sb.WriteString("\n")
	}
	if opts.IncludeIfNotExists {
		fmt.Fprintf(&sb, "IF DATABASE_PRINCIPAL_ID(N'%s') IS NULL\n", escapeSingle(u.Name))
	}
	stmt := "CREATE USER " + quoteIdent(u.Name)
	switch {
	case strings.EqualFold(u.AuthType, "DATABASE"):
		stmt += " WITH PASSWORD = N'<password, sysname, >'"
	case strings.EqualFold(u.AuthType, "EXTERNAL"):
		stmt += " FROM EXTERNAL PROVIDER"
	case u.LoginName != "":
		stmt += " FOR LOGIN " + quoteIdent(u.LoginName)
	case strings.EqualFold(u.AuthType, "INSTANCE"):
		// Orphaned: the login it was made for is gone, so its own name is
		// the only candidate left, and the script says so rather than
		// quietly turning it into a login-less user.
		stmt += " FOR LOGIN " + quoteIdent(u.Name)
	default:
		stmt += " WITHOUT LOGIN"
	}
	if u.DefaultSchema != "" {
		if strings.Contains(stmt, " WITH ") {
			stmt += ", DEFAULT_SCHEMA = " + quoteIdent(u.DefaultSchema)
		} else {
			stmt += " WITH DEFAULT_SCHEMA = " + quoteIdent(u.DefaultSchema)
		}
	}
	fmt.Fprintf(&sb, "%s;\nGO\n", stmt)
	return sb.String()
}

// ScriptDatabaseRole generates the CREATE (or DROP) script for one database
// role, including the ALTER ROLE statements that restore its membership.
func (sc *Scripter) ScriptDatabaseRole(ctx context.Context, name string) (string, error) {
	roles, err := sc.db.DatabaseRoles(ctx)
	if err != nil {
		return "", err
	}
	for _, r := range roles {
		if strings.EqualFold(r.Name, name) {
			return buildDatabaseRoleScript(r, sc.opts), nil
		}
	}
	return "", notFoundf("gosmo: database role %s not found", quoteIdent(name))
}

// buildDatabaseRoleScript assembles one database role's script.
func buildDatabaseRoleScript(r *DatabaseRole, opts ScriptOptions) string {
	var sb strings.Builder
	if v := opts.verb(); v == ScriptDrop || v == ScriptDropAndCreate {
		fmt.Fprintf(&sb, "DROP ROLE IF EXISTS %s;\nGO\n", quoteIdent(r.Name))
		if v == ScriptDrop {
			return sb.String()
		}
		sb.WriteString("\n")
	}
	if opts.IncludeIfNotExists {
		fmt.Fprintf(&sb, "IF DATABASE_PRINCIPAL_ID(N'%s') IS NULL\n", escapeSingle(r.Name))
	}
	fmt.Fprintf(&sb, "CREATE ROLE %s", quoteIdent(r.Name))
	if r.Owner != "" {
		fmt.Fprintf(&sb, " AUTHORIZATION %s", quoteIdent(r.Owner))
	}
	sb.WriteString(";\nGO\n")
	for _, m := range r.Members {
		fmt.Fprintf(&sb, "ALTER ROLE %s ADD MEMBER %s;\nGO\n", quoteIdent(r.Name), quoteIdent(m))
	}
	return sb.String()
}

// ScriptDatabaseAuditSpecification generates the CREATE (or DROP) script for
// one database audit specification.
func (sc *Scripter) ScriptDatabaseAuditSpecification(ctx context.Context, name string) (string, error) {
	spec, err := sc.db.DatabaseAuditSpecificationByName(ctx, name)
	if err != nil {
		return "", err
	}
	return buildDatabaseAuditSpecificationScript(spec, sc.opts)
}

// buildDatabaseAuditSpecificationScript assembles one specification's script.
//
// An orphaned specification — one whose audit has been dropped out from under
// it, which SQL Server allows — is refused rather than scripted with an empty
// FOR SERVER AUDIT clause, which would not parse. Same as the server half.
func buildDatabaseAuditSpecificationScript(s *DatabaseAuditSpecification, opts ScriptOptions) (string, error) {
	var sb strings.Builder
	if v := opts.verb(); v == ScriptDrop || v == ScriptDropAndCreate {
		fmt.Fprintf(&sb, "IF EXISTS (SELECT 1 FROM sys.database_audit_specifications WHERE name = N'%s')\nBEGIN\n"+
			"    ALTER DATABASE AUDIT SPECIFICATION %s WITH ( STATE = OFF );\n"+
			"    DROP DATABASE AUDIT SPECIFICATION %s;\nEND\nGO\n",
			escapeSingle(s.Name), quoteIdent(s.Name), quoteIdent(s.Name))
		if v == ScriptDrop {
			return sb.String(), nil
		}
		sb.WriteString("\n")
	}

	if s.AuditName == "" {
		return "", fmt.Errorf("gosmo: script database audit specification %q: it names no audit", s.Name)
	}
	create, err := DatabaseAuditSpecificationSpec{
		Name:         s.Name,
		AuditName:    s.AuditName,
		ActionGroups: s.ActionGroups,
		Actions:      s.Actions,
		Enabled:      s.IsEnabled,
	}.createStatement()
	if err != nil {
		return "", fmt.Errorf("gosmo: script database audit specification %q: %w", s.Name, err)
	}

	if opts.IncludeIfNotExists {
		fmt.Fprintf(&sb, "IF NOT EXISTS (SELECT 1 FROM sys.database_audit_specifications WHERE name = N'%s')\nBEGIN\n%s\nEND\nGO\n",
			escapeSingle(s.Name), create)
	} else {
		sb.WriteString(create)
		sb.WriteString("\nGO\n")
	}
	return sb.String(), nil
}

// ScriptDatabaseScopedCredential generates the CREATE (or DROP) script for
// one database-scoped credential.
func (sc *Scripter) ScriptDatabaseScopedCredential(ctx context.Context, name string) (string, error) {
	c, err := sc.db.DatabaseScopedCredentialByName(ctx, name)
	if err != nil {
		return "", err
	}
	return buildDatabaseScopedCredentialScript(c, sc.opts), nil
}

// buildDatabaseScopedCredentialScript assembles one database-scoped
// credential's script.
//
// The secret is not readable through any catalog view, so the script carries
// credentialSecretPlaceholder and says so — the same treatment, and for the
// same reason, as buildCredentialScript's. Emitting no SECRET clause at all
// would produce a script that silently creates the credential without one.
// DROP DATABASE SCOPED CREDENTIAL has no IF EXISTS form, so the drop is
// guarded with a sys.database_scoped_credentials lookup.
func buildDatabaseScopedCredentialScript(c *DatabaseScopedCredential, opts ScriptOptions) string {
	var sb strings.Builder
	if v := opts.verb(); v == ScriptDrop || v == ScriptDropAndCreate {
		fmt.Fprintf(&sb, "IF EXISTS (SELECT 1 FROM sys.database_scoped_credentials WHERE name = N'%s')\n"+
			"    DROP DATABASE SCOPED CREDENTIAL %s;\nGO\n",
			escapeSingle(c.Name), quoteIdent(c.Name))
		if v == ScriptDrop {
			return sb.String()
		}
		sb.WriteString("\n")
	}
	sb.WriteString("/* The credential's secret cannot be read from the server. Replace the\n" +
		"   placeholder below, or remove the SECRET clause if it has none. */\n")
	if opts.IncludeIfNotExists {
		fmt.Fprintf(&sb, "IF NOT EXISTS (SELECT 1 FROM sys.database_scoped_credentials WHERE name = N'%s')\n",
			escapeSingle(c.Name))
	}
	fmt.Fprintf(&sb, "CREATE DATABASE SCOPED CREDENTIAL %s WITH IDENTITY = N'%s', SECRET = N'%s';\nGO\n",
		quoteIdent(c.Name), escapeSingle(c.Identity), credentialSecretPlaceholder)
	return sb.String()
}

// ============================================================
// Scripter — certificates and keys
// ============================================================

// ScriptCertificate generates the CREATE (or DROP) script for one
// certificate.
//
// CREATE reads the public certificate with CERTENCODED and emits it as FROM
// BINARY, which recreates the same certificate — same thumbprint, subject,
// issuer, serial number and validity — on every supported version. The
// private key cannot be read back, so a script run elsewhere yields a
// certificate that can verify but not sign or decrypt; the script says so.
func (sc *Scripter) ScriptCertificate(ctx context.Context, name string) (string, error) {
	c, err := sc.db.CertificateByName(ctx, name)
	if err != nil {
		return "", err
	}
	var encoded []byte
	if sc.opts.verb() != ScriptDrop {
		if encoded, err = c.Encoded(ctx); err != nil {
			return "", err
		}
	}
	return buildCertificateScript(c, encoded, sc.opts), nil
}

// buildCertificateScript assembles one certificate's script from the catalog
// row and its CERTENCODED bytes (unused for a DROP-only script). DROP
// CERTIFICATE has no IF EXISTS form, so the drop is guarded with a
// sys.certificates lookup.
func buildCertificateScript(c *Certificate, encoded []byte, opts ScriptOptions) string {
	var sb strings.Builder
	if v := opts.verb(); v == ScriptDrop || v == ScriptDropAndCreate {
		fmt.Fprintf(&sb, "IF EXISTS (SELECT 1 FROM sys.certificates WHERE name = N'%s')\n"+
			"    DROP CERTIFICATE %s;\nGO\n",
			escapeSingle(c.Name), quoteIdent(c.Name))
		if v == ScriptDrop {
			return sb.String()
		}
		sb.WriteString("\n")
	}
	if c.HasPrivateKey() {
		sb.WriteString("/* The certificate's private key cannot be read from the server, so it is\n" +
			"   not scripted: this recreates the public certificate only, which can\n" +
			"   verify signatures and encrypt, but not sign or decrypt. */\n")
	}
	if opts.IncludeIfNotExists {
		fmt.Fprintf(&sb, "IF NOT EXISTS (SELECT 1 FROM sys.certificates WHERE name = N'%s')\n",
			escapeSingle(c.Name))
	}
	sb.WriteString("CREATE CERTIFICATE " + quoteIdent(c.Name))
	if c.Owner != "" {
		sb.WriteString(" AUTHORIZATION " + quoteIdent(c.Owner))
	}
	sb.WriteString("\n    FROM BINARY = " + binaryLiteral(encoded))
	// ON is the default, so only a certificate switched off says anything.
	if !c.IsActiveForBeginDialog {
		sb.WriteString("\n    ACTIVE FOR BEGIN_DIALOG = OFF")
	}
	sb.WriteString(";\nGO\n")
	return sb.String()
}

// ScriptAsymmetricKey generates the CREATE (or DROP) script for one
// asymmetric key.
//
// Neither half of an asymmetric key can be scripted back into existence:
// CREATE ASYMMETRIC KEY has no FROM BINARY form, only imports that read the
// server's filesystem or an EKM provider. So CREATE is the generated form with
// the key's algorithm and owner, and says the result is a new key pair.
func (sc *Scripter) ScriptAsymmetricKey(ctx context.Context, name string) (string, error) {
	k, err := sc.db.AsymmetricKeyByName(ctx, name)
	if err != nil {
		return "", err
	}
	return buildAsymmetricKeyScript(k, sc.opts), nil
}

// keyPasswordPlaceholder stands in for a key's protecting password in a
// generated script: no catalog view exposes it, and a script that silently
// switched the key to master-key protection would be wrong in a way nobody
// sees until the master key is missing.
const keyPasswordPlaceholder = "<insert password here>"

// buildAsymmetricKeyScript assembles one asymmetric key's script. DROP
// ASYMMETRIC KEY has no IF EXISTS form, so the drop is guarded with a
// sys.asymmetric_keys lookup.
func buildAsymmetricKeyScript(k *AsymmetricKey, opts ScriptOptions) string {
	var sb strings.Builder
	if v := opts.verb(); v == ScriptDrop || v == ScriptDropAndCreate {
		fmt.Fprintf(&sb, "IF EXISTS (SELECT 1 FROM sys.asymmetric_keys WHERE name = N'%s')\n"+
			"    DROP ASYMMETRIC KEY %s;\nGO\n",
			escapeSingle(k.Name), quoteIdent(k.Name))
		if v == ScriptDrop {
			return sb.String()
		}
		sb.WriteString("\n")
	}
	sb.WriteString("/* An asymmetric key cannot be recreated from what the server exposes, so\n" +
		"   this creates a NEW key pair with the same algorithm, not this key: what\n" +
		"   the original signed or encrypted will not verify or decrypt with it.")
	if !k.HasPrivateKey() {
		sb.WriteString("\n   The original holds only a public key; the result has a private key too.")
	}
	if k.ProviderType != "" {
		sb.WriteString("\n   The original is held by an EKM provider (FROM PROVIDER); the result is not.")
	}
	sb.WriteString(" */\n")
	if opts.IncludeIfNotExists {
		fmt.Fprintf(&sb, "IF NOT EXISTS (SELECT 1 FROM sys.asymmetric_keys WHERE name = N'%s')\n",
			escapeSingle(k.Name))
	}
	sb.WriteString("CREATE ASYMMETRIC KEY " + quoteIdent(k.Name))
	if k.Owner != "" {
		sb.WriteString(" AUTHORIZATION " + quoteIdent(k.Owner))
	}
	alg := k.Algorithm
	if !AsymmetricKeyAlgorithm(alg).valid() {
		// An EKM key's algorithm can be one CREATE ... WITH ALGORITHM does
		// not take, or none; leave the choice to the script's reader.
		alg = "<algorithm>"
	}
	sb.WriteString("\n    WITH ALGORITHM = " + alg)
	if k.PvtKeyEncryptionType == "ENCRYPTED_BY_PASSWORD" {
		sb.WriteString("\n    ENCRYPTION BY PASSWORD = N'" + keyPasswordPlaceholder + "'")
	}
	sb.WriteString(";\nGO\n")
	return sb.String()
}

// ScriptSymmetricKey generates the CREATE (or DROP) script for one symmetric
// key.
//
// A symmetric key's material cannot be read back, and neither can the
// KEY_SOURCE and IDENTITY_VALUE that would regenerate it. So CREATE carries
// the algorithm, owner and every ENCRYPTION BY the key has now — passwords
// as placeholders — and says the result is a new key.
func (sc *Scripter) ScriptSymmetricKey(ctx context.Context, name string) (string, error) {
	k, err := sc.db.SymmetricKeyByName(ctx, name)
	if err != nil {
		return "", err
	}
	return buildSymmetricKeyScript(k, sc.opts), nil
}

// buildSymmetricKeyScript assembles one symmetric key's script. DROP
// SYMMETRIC KEY has no IF EXISTS form, so the drop is guarded with a
// sys.symmetric_keys lookup.
func buildSymmetricKeyScript(k *SymmetricKey, opts ScriptOptions) string {
	var sb strings.Builder
	if v := opts.verb(); v == ScriptDrop || v == ScriptDropAndCreate {
		fmt.Fprintf(&sb, "IF EXISTS (SELECT 1 FROM sys.symmetric_keys WHERE name = N'%s')\n"+
			"    DROP SYMMETRIC KEY %s;\nGO\n",
			escapeSingle(k.Name), quoteIdent(k.Name))
		if v == ScriptDrop {
			return sb.String()
		}
		sb.WriteString("\n")
	}

	// Each encryptor becomes one ENCRYPTION BY item. An encryptor the reader
	// cannot see, or a kind a symmetric key cannot be created with (MASTER
	// KEY, an unrecognised one), is left as a placeholder rather than dropped:
	// a script that silently lost a way to open the key would be wrong in a
	// way nobody sees until that way is needed.
	var items, parents []string
	for _, e := range k.Encryptions {
		switch e.Kind {
		case SymmetricKeyByPassword:
			items = append(items, "PASSWORD = N'"+keyPasswordPlaceholder+"'")
		case SymmetricKeyByCertificate, SymmetricKeyByAsymmetricKey, SymmetricKeyBySymmetricKey:
			n := "<" + strings.ToLower(string(e.Kind)) + " name>"
			if e.Name != "" {
				n = quoteIdent(e.Name)
			}
			items = append(items, string(e.Kind)+" "+n)
			if e.Kind == SymmetricKeyBySymmetricKey {
				parents = append(parents, n)
			}
		default:
			items = append(items, "<"+e.CryptTypeDesc+">")
		}
	}

	sb.WriteString("/* A symmetric key's material cannot be read from the server, so this\n" +
		"   creates a NEW key with the same algorithm and encryptions, not this key:\n" +
		"   data encrypted with the original will not decrypt with it. Only a key\n" +
		"   created with KEY_SOURCE and IDENTITY_VALUE can be recreated, by adding\n" +
		"   both to the WITH clause with their original values.")
	if len(k.Encryptions) == 0 && k.ProviderType == "" {
		sb.WriteString("\n   No encryption of the original could be read; the one below is a placeholder.")
		items = append(items, "PASSWORD = N'"+keyPasswordPlaceholder+"'")
	}
	for _, p := range parents {
		sb.WriteString("\n   " + p + " must be open in this session first: OPEN SYMMETRIC KEY " + p +
			" DECRYPTION BY <decryptor>.")
	}
	if k.ProviderType != "" {
		sb.WriteString("\n   The original is held by an EKM provider (FROM PROVIDER); the result is not.")
	}
	sb.WriteString(" */\n")

	if opts.IncludeIfNotExists {
		fmt.Fprintf(&sb, "IF NOT EXISTS (SELECT 1 FROM sys.symmetric_keys WHERE name = N'%s')\n",
			escapeSingle(k.Name))
	}
	sb.WriteString("CREATE SYMMETRIC KEY " + quoteIdent(k.Name))
	if k.Owner != "" {
		sb.WriteString(" AUTHORIZATION " + quoteIdent(k.Owner))
	}
	alg := k.Algorithm
	if !SymmetricKeyAlgorithm(alg).valid() {
		alg = "<algorithm>"
	}
	sb.WriteString("\n    WITH ALGORITHM = " + alg)
	if len(items) > 0 {
		sb.WriteString("\n    ENCRYPTION BY " + strings.Join(items, ",\n        "))
	}
	sb.WriteString(";\nGO\n")
	return sb.String()
}

// ============================================================
// Scripter — row-level security and Always Encrypted keys
// ============================================================

// ScriptSecurityPolicy generates the CREATE (or DROP) script for one
// row-level security policy.
func (sc *Scripter) ScriptSecurityPolicy(ctx context.Context, schema, name string) (string, error) {
	policies, err := sc.db.SecurityPolicies(ctx)
	if err != nil {
		return "", err
	}
	for _, p := range policies {
		if strings.EqualFold(p.Name, name) && (schema == "" || strings.EqualFold(p.Schema, schema)) {
			return buildSecurityPolicyScript(p, sc.opts), nil
		}
	}
	return "", notFoundf("gosmo: security policy %s not found", qualifiedName(schema, name))
}

// buildSecurityPolicyScript assembles one security policy's script. STATE
// carries the policy's current enabled/disabled state: a disabled policy
// recreated as STATE = ON starts filtering rows the original was not.
func buildSecurityPolicyScript(p *SecurityPolicy, opts ScriptOptions) string {
	full := qualifiedName(p.Schema, p.Name)
	var sb strings.Builder
	if v := opts.verb(); v == ScriptDrop || v == ScriptDropAndCreate {
		fmt.Fprintf(&sb, "DROP SECURITY POLICY IF EXISTS %s;\nGO\n", full)
		if v == ScriptDrop {
			return sb.String()
		}
		sb.WriteString("\n")
	}
	if opts.IncludeIfNotExists {
		fmt.Fprintf(&sb, "IF OBJECT_ID(N'%s', N'SP') IS NULL\n", escapeSingle(full))
	}
	fmt.Fprintf(&sb, "CREATE SECURITY POLICY %s", full)
	for i, pred := range p.Predicates {
		sep := "\n    ADD "
		if i > 0 {
			sb.WriteString(",")
		}
		fmt.Fprintf(&sb, "%s%s PREDICATE %s ON %s", sep, pred.PredicateType,
			unwrapPredicate(pred.PredicateDefinition), qualifiedName(pred.TargetSchema, pred.TargetTable))
		if pred.Operation != "" {
			fmt.Fprintf(&sb, " %s", pred.Operation)
		}
	}
	state := "OFF"
	if p.IsEnabled {
		state = "ON"
	}
	fmt.Fprintf(&sb, "\n    WITH (STATE = %s, SCHEMABINDING = %s)", state, onOff(p.IsSchemaBound))
	if p.IsNotForReplication {
		sb.WriteString("\n    NOT FOR REPLICATION")
	}
	sb.WriteString(";\nGO\n")
	return sb.String()
}

// ScriptColumnMasterKey generates the CREATE (or DROP) script for one
// Always Encrypted column master key.
func (sc *Scripter) ScriptColumnMasterKey(ctx context.Context, name string) (string, error) {
	keys, err := sc.db.ColumnMasterKeys(ctx)
	if err != nil {
		return "", err
	}
	for _, k := range keys {
		if strings.EqualFold(k.Name, name) {
			return buildColumnMasterKeyScript(k, sc.opts), nil
		}
	}
	return "", notFoundf("gosmo: column master key %s not found", quoteIdent(name))
}

// buildColumnMasterKeyScript assembles one column master key's script. A key
// that allows enclave computations carries its signature verbatim — the
// server verifies it against the rest of the metadata, and it cannot be
// recomputed here.
func buildColumnMasterKeyScript(k *ColumnMasterKey, opts ScriptOptions) string {
	var sb strings.Builder
	if v := opts.verb(); v == ScriptDrop || v == ScriptDropAndCreate {
		// DROP COLUMN MASTER KEY has no IF EXISTS form.
		fmt.Fprintf(&sb, "IF EXISTS (SELECT 1 FROM sys.column_master_keys WHERE name = N'%s')\n    DROP COLUMN MASTER KEY %s;\nGO\n",
			escapeSingle(k.Name), quoteIdent(k.Name))
		if v == ScriptDrop {
			return sb.String()
		}
		sb.WriteString("\n")
	}
	if opts.IncludeIfNotExists {
		fmt.Fprintf(&sb, "IF NOT EXISTS (SELECT 1 FROM sys.column_master_keys WHERE name = N'%s')\n",
			escapeSingle(k.Name))
	}
	fmt.Fprintf(&sb, "CREATE COLUMN MASTER KEY %s\nWITH (\n    KEY_STORE_PROVIDER_NAME = N'%s',\n    KEY_PATH = N'%s'",
		quoteIdent(k.Name), escapeSingle(k.KeyStoreProviderName), escapeSingle(k.KeyPath))
	if k.AllowEnclaveComputations {
		fmt.Fprintf(&sb, ",\n    ENCLAVE_COMPUTATIONS (SIGNATURE = %s)", binaryLiteral(k.Signature))
	}
	sb.WriteString("\n);\nGO\n")
	return sb.String()
}

// ScriptColumnEncryptionKey generates the CREATE (or DROP) script for one
// Always Encrypted column encryption key.
func (sc *Scripter) ScriptColumnEncryptionKey(ctx context.Context, name string) (string, error) {
	keys, err := sc.db.ColumnEncryptionKeys(ctx)
	if err != nil {
		return "", err
	}
	for _, k := range keys {
		if strings.EqualFold(k.Name, name) {
			return buildColumnEncryptionKeyScript(k, sc.opts), nil
		}
	}
	return "", notFoundf("gosmo: column encryption key %s not found", quoteIdent(name))
}

// buildColumnEncryptionKeyScript assembles one column encryption key's
// script, restating every encrypted value it holds — a key mid-rotation has
// one per master key, and dropping any of them makes the data encrypted
// under it unreadable.
func buildColumnEncryptionKeyScript(k *ColumnEncryptionKey, opts ScriptOptions) string {
	var sb strings.Builder
	if v := opts.verb(); v == ScriptDrop || v == ScriptDropAndCreate {
		// DROP COLUMN ENCRYPTION KEY has no IF EXISTS form either.
		fmt.Fprintf(&sb, "IF EXISTS (SELECT 1 FROM sys.column_encryption_keys WHERE name = N'%s')\n    DROP COLUMN ENCRYPTION KEY %s;\nGO\n",
			escapeSingle(k.Name), quoteIdent(k.Name))
		if v == ScriptDrop {
			return sb.String()
		}
		sb.WriteString("\n")
	}
	if opts.IncludeIfNotExists {
		fmt.Fprintf(&sb, "IF NOT EXISTS (SELECT 1 FROM sys.column_encryption_keys WHERE name = N'%s')\n",
			escapeSingle(k.Name))
	}
	fmt.Fprintf(&sb, "CREATE COLUMN ENCRYPTION KEY %s\nWITH VALUES", quoteIdent(k.Name))
	for i, v := range k.Values {
		if i > 0 {
			sb.WriteString(",")
		}
		fmt.Fprintf(&sb, "\n(\n    COLUMN_MASTER_KEY = %s,\n    ALGORITHM = '%s',\n    ENCRYPTED_VALUE = %s\n)",
			quoteIdent(v.MasterKeyName), escapeSingle(v.EncryptionAlgorithm), binaryLiteral(v.EncryptedValue))
	}
	sb.WriteString(";\nGO\n")
	return sb.String()
}

// unwrapPredicate strips the parentheses sys.security_predicates wraps a
// predicate definition in. ADD FILTER PREDICATE takes a function call and
// nothing else — the catalog's own "([sec].[fn]([col]))" form is rejected
// with "Incorrect syntax near '('", which is only visible by running the
// generated script.
func unwrapPredicate(def string) string {
	def = strings.TrimSpace(def)
	if !strings.HasPrefix(def, "(") || !strings.HasSuffix(def, ")") {
		return def
	}
	// Only when that first "(" is the one the last ")" closes — a definition
	// that merely starts and ends with a paren must be left alone.
	depth := 0
	for i, r := range def {
		switch r {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 && i != len(def)-1 {
				return def
			}
		}
	}
	return strings.TrimSpace(def[1 : len(def)-1])
}

// onOff renders a T-SQL ON/OFF option value.
func onOff(v bool) string {
	if v {
		return "ON"
	}
	return "OFF"
}
