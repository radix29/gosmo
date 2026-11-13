//go:build livedb

// Live verification of the three extended-property writes with their
// arguments bound (R9, review 2026-11-13): add, update and drop at table and
// column level, an apostrophe in every name and value, the empty value (sent
// as the literal N” — a bound "" is nvarchar(max), which @value refuses),
// and each step's script run by hand.
//
//	go test -tags livedb . -run TestLiveExtendedPropertyWrites -v \
//	  -livedb 'sqlserver://sa:PASS@host?TrustServerCertificate=true'
//
// Creates and drops its own throwaway database; touches nothing else.
package gosmo

import (
	"testing"
)

func TestLiveExtendedPropertyWrites(t *testing.T) {
	db, ctx, done := liveDB(t)
	t.Cleanup(done)
	d, drop := liveScratchDB(t, db, ctx, "gosmo_live_extprop")
	t.Cleanup(drop)
	liveExecIn(t, d, ctx, "CREATE TABLE dbo.[o'brien] ([it's] int)")

	table := ExtendedPropertyLevel{Level0Type: "SCHEMA", Level0Name: "dbo", Level1Type: "TABLE", Level1Name: "o'brien"}
	column := table
	column.Level2Type, column.Level2Name = "COLUMN", "it's"

	read := func(level ExtendedPropertyLevel) map[string]string {
		t.Helper()
		props, err := d.ExtendedProperties(ctx, level)
		if err != nil {
			t.Fatalf("ExtendedProperties: %v", err)
		}
		got := map[string]string{}
		for _, p := range props {
			got[p.Name] = p.Value
		}
		return got
	}
	must := func(what string, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	}

	must("add at table", d.AddExtendedProperty(ctx, "n'1", "v'1", table))
	must("add at column", d.AddExtendedProperty(ctx, "n'1", "c'1", column))
	must("add empty", d.AddExtendedProperty(ctx, "empty", "", table))
	if got := read(table); got["n'1"] != "v'1" || len(got) != 2 {
		t.Errorf("table level after add = %q", got)
	} else if v, ok := got["empty"]; !ok || v != "" {
		t.Errorf("empty value = %q, %v", v, ok)
	}
	if got := read(column); got["n'1"] != "c'1" || len(got) != 1 {
		t.Errorf("column level after add = %q", got)
	}

	must("set", d.SetExtendedProperty(ctx, "n'1", "v'2", table))
	must("set to empty", d.SetExtendedProperty(ctx, "n'1", "", column))
	if got := read(table); got["n'1"] != "v'2" {
		t.Errorf("table level after set = %q", got)
	}
	if got := read(column); got["n'1"] != "" || len(got) != 1 {
		t.Errorf("column level after set = %q", got)
	}

	must("drop", d.DropExtendedProperty(ctx, "n'1", column))
	if got := read(column); len(got) != 0 {
		t.Errorf("column level after drop = %q", got)
	}

	// The captured script binds every argument back as a literal; run it by
	// hand and check it did what the direct call does.
	sctx, script := WithScript(ctx)
	must("scripted add", d.AddExtendedProperty(sctx, "s'1", "x'1", column))
	must("scripted set", d.SetExtendedProperty(sctx, "empty", "y'1", table))
	must("scripted drop", d.DropExtendedProperty(sctx, "n'1", table))
	for _, stmt := range script.Statements() {
		liveRunScript(t, d, ctx, stmt)
	}
	if got := read(table); got["empty"] != "y'1" || len(got) != 1 {
		t.Errorf("table level after the script = %q", got)
	}
	if got := read(column); got["s'1"] != "x'1" || len(got) != 1 {
		t.Errorf("column level after the script = %q", got)
	}
}
