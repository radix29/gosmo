package gosmo

import (
	"context"
	"database/sql"
	"errors"
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
}

// Table returns a lightweight handle to a table by name, without a query.
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
// Like Server.Database, this is also the only form that works under a
// WithScript-derived context, where no lookup can run at all.
func (d *Database) Table(schema, name string) *Table {
	if schema == "" {
		schema = "dbo"
	}
	return &Table{db: d, Schema: schema, Name: name}
}

// FullName returns [Schema].[Name].
func (t *Table) FullName() string { return qualifiedName(t.Schema, t.Name) }

// DB returns the parent Database.
func (t *Table) DB() *Database { return t.db }

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
func (t *Table) Detail() (*TableDetail, error) {
	return t.DetailContext(context.Background())
}

// DetailContext is the context-aware variant of Detail.
func (t *Table) DetailContext(ctx context.Context) (*TableDetail, error) {
	const q = `
SELECT owner.name, t.lock_escalation_desc, t.uses_ansi_nulls,
       t.is_replicated, t.is_tracked_by_cdc, t.temporal_type_desc,
       t.durability_desc, t.ledger_type_desc,
       ISNULL((SELECT TOP 1 i.name FROM sys.indexes i
               WHERE i.object_id = t.object_id AND i.is_primary_key = 1), ''),
       ISNULL((SELECT TOP 1 ds.name FROM sys.indexes i
               JOIN sys.data_spaces ds ON ds.data_space_id = i.data_space_id
               WHERE i.object_id = t.object_id AND i.index_id IN (0,1)), '')
FROM   sys.tables t
JOIN   sys.schemas s ON s.schema_id = t.schema_id
JOIN   sys.database_principals owner ON owner.principal_id = s.principal_id
WHERE  t.object_id = @p1`

	d := &TableDetail{}
	if err := t.db.queryRow(ctx, func(row *sql.Row) error {
		return row.Scan(
			&d.SchemaOwner, &d.LockEscalation, &d.UsesAnsiNulls,
			&d.IsReplicated, &d.IsTrackedByCDC, &d.TemporalType,
			&d.Durability, &d.LedgerType, &d.PrimaryKeyName, &d.DataSpace,
		)
	}, q, t.ObjectID); err != nil {
		return nil, fmt.Errorf("gosmo: table detail for %s: %w", t.FullName(), err)
	}
	return d, nil
}

// -- Columns -------------------------------------------------------------------

// Column mirrors Microsoft.SqlServer.Management.Smo.Column.
type Column struct {
	Name              string
	OrdinalPosition   int
	DataType          DataType
	MaxLength         int // -1 = MAX
	Precision         int
	Scale             int
	IsNullable        bool
	IsIdentity        bool
	IdentitySeed      int64
	IdentityIncrement int64
	IsComputed        bool
	ComputedText      string
	DefaultValue      *ColumnDefault
	IsRowGUID         bool
	Collation         string
	IsPrimaryKey      bool
}

// columnSelect is the SELECT and joins every column listing shares; each
// caller appends its own WHERE, because a Table already holds an object_id
// while Database.ObjectColumns has only a name to resolve.
const columnSelect = `
SELECT c.name, c.column_id,
       tp.name,
       c.max_length, c.precision, c.scale,
       c.is_nullable, c.is_identity, c.is_computed,
       ISNULL(cc.definition, ''),
       ISNULL(dc.name, ''), ISNULL(dc.definition, ''),
       c.is_rowguidcol, ISNULL(c.collation_name, ''),
       ISNULL(ic.seed_value, 0), ISNULL(ic.increment_value, 0),
       CAST(CASE WHEN pk.column_id IS NOT NULL THEN 1 ELSE 0 END AS BIT)
FROM   sys.columns c
JOIN   sys.types tp ON tp.user_type_id = c.user_type_id
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

// Columns returns all columns for this table in ordinal order.
func (t *Table) Columns() ([]*Column, error) {
	return t.ColumnsContext(context.Background())
}

// ColumnsContext is the context-aware variant of Columns.
func (t *Table) ColumnsContext(ctx context.Context) ([]*Column, error) {
	const q = columnSelect + `
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
func (d *Database) ObjectColumns(schema, name string) ([]*Column, error) {
	return d.ObjectColumnsContext(context.Background(), schema, name)
}

