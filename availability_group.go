package gosmo

// availability_group.go models Always On availability groups: the group
// itself, its replicas, the per-database synchronization state, and the
// listeners clients connect through.
//
// Everything here is readable from any replica, but not everything means the
// same thing on every replica. sys.availability_groups and
// sys.availability_replicas are cluster-wide metadata and agree everywhere;
// the sys.dm_hadr_* DMVs describe what *this* instance can currently see, and
// a secondary routinely reports less than the primary does — most visibly,
// per-database queue sizes and commit times are only populated for databases
// the local instance actually hosts. Callers that need the full picture should
// read from the primary; AvailabilityGroup.PrimaryReplicaServerName and
// IsLocalPrimary exist so they can tell where they are and follow it.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// AvailabilityGroup represents one Always On availability group, as seen from
// the instance the owning Server is connected to.
//
// The fields sourced from sys.dm_hadr_availability_group_states
// (PrimaryReplicaServerName, PrimaryRecoveryHealth, SynchronizationHealth) are
// empty when the local instance has no state row for the group — which happens
// while the group is resolving, or on an instance that has joined but not yet
// connected. An empty PrimaryReplicaServerName means "unknown from here", not
// "no primary exists".
type AvailabilityGroup struct {
	server *Server

	ID              string
	Name            string
	ResourceID      string
	ResourceGroupID string

	// ClusterType is WSFC, EXTERNAL or NONE. It is empty before SQL Server
	// 2017, which had no cluster_type column and only ever meant WSFC.
	// Upper-cased by agColumns, which is what makes those spellings true —
	// SQL Server reports this one in lower case.
	//
	// This is the field that decides whether a failover can be performed
	// through T-SQL at all: under EXTERNAL the cluster manager owns failover
	// and SQL Server rejects both ALTER AVAILABILITY GROUP ... FAILOVER and
	// ... FORCE_FAILOVER_ALLOW_DATA_LOSS with error 47104.
	ClusterType string

	AutomatedBackupPreference string
	FailureConditionLevel     int
	HealthCheckTimeout        int
	Version                   int

	// BasicFeatures reports a Basic availability group (Standard edition):
	// one database, two replicas, no readable secondary. SQL Server 2016+.
	BasicFeatures bool
	DTCSupport    bool
	DBFailover    bool
	IsDistributed bool

	// RequiredSynchronizedSecondariesToCommit is SQL Server 2017+; it is 0 on
	// older versions, which is also a legitimate value, so it cannot be used
	// to detect support.
	RequiredSynchronizedSecondariesToCommit int

	// IsContained reports a contained availability group, which carries its
	// own master and msdb. SQL Server 2022+.
	IsContained bool

	PrimaryReplicaServerName string
	PrimaryRecoveryHealth    string
	SynchronizationHealth    string
}

// IsLocalPrimary reports whether the instance this group was read from is
// currently the group's primary replica. False when the primary is elsewhere
// *or* unknown from here, so it is safe to branch on but not to invert:
// !IsLocalPrimary() does not prove a remote primary exists.
func (ag *AvailabilityGroup) IsLocalPrimary() bool {
	if ag.PrimaryReplicaServerName == "" || ag.server == nil {
		return false
	}
	return strings.EqualFold(ag.PrimaryReplicaServerName, ag.server.Name())
}

// Server returns the connection this availability group was read from.
func (ag *AvailabilityGroup) Server() *Server { return ag.server }

// agColumns builds the sys.availability_groups select list for the connected
// server's version. Columns added after 2012 are substituted with typed
// literals rather than omitted, so the scan destination list stays fixed and
// callers get a zero value instead of a "column not found" error on an older
// instance.
func (s *Server) agColumns() string {
	major := 0
	if info := s.Info(); info != nil {
		major = info.VersionMajor
	}

	// 2016 added the basic/DTC/failover/distributed flags.
	basicFeatures, dtcSupport, dbFailover, isDistributed := "ag.basic_features", "ag.dtc_support", "ag.db_failover", "ag.is_distributed"
	if major < int(SQLServer2016) {
		basicFeatures, dtcSupport, dbFailover, isDistributed = "CAST(0 AS bit)", "CAST(0 AS bit)", "CAST(0 AS bit)", "CAST(0 AS bit)"
	}

	// 2017 added the external-cluster support this whole type keys off.
	//
	// UPPER is not cosmetic: sys.availability_groups reports cluster_type_desc
	// and automated_backup_preference_desc in *lower* case ("external",
	// "secondary") while every sys.availability_replicas *_desc column is upper
	// case. Without it the values documented on these fields are not the values
	// they hold, and the obvious ClusterType == "EXTERNAL" test silently never
	// fires. Verified against SQL Server 2025.
	clusterType, requiredSync := "UPPER(ISNULL(ag.cluster_type_desc,''))", "ag.required_synchronized_secondaries_to_commit"
	if major < int(SQLServer2017) {
		clusterType, requiredSync = "CAST('' AS nvarchar(60))", "CAST(0 AS int)"
	}

	// 2022 added contained availability groups.
	isContained := "ag.is_contained"
	if major < int(SQLServer2022) {
		isContained = "CAST(0 AS bit)"
	}

	return strings.Join([]string{
		"CONVERT(varchar(36), ag.group_id)",
		"ag.name",
		"ISNULL(CONVERT(varchar(36), ag.resource_id),'')",
		"ISNULL(CONVERT(varchar(36), ag.resource_group_id),'')",
		clusterType,
		"UPPER(ISNULL(ag.automated_backup_preference_desc,''))", // lower case as reported — see clusterType above
		"ag.failure_condition_level",
		"ag.health_check_timeout",
		"ag.version",
		basicFeatures, dtcSupport, dbFailover, isDistributed,
		requiredSync, isContained,
		"ISNULL(gs.primary_replica,'')",
		"ISNULL(gs.primary_recovery_health_desc,'')",
		"ISNULL(gs.synchronization_health_desc,'')",
	}, ", ")
}

// scanAvailabilityGroup reads one row of agColumns' select list.
func (s *Server) scanAvailabilityGroup(scan func(...any) error) (*AvailabilityGroup, error) {
	ag := &AvailabilityGroup{server: s}
	if err := scan(
		&ag.ID, &ag.Name, &ag.ResourceID, &ag.ResourceGroupID,
		&ag.ClusterType, &ag.AutomatedBackupPreference,
		&ag.FailureConditionLevel, &ag.HealthCheckTimeout, &ag.Version,
		&ag.BasicFeatures, &ag.DTCSupport, &ag.DBFailover, &ag.IsDistributed,
		&ag.RequiredSynchronizedSecondariesToCommit, &ag.IsContained,
		&ag.PrimaryReplicaServerName, &ag.PrimaryRecoveryHealth, &ag.SynchronizationHealth,
	); err != nil {
		return nil, err
	}
	return ag, nil
}

// AvailabilityGroups returns every availability group this instance
// participates in. Returns an empty slice — not an error — on an instance
// where Always On is disabled or no group has been created.
func (s *Server) AvailabilityGroups() ([]*AvailabilityGroup, error) {
	return s.AvailabilityGroupsContext(context.Background())
}

// AvailabilityGroupsContext is the context-aware variant of AvailabilityGroups.
func (s *Server) AvailabilityGroupsContext(ctx context.Context) ([]*AvailabilityGroup, error) {
	q := `
	SELECT ` + s.agColumns() + `
	FROM sys.availability_groups ag
	LEFT JOIN sys.dm_hadr_availability_group_states gs ON gs.group_id = ag.group_id
	ORDER BY ag.name`

	rows, err := s.query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("gosmo: list availability groups: %w", err)
	}
	defer rows.Close()

	var groups []*AvailabilityGroup
	for rows.Next() {
		ag, err := s.scanAvailabilityGroup(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("gosmo: list availability groups: %w", err)
		}
		groups = append(groups, ag)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("gosmo: list availability groups: %w", err)
	}
	return groups, nil
}

// AvailabilityGroup returns a lightweight handle to an availability group by
// name, with no query and no metadata: every field but Name is zero, so
// ClusterType, PrimaryReplicaServerName and IsLocalPrimary all read as
// "unknown" rather than as fact. Use AvailabilityGroupByName to get a group
// whose fields mean something.
//
// This exists for the one case where the group cannot be read: a secondary of
// an EXTERNAL- or NONE-cluster group has no row for it until Join succeeds, so
// the join has to be issued against a handle built from the name alone. It is
// also what works under a WithScript-derived context, where nothing has been
// created yet to read back. The same split as Server.Database vs
// Server.DatabaseByName.
func (s *Server) AvailabilityGroup(name string) *AvailabilityGroup {
	return &AvailabilityGroup{server: s, Name: name}
}

// AvailabilityGroupByName returns one availability group by name, or an error
// wrapping ErrNotFound if this instance knows no group by that name. That
// error also satisfies errors.Is(err, sql.ErrNoRows), which this method
// promised before ErrNotFound existed. Note that neither sentinel was ever
// returned bare — both have always needed errors.Is rather than ==.
func (s *Server) AvailabilityGroupByName(name string) (*AvailabilityGroup, error) {
	return s.AvailabilityGroupByNameContext(context.Background(), name)
}

// AvailabilityGroupByNameContext is the context-aware variant of
// AvailabilityGroupByName.
func (s *Server) AvailabilityGroupByNameContext(ctx context.Context, name string) (*AvailabilityGroup, error) {
	q := `
	SELECT ` + s.agColumns() + `
	FROM sys.availability_groups ag
	LEFT JOIN sys.dm_hadr_availability_group_states gs ON gs.group_id = ag.group_id
	WHERE ag.name = @p1`

	var ag *AvailabilityGroup
	err := s.queryRow(ctx, func(row *sql.Row) error {
		var scanErr error
		ag, scanErr = s.scanAvailabilityGroup(row.Scan)
		return scanErr
	}, q, name)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, notFoundfAlso(sql.ErrNoRows, "gosmo: availability group %q not found", name)
		}
		return nil, fmt.Errorf("gosmo: availability group %q: %w", name, err)
	}
	return ag, nil
}

