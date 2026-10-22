package tsql

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestQuote(t *testing.T) {
	for _, tc := range []struct{ got, want string }{
		{QuoteName("a]b"), "[a]]b]"},
		{QuoteName(""), "[]"},
		{QuoteName("[x]"), "[[x]]]"},
		{QualifiedName("dbo", "Orders"), "[dbo].[Orders]"},
		{QualifiedName("", "Orders"), "[Orders]"},
		{QualifiedName("my]schema", "a]b"), "[my]]schema].[a]]b]"},
		{QualifiedName("my.schema", "a.b"), "[my.schema].[a.b]"},
		{QuoteLiteral("it's"), "N'it''s'"},
		{QuoteAnsiLiteral("it's"), "'it''s'"},
		{QuoteAnsiLiteral(""), "''"},
	} {
		if tc.got != tc.want {
			t.Errorf("got %s, want %s", tc.got, tc.want)
		}
	}
}

func TestUnquoteName(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"[a]]b]", "a]b"},
		{"[a]]]]b]", "a]]b"},
		{"[[x]", "[x"},
		{`"a""b"`, `a"b`},
		{"[]", ""},
		{"plain", "plain"},
		{"[open", "[open"},
		{"[a]b]", "[a]b]"},         // an undoubled ']' inside: not one quoted part
		{"[dbo].[t]", "[dbo].[t]"}, // two parts
		{"[", "["},
		{`"a"b"`, `"a"b"`},
	} {
		if got := UnquoteName(tc.in); got != tc.want {
			t.Errorf("UnquoteName(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	for _, name := range []string{"a]b", "]]", "x", "", "[y]", `q"`} {
		if got := UnquoteName(QuoteName(name)); got != name {
			t.Errorf("UnquoteName(QuoteName(%q)) = %q", name, got)
		}
	}
}

// TestImportsOnlyStdlib keeps this package a leaf: the reason it exists is
// that importing it does not link the mssql driver. A standard-library
// import path has no dot in its first element.
func TestImportsOnlyStdlib(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		ast, err := parser.ParseFile(token.NewFileSet(), f, src, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range ast.Imports {
			path, _ := strconv.Unquote(imp.Path.Value)
			if first, _, _ := strings.Cut(path, "/"); strings.Contains(first, ".") {
				t.Errorf("%s imports %s: package tsql may import only the standard library", f, path)
			}
		}
	}
}
