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
	// VIEW SERVER PERFORMANCE STATE and VIEW SERVER SECURITY STATE are the
	// two rights SQL Server 2022 split VIEW SERVER STATE into; an older
	// instance does not define them, which is not a typo in the list. They are
	// probed as alternatives to the wide right, which exists everywhere.
	since2022 := map[string]bool{
		"VIEW SERVER PERFORMANCE STATE": true,
		"VIEW SERVER SECURITY STATE":    true,
	}
	major := srv.serverMajorVersion()
	for _, name := range ProbedServerPermissions {
		if since2022[name] && !hasColumnSince(major, SQLServer2022) {
			t.Logf("server permission %q is 2022-only; major %d does not define it", name, major)
			continue
		}
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
	// External libraries arrived with Machine Learning Services in 2017.
	since2017 := map[string]bool{"ALTER ANY EXTERNAL LIBRARY": true}
	for _, name := range ProbedDatabasePermissions {
		if since2017[name] && !hasColumnSince(major, SQLServer2017) {
			t.Logf("database permission %q is 2017+; major %d does not define it", name, major)
			continue
		}
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
