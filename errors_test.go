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
