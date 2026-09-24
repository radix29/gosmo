package gosmo

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// The table kinds ScriptTable handles beyond a plain disk-based table —
// graph node and edge tables, memory-optimized tables, FILESTREAM and
// TEXTIMAGE_ON storage, ledger tables, Always Encrypted columns, FileTables
// and external tables — and the kinds it refuses.

// refusal returns the ErrUnsupported error for a table the scripter cannot
// express, or nil. Each refused kind is one no CREATE TABLE can make: a
// ledger history table is created by its ledger table's CREATE and by
// nothing else, and a dropped ledger table is the ledger's record of a
// table that no longer exists. Scripting either as a plain table would
// create something different under the same name.
func (o tableScriptOptions) refusal(fullName string) error {
	var kind string
	switch {
	case o.IsDroppedLedgerTable:
		kind = "a dropped ledger table"
	case o.LedgerType == 1:
		kind = "a ledger history table, which only its ledger table's CREATE TABLE creates,"
	default:
		return nil
	}
	return unsupportedf("gosmo: script table %s: %s cannot be scripted", fullName, kind)
}

// ledgerOptions returns a ledger table's entries for CREATE TABLE's WITH
// clause, or nil for a table that is not one. An updatable ledger table is
// system-versioned into its history table — sys.tables reports it with
// temporal_type 0 all the same, so SystemVersioned does not cover it — and
// an append-only one has no history. The ledger view is named with its
// four columns, since each can be renamed from its ledger_… default.
func ledgerOptions(o tableScriptOptions) []string {
	var opts, ledger []string
	switch o.LedgerType {
	case 2:
		if o.HistoryTable != "" {
			opts = append(opts, fmt.Sprintf("SYSTEM_VERSIONING = ON (HISTORY_TABLE = %s)",
				qualifiedName(o.HistorySchema, o.HistoryTable)))
		} else {
			opts = append(opts, "SYSTEM_VERSIONING = ON")
		}
	case 3:
	default:
		return nil
	}
	if o.LedgerViewName != "" {
		view := "LEDGER_VIEW = " + qualifiedName(o.LedgerViewSchema, o.LedgerViewName)
		if c := o.LedgerViewColumns; len(c) == 4 {
			view += fmt.Sprintf(" (TRANSACTION_ID_COLUMN_NAME = %s, SEQUENCE_NUMBER_COLUMN_NAME = %s, "+
				"OPERATION_TYPE_COLUMN_NAME = %s, OPERATION_TYPE_DESC_COLUMN_NAME = %s)",
				quoteIdent(c[0]), quoteIdent(c[1]), quoteIdent(c[2]), quoteIdent(c[3]))
		}
		ledger = append(ledger, view)
	}
	if o.LedgerType == 3 {
		ledger = append(ledger, "APPEND_ONLY = ON")
	}
	if len(ledger) == 0 {
		return append(opts, "LEDGER = ON")
	}
	return append(opts, "LEDGER = ON ("+strings.Join(ledger, ", ")+")")
}

// graphTableClause is the AS NODE / AS EDGE that follows a graph table's
// column list, or "".
func graphTableClause(o tableScriptOptions) string {
	switch {
	case o.IsNode:
		return " AS NODE"
	case o.IsEdge:
		return " AS EDGE"
	}
	return ""
}

// graphPseudoColumn is the pseudo-column a graph table's internal column is
// addressed by in DDL — $node_id, $edge_id, $from_id or $to_id — or "" for
// an ordinary column. An index on $from_id is stored on two internal columns
// (from_obj_id, from_id), so both map to the one pseudo-column.
func graphPseudoColumn(graphType int, isNode bool) string {
	switch graphType {
	case GraphColumnID, GraphColumnIDComputed:
		if isNode {
			return "$node_id"
		}
		return "$edge_id"
	case GraphColumnFromID, GraphColumnFromObjID, GraphColumnFromIDComputed:
		return "$from_id"
	case GraphColumnToID, GraphColumnToObjID, GraphColumnToIDComputed:
		return "$to_id"
	}
	return ""
}

