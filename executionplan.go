package gosmo

import (
	"context"
	"database/sql"
	"fmt"
)

// ============================================================
// Execution plans
// ============================================================

// ExecutionPlan holds one captured execution plan.
type ExecutionPlan struct {
	// XML is the plan in SQL Server's "Showplan XML" format — the same
	// document SSMS parses to draw its graphical plan.
	XML string
}

// showplanColumn is the fixed column name SQL Server has used for showplan
// output since SQL Server 2005; it doesn't change with the server version.
const showplanColumn = "Microsoft SQL Server 2005 XML Showplan"

// EstimatedPlan captures sql's estimated execution plan without running it
// (SET SHOWPLAN_XML ON) — SSMS's "Display Estimated Execution Plan".
func (d *Database) EstimatedPlan(sql string) (*ExecutionPlan, error) {
	return d.EstimatedPlanContext(context.Background(), sql)
}

// EstimatedPlanContext is the context-aware variant of EstimatedPlan.
func (d *Database) EstimatedPlanContext(ctx context.Context, sqlText string) (*ExecutionPlan, error) {
	return d.capturePlan(ctx, "SHOWPLAN_XML", sqlText)
}

// ActualPlan executes sql and captures its actual execution plan
// (SET STATISTICS XML ON) — SSMS's "Include Actual Execution Plan". Unlike
// EstimatedPlan, this runs the statement.
func (d *Database) ActualPlan(sql string) (*ExecutionPlan, error) {
	return d.ActualPlanContext(context.Background(), sql)
}

// ActualPlanContext is the context-aware variant of ActualPlan.
func (d *Database) ActualPlanContext(ctx context.Context, sqlText string) (*ExecutionPlan, error) {
	return d.capturePlan(ctx, "STATISTICS XML", sqlText)
}

// capturePlan runs sqlText with the given SET option on, then scans every
// result set for the one holding the plan: both SHOWPLAN_XML (the only
// result set, since the statement never runs) and STATISTICS XML (an extra
// result set appended after the statement's own) name it showplanColumn.
func (d *Database) capturePlan(ctx context.Context, setOpt, sqlText string) (*ExecutionPlan, error) {
	var plan string
	err := d.withConn(ctx, func(conn *sql.Conn) error {
		if _, err := conn.ExecContext(ctx, "SET "+setOpt+" ON"); err != nil {
			return fmt.Errorf("gosmo: enable %s: %w", setOpt, err)
		}
		defer conn.ExecContext(context.Background(), "SET "+setOpt+" OFF")

		rows, err := conn.QueryContext(ctx, sqlText)
		if err != nil {
			return err
		}
		defer rows.Close()

		for {
			cols, err := rows.Columns()
			if err != nil {
				return err
			}
			isPlan := len(cols) == 1 && cols[0] == showplanColumn
			for rows.Next() {
				if isPlan {
					if err := rows.Scan(&plan); err != nil {
						return err
					}
				}
			}
			if err := rows.Err(); err != nil {
				return err
			}
			if !rows.NextResultSet() {
				break
			}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("gosmo: capture execution plan: %w", err)
	}
	if plan == "" {
		return nil, fmt.Errorf("gosmo: capture execution plan: no plan was returned")
	}
	return &ExecutionPlan{XML: plan}, nil
}
