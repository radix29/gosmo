package gosmo

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// rgStatements runs write under WithScript and returns what it collected.
func rgStatements(t *testing.T, write func(ctx context.Context) error) []string {
	t.Helper()
	ctx, col := WithScript(context.Background())
	if err := write(ctx); err != nil {
		t.Fatalf("write: %v", err)
	}
	return col.Statements()
}

func wantOne(t *testing.T, got []string, want string) {
	t.Helper()
	if len(got) != 1 || got[0] != want {
		t.Errorf("got %q\nwant [%q]", got, want)
	}
}

func TestResourceGovernorStatementShapes(t *testing.T) {
	rg := (&Server{}).ResourceGovernorRef()
	cases := []struct {
		name  string
		write func(ctx context.Context) error
		want  string
	}{
		{"reconfigure", rg.Reconfigure, "ALTER RESOURCE GOVERNOR RECONFIGURE"},
		// SQL Server has no ENABLE: RECONFIGURE is how the governor is enabled.
		{"enable", rg.Enable, "ALTER RESOURCE GOVERNOR RECONFIGURE"},
		{"disable", rg.Disable, "ALTER RESOURCE GOVERNOR DISABLE"},
		{"reset statistics", rg.ResetStatistics, "ALTER RESOURCE GOVERNOR RESET STATISTICS"},
		{"set classifier", func(ctx context.Context) error { return rg.SetClassifier(ctx, "dbo", "clf]x") },
			"ALTER RESOURCE GOVERNOR WITH (CLASSIFIER_FUNCTION = [dbo].[clf]]x])"},
		{"remove classifier", func(ctx context.Context) error { return rg.SetClassifier(ctx, "", "") },
			"ALTER RESOURCE GOVERNOR WITH (CLASSIFIER_FUNCTION = NULL)"},
		{"max outstanding io", func(ctx context.Context) error { return rg.SetMaxOutstandingIOPerVolume(ctx, 60) },
			"ALTER RESOURCE GOVERNOR WITH (MAX_OUTSTANDING_IO_PER_VOLUME = 60)"},
		// The server refuses a literal 0 (Msg 1040); DEFAULT is the reset.
		{"max outstanding io reset", func(ctx context.Context) error { return rg.SetMaxOutstandingIOPerVolume(ctx, 0) },
			"ALTER RESOURCE GOVERNOR WITH (MAX_OUTSTANDING_IO_PER_VOLUME = DEFAULT)"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { wantOne(t, rgStatements(t, c.write), c.want) })
	}
}

func TestSetClassifierRequiresASchema(t *testing.T) {
	ctx, col := WithScript(context.Background())
	err := (&Server{}).ResourceGovernorRef().SetClassifier(ctx, "", "clf")
	if !errors.Is(err, ErrSchemaRequired) {
		t.Errorf("SetClassifier with no schema = %v, want ErrSchemaRequired", err)
	}
	if len(col.Statements()) != 0 {
		t.Errorf("a statement was built anyway: %v", col.Statements())
	}
}

// Under WithScript nothing ran, so the receiver must not claim the change.
func TestResourceGovernorWritesDoNotMirrorUnderScript(t *testing.T) {
	rg := &ResourceGovernor{server: &Server{}, ClassifierSchema: "dbo", ClassifierName: "old", ClassifierFunctionID: 7}
	rgStatements(t, func(ctx context.Context) error {
		if err := rg.Reconfigure(ctx); err != nil {
			return err
		}
		return rg.SetClassifier(ctx, "", "")
	})
	if rg.IsEnabled || rg.ClassifierName != "old" || rg.ClassifierFunctionID != 7 {
		t.Errorf("scripted writes mirrored onto the receiver: %+v", *rg)
	}
}

