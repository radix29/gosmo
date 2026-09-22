package gosmo

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode"
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

// Database returns the database the scripter scripts.
func (sc *Scripter) Database() *Database { return sc.db }

// NewScripter creates a Scripter for the given database.
func NewScripter(db *Database, opts ScriptOptions) *Scripter {
	return &Scripter{db: db, opts: opts}
}

// ============================================================
// Table
// ============================================================

// ScriptTable generates a CREATE TABLE (or DROP TABLE) script.
func (sc *Scripter) ScriptTable(ctx context.Context, schema, name string) (string, error) {
	t, err := sc.db.TableByName(ctx, schema, name)
	if err != nil {
		return "", err
	}
	var p tableScriptParts
	if p.cols, err = t.Columns(ctx); err != nil {
		return "", err
	}
	if p.indexes, err = t.Indexes(ctx); err != nil {
		return "", err
	}
	if p.fks, err = t.ForeignKeys(ctx); err != nil {
		return "", err
	}
	if p.checks, err = t.CheckConstraints(ctx); err != nil {
		return "", err
	}
	// The table's own ON clause comes from its heap or clustered index, and
	// a heap is not in the index list at all — see Table.DataSpace.
	if p.ds, err = t.DataSpace(ctx); err != nil {
		return "", err
	}
	if p.table, err = t.scriptOptions(ctx); err != nil {
		return "", err
	}
	return buildTableScript(schema, name, sc.db.Name, p, sc.opts), nil
}

// tableScriptParts is everything buildTableScript renders from, read by
// ScriptTable.
type tableScriptParts struct {
	cols    []*Column
	indexes []*Index
	fks     []*ForeignKey
	checks  []*CheckConstraint
	ds      DataSpace
	table   tableScriptOptions
}

// tableScriptOptions is what a CREATE TABLE says about the table as a whole
// that no column, index or constraint carries.
type tableScriptOptions struct {
	// SystemVersioned is a system-versioned temporal table, whose history
	// table is HistorySchema.HistoryTable.
	SystemVersioned             bool
	HistorySchema, HistoryTable string
	// PeriodStart and PeriodEnd are the PERIOD FOR SYSTEM_TIME columns; ""
	// when the table has no period.
	PeriodStart, PeriodEnd string
	// HeapCompression is a heap's DATA_COMPRESSION ("NONE" when it is not
	// compressed, "" when the table is not a heap). A clustered table's
	// compression is its clustered index's, and is rendered with that.
	HeapCompression string
	// DatabaseCollation is the database default, against which a column's
	// own collation is compared: a column that differs from it gets a
	// COLLATE clause, since the script would otherwise recreate it with
	// the target database's default.
	DatabaseCollation string
}

// scriptOptions reads tableScriptOptions in one round trip.
func (t *Table) scriptOptions(ctx context.Context) (tableScriptOptions, error) {
	const q = `
SELECT CAST(CASE WHEN t.temporal_type = 2 THEN 1 ELSE 0 END AS BIT),
       ISNULL(OBJECT_SCHEMA_NAME(t.history_table_id), ''),
       ISNULL(OBJECT_NAME(t.history_table_id), ''),
       ISNULL(COL_NAME(p.object_id, p.start_column_id), ''),
       ISNULL(COL_NAME(p.object_id, p.end_column_id), ''),
       ISNULL((SELECT TOP 1 pp.data_compression_desc FROM sys.partitions pp
               WHERE pp.object_id = t.object_id AND pp.index_id = 0
               ORDER BY pp.partition_number), ''),
       ISNULL(CONVERT(sysname, DATABASEPROPERTYEX(DB_NAME(), 'Collation')), '')
FROM   sys.tables AS t
LEFT   JOIN sys.periods p ON p.object_id = t.object_id
WHERE  t.object_id = @p1`
	var o tableScriptOptions
	if err := t.db.queryRow(ctx, func(row *sql.Row) error {
		return row.Scan(&o.SystemVersioned, &o.HistorySchema, &o.HistoryTable,
			&o.PeriodStart, &o.PeriodEnd, &o.HeapCompression, &o.DatabaseCollation)
	}, q, t.ObjectID); err != nil && !errors.Is(err, sql.ErrNoRows) {
		// No row is a table dropped since it was looked up; the column and
		// index reads beside this one answer that with empty lists, and so
		// does this.
		return o, fmt.Errorf("gosmo: script options for %s: %w", t.FullName(), err)
	}
	return o, nil
}

