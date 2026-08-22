package gosmo

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"
	"time"
)

// ============================================================
// Error Log
// ============================================================

// ErrorLogType selects which of the two log families a read or an
// enumeration addresses. The values are the log-type argument
// xp_readerrorlog and sp_enumerrorlogs themselves take, so they can be
// passed straight through.
type ErrorLogType int

const (
	// ErrorLogSQLServer is the SQL Server error log (ERRORLOG, ERRORLOG.1, …).
	ErrorLogSQLServer ErrorLogType = 1
	// ErrorLogAgent is the SQL Server Agent error log (SQLAGENT.OUT,
	// SQLAGENT.1, …).
	ErrorLogAgent ErrorLogType = 2
)

// String names the log family for display.
func (t ErrorLogType) String() string {
	switch t {
	case ErrorLogSQLServer:
		return "SQL Server"
	case ErrorLogAgent:
		return "SQL Server Agent"
	}
	return fmt.Sprintf("ErrorLogType(%d)", int(t))
}

// valid reports whether t is one of the two log families the extended
// procedures accept. Anything else would reach xp_readerrorlog as a bad
// argument and come back as a raw msg 22004, so callers get a clear error
// instead.
func (t ErrorLogType) valid() bool {
	return t == ErrorLogSQLServer || t == ErrorLogAgent
}

// ErrorLogEntry represents one row returned by xp_readerrorlog.
//
// The middle column differs by log family, so only one of Process and
// ErrorLevel is meaningful for a given entry: the SQL Server log reports a
// ProcessInfo string ("Server", "spid9s"), the Agent log an integer
// severity. See Source for the one that applies.
type ErrorLogEntry struct {
	// LogDate is Date rendered as RFC 3339, kept for compatibility with
	// callers written before Date existed.
	LogDate string
	// Process is the SQL Server log's ProcessInfo column, empty on an Agent
	// log entry.
	Process string
	Text    string
	// Date is the entry's timestamp. The log stores no time zone, so this
	// is the server's local wall clock carried in UTC.
	Date time.Time
	// ErrorLevel is the Agent log's severity column, 0 on a SQL Server log
	// entry.
	ErrorLevel int
}

// Source returns whichever of Process and ErrorLevel the entry's log family
// populated — the value to show in a "Source" column that spans both.
func (e *ErrorLogEntry) Source() string {
	if e.Process != "" {
		return e.Process
	}
	return fmt.Sprintf("%d", e.ErrorLevel)
}

// ErrorLogFile is one log file reported by sp_enumerrorlogs: the current log
// plus however many archives the instance is configured to keep.
type ErrorLogFile struct {
	// Number is the archive number — 0 for the current log, 1 for the most
	// recently archived one, and so on. It is what ReadLog takes.
	Number int
	// Date is the last-written timestamp exactly as the server formatted it,
	// kept because that formatting follows the server's locale and is the
	// only thing to display when LastWritten couldn't be parsed.
	Date string
	// LastWritten is Date parsed, or the zero time if the server's format
	// wasn't one of the known ones.
	LastWritten time.Time
	// SizeBytes is the log file's size on disk.
	SizeBytes int64
}

// EnumErrorLogs lists the available log files of the given family.
func (s *Server) EnumErrorLogs(logType ErrorLogType) ([]*ErrorLogFile, error) {
	return s.EnumErrorLogsContext(context.Background(), logType)
}

// EnumErrorLogsContext is the context-aware variant of EnumErrorLogs.
// Results are ordered by Number, current log first — sp_enumerrorlogs
// returns the Agent family's current log last, not first.
func (s *Server) EnumErrorLogsContext(ctx context.Context, logType ErrorLogType) ([]*ErrorLogFile, error) {
	if !logType.valid() {
		return nil, fmt.Errorf("gosmo: enumerate error logs: unknown log type %d", int(logType))
	}
	rows, err := s.query(ctx, fmt.Sprintf("EXEC sp_enumerrorlogs %d", int(logType)))
	if err != nil {
		return nil, fmt.Errorf("gosmo: enumerate %s error logs: %w", logType, err)
	}
	defer rows.Close()

	var files []*ErrorLogFile
	for rows.Next() {
		f := &ErrorLogFile{}
		if err := rows.Scan(&f.Number, &f.Date, &f.SizeBytes); err != nil {
			return nil, fmt.Errorf("gosmo: enumerate %s error logs: %w", logType, err)
		}
		f.LastWritten = parseErrorLogFileDate(f.Date)
		files = append(files, f)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("gosmo: enumerate %s error logs: %w", logType, err)
	}
	slices.SortFunc(files, func(a, b *ErrorLogFile) int { return a.Number - b.Number })
	return files, nil
}

