package gosmo

// User-defined types and XML schema collections — the four families SSMS
// groups under Programmability ▸ Types, plus the built-in list it shows
// beside them.
//
// All four live in sys.types, separated only by flag columns, and getting the
// predicates wrong makes one family's members appear in another's folder:
//
//	is_user_defined = 1, is_table_type = 0, is_assembly_type = 0  alias types
//	is_table_type   = 1                                          table types
//	is_assembly_type= 1                                          CLR types
//	is_user_defined = 0                                          system types
//
// A table type satisfies is_user_defined too, so every alias-type predicate
// here excludes it explicitly rather than relying on is_user_defined alone.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ============================================================
// Alias types  (CREATE TYPE ... FROM <base type>)
// ============================================================

// UserDefinedDataType is an alias type — a base system type with a fixed
// length/precision, nullability, and optionally a bound rule or default.
// It mirrors a sys.types row with is_user_defined = 1 and both is_table_type
// and is_assembly_type 0.
type UserDefinedDataType struct {
	db *Database

	Name       string
	Schema     string
	UserTypeID int

	// BaseType is the system type the alias is built on, as sys.types names
	// it ("varchar", "decimal", ...).
	BaseType string

	// MaxLength is the storage length in bytes, as sys.types reports it: -1
	// for a MAX type, and twice the character count for the Unicode types.
	MaxLength int
	Precision int
	Scale     int
	Collation string

	// IsNullable is the type's own nullability, which a column declaration
	// can still override.
	IsNullable bool

	// Rule and Default name the objects bound to the type with sp_bindrule
	// and sp_bindefault, empty when nothing is bound. Both mechanisms are
	// deprecated by Microsoft; a type carrying one is legacy, not a defect.
	Rule    string
	Default string
}

// FullName returns the schema-qualified, bracket-quoted name.
func (t *UserDefinedDataType) FullName() string { return qualifiedName(t.Schema, t.Name) }

// Database returns the database the type belongs to.
func (t *UserDefinedDataType) Database() *Database { return t.db }

// userDefinedDataTypeSelect is the SELECT the listing and the by-name finder
// share; each appends its own trailing clause.
//
// The rule and default are resolved through OBJECT_NAME rather than a join to
// sys.objects: rule_object_id and default_object_id are 0 rather than NULL
// when nothing is bound, and OBJECT_NAME(0) is NULL, which ISNULL turns into
// the empty string a caller can test.
const userDefinedDataTypeSelect = `
SELECT t.name, SCHEMA_NAME(t.schema_id), t.user_type_id,
       ISNULL(TYPE_NAME(t.system_type_id), ''),
       t.max_length, t.precision, t.scale,
       ISNULL(t.collation_name, ''), ISNULL(t.is_nullable, 0),
       ISNULL(OBJECT_NAME(t.rule_object_id), ''),
       ISNULL(OBJECT_NAME(t.default_object_id), '')
FROM   sys.types t
WHERE  t.is_user_defined = 1 AND t.is_table_type = 0 AND t.is_assembly_type = 0`

func scanUserDefinedDataType(d *Database, scan func(...any) error) (*UserDefinedDataType, error) {
	t := &UserDefinedDataType{db: d}
	if err := scan(
		&t.Name, &t.Schema, &t.UserTypeID,
		&t.BaseType,
		&t.MaxLength, &t.Precision, &t.Scale,
		&t.Collation, &t.IsNullable,
		&t.Rule, &t.Default,
	); err != nil {
		return nil, err
	}
	return t, nil
}

// UserDefinedDataTypes returns the alias types defined in the database.
func (d *Database) UserDefinedDataTypes() ([]*UserDefinedDataType, error) {
	return d.UserDefinedDataTypesContext(context.Background())
}

// UserDefinedDataTypesContext is the context-aware variant of
// UserDefinedDataTypes.
func (d *Database) UserDefinedDataTypesContext(ctx context.Context) ([]*UserDefinedDataType, error) {
	const q = userDefinedDataTypeSelect + `
ORDER  BY SCHEMA_NAME(t.schema_id), t.name`

	rows, err := d.query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("gosmo: list user-defined data types in %q: %w", d.name, err)
	}
	defer rows.Close()

	var types []*UserDefinedDataType
	for rows.Next() {
		t, err := scanUserDefinedDataType(d, rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("gosmo: list user-defined data types in %q: %w", d.name, err)
		}
		types = append(types, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("gosmo: list user-defined data types in %q: %w", d.name, err)
	}
	return types, nil
}

