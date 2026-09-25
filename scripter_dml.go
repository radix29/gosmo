package gosmo

import (
	"context"
	"fmt"
	"strings"
)

// ============================================================
// Scripter — DML templates (SELECT / INSERT / UPDATE / DELETE / EXECUTE)
// ============================================================

// These mirror SSMS's "SELECT To", "INSERT To" and friends: a statement
// skeleton for the object, with a <name, type, value> placeholder wherever
// the operator has to supply something. The placeholders are deliberately
// not valid T-SQL — a template that ran as-is would silently write whatever
// default was guessed for it.

// ScriptSelect generates a SELECT of every column of a table or view.
func (sc *Scripter) ScriptSelect(ctx context.Context, schema, name string) (string, error) {
	if err := requireSchema("script select", schema, name); err != nil {
		return "", err
	}
	cols, err := sc.db.ObjectColumns(ctx, schema, name)
	if err != nil {
		return "", err
	}
	return buildSelectScript(schema, name, cols), nil
}

// ScriptInsert generates an INSERT template for a table or view.
func (sc *Scripter) ScriptInsert(ctx context.Context, schema, name string) (string, error) {
	if err := requireSchema("script insert", schema, name); err != nil {
		return "", err
	}
	cols, err := sc.db.ObjectColumns(ctx, schema, name)
	if err != nil {
		return "", err
	}
	return buildInsertScript(schema, name, cols), nil
}

// ScriptUpdate generates an UPDATE template for a table or view.
func (sc *Scripter) ScriptUpdate(ctx context.Context, schema, name string) (string, error) {
	if err := requireSchema("script update", schema, name); err != nil {
		return "", err
	}
	cols, err := sc.db.ObjectColumns(ctx, schema, name)
	if err != nil {
		return "", err
	}
	return buildUpdateScript(schema, name, cols), nil
}

// ScriptDelete generates a DELETE template for a table or view.
func (sc *Scripter) ScriptDelete(ctx context.Context, schema, name string) (string, error) {
	if err := requireSchema("script delete", schema, name); err != nil {
		return "", err
	}
	return fmt.Sprintf("DELETE FROM %s\nWHERE  <Search Conditions,,>;\nGO\n", qualifiedName(schema, name)), nil
}

// ScriptExecute generates an EXECUTE template for a stored procedure.
func (sc *Scripter) ScriptExecute(ctx context.Context, schema, name string) (string, error) {
	if err := requireSchema("script execute", schema, name); err != nil {
		return "", err
	}
	params, err := sc.db.Parameters(ctx, schema, name)
	if err != nil {
		return "", err
	}
	return buildExecuteScript(schema, name, params), nil
}

// ScriptFunctionCall generates a call template for a function: a SELECT of a
// scalar function's result, or a SELECT from a table-valued one. funcType is
// the UserDefinedFunction.FuncType — "FN", "IF" or "TF".
func (sc *Scripter) ScriptFunctionCall(ctx context.Context, schema, name, funcType string) (string, error) {
	if err := requireSchema("script function call", schema, name); err != nil {
		return "", err
	}
	params, err := sc.db.Parameters(ctx, schema, name)
	if err != nil {
		return "", err
	}
	return buildFunctionCallScript(schema, name, funcType, params), nil
}

// scriptableColumns drops the columns a caller can't write to, since leaving
// one in an INSERT or UPDATE template produces a statement that always
// fails: an identity or computed column, a rowversion, a system-versioned
// table's GENERATED ALWAYS period column, and a graph table's internal
// columns.
//
// An edge table is the exception for INSERT: its row has to name the two
// nodes it joins, through the $from_id and $to_id pseudo-columns, and an
// INSERT without them fails on the NOT NULL internal columns behind them. The
// pseudo-column is written bare, because bracketed as [$from_id] it is an
// ordinary name that resolves to nothing (Msg 207). An edge's endpoints
// cannot be updated, so UPDATE leaves them out with the rest.
func scriptableColumns(cols []*Column, forInsert bool) []dmlColumn {
	out := make([]dmlColumn, 0, len(cols))
	for _, c := range cols {
		switch {
		case forInsert && c.GraphType == GraphColumnFromIDComputed:
			out = append(out, graphEndpoint("$from_id"))
		case forInsert && c.GraphType == GraphColumnToIDComputed:
			out = append(out, graphEndpoint("$to_id"))
		case c.IsIdentity, c.IsComputed, isRowVersion(c),
			c.GeneratedAlwaysType != 0, c.GraphType != 0:
			continue
		default:
			out = append(out, dmlColumn{name: quoteIdent(c.Name), placeholder: columnPlaceholder(c)})
		}
	}
	return out
}

// dmlColumn is one column of an INSERT or UPDATE template: the name as the
// statement writes it, and the value placeholder that goes with it.
type dmlColumn struct {
	name, placeholder string
}

