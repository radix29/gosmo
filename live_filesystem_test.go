//go:build livedb

// Live verification of EnumFileSystemContext's version gate.
//
// The gate decides between sys.dm_os_enumerate_filesystem (2017+, reports
// sizes and timestamps) and xp_dirtree (every version, names and the
// directory flag only). It is written positively — the DMV only on a known
// 2017-or-later instance, everything else including an *unknown* version on
// xp_dirtree — so that a pre-2017 instance whose ServerInfo never loaded
// degrades instead of failing outright against a DMV that isn't there.
//
// Only a server can say whether that degradation is real. The claim the flip
// rests on is that xp_dirtree returns the *same directory* as the DMV, just
// with less detail; if the two disagreed on which entries exist, the fallback
// would be a different answer rather than a lesser one, and the gate would be
// wrong. That is what TestLiveEnumFileSystemFallbackAgreesWithDMV checks.
//
//	go test -tags livedb . -run TestLiveEnumFileSystem -v \
//	  -livedb 'sqlserver://sa:PASS@host?TrustServerCertificate=true' \
//	  -livepath 'C:\Program Files\Microsoft SQL Server'
//
// Read-only: enumerates a directory and creates nothing.
package gosmo

import (
	"flag"
	"sort"
	"testing"
)

// livePath is the directory both branches are pointed at. It must exist on
// the server host and hold at least one entry.
var livePath = flag.String("livepath", `C:\Program Files\Microsoft SQL Server`,
	"directory on the live server host to enumerate")

// names returns the entry names, sorted, so two enumerations can be compared
// without depending on the order the server happened to return them in.
func names(entries []*FileSystemEntry) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Name)
	}
	sort.Strings(out)
	return out
}

// TestLiveEnumFileSystemKnownModernTakesTheDMV pins the branch the gate is
// meant to prefer: a server that reports 2017 or later must go to the DMV and
// come back with the detail only the DMV has.
func TestLiveEnumFileSystemKnownModernTakesTheDMV(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()

	srv := &Server{db: db}
	if err := srv.loadInfo(ctx); err != nil {
		t.Fatalf("load server info: %v", err)
	}
	info := srv.Info()
	if info == nil {
		t.Fatal("loadInfo reported success but left Info() nil")
	}
	if info.VersionMajor < 14 {
		t.Skipf("server is major %d, pre-2017; this test needs a DMV-capable instance", info.VersionMajor)
	}
	t.Logf("server major version %d", info.VersionMajor)

	entries, err := srv.EnumFileSystemContext(ctx, *livePath)
	if err != nil {
		t.Fatalf("enumerate %q on a major-%d server: %v", *livePath, info.VersionMajor, err)
	}
	if len(entries) == 0 {
		t.Fatalf("enumerate %q returned nothing; pick a -livepath that has entries", *livePath)
	}
	t.Logf("DMV branch returned %d entries", len(entries))

	// The DMV's whole advantage over xp_dirtree is Size and LastModified. If
	// neither is ever populated the gate is choosing a branch that buys
	// nothing, which is worth knowing.
	var sized, stamped int
	for _, e := range entries {
		if e.Size > 0 {
			sized++
		}
		if !e.LastModified.IsZero() {
			stamped++
		}
	}
	if stamped == 0 {
		t.Errorf("no entry under %q carried a LastModified; the DMV branch is "+
			"supposed to be the one that reports timestamps", *livePath)
	}
	t.Logf("DMV branch: %d/%d entries sized, %d/%d timestamped",
		sized, len(entries), stamped, len(entries))
}

// TestLiveEnumFileSystemFallbackAgreesWithDMV is the test the flip actually
// rests on. An unknown version (info nil) now takes xp_dirtree where it used
// to take the DMV, so xp_dirtree has to describe the same directory — same
// entries, same directory flags — and differ only in the detail it cannot
// report. A disagreement here would mean the fallback answers a different
// question, and the gate would have to go back.
func TestLiveEnumFileSystemFallbackAgreesWithDMV(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()

	// info nil is the unknown-version case the gate now routes to xp_dirtree.
	unknown := &Server{db: db}
	if unknown.info != nil {
		t.Fatal("fixture is wrong: this Server must have no ServerInfo loaded")
	}
	viaDirTree, err := unknown.EnumFileSystemContext(ctx, *livePath)
	if err != nil {
		t.Fatalf("unknown-version enumerate %q must degrade, not fail: %v", *livePath, err)
	}

	// The same server with a version known to be modern takes the DMV.
	known := &Server{db: db, info: &ServerInfo{VersionMajor: 17}}
	viaDMV, err := known.EnumFileSystemContext(ctx, *livePath)
	if err != nil {
		t.Fatalf("known-modern enumerate %q: %v", *livePath, err)
	}

	dirTreeNames, dmvNames := names(viaDirTree), names(viaDMV)
	if len(dirTreeNames) != len(dmvNames) {
		t.Fatalf("xp_dirtree saw %d entries under %q, the DMV saw %d — the fallback "+
			"must be the same directory with less detail, not a different listing\n"+
			"xp_dirtree: %v\nDMV:        %v",
			len(dirTreeNames), *livePath, len(dmvNames), dirTreeNames, dmvNames)
	}
	for i := range dmvNames {
		if dirTreeNames[i] != dmvNames[i] {
			t.Errorf("entry %d: xp_dirtree %q, DMV %q", i, dirTreeNames[i], dmvNames[i])
		}
	}

	// The directory flag is the one field Browse genuinely needs from the
	// fallback, so it has to survive the degradation.
	dmvDirs := map[string]bool{}
	for _, e := range viaDMV {
		dmvDirs[e.Name] = e.IsDirectory
	}
	for _, e := range viaDirTree {
		if want, ok := dmvDirs[e.Name]; ok && e.IsDirectory != want {
			t.Errorf("%q: xp_dirtree says IsDirectory=%v, the DMV says %v",
				e.Name, e.IsDirectory, want)
		}
	}
	t.Logf("both branches agree on %d entries under %q", len(dmvNames), *livePath)

	// And the degradation itself: xp_dirtree reports no sizes or timestamps.
	// Documented on EnumFileSystemContext as the cost of the fallback, so it
	// is pinned rather than left as a claim.
	for _, e := range viaDirTree {
		if e.Size != 0 || !e.LastModified.IsZero() {
			t.Errorf("%q: xp_dirtree returned Size=%d LastModified=%v; it reports "+
				"neither, and the doc comment says so", e.Name, e.Size, e.LastModified)
		}
	}
}