// errorLogFileDateLayouts are the formats sp_enumerrorlogs' Date column has
// been seen in. It is an nvarchar the extended procedure formats itself, so
// which one arrives depends on the server's locale — parseErrorLogFileDate
// tries each and reports the zero time rather than guessing.
var errorLogFileDateLayouts = []string{
	"01/02/2006  15:04",
	"01/02/2006 15:04",
	"01/02/2006  15:04:05",
	"2006-01-02 15:04",
	"2006-01-02 15:04:05",
}

// parseErrorLogFileDate parses one sp_enumerrorlogs Date value, returning
// the zero time if no known layout matches.
func parseErrorLogFileDate(s string) time.Time {
	s = strings.TrimSpace(s)
	for _, layout := range errorLogFileDateLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}

// ReadLog reads one log file of the given family.
// Pass logNumber=0 for the current log, 1 for the first archived log, etc.
func (s *Server) ReadLog(logType ErrorLogType, logNumber int) ([]*ErrorLogEntry, error) {
	return s.ReadLogContext(context.Background(), logType, logNumber)
}

// ReadLogContext is the context-aware variant of ReadLog. Which of an
// entry's Process and ErrorLevel is populated follows logType — see
// ErrorLogEntry.
func (s *Server) ReadLogContext(ctx context.Context, logType ErrorLogType, logNumber int) ([]*ErrorLogEntry, error) {
	return s.ReadLogFilteredContext(ctx, logType, logNumber, LogSearch{})
}

// LogSearch narrows a log read at the server. Every field is optional, and a
// zero LogSearch reads the whole file — what ReadLog does.
//
// These are xp_readerrorlog's own arguments 3-6, with its semantics: Text1 and
// Text2 are case-insensitive substrings AND-ed together (not two alternatives),
// and From/To bound the entry timestamp. Filtering here rather than in the
// caller matters for the current log on a busy instance, which runs to tens of
// thousands of entries.
type LogSearch struct {
	Text1, Text2 string
	From, To     time.Time
}

// empty reports whether the search narrows anything at all.
func (f LogSearch) empty() bool {
	return f.Text1 == "" && f.Text2 == "" && f.From.IsZero() && f.To.IsZero()
}

// ReadLogFiltered reads one log file of the given family, keeping only the
// entries the search matches.
func (s *Server) ReadLogFiltered(logType ErrorLogType, logNumber int, search LogSearch) ([]*ErrorLogEntry, error) {
	return s.ReadLogFilteredContext(context.Background(), logType, logNumber, search)
}

// ReadLogFilteredContext is the context-aware variant of ReadLogFiltered, and
// carries the read every other method here delegates to.
func (s *Server) ReadLogFilteredContext(ctx context.Context, logType ErrorLogType, logNumber int, search LogSearch) ([]*ErrorLogEntry, error) {
	if !logType.valid() {
		return nil, fmt.Errorf("gosmo: read error log: unknown log type %d", int(logType))
	}
	q, args := readErrorLogCall(logType, logNumber, search)
	rows, err := s.query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("gosmo: read %s error log %d: %w", logType, logNumber, err)
	}
	defer rows.Close()

	var entries []*ErrorLogEntry
	for rows.Next() {
		e := &ErrorLogEntry{}
		// The middle column is a ProcessInfo string on the SQL Server log and
		// an int severity on the Agent log; scanning the wrong one fails in
		// the driver, so each family gets its own destination.
		if logType == ErrorLogAgent {
			if err := rows.Scan(&e.Date, &e.ErrorLevel, &e.Text); err != nil {
				return nil, fmt.Errorf("gosmo: read %s error log %d: %w", logType, logNumber, err)
			}
		} else if err := rows.Scan(&e.Date, &e.Process, &e.Text); err != nil {
			return nil, fmt.Errorf("gosmo: read %s error log %d: %w", logType, logNumber, err)
		}
		e.LogDate = e.Date.Format(time.RFC3339Nano)
		entries = append(entries, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("gosmo: read %s error log %d: %w", logType, logNumber, err)
	}
	return entries, nil
}

