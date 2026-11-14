package gosmo

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ============================================================
// Database options  (sys.databases SET-option columns — SSMS's Database
// Properties > Options page)
// ============================================================

// DatabaseOptions holds the ALTER DATABASE SET options and related flags
// from sys.databases that aren't already covered by Database's own cached
// fields (RecoveryModel, CompatibilityLevel, Collation, IsReadOnly).
type DatabaseOptions struct {
	Owner             string
	PageVerify        string // e.g. "CHECKSUM", "TORN_PAGE_DETECTION", "NONE"
	UserAccess        UserAccess
	Containment       string // "NONE", "PARTIAL"
	DefaultCursor     string // "LOCAL" or "GLOBAL"
	SnapshotIsolation string // e.g. "OFF", "ON"

	AutoClose             bool
	AutoShrink            bool
	AutoCreateStats       bool
	AutoUpdateStats       bool
	AutoUpdateStatsAsync  bool
	ANSINullDefault       bool
	ANSINulls             bool
	ANSIPadding           bool
	ANSIWarnings          bool
	ArithAbort            bool
	ConcatNullYieldsNull  bool
	NumericRoundAbort     bool
	QuotedIdentifier      bool
	RecursiveTriggers     bool
	CursorCloseOnCommit   bool
	ReadCommittedSnapshot bool
	IsTrustworthy         bool
	IsBrokerEnabled       bool
	IsEncrypted           bool

	DBChaining             bool // DB_CHAINING (cross-database ownership chaining)
	ForcedParameterization bool // PARAMETERIZATION FORCED; false is SIMPLE
	DateCorrelation        bool // DATE_CORRELATION_OPTIMIZATION
	HonorBrokerPriority    bool // HONOR_BROKER_PRIORITY

	// DelayedDurability is "DISABLED", "ALLOWED" or "FORCED" ("" unknown —
	// see Options).
	DelayedDurability string
	// TargetRecoveryTimeSeconds is TARGET_RECOVERY_TIME; 0 is automatic
	// (non-indirect) checkpoints, -1 unknown (see Options).
	TargetRecoveryTimeSeconds int
	// AcceleratedDatabaseRecovery is ACCELERATED_DATABASE_RECOVERY, which
	// SQL Server 2019 introduced; false on an older instance.
	AcceleratedDatabaseRecovery bool
	// IsLedger is LEDGER = ON, which SQL Server 2022 introduced and only
	// CREATE DATABASE can set; false on an older instance.
	IsLedger bool

	// FILESTREAM's database-level options (sys.database_filestream_options):
	// NonTransactedAccess is "OFF", "READ_ONLY" or "FULL", and
	// DirectoryName is "" while none is set. Both are present whether or not
	// the database has a FILESTREAM filegroup.
	NonTransactedAccess string
	DirectoryName       string
}

// Options returns the database's ALTER DATABASE SET options.
//
// Queried against sys.databases at server scope (like
// Server.DatabaseByName), not through d.query — these are
// catalog-view columns, not per-database data, and the read works on a
// database that cannot be entered.
//
// A database AUTO_CLOSE has closed reports NULL for some columns until it
// is next opened: those read as unknown — DelayedDurability "",
// TargetRecoveryTimeSeconds -1, AcceleratedDatabaseRecovery and IsLedger
// false.
// ScriptDatabase reads them from inside the database, which opens it.
func (d *Database) Options(ctx context.Context) (*DatabaseOptions, error) {
	return d.readOptions(ctx, d.server.queryRow)
}

