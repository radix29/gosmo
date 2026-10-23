package gosmo

// availability_database.go is a database's side of an availability group: the
// per-database synchronization state read from the primary's DMVs, the
// ADD/REMOVE DATABASE that change which databases the group carries, and the
// secondary-side JOIN, UNJOIN, SUSPEND and RESUME that act on the local copy.
// The role each statement must be run from is availability_group.go's
// § Operations preamble.

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

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

	SynchronizationState  SynchronizationState
	SynchronizationHealth SynchronizationHealth
	DatabaseState         DatabaseState

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

// SynchronizationState is an availability database's
// synchronization_state_desc. NOT SYNCHRONIZING is spelled with a space, as
// the DMV reports it.
type SynchronizationState string

const (
	SyncNotSynchronizing SynchronizationState = "NOT SYNCHRONIZING"
	SyncSynchronizing    SynchronizationState = "SYNCHRONIZING"
	SyncSynchronized     SynchronizationState = "SYNCHRONIZED"
	SyncReverting        SynchronizationState = "REVERTING"
	SyncInitializing     SynchronizationState = "INITIALIZING"
)

// Databases returns the per-replica synchronization state of every database in
// the group.
//
// The database list comes from sys.availability_databases_cluster, which is
// cluster-wide metadata, so a database appears even on a replica that has not
// finished seeding it — with empty state rather than being silently missing.
func (ag *AvailabilityGroup) Databases(ctx context.Context) ([]*AvailabilityDatabase, error) {
	if err := requireID(fmt.Sprintf("list databases of availability group %q", ag.Name), ag.ID != ""); err != nil {
		return nil, err
	}
	s := ag.server

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
	       ISNULL(drs.secondary_lag_seconds, 0),
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
	return scanRows(rows, err, fmt.Sprintf("list databases of availability group %q", ag.Name), func(scan func(...any) error) (*AvailabilityDatabase, error) {
		d := &AvailabilityDatabase{}
		var sent, received, hardened, redone, commit sql.NullTime
		if err := scan(
			&d.GroupID, &d.ReplicaID, &d.ReplicaServerName, &d.DatabaseName,
			&d.GroupDatabaseID, &d.IsLocal, &d.IsPrimaryReplica,
			&d.SynchronizationState, &d.SynchronizationHealth, &d.DatabaseState,
			&d.IsSuspended, &d.SuspendReason,
			&d.LogSendQueueKB, &d.LogSendRateKBps, &d.RedoQueueKB, &d.RedoRateKBps,
			&d.SecondaryLagSeconds,
			&sent, &received, &hardened, &redone, &commit,
		); err != nil {
			return nil, err
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
		return d, nil
	})
}

// -- Candidates -----------------------------------------------------------------

// AvailabilityDatabaseCandidate is one database with every fact SQL Server
// checks before it lets the database into an availability group (CREATE
// AVAILABILITY GROUP ... FOR DATABASE, or ADD DATABASE) — so a caller can say
// which are refused, and why, instead of offering them and relaying the error.
//
// Each refusal below was driven on SQL Server 2025 except mirroring, which is
// from the documentation:
//
//   - not FULL recovery, or no log backup chain (LogBackupChainStarted): Msg
//     1475, which is also the answer for a RESTRICTED_USER database lacking a
//     backup — RESTRICTED_USER itself is accepted;
//   - read-only, which every database snapshot is: Msg 1466;
//   - AUTO_CLOSE on: Msg 1469;
//   - SINGLE_USER: Msg 1488;
//   - a database mirroring partner.
//
// Membership of a group is not here: the groups' own database lists say which
// group, which is what a caller shows.
type AvailabilityDatabaseCandidate struct {
	Name          string
	ID            int
	State         DatabaseState
	RecoveryModel RecoveryModel

	IsReadOnly    bool   // true of every snapshot as well
	IsSnapshot    bool   // source_database_id is set
	UserAccess    string // user_access_desc: MULTI_USER, SINGLE_USER or RESTRICTED_USER
	IsAutoCloseOn bool
	IsMirrored    bool // a database mirroring session is configured

	// LogBackupChainStarted is DatabaseRecoveryStatus.LogBackupChainStarted:
	// a full backup taken since the database entered FULL recovery.
	LogBackupChainStarted bool
}

// IsSystem reports one of the four system databases, as Database.IsSystem.
func (c *AvailabilityDatabaseCandidate) IsSystem() bool {
	return c.ID > 0 && c.ID <= systemDatabaseMaxID
}

// AvailabilityDatabaseCandidates returns every database on the instance with
// the facts AvailabilityDatabaseCandidate lists, in one read, ordered by name.
// Run it against the instance that will be (or is) the primary.
func (s *Server) AvailabilityDatabaseCandidates(ctx context.Context) ([]*AvailabilityDatabaseCandidate, error) {
	const q = `
SELECT d.name, d.database_id, d.state_desc, d.recovery_model_desc,
       d.is_read_only, CAST(CASE WHEN d.source_database_id IS NULL THEN 0 ELSE 1 END AS bit),
       d.user_access_desc, d.is_auto_close_on,
       CAST(CASE WHEN m.mirroring_guid IS NULL THEN 0 ELSE 1 END AS bit),
       CAST(CASE WHEN rs.last_log_backup_lsn IS NULL THEN 0 ELSE 1 END AS bit)
FROM   sys.databases d
LEFT   JOIN sys.database_mirroring m ON m.database_id = d.database_id
LEFT   JOIN sys.database_recovery_status rs ON rs.database_id = d.database_id
ORDER  BY d.name`

	rows, err := s.query(ctx, q)
	return scanRows(rows, err, "availability database candidates", func(scan func(...any) error) (*AvailabilityDatabaseCandidate, error) {
		c := &AvailabilityDatabaseCandidate{}
		var state, recovery, access sql.NullString
		if err := scan(&c.Name, &c.ID, &state, &recovery,
			&c.IsReadOnly, &c.IsSnapshot, &access, &c.IsAutoCloseOn,
			&c.IsMirrored, &c.LogBackupChainStarted); err != nil {
			return nil, err
		}
		c.State, c.RecoveryModel, c.UserAccess = DatabaseState(state.String), RecoveryModel(recovery.String), access.String
		return c, nil
	})
}

// -- Databases in the group ------------------------------------------------

// AddDatabase adds a database on the primary to the availability group.
//
// Run against the primary. The database must already be in the full recovery
// model with a log backup taken; SQL Server rejects it otherwise. What happens
// on the secondaries afterwards depends on their seeding mode: an AUTOMATIC
// replica seeds itself, a MANUAL one needs the database restored there and then
// JoinDatabase called against it.
func (ag *AvailabilityGroup) AddDatabase(ctx context.Context, name string) error {
	if strings.TrimSpace(name) == "" {
		return invalidf("gosmo: add database to availability group %q: empty database name", ag.Name)
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
func (ag *AvailabilityGroup) RemoveDatabase(ctx context.Context, name string) error {
	if strings.TrimSpace(name) == "" {
		return invalidf("gosmo: remove database from availability group %q: empty database name", ag.Name)
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
	_, err := ag.server.exec(ctx,
		fmt.Sprintf("ALTER DATABASE %s SET HADR %s", quoteIdent(name), clause))
	return err
}

// JoinDatabase joins a restored secondary copy of a database to the group.
//
// Run against the secondary holding the copy, after restoring it WITH NORECOVERY
// from a full and a log backup of the primary's. Only needed for a MANUAL-seeding
// replica; an AUTOMATIC one joins itself as part of seeding.
func (ag *AvailabilityGroup) JoinDatabase(ctx context.Context, name string) error {
	if strings.TrimSpace(name) == "" {
		return invalidf("gosmo: join database to availability group %q: empty database name", ag.Name)
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
// it on every other replica. Unjoining the database this Server's pool logs in
// to moves the pool to master (see InitialDatabase).
func (ag *AvailabilityGroup) UnjoinDatabase(ctx context.Context, name string) error {
	if strings.TrimSpace(name) == "" {
		return invalidf("gosmo: unjoin database from availability group %q: empty database name", ag.Name)
	}
	if err := ag.alterDatabaseHADR(ctx, name, "OFF"); err != nil {
		return fmt.Errorf("gosmo: unjoin database %q from availability group %q: %w", name, ag.Name, err)
	}
	ag.server.leftLoginDatabase(ctx, name, "master")
	return nil
}

// SuspendDatabase suspends data movement for one database.
//
// The scope depends on where it runs, and the difference matters: on a secondary
// it suspends that one secondary, on the primary it suspends the database on
// *every* secondary. Either way the primary keeps accepting writes and its log
// cannot be truncated while movement is suspended, so a long suspension fills
// the log drive.
func (ag *AvailabilityGroup) SuspendDatabase(ctx context.Context, name string) error {
	if strings.TrimSpace(name) == "" {
		return invalidf("gosmo: suspend database in availability group %q: empty database name", ag.Name)
	}
	if err := ag.alterDatabaseHADR(ctx, name, "SUSPEND"); err != nil {
		return fmt.Errorf("gosmo: suspend database %q in availability group %q: %w", name, ag.Name, err)
	}
	return nil
}

// ResumeDatabase resumes data movement for one database, on the same scope
// SuspendDatabase used.
func (ag *AvailabilityGroup) ResumeDatabase(ctx context.Context, name string) error {
	if strings.TrimSpace(name) == "" {
		return invalidf("gosmo: resume database in availability group %q: empty database name", ag.Name)
	}
	if err := ag.alterDatabaseHADR(ctx, name, "RESUME"); err != nil {
		return fmt.Errorf("gosmo: resume database %q in availability group %q: %w", name, ag.Name, err)
	}
	return nil
}
