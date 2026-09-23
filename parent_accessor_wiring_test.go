package gosmo

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// -- what this file pins ------------------------------------------------------
//
// 54 types in this package carry a parent back-pointer — an unexported
// `db *Database` or `server *Server` set when the type is scanned or built.
// Until 2026-09-18, 26 of them exposed it and 28 did not, which is close
// enough to a coin flip that no caller could guess: a caller holding a *Rule
// could reach the database it came from, a caller holding a *Login could not
// and had to thread a *Server alongside it.
//
// A child of a table holds `table *Table` and declares `Table() *Table` the
// same way. Index was the one such type without the field until 2026-09-23:
// every index write took its table as a parameter, so passing the wrong one
// compiled and altered an index of the same name on another table.
//
// That is the same defect CLAUDE.md § Conventions records against Database's
// accessors — the shape being unguessable from the type — and it drifts back
// the same way, one new type at a time. So the package is parsed, and every
// struct with a `db *Database` field must declare `Database() *Database`,
// every struct with a `server *Server` field must declare `Server() *Server`.
//
// Only the back-pointer is in scope. Catalog state stays an exported field
// and an accessor over one is the holdout this convention exists to prevent;
// a derivation (Database.IsSystem, Database.IsSnapshot) stays a method and is
// not a back-pointer, so neither is checked here.

// parentAccessorExceptions lists types that deliberately do not expose their
// back-pointer, with the reason. It is empty: every type with the field exposes
// it, and an entry here is a claim that a caller holding the child must not
// be able to reach its parent.
var parentAccessorExceptions = map[string]string{}

func TestEveryParentBackPointerHasItsAccessor(t *testing.T) {
	fset := token.NewFileSet()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}

	// typeName -> the accessor its field requires ("Database" or "Server").
	want := map[string]string{}
	// typeName -> the *T-returning zero-argument methods it declares.
	got := map[string]map[string]string{}

	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, file, nil, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", file, err)
		}
		for _, d := range f.Decls {
			switch d := d.(type) {
			case *ast.GenDecl:
				if d.Tok != token.TYPE {
					continue
				}
				for _, spec := range d.Specs {
					ts, ok := spec.(*ast.TypeSpec)
					if !ok {
						continue
					}
					st, ok := ts.Type.(*ast.StructType)
					if !ok {
						continue
					}
					if kind := backPointerKind(st); kind != "" {
						want[ts.Name.Name] = kind
					}
				}
			case *ast.FuncDecl:
				if d.Recv == nil || len(d.Recv.List) == 0 {
					continue
				}
				recv := receiverType(d)
				if recv == "" || d.Type.Params.NumFields() != 0 {
					continue
				}
				if d.Type.Results == nil || len(d.Type.Results.List) != 1 {
					continue
				}
				star, ok := d.Type.Results.List[0].Type.(*ast.StarExpr)
				if !ok {
					continue
				}
				id, ok := star.X.(*ast.Ident)
				if !ok {
					continue
				}
				if got[recv] == nil {
					got[recv] = map[string]string{}
				}
				got[recv][d.Name.Name] = id.Name
			}
		}
	}

	if len(want) == 0 {
		t.Fatal("no back-pointer fields found — the scan below would pass vacuously")
	}

	used := map[string]bool{}
	for typ, kind := range want {
		if reason, ok := parentAccessorExceptions[typ]; ok {
			if reason == "" {
				t.Errorf("%s is in parentAccessorExceptions with no reason", typ)
			}
			used[typ] = true
			continue
		}
		if got[typ][kind] != kind {
			t.Errorf("%s holds a %s back-pointer but declares no %s() *%s — add the one-line accessor, or an entry in parentAccessorExceptions with a reason",
				typ, strings.ToLower(kind), kind, kind)
		}
	}
	for typ := range parentAccessorExceptions {
		if !used[typ] {
			t.Errorf("parentAccessorExceptions lists %s, which no longer holds a back-pointer field", typ)
		}
	}
}

// TestNoParentAccessorIsNamedDB keeps the accessor's name uniform. Table.DB
// was the sole outlier until 2026-09-18, and the name it took is already
// spoken for: Server.DB() returns the *sql.DB pool, so "DB" meant two
// different things one method set apart. The three documents that cite the
// back-pointer convention all used Table.DB as the example, which is how the
// outlier came to be taught as the pattern.
func TestNoParentAccessorIsNamedDB(t *testing.T) {
	fset := token.NewFileSet()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, file, nil, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", file, err)
		}
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Name.Name != "DB" {
				continue
			}
			if fn.Recv == nil || len(fn.Recv.List) == 0 {
				continue
			}
			recv := receiverType(fn)
			if recv == "" {
				continue
			}
			if fn.Type.Results == nil || len(fn.Type.Results.List) != 1 {
				continue
			}
			star, ok := fn.Type.Results.List[0].Type.(*ast.StarExpr)
			if !ok {
				continue
			}
			if id, ok := star.X.(*ast.Ident); ok && id.Name == "Database" {
				t.Errorf("%s.DB returns *Database: the back-pointer accessor is named Database(), and DB() already means the *sql.DB pool (%s)",
					recv, fset.Position(fn.Pos()))
			}
		}
	}
}

// backPointerKind reports "Database", "Server" or "Table" when st holds the
// corresponding unexported parent field, and "" otherwise.
func backPointerKind(st *ast.StructType) string {
	for _, field := range st.Fields.List {
		star, ok := field.Type.(*ast.StarExpr)
		if !ok {
			continue
		}
		id, ok := star.X.(*ast.Ident)
		if !ok || (id.Name != "Database" && id.Name != "Server" && id.Name != "Table") {
			continue
		}
		for _, name := range field.Names {
			if name.Name == "db" && id.Name == "Database" {
				return "Database"
			}
			if name.Name == "server" && id.Name == "Server" {
				return "Server"
			}
			if name.Name == "table" && id.Name == "Table" {
				return "Table"
			}
		}
	}
	return ""
}

// receiverType names fn's receiver type without the star, or "" when it is
// not a plain named type.
func receiverType(fn *ast.FuncDecl) string {
	t := fn.Recv.List[0].Type
	if star, ok := t.(*ast.StarExpr); ok {
		t = star.X
	}
	if id, ok := t.(*ast.Ident); ok {
		return id.Name
	}
	return ""
}
