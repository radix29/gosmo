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
	"ALTER ANY SERVER AUDIT",
	"ALTER ANY ENDPOINT",
	"ALTER ANY LINKED SERVER",
	"ALTER ANY EVENT SESSION",
	"ALTER ANY AVAILABILITY GROUP",
}

// ProbedServerSecurablePermissions are the permissions Capabilities reads
// explicit server-scope DENY rows for, once per securable the login has one
// recorded on: SERVER_PRINCIPAL (class 101 — logins and server roles alike)
// and ENDPOINT (class 105).
//
// One name is enough for the reason it is at schema and class-4 scope:
// CONTROL is matched alongside it in the query, and nothing narrower than
// ALTER is what these gates ask about.
//
// Only the DENY direction is read, and that is a fact about SQL Server rather
// than a choice — probed live on majors 13 and 17 (2026-09-04, identical on
// both). HAS_PERMS_BY_NAME cannot answer here at all: it reads 0 for a denied
// ALTER *and* for one never granted, which is the ordinary state of a login
// working through the server-wide ALTER ANY LOGIN, so only the catalog can say
// a DENY row exists. See Capabilities.ExplicitServerPermissions for what each
// class's DENY actually withholds — the two do not agree, and a caller that
// treats them alike is wrong about one of them.
var ProbedServerSecurablePermissions = []string{
	"ALTER",
}

// ProbedAvailabilityGroupPermissions are the AVAILABILITY GROUP-scope (class
// 108) permissions Capabilities probes, once per availability group on the
// instance.
//
// This scope is asked with HAS_PERMS_BY_NAME and not read out of the catalog,
// which is the opposite of every other explicit-permission block here, and the
// reason is that the catalog cannot answer: class 108's major_id is an
// internal availability-group id that **no supported view maps back to a
// name** — sys.availability_groups exposes only the group_id GUID, and the
// internal table behind it exposes nothing more. A catalog read at this class
// produces rows nothing can be matched to.
//
// HAS_PERMS_BY_NAME can be asked per group because there are single digits of
// them, where there are hundreds of logins, and it answers the question this
// scope actually needs: a login holding the server-wide
// ALTER ANY AVAILABILITY GROUP reads 1 on each group and 0 on one carrying
// DENY ALTER — verified live on the two-node cluster, 2026-09-05. That is the
// distinction sys.server_permissions had to be read for at class 101, and here
// the probe makes it directly.
var ProbedAvailabilityGroupPermissions = []string{
	"ALTER",
}

// ServerSecurableKind is the kind of server securable
// Capabilities.ExplicitServerPermissions is keyed by. Its values are the
// securable words SQL Server itself uses in DENY ... ON <kind>::<name>.
//
// Logins and server roles are told apart by the *principal's* type_desc rather
// than by class: both are class 101 SERVER_PRINCIPAL, and there is no class
// 110. A caller probing a separate class for server roles finds nothing.
type ServerSecurableKind string

const (
	// ServerSecurableLogin is a login — class 101 with a type_desc of
	// SQL_LOGIN, WINDOWS_LOGIN, WINDOWS_GROUP, EXTERNAL_LOGIN,
	// EXTERNAL_GROUP, CERTIFICATE_MAPPED_LOGIN or
	// ASYMMETRIC_KEY_MAPPED_LOGIN.
	ServerSecurableLogin ServerSecurableKind = "LOGIN"

	// ServerSecurableServerRole is a server role — class 101 with a type_desc
	// of SERVER_ROLE.
	ServerSecurableServerRole ServerSecurableKind = "SERVER ROLE"

	// ServerSecurableEndpoint is an endpoint — class 105.
	ServerSecurableEndpoint ServerSecurableKind = "ENDPOINT"
)

