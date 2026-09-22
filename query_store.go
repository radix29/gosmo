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

// QueryStoreState is Query Store's operation mode, spelled as ALTER DATABASE
// SET QUERY_STORE takes it and sys.database_query_store_options reports it.
type QueryStoreState string

const (
	QueryStoreOff       QueryStoreState = "OFF"
	QueryStoreReadOnly  QueryStoreState = "READ_ONLY"
	QueryStoreReadWrite QueryStoreState = "READ_WRITE"
	// QueryStoreError is reported as an ActualState only; it cannot be set.
	QueryStoreError QueryStoreState = "ERROR"
)

// QueryStoreCaptureMode is QUERY_CAPTURE_MODE.
type QueryStoreCaptureMode string

const (
	QueryStoreCaptureNone   QueryStoreCaptureMode = "NONE"
	QueryStoreCaptureAuto   QueryStoreCaptureMode = "AUTO"
	QueryStoreCaptureAll    QueryStoreCaptureMode = "ALL"
	QueryStoreCaptureCustom QueryStoreCaptureMode = "CUSTOM" // SQL Server 2019+
)

// QueryStoreCleanupMode is SIZE_BASED_CLEANUP_MODE.
type QueryStoreCleanupMode string

const (
	QueryStoreCleanupOff  QueryStoreCleanupMode = "OFF"
	QueryStoreCleanupAuto QueryStoreCleanupMode = "AUTO"
)

// QueryStoreWaitStatsMode is WAIT_STATS_CAPTURE_MODE (SQL Server 2017+).
type QueryStoreWaitStatsMode string

const (
	QueryStoreWaitStatsOff QueryStoreWaitStatsMode = "OFF"
	QueryStoreWaitStatsOn  QueryStoreWaitStatsMode = "ON"
)

