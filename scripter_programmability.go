package gosmo

// Scripter — the Programmability families SSMS groups under Types,
// Assemblies, Rules, Defaults and Plan Guides.
//
// None of these objects has an ALTER form that restates it: CREATE TYPE,
// CREATE RULE and CREATE DEFAULT have no ALTER at all, ALTER ASSEMBLY takes
// a new binary rather than a new definition, and a plan guide is changed by
// dropping and recreating it. So every family here scripts as CREATE, DROP,
// or DROP-and-CREATE only, and ScriptAlter falls back to the CREATE the way
// ScriptOptions.Verb documents.
//
// Two of them carry something a script cannot reproduce. An assembly's
// payload is megabytes of binary that Assembly.FileContent deliberately does
// not fetch with the listing, and a plan guide's CREATE is an
// sp_create_plan_guide call whose arguments are the query text itself. Both
// are scripted with the shape the server would accept and a placeholder or
// the stored text in place of what cannot be read back — never with the
// clause omitted, which would produce a script that silently creates a
// different object (the same reasoning as buildCredentialScript's).

import (
	"context"
	"fmt"
	"strings"
)

// ============================================================
// Alias types  (CREATE TYPE ... FROM <base type>)
// ============================================================

// ScriptUserDefinedDataType generates the CREATE (or DROP) script for one
// alias type.
func (sc *Scripter) ScriptUserDefinedDataType(ctx context.Context, schema, name string) (string, error) {
	if err := requireSchema("script user defined data type", schema, name); err != nil {
		return "", err
	}
	t, err := sc.db.UserDefinedDataTypeByName(ctx, schema, name)
	if err != nil {
		return "", err
	}
	return buildUserDefinedDataTypeScript(t, sc.opts), nil
}

// buildUserDefinedDataTypeScript assembles one alias type's script.
//
// A bound rule or default is scripted as the sp_bindrule / sp_bindefault
// that bound it: the binding is part of what the type is, and a CREATE TYPE
// alone would produce a type that accepts values the original refuses. Both
// procedures are deprecated, which is a property of the object being
// scripted, not a reason to script it incompletely.
func buildUserDefinedDataTypeScript(t *UserDefinedDataType, opts ScriptOptions) string {
	fullName := qualifiedName(t.Schema, t.Name)
	drop := fmt.Sprintf("DROP TYPE IF EXISTS %s;\nGO\n", fullName)
	guard := fmt.Sprintf("IF TYPE_ID(N'%s') IS NULL\n", escapeSingle(fullName))
	return opts.envelope(drop, guard, func(sb *strings.Builder) {
		fmt.Fprintf(sb, "CREATE TYPE %s FROM %s %s;\nGO\n",
			fullName, t.BaseTypeString(), nullClause(t.IsNullable))
		if t.Rule != "" {
			fmt.Fprintf(sb, "\nEXEC sp_bindrule N'%s', N'%s';\nGO\n",
				escapeSingle(quoteIdent(t.Rule)), escapeSingle(fullName))
		}
		if t.Default != "" {
			fmt.Fprintf(sb, "\nEXEC sp_bindefault N'%s', N'%s';\nGO\n",
				escapeSingle(quoteIdent(t.Default)), escapeSingle(fullName))
		}
	})
}

// BaseTypeString renders the alias's base type with the length, precision or
// scale it was declared with — "nvarchar(20)", not the stored 40. BaseType is
// sys.types' own name for the system type, which is what DataType's constants
// are, so the shared renderer that serves Column.TypeString serves this too —
// a second copy would be the place nchar's byte-doubled max_length gets
// forgotten.
func (t *UserDefinedDataType) BaseTypeString() string {
	return TypeString(DataType(strings.ToLower(t.BaseType)), t.MaxLength, t.Precision, t.Scale)
}

