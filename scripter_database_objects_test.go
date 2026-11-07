package gosmo

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	mssql "github.com/microsoft/go-mssqldb"
)

func queueOf(gens ...func() (string, error)) []pendingObject {
	var q []pendingObject
	for i, g := range gens {
		q = append(q, pendingObject{obj: ScriptedObject{Kind: ScriptedProcedure, Schema: "dbo", Name: string(rune('a' + i))}, gen: g})
	}
	return q
}

func sqlGen(s string) func() (string, error) { return func() (string, error) { return s, nil } }

// Progress starts at 0, rises by one per object and ends at done == total,
// and an ErrUnsupported object counts like any other.
func TestScriptQueueProgress(t *testing.T) {
	q := queueOf(sqlGen("A\n"), func() (string, error) { return "", unsupportedf("gosmo: encrypted") }, sqlGen("C\n"))
	var calls [][2]int
	out, _, err := scriptQueue(context.Background(), q, nil, func(done, total int) { calls = append(calls, [2]int{done, total}) })
	if err != nil {
		t.Fatal(err)
	}
	want := [][2]int{{0, 3}, {1, 3}, {2, 3}, {3, 3}}
	if !slices.Equal(calls, want) {
		t.Fatalf("progress calls = %v, want %v", calls, want)
	}
	if len(out) != 3 || out[1].Note == "" || !strings.HasPrefix(out[1].SQL, "-- not scripted: ") || out[2].SQL != "C\n" {
		t.Fatalf("objects = %+v", out)
	}
}

func TestScriptQueueNilProgressAndEmpty(t *testing.T) {
	if out, _, err := scriptQueue(context.Background(), nil, nil, nil); err != nil || len(out) != 0 {
		t.Fatalf("empty queue = %v, %v", out, err)
	}
	var calls [][2]int
	if _, _, err := scriptQueue(context.Background(), nil, nil, func(d, n int) { calls = append(calls, [2]int{d, n}) }); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(calls, [][2]int{{0, 0}}) {
		t.Fatalf("empty queue progress = %v, want [[0 0]]", calls)
	}
	if _, _, err := scriptQueue(context.Background(), queueOf(sqlGen("A")), nil, nil); err != nil {
		t.Fatal(err)
	}
}

// A cancelled context stops before the next object, and any error other than
// ErrUnsupported ends the script.
func TestScriptQueueStops(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	ran := 0
	q := queueOf(func() (string, error) { ran++; cancel(); return "A", nil }, func() (string, error) { ran++; return "B", nil })
	if _, _, err := scriptQueue(ctx, q, nil, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if ran != 1 {
		t.Fatalf("ran %d objects after cancel, want 1", ran)
	}
	boom := errors.New("boom")
	if _, _, err := scriptQueue(context.Background(), queueOf(func() (string, error) { return "", boom }), nil, nil); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want boom", err)
	}
}

