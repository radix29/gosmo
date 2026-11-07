package gosmo

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/radix29/gosmo/tsql"
)

// ============================================================
// Scripter — every object of a database, in dependency order
// ============================================================

// ScriptedKind names the kind of one ScriptedObject.
type ScriptedKind string

const (
	ScriptedPrincipals          ScriptedKind = "principals" // owner, roles, users, schemas, memberships
	ScriptedSchema              ScriptedKind = "schema"
	ScriptedPartitionFunction   ScriptedKind = "partition function"
	ScriptedPartitionScheme     ScriptedKind = "partition scheme"
	ScriptedXMLSchemaCollection ScriptedKind = "xml schema collection"
	ScriptedType                ScriptedKind = "type"
	ScriptedTableType           ScriptedKind = "table type"
	ScriptedSequence            ScriptedKind = "sequence"
	ScriptedTable               ScriptedKind = "table"
	ScriptedFunction            ScriptedKind = "function"
	ScriptedView                ScriptedKind = "view"
	ScriptedProcedure           ScriptedKind = "procedure"
	ScriptedSynonym             ScriptedKind = "synonym"
	ScriptedForeignKey          ScriptedKind = "foreign key"
	ScriptedEdgeConstraint      ScriptedKind = "edge constraint"
	ScriptedTrigger             ScriptedKind = "trigger"
	ScriptedDatabaseTrigger     ScriptedKind = "database trigger"
	ScriptedPermissions         ScriptedKind = "permissions"
)

// ScriptedObject is one object's script within a database script.
type ScriptedObject struct {
	Kind   ScriptedKind
	Schema string // "" for an object that has none
	Name   string
	// Table is the table a foreign key, edge constraint or trigger belongs
	// to.
	Table string
	// SQL is the object's script, or — when the object cannot be scripted
	// (an encrypted module, a definition the login cannot see) — one comment
	// saying so; Note then carries the reason.
	SQL  string
	Note string
}

// ObjectScriptOptions selects what ScriptDatabaseObjects scripts.
type ObjectScriptOptions struct {
	// Kinds restricts the output to these kinds; nil is every kind. An
	// excluded kind still takes part in the ordering of the others.
	Kinds []ScriptedKind
	// IncludePermissions adds the database owner, roles, users, role
	// memberships and every GRANT/DENY (ScriptDatabaseSecurity's content),
	// the principals first and the permissions last. Without it the custom
	// schemas are still created, as their objects need them, but under dbo:
	// their owner may be a principal the script does not create.
	IncludePermissions bool
	// Progress, when set, is called once every list has been read, with done
	// 0 and the number of ScriptedObjects the script will hold, then after
	// each one is scripted, ending at done == total. Every list is read
	// before the first object is scripted, so total does not change; the
	// objects themselves are what takes the time — at least one round trip
	// each. It is called on the caller's goroutine.
	Progress func(done, total int)
}

// DatabaseObjectScript is the result of ScriptDatabaseObjects: the objects in
// creation order and what the ordering had to decide.
type DatabaseObjectScript struct {
	Objects []ScriptedObject
	// Warnings lists a degraded dependency source first (no VIEW
	// DEFINITION on the database: "dependency order read from
	// sys.sql_dependencies …"), then each dependency cycle that was broken,
	// one reference each ("dependency cycle broken: a -> b"), and each
	// scripted procedure,
	// function or synonym naming an object the script leaves out, which SQL
	// Server resolves only when it runs ("… references …, which is not
	// scripted").
	Warnings []string
}

// String joins the objects into one script: the database context, the
// warnings as comments, then every object's script.
func (r *DatabaseObjectScript) String() string {
	var sb strings.Builder
	for _, w := range r.Warnings {
		fmt.Fprintf(&sb, "-- %s\n", strings.ReplaceAll(w, "\n", " "))
	}
	for _, o := range r.Objects {
		sb.WriteString(o.SQL)
		if !strings.HasSuffix(o.SQL, "\n") {
			sb.WriteString("\n")
		}
	}
	return sb.String()
}