// UserDefinedDataTypeByName returns one alias type, or a not-found error
// (errors.Is ErrNotFound) when the database has none by that name.
func (d *Database) UserDefinedDataTypeByName(schema, name string) (*UserDefinedDataType, error) {
	return d.UserDefinedDataTypeByNameContext(context.Background(), schema, name)
}

// UserDefinedDataTypeByNameContext is the context-aware variant of
// UserDefinedDataTypeByName.
func (d *Database) UserDefinedDataTypeByNameContext(ctx context.Context, schema, name string) (*UserDefinedDataType, error) {
	if schema == "" {
		schema = "dbo"
	}
	var t *UserDefinedDataType
	err := d.queryRow(ctx, func(row *sql.Row) error {
		var err error
		t, err = scanUserDefinedDataType(d, row.Scan)
		return err
	}, userDefinedDataTypeSelect+`
   AND SCHEMA_NAME(t.schema_id) = @p1 AND t.name = @p2`, schema, name)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, notFoundf("gosmo: user-defined data type [%s].[%s] not found in %q", schema, name, d.name)
	}
	if err != nil {
		return nil, fmt.Errorf("gosmo: read user-defined data type [%s].[%s] in %q: %w", schema, name, d.name, err)
	}
	return t, nil
}

// Drop drops the alias type.
func (t *UserDefinedDataType) Drop() error { return t.DropContext(context.Background()) }

// DropContext is the context-aware variant of Drop.
func (t *UserDefinedDataType) DropContext(ctx context.Context) error {
	return t.db.DropTypeContext(ctx, t.Schema, t.Name)
}

// ============================================================
// Table types  (CREATE TYPE ... AS TABLE)
// ============================================================

// UserDefinedTableType mirrors a sys.table_types row.
type UserDefinedTableType struct {
	db *Database

	Name       string
	Schema     string
	UserTypeID int

	// TypeTableObjectID is the object_id of the *internal* table that holds
	// the type's shape. The type's columns, indexes and check constraints
	// hang off this id, not off UserTypeID and not off any id in sys.objects
	// a caller could reach by name — see ColumnsContext.
	TypeTableObjectID int

	// IsMemoryOptimized reports a memory-optimized table type (2014+). The
	// column is nullable in the catalog and reads as false where it is NULL.
	IsMemoryOptimized bool
}

// FullName returns the schema-qualified, bracket-quoted name.
func (t *UserDefinedTableType) FullName() string { return qualifiedName(t.Schema, t.Name) }

// Database returns the database the type belongs to.
func (t *UserDefinedTableType) Database() *Database { return t.db }

const userDefinedTableTypeSelect = `
SELECT tt.name, SCHEMA_NAME(tt.schema_id), tt.user_type_id,
       tt.type_table_object_id, ISNULL(tt.is_memory_optimized, 0)
FROM   sys.table_types tt
WHERE  tt.is_user_defined = 1`

func scanUserDefinedTableType(d *Database, scan func(...any) error) (*UserDefinedTableType, error) {
	t := &UserDefinedTableType{db: d}
	if err := scan(&t.Name, &t.Schema, &t.UserTypeID,
		&t.TypeTableObjectID, &t.IsMemoryOptimized); err != nil {
		return nil, err
	}
	return t, nil
}

// UserDefinedTableTypes returns the table types defined in the database.
func (d *Database) UserDefinedTableTypes() ([]*UserDefinedTableType, error) {
	return d.UserDefinedTableTypesContext(context.Background())
}

