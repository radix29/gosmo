// Command backup demonstrates gosmo's backup and restore surface: a full
// backup with live progress reporting, reading a backup device's headers and
// file list, verifying it, a differential and a log backup appended to the
// same media, the backup history in msdb, and a restore that relocates the
// database's files.
//
// It works on a throwaway database it creates itself, and drops it at the
// end. The backup device it writes stays on the server — SQL Server has no
// "delete file" verb, so remove it yourself if you care.
//
//	MSSQL_SERVER=localhost:1433 MSSQL_USER=sa MSSQL_PASSWORD=YourPw go run ./examples/backup
package main

import (
	"fmt"
	"time"

	gosmo "github.com/radix29/gosmo"
	"github.com/radix29/gosmo/examples/internal/demo"
)

const dbName = "GoSMOBackupDemo"

func main() {
	// First, so it runs after the cleanup deferred below it.
	defer demo.Exit()

	srv := demo.Connect()
	defer srv.Close()

	db, drop := demo.TempDatabase(srv, dbName)
	defer drop()

	// A log backup needs a database that is not in SIMPLE recovery —
	// TempDatabase creates it SIMPLE, where BACKUP LOG is an error.
	demo.Must(db.SetRecoveryModel(gosmo.RecoveryModelFull))

	demo.Must(db.CreateTable(gosmo.CreateTableRequest{
		Schema: "dbo",
		Name:   "Events",
		Columns: []gosmo.ColumnDefinition{
			{Name: "EventID", DataType: gosmo.DataTypeInt, IsIdentity: true, IdentitySeed: 1, IdentityIncr: 1, IsPrimaryKey: true},
			{Name: "Payload", DataType: gosmo.DataTypeNVarChar, MaxLength: 200, IsNullable: false},
		},
	}))
	rows := func(yield func([]any, error) bool) {
		for i := range 5000 {
			if !yield([]any{fmt.Sprintf("event payload %d", i)}, nil) {
				return
			}
		}
	}
	loaded := demo.Value(db.BulkInsert(gosmo.BulkCopy{
		Schema:  "dbo",
		Table:   "Events",
		Columns: []string{"Payload"},
		Options: gosmo.BulkOptions{TableLock: true},
	}, rows))
	fmt.Printf("Loaded %d rows to give the backup something to copy\n", loaded)

	dir := srv.Info().DefaultBackupPath
	device := demo.ServerPath(dir, dbName+".bak")

	// -- The statement, without running it ---------------------------------
	//
	// BuildBackupStatement is the same builder Server.Backup uses, exported
	// so a caller can show, log, or hand-edit the T-SQL first.
	demo.Section("Generated T-SQL")
	fmt.Println(" ", demo.Value(gosmo.BuildBackupStatement(gosmo.BackupOptions{
		Database:    dbName,
		Devices:     []string{device},
		Checksum:    true,
		Init:        true,
		Stats:       10,
		CopyOnly:    true,
		Compression: new(true),
	})))

	// -- Full backup, with progress ---------------------------------------
	//
	// Progress fires for every message the server emits while the backup
	// runs. Stats controls how often the percent-complete notices come; it
	// defaults to 10 when Progress is set and Stats is left at zero.
	demo.Section("Full backup")
	demo.Must(srv.Backup(gosmo.BackupOptions{
		Database:      dbName,
		Devices:       []string{device},
		BackupSetName: dbName + " full",
		Description:   "gosmo example full backup",
		Checksum:      true,
		Init:          true, // overwrite anything already on the media
		Stats:         25,
		Progress: func(pct int, message string) {
			if pct >= 0 {
				fmt.Printf("  %3d%%\n", pct)
				return
			}
			fmt.Printf("  %s\n", message)
		},
	}))

	// -- Differential, then log, appended to the same device --------------
	demo.Section("Differential and log backups")
	demo.Must(srv.Backup(gosmo.BackupOptions{
		Database:      dbName,
		Action:        gosmo.BackupActionDifferential,
		Devices:       []string{device},
		BackupSetName: dbName + " diff",
	}))
	demo.Must(srv.Backup(gosmo.BackupOptions{
		Database:      dbName,
		Action:        gosmo.BackupActionLog,
		Devices:       []string{device},
		BackupSetName: dbName + " log",
	}))
	fmt.Println("  appended")

	// -- What is on the media ---------------------------------------------
	demo.Section("Backup sets on the device (RESTORE HEADERONLY)")
	for _, h := range demo.Value(srv.BackupHeaders(device)) {
		fmt.Printf("  pos=%d  %-24s type=%-6s size=%.1f MB  %s\n",
			h.Position, h.BackupName, h.BackupType,
			float64(h.BackupSize)/(1024*1024), h.BackupFinish.Format(time.RFC3339))
	}

	demo.Section("Files inside the first set (RESTORE FILELISTONLY)")
	for _, f := range demo.Value(srv.BackupFileList(device)) {
		fmt.Printf("  %-16s %-6s %s\n", f.LogicalName, f.Type, f.PhysicalName)
	}

	demo.Section("Verify (RESTORE VERIFYONLY)")
	demo.Must(srv.VerifyBackup(device))
	fmt.Println("  backup set is readable and complete")

	// -- msdb's backup history --------------------------------------------
	demo.Section("Backup history for " + dbName)
	for _, b := range demo.Value(srv.BackupHistory(dbName)) {
		fmt.Printf("  %-6s %s  %.1f MB  -> %s\n",
			b.BackupType, b.BackupFinish.Format(time.RFC3339),
			float64(b.BackupSize)/(1024*1024), b.DeviceName)
	}

	// -- Restore, relocating the files ------------------------------------
	//
	// FileNumber picks the backup set on the device (1-based, matching
	// BackupHeader.Position). Leaving it zero always restores the first set,
	// so restoring the differential or log appended above needs it set.
	// RelocateFiles is what makes a restore under a new name — or onto a
	// server with different paths — work at all.
	demo.Section("Restore (full set, files relocated)")
	dataDir := srv.Info().DefaultDataPath
	files := demo.Value(srv.BackupFileList(device))
	relocate := make([]gosmo.RelocateFile, 0, len(files))
	for _, f := range files {
		ext := ".mdf"
		if f.Type == "L" {
			ext = ".ldf"
		}
		relocate = append(relocate, gosmo.RelocateFile{
			LogicalName:  f.LogicalName,
			PhysicalName: demo.ServerPath(dataDir, f.LogicalName+"_restored"+ext),
		})
	}
	demo.Must(srv.Restore(gosmo.RestoreOptions{
		Database:      dbName,
		Devices:       []string{device},
		FileNumber:    1,
		RelocateFiles: relocate,
		Replace:       true,
		Recovery:      true,
		Checksum:      true,
		Stats:         50,
		Progress: func(pct int, message string) {
			if pct >= 0 {
				fmt.Printf("  %3d%%\n", pct)
				return
			}
			fmt.Printf("  %s\n", message)
		},
	}))

	restored := demo.Value(srv.DatabaseByName(dbName))
	fmt.Printf("  [%s] is %s again; files now:\n", restored.Name(), restored.State())
	for _, f := range demo.Value(restored.Files()) {
		fmt.Printf("    %-16s %s\n", f.Name, f.PhysicalName)
	}

	// A point-in-time restore would add StopAt and NoRecovery on the full
	// set, then roll the log forward:
	//
	//	stopAt := time.Now().Add(-5 * time.Minute)
	//	srv.Restore(gosmo.RestoreOptions{Database: dbName, Devices: []string{device},
	//		FileNumber: 1, Replace: true, NoRecovery: true})
	//	srv.Restore(gosmo.RestoreOptions{Database: dbName, Devices: []string{device},
	//		FileNumber: 3, Action: gosmo.BackupActionLog, StopAt: &stopAt, Recovery: true})
	demo.Section("Cleanup")
	fmt.Printf("  the backup device %s is left on the server\n", device)
}
