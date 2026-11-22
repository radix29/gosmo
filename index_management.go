package gosmo

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
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
	return QuoteName(idx.Name) + " ON " + idx.on().FullName()
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
// On SQL database in Fabric it sends one ALTER INDEX per index instead — see
// RebuildAllIndexesScript.
func (t *Table) RebuildAllIndexes(ctx context.Context, fillFactor int) error {
	with := ""
	if fillFactor > 0 {
		with = fmt.Sprintf(" WITH (FILLFACTOR = %d)", fillFactor)
	}
	return t.alterAllIndexes(ctx, "rebuild all indexes on", "REBUILD", with)
}

// RebuildAllIndexesWith rebuilds every index on the table (ALTER INDEX ALL
// ... REBUILD) with a WITH clause only for the options opts sets — Rebuild's
// options, for the whole table. Unlike Rebuild it knows no index's type, so
// an option one of the table's indexes refuses (FILLFACTOR on a columnstore,
// ONLINE on an edition without online index operations) is left for the server to refuse.
// On SQL database in Fabric it sends one ALTER INDEX per index instead — see
// RebuildAllIndexesScript.
func (t *Table) RebuildAllIndexesWith(ctx context.Context, opts IndexRebuildOptions) error {
	with, err := opts.withClause()
	if err != nil {
		return fmt.Errorf("gosmo: rebuild all indexes on %s: %w", t.FullName(), err)
	}
	return t.alterAllIndexes(ctx, "rebuild all indexes on", "REBUILD", with)
}

// RebuildAllIndexesScript returns the batch RebuildAllIndexesWith would run
// for opts, without running it — for a caller that shows the statement for
// the user to run (a query window) and must not drift from what the method
// sends.
//
// Everywhere but SQL database in Fabric that is the one ALTER INDEX ALL
// statement, and nothing is read. Fabric refuses ALTER INDEX ALL (a named
// index is accepted), so there the table's indexes are read and the batch is
// one ALTER INDEX [name] ON ... REBUILD per index, in index_id order (the
// clustered index first), each with the same WITH clause: every index ALL
// would rebuild, disabled ones included (a rebuild enables them, as ALL
// does), hypothetical ones left out (ALTER INDEX does nothing to them). A
// table with no indexes — a heap — gives "", which RebuildAllIndexesWith
// sends nothing for.
func (t *Table) RebuildAllIndexesScript(ctx context.Context, opts IndexRebuildOptions) (string, error) {
	with, err := opts.withClause()
	if err != nil {
		return "", fmt.Errorf("gosmo: rebuild all indexes on %s: %w", t.FullName(), err)
	}
	if err := requireSchema("rebuild all indexes on", t.Schema, t.Name); err != nil {
		return "", err
	}
	q, err := t.allIndexesBatch(ctx, "REBUILD", with)
	if err != nil {
		return "", fmt.Errorf("gosmo: rebuild all indexes on %s: %w", t.FullName(), err)
	}
	return q, nil
}

// ReorganizeAllIndexes reorganizes every index on the table (ALTER INDEX
// ALL ... REORGANIZE): Index.Reorganize for the whole table, in one
// statement. A reorganize is always online and takes no options. On SQL
// database in Fabric, which refuses ALTER INDEX ALL, it sends one ALTER
// INDEX ... REORGANIZE per index ALL would reorganize — every index that is
// neither disabled nor hypothetical — in one batch (see
// RebuildAllIndexesScript).
func (t *Table) ReorganizeAllIndexes(ctx context.Context) error {
	return t.alterAllIndexes(ctx, "reorganize all indexes on", "REORGANIZE", "")
}

// alterAllIndexes runs allIndexesBatch's batch, wrapping every error with
// op ("rebuild all indexes on") and the table. An empty batch sends nothing.
// A table with no schema is refused first, as Table.exec would, so the Fabric
// read never resolves an unqualified name.
func (t *Table) alterAllIndexes(ctx context.Context, op, verb, with string) error {
	if err := requireSchema(op, t.Schema, t.Name); err != nil {
		return err
	}
	q, err := t.allIndexesBatch(ctx, verb, with)
	if err == nil && q != "" {
		err = t.exec(ctx, q)
	}
	if err != nil {
		return fmt.Errorf("gosmo: %s %s: %w", op, t.FullName(), err)
	}
	return nil
}

