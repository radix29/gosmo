package gosmo

import (
	"context"
	"fmt"
)

// tablePrefetch is what ScriptTable reads per table — options, columns,
// indexes with their columns, foreign keys, check constraints, the data
// space, user-created statistics and (2019+) edge constraints — read for every table of a database in one round trip, keyed by
// object_id. ScriptDatabaseObjectList scripts every table of a database, and
// ten reads each made the loop's cost a function of the table count, over a
// connection per read.
//
// A table with a key in options is covered; any other (one created since the
// prefetch) is read the ordinary way. A covered table's absent keys elsewhere
// mean "none", the same answer the per-table query gives.
type tablePrefetch struct {
	options map[int]tableScriptOptions
	cols    map[int][]*Column
	indexes map[int][]*Index
	fks     map[int][]*ForeignKey
	checks  map[int][]*CheckConstraint
	ds      map[int]DataSpace
	ecs     map[int][]*EdgeConstraint
	// stats is filled only for a Scripter whose options script statistics
	// (scriptedStatistics).
	stats map[int][]scriptedStatistic
}

// The batch reads every table in the database, so it also reads the ones the
// loop leaves out (a system-versioned table's history table): rows for an
// object that is not in the list are read past and dropped.

// allTables is the object predicate that selects every table of the database
// in the *With select builders.
const allTables = "IN (SELECT object_id FROM sys.tables)"

// prefetchTables reads the script inputs of tables in one batch of seven
// result sets on one connection (one more for statistics, and one more from
// SQL Server 2019, whose sys.edge_constraints adds the edge constraints), and
// nothing when there are none.
//
// withStats adds the user-created statistics (ScriptOptions.Statistics) as one
// more result set before the edge constraints.
func (d *Database) prefetchTables(ctx context.Context, tables []*Table, withStats bool) (*tablePrefetch, error) {
	if len(tables) == 0 {
		return nil, nil
	}
	byID := make(map[int]*Table, len(tables))
	for _, t := range tables {
		byID[t.ObjectID] = t
	}
	probe := &Table{db: d}
	p := &tablePrefetch{
		options: map[int]tableScriptOptions{},
		cols:    map[int][]*Column{},
		indexes: map[int][]*Index{},
		fks:     map[int][]*ForeignKey{},
		checks:  map[int][]*CheckConstraint{},
		ds:      map[int]DataSpace{},
		ecs:     map[int][]*EdgeConstraint{},
		stats:   map[int][]scriptedStatistic{},
	}
	idxCols := map[int]map[int][]IndexColumn{}

	q := probe.scriptOptionsSelectWith(", t.object_id", allTables) + ";\n" +
		d.columnSelectWith(", c.object_id") + "\nWHERE  c.object_id " + allTables + "\nORDER  BY c.column_id;\n" +
		probe.indexListSelectWith(", i.object_id", allTables) + "\nORDER  BY i.index_id;\n" +
		probe.indexColumnsSelectWith(", ic.object_id", allTables) + indexColumnsTail + ";\n" +
		foreignKeySelectWith(", fk.parent_object_id", allTables) + "\nORDER  BY fk.name;\n" +
		checkConstraintSelect(", cc.parent_object_id", allTables) + ";\n" +
		dataSpaceSelect(", i.object_id", allTables)
	if withStats {
		q += ";\n" + userStatisticsSelect(allTables)
	}
	withEdges := d.atLeast(SQLServer2019)
	if withEdges {
		q += ";\n" + edgeConstraintSelect(", ec.parent_object_id", allTables)
	}

	rows, err := d.query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("gosmo: read table scripts for %q: %w", d.Name, err)
	}
	defer rows.Close()

	reads := []func() error{
		func() error {
			for rows.Next() {
				var id int
				// The scan needs the table's kind flags to start from; they
				// come from the object_id, which is the last column.
				o := tableScriptOptions{}
				if err := scanTableScriptOptions(rows.Scan, &o, &id); err != nil {
					return err
				}
				t := byID[id]
				if t == nil {
					continue
				}
				o.IsNode, o.IsEdge, o.IsMemoryOptimized = t.IsNode, t.IsEdge, t.IsMemoryOptimized
				o.IsFileTable, o.IsExternal = t.IsFileTable, t.IsExternal
				p.options[id] = o
			}
			return rows.Err()
		},
		func() error {
			for rows.Next() {
				var id int
				c, err := scanColumn(rows.Scan, &id)
				if err != nil {
					return err
				}
				p.cols[id] = append(p.cols[id], c)
			}
			return rows.Err()
		},
		func() error {
			for rows.Next() {
				var id int
				// The owner is not known until the row is read, and the
				// Index wants it for its back-pointer: set it afterwards.
				idx, err := scanIndex(rows.Scan, nil, &id)
				if err != nil {
					return err
				}
				if t := byID[id]; t != nil {
					idx.table = t
					p.indexes[id] = append(p.indexes[id], idx)
				}
			}
			return rows.Err()
		},
		func() error {
			for rows.Next() {
				var id, indexID int
				c := IndexColumn{}
				if err := rows.Scan(&indexID, &c.Name, &c.Descending, &c.IsIncluded, &c.orderOrdinal, &id); err != nil {
					return err
				}
				if idxCols[id] == nil {
					idxCols[id] = map[int][]IndexColumn{}
				}
				idxCols[id][indexID] = append(idxCols[id][indexID], c)
			}
			return rows.Err()
		},
		func() error {
			for rows.Next() {
				var id int
				fk, err := scanForeignKey(rows.Scan, &id)
				if err != nil {
					return err
				}
				p.fks[id] = append(p.fks[id], fk)
			}
			return rows.Err()
		},
		func() error {
			for rows.Next() {
				var id int
				cc, err := scanCheckConstraint(rows.Scan, &id)
				if err != nil {
					return err
				}
				p.checks[id] = append(p.checks[id], cc)
			}
			return rows.Err()
		},
		func() error {
			for rows.Next() {
				var ds DataSpace
				var id int
				if err := rows.Scan(&ds.Name, &ds.IsPartitionScheme, &ds.IsDefaultFileGroup, &ds.PartitionColumn, &id); err != nil {
					return err
				}
				p.ds[id] = ds
			}
			return rows.Err()
		},
	}
	if withStats {
		reads = append(reads, func() error {
			stats, err := scanUserStatistics(rows.Rows, func(id int) *Table { return byID[id] })
			p.stats = stats
			return err
		})
	}
	if withEdges {
		reads = append(reads, func() error {
			for rows.Next() {
				var id int
				ec, err := scanEdgeConstraint(rows.Scan, &id)
				if err != nil {
					return err
				}
				p.ecs[id] = append(p.ecs[id], ec)
			}
			return rows.Err()
		})
	}
	for i, read := range reads {
		if i > 0 && !rows.NextResultSet() {
			err := rows.Err()
			if err == nil {
				err = fmt.Errorf("result set %d of %d is missing", i+1, len(reads))
			}
			return nil, fmt.Errorf("gosmo: read table scripts for %q: %w", d.Name, err)
		}
		if err := read(); err != nil {
			return nil, fmt.Errorf("gosmo: read table scripts for %q: %w", d.Name, err)
		}
	}
	for id, idxs := range p.indexes {
		distributeIndexColumns(idxs, idxCols[id])
	}
	return p, nil
}