// logSearchDateLayout is the format xp_readerrorlog parses a date filter in.
// Seconds are included: the procedure accepts a full datetime, and a bound
// rounded to the day would silently widen the range the caller asked for.
const logSearchDateLayout = "2006-01-02 15:04:05"

// readErrorLogCall builds the EXEC and its parameters. The two ints are
// interpolated — they are ints, and xp_readerrorlog is happy either way — but
// every value that came from the user is a parameter, and each argument up to
// the last one in use has to be present, so an unset one earlier in the list
// is passed as a typed NULL rather than omitted. A bare Go nil would reach the
// driver with no type at all.
func readErrorLogCall(logType ErrorLogType, logNumber int, search LogSearch) (string, []any) {
	call := fmt.Sprintf("EXEC xp_readerrorlog %d, %d", logNumber, int(logType))
	if search.empty() {
		return call, nil
	}
	text := func(v string) any {
		return sql.NullString{String: v, Valid: v != ""}
	}
	// The date bounds go as *text*, not as a datetime parameter: verified
	// live, xp_readerrorlog rejects a typed datetime with "The format for the
	// date filter is incorrect ... valid formats are 'YYYY-MM-DD' and
	// 'MM/DD/YYYY'". It parses the string itself, so this is the format it
	// asks for and not a convenience.
	stamp := func(v time.Time) any {
		if v.IsZero() {
			return sql.NullString{}
		}
		return sql.NullString{String: v.Format(logSearchDateLayout), Valid: true}
	}
	args := []any{text(search.Text1), text(search.Text2)}
	if !search.From.IsZero() || !search.To.IsZero() {
		args = append(args, stamp(search.From), stamp(search.To))
	}
	for i := range args {
		call += fmt.Sprintf(", @p%d", i+1)
	}
	return call, args
}

// ReadErrorLog reads a SQL Server error log file.
// Pass logNumber=0 for the current log, 1 for the first archived log, etc.
func (s *Server) ReadErrorLog(logNumber int) ([]*ErrorLogEntry, error) {
	return s.ReadErrorLogContext(context.Background(), logNumber)
}

// ReadErrorLogContext is the context-aware variant of ReadErrorLog. It is
// ReadLogContext fixed to the SQL Server log family.
func (s *Server) ReadErrorLogContext(ctx context.Context, logNumber int) ([]*ErrorLogEntry, error) {
	return s.ReadLogContext(ctx, ErrorLogSQLServer, logNumber)
}

// cycleLogStatements is the procedure each log family cycles with. The
// Agent's is named three-part because the procedure lives in msdb and the
// connection may sit in any database; sp_cycle_errorlog is in master and so
// resolves from anywhere through the sp_ prefix's master fallback.
var cycleLogStatements = map[ErrorLogType]string{
	ErrorLogSQLServer: "EXEC sp_cycle_errorlog",
	ErrorLogAgent:     "EXEC msdb.dbo.sp_cycle_agent_errorlog",
}

// CycleLog closes the current log of the given family and opens a new one,
// renumbering the archives and deleting the oldest if the instance is already
// holding as many as it is configured to keep.
func (s *Server) CycleLog(logType ErrorLogType) error {
	return s.CycleLogContext(context.Background(), logType)
}

// CycleLogContext is the context-aware variant of CycleLog. Cycling the Agent
// log requires SQL Server Agent to be running.
func (s *Server) CycleLogContext(ctx context.Context, logType ErrorLogType) error {
	stmt, ok := cycleLogStatements[logType]
	if !ok {
		return fmt.Errorf("gosmo: cycle error log: unknown log type %d", int(logType))
	}
	if err := s.execContext(ctx, stmt); err != nil {
		return fmt.Errorf("gosmo: cycle %s error log: %w", logType, err)
	}
	return nil
}

// CycleErrorLog closes the current error log and opens a new one.
// Equivalent to sp_cycle_errorlog. It is CycleLog fixed to the SQL Server
// log family.
func (s *Server) CycleErrorLog() error {
	return s.CycleErrorLogContext(context.Background())
}

// CycleErrorLogContext is the context-aware variant of CycleErrorLog.
func (s *Server) CycleErrorLogContext(ctx context.Context) error {
	return s.CycleLogContext(ctx, ErrorLogSQLServer)
}
