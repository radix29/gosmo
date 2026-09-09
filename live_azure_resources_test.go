//go:build livedb

// Live coverage for the three Azure-only instance reads:
// ServerResourceStats, InstanceResourceGovernance and OSJobObject.
//
// No unit test can reach these. All three read views that exist only on an
// Azure engine edition, every column of every one is nullable, and the shapes
// are Microsoft's — including a column the view itself spells
// `volume_local_max_oustanding_io`, missing a `t`. A query that names a column
// wrong fails the whole read, and `go test ./...` says nothing.
//
//	go test -tags livedb . -run TestLiveAzure -v \
//	  -livedb 'sqlserver://testgo:PASS@t-qmi-01…:3342?TrustServerCertificate=true'
//
// Read-only: nothing is created and nothing is dropped. On a non-Azure DSN
// each test asserts the refusal instead, which is the other half of the
// contract.
package gosmo

import (
	"errors"
	"flag"
	"testing"
)

// liveAzureDB names the database the per-database reads run against. It has to
// be one something has touched: sys.dm_db_resource_stats has no rows for a
// database idle since the instance last restarted.
var liveAzureDB = flag.String("liveazuredb", "GoTest01", "database for the per-database Azure resource reads")

// liveAzureServer opens the -livedb DSN and reports whether it is an Azure
// edition, so each test can assert the read or the refusal.
func liveAzureServer(t *testing.T) (*Server, bool) {
	t.Helper()
	db, ctx, done := liveDB(t)
	t.Cleanup(done)
	srv, err := NewServer(ctx, db)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	return srv, srv.Info().IsAzure()
}

func TestLiveAzureServerResourceStats(t *testing.T) {
	srv, azure := liveAzureServer(t)
	ctx := t.Context()

	stats, err := srv.ServerResourceStatsContext(ctx, 20)
	if !azure {
		if !errors.Is(err, ErrUnsupportedVersion) {
			t.Fatalf("on a non-Azure instance: err = %v, want ErrUnsupportedVersion", err)
		}
		return
	}
	if err != nil {
		t.Fatalf("ServerResourceStatsContext: %v", err)
	}
	if len(stats) == 0 {
		t.Fatal("no rows — sys.server_resource_stats retains ~14 days, so an instance up for one window has some")
	}
	// Oldest first is what a chart plots against, and the query's inner TOP
	// orders the other way — so this is the assertion that catches the
	// wrapper being dropped.
	for i := 1; i < len(stats); i++ {
		if stats[i].EndTime.Before(stats[i-1].EndTime) {
			t.Fatalf("row %d ends before row %d — the history is not oldest-first", i, i-1)
		}
	}
	newest := stats[len(stats)-1]
	if newest.SKU == "" || newest.HardwareGeneration == "" || newest.VirtualCoreCount <= 0 {
		t.Errorf("the instance's shape is not populated: sku=%q hw=%q cores=%d",
			newest.SKU, newest.HardwareGeneration, newest.VirtualCoreCount)
	}
	// The pair item 5 of the MI plan replaced dm_os_volume_stats with: a
	// reserved figure the used figure fits inside.
	if newest.ReservedStorageMB <= 0 {
		t.Errorf("ReservedStorageMB = %d, want the instance's storage quota", newest.ReservedStorageMB)
	}
	if newest.StorageSpaceUsedMB > float64(newest.ReservedStorageMB) {
		t.Errorf("used %.0f MB of a %d MB quota — the two columns are the wrong way round",
			newest.StorageSpaceUsedMB, newest.ReservedStorageMB)
	}

	latest, err := srv.LatestServerResourceStatsContext(ctx)
	if err != nil {
		t.Fatalf("LatestServerResourceStatsContext: %v", err)
	}
	if !latest.EndTime.Equal(newest.EndTime) {
		t.Errorf("Latest ends at %v, the history's newest at %v", latest.EndTime, newest.EndTime)
	}
}

func TestLiveAzureInstanceResourceGovernance(t *testing.T) {
	srv, azure := liveAzureServer(t)

	g, err := srv.InstanceResourceGovernanceContext(t.Context())
	if !azure {
		if !errors.Is(err, ErrUnsupportedVersion) {
			t.Fatalf("on a non-Azure instance: err = %v, want ErrUnsupportedVersion", err)
		}
		return
	}
	if err != nil {
		t.Fatalf("InstanceResourceGovernanceContext: %v", err)
	}
	// The four the Instance tab reads as ceilings. Each is a limit the
	// governor always has a value for, so a zero here means the column name
	// is wrong rather than that the instance is unlimited.
	if g.CapCPU <= 0 {
		t.Errorf("CapCPU = %d, want the instance's CPU cap", g.CapCPU)
	}
	if g.MaxWorkerThreads <= 0 {
		t.Errorf("MaxWorkerThreads = %d", g.MaxWorkerThreads)
	}
	if g.MaxLogRate <= 0 {
		t.Errorf("MaxLogRate = %d bytes/sec", g.MaxLogRate)
	}
	if g.LocalIOPS <= 0 {
		t.Errorf("LocalIOPS = %d", g.LocalIOPS)
	}
	// The misspelled column. Named separately because it is the one a
	// plausible correction would silently break.
	if g.LocalMaxOutstandingIO <= 0 {
		t.Errorf("LocalMaxOutstandingIO = %d — volume_local_max_oustanding_io is spelled without the first t", g.LocalMaxOutstandingIO)
	}
	if g.DataDirectoryQuotaMB <= 0 || g.DataDirectoryUsageMB < 0 {
		t.Errorf("data directory: %d MB used of %d MB", g.DataDirectoryUsageMB, g.DataDirectoryQuotaMB)
	}
}

