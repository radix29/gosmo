package gosmo

import (
	"context"
	"database/sql/driver"
	"errors"
	"reflect"
	"testing"
)

func TestBuiltinPermissions(t *testing.T) {
	d := captureDatabase(t)
	captured.reset(cannedRow{match: "sys.fn_builtin_permissions(DEFAULT)",
		cols: []string{"class_desc", "permission_name"},
		rows: [][]driver.Value{
			{"DATABASE", "SELECT"},
			{"OBJECT", "UNMASK"},
			{"SERVER", "VIEW SERVER PERFORMANCE STATE"},
		}})
	got, err := d.Server().BuiltinPermissions(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	want := []BuiltinPermission{{"DATABASE", "SELECT"}, {"OBJECT", "UNMASK"}, {"SERVER", "VIEW SERVER PERFORMANCE STATE"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("permissions = %v, want %v", got, want)
	}
}

func TestBuiltinPermissionsCanceled(t *testing.T) {
	d := captureDatabase(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	got, err := d.Server().BuiltinPermissions(ctx)
	if !errors.Is(err, context.Canceled) || got != nil {
		t.Fatalf("canceled permission catalog read = %v, %v", got, err)
	}
}
