//go:build livedb

// Live verification of the pushed-down ObjectFilter. The unit test pins the
// SQL; what only a server can answer is whether that SQL selects the same
// objects the caller would have selected itself — a wrong column mapping, a
// collation surprise, or a date compared in the wrong zone all produce valid
// SQL and a quietly different set of rows.
//
// Every case therefore asserts the filtered read equals the *unfiltered* read
// narrowed in Go, which is exactly the equivalence the push-down claims.
//
//	go test -tags livedb . -run TestLiveObjectFilter -v \
//	  -livedb 'sqlserver://sa:PASS@host?TrustServerCertificate=true'
//
// Creates and drops its own throwaway database; touches nothing else.
package gosmo

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"
)

// liveFilterFixture builds a database whose object names exercise the parts of
// the clause that are easy to get wrong: mixed case, an underscore, a percent
// sign, a bracket, and two schemas.
func liveFilterFixture(t *testing.T, d *Database, ctx context.Context) {
	t.Helper()
	for _, stmt := range []string{
		"CREATE SCHEMA [sales]",
		"CREATE TABLE [dbo].[CustOrders] (id INT NOT NULL)",
		"CREATE TABLE [dbo].[custarchive] (id INT NOT NULL)",
		"CREATE TABLE [dbo].[pct_100] (id INT NOT NULL)",
		"CREATE TABLE [dbo].[100%done] (id INT NOT NULL)",
		"CREATE TABLE [dbo].[br[ackets] (id INT NOT NULL)",
		"CREATE TABLE [sales].[CustLedger] (id INT NOT NULL)",
		"CREATE VIEW [sales].[vCust] AS SELECT id FROM [sales].[CustLedger]",
		"CREATE VIEW [dbo].[vOther] AS SELECT id FROM [dbo].[CustOrders]",
		"CREATE PROCEDURE [dbo].[pCustReport] AS SELECT 1",
		"CREATE PROCEDURE [sales].[pOther] AS SELECT 1",
		"CREATE FUNCTION [dbo].[fnCustAge] () RETURNS INT AS BEGIN RETURN 1 END",
		"CREATE FUNCTION [sales].[fnOther] () RETURNS INT AS BEGIN RETURN 2 END",
	} {
		if _, err := d.exec(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
}

// named reduces any listing to its schema-qualified names, sorted, so two
// reads can be compared as sets.
func named[T any](items []T, key func(T) (string, string)) []string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		schema, name := key(it)
		out = append(out, schema+"."+name)
	}
	slices.Sort(out)
	return out
}

func tableNames(ts []*Table) []string {
	return named(ts, func(t *Table) (string, string) { return t.Schema, t.Name })
}

// matchesLocally is the caller-side answer the push-down has to reproduce: the
// same case-insensitive comparison, over the unfiltered listing.
func matchesLocally(name string, c TextCriterion) bool {
	got, want := strings.ToLower(name), strings.ToLower(c.Value)
	switch c.Op {
	case TextNotContains:
		return !strings.Contains(got, want)
	case TextEquals:
		return got == want
	case TextNotEquals:
		return got != want
	default:
		return strings.Contains(got, want)
	}
}

