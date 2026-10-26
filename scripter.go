package gosmo

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/radix29/gosmo/tsql"
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
	// ScriptFunction, ScriptTrigger, ScriptSchema and ScriptDatabase (its
	// own, not a snapshot's); never to a DROP.
	ExtendedProperties bool
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
// the collation and the WITH list: FILESTREAM's non-transacted access and
// directory name, and LEDGER = ON. The recovery model, compatibility level,
// read-only filegroups, every SET option, change tracking, Query Store, the
// TDE key and SET ENCRYPTION ON, the scoped configurations away from their
// default, the owner, and last the user access and READ_ONLY/READ_WRITE
// follow as ALTER DATABASE / ALTER
// AUTHORIZATION (see writeDatabaseSettings). The physical paths and
// FILESTREAM directory name are this database's own, so a replay on the same
// instance needs them, or the name, edited first.
//
// A database snapshot scripts as CREATE DATABASE … ON (its sparse files) AS
// SNAPSHOT OF [source], with nothing after it: a snapshot takes no options,
// owner or Query Store setting of its own.
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
		// A snapshot's are its source's, and it is read-only.
		if layout.snapshotOf == "" {
			if layout.extProps, err = sc.scriptExtendedPropertiesAt(ctx, 0, 0, ExtendedPropertyLevel{}); err != nil {
				return "", fmt.Errorf("gosmo: script database %q: %w", d.Name, err)
			}
		}
	}
	return sc.scriptDatabaseFrom(d, layout)
}

// databaseLayout is what the CREATE DATABASE and the statements after it are
// built from beyond the Database's own fields: the filegroups with their
// files, the log files, which belong to none, and the database's options,
// change tracking and Query Store settings. A nil settings field scripts
// none of its statements. major is the source server's major version (0 when
// unknown, which scripts every version-gated option).
//
// For a snapshot only snapshotOf (the source database's name) and
// snapshotFiles (its sparse ROWS files) are set.
type databaseLayout struct {
	groups []*FileGroup
	logs   []*DatabaseFileInfo

	opts  *DatabaseOptions
	ct    *ChangeTrackingInfo
	qs    *QueryStoreInfo
	major int
	// collation stands in for Database.Collation when that read came back
	// empty (a database AUTO_CLOSE had closed).
	collation string

	// scoped is every database scoped configuration; writeScopedConfigs
	// picks those away from their default.
	scoped []*DatabaseScopedConfig
	// tde is the database's encryption key; nil when it has none, or when
	// the read was refused, which tdeRefused records for a database with
	// encryption on. tdeOff marks a key SET ENCRYPTION OFF left behind: it
	// is created, but encryption is not turned on.
	tde        *DatabaseEncryptionKey
	tdeRefused bool
	tdeOff     bool

	// pvsFileGroup is the ADR persistent version store's filegroup when ADR
	// is on ("" when unread); pvsRefused records that the read was refused.
	pvsFileGroup string
	pvsRefused   bool

	// extProps is the database's own extended properties, rendered ("" for
	// none, or with ScriptOptions.ExtendedProperties off).
	extProps string

	snapshotOf    string
	snapshotFiles []*DatabaseFileInfo
}

func readDatabaseLayout(ctx context.Context, d *Database) (*databaseLayout, error) {
	if d.IsSnapshot() {
		return readSnapshotLayout(ctx, d)
	}
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
		if f.Type == FileLog {
			l.logs = append(l.logs, f)
		}
	}
	// From inside the database, which keeps it open: one AUTO_CLOSE has
	// closed reports NULL for several options (and its collation) at server
	// scope.
	if l.opts, err = d.readOptions(ctx, d.queryRow); err != nil {
		return nil, err
	}
	if d.Collation == "" {
		if l.collation, err = d.inDatabaseCollation(ctx); err != nil {
			return nil, err
		}
	}
	if l.ct, err = d.ChangeTracking(ctx); err != nil {
		return nil, err
	}
	if l.qs, err = d.QueryStore().Options(ctx); err != nil {
		return nil, err
	}
	if l.scoped, err = d.DatabaseScopedConfigs(ctx); err != nil {
		return nil, err
	}
	// Whether encryption is on or not: SET ENCRYPTION OFF leaves the key
	// behind. The DMV needs VIEW SERVER STATE; a refusal is noted only when
	// encryption is on, so a caller without it scripts an unencrypted
	// database exactly as before, with no comment about a key it may not
	// have.
	switch l.tde, err = d.EncryptionKey(ctx); {
	case IsPermissionDenied(err):
		l.tde, l.tdeRefused = nil, l.opts.IsEncrypted
	case errors.Is(err, ErrNotFound):
		// No key; or is_encrypted is set but the key is gone — nothing to
		// restate either way.
	case err != nil:
		return nil, err
	}
	l.tdeOff = l.tde != nil && !l.opts.IsEncrypted
	l.major = d.server.serverMajorVersion()
	// Only with ADR on: turned on without the clause, it moves the version
	// store back to PRIMARY, so a filegroup kept from before it was turned
	// off has nothing to restate.
	if l.opts.AcceleratedDatabaseRecovery && majorAtLeast(l.major, SQLServer2019) {
		switch l.pvsFileGroup, err = d.PersistentVersionStoreFileGroup(ctx); {
		case IsPermissionDenied(err):
			l.pvsRefused = true
		case err != nil:
			return nil, err
		}
	}
	return l, nil
}