// graphScriptIndexes returns the indexes of a graph table as CREATE INDEX
// has to name them: each internal column replaced by its pseudo-column, and
// the GRAPH_UNIQUE_INDEX_… that AS NODE / AS EDGE creates by itself left
// out. The internal names (graph_id_…, from_id_…) carry a per-table GUID and
// cannot be written, and recreating the automatic index would duplicate it.
// indexes is returned unchanged for a table that is not a graph table.
func graphScriptIndexes(indexes []*Index, cols []*Column, o tableScriptOptions) []*Index {
	if !o.IsNode && !o.IsEdge {
		return indexes
	}
	graphType := make(map[string]int, len(cols))
	for _, c := range cols {
		if c.GraphType != 0 {
			graphType[c.Name] = c.GraphType
		}
	}
	mapCols := func(in []IndexColumn) []IndexColumn {
		var out []IndexColumn
		for _, c := range in {
			if ps := graphPseudoColumn(graphType[c.Name], o.IsNode); ps != "" {
				if n := len(out); n > 0 && out[n-1].Name == ps {
					continue // the second internal column of the same pseudo-column
				}
				c.Name, c.pseudo = ps, true
			}
			out = append(out, c)
		}
		return out
	}
	var out []*Index
	for _, idx := range indexes {
		if strings.HasPrefix(idx.Name, "GRAPH_UNIQUE_INDEX_") && len(idx.KeyColumns) == 1 &&
			graphType[idx.KeyColumns[0].Name] == GraphColumnID {
			continue
		}
		c := *idx
		c.KeyColumns = mapCols(idx.KeyColumns)
		c.IncludedColumns = mapCols(idx.IncludedColumns)
		out = append(out, &c)
	}
	return out
}

// inlineIndexes returns which of indexes must be declared inside the CREATE
// TABLE rather than created after it, by name:
//
//   - every index of a memory-optimized table, which cannot be given one by
//     CREATE INDEX at all;
//   - the unique constraints of a table with a FILESTREAM column. CREATE
//     TABLE refuses a FILESTREAM column unless the table already has a
//     ROWGUIDCOL column under a UNIQUE or PRIMARY KEY constraint (Msg 5505;
//     a unique index does not count), so adding the constraint after is too
//     late.
//
// The primary key is always inline and is not in the result.
func inlineIndexes(indexes []*Index, cols []*Column, o tableScriptOptions) map[string]bool {
	inline := map[string]bool{}
	fileStream := hasFileStreamColumn(cols)
	for _, idx := range indexes {
		switch {
		case idx.IsPrimaryKey:
		case o.IsMemoryOptimized, fileStream && idx.IsUniqueConstraint:
			inline[idx.Name] = true
		}
	}
	return inline
}

// inlineIndexDefinition renders an index declared inside CREATE TABLE: a
// unique constraint as `CONSTRAINT … UNIQUE`, anything else as `INDEX …`.
func inlineIndexDefinition(idx *Index, o tableScriptOptions) string {
	head := "INDEX " + quoteIdent(idx.Name) + " "
	if idx.IsUniqueConstraint {
		head = "CONSTRAINT " + quoteIdent(idx.Name) + " UNIQUE "
	} else if idx.IsUnique && idx.Type != IndexTypeClusteredColumnStore {
		head += "UNIQUE "
	}
	if o.IsMemoryOptimized {
		return head + memoryOptimizedIndexSpec(idx)
	}
	clust := "NONCLUSTERED"
	if idx.IsClustered {
		clust = "CLUSTERED"
	}
	return fmt.Sprintf("%s%s (%s)%s%s", head, clust, indexColumnList(idx.KeyColumns),
		indexWithClause(idx, " "), dataSpaceClause(idx.DataSpace))
}

// memoryOptimizedIndexSpec renders a memory-optimized table's index from its
// kind on: `NONCLUSTERED HASH (…) WITH (BUCKET_COUNT = n)`, a range index's
// `NONCLUSTERED (…)`, or `CLUSTERED COLUMNSTORE` with its COMPRESSION_DELAY.
//
// None of a disk index's options apply. The catalog reports a
// memory-optimized index with row and page locks off, and passing that
// through indexWithClause would emit ALLOW_ROW_LOCKS = OFF, which the server
// refuses on such a table. A hash index takes no sort direction.
func memoryOptimizedIndexSpec(idx *Index) string {
	switch idx.Type {
	case IndexTypeClusteredColumnStore:
		if idx.CompressionDelay > 0 {
			return fmt.Sprintf("CLUSTERED COLUMNSTORE WITH (COMPRESSION_DELAY = %d MINUTES)", idx.CompressionDelay)
		}
		return "CLUSTERED COLUMNSTORE"
	case IndexTypeNonClusteredHash:
		names := make([]string, len(idx.KeyColumns))
		for i, c := range idx.KeyColumns {
			names[i] = c.ref()
		}
		return fmt.Sprintf("NONCLUSTERED HASH (%s) WITH (BUCKET_COUNT = %d)",
			strings.Join(names, ", "), idx.BucketCount)
	}
	return "NONCLUSTERED (" + indexColumnList(idx.KeyColumns) + ")"
}

