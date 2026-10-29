package gosmo

import (
	"context"
	"fmt"
)

// ============================================================
// Trace flags  (DBCC TRACESTATUS / TRACEON / TRACEOFF)
// ============================================================

// TraceFlag is one enabled trace flag, a row of DBCC TRACESTATUS(-1).
type TraceFlag struct {
	Flag int

	// Global is on for every session; Session only for the session that
	// turned it on. A global flag shows here from any session, a session
	// flag only from its own.
	Global  bool
	Session bool
}

// TraceFlags returns the trace flags enabled now, globally or for the reading
// session, in flag order — none is an empty list, not an error. A flag set at
// startup (-T) shows as Global like one turned on with SetTraceFlag; the DBCC
// does not tell them apart.
func (s *Server) TraceFlags(ctx context.Context) ([]TraceFlag, error) {
	// With nothing enabled the DBCC returns no result set at all, which
	// reads as no rows.
	rows, err := s.query(ctx, `DBCC TRACESTATUS(-1) WITH NO_INFOMSGS`)
	return scanRows(rows, err, "list trace flags", func(scan func(...any) error) (TraceFlag, error) {
		var f TraceFlag
		var status, global, session int
		if err := scan(&f.Flag, &status, &global, &session); err != nil {
			return TraceFlag{}, err
		}
		f.Global, f.Session = global == 1, session == 1
		return f, nil
	})
}

// SetTraceFlag turns flag on or off globally (DBCC TRACEON/TRACEOFF(flag,
// -1)); it needs sysadmin. It is always global: a session flag lasts only as
// long as the connection that set it, and a pooled connection makes that
// connection an accident. Either way the flag is lost at the next restart —
// a lasting flag is a startup parameter (-T), which no SQL statement sets.
//
// The server does not refuse every flag it will not honour: a startup-only
// flag (834, large-page memory) is skipped with an informational "Ignoring
// trace flag" message and no error, and an undocumented number is accepted
// and listed. Read TraceFlags afterwards to know what took. A flag below 1 is
// refused with ErrInvalidRequest before anything is sent.
func (s *Server) SetTraceFlag(ctx context.Context, flag int, on bool) error {
	verb := "TRACEOFF"
	if on {
		verb = "TRACEON"
	}
	if flag < 1 {
		return invalidf("gosmo: %s %d: a trace flag is positive", verb, flag)
	}
	if _, err := s.exec(ctx, fmt.Sprintf("DBCC %s(%d, -1) WITH NO_INFOMSGS", verb, flag)); err != nil {
		return fmt.Errorf("gosmo: %s %d: %w", verb, flag, err)
	}
	return nil
}
