//go:build livedb

// Live verification of the scripters added for the Object Explorer's missing
// folders — alias types, table types, XML schema collections, rules, defaults
// and plan guides.
//
// The unit tests pin what the builders emit; only the server can say whether
// what they emit parses and creates the object again. So each script is
// generated from one throwaway database and *executed* against a second one,
// and the object is then read back through the same finder the first was read
// with. A CREATE that parses but produces a different object — a lost
// nullability, a dropped bound rule, an alias type widened by nchar's
// byte-doubled max_length — fails on the comparison rather than on the exec.
//
//	go test -tags livedb . -run TestLiveScriptedFamilies -v \
//	  -livedb 'sqlserver://sa:PASS@host?TrustServerCertificate=true'
//
// Creates and drops two throwaway databases; touches nothing else.
package gosmo

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// liveRunScript executes a generated script against d, one batch per GO — the
// separator is a client-side batch break, so the driver has to be handed the
// batches rather than the script.
func liveRunScript(t *testing.T, d *Database, ctx context.Context, script string) {
	t.Helper()
	for _, batch := range splitGoBatches(script) {
		if _, err := d.exec(ctx, batch); err != nil {
			t.Fatalf("the generated script does not run:\n%s\n\nfailing batch:\n%s\n\n%v", script, batch, err)
		}
	}
}

func splitGoBatches(script string) []string {
	var out []string
	var cur []string
	flush := func() {
		if b := strings.TrimSpace(strings.Join(cur, "\n")); b != "" {
			out = append(out, b)
		}
		cur = nil
	}
	for _, line := range strings.Split(script, "\n") {
		if strings.EqualFold(strings.TrimSpace(line), "GO") {
			flush()
			continue
		}
		cur = append(cur, line)
	}
	flush()
	return out
}

