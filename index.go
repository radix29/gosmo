package gosmo

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// -- Indexes -------------------------------------------------------------------

// Index mirrors Microsoft.SqlServer.Management.Smo.Index.
//
// It carries the object it is on — a table, or an indexed view; exactly one
// of table and view is set — so every write and read on it names that object
// itself; Table or View returns it.
type Index struct {
	table              *Table
	view               *View
	Name               string
	IndexID            int
	Type               IndexType
	IsClustered        bool
	IsUnique           bool
	IsPrimaryKey       bool
	IsUniqueConstraint bool
	IsDisabled         bool
	// IsHypothetical marks a hypothetical index (sys.indexes.is_hypothetical)
	// — the what-if index the Database Engine Tuning Advisor creates WITH
	// STATISTICS_ONLY and often leaves behind. It has statistics and no
	// data: no pages, no data space, and ALTER INDEX on it does nothing.
	// A script leaves it out (there is no documented DDL that recreates
	// one), and DROP INDEX is the one write that means anything on it.
	IsHypothetical bool
	FillFactor     int
	IsPadded       bool
	IgnoreDupKey   bool
	AllowRowLocks  bool
	AllowPageLocks bool
	// Partitions is each partition's DATA_COMPRESSION and XML_COMPRESSION,
	// in partition_number order: element i is partition i+1. An unpartitioned
	// index has one; a hypothetical index has none. DataCompression and
	// XMLCompression derive the first partition's.
	Partitions []PartitionOptions
	// StatisticsNoRecompute is the index's STATISTICS_NORECOMPUTE option,
	// read from its statistics object (sys.stats.no_recompute) — sys.indexes
	// has no column for it.
	StatisticsNoRecompute bool
	// StatisticsIncremental is the index's STATISTICS_INCREMENTAL option —
	// per-partition statistics on a partitioned index — read, like
	// StatisticsNoRecompute, from sys.stats.is_incremental.
	StatisticsIncremental bool
	// OptimizeForSequentialKey is SQL Server 2019's last-page-insert
	// contention option; always false on an older instance.
	OptimizeForSequentialKey bool
	// BucketCount is a hash index's BUCKET_COUNT (sys.hash_indexes), as the
	// server rounded it up to a power of two; 0 for every other index type.
	BucketCount int64
	// CompressionDelay is a columnstore index's COMPRESSION_DELAY in minutes
	// (sys.indexes.compression_delay); 0 for no delay and for every other
	// index type.
	CompressionDelay int
	// ColumnstoreOrder is an ordered columnstore index's ORDER (…) columns,
	// in order (sys.index_columns.column_store_order_ordinal, SQL Server
	// 2022); nil for an unordered one and for every other index type.
	ColumnstoreOrder []string
	KeyColumns       []IndexColumn
	IncludedColumns  []IndexColumn
	FilterDefinition string
	// IsFilterHidden is a filtered index whose predicate the login cannot
	// read: sys.indexes.filter_definition is NULL without VIEW DEFINITION,
	// while has_filter still says there is one. FilterDefinition is then ""
	// and the index is not to be restated as unfiltered.
	IsFilterHidden bool
	DataSpace      DataSpace

	// IsPrimaryXML, PrimaryXMLIndex and SecondaryXMLType describe an XML
	// index (sys.xml_indexes), as CreateIndexRequest's fields of the same
	// names do: the primary form, or a secondary one of that type built over
	// the primary index named. A secondary selective XML index has
	// PrimaryXMLIndex too — the selective index it is built over — and no
	// SecondaryXMLType. All three are zero for every other index type.
	IsPrimaryXML     bool
	PrimaryXMLIndex  string
	SecondaryXMLType XMLSecondaryIndexType
	// IsSelectiveXML marks a selective XML index (xml_index_type 2):
	// SelectiveXMLPaths are its FOR (…) promoted paths in path_id order, and
	// SelectiveXMLNamespaces its WITH XMLNAMESPACES, which the paths'
	// prefixes resolve through. SelectiveXMLPath is a secondary selective
	// XML index's one path, named in its FOR (…) — one of the paths of the
	// selective index PrimaryXMLIndex names. All are zero for every other
	// index type.
	IsSelectiveXML         bool
	SelectiveXMLPaths      []SelectiveXMLPath
	SelectiveXMLNamespaces []XMLNamespace
	SelectiveXMLPath       string
	// Tessellation, BoundingBox, GridLevels and CellsPerObject describe a
	// spatial index (sys.spatial_index_tessellations), as CreateIndexRequest's
	// fields of the same names do. BoundingBox is nil for a geography index,
	// and GridLevels is zero for an automatic-grid one.
	Tessellation   SpatialTessellation
	BoundingBox    *SpatialBoundingBox
	GridLevels     SpatialGridLevels
	CellsPerObject int
}

