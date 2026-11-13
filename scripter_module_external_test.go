package gosmo

import (
	"context"
	"database/sql/driver"
	"errors"
	"strings"
	"testing"
)

func TestScriptExternalFunctionUsesSQLModulesDefinition(t *testing.T) {
	const setOptions = "SET ANSI_NULLS ON;\nGO\nSET QUOTED_IDENTIFIER ON;\nGO\n"
	const drop = "DROP FUNCTION IF EXISTS [analytics].[Predict];\nGO\n"
	for _, def := range []struct {
		name, stored, create, alter string
	}{
		{
			name:   "plain",
			stored: "CREATE FUNCTION [analytics].[Predict] AS EXTERNAL FUNCTION [Functions].[PredictPython];",
			create: "CREATE FUNCTION [analytics].[Predict] AS EXTERNAL FUNCTION [Functions].[PredictPython];",
			alter:  "ALTER FUNCTION [analytics].[Predict] AS EXTERNAL FUNCTION [Functions].[PredictPython];",
		},
		{
			name:   "renamed and transferred",
			stored: "/* proxy */ CREATE FUNCTION dbo.OldName RETURNS VARCHAR(100)\nAS EXTERNAL FUNCTION [Set.With.Dot].[OldName];",
			create: "/* proxy */ CREATE FUNCTION [analytics].[Predict] RETURNS VARCHAR(100)\nAS EXTERNAL FUNCTION [Set.With.Dot].[OldName];",
			alter:  "/* proxy */ ALTER FUNCTION [analytics].[Predict] RETURNS VARCHAR(100)\nAS EXTERNAL FUNCTION [Set.With.Dot].[OldName];",
		},
		{
			name:   "stored alter",
			stored: "ALTER FUNCTION [analytics].[Predict] AS EXTERNAL FUNCTION [Functions].[PredictPython];",
			create: "CREATE FUNCTION [analytics].[Predict] AS EXTERNAL FUNCTION [Functions].[PredictPython];",
			alter:  "ALTER FUNCTION [analytics].[Predict] AS EXTERNAL FUNCTION [Functions].[PredictPython];",
		},
		{
			name:   "commented create",
			stored: "CREATE/* proxy */FUNCTION dbo.OldName AS EXTERNAL FUNCTION [Functions].[PredictPython];",
			create: "CREATE/* proxy */FUNCTION [analytics].[Predict] AS EXTERNAL FUNCTION [Functions].[PredictPython];",
			alter:  "ALTER/* proxy */FUNCTION [analytics].[Predict] AS EXTERNAL FUNCTION [Functions].[PredictPython];",
		},
		{
			name:   "commented alter",
			stored: "ALTER/* proxy */FUNCTION dbo.OldName AS EXTERNAL FUNCTION [Functions].[PredictPython];",
			create: "CREATE/* proxy */FUNCTION [analytics].[Predict] AS EXTERNAL FUNCTION [Functions].[PredictPython];",
			alter:  "ALTER/* proxy */FUNCTION [analytics].[Predict] AS EXTERNAL FUNCTION [Functions].[PredictPython];",
		},
		{
			name:   "commented create or alter",
			stored: "CREATE /* proxy */ OR/* binding */ALTER FUNCTION dbo.OldName AS EXTERNAL FUNCTION [Functions].[PredictPython];",
			create: "CREATE /* proxy */ OR/* binding */ALTER FUNCTION [analytics].[Predict] AS EXTERNAL FUNCTION [Functions].[PredictPython];",
			alter:  "CREATE /* proxy */ OR/* binding */ALTER FUNCTION [analytics].[Predict] AS EXTERNAL FUNCTION [Functions].[PredictPython];",
		},
		{
			name:   "create or alter",
			stored: "CREATE OR ALTER FUNCTION dbo.OldName AS EXTERNAL FUNCTION [Functions].[PredictPython];",
			create: "CREATE OR ALTER FUNCTION [analytics].[Predict] AS EXTERNAL FUNCTION [Functions].[PredictPython];",
			alter:  "CREATE OR ALTER FUNCTION [analytics].[Predict] AS EXTERNAL FUNCTION [Functions].[PredictPython];",
		},
	} {
		for _, verb := range []struct {
			name string
			verb ScriptVerb
			want string
		}{
			{"create", ScriptCreate, setOptions + def.create + "\nGO\n"},
			{"alter", ScriptAlter, setOptions + def.alter + "\nGO\n"},
			{"drop", ScriptDrop, drop},
			{"drop and create", ScriptDropAndCreate, drop + "\n" + setOptions + def.create + "\nGO\n"},
		} {
			t.Run(def.name+"/"+verb.name, func(t *testing.T) {
				d := captureDatabase(t)
				captured.reset(cannedRow{
					match: "o.type IN ('FN','TF','IF','FS','FT','XF')",
					cols:  []string{"definition", "ansi_nulls", "quoted_identifier", "clr", "object_id", "type"},
					rows:  [][]driver.Value{{def.stored, true, true, false, int64(42), "XF"}},
				})
				got, err := NewScripter(d, ScriptOptions{Verb: verb.verb}).ScriptFunction(context.Background(), "analytics", "Predict")
				if err != nil || got != verb.want {
					t.Fatalf("ScriptFunction = %q, %v; want %q", got, err, verb.want)
				}
				if verb.verb == ScriptDrop {
					if captured.count("FROM") != 0 {
						t.Error("DROP read the function definition")
					}
					return
				}
				q, args := captured.findArgs("FROM   sys.objects o")
				if !strings.Contains(q, "m.definition") || !strings.Contains(q, "JOIN sys.sql_modules m ON m.object_id = o.object_id") ||
					len(args) != 2 || args[0] != "analytics" || args[1] != "Predict" {
					t.Errorf("script did not read sys.sql_modules by schema/name: %s %v", q, args)
				}
				if !strings.Contains(q, "CASE WHEN o.type <> 'XF' AND m.object_id IS NULL THEN 1 ELSE 0 END") {
					t.Errorf("CLR classification must exclude XF while preserving the existing module-row check:\n%s", q)
				}
				if captured.count("vector_base_type_desc") != 0 || captured.count("sys.assembly_modules") != 0 {
					t.Error("external function scripting queried vector or CLR metadata")
				}
			})
		}
	}
}

