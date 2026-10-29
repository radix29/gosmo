package gosmo

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// -- Columns -------------------------------------------------------------------

// Column mirrors Microsoft.SqlServer.Management.Smo.Column.
type Column struct {
	Name            string
	OrdinalPosition int
	DataType        DataType
	MaxLength       int // -1 = MAX
	Precision       int
	Scale           int
	IsNullable      bool
	IsIdentity      bool
	// IdentitySeed and IdentityIncrement are the IDENTITY arguments as the
	// decimal digits the catalog holds, "" when the column is not an
	// identity. They are strings because a decimal(38,0) identity can be
	// seeded past int64 — reading it as an integer failed the whole column
	// listing for the table — and they are only ever rendered back into T-SQL.
	IdentitySeed      string
	IdentityIncrement string
	IsComputed        bool
	ComputedText      string
	DefaultValue      *ColumnDefault
	IsRowGUID         bool
	Collation         string
	IsPrimaryKey      bool

	// TypeSchema is the schema DataType belongs to — "sys" for a built-in
	// type — and IsUserDefinedType is sys.types.is_user_defined. An alias or
	// CLR type's name resolves against the executing user's default schema
	// when it is not qualified, so TypeString qualifies it.
	TypeSchema        string
	IsUserDefinedType bool
	// IsTypeHidden is set when the column's type is a user-defined type the
	// login cannot see — it holds no permission on the type. DataType is then
	// the base system type ("" for a CLR type), TypeSchema is "",
	// IsUserDefinedType is true, and BoundRule/BoundDefault are "" (a binding
	// cannot be told from the type's). Scripting such a column is refused.
	IsTypeHidden bool
	// IsPersisted is sys.computed_columns.is_persisted; false for a column
	// that is not computed.
	IsPersisted bool
	// IdentityNotForReplication is IDENTITY … NOT FOR REPLICATION.
	IdentityNotForReplication bool
	IsSparse                  bool
	// IsColumnSet is an XML column set FOR ALL_SPARSE_COLUMNS.
	IsColumnSet bool
	// MaskingFunction is the dynamic data masking function as written in
	// MASKED WITH (FUNCTION = '…'), unescaped — e.g. "default()" or
	// `partial(1,"X'X",0)`; "" when the column is not masked.
	// sys.masked_columns stores it with each quote already doubled
	// (`partial(1,"X''X",0)`); the read undoes that, so a scripter escaping
	// it into the literal does not double it again on every round trip.
	MaskingFunction string
	// GeneratedAlwaysType is sys.columns.generated_always_type: 0 for an
	// ordinary column, 1 for a system-versioned table's AS ROW START
	// column, 2 for its AS ROW END, and 7 to 10 for a ledger table's
	// TRANSACTION_ID START/END and SEQUENCE_NUMBER START/END columns (SQL
	// Server 2022).
	GeneratedAlwaysType int
	// IsHidden is a generated-always column declared HIDDEN.
	IsHidden bool
	// IsDroppedLedgerColumn is a column dropped from a ledger table, which
	// the ledger keeps under a MSSQL_DroppedLedgerColumn_… name rather than
	// removing. Always false before SQL Server 2022.
	IsDroppedLedgerColumn bool
	// ColumnEncryptionKey, EncryptionType and EncryptionAlgorithm describe
	// an Always Encrypted column: the column encryption key's name,
	// DETERMINISTIC or RANDOMIZED, and the algorithm (always
	// AEAD_AES_256_CBC_HMAC_SHA_256 so far). All three are "" for a column
	// that is not encrypted. DataType and Collation are the plaintext ones
	// the column was declared with.
	ColumnEncryptionKey string
	EncryptionType      string
	EncryptionAlgorithm string
	// IsFileStream is a varbinary(max) FILESTREAM column, whose data lives in
	// the table's FILESTREAM filegroup rather than in the row.
	IsFileStream bool
	// GraphType is sys.columns.graph_type: 0 for an ordinary column, and for
	// a node or edge table's internal columns one of the GraphColumn*
	// values. It is always 0 before SQL Server 2017, which has no graph
	// tables.
	GraphType int
	// XMLSchemaCollectionSchema and XMLSchemaCollection name a typed xml
	// column's schema collection, and IsXMLDocument is its DOCUMENT facet
	// (false is CONTENT). Both names are "" for an untyped xml column and
	// for every other type. An xml column scripted without them is untyped:
	// it accepts any well-formed XML its collection would have rejected.
	XMLSchemaCollectionSchema string
	XMLSchemaCollection       string
	IsXMLDocument             bool
	// VectorDimensions and VectorBaseType describe a vector column (SQL
	// Server 2025): vector(3) has 3 dimensions and base type "float32", the
	// default. Zero and "" for every other type and before 2025.
	VectorDimensions int
	VectorBaseType   string

	// BoundRule and BoundDefault name a legacy rule and default bound to
	// the column itself with sp_bindrule / sp_bindefault, in the schemas
	// BoundRuleSchema and BoundDefaultSchema; all "" when none is. A
	// default constraint is DefaultValue, not this, and a binding the column
	// inherits from its alias type (the same object as the type's) is the
	// type's — UserDefinedDataType.Rule and .Default — and is left out here.
	BoundRuleSchema, BoundRule       string
	BoundDefaultSchema, BoundDefault string
}