// graphEndpoint is an edge's $from_id or $to_id. The value is a node's
// $node_id, which is JSON text.
func graphEndpoint(pseudo string) dmlColumn {
	return dmlColumn{name: pseudo, placeholder: fmt.Sprintf("<%s, nvarchar(1000),>", pseudo)}
}

// isRowVersion reports a rowversion column, which the server fills on every
// write and which refuses an explicit value. sys.types still names the type
// by its deprecated synonym, timestamp.
func isRowVersion(c *Column) bool {
	return strings.EqualFold(string(c.DataType), "timestamp") || strings.EqualFold(string(c.DataType), string(DataTypeRowVersion))
}

// columnPlaceholder renders the <name, type, value> token SSMS's templates
// use for a value the operator has to supply.
func columnPlaceholder(c *Column) string {
	return fmt.Sprintf("<%s, %s,>", c.Name, c.TypeString())
}

func buildSelectScript(schema, name string, cols []*Column) string {
	full := qualifiedName(schema, name)
	if len(cols) == 0 {
		return fmt.Sprintf("SELECT *\nFROM   %s;\nGO\n", full)
	}
	names := make([]string, len(cols))
	for i, c := range cols {
		names[i] = quoteIdent(c.Name)
	}
	return fmt.Sprintf("SELECT %s\nFROM   %s;\nGO\n", strings.Join(names, "\n     , "), full)
}

func buildInsertScript(schema, name string, cols []*Column) string {
	full := qualifiedName(schema, name)
	writable := scriptableColumns(cols, true)
	if len(writable) == 0 {
		return fmt.Sprintf("INSERT INTO %s\nDEFAULT VALUES;\nGO\n", full)
	}
	names := make([]string, len(writable))
	values := make([]string, len(writable))
	for i, c := range writable {
		names[i] = c.name
		values[i] = c.placeholder
	}
	return fmt.Sprintf("INSERT INTO %s\n           (%s)\nVALUES     (%s);\nGO\n",
		full, strings.Join(names, "\n          , "), strings.Join(values, "\n          , "))
}

func buildUpdateScript(schema, name string, cols []*Column) string {
	full := qualifiedName(schema, name)
	writable := scriptableColumns(cols, false)
	if len(writable) == 0 {
		return fmt.Sprintf("-- %s has no updatable columns.\n", full)
	}
	sets := make([]string, len(writable))
	for i, c := range writable {
		sets[i] = fmt.Sprintf("%s = %s", c.name, c.placeholder)
	}
	return fmt.Sprintf("UPDATE %s\nSET    %s\nWHERE  <Search Conditions,,>;\nGO\n",
		full, strings.Join(sets, "\n     , "))
}

// buildExecuteScript assembles the EXEC template: every OUTPUT parameter gets
// a variable declared for it and selected back afterwards, since an OUTPUT
// argument has to be a variable — a placeholder there would not parse.
func buildExecuteScript(schema, name string, params []*Parameter) string {
	var sb strings.Builder
	sb.WriteString("DECLARE @return_value int;\n")
	for _, p := range params {
		if p.IsOutput {
			// The variable keeps the parameter's own @name: DECLARE takes an
			// @-prefixed name and nothing else — bracket-quoting it parses as
			// a cursor declaration and fails with "'decimal' is not a
			// recognized CURSOR option".
			fmt.Fprintf(&sb, "DECLARE %s %s;\n", p.Name, p.TypeString())
		}
	}
	fmt.Fprintf(&sb, "\nEXEC @return_value = %s", qualifiedName(schema, name))
	for i, p := range params {
		sep := "\n     "
		if i > 0 {
			sep = ",\n     "
		}
		if p.IsOutput {
			fmt.Fprintf(&sb, "%s%s = %s OUTPUT", sep, p.Name, p.Name)
			continue
		}
		fmt.Fprintf(&sb, "%s%s = <%s, %s,>", sep, p.Name, strings.TrimPrefix(p.Name, "@"), p.TypeString())
	}
	sb.WriteString(";\n\n")
	for _, p := range params {
		if p.IsOutput {
			fmt.Fprintf(&sb, "SELECT %s AS N'%s';\n", p.Name, escapeSingle(p.Name))
		}
	}
	sb.WriteString("SELECT 'Return Value' = @return_value;\nGO\n")
	return sb.String()
}

// buildFunctionCallScript assembles the call template for a function. A
// scalar function is selected as a value; a table-valued one is selected
// from, which is the only form that parses.
func buildFunctionCallScript(schema, name, funcType string, params []*Parameter) string {
	args := make([]string, len(params))
	for i, p := range params {
		args[i] = fmt.Sprintf("<%s, %s,>", strings.TrimPrefix(p.Name, "@"), p.TypeString())
	}
	call := fmt.Sprintf("%s(%s)", qualifiedName(schema, name), strings.Join(args, ", "))
	if strings.EqualFold(funcType, "FN") {
		return fmt.Sprintf("SELECT %s AS N'%s';\nGO\n", call, escapeSingle(name))
	}
	return fmt.Sprintf("SELECT *\nFROM   %s;\nGO\n", call)
}
