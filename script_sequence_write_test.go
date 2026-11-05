package gosmo

import (
	"context"
	"errors"
	"testing"
)

// Sequence.Restart mirrors the new value onto the handle, so it has to
// honour WithScript: a scripted restart is recorded, not run, and the server
// still hands out values from where it was. Mirroring anyway left the handle
// claiming a current value nothing on the server had ever produced.
func TestScriptedSequenceRestartDoesNotMoveCurrentValue(t *testing.T) {
	seq := &Sequence{db: scriptTestDB(), Schema: "Sales.Archive", Name: "o'brien", CurrentValue: "42", StartValue: "1", LastUsedValue: "42"}

	ctx, script := WithScript(context.Background())
	if err := seq.Restart(ctx, "1000"); err != nil {
		t.Fatalf("scripted restart: %v", err)
	}
	if seq.CurrentValue != "42" || seq.StartValue != "1" || seq.LastUsedValue != "42" {
		t.Errorf("CurrentValue/StartValue/LastUsedValue = %s/%s/%q after a scripted "+
			"restart, want 42/1/\"42\" — the statement was only captured, so the "+
			"server is still where it was",
			seq.CurrentValue, seq.StartValue, seq.LastUsedValue)
	}
	want := scriptUsePrefix + "ALTER SEQUENCE [Sales.Archive].[o'brien] RESTART WITH 1000"
	if len(script.Statements()) != 1 || script.Statements()[0] != want {
		t.Errorf("captured %q, want %q", script.Statements(), want)
	}
}

// CreateSequence's exact statement: every clause spelled, the precision on a
// decimal written as (p, 0), and a precision on any other type refused before
// the round trip.
func TestScriptedCreateSequenceStatement(t *testing.T) {
	cache := func(n int) *int { return &n }
	for _, tc := range []struct {
		name string
		req  CreateSequenceRequest
		want string
	}{
		{"defaults", CreateSequenceRequest{Schema: "Sales.Archive", Name: "o'brien", StartValue: "1", Increment: "1"},
			"CREATE SEQUENCE [Sales.Archive].[o'brien] AS bigint START WITH 1 INCREMENT BY 1 NO MINVALUE NO MAXVALUE NO CYCLE CACHE"},
		{"every clause", CreateSequenceRequest{Schema: "dbo", Name: "s]q", DataType: DataTypeInt, StartValue: "10", Increment: "-2",
			MinValue: "-5", MaxValue: "500", Cycle: true, Cache: cache(20)},
			"CREATE SEQUENCE [dbo].[s]]q] AS int START WITH 10 INCREMENT BY -2 MINVALUE -5 MAXVALUE 500 CYCLE CACHE 20"},
		{"no cache", CreateSequenceRequest{Schema: "dbo", Name: "s", DataType: DataTypeSmallInt, StartValue: "1", Increment: "1", Cache: cache(0)},
			"CREATE SEQUENCE [dbo].[s] AS smallint START WITH 1 INCREMENT BY 1 NO MINVALUE NO MAXVALUE NO CYCLE NO CACHE"},
		{"decimal precision", CreateSequenceRequest{Schema: "dbo", Name: "s", DataType: DataTypeDecimal, Precision: 38, StartValue: "1", Increment: "1"},
			"CREATE SEQUENCE [dbo].[s] AS decimal(38, 0) START WITH 1 INCREMENT BY 1 NO MINVALUE NO MAXVALUE NO CYCLE CACHE"},
		// Past int64 both ways, and a value given with a sign or leading
		// zeros is written as the number it is.
		{"past bigint", CreateSequenceRequest{Schema: "dbo", Name: "s", DataType: DataTypeDecimal, Precision: 38,
			StartValue: "+0010000000000000000000", Increment: "1",
			MinValue: "-10000000000000000000000000000000000000", MaxValue: "10000000000000000000000000000000000000"},
			"CREATE SEQUENCE [dbo].[s] AS decimal(38, 0) START WITH 10000000000000000000 INCREMENT BY 1 " +
				"MINVALUE -10000000000000000000000000000000000000 MAXVALUE 10000000000000000000000000000000000000 NO CYCLE CACHE"},
		// "" omits START WITH and INCREMENT BY: the server's defaults.
		{"server defaults", CreateSequenceRequest{Schema: "dbo", Name: "s"},
			"CREATE SEQUENCE [dbo].[s] AS bigint NO MINVALUE NO MAXVALUE NO CYCLE CACHE"},
	} {
		ctx, script := WithScript(context.Background())
		if _, err := scriptTestDB().CreateSequence(ctx, tc.req); err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		if got := script.Statements(); len(got) != 1 || got[0] != scriptUsePrefix+tc.want {
			t.Errorf("%s: captured %q, want %q", tc.name, got, scriptUsePrefix+tc.want)
		}
	}
	for _, req := range []CreateSequenceRequest{
		{Schema: "dbo", Name: "s", DataType: DataTypeInt, Precision: 10, Increment: "1"},
		{Schema: "dbo", Name: "s", DataType: DataTypeNumeric, Precision: 39, Increment: "1"},
		// A value is a whole number or nothing: the text never reaches the SQL.
		{Schema: "dbo", Name: "s", StartValue: "1; DROP TABLE t; --"},
		{Schema: "dbo", Name: "s", Increment: "1.5"},
		{Schema: "dbo", Name: "s", MinValue: " 1"},
		{Schema: "dbo", Name: "s", MaxValue: "1e9"},
		{Schema: "dbo", Name: "s", Increment: "-0"},
		{Schema: "dbo", Name: "s", MinValue: "10000000000000000000", MaxValue: "9999999999999999999"},
	} {
		ctx, script := WithScript(context.Background())
		if _, err := scriptTestDB().CreateSequence(ctx, req); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("%+v: err = %v, want ErrInvalidRequest", req, err)
		}
		if len(script.Statements()) != 0 {
			t.Errorf("%+v: captured %q after a refusal", req, script.Statements())
		}
	}
}

