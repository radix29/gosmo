//go:build livedb

package gosmo

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestLiveSnapshotDefaultsForFilesDifferingOnlyByExtension: a database whose
// primary and secondary data files share a folder and a stem (db.mdf,
// db.ndf). The defaults dropped the extension, so both mapped onto one sparse
// file and CreateDatabaseSnapshot failed "Cannot create file … because it
// already exists" — as did gossms's New Snapshot dialog, which fills its
// paths from SnapshotFileDefaults.
//
//	go test -tags livedb . -run TestLiveSnapshotDefaults -v -livedb '…'
//
// Creates and drops its own database and snapshot.
func TestLiveSnapshotDefaultsForFilesDifferingOnlyByExtension(t *testing.T) {
	db, ctx, done := liveDBTimeout(t, 2*time.Minute)
	defer done()
	srv := &Server{db: db}

	const name, snap = "gossms_live_snapdef", "gossms_live_snapdef_snap"
	cleanup := func() {
		c := context.Background()
		srv.exec(c, "IF DB_ID('"+snap+"') IS NOT NULL DROP DATABASE "+QuoteName(snap))
		srv.exec(c, "IF DB_ID('"+name+"') IS NOT NULL DROP DATABASE "+QuoteName(name))
	}
	cleanup()
	defer cleanup()

	var dir string
	if err := srv.db.QueryRowContext(ctx,
		"SELECT CAST(SERVERPROPERTY('InstanceDefaultDataPath') AS nvarchar(4000))").Scan(&dir); err != nil || dir == "" {
		t.Fatalf("default data path: %q, %v", dir, err)
	}
	sep := `\`
	if strings.HasPrefix(dir, "/") {
		sep = "/"
	}
	dir = strings.TrimRight(dir, `\/`) + sep
	stem := dir + name
	if _, err := srv.exec(ctx, "CREATE DATABASE "+QuoteName(name)+
		" ON PRIMARY (NAME = "+QuoteName(name)+", FILENAME = "+QuoteLiteral(stem+".mdf")+"),"+
		" FILEGROUP fg2 (NAME = "+QuoteName(name+"_2")+", FILENAME = "+QuoteLiteral(stem+".ndf")+")"+
		" LOG ON (NAME = "+QuoteName(name+"_log")+", FILENAME = "+QuoteLiteral(stem+"_log.ldf")+")"); err != nil {
		t.Fatalf("create %s: %v", name, err)
	}

	specs, err := srv.SnapshotFileDefaults(ctx, name, snap)
	if err != nil {
		t.Fatalf("SnapshotFileDefaults: %v", err)
	}
	t.Logf("defaults: %+v", specs)
	if len(specs) != 2 || strings.EqualFold(specs[0].FileName, specs[1].FileName) {
		t.Fatalf("defaults %+v, want two distinct sparse files", specs)
	}

	if _, err := srv.CreateDatabaseSnapshot(ctx, CreateDatabaseSnapshotRequest{Name: snap, SourceDatabase: name}); err != nil {
		t.Fatalf("CreateDatabaseSnapshot from the defaults: %v", err)
	}
	var n int
	if err := srv.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM sys.master_files WHERE database_id = DB_ID(@p1) AND type = 0", snap).Scan(&n); err != nil || n != 2 {
		t.Errorf("the snapshot has %d data files (%v), want 2", n, err)
	}
}
