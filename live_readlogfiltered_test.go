//go:build livedb

// Live verification of ReadLog. The unit test pins the EXEC and its
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
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"
)

func TestLiveReadLogFiltered(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()
	s := &Server{db: db}

	all, err := s.ReadLog(ctx, ErrorLogSQLServer, 0, LogSearch{})
	if err != nil {
		t.Fatalf("ReadLog: %v", err)
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

	got, err := s.ReadLog(ctx, ErrorLogSQLServer, 0, LogSearch{Text1: needle})
	if err != nil {
		t.Fatalf("ReadLog (text): %v", err)
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
	byDate, err := s.ReadLog(ctx, ErrorLogSQLServer, 0, LogSearch{From: from})
	if err != nil {
		t.Fatalf("ReadLog (dates): %v", err)
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
	andRead, err := s.ReadLog(ctx, ErrorLogSQLServer, 0, LogSearch{Text1: needle, Text2: "zzz_no_such_text_zzz"})
	if err != nil {
		t.Fatalf("ReadLog (two strings): %v", err)
	}
	if len(andRead) != 0 {
		t.Errorf("two search strings returned %d entries; they are AND-ed, so an impossible pair matches nothing", len(andRead))
	}
}

// TestLiveReadLogWithoutSysadmin is the fallback: xp_readerrorlog is
// sysadmin's alone, so a login with VIEW SERVER STATE reads through
// sp_readerrorlog, the dates applied in Go, and gets what sysadmin's
// server-filtered read gets; a login with none of sp_readerrorlog's
// permissions is refused, PermissionDenied (Msg 27219).
func TestLiveReadLogWithoutSysadmin(t *testing.T) {
	db, ctx, done := liveDB(t)
	t.Cleanup(done)
	s := &Server{db: db}
	const login, pass = "gosmo_live_readlog", "Pw!gosmo_live_readlog1"
	c := context.Background()
	db.ExecContext(c, "IF SUSER_ID(N'"+login+"') IS NOT NULL DROP LOGIN ["+login+"]")
	if _, err := db.ExecContext(ctx, "CREATE LOGIN ["+login+"] WITH PASSWORD = N'"+pass+"', CHECK_POLICY = OFF"); err != nil {
		t.Fatalf("create login: %v", err)
	}
	t.Cleanup(func() { db.ExecContext(c, "DROP LOGIN ["+login+"]") })
	pool, err := sql.Open("sqlserver", liveRestrictedDSN(t, login, pass))
	if err != nil {
		t.Fatalf("open as %s: %v", login, err)
	}
	t.Cleanup(func() { pool.Close() })
	restricted := &Server{db: pool}

	_, err = restricted.ReadLog(ctx, ErrorLogSQLServer, 0, LogSearch{})
	if se, ok := AsSQLError(err); !IsPermissionDenied(err) || !ok || se.Number != 27219 {
		t.Fatalf("no permission: %v, want PermissionDenied Msg 27219", err)
	}

	if _, err := db.ExecContext(ctx, "GRANT VIEW SERVER STATE TO ["+login+"]"); err != nil {
		t.Fatalf("grant: %v", err)
	}
	all, err := s.ReadLog(ctx, ErrorLogSQLServer, 1, LogSearch{})
	if err != nil || len(all) < 3 {
		t.Skipf("archive 1: %d entries, %v", len(all), err)
	}
	// A range inside archive 1, which no longer grows: its middle third.
	search := LogSearch{From: all[len(all)/3].Date, To: all[2*len(all)/3].Date}
	want, err := s.ReadLog(ctx, ErrorLogSQLServer, 1, search)
	if err != nil {
		t.Fatalf("sysadmin read: %v", err)
	}
	got, err := restricted.ReadLog(ctx, ErrorLogSQLServer, 1, search)
	if err != nil {
		t.Fatalf("VIEW SERVER STATE read: %v", err)
	}
	if len(got) != len(want) || len(got) == 0 {
		t.Fatalf("VIEW SERVER STATE read %d entries, sysadmin %d", len(got), len(want))
	}
	for i := range got {
		if !got[i].Date.Equal(want[i].Date) || got[i].Text != want[i].Text || got[i].Process != want[i].Process {
			t.Fatalf("entry %d: %+v, sysadmin %+v", i, got[i], want[i])
		}
	}
}