func TestScriptExternalFunctionPreservesExtendedProperties(t *testing.T) {
	d := captureDatabase(t)
	captured.reset(
		cannedRow{
			match: "o.type IN ('FN','TF','IF','FS','FT','XF')",
			cols:  []string{"definition", "ansi_nulls", "quoted_identifier", "clr", "object_id", "type"},
			rows: [][]driver.Value{{
				"CREATE FUNCTION [analytics].[Predict] AS EXTERNAL FUNCTION [Functions].[PredictPython];",
				true, true, false, int64(42), "XF",
			}},
		},
		cannedRow{
			match: "FROM   sys.extended_properties ep",
			cols:  []string{"schema", "level1type", "level1name", "level2type", "level2name", "name", "base_type", "precision", "scale", "length", "value"},
			rows: [][]driver.Value{{
				"analytics", "FUNCTION", "Predict", "", "", "MS_Description", "nvarchar", int64(0), int64(0), int64(44), "external's description",
			}},
		},
	)
	got, err := NewScripter(d, ScriptOptions{ExtendedProperties: true}).ScriptFunction(context.Background(), "analytics", "Predict")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"@level1type = N'FUNCTION'", "@level1name = N'Predict'", "@value = N'external''s description'"} {
		if !strings.Contains(got, want) {
			t.Errorf("external function script lost %q:\n%s", want, got)
		}
	}
	if q := captured.find("FROM   sys.extended_properties ep"); !strings.Contains(q, "WHEN 'XF' THEN N'FUNCTION'") {
		t.Errorf("extended-property catalog read excludes external functions:\n%s", q)
	}
}

