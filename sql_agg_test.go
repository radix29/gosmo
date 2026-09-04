package gosmo

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// commaList's two details are the ones a plausible simplification drops, so
// pin them by name rather than by comparing the whole rendering: TYPE + .value
// (without which an identifier containing &, < or > comes back XML-escaped)
// and the STUFF that removes the leading separator.
func TestCommaListKeepsTheTypedValueAndStripsTheLeadingSeparator(t *testing.T) {
	got := commaList("c.name", "\n FROM sys.columns c WHERE c.object_id = t.object_id", "c.column_id")
	for _, want := range []string{
		"STUFF((SELECT ',' + c.name",
		"ORDER BY c.column_id",
		"FOR XML PATH(''), TYPE).value('.', 'nvarchar(max)')",
		", 1, 1, '')",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("commaList does not contain %q:\n%s", want, got)
		}
	}
	// An aggregate whose order does not matter emits no ORDER BY at all —
	// a bare "ORDER BY" with nothing after it is a syntax error.
	if unordered := commaList("te.type_desc", "\n FROM sys.trigger_events te", ""); strings.Contains(unordered, "ORDER BY") {
		t.Errorf("empty orderBy still emitted an ORDER BY:\n%s", unordered)
	}
}

// C1: STRING_AGG is SQL Server 2017 and gosmo's floor is 2016 SP1, where it is
// "not a recognized built-in function name" — which fails the whole read, not
// just the column. commaList is the one rendering; a new query that reaches for
// STRING_AGG instead is caught here rather than on an instance nobody owns.
//
// String literals only, via the parser: a doc comment is allowed to name the
// function it replaces, and a source-text grep cannot tell the two apart.
func TestNoQueryUsesStringAgg(t *testing.T) {
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
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		f, err := parser.ParseFile(fset, name, src, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		checked++
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			if strings.Contains(lit.Value, "STRING_AGG") {
				t.Errorf("%s: a SQL literal uses STRING_AGG, which SQL Server 2016 does not have — use commaList", fset.Position(lit.Pos()))
			}
			return true
		})
	}
	if checked == 0 {
		t.Fatal("no source files checked; the glob is wrong and this test proves nothing")
	}
}

// C3: CREATE OR ALTER is what puts gosmo's floor at 2016 SP1 rather than 2016
// RTM, and it is worth one site rather than several — the RTM rewrite is then
// one statement, not a survey. This pins the inventory: string literals only,
// via the parser, because scripter.go's doc comment names the keywords while
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