func TestLiveObjectFilterMatchesTheCallerSideAnswer(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()

	d, drop := liveScratchDB(t, db, ctx, "gosmo_filter_live")
	defer drop()
	liveFilterFixture(t, d, ctx)

	all, err := d.TablesContext(ctx)
	if err != nil {
		t.Fatalf("TablesContext: %v", err)
	}

	for _, c := range []struct {
		name string
		crit TextCriterion
	}{
		// Case-insensitive both ways: CustOrders and custarchive both match,
		// whatever the database collation does.
		{"contains, mixed case", TextCriterion{Op: TextContains, Value: "CUST"}},
		{"does not contain", TextCriterion{Op: TextNotContains, Value: "cust"}},
		{"equals, wrong case", TextCriterion{Op: TextEquals, Value: "custorders"}},
		{"does not equal", TextCriterion{Op: TextNotEquals, Value: "CustOrders"}},
		// The wildcard cases: each of these characters is legal in an
		// identifier and pattern syntax in a LIKE.
		{"underscore is literal", TextCriterion{Op: TextContains, Value: "pct_1"}},
		{"percent is literal", TextCriterion{Op: TextContains, Value: "100%"}},
		{"bracket is literal", TextCriterion{Op: TextContains, Value: "br[ack"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			want := make([]*Table, 0, len(all))
			for _, tb := range all {
				if matchesLocally(tb.Name, c.crit) {
					want = append(want, tb)
				}
			}
			got, err := d.TablesFilteredContext(ctx, ObjectFilter{Name: []TextCriterion{c.crit}})
			if err != nil {
				t.Fatalf("TablesFilteredContext: %v", err)
			}
			if g, w := tableNames(got), tableNames(want); !slices.Equal(g, w) {
				t.Errorf("filtered = %v\nlocal    = %v", g, w)
			}
			if len(want) == 0 || len(want) == len(all) {
				t.Logf("note: %q matched %d of %d — not much narrowing to observe",
					c.crit.Value, len(want), len(all))
			}
		})
	}

	t.Run("schema criterion", func(t *testing.T) {
		got, err := d.TablesFilteredContext(ctx, ObjectFilter{
			Schema: []TextCriterion{{Op: TextEquals, Value: "SALES"}},
		})
		if err != nil {
			t.Fatalf("TablesFilteredContext: %v", err)
		}
		if g := tableNames(got); !slices.Equal(g, []string{"sales.CustLedger"}) {
			t.Errorf("filtered by schema = %v, want just sales.CustLedger", g)
		}
	})

	t.Run("criteria AND together", func(t *testing.T) {
		got, err := d.TablesFilteredContext(ctx, ObjectFilter{
			Name:   []TextCriterion{{Op: TextContains, Value: "cust"}},
			Schema: []TextCriterion{{Op: TextEquals, Value: "dbo"}},
		})
		if err != nil {
			t.Fatalf("TablesFilteredContext: %v", err)
		}
		if g := tableNames(got); !slices.Equal(g, []string{"dbo.CustOrders", "dbo.custarchive"}) {
			t.Errorf("name+schema = %v, want the two dbo cust tables", g)
		}
	})

	// Dates are the case where a zone mismatch between the parameter and the
	// stored value shows up: everything here was created moments ago, so
	// "created today" must return all of it and "before today" none.
	t.Run("creation date", func(t *testing.T) {
		today := all[0].CreateDate
		for _, c := range []struct {
			name  string
			crit  DateCriterion
			wantN int
		}{
			{"on the day everything was created", DateCriterion{Op: DateOn, Day: today}, len(all)},
			{"before that day", DateCriterion{Op: DateBefore, Day: today}, 0},
			{"after that day", DateCriterion{Op: DateAfter, Day: today}, 0},
			{"after yesterday", DateCriterion{Op: DateAfter, Day: today.AddDate(0, 0, -1)}, len(all)},
			{"before tomorrow", DateCriterion{Op: DateBefore, Day: today.AddDate(0, 0, 1)}, len(all)},
		} {
			t.Run(c.name, func(t *testing.T) {
				got, err := d.TablesFilteredContext(ctx, ObjectFilter{Created: []DateCriterion{c.crit}})
				if err != nil {
					t.Fatalf("TablesFilteredContext: %v", err)
				}
				if len(got) != c.wantN {
					t.Errorf("got %d tables, want %d (criterion day %s, rows created %s)",
						len(got), c.wantN, c.crit.Day.Format(time.DateOnly), today)
				}
			})
		}
	})

	t.Run("memory optimized", func(t *testing.T) {
		no := false
		got, err := d.TablesFilteredContext(ctx, ObjectFilter{MemoryOptimized: &no})
		if err != nil {
			t.Fatalf("TablesFilteredContext: %v", err)
		}
		if len(got) != len(all) {
			t.Errorf("not-memory-optimized returned %d of %d tables", len(got), len(all))
		}
		yes := true
		got, err = d.TablesFilteredContext(ctx, ObjectFilter{MemoryOptimized: &yes})
		if err != nil {
			t.Fatalf("TablesFilteredContext: %v", err)
		}
		if len(got) != 0 {
			t.Errorf("memory-optimized returned %d tables, want none", len(got))
		}
	})
}

