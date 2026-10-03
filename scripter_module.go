package gosmo

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"strings"
	"unicode"
)

// ============================================================
// Modules (view, stored procedure, function, trigger)
// ============================================================

// moduleKind describes one family of sys.sql_modules-backed objects: the DDL
// keyword its DROP uses, the noun a not-found error names it by, and the
// catalog query that finds its definition by schema and name.
type moduleKind struct {
	keyword string
	noun    string
	query   string
	// params marks a kind that has parameters, whose float16 vectors the
	// script names (float16Parameters).
	params bool
}

var (
	moduleView = moduleKind{"VIEW", "view", `
SELECT m.definition, m.uses_ansi_nulls, m.uses_quoted_identifier, CAST(0 AS bit)
FROM   sys.views v
JOIN   sys.sql_modules m ON m.object_id = v.object_id
WHERE  SCHEMA_NAME(v.schema_id) = @p1 AND v.name = @p2`, false}

	moduleProcedure = moduleKind{"PROCEDURE", "stored procedure", `
SELECT m.definition, ISNULL(m.uses_ansi_nulls, 0), ISNULL(m.uses_quoted_identifier, 0),
       CAST(CASE WHEN m.object_id IS NULL THEN 1 ELSE 0 END AS bit)
FROM   sys.procedures p
LEFT   JOIN sys.sql_modules m ON m.object_id = p.object_id
WHERE  SCHEMA_NAME(p.schema_id) = @p1 AND p.name = @p2`, true}

	moduleFunction = moduleKind{"FUNCTION", "function", `
SELECT m.definition, ISNULL(m.uses_ansi_nulls, 0), ISNULL(m.uses_quoted_identifier, 0),
       CAST(CASE WHEN m.object_id IS NULL THEN 1 ELSE 0 END AS bit)
FROM   sys.objects o
LEFT   JOIN sys.sql_modules m ON m.object_id = o.object_id
WHERE  SCHEMA_NAME(o.schema_id) = @p1 AND o.name = @p2
  AND  o.type IN ('FN','TF','IF','FS','FT')`, true}

	// A trigger's own schema is its parent table's — sys.triggers has no
	// schema_id of its own.
	moduleTrigger = moduleKind{"TRIGGER", "trigger", `
SELECT m.definition, ISNULL(m.uses_ansi_nulls, 0), ISNULL(m.uses_quoted_identifier, 0),
       CAST(CASE WHEN m.object_id IS NULL THEN 1 ELSE 0 END AS bit)
FROM   sys.triggers tr
JOIN   sys.objects o     ON o.object_id = tr.parent_id
LEFT   JOIN sys.sql_modules m ON m.object_id = tr.object_id
WHERE  SCHEMA_NAME(o.schema_id) = @p1 AND tr.name = @p2`, false}
)

// scriptModule renders one sys.sql_modules-backed object. CREATE and ALTER
// are the stored definition itself (rewritten for ALTER), never synthesized,
// so nothing about the original text is lost.
//
// An encrypted module stores a NULL definition, and it is an error rather
// than a script: envelopeErr discards the DROP it has already written, which
// would otherwise have left DROP AND CREATE a script that drops the module
// and does not put it back. DROP alone needs no definition, never reads it,
// and still works on one. A CLR procedure, function or trigger has no
// sys.sql_modules row at all; it is found through the LEFT JOIN and refused
// with ErrUnsupported, where a JOIN reported an object the caller can see in
// the listing as not found.
func (sc *Scripter) scriptModule(ctx context.Context, k moduleKind, schema, name string) (string, error) {
	drop := fmt.Sprintf("DROP %s IF EXISTS %s;\nGO\n", k.keyword, qualifiedName(schema, name))
	return sc.opts.envelopeErr(drop, "", func(sb *strings.Builder) error {
		var def sql.NullString
		var ansiNulls, quotedIdent, clr bool
		err := sc.db.queryRow(ctx, func(row *sql.Row) error {
			return row.Scan(&def, &ansiNulls, &quotedIdent, &clr)
		}, k.query, schema, name)
		if err != nil {
			return rowErr(err, notFoundf("gosmo: %s %s not found", k.noun, qualifiedName(schema, name)),
				fmt.Sprintf("script %s %s", k.noun, qualifiedName(schema, name)))
		}
		if clr {
			return unsupportedf("gosmo: script %s %s: a CLR %s has no T-SQL definition to script", k.noun, qualifiedName(schema, name), k.noun)
		}
		if !def.Valid {
			return fmt.Errorf("gosmo: script %s %s: definition is not readable (encrypted)", k.noun, qualifiedName(schema, name))
		}
		text := def.String
		if sc.opts.verb() == ScriptAlter {
			text = alterModuleDefinition(text)
		}
		var names []string
		if k.params {
			names, err = sc.db.float16Parameters(ctx, schema, name)
			if err != nil {
				return fmt.Errorf("gosmo: script %s %s: %w", k.noun, qualifiedName(schema, name), err)
			}
		}
		switch {
		case len(names) > 0:
			sb.WriteString(float16Note(names))
		case usesFloat16Vector(def.String):
			sb.WriteString(float16DefinitionNote)
		}
		sb.WriteString(moduleSetOptions(ansiNulls, quotedIdent))
		sb.WriteString(text)
		sb.WriteString("\nGO\n")
		return nil
	})
}