func TestLiveAzureOSJobObject(t *testing.T) {
	srv, azure := liveAzureServer(t)

	j, err := srv.OSJobObjectContext(t.Context())
	if !azure {
		if !errors.Is(err, ErrUnsupportedVersion) {
			t.Fatalf("on a non-Azure instance: err = %v, want ErrUnsupportedVersion", err)
		}
		return
	}
	if err != nil {
		t.Fatalf("OSJobObjectContext: %v", err)
	}
	if j.CPURate <= 0 {
		t.Errorf("CPURate = %d, want the job object's CPU allocation", j.CPURate)
	}
	if j.MemoryLimitMB <= 0 || j.ProcessMemoryLimitMB <= 0 {
		t.Errorf("memory limits: job %d MB, process %d MB", j.MemoryLimitMB, j.ProcessMemoryLimitMB)
	}
	if j.PeakJobMemoryUsedMB > j.MemoryLimitMB {
		t.Errorf("peak job memory %d MB exceeds the %d MB limit", j.PeakJobMemoryUsedMB, j.MemoryLimitMB)
	}
	// WorkingSetLimitMB is deliberately not asserted: it is NULL on a live
	// General Purpose instance, which is what the sql.Null* destinations are
	// for.
	if j.TotalUserTime <= 0 || j.TotalKernelTime <= 0 {
		t.Errorf("cpu time: user %d, kernel %d (100-ns units)", j.TotalUserTime, j.TotalKernelTime)
	}
}

// liveAzureDatabase is liveAzureServer plus a database to read per-database
// views through. It picks the DSN's own database rather than creating one:
// sys.dm_db_resource_stats has no rows for a database nothing has touched.
func liveAzureDatabase(t *testing.T, name string) (*Database, bool) {
	t.Helper()
	srv, azure := liveAzureServer(t)
	d, err := srv.DatabaseByNameContext(t.Context(), name)
	if err != nil {
		t.Fatalf("DatabaseByNameContext %s: %v", name, err)
	}
	return d, azure
}

func TestLiveAzureDatabaseResourceStats(t *testing.T) {
	d, azure := liveAzureDatabase(t, *liveAzureDB)

	stats, err := d.ResourceStatsContext(t.Context(), 0)
	if !azure {
		if !errors.Is(err, ErrUnsupportedVersion) {
			t.Fatalf("on a non-Azure instance: err = %v, want ErrUnsupportedVersion", err)
		}
		return
	}
	if err != nil {
		t.Fatalf("ResourceStatsContext: %v", err)
	}
	if len(stats) == 0 {
		t.Skip("sys.dm_db_resource_stats is empty — the database has been idle since the instance restarted")
	}
	t.Logf("%d rows, %s to %s", len(stats), stats[0].EndTime, stats[len(stats)-1].EndTime)

	// Oldest first, the order a chart wants and the one the query's nested
	// TOP makes easy to get backwards.
	for i := 1; i < len(stats); i++ {
		if stats[i].EndTime.Before(stats[i-1].EndTime) {
			t.Fatalf("row %d ends %s, before row %d's %s — not oldest-first",
				i, stats[i].EndTime, i-1, stats[i-1].EndTime)
		}
	}
	last := stats[len(stats)-1]
	if last.EndTime.IsZero() {
		t.Error("EndTime is zero — end_time did not scan")
	}
	// AllocatedStorageMB is the one column that is always positive for a
	// database with any file at all, so a zero means the name is wrong rather
	// than that the database is idle. Every percentage legitimately reads 0.
	if last.AllocatedStorageMB <= 0 {
		t.Errorf("AllocatedStorageMB = %d", last.AllocatedStorageMB)
	}
	if last.UsedStorageMB < 0 || last.UsedStorageMB > last.AllocatedStorageMB {
		t.Errorf("UsedStorageMB = %d of %d allocated", last.UsedStorageMB, last.AllocatedStorageMB)
	}
	for _, p := range []struct {
		name string
		v    float64
	}{
		{"AvgCPUPercent", last.AvgCPUPercent},
		{"AvgDataIOPercent", last.AvgDataIOPercent},
		{"AvgLogWritePercent", last.AvgLogWritePercent},
		{"AvgMemoryUsagePercent", last.AvgMemoryUsagePercent},
		{"MaxWorkerPercent", last.MaxWorkerPercent},
		{"MaxSessionPercent", last.MaxSessionPercent},
		{"AvgInstanceCPUPercent", last.AvgInstanceCPUPercent},
		{"AvgInstanceMemoryPercent", last.AvgInstanceMemoryPercent},
	} {
		if p.v < 0 || p.v > 100 {
			t.Errorf("%s = %v, want a percentage", p.name, p.v)
		}
	}

	// The Latest variant must agree with the last row of the history.
	latest, err := d.LatestResourceStatsContext(t.Context())
	if err != nil {
		t.Fatalf("LatestResourceStatsContext: %v", err)
	}
	if latest.EndTime.Before(last.EndTime) {
		t.Errorf("latest ends %s, before the history's last row at %s", latest.EndTime, last.EndTime)
	}
}

