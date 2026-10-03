package gosmo

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// ============================================================
// CLR modules (procedure, function, trigger … AS EXTERNAL NAME)
// ============================================================

// A CLR procedure, function or trigger has no stored definition: no
// sys.sql_modules row, so there is no text for scriptModule to replay. Its
// CREATE is rebuilt from the catalog instead — sys.assembly_modules names
// the method, sys.parameters the signature (including defaults, which the
// catalog keeps for CLR modules only), sys.columns a table function's result
// and sys.trigger_events a trigger's events — the way SSMS scripts one.
//
// The rebuilt CREATE starts with the keyword, so alterModuleDefinition turns
// it into the ALTER as it does a stored definition. ALTER of a CLR module is
// legal only while its signature is unchanged, and scripting the same object
// guarantees that.
//
// A table function's ORDER hint is reproduced (sys.function_order_columns).
// A COLLATE on a string column of its RETURNS TABLE is not: the column takes
// the database default on replay.

// clrModule is everything one CLR module's CREATE says, read from the
// catalog. renderCLRModule turns it into the statement.
type clrModule struct {
	keyword string // PROCEDURE, FUNCTION or TRIGGER
	name    string // as it follows CREATE <keyword>, already quoted

	assembly, class, method string

	// executeAs is the EXECUTE AS clause's argument: "" for CALLER, the
	// default, which is not written; OWNER; or a quoted principal name.
	// SELF is cataloged as the creating principal's id, so it comes back as
	// that principal's name — the same identity.
	executeAs  string
	nullOnNull bool // RETURNS NULL ON NULL INPUT (functions)

	params  []clrParam
	returns string   // a scalar function's return type
	table   []string // a table function's columns, "[name] type"
	order   []string // its ORDER hint, "[name] ASC|DESC"

	// on is a trigger's target: a quoted table or view, DATABASE, or
	// ALL SERVER.
	on                string
	insteadOf         bool
	notForReplication bool
	events            []string
}

// clrParam is one parameter of a CLR procedure or function.
type clrParam struct {
	name, typ string
	// def is the default, rendered as a literal ("NULL" included); "" for a
	// parameter that has none.
	def    string
	output bool
}

// renderCLRModule writes the CREATE for one CLR module, without a trailing
// batch separator.
func renderCLRModule(m clrModule) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "CREATE %s %s", m.keyword, m.name)
	var with []string
	if m.nullOnNull {
		with = append(with, "RETURNS NULL ON NULL INPUT")
	}
	if m.executeAs != "" {
		with = append(with, "EXECUTE AS "+m.executeAs)
	}
	withClause := ""
	if len(with) > 0 {
		withClause = "\nWITH " + strings.Join(with, ", ")
	}
	params := make([]string, len(m.params))
	for i, p := range m.params {
		s := p.name + " " + p.typ
		if p.def != "" {
			s += " = " + p.def
		}
		if p.output {
			s += " OUTPUT"
		}
		params[i] = s
	}
	switch m.keyword {
	case "PROCEDURE":
		if len(params) > 0 {
			sb.WriteString("\n    " + strings.Join(params, ",\n    "))
		}
		sb.WriteString(withClause)
	case "FUNCTION":
		fmt.Fprintf(&sb, "(%s)\nRETURNS ", strings.Join(params, ", "))
		if m.table != nil {
			fmt.Fprintf(&sb, "TABLE (\n    %s\n)", strings.Join(m.table, ",\n    "))
		} else {
			sb.WriteString(m.returns)
		}
		sb.WriteString(withClause)
		if len(m.order) > 0 {
			fmt.Fprintf(&sb, "\nORDER (%s)", strings.Join(m.order, ", "))
		}
	case "TRIGGER":
		sb.WriteString(" ON " + m.on)
		sb.WriteString(withClause)
		timing := "AFTER"
		if m.insteadOf {
			timing = "INSTEAD OF"
		}
		fmt.Fprintf(&sb, "\n%s %s", timing, strings.Join(m.events, ", "))
		if m.notForReplication {
			sb.WriteString("\nNOT FOR REPLICATION")
		}
	}
	externalName := quoteIdent(m.assembly)
	if m.class != "" {
		externalName += "." + quoteIdent(m.class)
	}
	externalName += "." + quoteIdent(m.method)
	fmt.Fprintf(&sb, "\nAS EXTERNAL NAME %s;", externalName)
	return sb.String()
}

