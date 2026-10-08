package gosmo

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	mssql "github.com/microsoft/go-mssqldb"
)

// H6 (review plan 2026-10-04): a database-scoped read goes as one `USE d; …`
// batch, and its parameters take the collation of the database the batch
// started in (useBatch). A built-in's result is collation-coercible too, so
// SCHEMA_NAME(x) = @p1 fails Msg 468 wherever d is collated unlike the
// session's starting database, and recheckUse re-runs it alone — batch, USE
// and query, three round trips for one lookup. COLLATE DATABASE_DEFAULT on the
// built-in's side (or comparing a catalog column, whose implicit collation
// beats the parameter) makes it one.
//
// This scans every string literal in the package — a + chain of literals
// folded into one — for a name built-in compared with a parameter, either way
// round, with no COLLATE on the built-in. The ObjectFilter column maps are
// checked separately: their expressions are compared with LOWER(@pN) by
// clause, so a map's built-in needs the COLLATE inside it.
func TestNoNameBuiltinComparesWithAParameterUncollated(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var bad []string
	checked, maps := 0, 0
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		checked++
		ast.Inspect(f, func(n ast.Node) bool {
			if cl, ok := n.(*ast.CompositeLit); ok {
				if id, ok := cl.Type.(*ast.Ident); ok && id.Name == "filterColumns" {
					maps++
					for _, e := range cl.Elts {
						kv, ok := e.(*ast.KeyValueExpr)
						if !ok {
							continue
						}
						s, ok := foldLiteral(kv.Value)
						if ok && nameBuiltinCall.MatchString(s) && !strings.Contains(strings.ToUpper(s), "COLLATE") {
							bad = append(bad, fset.Position(kv.Pos()).String()+": "+s)
						}
					}
					return false
				}
			}
			e, ok := n.(ast.Expr)
			if !ok {
				return true
			}
			s, ok := foldLiteral(e)
			if !ok {
				return true
			}
			for _, c := range uncollatedNameComparisons(s) {
				bad = append(bad, fset.Position(e.Pos()).String()+": "+c)
			}
			return false // a folded chain is checked whole, not again in parts
		})
	}
	if checked == 0 || maps == 0 {
		t.Fatalf("checked %d files and %d filter maps; the scan is wrong and this test proves nothing", checked, maps)
	}
	for _, b := range bad {
		t.Errorf("%s compared with a parameter without COLLATE DATABASE_DEFAULT (Msg 468 in a USE batch)", b)
	}
}

// Each by-name lookup, and each listing narrowed by schema, is one statement
// against a database collated unlike the session's. The driver models the
// server: a batch comparing a name built-in with a parameter uncollated fails
// Msg 468, which recheckUse answers with a bare USE and a bare re-run — three
// statements where one will do.
func TestByNameLookupsAreOneStatementAcrossCollations(t *testing.T) {
	ctx := context.Background()
	schema := []TextCriterion{{Op: TextEquals, Value: "Sales"}}
	sc := func(d *Database) *Scripter { return NewScripter(d, ScriptOptions{}) }
	lookups := map[string]func(d *Database) error{
		"TableByName":    func(d *Database) error { _, err := d.TableByName(ctx, "Sales", "t"); return err },
		"TablesBySchema": func(d *Database) error { _, err := d.TablesBySchema(ctx, "Sales"); return err },
		"TablesFiltered": func(d *Database) error { _, err := d.TablesFiltered(ctx, ObjectFilter{Schema: schema}); return err },
		"ViewsFiltered":  func(d *Database) error { _, err := d.ViewsFiltered(ctx, ObjectFilter{Schema: schema}); return err },
		"SystemViewsFiltered": func(d *Database) error {
			_, err := d.SystemViewsFiltered(ctx, ObjectFilter{Schema: schema})
			return err
		},
		"StoredProceduresFiltered": func(d *Database) error {
			_, err := d.StoredProceduresFiltered(ctx, ObjectFilter{Schema: schema})
			return err
		},
		"StoredProcedureByName":          func(d *Database) error { _, err := d.StoredProcedureByName(ctx, "Sales", "p"); return err },
		"BrokerQueueByName":              func(d *Database) error { _, err := d.BrokerQueueByName(ctx, "Sales", "q"); return err },
		"RuleByName":                     func(d *Database) error { _, err := d.RuleByName(ctx, "Sales", "r"); return err },
		"DefaultByName":                  func(d *Database) error { _, err := d.DefaultByName(ctx, "Sales", "df"); return err },
		"SequenceByName":                 func(d *Database) error { _, err := d.SequenceByName(ctx, "Sales", "s"); return err },
		"SynonymByName":                  func(d *Database) error { _, err := d.SynonymByName(ctx, "Sales", "sy"); return err },
		"UserDefinedDataTypeByName":      func(d *Database) error { _, err := d.UserDefinedDataTypeByName(ctx, "Sales", "u"); return err },
		"UserDefinedTableTypeByName":     func(d *Database) error { _, err := d.UserDefinedTableTypeByName(ctx, "Sales", "tt"); return err },
		"ClrTypeByName":                  func(d *Database) error { _, err := d.ClrTypeByName(ctx, "Sales", "c"); return err },
		"XMLSchemaCollectionByName":      func(d *Database) error { _, err := d.XMLSchemaCollectionByName(ctx, "Sales", "x"); return err },
		"SecurityPolicyByName":           func(d *Database) error { _, err := d.SecurityPolicyByName(ctx, "Sales", "sp"); return err },
		"Table.ChangeTracking":           func(d *Database) error { _, err := d.TableRef("Sales", "t").ChangeTracking(ctx); return err },
		"Scripter.ScriptView":            func(d *Database) error { _, err := sc(d).ScriptView(ctx, "Sales", "v"); return err },
		"Scripter.ScriptStoredProcedure": func(d *Database) error { _, err := sc(d).ScriptStoredProcedure(ctx, "Sales", "p"); return err },
		"Scripter.ScriptFunction":        func(d *Database) error { _, err := sc(d).ScriptFunction(ctx, "Sales", "f"); return err },
		"Scripter.ScriptTrigger":         func(d *Database) error { _, err := sc(d).ScriptTrigger(ctx, "Sales", "tr"); return err },
	}
	for name, lookup := range lookups {
		t.Run(name, func(t *testing.T) {
			d := useTestDB(t)
			useState.batchErr = func(q string) error {
				if len(uncollatedNameComparisons(q)) > 0 {
					return mssql.Error{Number: 468, Message: "Cannot resolve the collation conflict"}
				}
				return nil
			}
			// The fake's one-column row fails most scans; only the query's
			// own error is this test's business.
			if me, ok := errors.AsType[mssql.Error](lookup(d)); ok && me.Number == 468 {
				t.Fatalf("error = %v, want the batch to succeed", me)
			}
			if got := useState.stmts; len(got) != 1 {
				t.Errorf("statements = %q, want the one batch", got)
			}
		})
	}
}

