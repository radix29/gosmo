package gosmo

// Table kinds — the families SSMS files into their own folders under Tables:
// System Tables, FileTables, External Tables and Graph Tables, with the plain
// user tables listed beside them.
//
// Every one of them is an ordinary row in sys.tables distinguished by a flag,
// so Tables/TablesFiltered keep returning all of them: the catalog lists them
// and a listing that silently omitted one would disagree with the catalog.
// TablesOfKind is what a caller building a *tree* asks instead, so each table
// appears exactly once, under its own folder.

import (
	"context"
	"database/sql"
	"fmt"
)

// TableKind selects one family of tables. TableKindUser is the residue: a
// table that is none of the other four, which is what belongs directly under
// the Tables folder once the sub-folders have taken their own.
type TableKind int

const (
	TableKindUser TableKind = iota
	TableKindSystem
	TableKindFileTable
	TableKindExternal
	TableKindGraph
)

// String names the kind for error messages.
func (k TableKind) String() string {
	switch k {
	case TableKindUser:
		return "user"
	case TableKindSystem:
		return "system"
	case TableKindFileTable:
		return "filetable"
	case TableKindExternal:
		return "external"
	case TableKindGraph:
		return "graph"
	}
	return fmt.Sprintf("TableKind(%d)", int(k))
}

// graphPredicate is the "this row is a graph table" test, gated: is_node and
// is_edge are SQL Server 2017 columns, absent from sys.tables on 2016, where
// naming either would fail the whole read rather than the one predicate.
// Confirmed on the catalog itself — a 13.0.6500.1 instance has neither and a
// 14.0.2130.4 has both.
//
// Substituting a zero bit makes the predicate false for every row there,
// which is the truth: an instance with no graph columns has no graph tables.
func graphPredicate(major int) string {
	return "(" + colSince(major, SQLServer2017, "t.is_node", "CAST(0 AS bit)") + " = 1 OR " +
		colSince(major, SQLServer2017, "t.is_edge", "CAST(0 AS bit)") + " = 1)"
}

// kindClause is the WHERE fragment selecting one kind, including the
// is_ms_shipped test — which is part of the kind and not a separate filter:
// System Tables *is* the ms-shipped family, and every other kind excludes it.
func kindClause(major int, k TableKind) string {
	switch k {
	case TableKindSystem:
		return "AND t.is_ms_shipped = 1"
	case TableKindFileTable:
		return "AND t.is_ms_shipped = 0 AND t.is_filetable = 1"
	case TableKindExternal:
		return "AND t.is_ms_shipped = 0 AND t.is_external = 1"
	case TableKindGraph:
		return "AND t.is_ms_shipped = 0 AND " + graphPredicate(major)
	default: // TableKindUser
		return "AND t.is_ms_shipped = 0 AND t.is_filetable = 0 AND t.is_external = 0" +
			" AND NOT " + graphPredicate(major)
	}
}

// requireGraphTables refuses a graph-table listing on an instance whose
// sys.tables has no is_node/is_edge at all — SQL Server 2016 and older.
// The read would otherwise succeed and return nothing, which reads as "this
// database has no graph tables" rather than "this server cannot have any".
func (d *Database) requireGraphTables() error {
	if major := d.serverMajorVersion(); major != 0 && major < int(SQLServer2017) {
		return unsupportedVersionf(
			"gosmo: graph tables need SQL Server 2017 or later; this instance is major %d", major)
	}
	return nil
}

// TablesOfKind returns the tables of one kind.
func (d *Database) TablesOfKind(kind TableKind) ([]*Table, error) {
	return d.TablesOfKindContext(context.Background(), kind)
}

// TablesOfKindContext is the context-aware variant of TablesOfKind.
func (d *Database) TablesOfKindContext(ctx context.Context, kind TableKind) ([]*Table, error) {
	return d.TablesOfKindFilteredContext(ctx, kind, ObjectFilter{})
}

// TablesOfKindFiltered returns the tables of one kind an ObjectFilter
// matches, narrowed by the server. An empty filter is TablesOfKind.
func (d *Database) TablesOfKindFiltered(kind TableKind, filter ObjectFilter) ([]*Table, error) {
	return d.TablesOfKindFilteredContext(context.Background(), kind, filter)
}

// TablesOfKindFilteredContext is the context-aware variant of
// TablesOfKindFiltered.
//
// TableKindGraph is refused below SQL Server 2017 (errors.Is
// ErrUnsupportedVersion) rather than answered with an empty list.
func (d *Database) TablesOfKindFilteredContext(ctx context.Context, kind TableKind, filter ObjectFilter) ([]*Table, error) {
	if kind == TableKindGraph {
		if err := d.requireGraphTables(); err != nil {
			return nil, err
		}
	}
	where, args := filter.clause(tableFilterColumns, 1)
	return d.tablesWhere(ctx, kindClause(d.serverMajorVersion(), kind)+" "+where, args)
}

// TableKindPresence reports which table families a database actually has —
// what a tree builder needs to decide which sub-folders to show, in one
// query rather than one listing per folder.
//
// Graph is false on an instance older than SQL Server 2017 for the same
// reason TablesOfKind refuses the listing there: the columns do not exist,
// so no row can be one.
type TableKindPresence struct {
	System    bool
	FileTable bool
	External  bool
	Graph     bool
}

// TableKindsPresent reports which table families the database has.
func (d *Database) TableKindsPresent() (TableKindPresence, error) {
	return d.TableKindsPresentContext(context.Background())
}

// TableKindsPresentContext is the context-aware variant of
// TableKindsPresent.
func (d *Database) TableKindsPresentContext(ctx context.Context) (TableKindPresence, error) {
	q := `
SELECT CAST(MAX(CASE WHEN t.is_ms_shipped = 1 THEN 1 ELSE 0 END) AS bit),
       CAST(MAX(CASE WHEN t.is_ms_shipped = 0 AND t.is_filetable = 1 THEN 1 ELSE 0 END) AS bit),
       CAST(MAX(CASE WHEN t.is_ms_shipped = 0 AND t.is_external  = 1 THEN 1 ELSE 0 END) AS bit),
       CAST(MAX(CASE WHEN t.is_ms_shipped = 0 AND ` + graphPredicate(d.serverMajorVersion()) + ` THEN 1 ELSE 0 END) AS bit)
FROM   sys.tables t`

	var p TableKindPresence
	// MAX over no rows is NULL, not 0 — a database with no tables at all would
	// fail a scan straight into a bool, so each column is scanned as a
	// NullBool and read as false.
	err := d.queryRow(ctx, func(row *sql.Row) error {
		var sys, ft, ext, graph sql.NullBool
		if err := row.Scan(&sys, &ft, &ext, &graph); err != nil {
			return err
		}
		p = TableKindPresence{System: sys.Bool, FileTable: ft.Bool, External: ext.Bool, Graph: graph.Bool}
		return nil
	}, q)
	if err != nil {
		return TableKindPresence{}, fmt.Errorf("gosmo: read table kinds in %q: %w", d.name, err)
	}
	return p, nil
}
