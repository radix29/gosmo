package gosmo

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// CapabilityState is the answer to "does the connected login hold this
// permission?" — the three-way answer HAS_PERMS_BY_NAME actually gives.
//
// The third state is not padding. HAS_PERMS_BY_NAME returns NULL, without
// raising an error, for a permission name the instance does not define, so a
// permission introduced in a later version reads as CapabilityUnknown on an
// older one rather than as denied. A caller that folds Unknown into Denied
// hides a feature on every instance that has it under a different name.
type CapabilityState int

const (
	// CapabilityUnknown means the answer is not available: the permission is
	// not one this instance defines, or it was never probed, or the probe
	// itself failed. It is not a denial — see Capabilities.Allows.
	CapabilityUnknown CapabilityState = iota

	// CapabilityGranted means the login holds the permission, whether
	// directly, through a role, or through a wider permission that implies
	// it.
	CapabilityGranted

	// CapabilityDenied means the instance was asked and said no.
	CapabilityDenied
)

func (s CapabilityState) String() string {
	switch s {
	case CapabilityGranted:
		return "granted"
	case CapabilityDenied:
		return "denied"
	default:
		return "unknown"
	}
}

// ProbedServerRoles are the fixed server roles Capabilities probes.
//
// Membership in sysadmin does not imply membership in any other role, so a
// caller testing for a role must test for sysadmin too — see
// Capabilities.InServerRole.
var ProbedServerRoles = []string{
	"sysadmin",
	"serveradmin",
	"securityadmin",
	"processadmin",
	"setupadmin",
	"bulkadmin",
	"diskadmin",
	"dbcreator",
	"public",
}

// ProbedServerPermissions are the server-scope permissions Capabilities
// probes: the handful the application layer gates on, not the whole grantable
// catalog ServerPermissionNames returns. Every name here was checked against a
// live instance; a name with a typo would report CapabilityUnknown forever
// rather than failing.
//
// VIEW SERVER PERFORMANCE STATE and VIEW SERVER SECURITY STATE are the two
// narrower rights SQL Server 2022 split VIEW SERVER STATE into, and are what a
// modern instance names in its denial. All three are probed because holding
// the wide one grants both narrow ones, but not the reverse.
var ProbedServerPermissions = []string{
	"CONTROL SERVER",
	"ALTER SETTINGS",
	"SHUTDOWN",
	"ALTER TRACE",
	"ADMINISTER BULK OPERATIONS",
	"VIEW ANY DATABASE",
	"VIEW ANY DEFINITION",
	"VIEW SERVER STATE",
	"VIEW SERVER PERFORMANCE STATE",
	"VIEW SERVER SECURITY STATE",
	"CREATE ANY DATABASE",
	"ALTER ANY DATABASE",
	"ALTER ANY LOGIN",
	"ALTER ANY SERVER ROLE",
	"ALTER ANY CONNECTION",
	"ALTER ANY CREDENTIAL",
	"ALTER ANY ENDPOINT",
	"ALTER ANY LINKED SERVER",
	"ALTER ANY EVENT SESSION",
	"ALTER ANY AVAILABILITY GROUP",
}

// ProbedDatabaseRoles are the fixed database roles DatabaseCapabilities probes.
//
// The three SQLAgent* roles exist only in msdb; elsewhere IS_ROLEMEMBER
// returns NULL for them and they read as false. They are probed for every
// database rather than only msdb because doing so costs nothing and keeps one
// code path.
var ProbedDatabaseRoles = []string{
	"db_owner",
	"db_securityadmin",
	"db_accessadmin",
	"db_backupoperator",
	"db_ddladmin",
	"db_datawriter",
	"db_datareader",
	"db_denydatawriter",
	"db_denydatareader",
	"SQLAgentUserRole",
	"SQLAgentReaderRole",
	"SQLAgentOperatorRole",
}

