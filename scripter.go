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

// ScriptDatabase generates the database's script under the options' Verb:
// CREATE DATABASE with its recovery model and compatibility level, the DROP
// DATABASE that removes it, or both. ScriptAlter falls back to the CREATE,
// as it does for every object without an ALTER that restates it.
//
// The CREATE carries the containment, the file layout — every filegroup
// with its files under ON PRIMARY/FILEGROUP, CONTAINS FILESTREAM or
// MEMORY_OPTIMIZED_DATA, the default filegroup, and the log under LOG ON —
// the collation and WITH FILESTREAM's non-transacted access and directory
// name. The recovery model, compatibility level, read-only filegroups, Query
// Store and the owner always follow as ALTER DATABASE / ALTER AUTHORIZATION;
// AUTO_CLOSE, AUTO_SHRINK, AUTO_CREATE_STATISTICS, AUTO_UPDATE_STATISTICS(
// _ASYNC), PAGE_VERIFY, TRUSTWORTHY, READ_COMMITTED_SNAPSHOT,
// ALLOW_SNAPSHOT_ISOLATION and change tracking follow only where they differ
// from what CREATE DATABASE gives (see writeDatabaseSettings). The physical
// paths and FILESTREAM directory name are this database's own, so a replay
// on the same instance needs them, or the name, edited first.
//
// The layout is read from the database itself (Database.FileGroups and
// Files), so a database that cannot be entered — OFFLINE, RESTORING, or one
// the login has no user in — fails the script rather than silently
// scripting a database with default paths and only PRIMARY. A DROP alone
// reads nothing.
//
// The context is not decoration. Alone among the Script* methods this one
// renders from the Database's own cached metadata rather than querying, and a
// Database from Server.DatabaseRef(name) carries none — it is a bare handle
// by design. Rendering that handle emitted "SET RECOVERY ;" and
// "COMPATIBILITY_LEVEL = 0", neither of which is valid T-SQL, so a handle with
// no recovery model is refilled from sys.databases first — unless the script
// is a DROP alone, which needs only the name. Each line is still guarded on
// its own value: a refresh that cannot run leaves the script short a setting,
// which is recoverable, rather than syntactically broken, which is not.
func (sc *Scripter) ScriptDatabase(ctx context.Context) (string, error) {
	d := sc.db
	if sc.opts.verb() != ScriptDrop && (d.RecoveryModel == "" || d.CompatibilityLevel == 0) {
		full, err := d.server.DatabaseByName(ctx, d.Name)
		if err != nil {
			return "", fmt.Errorf("gosmo: script database %q: %w", d.Name, err)
		}
		d = full
	}
	var layout *databaseLayout
	if sc.opts.verb() != ScriptDrop {
		var err error
		if layout, err = readDatabaseLayout(ctx, d); err != nil {
			return "", fmt.Errorf("gosmo: script database %q: %w", d.Name, err)
		}
	}
	return sc.scriptDatabaseFrom(d, layout)
}

// databaseLayout is what the CREATE DATABASE and the statements after it are
// built from beyond the Database's own fields: the filegroups with their
// files, the log files, which belong to none, and the database's options,
// change tracking and Query Store settings. A nil settings field scripts
// none of its statements.
type databaseLayout struct {
	groups []*FileGroup
	logs   []*DatabaseFileInfo

	opts *DatabaseOptions
	ct   *ChangeTrackingInfo
	qs   *QueryStoreInfo
}

func readDatabaseLayout(ctx context.Context, d *Database) (*databaseLayout, error) {
	groups, err := d.FileGroups(ctx)
	if err != nil {
		return nil, err
	}
	files, err := d.Files(ctx)
	if err != nil {
		return nil, err
	}
	l := &databaseLayout{groups: groups}
	for _, f := range files {
		if f.Type == "LOG" {
			l.logs = append(l.logs, f)
		}
	}
	if l.opts, err = d.Options(ctx); err != nil {
		return nil, err
	}
	if l.ct, err = d.ChangeTracking(ctx); err != nil {
		return nil, err
	}
	if l.qs, err = d.QueryStore(ctx); err != nil {
		return nil, err
	}
	return l, nil
}

