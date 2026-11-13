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
//
// The value column is nullable — sp_addextendedproperty's @value defaults to
// NULL — so a property added without one comes back with Value "" and IsNull
// set. Scanning it into a plain string instead failed the whole read, and with
// it every Extended Properties page listing the object.
type ExtendedProperty struct {
	Name   string
	Value  string
	IsNull bool // the stored value is NULL; Value is ""
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
func (d *Database) DatabaseExtendedProperties(ctx context.Context) ([]*ExtendedProperty, error) {
	const q = `
SELECT name, CAST(value AS NVARCHAR(4000))
FROM   sys.extended_properties
WHERE  class = 0
ORDER  BY name`

	rows, err := d.query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("gosmo: database extended properties: %w", err)
	}
	defer rows.Close()
	props, err := scanExtProps(rows)
	if err != nil {
		return nil, fmt.Errorf("gosmo: database extended properties: %w", err)
	}
	return props, nil
}

// ExtendedProperties returns the extended properties for a specific object.
//
// Every level goes through nullableStr, level 0 included. Hard-coded quotes
// there instead send an empty N-literal for a level the caller left empty, and
// fn_listextendedproperty reads that as a level named by the empty string
// rather than as an absent one — so a zero ExtendedPropertyLevel, which
// AddExtendedProperty and its siblings write as @level0type = NULL, came back
// from this read as no rows at all. The read and the three writes have to name
// the same object.
func (d *Database) ExtendedProperties(ctx context.Context, level ExtendedPropertyLevel) ([]*ExtendedProperty, error) {
	q := fmt.Sprintf(`
SELECT name, CAST(value AS NVARCHAR(4000))
FROM   fn_listextendedproperty(
           NULL,
           %s,
           %s,
           %s,
           %s,
           %s,
           %s
       )
ORDER  BY name`,
		nullableStr(level.Level0Type), nullableStr(level.Level0Name),
		nullableStr(level.Level1Type), nullableStr(level.Level1Name),
		nullableStr(level.Level2Type), nullableStr(level.Level2Name),
	)
	rows, err := d.query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("gosmo: extended properties: %w", err)
	}
	defer rows.Close()
	props, err := scanExtProps(rows)
	if err != nil {
		return nil, fmt.Errorf("gosmo: extended properties: %w", err)
	}
	return props, nil
}

// AddExtendedProperty adds a new extended property on an object. Fails if
// a property with this name already exists at this level — see
// SetExtendedProperty to update one.
func (d *Database) AddExtendedProperty(ctx context.Context, name, value string, level ExtendedPropertyLevel) error {
	q, args := extendedPropertyWrite("sp_addextendedproperty", name, &value, level)
	if _, err := d.exec(ctx, q, args...); err != nil {
		return fmt.Errorf("gosmo: add extended property %q: %w", name, err)
	}
	return nil
}

// SetExtendedProperty updates the value of an existing extended property.
// Fails if no property with this name exists at this level — see
// AddExtendedProperty to create one.
func (d *Database) SetExtendedProperty(ctx context.Context, name, value string, level ExtendedPropertyLevel) error {
	q, args := extendedPropertyWrite("sp_updateextendedproperty", name, &value, level)
	if _, err := d.exec(ctx, q, args...); err != nil {
		return fmt.Errorf("gosmo: set extended property %q: %w", name, err)
	}
	return nil
}

// DropExtendedProperty drops an extended property from an object.
func (d *Database) DropExtendedProperty(ctx context.Context, name string, level ExtendedPropertyLevel) error {
	q, args := extendedPropertyWrite("sp_dropextendedproperty", name, nil, level)
	if _, err := d.exec(ctx, q, args...); err != nil {
		return fmt.Errorf("gosmo: drop extended property %q: %w", name, err)
	}
	return nil
}

// extendedPropertyWrite builds one of the three extended-property procedure
// calls with every caller-supplied argument bound: the name as @p1, the six
// level arguments as @p2…@p7 (an empty one as NULL — an unused level is NULL,
// not N”; see ExtendedProperties), and value, when there is one, as @p8.
//
// An empty value is the one argument written as a literal. The driver sends
// an empty string as nvarchar(max) (a zero length is its "max" marker), and
// @value is sql_variant, which refuses that type: "Operand type clash:
// nvarchar(max) is incompatible with sql_variant" (live on 17). N” is a
// constant, so nothing of the caller's reaches the text either way.
func extendedPropertyWrite(proc, name string, value *string, level ExtendedPropertyLevel) (string, []any) {
	args := []any{name}
	for _, s := range []string{
		level.Level0Type, level.Level0Name,
		level.Level1Type, level.Level1Name,
		level.Level2Type, level.Level2Name,
	} {
		if s == "" {
			args = append(args, nil)
		} else {
			args = append(args, s)
		}
	}
	valueClause := ""
	switch {
	case value == nil:
	case *value == "":
		valueClause = ", @value = N''"
	default:
		valueClause = ", @value = @p8"
		args = append(args, *value)
	}
	q := fmt.Sprintf(`
EXEC %s
    @name = @p1%s,
    @level0type = @p2, @level0name = @p3,
    @level1type = @p4, @level1name = @p5,
    @level2type = @p6, @level2name = @p7`, proc, valueClause)
	return q, args
}

// -- Helpers -------------------------------------------------------------------

func scanExtProps(rows *dbRows) ([]*ExtendedProperty, error) {
	var props []*ExtendedProperty
	for rows.Next() {
		var (
			p     = &ExtendedProperty{}
			value sql.NullString
		)
		if err := rows.Scan(&p.Name, &value); err != nil {
			return nil, err
		}
		p.Value, p.IsNull = value.String, !value.Valid
		props = append(props, p)
	}
	return props, rows.Err()
}