// ScriptDatabaseObjects scripts every user object of the database as one
// string, in an order a replay into an empty database accepts. See
// ScriptDatabaseObjectList for the order and what is left out.
func (sc *Scripter) ScriptDatabaseObjects(ctx context.Context, o ObjectScriptOptions) (string, error) {
	r, err := sc.ScriptDatabaseObjectList(ctx, o)
	if err != nil {
		return "", err
	}
	return sc.DatabaseContext() + r.String(), nil
}

// ScriptDatabaseObjectList scripts every user object of the database, one
// ScriptedObject each, in creation order:
//
//  1. schemas (with IncludePermissions, the principals around them),
//     partition functions and schemes;
//  2. XML schema collections, types, table types, sequences, tables (without
//     their foreign keys), functions, views, procedures and synonyms, sorted
//     by sys.sql_expression_dependencies and the types columns and
//     parameters use, ties by kind, schema and name. Without VIEW
//     DEFINITION on the database that view lists nothing (or is refused),
//     so the order is read from sys.sql_dependencies instead, which misses
//     a reference to an object created after the module naming it; a
//     Warning says so;
//  3. foreign keys, then edge constraints (ALTER TABLE … ADD CONSTRAINT,
//     once every table exists: the catalog records no dependency from an
//     edge table to its node tables), DML triggers, database triggers;
//  4. with IncludePermissions, the permissions.
//
// A cycle among references SQL Server resolves late is broken and reported in
// Warnings; schema-bound references are never broken. An object that is
// left out — an encrypted module, or one whose definition the login cannot
// see, is listed as a comment with its Note set — is followed by a note for
// everything whose creation needs it, transitively: a foreign key or edge
// constraint naming its table, a trigger on it, a table, view, table type or
// inline function selecting from or typed by it, and any schema-bound
// reference. A GRANT on it becomes a comment in the permissions. A
// procedure, multi-statement or scalar function or synonym naming it is
// still scripted (SQL Server resolves the name when it runs), with a
// Warning. The Scripter's verb
// must be ScriptCreate. Not scripted: system objects, a system-versioned
// or ledger table's history table and ledger view (its parent creates them),
// rules and defaults, CLR
// assemblies, full-text and Service Broker objects, external objects,
// credentials, certificates and keys, and any dependency across databases.
func (sc *Scripter) ScriptDatabaseObjectList(ctx context.Context, o ObjectScriptOptions) (*DatabaseObjectScript, error) {
	if sc.opts.verb() != ScriptCreate {
		return nil, invalidf("gosmo: script database objects: only the CREATE verb is supported")
	}
	d := sc.db
	opts := sc.opts
	opts.DeferReferences = true
	s := &Scripter{db: d, opts: opts}
	want := func(k ScriptedKind) bool { return o.Kinds == nil || slices.Contains(o.Kinds, k) }

	// Every list is read first and every object queued in script order, so
	// the total is known before the first object is scripted (Progress).
	var queue []pendingObject
	queue1 := func(obj ScriptedObject, gen func() (string, error)) {
		queue = append(queue, pendingObject{obj: obj, gen: gen})
	}
	sqlOf := func(sql string) func() (string, error) { return func() (string, error) { return sql, nil } }

	// 1. principals and schemas, partitioning.
	var sec *databaseSecurity
	if o.IncludePermissions && (want(ScriptedPrincipals) || want(ScriptedPermissions)) {
		var err error
		if sec, err = s.readDatabaseSecurity(ctx); err != nil {
			return nil, err
		}
		head, _ := buildDatabaseSecurityParts(d.Name, sec, nil)
		if want(ScriptedPrincipals) {
			queue1(ScriptedObject{Kind: ScriptedPrincipals, Name: d.Name}, sqlOf(head))
		}
	}
	// The principals block creates the schemas with their owners; without it
	// (no IncludePermissions, or ScriptedPrincipals excluded by Kinds) they
	// are created bare, under dbo.
	if (sec == nil || !want(ScriptedPrincipals)) && want(ScriptedSchema) {
		schemas, err := d.Schemas(ctx)
		if err != nil {
			return nil, err
		}
		for _, sch := range schemas {
			if sch.ID <= 4 || sch.ID >= 16384 {
				continue
			}
			c := *sch
			c.Owner = "dbo"
			opts := s.opts
			opts.IncludeIfNotExists = true
			queue1(ScriptedObject{Kind: ScriptedSchema, Name: sch.Name}, sqlOf(buildSchemaScript(&c, opts)))
		}
	}
	if want(ScriptedPartitionFunction) {
		pfs, err := d.PartitionFunctions(ctx)
		if err != nil {
			return nil, err
		}
		for _, pf := range pfs {
			queue1(ScriptedObject{Kind: ScriptedPartitionFunction, Name: pf.Name},
				func() (string, error) { return s.ScriptPartitionFunction(ctx, pf.Name) })
		}
	}
	if want(ScriptedPartitionScheme) {
		pss, err := d.PartitionSchemes(ctx)
		if err != nil {
			return nil, err
		}
		for _, ps := range pss {
			queue1(ScriptedObject{Kind: ScriptedPartitionScheme, Name: ps.Name},
				func() (string, error) { return s.ScriptPartitionScheme(ctx, ps.Name) })
		}
	}

	// 2. the dependency-ordered objects.
	type item struct {
		node orderNode
		kind ScriptedKind
		gen  func() (string, error)
		// late marks an object whose CREATE does not resolve the objects
		// its body names (deferred name resolution): a procedure, a
		// multi-statement or scalar function, a synonym.
		late bool
	}
	items := map[string]item{}
	var nodes []orderNode
	put := func(prefix byte, rank int, kind ScriptedKind, schema, name string, gen func() (string, error)) {
		n := orderNode{key: objectKey(prefix, schema, name), rank: rank, schema: schema, name: name}
		nodes = append(nodes, n)
		late := kind == ScriptedProcedure || kind == ScriptedSynonym
		items[n.key] = item{n, kind, gen, late}
	}
	xmls, err := d.XMLSchemaCollections(ctx)
	if err != nil {
		return nil, err
	}
	for _, x := range xmls {
		put('x', 0, ScriptedXMLSchemaCollection, x.Schema, x.Name, func() (string, error) { return s.ScriptXMLSchemaCollection(ctx, x.Schema, x.Name) })
	}
	types, err := d.UserDefinedDataTypes(ctx)
	if err != nil {
		return nil, err
	}
	for _, t := range types {
		put('t', 1, ScriptedType, t.Schema, t.Name, func() (string, error) { return s.ScriptUserDefinedDataType(ctx, t.Schema, t.Name) })
	}
	ttypes, err := d.UserDefinedTableTypes(ctx)
	if err != nil {
		return nil, err
	}
	for _, t := range ttypes {
		put('t', 2, ScriptedTableType, t.Schema, t.Name, func() (string, error) { return s.ScriptUserDefinedTableType(ctx, t.Schema, t.Name) })
	}
	seqs, err := d.Sequences(ctx)
	if err != nil {
		return nil, err
	}
	for _, q := range seqs {
		put('o', 3, ScriptedSequence, q.Schema, q.Name, func() (string, error) { return s.ScriptSequence(ctx, q.Schema, q.Name) })
	}
	byParent, err := d.createdByParent(ctx)
	if err != nil {
		return nil, err
	}
	tables, err := d.Tables(ctx, TableQuery{})
	if err != nil {
		return nil, err
	}
	var scriptedTables []*Table
	for _, t := range tables {
		if _, ok := byParent[objectKey('o', t.Schema, t.Name)]; ok {
			continue
		}
		scriptedTables = append(scriptedTables, t)
		put('o', 4, ScriptedTable, t.Schema, t.Name, func() (string, error) { return s.scriptTable(ctx, t) })
	}
	if s.tables, err = d.prefetchTables(ctx, scriptedTables); err != nil {
		return nil, err
	}
	funcs, err := d.UserDefinedFunctions(ctx, ObjectFilter{})
	if err != nil {
		return nil, err
	}
	for _, f := range funcs {
		put('o', 5, ScriptedFunction, f.Schema, f.Name, func() (string, error) { return s.ScriptFunction(ctx, f.Schema, f.Name) })
		if f.FuncType != FunctionTypeInlineTable {
			// An inline function's SELECT is compiled by its CREATE, like
			// a view's; the other forms' bodies resolve when they run.
			k := objectKey('o', f.Schema, f.Name)
			it := items[k]
			it.late = true
			items[k] = it
		}
	}
	views, err := d.Views(ctx, ObjectFilter{})
	if err != nil {
		return nil, err
	}
	for _, v := range views {
		gen := func() (string, error) { return s.ScriptView(ctx, v.Schema, v.Name) }
		switch dropped, ok := byParent[objectKey('o', v.Schema, v.Name)]; {
		case ok && dropped:
			// Its dropped ledger table gets a refusal note; a CREATE VIEW
			// would select from a table the replay never creates.
			gen = func() (string, error) {
				return "", unsupportedf("gosmo: script view %s: a dropped ledger view cannot be scripted", qualifiedName(v.Schema, v.Name))
			}
		case ok:
			continue
		}
		put('o', 6, ScriptedView, v.Schema, v.Name, gen)
	}
	procs, err := d.StoredProcedures(ctx, ObjectFilter{})
	if err != nil {
		return nil, err
	}
	for _, p := range procs {
		put('o', 7, ScriptedProcedure, p.Schema, p.Name, func() (string, error) { return s.ScriptStoredProcedure(ctx, p.Schema, p.Name) })
	}
	syns, err := d.Synonyms(ctx)
	if err != nil {
		return nil, err
	}
	for _, y := range syns {
		put('o', 8, ScriptedSynonym, y.Schema, y.Name, func() (string, error) { return s.ScriptSynonym(ctx, y.Schema, y.Name) })
	}

	edges, sourceWarning, err := readOrderEdges(ctx,
		func(ctx context.Context) (bool, error) { return d.HasPermission(ctx, "VIEW DEFINITION") },
		func(ctx context.Context) ([]orderEdge, error) {
			return d.dependencyOrderEdges(ctx, expressionDependencyRefs)
		},
		func(ctx context.Context) ([]orderEdge, error) {
			return d.dependencyOrderEdges(ctx, legacyDependencyRefs)
		})
	if err != nil {
		return nil, err
	}
	order, warnings, err := orderObjects(nodes, edges)
	if err != nil {
		return nil, err
	}
	res := &DatabaseObjectScript{Warnings: warnings}
	if sourceWarning != "" {
		res.Warnings = append([]string{sourceWarning}, warnings...)
	}
	// Every reference a CREATE resolves is a need: a schema-bound one, any
	// from a table, view, table type or inline function, and any to a type
	// or XML schema collection (a parameter's type is resolved too). A late
	// object's other references are only warned about.
	needs, lateRefs := map[string][]string{}, map[string][]string{}
	for _, e := range edges {
		it, ok := items[e.from]
		if !ok || e.from == e.to {
			continue
		}
		if e.bound || !it.late || e.to[0] != 'o' {
			needs[e.from] = append(needs[e.from], e.to)
		} else {
			lateRefs[e.from] = append(lateRefs[e.from], e.to)
		}
	}
	for _, n := range order {
		it := items[n.key]
		if want(it.kind) {
			queue = append(queue, pendingObject{obj: ScriptedObject{Kind: it.kind, Schema: n.schema, Name: n.name},
				gen: it.gen, key: n.key, needs: needs[n.key], late: lateRefs[n.key]})
		}
	}

	// 3. what needs every table: foreign keys, edge constraints, then
	// triggers. The keys and edge constraints come from the table prefetch:
	// a ScriptForeignKey each re-read the table and its keys, 1 + 2×keys
	// round trips per table.
	if want(ScriptedForeignKey) {
		for _, t := range scriptedTables {
			fks, err := s.tableForeignKeys(ctx, t)
			if err != nil {
				return nil, err
			}
			for _, fk := range fks {
				queue = append(queue, pendingObject{
					obj:   ScriptedObject{Kind: ScriptedForeignKey, Schema: t.Schema, Name: fk.Name, Table: t.Name},
					gen:   func() (string, error) { return s.scriptTableForeignKey(ctx, t, fk) },
					key:   objectKey('o', t.Schema, fk.Name),
					needs: []string{objectKey('o', t.Schema, t.Name), objectKey('o', fk.ReferencedSchema, fk.ReferencedTable)},
				})
			}
		}
	}
	if want(ScriptedEdgeConstraint) {
		for _, t := range scriptedTables {
			if !t.IsEdge {
				continue
			}
			ecs, err := s.tableEdgeConstraints(ctx, t)
			if err != nil {
				return nil, err
			}
			for _, ec := range ecs {
				ecNeeds := []string{objectKey('o', t.Schema, t.Name)}
				for _, c := range ec.Clauses {
					ecNeeds = append(ecNeeds, objectKey('o', c.FromSchema, c.FromTable), objectKey('o', c.ToSchema, c.ToTable))
				}
				queue = append(queue, pendingObject{
					obj: ScriptedObject{Kind: ScriptedEdgeConstraint, Schema: t.Schema, Name: ec.Name, Table: t.Name},
					// No extended properties: an edge constraint cannot
					// carry one (see withTableExtendedProperties).
					gen:   sqlOf(scriptEdgeConstraint(ec, qualifiedName(t.Schema, t.Name), s.opts)),
					key:   objectKey('o', t.Schema, ec.Name),
					needs: ecNeeds,
				})
			}
		}
	}
	if want(ScriptedTrigger) {
		trs, err := d.Triggers(ctx)
		if err != nil {
			return nil, err
		}
		for _, tr := range trs {
			queue = append(queue, pendingObject{
				obj:   ScriptedObject{Kind: ScriptedTrigger, Schema: tr.Schema, Name: tr.Name, Table: tr.TableName},
				gen:   func() (string, error) { return s.ScriptTrigger(ctx, tr.Schema, tr.Name) },
				key:   objectKey('o', tr.Schema, tr.Name),
				needs: []string{objectKey('o', tr.Schema, tr.TableName)},
			})
		}
	}
	if want(ScriptedDatabaseTrigger) {
		trs, err := d.DatabaseTriggers(ctx)
		if err != nil {
			return nil, err
		}
		for _, tr := range trs {
			queue1(ScriptedObject{Kind: ScriptedDatabaseTrigger, Name: tr.Name},
				func() (string, error) { return s.ScriptDatabaseTrigger(ctx, tr.Name) })
		}
	}

	// missing is every object the script leaves out, by objectKey: the
	// dropped ledger objects nothing creates, then whatever scriptQueue notes.
	missing := map[string]bool{}
	for k, dropped := range byParent {
		if dropped {
			missing[k] = true
		}
	}

	// 4. permissions, built at their turn: a GRANT on an object left out
	// would fail the replay (Msg 15151), so it becomes a comment.
	if sec != nil && want(ScriptedPermissions) {
		queue1(ScriptedObject{Kind: ScriptedPermissions, Name: d.Name}, func() (string, error) {
			_, tail := buildDatabaseSecurityParts(d.Name, sec, missing)
			return tail, nil
		})
	}

	var lateWarnings []string
	if res.Objects, lateWarnings, err = scriptQueue(ctx, queue, missing, o.Progress); err != nil {
		return nil, err
	}
	res.Warnings = append(res.Warnings, lateWarnings...)
	return res, nil
}