// An object needing one that was noted is noted too, without being scripted,
// and so is what needs it in turn; a late reference to a noted object leaves
// its referrer scripted with a warning, and a dropped object seeded into
// missing counts like a noted one.
func TestScriptQueueCascadesNotes(t *testing.T) {
	ran := map[string]bool{}
	gen := func(name, sql string) func() (string, error) {
		return func() (string, error) { ran[name] = true; return sql, nil }
	}
	q := []pendingObject{
		{obj: ScriptedObject{Kind: ScriptedTable, Schema: "dbo", Name: "Q"}, key: "o:[dbo].[Q]",
			gen: func() (string, error) { return "", unsupportedf("gosmo: script table [dbo].[Q]: hidden default") }},
		{obj: ScriptedObject{Kind: ScriptedTable, Schema: "dbo", Name: "C"}, key: "o:[dbo].[C]", gen: gen("C", "CREATE TABLE C\n")},
		{obj: ScriptedObject{Kind: ScriptedView, Schema: "dbo", Name: "vQ"}, key: "o:[dbo].[vQ]",
			needs: []string{"o:[dbo].[Q]"}, gen: gen("vQ", "CREATE VIEW vQ\n")},
		{obj: ScriptedObject{Kind: ScriptedView, Schema: "dbo", Name: "vvQ"}, key: "o:[dbo].[vvQ]",
			needs: []string{"o:[dbo].[C]", "o:[dbo].[vQ]"}, gen: gen("vvQ", "CREATE VIEW vvQ\n")},
		{obj: ScriptedObject{Kind: ScriptedProcedure, Schema: "dbo", Name: "p"}, key: "o:[dbo].[p]",
			late: []string{"o:[dbo].[vvQ]", "o:[dbo].[C]"}, gen: gen("p", "CREATE PROCEDURE p\n")},
		{obj: ScriptedObject{Kind: ScriptedForeignKey, Schema: "dbo", Name: "FK_C_Q", Table: "C"}, key: "o:[dbo].[FK_C_Q]",
			needs: []string{"o:[dbo].[C]", "o:[dbo].[Q]"}, gen: gen("FK_C_Q", "ALTER TABLE C ADD FK\n")},
		{obj: ScriptedObject{Kind: ScriptedTrigger, Schema: "dbo", Name: "tr", Table: "L"}, key: "o:[dbo].[tr]",
			needs: []string{"o:[dbo].[L]"}, gen: gen("tr", "CREATE TRIGGER tr\n")},
	}
	missing := map[string]bool{"o:[dbo].[L]": true}
	out, warnings, err := scriptQueue(context.Background(), q, missing, nil)
	if err != nil {
		t.Fatal(err)
	}
	noted := map[string]string{}
	for _, o := range out {
		if o.Note != "" {
			noted[o.Name] = o.Note
			if !strings.HasPrefix(o.SQL, "-- not scripted: ") {
				t.Errorf("%s: SQL = %q, want a not-scripted comment", o.Name, o.SQL)
			}
		}
	}
	for _, n := range []string{"Q", "vQ", "vvQ", "FK_C_Q", "tr"} {
		if noted[n] == "" {
			t.Errorf("%s is not noted; notes = %v", n, noted)
		}
		if ran[n] {
			t.Errorf("%s was scripted although it needs an object left out", n)
		}
	}
	for _, n := range []string{"C", "p"} {
		if noted[n] != "" || !ran[n] {
			t.Errorf("%s: noted %q, scripted %v; want scripted", n, noted[n], ran[n])
		}
	}
	if want := "needs [dbo].[Q], which is not scripted"; !strings.Contains(noted["FK_C_Q"], want) || !strings.Contains(noted["FK_C_Q"], "[dbo].[FK_C_Q] on [dbo].[C]") {
		t.Errorf("FK note = %q, want it naming the key, its table and %q", noted["FK_C_Q"], want)
	}
	if !strings.Contains(noted["vvQ"], "[dbo].[vQ]") {
		t.Errorf("vvQ note = %q, want it naming vQ (the cascade)", noted["vvQ"])
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "procedure [dbo].[p] references [dbo].[vvQ], which is not scripted") {
		t.Errorf("warnings = %q, want one for p's late reference to vvQ", warnings)
	}
	for _, k := range []string{"o:[dbo].[Q]", "o:[dbo].[vQ]", "o:[dbo].[vvQ]", "o:[dbo].[FK_C_Q]", "o:[dbo].[tr]"} {
		if !missing[k] {
			t.Errorf("missing lacks %s: %v", k, missing)
		}
	}
	if missing["o:[dbo].[C]"] || missing["o:[dbo].[p]"] {
		t.Errorf("missing holds a scripted object: %v", missing)
	}
}

