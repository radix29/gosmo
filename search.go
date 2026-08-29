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
// whose name contains pattern, matching SSMS's Object Explorer Details search
// box. The match is case-insensitive whatever the database's collation is.
func (d *Database) Search(pattern string) ([]*SearchResult, error) {
	return d.SearchContext(context.Background(), pattern)
}

// SearchContext is the context-aware variant of Search.
//
// Both sides of the LIKE are wrapped in LOWER, the rule ObjectFilter.clause
// documents: a bare LIKE follows the database's collation, so on a
// case-sensitive one a search for "customer" never finds Customer. Lowering
// only the column is worse still — a pattern with any upper-case letter then
// matches nothing at all.
func (d *Database) SearchContext(ctx context.Context, pattern string) ([]*SearchResult, error) {
	const q = `
SELECT SCHEMA_NAME(o.schema_id), o.name, o.type_desc
FROM   sys.objects o
WHERE  o.type IN ('U','V','P','FN','IF','TF','TR')
AND    LOWER(o.name) LIKE '%' + LOWER(@p1) + '%' ESCAPE '\'
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
			return nil, fmt.Errorf("gosmo: search objects matching %q: %w", pattern, err)
		}
		results = append(results, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("gosmo: search objects matching %q: %w", pattern, err)
	}
	return results, nil
}
