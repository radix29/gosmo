package gosmo

import (
	"context"
	"database/sql"
	"reflect"
	"testing"
	"time"
)

func TestErrorLogTypeString(t *testing.T) {
	cases := []struct {
		lt   ErrorLogType
		want string
	}{
		{ErrorLogSQLServer, "SQL Server"},
		{ErrorLogAgent, "SQL Server Agent"},
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
	for _, lt := range []ErrorLogType{ErrorLogSQLServer, ErrorLogAgent} {
		if !lt.valid() {
			t.Errorf("ErrorLogType(%d).valid() = false, want true", int(lt))
		}
	}
	for _, lt := range []ErrorLogType{0, 3, -1} {
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
	if _, err := s.ReadLogContext(context.Background(), ErrorLogType(7), 0); err == nil {
		t.Fatal("ReadLogContext with log type 7 returned no error")
	}
	if _, err := s.EnumErrorLogsContext(context.Background(), ErrorLogType(7)); err == nil {
		t.Fatal("EnumErrorLogsContext with log type 7 returned no error")
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
		if err := s.CycleLogContext(ctx, c.lt); err != nil {
			t.Fatalf("CycleLogContext(%s): %v", c.lt, err)
		}
		if len(script.Statements) != 1 {
			t.Fatalf("CycleLogContext(%s) recorded %d statements, want 1: %q", c.lt, len(script.Statements), script.Statements)
		}
		if got := script.Statements[0]; got != c.want {
			t.Errorf("CycleLogContext(%s) ran %q, want %q", c.lt, got, c.want)
		}
	}
}

// TestCycleErrorLogIsTheSQLServerFamily pins the older fixed-family method to
// the SQL Server log, so the delegation cannot be pointed at the Agent's
// procedure without a failure.
func TestCycleErrorLogIsTheSQLServerFamily(t *testing.T) {
	ctx, script := WithScript(context.Background())
	s := &Server{}
	if err := s.CycleErrorLogContext(ctx); err != nil {
		t.Fatalf("CycleErrorLogContext: %v", err)
	}
	want := []string{"EXEC sp_cycle_errorlog"}
	if len(script.Statements) != 1 || script.Statements[0] != want[0] {
		t.Errorf("CycleErrorLogContext ran %q, want %q", script.Statements, want)
	}
}

// TestCycleLogRejectsUnknownType pins the argument check, matching
// ReadLogContext's: an out-of-range type must fail before anything is run.
func TestCycleLogRejectsUnknownType(t *testing.T) {
	ctx, script := WithScript(context.Background())
	s := &Server{}
	if err := s.CycleLogContext(ctx, ErrorLogType(7)); err == nil {
		t.Fatal("CycleLogContext with log type 7 returned no error")
	}
	if len(script.Statements) != 0 {
		t.Errorf("a rejected log type still recorded %q", script.Statements)
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