// catalogQuerier is the read side Database and Server share: a CLR module's
// catalog is read through the database for a schema-scoped module or a
// database trigger, through the server for a server trigger.
type catalogQuerier interface {
	rowQuerier
	query(ctx context.Context, q string, args ...any) (*dbRows, error)
}

// clrMethod reads the assembly method and EXECUTE AS principal of the CLR
// module objectID into m. modules and assemblies are the scope's catalog
// views; principal is the function that names an EXECUTE AS principal id.
// sys.server_assembly_modules has no null_on_null_input — the option is a
// function's, and a server-scope module is only ever a trigger.
func clrMethod(ctx context.Context, q catalogQuerier, m *clrModule, objectID int, modules, assemblies, principal string) error {
	nullOnNull := "ISNULL(am.null_on_null_input, 0)"
	if modules == "sys.server_assembly_modules" {
		nullOnNull = "CAST(0 AS bit)"
	}
	var executeAs sql.NullString
	var owner bool
	err := q.queryRow(ctx, func(row *sql.Row) error {
		return row.Scan(&m.assembly, &m.class, &m.method, &m.nullOnNull, &owner, &executeAs)
	}, `
SELECT a.name, ISNULL(am.assembly_class, N''), am.assembly_method,
       `+nullOnNull+`,
       CAST(CASE WHEN am.execute_as_principal_id = -2 THEN 1 ELSE 0 END AS bit),
       `+principal+`(NULLIF(am.execute_as_principal_id, -2))
FROM   `+modules+` am
JOIN   `+assemblies+` a ON a.assembly_id = am.assembly_id
WHERE  am.object_id = @p1`, objectID)
	if err != nil {
		return err
	}
	switch {
	case owner:
		m.executeAs = "OWNER"
	case executeAs.Valid:
		m.executeAs = QuoteLiteral(executeAs.String)
	}
	return nil
}

// clrEvents reads a trigger's events, in the catalog's event-type order.
func clrEvents(ctx context.Context, q catalogQuerier, objectID int, events string) ([]string, error) {
	rows, err := q.query(ctx, `
SELECT te.type_desc
FROM   `+events+` te
WHERE  te.object_id = @p1
ORDER  BY te.type`, objectID)
	return scanRows(rows, err, "", func(scan func(...any) error) (string, error) {
		var e string
		err := scan(&e)
		return e, err
	})
}