// readOptions runs optionsSelect through queryRow: Server.queryRow for the
// server-scope read, Database.queryRow for one from inside the database.
func (d *Database) readOptions(ctx context.Context, queryRow func(context.Context, func(*sql.Row) error, string, ...any) error) (*DatabaseOptions, error) {
	o := &DatabaseOptions{}
	var owner, delayed sql.NullString
	var isLocalCursor bool
	var adr, ledger sql.NullBool

	err := queryRow(ctx, func(row *sql.Row) error {
		return row.Scan(
			&owner, &o.PageVerify, &o.UserAccess, &o.Containment, &isLocalCursor, &o.SnapshotIsolation,
			&o.AutoClose, &o.AutoShrink, &o.AutoCreateStats,
			&o.AutoUpdateStats, &o.AutoUpdateStatsAsync,
			&o.ANSINullDefault, &o.ANSINulls, &o.ANSIPadding,
			&o.ANSIWarnings, &o.ArithAbort, &o.ConcatNullYieldsNull,
			&o.NumericRoundAbort, &o.QuotedIdentifier, &o.RecursiveTriggers,
			&o.CursorCloseOnCommit, &o.ReadCommittedSnapshot,
			&o.IsTrustworthy, &o.IsBrokerEnabled, &o.IsEncrypted,
			&o.NonTransactedAccess, &o.DirectoryName,
			&o.DBChaining, &o.ForcedParameterization, &o.DateCorrelation,
			&o.HonorBrokerPriority, &delayed,
			&o.TargetRecoveryTimeSeconds,
			&adr, &ledger,
		)
	}, d.optionsSelect(), d.Name)
	if err != nil {
		return nil, rowErr(err, notFoundf("gosmo: database %q not found", d.Name),
			fmt.Sprintf("database options for %q", d.Name))
	}
	o.Owner = owner.String
	o.DelayedDurability = delayed.String
	o.AcceleratedDatabaseRecovery = adr.Bool
	o.IsLedger = ledger.Bool
	if isLocalCursor {
		o.DefaultCursor = "LOCAL"
	} else {
		o.DefaultCursor = "GLOBAL"
	}
	return o, nil
}

// optionsSelect is Options' query for the connected server's version:
// ACCELERATED_DATABASE_RECOVERY is gated to 2019, LEDGER to 2022.
func (d *Database) optionsSelect() string {
	return `
SELECT SUSER_SNAME(d.owner_sid), d.page_verify_option_desc, d.user_access_desc,
       d.containment_desc, d.is_local_cursor_default, d.snapshot_isolation_state_desc,
       d.is_auto_close_on, d.is_auto_shrink_on, d.is_auto_create_stats_on,
       d.is_auto_update_stats_on, d.is_auto_update_stats_async_on,
       d.is_ansi_null_default_on, d.is_ansi_nulls_on, d.is_ansi_padding_on,
       d.is_ansi_warnings_on, d.is_arithabort_on, d.is_concat_null_yields_null_on,
       d.is_numeric_roundabort_on, d.is_quoted_identifier_on, d.is_recursive_triggers_on,
       d.is_cursor_close_on_commit_on, d.is_read_committed_snapshot_on,
       d.is_trustworthy_on, d.is_broker_enabled, d.is_encrypted,
       ISNULL(fo.non_transacted_access_desc, 'OFF'), ISNULL(fo.directory_name, ''),
       d.is_db_chaining_on, d.is_parameterization_forced, d.is_date_correlation_on,
       d.is_honor_broker_priority_on, d.delayed_durability_desc,
       ISNULL(d.target_recovery_time_in_seconds, -1),
       ` + bitSince(d.server.serverMajorVersion(), SQLServer2019, "d.is_accelerated_database_recovery_on") + `,
       ` + bitSince(d.server.serverMajorVersion(), SQLServer2022, "d.is_ledger_on") + `
FROM   sys.databases d
LEFT   JOIN sys.database_filestream_options fo ON fo.database_id = d.database_id
WHERE  d.name = @p1`
}

// PersistentVersionStoreFileGroup names the filegroup holding the
// database's ADR persistent version store — "PRIMARY" unless
// ACCELERATED_DATABASE_RECOVERY was turned on with
// PERSISTENT_VERSION_STORE_FILEGROUP — or "" when the server reports none,
// as an instance older than SQL Server 2019 never does.
//
// sys.databases has no such column: only
// sys.dm_tran_persistent_version_store_stats reports it, and it keeps the
// filegroup after ADR is turned off, though SET ACCELERATED_DATABASE_RECOVERY
// = ON without the clause then moves it back to PRIMARY (both seen on 2025).
// Read from inside the database, where its owner has the DMV's permission
// (VIEW DATABASE PERFORMANCE STATE from SQL Server 2022): from master it
// takes a server-level grant. A refusal is Msg 262/297, which
// IsPermissionDenied recognises.
func (d *Database) PersistentVersionStoreFileGroup(ctx context.Context) (string, error) {
	if d.before(SQLServer2019) {
		return "", nil
	}
	var name sql.NullString
	err := d.queryRow(ctx, func(row *sql.Row) error { return row.Scan(&name) }, `
SELECT fg.name
FROM   sys.dm_tran_persistent_version_store_stats s
JOIN   sys.filegroups fg ON fg.data_space_id = s.pvs_filegroup_id
WHERE  s.database_id = DB_ID()`)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("gosmo: persistent version store filegroup of %q: %w", d.Name, err)
	}
	return name.String, nil
}

