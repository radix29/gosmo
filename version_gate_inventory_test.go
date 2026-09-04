package gosmo

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// gatedColumn is one version gate: the catalog view the column belongs to, the
// column itself, the expression the query names it with, and the release that
// introduced it.
//
// The inventory is written by hand from the citation on each call site, never
// generated from the call sites themselves — a wrong `since` copied out of the
// code would be pinned by a test that agrees with it forever. Its two jobs are
// separate: TestEveryVersionGateIsInTheInventory keeps it in step with the
// source, and the livedb TestLiveGatedColumnsMatchTheCatalog asks a real
// instance whether each gate decided correctly for its major.
type gatedColumn struct {
	view    string        // catalog view, unqualified, in the sys schema
	column  string        // the column as the catalog names it
	expr    string        // the col argument of the colSince call, verbatim
	since   ServerVersion // the release that introduced the column
	groupBy bool          // a hasColumnSince call drops a GROUP BY term with it

	// backported marks a column Microsoft added to an *older* major in a
	// service pack or CU, so its presence does not follow from the major at
	// all. The gate stays at the documented version, which is the only value
	// safe on an unserviced instance of every major; the live catalog check
	// then has to tolerate finding the column on a server the gate refuses it
	// to, while still failing the other way round — naming a column that is
	// absent kills the whole read, and no servicing level makes that safe.
	backported string
}

var gatedColumns = []gatedColumn{
	// sys.database_scoped_configurations — the view is 2016, is_value_default 2017.
	{"database_scoped_configurations", "is_value_default", "is_value_default", SQLServer2017, false, ""},

	// sys.database_query_store_options — wait stats 2017, the CUSTOM capture policy 2019.
	{"database_query_store_options", "wait_stats_capture_mode_desc", "wait_stats_capture_mode_desc", SQLServer2017, false, ""},
	{"database_query_store_options", "capture_policy_execution_count", "capture_policy_execution_count", SQLServer2019, false, ""},
	{"database_query_store_options", "capture_policy_total_compile_cpu_time_ms", "capture_policy_total_compile_cpu_time_ms", SQLServer2019, false, ""},
	{"database_query_store_options", "capture_policy_total_execution_cpu_time_ms", "capture_policy_total_execution_cpu_time_ms", SQLServer2019, false, ""},
	{"database_query_store_options", "capture_policy_stale_threshold_hours", "capture_policy_stale_threshold_hours", SQLServer2019, false, ""},

	// sys.query_store_plan — plan forcing type 2017; its GROUP BY term goes with it.
	{"query_store_plan", "plan_forcing_type_desc", "COALESCE(p.plan_forcing_type_desc, '')", SQLServer2017, true, ""},

	// sys.column_master_keys — the Always Encrypted with enclaves columns, 2019.
	{"column_master_keys", "allow_enclave_computations", "allow_enclave_computations", SQLServer2019, false, ""},
	{"column_master_keys", "signature", "signature", SQLServer2019, false, ""},

	// sys.availability_groups — 2016 flags, 2017 external cluster, 2022 contained AGs.
	{"availability_groups", "basic_features", "ag.basic_features", SQLServer2016, false, ""},
	{"availability_groups", "dtc_support", "ag.dtc_support", SQLServer2016, false, ""},
	{"availability_groups", "db_failover", "ag.db_failover", SQLServer2016, false, ""},
	{"availability_groups", "is_distributed", "ag.is_distributed", SQLServer2016, false, ""},
	{"availability_groups", "cluster_type_desc", "UPPER(ISNULL(ag.cluster_type_desc,''))", SQLServer2017, false, ""},
	{"availability_groups", "required_synchronized_secondaries_to_commit", "ag.required_synchronized_secondaries_to_commit", SQLServer2017, false, ""},
	{"availability_groups", "is_contained", "ag.is_contained", SQLServer2022, false, ""},

	// sys.availability_group_listeners — distributed network name, 2019.
	{"availability_group_listeners", "is_distributed_network_name", "ISNULL(l.is_distributed_network_name, 0)", SQLServer2019, false,
		"DNN was backported: win10cli\\sql2016 (13.0.6404.1, SP3) has the column while win10cli\\sql2017 (14.0.2120.1) does not"},

	// sys.tables — ledger tables, 2022.
	{"tables", "ledger_type_desc", "t.ledger_type_desc", SQLServer2022, false, ""},
}

