package gosmo

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"

	"github.com/radix29/gosmo/tsql"
)

// ============================================================
// Scripter — extended properties
// ============================================================

// scriptedExtProp is one extended property a script restates: the level it
// sits at, its name, and its sql_variant value as text with the base type
// that text converts back to.
type scriptedExtProp struct {
	level ExtendedPropertyLevel
	name  string
	// baseType, precision, scale and maxLength are the value's
	// SQL_VARIANT_PROPERTY; maxLength is in bytes.
	baseType                    string
	precision, scale, maxLength int
	// value is the value as nvarchar, in a style that converts back without
	// loss (extPropValueText); NULL for a property stored with no value.
	value sql.NullString
}

// extPropValueText is the value column of objectExtendedProperties: the
// sql_variant converted to nvarchar in the style that converts back without
// loss. The default conversion loses digits for float, real and money (two
// decimals), turns binary into its bytes read as text, and renders every
// type with a time of day in style 0 — "Oct 13 2026 12:34PM", seconds gone —
// even datetime2, datetimeoffset and time, whose own default is ISO: from a
// sql_variant it is not (verified on 17, 2026-10-13). Those three go out in
// style 121 and datetime in 126, both of which read back the same under any
// DATEFORMAT.
const extPropValueText = `CASE CONVERT(sysname, SQL_VARIANT_PROPERTY(ep.value, 'BaseType'))
           WHEN 'binary'        THEN CONVERT(nvarchar(max), CONVERT(varbinary(8000), ep.value), 1)
           WHEN 'varbinary'     THEN CONVERT(nvarchar(max), CONVERT(varbinary(8000), ep.value), 1)
           WHEN 'float'         THEN CONVERT(nvarchar(max), CONVERT(float, ep.value), 3)
           WHEN 'real'          THEN CONVERT(nvarchar(max), CONVERT(real, ep.value), 3)
           WHEN 'money'         THEN CONVERT(nvarchar(max), CONVERT(money, ep.value), 2)
           WHEN 'smallmoney'    THEN CONVERT(nvarchar(max), CONVERT(money, ep.value), 2)
           WHEN 'datetime'      THEN CONVERT(nvarchar(max), CONVERT(datetime, ep.value), 126)
           WHEN 'smalldatetime' THEN CONVERT(nvarchar(max), CONVERT(datetime, ep.value), 126)
           WHEN 'datetime2'      THEN CONVERT(nvarchar(max), CONVERT(datetime2(7), ep.value), 121)
           WHEN 'datetimeoffset' THEN CONVERT(nvarchar(max), CONVERT(datetimeoffset(7), ep.value), 121)
           WHEN 'time'           THEN CONVERT(nvarchar(max), CONVERT(time(7), ep.value), 121)
           ELSE CONVERT(nvarchar(max), ep.value)
       END`

