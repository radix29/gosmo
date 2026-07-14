package gosmo

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ============================================================
// Change tracking  (sys.change_tracking_databases / _tables — SSMS's
// Database Properties > Change Tracking page)
// ============================================================

// ChangeTrackingInfo holds database-level change tracking settings.
type ChangeTrackingInfo struct {
	Enabled         bool
	AutoCleanup     bool
	RetentionPeriod int
	RetentionUnit   string // e.g. "DAYS", "HOURS", "MINUTES"
}

// ChangeTracking returns the database's change tracking settings. Enabled
// is false (with the rest zero-valued) when change tracking has never
// been turned on for this database — there's simply no row for it in
// sys.change_tracking_databases, not an error.
func (d *Database) ChangeTracking() (*ChangeTrackingInfo, error) {
	return d.ChangeTrackingContext(context.Background())
}

// ChangeTrackingContext is the context-aware variant of ChangeTracking.
func (d *Database) ChangeTrackingContext(ctx context.Context) (*ChangeTrackingInfo, error) {
	const q = `
SELECT CASE WHEN ctd.database_id IS NOT NULL THEN 1 ELSE 0 END,
       ISNULL(ctd.is_auto_cleanup_on, 0),
       ISNULL(ctd.retention_period, 0),
       ISNULL(ctd.retention_period_units_desc, '')
FROM   sys.databases sd
LEFT   JOIN sys.change_tracking_databases ctd ON ctd.database_id = sd.database_id
WHERE  sd.name = @p1`

	info := &ChangeTrackingInfo{}
	row := d.server.db.QueryRowContext(ctx, q, d.name)
	if err := row.Scan(&info.Enabled, &info.AutoCleanup, &info.RetentionPeriod, &info.RetentionUnit); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("gosmo: database %q not found", d.name)
		}
		return nil, fmt.Errorf("gosmo: change tracking for %q: %w", d.name, err)
	}
	return info, nil
}

// changeTrackingRetentionUnits allowlists the RETENTION_PERIOD unit
// keywords SQL Server accepts — can't be identifier-quoted or
// parameterised (ALTER DATABASE is DDL).
var changeTrackingRetentionUnits = map[string]bool{"DAYS": true, "HOURS": true, "MINUTES": true}

// SetChangeTracking enables, reconfigures, or disables change tracking
// for the database. info.RetentionUnit defaults to "DAYS" when empty.
func (d *Database) SetChangeTracking(info ChangeTrackingInfo) error {
	return d.SetChangeTrackingContext(context.Background(), info)
}

// SetChangeTrackingContext is the context-aware variant of SetChangeTracking.
func (d *Database) SetChangeTrackingContext(ctx context.Context, info ChangeTrackingInfo) error {
	var q string
	if !info.Enabled {
		q = fmt.Sprintf("ALTER DATABASE %s SET CHANGE_TRACKING = OFF", quoteIdent(d.name))
	} else {
		unit := info.RetentionUnit
		if unit == "" {
			unit = "DAYS"
		}
		if !changeTrackingRetentionUnits[unit] {
			return fmt.Errorf("gosmo: set change tracking: unrecognized retention unit %q", unit)
		}
		autoCleanup := "OFF"
		if info.AutoCleanup {
			autoCleanup = "ON"
		}
		q = fmt.Sprintf("ALTER DATABASE %s SET CHANGE_TRACKING = ON (CHANGE_RETENTION = %d %s, AUTO_CLEANUP = %s)",
			quoteIdent(d.name), info.RetentionPeriod, unit, autoCleanup)
	}
	if err := d.server.execContext(ctx, q); err != nil {
		return fmt.Errorf("gosmo: set change tracking on %q: %w", d.name, err)
	}
	return nil
}

// SetTableChangeTracking enables or disables change tracking on one
// table. trackColumns is ignored when enable is false.
func (d *Database) SetTableChangeTracking(schema, name string, enable, trackColumns bool) error {
	return d.SetTableChangeTrackingContext(context.Background(), schema, name, enable, trackColumns)
}

// SetTableChangeTrackingContext is the context-aware variant of SetTableChangeTracking.
func (d *Database) SetTableChangeTrackingContext(ctx context.Context, schema, name string, enable, trackColumns bool) error {
	ref := qualifiedName(schema, name)
	var q string
	if !enable {
		q = fmt.Sprintf("ALTER TABLE %s DISABLE CHANGE_TRACKING", ref)
	} else {
		track := "OFF"
		if trackColumns {
			track = "ON"
		}
		q = fmt.Sprintf("ALTER TABLE %s ENABLE CHANGE_TRACKING WITH (TRACK_COLUMNS_UPDATED = %s)", ref, track)
	}
	if _, err := d.exec(ctx, q); err != nil {
		return fmt.Errorf("gosmo: set change tracking on %s: %w", ref, err)
	}
	return nil
}

// TableChangeTracking describes one user table's change tracking state.
type TableChangeTracking struct {
	Schema              string
	Name                string
	Enabled             bool
	TrackColumnsUpdated bool
}

// TableChangeTracking returns change tracking state for every user table
// in the database, whether or not tracking is actually enabled on it.
func (d *Database) TableChangeTracking() ([]*TableChangeTracking, error) {
	return d.TableChangeTrackingContext(context.Background())
}

// TableChangeTrackingContext is the context-aware variant of
// TableChangeTracking.
func (d *Database) TableChangeTrackingContext(ctx context.Context) ([]*TableChangeTracking, error) {
	const q = `
SELECT SCHEMA_NAME(t.schema_id), t.name,
       CASE WHEN ctt.object_id IS NOT NULL THEN 1 ELSE 0 END,
       ISNULL(ctt.is_track_columns_updated_on, 0)
FROM   sys.tables t
LEFT   JOIN sys.change_tracking_tables ctt ON ctt.object_id = t.object_id
WHERE  t.is_ms_shipped = 0
ORDER  BY SCHEMA_NAME(t.schema_id), t.name`

	rows, err := d.query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("gosmo: table change tracking in %q: %w", d.name, err)
	}
	defer rows.Close()

	var out []*TableChangeTracking
	for rows.Next() {
		t := &TableChangeTracking{}
		if err := rows.Scan(&t.Schema, &t.Name, &t.Enabled, &t.TrackColumnsUpdated); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}
