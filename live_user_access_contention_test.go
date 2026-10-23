//go:build livedb

// Live verification of B28 (gossms): SetUserAccess with other sessions
// switching into the database every 100 ms, as Object Explorer's reads do.
// Before the retry and DEADLOCK_PRIORITY HIGH nearly every ALTER failed here,
// SINGLE_USER and MULTI_USER alike (Msg 1205, at HIGH alone 924 and 5061).
//
//	go test -tags livedb . -run TestLiveSetUserAccessUnderContention -v \
//	  -livedb 'sqlserver://sa:PASS@host?TrustServerCertificate=true'
//
// Creates and drops its own throwaway database; touches nothing else.
package gosmo

import (
	"database/sql"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestLiveSetUserAccessUnderContention(t *testing.T) {
	db, ctx, done := liveDBTimeout(t, 3*time.Minute)
	defer done()

	const name = "gosmo_user_access_contention"
	d, drop := liveScratchDB(t, db, ctx, name)
	defer drop()

	readers, err := sql.Open("sqlserver", *liveDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer readers.Close()
	var stop atomic.Bool
	var wg sync.WaitGroup
	defer func() { stop.Store(true); wg.Wait() }()
	for range 4 {
		conn, err := readers.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		wg.Go(func() {
			defer conn.Close()
			for !stop.Load() {
				time.Sleep(100 * time.Millisecond)
				// Refused while another session holds the single-user slot;
				// only the attempt matters.
				conn.ExecContext(ctx, "USE ["+name+"]; SELECT COUNT(*) FROM sys.objects; USE master;")
			}
		})
	}

	for i := range 15 {
		if err := d.SetUserAccess(ctx, UserAccessSingle); err != nil {
			t.Fatalf("cycle %d: SetUserAccess SINGLE_USER: %v", i, err)
		}
		time.Sleep(100 * time.Millisecond)
		if err := d.SetUserAccess(ctx, UserAccessMulti); err != nil {
			t.Fatalf("cycle %d: SetUserAccess MULTI_USER: %v", i, err)
		}
	}
	var access string
	if err := db.QueryRowContext(ctx, "SELECT user_access_desc FROM sys.databases WHERE name = @p1",
		sql.Named("p1", name)).Scan(&access); err != nil {
		t.Fatal(err)
	}
	if access != "MULTI_USER" {
		t.Errorf("user_access_desc = %s, want MULTI_USER", access)
	}
}