// buildTableScript assembles the CREATE (or DROP) TABLE script from metadata
// already read. Split out of ScriptTable so the assembly — where every
// bug this has had has lived — can be unit-tested without a server; the
// method above is then only the catalog reads that feed it.
func buildTableScript(schema, name, dbName string, p tableScriptParts, opts ScriptOptions) string {
	fullName := qualifiedName(schema, name)
	var sb strings.Builder

	if v := opts.verb(); v == ScriptDrop || v == ScriptDropAndCreate {
		// DROP TABLE refuses a system-versioned table; versioning has to be
		// switched off first. The history table is left in place, so a DROP
		// And CREATE reattaches it by name and keeps the history.
		if p.table.SystemVersioned {
			if opts.IncludeIfNotExists {
				fmt.Fprintf(&sb, "IF OBJECT_ID(N'%s', N'U') IS NOT NULL\n    ",
					escapeSingle(fullName))
			}
			fmt.Fprintf(&sb, "ALTER TABLE %s SET (SYSTEM_VERSIONING = OFF);\nGO\n", fullName)
		}
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

	var pkIdx *Index
	for _, idx := range p.indexes {
		if idx.IsPrimaryKey {
			pkIdx = idx
			break
		}
	}

	elems := make([]string, 0, len(p.cols)+2)
	for _, col := range p.cols {
		elems = append(elems, tableColumnDefinition(col, p.table.DatabaseCollation))
	}
	if p.table.PeriodStart != "" && p.table.PeriodEnd != "" {
		elems = append(elems, fmt.Sprintf("PERIOD FOR SYSTEM_TIME (%s, %s)",
			quoteIdent(p.table.PeriodStart), quoteIdent(p.table.PeriodEnd)))
	}
	if pkIdx != nil {
		clust := "NONCLUSTERED"
		if pkIdx.IsClustered {
			clust = "CLUSTERED"
		}
		elems = append(elems, fmt.Sprintf("CONSTRAINT %s PRIMARY KEY %s (%s)%s%s",
			quoteIdent(pkIdx.Name), clust, indexColumnList(pkIdx.KeyColumns),
			indexWithClause(pkIdx, " "), dataSpaceClause(pkIdx.DataSpace)))
	}
	for i, e := range elems {
		sb.WriteString("    ")
		sb.WriteString(e)
		if i < len(elems)-1 {
			sb.WriteString(",")
		}
		sb.WriteString("\n")
	}

	var tableOpts []string
	if c := p.table.HeapCompression; c != "" && c != "NONE" {
		tableOpts = append(tableOpts, "DATA_COMPRESSION = "+c)
	}
	if p.table.SystemVersioned {
		if p.table.HistoryTable != "" {
			tableOpts = append(tableOpts, fmt.Sprintf("SYSTEM_VERSIONING = ON (HISTORY_TABLE = %s)",
				qualifiedName(p.table.HistorySchema, p.table.HistoryTable)))
		} else {
			tableOpts = append(tableOpts, "SYSTEM_VERSIONING = ON")
		}
	}
	with := ""
	if len(tableOpts) > 0 {
		with = "\nWITH (" + strings.Join(tableOpts, ", ") + ")"
	}
	fmt.Fprintf(&sb, ")%s%s;\nGO\n\n", dataSpaceClause(p.ds), with)

	// Non-PK indexes. A unique *constraint* is backed by an index in
	// sys.indexes but is not created with CREATE INDEX — it belongs to the
	// table, and scripting it as an index leaves the constraint missing.
	for _, idx := range p.indexes {
		if idx.IsPrimaryKey {
			continue
		}
		if idx.IsUniqueConstraint {
			sb.WriteString(scriptUniqueConstraint(idx, fullName, opts))
			continue
		}
		sb.WriteString(scriptIndex(idx, fullName, opts))
	}

	for _, fk := range p.fks {
		sb.WriteString(scriptForeignKey(fk, fullName, opts))
	}
	for _, ck := range p.checks {
		sb.WriteString(scriptCheckConstraint(ck, fullName, opts))
	}

	// Disabled indexes are disabled last. Disabling a clustered index takes
	// the whole table offline, so doing it where the index is created would
	// fail every CREATE INDEX and ADD CONSTRAINT after it.
	for _, idx := range p.indexes {
		sb.WriteString(indexDisableStatement(idx, fullName))
	}

	return sb.String()
}

// tableColumnDefinition renders one column of a CREATE TABLE, in the clause
// order the CREATE TABLE grammar lists. dbCollation is the database default;
// a column collated differently gets a COLLATE clause.
func tableColumnDefinition(col *Column, dbCollation string) string {
	var sb strings.Builder
	name := quoteIdent(col.Name)
	switch {
	case col.IsComputed && col.ComputedText != "":
		fmt.Fprintf(&sb, "%s AS %s", name, col.ComputedText)
		if col.IsPersisted {
			sb.WriteString(" PERSISTED")
			// NOT NULL is valid only on a persisted computed column; a
			// non-persisted one's nullability is derived from its expression.
			if !col.IsNullable {
				sb.WriteString(" NOT NULL")
			}
		}
		return sb.String()
	case col.IsColumnSet:
		// A column set takes no nullability clause: it is always nullable.
		fmt.Fprintf(&sb, "%s %s COLUMN_SET FOR ALL_SPARSE_COLUMNS", name, ColumnTypeString(col))
		return sb.String()
	}
	fmt.Fprintf(&sb, "%s %s", name, ColumnTypeString(col))
	if col.Collation != "" && dbCollation != "" && !strings.EqualFold(col.Collation, dbCollation) {
		fmt.Fprintf(&sb, " COLLATE %s", col.Collation)
	}
	if col.IsSparse {
		sb.WriteString(" SPARSE")
	}
	if col.MaskingFunction != "" {
		fmt.Fprintf(&sb, " MASKED WITH (FUNCTION = '%s')", escapeSingle(col.MaskingFunction))
	}
	if col.IsIdentity {
		fmt.Fprintf(&sb, " IDENTITY(%d,%d)", col.IdentitySeed, col.IdentityIncrement)
		if col.IdentityNotForReplication {
			sb.WriteString(" NOT FOR REPLICATION")
		}
	}
	switch col.GeneratedAlwaysType {
	case 1:
		sb.WriteString(" GENERATED ALWAYS AS ROW START")
	case 2:
		sb.WriteString(" GENERATED ALWAYS AS ROW END")
	}
	if col.IsHidden {
		sb.WriteString(" HIDDEN")
	}
	if col.IsRowGUID {
		sb.WriteString(" ROWGUIDCOL")
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
		fmt.Fprintf(&sb, "CREATE CLUSTERED COLUMNSTORE INDEX %s ON %s%s%s;\nGO\n\n",
			quoteIdent(idx.Name), tableName, columnstoreWithClause(idx), dataSpaceClause(idx.DataSpace))
		return sb.String()
	case idx.Type == IndexTypeColumnStore:
		cols := make([]string, len(idx.KeyColumns))
		for i, kc := range idx.KeyColumns {
			cols[i] = quoteIdent(kc.Name)
		}
		if opts.IncludeIfNotExists {
			sb.WriteString(indexExistenceGuard(idx.Name, tableName))
		}
		fmt.Fprintf(&sb, "CREATE NONCLUSTERED COLUMNSTORE INDEX %s\n    ON %s (%s)%s%s;\nGO\n\n",
			quoteIdent(idx.Name), tableName, strings.Join(cols, ", "), columnstoreWithClause(idx),
			dataSpaceClause(idx.DataSpace))
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
	sb.WriteString(indexWithClause(idx, "\n    "))
	sb.WriteString(dataSpaceClause(idx.DataSpace))
	sb.WriteString(";\nGO\n\n")
	return sb.String()
}

// indexWithClause renders a rowstore index's WITH (...) options — only those
// that differ from what CREATE INDEX does anyway — preceded by sep, or ""
// when there are none. It serves CREATE INDEX and the PRIMARY KEY / UNIQUE
// constraints alike, which take the same options.
func indexWithClause(idx *Index, sep string) string {
	var o []string
	if idx.IsPadded {
		o = append(o, "PAD_INDEX = ON")
	}
	if idx.FillFactor > 0 {
		o = append(o, fmt.Sprintf("FILLFACTOR = %d", idx.FillFactor))
	}
	if idx.IgnoreDupKey {
		o = append(o, "IGNORE_DUP_KEY = ON")
	}
	if !idx.AllowRowLocks {
		o = append(o, "ALLOW_ROW_LOCKS = OFF")
	}
	if !idx.AllowPageLocks {
		o = append(o, "ALLOW_PAGE_LOCKS = OFF")
	}
	if c := idx.DataCompression; c == "ROW" || c == "PAGE" {
		o = append(o, "DATA_COMPRESSION = "+c)
	}
	if len(o) == 0 {
		return ""
	}
	return sep + "WITH (" + strings.Join(o, ", ") + ")"
}

// columnstoreWithClause is indexWithClause for a columnstore index, whose
// only non-default compression is COLUMNSTORE_ARCHIVE.
func columnstoreWithClause(idx *Index) string {
	if idx.DataCompression == "COLUMNSTORE_ARCHIVE" {
		return " WITH (DATA_COMPRESSION = COLUMNSTORE_ARCHIVE)"
	}
	return ""
}

// indexDisableStatement renders the ALTER INDEX ... DISABLE that leaves a
// recreated index disabled as its source was, or "" for an enabled one.
func indexDisableStatement(idx *Index, tableName string) string {
	if !idx.IsDisabled {
		return ""
	}
	return fmt.Sprintf("ALTER INDEX %s ON %s DISABLE;\nGO\n\n", quoteIdent(idx.Name), tableName)
}

// dataSpaceClause renders the ON clause naming where an object's rows go, or
// "" where it would say nothing.
//
// A partition scheme is always emitted, and this is the whole point: without
// it a partitioned table or index is recreated on the default filegroup —
// silently unpartitioned, which no error anywhere reports. A filegroup is
// emitted only when it is not the default one, since ON [PRIMARY] is what
// the server does anyway and naming a filegroup the target database may not
// have turns a script that would have worked into one that fails.
func dataSpaceClause(ds DataSpace) string {
	switch {
	case ds.IsPartitionScheme && ds.PartitionColumn != "":
		return fmt.Sprintf(" ON %s(%s)", quoteIdent(ds.Name), quoteIdent(ds.PartitionColumn))
	case ds.Name == "" || ds.IsDefaultFileGroup || ds.IsPartitionScheme:
		// A partition scheme with no partitioning column is not a clause
		// anything can be written from: emit nothing rather than DDL that
		// cannot parse.
		return ""
	default:
		return fmt.Sprintf(" ON %s", quoteIdent(ds.Name))
	}
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
	fmt.Fprintf(&sb, "ALTER TABLE %s\n    ADD CONSTRAINT %s UNIQUE %s (%s)%s%s;\nGO\n\n",
		tableName, quoteIdent(idx.Name), clust, indexColumnList(idx.KeyColumns),
		indexWithClause(idx, " "), dataSpaceClause(idx.DataSpace))
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
SELECT m.definition, m.uses_ansi_nulls, m.uses_quoted_identifier
FROM   sys.views v
JOIN   sys.sql_modules m ON m.object_id = v.object_id
WHERE  SCHEMA_NAME(v.schema_id) = @p1 AND v.name = @p2`}

	moduleProcedure = moduleKind{"PROCEDURE", "stored procedure", `
SELECT m.definition, m.uses_ansi_nulls, m.uses_quoted_identifier
FROM   sys.procedures p
JOIN   sys.sql_modules m ON m.object_id = p.object_id
WHERE  SCHEMA_NAME(p.schema_id) = @p1 AND p.name = @p2`}

	moduleFunction = moduleKind{"FUNCTION", "function", `
SELECT m.definition, m.uses_ansi_nulls, m.uses_quoted_identifier
FROM   sys.objects o
JOIN   sys.sql_modules m ON m.object_id = o.object_id
WHERE  SCHEMA_NAME(o.schema_id) = @p1 AND o.name = @p2
  AND  o.type IN ('FN','TF','IF')`}

	// A trigger's own schema is its parent table's — sys.triggers has no
	// schema_id of its own.
	moduleTrigger = moduleKind{"TRIGGER", "trigger", `
SELECT m.definition, m.uses_ansi_nulls, m.uses_quoted_identifier
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
	var ansiNulls, quotedIdent bool
	err := sc.db.queryRow(ctx, func(row *sql.Row) error {
		return row.Scan(&def, &ansiNulls, &quotedIdent)
	}, k.query, schema, name)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", notFoundf("gosmo: %s %s not found", k.noun, qualifiedName(schema, name))
		}
		return "", err
	}
	if v == ScriptAlter {
		def = alterModuleDefinition(def)
	}
	sb.WriteString(moduleSetOptions(ansiNulls, quotedIdent))
	sb.WriteString(def)
	sb.WriteString("\nGO\n")
	return sb.String(), nil
}