// pendingObject is one object of a database script, queued with the call
// that scripts it.
type pendingObject struct {
	obj ScriptedObject
	gen func() (string, error)
	// key is the objectKey of what obj creates, "" when nothing can need it.
	key string
	// needs is the objectKeys obj cannot be created without; late is those
	// it names but SQL Server resolves only when it runs.
	needs, late []string
}

// scriptQueue scripts each queued object in order, reporting to progress (if
// set) first with 0 and then after each object. An object that cannot be
// scripted (ErrUnsupported: an encrypted module, a definition the login
// cannot see) becomes a comment with its Note set, rather than failing the
// whole database; any other error, and a done ctx, ends the script.
//
// missing (nil for none) holds the objectKeys of what the script leaves out.
// scriptQueue adds each noted object's key, and an object needing a missing
// one is noted without being scripted, its key added in turn: the queue is
// in dependency order, so one pass carries the cascade. A scripted object
// with a late reference to a missing one returns a warning.
func scriptQueue(ctx context.Context, queue []pendingObject, missing map[string]bool, progress func(done, total int)) ([]ScriptedObject, []string, error) {
	if progress == nil {
		progress = func(int, int) {}
	}
	if missing == nil {
		missing = map[string]bool{}
	}
	out := make([]ScriptedObject, 0, len(queue))
	var warnings []string
	progress(0, len(queue))
	for _, p := range queue {
		if err := ctx.Err(); err != nil {
			return nil, nil, fmt.Errorf("gosmo: script database objects: %w", err)
		}
		obj := p.obj
		var sql string
		var err error
		if i := slices.IndexFunc(p.needs, func(k string) bool { return missing[k] }); i >= 0 {
			err = unsupportedf("gosmo: script %s %s: needs %s, which is not scripted", obj.Kind, obj.display(), p.needs[i][2:])
		} else {
			sql, err = p.gen()
		}
		if err != nil {
			if !errors.Is(err, ErrUnsupported) {
				return nil, nil, err
			}
			obj.Note = err.Error()
			sql = "-- not scripted: " + tsql.CommentSafe(err.Error()) + "\n"
			if p.key != "" {
				missing[p.key] = true
			}
		} else {
			for _, k := range p.late {
				if missing[k] {
					warnings = append(warnings, fmt.Sprintf("%s %s references %s, which is not scripted", obj.Kind, obj.display(), k[2:]))
				}
			}
		}
		obj.SQL = sql
		out = append(out, obj)
		progress(len(out), len(queue))
	}
	return out, warnings, nil
}

