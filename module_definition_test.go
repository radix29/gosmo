package gosmo

import (
	"context"
	"database/sql/driver"
	"errors"
	"maps"
	"strings"
	"testing"
	"time"
)

// TestModuleListingsDoNotReadTheDefinition pins T52: a listing of modules
// carries no text. The sys schema alone ships about 1,400 procedures, and
// every listing used to pull each one's definition to show a name.
func TestModuleListingsDoNotReadTheDefinition(t *testing.T) {
	ctx := context.Background()
	for name, list := range map[string]func(*Database) error{
		"StoredProcedures":       func(d *Database) error { _, err := d.StoredProcedures(ctx, ObjectFilter{}); return err },
		"SystemStoredProcedures": func(d *Database) error { _, err := d.SystemStoredProcedures(ctx, ObjectFilter{}); return err },
		"Views":                  func(d *Database) error { _, err := d.Views(ctx, ObjectFilter{}); return err },
		"SystemViews":            func(d *Database) error { _, err := d.SystemViews(ctx, ObjectFilter{}); return err },
		"UserDefinedFunctions":   func(d *Database) error { _, err := d.UserDefinedFunctions(ctx, ObjectFilter{}); return err },
		"ExternalFunctions":      func(d *Database) error { _, err := d.ExternalFunctions(ctx, ObjectFilter{}); return err },
		"SystemFunctions":        func(d *Database) error { _, err := d.SystemFunctions(ctx, ObjectFilter{}); return err },
		"Triggers":               func(d *Database) error { _, err := d.Triggers(ctx); return err },
		"Rules":                  func(d *Database) error { _, err := d.Rules(ctx); return err },
		"Defaults":               func(d *Database) error { _, err := d.Defaults(ctx); return err },
	} {
		t.Run(name, func(t *testing.T) {
			d := captureDatabase(t)
			if err := list(d); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			if captured.count("FROM") == 0 {
				t.Fatal("no listing statement was generated")
			}
			for _, needle := range []string{"definition", "DEFINITION", "sql_modules"} {
				if q := captured.find(needle); q != "" {
					t.Errorf("the listing reads %q:\n%s", needle, q)
				}
			}
		})
	}
}

// TestModuleListingsReadTheirSource pins S3's one module-list reader: each
// family reads its own source and kind, orders as its scope wants, and a
// system listing finds the sys schema by schema_id 4, which a SCHEMA_NAME()
// comparison could not seek on. SystemCatalog's three reads do the same.
func TestModuleListingsReadTheirSource(t *testing.T) {
	ctx := context.Background()
	const userOrder, systemOrder = "ORDER  BY SCHEMA_NAME(o.schema_id), o.name", "ORDER  BY o.name"
	for name, tc := range map[string]struct {
		list func(*Database) error
		want []string
	}{
		"Views": {func(d *Database) error { _, err := d.Views(ctx, ObjectFilter{}); return err },
			[]string{"FROM   sys.views o\nWHERE  o.is_ms_shipped = 0 ", "'IsSchemaBound'", userOrder}},
		"SystemViews": {func(d *Database) error { _, err := d.SystemViews(ctx, ObjectFilter{}); return err },
			[]string{"FROM   sys.all_objects o\nWHERE  o.type = 'V' AND o.is_ms_shipped = 1 AND o.schema_id = 4 ", "'IsSchemaBound'", systemOrder}},
		"StoredProcedures": {func(d *Database) error { _, err := d.StoredProcedures(ctx, ObjectFilter{}); return err },
			[]string{"FROM   sys.procedures o\nWHERE  o.is_ms_shipped = 0 ", userOrder}},
		"SystemStoredProcedures": {func(d *Database) error { _, err := d.SystemStoredProcedures(ctx, ObjectFilter{}); return err },
			[]string{"FROM   sys.all_objects o\nWHERE  o.type IN ('P','PC') AND o.is_ms_shipped = 1 AND o.schema_id = 4 ", systemOrder}},
		"UserDefinedFunctions": {func(d *Database) error { _, err := d.UserDefinedFunctions(ctx, ObjectFilter{}); return err },
			[]string{"o.modify_date, o.type\nFROM   sys.objects o\nWHERE  o.type IN ('FN','TF','IF','FS','FT') AND o.is_ms_shipped = 0 ", userOrder}},
		"SystemFunctions": {func(d *Database) error { _, err := d.SystemFunctions(ctx, ObjectFilter{}); return err },
			[]string{"o.modify_date, o.type\nFROM   sys.all_objects o\nWHERE  o.type IN ('FN','TF','IF') AND o.is_ms_shipped = 1 AND o.schema_id = 4 ", systemOrder}},
		"Views(filtered)": {func(d *Database) error {
			_, err := d.Views(ctx, ObjectFilter{Schema: []TextCriterion{{Op: TextEquals, Value: "s"}}})
			return err
		}, []string{"SCHEMA_NAME(o.schema_id) COLLATE DATABASE_DEFAULT", userOrder}},
		"SystemCatalog": {func(d *Database) error { _, err := d.SystemCatalog(ctx); return err },
			[]string{"o.type = 'V' AND o.schema_id = 4", "o.type IN ('IF','TF','FT') AND o.schema_id = 4", "o.type = 'AF' AND o.schema_id = 4"}},
	} {
		t.Run(name, func(t *testing.T) {
			d := captureDatabase(t)
			_ = tc.list(d) // SystemCatalog errors on the capture driver's single result set
			q := captured.find("FROM")
			if q == "" {
				t.Fatal("no listing statement was generated")
			}
			for _, w := range tc.want {
				if !strings.Contains(q, w) {
					t.Errorf("query lacks %q:\n%s", w, q)
				}
			}
			if strings.Contains(q, "= 'sys'") {
				t.Errorf("query finds the sys schema by name:\n%s", q)
			}
		})
	}
}