// objectExtendedProperties reads, in one query, the extended properties a
// script of object objectID restates: the object's own, its columns',
// parameters' and indexes', and — when constraints is set, for a table — its
// constraints'. A trigger's own property sits under its parent, at TRIGGER
// level, and a trigger has none of the others.
//
// The levels come back fully named, so the caller renders them as read. An
// object of a kind with no level-1 type here reads as no properties.
func (d *Database) objectExtendedProperties(ctx context.Context, objectID int, constraints bool) ([]scriptedExtProp, error) {
	q := `
SELECT me.sch, me.l1type, me.l1name, ISNULL(p.l2type, ''), ISNULL(p.l2name, ''), ep.name,
       ISNULL(CONVERT(sysname, SQL_VARIANT_PROPERTY(ep.value, 'BaseType')), ''),
       ISNULL(CONVERT(int, SQL_VARIANT_PROPERTY(ep.value, 'Precision')), 0),
       ISNULL(CONVERT(int, SQL_VARIANT_PROPERTY(ep.value, 'Scale')), 0),
       ISNULL(CONVERT(int, SQL_VARIANT_PROPERTY(ep.value, 'MaxLength')), 0),
       ` + extPropValueText + `
FROM   sys.extended_properties ep
CROSS  JOIN (
       SELECT SCHEMA_NAME(o.schema_id),
              CASE ISNULL(par.type, o.type)
                  WHEN 'U'  THEN N'TABLE'     WHEN 'ET' THEN N'TABLE'
                  WHEN 'V'  THEN N'VIEW'
                  WHEN 'P'  THEN N'PROCEDURE' WHEN 'PC' THEN N'PROCEDURE' WHEN 'X' THEN N'PROCEDURE'
                  WHEN 'FN' THEN N'FUNCTION'  WHEN 'IF' THEN N'FUNCTION'  WHEN 'TF' THEN N'FUNCTION'
                  WHEN 'FS' THEN N'FUNCTION'  WHEN 'FT' THEN N'FUNCTION'
              END,
              ISNULL(par.name, o.name)
       FROM   sys.objects o
       LEFT   JOIN sys.objects par ON par.object_id = NULLIF(o.parent_object_id, 0)
       WHERE  o.object_id = @p1) AS me (sch, l1type, l1name)
CROSS  APPLY (
       SELECT 0, 0, CASE WHEN o.parent_object_id <> 0 THEN N'TRIGGER' END,
              CASE WHEN o.parent_object_id <> 0 THEN o.name END
       FROM   sys.objects o
       WHERE  ep.class = 1 AND ep.major_id = @p1 AND ep.minor_id = 0 AND o.object_id = @p1
       UNION ALL
       SELECT 1, c.column_id, N'COLUMN', c.name
       FROM   sys.columns c
       WHERE  ep.class = 1 AND ep.major_id = @p1 AND c.object_id = @p1 AND c.column_id = ep.minor_id
       UNION ALL
       SELECT 2, pr.parameter_id, N'PARAMETER', pr.name
       FROM   sys.parameters pr
       WHERE  ep.class = 2 AND ep.major_id = @p1 AND pr.object_id = @p1 AND pr.parameter_id = ep.minor_id
       UNION ALL
       SELECT 3, i.index_id, N'INDEX', i.name
       FROM   sys.indexes i
       WHERE  ep.class = 7 AND ep.major_id = @p1 AND i.object_id = @p1 AND i.index_id = ep.minor_id
         AND  i.is_hypothetical = 0
       UNION ALL
       SELECT 4, k.object_id, N'CONSTRAINT', k.name
       FROM   sys.objects k
       WHERE  @p2 = 1 AND ep.class = 1 AND ep.minor_id = 0 AND k.object_id = ep.major_id
         AND  k.parent_object_id = @p1 AND k.type IN ('PK', 'UQ', 'F', 'C', 'D', 'EC')
       ) AS p (kind, ord, l2type, l2name)
WHERE  ep.class IN (1, 2, 7) AND me.l1type IS NOT NULL
  AND  (ep.major_id = @p1 OR ep.major_id IN (SELECT object_id FROM sys.objects WHERE parent_object_id = @p1))
ORDER  BY p.kind, p.ord, ep.name`
	rows, err := d.query(ctx, q, objectID, constraints)
	return scanRows(rows, err, "extended properties to script", func(scan func(...any) error) (scriptedExtProp, error) {
		var p scriptedExtProp
		p.level.Level0Type = "SCHEMA"
		err := scan(&p.level.Level0Name, &p.level.Level1Type, &p.level.Level1Name,
			&p.level.Level2Type, &p.level.Level2Name, &p.name,
			&p.baseType, &p.precision, &p.scale, &p.maxLength, &p.value)
		return p, err
	})
}

// extendedPropertiesAt reads the extended properties on one entry of
// sys.extended_properties's (class, major_id, minor_id) — a securable that
// is not a schema-scoped object or one of its children, which
// objectExtendedProperties covers — at level, which the caller names: the
// catalog keys the property by id, and the statement that restates it by
// name. A database's own (class 0) take the zero level.
func (d *Database) extendedPropertiesAt(ctx context.Context, class, majorID, minorID int, level ExtendedPropertyLevel) ([]scriptedExtProp, error) {
	q := `
SELECT ep.name,
       ISNULL(CONVERT(sysname, SQL_VARIANT_PROPERTY(ep.value, 'BaseType')), ''),
       ISNULL(CONVERT(int, SQL_VARIANT_PROPERTY(ep.value, 'Precision')), 0),
       ISNULL(CONVERT(int, SQL_VARIANT_PROPERTY(ep.value, 'Scale')), 0),
       ISNULL(CONVERT(int, SQL_VARIANT_PROPERTY(ep.value, 'MaxLength')), 0),
       ` + extPropValueText + `
FROM   sys.extended_properties ep
WHERE  ep.class = @p1 AND ep.major_id = @p2 AND ep.minor_id = @p3
ORDER  BY ep.name`
	rows, err := d.query(ctx, q, class, majorID, minorID)
	return scanRows(rows, err, "extended properties to script", func(scan func(...any) error) (scriptedExtProp, error) {
		p := scriptedExtProp{level: level}
		err := scan(&p.name, &p.baseType, &p.precision, &p.scale, &p.maxLength, &p.value)
		return p, err
	})
}

