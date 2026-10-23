//go:build livedb

// Live verification of Database.useBatch — the USE and the read sent as one
// batch, which a read inside InTransaction still is. What it rests on is
// server behaviour no fake can show: that a failed USE stops at the guard
// rather than running the read in the session's previous database, that the
// error it raises arrives before any row, and that statements after the USE
// bind in the database it switched to. A pooled read goes through procRead
// since 2026-10-23; the refusal cases check that both report the same USE
// failure (recheckEntry), and the binding and line-number cases now run on
// procRead.
//
//	go test -tags livedb . -run TestLiveUseBatch -v \
//	  -livedb 'sqlserver://sa:PASS@host?TrustServerCertificate=true'
//
// Creates and drops its own throwaway databases and login; touches nothing
// else. Skipped entirely without -livedb.
package gosmo

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	mssql "github.com/microsoft/go-mssqldb"
)

// liveReadErrors reads through d every way a Database reads — query and
// queryRow, on a pooled connection (procRead) and inside InTransaction
// (useBatch) — keyed by which. Each read in a transaction gets its own: a
// refusal ends the transaction it ran in.
func liveReadErrors(ctx context.Context, d *Database) map[string]error {
	reads := map[string]func(ctx context.Context) error{
		"query": func(ctx context.Context) error {
			rows, err := d.query(ctx, "SELECT DB_NAME()")
			if err == nil {
				rows.Close()
			}
			return err
		},
		"queryRow": func(ctx context.Context) error {
			var s string
			return d.queryRow(ctx, func(r *sql.Row) error { return r.Scan(&s) }, "SELECT DB_NAME()")
		},
	}
	out := map[string]error{}
	for name, read := range reads {
		out["pooled "+name] = read(ctx)
		_ = d.server.InTransaction(ctx, func(ctx context.Context) error {
			out["in a transaction "+name] = read(ctx)
			return nil
		})
	}
	return out
}

// assertUseError checks err is the USE failure every database-scoped read has
// always reported: gosmo's wrapping, and the server's error with its number
// underneath. Inside a transaction the refusal has already ended it, so
// recheckUse cannot run its USE and the server's error comes back unwrapped.
func assertUseError(t *testing.T, what string, err error, dbName string, number int32) {
	t.Helper()
	if err == nil {
		t.Errorf("%s: no error, want USE failure %d", what, number)
		return
	}
	e, ok := errors.AsType[mssql.Error](err)
	if !ok || e.Number != number {
		t.Errorf("%s: %v, want mssql error %d underneath", what, err, number)
	}
	if strings.HasPrefix(what, "in a transaction") {
		return
	}
	if want := "gosmo: USE " + dbName + ": "; len(err.Error()) < len(want) || err.Error()[:len(want)] != want {
		t.Errorf("%s: %q, want the %q prefix", what, err, want)
	}
}

func TestLiveUseBatch(t *testing.T) {
	pool, ctx, done := liveDBTimeout(t, 5*time.Minute)
	defer done()
	srv := liveServer(t, pool, ctx)

	t.Run("missing database", func(t *testing.T) {
		d := srv.DatabaseRef("zz_gosmo_usebatch_absent")
		for what, err := range liveReadErrors(ctx, d) {
			assertUseError(t, what, err, d.Name, 911)
		}
	})

	a, dropA := liveScratchDB(t, pool, ctx, "gosmo_usebatch_a")
	defer dropA()
	b, dropB := liveScratchDB(t, pool, ctx, "gosmo_usebatch_b")
	defer dropB()
	liveExecIn(t, a, ctx, "CREATE TABLE dbo.t (x INT)", "INSERT dbo.t VALUES (1)")
	liveExecIn(t, b, ctx, "CREATE TABLE dbo.t (y INT)", "INSERT dbo.t VALUES (2)")

	// The pool has one connection, left sitting in a, so the read into b
	// starts from a session where dbo.t has no column y: it binds only if
	// the statement after the USE is compiled in b.
	t.Run("binds in the database switched to", func(t *testing.T) {
		pool.SetMaxOpenConns(1)
		defer pool.SetMaxOpenConns(0)
		if _, err := pool.ExecContext(ctx, "USE [gosmo_usebatch_a]"); err != nil {
			t.Fatalf("USE a: %v", err)
		}
		for _, args := range [][]any{nil, {1}} {
			q := "SELECT y FROM dbo.t"
			if args != nil {
				q += " WHERE @p1 = 1"
			}
			var y int
			if err := b.queryRow(ctx, func(r *sql.Row) error { return r.Scan(&y) }, q, args...); err != nil || y != 2 {
				t.Errorf("args %v: y = %d, err %v; want b's row, 2", args, y, err)
			}
		}
	})

	// An error in the read keeps the line number the read alone reported:
	// the USE shares its first line.
	t.Run("read error keeps its line", func(t *testing.T) {
		var n int
		err := b.queryRow(ctx, func(r *sql.Row) error { return r.Scan(&n) }, "SELECT 1\nSELECT nosuchcol FROM dbo.t")
		e, ok := errors.AsType[mssql.Error](err)
		if !ok || e.Number != 207 || e.LineNo != 2 {
			t.Errorf("error = %v (line %d), want 207 on line 2", err, e.LineNo)
		}
		if err != nil && err.Error()[:6] == "gosmo:" {
			t.Errorf("a read error was reported as a USE failure: %v", err)
		}
	})

	if srv.Info().IsAzure() {
		t.Log("Azure: no OFFLINE, and the no-access login is left to the boxed instances")
		return
	}
	t.Run("offline database", func(t *testing.T) {
		off, drop := liveScratchDB(t, pool, ctx, "gosmo_usebatch_offline")
		defer drop()
		if _, err := pool.ExecContext(ctx, "ALTER DATABASE [gosmo_usebatch_offline] SET OFFLINE WITH ROLLBACK IMMEDIATE"); err != nil {
			t.Fatalf("offline: %v", err)
		}
		defer pool.ExecContext(context.Background(), "ALTER DATABASE [gosmo_usebatch_offline] SET ONLINE")
		for what, err := range liveReadErrors(ctx, off) {
			assertUseError(t, what, err, off.Name, 942)
		}
	})
	t.Run("no access", func(t *testing.T) {
		pool.ExecContext(ctx, "IF SUSER_ID('gosmo_usebatch_noaccess') IS NOT NULL DROP LOGIN gosmo_usebatch_noaccess")
		if _, err := pool.ExecContext(ctx, "CREATE LOGIN gosmo_usebatch_noaccess WITH PASSWORD = 'Xy9!gosmo-usebatch', CHECK_POLICY = OFF"); err != nil {
			t.Fatalf("create login: %v", err)
		}
		defer pool.ExecContext(context.Background(), "DROP LOGIN gosmo_usebatch_noaccess")
		lp, err := openAs(t, "gosmo_usebatch_noaccess", "Xy9!gosmo-usebatch")
		if err != nil {
			t.Fatalf("openAs: %v", err)
		}
		defer lp.Close()
		d := (&Server{db: lp}).DatabaseRef("gosmo_usebatch_b")
		for what, err := range liveReadErrors(ctx, d) {
			assertUseError(t, what, err, d.Name, 916)
		}
	})
}
