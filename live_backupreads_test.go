//go:build livedb

// Live coverage for the four backup *reads*: BackupHeaders, BackupFileList,
// BackupFileList for a later set, and BackupHistory — plus all three RESTORE
// reads and the history of a striped backup.
//
// All four are reads — RESTORE HEADERONLY / FILELISTONLY and an msdb history
// query — so WithScript cannot capture them and no unit test can reach them:
// the result sets are the server's own, with column sets that vary by
// version, which is exactly what newNamedRow exists to absorb. Only a real
// backup file settles what these return.
//
//	go test -tags livedb . -run TestLiveBackupReads -v \
//	  -livedb 'sqlserver://sa:PASS@host?TrustServerCertificate=true'
//
// Creates two throwaway databases and one backup device beside their data
// files, and drops all three.
package gosmo

import (
	"context"
	"database/sql"
	"strings"
	"testing"
)

// liveBackupDevice backs first and second up to one device — first twice, so
// the device holds three sets and no assertion below can pass by reading
// whichever set happens to come first. Returns the device path and a
// best-effort remover.
func liveBackupDevice(t *testing.T, db *sql.DB, ctx context.Context, dir string, first, second *Database) (string, func()) {
	t.Helper()
	device := dir + "gosmo_backupreads.bak"
	// msdb's backup history outlives the database it describes — DROP
	// DATABASE leaves the backupset rows behind — so an earlier run of this
	// test is still in it under the same names, and the history assertion
	// below would count both runs. Clearing it is what makes the test
	// repeatable rather than passing once.
	for _, d := range []*Database{first, second} {
		if _, err := db.ExecContext(ctx,
			`EXEC msdb.dbo.sp_delete_database_backuphistory @database_name = @p1`, d.Name); err != nil {
			t.Fatalf("clear backup history for %s: %v", d.Name, err)
		}
	}
	// INIT on the first write, so a device left behind by an earlier run is
	// overwritten rather than appended to and counted twice.
	for i, stmt := range []string{
		`BACKUP DATABASE [` + first.Name + `] TO DISK = N'` + device + `' WITH INIT, NAME = N'set one'`,
		`BACKUP DATABASE [` + second.Name + `] TO DISK = N'` + device + `' WITH NOINIT, NAME = N'set two'`,
		`BACKUP LOG [` + first.Name + `] TO DISK = N'` + device + `' WITH NOINIT, NAME = N'set three'`,
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("backup %d: %v", i+1, err)
		}
	}
	return device, func() {
		c := context.Background()
		// xp_delete_files is best effort: the file is named and lives beside
		// the test databases' own files, so a server without it leaves one
		// obvious artifact rather than a mystery.
		db.ExecContext(c, `EXEC master.sys.xp_delete_files N'`+device+`'`)
		// And take this run's rows back out of msdb, so the server is left
		// as it was found.
		for _, d := range []*Database{first, second} {
			db.ExecContext(c, `EXEC msdb.dbo.sp_delete_database_backuphistory @database_name = @p1`, d.Name)
		}
	}
}

