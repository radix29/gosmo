package gosmo

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// -- Indexes -------------------------------------------------------------------

// Index mirrors Microsoft.SqlServer.Management.Smo.Index.
//
// It carries the table it is on, so every write and read on it names that
// table itself; Table returns it.
type Index struct {
	table              *Table
	Name               string
	IndexID            int
	Type               IndexType
	IsClustered        bool
	IsUnique           bool
	IsPrimaryKey       bool
	IsUniqueConstraint bool
	IsDisabled         bool
	FillFactor         int
	IsPadded           bool
	IgnoreDupKey       bool
	AllowRowLocks      bool
	AllowPageLocks     bool
	// DataCompression is the index's compression — its first partition's
	// when it is partitioned; PartitionCompression has every partition's.
	DataCompression DataCompression
	// PartitionCompression is each partition's DATA_COMPRESSION, in
	// partition_number order: element i is partition i+1. A partitioned
	// index can mix them, and a script written from DataCompression alone
	// recreated it with one compression throughout.
	PartitionCompression []DataCompression
	// StatisticsNoRecompute is the index's STATISTICS_NORECOMPUTE option,
	// read from its statistics object (sys.stats.no_recompute) — sys.indexes
	// has no column for it.
	StatisticsNoRecompute bool
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
	KeyColumns       []IndexColumn
	IncludedColumns  []IndexColumn
	FilterDefinition string
	DataSpace        DataSpace

	// IsPrimaryXML, PrimaryXMLIndex and SecondaryXMLType describe an XML
	// index (sys.xml_indexes), as CreateIndexRequest's fields of the same
	// names do: the primary form, or a secondary one of that type built over
	// the primary index named. All three are zero for a selective XML index,
	// which neither form describes, and for every other index type.
	IsPrimaryXML     bool
	PrimaryXMLIndex  string
	SecondaryXMLType XMLSecondaryIndexType
	// Tessellation, BoundingBox, GridLevels and CellsPerObject describe a
	// spatial index (sys.spatial_index_tessellations), as CreateIndexRequest's
	// fields of the same names do. BoundingBox is nil for a geography index,
	// and GridLevels is zero for an automatic-grid one.
	Tessellation   SpatialTessellation
	BoundingBox    *SpatialBoundingBox
	GridLevels     SpatialGridLevels
	CellsPerObject int
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
// index column on the object at once. Fetching each index's columns inside the
// loop over the indexes cost a query per index, and Database.query pins its
// own pooled connection and issues its own USE, so a table with 20 indexes ran
// 42 round trips across 21 connections — with the outer one held throughout,
// which is the shape that exhausts a pool rather than merely being slow.
func (t *Table) Indexes(ctx context.Context) ([]*Index, error) {
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
	return indexes, nil
}

// IndexByName returns one index on the table by name, with its columns.
//
// It returns an error satisfying errors.Is(err, ErrNotFound) when the table
// has no such index. Two queries, the same shape as Indexes — see its
// comment for why the columns are not fetched inside the index scan.
func (t *Table) IndexByName(ctx context.Context, name string) (*Index, error) {
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
		for _, c := range cols[idx.IndexID] {
			if c.IsIncluded {
				idx.IncludedColumns = append(idx.IncludedColumns, c)
			} else {
				idx.KeyColumns = append(idx.KeyColumns, c)
			}
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
// STATISTICS_NORECOMPUTE lives on the index's statistics object, which
// shares the index's ID; the join is a LEFT one because not every index type
// is guaranteed a row there.
func (t *Table) indexListSelect() string {
	return `
SELECT i.name, i.index_id, i.type_desc, i.is_unique, i.is_primary_key,
       i.is_unique_constraint, i.is_disabled, i.fill_factor,
       ISNULL(i.filter_definition, ''),
       i.is_padded, i.ignore_dup_key, i.allow_row_locks, i.allow_page_locks,
       ` + partitionCompressionList("i.object_id", "i.index_id") + `,
       ` + dataSpaceColumns + `,
       ISNULL(st.no_recompute, CAST(0 AS bit)),
       ` + colSince(t.db.serverMajorVersion(), SQLServer2019, "i.optimize_for_sequential_key", "CAST(0 AS bit)") + `,
       ISNULL(h.bucket_count, 0), ISNULL(i.compression_delay, 0),
       CAST(CASE WHEN xi.xml_index_type = 0 THEN 1 ELSE 0 END AS bit),
       CASE WHEN xi.xml_index_type = 1 THEN ISNULL(pxi.name, '') ELSE '' END,
       CASE WHEN xi.xml_index_type = 1 THEN ISNULL(xi.secondary_type_desc, '') ELSE '' END,
       ISNULL(sit.tessellation_scheme, ''),
       sit.bounding_box_xmin, sit.bounding_box_ymin, sit.bounding_box_xmax, sit.bounding_box_ymax,
       ISNULL(sit.level_1_grid_desc, ''), ISNULL(sit.level_2_grid_desc, ''),
       ISNULL(sit.level_3_grid_desc, ''), ISNULL(sit.level_4_grid_desc, ''),
       ISNULL(sit.cells_per_object, 0)
FROM   sys.indexes i
LEFT   JOIN sys.hash_indexes h ON h.object_id = i.object_id AND h.index_id = i.index_id
LEFT   JOIN sys.xml_indexes xi ON xi.object_id = i.object_id AND xi.index_id = i.index_id
LEFT   JOIN sys.xml_indexes pxi ON pxi.object_id = xi.object_id AND pxi.index_id = xi.using_xml_index_id
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
		var typeDesc, compression sql.NullString
		var secondary, tessellation string
		var xmin, ymin, xmax, ymax sql.NullFloat64
		if err := rows.Scan(&idx.Name, &idx.IndexID, &typeDesc,
			&idx.IsUnique, &idx.IsPrimaryKey, &idx.IsUniqueConstraint,
			&idx.IsDisabled, &idx.FillFactor, &idx.FilterDefinition,
			&idx.IsPadded, &idx.IgnoreDupKey, &idx.AllowRowLocks, &idx.AllowPageLocks,
			&compression,
			&idx.DataSpace.Name, &idx.DataSpace.IsPartitionScheme,
			&idx.DataSpace.IsDefaultFileGroup, &idx.DataSpace.PartitionColumn,
			&idx.StatisticsNoRecompute, &idx.OptimizeForSequentialKey,
			&idx.BucketCount, &idx.CompressionDelay,
			&idx.IsPrimaryXML, &idx.PrimaryXMLIndex, &secondary,
			&tessellation, &xmin, &ymin, &xmax, &ymax,
			&idx.GridLevels.Level1, &idx.GridLevels.Level2,
			&idx.GridLevels.Level3, &idx.GridLevels.Level4,
			&idx.CellsPerObject); err != nil {
			return nil, err
		}
		idx.SecondaryXMLType = XMLSecondaryIndexType(secondary)
		idx.Tessellation = SpatialTessellation(tessellation)
		if xmin.Valid && ymin.Valid && xmax.Valid && ymax.Valid {
			idx.BoundingBox = &SpatialBoundingBox{XMin: xmin.Float64, YMin: ymin.Float64, XMax: xmax.Float64, YMax: ymax.Float64}
		}
		if idx.PartitionCompression, err = decodePartitionCompression(compression); err != nil {
			return nil, err
		}
		idx.DataCompression = DataCompressionNone
		if len(idx.PartitionCompression) > 0 {
			idx.DataCompression = idx.PartitionCompression[0]
		}
		switch desc := strings.TrimSpace(typeDesc.String); desc {
		case "CLUSTERED":
			idx.Type = IndexTypeClustered
			idx.IsClustered = true
		case "NONCLUSTERED":
			idx.Type = IndexTypeNonClustered
		case "XML":
			idx.Type = IndexTypeXML
		case "SPATIAL":
			idx.Type = IndexTypeSpatial
		case "CLUSTERED COLUMNSTORE":
			idx.Type = IndexTypeClusteredColumnStore
			idx.IsClustered = true
		case "NONCLUSTERED COLUMNSTORE":
			idx.Type = IndexTypeColumnStore
		default:
			// A type_desc with no case here — NONCLUSTERED HASH, whose
			// verbatim text is IndexTypeNonClusteredHash, or a type a newer
			// SQL Server adds — is carried through as the server's own text rather than left
			// empty, so a caller displays the real type instead of nothing.
			idx.Type = IndexType(desc)
		}
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
// A table with no row there at all — a Database.TableRef handle, whose ObjectID
// is zero, or a memory-optimized table — reads as the zero DataSpace and no
// error: absence means "no filegroup to name", not a failure.
func (t *Table) DataSpace(ctx context.Context) (DataSpace, error) {
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
	q := `
SELECT ic.index_id, c.name, ic.is_descending_key, ic.is_included_column
FROM   sys.index_columns ic
JOIN   sys.columns c ON c.object_id = ic.object_id AND c.column_id = ic.column_id
WHERE  ic.object_id = @p1` + extra + `
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
		if err := rows.Scan(&indexID, &c.Name, &c.Descending, &c.IsIncluded); err != nil {
			return nil, err
		}
		cols[indexID] = append(cols[indexID], c)
	}
	return cols, rows.Err()
}

// -- Index management ----------------------------------------------------------

// Table returns the table the index is on.
func (idx *Index) Table() *Table { return idx.table }

// IndexRef returns a lightweight handle for name on the table without
// querying the server at all — unlike IndexByName, it doesn't verify the
// index exists or populate IndexID/Type/FillFactor/etc. (they stay at their
// zero value). The write methods that only name the index — Rebuild,
// Reorganize, Disable, Enable, Drop, SetOptions, Rename,
// UpdateStatistics — and the two reads StorageInfo and Fragmentation, which
// resolve the index by name, work from a handle. SetIncludedColumns and
// IncludedColumnsSupported restate the index as read and so need
// IndexByName. See Server.DatabaseRef's doc comment for when a handle is the
// right form.
func (t *Table) IndexRef(name string) *Index {
	return &Index{table: t, Name: name}
}

// target is the `[index] ON [schema].[table]` pair every ALTER/DROP INDEX
// statement names.
func (idx *Index) target() string {
	return quoteIdent(idx.Name) + " ON " + idx.table.FullName()
}

// Reorganize reorganizes the index (ALTER INDEX ... REORGANIZE).
func (idx *Index) Reorganize(ctx context.Context) error {
	q := fmt.Sprintf("ALTER INDEX %s REORGANIZE", idx.target())
	if _, err := idx.table.db.exec(ctx, q); err != nil {
		return fmt.Errorf("gosmo: reorganize index %q: %w", idx.Name, err)
	}
	return nil
}

// Disable disables the index (ALTER INDEX ... DISABLE).
func (idx *Index) Disable(ctx context.Context) error {
	q := fmt.Sprintf("ALTER INDEX %s DISABLE", idx.target())
	if _, err := idx.table.db.exec(ctx, q); err != nil {
		return fmt.Errorf("gosmo: disable index %q: %w", idx.Name, err)
	}
	setIfApplied(ctx, &idx.IsDisabled, true)
	return nil
}

// Enable re-enables a disabled index by rebuilding it — with no options, so
// the rebuild does not change a stored setting as a side effect.
func (idx *Index) Enable(ctx context.Context) error {
	return idx.Rebuild(ctx, IndexRebuildOptions{})
}

// Drop drops the index.
func (idx *Index) Drop(ctx context.Context) error {
	q := fmt.Sprintf("DROP INDEX %s", idx.target())
	if _, err := idx.table.db.exec(ctx, q); err != nil {
		return fmt.Errorf("gosmo: drop index %q: %w", idx.Name, err)
	}
	return nil
}

// RebuildAllIndexes rebuilds all indexes on the table (ALTER INDEX ALL ... REBUILD).
func (t *Table) RebuildAllIndexes(ctx context.Context, fillFactor int) error {
	q := fmt.Sprintf("ALTER INDEX ALL ON %s REBUILD", t.FullName())
	if fillFactor > 0 {
		q += fmt.Sprintf(" WITH (FILLFACTOR = %d)", fillFactor)
	}
	if _, err := t.db.exec(ctx, q); err != nil {
		return fmt.Errorf("gosmo: rebuild all indexes on %s: %w", t.FullName(), err)
	}
	return nil
}

// onOffKeyword renders a bool as the ON/OFF keyword ALTER INDEX SET/REBUILD
// WITH options expect.
func onOffKeyword(b bool) string {
	if b {
		return "ON"
	}
	return "OFF"
}

// IndexSetOptions are the index options ALTER INDEX ... SET changes in
// place. A nil field leaves that option as it is and is not sent.
//
// Leave IgnoreDupKey nil for an index backing a PRIMARY KEY or UNIQUE
// constraint: SQL Server rejects the option outright there, whatever its
// value ("Cannot use index option ignore_dup_key to alter index '...' as it
// enforces a primary or unique constraint").
type IndexSetOptions struct {
	IgnoreDupKey   *bool
	AllowRowLocks  *bool
	AllowPageLocks *bool
}

// SetOptions applies the index's SET-able runtime options (ALTER INDEX ...
// SET), sending only the ones opts names. Fill factor, pad index, and data
// compression only take effect on a rebuild — see Rebuild for those. An opts naming nothing is an error, not a no-op: ALTER INDEX ...
// SET () is a syntax error, and a caller that meant to change something
// should hear that it didn't.
func (idx *Index) SetOptions(ctx context.Context, opts IndexSetOptions) error {
	var set []string
	for _, o := range []struct {
		name string
		v    *bool
	}{
		{"IGNORE_DUP_KEY", opts.IgnoreDupKey},
		{"ALLOW_ROW_LOCKS", opts.AllowRowLocks},
		{"ALLOW_PAGE_LOCKS", opts.AllowPageLocks},
	} {
		if o.v != nil {
			set = append(set, o.name+" = "+onOffKeyword(*o.v))
		}
	}
	if len(set) == 0 {
		return fmt.Errorf("gosmo: set options on index %q: no option given", idx.Name)
	}
	q := fmt.Sprintf("ALTER INDEX %s SET (%s)", idx.target(), strings.Join(set, ", "))
	if _, err := idx.table.db.exec(ctx, q); err != nil {
		return fmt.Errorf("gosmo: set options on index %q: %w", idx.Name, err)
	}
	return nil
}

// Rename renames the index using sp_rename — also the mechanism for
// renaming a PRIMARY KEY or UNIQUE constraint, since its name is the
// backing index's name in sys.indexes.
func (idx *Index) Rename(ctx context.Context, newName string) error {
	objName := idx.table.FullName() + "." + quoteIdent(idx.Name)
	if _, err := idx.table.db.exec(ctx,
		"EXEC sp_rename @objname = @p1, @newname = @p2, @objtype = N'INDEX'",
		objName, newName,
	); err != nil {
		return fmt.Errorf("gosmo: rename index %q to %q: %w", idx.Name, newName, err)
	}
	setIfApplied(ctx, &idx.Name, newName)
	return nil
}

// partitionCompressionList renders a correlated FOR JSON subquery listing
// each partition's compression of the heap or index objectID/indexID name,
// in partition_number order, for decodePartitionCompression. JSON rather
// than TOP 1: the first partition's alone was what made a partitioned index
// with mixed compression script as uniformly compressed.
func partitionCompressionList(objectID, indexID string) string {
	return jsonRows("pp.partition_number AS n, pp.data_compression_desc AS c",
		"\n        FROM sys.partitions pp WHERE pp.object_id = "+objectID+" AND pp.index_id = "+indexID,
		"pp.partition_number")
}

// decodePartitionCompression decodes one partitionCompressionList column into
// a slice indexed by partition_number - 1. NULL (no partitions) is nil.
func decodePartitionCompression(col sql.NullString) ([]DataCompression, error) {
	rows, err := decodeJSONRows[struct {
		N int    `json:"n"`
		C string `json:"c"`
	}](col)
	if err != nil || rows == nil {
		return nil, err
	}
	out := make([]DataCompression, len(rows))
	for i, r := range rows {
		if r.N != i+1 {
			return nil, fmt.Errorf("gosmo: partition %d listed at position %d", r.N, i+1)
		}
		out[i] = DataCompression(r.C)
	}
	return out, nil
}

// DataCompression is an index's or partition's DATA_COMPRESSION keyword.
// NONE, ROW and PAGE apply to a rowstore index, COLUMNSTORE and
// COLUMNSTORE_ARCHIVE to a columnstore one.
type DataCompression string

const (
	DataCompressionNone               DataCompression = "NONE"
	DataCompressionRow                DataCompression = "ROW"
	DataCompressionPage               DataCompression = "PAGE"
	DataCompressionColumnstore        DataCompression = "COLUMNSTORE"
	DataCompressionColumnstoreArchive DataCompression = "COLUMNSTORE_ARCHIVE"
)

// valid reports whether c is one of the five keywords. The empty value is
// not; callers that read it as "unspecified" check for it first.
func (c DataCompression) valid() bool {
	switch c {
	case DataCompressionNone, DataCompressionRow, DataCompressionPage,
		DataCompressionColumnstore, DataCompressionColumnstoreArchive:
		return true
	}
	return false
}

// IndexRebuildOptions are the options ALTER INDEX ... REBUILD WITH can
// set. The zero value is a plain REBUILD that keeps every stored setting.
//
// Fill factor, pad index and data compression are rebuild-only: none is an
// ALTER INDEX ... SET option, so a rebuild is the only way to change them.
type IndexRebuildOptions struct {
	// FillFactor is the leaf-page fill percentage, 1-100. Zero keeps the
	// index's stored fill factor.
	FillFactor int
	// PadIndex applies the fill factor to the intermediate pages too
	// (PAD_INDEX). Nil leaves it unspecified.
	PadIndex *bool
	// DataCompression is the compression to rebuild with. Empty keeps the
	// index's current setting.
	DataCompression DataCompression
}

// Rebuild rebuilds the index (ALTER INDEX ... REBUILD), with a WITH clause
// only for the options opts sets.
func (idx *Index) Rebuild(ctx context.Context, opts IndexRebuildOptions) error {
	if opts.DataCompression != "" && !opts.DataCompression.valid() {
		return fmt.Errorf("gosmo: rebuild index %q: invalid data compression %q (must be NONE, ROW, PAGE, COLUMNSTORE, or COLUMNSTORE_ARCHIVE)", idx.Name, opts.DataCompression)
	}

	var withParts []string
	if opts.PadIndex != nil {
		withParts = append(withParts, "PAD_INDEX = "+onOffKeyword(*opts.PadIndex))
	}
	if opts.FillFactor > 0 {
		withParts = append(withParts, fmt.Sprintf("FILLFACTOR = %d", opts.FillFactor))
	}
	if opts.DataCompression != "" {
		withParts = append(withParts, "DATA_COMPRESSION = "+string(opts.DataCompression))
	}
	q := fmt.Sprintf("ALTER INDEX %s REBUILD", idx.target())
	if len(withParts) > 0 {
		q += " WITH (" + strings.Join(withParts, ", ") + ")"
	}
	if _, err := idx.table.db.exec(ctx, q); err != nil {
		return fmt.Errorf("gosmo: rebuild index %q: %w", idx.Name, err)
	}
	return nil
}

// SetIncludedColumns replaces the index's included (non-key) columns.
// Changing included columns isn't a plain ALTER — it requires recreating the
// index, so this reissues a full CREATE INDEX ... WITH (DROP_EXISTING = ON)
// from idx as read, with columns as the new INCLUDE list.
//
// DROP_EXISTING builds the index from what the statement says, not from the
// index it replaces: every option idx carries is restated, and the ON clause
// is always explicit (see explicitDataSpaceClause). A disabled index is
// enabled by the rebuild, so the same batch disables it again.
//
// Only a rowstore nonclustered index that backs no constraint can have its
// INCLUDE list changed. Anything else is refused before a statement is sent,
// with an error naming what the index is: a columnstore index would be
// recreated as a rowstore one, and a clustered, XML, spatial, hash or
// constraint-backing index would fail at the server anyway.
func (idx *Index) SetIncludedColumns(ctx context.Context, columns []string) error {
	if err := idx.IncludedColumnsSupported(); err != nil {
		return fmt.Errorf("gosmo: set included columns on %q: %w", idx.Name, err)
	}
	on, err := explicitDataSpaceClause(idx.DataSpace)
	if err != nil {
		return fmt.Errorf("gosmo: set included columns on %q: %w", idx.Name, err)
	}
	next := *idx
	next.IncludedColumns = make([]IndexColumn, len(columns))
	for i, c := range columns {
		next.IncludedColumns[i] = IndexColumn{Name: c, IsIncluded: true}
	}
	q := rowstoreIndexCreate(&next, idx.table.FullName(), indexWithClause(&next, "\n    ", "DROP_EXISTING = ON"), on)
	if idx.IsDisabled {
		q += fmt.Sprintf(";\nALTER INDEX %s DISABLE", idx.target())
	}
	if _, err := idx.table.db.exec(ctx, q); err != nil {
		return fmt.Errorf("gosmo: set included columns on index %q: %w", idx.Name, err)
	}
	return nil
}

// IncludedColumnsSupported reports whether SetIncludedColumns can change
// idx's INCLUDE list, and if not, why — so a caller can grey the choice out
// up front rather than learn it on Apply. nil means supported.
func (idx *Index) IncludedColumnsSupported() error {
	switch {
	case idx.Type == "":
		// An IndexRef handle: nothing was read, so nothing can be restated.
		return errors.New("the index's properties were not read — use Table.IndexByName, not IndexRef")
	case idx.Type != IndexTypeNonClustered:
		return fmt.Errorf("not supported for a %s index", idx.Type)
	case idx.IsPrimaryKey:
		return errors.New("not supported for an index backing a PRIMARY KEY constraint")
	case idx.IsUniqueConstraint:
		return errors.New("not supported for an index backing a UNIQUE constraint")
	case idx.DataSpace.Name == "":
		// A memory-optimized table's index: no data space of its own, and
		// its indexes are changed with ALTER TABLE, never CREATE INDEX.
		return errors.New("not supported for an index on a memory-optimized table")
	}
	return nil
}

// UpdateStatistics updates the statistics object tied to this index
// (UPDATE STATISTICS table (index) — every index has an implicit
// statistics object with the same name). samplePct means what it means to
// Statistic.Update: 0 is a FULLSCAN, 1-100 SAMPLE n PERCENT.
//
// It took no sample until 2026-09-23 and so used the server's default
// sampling, while Statistic.Update(0) was a FULLSCAN: "update statistics"
// read the table differently depending on which node it was asked from.
func (idx *Index) UpdateStatistics(ctx context.Context, samplePct int) error {
	if err := checkSamplePct("update statistics for index "+idx.Name, samplePct); err != nil {
		return err
	}
	q := fmt.Sprintf("UPDATE STATISTICS %s (%s) WITH %s", idx.table.FullName(), quoteIdent(idx.Name), sampleClause(samplePct))
	if _, err := idx.table.db.exec(ctx, q); err != nil {
		return fmt.Errorf("gosmo: update statistics for index %q: %w", idx.Name, err)
	}
	return nil
}

// IndexAllocationUnit is one row of an index's allocation-unit space
// breakdown (IN_ROW_DATA, LOB_DATA, ROW_OVERFLOW_DATA).
type IndexAllocationUnit struct {
	Type   string
	Pages  int64
	UsedKB int64
}

// IndexStorageInfo holds an index's filegroup/partitioning and space usage
// — SSMS's Index Properties > Storage page.
type IndexStorageInfo struct {
	FileGroup       string
	PartitionScheme string
	PartitionColumn string
	RowCount        int64
	UsedKB          int64
	ReservedKB      int64
	AvgRecordSize   float64
	Allocations     []IndexAllocationUnit
}

// StorageInfo returns filegroup/partitioning and space usage for this index.
// The row count comes from sys.partitions alone: summed over the
// allocation-unit join, each partition's rows count once per allocation
// unit, so a table with LOB and row-overflow columns reported three times its
// rows.
//
// The index is found by its table's name and its own, as Fragmentation finds
// it, so both work from an IndexRef handle — whose table may itself be a
// TableRef with no ObjectID.
func (idx *Index) StorageInfo(ctx context.Context) (*IndexStorageInfo, error) {
	target := fmt.Sprintf("i.object_id = OBJECT_ID(N'%s') AND i.name = @p1", escapeSingle(idx.table.FullName()))
	headerQ := `
SELECT
    ds.name, ds.type,
    ISNULL(pf.name, ''),
    ISNULL((SELECT c.name FROM sys.index_columns ic
            JOIN sys.columns c ON c.object_id = ic.object_id AND c.column_id = ic.column_id
            WHERE ic.object_id = i.object_id AND ic.index_id = i.index_id AND ic.partition_ordinal = 1), ''),
    ISNULL((SELECT SUM(p.rows) FROM sys.partitions p
            WHERE p.object_id = i.object_id AND p.index_id = i.index_id), 0),
    ISNULL(SUM(a.used_pages), 0) * 8,
    ISNULL(SUM(a.total_pages), 0) * 8
FROM   sys.indexes i
JOIN   sys.data_spaces ds ON ds.data_space_id = i.data_space_id
LEFT   JOIN sys.partition_schemes ps ON ps.data_space_id = i.data_space_id
LEFT   JOIN sys.partition_functions pf ON pf.function_id = ps.function_id
LEFT   JOIN sys.partitions p ON p.object_id = i.object_id AND p.index_id = i.index_id
LEFT   JOIN sys.allocation_units a ON a.container_id = p.partition_id
WHERE  ` + target + `
GROUP  BY ds.name, ds.type, pf.name, i.object_id, i.index_id`

	db := idx.table.db
	info := &IndexStorageInfo{}
	var dsType string
	var fgOrPS string
	err := db.queryRow(ctx, func(row *sql.Row) error {
		return row.Scan(&fgOrPS, &dsType, &info.PartitionScheme, &info.PartitionColumn,
			&info.RowCount, &info.UsedKB, &info.ReservedKB)
	}, headerQ, idx.Name)
	if err != nil {
		return nil, fmt.Errorf("gosmo: storage info for index %q: %w", idx.Name, err)
	}
	if strings.TrimSpace(dsType) == "FG" {
		info.FileGroup = fgOrPS
	} else {
		info.PartitionScheme = fgOrPS
	}

	avgQ := `
SELECT TOP 1 s.avg_record_size_in_bytes
FROM   sys.indexes i
CROSS  APPLY sys.dm_db_index_physical_stats(DB_ID(), i.object_id, i.index_id, NULL, 'SAMPLED') s
WHERE  ` + target + ` AND s.index_level = 0`
	var avg sql.NullFloat64
	if err := db.queryRow(ctx, func(row *sql.Row) error { return row.Scan(&avg) }, avgQ, idx.Name); err == nil {
		info.AvgRecordSize = avg.Float64
	}

	allocQ := `
SELECT a.type_desc, SUM(a.used_pages), SUM(a.used_pages) * 8
FROM   sys.indexes i
JOIN   sys.partitions p ON p.object_id = i.object_id AND p.index_id = i.index_id
JOIN   sys.allocation_units a ON a.container_id = p.partition_id
WHERE  ` + target + `
GROUP  BY a.type_desc
ORDER  BY a.type_desc`
	rows, err := db.query(ctx, allocQ, idx.Name)
	if err != nil {
		return nil, fmt.Errorf("gosmo: allocation units for index %q: %w", idx.Name, err)
	}
	defer rows.Close()
	for rows.Next() {
		var au IndexAllocationUnit
		if err := rows.Scan(&au.Type, &au.Pages, &au.UsedKB); err != nil {
			return nil, fmt.Errorf("gosmo: storage info for index %q: %w", idx.Name, err)
		}
		info.Allocations = append(info.Allocations, au)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("gosmo: storage info for index %q: %w", idx.Name, err)
	}
	return info, nil
}

// Fragmentation returns fragmentation and page-density statistics for this
// index alone — the single-index analog of Table.FragmentationStats, used
// by Index Properties' Fragmentation page. An empty mode is
// FragmentationLimited; page density is only populated by SAMPLED or
// DETAILED (LIMITED always reports 0, same as the underlying DMV).
//
// The DMV is applied to the index sys.indexes finds by name rather than
// called with OBJECT_ID directly: a name that resolves to nothing then
// yields no row, where a NULL object_id passed to the DMV would mean every
// object in the database.
func (idx *Index) Fragmentation(ctx context.Context, mode FragmentationMode) (*IndexFragmentation, error) {
	if mode == "" {
		mode = FragmentationLimited
	}
	switch mode {
	case FragmentationLimited, FragmentationSampled, FragmentationDetailed:
	default:
		return nil, fmt.Errorf("gosmo: fragmentation for index %q: invalid mode %q (must be LIMITED, SAMPLED, or DETAILED)", idx.Name, mode)
	}

	q := fmt.Sprintf(`
SELECT i.name, s.index_id,
       s.avg_fragmentation_in_percent,
       s.page_count,
       s.fragment_count,
       s.avg_page_space_used_in_percent
FROM   sys.indexes i
CROSS  APPLY sys.dm_db_index_physical_stats(DB_ID(), i.object_id, i.index_id, NULL, N'%s') s
WHERE  i.object_id = OBJECT_ID(N'%s') AND i.name = @p1 AND s.index_level = 0`,
		mode, escapeSingle(idx.table.FullName()))

	f := &IndexFragmentation{}
	var density sql.NullFloat64
	if err := idx.table.db.queryRow(ctx, func(row *sql.Row) error {
		return row.Scan(&f.IndexName, &f.IndexID, &f.AvgFragmentationPct, &f.PageCount, &f.FragmentCount, &density)
	}, q, idx.Name); err != nil {
		return nil, fmt.Errorf("gosmo: fragmentation for index %q: %w", idx.Name, err)
	}
	f.AvgPageSpaceUsedPct = density.Float64
	return f, nil
}

// XMLIndex is one XML index on a table — sys.xml_indexes. It carries what
// sys.indexes cannot: whether the index is the table's primary XML index or
// a secondary one, which secondary form it is, and which primary index it is
// built over. A secondary XML index can only be created over an existing
// primary one, so a caller offering to create one has to know which primary
// indexes are there.
type XMLIndex struct {
	Name    string
	IndexID int
	// IsPrimary is true for a primary XML index, which is the one built
	// directly on the xml column; a secondary index is built over it.
	IsPrimary bool
	// SecondaryType is PATH, VALUE or PROPERTY for a secondary index, and
	// empty for a primary one.
	SecondaryType XMLSecondaryIndexType
	// ColumnName is the xml column the index is on.
	ColumnName string
	// PrimaryIndexName is the primary XML index a secondary one is built
	// over, and empty for a primary index.
	PrimaryIndexName string
}

// XMLIndexes returns the XML indexes on the table, primary and secondary, in
// name order.
func (t *Table) XMLIndexes(ctx context.Context) ([]*XMLIndex, error) {
	const q = `
SELECT xi.name, xi.index_id, ISNULL(xi.secondary_type_desc, ''), c.name, ISNULL(p.name, '')
FROM   sys.xml_indexes xi
JOIN   sys.index_columns ic ON ic.object_id = xi.object_id AND ic.index_id = xi.index_id
JOIN   sys.columns c ON c.object_id = xi.object_id AND c.column_id = ic.column_id
LEFT   JOIN sys.xml_indexes p ON p.object_id = xi.object_id AND p.index_id = xi.using_xml_index_id
WHERE  xi.object_id = @p1
ORDER  BY xi.name`
	rows, err := t.db.query(ctx, q, t.ObjectID)
	return scanRows(rows, err, fmt.Sprintf("xml indexes on %s", t.FullName()), func(scan func(...any) error) (*XMLIndex, error) {
		x := &XMLIndex{}
		var secondary string
		if err := scan(&x.Name, &x.IndexID, &secondary, &x.ColumnName, &x.PrimaryIndexName); err != nil {
			return nil, err
		}
		x.SecondaryType = XMLSecondaryIndexType(secondary)
		x.IsPrimary = secondary == ""
		return x, nil
	})
}

// IndexFragmentation holds fragmentation statistics for one index.
// AvgPageSpaceUsedPct is only populated when the DMV ran in SAMPLED or
// DETAILED mode (see Index.Fragmentation's mode parameter);
// Table.FragmentationStats's own LIMITED-mode query leaves it zero,
// matching the underlying DMV.
type IndexFragmentation struct {
	IndexName           string
	IndexID             int
	AvgFragmentationPct float64
	PageCount           int64
	FragmentCount       int64
	AvgPageSpaceUsedPct float64
}

// FragmentationMode is sys.dm_db_index_physical_stats's scan mode.
type FragmentationMode string

const (
	FragmentationLimited  FragmentationMode = "LIMITED" // fastest; leaf-level page counts are estimates
	FragmentationSampled  FragmentationMode = "SAMPLED"
	FragmentationDetailed FragmentationMode = "DETAILED"
)

// FragmentationStats returns fragmentation info for all indexes on the table.
// An empty mode is FragmentationLimited.
func (t *Table) FragmentationStats(ctx context.Context, mode FragmentationMode) ([]*IndexFragmentation, error) {
	if mode == "" {
		mode = FragmentationLimited
	}
	// sys.dm_db_index_physical_stats does not accept parameters for the mode string;
	// validate it here to prevent injection.
	switch mode {
	case FragmentationLimited, FragmentationSampled, FragmentationDetailed:
	default:
		return nil, fmt.Errorf("gosmo: fragmentation stats: invalid mode %q (must be LIMITED, SAMPLED, or DETAILED)", mode)
	}

	q := fmt.Sprintf(`
SELECT i.name, s.index_id,
       s.avg_fragmentation_in_percent,
       s.page_count,
       s.fragment_count
FROM   sys.dm_db_index_physical_stats(DB_ID(), OBJECT_ID(N'%s'), NULL, NULL, N'%s') s
JOIN   sys.indexes i ON i.object_id = s.object_id AND i.index_id = s.index_id
WHERE  s.index_id > 0
ORDER  BY s.avg_fragmentation_in_percent DESC`,
		escapeSingle(t.FullName()), mode)

	rows, err := t.db.query(ctx, q)
	return scanRows(rows, err, fmt.Sprintf("fragmentation stats for %s", t.FullName()), func(scan func(...any) error) (*IndexFragmentation, error) {
		f := &IndexFragmentation{}
		if err := scan(&f.IndexName, &f.IndexID,
			&f.AvgFragmentationPct, &f.PageCount, &f.FragmentCount); err != nil {
			return nil, err
		}
		return f, nil
	})
}