// DataCompression is the index's DATA_COMPRESSION: its first partition's
// when it is partitioned (Partitions has every one's), NONE when it has no
// partition — a hypothetical index, or one whose catalog was never read.
func (idx *Index) DataCompression() DataCompression {
	if len(idx.Partitions) == 0 {
		return DataCompressionNone
	}
	return idx.Partitions[0].DataCompression
}

// XMLCompression is the index's XML_COMPRESSION (SQL Server 2022): its first
// partition's when it is partitioned, false when it has no partition.
func (idx *Index) XMLCompression() bool {
	return len(idx.Partitions) > 0 && idx.Partitions[0].XMLCompression
}

// DataSpace names where a table or index keeps its rows — the ON clause of
// CREATE TABLE and CREATE INDEX. It is either a filegroup or a partition
// scheme, and for a partition scheme the partitioning column is part of the
// clause, so it is carried here too: `ON [scheme]([column])`.
//
// Name is empty for an index with no data space of its own in sys.indexes —
// a memory-optimized table's, whose rows are not on a filegroup at all.
type DataSpace struct {
	Name              string
	IsPartitionScheme bool
	// IsDefaultFileGroup is true for the database's default filegroup, the
	// one an object with no ON clause lands on. A scripter uses it to leave
	// the clause off where it would say nothing.
	IsDefaultFileGroup bool
	// PartitionColumn is the column the scheme partitions by; set only when
	// IsPartitionScheme.
	PartitionColumn string
}

// dataSpaceColumns and dataSpaceJoins read an index's ON clause out of
// sys.indexes: the data space's name and kind, whether it is the default
// filegroup, and — for a partition scheme — the partitioning column, which
// sys.index_columns marks with partition_ordinal 1.
//
// Every join is a LEFT/OUTER one and every column is wrapped in ISNULL: an
// index can have no data space at all (a memory-optimized table's), and a
// filegroup row exists only for ds.type 'FG'. The partitioning column's
// join is aliased pic, not ic: the index-column query these sit beside is
// told apart from this one by its `sys.index_columns ic`, and two of its
// tests count round trips that way.
const dataSpaceColumns = `ISNULL(ds.name, ''), CASE WHEN ds.type = 'PS' THEN 1 ELSE 0 END,
       ISNULL(fg.is_default, 0), ISNULL(pc.name, '')`

const dataSpaceJoins = `LEFT   JOIN sys.data_spaces ds ON ds.data_space_id = i.data_space_id
LEFT   JOIN sys.filegroups fg ON fg.data_space_id = ds.data_space_id
OUTER  APPLY (SELECT TOP 1 c.name
              FROM   sys.index_columns pic
              JOIN   sys.columns c ON c.object_id = pic.object_id AND c.column_id = pic.column_id
              WHERE  pic.object_id = i.object_id AND pic.index_id = i.index_id
                AND  pic.partition_ordinal > 0
              ORDER  BY pic.partition_ordinal) pc`

