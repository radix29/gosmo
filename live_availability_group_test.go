//go:build livedb

// Live verification of the Always On read layer in availability_group.go.
//
// Every query here joins cluster-wide catalog views against sys.dm_hadr_*
// DMVs, and the joins are the whole risk: a wrong join key silently yields
// zero rows or a cross product, and both look like "no error" to a unit test.
// Nothing below asserts merely that a call returned — each check pins a
// relationship that only a real, healthy availability group can satisfy.
//
//	go test -tags livedb . -run TestLiveAvailabilityGroup -v \
//	  -liveag 'ubusql1' -liveag-user sa -liveag-pass PASS
//
// Read-only: creates and drops nothing. Skipped entirely without -liveag.
// See the gossms AAG test cluster (ubusql1/ubusql2) for an instance this
// runs green against.
package gosmo

import (
	"context"
	"flag"
	"strings"
	"testing"
	"time"
)

var (
	liveAGServer = flag.String("liveag", "", "SQL Server hosting an availability group, for the live Always On tests")
	liveAGUser   = flag.String("liveag-user", "sa", "login for -liveag")
	liveAGPass   = flag.String("liveag-pass", "", "password for -liveag")
)

func liveAG(t *testing.T) (*Server, context.Context, func()) {
	t.Helper()
	if *liveAGServer == "" {
		t.Skip("no -liveag server given")
	}
	srv, err := Connect(ConnectionOptions{
		Server:                 *liveAGServer,
		User:                   *liveAGUser,
		Password:               *liveAGPass,
		TrustServerCertificate: true,
		Encrypt:                "false",
	})
	if err != nil {
		t.Fatalf("connect to %s: %v", *liveAGServer, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	return srv, ctx, func() { cancel(); srv.Close() }
}

func TestLiveAvailabilityGroupRead(t *testing.T) {
	srv, ctx, done := liveAG(t)
	defer done()

	if info := srv.Info(); info != nil && !info.IsHADREnabled {
		t.Skip("Always On is not enabled on this instance")
	}

	groups, err := srv.AvailabilityGroupsContext(ctx)
	if err != nil {
		t.Fatalf("AvailabilityGroupsContext: %v", err)
	}
	if len(groups) == 0 {
		t.Skip("instance participates in no availability group")
	}

	for _, ag := range groups {
		t.Run(ag.Name, func(t *testing.T) {
			if ag.ID == "" {
				t.Error("group ID is empty; the CONVERT(varchar(36), group_id) scan is wrong")
			}
			// Every ...Context below filters on ag.ID, so a group_id that
			// does not round-trip would make all of them return nothing.
			if ag.ClusterType == "" {
				t.Logf("cluster type empty — expected only before SQL Server 2017 (major=%d)", srv.Info().VersionMajor)
			}
			// sys.availability_groups reports these two *_desc columns in
			// lower case, unlike every replica *_desc; agColumns upper-cases
			// them so the documented spellings ("EXTERNAL", "SECONDARY") are
			// the ones callers actually get. Drop the UPPER and an
			// == "EXTERNAL" test elsewhere silently never fires.
			for label, got := range map[string]string{
				"ClusterType":               ag.ClusterType,
				"AutomatedBackupPreference": ag.AutomatedBackupPreference,
			} {
				if got != strings.ToUpper(got) {
					t.Errorf("%s = %q, want it upper-cased by agColumns", label, got)
				}
			}

			byName := lookupByName(t, srv, ctx, ag.Name)
			if byName.ID != ag.ID {
				t.Errorf("AvailabilityGroupByName returned group %q, want %q", byName.ID, ag.ID)
			}

			replicas := checkReplicas(t, ctx, ag)
			checkDatabases(t, ctx, ag, replicas)
			checkListeners(t, ctx, ag)
		})
	}
}

func lookupByName(t *testing.T, srv *Server, ctx context.Context, name string) *AvailabilityGroup {
	t.Helper()
	ag, err := srv.AvailabilityGroupByNameContext(ctx, name)
	if err != nil {
		t.Fatalf("AvailabilityGroupByNameContext(%q): %v", name, err)
	}
	return ag
}

// checkReplicas pins the replica join and returns the replica names, which the
// database check needs to prove its own join covered every replica.
func checkReplicas(t *testing.T, ctx context.Context, ag *AvailabilityGroup) map[string]bool {
	t.Helper()
	replicas, err := ag.ReplicasContext(ctx)
	if err != nil {
		t.Fatalf("ReplicasContext: %v", err)
	}
	if len(replicas) == 0 {
		t.Fatal("group has no replicas; the group_id join in ReplicasContext is wrong")
	}

	names := make(map[string]bool, len(replicas))
	locals, primaries := 0, 0
	for _, r := range replicas {
		if r.ReplicaServerName == "" {
			t.Error("replica has empty server name")
		}
		if names[r.ReplicaServerName] {
			t.Errorf("replica %q listed twice — the replica_states join is duplicating rows",
				r.ReplicaServerName)
		}
		names[r.ReplicaServerName] = true

		if r.GroupID != ag.ID {
			t.Errorf("replica %q carries group %q, want %q", r.ReplicaServerName, r.GroupID, ag.ID)
		}
		if r.IsLocal {
			locals++
		}
		if strings.EqualFold(r.Role, "PRIMARY") {
			primaries++
		}
		if r.AvailabilityMode == "" {
			t.Errorf("replica %q has empty availability mode", r.ReplicaServerName)
		}
	}

	// Exactly one replica is this instance. More than one means the
	// replica_id join matched across groups.
	if locals != 1 {
		t.Errorf("got %d local replicas, want exactly 1", locals)
	}
	if primaries > 1 {
		t.Errorf("got %d primary replicas, want at most 1", primaries)
	}

	// IsLocalPrimary has to agree with the per-replica role the DMV reports,
	// since it is derived from a different column (gs.primary_replica).
	wantLocalPrimary := false
	for _, r := range replicas {
		if r.IsLocal && strings.EqualFold(r.Role, "PRIMARY") {
			wantLocalPrimary = true
		}
	}
	if got := ag.IsLocalPrimary(); got != wantLocalPrimary {
		t.Errorf("IsLocalPrimary()=%v, but the local replica's role says %v "+
			"(primary_replica=%q, server name=%q)",
			got, wantLocalPrimary, ag.PrimaryReplicaServerName, ag.Server().Name())
	}
	return names
}

// checkDatabases pins the three-way cluster-list/replica/DMV join. Its whole
// point is that a database must appear once per replica even when the DMV has
// no row for it — the case a plain inner join would silently drop.
func checkDatabases(t *testing.T, ctx context.Context, ag *AvailabilityGroup, replicaNames map[string]bool) {
	t.Helper()
	dbs, err := ag.DatabasesContext(ctx)
	if err != nil {
		t.Fatalf("DatabasesContext: %v", err)
	}
	if len(dbs) == 0 {
		t.Skip("group contains no databases")
	}

	seen := map[string]map[string]bool{} // database -> replica -> present
	for _, d := range dbs {
		if d.DatabaseName == "" {
			t.Error("availability database has empty name")
		}
		if !replicaNames[d.ReplicaServerName] {
			t.Errorf("database %q reports replica %q, which is not in the group's replica list",
				d.DatabaseName, d.ReplicaServerName)
		}
		if seen[d.DatabaseName] == nil {
			seen[d.DatabaseName] = map[string]bool{}
		}
		if seen[d.DatabaseName][d.ReplicaServerName] {
			t.Errorf("database %q appears twice for replica %q — the join is producing a cross product",
				d.DatabaseName, d.ReplicaServerName)
		}
		seen[d.DatabaseName][d.ReplicaServerName] = true
	}

	// One row per (database, replica) pair, always — that is the property the
	// LEFT JOIN onto dm_hadr_database_replica_states exists to guarantee.
	for db, reps := range seen {
		if len(reps) != len(replicaNames) {
			t.Errorf("database %q has %d replica rows, want %d (one per replica)",
				db, len(reps), len(replicaNames))
		}
	}

	// On the primary, at least one row must be flagged as the primary copy;
	// otherwise is_primary_replica was never actually read.
	if ag.IsLocalPrimary() {
		anyPrimary := false
		for _, d := range dbs {
			if d.IsPrimaryReplica && d.IsLocal {
				anyPrimary = true
			}
		}
		if !anyPrimary {
			t.Error("connected to the primary, but no database row is flagged is_primary_replica+is_local")
		}
	}
}

func checkListeners(t *testing.T, ctx context.Context, ag *AvailabilityGroup) {
	t.Helper()
	listeners, err := ag.ListenersContext(ctx)
	if err != nil {
		t.Fatalf("ListenersContext: %v", err)
	}
	for _, l := range listeners {
		if l.DNSName == "" {
			t.Error("listener has empty DNS name")
		}
		if l.GroupID != ag.ID {
			t.Errorf("listener %q carries group %q, want %q", l.DNSName, l.GroupID, ag.ID)
		}
		// A conformant listener created through SQL Server always has at
		// least one address; zero here means attachListenerIPs' listener_id
		// join failed rather than that none is configured.
		if l.IsConformant && len(l.IPAddresses) == 0 {
			t.Errorf("conformant listener %q has no IP addresses — the listener_id join in attachListenerIPs is wrong",
				l.DNSName)
		}
		for _, ip := range l.IPAddresses {
			if ip.IPAddress == "" && !ip.IsDHCP {
				t.Errorf("listener %q has a static address row with no IP", l.DNSName)
			}
		}
	}
}

// -- Writes --------------------------------------------------------------------
//
// Every ALTER the Properties pages can issue, exercised against a real group
// and then restored. Guarded by its own -liveag-write flag on top of -liveag,
// because unlike the reads above this one mutates a live availability group.
//
//	go test -tags livedb . -run TestLiveAvailabilityGroupWrite -v \
//	  -liveag ubusql1 -liveag-user sa -liveag-pass PASS -liveag-write
//
// Deliberately not covered: AVAILABILITY_MODE and FAILOVER_MODE. Dropping the
// last synchronous replica of an EXTERNAL-cluster group leaves Pacemaker unable
// to promote anything — it scores every replica -INFINITY — and the only way
// out is to drop and recreate the group. The statement text for both is pinned
// in availability_group_test.go instead.

var liveAGWrite = flag.Bool("liveag-write", false, "also run the Always On write tests, which modify the availability group")

func TestLiveAvailabilityGroupWrite(t *testing.T) {
	srv, ctx, done := liveAG(t)
	defer done()
	if !*liveAGWrite {
		t.Skip("write tests modify a live availability group; pass -liveag-write to run them")
	}

	groups, err := srv.AvailabilityGroupsContext(ctx)
	if err != nil {
		t.Fatalf("AvailabilityGroupsContext: %v", err)
	}
	var ag *AvailabilityGroup
	for _, g := range groups {
		if g.IsLocalPrimary() {
			ag = g
			break
		}
	}
	if ag == nil {
		t.Skip("this instance is not the primary of any availability group; ALTER is rejected on a secondary")
	}

	// reread pulls the group back from the server, so each check reads what
	// SQL Server actually stored rather than what the setter mirrored.
	reread := func() *AvailabilityGroup {
		t.Helper()
		fresh, err := srv.AvailabilityGroupByNameContext(ctx, ag.Name)
		if err != nil {
			t.Fatalf("re-read %q: %v", ag.Name, err)
		}
		return fresh
	}

	t.Run("group settings", func(t *testing.T) {
		orig := reread()

		if err := ag.SetHealthCheckTimeoutContext(ctx, orig.HealthCheckTimeout+5000); err != nil {
			t.Fatalf("SetHealthCheckTimeout: %v", err)
		}
		if got := reread().HealthCheckTimeout; got != orig.HealthCheckTimeout+5000 {
			t.Errorf("HealthCheckTimeout = %d, want %d", got, orig.HealthCheckTimeout+5000)
		}
		if err := ag.SetHealthCheckTimeoutContext(ctx, orig.HealthCheckTimeout); err != nil {
			t.Fatalf("restore HealthCheckTimeout: %v", err)
		}

		level := 3
		if orig.FailureConditionLevel == 3 {
			level = 4
		}
		if err := ag.SetFailureConditionLevelContext(ctx, level); err != nil {
			t.Fatalf("SetFailureConditionLevel: %v", err)
		}
		if got := reread().FailureConditionLevel; got != level {
			t.Errorf("FailureConditionLevel = %d, want %d", got, level)
		}
		if err := ag.SetFailureConditionLevelContext(ctx, orig.FailureConditionLevel); err != nil {
			t.Fatalf("restore FailureConditionLevel: %v", err)
		}

		if err := ag.SetDBFailoverContext(ctx, !orig.DBFailover); err != nil {
			t.Fatalf("SetDBFailover: %v", err)
		}
		if got := reread().DBFailover; got == orig.DBFailover {
			t.Errorf("DBFailover did not change from %v", orig.DBFailover)
		}
		if err := ag.SetDBFailoverContext(ctx, orig.DBFailover); err != nil {
			t.Fatalf("restore DBFailover: %v", err)
		}

		// DTC_SUPPORT is the one flag whose off value is NONE rather than OFF,
		// so a wrong keyword here fails on the server and nowhere else.
		if err := ag.SetDTCSupportContext(ctx, !orig.DTCSupport); err != nil {
			t.Fatalf("SetDTCSupport: %v", err)
		}
		if got := reread().DTCSupport; got == orig.DTCSupport {
			t.Errorf("DTCSupport did not change from %v", orig.DTCSupport)
		}
		if err := ag.SetDTCSupportContext(ctx, orig.DTCSupport); err != nil {
			t.Fatalf("restore DTCSupport: %v", err)
		}

		pref := "SECONDARY_ONLY"
		if strings.EqualFold(orig.AutomatedBackupPreference, pref) {
			pref = "PRIMARY"
		}
		if err := ag.SetAutomatedBackupPreferenceContext(ctx, pref); err != nil {
			t.Fatalf("SetAutomatedBackupPreference: %v", err)
		}
		if got := reread().AutomatedBackupPreference; !strings.EqualFold(got, pref) {
			t.Errorf("AutomatedBackupPreference = %q, want %q", got, pref)
		}
		if err := ag.SetAutomatedBackupPreferenceContext(ctx, orig.AutomatedBackupPreference); err != nil {
			t.Fatalf("restore AutomatedBackupPreference: %v", err)
		}
	})

	t.Run("replica settings", func(t *testing.T) {
		replicas, err := ag.ReplicasContext(ctx)
		if err != nil {
			t.Fatalf("ReplicasContext: %v", err)
		}
		// Pick a secondary: its settings are the ones a DBA actually tunes,
		// and every one of them is set from the primary, not from the replica
		// itself — which is the thing worth proving here.
		var target *AvailabilityReplica
		for _, r := range replicas {
			if !r.IsLocal {
				target = r
				break
			}
		}
		if target == nil {
			t.Skip("group has no remote replica to modify")
		}
		if target.GroupName != ag.Name {
			t.Fatalf("replica carries GroupName %q, want %q — MODIFY REPLICA would name the wrong group", target.GroupName, ag.Name)
		}

		rereadReplica := func() *AvailabilityReplica {
			t.Helper()
			rs, err := ag.ReplicasContext(ctx)
			if err != nil {
				t.Fatalf("re-read replicas: %v", err)
			}
			for _, r := range rs {
				if strings.EqualFold(r.ReplicaServerName, target.ReplicaServerName) {
					return r
				}
			}
			t.Fatalf("replica %q vanished", target.ReplicaServerName)
			return nil
		}
		orig := rereadReplica()

		priority := 30
		if orig.BackupPriority == priority {
			priority = 40
		}
		if err := target.SetBackupPriorityContext(ctx, priority); err != nil {
			t.Fatalf("SetBackupPriority: %v", err)
		}
		if got := rereadReplica().BackupPriority; got != priority {
			t.Errorf("BackupPriority = %d, want %d", got, priority)
		}
		if err := target.SetBackupPriorityContext(ctx, orig.BackupPriority); err != nil {
			t.Fatalf("restore BackupPriority: %v", err)
		}

		timeout := orig.SessionTimeout + 5
		if err := target.SetSessionTimeoutContext(ctx, timeout); err != nil {
			t.Fatalf("SetSessionTimeout: %v", err)
		}
		if got := rereadReplica().SessionTimeout; got != timeout {
			t.Errorf("SessionTimeout = %d, want %d", got, timeout)
		}
		if err := target.SetSessionTimeoutContext(ctx, orig.SessionTimeout); err != nil {
			t.Fatalf("restore SessionTimeout: %v", err)
		}

		mode := "AUTOMATIC"
		if strings.EqualFold(orig.SeedingMode, mode) {
			mode = "MANUAL"
		}
		if err := target.SetSeedingModeContext(ctx, mode); err != nil {
			t.Fatalf("SetSeedingMode: %v", err)
		}
		if got := rereadReplica().SeedingMode; !strings.EqualFold(got, mode) {
			t.Errorf("SeedingMode = %q, want %q", got, mode)
		}
		if err := target.SetSeedingModeContext(ctx, orig.SeedingMode); err != nil {
			t.Fatalf("restore SeedingMode: %v", err)
		}

		// PRIMARY_ROLE/SECONDARY_ROLE nest their option, and the nesting is
		// where the syntax is easy to get wrong.
		conn := "ALL"
		if strings.EqualFold(orig.SecondaryRoleAllowConnections, conn) {
			conn = "READ_ONLY"
		}
		if err := target.SetSecondaryRoleAllowConnectionsContext(ctx, conn); err != nil {
			t.Fatalf("SetSecondaryRoleAllowConnections: %v", err)
		}
		if got := rereadReplica().SecondaryRoleAllowConnections; !strings.EqualFold(got, conn) {
			t.Errorf("SecondaryRoleAllowConnections = %q, want %q", got, conn)
		}
		if err := target.SetSecondaryRoleAllowConnectionsContext(ctx, orig.SecondaryRoleAllowConnections); err != nil {
			t.Fatalf("restore SecondaryRoleAllowConnections: %v", err)
		}

		primaryConn := "READ_WRITE"
		if strings.EqualFold(orig.PrimaryRoleAllowConnections, primaryConn) {
			primaryConn = "ALL"
		}
		if err := target.SetPrimaryRoleAllowConnectionsContext(ctx, primaryConn); err != nil {
			t.Fatalf("SetPrimaryRoleAllowConnections: %v", err)
		}
		if got := rereadReplica().PrimaryRoleAllowConnections; !strings.EqualFold(got, primaryConn) {
			t.Errorf("PrimaryRoleAllowConnections = %q, want %q", got, primaryConn)
		}
		if err := target.SetPrimaryRoleAllowConnectionsContext(ctx, orig.PrimaryRoleAllowConnections); err != nil {
			t.Fatalf("restore PrimaryRoleAllowConnections: %v", err)
		}
	})

	t.Run("read-only routing", func(t *testing.T) {
		replicas, err := ag.ReplicasContext(ctx)
		if err != nil {
			t.Fatalf("ReplicasContext: %v", err)
		}
		if len(replicas) < 2 {
			t.Skip("read-only routing needs at least two replicas")
		}
		var local, remote *AvailabilityReplica
		for _, r := range replicas {
			if r.IsLocal {
				local = r
			} else if remote == nil {
				remote = r
			}
		}
		if local == nil || remote == nil {
			t.Skip("could not identify a local and a remote replica")
		}

		origURL := remote.ReadOnlyRoutingURL
		origList, err := local.ReadOnlyRoutingListContext(ctx)
		if err != nil {
			t.Fatalf("ReadOnlyRoutingListContext: %v", err)
		}

		url := "TCP://" + remote.ReplicaServerName + ":1433"
		if err := remote.SetReadOnlyRoutingURLContext(ctx, url); err != nil {
			t.Fatalf("SetReadOnlyRoutingURL: %v", err)
		}
		if err := local.SetReadOnlyRoutingListContext(ctx, [][]string{{remote.ReplicaServerName}}); err != nil {
			t.Fatalf("SetReadOnlyRoutingList: %v", err)
		}
		got, err := local.ReadOnlyRoutingListContext(ctx)
		if err != nil {
			t.Fatalf("re-read routing list: %v", err)
		}
		if len(got) != 1 || len(got[0]) != 1 || !strings.EqualFold(got[0][0], remote.ReplicaServerName) {
			t.Errorf("routing list = %v, want one entry naming %q", got, remote.ReplicaServerName)
		}

		// Clearing is the half most likely to be wrong: the URL clears with
		// NULL and the list with the bare keyword NONE, and neither spelling
		// is guessable from the set form.
		if err := local.SetReadOnlyRoutingListContext(ctx, nil); err != nil {
			t.Fatalf("clear routing list: %v", err)
		}
		if cleared, err := local.ReadOnlyRoutingListContext(ctx); err != nil {
			t.Fatalf("re-read cleared routing list: %v", err)
		} else if len(cleared) != 0 {
			t.Errorf("routing list after clearing = %v, want empty", cleared)
		}
		if err := remote.SetReadOnlyRoutingURLContext(ctx, ""); err != nil {
			t.Fatalf("clear routing URL: %v", err)
		}

		// Restore whatever was there before.
		if origURL != "" {
			if err := remote.SetReadOnlyRoutingURLContext(ctx, origURL); err != nil {
				t.Fatalf("restore routing URL: %v", err)
			}
		}
		if len(origList) > 0 {
			if err := local.SetReadOnlyRoutingListContext(ctx, origList); err != nil {
				t.Fatalf("restore routing list: %v", err)
			}
		}
	})
}

// TestLiveAvailabilityGroupOperations exercises the membership operations —
// adding and removing a database, suspending and resuming its data movement,
// and the listener round trip — against a live group, restoring everything it
// touched. Guarded by its own -liveag-ops flag on top of -liveag.
//
//	go test -tags livedb . -run TestLiveAvailabilityGroupOperations -v \
//	  -liveag ubusql1 -liveag-user sa -liveag-pass PASS -liveag-ops
//
// Deliberately not covered against a real group: RemoveReplica and Drop. Both
// were verified by building a throwaway CLUSTER_TYPE = NONE group across the
// same two instances and tearing it down again, which is what proved that a
// removed replica keeps a stale sys.availability_groups row (see
// AvailabilityGroup.RemoveReplica) — running them here would destroy the group
// under test.
var (
	liveAGOps       = flag.Bool("liveag-ops", false, "also run the Always On operation tests, which add and remove a database")
	liveAGBackupDir = flag.String("liveag-backupdir", "/var/opt/mssql/data", "server-side directory the -liveag-ops test writes its seeding backups to")
)

func TestLiveAvailabilityGroupOperations(t *testing.T) {
	srv, ctx, done := liveAG(t)
	defer done()
	if !*liveAGOps {
		t.Skip("operation tests add and remove a database; pass -liveag-ops to run them")
	}

	groups, err := srv.AvailabilityGroupsContext(ctx)
	if err != nil {
		t.Fatalf("AvailabilityGroupsContext: %v", err)
	}
	var ag *AvailabilityGroup
	for _, g := range groups {
		if g.IsLocalPrimary() {
			ag = g
			break
		}
	}
	if ag == nil {
		t.Skip("this instance is not the primary of any availability group; these operations are rejected on a secondary")
	}

	t.Run("database lifecycle", func(t *testing.T) {
		const dbName = "gosmo_agops"
		liveDropEverywhere(t, srv, ag, dbName)

		if err := srv.CreateDatabaseContext(ctx, dbName, &CreateDatabaseOptions{RecoveryModel: RecoveryModelFull}); err != nil {
			t.Fatalf("create %s: %v", dbName, err)
		}
		defer liveDropEverywhere(t, srv, ag, dbName)

		// ADD DATABASE is refused until the database has a full backup and a
		// log backup — the seeding chain, not a formality.
		for _, action := range []BackupAction{BackupActionDatabase, BackupActionLog} {
			ext := ".bak"
			if action == BackupActionLog {
				ext = ".trn"
			}
			if err := srv.BackupContext(ctx, BackupOptions{
				Database: dbName, Action: action,
				Devices: []string{*liveAGBackupDir + "/" + dbName + ext},
				Init:    true, Format: true,
			}); err != nil {
				t.Fatalf("backup %s (%v): %v", dbName, action, err)
			}
		}

		if err := ag.AddDatabaseContext(ctx, dbName); err != nil {
			t.Fatalf("AddDatabase: %v", err)
		}
		local := liveWaitForAGDatabase(t, ctx, ag, dbName, func(d *AvailabilityDatabase) bool {
			return d.IsLocal && d.SynchronizationState != ""
		})
		if !local.IsPrimaryReplica {
			t.Errorf("added database's local row is not the primary replica's")
		}

		// Suspending from the primary suspends every secondary, and the local
		// row is the one that reports it back here.
		if err := ag.SuspendDatabaseContext(ctx, dbName); err != nil {
			t.Fatalf("SuspendDatabase: %v", err)
		}
		suspended := liveWaitForAGDatabase(t, ctx, ag, dbName, func(d *AvailabilityDatabase) bool {
			return d.IsLocal && d.IsSuspended
		})
		if suspended.SuspendReason == "" {
			t.Errorf("suspended database reports no suspend reason")
		}

		if err := ag.ResumeDatabaseContext(ctx, dbName); err != nil {
			t.Fatalf("ResumeDatabase: %v", err)
		}
		liveWaitForAGDatabase(t, ctx, ag, dbName, func(d *AvailabilityDatabase) bool {
			return d.IsLocal && !d.IsSuspended
		})

		if err := ag.RemoveDatabaseContext(ctx, dbName); err != nil {
			t.Fatalf("RemoveDatabase: %v", err)
		}
		dbs, err := ag.DatabasesContext(ctx)
		if err != nil {
			t.Fatalf("Databases after removal: %v", err)
		}
		for _, d := range dbs {
			if strings.EqualFold(d.DatabaseName, dbName) {
				t.Fatalf("%s still in the group after RemoveDatabase", dbName)
			}
		}
	})

	// Removing and re-adding the group's own listener, byte for byte. The IP
	// form is the one worth exercising: its nested parentheses are unlike every
	// other clause in this file, and a wrong nesting is a syntax error rather
	// than a wrong result.
	t.Run("listener", func(t *testing.T) {
		listeners, err := ag.ListenersContext(ctx)
		if err != nil {
			t.Fatalf("Listeners: %v", err)
		}
		if len(listeners) == 0 {
			t.Skip("group has no listener to round-trip")
		}
		orig := listeners[0]
		spec := AvailabilityListenerSpec{DNSName: orig.DNSName, Port: orig.Port}
		for _, ip := range orig.IPAddresses {
			if ip.IsDHCP {
				t.Skip("group's listener is DHCP; re-adding it would not restore the same address")
			}
			spec.IPAddresses = append(spec.IPAddresses, AvailabilityListenerIPSpec{
				IPAddress: ip.IPAddress, SubnetMask: ip.SubnetMask,
			})
		}
		if len(spec.IPAddresses) == 0 {
			t.Skip("group's listener reports no addresses to restore it from")
		}

		if err := ag.RemoveListenerContext(ctx, orig.DNSName); err != nil {
			t.Fatalf("RemoveListener: %v", err)
		}
		if err := ag.AddListenerContext(ctx, spec); err != nil {
			t.Fatalf("AddListener (the group is now WITHOUT a listener): %v", err)
		}

		back, err := ag.ListenersContext(ctx)
		if err != nil {
			t.Fatalf("re-read listeners: %v", err)
		}
		if len(back) != 1 {
			t.Fatalf("group has %d listeners after the round trip, want 1", len(back))
		}
		if back[0].DNSName != orig.DNSName || back[0].Port != orig.Port {
			t.Errorf("listener came back as %s:%d, want %s:%d", back[0].DNSName, back[0].Port, orig.DNSName, orig.Port)
		}
		if len(back[0].IPAddresses) != len(orig.IPAddresses) {
			t.Fatalf("listener came back with %d addresses, want %d", len(back[0].IPAddresses), len(orig.IPAddresses))
		}
		for i, ip := range back[0].IPAddresses {
			if ip.IPAddress != orig.IPAddresses[i].IPAddress || ip.SubnetMask != orig.IPAddresses[i].SubnetMask {
				t.Errorf("address %d came back as %s/%s, want %s/%s", i,
					ip.IPAddress, ip.SubnetMask, orig.IPAddresses[i].IPAddress, orig.IPAddresses[i].SubnetMask)
			}
		}
	})

	// The one case where the right behaviour is a refusal. ClusterType is what
	// callers are meant to gate on, so this pins that the gate matches what the
	// server actually does rather than trusting the doc comment.
	t.Run("failover is refused by cluster type", func(t *testing.T) {
		switch ag.ClusterType {
		case "EXTERNAL":
			for name, run := range map[string]func() error{
				"Failover":                   func() error { return ag.FailoverContext(ctx) },
				"ForceFailoverAllowDataLoss": func() error { return ag.ForceFailoverAllowDataLossContext(ctx) },
			} {
				err := run()
				if err == nil {
					t.Fatalf("%s succeeded on an EXTERNAL-cluster group", name)
				}
				if !strings.Contains(err.Error(), "47104") && !strings.Contains(err.Error(), "EXTERNAL cluster type") {
					t.Errorf("%s failed with %v, want the 47104 EXTERNAL-cluster refusal", name, err)
				}
			}
		case "NONE":
			// Only the forced form exists here, so exercising the lossless one
			// is safe and the forced one is not.
			if err := ag.FailoverContext(ctx); err == nil {
				t.Error("Failover succeeded on a CLUSTER_TYPE = NONE group")
			} else if !strings.Contains(err.Error(), "47122") && !strings.Contains(err.Error(), "CLUSTER_TYPE = NONE") {
				t.Errorf("Failover failed with %v, want the 47122 refusal", err)
			}
		default:
			t.Skipf("cluster type %q allows failover; not failing a live group over", ag.ClusterType)
		}
	})
}

// liveWaitForAGDatabase polls the group's databases until one row for dbName
// satisfies want. Seeding, suspending and resuming are all asynchronous, so
// reading once after the ALTER returns races the state it is checking for.
func liveWaitForAGDatabase(t *testing.T, ctx context.Context, ag *AvailabilityGroup, dbName string, want func(*AvailabilityDatabase) bool) *AvailabilityDatabase {
	t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	for {
		dbs, err := ag.DatabasesContext(ctx)
		if err != nil {
			t.Fatalf("Databases: %v", err)
		}
		for _, d := range dbs {
			if strings.EqualFold(d.DatabaseName, dbName) && want(d) {
				return d
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s to reach the expected state", dbName)
		}
		time.Sleep(2 * time.Second)
	}
}

// liveDropEverywhere drops dbName on the primary and on every other replica,
// best effort. The secondaries need doing separately: RemoveDatabase leaves
// each of them holding a copy that is in no role, and nothing the primary can
// run cleans those up.
func liveDropEverywhere(t *testing.T, srv *Server, ag *AvailabilityGroup, dbName string) {
	t.Helper()
	ctx := context.Background()
	liveDropDatabase(t, srv, dbName)
	replicas, err := ag.ReplicasContext(ctx)
	if err != nil {
		t.Logf("listing replicas to clean up %s: %v", dbName, err)
		return
	}
	for _, r := range replicas {
		if strings.EqualFold(r.ReplicaServerName, srv.Name()) {
			continue
		}
		peer, err := Connect(ConnectionOptions{
			Server: r.ReplicaServerName, User: *liveAGUser, Password: *liveAGPass,
			TrustServerCertificate: true, Encrypt: "false",
		})
		if err != nil {
			t.Logf("connecting to %s to clean up %s: %v", r.ReplicaServerName, dbName, err)
			continue
		}
		liveDropDatabase(t, peer, dbName)
		peer.Close()
	}
}

// liveDropDatabase drops dbName if it is there at all, quietly.
//
// The plain DROP comes first because the forcing one does not fall back: it
// runs SET SINGLE_USER and returns on failure without ever issuing the DROP,
// and SET SINGLE_USER is exactly what a copy left behind by RemoveDatabase
// rejects — it is in no role, so every statement against it fails with 983.
func liveDropDatabase(t *testing.T, srv *Server, dbName string) {
	t.Helper()
	ctx := context.Background()
	if err := srv.DropDatabaseContext(ctx, dbName, false); err == nil {
		return
	}
	if err := srv.DropDatabaseContext(ctx, dbName, true); err != nil {
		exists, lookupErr := srv.DatabaseByNameContext(ctx, dbName)
		if lookupErr == nil && exists != nil {
			t.Logf("could not drop %s on %s: %v", dbName, srv.Name(), err)
		}
	}
}

// TestLiveAvailabilityGroupCreate stands up a real availability group across
// two instances and tears it down again — CREATE, JOIN on the secondary, GRANT
// CREATE ANY DATABASE, then DROP on both. Guarded by -liveag-create.
//
//	go test -tags livedb . -run TestLiveAvailabilityGroupCreate -v \
//	  -liveag ubusql1 -liveag-user sa -liveag-pass PASS -liveag-create
//
// The group it builds is CLUSTER_TYPE = NONE deliberately: a read-scale group
// needs no cluster manager, so it can be created and destroyed on a pair of
// Linux instances without touching Pacemaker or the group already running
// there. Everything except the cluster type is the same code path an EXTERNAL
// or WSFC group takes.
//
// The teardown is the part worth reading: dropping the group on the primary
// does *not* clean up the secondary, which keeps a stale sys.availability_groups
// row until DROP runs there too. This test asserts that, because the cleanup
// depends on it.
var (
	liveAGCreate     = flag.Bool("liveag-create", false, "also run the Always On create test, which builds and drops a throwaway availability group")
	liveAGSecondary  = flag.String("liveag-secondary", "", "second instance for -liveag-create; defaults to the other replica of an existing group")
	liveAGCreateName = flag.String("liveag-create-name", "gosmo_agtmp", "name of the throwaway group -liveag-create builds")
)

func TestLiveAvailabilityGroupCreate(t *testing.T) {
	srv, ctx, done := liveAG(t)
	defer done()
	if !*liveAGCreate {
		t.Skip("the create test builds and drops an availability group; pass -liveag-create to run it")
	}

	secondaryName := *liveAGSecondary
	if secondaryName == "" {
		secondaryName = liveOtherReplicaName(t, ctx, srv)
	}
	peer, err := Connect(ConnectionOptions{
		Server: secondaryName, User: *liveAGUser, Password: *liveAGPass,
		TrustServerCertificate: true, Encrypt: "false",
	})
	if err != nil {
		t.Fatalf("connect to secondary %s: %v", secondaryName, err)
	}
	defer peer.Close()

	// Both instances must already have a database mirroring endpoint. Creating
	// one is a separate problem — on Linux it needs a certificate exchanged
	// between the instances first — and an instance that has ever been in an
	// availability group already has exactly one.
	primaryEP := liveEndpoint(t, ctx, srv)
	secondaryEP := liveEndpoint(t, ctx, peer)

	name := *liveAGCreateName
	liveDropGroupEverywhere(t, ctx, name, srv, peer)
	defer liveDropGroupEverywhere(t, ctx, name, srv, peer)

	req := CreateAvailabilityGroupRequest{
		Name:                                    name,
		ClusterType:                             "NONE",
		RequiredSynchronizedSecondariesToCommit: -1,
		Replicas: []AvailabilityReplicaSpec{
			{
				ServerName: srv.Name(), EndpointURL: primaryEP.URL(),
				AvailabilityMode: "SYNCHRONOUS_COMMIT", FailoverMode: "MANUAL",
				SeedingMode: "AUTOMATIC", BackupPriority: -1,
			},
			{
				ServerName: peer.Name(), EndpointURL: secondaryEP.URL(),
				AvailabilityMode: "SYNCHRONOUS_COMMIT", FailoverMode: "MANUAL",
				SeedingMode: "AUTOMATIC", BackupPriority: -1,
			},
		},
	}
	ag, err := srv.CreateAvailabilityGroupContext(ctx, req)
	if err != nil {
		t.Fatalf("CreateAvailabilityGroup: %v", err)
	}
	if !strings.EqualFold(ag.ClusterType, "NONE") {
		t.Errorf("created group's cluster type = %q, want NONE", ag.ClusterType)
	}
	if !ag.IsLocalPrimary() {
		t.Errorf("the instance that ran CREATE is not the group's primary")
	}

	replicas, err := ag.ReplicasContext(ctx)
	if err != nil {
		t.Fatalf("Replicas: %v", err)
	}
	if len(replicas) != 2 {
		t.Fatalf("created group has %d replicas, want 2", len(replicas))
	}
	for _, r := range replicas {
		if !strings.EqualFold(r.SeedingMode, "AUTOMATIC") {
			t.Errorf("replica %s seeding mode = %q, want AUTOMATIC", r.ReplicaServerName, r.SeedingMode)
		}
	}

	// The secondary joins itself, blind: under CLUSTER_TYPE = NONE the group
	// does not exist there at all until the JOIN lands, so there is nothing to
	// read first and the handle has to be built from the name.
	if _, err := peer.AvailabilityGroupByNameContext(ctx, name); err == nil {
		t.Errorf("%s already knows the group before joining — only a WSFC cluster propagates that, "+
			"and AvailabilityGroup.Join's doc comment says otherwise", secondaryName)
	}
	peerAG := peer.AvailabilityGroup(name)
	if err := peerAG.JoinContext(ctx, "NONE"); err != nil {
		t.Fatalf("Join on %s: %v", secondaryName, err)
	}
	if err := peerAG.GrantCreateAnyDatabaseContext(ctx); err != nil {
		t.Fatalf("GrantCreateAnyDatabase on %s: %v", secondaryName, err)
	}

	// Joining is asynchronous: the connection comes up a moment later.
	deadline := time.Now().Add(60 * time.Second)
	for {
		replicas, err := ag.ReplicasContext(ctx)
		if err != nil {
			t.Fatalf("Replicas after join: %v", err)
		}
		joined := 0
		for _, r := range replicas {
			if strings.EqualFold(r.ConnectedState, "CONNECTED") {
				joined++
			}
		}
		if joined == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("only %d of 2 replicas connected within the timeout", joined)
		}
		time.Sleep(2 * time.Second)
	}

	// AVAILABILITY_MODE and FAILOVER_MODE are the two replica settings
	// TestLiveAvailabilityGroupWrite deliberately skips: on the standing
	// EXTERNAL group, dropping the last synchronous replica leaves Pacemaker
	// unable to promote anything, recoverable only by recreating the group.
	// This throwaway group is where they can be exercised for real — it is
	// CLUSTER_TYPE = NONE, no cluster manager watches it, and it is dropped
	// a few lines below however this subtest ends.
	t.Run("availability and failover modes", func(t *testing.T) {
		rereadReplica := func(name string) *AvailabilityReplica {
			t.Helper()
			rs, err := ag.ReplicasContext(ctx)
			if err != nil {
				t.Fatalf("re-read replicas: %v", err)
			}
			for _, r := range rs {
				if strings.EqualFold(r.ReplicaServerName, name) {
					return r
				}
			}
			t.Fatalf("replica %q vanished", name)
			return nil
		}

		target := rereadReplica(peer.Name())
		if !strings.EqualFold(target.AvailabilityMode, "SYNCHRONOUS_COMMIT") {
			t.Fatalf("secondary starts at AvailabilityMode %q, want SYNCHRONOUS_COMMIT", target.AvailabilityMode)
		}

		if err := target.SetAvailabilityModeContext(ctx, "ASYNCHRONOUS_COMMIT"); err != nil {
			t.Fatalf("SetAvailabilityMode ASYNCHRONOUS_COMMIT: %v", err)
		}
		if got := rereadReplica(peer.Name()).AvailabilityMode; !strings.EqualFold(got, "ASYNCHRONOUS_COMMIT") {
			t.Errorf("AvailabilityMode = %q, want ASYNCHRONOUS_COMMIT", got)
		}
		if err := target.SetAvailabilityModeContext(ctx, "SYNCHRONOUS_COMMIT"); err != nil {
			t.Fatalf("restore AvailabilityMode: %v", err)
		}
		if got := rereadReplica(peer.Name()).AvailabilityMode; !strings.EqualFold(got, "SYNCHRONOUS_COMMIT") {
			t.Errorf("restored AvailabilityMode = %q, want SYNCHRONOUS_COMMIT", got)
		}

		// FAILOVER_MODE cannot be round-tripped anywhere available here: a
		// CLUSTER_TYPE = NONE group accepts MANUAL and nothing else, and the
		// two it refuses need infrastructure this cluster does not have —
		// AUTOMATIC a WSFC, EXTERNAL a group created as EXTERNAL, which is
		// AAG1, the one group that must not be touched. So what is pinned is
		// the mode that applies plus the server's own refusal of the other
		// two: both come back "The cluster type of availability group '...'
		// only supports MANUAL failover mode", which is a semantic refusal —
		// a malformed statement would be Msg 102 instead, so this still
		// proves the statement reaches the server well-formed.
		if err := target.SetFailoverModeContext(ctx, "MANUAL"); err != nil {
			t.Fatalf("SetFailoverMode MANUAL: %v", err)
		}
		if got := rereadReplica(peer.Name()).FailoverMode; !strings.EqualFold(got, "MANUAL") {
			t.Errorf("FailoverMode = %q, want MANUAL", got)
		}
		for _, mode := range []string{"AUTOMATIC", "EXTERNAL"} {
			err := target.SetFailoverModeContext(ctx, mode)
			if err == nil {
				t.Errorf("SetFailoverMode %s on a NONE group succeeded; it should be refused", mode)
				continue
			}
			if !strings.Contains(err.Error(), "only supports MANUAL failover mode") {
				t.Errorf("SetFailoverMode %s: err = %v, want the cluster-type refusal", mode, err)
			}
			// A refused write must not move the in-memory field: setIfApplied
			// runs only after modifyReplica returns nil, and a caller that
			// mirrored optimistically would show the mode as changed.
			if !strings.EqualFold(target.FailoverMode, "MANUAL") {
				t.Errorf("after a refused SetFailoverMode %s, the replica reports FailoverMode %q, want MANUAL",
					mode, target.FailoverMode)
			}
			if got := rereadReplica(peer.Name()).FailoverMode; !strings.EqualFold(got, "MANUAL") {
				t.Errorf("after a refused SetFailoverMode %s, the server reports %q, want MANUAL", mode, got)
			}
		}
	})

	// The stale-row behaviour the teardown depends on: dropping on the primary
	// leaves the secondary still listing the group.
	if err := ag.DropContext(ctx); err != nil {
		t.Fatalf("Drop on the primary: %v", err)
	}
	if _, err := srv.AvailabilityGroupByNameContext(ctx, name); err == nil {
		t.Errorf("the group is still readable on the primary after Drop")
	}
	stale, err := peer.AvailabilityGroupByNameContext(ctx, name)
	if err != nil || stale == nil {
		t.Errorf("the secondary no longer lists the group after the primary dropped it (%v) — "+
			"if this is now cleaned up automatically, AvailabilityGroup.RemoveReplica's doc comment is out of date", err)
		return
	}
	if err := stale.DropContext(ctx); err != nil {
		t.Fatalf("Drop the stale row on %s: %v", secondaryName, err)
	}
}

// liveOtherReplicaName picks a second instance out of whatever availability
// group srv is already in, so the create test needs no extra flag on a cluster
// that already has one.
func liveOtherReplicaName(t *testing.T, ctx context.Context, srv *Server) string {
	t.Helper()
	groups, err := srv.AvailabilityGroupsContext(ctx)
	if err != nil {
		t.Fatalf("AvailabilityGroupsContext: %v", err)
	}
	for _, g := range groups {
		replicas, err := g.ReplicasContext(ctx)
		if err != nil {
			continue
		}
		for _, r := range replicas {
			if !strings.EqualFold(r.ReplicaServerName, srv.Name()) {
				return r.ReplicaServerName
			}
		}
	}
	t.Skip("no second instance to build a group with; pass -liveag-secondary")
	return ""
}

func liveEndpoint(t *testing.T, ctx context.Context, srv *Server) *DatabaseMirroringEndpoint {
	t.Helper()
	ep, err := srv.DatabaseMirroringEndpointContext(ctx)
	if err != nil {
		t.Fatalf("read the database mirroring endpoint on %s: %v", srv.Name(), err)
	}
	if ep == nil {
		t.Skipf("%s has no database mirroring endpoint; creating one needs a certificate exchange this test will not do", srv.Name())
	}
	if !strings.EqualFold(ep.State, "STARTED") {
		t.Skipf("%s's endpoint %q is %s, not STARTED", srv.Name(), ep.Name, ep.State)
	}
	return ep
}

// liveDropGroupEverywhere drops name on every given instance, best effort.
// Both halves are needed: see the stale-row assertion above.
func liveDropGroupEverywhere(t *testing.T, ctx context.Context, name string, servers ...*Server) {
	t.Helper()
	for _, srv := range servers {
		ag, err := srv.AvailabilityGroupByNameContext(ctx, name)
		if err != nil || ag == nil {
			continue
		}
		if err := ag.DropContext(ctx); err != nil {
			t.Logf("dropping %s on %s: %v", name, srv.Name(), err)
		}
	}
}
