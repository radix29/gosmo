//go:build livedb

// Live verification of RestoreOptions.CloseExistingConnections: the RESTORE
// clears the database of other sessions in its own batch, puts it back to
// MULTI_USER, and leaves alone the states whose ALTER the server refuses —
// a database that does not exist yet, one left RESTORING, one in STANDBY.
//
// The guards are what a string test cannot check: each refused ALTER would
// abort the RESTORE behind it, and whether SQL Server refuses it is the
// server's to say.
//
//	go test -tags livedb . -run TestLiveRestoreClose -v \
//	  -livedb 'sqlserver://sa:PASS@host?TrustServerCertificate=true'
//
// Creates and drops its own databases and backup files; touches nothing else.
package gosmo

import (
	"context"
	"database/sql"
	"testing"
)

func TestLiveRestoreCloseExistingConnections(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()

	const name, copyName = "gosmo_restore_close_live", "gosmo_restore_close_live_copy"
	_, drop := liveScratchDB(t, db, ctx, name)
	defer drop()
	srv, err := NewServer(ctx, db)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	if srv.refusesSingleUser() {
		t.Skip("a Managed Instance restores FROM URL only; its KILL form is pinned by unit tests")
	}

	device := liveBackupPath(t, srv, ctx, name+".bak")
	undo := liveBackupPath(t, srv, ctx, name+"_undo.dat")
	if err := srv.Backup(ctx, BackupOptions{Database: name, Devices: []BackupTarget{DiskTarget(device)}, Init: true}); err != nil {
		t.Fatalf("backup: %v", err)
	}
	defer db.ExecContext(context.Background(), "EXEC master.dbo.xp_delete_files @FilePath = N'"+device+"'")
	defer db.ExecContext(context.Background(), "EXEC master.dbo.xp_delete_files @FilePath = N'"+undo+"'")
	defer db.ExecContext(context.Background(), "EXEC msdb.dbo.sp_delete_database_backuphistory @database_name = N'"+name+"'")

	state := func(t *testing.T, db string) (string, string) {
		t.Helper()
		var st, access string
		err := srv.db.QueryRowContext(ctx,
			"SELECT state_desc, user_access_desc FROM sys.databases WHERE name = @p1", db).Scan(&st, &access)
		if err == sql.ErrNoRows {
			return "(no such database)", ""
		}
		if err != nil {
			t.Fatalf("read state of %s: %v", db, err)
		}
		return st, access
	}
	restore := func(opts RestoreOptions) error {
		opts.Database, opts.Devices, opts.Replace = name, []BackupTarget{DiskTarget(device)}, true
		return srv.Restore(ctx, opts)
	}

	// A second session parked in the database — its own pool, so nothing
	// here shares or recycles it.
	other, err := sql.Open("sqlserver", *liveDSN)
	if err != nil {
		t.Fatalf("open the second pool: %v", err)
	}
	defer other.Close()
	parked, err := other.Conn(ctx)
	if err != nil {
		t.Fatalf("second session: %v", err)
	}
	defer parked.Close()
	if _, err := parked.ExecContext(ctx, "USE "+quoteIdent(name)); err != nil {
		t.Fatalf("park the second session: %v", err)
	}

	t.Run("a parked session blocks a plain restore", func(t *testing.T) {
		err := restore(RestoreOptions{Recovery: RestoreWithRecovery})
		if err == nil {
			t.Fatal("restored with another session in the database; the parked session is not doing its job")
		}
		t.Logf("refused as staged: %v", err)
	})

	t.Run("closing connections restores and releases", func(t *testing.T) {
		if err := restore(RestoreOptions{Recovery: RestoreWithRecovery, CloseExistingConnections: true}); err != nil {
			t.Fatalf("restore: %v", err)
		}
		if st, access := state(t, name); st != "ONLINE" || access != "MULTI_USER" {
			t.Errorf("after the restore the database is %s / %s, want ONLINE / MULTI_USER", st, access)
		}
	})

	t.Run("NORECOVERY skips the release it would be refused", func(t *testing.T) {
		if err := restore(RestoreOptions{Recovery: RestoreWithNoRecovery, CloseExistingConnections: true}); err != nil {
			t.Fatalf("restore: %v", err)
		}
		if st, _ := state(t, name); st != "RESTORING" {
			t.Errorf("after a NORECOVERY restore the database is %s, want RESTORING", st)
		}
		// Now RESTORING: nobody to close, and SET SINGLE_USER would be
		// refused (Msg 5052) and abort the RESTORE behind it.
		if err := restore(RestoreOptions{Recovery: RestoreWithRecovery, CloseExistingConnections: true}); err != nil {
			t.Fatalf("restore over a RESTORING database: %v", err)
		}
		if st, access := state(t, name); st != "ONLINE" || access != "MULTI_USER" {
			t.Errorf("after recovery the database is %s / %s, want ONLINE / MULTI_USER", st, access)
		}
	})

	t.Run("STANDBY skips both ALTERs it would be refused", func(t *testing.T) {
		opts := RestoreOptions{Recovery: RestoreWithStandBy, StandByFile: undo, CloseExistingConnections: true}
		if err := restore(opts); err != nil {
			t.Fatalf("restore into standby: %v", err)
		}
		if err := restore(opts); err != nil {
			t.Fatalf("restore over a standby database: %v", err)
		}
		if err := restore(RestoreOptions{Recovery: RestoreWithRecovery, CloseExistingConnections: true}); err != nil {
			t.Fatalf("recover: %v", err)
		}
		if st, access := state(t, name); st != "ONLINE" || access != "MULTI_USER" {
			t.Errorf("after recovery the database is %s / %s, want ONLINE / MULTI_USER", st, access)
		}
	})

	t.Run("a database that does not exist yet", func(t *testing.T) {
		files, err := srv.BackupFileList(ctx, DiskTarget(device), 0)
		if err != nil {
			t.Fatalf("file list: %v", err)
		}
		var move []RelocateFile
		for _, f := range files {
			move = append(move, RelocateFile{LogicalName: f.LogicalName,
				PhysicalName: liveBackupPath(t, srv, ctx, copyName+"_"+f.LogicalName)})
		}
		defer db.ExecContext(context.Background(), "IF DB_ID('"+copyName+"') IS NOT NULL DROP DATABASE "+quoteIdent(copyName))
		if err := srv.Restore(ctx, RestoreOptions{
			Database: copyName, Devices: []BackupTarget{DiskTarget(device)}, RelocateFiles: move,
			CloseExistingConnections: true,
		}); err != nil {
			t.Fatalf("restore as a new database: %v", err)
		}
		if st, access := state(t, copyName); st != "ONLINE" || access != "MULTI_USER" {
			t.Errorf("the new database is %s / %s, want ONLINE / MULTI_USER", st, access)
		}
	})
}