// display is o's name as a script shows it: schema-qualified where it has a
// schema, and for a foreign key, edge constraint or trigger with its table.
func (o ScriptedObject) display() string {
	if o.Schema == "" {
		return quoteIdent(o.Name)
	}
	n := qualifiedName(o.Schema, o.Name)
	if o.Table != "" {
		n += " on " + qualifiedName(o.Schema, o.Table)
	}
	return n
}

// objectKey identifies a node: prefix o for sys.objects, t for a type, x for
// an XML schema collection. The parts are bracket-quoted so the key is
// injective: joined bare, [a.b].[c] and [a].[b.c] both gave "o:a.b.c", and the
// script created one twice and the other never.
func objectKey(prefix byte, schema, name string) string {
	return string(prefix) + ":" + qualifiedName(schema, name)
}

// createdByParent is what another table's CREATE TABLE creates, by objectKey:
// a system-versioned table's history table (temporal_type 1), an updatable
// ledger table's history table (ledger_type 1 — its temporal_type is 0) and a
// ledger table's ledger view. Scripted on their own, the history table was a
// "not scripted" note and the ledger view a CREATE VIEW the replay refused
// (Msg 2714: its table had already created it). The value is true for a
// dropped ledger table's ledger view and history table: nothing creates them
// (the view is noted, not skipped), so nothing in the script may need them.
func (d *Database) createdByParent(ctx context.Context) (map[string]bool, error) {
	rows, err := d.query(ctx, d.createdByParentSelect())
	type row struct {
		key     string
		dropped bool
	}
	list, err := scanRows(rows, err, fmt.Sprintf("list history tables and ledger views in %q", d.Name), func(scan func(...any) error) (row, error) {
		var s, n string
		var dropped bool
		err := scan(&s, &n, &dropped)
		return row{objectKey('o', s, n), dropped}, err
	})
	if err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(list))
	for _, r := range list {
		out[r.key] = r.dropped
	}
	return out, nil
}