// allIndexesBatch is the text an …All… method sends: ALTER INDEX ALL ON the
// table verb with, or, on SQL database in Fabric, the per-index expansion
// RebuildAllIndexesScript describes ("" when nothing qualifies). A
// REORGANIZE skips disabled indexes too: the server refuses one by name,
// and ALL passes over it.
func (t *Table) allIndexesBatch(ctx context.Context, verb, with string) (string, error) {
	if !t.db.serverInfo().IsFabricSQLDatabase() {
		return "ALTER INDEX ALL ON " + t.FullName() + " " + verb + with, nil
	}
	targets, err := t.allIndexTargets(ctx)
	if err != nil {
		return "", err
	}
	var stmts []string
	for _, x := range targets {
		if x.hypothetical || (x.disabled && verb == "REORGANIZE") {
			continue
		}
		stmts = append(stmts, "ALTER INDEX "+QuoteName(x.name)+" ON "+t.FullName()+" "+verb+with+";")
	}
	return strings.Join(stmts, "\n"), nil
}

// allIndexTarget is one row allIndexTargets reads.
type allIndexTarget struct {
	name                   string
	disabled, hypothetical bool
}

// allIndexTargetsQuery reads the indexes ALTER INDEX ALL would act on (type
// > 0: not the heap) by object id, or, for a TableRef with none, by name.
// The derived table gives one row even when the object is missing — a NULL
// object_id, refused as not found as ALL would be — or has no index (a NULL
// name).
const allIndexTargetsQuery = `
SELECT o.object_id, i.name, ISNULL(i.is_disabled, CAST(0 AS bit)), ISNULL(i.is_hypothetical, CAST(0 AS bit))
FROM   (SELECT ISNULL(NULLIF(@p1, 0), OBJECT_ID(@p2)) AS object_id) o
LEFT   JOIN sys.indexes i ON i.object_id = o.object_id AND i.type > 0
ORDER  BY i.index_id`

// allIndexTargets lists the table's indexes for allIndexesBatch's Fabric
// expansion: a lean read, not Indexes, so a TableRef handle works as it does
// for ALTER INDEX ALL.
func (t *Table) allIndexTargets(ctx context.Context) ([]allIndexTarget, error) {
	rows, err := t.db.query(ctx, allIndexTargetsQuery, t.ObjectID, t.FullName())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []allIndexTarget
	found := false
	for rows.Next() {
		var oid sql.NullInt64
		var name sql.NullString
		var x allIndexTarget
		if err := rows.Scan(&oid, &name, &x.disabled, &x.hypothetical); err != nil {
			return nil, err
		}
		found = found || oid.Valid
		if name.Valid {
			x.name = name.String
			out = append(out, x)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if !found {
		return nil, notFoundf("table %s not found", t.FullName())
	}
	return out, nil
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
	objName := idx.on().FullName() + "." + QuoteName(idx.Name)
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
// (UPDATE STATISTICS table (index) [WITH ...] — every index has an implicit
// statistics object with the same name), with the sampling opts asks for —
// see StatisticsUpdateOptions; its zero value lets the server choose the
// sample, as Statistic.Update's does.
func (idx *Index) UpdateStatistics(ctx context.Context, opts StatisticsUpdateOptions) error {
	with, err := opts.withClause()
	if err != nil {
		return fmt.Errorf("gosmo: update statistics for index %q: %w", idx.Name, err)
	}
	q := fmt.Sprintf("UPDATE STATISTICS %s (%s)", idx.on().FullName(), QuoteName(idx.Name)) + with
	if err := idx.on().exec(ctx, q); err != nil {
		return fmt.Errorf("gosmo: update statistics for index %q: %w", idx.Name, err)
	}
	return nil
}
