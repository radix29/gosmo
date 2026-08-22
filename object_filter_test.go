package gosmo

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

// testFilterCols stands in for one family's column mapping.
var testFilterCols = filterColumns{
	name:            "t.name",
	schema:          "SCHEMA_NAME(t.schema_id)",
	created:         "t.create_date",
	memoryOptimized: "t.is_memory_optimized",
}

func day(y int, m time.Month, d int) time.Time {
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

// The clause is what makes a pushed-down filter equal the caller-side one, so
// each operator's SQL and its parameters are asserted in full rather than by
// substring: a LIKE that lost its LOWER still runs, still returns rows, and
// returns the wrong ones on a case-sensitive database.
func TestObjectFilterClause(t *testing.T) {
	yes, no := true, false
	for _, c := range []struct {
		name     string
		filter   ObjectFilter
		wantSQL  string
		wantArgs []any
	}{
		{
			name:    "an empty filter narrows nothing",
			filter:  ObjectFilter{},
			wantSQL: "",
		},
		{
			name:     "contains",
			filter:   ObjectFilter{Name: []TextCriterion{{Op: TextContains, Value: "cust"}}},
			wantSQL:  ` AND LOWER(t.name) LIKE LOWER(@p1) ESCAPE '\'`,
			wantArgs: []any{"%cust%"},
		},
		{
			name:     "does not contain",
			filter:   ObjectFilter{Name: []TextCriterion{{Op: TextNotContains, Value: "tmp"}}},
			wantSQL:  ` AND LOWER(t.name) NOT LIKE LOWER(@p1) ESCAPE '\'`,
			wantArgs: []any{"%tmp%"},
		},
		{
			name:     "equals",
			filter:   ObjectFilter{Name: []TextCriterion{{Op: TextEquals, Value: "Orders"}}},
			wantSQL:  " AND LOWER(t.name) = LOWER(@p1)",
			wantArgs: []any{"Orders"},
		},
		{
			name:     "does not equal",
			filter:   ObjectFilter{Name: []TextCriterion{{Op: TextNotEquals, Value: "Orders"}}},
			wantSQL:  " AND LOWER(t.name) <> LOWER(@p1)",
			wantArgs: []any{"Orders"},
		},
		{
			// _ and % are legal in an identifier: unescaped, searching for
			// "pct_100" matches "pct1100" and every other one-character
			// variant, and a name containing [ matches nothing at all.
			name:     "wildcards in the search text are escaped",
			filter:   ObjectFilter{Name: []TextCriterion{{Op: TextContains, Value: "pct_100%[x]"}}},
			wantSQL:  ` AND LOWER(t.name) LIKE LOWER(@p1) ESCAPE '\'`,
			wantArgs: []any{`%pct\_100\%\[x]%`},
		},
		{
			name: "criteria AND together and number their own parameters",
			filter: ObjectFilter{
				Name:   []TextCriterion{{Op: TextContains, Value: "cust"}},
				Schema: []TextCriterion{{Op: TextEquals, Value: "sales"}},
			},
			wantSQL: ` AND LOWER(t.name) LIKE LOWER(@p1) ESCAPE '\'` +
				" AND LOWER(SCHEMA_NAME(t.schema_id)) = LOWER(@p2)",
			wantArgs: []any{"%cust%", "sales"},
		},
		{
			// A day, not an instant: the column is a timestamp, so "on the
			// 20th" is a half-open range and not an equality.
			name:     "on a day",
			filter:   ObjectFilter{Created: []DateCriterion{{Op: DateOn, Day: day(2026, 8, 20)}}},
			wantSQL:  " AND t.create_date >= @p1 AND t.create_date < @p2",
			wantArgs: []any{day(2026, 8, 20), day(2026, 8, 21)},
		},
		{
			name:     "before a day is midnight that morning",
			filter:   ObjectFilter{Created: []DateCriterion{{Op: DateBefore, Day: day(2026, 8, 20)}}},
			wantSQL:  " AND t.create_date < @p1",
			wantArgs: []any{day(2026, 8, 20)},
		},
		{
			// After the 20th excludes the 20th itself, matching a caller that
			// compares whole days.
			name:     "after a day starts the next morning",
			filter:   ObjectFilter{Created: []DateCriterion{{Op: DateAfter, Day: day(2026, 8, 20)}}},
			wantSQL:  " AND t.create_date >= @p1",
			wantArgs: []any{day(2026, 8, 21)},
		},
		{
			name:     "a criterion's time of day is discarded",
			filter:   ObjectFilter{Created: []DateCriterion{{Op: DateBefore, Day: time.Date(2026, 8, 20, 17, 45, 3, 0, time.UTC)}}},
			wantSQL:  " AND t.create_date < @p1",
			wantArgs: []any{day(2026, 8, 20)},
		},
		{
			name:     "memory optimized true",
			filter:   ObjectFilter{MemoryOptimized: &yes},
			wantSQL:  " AND t.is_memory_optimized = @p1",
			wantArgs: []any{1},
		},
		{
			name:     "memory optimized false",
			filter:   ObjectFilter{MemoryOptimized: &no},
			wantSQL:  " AND t.is_memory_optimized = @p1",
			wantArgs: []any{0},
		},
		{
			// An empty value is not a criterion — matching everything is what
			// a cleared filter box means, not matching the empty string.
			name:    "an empty value is dropped",
			filter:  ObjectFilter{Name: []TextCriterion{{Op: TextContains, Value: ""}}},
			wantSQL: "",
		},
		{
			name:    "a zero date is dropped",
			filter:  ObjectFilter{Created: []DateCriterion{{Op: DateOn}}},
			wantSQL: "",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			sql, args := c.filter.clause(testFilterCols, 1)
			if sql != c.wantSQL {
				t.Errorf("sql  = %q\nwant = %q", sql, c.wantSQL)
			}
			if len(args) == 0 && len(c.wantArgs) == 0 {
				return
			}
			if !reflect.DeepEqual(args, c.wantArgs) {
				t.Errorf("args = %#v, want %#v", args, c.wantArgs)
			}
		})
	}
}

