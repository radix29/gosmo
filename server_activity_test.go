package gosmo

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
)

// activityReply is the answer to the one query whose text contains key: its
// columns and rows. Every value is distinct per column, so a SELECT list and
// a Scan out of step show as a wrong field rather than passing.
type activityReply struct {
	key  string
	cols []string
	rows [][]driver.Value
}

// activityConnector answers each query with the first reply whose key the
// query contains, and records the queries and their arguments.
type activityConnector struct {
	replies []activityReply

	mu    sync.Mutex
	query string
	args  []driver.NamedValue
}

func (c *activityConnector) Connect(context.Context) (driver.Conn, error) {
	return &activityConn{c: c}, nil
}
func (*activityConnector) Driver() driver.Driver { return nil }

type activityConn struct{ c *activityConnector }

func (*activityConn) Prepare(string) (driver.Stmt, error) { return nil, driver.ErrSkip }
func (*activityConn) Close() error                        { return nil }
func (*activityConn) Begin() (driver.Tx, error)           { return nil, driver.ErrSkip }

func (cn *activityConn) QueryContext(_ context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	cn.c.mu.Lock()
	cn.c.query, cn.c.args = q, args
	cn.c.mu.Unlock()
	for _, r := range cn.c.replies {
		if strings.Contains(q, r.key) {
			return &activityRows{cols: r.cols, rows: r.rows}, nil
		}
	}
	return nil, errors.New("gosmo_test: no reply scripted for " + q)
}

type activityRows struct {
	cols []string
	rows [][]driver.Value
}

func (r *activityRows) Columns() []string { return r.cols }
func (r *activityRows) Close() error      { return nil }
func (r *activityRows) Next(dest []driver.Value) error {
	if len(r.rows) == 0 {
		return io.EOF
	}
	copy(dest, r.rows[0])
	r.rows = r.rows[1:]
	return nil
}

func activityServer(t *testing.T, replies ...activityReply) (*Server, *activityConnector) {
	t.Helper()
	c := &activityConnector{replies: replies}
	db := sql.OpenDB(c)
	t.Cleanup(func() { db.Close() })
	return &Server{db: db}, c
}

func cols(n int) []string { return make([]string, n) }

func TestPerformanceCountersStripTheInstancePrefixAndFilterBoth(t *testing.T) {
	s, c := activityServer(t, activityReply{key: "dm_os_performance_counters", cols: cols(5), rows: [][]driver.Value{
		{"SQLServer:Buffer Manager", "Page life expectancy", "", int64(4200), int64(65792)},
		{"MSSQL$INST:Databases", "Transactions/sec", "_Total", int64(17), int64(272696576)},
		{"NoPrefix ", "x", "y", int64(1), int64(1073939712)},
	}})
	got, err := s.PerformanceCounters(t.Context(), []string{"Page life expectancy", "Transactions/sec"}, []string{"", "_Total"})
	if err != nil {
		t.Fatal(err)
	}
	want := []PerformanceCounter{
		{Object: "Buffer Manager", Counter: "Page life expectancy", Instance: "", Value: 4200, Type: CounterRawCount},
		{Object: "Databases", Counter: "Transactions/sec", Instance: "_Total", Value: 17, Type: CounterBulkCount},
		{Object: "NoPrefix", Counter: "x", Instance: "y", Value: 1, Type: CounterRawBase},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d counters, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("counter %d = %+v, want %+v", i, got[i], want[i])
		}
	}

	// Both lists go as parameters, in order: the names are a caller's, not
	// constants this package could inline safely.
	if !strings.Contains(c.query, "RTRIM(counter_name) IN (@p1, @p2)") ||
		!strings.Contains(c.query, "RTRIM(instance_name) IN (@p3, @p4)") {
		t.Errorf("filter not as expected:\n%s", c.query)
	}
	var args []any
	for _, a := range c.args {
		args = append(args, a.Value)
	}
	if len(args) != 4 || args[0] != "Page life expectancy" || args[2] != "" || args[3] != "_Total" {
		t.Errorf("args = %q", args)
	}
}

