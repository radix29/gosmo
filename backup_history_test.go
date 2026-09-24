package gosmo

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"io"
	"slices"
	"strings"
	"testing"
	"time"
)

// -- fake driver: replays msdb rows for BackupHistory, NULLs included,
// without applying the query's own ISNULLs -------------------------------

type histDriver struct{ rows [][]driver.Value }

func (d *histDriver) Open(string) (driver.Conn, error) { return &histConn{d: d}, nil }

type histConn struct{ d *histDriver }

func (c *histConn) Prepare(string) (driver.Stmt, error) { return nil, driver.ErrSkip }
func (c *histConn) Close() error                        { return nil }
func (c *histConn) Begin() (driver.Tx, error)           { return nil, driver.ErrSkip }

func (c *histConn) QueryContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	return &histRows{rows: c.d.rows}, nil
}

type histRows struct{ rows [][]driver.Value }

func (r *histRows) Columns() []string {
	return []string{"database_name", "name", "description", "type",
		"backup_start_date", "backup_finish_date", "backup_size",
		"physical_device_name", "user_name", "server_name",
		"database_version", "compatibility_level",
		"position", "backup_set_id", "media_set_id", "mirror_count"}
}
func (r *histRows) Close() error { return nil }
func (r *histRows) Next(dest []driver.Value) error {
	if len(r.rows) == 0 {
		return io.EOF
	}
	copy(dest, r.rows[0])
	r.rows = r.rows[1:]
	return nil
}

// A Managed Instance's automated backups leave physical_device_name, user_name
// and server_name NULL, and every other column here is nullable too. Before
// the fix that was "sql: Scan error on column index 7" — a whole failed read,
// which is what Database Properties' General page showed instead of anything.
// The fake deliberately does not apply the query's ISNULLs, so this pins the
// scan destinations rather than the SQL.
func TestBackupHistoryScansNullColumns(t *testing.T) {
	finished := time.Date(2026, 9, 8, 22, 5, 0, 0, time.UTC)
	drv := &histDriver{rows: [][]driver.Value{
		// Everything NULL — the automated-backup row.
		{nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil},
		// A populated row, to prove the values still arrive.
		{"GoTest01", "set", "desc", "D", finished, finished, int64(2048),
			"https://example.blob.core.windows.net/b/GoTest01.bak", "testgo", "t-qmi-01",
			int64(957), int64(170), int64(1), int64(9), int64(4), int64(1)},
	}}
	sql.Register("fakebackuphistory", drv)

	db, err := sql.Open("fakebackuphistory", "")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	hist, err := (&Server{db: db}).BackupHistory(context.Background(), "GoTest01")
	if err != nil {
		t.Fatalf("BackupHistory: %v", err)
	}
	if len(hist) != 2 {
		t.Fatalf("got %d rows, want 2", len(hist))
	}

	null := hist[0]
	if null.DeviceName != "" || null.UserName != "" || null.ServerName != "" ||
		null.DatabaseName != "" || null.BackupSetName != "" || null.Description != "" {
		t.Errorf("all-NULL row did not come back with empty strings: %+v", *null)
	}
	if !null.BackupStart.IsZero() || !null.BackupFinish.IsZero() {
		t.Errorf("all-NULL row dates = %v/%v, want zero", null.BackupStart, null.BackupFinish)
	}
	if null.BackupSize != 0 || null.DatabaseVersion != 0 || null.CompatibilityLevel != 0 {
		t.Errorf("all-NULL row numbers not zero: %+v", *null)
	}
	// A NULL type must not masquerade as a full backup.
	if null.BackupType != "" {
		t.Errorf("all-NULL row BackupType = %q, want empty", null.BackupType)
	}

	got := hist[1]
	if got.DatabaseName != "GoTest01" || got.UserName != "testgo" || got.ServerName != "t-qmi-01" ||
		got.BackupSize != 2048 || got.DatabaseVersion != 957 || got.CompatibilityLevel != 170 ||
		got.BackupType != BackupActionDatabase || !got.BackupFinish.Equal(finished) {
		t.Errorf("populated row lost values: %+v", *got)
	}
}

// The Go-side Null destinations are only half the fix; the query wraps the
// same columns so the server never sends a NULL in the first place. Both
// halves are load-bearing — see BackupHistory's doc comment.
func TestBackupHistoryQueryWrapsEveryNullableColumn(t *testing.T) {
	list := selectList(t, backupHistorySelect)
	exprs := selectExprs(t, list)
	if len(exprs) != 16 {
		t.Fatalf("select list has %d expressions, want 16 — the scan passes 16 destinations", len(exprs))
	}
	for i, e := range exprs {
		// The two dates are deliberately unwrapped: a zero Time says
		// "still running" better than a substituted date would.
		if strings.Contains(e, "backup_start_date") || strings.Contains(e, "backup_finish_date") {
			continue
		}
		if !strings.HasPrefix(e, "ISNULL(") {
			t.Errorf("expression %d is %q, which is not ISNULL-wrapped", i, e)
		}
	}
}

// backupmediafamily holds one row per media family, so a set striped over two
// files came back as two history entries, and a restore from either named one
// stripe — which the server refuses: "The media set has 2 media families but
// only 1 are provided". The read now folds the families into one entry per
// set, in family order, and keeps two sets appended to one file (NOINIT)
// apart, each with its own Position.
func TestBackupHistoryGroupsStripedFamilies(t *testing.T) {
	at := func(m int) time.Time { return time.Date(2026, 9, 24, 10, m, 0, 0, time.UTC) }
	row := func(finish time.Time, device string, position, setID, mediaSetID int64) []driver.Value {
		return []driver.Value{"GoTest01", "", "", "D", finish, finish, int64(1024),
			device, "sa", "srv", int64(957), int64(160), position, setID, mediaSetID, int64(1)}
	}
	drv := &histDriver{rows: [][]driver.Value{
		// Newest: striped over two files.
		row(at(30), `/b/s1.bak`, 1, 12, 7),
		row(at(30), `/b/s2.bak`, 1, 12, 7),
		// Two sets appended to one file.
		row(at(20), `/b/app.bak`, 2, 11, 6),
		row(at(10), `/b/app.bak`, 1, 10, 6),
	}}
	sql.Register("fakebackuphistorystriped", drv)
	db, err := sql.Open("fakebackuphistorystriped", "")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	hist, err := (&Server{db: db}).BackupHistory(context.Background(), "GoTest01")
	if err != nil {
		t.Fatalf("BackupHistory: %v", err)
	}
	if len(hist) != 3 {
		t.Fatalf("got %d entries, want 3 (one per backup set): %+v", len(hist), hist)
	}
	striped := hist[0]
	if !slices.Equal(striped.Devices, []string{`/b/s1.bak`, `/b/s2.bak`}) || striped.DeviceName != `/b/s1.bak` {
		t.Errorf("striped set Devices = %q, DeviceName = %q; want both stripes in order, the first as DeviceName",
			striped.Devices, striped.DeviceName)
	}
	if striped.BackupSetID != 12 || striped.MediaSetID != 7 || striped.MirrorCount != 1 {
		t.Errorf("striped set keys = %d/%d/%d, want 12/7/1", striped.BackupSetID, striped.MediaSetID, striped.MirrorCount)
	}
	for i, want := range []int{2, 1} {
		b := hist[i+1]
		if b.Position != want || len(b.Devices) != 1 || b.Devices[0] != `/b/app.bak` {
			t.Errorf("appended set %d: Position %d, Devices %q; want Position %d on app.bak alone", i, b.Position, b.Devices, want)
		}
	}
}
