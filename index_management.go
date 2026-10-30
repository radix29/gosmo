package gosmo

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// -- Index management ----------------------------------------------------------

// Table returns the table the index is on, or nil for an indexed view's
// index — see View.
func (idx *Index) Table() *Table { return idx.table }

// View returns the indexed view the index is on, or nil for a table's index.
func (idx *Index) View() *View { return idx.view }

// on is the object the index is on as the Table every statement and read
// is built from: its table, or its view's carrier (View.carrier).
func (idx *Index) on() *Table {
	if idx.view != nil {
		return idx.view.carrier()
	}
	return idx.table
}

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
	return quoteIdent(idx.Name) + " ON " + idx.on().FullName()
}

// Reorganize reorganizes the index (ALTER INDEX ... REORGANIZE).
func (idx *Index) Reorganize(ctx context.Context) error {
	if err := idx.refuseHypothetical("reorganize"); err != nil {
		return err
	}
	q := fmt.Sprintf("ALTER INDEX %s REORGANIZE", idx.target())
	if err := idx.on().exec(ctx, q); err != nil {
		return fmt.Errorf("gosmo: reorganize index %q: %w", idx.Name, err)
	}
	return nil
}

// Disable disables the index (ALTER INDEX ... DISABLE).
func (idx *Index) Disable(ctx context.Context) error {
	if err := idx.refuseHypothetical("disable"); err != nil {
		return err
	}
	q := fmt.Sprintf("ALTER INDEX %s DISABLE", idx.target())
	if err := idx.on().exec(ctx, q); err != nil {
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
	if err := idx.on().exec(ctx, q); err != nil {
		return fmt.Errorf("gosmo: drop index %q: %w", idx.Name, err)
	}
	return nil
}

// RebuildAllIndexes rebuilds all indexes on the table (ALTER INDEX ALL ... REBUILD).
// RebuildAllIndexesWith takes every rebuild option, not only a fill factor.
func (t *Table) RebuildAllIndexes(ctx context.Context, fillFactor int) error {
	q := fmt.Sprintf("ALTER INDEX ALL ON %s REBUILD", t.FullName())
	if fillFactor > 0 {
		q += fmt.Sprintf(" WITH (FILLFACTOR = %d)", fillFactor)
	}
	if err := t.exec(ctx, q); err != nil {
		return fmt.Errorf("gosmo: rebuild all indexes on %s: %w", t.FullName(), err)
	}
	return nil
}

// RebuildAllIndexesWith rebuilds every index on the table (ALTER INDEX ALL
// ... REBUILD) with a WITH clause only for the options opts sets — Rebuild's
// options, for the whole table. Unlike Rebuild it knows no index's type, so
// an option one of the table's indexes refuses (FILLFACTOR on a columnstore,
// ONLINE on an edition without online index operations) is left for the server to refuse.
func (t *Table) RebuildAllIndexesWith(ctx context.Context, opts IndexRebuildOptions) error {
	with, err := opts.withClause()
	if err != nil {
		return fmt.Errorf("gosmo: rebuild all indexes on %s: %w", t.FullName(), err)
	}
	q := fmt.Sprintf("ALTER INDEX ALL ON %s REBUILD", t.FullName()) + with
	if err := t.exec(ctx, q); err != nil {
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
//
// All three are rowstore options: on an index read as columnstore, or a
// memory-optimized table's (see refuseAlterIndex), SetOptions is refused
// before anything is sent.
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
		return invalidf("gosmo: set options on index %q: no option given", idx.Name)
	}
	if err := idx.refuseAlterIndex("set options on"); err != nil {
		return err
	}
	if idx.Type.IsColumnStore() {
		// Msg 35327 (IGNORE_DUP_KEY), 35328 (the lock options).
		return invalidf("gosmo: set options on index %q: a %s index takes none of IGNORE_DUP_KEY, ALLOW_ROW_LOCKS, ALLOW_PAGE_LOCKS", idx.Name, idx.Type)
	}
	q := fmt.Sprintf("ALTER INDEX %s SET (%s)", idx.target(), strings.Join(set, ", "))
	if err := idx.on().exec(ctx, q); err != nil {
		return fmt.Errorf("gosmo: set options on index %q: %w", idx.Name, err)
	}
	return nil
}

// Rename renames the index using sp_rename — also the mechanism for
// renaming a PRIMARY KEY or UNIQUE constraint, since its name is the
// backing index's name in sys.indexes.
func (idx *Index) Rename(ctx context.Context, newName string) error {
	objName := idx.on().FullName() + "." + quoteIdent(idx.Name)
	if err := idx.on().exec(ctx,
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
//
// Each entry carries the partition's DATA_COMPRESSION and its XML_COMPRESSION;
// sys.partitions.xml_compression is SQL Server 2022's, and an older instance
// reads it as off — the only value it can have there.
// https://learn.microsoft.com/sql/relational-databases/system-catalog-views/sys-partitions-transact-sql
func partitionCompressionList(major int, objectID, indexID string) string {
	return jsonRows("pp.partition_number AS n, pp.data_compression_desc AS c, "+
		bitSince(major, SQLServer2022, "pp.xml_compression")+" AS x",
		"\n        FROM sys.partitions pp WHERE pp.object_id = "+objectID+" AND pp.index_id = "+indexID,
		"pp.partition_number")
}

// PartitionOptions is one partition's settings of a heap or index, as
// sys.partitions has them. They live per partition because a partitioned
// heap or index can mix them, and a script written from the first
// partition's alone recreated it with one setting throughout.
type PartitionOptions struct {
	// Number is the partition_number, from 1.
	Number int
	// DataCompression is the partition's DATA_COMPRESSION.
	DataCompression DataCompression
	// XMLCompression is the partition's XML_COMPRESSION (SQL Server 2022);
	// always false on an older instance.
	XMLCompression bool
}

// decodePartitionCompression decodes one partitionCompressionList column into
// each partition's options, in partition_number order: element i is
// partition i+1. NULL (no partitions) is nil.
func decodePartitionCompression(col sql.NullString) ([]PartitionOptions, error) {
	rows, err := decodeJSONRows[struct {
		N int    `json:"n"`
		C string `json:"c"`
		X bool   `json:"x"`
	}](col)
	if err != nil || rows == nil {
		return nil, err
	}
	out := make([]PartitionOptions, len(rows))
	for i, r := range rows {
		if r.N != i+1 {
			return nil, fmt.Errorf("gosmo: partition %d listed at position %d", r.N, i+1)
		}
		out[i] = PartitionOptions{Number: r.N, DataCompression: DataCompression(r.C), XMLCompression: r.X}
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
	// Online rebuilds while the table stays readable and writable
	// (ONLINE = ON), at the cost of a longer rebuild. Online index operations
	// are an Enterprise, Developer and Azure feature: other editions refuse
	// the option (Msg 1712), which is left for the server to say. False sends
	// nothing — an offline rebuild, the server's default.
	Online bool
	// MaxDOP caps the rebuild's parallelism (MAXDOP = n). Zero sends nothing,
	// so the server's own setting applies; 1 rebuilds serially.
	MaxDOP int
}

// withClause renders the options as REBUILD's " WITH (...)", or "" when opts
// sets nothing, refusing values the server would.
func (opts IndexRebuildOptions) withClause() (string, error) {
	if opts.DataCompression != "" && !opts.DataCompression.valid() {
		return "", invalidf("invalid data compression %q (must be NONE, ROW, PAGE, COLUMNSTORE, or COLUMNSTORE_ARCHIVE)", opts.DataCompression)
	}
	if opts.FillFactor < 0 || opts.FillFactor > 100 {
		return "", invalidf("fill factor %d out of range (1-100)", opts.FillFactor)
	}
	if opts.MaxDOP < 0 || opts.MaxDOP > 64 {
		return "", invalidf("MAXDOP %d out of range (1-64)", opts.MaxDOP)
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
	if opts.Online {
		withParts = append(withParts, "ONLINE = ON")
	}
	if opts.MaxDOP > 0 {
		withParts = append(withParts, fmt.Sprintf("MAXDOP = %d", opts.MaxDOP))
	}
	if len(withParts) == 0 {
		return "", nil
	}
	return " WITH (" + strings.Join(withParts, ", ") + ")", nil
}

// refuseAlterIndex refuses any ALTER INDEX on a memory-optimized table's
// index, which the server refuses outright ("The operation 'ALTER INDEX' is
// not supported with memory optimized tables"). It knows the table only as
// far as idx was read: a hash index always is one, and an index of a table
// read by TableByName says so; an IndexRef on a TableRef is left to the
// server. A hypothetical index is refused too (see refuseHypothetical).
func (idx *Index) refuseAlterIndex(verb string) error {
	if err := idx.refuseHypothetical(verb); err != nil {
		return err
	}
	if idx.Type == IndexTypeNonClusteredHash || (idx.table != nil && idx.table.IsMemoryOptimized) {
		return invalidf("gosmo: %s index %q: ALTER INDEX is not supported on a memory-optimized table", verb, idx.Name)
	}
	return nil
}

// refuseHypothetical refuses ALTER INDEX on an index read as hypothetical
// (Index.IsHypothetical). The server accepts REBUILD, REORGANIZE, DISABLE
// and SET on one and does nothing — no data to rebuild, nothing to disable —
// so the call reported a change that never happened. An IndexRef was not
// read and is left to the server.
func (idx *Index) refuseHypothetical(verb string) error {
	if idx.IsHypothetical {
		return invalidf("gosmo: %s index %q: it is a hypothetical (tuning advisor) index, which has no data; ALTER INDEX does nothing to it", verb, idx.Name)
	}
	return nil
}

// Rebuild rebuilds the index (ALTER INDEX ... REBUILD), with a WITH clause
// only for the options opts sets.
//
// On an index whose Type was read, the options its kind refuses are refused
// before anything is sent: FILLFACTOR, PAD_INDEX and NONE/ROW/PAGE
// compression on a columnstore index, COLUMNSTORE compression on a rowstore
// one. On a memory-optimized table's index every rebuild is refused (see
// refuseAlterIndex).
func (idx *Index) Rebuild(ctx context.Context, opts IndexRebuildOptions) error {
	with, err := opts.withClause()
	if err != nil {
		return fmt.Errorf("gosmo: rebuild index %q: %w", idx.Name, err)
	}
	if err := idx.refuseAlterIndex("rebuild"); err != nil {
		return err
	}
	if idx.Type != "" {
		columnstoreCompression := opts.DataCompression == DataCompressionColumnstore || opts.DataCompression == DataCompressionColumnstoreArchive
		switch {
		case idx.Type.IsColumnStore() && (opts.FillFactor > 0 || opts.PadIndex != nil):
			// Msg 35327.
			return invalidf("gosmo: rebuild index %q: a %s index takes neither FILLFACTOR nor PAD_INDEX", idx.Name, idx.Type)
		case idx.Type.IsColumnStore() && opts.DataCompression != "" && !columnstoreCompression:
			// Msg 10799.
			return invalidf("gosmo: rebuild index %q: a %s index takes COLUMNSTORE or COLUMNSTORE_ARCHIVE compression, not %s", idx.Name, idx.Type, opts.DataCompression)
		case !idx.Type.IsColumnStore() && columnstoreCompression:
			// Msg 10798.
			return invalidf("gosmo: rebuild index %q: %s compression applies to a columnstore index only, not a %s one", idx.Name, opts.DataCompression, idx.Type)
		}
	}

	q := fmt.Sprintf("ALTER INDEX %s REBUILD", idx.target()) + with
	if err := idx.on().exec(ctx, q); err != nil {
		return fmt.Errorf("gosmo: rebuild index %q: %w", idx.Name, err)
	}
	// A rebuild is what enables a disabled index (Enable is this call). Left
	// set, the stale flag made a later SetIncludedColumns on the same handle
	// re-disable the index it reissues.
	setIfApplied(ctx, &idx.IsDisabled, false)
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
	q := rowstoreIndexCreate(&next, idx.on().FullName(), indexWithClause(&next, "\n    ", "DROP_EXISTING = ON"), on)
	if idx.IsDisabled {
		q += fmt.Sprintf(";\nALTER INDEX %s DISABLE", idx.target())
	}
	if err := idx.on().exec(ctx, q); err != nil {
		return fmt.Errorf("gosmo: set included columns on index %q: %w", idx.Name, err)
	}
	setIfApplied(ctx, &idx.IncludedColumns, next.IncludedColumns)
	return nil
}

// IncludedColumnsSupported reports whether SetIncludedColumns can change
// idx's INCLUDE list, and if not, why — so a caller can grey the choice out
// up front rather than learn it on Apply. nil means supported.
func (idx *Index) IncludedColumnsSupported() error {
	switch {
	case idx.Type == "":
		// An IndexRef handle: nothing was read, so nothing can be restated.
		return fmt.Errorf("the index's properties were not read — an IndexRef: %w", ErrHandleNotLoaded)
	case idx.Type != IndexTypeNonClustered:
		return unsupportedf("not supported for a %s index", idx.Type)
	case idx.IsPrimaryKey:
		return unsupportedf("not supported for an index backing a PRIMARY KEY constraint")
	case idx.IsUniqueConstraint:
		return unsupportedf("not supported for an index backing a UNIQUE constraint")
	case idx.DataSpace.Name == "":
		// A memory-optimized table's index: no data space of its own, and
		// its indexes are changed with ALTER TABLE, never CREATE INDEX.
		return unsupportedf("not supported for an index on a memory-optimized table")
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
	q := fmt.Sprintf("UPDATE STATISTICS %s (%s) WITH %s", idx.on().FullName(), quoteIdent(idx.Name), sampleClause(samplePct))
	if err := idx.on().exec(ctx, q); err != nil {
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
func (t *Table) FragmentationStats(ctx context.Context, mode FragmentationMode) ([]*IndexFragmentation, error) {
	mode, err := mode.normalize()
	if err != nil {
		return nil, fmt.Errorf("gosmo: fragmentation stats: %w", err)
	}

	q := fragmentationSelect(mode) + `
WHERE  i.object_id = OBJECT_ID(@p1) AND i.index_id > 0 AND i.is_hypothetical = 0
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
