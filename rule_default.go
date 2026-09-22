package gosmo

// Rules and defaults — Programmability ▸ Rules and ▸ Defaults.
//
// Both are legacy: Microsoft deprecated CREATE RULE / sp_bindrule and
// CREATE DEFAULT / sp_bindefault in SQL Server 2008 in favour of check
// constraints and default constraints. gosmo reads them because databases
// that predate that are still in service; it deliberately offers no create.
//
// The trap is on the defaults side. sys.objects type 'D' covers *both*
// standalone defaults (the CREATE DEFAULT object, which is what belongs in
// the Defaults folder) and every DF_… default constraint on every table. A
// standalone default has parent_object_id = 0; without that predicate the
// listing is mostly table constraints. Rules have no such overlap — type 'R'
// is only ever a standalone rule — but the two listings are otherwise
// identical, so they share one query builder below.

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// Rule mirrors a sys.objects row of type 'R' — a CREATE RULE object.
type Rule struct {
	db *Database

	Name       string
	Schema     string
	ObjectID   int
	Definition string
	CreateDate time.Time
	ModifyDate time.Time
}

// FullName returns the schema-qualified, bracket-quoted name.
func (r *Rule) FullName() string { return qualifiedName(r.Schema, r.Name) }

// Database returns the database the rule belongs to.
func (r *Rule) Database() *Database { return r.db }

// Default mirrors a sys.objects row of type 'D' with parent_object_id = 0 —
// a CREATE DEFAULT object, not a default constraint. Table default
// constraints reach a caller through Column.DefaultValue.
type Default struct {
	db *Database

	Name       string
	Schema     string
	ObjectID   int
	Definition string
	CreateDate time.Time
	ModifyDate time.Time
}

// FullName returns the schema-qualified, bracket-quoted name.
func (df *Default) FullName() string { return qualifiedName(df.Schema, df.Name) }

// Database returns the database the default belongs to.
func (df *Default) Database() *Database { return df.db }

// boundObjectSelect builds the SELECT both families use. typeCode is the
// sys.objects type ('R' or 'D') and extra is the additional predicate that
// separates a standalone default from a default constraint.
//
// The join to sys.sql_modules is a LEFT join even though every rule and
// default has a module: an encrypted one has a row with a NULL definition,
// and an inner join plus a bare scan would drop the object from the listing
// entirely rather than showing it with no text.
func boundObjectSelect(typeCode, extra string) string {
	return `
SELECT o.name, SCHEMA_NAME(o.schema_id), o.object_id,
       ISNULL(m.definition, ''), o.create_date, o.modify_date
FROM   sys.objects o
LEFT   JOIN sys.sql_modules m ON m.object_id = o.object_id
WHERE  o.type = '` + typeCode + `'` + extra
}

// ruleSelect and defaultSelect are the two instantiations. The type codes are
// literals here rather than parameters because they are part of the query
// shape, not caller input.
var (
	ruleSelect    = boundObjectSelect("R", "")
	defaultSelect = boundObjectSelect("D", " AND o.parent_object_id = 0")
)

// ============================================================
// Rules
// ============================================================

// Rules returns the standalone rules defined in the database.
func (d *Database) Rules(ctx context.Context) ([]*Rule, error) {
	q := ruleSelect + `
ORDER  BY SCHEMA_NAME(o.schema_id), o.name`

	rows, err := d.query(ctx, q)
	return scanRows(rows, err, fmt.Sprintf("list rules in %q", d.Name), func(scan func(...any) error) (*Rule, error) {
		r := &Rule{db: d}
		if err := scan(&r.Name, &r.Schema, &r.ObjectID,
			&r.Definition, &r.CreateDate, &r.ModifyDate); err != nil {
			return nil, err
		}
		return r, nil
	})
}

// RuleByName returns one rule, or a not-found error (errors.Is ErrNotFound)
// when the database has none by that name.
func (d *Database) RuleByName(ctx context.Context, schema, name string) (*Rule, error) {
	if schema == "" {
		schema = "dbo"
	}
	r := &Rule{db: d}
	err := d.queryRow(ctx, func(row *sql.Row) error {
		return row.Scan(&r.Name, &r.Schema, &r.ObjectID,
			&r.Definition, &r.CreateDate, &r.ModifyDate)
	}, ruleSelect+`
   AND SCHEMA_NAME(o.schema_id) = @p1 AND o.name = @p2`, schema, name)
	return foundRow(r, err, notFoundf("gosmo: rule [%s].[%s] not found in %q", schema, name, d.Name), fmt.Sprintf("read rule [%s].[%s] in %q", schema, name, d.Name))
}

// DropRule drops a rule by name. A rule still bound to a column or type is
// refused by the server until sp_unbindrule releases it.
func (d *Database) DropRule(ctx context.Context, schema, name string) error {
	if schema == "" {
		schema = "dbo"
	}
	if _, err := d.exec(ctx, "DROP RULE "+qualifiedName(schema, name)); err != nil {
		return fmt.Errorf("gosmo: drop rule [%s].[%s]: %w", schema, name, err)
	}
	return nil
}

// Drop drops the rule.
func (r *Rule) Drop(ctx context.Context) error {
	return r.db.DropRule(ctx, r.Schema, r.Name)
}

// ============================================================
// Defaults
// ============================================================

// Defaults returns the standalone defaults defined in the database — the
// CREATE DEFAULT objects, not table default constraints.
func (d *Database) Defaults(ctx context.Context) ([]*Default, error) {
	q := defaultSelect + `
ORDER  BY SCHEMA_NAME(o.schema_id), o.name`

	rows, err := d.query(ctx, q)
	return scanRows(rows, err, fmt.Sprintf("list defaults in %q", d.Name), func(scan func(...any) error) (*Default, error) {
		df := &Default{db: d}
		if err := scan(&df.Name, &df.Schema, &df.ObjectID,
			&df.Definition, &df.CreateDate, &df.ModifyDate); err != nil {
			return nil, err
		}
		return df, nil
	})
}

// DefaultByName returns one standalone default, or a not-found error
// (errors.Is ErrNotFound) when the database has none by that name.
func (d *Database) DefaultByName(ctx context.Context, schema, name string) (*Default, error) {
	if schema == "" {
		schema = "dbo"
	}
	df := &Default{db: d}
	err := d.queryRow(ctx, func(row *sql.Row) error {
		return row.Scan(&df.Name, &df.Schema, &df.ObjectID,
			&df.Definition, &df.CreateDate, &df.ModifyDate)
	}, defaultSelect+`
   AND SCHEMA_NAME(o.schema_id) = @p1 AND o.name = @p2`, schema, name)
	return foundRow(df, err, notFoundf("gosmo: default [%s].[%s] not found in %q", schema, name, d.Name), fmt.Sprintf("read default [%s].[%s] in %q", schema, name, d.Name))
}

// DropDefault drops a standalone default by name. A default still bound to a
// column or type is refused by the server until sp_unbindefault releases it.
func (d *Database) DropDefault(ctx context.Context, schema, name string) error {
	if schema == "" {
		schema = "dbo"
	}
	if _, err := d.exec(ctx, "DROP DEFAULT "+qualifiedName(schema, name)); err != nil {
		return fmt.Errorf("gosmo: drop default [%s].[%s]: %w", schema, name, err)
	}
	return nil
}

// Drop drops the default.
func (df *Default) Drop(ctx context.Context) error {
	return df.db.DropDefault(ctx, df.Schema, df.Name)
}