// inDatabaseCollation reads the database's collation from inside it, which
// opens a database AUTO_CLOSE has closed: sys.databases reports NULL for it
// until then.
func (d *Database) inDatabaseCollation(ctx context.Context) (string, error) {
	var c string
	err := d.queryRow(ctx, func(row *sql.Row) error { return row.Scan(&c) },
		"SELECT ISNULL(CONVERT(sysname, DATABASEPROPERTYEX(DB_NAME(), 'Collation')), '')")
	return c, err
}

// readSnapshotLayout reads what a snapshot's CREATE needs: its source's name
// and its sparse files, from sys.master_files — the snapshot's own logical
// names are the source's, which is what AS SNAPSHOT OF matches on.
func readSnapshotLayout(ctx context.Context, d *Database) (*databaseLayout, error) {
	snap, err := d.server.DatabaseSnapshotByName(ctx, d.Name)
	if err != nil {
		return nil, err
	}
	if snap.SourceDatabase == "" {
		return nil, notFoundf("gosmo: the source of snapshot %q is not visible", d.Name)
	}
	files, err := d.server.DatabaseFiles(ctx, d.Name)
	if err != nil {
		return nil, err
	}
	l := &databaseLayout{snapshotOf: snap.SourceDatabase}
	for _, f := range files {
		if f.Type == FileRows {
			l.snapshotFiles = append(l.snapshotFiles, f)
		}
	}
	if len(l.snapshotFiles) == 0 {
		return nil, notFoundf("gosmo: snapshot %q has no visible data files", d.Name)
	}
	return l, nil
}

// fileGroupContains is the CONTAINS keyword CREATE DATABASE needs for a
// filegroup's type; "" for an ordinary ROWS filegroup.
func fileGroupContains(fgType FileGroupType) string {
	switch fgType {
	case FileStreamFileGroup:
		return " CONTAINS FILESTREAM"
	case MemoryOptimizedFileGroup:
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
		container := g.Type != RowsFileGroup && g.Type != ""
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
		if g.IsDefault && g.Type != MemoryOptimizedFileGroup {
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
		fmt.Fprintf(&sb, "/* Database: %s  Version: %s */\n\n", tsql.BlockCommentSafe(d.Name), tsql.BlockCommentSafe(version))
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
		if layout != nil && layout.snapshotOf != "" {
			for i, f := range layout.snapshotFiles {
				sep := ",\n"
				if i == 0 {
					sep = " ON\n"
				}
				fmt.Fprintf(sb, "%s( NAME = %s, FILENAME = %s )", sep, quoteIdent(f.Name), QuoteLiteral(f.PhysicalName))
			}
			fmt.Fprintf(sb, "\nAS SNAPSHOT OF %s;\n", quoteIdent(layout.snapshotOf))
			if sc.opts.IncludeIfNotExists {
				sb.WriteString("END\nGO\n")
			} else {
				sb.WriteString("GO\n")
			}
			return
		}
		// CONTAINMENT = PARTIAL needs the server's "contained database
		// authentication" on; a replay where it is off fails here, loudly,
		// rather than creating an uncontained database.
		if layout != nil && layout.opts != nil && layout.opts.Containment == "PARTIAL" {
			sb.WriteString(" CONTAINMENT = PARTIAL")
		}
		if layout != nil {
			writeDatabaseFileClauses(sb, layout)
		}
		collation := d.Collation
		if collation == "" && layout != nil {
			collation = layout.collation
		}
		if collation != "" {
			fmt.Fprintf(sb, " COLLATE %s", collation)
		}
		if layout != nil && layout.opts != nil {
			writeCreateWithOptions(sb, layout.opts)
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
				// read-only too, but only on a read-only database, whose
				// READ_ONLY comes last (writeDatabaseSettings).
				if g.IsReadOnly && g.Name != "PRIMARY" {
					fmt.Fprintf(sb, "ALTER DATABASE %s MODIFY FILEGROUP %s READ_ONLY;\nGO\n",
						quoteIdent(d.Name), quoteIdent(g.Name))
				}
			}
			writeDatabaseSettings(sb, d, layout)
		}
	}))
	return sb.String(), nil
}

