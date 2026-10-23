package gosmo

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// -- a driver that serves the reads Login.MapToDatabase and
// UnmapFromDatabase make before they write ---------------------------------
//
// WithScript cannot test these two: each reads the catalog to decide what to
// write, and under a script nothing ran, so there is nothing to read back.
// This driver answers those reads from canned rows and records every
// statement with the database its connection was in when it ran, so a test
// sees both the write's text and where it went.

type mapStmt struct {
	db   string // the connection's database when q ran; "" before any USE
	q    string
	args []driver.NamedValue
}

type mapRecorder struct {
	mu    sync.Mutex
	stmts []mapStmt

	databases map[string]bool   // names DatabaseByName finds
	sids      map[string][]byte // login name → SID, for a LoginRef's lookup
	// mappings answers userMappingsQuery: rows of (DB_NAME(), principal_id,
	// user name, default schema, role name or nil), keyed by database.
	mappings map[string][][]driver.Value
}

var mapLog mapRecorder

type mapDriver struct{}

func (mapDriver) Open(string) (driver.Conn, error) { return &mapConn{}, nil }

func init() { sql.Register("loginmap", mapDriver{}) }

// mapConn tracks its own current database, as a server session does: a USE
// on one pooled connection says nothing about another.
type mapConn struct{ db string }

func (c *mapConn) Prepare(string) (driver.Stmt, error) { return nil, driver.ErrSkip }
func (c *mapConn) Close() error                        { return nil }
func (c *mapConn) Begin() (driver.Tx, error)           { return nil, driver.ErrSkip }

// enter applies q's USE, bare or batched, and returns the statement behind it.
func (c *mapConn) enter(q string) string {
	if use, rest, ok := splitUseBatch(q); ok {
		c.db = strings.TrimPrefix(use, "USE ")
		return rest
	}
	if strings.HasPrefix(q, "USE ") && !strings.Contains(q, ";") {
		c.db = strings.TrimPrefix(q, "USE ")
		return ""
	}
	return q
}

func (c *mapConn) record(q string, args []driver.NamedValue) (rest string, restArgs []driver.NamedValue, db string) {
	// A procRead call runs in its database for that call only: the session
	// stays where it was.
	if db, inner, innerArgs, ok := unwrapProcRead(q, args); ok {
		mapLog.mu.Lock()
		mapLog.stmts = append(mapLog.stmts, mapStmt{db: db, q: inner, args: innerArgs})
		mapLog.mu.Unlock()
		return inner, innerArgs, db
	}
	rest = c.enter(q)
	if rest != "" {
		mapLog.mu.Lock()
		mapLog.stmts = append(mapLog.stmts, mapStmt{db: c.db, q: rest, args: args})
		mapLog.mu.Unlock()
	}
	return rest, args, c.db
}

func (c *mapConn) ExecContext(_ context.Context, q string, args []driver.NamedValue) (driver.Result, error) {
	c.record(q, args)
	return driver.ResultNoRows, nil
}

func (c *mapConn) QueryContext(_ context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	rest, args, db := c.record(q, args)
	mapLog.mu.Lock()
	defer mapLog.mu.Unlock()
	arg := func() any {
		if len(args) == 0 {
			return nil
		}
		return args[0].Value
	}
	switch {
	case strings.Contains(rest, "FROM sys.databases") && strings.Contains(rest, "WHERE name = @p1"):
		name, _ := arg().(string)
		if !mapLog.databases[name] {
			return &detRows{cols: make([]string, 11)}, nil
		}
		return &detRows{cols: make([]string, 11), rows: [][]driver.Value{{
			name, int64(7), "ONLINE", "FULL", int64(160), "SQL_Latin1_General_CP1_CI_AS",
			"DATABASE_DEFAULT", false, time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), int64(0), false,
		}}}, nil
	case strings.Contains(rest, "SELECT sid, type_desc FROM sys.server_principals"):
		name, _ := arg().(string)
		sid, ok := mapLog.sids[name]
		if !ok {
			return &detRows{cols: make([]string, 2)}, nil
		}
		return &detRows{cols: make([]string, 2), rows: [][]driver.Value{{sid, "SQL_LOGIN"}}}, nil
	case strings.Contains(rest, "WHERE  dp.sid = @p1"):
		// Rows only for the SID the test registered, so a mapping read that
		// passed the wrong SID finds nothing.
		sid, _ := arg().([]byte)
		if !bytes.Equal(sid, mapTestSID) {
			return &detRows{cols: make([]string, 5)}, nil
		}
		return &detRows{cols: make([]string, 5), rows: mapLog.mappings[strings.Trim(db, "[]")]}, nil
	}
	// Anything else — CreateUser's read-back among them — finds nothing.
	return &detRows{cols: make([]string, 1)}, nil
}

var mapTestSID = []byte{0x01, 0x05, 0xA1, 0xCE}

// mapServer returns a Server over the recording driver with the log reset.
func mapServer(t *testing.T) *Server {
	t.Helper()
	pool, err := sql.Open("loginmap", "")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { pool.Close() })
	mapLog.mu.Lock()
	mapLog.stmts = nil
	mapLog.databases = map[string]bool{"Sales": true}
	mapLog.sids = map[string][]byte{"alice": mapTestSID}
	mapLog.mappings = map[string][][]driver.Value{}
	mapLog.mu.Unlock()
	return &Server{db: pool}
}

