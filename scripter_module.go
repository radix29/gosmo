package gosmo

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/radix29/gosmo/tsql"
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
SELECT m.definition, m.uses_ansi_nulls, m.uses_quoted_identifier, CAST(0 AS bit), v.object_id
FROM   sys.views v
JOIN   sys.sql_modules m ON m.object_id = v.object_id
WHERE  SCHEMA_NAME(v.schema_id) COLLATE DATABASE_DEFAULT = @p1 AND v.name = @p2`, false}

	moduleProcedure = moduleKind{"PROCEDURE", "stored procedure", `
SELECT m.definition, ISNULL(m.uses_ansi_nulls, 0), ISNULL(m.uses_quoted_identifier, 0),
       CAST(CASE WHEN m.object_id IS NULL THEN 1 ELSE 0 END AS bit), p.object_id
FROM   sys.procedures p
LEFT   JOIN sys.sql_modules m ON m.object_id = p.object_id
WHERE  SCHEMA_NAME(p.schema_id) COLLATE DATABASE_DEFAULT = @p1 AND p.name = @p2`, true}

	moduleFunction = moduleKind{"FUNCTION", "function", `
SELECT m.definition, ISNULL(m.uses_ansi_nulls, 0), ISNULL(m.uses_quoted_identifier, 0),
       CAST(CASE WHEN m.object_id IS NULL THEN 1 ELSE 0 END AS bit), o.object_id
FROM   sys.objects o
LEFT   JOIN sys.sql_modules m ON m.object_id = o.object_id
WHERE  SCHEMA_NAME(o.schema_id) COLLATE DATABASE_DEFAULT = @p1 AND o.name = @p2
  AND  o.type IN ('FN','TF','IF','FS','FT')`, true}

	// A trigger's own schema is its parent table's — sys.triggers has no
	// schema_id of its own.
	moduleTrigger = moduleKind{"TRIGGER", "trigger", `
SELECT m.definition, ISNULL(m.uses_ansi_nulls, 0), ISNULL(m.uses_quoted_identifier, 0),
       CAST(CASE WHEN m.object_id IS NULL THEN 1 ELSE 0 END AS bit), tr.object_id
