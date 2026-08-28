//go:build livedb

// Live verification that every name in the Probed* lists is a permission SQL
// Server actually defines. A misspelt or non-existent name is not an error:
// HAS_PERMS_BY_NAME answers NULL for it, the probe records CapabilityUnknown,
// and Allows/Permits then fail open forever. Nothing at run time tells that
// apart from a login that holds the right, so it has to be pinned here.
//
//	go test -tags livedb . -run TestLiveEveryProbedPermission -v \
//	  -livedb 'sqlserver://sa:PASS@host?TrustServerCertificate=true'
//
// Runs as the connecting login, which must be a sysadmin: every real name then
// answers Granted, and only a name the instance does not define reads Unknown.
// Creates and drops one throwaway database; touches nothing else.
package gosmo

import "testing"

func TestLiveEveryProbedPermissionNameIsOneTheServerDefines(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()

	srv, err := NewServer(ctx, db)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	caps, err := srv.CapabilitiesContext(ctx)
	if err != nil {
		t.Fatalf("CapabilitiesContext: %v", err)
	}
	for _, name := range ProbedServerPermissions {
		if got := caps.Permission(name); got == CapabilityUnknown {
			t.Errorf("server permission %q reads %v — the instance does not define that name, "+
				"so it gates nothing", name, got)
		}
	}

	d, drop := liveScratchDB(t, db, ctx, "gosmo_probednames_live")
	defer drop()

	dcaps, err := d.CapabilitiesContext(ctx)
	if err != nil {
		t.Fatalf("CapabilitiesContext for %s: %v", d.Name(), err)
	}
	if !dcaps.Accessible {
		t.Fatalf("scratch database %s reads inaccessible", d.Name())
	}
	for _, name := range ProbedDatabasePermissions {
		if got := dcaps.Permission(name); got == CapabilityUnknown {
			t.Errorf("database permission %q reads %v — the instance does not define that name, "+
				"so it gates nothing", name, got)
		}
	}
	// dbo is the one schema every database has, so the schema block always
	// has a row to answer from.
	for _, name := range ProbedSchemaPermissions {
		if got := dcaps.SchemaPermission("dbo", name); got == CapabilityUnknown {
			t.Errorf("schema permission %q reads %v on dbo — the instance does not define that name, "+
				"so it gates nothing", name, got)
		}
	}
}
