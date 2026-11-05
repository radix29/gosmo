//go:build livedb

package gosmo

import (
	"context"
	"database/sql"
	"strings"
	"testing"
)

// liveRemoveFile deletes a file a live test made on the server — a backup
// device, a RESTORE ... STANDBY undo file, an event_file target — and checks
// it is gone. T-SQL has no file delete of its own, so it takes what the server
// offers without changing its configuration:
//
//   - sys.xp_delete_files (2019 and later, Linux included) deletes any file;
//   - sys.xp_delete_file 0, on every supported major, deletes a backup file
//     only (it reads the header and refuses anything else);
//   - xp_cmdshell's del, where it is already enabled, for a non-backup file
//     on 2016 and 2017.
//
// A file still there afterwards fails the test, naming it — except a
// non-backup file on a 2016 or 2017 server with xp_cmdshell off, which has no
// way to delete it, and is logged by name instead. A file that was never
// written (the test failed before its BACKUP) is not an error.
//
// Before 2026-11-04 these tests called xp_delete_files alone and ignored its
// error, so every run on 13 and 14 left its .bak/.trn files in the default
// data folder.
func liveRemoveFile(t *testing.T, db *sql.DB, file string) {
	t.Helper()
	ctx := context.Background()
	var deleteFiles, cmdshell bool
	if err := db.QueryRowContext(ctx, `
SELECT CAST(CASE WHEN OBJECT_ID(N'master.sys.xp_delete_files') IS NULL THEN 0 ELSE 1 END AS bit),
       CAST(ISNULL((SELECT value_in_use FROM sys.configurations WHERE name = N'xp_cmdshell'), 0) AS bit)`).
		Scan(&deleteFiles, &cmdshell); err != nil {
		t.Errorf("cleanup: %s: %v (remove it by hand)", file, err)
		return
	}
	var err error
	notBackup := false
	if deleteFiles {
		_, err = db.ExecContext(ctx, "EXEC master.sys.xp_delete_files @p1", file)
	} else {
		_, err = db.ExecContext(ctx, "EXEC master.sys.xp_delete_file 0, @p1", file)
		notBackup = err != nil && strings.Contains(err.Error(), "not a SQL Server backup file")
		if notBackup && cmdshell {
			_, err = db.ExecContext(ctx, `DECLARE @c nvarchar(4000) = N'del /q "' + @p1 + N'"'; EXEC master.sys.xp_cmdshell @c, no_output`, file)
		}
	}

	// sys.dm_os_file_exists arrived with xp_delete_files (2019); before it,
	// the undocumented xp_fileexist is the only check.
	var exists bool
	var xerr error
	if deleteFiles {
		xerr = db.QueryRowContext(ctx, `SELECT file_exists FROM sys.dm_os_file_exists(@p1)`, file).Scan(&exists)
	} else {
		xerr = db.QueryRowContext(ctx, `DECLARE @e int; EXEC master.dbo.xp_fileexist @p1, @e OUTPUT; SELECT CAST(@e AS bit)`, file).Scan(&exists)
	}
	switch {
	case xerr != nil:
		t.Errorf("cleanup: is %s gone? %v (delete: %v)", file, xerr, err)
	case !exists:
	case notBackup && !cmdshell:
		t.Logf("cleanup: %s left behind: not a backup file, and this server has neither xp_delete_files nor xp_cmdshell", file)
	default:
		t.Errorf("cleanup: %s left behind: %v", file, err)
	}
}