// A permission on an object left out becomes one comment line; the others,
// and one on a different class with the same name, stay statements.
func TestDatabaseSecurityPartsDropsGrantsOnMissing(t *testing.T) {
	sec := &databaseSecurity{grants: []securityGrant{
		{grantee: "r", state: "G", permission: "EXECUTE", class: 1, schema: "dbo", name: "enc"},
		{grantee: "r", state: "G", permission: "SELECT", class: 1, schema: "dbo", name: "enc", column: "c"},
		{grantee: "r", state: "G", permission: "SELECT", class: 1, schema: "dbo", name: "ok"},
		{grantee: "r", state: "G", permission: "REFERENCES", class: 6, schema: "dbo", name: "enc"},
		{grantee: "r", state: "G", permission: "REFERENCES", class: 6, schema: "dbo", name: "ty"},
	}}
	_, tail := buildDatabaseSecurityParts("d", sec, map[string]bool{"o:[dbo].[enc]": true, "t:[dbo].[ty]": true})
	for _, want := range []string{
		"-- not scripted: GRANT EXECUTE ON OBJECT::[dbo].[enc] TO [r]: [dbo].[enc] is not scripted\n",
		"-- not scripted: GRANT SELECT ON OBJECT::[dbo].[enc] ([c]) TO [r]: [dbo].[enc] is not scripted\n",
		"\nGRANT SELECT ON OBJECT::[dbo].[ok] TO [r];\n",
		"\nGRANT REFERENCES ON TYPE::[dbo].[enc] TO [r];\n",
		"-- not scripted: GRANT REFERENCES ON TYPE::[dbo].[ty] TO [r]: [dbo].[ty] is not scripted\n",
	} {
		if !strings.Contains(tail, want) {
			t.Errorf("permissions lack %q:\n%s", want, tail)
		}
	}
	if _, all := buildDatabaseSecurityParts("d", sec, nil); strings.Contains(all, "not scripted") {
		t.Errorf("nil missing commented a grant out:\n%s", all)
	}
}

// TestReadOrderEdgesFallsBack: without VIEW DEFINITION on the database the
// expression view is not read at all (it would answer no rows, no error); with
// it but refused Msg 229 it is; either way the order comes from the legacy
// view with a warning. Any other error is the read's failure.
func TestReadOrderEdgesFallsBack(t *testing.T) {
	expr := []orderEdge{{from: "o.e", to: "o.f"}}
	legacy := []orderEdge{{from: "o.l", to: "o.m", bound: true}}
	denied := mssql.Error{Number: 229, Class: 14, Message: "The SELECT permission was denied on the object 'sql_expression_dependencies'"}
	broken := errors.New("broken")
	for _, c := range []struct {
		name        string
		held        bool
		exprErr     error
		wantExprRun bool
		want        []orderEdge
		wantWarn    bool
		wantErr     error
	}{
		{"held", true, nil, true, expr, false, nil},
		{"not held", false, nil, false, legacy, true, nil},
		{"held, refused", true, denied, true, legacy, true, nil},
		{"held, other error", true, broken, true, nil, false, broken},
	} {
		t.Run(c.name, func(t *testing.T) {
			exprRan := false
			got, warn, err := readOrderEdges(context.Background(),
				func(context.Context) (bool, error) { return c.held, nil },
				func(context.Context) ([]orderEdge, error) {
					exprRan = true
					if c.exprErr != nil {
						return nil, c.exprErr
					}
					return expr, nil
				},
				func(context.Context) ([]orderEdge, error) { return legacy, nil })
			if !errors.Is(err, c.wantErr) {
				t.Fatalf("err = %v, want %v", err, c.wantErr)
			}
			if exprRan != c.wantExprRun {
				t.Errorf("expression view read = %v, want %v", exprRan, c.wantExprRun)
			}
			if !slices.Equal(got, c.want) {
				t.Errorf("edges = %v, want %v", got, c.want)
			}
			if (warn == legacyDependencyWarning) != c.wantWarn || (warn != "" && warn != legacyDependencyWarning) {
				t.Errorf("warning = %q, want it %v", warn, c.wantWarn)
			}
		})
	}
	probeErr := errors.New("probe")
	if _, _, err := readOrderEdges(context.Background(),
		func(context.Context) (bool, error) { return false, probeErr },
		func(context.Context) ([]orderEdge, error) { return expr, nil },
		func(context.Context) ([]orderEdge, error) { return legacy, nil }); !errors.Is(err, probeErr) {
		t.Errorf("a failed probe: err = %v, want it returned", err)
	}
}
