//go:build livedb

// Live verification of Server.AvailabilityDatabaseCandidates: each flag the
// AG pickers exclude on reads what sys.databases holds for a database put into
// that state.
//
//	go test -tags livedb . -run TestLiveAvailabilityDatabaseCandidates -v \
//	  -livedb 'sqlserver://sa:PASS@host?TrustServerCertificate=true'
//
// Creates and drops its own throwaway databases; touches nothing else.
package gosmo

import (
	"context"
	"strings"
	"testing"
)

func TestLiveAvailabilityDatabaseCandidates(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()

	const base = "gosmo_agcand_live"
	setups := map[string]string{
		base + "_ro": "SET READ_ONLY",
		base + "_ac": "SET AUTO_CLOSE ON",
		base + "_su": "SET SINGLE_USER",
		base + "_ok": "SET RECOVERY FULL",
	}
	for name, set := range setups {
		_, drop := liveScratchDB(t, db, ctx, name)
		defer drop()
		if _, err := db.ExecContext(ctx, "ALTER DATABASE ["+name+"] "+set); err != nil {
			t.Fatalf("%s %s: %v", name, set, err)
		}
	}
	srv, err := NewServer(ctx, db)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	// A snapshot of the healthy one; its sparse file goes beside the data files.
	const snap = base + "_snap"
	file := liveBackupPath(t, srv, ctx, snap+".ss")
	if _, err := db.ExecContext(ctx, "CREATE DATABASE ["+snap+"] ON (NAME = ["+base+"_ok], FILENAME = N'"+file+"') AS SNAPSHOT OF ["+base+"_ok]"); err != nil {
		t.Fatalf("create snapshot: %v", err)
	}
	defer db.ExecContext(context.Background(), "DROP DATABASE ["+snap+"]")

	// The full backup that starts the healthy database's log chain.
	device := liveBackupPath(t, srv, ctx, base+"_ok.bak")
	if err := srv.Backup(ctx, BackupOptions{Database: base + "_ok", Devices: []BackupTarget{DiskTarget(device)}, Init: true}); err != nil {
		t.Fatalf("backup: %v", err)
	}
	defer liveRemoveFile(t, db, device)
	defer db.ExecContext(context.Background(), "EXEC msdb.dbo.sp_delete_database_backuphistory @database_name = N'"+base+"_ok'")

	all, err := srv.AvailabilityDatabaseCandidates(ctx)
	if err != nil {
		t.Fatalf("AvailabilityDatabaseCandidates: %v", err)
	}
	got := map[string]*AvailabilityDatabaseCandidate{}
	for _, c := range all {
		if strings.HasPrefix(c.Name, base) {
			got[c.Name] = c
		}
		if c.Name == "master" && !c.IsSystem() {
			t.Errorf("master: IsSystem() = false")
		}
	}
	check := func(name, what string, ok bool) {
		t.Helper()
		if got[name] == nil {
			t.Fatalf("%s missing from the listing", name)
		}
		if !ok {
			t.Errorf("%s: want %s, got %+v", name, what, *got[name])
		}
	}
	check(base+"_ro", "read-only", got[base+"_ro"] != nil && got[base+"_ro"].IsReadOnly)
	check(base+"_ac", "auto close", got[base+"_ac"] != nil && got[base+"_ac"].IsAutoCloseOn)
	check(base+"_su", "single user", got[base+"_su"] != nil && got[base+"_su"].UserAccess == "SINGLE_USER")
	check(snap, "a read-only snapshot", got[snap] != nil && got[snap].IsSnapshot && got[snap].IsReadOnly)
	ok := got[base+"_ok"]
	check(base+"_ok", "an eligible database", ok != nil && ok.State == "ONLINE" && ok.RecoveryModel == RecoveryModelFull &&
		!ok.IsReadOnly && !ok.IsSnapshot && ok.UserAccess == "MULTI_USER" && !ok.IsAutoCloseOn && !ok.IsMirrored &&
		ok.LogBackupChainStarted && !ok.IsSystem())
}