// ObjectColumnsContext is the context-aware variant of ObjectColumns.
//
// The columns a view does not have — identity, computed text, defaults,
// primary key — come back at their zero values, because the joins that
// supply them simply do not match for a view. Name, ordinal, type,
// length/precision/scale, nullability and collation are all real.
func (d *Database) ObjectColumnsContext(ctx context.Context, schema, name string) ([]*Column, error) {
	const q = columnSelect + `
WHERE  c.object_id = OBJECT_ID(@p1)
ORDER  BY c.column_id`

	ref := qualifiedName(schema, name)
	rows, err := d.query(ctx, q, ref)
	if err != nil {
		return nil, fmt.Errorf("gosmo: list columns for %s in %q: %w", ref, d.name, err)
	}
	defer rows.Close()

	cols, err := scanColumns(rows.Rows)
	if err != nil {
		return nil, fmt.Errorf("gosmo: list columns for %s in %q: %w", ref, d.name, err)
	}
	// Every table and view has at least one column, so an empty result means
	// OBJECT_ID found nothing — report that rather than an empty column list,
	// which reads as "this object has no columns".
	if len(cols) == 0 {
		return nil, notFoundf("gosmo: table or view %s not found in %q", ref, d.name)
	}
	return cols, nil
}

// scanColumns reads the column shape columnSelect returns.
func scanColumns(rows *sql.Rows) ([]*Column, error) {
	var cols []*Column
	for rows.Next() {
		col := &Column{}
		var compText, dcName, dcDef, collation sql.NullString
		var seed, increment sql.NullInt64
		if err := rows.Scan(
			&col.Name, &col.OrdinalPosition,
			&col.DataType, &col.MaxLength, &col.Precision, &col.Scale,
			&col.IsNullable, &col.IsIdentity, &col.IsComputed,
			&compText, &dcName, &dcDef,
			&col.IsRowGUID, &collation,
			&seed, &increment,
			&col.IsPrimaryKey,
		); err != nil {
			return nil, err
		}
		col.ComputedText = compText.String
		col.Collation = collation.String
		if dcName.String != "" {
			col.DefaultValue = &ColumnDefault{Name: dcName.String, Definition: dcDef.String}
		}
		col.IdentitySeed = seed.Int64
		col.IdentityIncrement = increment.Int64
		cols = append(cols, col)
	}
	return cols, rows.Err()
}

// AlterColumn changes an existing column's data type and/or nullability
// (ALTER TABLE ... ALTER COLUMN). Identity and default are not settable this
// way — SQL Server requires dropping and re-adding the column, or its default
// constraint, for those.
func (t *Table) AlterColumn(col ColumnDefinition) error {
	return t.AlterColumnContext(context.Background(), col)
}

// AlterColumnContext is the context-aware variant of AlterColumn.
func (t *Table) AlterColumnContext(ctx context.Context, col ColumnDefinition) error {
	if col.Name == "" {
		return fmt.Errorf("gosmo: alter column: name is required")
	}
	if !validDataType(col.DataType) {
		return fmt.Errorf("gosmo: alter column %q: unrecognized data type %q", col.Name, col.DataType)
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "ALTER TABLE %s ALTER COLUMN %s %s", t.FullName(), quoteIdent(col.Name), colTypeSQL(col))
	if col.IsNullable {
		sb.WriteString(" NULL")
	} else {
		sb.WriteString(" NOT NULL")
	}

	if _, err := t.db.exec(ctx, sb.String()); err != nil {
		return fmt.Errorf("gosmo: alter column %q on %s: %w", col.Name, t.FullName(), err)
	}
	return nil
}

// -- Indexes -------------------------------------------------------------------

// Index mirrors Microsoft.SqlServer.Management.Smo.Index.
type Index struct {
	Name               string
	IndexID            int
	Type               IndexType
	IsClustered        bool
	IsUnique           bool
	IsPrimaryKey       bool
	IsUniqueConstraint bool
	IsDisabled         bool
	FillFactor         int
	IsPadded           bool
	IgnoreDupKey       bool
	AllowRowLocks      bool
	AllowPageLocks     bool
	DataCompression    string
	KeyColumns         []IndexColumn
	IncludedColumns    []IndexColumn
	FilterDefinition   string
	DataSpace          DataSpace
}