// ProbedDatabasePermissions are the database-scope permissions
// DatabaseCapabilities probes — again a working subset, not the grantable
// catalog DatabasePermissionNames returns.
//
// These are checked against the DATABASE securable class, not the server:
// HAS_PERMS_BY_NAME(NULL, NULL, 'ALTER') asks about the *server* and answers
// NULL, which is how a database-scope name mistakenly probed at server scope
// disappears without an error.
var ProbedDatabasePermissions = []string{
	"CONTROL",
	"ALTER",
	"VIEW DEFINITION",
	"VIEW DATABASE STATE",
	"BACKUP DATABASE",
	"BACKUP LOG",
	"CREATE TABLE",
	"CREATE VIEW",
	"CREATE PROCEDURE",
	"CREATE FUNCTION",
	"CREATE SCHEMA",
	"ALTER ANY USER",
	"ALTER ANY ROLE",
	"ALTER ANY SCHEMA",
	"ALTER ANY DATASPACE",
	"ALTER ANY COLUMN MASTER KEY",
	"ALTER ANY COLUMN ENCRYPTION KEY",
	"ALTER ANY SECURITY POLICY",
	"SELECT",
	"INSERT",
	"UPDATE",
	"DELETE",
	"EXECUTE",
	"SHOWPLAN",
}

// ProbedSchemaPermissions are the SCHEMA-scope permissions
// DatabaseCapabilities probes, once per schema in the database.
//
// One name is enough: HAS_PERMS_BY_NAME folds in the permissions that imply
// the one it is asked about, so a principal holding CONTROL on the schema, or
// ALTER ANY SCHEMA, or db_owner, answers 1 for ALTER without any of them being
// asked separately.
var ProbedSchemaPermissions = []string{
	"ALTER",
}

// ProbedObjectPermissions are the OBJECT-scope permissions
// DatabaseCapabilities probes, for every object the login has been granted one
// on or owns outright.
//
// This block is read out of the catalog rather than asked with
// HAS_PERMS_BY_NAME, which answers for one securable per call and so would
// cost a query per object. The consequence is that it reports only what is
// *explicit*: an object carrying no grant and no distinct owner has no row at
// all, which is why the answer is additive — see HasOnObject.
var ProbedObjectPermissions = []string{
	"ALTER",
}

// Capabilities is what the connected login may do at the server scope: its
// fixed-server-role memberships and the state of each permission in
// ProbedServerPermissions.
//
// Obtain one with Server.Capabilities. Every method is nil-safe, so a caller
// that could not probe — or chose not to — can hold a nil *Capabilities and
// still ask questions of it.
type Capabilities struct {
	// ServerRoles maps each name in ProbedServerRoles to membership.
	ServerRoles map[string]bool

	// ServerPermissions maps each name in ProbedServerPermissions to its state.
	ServerPermissions map[string]CapabilityState
}

// InServerRole reports whether the login is a member of the named fixed server
// role.
//
// sysadmin is *not* folded in, because SQL Server does not fold it in either:
// IS_SRVROLEMEMBER('SQLAgentUserRole') is 0 for sa. Where a feature accepts
// either, ask for both. Permissions need no such care — HAS_PERMS_BY_NAME
// already answers 1 for a sysadmin on every server permission.
func (c *Capabilities) InServerRole(name string) bool {
	if c == nil {
		return false
	}
	return c.ServerRoles[name]
}

// Probed reports whether these capabilities came from a server that answered.
//
// The zero value and a failed probe are both "nothing known", and every
// permission accessor already treats that as unknown — but a *role* test
// cannot: InServerRole answers false for a role that was never asked about,
// exactly as it does for one the login is not in. A caller that would withhold
// something on "not a member" must check this first, or an unprobed connection
// silently loses whatever the role guards.
func (c *Capabilities) Probed() bool { return c != nil && c.ServerRoles != nil }

// IsSysadmin reports membership in the sysadmin fixed server role.
func (c *Capabilities) IsSysadmin() bool { return c.InServerRole("sysadmin") }

// Permission returns the state of one server permission. A name that was never
// probed — including a misspelt one — is CapabilityUnknown.
func (c *Capabilities) Permission(name string) CapabilityState {
	if c == nil {
		return CapabilityUnknown
	}
	return c.ServerPermissions[name]
}