// moduleSetOptions renders the two SET options a module is compiled under,
// each in its own batch ahead of the definition, as SSMS does.
//
// They are not decoration. A module keeps the ANSI_NULLS and
// QUOTED_IDENTIFIER settings of the session that created it, and they govern
// what it does: whether "= NULL" ever matches, and whether "x" is a string
// or an identifier. Without them the module is recreated under whatever the
// running session has — ON for gossms and SSMS — so a legacy module written
// under OFF silently changes behaviour, or fails to compile where it used
// "string" literals.
func moduleSetOptions(ansiNulls, quotedIdent bool) string {
	return fmt.Sprintf("SET ANSI_NULLS %s;\nGO\nSET QUOTED_IDENTIFIER %s;\nGO\n", onOff(ansiNulls), onOff(quotedIdent))
}

// createKeyword matches the CREATE that opens a module definition once its
// leading whitespace and comments are skipped — the only CREATE that may be
// rewritten to ALTER. A CREATE elsewhere in the body (a temp table, say) must
// be left exactly as the author wrote it.
var createKeyword = regexp.MustCompile(`(?is)^CREATE(\s+OR\s+ALTER)?(\s)`)

// alterModuleDefinition rewrites a module's stored CREATE into an ALTER,
// leaving a definition it can't recognize untouched — an unrecognized one
// still runs, as the CREATE it already was, which beats emitting mangled DDL.
// A CREATE OR ALTER definition is already re-runnable and is returned as is.
func alterModuleDefinition(def string) string {
	at := leadingTriviaEnd(def)
	if at < 0 {
		return def
	}
	m := createKeyword.FindStringSubmatch(def[at:])
	if m == nil || m[1] != "" {
		return def
	}
	return def[:at] + "ALTER" + m[2] + def[at+len(m[0]):]
}