// tableStorageClauses renders CREATE TABLE's TEXTIMAGE_ON and FILESTREAM_ON,
// each only where it says something the ON clause does not.
//
// TEXTIMAGE_ON is emitted when the LOB data lives on a filegroup other than
// the table's own, which is where it goes when the clause is left out. The
// catalog keeps lob_data_space_id after the last LOB column is dropped, and
// the server refuses TEXTIMAGE_ON on a table with none (Msg 1709), so a LOB
// column is required too. A partitioned table's LOB data follows the scheme
// and takes no clause.
//
// FILESTREAM_ON is always named when the table has a FILESTREAM column:
// left out, it means the database's default FILESTREAM filegroup, which may
// not be the source's — and a partitioned table has no default at all.
func tableStorageClauses(p tableScriptParts) string {
	var sb strings.Builder
	o := p.table
	if o.LobDataSpace != "" && o.LobIsFileGroup && p.ds.Name != "" && !p.ds.IsPartitionScheme &&
		!strings.EqualFold(o.LobDataSpace, p.ds.Name) && hasLOBColumn(p.cols) {
		fmt.Fprintf(&sb, " TEXTIMAGE_ON %s", quoteIdent(o.LobDataSpace))
	}
	if o.FileStreamDataSpace != "" && hasFileStreamColumn(p.cols) {
		fmt.Fprintf(&sb, " FILESTREAM_ON %s", quoteIdent(o.FileStreamDataSpace))
	}
	return sb.String()
}

// hasLOBColumn reports whether any column stores its data in the table's
// LOB data space: a (max) type other than FILESTREAM, xml, a large CLR type,
// or one of the legacy text types.
func hasLOBColumn(cols []*Column) bool {
	for _, c := range cols {
		if c.IsComputed && !c.IsPersisted {
			continue
		}
		switch {
		case c.MaxLength == -1 && !c.IsFileStream,
			c.DataType == DataTypeText, c.DataType == DataTypeNText, c.DataType == DataTypeImage:
			return true
		}
	}
	return false
}

func hasFileStreamColumn(cols []*Column) bool {
	for _, c := range cols {
		if c.IsFileStream {
			return true
		}
	}
	return false
}

// scriptEdgeConstraint renders the ALTER TABLE … ADD CONSTRAINT … CONNECTION
// that creates ec, carrying its trust and disabled state the way
// scriptForeignKey does, and for the same reason.
func scriptEdgeConstraint(ec *EdgeConstraint, tableName string, opts ScriptOptions) string {
	var sb strings.Builder
	if opts.IncludeIfNotExists {
		sb.WriteString(constraintExistenceGuard(ec.Name, tableName))
	}
	clauses := make([]string, len(ec.Clauses))
	for i, c := range ec.Clauses {
		clauses[i] = qualifiedName(c.FromSchema, c.FromTable) + " TO " + qualifiedName(c.ToSchema, c.ToTable)
	}
	with := "WITH CHECK"
	if ec.IsDisabled || ec.IsNotTrusted {
		with = "WITH NOCHECK"
	}
	fmt.Fprintf(&sb, "ALTER TABLE %s %s\n    ADD CONSTRAINT %s\n    CONNECTION (%s)",
		tableName, with, quoteIdent(ec.Name), strings.Join(clauses, ", "))
	if ec.DeleteAction != "" && ec.DeleteAction != "NO_ACTION" {
		fmt.Fprintf(&sb, "\n    ON DELETE %s", strings.ReplaceAll(ec.DeleteAction, "_", " "))
	}
	sb.WriteString(";\nGO\n")
	if ec.IsDisabled {
		fmt.Fprintf(&sb, "ALTER TABLE %s NOCHECK CONSTRAINT %s;\nGO\n", tableName, quoteIdent(ec.Name))
	}
	sb.WriteString("\n")
	return sb.String()
}

