package gosmo

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
	"testing"
)

func TestQueryStoreOperationModeAllowlist(t *testing.T) {
	if !queryStoreOperationModes["READ_WRITE"] {
		t.Error("READ_WRITE should be a recognized query store operation mode")
	}
	if queryStoreOperationModes["READ_WRITE); DROP TABLE dbo.Secrets; --"] {
		t.Error("an injection attempt was accepted as a valid operation mode")
	}
}

func TestQueryStoreCaptureModeAllowlist(t *testing.T) {
	if !queryStoreCaptureModes["CUSTOM"] {
		t.Error("CUSTOM should be a recognized query store capture mode")
	}
	if queryStoreCaptureModes["CUSTOM); DROP TABLE dbo.Secrets; --"] {
		t.Error("an injection attempt was accepted as a valid capture mode")
	}
}

func TestQueryStoreCleanupModeAllowlist(t *testing.T) {
	if !queryStoreCleanupModes["AUTO"] {
		t.Error("AUTO should be a recognized query store cleanup mode")
	}
	if queryStoreCleanupModes["AUTO); DROP TABLE dbo.Secrets; --"] {
		t.Error("an injection attempt was accepted as a valid cleanup mode")
	}
}

func TestQueryStoreWaitStatsModeAllowlist(t *testing.T) {
	if !queryStoreWaitStatsModes["ON"] {
		t.Error("ON should be a recognized query store wait-stats capture mode")
	}
	if queryStoreWaitStatsModes["ON); DROP TABLE dbo.Secrets; --"] {
		t.Error("an injection attempt was accepted as a valid wait-stats capture mode")
	}
}

// TestSetQueryStoreOptionsRejectsUnknownValues confirms each of the four
// free-text fields is validated before QueryStore.SetOptions ever
// builds the ALTER DATABASE statement — an injection payload in any one of
// them must be rejected end-to-end, not just against the allowlist map in
// isolation above.
func TestSetQueryStoreOptionsRejectsUnknownValues(t *testing.T) {
	base := QueryStoreOptions{
		DesiredState:         "READ_WRITE",
		CaptureMode:          "AUTO",
		SizeCleanupMode:      "AUTO",
		WaitStatsCaptureMode: "ON",
	}
	inject := "AUTO); DROP TABLE dbo.Secrets; --"

	tests := []struct {
		name string
		opts QueryStoreOptions
	}{
		{"DesiredState", func() QueryStoreOptions { o := base; o.DesiredState = QueryStoreState(inject); return o }()},
		{"CaptureMode", func() QueryStoreOptions { o := base; o.CaptureMode = QueryStoreCaptureMode(inject); return o }()},
		{"SizeCleanupMode", func() QueryStoreOptions { o := base; o.SizeCleanupMode = QueryStoreCleanupMode(inject); return o }()},
		{"WaitStatsCaptureMode", func() QueryStoreOptions {
			o := base
			o.WaitStatsCaptureMode = QueryStoreWaitStatsMode(inject)
			return o
		}()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := &Database{Name: "appdb", server: &Server{}}
			if err := d.QueryStore().SetOptions(t.Context(), tt.opts); err == nil {
				t.Errorf("QueryStore.SetOptions accepted an injection payload in %s, want an error", tt.name)
			}
		})
	}
}

// TestSetQueryStoreOptionsAcceptsLegitimateValues confirms the allowlists
// don't reject the actual values gossms's own Query Store page offers
// (see internal/tui/database_props_query_store.go's stateItems/
// captureItems/cleanupItems/onOff) — the point of an allowlist is to
// reject only genuinely unrecognized input, not any of these.
func TestSetQueryStoreOptionsAcceptsLegitimateValues(t *testing.T) {
	for _, opts := range []QueryStoreOptions{
		{DesiredState: "READ_ONLY", CaptureMode: "NONE", SizeCleanupMode: "OFF", WaitStatsCaptureMode: "OFF"},
		{DesiredState: "READ_WRITE", CaptureMode: "ALL", SizeCleanupMode: "AUTO", WaitStatsCaptureMode: "ON"},
		{DesiredState: "READ_WRITE", CaptureMode: "CUSTOM", SizeCleanupMode: "AUTO", WaitStatsCaptureMode: "ON"},
	} {
		if !queryStoreOperationModes[opts.DesiredState] {
			t.Errorf("DesiredState %q rejected by allowlist, want accepted", opts.DesiredState)
		}
		if !queryStoreCaptureModes[opts.CaptureMode] {
			t.Errorf("CaptureMode %q rejected by allowlist, want accepted", opts.CaptureMode)
		}
		if !queryStoreCleanupModes[opts.SizeCleanupMode] {
			t.Errorf("SizeCleanupMode %q rejected by allowlist, want accepted", opts.SizeCleanupMode)
		}
		if !queryStoreWaitStatsModes[opts.WaitStatsCaptureMode] {
			t.Errorf("WaitStatsCaptureMode %q rejected by allowlist, want accepted", opts.WaitStatsCaptureMode)
		}
	}
}

