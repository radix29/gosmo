package gosmo

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"strings"
	"testing"
)

// -- fake driver answering loadInfo's single row -----------------------------

type fakeInfoDriver struct{}

func (fakeInfoDriver) Open(string) (driver.Conn, error) { return &fakeInfoConn{}, nil }

type fakeInfoConn struct{}

func (c *fakeInfoConn) Prepare(string) (driver.Stmt, error) { return nil, driver.ErrSkip }
func (c *fakeInfoConn) Close() error                        { return nil }
func (c *fakeInfoConn) Begin() (driver.Tx, error)           { return nil, driver.ErrSkip }

func (c *fakeInfoConn) QueryContext(_ context.Context, q string, _ []driver.NamedValue) (driver.Rows, error) {
	return fakeInfoAnswer(q, nil)
}

// fakeInfoAnswer answers either half of loadInfo's two statements. sysInfoErr,
// when non-nil, is what the sys.dm_os_sys_info half fails with — which is how
// a login without VIEW SERVER STATE is reproduced without a server.
func fakeInfoAnswer(q string, sysInfoErr error) (driver.Rows, error) {
	if strings.Contains(q, "dm_os_sys_info") {
		if sysInfoErr != nil {
			return nil, sysInfoErr
		}
		return &fakeSysInfoRows{}, nil
	}
	return &fakeInfoRows{}, nil
}

// fakeInfoRows yields the thirteen SERVERPROPERTY/@@VERSION columns loadInfo
// scans, once.
type fakeInfoRows struct{ done bool }

func (r *fakeInfoRows) Columns() []string { return make([]string, 13) }
func (r *fakeInfoRows) Close() error      { return nil }
func (r *fakeInfoRows) Next(dest []driver.Value) error {
	if r.done {
		return io.EOF
	}
	r.done = true
	vals := []driver.Value{
		"FAKE\\SQL", "Developer Edition (64-bit)", "16.0.4085.2", "RTM", "SQL_Latin1_General_CP1_CI_AS",
		int64(0), int64(1), int64(0), int64(3),
		"Microsoft SQL Server 2022 ... on Linux (Ubuntu 22.04.3 LTS)",
		`C:\Data`, `C:\Log`, `C:\Backup`,
	}
	copy(dest, vals)
	return nil
}

// fakeSysInfoRows yields the two sys.dm_os_sys_info columns, once.
type fakeSysInfoRows struct{ done bool }

func (r *fakeSysInfoRows) Columns() []string { return make([]string, 2) }
func (r *fakeSysInfoRows) Close() error      { return nil }
func (r *fakeSysInfoRows) Next(dest []driver.Value) error {
	if r.done {
		return io.EOF
	}
	r.done = true
	copy(dest, []driver.Value{int64(16384), int64(8)})
	return nil
}

func init() { sql.Register("fakeinfo", fakeInfoDriver{}) }

// TestNewServerWrapsACallerSuppliedPool is the seam itself: a *Server built
// over a pool the caller opened, with no network connection anywhere. It is
// what makes an application's database layer testable — before it, the only
// way to obtain a *Server was Connect, so every caller of one needed a live
// instance.
func TestNewServerWrapsACallerSuppliedPool(t *testing.T) {
	pool, err := sql.Open("fakeinfo", "")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer pool.Close()

	s, err := NewServer(context.Background(), pool)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	if s.DB() != pool {
		t.Error("DB() is not the pool that was passed in")
	}
	// The metadata has to be loaded, not left nil: Name() and every caller
	// of Info() dereferences it, so a Server that skipped the load would
	// panic on first use rather than fail here.
	if got := s.Name(); got != `FAKE\SQL` {
		t.Errorf("Name() = %q, want %q", got, `FAKE\SQL`)
	}
	if got := s.Info().EngineEdition; got != 3 {
		t.Errorf("EngineEdition = %d, want 3", got)
	}
	if got := s.Info().VersionMajor; got != 16 {
		t.Errorf("VersionMajor = %d, want 16", got)
	}
	if !s.Info().IsHADREnabled {
		t.Error("IsHADREnabled = false, want true")
	}
	if s.Info().Platform == "" {
		t.Error("Platform is empty: @@VERSION was not parsed")
	}
}

