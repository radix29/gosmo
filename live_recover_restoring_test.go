//go:build livedb

// Live verification of Database.RecoverFromRestoring: a database restored
// WITH NORECOVERY is refused SET ONLINE (Msg 5052) and brought online by
// RESTORE ... WITH RECOVERY.
//
//	go test -tags livedb . -run TestLiveRecoverFromRestoring -v \
//	  -livedb 'sqlserver://sa:PASS@host?TrustServerCertificate=true'
//
// Creates and drops its own database and backup file; touches nothing else.
package gosmo

import (
	"context"
	"errors"
	"slices"
	"testing"

	mssql "github.com/microsoft/go-mssqldb"
)

func TestLiveRecoverFromRestoring(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()

	const name = "gosmo_recover_restoring_live"
	_, drop := liveScratchDB(t, db, ctx, name)
	defer drop()
	srv, err := NewServer(ctx, db)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	if srv.refusesSingleUser() {
		t.Skip("a Managed Instance restores FROM URL only")
	}

	device := liveBackupPath(t, srv, ctx, name+".bak")
	if err := srv.Backup(ctx, BackupOptions{Database: name, Devices: []BackupTarget{DiskTarget(device)}, Init: true}); err != nil {
		t.Fatalf("backup: %v", err)
	}
	defer liveRemoveFile(t, db, device)
	defer db.ExecContext(context.Background(), "EXEC msdb.dbo.sp_delete_database_backuphistory @database_name = N'"+name+"'")

	if err := srv.Restore(ctx, RestoreOptions{Database: name, Devices: []BackupTarget{DiskTarget(device)},
		Replace: true, Recovery: RestoreWithNoRecovery}); err != nil {
		t.Fatalf("restore WITH NORECOVERY: %v", err)
	}
	state := func() DatabaseState {
		t.Helper()
		d, err := srv.DatabaseByName(ctx, name)
		if err != nil {
			t.Fatalf("reload %s: %v", name, err)
		}
		return d.State
	}
	if got := state(); got != "RESTORING" {
		t.Fatalf("after RESTORE WITH NORECOVERY the state is %s, want RESTORING", got)
	}

	// What RecoverFromRestoring exists for: SET ONLINE cannot reach it.
	err = srv.DatabaseRef(name).SetOnline(ctx)
	if msErr, ok := errors.AsType[mssql.Error](err); !ok || !slices.ContainsFunc(msErr.All, func(e mssql.Error) bool { return e.Number == 5052 }) {
		t.Errorf("SetOnline on a RESTORING database = %v, want Msg 5052", err)
	}

	d := srv.DatabaseRef(name)
	if err := d.RecoverFromRestoring(ctx); err != nil {
		t.Fatalf("RecoverFromRestoring: %v", err)
	}
	if got := state(); got != "ONLINE" {
		t.Errorf("after RecoverFromRestoring the server reports %s, want ONLINE", got)
	}
	if d.State != "ONLINE" {
		t.Errorf("the handle's State = %q, want ONLINE mirrored", d.State)
	}
}
