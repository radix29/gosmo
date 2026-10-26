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
				return "", fmt.Errorf("script: parameter %q: %w", p.name, err)
			}
			args = append(args, v+" = "+lit)
		case p.inOut:
			lit, err := scriptLiteral(dereference(p.dest))
			if err != nil {
				return "", fmt.Errorf("script: parameter %q: %w", p.name, err)
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
func (d *Database) ExecProc(ctx context.Context, schema, name string, params ...ProcParam) (ProcResult, error) {
	if err := requireSchema("exec proc", schema, name); err != nil {
		return ProcResult{}, err
	}
	if name == "" {
		return ProcResult{}, invalidf("gosmo: exec proc: no procedure name")
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
	// is what makes OUTPUT parameters and the return status available. The
	// name carries d's, so the procedure runs in d and the session stays in
	// its login database (see withConn) — a procedure runs in the database
	// it belongs to, sys ones in the database named, so a session switched to
	// d calls the same one.
	args := make([]any, 0, len(params)+1)
	for _, p := range params {
		args = append(args, p.arg())
	}
	var status mssql.ReturnStatus
	args = append(args, &status)

	err := d.withConn(ctx, func(ctx context.Context, c sqlConn) error {
		_, err := c.ExecContext(ctx, quoteIdent(d.Name)+"."+proc, args...)
		if err != nil && txFrom(ctx, d.server) == nil {
			if uerr := d.recheckEntry(ctx, c, err); uerr != nil {
				return uerr
			}
		}
		return err
	})
	if err != nil {
		return ProcResult{}, fmt.Errorf("gosmo: exec proc %s: %w", proc, err)
	}
	observe(ctx, d.server, ScriptEntry{Server: scriptServerName(ctx, d.server), Database: d.Name, SQL: proc})
	return ProcResult{ReturnStatus: int32(status)}, nil
}

// -- Stored procedures ---------------------------------------------------------

// StoredProcedure represents a stored procedure.
type StoredProcedure struct {
	db *Database

	ObjectID   int
	Schema     string
	Name       string
	CreateDate time.Time
	ModifyDate time.Time
}

// StoredProcedures returns the stored procedures an ObjectFilter matches,
// narrowed by the server. The zero ObjectFilter returns every one.
func (d *Database) StoredProcedures(ctx context.Context, filter ObjectFilter) ([]*StoredProcedure, error) {
	where, args := filter.clause(moduleFilterColumns)
	return moduleListWhere(ctx, d, "stored procs", userProcedures, "", where, args, scanStoredProcedure)
}

var (
	userProcedures   = moduleSource{from: "sys.procedures o", kind: "o.is_ms_shipped = 0"}
	systemProcedures = moduleSource{from: "sys.all_objects o", kind: "o.type IN ('P','PC') AND o.is_ms_shipped = 1 AND o.schema_id = 4", system: true}
)

func scanStoredProcedure(d *Database, scan func(...any) error) (*StoredProcedure, error) {
	p := &StoredProcedure{db: d}
	if err := scan(&p.ObjectID, &p.Schema, &p.Name,
		&p.CreateDate, &p.ModifyDate); err != nil {
		return nil, err
	}
	return p, nil
}

// StoredProcedureByName returns one user stored procedure by schema and
// name.
//
// It returns an error satisfying errors.Is(err, ErrNotFound) when the
// database has no such procedure.
func (d *Database) StoredProcedureByName(ctx context.Context, schema, name string) (*StoredProcedure, error) {
	if err := requireSchema("stored procedure by name", schema, name); err != nil {
		return nil, err
	}
	procs, err := moduleListWhere(ctx, d, "stored procs", userProcedures, "",
		"AND SCHEMA_NAME(o.schema_id) COLLATE DATABASE_DEFAULT = @p1 AND o.name = @p2", []any{schema, name}, scanStoredProcedure)
	if err != nil {
		return nil, fmt.Errorf("gosmo: find stored procedure %s in %q: %w", qualifiedName(schema, name), d.Name, err)
	}
	if len(procs) == 0 {
		return nil, notFoundf("gosmo: stored procedure %s not found in %q", qualifiedName(schema, name), d.Name)
	}
	return procs[0], nil
}

// CreateStoredProcedureRequest describes a stored procedure to create or
// replace.
type CreateStoredProcedureRequest struct {
	Schema string // required; see ErrSchemaRequired
	Name   string
	Body   string // the raw T-SQL after AS
}

// CreateStoredProcedure creates (or replaces) a stored procedure, and returns
// it read back from the catalog — or, under Scripting(ctx), one carrying only
// its schema and name, since nothing ran.
func (d *Database) CreateStoredProcedure(ctx context.Context, req CreateStoredProcedureRequest) (*StoredProcedure, error) {
	if req.Name == "" {
		return nil, invalidf("gosmo: create stored procedure: name is required")
	}
	schema := req.Schema
	if err := requireSchema("create stored procedure", schema, req.Name); err != nil {
		return nil, err
	}
	q := fmt.Sprintf("CREATE OR ALTER PROCEDURE %s\nAS\n%s", qualifiedName(schema, req.Name), req.Body)
	if _, err := d.exec(ctx, q); err != nil {
		return nil, fmt.Errorf("gosmo: create stored procedure %s: %w", qualifiedName(schema, req.Name), err)
	}
	return createdObject(ctx, d.StoredProcedureRef(schema, req.Name), func() (*StoredProcedure, error) {
		return d.StoredProcedureByName(ctx, schema, req.Name)
	})
}

// SystemStoredProcedures returns every system stored procedure SQL Server
// ships in the "sys" schema (sp_help, sp_who, ...) — see
// SystemStoredProcedures.
//
// Reads sys.all_objects rather than sys.procedures for the same reason
// SystemViews reads sys.all_objects instead of sys.views: shipped
// objects are invisible through the non-"all_" catalog views. Restricted to
// types 'P'/'PC' (SQL/CLR stored procedure), matching what sys.procedures
// itself documents — extended stored procedures ('X', e.g. xp_cmdshell) are
// a distinct object kind and excluded. The "sys" schema is identical in every
// database on a server, so this only needs loading once per connection. The
// zero ObjectFilter returns every one.
func (d *Database) SystemStoredProcedures(ctx context.Context, filter ObjectFilter) ([]*StoredProcedure, error) {
	where, args := filter.clause(moduleFilterColumns)
	return moduleListWhere(ctx, d, "system stored procs", systemProcedures, "", where, args, scanStoredProcedure)
}

// -- Parameters ----------------------------------------------------------------

// Parameter mirrors one row of sys.parameters: a parameter of a stored
// procedure or a function. The return value of a scalar function, which the
// catalog also stores there as parameter_id 0, is not a parameter and is not
// returned.
type Parameter struct {
	Name       string // including the leading @
	Ordinal    int
	DataType   DataType
	MaxLength  int // -1 = MAX
	Precision  int
	Scale      int
	IsOutput   bool
	HasDefault bool

	// TypeSchema is the schema DataType belongs to — "sys" for a built-in
	// type — and IsUserDefinedType is sys.types.is_user_defined, as on
	// Column: an alias or table type's unqualified name resolves against the
	// executing user's default schema, so TypeString qualifies it.
	TypeSchema        string
	IsUserDefinedType bool

	// XMLSchemaCollectionSchema, XMLSchemaCollection, IsXMLDocument,
	// VectorDimensions and VectorBaseType are the typed-xml and vector
	// facets, as on Column.
	XMLSchemaCollectionSchema string
	XMLSchemaCollection       string
	IsXMLDocument             bool
	VectorDimensions          int
	VectorBaseType            string
}

// TypeString returns the T-SQL data-type fragment for the parameter, in the
// same form Column.TypeString gives a column: a user-defined type is
// schema-qualified and carries no length.
func (p *Parameter) TypeString() string {
	return catalogType{
		dt: p.DataType, typeSchema: p.TypeSchema, userDefined: p.IsUserDefinedType,
		maxLength: p.MaxLength, precision: p.Precision, scale: p.Scale,
		xmlSchema: p.XMLSchemaCollectionSchema, xmlCollection: p.XMLSchemaCollection, xmlDocument: p.IsXMLDocument,
		vectorDimensions: p.VectorDimensions, vectorBaseType: p.VectorBaseType,
	}.String()
}

// Parameters returns the parameters of one stored procedure or function, in
// declaration order.
func (d *Database) Parameters(ctx context.Context, schema, name string) ([]*Parameter, error) {
	if err := requireSchema("parameters", schema, name); err != nil {
		return nil, err
	}
	rows, err := d.query(ctx, d.parameterSelect(), schema, name)
	return scanRows(rows, err, fmt.Sprintf("list parameters of %s", qualifiedName(schema, name)), func(scan func(...any) error) (*Parameter, error) {
		p := &Parameter{}
		var typeName string
		if err := scan(&p.Name, &p.Ordinal, &typeName,
			&p.MaxLength, &p.Precision, &p.Scale, &p.IsOutput, &p.HasDefault,
			&p.TypeSchema, &p.IsUserDefinedType,
			&p.XMLSchemaCollectionSchema, &p.XMLSchemaCollection, &p.IsXMLDocument,
			&p.VectorDimensions, &p.VectorBaseType); err != nil {
			return nil, err
		}
		p.DataType = DataType(typeName)
		return p, nil
	})
}

// parameterSelect is Parameters' query. The vector_* columns are SQL Server
// 2025's, with the vector type; the typed-xml ones predate gosmo's floor.
// https://learn.microsoft.com/sql/relational-databases/system-catalog-views/sys-parameters-transact-sql
func (d *Database) parameterSelect() string {
	major := d.serverMajorVersion()
	return `
SELECT p.name, p.parameter_id, tp.name,
       p.max_length, p.precision, p.scale,
       p.is_output, p.has_default_value,
       SCHEMA_NAME(tp.schema_id), tp.is_user_defined,
       ISNULL(SCHEMA_NAME(xsc.schema_id), ''), ISNULL(xsc.name, ''), p.is_xml_document,
       ` + colSince(major, SQLServer2025, "ISNULL(p.vector_dimensions, 0)", "CAST(0 AS int)") + `,
       ` + colSince(major, SQLServer2025, "ISNULL(p.vector_base_type_desc, '')", "CAST('' AS nvarchar(60))") + `
FROM   sys.parameters p
JOIN   sys.types tp ON tp.user_type_id = p.user_type_id
LEFT   JOIN sys.xml_schema_collections xsc
       ON  xsc.xml_collection_id = NULLIF(p.xml_collection_id, 0)
WHERE  p.object_id = OBJECT_ID(QUOTENAME(@p1) + N'.' + QUOTENAME(@p2))
  AND  p.parameter_id > 0
ORDER  BY p.parameter_id`
}

// StoredProcedureRef returns a lightweight handle for the stored procedure [schema].[name] — no
// query; every field but Schema and Name is zero. See Server.DatabaseRef for
// when a handle is the right form.
func (d *Database) StoredProcedureRef(schema, name string) *StoredProcedure {
	return &StoredProcedure{db: d, Schema: schema, Name: name}
}

// Database returns the database the stored procedure belongs to.
func (p *StoredProcedure) Database() *Database { return p.db }

// Definition returns the procedure's T-SQL text: "" for an encrypted or CLR
// procedure, ErrNotFound when there is no such procedure. Listings do not
// carry it; this reads it by name, so a StoredProcedureRef is enough.
func (p *StoredProcedure) Definition(ctx context.Context) (string, error) {
	return p.db.moduleDefinition(ctx, "stored procedure", "o.type IN ('P','PC')", p.Schema, p.Name)
}

// Drop drops the stored procedure. A stored procedure that isn't there is the server's
// error, not a silent success — see the note on Table.Drop.
func (p *StoredProcedure) Drop(ctx context.Context) error {
	return p.db.dropSchemaObject(ctx, "stored procedure", "PROCEDURE", p.Schema, p.Name)
}

// Rename renames the stored procedure (sp_rename's 'OBJECT' class). newName is a bare
// name; a rename never moves the stored procedure between schemas — see Transfer.
func (p *StoredProcedure) Rename(ctx context.Context, newName string) error {
	if err := p.db.renameSchemaObject(ctx, "stored procedure", renameObjectClass, p.Schema, p.Name, newName); err != nil {
		return err
	}
	setIfApplied(ctx, &p.Name, newName)
	return nil
}

// Transfer moves the stored procedure into another schema (ALTER SCHEMA ... TRANSFER).
// It keeps its name and object_id; permissions granted on it directly are
// dropped by the server.
func (p *StoredProcedure) Transfer(ctx context.Context, targetSchema string) error {
	if err := p.db.transferSchemaObject(ctx, "stored procedure", transferObjectClass, targetSchema, p.Schema, p.Name); err != nil {
		return err
	}
	setIfApplied(ctx, &p.Schema, targetSchema)
	return nil
}