// Sequence.Alter's exact statement: only the set clauses, in the statement's
// order, and the handle mirrors them only when the statement ran.
func TestScriptedSequenceAlterStatement(t *testing.T) {
	yes, no := true, false
	for _, tc := range []struct {
		name string
		ch   SequenceChanges
		want string
	}{
		{"every clause", SequenceChanges{RestartWith: "100", Increment: "-3", MinValue: "-1000", MaxValue: "1000",
			Cycle: &yes, Cache: &SequenceCache{Enabled: true, Size: 50}},
			"ALTER SEQUENCE [Sales.Archive].[o'brien] RESTART WITH 100 INCREMENT BY -3 MINVALUE -1000 MAXVALUE 1000 CYCLE CACHE 50"},
		{"increment alone", SequenceChanges{Increment: "5"},
			"ALTER SEQUENCE [Sales.Archive].[o'brien] INCREMENT BY 5"},
		{"no cycle, no cache", SequenceChanges{Cycle: &no, Cache: &SequenceCache{}},
			"ALTER SEQUENCE [Sales.Archive].[o'brien] NO CYCLE NO CACHE"},
		{"default cache", SequenceChanges{Cache: &SequenceCache{Enabled: true}},
			"ALTER SEQUENCE [Sales.Archive].[o'brien] CACHE"},
		{"max alone", SequenceChanges{MaxValue: "9"},
			"ALTER SEQUENCE [Sales.Archive].[o'brien] MAXVALUE 9"},
		{"past bigint", SequenceChanges{RestartWith: "-9223372036854775809", MaxValue: "99999999999999999999999999999999999999"},
			"ALTER SEQUENCE [Sales.Archive].[o'brien] RESTART WITH -9223372036854775809 MAXVALUE 99999999999999999999999999999999999999"},
	} {
		seq := &Sequence{db: scriptTestDB(), Schema: "Sales.Archive", Name: "o'brien", Increment: "1", MaxValue: "7", IsCycling: false}
		ctx, script := WithScript(context.Background())
		if err := seq.Alter(ctx, tc.ch); err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		if got := script.Statements(); len(got) != 1 || got[0] != scriptUsePrefix+tc.want {
			t.Errorf("%s: captured %q, want %q", tc.name, got, scriptUsePrefix+tc.want)
		}
		if seq.Increment != "1" || seq.MaxValue != "7" || seq.IsCycling {
			t.Errorf("%s: a scripted alter moved the handle to %+v", tc.name, seq)
		}
	}

	seq := &Sequence{db: scriptTestDB(), Schema: "dbo", Name: "s"}
	ctx, script := WithScript(context.Background())
	if err := seq.Alter(ctx, SequenceChanges{}); err != nil || len(script.Statements()) != 0 {
		t.Errorf("an empty change: err %v, captured %q; want nothing issued", err, script.Statements())
	}
	for name, ch := range map[string]SequenceChanges{
		"zero increment":        {Increment: "0"},
		"min not below max":     {MinValue: "5", MaxValue: "5"},
		"min past bigint above": {MinValue: "10000000000000000000", MaxValue: "9223372036854775807"},
		"negative cache":        {Cache: &SequenceCache{Enabled: true, Size: -1}},
		"not a number":          {RestartWith: "1 CYCLE"},
		"a fraction":            {MaxValue: "9.5"},
	} {
		if err := seq.Alter(ctx, ch); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("%s: err = %v, want ErrInvalidRequest", name, err)
		}
	}
	if len(script.Statements()) != 0 {
		t.Errorf("refusals captured %q", script.Statements())
	}
	if err := (&Sequence{db: scriptTestDB(), Name: "s"}).Alter(ctx, SequenceChanges{Increment: "1"}); !errors.Is(err, ErrSchemaRequired) {
		t.Errorf("no schema: err = %v, want ErrSchemaRequired", err)
	}
}
