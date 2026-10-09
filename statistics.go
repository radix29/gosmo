package gosmo

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"

	mssql "github.com/microsoft/go-mssqldb"
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

// statisticSelect is shared by Statistics and StatisticByName
// so a statistic carries the same fields however it was fetched.
//
// dm_db_stats_properties is OUTER APPLYed: it returns no row to a caller who
// can see the statistic in sys.stats (VIEW DEFINITION) but may not read it,
// and a CROSS APPLY dropped such a statistic from the list altogether. Its
// properties then read as zero, as they do for a statistic never computed.
const statisticSelect = `
SELECT s.name, s.stats_id,
       s.auto_created, s.user_created,
       s.has_filter, ISNULL(s.filter_definition, ''),
       sp.last_updated, sp.rows_sampled, sp.rows,
       sp.steps, sp.unfiltered_rows,
       s.no_recompute, s.is_incremental, sp.modification_counter
FROM   sys.stats s
OUTER  APPLY sys.dm_db_stats_properties(s.object_id, s.stats_id) sp
WHERE  s.object_id = @p1`

// Statistics returns all statistics objects for the table.
func (t *Table) Statistics(ctx context.Context) ([]*Statistic, error) {
	if err := t.requireLoaded("statistics for"); err != nil {
		return nil, err
	}
	rows, err := t.db.query(ctx, statisticSelect+`
ORDER  BY s.name`, t.ObjectID)
	return scanRows(rows, err, fmt.Sprintf("statistics for %s", t.FullName()), func(scan func(...any) error) (*Statistic, error) {
		return scanStatistic(t, scan)
	})
}

// StatisticByName returns one statistics object on the table by name.
//
// It returns an error satisfying errors.Is(err, ErrNotFound) when the table
// has no such statistic.
func (t *Table) StatisticByName(ctx context.Context, name string) (*Statistic, error) {
	if err := t.requireLoaded(fmt.Sprintf("find statistic %q on", name)); err != nil {
		return nil, err
	}
	var st *Statistic
	err := t.db.queryRow(ctx, func(row *sql.Row) error {
		var err error
		st, err = scanStatistic(t, row.Scan)
		return err
	}, statisticSelect+`
       AND s.name = @p2`, t.ObjectID, name)
	return foundRow(st, err, notFoundf("gosmo: statistic %q not found on %s", name, t.FullName()), fmt.Sprintf("find statistic %q on %s", name, t.FullName()))
}

// StatisticRef returns a lightweight handle for name on the table without
// querying the server at all — unlike StatisticByName,
// it doesn't verify the statistic exists or populate StatID/IsAutoCreated/
// LastUpdated/Steps/etc. (they stay at their zero value). Every write method
// on *Statistic (Update, Drop, Rename) only ever needs
// the statistic's name and its table's, never those cached fields, so this is
// sufficient for issuing further calls against a statistic the caller already
// knows exists — most commonly one it just created in the same operation. The
// read methods (Columns, Header, DensityVector,
// Histogram) work from the same two names and so are usable from a
// handle too. See Server.DatabaseRef's doc comment for why this also matters
// under a WithScript-derived context.
func (t *Table) StatisticRef(name string) *Statistic {
	return &Statistic{table: t, Name: name}
}

// Table returns the table the statistic is on.
func (st *Statistic) Table() *Table { return st.table }

