//go:build livedb

// Live verification of the restore plan (restore_plan.go): WITH FILE and the
// MOVE clauses name one set. The device holds two sets of one database, and
// the second has a data file the first lacks — so MOVE clauses read from the
// wrong set, or WITH FILE left off, fail the RESTORE or restore the wrong
// backup, where a single-set device would pass either way.
//
//	go test -tags livedb . -run TestLiveRestorePlan -v \
//	  -livedb 'sqlserver://sa:PASS@host?TrustServerCertificate=true'
//
// Creates and drops its own databases and backup file; touches nothing else.
package gosmo

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestLiveRestorePlanWithFileAndMove(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()

	const name, copyName = "gosmo_restore_plan_live", "gosmo_restore_plan_live_copy"
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
		t.Skip("a Managed Instance restores FROM URL only")
	}

	device := liveBackupPath(t, srv, ctx, name+".bak")
	defer db.ExecContext(context.Background(), "EXEC master.dbo.xp_delete_files @FilePath = N'"+device+"'")
	defer db.ExecContext(context.Background(), "EXEC msdb.dbo.sp_delete_database_backuphistory @database_name = N'"+name+"'")
	targets := []BackupTarget{DiskTarget(device)}

	// Set 1: the database as created. Set 2, appended: with an extra file.
	if err := srv.Backup(ctx, BackupOptions{Database: name, Devices: targets, Init: true}); err != nil {
		t.Fatalf("backup set 1: %v", err)
	}
	paths, err := srv.DefaultPaths(ctx)
	if err != nil {
		t.Fatalf("DefaultPaths: %v", err)
	}
	extra := JoinServerPath(paths.Data, name+"_extra.ndf")
	if _, err := db.ExecContext(ctx, "ALTER DATABASE ["+name+"] ADD FILE (NAME = N'extra', FILENAME = N'"+extra+"')"); err != nil {
		t.Fatalf("add file: %v", err)
	}
	if err := srv.Backup(ctx, BackupOptions{Database: name, Devices: targets}); err != nil {
		t.Fatalf("backup set 2: %v", err)
	}

	headers, err := srv.BackupHeaders(ctx, targets...)
	if err != nil {
		t.Fatalf("BackupHeaders: %v", err)
	}
	if len(headers) != 2 {
		t.Fatalf("%d sets on the device, want 2", len(headers))
	}
	h := BackupSetAt(headers, 2)
	if h == nil || h.SetNumber() != 2 {
		t.Fatalf("BackupSetAt(2) = %+v", h)
	}
	if BackupSetAt(headers, 3) != nil {
		t.Error("BackupSetAt found a set 3 the device does not hold")
	}
	files, err := srv.BackupFileList(ctx, h.SetNumber(), targets...)
	if err != nil {
		t.Fatalf("BackupFileList: %v", err)
	}
	if len(files) != 3 {
		t.Fatalf("set 2 lists %d files, want 3 (data, log, extra)", len(files))
	}

	reloc := RestoreRelocation{DefaultDataDir: paths.Data, DefaultLogDir: paths.Log}
	if !reloc.NeedsFileList(h.DatabaseName, copyName) {
		t.Fatal("a renamed restore does not need the file list")
	}
	opts := RestoreOptions{Database: copyName, Devices: targets, Recovery: RestoreWithRecovery}
	opts.FromHeader(h, files, reloc)
	stmt, err := srv.BuildRestoreStatement(opts)
	if err != nil {
		t.Fatalf("BuildRestoreStatement: %v", err)
	}
	t.Logf("%s", stmt)
	if !strings.Contains(stmt, "FILE = 2") || strings.Count(stmt, "MOVE N'") != 3 {
		t.Errorf("statement lacks FILE = 2 or three MOVEs:\n%s", stmt)
	}
	if err := srv.Restore(ctx, opts); err != nil {
		t.Fatalf("restore set 2 as %s: %v", copyName, err)
	}

	// The copy is set 2 (it has the extra file), at the planned paths.
	rows, err := db.QueryContext(ctx, "SELECT name, physical_name FROM sys.master_files WHERE database_id = DB_ID(@p1)", copyName)
	if err != nil {
		t.Fatalf("read the copy's files: %v", err)
	}
	got := map[string]string{}
	for rows.Next() {
		var n, p string
		if err := rows.Scan(&n, &p); err != nil {
			t.Fatal(err)
		}
		got[n] = p
	}
	rows.Close()
	for _, m := range opts.RelocateFiles {
		if !strings.EqualFold(got[m.LogicalName], m.PhysicalName) {
			t.Errorf("file %s is at %q, planned %q", m.LogicalName, got[m.LogicalName], m.PhysicalName)
		}
	}
	if _, ok := got["extra"]; !ok {
		t.Errorf("the copy has no extra file — set 1 was restored: %v", got)
	}

	// T49 on the same database: a TableRef read refuses, the read-back table
	// answers.
	cdb := srv.DatabaseRef(copyName)
	if _, err := db.ExecContext(ctx, "USE ["+copyName+"]; CREATE TABLE dbo.t (a int); USE master"); err != nil {
		t.Fatalf("create table: %v", err)
	}
	if _, err := cdb.TableRef("dbo", "t").Columns(ctx); !errors.Is(err, ErrHandleNotLoaded) {
		t.Errorf("TableRef Columns: err = %v, want ErrHandleNotLoaded", err)
	}
	tbl, err := cdb.TableByName(ctx, "dbo", "t")
	if err != nil {
		t.Fatalf("TableByName: %v", err)
	}
	if cols, err := tbl.Columns(ctx); err != nil || len(cols) != 1 {
		t.Errorf("loaded table Columns = %d, %v; want 1 column", len(cols), err)
	}
	ix, err := cdb.TableRef("dbo", "t").CreateIndex(ctx, CreateIndexRequest{Name: "ix_a", KeyColumns: []IndexColumnDef{{Name: "a"}}})
	if err != nil || ix.Name != "ix_a" {
		t.Errorf("CreateIndex on a TableRef = %+v, %v; want the handle", ix, err)
	}
	if cols, err := cdb.TableRef("dbo", "t").StatisticRef("ix_a").Columns(ctx); err != nil || len(cols) != 1 || cols[0] != "a" {
		t.Errorf("StatisticRef Columns on a TableRef = %v, %v; want [a]", cols, err)
	}
}

