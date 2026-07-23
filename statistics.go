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
	table               *Table
	Name                string
	StatID              int
	IsAutoCreated       bool
	IsUserCreated       bool
	HasFilter           bool
	FilterDef           string
	LastUpdated         time.Time
	RowsSampled         int64
	TotalRows           int64 // renamed from RowCount to avoid shadowing Table.RowCount()
	Steps               int
	UnfilteredRows      int64
	NoRecompute         bool
	IsIncremental       bool
	ModificationCounter int64
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
       sp.steps, sp.unfiltered_rows,
       s.no_recompute, s.is_incremental, sp.modification_counter
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
		var rowsSampled, totalRows, unfiltered, modCounter sql.NullInt64
		var steps sql.NullInt32
		if err := rows.Scan(
			&st.Name, &st.StatID,
			&st.IsAutoCreated, &st.IsUserCreated,
			&st.HasFilter, &st.FilterDef,
			&lastUpdated, &rowsSampled, &totalRows,
			&steps, &unfiltered,
			&st.NoRecompute, &st.IsIncremental, &modCounter,
		); err != nil {
			return nil, err
		}
		st.ModificationCounter = modCounter.Int64
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

// Columns returns this statistic's columns, in stat-column order. The
// leading column is what the statistic's histogram is built on; every
// column contributes to its density vector.
func (st *Statistic) Columns() ([]string, error) {
	return st.ColumnsContext(context.Background())
}

// ColumnsContext is the context-aware variant of Columns.
func (st *Statistic) ColumnsContext(ctx context.Context) ([]string, error) {
	const q = `
SELECT c.name
FROM   sys.stats_columns sc
JOIN   sys.columns c ON c.object_id = sc.object_id AND c.column_id = sc.column_id
WHERE  sc.object_id = @p1 AND sc.stats_id = @p2
ORDER  BY sc.stats_column_id`

	rows, err := st.table.db.query(ctx, q, st.table.ObjectID, st.StatID)
	if err != nil {
		return nil, fmt.Errorf("gosmo: columns for statistic %q: %w", st.Name, err)
	}
	defer rows.Close()

	var cols []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		cols = append(cols, name)
	}
	return cols, rows.Err()
}

// StatisticHeader mirrors the single result row of
// DBCC SHOW_STATISTICS ... WITH STAT_HEADER. Every field is zero-valued
// (Updated "") when the statistic exists as metadata but has never actually
// been populated (e.g. an auto-created statistic on a table nothing has
// queried yet) — SQL Server itself returns a header row of NULLs in that
// case.
type StatisticHeader struct {
	Updated                string
	Rows                   int64
	RowsSampled            int64
	Steps                  int
	Density                float64
	AverageKeyLength       float64
	StringIndex            string
	FilterExpression       string
	UnfilteredRows         int64
	PersistedSamplePercent float64
}

// Header returns this statistic's DBCC SHOW_STATISTICS header row.
func (st *Statistic) Header() (*StatisticHeader, error) {
	return st.HeaderContext(context.Background())
}

// HeaderContext is the context-aware variant of Header.
func (st *Statistic) HeaderContext(ctx context.Context) (*StatisticHeader, error) {
	// DBCC SHOW_STATISTICS does not accept parameters for the table/stat
	// name (same restriction UpdateContext already works around).
	q := fmt.Sprintf("DBCC SHOW_STATISTICS (N'%s', N'%s') WITH STAT_HEADER, NO_INFOMSGS",
		escapeSingle(st.table.FullName()), escapeSingle(st.Name))

	row, release, err := st.table.db.queryRow(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("gosmo: statistics header for %q: %w", st.Name, err)
	}
	defer release()

	var name string
	var updated, stringIndex, filterExpr sql.NullString
	var rowsN, rowsSampled, unfiltered sql.NullInt64
	var steps sql.NullInt16
	var density, avgKeyLen, samplePct sql.NullFloat64
	if err := row.Scan(&name, &updated, &rowsN, &rowsSampled, &steps, &density,
		&avgKeyLen, &stringIndex, &filterExpr, &unfiltered, &samplePct); err != nil {
		return nil, fmt.Errorf("gosmo: statistics header for %q: %w", st.Name, err)
	}
	return &StatisticHeader{
		Updated:                updated.String,
		Rows:                   rowsN.Int64,
		RowsSampled:            rowsSampled.Int64,
		Steps:                  int(steps.Int16),
		Density:                density.Float64,
		AverageKeyLength:       avgKeyLen.Float64,
		StringIndex:            stringIndex.String,
		FilterExpression:       filterExpr.String,
		UnfilteredRows:         unfiltered.Int64,
		PersistedSamplePercent: samplePct.Float64,
	}, nil
}

