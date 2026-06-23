package gosmo

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// ============================================================
// Statistics
// ============================================================

// Statistic mirrors sys.stats for a table.
type Statistic struct {
	table         *Table
	Name          string
	StatID        int
	IsAutoCreated bool
	IsUserCreated bool
	HasFilter     bool
	FilterDef     string
	LastUpdated   time.Time
	RowCount      int64
	RowsSampled   int64
	Steps         int
	Unfiltered    int64
}

// Statistics returns all statistics objects for the table.
func (t *Table) Statistics() ([]*Statistic, error) {
	const q = `
SELECT s.name, s.stats_id,
       s.auto_created, s.user_created,
       s.has_filter, ISNULL(s.filter_definition,''),
       sp.last_updated, sp.rows, sp.rows_sampled,
       sp.steps, sp.unfiltered_rows
FROM   sys.stats s
CROSS  APPLY sys.dm_db_stats_properties(s.object_id, s.stats_id) sp
WHERE  s.object_id = ?
ORDER  BY s.name`

	rows, err := t.db.query(context.Background(), q, t.ObjectID)
	if err != nil {
		return nil, fmt.Errorf("gosmo: statistics for %s: %w", t.FullName(), err)
	}
	defer rows.Close()

	var stats []*Statistic
	for rows.Next() {
		st := &Statistic{table: t}
		var lastUpdated sql.NullTime
		var rowCount, rowsSampled, unfiltered sql.NullInt64
		var steps sql.NullInt32
		if err := rows.Scan(
			&st.Name, &st.StatID,
			&st.IsAutoCreated, &st.IsUserCreated,
			&st.HasFilter, &st.FilterDef,
			&lastUpdated, &rowCount, &rowsSampled,
			&steps, &unfiltered,
		); err != nil {
			return nil, err
		}
		if lastUpdated.Valid {
			st.LastUpdated = lastUpdated.Time
		}
		st.RowCount = rowCount.Int64
		st.RowsSampled = rowsSampled.Int64
		st.Steps = int(steps.Int32)
		st.Unfiltered = unfiltered.Int64
		stats = append(stats, st)
	}
	return stats, rows.Err()
}

// Update updates the statistic (UPDATE STATISTICS … WITH FULLSCAN or SAMPLE n PERCENT).
// Pass samplePct=0 for FULLSCAN.
func (st *Statistic) Update(samplePct int) error {
	option := "FULLSCAN"
	if samplePct > 0 {
		option = fmt.Sprintf("SAMPLE %d PERCENT", samplePct)
	}
	q := fmt.Sprintf("UPDATE STATISTICS %s [%s] WITH %s",
		st.table.FullName(), st.Name, option)
	_, err := st.table.db.exec(context.Background(), q)
	if err != nil {
		return fmt.Errorf("gosmo: update statistic [%s]: %w", st.Name, err)
	}
	return nil
}

// Drop drops the statistic.
func (st *Statistic) Drop() error {
	q := fmt.Sprintf("DROP STATISTICS %s.[%s]", st.table.FullName(), st.Name)
	_, err := st.table.db.exec(context.Background(), q)
	if err != nil {
		return fmt.Errorf("gosmo: drop statistic [%s]: %w", st.Name, err)
	}
	return nil
}

// UpdateAllStatistics updates all statistics on the table.
func (t *Table) UpdateAllStatistics(samplePct int) error {
	option := "FULLSCAN"
	if samplePct > 0 {
		option = fmt.Sprintf("SAMPLE %d PERCENT", samplePct)
	}
	q := fmt.Sprintf("UPDATE STATISTICS %s WITH %s", t.FullName(), option)
	_, err := t.db.exec(context.Background(), q)
	if err != nil {
		return fmt.Errorf("gosmo: update all statistics on %s: %w", t.FullName(), err)
	}
	return nil
}

// CreateStatistic creates a manual statistic on one or more columns.
func (t *Table) CreateStatistic(name string, columns []string, samplePct int) error {
	if len(columns) == 0 {
		return fmt.Errorf("gosmo: create statistic: at least one column required")
	}
	cols := make([]string, len(columns))
	for i, c := range columns {
		cols[i] = fmt.Sprintf("[%s]", c)
	}
	q := fmt.Sprintf("CREATE STATISTICS [%s] ON %s (%s)",
		name, t.FullName(), joinStr(cols, ", "))
	if samplePct > 0 {
		q += fmt.Sprintf(" WITH SAMPLE %d PERCENT", samplePct)
	}
	_, err := t.db.exec(context.Background(), q)
	if err != nil {
		return fmt.Errorf("gosmo: create statistic [%s]: %w", name, err)
	}
	return nil
}

func joinStr(ss []string, sep string) string {
	result := ""
	for i, s := range ss {
		if i > 0 {
			result += sep
		}
		result += s
	}
	return result
}
