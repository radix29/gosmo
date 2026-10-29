package gosmo

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"io"
	"strings"
	"testing"
)

// spaceDriver serves one result of spaceRows and records the text of every
// query, so a test can name each scanned field and see which catalog views
// the version gates put in the batch.
type spaceDriver struct{}

var (
	spaceRows    [][]driver.Value
	spaceColumns int
	spaceQueries []string
)

func (spaceDriver) Open(string) (driver.Conn, error) { return spaceConn{}, nil }

type spaceConn struct{}

func (spaceConn) Prepare(string) (driver.Stmt, error) { return nil, driver.ErrSkip }
func (spaceConn) Close() error                        { return nil }
func (spaceConn) Begin() (driver.Tx, error)           { return nil, driver.ErrSkip }

func (spaceConn) ExecContext(context.Context, string, []driver.NamedValue) (driver.Result, error) {
	return driver.ResultNoRows, nil
}

func (spaceConn) QueryContext(_ context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	// Database reads arrive as [db].sys.sp_executesql with the batch as the
	// first argument; server reads arrive as the text itself.
	if len(args) > 0 {
		if s, ok := args[0].Value.(string); ok {
			q = s
		}
	}
	spaceQueries = append(spaceQueries, q)
	return &spaceResult{rows: spaceRows}, nil
}

type spaceResult struct{ rows [][]driver.Value }

func (r *spaceResult) Columns() []string { return make([]string, spaceColumns) }
func (r *spaceResult) Close() error      { return nil }
func (r *spaceResult) Next(dest []driver.Value) error {
	if len(r.rows) == 0 {
		return io.EOF
	}
	copy(dest, r.rows[0])
	r.rows = r.rows[1:]
	return nil
}

func init() { sql.Register("fakespace", spaceDriver{}) }

// spaceServer is a Server on the fake driver reporting the given version.
func spaceServer(t *testing.T, info *ServerInfo, rows ...[]driver.Value) *Server {
	t.Helper()
	db, err := sql.Open("fakespace", "")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	spaceRows, spaceQueries = rows, nil
	spaceColumns = 0
	if len(rows) > 0 {
		spaceColumns = len(rows[0])
	}
	return &Server{db: db, info: info}
}

