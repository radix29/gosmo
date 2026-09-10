package gosmo

// Plan guides — Programmability ▸ Plan Guides.
//
// A plan guide attaches query hints to a statement the application sends
// verbatim, without changing the application. gosmo reads them and can
// enable, disable and drop one; there is no create, which needs an
// sp_create_plan_guide call whose argument set is the statement itself.
//
// A disabled plan guide is invisible from the query side — the statements it
// covers simply compile without it — so Enable/Disable is the operation that
// makes the folder worth having.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// PlanGuideScope is the scope a plan guide matches on.
type PlanGuideScope string

// The three scope types sys.plan_guides.scope_type_desc reports.
const (
	// PlanGuideScopeObject matches statements inside one routine.
	PlanGuideScopeObject PlanGuideScope = "OBJECT"
	// PlanGuideScopeSQL matches a standalone statement or batch.
	PlanGuideScopeSQL PlanGuideScope = "SQL"
	// PlanGuideScopeTemplate matches statements that parameterize to the
	// same template.
	PlanGuideScopeTemplate PlanGuideScope = "TEMPLATE"
)

// PlanGuide mirrors a sys.plan_guides row.
type PlanGuide struct {
	db *Database

	Name        string
	PlanGuideID int

	// IsDisabled reports a plan guide the optimizer currently ignores.
	IsDisabled bool

	// QueryText is the statement the guide matches, exactly as
	// sp_create_plan_guide was given it — whitespace included, since the
	// match is textual.
	QueryText string

	// Scope is the scope type; ScopeObject is the schema-qualified routine
	// for an OBJECT-scoped guide and empty for the other two.
	Scope       PlanGuideScope
	ScopeObject string

	// ScopeSchema and ScopeName are ScopeObject's two halves, unquoted — the
	// routine as a securable, for a caller that asks about its permissions
	// rather than scripting it. Empty for the SQL and TEMPLATE scopes.
	ScopeSchema string
	ScopeName   string

	// ScopeBatch is the batch text a SQL-scoped guide is bound to, empty
	// when the guide matches the statement in any batch.
	ScopeBatch string

	// Parameters is the parameter list a SQL- or TEMPLATE-scoped guide
	// declares, empty when it has none.
	Parameters string

	// Hints is the OPTION clause the guide applies, or the XML showplan for
	// a guide created from a plan handle.
	Hints string

	CreateDate time.Time
	ModifyDate time.Time
}

// Database returns the database the plan guide belongs to.
func (g *PlanGuide) Database() *Database { return g.db }

// planGuideSelect is the SELECT the listing and the by-name finder share.
//
// scope_object_id is NULL for every scope but OBJECT, and OBJECT_NAME/
// OBJECT_SCHEMA_NAME of NULL is NULL, so the qualified name is built with
// ISNULL around the whole concatenation rather than around each half — the
// per-half form would yield a stray "[]." for the non-OBJECT scopes.
const planGuideSelect = `
SELECT g.plan_guide_id, g.name, g.is_disabled,
       ISNULL(g.query_text, ''),
       ISNULL(g.scope_type_desc, ''),
       ISNULL(QUOTENAME(OBJECT_SCHEMA_NAME(g.scope_object_id)) + '.' +
              QUOTENAME(OBJECT_NAME(g.scope_object_id)), ''),
       ISNULL(OBJECT_SCHEMA_NAME(g.scope_object_id), ''),
       ISNULL(OBJECT_NAME(g.scope_object_id), ''),
       ISNULL(g.scope_batch, ''), ISNULL(g.parameters, ''),
       ISNULL(g.hints, ''),
       g.create_date, g.modify_date
FROM   sys.plan_guides g`

func scanPlanGuide(d *Database, scan func(...any) error) (*PlanGuide, error) {
	g := &PlanGuide{db: d}
	var scope string
	if err := scan(&g.PlanGuideID, &g.Name, &g.IsDisabled,
		&g.QueryText, &scope, &g.ScopeObject, &g.ScopeSchema, &g.ScopeName,
		&g.ScopeBatch, &g.Parameters, &g.Hints,
		&g.CreateDate, &g.ModifyDate); err != nil {
		return nil, err
	}
	g.Scope = PlanGuideScope(scope)
	return g, nil
}

// PlanGuides returns the plan guides defined in the database.
func (d *Database) PlanGuides() ([]*PlanGuide, error) {
	return d.PlanGuidesContext(context.Background())
}