// mapStatements returns the recorded statements containing needle.
func mapStatements(needle string) []mapStmt {
	mapLog.mu.Lock()
	defer mapLog.mu.Unlock()
	var out []mapStmt
	for _, s := range mapLog.stmts {
		if strings.Contains(s.q, needle) {
			out = append(out, s)
		}
	}
	return out
}

// onlyMapStatement is the one recorded statement containing needle.
func onlyMapStatement(t *testing.T, needle string) mapStmt {
	t.Helper()
	found := mapStatements(needle)
	if len(found) != 1 {
		mapLog.mu.Lock()
		all := mapLog.stmts
		mapLog.mu.Unlock()
		t.Fatalf("%d statements contain %q, want exactly 1: %v", len(found), needle, all)
	}
	return found[0]
}

// readLogin is a login as Logins/LoginByName return it: SID already known.
func readLogin(s *Server) *Login {
	return &Login{server: s, Name: "alice", SID: mapTestSID, LoginType: "SQL_LOGIN"}
}

func TestLoginMapToDatabaseCreatesUserInThatDatabase(t *testing.T) {
	for _, tc := range []struct {
		name, schema, want string
	}{
		{"with default schema", "sales", "CREATE USER [app_alice] FOR LOGIN [alice] WITH DEFAULT_SCHEMA = [sales]"},
		{"server default schema", "", "CREATE USER [app_alice] FOR LOGIN [alice]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := mapServer(t)
			if err := readLogin(s).MapToDatabase(context.Background(), "Sales", "app_alice", tc.schema); err != nil {
				t.Fatalf("MapToDatabase: %v", err)
			}
			got := onlyMapStatement(t, "CREATE USER")
			if got.q != tc.want {
				t.Errorf("statement = %q, want %q", got.q, tc.want)
			}
			if got.db != "[Sales]" {
				t.Errorf("CREATE USER ran in %q, want [Sales]", got.db)
			}
			// The database is read by name before anything is written.
			read := onlyMapStatement(t, "FROM sys.databases")
			if len(read.args) != 1 || read.args[0].Value != "Sales" {
				t.Errorf("database read args = %v, want [Sales]", read.args)
			}
		})
	}
}

func TestLoginMapToDatabaseMissingDatabaseWritesNothing(t *testing.T) {
	s := mapServer(t)
	err := readLogin(s).MapToDatabase(context.Background(), "Nope", "app_alice", "")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	if n := len(mapStatements("CREATE USER")); n != 0 {
		t.Errorf("%d CREATE USER statements ran against a missing database", n)
	}
}

func TestLoginUnmapFromDatabaseDropsTheMappedUser(t *testing.T) {
	for _, tc := range []struct {
		name  string
		login func(*Server) *Login
	}{
		{"read login", readLogin},
		// A LoginRef has no SID: it is looked up by name first, and that SID
		// is the one the mapping read must use.
		{"login ref", func(s *Server) *Login { return s.LoginRef("alice") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := mapServer(t)
			// The mapped user's name differs from the login's: the DROP must
			// name the user, and two role rows are still one user.
			mapLog.mappings["Sales"] = [][]driver.Value{
				{"Sales", int64(5), "app_alice", "dbo", "db_datareader"},
				{"Sales", int64(5), "app_alice", "dbo", "db_datawriter"},
			}
			if err := tc.login(s).UnmapFromDatabase(context.Background(), "Sales"); err != nil {
				t.Fatalf("UnmapFromDatabase: %v", err)
			}
			got := onlyMapStatement(t, "DROP USER")
			if got.q != "DROP USER [app_alice]" {
				t.Errorf("statement = %q, want DROP USER [app_alice]", got.q)
			}
			if got.db != "[Sales]" {
				t.Errorf("DROP USER ran in %q, want [Sales]", got.db)
			}
			// The mapping is read in that database alone, not server-wide.
			m := onlyMapStatement(t, "WHERE  dp.sid = @p1")
			if m.db != "[Sales]" {
				t.Errorf("mapping read ran in %q, want [Sales]", m.db)
			}
		})
	}
}

func TestLoginUnmapFromDatabaseNotMappedWritesNothing(t *testing.T) {
	s := mapServer(t)
	err := readLogin(s).UnmapFromDatabase(context.Background(), "Sales")
	if err == nil || !strings.Contains(err.Error(), `login "alice" is not mapped to database "Sales"`) {
		t.Fatalf("err = %v, want not mapped", err)
	}
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want it to wrap ErrNotFound", err)
	}
	if n := len(mapStatements("DROP USER")); n != 0 {
		t.Errorf("%d DROP USER statements ran with no mapping", n)
	}
}

func TestLoginUnmapFromDatabaseMissingDatabaseReadsNoMapping(t *testing.T) {
	s := mapServer(t)
	err := readLogin(s).UnmapFromDatabase(context.Background(), "Nope")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	if n := len(mapStatements("dp.sid")) + len(mapStatements("DROP USER")); n != 0 {
		t.Errorf("%d mapping reads or drops ran against a missing database", n)
	}
}
