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
}

// Options returns the database's ALTER DATABASE SET options.
//
// Queried against sys.databases at server scope (like
// Server.DatabaseByName), not through d.query — these are
// catalog-view columns, not per-database data.
func (d *Database) Options(ctx context.Context) (*DatabaseOptions, error) {
	const q = `
SELECT SUSER_SNAME(owner_sid), page_verify_option_desc, user_access_desc,
       containment_desc, is_local_cursor_default, snapshot_isolation_state_desc,
       is_auto_close_on, is_auto_shrink_on, is_auto_create_stats_on,
       is_auto_update_stats_on, is_auto_update_stats_async_on,
       is_ansi_null_default_on, is_ansi_nulls_on, is_ansi_padding_on,
       is_ansi_warnings_on, is_arithabort_on, is_concat_null_yields_null_on,
       is_numeric_roundabort_on, is_quoted_identifier_on, is_recursive_triggers_on,
       is_cursor_close_on_commit_on, is_read_committed_snapshot_on,
       is_trustworthy_on, is_broker_enabled, is_encrypted
FROM   sys.databases
WHERE  name = @p1`

	o := &DatabaseOptions{}
	var owner sql.NullString
	var isLocalCursor bool

	err := d.server.queryRow(ctx, func(row *sql.Row) error {
		return row.Scan(
			&owner, &o.PageVerify, &o.UserAccess, &o.Containment, &isLocalCursor, &o.SnapshotIsolation,
			&o.AutoClose, &o.AutoShrink, &o.AutoCreateStats,
			&o.AutoUpdateStats, &o.AutoUpdateStatsAsync,
			&o.ANSINullDefault, &o.ANSINulls, &o.ANSIPadding,
			&o.ANSIWarnings, &o.ArithAbort, &o.ConcatNullYieldsNull,
			&o.NumericRoundAbort, &o.QuotedIdentifier, &o.RecursiveTriggers,
			&o.CursorCloseOnCommit, &o.ReadCommittedSnapshot,
			&o.IsTrustworthy, &o.IsBrokerEnabled, &o.IsEncrypted,
		)
	}, q, d.Name)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, notFoundf("gosmo: database %q not found", d.Name)
		}
		return nil, fmt.Errorf("gosmo: database options for %q: %w", d.Name, err)
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
// exclusive access — READ_COMMITTED_SNAPSHOT is the one here — which under
// TerminationNone waits for every other session in the database to leave;
// this Server's own idle sessions are released first for that option (see
// Server.ReleaseIdleConnections). The other options take it too.
func (d *Database) SetDatabaseOption(ctx context.Context, opt DatabaseOption, value string, term Termination) error {
	if !validDatabaseOption(opt) {
		return fmt.Errorf("gosmo: set database option: unrecognized option %q", opt)
	}
	if !isSimpleSetValue(value) {
		return fmt.Errorf("gosmo: set database option %s: invalid value %q", opt, value)
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
	q := fmt.Sprintf("ALTER DATABASE %s SET %s%s%s%s", quoteIdent(d.Name), opt, sep, value, with)
	if opt == DBOptReadCommittedSnapshot {
		d.server.releaseIdle(ctx)
	}
	if err := d.server.exec(ctx, q); err != nil {
		return fmt.Errorf("gosmo: set %s %s on %q: %w", opt, value, d.Name, err)
	}
	return nil
}

// SetOwner transfers database ownership to a new principal.
func (d *Database) SetOwner(ctx context.Context, principal string) error {
	q := fmt.Sprintf("ALTER AUTHORIZATION ON DATABASE::%s TO %s", quoteIdent(d.Name), quoteIdent(principal))
	if err := d.server.exec(ctx, q); err != nil {
		return fmt.Errorf("gosmo: set owner of %q to %q: %w", d.Name, principal, err)
	}
	return nil
}