// The sys.columns.graph_type values for a graph table's internal columns.
// The two an INSERT into an edge table supplies are the computed $from_id
// and $to_id pseudo-columns.
const (
	GraphColumnID             = 1
	GraphColumnIDComputed     = 2
	GraphColumnFromID         = 3
	GraphColumnFromObjID      = 4
	GraphColumnFromIDComputed = 5
	GraphColumnToID           = 6
	GraphColumnToObjID        = 7
	GraphColumnToIDComputed   = 8
)

// columnSelect is the SELECT and joins every column listing shares; each
// caller appends its own WHERE, because a Table already holds an object_id
// while Database.ObjectColumns has only a name to resolve.
//
// A bound rule or default is matched NULLIF the type's own: a column of an
// alias type carries the type's binding in sys.columns too, and scripting it
// on the column as well would bind it twice. A default constraint is also a
// 'D' object in default_object_id, told apart by having a parent.
//
// graph_type is SQL Server 2017's, with graph tables themselves,
// is_dropped_ledger_column 2022's, with ledger tables, and the vector_*
// columns 2025's, with the vector type. The Always Encrypted and typed-xml
// columns are older than gosmo's 2016 floor and need no gate.
// The type is LEFT JOINed (typeNameSQL): an inner join dropped the column of
// a type the login cannot see. Such a column binds nothing here — its type's
// rule_object_id is unknown, so the column's own binding cannot be told from
// the type's.
// https://learn.microsoft.com/sql/relational-databases/system-catalog-views/sys-columns-transact-sql
func (d *Database) columnSelect() string {
	major := d.serverMajorVersion()
	return `
SELECT c.name, c.column_id,
       ` + typeNameSQL("c") + `,
       c.max_length, c.precision, c.scale,
       c.is_nullable, c.is_identity, c.is_computed,
       ISNULL(cc.definition, ''),
       ISNULL(dc.name, ''), ISNULL(dc.definition, ''),
       c.is_rowguidcol, ISNULL(c.collation_name, ''),
       CONVERT(nvarchar(40), ic.seed_value), CONVERT(nvarchar(40), ic.increment_value),
       CAST(CASE WHEN pk.column_id IS NOT NULL THEN 1 ELSE 0 END AS BIT),
       SCHEMA_NAME(tp.schema_id), ISNULL(tp.is_user_defined, CAST(1 AS bit)),
       ISNULL(cc.is_persisted, 0), ISNULL(ic.is_not_for_replication, 0),
       c.is_sparse, c.is_column_set,
       ISNULL(mc.masking_function, ''),
       c.generated_always_type, c.is_hidden, c.is_filestream,
       ` + colSince(major, SQLServer2017, "ISNULL(c.graph_type, 0)", "CAST(0 AS int)") + `,
       ` + bitSince(major, SQLServer2022, "c.is_dropped_ledger_column") + `,
       ISNULL(cek.name, ''), ISNULL(c.encryption_type_desc, ''), ISNULL(c.encryption_algorithm_name, ''),
       ISNULL(SCHEMA_NAME(xsc.schema_id), ''), ISNULL(xsc.name, ''), c.is_xml_document,
       ` + colSince(major, SQLServer2025, "ISNULL(c.vector_dimensions, 0)", "CAST(0 AS int)") + `,
       ` + colSince(major, SQLServer2025, "ISNULL(c.vector_base_type_desc, '')", "CAST('' AS nvarchar(60))") + `,
       ISNULL(SCHEMA_NAME(br.schema_id), ''), ISNULL(br.name, ''),
       ISNULL(SCHEMA_NAME(bd.schema_id), ''), ISNULL(bd.name, ''),
       ` + typeHiddenSQL + `
FROM   sys.columns c
LEFT   JOIN sys.types tp ON tp.user_type_id = c.user_type_id
LEFT   JOIN sys.objects br
       ON  br.object_id = NULLIF(c.rule_object_id, tp.rule_object_id) AND tp.user_type_id IS NOT NULL
LEFT   JOIN sys.objects bd
       ON  bd.object_id = NULLIF(c.default_object_id, tp.default_object_id) AND bd.parent_object_id = 0
       AND tp.user_type_id IS NOT NULL
LEFT   JOIN sys.xml_schema_collections xsc
       ON  xsc.xml_collection_id = NULLIF(c.xml_collection_id, 0)
LEFT   JOIN sys.column_encryption_keys cek
       ON  cek.column_encryption_key_id = c.column_encryption_key_id
LEFT   JOIN sys.masked_columns mc
       ON  mc.object_id  = c.object_id AND mc.column_id = c.column_id AND mc.is_masked = 1
LEFT   JOIN sys.computed_columns cc
       ON  cc.object_id  = c.object_id AND cc.column_id = c.column_id
LEFT   JOIN sys.default_constraints dc
       ON  dc.parent_object_id = c.object_id AND dc.parent_column_id = c.column_id
LEFT   JOIN sys.identity_columns ic
       ON  ic.object_id  = c.object_id AND ic.column_id = c.column_id
LEFT   JOIN (
       SELECT ic2.object_id, ic2.column_id
       FROM   sys.index_columns ic2
       JOIN   sys.indexes i ON i.object_id = ic2.object_id AND i.index_id = ic2.index_id
       WHERE  i.is_primary_key = 1
       ) pk ON pk.object_id = c.object_id AND pk.column_id = c.column_id`
}