// clrParameterSelect reads a CLR module's parameters, parameter_id 0 — a
// scalar function's return type — included. The default is converted to text
// by its own base type, in a style that converts back to the same value:
// binary as 0x…, float and real with every digit, money with four decimals,
// date and time types in ISO 8601.
const clrParameterSelect = `
SELECT p.parameter_id, p.name, tp.name, p.max_length, p.precision, p.scale,
       SCHEMA_NAME(tp.schema_id), tp.is_user_defined, p.is_output, p.has_default_value,
       ISNULL(CAST(SQL_VARIANT_PROPERTY(p.default_value, 'BaseType') AS sysname), N''),
       CASE CAST(SQL_VARIANT_PROPERTY(p.default_value, 'BaseType') AS sysname)
         WHEN N'binary'         THEN CONVERT(nvarchar(max), CAST(p.default_value AS varbinary(max)), 1)
         WHEN N'varbinary'      THEN CONVERT(nvarchar(max), CAST(p.default_value AS varbinary(max)), 1)
         WHEN N'float'          THEN CONVERT(nvarchar(max), CAST(p.default_value AS float), 3)
         WHEN N'real'           THEN CONVERT(nvarchar(max), CAST(p.default_value AS real), 3)
         WHEN N'money'          THEN CONVERT(nvarchar(max), CAST(p.default_value AS money), 2)
         WHEN N'smallmoney'     THEN CONVERT(nvarchar(max), CAST(p.default_value AS money), 2)
         WHEN N'date'           THEN CONVERT(nvarchar(max), CAST(p.default_value AS date), 126)
         WHEN N'time'           THEN CONVERT(nvarchar(max), CAST(p.default_value AS time(7)), 126)
         WHEN N'datetime'       THEN CONVERT(nvarchar(max), CAST(p.default_value AS datetime), 126)
         WHEN N'smalldatetime'  THEN CONVERT(nvarchar(max), CAST(p.default_value AS datetime), 126)
         WHEN N'datetime2'      THEN CONVERT(nvarchar(max), CAST(p.default_value AS datetime2(7)), 126)
         WHEN N'datetimeoffset' THEN CONVERT(nvarchar(max), CAST(p.default_value AS datetimeoffset(7)), 127)
         ELSE CONVERT(nvarchar(max), p.default_value)
       END
FROM   sys.parameters p
JOIN   sys.types tp ON tp.user_type_id = p.user_type_id
WHERE  p.object_id = @p1
ORDER  BY p.parameter_id`

// clrDefaultLiteral renders a parameter default from its base type and text:
// quoted for a string, date, time or uniqueidentifier, bare for a number,
// bit or 0x binary, NULL for a default of NULL.
func clrDefaultLiteral(baseType string, text sql.NullString) string {
	if !text.Valid {
		return "NULL"
	}
	switch strings.ToLower(baseType) {
	case "char", "varchar", "nchar", "nvarchar", "text", "ntext",
		"date", "time", "datetime", "smalldatetime", "datetime2", "datetimeoffset",
		"uniqueidentifier":
		return QuoteLiteral(text.String)
	}
	return text.String
}

// clrSchemaModule reads the CLR procedure, function or DML trigger objectID,
// named [schema].[name], into the clrModule its CREATE is rendered from.
func (d *Database) clrSchemaModule(ctx context.Context, keyword string, objectID int, schema, name string) (clrModule, error) {
	m := clrModule{keyword: keyword, name: qualifiedName(schema, name)}
	if err := clrMethod(ctx, d, &m, objectID, "sys.assembly_modules", "sys.assemblies", "USER_NAME"); err != nil {
		return m, err
	}
	switch keyword {
	case "TRIGGER":
		var parentSchema, parent string
		err := d.queryRow(ctx, func(row *sql.Row) error {
			return row.Scan(&parentSchema, &parent, &m.insteadOf, &m.notForReplication)
		}, `
SELECT OBJECT_SCHEMA_NAME(tr.parent_id), OBJECT_NAME(tr.parent_id),
       tr.is_instead_of_trigger, tr.is_not_for_replication
FROM   sys.triggers tr
WHERE  tr.object_id = @p1`, objectID)
		if err != nil {
			return m, err
		}
		m.on = qualifiedName(parentSchema, parent)
		m.events, err = clrEvents(ctx, d, objectID, "sys.trigger_events")
		return m, err
	}

	rows, err := d.query(ctx, clrParameterSelect, objectID)
	type param struct {
		clrParam
		id int
	}
	params, err := scanRows(rows, err, "", func(scan func(...any) error) (param, error) {
		var p param
		var typeName, typeSchema, baseType string
		var maxLength, precision, scale int
		var userDefined, hasDefault bool
		var def sql.NullString
		err := scan(&p.id, &p.name, &typeName, &maxLength, &precision, &scale,
			&typeSchema, &userDefined, &p.output, &hasDefault, &baseType, &def)
		p.typ = catalogType{dt: DataType(typeName), typeSchema: typeSchema, userDefined: userDefined,
			maxLength: maxLength, precision: precision, scale: scale}.String()
		if hasDefault {
			p.def = clrDefaultLiteral(baseType, def)
		}
		return p, err
	})
	if err != nil {
		return m, err
	}
	for _, p := range params {
		if p.id == 0 {
			m.returns = p.typ
			continue
		}
		m.params = append(m.params, p.clrParam)
	}
	if keyword != "FUNCTION" || m.returns != "" {
		return m, nil
	}

	// A table function: no parameter 0, a result set in sys.columns.
	rows, err = d.query(ctx, `
SELECT c.name, tp.name, c.max_length, c.precision, c.scale,
       SCHEMA_NAME(tp.schema_id), tp.is_user_defined
FROM   sys.columns c
JOIN   sys.types tp ON tp.user_type_id = c.user_type_id
WHERE  c.object_id = @p1
ORDER  BY c.column_id`, objectID)
	m.table, err = scanRows(rows, err, "", func(scan func(...any) error) (string, error) {
		var colName, typeName, typeSchema string
		var maxLength, precision, scale int
		var userDefined bool
		err := scan(&colName, &typeName, &maxLength, &precision, &scale, &typeSchema, &userDefined)
		return quoteIdent(colName) + " " + catalogType{dt: DataType(typeName), typeSchema: typeSchema, userDefined: userDefined,
			maxLength: maxLength, precision: precision, scale: scale}.String(), err
	})
	if err != nil {
		return m, err
	}
	rows, err = d.query(ctx, `
SELECT c.name, oc.is_descending
FROM   sys.function_order_columns oc
JOIN   sys.columns c ON c.object_id = oc.object_id AND c.column_id = oc.column_id
WHERE  oc.object_id = @p1
ORDER  BY oc.order_column_id`, objectID)
	m.order, err = scanRows(rows, err, "", func(scan func(...any) error) (string, error) {
		var colName string
		var desc bool
		err := scan(&colName, &desc)
		dir := " ASC"
		if desc {
			dir = " DESC"
		}
		return quoteIdent(colName) + dir, err
	})
	return m, err
}