func TestResourcePoolStatementShapes(t *testing.T) {
	s := &Server{}
	wantOne(t, rgStatements(t, func(ctx context.Context) error {
		p, err := s.CreateResourcePool(ctx, CreateResourcePoolRequest{Name: "p]1"})
		if err == nil && (p == nil || p.Name != "p]1") {
			t.Errorf("CreateResourcePool under script returned %#v, want the name-only handle", p)
		}
		return err
	}), "CREATE RESOURCE POOL [p]]1]")

	wantOne(t, rgStatements(t, func(ctx context.Context) error {
		_, err := s.CreateResourcePool(ctx, CreateResourcePoolRequest{Name: "p", Options: ResourcePoolOptions{
			MinCPUPercent: Ptr(5), MaxCPUPercent: Ptr(60), CapCPUPercent: Ptr(70),
			MinMemoryPercent: Ptr(0), MaxMemoryPercent: Ptr(50),
			MinIOPSPerVolume: Ptr(10), MaxIOPSPerVolume: Ptr(500),
		}})
		return err
	}), "CREATE RESOURCE POOL [p] WITH (MIN_CPU_PERCENT = 5, MAX_CPU_PERCENT = 60, CAP_CPU_PERCENT = 70, "+
		"MIN_MEMORY_PERCENT = 0, MAX_MEMORY_PERCENT = 50, MIN_IOPS_PER_VOLUME = 10, MAX_IOPS_PER_VOLUME = 500)")

	p := s.ResourcePoolRef("p")
	wantOne(t, rgStatements(t, func(ctx context.Context) error {
		return p.Alter(ctx, ResourcePoolOptions{MaxCPUPercent: Ptr(40)})
	}), "ALTER RESOURCE POOL [p] WITH (MAX_CPU_PERCENT = 40)")
	if p.MaxCPUPercent != 0 {
		t.Error("a scripted Alter mirrored MaxCPUPercent onto the receiver")
	}
	// ALTER RESOURCE POOL with no WITH, or WITH (), is a syntax error.
	if got := rgStatements(t, func(ctx context.Context) error { return p.Alter(ctx, ResourcePoolOptions{}) }); len(got) != 0 {
		t.Errorf("an empty Alter issued %q", got)
	}
	wantOne(t, rgStatements(t, p.Drop), "DROP RESOURCE POOL [p]")

	if _, err := s.CreateResourcePool(context.Background(), CreateResourcePoolRequest{Name: " "}); err == nil {
		t.Error("a pool with no name was accepted")
	}
}