// Columns returns all columns for this table in ordinal order.
func (t *Table) Columns(ctx context.Context) ([]*Column, error) {
	if err := t.requireLoaded("list columns for"); err != nil {
		return nil, err
	}
	q := t.db.columnSelect() + `
WHERE  c.object_id = @p1
ORDER  BY c.column_id`

	rows, err := t.db.query(ctx, q, t.ObjectID)
	if err != nil {
		return nil, fmt.Errorf("gosmo: list columns for %s: %w", t.FullName(), err)
	}
	defer rows.Close()

	cols, err := scanColumns(rows.Rows)
	if err != nil {
		return nil, fmt.Errorf("gosmo: list columns for %s: %w", t.FullName(), err)
	}
	return cols, nil
}

// ObjectColumns returns the columns of the table or view schema.name, in
// ordinal order. Table.Columns covers tables only, and a view has no handle
// type of its own that carries an object_id, so this is the way to reach a
// view's columns — which do carry permissions, and so do turn up on a
// Securables page.
//
// The columns a view does not have — identity, computed text, defaults,
// primary key — come back at their zero values, because the joins that
// supply them simply do not match for a view. Name, ordinal, type,
// length/precision/scale, nullability and collation are all real.
func (d *Database) ObjectColumns(ctx context.Context, schema, name string) ([]*Column, error) {
	if err := requireSchema("object columns", schema, name); err != nil {
		return nil, err
	}
	q := d.columnSelect() + `
WHERE  c.object_id = OBJECT_ID(@p1)
ORDER  BY c.column_id`

	ref := qualifiedName(schema, name)
	rows, err := d.query(ctx, q, ref)
	if err != nil {
		return nil, fmt.Errorf("gosmo: list columns for %s in %q: %w", ref, d.Name, err)
	}
	defer rows.Close()

	cols, err := scanColumns(rows.Rows)
	if err != nil {
		return nil, fmt.Errorf("gosmo: list columns for %s in %q: %w", ref, d.Name, err)
	}
	// Every table and view has at least one column, so an empty result means
	// OBJECT_ID found nothing — report that rather than an empty column list,
	// which reads as "this object has no columns".
	if len(cols) == 0 {
		return nil, notFoundf("gosmo: table or view %s not found in %q", ref, d.Name)
	}
	return cols, nil
}