func scanStatistic(t *Table, scan func(...any) error) (*Statistic, error) {
	st := &Statistic{table: t}
	var lastUpdated sql.NullTime
	var rowsSampled, totalRows, unfiltered, modCounter sql.NullInt64
	var steps sql.NullInt32
	if err := scan(
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
	return st, nil
}

// checkSamplePct rejects a sampling percentage outside the 0-100 range
// SAMPLE n PERCENT accepts, so a caller's out-of-range value fails here
// with a gosmo error instead of as a server-side syntax error. 0 is always
// allowed — every caller reads it as "not a percentage" (FULLSCAN, or the
// server's default sampling).
func checkSamplePct(op string, samplePct int) error {
	if samplePct < 0 || samplePct > 100 {
		return invalidf("gosmo: %s: sample percentage %d out of range (0-100)", op, samplePct)
	}
	return nil
}

// sampleClause is the WITH option for a sampling percentage checked by
// checkSamplePct: FULLSCAN for 0, SAMPLE n PERCENT otherwise. Statistic.Update,
// Table.UpdateAllStatistics and Index.UpdateStatistics share it so the three
// read "0" the same way.
func sampleClause(samplePct int) string {
	if samplePct > 0 {
		return fmt.Sprintf("SAMPLE %d PERCENT", samplePct)
	}
	return "FULLSCAN"
}

// Update updates this statistic.
// Pass samplePct=0 for a FULLSCAN; any value 1-100 uses SAMPLE n PERCENT.
func (st *Statistic) Update(ctx context.Context, samplePct int) error {
	if err := checkSamplePct("update statistic "+st.Name, samplePct); err != nil {
		return err
	}
	option := sampleClause(samplePct)
	// UPDATE STATISTICS does not support parameterised stat names.
	q := fmt.Sprintf("UPDATE STATISTICS %s %s WITH %s",
		st.table.FullName(), quoteIdent(st.Name), option)
	if _, err := st.table.exec(ctx, q); err != nil {
		return fmt.Errorf("gosmo: update statistic %q: %w", st.Name, err)
	}
	return nil
}

// Drop drops this statistic.
// Correct T-SQL syntax: DROP STATISTICS table_name.stat_name
func (st *Statistic) Drop(ctx context.Context) error {
	// DROP STATISTICS syntax: schema.table.stat (not quoted as one unit)
	q := fmt.Sprintf("DROP STATISTICS %s.%s.%s",
		quoteIdent(st.table.Schema), quoteIdent(st.table.Name), quoteIdent(st.Name))
	if _, err := st.table.exec(ctx, q); err != nil {
		return fmt.Errorf("gosmo: drop statistic %q: %w", st.Name, err)
	}
	return nil
}

// UpdateAllStatistics updates all statistics on the table.
func (t *Table) UpdateAllStatistics(ctx context.Context, samplePct int) error {
	if err := checkSamplePct("update all statistics on "+t.FullName(), samplePct); err != nil {
		return err
	}
	option := sampleClause(samplePct)
	if _, err := t.exec(ctx, fmt.Sprintf("UPDATE STATISTICS %s WITH %s", t.FullName(), option)); err != nil {
		return fmt.Errorf("gosmo: update all statistics on %s: %w", t.FullName(), err)
	}
	return nil
}

// CreateStatisticRequest describes a user-defined statistic to create.
// Columns is ordered: the leading column is the one the histogram is built
// on, and every column contributes to the density vector.
type CreateStatisticRequest struct {
	Name    string
	Columns []string
	// SamplePercent scans that percentage of the rows (SAMPLE n PERCENT).
	// Zero lets the server pick its own sample, unless FullScan is set.
	SamplePercent int
	// FullScan reads every row (WITH FULLSCAN). An alternative to
	// SamplePercent, not a companion to it.
	FullScan bool
	// FilterDefinition is a filtered statistic's predicate, without the
	// WHERE.
	FilterDefinition string
	// NoRecompute stops the server refreshing this statistic automatically
	// (WITH NORECOMPUTE) — it then only changes when UPDATE STATISTICS runs.
	NoRecompute bool
	// Incremental builds the statistic per partition (WITH INCREMENTAL = ON),
	// which requires a partitioned table.
	Incremental bool
}

// CreateStatistic creates a user-defined statistic. A request naming only
// the statistic and its columns lets the server choose its own sample.
func (t *Table) CreateStatistic(ctx context.Context, req CreateStatisticRequest) (*Statistic, error) {
	q, err := buildCreateStatisticStatement(t.FullName(), req)
	if err != nil {
		return nil, err
	}
	if _, err := t.exec(ctx, q); err != nil {
		return nil, fmt.Errorf("gosmo: create statistic %q: %w", req.Name, err)
	}
	return createdObject(ctx, t.StatisticRef(req.Name), func() (*Statistic, error) {
		return t.StatisticByName(ctx, req.Name)
	})
}

// buildCreateStatisticStatement renders one CREATE STATISTICS statement, or
// reports why the request cannot be one. Separated from the write so the
// statement can be pinned without a server.
func buildCreateStatisticStatement(tableName string, req CreateStatisticRequest) (string, error) {
	if req.Name == "" {
		return "", invalidf("gosmo: create statistic: name is required")
	}
	if len(req.Columns) == 0 {
		return "", invalidf("gosmo: create statistic: at least one column required")
	}
	if err := checkSamplePct("create statistic "+req.Name, req.SamplePercent); err != nil {
		return "", err
	}
	if req.FullScan && req.SamplePercent > 0 {
		return "", invalidf("gosmo: create statistic %q: a full scan and a sample percentage are alternatives", req.Name)
	}

	quotedCols := make([]string, len(req.Columns))
	for i, c := range req.Columns {
		quotedCols[i] = quoteIdent(c)
	}
	q := fmt.Sprintf("CREATE STATISTICS %s ON %s (%s)",
		quoteIdent(req.Name), tableName, strings.Join(quotedCols, ", "))
	if req.FilterDefinition != "" {
		q += " WHERE " + req.FilterDefinition
	}

	var withs []string
	switch {
	case req.FullScan:
		withs = append(withs, "FULLSCAN")
	case req.SamplePercent > 0:
		withs = append(withs, fmt.Sprintf("SAMPLE %d PERCENT", req.SamplePercent))
	}
	if req.NoRecompute {
		withs = append(withs, "NORECOMPUTE")
	}
	if req.Incremental {
		withs = append(withs, "INCREMENTAL = ON")
	}
	if len(withs) > 0 {
		q += " WITH " + strings.Join(withs, ", ")
	}
	return q, nil
}

// Columns returns this statistic's columns, in stat-column order. The
// leading column is what the statistic's histogram is built on; every
// column contributes to its density vector.
//
// The statistic is found by its table's name and its own, like Header and
// Histogram, so it works from a StatisticRef on a TableRef. It read by
// ObjectID and StatID until 2026-10-03, so from a handle it found nothing
// and answered with no columns, although this doc already promised it.
func (st *Statistic) Columns(ctx context.Context) ([]string, error) {
	q := fmt.Sprintf(`
SELECT c.name
FROM   sys.stats s
JOIN   sys.stats_columns sc ON sc.object_id = s.object_id AND sc.stats_id = s.stats_id
JOIN   sys.columns c ON c.object_id = sc.object_id AND c.column_id = sc.column_id
WHERE  s.object_id = OBJECT_ID(N'%s') AND s.name = @p1
ORDER  BY sc.stats_column_id`, escapeSingle(st.table.FullName()))

	rows, err := st.table.db.query(ctx, q, st.Name)
	return scanRows(rows, err, fmt.Sprintf("columns for statistic %q", st.Name), func(scan func(...any) error) (string, error) {
		var name string
		if err := scan(&name); err != nil {
			return "", err
		}
		return name, nil
	})
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
func (st *Statistic) Header(ctx context.Context) (*StatisticHeader, error) {
	// DBCC SHOW_STATISTICS does not accept parameters for the table/stat
	// name (same restriction Update already works around).
	q := fmt.Sprintf("DBCC SHOW_STATISTICS (N'%s', N'%s') WITH STAT_HEADER, NO_INFOMSGS",
		escapeSingle(st.table.FullName()), escapeSingle(st.Name))

	// DBCC's result shape is not a documented contract and has changed: the
	// header is 10 columns before SQL Server 2019 and 11 from it (Persisted
	// Sample Percent), so a fixed destination list fails every statistic on an
	// older instance with "expected 10 destination arguments in Scan, not 11".
	// Bind by column name instead — a column the instance does not return
	// leaves its field zero, and one it grows next is discarded rather than
	// breaking the read.
	var updated, stringIndex, filterExpr sql.NullString
	var rowsN, rowsSampled, unfiltered sql.NullInt64
	var steps sql.NullInt16
	var density, avgKeyLen, samplePct sql.NullFloat64
	byName := map[string]any{
		"Updated":                  &updated,
		"Rows":                     &rowsN,
		"Rows Sampled":             &rowsSampled,
		"Steps":                    &steps,
		"Density":                  &density,
		"Average key length":       &avgKeyLen,
		"String Index":             &stringIndex,
		"Filter Expression":        &filterExpr,
		"Unfiltered Rows":          &unfiltered,
		"Persisted Sample Percent": &samplePct,
	}

	rows, err := st.table.db.query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("gosmo: statistics header for %q: %w", st.Name, err)
	}
	defer rows.Close()

	cols, err := rows.Columns()
	if err != nil {
		return nil, fmt.Errorf("gosmo: statistics header for %q: %w", st.Name, err)
	}
	dest := make([]any, len(cols))
	for i, c := range cols {
		if d, ok := byName[c]; ok {
			dest[i] = d
			continue
		}
		dest[i] = new(any)
	}
	if !rows.Next() {
		err := rows.Err()
		if err == nil {
			err = sql.ErrNoRows
		}
		return nil, fmt.Errorf("gosmo: statistics header for %q: %w", st.Name, err)
	}
	if err := rows.Scan(dest...); err != nil {
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
func (st *Statistic) DensityVector(ctx context.Context) ([]*StatisticDensity, error) {
	q := fmt.Sprintf("DBCC SHOW_STATISTICS (N'%s', N'%s') WITH DENSITY_VECTOR, NO_INFOMSGS",
		escapeSingle(st.table.FullName()), escapeSingle(st.Name))

	rows, err := st.table.db.query(ctx, q)
	return scanRows(rows, err, fmt.Sprintf("density vector for %q", st.Name), func(scan func(...any) error) (*StatisticDensity, error) {
		d := &StatisticDensity{}
		if err := scan(&d.AllDensity, &d.AverageLength, &d.Columns); err != nil {
			return nil, err
		}
		return d, nil
	})
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
func (st *Statistic) Histogram(ctx context.Context) ([]*StatisticHistogramStep, error) {
	q := fmt.Sprintf("DBCC SHOW_STATISTICS (N'%s', N'%s') WITH HISTOGRAM, NO_INFOMSGS",
		escapeSingle(st.table.FullName()), escapeSingle(st.Name))

	rows, err := st.table.db.query(ctx, q)
	var key histogramKeyType
	if err == nil {
		if key, err = histogramKeyTypeOf(rows); err != nil {
			rows.Close()
		}
	}
	return scanRows(rows, err, fmt.Sprintf("histogram for %q", st.Name), func(scan func(...any) error) (*StatisticHistogramStep, error) {
		var rangeHiKey any
		s := &StatisticHistogramStep{}
		if err := scan(&rangeHiKey, &s.RangeRows, &s.EqRows, &s.DistinctRangeRows, &s.AvgRangeRows); err != nil {
			return nil, err
		}
		s.RangeHighKey = key.format(rangeHiKey)
		return s, nil
	})
}

// histogramKeyType is RANGE_HI_KEY's SQL type, which follows the statistic's
// leading key column: the Go value the driver hands back for it does not say
// enough on its own to render it.
type histogramKeyType struct {
	name  string // DatabaseTypeName, upper case: "DECIMAL", "DATETIME2", ...
	scale int64  // fractional-second digits, for the types that have them
}

func histogramKeyTypeOf(rows *dbRows) (histogramKeyType, error) {
	cts, err := rows.ColumnTypes()
	if err != nil || len(cts) == 0 {
		return histogramKeyType{}, err
	}
	k := histogramKeyType{name: strings.ToUpper(cts[0].DatabaseTypeName())}
	if _, scale, ok := cts[0].DecimalSize(); ok {
		k.scale = scale
	}
	return k, nil
}

// format renders a RANGE_HI_KEY value as text, the way the server would
// print it. A NULL key (the histogram's step for NULL values, when the
// leading column is nullable) is "NULL" and a binary key is 0x-hex — both
// would otherwise render as Go's own "%v" artifacts ("<nil>", a decimal byte
// list).
//
// The rest is go-mssqldb's representation, not the value's: decimal, numeric
// and money arrive as the digits' bytes (shown as hex, 0x3132332E3435 for
// 123.45), a uniqueidentifier as its 16 bytes in wire order (shown as hex
// with the first three groups reversed), every date/time type as a time.Time
// in UTC ("2026-01-02 03:04:05 +0000 UTC" for a datetime2(0)), and a real
// widened to float64 (0.10000000149011612), a bit as Go's true/false.
func (k histogramKeyType) format(v any) string {
	switch v := v.(type) {
	case nil:
		return "NULL"
	case []byte:
		switch k.name {
		case "DECIMAL", "NUMERIC", "MONEY", "SMALLMONEY":
			return string(v)
		case "UNIQUEIDENTIFIER":
			var u mssql.UniqueIdentifier
			if u.Scan(v) == nil {
				return u.String()
			}
		}
		return binaryLiteral(v)
	case time.Time:
		if layout := k.timeLayout(); layout != "" {
			return v.Format(layout)
		}
		return v.String()
	case float64:
		if k.name == "REAL" {
			return strconv.FormatFloat(v, 'g', -1, 32)
		}
		return strconv.FormatFloat(v, 'g', -1, 64)
	case bool:
		if v {
			return "1"
		}
		return "0"
	default:
		return fmt.Sprintf("%v", v)
	}
}

// timeLayout is the layout a date/time key prints in: the type's own fields,
// with as many fractional-second digits as its scale.
func (k histogramKeyType) timeLayout() string {
	frac := ""
	if k.scale > 0 {
		frac = "." + strings.Repeat("0", int(min(k.scale, 9)))
	}
	switch k.name {
	case "DATE":
		return "2006-01-02"
	case "TIME":
		return "15:04:05" + frac
	case "SMALLDATETIME":
		return "2006-01-02 15:04:05"
	case "DATETIME":
		return "2006-01-02 15:04:05.000"
	case "DATETIME2":
		return "2006-01-02 15:04:05" + frac
	case "DATETIMEOFFSET":
		return "2006-01-02 15:04:05" + frac + " -07:00"
	}
	return ""
}

// Rename renames the statistic using sp_rename.
func (st *Statistic) Rename(ctx context.Context, newName string) error {
	objName := st.table.FullName() + "." + quoteIdent(st.Name)
	if _, err := st.table.exec(ctx,
		"EXEC sp_rename @objname = @p1, @newname = @p2, @objtype = N'STATISTICS'",
		objName, newName,
	); err != nil {
		return fmt.Errorf("gosmo: rename statistic %q to %q: %w", st.Name, newName, err)
	}
	setIfApplied(ctx, &st.Name, newName)
	return nil
}
