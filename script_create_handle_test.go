package gosmo

import (
	"context"
	"testing"
)

// The scripted creates. Each of these builds a statement, execs it, and then
// reads the object back by name — a read that cannot happen under WithScript,
// where nothing was created. Every one therefore branches on Scripting(ctx)
// and hands back a name-only handle instead, which is what the caller's next
// scripted step addresses. The statement builders are pinned in their own
// files; what these pin is the wrapper around them: exactly one statement,
// and a usable handle rather than a nil or a query against an object that
// does not exist.
//
// See script_write_common_test.go for why the values are quote-hostile.
func TestScriptedCreatesEmitOneStatement(t *testing.T) {
	runScriptCases(t, []scriptCase{
		{"CreateServerAudit", func(c context.Context) error {
			_, err := (&Server{}).CreateServerAuditContext(c, ServerAuditSpec{
				Name: "aud]1", Type: AuditToSecurityLog, OnFailure: AuditFailureContinue,
			})
			return err
		}, "CREATE SERVER AUDIT [aud]]1]\nTO SECURITY_LOG\nWITH ( QUEUE_DELAY = 0, ON_FAILURE = CONTINUE )"},
		{"CreateServerAuditSpecification", func(c context.Context) error {
			_, err := (&Server{}).CreateServerAuditSpecificationContext(c, ServerAuditSpecificationSpec{
				Name: "spec'1", AuditName: "aud]1", ActionGroups: []string{"FAILED_LOGIN_GROUP"},
			})
			return err
		}, "CREATE SERVER AUDIT SPECIFICATION [spec'1]\nFOR SERVER AUDIT [aud]]1]\n" +
			"    ADD (FAILED_LOGIN_GROUP)\nWITH ( STATE = OFF )"},
		{"CreateDatabaseAuditSpecification", func(c context.Context) error {
			_, err := scriptTestDB().CreateDatabaseAuditSpecificationContext(c, DatabaseAuditSpecificationSpec{
				Name: "spec'1", AuditName: "aud]1",
				ActionGroups: []string{"SCHEMA_OBJECT_ACCESS_GROUP"}, Enabled: true,
			})
			return err
		}, scriptUsePrefix + "CREATE DATABASE AUDIT SPECIFICATION [spec'1]\nFOR SERVER AUDIT [aud]]1]\n" +
			"    ADD (SCHEMA_OBJECT_ACCESS_GROUP)\nWITH ( STATE = ON )"},
		{"CreateDatabaseSnapshot", func(c context.Context) error {
			_, err := (&Server{}).CreateDatabaseSnapshotContext(c, CreateDatabaseSnapshotRequest{
				Name: "App'DB_snap", SourceDatabase: "App'DB",
				Files: []SnapshotFileSpec{{LogicalName: "App]Data", FileName: `C:\snap\a'1.ss`}},
			})
			return err
		}, "CREATE DATABASE [App'DB_snap] ON\n    ( NAME = [App]]Data], FILENAME = 'C:\\snap\\a''1.ss' )\n" +
			"AS SNAPSHOT OF [App'DB]"},
		{"AddListener", func(c context.Context) error {
			return (&AvailabilityGroup{server: &Server{}, Name: "AA]G1"}).AddListenerContext(c,
				AvailabilityListenerSpec{DNSName: "aaglsn", Port: 1433,
					IPAddresses: []AvailabilityListenerIPSpec{{IPAddress: "10.0.0.9", SubnetMask: "255.255.255.0"}}})
		}, "ALTER AVAILABILITY GROUP [AA]]G1] ADD LISTENER N'aaglsn' " +
			"(WITH IP ((N'10.0.0.9', N'255.255.255.0')), PORT = 1433)"},
	})
}

// The handles those creates hand back. A create whose read-back is skipped
// must still name the object it made — a nil, or a handle with an empty name,
// leaves the next scripted step with nothing to address.
func TestScriptedCreatesReturnANamedHandle(t *testing.T) {
	ctx, _ := WithScript(context.Background())
	srv := &Server{}

	audit, err := srv.CreateServerAuditContext(ctx, ServerAuditSpec{
		Name: "aud]1", Type: AuditToSecurityLog,
	})
	if err != nil {
		t.Fatalf("CreateServerAuditContext: %v", err)
	}
	if audit == nil || audit.Name != "aud]1" {
		t.Errorf("server audit handle = %+v, want one named aud]1", audit)
	}

	spec, err := srv.CreateServerAuditSpecificationContext(ctx, ServerAuditSpecificationSpec{
		Name: "spec'1", AuditName: "aud]1",
	})
	if err != nil {
		t.Fatalf("CreateServerAuditSpecificationContext: %v", err)
	}
	if spec == nil || spec.Name != "spec'1" {
		t.Errorf("server audit specification handle = %+v, want one named spec'1", spec)
	}

	dbSpec, err := scriptTestDB().CreateDatabaseAuditSpecificationContext(ctx,
		DatabaseAuditSpecificationSpec{Name: "spec'1", AuditName: "aud]1"})
	if err != nil {
		t.Fatalf("CreateDatabaseAuditSpecificationContext: %v", err)
	}
	if dbSpec == nil || dbSpec.Name != "spec'1" {
		t.Errorf("database audit specification handle = %+v, want one named spec'1", dbSpec)
	}

	snap, err := srv.CreateDatabaseSnapshotContext(ctx, CreateDatabaseSnapshotRequest{
		Name: "App'DB_snap", SourceDatabase: "App'DB",
		Files: []SnapshotFileSpec{{LogicalName: "App]Data", FileName: `C:\snap\a'1.ss`}},
	})
	if err != nil {
		t.Fatalf("CreateDatabaseSnapshotContext: %v", err)
	}
	if snap == nil || snap.Name != "App'DB_snap" || snap.SourceDatabase != "App'DB" {
		t.Errorf("snapshot handle = %+v, want one named App'DB_snap of App'DB", snap)
	}

	// The group's handle carries its cluster type as well as its name: the
	// join that follows a scripted create has to name it back, and under
	// EXTERNAL or NONE there is no metadata anywhere to read it from.
	ag, err := srv.CreateAvailabilityGroupContext(ctx, CreateAvailabilityGroupRequest{
		Name: "AA]G1", ClusterType: "external", RequiredSynchronizedSecondariesToCommit: -1,
		Replicas: []AvailabilityReplicaSpec{
			{ServerName: "ubusql1", EndpointURL: "tcp://ubusql1:5022", BackupPriority: -1},
		},
	})
	if err != nil {
		t.Fatalf("CreateAvailabilityGroupContext: %v", err)
	}
	if ag == nil || ag.Name != "AA]G1" || ag.ClusterType != "EXTERNAL" {
		t.Errorf("availability group handle = %+v, want AA]G1 with cluster type EXTERNAL", ag)
	}
}
