package gosmo

import (
	"context"
	"database/sql"
	"fmt"
)

// ============================================================
// Change tracking  (sys.change_tracking_databases / _tables — SSMS's
// Database Properties > Change Tracking page)
// ============================================================

// ChangeTrackingUnit is the unit of a change-tracking retention period,
// spelled as CHANGE_RETENTION takes it and retention_period_units_desc
// reports it.
type ChangeTrackingUnit string

const (
	ChangeTrackingDays    ChangeTrackingUnit = "DAYS"
	ChangeTrackingHours   ChangeTrackingUnit = "HOURS"
	ChangeTrackingMinutes ChangeTrackingUnit = "MINUTES"
)

// ChangeTrackingInfo holds database-level change tracking settings.
type ChangeTrackingInfo struct {
	Enabled         bool
	AutoCleanup     bool
	RetentionPeriod int
	RetentionUnit   ChangeTrackingUnit
}

// ChangeTracking returns the database's change tracking settings. Enabled
// is false (with the rest zero-valued) when change tracking has never
// been turned on for this database — there's simply no row for it in
// sys.change_tracking_databases, not an error.
func (d *Database) ChangeTracking(ctx context.Context) (*ChangeTrackingInfo, error) {
	const q = `
SELECT CASE WHEN ctd.database_id IS NOT NULL THEN 1 ELSE 0 END,
       ISNULL(ctd.is_auto_cleanup_on, 0),
       ISNULL(ctd.retention_period, 0),
       ISNULL(ctd.retention_period_units_desc, '')
FROM   sys.databases sd
LEFT   JOIN sys.change_tracking_databases ctd ON ctd.database_id = sd.database_id
WHERE  sd.name = @p1`

	info := &ChangeTrackingInfo{}
	err := d.server.queryRow(ctx, func(row *sql.Row) error {
		return row.Scan(&info.Enabled, &info.AutoCleanup, &info.RetentionPeriod, &info.RetentionUnit)
	}, q, d.Name)
	return foundRow(info, err, notFoundf("gosmo: database %q not found", d.Name), fmt.Sprintf("change tracking for %q", d.Name))
}

// changeTrackingRetentionUnits is ChangeTrackingUnit's validity check. The
// keyword can't be identifier-quoted or parameterised (ALTER DATABASE is DDL),
// so a value outside the constants is refused rather than spliced in.
var changeTrackingRetentionUnits = map[ChangeTrackingUnit]bool{
	ChangeTrackingDays: true, ChangeTrackingHours: true, ChangeTrackingMinutes: true,
}

// SetChangeTracking enables, reconfigures, or disables change tracking
// for the database. info.RetentionUnit defaults to ChangeTrackingDays when
// empty.
func (d *Database) SetChangeTracking(ctx context.Context, info ChangeTrackingInfo) error {
	q, err := buildSetChangeTrackingStatement(d.Name, info)
	if err != nil {
		return err
	}
	if err := d.server.exec(ctx, q); err != nil {
		return fmt.Errorf("gosmo: set change tracking on %q: %w", d.Name, err)
	}
	return nil
}