// clrDatabaseTrigger reads a CLR database-scope DDL trigger's CREATE, or ""
// when the trigger is not CLR (an encrypted one, whose definition is NULL).
func (d *Database) clrDatabaseTrigger(ctx context.Context, name string) (string, error) {
	var objectID int
	err := d.queryRow(ctx, func(row *sql.Row) error { return row.Scan(&objectID) }, `
SELECT tr.object_id
FROM   sys.triggers tr
JOIN   sys.assembly_modules am ON am.object_id = tr.object_id
WHERE  tr.parent_class = 0 AND tr.name = @p1`, name)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	m := clrModule{keyword: "TRIGGER", name: quoteIdent(name), on: "DATABASE"}
	if err := clrMethod(ctx, d, &m, objectID, "sys.assembly_modules", "sys.assemblies", "USER_NAME"); err != nil {
		return "", err
	}
	if m.events, err = clrEvents(ctx, d, objectID, "sys.trigger_events"); err != nil {
		return "", err
	}
	return renderCLRModule(m), nil
}

// clrServerTrigger reads a CLR server trigger's CREATE, or "" when the
// trigger is not CLR. Its assembly is master's — the only database a
// server-scope CLR trigger can bind to.
func (s *Server) clrServerTrigger(ctx context.Context, name string) (string, error) {
	var objectID int
	err := s.queryRow(ctx, func(row *sql.Row) error { return row.Scan(&objectID) }, `
SELECT tr.object_id
FROM   sys.server_triggers tr
JOIN   sys.server_assembly_modules am ON am.object_id = tr.object_id
WHERE  tr.name = @p1`, name)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	m := clrModule{keyword: "TRIGGER", name: quoteIdent(name), on: "ALL SERVER"}
	if err := clrMethod(ctx, s, &m, objectID, "sys.server_assembly_modules", "master.sys.assemblies", "SUSER_NAME"); err != nil {
		return "", err
	}
	if m.events, err = clrEvents(ctx, s, objectID, "sys.server_trigger_events"); err != nil {
		return "", err
	}
	return renderCLRModule(m), nil
}