// DataSpace names where a table or index keeps its rows — the ON clause of
// CREATE TABLE and CREATE INDEX. It is either a filegroup or a partition
// scheme, and for a partition scheme the partitioning column is part of the
// clause, so it is carried here too: `ON [scheme]([column])`.
//
// Name is empty for an index with no data space of its own in sys.indexes —
// a memory-optimized table's, whose rows are not on a filegroup at all.
type DataSpace struct {
	Name              string
	IsPartitionScheme bool
	// IsDefaultFileGroup is true for the database's default filegroup, the
	// one an object with no ON clause lands on. A scripter uses it to leave
	// the clause off where it would say nothing.
	IsDefaultFileGroup bool
	// PartitionColumn is the column the scheme partitions by; set only when
	// IsPartitionScheme.
	PartitionColumn string
}

// dataSpaceColumns and dataSpaceJoins read an index's ON clause out of
// sys.indexes: the data space's name and kind, whether it is the default
// filegroup, and — for a partition scheme — the partitioning column, which
// sys.index_columns marks with partition_ordinal 1.
//
// Every join is a LEFT/OUTER one and every column is wrapped in ISNULL: an
// index can have no data space at all (a memory-optimized table's), and a
// filegroup row exists only for ds.type 'FG'. The partitioning column's
// join is aliased pic, not ic: the index-column query these sit beside is
// told apart from this one by its `sys.index_columns ic`, and two of its
// tests count round trips that way.
const dataSpaceColumns = `ISNULL(ds.name, ''), CASE WHEN ds.type = 'PS' THEN 1 ELSE 0 END,
       ISNULL(fg.is_default, 0), ISNULL(pc.name, '')`

const dataSpaceJoins = `LEFT   JOIN sys.data_spaces ds ON ds.data_space_id = i.data_space_id
LEFT   JOIN sys.filegroups fg ON fg.data_space_id = ds.data_space_id
OUTER  APPLY (SELECT TOP 1 c.name
              FROM   sys.index_columns pic
              JOIN   sys.columns c ON c.object_id = pic.object_id AND c.column_id = pic.column_id
              WHERE  pic.object_id = i.object_id AND pic.index_id = i.index_id
                AND  pic.partition_ordinal > 0
              ORDER  BY pic.partition_ordinal) pc`

// IndexColumn represents one column in an index.
type IndexColumn struct {
	Name       string
	Descending bool
	IsIncluded bool
}

// Indexes returns all indexes on the table.
func (t *Table) Indexes() ([]*Index, error) {
	return t.IndexesContext(context.Background())
}

// IndexesContext is the context-aware variant of Indexes.
//
// Two queries, whatever the index count: one for the indexes, one for every
// index column on the object at once. Fetching each index's columns inside
// the loop over the indexes cost a query per index, and Database.query pins
// its own pooled connection and issues its own USE, so a table with 20
// indexes ran 42 round trips across 21 connections — with the outer one held
// throughout, which is the shape that exhausts a pool rather than merely
// being slow.
func (t *Table) IndexesContext(ctx context.Context) ([]*Index, error) {
	indexes, err := t.indexListContext(ctx, "")
	if err != nil {
		return nil, fmt.Errorf("gosmo: list indexes for %s: %w", t.FullName(), err)
	}
	if len(indexes) == 0 {
		return nil, nil
	}
	if err := t.attachIndexColumns(ctx, indexes, ""); err != nil {
		return nil, err
	}
	return indexes, nil
}

// IndexByName returns one index on the table by name, with its columns.
func (t *Table) IndexByName(name string) (*Index, error) {
	return t.IndexByNameContext(context.Background(), name)
}