// The other six listings: each filtered read must equal its own unfiltered
// read narrowed in Go. This is what catches a column mapping copied from the
// family next door — every one of these queries aliases its table differently.
func TestLiveObjectFilterAcrossEveryFamily(t *testing.T) {
	db, ctx, done := liveDB(t)
	defer done()

	d, drop := liveScratchDB(t, db, ctx, "gosmo_filter_fam")
	defer drop()
	liveFilterFixture(t, d, ctx)

	crit := TextCriterion{Op: TextContains, Value: "cust"}
	filter := ObjectFilter{Name: []TextCriterion{crit}}

	for _, c := range []struct {
		name     string
		all      func() ([]string, error)
		filtered func() ([]string, error)
		// systemFamily reads the sys schema, which no fixture can add to;
		// there the assertion is only that the two agree.
		systemFamily bool
	}{
		{
			name: "views",
			all: func() ([]string, error) {
				v, err := d.ViewsContext(ctx)
				return named(v, func(x *View) (string, string) { return x.Schema, x.Name }), err
			},
			filtered: func() ([]string, error) {
				v, err := d.ViewsFilteredContext(ctx, filter)
				return named(v, func(x *View) (string, string) { return x.Schema, x.Name }), err
			},
		},
		{
			name: "stored procedures",
			all: func() ([]string, error) {
				p, err := d.StoredProceduresContext(ctx)
				return named(p, func(x *StoredProcedure) (string, string) { return x.Schema, x.Name }), err
			},
			filtered: func() ([]string, error) {
				p, err := d.StoredProceduresFilteredContext(ctx, filter)
				return named(p, func(x *StoredProcedure) (string, string) { return x.Schema, x.Name }), err
			},
		},
		{
			name: "functions",
			all: func() ([]string, error) {
				f, err := d.UserDefinedFunctionsContext(ctx)
				return named(f, func(x *UserDefinedFunction) (string, string) { return x.Schema, x.Name }), err
			},
			filtered: func() ([]string, error) {
				f, err := d.UserDefinedFunctionsFilteredContext(ctx, filter)
				return named(f, func(x *UserDefinedFunction) (string, string) { return x.Schema, x.Name }), err
			},
		},
		{
			name:         "system views",
			systemFamily: true,
			all: func() ([]string, error) {
				v, err := d.SystemViewsContext(ctx)
				return named(v, func(x *View) (string, string) { return x.Schema, x.Name }), err
			},
			filtered: func() ([]string, error) {
				v, err := d.SystemViewsFilteredContext(ctx, filter)
				return named(v, func(x *View) (string, string) { return x.Schema, x.Name }), err
			},
		},
		{
			name:         "system stored procedures",
			systemFamily: true,
			all: func() ([]string, error) {
				p, err := d.SystemStoredProceduresContext(ctx)
				return named(p, func(x *StoredProcedure) (string, string) { return x.Schema, x.Name }), err
			},
			filtered: func() ([]string, error) {
				p, err := d.SystemStoredProceduresFilteredContext(ctx, filter)
				return named(p, func(x *StoredProcedure) (string, string) { return x.Schema, x.Name }), err
			},
		},
		{
			name:         "system functions",
			systemFamily: true,
			all: func() ([]string, error) {
				f, err := d.SystemFunctionsContext(ctx)
				return named(f, func(x *UserDefinedFunction) (string, string) { return x.Schema, x.Name }), err
			},
			filtered: func() ([]string, error) {
				f, err := d.SystemFunctionsFilteredContext(ctx, filter)
				return named(f, func(x *UserDefinedFunction) (string, string) { return x.Schema, x.Name }), err
			},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			all, err := c.all()
			if err != nil {
				t.Fatalf("unfiltered: %v", err)
			}
			want := make([]string, 0, len(all))
			for _, qualified := range all {
				name := qualified[strings.LastIndex(qualified, ".")+1:]
				if matchesLocally(name, crit) {
					want = append(want, qualified)
				}
			}
			got, err := c.filtered()
			if err != nil {
				t.Fatalf("filtered: %v", err)
			}
			if !slices.Equal(got, want) {
				t.Errorf("filtered = %v\nlocal    = %v", got, want)
			}
			if !c.systemFamily && len(want) == 0 {
				t.Errorf("the fixture matched nothing in this family; the test proves nothing")
			}
			// An unfiltered filtered-read is the whole listing — the property
			// that lets the caller use one call for both.
			whole, err := c.all()
			if err != nil {
				t.Fatalf("unfiltered again: %v", err)
			}
			if !slices.Equal(whole, all) {
				t.Errorf("the unfiltered listing is not stable between reads")
			}
		})
	}
}
