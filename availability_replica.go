package gosmo

// availability_replica.go is one replica of an availability group: what it is
// and how it is read, the ALTER AVAILABILITY GROUP ... MODIFY REPLICA settings
// that change it, and the add/remove that change which replicas the group has.
// The group itself is in availability_group.go, and the role each statement
// must be run from is its § Operations preamble.

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// -- Replica reads ------------------------------------------------------------------

// AvailabilityReplica is one replica's configuration plus, where the local
// instance can see it, its current state.
//
// The state fields (Role, OperationalState, ConnectedState, RecoveryHealth,
// SynchronizationHealth) come from sys.dm_hadr_availability_replica_states and
// are empty for a replica this instance has no state row for. OperationalState
// in particular is only ever populated for the local replica — SQL Server does
// not report a remote replica's operational state — so an empty value there is
// normal rather than a fault.
type AvailabilityReplica struct {
	server *Server

	GroupID string

	// GroupName is the owning group's name. ALTER AVAILABILITY GROUP addresses
	// a replica as "<group> MODIFY REPLICA ON '<replica>'", so every setter on
	// this type needs it; it is filled in by Replicas.
	GroupName string

	ReplicaID         string
	ReplicaServerName string
	EndpointURL       string

	AvailabilityMode AvailabilityMode
	FailoverMode     FailoverMode
	SessionTimeout   int

	PrimaryRoleAllowConnections   AllowConnections
	SecondaryRoleAllowConnections AllowConnections

	BackupPriority     int
	ReadOnlyRoutingURL string

	// SeedingMode is SQL Server 2016+; empty on older.
	SeedingMode SeedingMode

	CreateDate time.Time
	ModifyDate time.Time

	IsLocal               bool
	Role                  string
	OperationalState      string
	ConnectedState        string
	RecoveryHealth        string
	SynchronizationHealth string

	LastConnectErrorNumber      int
	LastConnectErrorDescription string
	LastConnectErrorTimestamp   time.Time
}

// Server returns the server the replica belongs to.
func (r *AvailabilityReplica) Server() *Server { return r.server }

// Replicas returns every replica in the group, ordered by server name.
func (ag *AvailabilityGroup) Replicas(ctx context.Context) ([]*AvailabilityReplica, error) {
	s := ag.server

	major := s.serverMajorVersion()
	seedingMode := "ISNULL(ar.seeding_mode_desc,'')"
	if major < int(SQLServer2016) {
		seedingMode = "CAST('' AS nvarchar(60))"
	}

	q := `
	SELECT CONVERT(varchar(36), ar.group_id), CONVERT(varchar(36), ar.replica_id),
	       ar.replica_server_name, ISNULL(ar.endpoint_url,''),
	       ISNULL(ar.availability_mode_desc,''), ISNULL(ar.failover_mode_desc,''),
	       ar.session_timeout,
	       ISNULL(ar.primary_role_allow_connections_desc,''),
	       ISNULL(ar.secondary_role_allow_connections_desc,''),
	       ar.backup_priority, ISNULL(ar.read_only_routing_url,''),
	       ` + seedingMode + `,
	       ar.create_date, ar.modify_date,
	       ISNULL(rs.is_local, 0),
	       ISNULL(rs.role_desc,''), ISNULL(rs.operational_state_desc,''),
	       ISNULL(rs.connected_state_desc,''), ISNULL(rs.recovery_health_desc,''),
	       ISNULL(rs.synchronization_health_desc,''),
	       ISNULL(rs.last_connect_error_number, 0),
	       ISNULL(rs.last_connect_error_description,''),
	       rs.last_connect_error_timestamp
	FROM sys.availability_replicas ar
	LEFT JOIN sys.dm_hadr_availability_replica_states rs ON rs.replica_id = ar.replica_id
	WHERE ar.group_id = @p1
	ORDER BY ar.replica_server_name`

	rows, err := s.query(ctx, q, ag.ID)
	return scanRows(rows, err, fmt.Sprintf("list replicas of availability group %q", ag.Name), func(scan func(...any) error) (*AvailabilityReplica, error) {
		r := &AvailabilityReplica{server: s, GroupName: ag.Name}
		// create_date/modify_date are NULL on a replica this instance holds
		// only as cluster metadata — every row on a secondary, in practice.
		var created, modified, lastErrTime sql.NullTime
		if err := scan(
			&r.GroupID, &r.ReplicaID, &r.ReplicaServerName, &r.EndpointURL,
			&r.AvailabilityMode, &r.FailoverMode, &r.SessionTimeout,
			&r.PrimaryRoleAllowConnections, &r.SecondaryRoleAllowConnections,
			&r.BackupPriority, &r.ReadOnlyRoutingURL, &r.SeedingMode,
			&created, &modified,
			&r.IsLocal, &r.Role, &r.OperationalState, &r.ConnectedState,
			&r.RecoveryHealth, &r.SynchronizationHealth,
			&r.LastConnectErrorNumber, &r.LastConnectErrorDescription, &lastErrTime,
		); err != nil {
			return nil, err
		}
		if created.Valid {
			r.CreateDate = created.Time
		}
		if modified.Valid {
			r.ModifyDate = modified.Time
		}
		if lastErrTime.Valid {
			r.LastConnectErrorTimestamp = lastErrTime.Time
		}
		return r, nil
	})
}

