//go:build livedb

// Live verification of DatabaseFileInfo.SizeOnDiskKB on a database snapshot,
// the case it exists for: sys.master_files.size is the sparse file's ceiling
// (its source file's size), not what it occupies.
//
//	go test -tags livedb . -run TestLiveSnapshotSizeOnDisk -v \
//	  -livedb 'sqlserver://sa:PASS@host?TrustServerCertificate=true'
//
// Creates and drops a throwaway source database, a snapshot of it and a
// login; touches nothing else.
package gosmo

import (
	"context"
	"testing"
)

func TestLiveSnapshotSizeOnDisk(t *testing.T) {
	db, ctx, done := liveDB(t)
	t.Cleanup(done) // after the drops below, which t.Cleanup runs first

	const name, snapName = "gosmo_snapsize_src", "gosmo_snapsize_snap"
	const login, pass = "gosmo_snapsize_login", "Sn4pSize!pw"
	dropSnap := func() {
		db.ExecContext(context.Background(), "IF DB_ID('"+snapName+"') IS NOT NULL DROP DATABASE ["+snapName+"]")
	}
	dropSnap()
	src, drop := liveScratchDB(t, db, ctx, name)
	t.Cleanup(drop)
	t.Cleanup(dropSnap)

	// Big enough that a near-empty sparse file is unmistakably smaller.
	var dataFile string
	if err := db.QueryRowContext(ctx,
		"SELECT name FROM sys.master_files WHERE database_id = DB_ID(@p1) AND type = 0", name).Scan(&dataFile); err != nil {
		t.Fatalf("locate the data file: %v", err)
	}
	if _, err := db.ExecContext(ctx, "ALTER DATABASE ["+name+"] MODIFY FILE (NAME = ["+dataFile+"], SIZE = 64MB)"); err != nil {
		t.Fatalf("grow the source: %v", err)
	}
	srv := src.server
	if _, err := srv.CreateDatabaseSnapshot(ctx, CreateDatabaseSnapshotRequest{Name: snapName, SourceDatabase: name}); err != nil {
		t.Fatalf("CreateDatabaseSnapshot: %v", err)
	}

	files, err := srv.DatabaseFiles(ctx, snapName)
	if err != nil {
		t.Fatalf("DatabaseFiles: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("the snapshot has no files")
	}
	for _, f := range files {
		t.Logf("%s: SizeKB %d, SizeOnDiskKB %d", f.Name, f.SizeKB, f.SizeOnDiskKB)
		if f.SizeKB < 64*1024 {
			t.Errorf("%s: SizeKB = %d, want the source's 64 MB ceiling", f.Name, f.SizeKB)
		}
		if f.SizeOnDiskKB <= 0 || f.SizeOnDiskKB > f.SizeKB/8 {
			t.Errorf("%s: SizeOnDiskKB = %d, want a near-empty sparse file well under SizeKB %d", f.Name, f.SizeOnDiskKB, f.SizeKB)
		}
	}

	// Without VIEW SERVER STATE the DMV is refused (Msg 300); the read must
	// still list the files, with the footprint unknown.
	db.ExecContext(ctx, "IF SUSER_ID('"+login+"') IS NOT NULL DROP LOGIN ["+login+"]")
	if _, err := db.ExecContext(ctx, "CREATE LOGIN ["+login+"] WITH PASSWORD = '"+pass+"', CHECK_POLICY = OFF"); err != nil {
		t.Fatalf("create login: %v", err)
	}
	t.Cleanup(func() { db.ExecContext(context.Background(), "DROP LOGIN ["+login+"]") })
	if _, err := db.ExecContext(ctx, "GRANT VIEW ANY DEFINITION TO ["+login+"]"); err != nil {
		t.Fatalf("grant: %v", err)
	}
	pool := liveOpenAs(t, login, pass)
	t.Cleanup(func() { pool.Close() })
	low, err := (&Server{db: pool}).DatabaseFiles(ctx, snapName)
	if err != nil {
		t.Fatalf("DatabaseFiles without VIEW SERVER STATE: %v", err)
	}
	if len(low) != len(files) {
		t.Fatalf("without VIEW SERVER STATE: %d files, want %d", len(low), len(files))
	}
	for i, f := range low {
		if f.SizeOnDiskKB != 0 || f.SizeKB != files[i].SizeKB {
			t.Errorf("without VIEW SERVER STATE: %s SizeKB %d, SizeOnDiskKB %d; want the ceiling and 0 (unknown)", f.Name, f.SizeKB, f.SizeOnDiskKB)
		}
	}
}