// ServerSecurableKey is the key ExplicitServerPermissions is indexed by: the
// securable kind and its name joined with "::", exactly as the probe records
// them and as the DENY statement spells them.
//
// The kind is part of the key rather than a separate map because the answer
// differs by kind — a class-101 DENY withholds everything on a login and only
// membership edits on a server role — and a caller must not be able to reach
// one kind's answer while asking about another's.
func ServerSecurableKey(kind ServerSecurableKind, name string) string {
	return string(kind) + "::" + name
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
	"ALTER ANY DATABASE AUDIT",
	"ALTER ANY DATABASE DDL TRIGGER",
	"ALTER ANY ASSEMBLY",
	"ALTER ANY EXTERNAL DATA SOURCE",
	"ALTER ANY EXTERNAL FILE FORMAT",
	// 2017 and later. On 2016 HAS_PERMS_BY_NAME answers NULL for a name it
	// does not know, which reads as CapabilityUnknown — there is no external
	// library on that version to be gated anyway.
	"ALTER ANY EXTERNAL LIBRARY",
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
//
// Each name is probed at column scope as well, into ColumnPermissions. Only a
// column-grantable permission can produce a row there — ALTER and CONTROL are
// not among them — so the block is empty for this list as it stands, and
// correct the moment SELECT, UPDATE or REFERENCES joins it.
var ProbedObjectPermissions = []string{
	"ALTER",
}

// ProbedPrincipalPermissions are the DATABASE_PRINCIPAL-scope (class 4)
// permissions DatabaseCapabilities probes, for every user or database role the
// login has one explicitly recorded on.
//
// Only the DENY direction is worth reading here, and that is a fact about SQL
// Server rather than a choice — verified live on majors 13, 14 and 17
// (2026-09-04, identical on all three):
//
//   - GRANT ALTER ON USER::x answers HAS_PERMS_BY_NAME 1 on the user and still
//     permits nothing: both ALTER USER ... WITH NAME and DROP USER are refused.
//     Those statements require ALTER ANY USER at database scope, so unlike an
//     object- or schema-scope grant there is no narrow grant for a wider map to
//     miss.
//   - DENY ALTER ON USER::x *does* withhold both, over a database-wide
//     ALTER ANY USER, so only the catalog can say what a gate needs to know.
//
// One name is enough for the same reason it is at schema scope, and CONTROL is
// matched alongside it in the query: DENY CONTROL ON USER::x withholds the
// same two statements and is recorded under its own permission_name.
var ProbedPrincipalPermissions = []string{
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

	// ExplicitServerPermissions maps a server securable — keyed by
	// ServerSecurableKey, so a login, a server role and an endpoint of the
	// same name stay apart — to the state each name in
	// ProbedServerSecurablePermissions is *explicitly* recorded in for the
	// login, read out of sys.server_permissions rather than asked with
	// HAS_PERMS_BY_NAME. Read it through DeniedOnLogin, DeniedOnServerRole,
	// DeniedOnEndpoint or the kind-taking DeniedOnServerSecurable.
	//
	// It is DatabaseCapabilities.ExplicitPrincipalPermissions' server-scope
	// twin and exists for the same reason: a class-101 DENY overrides the
	// server-wide ALTER ANY LOGIN a gate would otherwise read as permission,
	// and HAS_PERMS_BY_NAME cannot tell that DENY from a permission never
	// granted — it answers 0 for both.
	//
	// Sparse in ObjectPermissions' sense: a securable nobody denied anything
	// on has no row. Only DENY rows are read, for ExplicitDatabasePermissions'
	// reason — the grant direction is already answered, and answered better,
	// by HAS_PERMS_BY_NAME in ServerPermissions.
	//
	// **The three kinds are recorded alike and answer differently, and a
	// caller must not treat them alike.** Probed live on majors 13 and 17
	// (2026-09-04, identical on both), with HAS_PERMS_BY_NAME reading 0 for
	// the denied ALTER in every row including the two the server goes on to
	// allow:
	//
	//   - LOGIN: DENY ALTER ON LOGIN::x withholds ALTER LOGIN — rename and
	//     password alike — and DROP LOGIN, with no exceptions (Msg 15151).
	//   - SERVER ROLE: DENY ALTER ON SERVER ROLE::r withholds
	//     ALTER SERVER ROLE ... ADD MEMBER / DROP MEMBER (Msg 15151) and does
	//     *not* withhold the rename (WITH NAME) or DROP SERVER ROLE. This is
	//     the database role's split, not the login's all-or-nothing: a gate
	//     reading this map over a server role's rename withholds an action the
	//     server allows.
	//   - ENDPOINT: DENY ALTER ON ENDPOINT::e withholds ALTER ENDPOINT, and
	//     the refusal is Msg 6004 rather than 15151.
	//
	// Membership also checks ALTER on the *member*: adding a login carrying a
	// class-101 DENY to a server role nobody denied is refused too, so a
	// membership gate has to ask about both principals.
	ExplicitServerPermissions map[string]map[string]CapabilityState

	// AvailabilityGroupPermissions maps each availability group on the
	// instance to the state of each name in
	// ProbedAvailabilityGroupPermissions on it. Read it through
	// PermitsOnAvailabilityGroup or HasOnAvailabilityGroup.
	//
	// Unlike ExplicitServerPermissions this is a HAS_PERMS_BY_NAME answer, not
	// a catalog read — see ProbedAvailabilityGroupPermissions for why it has
	// to be — so it is *not* sparse: every group on the instance has a row,
	// and a missing group means the probe did not run rather than that nothing
	// was recorded. It is also, for that reason, the one server-scope map that
	// can be read in the Allows direction.
	//
	// What a class-108 DENY withholds, probed live on the two-node cluster
	// (major 17, 2026-09-05) with the server-wide ALTER ANY AVAILABILITY GROUP
	// held throughout: **every ALTER AVAILABILITY GROUP there is** — the
	// options SET, ADD DATABASE, REMOVE DATABASE, MODIFY REPLICA and FAILOVER,
	// each Msg 15151. This is the login's all-or-nothing shape, not the server
	// role's split.
	//
	// It does *not* withhold ALTER DATABASE ... SET HADR SUSPEND / RESUME,
	// which is checked against the database rather than the group and goes
	// through with the DENY in place.
	AvailabilityGroupPermissions map[string]map[string]CapabilityState
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

// DeniedOnServerSecurable reports that the permission is explicitly denied on
// one server securable — ServerPermissions' withholding counterpart, and the
// only sound read of ExplicitServerPermissions.
//
// It is sound where an Allows-style read of the sparse map would not be, for
// DeniedOnObject's reason: it asks for a state that was recorded rather than
// for the absence of one, so a securable nobody denied reads unknown, which is
// not a denial.
//
// A caller may withhold on it because SQL Server resolves a server-scope DENY
// over the server-wide ALTER ANY LOGIN / ALTER ANY SERVER ROLE /
// ALTER ANY ENDPOINT that would otherwise permit the write — but *which*
// writes it withholds depends on the kind, and one of the three answers is a
// split rather than an all-or-nothing. Read ExplicitServerPermissions before
// gating on this; a gate that assumes the login's answer for a server role
// withholds a rename the server allows.
//
// One exception belongs to the caller, as at every other scope: a member of
// sysadmin bypasses the check and must be asked about first, because the
// probe's principal set includes public and a DENY made to public is recorded
// for a sysadmin whose write SQL Server still allows.
func (c *Capabilities) DeniedOnServerSecurable(kind ServerSecurableKind, name, permission string) bool {
	if c == nil {
		return false
	}
	return c.ExplicitServerPermissions[ServerSecurableKey(kind, name)][permission] == CapabilityDenied
}

// DeniedOnLogin is DeniedOnServerSecurable for a login (class 101,
// type_desc SQL_LOGIN and friends). Its DENY is all-or-nothing: it withholds
// ALTER LOGIN and DROP LOGIN alike.
func (c *Capabilities) DeniedOnLogin(name, permission string) bool {
	return c.DeniedOnServerSecurable(ServerSecurableLogin, name, permission)
}

// DeniedOnServerRole is DeniedOnServerSecurable for a server role (class 101,
// type_desc SERVER_ROLE). Its DENY withholds the role's membership edits and
// *not* its rename or its drop — see ExplicitServerPermissions, where the live
// result is recorded.
func (c *Capabilities) DeniedOnServerRole(name, permission string) bool {
	return c.DeniedOnServerSecurable(ServerSecurableServerRole, name, permission)
}

// DeniedOnEndpoint is DeniedOnServerSecurable for an endpoint (class 105). Its
// DENY withholds ALTER ENDPOINT, refused with Msg 6004.
func (c *Capabilities) DeniedOnEndpoint(name, permission string) bool {
	return c.DeniedOnServerSecurable(ServerSecurableEndpoint, name, permission)
}

// AvailabilityGroupPermission returns the state of one AVAILABILITY GROUP-scope
// permission on the named group. A group that does not exist, or a name that
// was never probed — including every group of a server that was not probed at
// all — is CapabilityUnknown.
func (c *Capabilities) AvailabilityGroupPermission(group, name string) CapabilityState {
	if c == nil {
		return CapabilityUnknown
	}
	return c.AvailabilityGroupPermissions[group][name]
}

// HasOnAvailabilityGroup reports that the permission is known to be held on the
// group — the test for offering something extra. See Capabilities.Has.
func (c *Capabilities) HasOnAvailabilityGroup(group, name string) bool {
	return c.AvailabilityGroupPermission(group, name) == CapabilityGranted
}

// PermitsOnAvailabilityGroup reports that the permission is not known to be
// denied on the group — the test for withholding something scoped to one
// availability group. See Capabilities.Allows for why unknown must permit.
//
// It is sound in the withholding direction where the other server-scope maps
// are not, because this one is not sparse: every group on a probed instance has
// a row, so a 0 is an answer rather than a silence. What that 0 cannot tell
// apart is a DENY on the group from a login that holds nothing at this scope at
// all — and the second is already withheld by the server-wide permission a
// caller asks about beside this, so the two collapse to the same decision.
func (c *Capabilities) PermitsOnAvailabilityGroup(group, name string) bool {
	return c.AvailabilityGroupPermission(group, name) != CapabilityDenied
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
	vq, vargs := explicitServerCapabilityQuery(len(args)+1, ProbedServerSecurablePermissions)
	q = capabilityServerPrincipalCTE + q + "\nUNION ALL\n" + vq
	args = append(args, vargs...)

	gq, gargs := availabilityGroupCapabilityQuery(len(args)+1, ProbedAvailabilityGroupPermissions)
	q += "\nUNION ALL\n" + gq
	args = append(args, gargs...)

	rows, err := s.query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("gosmo: read server capabilities: %w", err)
	}
	defer rows.Close()

	c := &Capabilities{
		ServerRoles:                  map[string]bool{},
		ServerPermissions:            map[string]CapabilityState{},
		ExplicitServerPermissions:    map[string]map[string]CapabilityState{},
		AvailabilityGroupPermissions: map[string]map[string]CapabilityState{},
	}
	if err := scanCapabilityRows(rows, capabilityDest{
		roles:              c.ServerRoles,
		perms:              c.ServerPermissions,
		explicitServer:     c.ExplicitServerPermissions,
		availabilityGroups: c.AvailabilityGroupPermissions,
	}); err != nil {
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
			Roles:                        map[string]bool{},
			Permissions:                  map[string]CapabilityState{},
			SchemaPermissions:            map[string]map[string]CapabilityState{},
			ExplicitSchemaPermissions:    map[string]map[string]CapabilityState{},
			ExplicitDatabasePermissions:  map[string]CapabilityState{},
			ExplicitPrincipalPermissions: map[string]map[string]CapabilityState{},
			ObjectPermissions:            map[string]map[string]CapabilityState{},
			ColumnPermissions:            map[string]map[string]CapabilityState{},
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

	rows, err := d.query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("gosmo: read capabilities for database %q: %w", d.name, err)
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
	}); err != nil {
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
//   - minor_id splits the two scopes rather than filtering one away. A
//     column-scope row shares class 1 with the object-level ones, so folding
//     it in reports a grant on one column as a grant on the table and a DENY
//     on one column as a DENY on all of them; dropping it instead leaves the
//     wider grant to answer for a column SQL Server refuses. Column rows are
//     tagged "C:" and keyed "schema.object.column" — see
//     DatabaseCapabilities.ColumnPermissions.
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
	  AND OBJECT_NAME(p.major_id) IS NOT NULL
UNION ALL
	SELECT CONCAT('C:', n.v), CONCAT(OBJECT_SCHEMA_NAME(p.major_id), '.', OBJECT_NAME(p.major_id),
	                                 '.', COL_NAME(p.major_id, p.minor_id)),
	       CASE WHEN p.state IN ('D') THEN 0 ELSE 1 END
	FROM sys.database_permissions AS p CROSS JOIN (VALUES ` + valuesList(first, len(perms)) + `) AS n(v)
	WHERE p.class = 1 AND p.minor_id > 0
	  AND p.grantee_principal_id IN (SELECT id FROM cap_me)
	  AND p.permission_name IN (n.v, 'CONTROL')
	  AND OBJECT_NAME(p.major_id) IS NOT NULL
	  AND COL_NAME(p.major_id, p.minor_id) IS NOT NULL`, args
}

// explicitSchemaCapabilityQuery builds the SCHEMA-scope catalog block: one row
// per schema the login has an explicit permission recorded on, tagged
// "E:<permission>" with the schema as the name and 1 for held, 0 for denied.
//
// It is a catalog read beside the HAS_PERMS_BY_NAME block schemaCapabilityQuery
// already asks, not a replacement for it, and the two answer different
// questions: HAS_PERMS_BY_NAME returns 0 for a permission never granted just as
// it does for one explicitly denied, so only the catalog can say a DENY row
// exists. That distinction is the whole reason for this block — a schema-scope
// DENY overrides a database-wide GRANT, so a gate reading the wider grant alone
// offers a write the server then refuses with Msg 297.
//
// class 3 is SCHEMA, and major_id is the schema_id. As in the object block the
// principal set is the recursive cap_me CTE, CONTROL is matched alongside the
// permission asked about because it implies it, and a schema hidden by
// metadata visibility comes back with a NULL name and is dropped.
func explicitSchemaCapabilityQuery(first int, perms []string) (string, []any) {
	args := make([]any, len(perms))
	for i, n := range perms {
		args[i] = n
	}
	return `SELECT CONCAT('E:', n.v), SCHEMA_NAME(p.major_id),
	       CASE WHEN p.state IN ('D') THEN 0 ELSE 1 END
	FROM sys.database_permissions AS p CROSS JOIN (VALUES ` + valuesList(first, len(perms)) + `) AS n(v)
	WHERE p.class = 3
	  AND p.grantee_principal_id IN (SELECT id FROM cap_me)
	  AND p.permission_name IN (n.v, 'CONTROL')
	  AND SCHEMA_NAME(p.major_id) IS NOT NULL`, args
}

// explicitDatabaseCapabilityQuery builds the DATABASE-scope catalog block: one
// row per probed permission the login has an explicit DENY recorded for at
// class 0, tagged "D:<permission>" with the database as the name and 0 for
// denied.
//
// Only DENY rows are selected, which is what separates it from the schema
// block it is otherwise modelled on. The grant direction is already answered
// by the HAS_PERMS_BY_NAME block, which folds in role membership and covering
// permissions; the catalog can add nothing there. What only the catalog can
// say is that a DENY row exists, and a database-scope DENY overrides an
// object- or schema-scope GRANT — see
// DatabaseCapabilities.ExplicitDatabasePermissions.
//
// class 0 is DATABASE, where major_id is 0 and names nothing, so DB_NAME()
// stands in as the securable. CONTROL is deliberately not matched alongside
// the permission: DENY CONTROL at this scope denies CONNECT with it, so the
// login cannot open the database and never reaches this map.
func explicitDatabaseCapabilityQuery(first int, perms []string) (string, []any) {
	args := make([]any, len(perms))
	for i, n := range perms {
		args[i] = n
	}
	return `SELECT CONCAT('D:', n.v), DB_NAME(), 0
	FROM sys.database_permissions AS p CROSS JOIN (VALUES ` + valuesList(first, len(perms)) + `) AS n(v)
	WHERE p.class = 0
	  AND p.state = 'D'
	  AND p.grantee_principal_id IN (SELECT id FROM cap_me)
	  AND p.permission_name = n.v`, args
}

// explicitPrincipalCapabilityQuery builds the DATABASE_PRINCIPAL-scope (class 4)
// catalog block: one row per principal the login has an explicit DENY recorded
// on, tagged "N:<permission>" with the principal's name and 0 for denied.
//
// Only DENY rows are selected, as in the database block, and here the grant
// direction does not merely add nothing — it exists nowhere. GRANT ALTER ON
// USER::x reads HAS_PERMS_BY_NAME 1 and still permits neither the rename nor
// the drop, because both statements require ALTER ANY USER at database scope;
// see ProbedPrincipalPermissions for the live result.
//
// class 4 is DATABASE_PRINCIPAL and major_id is the principal_id. USER_NAME
// resolves it for a database role as well as for a user — verified live on
// majors 13, 14 and 17 — so both kinds land in one map, which is what
// DatabaseCapabilities.ExplicitPrincipalPermissions warns its callers about.
//
// CONTROL *is* matched alongside the permission asked about, unlike the
// database block: DENY CONTROL ON USER::x is recorded under its own
// permission_name and withholds the same two statements, and denying CONTROL
// on one principal does not lock the login out of the database the way DENY
// CONTROL at class 0 does.
//
// A principal hidden by metadata visibility would come back with a NULL name
// and is dropped, as in the schema and object blocks. Class 4 does not in fact
// suppress visibility — a user carrying a DENY stays listed, verified live —
// but the guard costs nothing and the block should not depend on that.
func explicitPrincipalCapabilityQuery(first int, perms []string) (string, []any) {
	args := make([]any, len(perms))
	for i, n := range perms {
		args[i] = n
	}
	return `SELECT CONCAT('N:', n.v), USER_NAME(p.major_id), 0
	FROM sys.database_permissions AS p CROSS JOIN (VALUES ` + valuesList(first, len(perms)) + `) AS n(v)
	WHERE p.class = 4
	  AND p.state = 'D'
	  AND p.grantee_principal_id IN (SELECT id FROM cap_me)
	  AND p.permission_name IN (n.v, 'CONTROL')
	  AND USER_NAME(p.major_id) IS NOT NULL`, args
}

// explicitServerCapabilityQuery builds the server-scope catalog block: one row
// per server securable the login has an explicit DENY recorded on, tagged
// "V:<permission>" with the securable as "<kind>::<name>" and 0 for denied.
//
// It is the only block of the server probe that is not a HAS_PERMS_BY_NAME
// question, and it has to be, for explicitSchemaCapabilityQuery's reason one
// scope wider: HAS_PERMS_BY_NAME reads 0 for an ALTER denied on a login and
// for one never granted alike — and never granted is the ordinary state of a
// login working through the server-wide ALTER ANY LOGIN — so only the catalog
// can say a DENY row exists.
//
// Only DENY rows are selected, as in the database and class-4 blocks. The
// grant direction is already answered by the HAS_PERMS_BY_NAME block, which
// folds in role membership and covering permissions.
//
// Two classes, one block. **There is no class 110**: logins and server roles
// are both class 101 SERVER_PRINCIPAL, told apart here by the principal's
// type_desc, exactly as class 4 tells a user from a database role. major_id is
// the principal_id at class 101 and the endpoint_id at class 105, which is why
// the two halves join to different catalog views and cannot be merged.
//
// CONTROL is matched alongside the permission asked about, as at class 4 and
// unlike the database block: DENY CONTROL on a login withholds the same
// statements and is recorded under its own permission_name, and denying
// CONTROL on one securable does not lock the login out of the server.
//
// The joins are inner, which drops a securable hidden by metadata visibility
// the way the NULL-name guards do in the database blocks. Such a securable is
// equally invisible in any listing built from the same catalog, so there is
// nothing for the answer to gate.
func explicitServerCapabilityQuery(first int, perms []string) (string, []any) {
	args := make([]any, len(perms))
	for i, n := range perms {
		args[i] = n
	}
	return `SELECT CONCAT('V:', n.v),
	       CONCAT(CASE sp.type_desc WHEN 'SERVER_ROLE' THEN 'SERVER ROLE' ELSE 'LOGIN' END, '::', sp.name), 0
	FROM sys.server_permissions AS p
	JOIN sys.server_principals AS sp ON sp.principal_id = p.major_id
	CROSS JOIN (VALUES ` + valuesList(first, len(perms)) + `) AS n(v)
	WHERE p.class = 101
	  AND p.state = 'D'
	  AND p.grantee_principal_id IN (SELECT id FROM cap_srv_me)
	  AND p.permission_name IN (n.v, 'CONTROL')
UNION ALL
	SELECT CONCAT('V:', n.v), CONCAT('ENDPOINT::', e.name), 0
	FROM sys.server_permissions AS p
	JOIN sys.endpoints AS e ON e.endpoint_id = p.major_id
	CROSS JOIN (VALUES ` + valuesList(first, len(perms)) + `) AS n(v)
	WHERE p.class = 105
	  AND p.state = 'D'
	  AND p.grantee_principal_id IN (SELECT id FROM cap_srv_me)
	  AND p.permission_name IN (n.v, 'CONTROL')`, args
}

// availabilityGroupCapabilityQuery builds the AVAILABILITY GROUP-scope (class
// 108) block: one row per availability group on the instance per probed
// permission, asked of every group in one pass rather than a query per group.
//
// Alone among the server-scope blocks this asks HAS_PERMS_BY_NAME rather than
// reading sys.server_permissions, and it has to — see
// ProbedAvailabilityGroupPermissions, where the reason and the live result are
// recorded.
//
// The permission travels in the *kind* column and the group in the name
// column, schemaCapabilityQuery's arrangement and for its reason: a permission
// name is ours and fixed, a group name is user data, and a group called "P"
// would otherwise be read back as a server-scope permission answer.
//
// sys.availability_groups exists on every edition and is empty when HADR is
// off, so this block costs one empty scan on an instance with no groups rather
// than needing a version or feature gate.
func availabilityGroupCapabilityQuery(first int, perms []string) (string, []any) {
	args := make([]any, len(perms))
	for i, n := range perms {
		args[i] = n
	}
	return "SELECT CONCAT('G:', n.v), ag.name, HAS_PERMS_BY_NAME(QUOTENAME(ag.name), 'AVAILABILITY GROUP', n.v)" +
		" FROM sys.availability_groups AS ag CROSS JOIN (VALUES " + valuesList(first, len(perms)) + ") AS n(v)", args
}

// capabilityServerPrincipalCTE is capabilityPrincipalCTE at server scope: the
// login itself, public, and every server role reachable through role
// membership at any depth.
//
// It is a separate CTE rather than the same one parameterised because every
// name in it differs — SUSER_ID for DATABASE_PRINCIPAL_ID,
// sys.server_role_members for sys.database_role_members — and because public
// has no SUSER_ID-style accessor at server scope and has to be looked up by
// name.
const capabilityServerPrincipalCTE = `WITH cap_srv_me AS (
	SELECT SUSER_ID() AS id
	UNION ALL SELECT principal_id FROM sys.server_principals WHERE name = 'public'
	UNION ALL
	SELECT rm.role_principal_id FROM sys.server_role_members AS rm
	JOIN cap_srv_me ON rm.member_principal_id = cap_srv_me.id
)
`

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

// capabilityDest is the set of maps scanCapabilityRows fills, one per block the
// probe can send.
//
// It is a struct rather than eight parameters because the server probe shares
// the scanner and asks for two of the blocks: it passed six nils in a row, and
// a seventh added in the wrong position would have been silently accepted. A
// nil field still means "this probe did not ask for that block", and its rows
// are dropped.
type capabilityDest struct {
	roles              map[string]bool
	perms              map[string]CapabilityState
	explicitServer     map[string]map[string]CapabilityState
	availabilityGroups map[string]map[string]CapabilityState
	schemas            map[string]map[string]CapabilityState
	explicitSchemas    map[string]map[string]CapabilityState
	explicitDB         map[string]CapabilityState
	explicitPrincipals map[string]map[string]CapabilityState
	objects            map[string]map[string]CapabilityState
	columns            map[string]map[string]CapabilityState
}

// scanCapabilityRows fills into from the probe's (kind, name, answer) rows. A
// NULL answer is left out of the map entirely, which is what makes it read back
// as CapabilityUnknown / false.
func scanCapabilityRows(rows *sql.Rows, into capabilityDest) error {
	for rows.Next() {
		var kind, name string
		var answer sql.NullInt64
		if err := rows.Scan(&kind, &name, &answer); err != nil {
			return err
		}
		switch {
		case kind == "R":
			if into.roles != nil {
				into.roles[name] = answer.Valid && answer.Int64 == 1
			}
		case kind == "P":
			if st, ok := capabilityStateOf(answer); ok && into.perms != nil {
				into.perms[name] = st
			}
		case strings.HasPrefix(kind, "S:"):
			// name is the schema here, and the permission rides in kind — see
			// schemaCapabilityQuery. A server probe passes a nil map and drops
			// these rows, which it never asks for.
			st, ok := capabilityStateOf(answer)
			if !ok || into.schemas == nil {
				continue
			}
			if into.schemas[name] == nil {
				into.schemas[name] = map[string]CapabilityState{}
			}
			into.schemas[name][strings.TrimPrefix(kind, "S:")] = st
		case strings.HasPrefix(kind, "E:"):
			// The schema catalog block, keyed by schema name. Kept apart from
			// the "S:" rows because those answer HAS_PERMS_BY_NAME, whose 0
			// cannot tell a DENY from a permission never granted — see
			// DatabaseCapabilities.ExplicitSchemaPermissions.
			if st, ok := capabilityStateOf(answer); ok {
				recordSecurableState(into.explicitSchemas, name, strings.TrimPrefix(kind, "E:"), st)
			}
		case strings.HasPrefix(kind, "D:"):
			// The database catalog block. name is DB_NAME() and is not read —
			// there is one database per probe — while the permission rides in
			// kind, as in every other catalog block. A server probe passes a
			// nil map and drops these rows, which it never asks for. Only DENY
			// rows are selected, so the state is always CapabilityDenied; it
			// is read back through capabilityStateOf anyway so a query change
			// that starts selecting grants is recorded rather than mislabelled.
			st, ok := capabilityStateOf(answer)
			if !ok || into.explicitDB == nil {
				continue
			}
			// A denial wins however the rows are ordered — recordSecurableState's
			// reasoning, one map shallower: the block can produce a row through
			// the login and another through a role it is in.
			perm := strings.TrimPrefix(kind, "D:")
			if into.explicitDB[perm] != CapabilityDenied {
				into.explicitDB[perm] = st
			}
		case strings.HasPrefix(kind, "N:"):
			// The class-4 catalog block, keyed by the principal's name. It is
			// "N:" rather than "P:" because "P" alone already tags the
			// HAS_PERMS_BY_NAME database-scope rows. Only DENY rows are
			// selected — see DatabaseCapabilities.ExplicitPrincipalPermissions
			// — and they are read back through capabilityStateOf anyway so a
			// query change that starts selecting grants is recorded rather
			// than mislabelled.
			if st, ok := capabilityStateOf(answer); ok {
				recordSecurableState(into.explicitPrincipals, name, strings.TrimPrefix(kind, "N:"), st)
			}
		case strings.HasPrefix(kind, "G:"):
			// The availability-group block, keyed by the group's name, with
			// the permission in kind as in every other per-securable block. It
			// is a HAS_PERMS_BY_NAME answer rather than a catalog row, so it
			// goes through capabilityStateOf for the same reason the "P" rows
			// do: a NULL is "this instance does not define the permission",
			// not a denial.
			st, ok := capabilityStateOf(answer)
			if !ok || into.availabilityGroups == nil {
				continue
			}
			if into.availabilityGroups[name] == nil {
				into.availabilityGroups[name] = map[string]CapabilityState{}
			}
			into.availabilityGroups[name][strings.TrimPrefix(kind, "G:")] = st
		case strings.HasPrefix(kind, "V:"):
			// The server-scope catalog block, keyed "<kind>::<name>" — see
			// ServerSecurableKey. It is the one catalog block the *server*
			// probe asks for and the database probe does not, so the database
			// probe passes a nil map and drops these rows. Only DENY rows are
			// selected — see Capabilities.ExplicitServerPermissions — and they
			// are read back through capabilityStateOf anyway so a query change
			// that starts selecting grants is recorded rather than mislabelled.
			if st, ok := capabilityStateOf(answer); ok {
				recordSecurableState(into.explicitServer, name, strings.TrimPrefix(kind, "V:"), st)
			}
		case strings.HasPrefix(kind, "O:"):
			// As with "S:", name is the securable and the permission rides in
			// kind.
			if st, ok := capabilityStateOf(answer); ok {
				recordSecurableState(into.objects, name, strings.TrimPrefix(kind, "O:"), st)
			}
		case strings.HasPrefix(kind, "C:"):
			// The column block, keyed "schema.object.column". It is kept apart
			// from the object map because a column row answers for the column
			// alone — see DatabaseCapabilities.ColumnPermissions.
			if st, ok := capabilityStateOf(answer); ok {
				recordSecurableState(into.columns, name, strings.TrimPrefix(kind, "C:"), st)
			}
		}
	}
	return rows.Err()
}

// recordSecurableState files one object- or column-scope answer under its
// securable, into a map a server probe passes as nil and so drops.
//
// A denial wins over a grant however the two rows are ordered: SQL Server
// resolves DENY over GRANT, and the catalog block can produce both rows for
// one securable — a grant to a role the login is in, a deny to the login
// itself.
func recordSecurableState(m map[string]map[string]CapabilityState, key, perm string, st CapabilityState) {
	if m == nil {
		return
	}
	if m[key] == nil {
		m[key] = map[string]CapabilityState{}
	}
	if m[key][perm] == CapabilityDenied {
		return
	}
	m[key][perm] = st
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