func TestNewServerRejectsANilPool(t *testing.T) {
	if _, err := NewServer(context.Background(), nil); err == nil {
		t.Error("nil pool: want an error, got nil")
	}
}

// -- fake driver denying only the sys.dm_os_sys_info half --------------------

type denySysInfoDriver struct{}

func (denySysInfoDriver) Open(string) (driver.Conn, error) { return &denySysInfoConn{}, nil }

type denySysInfoConn struct{}

func (c *denySysInfoConn) Prepare(string) (driver.Stmt, error) { return nil, driver.ErrSkip }
func (c *denySysInfoConn) Close() error                        { return nil }
func (c *denySysInfoConn) Begin() (driver.Tx, error)           { return nil, driver.ErrSkip }

// errNoServerState stands in for SQL Server's Msg 297/300 refusal.
var errNoServerState = errors.New("mssql: The user does not have permission to perform this action.")

func (c *denySysInfoConn) QueryContext(_ context.Context, q string, _ []driver.NamedValue) (driver.Rows, error) {
	return fakeInfoAnswer(q, errNoServerState)
}

func init() { sql.Register("denysysinfo", denySysInfoDriver{}) }

// TestServerInfoLoadsWithoutViewServerState pins the split in loadInfo. Both
// halves were one statement until 2026-08-25, and because the DMV's permission
// check failed the whole SELECT, a login without VIEW SERVER STATE could not
// connect at all — the error was "gosmo: load server info: mssql: The user
// does not have permission to perform this action", with every SERVERPROPERTY
// value in the same statement lost with it.
//
// Rejoining the two queries fails this test at NewServer.
func TestServerInfoLoadsWithoutViewServerState(t *testing.T) {
	pool, err := sql.Open("denysysinfo", "")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer pool.Close()

	s, err := NewServer(context.Background(), pool)
	if err != nil {
		t.Fatalf("NewServer with sys.dm_os_sys_info denied: %v", err)
	}

	info := s.Info()
	// Everything the public half supplies must survive the denied half.
	if got := info.Name; got != `FAKE\SQL` {
		t.Errorf("Name = %q, want %q", got, `FAKE\SQL`)
	}
	if got := info.ProductVersion; got != "16.0.4085.2" {
		t.Errorf("ProductVersion = %q, want %q", got, "16.0.4085.2")
	}
	if got := info.DefaultDataPath; got != `C:\Data` {
		t.Errorf("DefaultDataPath = %q, want %q", got, `C:\Data`)
	}

	// And the denied half must say so rather than report zeros as facts.
	if !info.SysInfoUnavailable {
		t.Error("SysInfoUnavailable = false, want true: a caller cannot tell 0 CPUs from unknown")
	}
	if info.LogicalCPUCount != 0 || info.PhysicalMemoryMB != 0 {
		t.Errorf("CPU/memory = %d/%d, want 0/0 alongside SysInfoUnavailable",
			info.LogicalCPUCount, info.PhysicalMemoryMB)
	}
}

// TestServerInfoReportsSysInfoWhenReadable is the other half of the pair: with
// the DMV readable, SysInfoUnavailable must stay false and the values arrive.
func TestServerInfoReportsSysInfoWhenReadable(t *testing.T) {
	pool, err := sql.Open("fakeinfo", "")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer pool.Close()

	s, err := NewServer(context.Background(), pool)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	info := s.Info()
	if info.SysInfoUnavailable {
		t.Error("SysInfoUnavailable = true, want false")
	}
	if info.LogicalCPUCount != 8 {
		t.Errorf("LogicalCPUCount = %d, want 8", info.LogicalCPUCount)
	}
	if info.PhysicalMemoryMB != 16384 {
		t.Errorf("PhysicalMemoryMB = %d, want 16384", info.PhysicalMemoryMB)
	}
}
