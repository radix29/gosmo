//go:build livedb

// Live coverage for the four backup *reads*: BackupHeaders, BackupFileList,
// BackupFileListForSet and BackupHistory.
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
			`EXEC msdb.dbo.sp_delete_database_backuphistory @database_name = @p1`, d.name); err != nil {
			t.Fatalf("clear backup history for %s: %v", d.name, err)
		}
	}
	// INIT on the first write, so a device left behind by an earlier run is
	// overwritten rather than appended to and counted twice.
	for i, stmt := range []string{
		`BACKUP DATABASE [` + first.name + `] TO DISK = N'` + device + `' WITH INIT, NAME = N'set one'`,
		`BACKUP DATABASE [` + second.name + `] TO DISK = N'` + device + `' WITH NOINIT, NAME = N'set two'`,
		`BACKUP LOG [` + first.name + `] TO DISK = N'` + device + `' WITH NOINIT, NAME = N'set three'`,
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
			db.ExecContext(c, `EXEC msdb.dbo.sp_delete_database_backuphistory @database_name = @p1`, d.name)
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
	if _, err := db.ExecContext(ctx, `ALTER DATABASE [`+first.name+`] SET RECOVERY FULL`); err != nil {
		t.Fatalf("set recovery full: %v", err)
	}
	device, remove := liveBackupDevice(t, db, ctx, liveDatabaseFileDir(t, first, ctx), first, second)
	defer remove()

	srv := first.server

	t.Run("headers", func(t *testing.T) {
		headers, err := srv.BackupHeadersContext(ctx, device)
		if err != nil {
			t.Fatalf("BackupHeadersContext: %v", err)
		}
		if len(headers) != 3 {
			t.Fatalf("got %d backup sets, want 3", len(headers))
		}
		for i, want := range []struct {
			name     string
			database string
			action   BackupAction
		}{
			{"set one", first.name, BackupActionDatabase},
			{"set two", second.name, BackupActionDatabase},
			{"set three", first.name, BackupActionLog},
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
		files, err := srv.BackupFileListContext(ctx, device)
		if err != nil {
			t.Fatalf("BackupFileListContext: %v", err)
		}
		assertBackupFileList(t, files, first.name)
	})

	// The set the caller asks for, not the first one on the device: with no
	// FILE clause this reads set 1, so a FileListForSet that ignored its
	// argument would still return a plausible answer.
	t.Run("file list for set two", func(t *testing.T) {
		files, err := srv.BackupFileListForSetContext(ctx, device, 2)
		if err != nil {
			t.Fatalf("BackupFileListForSetContext: %v", err)
		}
		assertBackupFileList(t, files, second.name)
	})

	t.Run("history", func(t *testing.T) {
		history, err := srv.BackupHistoryContext(ctx, first.name)
		if err != nil {
			t.Fatalf("BackupHistoryContext: %v", err)
		}
		if len(history) != 2 {
			t.Fatalf("got %d history rows for %s, want 2 (the other database's must not be here)",
				len(history), first.name)
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
			if b.DatabaseName != first.name {
				t.Errorf("history row for %q, want %q", b.DatabaseName, first.name)
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
	})

	t.Run("history of a database with none", func(t *testing.T) {
		history, err := srv.BackupHistoryContext(ctx, "gosmo_no_such_database")
		if err != nil {
			t.Fatalf("BackupHistoryContext for an unknown database: %v", err)
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
