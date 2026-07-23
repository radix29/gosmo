package gosmo

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// -- Index management ----------------------------------------------------------

// Rebuild rebuilds the index (ALTER INDEX ... REBUILD).
// Pass fillFactor=0 to keep the existing fill factor.
func (idx *Index) Rebuild(t *Table, fillFactor int) error {
	return idx.RebuildContext(context.Background(), t, fillFactor)
}

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

func (idx *Index) DisableContext(ctx context.Context, t *Table) error {
	q := fmt.Sprintf("ALTER INDEX %s ON %s DISABLE", quoteIdent(idx.Name), t.FullName())
	if _, err := t.db.exec(ctx, q); err != nil {
		return fmt.Errorf("gosmo: disable index %q: %w", idx.Name, err)
	}
	idx.IsDisabled = true
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
// constraint"), live-verified against a real PK-backed index.
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
	idx.Name = newName
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

	row, release, err := t.db.queryRow(ctx, headerQ, t.ObjectID, idx.IndexID)
	if err != nil {
		return nil, fmt.Errorf("gosmo: storage info for index %q: %w", idx.Name, err)
	}
	info := &IndexStorageInfo{}
	var dsType string
	var fgOrPS string
	err = row.Scan(&fgOrPS, &dsType, &info.PartitionScheme, &info.PartitionColumn,
		&info.RowCount, &info.UsedKB, &info.ReservedKB)
	release()
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
	if avgRow, avgRelease, err := t.db.queryRow(ctx, avgQ, t.ObjectID, idx.IndexID); err == nil {
		var avg sql.NullFloat64
		if avgRow.Scan(&avg) == nil {
			info.AvgRecordSize = avg.Float64
		}
		avgRelease()
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
			return nil, err
		}
		info.Allocations = append(info.Allocations, au)
	}
	if err := rows.Err(); err != nil {
		return nil, err
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
FROM   sys.dm_db_index_physical_stats(DB_ID(), OBJECT_ID(N'%s.%s'), %d, NULL, N'%s') s
JOIN   sys.indexes i ON i.object_id = s.object_id AND i.index_id = s.index_id
WHERE  s.index_level = 0`,
		escapeSingle(t.Schema), escapeSingle(t.Name), idx.IndexID, mode)

	row, release, err := t.db.queryRow(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("gosmo: fragmentation for index %q: %w", idx.Name, err)
	}
	defer release()

	f := &IndexFragmentation{}
	var density sql.NullFloat64
	if err := row.Scan(&f.IndexName, &f.IndexID, &f.AvgFragmentationPct, &f.PageCount, &f.FragmentCount, &density); err != nil {
		return nil, fmt.Errorf("gosmo: fragmentation for index %q: %w", idx.Name, err)
	}
	f.AvgPageSpaceUsedPct = density.Float64
	return f, nil
}

// CreateIndexRequest describes a new index to create.
type CreateIndexRequest struct {
	Name             string
	Type             IndexType
	IsUnique         bool
	KeyColumns       []IndexColumnDef
	IncludedColumns  []string
	FilterDefinition string
	FillFactor       int
	Online           bool
	SortInTempDB     bool
}

// IndexColumnDef describes one key column for a new index.
type IndexColumnDef struct {
	Name       string
	Descending bool
}

// CreateIndex creates a new index on the table.
func (t *Table) CreateIndex(req CreateIndexRequest) error {
	return t.CreateIndexContext(context.Background(), req)
}

func (t *Table) CreateIndexContext(ctx context.Context, req CreateIndexRequest) error {
	if req.Name == "" {
		return fmt.Errorf("gosmo: create index: name is required")
	}
	if len(req.KeyColumns) == 0 {
		return fmt.Errorf("gosmo: create index: at least one key column required")
	}

	var sb strings.Builder
	sb.WriteString("CREATE ")
	if req.IsUnique {
		sb.WriteString("UNIQUE ")
	}
	switch req.Type {
	case IndexTypeClustered:
		sb.WriteString("CLUSTERED ")
	case IndexTypeColumnStore:
		sb.WriteString("NONCLUSTERED COLUMNSTORE ")
	default:
		sb.WriteString("NONCLUSTERED ")
	}
	fmt.Fprintf(&sb, "INDEX %s ON %s (", quoteIdent(req.Name), t.FullName())

	keyCols := make([]string, len(req.KeyColumns))
	for i, c := range req.KeyColumns {
		dir := "ASC"
		if c.Descending {
			dir = "DESC"
		}
		keyCols[i] = fmt.Sprintf("%s %s", quoteIdent(c.Name), dir)
	}
	sb.WriteString(strings.Join(keyCols, ", "))
	sb.WriteString(")")

	if len(req.IncludedColumns) > 0 {
		inc := make([]string, len(req.IncludedColumns))
		for i, c := range req.IncludedColumns {
			inc[i] = quoteIdent(c)
		}
		fmt.Fprintf(&sb, " INCLUDE (%s)", strings.Join(inc, ", "))
	}
	if req.FilterDefinition != "" {
		fmt.Fprintf(&sb, " WHERE %s", req.FilterDefinition)
	}

	var withs []string
	if req.FillFactor > 0 {
		withs = append(withs, fmt.Sprintf("FILLFACTOR = %d", req.FillFactor))
	}
	if req.Online {
		withs = append(withs, "ONLINE = ON")
	}
	if req.SortInTempDB {
		withs = append(withs, "SORT_IN_TEMPDB = ON")
	}
	if len(withs) > 0 {
		fmt.Fprintf(&sb, " WITH (%s)", strings.Join(withs, ", "))
	}

	if _, err := t.db.exec(ctx, sb.String()); err != nil {
		return fmt.Errorf("gosmo: create index %q on %s: %w", req.Name, t.FullName(), err)
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
FROM   sys.dm_db_index_physical_stats(DB_ID(), OBJECT_ID(N'%s.%s'), NULL, NULL, N'%s') s
JOIN   sys.indexes i ON i.object_id = s.object_id AND i.index_id = s.index_id
WHERE  s.index_id > 0
ORDER  BY s.avg_fragmentation_in_percent DESC`,
		escapeSingle(t.Schema), escapeSingle(t.Name), mode)

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
			return nil, err
		}
		results = append(results, f)
	}
	return results, rows.Err()
}
