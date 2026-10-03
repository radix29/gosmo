package gosmo

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// TestModuleListingsDoNotReadTheDefinition pins T52: a listing of modules
// carries no text. The sys schema alone ships about 1,400 procedures, and
// every listing used to pull each one's definition to show a name.
func TestModuleListingsDoNotReadTheDefinition(t *testing.T) {
	ctx := context.Background()
	for name, list := range map[string]func(*Database) error{
		"StoredProcedures":       func(d *Database) error { _, err := d.StoredProcedures(ctx); return err },
		"SystemStoredProcedures": func(d *Database) error { _, err := d.SystemStoredProcedures(ctx); return err },
		"Views":                  func(d *Database) error { _, err := d.Views(ctx); return err },
		"SystemViews":            func(d *Database) error { _, err := d.SystemViews(ctx); return err },
		"UserDefinedFunctions":   func(d *Database) error { _, err := d.UserDefinedFunctions(ctx); return err },
		"SystemFunctions":        func(d *Database) error { _, err := d.SystemFunctions(ctx); return err },
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
		"Function":        {func(d *Database) (string, error) { return d.UserDefinedFunctionRef("dbo", "p").Definition(ctx) }, "o.type IN ('FN','TF','IF','FS','FT')"},
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
