package gosmo

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"strings"
	"sync"
	"testing"

	mssql "github.com/microsoft/go-mssqldb"
)

// contendedAlterDriver refuses the first fails ALTER DATABASE statements with
// Msg number (and the 5069 that trails it) and records every statement.
type contendedAlterDriver struct {
	mu     sync.Mutex
	number int32
	fails  int
	alters []string
}

func (d *contendedAlterDriver) Connect(context.Context) (driver.Conn, error) {
	return contendedAlterConn{d}, nil
}
func (d *contendedAlterDriver) Driver() driver.Driver { return nil }

type contendedAlterConn struct{ d *contendedAlterDriver }

func (c contendedAlterConn) Prepare(string) (driver.Stmt, error) { return nil, driver.ErrSkip }
func (c contendedAlterConn) Close() error                        { return nil }
func (c contendedAlterConn) Begin() (driver.Tx, error)           { return nil, driver.ErrSkip }
func (c contendedAlterConn) ExecContext(_ context.Context, q string, _ []driver.NamedValue) (driver.Result, error) {
	if !strings.Contains(q, "ALTER DATABASE") {
		return driver.ResultNoRows, nil
	}
	c.d.mu.Lock()
	defer c.d.mu.Unlock()
	c.d.alters = append(c.d.alters, q)
	if len(c.d.alters) <= c.d.fails {
		refusal := mssql.Error{Number: c.d.number, Class: 16, Message: "refused"}
		return nil, mssql.Error{Number: 5069, Class: 16, Message: "ALTER DATABASE statement failed.",
			All: []mssql.Error{refusal, {Number: 5069, Class: 16}}}
	}
	return driver.ResultNoRows, nil
}

// B28 (gossms): a session entering the database as SetUserAccess's ALTER runs
// refuses it with 924, 1205, 5061 or 5064. Those are re-run, up to
// userAccessAttempts in all, at DEADLOCK_PRIORITY HIGH; any other refusal is
// returned at once.
func TestSetUserAccessRetriesContention(t *testing.T) {
	for _, c := range []struct {
		name      string
		number    int32
		fails     int
		wantTries int
		wantErr   bool
	}{
		{"924 then success", 924, 2, 3, false},
		{"1205 then success", 1205, 1, 2, false},
		{"5061 then success", 5061, 1, 2, false},
		{"5064 every time", 5064, 99, userAccessAttempts, true},
		{"5011 not retried", 5011, 99, 1, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			d := &contendedAlterDriver{number: c.number, fails: c.fails}
			s := &Server{db: sql.OpenDB(d)}
			t.Cleanup(func() { s.db.Close() })
			err := s.DatabaseRef("Sales").SetUserAccess(t.Context(), UserAccessMulti)
			if (err != nil) != c.wantErr {
				t.Fatalf("err = %v, want error %v", err, c.wantErr)
			}
			if c.wantErr && !strings.Contains(err.Error(), "set user access MULTI_USER") {
				t.Errorf("err = %v, want the attempted operation named", err)
			}
			if len(d.alters) != c.wantTries {
				t.Errorf("tries = %d, want %d", len(d.alters), c.wantTries)
			}
			want := "SET DEADLOCK_PRIORITY HIGH;\nALTER DATABASE [Sales] SET MULTI_USER WITH ROLLBACK IMMEDIATE"
			for i, q := range d.alters {
				if q != want {
					t.Errorf("try %d ran %q, want %q", i+1, q, want)
				}
			}
		})
	}
}
