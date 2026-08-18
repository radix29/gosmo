package gosmo

import (
	"context"
	"strings"
	"testing"
)

// The script_*_write_test.go files pin the exact T-SQL every write method
// emits, using WithScript as a serverless harness: a zero-value Server or a
// Database built by hand is enough, because the exec chokepoints consult the
// collector before they ever reach a *sql.DB.
//
// They assert the whole statement, not a substring. The class of bug these
// exist to catch is a quoting one — a name that loses its brackets, a literal
// whose apostrophe stops being doubled — and a substring match passes right
// over both. Every case therefore feeds a quote-hostile value through each
// parameter that reaches the statement text:
//
//   - o'brien       an apostrophe, which must double inside a literal
//   - a]b           a closing bracket, which must double inside brackets
//   - Sales.Archive a dot, which resolves as a two-part name unbracketed
//
// A method that takes a name and does not bracket it, or a literal that does
// not escape, shows up here as a diff rather than as a live failure months
// later.

// scriptCase is one write call and the statement it must produce.
type scriptCase struct {
	name string
	call func(context.Context) error
	want string
}

// runScriptCases runs each case under its own collector and compares the one
// statement it captured against want.
func runScriptCases(t *testing.T, cases []scriptCase) {
	t.Helper()
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ctx, script := WithScript(context.Background())
			if err := c.call(ctx); err != nil {
				t.Fatalf("%s under WithScript: %v", c.name, err)
			}
			if len(script.Statements) != 1 {
				t.Fatalf("Statements = %d, want 1:\n%s", len(script.Statements),
					strings.Join(script.Statements, "\n---\n"))
			}
			if got := script.Statements[0]; got != c.want {
				t.Errorf("statement mismatch\n got: %q\nwant: %q", got, c.want)
			}
		})
	}
}

// scriptTestDB is the database every case in these files writes against. Its
// name carries an apostrophe so the USE prefix Database.exec adds is covered
// too — a name that is bracket-quoted needs no escaping there, and a build
// that starts doubling the apostrophe as well would name a different
// database.
func scriptTestDB() *Database { return &Database{server: &Server{}, name: "App'DB"} }

// scriptUsePrefix is what Database.exec prepends to every captured statement.
const scriptUsePrefix = "USE [App'DB];\n"