// createdByParentSelect is createdByParent's query. Ledger tables, and the
// three columns naming them, are SQL Server 2022's.
func (d *Database) createdByParentSelect() string {
	major := d.serverMajorVersion()
	return `
SELECT SCHEMA_NAME(t.schema_id), t.name,
       CAST(CASE WHEN EXISTS (SELECT 1 FROM sys.tables p
                              WHERE p.history_table_id = t.object_id AND ` + bitSince(major, SQLServer2022, "p.is_dropped_ledger_table") + ` = 1)
                 THEN 1 ELSE 0 END AS bit)
FROM   sys.tables t
WHERE  t.temporal_type = 1 OR ` + colSince(major, SQLServer2022, "t.ledger_type", "CAST(0 AS tinyint)") + ` = 1
UNION ALL
SELECT SCHEMA_NAME(v.schema_id), v.name, ` + bitSince(major, SQLServer2022, "v.is_dropped_ledger_view") + `
FROM   sys.views v
WHERE  v.object_id IN (SELECT ` + colSince(major, SQLServer2022, "t.ledger_view_id", "CAST(NULL AS int)") + ` FROM sys.tables t)`
}

// legacyDependencyWarning is the Warning a script whose order came from
// sys.sql_dependencies carries.
const legacyDependencyWarning = "dependency order read from sys.sql_dependencies (no VIEW DEFINITION on the database): " +
	"a reference to an object created after the module naming it is not seen"

