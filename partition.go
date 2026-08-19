package gosmo

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// partitionBoundaryPattern matches a well-formed SQL Server literal: a
// signed integer or decimal, a hex literal, a properly quoted (and
// escaped) string/date literal, or NULL.
var partitionBoundaryPattern = regexp.MustCompile(`(?i)^(-?\d+(\.\d+)?|0x[0-9a-f]+|n?'(?:[^']|'')*'|null)$`)

// validPartitionBoundary reports whether v is safe to splice directly into
// a partition function's VALUES/SPLIT RANGE/MERGE RANGE clause. These
// values can't be parameterized in DDL, so this validates literal *shape*
// rather than checking against a fixed set of values.
func validPartitionBoundary(v string) bool {
	return partitionBoundaryPattern.MatchString(strings.TrimSpace(v))
}

// ============================================================
// Partition Functions & Schemes
// ============================================================

// PartitionFunction mirrors sys.partition_functions.
type PartitionFunction struct {
	db            *Database
	Name          string
	FunctionID    int
	InputType     DataType
	BoundaryCount int
	IsRight       bool // RIGHT = boundary is in right partition
	Boundaries    []string
}

// partitionFunctionSelect is the column list and joins every partition
// function read shares; the listing adds ORDER BY, the by-name lookup a
// WHERE.
const partitionFunctionSelect = `
SELECT pf.name, pf.function_id, pf.fanout - 1,
       tp.name AS input_type, pf.boundary_value_on_right,
       -- Style 126 (ISO 8601) matters for a date/time boundary: the default
       -- conversion yields "Jan  1 2026", which loses any time part and has
       -- to be reparsed by whoever reads it. It is ignored for every other
       -- type, so it costs nothing there.
       (SELECT STRING_AGG(CONVERT(NVARCHAR(256), prv.value, 126), ',')
        WITHIN GROUP (ORDER BY prv.boundary_id)
        FROM sys.partition_range_values prv
        WHERE prv.function_id = pf.function_id) AS boundaries
FROM   sys.partition_functions pf
JOIN   sys.partition_parameters pp ON pp.function_id = pf.function_id
JOIN   sys.types tp ON tp.user_type_id = pp.user_type_id`

// PartitionFunctions returns all partition functions in the database.
func (d *Database) PartitionFunctions() ([]*PartitionFunction, error) {
	return d.PartitionFunctionsContext(context.Background())
}

// PartitionFunctionsContext is the context-aware variant of PartitionFunctions.
func (d *Database) PartitionFunctionsContext(ctx context.Context) ([]*PartitionFunction, error) {
	rows, err := d.query(ctx, partitionFunctionSelect+`
ORDER  BY pf.name`)
	if err != nil {
		return nil, fmt.Errorf("gosmo: list partition functions: %w", err)
	}
	defer rows.Close()

	var funcs []*PartitionFunction
	for rows.Next() {
		pf, err := scanPartitionFunction(d, rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("gosmo: list partition functions: %w", err)
		}
		funcs = append(funcs, pf)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("gosmo: list partition functions: %w", err)
	}
	return funcs, nil
}

// PartitionFunctionByName returns one partition function by name.
func (d *Database) PartitionFunctionByName(name string) (*PartitionFunction, error) {
	return d.PartitionFunctionByNameContext(context.Background(), name)
}

// PartitionFunctionByNameContext is the context-aware variant of
// PartitionFunctionByName.
func (d *Database) PartitionFunctionByNameContext(ctx context.Context, name string) (*PartitionFunction, error) {
	var pf *PartitionFunction
	err := d.queryRow(ctx, func(row *sql.Row) error {
		var err error
		pf, err = scanPartitionFunction(d, row.Scan)
		return err
	}, partitionFunctionSelect+`
WHERE  pf.name = @p1`, name)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, notFoundf("gosmo: partition function %q not found in %q", name, d.name)
		}
		return nil, fmt.Errorf("gosmo: find partition function %q in %q: %w", name, d.name, err)
	}
	return pf, nil
}

