package gosmo

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"io"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	mssql "github.com/microsoft/go-mssqldb"
)

func TestErrorLogTypeString(t *testing.T) {
	cases := []struct {
		lt   ErrorLogType
		want string
	}{
		{ErrorLogSQLServer, "SQL Server"},
		{ErrorLogAgent, "SQL Server Agent"},
		{ErrorLogDatabaseMail, "Database Mail"},
		{0, "ErrorLogType(0)"},
		{9, "ErrorLogType(9)"},
	}
	for _, c := range cases {
		if got := c.lt.String(); got != c.want {
			t.Errorf("ErrorLogType(%d).String() = %q, want %q", int(c.lt), got, c.want)
		}
	}
}

func TestErrorLogTypeValid(t *testing.T) {
	for _, lt := range []ErrorLogType{ErrorLogSQLServer, ErrorLogAgent, ErrorLogDatabaseMail} {
		if !lt.valid() {
			t.Errorf("ErrorLogType(%d).valid() = false, want true", int(lt))
		}
	}
	for _, lt := range []ErrorLogType{0, 4, -1} {
		if lt.valid() {
			t.Errorf("ErrorLogType(%d).valid() = true, want false", int(lt))
		}
	}
}

// TestReadLogRejectsUnknownType pins the argument check: an out-of-range log
// type must fail before the query, so the caller gets a named error instead
// of xp_readerrorlog's raw complaint.
func TestReadLogRejectsUnknownType(t *testing.T) {
	s := &Server{}
	if _, err := s.ReadLog(context.Background(), ErrorLogType(7), 0, LogSearch{}); err == nil {
		t.Fatal("ReadLog with log type 7 returned no error")
	}
	if _, err := s.EnumErrorLogs(context.Background(), ErrorLogType(7)); err == nil {
		t.Fatal("EnumErrorLogs with log type 7 returned no error")
	}
}

func TestErrorLogEntrySource(t *testing.T) {
	sqlEntry := &ErrorLogEntry{Process: "spid9s"}
	if got := sqlEntry.Source(); got != "spid9s" {
		t.Errorf("SQL Server entry Source() = %q, want %q", got, "spid9s")
	}
	agentEntry := &ErrorLogEntry{ErrorLevel: 3}
	if got := agentEntry.Source(); got != "3" {
		t.Errorf("Agent entry Source() = %q, want %q", got, "3")
	}
}

func TestParseErrorLogFileDate(t *testing.T) {
	want := time.Date(2026, 8, 12, 21, 49, 0, 0, time.UTC)
	// The two-space form is what SQL Server 2025 on Windows returns; the
	// others are tolerated because the column is a locale-formatted string.
	for _, in := range []string{"08/12/2026  21:49", "08/12/2026 21:49", " 08/12/2026  21:49 ", "2026-08-12 21:49"} {
		if got := parseErrorLogFileDate(in); !got.Equal(want) {
			t.Errorf("parseErrorLogFileDate(%q) = %v, want %v", in, got, want)
		}
	}
	for _, in := range []string{"", "not a date", "12.08.2026 21:49"} {
		if got := parseErrorLogFileDate(in); !got.IsZero() {
			t.Errorf("parseErrorLogFileDate(%q) = %v, want the zero time", in, got)
		}
	}
}

// TestCycleLogStatements pins each log family to the exact procedure it
// cycles with. The two are looked up from one shared table, so a test that
// only round-tripped "cycle then check it cycled" would pass with the entries
// swapped — cycling the Agent log when the user asked for the SQL Server one,
// and vice versa. Naming both here is what catches that.
func TestCycleLogStatements(t *testing.T) {
	cases := []struct {
		lt   ErrorLogType
		want string
	}{
		{ErrorLogSQLServer, "EXEC sp_cycle_errorlog"},
		{ErrorLogAgent, "EXEC msdb.dbo.sp_cycle_agent_errorlog"},
	}
	if len(cases) != len(cycleLogStatements) {
		t.Fatalf("cycleLogStatements has %d entries, this test names %d — a log family was added without pinning its statement",
			len(cycleLogStatements), len(cases))
	}
	for _, c := range cases {
		ctx, script := WithScript(context.Background())
		s := &Server{}
		if err := s.CycleLog(ctx, c.lt); err != nil {
			t.Fatalf("CycleLog(%s): %v", c.lt, err)
		}
		if len(script.Statements()) != 1 {
			t.Fatalf("CycleLog(%s) recorded %d statements, want 1: %q", c.lt, len(script.Statements()), script.Statements())
		}
		if got := script.Statements()[0]; got != c.want {
			t.Errorf("CycleLog(%s) ran %q, want %q", c.lt, got, c.want)
		}
	}
}

