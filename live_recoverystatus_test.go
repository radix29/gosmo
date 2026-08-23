//go:build livedb

// Live verification of the log backup chain reads — Server
// .DatabaseRecoveryStatusesContext and Database.RecoveryStatusContext.
//
// The state they report is what SQL Server itself tests before letting a
// database join an availability group, and the three transitions below are the
// ones that decide it. Verified against the AG test cluster on 2026-08-23 by
// actually running ALTER AVAILABILITY GROUP ... ADD DATABASE in each state:
// no backup and after a SIMPLE round trip both fail with Msg 1475, while a
// backed-up database succeeds even with its msdb history deleted. That last
// case is why this reads sys.database_recovery_status and not
// msdb.dbo.backupset: the history is wrong in both directions.
//
//	go test -tags livedb . -run TestLiveRecoveryStatus -v \
//	  -livedb 'sqlserver://sa:PASS@host?TrustServerCertificate=true'
//
// Creates and drops its own throwaway database; touches nothing else.
package gosmo

import (
	"context"
	"path"
	"strings"
	"testing"
)

// liveBackupPath puts the throwaway backup beside the server's own data
// files, the one directory the service account is certain to be able to write
// on either platform.
func liveBackupPath(t *testing.T, srv *Server, ctx context.Context, file string) string {
	t.Helper()
	var dir string
	if err := srv.queryRowScan(ctx, `SELECT CONVERT(nvarchar(4000), SERVERPROPERTY('InstanceDefaultDataPath'))`, nil, &dir); err != nil {
		t.Fatalf("data path: %v", err)
	}
	if dir == "" {
		t.Skip("server did not report a default data path")
	}
	if strings.Contains(dir, "\\") {
		return strings.TrimSuffix(dir, "\\") + "\\" + file
	}
	return path.Join(dir, file)
}

func TestLiveRecoveryStatusTracksTheLogBackupChain(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()

	const name = "gosmo_recoverystatus_live"
	d, drop := liveScratchDB(t, db, ctx, name)
	defer drop()
	srv := &Server{db: db}

	statusOf := func(t *testing.T, what string) *DatabaseRecoveryStatus {
		t.Helper()
		one, err := d.RecoveryStatusContext(ctx)
		if err != nil {
			t.Fatalf("%s: RecoveryStatusContext: %v", what, err)
		}
		all, err := srv.DatabaseRecoveryStatusesContext(ctx)
		if err != nil {
			t.Fatalf("%s: DatabaseRecoveryStatusesContext: %v", what, err)
		}
		// The two forms must agree, or a caller that reads the whole server
		// (the Add Database dialog) and one that reads a single database
		// disagree about the same database.
		var listed *DatabaseRecoveryStatus
		for _, st := range all {
			if strings.EqualFold(st.DatabaseName, name) {
				listed = st
			}
		}
		if listed == nil {
			t.Fatalf("%s: %s missing from the server-wide listing of %d databases", what, name, len(all))
		}
		if listed.LastLogBackupLSN != one.LastLogBackupLSN || listed.LogBackupChainStarted != one.LogBackupChainStarted {
			t.Errorf("%s: listing says %+v, by-name says %+v", what, listed, one)
		}
		return one
	}

	// A database created in SIMPLE and switched to FULL has no log chain: it
	// is running pseudo-simple until the first full backup.
	if _, err := db.ExecContext(ctx, "ALTER DATABASE ["+name+"] SET RECOVERY FULL"); err != nil {
		t.Fatalf("set recovery full: %v", err)
	}
	if st := statusOf(t, "before any backup"); st.LogBackupChainStarted {
		t.Errorf("chain reported started before any backup, LSN %q", st.LastLogBackupLSN)
	}

	device := liveBackupPath(t, srv, ctx, name+".bak")
	if err := srv.BackupContext(ctx, BackupOptions{Database: name, Devices: []string{device}, Init: true}); err != nil {
		t.Fatalf("backup: %v", err)
	}
	defer db.ExecContext(context.Background(), "EXEC master.dbo.xp_delete_files @FilePath = N'"+device+"'")
	defer db.ExecContext(context.Background(), "EXEC msdb.dbo.sp_delete_database_backuphistory @database_name = N'"+name+"'")

	after := statusOf(t, "after a full backup")
	if !after.LogBackupChainStarted {
		t.Fatalf("chain still reported unstarted after a full backup")
	}
	if after.LastLogBackupLSN == "" {
		t.Errorf("LogBackupChainStarted is true but LastLogBackupLSN is empty")
	}

	// Deleting the msdb history must not change the answer — the state lives
	// in the database, not in the history. This is the case that makes
	// msdb.dbo.backupset the wrong source: SQL Server still accepts such a
	// database into an availability group.
	if _, err := db.ExecContext(ctx, "EXEC msdb.dbo.sp_delete_database_backuphistory @database_name = N'"+name+"'"); err != nil {
		t.Fatalf("delete backup history: %v", err)
	}
	if st := statusOf(t, "after deleting the backup history"); !st.LogBackupChainStarted {
		t.Errorf("deleting msdb history reported the chain as unstarted")
	}

	// A round trip through SIMPLE breaks the chain again, and the history
	// still shows the full backup — the other direction the history gets
	// wrong.
	for _, stmt := range []string{"SET RECOVERY SIMPLE", "SET RECOVERY FULL"} {
		if _, err := db.ExecContext(ctx, "ALTER DATABASE ["+name+"] "+stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	if st := statusOf(t, "after a SIMPLE round trip"); st.LogBackupChainStarted {
		t.Errorf("chain reported started after a SIMPLE round trip, LSN %q", st.LastLogBackupLSN)
	}
}
