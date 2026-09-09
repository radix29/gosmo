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
	"errors"
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
func (d *Database) Rules() ([]*Rule, error) {
	return d.RulesContext(context.Background())
}

// RulesContext is the context-aware variant of Rules.
func (d *Database) RulesContext(ctx context.Context) ([]*Rule, error) {
	q := ruleSelect + `
ORDER  BY SCHEMA_NAME(o.schema_id), o.name`

	rows, err := d.query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("gosmo: list rules in %q: %w", d.name, err)
	}
	defer rows.Close()

	var rules []*Rule
	for rows.Next() {
		r := &Rule{db: d}
		if err := rows.Scan(&r.Name, &r.Schema, &r.ObjectID,
			&r.Definition, &r.CreateDate, &r.ModifyDate); err != nil {
			return nil, fmt.Errorf("gosmo: list rules in %q: %w", d.name, err)
		}
		rules = append(rules, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("gosmo: list rules in %q: %w", d.name, err)
	}
	return rules, nil
}

// RuleByName returns one rule, or a not-found error (errors.Is ErrNotFound)
// when the database has none by that name.
func (d *Database) RuleByName(schema, name string) (*Rule, error) {
	return d.RuleByNameContext(context.Background(), schema, name)
}

// RuleByNameContext is the context-aware variant of RuleByName.
func (d *Database) RuleByNameContext(ctx context.Context, schema, name string) (*Rule, error) {
	if schema == "" {
		schema = "dbo"
	}
	r := &Rule{db: d}
	err := d.queryRow(ctx, func(row *sql.Row) error {
		return row.Scan(&r.Name, &r.Schema, &r.ObjectID,
			&r.Definition, &r.CreateDate, &r.ModifyDate)
	}, ruleSelect+`
   AND SCHEMA_NAME(o.schema_id) = @p1 AND o.name = @p2`, schema, name)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, notFoundf("gosmo: rule [%s].[%s] not found in %q", schema, name, d.name)
	}
	if err != nil {
		return nil, fmt.Errorf("gosmo: read rule [%s].[%s] in %q: %w", schema, name, d.name, err)
	}
	return r, nil
}

// DropRule drops a rule by name. A rule still bound to a column or type is
// refused by the server until sp_unbindrule releases it.
func (d *Database) DropRule(schema, name string) error {
	return d.DropRuleContext(context.Background(), schema, name)
}

// DropRuleContext is the context-aware variant of DropRule.
func (d *Database) DropRuleContext(ctx context.Context, schema, name string) error {
	if schema == "" {
		schema = "dbo"
	}
	if _, err := d.exec(ctx, "DROP RULE "+qualifiedName(schema, name)); err != nil {
		return fmt.Errorf("gosmo: drop rule [%s].[%s]: %w", schema, name, err)
	}
	return nil
}

// Drop drops the rule.
func (r *Rule) Drop() error { return r.DropContext(context.Background()) }

// DropContext is the context-aware variant of Drop.
func (r *Rule) DropContext(ctx context.Context) error {
	return r.db.DropRuleContext(ctx, r.Schema, r.Name)
}

// ============================================================
// Defaults
// ============================================================

// Defaults returns the standalone defaults defined in the database — the
// CREATE DEFAULT objects, not table default constraints.
func (d *Database) Defaults() ([]*Default, error) {
	return d.DefaultsContext(context.Background())
}

// DefaultsContext is the context-aware variant of Defaults.
func (d *Database) DefaultsContext(ctx context.Context) ([]*Default, error) {
	q := defaultSelect + `
ORDER  BY SCHEMA_NAME(o.schema_id), o.name`

	rows, err := d.query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("gosmo: list defaults in %q: %w", d.name, err)
	}
	defer rows.Close()

	var defs []*Default
	for rows.Next() {
		df := &Default{db: d}
		if err := rows.Scan(&df.Name, &df.Schema, &df.ObjectID,
			&df.Definition, &df.CreateDate, &df.ModifyDate); err != nil {
			return nil, fmt.Errorf("gosmo: list defaults in %q: %w", d.name, err)
		}
		defs = append(defs, df)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("gosmo: list defaults in %q: %w", d.name, err)
	}
	return defs, nil
}

// DefaultByName returns one standalone default, or a not-found error
// (errors.Is ErrNotFound) when the database has none by that name.
func (d *Database) DefaultByName(schema, name string) (*Default, error) {
	return d.DefaultByNameContext(context.Background(), schema, name)
}

// DefaultByNameContext is the context-aware variant of DefaultByName.
func (d *Database) DefaultByNameContext(ctx context.Context, schema, name string) (*Default, error) {
	if schema == "" {
		schema = "dbo"
	}
	df := &Default{db: d}
	err := d.queryRow(ctx, func(row *sql.Row) error {
		return row.Scan(&df.Name, &df.Schema, &df.ObjectID,
			&df.Definition, &df.CreateDate, &df.ModifyDate)
	}, defaultSelect+`
   AND SCHEMA_NAME(o.schema_id) = @p1 AND o.name = @p2`, schema, name)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, notFoundf("gosmo: default [%s].[%s] not found in %q", schema, name, d.name)
	}
	if err != nil {
		return nil, fmt.Errorf("gosmo: read default [%s].[%s] in %q: %w", schema, name, d.name, err)
	}
	return df, nil
}

// DropDefault drops a standalone default by name. A default still bound to a
// column or type is refused by the server until sp_unbindefault releases it.
func (d *Database) DropDefault(schema, name string) error {
	return d.DropDefaultContext(context.Background(), schema, name)
}

// DropDefaultContext is the context-aware variant of DropDefault.
func (d *Database) DropDefaultContext(ctx context.Context, schema, name string) error {
	if schema == "" {
		schema = "dbo"
	}
	if _, err := d.exec(ctx, "DROP DEFAULT "+qualifiedName(schema, name)); err != nil {
		return fmt.Errorf("gosmo: drop default [%s].[%s]: %w", schema, name, err)
	}
	return nil
}

// Drop drops the default.
func (df *Default) Drop() error { return df.DropContext(context.Background()) }

// DropContext is the context-aware variant of Drop.
func (df *Default) DropContext(ctx context.Context) error {
	return df.db.DropDefaultContext(ctx, df.Schema, df.Name)
}
