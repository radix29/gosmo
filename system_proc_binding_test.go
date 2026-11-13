package gosmo

import (
	"context"
	"database/sql"
	"slices"
	"strings"
	"testing"
)

// TestSystemProcedureWritesBindTheirArguments pins R9 (review 2026-11-13):
// the extended-property, backup-device and sp_configure writes bind every
// caller-supplied value, as sp_rename and the Agent procedures do, instead of
// escaping it into the statement. The script-capture goldens
// (script_extended_properties_write_test.go, backup_device_write_test.go)
// cannot tell the two apart — bindScriptArgs renders a bound value as the
// literal the escaping built — so this goes through the capture driver and
// looks at what was sent.
func TestSystemProcedureWritesBindTheirArguments(t *testing.T) {
	db, err := sql.Open("capture", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	srv := &Server{db: db}
	d := &Database{server: srv, Name: "testdb"}
	ctx := context.Background()
	level := ExtendedPropertyLevel{
		Level0Type: "SCHEMA", Level0Name: "dbo",
		Level1Type: "TABLE", Level1Name: "o'brien",
	}
	// An unused level is NULL, so it binds nil.
	levelArgs := []any{"SCHEMA", "dbo", "TABLE", "o'brien", nil, nil}

	cases := []struct {
		name   string
		write  func() error
		needle string
		args   []any
	}{
		{"add extended property", func() error { return d.AddExtendedProperty(ctx, "n'1", "v'1", level) },
			"sp_addextendedproperty", append(append([]any{"n'1"}, levelArgs...), "v'1")},
		{"set extended property", func() error { return d.SetExtendedProperty(ctx, "n'1", "v'2", level) },
			"sp_updateextendedproperty", append(append([]any{"n'1"}, levelArgs...), "v'2")},
		{"drop extended property", func() error { return d.DropExtendedProperty(ctx, "n'1", level) },
			"sp_dropextendedproperty", append([]any{"n'1"}, levelArgs...)},
		{"create backup device", func() error {
			_, err := srv.CreateBackupDevice(ctx, CreateBackupDeviceRequest{Name: "d'1", PhysicalName: `C:\o'brien.bak`})
			return err
		}, "sp_addumpdevice", []any{"disk", "d'1", `C:\o'brien.bak`}},
		{"drop backup device", func() error { return srv.BackupDeviceRef("d'1").Drop(ctx, DeleteDeviceFile) },
			"sp_dropdevice", []any{"d'1"}},
		{"set configuration", func() error { return srv.ConfigurationRef("o'brien option").SetValue(ctx, 42) },
			"sp_configure @p1", []any{"o'brien option", int64(42)}},
		{"apply configuration", func() error {
			return srv.ApplyConfiguration(ctx, []ConfigChange{{Name: "o'brien option", Value: 42}}, ConfigApplyOptions{})
		}, "sys.sp_configure @p1", []any{"o'brien option", int64(42)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			captured.reset()
			_ = tc.write() // the capture driver serves no rows, so a read-back may fail
			q, args := captured.findArgs(tc.needle)
			if q == "" {
				t.Fatalf("no %s statement was sent", tc.needle)
			}
			if !slices.Equal(args, tc.args) {
				t.Errorf("args = %#v, want %#v", args, tc.args)
			}
			for _, v := range tc.args {
				if s, ok := v.(string); ok && strings.Contains(q, s) {
					t.Errorf("%q reached the statement text:\n%s", s, q)
				}
			}
			if strings.Contains(q, "''") {
				t.Errorf("an escaped literal reached the statement text:\n%s", q)
			}
		})
	}
}

// TestEmptyExtendedPropertyValueIsALiteral: the driver sends "" as
// nvarchar(max), which @value (sql_variant) refuses, so the empty value is the
// one argument written as the constant N”.
func TestEmptyExtendedPropertyValueIsALiteral(t *testing.T) {
	q, args := extendedPropertyWrite("sp_addextendedproperty", "n", new(""), ExtendedPropertyLevel{})
	if !strings.Contains(q, "@value = N''") || strings.Contains(q, "@p8") || len(args) != 7 {
		t.Errorf("empty value: args %#v\n%s", args, q)
	}
}