// scanColumns reads the column shape columnSelect returns.
func scanColumns(rows *sql.Rows) ([]*Column, error) {
	var cols []*Column
	for rows.Next() {
		col := &Column{}
		var compText, dcName, dcDef, collation, typeSchema sql.NullString
		var seed, increment sql.NullString
		if err := rows.Scan(
			&col.Name, &col.OrdinalPosition,
			&col.DataType, &col.MaxLength, &col.Precision, &col.Scale,
			&col.IsNullable, &col.IsIdentity, &col.IsComputed,
			&compText, &dcName, &dcDef,
			&col.IsRowGUID, &collation,
			&seed, &increment,
			&col.IsPrimaryKey,
			&typeSchema, &col.IsUserDefinedType,
			&col.IsPersisted, &col.IdentityNotForReplication,
			&col.IsSparse, &col.IsColumnSet,
			&col.MaskingFunction,
			&col.GeneratedAlwaysType, &col.IsHidden, &col.IsFileStream,
			&col.GraphType, &col.IsDroppedLedgerColumn,
			&col.ColumnEncryptionKey, &col.EncryptionType, &col.EncryptionAlgorithm,
			&col.XMLSchemaCollectionSchema, &col.XMLSchemaCollection, &col.IsXMLDocument,
			&col.VectorDimensions, &col.VectorBaseType,
			&col.BoundRuleSchema, &col.BoundRule, &col.BoundDefaultSchema, &col.BoundDefault,
			&col.IsTypeHidden,
		); err != nil {
			return nil, err
		}
		col.MaskingFunction = strings.ReplaceAll(col.MaskingFunction, "''", "'")
		col.ComputedText = compText.String
		col.Collation = collation.String
		col.TypeSchema = typeSchema.String
		if dcName.String != "" {
			col.DefaultValue = &ColumnDefault{Name: dcName.String, Definition: dcDef.String}
		}
		col.IdentitySeed = seed.String
		col.IdentityIncrement = increment.String
		cols = append(cols, col)
	}
	return cols, rows.Err()
}