// TestSetQueryStoreOptionsStatement pins the generated ALTER DATABASE. The
// allowlist tests above pass on a statement SQL Server won't parse: shipped
// with STALE_QUERY_THRESHOLD_DAYS at the top level, every non-OFF call failed
// with "Incorrect syntax near 'STALE_QUERY_THRESHOLD_DAYS'". It is only legal
// inside CLEANUP_POLICY.
func TestSetQueryStoreOptionsStatement(t *testing.T) {
	d := &Database{server: &Server{}, Name: "AppDB"}
	ctx, script := WithScript(context.Background())

	if err := d.QueryStore().SetOptions(ctx, QueryStoreOptions{
		DesiredState:         "READ_WRITE",
		MaxStorageMB:         256,
		CaptureMode:          "AUTO",
		SizeCleanupMode:      "AUTO",
		StaleThresholdDays:   7,
		FlushIntervalSec:     900,
		IntervalMinutes:      15,
		MaxPlansPerQuery:     200,
		WaitStatsCaptureMode: "ON",
	}); err != nil {
		t.Fatalf("QueryStore.SetOptions under WithScript: %v", err)
	}
	if len(script.Statements()) != 1 {
		t.Fatalf("Statements = %d, want 1", len(script.Statements()))
	}
	got := script.Statements()[0]
	for _, want := range []string{
		"ALTER DATABASE [AppDB] SET QUERY_STORE = ON (",
		"OPERATION_MODE = READ_WRITE",
		"MAX_STORAGE_SIZE_MB = 256",
		"CLEANUP_POLICY = (STALE_QUERY_THRESHOLD_DAYS = 7)",
		"QUERY_CAPTURE_MODE = AUTO",
		"WAIT_STATS_CAPTURE_MODE = ON",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("statement missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, ", STALE_QUERY_THRESHOLD_DAYS") {
		t.Errorf("STALE_QUERY_THRESHOLD_DAYS emitted as a top-level option:\n%s", got)
	}
}

// TestSetQueryStoreOptionsOffIgnoresTheRest pins that "OFF" turns Query Store
// off with no option list, as QueryStoreOptions documents.
func TestSetQueryStoreOptionsOffIgnoresTheRest(t *testing.T) {
	d := &Database{server: &Server{}, Name: "AppDB"}
	ctx, script := WithScript(context.Background())

	if err := d.QueryStore().SetOptions(ctx, QueryStoreOptions{
		DesiredState: "OFF",
		MaxStorageMB: 512, // ignored
	}); err != nil {
		t.Fatalf("QueryStore.SetOptions under WithScript: %v", err)
	}
	want := "ALTER DATABASE [AppDB] SET QUERY_STORE = OFF"
	if len(script.Statements()) != 1 || script.Statements()[0] != want {
		t.Errorf("Statements = %q, want [%q]", script.Statements(), want)
	}
}

// A7: WAIT_STATS_CAPTURE_MODE is SQL Server 2017 and later. Sent to 2016 it
// fails the whole statement, so every Apply on the Query Store properties page
// failed there — and the read has no column to fill the value from either, so
// what would be sent is whatever the caller's zero value maps to.
func TestSetQueryStoreOptionsOmitsWaitStatsBefore2017(t *testing.T) {
	opts := QueryStoreOptions{
		DesiredState:     "READ_WRITE",
		MaxStorageMB:     256,
		CaptureMode:      "AUTO",
		SizeCleanupMode:  "AUTO",
		FlushIntervalSec: 900,
		IntervalMinutes:  60,
	}

	t.Run("2016 omits the clause and does not validate it", func(t *testing.T) {
		d := &Database{server: &Server{info: &ServerInfo{VersionMajor: 13}}, Name: "AppDB"}
		ctx, script := WithScript(context.Background())

		// The 2016 read returns "" for a column that does not exist; that must
		// not be rejected as an unrecognized mode.
		if err := d.QueryStore().SetOptions(ctx, opts); err != nil {
			t.Fatalf("QueryStore.SetOptions: %v", err)
		}
		if len(script.Statements()) != 1 {
			t.Fatalf("Statements = %d, want 1", len(script.Statements()))
		}
		if strings.Contains(script.Statements()[0], "WAIT_STATS_CAPTURE_MODE") {
			t.Errorf("2016 statement names WAIT_STATS_CAPTURE_MODE:\n%s", script.Statements()[0])
		}
	})

	for _, major := range []int{14, 15, 16, 17, 0} {
		t.Run("major "+strconv.Itoa(major)+" still emits it", func(t *testing.T) {
			d := &Database{server: &Server{info: &ServerInfo{VersionMajor: major}}, Name: "AppDB"}
			ctx, script := WithScript(context.Background())

			o := opts
			o.WaitStatsCaptureMode = "ON"
			if err := d.QueryStore().SetOptions(ctx, o); err != nil {
				t.Fatalf("QueryStore.SetOptions: %v", err)
			}
			if !strings.Contains(script.Statements()[0], "WAIT_STATS_CAPTURE_MODE = ON") {
				t.Errorf("major %d dropped WAIT_STATS_CAPTURE_MODE:\n%s", major, script.Statements()[0])
			}
		})
	}

	// The allowlist still guards the versions that have the setting — dropping
	// the check along with the clause would let anything through there too.
	t.Run("2017 still rejects an unrecognized mode", func(t *testing.T) {
		d := &Database{server: &Server{info: &ServerInfo{VersionMajor: 14}}, Name: "AppDB"}
		ctx, _ := WithScript(context.Background())

		o := opts
		o.WaitStatsCaptureMode = "ON) --"
		if err := d.QueryStore().SetOptions(ctx, o); err == nil {
			t.Error("2017 accepted an unrecognized wait stats capture mode, want an error")
		}
	})
}

// M6 (review plan 2026-10-13): the numeric options the server refuses outside
// their bounds — probed live on 17.0 — are refused before anything is sent.
// The boundary values themselves are accepted.
func TestSetQueryStoreOptionsRefusesOutOfRangeValues(t *testing.T) {
	base := QueryStoreOptions{
		DesiredState: "READ_WRITE", CaptureMode: "CUSTOM", SizeCleanupMode: "AUTO", WaitStatsCaptureMode: "ON",
		FlushIntervalSec: 60, IntervalMinutes: 1,
		CapturePolicyExecCount: 1, CapturePolicyCompileCPUMs: 1, CapturePolicyExecCPUMs: 1, CapturePolicyStaleHours: 1,
	}
	d := &Database{server: &Server{}, Name: "AppDB"}
	set := func(o QueryStoreOptions) ([]string, error) {
		ctx, script := WithScript(context.Background())
		err := d.QueryStore().SetOptions(ctx, o)
		return script.Statements(), err
	}
	for _, edit := range []func(*QueryStoreOptions){
		func(o *QueryStoreOptions) {},
		func(o *QueryStoreOptions) { o.IntervalMinutes = 1440; o.CapturePolicyStaleHours = 168 },
		func(o *QueryStoreOptions) {
			o.CapturePolicyCompileCPUMs, o.CapturePolicyExecCPUMs = 2147483647, 2147483647
		},
		// The capture policy is checked only when it is sent.
		func(o *QueryStoreOptions) {
			o.CaptureMode = "AUTO"
			o.CapturePolicyExecCount, o.CapturePolicyStaleHours = 0, 0
		},
	} {
		o := base
		edit(&o)
		if stmts, err := set(o); err != nil || len(stmts) != 1 {
			t.Errorf("%+v: err = %v, statements %q; want it accepted", o, err, stmts)
		}
	}
	for name, edit := range map[string]func(*QueryStoreOptions){
		"interval 7 (153)":           func(o *QueryStoreOptions) { o.IntervalMinutes = 7 },
		"interval 0":                 func(o *QueryStoreOptions) { o.IntervalMinutes = 0 },
		"flush 59 (153)":             func(o *QueryStoreOptions) { o.FlushIntervalSec = 59 },
		"execution count 0 (12452)":  func(o *QueryStoreOptions) { o.CapturePolicyExecCount = 0 },
		"compile CPU 0 (12452)":      func(o *QueryStoreOptions) { o.CapturePolicyCompileCPUMs = 0 },
		"execution CPU 0 (12452)":    func(o *QueryStoreOptions) { o.CapturePolicyExecCPUMs = 0 },
		"compile CPU above int32":    func(o *QueryStoreOptions) { o.CapturePolicyCompileCPUMs = 2147483648 },
		"stale 0 hours (12453)":      func(o *QueryStoreOptions) { o.CapturePolicyStaleHours = 0 },
		"stale 169 hours (12453)":    func(o *QueryStoreOptions) { o.CapturePolicyStaleHours = 169 },
		"stale 500 hours (as shown)": func(o *QueryStoreOptions) { o.CapturePolicyStaleHours = 500 },
	} {
		o := base
		edit(&o)
		stmts, err := set(o)
		if !errors.Is(err, ErrInvalidRequest) || len(stmts) != 0 {
			t.Errorf("%s: err = %v, statements %q; want ErrInvalidRequest and nothing sent", name, err, stmts)
		}
	}
	if got := QueryStoreIntervalMinutes(); !slices.Equal(got, []int{1, 5, 10, 15, 30, 60, 1440}) {
		t.Errorf("QueryStoreIntervalMinutes() = %v", got)
	}
}

// master and tempdb have no row in sys.database_query_store_options: not
// found, not a bare sql.ErrNoRows.
func TestQueryStoreOptionsWithNoRowIsNotFound(t *testing.T) {
	d := qsRecDB(t, 16, []string{"desired_state_desc"}, nil)
	_, err := d.QueryStore().Options(context.Background())
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}
