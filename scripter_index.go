package gosmo

import (
	"errors"
	"fmt"
	"slices"
	"strings"
)

// ============================================================
// Scripter — index statements and the data-space clauses tables share
// ============================================================

// indexColumnList renders an index's key columns with their sort direction.
func indexColumnList(cols []IndexColumn) string {
	out := make([]string, len(cols))
	for i, kc := range cols {
		dir := "ASC"
		if kc.Descending {
			dir = "DESC"
		}
		out[i] = fmt.Sprintf("%s %s", kc.ref(), dir)
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
		// sys.index_columns marks every column of a nonclustered columnstore
		// index is_included_column = 1, so a read index carries them all in
		// IncludedColumns and none in KeyColumns — scripting KeyColumns alone
		// emitted "ON [t] ()", which does not parse.
		var cols []string
		for _, c := range slices.Concat(idx.KeyColumns, idx.IncludedColumns) {
			cols = append(cols, c.ref())
		}
		if opts.IncludeIfNotExists {
			sb.WriteString(indexExistenceGuard(idx.Name, tableName))
		}
		fmt.Fprintf(&sb, "CREATE NONCLUSTERED COLUMNSTORE INDEX %s\n    ON %s (%s)",
			quoteIdent(idx.Name), tableName, strings.Join(cols, ", "))
		// A filtered NCCI (2016+) recreated without its WHERE covers every
		// row instead — a different, larger index under the same name.
		if idx.FilterDefinition != "" {
			fmt.Fprintf(&sb, "\n    WHERE %s", idx.FilterDefinition)
		}
		fmt.Fprintf(&sb, "%s%s;\nGO\n\n", columnstoreWithClause(idx), dataSpaceClause(idx.DataSpace))
		return sb.String()
	case idx.Type == IndexTypeXML || idx.Type == IndexTypeSpatial:
		fmt.Fprintf(&sb, "-- %s index %s on %s is not scripted (its DDL has no generic form here).\n\n",
			idx.Type, quoteIdent(idx.Name), tableName)
		return sb.String()
	}

	if opts.IncludeIfNotExists {
		sb.WriteString(indexExistenceGuard(idx.Name, tableName))
	}
	sb.WriteString(rowstoreIndexCreate(idx, tableName, indexWithClause(idx, "\n    "), dataSpaceClause(idx.DataSpace)))
	sb.WriteString(";\nGO\n\n")
	return sb.String()
}

// rowstoreIndexCreate renders a rowstore index's CREATE INDEX up to the
// statement terminator: everything idx says, with the WITH and ON clauses
// supplied by the caller. It is the one builder behind both Script as CREATE
// and Index.SetIncludedColumns, so the two cannot drift — the latter, built
// by hand, once recreated an index without its fill factor, pad, lock,
// NORECOMPUTE and IGNORE_DUP_KEY options and on the wrong filegroup.
func rowstoreIndexCreate(idx *Index, tableName, with, on string) string {
	var sb strings.Builder
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
			inc[i] = c.ref()
		}
		fmt.Fprintf(&sb, "\n    INCLUDE (%s)", strings.Join(inc, ", "))
	}
	if idx.FilterDefinition != "" {
		fmt.Fprintf(&sb, "\n    WHERE %s", idx.FilterDefinition)
	}
	sb.WriteString(with)
	sb.WriteString(on)
	return sb.String()
}

// indexWithClause renders a rowstore index's WITH (...) options — only those
// that differ from what CREATE INDEX does anyway — preceded by sep, or ""
// when there are none. It serves CREATE INDEX and the PRIMARY KEY / UNIQUE
// constraints alike, which take the same options. extra is appended as
// given — DROP_EXISTING = ON for a rebuild in place — and forces a clause
// even when idx has nothing of its own to say.
func indexWithClause(idx *Index, sep string, extra ...string) string {
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
	if idx.StatisticsNoRecompute {
		o = append(o, "STATISTICS_NORECOMPUTE = ON")
	}
	if !idx.AllowRowLocks {
		o = append(o, "ALLOW_ROW_LOCKS = OFF")
	}
	if !idx.AllowPageLocks {
		o = append(o, "ALLOW_PAGE_LOCKS = OFF")
	}
	// Only ever true on 2019+, so emitting it never hands an older parser an
	// option it rejects unless the script is taken to an older server.
	if idx.OptimizeForSequentialKey {
		o = append(o, "OPTIMIZE_FOR_SEQUENTIAL_KEY = ON")
	}
	if c := idx.DataCompression; c == "ROW" || c == "PAGE" {
		o = append(o, "DATA_COMPRESSION = "+string(c))
	}
	o = append(o, extra...)
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

// explicitDataSpaceClause is dataSpaceClause without the default-filegroup
// omission, for CREATE INDEX ... WITH (DROP_EXISTING = ON): there an omitted
// ON does not mean the default filegroup but the *table's* data space, which
// moved an index off its own filegroup. It errors where no clause can be
// written — a memory-optimized table's index, or a partition scheme whose
// column was not read.
func explicitDataSpaceClause(ds DataSpace) (string, error) {
	switch {
	case ds.Name == "":
		return "", errors.New("the index has no data space to recreate it on")
	case ds.IsPartitionScheme && ds.PartitionColumn == "":
		return "", fmt.Errorf("partition scheme %s has no partitioning column", quoteIdent(ds.Name))
	case ds.IsPartitionScheme:
		return fmt.Sprintf(" ON %s(%s)", quoteIdent(ds.Name), quoteIdent(ds.PartitionColumn)), nil
	default:
		return fmt.Sprintf(" ON %s", quoteIdent(ds.Name)), nil
	}
}

// indexExistenceGuard renders the one-line IF that skips a CREATE INDEX when
// the index is already there. A single statement, so no BEGIN block — see
// buildTableScript on why a block here would break the batch.
func indexExistenceGuard(indexName, tableName string) string {
	return fmt.Sprintf("IF NOT EXISTS (SELECT 1 FROM sys.indexes WHERE name = N'%s' AND object_id = OBJECT_ID(N'%s'))\n",
		escapeSingle(indexName), escapeSingle(tableName))
}