func TestScriptNonExternalModulesKeepStoredAlterDefinition(t *testing.T) {
	for _, tc := range []struct {
		kind moduleKind
		body string
	}{
		{moduleView, "VIEW dbo.m AS SELECT 'ALTER' AS x;"},
		{moduleProcedure, "PROCEDURE dbo.m AS PRINT 'ALTER';"},
		{moduleFunction, "FUNCTION dbo.m() RETURNS int AS BEGIN RETURN 1 END;"},
		{moduleTrigger, "TRIGGER dbo.m ON dbo.t AFTER INSERT AS PRINT 'ALTER';"},
	} {
		for _, verb := range []struct {
			name string
			verb ScriptVerb
		}{{"create", ScriptCreate}, {"drop and create", ScriptDropAndCreate}} {
			t.Run(tc.kind.keyword+"/"+verb.name, func(t *testing.T) {
				d := captureDatabase(t)
				cols := []string{"definition", "ansi_nulls", "quoted_identifier", "clr", "object_id"}
				row := []driver.Value{"ALTER " + tc.body, true, true, false, int64(42)}
				if tc.kind.keyword == "FUNCTION" {
					cols = append(cols, "type")
					row = append(row, "FN")
				}
				captured.reset(cannedRow{match: "sys.sql_modules m", cols: cols, rows: [][]driver.Value{row}})
				got, err := NewScripter(d, ScriptOptions{Verb: verb.verb}).scriptModule(t.Context(), tc.kind, "dbo", "m", nil)
				want := "SET ANSI_NULLS ON;\nGO\nSET QUOTED_IDENTIFIER ON;\nGO\nALTER " + tc.body + "\nGO\n"
				if verb.verb == ScriptDropAndCreate {
					want = "DROP " + tc.kind.keyword + " IF EXISTS [dbo].[m];\nGO\n\n" + want
				}
				if err != nil || got != want {
					t.Fatalf("script = %q, %v; want %q", got, err, want)
				}
			})
		}
	}
}

func TestScriptExternalFunctionUnreadableDefinitionNeverLeavesADrop(t *testing.T) {
	for _, verb := range []ScriptVerb{ScriptCreate, ScriptAlter, ScriptDropAndCreate} {
		d := captureDatabase(t)
		captured.reset(cannedRow{
			match: "o.type IN ('FN','TF','IF','FS','FT','XF')",
			cols:  []string{"definition", "ansi_nulls", "quoted_identifier", "clr", "object_id", "type"},
			rows:  [][]driver.Value{{nil, false, false, false, int64(42), "XF"}},
		}, cannedRow{
			match: "SELECT OBJECTPROPERTY(@p1, 'IsEncrypted')",
			cols:  []string{"encrypted"},
			rows:  [][]driver.Value{{int64(0)}},
		})
		got, err := NewScripter(d, ScriptOptions{Verb: verb}).ScriptFunction(context.Background(), "analytics", "Predict")
		if !errors.Is(err, ErrUnsupported) || got != "" {
			t.Errorf("unreadable definition: script = %q, error = %v; want empty script and ErrUnsupported", got, err)
		}
		if captured.count("sys.assembly_modules") != 0 {
			t.Error("external function with no definition was treated as CLR")
		}
	}
}

func TestScriptFunctionsKeepRegularAndCLRMetadataBehavior(t *testing.T) {
	for _, tc := range []struct {
		typ        string
		definition driver.Value
		clr        bool
	}{
		{"FN", "CREATE FUNCTION dbo.f() RETURNS int AS BEGIN RETURN 1 END", false},
		{"FS", nil, true},
		{"FT", nil, true},
	} {
		t.Run(tc.typ, func(t *testing.T) {
			d := captureDatabase(t)
			captured.reset(cannedRow{
				match: "o.type IN ('FN','TF','IF','FS','FT','XF')",
				cols:  []string{"definition", "ansi_nulls", "quoted_identifier", "clr", "object_id", "type"},
				rows:  [][]driver.Value{{tc.definition, true, true, tc.clr, int64(42), tc.typ}},
			})
			got, err := NewScripter(d, ScriptOptions{}).ScriptFunction(context.Background(), "dbo", "f")
			if tc.clr {
				if captured.count("sys.assembly_modules") != 1 || err == nil || got != "" {
					t.Errorf("CLR scripting did not request its assembly metadata: script = %q, error = %v", got, err)
				}
			} else if err != nil || !strings.Contains(got, "RETURN 1 END") || captured.count("vector_base_type_desc") != 1 {
				t.Errorf("regular scripting changed: script = %q, error = %v", got, err)
			}
		})
	}
}

