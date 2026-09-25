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
	if err := requireSchema("rule by name", schema, name); err != nil {
		return nil, err
	}
	r := &Rule{db: d}
	err := d.queryRow(ctx, func(row *sql.Row) error {
		return row.Scan(&r.Name, &r.Schema, &r.ObjectID,
			&r.Definition, &r.CreateDate, &r.ModifyDate)
	}, ruleSelect+`
   AND SCHEMA_NAME(o.schema_id) = @p1 AND o.name = @p2`, schema, name)
	return foundRow(r, err, notFoundf("gosmo: rule %s not found in %q", qualifiedName(schema, name), d.Name), fmt.Sprintf("read rule %s in %q", qualifiedName(schema, name), d.Name))
}

// RuleRef returns a lightweight handle for a rule by name, without
// querying the catalog — the counterpart of Server.DatabaseRef. Every field
// but the schema and name stays at its zero value; RuleByName is what populates them.
//
// Every write on *Rule addresses it by name, so this handle is enough to
// drop one the caller already knows exists — and is the form to use when
// there is nothing to read yet, such as a script of a CREATE that was only
// collected.
//
// schema is taken as given: an empty one is refused by the handle's writes
// (ErrSchemaRequired), never defaulted.
func (d *Database) RuleRef(schema, name string) *Rule {
	return &Rule{db: d, Schema: schema, Name: name}
}

// Drop drops the rule. A rule still bound to a column or type is refused by
// the server until sp_unbindrule releases it.
func (r *Rule) Drop(ctx context.Context) error {
	if err := requireSchema("drop rule", r.Schema, r.Name); err != nil {
		return err
	}
	if _, err := r.db.exec(ctx, "DROP RULE "+qualifiedName(r.Schema, r.Name)); err != nil {
		return fmt.Errorf("gosmo: drop rule %s: %w", qualifiedName(r.Schema, r.Name), err)
	}
	return nil
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
	if err := requireSchema("default by name", schema, name); err != nil {
		return nil, err
	}
	df := &Default{db: d}
	err := d.queryRow(ctx, func(row *sql.Row) error {
		return row.Scan(&df.Name, &df.Schema, &df.ObjectID,
			&df.Definition, &df.CreateDate, &df.ModifyDate)
	}, defaultSelect+`
   AND SCHEMA_NAME(o.schema_id) = @p1 AND o.name = @p2`, schema, name)
	return foundRow(df, err, notFoundf("gosmo: default %s not found in %q", qualifiedName(schema, name), d.Name), fmt.Sprintf("read default %s in %q", qualifiedName(schema, name), d.Name))
}

// DefaultRef returns a lightweight handle for a standalone default by name, without
// querying the catalog — the counterpart of Server.DatabaseRef. Every field
// but the schema and name stays at its zero value; DefaultByName is what populates them.
//
// Every write on *Default addresses it by name, so this handle is enough to
// drop one the caller already knows exists — and is the form to use when
// there is nothing to read yet, such as a script of a CREATE that was only
// collected.
//
// schema is taken as given: an empty one is refused by the handle's writes
// (ErrSchemaRequired), never defaulted.
func (d *Database) DefaultRef(schema, name string) *Default {
	return &Default{db: d, Schema: schema, Name: name}
}

// Drop drops the default. A default still bound to a column or type is refused
// by the server until sp_unbindefault releases it.
func (df *Default) Drop(ctx context.Context) error {
	if err := requireSchema("drop default", df.Schema, df.Name); err != nil {
		return err
	}
	if _, err := df.db.exec(ctx, "DROP DEFAULT "+qualifiedName(df.Schema, df.Name)); err != nil {
		return fmt.Errorf("gosmo: drop default %s: %w", qualifiedName(df.Schema, df.Name), err)
	}
	return nil
}

// Rename renames the rule (sp_rename's 'OBJECT' class). newName is a bare
// name; a rename never moves the rule between schemas — see Transfer.
func (r *Rule) Rename(ctx context.Context, newName string) error {
	if err := r.db.renameSchemaObject(ctx, "rule", renameObjectClass, r.Schema, r.Name, newName); err != nil {
		return err
	}
	setIfApplied(ctx, &r.Name, newName)
	return nil
}

// Transfer moves the rule into another schema (ALTER SCHEMA ... TRANSFER).
// It keeps its name and object_id; permissions granted on it directly are
// dropped by the server.
func (r *Rule) Transfer(ctx context.Context, targetSchema string) error {
	if err := r.db.transferSchemaObject(ctx, "rule", transferObjectClass, targetSchema, r.Schema, r.Name); err != nil {
		return err
	}
	setIfApplied(ctx, &r.Schema, targetSchema)
	return nil
}

// Rename renames the default (sp_rename's 'OBJECT' class). newName is a bare
// name; a rename never moves the default between schemas — see Transfer.
func (df *Default) Rename(ctx context.Context, newName string) error {
	if err := df.db.renameSchemaObject(ctx, "default", renameObjectClass, df.Schema, df.Name, newName); err != nil {
		return err
	}
	setIfApplied(ctx, &df.Name, newName)
	return nil
}

// Transfer moves the default into another schema (ALTER SCHEMA ... TRANSFER).
// It keeps its name and object_id; permissions granted on it directly are
// dropped by the server.
func (df *Default) Transfer(ctx context.Context, targetSchema string) error {
	if err := df.db.transferSchemaObject(ctx, "default", transferObjectClass, targetSchema, df.Schema, df.Name); err != nil {
		return err
	}
	setIfApplied(ctx, &df.Schema, targetSchema)
	return nil
}
