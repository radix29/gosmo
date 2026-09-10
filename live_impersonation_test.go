//go:build livedb

// Live verification of readImpersonated (BUG-9 in gossms's review): an
// EXECUTE AS … REVERT batch whose read stops short leaves its pooled session
// impersonating, and the next caller handed that connection fails. Only the
// server can show either half — that an ad hoc EXECUTE AS outlives its batch,
// and what a reset of such a session does.
//
// The effective-permission reads finish in well under a millisecond after
// their first row, too fast to cut short on purpose, so these tests run
// readImpersonated over the same kind of batch with a WAITFOR widening the
// gap before the REVERT.
//
//	go test -tags livedb . -run TestLiveImpersonat -v \
//	  -livedb 'sqlserver://sa:PASS@host?TrustServerCertificate=true'
//
// Creates and drops a user WITHOUT LOGIN in tempdb; touches nothing else.
package gosmo

import (
	"context"
	"database/sql"
	"errors"
	"testing"
)

const liveImpUser = "gosmo_live_imp"

// liveImpersonation returns a one-connection pool — so the connection a read
// leaves behind is the one the next caller gets — and tempdb, holding a user
// to impersonate.
func liveImpersonation(t *testing.T) (*sql.DB, context.Context, *Database) {
	t.Helper()
	db, ctx, done := liveDB(t)
	t.Cleanup(done)
	db.SetMaxOpenConns(1)
	if _, err := db.ExecContext(ctx, "USE tempdb; IF DATABASE_PRINCIPAL_ID(N'"+liveImpUser+"') IS NULL CREATE USER "+liveImpUser+" WITHOUT LOGIN"); err != nil {
		t.Fatalf("create user: %v", err)
	}
	t.Cleanup(func() {
		if _, err := db.ExecContext(context.Background(), "USE tempdb; DROP USER IF EXISTS "+liveImpUser); err != nil {
			t.Errorf("drop user %s in tempdb: %v — drop it by hand", liveImpUser, err)
		}
	})
	return db, ctx, liveServer(t, db, ctx).Database("tempdb")
}

// sessionIdentity is who the pool's next caller runs as, and on which
// physical connection — a discarded connection comes back with a new id even
// when the server reuses the SPID.
func sessionIdentity(ctx context.Context, db *sql.DB) (user, connID string, err error) {
	err = db.QueryRowContext(ctx, "SELECT USER_NAME(), CAST(connection_id AS nvarchar(40)) "+
		"FROM sys.dm_exec_connections WHERE session_id = @@SPID").Scan(&user, &connID)
	return user, connID, err
}

// The premise: impersonation is session state, not batch state. Two batches
// on one pinned connection, nothing reset between them.
func TestLiveImpersonationOutlivesItsBatch(t *testing.T) {
	db, ctx, _ := liveImpersonation(t)
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "USE tempdb; EXECUTE AS USER = N'"+liveImpUser+"'"); err != nil {
		t.Fatalf("EXECUTE AS: %v", err)
	}
	var user string
	if err := conn.QueryRowContext(ctx, "SELECT USER_NAME()").Scan(&user); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx, "REVERT"); err != nil {
		t.Fatalf("REVERT: %v", err)
	}
	if user != liveImpUser {
		t.Fatalf("the batch after EXECUTE AS runs as %q, want %q — the impersonation ended with its batch", user, liveImpUser)
	}
}

// impersonatedBatch returns readImpersonated's three columns, more than one
// packet of them so the first rows reach the client while the batch is still
// short of its REVERT.
const impersonatedBatch = "EXECUTE AS USER = N'" + liveImpUser + "';\n" +
	"SELECT TOP (3000) a.name, N'', N'SELECT' FROM sys.all_objects a CROSS JOIN (VALUES (1), (2)) v(n);\n" +
	"WAITFOR DELAY '00:00:01';\n" +
	"REVERT;"