func TestBuildAtLeast(t *testing.T) {
	for _, c := range []struct {
		name string
		info *ServerInfo
		want bool
	}{
		{"nil info counts as newest", nil, true},
		{"unread version counts as newest", &ServerInfo{}, true},
		{"2016 SP1", &ServerInfo{VersionMajor: 13, VersionBuild: 4001}, false},
		{"2016 one build short of SP2", &ServerInfo{VersionMajor: 13, VersionBuild: 5025}, false},
		{"2016 SP2", &ServerInfo{VersionMajor: 13, VersionBuild: 5026}, true},
		{"2016 SP3", &ServerInfo{VersionMajor: 13, VersionBuild: 6500}, true},
		{"2017 RTM, a lower build of a later major", &ServerInfo{VersionMajor: 14, VersionBuild: 1000}, true},
		{"2014, below the major", &ServerInfo{VersionMajor: 12, VersionBuild: 9000}, false},
		{"Managed Instance reporting 12", &ServerInfo{VersionMajor: 12, VersionBuild: 2000, EngineEdition: int(EngineAzureManagedInst)}, true},
	} {
		if got := buildAtLeast(c.info, SQLServer2016, logInfoBuild); got != c.want {
			t.Errorf("%s: buildAtLeast = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestLogSpaceNamesLogInfoOnlyFrom2016SP2(t *testing.T) {
	row := []driver.Value{int64(1024), int64(512), float64(50), "LOG_BACKUP", int64(7), int64(3)}
	for _, c := range []struct {
		name string
		info *ServerInfo
		want LogSpace
	}{
		{"2016 SP1", &ServerInfo{VersionMajor: 13, VersionBuild: 4001},
			LogSpace{SizeKB: 1024, UsedKB: 512, UsedPercent: 50, ReuseWait: "LOG_BACKUP"}},
		{"2016 SP3", &ServerInfo{VersionMajor: 13, VersionBuild: 6500},
			LogSpace{SizeKB: 1024, UsedKB: 512, UsedPercent: 50, ReuseWait: "LOG_BACKUP", VLFs: 7, ActiveVLFs: 3, VLFsKnown: true}},
	} {
		srv := spaceServer(t, c.info, row)
		got, err := (&Database{server: srv, Name: "d"}).LogSpace(context.Background())
		if err != nil {
			t.Fatalf("%s: LogSpace: %v", c.name, err)
		}
		if got != c.want {
			t.Errorf("%s: LogSpace = %+v, want %+v", c.name, got, c.want)
		}
		named := strings.Contains(strings.Join(spaceQueries, "\n"), "dm_db_log_info")
		if named != c.want.VLFsKnown {
			t.Errorf("%s: query names dm_db_log_info = %v, want %v", c.name, named, c.want.VLFsKnown)
		}
	}
}

func TestSpaceReportFollowsTheVersionsProcedure(t *testing.T) {
	// data files 100 pages, log 50, used 40/10, checkpoint 20 pages,
	// 3 MB precreated (read), reserved 60, used 45, data 30.
	row := []driver.Value{int64(100), int64(50), int64(40), int64(10), int64(20), int64(3 << 20), true,
		int64(60), int64(45), int64(30)}
	base := SpaceReport{
		DataFilesKB: 800, LogFilesKB: 400, DataUsedKB: 320, LogUsedKB: 80, CheckpointFilesKB: 160,
		ReservedKB: 480, DataKB: 240, IndexKB: 120, UnusedKB: 120,
	}
	for _, c := range []struct {
		name       string
		info       *ServerInfo
		partitions string
		xtp        bool
		size       int64
		unalloc    int64
	}{
		// 2016's procedure: sys.partitions, no checkpoint files in the size,
		// no precreated files in unallocated.
		{"2016", &ServerInfo{VersionMajor: 13, VersionBuild: 6500}, "sys.partitions p", false, 1200, 320},
		{"2017", &ServerInfo{VersionMajor: 14}, "sys.system_internals_partitions p", true, 1360, 320 + 3072},
	} {
		srv := spaceServer(t, c.info, row)
		got, err := (&Database{server: srv, Name: "d"}).SpaceReport(context.Background())
		if err != nil {
			t.Fatalf("%s: SpaceReport: %v", c.name, err)
		}
		want := base
		want.DatabaseSizeKB, want.UnallocatedKB = c.size, c.unalloc
		if got != want {
			t.Errorf("%s: SpaceReport = %+v\nwant %+v", c.name, got, want)
		}
		q := strings.Join(spaceQueries, "\n")
		if !strings.Contains(q, c.partitions) {
			t.Errorf("%s: query does not read %s:\n%s", c.name, c.partitions, q)
		}
		if strings.Contains(q, "dm_db_xtp_checkpoint_files") != c.xtp {
			t.Errorf("%s: query names dm_db_xtp_checkpoint_files = %v, want %v", c.name, !c.xtp, c.xtp)
		}
	}
}

func TestSpaceReportFlagsUnreadPrecreatedFiles(t *testing.T) {
	// A memory-optimized filegroup exists (checkpoint pages non-NULL) but the
	// DMV was not read: unallocated leaves the precreated files out, and says so.
	row := []driver.Value{int64(100), int64(50), int64(40), int64(10), int64(20), nil, false,
		int64(60), int64(45), int64(30)}
	srv := spaceServer(t, &ServerInfo{VersionMajor: 16}, row)
	got, err := (&Database{server: srv, Name: "d"}).SpaceReport(context.Background())
	if err != nil {
		t.Fatalf("SpaceReport: %v", err)
	}
	if !got.XTPPrecreatedHidden || got.UnallocatedKB != 320 {
		t.Errorf("XTPPrecreatedHidden = %v, UnallocatedKB = %d; want true, 320", got.XTPPrecreatedHidden, got.UnallocatedKB)
	}
}

func TestTableSizesSplitsAsSpSpaceused(t *testing.T) {
	srv := spaceServer(t, nil,
		// reserved 10, used 7, data 5 pages
		[]driver.Value{int64(1), "dbo", "a", false, int64(42), int64(10), int64(7), int64(5)},
		// used below data pages and above reserved never go negative
		[]driver.Value{int64(2), "dbo", "b", false, int64(0), int64(4), int64(6), int64(8)},
		// memory-optimized: sizes not reported
		[]driver.Value{int64(3), "s", "m", true, int64(9), int64(10), int64(7), int64(5)},
	)
	got, err := (&Database{server: srv, Name: "d"}).TableSizes(context.Background())
	if err != nil {
		t.Fatalf("TableSizes: %v", err)
	}
	want := []TableSize{
		{ObjectID: 1, Schema: "dbo", Name: "a", Rows: 42, ReservedKB: 80, DataKB: 40, IndexKB: 16, UnusedKB: 24},
		{ObjectID: 2, Schema: "dbo", Name: "b", ReservedKB: 32, DataKB: 64, IndexKB: 0, UnusedKB: 0},
		{ObjectID: 3, Schema: "s", Name: "m", Rows: 9, IsMemoryOptimized: true},
	}
	if len(got) != len(want) {
		t.Fatalf("TableSizes returned %d rows, want %d", len(got), len(want))
	}
	for i := range want {
		if *got[i] != want[i] {
			t.Errorf("row %d = %+v, want %+v", i, *got[i], want[i])
		}
	}
}

func TestFileVolumesReportsHiddenWithoutPermission(t *testing.T) {
	srv := spaceServer(t, nil, []driver.Value{false, nil, nil, nil, nil, nil, nil, nil})
	vols, visible, err := srv.FileVolumes(context.Background())
	if err != nil || visible || len(vols) != 0 {
		t.Errorf("FileVolumes = %v, %v, %v; want no rows, hidden, no error", vols, visible, err)
	}

	srv = spaceServer(t, nil,
		[]driver.Value{true, int64(1), int64(1), `C:\d\master.mdf`, `C:\`, "OS", int64(1000), int64(400)},
		[]driver.Value{true, int64(2), int64(2), `D:\l\templog.ldf`, nil, nil, int64(500), int64(100)},
	)
	vols, visible, err = srv.FileVolumes(context.Background())
	if err != nil || !visible {
		t.Fatalf("FileVolumes: visible %v, err %v", visible, err)
	}
	want := []FileVolume{
		{DatabaseID: 1, FileID: 1, PhysicalName: `C:\d\master.mdf`, MountPoint: `C:\`, VolumeName: "OS", TotalBytes: 1000, AvailableBytes: 400},
		{DatabaseID: 2, FileID: 2, PhysicalName: `D:\l\templog.ldf`, TotalBytes: 500, AvailableBytes: 100},
	}
	if len(vols) != len(want) || vols[0] != want[0] || vols[1] != want[1] {
		t.Errorf("FileVolumes = %+v, want %+v", vols, want)
	}
}

func TestVolumePlausible(t *testing.T) {
	for _, c := range []struct {
		total, avail int64
		want         bool
	}{
		{1000, 400, true},
		{1000, 1000, true},
		{0, 0, false},
		{192, 65344, false}, // what an Azure Managed Instance reports for C:\
	} {
		if got := (FileVolume{TotalBytes: c.total, AvailableBytes: c.avail}).Plausible(); got != c.want {
			t.Errorf("FileVolume{%d, %d}.Plausible() = %v, want %v", c.total, c.avail, got, c.want)
		}
		if got := (DiskVolumeInfo{TotalMB: float64(c.total), AvailableMB: float64(c.avail)}).Plausible(); got != c.want {
			t.Errorf("DiskVolumeInfo{%d, %d}.Plausible() = %v, want %v", c.total, c.avail, got, c.want)
		}
	}
}
