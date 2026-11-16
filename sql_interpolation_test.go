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

// A4 (review plan 2026-11-15): a string field read from the catalog or handed
// in by a caller never reaches a SQL statement through a bare %s. A keyword
// such as sys.tables.lock_escalation_desc is safe only while the server keeps
// to its documented set, and a request field only while every caller
// validates it; either way the statement trusts text it did not check. Names
// go through QuoteName/qualifiedName, literals through QuoteLiteral, and a
// keyword through a closed set (scriptKeyword, or a typed enum's own check).
//
// This flags every fmt.Sprintf/Fprintf whose format reads as SQL and whose
// %s argument is an exported struct field of string kind, bare or converted
// with string(). Exported fields are what a catalog read fills and a caller
// hands in; an unexported one (moduleKind.keyword, xeScope.on, boundChange's
// bound parameters) is set only from this package's own constants. A field
// whose value is checked before the statement is built, or that is T-SQL text
// by design, is listed in checkedFields with where or why; a new one needs
// the same, or a closed-set mapping, not just an entry.
//
// Types resolve through go/types over this package alone, as in
// TestNoWriteGoesThroughARetryingRead: a field of a local struct resolves; one
// of an imported type would not, and none is interpolated today.
func TestNoRawStringFieldIsInterpolatedIntoSQL(t *testing.T) {
	// "file:Type.Field" → where the value is checked before any statement is
	// built, or why it is T-SQL text by design.
	checkedFields := map[string]string{
		"index_create.go:CreateIndexRequest.DataCompression":      "CreateIndexRequest.validateCompression",
		"index_create.go:CreateIndexRequest.SecondaryXMLType":     "CreateIndexRequest.validateXML",
		"index_create.go:CreateIndexRequest.Tessellation":         "CreateIndexRequest.validateSpatial",
		"partition.go:CreatePartitionFunctionRequest.InputType":   "CreatePartitionFunction: validDataType",
		"sequence_synonym.go:CreateSynonymRequest.BaseObject":     "CreateSynonym: validQualifiedObjectName",
		"server_databases.go:CreateDatabaseRequest.RecoveryModel": "CreateDatabase: validRecoveryModel",
		"scripter_database.go:ChangeTrackingInfo.RetentionUnit":   "the enclosing if: changeTrackingRetentionUnits",
		"query_store.go:QueryStoreOptions.CaptureMode":            "queryStoreOnOptions: queryStoreCaptureModes",
		"agent_category.go:CreateCategoryRequest.Class":           "CreateCategory: validCategoryClass",
		"agent_category.go:Category.Class":                        "Category.Drop: validCategoryClass",
		// T-SQL text by design: the caller's or the catalog's own code.
		"procedure.go:CreateStoredProcedureRequest.Body": "the procedure body the caller wrote",
		"scripter_objects.go:CheckConstraint.Definition": "sys.check_constraints.definition, an expression",
		// A parameter name follows identifier rules and cannot be delimited,
		// so the catalog's spelling is the only valid one.
		"scripter_dml.go:Parameter.Name": "sys.parameters.name, an undelimitable @identifier",
	}

	sqlish := regexp.MustCompile(`\b(CREATE|ALTER|DROP|EXEC|SET|WITH|SELECT|INSERT|UPDATE|DELETE|GRANT|DENY|REVOKE|BACKUP|RESTORE|DBCC|ENCRYPTED|AUTHENTICATION)\b|[A-Z_]{3,} = %`)

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
	info := &types.Info{
		Types:      map[ast.Expr]types.TypeAndValue{},
		Selections: map[*ast.SelectorExpr]*types.Selection{},
		Defs:       map[*ast.Ident]types.Object{},
		Uses:       map[*ast.Ident]types.Object{},
	}
	conf := types.Config{Importer: stubImporter{}, Error: func(error) {}}
	conf.Check("github.com/radix29/gosmo", fset, parsed, info)

	// rawField returns "Type.Field" when e is a string-kind struct field,
	// bare or wrapped in a conversion such as string(x.F).
	var rawField func(e ast.Expr) string
	rawField = func(e ast.Expr) string {
		switch e := e.(type) {
		case *ast.ParenExpr:
			return rawField(e.X)
		case *ast.CallExpr:
			if tv, ok := info.Types[e.Fun]; ok && tv.IsType() && len(e.Args) == 1 {
				return rawField(e.Args[0])
			}
		case *ast.SelectorExpr:
			sel := info.Selections[e]
			if sel == nil || sel.Kind() != types.FieldVal || !e.Sel.IsExported() {
				return ""
			}
			if b, ok := sel.Type().Underlying().(*types.Basic); !ok || b.Info()&types.IsString == 0 {
				return ""
			}
			recv := sel.Recv()
			if p, ok := recv.(*types.Pointer); ok {
				recv = p.Elem()
			}
			if n, ok := recv.(*types.Named); ok {
				return n.Obj().Name() + "." + e.Sel.Name
			}
			return e.Sel.Name
		}
		return ""
	}

	checked := 0
	used := map[string]bool{}
	for _, f := range parsed {
		file := filepath.Base(fset.Position(f.Pos()).Filename)
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			fn, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if pkg, ok := fn.X.(*ast.Ident); !ok || pkg.Name != "fmt" {
				return true
			}
			fmtArg := map[string]int{"Sprintf": 0, "Fprintf": 1}
			i, ok := fmtArg[fn.Sel.Name]
			if !ok || len(call.Args) <= i {
				return true
			}
			lit, ok := call.Args[i].(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			format, err := strconv.Unquote(lit.Value)
			if err != nil || !sqlish.MatchString(format) {
				return true
			}
			checked++
			for v, verb := range formatVerbs(format) {
				a := i + 1 + v
				if verb != 's' || a >= len(call.Args) {
					continue
				}
				field := rawField(call.Args[a])
				if field == "" {
					continue
				}
				if _, ok := checkedFields[file+":"+field]; ok {
					used[file+":"+field] = true
					continue
				}
				t.Errorf("%s: %s is interpolated into SQL by a bare %%s; map it through a closed set "+
					"(scriptKeyword, or its enum's check) or quote it",
					fset.Position(call.Args[a].Pos()), field)
			}
			return true
		})
	}
	for k := range checkedFields {
		if !used[k] {
			t.Errorf("checkedFields lists %s, which no SQL format interpolates any more; drop the entry", k)
		}
	}
	if checked < 100 {
		t.Fatalf("only %d SQL format strings found; the sqlish pattern is wrong and this test proves little", checked)
	}
}

// formatVerbs returns each argument-consuming verb of a fmt format, in order.
// An explicit index ([n]) or a * width stops it: none is used in SQL here,
// and guessing would misattribute the rest.
func formatVerbs(format string) []rune {
	var verbs []rune
	for i := 0; i < len(format); i++ {
		if format[i] != '%' {
			continue
		}
		i++
		for i < len(format) && strings.ContainsRune("+-# 0123456789.", rune(format[i])) {
			i++
		}
		if i >= len(format) {
			break
		}
		switch c := rune(format[i]); c {
		case '%':
		case '[', '*':
			return verbs
		default:
			verbs = append(verbs, c)
		}
	}
	return verbs
}
