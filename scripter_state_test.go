package gosmo

import (
	"strings"
	"testing"
)

// A foreign key is recreated as trusted, as enabled and as replicated as it
// was — the rendering scriptCheckConstraint already gives a CHECK constraint.
// Recreating an untrusted key WITH CHECK fails on the rows it was left
// untrusted for (Msg 547).
func TestScriptForeignKeyCarriesTrustAndState(t *testing.T) {
	base := func() *ForeignKey {
		return &ForeignKey{Name: "FK", Columns: []string{"p"}, ReferencedSchema: "dbo",
			ReferencedTable: "P", ReferencedColumns: []string{"id"}}
	}
	cases := []struct {
		name    string
		mut     func(*ForeignKey)
		want    []string
		notWant []string
	}{
		{"trusted", func(*ForeignKey) {},
			[]string{"ALTER TABLE [dbo].[C] WITH CHECK\n    ADD CONSTRAINT [FK]"},
			[]string{"NOCHECK", "NOT FOR REPLICATION"}},
		{"untrusted", func(fk *ForeignKey) { fk.IsNotTrusted = true },
			[]string{"ALTER TABLE [dbo].[C] WITH NOCHECK\n    ADD CONSTRAINT [FK]"},
			[]string{"NOCHECK CONSTRAINT"}},
		{"disabled", func(fk *ForeignKey) { fk.IsDisabled, fk.IsNotTrusted = true, true },
			[]string{"WITH NOCHECK\n", "ALTER TABLE [dbo].[C] NOCHECK CONSTRAINT [FK];\nGO\n"}, nil},
		{"not for replication", func(fk *ForeignKey) { fk.IsNotForReplication, fk.DeleteAction = true, "CASCADE" },
			[]string{"ON DELETE CASCADE\n    NOT FOR REPLICATION;\nGO\n"}, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fk := base()
			c.mut(fk)
			got := scriptForeignKey(fk, "[dbo].[C]", DefaultScriptOptions())
			for _, w := range c.want {
				if !strings.Contains(got, w) {
					t.Errorf("script:\n%s\nwant it to contain:\n%q", got, w)
				}
			}
			for _, w := range c.notWant {
				if strings.Contains(got, w) {
					t.Errorf("script:\n%s\nmust not contain %q", got, w)
				}
			}
		})
	}
}
