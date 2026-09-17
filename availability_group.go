package gosmo

// availability_group.go models Always On availability groups: the group
// itself, how it is read, the settings that change it, and the drop and
// failover that act on the whole group. Its replicas are in
// availability_replica.go, the per-database synchronization state in
// availability_database.go, the listeners clients connect through in
// availability_listener.go, and CREATE AVAILABILITY GROUP in
// availability_group_create.go.
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
	major := s.serverMajorVersion()

	// 2016 added the basic/DTC/failover/distributed flags.
	basicFeatures := colSince(major, SQLServer2016, "ag.basic_features", "CAST(0 AS bit)")
	dtcSupport := colSince(major, SQLServer2016, "ag.dtc_support", "CAST(0 AS bit)")
	dbFailover := colSince(major, SQLServer2016, "ag.db_failover", "CAST(0 AS bit)")
	isDistributed := colSince(major, SQLServer2016, "ag.is_distributed", "CAST(0 AS bit)")

	// 2017 added the external-cluster support this whole type keys off.
	//
	// UPPER is not cosmetic: sys.availability_groups reports cluster_type_desc
	// and automated_backup_preference_desc in *lower* case ("external",
	// "secondary") while every sys.availability_replicas *_desc column is upper
	// case. Without it the values documented on these fields are not the values
	// they hold, and the obvious ClusterType == "EXTERNAL" test silently never
	// fires. Verified against SQL Server 2025.
	clusterType := colSince(major, SQLServer2017, "UPPER(ISNULL(ag.cluster_type_desc,''))", "CAST('' AS nvarchar(60))")
	requiredSync := colSince(major, SQLServer2017, "ag.required_synchronized_secondaries_to_commit", "CAST(0 AS int)")

	// 2022 added contained availability groups.
	isContained := colSince(major, SQLServer2022, "ag.is_contained", "CAST(0 AS bit)")

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

// AvailabilityGroupRef returns a lightweight handle to an availability group by
// name, with no query and no metadata: every field but Name is zero, so
// ClusterType, PrimaryReplicaServerName and IsLocalPrimary all read as
// "unknown" rather than as fact. Use AvailabilityGroupByName to get a group
// whose fields mean something.
//
// This exists for the one case where the group cannot be read: a secondary of
// an EXTERNAL- or NONE-cluster group has no row for it until Join succeeds, so
// the join has to be issued against a handle built from the name alone. It is
// also what works under a WithScript-derived context, where nothing has been
// created yet to read back. The same split as Server.DatabaseRef vs
// Server.DatabaseByName.
func (s *Server) AvailabilityGroupRef(name string) *AvailabilityGroup {
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
