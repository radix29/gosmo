package gosmo

import (
	"errors"
	"fmt"
	"strings"

	mssql "github.com/microsoft/go-mssqldb"
)

// ============================================================
// Not found
// ============================================================

// ErrNotFound is the sentinel every by-name lookup that reports absence as an
// error wraps, so a caller can tell "this object does not exist" from "the
// lookup itself failed" with errors.Is(err, gosmo.ErrNotFound) instead of
// matching on message text.
//
// Making that distinction matters: a caller that treats any error as absence
// will go on to create an object it never established was missing, and report
// the creation's failure instead of the permission or connection error that
// actually stopped it.
//
// Three not-found conventions exist across the package, and the difference is
// deliberate rather than an oversight:
//
//   - Most by-name lookups — LoginByName, DatabaseByName, TableByName,
//     UserByName, RoleByName, AgentJobByName, AlertByName, OperatorByName,
//     ScheduleByName, ServerRoleByName, ConfigurationByName,
//     AvailabilityGroupByName and the scripter's view/procedure/function
//     lookups — return an error wrapping ErrNotFound.
//   - CertificateByName returns (nil, nil), because its callers branch on
//     absence as the ordinary case rather than the exceptional one.
//   - AgentStatus reports an unreachable Agent as a populated value
//     (StatusText "Unknown"), not an error.
//
// AvailabilityGroupByName's not-found error additionally still satisfies
// errors.Is(err, sql.ErrNoRows), which it promised before ErrNotFound existed.
var ErrNotFound = errors.New("not found")

// notFoundError carries a caller-facing message that reads naturally on its
// own while still reaching ErrNotFound through the error chain — so adding the
// sentinel changed no existing message text.
type notFoundError struct {
	msg  string
	also error // an extra error kept reachable, e.g. sql.ErrNoRows
}

func (e *notFoundError) Error() string { return e.msg }

func (e *notFoundError) Unwrap() []error {
	if e.also == nil {
		return []error{ErrNotFound}
	}
	return []error{ErrNotFound, e.also}
}

// notFoundf builds a not-found error whose message is exactly format/args.
func notFoundf(format string, args ...any) error {
	return &notFoundError{msg: fmt.Sprintf(format, args...)}
}

// notFoundfAlso is notFoundf keeping a second error reachable through
// errors.Is — for a lookup that documented a different sentinel before
// ErrNotFound existed and must go on satisfying it.
func notFoundfAlso(also error, format string, args ...any) error {
	return &notFoundError{msg: fmt.Sprintf(format, args...), also: also}
}

// ============================================================
// SQL Server errors
// ============================================================

// SQLError is a structured SQL Server error — the "Msg 208, Level 16,
// State 1, Line 4" detail SSMS shows in its Messages pane. It is extracted
// from the driver's own error type so callers can inspect the error number,
// severity, and line without importing github.com/microsoft/go-mssqldb
// directly. Use AsSQLError to obtain one from any error.
type SQLError struct {
	// Number is the SQL Server error number (the "Msg" value), e.g. 208
	// for "Invalid object name".
	Number int32

	// State is the error state, disambiguating errors that share a Number.
	State uint8

	// Class is the severity level (the "Level" value). 0-10 are
	// informational; 11-16 are user-correctable; 17+ are software or
	// hardware errors.
	Class uint8

	// Message is the human-readable error text.
	Message string

	// ServerName is the instance that raised the error.
	ServerName string

	// ProcName is the stored procedure, function, or trigger that raised
	// the error; empty for an ad-hoc batch.
	ProcName string

	// LineNo is the 1-based line within the batch or procedure.
	LineNo int32

	// All lists every error the batch produced, first to last. The final
	// entry mirrors the fields above. Nil when only a single error is
	// reported.
	All []SQLError
}

// Header renders the SSMS status line for the error: its number, level,
// state, optional procedure, and line — everything but the message text.
// SSMS shows this on its own line above the message.
func (e *SQLError) Header() string {
	var b strings.Builder
	fmt.Fprintf(&b, "Msg %d, Level %d, State %d", e.Number, e.Class, e.State)
	if e.ProcName != "" {
		fmt.Fprintf(&b, ", Procedure %s", e.ProcName)
	}
	fmt.Fprintf(&b, ", Line %d", e.LineNo)
	return b.String()
}

// Error renders the error in the multi-line form SSMS uses: the Header line
// followed by the message text.
func (e *SQLError) Error() string {
	if e.Message == "" {
		return e.Header()
	}
	return e.Header() + "\n" + e.Message
}

// IsError reports whether the severity level is high enough to be treated
// as a failure (11 and above) rather than an informational message.
func (e *SQLError) IsError() bool { return e.Class >= 11 }

// AsSQLError reports whether err, or any error it wraps, is a SQL Server
// error and, if so, returns its structured form.
func AsSQLError(err error) (*SQLError, bool) {
	me, ok := errors.AsType[mssql.Error](err)
	if !ok {
		return nil, false
	}
	return convertSQLError(me), true
}

// convertSQLError copies a driver mssql.Error into a gosmo SQLError,
// including its All list.
func convertSQLError(me mssql.Error) *SQLError {
	e := newSQLErrorFrom(me)
	if len(me.All) > 0 {
		e.All = make([]SQLError, len(me.All))
		for i, a := range me.All {
			e.All[i] = *newSQLErrorFrom(a)
		}
	}
	return e
}

func newSQLErrorFrom(me mssql.Error) *SQLError {
	return &SQLError{
		Number:     me.Number,
		State:      me.State,
		Class:      me.Class,
		Message:    me.Message,
		ServerName: me.ServerName,
		ProcName:   me.ProcName,
		LineNo:     me.LineNo,
	}
}
