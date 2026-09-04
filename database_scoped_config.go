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
}

// scopedConfigSelect renders the read. Split out from
// DatabaseScopedConfigsContext so the version gate can be asserted at every
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
       ` + colSince(d.serverMajorVersion(), SQLServer2017, "is_value_default", "CAST(0 AS bit)") + `
FROM   sys.database_scoped_configurations
ORDER  BY name`
}

// DatabaseScopedConfigs returns every database scoped configuration option.
func (d *Database) DatabaseScopedConfigs() ([]*DatabaseScopedConfig, error) {
	return d.DatabaseScopedConfigsContext(context.Background())
}

// DatabaseScopedConfigsContext is the context-aware variant of
// DatabaseScopedConfigs.
func (d *Database) DatabaseScopedConfigsContext(ctx context.Context) ([]*DatabaseScopedConfig, error) {
	q := d.scopedConfigSelect()

	rows, err := d.query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("gosmo: database scoped configurations in %q: %w", d.name, err)
	}
	defer rows.Close()

	var configs []*DatabaseScopedConfig
	for rows.Next() {
		c := &DatabaseScopedConfig{}
		if err := rows.Scan(&c.ID, &c.Name, &c.Value, &c.ValueForSecondary, &c.IsValueDefault); err != nil {
			return nil, fmt.Errorf("gosmo: database scoped configurations in %q: %w", d.name, err)
		}
		configs = append(configs, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("gosmo: database scoped configurations in %q: %w", d.name, err)
	}
	return configs, nil
}

// buildScopedConfigStatement renders (and validates the inputs of) one
// ALTER DATABASE SCOPED CONFIGURATION statement.
//
// FOR SECONDARY precedes SET: "ALTER DATABASE SCOPED CONFIGURATION FOR
// SECONDARY SET MAXDOP = PRIMARY". Appending it after the assignment instead
// is a syntax error, not a differently-ordered but valid clause — that made
// forSecondary unusable outright. Split out from the method so the clause
// order can be asserted without a server.
func buildScopedConfigStatement(name, value string, forSecondary bool) (string, error) {
	if !isSimpleIdentifier(name) {
		return "", fmt.Errorf("gosmo: set database scoped configuration: invalid name %q", name)
	}
	if !isSimpleSetValue(value) {
		return "", fmt.Errorf("gosmo: set database scoped configuration %s: invalid value %q", name, value)
	}
	scope := ""
	if forSecondary {
		scope = "FOR SECONDARY "
	}
	return fmt.Sprintf("ALTER DATABASE SCOPED CONFIGURATION %sSET %s = %s", scope, name, value), nil
}

// SetDatabaseScopedConfig changes one database scoped configuration option.
// value is the keyword or literal that follows the option name verbatim,
// e.g. "ON", "OFF", "4" — see ALTER DATABASE SCOPED CONFIGURATION's
// reference for each option's accepted values. forSecondary applies the
// change to readable secondary replicas (FOR SECONDARY) instead of the
// primary.
func (d *Database) SetDatabaseScopedConfig(name, value string, forSecondary bool) error {
	return d.SetDatabaseScopedConfigContext(context.Background(), name, value, forSecondary)
}

// SetDatabaseScopedConfigContext is the context-aware variant of
// SetDatabaseScopedConfig. Unlike ALTER DATABASE SET options
// (SetDatabaseOptionContext), ALTER DATABASE SCOPED CONFIGURATION is
// scoped to whichever database is current, so this runs through d.exec
// (USE first), not d.server.execContext.
func (d *Database) SetDatabaseScopedConfigContext(ctx context.Context, name, value string, forSecondary bool) error {
	q, err := buildScopedConfigStatement(name, value, forSecondary)
	if err != nil {
		return err
	}
	if _, err := d.exec(ctx, q); err != nil {
		return fmt.Errorf("gosmo: set database scoped configuration %s = %s on %q: %w", name, value, d.name, err)
	}
	return nil
}