// fileGroupContains is the CONTAINS keyword CREATE DATABASE needs for a
// filegroup's type; "" for an ordinary ROWS filegroup.
func fileGroupContains(fgType string) string {
	switch fgType {
	case "FILESTREAM_DATA_FILEGROUP":
		return " CONTAINS FILESTREAM"
	case "MEMORY_OPTIMIZED_DATA_FILEGROUP":
		return " CONTAINS MEMORY_OPTIMIZED_DATA"
	}
	return ""
}

// scriptFileSpec renders one file of CREATE DATABASE's ON/LOG ON list.
//
// A FILESTREAM or memory-optimized container is a directory: SIZE and
// FILEGROWTH do not apply to one, and only a
// limited MAXSIZE means anything, UNLIMITED being the default. An ordinary
// file with growth 0 has growth disabled, which has to be said: leaving
// FILEGROWTH out gives the instance's default of 64 MB.
func scriptFileSpec(f *DatabaseFileInfo, container bool) string {
	spec := DatabaseFileSpec{Name: f.Name, Path: f.PhysicalName}
	if container {
		if f.MaxSizeKB > 0 {
			spec.MaxSizeKB = f.MaxSizeKB
		}
	} else {
		spec.SizeKB, spec.MaxSizeKB = f.SizeKB, f.MaxSizeKB
		spec.GrowthKB, spec.GrowthPercent = f.GrowthKB, f.GrowthPercent
		spec.DisableGrowth = f.GrowthKB == 0 && f.GrowthPercent == 0
	}
	return buildFileDefClause(spec)
}

// writeDatabaseFileClauses writes " ON PRIMARY … LOG ON …" for l: PRIMARY
// first, as CREATE DATABASE requires (its first file is the primary file),
// then every other filegroup that has files, in the order FileGroups read
// them. A filegroup with no files cannot appear in CREATE DATABASE — the
// syntax needs at least one file — so the caller adds those afterwards.
func writeDatabaseFileClauses(sb *strings.Builder, l *databaseLayout) {
	var primary *FileGroup
	for _, g := range l.groups {
		if g.Name == "PRIMARY" {
			primary = g
		}
	}
	if primary == nil || len(primary.Files) == 0 {
		// Without a primary file there is nothing CREATE DATABASE can be
		// given; a log alone is refused, so leave the layout to the server.
		return
	}
	sep := "\n ON PRIMARY\n"
	writeGroup := func(g *FileGroup) {
		container := g.Type != "ROWS_FILEGROUP" && g.Type != ""
		for _, f := range g.Files {
			sb.WriteString(sep)
			sb.WriteString(scriptFileSpec(f, container))
			sep = ",\n"
		}
	}
	writeGroup(primary)
	for _, g := range l.groups {
		if g == primary || len(g.Files) == 0 {
			continue
		}
		fmt.Fprintf(sb, ",\n FILEGROUP %s%s", quoteIdent(g.Name), fileGroupContains(g.Type))
		// DEFAULT is per filegroup type: the one FILESTREAM filegroup always
		// reports it, and saying so is harmless. A memory-optimized
		// filegroup cannot take it.
		if g.IsDefault && g.Type != "MEMORY_OPTIMIZED_DATA_FILEGROUP" {
			sb.WriteString(" DEFAULT")
		}
		sep = "\n"
		writeGroup(g)
	}
	sep = "\n LOG ON\n"
	for _, f := range l.logs {
		sb.WriteString(sep)
		sb.WriteString(scriptFileSpec(f, false))
		sep = ",\n"
	}
}