// IndexByNameContext is the context-aware variant of IndexByName. It returns
// an error satisfying errors.Is(err, ErrNotFound) when the table has no such
// index. Two queries, the same shape as IndexesContext — see its comment for
// why the columns are not fetched inside the index scan.
func (t *Table) IndexByNameContext(ctx context.Context, name string) (*Index, error) {
	indexes, err := t.indexListContext(ctx, " AND i.name = @p2", name)
	if err != nil {
		return nil, fmt.Errorf("gosmo: find index %q on %s: %w", name, t.FullName(), err)
	}
	if len(indexes) == 0 {
		return nil, notFoundf("gosmo: index %q not found on %s", name, t.FullName())
	}
	if err := t.attachIndexColumns(ctx, indexes, " AND ic.index_id = @p2", indexes[0].IndexID); err != nil {
		return nil, err
	}
	return indexes[0], nil
}

// attachIndexColumns fetches the object's index columns in one query and
// distributes them over indexes by index ID.
func (t *Table) attachIndexColumns(ctx context.Context, indexes []*Index, extra string, args ...any) error {
	cols, err := t.indexColumnsContext(ctx, extra, args...)
	if err != nil {
		return fmt.Errorf("gosmo: columns of indexes on %s: %w", t.FullName(), err)
	}
	for _, idx := range indexes {
		for _, c := range cols[idx.IndexID] {
			if c.IsIncluded {
				idx.IncludedColumns = append(idx.IncludedColumns, c)
			} else {
				idx.KeyColumns = append(idx.KeyColumns, c)
			}
		}
	}
	return nil
}

// indexListContext returns the table's indexes with no columns attached,
// narrowed by extra — an additional predicate ANDed onto the object filter,
// with its parameters starting at @p2. Its rows are drained and closed before
// the caller asks for the columns, so the two queries never hold two pooled
// connections at once.
func (t *Table) indexListContext(ctx context.Context, extra string, args ...any) ([]*Index, error) {
	q := `
SELECT i.name, i.index_id, i.type_desc, i.is_unique, i.is_primary_key,
       i.is_unique_constraint, i.is_disabled, i.fill_factor,
       ISNULL(i.filter_definition, ''),
       i.is_padded, i.ignore_dup_key, i.allow_row_locks, i.allow_page_locks,
       ISNULL(p.data_compression_desc, 'NONE'),
       ` + dataSpaceColumns + `
FROM   sys.indexes i
OUTER  APPLY (SELECT TOP 1 pp.data_compression_desc FROM sys.partitions pp
              WHERE pp.object_id = i.object_id AND pp.index_id = i.index_id
              ORDER BY pp.partition_number) p
` + dataSpaceJoins + `
WHERE  i.object_id = @p1 AND i.type > 0` + extra + `
ORDER  BY i.index_id`

	rows, err := t.db.query(ctx, q, append([]any{t.ObjectID}, args...)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var indexes []*Index
	for rows.Next() {
		idx := &Index{}
		var typeDesc sql.NullString
		if err := rows.Scan(&idx.Name, &idx.IndexID, &typeDesc,
			&idx.IsUnique, &idx.IsPrimaryKey, &idx.IsUniqueConstraint,
			&idx.IsDisabled, &idx.FillFactor, &idx.FilterDefinition,
			&idx.IsPadded, &idx.IgnoreDupKey, &idx.AllowRowLocks, &idx.AllowPageLocks,
			&idx.DataCompression,
			&idx.DataSpace.Name, &idx.DataSpace.IsPartitionScheme,
			&idx.DataSpace.IsDefaultFileGroup, &idx.DataSpace.PartitionColumn); err != nil {
			return nil, err
		}
		switch desc := strings.TrimSpace(typeDesc.String); desc {
		case "CLUSTERED":
			idx.Type = IndexTypeClustered
			idx.IsClustered = true
		case "NONCLUSTERED":
			idx.Type = IndexTypeNonClustered
		case "XML":
			idx.Type = IndexTypeXML
		case "SPATIAL":
			idx.Type = IndexTypeSpatial
		case "CLUSTERED COLUMNSTORE":
			idx.Type = IndexTypeClusteredColumnStore
			idx.IsClustered = true
		case "NONCLUSTERED COLUMNSTORE":
			idx.Type = IndexTypeColumnStore
		default:
			// A type_desc with no constant — NONCLUSTERED HASH on a
			// memory-optimized table, or a type a newer SQL Server adds —
			// is carried through as the server's own text rather than left
			// empty, so a caller displays the real type instead of nothing.
			idx.Type = IndexType(desc)
		}
		indexes = append(indexes, idx)
	}
	return indexes, rows.Err()
}

// DataSpace returns where the table itself stores its rows — the filegroup
// or partition scheme its heap or clustered index is on, which is CREATE
// TABLE's ON clause.
func (t *Table) DataSpace() (DataSpace, error) {
	return t.DataSpaceContext(context.Background())
}

// DataSpaceContext is the context-aware variant of DataSpace.
//
// Read from index_id 0 or 1, so it answers for a heap as well as a clustered
// table — which is why it is a query of its own rather than a field of the
// index list, whose `i.type > 0` filter has no heap in it.
//
// A table with no row there at all — a Database.Table handle, whose ObjectID
// is zero, or a memory-optimized table — reads as the zero DataSpace and no
// error: absence means "no filegroup to name", not a failure.
func (t *Table) DataSpaceContext(ctx context.Context) (DataSpace, error) {
	q := `
SELECT ` + dataSpaceColumns + `
FROM   sys.indexes i
` + dataSpaceJoins + `
WHERE  i.object_id = @p1 AND i.index_id IN (0, 1)`

	var ds DataSpace
	err := t.db.queryRow(ctx, func(row *sql.Row) error {
		return row.Scan(&ds.Name, &ds.IsPartitionScheme, &ds.IsDefaultFileGroup, &ds.PartitionColumn)
	}, q, t.ObjectID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return DataSpace{}, nil
		}
		return DataSpace{}, fmt.Errorf("gosmo: data space of %s: %w", t.FullName(), err)
	}
	return ds, nil
}