// nullClause is the explicit nullability CREATE TYPE takes. Always emitted:
// the default depends on the connection's ANSI_NULL_DFLT_ON setting, so a
// script that leaves it out creates a type whose nullability depends on who
// runs it.
func nullClause(nullable bool) string {
	if nullable {
		return "NULL"
	}
	return "NOT NULL"
}

// ============================================================
// Table types  (CREATE TYPE ... AS TABLE)
// ============================================================

// ScriptUserDefinedTableType generates the CREATE (or DROP) script for one
// table type.
//
// The columns are read only for a verb that emits a CREATE: a DROP names the
// type and nothing else, and a caller scripting a drop should not pay for the
// column read or fail on it.
func (sc *Scripter) ScriptUserDefinedTableType(ctx context.Context, schema, name string) (string, error) {
	if err := requireSchema("script user defined table type", schema, name); err != nil {
		return "", err
	}
	t, err := sc.db.UserDefinedTableTypeByName(ctx, schema, name)
	if err != nil {
		return "", err
	}
	var cols []*Column
	if sc.opts.verb() != ScriptDrop {
		if cols, err = t.Columns(ctx); err != nil {
			return "", err
		}
	}
	return buildUserDefinedTableTypeScript(t, cols, sc.opts), nil
}

// buildUserDefinedTableTypeScript assembles one table type's script.
//
// A memory-optimized table type must be created WITH (MEMORY_OPTIMIZED = ON)
// and must carry an index — the server refuses one without — so the clause
// is emitted whenever the flag is set, and the index the type really has is
// left to the reader as a comment rather than guessed at: a table type's
// indexes hang off its internal table id, which gosmo does not read.
func buildUserDefinedTableTypeScript(t *UserDefinedTableType, cols []*Column, opts ScriptOptions) string {
	fullName := qualifiedName(t.Schema, t.Name)
	drop := fmt.Sprintf("DROP TYPE IF EXISTS %s;\nGO\n", fullName)
	guard := fmt.Sprintf("IF TYPE_ID(N'%s') IS NULL\n", escapeSingle(fullName))
	return opts.envelope(drop, guard, func(sb *strings.Builder) {
		fmt.Fprintf(sb, "CREATE TYPE %s AS TABLE (\n", fullName)
		for i, col := range cols {
			sb.WriteString("    ")
			sb.WriteString(tableTypeColumn(col))
			if i != len(cols)-1 {
				sb.WriteString(",")
			}
			sb.WriteString("\n")
		}
		if t.IsMemoryOptimized {
			sb.WriteString(")\nWITH (MEMORY_OPTIMIZED = ON);\nGO\n")
			return
		}
		sb.WriteString(");\nGO\n")
	})
}

// tableTypeColumn renders one column of a table type, in the same shape
// buildTableScript renders a table's.
func tableTypeColumn(col *Column) string {
	if col.IsComputed && col.ComputedText != "" {
		return fmt.Sprintf("%s AS %s", quoteIdent(col.Name), col.ComputedText)
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "%s %s", quoteIdent(col.Name), col.TypeString())
	if col.IsIdentity {
		fmt.Fprintf(&sb, " IDENTITY(%s,%s)", col.IdentitySeed, col.IdentityIncrement)
	}
	fmt.Fprintf(&sb, " %s", nullClause(col.IsNullable))
	if col.DefaultValue != nil {
		// A table type's default has no constraint name to keep: the name
		// belongs to the internal table, and naming it in the CREATE would
		// collide the second time the script runs.
		fmt.Fprintf(&sb, " DEFAULT %s", col.DefaultValue.Definition)
	}
	return sb.String()
}

// ============================================================
// CLR types  (CREATE TYPE ... EXTERNAL NAME)
// ============================================================

// ScriptClrType generates the CREATE (or DROP) script for one CLR type.
func (sc *Scripter) ScriptClrType(ctx context.Context, schema, name string) (string, error) {
	if err := requireSchema("script clr type", schema, name); err != nil {
		return "", err
	}
	t, err := sc.db.ClrTypeByName(ctx, schema, name)
	if err != nil {
		return "", err
	}
	return buildClrTypeScript(t, sc.opts), nil
}

