package gosmo

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// ============================================================
// Scripter  (mirrors Microsoft.SqlServer.Management.Smo.Scripter)
// ============================================================

// ScriptVerb selects which statement form a Scripter emits.
type ScriptVerb int

const (
	// ScriptCreate emits the object's CREATE statement (the zero value).
	ScriptCreate ScriptVerb = iota
	// ScriptDrop emits its DROP statement.
	ScriptDrop
	// ScriptDropAndCreate emits the DROP followed by the CREATE, in that
	// order and in separate batches — the re-runnable form.
	ScriptDropAndCreate
	// ScriptAlter emits ALTER instead of CREATE. Only module objects
	// (views, stored procedures, functions, triggers) have an ALTER form
	// that restates the whole object; everything else falls back to CREATE.
	ScriptAlter
)

// ScriptOptions controls how objects are scripted.
type ScriptOptions struct {
	// Verb selects CREATE (the default), DROP, DROP-and-CREATE, or ALTER.
	Verb ScriptVerb
	// IncludeHeaders adds an informational header comment. Applies to
	// ScriptTable and ScriptDatabase only.
	IncludeHeaders bool
	// IncludeIfNotExists guards each generated statement with its own
	// existence check. Applies to ScriptTable and ScriptDatabase only:
	// ScriptView/StoredProcedure/Function return the module's definition
	// verbatim from sys.sql_modules and don't synthesize DDL to guard.
	//
	// The guard is per statement, never a block spanning several. A BEGIN
	// block containing GO separators is split across batches — GO is a
	// client-side batch break — leaving an unclosed BEGIN in one batch and a
	// bare END in another, which is a script that cannot parse.
	IncludeIfNotExists bool
	// ScriptDrops is the older, narrower spelling of Verb = ScriptDrop, and
	// still honoured: it applies only while Verb is left at its zero value.
	// New code should set Verb.
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

// verb resolves which statement form to emit, folding the older
// ScriptDrops bool into the Verb it stands for. Verb wins whenever it is set
// to anything but its zero value, so a caller that sets both is not silently
// given the drop.
func (o ScriptOptions) verb() ScriptVerb {
	if o.Verb == ScriptCreate && o.ScriptDrops {
		return ScriptDrop
	}
	return o.Verb
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

	return buildTableScript(schema, name, sc.db.name, cols, indexes, fks, sc.opts), nil
}

// buildTableScript assembles the CREATE (or DROP) TABLE script from metadata
// already read. Split out of ScriptTableContext so the assembly — where every
// bug this has had has lived — can be unit-tested without a server; the
// method above is then only the four catalog reads that feed it.
func buildTableScript(schema, name, dbName string, cols []*Column, indexes []*Index, fks []*ForeignKey, opts ScriptOptions) string {
	fullName := qualifiedName(schema, name)
	var sb strings.Builder

	if v := opts.verb(); v == ScriptDrop || v == ScriptDropAndCreate {
		if opts.IncludeIfNotExists {
			fmt.Fprintf(&sb, "IF OBJECT_ID(N'%s', N'U') IS NOT NULL\n    ",
				escapeSingle(fullName))
		}
		fmt.Fprintf(&sb, "DROP TABLE %s;\nGO\n", fullName)
		if v == ScriptDrop {
			return sb.String()
		}
		sb.WriteString("\n")
	}

	if opts.IncludeHeaders {
		fmt.Fprintf(&sb, "/* Table: %s  Database: %s */\n", fullName, dbName)
	}
	if opts.AnsiPadding {
		sb.WriteString("SET ANSI_PADDING ON;\nGO\n\n")
	}
	// The existence check guards only the CREATE TABLE, and closes before the
	// batch does. Wrapping the indexes and foreign keys in it too — which is
	// what this did — put GO separators inside a BEGIN block: GO is a
	// client-side batch break, so the block was split across batches, leaving
	// batch one with an unclosed BEGIN and the last batch a bare END. The
	// whole script failed to parse. Each following statement gets its own
	// guard instead, which is also what SSMS emits.
	if opts.IncludeIfNotExists {
		fmt.Fprintf(&sb, "IF OBJECT_ID(N'%s', N'U') IS NULL\n", escapeSingle(fullName))
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
		clust := "NONCLUSTERED"
		if pkIdx.IsClustered {
			clust = "CLUSTERED"
		}
		fmt.Fprintf(&sb, "    CONSTRAINT %s PRIMARY KEY %s (%s)\n",
			quoteIdent(pkIdx.Name), clust, indexColumnList(pkIdx.KeyColumns))
	}

	sb.WriteString(");\nGO\n\n")

	// Non-PK indexes. A unique *constraint* is backed by an index in
	// sys.indexes but is not created with CREATE INDEX — it belongs to the
	// table, and scripting it as an index leaves the constraint missing.
	for _, idx := range indexes {
		if idx.IsPrimaryKey {
			continue
		}
		if idx.IsUniqueConstraint {
			sb.WriteString(scriptUniqueConstraint(idx, fullName, opts))
			continue
		}
		sb.WriteString(scriptIndex(idx, fullName, opts))
	}

	// Foreign keys
	for _, fk := range fks {
		sb.WriteString(scriptForeignKey(fk, fullName, opts))
	}

	return sb.String()
}

// indexColumnList renders an index's key columns with their sort direction.
func indexColumnList(cols []IndexColumn) string {
	out := make([]string, len(cols))
	for i, kc := range cols {
		dir := "ASC"
		if kc.Descending {
			dir = "DESC"
		}
		out[i] = fmt.Sprintf("%s %s", quoteIdent(kc.Name), dir)
	}
	return strings.Join(out, ", ")
}

// scriptIndex renders one CREATE INDEX statement for a table-level index.
//
// The index type decides the grammar, not just a keyword: a clustered
// columnstore index takes no column list at all, a nonclustered columnstore
// takes columns but rejects ASC/DESC, and XML/spatial indexes have their own
// syntax entirely (a USING/primary-XML-index clause, a bounding box). Pasting
// the type_desc into the B-tree form — which is what this did — emits DDL SQL
// Server rejects, so those cases are emitted as a comment naming what was
// skipped rather than as a statement that cannot run.
func scriptIndex(idx *Index, tableName string, opts ScriptOptions) string {
	var sb strings.Builder
	switch {
	case idx.Type == IndexTypeClusteredColumnStore:
		if opts.IncludeIfNotExists {
			sb.WriteString(indexExistenceGuard(idx.Name, tableName))
		}
		fmt.Fprintf(&sb, "CREATE CLUSTERED COLUMNSTORE INDEX %s ON %s;\nGO\n\n",
			quoteIdent(idx.Name), tableName)
		return sb.String()
	case idx.Type == IndexTypeColumnStore:
		cols := make([]string, len(idx.KeyColumns))
		for i, kc := range idx.KeyColumns {
			cols[i] = quoteIdent(kc.Name)
		}
		if opts.IncludeIfNotExists {
			sb.WriteString(indexExistenceGuard(idx.Name, tableName))
		}
		fmt.Fprintf(&sb, "CREATE NONCLUSTERED COLUMNSTORE INDEX %s\n    ON %s (%s);\nGO\n\n",
			quoteIdent(idx.Name), tableName, strings.Join(cols, ", "))
		return sb.String()
	case idx.Type == IndexTypeXML || idx.Type == IndexTypeSpatial:
		fmt.Fprintf(&sb, "-- %s index %s on %s is not scripted (its DDL has no generic form here).\n\n",
			idx.Type, quoteIdent(idx.Name), tableName)
		return sb.String()
	}

	if opts.IncludeIfNotExists {
		sb.WriteString(indexExistenceGuard(idx.Name, tableName))
	}
	uniq := ""
	if idx.IsUnique {
		uniq = "UNIQUE "
	}
	clust := "NONCLUSTERED"
	if idx.IsClustered {
		clust = "CLUSTERED"
	}
	fmt.Fprintf(&sb, "CREATE %s%s INDEX %s\n    ON %s (%s)",
		uniq, clust, quoteIdent(idx.Name), tableName, indexColumnList(idx.KeyColumns))
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

// indexExistenceGuard renders the one-line IF that skips a CREATE INDEX when
// the index is already there. A single statement, so no BEGIN block — see
// buildTableScript on why a block here would break the batch.
func indexExistenceGuard(indexName, tableName string) string {
	return fmt.Sprintf("IF NOT EXISTS (SELECT 1 FROM sys.indexes WHERE name = N'%s' AND object_id = OBJECT_ID(N'%s'))\n",
		escapeSingle(indexName), escapeSingle(tableName))
}

// scriptUniqueConstraint renders a unique constraint backed by idx as the
// ALTER TABLE ... ADD CONSTRAINT it really is.
func scriptUniqueConstraint(idx *Index, tableName string, opts ScriptOptions) string {
	var sb strings.Builder
	if opts.IncludeIfNotExists {
		sb.WriteString(constraintExistenceGuard(idx.Name, tableName))
	}
	clust := "NONCLUSTERED"
	if idx.IsClustered {
		clust = "CLUSTERED"
	}
	fmt.Fprintf(&sb, "ALTER TABLE %s\n    ADD CONSTRAINT %s UNIQUE %s (%s);\nGO\n\n",
		tableName, quoteIdent(idx.Name), clust, indexColumnList(idx.KeyColumns))
	return sb.String()
}

// constraintExistenceGuard renders the one-line IF that skips adding a
// constraint that already exists on tableName.
//
// Scoped by parent_object_id, not by name alone: constraint names are only
// unique per schema, so a bare name match would skip a genuinely missing
// constraint because something unrelated elsewhere shares its name — and a
// false positive here means silently omitting DDL, which is worse than
// re-running it and getting an error. Both unique constraints and foreign
// keys carry the table as their parent.
func constraintExistenceGuard(name, tableName string) string {
	return fmt.Sprintf("IF NOT EXISTS (SELECT 1 FROM sys.objects WHERE name = N'%s' AND parent_object_id = OBJECT_ID(N'%s'))\n",
		escapeSingle(name), escapeSingle(tableName))
}

func scriptForeignKey(fk *ForeignKey, tableName string, opts ScriptOptions) string {
	var sb strings.Builder
	if opts.IncludeIfNotExists {
		sb.WriteString(constraintExistenceGuard(fk.Name, tableName))
	}
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
		tableName, quoteIdent(fk.Name),
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
// Modules (view, stored procedure, function, trigger)
// ============================================================

// moduleKind describes one family of sys.sql_modules-backed objects: the DDL
// keyword its DROP uses, the noun a not-found error names it by, and the
// catalog query that finds its definition by schema and name.
type moduleKind struct {
	keyword string
	noun    string
	query   string
}

var (
	moduleView = moduleKind{"VIEW", "view", `
SELECT m.definition
FROM   sys.views v
JOIN   sys.sql_modules m ON m.object_id = v.object_id
WHERE  SCHEMA_NAME(v.schema_id) = @p1 AND v.name = @p2`}

	moduleProcedure = moduleKind{"PROCEDURE", "stored procedure", `
SELECT m.definition
FROM   sys.procedures p
JOIN   sys.sql_modules m ON m.object_id = p.object_id
WHERE  SCHEMA_NAME(p.schema_id) = @p1 AND p.name = @p2`}

	moduleFunction = moduleKind{"FUNCTION", "function", `
SELECT m.definition
FROM   sys.objects o
JOIN   sys.sql_modules m ON m.object_id = o.object_id
WHERE  SCHEMA_NAME(o.schema_id) = @p1 AND o.name = @p2
  AND  o.type IN ('FN','TF','IF')`}

	// A trigger's own schema is its parent table's — sys.triggers has no
	// schema_id of its own.
	moduleTrigger = moduleKind{"TRIGGER", "trigger", `
SELECT m.definition
FROM   sys.triggers tr
JOIN   sys.objects o     ON o.object_id = tr.parent_id
JOIN   sys.sql_modules m ON m.object_id = tr.object_id
WHERE  SCHEMA_NAME(o.schema_id) = @p1 AND tr.name = @p2`}
)

// scriptModule renders one sys.sql_modules-backed object. CREATE and ALTER
// are the stored definition itself (rewritten for ALTER), never synthesized,
// so nothing about the original text is lost.
func (sc *Scripter) scriptModule(ctx context.Context, k moduleKind, schema, name string) (string, error) {
	v := sc.opts.verb()
	var sb strings.Builder
	if v == ScriptDrop || v == ScriptDropAndCreate {
		fmt.Fprintf(&sb, "DROP %s IF EXISTS %s;\nGO\n", k.keyword, qualifiedName(schema, name))
		if v == ScriptDrop {
			return sb.String(), nil
		}
		sb.WriteString("\n")
	}
	var def string
	err := sc.db.queryRow(ctx, func(row *sql.Row) error { return row.Scan(&def) }, k.query, schema, name)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", notFoundf("gosmo: %s %s not found", k.noun, qualifiedName(schema, name))
		}
		return "", err
	}
	if v == ScriptAlter {
		def = alterModuleDefinition(def)
	}
	sb.WriteString(def + "\nGO\n")
	return sb.String(), nil
}

// createKeyword matches the CREATE that opens a module definition, after any
// leading whitespace and line or block comments — the only CREATE that may be
// rewritten to ALTER. A CREATE elsewhere in the body (a temp table, say) must
// be left exactly as the author wrote it.
var createKeyword = regexp.MustCompile(`(?is)^((?:\s|--[^\n]*\n|/\*.*?\*/)*)CREATE(\s+OR\s+ALTER)?(\s)`)

// alterModuleDefinition rewrites a module's stored CREATE into an ALTER,
// leaving a definition it can't recognize untouched — an unrecognized one
// still runs, as the CREATE it already was, which beats emitting mangled DDL.
// A CREATE OR ALTER definition is already re-runnable and is returned as is.
func alterModuleDefinition(def string) string {
	m := createKeyword.FindStringSubmatch(def)
	if m == nil || m[2] != "" {
		return def
	}
	return m[1] + "ALTER" + m[3] + def[len(m[0]):]
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
	return sc.scriptModule(ctx, moduleView, schema, name)
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
	return sc.scriptModule(ctx, moduleProcedure, schema, name)
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
	return sc.scriptModule(ctx, moduleFunction, schema, name)
}

// ============================================================
// Trigger
// ============================================================

// ScriptTrigger returns the CREATE TRIGGER definition. schema is the
// trigger's own schema, i.e. its parent table's.
func (sc *Scripter) ScriptTrigger(schema, name string) (string, error) {
	return sc.ScriptTriggerContext(context.Background(), schema, name)
}

// ScriptTriggerContext is the context-aware variant of ScriptTrigger.
func (sc *Scripter) ScriptTriggerContext(ctx context.Context, schema, name string) (string, error) {
	return sc.scriptModule(ctx, moduleTrigger, schema, name)
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
// sys.columns.
func ColumnTypeString(col *Column) string {
	return sqlTypeString(col.DataType, col.MaxLength, col.Precision, col.Scale)
}

// sqlTypeString renders a catalog data type with whatever length, precision
// or scale that type actually carries — shared by ColumnTypeString and
// Parameter.TypeString, which read the same columns out of sys.columns and
// sys.parameters. nchar/nvarchar store max_length in bytes (2 per
// character); -1 is MAX.
func sqlTypeString(dt DataType, maxLength, precision, scale int) string {
	switch dt {
	case DataTypeVarChar, DataTypeChar, DataTypeBinary, DataTypeVarBinary:
		if maxLength == -1 {
			return fmt.Sprintf("%s(MAX)", dt)
		}
		if maxLength > 0 {
			return fmt.Sprintf("%s(%d)", dt, maxLength)
		}
	case DataTypeNVarChar, DataTypeNChar:
		if maxLength == -1 {
			return fmt.Sprintf("%s(MAX)", dt)
		}
		if maxLength > 0 {
			return fmt.Sprintf("%s(%d)", dt, maxLength/2)
		}
	case DataTypeDecimal, DataTypeNumeric:
		if precision > 0 {
			return fmt.Sprintf("%s(%d,%d)", dt, precision, scale)
		}
	case DataTypeDatetime2, DataTypeTime, DataTypeDatetimeOffset:
		if scale > 0 {
			return fmt.Sprintf("%s(%d)", dt, scale)
		}
	}
	return string(dt)
}