// AlterColumn changes an existing column's data type, nullability and/or
// collation (ALTER TABLE ... ALTER COLUMN). Identity and default are not
// settable this way — SQL Server requires dropping and re-adding the column,
// or its default constraint, for those.
//
// ALTER COLUMN resets every attribute the statement does not restate, with
// no error and no warning, so this reads the column from sys.columns first
// and restates what it has. A column the read does not find is ErrNotFound.
//
//   - A string column keeps its collation unless col.Collation names
//     another: with no COLLATE a char/varchar/nchar/nvarchar column takes the
//     database default, so changing only the length of a Latin1_General_CS_AS
//     column in a CI database would make it case-insensitive (and can break
//     an index or foreign key that depends on it). SSMS's designer restates
//     it too; a column that had none (a non-string column becoming a string
//     one) takes the database default.
//   - A SPARSE column stays sparse. A sparse column must be nullable, so
//     asking for NOT NULL on one is refused (ErrInvalidRequest); drop SPARSE
//     first.
//   - A dynamic data mask is re-applied (ALTER COLUMN ... ADD MASKED) in the
//     same atomic batch as the ALTER, so no reader without UNMASK sees the
//     column unmasked in between, and a mask the new type cannot take fails
//     the whole change. Without it, widening a masked column would expose
//     its clear values.
//   - A column both SPARSE and masked is refused on SQL Server 2016
//     (ErrUnsupported) before anything runs: there ADD MASKED clears SPARSE
//     and ADD SPARSE clears the mask, so no ALTER keeps both. 2017 and later
//     keep both.
//
// Changing or removing the sparse format or the mask is not this method's
// job: neither is carried in ColumnDefinition, and both are kept.
func (t *Table) AlterColumn(ctx context.Context, col ColumnDefinition) error {
	if col.Name == "" {
		return invalidf("gosmo: alter column: name is required")
	}
	if err := checkColumnDefinition(col); err != nil {
		return fmt.Errorf("gosmo: alter column %q: %w", col.Name, err)
	}
	current, err := t.columnAttributes(ctx, col.Name)
	if err != nil {
		return err
	}
	if col.Collation == "" && collatable(col.DataType) {
		col.Collation = current.collation
	}
	if current.sparse && !col.IsNullable {
		return invalidf("gosmo: alter column %q on %s: the column is SPARSE, which requires NULL; drop SPARSE first",
			col.Name, t.FullName())
	}
	if current.sparse && current.mask != "" && t.db.before(SQLServer2017) {
		return unsupportedf("gosmo: alter column %q on %s: the column is SPARSE and masked, and SQL Server 2016 cannot keep both through ALTER COLUMN",
			col.Name, t.FullName())
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "ALTER TABLE %s ALTER COLUMN %s %s%s",
		t.FullName(), quoteIdent(col.Name), colTypeSQL(col), collateClause(col.Collation))
	if current.sparse {
		// SPARSE goes before NULL: "NULL SPARSE" is Msg 102.
		sb.WriteString(" SPARSE")
	}
	if col.IsNullable {
		sb.WriteString(" NULL")
	} else {
		sb.WriteString(" NOT NULL")
	}

	if current.mask == "" {
		err = t.exec(ctx, sb.String())
	} else {
		err = t.db.execAtomic(ctx, []string{sb.String(),
			fmt.Sprintf("ALTER TABLE %s ALTER COLUMN %s ADD MASKED WITH (FUNCTION = %s)",
				t.FullName(), quoteIdent(col.Name), QuoteLiteral(current.mask))})
	}
	if err != nil {
		return fmt.Errorf("gosmo: alter column %q on %s: %w", col.Name, t.FullName(), err)
	}
	return nil
}

// columnAttrs is what AlterColumn restates of a column: its collation (""
// for none), whether it is SPARSE, and its masking function, unescaped (""
// when it is not masked).
type columnAttrs struct {
	collation string
	sparse    bool
	mask      string
}

// columnAttributes reads a column's current attributes for AlterColumn. The
// name is compared with sys.columns.name in the catalog collation, which is
// how ALTER COLUMN resolves it. sys.masked_columns exists from 2016, gosmo's
// floor, so it needs no version gate.
func (t *Table) columnAttributes(ctx context.Context, name string) (columnAttrs, error) {
	var a columnAttrs
	var collation, mask sql.NullString
	err := t.db.queryRow(ctx, func(row *sql.Row) error { return row.Scan(&collation, &a.sparse, &mask) }, `
SELECT c.collation_name, c.is_sparse, mc.masking_function
FROM   sys.columns c
LEFT   JOIN sys.masked_columns mc
       ON  mc.object_id = c.object_id AND mc.column_id = c.column_id AND mc.is_masked = 1
WHERE  c.object_id = OBJECT_ID(@p1) AND c.name = @p2`, t.FullName(), name)
	a.collation = collation.String
	// Stored with each quote already doubled; see Column.MaskingFunction.
	a.mask = strings.ReplaceAll(mask.String, "''", "'")
	return foundRow(a, err,
		notFoundf("gosmo: alter column %q: no such column on %s", name, t.FullName()),
		fmt.Sprintf("alter column %q on %s: read current attributes", name, t.FullName()))
}

