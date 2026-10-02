package gosmo

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"

	mssql "github.com/microsoft/go-mssqldb"
)

func TestSQLErrorFormat(t *testing.T) {
	e := &SQLError{Number: 208, Class: 16, State: 1, LineNo: 4, Message: "Invalid object name 'foo'."}
	want := "Msg 208, Level 16, State 1, Line 4\nInvalid object name 'foo'."
	if got := e.Error(); got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}

func TestSQLErrorFormatWithProcedure(t *testing.T) {
	e := &SQLError{Number: 2812, Class: 16, State: 62, ProcName: "myproc", LineNo: 1, Message: "Could not find stored procedure 'x'."}
	want := "Msg 2812, Level 16, State 62, Procedure myproc, Line 1\nCould not find stored procedure 'x'."
	if got := e.Error(); got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}

func TestSQLErrorIsError(t *testing.T) {
	if (&SQLError{Class: 10}).IsError() {
		t.Error("Class 10 should be informational, not an error")
	}
	if !(&SQLError{Class: 16}).IsError() {
		t.Error("Class 16 should be an error")
	}
}

func TestAsSQLError(t *testing.T) {
	driverErr := mssql.Error{
		Number:     208,
		State:      1,
		Class:      16,
		Message:    "Invalid object name 'foo'.",
		ServerName: "SQL01",
		ProcName:   "",
		LineNo:     4,
	}

	// Wrapped, to prove the errors.AsType unwrap works.
	wrapped := fmt.Errorf("run batch: %w", driverErr)

	se, ok := AsSQLError(wrapped)
	if !ok {
		t.Fatal("AsSQLError returned ok=false for a wrapped mssql.Error")
	}
	if se.Number != 208 || se.Class != 16 || se.State != 1 || se.LineNo != 4 {
		t.Errorf("fields = %+v, want Number 208 Class 16 State 1 LineNo 4", se)
	}
	if se.ServerName != "SQL01" {
		t.Errorf("ServerName = %q, want SQL01", se.ServerName)
	}
	if se.Message != "Invalid object name 'foo'." {
		t.Errorf("Message = %q", se.Message)
	}
}

func TestAsSQLErrorCopiesAll(t *testing.T) {
	driverErr := mssql.Error{
		Number:  102,
		Class:   15,
		Message: "Incorrect syntax near 'x'.",
		All: []mssql.Error{
			{Number: 102, Class: 15, Message: "Incorrect syntax near 'x'."},
			{Number: 105, Class: 15, Message: "Unclosed quotation mark."},
		},
	}
	se, ok := AsSQLError(driverErr)
	if !ok {
		t.Fatal("AsSQLError returned ok=false")
	}
	if len(se.All) != 2 {
		t.Fatalf("len(All) = %d, want 2", len(se.All))
	}
	if se.All[1].Number != 105 {
		t.Errorf("All[1].Number = %d, want 105", se.All[1].Number)
	}
}

func TestAsSQLErrorNonSQL(t *testing.T) {
	if _, ok := AsSQLError(errors.New("plain error")); ok {
		t.Error("AsSQLError returned ok=true for a non-SQL error")
	}
	if _, ok := AsSQLError(nil); ok {
		t.Error("AsSQLError returned ok=true for nil")
	}
}

func TestNotFoundWrapsSentinel(t *testing.T) {
	err := notFoundf("gosmo: login %q not found", "sa")
	if !errors.Is(err, ErrNotFound) {
		t.Error("notFoundf should satisfy errors.Is(err, ErrNotFound)")
	}
	// The sentinel reaches errors.Is through Unwrap without appearing in the
	// text, which is what let ErrNotFound be added without rewording any of
	// the 18 existing messages.
	if got, want := err.Error(), `gosmo: login "sa" not found`; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}

