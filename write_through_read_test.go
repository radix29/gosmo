package gosmo

import (
	"go/ast"
	"go/parser"
	"go/token"
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
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		parsed = append(parsed, f)
	}
	if len(parsed) == 0 {
		t.Fatal("no source files checked; the glob is wrong and this test proves nothing")
	}

	// Package-level consts and vars, for an identifier the parser's file-scope
	// resolution can't follow into another file.
	pkgValues := map[string]ast.Expr{}
	for _, f := range parsed {
		for _, d := range f.Decls {
			g, ok := d.(*ast.GenDecl)
			if !ok {
				continue
			}
			for _, s := range g.Specs {
				if vs, ok := s.(*ast.ValueSpec); ok {
					for i, n := range vs.Names {
						if i < len(vs.Values) {
							pkgValues[n.Name] = vs.Values[i]
						}
					}
				}
			}
		}
	}

	// literals collects the string literals expr is built from.
	var literals func(expr ast.Node, seen map[*ast.Object]bool) []string
	literals = func(expr ast.Node, seen map[*ast.Object]bool) []string {
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
				if n.Obj == nil {
					if v, ok := pkgValues[n.Name]; ok {
						out = append(out, literals(v, seen)...)
					}
					return true
				}
				if seen[n.Obj] {
					return true
				}
				seen[n.Obj] = true
				switch d := n.Obj.Decl.(type) {
				case *ast.ValueSpec:
					for i, name := range d.Names {
						if name.Name == n.Name && i < len(d.Values) {
							out = append(out, literals(d.Values[i], seen)...)
						}
					}
				case *ast.AssignStmt:
					for i, lhs := range d.Lhs {
						if id, ok := lhs.(*ast.Ident); ok && id.Name == n.Name && i < len(d.Rhs) {
							out = append(out, literals(d.Rhs[i], seen)...)
						}
					}
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
			for _, lit := range literals(call.Args[i], map[*ast.Object]bool{}) {
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