// IndexColumn represents one column in an index.
type IndexColumn struct {
	Name       string
	Descending bool
	IsIncluded bool

	// orderOrdinal is the column's place in an ordered columnstore index's
	// ORDER (…), 0 when it has none; Index.ColumnstoreOrder is built from it.
	orderOrdinal int

	// pseudo marks a graph pseudo-column ($node_id, $from_id, …) the
	// scripter substituted for a graph table's internal column. It is
	// written bare: SQL Server 2017 does not resolve it bracketed (Msg 1911),
	// though later releases do.
	pseudo bool
}

// ref is the column as DDL names it: bracket-quoted, or bare for a graph
// pseudo-column.
func (c IndexColumn) ref() string {
	if c.pseudo {
		return c.Name
	}
	return quoteIdent(c.Name)
}

// Indexes returns all indexes on the table.
//
// Two queries, whatever the index count: one for the indexes, one for every
// index column on the object at once — and two more, for the paths and
// namespaces, only when the table has a selective XML index. Fetching each index's columns inside the
// loop over the indexes cost a query per index, and Database.query pins its
// own pooled connection and issues its own USE, so a table with 20 indexes ran
// 42 round trips across 21 connections — with the outer one held throughout,
// which is the shape that exhausts a pool rather than merely being slow.
func (t *Table) Indexes(ctx context.Context) ([]*Index, error) {
	if err := t.requireLoaded("list indexes for"); err != nil {
		return nil, err
	}
	indexes, err := t.indexList(ctx, "")
	if err != nil {
		return nil, fmt.Errorf("gosmo: list indexes for %s: %w", t.FullName(), err)
	}
	if len(indexes) == 0 {
		return nil, nil
	}
	if err := t.attachIndexColumns(ctx, indexes, ""); err != nil {
		return nil, err
	}
	if err := t.attachSelectiveXML(ctx, indexes); err != nil {
		return nil, err
	}
	return indexes, nil
}

// IndexByName returns one index on the table by name, with its columns.
//
// It returns an error satisfying errors.Is(err, ErrNotFound) when the table
// has no such index. Two queries, the same shape as Indexes — see its
// comment for why the columns are not fetched inside the index scan.
func (t *Table) IndexByName(ctx context.Context, name string) (*Index, error) {
	if err := t.requireLoaded(fmt.Sprintf("find index %q on", name)); err != nil {
		return nil, err
	}
	indexes, err := t.indexList(ctx, " AND i.name = @p2", name)
	if err != nil {
		return nil, fmt.Errorf("gosmo: find index %q on %s: %w", name, t.FullName(), err)
	}
	if len(indexes) == 0 {
		return nil, notFoundf("gosmo: index %q not found on %s", name, t.FullName())
	}
	if err := t.attachIndexColumns(ctx, indexes, " AND ic.index_id = @p2", indexes[0].IndexID); err != nil {
		return nil, err
	}
	if err := t.attachSelectiveXML(ctx, indexes); err != nil {
		return nil, err
	}
	return indexes[0], nil
}

// attachIndexColumns fetches the object's index columns in one query and
// distributes them over indexes by index ID.
func (t *Table) attachIndexColumns(ctx context.Context, indexes []*Index, extra string, args ...any) error {
	cols, err := t.indexColumns(ctx, extra, args...)
	if err != nil {
		return fmt.Errorf("gosmo: columns of indexes on %s: %w", t.FullName(), err)
	}
	for _, idx := range indexes {
		var ordered []IndexColumn
		for _, c := range cols[idx.IndexID] {
			if c.IsIncluded {
				idx.IncludedColumns = append(idx.IncludedColumns, c)
			} else {
				idx.KeyColumns = append(idx.KeyColumns, c)
			}
			if c.orderOrdinal > 0 {
				ordered = append(ordered, c)
			}
		}
		slices.SortFunc(ordered, func(a, b IndexColumn) int { return a.orderOrdinal - b.orderOrdinal })
		for _, c := range ordered {
			idx.ColumnstoreOrder = append(idx.ColumnstoreOrder, c.Name)
		}
	}
	return nil
}

