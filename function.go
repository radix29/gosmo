package gosmo

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// -- User-defined functions -----------------------------------------------------

// UserDefinedFunction represents a UDF.
type UserDefinedFunction struct {
	ObjectID   int
	Schema     string
	Name       string
	FuncType   string // "FN" scalar, "TF" multi-statement table-valued, "IF" inline table-valued
	Definition string
	CreateDate time.Time
	ModifyDate time.Time
}

// UserDefinedFunctions returns all UDFs in the database.
func (d *Database) UserDefinedFunctions(ctx context.Context) ([]*UserDefinedFunction, error) {
	return d.userDefinedFunctionsWhere(ctx, "", nil)
}

// UserDefinedFunctionsFiltered returns the UDFs an ObjectFilter matches,
// narrowed by the server. An empty filter is UserDefinedFunctions.
func (d *Database) UserDefinedFunctionsFiltered(ctx context.Context, filter ObjectFilter) ([]*UserDefinedFunction, error) {
	where, args := filter.clause(allObjectsFilterColumns, 1)
	return d.userDefinedFunctionsWhere(ctx, where, args)
}

func (d *Database) userDefinedFunctionsWhere(ctx context.Context, where string, args []any) ([]*UserDefinedFunction, error) {
	q := `
SELECT o.object_id, SCHEMA_NAME(o.schema_id), o.name, o.type,
       ISNULL(m.definition,''), o.create_date, o.modify_date
FROM   sys.objects o
JOIN   sys.sql_modules m ON m.object_id = o.object_id
WHERE  o.type IN ('FN','TF','IF') AND o.is_ms_shipped = 0 ` + where + `
ORDER  BY SCHEMA_NAME(o.schema_id), o.name`

	rows, err := d.query(ctx, q, args...)
	return scanRows(rows, err, fmt.Sprintf("list UDFs in %q", d.Name), func(scan func(...any) error) (*UserDefinedFunction, error) {
		f := &UserDefinedFunction{}
		if err := scan(&f.ObjectID, &f.Schema, &f.Name, &f.FuncType,
			&f.Definition, &f.CreateDate, &f.ModifyDate); err != nil {
			return nil, err
		}
		f.FuncType = strings.TrimSpace(f.FuncType)
		return f, nil
	})
}

// SystemFunctions returns every system function SQL Server ships in the
// "sys" schema (sys.fn_listextendedproperty, ...) — see
// SystemFunctions.
//
// Reads sys.all_objects rather than sys.objects for the same reason
// SystemViews reads sys.all_objects instead of sys.views: shipped
// objects are invisible through the non-"all_" catalog views. Restricted to
// the same type set as UserDefinedFunctions ('FN'/'TF'/'IF') —
// aggregate ('AF') and CLR scalar ('FS') functions are excluded, matching that
// same scope. The "sys" schema is identical in every database on a server, so
// this only needs loading once per connection.
func (d *Database) SystemFunctions(ctx context.Context) ([]*UserDefinedFunction, error) {
	return d.systemFunctionsWhere(ctx, "", nil)
}

// SystemFunctionsFiltered returns the system functions an ObjectFilter
// matches, narrowed by the server. An empty filter is SystemFunctions.
func (d *Database) SystemFunctionsFiltered(ctx context.Context, filter ObjectFilter) ([]*UserDefinedFunction, error) {
	where, args := filter.clause(allObjectsFilterColumns, 1)
	return d.systemFunctionsWhere(ctx, where, args)
}

func (d *Database) systemFunctionsWhere(ctx context.Context, where string, args []any) ([]*UserDefinedFunction, error) {
	q := `
SELECT o.object_id, SCHEMA_NAME(o.schema_id), o.name, o.type,
       ISNULL(m.definition,''), o.create_date, o.modify_date
FROM   sys.all_objects o
LEFT JOIN sys.all_sql_modules m ON m.object_id = o.object_id
WHERE  o.type IN ('FN','TF','IF') AND o.is_ms_shipped = 1 AND SCHEMA_NAME(o.schema_id) = 'sys' ` + where + `
ORDER  BY o.name`

	rows, err := d.query(ctx, q, args...)
	return scanRows(rows, err, fmt.Sprintf("list system UDFs in %q", d.Name), func(scan func(...any) error) (*UserDefinedFunction, error) {
		f := &UserDefinedFunction{}
		if err := scan(&f.ObjectID, &f.Schema, &f.Name, &f.FuncType,
			&f.Definition, &f.CreateDate, &f.ModifyDate); err != nil {
			return nil, err
		}
		f.FuncType = strings.TrimSpace(f.FuncType)
		return f, nil
	})
}

// DropFunction drops a user-defined function — scalar, inline
// table-valued, or multi-statement table-valued alike, all of which DROP
// FUNCTION removes. A function that isn't there is the server's error, not a
// silent success — see the note on Database.DropTable.
func (d *Database) DropFunction(ctx context.Context, schema, name string) error {
	if err := requireSchema("drop function", schema, name); err != nil {
		return err
	}
	if _, err := d.exec(ctx, "DROP FUNCTION "+qualifiedName(schema, name)); err != nil {
		return fmt.Errorf("gosmo: drop function [%s].[%s]: %w", schema, name, err)
	}
	return nil
}
