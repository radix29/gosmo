package gosmo

import (
	"database/sql"
	"encoding/json"
	"fmt"
)

// jsonList renders a correlated subquery that aggregates expr over the rows
// the from clause selects into one JSON array, [{"v":…},{"v":…}], which
// decodeJSONList turns back into a []string.
//
// JSON, not a separator: every earlier form here (FOR XML PATH with STUFF,
// and STRING_AGG, which is 2017 anyway) joined the values with ',' and Go
// split them apart again, so any name containing the separator came back as
// two — a foreign key on column [ref,1] scripted as ([ref], [1]), a role
// member [r, x] as two ADD MEMBER statements. FOR JSON escapes each value,
// so no value can be confused with the framing. It is SQL Server 2016, gosmo's
// floor, and unlike OPENJSON does not depend on the database's compatibility
// level.
//
// orderBy may be empty, for an aggregate whose order does not matter. Over
// zero rows the subquery yields NULL, which decodes to a nil slice.
func jsonList(expr, from, orderBy string) string {
	return jsonRows(expr+" AS v", from, orderBy)
}

// jsonRows is jsonList for more than one column: selectList names each
// column with an alias, which becomes that column's JSON key. A NULL column
// is left out of its object (FOR JSON's default), so it decodes as the zero
// value.
func jsonRows(selectList, from, orderBy string) string {
	order := ""
	if orderBy != "" {
		order = "\n        ORDER BY " + orderBy
	}
	return fmt.Sprintf("(SELECT %s%s%s\n        FOR JSON PATH)", selectList, from, order)
}

// decodeJSONList decodes one jsonList column. NULL (no rows) is a nil slice.
func decodeJSONList(col sql.NullString) ([]string, error) {
	rows, err := decodeJSONRows[struct {
		V string `json:"v"`
	}](col)
	if err != nil || rows == nil {
		return nil, err
	}
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.V
	}
	return out, nil
}

// decodeJSONRows decodes one jsonRows column into a slice of T, whose json
// tags match the aliases jsonRows was given. NULL (no rows) is a nil slice.
func decodeJSONRows[T any](col sql.NullString) ([]T, error) {
	if !col.Valid || col.String == "" {
		return nil, nil
	}
	var rows []T
	if err := json.Unmarshal([]byte(col.String), &rows); err != nil {
		return nil, fmt.Errorf("gosmo: decode FOR JSON list: %w", err)
	}
	return rows, nil
}