// DropColumn removes a column from the table (ALTER TABLE ... DROP COLUMN).
//
// Bare, like every other Drop in this package: SQL Server refuses a column a
// default constraint, index, check constraint or statistic depends on, and
// that refusal is the answer — dropping the dependencies first is a decision
// the caller makes, not one a library can make for them. The data in the
// column goes with it and is not recoverable.
func (t *Table) DropColumn(ctx context.Context, name string) error {
	if name == "" {
		return invalidf("gosmo: drop column on %s: name is required", t.FullName())
	}
	if err := t.exec(ctx,
		fmt.Sprintf("ALTER TABLE %s DROP COLUMN %s", t.FullName(), quoteIdent(name))); err != nil {
		return fmt.Errorf("gosmo: drop column %q on %s: %w", name, t.FullName(), err)
	}
	return nil
}

// RenameColumn renames a column using sp_rename's 'COLUMN' class.
//
// Bare, like the rest of this family: sp_rename does not update anything that
// names the column. Views, procedures, functions, computed columns, indexes
// with a filter predicate and check constraints keep the old name in their
// definitions and break at their next use, and SQL Server reports nothing at
// rename time beyond its standing caution. Deciding whether that is
// acceptable is the caller's.
//
// newName is a bare name: sp_rename refuses a qualified one for the new name,
// while @objname must be the three-part table.column form, which this builds.
func (t *Table) RenameColumn(ctx context.Context, name, newName string) error {
	if name == "" || newName == "" {
		return invalidf("gosmo: rename column on %s: both names are required", t.FullName())
	}
	objName := t.FullName() + "." + quoteIdent(name)
	if err := t.exec(ctx,
		"EXEC sp_rename @objname = @p1, @newname = @p2, @objtype = N'COLUMN'",
		objName, newName,
	); err != nil {
		return fmt.Errorf("gosmo: rename column %q to %q on %s: %w", name, newName, t.FullName(), err)
	}
	return nil
}

// -- Column type builder -------------------------------------------------------

// colTypeSQL returns the T-SQL data-type fragment for a ColumnDefinition.
// Callers must validate col.DataType (see validDataType) before calling this
// — it trusts its input and does not itself reject an unrecognized type.
// column_type.go's Column.TypeString does the equivalent for a *Column (from
// sys.columns), which uses different field names.
func colTypeSQL(col ColumnDefinition) string {
	switch col.DataType {
	case DataTypeVarChar, DataTypeChar, DataTypeBinary, DataTypeVarBinary,
		DataTypeNVarChar, DataTypeNChar:
		switch col.MaxLength {
		case -1:
			return fmt.Sprintf("%s(MAX)", col.DataType)
		case 0:
			return string(col.DataType)
		default:
			return fmt.Sprintf("%s(%d)", col.DataType, col.MaxLength)
		}
	case DataTypeDecimal, DataTypeNumeric:
		switch {
		case col.Precision != nil && col.Scale != nil:
			return fmt.Sprintf("%s(%d,%d)", col.DataType, *col.Precision, *col.Scale)
		case col.Precision != nil:
			return fmt.Sprintf("%s(%d)", col.DataType, *col.Precision)
		}
	case DataTypeDatetime2, DataTypeTime, DataTypeDatetimeOffset:
		if col.Scale != nil {
			return fmt.Sprintf("%s(%d)", col.DataType, *col.Scale)
		}
	case DataTypeXML, DataTypeVector:
		// The same spelling ScriptTable gives a column read back.
		return catalogType{
			dt:        col.DataType,
			xmlSchema: col.XMLSchemaCollectionSchema, xmlCollection: col.XMLSchemaCollection, xmlDocument: col.IsXMLDocument,
			vectorDimensions: col.VectorDimensions, vectorBaseType: col.VectorBaseType,
		}.String()
	}
	return string(col.DataType)
}

// collatable reports whether a column of type dt has a collation.
func collatable(dt DataType) bool {
	switch dt {
	case DataTypeChar, DataTypeVarChar, DataTypeText,
		DataTypeNChar, DataTypeNVarChar, DataTypeNText:
		return true
	}
	return false
}

// validCollationName reports whether s can follow COLLATE unquoted. COLLATE
// takes a bare name, not a quoted identifier, and every SQL Server collation
// name (Windows or SQL_) is letters, digits and underscores, so anything else
// is refused rather than written into the statement.
func validCollationName(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !(r == '_' || r >= '0' && r <= '9' || r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z') {
			return false
		}
	}
	return true
}