func TestNotFoundAlsoKeepsSecondSentinel(t *testing.T) {
	// AvailabilityGroupByName documented sql.ErrNoRows before ErrNotFound
	// existed; both must keep matching or a library consumer's check breaks.
	err := notFoundfAlso(sql.ErrNoRows, "gosmo: availability group %q not found", "AAG1")
	if !errors.Is(err, ErrNotFound) {
		t.Error("should satisfy ErrNotFound")
	}
	if !errors.Is(err, sql.ErrNoRows) {
		t.Error("should still satisfy sql.ErrNoRows")
	}
}

func TestNotFoundDoesNotMatchOtherErrors(t *testing.T) {
	// The point of the sentinel is telling absence from failure, so a
	// permission or connection error must not read as "not found".
	permissionDenied := fmt.Errorf("gosmo: find login %q: %w", "sa",
		mssql.Error{Number: 229, Class: 14, Message: "The SELECT permission was denied"})
	if errors.Is(permissionDenied, ErrNotFound) {
		t.Error("an unrelated error must not satisfy ErrNotFound")
	}
	// Only the lookups that opted in report absence. A raw ErrNoRows escaping
	// from somewhere that never classified it must not read as ErrNotFound,
	// or the sentinel would mean "some query returned no rows" instead.
	rawNoRows := fmt.Errorf("gosmo: read something: %w", sql.ErrNoRows)
	if errors.Is(rawNoRows, ErrNotFound) {
		t.Error("an unclassified sql.ErrNoRows must not satisfy ErrNotFound")
	}
}

// -- multi-message batches ---------------------------------------------------

// altErr is the pair SQL Server actually sends when ALTER DATABASE is refused
// for want of permission, captured live from win10cli 2026-08-25.
func altErr() mssql.Error {
	first := mssql.Error{
		Number: 5011, Class: 14, State: 9,
		Message: "User does not have permission to alter database 'HealthClinic', the database does not exist, or the database is not in a state that allows access checks.",
	}
	last := mssql.Error{
		Number: 5069, Class: 16, State: 1,
		Message: "ALTER DATABASE statement failed.",
	}
	last.All = []mssql.Error{first, last}
	return last
}

func TestWithAllMessagesKeepsTheMessageThatNamesTheCause(t *testing.T) {
	got := withAllMessages(altErr()).Error()

	// The cause, which database/sql drops, has to be there...
	if !strings.Contains(got, "does not have permission to alter database") {
		t.Errorf("Error() = %q\nwant it to name the permission failure", got)
	}
	// ...and the message that did surface must not be dropped in exchange.
	if !strings.Contains(got, "ALTER DATABASE statement failed.") {
		t.Errorf("Error() = %q\nwant it to keep the trailing message too", got)
	}
}

func TestWithAllMessagesStaysAnSQLError(t *testing.T) {
	err := fmt.Errorf("gosmo: set recovery model: %w", withAllMessages(altErr()))

	se, ok := AsSQLError(err)
	if !ok {
		t.Fatal("AsSQLError = false: wrapping broke the error chain")
	}
	if se.Number != 5069 {
		t.Errorf("Number = %d, want 5069 (the error that surfaced)", se.Number)
	}
	if len(se.All) != 2 {
		t.Errorf("len(All) = %d, want 2", len(se.All))
	}
	if !strings.HasPrefix(err.Error(), "gosmo: set recovery model: ") {
		t.Errorf("Error() = %q, want the caller's prefix preserved", err.Error())
	}
}

func TestWithAllMessagesLeavesOrdinaryErrorsAlone(t *testing.T) {
	single := mssql.Error{Number: 208, Class: 16, Message: "Invalid object name 'x'."}
	if got := withAllMessages(single); got.Error() != single.Error() {
		t.Errorf("single-message error rewritten to %q, want %q", got.Error(), single.Error())
	}
	if withAllMessages(nil) != nil {
		t.Error("withAllMessages(nil) is not nil")
	}
	plain := errors.New("dial tcp: connection refused")
	if got := withAllMessages(plain); got != plain {
		t.Errorf("non-SQL error rewritten to %v, want it returned unchanged", got)
	}
}