// leadingTriviaEnd returns the offset of the first character of def that is
// neither whitespace nor inside a comment, or -1 if there is none or the
// first thing def holds is quoted text rather than code.
//
// This is scriptCodeSpans' walk, not a regular expression, because T-SQL
// block comments nest: a lazy /\*.*?\*/ stops at the first */ of
// "/* a /* b */ c */", so a definition opening with a nested comment was
// not recognised and Script as ALTER handed back the CREATE, which then
// failed with "There is already an object named …".
func leadingTriviaEnd(def string) int {
	for _, sp := range scriptCodeSpans(def) {
		if sp.start > sp.prev && def[sp.prev] != '-' && def[sp.prev] != '/' {
			return -1
		}
		code := def[sp.start:sp.end]
		if i := strings.IndexFunc(code, func(r rune) bool { return !unicode.IsSpace(r) }); i >= 0 {
			return sp.start + i
		}
	}
	return -1
}

// ============================================================
// View
// ============================================================

// ScriptView returns the CREATE VIEW definition as stored in sys.sql_modules.
func (sc *Scripter) ScriptView(ctx context.Context, schema, name string) (string, error) {
	return sc.scriptModule(ctx, moduleView, schema, name)
}

// ============================================================
// Stored Procedure
// ============================================================

// ScriptStoredProcedure returns the CREATE PROCEDURE definition.
func (sc *Scripter) ScriptStoredProcedure(ctx context.Context, schema, name string) (string, error) {
	return sc.scriptModule(ctx, moduleProcedure, schema, name)
}

