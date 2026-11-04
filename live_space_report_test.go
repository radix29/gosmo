//go:build livedb

// Live verification of SpaceReport, TableSizes, LogSpace and FileVolumes
// against the server's own answers: sp_spaceused (database and per table),
// sys.dm_db_log_space_usage and sys.dm_db_log_info. The figures are compared
// exactly, in KB, never within a tolerance — matching those is the reason the
// reads exist.
//
//	go test -tags livedb . -run 'TestLive(SpaceReport|TableSizes|LogSpace|FileVolumes)' -v \
//	  -livedb 'sqlserver://sa:PASS@host?TrustServerCertificate=true'
//
// Builds one throwaway database, spaceLiveName, and drops it; touches
// nothing else.
package gosmo

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"
)

const spaceLiveName = "gosmo_live_space"

// spaceLiveSetup creates the throwaway database with the shapes sp_spaceused
// treats specially: a heap with LOB data, a table with primary and secondary
// XML indexes (internal tables), a nonclustered index carrying an
// nvarchar(max) INCLUDE column (LOB that counts as index), and a table
// partitioned three ways.
func spaceLiveSetup(t *testing.T, db *sql.DB, ctx context.Context) (*Server, *Database) {
	t.Helper()
	srv, err := NewServer(ctx, db)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	drop := func() {
		c := context.Background()
		db.ExecContext(c, "IF DB_ID(N'"+spaceLiveName+"') IS NOT NULL ALTER DATABASE ["+spaceLiveName+"] SET SINGLE_USER WITH ROLLBACK IMMEDIATE")
		db.ExecContext(c, "IF DB_ID(N'"+spaceLiveName+"') IS NOT NULL DROP DATABASE ["+spaceLiveName+"]")
	}
	drop()
	t.Cleanup(drop)
	if _, err := db.ExecContext(ctx, "CREATE DATABASE ["+spaceLiveName+"]"); err != nil {
		t.Fatalf("create throwaway database: %v", err)
	}
	for _, stmt := range []string{
		`CREATE TABLE dbo.heap_lob (id int NOT NULL, big varchar(max) NULL, pad char(200) NOT NULL DEFAULT '')`,
		`INSERT dbo.heap_lob (id, big) SELECT TOP (300) ROW_NUMBER() OVER (ORDER BY (SELECT 0)), REPLICATE(CAST('x' AS varchar(max)), 9000) FROM sys.all_objects`,
		`CREATE TABLE dbo.xml_doc (id int NOT NULL PRIMARY KEY, doc xml NULL)`,
		`INSERT dbo.xml_doc (id, doc) SELECT TOP (200) ROW_NUMBER() OVER (ORDER BY (SELECT 0)), CAST('<r><a>' + REPLICATE('y', 500) + '</a><b n="1"/><b n="2"/></r>' AS xml) FROM sys.all_objects`,
		`CREATE PRIMARY XML INDEX px_doc ON dbo.xml_doc (doc)`,
		`CREATE XML INDEX sx_doc_path ON dbo.xml_doc (doc) USING XML INDEX px_doc FOR PATH`,
		`CREATE TABLE dbo.nc_include (id int NOT NULL PRIMARY KEY, k int NOT NULL, note nvarchar(max) NULL)`,
		`INSERT dbo.nc_include (id, k, note) SELECT TOP (250) ROW_NUMBER() OVER (ORDER BY (SELECT 0)), ROW_NUMBER() OVER (ORDER BY (SELECT 0)) % 17, REPLICATE(CAST(N'z' AS nvarchar(max)), 5000) FROM sys.all_objects`,
		`CREATE NONCLUSTERED INDEX ix_nc_k ON dbo.nc_include (k) INCLUDE (note)`,
		`CREATE PARTITION FUNCTION pf_space (int) AS RANGE RIGHT FOR VALUES (100, 200)`,
		`CREATE PARTITION SCHEME ps_space AS PARTITION pf_space ALL TO ([PRIMARY])`,
		`CREATE TABLE dbo.parted (id int NOT NULL, pad char(400) NOT NULL DEFAULT '', CONSTRAINT pk_parted PRIMARY KEY (id)) ON ps_space (id)`,
		`INSERT dbo.parted (id) SELECT TOP (300) ROW_NUMBER() OVER (ORDER BY (SELECT 0)) FROM sys.all_objects`,
		`CREATE TABLE dbo.empty_one (id int NOT NULL)`,
	} {
		if _, err := db.ExecContext(ctx, "USE ["+spaceLiveName+"]; "+stmt); err != nil {
			t.Fatalf("setup %q: %v", stmt, err)
		}
	}
	d, err := srv.DatabaseByName(ctx, spaceLiveName)
	if err != nil {
		t.Fatalf("DatabaseByName: %v", err)
	}
	return srv, d
}