// QueryStoreInfo mirrors the single row of sys.database_query_store_options
// every database has, whether or not Query Store is actually turned on.
type QueryStoreInfo struct {
	DesiredState       QueryStoreState
	ActualState        QueryStoreState
	ReadOnlyReason     int
	CurrentStorageMB   int64
	MaxStorageMB       int64
	FlushIntervalSec   int
	IntervalMinutes    int
	MaxPlansPerQuery   int
	CaptureMode        QueryStoreCaptureMode
	SizeCleanupMode    QueryStoreCleanupMode
	StaleThresholdDays int
	// WaitStatsCaptureMode is empty on SQL Server 2016, which has no such
	// setting.
	WaitStatsCaptureMode QueryStoreWaitStatsMode
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
func (d *Database) QueryStore(ctx context.Context) (*QueryStoreInfo, error) {
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
		return nil, fmt.Errorf("gosmo: query store options for %q: %w", d.Name, err)
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
	DesiredState       QueryStoreState
	MaxStorageMB       int64
	CaptureMode        QueryStoreCaptureMode
	SizeCleanupMode    QueryStoreCleanupMode
	StaleThresholdDays int
	FlushIntervalSec   int
	IntervalMinutes    int
	MaxPlansPerQuery   int
	// WaitStatsCaptureMode is ignored on SQL Server 2016, which has no such
	// setting — the clause is left out of the statement rather than sent and
	// rejected.
	WaitStatsCaptureMode QueryStoreWaitStatsMode
	// Custom capture policy thresholds, used only when CaptureMode is
	// "CUSTOM".
	CapturePolicyExecCount    int
	CapturePolicyCompileCPUMs int64
	CapturePolicyExecCPUMs    int64
	CapturePolicyStaleHours   int
}

// The validity checks for the Query Store keywords, which can't be
// identifier-quoted or parameterised (ALTER DATABASE is DDL): a value outside
// the constants — a conversion from an arbitrary string — is refused rather
// than spliced in. The operation modes are the settable ones, once already
// past the OFF case handled separately below.
var (
	queryStoreOperationModes = map[QueryStoreState]bool{QueryStoreReadOnly: true, QueryStoreReadWrite: true}
	queryStoreCaptureModes   = map[QueryStoreCaptureMode]bool{
		QueryStoreCaptureNone: true, QueryStoreCaptureAuto: true, QueryStoreCaptureAll: true, QueryStoreCaptureCustom: true,
	}
	queryStoreCleanupModes   = map[QueryStoreCleanupMode]bool{QueryStoreCleanupOff: true, QueryStoreCleanupAuto: true}
	queryStoreWaitStatsModes = map[QueryStoreWaitStatsMode]bool{QueryStoreWaitStatsOff: true, QueryStoreWaitStatsOn: true}
)

// SetQueryStoreOptions turns Query Store on (reconfiguring it) or off.
//
// Like SetRecoveryModel, this is an ALTER DATABASE statement naming the
// database explicitly, so it runs through d.server.exec rather than
// d.exec.
func (d *Database) SetQueryStoreOptions(ctx context.Context, opts QueryStoreOptions) error {
	if opts.DesiredState == QueryStoreOff {
		if err := d.server.exec(ctx,
			fmt.Sprintf("ALTER DATABASE %s SET QUERY_STORE = OFF", quoteIdent(d.Name)),
		); err != nil {
			return fmt.Errorf("gosmo: disable query store on %q: %w", d.Name, err)
		}
		return nil
	}

	if !queryStoreOperationModes[opts.DesiredState] {
		return fmt.Errorf("gosmo: set query store options on %q: unrecognized operation mode %q", d.Name, opts.DesiredState)
	}
	if !queryStoreCaptureModes[opts.CaptureMode] {
		return fmt.Errorf("gosmo: set query store options on %q: unrecognized capture mode %q", d.Name, opts.CaptureMode)
	}
	if !queryStoreCleanupModes[opts.SizeCleanupMode] {
		return fmt.Errorf("gosmo: set query store options on %q: unrecognized size cleanup mode %q", d.Name, opts.SizeCleanupMode)
	}
	// WAIT_STATS_CAPTURE_MODE is SQL Server 2017 and later. Below it the
	// setting does not exist — the read has no column to report and returns
	// "" — so the clause is omitted rather than sent and rejected.
	waitStats := d.serverMajorVersion() == 0 || d.serverMajorVersion() >= int(SQLServer2017)
	if waitStats && !queryStoreWaitStatsModes[opts.WaitStatsCaptureMode] {
		return fmt.Errorf("gosmo: set query store options on %q: unrecognized wait stats capture mode %q", d.Name, opts.WaitStatsCaptureMode)
	}

	withs := []string{
		"OPERATION_MODE = " + string(opts.DesiredState),
		fmt.Sprintf("MAX_STORAGE_SIZE_MB = %d", opts.MaxStorageMB),
		fmt.Sprintf("DATA_FLUSH_INTERVAL_SECONDS = %d", opts.FlushIntervalSec),
		fmt.Sprintf("INTERVAL_LENGTH_MINUTES = %d", opts.IntervalMinutes),
		fmt.Sprintf("MAX_PLANS_PER_QUERY = %d", opts.MaxPlansPerQuery),
		"SIZE_BASED_CLEANUP_MODE = " + string(opts.SizeCleanupMode),
		fmt.Sprintf("QUERY_CAPTURE_MODE = %s", opts.CaptureMode),
		// STALE_QUERY_THRESHOLD_DAYS is not a top-level option: SET
		// QUERY_STORE only accepts it inside CLEANUP_POLICY, and rejects the
		// whole statement with a syntax error otherwise.
		fmt.Sprintf("CLEANUP_POLICY = (STALE_QUERY_THRESHOLD_DAYS = %d)", opts.StaleThresholdDays),
	}
	if waitStats {
		withs = append(withs, "WAIT_STATS_CAPTURE_MODE = "+string(opts.WaitStatsCaptureMode))
	}
	if opts.CaptureMode == QueryStoreCaptureCustom {
		withs = append(withs, fmt.Sprintf(
			"QUERY_CAPTURE_POLICY = (EXECUTION_COUNT = %d, TOTAL_COMPILE_CPU_TIME_MS = %d, TOTAL_EXECUTION_CPU_TIME_MS = %d, STALE_CAPTURE_POLICY_THRESHOLD = %d HOURS)",
			opts.CapturePolicyExecCount, opts.CapturePolicyCompileCPUMs, opts.CapturePolicyExecCPUMs, opts.CapturePolicyStaleHours,
		))
	}

	q := fmt.Sprintf("ALTER DATABASE %s SET QUERY_STORE = ON (%s)", quoteIdent(d.Name), strings.Join(withs, ", "))
	if err := d.server.exec(ctx, q); err != nil {
		return fmt.Errorf("gosmo: set query store options on %q: %w", d.Name, err)
	}
	return nil
}

// FlushQueryStore forces Query Store to persist its in-memory data to disk
// immediately (SSMS's "Flush Data" action), via sys.sp_query_store_flush_db.
func (d *Database) FlushQueryStore(ctx context.Context) error {
	if _, err := d.exec(ctx, "EXEC sys.sp_query_store_flush_db"); err != nil {
		return fmt.Errorf("gosmo: flush query store on %q: %w", d.Name, err)
	}
	return nil
}

// ClearQueryStore discards all captured Query Store data (SSMS's "Clear
// Query Store" action) without changing its configuration.
func (d *Database) ClearQueryStore(ctx context.Context) error {
	if err := d.server.exec(ctx,
		fmt.Sprintf("ALTER DATABASE %s SET QUERY_STORE CLEAR", quoteIdent(d.Name)),
	); err != nil {
		return fmt.Errorf("gosmo: clear query store on %q: %w", d.Name, err)
	}
	return nil
}