func scanPartitionFunction(d *Database, scan func(...any) error) (*PartitionFunction, error) {
	pf := &PartitionFunction{db: d}
	var boundaries sql.NullString
	if err := scan(&pf.Name, &pf.FunctionID, &pf.BoundaryCount,
		&pf.InputType, &pf.IsRight, &boundaries); err != nil {
		return nil, err
	}
	if boundaries.Valid && boundaries.String != "" {
		pf.Boundaries = strings.Split(boundaries.String, ",")
	}
	return pf, nil
}

// CreatePartitionFunctionRequest describes a partition function to create.
type CreatePartitionFunctionRequest struct {
	Name       string
	InputType  DataType
	IsRight    bool
	Boundaries []string // literal boundary values, e.g. {"100","200","300"}
}

// CreatePartitionFunction creates a partition function.
func (d *Database) CreatePartitionFunction(req CreatePartitionFunctionRequest) error {
	return d.CreatePartitionFunctionContext(context.Background(), req)
}

// CreatePartitionFunctionContext is the context-aware variant of CreatePartitionFunction.
func (d *Database) CreatePartitionFunctionContext(ctx context.Context, req CreatePartitionFunctionRequest) error {
	if len(req.Boundaries) == 0 {
		return fmt.Errorf("gosmo: create partition function: at least one boundary required")
	}
	if !validDataType(req.InputType) {
		return fmt.Errorf("gosmo: create partition function %q: unrecognized data type %q", req.Name, req.InputType)
	}
	for _, b := range req.Boundaries {
		if !validPartitionBoundary(b) {
			return fmt.Errorf("gosmo: create partition function %q: invalid boundary literal %q", req.Name, b)
		}
	}
	side := "LEFT"
	if req.IsRight {
		side = "RIGHT"
	}
	vals := strings.Join(req.Boundaries, ", ")
	q := fmt.Sprintf(
		"CREATE PARTITION FUNCTION %s (%s) AS RANGE %s FOR VALUES (%s)",
		quoteIdent(req.Name), req.InputType, side, vals,
	)
	_, err := d.exec(ctx, q)
	if err != nil {
		return fmt.Errorf("gosmo: create partition function [%s]: %w", req.Name, err)
	}
	return nil
}

// Drop drops the partition function.
func (pf *PartitionFunction) Drop() error {
	return pf.DropContext(context.Background())
}

// DropContext is the context-aware variant of Drop.
func (pf *PartitionFunction) DropContext(ctx context.Context) error {
	_, err := pf.db.exec(ctx,
		fmt.Sprintf("DROP PARTITION FUNCTION %s", quoteIdent(pf.Name)))
	if err != nil {
		return fmt.Errorf("gosmo: drop partition function [%s]: %w", pf.Name, err)
	}
	return nil
}

// SplitRange adds a new boundary value to the partition function.
func (pf *PartitionFunction) SplitRange(value string) error {
	return pf.SplitRangeContext(context.Background(), value)
}

// SplitRangeContext is the context-aware variant of SplitRange.
func (pf *PartitionFunction) SplitRangeContext(ctx context.Context, value string) error {
	if !validPartitionBoundary(value) {
		return fmt.Errorf("gosmo: split range on [%s]: invalid boundary literal %q", pf.Name, value)
	}
	_, err := pf.db.exec(ctx,
		fmt.Sprintf("ALTER PARTITION FUNCTION %s() SPLIT RANGE (%s)", quoteIdent(pf.Name), value))
	if err != nil {
		return fmt.Errorf("gosmo: split range on [%s]: %w", pf.Name, err)
	}
	return nil
}

// MergeRange removes a boundary value from the partition function.
func (pf *PartitionFunction) MergeRange(value string) error {
	return pf.MergeRangeContext(context.Background(), value)
}

// MergeRangeContext is the context-aware variant of MergeRange.
func (pf *PartitionFunction) MergeRangeContext(ctx context.Context, value string) error {
	if !validPartitionBoundary(value) {
		return fmt.Errorf("gosmo: merge range on [%s]: invalid boundary literal %q", pf.Name, value)
	}
	_, err := pf.db.exec(ctx,
		fmt.Sprintf("ALTER PARTITION FUNCTION %s() MERGE RANGE (%s)", quoteIdent(pf.Name), value))
	if err != nil {
		return fmt.Errorf("gosmo: merge range on [%s]: %w", pf.Name, err)
	}
	return nil
}

