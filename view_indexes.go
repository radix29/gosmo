package gosmo

import "context"

// -- Indexed views ---------------------------------------------------------------

// A view's indexes and statistics are a table's in every respect the catalog
// and the DDL care about: sys.indexes and sys.stats key them by the object's
// id, and CREATE/ALTER/DROP INDEX, CREATE/UPDATE/DROP STATISTICS and
// sp_rename name the object by its two-part name, whichever kind it is. So a
// view's reads and writes are a table's, run through a Table standing for the
// view (carrier), and what they return is re-parented onto the view: Index.View
// and Statistic.View answer it, and Table answers nil.
//
// What a view adds is the server's to enforce: it must be WITH SCHEMABINDING
// (IsSchemaBound) and its first index unique clustered; columnstore, XML and
// spatial indexes are table-only.

// carrier is a Table standing for the view, keyed by its ObjectID, Schema and
// Name. It is built fresh on each call, so a Rename or Transfer of the view is
// followed, and is never handed to a caller.
func (v *View) carrier() *Table {
	return &Table{db: v.db, ObjectID: v.ObjectID, Schema: v.Schema, Name: v.Name}
}

func (v *View) adoptIndex(idx *Index) *Index {
	if idx != nil {
		idx.table, idx.view = nil, v
	}
	return idx
}

func (v *View) adoptStatistic(st *Statistic) *Statistic {
	if st != nil {
		st.table, st.view = nil, v
	}
	return st
}

// Columns returns the view's columns in ordinal order — Table.Columns for a
// view. A ViewRef handle has no ObjectID and is refused with
// ErrHandleNotLoaded; Database.ObjectColumns reads a view by name instead.
func (v *View) Columns(ctx context.Context) ([]*Column, error) {
	return v.carrier().Columns(ctx)
}

// Indexes returns the indexed view's indexes with their columns — see
// Table.Indexes. A view that is not indexed has none.
func (v *View) Indexes(ctx context.Context) ([]*Index, error) {
	indexes, err := v.carrier().Indexes(ctx)
	for _, idx := range indexes {
		v.adoptIndex(idx)
	}
	return indexes, err
}

// IndexUsage returns the usage counters of the indexed view's indexes — see
// Table.IndexUsage. A view that is not indexed has none.
func (v *View) IndexUsage(ctx context.Context) ([]*IndexUsage, error) {
	return v.carrier().IndexUsage(ctx)
}

// IndexByName returns one index on the view by name, with its columns — see
// Table.IndexByName.
func (v *View) IndexByName(ctx context.Context, name string) (*Index, error) {
	idx, err := v.carrier().IndexByName(ctx, name)
	return v.adoptIndex(idx), err
}

// IndexRef returns a lightweight handle for name on the view without querying
// the server — see Table.IndexRef for what a handle can do.
func (v *View) IndexRef(name string) *Index {
	return &Index{view: v, Name: name}
}

// CreateIndex creates an index on the view — see Table.CreateIndex. The
// view's first index must be UNIQUE CLUSTERED, and the server refuses it on
// a view that is not schema-bound.
func (v *View) CreateIndex(ctx context.Context, req CreateIndexRequest) (*Index, error) {
	idx, err := v.carrier().CreateIndex(ctx, req)
	return v.adoptIndex(idx), err
}

// RebuildAllIndexes rebuilds every index on the view — see
// Table.RebuildAllIndexes.
func (v *View) RebuildAllIndexes(ctx context.Context, fillFactor int) error {
	return v.carrier().RebuildAllIndexes(ctx, fillFactor)
}

// Statistics returns the view's statistics objects — see Table.Statistics.
// Only an indexed view has any.
func (v *View) Statistics(ctx context.Context) ([]*Statistic, error) {
	stats, err := v.carrier().Statistics(ctx)
	for _, st := range stats {
		v.adoptStatistic(st)
	}
	return stats, err
}

// StatisticByName returns one statistics object on the view by name — see
// Table.StatisticByName.
func (v *View) StatisticByName(ctx context.Context, name string) (*Statistic, error) {
	st, err := v.carrier().StatisticByName(ctx, name)
	return v.adoptStatistic(st), err
}

// StatisticRef returns a lightweight handle for name on the view without
// querying the server — see Table.StatisticRef for what a handle can do.
func (v *View) StatisticRef(name string) *Statistic {
	return &Statistic{view: v, Name: name}
}

// CreateStatistic creates a user-defined statistic on the view — see
// Table.CreateStatistic. The server allows one only on an indexed view.
func (v *View) CreateStatistic(ctx context.Context, req CreateStatisticRequest) (*Statistic, error) {
	st, err := v.carrier().CreateStatistic(ctx, req)
	return v.adoptStatistic(st), err
}

// UpdateAllStatistics updates every statistic on the view — see
// Table.UpdateAllStatistics.
func (v *View) UpdateAllStatistics(ctx context.Context, samplePct int) error {
	return v.carrier().UpdateAllStatistics(ctx, samplePct)
}
