package gosmo

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"strings"
)

// partitionBoundaryPattern matches a well-formed SQL Server literal: a
// signed integer or decimal (with an optional exponent, the form a float
// boundary reads back as), a hex literal, a properly quoted (and
// escaped) string/date literal, or NULL.
var partitionBoundaryPattern = regexp.MustCompile(`(?i)^(-?\d+(\.\d+)?(e[+-]?\d+)?|0x[0-9a-f]+|n?'(?:[^']|'')*'|null)$`)

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
	db         *Database
	Name       string
	FunctionID int
	InputType  DataType
	// MaxLength, Precision and Scale qualify InputType the way sys.columns'
	// columns of the same names qualify a column's type — a datetime2(0) or
	// nvarchar(20) partitioning function is not the bare type.
	MaxLength     int
	Precision     int
	Scale         int
	BoundaryCount int
	IsRight       bool // RIGHT = boundary is in right partition
	// Boundaries are T-SQL literals in boundary order, ready to splice into
	// a VALUES, SPLIT RANGE or MERGE RANGE clause: 100, N'2026-01-01',
	// 0x0A, NULL. Each is rendered from the stored value's own base type
	// (see partitionBoundaryLiteral), the same form
	// CreatePartitionFunctionRequest.Boundaries takes.
	Boundaries []string
}

// Database returns the database the partition function belongs to.
func (pf *PartitionFunction) Database() *Database { return pf.db }

// partitionFunctionSelect is the column list and joins every partition
// function read shares; the listing adds ORDER BY, the by-name lookup a
// WHERE.
var partitionFunctionSelect = `
SELECT pf.name, pf.function_id, pf.fanout - 1,
       tp.name AS input_type, pp.max_length, pp.precision, pp.scale,
       pf.boundary_value_on_right,
       ` + jsonRows(`b.t, CASE
                 -- Style 1 keeps the 0x prefix. Style 126 — right for a
                 -- date — is refused for varbinary (Msg 9809), and that
                 -- error failed every partition function in the database.
                 WHEN b.t IN (N'binary', N'varbinary')
                      THEN CONVERT(nvarchar(max), CONVERT(varbinary(max), prv.value), 1)
                 -- Style 126 (ISO 8601): the default conversion yields
                 -- "Jan  1 2026", which loses any time part.
                 WHEN b.t IN (N'date', N'time', N'datetime', N'datetime2',
                              N'smalldatetime', N'datetimeoffset')
                      THEN CONVERT(nvarchar(max), prv.value, 126)
                 -- Style 3 is float's lossless 17 digits; the default
                 -- rounds to 6.
                 WHEN b.t IN (N'float', N'real')
                      THEN CONVERT(nvarchar(max), CONVERT(float, prv.value), 3)
                 -- Style 2 keeps money's 4 decimals; the default keeps 2.
                 WHEN b.t IN (N'money', N'smallmoney')
                      THEN CONVERT(nvarchar(max), CONVERT(money, prv.value), 2)
                 ELSE CONVERT(nvarchar(max), prv.value)
               END AS v`, `
        FROM   sys.partition_range_values prv
        CROSS  APPLY (SELECT CONVERT(nvarchar(128),
                        SQL_VARIANT_PROPERTY(prv.value, 'BaseType')) AS t) b
        WHERE  prv.function_id = pf.function_id`, "prv.boundary_id") + ` AS boundaries
FROM   sys.partition_functions pf
JOIN   sys.partition_parameters pp ON pp.function_id = pf.function_id
JOIN   sys.types tp ON tp.user_type_id = pp.user_type_id`

// PartitionFunctions returns all partition functions in the database.
func (d *Database) PartitionFunctions(ctx context.Context) ([]*PartitionFunction, error) {
	rows, err := d.query(ctx, partitionFunctionSelect+`
ORDER  BY pf.name`)
	return scanRows(rows, err, "list partition functions", func(scan func(...any) error) (*PartitionFunction, error) {
		return scanPartitionFunction(d, scan)
	})
}