// scriptDatabaseFrom renders the script from d's metadata and layout, with
// no reads of its own. A nil layout scripts no file clauses.
//
// The DROP switches to master first: DROP DATABASE fails on the database the
// session is in, and a script of a database is most often run from it. With
// IncludeIfNotExists it is guarded by DB_ID, as the CREATE is.
func (sc *Scripter) scriptDatabaseFrom(d *Database, layout *databaseLayout) (string, error) {
	var sb strings.Builder
	if sc.opts.IncludeHeaders {
		// info is nil on a Server built without NewServer; the header reports
		// no version rather than panicking.
		version := ""
		if d.server != nil && d.server.info != nil {
			version = d.server.info.ProductVersion
		}
		fmt.Fprintf(&sb, "/* Database: %s  Version: %s */\n\n", blockCommentSafe(d.Name), blockCommentSafe(version))
	}
	drop := "USE [master];\nGO\n"
	if sc.opts.IncludeIfNotExists {
		drop += fmt.Sprintf("IF DB_ID(N'%s') IS NOT NULL\n    ", escapeSingle(d.Name))
	}
	drop += fmt.Sprintf("DROP DATABASE %s;\nGO\n", quoteIdent(d.Name))
	sb.WriteString(sc.opts.envelope(drop, "", func(sb *strings.Builder) {
		if sc.opts.IncludeIfNotExists {
			fmt.Fprintf(sb, "IF DB_ID(N'%s') IS NULL\nBEGIN\n    ", escapeSingle(d.Name))
		}
		fmt.Fprintf(sb, "CREATE DATABASE %s", quoteIdent(d.Name))
		// CONTAINMENT = PARTIAL needs the server's "contained database
		// authentication" on; a replay where it is off fails here, loudly,
		// rather than creating an uncontained database.
		if layout != nil && layout.opts != nil && layout.opts.Containment == "PARTIAL" {
			sb.WriteString(" CONTAINMENT = PARTIAL")
		}
		if layout != nil {
			writeDatabaseFileClauses(sb, layout)
		}
		if d.Collation != "" {
			fmt.Fprintf(sb, " COLLATE %s", d.Collation)
		}
		if layout != nil && layout.opts != nil {
			writeFileStreamOptions(sb, layout.opts)
		}
		sb.WriteString(";\n")
		if sc.opts.IncludeIfNotExists {
			sb.WriteString("END\nGO\n\n")
		} else {
			sb.WriteString("GO\n\n")
		}
		if d.RecoveryModel != "" {
			fmt.Fprintf(sb, "ALTER DATABASE %s SET RECOVERY %s;\nGO\n",
				quoteIdent(d.Name), d.RecoveryModel)
		}
		if d.CompatibilityLevel != 0 {
			fmt.Fprintf(sb, "ALTER DATABASE %s SET COMPATIBILITY_LEVEL = %d;\nGO\n",
				quoteIdent(d.Name), d.CompatibilityLevel)
		}
		if layout != nil {
			for _, g := range layout.groups {
				if g.Name == "PRIMARY" {
					continue
				}
				if len(g.Files) == 0 {
					fmt.Fprintf(sb, "ALTER DATABASE %s ADD FILEGROUP %s%s;\nGO\n",
						quoteIdent(d.Name), quoteIdent(g.Name), fileGroupContains(g.Type))
				}
			}
			for _, g := range layout.groups {
				// Read-only cannot be said in CREATE DATABASE. PRIMARY can be
				// read-only too, but only on a read-only database, which is
				// not scripted.
				if g.IsReadOnly && g.Name != "PRIMARY" {
					fmt.Fprintf(sb, "ALTER DATABASE %s MODIFY FILEGROUP %s READ_ONLY;\nGO\n",
						quoteIdent(d.Name), quoteIdent(g.Name))
				}
			}
			writeDatabaseSettings(sb, d.Name, layout)
		}
	}))
	return sb.String(), nil
}

// writeFileStreamOptions writes CREATE DATABASE's " WITH FILESTREAM (…)" when
// either option differs from a new database's (access OFF, no directory). It
// belongs to the CREATE: SET FILESTREAM after it would need the database to
// itself, which a script cannot promise.
func writeFileStreamOptions(sb *strings.Builder, o *DatabaseOptions) {
	var parts []string
	switch o.NonTransactedAccess {
	case "READ_ONLY", "FULL":
		parts = append(parts, "NON_TRANSACTED_ACCESS = "+o.NonTransactedAccess)
	}
	if o.DirectoryName != "" {
		parts = append(parts, "DIRECTORY_NAME = "+QuoteLiteral(o.DirectoryName))
	}
	if len(parts) > 0 {
		fmt.Fprintf(sb, " WITH FILESTREAM (%s)", strings.Join(parts, ", "))
	}
}

