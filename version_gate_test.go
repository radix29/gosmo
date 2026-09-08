package gosmo

import (
	"context"
	"strings"
	"testing"
)

func TestColSinceNamesTheColumnOnlyFromItsVersion(t *testing.T) {
	const col, zero = "ag.is_contained", "CAST(0 AS bit)"
	cases := []struct {
		major int
		want  string
	}{
		{11, zero},
		{12, zero},
		{13, zero},
		{14, zero},
		{15, zero},
		{16, col}, // the introducing version itself
		{17, col},
		{0, col}, // never read: treat as newest
	}
	for _, c := range cases {
		if got := colSince(c.major, SQLServer2022, col, zero); got != c.want {
			t.Errorf("colSince(major %d, 2022) = %q, want %q", c.major, got, c.want)
		}
	}
}

// The substitute must carry a CAST: the driver types a bare literal by how it
// parsed, and a destination expecting a bit or a bigint then fails to scan it.
// Which expressions are substitutes is not guessed — they are exactly the ones
// that differ from the newest rendering.
func TestAgColumnsSubstitutesAreCast(t *testing.T) {
	newest := selectExprs(t, (&Server{info: &ServerInfo{VersionMajor: 17}}).agColumns())
	oldest := selectExprs(t, (&Server{info: &ServerInfo{VersionMajor: int(MinimumServerVersion)}}).agColumns())
	if len(newest) != len(oldest) {
		t.Fatalf("arity differs: %d at major 17, %d at major %d", len(newest), len(oldest), MinimumServerVersion)
	}
	substitutes := 0
	for i := range newest {
		if newest[i] == oldest[i] {
			continue
		}
		substitutes++
		if !strings.HasPrefix(oldest[i], "CAST(") {
			t.Errorf("expression %d substitutes %q for %q, which is not a CAST", i, oldest[i], newest[i])
		}
	}
	// 2017 gates cluster_type_desc and required_synchronized_secondaries_to_commit,
	// 2022 gates is_contained; the 2016 flags are named at the floor.
	if substitutes != 3 {
		t.Errorf("major %d substitutes %d expressions, want 3", MinimumServerVersion, substitutes)
	}
}

// The whole point of substituting a literal rather than dropping the column is
// that the scan destination list never has to change. A gate that drops one
// shows up here and nowhere else until a live run on that major.
func TestAgColumnsHasTheSameArityAtEveryMajor(t *testing.T) {
	// scanAvailabilityGroup passes 18 destinations.
	const want = 18
	for _, major := range []int{0, 11, 12, 13, 14, 15, 16, 17} {
		s := &Server{info: &ServerInfo{VersionMajor: major}}
		if got := len(selectExprs(t, s.agColumns())); got != want {
			t.Errorf("agColumns at major %d selects %d expressions, want %d — "+
				"scanAvailabilityGroup will fail with a Scan arity error there", major, got, want)
		}
	}
}

func TestMinimumServerVersionIs2016(t *testing.T) {
	if MinimumServerVersion != SQLServer2016 {
		t.Errorf("MinimumServerVersion = %d, want %d (2016 SP1, the CREATE OR ALTER floor)",
			MinimumServerVersion, SQLServer2016)
	}
	if SQLServer2025 != 17 {
		t.Errorf("SQLServer2025 = %d, want 17", SQLServer2025)
	}
}

// selectExprs splits a SELECT list on its top-level commas — the ones outside
// any parentheses, since ISNULL(x,”) and CAST(...) both contain their own.
func selectExprs(t *testing.T, list string) []string {
	t.Helper()
	var out []string
	depth, start := 0, 0
	for i, r := range list {
		switch r {
		case '(':
			depth++
		case ')':
			depth--
		case ',':
			if depth == 0 {
				out = append(out, strings.TrimSpace(list[start:i]))
				start = i + 1
			}
		}
	}
	if depth != 0 {
		t.Fatalf("unbalanced parentheses in %q", list)
	}
	out = append(out, strings.TrimSpace(list[start:]))
	// An empty expression is what a gate whose substitute is "" produces: the
	// arity looks unchanged because the comma is still there, and the server
	// rejects the statement. Catch it here rather than live.
	for i, expr := range out {
		if expr == "" {
			t.Errorf("expression %d of %q is empty — a gate substituted nothing", i, list)
		}
	}
	return out
}

