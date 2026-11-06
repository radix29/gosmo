package gosmo

import (
	"context"
	"fmt"
)

// ============================================================
// Database scoped configuration (sys.database_scoped_configurations —
// SSMS's Database Properties > Database Scoped Configurations page)
// ============================================================

// DatabaseScopedConfig mirrors one row of
// sys.database_scoped_configurations. Value and ValueForSecondary are the
// raw CAST(... AS NVARCHAR) text SQL Server reports for that option's
// sql_variant column — boolean-style options render as "0"/"1" this way,
// not "OFF"/"ON", while enum-style options like ELEVATE_ONLINE render
// their keyword directly (e.g. "OFF"). Callers that know an option is
// boolean should compare against "1", not "ON".
type DatabaseScopedConfig struct {
	ID                int
	Name              string
	Value             string
	ValueForSecondary string
	// IsValueDefault is false on SQL Server 2016, whose
	// sys.database_scoped_configurations has no is_value_default column —
	// see scopedConfigSelect.
	IsValueDefault bool
	// ValueType and ValueForSecondaryType are the sql_variant base types
	// of Value and ValueForSecondary (SQL_VARIANT_PROPERTY(…, 'BaseType'))
	// — "bit" for a switch, "int"/"smallint" for a number, "nvarchar" for a
	// keyword — and "" where the value is NULL. They are what tells a
	// switch reporting "1" from a number reporting 1; the same on 2016.
	ValueType             string
	ValueForSecondaryType string
}

// scopedConfigSelect renders the read. Split out from
// DatabaseScopedConfigs so the version gate can be asserted at every
// major without a server.
func (d *Database) scopedConfigSelect() string {
	// is_value_default was "Added in SQL Server 2017" — the column table of
	// https://learn.microsoft.com/sql/relational-databases/system-catalog-views/sys-database-scoped-configurations-transact-sql
	// — while the view itself is 2016. Naming it on 2016 fails the whole read,
	// so it is substituted there and IsValueDefault reads false for every
	// option, which is what a caller that cannot ask must assume.
	return `
SELECT configuration_id, name, CAST(value AS NVARCHAR(256)),
       CAST(ISNULL(value_for_secondary, '') AS NVARCHAR(256)),
       ` + bitSince(d.serverMajorVersion(), SQLServer2017, "is_value_default") + `,
       CAST(ISNULL(SQL_VARIANT_PROPERTY(value, 'BaseType'), '') AS NVARCHAR(128)),
       CAST(ISNULL(SQL_VARIANT_PROPERTY(value_for_secondary, 'BaseType'), '') AS NVARCHAR(128))
FROM   sys.database_scoped_configurations
ORDER  BY name`
}

// DatabaseScopedConfigs returns every database scoped configuration option.
func (d *Database) DatabaseScopedConfigs(ctx context.Context) ([]*DatabaseScopedConfig, error) {
	q := d.scopedConfigSelect()

	rows, err := d.query(ctx, q)
	return scanRows(rows, err, fmt.Sprintf("database scoped configurations in %q", d.Name), func(scan func(...any) error) (*DatabaseScopedConfig, error) {
		c := &DatabaseScopedConfig{}
		if err := scan(&c.ID, &c.Name, &c.Value, &c.ValueForSecondary, &c.IsValueDefault, &c.ValueType, &c.ValueForSecondaryType); err != nil {
			return nil, err
		}
		return c, nil
	})
}

// ScopedConfigReplica says which replicas a database scoped configuration
// change applies to.
type ScopedConfigReplica int

const (
	// ScopedConfigPrimary sets the value for the primary (and a database not
	// in an availability group).
	ScopedConfigPrimary ScopedConfigReplica = iota

	// ScopedConfigSecondary sets the value for readable secondary replicas
	// (FOR SECONDARY).
	ScopedConfigSecondary
)

// buildScopedConfigStatement renders (and validates the inputs of) one
// ALTER DATABASE SCOPED CONFIGURATION statement.
//
// FOR SECONDARY precedes SET: "ALTER DATABASE SCOPED CONFIGURATION FOR
// SECONDARY SET MAXDOP = PRIMARY". Appending it after the assignment instead
// is a syntax error, not a differently-ordered but valid clause — that made
// ScopedConfigSecondary unusable outright. Split out from the method so the clause
// order can be asserted without a server.
func buildScopedConfigStatement(name, value string, replica ScopedConfigReplica) (string, error) {
	if !isSimpleSetValue(value) {
		return "", invalidf("gosmo: set database scoped configuration %s: invalid value %q", name, value)
	}
	return scopedConfigStatement(name, value, replica)
}

// scopedConfigStatement is buildScopedConfigStatement without the value
// check, for a value the caller has already made safe: the scripter's
// tsql.QuoteLiteral of a string-valued configuration (N'https://…'), which
// isSimpleSetValue rejects for its quotes. Name and replica are still
// validated.
func scopedConfigStatement(name, value string, replica ScopedConfigReplica) (string, error) {
	if !isSimpleIdentifier(name) {
		return "", invalidf("gosmo: set database scoped configuration: invalid name %q", name)
	}
	scope := ""
	switch replica {
	case ScopedConfigPrimary:
	case ScopedConfigSecondary:
		scope = "FOR SECONDARY "
	default:
		return "", invalidf("gosmo: set database scoped configuration %s: unrecognized replica %d", name, replica)
	}
	return fmt.Sprintf("ALTER DATABASE SCOPED CONFIGURATION %sSET %s = %s", scope, name, value), nil
}

// SetDatabaseScopedConfig changes one database scoped configuration option.
// value is the keyword or literal that follows the option name verbatim,
// e.g. "ON", "OFF", "4" — see ALTER DATABASE SCOPED CONFIGURATION's
// reference for each option's accepted values. ScopedConfigSecondary applies
// the change to readable secondary replicas (FOR SECONDARY) instead of the
// primary.
//
// Unlike ALTER DATABASE SET options (SetDatabaseOption), ALTER DATABASE
// SCOPED CONFIGURATION is scoped to whichever database is current, so this
// runs through d.exec (USE first), not d.server.exec.
func (d *Database) SetDatabaseScopedConfig(ctx context.Context, name, value string, replica ScopedConfigReplica) error {
	q, err := buildScopedConfigStatement(name, value, replica)
	if err != nil {
		return err
	}
	if _, err := d.exec(ctx, q); err != nil {
		return fmt.Errorf("gosmo: set database scoped configuration %s = %s on %q: %w", name, value, d.Name, err)
	}
	return nil
}