// ============================================================
// Function
// ============================================================

// ScriptFunction returns the CREATE FUNCTION definition.
func (sc *Scripter) ScriptFunction(ctx context.Context, schema, name string) (string, error) {
	return sc.scriptModule(ctx, moduleFunction, schema, name)
}

// ============================================================
// Trigger
// ============================================================

// ScriptTrigger returns the CREATE TRIGGER definition. schema is the
// trigger's own schema, i.e. its parent table's.
func (sc *Scripter) ScriptTrigger(ctx context.Context, schema, name string) (string, error) {
	return sc.scriptModule(ctx, moduleTrigger, schema, name)
}

// ============================================================
// Database Trigger
// ============================================================

// ScriptDatabaseTrigger generates the CREATE (or DROP) script for one
// database-scope DDL trigger.
//
// scriptModule is not reusable here: it addresses a module by schema and name,
// and a DDL trigger has no schema.
func (sc *Scripter) ScriptDatabaseTrigger(ctx context.Context, name string) (string, error) {
	t, err := sc.db.DatabaseTriggerByName(ctx, name)
	if err != nil {
		return "", err
	}
	return buildDatabaseTriggerScript(t, sc.opts)
}

// buildDatabaseTriggerScript assembles one database trigger's script from the
// definition sys.sql_modules stores.
//
// A trigger with no readable definition — encrypted, or CLR, which has no row
// in that view at all — is an error rather than an empty CREATE half: emitting
// nothing produces a script that drops the trigger and does not put it back.
// IncludeIfNotExists is not honoured because CREATE TRIGGER must be the first
// statement in its batch, the same reason scriptModule ignores it. Both are
// buildServerTriggerScript's reasoning, unchanged; only the scope clause
// differs.
func buildDatabaseTriggerScript(t *DatabaseTrigger, opts ScriptOptions) (string, error) {
	var sb strings.Builder
	if v := opts.verb(); v == ScriptDrop || v == ScriptDropAndCreate {
		fmt.Fprintf(&sb, "DROP TRIGGER IF EXISTS %s ON DATABASE;\nGO\n", quoteIdent(t.Name))
		if v == ScriptDrop {
			return sb.String(), nil
		}
		sb.WriteString("\n")
	}
	if strings.TrimSpace(t.Definition) == "" {
		return "", fmt.Errorf("gosmo: script database trigger %q: definition is not readable (encrypted or CLR)", t.Name)
	}
	def := t.Definition
	if opts.verb() == ScriptAlter {
		def = alterModuleDefinition(def)
	}
	sb.WriteString(def)
	sb.WriteString("\nGO\n")
	if !t.IsEnabled {
		fmt.Fprintf(&sb, "DISABLE TRIGGER %s ON DATABASE;\nGO\n", quoteIdent(t.Name))
	}
	return sb.String(), nil
}

