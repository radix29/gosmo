package gosmo

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"io"
	"strings"
	"testing"
)

// defaultPathsDriver answers DefaultPaths' SERVERPROPERTY read with a data
// and log directory and a NULL backup one — what SQL Server 2017 and older
// return — and the registry read that fills the gap with its own directory.
type defaultPathsDriver struct{}

func (defaultPathsDriver) Open(string) (driver.Conn, error) { return &defaultPathsConn{}, nil }

type defaultPathsConn struct{}

func (*defaultPathsConn) Prepare(string) (driver.Stmt, error) { return nil, driver.ErrSkip }
func (*defaultPathsConn) Close() error                        { return nil }
func (*defaultPathsConn) Begin() (driver.Tx, error)           { return nil, driver.ErrSkip }

func (*defaultPathsConn) QueryContext(_ context.Context, q string, _ []driver.NamedValue) (driver.Rows, error) {
	if strings.Contains(q, "xp_instance_regread") {
		return &defaultPathsRows{vals: []driver.Value{`R:\Backup`}}, nil
	}
	return &defaultPathsRows{vals: []driver.Value{`D:\Data\`, `L:\Log\`, nil}}, nil
}

type defaultPathsRows struct {
	vals []driver.Value
	done bool
}

func (r *defaultPathsRows) Columns() []string { return make([]string, len(r.vals)) }
func (r *defaultPathsRows) Close() error      { return nil }
func (r *defaultPathsRows) Next(dest []driver.Value) error {
	if r.done {
		return io.EOF
	}
	r.done = true
	copy(dest, r.vals)
	return nil
}

func init() { sql.Register("fakedefaultpaths", defaultPathsDriver{}) }

// DefaultPaths reads the server now; Info keeps what connect read. A default
// moved since connect is exactly the difference, and a restore relocating
// files by Info's copy would send them to the old directory.
func TestDefaultPathsReadsTheServerNotTheSnapshot(t *testing.T) {
	db, err := sql.Open("fakedefaultpaths", "")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()
	snapshot := &ServerInfo{Platform: "Windows", DefaultDataPath: `C:\Old\`, DefaultLogPath: `C:\Old\`}
	s := &Server{db: db, info: snapshot}

	got, err := s.DefaultPaths(context.Background())
	if err != nil {
		t.Fatalf("DefaultPaths: %v", err)
	}
	want := DefaultPaths{Data: `D:\Data\`, Log: `L:\Log\`, Backup: `R:\Backup`}
	if got != want {
		t.Errorf("DefaultPaths = %+v, want %+v", got, want)
	}
	if s.Info().DefaultDataPath != `C:\Old\` {
		t.Errorf("Info().DefaultDataPath = %q; DefaultPaths must not rewrite the snapshot", s.Info().DefaultDataPath)
	}
}