// PlanGuidesContext is the context-aware variant of PlanGuides.
func (d *Database) PlanGuidesContext(ctx context.Context) ([]*PlanGuide, error) {
	const q = planGuideSelect + `
ORDER  BY g.name`

	rows, err := d.query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("gosmo: list plan guides in %q: %w", d.name, err)
	}
	defer rows.Close()

	var guides []*PlanGuide
	for rows.Next() {
		g, err := scanPlanGuide(d, rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("gosmo: list plan guides in %q: %w", d.name, err)
		}
		guides = append(guides, g)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("gosmo: list plan guides in %q: %w", d.name, err)
	}
	return guides, nil
}

// PlanGuideByName returns one plan guide with every field populated, or a
// not-found error (errors.Is ErrNotFound) when the database has none by that
// name.
func (d *Database) PlanGuideByName(name string) (*PlanGuide, error) {
	return d.PlanGuideByNameContext(context.Background(), name)
}

// PlanGuideByNameContext is the context-aware variant of PlanGuideByName.
func (d *Database) PlanGuideByNameContext(ctx context.Context, name string) (*PlanGuide, error) {
	var g *PlanGuide
	err := d.queryRow(ctx, func(row *sql.Row) error {
		var err error
		g, err = scanPlanGuide(d, row.Scan)
		return err
	}, planGuideSelect+`
WHERE  g.name = @p1`, name)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, notFoundf("gosmo: plan guide %q not found in %q", name, d.name)
	}
	if err != nil {
		return nil, fmt.Errorf("gosmo: read plan guide %q in %q: %w", name, d.name, err)
	}
	return g, nil
}

// PlanGuide returns a lightweight handle for a plan guide by name, without
// querying sys.plan_guides — the counterpart of Server.Database and
// Database.DatabaseTrigger.
//
// Every other field stays at its zero value; PlanGuideByName is what
// populates them. Enable, Disable and Drop address the guide by name, so
// this handle is enough to act on one the caller already knows exists, and is
// the only usable form under a WithScript context, where
// PlanGuideByNameContext's lookup is a real read.
func (d *Database) PlanGuide(name string) *PlanGuide {
	return &PlanGuide{db: d, Name: name}
}

// ============================================================
// Enable / disable / drop
// ============================================================

// controlPlanGuide issues one sp_control_plan_guide operation. The operation
// is a fixed literal chosen by the caller, never caller input; the name goes
// as a parameter.
func (g *PlanGuide) controlPlanGuide(ctx context.Context, operation, verb string) error {
	_, err := g.db.exec(ctx,
		"EXEC sp_control_plan_guide @operation = @p1, @name = @p2",
		operation, g.Name)
	if err != nil {
		return fmt.Errorf("gosmo: %s plan guide %q in %q: %w", verb, g.Name, g.db.name, err)
	}
	return nil
}

// Enable enables the plan guide.
func (g *PlanGuide) Enable() error { return g.EnableContext(context.Background()) }

// EnableContext is the context-aware variant of Enable.
func (g *PlanGuide) EnableContext(ctx context.Context) error {
	if err := g.controlPlanGuide(ctx, "ENABLE", "enable"); err != nil {
		return err
	}
	setIfApplied(ctx, &g.IsDisabled, false)
	return nil
}

// Disable disables the plan guide. The optimizer then ignores it; the guide
// itself stays defined.
func (g *PlanGuide) Disable() error { return g.DisableContext(context.Background()) }

// DisableContext is the context-aware variant of Disable.
func (g *PlanGuide) DisableContext(ctx context.Context) error {
	if err := g.controlPlanGuide(ctx, "DISABLE", "disable"); err != nil {
		return err
	}
	setIfApplied(ctx, &g.IsDisabled, true)
	return nil
}

// Drop drops the plan guide.
func (g *PlanGuide) Drop() error { return g.DropContext(context.Background()) }

// DropContext is the context-aware variant of Drop.
func (g *PlanGuide) DropContext(ctx context.Context) error {
	return g.controlPlanGuide(ctx, "DROP", "drop")
}

// DropPlanGuide drops a plan guide by name — the form for a caller that has
// the name but not the object.
func (d *Database) DropPlanGuide(name string) error {
	return d.DropPlanGuideContext(context.Background(), name)
}

// DropPlanGuideContext is the context-aware variant of DropPlanGuide.
func (d *Database) DropPlanGuideContext(ctx context.Context, name string) error {
	return d.PlanGuide(name).DropContext(ctx)
}
