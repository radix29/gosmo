//go:build livedb

// Live verification of PlanRestore (restore_plan.go) and the tail-log
// backup (BackupOptions.NoRecovery): msdb history the server wrote, the
// chain it plans, and the rows each restore brings back.
//
//	go test -tags livedb . -run TestLiveRestoreChain -v \
//	  -livedb 'sqlserver://sa:PASS@host?TrustServerCertificate=true'
//
// Creates and drops its own databases and backup files; touches nothing else.
package gosmo

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"
)

func TestLiveRestoreChain(t *testing.T) {
	db, ctx, done := liveDBTimeout(t, 3*time.Minute)
	t.Cleanup(done)

	const name, copyName = "gosmo_restore_chain_live", "gosmo_restore_chain_live_copy"
	_, drop := liveScratchDB(t, db, ctx, name)
	t.Cleanup(drop)
	_, dropCopy := liveScratchDB(t, db, ctx, copyName)
	dropCopy() // only its cleanup is wanted: the restore creates it
	t.Cleanup(dropCopy)
	srv, err := NewServer(ctx, db)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	if srv.refusesSingleUser() {
		t.Skip("a Managed Instance restores FROM URL only")
	}
	t.Cleanup(func() {
		db.ExecContext(context.Background(), "EXEC msdb.dbo.sp_delete_database_backuphistory @database_name = N'"+name+"'")
	})
	exec := func(q string) {
		t.Helper()
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	insert := func(i int) { exec(fmt.Sprintf("INSERT INTO [%s].dbo.t VALUES (%d)", name, i)) }
	backup := func(file string, opts BackupOptions) {
		t.Helper()
		device := liveBackupPath(t, srv, ctx, file)
		t.Cleanup(func() {
			db.ExecContext(context.Background(), "EXEC master.dbo.xp_delete_files @FilePath = N'"+device+"'")
		})
		opts.Database, opts.Devices, opts.Init = name, []BackupTarget{DiskTarget(device)}, true
		if err := srv.Backup(ctx, opts); err != nil {
			t.Fatalf("backup %s: %v", file, err)
		}
	}

	exec("ALTER DATABASE [" + name + "] SET RECOVERY FULL")
	exec("CREATE TABLE [" + name + "].dbo.t (i int)")
	backup(name+"_full.bak", BackupOptions{})
	insert(1)
	backup(name+"_diff.bak", BackupOptions{Action: BackupActionDifferential})
	insert(2)
	backup(name+"_log1.trn", BackupOptions{Action: BackupActionLog})
	insert(3)
	exec("WAITFOR DELAY '00:00:01.500'")
	var stop time.Time
	if err := db.QueryRowContext(ctx, "SELECT GETDATE()").Scan(&stop); err != nil {
		t.Fatal(err)
	}
	exec("WAITFOR DELAY '00:00:01.500'")
	insert(4)
	backup(name+"_log2.trn", BackupOptions{Action: BackupActionLog})
	insert(5)

	hist, err := srv.BackupHistory(ctx, name)
	if err != nil {
		t.Fatalf("BackupHistory: %v", err)
	}
	for _, b := range hist {
		if b.FirstLSN == "" || b.LastLSN == "" || b.BackupSetGUID == "" {
			t.Errorf("%s backup has no LSNs or GUID: %+v", b.SetType, *b)
		}
	}

	types := func(p *RestorePlan) []BackupSetType {
		var ts []BackupSetType
		for _, b := range p.Sets {
			ts = append(ts, b.SetType)
		}
		return ts
	}
	chain := []BackupSetType{BackupSetDatabase, BackupSetDifferential, BackupSetLog, BackupSetLog}

	// The differential is matched to its full by GUID, and the logs chain by
	// LSN, from what the server recorded.
	plan, err := PlanRestore(hist, RestorePlanOptions{StopAt: &stop})
	if err != nil {
		t.Fatalf("PlanRestore StopAt: %v", err)
	}
	if !slices.Equal(types(plan), chain) {
		t.Fatalf("plan = %v, want %v", types(plan), chain)
	}

	// restore applies plan to target, then the tail log if any: every set
	// WITH NORECOVERY but the last, StopAt on each log.
	restore := func(target string, plan *RestorePlan, stopAt *time.Time, tail string) {
		t.Helper()
		paths, err := srv.DefaultPaths(ctx)
		if err != nil {
			t.Fatalf("DefaultPaths: %v", err)
		}
		reloc := RestoreRelocation{DefaultDataDir: paths.Data, DefaultLogDir: paths.Log}
		steps := slices.Clone(plan.Sets)
		if tail != "" {
			steps = append(steps, &BackupInfo{SetType: BackupSetLog, Devices: []string{tail}})
		}
		for i, b := range steps {
			targets := []BackupTarget{DiskTarget(b.Devices[0])}
			headers, err := srv.BackupHeaders(ctx, targets...)
			if err != nil {
				t.Fatalf("BackupHeaders: %v", err)
			}
			h := BackupSetAt(headers, b.SetNumber())
			var files []*BackupFile
			if h.SetType.PlacesFiles() {
				if files, err = srv.BackupFileList(ctx, h.SetNumber(), targets...); err != nil {
					t.Fatalf("BackupFileList: %v", err)
				}
			}
			opts := RestoreOptions{Database: target, Devices: targets, Recovery: RestoreWithNoRecovery, Replace: i == 0}
			opts.FromHeader(h, files, reloc)
			if i == len(steps)-1 {
				opts.Recovery = RestoreWithRecovery
			}
			if h.SetType == BackupSetLog {
				opts.StopAt = stopAt
			}
			if err := srv.Restore(ctx, opts); err != nil {
				t.Fatalf("restore step %d (%s): %v", i, h.SetType, err)
			}
		}
	}
	count := func(dbName string) int {
		t.Helper()
		var n int
		if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM ["+dbName+"].dbo.t").Scan(&n); err != nil {
			t.Fatalf("count %s: %v", dbName, err)
		}
		return n
	}

	restore(copyName, plan, &stop, "")
	if n := count(copyName); n != 3 {
		t.Errorf("point-in-time copy has %d rows, want 3 (the rows before StopAt)", n)
	}

	// A StopAt past the newest log needs the tail.
	late := stop.Add(time.Hour)
	if _, err := PlanRestore(hist, RestorePlanOptions{StopAt: &late}); err == nil {
		t.Error("PlanRestore past the newest log without a tail log: no error")
	}

	// The tail-log restore over the database itself: planned first, then the
	// tail backed up WITH NORECOVERY, then the plan and the tail restored.
	plan, err = PlanRestore(hist, RestorePlanOptions{TailLog: true})
	if err != nil || !plan.TailLog || !slices.Equal(types(plan), chain) {
		t.Fatalf("PlanRestore TailLog: %v (tail %v), %v", types(plan), plan != nil && plan.TailLog, err)
	}
	backup(name+"_tail.trn", BackupOptions{Action: BackupActionLog, NoRecovery: true})
	var state string
	if err := db.QueryRowContext(ctx, "SELECT state_desc FROM sys.databases WHERE name = @p1", name).Scan(&state); err != nil || state != "RESTORING" {
		t.Fatalf("after the tail-log backup the database is %q (%v), want RESTORING", state, err)
	}
	restore(name, plan, nil, liveBackupPath(t, srv, ctx, name+"_tail.trn"))
	if n := count(name); n != 5 {
		t.Errorf("restored database has %d rows, want 5 (the tail's row included)", n)
	}
}
