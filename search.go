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
//
// Both sides of the LIKE are wrapped in LOWER, the rule ObjectFilter.clause
// documents: a bare LIKE follows the database's collation, so on a
// case-sensitive one a search for "customer" never finds Customer. Lowering
// only the column is worse still — a pattern with any upper-case letter then
// matches nothing at all.
func (d *Database) Search(ctx context.Context, pattern string) ([]*SearchResult, error) {
	const q = `
SELECT SCHEMA_NAME(o.schema_id), o.name, o.type_desc
FROM   sys.objects o
WHERE  o.type IN ('U','V','P','FN','IF','TF','TR')
AND    LOWER(o.name) LIKE '%' + LOWER(@p1) + '%' ESCAPE '\'
ORDER  BY o.type_desc, SCHEMA_NAME(o.schema_id), o.name`

	rows, err := d.query(ctx, q, likeEscape(pattern))
	return scanRows(rows, err, fmt.Sprintf("search objects matching %q", pattern), func(scan func(...any) error) (*SearchResult, error) {
		r := &SearchResult{}
		if err := scan(&r.Schema, &r.Name, &r.TypeDesc); err != nil {
			return nil, err
		}
		return r, nil
	})
}
