package gosmo

import (
	"context"
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
// free-text fields is validated before SetQueryStoreOptionsContext ever
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
		{"DesiredState", func() QueryStoreOptions { o := base; o.DesiredState = inject; return o }()},
		{"CaptureMode", func() QueryStoreOptions { o := base; o.CaptureMode = inject; return o }()},
		{"SizeCleanupMode", func() QueryStoreOptions { o := base; o.SizeCleanupMode = inject; return o }()},
		{"WaitStatsCaptureMode", func() QueryStoreOptions { o := base; o.WaitStatsCaptureMode = inject; return o }()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := &Database{name: "appdb", server: &Server{}}
			if err := d.SetQueryStoreOptions(tt.opts); err == nil {
				t.Errorf("SetQueryStoreOptions accepted an injection payload in %s, want an error", tt.name)
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
	d := &Database{server: &Server{}, name: "AppDB"}
	ctx, script := WithScript(context.Background())

	if err := d.SetQueryStoreOptionsContext(ctx, QueryStoreOptions{
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
		t.Fatalf("SetQueryStoreOptionsContext under WithScript: %v", err)
	}
	if len(script.Statements) != 1 {
		t.Fatalf("Statements = %d, want 1", len(script.Statements))
	}
	got := script.Statements[0]
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
	d := &Database{server: &Server{}, name: "AppDB"}
	ctx, script := WithScript(context.Background())

	if err := d.SetQueryStoreOptionsContext(ctx, QueryStoreOptions{
		DesiredState: "OFF",
		MaxStorageMB: 512, // ignored
	}); err != nil {
		t.Fatalf("SetQueryStoreOptionsContext under WithScript: %v", err)
	}
	want := "ALTER DATABASE [AppDB] SET QUERY_STORE = OFF"
	if len(script.Statements) != 1 || script.Statements[0] != want {
		t.Errorf("Statements = %q, want [%q]", script.Statements, want)
	}
}
