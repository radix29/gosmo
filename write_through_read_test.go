package gosmo

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// K1 (review plan 2026-10-02): a write never goes through the read helpers.
// query/queryRow/queryRowScan retry on a transient connection failure, and a
// connection that breaks after the server ran a write is exactly such a
// failure — so the write runs twice. sp_send_dbmail was the case: a retried
// send queued the message twice. A write that reads a value back uses execScan
// (CLAUDE.md § Conventions).
//
// This scans the SQL handed to every read-helper call: the argument's string
// literals, following an identifier to its const/var or local assignment.
// A temp table or table variable written inside a read is fine — it is the
// read's own scratch, gone with the connection — so INSERT/UPDATE/DELETE on a
// #name or @name are not counted.
//
// Identifiers resolve through go/types over this package's own declarations:
// every import is an empty stub, so nothing waits on type-checking the
// driver, and the type errors that leaves are ignored. A const, var or local
// holding SQL is declared here, so it resolves all the same.
func TestNoWriteGoesThroughARetryingRead(t *testing.T) {
	write := regexp.MustCompile(`(?i)\bEXEC(?:UTE)?\s+(?:\S+\.)?sp_(?:send|add|update|delete)\w*` +
		`|\bINSERT\s+[^\s#@(]` +
		`|\bUPDATE\s+[^\s#@]\S*\s+SET\b` +
		`|\bDELETE\s+[^\s#@]`)
	// INTO/FROM are optional, and RE2 can't look past them to the target, so
	// they are dropped first: "INSERT INTO @t" then reads as "INSERT @t".
	noise := regexp.MustCompile(`(?i)\b(INSERT|DELETE)\s+(?:INTO|FROM)\s+`)

	// The SQL argument's index for each read helper.
	sqlArg := map[string]int{"query": 1, "queryRow": 2, "queryRowScan": 1}

	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var parsed []*ast.File
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		parsed = append(parsed, f)
	}
	if len(parsed) == 0 {
		t.Fatal("no source files checked; the glob is wrong and this test proves nothing")
	}

	info := &types.Info{Defs: map[*ast.Ident]types.Object{}, Uses: map[*ast.Ident]types.Object{}}
	conf := types.Config{Importer: stubImporter{}, Error: func(error) {}}
	conf.Check("github.com/radix29/gosmo", fset, parsed, info)

	// The value each const, var or := local was declared with, package-level
	// or local, by object.
	values := map[types.Object]ast.Expr{}
	for _, f := range parsed {
		ast.Inspect(f, func(n ast.Node) bool {
			switch d := n.(type) {
			case *ast.ValueSpec:
				for i, name := range d.Names {
					if o := info.Defs[name]; o != nil && i < len(d.Values) {
						values[o] = d.Values[i]
					}
				}
			case *ast.AssignStmt:
				if d.Tok != token.DEFINE || len(d.Lhs) != len(d.Rhs) {
					return true
				}
				for i, lhs := range d.Lhs {
					if id, ok := lhs.(*ast.Ident); ok {
						if o := info.Defs[id]; o != nil {
							values[o] = d.Rhs[i]
						}
					}
				}
			}
			return true
		})
	}

	// literals collects the string literals expr is built from.
	var literals func(expr ast.Node, seen map[types.Object]bool) []string
	literals = func(expr ast.Node, seen map[types.Object]bool) []string {
		var out []string
		ast.Inspect(expr, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.BasicLit:
				if n.Kind == token.STRING {
					if s, err := strconv.Unquote(n.Value); err == nil {
						out = append(out, s)
					}
				}
			case *ast.Ident:
				o := info.Uses[n]
				if o == nil || seen[o] {
					return true
				}
				seen[o] = true
				if v, ok := values[o]; ok {
					out = append(out, literals(v, seen)...)
				}
			}
			return true
		})
		return out
	}

	calls := 0
	for _, f := range parsed {
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			i, ok := sqlArg[sel.Sel.Name]
			if !ok || i >= len(call.Args) {
				return true
			}
			calls++
			for _, lit := range literals(call.Args[i], map[types.Object]bool{}) {
				if m := write.FindString(noise.ReplaceAllString(lit, "$1 ")); m != "" {
					t.Errorf("%s: %s is handed SQL containing %q — a retried read runs a write twice; use exec, or execScan for a write that reads a value back",
						fset.Position(call.Pos()), sel.Sel.Name, m)
				}
			}
			return true
		})
	}
	if calls == 0 {
		t.Fatal("no read-helper calls found; the helper names are wrong and this test proves nothing")
	}
}

// stubImporter hands back an empty package for every import: the check needs
// only the package's own declarations resolved.
type stubImporter struct{}

func (stubImporter) Import(path string) (*types.Package, error) {
	p := types.NewPackage(path, filepath.Base(path))
	p.MarkComplete()
	return p, nil
}