// ============================================================
// FileTables
// ============================================================

// buildFileTableScript assembles a FileTable's script. Its columns are
// fixed by AS FILETABLE and are not the table's to declare, and neither are
// the constraints AS FILETABLE adds with them — the primary key, the two
// unique constraints, and the defaults, checks and self-referencing foreign
// key sys.filetable_system_defined_objects lists. The primary key and
// unique constraints take their names from the WITH clause, so a DROP AND
// CREATE keeps them; the rest are left to AS FILETABLE, whose names are
// generated. Only what was added to the table afterwards is scripted after
// it.
func buildFileTableScript(schema, name, dbName string, p tableScriptParts, opts ScriptOptions) string {
	fullName := qualifiedName(schema, name)
	var drop strings.Builder
	if opts.IncludeIfNotExists {
		fmt.Fprintf(&drop, "IF OBJECT_ID(N'%s', N'U') IS NOT NULL\n    ", escapeSingle(fullName))
	}
	fmt.Fprintf(&drop, "DROP TABLE %s;\nGO\n", fullName)

	system := make(map[string]bool, len(p.table.FileTableSystemObjects))
	for _, n := range p.table.FileTableSystemObjects {
		system[n] = true
	}
	with := newClauseList()
	with.addLiteral("FILETABLE_DIRECTORY", p.table.FileTableDirectory)
	if p.table.FileTableCollation != "" {
		with.addKeyword("FILETABLE_COLLATE_FILENAME", p.table.FileTableCollation)
	}
	var user []*Index
	for _, idx := range p.indexes {
		if !system[idx.Name] {
			user = append(user, idx)
			continue
		}
		switch {
		case idx.IsPrimaryKey:
			with.addKeyword("FILETABLE_PRIMARY_KEY_CONSTRAINT_NAME", quoteIdent(idx.Name))
		case idx.IsUniqueConstraint && len(idx.KeyColumns) == 1 && idx.KeyColumns[0].Name == "stream_id":
			with.addKeyword("FILETABLE_STREAMID_UNIQUE_CONSTRAINT_NAME", quoteIdent(idx.Name))
		case idx.IsUniqueConstraint:
			with.addKeyword("FILETABLE_FULLPATH_UNIQUE_CONSTRAINT_NAME", quoteIdent(idx.Name))
		}
	}
	rest := p
	rest.fks, rest.checks = nil, nil
	for _, fk := range p.fks {
		if !system[fk.Name] {
			rest.fks = append(rest.fks, fk)
		}
	}
	for _, ck := range p.checks {
		if !system[ck.Name] {
			rest.checks = append(rest.checks, ck)
		}
	}

	return opts.envelope(drop.String(), "", func(sb *strings.Builder) {
		if opts.IncludeHeaders {
			fmt.Fprintf(sb, "/* FileTable: %s  Database: %s */\n", fullName, dbName)
		}
		if opts.IncludeIfNotExists {
			fmt.Fprintf(sb, "IF OBJECT_ID(N'%s', N'U') IS NULL\n", escapeSingle(fullName))
		}
		fmt.Fprintf(sb, "CREATE TABLE %s AS FILETABLE%s", fullName, dataSpaceClause(p.ds))
		if p.table.FileStreamDataSpace != "" {
			fmt.Fprintf(sb, " FILESTREAM_ON %s", quoteIdent(p.table.FileStreamDataSpace))
		}
		if !with.empty() {
			sb.WriteString("\nWITH (\n" + with.render("    ") + ")")
		}
		sb.WriteString(";\nGO\n\n")
		writeTableDependents(sb, user, nil, rest, fullName, opts)
		if !p.table.FileTableNamespaceEnabled {
			fmt.Fprintf(sb, "ALTER TABLE %s DISABLE FILETABLE_NAMESPACE;\nGO\n\n", fullName)
		}
	})
}

// ============================================================
// External tables
// ============================================================

