//go:build livedb

// Live verification of killDatabaseSessionsBatch's wait for a session killed
// because it holds a DATABASE lock in the target (open thread V4): the
// session's own context is master, so database_id alone misses it, and its
// rollback outlasts the KILL. The DROP right behind the batch must not fail
// Msg 3702.
//
//	go test -tags livedb . -run TestLiveKillDatabaseSessionsLockWait -v \
//	  -livedb 'sqlserver://sa:PASS@host?TrustServerCertificate=true'
//
// Creates and drops its own throwaway database; touches nothing else.
package gosmo

import (
	"context"
	"testing"
	"time"
)

func TestLiveKillDatabaseSessionsLockWait(t *testing.T) {
	db, ctx, done := liveDBTimeout(t, 3*time.Minute)
	defer done()
	const name = "gosmo_kill_lockwait"
	d, drop := liveScratchDB(t, db, ctx, name)
	defer drop()
	liveExecIn(t, d, ctx, "CREATE TABLE dbo.t (a int, b char(200))")

	holder, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("holder conn: %v", err)
	}
	defer holder.Close()
	// Current database stays master; the open transaction's cross-database
	// write holds a DATABASE lock in name and a rollback worth waiting for.
	for _, q := range []string{
		"USE master",
		"BEGIN TRAN",
		"INSERT [" + name + "].dbo.t SELECT TOP (1000000) 1, 'x' FROM sys.all_columns a CROSS JOIN sys.all_columns b",
	} {
		if _, err := holder.ExecContext(ctx, q); err != nil {
			t.Fatalf("holder %.40q: %v", q, err)
		}
	}
	var outside int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sys.dm_tran_locks AS l
JOIN sys.dm_exec_sessions AS s ON s.session_id = l.request_session_id
WHERE l.resource_type = N'DATABASE' AND l.resource_database_id = DB_ID(@p1)
  AND s.is_user_process = 1 AND s.database_id <> DB_ID(@p1)`, name).Scan(&outside); err != nil {
		t.Fatalf("inspect holder: %v", err)
	}
	if outside == 0 {
		t.Fatalf("precondition: no session outside %s holds its DATABASE lock", name)
	}

	dctx, cancel := context.WithTimeout(ctx, exclusiveDeadline*2)
	defer cancel()
	if _, err := db.ExecContext(dctx, killDatabaseSessionsBatch(name)+";\nDROP DATABASE ["+name+"];"); err != nil {
		t.Fatalf("kill batch + DROP: %v (Msg 3702 means the wait missed the lock holder)", err)
	}
	if databaseExists(t, db, ctx, name) {
		t.Fatal("database still exists after a DROP that reported success")
	}
}
