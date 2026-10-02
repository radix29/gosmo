//go:build livedb

// Live verification of the Server lifetime: Close stops a statement in flight
// on the server, not just in the client.
//
//	go test -tags livedb . -run TestLiveCloseEndsAStatementInFlight -v \
//	  -livedb 'sqlserver://sa:PASS@host?TrustServerCertificate=true'
//
// Creates nothing; the held statement is a WAITFOR through sp_executesql.
package gosmo

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"
)

// Without the lifetime, *sql.DB.Close waits for the statement — the whole
// WAITFOR — and the session holds on until it ends.
func TestLiveCloseEndsAStatementInFlight(t *testing.T) {
	watch, ctx, done := liveDB(t)
	defer done()

	pool, err := sql.Open("sqlserver", *liveDSN)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	srv := liveServer(t, pool, ctx)
	const marker = "WAITFOR DELAY '00:00:40' -- gosmo_w16_lifetime"

	ran := make(chan error, 1)
	go func() {
		_, err := srv.DatabaseRef("master").ExecProc(context.Background(), "sys", "sp_executesql",
			In("stmt", marker))
		ran <- err
	}()

	session := func() int {
		var id int
		err := watch.QueryRowContext(ctx, `
SELECT r.session_id FROM sys.dm_exec_requests r
CROSS APPLY sys.dm_exec_sql_text(r.sql_handle) t
WHERE t.text LIKE N'%gosmo_w16_lifetime%' AND r.session_id <> @@SPID`).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			return 0
		}
		if err != nil {
			t.Fatalf("watch: %v", err)
		}
		return id
	}
	deadline := time.Now().Add(10 * time.Second)
	for session() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the WAITFOR never showed up in sys.dm_exec_requests")
		}
		time.Sleep(100 * time.Millisecond)
	}

	closed := make(chan struct{})
	start := time.Now()
	go func() { srv.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(10 * time.Second):
		t.Fatal("Close waited for the statement")
	}
	select {
	case err := <-ran:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("the statement ended with %v, want context.Canceled", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the statement outlived Close")
	}
	for session() != 0 {
		if time.Since(start) > 10*time.Second {
			t.Fatal("the request is still on the server after Close")
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Logf("Close ended the statement in %v", time.Since(start))
}
