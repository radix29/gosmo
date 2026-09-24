package gosmo

import (
	"fmt"
	"strings"
)

// The table kinds ScriptTable handles beyond a plain disk-based table —
// graph node and edge tables, memory-optimized tables, FILESTREAM and
// TEXTIMAGE_ON storage — and the kinds it refuses.

// refusal returns the ErrUnsupported error for a table the scripter cannot
// express, or nil. Each refused kind would otherwise script as a plain table
// with the same name: an external table's rows are elsewhere, a FileTable's
// fixed schema is not its own to declare, a ledger table would lose its
// ledger, and an Always Encrypted column would be recreated in plaintext.
func (o tableScriptOptions) refusal(fullName string) error {
	var kind string
	switch {
	case o.IsExternal:
		kind = "an external table"
	case o.IsFileTable:
		kind = "a FileTable"
	case o.LedgerType != 0:
		kind = "a ledger table"
	case o.HasEncryptedColumns:
		kind = "a table with Always Encrypted columns"
	default:
		return nil
	}
	return unsupportedf("gosmo: script table %s: %s cannot be scripted", fullName, kind)
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
// `NONCLUSTERED (…)`, or `CLUSTERED COLUMNSTORE`.
//
// None of a disk index's options apply. The catalog reports a
// memory-optimized index with row and page locks off, and passing that
// through indexWithClause would emit ALLOW_ROW_LOCKS = OFF, which the server
// refuses on such a table. A hash index takes no sort direction.
func memoryOptimizedIndexSpec(idx *Index) string {
	switch idx.Type {
	case IndexTypeClusteredColumnStore:
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