// UserDefinedTableTypesContext is the context-aware variant of
// UserDefinedTableTypes.
func (d *Database) UserDefinedTableTypesContext(ctx context.Context) ([]*UserDefinedTableType, error) {
	const q = userDefinedTableTypeSelect + `
ORDER  BY SCHEMA_NAME(tt.schema_id), tt.name`

	rows, err := d.query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("gosmo: list user-defined table types in %q: %w", d.name, err)
	}
	defer rows.Close()

	var types []*UserDefinedTableType
	for rows.Next() {
		t, err := scanUserDefinedTableType(d, rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("gosmo: list user-defined table types in %q: %w", d.name, err)
		}
		types = append(types, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("gosmo: list user-defined table types in %q: %w", d.name, err)
	}
	return types, nil
}

// UserDefinedTableTypeByName returns one table type, or a not-found error
// (errors.Is ErrNotFound) when the database has none by that name.
func (d *Database) UserDefinedTableTypeByName(schema, name string) (*UserDefinedTableType, error) {
	return d.UserDefinedTableTypeByNameContext(context.Background(), schema, name)
}

// UserDefinedTableTypeByNameContext is the context-aware variant of
// UserDefinedTableTypeByName.
func (d *Database) UserDefinedTableTypeByNameContext(ctx context.Context, schema, name string) (*UserDefinedTableType, error) {
	if schema == "" {
		schema = "dbo"
	}
	var t *UserDefinedTableType
	err := d.queryRow(ctx, func(row *sql.Row) error {
		var err error
		t, err = scanUserDefinedTableType(d, row.Scan)
		return err
	}, userDefinedTableTypeSelect+`
   AND SCHEMA_NAME(tt.schema_id) = @p1 AND tt.name = @p2`, schema, name)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, notFoundf("gosmo: user-defined table type [%s].[%s] not found in %q", schema, name, d.name)
	}
	if err != nil {
		return nil, fmt.Errorf("gosmo: read user-defined table type [%s].[%s] in %q: %w", schema, name, d.name, err)
	}
	return t, nil
}

// Columns returns the table type's columns in ordinal order.
func (t *UserDefinedTableType) Columns() ([]*Column, error) {
	return t.ColumnsContext(context.Background())
}

// ColumnsContext is the context-aware variant of Columns.
//
// The columns are read through TypeTableObjectID, the internal table
// sys.table_types points at — a table type's columns are *not* on
// sys.columns under its user_type_id, and OBJECT_ID('[schema].[name]') does
// not resolve a type at all, so neither of the obvious lookups finds
// anything. A type built by hand rather than by a listing has a zero
// TypeTableObjectID and gets a not-found error rather than an empty list.
func (t *UserDefinedTableType) ColumnsContext(ctx context.Context) ([]*Column, error) {
	if t.TypeTableObjectID == 0 {
		return nil, notFoundf("gosmo: user-defined table type %s in %q has no internal table id — read it with UserDefinedTableTypeByName",
			t.FullName(), t.db.name)
	}
	const q = columnSelect + `
WHERE  c.object_id = @p1
ORDER  BY c.column_id`

	rows, err := t.db.query(ctx, q, t.TypeTableObjectID)
	if err != nil {
		return nil, fmt.Errorf("gosmo: list columns for table type %s in %q: %w", t.FullName(), t.db.name, err)
	}
	defer rows.Close()

	cols, err := scanColumns(rows.Rows)
	if err != nil {
		return nil, fmt.Errorf("gosmo: list columns for table type %s in %q: %w", t.FullName(), t.db.name, err)
	}
	return cols, nil
}

// Drop drops the table type.
func (t *UserDefinedTableType) Drop() error { return t.DropContext(context.Background()) }

// DropContext is the context-aware variant of Drop.
func (t *UserDefinedTableType) DropContext(ctx context.Context) error {
	return t.db.DropTypeContext(ctx, t.Schema, t.Name)
}

// ============================================================
// CLR types  (CREATE TYPE ... EXTERNAL NAME)
// ============================================================

// ClrType is a user-defined type implemented by a CLR assembly — a sys.types
// row with is_assembly_type = 1.
type ClrType struct {
	db *Database

	Name       string
	Schema     string
	UserTypeID int

	// MaxLength is the serialized length in bytes, -1 for a MAX type.
	MaxLength int
	Precision int
	Scale     int

	// IsNullable is the type's own nullability.
	IsNullable bool

	// Assembly and AssemblyClass name the implementation. Both come from
	// sys.assembly_types, and are empty on the rare row that has none.
	Assembly      string
	AssemblyClass string
}