func TestWorkloadGroupStatementShapes(t *testing.T) {
	s := &Server{}
	cases := []struct {
		name string
		opts WorkloadGroupOptions
		want string
	}{
		{"defaults", WorkloadGroupOptions{}, "CREATE WORKLOAD GROUP [g]"},
		{"pool only", WorkloadGroupOptions{Pool: Ptr("p")}, "CREATE WORKLOAD GROUP [g] USING [p]"},
		{"both pools", WorkloadGroupOptions{Pool: Ptr("default"), ExternalPool: Ptr("e")},
			"CREATE WORKLOAD GROUP [g] USING [default], EXTERNAL [e]"},
		{"every option", WorkloadGroupOptions{
			Importance: Ptr(WorkloadImportance("high")), RequestMaxMemoryGrantPercent: Ptr(12.5),
			RequestMaxCPUTimeSec: Ptr(30), RequestMemoryGrantTimeoutSec: Ptr(20), MaxDOP: Ptr(2),
			GroupMaxRequests: Ptr(7), GroupMaxTempdbDataPercent: Ptr(10.0), GroupMaxTempdbDataMB: Ptr(256.0),
			Pool: Ptr("p"),
		}, "CREATE WORKLOAD GROUP [g] WITH (IMPORTANCE = HIGH, REQUEST_MAX_MEMORY_GRANT_PERCENT = 12.5, " +
			"REQUEST_MAX_CPU_TIME_SEC = 30, REQUEST_MEMORY_GRANT_TIMEOUT_SEC = 20, MAX_DOP = 2, GROUP_MAX_REQUESTS = 7, " +
			"GROUP_MAX_TEMPDB_DATA_PERCENT = 10, GROUP_MAX_TEMPDB_DATA_MB = 256) USING [p]"},
		{"clear tempdb", WorkloadGroupOptions{ClearGroupMaxTempdbDataPercent: true, ClearGroupMaxTempdbDataMB: true},
			"CREATE WORKLOAD GROUP [g] WITH (GROUP_MAX_TEMPDB_DATA_PERCENT = NULL, GROUP_MAX_TEMPDB_DATA_MB = NULL)"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			wantOne(t, rgStatements(t, func(ctx context.Context) error {
				_, err := s.CreateWorkloadGroup(ctx, CreateWorkloadGroupRequest{Name: "g", Options: c.opts})
				return err
			}), c.want)
		})
	}

	g := s.WorkloadGroupRef("g")
	// A move with no WITH is valid (probed on 17).
	wantOne(t, rgStatements(t, func(ctx context.Context) error {
		return g.Alter(ctx, WorkloadGroupOptions{Pool: Ptr("p2")})
	}), "ALTER WORKLOAD GROUP [g] USING [p2]")
	wantOne(t, rgStatements(t, func(ctx context.Context) error {
		return g.Alter(ctx, WorkloadGroupOptions{Importance: Ptr(ImportanceLow)})
	}), "ALTER WORKLOAD GROUP [g] WITH (IMPORTANCE = LOW)")
	if got := rgStatements(t, func(ctx context.Context) error { return g.Alter(ctx, WorkloadGroupOptions{}) }); len(got) != 0 {
		t.Errorf("an empty Alter issued %q", got)
	}
	wantOne(t, rgStatements(t, g.Drop), "DROP WORKLOAD GROUP [g]")
}

func TestWorkloadGroupOptionsRefusals(t *testing.T) {
	cases := []struct {
		name    string
		major   int
		opts    WorkloadGroupOptions
		version bool // want ErrUnsupportedVersion
	}{
		// 13.0.6500.1: REQUEST_MAX_MEMORY_GRANT_PERCENT = 12.5 is Msg 102.
		{"fractional grant on 2016", 13, WorkloadGroupOptions{RequestMaxMemoryGrantPercent: Ptr(12.5)}, true},
		{"tempdb on 2022", 16, WorkloadGroupOptions{GroupMaxTempdbDataMB: Ptr(1.0)}, true},
		{"tempdb clear on 2019", 15, WorkloadGroupOptions{ClearGroupMaxTempdbDataPercent: true}, true},
		{"set and clear", 17, WorkloadGroupOptions{GroupMaxTempdbDataMB: Ptr(1.0), ClearGroupMaxTempdbDataMB: true}, false},
		{"bad importance", 17, WorkloadGroupOptions{Importance: Ptr(WorkloadImportance("urgent"))}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := &Server{info: &ServerInfo{VersionMajor: c.major}}
			ctx, col := WithScript(context.Background())
			_, err := s.CreateWorkloadGroup(ctx, CreateWorkloadGroupRequest{Name: "g", Options: c.opts})
			if err == nil {
				t.Fatal("accepted")
			}
			if errors.Is(err, ErrUnsupportedVersion) != c.version {
				t.Errorf("err = %v; ErrUnsupportedVersion = %v, want %v", err, !c.version, c.version)
			}
			if len(col.Statements()) != 0 {
				t.Errorf("a statement was built anyway: %v", col.Statements())
			}
		})
	}
	// Whole numbers stay legal on the old majors.
	s := &Server{info: &ServerInfo{VersionMajor: 13}}
	wantOne(t, rgStatements(t, func(ctx context.Context) error {
		_, err := s.CreateWorkloadGroup(ctx, CreateWorkloadGroupRequest{Name: "g",
			Options: WorkloadGroupOptions{RequestMaxMemoryGrantPercent: Ptr(30.0)}})
		return err
	}), "CREATE WORKLOAD GROUP [g] WITH (REQUEST_MAX_MEMORY_GRANT_PERCENT = 30)")
}

