package gosmo

import (
	"context"
	"fmt"
	"strings"
)

// SecurableRef names one schema-scoped object a permission can be granted
// on. Type is "SCHEMA", "TABLE" or "VIEW", matching the SecurableType
// strings PrincipalSecurable reports; Schema is empty for a "SCHEMA".
//
// This is deliberately just the identity of the securable, not the object:
// a permissions UI listing candidates to grant on needs the name and the
// type and nothing else, and loading Table or View values for thousands of
// candidates to show a picker is work no caller wants.
type SecurableRef struct {
	Type   string
	Schema string
	Name   string
}

// SecurableSearch narrows FindSecurables.
//
// Name is matched case-insensitively as a substring of the securable's
// qualified name — "dbo.Orders" for a table, the bare name for a schema —
// so both "ord" and "dbo.ord" find dbo.Orders. LIKE wildcards in it are
// matched literally, not as wildcards. An empty Name matches everything.
//
// Limit caps the rows returned; 0 means no cap. Results are ordered
// schemas, then tables, then views, each by qualified name, so a capped
// search returns a stable prefix rather than an arbitrary subset.
type SecurableSearch struct {
	Name  string
	Limit int
}

// FindSecurables returns the schemas, tables and views matching search.
func (d *Database) FindSecurables(search SecurableSearch) ([]SecurableRef, error) {
	return d.FindSecurablesContext(context.Background(), search)
}

// FindSecurablesContext is the context-aware variant of FindSecurables.
//
// One query over sys.schemas, sys.tables and sys.views, for a caller that
// needs candidates matching what the user typed rather than the whole
// catalog — a database with thousands of tables makes "list everything and
// filter in the client" both slow to open and useless as a picker.
func (d *Database) FindSecurablesContext(ctx context.Context, search SecurableSearch) ([]SecurableRef, error) {
	top := ""
	if search.Limit > 0 {
		top = "TOP (@p2) "
	}
	q := `
SELECT ` + top + `x.kind, x.rank, x.[schema], x.name
FROM (
    SELECT 'SCHEMA' AS kind, 0 AS rank, '' AS [schema], s.name AS name,
           LOWER(s.name) AS qualified
    FROM   sys.schemas s
    JOIN   sys.database_principals p ON p.principal_id = s.principal_id
    UNION ALL
    SELECT 'TABLE', 1, SCHEMA_NAME(t.schema_id), t.name,
           LOWER(SCHEMA_NAME(t.schema_id) + '.' + t.name)
    FROM   sys.tables t
    WHERE  t.is_ms_shipped = 0
    UNION ALL
    SELECT 'VIEW', 2, SCHEMA_NAME(v.schema_id), v.name,
           LOWER(SCHEMA_NAME(v.schema_id) + '.' + v.name)
    FROM   sys.views v
    WHERE  v.is_ms_shipped = 0
) x
WHERE  x.qualified LIKE @p1 ESCAPE '\'
ORDER  BY x.rank, x.qualified`

	args := []any{"%" + likeEscape(strings.ToLower(search.Name)) + "%"}
	if search.Limit > 0 {
		args = append(args, search.Limit)
	}

	rows, err := d.query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("gosmo: find securables in %q: %w", d.name, err)
	}
	defer rows.Close()

	var refs []SecurableRef
	for rows.Next() {
		var r SecurableRef
		var rank int
		if err := rows.Scan(&r.Type, &rank, &r.Schema, &r.Name); err != nil {
			return nil, fmt.Errorf("gosmo: find securables in %q: %w", d.name, err)
		}
		refs = append(refs, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("gosmo: find securables in %q: %w", d.name, err)
	}
	return refs, nil
}
