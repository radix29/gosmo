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
	CreateDate time.Time
	ModifyDate time.Time
}

// FullName returns the schema-qualified, bracket-quoted name.
func (r *Rule) FullName() string { return qualifiedName(r.Schema, r.Name) }

// Database returns the database the rule belongs to.
func (r *Rule) Database() *Database { return r.db }

// Definition returns the rule's text — the CREATE RULE statement as the
// server stores it — or "" for an encrypted one, and ErrNotFound when there
// is no such rule. Listings do not carry it; a RuleRef is enough.
func (r *Rule) Definition(ctx context.Context) (string, error) {
	return r.db.moduleDefinition(ctx, "rule", "o.type = 'R'", r.Schema, r.Name)
}

// Default mirrors a sys.objects row of type 'D' with parent_object_id = 0 —
// a CREATE DEFAULT object, not a default constraint. Table default
// constraints reach a caller through Column.DefaultValue.
type Default struct {
	db *Database

	Name       string
	Schema     string
	ObjectID   int
	CreateDate time.Time
	ModifyDate time.Time
}

// FullName returns the schema-qualified, bracket-quoted name.
func (df *Default) FullName() string { return qualifiedName(df.Schema, df.Name) }

// Database returns the database the default belongs to.
func (df *Default) Database() *Database { return df.db }

// Definition returns the default's text — the CREATE DEFAULT statement as
// the server stores it — or "" for an encrypted one, and ErrNotFound when
// there is no such standalone default (a default constraint is not one).
// Listings do not carry it; a DefaultRef is enough.
func (df *Default) Definition(ctx context.Context) (string, error) {
	return df.db.moduleDefinition(ctx, "default", "o.type = 'D' AND o.parent_object_id = 0", df.Schema, df.Name)
}

// boundObjectSelect builds the SELECT both families use. typeCode is the
// sys.objects type ('R' or 'D') and extra is the additional predicate that
// separates a standalone default from a default constraint.
func boundObjectSelect(typeCode, extra string) string {
	return `
SELECT o.name, SCHEMA_NAME(o.schema_id), o.object_id,
       o.create_date, o.modify_date
FROM   sys.objects o
WHERE  o.type = '` + typeCode + `'` + extra
}

// ruleSelect and defaultSelect are the two instantiations. The type codes are
// literals here rather than parameters because they are part of the query
// shape, not caller input.
var (
	ruleSelect    = boundObjectSelect("R", "")
	defaultSelect = boundObjectSelect("D", standaloneDefault)
)

// standaloneDefault is the predicate that keeps default constraints out of a
// type 'D' read (see the top of this file).
const standaloneDefault = " AND o.parent_object_id = 0"

// boundObjectDefinitions reads the text of every rule or standalone default
// (typeCode and extra as boundObjectSelect) in one round trip, keyed by
// object_id. NULL — an encrypted object, or one the login may not see — is
// "", as Definition reports it.
func (d *Database) boundObjectDefinitions(ctx context.Context, what, typeCode, extra string) (map[int]string, error) {
	q := `
SELECT o.object_id, OBJECT_DEFINITION(o.object_id)
FROM   sys.objects o
WHERE  o.type = '` + typeCode + `'` + extra

	type objectText struct {
		id  int
		def sql.NullString
	}
	rows, err := d.query(ctx, q)
	texts, err := scanRows(rows, err, fmt.Sprintf("read %s definitions in %q", what, d.Name), func(scan func(...any) error) (objectText, error) {
		var t objectText
		err := scan(&t.id, &t.def)
		return t, err
	})
	if err != nil {
		return nil, err
	}
	out := make(map[int]string, len(texts))
	for _, t := range texts {
		out[t.id] = t.def.String
	}
	return out, nil
}

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
			&r.CreateDate, &r.ModifyDate); err != nil {
			return nil, err
		}
		return r, nil
	})
}

// RuleDefinitions returns the text of every rule in the database in one
// round trip, keyed by Rule.ObjectID: for a listing that shows each rule's
// text, where Definition would cost a round trip per row. A rule created
// since Rules was read may be in it, and one dropped since is not.
func (d *Database) RuleDefinitions(ctx context.Context) (map[int]string, error) {
	return d.boundObjectDefinitions(ctx, "rule", "R", "")
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
			&r.CreateDate, &r.ModifyDate)
	}, ruleSelect+`
   AND SCHEMA_NAME(o.schema_id) COLLATE DATABASE_DEFAULT = @p1 AND o.name = @p2`, schema, name)
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
			&df.CreateDate, &df.ModifyDate); err != nil {
			return nil, err
		}
		return df, nil
	})
}

// DefaultDefinitions is RuleDefinitions for the standalone defaults, keyed
// by Default.ObjectID. Default constraints are not in it.
func (d *Database) DefaultDefinitions(ctx context.Context) (map[int]string, error) {
	return d.boundObjectDefinitions(ctx, "default", "D", standaloneDefault)
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
			&df.CreateDate, &df.ModifyDate)
	}, defaultSelect+`
   AND SCHEMA_NAME(o.schema_id) COLLATE DATABASE_DEFAULT = @p1 AND o.name = @p2`, schema, name)
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
