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
	UserAccess        string // "MULTI_USER", "SINGLE_USER", "RESTRICTED_USER"
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
}

// Options returns the database's ALTER DATABASE SET options.
func (d *Database) Options() (*DatabaseOptions, error) {
	return d.OptionsContext(context.Background())
}

// OptionsContext is the context-aware variant of Options. Queried against
// sys.databases at server scope (like Server.DatabaseByNameContext), not
// through d.query — these are catalog-view columns, not per-database data.
func (d *Database) OptionsContext(ctx context.Context) (*DatabaseOptions, error) {
	const q = `
SELECT SUSER_SNAME(owner_sid), page_verify_option_desc, user_access_desc,
       containment_desc, is_local_cursor_default, snapshot_isolation_state_desc,
       is_auto_close_on, is_auto_shrink_on, is_auto_create_stats_on,
       is_auto_update_stats_on, is_auto_update_stats_async_on,
       is_ansi_null_default_on, is_ansi_nulls_on, is_ansi_padding_on,
       is_ansi_warnings_on, is_arithabort_on, is_concat_null_yields_null_on,
       is_numeric_roundabort_on, is_quoted_identifier_on, is_recursive_triggers_on,
       is_cursor_close_on_commit_on, is_read_committed_snapshot_on,
       is_trustworthy_on, is_broker_enabled
FROM   sys.databases
WHERE  name = @p1`

	o := &DatabaseOptions{}
	var owner sql.NullString
	var isLocalCursor bool

	row := d.server.db.QueryRowContext(ctx, q, d.name)
	if err := row.Scan(
		&owner, &o.PageVerify, &o.UserAccess, &o.Containment, &isLocalCursor, &o.SnapshotIsolation,
		&o.AutoClose, &o.AutoShrink, &o.AutoCreateStats,
		&o.AutoUpdateStats, &o.AutoUpdateStatsAsync,
		&o.ANSINullDefault, &o.ANSINulls, &o.ANSIPadding,
		&o.ANSIWarnings, &o.ArithAbort, &o.ConcatNullYieldsNull,
		&o.NumericRoundAbort, &o.QuotedIdentifier, &o.RecursiveTriggers,
		&o.CursorCloseOnCommit, &o.ReadCommittedSnapshot,
		&o.IsTrustworthy, &o.IsBrokerEnabled,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("gosmo: database %q not found", d.name)
		}
		return nil, fmt.Errorf("gosmo: database options for %q: %w", d.name, err)
	}
	o.Owner = owner.String
	if isLocalCursor {
		o.DefaultCursor = "LOCAL"
	} else {
		o.DefaultCursor = "GLOBAL"
	}
	return o, nil
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
func (d *Database) SetDatabaseOption(opt DatabaseOption, value string) error {
	return d.SetDatabaseOptionContext(context.Background(), opt, value)
}

// SetDatabaseOptionContext is the context-aware variant of SetDatabaseOption.
func (d *Database) SetDatabaseOptionContext(ctx context.Context, opt DatabaseOption, value string) error {
	if !validDatabaseOption(opt) {
		return fmt.Errorf("gosmo: set database option: unrecognized option %q", opt)
	}
	if !isSimpleSetValue(value) {
		return fmt.Errorf("gosmo: set database option %s: invalid value %q", opt, value)
	}
	q := fmt.Sprintf("ALTER DATABASE %s SET %s %s", quoteIdent(d.name), opt, value)
	if err := d.server.execContext(ctx, q); err != nil {
		return fmt.Errorf("gosmo: set %s %s on %q: %w", opt, value, d.name, err)
	}
	return nil
}

// SetOwner transfers database ownership to a new principal.
func (d *Database) SetOwner(principal string) error {
	return d.SetOwnerContext(context.Background(), principal)
}

// SetOwnerContext is the context-aware variant of SetOwner.
func (d *Database) SetOwnerContext(ctx context.Context, principal string) error {
	q := fmt.Sprintf("ALTER AUTHORIZATION ON DATABASE::%s TO %s", quoteIdent(d.name), quoteIdent(principal))
	if err := d.server.execContext(ctx, q); err != nil {
		return fmt.Errorf("gosmo: set owner of %q to %q: %w", d.name, principal, err)
	}
	return nil
}
