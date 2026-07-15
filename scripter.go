package gosmo

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// ============================================================
// Scripter  (mirrors Microsoft.SqlServer.Management.Smo.Scripter)
// ============================================================

// ScriptOptions controls how objects are scripted.
type ScriptOptions struct {
	// IncludeHeaders adds an informational header comment.
	IncludeHeaders bool
	// IncludeIfNotExists wraps DDL in an existence check.
	IncludeIfNotExists bool
	// ScriptDrops emits DROP statements instead of CREATE statements.
	ScriptDrops bool
	// SchemaQualify prefixes object names with their schema.
	SchemaQualify bool
	// AnsiPadding emits SET ANSI_PADDING ON before CREATE TABLE.
	AnsiPadding bool
}

// DefaultScriptOptions returns sensible defaults.
func DefaultScriptOptions() ScriptOptions {
	return ScriptOptions{
		IncludeHeaders:     true,
		SchemaQualify:      true,
		IncludeIfNotExists: true,
		AnsiPadding:        true,
	}
}

// Scripter generates T-SQL DDL scripts for objects in a database.
type Scripter struct {
	db   *Database
	opts ScriptOptions
}

// NewScripter creates a Scripter for the given database.
func NewScripter(db *Database, opts ScriptOptions) *Scripter {
	return &Scripter{db: db, opts: opts}
}

// ============================================================
// Table
// ============================================================

// ScriptTable generates a CREATE TABLE (or DROP TABLE) script.
func (sc *Scripter) ScriptTable(schema, name string) (string, error) {
	return sc.ScriptTableContext(context.Background(), schema, name)
}

// ScriptTableContext is the context-aware variant of ScriptTable.
func (sc *Scripter) ScriptTableContext(ctx context.Context, schema, name string) (string, error) {
	t, err := sc.db.TableByNameContext(ctx, schema, name)
	if err != nil {
		return "", err
	}
	cols, err := t.ColumnsContext(ctx)
	if err != nil {
		return "", err
	}
	indexes, err := t.IndexesContext(ctx)
	if err != nil {
		return "", err
	}
	fks, err := t.ForeignKeysContext(ctx)
	if err != nil {
		return "", err
	}

	fullName := qualifiedName(schema, name)
	var sb strings.Builder

	if sc.opts.ScriptDrops {
		if sc.opts.IncludeIfNotExists {
			fmt.Fprintf(&sb, "IF OBJECT_ID(N'%s.%s', N'U') IS NOT NULL\n    ",
				escapeSingle(schema), escapeSingle(name))
		}
		fmt.Fprintf(&sb, "DROP TABLE %s;\nGO\n", fullName)
		return sb.String(), nil
	}

	if sc.opts.IncludeHeaders {
		fmt.Fprintf(&sb, "/* Table: %s  Database: %s */\n", fullName, sc.db.name)
	}
	if sc.opts.AnsiPadding {
		sb.WriteString("SET ANSI_PADDING ON;\nGO\n\n")
	}
	if sc.opts.IncludeIfNotExists {
		fmt.Fprintf(&sb, "IF OBJECT_ID(N'%s.%s', N'U') IS NULL\nBEGIN\n",
			escapeSingle(schema), escapeSingle(name))
	}

	fmt.Fprintf(&sb, "CREATE TABLE %s (\n", fullName)

	// Find PK index once
	var pkIdx *Index
	for _, idx := range indexes {
		if idx.IsPrimaryKey {
			pkIdx = idx
			break
		}
	}

	for i, col := range cols {
		if col.IsComputed && col.ComputedText != "" {
			fmt.Fprintf(&sb, "    %s AS %s", quoteIdent(col.Name), col.ComputedText)
		} else {
			fmt.Fprintf(&sb, "    %s %s", quoteIdent(col.Name), ColumnTypeString(col))
			if col.IsIdentity {
				fmt.Fprintf(&sb, " IDENTITY(%d,%d)", col.IdentitySeed, col.IdentityIncrement)
			}
			if !col.IsNullable {
				sb.WriteString(" NOT NULL")
			} else {
				sb.WriteString(" NULL")
			}
			if col.DefaultValue != nil {
				fmt.Fprintf(&sb, " CONSTRAINT %s DEFAULT %s",
					quoteIdent(col.DefaultValue.Name), col.DefaultValue.Definition)
			}
		}
		isLast := i == len(cols)-1
		if !isLast || pkIdx != nil {
			sb.WriteString(",")
		}
		sb.WriteString("\n")
	}

	if pkIdx != nil {
		keyCols := make([]string, len(pkIdx.KeyColumns))
		for i, kc := range pkIdx.KeyColumns {
			dir := "ASC"
			if kc.Descending {
				dir = "DESC"
			}
			keyCols[i] = fmt.Sprintf("%s %s", quoteIdent(kc.Name), dir)
		}
		clust := "NONCLUSTERED"
		if pkIdx.IsClustered {
			clust = "CLUSTERED"
		}
		fmt.Fprintf(&sb, "    CONSTRAINT %s PRIMARY KEY %s (%s)\n",
			quoteIdent(pkIdx.Name), clust, strings.Join(keyCols, ", "))
	}

	sb.WriteString(");\nGO\n\n")

	// Non-PK indexes
	for _, idx := range indexes {
		if !idx.IsPrimaryKey {
			sb.WriteString(sc.scriptIndex(idx, t))
		}
	}

	// Foreign keys
	for _, fk := range fks {
		sb.WriteString(sc.scriptForeignKey(fk, t))
	}

	if sc.opts.IncludeIfNotExists {
		sb.WriteString("END\nGO\n")
	}
	return sb.String(), nil
}