// Has reports that the permission is known to be held. Use it to *offer*
// something extra.
func (c *Capabilities) Has(name string) bool {
	return c.Permission(name) == CapabilityGranted
}

// Allows reports that the permission is not known to be denied — granted, or
// unknown. Use it to decide whether to *withhold* something.
//
// The asymmetry with Has is the whole point, and gating on the wrong one is
// the mistake this pair exists to prevent. A probe that failed, or an instance
// that does not define the permission, leaves every answer Unknown; gating a
// menu item on Has would then hide the entire application from a login that
// may well be a sysadmin. The server remains the authority — withholding is
// only ever a courtesy, and it must fail open.
func (c *Capabilities) Allows(name string) bool {
	return c.Permission(name) != CapabilityDenied
}

// Capabilities reports what the connected login may do at the server scope.
func (s *Server) Capabilities() (*Capabilities, error) {
	return s.CapabilitiesContext(context.Background())
}

// CapabilitiesContext is the context-aware variant of Capabilities.
//
// One round trip: role membership and permission states come back as one
// result set of (kind, name, answer) rows, read by name rather than by column
// position, so adding a name to either list cannot shift the answers after it.
func (s *Server) CapabilitiesContext(ctx context.Context) (*Capabilities, error) {
	q, args := capabilityQuery(
		"SELECT 'R', n.v, IS_SRVROLEMEMBER(n.v)", ProbedServerRoles,
		"SELECT 'P', n.v, HAS_PERMS_BY_NAME(NULL, NULL, n.v)", ProbedServerPermissions,
	)

	rows, err := s.query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("gosmo: read server capabilities: %w", err)
	}
	defer rows.Close()

	c := &Capabilities{
		ServerRoles:       map[string]bool{},
		ServerPermissions: map[string]CapabilityState{},
	}
	if err := scanCapabilityRows(rows, c.ServerRoles, c.ServerPermissions, nil, nil); err != nil {
		return nil, fmt.Errorf("gosmo: read server capabilities: %w", err)
	}
	return c, nil
}

// DatabaseCapabilities is what the connected login may do inside one database.
//
// Obtain one with Database.Capabilities. Every method is nil-safe.
type DatabaseCapabilities struct {
	// Accessible reports HAS_DBACCESS: whether the login can open the
	// database at all. When it is false the two maps are empty, because
	// nothing inside the database could be asked — a USE into it fails.
	//
	// This is the one field to check before expanding a database in a tree or
	// opening its properties: every folder under an inaccessible database
	// fails separately and identically.
	Accessible bool

	// Roles maps each name in ProbedDatabaseRoles to membership.
	Roles map[string]bool

	// Permissions maps each name in ProbedDatabasePermissions to its state.
	Permissions map[string]CapabilityState

	// SchemaPermissions maps each schema in the database to the state of each
	// name in ProbedSchemaPermissions on it. Read it through
	// SchemaPermission/PermitsOnSchema rather than directly.
	//
	// It exists because the database-scope map cannot answer for a schema: a
	// principal granted ALTER on one schema and nothing else holds no
	// database-wide permission at all, and a caller gating a rename or a drop
	// on the database-scope answer withholds it from exactly the principal
	// SQL Server would let through.
	SchemaPermissions map[string]map[string]CapabilityState

	// ObjectPermissions maps "schema.object" to the state of each name in
	// ProbedObjectPermissions on it. Read it through HasOnObject.
	//
	// Unlike the other three maps this one is *sparse*: it holds a row only
	// for an object the login was granted a permission on, was denied one on,
	// or owns. A missing entry means "no explicit grant", never "not probed",
	// so an Allows/Permits-style reading of it would report every object in
	// the database as permitted. HasOnObject is the only safe test.
	ObjectPermissions map[string]map[string]CapabilityState
}

// SchemaPermission returns the state of one SCHEMA-scope permission on the
// named schema. A schema that does not exist, or a name that was never probed,
// is CapabilityUnknown — as is every schema of a database that was not probed
// at all.
func (c *DatabaseCapabilities) SchemaPermission(schema, name string) CapabilityState {
	if c == nil {
		return CapabilityUnknown
	}
	return c.SchemaPermissions[schema][name]
}