// buildClrTypeScript assembles one CLR type's script. The CREATE names the
// assembly and class the type is implemented by; the assembly itself has to
// exist first, which is what the note says.
func buildClrTypeScript(t *ClrType, opts ScriptOptions) string {
	fullName := qualifiedName(t.Schema, t.Name)
	drop := fmt.Sprintf("DROP TYPE IF EXISTS %s;\nGO\n", fullName)
	return opts.envelope(drop, "", func(sb *strings.Builder) {
		sb.WriteString("/* The assembly named below must already be registered in the database. */\n")
		if opts.IncludeIfNotExists {
			fmt.Fprintf(sb, "IF TYPE_ID(N'%s') IS NULL\n", escapeSingle(fullName))
		}
		externalName := quoteIdent(t.Assembly)
		if t.AssemblyClass != "" {
			externalName += "." + quoteIdent(t.AssemblyClass)
		}
		fmt.Fprintf(sb, "CREATE TYPE %s EXTERNAL NAME %s;\nGO\n", fullName, externalName)
	})
}

// ============================================================
// XML schema collections
// ============================================================

// ScriptXMLSchemaCollection generates the CREATE (or DROP) script for one XML
// schema collection.
//
// The schema documents are read only for a verb that emits a CREATE:
// XML_SCHEMA_NAMESPACE reassembles the whole collection, which a drop does not
// need.
func (sc *Scripter) ScriptXMLSchemaCollection(ctx context.Context, schema, name string) (string, error) {
	if err := requireSchema("script XML schema collection", schema, name); err != nil {
		return "", err
	}
	c, err := sc.db.XMLSchemaCollectionByName(ctx, schema, name)
	if err != nil {
		return "", err
	}
	var def string
	if sc.opts.verb() != ScriptDrop {
		if def, err = c.Definition(ctx); err != nil {
			return "", err
		}
	}
	return buildXMLSchemaCollectionScript(c, def, sc.opts), nil
}

// buildXMLSchemaCollectionScript assembles one collection's script.
//
// DROP XML SCHEMA COLLECTION has no IF EXISTS form, so the drop is guarded
// with a sys.xml_schema_collections lookup — the schema is matched too,
// since two schemas can hold collections of the same name.
func buildXMLSchemaCollectionScript(c *XMLSchemaCollection, def string, opts ScriptOptions) string {
	fullName := qualifiedName(c.Schema, c.Name)
	drop := xmlSchemaCollectionGuard(c, "IF EXISTS") + "    DROP XML SCHEMA COLLECTION " + fullName + ";\nGO\n"
	guard := xmlSchemaCollectionGuard(c, "IF NOT EXISTS")
	return opts.envelope(drop, guard, func(sb *strings.Builder) {
		fmt.Fprintf(sb, "CREATE XML SCHEMA COLLECTION %s AS N'%s';\nGO\n",
			fullName, escapeSingle(def))
	})
}

// xmlSchemaCollectionGuard is the existence check both verbs use, in the
// sense the caller asks for.
func xmlSchemaCollectionGuard(c *XMLSchemaCollection, sense string) string {
	return fmt.Sprintf("%s (SELECT 1 FROM sys.xml_schema_collections x\n"+
		"    WHERE x.name = N'%s' AND SCHEMA_NAME(x.schema_id) = N'%s')\n",
		sense, escapeSingle(c.Name), escapeSingle(c.Schema))
}

// ============================================================
// Rules and defaults
// ============================================================

// ScriptRule generates the CREATE (or DROP) script for one rule.
func (sc *Scripter) ScriptRule(ctx context.Context, schema, name string) (string, error) {
	if err := requireSchema("script rule", schema, name); err != nil {
		return "", err
	}
	r, err := sc.db.RuleByName(ctx, schema, name)
	if err != nil {
		return "", err
	}
	return buildBoundObjectScript("RULE", r.Schema, r.Name, r.Definition, sc.opts), nil
}

