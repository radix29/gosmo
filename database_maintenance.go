package gosmo

// database_maintenance.go is the whole-database maintenance a DBA runs on a
// schedule: DBCC CHECKDB, fragmentation across every index, and
// sp_updatestats. The per-table and per-index forms are in
// index_management.go and statistics.go.

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// ============================================================
// DBCC CHECKDB
// ============================================================

// CheckDBOptions shapes Database.CheckDB's DBCC CHECKDB. The zero value is a
// full logical and physical check. There is deliberately no REPAIR_* option:
// a repair needs the database in SINGLE_USER and REPAIR_ALLOW_DATA_LOSS
// deletes what it cannot fix — a decision for a person, not a library default.
type CheckDBOptions struct {
	// PhysicalOnly checks page and record structure and allocation only
	// (PHYSICAL_ONLY): much faster on a large database, but it skips the
	// logical checks. Neither DataPurity nor ExtendedLogicalChecks goes with
	// it.
	PhysicalOnly bool
	// NoIndex skips the nonclustered indexes of user tables (NOINDEX).
	NoIndex bool
	// ExtendedLogicalChecks adds the logical checks of indexed views, XML
	// and spatial indexes (EXTENDED_LOGICAL_CHECKS).
	ExtendedLogicalChecks bool
	// DataPurity checks column values against their types (DATA_PURITY),
	// needed once on a database upgraded from SQL Server 2000 or earlier.
	DataPurity bool
	// MaxDOP caps the check's parallelism (MAXDOP = n). Zero sends nothing,
	// so the server's own setting applies.
	MaxDOP int
}

// CheckDBError is one row of DBCC CHECKDB ... WITH TABLERESULTS: an error the
// check found, or — at Level 10 — its summary (8990 per table, 8989 for the
// database, "CHECKDB found N allocation errors and M consistency errors").
// A clean database returns no row at all.
type CheckDBError struct {
	// Error is the message number (8939, 8928, ...).
	Error   int
	Level   int
	State   int
	Message string
	// RepairLevel is the least repair that fixes this error —
	// "repair_rebuild" (no data loss) or "repair_allow_data_loss" — and empty
	// on a summary row or an error no repair fixes.
	RepairLevel string
	// ObjectID and IndexID name the object and index the error is in; zero
	// when the error is not in one (an allocation error, a summary).
	ObjectID    int
	IndexID     int
	PartitionID int64
	AllocUnitID int64
	// File, Page and Slot locate the damaged record; zero when the error
	// names none.
	File int
	Page int
	Slot int
}

// IsSummary reports whether the row is a summary (8989, 8990) rather than an
// error the check found.
func (e *CheckDBError) IsSummary() bool { return e.Level <= 10 }