// FullName returns the schema-qualified, bracket-quoted name.
func (t *ClrType) FullName() string { return qualifiedName(t.Schema, t.Name) }

// Database returns the database the type belongs to.
func (t *ClrType) Database() *Database { return t.db }

// clrTypeSelect joins sys.assembly_types, not sys.assembly_modules: a CLR
// *type* is registered in the former, while the latter carries CLR
// procedures, functions and triggers, and matching a type's id against it
// returns nothing.
const clrTypeSelect = `
SELECT t.name, SCHEMA_NAME(t.schema_id), t.user_type_id,
       t.max_length, t.precision, t.scale, ISNULL(t.is_nullable, 0),
       ISNULL(a.name, ''), ISNULL(at.assembly_class, '')
FROM   sys.types t
LEFT   JOIN sys.assembly_types at ON at.user_type_id = t.user_type_id
LEFT   JOIN sys.assemblies a ON a.assembly_id = at.assembly_id
WHERE  t.is_assembly_type = 1`

func scanClrType(d *Database, scan func(...any) error) (*ClrType, error) {
	t := &ClrType{db: d}
	if err := scan(&t.Name, &t.Schema, &t.UserTypeID,
		&t.MaxLength, &t.Precision, &t.Scale, &t.IsNullable,
		&t.Assembly, &t.AssemblyClass); err != nil {
		return nil, err
	}
	return t, nil
}

// ClrTypes returns the CLR user-defined types in the database.
func (d *Database) ClrTypes() ([]*ClrType, error) {
	return d.ClrTypesContext(context.Background())
}

// ClrTypesContext is the context-aware variant of ClrTypes.
func (d *Database) ClrTypesContext(ctx context.Context) ([]*ClrType, error) {
	const q = clrTypeSelect + `
ORDER  BY SCHEMA_NAME(t.schema_id), t.name`

	rows, err := d.query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("gosmo: list CLR types in %q: %w", d.name, err)
	}
	defer rows.Close()

	var types []*ClrType
	for rows.Next() {
		t, err := scanClrType(d, rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("gosmo: list CLR types in %q: %w", d.name, err)
		}
		types = append(types, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("gosmo: list CLR types in %q: %w", d.name, err)
	}
	return types, nil
}

// ClrTypeByName returns one CLR type, or a not-found error (errors.Is
// ErrNotFound) when the database has none by that name.
func (d *Database) ClrTypeByName(schema, name string) (*ClrType, error) {
	return d.ClrTypeByNameContext(context.Background(), schema, name)
}

// ClrTypeByNameContext is the context-aware variant of ClrTypeByName.
func (d *Database) ClrTypeByNameContext(ctx context.Context, schema, name string) (*ClrType, error) {
	if schema == "" {
		schema = "dbo"
	}
	var t *ClrType
	err := d.queryRow(ctx, func(row *sql.Row) error {
		var err error
		t, err = scanClrType(d, row.Scan)
		return err
	}, clrTypeSelect+`
   AND SCHEMA_NAME(t.schema_id) = @p1 AND t.name = @p2`, schema, name)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, notFoundf("gosmo: CLR type [%s].[%s] not found in %q", schema, name, d.name)
	}
	if err != nil {
		return nil, fmt.Errorf("gosmo: read CLR type [%s].[%s] in %q: %w", schema, name, d.name, err)
	}
	return t, nil
}

// Drop drops the CLR type.
func (t *ClrType) Drop() error { return t.DropContext(context.Background()) }

// DropContext is the context-aware variant of Drop.
func (t *ClrType) DropContext(ctx context.Context) error {
	return t.db.DropTypeContext(ctx, t.Schema, t.Name)
}

// ============================================================
// System types
// ============================================================