// float16Parameters lists [schema].[name]'s vector(n, float16) parameters,
// comment-safe, for the same PREVIEW_FEATURES note a table script carries
// (previewFeaturesNote): the type sits inside the stored definition, but
// parameters are cataloged. A local variable, table-variable column, RETURNS
// table column or CAST target is not; usesFloat16Vector finds those in the
// definition instead, unnamed. Below SQL Server 2025 there is no vector type
// and no read.
func (d *Database) float16Parameters(ctx context.Context, schema, name string) ([]string, error) {
	if m := d.serverMajorVersion(); m != 0 && m < int(SQLServer2025) {
		return nil, nil
	}
	rows, err := d.query(ctx, `
SELECT p.name
FROM   sys.parameters p
WHERE  p.object_id = OBJECT_ID(QUOTENAME(@p1) + N'.' + QUOTENAME(@p2))
  AND  p.vector_base_type_desc = N'float16'
ORDER  BY p.parameter_id`, schema, name)
	return scanRows(rows, err, "", func(scan func(...any) error) (string, error) {
		var n string
		err := scan(&n)
		return commentSafe(n), err
	})
}

// float16VectorType matches vector(<int>, float16) in a definition's code,
// case-insensitively and with any whitespace a comment left between the
// tokens. The leading class keeps a longer identifier ending in "vector"
// (my_vector, x$vector, @vector) from matching.
var float16VectorType = regexp.MustCompile(`(?i)(?:^|[^\w@#$])vector\s*\(\s*\d+\s*,\s*float16\s*\)`)

// usesFloat16Vector reports whether a module definition names the
// vector(n, float16) type anywhere a parameter catalog cannot see: a DECLAREd
// variable, a table variable's or RETURNS table's column, a CAST. Replaying
// it into a database with PREVIEW_FEATURES off fails with Msg 195 just as a
// float16 parameter does.
//
// It is a lexical scan, not a parse: scriptCodeSpans drops comments, string
// literals and quoted identifiers, each replaced by a space since the server
// treats a comment between two tokens as whitespace. The type spelled inside
// dynamic SQL is a string literal, and is not found.
func usesFloat16Vector(def string) bool {
	var code strings.Builder
	for _, sp := range scriptCodeSpans(def) {
		code.WriteString(def[sp.start:sp.end])
		code.WriteByte(' ')
	}
	return float16VectorType.MatchString(code.String())
}

// moduleSetOptions renders the two SET options a module is compiled under,
// each in its own batch ahead of the definition, as SSMS does.
//
// They are not decoration. A module keeps the ANSI_NULLS and
// QUOTED_IDENTIFIER settings of the session that created it, and they govern
// what it does: whether "= NULL" ever matches, and whether "x" is a string
// or an identifier. Without them the module is recreated under whatever the
// running session has — ON for gossms and SSMS — so a legacy module written
// under OFF silently changes behaviour, or fails to compile where it used
// "string" literals.
func moduleSetOptions(ansiNulls, quotedIdent bool) string {
	return fmt.Sprintf("SET ANSI_NULLS %s;\nGO\nSET QUOTED_IDENTIFIER %s;\nGO\n", onOff(ansiNulls), onOff(quotedIdent))
}