// sharedSLO is the service-level objective the instance's internal databases
// run under, and the one where the size ceilings are genuinely zero.
const sharedSLO = "Shared"

func TestLiveAzureUserDBResourceGovernance(t *testing.T) {
	srv, azure := liveAzureServer(t)

	all, err := srv.UserDBResourceGovernanceContext(t.Context())
	if !azure {
		if !errors.Is(err, ErrUnsupportedVersion) {
			t.Fatalf("on a non-Azure instance: err = %v, want ErrUnsupportedVersion", err)
		}
		return
	}
	if err != nil {
		t.Fatalf("UserDBResourceGovernanceContext: %v", err)
	}
	if len(all) == 0 {
		t.Fatal("no rows — the instance governs at least its own system databases")
	}
	for i, g := range all {
		t.Logf("%s: slo=%s max_dop=%d max_sessions=%d max_size=%d MB",
			g.DatabaseName, g.SLOName, g.MaxDOP, g.MaxSessions, g.MaxDBMaxSizeMB)
		if g.DatabaseName == "" {
			t.Errorf("row %d has no database name", i)
		}
		if g.DatabaseID <= 0 {
			t.Errorf("%s: DatabaseID = %d", g.DatabaseName, g.DatabaseID)
		}
		// A GUID that did not scan is the empty string, which is what the
		// CAST in the query exists to prevent.
		if len(g.LogicalDatabaseGUID) != 36 {
			t.Errorf("%s: LogicalDatabaseGUID = %q, want a 36-character GUID", g.DatabaseName, g.LogicalDatabaseGUID)
		}
		if len(g.PhysicalDatabaseGUID) != 36 {
			t.Errorf("%s: PhysicalDatabaseGUID = %q", g.DatabaseName, g.PhysicalDatabaseGUID)
		}
		// The columns the governor always has a value for. A zero in any of
		// these means a column name is wrong, not that the limit is absent.
		if g.SLOName == "" {
			t.Errorf("%s: SLOName is empty", g.DatabaseName)
		}
		if g.MaxDOP <= 0 {
			t.Errorf("%s: MaxDOP = %d", g.DatabaseName, g.MaxDOP)
		}
		if g.MaxSessions <= 0 {
			t.Errorf("%s: MaxSessions = %d", g.DatabaseName, g.MaxSessions)
		}
		// The size ceilings are 0 on the "Shared" SLO — the internal
		// databases (model_msdb, model_replicatedmaster) the instance keeps
		// outside the governed pool. Confirmed in sqlcmd, not assumed.
		if g.SLOName != sharedSLO && g.MaxDBMaxSizeMB <= 0 {
			t.Errorf("%s: MaxDBMaxSizeMB = %d on SLO %s", g.DatabaseName, g.MaxDBMaxSizeMB, g.SLOName)
		}
		if g.InstanceMaxWorkerThreads <= 0 {
			t.Errorf("%s: InstanceMaxWorkerThreads = %d", g.DatabaseName, g.InstanceMaxWorkerThreads)
		}
		if g.LastUpdatedUTC.IsZero() {
			t.Errorf("%s: LastUpdatedUTC is zero", g.DatabaseName)
		}
	}

	// Ordered by database name, and the per-database read must find the same
	// row the listing carries.
	for i := 1; i < len(all); i++ {
		if all[i].DatabaseName < all[i-1].DatabaseName {
			t.Errorf("row %d (%s) sorts before row %d (%s)", i, all[i].DatabaseName, i-1, all[i-1].DatabaseName)
		}
	}

	d, _ := liveAzureDatabase(t, *liveAzureDB)
	one, err := d.ResourceGovernanceContext(t.Context())
	if err != nil {
		t.Fatalf("ResourceGovernanceContext: %v", err)
	}
	if one.DatabaseName != *liveAzureDB {
		t.Errorf("ResourceGovernance returned %q, want %q — the view ignores the connection's database, so the WHERE is the only thing selecting it",
			one.DatabaseName, *liveAzureDB)
	}
	var listed *UserDBResourceGovernance
	for _, g := range all {
		if g.DatabaseName == one.DatabaseName {
			listed = g
		}
	}
	if listed == nil {
		t.Fatalf("%s is missing from the instance-wide listing", one.DatabaseName)
	}
	if listed.SLOName != one.SLOName || listed.MaxDOP != one.MaxDOP || listed.MaxDBMaxSizeMB != one.MaxDBMaxSizeMB {
		t.Errorf("the per-database read disagrees with the listing: %+v vs %+v", one, listed)
	}
}
