package gosmo

import (
	"database/sql"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// jsonList is a FOR JSON subquery with the value under key "v", and the
// order clause only when asked for — a bare "ORDER BY" is a syntax error.
func TestJSONListRendersAForJSONSubquery(t *testing.T) {
	got := jsonList("c.name", "\n FROM sys.columns c WHERE c.object_id = t.object_id", "c.column_id")
	for _, want := range []string{
		"(SELECT c.name AS v",
		"ORDER BY c.column_id",
		"FOR JSON PATH)",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("jsonList does not contain %q:\n%s", want, got)
		}
	}
	if unordered := jsonList("te.type_desc", "\n FROM sys.trigger_events te", ""); strings.Contains(unordered, "ORDER BY") {
		t.Errorf("empty orderBy still emitted an ORDER BY:\n%s", unordered)
	}
}

// T8: the point of the JSON form is that a value containing the old
// separator, or anything JSON itself escapes, comes back whole.
func TestDecodeJSONListKeepsSeparatorsInsideValues(t *testing.T) {
	col := sql.NullString{Valid: true,
		String: `[{"v":"ref,1"},{"v":"r, x"},{"v":"a\"b\\c"},{"v":"<&>"}]`}
	got, err := decodeJSONList(col)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"ref,1", "r, x", `a"b\c`, "<&>"}
	if !slices.Equal(got, want) {
		t.Errorf("decodeJSONList = %q, want %q", got, want)
	}
	// Zero rows: FOR JSON yields NULL, which is no list at all.
	if got, err := decodeJSONList(sql.NullString{}); err != nil || got != nil {
		t.Errorf("NULL decoded to %q, %v; want nil, nil", got, err)
	}
	if _, err := decodeJSONList(sql.NullString{Valid: true, String: "a,b"}); err == nil {
		t.Error("a non-JSON column decoded without error")
	}
}

// T8: every name list is read through jsonList. A FOR XML PATH aggregate
// joins with a separator Go then splits on, which is the bug jsonList
// replaced; string literals only, so a comment may still name the old form.
func TestNoQueryAggregatesWithForXMLPath(t *testing.T) {
	forEachSQLLiteral(t, func(pos token.Position, lit string) {
		if strings.Contains(strings.ToUpper(lit), "FOR XML PATH") {
			t.Errorf("%s: a SQL literal aggregates with FOR XML PATH — use jsonList", pos)
		}
	})
}

// forEachSQLLiteral calls fn with every string literal in gosmo's non-test
// source.
func forEachSQLLiteral(t *testing.T, fn func(token.Position, string)) {
	t.Helper()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	checked := 0
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		checked++
		ast.Inspect(f, func(n ast.Node) bool {
			if lit, ok := n.(*ast.BasicLit); ok && lit.Kind == token.STRING {
				fn(fset.Position(lit.Pos()), lit.Value)
			}
			return true
		})
	}
	if checked == 0 {
		t.Fatal("no source files checked; the glob is wrong and this test proves nothing")
	}
}

// C1: STRING_AGG is SQL Server 2017 and gosmo's floor is 2016 SP1, where it is
// "not a recognized built-in function name" — which fails the whole read, not
// just the column. jsonList is the one rendering; a new query that reaches for
// STRING_AGG instead is caught here rather than on an instance nobody owns.
//
// String literals only, via the parser: a doc comment is allowed to name the
// function it replaces, and a source-text grep cannot tell the two apart.
func TestNoQueryUsesStringAgg(t *testing.T) {
	forEachSQLLiteral(t, func(pos token.Position, lit string) {
		if strings.Contains(lit, "STRING_AGG") {
			t.Errorf("%s: a SQL literal uses STRING_AGG, which SQL Server 2016 does not have — use jsonList", pos)
		}
	})
}

// C3: CREATE OR ALTER is what puts gosmo's floor at 2016 SP1 rather than 2016
// RTM, and it is worth one site rather than several — the RTM rewrite is then
// one statement, not a survey. This pins the inventory: string literals only,
// via the parser, because scripter_module.go's doc comment names the keywords while
// its code only recognises a definition the server already stored.
func TestOnlyKnownSitesEmitCreateOrAlter(t *testing.T) {
	// The one statement gosmo builds that 2016 RTM cannot parse. Adding a file
	// here is a decision to raise the effective floor for another operation;
	// make it deliberately, and say so in README.md's version section.
	allowed := map[string]bool{"procedure.go": true}

	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	found := map[string]bool{}
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		f, err := parser.ParseFile(fset, name, src, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			if !strings.Contains(strings.ToUpper(lit.Value), "CREATE OR ALTER") {
				return true
			}
			found[name] = true
			if !allowed[name] {
				t.Errorf("%s: a SQL literal emits CREATE OR ALTER, which SQL Server 2016 RTM "+
					"cannot parse — every such site raises the supported floor, so add it to "+
					"allowed here and to README.md deliberately", fset.Position(lit.Pos()))
			}
			return true
		})
	}
	for name := range allowed {
		if !found[name] {
			t.Errorf("%s no longer emits CREATE OR ALTER; drop it from allowed, and if it was "+
				"the last one the floor can fall back to 2016 RTM", name)
		}
	}
}
