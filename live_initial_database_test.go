//go:build livedb

// Live verification that the exclusive database writes run from master when
// the connection was opened on the very database they name.
//
// A pooled connection is reset on every checkout to the DSN's database
// (ConnectionOptions.Database), so a Server opened on Sales runs a DROP
// DATABASE [Sales] from a session in Sales — which fails Msg 3702 even after
// SET SINGLE_USER, because the executing session is then the single user.
// RESTORE fails Msg 3102 and sp_detach_db Msg 3703 the same way (see
// fromMaster). Rename, SET OFFLINE and SET READ_ONLY succeed from there and
// are pinned as such, so a later change knows they were checked.
//
//	go test -tags livedb . -run TestLiveInitialDatabase -v \
//	  -livedb 'sqlserver://sa:PASS@host?TrustServerCertificate=true'
//
// Creates and drops its own throwaway database, snapshot and backup file;
// touches nothing else.
package gosmo

import (
	"context"
	"database/sql"
	"net/url"
	"strings"
	"testing"
)

const initDBLiveName = "zz_gossms_initial_db_probe"

// liveServerInDatabase opens a second Server whose DSN names database as its
// initial one, and checks that its sessions really are in it.
func liveServerInDatabase(t *testing.T, ctx context.Context, database string) *Server {
	t.Helper()
	u, err := url.Parse(*liveDSN)
	if err != nil || u.Scheme != "sqlserver" {
		t.Skipf("-livedb is not a sqlserver:// URL, so no database can be added to it: %v", err)
	}
	q := u.Query()
	q.Set("database", database)
	u.RawQuery = q.Encode()
	db, err := sql.Open("sqlserver", u.String())
	if err != nil {
		t.Fatalf("open on %s: %v", database, err)
	}
	t.Cleanup(func() { db.Close() })
	srv, err := NewServer(ctx, db)
	if err != nil {
		t.Fatalf("NewServer on %s: %v", database, err)
	}
	var cur string
	if err := db.QueryRowContext(ctx, "SELECT DB_NAME()").Scan(&cur); err != nil || cur != database {
		t.Fatalf("session is in %q (%v), want %q", cur, err, database)
	}
	return srv
}

// initDBLiveSetup creates the throwaway database through admin, registers its
// drop, and returns its file paths, primary first.
func initDBLiveSetup(t *testing.T, ctx context.Context, admin *Server) []string {
	t.Helper()
	drop := func() {
		admin.exec(context.Background(), "IF DB_ID(N'"+initDBLiveName+"_snap') IS NOT NULL DROP DATABASE "+quoteIdent(initDBLiveName+"_snap"))
		admin.exec(context.Background(), "IF DB_ID(N'"+initDBLiveName+"2') IS NOT NULL ALTER DATABASE "+quoteIdent(initDBLiveName+"2")+" MODIFY NAME = "+quoteIdent(initDBLiveName))
		admin.exec(context.Background(),
			"IF DB_ID(N'"+initDBLiveName+"') IS NOT NULL BEGIN "+
				"ALTER DATABASE "+quoteIdent(initDBLiveName)+" SET ONLINE; "+
				"ALTER DATABASE "+quoteIdent(initDBLiveName)+" SET MULTI_USER WITH ROLLBACK IMMEDIATE; "+
				"DROP DATABASE "+quoteIdent(initDBLiveName)+"; END")
	}
	drop()
	if _, err := admin.exec(ctx, "CREATE DATABASE "+quoteIdent(initDBLiveName)); err != nil {
		t.Fatalf("create throwaway database: %v", err)
	}
	t.Cleanup(drop)
	rows, err := admin.db.QueryContext(ctx,
		"SELECT physical_name FROM sys.master_files WHERE database_id = DB_ID(@p1) ORDER BY file_id", initDBLiveName)
	if err != nil {
		t.Fatalf("read files: %v", err)
	}
	defer rows.Close()
	var files []string
	for rows.Next() {
		var f string
		if err := rows.Scan(&f); err != nil {
			t.Fatalf("read files: %v", err)
		}
		files = append(files, f)
	}
	if err := rows.Err(); err != nil || len(files) == 0 {
		t.Fatalf("read files: %v (%d)", err, len(files))
	}
	return files
}

var initDBTerminations = []struct {
	name string
	term Termination
}{{"plain", TerminationNone}, {"forced", TerminationRollbackImmediate}}

func TestLiveInitialDatabaseDrop(t *testing.T) {
	db, ctx, done := liveDB(t)
	t.Cleanup(done)
	admin := liveServer(t, db, ctx)
	for _, tc := range initDBTerminations {
		t.Run(tc.name, func(t *testing.T) {
			initDBLiveSetup(t, ctx, admin)
			in := liveServerInDatabase(t, ctx, initDBLiveName)
			if err := in.DatabaseRef(initDBLiveName).Drop(ctx, tc.term); err != nil {
				t.Fatalf("Drop from a connection opened on the database: %v", err)
			}
			if databaseExists(t, db, ctx, initDBLiveName) {
				t.Error("Drop returned nil, but the database is still there")
			}
		})
	}
}