// ReadOnlyRoutingList returns the read-only routing list this replica uses
// while it holds the primary role: the secondaries read-intent connections are
// redirected to, in priority order.
//
// The outer slice is the priority order; each inner slice holds the replicas
// sharing one priority, which SQL Server load-balances between (2016+). A
// replica with no routing list configured returns nil, not an error.
func (r *AvailabilityReplica) ReadOnlyRoutingList(ctx context.Context) ([][]string, error) {
	const q = `
	SELECT rl.routing_priority, tgt.replica_server_name
	FROM sys.availability_read_only_routing_lists rl
	JOIN sys.availability_replicas tgt ON tgt.replica_id = rl.read_only_replica_id
	WHERE rl.replica_id = @p1
	ORDER BY rl.routing_priority, tgt.replica_server_name`

	rows, err := r.server.query(ctx, q, r.ReplicaID)
	if err != nil {
		return nil, fmt.Errorf("gosmo: read read-only routing list of replica %q: %w", r.ReplicaServerName, err)
	}
	defer rows.Close()

	var list [][]string
	lastPriority := -1
	for rows.Next() {
		var priority int
		var name string
		if err := rows.Scan(&priority, &name); err != nil {
			return nil, fmt.Errorf("gosmo: read read-only routing list of replica %q: %w", r.ReplicaServerName, err)
		}
		// Equal priorities are one load-balanced set, so a new group starts only
		// when the priority changes — the rows are ordered by it above.
		if priority != lastPriority {
			list = append(list, nil)
			lastPriority = priority
		}
		list[len(list)-1] = append(list[len(list)-1], name)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("gosmo: read read-only routing list of replica %q: %w", r.ReplicaServerName, err)
	}
	return list, nil
}

// -- Replica settings ------------------------------------------------------
//
// Like the group settings above, every setter here runs one ALTER AVAILABILITY
// GROUP ... MODIFY REPLICA statement against the primary — including when the
// replica being modified is a secondary.

// The keywords ALTER MODIFY REPLICA and CREATE AVAILABILITY GROUP accept,
// spelled as the matching *_desc column reports them so a value read off a
// replica can be handed straight back.

// AvailabilityMode is a replica's AVAILABILITY_MODE.
type AvailabilityMode string

const (
	AvailabilitySynchronousCommit  AvailabilityMode = "SYNCHRONOUS_COMMIT"
	AvailabilityAsynchronousCommit AvailabilityMode = "ASYNCHRONOUS_COMMIT"
	AvailabilityConfigurationOnly  AvailabilityMode = "CONFIGURATION_ONLY"
)

