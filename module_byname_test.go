package gosmo

import (
	"context"
	"database/sql/driver"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

// UserDefinedFunctionByName and TriggerByName are their listings narrowed to
// one schema and name: the row the server returns is scanned exactly as the
// listing scans it, and no row is ErrNotFound.
func TestModuleByNameFinders(t *testing.T) {
	ctx := context.Background()
	created := time.Date(2026, 10, 23, 9, 0, 0, 0, time.UTC)

	t.Run("UserDefinedFunctionByName", func(t *testing.T) {
		d := captureTable(t).db
		captured.reset(cannedRow{
			match: "o.type IN ('FN','TF','IF','FS','FT')",
			cols:  []string{"object_id", "schema", "name", "create_date", "modify_date", "type"},
			row:   []driver.Value{int64(42), "Sales", "fn_total", created, created, "TF"},
		})
		f, err := d.UserDefinedFunctionByName(ctx, "Sales", "fn_total")
		if err != nil {
			t.Fatalf("UserDefinedFunctionByName: %v", err)
		}
		if f.ObjectID != 42 || f.Schema != "Sales" || f.Name != "fn_total" ||
			f.FuncType != FunctionTypeTable || !f.CreateDate.Equal(created) || f.Database() != d {
			t.Errorf("function = %+v", f)
		}
		q, args := captured.findArgs("FROM   sys.objects o")
		if !strings.Contains(q, "SCHEMA_NAME(o.schema_id) COLLATE DATABASE_DEFAULT = @p1 AND o.name = @p2") {
			t.Errorf("query = %q, want it narrowed by schema and name", q)
		}
		if !slices.Equal(args, []any{"Sales", "fn_total"}) {
			t.Errorf("args = %v, want [Sales fn_total]", args)
		}

		captured.reset()
		if _, err := d.UserDefinedFunctionByName(ctx, "Sales", "absent"); !errors.Is(err, ErrNotFound) {
			t.Errorf("no rows: err = %v, want ErrNotFound", err)
		}
	})

	t.Run("TriggerByName", func(t *testing.T) {
		d := captureTable(t).db
		captured.reset(cannedRow{
			match: "FROM   sys.triggers tr",
			cols:  []string{"name", "table", "schema", "is_disabled", "events"},
			row:   []driver.Value{"trg_audit", "Orders", "Sales", true, `[{"v":"INSERT"},{"v":"UPDATE"}]`},
		})
		tr, err := d.TriggerByName(ctx, "Sales", "trg_audit")
		if err != nil {
			t.Fatalf("TriggerByName: %v", err)
		}
		if tr.Name != "trg_audit" || tr.TableName != "Orders" || tr.Schema != "Sales" || tr.IsEnabled ||
			!slices.Equal(tr.Events, []string{"INSERT", "UPDATE"}) || tr.Database() != d {
			t.Errorf("trigger = %+v", tr)
		}
		q, args := captured.findArgs("FROM   sys.triggers tr")
		if !strings.Contains(q, "SCHEMA_NAME(o.schema_id) COLLATE DATABASE_DEFAULT = @p1 AND tr.name = @p2") {
			t.Errorf("query = %q, want it narrowed by schema and name", q)
		}
		if !slices.Equal(args, []any{"Sales", "trg_audit"}) {
			t.Errorf("args = %v, want [Sales trg_audit]", args)
		}

		captured.reset()
		if _, err := d.TriggerByName(ctx, "Sales", "absent"); !errors.Is(err, ErrNotFound) {
			t.Errorf("no rows: err = %v, want ErrNotFound", err)
		}
	})
}
