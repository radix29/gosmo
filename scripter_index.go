package gosmo

import (
	"fmt"
	"slices"
	"strings"

	"github.com/radix29/gosmo/tsql"
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
// syntax entirely (a USING/primary-XML-index clause, a selective index's
// promoted paths, a bounding box). Pasting the type_desc into the B-tree
// form — which is what this once did — emits DDL SQL Server rejects. An XML
// or spatial index whose form was not read is emitted as a comment naming
// what was skipped rather than as a statement that cannot run.
//
// A hypothetical index is a comment too. It has no data, only the
// statistics the tuning advisor reads, and no documented DDL recreates one:
// scripted as CREATE INDEX it was built on the target for real, at real
// build cost and write overhead — dozens of them in a database the advisor
// has been through.
func scriptIndex(idx *Index, tableName string, tableDS DataSpace, opts ScriptOptions) string {
	var sb strings.Builder
	switch {
	case idx.IsHypothetical:
		fmt.Fprintf(&sb, "-- hypothetical index %s on %s (tuning advisor) not scripted\n\n",
			tsql.CommentSafe(QuoteName(idx.Name)), tsql.CommentSafe(tableName))
		return sb.String()
	case idx.Type == IndexTypeClusteredColumnStore:
		if opts.IncludeIfNotExists {
			sb.WriteString(indexExistenceGuard(idx.Name, tableName))
		}
		fmt.Fprintf(&sb, "CREATE CLUSTERED COLUMNSTORE INDEX %s ON %s%s%s%s;\nGO\n\n",
			QuoteName(idx.Name), tableName, columnstoreOrderClause(idx), columnstoreWithClause(idx), indexDataSpaceClause(idx.DataSpace, tableDS))
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
		fmt.Fprintf(&sb, "CREATE NONCLUSTERED COLUMNSTORE INDEX %s\n    ON %s (%s)%s",
			QuoteName(idx.Name), tableName, strings.Join(cols, ", "), columnstoreOrderClause(idx))
		// A filtered NCCI (2016+) recreated without its WHERE covers every
		// row instead — a different, larger index under the same name.
		if idx.FilterDefinition != "" {
			fmt.Fprintf(&sb, "\n    WHERE %s", idx.FilterDefinition)
		}
		fmt.Fprintf(&sb, "%s%s;\nGO\n\n", columnstoreWithClause(idx), indexDataSpaceClause(idx.DataSpace, tableDS))
		return sb.String()
	case idx.Type == IndexTypeXML && (idx.IsSelectiveXML || idx.SelectiveXMLPath != ""):
		if opts.IncludeIfNotExists {
			sb.WriteString(indexExistenceGuard(idx.Name, tableName))
		}
		sb.WriteString(selectiveXMLIndexCreate(idx, tableName))
		sb.WriteString(";\nGO\n\n")
		return sb.String()
	case idx.Type == IndexTypeXML && (idx.IsPrimaryXML || idx.PrimaryXMLIndex != ""),
		idx.Type == IndexTypeSpatial && idx.Tessellation != "":
		if opts.IncludeIfNotExists {
			sb.WriteString(indexExistenceGuard(idx.Name, tableName))
		}
		sb.WriteString(xmlOrSpatialIndexCreate(idx, tableName, tableDS))
		sb.WriteString(";\nGO\n\n")
		return sb.String()
	case idx.Type == IndexTypeXML || idx.Type == IndexTypeSpatial:
		fmt.Fprintf(&sb, "-- %s index %s on %s is not scripted (its DDL has no generic form here).\n\n",
			idx.Type, tsql.CommentSafe(QuoteName(idx.Name)), tsql.CommentSafe(tableName))
		return sb.String()
	}

	if opts.IncludeIfNotExists {
		sb.WriteString(indexExistenceGuard(idx.Name, tableName))
	}
	sb.WriteString(rowstoreIndexCreate(idx, tableName, indexWithClause(idx, "\n    "), indexDataSpaceClause(idx.DataSpace, tableDS)))
	sb.WriteString(";\nGO\n\n")
	return sb.String()
}

// columnstoreOrderClause renders an ordered columnstore index's ORDER (…),
// with its leading space, or "" for an unordered one. Left off, the index is
// recreated unordered and loses the segment elimination it was ordered for.
func columnstoreOrderClause(idx *Index) string {
	if len(idx.ColumnstoreOrder) == 0 {
		return ""
	}
	cols := make([]string, len(idx.ColumnstoreOrder))
	for i, c := range idx.ColumnstoreOrder {
		cols[i] = QuoteName(c)
	}
	return " ORDER (" + strings.Join(cols, ", ") + ")"
}

// xmlOrSpatialIndexCreate renders an XML or spatial index's CREATE up to the
// statement terminator.
//
// Neither takes a sort direction, and each takes a narrower WITH list than a
// B-tree index: an XML index has no STATISTICS_NORECOMPUTE or compression
// before 2022, and no ON clause at all — it lives with the table. A spatial
// index's ON is a filegroup only; on a partitioned table it is aligned with
// the table by the server, and naming the scheme is refused.
func xmlOrSpatialIndexCreate(idx *Index, tableName string, tableDS DataSpace) string {
	var sb strings.Builder
	col := ""
	if len(idx.KeyColumns) > 0 {
		col = idx.KeyColumns[0].ref()
	}
	var o []string
	if idx.Type == IndexTypeXML {
		if idx.IsPrimaryXML {
			fmt.Fprintf(&sb, "CREATE PRIMARY XML INDEX %s\n    ON %s (%s)", QuoteName(idx.Name), tableName, col)
		} else {
			fmt.Fprintf(&sb, "CREATE XML INDEX %s\n    ON %s (%s)\n    USING XML INDEX %s FOR %s",
				QuoteName(idx.Name), tableName, col, QuoteName(idx.PrimaryXMLIndex),
				scriptKeyword("secondary XML index type", idx.SecondaryXMLType,
					XMLSecondaryPath, XMLSecondaryValue, XMLSecondaryProperty))
		}
	} else {
		fmt.Fprintf(&sb, "CREATE SPATIAL INDEX %s\n    ON %s (%s)\n    USING %s",
			QuoteName(idx.Name), tableName, col, scriptKeyword("tessellation scheme", idx.Tessellation,
				SpatialGeometryGrid, SpatialGeometryAutoGrid, SpatialGeographyGrid, SpatialGeographyAutoGrid))
		if b := idx.BoundingBox; b != nil && idx.Tessellation.IsGeometry() {
			o = append(o, fmt.Sprintf("BOUNDING_BOX = (%s, %s, %s, %s)",
				floatLiteral(b.XMin), floatLiteral(b.YMin), floatLiteral(b.XMax), floatLiteral(b.YMax)))
		}
		if g := idx.GridLevels.levels(); g != "" && !idx.Tessellation.IsAutoGrid() {
			o = append(o, "GRIDS = ("+g+")")
		}
		if idx.CellsPerObject > 0 {
			o = append(o, fmt.Sprintf("CELLS_PER_OBJECT = %d", idx.CellsPerObject))
		}
	}
	sb.WriteString(withList(append(o, xmlOrSpatialOptions(idx)...)))
	if ds := idx.DataSpace; idx.Type == IndexTypeSpatial && !ds.IsPartitionScheme {
		sb.WriteString(indexDataSpaceClause(ds, tableDS))
	}
	return sb.String()
}

// xmlOrSpatialOptions is the part of an XML or spatial index's WITH list
// the two share — page fill and locking — plus a spatial index's
// NORECOMPUTE and compression, which an XML index does not take.
func xmlOrSpatialOptions(idx *Index) []string {
	var o []string
	if idx.IsPadded {
		o = append(o, "PAD_INDEX = ON")
	}
	if idx.FillFactor > 0 {
		o = append(o, fmt.Sprintf("FILLFACTOR = %d", idx.FillFactor))
	}
	if idx.Type == IndexTypeSpatial && idx.StatisticsNoRecompute {
		o = append(o, "STATISTICS_NORECOMPUTE = ON")
	}
	if !idx.AllowRowLocks {
		o = append(o, "ALLOW_ROW_LOCKS = OFF")
	}
	if !idx.AllowPageLocks {
		o = append(o, "ALLOW_PAGE_LOCKS = OFF")
	}
	if idx.Type == IndexTypeSpatial {
		o = append(o, compressionOptions(idx.Partitions, rowstoreCompressed)...)
	}
	return o
}

// xmlIndexWithClause is an XML index's WITH (…), with its leading line
// break, or "" when every option is at its default.
func xmlIndexWithClause(idx *Index) string {
	return withList(xmlOrSpatialOptions(idx))
}

// withList renders options as an index's WITH (…), with its leading line
// break, or "" for none.
func withList(o []string) string {
	if len(o) == 0 {
		return ""
	}
	return "\n    WITH (" + strings.Join(o, ", ") + ")"
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
		uniq, clust, QuoteName(idx.Name), tableName, indexColumnList(idx.KeyColumns))
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
	if idx.StatisticsIncremental {
		o = append(o, "STATISTICS_INCREMENTAL = ON")
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
	o = append(o, compressionOptions(idx.Partitions, rowstoreCompressed)...)
	o = append(o, xmlCompressionOptions(idx.Partitions)...)
	o = append(o, extra...)
	if len(o) == 0 {
		return ""
	}
	return sep + "WITH (" + strings.Join(o, ", ") + ")"
}

// columnstoreWithClause is indexWithClause for a columnstore index, whose
// only options are a COLUMNSTORE_ARCHIVE compression and a COMPRESSION_DELAY.
func columnstoreWithClause(idx *Index) string {
	o := compressionOptions(idx.Partitions, func(c DataCompression) bool {
		return c == DataCompressionColumnstoreArchive
	})
	if idx.CompressionDelay > 0 {
		o = append(o, fmt.Sprintf("COMPRESSION_DELAY = %d MINUTES", idx.CompressionDelay))
	}
	if len(o) == 0 {
		return ""
	}
	return " WITH (" + strings.Join(o, ", ") + ")"
}

// rowstoreCompressed reports whether a rowstore heap or index compression
// needs naming — NONE is what CREATE does anyway.
func rowstoreCompressed(c DataCompression) bool {
	return c == DataCompressionRow || c == DataCompressionPage
}

// compressionOptions renders the DATA_COMPRESSION option(s) for a heap or
// index whose partitions are parts; named says which values are worth
// writing, the rest being the statement's default.
//
// When every partition agrees — or there is only one — this is the
// single-value form; none read is no option. Otherwise each compression gets its own
// DATA_COMPRESSION = X ON PARTITIONS (…), with contiguous partitions
// collapsed to a range, in the order each first appears: writing only the
// first partition's, which is what this did, recreated a table whose
// partitions mixed PAGE and NONE as PAGE (or NONE) throughout.
func compressionOptions(partitions []PartitionOptions, named func(DataCompression) bool) []string {
	if len(partitions) == 0 {
		return nil
	}
	parts := make([]DataCompression, len(partitions))
	for i, p := range partitions {
		parts[i] = p.DataCompression
	}
	if slices.ContainsFunc(parts[1:], func(c DataCompression) bool { return c != parts[0] }) {
		order, ranges := partitionRuns(parts)
		var o []string
		for _, c := range order {
			if named(c) {
				o = append(o, fmt.Sprintf("DATA_COMPRESSION = %s ON PARTITIONS (%s)", compressionKeyword(c), strings.Join(ranges[c], ", ")))
			}
		}
		return o
	}
	if named(parts[0]) {
		return []string{"DATA_COMPRESSION = " + compressionKeyword(parts[0])}
	}
	return nil
}

// compressionKeyword is c as the DATA_COMPRESSION keyword, checked: c is a
// catalog data_compression_desc here, not a validated request.
func compressionKeyword(c DataCompression) string {
	if c.valid() {
		return string(c)
	}
	return unrecognized("data compression")
}

// xmlCompressionOptions renders XML_COMPRESSION for a heap or rowstore index
// whose partitions are partitions: nothing when none is on, the single form
// when all are, else ON PARTITIONS (…) naming those that are — OFF is the
// statement's default, so the rest need no clause of their own.
func xmlCompressionOptions(partitions []PartitionOptions) []string {
	parts := make([]bool, len(partitions))
	for i, p := range partitions {
		parts[i] = p.XMLCompression
	}
	if !slices.Contains(parts, true) {
		return nil
	}
	if !slices.Contains(parts, false) {
		return []string{"XML_COMPRESSION = ON"}
	}
	_, ranges := partitionRuns(parts)
	return []string{"XML_COMPRESSION = ON ON PARTITIONS (" + strings.Join(ranges[true], ", ") + ")"}
}

// partitionRuns groups per-partition values (element i is partition i+1)
// into ON PARTITIONS ranges, contiguous partitions collapsed to "a TO b":
// each distinct value in the order it first appears, and its ranges.
func partitionRuns[T comparable](parts []T) ([]T, map[T][]string) {
	var order []T
	ranges := map[T][]string{}
	for start := 0; start < len(parts); {
		end := start
		for end+1 < len(parts) && parts[end+1] == parts[start] {
			end++
		}
		c := parts[start]
		if _, seen := ranges[c]; !seen {
			order = append(order, c)
		}
		if start == end {
			ranges[c] = append(ranges[c], fmt.Sprint(start+1))
		} else {
			ranges[c] = append(ranges[c], fmt.Sprintf("%d TO %d", start+1, end+1))
		}
		start = end + 1
	}
	return order, ranges
}

// indexDisableStatement renders the ALTER INDEX ... DISABLE that leaves a
// recreated index disabled as its source was, or "" for an enabled one —
// and for a hypothetical one, which scriptIndex does not recreate.
func indexDisableStatement(idx *Index, tableName string) string {
	if !idx.IsDisabled || idx.IsHypothetical {
		return ""
	}
	return fmt.Sprintf("ALTER INDEX %s ON %s DISABLE;\nGO\n\n", QuoteName(idx.Name), tableName)
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
		return fmt.Sprintf(" ON %s(%s)", QuoteName(ds.Name), QuoteName(ds.PartitionColumn))
	case ds.Name == "" || ds.IsDefaultFileGroup || ds.IsPartitionScheme:
		// A partition scheme with no partitioning column is not a clause
		// anything can be written from: emit nothing rather than DDL that
		// cannot parse.
		return ""
	default:
		return fmt.Sprintf(" ON %s", QuoteName(ds.Name))
	}
}