// collateClause is " COLLATE <name>", or "" for no collation; the name was
// checked by checkColumnDefinition or read from sys.columns.
func collateClause(collation string) string {
	if collation == "" {
		return ""
	}
	return " COLLATE " + collation
}

// checkIdentity refuses an IDENTITY column the server would refuse, before
// anything is sent: an increment of 0 (Msg 2753; both zero is the bare
// IDENTITY, not this), a type that is not an integer or a decimal/numeric of scale 0
// (Msg 2749), a nullable column (Msg 8147) and a DEFAULT (Msg 1754).
func checkIdentity(col ColumnDefinition) error {
	if !col.IsIdentity {
		return nil
	}
	switch col.DataType {
	case DataTypeTinyInt, DataTypeSmallInt, DataTypeInt, DataTypeBigInt:
	case DataTypeDecimal, DataTypeNumeric:
		if col.Scale != nil && *col.Scale != 0 {
			return invalidf("an identity %s needs a scale of 0", col.DataType)
		}
	default:
		return invalidf("%s cannot be an identity", col.DataType)
	}
	switch {
	case col.IdentityIncr == 0 && col.IdentitySeed != 0:
		return invalidf("an identity increment cannot be 0")
	case col.IsNullable:
		return invalidf("an identity column cannot be nullable")
	case col.DefaultValue != "":
		return invalidf("an identity column cannot have a default")
	}
	return nil
}

// checkColumnDefinition refuses a definition colTypeSQL cannot render as
// written, rather than rendering something else: a decimal scale with no
// precision has no T-SQL spelling (decimal(,2) is a syntax error), and a
// precision or scale on a type that takes neither would be dropped silently.
// The same holds for a schema collection off an xml column and dimensions
// off a vector, and a vector without dimensions does not parse (Msg 2715).
func checkColumnDefinition(col ColumnDefinition) error {
	if !validDataType(col.DataType) {
		return invalidf("unrecognized data type %q", col.DataType)
	}
	if col.DataType != DataTypeXML &&
		(col.XMLSchemaCollectionSchema != "" || col.XMLSchemaCollection != "" || col.IsXMLDocument) {
		return invalidf("%s takes no XML schema collection", col.DataType)
	}
	if col.DataType != DataTypeVector && (col.VectorDimensions != 0 || col.VectorBaseType != "") {
		return invalidf("%s takes no vector dimensions or base type", col.DataType)
	}
	if col.Collation != "" {
		if !collatable(col.DataType) {
			return invalidf("%s takes no collation", col.DataType)
		}
		if !validCollationName(col.Collation) {
			return invalidf("invalid collation name %q", col.Collation)
		}
	}
	switch col.DataType {
	case DataTypeXML:
		switch {
		case col.XMLSchemaCollection == "" && (col.XMLSchemaCollectionSchema != "" || col.IsXMLDocument):
			return invalidf("an xml schema or DOCUMENT facet needs a schema collection")
		case col.XMLSchemaCollection != "" && col.XMLSchemaCollectionSchema == "":
			return fmt.Errorf("xml schema collection %q: %w", col.XMLSchemaCollection, ErrSchemaRequired)
		}
	case DataTypeVector:
		if col.VectorDimensions <= 0 {
			return invalidf("a vector needs a positive number of dimensions")
		}
		switch strings.ToLower(col.VectorBaseType) {
		case "", "float32", "float16":
		default:
			return invalidf("unrecognized vector base type %q", col.VectorBaseType)
		}
	}
	switch col.DataType {
	case DataTypeDecimal, DataTypeNumeric:
		if col.Scale != nil && col.Precision == nil {
			return invalidf("a %s scale needs a precision", col.DataType)
		}
	case DataTypeDatetime2, DataTypeTime, DataTypeDatetimeOffset:
		if col.Precision != nil {
			return invalidf("%s takes a scale, not a precision", col.DataType)
		}
	default:
		if col.Precision != nil || col.Scale != nil {
			return invalidf("%s takes no precision or scale", col.DataType)
		}
	}
	return nil
}
