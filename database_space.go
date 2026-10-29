package gosmo

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// ============================================================
// sp_spaceused figures and log space
// ============================================================

// spaceusedInternalTypes is the internal_type list sp_spaceused excludes from
// "data" and, per object, adds to the parent table's reserved and used pages:
// XML, full-text, semantic, spatial, JSON and vector index internal tables.
// Copied from the procedure's source (OBJECT_DEFINITION, 2026-10-29): 2016 and
// 2017 list 202,204,207,211,212,213,214,215,216,221,222,236; 2025 adds 30, 31
// and 235. The 2025 superset is used on every version — a type an older
// server does not have matches no row there.
const spaceusedInternalTypes = "30,31,202,204,207,211,212,213,214,215,216,221,222,235,236"

// SpaceReport is a database's space summary in sp_spaceused's arithmetic (no
// arguments, the default @mode), so its figures match that procedure's output
// exactly — in KB where it prints KB, and in KB that round to its two-decimal
// MB where it prints MB.
//
// It differs from SpaceUsed and DiskUsage on purpose: those count used pages
// of every allocation unit of a heap or clustered index as data (IAM and
// B-tree upper levels included), where sp_spaceused counts the in-row unit's
// data pages only, and those leave XML and full-text index internal tables
// inside data. The TUI's figures come from those reads; an agent comparing
// with sp_spaceused needs these.
type SpaceReport struct {
	// DataFilesKB and LogFilesKB are sp_spaceused's @dbsize and @logsize:
	// the ROWS and LOG files. They read sys.sysfiles, as the procedure does,
	// which lists only those two file types — a FILESTREAM or full-text
	// container is in neither.
	DataFilesKB int64
	LogFilesKB  int64
	// DataUsedKB and LogUsedKB are the space allocated inside those files
	// (FILEPROPERTY(name, 'SpaceUsed')), which sp_spaceused does not print.
	DataUsedKB int64
	LogUsedKB  int64
	// CheckpointFilesKB is the memory-optimized filegroup's containers
	// (sys.database_files on a data space of type FX), 0 for a database
	// without one. It is part of DatabaseSizeKB from SQL Server 2017, as in
	// sp_spaceused; 2016's procedure leaves it out.
	CheckpointFilesKB int64
	// DatabaseSizeKB is sp_spaceused's database_size: data files, log files
	// and, from 2017, the checkpoint files.
	DatabaseSizeKB int64
	// UnallocatedKB is sp_spaceused's "unallocated space": DataFilesKB less
	// ReservedKB (0 when reserved exceeds it) plus, from 2017, the
	// memory-optimized checkpoint files in state PRECREATED.
	UnallocatedKB int64
	// XTPPrecreatedHidden reports that the PRECREATED checkpoint files could
	// not be read — sys.dm_db_xtp_checkpoint_files needs VIEW DATABASE STATE —
	// so UnallocatedKB leaves them out. Only a database with a
	// memory-optimized filegroup on 2017 or later can set it.
	XTPPrecreatedHidden bool
	// ReservedKB, DataKB, IndexKB and UnusedKB are sp_spaceused's second
	// result set: pages reserved by every allocation unit; in-row data pages
	// of heaps and clustered indexes plus their LOB and row-overflow used
	// pages, internal index tables excluded; the rest of the used pages; and
	// reserved less used.
	ReservedKB int64
	DataKB     int64
	IndexKB    int64
	UnusedKB   int64
}

