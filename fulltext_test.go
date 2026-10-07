package gosmo

import (
	"strings"
	"testing"
)

func TestFullTextEnumStrings(t *testing.T) {
	for v, want := range map[FullTextUpgradeOption]string{
		FullTextUpgradeRebuild: "Rebuild", FullTextUpgradeReset: "Reset", FullTextUpgradeImport: "Import",
		7: "FullTextUpgradeOption(7)",
	} {
		if got := v.String(); got != want {
			t.Errorf("FullTextUpgradeOption(%d) = %q, want %q", int(v), got, want)
		}
	}
	for v, want := range map[FullTextCatalogPopulateStatus]string{
		FullTextCatalogIdle: "Idle", FullTextCatalogIncrementalPopulation: "Incremental population in progress",
		FullTextCatalogChangeTracking: "Change tracking", 10: "FullTextCatalogPopulateStatus(10)",
	} {
		if got := v.String(); got != want {
			t.Errorf("FullTextCatalogPopulateStatus(%d) = %q, want %q", int(v), got, want)
		}
	}
	for v, want := range map[FullTextTablePopulateStatus]string{
		FullTextTableIdle: "Idle", FullTextTableThrottledOrPaused: "Throttled or paused",
		-1: "FullTextTablePopulateStatus(-1)", 6: "FullTextTablePopulateStatus(6)",
	} {
		if got := v.String(); got != want {
			t.Errorf("FullTextTablePopulateStatus(%d) = %q, want %q", int(v), got, want)
		}
	}
	for v, want := range map[FullTextStoplistKind]string{
		FullTextStoplistOff: "Off", FullTextStoplistSystem: "System", FullTextStoplistUser: "User",
		3: "FullTextStoplistKind(3)",
	} {
		if got := v.String(); got != want {
			t.Errorf("FullTextStoplistKind(%d) = %q, want %q", int(v), got, want)
		}
	}
}

// Every catalog and table status SQL Server documents has a name, so none
// reaches a caller as a bare number.
func TestFullTextPopulateStatusesAreAllNamed(t *testing.T) {
	for s := FullTextCatalogIdle; s <= FullTextCatalogChangeTracking; s++ {
		if strings.HasPrefix(s.String(), "FullTextCatalogPopulateStatus(") {
			t.Errorf("catalog status %d has no name", int(s))
		}
	}
	for s := FullTextTableIdle; s <= FullTextTableThrottledOrPaused; s++ {
		if strings.HasPrefix(s.String(), "FullTextTablePopulateStatus(") {
			t.Errorf("table status %d has no name", int(s))
		}
	}
}

// index_version is 2025's; below it the read substitutes 1, the only
// version there is, and keeps its arity.
func TestFullTextIndexSelectGatesIndexVersion(t *testing.T) {
	for _, c := range []struct {
		major int
		named bool
	}{{13, false}, {14, false}, {16, false}, {17, true}, {0, true}} {
		q := dbAtMajor(c.major).fullTextIndexSelect("")
		if got := strings.Contains(q, "fi.index_version"); got != c.named {
			t.Errorf("major %d: names index_version = %v, want %v", c.major, got, c.named)
		}
		if !c.named && !strings.Contains(q, "CAST(1 AS int)") {
			t.Errorf("major %d: no substitute for index_version", c.major)
		}
	}
}