// CheckDB runs DBCC CHECKDB on the database WITH NO_INFOMSGS, ALL_ERRORMSGS,
// TABLERESULTS and returns what it found, as rows: none for a clean
// database, the errors and their summaries otherwise. Finding corruption is
// not an error — the check did its job; the rows say what it found.
//
// It needs sysadmin or db_owner. It is never retried: a check reads every
// page and can run for hours, and a connection lost after it started would
// start it over. Under WithScript it captures the statement and returns no
// rows.
func (d *Database) CheckDB(ctx context.Context, opts CheckDBOptions) ([]*CheckDBError, error) {
	stmt, err := buildCheckDBStatement(d.Name, opts)
	if err != nil {
		return nil, fmt.Errorf("gosmo: check database %q: %w", d.Name, err)
	}
	if Scripting(ctx) {
		if _, err := d.exec(ctx, stmt); err != nil {
			return nil, fmt.Errorf("gosmo: check database %q: %w", d.Name, err)
		}
		return nil, nil
	}
	var found []*CheckDBError
	err = d.execIn(ctx, stmt, nil, func(ctx context.Context, c sqlConn, q string, args []any) error {
		rows, err := c.QueryContext(ctx, q, args...)
		if err != nil {
			return err
		}
		found, err = scanCheckDBRows(rows)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("gosmo: check database %q: %w", d.Name, err)
	}
	observe(ctx, d.server, ScriptEntry{Server: scriptServerName(ctx, d.server), Database: d.Name, SQL: stmt})
	return found, nil
}

// buildCheckDBStatement renders CheckDB's statement, naming the database as a
// literal, as SSMS's maintenance plans do.
func buildCheckDBStatement(name string, opts CheckDBOptions) (string, error) {
	if opts.PhysicalOnly && opts.DataPurity {
		return "", invalidf("PHYSICAL_ONLY and DATA_PURITY cannot be combined")
	}
	if opts.PhysicalOnly && opts.ExtendedLogicalChecks {
		return "", invalidf("PHYSICAL_ONLY and EXTENDED_LOGICAL_CHECKS cannot be combined")
	}
	if opts.MaxDOP < 0 {
		return "", invalidf("MAXDOP %d is negative", opts.MaxDOP)
	}
	target := QuoteLiteral(name)
	if opts.NoIndex {
		target += ", NOINDEX"
	}
	with := []string{"NO_INFOMSGS", "ALL_ERRORMSGS", "TABLERESULTS"}
	if opts.PhysicalOnly {
		with = append(with, "PHYSICAL_ONLY")
	}
	if opts.ExtendedLogicalChecks {
		with = append(with, "EXTENDED_LOGICAL_CHECKS")
	}
	if opts.DataPurity {
		with = append(with, "DATA_PURITY")
	}
	if opts.MaxDOP > 0 {
		with = append(with, fmt.Sprintf("MAXDOP = %d", opts.MaxDOP))
	}
	return fmt.Sprintf("DBCC CHECKDB (%s) WITH %s", target, strings.Join(with, ", ")), nil
}

// scanCheckDBRows reads TABLERESULTS by column name: the result has 23
// columns on 2016 to 2025 alike, but it is DBCC output, not a catalog view,
// and nothing documents its shape — a column added or moved must not shift
// every value after it. Columns CheckDBError does not carry are read and
// dropped. The caller wraps the error.
func scanCheckDBRows(rows *sql.Rows) ([]*CheckDBError, error) {
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	var out []*CheckDBError
	for rows.Next() {
		var (
			errNo, level, state, objectID, indexID, partitionID, allocUnitID, file, page, slot sql.NullInt64
			msg, repair                                                                        sql.NullString
		)
		dest := make([]any, len(cols))
		for i, c := range cols {
			switch c {
			case "Error":
				dest[i] = &errNo
			case "Level":
				dest[i] = &level
			case "State":
				dest[i] = &state
			case "MessageText":
				dest[i] = &msg
			case "RepairLevel":
				dest[i] = &repair
			case "ObjectId":
				dest[i] = &objectID
			case "IndexId":
				dest[i] = &indexID
			case "PartitionId":
				dest[i] = &partitionID
			case "AllocUnitId":
				dest[i] = &allocUnitID
			case "File":
				dest[i] = &file
			case "Page":
				dest[i] = &page
			case "Slot":
				dest[i] = &slot
			default:
				dest[i] = new(any)
			}
		}
		if err := rows.Scan(dest...); err != nil {
			return nil, err
		}
		out = append(out, &CheckDBError{
			Error: int(errNo.Int64), Level: int(level.Int64), State: int(state.Int64),
			Message: msg.String, RepairLevel: repair.String,
			ObjectID: int(objectID.Int64), IndexID: int(indexID.Int64),
			PartitionID: partitionID.Int64, AllocUnitID: allocUnitID.Int64,
			File: int(file.Int64), Page: int(page.Int64), Slot: int(slot.Int64),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// ============================================================
// Fragmentation across the database
// ============================================================

// DatabaseIndexFragmentation is one index's fragmentation in a
// Database.FragmentationStats read: Table.FragmentationStats's
// IndexFragmentation plus the object and index kind it belongs to, which a
// whole-database read has to say.
type DatabaseIndexFragmentation struct {
	IndexFragmentation
	Schema     string
	ObjectName string
	ObjectID   int
	// IsView is true for an indexed view's index, false for a table's.
	IsView    bool
	IndexType IndexType
	// PartitionCount is how many partitions were folded into the row (see
	// fragmentationSelect for how): 1 for an unpartitioned index.
	PartitionCount int
}

// FragmentationStats returns the fragmentation of every index on the
// database's user tables and indexed views whose fragmentation is at least
// minPct percent and whose leaf has at least minPages pages, most fragmented
// first. Heaps are left out (index_id 0 has no logical order to fragment), as
// in Table.FragmentationStats; partitions are folded into one row per index
// the same way. An empty mode is FragmentationLimited.
//
// It reads sys.dm_db_index_physical_stats once for the whole database and
// filters on the server: the minimum page count is what keeps a small
// index's meaningless percentage out (Microsoft's guidance ignores indexes
// under about 1,000 pages). Reading every object needs VIEW DATABASE STATE;
// a DETAILED scan reads every page and is slow on a large database.
func (d *Database) FragmentationStats(ctx context.Context, mode FragmentationMode, minPct float64, minPages int64) ([]*DatabaseIndexFragmentation, error) {
	what := fmt.Sprintf("fragmentation stats for database %q", d.Name)
	mode, err := mode.normalize()
	if err != nil {
		return nil, fmt.Errorf("gosmo: %s: %w", what, err)
	}
	if minPct < 0 || minPct > 100 {
		return nil, invalidf("gosmo: %s: minimum fragmentation %g%% is outside 0–100", what, minPct)
	}
	if minPages < 0 {
		return nil, invalidf("gosmo: %s: minimum page count %d is negative", what, minPages)
	}
	// The DMV is called once with a NULL object_id — every object — which
	// is the point here, not the hazard quoting.go warns of. Its rows are
	// filtered as fragmentationSelect's are, then folded per index.
	q := fmt.Sprintf(`
SELECT ISNULL(i.name, N''), i.index_id,
       ISNULL(SUM(s.avg_fragmentation_in_percent * s.page_count) / NULLIF(SUM(s.page_count), 0), 0),
       ISNULL(SUM(s.page_count), 0),
       ISNULL(SUM(s.fragment_count), 0),
       ISNULL(SUM(s.avg_page_space_used_in_percent * s.page_count) / NULLIF(SUM(s.page_count), 0), 0),
       SCHEMA_NAME(o.schema_id), o.name, o.object_id,
       CAST(CASE WHEN o.type = 'V' THEN 1 ELSE 0 END AS bit),
       i.type_desc, COUNT(*)
FROM   sys.dm_db_index_physical_stats(DB_ID(), NULL, NULL, NULL, N'%s') s
JOIN   sys.indexes i ON i.object_id = s.object_id AND i.index_id = s.index_id
JOIN   sys.objects o ON o.object_id = s.object_id
WHERE  s.index_level = 0 AND s.alloc_unit_type_desc = N'IN_ROW_DATA'
       AND s.index_id > 0 AND o.type IN ('U', 'V') AND o.is_ms_shipped = 0
GROUP  BY o.schema_id, o.name, o.object_id, o.type, i.name, i.index_id, i.type_desc
HAVING ISNULL(SUM(s.avg_fragmentation_in_percent * s.page_count) / NULLIF(SUM(s.page_count), 0), 0) >= @p1
       AND ISNULL(SUM(s.page_count), 0) >= @p2
ORDER  BY 3 DESC, 7, 8, 1`, mode)

	rows, err := d.query(ctx, q, minPct, minPages)
	return scanRows(rows, err, what, func(scan func(...any) error) (*DatabaseIndexFragmentation, error) {
		f := &DatabaseIndexFragmentation{}
		var desc string
		if err := scan(&f.IndexName, &f.IndexID, &f.AvgFragmentationPct, &f.PageCount, &f.FragmentCount, &f.AvgPageSpaceUsedPct,
			&f.Schema, &f.ObjectName, &f.ObjectID, &f.IsView, &desc, &f.PartitionCount); err != nil {
			return nil, err
		}
		f.IndexType = indexTypeFromDesc(desc)
		return f, nil
	})
}

// ============================================================
// sp_updatestats
// ============================================================

// UpdateStatistics updates the statistics of every user table and indexed
// view in the database that has had rows modified since its statistics were
// last updated (sys.sp_updatestats) — the database-wide form of
// Table.UpdateAllStatistics, which updates a table's whether stale or not.
// With resample each statistic keeps the sample rate of its last update
// (@resample = 'resample'); without it the server picks its default sample.
//
// It needs sysadmin or db_owner (the procedure checks for the database's
// owner). Under WithScript it captures the statement.
func (d *Database) UpdateStatistics(ctx context.Context, resample bool) error {
	stmt := "EXEC sys.sp_updatestats"
	if resample {
		stmt += " @resample = 'resample'"
	}
	if _, err := d.exec(ctx, stmt); err != nil {
		return fmt.Errorf("gosmo: update statistics in database %q: %w", d.Name, err)
	}
	return nil
}