// writeDatabaseSettings writes the ALTER DATABASE … SET statements for the
// options CREATE DATABASE cannot carry, and the owner.
//
// An option is written only where it differs from what CREATE DATABASE gives
// on an instance whose model is as shipped — AUTO_CLOSE and AUTO_SHRINK off,
// statistics created and updated synchronously, PAGE_VERIFY CHECKSUM, neither
// snapshot option, TRUSTWORTHY off (always, whatever model says), no change
// tracking — so a script stays short. Two are written whatever their value:
//
//   - Query Store, because a new database's default depends on the instance
//     the script runs on — off before SQL Server 2022, READ_WRITE from it —
//     so a 2019 database replayed on 2022 would silently gain it. Its
//     settings default by version too, so ON restates every one.
//   - The owner, because a new database belongs to whoever runs the script.
//     An owner SID that maps to no login cannot be restated, and says so.
//
// Every keyword spliced in is checked against the values the catalog
// reports; anything else is left out rather than written.
func writeDatabaseSettings(sb *strings.Builder, name string, l *databaseLayout) {
	set := func(clause string) {
		fmt.Fprintf(sb, "ALTER DATABASE %s SET %s;\nGO\n", quoteIdent(name), clause)
	}
	if o := l.opts; o != nil {
		onOff := func(opt string, v, def bool) {
			if v == def {
				return
			}
			if v {
				set(opt + " ON")
			} else {
				set(opt + " OFF")
			}
		}
		onOff("AUTO_CLOSE", o.AutoClose, false)
		onOff("AUTO_SHRINK", o.AutoShrink, false)
		onOff("AUTO_CREATE_STATISTICS", o.AutoCreateStats, true)
		onOff("AUTO_UPDATE_STATISTICS", o.AutoUpdateStats, true)
		onOff("AUTO_UPDATE_STATISTICS_ASYNC", o.AutoUpdateStatsAsync, false)
		switch o.PageVerify {
		case "TORN_PAGE_DETECTION", "NONE":
			set("PAGE_VERIFY " + o.PageVerify)
		}
		onOff("TRUSTWORTHY", o.IsTrustworthy, false)
		// A database the script has just created has no other session in
		// it, so READ_COMMITTED_SNAPSHOT needs no termination clause.
		onOff("READ_COMMITTED_SNAPSHOT", o.ReadCommittedSnapshot, false)
		// IN_TRANSITION_TO_ON settles at ON once the open transactions end.
		onOff("ALLOW_SNAPSHOT_ISOLATION",
			o.SnapshotIsolation == "ON" || o.SnapshotIsolation == "IN_TRANSITION_TO_ON", false)
	}
	if ct := l.ct; ct != nil && ct.Enabled && changeTrackingRetentionUnits[ct.RetentionUnit] {
		cleanup := "OFF"
		if ct.AutoCleanup {
			cleanup = "ON"
		}
		set(fmt.Sprintf("CHANGE_TRACKING = ON (CHANGE_RETENTION = %d %s, AUTO_CLEANUP = %s)",
			ct.RetentionPeriod, ct.RetentionUnit, cleanup))
	}
	if qs := l.qs; qs != nil {
		if qs.DesiredState == QueryStoreOff {
			set("QUERY_STORE = OFF")
		} else if withs, err := queryStoreOnOptions(QueryStoreOptions{
			DesiredState:              qs.DesiredState,
			MaxStorageMB:              qs.MaxStorageMB,
			CaptureMode:               qs.CaptureMode,
			SizeCleanupMode:           qs.SizeCleanupMode,
			StaleThresholdDays:        qs.StaleThresholdDays,
			FlushIntervalSec:          qs.FlushIntervalSec,
			IntervalMinutes:           qs.IntervalMinutes,
			MaxPlansPerQuery:          qs.MaxPlansPerQuery,
			WaitStatsCaptureMode:      qs.WaitStatsCaptureMode,
			CapturePolicyExecCount:    qs.CapturePolicyExecCount,
			CapturePolicyCompileCPUMs: qs.CapturePolicyCompileCPUMs,
			CapturePolicyExecCPUMs:    qs.CapturePolicyExecCPUMs,
			CapturePolicyStaleHours:   qs.CapturePolicyStaleHours,
			// "" is what SQL Server 2016 reports: it has no such setting.
		}, qs.WaitStatsCaptureMode != ""); err == nil {
			set("QUERY_STORE = ON (" + withs + ")")
		} else {
			sb.WriteString("-- Query Store reports a setting with no SET form: QUERY_STORE not scripted.\n")
		}
	}
	if o := l.opts; o != nil {
		if o.Owner != "" {
			fmt.Fprintf(sb, "ALTER AUTHORIZATION ON DATABASE::%s TO %s;\nGO\n", quoteIdent(name), quoteIdent(o.Owner))
		} else {
			sb.WriteString("-- The owner's SID maps to no login: ALTER AUTHORIZATION not scripted.\n")
		}
	}
}
