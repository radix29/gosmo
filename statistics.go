package gosmo

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// ============================================================
// Statistics
// ============================================================

// Statistic mirrors sys.stats for a table.
type Statistic struct {
	table          *Table
	Name           string
	StatID         int
	IsAutoCreated  bool
	IsUserCreated  bool
	HasFilter      bool
	FilterDef      string
	LastUpdated    time.Time
	RowsSampled    int64
	TotalRows      int64 // renamed from RowCount to avoid shadowing Table.RowCount()
	Steps          int
	UnfilteredRows int64
}

// Statistics returns all statistics objects for the table.
func (t *Table) Statistics() ([]*Statistic, error) {
	return t.StatisticsContext(context.Background())
}

// StatisticsContext is the context-aware variant of Statistics.
func (t *Table) StatisticsContext(ctx context.Context) ([]*Statistic, error) {
	const q = `
SELECT s.name, s.stats_id,
       s.auto_created, s.user_created,
       s.has_filter, ISNULL(s.filter_definition, ''),
       sp.last_updated, sp.rows_sampled, sp.rows,
       sp.steps, sp.unfiltered_rows
FROM   sys.stats s
CROSS  APPLY sys.dm_db_stats_properties(s.object_id, s.stats_id) sp
WHERE  s.object_id = @p1
ORDER  BY s.name`

	rows, err := t.db.query(ctx, q, t.ObjectID)
	if err != nil {
		return nil, fmt.Errorf("gosmo: statistics for %s: %w", t.FullName(), err)
	}
	defer rows.Close()

	var stats []*Statistic
	for rows.Next() {
		st := &Statistic{table: t}
		var lastUpdated sql.NullTime
		var rowsSampled, totalRows, unfiltered sql.NullInt64
		var steps sql.NullInt32
		if err := rows.Scan(
			&st.Name, &st.StatID,
			&st.IsAutoCreated, &st.IsUserCreated,
			&st.HasFilter, &st.FilterDef,
			&lastUpdated, &rowsSampled, &totalRows,
			&steps, &unfiltered,
		); err != nil {
			return nil, err
		}
		if lastUpdated.Valid {
			st.LastUpdated = lastUpdated.Time
		}
		st.RowsSampled = rowsSampled.Int64
		st.TotalRows = totalRows.Int64
		st.Steps = int(steps.Int32)
		st.UnfilteredRows = unfiltered.Int64
		stats = append(stats, st)
	}
	return stats, rows.Err()
}

// Update updates this statistic.
// Pass samplePct=0 for a FULLSCAN; any value 1-100 uses SAMPLE n PERCENT.
func (st *Statistic) Update(samplePct int) error {
	return st.UpdateContext(context.Background(), samplePct)
}

func (st *Statistic) UpdateContext(ctx context.Context, samplePct int) error {
	option := "FULLSCAN"
	if samplePct > 0 {
		option = fmt.Sprintf("SAMPLE %d PERCENT", samplePct)
	}
	// UPDATE STATISTICS does not support parameterised stat names.
	q := fmt.Sprintf("UPDATE STATISTICS %s %s WITH %s",
		st.table.FullName(), quoteIdent(st.Name), option)
	if _, err := st.table.db.exec(ctx, q); err != nil {
		return fmt.Errorf("gosmo: update statistic %q: %w", st.Name, err)
	}
	return nil
}

// Drop drops this statistic.
// Correct T-SQL syntax: DROP STATISTICS table_name.stat_name
func (st *Statistic) Drop() error {
	return st.DropContext(context.Background())
}

func (st *Statistic) DropContext(ctx context.Context) error {
	// DROP STATISTICS syntax: schema.table.stat (not quoted as one unit)
	q := fmt.Sprintf("DROP STATISTICS %s.%s.%s",
		quoteIdent(st.table.Schema), quoteIdent(st.table.Name), quoteIdent(st.Name))
	if _, err := st.table.db.exec(ctx, q); err != nil {
		return fmt.Errorf("gosmo: drop statistic %q: %w", st.Name, err)
	}
	return nil
}

// UpdateAllStatistics updates all statistics on the table.
func (t *Table) UpdateAllStatistics(samplePct int) error {
	return t.UpdateAllStatisticsContext(context.Background(), samplePct)
}

func (t *Table) UpdateAllStatisticsContext(ctx context.Context, samplePct int) error {
	option := "FULLSCAN"
	if samplePct > 0 {
		option = fmt.Sprintf("SAMPLE %d PERCENT", samplePct)
	}
	if _, err := t.db.exec(ctx, fmt.Sprintf("UPDATE STATISTICS %s WITH %s", t.FullName(), option)); err != nil {
		return fmt.Errorf("gosmo: update all statistics on %s: %w", t.FullName(), err)
	}
	return nil
}

// CreateStatistic creates a user-defined statistic on one or more columns.
func (t *Table) CreateStatistic(name string, columns []string, samplePct int) error {
	return t.CreateStatisticContext(context.Background(), name, columns, samplePct)
}

func (t *Table) CreateStatisticContext(ctx context.Context, name string, columns []string, samplePct int) error {
	if name == "" {
		return fmt.Errorf("gosmo: create statistic: name is required")
	}
	if len(columns) == 0 {
		return fmt.Errorf("gosmo: create statistic: at least one column required")
	}
	quotedCols := make([]string, len(columns))
	for i, c := range columns {
		quotedCols[i] = quoteIdent(c)
	}
	q := fmt.Sprintf("CREATE STATISTICS %s ON %s (%s)",
		quoteIdent(name), t.FullName(), strings.Join(quotedCols, ", "))
	if samplePct > 0 {
		q += fmt.Sprintf(" WITH SAMPLE %d PERCENT", samplePct)
	}
	if _, err := t.db.exec(ctx, q); err != nil {
		return fmt.Errorf("gosmo: create statistic %q: %w", name, err)
	}
	return nil
}