// -- Partition Schemes ---------------------------------------------------------

// PartitionScheme mirrors sys.partition_schemes.
type PartitionScheme struct {
	db           *Database
	Name         string
	SchemeID     int
	FunctionName string
	FileGroups   []string
}

// partitionSchemeSelect is the column list and joins every partition
// scheme read shares; the listing adds ORDER BY, the by-name lookup a WHERE.
const partitionSchemeSelect = `
SELECT ps.name, ps.data_space_id, pf.name AS func_name,
       (SELECT STRING_AGG(fg.name, ',') WITHIN GROUP (ORDER BY dds.destination_id)
        FROM sys.destination_data_spaces dds
        JOIN sys.filegroups fg ON fg.data_space_id = dds.data_space_id
        WHERE dds.partition_scheme_id = ps.data_space_id) AS filegroups
FROM   sys.partition_schemes ps
JOIN   sys.partition_functions pf ON pf.function_id = ps.function_id`

// PartitionSchemes returns all partition schemes in the database.
func (d *Database) PartitionSchemes() ([]*PartitionScheme, error) {
	return d.PartitionSchemesContext(context.Background())
}

// PartitionSchemesContext is the context-aware variant of PartitionSchemes.
func (d *Database) PartitionSchemesContext(ctx context.Context) ([]*PartitionScheme, error) {
	rows, err := d.query(ctx, partitionSchemeSelect+`
ORDER  BY ps.name`)
	if err != nil {
		return nil, fmt.Errorf("gosmo: list partition schemes: %w", err)
	}
	defer rows.Close()

	var schemes []*PartitionScheme
	for rows.Next() {
		ps, err := scanPartitionScheme(d, rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("gosmo: list partition schemes: %w", err)
		}
		schemes = append(schemes, ps)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("gosmo: list partition schemes: %w", err)
	}
	return schemes, nil
}

// PartitionSchemeByName returns one partition scheme by name.
func (d *Database) PartitionSchemeByName(name string) (*PartitionScheme, error) {
	return d.PartitionSchemeByNameContext(context.Background(), name)
}

// PartitionSchemeByNameContext is the context-aware variant of
// PartitionSchemeByName.
func (d *Database) PartitionSchemeByNameContext(ctx context.Context, name string) (*PartitionScheme, error) {
	var ps *PartitionScheme
	err := d.queryRow(ctx, func(row *sql.Row) error {
		var err error
		ps, err = scanPartitionScheme(d, row.Scan)
		return err
	}, partitionSchemeSelect+`
WHERE  ps.name = @p1`, name)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, notFoundf("gosmo: partition scheme %q not found in %q", name, d.name)
		}
		return nil, fmt.Errorf("gosmo: find partition scheme %q in %q: %w", name, d.name, err)
	}
	return ps, nil
}

func scanPartitionScheme(d *Database, scan func(...any) error) (*PartitionScheme, error) {
	ps := &PartitionScheme{db: d}
	var fgs sql.NullString
	if err := scan(&ps.Name, &ps.SchemeID, &ps.FunctionName, &fgs); err != nil {
		return nil, err
	}
	if fgs.Valid && fgs.String != "" {
		ps.FileGroups = strings.Split(fgs.String, ",")
	}
	return ps, nil
}

// CreatePartitionScheme creates a partition scheme backed by a partition function.
func (d *Database) CreatePartitionScheme(name, functionName string, fileGroups []string) error {
	return d.CreatePartitionSchemeContext(context.Background(), name, functionName, fileGroups)
}

// CreatePartitionSchemeContext is the context-aware variant of CreatePartitionScheme.
func (d *Database) CreatePartitionSchemeContext(ctx context.Context, name, functionName string, fileGroups []string) error {
	if len(fileGroups) == 0 {
		return fmt.Errorf("gosmo: create partition scheme: at least one filegroup required")
	}
	fgs := make([]string, len(fileGroups))
	for i, fg := range fileGroups {
		fgs[i] = quoteIdent(fg)
	}
	q := fmt.Sprintf(
		"CREATE PARTITION SCHEME %s AS PARTITION %s TO (%s)",
		quoteIdent(name), quoteIdent(functionName), strings.Join(fgs, ", "),
	)
	_, err := d.exec(ctx, q)
	if err != nil {
		return fmt.Errorf("gosmo: create partition scheme [%s]: %w", name, err)
	}
	return nil
}

