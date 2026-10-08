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

// ShowplanColumn is the fixed column name SQL Server has used for showplan
// output since SQL Server 2005; it doesn't change with the server version. A
// result set of exactly one column with this name is a plan document, not
// data — capturePlan uses it to tell the two apart, and it is exported so a
// caller running its own SET SHOWPLAN_XML / SET STATISTICS XML batch can make
// the same distinction.
const ShowplanColumn = "Microsoft SQL Server 2005 XML Showplan"

// PlanMode selects which execution plan a capture asks the server for.
type PlanMode int

const (
	// PlanEstimated is SET SHOWPLAN_XML ON: the server compiles each batch
	// and returns its plan instead of running it.
	PlanEstimated PlanMode = iota + 1

	// PlanActual is SET STATISTICS XML ON: each statement really runs, and
	// its plan follows its own results as one more result set.
	PlanActual
)

// String is the mode's name in an error message: "estimated" or "actual".
func (m PlanMode) String() string {
	switch m {
	case PlanEstimated:
		return "estimated"
	case PlanActual:
		return "actual"
	}
	return fmt.Sprintf("PlanMode(%d)", int(m))
}

func (m PlanMode) setOption() string {
	if m == PlanEstimated {
		return "SHOWPLAN_XML"
	}
	return "STATISTICS XML"
}

// planStopTimeout bounds the SET ... OFF that ends a plan capture.
const planStopTimeout = 5 * time.Second

// StartPlanCapture switches mode's SET option on for the session behind conn
// — a *sql.Conn or *sql.Tx, since the option is per session and a pooled
// *sql.DB would hand the next statement another one — and returns stop, which
// switches it off again. A caller defers stop on every exit: a session left
// under SHOWPLAN_XML compiles everything it is sent afterwards and runs
// nothing.
//
// stop runs detached from ctx's cancellation, keeping its values: a cancelled
// run is the likeliest one to need it. planStopTimeout bounds how long an
// unresponsive connection can hold it, which context.Background() would not.
//
// Both errors read "gosmo: enable|disable <mode> execution plan capture: ...".
// An error enabling the capture means nothing was switched on, and stop is
// nil.
func StartPlanCapture(ctx context.Context, conn interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}, mode PlanMode) (stop func() error, err error) {
	if mode != PlanEstimated && mode != PlanActual {
		return nil, invalidf("gosmo: start plan capture: unknown mode %v", mode)
	}
	set := "SET " + mode.setOption()
	if _, err := conn.ExecContext(ctx, set+" ON"); err != nil {
		return nil, fmt.Errorf("gosmo: enable %s execution plan capture: %w", mode, err)
	}
	return func() error {
		sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), planStopTimeout)
		defer cancel()
		if _, err := conn.ExecContext(sctx, set+" OFF"); err != nil {
			return fmt.Errorf("gosmo: disable %s execution plan capture: %w", mode, err)
		}
		return nil
	}, nil
}

// EstimatedPlan captures sql's estimated execution plan without running it
// (SET SHOWPLAN_XML ON) — SSMS's "Display Estimated Execution Plan".
func (d *Database) EstimatedPlan(ctx context.Context, sqlText string) (*ExecutionPlan, error) {
	return d.capturePlan(ctx, PlanEstimated, sqlText)
}

// ActualPlan executes sql and captures its actual execution plan
// (SET STATISTICS XML ON) — SSMS's "Include Actual Execution Plan". Unlike
// EstimatedPlan, this runs the statement.
func (d *Database) ActualPlan(ctx context.Context, sqlText string) (*ExecutionPlan, error) {
	return d.capturePlan(ctx, PlanActual, sqlText)
}

// capturePlan runs sqlText with mode's capture on, then collects every
// plan document it finds: both SHOWPLAN_XML (whose result sets are the only
// ones, since no statement runs) and STATISTICS XML (an extra result set
// appended after each statement's own) name the plan column ShowplanColumn.
//
// Every row of every such set is kept, not just the last. That is a
// tolerance, not a shape any server has been seen to produce: probed against
// SQL Server 17 over multi-statement batches, EXEC of a (nested) procedure,
// control flow, cursors and dynamic SQL, every showplan set held exactly one
// row — SHOWPLAN_XML one combined document per batch, STATISTICS XML one
// document per executed statement in a set of its own. A server that ever
// split a set across rows would lose all but one plan to an overwriting
// scan, so the loop stays.
func (d *Database) capturePlan(ctx context.Context, mode PlanMode, sqlText string) (*ExecutionPlan, error) {
	var plans []string
	err := d.withConn(ctx, func(ctx context.Context, conn sqlConn) error {
		stop, err := StartPlanCapture(ctx, conn, mode)
		if err != nil {
			return err
		}
		// conn goes back to the pool in a known state even when ctx is
		// already cancelled; a failed stop has nothing left to report to.
		defer stop()

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
			isPlan := len(cols) == 1 && cols[0] == ShowplanColumn
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