func TestLiveInitialDatabaseDetach(t *testing.T) {
	db, ctx, done := liveDB(t)
	t.Cleanup(done)
	admin := liveServer(t, db, ctx)
	for _, dropConns := range []bool{false, true} {
		t.Run(map[bool]string{false: "plain", true: "forced"}[dropConns], func(t *testing.T) {
			files := initDBLiveSetup(t, ctx, admin)
			in := liveServerInDatabase(t, ctx, initDBLiveName)
			if err := in.DatabaseRef(initDBLiveName).Detach(ctx, DetachOptions{DropConnections: dropConns}); err != nil {
				t.Fatalf("Detach from a connection opened on the database: %v", err)
			}
			if databaseExists(t, db, ctx, initDBLiveName) {
				t.Fatal("Detach returned nil, but the database is still attached")
			}
			// Attached back, so the cleanup's DROP deletes the files.
			if err := admin.AttachDatabase(ctx, AttachSpec{Name: initDBLiveName, Files: files}); err != nil {
				t.Fatalf("attach back: %v", err)
			}
		})
	}
}

func TestLiveInitialDatabaseRestore(t *testing.T) {
	db, ctx, done := liveDB(t)
	t.Cleanup(done)
	admin := liveServer(t, db, ctx)
	dir := admin.Info().DefaultBackupPath
	if dir == "" {
		t.Skip("the instance reported no default backup path")
	}
	sep := "/"
	if admin.Info().IsWindows() {
		sep = `\`
	}
	bak := strings.TrimRight(dir, `\/`) + sep + initDBLiveName + ".bak"
	t.Cleanup(func() {
		admin.exec(context.Background(), "EXEC master.sys.xp_delete_file 0, "+QuoteLiteral(bak))
	})
	for _, close := range []bool{false, true} {
		for _, progress := range []bool{false, true} {
			name := map[bool]string{false: "plain", true: "close"}[close] + map[bool]string{false: "", true: "+progress"}[progress]
			t.Run(name, func(t *testing.T) {
				initDBLiveSetup(t, ctx, admin)
				if err := admin.Backup(ctx, BackupOptions{Database: initDBLiveName, Devices: []BackupTarget{DiskTarget(bak)},
					CopyOnly: true, Init: true}); err != nil {
					t.Fatalf("backup: %v", err)
				}
				in := liveServerInDatabase(t, ctx, initDBLiveName)
				opts := RestoreOptions{Database: initDBLiveName, Devices: []BackupTarget{DiskTarget(bak)},
					Replace: true, CloseExistingConnections: close}
				if progress {
					opts.Progress = func(int, string) {}
				}
				if err := in.Restore(ctx, opts); err != nil {
					t.Fatalf("Restore from a connection opened on the database: %v", err)
				}
			})
		}
	}
}

func TestLiveInitialDatabaseRestoreFromSnapshot(t *testing.T) {
	db, ctx, done := liveDB(t)
	t.Cleanup(done)
	admin := liveServer(t, db, ctx)
	initDBLiveSetup(t, ctx, admin)
	if _, err := admin.CreateDatabaseSnapshot(ctx, CreateDatabaseSnapshotRequest{
		Name: initDBLiveName + "_snap", SourceDatabase: initDBLiveName}); err != nil {
		t.Skipf("create snapshot (edition without snapshots?): %v", err)
	}
	in := liveServerInDatabase(t, ctx, initDBLiveName)
	if err := in.RestoreFromSnapshot(ctx, initDBLiveName, initDBLiveName+"_snap"); err != nil {
		t.Fatalf("RestoreFromSnapshot from a connection opened on the database: %v", err)
	}
}

// The writes fromMaster leaves alone: each succeeds from a session in the
// database it names. If one starts failing here, it needs the prefix too.
func TestLiveInitialDatabaseWritesThatNeedNoMaster(t *testing.T) {
	db, ctx, done := liveDB(t)
	t.Cleanup(done)
	admin := liveServer(t, db, ctx)
	for _, tc := range initDBTerminations {
		t.Run("rename "+tc.name, func(t *testing.T) {
			initDBLiveSetup(t, ctx, admin)
			in := liveServerInDatabase(t, ctx, initDBLiveName)
			if err := in.DatabaseRef(initDBLiveName).Rename(ctx, initDBLiveName+"2", tc.term); err != nil {
				t.Fatalf("Rename from a connection opened on the database: %v", err)
			}
			// The cleanup renames it back before dropping.
		})
		t.Run("read-only "+tc.name, func(t *testing.T) {
			initDBLiveSetup(t, ctx, admin)
			in := liveServerInDatabase(t, ctx, initDBLiveName)
			if err := in.DatabaseRef(initDBLiveName).SetReadOnly(ctx, true, tc.term); err != nil {
				t.Fatalf("SetReadOnly from a connection opened on the database: %v", err)
			}
		})
	}
	t.Run("offline", func(t *testing.T) {
		initDBLiveSetup(t, ctx, admin)
		in := liveServerInDatabase(t, ctx, initDBLiveName)
		if err := in.DatabaseRef(initDBLiveName).SetOffline(ctx); err != nil {
			t.Fatalf("SetOffline from a connection opened on the database: %v", err)
		}
	})
}