// indexListSelect is indexList's query up to its object filter, which the
// caller extends with its own predicate and ORDER BY.
//
// optimize_for_sequential_key is SQL Server 2019 (15.x); sys.indexes has no
// such column before then, and naming it fails the whole read, so an older
// instance reads the option as off — the only value it can have there.
// https://learn.microsoft.com/sql/relational-databases/system-catalog-views/sys-indexes-transact-sql
// STATISTICS_NORECOMPUTE and STATISTICS_INCREMENTAL live on the index's
// statistics object, which shares the index's ID; the join is a LEFT one
// because not every index type is guaranteed a row there.
func (t *Table) indexListSelect() string {
	return `
SELECT i.name, i.index_id, i.type_desc, i.is_unique, i.is_primary_key,
       i.is_unique_constraint, i.is_disabled, i.is_hypothetical, i.fill_factor,
       CASE WHEN i.has_filter = 1 AND i.filter_definition IS NULL THEN NULL ELSE ISNULL(i.filter_definition, '') END,
       i.is_padded, i.ignore_dup_key, i.allow_row_locks, i.allow_page_locks,
       ` + partitionCompressionList(t.db.serverMajorVersion(), "i.object_id", "i.index_id") + `,
       ` + dataSpaceColumns + `,
       ISNULL(st.no_recompute, CAST(0 AS bit)), ISNULL(st.is_incremental, CAST(0 AS bit)),
       ` + bitSince(t.db.serverMajorVersion(), SQLServer2019, "i.optimize_for_sequential_key") + `,
       ISNULL(h.bucket_count, 0), ISNULL(i.compression_delay, 0),
       CAST(CASE WHEN xi.xml_index_type = 0 THEN 1 ELSE 0 END AS bit),
       CASE WHEN xi.xml_index_type IN (1, 3) THEN ISNULL(pxi.name, '') ELSE '' END,
       CASE WHEN xi.xml_index_type = 1 THEN ISNULL(xi.secondary_type_desc, '') ELSE '' END,
       CAST(CASE WHEN xi.xml_index_type = 2 THEN 1 ELSE 0 END AS bit),
       CASE WHEN xi.xml_index_type = 3 THEN ISNULL(sxp.name, '') ELSE '' END,
       ISNULL(sit.tessellation_scheme, ''),
       sit.bounding_box_xmin, sit.bounding_box_ymin, sit.bounding_box_xmax, sit.bounding_box_ymax,
       ISNULL(sit.level_1_grid_desc, ''), ISNULL(sit.level_2_grid_desc, ''),
       ISNULL(sit.level_3_grid_desc, ''), ISNULL(sit.level_4_grid_desc, ''),
       ISNULL(sit.cells_per_object, 0)
FROM   sys.indexes i
LEFT   JOIN sys.hash_indexes h ON h.object_id = i.object_id AND h.index_id = i.index_id
LEFT   JOIN sys.xml_indexes xi ON xi.object_id = i.object_id AND xi.index_id = i.index_id
LEFT   JOIN sys.xml_indexes pxi ON pxi.object_id = xi.object_id AND pxi.index_id = xi.using_xml_index_id
LEFT   JOIN sys.selective_xml_index_paths sxp ON sxp.object_id = xi.object_id
                AND sxp.index_id = xi.using_xml_index_id AND sxp.path_id = xi.path_id
LEFT   JOIN sys.spatial_index_tessellations sit ON sit.object_id = i.object_id AND sit.index_id = i.index_id
LEFT   JOIN sys.stats st ON st.object_id = i.object_id AND st.stats_id = i.index_id
` + dataSpaceJoins + `
WHERE  i.object_id = @p1 AND i.type > 0`
}

