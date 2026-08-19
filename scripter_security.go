package gosmo

import (
	"context"
	"encoding/hex"
	"fmt"
	"strings"
)

// ============================================================
// Scripter — database principals (schema, user, role)
// ============================================================

// ScriptSchema generates the CREATE (or DROP) script for one schema.
func (sc *Scripter) ScriptSchema(name string) (string, error) {
	return sc.ScriptSchemaContext(context.Background(), name)
}

// ScriptSchemaContext is the context-aware variant of ScriptSchema.
func (sc *Scripter) ScriptSchemaContext(ctx context.Context, name string) (string, error) {
	schemas, err := sc.db.SchemasContext(ctx)
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
func (sc *Scripter) ScriptUser(name string) (string, error) {
	return sc.ScriptUserContext(context.Background(), name)
}

// ScriptUserContext is the context-aware variant of ScriptUser.
func (sc *Scripter) ScriptUserContext(ctx context.Context, name string) (string, error) {
	u, err := sc.db.UserByNameContext(ctx, name)
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
func (sc *Scripter) ScriptDatabaseRole(name string) (string, error) {
	return sc.ScriptDatabaseRoleContext(context.Background(), name)
}

// ScriptDatabaseRoleContext is the context-aware variant.
func (sc *Scripter) ScriptDatabaseRoleContext(ctx context.Context, name string) (string, error) {
	roles, err := sc.db.DatabaseRolesContext(ctx)
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

// ============================================================
// Scripter — row-level security and Always Encrypted keys
// ============================================================

// ScriptSecurityPolicy generates the CREATE (or DROP) script for one
// row-level security policy.
func (sc *Scripter) ScriptSecurityPolicy(schema, name string) (string, error) {
	return sc.ScriptSecurityPolicyContext(context.Background(), schema, name)
}

// ScriptSecurityPolicyContext is the context-aware variant of
// ScriptSecurityPolicy.
func (sc *Scripter) ScriptSecurityPolicyContext(ctx context.Context, schema, name string) (string, error) {
	policies, err := sc.db.SecurityPoliciesContext(ctx)
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
func (sc *Scripter) ScriptColumnMasterKey(name string) (string, error) {
	return sc.ScriptColumnMasterKeyContext(context.Background(), name)
}

// ScriptColumnMasterKeyContext is the context-aware variant of
// ScriptColumnMasterKey.
func (sc *Scripter) ScriptColumnMasterKeyContext(ctx context.Context, name string) (string, error) {
	keys, err := sc.db.ColumnMasterKeysContext(ctx)
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
		fmt.Fprintf(&sb, ",\n    ENCLAVE_COMPUTATIONS (SIGNATURE = %s)", hexLiteral(k.Signature))
	}
	sb.WriteString("\n);\nGO\n")
	return sb.String()
}

// ScriptColumnEncryptionKey generates the CREATE (or DROP) script for one
// Always Encrypted column encryption key.
func (sc *Scripter) ScriptColumnEncryptionKey(name string) (string, error) {
	return sc.ScriptColumnEncryptionKeyContext(context.Background(), name)
}

// ScriptColumnEncryptionKeyContext is the context-aware variant of
// ScriptColumnEncryptionKey.
func (sc *Scripter) ScriptColumnEncryptionKeyContext(ctx context.Context, name string) (string, error) {
	keys, err := sc.db.ColumnEncryptionKeysContext(ctx)
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
			quoteIdent(v.MasterKeyName), escapeSingle(v.EncryptionAlgorithm), hexLiteral(v.EncryptedValue))
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

// hexLiteral renders bytes as the 0x… binary literal T-SQL takes.
func hexLiteral(b []byte) string {
	if len(b) == 0 {
		return "0x"
	}
	return "0x" + strings.ToUpper(hex.EncodeToString(b))
}
