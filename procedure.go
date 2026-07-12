package gosmo

import (
	"context"
	"database/sql"
	"fmt"

	mssql "github.com/microsoft/go-mssqldb"
)

// ============================================================
// Stored-procedure execution (mirrors SSMS "Execute Stored Procedure")
// ============================================================

// ProcParam is one argument to a stored procedure. Build it with In (input),
// Out (output), or InOut (both). Output and in/out parameters carry a pointer
// the returned value is written to, exactly as with database/sql's sql.Out.
type ProcParam struct {
	name  string
	value any // input value, for In
	dest  any // pointer written to, for Out / InOut
	inOut bool
}

// In supplies an input parameter (@name = value).
func In(name string, value any) ProcParam {
	return ProcParam{name: name, value: value}
}

// Out captures an OUTPUT parameter. dest must be a non-nil pointer to a
// settable value (e.g. *int64, *string); it receives the value the procedure
// writes to @name.
func Out(name string, dest any) ProcParam {
	return ProcParam{name: name, dest: dest}
}

// InOut supplies an INPUT parameter that the procedure also writes back.
// dest is both the input (its current pointed-to value is sent) and the
// output (it is overwritten with the returned value).
func InOut(name string, dest any) ProcParam {
	return ProcParam{name: name, dest: dest, inOut: true}
}

// arg converts the parameter to the driver argument ExecContext expects: a
// plain named value for input, or a named sql.Out for output / in-out.
func (p ProcParam) arg() any {
	if p.dest != nil {
		return sql.Named(p.name, sql.Out{Dest: p.dest, In: p.inOut})
	}
	return sql.Named(p.name, p.value)
}

// ProcResult is what ExecProc reports beyond the values written to any output
// parameters' pointers.
type ProcResult struct {
	// ReturnStatus is the procedure's RETURN value (0 unless the procedure
	// returns another code). SQL Server uses it by convention to signal
	// success (0) or an error (non-zero).
	ReturnStatus int32
}

// ExecProc executes a stored procedure by schema and name, binding the given
// parameters and capturing its return status. Output parameter values are
// written to the pointers passed to Out / InOut. Any result sets the
// procedure emits are discarded; use the query methods when you need the rows.
func (d *Database) ExecProc(schema, name string, params ...ProcParam) (ProcResult, error) {
	return d.ExecProcContext(context.Background(), schema, name, params...)
}

// ExecProcContext is the context-aware variant of ExecProc.
func (d *Database) ExecProcContext(ctx context.Context, schema, name string, params ...ProcParam) (ProcResult, error) {
	if name == "" {
		return ProcResult{}, fmt.Errorf("gosmo: exec proc: no procedure name")
	}
	if schema == "" {
		schema = "dbo"
	}
	proc := qualifiedName(schema, name)

	// The driver runs a bare procedure name with named args as an RPC, which
	// is what makes OUTPUT parameters and the return status available.
	args := make([]any, 0, len(params)+1)
	for _, p := range params {
		args = append(args, p.arg())
	}
	var status mssql.ReturnStatus
	args = append(args, &status)

	if _, err := d.exec(ctx, proc, args...); err != nil {
		return ProcResult{}, fmt.Errorf("gosmo: exec proc %s: %w", proc, err)
	}
	return ProcResult{ReturnStatus: int32(status)}, nil
}