func TestExternalResourcePoolStatementShapes(t *testing.T) {
	s := &Server{}
	wantOne(t, rgStatements(t, func(ctx context.Context) error {
		_, err := s.CreateExternalResourcePool(ctx, CreateExternalResourcePoolRequest{Name: "e",
			Options: ExternalResourcePoolOptions{MaxCPUPercent: Ptr(40), MaxMemoryPercent: Ptr(30), MaxProcesses: Ptr(5)}})
		return err
	}), "CREATE EXTERNAL RESOURCE POOL [e] WITH (MAX_CPU_PERCENT = 40, MAX_MEMORY_PERCENT = 30, MAX_PROCESSES = 5)")
	e := s.ExternalResourcePoolRef("e")
	wantOne(t, rgStatements(t, func(ctx context.Context) error {
		return e.Alter(ctx, ExternalResourcePoolOptions{MaxProcesses: Ptr(0)})
	}), "ALTER EXTERNAL RESOURCE POOL [e] WITH (MAX_PROCESSES = 0)")
	wantOne(t, rgStatements(t, e.Drop), "DROP EXTERNAL RESOURCE POOL [e]")
}

// -- Scripter ---------------------------------------------------------------------

func TestResourceGovernorScript(t *testing.T) {
	got, err := buildResourceGovernorScript(&ResourceGovernor{
		IsEnabled: true, ClassifierSchema: "dbo", ClassifierName: "clf", MaxOutstandingIOPerVolume: 60,
	}, ScriptOptions{})
	if err != nil {
		t.Fatal(err)
	}
	want := "ALTER RESOURCE GOVERNOR WITH (CLASSIFIER_FUNCTION = [dbo].[clf]);\nGO\n" +
		"ALTER RESOURCE GOVERNOR WITH (MAX_OUTSTANDING_IO_PER_VOLUME = 60);\nGO\n" +
		"ALTER RESOURCE GOVERNOR RECONFIGURE;\nGO\n"
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
	// Disabled: DISABLE, never RECONFIGURE, which would enable it.
	got, _ = buildResourceGovernorScript(&ResourceGovernor{}, ScriptOptions{})
	if got != "ALTER RESOURCE GOVERNOR DISABLE;\nGO\n" {
		t.Errorf("disabled governor scripted as:\n%s", got)
	}
	if _, err := buildResourceGovernorScript(&ResourceGovernor{}, ScriptOptions{Verb: ScriptDrop}); !errors.Is(err, ErrUnsupported) {
		t.Errorf("DROP of the configuration = %v, want ErrUnsupported", err)
	}
}

func TestResourcePoolScript(t *testing.T) {
	p := &ResourcePool{ID: 256, Name: "p", MinCPUPercent: 5, MaxCPUPercent: 100, CapCPUPercent: 70,
		MaxMemoryPercent: 100, Affinity: []ResourcePoolAffinity{{0, 0b1011_0111}}}
	got, err := buildResourcePoolScript(p, ScriptOptions{Verb: ScriptDropAndCreate, IncludeIfNotExists: true})
	if err != nil {
		t.Fatal(err)
	}
	want := "IF EXISTS (SELECT 1 FROM sys.resource_governor_resource_pools WHERE name = N'p')\n    DROP RESOURCE POOL [p];\nGO\n\n" +
		"IF NOT EXISTS (SELECT 1 FROM sys.resource_governor_resource_pools WHERE name = N'p')\n" +
		"CREATE RESOURCE POOL [p] WITH (MIN_CPU_PERCENT = 5, CAP_CPU_PERCENT = 70, AFFINITY SCHEDULER = (0 TO 2, 4 TO 5, 7));\nGO\n" +
		rgReconfigureNote
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
	if strings.Contains(got, "RECONFIGURE;") {
		t.Error("a per-object script reconfigures, which enables a disabled governor")
	}

	p.Affinity = []ResourcePoolAffinity{{1, 1}}
	if _, err := buildResourcePoolScript(p, ScriptOptions{}); !errors.Is(err, ErrUnsupported) {
		t.Errorf("affinity in processor group 1 = %v, want ErrUnsupported", err)
	}
	// Bit 63 is a scheduler too, not a sign.
	p.Affinity = []ResourcePoolAffinity{{0, -1 << 63}}
	if got, _ := buildResourcePoolScript(p, ScriptOptions{}); !strings.Contains(got, "AFFINITY SCHEDULER = (63)") {
		t.Errorf("mask with bit 63 scripted as:\n%s", got)
	}
}