// nameBuiltins are the built-ins returning a name: sysname, so
// collation-coercible like a parameter.
const nameBuiltins = `(?:OBJECT_SCHEMA_NAME|SCHEMA_NAME|OBJECT_NAME|TYPE_NAME|USER_NAME|DB_NAME|SUSER_S?NAME|COL_NAME|FILE_NAME|FILEGROUP_NAME|INDEX_COL)`

var (
	nameBuiltinCall = regexp.MustCompile(`(?i)\b` + nameBuiltins + `\s*\(`)
	paramAfter      = regexp.MustCompile(`(?is)^\s*` + paramCmp + `\s*(?:LOWER\s*\(\s*|\(\s*)?@p\d`)
	paramBefore     = regexp.MustCompile(`(?is)@p\d+\s*` + paramCmp + `\s*$`)
)

const paramCmp = `(?:=|<>|!=|\bNOT\s+LIKE\b|\bLIKE\b|\bIN\b)`

// uncollatedNameComparisons lists each name built-in in q compared with a
// parameter, either way round, with no COLLATE on the built-in — the
// comparisons that fail Msg 468 in a USE batch.
func uncollatedNameComparisons(q string) []string {
	var found []string
	for _, loc := range nameBuiltinCall.FindAllStringIndex(q, -1) {
		end := closingParen(q, loc[1]-1)
		if end < 0 {
			continue
		}
		rest := q[end+1:]
		if strings.HasPrefix(strings.ToUpper(strings.TrimSpace(rest)), "COLLATE") {
			continue
		}
		if paramAfter.MatchString(rest) || paramBefore.MatchString(q[:loc[0]]) {
			found = append(found, strings.TrimSpace(q[loc[0]:end+1]))
		}
	}
	return found
}

// foldLiteral is e's value when e is a string literal or a + chain of them.
func foldLiteral(e ast.Expr) (string, bool) {
	switch x := e.(type) {
	case *ast.BasicLit:
		if x.Kind != token.STRING {
			return "", false
		}
		s, err := strconv.Unquote(x.Value)
		return s, err == nil
	case *ast.ParenExpr:
		return foldLiteral(x.X)
	case *ast.BinaryExpr:
		if x.Op != token.ADD {
			return "", false
		}
		l, ok := foldLiteral(x.X)
		if !ok {
			return "", false
		}
		r, ok := foldLiteral(x.Y)
		return l + r, ok
	}
	return "", false
}

// closingParen is the index of the parenthesis closing the one at open, or -1.
func closingParen(s string, open int) int {
	depth := 0
	for i := open; i < len(s); i++ {
		switch s[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}