// spaceusedKB parses one of sp_spaceused's "1234 KB" strings.
func spaceusedKB(t *testing.T, s string) int64 {
	t.Helper()
	n, err := strconv.ParseInt(strings.TrimSuffix(strings.TrimSpace(s), " KB"), 10, 64)
	if err != nil {
		t.Fatalf("parse sp_spaceused figure %q: %v", s, err)
	}
	return n
}

// spaceusedMB renders KB the way sp_spaceused prints MB: STR(x, 15, 2), which
// rounds half away from zero, then LTRIM.
func spaceusedMB(kb int64) string {
	// kb/1024 to two decimals, rounded half up, in integers so no float
	// rounding decides the last digit.
	hundredths := (kb*100*2 + 1024) / (1024 * 2)
	return fmt.Sprintf("%d.%02d MB", hundredths/100, hundredths%100)
}

// spaceLiveConn pins one connection in the throwaway database, so the
// procedure and the DMVs answer for it.
func spaceLiveConn(t *testing.T, db *sql.DB, ctx context.Context) *sql.Conn {
	t.Helper()
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("conn: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	if _, err := conn.ExecContext(ctx, "USE ["+spaceLiveName+"]"); err != nil {
		t.Fatalf("use: %v", err)
	}
	return conn
}

func TestLiveSpaceReportMatchesSpSpaceused(t *testing.T) {
	db, ctx, done := liveDBTimeout(t, 180e9)
	t.Cleanup(done)
	_, d := spaceLiveSetup(t, db, ctx)

	// A fresh database's figures settle a moment after the setup (2016
	// counted two more data pages a second later, from neither read), so
	// SpaceReport is bracketed by two sp_spaceused calls and taken only when
	// they agree.
	conn := spaceLiveConn(t, db, ctx)
	type summary struct{ name, size, unalloc, reserved, data, index, unused string }
	spaceused := func() summary {
		var s summary
		if err := conn.QueryRowContext(ctx, "EXEC sys.sp_spaceused @oneresultset = 1").
			Scan(&s.name, &s.size, &s.unalloc, &s.reserved, &s.data, &s.index, &s.unused); err != nil {
			t.Fatalf("sp_spaceused: %v", err)
		}
		return s
	}
	var got SpaceReport
	var after summary
	for try := 1; ; try++ {
		before := spaceused()
		var err error
		if got, err = d.SpaceReport(ctx); err != nil {
			t.Fatalf("SpaceReport: %v", err)
		}
		if after = spaceused(); after == before {
			break
		}
		if try == 5 {
			t.Fatalf("sp_spaceused did not settle: %+v then %+v", before, after)
		}
		time.Sleep(time.Second)
	}
	size, unalloc, reserved, data, index, unused := after.size, after.unalloc, after.reserved, after.data, after.index, after.unused
	if g := spaceusedMB(got.DatabaseSizeKB); g != size {
		t.Errorf("database_size: got %s (%d KB), sp_spaceused %s", g, got.DatabaseSizeKB, size)
	}
	if g := spaceusedMB(got.UnallocatedKB); g != unalloc {
		t.Errorf("unallocated space: got %s (%d KB), sp_spaceused %s", g, got.UnallocatedKB, unalloc)
	}
	for _, c := range []struct {
		what string
		got  int64
		want string
	}{
		{"reserved", got.ReservedKB, reserved},
		{"data", got.DataKB, data},
		{"index_size", got.IndexKB, index},
		{"unused", got.UnusedKB, unused},
	} {
		if w := spaceusedKB(t, c.want); c.got != w {
			t.Errorf("%s: got %d KB, sp_spaceused %d KB", c.what, c.got, w)
		}
	}

	// The file figures against sys.database_files directly.
	var dataKB, logKB, dataUsed, logUsed int64
	if err := conn.QueryRowContext(ctx, `
SELECT SUM(CASE WHEN type = 0 THEN CAST(size AS bigint) ELSE 0 END) * 8,
       SUM(CASE WHEN type = 1 THEN CAST(size AS bigint) ELSE 0 END) * 8,
       SUM(CASE WHEN type = 0 THEN CAST(FILEPROPERTY(name, 'SpaceUsed') AS bigint) ELSE 0 END) * 8,
       SUM(CASE WHEN type = 1 THEN CAST(FILEPROPERTY(name, 'SpaceUsed') AS bigint) ELSE 0 END) * 8
FROM sys.database_files`).Scan(&dataKB, &logKB, &dataUsed, &logUsed); err != nil {
		t.Fatalf("database_files: %v", err)
	}
	// The log's used space moves between two reads (the setup's last
	// statements are still being checkpointed), so it is bounded, not matched.
	if got.DataFilesKB != dataKB || got.LogFilesKB != logKB || got.DataUsedKB != dataUsed ||
		got.LogUsedKB <= 0 || got.LogUsedKB > got.LogFilesKB {
		t.Errorf("files: got data %d/%d log %d/%d KB, database_files data %d/%d log %d/%d KB",
			got.DataFilesKB, got.DataUsedKB, got.LogFilesKB, got.LogUsedKB, dataKB, dataUsed, logKB, logUsed)
	}
}