// HasOnSchema reports that the permission is known to be held on the schema —
// the test for offering something extra. See Capabilities.Has.
func (c *DatabaseCapabilities) HasOnSchema(schema, name string) bool {
	return c.SchemaPermission(schema, name) == CapabilityGranted
}

// AllowsOnSchema reports that the permission is not known to be denied on the
// schema. See Capabilities.Allows.
func (c *DatabaseCapabilities) AllowsOnSchema(schema, name string) bool {
	return c.SchemaPermission(schema, name) != CapabilityDenied
}

// PermitsOnSchema is the test for withholding something scoped to one schema:
// AllowsOnSchema, plus the accessibility every answer inside the database
// takes for granted. Permits's counterpart — see it for why accessibility
// belongs in the withholding test.
func (c *DatabaseCapabilities) PermitsOnSchema(schema, name string) bool {
	if c == nil {
		return true
	}
	return c.Accessible && c.AllowsOnSchema(schema, name)
}

// Probed reports whether these capabilities came from a database that
// answered. Capabilities.Probed's counterpart, and needed for the same reason:
// InRole answers false for a role that was never asked about exactly as it
// does for one the login is not in, so a caller that would withhold something
// on "not a member" must check this first.
//
// It reads Roles rather than Accessible because an inaccessible database is a
// real answer — the probe ran and reported that nothing inside could be asked.
func (c *DatabaseCapabilities) Probed() bool { return c != nil && c.Roles != nil }

// ObjectKey is the key ObjectPermissions is indexed by: the schema and object
// name joined with a dot, unquoted, exactly as the probe records them.
func ObjectKey(schema, object string) string { return schema + "." + object }

// ObjectPermission returns the state of one OBJECT-scope permission on the
// named object. An object with no explicit grant, deny or distinct owner is
// CapabilityUnknown — which here means "nothing was recorded for it", not
// "the probe did not run".
func (c *DatabaseCapabilities) ObjectPermission(schema, object, name string) CapabilityState {
	if c == nil {
		return CapabilityUnknown
	}
	return c.ObjectPermissions[ObjectKey(schema, object)][name]
}

// HasOnObject reports that the permission is known to be held on the object.
//
// This is the only sound test against ObjectPermissions, and the reason is the
// map's sparseness rather than the usual offer-versus-withhold distinction: an
// object nobody granted anything on has no row, so "not denied" is true of
// every object in the database and an AllowsOnObject would gate nothing.
//
// Use it as an *additional* reason to permit something, alongside the
// database- and schema-scope answers — never as the reason to withhold it.
// A principal granted ALTER on one table holds no permission at either wider
// scope, so those answer 0 and a caller reading only them withholds a write
// SQL Server would have allowed.
func (c *DatabaseCapabilities) HasOnObject(schema, object, name string) bool {
	return c.ObjectPermission(schema, object, name) == CapabilityGranted
}

// InRole reports whether the login's user in this database is a member of the
// named fixed database role. As with Capabilities.InServerRole, membership in
// db_owner (or in sysadmin) is not folded in.
func (c *DatabaseCapabilities) InRole(name string) bool {
	if c == nil {
		return false
	}
	return c.Roles[name]
}

// Permission returns the state of one database-scope permission. A name that
// was never probed is CapabilityUnknown.
func (c *DatabaseCapabilities) Permission(name string) CapabilityState {
	if c == nil {
		return CapabilityUnknown
	}
	return c.Permissions[name]
}

// Has reports that the permission is known to be held — the test for offering
// something extra. See Capabilities.Has.
func (c *DatabaseCapabilities) Has(name string) bool {
	return c.Permission(name) == CapabilityGranted
}

// Allows reports that the permission is not known to be denied. See
// Capabilities.Allows, which explains why it and Has are not opposites.
//
// At database scope this is *not* the whole test for withholding something —
// use Permits. Allows answers only the question it is asked, and an
// inaccessible database was never asked anything.
func (c *DatabaseCapabilities) Allows(name string) bool {
	return c.Permission(name) != CapabilityDenied
}