// A family with no column for a criterion drops it rather than generating SQL
// against a column that isn't there — sys.views has no is_memory_optimized,
// and a filter carrying one is still a legal filter.
func TestObjectFilterClauseDropsWhatTheFamilyLacks(t *testing.T) {
	yes := true
	f := ObjectFilter{
		Name:            []TextCriterion{{Op: TextContains, Value: "v"}},
		MemoryOptimized: &yes,
	}
	sql, args := f.clause(viewFilterColumns, 1)
	if strings.Contains(sql, "memory") {
		t.Errorf("sql = %q, want no memory-optimized term for a view", sql)
	}
	if len(args) != 1 {
		t.Errorf("args = %#v, want only the name pattern", args)
	}
}

// The parameter numbering has to start where the caller's own arguments end,
// or a listing that already binds @p1 gets its argument shadowed.
func TestObjectFilterClauseStartsAtTheGivenParameter(t *testing.T) {
	f := ObjectFilter{Name: []TextCriterion{{Op: TextEquals, Value: "Orders"}}}
	sql, _ := f.clause(testFilterCols, 3)
	if !strings.Contains(sql, "@p3") {
		t.Errorf("sql = %q, want it to bind @p3", sql)
	}
}

func TestObjectFilterEmpty(t *testing.T) {
	yes := true
	for _, c := range []struct {
		name   string
		filter ObjectFilter
		want   bool
	}{
		{"zero", ObjectFilter{}, true},
		{"a name", ObjectFilter{Name: []TextCriterion{{Value: "x"}}}, false},
		{"a schema", ObjectFilter{Schema: []TextCriterion{{Value: "x"}}}, false},
		{"a date", ObjectFilter{Created: []DateCriterion{{Day: day(2026, 8, 20)}}}, false},
		{"memory optimized", ObjectFilter{MemoryOptimized: &yes}, false},
	} {
		if got := c.filter.Empty(); got != c.want {
			t.Errorf("%s: Empty() = %v, want %v", c.name, got, c.want)
		}
	}
}
