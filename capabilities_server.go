package gosmo

// capabilities_server.go is the server-scoped half of the probe: which fixed
// server roles the login is in, which server permissions it holds, and the
// per-securable answers for a login, server role, endpoint or availability
// group. What is asked is capabilities.go's Probed* lists.

import (
	"context"
	"fmt"
)

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