// typeColumnExtendedProperties reads the properties on a table type's
// columns (class 8, keyed by the type's user_type_id and the column's id in
// the type's internal table), each at SCHEMA/TYPE/COLUMN level, in column
// order.
func (d *Database) typeColumnExtendedProperties(ctx context.Context, t *UserDefinedTableType) ([]scriptedExtProp, error) {
	q := `
SELECT c.name, ep.name,
       ISNULL(CONVERT(sysname, SQL_VARIANT_PROPERTY(ep.value, 'BaseType')), ''),
       ISNULL(CONVERT(int, SQL_VARIANT_PROPERTY(ep.value, 'Precision')), 0),
       ISNULL(CONVERT(int, SQL_VARIANT_PROPERTY(ep.value, 'Scale')), 0),
       ISNULL(CONVERT(int, SQL_VARIANT_PROPERTY(ep.value, 'MaxLength')), 0),
       ` + extPropValueText + `
FROM   sys.extended_properties ep
JOIN   sys.columns c ON c.object_id = @p2 AND c.column_id = ep.minor_id
WHERE  ep.class = 8 AND ep.major_id = @p1
ORDER  BY c.column_id, ep.name`
	rows, err := d.query(ctx, q, t.UserTypeID, t.TypeTableObjectID)
	return scanRows(rows, err, "extended properties to script", func(scan func(...any) error) (scriptedExtProp, error) {
		p := scriptedExtProp{level: ExtendedPropertyLevel{Level0Type: "SCHEMA", Level0Name: t.Schema,
			Level1Type: "TYPE", Level1Name: t.Name, Level2Type: "COLUMN"}}
		err := scan(&p.level.Level2Name, &p.name, &p.baseType, &p.precision, &p.scale, &p.maxLength, &p.value)
		return p, err
	})
}

// wantsExtendedProperties reports whether a script restates extended
// properties: when the options ask for them and the script creates anything
// — a DROP alone leaves them out.
func (sc *Scripter) wantsExtendedProperties() bool {
	return sc.opts.ExtendedProperties && sc.opts.verb() != ScriptDrop
}

// scriptExtendedProperties appends the extended-property statements for
// objectID to a script (see wantsExtendedProperties). skip drops properties
// at levels the script does not create (nil keeps every one).
func (sc *Scripter) scriptExtendedProperties(ctx context.Context, objectID int, constraints bool,
	skip func(scriptedExtProp) bool) (string, error) {
	if !sc.wantsExtendedProperties() || objectID == 0 {
		return "", nil
	}
	props, err := sc.db.objectExtendedProperties(ctx, objectID, constraints)
	if err != nil {
		return "", err
	}
	return sc.renderExtendedProperties(props, skip), nil
}

// scriptExtendedPropertiesAt is scriptExtendedProperties for one
// extendedPropertiesAt entry.
func (sc *Scripter) scriptExtendedPropertiesAt(ctx context.Context, class, majorID, minorID int, level ExtendedPropertyLevel) (string, error) {
	if !sc.wantsExtendedProperties() {
		return "", nil
	}
	props, err := sc.db.extendedPropertiesAt(ctx, class, majorID, minorID, level)
	if err != nil {
		return "", err
	}
	return sc.renderExtendedProperties(props, nil), nil
}

// withExtendedPropertiesAt appends to script, the script of one securable,
// the properties scriptExtendedPropertiesAt reads for it; what names the
// securable in an error ("sequence [dbo].[s]").
func (sc *Scripter) withExtendedPropertiesAt(ctx context.Context, script, what string, class, majorID, minorID int,
	level ExtendedPropertyLevel) (string, error) {
	props, err := sc.scriptExtendedPropertiesAt(ctx, class, majorID, minorID, level)
	if err != nil {
		return "", fmt.Errorf("gosmo: script %s: %w", what, err)
	}
	return script + props, nil
}

// withTableChildExtendedProperties appends to script, the script of one
// index or constraint on table, that child's own properties: those
// objectExtendedProperties reads for the table at one of levels (INDEX,
// CONSTRAINT) under name. A primary key or unique constraint can carry them
// at both — on its index and on the constraint.
func (sc *Scripter) withTableChildExtendedProperties(ctx context.Context, script string, t *Table, name string,
	levels ...string) (string, error) {
	props, err := sc.scriptExtendedProperties(ctx, t.ObjectID, true, func(p scriptedExtProp) bool {
		return p.level.Level2Name != name || !slices.Contains(levels, p.level.Level2Type)
	})
	if err != nil {
		return "", fmt.Errorf("gosmo: script %s on %s: %w", quoteIdent(name), t.FullName(), err)
	}
	return script + props, nil
}