// SpaceReport returns the database's space summary in sp_spaceused's
// arithmetic, in one round trip — the procedure's own source, read from 2016,
// 2017 and 2025, not a recollection of it.
//
// It runs inside the database, so it needs the database ONLINE and
// accessible. Its page counts come from sys.allocation_units, which metadata
// visibility filters, so a login that sees few objects gets smaller figures —
// as it does from sp_spaceused.
func (d *Database) SpaceReport(ctx context.Context) (SpaceReport, error) {
	q := d.spaceReportQuery()
	var r SpaceReport
	var dataPages, logPages, dataUsed, logUsed, ckpt, reserved, used, pages sql.NullInt64
	var precreated sql.NullInt64
	var precreatedRead bool
	if err := d.queryRow(ctx, func(row *sql.Row) error {
		return row.Scan(&dataPages, &logPages, &dataUsed, &logUsed, &ckpt, &precreated, &precreatedRead,
			&reserved, &used, &pages)
	}, q); err != nil {
		return SpaceReport{}, fmt.Errorf("gosmo: space report for %q: %w", d.Name, err)
	}
	r.DataFilesKB = dataPages.Int64 * 8
	r.LogFilesKB = logPages.Int64 * 8
	r.DataUsedKB = dataUsed.Int64 * 8
	r.LogUsedKB = logUsed.Int64 * 8
	r.CheckpointFilesKB = ckpt.Int64 * 8
	r.ReservedKB = reserved.Int64 * 8
	r.DataKB = pages.Int64 * 8
	r.IndexKB = (used.Int64 - pages.Int64) * 8
	r.UnusedKB = (reserved.Int64 - used.Int64) * 8

	since2017 := d.atLeast(SQLServer2017)
	r.DatabaseSizeKB = r.DataFilesKB + r.LogFilesKB
	if since2017 {
		r.DatabaseSizeKB += r.CheckpointFilesKB
	}
	if dataPages.Int64 >= reserved.Int64 {
		r.UnallocatedKB = (dataPages.Int64 - reserved.Int64) * 8
	}
	if since2017 {
		r.UnallocatedKB += precreated.Int64 / 1024
		r.XTPPrecreatedHidden = ckpt.Valid && !precreatedRead
	}
	return r, nil
}

// spaceReportQuery is SpaceReport's batch for d's server version. 2017's
// sp_spaceused counts pages over sys.system_internals_partitions, 2016's over
// sys.partitions; the checkpoint DMV is read only where the procedure reads
// it, and only with the permission it needs, so a login without VIEW
// DATABASE STATE is not refused the whole report for one memory-optimized
// figure (a DMV's permission is checked when its statement runs).
func (d *Database) spaceReportQuery() string {
	partitions := "sys.partitions"
	precreated := `
DECLARE @precreated bigint, @precreatedRead bit = 0;`
	if d.atLeast(SQLServer2017) {
		partitions = "sys.system_internals_partitions"
		precreated += `
IF EXISTS (SELECT 1 FROM sys.data_spaces WHERE type = N'FX')
   AND (HAS_PERMS_BY_NAME(NULL, 'DATABASE', 'VIEW DATABASE STATE') = 1
        OR HAS_PERMS_BY_NAME(NULL, 'DATABASE', 'VIEW DATABASE PERFORMANCE STATE') = 1)
BEGIN
    SELECT @precreated = SUM(CONVERT(bigint, file_size_in_bytes))
    FROM   sys.dm_db_xtp_checkpoint_files
    WHERE  state_desc = N'PRECREATED';
    SET @precreatedRead = 1;
END;`
	}
	return precreated + `
SELECT f.data_pages, f.log_pages, u.data_used, u.log_used, c.ckpt_pages,
       @precreated, @precreatedRead,
       a.reserved_pages, a.used_pages, a.data_pages
FROM (
    SELECT SUM(CONVERT(bigint, CASE WHEN status & 64 = 0 THEN size ELSE 0 END)) AS data_pages,
           SUM(CONVERT(bigint, CASE WHEN status & 64 <> 0 THEN size ELSE 0 END)) AS log_pages
    FROM   sys.sysfiles
) f
CROSS JOIN (
    SELECT SUM(CASE WHEN type = 0 THEN CAST(FILEPROPERTY(name, 'SpaceUsed') AS bigint) ELSE 0 END) AS data_used,
           SUM(CASE WHEN type = 1 THEN CAST(FILEPROPERTY(name, 'SpaceUsed') AS bigint) ELSE 0 END) AS log_used
    FROM   sys.database_files
) u
CROSS JOIN (
    SELECT SUM(CONVERT(bigint, size)) AS ckpt_pages
    FROM   sys.database_files
    WHERE  data_space_id IN (SELECT data_space_id FROM sys.data_spaces WHERE type = N'FX')
) c
CROSS JOIN (
    SELECT SUM(a.total_pages) AS reserved_pages,
           SUM(a.used_pages)  AS used_pages,
           SUM(CASE
                   WHEN it.internal_type IN (` + spaceusedInternalTypes + `) THEN 0
                   WHEN a.type <> 1 AND p.index_id < 2 THEN a.used_pages
                   WHEN p.index_id < 2 THEN a.data_pages
                   ELSE 0
               END) AS data_pages
    FROM   ` + partitions + ` p
    JOIN   sys.allocation_units a ON p.partition_id = a.container_id
    LEFT   JOIN sys.internal_tables it ON p.object_id = it.object_id
) a`
}

