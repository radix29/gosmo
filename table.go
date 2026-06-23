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
func (t *Table) FullName() string {
	return fmt.Sprintf("[%s].[%s]", t.Schema, t.Name)
}

// ── Columns ───────────────────────────────────────────────────────────────────

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
}

// Columns returns all columns for this table in ordinal order.
func (t *Table) Columns() ([]*Column, error) {
	const q = `
SELECT c.name, c.column_id,
       tp.name AS type_name, c.max_length, c.precision, c.scale,
       c.is_nullable, c.is_identity, c.is_computed,
       ISNULL(cc.definition, ''),
       ISNULL(dc.name,''), ISNULL(dc.definition,''),
       c.is_rowguidcol, ISNULL(c.collation_name,''),
       ISNULL(ic.seed_value, 0), ISNULL(ic.increment_value, 0)
FROM   sys.columns c
JOIN   sys.types tp ON tp.user_type_id = c.user_type_id
LEFT   JOIN sys.computed_columns cc ON cc.object_id = c.object_id AND cc.column_id = c.column_id
LEFT   JOIN sys.default_constraints dc ON dc.parent_object_id = c.object_id
                                       AND dc.parent_column_id = c.column_id
LEFT   JOIN sys.identity_columns ic ON ic.object_id = c.object_id AND ic.column_id = c.column_id
WHERE  c.object_id = ?
ORDER  BY c.column_id`

	rows, err := t.db.query(context.Background(), q, t.ObjectID)
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

// ── Indexes ───────────────────────────────────────────────────────────────────

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
	const q = `
SELECT i.name, i.index_id, i.type_desc, i.is_unique, i.is_primary_key,
       i.is_unique_constraint, i.is_disabled, i.fill_factor,
       ISNULL(i.filter_definition,'')
FROM   sys.indexes i
WHERE  i.object_id = ? AND i.type > 0
ORDER  BY i.index_id`

	rows, err := t.db.query(context.Background(), q, t.ObjectID)
	if err != nil {
		return nil, fmt.Errorf("gosmo: list indexes for %s: %w", t.FullName(), err)
	}
	defer rows.Close()

	var indexes []*Index
	for rows.Next() {
		idx := &Index{}
		var typeDesc sql.NullString
		var filterDef string
		if err := rows.Scan(&idx.Name, &idx.IndexID, &typeDesc,
			&idx.IsUnique, &idx.IsPrimaryKey, &idx.IsUniqueConstraint,
			&idx.IsDisabled, &idx.FillFactor, &filterDef); err != nil {
			return nil, err
		}
		idx.FilterDefinition = filterDef
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

		// Load columns
		cols, err := t.indexColumns(idx.IndexID)
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

func (t *Table) indexColumns(indexID int) ([]IndexColumn, error) {
	const q = `
SELECT c.name, ic.is_descending_key, ic.is_included_column
FROM   sys.index_columns ic
JOIN   sys.columns c ON c.object_id = ic.object_id AND c.column_id = ic.column_id
WHERE  ic.object_id = ? AND ic.index_id = ?
ORDER  BY ic.key_ordinal, ic.index_column_id`

	rows, err := t.db.query(context.Background(), q, t.ObjectID, indexID)
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

// ── Foreign keys ──────────────────────────────────────────────────────────────

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
	const q = `
SELECT fk.name, fk.is_disabled, fk.is_not_for_replication,
       fk.delete_referential_action_desc, fk.update_referential_action_desc,
       SCHEMA_NAME(rt.schema_id) AS ref_schema, rt.name AS ref_table,
       (SELECT STRING_AGG(c.name, ',') WITHIN GROUP (ORDER BY fkc.constraint_column_id)
        FROM sys.foreign_key_columns fkc
        JOIN sys.columns c ON c.object_id = fkc.parent_object_id AND c.column_id = fkc.parent_column_id
        WHERE fkc.constraint_object_id = fk.object_id) AS cols,
       (SELECT STRING_AGG(c.name, ',') WITHIN GROUP (ORDER BY fkc.constraint_column_id)
        FROM sys.foreign_key_columns fkc
        JOIN sys.columns c ON c.object_id = fkc.referenced_object_id AND c.column_id = fkc.referenced_column_id
        WHERE fkc.constraint_object_id = fk.object_id) AS ref_cols
FROM   sys.foreign_keys fk
JOIN   sys.tables rt ON rt.object_id = fk.referenced_object_id
WHERE  fk.parent_object_id = ?
ORDER  BY fk.name`

	rows, err := t.db.query(context.Background(), q, t.ObjectID)
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

// ── Check constraints ─────────────────────────────────────────────────────────

// CheckConstraint represents a CHECK constraint.
type CheckConstraint struct {
	Name       string
	Definition string
	IsDisabled bool
	Column     string
}

// CheckConstraints returns all CHECK constraints on the table.
func (t *Table) CheckConstraints() ([]*CheckConstraint, error) {
	const q = `
SELECT cc.name, cc.definition, cc.is_disabled,
       ISNULL(c.name,'')
FROM   sys.check_constraints cc
LEFT   JOIN sys.columns c ON c.object_id = cc.parent_object_id
                          AND c.column_id = cc.parent_column_id
WHERE  cc.parent_object_id = ?
ORDER  BY cc.name`

	rows, err := t.db.query(context.Background(), q, t.ObjectID)
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

// ── DDL helpers ───────────────────────────────────────────────────────────────

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
	MaxLength    int // used for char/varchar/nchar/nvarchar; 0 = omit; -1 = MAX
	Precision    int // for decimal/numeric
	Scale        int // for decimal/numeric
	IsNullable   bool
	IsIdentity   bool
	IdentitySeed int64
	IdentityIncr int64
	DefaultValue string // e.g. "getdate()" or "0"
	IsPrimaryKey bool
}

// CreateTable creates a table from a CreateTableRequest.
func (d *Database) CreateTable(req CreateTableRequest) error {
	var sb strings.Builder
	fmt.Fprintf(&sb, "CREATE TABLE [%s].[%s] (\n", req.Schema, req.Name)

	var pkCols []string
	for i, col := range req.Columns {
		fmt.Fprintf(&sb, "    [%s] %s", col.Name, colTypeSQL(col))
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
			pkCols = append(pkCols, fmt.Sprintf("[%s]", col.Name))
		}
		if i < len(req.Columns)-1 || len(pkCols) > 0 {
			sb.WriteString(",")
		}
		sb.WriteString("\n")
	}
	if len(pkCols) > 0 {
		fmt.Fprintf(&sb, "    CONSTRAINT [PK_%s] PRIMARY KEY CLUSTERED (%s)\n",
			req.Name, strings.Join(pkCols, ", "))
	}
	sb.WriteString(")")

	_, err := d.exec(context.Background(), sb.String())
	if err != nil {
		return fmt.Errorf("gosmo: create table [%s].[%s]: %w", req.Schema, req.Name, err)
	}
	return nil
}

// DropTable drops a table. Set cascade=true to first drop all FK constraints pointing to it.
func (d *Database) DropTable(schema, name string, cascade bool) error {
	if cascade {
		// Disable / drop FKs referencing this table first
		_, _ = d.exec(context.Background(), fmt.Sprintf(`
DECLARE @sql NVARCHAR(MAX) = '';
SELECT @sql += 'ALTER TABLE ['+SCHEMA_NAME(fk.schema_id)+'].['+OBJECT_NAME(fk.parent_object_id)+
               '] DROP CONSTRAINT ['+fk.name+']; '
FROM   sys.foreign_keys fk
WHERE  OBJECT_NAME(fk.referenced_object_id) = N'%s'
  AND  SCHEMA_NAME(OBJECT_SCHEMA_ID(fk.referenced_object_id)) = N'%s';
EXEC sp_executesql @sql;`, escapeSingle(name), escapeSingle(schema)))
	}
	_, err := d.exec(context.Background(),
		fmt.Sprintf("DROP TABLE IF EXISTS [%s].[%s]", schema, name))
	if err != nil {
		return fmt.Errorf("gosmo: drop table [%s].[%s]: %w", schema, name, err)
	}
	return nil
}

// RenameTable renames a table using sp_rename.
func (d *Database) RenameTable(schema, oldName, newName string) error {
	_, err := d.exec(context.Background(),
		fmt.Sprintf("EXEC sp_rename N'[%s].[%s]', N'%s', 'OBJECT'",
			schema, oldName, escapeSingle(newName)))
	if err != nil {
		return fmt.Errorf("gosmo: rename table: %w", err)
	}
	return nil
}

// TruncateTable truncates a table.
func (t *Table) TruncateTable() error {
	_, err := t.db.exec(context.Background(),
		fmt.Sprintf("TRUNCATE TABLE %s", t.FullName()))
	if err != nil {
		return fmt.Errorf("gosmo: truncate %s: %w", t.FullName(), err)
	}
	return nil
}

// RowCount returns the approximate row count using partition stats.
func (t *Table) RowCount() (int64, error) {
	var n int64
	row := t.db.queryRow(context.Background(), `
SELECT SUM(p.rows) FROM sys.partitions p
WHERE  p.object_id = ? AND p.index_id IN (0,1)`, t.ObjectID)
	if err := row.Scan(&n); err != nil {
		return 0, fmt.Errorf("gosmo: row count for %s: %w", t.FullName(), err)
	}
	return n, nil
}

// ── Column type builder ───────────────────────────────────────────────────────

func colTypeSQL(col ColumnDefinition) string {
	switch col.DataType {
	case DataTypeVarChar, DataTypeChar, DataTypeBinary, DataTypeVarBinary:
		if col.MaxLength == -1 {
			return fmt.Sprintf("%s(MAX)", col.DataType)
		} else if col.MaxLength > 0 {
			return fmt.Sprintf("%s(%d)", col.DataType, col.MaxLength)
		}
	case DataTypeNVarChar, DataTypeNChar:
		if col.MaxLength == -1 {
			return fmt.Sprintf("%s(MAX)", col.DataType)
		} else if col.MaxLength > 0 {
			return fmt.Sprintf("%s(%d)", col.DataType, col.MaxLength)
		}
	case DataTypeDecimal, DataTypeNumeric:
		if col.Precision > 0 {
			return fmt.Sprintf("%s(%d,%d)", col.DataType, col.Precision, col.Scale)
		}
	}
	return string(col.DataType)
}
