//go:build livedb

// Live pool affinity writes: AFFINITY SCHEDULER / CPU / NUMANODE / AUTO on a
// disposable pool and external pool, read back as the catalog's masks, with
// the ids taken from Server.Schedulers.
//
//	go test -tags livedb . -run TestLiveResourcePoolAffinity -v \
//	  -livedb 'sqlserver://sa:PASS@host?TrustServerCertificate=true'
//
// Same discipline as live_resource_governor_test.go: runs only where the
// governor is disabled with nothing pending, and DISABLE at the end clears
// the pending flag the DDL set without enabling anything.
package gosmo

import (
	"context"
	"strings"
	"testing"
)

const (
	liveAffPool    = "gosmo_live_aff_pool"
	liveAffExtPool = "gosmo_live_aff_ext"
)

func TestLiveResourcePoolAffinity(t *testing.T) {
	db, ctx, done := liveDB(t)
	t.Cleanup(done)

	srv, err := NewServer(ctx, db)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
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

	scheds, err := srv.Schedulers(ctx)
	if err != nil {
		t.Fatalf("Schedulers: %v", err)
	}
	if len(scheds) == 0 {
		t.Fatal("Schedulers is empty")
	}
	for i, s := range scheds {
		if s.ID >= 1048576 {
			t.Errorf("Schedulers lists hidden or DAC scheduler %d", s.ID)
		}
		if i > 0 && s.ID <= scheds[i-1].ID {
			t.Errorf("Schedulers not in id order at %d", i)
		}
	}
	first, last := scheds[0], scheds[len(scheds)-1]
	if first.ProcessorGroup != 0 || last.ProcessorGroup != 0 {
		t.Skipf("more than one processor group; the mask checks below assume group 0")
	}
	nodeMask := int64(0)
	for _, s := range scheds {
		if s.NUMANode == first.NUMANode {
			nodeMask |= 1 << s.ID
		}
	}

	bg := context.Background()
	cleanup := func(what string, f func() error) {
		t.Cleanup(func() {
			if err := f(); err != nil && !strings.Contains(err.Error(), "does not exist") {
				t.Errorf("cleanup %s: %v", what, err)
			}
		})
	}
	cleanup("disable", func() error { return srv.ResourceGovernorRef().Disable(bg) })
	cleanup("drop external pool", func() error { return srv.ExternalResourcePoolRef(liveAffExtPool).Drop(bg) })
	cleanup("drop pool", func() error { return srv.ResourcePoolRef(liveAffPool).Drop(bg) })

	mask := func(p *ResourcePool) int64 {
		t.Helper()
		back, err := srv.ResourcePoolByName(ctx, p.Name)
		if err != nil {
			t.Fatalf("ResourcePoolByName: %v", err)
		}
		var m int64
		for _, a := range back.Affinity {
			if a.ProcessorGroup != 0 {
				t.Errorf("affinity in processor group %d", a.ProcessorGroup)
			}
			m |= a.SchedulerMask
		}
		return m
	}

	pool, err := srv.CreateResourcePool(ctx, CreateResourcePoolRequest{Name: liveAffPool, Options: ResourcePoolOptions{
		MaxCPUPercent: Ptr(50), Affinity: &PoolAffinity{Schedulers: []int{first.ID, last.ID}}}})
	if err != nil {
		t.Fatalf("CreateResourcePool with affinity: %v", err)
	}
	if got, want := mask(pool), int64(1)<<first.ID|int64(1)<<last.ID; got != want {
		t.Errorf("created with schedulers %d, %d: mask %#x, want %#x", first.ID, last.ID, got, want)
	}
	if err := pool.Alter(ctx, ResourcePoolOptions{Affinity: &PoolAffinity{NUMANodes: []int{first.NUMANode}}}); err != nil {
		t.Fatalf("Alter to NUMANODE: %v", err)
	}
	if got := mask(pool); got != nodeMask {
		t.Errorf("NUMANODE %d stored as mask %#x, want its schedulers %#x", first.NUMANode, got, nodeMask)
	}
	if err := pool.Alter(ctx, ResourcePoolOptions{Affinity: &PoolAffinity{}}); err != nil {
		t.Fatalf("Alter to AUTO: %v", err)
	}
	if got := mask(pool); got != 0 {
		t.Errorf("AUTO left mask %#x", got)
	}
	// An id past the last scheduler is the server's to refuse (Msg 10926).
	if err := pool.Alter(ctx, ResourcePoolOptions{Affinity: &PoolAffinity{Schedulers: []int{last.ID + 1}}}); err == nil {
		t.Errorf("scheduler %d, past the last, was accepted", last.ID+1)
	}

	ext, err := srv.CreateExternalResourcePool(ctx, CreateExternalResourcePoolRequest{Name: liveAffExtPool,
		Options: ExternalResourcePoolOptions{Affinity: &PoolAffinity{Schedulers: []int{first.CPUID}}}})
	if err != nil {
		t.Logf("external pool not created, its affinity is not checked: %v", err)
		return
	}
	if len(ext.Affinity) != 1 || ext.Affinity[0].CPUMask != 1<<first.CPUID {
		t.Errorf("external pool with CPU %d read back as %+v", first.CPUID, ext.Affinity)
	}
	if err := ext.Alter(ctx, ExternalResourcePoolOptions{Affinity: &PoolAffinity{}}); err != nil {
		t.Fatalf("external Alter to AUTO: %v", err)
	}
	if back, err := srv.ExternalResourcePoolByName(ctx, liveAffExtPool); err != nil || len(back.Affinity) != 0 {
		t.Errorf("external AUTO read back as %+v (%v)", back, err)
	}
}
