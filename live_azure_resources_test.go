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
	"testing"
)

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
