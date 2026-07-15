package gosmo

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// ============================================================
// Query Store (sys.database_query_store_options — SSMS's Database
// Properties > Query Store page)
// ============================================================

// QueryStoreInfo mirrors the single row of sys.database_query_store_options
// every database has, whether or not Query Store is actually turned on.
type QueryStoreInfo struct {
	DesiredState              string // "OFF", "READ_ONLY", "READ_WRITE"
	ActualState               string
	ReadOnlyReason            int
	CurrentStorageMB          int64
	MaxStorageMB              int64
	FlushIntervalSec          int
	IntervalMinutes           int
	MaxPlansPerQuery          int
	CaptureMode               string // "NONE", "AUTO", "ALL", "CUSTOM"
	SizeCleanupMode           string // "OFF", "AUTO"
	StaleThresholdDays        int
	WaitStatsCaptureMode      string // "OFF", "ON"
	CapturePolicyExecCount    int
	CapturePolicyCompileCPUMs int64
	CapturePolicyExecCPUMs    int64
	CapturePolicyStaleHours   int
}

// QueryStore returns the database's Query Store configuration and state.
func (d *Database) QueryStore() (*QueryStoreInfo, error) {
	return d.QueryStoreContext(context.Background())
}

// QueryStoreContext is the context-aware variant of QueryStore.
func (d *Database) QueryStoreContext(ctx context.Context) (*QueryStoreInfo, error) {
	const q = `
SELECT desired_state_desc, actual_state_desc, readonly_reason,
       current_storage_size_mb, max_storage_size_mb,
       flush_interval_seconds, interval_length_minutes, max_plans_per_query,
       query_capture_mode_desc, size_based_cleanup_mode_desc,
       stale_query_threshold_days, wait_stats_capture_mode_desc,
       capture_policy_execution_count, capture_policy_total_compile_cpu_time_ms,
       capture_policy_total_execution_cpu_time_ms, capture_policy_stale_threshold_hours
FROM   sys.database_query_store_options`

	row, release, err := d.queryRow(ctx, q)
	if err != nil {
		return nil, err
	}
	defer release()

	// The four capture_policy_* columns are NULL whenever query capture
	// mode isn't CUSTOM (the common case) — verified live, not assumed.
	var execCount, staleHours sql.NullInt64
	var compileCPU, execCPU sql.NullInt64

	info := &QueryStoreInfo{}
	if err := row.Scan(
		&info.DesiredState, &info.ActualState, &info.ReadOnlyReason,
		&info.CurrentStorageMB, &info.MaxStorageMB,
		&info.FlushIntervalSec, &info.IntervalMinutes, &info.MaxPlansPerQuery,
		&info.CaptureMode, &info.SizeCleanupMode,
		&info.StaleThresholdDays, &info.WaitStatsCaptureMode,
		&execCount, &compileCPU, &execCPU, &staleHours,
	); err != nil {
		return nil, fmt.Errorf("gosmo: query store options for %q: %w", d.name, err)
	}
	info.CapturePolicyExecCount = int(execCount.Int64)
	info.CapturePolicyCompileCPUMs = compileCPU.Int64
	info.CapturePolicyExecCPUMs = execCPU.Int64
	info.CapturePolicyStaleHours = int(staleHours.Int64)
	return info, nil
}

// QueryStoreOptions holds the settings SetQueryStoreOptions writes via
// ALTER DATABASE ... SET QUERY_STORE = ON (...). DesiredState of "OFF"
// turns Query Store off and ignores every other field.
type QueryStoreOptions struct {
	DesiredState         string // "OFF", "READ_ONLY", "READ_WRITE"
	MaxStorageMB         int64
	CaptureMode          string // "NONE", "AUTO", "ALL", "CUSTOM"
	SizeCleanupMode      string // "OFF", "AUTO"
	StaleThresholdDays   int
	FlushIntervalSec     int
	IntervalMinutes      int
	MaxPlansPerQuery     int
	WaitStatsCaptureMode string // "OFF", "ON"
	// Custom capture policy thresholds, used only when CaptureMode is
	// "CUSTOM".
	CapturePolicyExecCount    int
	CapturePolicyCompileCPUMs int64
	CapturePolicyExecCPUMs    int64
	CapturePolicyStaleHours   int
}

