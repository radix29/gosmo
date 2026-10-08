package gosmo

import (
	"context"
	"fmt"
	"slices"
	"strings"
)

// ============================================================
// GRANT/DENY/REVOKE modifiers — WITH GRANT OPTION, CASCADE, and
// GRANT OPTION FOR
// ============================================================

// PermissionOptions carries the GRANT/DENY/REVOKE modifiers ApplyPermission
// takes as its last argument. The zero value renders the plain statement.
//
// The three fields are not independent of each other in practice, because
// SQL Server refuses some sequences outright:
//
//   - A permission granted WITH GRANT OPTION cannot be revoked or denied
//     without CASCADE — "the permission was granted WITH GRANT OPTION" is a
//     hard error, not a warning. Anything that takes such a grant away
//     therefore needs Cascade set.
//   - GrantOptionOnly (REVOKE GRANT OPTION FOR) takes away only the right to
//     re-grant and leaves the underlying GRANT in place. It is the
//     "WITH GRANT OPTION -> plain GRANT" downgrade, and SQL Server requires
//     CASCADE with it as well, so Cascade is implied and need not be set.
//
// CASCADE reaches every principal the grantee granted the permission on to,
// which is the point of it and also why it is opt-in rather than always sent.
type PermissionOptions struct {
	// WithGrantOption appends WITH GRANT OPTION to a GRANT, letting the
	// grantee grant the same permission on to others. GRANT only.
	WithGrantOption bool

	// Cascade appends CASCADE to a DENY or REVOKE, applying it to every
	// principal the grantee passed the permission on to. Required whenever
	// the permission being taken away was granted WITH GRANT OPTION.
	Cascade bool

	// GrantOptionOnly turns a REVOKE into REVOKE GRANT OPTION FOR: the
	// grantee keeps the permission but loses the right to grant it onward.
	// REVOKE only, and always CASCADE.
	GrantOptionOnly bool
}

// ============================================================
// ApplyPermission — one GRANT/DENY/REVOKE on any securable
// ============================================================

// PermissionVerb is the statement ApplyPermission issues.
type PermissionVerb string

const (
	VerbGrant  PermissionVerb = "GRANT"
	VerbDeny   PermissionVerb = "DENY"
	VerbRevoke PermissionVerb = "REVOKE"
)

// SecurableClass is the kind of thing a permission is granted on. It decides
// the ON clause, which names are schema-qualified, and which permission
// names the statement may carry (PermissionNames).
type SecurableClass string

const (
	// SecurableServer and SecurableDatabase name no securable: the
	// permission is on the instance, or on the database the receiver
	// addresses. Server.ApplyPermission takes the first and only it;
	// Database.ApplyPermission takes every other class.
	SecurableServer   SecurableClass = "SERVER"
	SecurableDatabase SecurableClass = "DATABASE"
	SecurableSchema   SecurableClass = "SCHEMA"
	// SecurableTable and SecurableView are the two classes that carry
	// column permissions (Securable.Columns).
	SecurableTable          SecurableClass = "TABLE"
	SecurableView           SecurableClass = "VIEW"
	SecurableProcedure      SecurableClass = "PROCEDURE"
	SecurableScalarFunction SecurableClass = "SCALAR FUNCTION"
	// A table-valued function is queried like a table, so it carries
	// SELECT, not EXECUTE. Only an inline one (sys.objects type IF) also
	// takes INSERT, UPDATE and DELETE, since only it can be the target of
	// one; a multi-statement one (TF) refuses them.
	SecurableInlineFunction SecurableClass = "INLINE FUNCTION"
	SecurableTableFunction  SecurableClass = "TABLE FUNCTION"
	SecurableSequence       SecurableClass = "SEQUENCE"
	SecurableSynonym        SecurableClass = "SYNONYM"
	// SecurableUserType is a user-defined data type or table type
	// (ON TYPE::).
	SecurableUserType    SecurableClass = "TYPE"
	SecurableCertificate SecurableClass = "CERTIFICATE"
)

