package gosmo

import (
	"context"
	"errors"
	"testing"
)

// A TableRef carries no ObjectID, and every read keyed by one used to ask for
// object 0 and answer with an empty result — no columns, no indexes, a row
// count of zero — which a caller could not tell from a real empty table.
// Each now refuses the handle with ErrHandleNotLoaded before any query: the
// Database here has no pool behind it, so a read that got past the guard
// fails on the missing connection instead, and is reported.
func TestTableRefReadsAreRefused(t *testing.T) {
	cases := []struct {
		name string
		call func(context.Context, *Table) error
	}{
		{"Detail", func(c context.Context, t *Table) error { return errOnly(t.Detail(c)) }},
		{"Columns", func(c context.Context, t *Table) error { return errOnly(t.Columns(c)) }},
		{"ForeignKeys", func(c context.Context, t *Table) error { return errOnly(t.ForeignKeys(c)) }},
		{"ForeignKeyByName", func(c context.Context, t *Table) error { return errOnly(t.ForeignKeyByName(c, "fk")) }},
		{"CheckConstraints", func(c context.Context, t *Table) error { return errOnly(t.CheckConstraints(c)) }},
		{"Triggers", func(c context.Context, t *Table) error { return errOnly(t.Triggers(c)) }},
		{"RowCount", func(c context.Context, t *Table) error { return errOnly(t.RowCount(c)) }},
		{"Indexes", func(c context.Context, t *Table) error { return errOnly(t.Indexes(c)) }},
		{"IndexByName", func(c context.Context, t *Table) error { return errOnly(t.IndexByName(c, "ix")) }},
		{"DataSpace", func(c context.Context, t *Table) error { return errOnly(t.DataSpace(c)) }},
		{"XMLIndexes", func(c context.Context, t *Table) error { return errOnly(t.XMLIndexes(c)) }},
		{"Partitions", func(c context.Context, t *Table) error { return errOnly(t.Partitions(c)) }},
		{"SpaceUsed", func(c context.Context, t *Table) error { return errOnly(t.SpaceUsed(c)) }},
		{"Statistics", func(c context.Context, t *Table) error { return errOnly(t.Statistics(c)) }},
		{"StatisticByName", func(c context.Context, t *Table) error { return errOnly(t.StatisticByName(c, "st")) }},
		{"EdgeConstraints", func(c context.Context, t *Table) error { return errOnly(t.EdgeConstraints(c)) }},
		{"Index.IncludedColumnsSupported", func(_ context.Context, t *Table) error {
			return t.IndexRef("ix").IncludedColumnsSupported()
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := &Database{server: &Server{}, Name: "AppDB"}
			err := tc.call(context.Background(), d.TableRef("dbo", "t"))
			if !errors.Is(err, ErrHandleNotLoaded) {
				t.Fatalf("err = %v, want ErrHandleNotLoaded", err)
			}
		})
	}
}

// A create on a TableRef is a valid write — it names the table in the
// statement — and only its read-back needs the ObjectID the handle lacks.
// createdObject answers that refusal with the new object's handle, as it does
// a read-back the caller cannot see, rather than report a create that
// happened as failed.
func TestCreateOnATableRefReturnsTheHandle(t *testing.T) {
	ref := captureTable(t).db.TableRef("dbo", "t")

	idx, err := ref.CreateIndex(t.Context(), CreateIndexRequest{Name: "ix", KeyColumns: []IndexColumnDef{{Name: "a"}}})
	if err != nil {
		t.Fatalf("CreateIndex: %v", err)
	}
	if idx.Name != "ix" || idx.Table() != ref {
		t.Errorf("CreateIndex = %+v, want the IndexRef handle", idx)
	}
	st, err := ref.CreateStatistic(t.Context(), CreateStatisticRequest{Name: "st", Columns: []string{"a"}})
	if err != nil {
		t.Fatalf("CreateStatistic: %v", err)
	}
	if st.Name != "st" || st.Table() != ref {
		t.Errorf("CreateStatistic = %+v, want the StatisticRef handle", st)
	}
	if captured.find("INDEX [ix]") == "" || captured.find("STATISTICS [st]") == "" {
		t.Error("the creates were not sent")
	}
}