// readOrderEdges picks the source of the dependency order. Without VIEW
// DEFINITION on the database, sys.sql_expression_dependencies returns no rows
// and no error (db_datareader with VIEW DEFINITION on a schema), and without
// SELECT on it, which public lacks, it is refused with Msg 229 (VIEW
// DEFINITION alone). Either way the same login reads sys.sql_dependencies,
// the deprecated view SQL Server still maintains (13 through 17), and the
// result says so in a warning rather than ordering by name in silence.
func readOrderEdges(ctx context.Context, viewDefinition func(context.Context) (bool, error),
	expression, legacy func(context.Context) ([]orderEdge, error)) ([]orderEdge, string, error) {
	held, err := viewDefinition(ctx)
	if err != nil {
		return nil, "", err
	}
	if held {
		edges, err := expression(ctx)
		if err == nil || !IsPermissionDenied(err) {
			return edges, "", err
		}
	}
	edges, err := legacy(ctx)
	if err != nil {
		return nil, "", err
	}
	return edges, legacyDependencyWarning, nil
}

// expressionDependencyRefs is dependencyOrderEdges's object, type and XML
// schema collection references from sys.sql_expression_dependencies.
const expressionDependencyRefs = `
    SELECT sed.referencing_id AS rid, 'o' AS k, SCHEMA_NAME(o.schema_id) AS s, o.name AS n,
           CAST(sed.is_schema_bound_reference AS int) AS b
    FROM   sys.sql_expression_dependencies sed
    JOIN   sys.objects o ON o.object_id = sed.referenced_id
    WHERE  sed.referenced_class = 1 AND sed.referenced_database_name IS NULL AND o.is_ms_shipped = 0
    UNION ALL
    SELECT sed.referencing_id, 't', SCHEMA_NAME(t.schema_id), t.name, CAST(sed.is_schema_bound_reference AS int)
    FROM   sys.sql_expression_dependencies sed
    JOIN   sys.types t ON t.user_type_id = sed.referenced_id
    WHERE  sed.referenced_class = 6 AND t.is_user_defined = 1
    UNION ALL
    SELECT sed.referencing_id, 'x', SCHEMA_NAME(c.schema_id), c.name, CAST(sed.is_schema_bound_reference AS int)
    FROM   sys.sql_expression_dependencies sed
    JOIN   sys.xml_schema_collections c ON c.xml_collection_id = sed.referenced_id
    WHERE  sed.referenced_class = 10 AND c.schema_id <> 4`