func TestSystemResourceGovernorObjectsScriptAsAlter(t *testing.T) {
	def := &ResourcePool{ID: 2, Name: "default", MaxCPUPercent: 80, CapCPUPercent: 100, MaxMemoryPercent: 100}
	got, err := buildResourcePoolScript(def, ScriptOptions{})
	if err != nil || got != "ALTER RESOURCE POOL [default] WITH (MAX_CPU_PERCENT = 80);\nGO\n" {
		t.Errorf("default pool scripted as %q, %v", got, err)
	}
	internal := &ResourcePool{ID: 1, Name: "internal", MaxCPUPercent: 100, CapCPUPercent: 100, MaxMemoryPercent: 100}
	if got, _ := buildResourcePoolScript(internal, ScriptOptions{}); !strings.HasPrefix(got, "-- ") {
		t.Errorf("internal pool scripted as %q, want only a comment", got)
	}
	if _, err := buildResourcePoolScript(def, ScriptOptions{Verb: ScriptDrop}); !errors.Is(err, ErrUnsupported) {
		t.Errorf("DROP of default = %v, want ErrUnsupported", err)
	}
	g := &WorkloadGroup{ID: 2, Name: "default", PoolName: "default", ExternalPoolName: "default",
		Importance: "Medium", RequestMaxMemoryGrantPercent: 25}
	if got, _ := buildWorkloadGroupScript(g, ScriptOptions{}); !strings.HasPrefix(got, "-- ") {
		t.Errorf("unchanged default group scripted as %q", got)
	}
	ext := &ExternalResourcePool{ID: 2, Name: "default", MaxCPUPercent: 100, MaxMemoryPercent: 20}
	if got, _ := buildExternalResourcePoolScript(ext, ScriptOptions{}); !strings.HasPrefix(got, "-- ") {
		t.Errorf("unchanged default external pool scripted as %q", got)
	}
}

func TestWorkloadGroupScript(t *testing.T) {
	g := &WorkloadGroup{ID: 256, Name: "g", PoolName: "default", ExternalPoolName: "e",
		Importance: "High", RequestMaxMemoryGrantPercent: 12.5, MaxDOP: 2,
		GroupMaxTempdbDataMB: Ptr(256.0)}
	got, err := buildWorkloadGroupScript(g, ScriptOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// USING EXTERNAL alone is not the grammar, so default is named.
	want := "CREATE WORKLOAD GROUP [g] WITH (IMPORTANCE = HIGH, REQUEST_MAX_MEMORY_GRANT_PERCENT = 12.5, MAX_DOP = 2, " +
		"GROUP_MAX_TEMPDB_DATA_MB = 256) USING [default], EXTERNAL [e];\nGO\n" + rgReconfigureNote
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
	got, _ = buildWorkloadGroupScript(g, ScriptOptions{Verb: ScriptDrop})
	if got != "IF EXISTS (SELECT 1 FROM sys.resource_governor_workload_groups WHERE name = N'g')\n    DROP WORKLOAD GROUP [g];\nGO\n" {
		t.Errorf("drop scripted as:\n%s", got)
	}
}