// buildSetChangeTrackingStatement renders SetChangeTracking's batch.
//
// Enabling branches on the server, not in Go: SET CHANGE_TRACKING = ON on a
// database already tracked is Msg 5088 ("already enabled"), and the form that
// changes retention or auto cleanup there — SET CHANGE_TRACKING (...) without
// "= ON" — is refused on an untracked one. Deciding in the batch needs no
// read first, so a WithScript capture is the same statement execution runs.
// Accepted inside IF on 13, 14 and 17, and under a login holding only ALTER
// on the database (the catalog view shows it its own database's row).
func buildSetChangeTrackingStatement(dbName string, info ChangeTrackingInfo) (string, error) {
	db := quoteIdent(dbName)
	if !info.Enabled {
		return fmt.Sprintf("ALTER DATABASE %s SET CHANGE_TRACKING = OFF", db), nil
	}
	unit := info.RetentionUnit
	if unit == "" {
		unit = ChangeTrackingDays
	}
	if !changeTrackingRetentionUnits[unit] {
		return "", invalidf("gosmo: set change tracking: unrecognized retention unit %q", unit)
	}
	autoCleanup := "OFF"
	if info.AutoCleanup {
		autoCleanup = "ON"
	}
	opts := fmt.Sprintf("(CHANGE_RETENTION = %d %s, AUTO_CLEANUP = %s)", info.RetentionPeriod, unit, autoCleanup)
	return fmt.Sprintf(`IF EXISTS (SELECT 1 FROM sys.change_tracking_databases WHERE database_id = DB_ID(%s))
    ALTER DATABASE %s SET CHANGE_TRACKING %s;
ELSE
    ALTER DATABASE %s SET CHANGE_TRACKING = ON %s;`, QuoteLiteral(dbName), db, opts, db, opts), nil
}

// SetChangeTracking enables or disables change tracking on the table.
// trackColumns is ignored when enable is false. A TableRef is enough.
func (t *Table) SetChangeTracking(ctx context.Context, enable, trackColumns bool) error {
	if err := requireSchema("set table change tracking", t.Schema, t.Name); err != nil {
		return err
	}
	ref := qualifiedName(t.Schema, t.Name)
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
	if _, err := t.db.exec(ctx, q); err != nil {
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

// tableChangeTrackingSelect is shared by Database.TableChangeTracking and
// Table.ChangeTracking. The LEFT JOIN is what makes a table with
// tracking switched off still produce a row.
const tableChangeTrackingSelect = `
SELECT SCHEMA_NAME(t.schema_id), t.name,
       CASE WHEN ctt.object_id IS NOT NULL THEN 1 ELSE 0 END,
       ISNULL(ctt.is_track_columns_updated_on, 0)
FROM   sys.tables t
LEFT   JOIN sys.change_tracking_tables ctt ON ctt.object_id = t.object_id
WHERE  t.is_ms_shipped = 0`

// TableChangeTracking returns change tracking state for every user table
// in the database, whether or not tracking is actually enabled on it.
func (d *Database) TableChangeTracking(ctx context.Context) ([]*TableChangeTracking, error) {
	rows, err := d.query(ctx, tableChangeTrackingSelect+`
ORDER  BY SCHEMA_NAME(t.schema_id), t.name`)
	return scanRows(rows, err, fmt.Sprintf("table change tracking in %q", d.Name), func(scan func(...any) error) (*TableChangeTracking, error) {
		t := &TableChangeTracking{}
		if err := scan(&t.Schema, &t.Name, &t.Enabled, &t.TrackColumnsUpdated); err != nil {
			return nil, err
		}
		return t, nil
	})
}

// ChangeTracking returns the table's change tracking state; it reads by
// name, so a TableRef is enough.
//
// A table that exists but has tracking switched off is not an error — it
// comes back with Enabled false. The error satisfies errors.Is(err,
// ErrNotFound) only when the database has no such user table.
func (t *Table) ChangeTracking(ctx context.Context) (*TableChangeTracking, error) {
	if err := requireSchema("table change tracking for", t.Schema, t.Name); err != nil {
		return nil, err
	}
	d := t.db
	ct := &TableChangeTracking{}
	err := d.queryRow(ctx, func(row *sql.Row) error {
		return row.Scan(&ct.Schema, &ct.Name, &ct.Enabled, &ct.TrackColumnsUpdated)
	}, tableChangeTrackingSelect+`
       AND SCHEMA_NAME(t.schema_id) COLLATE DATABASE_DEFAULT = @p1 AND t.name = @p2`, t.Schema, t.Name)
	return foundRow(ct, err, notFoundf("gosmo: table %s.%s not found in %q", t.Schema, t.Name, d.Name), fmt.Sprintf("change tracking for %s.%s in %q", t.Schema, t.Name, d.Name))
}
