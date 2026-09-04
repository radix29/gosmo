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

// A5: SERVERPROPERTY('InstanceDefaultBackupPath') is SQL Server 2019 and
// later, so loadInfo falls back to xp_instance_regread when it comes back
// NULL. These fakes answer loadInfo with a NULL backup path and control what
// the registry read does.

type backupPathDriver struct{}

// backupPathCfg configures the next connection: the platform @@VERSION
// reports, the registry read's answer, and whether it fails outright. Only one
// test runs against it at a time.
var backupPathCfg struct {
	platform  string
	propValue any // SERVERPROPERTY('InstanceDefaultBackupPath'); nil is a pre-2019 instance
	regValue  any // string, or nil for a NULL registry value
	regErr    error
	regReads  int
}

func (backupPathDriver) Open(string) (driver.Conn, error) { return backupPathConn{}, nil }

type backupPathConn struct{}

func (backupPathConn) Prepare(string) (driver.Stmt, error) { return nil, driver.ErrSkip }
func (backupPathConn) Close() error                        { return nil }
func (backupPathConn) Begin() (driver.Tx, error)           { return nil, driver.ErrSkip }

func (backupPathConn) QueryContext(_ context.Context, q string, _ []driver.NamedValue) (driver.Rows, error) {
	switch {
	case strings.Contains(q, "xp_instance_regread"):
		backupPathCfg.regReads++
		if backupPathCfg.regErr != nil {
			return nil, backupPathCfg.regErr
		}
		return &oneRow{vals: []driver.Value{backupPathCfg.regValue}}, nil
	case strings.Contains(q, "dm_os_sys_info"):
		return &oneRow{vals: []driver.Value{int64(16384), int64(8)}}, nil
	default:
		return &oneRow{vals: []driver.Value{
			`FAKE\SQL`, "Developer Edition (64-bit)", "14.0.2120.1", "RTM", "SQL_Latin1_General_CP1_CI_AS",
			int64(0), int64(0), int64(0), int64(3),
			"Microsoft SQL Server 2017 ... (64-bit) on " + backupPathCfg.platform + " 10 Pro",
			`C:\Data`, `C:\Log`, backupPathCfg.propValue,
		}}, nil
	}
}

// oneRow yields vals once, then EOF.
type oneRow struct {
	vals []driver.Value
	done bool
}

func (r *oneRow) Columns() []string { return make([]string, len(r.vals)) }
func (r *oneRow) Close() error      { return nil }
func (r *oneRow) Next(dest []driver.Value) error {
	if r.done {
		return io.EOF
	}
	r.done = true
	copy(dest, r.vals)
	return nil
}

func init() { sql.Register("backuppath", backupPathDriver{}) }

func serverWithBackupPath(t *testing.T, platform string, regValue any, regErr error) *Server {
	t.Helper()
	backupPathCfg.platform, backupPathCfg.regValue = platform, regValue
	backupPathCfg.regErr, backupPathCfg.regReads = regErr, 0
	backupPathCfg.propValue = nil // a pre-2019 instance unless a test says otherwise

	pool, err := sql.Open("backuppath", "")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { pool.Close() })

	s, err := NewServer(context.Background(), pool)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	return s
}

// The whole point: on a Windows instance whose SERVERPROPERTY is NULL, the
// registry supplies the path. Everything that defaults a backup location came
// up blank on 2017 and older without this.
func TestDefaultBackupPathFallsBackToTheRegistryOnWindows(t *testing.T) {
	const want = `C:\Program Files\Microsoft SQL Server\MSSQL14.SQL2017\MSSQL\Backup`
	s := serverWithBackupPath(t, "Windows", want, nil)

	if got := s.Info().DefaultBackupPath; got != want {
		t.Errorf("DefaultBackupPath = %q, want %q", got, want)
	}
	if backupPathCfg.regReads != 1 {
		t.Errorf("registry read ran %d times, want 1", backupPathCfg.regReads)
	}
}

// There is no registry on Linux, and every caller copes with an empty path.
func TestDefaultBackupPathDoesNotReadTheRegistryOnLinux(t *testing.T) {
	s := serverWithBackupPath(t, "Linux", `C:\never`, nil)

	if got := s.Info().DefaultBackupPath; got != "" {
		t.Errorf("DefaultBackupPath = %q on Linux, want empty", got)
	}
	if backupPathCfg.regReads != 0 {
		t.Errorf("registry read ran %d times on Linux, want 0", backupPathCfg.regReads)
	}
}

// loadInfo is on Connect's path, so a login without the rights to run
// xp_instance_regread must still get a connection — with an empty path, not an
// error.
func TestDefaultBackupPathSurvivesARegistryReadFailure(t *testing.T) {
	s := serverWithBackupPath(t, "Windows", nil, errors.New("mssql: The EXECUTE permission was denied on the object 'xp_instance_regread'."))

	if got := s.Info().DefaultBackupPath; got != "" {
		t.Errorf("DefaultBackupPath = %q, want empty", got)
	}
	if s.Info().VersionMajor != 14 {
		t.Errorf("VersionMajor = %d, want 14 — the rest of ServerInfo must survive too", s.Info().VersionMajor)
	}
}

// A NULL registry value is not an error either.
func TestDefaultBackupPathAcceptsANullRegistryValue(t *testing.T) {
	s := serverWithBackupPath(t, "Windows", nil, nil)

	if got := s.Info().DefaultBackupPath; got != "" {
		t.Errorf("DefaultBackupPath = %q, want empty", got)
	}
}

// 2019 and later answer the SERVERPROPERTY, and that answer must stand
// untouched — the fallback is for the NULL case only, so the registry is not
// even read.
func TestDefaultBackupPathPrefersTheServerProperty(t *testing.T) {
	const want = `C:\Program Files\Microsoft SQL Server\MSSQL17.MSSQLSERVER\MSSQL\Backup`
	backupPathCfg.propValue = want
	defer func() { backupPathCfg.propValue = nil }()

	backupPathCfg.platform, backupPathCfg.regValue = "Windows", `C:\registry`
	backupPathCfg.regErr, backupPathCfg.regReads = nil, 0

	pool, err := sql.Open("backuppath", "")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer pool.Close()
	s, err := NewServer(context.Background(), pool)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	if got := s.Info().DefaultBackupPath; got != want {
		t.Errorf("DefaultBackupPath = %q, want the SERVERPROPERTY value %q", got, want)
	}
	if backupPathCfg.regReads != 0 {
		t.Errorf("registry read ran %d times when the SERVERPROPERTY answered, want 0", backupPathCfg.regReads)
	}
}
