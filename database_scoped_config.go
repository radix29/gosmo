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
// not "OFF"/"ON" (verified live), while enum-style options like
// ELEVATE_ONLINE render their keyword directly (e.g. "OFF"). Callers that
// know an option is boolean should compare against "1", not "ON".
type DatabaseScopedConfig struct {
	ID                int
	Name              string
	Value             string
	ValueForSecondary string
	IsValueDefault    bool
}

// DatabaseScopedConfigs returns every database scoped configuration option.
func (d *Database) DatabaseScopedConfigs() ([]*DatabaseScopedConfig, error) {
	return d.DatabaseScopedConfigsContext(context.Background())
}

// DatabaseScopedConfigsContext is the context-aware variant of
// DatabaseScopedConfigs.
func (d *Database) DatabaseScopedConfigsContext(ctx context.Context) ([]*DatabaseScopedConfig, error) {
	const q = `
SELECT configuration_id, name, CAST(value AS NVARCHAR(256)),
       CAST(ISNULL(value_for_secondary, '') AS NVARCHAR(256)), is_value_default
FROM   sys.database_scoped_configurations
ORDER  BY name`

	rows, err := d.query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("gosmo: database scoped configurations in %q: %w", d.name, err)
	}
	defer rows.Close()

	var configs []*DatabaseScopedConfig
	for rows.Next() {
		c := &DatabaseScopedConfig{}
		if err := rows.Scan(&c.ID, &c.Name, &c.Value, &c.ValueForSecondary, &c.IsValueDefault); err != nil {
			return nil, err
		}
		configs = append(configs, c)
	}
	return configs, rows.Err()
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
	if !isSimpleIdentifier(name) {
		return fmt.Errorf("gosmo: set database scoped configuration: invalid name %q", name)
	}
	if !isSimpleSetValue(value) {
		return fmt.Errorf("gosmo: set database scoped configuration %s: invalid value %q", name, value)
	}
	q := fmt.Sprintf("ALTER DATABASE SCOPED CONFIGURATION SET %s = %s", name, value)
	if forSecondary {
		q += " FOR SECONDARY"
	}
	if _, err := d.exec(ctx, q); err != nil {
		return fmt.Errorf("gosmo: set database scoped configuration %s = %s on %q: %w", name, value, d.name, err)
	}
	return nil
}
