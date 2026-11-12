package gosmo

import "strings"

// ============================================================
// Scripter  (mirrors Microsoft.SqlServer.Management.Smo.Scripter)
// ============================================================

// ScriptVerb selects which statement form a Scripter emits.
type ScriptVerb int

const (
	// ScriptCreate emits the object's CREATE statement (the zero value).
	ScriptCreate ScriptVerb = iota
	// ScriptDrop emits its DROP statement.
	ScriptDrop
	// ScriptDropAndCreate emits the DROP followed by the CREATE, in that
	// order and in separate batches — the re-runnable form.
	ScriptDropAndCreate
	// ScriptAlter emits ALTER instead of CREATE. Only module objects
	// (views, stored procedures, functions, triggers) have an ALTER form
	// that restates the whole object; everything else falls back to CREATE.
	ScriptAlter
)

// ScriptOptions controls how objects are scripted.
type ScriptOptions struct {
	// Verb selects CREATE (the default), DROP, DROP-and-CREATE, or ALTER.
	Verb ScriptVerb
	// IncludeHeaders adds an informational header comment. Applies to
	// ScriptTable and ScriptDatabase only.
	IncludeHeaders bool
	// IncludeIfNotExists guards each generated statement with its own
	// existence check. ScriptView/StoredProcedure/Function return the
	// module's definition verbatim from sys.sql_modules, unguarded, and
	// guard only what follows it: a view's indexes and the extended
	// properties.
	//
	// The guard is per statement, never a block spanning several. A BEGIN
	// block containing GO separators is split across batches — GO is a
	// client-side batch break — leaving an unclosed BEGIN in one batch and a
	// bare END in another, which is a script that cannot parse.
	IncludeIfNotExists bool
	// ScriptDrops is the older, narrower spelling of Verb = ScriptDrop, and
	// still honoured: it applies only while Verb is left at its zero value.
	// New code should set Verb.
	ScriptDrops bool
	// SchemaQualify prefixes object names with their schema.
	SchemaQualify bool
	// AnsiPadding emits SET ANSI_PADDING ON before CREATE TABLE.
	AnsiPadding bool
	// ExtendedProperties restates the extended properties of what the script
	// creates — EXEC sys.sp_addextendedproperty after the object, for it and
	// its columns, parameters, indexes and (a table's) constraints, as SSMS's
	// "Script extended properties" option does, on by default there too.
	// Applies to ScriptTable, ScriptView, ScriptStoredProcedure,
	// ScriptFunction, ScriptTrigger, ScriptSchema, ScriptDatabase (its own
	// and its filegroups' and files', not a snapshot's) and every other
	// single-object script whose object can carry one — all but a broker
	// priority; never to a DROP.
	ExtendedProperties bool
	// DeferReferences leaves what a table references other tables through —
	// its foreign keys, with their extended properties, and (an edge
	// table's) edge constraints — out of its script, for a caller emitting them
	// afterwards as ALTER TABLE … ADD CONSTRAINT, once every table exists
	// (see ScriptDatabaseObjectList). Applies to ScriptTable only.
	DeferReferences bool
}

// DefaultScriptOptions returns sensible defaults.
func DefaultScriptOptions() ScriptOptions {
	return ScriptOptions{
		IncludeHeaders:     true,
		SchemaQualify:      true,
		IncludeIfNotExists: true,
		AnsiPadding:        true,
		ExtendedProperties: true,
	}
}

// verb resolves which statement form to emit, folding the older
// ScriptDrops bool into the Verb it stands for. Verb wins whenever it is set
// to anything but its zero value, so a caller that sets both is not silently
// given the drop.
func (o ScriptOptions) verb() ScriptVerb {
	if o.Verb == ScriptCreate && o.ScriptDrops {
		return ScriptDrop
	}
	return o.Verb
}

// envelope is the verb handling every single-object builder shares. Under
// DROP or DROP AND CREATE it writes drop, and under DROP it stops there.
// Otherwise it writes guard when IncludeIfNotExists is set — the existence
// test that has to sit directly before the CREATE statement — and then
// whatever create writes. A builder whose guard must follow something else it
// writes first, such as a note about what cannot be scripted, passes "" and
// writes its own.
//
// The builders held 47 hand-written copies of this preamble until 2026-09-24.
func (o ScriptOptions) envelope(drop, guard string, create func(sb *strings.Builder)) string {
	s, _ := o.envelopeErr(drop, guard, func(sb *strings.Builder) error {
		create(sb)
		return nil
	})
	return s
}

// envelopeErr is envelope for a builder that can refuse. An error from
// create discards everything, the DROP included: a DROP AND CREATE whose
// CREATE half cannot be written must not come back as a script that drops the
// object and does not put it back.
func (o ScriptOptions) envelopeErr(drop, guard string, create func(sb *strings.Builder) error) (string, error) {
	var sb strings.Builder
	if v := o.verb(); v == ScriptDrop || v == ScriptDropAndCreate {
		sb.WriteString(drop)
		if v == ScriptDrop {
			return sb.String(), nil
		}
		sb.WriteString("\n")
	}
	if guard != "" && o.IncludeIfNotExists {
		sb.WriteString(guard)
	}
	if err := create(&sb); err != nil {
		return "", err
	}
	return sb.String(), nil
}

// Scripter generates T-SQL DDL scripts for objects in a database.
type Scripter struct {
	db   *Database
	opts ScriptOptions
	// tables, when set, holds every table's script inputs already read — see
	// tablePrefetch. Only ScriptDatabaseObjectList sets it.
	tables *tablePrefetch
}

// Database returns the database the scripter scripts.
func (sc *Scripter) Database() *Database { return sc.db }

// DatabaseContext is the batch that puts a script into the scripter's
// database: "USE [db];" and a GO, as SSMS opens every script of a
// database-scoped object. The Script* methods leave it out, so a caller
// joining several scripts writes it once; one handing a script to a reader —
// a file, the clipboard — prefixes it, or the script runs in whatever database
// the reader's session is in, and a DROP drops that database's namesake.
//
// ScriptDatabase needs none: it switches to master itself.
func (sc *Scripter) DatabaseContext() string { return scriptUse(sc.db.Name) }

// NewScripter creates a Scripter for the given database.
func NewScripter(db *Database, opts ScriptOptions) *Scripter {
	return &Scripter{db: db, opts: opts}
}
