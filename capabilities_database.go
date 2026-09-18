package gosmo

// capabilities_database.go is the database-scoped half of the probe: the fixed
// database roles the login is in, the database permissions it holds, and the
// per-schema, per-object, per-column, per-principal and per-securable answers
// beneath them. What is asked is capabilities.go's Probed* lists.

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

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

	// ExplicitSchemaPermissions maps a schema name to the state each name in
	// ProbedSchemaPermissions is *explicitly* recorded in for the login, read
	// out of sys.database_permissions rather than asked with
	// HAS_PERMS_BY_NAME. Read it through DeniedOnSchema.
	//
	// It exists because SchemaPermissions cannot answer "is this denied?":
	// HAS_PERMS_BY_NAME returns 0 both for a permission explicitly denied on
	// the schema and for one simply never granted, so its CapabilityDenied
	// means "the server said no to this question", not "a DENY row exists".
	// The difference decides whether a wider grant may answer for the schema —
	// it may for the second, and must not for the first, because SQL Server
	// resolves a schema-scope DENY over a database-wide GRANT.
	//
	// Sparse in ObjectPermissions' sense: a schema nobody granted or denied
	// anything on has no row. Ownership is deliberately not folded in — an
	// owner never carries a DENY, so it cannot change the one answer this map
	// is read for.
	ExplicitSchemaPermissions map[string]map[string]CapabilityState

	// ExplicitDatabasePermissions maps each name in ProbedDatabasePermissions
	// to the state it is *explicitly* recorded in for the login at DATABASE
	// scope (class 0), read out of sys.database_permissions rather than asked
	// with HAS_PERMS_BY_NAME. Read it through DeniedOnDatabase.
	//
	// It is ExplicitSchemaPermissions' database-scope twin and exists for the
	// same reason: Permissions cannot answer "is this denied?", because
	// HAS_PERMS_BY_NAME returns 0 both for a permission explicitly denied and
	// for one simply never granted. The difference decides whether a
	// *narrower* grant may answer for the database — it may for the second,
	// and must not for the first, because SQL Server resolves a database-scope
	// DENY over an object- or schema-scope GRANT.
	//
	// Sparse in ObjectPermissions' sense: a permission with no explicit row
	// has no entry.
	//
	// Only DENY rows are read. The grant direction is already answered, and
	// answered better, by HAS_PERMS_BY_NAME in Permissions, which folds in
	// role membership and covering permissions. CONTROL is deliberately not
	// matched alongside the permission asked about the way the schema block
	// matches it: DENY CONTROL at database scope denies CONNECT with it, so
	// the login cannot open the database at all and Accessible false is what
	// reports that — verified live 2026-09-04, Msg 916.
	ExplicitDatabasePermissions map[string]CapabilityState

	// ExplicitPrincipalPermissions maps a database principal's name — a user
	// or a database role — to the state each name in
	// ProbedPrincipalPermissions is *explicitly* recorded in for the login at
	// DATABASE_PRINCIPAL scope (class 4), read out of sys.database_permissions.
	// Read it through DeniedOnPrincipal.
	//
	// It is ExplicitSchemaPermissions' class-4 twin and exists for the same
	// reason: a class-4 DENY overrides the database-wide ALTER ANY USER a gate
	// would otherwise read as permission, and HAS_PERMS_BY_NAME cannot tell
	// that DENY from a permission never granted.
	//
	// Sparse in ObjectPermissions' sense: a principal nobody denied anything
	// on has no row.
	//
	// Only DENY rows are read, for ExplicitDatabasePermissions' reason and one
	// of its own: at this class there is no grant direction to miss at all.
	// GRANT ALTER ON USER::x permits neither the rename nor the drop — see
	// ProbedPrincipalPermissions, where the live result is recorded.
	//
	// **Roles are recorded but answer differently, and a caller must not treat
	// the two alike.** Verified live on majors 13, 14 and 17: a class-4 DENY on
	// a *role* withholds nothing — DROP ROLE and ALTER ROLE ... WITH NAME check
	// ALTER ANY ROLE at database scope and are permitted with the DENY in
	// place, even though HAS_PERMS_BY_NAME reports 0 for ALTER on the role.
	// The rows are kept because they are what the catalog says and a caller may
	// have a use for them, but a gate over role rename or drop that reads this
	// map withholds an action the server allows.
	ExplicitPrincipalPermissions map[string]map[string]CapabilityState

	// ObjectPermissions maps "schema.object" to the state of each name in
	// ProbedObjectPermissions on it. Read it through HasOnObject.
	//
	// Unlike the other three maps this one is *sparse*: it holds a row only
	// for an object the login was granted a permission on, was denied one on,
	// or owns. A missing entry means "no explicit grant", never "not probed",
	// so an Allows/Permits-style reading of it would report every object in
	// the database as permitted. HasOnObject is the only safe test.
	ObjectPermissions map[string]map[string]CapabilityState

	// ColumnPermissions maps "schema.object.column" to the state of each name
	// in ProbedObjectPermissions that was granted or denied on that column.
	// Read it through HasOnColumn/DeniedOnColumn/DeniedOnAnyColumn.
	//
	// It is separate from ObjectPermissions rather than folded into it because
	// a column-scope row answers for the column alone: recorded on the table
	// it would report a DENY on one column as a DENY on the whole table, and a
	// GRANT on one column as a grant on all of them. Sparse for
	// ObjectPermissions' reason, and read the same way.
	ColumnPermissions map[string]map[string]CapabilityState

	// SecurablePermissions maps each assembly, user-defined type and XML
	// schema collection in the database — keyed by DatabaseSecurableKey — to
	// the state of each name in ProbedSecurablePermissions on it. Read it
	// through HasOnSecurable or PermitsOnSecurable.
	//
	// It exists because none of the maps above can answer for these three
	// classes: ObjectPermissions is class 1 only, and a principal granted
	// CONTROL on one assembly, or owning it, holds no database- or
	// schema-scope permission at all. A caller gating the drop on those alone
	// withholds it from exactly the principal SQL Server lets through.
	//
	// Like AvailabilityGroupPermissions this is a HAS_PERMS_BY_NAME answer, so
	// it is *not* sparse: every securable the login can see has a row, and a
	// missing one means it was created after the probe or was never asked.
	// Which statements its answer decides is recorded, with the live result,
	// on ProbedSecurablePermissions — a transfer entirely, a drop only in the
	// permitting direction.
	SecurablePermissions map[string]map[string]CapabilityState
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

