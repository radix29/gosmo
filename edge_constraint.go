package gosmo

import (
	"context"
	"database/sql"
	"fmt"
)

// EdgeConstraint is a graph edge table's CONNECTION constraint (SQL Server
// 2019+): the node-table pairs an edge may join, and what deleting a node
// does to the edges that reference it.
type EdgeConstraint struct {
	Name    string
	Clauses []EdgeConnection
	// DeleteAction is NO_ACTION or CASCADE.
	DeleteAction string
	IsDisabled   bool
	// IsNotTrusted is set when the server has not verified the constraint
	// against every existing edge — always for a disabled one, and for one
	// added WITH NOCHECK.
	IsNotTrusted bool
}

// EdgeConnection is one `from TO to` clause of an edge constraint.
type EdgeConnection struct {
	FromSchema, FromTable string
	ToSchema, ToTable     string
}

// edgeConstraintSelect reads edge constraints with their clauses aggregated
// per constraint — one query, not one per constraint — for the tables
// parent selects (a predicate on ec.parent_object_id); extra is appended to
// the SELECT list.
func edgeConstraintSelect(extra, parent string) string {
	return `
SELECT ec.name, ec.delete_referential_action_desc, ec.is_disabled, ec.is_not_trusted,
       (SELECT OBJECT_SCHEMA_NAME(ecc.from_object_id) AS fs, OBJECT_NAME(ecc.from_object_id) AS ft,
               OBJECT_SCHEMA_NAME(ecc.to_object_id) AS ts, OBJECT_NAME(ecc.to_object_id) AS tt
        FROM   sys.edge_constraint_clauses ecc
        WHERE  ecc.object_id = ec.object_id
        ORDER BY ecc.clause_number
        FOR JSON PATH)` + extra + `
FROM   sys.edge_constraints ec
WHERE  ec.parent_object_id ` + parent + `
ORDER  BY ec.name`
}

// scanEdgeConstraint scans one row of edgeConstraintSelect.
func scanEdgeConstraint(scan func(...any) error, extra ...any) (*EdgeConstraint, error) {
	ec := &EdgeConstraint{}
	var clauses sql.NullString
	if err := scan(append([]any{&ec.Name, &ec.DeleteAction, &ec.IsDisabled, &ec.IsNotTrusted, &clauses}, extra...)...); err != nil {
		return nil, err
	}
	cs, err := decodeJSONRows[struct{ FS, FT, TS, TT string }](clauses)
	if err != nil {
		return nil, err
	}
	for _, c := range cs {
		ec.Clauses = append(ec.Clauses, EdgeConnection{c.FS, c.FT, c.TS, c.TT})
	}
	return ec, nil
}

// EdgeConstraints returns the edge constraints on the table — none for a
// table that is not an edge table.
//
// Edge constraints are SQL Server 2019 (15.x); graph tables are 2017. On
// 2017 an edge table can have none, and sys.edge_constraints does not exist
// to ask, so the answer there is an empty list rather than an error.
// https://learn.microsoft.com/sql/relational-databases/tables/graph-edge-constraints
func (t *Table) EdgeConstraints(ctx context.Context) ([]*EdgeConstraint, error) {
	if err := t.requireLoaded("list edge constraints for"); err != nil {
		return nil, err
	}
	if t.db.before(SQLServer2019) {
		return nil, nil
	}
	rows, err := t.db.query(ctx, edgeConstraintSelect("", "= @p1"), t.ObjectID)
	return scanRows(rows, err, fmt.Sprintf("list edge constraints for %s", t.FullName()), func(scan func(...any) error) (*EdgeConstraint, error) {
		return scanEdgeConstraint(scan)
	})
}
