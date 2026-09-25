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
	"fmt"
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
func (d *Database) UserDefinedDataTypes(ctx context.Context) ([]*UserDefinedDataType, error) {
	const q = userDefinedDataTypeSelect + `
ORDER  BY SCHEMA_NAME(t.schema_id), t.name`

	rows, err := d.query(ctx, q)
	return scanRows(rows, err, fmt.Sprintf("list user-defined data types in %q", d.Name), func(scan func(...any) error) (*UserDefinedDataType, error) {
		return scanUserDefinedDataType(d, scan)
	})
}

// UserDefinedDataTypeByName returns one alias type, or a not-found error
// (errors.Is ErrNotFound) when the database has none by that name.
func (d *Database) UserDefinedDataTypeByName(ctx context.Context, schema, name string) (*UserDefinedDataType, error) {
	if err := requireSchema("user defined data type by name", schema, name); err != nil {
		return nil, err
	}
	var t *UserDefinedDataType
	err := d.queryRow(ctx, func(row *sql.Row) error {
		var err error
		t, err = scanUserDefinedDataType(d, row.Scan)
		return err
	}, userDefinedDataTypeSelect+`
   AND SCHEMA_NAME(t.schema_id) = @p1 AND t.name = @p2`, schema, name)
	return foundRow(t, err, notFoundf("gosmo: user-defined data type %s not found in %q", qualifiedName(schema, name), d.Name), fmt.Sprintf("read user-defined data type %s in %q", qualifiedName(schema, name), d.Name))
}

// UserDefinedDataTypeRef returns a lightweight handle for an alias type by name, without
// querying the catalog — the counterpart of Server.DatabaseRef. Every field
// but the schema and name stays at its zero value; UserDefinedDataTypeByName is what populates them.
//
// Every write on *UserDefinedDataType addresses it by name, so this handle is enough to
// drop one the caller already knows exists — and is the form to use when
// there is nothing to read yet, such as a script of a CREATE that was only
// collected.
//
// schema is taken as given: an empty one is refused by the handle's writes
// (ErrSchemaRequired), never defaulted.
func (d *Database) UserDefinedDataTypeRef(schema, name string) *UserDefinedDataType {
	return &UserDefinedDataType{db: d, Schema: schema, Name: name}
}

