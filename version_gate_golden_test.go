package gosmo

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// updateGolden rewrites the golden files instead of comparing against them.
// Regenerating is not review: the point of the files is that a gate can only
// change through a diff a human reads, so run it, then read `git diff`.
var updateGolden = flag.Bool("updategolden", false, "rewrite the version-gate golden files")

// goldenMajors are the majors gosmo supports, from the floor to the newest
// release it knows. 13, 15 and 16 have no instance in the house and never
// will have one here — the golden file is the only thing that says what gosmo
// sends them.
var goldenMajors = []int{13, 14, 15, 16, 17}

// goldenQuery is one gated SELECT-list builder, rendered per major.
//
// views lists the catalog views the query reads that carry gated columns, so
// the rendering can be cross-checked against the hand-written gatedColumns
// inventory rather than only against itself: a golden file regenerated from a
// builder whose `since` is wrong would otherwise pin the bug forever.
type goldenQuery struct {
	name   string
	views  []string
	render func(major int) string
}

var goldenQueries = []goldenQuery{
	{"agcolumns", []string{"availability_groups"}, func(m int) string {
		return (&Server{info: &ServerInfo{VersionMajor: m}}).agColumns()
	}},
	{"listeners", []string{"availability_group_listeners"}, func(m int) string {
		return (&Server{info: &ServerInfo{VersionMajor: m}}).listenerSelect()
	}},
	{"column_master_keys", []string{"column_master_keys"}, func(m int) string {
		return dbAtMajor(m).columnMasterKeySelect()
	}},
	{"query_store_options", []string{"database_query_store_options"}, func(m int) string {
		return dbAtMajor(m).queryStoreOptionsSelect()
	}},
	{"query_store_plans", []string{"query_store_plan"}, func(m int) string {
		return dbAtMajor(m).queryStorePlansQuery("CAST(0 AS float)", "@p1", "@p2", "@p3")
	}},
	{"scoped_config", []string{"database_scoped_configurations"}, func(m int) string {
		return dbAtMajor(m).scopedConfigSelect()
	}},
	{"table_detail", []string{"tables"}, func(m int) string {
		return (&Table{db: dbAtMajor(m)}).detailSelect()
	}},
}

// renderGolden is the file's whole text: every major, in order, under a header
// naming it.
func renderGolden(q goldenQuery) string {
	var sb strings.Builder
	for _, major := range goldenMajors {
		fmt.Fprintf(&sb, "-- major %d\n%s\n", major, strings.TrimRight(q.render(major), "\n"))
	}
	return sb.String()
}

func goldenPath(name string) string {
	return filepath.Join("testdata", "version_gates", name+".sql")
}

// The SQL gosmo sends majors 13, 15 and 16 cannot be run anywhere here, so it
// is pinned as text. A gate that changes shows up as a diff on this file.
func TestGatedQueriesMatchTheirGoldenFiles(t *testing.T) {
	for _, q := range goldenQueries {
		t.Run(q.name, func(t *testing.T) {
			got := renderGolden(q)
			path := goldenPath(q.name)
			if *updateGolden {
				if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
					t.Fatalf("write %s: %v", path, err)
				}
				t.Logf("rewrote %s — read the diff before committing it", path)
				return
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read %s: %v (run go test -run TestGatedQueries -updategolden to create it)", path, err)
			}
			if got != string(want) {
				t.Errorf("rendered SQL differs from %s.\n--- got ---\n%s\n--- want ---\n%s", path, got, want)
			}
		})
	}
}

// The golden files are generated from the builders, so on their own they pin
// whatever the builders do — including a wrong `since`. This checks each
// rendering against gatedColumns instead, which is written from the citation
// on each call site and never from the code.
func TestGoldenRenderingsAgreeWithTheGateInventory(t *testing.T) {
	for _, q := range goldenQueries {
		gates := 0
		for _, g := range gatedColumns {
			if !containsString(q.views, g.view) {
				continue
			}
			gates++
			for _, major := range goldenMajors {
				sql := q.render(major)
				named := strings.Contains(sql, g.column)
				if want := hasColumnSince(major, g.since); named != want {
					t.Errorf("%s at major %d names %s.%s = %v, want %v (introduced in %s)",
						q.name, major, g.view, g.column, named, want, versionConstName(g.since))
				}
			}
		}
		if gates == 0 {
			t.Errorf("%s has no inventory entry for any of %v — the golden file checks nothing but itself", q.name, q.views)
		}
	}
}

// Every gated column in the inventory must be reachable from a golden query,
// or a gate exists that no golden file covers.
func TestEveryGatedColumnHasAGoldenQuery(t *testing.T) {
	for _, g := range gatedColumns {
		covered := false
		for _, q := range goldenQueries {
			if containsString(q.views, g.view) {
				covered = true
			}
		}
		if !covered {
			t.Errorf("%s.%s is gated but no golden query reads %s", g.view, g.column, g.view)
		}
	}
}

// Arity is what A4 broke: a gate that drops a column instead of substituting
// one leaves the scan destination list one short, and nothing says so until a
// live run on that major. Per-query destination counts are asserted in
// version_gate_test.go; this is the blanket rule.
func TestGatedQueriesHaveTheSameArityAtEveryMajor(t *testing.T) {
	for _, q := range goldenQueries {
		want := -1
		for _, major := range goldenMajors {
			sql := q.render(major)
			list := sql
			if strings.Contains(sql, "SELECT") {
				list = selectList(t, sql)
			}
			n := len(selectExprs(t, list))
			if want == -1 {
				want = n
				continue
			}
			if n != want {
				t.Errorf("%s selects %d expressions at major %d, %d at major %d — a Scan arity error on one of them",
					q.name, n, major, want, goldenMajors[0])
			}
		}
	}
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