// FailoverMode is a replica's FAILOVER_MODE.
type FailoverMode string

const (
	FailoverAutomatic FailoverMode = "AUTOMATIC"
	FailoverManual    FailoverMode = "MANUAL"
	// FailoverExternal is the only mode a group with ClusterTypeExternal
	// accepts, since the cluster manager owns failover there.
	FailoverExternal FailoverMode = "EXTERNAL"
)

// SeedingMode is a replica's SEEDING_MODE (SQL Server 2016+).
type SeedingMode string

const (
	SeedingAutomatic SeedingMode = "AUTOMATIC" // direct seeding
	SeedingManual    SeedingMode = "MANUAL"    // backup and restore
)

// AllowConnections is a replica role's ALLOW_CONNECTIONS. The primary role
// takes ReadWrite or All; the secondary role takes No, ReadOnly or All.
type AllowConnections string

const (
	AllowConnectionsAll       AllowConnections = "ALL"
	AllowConnectionsReadWrite AllowConnections = "READ_WRITE"
	AllowConnectionsReadOnly  AllowConnections = "READ_ONLY"
	AllowConnectionsNo        AllowConnections = "NO"
)

// The types' validity checks. The keywords are spliced into DDL, so a value
// outside the constants — a conversion from an arbitrary string — is refused.
var (
	availabilityModes = map[AvailabilityMode]bool{
		AvailabilitySynchronousCommit: true, AvailabilityAsynchronousCommit: true, AvailabilityConfigurationOnly: true,
	}
	failoverModes = map[FailoverMode]bool{FailoverAutomatic: true, FailoverManual: true, FailoverExternal: true}
	seedingModes  = map[SeedingMode]bool{SeedingAutomatic: true, SeedingManual: true}
	// The primary role has no NO: a primary that accepts no connections would
	// be unusable.
	primaryRoleConnections   = map[AllowConnections]bool{AllowConnectionsReadWrite: true, AllowConnectionsAll: true}
	secondaryRoleConnections = map[AllowConnections]bool{
		AllowConnectionsNo: true, AllowConnectionsReadOnly: true, AllowConnectionsAll: true,
	}
)

// upperKeyword upper-cases a keyword value, so a lower-case spelling converted
// from a string is accepted as it always was.
func upperKeyword[K ~string](v K) K { return K(strings.ToUpper(string(v))) }

// modifyReplica runs one ALTER AVAILABILITY GROUP ... MODIFY REPLICA ON ...
// WITH (<with>) statement.
//
// The replica is addressed by name as a string literal, not an identifier —
// that is the syntax ALTER requires — so escapeSingle is what protects it.
func (r *AvailabilityReplica) modifyReplica(ctx context.Context, with string) error {
	if r.server == nil || r.GroupName == "" {
		return fmt.Errorf("gosmo: modify replica %q: replica did not come from AvailabilityGroup.Replicas", r.ReplicaServerName)
	}
	return r.server.exec(ctx, fmt.Sprintf(
		"ALTER AVAILABILITY GROUP %s MODIFY REPLICA ON N'%s' WITH (%s)",
		quoteIdent(r.GroupName), escapeSingle(r.ReplicaServerName), with))
}

// setReplicaKeyword is the shared body of the keyword-valued replica setters:
// validate against a closed set, run the ALTER, mirror the new value back.
func setReplicaKeyword[K ~string](ctx context.Context, r *AvailabilityReplica, what, option string, value K, allowed map[K]bool, dst *K) error {
	value = upperKeyword(value)
	if !allowed[value] {
		return fmt.Errorf("gosmo: set %s: unrecognized value %q", what, value)
	}
	if err := r.modifyReplica(ctx, option+" = "+string(value)); err != nil {
		return fmt.Errorf("gosmo: set %s of replica %q: %w", what, r.ReplicaServerName, err)
	}
	setIfApplied(ctx, dst, value)
	return nil
}

