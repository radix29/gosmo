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
// Rename, the ALTER-style writes, FragmentationStats, CountWhere). Every
// method that queries by ObjectID — Columns, Indexes, Statistics, Triggers,
// Partitions, the size and detail reads — refuses it with ErrHandleNotLoaded
// (see requireLoaded), so those need a Table from Tables/TableByName instead.
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

// requireLoaded refuses a read keyed by ObjectID on a table that has none —
// a TableRef handle. Until it existed such a read asked for object 0 and
// answered with an empty list, which a caller could not tell from a table
// that really has no columns or indexes.
func (t *Table) requireLoaded(what string) error {
	if t.ObjectID == 0 {
		return fmt.Errorf("gosmo: %s %s: %w", what, t.FullName(), ErrHandleNotLoaded)
	}
	return nil
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
	if err := t.requireLoaded("table detail for"); err != nil {
		return nil, err
	}
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
	if err := t.requireLoaded("list foreign keys for"); err != nil {
		return nil, err
	}
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
	if err := t.requireLoaded(fmt.Sprintf("find foreign key %q on", name)); err != nil {
		return nil, err
	}
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
	if err := t.requireLoaded("list check constraints for"); err != nil {
		return nil, err
	}
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
	if err := t.requireLoaded("list triggers for"); err != nil {
		return nil, err
	}
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
	Scale *int
	// XMLSchemaCollectionSchema and XMLSchemaCollection make an xml column
	// typed by that schema collection, and IsXMLDocument picks DOCUMENT over
	// CONTENT; all three are refused on any other type, and the schema is
	// required with the collection (unqualified, it resolves against the
	// caller's default schema). Both names "" is an untyped xml column.
	XMLSchemaCollectionSchema string
	XMLSchemaCollection       string
	IsXMLDocument             bool
	// VectorDimensions and VectorBaseType declare a vector column (SQL
	// Server 2025): the dimensions are required, and VectorBaseType is ""
	// or "float32" (the default) or "float16" — which the server accepts
	// only with the database's PREVIEW_FEATURES scoped configuration on.
	VectorDimensions int
	VectorBaseType   string
	IsNullable       bool
	IsIdentity       bool
	IdentitySeed     int64
	IdentityIncr     int64
	DefaultValue     string // expression, e.g. "sysdatetime()" or "0"
	IsPrimaryKey     bool
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

// Drop drops the table.
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
func (t *Table) Drop(ctx context.Context, cascade bool) error {
	qn := t.FullName()
	if !cascade {
		if _, err := t.exec(ctx, "DROP TABLE "+qn); err != nil {
			return fmt.Errorf("gosmo: drop table %s: %w", qn, err)
		}
		return nil
	}
	if err := requireSchema("write to table", t.Schema, t.Name); err != nil {
		return err
	}
	if err := t.db.execAtomic(ctx, dropTableCascadeStmts(qn)); err != nil {
		return fmt.Errorf("gosmo: drop table %s with its incoming foreign keys: %w", qn, err)
	}
	return nil
}

// dropTableCascadeStmts is the cascade drop of the table qn (already
// bracket-quoted), run as one atomicBatch: every incoming foreign key, then
// the table, or neither.
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
func dropTableCascadeStmts(qn string) []string {
	dropFKs := `DECLARE @sql NVARCHAR(MAX) = N'';
SELECT @sql += N'ALTER TABLE ' + QUOTENAME(SCHEMA_NAME(fk.schema_id)) +
               N'.' + QUOTENAME(OBJECT_NAME(fk.parent_object_id)) +
               N' DROP CONSTRAINT ' + QUOTENAME(fk.name) + N'; '
FROM   sys.foreign_keys fk
WHERE  fk.referenced_object_id = OBJECT_ID(` + QuoteLiteral(qn) + `);
IF LEN(@sql) > 0 EXEC sp_executesql @sql;`
	return []string{
		"EXEC(" + QuoteLiteral(dropFKs) + ")",
		"DROP TABLE " + qn,
	}
}

// Rename renames the table (sp_rename's 'OBJECT' class). newName is a bare
// name; a rename never moves the table between schemas — see Transfer.
func (t *Table) Rename(ctx context.Context, newName string) error {
	if err := t.db.renameSchemaObject(ctx, "table", renameObjectClass, t.Schema, t.Name, newName); err != nil {
		return err
	}
	setIfApplied(ctx, &t.Name, newName)
	return nil
}

// Transfer moves the table into another schema (ALTER SCHEMA ... TRANSFER).
// It keeps its name and object_id, and its indexes, constraints and triggers
// move with it; permissions granted on it directly are dropped by the server.
func (t *Table) Transfer(ctx context.Context, targetSchema string) error {
	if err := t.db.transferSchemaObject(ctx, "table", transferObjectClass, targetSchema, t.Schema, t.Name); err != nil {
		return err
	}
	setIfApplied(ctx, &t.Schema, targetSchema)
	return nil
}

// Truncate empties the table (TRUNCATE TABLE).
func (t *Table) Truncate(ctx context.Context) error {
	if _, err := t.exec(ctx, "TRUNCATE TABLE "+t.FullName()); err != nil {
		return fmt.Errorf("gosmo: truncate %s: %w", t.FullName(), err)
	}
	return nil
}

// RowCount returns the approximate row count using partition statistics.
func (t *Table) RowCount(ctx context.Context) (int64, error) {
	if err := t.requireLoaded("row count for"); err != nil {
		return 0, err
	}
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

// RenameConstraint renames a foreign key, CHECK or default constraint on the
// table (sp_rename's 'OBJECT' class — a constraint is a row of sys.objects,
// in the table's schema). A primary key or unique constraint is renamed
// through its backing index instead — Index.Rename — since its name is the
// index's name in sys.indexes.
func (t *Table) RenameConstraint(ctx context.Context, name, newName string) error {
	if name == "" || newName == "" {
		return fmt.Errorf("gosmo: rename constraint on %s: both names are required", t.FullName())
	}
	if err := t.db.renameSchemaObject(ctx, "constraint", renameObjectClass, t.Schema, name, newName); err != nil {
		return err
	}
	return nil
}