// Permits is the test for withholding something at database scope: Allows,
// plus the accessibility the permission answer takes for granted.
//
// A database the login cannot open answers CapabilityUnknown to every
// permission, because there was nothing inside it to ask — Accessible false is
// the only thing the server said. Unknown fails open, so Allows alone reports
// "not known to be denied" for a database the login cannot so much as connect
// to, and a caller following Capabilities.Allows's advice would offer Back Up
// and Delete on exactly the databases it has no business writing to.
//
// The fail-open direction is kept where it belongs: a probe that could not run
// at all leaves Accessible true (see Database.CapabilitiesContext), so Permits
// still says yes there. Only a measured "cannot open this" withholds.
//
// Capabilities has no counterpart because there is no server-scope equivalent
// of an inaccessible database: a login that cannot reach the instance has no
// Capabilities to ask.
//
// One shape to know: a nil *DatabaseCapabilities is "nothing known" and fails
// open, but the *zero value* is not — its Accessible is false, which reads as a
// measured "cannot open this" and withholds. Anything hand-building one to
// stand in for a probe that could not run must set Accessible true, the way
// CapabilitiesContext does for every database it reached.
func (c *DatabaseCapabilities) Permits(name string) bool {
	if c == nil {
		return true
	}
	return c.Accessible && c.Allows(name)
}

// Capabilities reports what the connected login may do inside d.
func (d *Database) Capabilities() (*DatabaseCapabilities, error) {
	return d.CapabilitiesContext(context.Background())
}

// CapabilitiesContext is the context-aware variant of Capabilities.
//
// Accessibility is settled first, at the *server* scope, and an inaccessible
// database returns early with Accessible false and no error. That ordering is
// required rather than tidy: the role and permission probe runs inside the
// database, and Database.query opens with a USE, which is itself what fails
// for a login that cannot connect there.
func (d *Database) CapabilitiesContext(ctx context.Context) (*DatabaseCapabilities, error) {
	var access sql.NullBool
	if err := d.server.queryRowScan(ctx, "SELECT HAS_DBACCESS(@p1)", []any{d.name}, &access); err != nil {
		return nil, fmt.Errorf("gosmo: read capabilities for database %q: %w", d.name, err)
	}
	// NULL means the database does not exist or is not visible; either way
	// there is nothing inside it to ask about.
	if !access.Valid || !access.Bool {
		return &DatabaseCapabilities{
			Roles:             map[string]bool{},
			Permissions:       map[string]CapabilityState{},
			SchemaPermissions: map[string]map[string]CapabilityState{},
			ObjectPermissions: map[string]map[string]CapabilityState{},
		}, nil
	}

	q, args := capabilityQuery(
		"SELECT 'R', n.v, IS_ROLEMEMBER(n.v)", ProbedDatabaseRoles,
		"SELECT 'P', n.v, HAS_PERMS_BY_NAME(DB_NAME(), 'DATABASE', n.v)", ProbedDatabasePermissions,
	)
	sq, sargs := schemaCapabilityQuery(len(args)+1, ProbedSchemaPermissions)
	q += "\nUNION ALL\n" + sq
	args = append(args, sargs...)

	oq, oargs := objectCapabilityQuery(len(args)+1, ProbedObjectPermissions)
	q = capabilityPrincipalCTE + q + "\nUNION ALL\n" + oq
	args = append(args, oargs...)

	rows, err := d.query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("gosmo: read capabilities for database %q: %w", d.name, err)
	}
	defer rows.Close()

	c := &DatabaseCapabilities{
		Accessible:        true,
		Roles:             map[string]bool{},
		Permissions:       map[string]CapabilityState{},
		SchemaPermissions: map[string]map[string]CapabilityState{},
		ObjectPermissions: map[string]map[string]CapabilityState{},
	}
	if err := scanCapabilityRows(rows.Rows, c.Roles, c.Permissions, c.SchemaPermissions, c.ObjectPermissions); err != nil {
		return nil, fmt.Errorf("gosmo: read capabilities for database %q: %w", d.name, err)
	}
	return c, nil
}