// DatabaseOption identifies one ALTER DATABASE ... SET option.
type DatabaseOption string

const (
	DBOptAutoClose                 DatabaseOption = "AUTO_CLOSE"
	DBOptAutoShrink                DatabaseOption = "AUTO_SHRINK"
	DBOptAutoCreateStatistics      DatabaseOption = "AUTO_CREATE_STATISTICS"
	DBOptAutoUpdateStatistics      DatabaseOption = "AUTO_UPDATE_STATISTICS"
	DBOptAutoUpdateStatisticsAsync DatabaseOption = "AUTO_UPDATE_STATISTICS_ASYNC"
	DBOptANSINullDefault           DatabaseOption = "ANSI_NULL_DEFAULT"
	DBOptANSINulls                 DatabaseOption = "ANSI_NULLS"
	DBOptANSIPadding               DatabaseOption = "ANSI_PADDING"
	DBOptANSIWarnings              DatabaseOption = "ANSI_WARNINGS"
	DBOptArithAbort                DatabaseOption = "ARITHABORT"
	DBOptConcatNullYieldsNull      DatabaseOption = "CONCAT_NULL_YIELDS_NULL"
	DBOptNumericRoundAbort         DatabaseOption = "NUMERIC_ROUNDABORT"
	DBOptQuotedIdentifier          DatabaseOption = "QUOTED_IDENTIFIER"
	DBOptRecursiveTriggers         DatabaseOption = "RECURSIVE_TRIGGERS"
	DBOptCursorCloseOnCommit       DatabaseOption = "CURSOR_CLOSE_ON_COMMIT"
	DBOptCursorDefault             DatabaseOption = "CURSOR_DEFAULT"
	DBOptTrustworthy               DatabaseOption = "TRUSTWORTHY"
	DBOptPageVerify                DatabaseOption = "PAGE_VERIFY"
	DBOptContainment               DatabaseOption = "CONTAINMENT"
	DBOptSnapshotIsolation         DatabaseOption = "ALLOW_SNAPSHOT_ISOLATION"
	DBOptReadCommittedSnapshot     DatabaseOption = "READ_COMMITTED_SNAPSHOT"
)

// databaseOptionNames allowlists every DatabaseOption SetDatabaseOption
// accepts. Like permission names, a SET option keyword can't be
// identifier-quoted or parameterised (ALTER DATABASE is DDL).
var databaseOptionNames = map[DatabaseOption]bool{
	DBOptAutoClose: true, DBOptAutoShrink: true,
	DBOptAutoCreateStatistics: true, DBOptAutoUpdateStatistics: true, DBOptAutoUpdateStatisticsAsync: true,
	DBOptANSINullDefault: true, DBOptANSINulls: true, DBOptANSIPadding: true, DBOptANSIWarnings: true,
	DBOptArithAbort: true, DBOptConcatNullYieldsNull: true, DBOptNumericRoundAbort: true,
	DBOptQuotedIdentifier: true, DBOptRecursiveTriggers: true, DBOptCursorCloseOnCommit: true,
	DBOptCursorDefault: true, DBOptTrustworthy: true, DBOptPageVerify: true,
	DBOptContainment: true, DBOptSnapshotIsolation: true, DBOptReadCommittedSnapshot: true,
}

// validDatabaseOption reports whether opt is a recognized SET option.
func validDatabaseOption(opt DatabaseOption) bool { return databaseOptionNames[opt] }

// NeedsExclusiveAccess reports whether setting opt takes an exclusive lock on
// the database: READ_COMMITTED_SNAPSHOT and CONTAINMENT. Under
// TerminationNone such a SET fails or waits while any other session is in
// the database — READ_COMMITTED_SNAPSHOT indefinitely, CONTAINMENT for a few
// seconds before Msg 5061 (17.0) — so a caller offering it should offer
// TerminationRollbackImmediate too. ALLOW_SNAPSHOT_ISOLATION is not one: it
// waits for open transactions, not sessions, and refuses a termination
// clause.
func (o DatabaseOption) NeedsExclusiveAccess() bool {
	return o == DBOptReadCommittedSnapshot || o == DBOptContainment
}

