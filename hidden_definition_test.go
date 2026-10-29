package gosmo

import (
	"errors"
	"strings"
	"testing"
)

// TestHiddenDefinitionRefusal pins which empty expressions refuse a table's
// CREATE (ErrDefinitionHidden, ErrUnsupported too), that DROP is never
// refused, and that visible definitions pass.
func TestHiddenDefinitionRefusal(t *testing.T) {
	visible := []*Column{
		{Name: "id"},
		{Name: "c", IsComputed: true, ComputedText: "([id]*(2))"},
		{Name: "v", DefaultValue: &ColumnDefault{Name: "DF_v", Definition: "((1))"}},
	}
	checks := []*CheckConstraint{{Name: "CK_ok", Definition: "([id]>(0))"}}
	if err := hiddenDefinitionRefusal("table [dbo].[T]", ScriptCreate, visible, []*Index{{Name: "IX_f", FilterDefinition: "([id]>(0))"}}, checks); err != nil {
		t.Fatalf("visible definitions refused: %v", err)
	}
	for _, tc := range []struct {
		name    string
		cols    []*Column
		indexes []*Index
		checks  []*CheckConstraint
		want    string
	}{
		{"computed", []*Column{{Name: "c", IsComputed: true}}, nil, nil, "computed column [c]"},
		{"default", []*Column{{Name: "v", DefaultValue: &ColumnDefault{Name: "DF_v"}}}, nil, nil, "default [DF_v]"},
		{"filter", visible, []*Index{{Name: "IX_h", IsFilterHidden: true}}, nil, "index [IX_h]'s filter"},
		{"check", visible, nil, []*CheckConstraint{{Name: "CK_h"}}, "check constraint [CK_h]"},
	} {
		for _, v := range []ScriptVerb{ScriptCreate, ScriptDropAndCreate} {
			err := hiddenDefinitionRefusal("table [dbo].[T]", v, tc.cols, tc.indexes, tc.checks)
			if !errors.Is(err, ErrDefinitionHidden) || !errors.Is(err, ErrUnsupported) || !strings.Contains(err.Error(), tc.want) ||
				!strings.HasPrefix(err.Error(), "gosmo: script table [dbo].[T]: ") {
				t.Errorf("%s, verb %v: %v", tc.name, v, err)
			}
		}
		if err := hiddenDefinitionRefusal("table [dbo].[T]", ScriptDrop, tc.cols, tc.indexes, tc.checks); err != nil {
			t.Errorf("%s: DROP refused: %v", tc.name, err)
		}
	}
}