// TestModuleDefinitionReadsByName pins that every Definition(ctx) works from
// a Ref handle — one statement, by name through a parameter, restricted to
// its own kind — and that no row is ErrNotFound.
func TestModuleDefinitionReadsByName(t *testing.T) {
	ctx := context.Background()
	for name, tc := range map[string]struct {
		read func(*Database) (string, error)
		kind string
	}{
		"StoredProcedure": {func(d *Database) (string, error) { return d.StoredProcedureRef("dbo", "p").Definition(ctx) }, "o.type IN ('P','PC')"},
		"View":            {func(d *Database) (string, error) { return d.ViewRef("dbo", "p").Definition(ctx) }, "o.type = 'V'"},
		"Function":        {func(d *Database) (string, error) { return d.UserDefinedFunctionRef("dbo", "p").Definition(ctx) }, "o.type IN ('FN','TF','IF','FS','FT','XF')"},
		"Trigger":         {func(d *Database) (string, error) { return d.TriggerRef("dbo", "p").Definition(ctx) }, "o.type IN ('TR','TA')"},
		"Rule":            {func(d *Database) (string, error) { return d.RuleRef("dbo", "p").Definition(ctx) }, "o.type = 'R'"},
		"Default":         {func(d *Database) (string, error) { return d.DefaultRef("dbo", "p").Definition(ctx) }, "o.type = 'D' AND o.parent_object_id = 0"},
	} {
		t.Run(name, func(t *testing.T) {
			d := captureDatabase(t)
			if _, err := tc.read(d); !errors.Is(err, ErrNotFound) {
				t.Errorf("no row: err = %v, want ErrNotFound", err)
			}
			if n := captured.count("OBJECT_DEFINITION"); n != 1 {
				t.Fatalf("%d OBJECT_DEFINITION statements, want 1", n)
			}
			q := captured.find("OBJECT_DEFINITION")
			if !strings.Contains(q, "OBJECT_ID(@p1)") || !strings.Contains(q, tc.kind) {
				t.Errorf("statement does not read by name restricted to %s:\n%s", tc.kind, q)
			}
		})
	}

	t.Run("an empty schema is refused before any query", func(t *testing.T) {
		d := captureDatabase(t)
		if _, err := d.ViewRef("", "v").Definition(ctx); !errors.Is(err, ErrSchemaRequired) {
			t.Errorf("err = %v, want ErrSchemaRequired", err)
		}
		if captured.count("OBJECT_DEFINITION") != 0 {
			t.Error("a statement was sent for an empty schema")
		}
	})
}