FROM   sys.triggers tr
JOIN   sys.objects o     ON o.object_id = tr.parent_id
LEFT   JOIN sys.sql_modules m ON m.object_id = tr.object_id
WHERE  SCHEMA_NAME(o.schema_id) COLLATE DATABASE_DEFAULT = @p1 AND tr.name = @p2`, false}
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
// sys.sql_modules row at all; it is found through the LEFT JOIN (a JOIN
// reported an object the caller can see in the listing as not found) and its
// CREATE is rebuilt from the catalog by clrSchemaModule. It carries no SET
// options: they govern T-SQL compilation, and a CLR module has none.
//
// then, when not nil, writes what follows the definition (a view's indexes)
// from the module's object_id; the module's extended properties come last,
// after anything then created that they sit on.
func (sc *Scripter) scriptModule(ctx context.Context, k moduleKind, schema, name string,
	then func(sb *strings.Builder, objectID int) error) (string, error) {
	drop := fmt.Sprintf("DROP %s IF EXISTS %s;\nGO\n", k.keyword, qualifiedName(schema, name))
	return sc.opts.envelopeErr(drop, "", func(sb *strings.Builder) error {
		var def sql.NullString
		var ansiNulls, quotedIdent, clr bool
		var objectID int
		err := sc.db.queryRow(ctx, func(row *sql.Row) error {
			return row.Scan(&def, &ansiNulls, &quotedIdent, &clr, &objectID)
		}, k.query, schema, name)
		if err != nil {
			return rowErr(err, notFoundf("gosmo: %s %s not found", k.noun, qualifiedName(schema, name)),
				fmt.Sprintf("script %s %s", k.noun, qualifiedName(schema, name)))
		}
		if err := sc.writeModuleDefinition(ctx, sb, k, schema, name, def, ansiNulls, quotedIdent, clr, objectID); err != nil {
			return err
		}
		if then != nil {
			if err := then(sb, objectID); err != nil {
				return fmt.Errorf("gosmo: script %s %s: %w", k.noun, qualifiedName(schema, name), err)
			}
		}
		props, err := sc.scriptExtendedProperties(ctx, objectID, false, nil)
		if err != nil {
			return fmt.Errorf("gosmo: script %s %s: %w", k.noun, qualifiedName(schema, name), err)
		}
		sb.WriteString(props)
		return nil
	})
}

// writeModuleDefinition writes scriptModule's CREATE or ALTER: the stored
// definition with its SET options, or a CLR module's rebuilt one.
func (sc *Scripter) writeModuleDefinition(ctx context.Context, sb *strings.Builder, k moduleKind, schema, name string,
	def sql.NullString, ansiNulls, quotedIdent, clr bool, objectID int) error {
	if clr {
		m, err := sc.db.clrSchemaModule(ctx, k.keyword, objectID, schema, name)
		if err != nil {
			return fmt.Errorf("gosmo: script %s %s: %w", k.noun, qualifiedName(schema, name), err)
		}
		text := renameModuleHeader(renderCLRModule(m), k.keyword, schema, name)
		if sc.opts.verb() == ScriptAlter {
			text = alterModuleDefinition(text)
		}
		sb.WriteString(text)
		sb.WriteString("\nGO\n")
		return nil
	}
	if !def.Valid {
		return sc.db.hiddenModuleRefusal(ctx, k, schema, name, objectID)
	}
	text := renameModuleHeader(def.String, k.keyword, schema, name)
	if sc.opts.verb() == ScriptAlter {
		text = alterModuleDefinition(text)
	}
	var names []string
	if k.params {
		var err error
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
	case buildsFloat16Vector(def.String):
		sb.WriteString(float16DynamicNote)
	}
	sb.WriteString(moduleSetOptions(ansiNulls, quotedIdent))
	sb.WriteString(text)
	sb.WriteString("\nGO\n")
	return nil
}

// hiddenModuleRefusal is the refusal for a module whose definition read NULL:
// it is encrypted, which no permission reveals, or the login lacks VIEW
// DEFINITION on it — which EXECUTE or SELECT alone leaves out, and a script
// must not call encryption. OBJECTPROPERTY needs only the object to be
// visible, as it is here, so one more read tells the two apart; it is made
// only on this path.
func (d *Database) hiddenModuleRefusal(ctx context.Context, k moduleKind, schema, name string, objectID int) error {
	var encrypted sql.NullInt64
	err := d.queryRow(ctx, func(row *sql.Row) error { return row.Scan(&encrypted) },
		`SELECT OBJECTPROPERTY(@p1, 'IsEncrypted')`, objectID)
	if err != nil {
		return fmt.Errorf("gosmo: script %s %s: %w", k.noun, qualifiedName(schema, name), err)
	}
	if encrypted.Valid && encrypted.Int64 == 1 {
		return unsupportedf("gosmo: script %s %s: definition is not readable (encrypted)", k.noun, qualifiedName(schema, name))
	}
	return unsupportedf("gosmo: script %s %s: %w; it needs VIEW DEFINITION on it",
		k.noun, qualifiedName(schema, name), ErrDefinitionHidden)
}

// float16Parameters lists [schema].[name]'s vector(n, float16) parameters,
// comment-safe, for the same PREVIEW_FEATURES note a table script carries
// (previewFeaturesNote): the type sits inside the stored definition, but
// parameters are cataloged. A local variable, table-variable column, RETURNS
// table column or CAST target is not; usesFloat16Vector finds those in the
// definition instead, unnamed. Below SQL Server 2025 there is no vector type
// and no read.
func (d *Database) float16Parameters(ctx context.Context, schema, name string) ([]string, error) {
	if d.before(SQLServer2025) {
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
		return tsql.CommentSafe(n), err
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
// It is a lexical scan, not a parse: codeOnly drops comments, string
// literals and quoted identifiers. The type spelled inside dynamic SQL is a
// string literal, and is not found: buildsFloat16Vector looks there.
func usesFloat16Vector(def string) bool {
	return float16VectorType.MatchString(codeOnly(def))
}

// codeOnly returns def's code (tsql.Spans) with every comment, literal and
// quoted identifier replaced by a space, as the server treats a comment
// between two tokens as whitespace.
func codeOnly(def string) string {
	var code strings.Builder
	for sp := range tsql.Spans(def) {
		if sp.Kind == tsql.SpanCode {
			code.WriteString(def[sp.Start:sp.End])
		} else {
			code.WriteByte(' ')
		}
	}
	return code.String()
}

// executesDynamicSQL matches EXEC, EXECUTE or sp_executesql as a word.
var executesDynamicSQL = regexp.MustCompile(`(?i)(?:^|[^\w@#$])(?:exec(?:ute)?|sp_executesql)(?:[^\w@#$]|$)`)

// buildsFloat16Vector reports whether a definition that executes dynamic SQL
// spells vector(n, float16) inside a string literal, as code of that literal
// (not in its comments or strings) or, nested, inside a literal it executes
// in turn. Unlike usesFloat16Vector's case the CREATE replays with
// PREVIEW_FEATURES off; the module fails with Msg 195 when it runs the batch.
//
// It is conservative, not a parse: any EXEC in the code — a procedure call
// too — admits every literal of the definition, since the batch is usually
// assigned to a variable before it is executed. A false positive costs a
// comment. Literals joined by + are folded first (stringLiterals), so a type
// name split across them is found; one split around a variable is not.
func buildsFloat16Vector(def string) bool {
	if !executesDynamicSQL.MatchString(codeOnly(def)) {
		return false
	}
	for _, text := range stringLiterals(def) {
		if usesFloat16Vector(text) || buildsFloat16Vector(text) {
			return true
		}
	}
	return false
}

// concatenatesLiterals matches the code between two string literals that
// concatenates them: a + and nothing else.
var concatenatesLiterals = regexp.MustCompile(`^\s*\+\s*$`)

// stringLiterals returns the values of def's string literals, unescaped, in
// order. A literal joined to the one before it by + alone, comments aside, is
// folded into it, as the server would concatenate them: 'vector(4, ' +
// N'float16)' is one value.
func stringLiterals(def string) []string {
	var lits []string
	var between strings.Builder // the code since the last literal
	for sp := range tsql.Spans(def) {
		switch {
		case sp.Kind == tsql.SpanCode:
			between.WriteString(def[sp.Start:sp.End])
		case sp.IsLiteral():
			if text := sp.Unescape(def); len(lits) > 0 && concatenatesLiterals.MatchString(between.String()) {
				lits[len(lits)-1] += text
			} else {
				lits = append(lits, text)
			}
			between.Reset()
		}
	}
	return lits
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
// This is tsql.Spans' walk, not a regular expression, because T-SQL
// block comments nest: a lazy /\*.*?\*/ stops at the first */ of
// "/* a /* b */ c */", so a definition opening with a nested comment was
// not recognised and Script as ALTER handed back the CREATE, which then
// failed with "There is already an object named …".
func leadingTriviaEnd(def string) int {
	for sp := range tsql.Spans(def) {
		switch {
		case sp.IsComment():
		case sp.Kind != tsql.SpanCode:
			return -1
		default:
			if i := strings.IndexFunc(def[sp.Start:sp.End], func(r rune) bool { return !unicode.IsSpace(r) }); i >= 0 {
				return sp.Start + i
			}
		}
	}
	return -1
}

// renameModuleHeader replaces the name in a module definition's header —
// CREATE [OR ALTER] <keyword> <name> — with [schema].[name] when it does not
// already name that object, and returns def unchanged when it does or when
// the header cannot be read.
//
// sys.sql_modules keeps the text the module was created with, and neither
// sp_rename nor ALTER SCHEMA … TRANSFER touches it: a renamed procedure's
// stored definition still says CREATE PROCEDURE dbo.oldp. Replayed verbatim,
// the CREATE recreates the old name, a DROP AND CREATE drops the new one and
// creates the old, and the ALTER fails — or, once something else has taken
// the old name, alters that instead. SMO rewrites the header for the same
// reason.
//
// Only the header name is touched: a trigger's ON <table> and the body are
// left as written. A one-part name resolves to the creating session's default
// schema, taken here as dbo; a trigger's resolves to its table's schema,
// which is the trigger's own. A name that matches is left as written, so an
// unrenamed module scripts byte for byte as it is stored.
func renameModuleHeader(def, keyword, schema, name string) string {
	at := leadingTriviaEnd(def)
	if at < 0 {
		return def
	}
	tok, ok := nextHeaderToken(def, at)
	if !ok || !tok.isWord("CREATE") && !tok.isWord("ALTER") {
		return def
	}
	if tok.isWord("CREATE") {
		if tok, ok = nextHeaderToken(def, tok.end); ok && tok.isWord("OR") {
			if tok, ok = nextHeaderToken(def, tok.end); !ok || !tok.isWord("ALTER") {
				return def
			}
			tok, ok = nextHeaderToken(def, tok.end)
		}
	} else {
		tok, ok = nextHeaderToken(def, tok.end)
	}
	if !ok || !tok.isWord(keyword) && !(keyword == "PROCEDURE" && tok.isWord("PROC")) {
		return def
	}

	// The name: one to three identifiers joined by dots, a part between two
	// dots allowed to be empty (db..name).
	if tok, ok = nextHeaderToken(def, tok.end); !ok || !tok.ident {
		return def
	}
	parts := []string{tok.text}
	start, end := tok.start, tok.end
	for {
		dot, ok := nextHeaderToken(def, end)
		if !ok || dot.ident || dot.text != "." {
			break
		}
		tok, ok = nextHeaderToken(def, dot.end)
		for ok && !tok.ident && tok.text == "." {
			parts = append(parts, "")
			tok, ok = nextHeaderToken(def, tok.end)
		}
		if !ok || !tok.ident {
			return def
		}
		parts = append(parts, tok.text)
		end = tok.end
	}
	if len(parts) > 3 {
		return def
	}
	resolves := parts[len(parts)-1] == name
	switch {
	case len(parts) >= 2:
		resolves = resolves && parts[len(parts)-2] == schema
	case keyword != "TRIGGER":
		resolves = resolves && schema == "dbo"
	}
	if resolves {
		return def
	}
	return def[:start] + qualifiedName(schema, name) + def[end:]
}

// headerToken is one token of a module header: an identifier — a word, or a
// [bracketed] or "quoted" one with its value unescaped — or a single other
// character.
type headerToken struct {
	start, end int
	text       string
	ident      bool
	quoted     bool
}

// isWord reports whether t is the unquoted keyword kw, in any case.
func (t headerToken) isWord(kw string) bool {
	return t.ident && !t.quoted && strings.EqualFold(t.text, kw)
}

// nextHeaderToken returns the first token of def at or after i, skipping
// whitespace and comments (tsql.Spans). ok is false at the end of def and at
// an unterminated quoted identifier, where nothing after can be trusted. A
// string literal is one token that is not an identifier.
func nextHeaderToken(def string, i int) (headerToken, bool) {
	isWordRune := func(r rune) bool {
		return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '@' || r == '#' || r == '$'
	}
	rest := def[i:]
	for sp := range tsql.Spans(rest) {
		start, end := i+sp.Start, i+sp.End
		switch {
		case sp.IsComment():
			continue
		case sp.IsLiteral():
			return headerToken{start: start, end: end, text: def[start:end]}, true
		case sp.IsQuoted():
			if !sp.Closed {
				return headerToken{}, false
			}
			return headerToken{start: start, end: end, text: sp.Unescape(rest), ident: true, quoted: true}, true
		}
		for j := start; j < end; {
			r, size := utf8.DecodeRuneInString(def[j:end])
			switch {
			case unicode.IsSpace(r):
				j += size
			case isWordRune(r):
				k := j + size
				for k < end {
					r, size := utf8.DecodeRuneInString(def[k:end])
					if !isWordRune(r) {
						break
					}
					k += size
				}
				return headerToken{start: j, end: k, text: def[j:k], ident: true}, true
			default:
				return headerToken{start: j, end: j + size, text: def[j : j+size]}, true
			}
		}
	}
	return headerToken{}, false
}

// ============================================================
// View
// ============================================================

// ScriptView returns the CREATE VIEW definition as stored in sys.sql_modules,
// followed by the view's indexes (viewIndexes).
func (sc *Scripter) ScriptView(ctx context.Context, schema, name string) (string, error) {
	if err := requireSchema("script view", schema, name); err != nil {
		return "", err
	}
	return sc.scriptModule(ctx, moduleView, schema, name, func(sb *strings.Builder, objectID int) error {
		// The index reads are keyed by object_id alone, so a Table carrying
		// the view's serves them; it goes no further than this call.
		indexes, err := (&Table{db: sc.db, Schema: schema, Name: name, ObjectID: objectID}).Indexes(ctx)
		if err != nil {
			return err
		}
		sb.WriteString(viewIndexes(indexes, qualifiedName(schema, name), sc.opts))
		return nil
	})
}

// viewIndexes renders an indexed view's indexes, as ScriptTable renders a
// table's and each guarded the same way: the unique clustered index first,
// which every other index on a view needs, then the nonclustered ones, then
// the disabling of any disabled one. Without them a DROP AND CREATE turns an
// indexed view into a plain one, and every query relying on its
// materialisation silently regresses. A view's statistics are not scripted,
// as a table's are not.
//
// Under ScriptAlter they follow too, after a comment saying why: ALTER VIEW
// drops every index on the view.
func viewIndexes(indexes []*Index, fullName string, opts ScriptOptions) string {
	if len(indexes) == 0 {
		return ""
	}
	ordered := slices.Clone(indexes)
	slices.SortStableFunc(ordered, func(a, b *Index) int {
		switch {
		case a.IsClustered == b.IsClustered:
			return 0
		case a.IsClustered:
			return -1
		}
		return 1
	})
	var sb strings.Builder
	if opts.verb() == ScriptAlter {
		sb.WriteString("-- ALTER VIEW drops every index on the view; these recreate them.\n")
	}
	// The view's data space is its clustered index's: a nonclustered index
	// with no ON clause lands there (indexDataSpaceClause).
	var p tableScriptParts
	if ordered[0].IsClustered {
		p.ds = ordered[0].DataSpace
	}
	writeTableDependents(&sb, ordered, nil, p, fullName, opts)
	return sb.String()
}

// ============================================================
// Stored Procedure
// ============================================================

// ScriptStoredProcedure returns the CREATE PROCEDURE definition.
func (sc *Scripter) ScriptStoredProcedure(ctx context.Context, schema, name string) (string, error) {
	if err := requireSchema("script stored procedure", schema, name); err != nil {
		return "", err
	}
	return sc.scriptModule(ctx, moduleProcedure, schema, name, nil)
}

// ============================================================
// Function
// ============================================================

// ScriptFunction returns the CREATE FUNCTION definition.
func (sc *Scripter) ScriptFunction(ctx context.Context, schema, name string) (string, error) {
	if err := requireSchema("script function", schema, name); err != nil {
		return "", err
	}
	return sc.scriptModule(ctx, moduleFunction, schema, name, nil)
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
	script, err := sc.scriptModule(ctx, moduleTrigger, schema, name, nil)
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
WHERE  SCHEMA_NAME(o.schema_id) COLLATE DATABASE_DEFAULT = @p1 AND tr.name = @p2
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
// database-scope DDL trigger, with its extended properties
// (ScriptOptions.ExtendedProperties).
//
// scriptModule is not reusable here: it addresses a module by schema and name,
// and a DDL trigger has no schema.
func (sc *Scripter) ScriptDatabaseTrigger(ctx context.Context, name string) (string, error) {
	t, err := sc.db.DatabaseTriggerByName(ctx, name)
	if err != nil {
		return "", err
	}
	if t.Definition == "" && sc.opts.verb() != ScriptDrop {
		// A CLR trigger has no stored definition; its CREATE is rebuilt from
		// the catalog and scripted as if it were the stored one. t is this
		// call's own read, so filling in its Definition changes no caller's.
		def, err := sc.db.clrDatabaseTrigger(ctx, name)
		if err != nil {
			return "", fmt.Errorf("gosmo: script database trigger %q: %w", name, err)
		}
		t.Definition = def
	}
	script, err := buildDatabaseTriggerScript(t, sc.opts)
	if err != nil {
		return "", err
	}
	return sc.withExtendedPropertiesAt(ctx, script, fmt.Sprintf("database trigger %q", t.Name),
		1, t.ObjectID, ExtendedPropertyLevel{Level0Type: "TRIGGER", Level0Name: t.Name})
}

// buildDatabaseTriggerScript assembles one database trigger's script from the
// definition sys.sql_modules stores.
//
// A trigger with no readable definition — encrypted; a CLR one's is rebuilt by
// ScriptDatabaseTrigger — is an error rather than an empty CREATE half: emitting
// nothing produces a script that drops the trigger and does not put it back.
// IncludeIfNotExists is not honoured because CREATE TRIGGER must be the first
// statement in its batch, the same reason scriptModule ignores it. Both are
// buildServerTriggerScript's reasoning, unchanged; only the scope clause
// differs.
//
// The definition is preceded by the SET options it was created under, as
// every module's is (moduleSetOptions) — except a CLR trigger's, which has
// no T-SQL to compile.
func buildDatabaseTriggerScript(t *DatabaseTrigger, opts ScriptOptions) (string, error) {
	drop := fmt.Sprintf("DROP TRIGGER IF EXISTS %s ON DATABASE;\nGO\n", quoteIdent(t.Name))
	return opts.envelopeErr(drop, "", func(sb *strings.Builder) error {
		if strings.TrimSpace(t.Definition) == "" {
			return unsupportedf("gosmo: script database trigger %q: definition is not readable (encrypted)", t.Name)
		}
		def := t.Definition
		if opts.verb() == ScriptAlter {
			def = alterModuleDefinition(def)
		}
		if !t.IsCLR {
			sb.WriteString(moduleSetOptions(t.UsesANSINulls, t.UsesQuotedIdentifier))
		}
		sb.WriteString(def)
		sb.WriteString("\nGO\n")
		if !t.IsEnabled {
			fmt.Fprintf(sb, "DISABLE TRIGGER %s ON DATABASE;\nGO\n", quoteIdent(t.Name))
		}
		return nil
	})
}
