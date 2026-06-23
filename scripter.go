package gosmo

import (
	"context"
	"database/sql"
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
	// IncludeIfNotExists wraps DDL in existence checks.
	IncludeIfNotExists bool
	// IncludeDependencies pulls in referenced objects (best-effort).
	IncludeDependencies bool
	// ScriptDrops emits DROP statements instead of CREATE.
	ScriptDrops bool
	// SchemaQualify prefixes object names with their schema.
	SchemaQualify bool
	// AnsiPadding adds SET ANSI_PADDING ON/OFF.
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

// Scripter can generate T-SQL DDL scripts for objects in a database.
type Scripter struct {
	db   *Database
	opts ScriptOptions
}

// NewScripter creates a Scripter for the given database.
func NewScripter(db *Database, opts ScriptOptions) *Scripter {
	return &Scripter{db: db, opts: opts}
}

// ── Table ─────────────────────────────────────────────────────────────────────

// ScriptTable generates a CREATE TABLE (or DROP TABLE) script.
func (sc *Scripter) ScriptTable(schema, name string) (string, error) {
	t, err := sc.db.TableByName(schema, name)
	if err != nil {
		return "", err
	}
	cols, err := t.Columns()
	if err != nil {
		return "", err
	}
	indexes, err := t.Indexes()
	if err != nil {
		return "", err
	}
	fks, err := t.ForeignKeys()
	if err != nil {
		return "", err
	}

	var sb strings.Builder
	fullName := fmt.Sprintf("[%s].[%s]", schema, name)

	if sc.opts.ScriptDrops {
		if sc.opts.IncludeIfNotExists {
			fmt.Fprintf(&sb, "IF OBJECT_ID(N'%s.%s', N'U') IS NOT NULL\n", schema, name)
			sb.WriteString("    ")
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
		fmt.Fprintf(&sb, "IF OBJECT_ID(N'%s.%s', N'U') IS NULL\nBEGIN\n", schema, name)
	}

	fmt.Fprintf(&sb, "CREATE TABLE %s (\n", fullName)

	var pkCols []string
	for i, col := range cols {
		sb.WriteString("    ")
		fmt.Fprintf(&sb, "[%s] %s", col.Name, scriptColType(col))
		if col.IsIdentity {
			fmt.Fprintf(&sb, " IDENTITY(%d,%d)", col.IdentitySeed, col.IdentityIncrement)
		}
		if col.IsComputed && col.ComputedText != "" {
			// rewrite as computed
			sb.Reset()
			fmt.Fprintf(&sb, "    [%s] AS %s", col.Name, col.ComputedText)
		} else if !col.IsNullable {
			sb.WriteString(" NOT NULL")
		} else {
			sb.WriteString(" NULL")
		}
		if col.DefaultValue != nil {
			fmt.Fprintf(&sb, "\n        CONSTRAINT [%s] DEFAULT %s",
				col.DefaultValue.Name, col.DefaultValue.Definition)
		}
		// check if part of clustered PK
		for _, idx := range indexes {
			if idx.IsPrimaryKey && idx.IsClustered {
				for _, kc := range idx.KeyColumns {
					if strings.EqualFold(kc.Name, col.Name) {
						pkCols = append(pkCols, fmt.Sprintf("[%s]", col.Name))
					}
				}
				break
			}
		}
		if i < len(cols)-1 || len(pkCols) > 0 {
			sb.WriteString(",")
		}
		sb.WriteString("\n")
	}

	// Primary key constraint
	for _, idx := range indexes {
		if idx.IsPrimaryKey {
			keyCols := make([]string, len(idx.KeyColumns))
			for i, kc := range idx.KeyColumns {
				dir := "ASC"
				if kc.Descending {
					dir = "DESC"
				}
				keyCols[i] = fmt.Sprintf("[%s] %s", kc.Name, dir)
			}
			clust := "NONCLUSTERED"
			if idx.IsClustered {
				clust = "CLUSTERED"
			}
			fmt.Fprintf(&sb, "    CONSTRAINT [%s] PRIMARY KEY %s (%s)\n",
				idx.Name, clust, strings.Join(keyCols, ", "))
			break
		}
	}
	sb.WriteString(");\nGO\n\n")

	// Non-PK indexes
	for _, idx := range indexes {
		if idx.IsPrimaryKey {
			continue
		}
		sb.WriteString(sc.scriptIndex(idx, t))
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
		keyCols[i] = fmt.Sprintf("[%s] %s", kc.Name, dir)
	}
	fmt.Fprintf(&sb, "CREATE %s%s INDEX [%s]\n    ON %s (%s)",
		uniq, idx.Type, idx.Name, t.FullName(), strings.Join(keyCols, ", "))
	if len(idx.IncludedColumns) > 0 {
		inc := make([]string, len(idx.IncludedColumns))
		for i, c := range idx.IncludedColumns {
			inc[i] = fmt.Sprintf("[%s]", c.Name)
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
		cols[i] = fmt.Sprintf("[%s]", c)
	}
	refCols := make([]string, len(fk.ReferencedColumns))
	for i, c := range fk.ReferencedColumns {
		refCols[i] = fmt.Sprintf("[%s]", c)
	}
	fmt.Fprintf(&sb,
		"ALTER TABLE %s\n    ADD CONSTRAINT [%s]\n    FOREIGN KEY (%s)\n    REFERENCES [%s].[%s] (%s)",
		t.FullName(), fk.Name,
		strings.Join(cols, ", "),
		fk.ReferencedSchema, fk.ReferencedTable,
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

// ── View ──────────────────────────────────────────────────────────────────────

// ScriptView returns the CREATE VIEW definition.
func (sc *Scripter) ScriptView(schema, name string) (string, error) {
	row := sc.db.queryRow(context.Background(), `
SELECT m.definition
FROM   sys.views v
JOIN   sys.sql_modules m ON m.object_id = v.object_id
WHERE  SCHEMA_NAME(v.schema_id) = ? AND v.name = ?`, schema, name)

	var def string
	if err := row.Scan(&def); err != nil {
		if err == sql.ErrNoRows {
			return "", fmt.Errorf("gosmo: view [%s].[%s] not found", schema, name)
		}
		return "", err
	}
	if sc.opts.ScriptDrops {
		return fmt.Sprintf("DROP VIEW IF EXISTS [%s].[%s];\nGO\n", schema, name), nil
	}
	return def + "\nGO\n", nil
}

// ── Stored procedure ──────────────────────────────────────────────────────────

// ScriptStoredProcedure returns the CREATE PROCEDURE definition.
func (sc *Scripter) ScriptStoredProcedure(schema, name string) (string, error) {
	row := sc.db.queryRow(context.Background(), `
SELECT m.definition
FROM   sys.procedures p
JOIN   sys.sql_modules m ON m.object_id = p.object_id
WHERE  SCHEMA_NAME(p.schema_id) = ? AND p.name = ?`, schema, name)

	var def string
	if err := row.Scan(&def); err != nil {
		if err == sql.ErrNoRows {
			return "", fmt.Errorf("gosmo: stored procedure [%s].[%s] not found", schema, name)
		}
		return "", err
	}
	if sc.opts.ScriptDrops {
		return fmt.Sprintf("DROP PROCEDURE IF EXISTS [%s].[%s];\nGO\n", schema, name), nil
	}
	return def + "\nGO\n", nil
}

// ── Function ──────────────────────────────────────────────────────────────────

// ScriptFunction returns the CREATE FUNCTION definition.
func (sc *Scripter) ScriptFunction(schema, name string) (string, error) {
	row := sc.db.queryRow(context.Background(), `
SELECT m.definition
FROM   sys.objects o
JOIN   sys.sql_modules m ON m.object_id = o.object_id
WHERE  SCHEMA_NAME(o.schema_id) = ? AND o.name = ?
  AND  o.type IN ('FN','TF','IF')`, schema, name)

	var def string
	if err := row.Scan(&def); err != nil {
		if err == sql.ErrNoRows {
			return "", fmt.Errorf("gosmo: function [%s].[%s] not found", schema, name)
		}
		return "", err
	}
	if sc.opts.ScriptDrops {
		return fmt.Sprintf("DROP FUNCTION IF EXISTS [%s].[%s];\nGO\n", schema, name), nil
	}
	return def + "\nGO\n", nil
}

// ── Database-level script ─────────────────────────────────────────────────────

// ScriptDatabase generates a CREATE DATABASE script for the current database.
func (sc *Scripter) ScriptDatabase() (string, error) {
	var sb strings.Builder
	d := sc.db
	if sc.opts.IncludeHeaders {
		fmt.Fprintf(&sb, "/* Database: %s  Version: %s */\n\n", d.name, d.server.info.ProductVersion)
	}
	if sc.opts.IncludeIfNotExists {
		fmt.Fprintf(&sb, "IF DB_ID(N'%s') IS NULL\nBEGIN\n    ", escapeSingle(d.name))
	}
	fmt.Fprintf(&sb, "CREATE DATABASE [%s]", d.name)
	if d.collation != "" {
		fmt.Fprintf(&sb, " COLLATE %s", d.collation)
	}
	sb.WriteString(";\n")
	if sc.opts.IncludeIfNotExists {
		sb.WriteString("END\nGO\n\n")
	} else {
		sb.WriteString("GO\n\n")
	}
	fmt.Fprintf(&sb, "ALTER DATABASE [%s] SET RECOVERY %s;\nGO\n", d.name, d.recoveryModel)
	fmt.Fprintf(&sb, "ALTER DATABASE [%s] SET COMPATIBILITY_LEVEL = %d;\nGO\n", d.name, d.compatLevel)
	return sb.String(), nil
}

// ── Column type helper ────────────────────────────────────────────────────────

func scriptColType(col *Column) string {
	switch col.DataType {
	case DataTypeVarChar, DataTypeChar, DataTypeBinary, DataTypeVarBinary,
		DataTypeNVarChar, DataTypeNChar:
		if col.MaxLength == -1 {
			return fmt.Sprintf("%s(MAX)", col.DataType)
		}
		if col.MaxLength > 0 {
			l := col.MaxLength
			// nchar/nvarchar store length in bytes (2 per char)
			if col.DataType == DataTypeNVarChar || col.DataType == DataTypeNChar {
				l = l / 2
			}
			return fmt.Sprintf("%s(%d)", col.DataType, l)
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