// TestCycleErrorLogIsTheSQLServerFamily pins the older fixed-family method to
// the SQL Server log, so the delegation cannot be pointed at the Agent's
// procedure without a failure.
func TestCycleErrorLogIsTheSQLServerFamily(t *testing.T) {
	ctx, script := WithScript(context.Background())
	s := &Server{}
	if err := s.CycleErrorLog(ctx); err != nil {
		t.Fatalf("CycleErrorLog: %v", err)
	}
	want := []string{"EXEC sp_cycle_errorlog"}
	if len(script.Statements()) != 1 || script.Statements()[0] != want[0] {
		t.Errorf("CycleErrorLog ran %q, want %q", script.Statements(), want)
	}
}

// TestCycleLogRejectsUnknownType pins the argument check, matching
// ReadLog's: an out-of-range type must fail before anything is run.
func TestCycleLogRejectsUnknownType(t *testing.T) {
	ctx, script := WithScript(context.Background())
	s := &Server{}
	if err := s.CycleLog(ctx, ErrorLogType(7)); err == nil {
		t.Fatal("CycleLog with log type 7 returned no error")
	}
	if len(script.Statements()) != 0 {
		t.Errorf("a rejected log type still recorded %q", script.Statements())
	}
}

// TestCycleLogRefusesDatabaseMail: the Database Mail log is a table with no
// archives, so there is nothing to cycle and nothing may be run.
func TestCycleLogRefusesDatabaseMail(t *testing.T) {
	ctx, script := WithScript(context.Background())
	s := &Server{}
	if err := s.CycleLog(ctx, ErrorLogDatabaseMail); err == nil {
		t.Fatal("CycleLog(ErrorLogDatabaseMail) returned no error")
	}
	if len(script.Statements()) != 0 {
		t.Errorf("a refused cycle still recorded %q", script.Statements())
	}
}

// TestReadErrorLogCall pins the EXEC and its arguments. xp_readerrorlog takes
// its search strings and date range positionally as arguments 3-6, so an
// argument that is not set but sits *before* one that is has to be a typed
// NULL and not be dropped — dropping it shifts every argument after it and
// silently searches for the wrong thing, or filters on a date the caller
// passed as text.
func TestReadErrorLogCall(t *testing.T) {
	from := time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 8, 21, 0, 0, 0, 0, time.UTC)

	for _, c := range []struct {
		name     string
		search   LogSearch
		wantCall string
		wantArgs []any
	}{
		{
			name:     "no search reads the whole file",
			wantCall: "EXEC xp_readerrorlog 0, 1",
		},
		{
			name:     "one search string",
			search:   LogSearch{Text1: "login failed"},
			wantCall: "EXEC xp_readerrorlog 0, 1, @p1, @p2",
			wantArgs: []any{
				sql.NullString{String: "login failed", Valid: true},
				sql.NullString{},
			},
		},
		{
			name:     "both search strings are AND-ed by the server",
			search:   LogSearch{Text1: "login", Text2: "failed"},
			wantCall: "EXEC xp_readerrorlog 0, 1, @p1, @p2",
			wantArgs: []any{
				sql.NullString{String: "login", Valid: true},
				sql.NullString{String: "failed", Valid: true},
			},
		},
		{
			// The case the typed NULLs exist for: a date range with no text.
			name:     "dates only still pass both text arguments",
			search:   LogSearch{From: from, To: to},
			wantCall: "EXEC xp_readerrorlog 0, 1, @p1, @p2, @p3, @p4",
			wantArgs: []any{
				sql.NullString{}, sql.NullString{},
				sql.NullString{String: "2026-08-20 00:00:00", Valid: true},
				sql.NullString{String: "2026-08-21 00:00:00", Valid: true},
			},
		},
		{
			name:     "an open-ended range leaves the other end NULL",
			search:   LogSearch{Text1: "x", From: from},
			wantCall: "EXEC xp_readerrorlog 0, 1, @p1, @p2, @p3, @p4",
			wantArgs: []any{
				sql.NullString{String: "x", Valid: true},
				sql.NullString{},
				sql.NullString{String: "2026-08-20 00:00:00", Valid: true},
				sql.NullString{},
			},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			call, args := readErrorLogCall(ErrorLogSQLServer, 0, c.search)
			if call != c.wantCall {
				t.Errorf("call = %q, want %q", call, c.wantCall)
			}
			if !reflect.DeepEqual(args, c.wantArgs) {
				t.Errorf("args = %#v, want %#v", args, c.wantArgs)
			}
		})
	}

	// The log number and family reach the statement as themselves.
	if call, _ := readErrorLogCall(ErrorLogAgent, 3, LogSearch{}); call != "EXEC xp_readerrorlog 3, 2" {
		t.Errorf("agent archive call = %q", call)
	}
}

