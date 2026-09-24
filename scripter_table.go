package gosmo

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
)

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
	// The table-wide options come first: they decide whether this table is
	// one the scripter refuses, and a refusal must come before any script.
	if p.table, err = t.scriptOptions(ctx); err != nil {
		return "", err
	}
	if err := p.table.refusal(t.FullName()); err != nil {
		return "", err
	}
	if p.cols, err = t.Columns(ctx); err != nil {
		return "", err
	}
	if p.table.IsExternal {
		// An external table has no indexes, constraints or data space of
		// its own; what it has instead is the data source and file format
		// it reads through.
		return sc.scriptExternalTable(ctx, schema, name, p)
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
	if t.IsEdge {
		if p.ecs, err = t.EdgeConstraints(ctx); err != nil {
			return "", err
		}
	}
	if p.table.IsFileTable {
		return buildFileTableScript(schema, name, sc.db.Name, p, sc.opts), nil
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
	ecs     []*EdgeConstraint
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
	// HeapPartitionCompression is each of the heap's partitions'
	// compression, in partition_number order; nil when the table is not a
	// heap.
	HeapPartitionCompression []DataCompression
	// DatabaseCollation is the database default, against which a column's
	// own collation is compared: a column that differs from it gets a
	// COLLATE clause, since the script would otherwise recreate it with
	// the target database's default.
	DatabaseCollation string

	// The table's kind, copied from Table: a node or edge table (AS NODE /
	// AS EDGE), a memory-optimized one, and the two kinds refusal turns
	// away.
	IsNode, IsEdge, IsMemoryOptimized bool
	IsFileTable, IsExternal           bool
	// Durability is a memory-optimized table's DURABILITY, SCHEMA_AND_DATA or
	// SCHEMA_ONLY.
	Durability string
	// LobDataSpace is the TEXTIMAGE_ON data space, "" when the table has
	// none; LobIsFileGroup is false for a partition scheme, which takes no
	// TEXTIMAGE_ON clause. FileStreamDataSpace is the FILESTREAM_ON one.
	LobDataSpace        string
	LobIsFileGroup      bool
	FileStreamDataSpace string
	// LedgerType is sys.tables.ledger_type: 0 for a non-ledger table, 1 for
	// an updatable ledger table's history table, 2 for an updatable ledger
	// table and 3 for an append-only one. Always 0 before SQL Server 2022.
	LedgerType int
	// IsDroppedLedgerTable is a ledger table that has been dropped, which
	// the ledger keeps under a MSSQL_DroppedLedgerTable_… name.
	IsDroppedLedgerTable bool
	// LedgerViewSchema and LedgerViewName name a ledger table's ledger view,
	// and LedgerViewColumns the view's four ledger columns, in the order
	// LEDGER_VIEW takes them: transaction id, sequence number, operation
	// type, operation type description.
	LedgerViewSchema, LedgerViewName string
	LedgerViewColumns                []string

	// A FileTable's directory, file-name collation and whether its
	// namespace is enabled (sys.filetables), and the names of the
	// constraints and indexes AS FILETABLE created for it — which the table
	// script must not create a second time.
	FileTableDirectory, FileTableCollation string
	FileTableNamespaceEnabled              bool
	FileTableSystemObjects                 []string

	// External is an external table's sys.external_tables row; zero for any
	// other table.
	External externalTableOptions
}

// externalTableOptions is what CREATE EXTERNAL TABLE's WITH clause says,
// read from sys.external_tables.
type externalTableOptions struct {
	DataSource, FileFormat, Location string
	// RejectType is VALUE or PERCENTAGE, "" for a source that takes no
	// reject options; RejectSampleValue applies to PERCENTAGE only.
	RejectType                     string
	RejectValue, RejectSampleValue sql.NullFloat64
	// RemoteSchema and RemoteObject are an elastic-query table's SCHEMA_NAME
	// and OBJECT_NAME, and Distribution with ShardingColumn a shard-map
	// table's DISTRIBUTION; "" when the table has none.
	RemoteSchema, RemoteObject   string
	Distribution, ShardingColumn string
}

// scriptOptions reads tableScriptOptions in one round trip.
func (t *Table) scriptOptions(ctx context.Context) (tableScriptOptions, error) {
	o := tableScriptOptions{
		IsNode: t.IsNode, IsEdge: t.IsEdge, IsMemoryOptimized: t.IsMemoryOptimized,
		IsFileTable: t.IsFileTable, IsExternal: t.IsExternal,
	}
	var heap, ledgerCols, ftObjects sql.NullString
	if err := t.db.queryRow(ctx, func(row *sql.Row) error {
		e := &o.External
		if err := row.Scan(&o.SystemVersioned, &o.HistorySchema, &o.HistoryTable,
			&o.PeriodStart, &o.PeriodEnd, &heap, &o.DatabaseCollation,
			&o.Durability, &o.LobDataSpace, &o.LobIsFileGroup, &o.FileStreamDataSpace,
			&o.LedgerType, &o.IsDroppedLedgerTable, &o.LedgerViewSchema, &o.LedgerViewName, &ledgerCols,
			&o.FileTableDirectory, &o.FileTableCollation, &o.FileTableNamespaceEnabled, &ftObjects,
			&e.DataSource, &e.FileFormat, &e.Location, &e.RejectType, &e.RejectValue, &e.RejectSampleValue,
			&e.RemoteSchema, &e.RemoteObject, &e.Distribution, &e.ShardingColumn); err != nil {
			return err
		}
		parts, err := decodePartitionCompression(heap)
		if err != nil {
			return err
		}
		o.HeapPartitionCompression = parts
		if len(parts) > 0 {
			o.HeapCompression = string(parts[0])
		}
		// Read last-first, so the four come back in view order.
		if o.LedgerViewColumns, err = decodeJSONList(ledgerCols); err != nil {
			return err
		}
		slices.Reverse(o.LedgerViewColumns)
		o.FileTableSystemObjects, err = decodeJSONList(ftObjects)
		return err
	}, t.scriptOptionsSelect(), t.ObjectID); err != nil && !errors.Is(err, sql.ErrNoRows) {
		// No row is a table dropped since it was looked up; the column and
		// index reads beside this one answer that with empty lists, and so
		// does this.
		return o, fmt.Errorf("gosmo: script options for %s: %w", t.FullName(), err)
	}
	return o, nil
}

// scriptOptionsSelect is scriptOptions' query, version-gated.
//
// The ledger columns are SQL Server 2022's, with ledger tables; every table
// on an older instance is a non-ledger one. A ledger view's four ledger
// columns follow the table's own, so they are its last four by column_id,
// read as a jsonList-shaped column.
// sys.filetables and sys.external_tables are older than gosmo's 2016 floor.
// https://learn.microsoft.com/sql/relational-databases/system-catalog-views/sys-tables-transact-sql
func (t *Table) scriptOptionsSelect() string {
	major := t.db.serverMajorVersion()
	return `
SELECT CAST(CASE WHEN t.temporal_type = 2 THEN 1 ELSE 0 END AS BIT),
       ISNULL(OBJECT_SCHEMA_NAME(t.history_table_id), ''),
       ISNULL(OBJECT_NAME(t.history_table_id), ''),
       ISNULL(COL_NAME(p.object_id, p.start_column_id), ''),
       ISNULL(COL_NAME(p.object_id, p.end_column_id), ''),
       ` + partitionCompressionList("t.object_id", "0") + `,
       ISNULL(CONVERT(sysname, DATABASEPROPERTYEX(DB_NAME(), 'Collation')), ''),
       ISNULL(t.durability_desc, ''),
       ISNULL(lds.name, ''), CAST(CASE WHEN lds.type = 'FG' THEN 1 ELSE 0 END AS bit),
       ISNULL(fds.name, ''),
       ` + colSince(major, SQLServer2022, "t.ledger_type", "CAST(0 AS tinyint)") + `,
       ` + colSince(major, SQLServer2022, "t.is_dropped_ledger_table", "CAST(0 AS bit)") + `,
       ` + colSince(major, SQLServer2022, "ISNULL(OBJECT_SCHEMA_NAME(t.ledger_view_id), '')", "CAST('' AS sysname)") + `,
       ` + colSince(major, SQLServer2022, "ISNULL(OBJECT_NAME(t.ledger_view_id), '')", "CAST('' AS sysname)") + `,
       ` + colSince(major, SQLServer2022, "(SELECT TOP (4) lv.name AS v FROM sys.columns lv WHERE lv.object_id = t.ledger_view_id ORDER BY lv.column_id DESC FOR JSON PATH)",
		"CAST(NULL AS nvarchar(max))") + `,
       ISNULL(ft.directory_name, ''), ISNULL(ft.filename_collation_name, ''), ISNULL(ft.is_enabled, 0),
       ` + jsonList("OBJECT_NAME(so.object_id)",
		"\n        FROM sys.filetable_system_defined_objects so WHERE so.parent_object_id = t.object_id", "") + `,
       ISNULL(eds.name, ''), ISNULL(eff.name, ''), ISNULL(et.location, ''),
       ISNULL(et.reject_type, ''), et.reject_value, et.reject_sample_value,
       ISNULL(et.remote_schema_name, ''), ISNULL(et.remote_object_name, ''),
       ISNULL(et.distribution_desc, ''), ISNULL(COL_NAME(et.object_id, et.sharding_col_id), '')
FROM   sys.tables AS t
LEFT   JOIN sys.periods p ON p.object_id = t.object_id
LEFT   JOIN sys.data_spaces lds ON lds.data_space_id = NULLIF(t.lob_data_space_id, 0)
LEFT   JOIN sys.data_spaces fds ON fds.data_space_id = t.filestream_data_space_id
LEFT   JOIN sys.filetables ft ON ft.object_id = t.object_id
LEFT   JOIN sys.external_tables et ON et.object_id = t.object_id
LEFT   JOIN sys.external_data_sources eds ON eds.data_source_id = et.data_source_id
LEFT   JOIN sys.external_file_formats eff ON eff.file_format_id = et.file_format_id
WHERE  t.object_id = @p1`
}

// buildTableScript assembles the CREATE (or DROP) TABLE script from metadata
// already read. Split out of ScriptTable so the assembly — where every
// bug this has had has lived — can be unit-tested without a server; the
// method above is then only the catalog reads that feed it.
func buildTableScript(schema, name, dbName string, p tableScriptParts, opts ScriptOptions) string {
	fullName := qualifiedName(schema, name)

	// DROP TABLE refuses a system-versioned table; versioning has to be
	// switched off first. The history table is left in place, so a DROP
	// And CREATE reattaches it by name and keeps the history.
	var drop strings.Builder
	if p.table.SystemVersioned {
		if opts.IncludeIfNotExists {
			fmt.Fprintf(&drop, "IF OBJECT_ID(N'%s', N'U') IS NOT NULL\n    ",
				escapeSingle(fullName))
		}
		fmt.Fprintf(&drop, "ALTER TABLE %s SET (SYSTEM_VERSIONING = OFF);\nGO\n", fullName)
	}
	if opts.IncludeIfNotExists {
		fmt.Fprintf(&drop, "IF OBJECT_ID(N'%s', N'U') IS NOT NULL\n    ",
			escapeSingle(fullName))
	}
	fmt.Fprintf(&drop, "DROP TABLE %s;\nGO\n", fullName)

	// The CREATE TABLE's guard follows the header and SET lines, so it is
	// written below rather than passed as the envelope's.
	return opts.envelope(drop.String(), "", func(sb *strings.Builder) {
		if opts.IncludeHeaders {
			fmt.Fprintf(sb, "/* Table: %s  Database: %s */\n", fullName, dbName)
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
			fmt.Fprintf(sb, "IF OBJECT_ID(N'%s', N'U') IS NULL\n", escapeSingle(fullName))
		}
		fmt.Fprintf(sb, "CREATE TABLE %s (\n", fullName)

		indexes := graphScriptIndexes(p.indexes, p.cols, p.table)
		inline := inlineIndexes(indexes, p.cols, p.table)

		var pkIdx *Index
		for _, idx := range indexes {
			if idx.IsPrimaryKey {
				pkIdx = idx
				break
			}
		}

		elems := make([]string, 0, len(p.cols)+2)
		for _, col := range p.cols {
			// A graph table's internal columns are AS NODE / AS EDGE's to
			// create, and a column dropped from a ledger table is the ledger's
			// record of it, not a column the table has.
			if col.GraphType != 0 || col.IsDroppedLedgerColumn {
				continue
			}
			elems = append(elems, tableColumnDefinition(col, p.table.DatabaseCollation))
		}
		if p.table.PeriodStart != "" && p.table.PeriodEnd != "" {
			elems = append(elems, fmt.Sprintf("PERIOD FOR SYSTEM_TIME (%s, %s)",
				quoteIdent(p.table.PeriodStart), quoteIdent(p.table.PeriodEnd)))
		}
		switch {
		case pkIdx != nil && p.table.IsMemoryOptimized:
			elems = append(elems, fmt.Sprintf("CONSTRAINT %s PRIMARY KEY %s",
				quoteIdent(pkIdx.Name), memoryOptimizedIndexSpec(pkIdx)))
		case pkIdx != nil:
			clust := "NONCLUSTERED"
			if pkIdx.IsClustered {
				clust = "CLUSTERED"
			}
			elems = append(elems, fmt.Sprintf("CONSTRAINT %s PRIMARY KEY %s (%s)%s%s",
				quoteIdent(pkIdx.Name), clust, indexColumnList(pkIdx.KeyColumns),
				indexWithClause(pkIdx, " "), dataSpaceClause(pkIdx.DataSpace)))
		}
		for _, idx := range indexes {
			if inline[idx.Name] {
				elems = append(elems, inlineIndexDefinition(idx, p.table))
			}
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
		if p.table.IsMemoryOptimized {
			tableOpts = append(tableOpts, "MEMORY_OPTIMIZED = ON")
			if p.table.Durability != "" {
				tableOpts = append(tableOpts, "DURABILITY = "+p.table.Durability)
			}
		} else {
			tableOpts = append(tableOpts, compressionOptions(
				DataCompression(p.table.HeapCompression), p.table.HeapPartitionCompression, rowstoreCompressed)...)
		}
		if p.table.SystemVersioned {
			if p.table.HistoryTable != "" {
				tableOpts = append(tableOpts, fmt.Sprintf("SYSTEM_VERSIONING = ON (HISTORY_TABLE = %s)",
					qualifiedName(p.table.HistorySchema, p.table.HistoryTable)))
			} else {
				tableOpts = append(tableOpts, "SYSTEM_VERSIONING = ON")
			}
		}
		tableOpts = append(tableOpts, ledgerOptions(p.table)...)
		with := ""
		if len(tableOpts) > 0 {
			with = "\nWITH (" + strings.Join(tableOpts, ", ") + ")"
		}
		// AS NODE / AS EDGE precedes the ON clause; the server refuses it after.
		fmt.Fprintf(sb, ")%s%s%s%s;\nGO\n\n", graphTableClause(p.table),
			dataSpaceClause(p.ds), tableStorageClauses(p), with)

		writeTableDependents(sb, indexes, inline, p, fullName, opts)
	})
}

// writeTableDependents writes the statements that follow a CREATE TABLE:
// the indexes and unique constraints not declared inside it (inline, by
// name, and the primary key), the foreign keys, check constraints and edge
// constraints, and last the disabling of any disabled index.
func writeTableDependents(sb *strings.Builder, indexes []*Index, inline map[string]bool,
	p tableScriptParts, fullName string, opts ScriptOptions) {
	// Non-PK indexes. A unique *constraint* is backed by an index in
	// sys.indexes but is not created with CREATE INDEX — it belongs to the
	// table, and scripting it as an index leaves the constraint missing.
	//
	// A secondary XML index is built over its primary, which must exist
	// first, so the secondaries go last whatever their index IDs say.
	var ordered, secondaryXML []*Index
	for _, idx := range indexes {
		if idx.PrimaryXMLIndex != "" {
			secondaryXML = append(secondaryXML, idx)
		} else {
			ordered = append(ordered, idx)
		}
	}
	ordered = append(ordered, secondaryXML...)
	for _, idx := range ordered {
		if idx.IsPrimaryKey || inline[idx.Name] {
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
	for _, ec := range p.ecs {
		sb.WriteString(scriptEdgeConstraint(ec, fullName, opts))
	}

	// Disabled indexes are disabled last. Disabling a clustered index takes
	// the whole table offline, so doing it where the index is created would
	// fail every CREATE INDEX and ADD CONSTRAINT after it.
	for _, idx := range indexes {
		sb.WriteString(indexDisableStatement(idx, fullName))
	}
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
	if col.IsFileStream {
		sb.WriteString(" FILESTREAM")
	}
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
		fmt.Fprintf(&sb, " IDENTITY(%s,%s)", col.IdentitySeed, col.IdentityIncrement)
		if col.IdentityNotForReplication {
			sb.WriteString(" NOT FOR REPLICATION")
		}
	}
	if g := generatedAlwaysClauses[col.GeneratedAlwaysType]; g != "" {
		sb.WriteString(" GENERATED ALWAYS AS " + g)
		// HIDDEN is part of the GENERATED ALWAYS clause and does not parse
		// without it.
		if col.IsHidden {
			sb.WriteString(" HIDDEN")
		}
	}
	if col.IsRowGUID {
		sb.WriteString(" ROWGUIDCOL")
	}
	if col.ColumnEncryptionKey != "" {
		fmt.Fprintf(&sb, " ENCRYPTED WITH (COLUMN_ENCRYPTION_KEY = %s, ENCRYPTION_TYPE = %s, ALGORITHM = '%s')",
			quoteIdent(col.ColumnEncryptionKey), col.EncryptionType, escapeSingle(col.EncryptionAlgorithm))
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

// generatedAlwaysClauses is what follows GENERATED ALWAYS AS for each
// sys.columns.generated_always_type: a temporal table's period columns and
// a ledger table's transaction-id and sequence-number columns.
var generatedAlwaysClauses = map[int]string{
	1:  "ROW START",
	2:  "ROW END",
	7:  "TRANSACTION_ID START",
	8:  "TRANSACTION_ID END",
	9:  "SEQUENCE_NUMBER START",
	10: "SEQUENCE_NUMBER END",
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

// scriptForeignKey renders the ALTER TABLE ... ADD CONSTRAINT that creates fk.
//
// Trust, disabled state and NOT FOR REPLICATION are carried over exactly as
// scriptCheckConstraint carries them, for the same reason: adding an
// untrusted key WITH CHECK fails (Msg 547) on the very rows it was left
// untrusted for, and a key recreated enabled starts refusing writes the
// original let through.
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
	with := "WITH CHECK"
	if fk.IsDisabled || fk.IsNotTrusted {
		with = "WITH NOCHECK"
	}
	fmt.Fprintf(&sb,
		"ALTER TABLE %s %s\n    ADD CONSTRAINT %s\n    FOREIGN KEY (%s)\n    REFERENCES %s (%s)",
		tableName, with, quoteIdent(fk.Name),
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
	if fk.IsNotForReplication {
		sb.WriteString("\n    NOT FOR REPLICATION")
	}
	sb.WriteString(";\nGO\n")
	if fk.IsDisabled {
		fmt.Fprintf(&sb, "ALTER TABLE %s NOCHECK CONSTRAINT %s;\nGO\n", tableName, quoteIdent(fk.Name))
	}
	sb.WriteString("\n")
	return sb.String()
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
	return catalogTypeString(col.DataType, col.TypeSchema, col.IsUserDefinedType, col.MaxLength, col.Precision, col.Scale)
}

// catalogTypeString is ColumnTypeString's rendering, shared with
// Parameter.TypeString: a user-defined type qualified and bare, any other
// type through sqlTypeString.
func catalogTypeString(dt DataType, typeSchema string, userDefined bool, maxLength, precision, scale int) string {
	if userDefined {
		return qualifiedName(typeSchema, string(dt))
	}
	return sqlTypeString(dt, maxLength, precision, scale)
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