// Securable identifies what ApplyPermission grants on.
type Securable struct {
	Class SecurableClass
	// Schema is required for every schema-scoped class (tables, views,
	// modules, sequences, synonyms, types) and must be empty for the others.
	Schema string
	// Name is empty for SecurableServer and SecurableDatabase and required
	// for every other class.
	Name string
	// Columns narrows a SecurableTable or SecurableView permission to these
	// columns, rendered as the one statement SQL Server accepts for them —
	// GRANT SELECT (a, b) ON ... Nil means the whole object; any other class
	// refuses a column list.
	Columns []string
}

// PermissionName is a permission name: an ObjectPermission,
// DatabasePermission or ServerPermission. Which names a statement accepts
// depends on the securable's class, not on the Go type — EXECUTE is an
// ObjectPermission constant and a database-scoped name alike.
type PermissionName interface{ permissionName() string }

func (p ObjectPermission) permissionName() string   { return string(p) }
func (p DatabasePermission) permissionName() string { return string(p) }
func (p ServerPermission) permissionName() string   { return string(p) }

// Per-class allowlists for the classes that have no catalog of their own
// elsewhere (objectPermissionNames, schemaPermissionNames,
// databasePermissionNames, serverPermissionNames, columnPermissionNames).
// Each is what SQL Server 2016 and 2025 both accepted when every
// ObjectPermission constant was granted on an object of the class (probed
// live 2026-10-08); a name outside it fails on the server with "Granted or
// revoked privilege X is not compatible with object". The probe and the GRANT
// Object Permissions page disagree in three places, and the server wins: a
// procedure takes REFERENCES, a synonym takes ALTER, and a view refuses VIEW
// CHANGE TRACKING. See serverPermissionNames for why an allowlist rather
// than quoting.
var (
	viewPermissionNames = map[ObjectPermission]bool{
		PermAlter: true, PermControl: true, PermDelete: true,
		PermInsert: true, PermReferences: true, PermSelect: true, PermTakeOwnership: true,
		PermUpdate: true, PermView: true,
	}
	procedurePermissionNames = map[ObjectPermission]bool{
		PermAlter: true, PermControl: true, PermExecute: true, PermReferences: true,
		PermTakeOwnership: true, PermView: true,
	}
	scalarFunctionPermissionNames = map[ObjectPermission]bool{
		PermAlter: true, PermControl: true, PermExecute: true, PermReferences: true,
		PermTakeOwnership: true, PermView: true,
	}
	inlineFunctionPermissionNames = map[ObjectPermission]bool{
		PermAlter: true, PermControl: true, PermDelete: true, PermInsert: true,
		PermReferences: true, PermSelect: true, PermTakeOwnership: true,
		PermUpdate: true, PermView: true,
	}
	tableFunctionPermissionNames = map[ObjectPermission]bool{
		PermAlter: true, PermControl: true, PermReferences: true, PermSelect: true,
		PermTakeOwnership: true, PermView: true,
	}
	sequencePermissionNames = map[ObjectPermission]bool{
		PermAlter: true, PermControl: true, PermReferences: true, PermUpdate: true,
		PermTakeOwnership: true, PermView: true,
	}
	synonymPermissionNames = map[ObjectPermission]bool{
		PermAlter: true, PermControl: true, PermDelete: true, PermExecute: true, PermInsert: true,
		PermSelect: true, PermTakeOwnership: true, PermUpdate: true, PermView: true,
	}
	userTypePermissionNames = map[ObjectPermission]bool{
		PermControl: true, PermExecute: true, PermReferences: true,
		PermTakeOwnership: true, PermView: true,
	}
	certificatePermissionNames = map[ObjectPermission]bool{
		PermAlter: true, PermControl: true, PermReferences: true,
		PermTakeOwnership: true, PermView: true,
	}
)

