package gosmo

import (
	"errors"
	"fmt"
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
