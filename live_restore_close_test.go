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
	// t.Cleanup, not defer: the file removals below are cleanups that
	// run after the drop, which releases a STANDBY undo file.
	t.Cleanup(done)

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
	t.Cleanup(func() { liveRemoveFile(t, db, device) })
	t.Cleanup(func() { liveRemoveFile(t, db, undo) })
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

	// Restore repairs MULTI_USER from Go only when the batch was cut short
	// (T21). That is safe only because a RESTORE the server refuses is
	// statement-level: the batch goes on to its own release. Were it
	// batch-aborting, as a failed DROP DATABASE is, this database would be
	// left SINGLE_USER.
	t.Run("a refused restore is released by its own batch", func(t *testing.T) {
		missing := liveBackupPath(t, srv, ctx, name+"_missing.bak")
		err := srv.Restore(ctx, RestoreOptions{Database: name, Devices: []BackupTarget{DiskTarget(missing)},
			Replace: true, Recovery: RestoreWithRecovery, CloseExistingConnections: true})
		if err == nil {
			t.Fatal("restored from a backup file that does not exist")
		}
		if batchCutShort(err) {
			t.Fatalf("the refusal reads as a batch cut short, so this does not test the batch's own release: %v", err)
		}
		if st, access := state(t, name); st != "ONLINE" || access != "MULTI_USER" {
			t.Errorf("after a refused restore the database is %s / %s, want ONLINE / MULTI_USER", st, access)
		}
	})

	// Y2 (2026-10-14): the release used to be a hard-coded SET MULTI_USER.
	// A refused RESTORE over a database left RESTRICTED_USER opened it to
	// everyone, and a successful one of a RESTRICTED_USER backup did too —
	// RESTORE leaves the backup's own access mode, which the release then
	// overwrote.
	setAccess := func(t *testing.T, mode string) {
		t.Helper()
		if _, err := srv.db.ExecContext(ctx, "ALTER DATABASE "+quoteIdent(name)+" SET "+mode+" WITH ROLLBACK IMMEDIATE"); err != nil {
			t.Fatalf("set %s: %v", mode, err)
		}
	}
	t.Run("a refused restore puts back RESTRICTED_USER", func(t *testing.T) {
		setAccess(t, "RESTRICTED_USER")
		defer setAccess(t, "MULTI_USER")
		for _, c := range []struct {
			what string
			opts RestoreOptions
		}{
			{"missing file", RestoreOptions{Devices: []BackupTarget{DiskTarget(liveBackupPath(t, srv, ctx, name+"_missing.bak"))}}},
			{"no such backup set", RestoreOptions{Devices: []BackupTarget{DiskTarget(device)}, FileNumber: 99}},
		} {
			c.opts.Database, c.opts.Replace, c.opts.CloseExistingConnections = name, true, true
			err := srv.Restore(ctx, c.opts)
			if err == nil {
				t.Fatalf("%s: the restore succeeded", c.what)
			}
			if batchCutShort(err) {
				t.Fatalf("%s: the refusal reads as a batch cut short, so this does not test the batch's own release: %v", c.what, err)
			}
			if st, access := state(t, name); st != "ONLINE" || access != "RESTRICTED_USER" {
				t.Errorf("%s: after a refused restore the database is %s / %s, want ONLINE / RESTRICTED_USER", c.what, st, access)
			}
		}
	})

	t.Run("a successful restore keeps the backup's access mode", func(t *testing.T) {
		restricted := liveBackupPath(t, srv, ctx, name+"_restricted.bak")
		defer liveRemoveFile(t, db, restricted)
		setAccess(t, "RESTRICTED_USER")
		if err := srv.Backup(ctx, BackupOptions{Database: name, Devices: []BackupTarget{DiskTarget(restricted)}, Init: true}); err != nil {
			t.Fatalf("backup: %v", err)
		}
		setAccess(t, "MULTI_USER")
		for _, c := range []struct {
			before, dev, want string
		}{
			{"MULTI_USER", restricted, "RESTRICTED_USER"},
			{"RESTRICTED_USER", device, "MULTI_USER"},
			{"RESTRICTED_USER", restricted, "RESTRICTED_USER"},
		} {
			setAccess(t, c.before)
			err := srv.Restore(ctx, RestoreOptions{Database: name, Devices: []BackupTarget{DiskTarget(c.dev)},
				Replace: true, Recovery: RestoreWithRecovery, CloseExistingConnections: true})
			if err != nil {
				t.Fatalf("restore over %s: %v", c.before, err)
			}
			if st, access := state(t, name); st != "ONLINE" || access != c.want {
				t.Errorf("a %s backup restored over a %s database left it %s / %s, want ONLINE / %s",
					c.want, c.before, st, access, c.want)
			}
		}
		setAccess(t, "MULTI_USER")
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
		files, err := srv.BackupFileList(ctx, 0, DiskTarget(device))
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
	// t.Cleanup, not defer: the file removals below are cleanups that
	// run after the drop, which releases a STANDBY undo file.
	t.Cleanup(done)

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
		t.Cleanup(func() { liveRemoveFile(t, db, f) })
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
