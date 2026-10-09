//go:build livedb

// Live verification of BackupSetType: every set type a backup can produce is
// named, from RESTORE HEADERONLY and from msdb alike, and a log set restores
// through FromHeader as RESTORE LOG (it used to be sent as RESTORE DATABASE,
// which the server refuses).
//
//	go test -tags livedb . -run TestLiveBackupSetTypes -v \
//	  -livedb 'sqlserver://sa:PASS@host?TrustServerCertificate=true'
//
// Creates and drops its own databases, backup file and backup history;
// touches nothing else.
package gosmo

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestLiveBackupSetTypes(t *testing.T) {
	db, ctx, done := liveDBTimeout(t, 3*time.Minute)
	defer done()

	const name, copyName = "gosmo_settypes_live", "gosmo_settypes_live_copy"
	_, drop := liveScratchDB(t, db, ctx, name)
	defer drop()
	_, dropCopy := liveScratchDB(t, db, ctx, copyName)
	dropCopy() // only its cleanup is wanted: the restore creates it
	defer dropCopy()
	srv, err := NewServer(ctx, db)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	if srv.refusesSingleUser() {
		t.Skip("a Managed Instance restores FROM URL only and takes no partial or file backups")
	}
	clearHistory := func(c context.Context) {
		db.ExecContext(c, "EXEC msdb.dbo.sp_delete_database_backuphistory @database_name = N'"+name+"'")
	}
	clearHistory(ctx)
	defer clearHistory(context.Background())
	if _, err := db.ExecContext(ctx, "ALTER DATABASE ["+name+"] SET RECOVERY FULL"); err != nil {
		t.Fatalf("set recovery full: %v", err)
	}

	device := liveBackupPath(t, srv, ctx, name+".bak")
	defer db.ExecContext(context.Background(), "EXEC master.dbo.xp_delete_files @FilePath = N'"+device+"'")
	to := " TO DISK = N'" + device + "' WITH "
	sets := []struct {
		stmt     string
		want     BackupSetType
		copyOnly bool
	}{
		{"BACKUP DATABASE [" + name + "]" + to + "INIT", BackupSetDatabase, false},
		{"BACKUP LOG [" + name + "]" + to + "NOINIT", BackupSetLog, false},
		{"BACKUP DATABASE [" + name + "]" + to + "NOINIT, DIFFERENTIAL", BackupSetDifferential, false},
		{"BACKUP DATABASE [" + name + "] READ_WRITE_FILEGROUPS" + to + "NOINIT", BackupSetPartial, false},
		{"BACKUP DATABASE [" + name + "] READ_WRITE_FILEGROUPS" + to + "NOINIT, DIFFERENTIAL", BackupSetDifferentialPartial, false},
		{"BACKUP DATABASE [" + name + "] FILEGROUP = N'PRIMARY'" + to + "NOINIT", BackupSetFile, false},
		{"BACKUP DATABASE [" + name + "] FILEGROUP = N'PRIMARY'" + to + "NOINIT, DIFFERENTIAL", BackupSetDifferentialFile, false},
		{"BACKUP DATABASE [" + name + "]" + to + "NOINIT, COPY_ONLY", BackupSetDatabase, true},
	}
	for i, s := range sets {
		if _, err := db.ExecContext(ctx, s.stmt); err != nil {
			t.Fatalf("set %d (%s): %v", i+1, s.want, err)
		}
	}
	targets := []BackupTarget{DiskTarget(device)}

	headers, err := srv.BackupHeaders(ctx, targets...)
	if err != nil {
		t.Fatalf("BackupHeaders: %v", err)
	}
	if len(headers) != len(sets) {
		t.Fatalf("%d sets on the device, want %d", len(headers), len(sets))
	}
	for i, s := range sets {
		if h := headers[i]; h.SetType != s.want || h.IsCopyOnly != s.copyOnly {
			t.Errorf("header %d = %q copy-only %v, want %q copy-only %v", i+1, h.SetType, h.IsCopyOnly, s.want, s.copyOnly)
		}
	}

	history, err := srv.BackupHistory(ctx, name)
	if err != nil {
		t.Fatalf("BackupHistory: %v", err)
	}
	if len(history) != len(sets) {
		t.Fatalf("%d history entries, want %d", len(history), len(sets))
	}
	for _, b := range history { // newest first: match on the position instead
		s := sets[b.Position-1]
		if b.SetType != s.want || b.IsCopyOnly != s.copyOnly {
			t.Errorf("history set %d = %q copy-only %v, want %q copy-only %v", b.Position, b.SetType, b.IsCopyOnly, s.want, s.copyOnly)
		}
	}

	// The full set under a new name WITH NORECOVERY, then the log set on top
	// of it: FromHeader must make the second a RESTORE LOG with no MOVEs.
	paths, err := srv.DefaultPaths(ctx)
	if err != nil {
		t.Fatalf("DefaultPaths: %v", err)
	}
	reloc := RestoreRelocation{DefaultDataDir: paths.Data, DefaultLogDir: paths.Log}
	full, log := headers[0], headers[1]
	files, err := srv.BackupFileList(ctx, full.SetNumber(), targets...)
	if err != nil {
		t.Fatalf("BackupFileList: %v", err)
	}
	opts := RestoreOptions{Database: copyName, Devices: targets, Recovery: RestoreWithNoRecovery}
	opts.FromHeader(full, files, reloc)
	if err := srv.Restore(ctx, opts); err != nil {
		t.Fatalf("restore the full set as %s: %v", copyName, err)
	}
	opts = RestoreOptions{Database: copyName, Devices: targets, Recovery: RestoreWithRecovery}
	opts.FromHeader(log, nil, reloc)
	stmt, err := srv.BuildRestoreStatement(opts)
	if err != nil {
		t.Fatalf("BuildRestoreStatement: %v", err)
	}
	t.Logf("%s", stmt)
	if !strings.HasPrefix(stmt, "RESTORE LOG ") || strings.Contains(stmt, "MOVE") {
		t.Errorf("the log set's statement is not a RESTORE LOG without MOVEs:\n%s", stmt)
	}
	if err := srv.Restore(ctx, opts); err != nil {
		t.Fatalf("restore the log set onto %s: %v", copyName, err)
	}
	var state string
	if err := db.QueryRowContext(ctx, "SELECT state_desc FROM sys.databases WHERE name = @p1", copyName).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "ONLINE" {
		t.Errorf("%s is %s after the log restore WITH RECOVERY, want ONLINE", copyName, state)
	}
}
