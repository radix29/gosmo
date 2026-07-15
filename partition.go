package gosmo

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

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

// PartitionFunctions returns all partition functions in the database.
func (d *Database) PartitionFunctions() ([]*PartitionFunction, error) {
	const q = `
SELECT pf.name, pf.function_id, pf.fanout - 1,
       tp.name AS input_type, pf.boundary_value_on_right,
       (SELECT STRING_AGG(CAST(prv.value AS NVARCHAR(256)), ',')
        WITHIN GROUP (ORDER BY prv.boundary_id)
        FROM sys.partition_range_values prv
        WHERE prv.function_id = pf.function_id) AS boundaries
FROM   sys.partition_functions pf
JOIN   sys.partition_parameters pp ON pp.function_id = pf.function_id
JOIN   sys.types tp ON tp.user_type_id = pp.user_type_id
ORDER  BY pf.name`

	rows, err := d.query(context.Background(), q)
	if err != nil {
		return nil, fmt.Errorf("gosmo: list partition functions: %w", err)
	}
	defer rows.Close()

	var funcs []*PartitionFunction
	for rows.Next() {
		pf := &PartitionFunction{db: d}
		var boundaries sql.NullString
		if err := rows.Scan(&pf.Name, &pf.FunctionID, &pf.BoundaryCount,
			&pf.InputType, &pf.IsRight, &boundaries); err != nil {
			return nil, err
		}
		if boundaries.Valid && boundaries.String != "" {
			pf.Boundaries = strings.Split(boundaries.String, ",")
		}
		funcs = append(funcs, pf)
	}
	return funcs, rows.Err()
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
	if len(req.Boundaries) == 0 {
		return fmt.Errorf("gosmo: create partition function: at least one boundary required")
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
	_, err := d.exec(context.Background(), q)
	if err != nil {
		return fmt.Errorf("gosmo: create partition function [%s]: %w", req.Name, err)
	}
	return nil
}

// Drop drops the partition function.
func (pf *PartitionFunction) Drop() error {
	_, err := pf.db.exec(context.Background(),
		fmt.Sprintf("DROP PARTITION FUNCTION %s", quoteIdent(pf.Name)))
	if err != nil {
		return fmt.Errorf("gosmo: drop partition function [%s]: %w", pf.Name, err)
	}
	return nil
}

// SplitRange adds a new boundary value to the partition function.
func (pf *PartitionFunction) SplitRange(value string) error {
	_, err := pf.db.exec(context.Background(),
		fmt.Sprintf("ALTER PARTITION FUNCTION %s() SPLIT RANGE (%s)", quoteIdent(pf.Name), value))
	if err != nil {
		return fmt.Errorf("gosmo: split range on [%s]: %w", pf.Name, err)
	}
	return nil
}

// MergeRange removes a boundary value from the partition function.
func (pf *PartitionFunction) MergeRange(value string) error {
	_, err := pf.db.exec(context.Background(),
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

// PartitionSchemes returns all partition schemes in the database.
func (d *Database) PartitionSchemes() ([]*PartitionScheme, error) {
	const q = `
SELECT ps.name, ps.data_space_id, pf.name AS func_name,
       (SELECT STRING_AGG(fg.name, ',') WITHIN GROUP (ORDER BY dds.destination_id)
        FROM sys.destination_data_spaces dds
        JOIN sys.filegroups fg ON fg.data_space_id = dds.data_space_id
        WHERE dds.partition_scheme_id = ps.data_space_id) AS filegroups
FROM   sys.partition_schemes ps
JOIN   sys.partition_functions pf ON pf.function_id = ps.function_id
ORDER  BY ps.name`

	rows, err := d.query(context.Background(), q)
	if err != nil {
		return nil, fmt.Errorf("gosmo: list partition schemes: %w", err)
	}
	defer rows.Close()

	var schemes []*PartitionScheme
	for rows.Next() {
		ps := &PartitionScheme{db: d}
		var fgs sql.NullString
		if err := rows.Scan(&ps.Name, &ps.SchemeID, &ps.FunctionName, &fgs); err != nil {
			return nil, err
		}
		if fgs.Valid && fgs.String != "" {
			ps.FileGroups = strings.Split(fgs.String, ",")
		}
		schemes = append(schemes, ps)
	}
	return schemes, rows.Err()
}

// CreatePartitionScheme creates a partition scheme backed by a partition function.
func (d *Database) CreatePartitionScheme(name, functionName string, fileGroups []string) error {
	if len(fileGroups) == 0 {
		return fmt.Errorf("gosmo: create partition scheme: at least one filegroup required")
	}
	fgs := make([]string, len(fileGroups))
	for i, fg := range fileGroups {
		fgs[i] = fmt.Sprintf("[%s]", fg)
	}
	q := fmt.Sprintf(
		"CREATE PARTITION SCHEME %s AS PARTITION %s TO (%s)",
		quoteIdent(name), quoteIdent(functionName), strings.Join(fgs, ", "),
	)
	_, err := d.exec(context.Background(), q)
	if err != nil {
		return fmt.Errorf("gosmo: create partition scheme [%s]: %w", name, err)
	}
	return nil
}

// Drop drops the partition scheme.
func (ps *PartitionScheme) Drop() error {
	_, err := ps.db.exec(context.Background(),
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
			return nil, err
		}
		parts = append(parts, p)
	}
	return parts, rows.Err()
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

	row, release, err := t.db.queryRow(ctx, q, t.ObjectID)
	if err != nil {
		return nil, err
	}
	defer release()

	info := &TableSpaceInfo{}
	var fg sql.NullString
	if err := row.Scan(&info.ReservedKB, &info.DataKB, &info.IndexKB, &info.LOBKB, &info.UnusedKB, &fg); err != nil {
		return nil, fmt.Errorf("gosmo: space used for %s: %w", t.FullName(), err)
	}
	info.FileGroup = fg.String
	return info, nil
}
