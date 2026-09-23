package gosmo

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

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
	DataCompression DataCompression
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
func (t *Table) CreateIndex(ctx context.Context, req CreateIndexRequest) error {
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
// why the request cannot be one. Separated from CreateIndex so the
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