// allows reports whether name is a permission the class accepts; columns
// selects the column-level list of a table or view.
func (c SecurableClass) allows(name string, columns bool) bool {
	if columns {
		return columnPermissionNames[ObjectPermission(name)]
	}
	switch c {
	case SecurableServer:
		return serverPermissionNames[ServerPermission(name)]
	case SecurableDatabase:
		return databasePermissionNames[DatabasePermission(name)]
	}
	m := c.objectNames()
	return m != nil && m[ObjectPermission(name)]
}

// objectNames is the allowlist of every class below the database; nil for
// SecurableServer, SecurableDatabase and an unknown class.
func (c SecurableClass) objectNames() map[ObjectPermission]bool {
	switch c {
	case SecurableSchema:
		return schemaPermissionNames
	case SecurableTable:
		return objectPermissionNames
	case SecurableView:
		return viewPermissionNames
	case SecurableProcedure:
		return procedurePermissionNames
	case SecurableScalarFunction:
		return scalarFunctionPermissionNames
	case SecurableInlineFunction:
		return inlineFunctionPermissionNames
	case SecurableTableFunction:
		return tableFunctionPermissionNames
	case SecurableSequence:
		return sequencePermissionNames
	case SecurableSynonym:
		return synonymPermissionNames
	case SecurableUserType:
		return userTypePermissionNames
	case SecurableCertificate:
		return certificatePermissionNames
	}
	return nil
}

// PermissionNames returns every permission name GRANT/DENY/REVOKE accepts on
// the class, sorted — the catalog a permissions grid enumerates for it. An
// unknown class has none. Column-level names are ColumnPermissionNames.
func (c SecurableClass) PermissionNames() []string {
	switch c {
	case SecurableServer:
		return ServerPermissionNames()
	case SecurableDatabase:
		return DatabasePermissionNames()
	}
	m := c.objectNames()
	names := make([]string, 0, len(m))
	for name := range m {
		names = append(names, string(name))
	}
	slices.Sort(names)
	return names
}

// schemaScoped reports whether the class's securables live in a schema.
func (c SecurableClass) schemaScoped() bool {
	switch c {
	case SecurableTable, SecurableView, SecurableProcedure, SecurableScalarFunction,
		SecurableInlineFunction, SecurableTableFunction, SecurableSequence, SecurableSynonym, SecurableUserType:
		return true
	}
	return false
}

// on renders the statement's ON clause, empty for a class that names no
// securable. Objects keep the bare two-part form (ON [dbo].[t]) rather than
// OBJECT::, which is what every script before ApplyPermission emitted.
func (s Securable) on() string {
	switch s.Class {
	case SecurableServer, SecurableDatabase:
		return ""
	case SecurableSchema:
		return "ON SCHEMA::" + quoteIdent(s.Name)
	case SecurableUserType:
		return "ON TYPE::" + qualifiedName(s.Schema, s.Name)
	case SecurableCertificate:
		return "ON CERTIFICATE::" + quoteIdent(s.Name)
	}
	return "ON " + qualifiedName(s.Schema, s.Name)
}

// check refuses a request whose verb, securable or permission name the
// statement cannot carry, before anything is rendered.
func (s Securable) check(verb PermissionVerb, perm string) error {
	lower := strings.ToLower(string(verb))
	switch verb {
	case VerbGrant, VerbDeny, VerbRevoke:
	default:
		return invalidf("gosmo: apply permission: unknown verb %q", verb)
	}
	if s.Class.objectNames() == nil && s.Class != SecurableServer && s.Class != SecurableDatabase {
		return invalidf("gosmo: %s permission: unknown securable class %q", lower, s.Class)
	}
	named := s.Class != SecurableServer && s.Class != SecurableDatabase
	switch {
	case named && s.Name == "":
		return invalidf("gosmo: %s permission: a %s securable needs a name", lower, strings.ToLower(string(s.Class)))
	case !named && (s.Name != "" || s.Schema != ""):
		return invalidf("gosmo: %s permission: a %s permission names no securable", lower, strings.ToLower(string(s.Class)))
	case !s.Class.schemaScoped() && s.Schema != "":
		return invalidf("gosmo: %s permission: a %s is not schema-scoped", lower, strings.ToLower(string(s.Class)))
	}
	if s.Class.schemaScoped() {
		if err := requireSchema(lower+" permission", s.Schema, s.Name); err != nil {
			return err
		}
	}
	if s.Columns != nil {
		if s.Class != SecurableTable && s.Class != SecurableView {
			return invalidf("gosmo: %s permission: a %s has no column permissions", lower, strings.ToLower(string(s.Class)))
		}
		if len(s.Columns) == 0 {
			// A caller that meant the whole object says so with nil;
			// silently widening a column grant to the object is the wrong
			// direction to guess in.
			return invalidf("gosmo: %s column permission: no columns named", lower)
		}
		if !s.Class.allows(perm, true) {
			return invalidf("gosmo: %s column permission: %q cannot be granted on a column", lower, perm)
		}
		return nil
	}
	if !s.Class.allows(perm, false) {
		return invalidf("gosmo: %s permission: %q cannot be granted on a %s", lower, perm, strings.ToLower(string(s.Class)))
	}
	return nil
}