// createKeyword matches the CREATE that opens a module definition once its
// leading whitespace and comments are skipped — the only CREATE that may be
// rewritten to ALTER. A CREATE elsewhere in the body (a temp table, say) must
// be left exactly as the author wrote it.
var createKeyword = regexp.MustCompile(`(?is)^CREATE(\s+OR\s+ALTER)?(\s)`)

// alterModuleDefinition rewrites a module's stored CREATE into an ALTER,
// leaving a definition it can't recognize untouched — an unrecognized one
// still runs, as the CREATE it already was, which beats emitting mangled DDL.
// A CREATE OR ALTER definition is already re-runnable and is returned as is.
func alterModuleDefinition(def string) string {
	at := leadingTriviaEnd(def)
	if at < 0 {
		return def
	}
	m := createKeyword.FindStringSubmatch(def[at:])
	if m == nil || m[1] != "" {
		return def
	}
	return def[:at] + "ALTER" + m[2] + def[at+len(m[0]):]
}

// leadingTriviaEnd returns the offset of the first character of def that is
// neither whitespace nor inside a comment, or -1 if there is none or the
// first thing def holds is quoted text rather than code.
//
// This is scriptCodeSpans' walk, not a regular expression, because T-SQL
// block comments nest: a lazy /\*.*?\*/ stops at the first */ of
// "/* a /* b */ c */", so a definition opening with a nested comment was
// not recognised and Script as ALTER handed back the CREATE, which then
// failed with "There is already an object named …".
func leadingTriviaEnd(def string) int {
	for _, sp := range scriptCodeSpans(def) {
		if sp.start > sp.prev && def[sp.prev] != '-' && def[sp.prev] != '/' {
			return -1
		}
		code := def[sp.start:sp.end]
		if i := strings.IndexFunc(code, func(r rune) bool { return !unicode.IsSpace(r) }); i >= 0 {
			return sp.start + i
		}
	}
	return -1
}

// ============================================================
// View
// ============================================================

// ScriptView returns the CREATE VIEW definition as stored in sys.sql_modules.
func (sc *Scripter) ScriptView(ctx context.Context, schema, name string) (string, error) {
	if err := requireSchema("script view", schema, name); err != nil {
		return "", err
	}
	return sc.scriptModule(ctx, moduleView, schema, name)
}

// ============================================================
// Stored Procedure
// ============================================================

// ScriptStoredProcedure returns the CREATE PROCEDURE definition.
func (sc *Scripter) ScriptStoredProcedure(ctx context.Context, schema, name string) (string, error) {
	if err := requireSchema("script stored procedure", schema, name); err != nil {
		return "", err
	}
	return sc.scriptModule(ctx, moduleProcedure, schema, name)
}

// ============================================================
// Function
// ============================================================

// ScriptFunction returns the CREATE FUNCTION definition.
func (sc *Scripter) ScriptFunction(ctx context.Context, schema, name string) (string, error) {
	if err := requireSchema("script function", schema, name); err != nil {
		return "", err
	}
	return sc.scriptModule(ctx, moduleFunction, schema, name)
}

// ============================================================
// Trigger
// ============================================================

// ScriptTrigger returns the CREATE TRIGGER definition. schema is the
// trigger's own schema, i.e. its parent table's.
//
// The definition alone recreates the trigger enabled and unordered, so the
// state it does not carry follows it: sp_settriggerorder for each statement
// type the trigger fires First or Last on, then DISABLE TRIGGER for a
// disabled one. Both follow ALTER too — ALTER TRIGGER resets a First or Last
// order to None.
func (sc *Scripter) ScriptTrigger(ctx context.Context, schema, name string) (string, error) {
	if err := requireSchema("script trigger", schema, name); err != nil {
		return "", err
	}
	script, err := sc.scriptModule(ctx, moduleTrigger, schema, name)
	if err != nil || sc.opts.verb() == ScriptDrop {
		return script, err
	}
	state, err := sc.triggerState(ctx, schema, name)
	if err != nil {
		return "", err
	}
	return script + state, nil
}