func TestLiveScriptedFamiliesRecreateTheirObjects(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()

	src, dropSrc := liveScratchDB(t, db, ctx, "gosmo_script_src_live")
	defer dropSrc()
	dst, dropDst := liveScratchDB(t, db, ctx, "gosmo_script_dst_live")
	defer dropDst()

	liveExecIn(t, src, ctx,
		`CREATE RULE dbo.scr_rule AS @value > 0`,
		`CREATE DEFAULT dbo.scr_default AS 0`,
		`CREATE TYPE dbo.scr_alias FROM NVARCHAR(20) NOT NULL`,
		`EXEC sp_bindrule N'dbo.scr_rule', N'dbo.scr_alias'`,
		`CREATE TYPE dbo.scr_tabletype AS TABLE (id INT NOT NULL, note NVARCHAR(50) NULL)`,
		`CREATE XML SCHEMA COLLECTION dbo.scr_xsd AS N'<xsd:schema xmlns:xsd="http://www.w3.org/2001/XMLSchema"><xsd:element name="scr" type="xsd:string"/></xsd:schema>'`,
		`CREATE TABLE dbo.scr_orders (id INT NOT NULL PRIMARY KEY, amount DECIMAL(18,2) NULL)`,
		`EXEC sp_create_plan_guide @name = N'scr_pg', @stmt = N'SELECT id FROM dbo.scr_orders WHERE amount > 0',
		     @type = N'SQL', @module_or_batch = NULL, @params = NULL, @hints = N'OPTION (MAXDOP 1)'`,
	)
	// The destination needs the table the plan guide's statement names: a
	// SQL-scoped guide is validated against the objects it references.
	liveExecIn(t, dst, ctx,
		`CREATE TABLE dbo.scr_orders (id INT NOT NULL PRIMARY KEY, amount DECIMAL(18,2) NULL)`,
	)

	sc := NewScripter(src, DefaultScriptOptions())

	// Rules and defaults first: the alias type's script binds them, and a
	// binding to an object that does not exist yet is refused.
	t.Run("rule", func(t *testing.T) {
		script, err := sc.ScriptRuleContext(ctx, "dbo", "scr_rule")
		if err != nil {
			t.Fatalf("ScriptRule: %v", err)
		}
		liveRunScript(t, dst, ctx, script)
		r, err := dst.RuleByNameContext(ctx, "dbo", "scr_rule")
		if err != nil {
			t.Fatalf("the scripted rule is not there: %v", err)
		}
		if !strings.Contains(r.Definition, "@value") {
			t.Errorf("recreated rule definition = %q", r.Definition)
		}
	})

	t.Run("default", func(t *testing.T) {
		script, err := sc.ScriptDefaultContext(ctx, "dbo", "scr_default")
		if err != nil {
			t.Fatalf("ScriptDefault: %v", err)
		}
		liveRunScript(t, dst, ctx, script)
		if _, err := dst.DefaultByNameContext(ctx, "dbo", "scr_default"); err != nil {
			t.Fatalf("the scripted default is not there: %v", err)
		}
	})

	t.Run("alias type keeps its width, nullability and bound rule", func(t *testing.T) {
		script, err := sc.ScriptUserDefinedDataTypeContext(ctx, "dbo", "scr_alias")
		if err != nil {
			t.Fatalf("ScriptUserDefinedDataType: %v", err)
		}
		liveRunScript(t, dst, ctx, script)

		before, err := src.UserDefinedDataTypeByNameContext(ctx, "dbo", "scr_alias")
		if err != nil {
			t.Fatalf("read the original: %v", err)
		}
		after, err := dst.UserDefinedDataTypeByNameContext(ctx, "dbo", "scr_alias")
		if err != nil {
			t.Fatalf("the scripted type is not there: %v", err)
		}
		if after.BaseType != before.BaseType || after.MaxLength != before.MaxLength {
			t.Errorf("recreated as %s(%d), want %s(%d) — max_length is bytes, not characters",
				after.BaseType, after.MaxLength, before.BaseType, before.MaxLength)
		}
		if after.IsNullable != before.IsNullable {
			t.Errorf("nullability changed: %v, want %v", after.IsNullable, before.IsNullable)
		}
		if after.Rule != before.Rule {
			t.Errorf("bound rule = %q, want %q — the type accepts values the original refuses",
				after.Rule, before.Rule)
		}
	})

	t.Run("table type keeps its columns", func(t *testing.T) {
		script, err := sc.ScriptUserDefinedTableTypeContext(ctx, "dbo", "scr_tabletype")
		if err != nil {
			t.Fatalf("ScriptUserDefinedTableType: %v", err)
		}
		liveRunScript(t, dst, ctx, script)

		after, err := dst.UserDefinedTableTypeByNameContext(ctx, "dbo", "scr_tabletype")
		if err != nil {
			t.Fatalf("the scripted table type is not there: %v", err)
		}
		cols, err := after.ColumnsContext(ctx)
		if err != nil {
			t.Fatalf("columns of the recreated type: %v", err)
		}
		if len(cols) != 2 || cols[0].Name != "id" || cols[1].Name != "note" || cols[0].IsNullable {
			t.Errorf("recreated columns = %v", columnSummaries(cols))
		}
	})

	t.Run("XML schema collection keeps its documents", func(t *testing.T) {
		script, err := sc.ScriptXmlSchemaCollectionContext(ctx, "dbo", "scr_xsd")
		if err != nil {
			t.Fatalf("ScriptXmlSchemaCollection: %v", err)
		}
		liveRunScript(t, dst, ctx, script)

		after, err := dst.XmlSchemaCollectionByNameContext(ctx, "dbo", "scr_xsd")
		if err != nil {
			t.Fatalf("the scripted collection is not there: %v", err)
		}
		def, err := after.DefinitionContext(ctx)
		if err != nil {
			t.Fatalf("definition of the recreated collection: %v", err)
		}
		if !strings.Contains(def, `name="scr"`) {
			t.Errorf("recreated definition = %q", def)
		}
	})

	t.Run("plan guide", func(t *testing.T) {
		script, err := sc.ScriptPlanGuideContext(ctx, "scr_pg")
		if err != nil {
			t.Fatalf("ScriptPlanGuide: %v", err)
		}
		liveRunScript(t, dst, ctx, script)

		after, err := dst.PlanGuideByNameContext(ctx, "scr_pg")
		if err != nil {
			t.Fatalf("the scripted plan guide is not there: %v", err)
		}
		// The match is textual, so a query text that came back re-escaped or
		// re-wrapped is a guide that matches nothing.
		before, err := src.PlanGuideByNameContext(ctx, "scr_pg")
		if err != nil {
			t.Fatalf("read the original: %v", err)
		}
		if after.QueryText != before.QueryText {
			t.Errorf("query text = %q, want %q", after.QueryText, before.QueryText)
		}
		if after.Hints != before.Hints {
			t.Errorf("hints = %q, want %q", after.Hints, before.Hints)
		}
		if after.IsDisabled {
			t.Error("an enabled guide came back disabled")
		}
	})

	// The drops are the other half of what the Script menu offers, and the
	// only check that each guarded drop's catalog lookup names the right
	// view. Run last, against the objects the CREATEs just made.
	t.Run("drops", func(t *testing.T) {
		dropOpts := DefaultScriptOptions()
		dropOpts.Verb = ScriptDrop
		dropper := NewScripter(src, dropOpts)

		for _, tc := range []struct {
			name   string
			script func() (string, error)
			gone   func() error
		}{
			{"plan guide",
				func() (string, error) { return dropper.ScriptPlanGuideContext(ctx, "scr_pg") },
				func() error { _, err := dst.PlanGuideByNameContext(ctx, "scr_pg"); return err }},
			{"XML schema collection",
				func() (string, error) { return dropper.ScriptXmlSchemaCollectionContext(ctx, "dbo", "scr_xsd") },
				func() error { _, err := dst.XmlSchemaCollectionByNameContext(ctx, "dbo", "scr_xsd"); return err }},
			{"table type",
				func() (string, error) {
					return dropper.ScriptUserDefinedTableTypeContext(ctx, "dbo", "scr_tabletype")
				},
				func() error { _, err := dst.UserDefinedTableTypeByNameContext(ctx, "dbo", "scr_tabletype"); return err }},
		} {
			t.Run(tc.name, func(t *testing.T) {
				script, err := tc.script()
				if err != nil {
					t.Fatalf("script the drop: %v", err)
				}
				liveRunScript(t, dst, ctx, script)
				if err := tc.gone(); err == nil {
					t.Error("the object is still there after the scripted drop")
				} else if !errors.Is(err, ErrNotFound) {
					t.Errorf("after the drop: %v", err)
				}
			})
		}
	})
}