// -- Replicas ------------------------------------------------------------------

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
	// this type needs it; it is filled in by ReplicasContext.
	GroupName string

	ReplicaID         string
	ReplicaServerName string
	EndpointURL       string

	AvailabilityMode string
	FailoverMode     string
	SessionTimeout   int

	PrimaryRoleAllowConnections   string
	SecondaryRoleAllowConnections string

	BackupPriority     int
	ReadOnlyRoutingURL string

	// SeedingMode is AUTOMATIC or MANUAL. SQL Server 2016+; empty on older.
	SeedingMode string

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

// Replicas returns every replica in the group, ordered by server name.
func (ag *AvailabilityGroup) Replicas() ([]*AvailabilityReplica, error) {
	return ag.ReplicasContext(context.Background())
}

// ReplicasContext is the context-aware variant of Replicas.
func (ag *AvailabilityGroup) ReplicasContext(ctx context.Context) ([]*AvailabilityReplica, error) {
	s := ag.server

	major := 0
	if info := s.Info(); info != nil {
		major = info.VersionMajor
	}
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
	if err != nil {
		return nil, fmt.Errorf("gosmo: list replicas of availability group %q: %w", ag.Name, err)
	}
	defer rows.Close()

	var replicas []*AvailabilityReplica
	for rows.Next() {
		r := &AvailabilityReplica{server: s, GroupName: ag.Name}
		// create_date/modify_date are NULL on a replica this instance holds
		// only as cluster metadata — every row on a secondary, in practice.
		var created, modified, lastErrTime sql.NullTime
		if err := rows.Scan(
			&r.GroupID, &r.ReplicaID, &r.ReplicaServerName, &r.EndpointURL,
			&r.AvailabilityMode, &r.FailoverMode, &r.SessionTimeout,
			&r.PrimaryRoleAllowConnections, &r.SecondaryRoleAllowConnections,
			&r.BackupPriority, &r.ReadOnlyRoutingURL, &r.SeedingMode,
			&created, &modified,
			&r.IsLocal, &r.Role, &r.OperationalState, &r.ConnectedState,
			&r.RecoveryHealth, &r.SynchronizationHealth,
			&r.LastConnectErrorNumber, &r.LastConnectErrorDescription, &lastErrTime,
		); err != nil {
			return nil, fmt.Errorf("gosmo: list replicas of availability group %q: %w", ag.Name, err)
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
		replicas = append(replicas, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("gosmo: list replicas of availability group %q: %w", ag.Name, err)
	}
	return replicas, nil
}

// ReadOnlyRoutingList returns the read-only routing list this replica uses
// while it holds the primary role: the secondaries read-intent connections are
// redirected to, in priority order.
//
// The outer slice is the priority order; each inner slice holds the replicas
// sharing one priority, which SQL Server load-balances between (2016+). A
// replica with no routing list configured returns nil, not an error.
func (r *AvailabilityReplica) ReadOnlyRoutingList() ([][]string, error) {
	return r.ReadOnlyRoutingListContext(context.Background())
}

// ReadOnlyRoutingListContext is the context-aware variant of
// ReadOnlyRoutingList.
func (r *AvailabilityReplica) ReadOnlyRoutingListContext(ctx context.Context) ([][]string, error) {
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

// -- Databases -----------------------------------------------------------------

// AvailabilityDatabase is one database's synchronization state on one replica.
// A group with two databases and three replicas yields six rows, of which the
// local instance can normally only populate the queue/rate/LSN detail for its
// own.
//
// The queue and rate figures are in kilobytes and kilobytes per second, as SQL
// Server reports them; they are left at zero for a replica whose state this
// instance cannot see.
type AvailabilityDatabase struct {
	GroupID           string
	ReplicaID         string
	ReplicaServerName string
	DatabaseName      string
	GroupDatabaseID   string

	IsLocal          bool
	IsPrimaryReplica bool

	SynchronizationState  string
	SynchronizationHealth string
	DatabaseState         string

	IsSuspended   bool
	SuspendReason string

	LogSendQueueKB  int64
	LogSendRateKBps int64
	RedoQueueKB     int64
	RedoRateKBps    int64

	// SecondaryLagSeconds is how far this secondary trails the primary.
	// SQL Server 2016+; 0 on older versions and on the primary itself.
	SecondaryLagSeconds int64

	LastSentTime     time.Time
	LastReceivedTime time.Time
	LastHardenedTime time.Time
	LastRedoneTime   time.Time
	LastCommitTime   time.Time
}

// Databases returns the per-replica synchronization state of every database in
// the group.
//
// The database list comes from sys.availability_databases_cluster, which is
// cluster-wide metadata, so a database appears even on a replica that has not
// finished seeding it — with empty state rather than being silently missing.
func (ag *AvailabilityGroup) Databases() ([]*AvailabilityDatabase, error) {
	return ag.DatabasesContext(context.Background())
}

// DatabasesContext is the context-aware variant of Databases.
func (ag *AvailabilityGroup) DatabasesContext(ctx context.Context) ([]*AvailabilityDatabase, error) {
	s := ag.server

	major := 0
	if info := s.Info(); info != nil {
		major = info.VersionMajor
	}
	lag := "ISNULL(drs.secondary_lag_seconds, 0)"
	if major < int(SQLServer2016) {
		lag = "CAST(0 AS bigint)"
	}

	// Cross-joining the cluster-wide database list with the replica list is
	// what makes a not-yet-seeded database show up as an empty row instead of
	// vanishing: dm_hadr_database_replica_states only has rows for databases a
	// replica has actually materialised.
	q := `
	SELECT CONVERT(varchar(36), adc.group_id), CONVERT(varchar(36), ar.replica_id),
	       ar.replica_server_name, adc.database_name,
	       CONVERT(varchar(36), adc.group_database_id),
	       ISNULL(drs.is_local, 0), ISNULL(drs.is_primary_replica, 0),
	       ISNULL(drs.synchronization_state_desc,''),
	       ISNULL(drs.synchronization_health_desc,''),
	       ISNULL(drs.database_state_desc,''),
	       ISNULL(drs.is_suspended, 0), ISNULL(drs.suspend_reason_desc,''),
	       ISNULL(drs.log_send_queue_size, 0), ISNULL(drs.log_send_rate, 0),
	       ISNULL(drs.redo_queue_size, 0), ISNULL(drs.redo_rate, 0),
	       ` + lag + `,
	       drs.last_sent_time, drs.last_received_time,
	       drs.last_hardened_time, drs.last_redone_time, drs.last_commit_time
	FROM sys.availability_databases_cluster adc
	JOIN sys.availability_replicas ar ON ar.group_id = adc.group_id
	LEFT JOIN sys.dm_hadr_database_replica_states drs
	       ON  drs.group_id = adc.group_id
	       AND drs.replica_id = ar.replica_id
	       AND drs.group_database_id = adc.group_database_id
	WHERE adc.group_id = @p1
	ORDER BY adc.database_name, ar.replica_server_name`

	rows, err := s.query(ctx, q, ag.ID)
	if err != nil {
		return nil, fmt.Errorf("gosmo: list databases of availability group %q: %w", ag.Name, err)
	}
	defer rows.Close()

	var dbs []*AvailabilityDatabase
	for rows.Next() {
		d := &AvailabilityDatabase{}
		var sent, received, hardened, redone, commit sql.NullTime
		if err := rows.Scan(
			&d.GroupID, &d.ReplicaID, &d.ReplicaServerName, &d.DatabaseName,
			&d.GroupDatabaseID, &d.IsLocal, &d.IsPrimaryReplica,
			&d.SynchronizationState, &d.SynchronizationHealth, &d.DatabaseState,
			&d.IsSuspended, &d.SuspendReason,
			&d.LogSendQueueKB, &d.LogSendRateKBps, &d.RedoQueueKB, &d.RedoRateKBps,
			&d.SecondaryLagSeconds,
			&sent, &received, &hardened, &redone, &commit,
		); err != nil {
			return nil, fmt.Errorf("gosmo: list databases of availability group %q: %w", ag.Name, err)
		}
		for _, f := range []struct {
			src sql.NullTime
			dst *time.Time
		}{
			{sent, &d.LastSentTime}, {received, &d.LastReceivedTime},
			{hardened, &d.LastHardenedTime}, {redone, &d.LastRedoneTime},
			{commit, &d.LastCommitTime},
		} {
			if f.src.Valid {
				*f.dst = f.src.Time
			}
		}
		dbs = append(dbs, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("gosmo: list databases of availability group %q: %w", ag.Name, err)
	}
	return dbs, nil
}

// -- Listeners -----------------------------------------------------------------

// AvailabilityGroupListener is one virtual network name clients connect to.
// Its addresses are carried inline rather than behind another round trip,
// since a listener without them tells a caller almost nothing.
type AvailabilityGroupListener struct {
	GroupID    string
	ListenerID string
	DNSName    string
	Port       int

	// IsConformant is false for a listener created outside SQL Server (added
	// directly in the cluster manager), whose configuration SQL Server can
	// report but not fully validate.
	IsConformant bool

	IPConfigurationString    string
	IsDistributedNetworkName bool

	IPAddresses []AvailabilityListenerIP
}

// AvailabilityListenerIP is one address bound to a listener. A multi-subnet
// availability group has one per subnet.
type AvailabilityListenerIP struct {
	IPAddress  string
	SubnetMask string
	IsDHCP     bool
	State      string
}

// Listeners returns the group's listeners, each with its IP addresses.
func (ag *AvailabilityGroup) Listeners() ([]*AvailabilityGroupListener, error) {
	return ag.ListenersContext(context.Background())
}

// ListenersContext is the context-aware variant of Listeners.
func (ag *AvailabilityGroup) ListenersContext(ctx context.Context) ([]*AvailabilityGroupListener, error) {
	s := ag.server

	const q = `
	SELECT CONVERT(varchar(36), l.group_id), CONVERT(varchar(36), l.listener_id),
	       ISNULL(l.dns_name,''), ISNULL(l.port, 0), ISNULL(l.is_conformant, 0),
	       ISNULL(l.ip_configuration_string_from_cluster,''),
	       ISNULL(l.is_distributed_network_name, 0)
	FROM sys.availability_group_listeners l
	WHERE l.group_id = @p1
	ORDER BY l.dns_name`

	rows, err := s.query(ctx, q, ag.ID)
	if err != nil {
		return nil, fmt.Errorf("gosmo: list listeners of availability group %q: %w", ag.Name, err)
	}
	defer rows.Close()

	var listeners []*AvailabilityGroupListener
	byID := map[string]*AvailabilityGroupListener{}
	for rows.Next() {
		l := &AvailabilityGroupListener{}
		if err := rows.Scan(
			&l.GroupID, &l.ListenerID, &l.DNSName, &l.Port, &l.IsConformant,
			&l.IPConfigurationString, &l.IsDistributedNetworkName,
		); err != nil {
			return nil, fmt.Errorf("gosmo: list listeners of availability group %q: %w", ag.Name, err)
		}
		listeners = append(listeners, l)
		byID[strings.ToLower(l.ListenerID)] = l
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("gosmo: list listeners of availability group %q: %w", ag.Name, err)
	}
	if len(listeners) == 0 {
		return nil, nil
	}

	if err := ag.attachListenerIPs(ctx, byID); err != nil {
		return nil, err
	}
	return listeners, nil
}

// attachListenerIPs fills in each listener's IPAddresses. Split out so the
// listener scan above closes its rows before this second query runs.
func (ag *AvailabilityGroup) attachListenerIPs(ctx context.Context, byID map[string]*AvailabilityGroupListener) error {
	const q = `
	SELECT CONVERT(varchar(36), ip.listener_id),
	       ISNULL(ip.ip_address,''), ISNULL(ip.ip_subnet_mask,''),
	       ISNULL(ip.is_dhcp, 0), ISNULL(ip.state_desc,'')
	FROM sys.availability_group_listener_ip_addresses ip
	JOIN sys.availability_group_listeners l ON l.listener_id = ip.listener_id
	WHERE l.group_id = @p1`

	rows, err := ag.server.query(ctx, q, ag.ID)
	if err != nil {
		return fmt.Errorf("gosmo: list listener addresses of availability group %q: %w", ag.Name, err)
	}
	defer rows.Close()

	for rows.Next() {
		var listenerID string
		var ip AvailabilityListenerIP
		if err := rows.Scan(&listenerID, &ip.IPAddress, &ip.SubnetMask, &ip.IsDHCP, &ip.State); err != nil {
			return fmt.Errorf("gosmo: list listener addresses of availability group %q: %w", ag.Name, err)
		}
		if l := byID[strings.ToLower(listenerID)]; l != nil {
			l.IPAddresses = append(l.IPAddresses, ip)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("gosmo: list listener addresses of availability group %q: %w", ag.Name, err)
	}
	return nil
}

// -- Group settings --------------------------------------------------------
//
// Every setter below issues one ALTER AVAILABILITY GROUP statement and must be
// run against the primary replica: SQL Server rejects these on a secondary.
// AvailabilityGroup.IsLocalPrimary reports whether the connection this group
// was read from qualifies.
//
// Changing settings is *not* the same as failing over, which is the one
// operation an EXTERNAL (Pacemaker/Corosync-managed) cluster refuses through
// T-SQL — see AvailabilityGroup.ClusterType. Note though that a cluster
// manager may own some of these values and reassert its own: on Linux the
// ocf:mssql:ag resource agent maintains
// REQUIRED_SYNCHRONIZED_SECONDARIES_TO_COMMIT itself.

// alterSet runs one ALTER AVAILABILITY GROUP ... SET (<option>) statement.
func (ag *AvailabilityGroup) alterSet(ctx context.Context, option string) error {
	return ag.server.execContext(ctx,
		fmt.Sprintf("ALTER AVAILABILITY GROUP %s SET (%s)", quoteIdent(ag.Name), option))
}

// backupPreferences is the closed set of AUTOMATED_BACKUP_PREFERENCE values,
// spelled as both ALTER accepts them and automated_backup_preference_desc
// reports them, so a value read off a group round-trips back through the
// setter unchanged.
var backupPreferences = map[string]bool{
	"PRIMARY": true, "SECONDARY_ONLY": true, "SECONDARY": true, "NONE": true,
}

// SetAutomatedBackupPreference chooses where automated backups of this group's
// databases should run: PRIMARY, SECONDARY_ONLY, SECONDARY (prefer a secondary
// but fall back to the primary) or NONE (any replica).
//
// The preference is advisory. SQL Server does not enforce it — it is exposed to
// backup jobs through sys.fn_hadr_backup_is_preferred_replica, which the job
// has to consult.
func (ag *AvailabilityGroup) SetAutomatedBackupPreference(pref string) error {
	return ag.SetAutomatedBackupPreferenceContext(context.Background(), pref)
}

// SetAutomatedBackupPreferenceContext is the context-aware variant of
// SetAutomatedBackupPreference.
func (ag *AvailabilityGroup) SetAutomatedBackupPreferenceContext(ctx context.Context, pref string) error {
	pref = strings.ToUpper(pref)
	if !backupPreferences[pref] {
		return fmt.Errorf("gosmo: set automated backup preference: unrecognized preference %q", pref)
	}
	if err := ag.alterSet(ctx, "AUTOMATED_BACKUP_PREFERENCE = "+pref); err != nil {
		return fmt.Errorf("gosmo: set automated backup preference of availability group %q: %w", ag.Name, err)
	}
	setIfApplied(ctx, &ag.AutomatedBackupPreference, pref)
	return nil
}

// SetFailureConditionLevel sets how severe a condition must be before an
// automatic failover is triggered, 1 (server down only) to 5 (any qualifying
// internal error).
func (ag *AvailabilityGroup) SetFailureConditionLevel(level int) error {
	return ag.SetFailureConditionLevelContext(context.Background(), level)
}

// SetFailureConditionLevelContext is the context-aware variant of
// SetFailureConditionLevel.
func (ag *AvailabilityGroup) SetFailureConditionLevelContext(ctx context.Context, level int) error {
	if level < 1 || level > 5 {
		return fmt.Errorf("gosmo: set failure condition level: level %d out of range 1-5", level)
	}
	if err := ag.alterSet(ctx, fmt.Sprintf("FAILURE_CONDITION_LEVEL = %d", level)); err != nil {
		return fmt.Errorf("gosmo: set failure condition level of availability group %q: %w", ag.Name, err)
	}
	setIfApplied(ctx, &ag.FailureConditionLevel, level)
	return nil
}

// SetHealthCheckTimeout sets how long, in milliseconds, the cluster waits for
// sp_server_diagnostics before declaring the instance unresponsive. SQL Server
// enforces a 15000 ms floor.
func (ag *AvailabilityGroup) SetHealthCheckTimeout(ms int) error {
	return ag.SetHealthCheckTimeoutContext(context.Background(), ms)
}

// SetHealthCheckTimeoutContext is the context-aware variant of
// SetHealthCheckTimeout.
func (ag *AvailabilityGroup) SetHealthCheckTimeoutContext(ctx context.Context, ms int) error {
	if ms < 15000 {
		return fmt.Errorf("gosmo: set health check timeout: %d ms is below the 15000 ms minimum", ms)
	}
	if err := ag.alterSet(ctx, fmt.Sprintf("HEALTH_CHECK_TIMEOUT = %d", ms)); err != nil {
		return fmt.Errorf("gosmo: set health check timeout of availability group %q: %w", ag.Name, err)
	}
	setIfApplied(ctx, &ag.HealthCheckTimeout, ms)
	return nil
}

// SetDBFailover turns database-level health detection on or off: with it on, a
// single database going offline triggers failover of the whole group.
func (ag *AvailabilityGroup) SetDBFailover(on bool) error {
	return ag.SetDBFailoverContext(context.Background(), on)
}

// SetDBFailoverContext is the context-aware variant of SetDBFailover.
func (ag *AvailabilityGroup) SetDBFailoverContext(ctx context.Context, on bool) error {
	if err := ag.alterSet(ctx, "DB_FAILOVER = "+onOffKeyword(on)); err != nil {
		return fmt.Errorf("gosmo: set database level health detection of availability group %q: %w", ag.Name, err)
	}
	setIfApplied(ctx, &ag.DBFailover, on)
	return nil
}

// SetDTCSupport turns per-database DTC support on (PER_DB) or off (NONE).
// SQL Server 2016+.
func (ag *AvailabilityGroup) SetDTCSupport(perDB bool) error {
	return ag.SetDTCSupportContext(context.Background(), perDB)
}

// SetDTCSupportContext is the context-aware variant of SetDTCSupport.
func (ag *AvailabilityGroup) SetDTCSupportContext(ctx context.Context, perDB bool) error {
	value := "NONE"
	if perDB {
		value = "PER_DB"
	}
	if err := ag.alterSet(ctx, "DTC_SUPPORT = "+value); err != nil {
		return fmt.Errorf("gosmo: set DTC support of availability group %q: %w", ag.Name, err)
	}
	setIfApplied(ctx, &ag.DTCSupport, perDB)
	return nil
}

// SetRequiredSynchronizedSecondariesToCommit sets how many synchronous
// secondaries must acknowledge a transaction before it commits on the primary.
// SQL Server 2017+.
//
// Raising it above the number of healthy synchronous secondaries stops the
// primary accepting writes, which is the intended trade for guaranteed
// zero-data-loss failover — it is not a setting to nudge experimentally on a
// live group.
func (ag *AvailabilityGroup) SetRequiredSynchronizedSecondariesToCommit(n int) error {
	return ag.SetRequiredSynchronizedSecondariesToCommitContext(context.Background(), n)
}

// SetRequiredSynchronizedSecondariesToCommitContext is the context-aware
// variant of SetRequiredSynchronizedSecondariesToCommit.
func (ag *AvailabilityGroup) SetRequiredSynchronizedSecondariesToCommitContext(ctx context.Context, n int) error {
	if n < 0 {
		return fmt.Errorf("gosmo: set required synchronized secondaries to commit: %d is negative", n)
	}
	if err := ag.alterSet(ctx, fmt.Sprintf("REQUIRED_SYNCHRONIZED_SECONDARIES_TO_COMMIT = %d", n)); err != nil {
		return fmt.Errorf("gosmo: set required synchronized secondaries to commit of availability group %q: %w", ag.Name, err)
	}
	setIfApplied(ctx, &ag.RequiredSynchronizedSecondariesToCommit, n)
	return nil
}

// -- Replica settings ------------------------------------------------------
//
// Like the group settings above, every setter here runs one ALTER AVAILABILITY
// GROUP ... MODIFY REPLICA statement against the primary — including when the
// replica being modified is a secondary.

// Closed sets of the keywords ALTER MODIFY REPLICA accepts, spelled as the
// matching *_desc column reports them so a value read off a replica can be
// handed straight back.
var (
	availabilityModes = map[string]bool{
		"SYNCHRONOUS_COMMIT": true, "ASYNCHRONOUS_COMMIT": true, "CONFIGURATION_ONLY": true,
	}
	failoverModes = map[string]bool{"AUTOMATIC": true, "MANUAL": true, "EXTERNAL": true}
	seedingModes  = map[string]bool{"AUTOMATIC": true, "MANUAL": true}
	// The primary role has no NO: a primary that accepts no connections would
	// be unusable.
	primaryRoleConnections   = map[string]bool{"READ_WRITE": true, "ALL": true}
	secondaryRoleConnections = map[string]bool{"NO": true, "READ_ONLY": true, "ALL": true}
)

// modifyReplica runs one ALTER AVAILABILITY GROUP ... MODIFY REPLICA ON ...
// WITH (<with>) statement.
//
// The replica is addressed by name as a string literal, not an identifier —
// that is the syntax ALTER requires — so escapeSingle is what protects it.
func (r *AvailabilityReplica) modifyReplica(ctx context.Context, with string) error {
	if r.server == nil || r.GroupName == "" {
		return fmt.Errorf("gosmo: modify replica %q: replica did not come from AvailabilityGroup.Replicas", r.ReplicaServerName)
	}
	return r.server.execContext(ctx, fmt.Sprintf(
		"ALTER AVAILABILITY GROUP %s MODIFY REPLICA ON N'%s' WITH (%s)",
		quoteIdent(r.GroupName), escapeSingle(r.ReplicaServerName), with))
}

// setReplicaKeyword is the shared body of the keyword-valued replica setters:
// validate against a closed set, run the ALTER, mirror the new value back.
func setReplicaKeyword(ctx context.Context, r *AvailabilityReplica, what, option, value string, allowed map[string]bool, dst *string) error {
	value = strings.ToUpper(value)
	if !allowed[value] {
		return fmt.Errorf("gosmo: set %s: unrecognized value %q", what, value)
	}
	if err := r.modifyReplica(ctx, option+" = "+value); err != nil {
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
func (r *AvailabilityReplica) SetAvailabilityMode(mode string) error {
	return r.SetAvailabilityModeContext(context.Background(), mode)
}

// SetAvailabilityModeContext is the context-aware variant of
// SetAvailabilityMode.
func (r *AvailabilityReplica) SetAvailabilityModeContext(ctx context.Context, mode string) error {
	return setReplicaKeyword(ctx, r, "availability mode", "AVAILABILITY_MODE", mode, availabilityModes, &r.AvailabilityMode)
}

// SetFailoverMode switches the replica between AUTOMATIC, MANUAL and EXTERNAL
// failover. EXTERNAL is the only mode a group with ClusterType EXTERNAL
// accepts, since the cluster manager owns failover there.
func (r *AvailabilityReplica) SetFailoverMode(mode string) error {
	return r.SetFailoverModeContext(context.Background(), mode)
}

// SetFailoverModeContext is the context-aware variant of SetFailoverMode.
func (r *AvailabilityReplica) SetFailoverModeContext(ctx context.Context, mode string) error {
	return setReplicaKeyword(ctx, r, "failover mode", "FAILOVER_MODE", mode, failoverModes, &r.FailoverMode)
}

// SetSeedingMode switches the replica between AUTOMATIC (direct seeding) and
// MANUAL (backup and restore) database seeding. SQL Server 2016+.
func (r *AvailabilityReplica) SetSeedingMode(mode string) error {
	return r.SetSeedingModeContext(context.Background(), mode)
}

// SetSeedingModeContext is the context-aware variant of SetSeedingMode.
func (r *AvailabilityReplica) SetSeedingModeContext(ctx context.Context, mode string) error {
	return setReplicaKeyword(ctx, r, "seeding mode", "SEEDING_MODE", mode, seedingModes, &r.SeedingMode)
}

// SetPrimaryRoleAllowConnections sets which connections the replica accepts
// while it is the primary: ALL, or READ_WRITE (which turns away connections
// asking for ApplicationIntent=ReadOnly).
func (r *AvailabilityReplica) SetPrimaryRoleAllowConnections(mode string) error {
	return r.SetPrimaryRoleAllowConnectionsContext(context.Background(), mode)
}

// SetPrimaryRoleAllowConnectionsContext is the context-aware variant of
// SetPrimaryRoleAllowConnections.
func (r *AvailabilityReplica) SetPrimaryRoleAllowConnectionsContext(ctx context.Context, mode string) error {
	mode = strings.ToUpper(mode)
	if !primaryRoleConnections[mode] {
		return fmt.Errorf("gosmo: set primary role connections: unrecognized value %q", mode)
	}
	if err := r.modifyReplica(ctx, "PRIMARY_ROLE (ALLOW_CONNECTIONS = "+mode+")"); err != nil {
		return fmt.Errorf("gosmo: set primary role connections of replica %q: %w", r.ReplicaServerName, err)
	}
	setIfApplied(ctx, &r.PrimaryRoleAllowConnections, mode)
	return nil
}

// SetSecondaryRoleAllowConnections sets whether the replica is readable while
// it is a secondary: NO, READ_ONLY (read-intent connections only) or ALL.
func (r *AvailabilityReplica) SetSecondaryRoleAllowConnections(mode string) error {
	return r.SetSecondaryRoleAllowConnectionsContext(context.Background(), mode)
}

// SetSecondaryRoleAllowConnectionsContext is the context-aware variant of
// SetSecondaryRoleAllowConnections.
func (r *AvailabilityReplica) SetSecondaryRoleAllowConnectionsContext(ctx context.Context, mode string) error {
	mode = strings.ToUpper(mode)
	if !secondaryRoleConnections[mode] {
		return fmt.Errorf("gosmo: set secondary role connections: unrecognized value %q", mode)
	}
	if err := r.modifyReplica(ctx, "SECONDARY_ROLE (ALLOW_CONNECTIONS = "+mode+")"); err != nil {
		return fmt.Errorf("gosmo: set secondary role connections of replica %q: %w", r.ReplicaServerName, err)
	}
	setIfApplied(ctx, &r.SecondaryRoleAllowConnections, mode)
	return nil
}

// SetSessionTimeout sets how many seconds a replica waits for a message from
// its partner before reporting the connection down. SQL Server enforces a
// 5-second floor; below about 10 seconds a busy system reports false failures.
func (r *AvailabilityReplica) SetSessionTimeout(seconds int) error {
	return r.SetSessionTimeoutContext(context.Background(), seconds)
}

// SetSessionTimeoutContext is the context-aware variant of SetSessionTimeout.
func (r *AvailabilityReplica) SetSessionTimeoutContext(ctx context.Context, seconds int) error {
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
func (r *AvailabilityReplica) SetBackupPriority(priority int) error {
	return r.SetBackupPriorityContext(context.Background(), priority)
}

// SetBackupPriorityContext is the context-aware variant of SetBackupPriority.
func (r *AvailabilityReplica) SetBackupPriorityContext(ctx context.Context, priority int) error {
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
func (r *AvailabilityReplica) SetReadOnlyRoutingURL(url string) error {
	return r.SetReadOnlyRoutingURLContext(context.Background(), url)
}

// SetReadOnlyRoutingURLContext is the context-aware variant of
// SetReadOnlyRoutingURL.
func (r *AvailabilityReplica) SetReadOnlyRoutingURLContext(ctx context.Context, url string) error {
	value := "NONE"
	if url != "" {
		value = nStringLiteral(url)
	}
	if err := r.modifyReplica(ctx, "SECONDARY_ROLE (READ_ONLY_ROUTING_URL = "+value+")"); err != nil {
		return fmt.Errorf("gosmo: set read-only routing URL of replica %q: %w", r.ReplicaServerName, err)
	}
	setIfApplied(ctx, &r.ReadOnlyRoutingURL, url)
	return nil
}

// SetReadOnlyRoutingList sets the routing list this replica uses while it holds
// the primary role, in the shape ReadOnlyRoutingListContext returns: the outer
// slice is priority order, and replicas sharing an inner slice are
// load-balanced between (SQL Server 2016+). An empty list clears the routing
// list.
func (r *AvailabilityReplica) SetReadOnlyRoutingList(list [][]string) error {
	return r.SetReadOnlyRoutingListContext(context.Background(), list)
}

// SetReadOnlyRoutingListContext is the context-aware variant of
// SetReadOnlyRoutingList.
func (r *AvailabilityReplica) SetReadOnlyRoutingListContext(ctx context.Context, list [][]string) error {
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
			names = append(names, nStringLiteral(name))
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

// -- Operations ------------------------------------------------------------
//
// The statements below change the *shape* of a group rather than its settings:
// which databases and replicas belong to it, which listener clients reach it
// through, whether it exists at all, and which replica is primary.
//
// Each one has a role it must be run from, and running it from the wrong one
// fails rather than doing something surprising — the doc comment on each says
// which. Two rules cover most of them: membership changes (ADD/REMOVE DATABASE,
// REMOVE REPLICA, ADD/REMOVE LISTENER, DROP) go to the primary, and the
// secondary-side database statements (JoinDatabase, UnjoinDatabase, Suspend,
// Resume) act on the copy held by the instance they are run against.

// alter runs one ALTER AVAILABILITY GROUP <name> <clause> statement.
func (ag *AvailabilityGroup) alter(ctx context.Context, clause string) error {
	return ag.server.execContext(ctx,
		fmt.Sprintf("ALTER AVAILABILITY GROUP %s %s", quoteIdent(ag.Name), clause))
}

// -- Databases in the group ------------------------------------------------

// AddDatabase adds a database on the primary to the availability group.
//
// Run against the primary. The database must already be in the full recovery
// model with a log backup taken; SQL Server rejects it otherwise. What happens
// on the secondaries afterwards depends on their seeding mode: an AUTOMATIC
// replica seeds itself, a MANUAL one needs the database restored there and then
// JoinDatabase called against it.
func (ag *AvailabilityGroup) AddDatabase(name string) error {
	return ag.AddDatabaseContext(context.Background(), name)
}

// AddDatabaseContext is the context-aware variant of AddDatabase.
func (ag *AvailabilityGroup) AddDatabaseContext(ctx context.Context, name string) error {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("gosmo: add database to availability group %q: empty database name", ag.Name)
	}
	if err := ag.alter(ctx, "ADD DATABASE "+quoteIdent(name)); err != nil {
		return fmt.Errorf("gosmo: add database %q to availability group %q: %w", name, ag.Name, err)
	}
	return nil
}

// RemoveDatabase removes a database from the availability group.
//
// Run against the primary, which removes the database from the group
// cluster-wide. The primary's copy stays online and read-write; each secondary
// is left holding a copy that is no longer in any role, so every connection to
// it fails with error 983 until it is dropped or restored WITH RECOVERY.
// sys.databases still reports that copy as ONLINE, so state_desc is not the way
// to find one — verified against SQL Server 2025.
func (ag *AvailabilityGroup) RemoveDatabase(name string) error {
	return ag.RemoveDatabaseContext(context.Background(), name)
}

// RemoveDatabaseContext is the context-aware variant of RemoveDatabase.
func (ag *AvailabilityGroup) RemoveDatabaseContext(ctx context.Context, name string) error {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("gosmo: remove database from availability group %q: empty database name", ag.Name)
	}
	if err := ag.alter(ctx, "REMOVE DATABASE "+quoteIdent(name)); err != nil {
		return fmt.Errorf("gosmo: remove database %q from availability group %q: %w", name, ag.Name, err)
	}
	return nil
}

// -- Databases on a secondary ----------------------------------------------
//
// These four are ALTER DATABASE statements, not ALTER AVAILABILITY GROUP ones,
// and they act on the copy of the database held by the instance this group was
// read from — not on the group as a whole. Which instance the connection points
// at is therefore part of what they mean.

// alterDatabaseHADR runs one ALTER DATABASE <name> SET HADR <clause> statement.
func (ag *AvailabilityGroup) alterDatabaseHADR(ctx context.Context, name, clause string) error {
	return ag.server.execContext(ctx,
		fmt.Sprintf("ALTER DATABASE %s SET HADR %s", quoteIdent(name), clause))
}

// JoinDatabase joins a restored secondary copy of a database to the group.
//
// Run against the secondary holding the copy, after restoring it WITH NORECOVERY
// from a full and a log backup of the primary's. Only needed for a MANUAL-seeding
// replica; an AUTOMATIC one joins itself as part of seeding.
func (ag *AvailabilityGroup) JoinDatabase(name string) error {
	return ag.JoinDatabaseContext(context.Background(), name)
}

// JoinDatabaseContext is the context-aware variant of JoinDatabase.
func (ag *AvailabilityGroup) JoinDatabaseContext(ctx context.Context, name string) error {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("gosmo: join database to availability group %q: empty database name", ag.Name)
	}
	if err := ag.alterDatabaseHADR(ctx, name, "AVAILABILITY GROUP = "+quoteIdent(ag.Name)); err != nil {
		return fmt.Errorf("gosmo: join database %q to availability group %q: %w", name, ag.Name, err)
	}
	return nil
}

// UnjoinDatabase removes this instance's secondary copy of a database from the
// group, leaving it in the RESTORING state.
//
// Run against the secondary. This is the per-secondary counterpart of
// RemoveDatabase: it takes one copy out of the group and leaves the database in
// it on every other replica.
func (ag *AvailabilityGroup) UnjoinDatabase(name string) error {
	return ag.UnjoinDatabaseContext(context.Background(), name)
}

// UnjoinDatabaseContext is the context-aware variant of UnjoinDatabase.
func (ag *AvailabilityGroup) UnjoinDatabaseContext(ctx context.Context, name string) error {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("gosmo: unjoin database from availability group %q: empty database name", ag.Name)
	}
	if err := ag.alterDatabaseHADR(ctx, name, "OFF"); err != nil {
		return fmt.Errorf("gosmo: unjoin database %q from availability group %q: %w", name, ag.Name, err)
	}
	return nil
}

// SuspendDatabase suspends data movement for one database.
//
// The scope depends on where it runs, and the difference matters: on a secondary
// it suspends that one secondary, on the primary it suspends the database on
// *every* secondary. Either way the primary keeps accepting writes and its log
// cannot be truncated while movement is suspended, so a long suspension fills
// the log drive.
func (ag *AvailabilityGroup) SuspendDatabase(name string) error {
	return ag.SuspendDatabaseContext(context.Background(), name)
}

// SuspendDatabaseContext is the context-aware variant of SuspendDatabase.
func (ag *AvailabilityGroup) SuspendDatabaseContext(ctx context.Context, name string) error {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("gosmo: suspend database in availability group %q: empty database name", ag.Name)
	}
	if err := ag.alterDatabaseHADR(ctx, name, "SUSPEND"); err != nil {
		return fmt.Errorf("gosmo: suspend database %q in availability group %q: %w", name, ag.Name, err)
	}
	return nil
}

// ResumeDatabase resumes data movement for one database, on the same scope
// SuspendDatabase used.
func (ag *AvailabilityGroup) ResumeDatabase(name string) error {
	return ag.ResumeDatabaseContext(context.Background(), name)
}

// ResumeDatabaseContext is the context-aware variant of ResumeDatabase.
func (ag *AvailabilityGroup) ResumeDatabaseContext(ctx context.Context, name string) error {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("gosmo: resume database in availability group %q: empty database name", ag.Name)
	}
	if err := ag.alterDatabaseHADR(ctx, name, "RESUME"); err != nil {
		return fmt.Errorf("gosmo: resume database %q in availability group %q: %w", name, ag.Name, err)
	}
	return nil
}

// -- Replicas --------------------------------------------------------------

// addReplicaClause builds the ADD REPLICA clause from a replica spec — the same
// WITH body CREATE AVAILABILITY GROUP's REPLICA ON list uses, which is why it
// goes through AvailabilityReplicaSpec rather than a second set of arguments.
func addReplicaClause(spec AvailabilityReplicaSpec) (string, error) {
	with, err := spec.withClause()
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("ADD REPLICA ON %s WITH (%s)", nStringLiteral(spec.ServerName), with), nil
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
func (ag *AvailabilityGroup) AddReplica(spec AvailabilityReplicaSpec) error {
	return ag.AddReplicaContext(context.Background(), spec)
}

// AddReplicaContext is the context-aware variant of AddReplica.
func (ag *AvailabilityGroup) AddReplicaContext(ctx context.Context, spec AvailabilityReplicaSpec) error {
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
func (ag *AvailabilityGroup) RemoveReplica(serverName string) error {
	return ag.RemoveReplicaContext(context.Background(), serverName)
}

// RemoveReplicaContext is the context-aware variant of RemoveReplica.
func (ag *AvailabilityGroup) RemoveReplicaContext(ctx context.Context, serverName string) error {
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
func (r *AvailabilityReplica) Drop() error {
	return r.DropContext(context.Background())
}

// DropContext is the context-aware variant of Drop.
func (r *AvailabilityReplica) DropContext(ctx context.Context) error {
	if r.server == nil || r.GroupName == "" {
		return fmt.Errorf("gosmo: drop replica %q: replica did not come from AvailabilityGroup.Replicas", r.ReplicaServerName)
	}
	if err := r.server.execContext(ctx, fmt.Sprintf("ALTER AVAILABILITY GROUP %s %s",
		quoteIdent(r.GroupName), removeReplicaClause(r.ReplicaServerName))); err != nil {
		return fmt.Errorf("gosmo: remove replica %q from availability group %q: %w", r.ReplicaServerName, r.GroupName, err)
	}
	return nil
}

// -- Listeners -------------------------------------------------------------

// AvailabilityListenerSpec describes a listener to create with AddListener.
//
// Exactly one addressing mode has to be chosen: either DHCP, or one or more
// static IPAddresses. A group can have only one listener at a time, so adding a
// second is an error (19477) rather than a second name to reach it by.
type AvailabilityListenerSpec struct {
	// DNSName is the virtual network name clients connect to. Required.
	DNSName string

	// Port is the TCP port the listener answers on. Zero omits PORT from the
	// statement, which SQL Server defaults to 1433.
	Port int

	// IPAddresses are the static addresses to bind, one per subnet. An entry
	// with an empty SubnetMask is emitted as an IPv6 address, which takes no
	// mask; an IPv4 entry needs one.
	IPAddresses []AvailabilityListenerIPSpec

	// DHCP requests an address from DHCP instead of binding static ones.
	// Mutually exclusive with IPAddresses.
	DHCP bool

	// DHCPSubnet and DHCPSubnetMask optionally name the subnet to take the
	// address from — WITH DHCP ON (N'network', N'mask'). Both or neither.
	DHCPSubnet     string
	DHCPSubnetMask string
}

// AvailabilityListenerIPSpec is one static address for a listener being created.
type AvailabilityListenerIPSpec struct {
	IPAddress string

	// SubnetMask is required for IPv4 and must be empty for IPv6.
	SubnetMask string
}

// addListenerClause builds the ADD LISTENER clause, validating the spec.
//
// PORT is emitted for both addressing modes. The documented grammar allows it
// only after WITH IP, but SQL Server 2025 parses "WITH DHCP, PORT = n" too, and
// leaving it off a DHCP listener would silently give it 1433.
func (spec AvailabilityListenerSpec) addListenerClause() (string, error) {
	if strings.TrimSpace(spec.DNSName) == "" {
		return "", fmt.Errorf("listener has no DNS name")
	}
	if spec.Port < 0 || spec.Port > 65535 {
		return "", fmt.Errorf("listener port %d out of range 1-65535", spec.Port)
	}
	if spec.DHCP && len(spec.IPAddresses) > 0 {
		return "", fmt.Errorf("listener %q asks for both DHCP and static addresses", spec.DNSName)
	}
	if !spec.DHCP && len(spec.IPAddresses) == 0 {
		return "", fmt.Errorf("listener %q has neither DHCP nor a static address", spec.DNSName)
	}

	var with string
	if spec.DHCP {
		with = "WITH DHCP"
		switch {
		case spec.DHCPSubnet != "" && spec.DHCPSubnetMask != "":
			with += fmt.Sprintf(" ON (%s, %s)", nStringLiteral(spec.DHCPSubnet), nStringLiteral(spec.DHCPSubnetMask))
		case spec.DHCPSubnet != "" || spec.DHCPSubnetMask != "":
			return "", fmt.Errorf("listener %q gives only one half of the DHCP subnet and mask", spec.DNSName)
		}
	} else {
		addrs := make([]string, 0, len(spec.IPAddresses))
		for _, ip := range spec.IPAddresses {
			addr, err := listenerIPLiteral(ip)
			if err != nil {
				return "", fmt.Errorf("listener %q has an empty IP address", spec.DNSName)
			}
			addrs = append(addrs, addr)
		}
		with = "WITH IP (" + strings.Join(addrs, ", ") + ")"
	}
	if spec.Port > 0 {
		with += fmt.Sprintf(", PORT = %d", spec.Port)
	}
	return fmt.Sprintf("ADD LISTENER %s (%s)", nStringLiteral(spec.DNSName), with), nil
}

// AddListener creates the group's listener. Run against the primary.
//
// Under an EXTERNAL cluster type this records the listener in SQL Server's own
// metadata only — the address itself belongs to the external cluster manager
// (on Linux, a Pacemaker IPaddr2 resource), which has to be configured
// separately for clients to actually reach it. Unlike failover, the statement
// itself is accepted.
func (ag *AvailabilityGroup) AddListener(spec AvailabilityListenerSpec) error {
	return ag.AddListenerContext(context.Background(), spec)
}

// AddListenerContext is the context-aware variant of AddListener.
func (ag *AvailabilityGroup) AddListenerContext(ctx context.Context, spec AvailabilityListenerSpec) error {
	clause, err := spec.addListenerClause()
	if err != nil {
		return fmt.Errorf("gosmo: add listener to availability group %q: %w", ag.Name, err)
	}
	if err := ag.alter(ctx, clause); err != nil {
		return fmt.Errorf("gosmo: add listener %q to availability group %q: %w", spec.DNSName, ag.Name, err)
	}
	return nil
}

// modifyListenerClause builds a MODIFY LISTENER clause around one option.
//
// The grammar takes exactly one option per statement — a single MODIFY LISTENER
// cannot both change the port and add an address — so each caller below builds
// its own statement rather than accumulating a list.
func modifyListenerClause(dnsName, option string) (string, error) {
	if strings.TrimSpace(dnsName) == "" {
		return "", fmt.Errorf("listener has no DNS name")
	}
	return fmt.Sprintf("MODIFY LISTENER %s (%s)", nStringLiteral(dnsName), option), nil
}

// SetListenerPort changes the port an existing listener answers on. Run against
// the primary.
//
// Clients already connected through the old port stay connected; only new
// connections are affected, and any that name the port explicitly will need
// updating.
func (ag *AvailabilityGroup) SetListenerPort(dnsName string, port int) error {
	return ag.SetListenerPortContext(context.Background(), dnsName, port)
}

// SetListenerPortContext is the context-aware variant of SetListenerPort.
func (ag *AvailabilityGroup) SetListenerPortContext(ctx context.Context, dnsName string, port int) error {
	if port < 1 || port > 65535 {
		return fmt.Errorf("gosmo: modify listener %q on availability group %q: port %d out of range 1-65535", dnsName, ag.Name, port)
	}
	clause, err := modifyListenerClause(dnsName, fmt.Sprintf("PORT = %d", port))
	if err != nil {
		return fmt.Errorf("gosmo: modify listener on availability group %q: %w", ag.Name, err)
	}
	if err := ag.alter(ctx, clause); err != nil {
		return fmt.Errorf("gosmo: set listener %q port on availability group %q: %w", dnsName, ag.Name, err)
	}
	return nil
}

// AddListenerIP binds another static address to an existing listener — the
// second and later subnets of a multi-subnet listener. Run against the primary.
//
// A listener created WITH DHCP cannot be given static addresses this way; SQL
// Server rejects the statement.
//
// There is no matching REMOVE IP: an address bound here stays for the life of
// the listener, and correcting one means REMOVE LISTENER and ADD LISTENER.
//
// Under an EXTERNAL cluster type the address is recorded but not brought up —
// it appears in sys.availability_group_listener_ip_addresses as OFFLINE,
// because the external cluster manager owns the address, not SQL Server.
// Verified on SQL Server 2025 under Pacemaker.
func (ag *AvailabilityGroup) AddListenerIP(dnsName string, ip AvailabilityListenerIPSpec) error {
	return ag.AddListenerIPContext(context.Background(), dnsName, ip)
}

// AddListenerIPContext is the context-aware variant of AddListenerIP.
func (ag *AvailabilityGroup) AddListenerIPContext(ctx context.Context, dnsName string, ip AvailabilityListenerIPSpec) error {
	addr, err := listenerIPLiteral(ip)
	if err != nil {
		return fmt.Errorf("gosmo: add an address to listener %q on availability group %q: %w", dnsName, ag.Name, err)
	}
	clause, err := modifyListenerClause(dnsName, "ADD IP "+addr)
	if err != nil {
		return fmt.Errorf("gosmo: modify listener on availability group %q: %w", ag.Name, err)
	}
	if err := ag.alter(ctx, clause); err != nil {
		return fmt.Errorf("gosmo: add address %q to listener %q on availability group %q: %w", ip.IPAddress, dnsName, ag.Name, err)
	}
	return nil
}

// listenerIPLiteral renders one address as the grammar's parenthesised pair,
// or single value for IPv6, which takes no mask.
func listenerIPLiteral(ip AvailabilityListenerIPSpec) (string, error) {
	if strings.TrimSpace(ip.IPAddress) == "" {
		return "", fmt.Errorf("empty IP address")
	}
	if ip.SubnetMask == "" {
		return "(" + nStringLiteral(ip.IPAddress) + ")", nil
	}
	return fmt.Sprintf("(%s, %s)", nStringLiteral(ip.IPAddress), nStringLiteral(ip.SubnetMask)), nil
}

// RemoveListener drops the group's listener by DNS name. Run against the
// primary. Existing connections made through the listener are not dropped.
func (ag *AvailabilityGroup) RemoveListener(dnsName string) error {
	return ag.RemoveListenerContext(context.Background(), dnsName)
}

// RemoveListenerContext is the context-aware variant of RemoveListener.
func (ag *AvailabilityGroup) RemoveListenerContext(ctx context.Context, dnsName string) error {
	if strings.TrimSpace(dnsName) == "" {
		return fmt.Errorf("gosmo: remove listener from availability group %q: empty DNS name", ag.Name)
	}
	if err := ag.alter(ctx, "REMOVE LISTENER "+nStringLiteral(dnsName)); err != nil {
		return fmt.Errorf("gosmo: remove listener %q from availability group %q: %w", dnsName, ag.Name, err)
	}
	return nil
}

// -- The group itself ------------------------------------------------------

// Drop deletes the availability group.
//
// Run against the primary, which drops the group cluster-wide. Running it on a
// secondary instead removes only that replica's participation and leaves the
// group running elsewhere — SQL Server does not warn about the difference, so
// check IsLocalPrimary first if the intent is to delete the group.
//
// The databases survive: the primary's copies stay online and read-write, and
// each secondary is left with the same unusable copies RemoveDatabase leaves
// behind.
func (ag *AvailabilityGroup) Drop() error {
	return ag.DropContext(context.Background())
}

// DropContext is the context-aware variant of Drop.
func (ag *AvailabilityGroup) DropContext(ctx context.Context) error {
	if err := ag.server.execContext(ctx, "DROP AVAILABILITY GROUP "+quoteIdent(ag.Name)); err != nil {
		return fmt.Errorf("gosmo: drop availability group %q: %w", ag.Name, err)
	}
	return nil
}

// Failover makes this replica the primary, without data loss.
//
// Run against the secondary that should become primary — not against the
// current primary. The target must be a synchronous-commit replica in the
// SYNCHRONIZED state; SQL Server refuses otherwise rather than failing over with
// loss.
//
// Whether this works at all is decided by ClusterType, and only WSFC allows it.
// Under EXTERNAL it is rejected with error 47104 ("Use the cluster management
// tools to perform the operation"), because the external cluster manager owns
// failover — Pacemaker's `crm resource move`, not SQL Server. Under NONE it is
// rejected with error 47122, which says only forced failover is supported. Both
// verified against SQL Server 2025. Check ClusterType before offering this; the
// statement is sent and refused, not silently ignored.
func (ag *AvailabilityGroup) Failover() error {
	return ag.FailoverContext(context.Background())
}

// FailoverContext is the context-aware variant of Failover.
func (ag *AvailabilityGroup) FailoverContext(ctx context.Context) error {
	if err := ag.alter(ctx, "FAILOVER"); err != nil {
		return fmt.Errorf("gosmo: fail over availability group %q: %w", ag.Name, err)
	}
	return nil
}

// ForceFailoverAllowDataLoss makes this replica the primary even though it may
// not hold every committed transaction.
//
// Run against the secondary that should become primary. This is the disaster
// path: any transaction the target had not hardened is lost, and every other
// secondary has to be resumed (and may need reseeding) afterwards. Prefer
// Failover wherever the target is SYNCHRONIZED.
//
// Rejected with error 47104 under an EXTERNAL cluster type, exactly as Failover
// is. Under NONE it is the *only* failover there is, which is why a read-scale
// group has no lossless one.
func (ag *AvailabilityGroup) ForceFailoverAllowDataLoss() error {
	return ag.ForceFailoverAllowDataLossContext(context.Background())
}

// ForceFailoverAllowDataLossContext is the context-aware variant of
// ForceFailoverAllowDataLoss.
func (ag *AvailabilityGroup) ForceFailoverAllowDataLossContext(ctx context.Context) error {
	if err := ag.alter(ctx, "FORCE_FAILOVER_ALLOW_DATA_LOSS"); err != nil {
		return fmt.Errorf("gosmo: force fail over availability group %q with data loss: %w", ag.Name, err)
	}
	return nil
}

// -- Creating a group ------------------------------------------------------
//
// Creating an availability group is not one statement, and the parts that are
// not CREATE AVAILABILITY GROUP have to run somewhere else:
//
//  1. every instance needs a started database mirroring endpoint, and each
//     one needs to have granted the others' service accounts CONNECT on it —
//     see endpoint.go;
//  2. CREATE AVAILABILITY GROUP runs on the instance that becomes the primary,
//     and names every replica;
//  3. each secondary then runs ALTER AVAILABILITY GROUP ... JOIN against
//     itself — the primary cannot join them;
//  4. each secondary that will seed automatically also needs
//     ALTER AVAILABILITY GROUP ... GRANT CREATE ANY DATABASE, without which
//     SEEDING_MODE = AUTOMATIC silently seeds nothing.
//
// Only steps 2-4 are here. Step 1 is the endpoint's own business and step 3
// needs a connection per secondary, which is the caller's to open.

// AvailabilityReplicaSpec describes one replica of a group being created.
type AvailabilityReplicaSpec struct {
	// ServerName is the instance name, as @@SERVERNAME reports it there.
	// Required — it is how every later ALTER addresses this replica.
	ServerName string

	// EndpointURL is the replica's database mirroring endpoint address,
	// "tcp://host:port". Required; DatabaseMirroringEndpoint.URL builds it.
	EndpointURL string

	// AvailabilityMode is SYNCHRONOUS_COMMIT, ASYNCHRONOUS_COMMIT or
	// CONFIGURATION_ONLY. Empty means SYNCHRONOUS_COMMIT.
	AvailabilityMode string

	// FailoverMode is MANUAL, AUTOMATIC or EXTERNAL. Empty means MANUAL.
	// EXTERNAL is required — and the only legal value — under
	// CLUSTER_TYPE = EXTERNAL.
	FailoverMode string

	// SeedingMode is AUTOMATIC or MANUAL. Empty omits the clause, which the
	// server defaults to MANUAL.
	SeedingMode string

	// BackupPriority is 0-100; 0 excludes the replica from automated backups.
	// Negative omits the clause, leaving the server's default of 50 — which is
	// why this is not simply "0 means default".
	BackupPriority int

	// SessionTimeout is the seconds a replica waits for a partner before
	// declaring the connection dead. Zero omits the clause (server default 10).
	SessionTimeout int

	// PrimaryRoleAllowConnections is ALL or READ_WRITE; empty omits the clause.
	PrimaryRoleAllowConnections string

	// SecondaryRoleAllowConnections is NO, READ_ONLY or ALL; empty omits it.
	SecondaryRoleAllowConnections string

	// ReadOnlyRoutingURL is this replica's routing address for read-intent
	// redirection, set inside SECONDARY_ROLE. Empty omits it.
	ReadOnlyRoutingURL string
}

// CreateAvailabilityGroupRequest describes an availability group to create.
type CreateAvailabilityGroupRequest struct {
	// Name is the group's name. Required.
	Name string

	// ClusterType is WSFC, EXTERNAL or NONE. Empty omits the clause, which
	// means WSFC — and fails on an instance with no Windows cluster under it,
	// so Linux callers must set this.
	ClusterType string

	// AutomatedBackupPreference is PRIMARY, SECONDARY_ONLY, SECONDARY or NONE.
	// Empty omits the clause.
	AutomatedBackupPreference string

	// FailureConditionLevel is 1-5; zero omits the clause.
	FailureConditionLevel int

	// HealthCheckTimeout is milliseconds; zero omits the clause.
	HealthCheckTimeout int

	// DBFailover turns on database-level health detection.
	DBFailover bool

	// DTCSupport requests DTC_SUPPORT = PER_DB.
	DTCSupport bool

	// RequiredSynchronizedSecondariesToCommit is SQL Server 2017+. Negative
	// omits the clause; zero is a legitimate value and is written.
	RequiredSynchronizedSecondariesToCommit int

	// Basic creates a Basic availability group (Standard edition): one
	// database, two replicas, no readable secondary.
	Basic bool

	// Contained creates a contained availability group, which carries its own
	// master and msdb. SQL Server 2022+.
	Contained bool

	// Databases are the databases to include. May be empty — a group with no
	// databases is legal and is how the "add them afterwards" flow starts.
	// Each must already be in full recovery with a full backup taken.
	Databases []string

	// Replicas are the group's replicas, primary first: CREATE AVAILABILITY
	// GROUP makes the instance it runs on the primary, so the first entry has
	// to name that instance. At least one is required.
	Replicas []AvailabilityReplicaSpec
}

// withClause renders one replica's WITH (...) body.
func (spec AvailabilityReplicaSpec) withClause() (string, error) {
	if strings.TrimSpace(spec.ServerName) == "" {
		return "", fmt.Errorf("replica has no server name")
	}
	if strings.TrimSpace(spec.EndpointURL) == "" {
		return "", fmt.Errorf("replica %q has no endpoint URL", spec.ServerName)
	}

	availability := strings.ToUpper(orElse(spec.AvailabilityMode, "SYNCHRONOUS_COMMIT"))
	if !availabilityModes[availability] {
		return "", fmt.Errorf("replica %q: unrecognized availability mode %q", spec.ServerName, spec.AvailabilityMode)
	}
	failover := strings.ToUpper(orElse(spec.FailoverMode, "MANUAL"))
	if !failoverModes[failover] {
		return "", fmt.Errorf("replica %q: unrecognized failover mode %q", spec.ServerName, spec.FailoverMode)
	}

	parts := []string{
		"ENDPOINT_URL = " + nStringLiteral(spec.EndpointURL),
		"AVAILABILITY_MODE = " + availability,
		"FAILOVER_MODE = " + failover,
	}
	if spec.SeedingMode != "" {
		seeding := strings.ToUpper(spec.SeedingMode)
		if !seedingModes[seeding] {
			return "", fmt.Errorf("replica %q: unrecognized seeding mode %q", spec.ServerName, spec.SeedingMode)
		}
		parts = append(parts, "SEEDING_MODE = "+seeding)
	}
	if spec.BackupPriority >= 0 {
		if spec.BackupPriority > 100 {
			return "", fmt.Errorf("replica %q: backup priority %d out of range 0-100", spec.ServerName, spec.BackupPriority)
		}
		parts = append(parts, fmt.Sprintf("BACKUP_PRIORITY = %d", spec.BackupPriority))
	}
	if spec.SessionTimeout > 0 {
		parts = append(parts, fmt.Sprintf("SESSION_TIMEOUT = %d", spec.SessionTimeout))
	}
	if spec.PrimaryRoleAllowConnections != "" {
		v := strings.ToUpper(spec.PrimaryRoleAllowConnections)
		if !primaryRoleConnections[v] {
			return "", fmt.Errorf("replica %q: unrecognized primary role connections %q", spec.ServerName, spec.PrimaryRoleAllowConnections)
		}
		parts = append(parts, "PRIMARY_ROLE (ALLOW_CONNECTIONS = "+v+")")
	}

	var secondary []string
	if spec.SecondaryRoleAllowConnections != "" {
		v := strings.ToUpper(spec.SecondaryRoleAllowConnections)
		if !secondaryRoleConnections[v] {
			return "", fmt.Errorf("replica %q: unrecognized secondary role connections %q", spec.ServerName, spec.SecondaryRoleAllowConnections)
		}
		secondary = append(secondary, "ALLOW_CONNECTIONS = "+v)
	}
	if spec.ReadOnlyRoutingURL != "" {
		secondary = append(secondary, "READ_ONLY_ROUTING_URL = "+nStringLiteral(spec.ReadOnlyRoutingURL))
	}
	if len(secondary) > 0 {
		parts = append(parts, "SECONDARY_ROLE ("+strings.Join(secondary, ", ")+")")
	}

	return strings.Join(parts, ", "), nil
}

// createStatement builds the whole CREATE AVAILABILITY GROUP statement.
func (req CreateAvailabilityGroupRequest) createStatement() (string, error) {
	if strings.TrimSpace(req.Name) == "" {
		return "", fmt.Errorf("availability group has no name")
	}
	if len(req.Replicas) == 0 {
		return "", fmt.Errorf("availability group %q has no replicas", req.Name)
	}

	var options []string
	if req.ClusterType != "" {
		clusterType := strings.ToUpper(req.ClusterType)
		if !clusterTypes[clusterType] {
			return "", fmt.Errorf("unrecognized cluster type %q", req.ClusterType)
		}
		options = append(options, "CLUSTER_TYPE = "+clusterType)
	}
	if req.AutomatedBackupPreference != "" {
		pref := strings.ToUpper(req.AutomatedBackupPreference)
		if !backupPreferences[pref] {
			return "", fmt.Errorf("unrecognized automated backup preference %q", req.AutomatedBackupPreference)
		}
		options = append(options, "AUTOMATED_BACKUP_PREFERENCE = "+pref)
	}
	if req.FailureConditionLevel != 0 {
		if req.FailureConditionLevel < 1 || req.FailureConditionLevel > 5 {
			return "", fmt.Errorf("failure condition level %d out of range 1-5", req.FailureConditionLevel)
		}
		options = append(options, fmt.Sprintf("FAILURE_CONDITION_LEVEL = %d", req.FailureConditionLevel))
	}
	if req.HealthCheckTimeout != 0 {
		options = append(options, fmt.Sprintf("HEALTH_CHECK_TIMEOUT = %d", req.HealthCheckTimeout))
	}
	if req.DBFailover {
		options = append(options, "DB_FAILOVER = ON")
	}
	if req.DTCSupport {
		options = append(options, "DTC_SUPPORT = PER_DB")
	}
	if req.RequiredSynchronizedSecondariesToCommit >= 0 {
		options = append(options, fmt.Sprintf("REQUIRED_SYNCHRONIZED_SECONDARIES_TO_COMMIT = %d",
			req.RequiredSynchronizedSecondariesToCommit))
	}
	if req.Basic {
		options = append(options, "BASIC")
	}
	if req.Contained {
		options = append(options, "CONTAINED")
	}

	stmt := "CREATE AVAILABILITY GROUP " + quoteIdent(req.Name)
	if len(options) > 0 {
		stmt += " WITH (" + strings.Join(options, ", ") + ")"
	}
	// FOR introduces the whole body, databases or not: with none it reads
	// "... FOR REPLICA ON", and dropping the FOR is a syntax error reported
	// against the *replica's* WITH, which points at entirely the wrong place.
	stmt += " FOR"
	if len(req.Databases) > 0 {
		names := make([]string, 0, len(req.Databases))
		for _, name := range req.Databases {
			if strings.TrimSpace(name) == "" {
				return "", fmt.Errorf("availability group %q has an empty database name", req.Name)
			}
			names = append(names, quoteIdent(name))
		}
		stmt += " DATABASE " + strings.Join(names, ", ")
	}

	replicas := make([]string, 0, len(req.Replicas))
	for _, spec := range req.Replicas {
		with, err := spec.withClause()
		if err != nil {
			return "", err
		}
		replicas = append(replicas, fmt.Sprintf("%s WITH (%s)", nStringLiteral(spec.ServerName), with))
	}
	return stmt + " REPLICA ON " + strings.Join(replicas, ", "), nil
}

// clusterTypes is the closed set of CLUSTER_TYPE values, spelled as CREATE
// accepts them. Note that sys.availability_groups reports cluster_type_desc in
// *lower* case — see AvailabilityGroup.ClusterType.
var clusterTypes = map[string]bool{"WSFC": true, "EXTERNAL": true, "NONE": true}

// CreateAvailabilityGroup creates an availability group with this instance as
// its primary.
//
// This is step 2 of four; see this section's doc comment for the rest. On its
// own it leaves a group whose secondaries are all disconnected, because none of
// them has joined yet.
func (s *Server) CreateAvailabilityGroup(req CreateAvailabilityGroupRequest) (*AvailabilityGroup, error) {
	return s.CreateAvailabilityGroupContext(context.Background(), req)
}

// CreateAvailabilityGroupContext is the context-aware variant of
// CreateAvailabilityGroup.
func (s *Server) CreateAvailabilityGroupContext(ctx context.Context, req CreateAvailabilityGroupRequest) (*AvailabilityGroup, error) {
	stmt, err := req.createStatement()
	if err != nil {
		return nil, fmt.Errorf("gosmo: create availability group: %w", err)
	}
	if err := s.execContext(ctx, stmt); err != nil {
		return nil, fmt.Errorf("gosmo: create availability group %q: %w", req.Name, err)
	}
	if Scripting(ctx) {
		// The group does not exist to be read back; hand out a handle carrying
		// the name and server so the caller's next scripted step can address it.
		return &AvailabilityGroup{server: s, Name: req.Name, ClusterType: strings.ToUpper(req.ClusterType)}, nil
	}
	return s.AvailabilityGroupByNameContext(ctx, req.Name)
}

// Join joins the instance this group was read from to it, as a secondary.
//
// Run against the secondary — the primary cannot join anything on its behalf.
// The group must already name this instance as a replica, which
// CreateAvailabilityGroup's REPLICA ON list does.
//
// **Under CLUSTER_TYPE = EXTERNAL or NONE the group does not exist on the
// secondary until this succeeds.** Only a WSFC cluster propagates the metadata
// ahead of the join, so AvailabilityGroupByName on the secondary comes back
// "no rows" and there is nothing to call this on — use Server.AvailabilityGroup
// to build a handle by name instead. Verified against SQL Server 2025.
//
// clusterType is passed rather than read off the group for the same reason: a
// handle that had to be built by name has no metadata to read. It must match
// what the group was created with, and EXTERNAL and NONE are rejected when it
// does not; pass "" or "WSFC" for a Windows cluster, which takes no clause.
func (ag *AvailabilityGroup) Join(clusterType string) error {
	return ag.JoinContext(context.Background(), clusterType)
}

// JoinContext is the context-aware variant of Join.
func (ag *AvailabilityGroup) JoinContext(ctx context.Context, clusterType string) error {
	clause := "JOIN"
	if ct := strings.ToUpper(clusterType); ct == "EXTERNAL" || ct == "NONE" {
		clause += " WITH (CLUSTER_TYPE = " + ct + ")"
	}
	if err := ag.alter(ctx, clause); err != nil {
		return fmt.Errorf("gosmo: join availability group %q: %w", ag.Name, err)
	}
	return nil
}

// GrantCreateAnyDatabase lets the availability group create databases on this
// instance, which is what automatic seeding needs to materialise a secondary
// copy.
//
// Run against each secondary. Without it a replica set to
// SEEDING_MODE = AUTOMATIC seeds nothing, and reports no error for it — the
// database simply never appears.
func (ag *AvailabilityGroup) GrantCreateAnyDatabase() error {
	return ag.GrantCreateAnyDatabaseContext(context.Background())
}

// GrantCreateAnyDatabaseContext is the context-aware variant of
// GrantCreateAnyDatabase.
func (ag *AvailabilityGroup) GrantCreateAnyDatabaseContext(ctx context.Context) error {
	if err := ag.alter(ctx, "GRANT CREATE ANY DATABASE"); err != nil {
		return fmt.Errorf("gosmo: grant create any database to availability group %q: %w", ag.Name, err)
	}
	return nil
}

// DenyCreateAnyDatabase revokes what GrantCreateAnyDatabase granted.
func (ag *AvailabilityGroup) DenyCreateAnyDatabase() error {
	return ag.DenyCreateAnyDatabaseContext(context.Background())
}

// DenyCreateAnyDatabaseContext is the context-aware variant of
// DenyCreateAnyDatabase.
func (ag *AvailabilityGroup) DenyCreateAnyDatabaseContext(ctx context.Context) error {
	if err := ag.alter(ctx, "DENY CREATE ANY DATABASE"); err != nil {
		return fmt.Errorf("gosmo: deny create any database to availability group %q: %w", ag.Name, err)
	}
	return nil
}
