package gosmo

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

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

// scriptExecProc renders an ExecProc call as the T-SQL EXEC statement that
// would run it, for capture under WithScript — input values as literals,
// output and in/out parameters as a declared variable followed by OUTPUT,
// since a script run by hand has nowhere else to put the returned value.
//
// The variable names are the parameter names, which SQL Server guarantees are
// unique within one call, so a DECLARE per output parameter can't collide
// within the statement. Two scripted calls to the same procedure in one batch
// would, which is why the DECLARE rides along with the statement it belongs
// to rather than being hoisted.
func scriptExecProc(proc string, params []ProcParam) (string, error) {
	var decls, args []string
	for _, p := range params {
		v := "@" + p.name
		switch {
		case p.dest == nil:
			lit, err := scriptLiteral(p.value)
			if err != nil {
				return "", fmt.Errorf("gosmo: script: parameter %q: %w", p.name, err)
			}
			args = append(args, v+" = "+lit)
		case p.inOut:
			lit, err := scriptLiteral(dereference(p.dest))
			if err != nil {
				return "", fmt.Errorf("gosmo: script: parameter %q: %w", p.name, err)
			}
			decls = append(decls, "DECLARE "+v+" "+scriptDeclType(p.dest)+" = "+lit+";")
			args = append(args, v+" = "+v+" OUTPUT")
		default:
			decls = append(decls, "DECLARE "+v+" "+scriptDeclType(p.dest)+";")
			args = append(args, v+" = "+v+" OUTPUT")
		}
	}
	stmt := "EXEC " + proc
	if len(args) > 0 {
		stmt += " " + strings.Join(args, ", ")
	}
	if len(decls) > 0 {
		stmt = strings.Join(decls, "\n") + "\n" + stmt
	}
	return stmt, nil
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

	// Scripting can't go through d.exec: the statement handed to the driver
	// is the bare procedure name, with everything else carried as RPC
	// arguments, so capturing that text alone yields a "statement" that is
	// just an object name — no EXEC, no parameters. Build the T-SQL form
	// here instead. The status is left at its zero value, as it is for every
	// other write under WithScript, since nothing ran.
	if Scripting(ctx) {
		stmt, err := scriptExecProc(proc, params)
		if err != nil {
			return ProcResult{}, fmt.Errorf("gosmo: exec proc %s: %w", proc, err)
		}
		if _, err := d.exec(ctx, stmt); err != nil {
			return ProcResult{}, fmt.Errorf("gosmo: exec proc %s: %w", proc, err)
		}
		return ProcResult{}, nil
	}

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

// -- Stored procedures ---------------------------------------------------------

// StoredProcedure represents a stored procedure.
type StoredProcedure struct {
	ObjectID   int
	Schema     string
	Name       string
	Definition string
	CreateDate time.Time
	ModifyDate time.Time
}

// StoredProcedures returns all stored procedures in the database.
func (d *Database) StoredProcedures() ([]*StoredProcedure, error) {
	return d.StoredProceduresContext(context.Background())
}

// StoredProceduresContext is the context-aware variant of StoredProcedures.
func (d *Database) StoredProceduresContext(ctx context.Context) ([]*StoredProcedure, error) {
	const q = `
SELECT p.object_id, SCHEMA_NAME(p.schema_id), p.name,
       ISNULL(m.definition,''), p.create_date, p.modify_date
FROM   sys.procedures p
JOIN   sys.sql_modules m ON m.object_id = p.object_id
WHERE  p.is_ms_shipped = 0
ORDER  BY SCHEMA_NAME(p.schema_id), p.name`

	rows, err := d.query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("gosmo: list stored procs in %q: %w", d.name, err)
	}
	defer rows.Close()

	var procs []*StoredProcedure
	for rows.Next() {
		p := &StoredProcedure{}
		if err := rows.Scan(&p.ObjectID, &p.Schema, &p.Name,
			&p.Definition, &p.CreateDate, &p.ModifyDate); err != nil {
			return nil, err
		}
		procs = append(procs, p)
	}
	return procs, rows.Err()
}

// CreateStoredProcedure creates (or replaces) a stored procedure.
// schema may be empty (defaults to dbo). body is the raw T-SQL after AS.
func (d *Database) CreateStoredProcedure(schema, name, body string) error {
	return d.CreateStoredProcedureContext(context.Background(), schema, name, body)
}

// CreateStoredProcedureContext is the context-aware variant.
func (d *Database) CreateStoredProcedureContext(ctx context.Context, schema, name, body string) error {
	if name == "" {
		return fmt.Errorf("gosmo: create stored procedure: name is required")
	}
	if schema == "" {
		schema = "dbo"
	}
	q := fmt.Sprintf("CREATE OR ALTER PROCEDURE %s\nAS\n%s", qualifiedName(schema, name), body)
	if _, err := d.exec(ctx, q); err != nil {
		return fmt.Errorf("gosmo: create stored procedure [%s].[%s]: %w", schema, name, err)
	}
	return nil
}

// DropStoredProcedure drops a stored procedure.
func (d *Database) DropStoredProcedure(schema, name string) error {
	return d.DropStoredProcedureContext(context.Background(), schema, name)
}

// DropStoredProcedureContext is the context-aware variant.
func (d *Database) DropStoredProcedureContext(ctx context.Context, schema, name string) error {
	if schema == "" {
		schema = "dbo"
	}
	if _, err := d.exec(ctx, "DROP PROCEDURE IF EXISTS "+qualifiedName(schema, name)); err != nil {
		return fmt.Errorf("gosmo: drop stored procedure [%s].[%s]: %w", schema, name, err)
	}
	return nil
}

// SystemStoredProcedures returns every system stored procedure SQL Server
// ships in the "sys" schema (sp_help, sp_who, ...) — see
// SystemStoredProceduresContext.
func (d *Database) SystemStoredProcedures() ([]*StoredProcedure, error) {
	return d.SystemStoredProceduresContext(context.Background())
}

// SystemStoredProceduresContext is the context-aware variant of
// SystemStoredProcedures. Reads sys.all_objects rather than sys.procedures
// for the same reason SystemViewsContext reads sys.all_objects instead of
// sys.views: shipped objects are invisible through the non-"all_" catalog
// views. Restricted to types 'P'/'PC' (SQL/CLR stored procedure), matching
// what sys.procedures itself documents — extended stored procedures ('X',
// e.g. xp_cmdshell) are a distinct object kind and excluded. The "sys"
// schema is identical in every database on a server, so this only needs
// loading once per connection.
func (d *Database) SystemStoredProceduresContext(ctx context.Context) ([]*StoredProcedure, error) {
	const q = `
SELECT o.object_id, SCHEMA_NAME(o.schema_id), o.name,
       ISNULL(m.definition,''), o.create_date, o.modify_date
FROM   sys.all_objects o
LEFT JOIN sys.all_sql_modules m ON m.object_id = o.object_id
WHERE  o.type IN ('P','PC') AND o.is_ms_shipped = 1 AND SCHEMA_NAME(o.schema_id) = 'sys'
ORDER  BY o.name`

	rows, err := d.query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("gosmo: list system stored procs in %q: %w", d.name, err)
	}
	defer rows.Close()

	var procs []*StoredProcedure
	for rows.Next() {
		p := &StoredProcedure{}
		if err := rows.Scan(&p.ObjectID, &p.Schema, &p.Name,
			&p.Definition, &p.CreateDate, &p.ModifyDate); err != nil {
			return nil, err
		}
		procs = append(procs, p)
	}
	return procs, rows.Err()
}