func TestLiveTableSizesMatchSpSpaceused(t *testing.T) {
	db, ctx, done := liveDBTimeout(t, 180e9)
	t.Cleanup(done)
	_, d := spaceLiveSetup(t, db, ctx)

	sizes, err := d.TableSizes(ctx)
	if err != nil {
		t.Fatalf("TableSizes: %v", err)
	}
	conn := spaceLiveConn(t, db, ctx)
	seen := map[string]bool{}
	for _, ts := range sizes {
		full := ts.Schema + "." + ts.Name
		seen[full] = true
		var name, rows, reserved, data, index, unused string
		if err := conn.QueryRowContext(ctx, "EXEC sys.sp_spaceused @objname = @p1", full).
			Scan(&name, &rows, &reserved, &data, &index, &unused); err != nil {
			t.Fatalf("sp_spaceused %s: %v", full, err)
		}
		t.Logf("%-16s sp_spaceused rows %s reserved %s data %s index %s unused %s | got %+v",
			full, strings.TrimSpace(rows), reserved, data, index, unused, *ts)
		if r, _ := strconv.ParseInt(strings.TrimSpace(rows), 10, 64); r != ts.Rows {
			t.Errorf("%s rows: got %d, sp_spaceused %s", full, ts.Rows, rows)
		}
		for _, c := range []struct {
			what string
			got  int64
			want string
		}{
			{"reserved", ts.ReservedKB, reserved},
			{"data", ts.DataKB, data},
			{"index_size", ts.IndexKB, index},
			{"unused", ts.UnusedKB, unused},
		} {
			if w := spaceusedKB(t, c.want); c.got != w {
				t.Errorf("%s %s: got %d KB, sp_spaceused %d KB", full, c.what, c.got, w)
			}
		}
	}
	for _, want := range []string{"dbo.heap_lob", "dbo.xml_doc", "dbo.nc_include", "dbo.parted", "dbo.empty_one"} {
		if !seen[want] {
			t.Errorf("TableSizes did not list %s", want)
		}
	}
}