// dbAtMajor is a Database whose only purpose is to answer serverMajorVersion,
// which is all a gated SELECT-list builder reads.
func dbAtMajor(major int) *Database {
	return &Database{server: &Server{info: &ServerInfo{VersionMajor: major}}}
}

// A1: allow_enclave_computations and signature are SQL Server 2019.
func TestColumnMasterKeySelectGatesTheEnclaveColumns(t *testing.T) {
	for _, c := range []struct{ major, wantNamed int }{
		{13, 0}, {14, 0}, {15, 2}, {16, 2}, {17, 2}, {0, 2},
	} {
		q := dbAtMajor(c.major).columnMasterKeySelect()
		named := 0
		for _, col := range []string{"allow_enclave_computations", "signature"} {
			if strings.Contains(q, col) {
				named++
			}
		}
		if named != c.wantNamed {
			t.Errorf("major %d names %d of the two enclave columns, want %d:\n%s", c.major, named, c.wantNamed, q)
		}
		if got := len(selectExprs(t, selectList(t, q))); got != 6 {
			t.Errorf("major %d selects %d expressions, want 6 — scanColumnMasterKey passes 6 destinations", c.major, got)
		}
	}
}

// A2 and C2: wait_stats_capture_mode_desc is SQL Server 2017, the four
// capture_policy_* columns SQL Server 2019.
func TestQueryStoreOptionsSelectGatesItsLateColumns(t *testing.T) {
	policy := []string{
		"capture_policy_execution_count",
		"capture_policy_total_compile_cpu_time_ms",
		"capture_policy_total_execution_cpu_time_ms",
		"capture_policy_stale_threshold_hours",
	}
	for _, c := range []struct {
		major      int
		wantWait   bool
		wantPolicy int
	}{
		{13, false, 0}, {14, true, 0}, {15, true, 4}, {16, true, 4}, {17, true, 4}, {0, true, 4},
	} {
		q := dbAtMajor(c.major).queryStoreOptionsSelect()
		if got := strings.Contains(q, "wait_stats_capture_mode_desc"); got != c.wantWait {
			t.Errorf("major %d names wait_stats_capture_mode_desc = %v, want %v", c.major, got, c.wantWait)
		}
		named := 0
		for _, col := range policy {
			if strings.Contains(q, col) {
				named++
			}
		}
		if named != c.wantPolicy {
			t.Errorf("major %d names %d of the four capture_policy_* columns, want %d", c.major, named, c.wantPolicy)
		}
		if got := len(selectExprs(t, selectList(t, q))); got != 16 {
			t.Errorf("major %d selects %d expressions, want 16 — QueryStoreContext scans 16 destinations", c.major, got)
		}
	}
}

// selectList returns the expressions between SELECT and its own FROM — the
// one at paren depth 0, since a select-list ISNULL((SELECT ... FROM ...), ”)
// carries a FROM of its own.
func selectList(t *testing.T, q string) string {
	t.Helper()
	_, rest, ok := strings.Cut(q, "SELECT ")
	if !ok {
		t.Fatalf("no SELECT in %q", q)
	}
	depth := 0
	for i, r := range rest {
		switch r {
		case '(':
			depth++
		case ')':
			depth--
		case 'F':
			if depth == 0 && strings.HasPrefix(rest[i:], "FROM") {
				return rest[:i]
			}
		}
	}
	t.Fatalf("no top-level FROM in %q", q)
	return ""
}

// A3: ledger_type_desc is SQL Server 2022.
func TestTableDetailSelectGatesTheLedgerColumn(t *testing.T) {
	for _, c := range []struct {
		major     int
		wantNamed bool
	}{
		{13, false}, {14, false}, {15, false}, {16, true}, {17, true}, {0, true},
	} {
		tb := &Table{db: dbAtMajor(c.major)}
		q := tb.detailSelect()
		if got := strings.Contains(q, "ledger_type_desc"); got != c.wantNamed {
			t.Errorf("major %d names ledger_type_desc = %v, want %v", c.major, got, c.wantNamed)
		}
		if got := len(selectExprs(t, selectList(t, q))); got != 10 {
			t.Errorf("major %d selects %d expressions, want 10 — DetailContext scans 10 destinations", c.major, got)
		}
	}
}