// The log-shipping secondary case: a database in STANDBY is readable, so it
// has readers, and SET SINGLE_USER is refused there. Closing connections kills
// them instead, so the next log restores rather than failing with "Exclusive
// access could not be obtained".
func TestLiveRestoreCloseReleasesAStandbyReader(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()

	const name = "gosmo_restore_standby_live"
	_, drop := liveScratchDB(t, db, ctx, name)
	// A STANDBY database refuses the SINGLE_USER the scratch drop leads with,
	// so recover it first; on a database already online this is refused and
	// harmless.
	defer drop()
	defer db.ExecContext(context.Background(), "RESTORE DATABASE "+quoteIdent(name)+" WITH RECOVERY")
	srv, err := NewServer(ctx, db)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	if srv.refusesSingleUser() {
		t.Skip("a Managed Instance restores FROM URL only")
	}

	full := liveBackupPath(t, srv, ctx, name+".bak")
	logDev := liveBackupPath(t, srv, ctx, name+".trn")
	undo := liveBackupPath(t, srv, ctx, name+"_undo.dat")
	for _, f := range []string{full, logDev, undo} {
		defer db.ExecContext(context.Background(), "EXEC master.dbo.xp_delete_files @FilePath = N'"+f+"'")
	}
	defer db.ExecContext(context.Background(), "EXEC msdb.dbo.sp_delete_database_backuphistory @database_name = N'"+name+"'")

	if _, err := db.ExecContext(ctx, "ALTER DATABASE "+quoteIdent(name)+" SET RECOVERY FULL"); err != nil {
		t.Fatalf("recovery model: %v", err)
	}
	if err := srv.Backup(ctx, BackupOptions{Database: name, Devices: []BackupTarget{DiskTarget(full)}, Init: true}); err != nil {
		t.Fatalf("full backup: %v", err)
	}
	if _, err := db.ExecContext(ctx, "CREATE TABLE "+quoteIdent(name)+".dbo.t (i int); INSERT "+quoteIdent(name)+".dbo.t VALUES (1)"); err != nil {
		t.Fatalf("log some work: %v", err)
	}
	if err := srv.Backup(ctx, BackupOptions{Database: name, Action: BackupActionLog, Devices: []BackupTarget{DiskTarget(logDev)}, Init: true}); err != nil {
		t.Fatalf("log backup: %v", err)
	}
	if err := srv.Restore(ctx, RestoreOptions{
		Database: name, Devices: []BackupTarget{DiskTarget(full)}, Replace: true,
		Recovery: RestoreWithStandBy, StandByFile: undo, CloseExistingConnections: true,
	}); err != nil {
		t.Fatalf("restore the full backup into standby: %v", err)
	}

	// A reader parked in the standby database, on its own pool.
	other, err := sql.Open("sqlserver", *liveDSN)
	if err != nil {
		t.Fatalf("open the second pool: %v", err)
	}
	defer other.Close()
	parked, err := other.Conn(ctx)
	if err != nil {
		t.Fatalf("second session: %v", err)
	}
	defer parked.Close()
	var spid int
	if err := parked.QueryRowContext(ctx, "USE "+quoteIdent(name)+"; SELECT @@SPID").Scan(&spid); err != nil {
		t.Fatalf("park the reader: %v", err)
	}

	if err := srv.Restore(ctx, RestoreOptions{
		Database: name, Action: BackupActionLog, Devices: []BackupTarget{DiskTarget(logDev)},
		Recovery: RestoreWithStandBy, StandByFile: undo, CloseExistingConnections: true,
	}); err != nil {
		t.Fatalf("restore the next log over a standby database with a reader: %v", err)
	}

	var standby bool
	if err := db.QueryRowContext(ctx, "SELECT is_in_standby FROM sys.databases WHERE name = @p1", name).Scan(&standby); err != nil {
		t.Fatalf("read standby: %v", err)
	}
	if !standby {
		t.Error("after the log restore the database is not in standby")
	}
	var left int
	if err := db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM sys.dm_exec_sessions WHERE session_id = @p1 AND database_id = DB_ID(@p2)", spid, name).Scan(&left); err != nil {
		t.Fatalf("read sessions: %v", err)
	}
	if left != 0 {
		t.Errorf("the reader's session %d is still in the database", spid)
	}
	var n int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+quoteIdent(name)+".dbo.t").Scan(&n); err != nil || n != 1 {
		t.Errorf("the log's row after the restore: %d, %v; want 1", n, err)
	}
}