func TestExternalModuleRewriteKeepsLegacyAlterHelperUnchanged(t *testing.T) {
	const definition = "CREATE/* header */FUNCTION dbo.f() RETURNS int AS BEGIN RETURN 1 END"
	if got := alterModuleDefinition(definition); got != definition {
		t.Errorf("ordinary module rewrite = %q, want the unchanged upstream definition", got)
	}
	if got := rewriteModuleVerb(definition, ScriptAlter); got != strings.Replace(definition, "CREATE", "ALTER", 1) {
		t.Errorf("external module rewrite = %q, want only the leading verb changed", got)
	}
}

func TestNonFabricDDLTriggersKeepStoredAlterDefinition(t *testing.T) {
	for _, scope := range []string{"DATABASE", "ALL SERVER"} {
		for _, verb := range []struct {
			name string
			verb ScriptVerb
		}{{"create", ScriptCreate}, {"drop and create", ScriptDropAndCreate}} {
			t.Run(scope+"/"+verb.name, func(t *testing.T) {
				body := "TRIGGER [ddl_audit] ON " + scope + " FOR CREATE_TABLE AS PRINT 'ALTER';"
				opts := ScriptOptions{Verb: verb.verb}
				var got string
				var err error
				if scope == "DATABASE" {
					got, err = buildDatabaseTriggerScript(&DatabaseTrigger{
						Name: "ddl_audit", IsEnabled: true, Definition: "ALTER " + body,
					}, opts)
				} else {
					got, err = buildServerTriggerScript(&ServerTrigger{
						Name: "ddl_audit", IsEnabled: true, Definition: "ALTER " + body,
					}, opts)
				}
				want := "SET ANSI_NULLS OFF;\nGO\nSET QUOTED_IDENTIFIER OFF;\nGO\nALTER " + body + "\nGO\n"
				if verb.verb == ScriptDropAndCreate {
					want = "DROP TRIGGER IF EXISTS [ddl_audit] ON " + scope + ";\nGO\n\n" + want
				}
				if err != nil || got != want {
					t.Fatalf("script = %q, %v; want %q", got, err, want)
				}
			})
		}
	}
}

func FuzzExternalModuleVerb(f *testing.F) {
	for _, def := range []string{
		"", "CREATE FUNCTION dbo.f AS EXTERNAL FUNCTION [Fns].[f];",
		"ALTER/* proxy */FUNCTION dbo.f AS EXTERNAL FUNCTION [Fns].[f];",
		"CREATE /* proxy */ OR/* binding */ALTER FUNCTION dbo.f AS EXTERNAL FUNCTION [Fns].[f];",
		"/* a /* nested */ b */ CREATE FUNCTION dbo.f AS EXTERNAL FUNCTION [Fns].[f];",
		"[ALTER] FUNCTION", "SELECT 'CREATE'",
	} {
		f.Add(def)
	}
	f.Fuzz(func(t *testing.T, def string) {
		for _, rewrite := range []struct{ got, from, to string }{
			{rewriteModuleVerb(def, ScriptCreate), "ALTER", "CREATE"},
			{rewriteModuleVerb(def, ScriptAlter), "CREATE", "ALTER"},
		} {
			if rewrite.got == def {
				continue
			}
			at := 0
			for at < min(len(def), len(rewrite.got)) && def[at] == rewrite.got[at] {
				at++
			}
			if !strings.EqualFold(def[at:min(at+len(rewrite.from), len(def))], rewrite.from) ||
				rewrite.got != def[:at]+rewrite.to+def[at+len(rewrite.from):] {
				t.Fatalf("external module rewrite(%q) = %q: not %s to %s", def, rewrite.got, rewrite.from, rewrite.to)
			}
		}
	})
}
