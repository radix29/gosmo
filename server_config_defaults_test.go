package gosmo

import (
	"bufio"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
)

// testdataConfigurationNames reads testdata/configurations/<major>.txt, the
// sys.configurations names a real instance of that major listed.
func testdataConfigurationNames(t *testing.T, major int) []string {
	t.Helper()
	f, err := os.Open(fmt.Sprintf("testdata/configurations/%d.txt", major))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var names []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if line := sc.Text(); line != "" && !strings.HasPrefix(line, "#") {
			names = append(names, line)
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return names
}

// TestConfigDefaultsNamesExistOnTheirMajor: every option the table gives a
// default on 13, 14 or 17 is a name that major's sys.configurations has,
// spelled exactly — a typo or a wrong since would otherwise leave a real
// option without a default and a phantom one with it. 15 and 16 have no
// instance to check against.
func TestConfigDefaultsNamesExistOnTheirMajor(t *testing.T) {
	// Added by a CU after the 17.0.1145 instance the list was read from.
	laterCU := map[int][]string{17: {"max lock manager cache memory (%)"}}
	for _, major := range []int{13, 14, 17} {
		have := testdataConfigurationNames(t, major)
		for name := range configDefaultsByMajor[major] {
			if !slices.Contains(have, name) && !slices.Contains(laterCU[major], name) {
				t.Errorf("major %d: the table has a default for %q, which sys.configurations does not list", major, name)
			}
		}
	}
}

// TestConfigDefaultsOmissionsAreTheDocumentedOnes: what a real instance lists
// and the table has no default for is exactly the list configDefaults's doc
// comment gives — so a new option is a decision, not an oversight.
func TestConfigDefaultsOmissionsAreTheDocumentedOnes(t *testing.T) {
	want := map[int][]string{
		13: nil,
		14: nil,
		17: {
			"ADR cleaner lock timeout (s)", "allow filesystem enumeration", "external AI runtimes enabled",
			"max server tiered memory (MB)", "openrowset auto_create_statistics", "SLOG memory quota (%)",
			"tiered memory enabled", "version high part of SQL Server", "version low part of SQL Server",
		},
	}
	for major, w := range want {
		var missing []string
		for _, name := range testdataConfigurationNames(t, major) {
			if _, ok := configurationDefault(major, name); !ok {
				missing = append(missing, name)
			}
		}
		slices.Sort(missing)
		slices.Sort(w)
		if !slices.Equal(missing, w) {
			t.Errorf("major %d: no default for %q, want exactly %q", major, missing, w)
		}
	}
}

// TestConfigDefaultsRows: no option has two rows covering one major, and no
// row starts below the supported floor or ends before it starts.
func TestConfigDefaultsRows(t *testing.T) {
	for i, a := range configDefaults {
		if a.since < MinimumServerVersion || (a.until != 0 && a.until <= a.since) {
			t.Errorf("%q: range [%d, %d) is not a supported one", a.name, a.since, a.until)
		}
		for _, b := range configDefaults[i+1:] {
			if a.name != b.name {
				continue
			}
			aEnd, bEnd := a.until, b.until
			if aEnd == 0 {
				aEnd = newestDefaultsMajor + 1
			}
			if bEnd == 0 {
				bEnd = newestDefaultsMajor + 1
			}
			if a.since < bEnd && b.since < aEnd {
				t.Errorf("%q: two rows cover the same major", a.name)
			}
		}
	}
}

// TestConfigurationDefaultLookup pins the edges: a default that changed
// between majors, an option before its major, a major newer than the table,
// and Azure (feature major 0).
func TestConfigurationDefaultLookup(t *testing.T) {
	cases := []struct {
		major int
		name  string
		want  int64
		ok    bool
	}{
		{13, "max degree of parallelism", 0, true},
		{17, "max server memory (MB)", 2147483647, true},
		{17, "cost threshold for parallelism", 5, true},
		{17, "Agent XPs", 0, true}, // the engine's, not what starting Agent sets
		{15, "ADR cleaner retry timeout (min)", 120, true},
		{16, "ADR cleaner retry timeout (min)", 15, true},
		{17, "ADR cleaner retry timeout (min)", 15, true},
		{14, "ADR cleaner retry timeout (min)", 0, false},
		{13, "clr strict security", 0, false},
		{14, "clr strict security", 1, true},
		{17, "Max Degree Of Parallelism", 0, false}, // exact names only
		{18, "max degree of parallelism", 0, false},
		{0, "max degree of parallelism", 0, false},
		{17, "version high part of SQL Server", 0, false},
	}
	for _, c := range cases {
		got, ok := configurationDefault(c.major, c.name)
		if got != c.want || ok != c.ok {
			t.Errorf("configurationDefault(%d, %q) = %d, %v; want %d, %v", c.major, c.name, got, ok, c.want, c.ok)
		}
	}
}

// TestFillDefaultFollowsTheServer: the default comes from the option's own
// server — its major, and none on Azure, whose ProductVersion is frozen at 12.
func TestFillDefaultFollowsTheServer(t *testing.T) {
	onPrem := &Server{info: &ServerInfo{VersionMajor: 16}}
	c := &ConfigurationOption{server: onPrem, Name: "ADR cleaner retry timeout (min)"}
	c.fillDefault()
	if c.Default != 15 || !c.HasDefault {
		t.Errorf("on 16: Default %d, HasDefault %v; want 15, true", c.Default, c.HasDefault)
	}

	mi := &Server{info: &ServerInfo{VersionMajor: 12, EngineEdition: int(EngineAzureManagedInst)}}
	c = &ConfigurationOption{server: mi, Name: "max degree of parallelism", Default: 99, HasDefault: true}
	c.fillDefault()
	if c.Default != 0 || c.HasDefault {
		t.Errorf("on a Managed Instance: Default %d, HasDefault %v; want 0, false", c.Default, c.HasDefault)
	}
}
