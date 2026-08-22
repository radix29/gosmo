package gosmo

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// ============================================================
// Execution plans
// ============================================================

// ExecutionPlan holds the execution plans captured for one batch.
//
// A batch of several statements yields several plan documents, not one: under
// SET SHOWPLAN_XML the server returns one row per statement, and under SET
// STATISTICS XML one extra result set per statement. All holds every one of
// them, in the order the server produced them; XML is the *last*, which is
// what this type returned when it held a single string and is kept for
// callers written against that. A caller that means "the plan for the batch"
// wants All.
type ExecutionPlan struct {
	// XML is the last plan in All, in SQL Server's "Showplan XML" format —
	// the same document SSMS parses to draw its graphical plan.
	XML string

	// All holds every captured plan document, one per statement, in the
	// order the server returned them. Never empty on a successful capture.
	All []string
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

// capturePlan runs sqlText with the given SET option on, then collects every
// plan document it finds: both SHOWPLAN_XML (whose result sets are the only
// ones, since no statement runs) and STATISTICS XML (an extra result set
// appended after each statement's own) name the plan column showplanColumn.
//
// Every row of every such set is kept, not just the last: SHOWPLAN_XML
// returns one row per statement in a single result set, so a multi-statement
// batch loses all but one plan if the scan overwrites.
func (d *Database) capturePlan(ctx context.Context, setOpt, sqlText string) (*ExecutionPlan, error) {
	var plans []string
	err := d.withConn(ctx, func(conn *sql.Conn) error {
		if _, err := conn.ExecContext(ctx, "SET "+setOpt+" ON"); err != nil {
			return fmt.Errorf("gosmo: enable %s: %w", setOpt, err)
		}
		// Cleanup must still run (and return conn to the pool in a known
		// state) even if ctx is already canceled by the time capturePlan
		// returns — context.WithoutCancel keeps ctx's values without its
		// cancellation, and the timeout bounds how long a genuinely
		// unresponsive connection can block it, unlike context.Background()
		// which never times out.
		defer func() {
			cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			conn.ExecContext(cctx, "SET "+setOpt+" OFF")
		}()

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
					var plan string
					if err := rows.Scan(&plan); err != nil {
						return err
					}
					if plan != "" {
						plans = append(plans, plan)
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
	if len(plans) == 0 {
		return nil, fmt.Errorf("gosmo: capture execution plan: no plan was returned")
	}
	return &ExecutionPlan{XML: plans[len(plans)-1], All: plans}, nil
}