// Drop drops the partition scheme.
func (ps *PartitionScheme) Drop() error {
	return ps.DropContext(context.Background())
}

// DropContext is the context-aware variant of Drop.
func (ps *PartitionScheme) DropContext(ctx context.Context) error {
	_, err := ps.db.exec(ctx,
		fmt.Sprintf("DROP PARTITION SCHEME %s", quoteIdent(ps.Name)))
	if err != nil {
		return fmt.Errorf("gosmo: drop partition scheme [%s]: %w", ps.Name, err)
	}
	return nil
}

// -- Table partition info ------------------------------------------------------

// PartitionInfo holds per-partition row counts for a table.
type PartitionInfo struct {
	PartitionNumber int
	Rows            int64
	DataCompression string
}

// Partitions returns per-partition row counts and compression for the table.
func (t *Table) Partitions() ([]*PartitionInfo, error) {
	return t.PartitionsContext(context.Background())
}

// PartitionsContext is the context-aware variant of Partitions. A
// non-partitioned table still returns exactly one row (partition number 1),
// same as sys.partitions itself.
func (t *Table) PartitionsContext(ctx context.Context) ([]*PartitionInfo, error) {
	const q = `
SELECT p.partition_number, p.rows, p.data_compression_desc
FROM   sys.partitions p
WHERE  p.object_id = @p1 AND p.index_id IN (0,1)
ORDER  BY p.partition_number`

	rows, err := t.db.query(ctx, q, t.ObjectID)
	if err != nil {
		return nil, fmt.Errorf("gosmo: partitions for %s: %w", t.FullName(), err)
	}
	defer rows.Close()

	var parts []*PartitionInfo
	for rows.Next() {
		p := &PartitionInfo{}
		if err := rows.Scan(&p.PartitionNumber, &p.Rows, &p.DataCompression); err != nil {
			return nil, fmt.Errorf("gosmo: partitions for %s: %w", t.FullName(), err)
		}
		parts = append(parts, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("gosmo: partitions for %s: %w", t.FullName(), err)
	}
	return parts, nil
}

// -- Table space usage -----------------------------------------------------

// TableSpaceInfo holds space usage for a table (SSMS's Table Properties >
// Storage page), mirroring the classic sp_spaceused breakdown: DataKB is
// the heap/clustered index's own row data, IndexKB is every other
// (nonclustered) index's row data, LOBKB is off-row large-object storage,
// and UnusedKB is reserved-but-not-yet-used space within already allocated
// extents.
type TableSpaceInfo struct {
	ReservedKB int64
	DataKB     int64
	IndexKB    int64
	LOBKB      int64
	UnusedKB   int64
	FileGroup  string
}

// SpaceUsed returns space usage for the table.
func (t *Table) SpaceUsed() (*TableSpaceInfo, error) {
	return t.SpaceUsedContext(context.Background())
}

// SpaceUsedContext is the context-aware variant of SpaceUsed.
func (t *Table) SpaceUsedContext(ctx context.Context) (*TableSpaceInfo, error) {
	const q = `
SELECT
    SUM(a.total_pages) * 8 AS reserved_kb,
    SUM(CASE WHEN i.index_id IN (0,1) AND a.type IN (1,3) THEN a.used_pages ELSE 0 END) * 8 AS data_kb,
    SUM(CASE WHEN i.index_id > 1 THEN a.used_pages ELSE 0 END) * 8 AS index_kb,
    SUM(CASE WHEN a.type = 2 THEN a.used_pages ELSE 0 END) * 8 AS lob_kb,
    SUM(a.total_pages - a.used_pages) * 8 AS unused_kb,
    (SELECT TOP 1 fg.name
     FROM   sys.indexes idx
     JOIN   sys.filegroups fg ON fg.data_space_id = idx.data_space_id
     WHERE  idx.object_id = @p1 AND idx.index_id IN (0,1)) AS filegroup
FROM   sys.partitions p
JOIN   sys.allocation_units a ON a.container_id = p.partition_id
JOIN   sys.indexes i ON i.object_id = p.object_id AND i.index_id = p.index_id
WHERE  p.object_id = @p1`

	info := &TableSpaceInfo{}
	var fg sql.NullString
	if err := t.db.queryRow(ctx, func(row *sql.Row) error {
		return row.Scan(&info.ReservedKB, &info.DataKB, &info.IndexKB, &info.LOBKB, &info.UnusedKB, &fg)
	}, q, t.ObjectID); err != nil {
		return nil, fmt.Errorf("gosmo: space used for %s: %w", t.FullName(), err)
	}
	info.FileGroup = fg.String
	return info, nil
}

// TableSpaceUsedAll returns space usage for every user table in the
// database, keyed by object_id — the same breakdown Table.SpaceUsed gives
// for one table, for all of them in a single round trip.
//
// Use this over a loop of Table.SpaceUsed whenever the caller wants more
// than a couple of tables: the per-table form costs one query (and one
// pooled connection) each, so a grid listing a few hundred tables is a few
// hundred round trips. The aggregate expressions and joins are the same, so
// the numbers are identical either way.
//
// A table with no allocated pages at all has no row in sys.partitions to
// aggregate and is therefore absent from the map, not present with zeroes —
// callers should treat a missing key as "no space used".
func (d *Database) TableSpaceUsedAll() (map[int]*TableSpaceInfo, error) {
	return d.TableSpaceUsedAllContext(context.Background())
}

// TableSpaceUsedAllContext is the context-aware variant of
// TableSpaceUsedAll.
func (d *Database) TableSpaceUsedAllContext(ctx context.Context) (map[int]*TableSpaceInfo, error) {
	// Same joins and aggregates as Table.SpaceUsedContext, grouped by object
	// instead of filtered to one. The filegroup is a LEFT JOIN rather than
	// that method's correlated subquery: sys.indexes has exactly one row per
	// object with index_id IN (0,1), so it can't multiply the aggregate. It
	// stays NULL for a partitioned table, whose base index sits on a
	// partition scheme rather than a filegroup — the subquery form returns
	// NULL there too.
	const q = `
SELECT
    p.object_id,
    SUM(a.total_pages) * 8 AS reserved_kb,
    SUM(CASE WHEN i.index_id IN (0,1) AND a.type IN (1,3) THEN a.used_pages ELSE 0 END) * 8 AS data_kb,
    SUM(CASE WHEN i.index_id > 1 THEN a.used_pages ELSE 0 END) * 8 AS index_kb,
    SUM(CASE WHEN a.type = 2 THEN a.used_pages ELSE 0 END) * 8 AS lob_kb,
    SUM(a.total_pages - a.used_pages) * 8 AS unused_kb,
    MIN(fg.name) AS filegroup
FROM   sys.partitions p
JOIN   sys.tables t ON t.object_id = p.object_id
JOIN   sys.allocation_units a ON a.container_id = p.partition_id
JOIN   sys.indexes i ON i.object_id = p.object_id AND i.index_id = p.index_id
LEFT   JOIN sys.indexes base ON base.object_id = p.object_id AND base.index_id IN (0,1)
LEFT   JOIN sys.filegroups fg ON fg.data_space_id = base.data_space_id
GROUP  BY p.object_id`

	rows, err := d.query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("gosmo: table space used on %q: %w", d.name, err)
	}
	defer rows.Close()

	out := make(map[int]*TableSpaceInfo)
	for rows.Next() {
		var objectID int
		var fg sql.NullString
		info := &TableSpaceInfo{}
		if err := rows.Scan(&objectID, &info.ReservedKB, &info.DataKB,
			&info.IndexKB, &info.LOBKB, &info.UnusedKB, &fg); err != nil {
			return nil, fmt.Errorf("gosmo: table space used on %q: %w", d.name, err)
		}
		info.FileGroup = fg.String
		out[objectID] = info
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("gosmo: table space used on %q: %w", d.name, err)
	}
	return out, nil
}
