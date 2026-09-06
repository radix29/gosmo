//go:build livedb

// Live verification of Capabilities' AVAILABILITY GROUP (class 108) block:
// what a class-108 DENY actually withholds, and what it leaves alone.
//
//	go test -tags livedb . -run TestLiveAvailabilityGroupCapabilities -v \
//	  -livedb 'sqlserver://sa:PASS@primary?TrustServerCertificate=true'
//
// Must be run against the *primary* replica of a real availability group, and
// skips itself where there is none. It creates and drops a throwaway login and
// nothing else: every statement it asks the server to judge is either a no-op
// on success or names a database that does not exist, so a permitted one
// changes nothing on the cluster.
package gosmo

import (
	"context"
	"database/sql"
	"strings"
	"testing"
)

// TestLiveAvailabilityGroupCapabilitiesMatchWhatTheServerEnforces pins the
// class-108 answer, measured 2026-09-05 on a two-node Pacemaker cluster
// (major 17) because win10cli cannot host an availability group at all.
//
// The shape is the login's all-or-nothing rather than the server role's split:
// under DENY ALTER ON AVAILABILITY GROUP::g, every ALTER AVAILABILITY GROUP
// there is — the options SET, ADD/REMOVE DATABASE, MODIFY REPLICA and FAILOVER
// — is refused with Msg 15151, while the server-wide
// ALTER ANY AVAILABILITY GROUP reads 1 throughout.
//
// ALTER DATABASE ... SET HADR SUSPEND / RESUME is the exception, and the one
// worth not rediscovering: it is checked against the *database* and goes
// through with the group's DENY in place, so a gate must not withhold
// Suspend/Resume on this answer.
func TestLiveAvailabilityGroupCapabilitiesMatchWhatTheServerEnforces(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()

	var ag string
	// The primary, and only the primary: ALTER AVAILABILITY GROUP is refused
	// on a secondary for a reason that has nothing to do with permissions, so
	// a run there would read every statement below as denied.
	err := db.QueryRowContext(ctx, `SELECT TOP 1 ag.name
		FROM sys.availability_groups AS ag
		JOIN sys.dm_hadr_availability_replica_states AS ars ON ars.group_id = ag.group_id
		WHERE ars.is_local = 1 AND ars.role_desc = 'PRIMARY'`).Scan(&ag)
	if err != nil {
		t.Skipf("no local primary availability group on this instance: %v", err)
	}

	const login = "gosmo_live_agcaps"
	const pass = "P@ssw0rd_gosmo_live"

	db.ExecContext(ctx, "IF SUSER_ID('"+login+"') IS NOT NULL DROP LOGIN ["+login+"]")
	if _, err := db.ExecContext(ctx, "CREATE LOGIN ["+login+"] WITH PASSWORD = '"+pass+"', CHECK_POLICY = OFF"); err != nil {
		t.Fatalf("create login: %v", err)
	}
	defer db.ExecContext(context.Background(), "DROP LOGIN ["+login+"]")

	for _, stmt := range []string{
		"GRANT ALTER ANY AVAILABILITY GROUP TO [" + login + "]",
		"GRANT ALTER ANY DATABASE TO [" + login + "]",
		"DENY ALTER ON AVAILABILITY GROUP::[" + ag + "] TO [" + login + "]",
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}

	pool, err := sql.Open("sqlserver", liveRestrictedDSN(t, login, pass))
	if err != nil {
		t.Fatalf("open as %s: %v", login, err)
	}
	defer pool.Close()

	// Not NewServer, for live_schemacaps_test.go's reason.
	caps, err := (&Server{db: pool}).CapabilitiesContext(ctx)
	if err != nil {
		t.Fatalf("CapabilitiesContext as %s: %v", login, err)
	}
	// The server-wide grant is intact throughout — the fixture is worthless
	// without this, since a denial that only agreed with a right the login had
	// lost would prove nothing.
	if !caps.Allows("ALTER ANY AVAILABILITY GROUP") {
		t.Fatal("ALTER ANY AVAILABILITY GROUP did not read back; this fixture no longer isolates the class-108 DENY")
	}
	if caps.PermitsOnAvailabilityGroup(ag, "ALTER") {
		t.Errorf("the class-108 DENY on %s did not read back", ag)
	}

	// The oracle: what the server actually accepts from this login. Each
	// statement is a no-op or names a database that does not exist, so a
	// permitted one leaves the cluster as it was.
	as := func(stmt string) error {
		_, err := pool.ExecContext(ctx, stmt)
		return err
	}
	for _, tc := range []struct{ what, stmt string }{
		{"the options SET", "ALTER AVAILABILITY GROUP [" + ag + "] SET (DB_FAILOVER = ON)"},
		{"ADD DATABASE", "ALTER AVAILABILITY GROUP [" + ag + "] ADD DATABASE [gosmo_no_such_db]"},
		{"REMOVE DATABASE", "ALTER AVAILABILITY GROUP [" + ag + "] REMOVE DATABASE [gosmo_no_such_db]"},
		{"FAILOVER", "ALTER AVAILABILITY GROUP [" + ag + "] FAILOVER"},
	} {
		err := as(tc.stmt)
		if err == nil {
			t.Errorf("the server performed %s on a group carrying a class-108 DENY — "+
				"PermitsOnAvailabilityGroup would be withholding nothing real", tc.what)
			continue
		}
		// Msg 15151, the same number every class-101 refusal carries. A
		// different one means the statement failed for its own reasons — the
		// nonexistent database, say — and proves nothing about the permission.
		if !strings.Contains(err.Error(), ag) {
			t.Errorf("%s failed for some other reason than the denial: %v", tc.what, err)
		}
	}
}