// triggerState renders the statements that restore a DML trigger's firing
// order and disabled state after its definition has been run.
//
// DISABLE TRIGGER … ON rather than ALTER TABLE … DISABLE TRIGGER, because an
// INSTEAD OF trigger's parent can be a view, which ALTER TABLE refuses.
func (sc *Scripter) triggerState(ctx context.Context, schema, name string) (string, error) {
	const q = `
SELECT OBJECT_NAME(tr.parent_id), tr.is_disabled,
       ISNULL(te.type_desc, ''), ISNULL(te.is_first, 0), ISNULL(te.is_last, 0)
FROM   sys.triggers tr
JOIN   sys.objects o ON o.object_id = tr.parent_id
LEFT   JOIN sys.trigger_events te
       ON  te.object_id = tr.object_id AND (te.is_first = 1 OR te.is_last = 1)
WHERE  SCHEMA_NAME(o.schema_id) = @p1 AND tr.name = @p2
ORDER  BY te.type`
	type orderRow struct {
		parent, event string
		disabled      bool
		first, last   bool
	}
	rows, err := sc.db.query(ctx, q, schema, name)
	got, err := scanRows(rows, err, fmt.Sprintf("read state of trigger %s", qualifiedName(schema, name)), func(scan func(...any) error) (orderRow, error) {
		var r orderRow
		err := scan(&r.parent, &r.disabled, &r.event, &r.first, &r.last)
		return r, err
	})
	if err != nil {
		return "", err
	}
	if len(got) == 0 {
		return "", notFoundf("gosmo: trigger %s not found", qualifiedName(schema, name))
	}
	var sb strings.Builder
	trigger := qualifiedName(schema, name)
	for _, r := range got {
		order := ""
		switch {
		case r.first:
			order = "First"
		case r.last:
			order = "Last"
		default:
			continue
		}
		fmt.Fprintf(&sb, "EXEC sys.sp_settriggerorder @triggername = N'%s', @order = N'%s', @stmttype = N'%s';\nGO\n",
			escapeSingle(trigger), order, escapeSingle(r.event))
	}
	if got[0].disabled {
		fmt.Fprintf(&sb, "DISABLE TRIGGER %s ON %s;\nGO\n", trigger, qualifiedName(schema, got[0].parent))
	}
	return sb.String(), nil
}

// ============================================================
// Database Trigger
// ============================================================

// ScriptDatabaseTrigger generates the CREATE (or DROP) script for one
// database-scope DDL trigger.
//
// scriptModule is not reusable here: it addresses a module by schema and name,
// and a DDL trigger has no schema.
func (sc *Scripter) ScriptDatabaseTrigger(ctx context.Context, name string) (string, error) {
	t, err := sc.db.DatabaseTriggerByName(ctx, name)
	if err != nil {
		return "", err
	}
	return buildDatabaseTriggerScript(t, sc.opts)
}

// buildDatabaseTriggerScript assembles one database trigger's script from the
// definition sys.sql_modules stores.
//
// A trigger with no readable definition — encrypted, or CLR, which has no row
// in that view at all — is an error rather than an empty CREATE half: emitting
// nothing produces a script that drops the trigger and does not put it back.
// IncludeIfNotExists is not honoured because CREATE TRIGGER must be the first
// statement in its batch, the same reason scriptModule ignores it. Both are
// buildServerTriggerScript's reasoning, unchanged; only the scope clause
// differs.
func buildDatabaseTriggerScript(t *DatabaseTrigger, opts ScriptOptions) (string, error) {
	drop := fmt.Sprintf("DROP TRIGGER IF EXISTS %s ON DATABASE;\nGO\n", quoteIdent(t.Name))
	return opts.envelopeErr(drop, "", func(sb *strings.Builder) error {
		if strings.TrimSpace(t.Definition) == "" {
			return fmt.Errorf("gosmo: script database trigger %q: definition is not readable (encrypted or CLR)", t.Name)
		}
		def := t.Definition
		if opts.verb() == ScriptAlter {
			def = alterModuleDefinition(def)
		}
		sb.WriteString(def)
		sb.WriteString("\nGO\n")
		if !t.IsEnabled {
			fmt.Fprintf(sb, "DISABLE TRIGGER %s ON DATABASE;\nGO\n", quoteIdent(t.Name))
		}
		return nil
	})
}