// ============================================================
// Database
// ============================================================

// ScriptDatabase generates a CREATE DATABASE script for the attached database.
//
// The context is not decoration. Alone among the Script* methods this one
// renders from the Database's own cached metadata rather than querying, and a
// Database from Server.DatabaseRef(name) carries none — it is a bare handle
// by design. Rendering that handle emitted "SET RECOVERY ;" and
// "COMPATIBILITY_LEVEL = 0", neither of which is valid T-SQL, so a handle with
// no recovery model is refilled from sys.databases first. Each line is still
// guarded on its own value: a refresh that cannot run leaves the script short
// a setting, which is recoverable, rather than syntactically broken, which is
// not.
func (sc *Scripter) ScriptDatabase(ctx context.Context) (string, error) {
	d := sc.db
	if d.RecoveryModel == "" || d.CompatibilityLevel == 0 {
		full, err := d.server.DatabaseByName(ctx, d.Name)
		if err != nil {
			return "", fmt.Errorf("gosmo: script database %q: %w", d.Name, err)
		}
		d = full
	}
	return sc.scriptDatabaseFrom(d)
}

// scriptDatabaseFrom renders the script from d's metadata, with no reads of
// its own.
func (sc *Scripter) scriptDatabaseFrom(d *Database) (string, error) {
	var sb strings.Builder
	if sc.opts.IncludeHeaders {
		// info is nil on a Server built without NewServer; the header reports
		// no version rather than panicking.
		version := ""
		if d.server != nil && d.server.info != nil {
			version = d.server.info.ProductVersion
		}
		fmt.Fprintf(&sb, "/* Database: %s  Version: %s */\n\n", d.Name, version)
	}
	if sc.opts.IncludeIfNotExists {
		fmt.Fprintf(&sb, "IF DB_ID(N'%s') IS NULL\nBEGIN\n    ", escapeSingle(d.Name))
	}
	fmt.Fprintf(&sb, "CREATE DATABASE %s", quoteIdent(d.Name))
	if d.Collation != "" {
		fmt.Fprintf(&sb, " COLLATE %s", d.Collation)
	}
	sb.WriteString(";\n")
	if sc.opts.IncludeIfNotExists {
		sb.WriteString("END\nGO\n\n")
	} else {
		sb.WriteString("GO\n\n")
	}
	if d.RecoveryModel != "" {
		fmt.Fprintf(&sb, "ALTER DATABASE %s SET RECOVERY %s;\nGO\n",
			quoteIdent(d.Name), d.RecoveryModel)
	}
	if d.CompatibilityLevel != 0 {
		fmt.Fprintf(&sb, "ALTER DATABASE %s SET COMPATIBILITY_LEVEL = %d;\nGO\n",
			quoteIdent(d.Name), d.CompatibilityLevel)
	}
	return sb.String(), nil
}

// ============================================================
// Column type formatting (used by ScriptTable and by callers rendering a
// Column's type for display, e.g. SSMS's Table Properties > Columns page)
// ============================================================

// ColumnTypeString returns the T-SQL data-type fragment for a Column read from
// sys.columns.
//
// A user-defined alias or CLR type is schema-qualified and bracketed
// ([dbo].[Phone]), and never given a length: an alias type's length is part
// of its own definition, and an unqualified name resolves against the
// executing user's default schema — a different type, or none.
func ColumnTypeString(col *Column) string {
	if col.IsUserDefinedType {
		return qualifiedName(col.TypeSchema, string(col.DataType))
	}
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
		// Always emitted: the bare type means scale 7, so dropping a zero
		// scale — the common "no fractional seconds" choice — turned
		// datetime2(0) into datetime2(7). The catalog always reports one.
		return fmt.Sprintf("%s(%d)", dt, scale)
	}
	return string(dt)
}