// DeniedOnSchema reports that the permission is explicitly denied on the
// schema — SchemaPermissions' withholding counterpart, and the only sound read
// of ExplicitSchemaPermissions.
//
// It is sound where an AllowsOnSchema-style read of the sparse map would not
// be, for DeniedOnObject's reason: it asks for a state that was recorded
// rather than for the absence of one, so a schema nobody mentioned reads
// unknown, which is not a denial.
//
// A caller may withhold on it because SQL Server resolves DENY over GRANT
// across scopes: a principal holding database-wide ALTER whose schema carries
// DENY ALTER cannot rename, move or drop anything in it, and the write fails
// Msg 297. The same two exceptions belong to the caller as for DeniedOnObject:
// a member of sysadmin bypasses the check and must be asked about first, and a
// database that was never probed records nothing.
func (c *DatabaseCapabilities) DeniedOnSchema(schema, name string) bool {
	if c == nil {
		return false
	}
	return c.ExplicitSchemaPermissions[schema][name] == CapabilityDenied
}

// DeniedOnDatabase reports that the permission is explicitly denied at
// DATABASE scope — Permissions' withholding counterpart, and the only sound
// read of ExplicitDatabasePermissions.
//
// It is DeniedOnSchema one scope wider, and sound for the same reason: it asks
// for a state that was recorded rather than for the absence of one, so a
// permission nobody denied reads unknown, which is not a denial. Permits
// cannot stand in for it — HAS_PERMS_BY_NAME answers 0 for a permission never
// granted, which is the ordinary case for a principal working through an
// object- or schema-scope grant, and withholding on that would take the write
// away from exactly the principal it was granted to.
//
// A caller may withhold on it because SQL Server resolves DENY over GRANT
// across scopes in *both* directions: a principal granted ALTER on one table,
// in a database that denies it ALTER, reads
// HAS_PERMS_BY_NAME('dbo.t1','OBJECT','ALTER') = 0 and its ALTER TABLE fails —
// verified live 2026-09-04. The narrower grant does not survive the wider
// DENY; only a *column* grant overrides an object DENY, which is the one
// documented exception and runs the other way. The same two exceptions belong
// to the caller as for DeniedOnSchema: a member of sysadmin bypasses the check
// and must be asked about first, and a database that was never probed records
// nothing.
func (c *DatabaseCapabilities) DeniedOnDatabase(name string) bool {
	if c == nil {
		return false
	}
	return c.ExplicitDatabasePermissions[name] == CapabilityDenied
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

// DeniedOnPrincipal reports that the permission is explicitly denied on the
// database principal — the only sound read of ExplicitPrincipalPermissions,
// for DeniedOnSchema's reason: it asks for a state that was recorded rather
// than for the absence of one, so a principal nobody denied reads unknown,
// which is not a denial.
//
// A caller may withhold on it because SQL Server resolves a class-4 DENY over
// the database-wide ALTER ANY USER that would otherwise permit the write:
// verified live on majors 13, 14 and 17, DENY ALTER ON USER::x (or DENY
// CONTROL) refuses both ALTER USER ... WITH NAME and DROP USER for a principal
// holding ALTER ANY USER. The same two exceptions belong to the caller as for
// DeniedOnSchema — a member of sysadmin bypasses the check and must be asked
// about first, and a database that was never probed records nothing.
//
// **For a database role, the answer is actionable per action, not per role.**
// The map records users and roles alike, because both are class 4 and the
// catalog does not distinguish them here, but a class-4 DENY on a *role* does
// not withhold everything a DENY on a user does. Verified live on majors 13, 14
// and 17, with HAS_PERMS_BY_NAME reporting 0 for ALTER on the role throughout:
//
//   - DROP ROLE and ALTER ROLE ... WITH NAME check ALTER ANY ROLE at database
//     scope and go through with the DENY in place. A gate that withholds a role
//     rename or drop on this answer withholds an action the server allows.
//   - ALTER ROLE ... ADD MEMBER / DROP MEMBER is refused (Msg 15151). A gate
//     that offers a membership edit on this answer offers one the server
//     refuses (probed 2026-09-04 on majors 13 and 17).
//
// Membership also checks ALTER on the *member*: adding a user carrying a
// class-4 DENY to a role nobody denied is refused too, so a membership gate has
// to ask about both principals.
func (c *DatabaseCapabilities) DeniedOnPrincipal(principal, name string) bool {
	if c == nil {
		return false
	}
	return c.ExplicitPrincipalPermissions[principal][name] == CapabilityDenied
}

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

// DeniedOnObject reports that the permission is explicitly denied on the
// object — the one thing this map may be read for in order to *withhold*
// something, and the counterpart to HasOnObject.
//
// It is sound where an AllowsOnObject would not be, because it asks for the
// state that was actually recorded rather than for the absence of one: an
// object nobody mentioned has no row and reads CapabilityUnknown, which is not
// a denial. Only a DENY reaching the login — directly, through a role, or
// through public — puts CapabilityDenied here.
//
// A caller may withhold on it because SQL Server resolves an object-scope DENY
// over every wider grant: a principal holding database-wide ALTER, or db_owner,
// reads HAS_PERMS_BY_NAME 0 on a table denied ALTER and its rename fails
// Msg 297 (verified live 2026-09-01). Two exceptions belong to the caller, not
// here: a member of sysadmin bypasses the check entirely and must be asked
// about first, and a database that was never probed records nothing, which
// reads as no denial and so withholds nothing.
//
// Ownership needs no such care. A DENY cannot be made to the owner of the
// securable — SQL Server refuses it — and transferring ownership to a denied
// principal *deletes* the DENY row, so an owner never carries one (both
// verified live 2026-09-01). An owner denied through public is genuinely
// refused by the server, which is what this then reports.
func (c *DatabaseCapabilities) DeniedOnObject(schema, object, name string) bool {
	return c.ObjectPermission(schema, object, name) == CapabilityDenied
}

// ColumnKey is the key ColumnPermissions is indexed by: the schema, object and
// column joined with dots, unquoted, exactly as the probe records them.
func ColumnKey(schema, object, column string) string {
	return schema + "." + object + "." + column
}

// ColumnPermission returns the state of one OBJECT-scope permission recorded
// on a single column. A column with no explicit grant or deny is
// CapabilityUnknown, which here means "nothing was recorded for it".
func (c *DatabaseCapabilities) ColumnPermission(schema, object, column, name string) CapabilityState {
	if c == nil {
		return CapabilityUnknown
	}
	return c.ColumnPermissions[ColumnKey(schema, object, column)][name]
}

// HasOnColumn reports that the permission is known to be held on the column.
// HasOnObject's counterpart, and additive for the same reason: a column
// carrying no row of its own is covered by whatever the table and the wider
// scopes grant.
func (c *DatabaseCapabilities) HasOnColumn(schema, object, column, name string) bool {
	return c.ColumnPermission(schema, object, column, name) == CapabilityGranted
}

// DeniedOnColumn reports that the permission is explicitly denied on the
// column. DeniedOnObject's counterpart and sound for its reason — it asks for
// a recorded state rather than for the absence of one — with the same two
// exceptions belonging to the caller: sysadmin bypasses the check, and a
// database that was never probed records nothing.
func (c *DatabaseCapabilities) DeniedOnColumn(schema, object, column, name string) bool {
	return c.ColumnPermission(schema, object, column, name) == CapabilityDenied
}

// DeniedOnAnyColumn reports that the permission is denied on at least one
// column of the object, and names one such column.
//
// This is what a caller gating a *table-wide* action asks. SQL Server resolves
// a column-scope DENY over every wider grant exactly as it does an
// object-scope one, so a statement touching all the columns fails Msg 230 for
// a principal that holds the permission on the table itself — and asking
// DeniedOnObject alone lets the wider grant answer for a column it does not
// cover. An action scoped to named columns should ask DeniedOnColumn per
// column instead.
func (c *DatabaseCapabilities) DeniedOnAnyColumn(schema, object, name string) (string, bool) {
	if c == nil {
		return "", false
	}
	prefix := ObjectKey(schema, object) + "."
	// The lowest name rather than the first the map yields: a caller that puts
	// the column in a message would otherwise show a different one each time
	// two of them are denied.
	found := ""
	for key, states := range c.ColumnPermissions {
		if !strings.HasPrefix(key, prefix) || states[name] != CapabilityDenied {
			continue
		}
		if col := strings.TrimPrefix(key, prefix); found == "" || col < found {
			found = col
		}
	}
	return found, found != ""
}

// SecurablePermission returns the state of one permission on an assembly, a
// user-defined type or an XML schema collection; schema is "" for an assembly.
// A securable the probe did not reach, a name that was never probed, and every
// securable of a database that was not probed at all are CapabilityUnknown.
func (c *DatabaseCapabilities) SecurablePermission(kind DatabaseSecurableKind, schema, name, perm string) CapabilityState {
	if c == nil {
		return CapabilityUnknown
	}
	return c.SecurablePermissions[DatabaseSecurableKey(kind, schema, name)][perm]
}

// HasOnSecurable reports that the permission is known to be held on the
// securable — the test for offering something extra, such as a drop the wider
// rights beside it do not permit. See Capabilities.Has.
func (c *DatabaseCapabilities) HasOnSecurable(kind DatabaseSecurableKind, schema, name, perm string) bool {
	return c.SecurablePermission(kind, schema, name, perm) == CapabilityGranted
}

// PermitsOnSecurable is the test for withholding something the securable's own
// permission decides alone — ALTER SCHEMA ... TRANSFER of a type or a
// collection, which nothing narrower or wider than CONTROL permits. It is
// PermitsOnSchema one scope down: not known to be denied, plus the
// accessibility every answer inside the database takes for granted.
//
// It is sound in the withholding direction because the map is not sparse, for
// Capabilities.PermitsOnAvailabilityGroup's reason: a 0 here is the server's
// answer about this securable, not a silence.
func (c *DatabaseCapabilities) PermitsOnSecurable(kind DatabaseSecurableKind, schema, name, perm string) bool {
	if c == nil {
		return true
	}
	return c.Accessible && c.SecurablePermission(kind, schema, name, perm) != CapabilityDenied
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
	if err := d.server.queryRowScan(ctx, "SELECT HAS_DBACCESS(@p1)", []any{d.Name}, &access); err != nil {
		return nil, fmt.Errorf("gosmo: read capabilities for database %q: %w", d.Name, err)
	}
	// NULL means the database does not exist or is not visible; either way
	// there is nothing inside it to ask about.
	if !access.Valid || !access.Bool {
		return &DatabaseCapabilities{
			Roles:                        map[string]bool{},
			Permissions:                  map[string]CapabilityState{},
			SchemaPermissions:            map[string]map[string]CapabilityState{},
			ExplicitSchemaPermissions:    map[string]map[string]CapabilityState{},
			ExplicitDatabasePermissions:  map[string]CapabilityState{},
			ExplicitPrincipalPermissions: map[string]map[string]CapabilityState{},
			ObjectPermissions:            map[string]map[string]CapabilityState{},
			ColumnPermissions:            map[string]map[string]CapabilityState{},
			SecurablePermissions:         map[string]map[string]CapabilityState{},
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

	dq, dargs := explicitSchemaCapabilityQuery(len(args)+1, ProbedSchemaPermissions)
	q += "\nUNION ALL\n" + dq
	args = append(args, dargs...)

	bq, bargs := explicitDatabaseCapabilityQuery(len(args)+1, ProbedDatabasePermissions)
	q += "\nUNION ALL\n" + bq
	args = append(args, bargs...)

	pq, pargs := explicitPrincipalCapabilityQuery(len(args)+1, ProbedPrincipalPermissions)
	q += "\nUNION ALL\n" + pq
	args = append(args, pargs...)

	kq, kargs := securableCapabilityQuery(len(args)+1, ProbedSecurablePermissions)
	q += "\nUNION ALL\n" + kq
	args = append(args, kargs...)

	rows, err := d.query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("gosmo: read capabilities for database %q: %w", d.Name, err)
	}
	defer rows.Close()

	c := &DatabaseCapabilities{
		Accessible:                   true,
		Roles:                        map[string]bool{},
		Permissions:                  map[string]CapabilityState{},
		SchemaPermissions:            map[string]map[string]CapabilityState{},
		ExplicitSchemaPermissions:    map[string]map[string]CapabilityState{},
		ExplicitDatabasePermissions:  map[string]CapabilityState{},
		ExplicitPrincipalPermissions: map[string]map[string]CapabilityState{},
		ObjectPermissions:            map[string]map[string]CapabilityState{},
		ColumnPermissions:            map[string]map[string]CapabilityState{},
		SecurablePermissions:         map[string]map[string]CapabilityState{},
	}
	if err := scanCapabilityRows(rows.Rows, capabilityDest{
		roles:              c.Roles,
		perms:              c.Permissions,
		schemas:            c.SchemaPermissions,
		explicitSchemas:    c.ExplicitSchemaPermissions,
		explicitDB:         c.ExplicitDatabasePermissions,
		explicitPrincipals: c.ExplicitPrincipalPermissions,
		objects:            c.ObjectPermissions,
		columns:            c.ColumnPermissions,
		securables:         c.SecurablePermissions,
	}); err != nil {
		return nil, fmt.Errorf("gosmo: read capabilities for database %q: %w", d.Name, err)
	}
	return c, nil
}
