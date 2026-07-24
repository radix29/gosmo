package gosmo

import "testing"

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
