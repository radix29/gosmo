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
	DesiredState       string // "OFF", "READ_ONLY", "READ_WRITE"
	ActualState        string
	ReadOnlyReason     int
	CurrentStorageMB   int64
	MaxStorageMB       int64
	FlushIntervalSec   int
	IntervalMinutes    int
	MaxPlansPerQuery   int
	CaptureMode        string // "NONE", "AUTO", "ALL", "CUSTOM"
	SizeCleanupMode    string // "OFF", "AUTO"
	StaleThresholdDays int
	// WaitStatsCaptureMode is "OFF" or "ON", and empty on SQL Server 2016,
	// which has no such setting.
	WaitStatsCaptureMode string
	// The custom capture policy is SQL Server 2019 and later; these four are
	// zero on anything older, as they are on any instance whose capture mode
	// isn't CUSTOM.
	CapturePolicyExecCount    int
	CapturePolicyCompileCPUMs int64
	CapturePolicyExecCPUMs    int64
	CapturePolicyStaleHours   int
}

// Two sets of columns postdate the view, and naming one the instance
// lacks fails the whole read — Database Properties > Query Store then
// renders an error in place of the page. Dated from the column table of
// https://learn.microsoft.com/sql/relational-databases/system-catalog-views/sys-database-query-store-options-transact-sql:
// wait_stats_capture_mode_desc is "SQL Server 2017 (14.x) and later
// versions", the four capture_policy_* columns (the CUSTOM query capture
// policy) "SQL Server 2019 (15.x) and later versions".
func (d *Database) queryStoreOptionsSelect() string {
	major := d.serverMajorVersion()
	return `
SELECT desired_state_desc, actual_state_desc, readonly_reason,
       current_storage_size_mb, max_storage_size_mb,
       flush_interval_seconds, interval_length_minutes, max_plans_per_query,
       query_capture_mode_desc, size_based_cleanup_mode_desc,
       stale_query_threshold_days,
       ` + colSince(major, SQLServer2017, "wait_stats_capture_mode_desc", "CAST('' AS nvarchar(60))") + `,
       ` + colSince(major, SQLServer2019, "capture_policy_execution_count", "CAST(NULL AS int)") + `,
       ` + colSince(major, SQLServer2019, "capture_policy_total_compile_cpu_time_ms", "CAST(NULL AS bigint)") + `,
       ` + colSince(major, SQLServer2019, "capture_policy_total_execution_cpu_time_ms", "CAST(NULL AS bigint)") + `,
       ` + colSince(major, SQLServer2019, "capture_policy_stale_threshold_hours", "CAST(NULL AS int)") + `
FROM   sys.database_query_store_options`
}

// QueryStore returns the database's Query Store configuration and state.
func (d *Database) QueryStore() (*QueryStoreInfo, error) {
	return d.QueryStoreContext(context.Background())
}