// colSinceCall matches a colSince call site's version argument and its col
// argument, which is always a double-quoted literal — a computed expression
// there would be invisible to this scan, so the test below insists the two
// counts agree rather than only that every call it saw is known.
var colSinceCall = regexp.MustCompile(`colSince\([^,]+,\s*(SQLServer\d+),\s*"((?:[^"\\]|\\.)*)"`)

// A gate the inventory does not know about is a gate nothing checks against a
// real catalog, and a stale entry is a check of a query that no longer exists.
func TestEveryVersionGateIsInTheInventory(t *testing.T) {
	inSource := map[string]int{}
	colSinceSites, hasColumnSites := 0, 0
	for _, path := range gosmoSourceFiles(t) {
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		text := string(src)
		if filepath.Base(path) == "version_gate.go" {
			continue // the declarations themselves, not call sites
		}
		for _, m := range colSinceCall.FindAllStringSubmatch(text, -1) {
			inSource[m[1]+" "+m[2]]++
		}
		colSinceSites += strings.Count(text, "colSince(")
		hasColumnSites += strings.Count(text, "hasColumnSince(")
	}
	if sum := sumCounts(inSource); sum != colSinceSites {
		t.Errorf("scan matched %d colSince calls but the source has %d — a call site names its column with something other than a literal", sum, colSinceSites)
	}

	inInventory := map[string]int{}
	wantGroupBy := 0
	for _, g := range gatedColumns {
		inInventory[versionConstName(g.since)+" "+g.expr]++
		if g.groupBy {
			wantGroupBy++
		}
	}
	if hasColumnSites != wantGroupBy {
		t.Errorf("source has %d hasColumnSince calls, inventory declares %d groupBy gates", hasColumnSites, wantGroupBy)
	}
	for key, n := range inSource {
		if inInventory[key] != n {
			t.Errorf("gate %q: %d call sites, %d inventory entries", key, n, inInventory[key])
		}
	}
	for key, n := range inInventory {
		if inSource[key] != n {
			t.Errorf("inventory entry %q: %d entries, %d call sites — stale?", key, n, inSource[key])
		}
	}
}

// The column named in each entry must appear in the expression that selects
// it, or the live catalog check asks about a different column than the query
// reads.
func TestEveryInventoryEntryNamesItsColumn(t *testing.T) {
	for _, g := range gatedColumns {
		if !strings.Contains(g.expr, g.column) {
			t.Errorf("%s.%s: expression %q does not name the column", g.view, g.column, g.expr)
		}
	}
}

func sumCounts(m map[string]int) int {
	n := 0
	for _, v := range m {
		n += v
	}
	return n
}

// versionConstName renders a ServerVersion the way the source names it, so the
// inventory and the scan compare on the same key.
func versionConstName(v ServerVersion) string {
	switch v {
	case SQLServer2012:
		return "SQLServer2012"
	case SQLServer2014:
		return "SQLServer2014"
	case SQLServer2016:
		return "SQLServer2016"
	case SQLServer2017:
		return "SQLServer2017"
	case SQLServer2019:
		return "SQLServer2019"
	case SQLServer2022:
		return "SQLServer2022"
	case SQLServer2025:
		return "SQLServer2025"
	}
	return "unknown"
}

// gosmoSourceFiles lists the package's non-test .go files.
func gosmoSourceFiles(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	var files []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		files = append(files, name)
	}
	sort.Strings(files)
	return files
}
