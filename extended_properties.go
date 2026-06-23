package gosmo

import (
	"context"
	"database/sql"
	"fmt"
)

// ============================================================
// Extended Properties
// ============================================================

// ExtendedProperty mirrors a row from sys.extended_properties.
type ExtendedProperty struct {
	Name  string
	Value string
}

// ExtendedPropertyLevel identifies the object level for an extended property.
type ExtendedPropertyLevel struct {
	Level0Type string // e.g. "SCHEMA"
	Level0Name string
	Level1Type string // e.g. "TABLE"
	Level1Name string
	Level2Type string // e.g. "COLUMN"
	Level2Name string
}

// DatabaseExtendedProperties returns all extended properties at database level.
func (d *Database) DatabaseExtendedProperties() ([]*ExtendedProperty, error) {
	const q = `
SELECT name, CAST(value AS NVARCHAR(4000))
FROM   sys.extended_properties
WHERE  class = 0
ORDER  BY name`

	rows, err := d.query(context.Background(), q)
	if err != nil {
		return nil, fmt.Errorf("gosmo: database extended properties: %w", err)
	}
	defer rows.Close()
	return scanExtProps(rows)
}

// ExtendedProperties returns the extended properties for a specific object.
func (d *Database) ExtendedProperties(level ExtendedPropertyLevel) ([]*ExtendedProperty, error) {
	q := fmt.Sprintf(`
SELECT name, CAST(value AS NVARCHAR(4000))
FROM   fn_listextendedproperty(
           NULL,
           N'%s', N'%s',
           %s,
           %s,
           %s,
           %s
       )
ORDER  BY name`,
		escapeSingle(level.Level0Type), escapeSingle(level.Level0Name),
		nullableStr(level.Level1Type), nullableStr(level.Level1Name),
		nullableStr(level.Level2Type), nullableStr(level.Level2Name),
	)
	rows, err := d.query(context.Background(), q)
	if err != nil {
		return nil, fmt.Errorf("gosmo: extended properties: %w", err)
	}
	defer rows.Close()
	return scanExtProps(rows)
}

// AddExtendedProperty adds or updates an extended property on an object.
func (d *Database) AddExtendedProperty(name, value string, level ExtendedPropertyLevel) error {
	// Try UPDATE first; if it touches 0 rows, ADD.
	updateQ := fmt.Sprintf(`
EXEC sp_updateextendedproperty
    @name = N'%s', @value = N'%s',
    @level0type = %s, @level0name = %s,
    @level1type = %s, @level1name = %s,
    @level2type = %s, @level2name = %s`,
		escapeSingle(name), escapeSingle(value),
		nullableStr(level.Level0Type), nullableStr(level.Level0Name),
		nullableStr(level.Level1Type), nullableStr(level.Level1Name),
		nullableStr(level.Level2Type), nullableStr(level.Level2Name),
	)

	res, err := d.exec(context.Background(), updateQ)
	if err == nil {
		n, _ := res.RowsAffected()
		if n > 0 {
			return nil
		}
	}

	addQ := fmt.Sprintf(`
EXEC sp_addextendedproperty
    @name = N'%s', @value = N'%s',
    @level0type = %s, @level0name = %s,
    @level1type = %s, @level1name = %s,
    @level2type = %s, @level2name = %s`,
		escapeSingle(name), escapeSingle(value),
		nullableStr(level.Level0Type), nullableStr(level.Level0Name),
		nullableStr(level.Level1Type), nullableStr(level.Level1Name),
		nullableStr(level.Level2Type), nullableStr(level.Level2Name),
	)
	_, err = d.exec(context.Background(), addQ)
	if err != nil {
		return fmt.Errorf("gosmo: add extended property %q: %w", name, err)
	}
	return nil
}

// DropExtendedProperty drops an extended property from an object.
func (d *Database) DropExtendedProperty(name string, level ExtendedPropertyLevel) error {
	q := fmt.Sprintf(`
EXEC sp_dropextendedproperty
    @name = N'%s',
    @level0type = %s, @level0name = %s,
    @level1type = %s, @level1name = %s,
    @level2type = %s, @level2name = %s`,
		escapeSingle(name),
		nullableStr(level.Level0Type), nullableStr(level.Level0Name),
		nullableStr(level.Level1Type), nullableStr(level.Level1Name),
		nullableStr(level.Level2Type), nullableStr(level.Level2Name),
	)
	_, err := d.exec(context.Background(), q)
	if err != nil {
		return fmt.Errorf("gosmo: drop extended property %q: %w", name, err)
	}
	return nil
}

// -- Helpers -------------------------------------------------------------------

func scanExtProps(rows *sql.Rows) ([]*ExtendedProperty, error) {
	var props []*ExtendedProperty
	for rows.Next() {
		p := &ExtendedProperty{}
		if err := rows.Scan(&p.Name, &p.Value); err != nil {
			return nil, err
		}
		props = append(props, p)
	}
	return props, rows.Err()
}