// TestWithAllMessagesDropsInformationalMessages pins the severity filter: a
// batch that issues USE first collects a class-0 "Changed database context"
// notice, which is not part of the failure and must not be prepended to it.
func TestWithAllMessagesDropsInformationalMessages(t *testing.T) {
	notice := mssql.Error{Number: 5701, Class: 0, Message: "Changed database context to 'HealthClinic'."}
	e := altErr()
	e.All = append([]mssql.Error{notice}, e.All...)

	got := withAllMessages(e).Error()
	if strings.Contains(got, "Changed database context") {
		t.Errorf("Error() = %q\nwant the class-0 notice dropped", got)
	}
	if !strings.Contains(got, "does not have permission") {
		t.Errorf("Error() = %q\nwant the real cause kept", got)
	}
}

// TestUnsupportedKeepsAWrappedCause: a refusal built from another error with
// %w — schedulerGroupSizes' permission refusal — still answers errors.As for
// the server's error, and errors.Is for ErrUnsupported; one built without %w
// reaches the sentinel alone.
func TestUnsupportedKeepsAWrappedCause(t *testing.T) {
	cause := mssql.Error{Number: 300, Class: 14, Message: "VIEW SERVER STATE permission was denied"}
	err := unsupportedf("mapping affinity needs VIEW SERVER STATE: %w", cause)
	if !errors.Is(err, ErrUnsupported) {
		t.Error("lost ErrUnsupported")
	}
	if se, ok := AsSQLError(err); !ok || se.Number != 300 {
		t.Errorf("AsSQLError = %v, %v; want the wrapped Msg 300", se, ok)
	}
	if want := "mapping affinity needs VIEW SERVER STATE: " + cause.Error(); err.Error() != want {
		t.Errorf("message = %q, want %q", err.Error(), want)
	}
	plain := unsupportedf("gosmo: script %s: no form", "x")
	if !errors.Is(plain, ErrUnsupported) || errors.Is(plain, ErrNotFound) {
		t.Errorf("plain refusal reaches the wrong sentinels: %v", plain)
	}
}

// msgs builds a driver error carrying every message of a batch, as the driver
// reports it: the last one mirrored into the top-level fields.
func msgs(all ...mssql.Error) mssql.Error {
	last := all[len(all)-1]
	last.All = all
	return last
}

// TestClassifyRefusalReadsTheFirstMessage: a refused DMV read sends Msg 300,
// which names the right, and then the contentless Msg 297 that database/sql
// surfaces. The classification must come from the first. Captured live on
// win10cli, 2026-08-25.
func TestClassifyRefusalReadsTheFirstMessage(t *testing.T) {
	err := fmt.Errorf("gosmo: memory: %w", msgs(
		mssql.Error{Number: 300, Class: 14, Message: "VIEW SERVER PERFORMANCE STATE permission was denied on object 'server', database 'master'."},
		mssql.Error{Number: 297, Class: 16, Message: "The user does not have permission to perform this action."},
	))
	kind, m := ClassifyRefusal(err)
	if kind != PermissionDenied || m == nil || m.Number != 300 {
		t.Fatalf("ClassifyRefusal = %v, %+v; want PermissionDenied from Msg 300", kind, m)
	}
	if !IsPermissionDenied(err) || IsMissingOrDenied(err) {
		t.Error("a stated denial must be IsPermissionDenied and not IsMissingOrDenied")
	}
}