// SystemDataType is one of the built-in types the instance ships — the fixed
// list SSMS shows under Types ▸ System Data Types.
//
// It is read from the catalog rather than hard-coded so the list is always
// the connected instance's own: the set has grown between releases, and a
// literal list in Go would be a list of some other version's types.
type SystemDataType struct {
	Name       string
	SystemType int

	// MaxLength is the storage length in bytes as sys.types reports it, -1
	// for a MAX type. For the parameterized types (varchar, decimal, ...)
	// this is the catalog's declaration of the type itself, not of any
	// column using it.
	MaxLength  int
	Precision  int
	Scale      int
	IsNullable bool
}

// SystemDataTypes returns the built-in data types the instance ships.
func (d *Database) SystemDataTypes() ([]*SystemDataType, error) {
	return d.SystemDataTypesContext(context.Background())
}

// SystemDataTypesContext is the context-aware variant of SystemDataTypes.
func (d *Database) SystemDataTypesContext(ctx context.Context) ([]*SystemDataType, error) {
	const q = `
SELECT t.name, t.system_type_id, t.max_length, t.precision, t.scale,
       ISNULL(t.is_nullable, 0)
FROM   sys.types t
WHERE  t.is_user_defined = 0
ORDER  BY t.name`

	rows, err := d.query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("gosmo: list system data types in %q: %w", d.name, err)
	}
	defer rows.Close()

	var types []*SystemDataType
	for rows.Next() {
		t := &SystemDataType{}
		if err := rows.Scan(&t.Name, &t.SystemType, &t.MaxLength,
			&t.Precision, &t.Scale, &t.IsNullable); err != nil {
			return nil, fmt.Errorf("gosmo: list system data types in %q: %w", d.name, err)
		}
		types = append(types, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("gosmo: list system data types in %q: %w", d.name, err)
	}
	return types, nil
}

// ============================================================
// XML schema collections
// ============================================================

// XmlSchemaCollection mirrors a sys.xml_schema_collections row.
type XmlSchemaCollection struct {
	db *Database

	Name         string
	Schema       string
	CollectionID int
	CreateDate   time.Time
	ModifyDate   time.Time
}

// FullName returns the schema-qualified, bracket-quoted name.
func (c *XmlSchemaCollection) FullName() string { return qualifiedName(c.Schema, c.Name) }

// Database returns the database the collection belongs to.
func (c *XmlSchemaCollection) Database() *Database { return c.db }

// xmlSchemaCollectionSelect excludes the sys schema, which holds the
// server's own sys.sys collection in every database and is not the user's.
const xmlSchemaCollectionSelect = `
SELECT x.name, SCHEMA_NAME(x.schema_id), x.xml_collection_id,
       x.create_date, x.modify_date
FROM   sys.xml_schema_collections x
WHERE  SCHEMA_NAME(x.schema_id) <> 'sys'`

func scanXmlSchemaCollection(d *Database, scan func(...any) error) (*XmlSchemaCollection, error) {
	c := &XmlSchemaCollection{db: d}
	if err := scan(&c.Name, &c.Schema, &c.CollectionID,
		&c.CreateDate, &c.ModifyDate); err != nil {
		return nil, err
	}
	return c, nil
}

// XmlSchemaCollections returns the XML schema collections in the database.
func (d *Database) XmlSchemaCollections() ([]*XmlSchemaCollection, error) {
	return d.XmlSchemaCollectionsContext(context.Background())
}

// XmlSchemaCollectionsContext is the context-aware variant of
// XmlSchemaCollections.
func (d *Database) XmlSchemaCollectionsContext(ctx context.Context) ([]*XmlSchemaCollection, error) {
	const q = xmlSchemaCollectionSelect + `
ORDER  BY SCHEMA_NAME(x.schema_id), x.name`

	rows, err := d.query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("gosmo: list XML schema collections in %q: %w", d.name, err)
	}
	defer rows.Close()

	var cols []*XmlSchemaCollection
	for rows.Next() {
		c, err := scanXmlSchemaCollection(d, rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("gosmo: list XML schema collections in %q: %w", d.name, err)
		}
		cols = append(cols, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("gosmo: list XML schema collections in %q: %w", d.name, err)
	}
	return cols, nil
}

