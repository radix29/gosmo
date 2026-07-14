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
}

// FullName returns [Schema].[Name].
func (t *Table) FullName() string { return qualifiedName(t.Schema, t.Name) }

// DB returns the parent Database.
func (t *Table) DB() *Database { return t.db }

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

// Columns returns all columns for this table in ordinal order.
func (t *Table) Columns() ([]*Column, error) {
	return t.ColumnsContext(context.Background())
}

// ColumnsContext is the context-aware variant of Columns.
func (t *Table) ColumnsContext(ctx context.Context) ([]*Column, error) {
	const q = `
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
       ) pk ON pk.object_id = c.object_id AND pk.column_id = c.column_id
WHERE  c.object_id = @p1
ORDER  BY c.column_id`

	rows, err := t.db.query(ctx, q, t.ObjectID)
	if err != nil {
		return nil, fmt.Errorf("gosmo: list columns for %s: %w", t.FullName(), err)
	}
	defer rows.Close()

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

// AddColumn adds a new column to the table (ALTER TABLE ... ADD).
func (t *Table) AddColumn(col ColumnDefinition) error {
	return t.AddColumnContext(context.Background(), col)
}

// AddColumnContext is the context-aware variant of AddColumn.
func (t *Table) AddColumnContext(ctx context.Context, col ColumnDefinition) error {
	if col.Name == "" {
		return fmt.Errorf("gosmo: add column: name is required")
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "ALTER TABLE %s ADD %s %s", t.FullName(), quoteIdent(col.Name), colTypeSQL(col))
	if col.IsIdentity {
		fmt.Fprintf(&sb, " IDENTITY(%d,%d)", col.IdentitySeed, col.IdentityIncr)
	}
	if col.IsNullable {
		sb.WriteString(" NULL")
	} else {
		sb.WriteString(" NOT NULL")
	}
	if col.DefaultValue != "" {
		fmt.Fprintf(&sb, " DEFAULT (%s)", col.DefaultValue)
	}

	if _, err := t.db.exec(ctx, sb.String()); err != nil {
		return fmt.Errorf("gosmo: add column %q to %s: %w", col.Name, t.FullName(), err)
	}
	return nil
}

// AlterColumn changes an existing column's data type and/or nullability
// (ALTER TABLE ... ALTER COLUMN). Identity and default are not settable this
// way — SQL Server requires dropping and re-adding the column (identity) or
// the default constraint (DropColumn/AddColumn) for those.
func (t *Table) AlterColumn(col ColumnDefinition) error {
	return t.AlterColumnContext(context.Background(), col)
}

// AlterColumnContext is the context-aware variant of AlterColumn.
func (t *Table) AlterColumnContext(ctx context.Context, col ColumnDefinition) error {
	if col.Name == "" {
		return fmt.Errorf("gosmo: alter column: name is required")
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

// DropColumn drops a column from the table (ALTER TABLE ... DROP COLUMN),
// first dropping its default constraint, if it has one — SQL Server refuses
// to drop a column that a default constraint still references.
func (t *Table) DropColumn(name string) error {
	return t.DropColumnContext(context.Background(), name)
}

// DropColumnContext is the context-aware variant of DropColumn.
func (t *Table) DropColumnContext(ctx context.Context, name string) error {
	const dropDefault = `
DECLARE @sql NVARCHAR(MAX) = N'';
SELECT @sql = N'ALTER TABLE ' + QUOTENAME(SCHEMA_NAME(tb.schema_id)) + N'.' + QUOTENAME(tb.name) +
              N' DROP CONSTRAINT ' + QUOTENAME(dc.name)
FROM   sys.default_constraints dc
JOIN   sys.columns c  ON c.object_id = dc.parent_object_id AND c.column_id = dc.parent_column_id
JOIN   sys.tables  tb ON tb.object_id = dc.parent_object_id
WHERE  dc.parent_object_id = @p1 AND c.name = @p2;
IF LEN(@sql) > 0 EXEC sp_executesql @sql;`

	if _, err := t.db.exec(ctx, dropDefault, t.ObjectID, name); err != nil {
		return fmt.Errorf("gosmo: drop default constraint on %s.%s: %w", t.FullName(), name, err)
	}
	if _, err := t.db.exec(ctx, fmt.Sprintf("ALTER TABLE %s DROP COLUMN %s", t.FullName(), quoteIdent(name))); err != nil {
		return fmt.Errorf("gosmo: drop column %q from %s: %w", name, t.FullName(), err)
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
	KeyColumns         []IndexColumn
	IncludedColumns    []IndexColumn
	FilterDefinition   string
}

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
func (t *Table) IndexesContext(ctx context.Context) ([]*Index, error) {
	const q = `
SELECT i.name, i.index_id, i.type_desc, i.is_unique, i.is_primary_key,
       i.is_unique_constraint, i.is_disabled, i.fill_factor,
       ISNULL(i.filter_definition, '')
FROM   sys.indexes i
WHERE  i.object_id = @p1 AND i.type > 0
ORDER  BY i.index_id`

	rows, err := t.db.query(ctx, q, t.ObjectID)
	if err != nil {
		return nil, fmt.Errorf("gosmo: list indexes for %s: %w", t.FullName(), err)
	}
	defer rows.Close()

	var indexes []*Index
	for rows.Next() {
		idx := &Index{}
		var typeDesc sql.NullString
		if err := rows.Scan(&idx.Name, &idx.IndexID, &typeDesc,
			&idx.IsUnique, &idx.IsPrimaryKey, &idx.IsUniqueConstraint,
			&idx.IsDisabled, &idx.FillFactor, &idx.FilterDefinition); err != nil {
			return nil, err
		}
		switch strings.TrimSpace(typeDesc.String) {
		case "CLUSTERED":
			idx.Type = IndexTypeClustered
			idx.IsClustered = true
		case "NONCLUSTERED":
			idx.Type = IndexTypeNonClustered
		case "XML":
			idx.Type = IndexTypeXML
		case "SPATIAL":
			idx.Type = IndexTypeSpatial
		case "CLUSTERED COLUMNSTORE", "NONCLUSTERED COLUMNSTORE":
			idx.Type = IndexTypeColumnStore
		}

		cols, err := t.indexColumnsContext(ctx, idx.IndexID)
		if err != nil {
			return nil, err
		}
		for _, c := range cols {
			if c.IsIncluded {
				idx.IncludedColumns = append(idx.IncludedColumns, c)
			} else {
				idx.KeyColumns = append(idx.KeyColumns, c)
			}
		}
		indexes = append(indexes, idx)
	}
	return indexes, rows.Err()
}

func (t *Table) indexColumnsContext(ctx context.Context, indexID int) ([]IndexColumn, error) {
	const q = `
SELECT c.name, ic.is_descending_key, ic.is_included_column
FROM   sys.index_columns ic
JOIN   sys.columns c ON c.object_id = ic.object_id AND c.column_id = ic.column_id
WHERE  ic.object_id = @p1 AND ic.index_id = @p2
ORDER  BY ic.key_ordinal, ic.index_column_id`

	rows, err := t.db.query(ctx, q, t.ObjectID, indexID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var cols []IndexColumn
	for rows.Next() {
		c := IndexColumn{}
		if err := rows.Scan(&c.Name, &c.Descending, &c.IsIncluded); err != nil {
			return nil, err
		}
		cols = append(cols, c)
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

// ForeignKeysContext is the context-aware variant of ForeignKeys.
func (t *Table) ForeignKeysContext(ctx context.Context) ([]*ForeignKey, error) {
	const q = `
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
WHERE  fk.parent_object_id = @p1
ORDER  BY fk.name`

	rows, err := t.db.query(ctx, q, t.ObjectID)
	if err != nil {
		return nil, fmt.Errorf("gosmo: list foreign keys for %s: %w", t.FullName(), err)
	}
	defer rows.Close()

	var fks []*ForeignKey
	for rows.Next() {
		fk := &ForeignKey{}
		var cols, refCols sql.NullString
		if err := rows.Scan(&fk.Name, &fk.IsDisabled, &fk.IsNotForReplication,
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
		fks = append(fks, fk)
	}
	return fks, rows.Err()
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
			return nil, err
		}
		ccs = append(ccs, cc)
	}
	return ccs, rows.Err()
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
	if _, err := d.exec(ctx, "DROP TABLE IF EXISTS "+qualifiedName(schema, name)); err != nil {
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
	row, release, err := t.db.queryRow(ctx, `
SELECT SUM(p.rows)
FROM   sys.partitions p
WHERE  p.object_id = @p1 AND p.index_id IN (0, 1)`, t.ObjectID)
	if err != nil {
		return 0, err
	}
	defer release()

	var n int64
	if err := row.Scan(&n); err != nil {
		return 0, fmt.Errorf("gosmo: row count for %s: %w", t.FullName(), err)
	}
	return n, nil
}

// -- Column type builder -------------------------------------------------------

// colTypeSQL returns the T-SQL data-type fragment for a ColumnDefinition.
// This is the single canonical implementation; scripter.go calls formatColumnType
// on a *Column (from sys.columns), which uses different field names.
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
