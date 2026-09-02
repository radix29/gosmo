//go:build livedb

// Live verification of the backup device path: that sp_addumpdevice and
// sp_dropdevice as gosmo builds them are accepted, that the device reads back
// through both BackupDevicesContext and BackupDeviceByNameContext, and — the
// point of the BackupTarget change — that the RESTORE-side reads can address a
// logical device rather than only a path.
//
//	go test -tags livedb . -run TestLiveBackupDevice -v \
//	  -livedb 'sqlserver://sa:PASS@host?TrustServerCertificate=true'
//
// Creates and drops its own throwaway device and the backup file behind it;
// touches nothing else. The backup it takes is a COPY_ONLY backup of master,
// which breaks no log chain.
package gosmo

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

const liveBackupDeviceName = "gossms_plan_dev_test"

func TestLiveBackupDeviceCreateReadDrop(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()
	s, err := NewServer(ctx, db)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	// The device's file goes next to master's own backup directory so the
	// service account can certainly write it.
	dir := s.Info().DefaultBackupPath
	if dir == "" {
		t.Fatal("the server reports no default backup path")
	}
	sep := "\\"
	if strings.HasPrefix(dir, "/") {
		sep = "/"
	}
	path := strings.TrimRight(dir, sep) + sep + liveBackupDeviceName + ".bak"

	cleanup := func() {
		if d, err := s.BackupDeviceByNameContext(ctx, liveBackupDeviceName); err == nil {
			// @delfile deletes the .bak too, which is what leaves the server
			// as it was found — and exercises the branch.
			if err := d.DropContext(ctx, true); err != nil {
				t.Logf("cleanup of backup device %q: %v", liveBackupDeviceName, err)
			}
		}
	}
	cleanup()
	defer cleanup()

	d, err := s.CreateBackupDeviceContext(ctx, liveBackupDeviceName, BackupDeviceDisk, path)
	if err != nil {
		t.Fatalf("CreateBackupDeviceContext: %v", err)
	}
	if d.PhysicalName != path {
		t.Errorf("created device reads back physical name %q, want %q", d.PhysicalName, path)
	}
	if d.Type != "DISK" {
		t.Errorf("created device reports type %q, want DISK", d.Type)
	}

	var listed *BackupDevice
	all, err := s.BackupDevicesContext(ctx)
	if err != nil {
		t.Fatalf("BackupDevicesContext: %v", err)
	}
	for _, got := range all {
		if got.Name == liveBackupDeviceName {
			listed = got
		}
	}
	if listed == nil {
		t.Fatalf("the new device is not in BackupDevicesContext's %d rows", len(all))
	}
	if listed.PhysicalName != d.PhysicalName || listed.Type != d.Type {
		t.Errorf("listing has %+v, by-name read has %+v", listed, d)
	}

	// Write a backup set onto the device, addressing it by its logical name —
	// which is also the form BACKUP takes.
	if _, err := db.ExecContext(ctx, "BACKUP DATABASE master TO "+quoteIdent(liveBackupDeviceName)+
		" WITH INIT, COPY_ONLY, NAME = N'gosmo live device probe'"); err != nil {
		t.Fatalf("BACKUP to the device: %v", err)
	}

	// This is what the BackupTarget change exists for: before it, the only way
	// to read a device's contents was by path, and a logical device has none
	// the caller is meant to know.
	headers, err := d.HeadersContext(ctx)
	if err != nil {
		t.Fatalf("HeadersContext on the device: %v", err)
	}
	if len(headers) != 1 {
		t.Fatalf("device holds %d backup sets, want 1", len(headers))
	}
	if headers[0].DatabaseName != "master" || headers[0].BackupName != "gosmo live device probe" {
		t.Errorf("header reads %+v, want the master backup just taken", headers[0])
	}

	files, err := s.BackupFileListForSetFromContext(ctx, d.Target(), headers[0].Position)
	if err != nil {
		t.Fatalf("BackupFileListForSetFromContext on the device: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("the device's file list came back empty")
	}
	if filepath.Base(files[0].PhysicalName) == "" {
		t.Errorf("file list has no physical name: %+v", files[0])
	}
	if err := s.VerifyBackupFromContext(ctx, d.Target()); err != nil {
		t.Fatalf("VerifyBackupFromContext on the device: %v", err)
	}

	// A drop that keeps the file must leave the file behind — the alias goes,
	// the backup does not.
	if err := d.DropContext(ctx, false); err != nil {
		t.Fatalf("DropContext(false): %v", err)
	}
	if _, err := s.BackupDeviceByNameContext(ctx, liveBackupDeviceName); !errors.Is(err, ErrNotFound) {
		t.Errorf("after the drop, the by-name read returned %v, want ErrNotFound", err)
	}
	// The file is still readable by path, which proves @delfile was not sent.
	if _, err := s.BackupHeadersContext(ctx, path); err != nil {
		t.Errorf("a drop without @delfile took the backup file with it: %v", err)
	}

	// Re-add it so the deferred cleanup's @delfile branch runs and removes the
	// file this test wrote.
	if _, err := s.CreateBackupDeviceContext(ctx, liveBackupDeviceName, BackupDeviceDisk, path); err != nil {
		t.Fatalf("re-create for cleanup: %v", err)
	}
}

// The generated script has to be something SQL Server actually accepts.
func TestLiveBackupDeviceScriptRunsAsGenerated(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()
	s, err := NewServer(ctx, db)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	const name = liveBackupDeviceName + "_script"
	drop := func() {
		if d, err := s.BackupDeviceByNameContext(ctx, name); err == nil {
			d.DropContext(ctx, false)
		}
	}
	drop()
	defer drop()

	dir := s.Info().DefaultBackupPath
	if dir == "" {
		t.Fatal("the server reports no default backup path")
	}
	sep := "\\"
	if strings.HasPrefix(dir, "/") {
		sep = "/"
	}
	path := strings.TrimRight(dir, sep) + sep + name + ".bak"

	src, err := s.CreateBackupDeviceContext(ctx, name, BackupDeviceDisk, path)
	if err != nil {
		t.Fatalf("CreateBackupDeviceContext: %v", err)
	}
	script := buildBackupDeviceScript(src, ScriptOptions{Verb: ScriptDropAndCreate})
	if err := src.DropContext(ctx, false); err != nil {
		t.Fatalf("DropContext before replay: %v", err)
	}

	for _, batch := range strings.Split(script, "\nGO\n") {
		if strings.TrimSpace(batch) == "" {
			continue
		}
		if _, err := db.ExecContext(ctx, batch); err != nil {
			t.Fatalf("replaying the generated script failed: %v\n%s", err, batch)
		}
	}
	back, err := s.BackupDeviceByNameContext(ctx, name)
	if err != nil {
		t.Fatalf("read back the scripted device: %v", err)
	}
	if back.PhysicalName != path || back.Type != "DISK" {
		t.Errorf("scripted device reads %+v, want %q DISK", back, path)
	}
}