func TestExternalFunctionsReadOnlyXFObjects(t *testing.T) {
	created := time.Date(2026, 10, 21, 10, 0, 0, 0, time.UTC)
	for _, filtered := range []bool{false, true} {
		d := captureDatabase(t)
		captured.reset(cannedRow{match: "FROM   sys.objects o",
			cols: []string{"object_id", "schema_name", "name", "create_date", "modify_date", "type"},
			rows: [][]driver.Value{{int64(42), "analytics", "Predict", created, created, "XF"}}})
		var funcs []*UserDefinedFunction
		var err error
		if filtered {
			funcs, err = d.ExternalFunctions(context.Background(),
				ObjectFilter{Name: []TextCriterion{{Op: TextEquals, Value: "Predict"}}})
		} else {
			funcs, err = d.ExternalFunctions(context.Background(), ObjectFilter{})
		}
		if err != nil {
			t.Fatal(err)
		}
		if len(funcs) != 1 {
			t.Fatalf("ExternalFunctions = %v, want one function", funcs)
		}
		f := funcs[0]
		if f.ObjectID != 42 || f.Schema != "analytics" || f.Name != "Predict" || f.FuncType != "XF" ||
			!f.CreateDate.Equal(created) || !f.ModifyDate.Equal(created) || f.Database() != d {
			t.Errorf("external function lost catalog fields or database: %+v", f)
		}
		if !f.FuncType.IsScalar() || f.FuncType.IsCLR() {
			t.Errorf("XF must be scalar and not CLR")
		}
		q, args := captured.findArgs("FROM   sys.objects o")
		if !strings.Contains(q, "WHERE  o.type = 'XF'") || strings.Contains(q, "o.type IN") {
			t.Errorf("external listing is not restricted to XF:\n%s", q)
		}
		if filtered && (!strings.Contains(q, "LOWER(o.name) = LOWER(@p1)") || len(args) != 1 || args[0] != "Predict") {
			t.Errorf("external listing lost its pushed-down filter: %s %v", q, args)
		}
	}
	d := captureDatabase(t)
	if funcs, err := d.ExternalFunctions(context.Background(), ObjectFilter{}); err != nil || len(funcs) != 0 {
		t.Errorf("empty external function listing = %v, %v", funcs, err)
	}
	if _, err := d.UserDefinedFunctions(context.Background(), ObjectFilter{}); err != nil {
		t.Fatal(err)
	}
	if q := captured.find("WHERE  o.type IN"); !strings.Contains(q, "o.type IN ('FN','TF','IF','FS','FT') AND o.is_ms_shipped = 0") {
		t.Errorf("regular function listing changed or includes XF:\n%s", q)
	}
}

// TestBoundObjectDefinitionsReadEveryRowAtOnce pins RuleDefinitions and
// DefaultDefinitions: one statement for the whole family, the defaults one
// restricted to standalone defaults, and a NULL text (encrypted, or not
// visible) read as "" the way Definition reports it.
func TestBoundObjectDefinitionsReadEveryRowAtOnce(t *testing.T) {
	ctx := context.Background()
	for name, tc := range map[string]struct {
		read func(*Database) (map[int]string, error)
		kind string
	}{
		"Rule":    {func(d *Database) (map[int]string, error) { return d.RuleDefinitions(ctx) }, "o.type = 'R'"},
		"Default": {func(d *Database) (map[int]string, error) { return d.DefaultDefinitions(ctx) }, "o.type = 'D' AND o.parent_object_id = 0"},
	} {
		t.Run(name, func(t *testing.T) {
			d := captureDatabase(t)
			captured.reset(cannedRow{match: "OBJECT_DEFINITION", cols: []string{"object_id", "def"},
				rows: [][]driver.Value{{int64(7), "CREATE x AS 1"}, {int64(9), nil}}})
			got, err := tc.read(d)
			if err != nil {
				t.Fatal(err)
			}
			if want := map[int]string{7: "CREATE x AS 1", 9: ""}; !maps.Equal(got, want) {
				t.Errorf("definitions = %v, want %v", got, want)
			}
			if n := captured.count("OBJECT_DEFINITION"); n != 1 {
				t.Fatalf("%d OBJECT_DEFINITION statements, want 1", n)
			}
			if q := captured.find("OBJECT_DEFINITION"); !strings.Contains(q, tc.kind) {
				t.Errorf("statement is not restricted to %s:\n%s", tc.kind, q)
			}
		})
	}
}
