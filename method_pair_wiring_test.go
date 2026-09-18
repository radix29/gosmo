package gosmo

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// -- what this file pins ------------------------------------------------------
//
// CLAUDE.md § Conventions requires every database-touching method in two
// forms: Foo(...) delegating to FooContext(ctx, ...). There are 676 of those
// delegates, they are one line each, and their coverage is 0.0% — gossms uses
// the Context form throughout, so nothing in either repo executes them.
//
// That is iter_wiring_test.go's problem at seven times the scale, and with the
// same failure mode: a delegate that names the wrong sibling still compiles
// whenever the parameter and result types match, and 293 of the 676 sit in
// groups that share a receiver, a parameter list and a result list. The worst
// shapes are the ones where a swap silently inverts a write —
// AddRoleMember/RemoveRoleMember, SuspendDatabase/ResumeDatabase,
// Grant/Deny/Revoke.
//
// Unlike the *Seq wiring this needs no driver and no server: the delegate's
// whole contract is visible in its source. So the package is parsed, and every
// exported method that has a FooContext sibling on the same receiver must be
// exactly
//
//	func (r T) Foo(a A, b ...B) (R, error) { return r.FooContext(context.Background(), a, b...) }
//
// — the callee is the receiver's own Foo+Context, the first argument is
// context.Background(), and the rest are the declared parameters in
// declaration order, variadics forwarded with "...".
//
// Methods whose own name ends in Context are not delegates and are not
// checked here: the …Context → …FromContext / alter…Context / catalogContext
// layers are a different shape, and a Context method has callers.

// delegateExceptions lists Foo/FooContext pairs where Foo is deliberately not
// the mechanical delegate, with the reason — the shape exemptSource uses in
// gossms's doc_links_test.go. It is empty today; an entry here is a claim that
// a caller of Foo gets something FooContext(context.Background(), …) would not.
var delegateExceptions = map[string]string{}

func TestEveryMethodPairDelegatesToItsOwnContextForm(t *testing.T) {
	fset := token.NewFileSet()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	type method struct {
		fn   *ast.FuncDecl
		file string
	}
	// Keyed "T.Foo", receiver type without the star: the pair is declared on
	// the same type whether or not either half takes a pointer.
	methods := map[string]method{}
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
			t.Fatalf("parsing %s: %v", name, err)
		}
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Recv == nil || len(fn.Recv.List) != 1 || !fn.Name.IsExported() {
				continue
			}
			methods[receiverType(fn)+"."+fn.Name.Name] = method{fn, name}
		}
	}
	if len(methods) < 1500 {
		t.Fatalf("found only %d exported methods; the parser has stopped seeing the package", len(methods))
	}

	checked := 0
	for key, m := range methods {
		if strings.HasSuffix(key, "Context") {
			continue
		}
		if _, ok := methods[key+"Context"]; !ok {
			continue
		}
		if why := delegateExceptions[key]; why != "" {
			continue
		}
		checked++
		if problem := checkDelegate(m.fn); problem != "" {
			t.Errorf("%s: %s: %s", fset.Position(m.fn.Pos()), key, problem)
		}
	}
	if checked < 600 {
		t.Fatalf("checked only %d delegates; the pair detection has stopped working", checked)
	}
	for key := range delegateExceptions {
		if _, ok := methods[key]; !ok {
			t.Errorf("delegateExceptions names %s, which no longer exists", key)
		}
	}
}

// receiverType is the receiver's type name, pointer or not.
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

// checkDelegate returns "" when fn is the mechanical delegate to its own
// Context form, or a description of the first thing that is wrong with it.
func checkDelegate(fn *ast.FuncDecl) string {
	if fn.Body == nil || len(fn.Body.List) != 1 {
		return "is not a single-statement delegate to " + fn.Name.Name + "Context"
	}
	var call *ast.CallExpr
	switch s := fn.Body.List[0].(type) {
	case *ast.ReturnStmt:
		if len(s.Results) != 1 {
			return "does not return one call to " + fn.Name.Name + "Context"
		}
		c, ok := s.Results[0].(*ast.CallExpr)
		if !ok {
			return "does not return a call to " + fn.Name.Name + "Context"
		}
		call = c
	case *ast.ExprStmt:
		c, ok := s.X.(*ast.CallExpr)
		if !ok {
			return "is not a call to " + fn.Name.Name + "Context"
		}
		call = c
	default:
		return "is not a single call to " + fn.Name.Name + "Context"
	}

	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return "does not call a method on its receiver"
	}
	recv, ok := sel.X.(*ast.Ident)
	if !ok {
		return "does not call a method on its receiver"
	}
	if want := receiverName(fn); want == "" || recv.Name != want {
		return "calls " + recv.Name + "." + sel.Sel.Name + ", not a method on its own receiver"
	}
	if sel.Sel.Name != fn.Name.Name+"Context" {
		return "delegates to " + sel.Sel.Name + ", not " + fn.Name.Name + "Context"
	}

	if len(call.Args) == 0 || !isContextBackground(call.Args[0]) {
		return "does not pass context.Background() as its first argument"
	}

	want := paramNames(fn)
	got := call.Args[1:]
	if len(got) != len(want) {
		return "forwards " + strconv.Itoa(len(got)) + " arguments for " + strconv.Itoa(len(want)) + " parameters"
	}
	for i, name := range want {
		id, ok := got[i].(*ast.Ident)
		if !ok || id.Name != name {
			return "forwards " + exprText(got[i]) + " where parameter " + name + " is declared"
		}
	}
	if isVariadic(fn) && call.Ellipsis == token.NoPos {
		return "does not forward its variadic parameter with ..."
	}
	if !isVariadic(fn) && call.Ellipsis != token.NoPos {
		return "forwards with ... but declares no variadic parameter"
	}
	return ""
}

// receiverName is the receiver's identifier, or "" when it is unnamed — a
// delegate cannot be written without one.
func receiverName(fn *ast.FuncDecl) string {
	names := fn.Recv.List[0].Names
	if len(names) != 1 {
		return ""
	}
	return names[0].Name
}

// paramNames is every declared parameter name in declaration order, with
// grouped declarations ("a, b string") flattened.
func paramNames(fn *ast.FuncDecl) []string {
	var out []string
	if fn.Type.Params == nil {
		return out
	}
	for _, f := range fn.Type.Params.List {
		if len(f.Names) == 0 {
			// Unnamed: nothing to forward, so the delegate cannot be
			// mechanical. Reported as a count mismatch below.
			out = append(out, "_")
			continue
		}
		for _, n := range f.Names {
			out = append(out, n.Name)
		}
	}
	return out
}

func isVariadic(fn *ast.FuncDecl) bool {
	if fn.Type.Params == nil || len(fn.Type.Params.List) == 0 {
		return false
	}
	last := fn.Type.Params.List[len(fn.Type.Params.List)-1]
	_, ok := last.Type.(*ast.Ellipsis)
	return ok
}

func isContextBackground(e ast.Expr) bool {
	call, ok := e.(*ast.CallExpr)
	if !ok || len(call.Args) != 0 {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Background" {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == "context"
}

func exprText(e ast.Expr) string {
	if id, ok := e.(*ast.Ident); ok {
		return id.Name
	}
	return "an expression"
}