// B1: is_distributed_network_name is SQL Server 2019.
func TestListenerSelectGatesTheDistributedNetworkNameColumn(t *testing.T) {
	for _, c := range []struct {
		major     int
		wantNamed bool
	}{
		{13, false}, {14, false}, {15, true}, {16, true}, {17, true}, {0, true},
	} {
		q := (&Server{info: &ServerInfo{VersionMajor: c.major}}).listenerSelect()
		if got := strings.Contains(q, "is_distributed_network_name"); got != c.wantNamed {
			t.Errorf("major %d names is_distributed_network_name = %v, want %v", c.major, got, c.wantNamed)
		}
		if got := len(selectExprs(t, selectList(t, q))); got != 7 {
			t.Errorf("major %d selects %d expressions, want 7 — ListenersContext scans 7 destinations", c.major, got)
		}
	}
}

// B2: ENCLAVE_COMPUTATIONS is SQL Server 2019 syntax, and below it the parser
// rejects the whole CREATE — so the refusal has to happen before the statement
// is sent, not be left to the server.
func TestCreateColumnMasterKeyWithSignatureRefusesEnclaveComputationsBelow2019(t *testing.T) {
	for _, c := range []struct {
		major    int
		wantEmit bool
	}{
		{13, false}, {14, false}, {15, true}, {16, true}, {17, true}, {0, true},
	} {
		ctx, script := WithScript(context.Background())
		err := dbAtMajor(c.major).CreateColumnMasterKeyWithSignatureContext(
			ctx, "CMK1", "MSSQL_CERTIFICATE_STORE", "CurrentUser/my/ab", []byte{0x0a, 0xff})
		if c.wantEmit {
			if err != nil {
				t.Errorf("major %d: err = %v, want the create to go through", c.major, err)
			}
			if len(script.Statements) != 1 {
				t.Errorf("major %d emitted %d statement(s), want 1", c.major, len(script.Statements))
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), "SQL Server 2019 or later") {
			t.Errorf("major %d: err = %v, want a refusal naming the version requirement", c.major, err)
		}
		if len(script.Statements) != 0 {
			t.Errorf("major %d emitted %d statement(s), want none:\n%s", c.major,
				len(script.Statements), strings.Join(script.Statements, "\n---\n"))
		}
	}
}

// C4: plan_forcing_type_desc is SQL Server 2017. The GROUP BY term is gated
// with it — SQL Server rejects a constant there, so substituting rather than
// dropping it would trade one error for another.
func TestQueryStorePlansQueryGatesThePlanForcingTypeColumn(t *testing.T) {
	for _, c := range []struct {
		major     int
		wantNamed bool
	}{
		{13, false}, {14, true}, {15, true}, {16, true}, {17, true}, {0, true},
	} {
		q := dbAtMajor(c.major).queryStorePlansQuery("CAST(0 AS float)", "@p1", "@p2", "@p3")
		if got := strings.Contains(q, "plan_forcing_type_desc"); got != c.wantNamed {
			t.Errorf("major %d names plan_forcing_type_desc = %v, want %v", c.major, got, c.wantNamed)
		}
		_, groupBy, ok := strings.Cut(q, "GROUP BY ")
		if !ok {
			t.Fatalf("major %d: no GROUP BY in %q", c.major, q)
		}
		groupBy, _, _ = strings.Cut(groupBy, "ORDER BY")
		if got := strings.Contains(groupBy, "plan_forcing_type_desc"); got != c.wantNamed {
			t.Errorf("major %d groups by plan_forcing_type_desc = %v, want %v", c.major, got, c.wantNamed)
		}
		// A constant in the GROUP BY is a SQL Server error, so the substitute
		// must be absent from it rather than present as a CAST.
		if strings.Contains(groupBy, "CAST(") {
			t.Errorf("major %d groups by a constant: %q", c.major, groupBy)
		}
		if got := len(selectExprs(t, selectList(t, q))); got != 14 {
			t.Errorf("major %d selects %d expressions, want 14 — QueryStorePlansContext scans 14 destinations", c.major, got)
		}
	}
}

