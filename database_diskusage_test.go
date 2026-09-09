package gosmo

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"io"
	"testing"
)

// The disk-usage row is seven anonymous floats, so a column added to the
// SELECT in the wrong place, or a Scan argument out of order, produces a
// perfectly valid DiskUsage describing the wrong things — index space read
// as unused, a log bar drawn from data-file figures. This driver answers
// with a distinct value per column so every field can be named.
type diskUsageDriver struct{}

func (diskUsageDriver) Open(string) (driver.Conn, error) { return &diskUsageConn{}, nil }

type diskUsageConn struct{}

func (*diskUsageConn) Prepare(string) (driver.Stmt, error) { return nil, driver.ErrSkip }
func (*diskUsageConn) Close() error                        { return nil }
func (*diskUsageConn) Begin() (driver.Tx, error)           { return nil, driver.ErrSkip }

func (*diskUsageConn) ExecContext(context.Context, string, []driver.NamedValue) (driver.Result, error) {
	return driver.ResultNoRows, nil // the USE
}

func (*diskUsageConn) QueryContext(context.Context, string, []driver.NamedValue) (driver.Rows, error) {
	return &diskUsageRows{}, nil
}

type diskUsageRows struct{ done bool }

func (r *diskUsageRows) Columns() []string {
	return []string{"data_files_mb", "log_files_mb", "unallocated_mb", "avail_log_mb",
		"data_mb", "index_mb", "unused_mb"}
}
func (r *diskUsageRows) Close() error { return nil }
func (r *diskUsageRows) Next(dest []driver.Value) error {
	if r.done {
		return io.EOF
	}
	r.done = true
	for i, v := range []float64{100, 40, 10, 4, 60, 20, 8} {
		dest[i] = v
	}
	return nil
}

func init() { sql.Register("fakediskusage", diskUsageDriver{}) }

func TestDiskUsageScansEveryColumnIntoTheRightField(t *testing.T) {
	db, err := sql.Open("fakediskusage", "")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	d := &Database{server: &Server{db: db}, name: "test"}
	got, err := d.DiskUsageContext(context.Background())
	if err != nil {
		t.Fatalf("DiskUsageContext: %v", err)
	}

	// LogUsedMB is the one derived field: the log files less what a shrink
	// could give back.
	want := DiskUsage{
		DataFilesMB: 100, LogFilesMB: 40,
		DataMB: 60, IndexMB: 20, UnusedMB: 8, UnallocatedMB: 10,
		LogUsedMB: 36, LogUnusedMB: 4,
	}
	if got != want {
		t.Errorf("DiskUsage = %+v, want %+v", got, want)
	}
}