// indexList returns the table's indexes with no columns attached,
// narrowed by extra — an additional predicate ANDed onto the object filter,
// with its parameters starting at @p2. Its rows are drained and closed before
// the caller asks for the columns, so the two queries never hold two pooled
// connections at once.
func (t *Table) indexList(ctx context.Context, extra string, args ...any) ([]*Index, error) {
	q := t.indexListSelect() + extra + `
ORDER  BY i.index_id`

	rows, err := t.db.query(ctx, q, append([]any{t.ObjectID}, args...)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var indexes []*Index
	for rows.Next() {
		idx := &Index{table: t}
		var typeDesc, compression, filter sql.NullString
		var secondary, tessellation string
		var xmin, ymin, xmax, ymax sql.NullFloat64
		if err := rows.Scan(&idx.Name, &idx.IndexID, &typeDesc,
			&idx.IsUnique, &idx.IsPrimaryKey, &idx.IsUniqueConstraint,
			&idx.IsDisabled, &idx.IsHypothetical, &idx.FillFactor, &filter,
			&idx.IsPadded, &idx.IgnoreDupKey, &idx.AllowRowLocks, &idx.AllowPageLocks,
			&compression,
			&idx.DataSpace.Name, &idx.DataSpace.IsPartitionScheme,
			&idx.DataSpace.IsDefaultFileGroup, &idx.DataSpace.PartitionColumn,
			&idx.StatisticsNoRecompute, &idx.StatisticsIncremental, &idx.OptimizeForSequentialKey,
			&idx.BucketCount, &idx.CompressionDelay,
			&idx.IsPrimaryXML, &idx.PrimaryXMLIndex, &secondary,
			&idx.IsSelectiveXML, &idx.SelectiveXMLPath,
			&tessellation, &xmin, &ymin, &xmax, &ymax,
			&idx.GridLevels.Level1, &idx.GridLevels.Level2,
			&idx.GridLevels.Level3, &idx.GridLevels.Level4,
			&idx.CellsPerObject); err != nil {
			return nil, err
		}
		idx.FilterDefinition, idx.IsFilterHidden = filter.String, !filter.Valid
		idx.SecondaryXMLType = XMLSecondaryIndexType(secondary)
		idx.Tessellation = SpatialTessellation(tessellation)
		if xmin.Valid && ymin.Valid && xmax.Valid && ymax.Valid {
			idx.BoundingBox = &SpatialBoundingBox{XMin: xmin.Float64, YMin: ymin.Float64, XMax: xmax.Float64, YMax: ymax.Float64}
		}
		if idx.Partitions, err = decodePartitionCompression(compression); err != nil {
			return nil, err
		}
		idx.Type = indexTypeFromDesc(typeDesc.String)
		idx.IsClustered = idx.Type == IndexTypeClustered || idx.Type == IndexTypeClusteredColumnStore
		indexes = append(indexes, idx)
	}
	return indexes, rows.Err()
}

// DataSpace returns where the table itself stores its rows — the filegroup
// or partition scheme its heap or clustered index is on, which is CREATE
// TABLE's ON clause.
//
// Read from index_id 0 or 1, so it answers for a heap as well as a clustered
// table — which is why it is a query of its own rather than a field of the
// index list, whose `i.type > 0` filter has no heap in it.
//
// A table with no row there — a memory-optimized table — reads as the zero
// DataSpace and no error: absence means "no filegroup to name", not a
// failure. A Database.TableRef handle has no ObjectID to read by and is
// refused with ErrHandleNotLoaded.
func (t *Table) DataSpace(ctx context.Context) (DataSpace, error) {
	if err := t.requireLoaded("data space of"); err != nil {
		return DataSpace{}, err
	}
	q := `
SELECT ` + dataSpaceColumns + `
FROM   sys.indexes i
` + dataSpaceJoins + `
WHERE  i.object_id = @p1 AND i.index_id IN (0, 1)`

	var ds DataSpace
	err := t.db.queryRow(ctx, func(row *sql.Row) error {
		return row.Scan(&ds.Name, &ds.IsPartitionScheme, &ds.IsDefaultFileGroup, &ds.PartitionColumn)
	}, q, t.ObjectID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return DataSpace{}, nil
		}
		return DataSpace{}, fmt.Errorf("gosmo: data space of %s: %w", t.FullName(), err)
	}
	return ds, nil
}