// SetAvailabilityMode switches the replica between SYNCHRONOUS_COMMIT,
// ASYNCHRONOUS_COMMIT and CONFIGURATION_ONLY.
//
// Only a synchronous-commit replica can be an automatic failover target, so
// dropping one to asynchronous also silently removes it as a candidate.
func (r *AvailabilityReplica) SetAvailabilityMode(ctx context.Context, mode AvailabilityMode) error {
	return setReplicaKeyword(ctx, r, "availability mode", "AVAILABILITY_MODE", mode, availabilityModes, &r.AvailabilityMode)
}

// SetFailoverMode switches the replica between AUTOMATIC, MANUAL and EXTERNAL
// failover. EXTERNAL is the only mode a group with ClusterType EXTERNAL
// accepts, since the cluster manager owns failover there.
func (r *AvailabilityReplica) SetFailoverMode(ctx context.Context, mode FailoverMode) error {
	return setReplicaKeyword(ctx, r, "failover mode", "FAILOVER_MODE", mode, failoverModes, &r.FailoverMode)
}

// SetSeedingMode switches the replica between AUTOMATIC (direct seeding) and
// MANUAL (backup and restore) database seeding. SQL Server 2016+.
func (r *AvailabilityReplica) SetSeedingMode(ctx context.Context, mode SeedingMode) error {
	return setReplicaKeyword(ctx, r, "seeding mode", "SEEDING_MODE", mode, seedingModes, &r.SeedingMode)
}

// SetPrimaryRoleAllowConnections sets which connections the replica accepts
// while it is the primary: ALL, or READ_WRITE (which turns away connections
// asking for ApplicationIntent=ReadOnly).
func (r *AvailabilityReplica) SetPrimaryRoleAllowConnections(ctx context.Context, mode AllowConnections) error {
	mode = upperKeyword(mode)
	if !primaryRoleConnections[mode] {
		return fmt.Errorf("gosmo: set primary role connections: unrecognized value %q", mode)
	}
	if err := r.modifyReplica(ctx, "PRIMARY_ROLE (ALLOW_CONNECTIONS = "+string(mode)+")"); err != nil {
		return fmt.Errorf("gosmo: set primary role connections of replica %q: %w", r.ReplicaServerName, err)
	}
	setIfApplied(ctx, &r.PrimaryRoleAllowConnections, mode)
	return nil
}

// SetSecondaryRoleAllowConnections sets whether the replica is readable while
// it is a secondary: NO, READ_ONLY (read-intent connections only) or ALL.
func (r *AvailabilityReplica) SetSecondaryRoleAllowConnections(ctx context.Context, mode AllowConnections) error {
	mode = upperKeyword(mode)
	if !secondaryRoleConnections[mode] {
		return fmt.Errorf("gosmo: set secondary role connections: unrecognized value %q", mode)
	}
	if err := r.modifyReplica(ctx, "SECONDARY_ROLE (ALLOW_CONNECTIONS = "+string(mode)+")"); err != nil {
		return fmt.Errorf("gosmo: set secondary role connections of replica %q: %w", r.ReplicaServerName, err)
	}
	setIfApplied(ctx, &r.SecondaryRoleAllowConnections, mode)
	return nil
}

// SetSessionTimeout sets how many seconds a replica waits for a message from
// its partner before reporting the connection down. SQL Server enforces a
// 5-second floor; below about 10 seconds a busy system reports false failures.
func (r *AvailabilityReplica) SetSessionTimeout(ctx context.Context, seconds int) error {
	if seconds < 5 {
		return fmt.Errorf("gosmo: set session timeout: %d s is below the 5 s minimum", seconds)
	}
	if err := r.modifyReplica(ctx, fmt.Sprintf("SESSION_TIMEOUT = %d", seconds)); err != nil {
		return fmt.Errorf("gosmo: set session timeout of replica %q: %w", r.ReplicaServerName, err)
	}
	setIfApplied(ctx, &r.SessionTimeout, seconds)
	return nil
}