// startImpersonatedRead issues impersonatedBatch and returns once its first
// rows have arrived, with the context the read runs under.
func startImpersonatedRead(t *testing.T, ctx context.Context, d *Database) (*dbRows, context.CancelFunc) {
	t.Helper()
	rctx, cancel := context.WithCancel(ctx)
	rows, err := d.query(rctx, impersonatedBatch)
	if err != nil {
		cancel()
		t.Fatalf("query: %v", err)
	}
	return rows, cancel
}

func TestLiveImpersonatedReadCutShort(t *testing.T) {
	db, ctx, d := liveImpersonation(t)
	// The control, and what effectivePermissions did before readImpersonated:
	// scan, and close on the way out. Without it this test cannot fail.
	t.Run("plain close poisons the pool", func(t *testing.T) {
		rows, cancel := startImpersonatedRead(t, ctx, d)
		cancel()
		_, scanErr := scanEffectivePermissions(rows.Rows)
		rows.Close()
		if !errors.Is(scanErr, context.Canceled) {
			t.Fatalf("scan error = %v, want the cancellation", scanErr)
		}
		user, _, err := sessionIdentity(ctx, db)
		if err == nil {
			t.Fatalf("the next caller ran fine as %q — the hazard readImpersonated guards against did not happen", user)
		}
		t.Logf("next caller after a plain close: %v", err)
		// Whatever it was, the pool is usable again after it.
		if _, _, err := sessionIdentity(ctx, db); err != nil {
			t.Fatalf("second caller: %v", err)
		}
	})

	t.Run("cancelled mid-read", func(t *testing.T) {
		_, prev, err := sessionIdentity(ctx, db)
		if err != nil {
			t.Fatal(err)
		}
		rows, cancel := startImpersonatedRead(t, ctx, d)
		cancel()
		if _, err := readImpersonated(rows); !errors.Is(err, context.Canceled) {
			t.Fatalf("readImpersonated = %v, want the cancellation", err)
		}
		user, id, err := sessionIdentity(ctx, db)
		if err != nil {
			t.Fatalf("the next caller failed: %v — the impersonated connection went back to the pool", err)
		}
		if user == liveImpUser {
			t.Fatalf("the next caller runs as %q", user)
		}
		if id == prev {
			t.Errorf("the next caller got the same connection %s — it was not discarded", id)
		}
	})

	t.Run("complete read keeps the connection", func(t *testing.T) {
		_, prev, err := sessionIdentity(ctx, db)
		if err != nil {
			t.Fatal(err)
		}
		rows, cancel := startImpersonatedRead(t, ctx, d)
		defer cancel()
		perms, err := readImpersonated(rows)
		if err != nil || len(perms) != 3000 {
			t.Fatalf("readImpersonated = %d rows, %v; want 3000, nil", len(perms), err)
		}
		user, id, err := sessionIdentity(ctx, db)
		if err != nil {
			t.Fatalf("the next caller failed: %v", err)
		}
		if user == liveImpUser {
			t.Fatalf("the next caller runs as %q — a complete read did not reach the REVERT", user)
		}
		if id != prev {
			t.Errorf("a complete read discarded its connection (%s → %s); only a stopped one should", prev, id)
		}
	})
}

// The public reads still answer, over the pinned connection
// EffectiveServerPermissions now uses as well.
func TestLiveImpersonatedPublicReads(t *testing.T) {
	db, ctx, d := liveImpersonation(t)
	perms, err := d.EffectivePermissionsContext(ctx, liveImpUser)
	if err != nil || len(perms) == 0 {
		t.Fatalf("EffectivePermissions = %d, %v", len(perms), err)
	}
	var login string
	if err := db.QueryRowContext(ctx, "SELECT SUSER_SNAME()").Scan(&login); err != nil {
		t.Fatal(err)
	}
	sp, err := d.server.EffectiveServerPermissionsContext(ctx, login)
	if err != nil || len(sp) == 0 {
		t.Fatalf("EffectiveServerPermissions(%s) = %d, %v", login, len(sp), err)
	}
	if user, _, err := sessionIdentity(ctx, db); err != nil || user == liveImpUser {
		t.Fatalf("after both reads the pool's next caller is %q, %v", user, err)
	}
}