// legacyDependencyRefs is the same from sys.sql_dependencies, one row per
// referenced column. Its classes, checked on 13 and 17 (2026-11-07): 0 an
// object reference, 1 a schema-bound one, 3 an XML schema collection
// (schema-bound, a typed xml parameter's included). It records no alias type
// at all, which the columns and parameters branches cover, and nothing for a
// synonym or for a name that did not exist when the module was created.
const legacyDependencyRefs = `
    SELECT DISTINCT d.object_id AS rid, 'o' AS k, SCHEMA_NAME(o.schema_id) AS s, o.name AS n,
           CAST(d.class AS int) AS b
    FROM   sys.sql_dependencies d
    JOIN   sys.objects o ON o.object_id = d.referenced_major_id
    WHERE  d.class IN (0, 1) AND o.is_ms_shipped = 0
    UNION ALL
    SELECT DISTINCT d.object_id, 'x', SCHEMA_NAME(c.schema_id), c.name, 1
    FROM   sys.sql_dependencies d
    JOIN   sys.xml_schema_collections c ON c.xml_collection_id = d.referenced_major_id
    WHERE  d.class = 3 AND c.schema_id <> 4`

// dependencyOrderEdges reads every reference one object's creation needs from
// another, within the database: refs, the catalog's dependencies
// (expressionDependencyRefs or legacyDependencyRefs; a default or check
// constraint counts for its table), and the types and XML schema
// collections columns and parameters use.
func (d *Database) dependencyOrderEdges(ctx context.Context, refs string) ([]orderEdge, error) {
	q := `
SELECT 'o', SCHEMA_NAME(ro.schema_id), ro.name, x.k, x.s, x.n, x.b
FROM (` + refs + `
    UNION ALL
    SELECT col.object_id, 't', SCHEMA_NAME(t.schema_id), t.name, 0
    FROM   sys.columns col
    JOIN   sys.types t ON t.user_type_id = col.user_type_id
    WHERE  t.is_user_defined = 1 AND t.is_table_type = 0
    UNION ALL
    SELECT col.object_id, 'x', SCHEMA_NAME(c.schema_id), c.name, 0
    FROM   sys.columns col
    JOIN   sys.xml_schema_collections c ON c.xml_collection_id = col.xml_collection_id
    WHERE  c.schema_id <> 4
    UNION ALL
    SELECT p.object_id, 't', SCHEMA_NAME(t.schema_id), t.name, 0
    FROM   sys.parameters p
    JOIN   sys.types t ON t.user_type_id = p.user_type_id
    WHERE  t.is_user_defined = 1
) x
JOIN sys.objects o2 ON o2.object_id = x.rid
JOIN sys.objects ro ON ro.object_id = CASE WHEN o2.type IN ('D', 'C') THEN o2.parent_object_id ELSE o2.object_id END
WHERE ro.is_ms_shipped = 0
UNION ALL
SELECT 't', SCHEMA_NAME(tt.schema_id), tt.name, x.k, x.s, x.n, 0
FROM   sys.table_types tt
JOIN   sys.columns col ON col.object_id = tt.type_table_object_id
CROSS APPLY (
    SELECT 't' AS k, SCHEMA_NAME(t.schema_id) AS s, t.name AS n
    FROM   sys.types t WHERE t.user_type_id = col.user_type_id AND t.is_user_defined = 1 AND t.is_table_type = 0
    UNION ALL
    SELECT 'x', SCHEMA_NAME(c.schema_id), c.name
    FROM   sys.xml_schema_collections c WHERE c.xml_collection_id = col.xml_collection_id AND c.schema_id <> 4
) x`
	rows, err := d.query(ctx, q)
	return scanRows(rows, err, fmt.Sprintf("list dependencies in %q", d.Name), func(scan func(...any) error) (orderEdge, error) {
		var fk, tk, fs, fn, ts, tn string
		var bound int
		if err := scan(&fk, &fs, &fn, &tk, &ts, &tn, &bound); err != nil {
			return orderEdge{}, err
		}
		return orderEdge{from: objectKey(fk[0], fs, fn), to: objectKey(tk[0], ts, tn), bound: bound != 0}, nil
	})
}