// StatisticDensity is one row of DBCC SHOW_STATISTICS ... WITH
// DENSITY_VECTOR — one row per leading-column prefix of the statistic's key
// columns (e.g. a 2-column statistic yields 2 rows: {col1} and {col1,col2}).
type StatisticDensity struct {
	AllDensity    float64
	AverageLength float64
	Columns       string
}

// DensityVector returns this statistic's density vector.
func (st *Statistic) DensityVector() ([]*StatisticDensity, error) {
	return st.DensityVectorContext(context.Background())
}

// DensityVectorContext is the context-aware variant of DensityVector.
func (st *Statistic) DensityVectorContext(ctx context.Context) ([]*StatisticDensity, error) {
	q := fmt.Sprintf("DBCC SHOW_STATISTICS (N'%s', N'%s') WITH DENSITY_VECTOR, NO_INFOMSGS",
		escapeSingle(st.table.FullName()), escapeSingle(st.Name))

	rows, err := st.table.db.query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("gosmo: density vector for %q: %w", st.Name, err)
	}
	defer rows.Close()

	var out []*StatisticDensity
	for rows.Next() {
		d := &StatisticDensity{}
		if err := rows.Scan(&d.AllDensity, &d.AverageLength, &d.Columns); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// StatisticHistogramStep is one step of DBCC SHOW_STATISTICS ... WITH
// HISTOGRAM. RangeHighKey is formatted as text since its underlying SQL
// type follows the statistic's leading key column (int, varchar, datetime,
// ...), not one fixed Go type.
type StatisticHistogramStep struct {
	RangeHighKey      string
	RangeRows         float64
	EqRows            float64
	DistinctRangeRows int64
	AvgRangeRows      float64
}

// Histogram returns this statistic's histogram steps.
func (st *Statistic) Histogram() ([]*StatisticHistogramStep, error) {
	return st.HistogramContext(context.Background())
}

// HistogramContext is the context-aware variant of Histogram.
func (st *Statistic) HistogramContext(ctx context.Context) ([]*StatisticHistogramStep, error) {
	q := fmt.Sprintf("DBCC SHOW_STATISTICS (N'%s', N'%s') WITH HISTOGRAM, NO_INFOMSGS",
		escapeSingle(st.table.FullName()), escapeSingle(st.Name))

	rows, err := st.table.db.query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("gosmo: histogram for %q: %w", st.Name, err)
	}
	defer rows.Close()

	var out []*StatisticHistogramStep
	for rows.Next() {
		var rangeHiKey any
		s := &StatisticHistogramStep{}
		if err := rows.Scan(&rangeHiKey, &s.RangeRows, &s.EqRows, &s.DistinctRangeRows, &s.AvgRangeRows); err != nil {
			return nil, err
		}
		s.RangeHighKey = formatHistogramKey(rangeHiKey)
		out = append(out, s)
	}
	return out, rows.Err()
}

// formatHistogramKey renders a RANGE_HI_KEY value as text. A NULL key (the
// histogram's step for NULL values, when the leading column is nullable) is
// "NULL", and a binary-typed key is 0x-hex — both would otherwise render as
// Go's own "%v" artifacts ("<nil>", a decimal byte list).
func formatHistogramKey(v any) string {
	switch k := v.(type) {
	case nil:
		return "NULL"
	case []byte:
		return fmt.Sprintf("0x%X", k)
	default:
		return fmt.Sprintf("%v", k)
	}
}