// TestClassifyRefusalKinds pins every measured number to its kind, and the
// filters: an informational message, a message with no text and a non-SQL
// error are never refusals.
func TestClassifyRefusalKinds(t *testing.T) {
	for _, n := range []int32{229, 230, 262, 297, 300, 916} {
		if k, _ := ClassifyRefusal(mssql.Error{Number: n, Class: 14, Message: "x"}); k != PermissionDenied {
			t.Errorf("Msg %d = %v, want PermissionDenied", n, k)
		}
	}
	for _, n := range []int32{1088, 3701, 5011, 15151, 15247} {
		err := mssql.Error{Number: n, Class: 16, Message: "x"}
		if k, _ := ClassifyRefusal(err); k != MissingOrDenied {
			t.Errorf("Msg %d = %v, want MissingOrDenied", n, k)
		}
		if IsPermissionDenied(err) || !IsMissingOrDenied(err) {
			t.Errorf("Msg %d: the ambiguity must not be narrowed to a denial", n)
		}
	}
	for name, err := range map[string]error{
		"other number":  mssql.Error{Number: 208, Class: 16, Message: "Invalid object name 'x'."},
		"informational": mssql.Error{Number: 229, Class: 10, Message: "x"},
		"no text":       mssql.Error{Number: 229, Class: 14},
		"not SQL":       errors.New("The SELECT permission was denied"),
		"nil":           nil,
	} {
		if k, m := ClassifyRefusal(err); k != NotRefused || m != nil {
			t.Errorf("%s: ClassifyRefusal = %v, %+v; want NotRefused, nil", name, k, m)
		}
	}
}

// TestIsAlreadyExistsMatchesTheNumberInAnyLanguage: the message follows the
// session's language, so only the number is read — 15025 and 15023 confirmed
// live under SET LANGUAGE Deutsch.
func TestIsAlreadyExistsMatchesTheNumberInAnyLanguage(t *testing.T) {
	for _, c := range []struct {
		name string
		err  error
		want bool
	}{
		{"login, German", mssql.Error{Number: 15025, Message: `Der Serverprinzipal "x_login" ist bereits vorhanden.`}, true},
		{"user, German", mssql.Error{Number: 15023, Message: `Der Benutzer, die Gruppe oder die Rolle "x_user" ist in der aktuellen Datenbank bereits vorhanden.`}, true},
		{"wrapped", fmt.Errorf("gosmo: create login: %w", mssql.Error{Number: 15025, Message: "déjà"}), true},
		{"object", mssql.Error{Number: 2714, Message: "There is already an object named 'T' in the database."}, true},
		{"database", mssql.Error{Number: 1801, Message: "Database 'D' already exists."}, true},
		{"index", mssql.Error{Number: 1913, Message: "The operation failed because an index or statistics with name 'IX' already exists on table 'T'."}, true},
		{"in a later message", msgs(
			mssql.Error{Number: 15025, Class: 16, Message: "exists"},
			mssql.Error{Number: 3609, Class: 16, Message: "The transaction ended in the trigger."},
		), true},
		{"other number", mssql.Error{Number: 15247, Message: "User does not have permission; the object already exists elsewhere"}, false},
		{"not SQL", errors.New("login already exists"), false},
		{"nil", nil, false},
	} {
		if got := IsAlreadyExists(c.err); got != c.want {
			t.Errorf("%s: IsAlreadyExists = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestSendMailRefusalsReachTheirSentinel: each refusal sp_send_dbmail raises
// is reachable through errors.Is, keeps its text, and keeps the SQL error.
func TestSendMailRefusalsReachTheirSentinel(t *testing.T) {
	for n, want := range map[int32]error{
		14607: ErrMailProfileInvalid,
		14636: ErrMailNoDefaultProfile,
		14641: ErrMailStopped,
		15281: ErrMailXPsDisabled,
	} {
		raw := mssql.Error{Number: n, Class: 16, Message: "the procedure's own text"}
		err := fmt.Errorf("gosmo: send mail: %w", classifyMailSendError(raw))
		if !errors.Is(err, want) {
			t.Errorf("Msg %d: errors.Is(%v) = false", n, want)
		}
		if se, ok := AsSQLError(err); !ok || se.Number != n {
			t.Errorf("Msg %d: the SQL error is no longer reachable", n)
		}
		if err.Error() != "gosmo: send mail: "+raw.Error() {
			t.Errorf("Msg %d: text changed to %q", n, err.Error())
		}
	}
	other := mssql.Error{Number: 208, Class: 16, Message: "x"}
	if _, wrapped := classifyMailSendError(other).(*classifiedError); wrapped {
		t.Error("an unrelated error was rewrapped")
	}
}