// PartitionFunctionByName returns one partition function by name.
func (d *Database) PartitionFunctionByName(ctx context.Context, name string) (*PartitionFunction, error) {
	var pf *PartitionFunction
	err := d.queryRow(ctx, func(row *sql.Row) error {
		var err error
		pf, err = scanPartitionFunction(d, row.Scan)
		return err
	}, partitionFunctionSelect+`
WHERE  pf.name = @p1`, name)
	return foundRow(pf, err, notFoundf("gosmo: partition function %q not found in %q", name, d.Name), fmt.Sprintf("find partition function %q in %q", name, d.Name))
}

func scanPartitionFunction(d *Database, scan func(...any) error) (*PartitionFunction, error) {
	pf := &PartitionFunction{db: d}
	var boundaries sql.NullString
	if err := scan(&pf.Name, &pf.FunctionID, &pf.BoundaryCount,
		&pf.InputType, &pf.MaxLength, &pf.Precision, &pf.Scale,
		&pf.IsRight, &boundaries); err != nil {
		return nil, err
	}
	values, err := decodeJSONRows[partitionBoundary](boundaries)
	if err != nil {
		return nil, err
	}
	for _, b := range values {
		pf.Boundaries = append(pf.Boundaries, partitionBoundaryLiteral(b.BaseType, b.Value))
	}
	return pf, nil
}

// partitionBoundary is one sys.partition_range_values row as
// partitionFunctionSelect's boundary list renders it: the sql_variant's base
// type, and its value as text in the style that type needs.
type partitionBoundary struct {
	BaseType string `json:"t"`
	Value    string `json:"v"`
}

// partitionBoundaryLiteral renders one boundary as a T-SQL literal. It goes
// by the stored value's base type, not the function's input type: the value
// is what was converted to text, so its type is what says how to read the
// text back. A NULL boundary has neither, since FOR JSON leaves out a NULL.
func partitionBoundaryLiteral(baseType, value string) string {
	switch baseType {
	case "":
		return "NULL"
	case "bigint", "int", "smallint", "tinyint", "bit",
		"decimal", "numeric", "float", "real", "money", "smallmoney",
		"binary", "varbinary":
		return value
	}
	return "N'" + escapeSingle(value) + "'"
}

// CreatePartitionFunctionRequest describes a partition function to create.
type CreatePartitionFunctionRequest struct {
	Name       string
	InputType  DataType
	IsRight    bool
	Boundaries []string // T-SQL literals, e.g. {"100","200","300"} or {"N'2026-01-01'"}
}

