//go:build livedb

// Live Resource Governor reads: create a disposable pool, workload group and
// external pool with raw DDL, read them back through gosmo, and check what a
// login without VIEW ANY DEFINITION / VIEW SERVER STATE is told.
//
//	go test -tags livedb . -run TestLiveResourceGovernor -v \
//	  -livedb 'sqlserver://sa:PASS@host?TrustServerCertificate=true'
//
// The governor is server-wide state on a shared instance. The test runs only
// on one found disabled with nothing pending, and restores exactly that: the
// fixtures are dropped and ALTER RESOURCE GOVERNOR DISABLE clears the pending
// flag the DDL set, without enabling anything (a RECONFIGURE would enable it).
package gosmo

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
)

const (
	liveRGPool     = "gosmo_live_rg_pool"
	liveRGGroup    = "gosmo_live_rg_group"
	liveRGExtPool  = "gosmo_live_rg_ext"
	liveRGLogin    = "gosmo_live_rg_login"
	liveRGPassword = "P@ssw0rd_gosmo_live_rg"
)

func TestLiveResourceGovernorReads(t *testing.T) {
	db, ctx, done := liveDB(t)
	t.Cleanup(done) // registered first, so it runs after every drop below

	srv, err := NewServer(ctx, db)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	major := srv.serverMajorVersion()

	rg, err := srv.ResourceGovernor(ctx)
	if err != nil {
		t.Fatalf("ResourceGovernor: %v", err)
	}
	st, err := srv.ResourceGovernorStatus(ctx)
	if err != nil {
		t.Fatalf("ResourceGovernorStatus: %v", err)
	}
	if rg.IsEnabled || st.IsReconfigurationPending {
		t.Skipf("resource governor is enabled (%v) or has a pending change (%v) — "+
			"this test only runs where it can restore the state exactly", rg.IsEnabled, st.IsReconfigurationPending)
	}
	if st.MaxOutstandingIOPerVolume == 0 {
		t.Error("ResourceGovernorStatus.MaxOutstandingIOPerVolume is 0 — the effective value is never 0")
	}

	exec := func(stmt string) {
		t.Helper()
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	cleanup := func(stmt string) {
		t.Cleanup(func() {
			if _, err := db.ExecContext(context.Background(), stmt); err != nil {
				t.Errorf("cleanup %s: %v", stmt, err)
			}
		})
	}

	// Cleanups run last-registered first: group, pool, external pool, then
	// DISABLE to clear the pending flag.
	cleanup("ALTER RESOURCE GOVERNOR DISABLE")

	extPool := false
	if _, err := db.ExecContext(ctx, "CREATE EXTERNAL RESOURCE POOL ["+liveRGExtPool+
		"] WITH (MAX_CPU_PERCENT = 40, MAX_MEMORY_PERCENT = 30, AFFINITY CPU = (0))"); err != nil {
		t.Logf("external pool not created, its read is checked on default only: %v", err)
	} else {
		extPool = true
		cleanup("DROP EXTERNAL RESOURCE POOL [" + liveRGExtPool + "]")
	}

	exec("CREATE RESOURCE POOL [" + liveRGPool + "] WITH (MIN_CPU_PERCENT = 5, MAX_CPU_PERCENT = 60, CAP_CPU_PERCENT = 70, " +
		"MIN_MEMORY_PERCENT = 3, MAX_MEMORY_PERCENT = 50, MIN_IOPS_PER_VOLUME = 10, MAX_IOPS_PER_VOLUME = 500, " +
		"AFFINITY SCHEDULER = (0))")
	cleanup("DROP RESOURCE POOL [" + liveRGPool + "]")

	groupOpts := "IMPORTANCE = HIGH, REQUEST_MAX_MEMORY_GRANT_PERCENT = 12, REQUEST_MAX_CPU_TIME_SEC = 30, " +
		"REQUEST_MEMORY_GRANT_TIMEOUT_SEC = 20, MAX_DOP = 2, GROUP_MAX_REQUESTS = 7"
	wantGrant := 12.0
	if major == 0 || major >= int(SQLServer2019) {
		groupOpts = "IMPORTANCE = HIGH, REQUEST_MAX_MEMORY_GRANT_PERCENT = 12.5, REQUEST_MAX_CPU_TIME_SEC = 30, " +
			"REQUEST_MEMORY_GRANT_TIMEOUT_SEC = 20, MAX_DOP = 2, GROUP_MAX_REQUESTS = 7"
		wantGrant = 12.5
	}
	tempdb := major == 0 || major >= int(SQLServer2025)
	if tempdb {
		groupOpts += ", GROUP_MAX_TEMPDB_DATA_MB = 256"
	}
	using := "[" + liveRGPool + "]"
	if extPool {
		using += ", EXTERNAL [" + liveRGExtPool + "]"
	}
	exec("CREATE WORKLOAD GROUP [" + liveRGGroup + "] WITH (" + groupOpts + ") USING " + using)
	cleanup("DROP WORKLOAD GROUP [" + liveRGGroup + "]")

	// Pools.
	pools, err := srv.ResourcePools(ctx)
	if err != nil {
		t.Fatalf("ResourcePools: %v", err)
	}
	var system int
	for _, p := range pools {
		if p.IsSystem() {
			system++
		}
	}
	if system != 2 {
		t.Errorf("ResourcePools has %d system pools, want internal and default", system)
	}
	p, err := srv.ResourcePoolByName(ctx, liveRGPool)
	if err != nil {
		t.Fatalf("ResourcePoolByName: %v", err)
	}
	if p.IsSystem() || p.MinCPUPercent != 5 || p.MaxCPUPercent != 60 || p.CapCPUPercent != 70 ||
		p.MinMemoryPercent != 3 || p.MaxMemoryPercent != 50 || p.MinIOPSPerVolume != 10 || p.MaxIOPSPerVolume != 500 {
		t.Errorf("pool read back as %+v", *p)
	}
	if len(p.Affinity) != 1 || p.Affinity[0].ProcessorGroup != 0 || p.Affinity[0].SchedulerMask != 1 {
		t.Errorf("pool affinity = %+v, want processor group 0, scheduler mask 1", p.Affinity)
	}
	if _, err := srv.ResourcePoolByName(ctx, "gosmo_live_rg_absent"); !errors.Is(err, ErrNotFound) {
		t.Errorf("ResourcePoolByName(absent) = %v, want ErrNotFound", err)
	}

	// Groups, through both listings and the finder.
	groups, err := p.WorkloadGroups(ctx)
	if err != nil {
		t.Fatalf("ResourcePool.WorkloadGroups: %v", err)
	}
	if len(groups) != 1 || groups[0].Name != liveRGGroup {
		t.Fatalf("ResourcePool.WorkloadGroups = %d groups, want only %s", len(groups), liveRGGroup)
	}
	g := groups[0]
	if g.PoolName != liveRGPool || g.PoolID != p.ID || g.Importance != "High" ||
		g.RequestMaxMemoryGrantPercent != wantGrant || g.RequestMaxCPUTimeSec != 30 ||
		g.RequestMemoryGrantTimeoutSec != 20 || g.MaxDOP != 2 || g.GroupMaxRequests != 7 {
		t.Errorf("group read back as %+v", *g)
	}
	switch {
	case tempdb && (g.GroupMaxTempdbDataMB == nil || *g.GroupMaxTempdbDataMB != 256 || g.GroupMaxTempdbDataPercent != nil):
		t.Errorf("tempdb limits = %v MB / %v %%, want 256 MB and no percent", g.GroupMaxTempdbDataMB, g.GroupMaxTempdbDataPercent)
	case !tempdb && (g.GroupMaxTempdbDataMB != nil || g.GroupMaxTempdbDataPercent != nil):
		t.Errorf("tempdb limits set on major %d, which has none", major)
	}
	if extPool && g.ExternalPoolName != liveRGExtPool {
		t.Errorf("group's external pool = %q, want %q", g.ExternalPoolName, liveRGExtPool)
	}
	if !extPool && g.ExternalPoolName != "default" {
		t.Errorf("group's external pool = %q, want default", g.ExternalPoolName)
	}
	byName, err := srv.WorkloadGroupByName(ctx, liveRGGroup)
	if err != nil || !reflect.DeepEqual(byName, g) {
		t.Errorf("WorkloadGroupByName = %+v, %v; want the listing's row %+v", byName, err, g)
	}
	all, err := srv.WorkloadGroups(ctx)
	if err != nil {
		t.Fatalf("WorkloadGroups: %v", err)
	}
	if len(all) < 3 {
		t.Errorf("WorkloadGroups = %d groups, want internal, default and %s", len(all), liveRGGroup)
	}

	// External pools.
	ext, err := srv.ExternalResourcePools(ctx)
	if err != nil {
		t.Fatalf("ExternalResourcePools: %v", err)
	}
	if len(ext) == 0 {
		t.Error("ExternalResourcePools is empty — default always exists")
	}
	if extPool {
		ep, err := srv.ExternalResourcePoolByName(ctx, liveRGExtPool)
		if err != nil {
			t.Fatalf("ExternalResourcePoolByName: %v", err)
		}
		if ep.IsSystem() || ep.MaxCPUPercent != 40 || ep.MaxMemoryPercent != 30 ||
			len(ep.Affinity) != 1 || ep.Affinity[0].CPUMask != 1 {
			t.Errorf("external pool read back as %+v", *ep)
		}
	}

	// The DDL set the pending flag, even disabled; the pool is not in force.
	st, err = srv.ResourceGovernorStatus(ctx)
	if err != nil {
		t.Fatalf("ResourceGovernorStatus after DDL: %v", err)
	}
	if !st.IsReconfigurationPending {
		t.Error("IsReconfigurationPending is false after CREATE RESOURCE POOL")
	}
	pstats, err := srv.ResourcePoolStats(ctx)
	if err != nil {
		t.Fatalf("ResourcePoolStats: %v", err)
	}
	for _, ps := range pstats {
		if ps.Name == liveRGPool {
			t.Errorf("%s has a runtime row before RECONFIGURE", liveRGPool)
		}
	}
	if len(pstats) < 2 {
		t.Errorf("ResourcePoolStats = %d rows, want at least internal and default", len(pstats))
	}
	gstats, err := srv.WorkloadGroupStats(ctx)
	if err != nil {
		t.Fatalf("WorkloadGroupStats: %v", err)
	}
	if len(gstats) < 2 {
		t.Errorf("WorkloadGroupStats = %d rows, want at least internal and default", len(gstats))
	}
}

// A login without VIEW ANY DEFINITION sees an empty catalog, not an error, and
// the DMV reads fail on it. What gosmo reports for each is the contract the
// tree relies on to say "not visible" rather than "no pools".
func TestLiveResourceGovernorInvisibleToALoginWithoutRights(t *testing.T) {
	db, ctx, done := liveDB(t)
	t.Cleanup(done)

	db.ExecContext(ctx, "IF SUSER_ID('"+liveRGLogin+"') IS NOT NULL DROP LOGIN ["+liveRGLogin+"]")
	if _, err := db.ExecContext(ctx, "CREATE LOGIN ["+liveRGLogin+"] WITH PASSWORD = '"+liveRGPassword+"', CHECK_POLICY = OFF"); err != nil {
		t.Fatalf("create login: %v", err)
	}
	t.Cleanup(func() { db.ExecContext(context.Background(), "DROP LOGIN ["+liveRGLogin+"]") })

	pool, err := sql.Open("sqlserver", liveRestrictedDSN(t, liveRGLogin, liveRGPassword))
	if err != nil {
		t.Fatalf("open as %s: %v", liveRGLogin, err)
	}
	t.Cleanup(func() { pool.Close() })
	// Not NewServer: loadInfo's DMV half needs rights this login lacks. The
	// zero major reads as newest, which is right for the 17 instance and
	// harmless for the catalog views, which return nothing here anyway.
	restricted := &Server{db: pool}

	if _, err := restricted.ResourceGovernor(ctx); !errors.Is(err, ErrNotFound) {
		t.Errorf("ResourceGovernor = %v, want ErrNotFound (no configuration row is visible)", err)
	}
	for name, list := range map[string]func() (int, error){
		"ResourcePools": func() (int, error) { v, err := restricted.ResourcePools(ctx); return len(v), err },
		"ExternalResourcePools": func() (int, error) {
			v, err := restricted.ExternalResourcePools(ctx)
			return len(v), err
		},
	} {
		if n, err := list(); err != nil || n != 0 {
			t.Errorf("%s = %d rows, %v; want 0 rows and no error", name, n, err)
		}
	}
	if _, err := restricted.ResourceGovernorStatus(ctx); err == nil {
		t.Error("ResourceGovernorStatus succeeded without VIEW SERVER STATE")
	}
	if _, err := restricted.ResourcePoolStats(ctx); err == nil {
		t.Error("ResourcePoolStats succeeded without VIEW SERVER STATE")
	}
}

const (
	liveRGWPool       = "gosmo_live_rgw_pool"
	liveRGWGroup      = "gosmo_live_rgw_group"
	liveRGWExtPool    = "gosmo_live_rgw_ext"
	liveRGWClassifier = "gosmo_live_rgw_classifier"
)

// TestLiveResourceGovernorWrites drives every write through gosmo, reads each
// back, checks the refusals the write methods pass through, and runs the
// scripter's output against the server to show it recreates what it read.
//
// Like the reads test it runs only on a governor found disabled with nothing
// pending and no classifier, and leaves it that way: classifier removed and
// reconfigured (so the function can be dropped), fixtures dropped, stored
// I/O setting back to DEFAULT, then DISABLE.
func TestLiveResourceGovernorWrites(t *testing.T) {
	db, ctx, done := liveDB(t)
	t.Cleanup(done)

	srv, err := NewServer(ctx, db)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	major := srv.serverMajorVersion()
	rg, err := srv.ResourceGovernor(ctx)
	if err != nil {
		t.Fatalf("ResourceGovernor: %v", err)
	}
	st, err := srv.ResourceGovernorStatus(ctx)
	if err != nil {
		t.Fatalf("ResourceGovernorStatus: %v", err)
	}
	if rg.IsEnabled || st.IsReconfigurationPending || rg.ClassifierFunctionID != 0 || rg.MaxOutstandingIOPerVolume != 0 {
		t.Skipf("resource governor is not in its pristine state (%+v, pending %v) — "+
			"this test only runs where it can restore the state exactly", *rg, st.IsReconfigurationPending)
	}

	bg := context.Background()
	cleanup := func(what string, f func() error) {
		t.Cleanup(func() {
			if err := f(); err != nil {
				t.Errorf("cleanup %s: %v", what, err)
			}
		})
	}
	ignoreMissing := func(err error) error {
		if err != nil && strings.Contains(err.Error(), "does not exist") {
			return nil
		}
		return err
	}
	handle := srv.ResourceGovernorRef()

	// Cleanups run last-registered first, so this reads bottom-up: classifier
	// off and applied, group, pools, stored I/O back to DEFAULT, RECONFIGURE,
	// DISABLE, and the function last — it cannot be dropped while in force.
	// The second RECONFIGURE takes the drops out of force: DISABLE alone
	// clears the pending flag without applying, so the dropped pool and group
	// stayed in sys.dm_resource_governor_* on every instance until the next
	// enable (found 2026-10-01).
	clf := "[dbo].[" + liveRGWClassifier + "]"
	cleanup("drop classifier function", func() error {
		_, err := db.ExecContext(bg, "EXEC master.sys.sp_executesql N'DROP FUNCTION IF EXISTS "+clf+"'")
		return err
	})
	cleanup("nothing left in force", func() error {
		var n int
		if err := db.QueryRowContext(bg, `SELECT (SELECT COUNT(*) FROM sys.dm_resource_governor_resource_pools WHERE name = @p1)
			+ (SELECT COUNT(*) FROM sys.dm_resource_governor_workload_groups WHERE name = @p2)`, liveRGWPool, liveRGWGroup).Scan(&n); err != nil {
			return err
		}
		if n != 0 {
			return fmt.Errorf("%d dropped pool/group rows still in force", n)
		}
		return nil
	})
	cleanup("disable", func() error { return handle.Disable(bg) })
	cleanup("apply the drops", func() error { return handle.Reconfigure(bg) })
	cleanup("max outstanding I/O", func() error { return handle.SetMaxOutstandingIOPerVolume(bg, 0) })
	cleanup("drop external pool", func() error { return ignoreMissing(srv.ExternalResourcePoolRef(liveRGWExtPool).Drop(bg)) })
	cleanup("drop pool", func() error { return ignoreMissing(srv.ResourcePoolRef(liveRGWPool).Drop(bg)) })
	cleanup("drop group", func() error { return ignoreMissing(srv.WorkloadGroupRef(liveRGWGroup).Drop(bg)) })
	cleanup("apply classifier removal", func() error { return handle.Reconfigure(bg) })
	cleanup("remove classifier", func() error { return handle.SetClassifier(bg, "", "") })

	// The classifier routes nothing anywhere: every session goes to default.
	if _, err := db.ExecContext(ctx, "EXEC master.sys.sp_executesql N'CREATE FUNCTION "+clf+
		"() RETURNS sysname WITH SCHEMABINDING AS BEGIN RETURN N''default'' END'"); err != nil {
		t.Fatalf("create classifier function: %v", err)
	}
	cands, err := srv.ClassifierFunctionCandidates(ctx)
	if err != nil {
		t.Fatalf("ClassifierFunctionCandidates: %v", err)
	}
	if !slices.Contains(cands, ClassifierFunction{"dbo", liveRGWClassifier}) {
		t.Errorf("ClassifierFunctionCandidates = %v, missing dbo.%s", cands, liveRGWClassifier)
	}

	// Pools and group.
	pool, err := srv.CreateResourcePool(ctx, CreateResourcePoolRequest{Name: liveRGWPool, Options: ResourcePoolOptions{
		MinCPUPercent: Ptr(5), MaxCPUPercent: Ptr(60), MaxMemoryPercent: Ptr(50), MaxIOPSPerVolume: Ptr(500),
	}})
	if err != nil {
		t.Fatalf("CreateResourcePool: %v", err)
	}
	if pool.ID == 0 || pool.MinCPUPercent != 5 || pool.MaxCPUPercent != 60 || pool.MaxMemoryPercent != 50 ||
		pool.MaxIOPSPerVolume != 500 || pool.CapCPUPercent != 100 {
		t.Errorf("created pool read back as %+v", *pool)
	}
	if err := pool.Alter(ctx, ResourcePoolOptions{CapCPUPercent: Ptr(70), MinIOPSPerVolume: Ptr(10)}); err != nil {
		t.Fatalf("ResourcePool.Alter: %v", err)
	}
	if back, err := srv.ResourcePoolByName(ctx, liveRGWPool); err != nil || !reflect.DeepEqual(back, pool) {
		t.Errorf("after Alter the catalog has %+v (%v), the handle mirrors %+v", back, err, pool)
	}

	ext, extErr := srv.CreateExternalResourcePool(ctx, CreateExternalResourcePoolRequest{Name: liveRGWExtPool,
		Options: ExternalResourcePoolOptions{MaxCPUPercent: Ptr(40), MaxProcesses: Ptr(5)}})
	if extErr != nil {
		t.Logf("external pool not created, its writes are not checked: %v", extErr)
	} else {
		if err := ext.Alter(ctx, ExternalResourcePoolOptions{MaxMemoryPercent: Ptr(30)}); err != nil {
			t.Fatalf("ExternalResourcePool.Alter: %v", err)
		}
		back, err := srv.ExternalResourcePoolByName(ctx, liveRGWExtPool)
		if err != nil || back.MaxCPUPercent != 40 || back.MaxMemoryPercent != 30 || back.MaxProcesses != 5 {
			t.Errorf("external pool read back as %+v (%v)", back, err)
		}
	}

	grant, fractional := 30.0, major == 0 || major >= int(SQLServer2019)
	if fractional {
		grant = 12.5
	}
	opts := WorkloadGroupOptions{
		Importance: Ptr(ImportanceHigh), RequestMaxMemoryGrantPercent: Ptr(grant),
		RequestMaxCPUTimeSec: Ptr(30), MaxDOP: Ptr(2), Pool: Ptr(liveRGWPool),
	}
	tempdb := major == 0 || major >= int(SQLServer2025)
	if tempdb {
		opts.GroupMaxTempdbDataMB = Ptr(256.0)
	}
	if extErr == nil {
		opts.ExternalPool = Ptr(liveRGWExtPool)
	}
	grp, err := srv.CreateWorkloadGroup(ctx, CreateWorkloadGroupRequest{Name: liveRGWGroup, Options: opts})
	if err != nil {
		t.Fatalf("CreateWorkloadGroup: %v", err)
	}
	if grp.PoolName != liveRGWPool || grp.Importance != "High" || grp.RequestMaxMemoryGrantPercent != grant ||
		grp.RequestMaxCPUTimeSec != 30 || grp.MaxDOP != 2 {
		t.Errorf("created group read back as %+v", *grp)
	}
	if tempdb && (grp.GroupMaxTempdbDataMB == nil || *grp.GroupMaxTempdbDataMB != 256) {
		t.Errorf("tempdb limit read back as %v", grp.GroupMaxTempdbDataMB)
	}
	alt := WorkloadGroupOptions{Importance: Ptr(WorkloadImportance("low")), GroupMaxRequests: Ptr(7)}
	if tempdb {
		alt.ClearGroupMaxTempdbDataMB = true
		alt.GroupMaxTempdbDataPercent = Ptr(10.0)
	}
	if err := grp.Alter(ctx, alt); err != nil {
		t.Fatalf("WorkloadGroup.Alter: %v", err)
	}
	if back, err := srv.WorkloadGroupByName(ctx, liveRGWGroup); err != nil || !reflect.DeepEqual(back, grp) {
		t.Errorf("after Alter the catalog has %+v (%v), the handle mirrors %+v", back, err, grp)
	}
	if !fractional {
		_, err := srv.CreateWorkloadGroup(ctx, CreateWorkloadGroupRequest{Name: "gosmo_live_rgw_never",
			Options: WorkloadGroupOptions{RequestMaxMemoryGrantPercent: Ptr(12.5)}})
		if !errors.Is(err, ErrUnsupportedVersion) {
			t.Errorf("fractional grant on major %d = %v, want ErrUnsupportedVersion", major, err)
		}
	}

	// What the server refuses comes through as its own message.
	for what, err := range map[string]error{
		"drop pool with a group": srv.ResourcePoolRef(liveRGWPool).Drop(ctx),
		"alter internal pool":    srv.ResourcePoolRef("internal").Alter(ctx, ResourcePoolOptions{MaxCPUPercent: Ptr(90)}),
		"alter internal group":   srv.WorkloadGroupRef("internal").Alter(ctx, WorkloadGroupOptions{Importance: Ptr(ImportanceLow)}),
		"drop default pool":      srv.ResourcePoolRef("default").Drop(ctx),
		"drop default group":     srv.WorkloadGroupRef("default").Drop(ctx),
	} {
		if err == nil {
			t.Errorf("%s: the server accepted it", what)
		}
	}
	if extErr == nil {
		err := srv.ExternalResourcePoolRef(liveRGWExtPool).Drop(ctx)
		t.Logf("drop external pool still used by a group: %v", err)
		if err == nil {
			t.Error("dropping an external pool a group uses was accepted")
		}
	}

	// The governor itself.
	if err := handle.SetClassifier(ctx, "dbo", liveRGWClassifier); err != nil {
		t.Fatalf("SetClassifier: %v", err)
	}
	if err := handle.SetMaxOutstandingIOPerVolume(ctx, 60); err != nil {
		t.Fatalf("SetMaxOutstandingIOPerVolume: %v", err)
	}
	// DROP FUNCTION takes no database prefix (Msg 166), so it runs in master.
	if _, err := db.ExecContext(ctx, "EXEC master.sys.sp_executesql N'DROP FUNCTION "+clf+"'"); err == nil ||
		!strings.Contains(err.Error(), "classifier") {
		t.Errorf("dropping the stored classifier function = %v, want Msg 10920", err)
	}
	stored, err := srv.ResourceGovernor(ctx)
	if err != nil {
		t.Fatalf("ResourceGovernor: %v", err)
	}
	if stored.IsEnabled || stored.ClassifierName != liveRGWClassifier || stored.ClassifierSchema != "dbo" ||
		stored.MaxOutstandingIOPerVolume != 60 {
		t.Errorf("stored configuration = %+v, want the classifier and 60, still disabled", *stored)
	}
	if st, err := srv.ResourceGovernorStatus(ctx); err != nil || !st.IsReconfigurationPending {
		t.Errorf("status before Reconfigure = %+v, %v; want pending", st, err)
	}
	if err := handle.Reconfigure(ctx); err != nil {
		t.Fatalf("Reconfigure: %v", err)
	}
	stored, _ = srv.ResourceGovernor(ctx)
	st, err = srv.ResourceGovernorStatus(ctx)
	if err != nil || st.IsReconfigurationPending || st.ClassifierName != liveRGWClassifier || !stored.IsEnabled {
		t.Errorf("after Reconfigure: stored %+v, status %+v, %v — want enabled, applied, classifier in force", stored, st, err)
	}
	stats, err := srv.ResourcePoolStats(ctx)
	if err != nil {
		t.Fatalf("ResourcePoolStats: %v", err)
	}
	if !slices.ContainsFunc(stats, func(s *ResourcePoolStats) bool { return s.Name == liveRGWPool }) {
		t.Errorf("%s has no runtime row after Reconfigure", liveRGWPool)
	}
	if err := handle.ResetStatistics(ctx); err != nil {
		t.Errorf("ResetStatistics: %v", err)
	}

	// Script, and run the script: the configuration's, then each object's
	// DROP AND CREATE, which must put back exactly what was read.
	sc := NewServerScripter(srv, ScriptOptions{Verb: ScriptDropAndCreate, IncludeIfNotExists: true})
	runScript := func(what, script string) {
		t.Helper()
		for batch := range strings.SplitSeq(script, "\nGO\n") {
			if strings.TrimSpace(batch) == "" {
				continue
			}
			if _, err := db.ExecContext(ctx, batch); err != nil {
				t.Fatalf("%s script batch %q: %v\nscript:\n%s", what, batch, err, script)
			}
		}
	}
	rgScript, err := NewServerScripter(srv, ScriptOptions{}).ScriptResourceGovernor(ctx)
	if err != nil {
		t.Fatalf("ScriptResourceGovernor: %v", err)
	}
	if !strings.Contains(rgScript, liveRGWClassifier) || !strings.Contains(rgScript, "= 60") ||
		!strings.Contains(rgScript, "RECONFIGURE") {
		t.Errorf("ScriptResourceGovernor:\n%s", rgScript)
	}
	runScript("resource governor", rgScript)

	groupBefore, _ := srv.WorkloadGroupByName(ctx, liveRGWGroup)
	groupScript, err := sc.ScriptWorkloadGroup(ctx, liveRGWGroup)
	if err != nil {
		t.Fatalf("ScriptWorkloadGroup: %v", err)
	}
	// The pool can only be dropped and recreated with no group in it.
	if err := grp.Drop(ctx); err != nil {
		t.Fatalf("WorkloadGroup.Drop: %v", err)
	}
	if _, err := db.ExecContext(ctx, "ALTER RESOURCE POOL ["+liveRGWPool+"] WITH (AFFINITY SCHEDULER = (0))"); err != nil {
		t.Fatalf("set pool affinity: %v", err)
	}
	poolBefore, _ := srv.ResourcePoolByName(ctx, liveRGWPool)
	poolScript, err := sc.ScriptResourcePool(ctx, liveRGWPool)
	if err != nil {
		t.Fatalf("ScriptResourcePool: %v", err)
	}
	runScript("pool", poolScript)
	if after, err := srv.ResourcePoolByName(ctx, liveRGWPool); err != nil || !sameExceptID(after, poolBefore) {
		t.Errorf("pool recreated from its script as %+v (%v), was %+v\nscript:\n%s", after, err, poolBefore, poolScript)
	}
	runScript("group", groupScript)
	if after, err := srv.WorkloadGroupByName(ctx, liveRGWGroup); err != nil ||
		!sameExceptID(after, groupBefore) {
		t.Errorf("group recreated from its script as %+v (%v), was %+v\nscript:\n%s", after, err, groupBefore, groupScript)
	}
	if extErr == nil {
		extBefore, _ := srv.ExternalResourcePoolByName(ctx, liveRGWExtPool)
		extScript, err := NewServerScripter(srv, ScriptOptions{}).ScriptExternalResourcePool(ctx, liveRGWExtPool)
		if err != nil {
			t.Fatalf("ScriptExternalResourcePool: %v", err)
		}
		if !strings.Contains(extScript, "MAX_CPU_PERCENT = 40, MAX_MEMORY_PERCENT = 30, MAX_PROCESSES = 5") {
			t.Errorf("ScriptExternalResourcePool = %q for %+v", extScript, extBefore)
		}
	}
	for _, name := range []string{"internal", "default"} {
		if s, err := sc.ScriptResourcePool(ctx, name); !errors.Is(err, ErrUnsupported) {
			t.Errorf("DROP AND CREATE of built-in pool %s = %q, %v; want ErrUnsupported", name, s, err)
		}
	}
}

// sameExceptID compares two catalog reads of an object that was dropped and
// recreated in between, which gives it new ids.
func sameExceptID[T ResourcePool | WorkloadGroup](a, b *T) bool {
	if a == nil || b == nil {
		return false
	}
	x, y := *a, *b
	switch x := any(&x).(type) {
	case *ResourcePool:
		x.ID = 0
		any(&y).(*ResourcePool).ID = 0
	case *WorkloadGroup:
		x.ID, x.PoolID, x.ExternalPoolID = 0, 0, 0
		yy := any(&y).(*WorkloadGroup)
		yy.ID, yy.PoolID, yy.ExternalPoolID = 0, 0, 0
	}
	return reflect.DeepEqual(x, y)
}