// C4: is_value_default is SQL Server 2017, on a view that is 2016.
func TestScopedConfigSelectGatesTheIsValueDefaultColumn(t *testing.T) {
	for _, c := range []struct {
		major     int
		wantNamed bool
	}{
		{13, false}, {14, true}, {15, true}, {16, true}, {17, true}, {0, true},
	} {
		q := dbAtMajor(c.major).scopedConfigSelect()
		if got := strings.Contains(q, "is_value_default"); got != c.wantNamed {
			t.Errorf("major %d names is_value_default = %v, want %v", c.major, got, c.wantNamed)
		}
		if got := len(selectExprs(t, selectList(t, q))); got != 5 {
			t.Errorf("major %d selects %d expressions, want 5 — DatabaseScopedConfigsContext scans 5 destinations", c.major, got)
		}
	}
}

// azureMI is the ServerInfo a live General Purpose Gen5 Managed Instance
// returns: engine edition 8 with SQL Server 2014's version number, while the
// engine is an 18.x build carrying every catalog column gosmo gates on.
func azureMI() *ServerInfo {
	return &ServerInfo{EngineEdition: int(EngineAzureManagedInst), VersionMajor: 12,
		ProductVersion: "12.0.2000.8", Edition: "SQL Azure"}
}

func TestIsAzureCoversEveryAzureEngineEdition(t *testing.T) {
	for _, c := range []struct {
		edition EngineEdition
		want    bool
	}{
		{EnginePersonal, false}, {EngineStandard, false}, {EngineEnterprise, false},
		{EngineExpress, false}, {EngineAzureSQLDatabase, true}, {EngineAzureSynapse, true},
		{7, false}, {EngineAzureManagedInst, true}, {EngineAzureSQLEdge, true},
		{10, false}, {EngineAzureSynapseSrvls, true}, {12, false}, {0, false},
	} {
		if got := c.edition.IsAzure(); got != c.want {
			t.Errorf("EngineEdition(%d).IsAzure() = %v, want %v", c.edition, got, c.want)
		}
		info := &ServerInfo{EngineEdition: int(c.edition)}
		if got := info.IsAzure(); got != c.want {
			t.Errorf("ServerInfo{EngineEdition: %d}.IsAzure() = %v, want %v", c.edition, got, c.want)
		}
	}
	if (*ServerInfo)(nil).IsAzure() {
		t.Error("nil ServerInfo.IsAzure() = true, want false")
	}
}

// The whole point of item 2 of the Azure plan: an MI's 12 must not put it
// below every gate here. VersionMajor itself keeps the 12, for display.
func TestAzureMajorIsZeroSoEveryGateTreatsItAsNewest(t *testing.T) {
	s := &Server{info: azureMI()}
	if got := s.serverMajorVersion(); got != 0 {
		t.Errorf("Server.serverMajorVersion() on MI = %d, want 0", got)
	}
	if got := (&Database{server: s}).serverMajorVersion(); got != 0 {
		t.Errorf("Database.serverMajorVersion() on MI = %d, want 0", got)
	}
	if got := s.Info().VersionMajor; got != 12 {
		t.Errorf("Info().VersionMajor = %d, want 12 — the displayed version is unchanged", got)
	}

	d := &Database{name: "GoTest01", server: s}
	if !d.QueryStoreWaitStatsSupported() {
		t.Error("QueryStoreWaitStatsSupported() = false on MI, which has sys.query_store_wait_stats")
	}
	for _, c := range []struct {
		what string
		q    string
		col  string
	}{
		{"scoped configs", d.scopedConfigSelect(), "is_value_default"},
		{"column master keys", d.columnMasterKeySelect(), "allow_enclave_computations"},
	} {
		if !strings.Contains(c.q, c.col) {
			t.Errorf("%s on MI omits %s, which the instance has", c.what, c.col)
		}
	}
}

// Both filesystem gates fall back to an extended procedure below their
// version. Those work on MI, so nothing errors — it just silently loses
// columns, which is why this is pinned rather than left to a live run.
func TestAzureTakesTheModernFilesystemPaths(t *testing.T) {
	s := &Server{info: azureMI()}
	if s.EnumFileSystemIsLegacy() {
		t.Error("EnumFileSystemIsLegacy() = true on MI, which has sys.dm_os_enumerate_filesystem")
	}
	if !(&Server{}).EnumFileSystemIsLegacy() {
		t.Error("EnumFileSystemIsLegacy() = false with no info, want the xp_dirtree path")
	}
	if s.serverMajorVersion() >= 15 {
		t.Error("FixedDrivesContext would take the xp_fixeddrives path on MI")
	}
}
