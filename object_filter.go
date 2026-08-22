package gosmo

import (
	"fmt"
	"strings"
	"time"
)

// object_filter.go narrows a catalog listing at the server. It is what an
// Object Explorer-style folder filter ("show me the tables whose name contains
// cust") is built on: without it a folder of ten thousand tables is read whole
// and filtered by the caller, which is exactly the folder where that hurts.

// TextOp is one comparison a text criterion makes.
type TextOp int

const (
	TextContains TextOp = iota
	TextNotContains
	TextEquals
	TextNotEquals
)

// DateOp is one comparison a date criterion makes. All three work on whole
// calendar days: a creation date is a timestamp, and "created on the 20th"
// means the day, not midnight exactly.
type DateOp int

const (
	DateOn DateOp = iota
	DateBefore
	DateAfter
)

// TextCriterion is one comparison against a name or schema.
type TextCriterion struct {
	Op    TextOp
	Value string
}

// DateCriterion is one comparison against a creation date. Day's time of day
// is ignored.
type DateCriterion struct {
	Op  DateOp
	Day time.Time
}

// ObjectFilter narrows a catalog listing. Every criterion narrows it further —
// they are AND-ed, never OR-ed — and a zero ObjectFilter narrows nothing, so
// the unfiltered listing is the same call with an empty one.
//
// Matching is case-insensitive regardless of the database's collation (see
// the note on clause), which is the behaviour a user typing into a filter box
// expects and the one a case-sensitive database would otherwise break.
type ObjectFilter struct {
	Name    []TextCriterion
	Schema  []TextCriterion
	Created []DateCriterion
	// MemoryOptimized, when set, requires sys.tables.is_memory_optimized to
	// equal it. Only the table listing has such a column; on any other family
	// it is ignored rather than failing, since a filter is a description of
	// what the caller wants and not every family can express all of it.
	MemoryOptimized *bool
}

// Empty reports whether f narrows anything at all.
func (f ObjectFilter) Empty() bool {
	return len(f.Name) == 0 && len(f.Schema) == 0 && len(f.Created) == 0 && f.MemoryOptimized == nil
}

// filterColumns names the SQL expression each criterion compares against, per
// family — sys.tables calls its schema SCHEMA_NAME(t.schema_id) and
// sys.all_objects calls it SCHEMA_NAME(o.schema_id). An empty expression means
// this family has no such column and criteria against it are dropped.
type filterColumns struct {
	name            string
	schema          string
	created         string
	memoryOptimized string
}

// clause builds the SQL fragment and parameters for f against one family's
// columns. The fragment is a series of " AND (...)" terms, so it appends to a
// query that already has a WHERE; nextArg is the number of the first free
// parameter (queries here are positional, @p1 upward).
//
// Two rules decide whether pushing a filter down is safe at all, and both are
// implemented here rather than left to the caller:
//
//   - **Case.** Both sides are wrapped in LOWER, so the comparison does not
//     depend on the database's collation. Under a case-sensitive collation a
//     bare LIKE would drop rows a case-insensitive caller expects to keep,
//     which is worse than not filtering at all.
//   - **Wildcards.** The user's text goes through likeEscape and the pattern
//     carries ESCAPE, so `%`, `_` and `[` in an object name are matched
//     literally rather than as pattern syntax.
//
// Dates compare as half-open day ranges for the same reason DateOp documents:
// the column is a timestamp and the criterion is a day.
func (f ObjectFilter) clause(cols filterColumns, nextArg int) (string, []any) {
	var b strings.Builder
	var args []any
	arg := func(v any) string {
		args = append(args, v)
		n := nextArg + len(args) - 1
		return fmt.Sprintf("@p%d", n)
	}

	text := func(col string, criteria []TextCriterion) {
		if col == "" {
			return
		}
		for _, c := range criteria {
			if c.Value == "" {
				continue
			}
			switch c.Op {
			case TextEquals:
				fmt.Fprintf(&b, " AND LOWER(%s) = LOWER(%s)", col, arg(c.Value))
			case TextNotEquals:
				fmt.Fprintf(&b, " AND LOWER(%s) <> LOWER(%s)", col, arg(c.Value))
			case TextNotContains:
				fmt.Fprintf(&b, " AND LOWER(%s) NOT LIKE LOWER(%s) ESCAPE '\\'",
					col, arg("%"+likeEscape(c.Value)+"%"))
			default: // TextContains
				fmt.Fprintf(&b, " AND LOWER(%s) LIKE LOWER(%s) ESCAPE '\\'",
					col, arg("%"+likeEscape(c.Value)+"%"))
			}
		}
	}
	text(cols.name, f.Name)
	text(cols.schema, f.Schema)

	if cols.created != "" {
		for _, c := range f.Created {
			if c.Day.IsZero() {
				continue
			}
			day := time.Date(c.Day.Year(), c.Day.Month(), c.Day.Day(), 0, 0, 0, 0, c.Day.Location())
			next := day.AddDate(0, 0, 1)
			switch c.Op {
			case DateBefore:
				fmt.Fprintf(&b, " AND %s < %s", cols.created, arg(day))
			case DateAfter:
				fmt.Fprintf(&b, " AND %s >= %s", cols.created, arg(next))
			default: // DateOn
				fmt.Fprintf(&b, " AND %s >= %s AND %s < %s",
					cols.created, arg(day), cols.created, arg(next))
			}
		}
	}

	if f.MemoryOptimized != nil && cols.memoryOptimized != "" {
		v := 0
		if *f.MemoryOptimized {
			v = 1
		}
		fmt.Fprintf(&b, " AND %s = %s", cols.memoryOptimized, arg(v))
	}

	return b.String(), args
}