// TestLiveRestorePlanFilestreamContainer: a database restored under a new
// name gets its FILESTREAM container at "<target>_<logical>" — a directory
// with no data file's extension — and the RESTORE accepts that path.
func TestLiveRestorePlanFilestreamContainer(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()
	var fsLevel int
	if err := db.QueryRowContext(ctx, "SELECT CONVERT(int, ISNULL(SERVERPROPERTY('FilestreamEffectiveLevel'), 0))").Scan(&fsLevel); err != nil {
		t.Fatal(err)
	}
	if fsLevel < 2 {
		t.Skip("FILESTREAM is not enabled for file I/O on this instance")
	}

	const name, copyName = "gosmo_restore_fs_live", "gosmo_restore_fs_live_copy"
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
		t.Skip("a Managed Instance restores FROM URL only")
	}
	paths, err := srv.DefaultPaths(ctx)
	if err != nil {
		t.Fatalf("DefaultPaths: %v", err)
	}
	for _, s := range []string{
		"ALTER DATABASE [" + name + "] ADD FILEGROUP FSG CONTAINS FILESTREAM",
		"ALTER DATABASE [" + name + "] ADD FILE (NAME = N'fs', FILENAME = N'" + JoinServerPath(paths.Data, name+"_fs") + "') TO FILEGROUP FSG",
	} {
		if _, err := db.ExecContext(ctx, s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}

	device := liveBackupPath(t, srv, ctx, name+".bak")
	defer db.ExecContext(context.Background(), "EXEC master.dbo.xp_delete_files @FilePath = N'"+device+"'")
	defer db.ExecContext(context.Background(), "EXEC msdb.dbo.sp_delete_database_backuphistory @database_name = N'"+name+"'")
	targets := []BackupTarget{DiskTarget(device)}
	if err := srv.Backup(ctx, BackupOptions{Database: name, Devices: targets, Init: true}); err != nil {
		t.Fatalf("backup: %v", err)
	}
	headers, err := srv.BackupHeaders(ctx, targets...)
	if err != nil || len(headers) != 1 {
		t.Fatalf("BackupHeaders: %d sets, %v", len(headers), err)
	}
	files, err := srv.BackupFileList(ctx, headers[0].SetNumber(), targets...)
	if err != nil {
		t.Fatalf("BackupFileList: %v", err)
	}
	opts := RestoreOptions{Database: copyName, Devices: targets, Recovery: RestoreWithRecovery}
	opts.FromHeader(headers[0], files, RestoreRelocation{DefaultDataDir: paths.Data, DefaultLogDir: paths.Log})
	want := JoinServerPath(paths.Data, copyName+"_fs")
	planned := false
	for _, m := range opts.RelocateFiles {
		if m.LogicalName == "fs" {
			planned = true
			if m.PhysicalName != want {
				t.Errorf("container planned at %q, want %q", m.PhysicalName, want)
			}
		}
	}
	if !planned {
		t.Fatalf("no MOVE for the container: %+v", opts.RelocateFiles)
	}
	if err := srv.Restore(ctx, opts); err != nil {
		t.Fatalf("restore as %s: %v", copyName, err)
	}
	var got string
	if err := db.QueryRowContext(ctx, "SELECT physical_name FROM sys.master_files WHERE database_id = DB_ID(@p1) AND name = N'fs'", copyName).Scan(&got); err != nil {
		t.Fatalf("read the copy's container: %v", err)
	}
	if !strings.EqualFold(got, want) {
		t.Errorf("the copy's container is at %q, want %q", got, want)
	}
}