// capabilityQuery builds the two-part probe: one row per name, tagged 'R' or
// 'P', with each name passed as a parameter through a VALUES constructor
// rather than pasted into the text.
//
// The name is selected back alongside its answer so the caller reads the
// result by name. Scanning N booleans positionally instead would make adding a
// name to one of the lists silently shift every answer after it.
func capabilityQuery(roleSelect string, roles []string, permSelect string, perms []string) (string, []any) {
	args := make([]any, 0, len(roles)+len(perms))
	for _, n := range roles {
		args = append(args, n)
	}
	for _, n := range perms {
		args = append(args, n)
	}

	var b strings.Builder
	b.WriteString(roleSelect)
	b.WriteString(valuesClause(1, len(roles)))
	b.WriteString("\nUNION ALL\n")
	b.WriteString(permSelect)
	b.WriteString(valuesClause(1+len(roles), len(perms)))
	return b.String(), args
}

// schemaCapabilityQuery builds the third block of the database probe: one row
// per schema per probed permission, asked of every schema in the database in
// one pass rather than a query per schema.
//
// The permission travels in the *kind* column and the schema in the name
// column, not the other way round: a permission name is ours and fixed, a
// schema name is user data, and a schema called "P" would otherwise be read
// back as a database-scope permission answer.
func schemaCapabilityQuery(first int, perms []string) (string, []any) {
	args := make([]any, len(perms))
	for i, n := range perms {
		args[i] = n
	}
	return "SELECT CONCAT('S:', n.v), s.name, HAS_PERMS_BY_NAME(QUOTENAME(s.name), 'SCHEMA', n.v)" +
		" FROM sys.schemas AS s CROSS JOIN (VALUES " + valuesList(first, len(perms)) + ") AS n(v)", args
}

// objectCapabilityQuery builds the OBJECT-scope block: one row per object the
// login has an explicit permission on or owns, tagged "O:<permission>" with
// the object as "schema.object" and 1 for held, 0 for denied.
//
// It is a catalog read rather than a HAS_PERMS_BY_NAME probe because that
// function answers for one securable per call — a query per object, which is
// what kept this scope unprobed. Four details are load-bearing, each of them a
// wrong answer if dropped:
//
//   - The recursive CTE walks role membership. A permission granted to a role
//     the login reaches only through another role is held just as fully as one
//     granted directly, and stopping at the first level misses it.
//   - public is in the principal set, so the permission_name filter has to
//     stay: without it every catalog view's SELECT grant to public comes back,
//     235 rows on a stock database against the 3 that matter.
//   - minor_id = 0 keeps column-level grants out. They share class 1 with the
//     object-level ones and would otherwise report a column grant as a grant
//     on the table.
//   - The sys.objects half is not redundant with the permissions half: an
//     object's owner holds implicit CONTROL and has *no* permission row at
//     all, so ownership is invisible to the catalog's permission list.
//
// An object the login can see nothing of comes back with a NULL name — a DENY
// leaves no permission behind, and metadata visibility then hides the object —
// and is dropped. Such an object is equally invisible in any listing built
// from the same catalog, so there is nothing for the answer to gate.
func objectCapabilityQuery(first int, perms []string) (string, []any) {
	args := make([]any, len(perms))
	for i, n := range perms {
		args[i] = n
	}
	return `SELECT CONCAT('O:', n.v), CONCAT(SCHEMA_NAME(o.schema_id), '.', o.name), 1
	FROM sys.objects AS o CROSS JOIN (VALUES ` + valuesList(first, len(perms)) + `) AS n(v)
	WHERE o.principal_id IN (SELECT id FROM cap_me)
UNION ALL
	SELECT CONCAT('O:', n.v), CONCAT(OBJECT_SCHEMA_NAME(p.major_id), '.', OBJECT_NAME(p.major_id)),
	       CASE WHEN p.state IN ('D') THEN 0 ELSE 1 END
	FROM sys.database_permissions AS p CROSS JOIN (VALUES ` + valuesList(first, len(perms)) + `) AS n(v)
	WHERE p.class = 1 AND p.minor_id = 0
	  AND p.grantee_principal_id IN (SELECT id FROM cap_me)
	  AND p.permission_name IN (n.v, 'CONTROL')
	  AND OBJECT_NAME(p.major_id) IS NOT NULL`, args
}