// ScriptDefault generates the CREATE (or DROP) script for one standalone
// default.
func (sc *Scripter) ScriptDefault(ctx context.Context, schema, name string) (string, error) {
	if err := requireSchema("script default", schema, name); err != nil {
		return "", err
	}
	df, err := sc.db.DefaultByName(ctx, schema, name)
	if err != nil {
		return "", err
	}
	return buildBoundObjectScript("DEFAULT", df.Schema, df.Name, df.Definition, sc.opts), nil
}

// buildBoundObjectScript assembles a rule's or a default's script — one
// builder, because the two differ only in the keyword.
//
// The CREATE is the definition sys.sql_modules stores, emitted verbatim the
// way a view's or a procedure's is, and it carries no existence guard:
// CREATE RULE and CREATE DEFAULT must be the first statement in their batch,
// so an IF wrapped around either is a script the server refuses to parse.
// An encrypted object has no definition at all, and says so rather than
// emitting an empty batch that would look like a rule with no expression.
func buildBoundObjectScript(keyword, schema, name, definition string, opts ScriptOptions) string {
	fullName := qualifiedName(schema, name)
	drop := fmt.Sprintf("DROP %s IF EXISTS %s;\nGO\n", keyword, fullName)
	return opts.envelope(drop, "", func(sb *strings.Builder) {
		if strings.TrimSpace(definition) == "" {
			fmt.Fprintf(sb, "/* The definition of %s %s cannot be read — it is encrypted. */\n",
				strings.ToLower(keyword), fullName)
			return
		}
		sb.WriteString(strings.TrimRight(definition, "\r\n"))
		sb.WriteString("\nGO\n")
	})
}

// ============================================================
// Assemblies
// ============================================================

// assemblyBinaryPlaceholder stands in for an assembly's payload in a
// generated script. The bytes run to megabytes and are not what a reader of
// a script wants pasted into a query window, so the CREATE carries a
// placeholder that cannot be mistaken for a real assembly — the same
// treatment, and for the same reason, as credentialSecretPlaceholder.
const assemblyBinaryPlaceholder = "0x00 /* <replace with the assembly binary, or a FROM '<path>' clause> */"

// ScriptAssembly generates the CREATE (or DROP) script for one CLR assembly.
func (sc *Scripter) ScriptAssembly(ctx context.Context, name string) (string, error) {
	a, err := sc.db.AssemblyByName(ctx, name)
	if err != nil {
		return "", err
	}
	return buildAssemblyScript(a, sc.opts), nil
}

// buildAssemblyScript assembles one assembly's script.
//
// The payload is elided (see assemblyBinaryPlaceholder), so the script is a
// template rather than something that runs as generated, and it says so.
// PERMISSION_SET is always emitted: its default is SAFE, and an assembly
// registered UNSAFE that came back SAFE would run under a policy it was not
// given.
func buildAssemblyScript(a *Assembly, opts ScriptOptions) string {
	drop := fmt.Sprintf("DROP ASSEMBLY IF EXISTS %s;\nGO\n", quoteIdent(a.Name))
	return opts.envelope(drop, "", func(sb *strings.Builder) {
		sb.WriteString("/* The assembly binary cannot be scripted — it is not text. Replace the\n" +
			"   placeholder below with the assembly's bytes, or with a FROM '<path>' clause. */\n")
		fmt.Fprintf(sb, "CREATE ASSEMBLY %s", quoteIdent(a.Name))
		if a.Owner != "" {
			fmt.Fprintf(sb, " AUTHORIZATION %s", quoteIdent(a.Owner))
		}
		permSet := a.PermissionSet
		if permSet == "" {
			permSet = AssemblySafe
		}
		fmt.Fprintf(sb, "\nFROM %s\nWITH PERMISSION_SET = %s;\nGO\n",
			assemblyBinaryPlaceholder, permSet)
		if !a.IsVisible {
			// is_visible = 0 is how a referenced-only dependency is registered;
			// recreating it visible would let CREATE PROCEDURE bind to routines
			// the original assembly deliberately hides.
			fmt.Fprintf(sb, "\nALTER ASSEMBLY %s WITH VISIBILITY = OFF;\nGO\n", quoteIdent(a.Name))
		}
	})
}

