package gosmo

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// ============================================================
// Table
// ============================================================

// Table mirrors Microsoft.SqlServer.Management.Smo.Table.
type Table struct {
	db                   *Database
	ObjectID             int
	Schema               string
	Name                 string
	CreateDate           time.Time
	ModifyDate           time.Time
	HasReplicationFilter bool
	IsMemoryOptimized    bool

	// The four flags that decide which family a table belongs to — see
	// TableKind, which is how a caller asks for one family at a time.
	// IsSystem is sys.tables.is_ms_shipped: a table SQL Server itself
	// created, msdb's own hundred and forty-odd included.
	IsSystem    bool
	IsFileTable bool
	IsExternal  bool
	IsNode      bool
	IsEdge      bool
}

// TableRef returns a lightweight handle to a table by name, without a query.
// Nothing verifies that the table exists, and every field but Schema and Name
// stays at its zero value — ObjectID included.
//
// That is the limit of what this handle is for: the methods it serves are the
// name-only ones, which name the table in the statement text (DropConstraint,
// Rename, the ALTER-style writes). Every method that queries by ObjectID —
// Columns, Indexes, Statistics, Triggers, Partitions, the size and detail
// reads — would find object 0 and return nothing, so those need a Table from
// Tables/TableByName instead.
//
// Like Server.DatabaseRef, it is also the only form that works before the
// table exists — a CREATE TABLE a WithScript context merely collected is not
// in the catalog to find.
//
// schema is taken as given: an empty one is refused by every write on the
// handle (ErrSchemaRequired), never defaulted — see Table.exec.
func (d *Database) TableRef(schema, name string) *Table {
	return &Table{db: d, Schema: schema, Name: name}
}

// exec is every write on a table, index or statistic: the statement names
// the table by FullName, and a table with no schema — only a TableRef can be
// one — would name it unqualified, resolving through the caller's default
// schema. It is refused here instead; see requireSchema.
func (t *Table) exec(ctx context.Context, q string, args ...any) (sql.Result, error) {
	if err := requireSchema("write to table", t.Schema, t.Name); err != nil {
		return nil, err
	}
	return t.db.exec(ctx, q, args...)
}

// FullName returns [Schema].[Name].
func (t *Table) FullName() string { return qualifiedName(t.Schema, t.Name) }

// Database returns the database the table belongs to.
func (t *Table) Database() *Database { return t.db }

// TableDetail holds the sys.tables columns and related lookups Table itself
// doesn't carry (Table is also used to populate the Object Explorer tree
// and the scripter, so it stays lean) — SSMS's Table Properties > General
// page's "Object details" and "Dependencies" sections.
type TableDetail struct {
	SchemaOwner    string
	LockEscalation string // e.g. "TABLE", "AUTO", "DISABLE"
	UsesAnsiNulls  bool
	IsReplicated   bool
	IsTrackedByCDC bool
	TemporalType   string // e.g. "NON_TEMPORAL_TABLE", "SYSTEM_VERSIONED_TEMPORAL_TABLE"
	Durability     string // "SCHEMA_AND_DATA" or "SCHEMA_ONLY" — memory-optimized tables only
	LedgerType     string // e.g. "NON_LEDGER_TABLE", "APPEND_ONLY_LEDGER_TABLE"
	PrimaryKeyName string // "" if the table has no primary key
	DataSpace      string // filegroup (or partition scheme) backing the heap/clustered index
}

// Detail returns TableDetail for the table.
func (t *Table) Detail(ctx context.Context) (*TableDetail, error) {
	d := &TableDetail{}
	if err := t.db.queryRow(ctx, func(row *sql.Row) error {
		return row.Scan(
			&d.SchemaOwner, &d.LockEscalation, &d.UsesAnsiNulls,
			&d.IsReplicated, &d.IsTrackedByCDC, &d.TemporalType,
			&d.Durability, &d.LedgerType, &d.PrimaryKeyName, &d.DataSpace,
		)
	}, t.detailSelect(), t.ObjectID); err != nil {
		return nil, fmt.Errorf("gosmo: table detail for %s: %w", t.FullName(), err)
	}
	return d, nil
}

