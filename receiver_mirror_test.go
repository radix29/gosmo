package gosmo

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// -- what this file pins ------------------------------------------------------
//
// A write that changes a scanned field mirrors it onto the receiver through
// setIfApplied (CLAUDE.md § Script mode), so a caller reading the handle
// afterwards sees what it wrote. Five setters did not until 2026-10-03 —
// Login.SetDefaultLanguage beside a SetDefaultDatabase that did, among them —
// and a caller doing both saw one value updated and one stale.
//
// So the package is parsed, and every exported Set<X> method on a type with a
// field <X> or Is<X> must call setIfApplied (or setPtrIfApplied), call a
// method on its receiver or a package function that does (an Alter it
// delegates to, setReplicaKeyword), or be listed below with the reason it
// does not.

// receiverMirrorExceptions lists Set<X> methods that deliberately leave the
// field <X> as it was, keyed "Type.Method".
var receiverMirrorExceptions = map[string]string{}

func TestEverySetterMirrorsItsField(t *testing.T) {
	fset := token.NewFileSet()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fields := map[string]map[string]bool{} // type -> its field names
	methods := map[string]*ast.FuncDecl{}  // "Type.Method" -> declaration
	funcs := map[string]*ast.FuncDecl{}    // package function -> declaration
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
				for _, spec := range d.Specs {
					ts, ok := spec.(*ast.TypeSpec)
					if !ok {
						continue
					}
					st, ok := ts.Type.(*ast.StructType)
					if !ok {
						continue
					}
					names := map[string]bool{}
					for _, fld := range st.Fields.List {
						for _, n := range fld.Names {
							names[n.Name] = true
						}
					}
					fields[ts.Name.Name] = names
				}
			case *ast.FuncDecl:
				switch {
				case d.Body == nil:
				case d.Recv == nil:
					funcs[d.Name.Name] = d
				case len(d.Recv.List) == 1:
					methods[recvTypeName(d.Recv.List[0].Type)+"."+d.Name.Name] = d
				}
			}
		}
	}

	// mirrors reports whether the method or package function key ("T.M" or
	// "f") calls setIfApplied, directly or through a method on its own
	// receiver or a package function.
	var mirrors func(key string, seen map[string]bool) bool
	mirrors = func(key string, seen map[string]bool) bool {
		fd, typ := funcs[key], ""
		if i := strings.IndexByte(key, '.'); i >= 0 {
			fd, typ = methods[key], key[:i]
		}
		if fd == nil || seen[key] {
			return false
		}
		seen[key] = true
		recv := ""
		if fd.Recv != nil && len(fd.Recv.List[0].Names) == 1 {
			recv = fd.Recv.List[0].Names[0].Name
		}
		calls := func(name string) bool {
			return name == "setIfApplied" || name == "setPtrIfApplied" || mirrors(name, seen)
		}
		found := false
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || found {
				return !found
			}
			switch fn := call.Fun.(type) {
			case *ast.Ident:
				found = calls(fn.Name)
			case *ast.IndexExpr: // f[T](…)
				if id, ok := fn.X.(*ast.Ident); ok {
					found = calls(id.Name)
				}
			case *ast.SelectorExpr:
				if id, ok := fn.X.(*ast.Ident); ok && recv != "" && id.Name == recv {
					found = mirrors(typ+"."+fn.Sel.Name, seen)
				}
			}
			return !found
		})
		return found
	}

	var checked, missing []string
	for key, fd := range methods {
		typ, name := key[:strings.IndexByte(key, '.')], fd.Name.Name
		field, ok := strings.CutPrefix(name, "Set")
		if !ok || field == "" || !ast.IsExported(name) || !ast.IsExported(typ) {
			continue
		}
		if !fields[typ][field] && !fields[typ]["Is"+field] {
			continue
		}
		checked = append(checked, key)
		_, excepted := receiverMirrorExceptions[key]
		switch m := mirrors(key, map[string]bool{}); {
		case m && excepted:
			t.Errorf("%s mirrors its field: remove it from receiverMirrorExceptions", key)
		case !m && !excepted:
			missing = append(missing, key)
		}
	}
	sort.Strings(missing)
	for _, key := range missing {
		t.Errorf("%s writes the field %s but never mirrors it onto the receiver with setIfApplied — "+
			"mirror it, or list it in receiverMirrorExceptions with the reason", key, strings.TrimPrefix(key[strings.IndexByte(key, '.')+1:], "Set"))
	}
	for key := range receiverMirrorExceptions {
		if methods[key] == nil {
			t.Errorf("receiverMirrorExceptions names %s, which no longer exists", key)
		}
	}
	if len(checked) < 10 {
		t.Errorf("only %d setters checked — the scan has stopped finding them", len(checked))
	}
}

// recvTypeName is the type name of a method receiver: T for T, *T or T[P].
func recvTypeName(e ast.Expr) string {
	switch e := e.(type) {
	case *ast.StarExpr:
		return recvTypeName(e.X)
	case *ast.IndexExpr:
		return recvTypeName(e.X)
	case *ast.Ident:
		return e.Name
	}
	return ""
}
