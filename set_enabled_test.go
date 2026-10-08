package gosmo

import (
	"context"
	"slices"
	"strings"
	"testing"
)

// enabler is every handle with an Enable/Disable pair and the SetEnabled
// that picks one by flag. Index and ResourceGovernor have the pair but no
// SetEnabled: their Enable does more than flip a flag (a rebuild, a
// RECONFIGURE that applies every pending change).
type enabler interface {
	Enable(context.Context) error
	Disable(context.Context) error
	SetEnabled(context.Context, bool) error
}

// TestSetEnabledIsEnableOrDisable pins SetEnabled(true) to Enable's statement
// and SetEnabled(false) to Disable's, for each handle that has it.
func TestSetEnabledIsEnableOrDisable(t *testing.T) {
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
			for _, on := range []bool{true, false} {
				want := capture(t, h().Disable)
				if on {
					want = capture(t, h().Enable)
				}
				got := capture(t, func(ctx context.Context) error { return h().SetEnabled(ctx, on) })
				if len(want) == 0 || !slices.Equal(got, want) {
					t.Errorf("SetEnabled(%v):\n got: %s\nwant: %s", on,
						strings.Join(got, "\n---\n"), strings.Join(want, "\n---\n"))
				}
			}
		})
	}
}