// TestReadErrorLogProcCall pins sp_readerrorlog's form: the log number and
// family, then the two search strings or nothing — it takes no dates.
func TestReadErrorLogProcCall(t *testing.T) {
	from := time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		name     string
		search   LogSearch
		wantCall string
		wantArgs []any
	}{
		{name: "no search", wantCall: "EXEC sp_readerrorlog 0, 1"},
		{name: "dates alone pass nothing", search: LogSearch{From: from}, wantCall: "EXEC sp_readerrorlog 0, 1"},
		{
			name:     "the second string alone keeps the first NULL",
			search:   LogSearch{Text2: "failed", From: from},
			wantCall: "EXEC sp_readerrorlog 0, 1, @p1, @p2",
			wantArgs: []any{sql.NullString{}, sql.NullString{String: "failed", Valid: true}},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			call, args := readErrorLogProcCall(ErrorLogSQLServer, 0, c.search)
			if call != c.wantCall {
				t.Errorf("call = %q, want %q", call, c.wantCall)
			}
			if !reflect.DeepEqual(args, c.wantArgs) {
				t.Errorf("args = %#v, want %#v", args, c.wantArgs)
			}
		})
	}
	if call, _ := readErrorLogProcCall(ErrorLogAgent, 3, LogSearch{}); call != "EXEC sp_readerrorlog 3, 2" {
		t.Errorf("agent archive call = %q", call)
	}
}