func TestLiveBackupReads(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()

	first, dropFirst := liveScratchDB(t, db, ctx, "gosmo_backupreads_one")
	defer dropFirst()
	second, dropSecond := liveScratchDB(t, db, ctx, "gosmo_backupreads_two")
	defer dropSecond()

	// A full backup needs a recovery model that has one; the scratch
	// databases inherit model's, which is usually SIMPLE — the log backup
	// below needs FULL.
	if _, err := db.ExecContext(ctx, `ALTER DATABASE [`+first.Name+`] SET RECOVERY FULL`); err != nil {
		t.Fatalf("set recovery full: %v", err)
	}
	device, remove := liveBackupDevice(t, db, ctx, liveDatabaseFileDir(t, first, ctx), first, second)
	defer remove()

	srv := first.server

	t.Run("headers", func(t *testing.T) {
		headers, err := srv.BackupHeaders(ctx, DiskTarget(device))
		if err != nil {
			t.Fatalf("BackupHeaders: %v", err)
		}
		if len(headers) != 3 {
			t.Fatalf("got %d backup sets, want 3", len(headers))
		}
		for i, want := range []struct {
			name     string
			database string
			action   BackupAction
		}{
			{"set one", first.Name, BackupActionDatabase},
			{"set two", second.Name, BackupActionDatabase},
			{"set three", first.Name, BackupActionLog},
		} {
			h := headers[i]
			if h.Position != i+1 {
				t.Errorf("set %d Position = %d, want %d", i+1, h.Position, i+1)
			}
			if h.BackupName != want.name || h.DatabaseName != want.database {
				t.Errorf("set %d = %q of %q, want %q of %q", i+1,
					h.BackupName, h.DatabaseName, want.name, want.database)
			}
			if h.BackupType != want.action {
				t.Errorf("set %d BackupType = %v, want %v", i+1, h.BackupType, want.action)
			}
			if h.BackupSize <= 0 {
				t.Errorf("set %d BackupSize = %d, want a real size", i+1, h.BackupSize)
			}
			if h.BackupFinish.Before(h.BackupStart) {
				t.Errorf("set %d finished (%v) before it started (%v)", i+1, h.BackupFinish, h.BackupStart)
			}
			if h.ServerName == "" || h.RecoveryModel == "" || h.DatabaseVersion == 0 {
				t.Errorf("set %d lost a header column: %+v", i+1, h)
			}
		}
	})

	t.Run("file list", func(t *testing.T) {
		files, err := srv.BackupFileList(ctx, 0, DiskTarget(device))
		if err != nil {
			t.Fatalf("BackupFileList: %v", err)
		}
		assertBackupFileList(t, files, first.Name)
	})

	// The set the caller asks for, not the first one on the device: with no
	// FILE clause this reads set 1, so a FileListForSet that ignored its
	// argument would still return a plausible answer.
	t.Run("file list for set two", func(t *testing.T) {
		files, err := srv.BackupFileList(ctx, 2, DiskTarget(device))
		if err != nil {
			t.Fatalf("BackupFileList set 2: %v", err)
		}
		assertBackupFileList(t, files, second.Name)
	})

	t.Run("history", func(t *testing.T) {
		history, err := srv.BackupHistory(ctx, first.Name)
		if err != nil {
			t.Fatalf("BackupHistory: %v", err)
		}
		if len(history) != 2 {
			t.Fatalf("got %d history rows for %s, want 2 (the other database's must not be here)",
				len(history), first.Name)
		}
		// Newest first, and the log backup was taken last.
		if history[0].BackupType != BackupActionLog || history[1].BackupType != BackupActionDatabase {
			t.Errorf("history types = %v, %v; want the log backup first (newest) then the full one",
				history[0].BackupType, history[1].BackupType)
		}
		if history[0].BackupFinish.Before(history[1].BackupFinish) {
			t.Errorf("history is oldest-first: %v then %v", history[0].BackupFinish, history[1].BackupFinish)
		}
		for _, b := range history {
			if b.DatabaseName != first.Name {
				t.Errorf("history row for %q, want %q", b.DatabaseName, first.Name)
			}
			if b.DeviceName != device {
				t.Errorf("history DeviceName = %q, want %q", b.DeviceName, device)
			}
			if b.BackupSize <= 0 || b.UserName == "" || b.ServerName == "" {
				t.Errorf("history row lost a column: %+v", b)
			}
		}
		if history[0].BackupSetName != "set three" || history[1].BackupSetName != "set one" {
			t.Errorf("history set names = %q, %q; want \"set three\", \"set one\"",
				history[0].BackupSetName, history[1].BackupSetName)
		}
		// Both sets share one file, so only Position tells a RESTORE which
		// of them to read; without it the server reads set 1, the oldest.
		if history[0].Position != 3 || history[1].Position != 1 {
			t.Errorf("history positions = %d, %d; want 3, 1", history[0].Position, history[1].Position)
		}
	})

	// A backup striped over two files is one set in the history, naming both
	// files in order, and every RESTORE-side read accepts the pair — one
	// stripe alone is refused.
	t.Run("striped", func(t *testing.T) {
		s1, s2 := device+".s1", device+".s2"
		defer db.ExecContext(context.Background(), `EXEC master.sys.xp_delete_files N'`+s1+`'`)
		defer db.ExecContext(context.Background(), `EXEC master.sys.xp_delete_files N'`+s2+`'`)
		if _, err := db.ExecContext(ctx, `BACKUP DATABASE [`+second.Name+`] TO DISK = N'`+s1+`', DISK = N'`+s2+`' WITH INIT, COPY_ONLY`); err != nil {
			t.Fatalf("striped backup: %v", err)
		}
		history, err := srv.BackupHistory(ctx, second.Name)
		if err != nil {
			t.Fatalf("BackupHistory: %v", err)
		}
		var striped *BackupInfo
		for _, b := range history {
			if len(b.Devices) > 1 {
				striped = b
			}
		}
		if striped == nil || len(history) != 2 {
			t.Fatalf("history for %s = %d entries, striped %v; want 2 with one striped set", second.Name, len(history), striped)
		}
		if striped.Devices[0] != s1 || striped.Devices[1] != s2 || striped.Position != 1 {
			t.Errorf("striped set Devices %q Position %d; want [%s %s] at 1", striped.Devices, striped.Position, s1, s2)
		}
		both := []BackupTarget{DiskTarget(s1), DiskTarget(s2)}
		if err := srv.VerifyBackup(ctx, both...); err != nil {
			t.Errorf("VerifyBackup of both stripes: %v", err)
		}
		if err := srv.VerifyBackup(ctx, DiskTarget(s1)); err == nil {
			t.Error("VerifyBackup of one stripe: want the server's refusal")
		}
		if h, err := srv.BackupHeaders(ctx, both...); err != nil || len(h) != 1 {
			t.Errorf("BackupHeaders of both stripes = %d, %v; want one set", len(h), err)
		}
		files, err := srv.BackupFileList(ctx, 0, both...)
		if err != nil {
			t.Fatalf("BackupFileList of both stripes: %v", err)
		}
		assertBackupFileList(t, files, second.Name)
	})

	t.Run("history of a database with none", func(t *testing.T) {
		history, err := srv.BackupHistory(ctx, "gosmo_no_such_database")
		if err != nil {
			t.Fatalf("BackupHistory for an unknown database: %v", err)
		}
		if len(history) != 0 {
			t.Errorf("got %d history rows, want none", len(history))
		}
	})
}

