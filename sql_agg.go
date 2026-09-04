package gosmo

import "fmt"

// commaList renders a comma-separated aggregate of expr over the rows the
// from clause selects — the shape `STRING_AGG(expr, ',') WITHIN GROUP (ORDER
// BY orderBy)` has, written as a correlated FOR XML PATH subquery instead.
//
// STRING_AGG is SQL Server 2017; this form works from 2008 and so at gosmo's
// whole supported range. Both are metadata reads over tens of rows, so the
// older construct costs nothing measurable, and one rendering is what keeps
// the two from drifting apart in seven separate queries.
//
// Two details are load-bearing, and a plausible simplification drops either:
//
//   - `, TYPE).value('.', 'nvarchar(max)')` rather than letting FOR XML return
//     its string directly. Without it an identifier containing &, < or > — all
//     legal in a SQL Server name — comes back XML-escaped as &amp;, &lt;, &gt;
//     and never matches the object it names.
//   - `STUFF(…, 1, 1, ”)` strips the leading separator the `',' + expr`
//     concatenation puts in front of every row, including the first.
//
// orderBy may be empty, for an aggregate whose order does not matter. Over
// zero rows this yields NULL, exactly as STRING_AGG does.
func commaList(expr, from, orderBy string) string {
	order := ""
	if orderBy != "" {
		order = "\n        ORDER BY " + orderBy
	}
	return fmt.Sprintf("STUFF((SELECT ',' + %s%s%s\n        FOR XML PATH(''), TYPE).value('.', 'nvarchar(max)'), 1, 1, '')",
		expr, from, order)
}
