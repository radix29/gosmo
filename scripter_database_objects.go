package gosmo

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
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
	ScriptedTrigger             ScriptedKind = "trigger"
	ScriptedDatabaseTrigger     ScriptedKind = "database trigger"
	ScriptedPermissions         ScriptedKind = "permissions"
)

// ScriptedObject is one object's script within a database script.
type ScriptedObject struct {
	Kind   ScriptedKind
	Schema string // "" for an object that has none
	Name   string
	// Table is the table a foreign key or trigger belongs to.
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
}

// DatabaseObjectScript is the result of ScriptDatabaseObjects: the objects in
// creation order and what the ordering had to decide.
type DatabaseObjectScript struct {
	Objects []ScriptedObject
	// Warnings lists each dependency cycle that was broken, one reference
	// each ("dependency cycle broken: a -> b").
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
//     parameters use, ties by kind, schema and name;
//  3. foreign keys (ALTER TABLE … ADD CONSTRAINT), DML triggers, database
//     triggers;
//  4. with IncludePermissions, the permissions.
//
// A cycle among references SQL Server resolves late is broken and reported in
// Warnings; schema-bound references are never broken. The Scripter's verb
// must be ScriptCreate. Not scripted: system objects, a system-versioned
// table's history table (its parent creates it), rules and defaults, CLR
// assemblies, full-text and Service Broker objects, external objects,
// credentials, certificates and keys, and any dependency across databases. An
// encrypted module, or one the login has no VIEW DEFINITION on, is listed as
// a comment.
func (sc *Scripter) ScriptDatabaseObjectList(ctx context.Context, o ObjectScriptOptions) (*DatabaseObjectScript, error) {
	if sc.opts.verb() != ScriptCreate {
		return nil, invalidf("gosmo: script database objects: only the CREATE verb is supported")
	}
	d := sc.db
	opts := sc.opts
	opts.DeferForeignKeys = true
	s := &Scripter{db: d, opts: opts}
	want := func(k ScriptedKind) bool { return o.Kinds == nil || slices.Contains(o.Kinds, k) }

	res := &DatabaseObjectScript{}
	add := func(obj ScriptedObject) { res.Objects = append(res.Objects, obj) }
	// emit scripts one object; a definition that cannot be read becomes a
	// comment rather than failing the whole database.
	emit := func(obj ScriptedObject, gen func() (string, error)) error {
		sql, err := gen()
		if err != nil {
			if !errors.Is(err, ErrUnsupported) {
				return err
			}
			obj.Note = err.Error()
			sql = "-- not scripted: " + strings.ReplaceAll(err.Error(), "\n", " ") + "\n"
		}
		obj.SQL = sql
		add(obj)
		return nil
	}

	// 1. principals and schemas, partitioning.
	var sec *databaseSecurity
	if o.IncludePermissions && (want(ScriptedPrincipals) || want(ScriptedPermissions)) {
		var err error
		if sec, err = s.readDatabaseSecurity(ctx); err != nil {
			return nil, err
		}
		head, _ := buildDatabaseSecurityParts(d.Name, sec)
		if want(ScriptedPrincipals) {
			add(ScriptedObject{Kind: ScriptedPrincipals, Name: d.Name, SQL: head})
		}
	} else if want(ScriptedSchema) && !o.IncludePermissions {
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
			add(ScriptedObject{Kind: ScriptedSchema, Name: sch.Name, SQL: buildSchemaScript(&c, opts)})
		}
	}
	if want(ScriptedPartitionFunction) {
		pfs, err := d.PartitionFunctions(ctx)
		if err != nil {
			return nil, err
		}
		for _, pf := range pfs {
			pf := pf
			if err := emit(ScriptedObject{Kind: ScriptedPartitionFunction, Name: pf.Name},
				func() (string, error) { return s.ScriptPartitionFunction(ctx, pf.Name) }); err != nil {
				return nil, err
			}
		}
	}
	if want(ScriptedPartitionScheme) {
		pss, err := d.PartitionSchemes(ctx)
		if err != nil {
			return nil, err
		}
		for _, ps := range pss {
			ps := ps
			if err := emit(ScriptedObject{Kind: ScriptedPartitionScheme, Name: ps.Name},
				func() (string, error) { return s.ScriptPartitionScheme(ctx, ps.Name) }); err != nil {
				return nil, err
			}
		}
	}

	// 2. the dependency-ordered objects.
	type item struct {
		node orderNode
		kind ScriptedKind
		gen  func() (string, error)
	}
	items := map[string]item{}
	var nodes []orderNode
	put := func(prefix byte, rank int, kind ScriptedKind, schema, name string, gen func() (string, error)) {
		n := orderNode{key: objectKey(prefix, schema, name), rank: rank, schema: schema, name: name}
		nodes = append(nodes, n)
		items[n.key] = item{n, kind, gen}
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
	history, err := d.historyTables(ctx)
	if err != nil {
		return nil, err
	}
	tables, err := d.Tables(ctx, TableQuery{})
	if err != nil {
		return nil, err
	}
	var scriptedTables []*Table
	for _, t := range tables {
		if history[objectKey('o', t.Schema, t.Name)] {
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
	}
	views, err := d.Views(ctx, ObjectFilter{})
	if err != nil {
		return nil, err
	}
	for _, v := range views {
		put('o', 6, ScriptedView, v.Schema, v.Name, func() (string, error) { return s.ScriptView(ctx, v.Schema, v.Name) })
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

	edges, err := d.dependencyOrderEdges(ctx)
	if err != nil {
		return nil, err
	}
	order, warnings, err := orderObjects(nodes, edges)
	if err != nil {
		return nil, err
	}
	res.Warnings = warnings
	for _, n := range order {
		it := items[n.key]
		if !want(it.kind) {
			continue
		}
		if err := emit(ScriptedObject{Kind: it.kind, Schema: n.schema, Name: n.name}, it.gen); err != nil {
			return nil, err
		}
	}

	// 3. what needs every table: foreign keys, then triggers.
	if want(ScriptedForeignKey) {
		for _, t := range scriptedTables {
			fks, err := t.ForeignKeys(ctx)
			if err != nil {
				return nil, err
			}
			for _, fk := range fks {
				if err := emit(ScriptedObject{Kind: ScriptedForeignKey, Schema: t.Schema, Name: fk.Name, Table: t.Name},
					func() (string, error) { return s.ScriptForeignKey(ctx, t.Schema, t.Name, fk.Name) }); err != nil {
					return nil, err
				}
			}
		}
	}
	if want(ScriptedTrigger) {
		trs, err := d.Triggers(ctx)
		if err != nil {
			return nil, err
		}
		for _, tr := range trs {
			if err := emit(ScriptedObject{Kind: ScriptedTrigger, Schema: tr.Schema, Name: tr.Name, Table: tr.TableName},
				func() (string, error) { return s.ScriptTrigger(ctx, tr.Schema, tr.Name) }); err != nil {
				return nil, err
			}
		}
	}
	if want(ScriptedDatabaseTrigger) {
		trs, err := d.DatabaseTriggers(ctx)
		if err != nil {
			return nil, err
		}
		for _, tr := range trs {
			if err := emit(ScriptedObject{Kind: ScriptedDatabaseTrigger, Name: tr.Name},
				func() (string, error) { return s.ScriptDatabaseTrigger(ctx, tr.Name) }); err != nil {
				return nil, err
			}
		}
	}

	// 4. permissions.
	if sec != nil && want(ScriptedPermissions) {
		_, tail := buildDatabaseSecurityParts(d.Name, sec)
		add(ScriptedObject{Kind: ScriptedPermissions, Name: d.Name, SQL: tail})
	}
	return res, nil
}

// objectKey identifies a node: prefix o for sys.objects, t for a type, x for
// an XML schema collection.
func objectKey(prefix byte, schema, name string) string {
	return string(prefix) + ":" + schema + "." + name
}

// historyTables is the system-versioned tables' history tables, by objectKey:
// the parent's CREATE TABLE names one and creates it when absent.
func (d *Database) historyTables(ctx context.Context) (map[string]bool, error) {
	rows, err := d.query(ctx, `
SELECT SCHEMA_NAME(schema_id), name FROM sys.tables WHERE temporal_type = 1`)
	list, err := scanRows(rows, err, fmt.Sprintf("list history tables in %q", d.Name), func(scan func(...any) error) (string, error) {
		var s, n string
		err := scan(&s, &n)
		return objectKey('o', s, n), err
	})
	if err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(list))
	for _, k := range list {
		out[k] = true
	}
	return out, nil
}

// dependencyOrderEdges reads every reference one object's creation needs from
// another, within the database: the catalog's expression dependencies (a
// default or check constraint counts for its table), and the types and XML
// schema collections columns and parameters use.
func (d *Database) dependencyOrderEdges(ctx context.Context) ([]orderEdge, error) {
	const q = `
SELECT 'o', SCHEMA_NAME(ro.schema_id), ro.name, x.k, x.s, x.n, x.b
FROM (
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
    WHERE  sed.referenced_class = 10 AND c.schema_id <> 4
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