// SetBackupPriority sets this replica's automated-backup priority, 1 (lowest)
// to 100 (highest). 0 excludes the replica from automated backups altogether —
// the value behind SSMS's "Exclude Replica" checkbox.
func (r *AvailabilityReplica) SetBackupPriority(ctx context.Context, priority int) error {
	if priority < 0 || priority > 100 {
		return fmt.Errorf("gosmo: set backup priority: %d out of range 0-100", priority)
	}
	if err := r.modifyReplica(ctx, fmt.Sprintf("BACKUP_PRIORITY = %d", priority)); err != nil {
		return fmt.Errorf("gosmo: set backup priority of replica %q: %w", r.ReplicaServerName, err)
	}
	setIfApplied(ctx, &r.BackupPriority, priority)
	return nil
}

// SetReadOnlyRoutingURL sets the address read-intent connections are
// redirected to when this replica is a readable secondary, e.g.
// "TCP://ubusql2.example.com:1433". An empty url clears it.
//
// The URL is a property of the *secondary* role, while the routing list that
// points at it is a property of the primary role — see SetReadOnlyRoutingList.
// Both have to be set for read-only routing to work.
//
// Clearing writes the bare keyword NONE, matching the routing list. Neither
// NULL nor an empty string works: NULL is a syntax error and N” is rejected
// as "Invalid usage of the option READ_ONLY_ROUTING_URL" — both verified
// against SQL Server 2025.
func (r *AvailabilityReplica) SetReadOnlyRoutingURL(ctx context.Context, url string) error {
	value := "NONE"
	if url != "" {
		value = QuoteLiteral(url)
	}
	if err := r.modifyReplica(ctx, "SECONDARY_ROLE (READ_ONLY_ROUTING_URL = "+value+")"); err != nil {
		return fmt.Errorf("gosmo: set read-only routing URL of replica %q: %w", r.ReplicaServerName, err)
	}
	setIfApplied(ctx, &r.ReadOnlyRoutingURL, url)
	return nil
}

// SetReadOnlyRoutingList sets the routing list this replica uses while it holds
// the primary role, in the shape ReadOnlyRoutingList returns: the outer
// slice is priority order, and replicas sharing an inner slice are
// load-balanced between (SQL Server 2016+). An empty list clears the routing
// list.
func (r *AvailabilityReplica) SetReadOnlyRoutingList(ctx context.Context, list [][]string) error {
	value, err := formatRoutingList(list)
	if err != nil {
		return fmt.Errorf("gosmo: set read-only routing list of replica %q: %w", r.ReplicaServerName, err)
	}
	if err := r.modifyReplica(ctx, "PRIMARY_ROLE (READ_ONLY_ROUTING_LIST = "+value+")"); err != nil {
		return fmt.Errorf("gosmo: set read-only routing list of replica %q: %w", r.ReplicaServerName, err)
	}
	return nil
}

// formatRoutingList renders a routing list as the READ_ONLY_ROUTING_LIST
// right-hand side: NONE when empty, otherwise a parenthesised priority
// sequence whose load-balanced sets are parenthesised again —
// ((N'a',N'b'),N'c'). Empty inner slices are dropped rather than emitted as
// "()", which is a syntax error.
func formatRoutingList(list [][]string) (string, error) {
	groups := make([]string, 0, len(list))
	for _, set := range list {
		names := make([]string, 0, len(set))
		for _, name := range set {
			if strings.TrimSpace(name) == "" {
				return "", fmt.Errorf("routing list contains an empty replica name")
			}
			names = append(names, QuoteLiteral(name))
		}
		switch len(names) {
		case 0:
		case 1:
			groups = append(groups, names[0])
		default:
			groups = append(groups, "("+strings.Join(names, ", ")+")")
		}
	}
	if len(groups) == 0 {
		return "NONE", nil
	}
	return "(" + strings.Join(groups, ", ") + ")", nil
}