// CreatePartitionFunction creates a partition function.
func (d *Database) CreatePartitionFunction(ctx context.Context, req CreatePartitionFunctionRequest) error {
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
func (pf *PartitionFunction) Drop(ctx context.Context) error {
	_, err := pf.db.exec(ctx,
		fmt.Sprintf("DROP PARTITION FUNCTION %s", quoteIdent(pf.Name)))
	if err != nil {
		return fmt.Errorf("gosmo: drop partition function [%s]: %w", pf.Name, err)
	}
	return nil
}

// SplitRange adds a new boundary value to the partition function.
func (pf *PartitionFunction) SplitRange(ctx context.Context, value string) error {
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
func (pf *PartitionFunction) MergeRange(ctx context.Context, value string) error {
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

// Database returns the database the partition scheme belongs to.
func (ps *PartitionScheme) Database() *Database { return ps.db }

// partitionSchemeSelect is the column list and joins every partition
// scheme read shares; the listing adds ORDER BY, the by-name lookup a WHERE.
var partitionSchemeSelect = `
SELECT ps.name, ps.data_space_id, pf.name AS func_name,
       ` + jsonList("fg.name", `
        FROM sys.destination_data_spaces dds
        JOIN sys.filegroups fg ON fg.data_space_id = dds.data_space_id
        WHERE dds.partition_scheme_id = ps.data_space_id`, "dds.destination_id") + ` AS filegroups
FROM   sys.partition_schemes ps
JOIN   sys.partition_functions pf ON pf.function_id = ps.function_id`

// PartitionSchemes returns all partition schemes in the database.
func (d *Database) PartitionSchemes(ctx context.Context) ([]*PartitionScheme, error) {
	rows, err := d.query(ctx, partitionSchemeSelect+`
ORDER  BY ps.name`)
	return scanRows(rows, err, "list partition schemes", func(scan func(...any) error) (*PartitionScheme, error) {
		return scanPartitionScheme(d, scan)
	})
}

// PartitionSchemeByName returns one partition scheme by name.
func (d *Database) PartitionSchemeByName(ctx context.Context, name string) (*PartitionScheme, error) {
	var ps *PartitionScheme
	err := d.queryRow(ctx, func(row *sql.Row) error {
		var err error
		ps, err = scanPartitionScheme(d, row.Scan)
		return err
	}, partitionSchemeSelect+`
WHERE  ps.name = @p1`, name)
	return foundRow(ps, err, notFoundf("gosmo: partition scheme %q not found in %q", name, d.Name), fmt.Sprintf("find partition scheme %q in %q", name, d.Name))
}

func scanPartitionScheme(d *Database, scan func(...any) error) (*PartitionScheme, error) {
	ps := &PartitionScheme{db: d}
	var fgs sql.NullString
	if err := scan(&ps.Name, &ps.SchemeID, &ps.FunctionName, &fgs); err != nil {
		return nil, err
	}
	var err error
	if ps.FileGroups, err = decodeJSONList(fgs); err != nil {
		return nil, err
	}
	return ps, nil
}

// CreatePartitionScheme creates a partition scheme backed by a partition function.
func (d *Database) CreatePartitionScheme(ctx context.Context, name, functionName string, fileGroups []string) error {
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
func (ps *PartitionScheme) Drop(ctx context.Context) error {
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
//
// A non-partitioned table still returns exactly one row (partition number 1),
// same as sys.partitions itself.
func (t *Table) Partitions(ctx context.Context) ([]*PartitionInfo, error) {
	const q = `
SELECT p.partition_number, p.rows, p.data_compression_desc
FROM   sys.partitions p
WHERE  p.object_id = @p1 AND p.index_id IN (0,1)
ORDER  BY p.partition_number`

	rows, err := t.db.query(ctx, q, t.ObjectID)
	return scanRows(rows, err, fmt.Sprintf("partitions for %s", t.FullName()), func(scan func(...any) error) (*PartitionInfo, error) {
		p := &PartitionInfo{}
		if err := scan(&p.PartitionNumber, &p.Rows, &p.DataCompression); err != nil {
			return nil, err
		}
		return p, nil
	})
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
func (t *Table) SpaceUsed(ctx context.Context) (*TableSpaceInfo, error) {
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
func (d *Database) TableSpaceUsedAll(ctx context.Context) (map[int]*TableSpaceInfo, error) {
	// Same joins and aggregates as Table.SpaceUsed, grouped by object
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
		return nil, fmt.Errorf("gosmo: table space used on %q: %w", d.Name, err)
	}
	defer rows.Close()

	out := make(map[int]*TableSpaceInfo)
	for rows.Next() {
		var objectID int
		var fg sql.NullString
		info := &TableSpaceInfo{}
		if err := rows.Scan(&objectID, &info.ReservedKB, &info.DataKB,
			&info.IndexKB, &info.LOBKB, &info.UnusedKB, &fg); err != nil {
			return nil, fmt.Errorf("gosmo: table space used on %q: %w", d.Name, err)
		}
		info.FileGroup = fg.String
		out[objectID] = info
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("gosmo: table space used on %q: %w", d.Name, err)
	}
	return out, nil
}