// ============================================================
// Plan guides
// ============================================================

// ScriptPlanGuide generates the CREATE (or DROP) script for one plan guide.
func (sc *Scripter) ScriptPlanGuide(ctx context.Context, name string) (string, error) {
	g, err := sc.db.PlanGuideByName(ctx, name)
	if err != nil {
		return "", err
	}
	return buildPlanGuideScript(g, sc.opts), nil
}

// buildPlanGuideScript assembles one plan guide's script.
//
// A plan guide has no CREATE statement: it is an sp_create_plan_guide call,
// and its arguments are the query text, the scope and the hints, so the
// script is that EXEC with each argument named. @module_or_batch carries the
// routine for an OBJECT-scoped guide and the batch text for a SQL-scoped
// one — the same parameter, two different things, which is why the two
// scopes are branched on rather than both reading whichever field is
// non-empty.
//
// A guide created from a plan handle stores XML showplan in Hints; that is
// what sp_create_plan_guide takes back, so it is emitted as stored.
//
// The drop is sp_control_plan_guide's, the statement gosmo's own
// PlanGuide.Drop runs.
func buildPlanGuideScript(g *PlanGuide, opts ScriptOptions) string {
	drop := fmt.Sprintf("EXEC sp_control_plan_guide @operation = N'DROP', @name = N'%s';\nGO\n",
		escapeSingle(g.Name))
	return opts.envelope(drop, "", func(sb *strings.Builder) {
		fmt.Fprintf(sb, "EXEC sp_create_plan_guide\n     @name = N'%s',\n     @stmt = N'%s',\n     @type = N'%s',\n",
			escapeSingle(g.Name), escapeSingle(g.QueryText), escapeSingle(string(g.Scope)))
		fmt.Fprintf(sb, "     @module_or_batch = %s,\n", planGuideModuleOrBatch(g))
		fmt.Fprintf(sb, "     @params = %s,\n", nullableLiteral(g.Parameters))
		fmt.Fprintf(sb, "     @hints = %s;\nGO\n", nullableLiteral(g.Hints))
		if g.IsDisabled {
			// A guide created by sp_create_plan_guide is enabled; recreating a
			// disabled one without this would quietly start applying hints the
			// original stopped applying.
			fmt.Fprintf(sb, "\nEXEC sp_control_plan_guide @operation = N'DISABLE', @name = N'%s';\nGO\n",
				escapeSingle(g.Name))
		}
	})
}

// planGuideModuleOrBatch is the @module_or_batch argument for a guide's
// scope: the routine for OBJECT, the batch text for SQL when it is bound to
// one, and NULL otherwise — which is what makes a SQL-scoped guide match the
// statement in any batch, and the only value TEMPLATE accepts.
func planGuideModuleOrBatch(g *PlanGuide) string {
	if g.Scope == PlanGuideScopeObject {
		return nullableLiteral(g.ScopeObject)
	}
	if g.Scope == PlanGuideScopeSQL {
		return nullableLiteral(g.ScopeBatch)
	}
	return "NULL"
}

// nullableLiteral renders an optional argument: an N'…' literal, or the
// keyword NULL for the empty string. Every field gosmo reads here is
// ISNULL'd to ” at the catalog, and passing ” where the procedure expects
// NULL is not the same argument.
func nullableLiteral(s string) string {
	if s == "" {
		return "NULL"
	}
	return "N'" + escapeSingle(s) + "'"
}
