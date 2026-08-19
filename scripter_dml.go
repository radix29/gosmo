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
func (sc *Scripter) ScriptSelect(schema, name string) (string, error) {
	return sc.ScriptSelectContext(context.Background(), schema, name)
}

// ScriptSelectContext is the context-aware variant of ScriptSelect.
func (sc *Scripter) ScriptSelectContext(ctx context.Context, schema, name string) (string, error) {
	cols, err := sc.db.ObjectColumnsContext(ctx, schema, name)
	if err != nil {
		return "", err
	}
	return buildSelectScript(schema, name, cols), nil
}

// ScriptInsert generates an INSERT template for a table or view.
func (sc *Scripter) ScriptInsert(schema, name string) (string, error) {
	return sc.ScriptInsertContext(context.Background(), schema, name)
}

// ScriptInsertContext is the context-aware variant of ScriptInsert.
func (sc *Scripter) ScriptInsertContext(ctx context.Context, schema, name string) (string, error) {
	cols, err := sc.db.ObjectColumnsContext(ctx, schema, name)
	if err != nil {
		return "", err
	}
	return buildInsertScript(schema, name, cols), nil
}

// ScriptUpdate generates an UPDATE template for a table or view.
func (sc *Scripter) ScriptUpdate(schema, name string) (string, error) {
	return sc.ScriptUpdateContext(context.Background(), schema, name)
}

// ScriptUpdateContext is the context-aware variant of ScriptUpdate.
func (sc *Scripter) ScriptUpdateContext(ctx context.Context, schema, name string) (string, error) {
	cols, err := sc.db.ObjectColumnsContext(ctx, schema, name)
	if err != nil {
		return "", err
	}
	return buildUpdateScript(schema, name, cols), nil
}

// ScriptDelete generates a DELETE template for a table or view.
func (sc *Scripter) ScriptDelete(schema, name string) (string, error) {
	return sc.ScriptDeleteContext(context.Background(), schema, name)
}

// ScriptDeleteContext is the context-aware variant of ScriptDelete.
func (sc *Scripter) ScriptDeleteContext(ctx context.Context, schema, name string) (string, error) {
	return fmt.Sprintf("DELETE FROM %s\nWHERE  <Search Conditions,,>;\nGO\n", qualifiedName(schema, name)), nil
}

// ScriptExecute generates an EXECUTE template for a stored procedure.
func (sc *Scripter) ScriptExecute(schema, name string) (string, error) {
	return sc.ScriptExecuteContext(context.Background(), schema, name)
}

// ScriptExecuteContext is the context-aware variant of ScriptExecute.
func (sc *Scripter) ScriptExecuteContext(ctx context.Context, schema, name string) (string, error) {
	params, err := sc.db.ParametersContext(ctx, schema, name)
	if err != nil {
		return "", err
	}
	return buildExecuteScript(schema, name, params), nil
}

// ScriptFunctionCall generates a call template for a function: a SELECT of a
// scalar function's result, or a SELECT from a table-valued one. funcType is
// the UserDefinedFunction.FuncType — "FN", "IF" or "TF".
func (sc *Scripter) ScriptFunctionCall(schema, name, funcType string) (string, error) {
	return sc.ScriptFunctionCallContext(context.Background(), schema, name, funcType)
}

// ScriptFunctionCallContext is the context-aware variant of
// ScriptFunctionCall.
func (sc *Scripter) ScriptFunctionCallContext(ctx context.Context, schema, name, funcType string) (string, error) {
	params, err := sc.db.ParametersContext(ctx, schema, name)
	if err != nil {
		return "", err
	}
	return buildFunctionCallScript(schema, name, funcType, params), nil
}

// scriptableColumns drops the columns a caller can't write to: an identity
// column and a computed one both reject an explicit value, so leaving them in
// an INSERT or UPDATE template produces a statement that always fails.
func scriptableColumns(cols []*Column) []*Column {
	out := make([]*Column, 0, len(cols))
	for _, c := range cols {
		if c.IsIdentity || c.IsComputed {
			continue
		}
		out = append(out, c)
	}
	return out
}

// columnPlaceholder renders the <name, type, value> token SSMS's templates
// use for a value the operator has to supply.
func columnPlaceholder(c *Column) string {
	return fmt.Sprintf("<%s, %s,>", c.Name, ColumnTypeString(c))
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
	writable := scriptableColumns(cols)
	if len(writable) == 0 {
		return fmt.Sprintf("INSERT INTO %s\nDEFAULT VALUES;\nGO\n", full)
	}
	names := make([]string, len(writable))
	values := make([]string, len(writable))
	for i, c := range writable {
		names[i] = quoteIdent(c.Name)
		values[i] = columnPlaceholder(c)
	}
	return fmt.Sprintf("INSERT INTO %s\n           (%s)\nVALUES     (%s);\nGO\n",
		full, strings.Join(names, "\n          , "), strings.Join(values, "\n          , "))
}

func buildUpdateScript(schema, name string, cols []*Column) string {
	full := qualifiedName(schema, name)
	writable := scriptableColumns(cols)
	if len(writable) == 0 {
		return fmt.Sprintf("-- %s has no updatable columns.\n", full)
	}
	sets := make([]string, len(writable))
	for i, c := range writable {
		sets[i] = fmt.Sprintf("%s = %s", quoteIdent(c.Name), columnPlaceholder(c))
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