// indexColumnsContext returns every index column on the table, keyed by
// index_id and in each index's own key order.
//
// The rows for index_id 0 — the heap's, which no index in the list claims —
// come back too, and are simply never looked up: excluding them would cost a
// predicate to save nothing, since a heap has at most one such row.
//
// extra is an additional predicate ANDed onto the object filter, with its
// parameters starting at @p2 — the same contract as indexListContext.
func (t *Table) indexColumnsContext(ctx context.Context, extra string, args ...any) (map[int][]IndexColumn, error) {
	q := `
SELECT ic.index_id, c.name, ic.is_descending_key, ic.is_included_column
FROM   sys.index_columns ic
JOIN   sys.columns c ON c.object_id = ic.object_id AND c.column_id = ic.column_id
WHERE  ic.object_id = @p1` + extra + `
ORDER  BY ic.index_id, ic.key_ordinal, ic.index_column_id`

	rows, err := t.db.query(ctx, q, append([]any{t.ObjectID}, args...)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	cols := make(map[int][]IndexColumn)
	for rows.Next() {
		var indexID int
		c := IndexColumn{}
		if err := rows.Scan(&indexID, &c.Name, &c.Descending, &c.IsIncluded); err != nil {
			return nil, err
		}
		cols[indexID] = append(cols[indexID], c)
	}
	return cols, rows.Err()
}

// -- Foreign keys --------------------------------------------------------------

// ForeignKey mirrors Microsoft.SqlServer.Management.Smo.ForeignKey.
type ForeignKey struct {
	Name                string
	Columns             []string
	ReferencedTable     string
	ReferencedSchema    string
	ReferencedColumns   []string
	DeleteAction        string // NO_ACTION, CASCADE, SET_NULL, SET_DEFAULT
	UpdateAction        string
	IsDisabled          bool
	IsNotForReplication bool
}

// ForeignKeys returns all foreign keys on the table.
func (t *Table) ForeignKeys() ([]*ForeignKey, error) {
	return t.ForeignKeysContext(context.Background())
}

// foreignKeySelect is shared by ForeignKeysContext and
// ForeignKeyByNameContext so a foreign key carries the same fields however
// it was fetched.
const foreignKeySelect = `
SELECT fk.name, fk.is_disabled, fk.is_not_for_replication,
       fk.delete_referential_action_desc, fk.update_referential_action_desc,
       SCHEMA_NAME(rt.schema_id), rt.name,
       (SELECT STRING_AGG(c.name, ',') WITHIN GROUP (ORDER BY fkc.constraint_column_id)
        FROM   sys.foreign_key_columns fkc
        JOIN   sys.columns c
               ON  c.object_id = fkc.parent_object_id
               AND c.column_id = fkc.parent_column_id
        WHERE  fkc.constraint_object_id = fk.object_id),
       (SELECT STRING_AGG(c.name, ',') WITHIN GROUP (ORDER BY fkc.constraint_column_id)
        FROM   sys.foreign_key_columns fkc
        JOIN   sys.columns c
               ON  c.object_id = fkc.referenced_object_id
               AND c.column_id = fkc.referenced_column_id
        WHERE  fkc.constraint_object_id = fk.object_id)
FROM   sys.foreign_keys fk
JOIN   sys.tables rt ON rt.object_id = fk.referenced_object_id
WHERE  fk.parent_object_id = @p1`

// ForeignKeysContext is the context-aware variant of ForeignKeys.
func (t *Table) ForeignKeysContext(ctx context.Context) ([]*ForeignKey, error) {
	rows, err := t.db.query(ctx, foreignKeySelect+`
ORDER  BY fk.name`, t.ObjectID)
	if err != nil {
		return nil, fmt.Errorf("gosmo: list foreign keys for %s: %w", t.FullName(), err)
	}
	defer rows.Close()

	var fks []*ForeignKey
	for rows.Next() {
		fk, err := scanForeignKey(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("gosmo: list foreign keys for %s: %w", t.FullName(), err)
		}
		fks = append(fks, fk)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("gosmo: list foreign keys for %s: %w", t.FullName(), err)
	}
	return fks, nil
}

// ForeignKeyByName returns one foreign key on the table by name.
func (t *Table) ForeignKeyByName(name string) (*ForeignKey, error) {
	return t.ForeignKeyByNameContext(context.Background(), name)
}

// ForeignKeyByNameContext is the context-aware variant of ForeignKeyByName.
// It returns an error satisfying errors.Is(err, ErrNotFound) when the table
// has no such foreign key.
func (t *Table) ForeignKeyByNameContext(ctx context.Context, name string) (*ForeignKey, error) {
	var fk *ForeignKey
	err := t.db.queryRow(ctx, func(row *sql.Row) error {
		var err error
		fk, err = scanForeignKey(row.Scan)
		return err
	}, foreignKeySelect+`
       AND fk.name = @p2`, t.ObjectID, name)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, notFoundf("gosmo: foreign key %q not found on %s", name, t.FullName())
		}
		return nil, fmt.Errorf("gosmo: find foreign key %q on %s: %w", name, t.FullName(), err)
	}
	return fk, nil
}