// -- Replica operations --------------------------------------------------------------

// addReplicaClause builds the ADD REPLICA clause from a replica spec — the same
// WITH body CREATE AVAILABILITY GROUP's REPLICA ON list uses, which is why it
// goes through AvailabilityReplicaSpec rather than a second set of arguments.
func addReplicaClause(spec AvailabilityReplicaSpec) (string, error) {
	with, err := spec.withClause()
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("ADD REPLICA ON %s WITH (%s)", QuoteLiteral(spec.ServerName), with), nil
}

// AddReplica adds a secondary replica to an existing availability group.
//
// This is the first of three statements, and on its own it leaves the new
// replica disconnected — exactly as CreateAvailabilityGroup does for the
// replicas it names. Run this against the primary, then, connected to the new
// replica itself:
//
//  1. Join, which is the only way the replica actually enters the group; and
//  2. GrantCreateAnyDatabase, if the spec seeds AUTOMATIC, without which
//     seeding silently copies nothing.
//
// The replica must already have a started database mirroring endpoint that the
// other replicas can reach, and its FailoverMode has to match what the group's
// cluster type permits — EXTERNAL requires EXTERNAL, NONE requires MANUAL.
func (ag *AvailabilityGroup) AddReplica(ctx context.Context, spec AvailabilityReplicaSpec) error {
	clause, err := addReplicaClause(spec)
	if err != nil {
		return fmt.Errorf("gosmo: add replica to availability group %q: %w", ag.Name, err)
	}
	if err := ag.alter(ctx, clause); err != nil {
		return fmt.Errorf("gosmo: add replica %q to availability group %q: %w", spec.ServerName, ag.Name, err)
	}
	return nil
}

// removeReplicaClause builds the REMOVE REPLICA clause. The replica is named as
// a string literal, not an identifier, which is why escapeSingle rather than
// quoteIdent guards it — the same asymmetry as MODIFY REPLICA ON.
func removeReplicaClause(serverName string) string {
	return fmt.Sprintf("REMOVE REPLICA ON N'%s'", escapeSingle(serverName))
}

// RemoveReplica removes a secondary replica from the group.
//
// Run against the primary; a secondary cannot remove itself, and rejects the
// attempt with error 41190.
//
// The removed instance is not cleaned up by this: it keeps both its copies of
// the databases and a stale row for the group in its own
// sys.availability_groups, which only DROP AVAILABILITY GROUP run *there*
// clears. Removing a replica and then dropping the group on the primary
// therefore still leaves the group listed on the instance that was removed —
// verified against SQL Server 2025.
func (ag *AvailabilityGroup) RemoveReplica(ctx context.Context, serverName string) error {
	if strings.TrimSpace(serverName) == "" {
		return fmt.Errorf("gosmo: remove replica from availability group %q: empty replica name", ag.Name)
	}
	if err := ag.alter(ctx, removeReplicaClause(serverName)); err != nil {
		return fmt.Errorf("gosmo: remove replica %q from availability group %q: %w", serverName, ag.Name, err)
	}
	return nil
}

// Drop removes this replica from its availability group — the same statement
// AvailabilityGroup.RemoveReplica issues, addressed from the replica instead.
// Run against the primary.
func (r *AvailabilityReplica) Drop(ctx context.Context) error {
	if r.server == nil || r.GroupName == "" {
		return fmt.Errorf("gosmo: drop replica %q: replica did not come from AvailabilityGroup.Replicas", r.ReplicaServerName)
	}
	if err := r.server.exec(ctx, fmt.Sprintf("ALTER AVAILABILITY GROUP %s %s",
		quoteIdent(r.GroupName), removeReplicaClause(r.ReplicaServerName))); err != nil {
		return fmt.Errorf("gosmo: remove replica %q from availability group %q: %w", r.ReplicaServerName, r.GroupName, err)
	}
	return nil
}