// statement validates the request and renders it.
func (s Securable) statement(verb PermissionVerb, perm PermissionName, principal string, opts PermissionOptions) (string, error) {
	if perm == nil {
		return "", invalidf("gosmo: %s permission: no permission named", strings.ToLower(string(verb)))
	}
	name := perm.permissionName()
	if err := s.check(verb, name); err != nil {
		return "", err
	}
	return permissionStmt{
		verb: string(verb), permission: name, columns: s.Columns,
		on: s.on(), principal: principal, opts: opts,
	}.render()
}

// ApplyPermission issues one GRANT, DENY or REVOKE of perm on sec to
// principal. Every class but SecurableServer goes through here; opts adds
// WITH GRANT OPTION, CASCADE or GRANT OPTION FOR, and the zero value is the
// plain statement.
//
// A permission name the class does not accept (EXECUTE on a table, SELECT on
// a procedure), a modifier the verb has no form for, and an incomplete
// securable are refused with ErrInvalidRequest before anything is sent; a
// missing schema with ErrSchemaRequired.
func (d *Database) ApplyPermission(ctx context.Context, verb PermissionVerb, sec Securable, perm PermissionName, principal string, opts PermissionOptions) error {
	if sec.Class == SecurableServer {
		return invalidf("gosmo: %s permission: a server permission goes through Server.ApplyPermission", strings.ToLower(string(verb)))
	}
	q, err := sec.statement(verb, perm, principal, opts)
	if err != nil {
		return err
	}
	if _, err := d.exec(ctx, q); err != nil {
		return fmt.Errorf("gosmo: %s %s %s%s %q in %q: %w", strings.ToLower(string(verb)), perm.permissionName(),
			onText(sec), fromOrTo(verb), principal, d.Name, err)
	}
	return nil
}

// ApplyPermission issues one GRANT, DENY or REVOKE of a server-scoped
// permission to principal; sec.Class must be SecurableServer. See
// Database.ApplyPermission for opts and the refusals.
//
// SQL Server rejects GRANT/DENY/REVOKE at server scope outright unless the
// session's current database is master ("Permissions at the server scope can
// only be granted when the current database is master") — its own
// restriction, not one gosmo imposes — so every statement here is prefixed
// with USE master in the same batch.
//
// That USE does not leak into whatever borrows the connection next, and the
// reason is the driver, not this package: USE is session state and would
// otherwise survive the connection's return to the pool. database/sql calls
// driver.SessionResetter.ResetSession before handing a pooled connection to
// its next user, and go-mssqldb implements it by flagging the next TDS batch
// as a connection reset (Conn.ResetSession -> sendSqlBatch72's resetSession),
// which restores the session's database to the connection string's.
//
// Verified live 2026-08-01, A/B against a connection opened with
// Database set: eight pooled connections all still reported that database
// after a GRANT. Recorded because the shape of this code invites the
// opposite conclusion — a review that session proposed replacing it with a
// pinned connection that reads DB_NAME(), switches, and switches back, which
// is three extra round trips per grant to re-solve what the driver already
// handles.
func (s *Server) ApplyPermission(ctx context.Context, verb PermissionVerb, sec Securable, perm PermissionName, principal string, opts PermissionOptions) error {
	if sec.Class != SecurableServer {
		return invalidf("gosmo: %s permission: a %s permission goes through Database.ApplyPermission",
			strings.ToLower(string(verb)), strings.ToLower(string(sec.Class)))
	}
	stmt, err := sec.statement(verb, perm, principal, opts)
	if err != nil {
		return err
	}
	err = s.exec(ctx, "USE master; "+stmt)
	if t := txFrom(ctx, s); t != nil {
		// No pool reset inside a transaction: the session stays in master,
		// which the transaction's USE tracking has to hear of.
		t.lost()
	}
	if err != nil {
		return fmt.Errorf("gosmo: %s %s %s %q: %w", strings.ToLower(string(verb)), perm.permissionName(), fromOrTo(verb), principal, err)
	}
	return nil
}