// TableSize is one table's space in sp_spaceused's per-object arithmetic, so
// it matches `EXEC sp_spaceused 'schema.table'` exactly.
//
// It differs from TableSpaceInfo (Table.SpaceUsed, TableSpaceUsedAll) on
// purpose: sp_spaceused counts the base table's LOB and row-overflow pages as
// data and a nonclustered index's as index, counts in-row data pages rather
// than used pages, and adds the table's XML, spatial and full-text index
// internal tables to reserved and used. The TUI's Tables pane shows
// TableSpaceInfo's split.
type TableSize struct {
	ObjectID int
	Schema   string
	Name     string
	// Rows is the heap's or clustered index's row count, summed over
	// partitions — for a memory-optimized table, over index_id 0, 1 and 5,
	// as sp_spaceused counts it.
	Rows int64
	// IsMemoryOptimized marks a memory-optimized table. sp_spaceused prints
	// NULL for its four sizes, so they are 0 here and mean "not reported",
	// never "empty".
	IsMemoryOptimized bool
	ReservedKB        int64
	DataKB            int64
	IndexKB           int64
	UnusedKB          int64
}

// TableSizes returns every table in sys.tables with its space in
// sp_spaceused's per-object arithmetic, in one round trip — the per-table
// procedure costs a call per table. Ordered by schema and name.
//
// sp_spaceused reads sys.dm_db_partition_stats, which needs VIEW DATABASE
// STATE; this reads the same counters from sys.partitions and
// sys.allocation_units, which need only metadata visibility, so a read-only
// login gets the figures for the tables it can see.
func (d *Database) TableSizes(ctx context.Context) ([]*TableSize, error) {
	// alloc maps every allocation unit to the table it counts for: its own
	// object, or the parent of an internal index table of one of the types
	// sp_spaceused adds in. An internal table of any other type keeps its
	// own object_id, which is no table's, and drops out of the join. The
	// "data" counter is the base table's in-row data pages plus its LOB and
	// row-overflow used pages (dm_db_partition_stats' in_row_data_page_count
	// is the in-row unit's data_pages); an internal table contributes none.
	const q = `
WITH alloc AS (
    SELECT COALESCE(it.parent_id, p.object_id) AS object_id,
           a.total_pages, a.used_pages,
           CASE WHEN it.object_id IS NULL AND p.index_id < 2
                THEN CASE WHEN a.type = 1 THEN a.data_pages ELSE a.used_pages END
                ELSE 0 END AS data_pages
    FROM   sys.partitions p
    JOIN   sys.allocation_units a ON a.container_id = p.partition_id
    LEFT   JOIN sys.internal_tables it
           ON it.object_id = p.object_id AND it.internal_type IN (` + spaceusedInternalTypes + `)
)
SELECT t.object_id, SCHEMA_NAME(t.schema_id), t.name, t.is_memory_optimized,
       ISNULL(r.row_count, 0),
       ISNULL(SUM(al.total_pages), 0), ISNULL(SUM(al.used_pages), 0), ISNULL(SUM(al.data_pages), 0)
FROM   sys.tables t
OUTER  APPLY (
    SELECT SUM(p.rows) AS row_count
    FROM   sys.partitions p
    WHERE  p.object_id = t.object_id
      AND  (p.index_id < 2 OR (t.is_memory_optimized = 1 AND p.index_id = 5))
) r
LEFT   JOIN alloc al ON al.object_id = t.object_id
GROUP  BY t.object_id, t.schema_id, t.name, t.is_memory_optimized, r.row_count
ORDER  BY SCHEMA_NAME(t.schema_id), t.name`

	rows, err := d.query(ctx, q)
	return scanRows(rows, err, fmt.Sprintf("table sizes in %q", d.Name), func(scan func(...any) error) (*TableSize, error) {
		ts := &TableSize{}
		var reserved, used, pages int64
		if err := scan(&ts.ObjectID, &ts.Schema, &ts.Name, &ts.IsMemoryOptimized, &ts.Rows,
			&reserved, &used, &pages); err != nil {
			return nil, err
		}
		if !ts.IsMemoryOptimized {
			ts.ReservedKB, ts.DataKB, ts.IndexKB, ts.UnusedKB = spaceusedSplit(reserved, used, pages)
		}
		return ts, nil
	})
}