// indexColumns returns every index column on the table, keyed by
// index_id and in each index's own key order.
//
// The rows for index_id 0 — the heap's, which no index in the list claims —
// come back too, and are simply never looked up: excluding them would cost a
// predicate to save nothing, since a heap has at most one such row.
//
// A partitioning column the index does not name is left out. The server
// adds one to every partition-aligned index that lacks it and lists it with
// key_ordinal 0 and is_included_column 0, and the ORDER BY would then put it
// first — CREATE INDEX CX (ID) ON ps(Yr) came back as CX (Yr, ID). Every
// column a CREATE INDEX does name is kept: a rowstore key has key_ordinal > 0,
// an included or columnstore column has is_included_column 1, and an XML or
// spatial index's one column has partition_ordinal 0.
//
// extra is an additional predicate ANDed onto the object filter, with its
// parameters starting at @p2 — the same contract as indexList.
func (t *Table) indexColumns(ctx context.Context, extra string, args ...any) (map[int][]IndexColumn, error) {
	q := t.indexColumnsSelect() + extra + `
  AND  NOT (ic.key_ordinal = 0 AND ic.is_included_column = 0 AND ic.partition_ordinal > 0)
ORDER  BY ic.index_id, ic.key_ordinal, ic.index_column_id`

	rows, err := t.db.query(ctx, q, append([]any{t.ObjectID}, args...)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	cols := make(map[int][]IndexColumn)
	for rows.Next() {
		var indexID int
		c := IndexColumn{}
		if err := rows.Scan(&indexID, &c.Name, &c.Descending, &c.IsIncluded, &c.orderOrdinal); err != nil {
			return nil, err
		}
		cols[indexID] = append(cols[indexID], c)
	}
	return cols, rows.Err()
}

// indexColumnsSelect is indexColumns' query up to its object filter.
//
// column_store_order_ordinal is SQL Server 2022's, with ordered columnstore
// indexes; every columnstore index on an older instance is unordered.
// https://learn.microsoft.com/sql/relational-databases/system-catalog-views/sys-index-columns-transact-sql
func (t *Table) indexColumnsSelect() string {
	return `
SELECT ic.index_id, c.name, ic.is_descending_key, ic.is_included_column,
       ` + colSince(t.db.serverMajorVersion(), SQLServer2022, "ic.column_store_order_ordinal", "CAST(0 AS tinyint)") + `
FROM   sys.index_columns ic
JOIN   sys.columns c ON c.object_id = ic.object_id AND c.column_id = ic.column_id
WHERE  ic.object_id = @p1`
}

// indexTypeFromDesc maps sys.indexes.type_desc to an IndexType. A type_desc
// with no case here — NONCLUSTERED HASH, whose verbatim text is
// IndexTypeNonClusteredHash, or a type a newer SQL Server adds — is carried
// through as the server's own text rather than left empty, so a caller
// displays the real type instead of nothing.
func indexTypeFromDesc(desc string) IndexType {
	switch desc = strings.TrimSpace(desc); desc {
	case "CLUSTERED":
		return IndexTypeClustered
	case "NONCLUSTERED":
		return IndexTypeNonClustered
	case "XML":
		return IndexTypeXML
	case "SPATIAL":
		return IndexTypeSpatial
	case "CLUSTERED COLUMNSTORE":
		return IndexTypeClusteredColumnStore
	case "NONCLUSTERED COLUMNSTORE":
		return IndexTypeColumnStore
	}
	return IndexType(desc)
}
