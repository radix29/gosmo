package gosmo

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"io"
	"testing"
)

// -- fake driver answering loadInfo's single row -----------------------------

type fakeInfoDriver struct{}

func (fakeInfoDriver) Open(string) (driver.Conn, error) { return &fakeInfoConn{}, nil }

type fakeInfoConn struct{}

func (c *fakeInfoConn) Prepare(string) (driver.Stmt, error) { return nil, driver.ErrSkip }
func (c *fakeInfoConn) Close() error                        { return nil }
func (c *fakeInfoConn) Begin() (driver.Tx, error)           { return nil, driver.ErrSkip }

func (c *fakeInfoConn) QueryContext(context.Context, string, []driver.NamedValue) (driver.Rows, error) {
	return &fakeInfoRows{}, nil
}

// fakeInfoRows yields the fifteen columns loadInfo scans, once.
type fakeInfoRows struct{ done bool }

func (r *fakeInfoRows) Columns() []string { return make([]string, 15) }
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
		int64(16384), int64(8),
		`C:\Data`, `C:\Log`, `C:\Backup`,
	}
	copy(dest, vals)
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
