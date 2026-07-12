package gosmo

import (
	"context"
	"fmt"
)

// ============================================================
// Object search
// ============================================================

// SearchResult is one object matched by Database.Search.
type SearchResult struct {
	Schema   string
	Name     string
	TypeDesc string // e.g. "USER_TABLE", "VIEW", "SQL_STORED_PROCEDURE"
}

// Search finds tables, views, stored procedures, functions, and triggers
// whose name contains pattern (case-insensitivity follows the database's
// own collation), matching SSMS's Object Explorer Details search box.
func (d *Database) Search(pattern string) ([]*SearchResult, error) {
	return d.SearchContext(context.Background(), pattern)
}

// SearchContext is the context-aware variant of Search.
func (d *Database) SearchContext(ctx context.Context, pattern string) ([]*SearchResult, error) {
	const q = `
SELECT SCHEMA_NAME(o.schema_id), o.name, o.type_desc
FROM   sys.objects o
WHERE  o.type IN ('U','V','P','FN','IF','TF','TR')
AND    o.name LIKE '%' + @p1 + '%' ESCAPE '\'
ORDER  BY o.type_desc, SCHEMA_NAME(o.schema_id), o.name`

	rows, err := d.query(ctx, q, likeEscape(pattern))
	if err != nil {
		return nil, fmt.Errorf("gosmo: search objects matching %q: %w", pattern, err)
	}
	defer rows.Close()

	var results []*SearchResult
	for rows.Next() {
		r := &SearchResult{}
		if err := rows.Scan(&r.Schema, &r.Name, &r.TypeDesc); err != nil {
			return nil, err
		}
		results = append(results, r)
	}
	return results, rows.Err()
}