func (sc *Scripter) scriptIndex(idx *Index, t *Table) string {
	var sb strings.Builder
	uniq := ""
	if idx.IsUnique {
		uniq = "UNIQUE "
	}
	keyCols := make([]string, len(idx.KeyColumns))
	for i, kc := range idx.KeyColumns {
		dir := "ASC"
		if kc.Descending {
			dir = "DESC"
		}
		keyCols[i] = fmt.Sprintf("%s %s", quoteIdent(kc.Name), dir)
	}
	fmt.Fprintf(&sb, "CREATE %s%s INDEX %s\n    ON %s (%s)",
		uniq, idx.Type, quoteIdent(idx.Name), t.FullName(), strings.Join(keyCols, ", "))
	if len(idx.IncludedColumns) > 0 {
		inc := make([]string, len(idx.IncludedColumns))
		for i, c := range idx.IncludedColumns {
			inc[i] = quoteIdent(c.Name)
		}
		fmt.Fprintf(&sb, "\n    INCLUDE (%s)", strings.Join(inc, ", "))
	}
	if idx.FilterDefinition != "" {
		fmt.Fprintf(&sb, "\n    WHERE %s", idx.FilterDefinition)
	}
	if idx.FillFactor > 0 {
		fmt.Fprintf(&sb, "\n    WITH (FILLFACTOR = %d)", idx.FillFactor)
	}
	sb.WriteString(";\nGO\n\n")
	return sb.String()
}

func (sc *Scripter) scriptForeignKey(fk *ForeignKey, t *Table) string {
	var sb strings.Builder
	cols := make([]string, len(fk.Columns))
	for i, c := range fk.Columns {
		cols[i] = quoteIdent(c)
	}
	refCols := make([]string, len(fk.ReferencedColumns))
	for i, c := range fk.ReferencedColumns {
		refCols[i] = quoteIdent(c)
	}
	fmt.Fprintf(&sb,
		"ALTER TABLE %s\n    ADD CONSTRAINT %s\n    FOREIGN KEY (%s)\n    REFERENCES %s (%s)",
		t.FullName(), quoteIdent(fk.Name),
		strings.Join(cols, ", "),
		qualifiedName(fk.ReferencedSchema, fk.ReferencedTable),
		strings.Join(refCols, ", "),
	)
	if fk.DeleteAction != "" && fk.DeleteAction != "NO_ACTION" {
		fmt.Fprintf(&sb, "\n    ON DELETE %s", strings.ReplaceAll(fk.DeleteAction, "_", " "))
	}
	if fk.UpdateAction != "" && fk.UpdateAction != "NO_ACTION" {
		fmt.Fprintf(&sb, "\n    ON UPDATE %s", strings.ReplaceAll(fk.UpdateAction, "_", " "))
	}
	sb.WriteString(";\nGO\n\n")
	return sb.String()
}

// ============================================================
// View
// ============================================================

// ScriptView returns the CREATE VIEW definition as stored in sys.sql_modules.
func (sc *Scripter) ScriptView(schema, name string) (string, error) {
	return sc.ScriptViewContext(context.Background(), schema, name)
}

// ScriptViewContext is the context-aware variant of ScriptView.
func (sc *Scripter) ScriptViewContext(ctx context.Context, schema, name string) (string, error) {
	if sc.opts.ScriptDrops {
		return fmt.Sprintf("DROP VIEW IF EXISTS %s;\nGO\n", qualifiedName(schema, name)), nil
	}
	row, release, err := sc.db.queryRow(ctx, `
SELECT m.definition
FROM   sys.views v
JOIN   sys.sql_modules m ON m.object_id = v.object_id
WHERE  SCHEMA_NAME(v.schema_id) = @p1 AND v.name = @p2`, schema, name)
	if err != nil {
		return "", err
	}
	defer release()

	var def string
	if err := row.Scan(&def); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", fmt.Errorf("gosmo: view %s not found", qualifiedName(schema, name))
		}
		return "", err
	}
	return def + "\nGO\n", nil
}

// ============================================================
// Stored Procedure
// ============================================================