// capabilityPrincipalCTE is the "every principal the login's permissions can
// arrive through" set the object block selects from: the user itself, public,
// and every role reachable through role membership at any depth.
const capabilityPrincipalCTE = `WITH cap_me AS (
	SELECT DATABASE_PRINCIPAL_ID() AS id
	UNION ALL SELECT DATABASE_PRINCIPAL_ID('public')
	UNION ALL
	SELECT rm.role_principal_id FROM sys.database_role_members AS rm
	JOIN cap_me ON rm.member_principal_id = cap_me.id
)
`

// valuesList renders "(@pN),(@pN+1),..." for count parameters starting at
// first — the placeholder list both probe blocks bind their names through.
func valuesList(first, count int) string {
	var b strings.Builder
	for i := range count {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, "(@p%d)", first+i)
	}
	return b.String()
}

// valuesClause renders " FROM (VALUES (@pN),(@pN+1),...) AS n(v)" for count
// parameters starting at first.
func valuesClause(first, count int) string {
	var b strings.Builder
	b.WriteString(" FROM (VALUES ")
	b.WriteString(valuesList(first, count))
	b.WriteString(") AS n(v)")
	return b.String()
}

// scanCapabilityRows fills roles and perms from the probe's (kind, name,
// answer) rows. A NULL answer is left out of the map entirely, which is what
// makes it read back as CapabilityUnknown / false.
func scanCapabilityRows(rows *sql.Rows, roles map[string]bool, perms map[string]CapabilityState, schemas, objects map[string]map[string]CapabilityState) error {
	for rows.Next() {
		var kind, name string
		var answer sql.NullInt64
		if err := rows.Scan(&kind, &name, &answer); err != nil {
			return err
		}
		switch {
		case kind == "R":
			roles[name] = answer.Valid && answer.Int64 == 1
		case kind == "P":
			if st, ok := capabilityStateOf(answer); ok {
				perms[name] = st
			}
		case strings.HasPrefix(kind, "S:"):
			// name is the schema here, and the permission rides in kind — see
			// schemaCapabilityQuery. A server probe passes a nil map and drops
			// these rows, which it never asks for.
			st, ok := capabilityStateOf(answer)
			if !ok || schemas == nil {
				continue
			}
			if schemas[name] == nil {
				schemas[name] = map[string]CapabilityState{}
			}
			schemas[name][strings.TrimPrefix(kind, "S:")] = st
		case strings.HasPrefix(kind, "O:"):
			// As with "S:", name is the securable and the permission rides in
			// kind. A denial wins over a grant however the two rows are
			// ordered: SQL Server resolves DENY over GRANT, and the object
			// block can produce both for one object — a grant on a role and a
			// deny on the user.
			st, ok := capabilityStateOf(answer)
			if !ok || objects == nil {
				continue
			}
			perm := strings.TrimPrefix(kind, "O:")
			if objects[name] == nil {
				objects[name] = map[string]CapabilityState{}
			}
			if objects[name][perm] == CapabilityDenied {
				continue
			}
			objects[name][perm] = st
		}
	}
	return rows.Err()
}

// capabilityStateOf maps one HAS_PERMS_BY_NAME answer to a state. NULL is not
// a state: the permission does not apply to the securable on this instance,
// and recording it as denied would withhold whatever it gates.
func capabilityStateOf(answer sql.NullInt64) (CapabilityState, bool) {
	if !answer.Valid {
		return CapabilityUnknown, false
	}
	if answer.Int64 == 1 {
		return CapabilityGranted, true
	}
	return CapabilityDenied, true
}
