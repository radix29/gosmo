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
	db *Database

	ObjectID   int
	Schema     string
	Name       string
	FuncType   FunctionType
	CreateDate time.Time
	ModifyDate time.Time
}

// FunctionType is a function's sys.objects type: the three T-SQL forms and
// the two CLR ones. A CLR aggregate (AF) is not one — CREATE AGGREGATE makes
// it and DROP AGGREGATE removes it.
type FunctionType string

const (
	FunctionTypeScalar      FunctionType = "FN" // T-SQL scalar
	FunctionTypeInlineTable FunctionType = "IF" // T-SQL inline table-valued
	FunctionTypeTable       FunctionType = "TF" // T-SQL multi-statement table-valued
	FunctionTypeCLRScalar   FunctionType = "FS" // CLR scalar
	FunctionTypeCLRTable    FunctionType = "FT" // CLR table-valued
)

// IsScalar reports a function that returns a value, T-SQL or CLR — the one
// a call selects rather than selects from.
func (t FunctionType) IsScalar() bool {
	return t == FunctionTypeScalar || t == FunctionTypeCLRScalar
}

// IsCLR reports a function implemented in an assembly, with no T-SQL
// definition.
func (t FunctionType) IsCLR() bool {
	return t == FunctionTypeCLRScalar || t == FunctionTypeCLRTable
}

// UserDefinedFunctions returns the UDFs, T-SQL and CLR, an ObjectFilter
// matches, narrowed by the server. The zero ObjectFilter returns every one.
func (d *Database) UserDefinedFunctions(ctx context.Context, filter ObjectFilter) ([]*UserDefinedFunction, error) {
	where, args := filter.clause(moduleFilterColumns, 1)
	return moduleListWhere(ctx, d, "UDFs", userFunctions, functionExtra, where, args, scanFunction)
}

var (
	userFunctions   = moduleSource{from: "sys.objects o", kind: "o.type IN ('FN','TF','IF','FS','FT') AND o.is_ms_shipped = 0"}
	systemFunctions = moduleSource{from: "sys.all_objects o", kind: "o.type IN ('FN','TF','IF') AND o.is_ms_shipped = 1 AND o.schema_id = 4", system: true}
)

// functionExtra is the column a function carries beyond moduleColumns.
const functionExtra = `, o.type`

func scanFunction(d *Database, scan func(...any) error) (*UserDefinedFunction, error) {
	f := &UserDefinedFunction{db: d}
	if err := scan(&f.ObjectID, &f.Schema, &f.Name,
		&f.CreateDate, &f.ModifyDate, &f.FuncType); err != nil {
		return nil, err
	}
	f.FuncType = FunctionType(strings.TrimSpace(string(f.FuncType)))
	return f, nil
}

// SystemFunctions returns every system function SQL Server ships in the
// "sys" schema (sys.fn_listextendedproperty, ...) — see
// SystemFunctions.
//
// Reads sys.all_objects rather than sys.objects for the same reason
// SystemViews reads sys.all_objects instead of sys.views: shipped
// objects are invisible through the non-"all_" catalog views. Restricted to
// the three T-SQL types ('FN'/'TF'/'IF'): the shipped functions are T-SQL,
// and aggregates ('AF') are not functions DROP FUNCTION removes. The "sys"
// schema is identical in every database on a server, so this only needs
// loading once per connection. The zero ObjectFilter returns every one.
func (d *Database) SystemFunctions(ctx context.Context, filter ObjectFilter) ([]*UserDefinedFunction, error) {
	where, args := filter.clause(moduleFilterColumns, 1)
	return moduleListWhere(ctx, d, "system UDFs", systemFunctions, functionExtra, where, args, scanFunction)
}

// UserDefinedFunctionByName returns one user-defined function, T-SQL or
// CLR, by schema and name.
//
// It returns an error satisfying errors.Is(err, ErrNotFound) when the
// database has no such function.
func (d *Database) UserDefinedFunctionByName(ctx context.Context, schema, name string) (*UserDefinedFunction, error) {
	if err := requireSchema("function by name", schema, name); err != nil {
		return nil, err
	}
	fns, err := moduleListWhere(ctx, d, "UDFs", userFunctions, functionExtra,
		"AND SCHEMA_NAME(o.schema_id) COLLATE DATABASE_DEFAULT = @p1 AND o.name = @p2", []any{schema, name}, scanFunction)
	if err != nil {
		return nil, fmt.Errorf("gosmo: find function %s in %q: %w", qualifiedName(schema, name), d.Name, err)
	}
	if len(fns) == 0 {
		return nil, notFoundf("gosmo: function %s not found in %q", qualifiedName(schema, name), d.Name)
	}
	return fns[0], nil
}

// UserDefinedFunctionRef returns a lightweight handle for the function [schema].[name] — no
// query; every field but Schema and Name is zero. See Server.DatabaseRef for
// when a handle is the right form.
func (d *Database) UserDefinedFunctionRef(schema, name string) *UserDefinedFunction {
	return &UserDefinedFunction{db: d, Schema: schema, Name: name}
}

// Database returns the database the function belongs to.
func (f *UserDefinedFunction) Database() *Database { return f.db }

// Definition returns the function's T-SQL text: "" for an encrypted or CLR
// function, ErrNotFound when there is no such function. Listings do not
// carry it; this reads it by name, so a UserDefinedFunctionRef is enough.
func (f *UserDefinedFunction) Definition(ctx context.Context) (string, error) {
	return f.db.moduleDefinition(ctx, "function", "o.type IN ('FN','TF','IF','FS','FT')", f.Schema, f.Name)
}

// Drop drops the function — scalar, inline table-valued or multi-statement
// table-valued, T-SQL or CLR alike, all of which DROP FUNCTION removes. A function that
// isn't there is the server's error, not a silent success — see the note on
// Table.Drop.
func (f *UserDefinedFunction) Drop(ctx context.Context) error {
	return f.db.dropSchemaObject(ctx, "function", "FUNCTION", f.Schema, f.Name)
}

// Rename renames the function (sp_rename's 'OBJECT' class). newName is a bare
// name; a rename never moves the function between schemas — see Transfer.
func (f *UserDefinedFunction) Rename(ctx context.Context, newName string) error {
	if err := f.db.renameSchemaObject(ctx, "function", renameObjectClass, f.Schema, f.Name, newName); err != nil {
		return err
	}
	setIfApplied(ctx, &f.Name, newName)
	return nil
}

// Transfer moves the function into another schema (ALTER SCHEMA ... TRANSFER).
// It keeps its name and object_id; permissions granted on it directly are
// dropped by the server.
func (f *UserDefinedFunction) Transfer(ctx context.Context, targetSchema string) error {
	if err := f.db.transferSchemaObject(ctx, "function", transferObjectClass, targetSchema, f.Schema, f.Name); err != nil {
		return err
	}
	setIfApplied(ctx, &f.Schema, targetSchema)
	return nil
}