// detailSelect is Detail's query, version-gated.
func (t *Table) detailSelect() string {
	// ledger_type_desc is a ledger column, and ledger tables are SQL Server
	// 2022 (16.x) and later; sys.tables has no such column before then, and
	// naming it fails the whole read rather than the one field. Every table on
	// an older instance is a non-ledger table, so that is the honest
	// substitute.
	// https://learn.microsoft.com/sql/relational-databases/security/ledger/ledger-overview
	return `
SELECT owner.name, t.lock_escalation_desc, t.uses_ansi_nulls,
       t.is_replicated, t.is_tracked_by_cdc, t.temporal_type_desc,
       t.durability_desc,
       ` + colSince(t.db.serverMajorVersion(), SQLServer2022, "t.ledger_type_desc", "CAST('NON_LEDGER_TABLE' AS nvarchar(60))") + `,
       ISNULL((SELECT TOP 1 i.name FROM sys.indexes i
               WHERE i.object_id = t.object_id AND i.is_primary_key = 1), ''),
       ISNULL((SELECT TOP 1 ds.name FROM sys.indexes i
               JOIN sys.data_spaces ds ON ds.data_space_id = i.data_space_id
               WHERE i.object_id = t.object_id AND i.index_id IN (0,1)), '')
FROM   sys.tables t
JOIN   sys.schemas s ON s.schema_id = t.schema_id
JOIN   sys.database_principals owner ON owner.principal_id = s.principal_id
WHERE  t.object_id = @p1`
}

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
	// when it is not qualified, so ColumnTypeString qualifies it.
	TypeSchema        string
	IsUserDefinedType bool
	// IsPersisted is sys.computed_columns.is_persisted; false for a column
	// that is not computed.
	IsPersisted bool
	// IdentityNotForReplication is IDENTITY … NOT FOR REPLICATION.
	IdentityNotForReplication bool
	IsSparse                  bool
	// IsColumnSet is an XML column set FOR ALL_SPARSE_COLUMNS.
	IsColumnSet bool
	// MaskingFunction is the dynamic data masking function, e.g. "default()"
	// or `partial(1,"XXX",0)`; "" when the column is not masked.
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
// graph_type is SQL Server 2017's, with graph tables themselves, and
// is_dropped_ledger_column 2022's, with ledger tables. The Always Encrypted
// columns are 2016, gosmo's floor, and need no gate.
// https://learn.microsoft.com/sql/relational-databases/system-catalog-views/sys-columns-transact-sql
func (d *Database) columnSelect() string {
	major := d.serverMajorVersion()
	return `
SELECT c.name, c.column_id,
       tp.name,
       c.max_length, c.precision, c.scale,
       c.is_nullable, c.is_identity, c.is_computed,
       ISNULL(cc.definition, ''),
       ISNULL(dc.name, ''), ISNULL(dc.definition, ''),
       c.is_rowguidcol, ISNULL(c.collation_name, ''),
       CONVERT(nvarchar(40), ic.seed_value), CONVERT(nvarchar(40), ic.increment_value),
       CAST(CASE WHEN pk.column_id IS NOT NULL THEN 1 ELSE 0 END AS BIT),
       SCHEMA_NAME(tp.schema_id), tp.is_user_defined,
       ISNULL(cc.is_persisted, 0), ISNULL(ic.is_not_for_replication, 0),
       c.is_sparse, c.is_column_set,
       ISNULL(mc.masking_function, ''),
       c.generated_always_type, c.is_hidden, c.is_filestream,
       ` + colSince(major, SQLServer2017, "ISNULL(c.graph_type, 0)", "CAST(0 AS int)") + `,
       ` + colSince(major, SQLServer2022, "c.is_dropped_ledger_column", "CAST(0 AS bit)") + `,
       ISNULL(cek.name, ''), ISNULL(c.encryption_type_desc, ''), ISNULL(c.encryption_algorithm_name, '')
FROM   sys.columns c
JOIN   sys.types tp ON tp.user_type_id = c.user_type_id
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
		); err != nil {
			return nil, err
		}
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

// AlterColumn changes an existing column's data type and/or nullability
// (ALTER TABLE ... ALTER COLUMN). Identity and default are not settable this
// way — SQL Server requires dropping and re-adding the column, or its default
// constraint, for those.
func (t *Table) AlterColumn(ctx context.Context, col ColumnDefinition) error {
	if col.Name == "" {
		return fmt.Errorf("gosmo: alter column: name is required")
	}
	if err := checkColumnDefinition(col); err != nil {
		return fmt.Errorf("gosmo: alter column %q: %w", col.Name, err)
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "ALTER TABLE %s ALTER COLUMN %s %s", t.FullName(), quoteIdent(col.Name), colTypeSQL(col))
	if col.IsNullable {
		sb.WriteString(" NULL")
	} else {
		sb.WriteString(" NOT NULL")
	}

	if _, err := t.exec(ctx, sb.String()); err != nil {
		return fmt.Errorf("gosmo: alter column %q on %s: %w", col.Name, t.FullName(), err)
	}
	return nil
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
		return fmt.Errorf("gosmo: drop column on %s: name is required", t.FullName())
	}
	if _, err := t.exec(ctx,
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
		return fmt.Errorf("gosmo: rename column on %s: both names are required", t.FullName())
	}
	objName := t.FullName() + "." + quoteIdent(name)
	if _, err := t.exec(ctx,
		"EXEC sp_rename @objname = @p1, @newname = @p2, @objtype = N'COLUMN'",
		objName, newName,
	); err != nil {
		return fmt.Errorf("gosmo: rename column %q to %q on %s: %w", name, newName, t.FullName(), err)
	}
	return nil
}

// -- Foreign keys --------------------------------------------------------------

// ForeignKey mirrors Microsoft.SqlServer.Management.Smo.ForeignKey.
type ForeignKey struct {
	Name              string
	Columns           []string
	ReferencedTable   string
	ReferencedSchema  string
	ReferencedColumns []string
	DeleteAction      string // NO_ACTION, CASCADE, SET_NULL, SET_DEFAULT
	UpdateAction      string
	IsDisabled        bool
	// IsNotTrusted is set when the server has not verified the key against
	// every existing row — always for a disabled one, and for one enabled or
	// added WITH NOCHECK.
	IsNotTrusted        bool
	IsNotForReplication bool
}

// foreignKeySelect is shared by ForeignKeys and
// ForeignKeyByName so a foreign key carries the same fields however
// it was fetched.
var foreignKeySelect = `
SELECT fk.name, fk.is_disabled, fk.is_not_trusted, fk.is_not_for_replication,
       fk.delete_referential_action_desc, fk.update_referential_action_desc,
       SCHEMA_NAME(rt.schema_id), rt.name,
       ` + jsonList("c.name", `
        FROM   sys.foreign_key_columns fkc
        JOIN   sys.columns c
               ON  c.object_id = fkc.parent_object_id
               AND c.column_id = fkc.parent_column_id
        WHERE  fkc.constraint_object_id = fk.object_id`,
	"fkc.constraint_column_id") + `,
       ` + jsonList("c.name", `
        FROM   sys.foreign_key_columns fkc
        JOIN   sys.columns c
               ON  c.object_id = fkc.referenced_object_id
               AND c.column_id = fkc.referenced_column_id
        WHERE  fkc.constraint_object_id = fk.object_id`,
	"fkc.constraint_column_id") + `
FROM   sys.foreign_keys fk
JOIN   sys.tables rt ON rt.object_id = fk.referenced_object_id
WHERE  fk.parent_object_id = @p1`

// ForeignKeys returns all foreign keys on the table.
func (t *Table) ForeignKeys(ctx context.Context) ([]*ForeignKey, error) {
	rows, err := t.db.query(ctx, foreignKeySelect+`
ORDER  BY fk.name`, t.ObjectID)
	return scanRows(rows, err, fmt.Sprintf("list foreign keys for %s", t.FullName()), func(scan func(...any) error) (*ForeignKey, error) {
		return scanForeignKey(scan)
	})
}

// ForeignKeyByName returns one foreign key on the table by name.
//
// It returns an error satisfying errors.Is(err, ErrNotFound) when the table
// has no such foreign key.
func (t *Table) ForeignKeyByName(ctx context.Context, name string) (*ForeignKey, error) {
	var fk *ForeignKey
	err := t.db.queryRow(ctx, func(row *sql.Row) error {
		var err error
		fk, err = scanForeignKey(row.Scan)
		return err
	}, foreignKeySelect+`
       AND fk.name = @p2`, t.ObjectID, name)
	return foundRow(fk, err, notFoundf("gosmo: foreign key %q not found on %s", name, t.FullName()), fmt.Sprintf("find foreign key %q on %s", name, t.FullName()))
}

func scanForeignKey(scan func(...any) error) (*ForeignKey, error) {
	fk := &ForeignKey{}
	var cols, refCols sql.NullString
	if err := scan(&fk.Name, &fk.IsDisabled, &fk.IsNotTrusted, &fk.IsNotForReplication,
		&fk.DeleteAction, &fk.UpdateAction,
		&fk.ReferencedSchema, &fk.ReferencedTable,
		&cols, &refCols); err != nil {
		return nil, err
	}
	var err error
	if fk.Columns, err = decodeJSONList(cols); err != nil {
		return nil, err
	}
	if fk.ReferencedColumns, err = decodeJSONList(refCols); err != nil {
		return nil, err
	}
	return fk, nil
}

// -- Check constraints ---------------------------------------------------------

// CheckConstraint represents a CHECK constraint.
type CheckConstraint struct {
	Name       string
	Definition string
	IsDisabled bool
	Column     string // empty for table-level checks
	// IsNotTrusted is set when the server has not verified the constraint
	// against every existing row — always for a disabled one, and for one
	// enabled or added WITH NOCHECK.
	IsNotTrusted        bool
	IsNotForReplication bool
}

// CheckConstraints returns all CHECK constraints on the table.
func (t *Table) CheckConstraints(ctx context.Context) ([]*CheckConstraint, error) {
	const q = `
SELECT cc.name, cc.definition, cc.is_disabled, ISNULL(c.name, ''),
       cc.is_not_trusted, cc.is_not_for_replication
FROM   sys.check_constraints cc
LEFT   JOIN sys.columns c
       ON  c.object_id  = cc.parent_object_id
       AND c.column_id  = cc.parent_column_id
WHERE  cc.parent_object_id = @p1
ORDER  BY cc.name`

	rows, err := t.db.query(ctx, q, t.ObjectID)
	return scanRows(rows, err, fmt.Sprintf("list check constraints for %s", t.FullName()), func(scan func(...any) error) (*CheckConstraint, error) {
		cc := &CheckConstraint{}
		if err := scan(&cc.Name, &cc.Definition, &cc.IsDisabled, &cc.Column,
			&cc.IsNotTrusted, &cc.IsNotForReplication); err != nil {
			return nil, err
		}
		return cc, nil
	})
}

// -- Triggers --------------------------------------------------------------------

// Triggers returns all DML triggers attached to this table.
func (t *Table) Triggers(ctx context.Context) ([]*Trigger, error) {
	return t.db.triggersWhere(ctx, "AND tr.parent_id = @p1", []any{t.ObjectID})
}

// -- DDL helpers ---------------------------------------------------------------

// CreateTableRequest describes a table to be created.
type CreateTableRequest struct {
	Schema  string
	Name    string
	Columns []ColumnDefinition
}

// ColumnDefinition describes a column in a CREATE TABLE statement.
type ColumnDefinition struct {
	Name      string
	DataType  DataType
	MaxLength int // char/varchar/nchar/nvarchar/binary/varbinary: 0 = omit, -1 = MAX
	// Precision is a decimal/numeric column's precision; nil leaves it to the
	// type's default (18).
	Precision *int
	// Scale is a decimal/numeric column's scale, or the fractional-seconds
	// precision of a datetime2, time or datetimeoffset one; nil leaves it to
	// the type's default (0 for decimal, 7 for the three time types).
	//
	// A pointer because 0 is a real scale: as an int, datetime2(0), time(0)
	// and datetimeoffset(0) could not be asked for — zero read as
	// "unspecified" and produced the 7-digit default — and decimal(p,0) with
	// no precision became decimal(18,0).
	Scale        *int
	IsNullable   bool
	IsIdentity   bool
	IdentitySeed int64
	IdentityIncr int64
	DefaultValue string // expression, e.g. "sysdatetime()" or "0"
	IsPrimaryKey bool
}

// CreateTable creates a table from a CreateTableRequest.
func (d *Database) CreateTable(ctx context.Context, req CreateTableRequest) (*Table, error) {
	if req.Name == "" {
		return nil, fmt.Errorf("gosmo: create table: name is required")
	}
	if err := requireSchema("create table", req.Schema, req.Name); err != nil {
		return nil, err
	}
	if len(req.Columns) == 0 {
		return nil, fmt.Errorf("gosmo: create table: at least one column is required")
	}
	for _, col := range req.Columns {
		if err := checkColumnDefinition(col); err != nil {
			return nil, fmt.Errorf("gosmo: create table %q: column %q: %w", req.Name, col.Name, err)
		}
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "CREATE TABLE %s (\n", qualifiedName(req.Schema, req.Name))

	var pkCols []string
	for _, col := range req.Columns {
		fmt.Fprintf(&sb, "    %s %s", quoteIdent(col.Name), colTypeSQL(col))
		if col.IsIdentity {
			fmt.Fprintf(&sb, " IDENTITY(%d,%d)", col.IdentitySeed, col.IdentityIncr)
		}
		if !col.IsNullable {
			sb.WriteString(" NOT NULL")
		} else {
			sb.WriteString(" NULL")
		}
		if col.DefaultValue != "" {
			fmt.Fprintf(&sb, " DEFAULT (%s)", col.DefaultValue)
		}
		if col.IsPrimaryKey {
			pkCols = append(pkCols, quoteIdent(col.Name))
		}
		sb.WriteString(",\n")
	}
	if len(pkCols) > 0 {
		fmt.Fprintf(&sb, "    CONSTRAINT %s PRIMARY KEY CLUSTERED (%s)\n",
			quoteIdent("PK_"+req.Name), strings.Join(pkCols, ", "))
	} else {
		// trim the trailing comma from the last column line
		s := sb.String()
		if i := strings.LastIndex(s, ",\n"); i >= 0 {
			sb.Reset()
			sb.WriteString(s[:i])
			sb.WriteString("\n")
		}
	}
	sb.WriteString(")")

	if _, err := d.exec(ctx, sb.String()); err != nil {
		return nil, fmt.Errorf("gosmo: create table %s: %w", qualifiedName(req.Schema, req.Name), err)
	}
	return createdObject(ctx, d.TableRef(req.Schema, req.Name), func() (*Table, error) {
		return d.TableByName(ctx, req.Schema, req.Name)
	})
}

// DropTable drops a table.
// When cascade=true it first drops all incoming foreign-key constraints.
//
// # Dropping something that isn't there is an error
//
// This and every other Drop* write method issue a bare DROP, so a name that
// matches nothing comes back as the server's "Cannot drop ... because it does
// not exist" rather than as success. Half of them used to carry IF EXISTS and
// half did not, which made the same gesture in a caller's UI report two
// different things about the same situation: a deleted view that was already
// gone said "deleted", a deleted sequence said the server refused. Callers that
// want the idempotent form should ignore the error, which is a decision they
// can make and this package cannot make for them.
//
// The generated *scripts* keep IF EXISTS — Scripter's DROP-and-CREATE output
// exists to be re-run, which is the opposite requirement.
func (d *Database) DropTable(ctx context.Context, schema, name string, cascade bool) error {
	if err := requireSchema("drop table", schema, name); err != nil {
		return err
	}
	qn := qualifiedName(schema, name)
	if !cascade {
		if _, err := d.exec(ctx, "DROP TABLE "+qn); err != nil {
			return fmt.Errorf("gosmo: drop table %s: %w", qn, err)
		}
		return nil
	}
	if _, err := d.exec(ctx, dropTableCascadeBatch(qn)); err != nil {
		return fmt.Errorf("gosmo: drop table %s with its incoming foreign keys: %w", qn, err)
	}
	return nil
}

// dropTableCascadeBatch renders the cascade drop of the table qn (already
// bracket-quoted) as one atomicBatch: every incoming foreign key, then the
// table, or neither.
//
// It was two execs, and the DROP TABLE failing after the first had committed
// — a schema-bound view on the table (Msg 3729), a permission the second
// statement needs, a cancel — left the table in place with its foreign keys
// gone for good and nothing saying so.
//
// The key drop runs as its own dynamic SQL through EXEC(N'…'), so its
// DECLARE @sql is scoped to that inner batch: a batch-scoped DECLARE would
// collide with itself when a ScriptCollector concatenates two of these (see
// atomicBatch). atomicBatch takes no parameters, so the table name is inlined
// as a literal — escaped once for OBJECT_ID's literal and once more for the
// EXEC string around it.
func dropTableCascadeBatch(qn string) string {
	dropFKs := `DECLARE @sql NVARCHAR(MAX) = N'';
SELECT @sql += N'ALTER TABLE ' + QUOTENAME(SCHEMA_NAME(fk.schema_id)) +
               N'.' + QUOTENAME(OBJECT_NAME(fk.parent_object_id)) +
               N' DROP CONSTRAINT ' + QUOTENAME(fk.name) + N'; '
FROM   sys.foreign_keys fk
WHERE  fk.referenced_object_id = OBJECT_ID(` + QuoteLiteral(qn) + `);
IF LEN(@sql) > 0 EXEC sp_executesql @sql;`
	return atomicBatch([]string{
		"EXEC(" + QuoteLiteral(dropFKs) + ")",
		"DROP TABLE " + qn,
	})
}

// RenameTable renames a table using sp_rename.
func (d *Database) RenameTable(ctx context.Context, schema, oldName, newName string) error {
	if err := requireSchema("rename table", schema, oldName); err != nil {
		return err
	}
	if _, err := d.exec(ctx,
		"EXEC sp_rename @objname = @p1, @newname = @p2, @objtype = N'OBJECT'",
		qualifiedName(schema, oldName), newName,
	); err != nil {
		return fmt.Errorf("gosmo: rename table %s -> %s: %w", qualifiedName(schema, oldName), newName, err)
	}
	return nil
}

// TruncateTable truncates a table.
func (t *Table) TruncateTable(ctx context.Context) error {
	if _, err := t.exec(ctx, "TRUNCATE TABLE "+t.FullName()); err != nil {
		return fmt.Errorf("gosmo: truncate %s: %w", t.FullName(), err)
	}
	return nil
}

// RowCount returns the approximate row count using partition statistics.
func (t *Table) RowCount(ctx context.Context) (int64, error) {
	var n int64
	if err := t.db.queryRow(ctx, func(row *sql.Row) error { return row.Scan(&n) }, `
SELECT SUM(p.rows)
FROM   sys.partitions p
WHERE  p.object_id = @p1 AND p.index_id IN (0, 1)`, t.ObjectID); err != nil {
		return 0, fmt.Errorf("gosmo: row count for %s: %w", t.FullName(), err)
	}
	return n, nil
}

// TableRowCounts returns the row count of every user table in the database,
// keyed by object_id — Table.RowCount for all tables in a single round trip.
//
// Use this over a loop of Table.RowCount whenever the caller wants more than
// a couple of tables: the per-table form costs one query (and one pooled
// connection) each. The filter and aggregate are the same, so the counts are
// identical either way — metadata counts from sys.partitions, which is what
// SSMS's object grids show, not a COUNT(*).
//
// A table with no row in sys.partitions is absent from the map rather than
// present as 0; callers should treat a missing key as zero rows.
func (d *Database) TableRowCounts(ctx context.Context) (map[int]int64, error) {
	const q = `
SELECT p.object_id, SUM(p.rows)
FROM   sys.partitions p
JOIN   sys.tables t ON t.object_id = p.object_id
WHERE  p.index_id IN (0, 1)
GROUP  BY p.object_id`

	rows, err := d.query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("gosmo: table row counts on %q: %w", d.Name, err)
	}
	defer rows.Close()

	out := make(map[int]int64)
	for rows.Next() {
		var objectID int
		var n int64
		if err := rows.Scan(&objectID, &n); err != nil {
			return nil, fmt.Errorf("gosmo: table row counts on %q: %w", d.Name, err)
		}
		out[objectID] = n
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("gosmo: table row counts on %q: %w", d.Name, err)
	}
	return out, nil
}

// -- Predicate helpers -----------------------------------------------------

// CountWhere returns the number of rows in the table matching a WHERE
// predicate — used to estimate qualifying rows for a filtered index or
// filtered statistic's predicate (SSMS's "Estimate Rows" action).
//
// predicate is interpolated as-is after WHERE; callers pass a filter
// expression already captured from the server (e.g. an index or statistic's
// own FilterDefinition), not raw user input.
func (t *Table) CountWhere(ctx context.Context, predicate string) (int64, error) {
	q := fmt.Sprintf("SELECT COUNT_BIG(*) FROM %s WHERE %s", t.FullName(), predicate)
	var n int64
	if err := t.db.queryRow(ctx, func(row *sql.Row) error { return row.Scan(&n) }, q); err != nil {
		return 0, fmt.Errorf("gosmo: count where for %s: %w", t.FullName(), err)
	}
	return n, nil
}

// CheckWhereSyntax validates a WHERE predicate against the table without
// scanning any data (SSMS's "Check Syntax" action for a filtered index or
// statistic's predicate).
func (t *Table) CheckWhereSyntax(ctx context.Context, predicate string) error {
	q := fmt.Sprintf("SELECT TOP (0) 1 AS ok FROM %s WHERE %s", t.FullName(), predicate)
	rows, err := t.db.query(ctx, q)
	if err != nil {
		return fmt.Errorf("gosmo: check syntax for %s: %w", t.FullName(), err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("gosmo: check syntax for %s: %w", t.FullName(), err)
	}
	return nil
}

// -- Column type builder -------------------------------------------------------

// colTypeSQL returns the T-SQL data-type fragment for a ColumnDefinition.
// Callers must validate col.DataType (see validDataType) before calling this
// — it trusts its input and does not itself reject an unrecognized type.
// scripter_table.go's ColumnTypeString does the equivalent for a *Column (from
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
	}
	return string(col.DataType)
}

// checkColumnDefinition refuses a definition colTypeSQL cannot render as
// written, rather than rendering something else: a decimal scale with no
// precision has no T-SQL spelling (decimal(,2) is a syntax error), and a
// precision or scale on a type that takes neither would be dropped silently.
func checkColumnDefinition(col ColumnDefinition) error {
	if !validDataType(col.DataType) {
		return fmt.Errorf("unrecognized data type %q", col.DataType)
	}
	switch col.DataType {
	case DataTypeDecimal, DataTypeNumeric:
		if col.Scale != nil && col.Precision == nil {
			return fmt.Errorf("a %s scale needs a precision", col.DataType)
		}
	case DataTypeDatetime2, DataTypeTime, DataTypeDatetimeOffset:
		if col.Precision != nil {
			return fmt.Errorf("%s takes a scale, not a precision", col.DataType)
		}
	default:
		if col.Precision != nil || col.Scale != nil {
			return fmt.Errorf("%s takes no precision or scale", col.DataType)
		}
	}
	return nil
}

// DropConstraint drops a named table constraint — a PRIMARY KEY, UNIQUE
// constraint, FOREIGN KEY, or CHECK constraint. All four share one
// per-table name space and are all removed by ALTER TABLE ... DROP
// CONSTRAINT; an index that is not backing a key constraint is not a
// constraint and needs Index.Drop instead.
func (t *Table) DropConstraint(ctx context.Context, name string) error {
	q := fmt.Sprintf("ALTER TABLE %s DROP CONSTRAINT %s", t.FullName(), quoteIdent(name))
	if _, err := t.exec(ctx, q); err != nil {
		return fmt.Errorf("gosmo: drop constraint %q on %s: %w", name, t.FullName(), err)
	}
	return nil
}