// writeCreateWithOptions writes CREATE DATABASE's WITH list: FILESTREAM (…)
// when either option differs from a new database's (access OFF, no
// directory), and LEDGER = ON for a ledger database. Both belong to the
// CREATE: SET FILESTREAM after it would need the database to itself, which a
// script cannot promise, and LEDGER has no SET form at all.
func writeCreateWithOptions(sb *strings.Builder, o *DatabaseOptions) {
	var with, fs []string
	switch o.NonTransactedAccess {
	case "READ_ONLY", "FULL":
		fs = append(fs, "NON_TRANSACTED_ACCESS = "+o.NonTransactedAccess)
	}
	if o.DirectoryName != "" {
		fs = append(fs, "DIRECTORY_NAME = "+QuoteLiteral(o.DirectoryName))
	}
	if len(fs) > 0 {
		with = append(with, "FILESTREAM ("+strings.Join(fs, ", ")+")")
	}
	if o.IsLedger {
		with = append(with, "LEDGER = ON")
	}
	if len(with) > 0 {
		sb.WriteString(" WITH " + strings.Join(with, ", "))
	}
}

// writeDatabaseSettings writes the ALTER DATABASE … SET statements for the
// options CREATE DATABASE cannot carry, and the owner.
//
// Every option is written whatever its value, as SSMS does: a new database
// takes its options from the replaying instance's model, so an option left
// out because it matched the shipped default would silently take whatever
// that model says. Change tracking is the exception — written only when on,
// since it is off on every new database and SET CHANGE_TRACKING = OFF on one
// is refused. Also written whatever their value:
//
//   - Query Store, because a new database's default depends on the instance
//     the script runs on — off before SQL Server 2022, READ_WRITE from it —
//     so a 2019 database replayed on 2022 would silently gain it. Its
//     settings default by version too, so ON restates every one.
//   - The owner, because a new database belongs to whoever runs the script.
//     An owner SID that maps to no login cannot be restated, and says so.
//
// The TDE key and scoped configurations follow Query Store (see
// writeInDatabaseSettings). The user access and READ_ONLY/READ_WRITE come
// last, after the owner:
// READ_ONLY first would refuse every ALTER after it, and SINGLE_USER would
// let the first other session in the database lock the script out.
// ACCELERATED_DATABASE_RECOVERY is written only for a source of SQL Server
// 2019 or later (l.major), since an older one has no such option to restate;
// when on, with its version store's filegroup if that is not PRIMARY.
//
// Every keyword spliced in is checked against the values the catalog
// reports; anything else is left out rather than written.
func writeDatabaseSettings(sb *strings.Builder, d *Database, l *databaseLayout) {
	name := d.Name
	set := func(clause string) {
		fmt.Fprintf(sb, "ALTER DATABASE %s SET %s;\nGO\n", quoteIdent(name), clause)
	}
	onOff := func(opt string, v bool) {
		if v {
			set(opt + " ON")
		} else {
			set(opt + " OFF")
		}
	}
	o := l.opts
	if o != nil {
		onOff("ANSI_NULL_DEFAULT", o.ANSINullDefault)
		onOff("ANSI_NULLS", o.ANSINulls)
		onOff("ANSI_PADDING", o.ANSIPadding)
		onOff("ANSI_WARNINGS", o.ANSIWarnings)
		onOff("ARITHABORT", o.ArithAbort)
		onOff("AUTO_CLOSE", o.AutoClose)
		onOff("AUTO_SHRINK", o.AutoShrink)
		onOff("AUTO_CREATE_STATISTICS", o.AutoCreateStats)
		onOff("AUTO_UPDATE_STATISTICS", o.AutoUpdateStats)
		onOff("AUTO_UPDATE_STATISTICS_ASYNC", o.AutoUpdateStatsAsync)
		onOff("CURSOR_CLOSE_ON_COMMIT", o.CursorCloseOnCommit)
		switch o.DefaultCursor {
		case "LOCAL", "GLOBAL":
			set("CURSOR_DEFAULT " + o.DefaultCursor)
		}
		onOff("CONCAT_NULL_YIELDS_NULL", o.ConcatNullYieldsNull)
		onOff("NUMERIC_ROUNDABORT", o.NumericRoundAbort)
		onOff("QUOTED_IDENTIFIER", o.QuotedIdentifier)
		onOff("RECURSIVE_TRIGGERS", o.RecursiveTriggers)
		if o.IsBrokerEnabled {
			set("ENABLE_BROKER")
		} else {
			set("DISABLE_BROKER")
		}
		onOff("DATE_CORRELATION_OPTIMIZATION", o.DateCorrelation)
		onOff("TRUSTWORTHY", o.IsTrustworthy)
		// IN_TRANSITION_TO_ON settles at ON once the open transactions end.
		onOff("ALLOW_SNAPSHOT_ISOLATION",
			o.SnapshotIsolation == "ON" || o.SnapshotIsolation == "IN_TRANSITION_TO_ON")
		if o.ForcedParameterization {
			set("PARAMETERIZATION FORCED")
		} else {
			set("PARAMETERIZATION SIMPLE")
		}
		// A database the script has just created has no other session in
		// it, so READ_COMMITTED_SNAPSHOT needs no termination clause.
		onOff("READ_COMMITTED_SNAPSHOT", o.ReadCommittedSnapshot)
		onOff("HONOR_BROKER_PRIORITY", o.HonorBrokerPriority)
		switch o.PageVerify {
		case "CHECKSUM", "TORN_PAGE_DETECTION", "NONE":
			set("PAGE_VERIFY " + o.PageVerify)
		}
		onOff("DB_CHAINING", o.DBChaining)
		if o.TargetRecoveryTimeSeconds >= 0 {
			set(fmt.Sprintf("TARGET_RECOVERY_TIME = %d SECONDS", o.TargetRecoveryTimeSeconds))
		}
		switch o.DelayedDurability {
		case "DISABLED", "ALLOWED", "FORCED":
			set("DELAYED_DURABILITY = " + o.DelayedDurability)
		}
		if majorAtLeast(l.major, SQLServer2019) {
			// The one option here spelled with "=". The version store's
			// filegroup exists by now: CREATE DATABASE made every one.
			switch {
			case o.AcceleratedDatabaseRecovery && l.pvsFileGroup != "" && l.pvsFileGroup != "PRIMARY":
				set("ACCELERATED_DATABASE_RECOVERY = ON (PERSISTENT_VERSION_STORE_FILEGROUP = " + quoteIdent(l.pvsFileGroup) + ")")
			case o.AcceleratedDatabaseRecovery && l.pvsRefused:
				sb.WriteString("-- Reading ADR's version store filegroup needs VIEW DATABASE PERFORMANCE STATE (VIEW SERVER STATE before 2022): PERSISTENT_VERSION_STORE_FILEGROUP not scripted.\n")
				fallthrough
			default:
				onOff("ACCELERATED_DATABASE_RECOVERY =", o.AcceleratedDatabaseRecovery)
			}
		}
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
	writeInDatabaseSettings(sb, name, l)
	if o != nil {
		if o.Owner != "" {
			fmt.Fprintf(sb, "ALTER AUTHORIZATION ON DATABASE::%s TO %s;\nGO\n", quoteIdent(name), quoteIdent(o.Owner))
		} else {
			sb.WriteString("-- The owner's SID maps to no login: ALTER AUTHORIZATION not scripted.\n")
		}
		if userAccessModes[o.UserAccess] {
			set(string(o.UserAccess))
		}
	}
	if d.IsReadOnly {
		set("READ_ONLY")
	} else {
		set("READ_WRITE")
	}
}

// writeInDatabaseSettings writes what is set from inside the database — the
// TDE key, the scoped configurations away from their default and the
// database's extended properties — between USE [db] and USE [master], then
// SET ENCRYPTION ON. It runs before the owner, user access and READ_ONLY: a
// key cannot be created in a read-only database, nor a scoped configuration
// changed or a property added.
//
// The TDE key is protected by a certificate or asymmetric key in master,
// referenced by name like an Always Encrypted key: it must exist on the
// instance the script runs on, with its private key. A key SET ENCRYPTION
// OFF left behind (tdeOff) is created all the same, and encryption stays off.
func writeInDatabaseSettings(sb *strings.Builder, name string, l *databaseLayout) {
	scoped := scopedConfigStatements(l)
	if l.tde == nil && !l.tdeRefused && len(scoped) == 0 && l.extProps == "" {
		return
	}
	fmt.Fprintf(sb, "USE %s;\nGO\n", quoteIdent(name))
	encrypt := false
	if k := l.tde; k != nil {
		var by string
		switch {
		case k.ServerCertificate != "":
			by = "SERVER CERTIFICATE " + quoteIdent(k.ServerCertificate)
		case k.ServerAsymmetricKey != "":
			by = "SERVER ASYMMETRIC KEY " + quoteIdent(k.ServerAsymmetricKey)
		}
		switch alg := k.Algorithm(); {
		case by == "":
			sb.WriteString("-- The database encryption key's protector is not visible in master: encryption not scripted.\n")
		case alg == "":
			sb.WriteString("-- The database encryption key reports an algorithm with no CREATE form: encryption not scripted.\n")
		default:
			fmt.Fprintf(sb, "CREATE DATABASE ENCRYPTION KEY WITH ALGORITHM = %s ENCRYPTION BY %s;\nGO\n", alg, by)
			encrypt = !l.tdeOff
		}
	} else if l.tdeRefused {
		sb.WriteString("-- Encryption is on, and reading its key needs VIEW SERVER STATE: encryption not scripted.\n")
	}
	for _, s := range scoped {
		sb.WriteString(s)
	}
	sb.WriteString(l.extProps)
	sb.WriteString("USE [master];\nGO\n")
	if encrypt {
		fmt.Fprintf(sb, "ALTER DATABASE %s SET ENCRYPTION ON;\nGO\n", quoteIdent(name))
	}
}

// scopedConfigDefaults2016 are the documented defaults of the four scoped
// configurations SQL Server 2016 has. Its catalog has no is_value_default
// (DatabaseScopedConfigs reads false for every row), so a 2016 source is
// compared against these instead.
var scopedConfigDefaults2016 = map[string]string{
	"MAXDOP": "0", "LEGACY_CARDINALITY_ESTIMATION": "0",
	"PARAMETER_SNIFFING": "1", "QUERY_OPTIMIZER_HOTFIXES": "0",
}

// scopedConfigStatements renders an ALTER DATABASE SCOPED CONFIGURATION for
// each of l.scoped whose value is away from its default, and a FOR
// SECONDARY one for each with a secondary value of its own (NULL, the
// default, means "as the primary").
//
// Only what differs is written, unlike the SET options: the set of scoped
// configurations grows with every release, and restating one the replaying
// instance does not know fails the script. On a 2016 source a row missing
// from scopedConfigDefaults2016 is left out — its default is not known.
func scopedConfigStatements(l *databaseLayout) []string {
	var out []string
	add := func(c *DatabaseScopedConfig, raw, baseType string, replica ScopedConfigReplica) {
		v, ok := scopedConfigValue(raw, baseType)
		stmt, err := buildScopedConfigStatement(c.Name, v, replica)
		if !ok || err != nil {
			out = append(out, fmt.Sprintf("-- Database scoped configuration %s reports a value with no SET form: not scripted.\n", tsql.CommentSafe(c.Name)))
			return
		}
		out = append(out, stmt+";\nGO\n")
	}
	for _, c := range l.scoped {
		isDefault := c.IsValueDefault
		if l.major == int(SQLServer2016) {
			def, known := scopedConfigDefaults2016[c.Name]
			isDefault = !known || c.Value == def
		}
		if !isDefault {
			add(c, c.Value, c.ValueType, ScopedConfigPrimary)
		}
		if c.ValueForSecondary != "" {
			add(c, c.ValueForSecondary, c.ValueForSecondaryType, ScopedConfigSecondary)
		}
	}
	return out
}

// scopedConfigValue is raw — the value as CAST to nvarchar — as the SET
// clause spells it, by its sql_variant base type: OFF/ON for a bit, the
// number for an integer type, a keyword (WHEN_SUPPORTED) for a string as it
// is. Anything else — a quoted string, a value with spaces, a type with no
// known form — is not written (false). The type, not the option's name,
// decides: a hand-kept list of numeric options would write a new one
// reporting 0 or 1 as OFF/ON.
func scopedConfigValue(raw, baseType string) (string, bool) {
	switch baseType {
	case "bit":
		switch raw {
		case "0":
			return "OFF", true
		case "1":
			return "ON", true
		}
	case "tinyint", "smallint", "int", "bigint":
		if _, err := strconv.ParseInt(raw, 10, 64); err == nil {
			return raw, true
		}
	case "nvarchar", "varchar", "nchar", "char", "sysname":
		if isSimpleIdentifier(raw) && (raw[0] < '0' || raw[0] > '9') {
			return raw, true
		}
	}
	return "", false
}
