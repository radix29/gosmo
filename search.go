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

// SearchDefinitions finds the modules — procedures, functions, views,
// DML triggers, rules and defaults — whose T-SQL text contains pattern, case-
// insensitively whatever the collation and with %, _ and [ matched literally
// (Search's rule). An encrypted module has no readable text and is never
// found; a CLR module has none either. A database-level DDL trigger is not a
// schema-scoped object and is not searched.
func (d *Database) SearchDefinitions(ctx context.Context, pattern string) ([]*SearchResult, error) {
	const q = `
SELECT SCHEMA_NAME(o.schema_id), o.name, o.type_desc
FROM   sys.sql_modules m
JOIN   sys.objects o ON o.object_id = m.object_id
WHERE  LOWER(m.definition) LIKE '%' + LOWER(@p1) + '%' ESCAPE '\'
ORDER  BY o.type_desc, SCHEMA_NAME(o.schema_id), o.name`

	rows, err := d.query(ctx, q, likeEscape(pattern))
	return scanRows(rows, err, fmt.Sprintf("search definitions containing %q", pattern), func(scan func(...any) error) (*SearchResult, error) {
		r := &SearchResult{}
		if err := scan(&r.Schema, &r.Name, &r.TypeDesc); err != nil {
			return nil, err
		}
		return r, nil
	})
}

// ColumnSearchResult is one column matched by Database.SearchColumns.
type ColumnSearchResult struct {
	Schema   string
	Object   string // the table or view
	TypeDesc string // "USER_TABLE" or "VIEW"
	Column   string
	// DataType is the column's type as TypeString renders it — nvarchar(50),
	// decimal(9,2) — and a user-defined type schema-qualified ([dbo].[Phone]).
	// A typed xml reads as xml and a vector without its dimensions: the facets
	// are Column.TypeString's, from Table.Columns.
	DataType string
	// IsTypeHidden is Column.IsTypeHidden's: DataType is the base system type
	// ("" for a CLR type) of a user-defined type the login cannot see.
	IsTypeHidden bool
}

// SearchColumns finds the columns of user tables and views whose name
// contains pattern, under Search's matching rule, ordered by schema, object
// and column position.
func (d *Database) SearchColumns(ctx context.Context, pattern string) ([]*ColumnSearchResult, error) {
	q := `
SELECT SCHEMA_NAME(o.schema_id), o.name, o.type_desc, c.name,
       ` + typeNameSQL("c") + `, c.max_length, c.precision, c.scale,
       ISNULL(SCHEMA_NAME(tp.schema_id), N''), ISNULL(tp.is_user_defined, CAST(1 AS bit)), ` + typeHiddenSQL + `
FROM   sys.columns c
JOIN   sys.objects o ON o.object_id = c.object_id
LEFT   JOIN sys.types tp ON tp.user_type_id = c.user_type_id
WHERE  o.type IN ('U','V')
AND    LOWER(c.name) LIKE '%' + LOWER(@p1) + '%' ESCAPE '\'
ORDER  BY SCHEMA_NAME(o.schema_id), o.name, c.column_id`

	rows, err := d.query(ctx, q, likeEscape(pattern))
	return scanRows(rows, err, fmt.Sprintf("search columns matching %q", pattern), func(scan func(...any) error) (*ColumnSearchResult, error) {
		r := &ColumnSearchResult{}
		var ct catalogType
		var dt string
		if err := scan(&r.Schema, &r.Object, &r.TypeDesc, &r.Column,
			&dt, &ct.maxLength, &ct.precision, &ct.scale, &ct.typeSchema, &ct.userDefined, &ct.hidden); err != nil {
			return nil, err
		}
		ct.dt = DataType(dt)
		r.DataType, r.IsTypeHidden = ct.String(), ct.hidden
		return r, nil
	})
}
