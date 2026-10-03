package gosmo

// security_policy.go is row-level security: a policy, the predicates attached
// to it, and the enable/disable/drop that act on the whole policy. The
// GRANT/DENY/REVOKE surface is in security.go.

import (
	"context"
	"database/sql"
	"fmt"
)

// SecurityPolicy mirrors sys.security_policies.
type SecurityPolicy struct {
	db                  *Database
	Name                string
	Schema              string
	ObjectID            int
	IsEnabled           bool
	IsNotForReplication bool
	// IsSchemaBound reports whether the policy binds the schema of the
	// tables and predicate functions it names, which blocks any change to
	// them while it exists. Part of the CREATE statement, so scripting a
	// policy without it produces one that behaves differently.
	IsSchemaBound bool
	Predicates    []*SecurityPredicate
}

// Database returns the database the security policy belongs to.
func (p *SecurityPolicy) Database() *Database { return p.db }

// SecurityPredicate represents one predicate in a security policy.
type SecurityPredicate struct {
	PredicateType       string // "FILTER" or "BLOCK"
	PredicateDefinition string
	TargetSchema        string
	TargetTable         string
	Operation           string // for BLOCK: AFTER INSERT, AFTER UPDATE, etc.
}

// securityPolicySelect is the column list every security policy read
// shares; the listing adds ORDER BY, the by-name lookup a WHERE.
const securityPolicySelect = `
SELECT sp.name, SCHEMA_NAME(sp.schema_id), sp.object_id,
       sp.is_enabled, sp.is_not_for_replication, sp.is_schema_bound
FROM   sys.security_policies sp`

// SecurityPolicies returns all security policies in the database.
func (d *Database) SecurityPolicies(ctx context.Context) ([]*SecurityPolicy, error) {
	rows, err := d.query(ctx, securityPolicySelect+`
ORDER  BY sp.name`)
	if err != nil {
		return nil, fmt.Errorf("gosmo: list security policies: %w", err)
	}
	defer rows.Close()

	var policies []*SecurityPolicy
	for rows.Next() {
		p, err := scanSecurityPolicy(d, rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("gosmo: list security policies: %w", err)
		}
		policies = append(policies, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("gosmo: list security policies: %w", err)
	}
	if len(policies) == 0 {
		return policies, nil
	}
	// Every policy's predicates in one query, grouped here, after the row
	// scan — not one query per policy, and never one while the outer rows
	// are still open.
	byPolicy, err := d.securityPredicates(ctx, "")
	if err != nil {
		return nil, fmt.Errorf("gosmo: list security policies: predicates in %q: %w", d.Name, err)
	}
	for _, p := range policies {
		p.Predicates = byPolicy[p.ObjectID]
	}
	return policies, nil
}

// SecurityPolicyByName returns one security policy by schema-qualified name.
func (d *Database) SecurityPolicyByName(ctx context.Context, schema, name string) (*SecurityPolicy, error) {
	if err := requireSchema("security policy by name", schema, name); err != nil {
		return nil, err
	}
	p, err := readByName(ctx, d, scanSecurityPolicy, securityPolicySelect+`
WHERE  SCHEMA_NAME(sp.schema_id) = @p1
  AND  sp.name                   = @p2`, []any{schema, name},
		notFoundf("gosmo: security policy %s not found in %q", qualifiedName(schema, name), d.Name),
		fmt.Sprintf("find security policy %s in %q", qualifiedName(schema, name), d.Name))
	if err != nil {
		return nil, err
	}
	if err := d.loadSecurityPredicates(ctx, p); err != nil {
		return nil, err
	}
	return p, nil
}

func scanSecurityPolicy(d *Database, scan func(...any) error) (*SecurityPolicy, error) {
	p := &SecurityPolicy{db: d}
	if err := scan(&p.Name, &p.Schema, &p.ObjectID,
		&p.IsEnabled, &p.IsNotForReplication, &p.IsSchemaBound); err != nil {
		return nil, err
	}
	return p, nil
}

func (d *Database) loadSecurityPredicates(ctx context.Context, p *SecurityPolicy) error {
	byPolicy, err := d.securityPredicates(ctx, "AND spr.object_id = @p1", p.ObjectID)
	if err != nil {
		return fmt.Errorf("gosmo: predicates of security policy %q in %q: %w", p.Name, d.Name, err)
	}
	p.Predicates = byPolicy[p.ObjectID]
	return nil
}

// securityPredicates reads the predicates where admits (an extra predicate
// on spr, or "" for every policy's), grouped by the policy's object_id.
func (d *Database) securityPredicates(ctx context.Context, where string, args ...any) (map[int][]*SecurityPredicate, error) {
	q := `
SELECT spr.object_id, spr.predicate_type_desc, spr.predicate_definition,
       SCHEMA_NAME(t.schema_id), t.name, spr.operation_desc
FROM   sys.security_predicates spr
JOIN   sys.tables t ON t.object_id = spr.target_object_id
WHERE  1 = 1 ` + where + `
ORDER  BY spr.object_id, spr.predicate_type_desc`

	rows, err := d.query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	byPolicy := map[int][]*SecurityPredicate{}
	for rows.Next() {
		var policyID int
		p := &SecurityPredicate{}
		var op sql.NullString
		if err := rows.Scan(&policyID, &p.PredicateType, &p.PredicateDefinition,
			&p.TargetSchema, &p.TargetTable, &op); err != nil {
			return nil, err
		}
		p.Operation = op.String
		byPolicy[policyID] = append(byPolicy[policyID], p)
	}
	return byPolicy, rows.Err()
}

// Enable enables the security policy.
func (p *SecurityPolicy) Enable(ctx context.Context) error {
	_, err := p.db.exec(ctx,
		fmt.Sprintf("ALTER SECURITY POLICY %s WITH (STATE = ON)", qualifiedName(p.Schema, p.Name)))
	if err != nil {
		return fmt.Errorf("gosmo: enable security policy %q: %w", p.Name, err)
	}
	setIfApplied(ctx, &p.IsEnabled, true)
	return nil
}

// Disable disables the security policy.
func (p *SecurityPolicy) Disable(ctx context.Context) error {
	_, err := p.db.exec(ctx,
		fmt.Sprintf("ALTER SECURITY POLICY %s WITH (STATE = OFF)", qualifiedName(p.Schema, p.Name)))
	if err != nil {
		return fmt.Errorf("gosmo: disable security policy %q: %w", p.Name, err)
	}
	setIfApplied(ctx, &p.IsEnabled, false)
	return nil
}

// Drop drops the security policy. A policy that isn't there is the server's
// error, not a silent success — see the note on Table.Drop.
func (p *SecurityPolicy) Drop(ctx context.Context) error {
	_, err := p.db.exec(ctx,
		fmt.Sprintf("DROP SECURITY POLICY %s", qualifiedName(p.Schema, p.Name)))
	if err != nil {
		return fmt.Errorf("gosmo: drop security policy %q: %w", p.Name, err)
	}
	return nil
}