// renderExtendedProperties renders props but those skip drops.
//
// Under ScriptAlter every statement is guarded whatever IncludeIfNotExists
// says: ALTER keeps some of an object's properties and drops others with what
// it recreates (a view's indexes), and the guard restores exactly the missing
// ones.
func (sc *Scripter) renderExtendedProperties(props []scriptedExtProp, skip func(scriptedExtProp) bool) string {
	guard := sc.opts.IncludeIfNotExists || sc.opts.verb() == ScriptAlter
	var sb strings.Builder
	for _, p := range props {
		if skip == nil || !skip(p) {
			sb.WriteString(extendedPropertyStatement(p, guard))
		}
	}
	return sb.String()
}

// extendedPropertyStatement renders one EXEC sys.sp_addextendedproperty, in
// its own batch, as SSMS writes it: level arguments the property does not
// use are left out, and with guard it is skipped when the property already
// exists.
//
// The value keeps its base type. An nvarchar — nearly every property, and
// every MS_Description SSMS writes — is passed as an N'…' literal; any other
// type is converted back from its text into a variable first, since an EXEC
// argument cannot be an expression, and N'5' would make an int property an
// nvarchar one. A base type with no conversion here is a comment rather than
// a statement with the wrong type.
//
// The levels are written down to the first empty type: a database's own
// property has none, a schema's or a user's only level 0.
func extendedPropertyStatement(p scriptedExtProp, guard bool) string {
	l := p.level
	levels := []string{l.Level0Type, l.Level0Name, l.Level1Type, l.Level1Name, l.Level2Type, l.Level2Name}
	var args strings.Builder
	for i := 0; i < len(levels) && levels[i] != ""; i += 2 {
		fmt.Fprintf(&args, ", @level%[1]dtype = %[2]s, @level%[1]dname = %[3]s", i/2, QuoteLiteral(levels[i]), QuoteLiteral(levels[i+1]))
	}

	var sb strings.Builder
	value := "NULL"
	switch {
	case !p.value.Valid:
	case p.baseType == "nvarchar":
		value = QuoteLiteral(p.value.String)
	default:
		typ, style := extPropType(p)
		if typ == "" {
			where := make([]string, 0, 3)
			for i := 1; i < len(levels); i += 2 {
				if levels[i] != "" {
					where = append(where, quoteIdent(levels[i]))
				}
			}
			on := "the database"
			if len(where) > 0 {
				on = strings.Join(where, ".")
			}
			return fmt.Sprintf("-- Extended property %s on %s holds a %s value with no literal form: not scripted.\n",
				tsql.CommentSafe(QuoteLiteral(p.name)), tsql.CommentSafe(on), tsql.CommentSafe(p.baseType))
		}
		fmt.Fprintf(&sb, "DECLARE @value sql_variant = CONVERT(%s, %s%s);\n", typ, QuoteLiteral(p.value.String), style)
		value = "@value"
	}
	if guard {
		lits := make([]string, len(levels))
		for i, s := range levels {
			lits[i] = "NULL"
			if s != "" {
				lits[i] = QuoteLiteral(s)
			}
		}
		fmt.Fprintf(&sb, "IF NOT EXISTS (SELECT 1 FROM sys.fn_listextendedproperty(%s, %s))\n    ",
			QuoteLiteral(p.name), strings.Join(lits, ", "))
	}
	fmt.Fprintf(&sb, "EXEC sys.sp_addextendedproperty @name = %s, @value = %s%s;\nGO\n", QuoteLiteral(p.name), value, args.String())
	return sb.String()
}

// extPropType is the type a non-nvarchar value converts back to, with its
// length, precision or scale, and the CONVERT style that reads
// extPropValueText's text back ("" when the default does). "" for a base
// type sql_variant can hold that has no case here.
func extPropType(p scriptedExtProp) (typ, style string) {
	n := max(p.maxLength, 1)
	switch p.baseType {
	case "binary", "varbinary":
		return fmt.Sprintf("%s(%d)", p.baseType, n), ", 1"
	case "char", "varchar":
		return fmt.Sprintf("%s(%d)", p.baseType, n), ""
	case "nchar":
		return fmt.Sprintf("nchar(%d)", max(p.maxLength/2, 1)), ""
	case "decimal", "numeric":
		return fmt.Sprintf("%s(%d,%d)", p.baseType, p.precision, p.scale), ""
	case "datetime2", "time", "datetimeoffset":
		return fmt.Sprintf("%s(%d)", p.baseType, p.scale), ""
	case "bit", "tinyint", "smallint", "int", "bigint", "real", "float", "money", "smallmoney",
		"date", "datetime", "smalldatetime", "uniqueidentifier":
		return p.baseType, ""
	}
	return "", ""
}