// SetQueryStoreOptions turns Query Store on (reconfiguring it) or off.
func (d *Database) SetQueryStoreOptions(opts QueryStoreOptions) error {
	return d.SetQueryStoreOptionsContext(context.Background(), opts)
}

// SetQueryStoreOptionsContext is the context-aware variant of
// SetQueryStoreOptions. Like SetRecoveryModelContext, this is an ALTER
// DATABASE statement naming the database explicitly, so it runs through
// d.server.execContext rather than d.exec.
func (d *Database) SetQueryStoreOptionsContext(ctx context.Context, opts QueryStoreOptions) error {
	if opts.DesiredState == "OFF" {
		if err := d.server.execContext(ctx,
			fmt.Sprintf("ALTER DATABASE %s SET QUERY_STORE = OFF", quoteIdent(d.name)),
		); err != nil {
			return fmt.Errorf("gosmo: disable query store on %q: %w", d.name, err)
		}
		return nil
	}

	withs := []string{
		"OPERATION_MODE = " + opts.DesiredState,
		fmt.Sprintf("MAX_STORAGE_SIZE_MB = %d", opts.MaxStorageMB),
		fmt.Sprintf("DATA_FLUSH_INTERVAL_SECONDS = %d", opts.FlushIntervalSec),
		fmt.Sprintf("INTERVAL_LENGTH_MINUTES = %d", opts.IntervalMinutes),
		fmt.Sprintf("MAX_PLANS_PER_QUERY = %d", opts.MaxPlansPerQuery),
		"SIZE_BASED_CLEANUP_MODE = " + opts.SizeCleanupMode,
		fmt.Sprintf("QUERY_CAPTURE_MODE = %s", opts.CaptureMode),
		fmt.Sprintf("STALE_QUERY_THRESHOLD_DAYS = %d", opts.StaleThresholdDays),
		"WAIT_STATS_CAPTURE_MODE = " + opts.WaitStatsCaptureMode,
	}
	if opts.CaptureMode == "CUSTOM" {
		withs = append(withs, fmt.Sprintf(
			"QUERY_CAPTURE_POLICY = (EXECUTION_COUNT = %d, TOTAL_COMPILE_CPU_TIME_MS = %d, TOTAL_EXECUTION_CPU_TIME_MS = %d, STALE_CAPTURE_POLICY_THRESHOLD = %d HOURS)",
			opts.CapturePolicyExecCount, opts.CapturePolicyCompileCPUMs, opts.CapturePolicyExecCPUMs, opts.CapturePolicyStaleHours,
		))
	}

	q := fmt.Sprintf("ALTER DATABASE %s SET QUERY_STORE = ON (%s)", quoteIdent(d.name), strings.Join(withs, ", "))
	if err := d.server.execContext(ctx, q); err != nil {
		return fmt.Errorf("gosmo: set query store options on %q: %w", d.name, err)
	}
	return nil
}

// FlushQueryStore forces Query Store to persist its in-memory data to disk
// immediately (SSMS's "Flush Data" action), via sys.sp_query_store_flush_db.
func (d *Database) FlushQueryStore() error {
	return d.FlushQueryStoreContext(context.Background())
}

// FlushQueryStoreContext is the context-aware variant of FlushQueryStore.
func (d *Database) FlushQueryStoreContext(ctx context.Context) error {
	if _, err := d.exec(ctx, "EXEC sys.sp_query_store_flush_db"); err != nil {
		return fmt.Errorf("gosmo: flush query store on %q: %w", d.name, err)
	}
	return nil
}

// ClearQueryStore discards all captured Query Store data (SSMS's "Clear
// Query Store" action) without changing its configuration.
func (d *Database) ClearQueryStore() error {
	return d.ClearQueryStoreContext(context.Background())
}

// ClearQueryStoreContext is the context-aware variant of ClearQueryStore.
func (d *Database) ClearQueryStoreContext(ctx context.Context) error {
	if err := d.server.execContext(ctx,
		fmt.Sprintf("ALTER DATABASE %s SET QUERY_STORE CLEAR", quoteIdent(d.name)),
	); err != nil {
		return fmt.Errorf("gosmo: clear query store on %q: %w", d.name, err)
	}
	return nil
}
