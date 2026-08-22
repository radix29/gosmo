package gosmo

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
)

// -- Index management ----------------------------------------------------------

// Rebuild rebuilds the index (ALTER INDEX ... REBUILD).
// Pass fillFactor=0 to keep the existing fill factor.
func (idx *Index) Rebuild(t *Table, fillFactor int) error {
	return idx.RebuildContext(context.Background(), t, fillFactor)
}

// RebuildContext is the context-aware variant of Rebuild.
func (idx *Index) RebuildContext(ctx context.Context, t *Table, fillFactor int) error {
	q := fmt.Sprintf("ALTER INDEX %s ON %s REBUILD", quoteIdent(idx.Name), t.FullName())
	if fillFactor > 0 {
		q += fmt.Sprintf(" WITH (FILLFACTOR = %d)", fillFactor)
	}
	if _, err := t.db.exec(ctx, q); err != nil {
		return fmt.Errorf("gosmo: rebuild index %q: %w", idx.Name, err)
	}
	return nil
}

// Reorganize reorganizes the index (ALTER INDEX ... REORGANIZE).
func (idx *Index) Reorganize(t *Table) error {
	return idx.ReorganizeContext(context.Background(), t)
}

// ReorganizeContext is the context-aware variant of Reorganize.
func (idx *Index) ReorganizeContext(ctx context.Context, t *Table) error {
	q := fmt.Sprintf("ALTER INDEX %s ON %s REORGANIZE", quoteIdent(idx.Name), t.FullName())
	if _, err := t.db.exec(ctx, q); err != nil {
		return fmt.Errorf("gosmo: reorganize index %q: %w", idx.Name, err)
	}
	return nil
}

// Disable disables the index (ALTER INDEX ... DISABLE).
func (idx *Index) Disable(t *Table) error {
	return idx.DisableContext(context.Background(), t)
}

// DisableContext is the context-aware variant of Disable.
func (idx *Index) DisableContext(ctx context.Context, t *Table) error {
	q := fmt.Sprintf("ALTER INDEX %s ON %s DISABLE", quoteIdent(idx.Name), t.FullName())
	if _, err := t.db.exec(ctx, q); err != nil {
		return fmt.Errorf("gosmo: disable index %q: %w", idx.Name, err)
	}
	setIfApplied(ctx, &idx.IsDisabled, true)
	return nil
}

// Enable re-enables a disabled index by rebuilding it.
func (idx *Index) Enable(t *Table) error {
	return idx.EnableContext(context.Background(), t)
}

// EnableContext is the context-aware variant of Enable.
func (idx *Index) EnableContext(ctx context.Context, t *Table) error {
	return idx.RebuildContext(ctx, t, 0)
}

// Drop drops the index.
func (idx *Index) Drop(t *Table) error {
	return idx.DropContext(context.Background(), t)
}

// DropContext is the context-aware variant of Drop.
func (idx *Index) DropContext(ctx context.Context, t *Table) error {
	q := fmt.Sprintf("DROP INDEX %s ON %s", quoteIdent(idx.Name), t.FullName())
	if _, err := t.db.exec(ctx, q); err != nil {
		return fmt.Errorf("gosmo: drop index %q: %w", idx.Name, err)
	}
	return nil
}

// RebuildAllIndexes rebuilds all indexes on the table (ALTER INDEX ALL ... REBUILD).
func (t *Table) RebuildAllIndexes(fillFactor int) error {
	return t.RebuildAllIndexesContext(context.Background(), fillFactor)
}