// isSimpleIdentifier reports whether s is a bare identifier token (letters,
// digits, underscore) — the shape a database scoped configuration option
// name always takes (MAXDOP, LEGACY_CARDINALITY_ESTIMATION, ...), unlike
// isSimpleSetValue's more permissive value grammar. Used instead of an
// exhaustive name allowlist because SQL Server adds new scoped
// configuration options with every release; validating the token shape
// blocks injection without going stale as new options appear.
func isSimpleIdentifier(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z', r >= '0' && r <= '9':
		case r == '_':
		default:
			return false
		}
	}
	return true
}

// isSimpleSetValue reports whether s is safe to splice directly after a
// SET option keyword: SQL Server SET-option values are always a bare
// keyword (ON, OFF, CHECKSUM, PARTIAL, ...), a percentage, or a small
// parenthesised clause built by the caller — never free text — so this
// rejects anything containing characters that could break out of that
// position (quotes, semicolons, comment markers).
func isSimpleSetValue(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z', r >= '0' && r <= '9':
		case r == '_' || r == ' ' || r == '(' || r == ')' || r == ',' || r == '=':
		default:
			return false
		}
	}
	return true
}

// SetDatabaseOption changes one ALTER DATABASE ... SET option. value is
// the keyword or clause that follows the option name verbatim, e.g. "ON",
// "OFF", "CHECKSUM", "PARTIAL", "SNAPSHOT_ISOLATION" — see SQL Server's
// ALTER DATABASE SET reference for each option's accepted values.
//
// term is the statement's WITH clause. It matters for an option needing
// exclusive access (DatabaseOption.NeedsExclusiveAccess), which under
// TerminationNone waits for every other session in the database to leave;
// this Server's own idle sessions are released first for those options (see
// Server.ReleaseIdleConnections). The other options take it too.
// CONTAINMENT ignores the clause, so for it TerminationRollbackImmediate
// kills the database's sessions in the same batch instead — which needs
// ALTER ANY CONNECTION, as FileGroup.SetReadOnly's does.
func (d *Database) SetDatabaseOption(ctx context.Context, opt DatabaseOption, value string, term Termination) error {
	if !validDatabaseOption(opt) {
		return invalidf("gosmo: set database option: unrecognized option %q", opt)
	}
	if !isSimpleSetValue(value) {
		return invalidf("gosmo: set database option %s: invalid value %q", opt, value)
	}
	with, err := term.withClause()
	if err != nil {
		return fmt.Errorf("gosmo: set database option %s: %w", opt, err)
	}
	// CONTAINMENT is the one option here whose grammar needs '=' —
	// "SET CONTAINMENT = PARTIAL"; without it the server answers
	// "Incorrect syntax near 'PARTIAL'".
	sep := " "
	if opt == DBOptContainment {
		sep = " = "
	}
	q := fmt.Sprintf("ALTER DATABASE %s SET %s%s%s%s", QuoteName(d.Name), opt, sep, value, with)
	if opt == DBOptContainment && term == TerminationRollbackImmediate {
		// SET CONTAINMENT parses WITH ROLLBACK IMMEDIATE and ignores it, as
		// MODIFY FILEGROUP does (see FileGroup.SetReadOnly): probed on 17.0,
		// it failed Msg 5061 behind another session either way. The kill
		// batch ahead of it is what gets past.
		q = killDatabaseSessionsBatch(d.Name) + ";\n" +
			fmt.Sprintf("ALTER DATABASE %s SET %s%s%s", QuoteName(d.Name), opt, sep, value) + ";"
	}
	if opt.NeedsExclusiveAccess() {
		d.server.releaseIdle(ctx)
	}
	if _, err := d.server.exec(ctx, q); err != nil {
		return fmt.Errorf("gosmo: set %s %s on %q: %w", opt, value, d.Name, err)
	}
	return nil
}

// SetOwner transfers database ownership to a new principal.
func (d *Database) SetOwner(ctx context.Context, principal string) error {
	q := fmt.Sprintf("ALTER AUTHORIZATION ON DATABASE::%s TO %s", QuoteName(d.Name), QuoteName(principal))
	if _, err := d.server.exec(ctx, q); err != nil {
		return fmt.Errorf("gosmo: set owner of %q to %q: %w", d.Name, principal, err)
	}
	return nil
}
