//go:build livedb

// Live verification that a Server opened on a database keeps working when that
// database starts refusing logins — after this Server's own write (RESTORE
// WITH NORECOVERY, SET SINGLE_USER) or another session's (SET OFFLINE,
// RESTORE WITH NORECOVERY, SINGLE_USER held elsewhere). Before the retarget
// covered them, each read failed once the pool dialled again, with Msg 4063 at
// login, until the caller reconnected — except after this Server's own
// SINGLE_USER, where the next dial took the slot its idle session had freed.
//
//	go test -tags livedb . -run TestLiveLoginRefusal -v \
//	  -livedb 'sqlserver://sa:PASS@host?TrustServerCertificate=true'
//
// Uses the zz_gossms_initial_db_probe throwaway database and a backup file of
// it; touches nothing else.
package gosmo

import (
	"context"
	"errors"
	"net"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	mssql "github.com/microsoft/go-mssqldb"
)

// loginRefusalIdle is the pool's ConnMaxIdleTime here: short, so a test can
// outwait it and make the pool dial again rather than reuse a session.
const loginRefusalIdle = 2 * time.Second

// liveConnectOn opens a Server through Connect — so its pool can be moved —
// whose login database is database.
func liveConnectOn(t *testing.T, ctx context.Context, database string) *Server {
	t.Helper()
	u, err := url.Parse(*liveDSN)
	if err != nil || u.Scheme != "sqlserver" {
		t.Skipf("-livedb is not a sqlserver:// URL: %v", err)
	}
	opts := ConnectionOptions{
		Server:                 u.Hostname(),
		User:                   u.User.Username(),
		Database:               database,
		TrustServerCertificate: strings.EqualFold(u.Query().Get("TrustServerCertificate"), "true"),
		Encrypt:                u.Query().Get("encrypt"),
		ConnMaxIdleTime:        loginRefusalIdle,
	}
	opts.Password, _ = u.User.Password()
	if inst := strings.Trim(u.Path, "/"); inst != "" {
		opts.Server += `\` + inst
	}
	if _, p, err := net.SplitHostPort(u.Host); err == nil {
		opts.Port, _ = strconv.Atoi(p)
	}
	srv, err := Connect(ctx, opts)
	if err != nil {
		t.Fatalf("Connect on %s: %v", database, err)
	}
	t.Cleanup(func() { srv.Close() })
	return srv
}

// readAfterIdle waits out the pool's idle connections, so the read dials
// again, then lists the databases and checks the pool logs in to want.
func readAfterIdle(t *testing.T, ctx context.Context, srv *Server, want string) {
	t.Helper()
	time.Sleep(loginRefusalIdle + time.Second)
	if _, err := srv.Databases(ctx); err != nil {
		t.Fatalf("Databases after the pool dialled again: %v", err)
	}
	if got := srv.InitialDatabase(); got != want {
		t.Errorf("InitialDatabase = %q, want %q", got, want)
	}
}

// recoverProbe brings the throwaway database back out of RESTORING, so the
// setup's drop finds it ONLINE.
func recoverProbe(admin *Server) {
	admin.exec(context.Background(), "IF DATABASEPROPERTYEX(N'"+initDBLiveName+"', 'Status') = 'RESTORING' RESTORE DATABASE "+quoteIdent(initDBLiveName)+" WITH RECOVERY")
}

func TestLiveLoginRefusal(t *testing.T) {
	// liveDB's 60 s is too short for the idle waits here.
	db, ctx, done := liveDBTimeout(t, 5*time.Minute)
	t.Cleanup(done)
	admin := liveServer(t, db, ctx)

	bak := ""
	backup := func(t *testing.T) string {
		t.Helper()
		if bak != "" {
			return bak
		}
		dir := admin.Info().DefaultBackupPath
		if dir == "" {
			t.Skip("the instance reported no default backup path")
		}
		sep := "/"
		if admin.Info().IsWindows() {
			sep = `\`
		}
		bak = strings.TrimRight(dir, `\/`) + sep + initDBLiveName + "_refusal.bak"
		t.Cleanup(func() {
			admin.exec(context.Background(), "EXEC master.sys.xp_delete_file 0, "+QuoteLiteral(bak))
		})
		return bak
	}
	backupProbe := func(t *testing.T) string {
		t.Helper()
		path := backup(t)
		if err := admin.Backup(ctx, BackupOptions{Database: initDBLiveName, Devices: []BackupTarget{DiskTarget(path)},
			CopyOnly: true, Init: true}); err != nil {
			t.Fatalf("backup: %v", err)
		}
		return path
	}

	t.Run("own RESTORE NORECOVERY", func(t *testing.T) {
		initDBLiveSetup(t, ctx, admin)
		t.Cleanup(func() { recoverProbe(admin) })
		path := backupProbe(t)
		srv := liveConnectOn(t, ctx, initDBLiveName)
		if err := srv.Restore(ctx, RestoreOptions{Database: initDBLiveName, Devices: []BackupTarget{DiskTarget(path)},
			Replace: true, Recovery: RestoreWithNoRecovery}); err != nil {
			t.Fatalf("Restore WITH NORECOVERY: %v", err)
		}
		if _, err := srv.Databases(ctx); err != nil {
			t.Fatalf("Databases straight after: %v", err)
		}
		readAfterIdle(t, ctx, srv, "master")
	})

	t.Run("own SINGLE_USER", func(t *testing.T) {
		initDBLiveSetup(t, ctx, admin)
		srv := liveConnectOn(t, ctx, initDBLiveName)
		d := srv.DatabaseRef(initDBLiveName)
		if err := d.SetUserAccess(ctx, UserAccessSingle); err != nil {
			t.Fatalf("SetUserAccess SINGLE_USER: %v", err)
		}
		if _, err := srv.Databases(ctx); err != nil {
			t.Fatalf("Databases straight after: %v", err)
		}
		readAfterIdle(t, ctx, srv, "master")
		if err := d.SetUserAccess(ctx, UserAccessMulti); err != nil {
			t.Fatalf("SetUserAccess MULTI_USER back: %v", err)
		}
		readAfterIdle(t, ctx, srv, "master")
	})

	// Another session's write: nothing told this Server, so only the refused
	// dial can move it.
	external := []struct {
		name  string
		write func(t *testing.T)
	}{
		{"OFFLINE", func(t *testing.T) {
			if err := admin.DatabaseRef(initDBLiveName).SetOffline(ctx); err != nil {
				t.Fatalf("SetOffline: %v", err)
			}
		}},
		{"RESTORE NORECOVERY", func(t *testing.T) {
			t.Cleanup(func() { recoverProbe(admin) })
			path := backupProbe(t)
			if err := admin.Restore(ctx, RestoreOptions{Database: initDBLiveName, Devices: []BackupTarget{DiskTarget(path)},
				Replace: true, Recovery: RestoreWithNoRecovery, CloseExistingConnections: true}); err != nil {
				t.Fatalf("Restore WITH NORECOVERY: %v", err)
			}
		}},
		{"SINGLE_USER held", func(t *testing.T) {
			conn, err := admin.db.Conn(ctx)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				conn.ExecContext(context.Background(), "ALTER DATABASE "+quoteIdent(initDBLiveName)+" SET MULTI_USER")
				conn.Close()
			})
			if _, err := conn.ExecContext(ctx, "USE "+quoteIdent(initDBLiveName)+"; ALTER DATABASE "+
				quoteIdent(initDBLiveName)+" SET SINGLE_USER WITH ROLLBACK IMMEDIATE"); err != nil {
				t.Fatalf("SINGLE_USER: %v", err)
			}
		}},
	}
	for _, tc := range external {
		t.Run("other session's "+tc.name, func(t *testing.T) {
			initDBLiveSetup(t, ctx, admin)
			srv := liveConnectOn(t, ctx, initDBLiveName)
			tc.write(t)
			readAfterIdle(t, ctx, srv, "master")
		})
	}

	// A login database refused from the start fails Connect: the fallback
	// is for a pool that was working.
	t.Run("refused at Connect", func(t *testing.T) {
		u, _ := url.Parse(*liveDSN)
		opts := ConnectionOptions{Server: u.Hostname(), User: u.User.Username(), Database: "zz_gossms_no_such_db",
			TrustServerCertificate: strings.EqualFold(u.Query().Get("TrustServerCertificate"), "true"), Encrypt: u.Query().Get("encrypt")}
		opts.Password, _ = u.User.Password()
		if inst := strings.Trim(u.Path, "/"); inst != "" {
			opts.Server += `\` + inst
		}
		if _, p, err := net.SplitHostPort(u.Host); err == nil {
			opts.Port, _ = strconv.Atoi(p)
		}
		srv, err := Connect(ctx, opts)
		if err == nil {
			srv.Close()
			t.Fatal("Connect on a missing database succeeded")
		}
		if me, ok := errors.AsType[mssql.Error](err); ok {
			for _, e := range me.All {
				t.Logf("refused at Connect: Msg %d: %s", e.Number, e.Message)
			}
		}
		if !loginDatabaseRefused(err) {
			t.Errorf("Connect's error is not a login-database refusal: %v", err)
		}
	})
}