// indexDataSpaceClause is dataSpaceClause for an index or a key constraint,
// whose omitted ON means something else: not the default filegroup but the
// data space of its table (tableDS), as for a constraint declared inside
// CREATE TABLE. An index on the default filegroup of a table that is
// elsewhere — a non-aligned index on a partitioned table, an index ON
// [PRIMARY] of a table on [fg 2] — is therefore given its ON clause, or the
// replay puts it beside the table. Where tableDS is not known (the zero
// DataSpace) the default-filegroup rule stands.
func indexDataSpaceClause(ds, tableDS DataSpace) string {
	c := dataSpaceClause(ds)
	if c != "" || tableDS.Name == "" || ds == tableDS {
		return c
	}
	c, err := explicitDataSpaceClause(ds)
	if err != nil {
		return ""
	}
	return c
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
		return "", unsupportedf("the index has no data space to recreate it on")
	case ds.IsPartitionScheme && ds.PartitionColumn == "":
		return "", fmt.Errorf("partition scheme %s has no partitioning column", QuoteName(ds.Name))
	case ds.IsPartitionScheme:
		return fmt.Sprintf(" ON %s(%s)", QuoteName(ds.Name), QuoteName(ds.PartitionColumn)), nil
	default:
		return fmt.Sprintf(" ON %s", QuoteName(ds.Name)), nil
	}
}

// indexExistenceGuard renders the one-line IF that skips a CREATE INDEX when
// the index is already there. A single statement, so no BEGIN block — see
// buildTableScript on why a block here would break the batch.
func indexExistenceGuard(indexName, tableName string) string {
	return fmt.Sprintf("IF NOT EXISTS (SELECT 1 FROM sys.indexes WHERE name = %s AND object_id = OBJECT_ID(%s))\n",
		QuoteLiteral(indexName), QuoteLiteral(tableName))
}
