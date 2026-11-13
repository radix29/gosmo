package gosmo

import (
	"context"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// enabler is every handle whose enabled state is one on/off switch. They
// have SetEnabled only, no Enable/Disable pair (review R7, 2026-11-13).
// Index and ResourceGovernor are the opposite: Enable/Disable and no
// SetEnabled, since their Enable does more than flip a flag (a rebuild, a
// RECONFIGURE that applies every pending change).
type enabler interface {
	SetEnabled(context.Context, bool) error
}

// TestSetEnabledIsTheOneForm checks that each switch-like handle has no
// Enable/Disable pair beside SetEnabled, and that SetEnabled(true) and
// SetEnabled(false) each send a statement and not the same one. The exact
// statements are in each family's script_*_write_test.go table.
func TestSetEnabledIsTheOneForm(t *testing.T) {
	srv := &Server{}
	handles := map[string]func() enabler{
		"Job":                        func() enabler { return &Job{server: srv, Name: "j'1"} },
		"Schedule":                   func() enabler { return &Schedule{server: srv, ID: 7, Name: "s'1"} },
		"Alert":                      func() enabler { return &Alert{server: srv, Name: "a'1"} },
		"Operator":                   func() enabler { return &Operator{server: srv, Name: "o'1"} },
		"ServerAudit":                func() enabler { return srv.ServerAuditRef("au]1") },
		"ServerAuditSpecification":   func() enabler { return srv.ServerAuditSpecificationRef("sp]1") },
		"DatabaseAuditSpecification": func() enabler { return scriptTestDB().DatabaseAuditSpecificationRef("sp]1") },
		"DatabaseTrigger":            func() enabler { return scriptTestDB().DatabaseTriggerRef("tr]1") },
		"ServerTrigger":              func() enabler { return srv.ServerTriggerRef("tr]1") },
		"Trigger":                    func() enabler { return &Trigger{db: scriptTestDB(), Schema: "dbo", Name: "tr]1", TableName: "o'brien"} },
		"SecurityPolicy":             func() enabler { return &SecurityPolicy{db: scriptTestDB(), Schema: "Se]c", Name: "sp'1"} },
		"PlanGuide":                  func() enabler { return &PlanGuide{db: scriptTestDB(), Name: "PG_o'brien"} },
		"Login":                      func() enabler { return &Login{server: srv, Name: "o'brien"} },
		"FullTextIndex":              func() enabler { return scriptTestDB().TableRef("Sales.Archive", "o'brien").FullTextIndexRef() },
	}
	capture := func(t *testing.T, call func(context.Context) error) []string {
		t.Helper()
		ctx, script := WithScript(context.Background())
		if err := call(ctx); err != nil {
			t.Fatal(err)
		}
		return script.Statements()
	}
	for name, h := range handles {
		t.Run(name, func(t *testing.T) {
			typ := reflect.TypeOf(h())
			for _, m := range []string{"Enable", "Disable"} {
				if _, ok := typ.MethodByName(m); ok {
					t.Errorf("%s has %s beside SetEnabled; keep the one form", typ, m)
				}
			}
			on := capture(t, func(ctx context.Context) error { return h().SetEnabled(ctx, true) })
			off := capture(t, func(ctx context.Context) error { return h().SetEnabled(ctx, false) })
			if len(on) == 0 || len(off) == 0 || slices.Equal(on, off) {
				t.Errorf("SetEnabled:\n true: %s\nfalse: %s",
					strings.Join(on, "\n---\n"), strings.Join(off, "\n---\n"))
			}
		})
	}
}
