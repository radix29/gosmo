package gosmo

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// -- Index statistics and usage (reads) ----------------------------------------

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
	const target = "i.object_id = OBJECT_ID(@p2) AND i.name = @p1"
	table := idx.on().FullName()
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

	db := idx.on().db
	info := &IndexStorageInfo{}
	var dsType string
	var fgOrPS string
	err := db.queryRow(ctx, func(row *sql.Row) error {
		return row.Scan(&fgOrPS, &dsType, &info.PartitionScheme, &info.PartitionColumn,
			&info.RowCount, &info.UsedKB, &info.ReservedKB)
	}, headerQ, idx.Name, table)
	if err != nil {
		return nil, fmt.Errorf("gosmo: storage info for index %q: %w", idx.Name, err)
	}
	if strings.TrimSpace(dsType) == "FG" {
		info.FileGroup = fgOrPS
	} else {
		info.PartitionScheme = fgOrPS
	}

	// The DMV returns one row per partition and allocation unit; the record
	// size is the leaf's in-row one, averaged over partitions by record
	// count. TOP 1 without the filter picked an arbitrary row — on a table
	// with (max) columns often the LOB unit's, ~100x the real figure.
	avgQ := `
SELECT SUM(s.avg_record_size_in_bytes * s.record_count) / NULLIF(SUM(s.record_count), 0)
FROM   sys.indexes i
CROSS  APPLY sys.dm_db_index_physical_stats(DB_ID(), i.object_id, i.index_id, NULL, 'SAMPLED') s
WHERE  ` + target + ` AND s.index_level = 0 AND s.alloc_unit_type_desc = N'IN_ROW_DATA'`
	var avg sql.NullFloat64
	if err := db.queryRow(ctx, func(row *sql.Row) error { return row.Scan(&avg) }, avgQ, idx.Name, table); err == nil {
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
	rows, err := db.query(ctx, allocQ, idx.Name, table)
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
// A partitioned index reports one row for all its partitions; see
// fragmentationSelect for how they are combined, and why the DMV is reached
// through sys.indexes.
func (idx *Index) Fragmentation(ctx context.Context, mode FragmentationMode) (*IndexFragmentation, error) {
	mode, err := mode.normalize()
	if err != nil {
		return nil, fmt.Errorf("gosmo: fragmentation for index %q: %w", idx.Name, err)
	}

	q := fragmentationSelect(mode) + `
WHERE  i.object_id = OBJECT_ID(@p2) AND i.name = @p1
GROUP  BY i.name, i.index_id`

	var f *IndexFragmentation
	if err := idx.on().db.queryRow(ctx, func(row *sql.Row) error {
		var err error
		f, err = scanFragmentation(row.Scan)
		return err
	}, q, idx.Name, idx.on().FullName()); err != nil {
		return nil, fmt.Errorf("gosmo: fragmentation for index %q: %w", idx.Name, err)
	}
	return f, nil
}

// fragmentationSelect is the SELECT ... FROM shared by Index.Fragmentation
// and Table.FragmentationStats, for the caller to finish with a WHERE on
// sys.indexes i and GROUP BY i.name, i.index_id. mode must already have been
// through FragmentationMode.normalize — the DMV takes no parameter for it.
//
// The DMV is applied to the indexes sys.indexes finds rather than called
// with OBJECT_ID directly: a name that resolves to nothing then yields no
// row, where a NULL object_id passed to the DMV means every object in the
// database (quoting.go names the hazard).
//
// The DMV returns one row per partition, allocation unit and (DETAILED)
// level. Only the leaf's IN_ROW_DATA rows describe the index's own pages —
// a LOB_DATA row reports 0% fragmentation for a table with (max) columns —
// so those are kept, and the partitions are folded into one row: page and
// fragment counts summed, the percentages weighted by page count. It is an
// OUTER APPLY so an index with no such row (a columnstore with no delta
// store) still reports, as zeros. fragment_count is NULL in SAMPLED mode for
// a heap and avg_page_space_used_in_percent in LIMITED mode, hence ISNULL.
func fragmentationSelect(mode FragmentationMode) string {
	return fmt.Sprintf(`
SELECT i.name, i.index_id,
       ISNULL(SUM(s.avg_fragmentation_in_percent * s.page_count) / NULLIF(SUM(s.page_count), 0), 0),
       ISNULL(SUM(s.page_count), 0),
       ISNULL(SUM(s.fragment_count), 0),
       ISNULL(SUM(s.avg_page_space_used_in_percent * s.page_count) / NULLIF(SUM(s.page_count), 0), 0)
FROM   sys.indexes i
OUTER  APPLY (SELECT d.avg_fragmentation_in_percent, d.page_count, d.fragment_count, d.avg_page_space_used_in_percent
              FROM   sys.dm_db_index_physical_stats(DB_ID(), i.object_id, i.index_id, NULL, N'%s') d
              WHERE  d.index_level = 0 AND d.alloc_unit_type_desc = N'IN_ROW_DATA') s`, mode)
}

func scanFragmentation(scan func(...any) error) (*IndexFragmentation, error) {
	f := &IndexFragmentation{}
	if err := scan(&f.IndexName, &f.IndexID, &f.AvgFragmentationPct, &f.PageCount, &f.FragmentCount, &f.AvgPageSpaceUsedPct); err != nil {
		return nil, err
	}
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
	// IsSelective is true for a selective XML index and for a secondary
	// one built over it (PrimaryIndexName then names the selective index).
	// Neither is a primary XML index, and a PATH, VALUE or PROPERTY
	// secondary index cannot be built over either.
	IsSelective bool
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
	if err := t.requireLoaded("xml indexes on"); err != nil {
		return nil, err
	}
	const q = `
SELECT xi.name, xi.index_id, ISNULL(xi.secondary_type_desc, ''), c.name, ISNULL(p.name, ''),
       CAST(CASE WHEN xi.xml_index_type = 0 THEN 1 ELSE 0 END AS bit),
       CAST(CASE WHEN xi.xml_index_type IN (2, 3) THEN 1 ELSE 0 END AS bit)
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
		if err := scan(&x.Name, &x.IndexID, &secondary, &x.ColumnName, &x.PrimaryIndexName,
			&x.IsPrimary, &x.IsSelective); err != nil {
			return nil, err
		}
		x.SecondaryType = XMLSecondaryIndexType(secondary)
		return x, nil
	})
}

// IndexFragmentation holds fragmentation statistics for one index, its
// partitions combined (see fragmentationSelect). AvgPageSpaceUsedPct is only
// populated in SAMPLED or DETAILED mode; LIMITED leaves it zero, as the
// underlying DMV does.
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

// normalize returns m with the empty mode defaulted to FragmentationLimited,
// or an error for any other value outside the three constants.
// sys.dm_db_index_physical_stats takes no parameter for the mode, so
// fragmentationSelect formats it into the query: this check is what keeps
// that safe.
func (m FragmentationMode) normalize() (FragmentationMode, error) {
	switch m {
	case "":
		return FragmentationLimited, nil
	case FragmentationLimited, FragmentationSampled, FragmentationDetailed:
		return m, nil
	}
	return "", invalidf("invalid mode %q (must be LIMITED, SAMPLED, or DETAILED)", m)
}

// FragmentationStats returns fragmentation info for all indexes on the table,
// one row per index, most fragmented first. An empty mode is
// FragmentationLimited. A name that resolves to no table returns no rows.
//
// A disabled index is left out, as Database.FragmentationStats leaves it out:
// it has no data, and the DMV called with its index_id refuses the whole
// statement (Msg 2530, "The index … is disabled"), so one disabled index
// failed the read of every other (found by gossms maint index-rebuild,
// 2026-11-03).
func (t *Table) FragmentationStats(ctx context.Context, mode FragmentationMode) ([]*IndexFragmentation, error) {
	mode, err := mode.normalize()
	if err != nil {
		return nil, fmt.Errorf("gosmo: fragmentation stats: %w", err)
	}

	q := fragmentationSelect(mode) + `
WHERE  i.object_id = OBJECT_ID(@p1) AND i.index_id > 0 AND i.is_hypothetical = 0 AND i.is_disabled = 0
GROUP  BY i.name, i.index_id
ORDER  BY 3 DESC, i.name`

	rows, err := t.db.query(ctx, q, t.FullName())
	return scanRows(rows, err, fmt.Sprintf("fragmentation stats for %s", t.FullName()), scanFragmentation)
}

// IndexUsage is one index's row of sys.dm_db_index_usage_stats: how often
// user queries have read and maintained it since the server started (or the
// database came online). An index no query has touched since then has zero
// counts and zero times.
type IndexUsage struct {
	IndexName string // "" for a heap (IndexID 0)
	IndexID   int
	// UsageVisible is false when the login may not read the DMV (it needs
	// VIEW SERVER STATE, or VIEW SERVER PERFORMANCE STATE on 2022 and later):
	// the counts are then zero and the times zero because they were not read,
	// not because the index is unused.
	UsageVisible bool
	UserSeeks    int64
	UserScans    int64
	UserLookups  int64
	UserUpdates  int64
	// The last user read or write of each kind; zero when there was none (or
	// UsageVisible is false).
	LastUserSeek   time.Time
	LastUserScan   time.Time
	LastUserLookup time.Time
	LastUserUpdate time.Time
}

// IndexUsage returns the usage counters of every index on the table, the heap
// included, in index_id order. Like FragmentationStats it names the table, so
// a TableRef is enough, and a name that resolves to no table returns no rows.
//
// The DMV is refused outright without the permission (Msg 300), which would
// cost the index list too, so the batch asks first and reads every index with
// UsageVisible false instead — Server.DatabaseFiles' pattern.
func (t *Table) IndexUsage(ctx context.Context) ([]*IndexUsage, error) {
	const cols = `
SELECT ISNULL(i.name, N''), i.index_id, `
	const from = `
FROM   sys.indexes i`
	const where = `
WHERE  i.object_id = OBJECT_ID(@p1) AND i.is_hypothetical = 0
ORDER  BY i.index_id`
	const q = `
IF HAS_PERMS_BY_NAME(NULL, NULL, 'VIEW SERVER STATE') = 1
   OR HAS_PERMS_BY_NAME(NULL, NULL, 'VIEW SERVER PERFORMANCE STATE') = 1` + cols + `CAST(1 AS bit),
       ISNULL(u.user_seeks, 0), ISNULL(u.user_scans, 0), ISNULL(u.user_lookups, 0), ISNULL(u.user_updates, 0),
       u.last_user_seek, u.last_user_scan, u.last_user_lookup, u.last_user_update` + from + `
LEFT   JOIN sys.dm_db_index_usage_stats u
       ON  u.database_id = DB_ID() AND u.object_id = i.object_id AND u.index_id = i.index_id` + where + `
ELSE` + cols + `CAST(0 AS bit),
       CAST(0 AS bigint), CAST(0 AS bigint), CAST(0 AS bigint), CAST(0 AS bigint),
       CAST(NULL AS datetime), CAST(NULL AS datetime), CAST(NULL AS datetime), CAST(NULL AS datetime)` + from + where

	rows, err := t.db.query(ctx, q, t.FullName())
	return scanRows(rows, err, fmt.Sprintf("index usage for %s", t.FullName()), func(scan func(...any) error) (*IndexUsage, error) {
		u := &IndexUsage{}
		var seek, scanned, lookup, update sql.NullTime
		if err := scan(&u.IndexName, &u.IndexID, &u.UsageVisible,
			&u.UserSeeks, &u.UserScans, &u.UserLookups, &u.UserUpdates,
			&seek, &scanned, &lookup, &update); err != nil {
			return nil, err
		}
		u.LastUserSeek, u.LastUserScan = seek.Time, scanned.Time
		u.LastUserLookup, u.LastUserUpdate = lookup.Time, update.Time
		return u, nil
	})
}