// XmlSchemaCollectionByName returns one XML schema collection, or a
// not-found error (errors.Is ErrNotFound) when the database has none by that
// name.
func (d *Database) XmlSchemaCollectionByName(schema, name string) (*XmlSchemaCollection, error) {
	return d.XmlSchemaCollectionByNameContext(context.Background(), schema, name)
}

// XmlSchemaCollectionByNameContext is the context-aware variant of
// XmlSchemaCollectionByName.
func (d *Database) XmlSchemaCollectionByNameContext(ctx context.Context, schema, name string) (*XmlSchemaCollection, error) {
	if schema == "" {
		schema = "dbo"
	}
	var c *XmlSchemaCollection
	err := d.queryRow(ctx, func(row *sql.Row) error {
		var err error
		c, err = scanXmlSchemaCollection(d, row.Scan)
		return err
	}, xmlSchemaCollectionSelect+`
   AND SCHEMA_NAME(x.schema_id) = @p1 AND x.name = @p2`, schema, name)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, notFoundf("gosmo: XML schema collection [%s].[%s] not found in %q", schema, name, d.name)
	}
	if err != nil {
		return nil, fmt.Errorf("gosmo: read XML schema collection [%s].[%s] in %q: %w", schema, name, d.name, err)
	}
	return c, nil
}

// Definition returns the collection's schema documents as one XML string —
// what CREATE XML SCHEMA COLLECTION was given, as the server reassembles it.
func (c *XmlSchemaCollection) Definition() (string, error) {
	return c.DefinitionContext(context.Background())
}

// DefinitionContext is the context-aware variant of Definition.
//
// XML_SCHEMA_NAMESPACE takes the schema and collection name as *string
// literals*, not identifiers, so both are passed as parameters rather than
// bracket-quoted into the statement.
func (c *XmlSchemaCollection) DefinitionContext(ctx context.Context) (string, error) {
	const q = `SELECT CAST(XML_SCHEMA_NAMESPACE(@p1, @p2) AS NVARCHAR(MAX))`

	var def sql.NullString
	err := c.db.queryRow(ctx, func(row *sql.Row) error {
		return row.Scan(&def)
	}, q, c.Schema, c.Name)
	if errors.Is(err, sql.ErrNoRows) {
		return "", notFoundf("gosmo: XML schema collection %s not found in %q", c.FullName(), c.db.name)
	}
	if err != nil {
		return "", fmt.Errorf("gosmo: read XML schema collection %s in %q: %w", c.FullName(), c.db.name, err)
	}
	return def.String, nil
}

// Drop drops the XML schema collection.
func (c *XmlSchemaCollection) Drop() error { return c.DropContext(context.Background()) }

// DropContext is the context-aware variant of Drop.
func (c *XmlSchemaCollection) DropContext(ctx context.Context) error {
	return c.db.DropXmlSchemaCollectionContext(ctx, c.Schema, c.Name)
}

// ============================================================
// Drops by name
// ============================================================

// DropType drops an alias, table or CLR type — all three are DROP TYPE, and
// nothing in the statement distinguishes them, so one method serves all
// three families. A type still referenced by a column, parameter or function
// is refused by the server; that error is the caller's to report.
func (d *Database) DropType(schema, name string) error {
	return d.DropTypeContext(context.Background(), schema, name)
}

// DropTypeContext is the context-aware variant of DropType.
func (d *Database) DropTypeContext(ctx context.Context, schema, name string) error {
	if schema == "" {
		schema = "dbo"
	}
	if _, err := d.exec(ctx, "DROP TYPE "+qualifiedName(schema, name)); err != nil {
		return fmt.Errorf("gosmo: drop type [%s].[%s]: %w", schema, name, err)
	}
	return nil
}

// DropXmlSchemaCollection drops an XML schema collection.
func (d *Database) DropXmlSchemaCollection(schema, name string) error {
	return d.DropXmlSchemaCollectionContext(context.Background(), schema, name)
}