// spaceusedSplit is sp_spaceused's per-object result from its three page
// counters: index and unused never go negative (the procedure's CASEs).
func spaceusedSplit(reserved, used, pages int64) (reservedKB, dataKB, indexKB, unusedKB int64) {
	return reserved * 8, pages * 8, max(used-pages, 0) * 8, max(reserved-used, 0) * 8
}

// LogSpace is a database's transaction log usage: sys.dm_db_log_space_usage,
// the reason the log cannot be truncated, and its virtual log files.
type LogSpace struct {
	SizeKB int64 // total_log_size_in_bytes / 1024
	UsedKB int64 // used_log_space_in_bytes / 1024, rounded down
	// UsedPercent is the DMV's used_log_space_in_percent as it reports it,
	// not recomputed from the rounded KB.
	UsedPercent float64
	// ReuseWait is sys.databases.log_reuse_wait_desc: NOTHING, CHECKPOINT,
	// LOG_BACKUP, ACTIVE_TRANSACTION, AVAILABILITY_REPLICA, …
	ReuseWait string
	// VLFs and ActiveVLFs count sys.dm_db_log_info rows, all and vlf_active
	// ones. The DMV exists from SQL Server 2016 SP2 (13.0.5026); on an older
	// build VLFsKnown is false and both are 0, which then means "not read".
	VLFs       int
	ActiveVLFs int
	VLFsKnown  bool
}

// logInfoBuild is the SQL Server 2016 build that added sys.dm_db_log_info
// (SP2, 13.0.5026).
const logInfoBuild = 5026

// LogSpace returns the database's log usage, reuse wait and VLF counts in one
// read inside the database (sys.dm_db_log_space_usage answers for the current
// database only). Both DMVs need the *server* permission VIEW SERVER STATE
// (VIEW SERVER PERFORMANCE STATE from 2022) — VIEW DATABASE STATE is not
// enough; a reader with it is still refused Msg 300 (13 and 17, 2026-10-29).
// Without it the read fails, as the DMVs do. A database snapshot has no log,
// and the read fails there too.
func (d *Database) LogSpace(ctx context.Context) (LogSpace, error) {
	vlfs := d.hasLogInfo()
	q := logSpaceQuery(vlfs)
	var ls LogSpace
	var vlfCount, activeCount sql.NullInt64
	if err := d.queryRow(ctx, func(row *sql.Row) error {
		return row.Scan(&ls.SizeKB, &ls.UsedKB, &ls.UsedPercent, &ls.ReuseWait, &vlfCount, &activeCount)
	}, q); err != nil {
		return LogSpace{}, fmt.Errorf("gosmo: log space for %q: %w", d.Name, err)
	}
	if vlfs {
		ls.VLFs, ls.ActiveVLFs, ls.VLFsKnown = int(vlfCount.Int64), int(activeCount.Int64), true
	}
	return ls, nil
}

// hasLogInfo reports whether d's server has sys.dm_db_log_info (2016 SP2 and
// later; an unread version counts as newest).
func (d *Database) hasLogInfo() bool {
	var info *ServerInfo
	if d.server != nil {
		info = d.server.info
	}
	return buildAtLeast(info, SQLServer2016, logInfoBuild)
}

// logSpaceQuery is LogSpace's query, naming sys.dm_db_log_info only when the
// server has it — a reference to a missing DMV fails the whole statement at
// compile, so the gate has to be in the text.
func logSpaceQuery(vlfs bool) string {
	vlf := "CAST(NULL AS bigint), CAST(NULL AS bigint)"
	if vlfs {
		vlf = "v.vlf_count, v.active_count"
	}
	var sb strings.Builder
	sb.WriteString(`
SELECT ls.total_log_size_in_bytes / 1024, ls.used_log_space_in_bytes / 1024,
       CAST(ls.used_log_space_in_percent AS float), d.log_reuse_wait_desc, ` + vlf + `
FROM   sys.dm_db_log_space_usage ls
JOIN   sys.databases d ON d.database_id = ls.database_id`)
	if vlfs {
		sb.WriteString(`
CROSS  APPLY (
    SELECT COUNT_BIG(*) AS vlf_count, SUM(CAST(li.vlf_active AS bigint)) AS active_count
    FROM   sys.dm_db_log_info(DB_ID()) li
) v`)
	}
	return sb.String()
}
