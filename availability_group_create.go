package gosmo

// availability_group_create.go is CREATE AVAILABILITY GROUP and the steps
// around it that are not one statement — the secondary's own JOIN, and the
// GRANT CREATE ANY DATABASE automatic seeding needs. The group itself is in
// availability_group.go.

import (
	"cmp"
	"context"
	"fmt"
	"strings"
)

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
	AvailabilityMode AvailabilityMode

	// FailoverMode is MANUAL, AUTOMATIC or EXTERNAL. Empty means MANUAL.
	// EXTERNAL is required — and the only legal value — under
	// CLUSTER_TYPE = EXTERNAL.
	FailoverMode FailoverMode

	// SeedingMode is AUTOMATIC or MANUAL. Empty omits the clause, which the
	// server defaults to MANUAL.
	SeedingMode SeedingMode

	// BackupPriority is 0-100; 0 excludes the replica from automated backups.
	// Negative omits the clause, leaving the server's default of 50 — which is
	// why this is not simply "0 means default".
	BackupPriority int

	// SessionTimeout is the seconds a replica waits for a partner before
	// declaring the connection dead. Zero omits the clause (server default 10).
	SessionTimeout int

	// PrimaryRoleAllowConnections is ALL or READ_WRITE; empty omits the clause.
	PrimaryRoleAllowConnections AllowConnections

	// SecondaryRoleAllowConnections is NO, READ_ONLY or ALL; empty omits it.
	SecondaryRoleAllowConnections AllowConnections

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
	ClusterType ClusterType

	// AutomatedBackupPreference is PRIMARY, SECONDARY_ONLY, SECONDARY or NONE.
	// Empty omits the clause.
	AutomatedBackupPreference BackupPreference

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

	availability := upperKeyword(cmp.Or(spec.AvailabilityMode, AvailabilitySynchronousCommit))
	if !availabilityModes[availability] {
		return "", fmt.Errorf("replica %q: unrecognized availability mode %q", spec.ServerName, spec.AvailabilityMode)
	}
	failover := upperKeyword(cmp.Or(spec.FailoverMode, FailoverManual))
	if !failoverModes[failover] {
		return "", fmt.Errorf("replica %q: unrecognized failover mode %q", spec.ServerName, spec.FailoverMode)
	}

	parts := []string{
		"ENDPOINT_URL = " + QuoteLiteral(spec.EndpointURL),
		"AVAILABILITY_MODE = " + string(availability),
		"FAILOVER_MODE = " + string(failover),
	}
	if spec.SeedingMode != "" {
		seeding := upperKeyword(spec.SeedingMode)
		if !seedingModes[seeding] {
			return "", fmt.Errorf("replica %q: unrecognized seeding mode %q", spec.ServerName, spec.SeedingMode)
		}
		parts = append(parts, "SEEDING_MODE = "+string(seeding))
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
		v := upperKeyword(spec.PrimaryRoleAllowConnections)
		if !primaryRoleConnections[v] {
			return "", fmt.Errorf("replica %q: unrecognized primary role connections %q", spec.ServerName, spec.PrimaryRoleAllowConnections)
		}
		parts = append(parts, "PRIMARY_ROLE (ALLOW_CONNECTIONS = "+string(v)+")")
	}

	var secondary []string
	if spec.SecondaryRoleAllowConnections != "" {
		v := upperKeyword(spec.SecondaryRoleAllowConnections)
		if !secondaryRoleConnections[v] {
			return "", fmt.Errorf("replica %q: unrecognized secondary role connections %q", spec.ServerName, spec.SecondaryRoleAllowConnections)
		}
		secondary = append(secondary, "ALLOW_CONNECTIONS = "+string(v))
	}
	if spec.ReadOnlyRoutingURL != "" {
		secondary = append(secondary, "READ_ONLY_ROUTING_URL = "+QuoteLiteral(spec.ReadOnlyRoutingURL))
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
		clusterType := upperKeyword(req.ClusterType)
		if !clusterTypes[clusterType] {
			return "", fmt.Errorf("unrecognized cluster type %q", req.ClusterType)
		}
		options = append(options, "CLUSTER_TYPE = "+string(clusterType))
	}
	if req.AutomatedBackupPreference != "" {
		pref := upperKeyword(req.AutomatedBackupPreference)
		if !backupPreferences[pref] {
			return "", fmt.Errorf("unrecognized automated backup preference %q", req.AutomatedBackupPreference)
		}
		options = append(options, "AUTOMATED_BACKUP_PREFERENCE = "+string(pref))
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
		replicas = append(replicas, fmt.Sprintf("%s WITH (%s)", QuoteLiteral(spec.ServerName), with))
	}
	return stmt + " REPLICA ON " + strings.Join(replicas, ", "), nil
}

// ClusterType is a group's CLUSTER_TYPE, spelled as CREATE accepts it.
// sys.availability_groups reports cluster_type_desc in *lower* case; the
// AvailabilityGroup.ClusterType read upper-cases it to match.
type ClusterType string

const (
	ClusterTypeWSFC     ClusterType = "WSFC"
	ClusterTypeExternal ClusterType = "EXTERNAL" // Pacemaker and the like
	ClusterTypeNone     ClusterType = "NONE"     // read-scale, no cluster manager
)

// clusterTypes is ClusterType's validity check.
var clusterTypes = map[ClusterType]bool{ClusterTypeWSFC: true, ClusterTypeExternal: true, ClusterTypeNone: true}

// CreateAvailabilityGroup creates an availability group with this instance as
// its primary.
//
// This is step 2 of four; see this section's doc comment for the rest. On its
// own it leaves a group whose secondaries are all disconnected, because none of
// them has joined yet.
func (s *Server) CreateAvailabilityGroup(ctx context.Context, req CreateAvailabilityGroupRequest) (*AvailabilityGroup, error) {
	stmt, err := req.createStatement()
	if err != nil {
		return nil, fmt.Errorf("gosmo: create availability group: %w", err)
	}
	if err := s.exec(ctx, stmt); err != nil {
		return nil, fmt.Errorf("gosmo: create availability group %q: %w", req.Name, err)
	}
	// Under Scripting(ctx) the group does not exist yet; the handle carries
	// the name and server so the caller's next scripted step can address it.
	return createdObject(ctx, &AvailabilityGroup{server: s, Name: req.Name, ClusterType: upperKeyword(req.ClusterType)}, func() (*AvailabilityGroup, error) {
		return s.AvailabilityGroupByName(ctx, req.Name)
	})
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
// does not; pass "" or ClusterTypeWSFC for a Windows cluster, which takes no
// clause.
func (ag *AvailabilityGroup) Join(ctx context.Context, clusterType ClusterType) error {
	clause := "JOIN"
	if ct := upperKeyword(clusterType); ct == ClusterTypeExternal || ct == ClusterTypeNone {
		clause += " WITH (CLUSTER_TYPE = " + string(ct) + ")"
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
func (ag *AvailabilityGroup) GrantCreateAnyDatabase(ctx context.Context) error {
	if err := ag.alter(ctx, "GRANT CREATE ANY DATABASE"); err != nil {
		return fmt.Errorf("gosmo: grant create any database to availability group %q: %w", ag.Name, err)
	}
	return nil
}

// DenyCreateAnyDatabase revokes what GrantCreateAnyDatabase granted.
func (ag *AvailabilityGroup) DenyCreateAnyDatabase(ctx context.Context) error {
	if err := ag.alter(ctx, "DENY CREATE ANY DATABASE"); err != nil {
		return fmt.Errorf("gosmo: deny create any database to availability group %q: %w", ag.Name, err)
	}
	return nil
}