func TestLiveLogSpaceMatchesTheDMVs(t *testing.T) {
	db, ctx, done := liveDBTimeout(t, 180e9)
	t.Cleanup(done)
	srv, d := spaceLiveSetup(t, db, ctx)

	got, err := d.LogSpace(ctx)
	if err != nil {
		t.Fatalf("LogSpace: %v", err)
	}
	conn := spaceLiveConn(t, db, ctx)
	var size, used int64
	var pct float64
	var wait string
	if err := conn.QueryRowContext(ctx, `
SELECT ls.total_log_size_in_bytes / 1024, ls.used_log_space_in_bytes / 1024,
       CAST(ls.used_log_space_in_percent AS float), d.log_reuse_wait_desc
FROM sys.dm_db_log_space_usage ls JOIN sys.databases d ON d.database_id = DB_ID()`).
		Scan(&size, &used, &pct, &wait); err != nil {
		t.Fatalf("dm_db_log_space_usage: %v", err)
	}
	t.Logf("LogSpace %+v; DMV size %d used %d pct %v wait %s", got, size, used, pct, wait)
	// The log is idle in a throwaway database, so the two reads see the same
	// figures; used space can still move by a checkpoint between them, which
	// the size never does.
	if got.SizeKB != size || got.ReuseWait != wait {
		t.Errorf("LogSpace size %d wait %s, DMV size %d wait %s", got.SizeKB, got.ReuseWait, size, wait)
	}
	if got.UsedKB != used || got.UsedPercent != pct {
		t.Errorf("LogSpace used %d KB (%v%%), DMV %d KB (%v%%)", got.UsedKB, got.UsedPercent, used, pct)
	}

	wantVLFs := buildAtLeast(srv.Info(), SQLServer2016, logInfoBuild)
	if got.VLFsKnown != wantVLFs {
		t.Fatalf("VLFsKnown = %v on %s, want %v", got.VLFsKnown, srv.Info().ProductVersion, wantVLFs)
	}
	if wantVLFs {
		var vlfs, active int
		if err := conn.QueryRowContext(ctx, `SELECT COUNT(*), SUM(CAST(vlf_active AS int)) FROM sys.dm_db_log_info(DB_ID())`).
			Scan(&vlfs, &active); err != nil {
			t.Fatalf("dm_db_log_info: %v", err)
		}
		if got.VLFs != vlfs || got.ActiveVLFs != active || vlfs == 0 || active == 0 {
			t.Errorf("VLFs %d/%d active, dm_db_log_info %d/%d", got.VLFs, got.ActiveVLFs, vlfs, active)
		}
	}
}

func TestLiveFileVolumesCoversEveryFile(t *testing.T) {
	db, ctx, done := liveDB(t)
	t.Cleanup(done)
	srv, err := NewServer(ctx, db)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	vols, visible, err := srv.FileVolumes(ctx)
	if err != nil {
		t.Fatalf("FileVolumes: %v", err)
	}
	if !visible {
		t.Fatal("FileVolumes reports hidden for a login with VIEW SERVER STATE")
	}
	var files int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM sys.master_files").Scan(&files); err != nil {
		t.Fatalf("count master_files: %v", err)
	}
	if len(vols) != files {
		t.Errorf("FileVolumes returned %d rows, sys.master_files has %d", len(vols), files)
	}
	for _, v := range vols {
		if v.PhysicalName == "" || !v.Plausible() {
			t.Errorf("implausible or unnamed row: %+v", v)
		}
	}

	// Every grouped DiskVolumes row is some file's volume.
	disks, err := srv.DiskVolumes(ctx)
	if err != nil {
		t.Fatalf("DiskVolumes: %v", err)
	}
	for _, dv := range disks {
		found := false
		for _, v := range vols {
			if v.MountPoint == dv.MountPoint && v.VolumeName == dv.VolumeName {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("DiskVolumes row %+v matches no FileVolumes row", dv)
		}
	}
}

// TestLiveFileVolumesHiddenWithoutViewServerState is the permission branch:
// a login without VIEW SERVER STATE gets visible=false and no error, where a
// bare read of the DMV fails Msg 300.
func TestLiveFileVolumesHiddenWithoutViewServerState(t *testing.T) {
	db, ctx, done := liveDB(t)
	t.Cleanup(done)
	const login, pass = "gosmo_live_space_nvss", "Pw!gosmo_live_space_nvss1"
	c := context.Background()
	db.ExecContext(c, "IF SUSER_ID(N'"+login+"') IS NOT NULL DROP LOGIN ["+login+"]")
	if _, err := db.ExecContext(ctx, "CREATE LOGIN ["+login+"] WITH PASSWORD = N'"+pass+"', CHECK_POLICY = OFF"); err != nil {
		t.Fatalf("create login: %v", err)
	}
	t.Cleanup(func() { db.ExecContext(c, "DROP LOGIN ["+login+"]") })

	pool, err := sql.Open("sqlserver", liveRestrictedDSN(t, login, pass))
	if err != nil {
		t.Fatalf("open as %s: %v", login, err)
	}
	t.Cleanup(func() { pool.Close() })
	// Not NewServer: its server-info read needs the permission withheld here.
	vols, visible, err := (&Server{db: pool}).FileVolumes(ctx)
	if err != nil {
		t.Fatalf("FileVolumes without VIEW SERVER STATE: %v", err)
	}
	if visible || len(vols) != 0 {
		t.Errorf("FileVolumes = %d rows, visible %v; want none, hidden", len(vols), visible)
	}
}
