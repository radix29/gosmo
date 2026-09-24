package gosmo

import (
	"context"
	"fmt"
	"strings"
)

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
	// existence check. Applies to ScriptTable and ScriptDatabase only:
	// ScriptView/StoredProcedure/Function return the module's definition
	// verbatim from sys.sql_modules and don't synthesize DDL to guard.
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
}

// DefaultScriptOptions returns sensible defaults.
func DefaultScriptOptions() ScriptOptions {
	return ScriptOptions{
		IncludeHeaders:     true,
		SchemaQualify:      true,
		IncludeIfNotExists: true,
		AnsiPadding:        true,
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
}

// Database returns the database the scripter scripts.
func (sc *Scripter) Database() *Database { return sc.db }

// NewScripter creates a Scripter for the given database.
func NewScripter(db *Database, opts ScriptOptions) *Scripter {
	return &Scripter{db: db, opts: opts}
}

// ============================================================
// Database
// ============================================================

// ScriptDatabase generates a CREATE DATABASE script for the attached database.
//
// The context is not decoration. Alone among the Script* methods this one
// renders from the Database's own cached metadata rather than querying, and a
// Database from Server.DatabaseRef(name) carries none — it is a bare handle
// by design. Rendering that handle emitted "SET RECOVERY ;" and
// "COMPATIBILITY_LEVEL = 0", neither of which is valid T-SQL, so a handle with
// no recovery model is refilled from sys.databases first. Each line is still
// guarded on its own value: a refresh that cannot run leaves the script short
// a setting, which is recoverable, rather than syntactically broken, which is
// not.
func (sc *Scripter) ScriptDatabase(ctx context.Context) (string, error) {
	d := sc.db
	if d.RecoveryModel == "" || d.CompatibilityLevel == 0 {
		full, err := d.server.DatabaseByName(ctx, d.Name)
		if err != nil {
			return "", fmt.Errorf("gosmo: script database %q: %w", d.Name, err)
		}
		d = full
	}
	return sc.scriptDatabaseFrom(d)
}

// scriptDatabaseFrom renders the script from d's metadata, with no reads of
// its own.
func (sc *Scripter) scriptDatabaseFrom(d *Database) (string, error) {
	var sb strings.Builder
	if sc.opts.IncludeHeaders {
		// info is nil on a Server built without NewServer; the header reports
		// no version rather than panicking.
		version := ""
		if d.server != nil && d.server.info != nil {
			version = d.server.info.ProductVersion
		}
		fmt.Fprintf(&sb, "/* Database: %s  Version: %s */\n\n", d.Name, version)
	}
	if sc.opts.IncludeIfNotExists {
		fmt.Fprintf(&sb, "IF DB_ID(N'%s') IS NULL\nBEGIN\n    ", escapeSingle(d.Name))
	}
	fmt.Fprintf(&sb, "CREATE DATABASE %s", quoteIdent(d.Name))
	if d.Collation != "" {
		fmt.Fprintf(&sb, " COLLATE %s", d.Collation)
	}
	sb.WriteString(";\n")
	if sc.opts.IncludeIfNotExists {
		sb.WriteString("END\nGO\n\n")
	} else {
		sb.WriteString("GO\n\n")
	}
	if d.RecoveryModel != "" {
		fmt.Fprintf(&sb, "ALTER DATABASE %s SET RECOVERY %s;\nGO\n",
			quoteIdent(d.Name), d.RecoveryModel)
	}
	if d.CompatibilityLevel != 0 {
		fmt.Fprintf(&sb, "ALTER DATABASE %s SET COMPATIBILITY_LEVEL = %d;\nGO\n",
			quoteIdent(d.Name), d.CompatibilityLevel)
	}
	return sb.String(), nil
}