func TestPerformanceCountersWithNoFilterReadEveryRow(t *testing.T) {
	s, c := activityServer(t, activityReply{key: "dm_os_performance_counters", cols: cols(5)})
	if _, err := s.PerformanceCounters(t.Context(), nil, nil); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(c.query, " IN (") || len(c.args) != 0 {
		t.Errorf("an empty filter still filtered: %s %v", c.query, c.args)
	}
}

func TestWaitStatsScanEveryColumn(t *testing.T) {
	s, _ := activityServer(t, activityReply{key: "dm_os_wait_stats", cols: cols(5), rows: [][]driver.Value{
		{"PAGEIOLATCH_SH", int64(12), int64(900), int64(30), int64(77)},
	}})
	got, err := s.WaitStats(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	want := WaitStat{WaitType: "PAGEIOLATCH_SH", WaitingTasks: 12, WaitTimeMs: 900, SignalWaitTimeMs: 30, MaxWaitTimeMs: 77}
	if len(got) != 1 || got[0] != want {
		t.Errorf("WaitStats = %+v, want [%+v]", got, want)
	}
}

func TestFileIOStatsScanEveryColumn(t *testing.T) {
	s, _ := activityServer(t, activityReply{key: "dm_io_virtual_file_stats", cols: cols(10), rows: [][]driver.Value{
		{int64(5), int64(2), "", int64(1), int64(11), int64(12), int64(13), int64(14), int64(15), int64(16)},
	}})
	got, err := s.FileIOStats(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	want := FileIOStat{DatabaseID: 5, FileID: 2, Database: "", IsLog: true,
		Reads: 11, BytesRead: 12, IOStallReadMs: 13, Writes: 14, BytesWritten: 15, IOStallWriteMs: 16}
	if len(got) != 1 || got[0] != want {
		t.Errorf("FileIOStats = %+v, want [%+v]", got, want)
	}
}

func TestMemoryClerksScanEveryColumn(t *testing.T) {
	s, _ := activityServer(t, activityReply{key: "dm_os_memory_clerks", cols: cols(2), rows: [][]driver.Value{
		{"MEMORYCLERK_SQLBUFFERPOOL", float64(1024)},
	}})
	got, err := s.MemoryClerks(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != (MemoryClerk{Type: "MEMORYCLERK_SQLBUFFERPOOL", MB: 1024}) {
		t.Errorf("MemoryClerks = %+v", got)
	}
}

func TestSchedulersScanTheLoadColumns(t *testing.T) {
	s, _ := activityServer(t, activityReply{key: "dm_os_schedulers", cols: cols(10), rows: [][]driver.Value{
		{int64(3), int64(4), int64(1), int64(0), true, int64(2), int64(30), int64(40), int64(5), int64(12)},
	}})
	got, err := s.Schedulers(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	want := Scheduler{ID: 3, CPUID: 4, NUMANode: 1, ProcessorGroup: 0, IsOnline: true,
		RunnableTasks: 2, CurrentTasks: 30, ActiveWorkers: 40, WorkQueue: 5, LoadFactor: 12}
	if len(got) != 1 || got[0] != want {
		t.Errorf("Schedulers = %+v, want [%+v]", got, want)
	}
}

func TestRequestActivityScansEveryColumn(t *testing.T) {
	s, _ := activityServer(t, activityReply{key: "dm_exec_requests", cols: cols(5), rows: [][]driver.Value{
		{int64(17), int64(5), int64(2), int64(1), int64(3)},
	}})
	got, err := s.RequestActivity(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	want := RequestActivity{UserSessions: 17, ActiveRequests: 5, RunnableRequests: 2, SuspendedRequests: 1, BlockedRequests: 3}
	if got != want {
		t.Errorf("RequestActivity = %+v, want %+v", got, want)
	}
}

func TestHostCPUReadsTheNewestRecordOrZero(t *testing.T) {
	s, _ := activityServer(t, activityReply{key: "RING_BUFFER_SCHEDULER_MONITOR", cols: cols(2), rows: [][]driver.Value{
		{int64(63), int64(11)},
	}})
	got, err := s.HostCPU(t.Context())
	if err != nil || got != (HostCPU{SQLServerPercent: 63, OtherPercent: 11}) {
		t.Errorf("HostCPU = %+v, %v", got, err)
	}

	// A freshly started instance has no record yet: a zero reading, not an
	// error, or an activity monitor opened in the first minute fails.
	s, _ = activityServer(t, activityReply{key: "RING_BUFFER_SCHEDULER_MONITOR", cols: cols(2)})
	got, err = s.HostCPU(t.Context())
	if err != nil || got != (HostCPU{}) {
		t.Errorf("HostCPU with no record = %+v, %v; want zero, nil", got, err)
	}
}

func TestTempDBReadsConvertPagesToMB(t *testing.T) {
	s, _ := activityServer(t,
		activityReply{key: "SUM(CAST(total_page_count", cols: cols(6), rows: [][]driver.Value{
			{int64(1280), int64(640), int64(128), int64(256), int64(64), int64(32)},
		}},
		activityReply{key: "tempdb.sys.database_files", cols: cols(7), rows: [][]driver.Value{
			{int64(1), "tempdev", "ROWS", int64(1024), int64(256), int64(8192), false},
			{int64(2), "templog", "LOG", int64(512), int64(0), int64(10), true},
		}},
		activityReply{key: "tempdb.sys.objects", cols: cols(5), rows: [][]driver.Value{
			{int64(1), int64(2), int64(128), int64(64), int64(99)},
		}},
		activityReply{key: "dm_db_session_space_usage", cols: cols(6), rows: [][]driver.Value{
			{int64(55), "h", "p", "l", int64(256), int64(-5)},
		}},
	)
	ctx := t.Context()

	space, err := s.TempDBSpace(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if space != (TempDBSpace{TotalMB: 10, FreeMB: 5, VersionStoreMB: 1, UserObjectMB: 2, InternalObjectMB: 0.5, MixedExtentMB: 0.25}) {
		t.Errorf("TempDBSpace = %+v", space)
	}

	files, err := s.TempDBFiles(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 ||
		files[0] != (TempDBFile{FileID: 1, Name: "tempdev", Type: "ROWS", SizeMB: 8, UsedMB: 2, GrowthMB: 64}) ||
		files[1] != (TempDBFile{FileID: 2, Name: "templog", Type: "LOG", SizeMB: 4, GrowthMB: 10, PercentGrowth: true}) {
		t.Errorf("TempDBFiles = %+v (a percentage growth must stay a percentage)", files)
	}

	objs, err := s.TempDBObjects(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(objs) != 1 || objs[0] != (TempDBObjects{Kind: TempDBGlobalTemp, Count: 2, ReservedMB: 1, UsedMB: 0.5, Rows: 99}) {
		t.Errorf("TempDBObjects = %+v", objs)
	}

	sess, err := s.TempDBSessions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(sess) != 1 || sess[0] != (TempDBSession{SessionID: 55, Host: "h", Program: "p", Login: "l", UserMB: 2, InternalMB: 0, TotalMB: 2}) {
		t.Errorf("TempDBSessions = %+v (a negative half reads 0)", sess)
	}
}

func TestHasViewServerState(t *testing.T) {
	for _, v := range []int64{0, 1} {
		s, _ := activityServer(t, activityReply{key: "HAS_PERMS_BY_NAME", cols: cols(1), rows: [][]driver.Value{{v}}})
		ok, err := s.HasViewServerState(t.Context())
		if err != nil || ok != (v == 1) {
			t.Errorf("answer %d: HasViewServerState = %v, %v", v, ok, err)
		}
	}
}