// TestFilterLogDates: both bounds inclusive and cut to the second, as
// xp_readerrorlog reads them (verified live: an entry at .270 is outside a
// To of the same second, inside a From of it).
func TestFilterLogDates(t *testing.T) {
	at := func(sec, ms int) time.Time { return time.Date(2026, 10, 29, 18, 20, sec, ms*1e6, time.UTC) }
	entries := func() []*ErrorLogEntry {
		return []*ErrorLogEntry{{Date: at(32, 900)}, {Date: at(33, 0)}, {Date: at(33, 270)}, {Date: at(34, 0)}, {Date: at(35, 10)}}
	}
	dates := func(es []*ErrorLogEntry) []time.Time {
		var out []time.Time
		for _, e := range es {
			out = append(out, e.Date)
		}
		return out
	}
	for _, c := range []struct {
		name     string
		from, to time.Time
		want     []time.Time
	}{
		{"open", time.Time{}, time.Time{}, []time.Time{at(32, 900), at(33, 0), at(33, 270), at(34, 0), at(35, 10)}},
		{"from inclusive, cut to the second", at(33, 500), time.Time{}, []time.Time{at(33, 0), at(33, 270), at(34, 0), at(35, 10)}},
		{"to inclusive at the second", time.Time{}, at(34, 0), []time.Time{at(32, 900), at(33, 0), at(33, 270), at(34, 0)}},
		{"to cut to the second", time.Time{}, at(33, 999), []time.Time{at(32, 900), at(33, 0)}},
		{"both", at(33, 0), at(34, 0), []time.Time{at(33, 0), at(33, 270), at(34, 0)}},
	} {
		if got := dates(filterLogDates(entries(), c.from, c.to)); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}

// logReadScript is what the errorlogread driver answers: xp_readerrorlog
// refused (Msg 229) unless sysadmin, sp_readerrorlog refused (Msg 27219)
// when denied, else the rows; calls records each statement.
type logReadScript struct {
	sysadmin, denied bool
	rows             [][]driver.Value
	mu               sync.Mutex
	calls            []string
}

var logReadScripts sync.Map // DSN → *logReadScript

type logReadDriver struct{}

func (logReadDriver) Open(name string) (driver.Conn, error) {
	s, _ := logReadScripts.Load(name)
	return &logReadConn{s.(*logReadScript)}, nil
}

type logReadConn struct{ s *logReadScript }

func (c *logReadConn) Prepare(string) (driver.Stmt, error) { return nil, driver.ErrSkip }
func (c *logReadConn) Close() error                        { return nil }
func (c *logReadConn) Begin() (driver.Tx, error)           { return nil, driver.ErrSkip }

func (c *logReadConn) QueryContext(_ context.Context, q string, _ []driver.NamedValue) (driver.Rows, error) {
	c.s.mu.Lock()
	c.s.calls = append(c.s.calls, q)
	c.s.mu.Unlock()
	switch {
	case strings.HasPrefix(q, "EXEC xp_readerrorlog") && !c.s.sysadmin:
		return nil, mssql.Error{Number: 229, Class: 14,
			Message: "The EXECUTE permission was denied on the object 'xp_readerrorlog', database 'mssqlsystemresource', schema 'sys'."}
	case strings.HasPrefix(q, "EXEC sp_readerrorlog") && c.s.denied:
		return nil, mssql.Error{Number: 27219, Class: 16, Message: "Caller does not have permissions to execute the stored procedure."}
	}
	return &logReadRows{rows: c.s.rows}, nil
}

type logReadRows struct{ rows [][]driver.Value }

func (r *logReadRows) Columns() []string { return []string{"LogDate", "ProcessInfo", "Text"} }
func (r *logReadRows) Close() error      { return nil }
func (r *logReadRows) Next(dest []driver.Value) error {
	if len(r.rows) == 0 {
		return io.EOF
	}
	copy(dest, r.rows[0])
	r.rows = r.rows[1:]
	return nil
}

func init() { sql.Register("errorlogread", logReadDriver{}) }

// TestReadLogFallsBackToSpReadErrorLog: a login refused xp_readerrorlog reads
// through sp_readerrorlog, the dates applied in Go; a sysadmin never calls
// it; a login refused both gets the second refusal, PermissionDenied.
func TestReadLogFallsBackToSpReadErrorLog(t *testing.T) {
	at := func(h int) time.Time { return time.Date(2026, 10, 29, h, 0, 0, 0, time.UTC) }
	rows := [][]driver.Value{{at(8), "Server", "a"}, {at(10), "spid9s", "b"}, {at(12), "Logon", "c"}}
	search := LogSearch{Text1: "x", From: at(9), To: at(11)}
	read := func(name string, s *logReadScript) ([]*ErrorLogEntry, error) {
		t.Helper()
		logReadScripts.Store(name, s)
		db, err := sql.Open("errorlogread", name)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { db.Close() })
		return (&Server{db: db}).ReadLog(context.Background(), ErrorLogSQLServer, 2, search)
	}

	s := &logReadScript{rows: rows}
	got, err := read("fallback", s)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Text != "b" || got[0].Process != "spid9s" {
		t.Errorf("entries %+v, want only b (the dates applied in Go)", got)
	}
	wantCalls := []string{"EXEC xp_readerrorlog 2, 1, @p1, @p2, @p3, @p4", "EXEC sp_readerrorlog 2, 1, @p1, @p2"}
	if !reflect.DeepEqual(s.calls, wantCalls) {
		t.Errorf("calls %q, want %q", s.calls, wantCalls)
	}

	// A sysadmin: one call, the server's filter (this fake applies none).
	s = &logReadScript{sysadmin: true, rows: rows}
	if got, err = read("sysadmin", s); err != nil || len(got) != 3 || len(s.calls) != 1 {
		t.Errorf("sysadmin: %d entries, %v, calls %q", len(got), err, s.calls)
	}

	s = &logReadScript{denied: true, rows: rows}
	_, err = read("denied", s)
	if !IsPermissionDenied(err) {
		t.Fatalf("refused both: %v, want PermissionDenied", err)
	}
	if se, ok := AsSQLError(err); !ok || se.Number != 27219 {
		t.Errorf("refused both: %v, want Msg 27219", err)
	}
	if !strings.HasPrefix(err.Error(), "gosmo: read SQL Server error log 2: ") {
		t.Errorf("error %q lacks the operation prefix", err)
	}
}
