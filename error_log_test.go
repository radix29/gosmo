package gosmo

import (
	"context"
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