func columnSummaries(cols []*Column) []string {
	out := make([]string, len(cols))
	for i, c := range cols {
		out[i] = c.Name + " " + ColumnTypeString(c)
	}
	return out
}

// TestLiveScriptedClrFamiliesReadRealCatalogRows closes the half of the
// scripter verification that TestLiveScriptedFamiliesRecreateTheirObjects
// cannot reach.
//
// The CLR type and assembly scripters were shipped unit-tested only: the test
// instances have CLR off and no user assembly, so there was nothing to script.
// There is, though — every database carries Microsoft.SqlServer.Types and the
// three system CLR types it implements (geometry, geography, hierarchyid), and
// those are real catalog rows read through the real finders. What cannot be
// done with them is the round trip the sibling test does: dropping and
// recreating a system assembly is not something a test may do, and the
// binary is elided from the script by design anyway.
//
// So this asserts the narrower thing that was actually missing — that the
// by-name reads run against a live catalog and that the emitted statement
// carries what that catalog says, rather than what a fabricated row said.
// The assembly is the valuable case: it is registered UNSAFE, not the SAFE
// default, which is precisely the value buildAssemblyScript must not lose.
func TestLiveScriptedClrFamiliesReadRealCatalogRows(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()

	srv, err := NewServer(ctx, db)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	d, err := srv.DatabaseByNameContext(ctx, "master")
	if err != nil {
		t.Fatalf("DatabaseByNameContext master: %v", err)
	}
	sc := NewScripter(d, DefaultScriptOptions())

	t.Run("assembly keeps the permission set the catalog reports", func(t *testing.T) {
		const name = "Microsoft.SqlServer.Types"
		a, err := d.AssemblyByNameContext(ctx, name)
		if err != nil {
			t.Fatalf("AssemblyByName %s: %v — every database has this assembly", name, err)
		}
		if a.PermissionSet == "" {
			t.Fatal("the assembly read back with an empty permission set")
		}
		script, err := sc.ScriptAssemblyContext(ctx, name)
		if err != nil {
			t.Fatalf("ScriptAssembly: %v", err)
		}
		want := "WITH PERMISSION_SET = " + string(a.PermissionSet)
		if !strings.Contains(script, want) {
			t.Errorf("script does not carry the catalog's permission set %q:\n%s", a.PermissionSet, script)
		}
		if !strings.Contains(script, "CREATE ASSEMBLY ["+name+"]") {
			t.Errorf("script does not name the assembly:\n%s", script)
		}
		// The binary is deliberately not scripted; the placeholder is what
		// says so, and a script that silently emitted 0x alone would look
		// runnable and produce an empty assembly.
		if !strings.Contains(script, assemblyBinaryPlaceholder) {
			t.Errorf("script omits the binary placeholder:\n%s", script)
		}
	})

	// The three system CLR types, so a scripter that ignored the object it
	// was given cannot pass on a single-type check.
	for _, tc := range []struct{ name, class string }{
		{"geography", "Microsoft.SqlServer.Types.SqlGeography"},
		{"geometry", "Microsoft.SqlServer.Types.SqlGeometry"},
		{"hierarchyid", "Microsoft.SqlServer.Types.SqlHierarchyId"},
	} {
		t.Run("CLR type "+tc.name, func(t *testing.T) {
			ct, err := d.ClrTypeByNameContext(ctx, "sys", tc.name)
			if err != nil {
				t.Fatalf("ClrTypeByName sys.%s: %v — this type is in every database", tc.name, err)
			}
			if ct.AssemblyClass != tc.class {
				t.Errorf("assembly class = %q, want %q — the catalog read picked up the wrong column", ct.AssemblyClass, tc.class)
			}
			script, err := sc.ScriptClrTypeContext(ctx, "sys", tc.name)
			if err != nil {
				t.Fatalf("ScriptClrType: %v", err)
			}
			want := "EXTERNAL NAME [" + ct.Assembly + "].[" + tc.class + "]"
			if !strings.Contains(script, want) {
				t.Errorf("script does not carry %s:\n%s", want, script)
			}
			if !strings.Contains(script, "CREATE TYPE [sys].["+tc.name+"]") {
				t.Errorf("script does not name the type:\n%s", script)
			}
		})
	}
}