// QueryStoreContext is the context-aware variant of QueryStore.
func (d *Database) QueryStoreContext(ctx context.Context) (*QueryStoreInfo, error) {
	q := d.queryStoreOptionsSelect()

	// The four capture_policy_* columns are NULL whenever query capture
	// mode isn't CUSTOM, which is the common case.
	var execCount, staleHours sql.NullInt64
	var compileCPU, execCPU sql.NullInt64

	info := &QueryStoreInfo{}
	if err := d.queryRow(ctx, func(row *sql.Row) error {
		return row.Scan(
			&info.DesiredState, &info.ActualState, &info.ReadOnlyReason,
			&info.CurrentStorageMB, &info.MaxStorageMB,
			&info.FlushIntervalSec, &info.IntervalMinutes, &info.MaxPlansPerQuery,
			&info.CaptureMode, &info.SizeCleanupMode,
			&info.StaleThresholdDays, &info.WaitStatsCaptureMode,
			&execCount, &compileCPU, &execCPU, &staleHours,
		)
	}, q); err != nil {
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
	DesiredState       string // "OFF", "READ_ONLY", "READ_WRITE"
	MaxStorageMB       int64
	CaptureMode        string // "NONE", "AUTO", "ALL", "CUSTOM"
	SizeCleanupMode    string // "OFF", "AUTO"
	StaleThresholdDays int
	FlushIntervalSec   int
	IntervalMinutes    int
	MaxPlansPerQuery   int
	// WaitStatsCaptureMode is "OFF" or "ON". It is ignored on SQL Server 2016,
	// which has no such setting — the clause is left out of the statement
	// rather than sent and rejected.
	WaitStatsCaptureMode string
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

// queryStoreOperationModes allowlists the ALTER DATABASE SET QUERY_STORE
// OPERATION_MODE keywords SQL Server accepts (once already past the OFF
// case, handled separately below) — can't be identifier-quoted or
// parameterised (ALTER DATABASE is DDL).
var queryStoreOperationModes = map[string]bool{"READ_ONLY": true, "READ_WRITE": true}

// queryStoreCaptureModes allowlists the QUERY_CAPTURE_MODE keywords.
var queryStoreCaptureModes = map[string]bool{"NONE": true, "AUTO": true, "ALL": true, "CUSTOM": true}

// queryStoreCleanupModes allowlists the SIZE_BASED_CLEANUP_MODE keywords.
var queryStoreCleanupModes = map[string]bool{"OFF": true, "AUTO": true}

// queryStoreWaitStatsModes allowlists the WAIT_STATS_CAPTURE_MODE keywords.
var queryStoreWaitStatsModes = map[string]bool{"OFF": true, "ON": true}

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

	if !queryStoreOperationModes[opts.DesiredState] {
		return fmt.Errorf("gosmo: set query store options on %q: unrecognized operation mode %q", d.name, opts.DesiredState)
	}
	if !queryStoreCaptureModes[opts.CaptureMode] {
		return fmt.Errorf("gosmo: set query store options on %q: unrecognized capture mode %q", d.name, opts.CaptureMode)
	}
	if !queryStoreCleanupModes[opts.SizeCleanupMode] {
		return fmt.Errorf("gosmo: set query store options on %q: unrecognized size cleanup mode %q", d.name, opts.SizeCleanupMode)
	}
	// WAIT_STATS_CAPTURE_MODE is SQL Server 2017 and later. Below it the
	// setting does not exist — the read has no column to report and returns
	// "" — so the clause is omitted rather than sent and rejected.
	waitStats := d.serverMajorVersion() == 0 || d.serverMajorVersion() >= int(SQLServer2017)
	if waitStats && !queryStoreWaitStatsModes[opts.WaitStatsCaptureMode] {
		return fmt.Errorf("gosmo: set query store options on %q: unrecognized wait stats capture mode %q", d.name, opts.WaitStatsCaptureMode)
	}

	withs := []string{
		"OPERATION_MODE = " + opts.DesiredState,
		fmt.Sprintf("MAX_STORAGE_SIZE_MB = %d", opts.MaxStorageMB),
		fmt.Sprintf("DATA_FLUSH_INTERVAL_SECONDS = %d", opts.FlushIntervalSec),
		fmt.Sprintf("INTERVAL_LENGTH_MINUTES = %d", opts.IntervalMinutes),
		fmt.Sprintf("MAX_PLANS_PER_QUERY = %d", opts.MaxPlansPerQuery),
		"SIZE_BASED_CLEANUP_MODE = " + opts.SizeCleanupMode,
		fmt.Sprintf("QUERY_CAPTURE_MODE = %s", opts.CaptureMode),
		// STALE_QUERY_THRESHOLD_DAYS is not a top-level option: SET
		// QUERY_STORE only accepts it inside CLEANUP_POLICY, and rejects the
		// whole statement with a syntax error otherwise.
		fmt.Sprintf("CLEANUP_POLICY = (STALE_QUERY_THRESHOLD_DAYS = %d)", opts.StaleThresholdDays),
	}
	if waitStats {
		withs = append(withs, "WAIT_STATS_CAPTURE_MODE = "+opts.WaitStatsCaptureMode)
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
