package gosmo

import (
	"errors"
	"fmt"
	"slices"
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
// Two not-found conventions exist across the package, and the difference is
// deliberate rather than an oversight:
//
//   - Every by-name lookup — LoginByName, DatabaseByName, TableByName,
//     UserByName, RoleByName, JobByName, AlertByName, OperatorByName,
//     ScheduleByName, ServerRoleByName, ConfigurationByName,
//     AvailabilityGroupByName, CertificateByName, AsymmetricKeyByName,
//     SymmetricKeyByName and the scripter's lookups among them — returns an
//     error wrapping ErrNotFound. CertificateByName and AsymmetricKeyByName
//     answered (nil, nil) instead until 2026-09-22. ScheduleByName also
//     returns ErrAmbiguous for a name two schedules share.
//   - AgentInfo reports an unreachable Agent as a populated value
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

// ErrAmbiguous is wrapped by a by-name lookup whose name the catalog does not
// keep unique and which matched more than one object, so a caller can tell
// "pick by id" from "not there" (ErrNotFound) with errors.Is.
//
// Returning one of the matches instead would be wrong: a write keyed by its
// id then lands on whichever row the server happened to return first.
// ScheduleByName is the case — msdb.dbo.sysschedules.name has no unique
// constraint, and SSMS's New Job ▸ Schedules makes one schedule per job, so
// several jobs scheduled "Daily" mean several schedules named Daily. Its
// error names ScheduleByID, the lookup that is always exact.
var ErrAmbiguous = errors.New("ambiguous name")

// ErrSchemaRequired is wrapped by every call given a schema-scoped name with
// an empty schema. Nothing defaults it: dbo and the caller's own default
// schema are both plausible readings, and guessing wrong addresses — or
// drops — a different object.
var ErrSchemaRequired = errors.New("schema is required")

// ErrHandleNotLoaded is wrapped by every read keyed by a catalog id the
// receiver does not have — a Table from Database.TableRef, whose ObjectID is
// zero, or any other …Ref handle whose child reads key on an id (an
// assembly's files, an alert's notifications, an availability group's
// replicas). The read would otherwise ask for object 0 and get back an empty
// result indistinguishable from an object with no children. Read the object
// with its *ByName lookup (or the listing) instead. Job and Login child reads
// are not refused: they look the id up by name themselves.
var ErrHandleNotLoaded = errors.New("handle not loaded: read it by name first")

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

// ErrUnsupportedVersion reports a call gosmo refused because the connected
// instance is older than the feature it names — a refusal decided here, before
// any statement is sent, because the server's own answer would be a parse
// error naming syntax the caller never wrote.
//
// Every such refusal wraps it, so a caller (or a sweep across an old instance)
// can tell "this server is too old for this feature" from "this read is
// broken". The message text is unchanged by the sentinel.
var ErrUnsupportedVersion = errors.New("unsupported server version")

// unsupportedVersionError carries its own message and reaches
// ErrUnsupportedVersion through the chain, the way notFoundError does for
// ErrNotFound.
type unsupportedVersionError struct{ msg string }

func (e *unsupportedVersionError) Error() string { return e.msg }

func (e *unsupportedVersionError) Unwrap() error { return ErrUnsupportedVersion }

// unsupportedVersionf builds a version refusal whose message is exactly
// format/args.
func unsupportedVersionf(format string, args ...any) error {
	return &unsupportedVersionError{msg: fmt.Sprintf(format, args...)}
}

// ErrUnsupported reports a request gosmo refuses because it has no faithful
// form for it, decided before any text is produced or statement sent:
//
//   - a script of an object it cannot express — an external table, a
//     FileTable, a ledger table, a table with Always Encrypted columns, a CLR
//     module's CREATE, a pool affinity it cannot map to scheduler ids, or a
//     DROP of what cannot be dropped (a built-in Resource Governor pool or
//     group, the Resource Governor or Database Mail configuration). A script
//     that recreates something *different* under the same name is worse than
//     none, and under DROP AND CREATE it drops the object and never recreates
//     it;
//   - Database.BulkInsert under WithScript: a bulk load has no T-SQL form to
//     collect;
//   - a read with no form for its input: Server.EventFiles given a blob URL,
//     which cannot be listed.
//
// It is not ErrUnsupportedVersion: the server is not too old for anything,
// gosmo simply has no form for the request.
var ErrUnsupported = errors.New("not supported")

// unsupportedError carries its own message and reaches ErrUnsupported through
// the chain — and, when its format wrapped one with %w, the cause too.
type unsupportedError struct {
	msg   string
	cause error // the fmt.Errorf result when it wraps something, else nil
}

func (e *unsupportedError) Error() string { return e.msg }

func (e *unsupportedError) Unwrap() []error {
	if e.cause == nil {
		return []error{ErrUnsupported}
	}
	return []error{ErrUnsupported, e.cause}
}

// unsupportedf builds a refusal whose message is exactly format/args. A %w in
// format keeps that error reachable through errors.Is and errors.As, as
// fmt.Errorf would.
func unsupportedf(format string, args ...any) error {
	err := fmt.Errorf(format, args...)
	e := &unsupportedError{msg: err.Error()}
	switch err.(type) {
	case interface{ Unwrap() error }, interface{ Unwrap() []error }:
		e.cause = err
	}
	return e
}

// ErrInvalidRequest reports a call gosmo refused because of its own
// arguments, before any statement was sent: a permission name no allowlist
// holds for the securable, a modifier the verb has no form for, an empty
// column list, a value that cannot be written safely. The request is wrong as
// asked; retrying it, or asking another server, gives the same answer.
//
// It is distinct from ErrUnsupported, where the request is well formed but
// gosmo has no faithful form for it, and from ErrSchemaRequired, which a
// missing schema wraps instead. The message text is unchanged by the
// sentinel.
var ErrInvalidRequest = errors.New("invalid request")

// invalidRequestError carries its own message and reaches ErrInvalidRequest
// through the chain, the way unsupportedError does for ErrUnsupported.
type invalidRequestError struct {
	msg   string
	cause error // the fmt.Errorf result when it wraps something, else nil
}

func (e *invalidRequestError) Error() string { return e.msg }

func (e *invalidRequestError) Unwrap() []error {
	if e.cause == nil {
		return []error{ErrInvalidRequest}
	}
	return []error{ErrInvalidRequest, e.cause}
}

// invalidf builds a refusal of the caller's arguments whose message is
// exactly format/args. A %w in format keeps that error reachable, as
// fmt.Errorf would.
func invalidf(format string, args ...any) error {
	err := fmt.Errorf(format, args...)
	e := &invalidRequestError{msg: err.Error()}
	switch err.(type) {
	case interface{ Unwrap() error }, interface{ Unwrap() []error }:
		e.cause = err
	}
	return e
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

// ============================================================
// Multi-message batches
// ============================================================

// multiMessageError renders every error message a failed batch produced,
// not just the last one. It wraps the driver error unchanged, so AsSQLError,
// errors.Is and errors.As all still reach it.
type multiMessageError struct {
	err  error
	text string
}

func (e *multiMessageError) Error() string { return e.text }
func (e *multiMessageError) Unwrap() error { return e.err }

// withAllMessages rewrites a driver error whose batch produced more than one
// error message so its text carries all of them, first to last.
//
// SQL Server routinely explains a failure in one message and reports it in
// another, and database/sql surfaces only the last — which is the useless
// half. Refusing an ALTER DATABASE for want of permission sends both:
//
//	Msg 5011 — User does not have permission to alter database 'X', ...
//	Msg 5069 — ALTER DATABASE statement failed.
//
// and until 2026-08-25 a caller saw nothing but "ALTER DATABASE statement
// failed", with no way to tell a permissions problem from a state one. The
// detail was never lost — mssql.Error.All has carried it all along, and
// AsSQLError exposes it — but nothing that merely prints the error saw it.
//
// Informational messages (severity below 11) are dropped: a batch that runs
// USE first collects a class-0 "Changed database context" that is not part of
// the failure. An error with a single message is returned untouched, which is
// the overwhelmingly common case.
func withAllMessages(err error) error {
	if err == nil {
		return nil
	}
	me, ok := errors.AsType[mssql.Error](err)
	if !ok || len(me.All) < 2 {
		return err
	}
	var parts []string
	for _, m := range me.All {
		if m.Class >= 11 && m.Message != "" {
			parts = append(parts, m.Message)
		}
	}
	if len(parts) < 2 {
		return err
	}
	return &multiMessageError{err: err, text: "mssql: " + strings.Join(parts, " ")}
}

// ============================================================
// Classifying SQL Server errors
// ============================================================

// RefusalKind is how much a SQL Server error lets a caller claim about
// permissions.
type RefusalKind int

const (
	// NotRefused — the error says nothing about permissions.
	NotRefused RefusalKind = iota

	// PermissionDenied — the server stated that a permission was denied.
	PermissionDenied

	// MissingOrDenied — the server said the object "does not exist or you do
	// not have permission", which it does deliberately: telling the two apart
	// would let an unprivileged login enumerate objects it cannot see. Nothing
	// downstream may narrow it to one or the other.
	MissingOrDenied
)

// refusalNumbers classifies the SQL Server error numbers that mean a login was
// refused. Every one was captured from a live instance against a
// least-privileged test login, not taken from documentation — the wording
// differs between numbers in ways no amount of reasoning predicts, and a
// pattern written from the docs matches nothing.
//
//	229   The SELECT/EXECUTE permission was denied on the object '…'
//	230   The SELECT permission was denied on the column '…'
//	262   CREATE TABLE / BACKUP DATABASE / … permission denied in database '…'
//	297   The user does not have permission to perform this action
//	300   VIEW SERVER (PERFORMANCE|SECURITY) STATE permission was denied …
//	916   The server principal "…" is not able to access the database "…"
//	1088  Cannot find the object "…" because it does not exist or you do not
//	      have permissions
//	3701  Cannot drop the table '…', because it does not exist or …
//	5011  User does not have permission to alter database '…', the database
//	      does not exist, or …
//	15151 Cannot alter the login '…', because it does not exist or …
//	15247 User does not have permission to perform this action
//
// A number missing from this map is simply NotRefused.
var refusalNumbers = map[int32]RefusalKind{
	229: PermissionDenied, 230: PermissionDenied, 262: PermissionDenied,
	297: PermissionDenied, 300: PermissionDenied, 916: PermissionDenied,

	1088: MissingOrDenied,
	3701: MissingOrDenied, 5011: MissingOrDenied,
	15151: MissingOrDenied, 15247: MissingOrDenied,
}

// ClassifyRefusal reports what err says about permissions, and the message
// that says it. It reads the *first* qualifying message of the batch rather
// than the last, because that is the one that names the right: a refused
// sys.dm_os_process_memory read sends "VIEW SERVER PERFORMANCE STATE
// permission was denied on object 'server'" (Msg 300) followed by the
// contentless "The user does not have permission to perform this action"
// (Msg 297), and database/sql surfaces only the second. A refused BACKUP
// sends Msg 262 then the contentless Msg 3013 the same way. Both
// live-captured 2026-08-25.
//
// Only an error-severity message (11 and above) with text qualifies. The
// result is (NotRefused, nil) when nothing does, including for an error that
// is not a SQL Server error at all.
//
// Classification is keyed on the number, never the wording: the message
// follows the session's language, the number does not.
func ClassifyRefusal(err error) (RefusalKind, *SQLError) {
	se, ok := AsSQLError(err)
	if !ok {
		return NotRefused, nil
	}
	for _, m := range se.messages() {
		if kind := refusalNumbers[m.Number]; kind != NotRefused && m.IsError() && m.Message != "" {
			return kind, &m
		}
	}
	return NotRefused, nil
}

// IsPermissionDenied reports whether err is SQL Server stating that the login
// lacks a permission. The "does not exist or you do not have permission"
// errors are not this: see IsMissingOrDenied.
func IsPermissionDenied(err error) bool {
	kind, _ := ClassifyRefusal(err)
	return kind == PermissionDenied
}

// IsMissingOrDenied reports whether err is SQL Server's deliberately ambiguous
// "does not exist or you do not have permission". The server will not say
// which, so neither may a caller: treat it as both.
func IsMissingOrDenied(err error) bool {
	kind, _ := ClassifyRefusal(err)
	return kind == MissingOrDenied
}

// alreadyExistsNumbers are the errors a CREATE raises when its name is taken.
//
//	1801  Database '…' already exists.
//	1913  The operation failed because an index or statistics with name '…'
//	      already exists on table '…'.
//	2714  There is already an object named '…' in the database.
//	15023 User, group, or role '…' already exists in the current database.
//	15025 The server principal '…' already exists.
//
// 15023 and 15025 were confirmed live under SET LANGUAGE Deutsch, where the
// text reads "… ist bereits vorhanden" — which is why this is keyed on the
// number.
var alreadyExistsNumbers = []int32{1801, 1913, 2714, 15023, 15025}

// IsAlreadyExists reports whether err is SQL Server refusing a CREATE because
// the name is already taken, in any message of the batch. It matches error
// numbers only, so it holds on a server of any language, and an error that is
// not a SQL Server error is never one.
func IsAlreadyExists(err error) bool {
	se, ok := AsSQLError(err)
	if !ok {
		return false
	}
	return slices.ContainsFunc(se.messages(), func(m SQLError) bool {
		return slices.Contains(alreadyExistsNumbers, m.Number)
	})
}

// messages is every message the batch produced: All, or the error itself when
// it reported only one.
func (e *SQLError) messages() []SQLError {
	if len(e.All) > 0 {
		return e.All
	}
	return []SQLError{*e}
}
