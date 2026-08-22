//go:build livedb

// Live verification of ReadLogFiltered. The unit test pins the EXEC and its
// arguments; only the server can say whether xp_readerrorlog accepts them as
// parameters at all — it is an extended stored procedure, and the sp_executesql
// wrapper the driver puts around a parameterised EXEC is not obviously
// something it tolerates. It does, and this is what says so.
//
//	go test -tags livedb . -run TestLiveReadLogFiltered -v \
//	  -livedb 'sqlserver://sa:PASS@host?TrustServerCertificate=true'
//
// Read-only: writes nothing to the instance.
package gosmo

import (
	"strings"
	"testing"
	"time"
)

func TestLiveReadLogFiltered(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()
	s := &Server{db: db}

	all, err := s.ReadLogContext(ctx, ErrorLogSQLServer, 0)
	if err != nil {
		t.Fatalf("ReadLogContext: %v", err)
	}
	if len(all) == 0 {
		t.Skip("the current error log is empty")
	}

	// A word from the middle of some entry, so the filtered read has
	// something to find and the unfiltered one has more than it.
	needle := "Server"
	var wantCount int
	for _, e := range all {
		if strings.Contains(strings.ToLower(e.Text), strings.ToLower(needle)) {
			wantCount++
		}
	}
	if wantCount == 0 || wantCount == len(all) {
		t.Skipf("%q matches %d of %d entries — no narrowing to observe", needle, wantCount, len(all))
	}

	got, err := s.ReadLogFilteredContext(ctx, ErrorLogSQLServer, 0, LogSearch{Text1: needle})
	if err != nil {
		t.Fatalf("ReadLogFilteredContext (text): %v", err)
	}
	if len(got) != wantCount {
		t.Errorf("filtered read returned %d entries, want the %d that match %q locally",
			len(got), wantCount, needle)
	}
	for _, e := range got {
		if !strings.Contains(strings.ToLower(e.Text), strings.ToLower(needle)) {
			t.Errorf("filtered read returned a non-matching entry: %q", e.Text)
			break
		}
	}

	// A date range on its own — the case where both text arguments have to
	// reach the server as typed NULLs rather than being dropped.
	newest := all[0].Date
	for _, e := range all {
		if e.Date.After(newest) {
			newest = e.Date
		}
	}
	from := newest.Add(-time.Hour)
	byDate, err := s.ReadLogFilteredContext(ctx, ErrorLogSQLServer, 0, LogSearch{From: from})
	if err != nil {
		t.Fatalf("ReadLogFilteredContext (dates): %v", err)
	}
	for _, e := range byDate {
		if e.Date.Before(from) {
			t.Errorf("entry at %s is before the requested start %s", e.Date, from)
			break
		}
	}
	if len(byDate) > len(all) {
		t.Errorf("a filtered read returned more entries (%d) than the whole file (%d)", len(byDate), len(all))
	}

	// Both strings together are AND, not OR: a second string that cannot
	// co-occur must narrow to nothing rather than widening the result.
	andRead, err := s.ReadLogFilteredContext(ctx, ErrorLogSQLServer, 0,
		LogSearch{Text1: needle, Text2: "zzz_no_such_text_zzz"})
	if err != nil {
		t.Fatalf("ReadLogFilteredContext (two strings): %v", err)
	}
	if len(andRead) != 0 {
		t.Errorf("two search strings returned %d entries; they are AND-ed, so an impossible pair matches nothing", len(andRead))
	}
}