// RebuildAllIndexesContext is the context-aware variant of RebuildAllIndexes.
func (t *Table) RebuildAllIndexesContext(ctx context.Context, fillFactor int) error {
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

// SetOptions applies the index's SET-able runtime options (ALTER INDEX ...
// SET). Fill factor, pad index, and data compression only take effect on a
// rebuild — see RebuildWithOptions for those.
func (idx *Index) SetOptions(t *Table, ignoreDupKey, allowRowLocks, allowPageLocks bool) error {
	return idx.SetOptionsContext(context.Background(), t, ignoreDupKey, allowRowLocks, allowPageLocks)
}

// SetOptionsContext is the context-aware variant of SetOptions.
func (idx *Index) SetOptionsContext(ctx context.Context, t *Table, ignoreDupKey, allowRowLocks, allowPageLocks bool) error {
	q := fmt.Sprintf("ALTER INDEX %s ON %s SET (IGNORE_DUP_KEY = %s, ALLOW_ROW_LOCKS = %s, ALLOW_PAGE_LOCKS = %s)",
		quoteIdent(idx.Name), t.FullName(),
		onOffKeyword(ignoreDupKey), onOffKeyword(allowRowLocks), onOffKeyword(allowPageLocks))
	if _, err := t.db.exec(ctx, q); err != nil {
		return fmt.Errorf("gosmo: set options on index %q: %w", idx.Name, err)
	}
	return nil
}

// SetLockOptions applies just the lock-granularity SET options (ALTER INDEX
// ... SET (ALLOW_ROW_LOCKS = .., ALLOW_PAGE_LOCKS = ..)) — unlike SetOptions,
// this never touches IGNORE_DUP_KEY, which SQL Server rejects outright on an
// index backing a PRIMARY KEY or UNIQUE constraint ("Cannot use index option
// ignore_dup_key to alter index '...' as it enforces a primary or unique
// constraint").
func (idx *Index) SetLockOptions(t *Table, allowRowLocks, allowPageLocks bool) error {
	return idx.SetLockOptionsContext(context.Background(), t, allowRowLocks, allowPageLocks)
}

// SetLockOptionsContext is the context-aware variant of SetLockOptions.
func (idx *Index) SetLockOptionsContext(ctx context.Context, t *Table, allowRowLocks, allowPageLocks bool) error {
	q := fmt.Sprintf("ALTER INDEX %s ON %s SET (ALLOW_ROW_LOCKS = %s, ALLOW_PAGE_LOCKS = %s)",
		quoteIdent(idx.Name), t.FullName(), onOffKeyword(allowRowLocks), onOffKeyword(allowPageLocks))
	if _, err := t.db.exec(ctx, q); err != nil {
		return fmt.Errorf("gosmo: set lock options on index %q: %w", idx.Name, err)
	}
	return nil
}

// Rename renames the index using sp_rename — also the mechanism for
// renaming a PRIMARY KEY or UNIQUE constraint, since its name is the
// backing index's name in sys.indexes.
func (idx *Index) Rename(t *Table, newName string) error {
	return idx.RenameContext(context.Background(), t, newName)
}

// RenameContext is the context-aware variant of Rename.
func (idx *Index) RenameContext(ctx context.Context, t *Table, newName string) error {
	objName := t.FullName() + "." + quoteIdent(idx.Name)
	if _, err := t.db.exec(ctx,
		"EXEC sp_rename @objname = @p1, @newname = @p2, @objtype = N'INDEX'",
		objName, newName,
	); err != nil {
		return fmt.Errorf("gosmo: rename index %q to %q: %w", idx.Name, newName, err)
	}
	setIfApplied(ctx, &idx.Name, newName)
	return nil
}

// RebuildWithOptions rebuilds the index with an explicit fill factor, pad
// index setting, and data compression (ALTER INDEX ... REBUILD WITH) — the
// only way to change these three, since none is a plain ALTER INDEX SET
// option. Pass dataCompression="" to leave compression unspecified (keeps
// the index's current setting).
func (idx *Index) RebuildWithOptions(t *Table, fillFactor int, padIndex bool, dataCompression string) error {
	return idx.RebuildWithOptionsContext(context.Background(), t, fillFactor, padIndex, dataCompression)
}

// RebuildWithOptionsContext is the context-aware variant of RebuildWithOptions.
func (idx *Index) RebuildWithOptionsContext(ctx context.Context, t *Table, fillFactor int, padIndex bool, dataCompression string) error {
	switch dataCompression {
	case "", "NONE", "ROW", "PAGE", "COLUMNSTORE", "COLUMNSTORE_ARCHIVE":
	default:
		return fmt.Errorf("gosmo: rebuild index %q with options: invalid data compression %q (must be NONE, ROW, PAGE, COLUMNSTORE, or COLUMNSTORE_ARCHIVE)", idx.Name, dataCompression)
	}

	withParts := []string{fmt.Sprintf("PAD_INDEX = %s", onOffKeyword(padIndex))}
	if fillFactor > 0 {
		withParts = append(withParts, fmt.Sprintf("FILLFACTOR = %d", fillFactor))
	}
	if dataCompression != "" {
		withParts = append(withParts, fmt.Sprintf("DATA_COMPRESSION = %s", dataCompression))
	}
	q := fmt.Sprintf("ALTER INDEX %s ON %s REBUILD WITH (%s)",
		quoteIdent(idx.Name), t.FullName(), strings.Join(withParts, ", "))
	if _, err := t.db.exec(ctx, q); err != nil {
		return fmt.Errorf("gosmo: rebuild index %q with options: %w", idx.Name, err)
	}
	return nil
}

// SetIncludedColumns replaces the index's included (non-key) columns.
// Changing included columns isn't a plain ALTER — it requires recreating the
// index, so this reissues a full CREATE INDEX ... WITH (DROP_EXISTING = ON)
// from idx's own key columns, uniqueness, type, and filter, with columns as
// the new INCLUDE list.
func (idx *Index) SetIncludedColumns(t *Table, columns []string) error {
	return idx.SetIncludedColumnsContext(context.Background(), t, columns)
}

// SetIncludedColumnsContext is the context-aware variant of SetIncludedColumns.
func (idx *Index) SetIncludedColumnsContext(ctx context.Context, t *Table, columns []string) error {
	// A columnstore index has no INCLUDE list, and the CREATE below would
	// recreate it as a rowstore index of whatever clustering IsClustered
	// reports — silently replacing the index with a different kind.
	if idx.Type.IsColumnStore() {
		return fmt.Errorf("gosmo: set included columns on %q: not supported for a %s index",
			idx.Name, idx.Type)
	}
	var sb strings.Builder
	sb.WriteString("CREATE ")
	if idx.IsUnique {
		sb.WriteString("UNIQUE ")
	}
	if idx.IsClustered {
		sb.WriteString("CLUSTERED ")
	} else {
		sb.WriteString("NONCLUSTERED ")
	}
	fmt.Fprintf(&sb, "INDEX %s ON %s (", quoteIdent(idx.Name), t.FullName())

	keyCols := make([]string, len(idx.KeyColumns))
	for i, c := range idx.KeyColumns {
		dir := "ASC"
		if c.Descending {
			dir = "DESC"
		}
		keyCols[i] = fmt.Sprintf("%s %s", quoteIdent(c.Name), dir)
	}
	sb.WriteString(strings.Join(keyCols, ", "))
	sb.WriteString(")")

	if len(columns) > 0 {
		inc := make([]string, len(columns))
		for i, c := range columns {
			inc[i] = quoteIdent(c)
		}
		fmt.Fprintf(&sb, " INCLUDE (%s)", strings.Join(inc, ", "))
	}
	if idx.FilterDefinition != "" {
		fmt.Fprintf(&sb, " WHERE %s", idx.FilterDefinition)
	}
	sb.WriteString(" WITH (DROP_EXISTING = ON)")

	if _, err := t.db.exec(ctx, sb.String()); err != nil {
		return fmt.Errorf("gosmo: set included columns on index %q: %w", idx.Name, err)
	}
	return nil
}

// UpdateStatistics updates the statistics object tied to this index
// (UPDATE STATISTICS table (index) — every index has an implicit
// statistics object with the same name).
func (idx *Index) UpdateStatistics(t *Table) error {
	return idx.UpdateStatisticsContext(context.Background(), t)
}

// UpdateStatisticsContext is the context-aware variant of UpdateStatistics.
func (idx *Index) UpdateStatisticsContext(ctx context.Context, t *Table) error {
	q := fmt.Sprintf("UPDATE STATISTICS %s (%s)", t.FullName(), quoteIdent(idx.Name))
	if _, err := t.db.exec(ctx, q); err != nil {
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
func (idx *Index) StorageInfo(t *Table) (*IndexStorageInfo, error) {
	return idx.StorageInfoContext(context.Background(), t)
}

// StorageInfoContext is the context-aware variant of StorageInfo.
func (idx *Index) StorageInfoContext(ctx context.Context, t *Table) (*IndexStorageInfo, error) {
	const headerQ = `
SELECT
    ds.name, ds.type,
    ISNULL(pf.name, ''),
    ISNULL((SELECT c.name FROM sys.index_columns ic
            JOIN sys.columns c ON c.object_id = ic.object_id AND c.column_id = ic.column_id
            WHERE ic.object_id = i.object_id AND ic.index_id = i.index_id AND ic.partition_ordinal = 1), ''),
    ISNULL(SUM(p.rows), 0),
    ISNULL(SUM(a.used_pages), 0) * 8,
    ISNULL(SUM(a.total_pages), 0) * 8
FROM   sys.indexes i
JOIN   sys.data_spaces ds ON ds.data_space_id = i.data_space_id
LEFT   JOIN sys.partition_schemes ps ON ps.data_space_id = i.data_space_id
LEFT   JOIN sys.partition_functions pf ON pf.function_id = ps.function_id
LEFT   JOIN sys.partitions p ON p.object_id = i.object_id AND p.index_id = i.index_id
LEFT   JOIN sys.allocation_units a ON a.container_id = p.partition_id
WHERE  i.object_id = @p1 AND i.index_id = @p2
GROUP  BY ds.name, ds.type, pf.name, i.object_id, i.index_id`

	info := &IndexStorageInfo{}
	var dsType string
	var fgOrPS string
	err := t.db.queryRow(ctx, func(row *sql.Row) error {
		return row.Scan(&fgOrPS, &dsType, &info.PartitionScheme, &info.PartitionColumn,
			&info.RowCount, &info.UsedKB, &info.ReservedKB)
	}, headerQ, t.ObjectID, idx.IndexID)
	if err != nil {
		return nil, fmt.Errorf("gosmo: storage info for index %q: %w", idx.Name, err)
	}
	if strings.TrimSpace(dsType) == "FG" {
		info.FileGroup = fgOrPS
	} else {
		info.PartitionScheme = fgOrPS
	}

	const avgQ = `
SELECT TOP 1 avg_record_size_in_bytes
FROM   sys.dm_db_index_physical_stats(DB_ID(), @p1, @p2, NULL, 'SAMPLED')
WHERE  index_level = 0`
	var avg sql.NullFloat64
	if err := t.db.queryRow(ctx, func(row *sql.Row) error { return row.Scan(&avg) }, avgQ, t.ObjectID, idx.IndexID); err == nil {
		info.AvgRecordSize = avg.Float64
	}

	const allocQ = `
SELECT a.type_desc, SUM(a.used_pages), SUM(a.used_pages) * 8
FROM   sys.partitions p
JOIN   sys.allocation_units a ON a.container_id = p.partition_id
WHERE  p.object_id = @p1 AND p.index_id = @p2
GROUP  BY a.type_desc
ORDER  BY a.type_desc`
	rows, err := t.db.query(ctx, allocQ, t.ObjectID, idx.IndexID)
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
// by Index Properties' Fragmentation page. mode follows
// Table.FragmentationStats's (LIMITED, SAMPLED, or DETAILED); page density
// is only populated by SAMPLED or DETAILED (LIMITED always reports 0, same
// as the underlying DMV).
func (idx *Index) Fragmentation(t *Table, mode string) (*IndexFragmentation, error) {
	return idx.FragmentationContext(context.Background(), t, mode)
}

// FragmentationContext is the context-aware variant of Fragmentation.
func (idx *Index) FragmentationContext(ctx context.Context, t *Table, mode string) (*IndexFragmentation, error) {
	if mode == "" {
		mode = "LIMITED"
	}
	switch mode {
	case "LIMITED", "SAMPLED", "DETAILED":
	default:
		return nil, fmt.Errorf("gosmo: fragmentation for index %q: invalid mode %q (must be LIMITED, SAMPLED, or DETAILED)", idx.Name, mode)
	}

	q := fmt.Sprintf(`
SELECT i.name, s.index_id,
       s.avg_fragmentation_in_percent,
       s.page_count,
       s.fragment_count,
       s.avg_page_space_used_in_percent
FROM   sys.dm_db_index_physical_stats(DB_ID(), OBJECT_ID(N'%s'), %d, NULL, N'%s') s
JOIN   sys.indexes i ON i.object_id = s.object_id AND i.index_id = s.index_id
WHERE  s.index_level = 0`,
		escapeSingle(t.FullName()), idx.IndexID, mode)

	f := &IndexFragmentation{}
	var density sql.NullFloat64
	if err := t.db.queryRow(ctx, func(row *sql.Row) error {
		return row.Scan(&f.IndexName, &f.IndexID, &f.AvgFragmentationPct, &f.PageCount, &f.FragmentCount, &density)
	}, q); err != nil {
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
func (t *Table) XMLIndexes() ([]*XMLIndex, error) {
	return t.XMLIndexesContext(context.Background())
}

// XMLIndexesContext is the context-aware variant of XMLIndexes.
func (t *Table) XMLIndexesContext(ctx context.Context) ([]*XMLIndex, error) {
	const q = `
SELECT xi.name, xi.index_id, ISNULL(xi.secondary_type_desc, ''), c.name, ISNULL(p.name, '')
FROM   sys.xml_indexes xi
JOIN   sys.index_columns ic ON ic.object_id = xi.object_id AND ic.index_id = xi.index_id
JOIN   sys.columns c ON c.object_id = xi.object_id AND c.column_id = ic.column_id
LEFT   JOIN sys.xml_indexes p ON p.object_id = xi.object_id AND p.index_id = xi.using_xml_index_id
WHERE  xi.object_id = @p1
ORDER  BY xi.name`
	rows, err := t.db.query(ctx, q, t.ObjectID)
	if err != nil {
		return nil, fmt.Errorf("gosmo: xml indexes on %s: %w", t.FullName(), err)
	}
	defer rows.Close()

	var out []*XMLIndex
	for rows.Next() {
		x := &XMLIndex{}
		var secondary string
		if err := rows.Scan(&x.Name, &x.IndexID, &secondary, &x.ColumnName, &x.PrimaryIndexName); err != nil {
			return nil, fmt.Errorf("gosmo: xml indexes on %s: %w", t.FullName(), err)
		}
		x.SecondaryType = XMLSecondaryIndexType(secondary)
		x.IsPrimary = secondary == ""
		out = append(out, x)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("gosmo: xml indexes on %s: %w", t.FullName(), err)
	}
	return out, nil
}

// CreateIndexRequest describes a new index to create. Which fields apply
// depends on Type, and CreateIndex refuses a combination the server would
// reject rather than emitting DDL that fails at the far end:
//
//   - rowstore (CLUSTERED, NONCLUSTERED, and the zero value): key columns,
//     IsUnique, IncludedColumns (nonclustered only), FilterDefinition
//     (nonclustered only), FillFactor/PadIndex, DataCompression NONE/ROW/PAGE.
//   - COLUMNSTORE: key columns and FilterDefinition (the filtered NCCI form),
//     DataCompression COLUMNSTORE/COLUMNSTORE_ARCHIVE, CompressionDelay.
//   - CLUSTERED COLUMNSTORE: no key columns at all — the index covers every
//     column of the table.
//   - XML: one key column (the xml one) plus IsPrimaryXML, or
//     PrimaryXMLIndex and SecondaryXMLType for a secondary index.
//   - SPATIAL: one key column (the geometry/geography one) plus Tessellation,
//     and BoundingBox for the two GEOMETRY_ schemes.
type CreateIndexRequest struct {
	Name       string
	Type       IndexType
	IsUnique   bool
	KeyColumns []IndexColumnDef
	// IncludedColumns are the non-key columns of a nonclustered rowstore
	// index (INCLUDE).
	IncludedColumns []string
	// FilterDefinition is a filtered index's predicate, without the WHERE.
	FilterDefinition string
	FillFactor       int
	PadIndex         bool
	Online           bool
	SortInTempDB     bool
	// DropExisting recreates an index of the same name in place
	// (DROP_EXISTING = ON) instead of failing on the collision.
	DropExisting bool
	// DataCompression is the compression keyword — NONE, ROW or PAGE for a
	// rowstore index, COLUMNSTORE or COLUMNSTORE_ARCHIVE for a columnstore
	// one. Empty leaves it unspecified.
	DataCompression string
	// CompressionDelay is a columnstore index's COMPRESSION_DELAY, in
	// minutes. Zero leaves it unspecified.
	CompressionDelay int
	// FileGroup is the filegroup the index is created on, and
	// PartitionScheme/PartitionColumns the partition scheme it is partitioned
	// by. The two are alternatives — an index has one ON clause.
	FileGroup        string
	PartitionScheme  string
	PartitionColumns []string
	// IsPrimaryXML selects the primary XML index form; PrimaryXMLIndex and
	// SecondaryXMLType describe a secondary one, which is built over the
	// primary index named here.
	IsPrimaryXML     bool
	PrimaryXMLIndex  string
	SecondaryXMLType XMLSecondaryIndexType
	// Tessellation is a spatial index's tessellation scheme (USING).
	Tessellation SpatialTessellation
	// BoundingBox bounds a geometry index's tessellation. Required for the
	// two GEOMETRY_ schemes and rejected for the GEOGRAPHY_ ones, which
	// tessellate the whole globe.
	BoundingBox *SpatialBoundingBox
	// GridLevels is the per-level grid density (GRIDS), which only the two
	// non-automatic schemes accept.
	GridLevels SpatialGridLevels
	// CellsPerObject is the tessellation cell budget per object
	// (CELLS_PER_OBJECT), 1-8192. Zero leaves it unspecified.
	CellsPerObject int
}

// IndexColumnDef describes one key column for a new index.
type IndexColumnDef struct {
	Name       string
	Descending bool
}

// SpatialBoundingBox is the rectangle a geometry index tessellates
// (BOUNDING_BOX). Anything outside it lands in the single top-level cell,
// so it belongs around the data, not around the coordinate system.
type SpatialBoundingBox struct {
	XMin, YMin, XMax, YMax float64
}

// SpatialGridLevels is a spatial index's per-level grid density (GRIDS).
// A level left empty is omitted from the clause and takes the server's
// default, so the zero value means "no GRIDS clause at all".
type SpatialGridLevels struct {
	Level1, Level2, Level3, Level4 SpatialGridDensity
}

// levels renders the GRIDS clause body, or "" when no level is set.
func (g SpatialGridLevels) levels() string {
	var parts []string
	for i, d := range []SpatialGridDensity{g.Level1, g.Level2, g.Level3, g.Level4} {
		if d != "" {
			parts = append(parts, fmt.Sprintf("LEVEL_%d = %s", i+1, d))
		}
	}
	return strings.Join(parts, ", ")
}

// CreateIndex creates a new index on the table.
func (t *Table) CreateIndex(req CreateIndexRequest) error {
	return t.CreateIndexContext(context.Background(), req)
}

// CreateIndexContext is the context-aware variant of CreateIndex.
func (t *Table) CreateIndexContext(ctx context.Context, req CreateIndexRequest) error {
	stmt, err := buildCreateIndexStatement(t.FullName(), req)
	if err != nil {
		return err
	}
	if _, err := t.db.exec(ctx, stmt); err != nil {
		return fmt.Errorf("gosmo: create index %q on %s: %w", req.Name, t.FullName(), err)
	}
	return nil
}

// buildCreateIndexStatement renders one CREATE INDEX statement, or reports
// why the request cannot be one. Separated from CreateIndexContext so the
// statement each index type produces can be pinned without a server.
func buildCreateIndexStatement(tableName string, req CreateIndexRequest) (string, error) {
	if err := req.validate(); err != nil {
		return "", err
	}

	var sb strings.Builder
	name := quoteIdent(req.Name)
	switch req.Type {
	case IndexTypeClusteredColumnStore:
		fmt.Fprintf(&sb, "CREATE CLUSTERED COLUMNSTORE INDEX %s ON %s", name, tableName)
	case IndexTypeColumnStore:
		fmt.Fprintf(&sb, "CREATE NONCLUSTERED COLUMNSTORE INDEX %s ON %s (%s)",
			name, tableName, createIndexColumnList(req.KeyColumns, false))
	case IndexTypeXML:
		if req.IsPrimaryXML {
			fmt.Fprintf(&sb, "CREATE PRIMARY XML INDEX %s ON %s (%s)",
				name, tableName, createIndexColumnList(req.KeyColumns, false))
		} else {
			fmt.Fprintf(&sb, "CREATE XML INDEX %s ON %s (%s) USING XML INDEX %s FOR %s",
				name, tableName, createIndexColumnList(req.KeyColumns, false),
				quoteIdent(req.PrimaryXMLIndex), req.SecondaryXMLType)
		}
	case IndexTypeSpatial:
		fmt.Fprintf(&sb, "CREATE SPATIAL INDEX %s ON %s (%s) USING %s",
			name, tableName, createIndexColumnList(req.KeyColumns, false), req.Tessellation)
	default:
		sb.WriteString("CREATE ")
		if req.IsUnique {
			sb.WriteString("UNIQUE ")
		}
		if req.Type == IndexTypeClustered {
			sb.WriteString("CLUSTERED ")
		} else {
			sb.WriteString("NONCLUSTERED ")
		}
		fmt.Fprintf(&sb, "INDEX %s ON %s (%s)", name, tableName, createIndexColumnList(req.KeyColumns, true))
		if len(req.IncludedColumns) > 0 {
			inc := make([]string, len(req.IncludedColumns))
			for i, c := range req.IncludedColumns {
				inc[i] = quoteIdent(c)
			}
			fmt.Fprintf(&sb, " INCLUDE (%s)", strings.Join(inc, ", "))
		}
	}

	if req.FilterDefinition != "" {
		fmt.Fprintf(&sb, " WHERE %s", req.FilterDefinition)
	}
	if withs := req.withOptions(); len(withs) > 0 {
		fmt.Fprintf(&sb, " WITH (%s)", strings.Join(withs, ", "))
	}
	switch {
	case req.PartitionScheme != "":
		cols := make([]string, len(req.PartitionColumns))
		for i, c := range req.PartitionColumns {
			cols[i] = quoteIdent(c)
		}
		fmt.Fprintf(&sb, " ON %s (%s)", quoteIdent(req.PartitionScheme), strings.Join(cols, ", "))
	case req.FileGroup != "":
		fmt.Fprintf(&sb, " ON %s", quoteIdent(req.FileGroup))
	}
	return sb.String(), nil
}

// withOptions is the WITH clause's contents, in the order CREATE INDEX
// documents them: the spatial tessellation options first, then the ones
// every index form shares.
func (req CreateIndexRequest) withOptions() []string {
	var withs []string
	if req.Type == IndexTypeSpatial {
		if b := req.BoundingBox; b != nil {
			withs = append(withs, fmt.Sprintf("BOUNDING_BOX = (%s, %s, %s, %s)",
				floatLiteral(b.XMin), floatLiteral(b.YMin), floatLiteral(b.XMax), floatLiteral(b.YMax)))
		}
		if g := req.GridLevels.levels(); g != "" {
			withs = append(withs, fmt.Sprintf("GRIDS = (%s)", g))
		}
		if req.CellsPerObject > 0 {
			withs = append(withs, fmt.Sprintf("CELLS_PER_OBJECT = %d", req.CellsPerObject))
		}
	}
	if req.PadIndex {
		withs = append(withs, "PAD_INDEX = ON")
	}
	if req.FillFactor > 0 {
		withs = append(withs, fmt.Sprintf("FILLFACTOR = %d", req.FillFactor))
	}
	if req.Online {
		withs = append(withs, "ONLINE = ON")
	}
	if req.SortInTempDB {
		withs = append(withs, "SORT_IN_TEMPDB = ON")
	}
	if req.DropExisting {
		withs = append(withs, "DROP_EXISTING = ON")
	}
	if req.DataCompression != "" {
		withs = append(withs, fmt.Sprintf("DATA_COMPRESSION = %s", req.DataCompression))
	}
	if req.CompressionDelay > 0 {
		withs = append(withs, fmt.Sprintf("COMPRESSION_DELAY = %d MINUTES", req.CompressionDelay))
	}
	return withs
}

// createIndexColumnList renders a key column list. Only a rowstore index
// orders its key columns: ASC/DESC on a columnstore, XML or spatial column
// list is a syntax error, which is why the direction is the caller's choice
// rather than the column's.
func createIndexColumnList(cols []IndexColumnDef, withDirection bool) string {
	out := make([]string, len(cols))
	for i, c := range cols {
		out[i] = quoteIdent(c.Name)
		if withDirection {
			dir := "ASC"
			if c.Descending {
				dir = "DESC"
			}
			out[i] += " " + dir
		}
	}
	return strings.Join(out, ", ")
}

// floatLiteral renders a bounding-box coordinate without an exponent or a
// trailing ".0" — BOUNDING_BOX takes a plain numeric literal.
func floatLiteral(v float64) string {
	return strconv.FormatFloat(v, 'f', -1, 64)
}

// validate reports why req cannot become a CREATE INDEX statement. Each
// check is a combination SQL Server itself rejects; refusing here means the
// caller gets a message naming the field rather than a parse error naming a
// column number.
func (req CreateIndexRequest) validate() error {
	fail := func(format string, args ...any) error {
		return fmt.Errorf("gosmo: create index %q: %s", req.Name, fmt.Sprintf(format, args...))
	}
	if req.Name == "" {
		return fmt.Errorf("gosmo: create index: name is required")
	}

	rowstore := req.Type == "" || req.Type == IndexTypeClustered || req.Type == IndexTypeNonClustered
	nonclustered := req.Type == "" || req.Type == IndexTypeNonClustered
	switch req.Type {
	case "", IndexTypeClustered, IndexTypeNonClustered,
		IndexTypeColumnStore, IndexTypeClusteredColumnStore, IndexTypeXML, IndexTypeSpatial:
	default:
		return fail("index type %q cannot be created here", req.Type)
	}

	// CREATE CLUSTERED COLUMNSTORE INDEX covers every column of the table and
	// takes no column list at all; everything else names at least one.
	if req.Type == IndexTypeClusteredColumnStore {
		if len(req.KeyColumns) > 0 {
			return fail("a clustered columnstore index takes no key columns — it covers every column of the table")
		}
	} else if len(req.KeyColumns) == 0 {
		return fail("at least one key column required")
	}
	if req.Type == IndexTypeXML || req.Type == IndexTypeSpatial {
		if len(req.KeyColumns) != 1 {
			return fail("%s takes exactly one key column", strings.ToLower(string(req.Type))+" index")
		}
	}
	if !rowstore {
		for _, c := range req.KeyColumns {
			if c.Descending {
				return fail("only a rowstore index orders its key columns")
			}
		}
	}
	for _, c := range req.KeyColumns {
		if c.Name == "" {
			return fail("a key column has no name")
		}
	}

	if req.IsUnique && !rowstore {
		return fail("only a rowstore index can be unique")
	}
	if len(req.IncludedColumns) > 0 && !nonclustered {
		return fail("only a nonclustered rowstore index has included columns")
	}
	if req.FilterDefinition != "" && !nonclustered && req.Type != IndexTypeColumnStore {
		return fail("only a nonclustered rowstore or columnstore index can be filtered")
	}
	if req.FillFactor < 0 || req.FillFactor > 100 {
		return fail("fill factor %d out of range (0-100)", req.FillFactor)
	}
	if (req.FillFactor > 0 || req.PadIndex) && req.Type.IsColumnStore() {
		return fail("a columnstore index has no fill factor")
	}
	if req.SortInTempDB && req.Type.IsColumnStore() {
		return fail("a columnstore index cannot sort in tempdb")
	}
	if err := req.validateCompression(fail); err != nil {
		return err
	}
	if err := req.validateXML(fail); err != nil {
		return err
	}
	if err := req.validateSpatial(fail); err != nil {
		return err
	}

	if req.PartitionScheme != "" && req.FileGroup != "" {
		return fail("an index is created on a filegroup or on a partition scheme, not both")
	}
	if (req.PartitionScheme != "") != (len(req.PartitionColumns) > 0) {
		return fail("a partition scheme and its partitioning column go together")
	}
	if (req.PartitionScheme != "" || req.FileGroup != "") && req.Type == IndexTypeXML {
		return fail("an XML index is stored with the table it indexes and takes no filegroup")
	}
	return nil
}

// validateCompression checks the two compression options against the index
// family that accepts them — the rowstore and columnstore keywords are not
// interchangeable.
func (req CreateIndexRequest) validateCompression(fail func(string, ...any) error) error {
	switch req.DataCompression {
	case "":
	case "NONE", "ROW", "PAGE":
		if req.Type.IsColumnStore() {
			return fail("data compression %s is a rowstore setting", req.DataCompression)
		}
	case "COLUMNSTORE", "COLUMNSTORE_ARCHIVE":
		if !req.Type.IsColumnStore() {
			return fail("data compression %s applies to a columnstore index only", req.DataCompression)
		}
	default:
		return fail("invalid data compression %q (must be NONE, ROW, PAGE, COLUMNSTORE, or COLUMNSTORE_ARCHIVE)", req.DataCompression)
	}
	if req.CompressionDelay != 0 {
		if !req.Type.IsColumnStore() {
			return fail("compression delay applies to a columnstore index only")
		}
		if req.CompressionDelay < 0 {
			return fail("compression delay %d is negative", req.CompressionDelay)
		}
	}
	return nil
}

// validateXML checks the three XML fields, which are meaningless off an XML
// index and, on one, describe either the primary form or the secondary form
// but never both.
func (req CreateIndexRequest) validateXML(fail func(string, ...any) error) error {
	if req.Type != IndexTypeXML {
		if req.IsPrimaryXML || req.PrimaryXMLIndex != "" || req.SecondaryXMLType != "" {
			return fail("the XML index options apply to an XML index only")
		}
		return nil
	}
	if req.IsPrimaryXML {
		if req.PrimaryXMLIndex != "" || req.SecondaryXMLType != "" {
			return fail("a primary XML index is not built over another index")
		}
		return nil
	}
	if req.PrimaryXMLIndex == "" {
		return fail("a secondary XML index names the primary XML index it is built over")
	}
	switch req.SecondaryXMLType {
	case XMLSecondaryPath, XMLSecondaryValue, XMLSecondaryProperty:
	default:
		return fail("secondary XML index type %q is not PATH, VALUE, or PROPERTY", req.SecondaryXMLType)
	}
	return nil
}

// validateSpatial checks the tessellation options. BOUNDING_BOX is the one
// that is required rather than merely allowed: a geometry index without one
// is rejected by the server, since there is nothing to tessellate.
func (req CreateIndexRequest) validateSpatial(fail func(string, ...any) error) error {
	if req.Type != IndexTypeSpatial {
		if req.Tessellation != "" || req.BoundingBox != nil || req.GridLevels.levels() != "" || req.CellsPerObject != 0 {
			return fail("the spatial index options apply to a spatial index only")
		}
		return nil
	}
	switch req.Tessellation {
	case SpatialGeometryGrid, SpatialGeometryAutoGrid, SpatialGeographyGrid, SpatialGeographyAutoGrid:
	default:
		return fail("tessellation scheme %q is not one of GEOMETRY_GRID, GEOMETRY_AUTO_GRID, GEOGRAPHY_GRID, GEOGRAPHY_AUTO_GRID", req.Tessellation)
	}
	if req.Tessellation.IsGeometry() && req.BoundingBox == nil {
		return fail("a %s index requires a bounding box", req.Tessellation)
	}
	if !req.Tessellation.IsGeometry() && req.BoundingBox != nil {
		return fail("a %s index tessellates the whole globe and takes no bounding box", req.Tessellation)
	}
	if b := req.BoundingBox; b != nil && (b.XMax <= b.XMin || b.YMax <= b.YMin) {
		return fail("bounding box is empty — xmax and ymax must exceed xmin and ymin")
	}
	if req.GridLevels.levels() != "" && req.Tessellation.IsAutoGrid() {
		return fail("a %s index chooses its own grid densities", req.Tessellation)
	}
	for _, d := range []SpatialGridDensity{req.GridLevels.Level1, req.GridLevels.Level2, req.GridLevels.Level3, req.GridLevels.Level4} {
		switch d {
		case "", SpatialGridLow, SpatialGridMedium, SpatialGridHigh:
		default:
			return fail("grid density %q is not LOW, MEDIUM, or HIGH", d)
		}
	}
	if req.CellsPerObject < 0 || req.CellsPerObject > 8192 {
		return fail("cells per object %d out of range (1-8192)", req.CellsPerObject)
	}
	return nil
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

// FragmentationStats returns fragmentation info for all indexes on the table.
// mode must be one of "LIMITED" (fast, default), "SAMPLED", or "DETAILED".
func (t *Table) FragmentationStats(mode string) ([]*IndexFragmentation, error) {
	return t.FragmentationStatsContext(context.Background(), mode)
}

// FragmentationStatsContext is the context-aware variant of FragmentationStats.
func (t *Table) FragmentationStatsContext(ctx context.Context, mode string) ([]*IndexFragmentation, error) {
	if mode == "" {
		mode = "LIMITED"
	}
	// sys.dm_db_index_physical_stats does not accept parameters for the mode string;
	// validate it here to prevent injection.
	switch mode {
	case "LIMITED", "SAMPLED", "DETAILED":
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
	if err != nil {
		return nil, fmt.Errorf("gosmo: fragmentation stats for %s: %w", t.FullName(), err)
	}
	defer rows.Close()

	var results []*IndexFragmentation
	for rows.Next() {
		f := &IndexFragmentation{}
		if err := rows.Scan(&f.IndexName, &f.IndexID,
			&f.AvgFragmentationPct, &f.PageCount, &f.FragmentCount); err != nil {
			return nil, fmt.Errorf("gosmo: fragmentation stats for %s: %w", t.FullName(), err)
		}
		results = append(results, f)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("gosmo: fragmentation stats for %s: %w", t.FullName(), err)
	}
	return results, nil
}
