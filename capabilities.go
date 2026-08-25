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
	"SELECT",
	"INSERT",
	"UPDATE",
	"DELETE",
	"EXECUTE",
	"SHOWPLAN",
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
	if err := scanCapabilityRows(rows, c.ServerRoles, c.ServerPermissions); err != nil {
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

// Allows reports that the permission is not known to be denied — the test for
// withholding something. See Capabilities.Allows, which explains why the two
// are not opposites.
func (c *DatabaseCapabilities) Allows(name string) bool {
	return c.Permission(name) != CapabilityDenied
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
			Roles:       map[string]bool{},
			Permissions: map[string]CapabilityState{},
		}, nil
	}

	q, args := capabilityQuery(
		"SELECT 'R', n.v, IS_ROLEMEMBER(n.v)", ProbedDatabaseRoles,
		"SELECT 'P', n.v, HAS_PERMS_BY_NAME(DB_NAME(), 'DATABASE', n.v)", ProbedDatabasePermissions,
	)

	rows, err := d.query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("gosmo: read capabilities for database %q: %w", d.name, err)
	}
	defer rows.Close()

	c := &DatabaseCapabilities{
		Accessible:  true,
		Roles:       map[string]bool{},
		Permissions: map[string]CapabilityState{},
	}
	if err := scanCapabilityRows(rows.Rows, c.Roles, c.Permissions); err != nil {
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

// valuesClause renders " FROM (VALUES (@pN),(@pN+1),...) AS n(v)" for count
// parameters starting at first.
func valuesClause(first, count int) string {
	var b strings.Builder
	b.WriteString(" FROM (VALUES ")
	for i := range count {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, "(@p%d)", first+i)
	}
	b.WriteString(") AS n(v)")
	return b.String()
}

// scanCapabilityRows fills roles and perms from the probe's (kind, name,
// answer) rows. A NULL answer is left out of the map entirely, which is what
// makes it read back as CapabilityUnknown / false.
func scanCapabilityRows(rows *sql.Rows, roles map[string]bool, perms map[string]CapabilityState) error {
	for rows.Next() {
		var kind, name string
		var answer sql.NullInt64
		if err := rows.Scan(&kind, &name, &answer); err != nil {
			return err
		}
		switch kind {
		case "R":
			roles[name] = answer.Valid && answer.Int64 == 1
		case "P":
			if !answer.Valid {
				continue
			}
			if answer.Int64 == 1 {
				perms[name] = CapabilityGranted
			} else {
				perms[name] = CapabilityDenied
			}
		}
	}
	return rows.Err()
}