// onText is the securable as an error message names it — "on [dbo].[t] ",
// with its trailing space — or nothing for a database permission.
func onText(sec Securable) string {
	on := sec.on()
	if on == "" {
		return ""
	}
	if len(sec.Columns) > 0 {
		cols := make([]string, len(sec.Columns))
		for i, c := range sec.Columns {
			cols[i] = quoteIdent(c)
		}
		on += " (" + strings.Join(cols, ", ") + ")"
	}
	return "on" + on[2:] + " "
}

// fromOrTo picks the preposition a verb's error message reads with.
func fromOrTo(verb PermissionVerb) string {
	if verb == VerbRevoke {
		return "from"
	}
	return "to"
}

// permissionStmt is one rendered GRANT/DENY/REVOKE, shared by every scope so
// the modifier placement is decided in one place. on is the full "ON ..."
// clause ("ON [dbo].[t]", "ON SCHEMA::[s]") or empty for database- and
// server-scoped permissions, which name no securable.
type permissionStmt struct {
	verb       string // "GRANT", "DENY", "REVOKE"
	permission string
	columns    []string // column-level permission; object scope only
	on         string
	principal  string
	opts       PermissionOptions
}

// render builds the statement, rejecting a modifier the verb has no form
// for rather than quietly dropping it — a caller that asks for WITH GRANT
// OPTION on a DENY has a bug, and a silently plain DENY hides it.
func (p permissionStmt) render() (string, error) {
	o := p.opts
	if o.WithGrantOption && p.verb != "GRANT" {
		return "", invalidf("gosmo: %s: WITH GRANT OPTION applies to GRANT only", strings.ToLower(p.verb))
	}
	if o.GrantOptionOnly && p.verb != "REVOKE" {
		return "", invalidf("gosmo: %s: GRANT OPTION FOR applies to REVOKE only", strings.ToLower(p.verb))
	}
	if o.Cascade && p.verb == "GRANT" {
		return "", invalidf("gosmo: grant: CASCADE applies to DENY and REVOKE only")
	}

	var b strings.Builder
	b.WriteString(p.verb)
	b.WriteByte(' ')
	if o.GrantOptionOnly {
		b.WriteString("GRANT OPTION FOR ")
	}
	b.WriteString(p.permission)
	if len(p.columns) > 0 {
		b.WriteString(" (")
		for i, c := range p.columns {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(quoteIdent(c))
		}
		b.WriteByte(')')
	}
	if p.on != "" {
		b.WriteByte(' ')
		b.WriteString(p.on)
	}
	if p.verb == "REVOKE" {
		b.WriteString(" FROM ")
	} else {
		b.WriteString(" TO ")
	}
	b.WriteString(quoteIdent(p.principal))
	switch {
	case o.WithGrantOption:
		b.WriteString(" WITH GRANT OPTION")
	case o.Cascade || o.GrantOptionOnly:
		b.WriteString(" CASCADE")
	}
	return b.String(), nil
}