// DropXmlSchemaCollectionContext is the context-aware variant of
// DropXmlSchemaCollection.
func (d *Database) DropXmlSchemaCollectionContext(ctx context.Context, schema, name string) error {
	if schema == "" {
		schema = "dbo"
	}
	if _, err := d.exec(ctx, "DROP XML SCHEMA COLLECTION "+qualifiedName(schema, name)); err != nil {
		return fmt.Errorf("gosmo: drop XML schema collection [%s].[%s]: %w", schema, name, err)
	}
	return nil
}

// ============================================================
// Schema transfers
// ============================================================

// TransferType moves an alias, table or CLR type into another schema.
//
// ALTER SCHEMA ... TRANSFER's default class covers only the objects in
// sys.objects; a type lives in sys.types and needs the TYPE:: prefix, which
// is why Database.TransferObject does not serve here. Everything else about
// the operation is that method's: the type keeps its name, and permissions
// granted on it directly are dropped by the server.
func (d *Database) TransferType(targetSchema, schema, name string) error {
	return d.TransferTypeContext(context.Background(), targetSchema, schema, name)
}

// TransferTypeContext is the context-aware variant of TransferType.
func (d *Database) TransferTypeContext(ctx context.Context, targetSchema, schema, name string) error {
	return d.transferWithClass(ctx, "TYPE", targetSchema, schema, name)
}

// TransferXmlSchemaCollection moves an XML schema collection into another
// schema. Its class prefix is the whole three-word noun, not an abbreviation
// of it.
func (d *Database) TransferXmlSchemaCollection(targetSchema, schema, name string) error {
	return d.TransferXmlSchemaCollectionContext(context.Background(), targetSchema, schema, name)
}

// TransferXmlSchemaCollectionContext is the context-aware variant of
// TransferXmlSchemaCollection.
func (d *Database) TransferXmlSchemaCollectionContext(ctx context.Context, targetSchema, schema, name string) error {
	return d.transferWithClass(ctx, "XML SCHEMA COLLECTION", targetSchema, schema, name)
}

// transferWithClass is ALTER SCHEMA ... TRANSFER for a securable that needs a
// class prefix. class is a fixed keyword chosen by the caller here, never
// caller input.
func (d *Database) transferWithClass(ctx context.Context, class, targetSchema, schema, name string) error {
	if targetSchema == "" {
		return fmt.Errorf("gosmo: transfer %s: target schema is required", qualifiedName(schema, name))
	}
	if schema == "" {
		schema = "dbo"
	}
	if strings.EqualFold(targetSchema, schema) {
		return fmt.Errorf("gosmo: transfer %s: it is already in schema [%s]", qualifiedName(schema, name), schema)
	}
	if _, err := d.exec(ctx, fmt.Sprintf("ALTER SCHEMA %s TRANSFER %s::%s",
		quoteIdent(targetSchema), class, qualifiedName(schema, name))); err != nil {
		return fmt.Errorf("gosmo: transfer %s to schema [%s]: %w", qualifiedName(schema, name), targetSchema, err)
	}
	return nil
}

// RenameUserDefinedDataType renames an alias type (sp_rename's
// 'USERDATATYPE' class).
//
// Alias types only. The class is documented as covering "an alias data type
// added by sp_addtype or CREATE TYPE", and it is the whole of what sp_rename
// can rename in sys.types: a table type or a CLR type has no @objtype at
// all, and passing one of those here renames nothing while reporting
// success — so callers must not route them through this method.
//
// newName is a bare name, as everywhere sp_rename is used.
func (d *Database) RenameUserDefinedDataType(schema, oldName, newName string) error {
	return d.RenameUserDefinedDataTypeContext(context.Background(), schema, oldName, newName)
}

// RenameUserDefinedDataTypeContext is the context-aware variant of
// RenameUserDefinedDataType.
func (d *Database) RenameUserDefinedDataTypeContext(ctx context.Context, schema, oldName, newName string) error {
	if schema == "" {
		schema = "dbo"
	}
	if _, err := d.exec(ctx,
		"EXEC sp_rename @objname = @p1, @newname = @p2, @objtype = N'USERDATATYPE'",
		qualifiedName(schema, oldName), newName,
	); err != nil {
		return fmt.Errorf("gosmo: rename type %s -> %q: %w", qualifiedName(schema, oldName), newName, err)
	}
	return nil
}