// scriptExternalTable reads the data source and file format an external
// table reads through, and renders the table with them.
func (sc *Scripter) scriptExternalTable(ctx context.Context, schema, name string, p tableScriptParts) (string, error) {
	var deps []string
	// Each is always guarded and never dropped, whatever the options say:
	// both are shared by every external table over the same source, and
	// already exist wherever the table itself is being recreated.
	depOpts := ScriptOptions{Verb: ScriptCreate, IncludeIfNotExists: true}
	if n := p.table.External.DataSource; n != "" {
		ds, err := sc.db.ExternalDataSourceByName(ctx, n)
		if err != nil {
			return "", err
		}
		deps = append(deps, buildExternalDataSourceScript(ds, depOpts))
	}
	if n := p.table.External.FileFormat; n != "" {
		ff, err := sc.db.ExternalFileFormatByName(ctx, n)
		if err != nil {
			return "", err
		}
		deps = append(deps, buildExternalFileFormatScript(ff, depOpts))
	}
	return buildExternalTableScript(schema, name, sc.db.Name, p, deps, sc.opts), nil
}

// buildExternalTableScript assembles an external table's script: deps (its
// data source and file format, already rendered) ahead of a CREATE
// EXTERNAL TABLE, whose columns carry only a type, a collation and
// nullability.
//
// The existence checks name no object type: an external table is 'U' in
// sys.objects on some instances and 'ET' on others.
func buildExternalTableScript(schema, name, dbName string, p tableScriptParts, deps []string, opts ScriptOptions) string {
	fullName := qualifiedName(schema, name)
	var drop strings.Builder
	if opts.IncludeIfNotExists {
		fmt.Fprintf(&drop, "IF OBJECT_ID(N'%s') IS NOT NULL\n    ", escapeSingle(fullName))
	}
	fmt.Fprintf(&drop, "DROP EXTERNAL TABLE %s;\nGO\n", fullName)

	e := p.table.External
	with := newClauseList()
	with.addLiteral("LOCATION", e.Location)
	if e.DataSource != "" {
		with.addKeyword("DATA_SOURCE", quoteIdent(e.DataSource))
	}
	if e.FileFormat != "" {
		with.addKeyword("FILE_FORMAT", quoteIdent(e.FileFormat))
		// The reject options belong to a file-format table; the catalog
		// reports the defaults, VALUE and 0, for one created without them.
		if e.RejectType != "" && e.RejectValue.Valid {
			with.addKeyword("REJECT_TYPE", e.RejectType)
			with.addKeyword("REJECT_VALUE", strconv.FormatFloat(e.RejectValue.Float64, 'f', -1, 64))
			if e.RejectType == "PERCENTAGE" && e.RejectSampleValue.Valid {
				with.addKeyword("REJECT_SAMPLE_VALUE", strconv.FormatFloat(e.RejectSampleValue.Float64, 'f', -1, 64))
			}
		}
	}
	with.addLiteral("SCHEMA_NAME", e.RemoteSchema)
	with.addLiteral("OBJECT_NAME", e.RemoteObject)
	switch e.Distribution {
	case "SHARDED":
		if e.ShardingColumn != "" {
			with.addKeyword("DISTRIBUTION", "SHARDED("+quoteIdent(e.ShardingColumn)+")")
		}
	case "REPLICATED", "ROUND_ROBIN":
		with.addKeyword("DISTRIBUTION", e.Distribution)
	}

	return opts.envelope(drop.String(), "", func(sb *strings.Builder) {
		if opts.IncludeHeaders {
			fmt.Fprintf(sb, "/* External table: %s  Database: %s */\n", fullName, dbName)
		}
		for _, d := range deps {
			sb.WriteString(d)
			sb.WriteString("\n")
		}
		if opts.IncludeIfNotExists {
			fmt.Fprintf(sb, "IF OBJECT_ID(N'%s') IS NULL\n", escapeSingle(fullName))
		}
		fmt.Fprintf(sb, "CREATE EXTERNAL TABLE %s (\n", fullName)
		for i, col := range p.cols {
			sb.WriteString("    " + tableColumnDefinition(col, p.table.DatabaseCollation))
			if i < len(p.cols)-1 {
				sb.WriteString(",")
			}
			sb.WriteString("\n")
		}
		sb.WriteString(")\nWITH (\n" + with.render("    ") + ");\nGO\n")
	})
}