// Drop drops the alias type.
func (t *UserDefinedDataType) Drop(ctx context.Context) error {
	return dropType(ctx, t.db, t.Schema, t.Name)
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
	// a caller could reach by name — see Columns.
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
func (d *Database) UserDefinedTableTypes(ctx context.Context) ([]*UserDefinedTableType, error) {
	const q = userDefinedTableTypeSelect + `
ORDER  BY SCHEMA_NAME(tt.schema_id), tt.name`

	rows, err := d.query(ctx, q)
	return scanRows(rows, err, fmt.Sprintf("list user-defined table types in %q", d.Name), func(scan func(...any) error) (*UserDefinedTableType, error) {
		return scanUserDefinedTableType(d, scan)
	})
}

// UserDefinedTableTypeByName returns one table type, or a not-found error
// (errors.Is ErrNotFound) when the database has none by that name.
func (d *Database) UserDefinedTableTypeByName(ctx context.Context, schema, name string) (*UserDefinedTableType, error) {
	if err := requireSchema("user defined table type by name", schema, name); err != nil {
		return nil, err
	}
	var t *UserDefinedTableType
	err := d.queryRow(ctx, func(row *sql.Row) error {
		var err error
		t, err = scanUserDefinedTableType(d, row.Scan)
		return err
	}, userDefinedTableTypeSelect+`
   AND SCHEMA_NAME(tt.schema_id) = @p1 AND tt.name = @p2`, schema, name)
	return foundRow(t, err, notFoundf("gosmo: user-defined table type %s not found in %q", qualifiedName(schema, name), d.Name), fmt.Sprintf("read user-defined table type %s in %q", qualifiedName(schema, name), d.Name))
}

// UserDefinedTableTypeRef returns a lightweight handle for a table type by name, without
// querying the catalog — the counterpart of Server.DatabaseRef. Every field
// but the schema and name stays at its zero value; UserDefinedTableTypeByName is what populates them.
//
// Every write on *UserDefinedTableType addresses it by name, so this handle is enough to
// drop one the caller already knows exists — and is the form to use when
// there is nothing to read yet, such as a script of a CREATE that was only
// collected.
//
// schema is taken as given: an empty one is refused by the handle's writes
// (ErrSchemaRequired), never defaulted.
func (d *Database) UserDefinedTableTypeRef(schema, name string) *UserDefinedTableType {
	return &UserDefinedTableType{db: d, Schema: schema, Name: name}
}

// Columns returns the table type's columns in ordinal order.
//
// The columns are read through TypeTableObjectID, the internal table
// sys.table_types points at — a table type's columns are *not* on
// sys.columns under its user_type_id, and OBJECT_ID('[schema].[name]') does
// not resolve a type at all, so neither of the obvious lookups finds anything.
// A type built by hand rather than by a listing has a zero TypeTableObjectID
// and gets a not-found error rather than an empty list.
func (t *UserDefinedTableType) Columns(ctx context.Context) ([]*Column, error) {
	if t.TypeTableObjectID == 0 {
		return nil, notFoundf("gosmo: user-defined table type %s in %q has no internal table id — read it with UserDefinedTableTypeByName",
			t.FullName(), t.db.Name)
	}
	q := t.db.columnSelect() + `
WHERE  c.object_id = @p1
ORDER  BY c.column_id`

	rows, err := t.db.query(ctx, q, t.TypeTableObjectID)
	if err != nil {
		return nil, fmt.Errorf("gosmo: list columns for table type %s in %q: %w", t.FullName(), t.db.Name, err)
	}
	defer rows.Close()

	cols, err := scanColumns(rows.Rows)
	if err != nil {
		return nil, fmt.Errorf("gosmo: list columns for table type %s in %q: %w", t.FullName(), t.db.Name, err)
	}
	return cols, nil
}

// Drop drops the table type.
func (t *UserDefinedTableType) Drop(ctx context.Context) error {
	return dropType(ctx, t.db, t.Schema, t.Name)
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
func (d *Database) ClrTypes(ctx context.Context) ([]*ClrType, error) {
	const q = clrTypeSelect + `
ORDER  BY SCHEMA_NAME(t.schema_id), t.name`

	rows, err := d.query(ctx, q)
	return scanRows(rows, err, fmt.Sprintf("list CLR types in %q", d.Name), func(scan func(...any) error) (*ClrType, error) {
		return scanClrType(d, scan)
	})
}

// ClrTypeByName returns one CLR type, or a not-found error (errors.Is
// ErrNotFound) when the database has none by that name.
func (d *Database) ClrTypeByName(ctx context.Context, schema, name string) (*ClrType, error) {
	if err := requireSchema("clr type by name", schema, name); err != nil {
		return nil, err
	}
	var t *ClrType
	err := d.queryRow(ctx, func(row *sql.Row) error {
		var err error
		t, err = scanClrType(d, row.Scan)
		return err
	}, clrTypeSelect+`
   AND SCHEMA_NAME(t.schema_id) = @p1 AND t.name = @p2`, schema, name)
	return foundRow(t, err, notFoundf("gosmo: CLR type %s not found in %q", qualifiedName(schema, name), d.Name), fmt.Sprintf("read CLR type %s in %q", qualifiedName(schema, name), d.Name))
}

// ClrTypeRef returns a lightweight handle for a CLR type by name, without
// querying the catalog — the counterpart of Server.DatabaseRef. Every field
// but the schema and name stays at its zero value; ClrTypeByName is what populates them.
//
// Every write on *ClrType addresses it by name, so this handle is enough to
// drop one the caller already knows exists — and is the form to use when
// there is nothing to read yet, such as a script of a CREATE that was only
// collected.
//
// schema is taken as given: an empty one is refused by the handle's writes
// (ErrSchemaRequired), never defaulted.
func (d *Database) ClrTypeRef(schema, name string) *ClrType {
	return &ClrType{db: d, Schema: schema, Name: name}
}

// Drop drops the CLR type.
func (t *ClrType) Drop(ctx context.Context) error {
	return dropType(ctx, t.db, t.Schema, t.Name)
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
func (d *Database) SystemDataTypes(ctx context.Context) ([]*SystemDataType, error) {
	const q = `
SELECT t.name, t.system_type_id, t.max_length, t.precision, t.scale,
       ISNULL(t.is_nullable, 0)
FROM   sys.types t
WHERE  t.is_user_defined = 0
ORDER  BY t.name`

	rows, err := d.query(ctx, q)
	return scanRows(rows, err, fmt.Sprintf("list system data types in %q", d.Name), func(scan func(...any) error) (*SystemDataType, error) {
		t := &SystemDataType{}
		if err := scan(&t.Name, &t.SystemType, &t.MaxLength,
			&t.Precision, &t.Scale, &t.IsNullable); err != nil {
			return nil, err
		}
		return t, nil
	})
}

// ============================================================
// XML schema collections
// ============================================================

// XMLSchemaCollection mirrors a sys.xml_schema_collections row.
type XMLSchemaCollection struct {
	db *Database

	Name         string
	Schema       string
	CollectionID int
	CreateDate   time.Time
	ModifyDate   time.Time
}

// FullName returns the schema-qualified, bracket-quoted name.
func (c *XMLSchemaCollection) FullName() string { return qualifiedName(c.Schema, c.Name) }

// Database returns the database the collection belongs to.
func (c *XMLSchemaCollection) Database() *Database { return c.db }

// xmlSchemaCollectionSelect excludes the sys schema, which holds the
// server's own sys.sys collection in every database and is not the user's.
const xmlSchemaCollectionSelect = `
SELECT x.name, SCHEMA_NAME(x.schema_id), x.xml_collection_id,
       x.create_date, x.modify_date
FROM   sys.xml_schema_collections x
WHERE  SCHEMA_NAME(x.schema_id) <> 'sys'`

func scanXMLSchemaCollection(d *Database, scan func(...any) error) (*XMLSchemaCollection, error) {
	c := &XMLSchemaCollection{db: d}
	if err := scan(&c.Name, &c.Schema, &c.CollectionID,
		&c.CreateDate, &c.ModifyDate); err != nil {
		return nil, err
	}
	return c, nil
}

// XMLSchemaCollections returns the XML schema collections in the database.
func (d *Database) XMLSchemaCollections(ctx context.Context) ([]*XMLSchemaCollection, error) {
	const q = xmlSchemaCollectionSelect + `
ORDER  BY SCHEMA_NAME(x.schema_id), x.name`

	rows, err := d.query(ctx, q)
	return scanRows(rows, err, fmt.Sprintf("list XML schema collections in %q", d.Name), func(scan func(...any) error) (*XMLSchemaCollection, error) {
		return scanXMLSchemaCollection(d, scan)
	})
}

// XMLSchemaCollectionByName returns one XML schema collection, or a
// not-found error (errors.Is ErrNotFound) when the database has none by that
// name.
func (d *Database) XMLSchemaCollectionByName(ctx context.Context, schema, name string) (*XMLSchemaCollection, error) {
	if err := requireSchema("XML schema collection by name", schema, name); err != nil {
		return nil, err
	}
	var c *XMLSchemaCollection
	err := d.queryRow(ctx, func(row *sql.Row) error {
		var err error
		c, err = scanXMLSchemaCollection(d, row.Scan)
		return err
	}, xmlSchemaCollectionSelect+`
   AND SCHEMA_NAME(x.schema_id) = @p1 AND x.name = @p2`, schema, name)
	return foundRow(c, err, notFoundf("gosmo: XML schema collection %s not found in %q", qualifiedName(schema, name), d.Name), fmt.Sprintf("read XML schema collection %s in %q", qualifiedName(schema, name), d.Name))
}

// XMLSchemaCollectionRef returns a lightweight handle for an XML schema collection by name, without
// querying the catalog — the counterpart of Server.DatabaseRef. Every field
// but the schema and name stays at its zero value; XMLSchemaCollectionByName is what populates them.
//
// Every write on *XMLSchemaCollection addresses it by name, so this handle is enough to
// drop one the caller already knows exists — and is the form to use when
// there is nothing to read yet, such as a script of a CREATE that was only
// collected.
//
// schema is taken as given: an empty one is refused by the handle's writes
// (ErrSchemaRequired), never defaulted.
func (d *Database) XMLSchemaCollectionRef(schema, name string) *XMLSchemaCollection {
	return &XMLSchemaCollection{db: d, Schema: schema, Name: name}
}

// Definition returns the collection's schema documents as one XML string —
// what CREATE XML SCHEMA COLLECTION was given, as the server reassembles it.
//
// XML_SCHEMA_NAMESPACE takes the schema and collection name as *string
// literals*, not identifiers, so both are passed as parameters rather than
// bracket-quoted into the statement.
func (c *XMLSchemaCollection) Definition(ctx context.Context) (string, error) {
	const q = `SELECT CAST(XML_SCHEMA_NAMESPACE(@p1, @p2) AS NVARCHAR(MAX))`

	var def sql.NullString
	err := c.db.queryRow(ctx, func(row *sql.Row) error {
		return row.Scan(&def)
	}, q, c.Schema, c.Name)
	return foundRow(def.String, err, notFoundf("gosmo: XML schema collection %s not found in %q", c.FullName(), c.db.Name), fmt.Sprintf("read XML schema collection %s in %q", c.FullName(), c.db.Name))
}

// Drop drops the XML schema collection.
func (c *XMLSchemaCollection) Drop(ctx context.Context) error {
	if err := requireSchema("drop XML schema collection", c.Schema, c.Name); err != nil {
		return err
	}
	if _, err := c.db.exec(ctx, "DROP XML SCHEMA COLLECTION "+qualifiedName(c.Schema, c.Name)); err != nil {
		return fmt.Errorf("gosmo: drop XML schema collection %s: %w", qualifiedName(c.Schema, c.Name), err)
	}
	return nil
}

// ============================================================
// Drops by name
// ============================================================

// dropType is the Drop of the alias, table and CLR type families — all three
// are DROP TYPE, and nothing in the statement distinguishes them. A type
// still referenced by a column, parameter or function is refused by the
// server; that error is the caller's to report.
func dropType(ctx context.Context, d *Database, schema, name string) error {
	if err := requireSchema("drop type", schema, name); err != nil {
		return err
	}
	if _, err := d.exec(ctx, "DROP TYPE "+qualifiedName(schema, name)); err != nil {
		return fmt.Errorf("gosmo: drop type %s: %w", qualifiedName(schema, name), err)
	}
	return nil
}

// ============================================================
// Renames and schema transfers
// ============================================================

// Rename renames the alias type (sp_rename's 'USERDATATYPE' class). newName
// is a bare name. It is the only type family with a rename: sp_rename has no
// class for a table type or a CLR type, so UserDefinedTableType and ClrType
// have none.
func (t *UserDefinedDataType) Rename(ctx context.Context, newName string) error {
	if err := t.db.renameSchemaObject(ctx, "type", renameAliasTypeClass, t.Schema, t.Name, newName); err != nil {
		return err
	}
	setIfApplied(ctx, &t.Name, newName)
	return nil
}

// Transfer moves the alias type into another schema (ALTER SCHEMA ...
// TRANSFER TYPE::). The type keeps its name; permissions granted on it
// directly are dropped by the server.
func (t *UserDefinedDataType) Transfer(ctx context.Context, targetSchema string) error {
	if err := t.db.transferSchemaObject(ctx, "type", transferTypeClass, targetSchema, t.Schema, t.Name); err != nil {
		return err
	}
	setIfApplied(ctx, &t.Schema, targetSchema)
	return nil
}

// Transfer moves the table type into another schema (ALTER SCHEMA ...
// TRANSFER TYPE::). The type keeps its name; permissions granted on it
// directly are dropped by the server.
func (t *UserDefinedTableType) Transfer(ctx context.Context, targetSchema string) error {
	if err := t.db.transferSchemaObject(ctx, "type", transferTypeClass, targetSchema, t.Schema, t.Name); err != nil {
		return err
	}
	setIfApplied(ctx, &t.Schema, targetSchema)
	return nil
}

// Transfer moves the CLR type into another schema (ALTER SCHEMA ... TRANSFER
// TYPE::). The type keeps its name; permissions granted on it directly are
// dropped by the server.
func (t *ClrType) Transfer(ctx context.Context, targetSchema string) error {
	if err := t.db.transferSchemaObject(ctx, "type", transferTypeClass, targetSchema, t.Schema, t.Name); err != nil {
		return err
	}
	setIfApplied(ctx, &t.Schema, targetSchema)
	return nil
}

// Transfer moves the XML schema collection into another schema (ALTER
// SCHEMA ... TRANSFER XML SCHEMA COLLECTION::). There is no rename: sp_rename
// has no class for one.
func (c *XMLSchemaCollection) Transfer(ctx context.Context, targetSchema string) error {
	if err := c.db.transferSchemaObject(ctx, "XML schema collection", transferXMLSchemaCollectionClass, targetSchema, c.Schema, c.Name); err != nil {
		return err
	}
	setIfApplied(ctx, &c.Schema, targetSchema)
	return nil
}