// ScriptStoredProcedure returns the CREATE PROCEDURE definition.
func (sc *Scripter) ScriptStoredProcedure(schema, name string) (string, error) {
	return sc.ScriptStoredProcedureContext(context.Background(), schema, name)
}

// ScriptStoredProcedureContext is the context-aware variant.
func (sc *Scripter) ScriptStoredProcedureContext(ctx context.Context, schema, name string) (string, error) {
	if sc.opts.ScriptDrops {
		return fmt.Sprintf("DROP PROCEDURE IF EXISTS %s;\nGO\n", qualifiedName(schema, name)), nil
	}
	row, release, err := sc.db.queryRow(ctx, `
SELECT m.definition
FROM   sys.procedures p
JOIN   sys.sql_modules m ON m.object_id = p.object_id
WHERE  SCHEMA_NAME(p.schema_id) = @p1 AND p.name = @p2`, schema, name)
	if err != nil {
		return "", err
	}
	defer release()

	var def string
	if err := row.Scan(&def); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", fmt.Errorf("gosmo: stored procedure %s not found", qualifiedName(schema, name))
		}
		return "", err
	}
	return def + "\nGO\n", nil
}

// ============================================================
// Function
// ============================================================

// ScriptFunction returns the CREATE FUNCTION definition.
func (sc *Scripter) ScriptFunction(schema, name string) (string, error) {
	return sc.ScriptFunctionContext(context.Background(), schema, name)
}

// ScriptFunctionContext is the context-aware variant.
func (sc *Scripter) ScriptFunctionContext(ctx context.Context, schema, name string) (string, error) {
	if sc.opts.ScriptDrops {
		return fmt.Sprintf("DROP FUNCTION IF EXISTS %s;\nGO\n", qualifiedName(schema, name)), nil
	}
	row, release, err := sc.db.queryRow(ctx, `
SELECT m.definition
FROM   sys.objects o
JOIN   sys.sql_modules m ON m.object_id = o.object_id
WHERE  SCHEMA_NAME(o.schema_id) = @p1 AND o.name = @p2
  AND  o.type IN ('FN','TF','IF')`, schema, name)
	if err != nil {
		return "", err
	}
	defer release()

	var def string
	if err := row.Scan(&def); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", fmt.Errorf("gosmo: function %s not found", qualifiedName(schema, name))
		}
		return "", err
	}
	return def + "\nGO\n", nil
}

// ============================================================
// Database
// ============================================================

// ScriptDatabase generates a CREATE DATABASE script for the attached database.
func (sc *Scripter) ScriptDatabase() (string, error) {
	var sb strings.Builder
	d := sc.db
	if sc.opts.IncludeHeaders {
		fmt.Fprintf(&sb, "/* Database: %s  Version: %s */\n\n",
			d.name, d.server.info.ProductVersion)
	}
	if sc.opts.IncludeIfNotExists {
		fmt.Fprintf(&sb, "IF DB_ID(N'%s') IS NULL\nBEGIN\n    ", escapeSingle(d.name))
	}
	fmt.Fprintf(&sb, "CREATE DATABASE %s", quoteIdent(d.name))
	if d.collation != "" {
		fmt.Fprintf(&sb, " COLLATE %s", d.collation)
	}
	sb.WriteString(";\n")
	if sc.opts.IncludeIfNotExists {
		sb.WriteString("END\nGO\n\n")
	} else {
		sb.WriteString("GO\n\n")
	}
	fmt.Fprintf(&sb, "ALTER DATABASE %s SET RECOVERY %s;\nGO\n",
		quoteIdent(d.name), d.recoveryModel)
	fmt.Fprintf(&sb, "ALTER DATABASE %s SET COMPATIBILITY_LEVEL = %d;\nGO\n",
		quoteIdent(d.name), d.compatLevel)
	return sb.String(), nil
}

// ============================================================
// Column type formatting (used by ScriptTable and by callers rendering a
// Column's type for display, e.g. SSMS's Table Properties > Columns page)
// ============================================================

// ColumnTypeString returns the T-SQL data-type fragment for a Column read from
// sys.columns. nchar/nvarchar store max_length in bytes (2 per character).
func ColumnTypeString(col *Column) string {
	switch col.DataType {
	case DataTypeVarChar, DataTypeChar, DataTypeBinary, DataTypeVarBinary:
		if col.MaxLength == -1 {
			return fmt.Sprintf("%s(MAX)", col.DataType)
		}
		if col.MaxLength > 0 {
			return fmt.Sprintf("%s(%d)", col.DataType, col.MaxLength)
		}
	case DataTypeNVarChar, DataTypeNChar:
		if col.MaxLength == -1 {
			return fmt.Sprintf("%s(MAX)", col.DataType)
		}
		if col.MaxLength > 0 {
			// SQL Server stores nchar/nvarchar max_length in bytes (2 per char).
			return fmt.Sprintf("%s(%d)", col.DataType, col.MaxLength/2)
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