// assertBackupFileList checks one set's file list names the database it was
// taken from: a data file and a log file, with that database's own logical
// names.
func assertBackupFileList(t *testing.T, files []*BackupFile, database string) {
	t.Helper()
	if len(files) != 2 {
		t.Fatalf("got %d files, want a data file and a log file: %+v", len(files), files)
	}
	byType := map[string]*BackupFile{}
	for _, f := range files {
		byType[f.Type] = f
	}
	data, ok := byType["D"]
	if !ok {
		t.Fatalf("no data file in the list: %+v", files)
	}
	logFile, ok := byType["L"]
	if !ok {
		t.Fatalf("no log file in the list: %+v", files)
	}
	if data.LogicalName != database {
		t.Errorf("data file logical name = %q, want %q — this is another set's file list",
			data.LogicalName, database)
	}
	if !strings.HasPrefix(logFile.LogicalName, database) {
		t.Errorf("log file logical name = %q, want it to belong to %q", logFile.LogicalName, database)
	}
	if data.FileGroupName != "PRIMARY" {
		t.Errorf("data file filegroup = %q, want PRIMARY", data.FileGroupName)
	}
	if logFile.FileGroupName != "" {
		t.Errorf("log file filegroup = %q, want empty — a log file is not on a filegroup", logFile.FileGroupName)
	}
	if data.Size <= 0 || data.PhysicalName == "" {
		t.Errorf("data file lost a column: %+v", data)
	}
}