func scanForeignKey(scan func(...any) error) (*ForeignKey, error) {
	fk := &ForeignKey{}
	var cols, refCols sql.NullString
	if err := scan(&fk.Name, &fk.IsDisabled, &fk.IsNotForReplication,
		&fk.DeleteAction, &fk.UpdateAction,
		&fk.ReferencedSchema, &fk.ReferencedTable,
		&cols, &refCols); err != nil {
		return nil, err
	}
	if cols.Valid {
		fk.Columns = strings.Split(cols.String, ",")
	}
	if refCols.Valid {
		fk.ReferencedColumns = strings.Split(refCols.String, ",")
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
}

// CheckConstraints returns all CHECK constraints on the table.
func (t *Table) CheckConstraints() ([]*CheckConstraint, error) {
	return t.CheckConstraintsContext(context.Background())
}

// CheckConstraintsContext is the context-aware variant of CheckConstraints.
func (t *Table) CheckConstraintsContext(ctx context.Context) ([]*CheckConstraint, error) {
	const q = `
SELECT cc.name, cc.definition, cc.is_disabled, ISNULL(c.name, '')
FROM   sys.check_constraints cc
LEFT   JOIN sys.columns c
       ON  c.object_id  = cc.parent_object_id
       AND c.column_id  = cc.parent_column_id
WHERE  cc.parent_object_id = @p1
ORDER  BY cc.name`

	rows, err := t.db.query(ctx, q, t.ObjectID)
	if err != nil {
		return nil, fmt.Errorf("gosmo: list check constraints for %s: %w", t.FullName(), err)
	}
	defer rows.Close()

	var ccs []*CheckConstraint
	for rows.Next() {
		cc := &CheckConstraint{}
		if err := rows.Scan(&cc.Name, &cc.Definition, &cc.IsDisabled, &cc.Column); err != nil {
			return nil, fmt.Errorf("gosmo: list check constraints for %s: %w", t.FullName(), err)
		}
		ccs = append(ccs, cc)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("gosmo: list check constraints for %s: %w", t.FullName(), err)
	}
	return ccs, nil
}

// -- Triggers --------------------------------------------------------------------

// Triggers returns all DML triggers attached to this table.
func (t *Table) Triggers() ([]*Trigger, error) {
	return t.TriggersContext(context.Background())
}

// TriggersContext is the context-aware variant of Triggers.
func (t *Table) TriggersContext(ctx context.Context) ([]*Trigger, error) {
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
	Name         string
	DataType     DataType
	MaxLength    int // char/varchar/nchar/nvarchar: 0 = omit, -1 = MAX
	Precision    int // decimal/numeric
	Scale        int // decimal/numeric / datetime2 / time
	IsNullable   bool
	IsIdentity   bool
	IdentitySeed int64
	IdentityIncr int64
	DefaultValue string // expression, e.g. "sysdatetime()" or "0"
	IsPrimaryKey bool
}

// CreateTable creates a table from a CreateTableRequest.
func (d *Database) CreateTable(req CreateTableRequest) error {
	return d.CreateTableContext(context.Background(), req)
}

// CreateTableContext is the context-aware variant of CreateTable.
func (d *Database) CreateTableContext(ctx context.Context, req CreateTableRequest) error {
	if req.Schema == "" {
		req.Schema = "dbo"
	}
	if req.Name == "" {
		return fmt.Errorf("gosmo: create table: name is required")
	}
	if len(req.Columns) == 0 {
		return fmt.Errorf("gosmo: create table: at least one column is required")
	}
	for _, col := range req.Columns {
		if !validDataType(col.DataType) {
			return fmt.Errorf("gosmo: create table %q: unrecognized data type %q for column %q", req.Name, col.DataType, col.Name)
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
		return fmt.Errorf("gosmo: create table %s: %w", qualifiedName(req.Schema, req.Name), err)
	}
	return nil
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
func (d *Database) DropTable(schema, name string, cascade bool) error {
	return d.DropTableContext(context.Background(), schema, name, cascade)
}

// DropTableContext is the context-aware variant of DropTable.
func (d *Database) DropTableContext(ctx context.Context, schema, name string, cascade bool) error {
	if cascade {
		const dropFKs = `
DECLARE @sql NVARCHAR(MAX) = N'';
SELECT @sql += N'ALTER TABLE ' + QUOTENAME(SCHEMA_NAME(fk.schema_id)) +
               N'.' + QUOTENAME(OBJECT_NAME(fk.parent_object_id)) +
               N' DROP CONSTRAINT ' + QUOTENAME(fk.name) + N'; '
FROM   sys.foreign_keys fk
WHERE  fk.referenced_object_id = OBJECT_ID(@p1);
IF LEN(@sql) > 0 EXEC sp_executesql @sql;`
		if _, err := d.exec(ctx, dropFKs, qualifiedName(schema, name)); err != nil {
			return fmt.Errorf("gosmo: drop incoming FKs for %s: %w", qualifiedName(schema, name), err)
		}
	}
	if _, err := d.exec(ctx, "DROP TABLE "+qualifiedName(schema, name)); err != nil {
		return fmt.Errorf("gosmo: drop table %s: %w", qualifiedName(schema, name), err)
	}
	return nil
}

// RenameTable renames a table using sp_rename.
func (d *Database) RenameTable(schema, oldName, newName string) error {
	return d.RenameTableContext(context.Background(), schema, oldName, newName)
}

// RenameTableContext is the context-aware variant of RenameTable.
func (d *Database) RenameTableContext(ctx context.Context, schema, oldName, newName string) error {
	if _, err := d.exec(ctx,
		"EXEC sp_rename @objname = @p1, @newname = @p2, @objtype = N'OBJECT'",
		qualifiedName(schema, oldName), newName,
	); err != nil {
		return fmt.Errorf("gosmo: rename table %s -> %s: %w", qualifiedName(schema, oldName), newName, err)
	}
	return nil
}

// TruncateTable truncates a table.
func (t *Table) TruncateTable() error {
	return t.TruncateTableContext(context.Background())
}

// TruncateTableContext is the context-aware variant of TruncateTable.
func (t *Table) TruncateTableContext(ctx context.Context) error {
	if _, err := t.db.exec(ctx, "TRUNCATE TABLE "+t.FullName()); err != nil {
		return fmt.Errorf("gosmo: truncate %s: %w", t.FullName(), err)
	}
	return nil
}

// RowCount returns the approximate row count using partition statistics.
func (t *Table) RowCount() (int64, error) {
	return t.RowCountContext(context.Background())
}

// RowCountContext is the context-aware variant of RowCount.
func (t *Table) RowCountContext(ctx context.Context) (int64, error) {
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
func (d *Database) TableRowCounts() (map[int]int64, error) {
	return d.TableRowCountsContext(context.Background())
}

// TableRowCountsContext is the context-aware variant of TableRowCounts.
func (d *Database) TableRowCountsContext(ctx context.Context) (map[int]int64, error) {
	const q = `
SELECT p.object_id, SUM(p.rows)
FROM   sys.partitions p
JOIN   sys.tables t ON t.object_id = p.object_id
WHERE  p.index_id IN (0, 1)
GROUP  BY p.object_id`

	rows, err := d.query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("gosmo: table row counts on %q: %w", d.name, err)
	}
	defer rows.Close()

	out := make(map[int]int64)
	for rows.Next() {
		var objectID int
		var n int64
		if err := rows.Scan(&objectID, &n); err != nil {
			return nil, fmt.Errorf("gosmo: table row counts on %q: %w", d.name, err)
		}
		out[objectID] = n
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("gosmo: table row counts on %q: %w", d.name, err)
	}
	return out, nil
}

// -- Predicate helpers -----------------------------------------------------

// CountWhere returns the number of rows in the table matching a WHERE
// predicate — used to estimate qualifying rows for a filtered index or
// filtered statistic's predicate (SSMS's "Estimate Rows" action).
func (t *Table) CountWhere(predicate string) (int64, error) {
	return t.CountWhereContext(context.Background(), predicate)
}

// CountWhereContext is the context-aware variant of CountWhere. predicate is
// interpolated as-is after WHERE; callers pass a filter expression already
// captured from the server (e.g. an index or statistic's own
// FilterDefinition), not raw user input.
func (t *Table) CountWhereContext(ctx context.Context, predicate string) (int64, error) {
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
func (t *Table) CheckWhereSyntax(predicate string) error {
	return t.CheckWhereSyntaxContext(context.Background(), predicate)
}

// CheckWhereSyntaxContext is the context-aware variant of CheckWhereSyntax.
func (t *Table) CheckWhereSyntaxContext(ctx context.Context, predicate string) error {
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
// scripter.go's ColumnTypeString does the equivalent for a *Column (from
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
		if col.Precision > 0 {
			return fmt.Sprintf("%s(%d,%d)", col.DataType, col.Precision, col.Scale)
		}
	case DataTypeDatetime2, DataTypeTime, DataTypeDatetimeOffset:
		if col.Scale > 0 {
			return fmt.Sprintf("%s(%d)", col.DataType, col.Scale)
		}
	}
	return string(col.DataType)
}

// DropConstraint drops a named table constraint — a PRIMARY KEY, UNIQUE
// constraint, FOREIGN KEY, or CHECK constraint. All four share one
// per-table name space and are all removed by ALTER TABLE ... DROP
// CONSTRAINT; an index that is not backing a key constraint is not a
// constraint and needs Index.Drop instead.
func (t *Table) DropConstraint(name string) error {
	return t.DropConstraintContext(context.Background(), name)
}

// DropConstraintContext is the context-aware variant of DropConstraint.
func (t *Table) DropConstraintContext(ctx context.Context, name string) error {
	q := fmt.Sprintf("ALTER TABLE %s DROP CONSTRAINT %s", t.FullName(), quoteIdent(name))
	if _, err := t.db.exec(ctx, q); err != nil {
		return fmt.Errorf("gosmo: drop constraint %q on %s: %w", name, t.FullName(), err)
	}
	return nil
}
