package gosmo

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// Database mirrors Microsoft.SqlServer.Management.Smo.Database.
//
// Every field here is populated from one sys.databases row, so a Database
// from Server.DatabaseRef carries only Name and leaves the rest at its zero
// value — see Server.DatabaseRef. IsSystem and IsSnapshot are derived from
// ID and SourceDatabaseID rather than stored, so they answer false on such a
// handle, master included.
type Database struct {
	server *Server

	Name               string
	ID                 int
	State              DatabaseState
	RecoveryModel      RecoveryModel
	CompatibilityLevel CompatibilityLevel
	Collation          string
	IsReadOnly         bool
	CreateDate         time.Time

	// CatalogCollation is the collation the database compares its object
	// names under — Collation, except in a partially contained database
	// (always Latin1_General_100_CI_AS_KS_WS_SC, case-insensitive) and an
	// Azure SQL Database created WITH CATALOG_COLLATION. A caller deciding
	// whether two names inside the database are the same wants this, not
	// Collation: on a contained _CS_ database "dbo.ORDERS" resolves to
	// dbo.Orders. Empty on a DatabaseRef handle, like Collation.
	CatalogCollation string

	// SourceDatabaseID is sys.databases.source_database_id: the database a
	// snapshot was taken of, and 0 on every database that is not one. It
	// backs IsSnapshot, which is how a caller building a tree keeps a
	// snapshot out of the user-database list — Server.Databases returns it
	// like any other row, because the catalog does.
	SourceDatabaseID int

	// IsDistributor is sys.databases.is_distributor: this is a replication
	// Distributor's distribution database. It is not one of the four
	// built-ins, so IsSystem answers false for it, but SSMS lists it under
	// System Databases and it is dropped with sp_dropdistributiondb, not
	// DROP DATABASE — a caller building a tree or offering destructive
	// actions wants IsSystem() || IsDistributor. False on a DatabaseRef
	// handle, like every other field.
	IsDistributor bool
}

// systemDatabaseMaxID is the highest database_id SQL Server permanently
// reserves for its own system databases: master=1, tempdb=2, model=3,
// msdb=4. Every user database gets an id above this range.
const systemDatabaseMaxID = 4

// IsSystem reports whether this is one of SQL Server's four built-in
// system databases (master, tempdb, model, msdb), identified by their
// permanently reserved database_id (1-4) rather than by name. It is derived
// from ID, so it answers false on a Server.DatabaseRef handle, master
// included.
func (d *Database) IsSystem() bool { return d.ID > 0 && d.ID <= systemDatabaseMaxID }

// IsSnapshot reports whether this database is a database snapshot. Like
// IsSystem it is derived — from SourceDatabaseID — so it answers false on a
// Server.DatabaseRef handle.
func (d *Database) IsSnapshot() bool { return d.SourceDatabaseID != 0 }

// Server returns the parent Server. It is a back-pointer, not catalog state,
// so it stays a method — as Table.Database does.
func (d *Database) Server() *Server { return d.server }

// -- Size / space --------------------------------------------------------------

// SpaceInfo holds space usage information for a database.
type SpaceInfo struct {
	TotalMB float64
	DataMB  float64
	LogMB   float64
	// UnallocatedMB is free space within the database's already-allocated
	// data files (SSMS's Database Properties > General "Space available"),
	// not free disk space — it can only shrink the database's on-disk
	// footprint, not grow it, without a file autogrowth event.
	UnallocatedMB float64
	// AvailLogMB is the same free-space measure as UnallocatedMB, but for
	// the log file(s) rather than the data file(s).
	AvailLogMB float64
}

// SpaceUsed returns space usage for the database.
func (d *Database) SpaceUsed(ctx context.Context) (SpaceInfo, error) {
	const q = `
SELECT
    SUM(CAST(size AS bigint)) * 8.0 / 1024                                          AS total_mb,
    SUM(CASE WHEN type_desc <> 'LOG' THEN CAST(size AS bigint) ELSE 0 END) * 8.0 / 1024 AS data_mb,
    SUM(CASE WHEN type_desc =  'LOG' THEN CAST(size AS bigint) ELSE 0 END) * 8.0 / 1024 AS log_mb,
    SUM(CASE WHEN type_desc <> 'LOG'
             THEN CAST(size AS bigint) - CAST(FILEPROPERTY(name, 'SpaceUsed') AS INT)
             ELSE 0 END) * 8.0 / 1024                                          AS unallocated_mb,
    SUM(CASE WHEN type_desc = 'LOG'
             THEN CAST(size AS bigint) - CAST(FILEPROPERTY(name, 'SpaceUsed') AS INT)
             ELSE 0 END) * 8.0 / 1024                                          AS avail_log_mb
FROM sys.database_files`

	var si SpaceInfo
	if err := d.queryRow(ctx, func(row *sql.Row) error {
		return row.Scan(&si.TotalMB, &si.DataMB, &si.LogMB, &si.UnallocatedMB, &si.AvailLogMB)
	}, q); err != nil {
		return SpaceInfo{}, fmt.Errorf("gosmo: space used: %w", err)
	}
	return si, nil
}

// DiskUsage is a database's disk-usage breakdown — the numbers behind
// SSMS's "Disk Usage" report, in MB.
//
// The two halves are read separately, one per file kind, and each is a
// composition of the space its files hold:
//
//	data files: DataMB + IndexMB + UnusedMB + UnallocatedMB
//	log files:  LogUsedMB + LogUnusedMB
//
// UnallocatedMB and LogUnusedMB are file space not yet handed to any
// object, the same free-space measure SpaceInfo reports; UnusedMB is space
// already allocated to an object in extents whose pages it has not filled
// yet, so shrinking a file reclaims the former and rebuilding an index the
// latter.
//
// The data-file parts are counted from allocated pages and the file totals
// from the files themselves, so the four data parts sum to slightly less
// than DataFilesMB: the difference is the database's own internal pages
// (IAM, boot page, allocation bitmaps), which belong to no allocation unit.
// Read the parts against each other, not against DataFilesMB — that is how
// the SSMS report presents them too.
//
// These are not sp_spaceused's figures — SpaceReport is the read that
// matches that procedure, and its doc says where the two differ.
type DiskUsage struct {
	// DataFilesMB and LogFilesMB are the on-disk sizes of the ROWS and LOG
	// files respectively — DataFilesMB + LogFilesMB is the database's
	// footprint.
	DataFilesMB float64
	LogFilesMB  float64

	// DataMB is row data in heaps and clustered indexes, IndexMB is row
	// data in every other index, and both include the LOB and row-overflow
	// pages belonging to them, so no used page is counted twice or missed.
	DataMB  float64
	IndexMB float64
	// UnusedMB is reserved-but-unused space inside extents already
	// allocated to an object.
	UnusedMB float64
	// UnallocatedMB is data-file space not yet allocated to anything.
	UnallocatedMB float64

	// LogUsedMB and LogUnusedMB split the log files the same way — the
	// active portion against what a shrink could give back.
	LogUsedMB   float64
	LogUnusedMB float64
}

// DiskUsage returns the database's disk-usage breakdown.
//
// It is one round trip: the file figures and the allocation figures are
// unrelated aggregates over unrelated tables, so they are cross-joined rather
// than queried one after the other.
func (d *Database) DiskUsage(ctx context.Context) (DiskUsage, error) {
	// The allocation half deliberately spans every object, system tables
	// included: this describes the file, not the user's schema, and pages
	// left out of the sum would show up as unallocated space that a shrink
	// cannot reclaim. index_id 0/1 is the heap or clustered index, so its
	// LOB and row-overflow units (type 2 and 3) are the table's own data;
	// everything above is a nonclustered index.
	const q = `
SELECT
    f.data_files_mb, f.log_files_mb, f.unallocated_mb, f.avail_log_mb,
    a.data_mb, a.index_mb, a.unused_mb
FROM (
    SELECT
        SUM(CASE WHEN type_desc <> 'LOG' THEN CAST(size AS bigint) ELSE 0 END) * 8.0 / 1024 AS data_files_mb,
        SUM(CASE WHEN type_desc =  'LOG' THEN CAST(size AS bigint) ELSE 0 END) * 8.0 / 1024 AS log_files_mb,
        SUM(CASE WHEN type_desc <> 'LOG'
                 THEN CAST(size AS bigint) - CAST(FILEPROPERTY(name, 'SpaceUsed') AS INT)
                 ELSE 0 END) * 8.0 / 1024                                   AS unallocated_mb,
        SUM(CASE WHEN type_desc = 'LOG'
                 THEN CAST(size AS bigint) - CAST(FILEPROPERTY(name, 'SpaceUsed') AS INT)
                 ELSE 0 END) * 8.0 / 1024                                   AS avail_log_mb
    FROM sys.database_files
) f
CROSS JOIN (
    SELECT
        ISNULL(SUM(CASE WHEN i.index_id IN (0,1) THEN a.used_pages ELSE 0 END), 0) * 8.0 / 1024 AS data_mb,
        ISNULL(SUM(CASE WHEN i.index_id  > 1     THEN a.used_pages ELSE 0 END), 0) * 8.0 / 1024 AS index_mb,
        ISNULL(SUM(a.total_pages - a.used_pages), 0) * 8.0 / 1024                               AS unused_mb
    FROM sys.partitions p
    JOIN sys.allocation_units a ON a.container_id = CASE WHEN a.type = 2
                                                        THEN p.partition_id
                                                        ELSE p.hobt_id END
    JOIN sys.indexes i ON i.object_id = p.object_id AND i.index_id = p.index_id
) a`

	var du DiskUsage
	if err := d.queryRow(ctx, func(row *sql.Row) error {
		return row.Scan(&du.DataFilesMB, &du.LogFilesMB, &du.UnallocatedMB, &du.LogUnusedMB,
			&du.DataMB, &du.IndexMB, &du.UnusedMB)
	}, q); err != nil {
		return DiskUsage{}, fmt.Errorf("gosmo: disk usage: %w", err)
	}
	du.LogUsedMB = du.LogFilesMB - du.LogUnusedMB
	return du, nil
}

// -- Schemas -------------------------------------------------------------------

// schemaSelect is shared by Schemas and SchemaByName so a
// schema carries the same fields however it was fetched.
const schemaSelect = `
SELECT s.name, s.schema_id, p.name AS owner
FROM   sys.schemas s
JOIN   sys.database_principals p ON p.principal_id = s.principal_id`

// Schemas returns all schemas in the database.
func (d *Database) Schemas(ctx context.Context) ([]*Schema, error) {
	rows, err := d.query(ctx, schemaSelect+`
ORDER  BY s.name`)
	return scanRows(rows, err, fmt.Sprintf("list schemas in %q", d.Name), func(scan func(...any) error) (*Schema, error) {
		return scanSchema(d, scan)
	})
}

// SchemaByName returns one schema by name.
//
// It returns an error satisfying errors.Is(err, ErrNotFound) when the database
// has no such schema.
func (d *Database) SchemaByName(ctx context.Context, name string) (*Schema, error) {
	return readByName(ctx, d, scanSchema, schemaSelect+`
WHERE  s.name = @p1`, []any{name},
		notFoundf("gosmo: schema %q not found in %q", name, d.Name), fmt.Sprintf("find schema %q in %q", name, d.Name))
}

// SchemaRef returns a lightweight handle for a schema by name, without
// querying the catalog — the counterpart of Server.DatabaseRef. Every field
// but the name stays at its zero value; SchemaByName is what populates them.
//
// Every write on *Schema addresses it by name, so this handle is enough to
// drop one the caller already knows exists — and is the form to use when
// there is nothing to read yet, such as a script of a CREATE that was only
// collected.
func (d *Database) SchemaRef(name string) *Schema {
	return &Schema{db: d, Name: name}
}

func scanSchema(d *Database, scan func(...any) error) (*Schema, error) {
	sc := &Schema{db: d}
	if err := scan(&sc.Name, &sc.ID, &sc.Owner); err != nil {
		return nil, err
	}
	return sc, nil
}

// CreateSchemaRequest describes a new schema.
type CreateSchemaRequest struct {
	Name string
	// Owner is the AUTHORIZATION principal; empty leaves it to the server
	// (the caller's user).
	Owner string
}

// CreateSchema creates a new schema in the database, and returns it read
// back from the catalog — or, under Scripting(ctx), the SchemaRef handle,
// since nothing ran.
func (d *Database) CreateSchema(ctx context.Context, req CreateSchemaRequest) (*Schema, error) {
	return createOwned(ctx, d.exec, "schema", "CREATE SCHEMA", req.Name, req.Owner, d.SchemaRef(req.Name), d.SchemaByName)
}

// -- Tables --------------------------------------------------------------------

// TableQuery selects which tables Tables returns. The zero value is every
// table SQL Server did not ship (TableKindAll), unfiltered.
type TableQuery struct {
	// Kind narrows the listing to one family; see TableKind.
	Kind TableKind
	// Filter narrows it at the server; the zero value matches every table.
	Filter ObjectFilter
}

// Tables returns the tables a TableQuery selects, narrowed by the server
// rather than by the caller. Tables(ctx, TableQuery{}) is every user table.
//
// TableKindGraph is refused below SQL Server 2017 (errors.Is
// ErrUnsupportedVersion) rather than answered with an empty list.
func (d *Database) Tables(ctx context.Context, q TableQuery) ([]*Table, error) {
	if q.Kind == TableKindGraph {
		if err := d.requireGraphTables(); err != nil {
			return nil, err
		}
	}
	where, args := q.Filter.clause(tableFilterColumns)
	return d.tablesWhere(ctx, kindClause(d.serverMajorVersion(), q.Kind)+" "+where, args)
}

// userTablesClause is the "not SQL Server's own" test every listing that is
// not asked for a specific TableKind applies (TableKindAll's clause). The kind
// listings supply their own is_ms_shipped test instead — see kindClause.
const userTablesClause = "AND t.is_ms_shipped = 0"

// tableFilterColumns maps an ObjectFilter onto sys.tables as tablesWhere
// aliases it.
var tableFilterColumns = filterColumns{
	name:            "t.name",
	schema:          "SCHEMA_NAME(t.schema_id) COLLATE DATABASE_DEFAULT",
	created:         "t.create_date",
	memoryOptimized: "t.is_memory_optimized",
}

// TablesBySchema returns all tables in a specific schema.
func (d *Database) TablesBySchema(ctx context.Context, schema string) ([]*Table, error) {
	if err := requireSchema("tables by schema", schema, schema); err != nil {
		return nil, err
	}
	return d.tablesWhere(ctx, userTablesClause+" AND SCHEMA_NAME(t.schema_id) COLLATE DATABASE_DEFAULT = @p1", []any{schema})
}

// tableSelect is the SELECT list every Table listing shares, version-gated:
// is_node and is_edge are 2017 columns (see graphPredicate), substituted with
// a zero bit below that so the scan destinations stay the same at every
// major.
func (d *Database) tableSelect() string {
	major := d.serverMajorVersion()
	return `
SELECT t.object_id, SCHEMA_NAME(t.schema_id), t.name,
       t.create_date, t.modify_date,
       t.has_replication_filter, t.is_memory_optimized,
       t.is_ms_shipped, t.is_filetable, t.is_external,
       ` + bitSince(major, SQLServer2017, "t.is_node") + `,
       ` + bitSince(major, SQLServer2017, "t.is_edge") + `
FROM   sys.tables t`
}

// scanTable reads one row of tableSelect.
func scanTable(d *Database, scan func(...any) error) (*Table, error) {
	t := &Table{db: d}
	if err := scan(&t.ObjectID, &t.Schema, &t.Name,
		&t.CreateDate, &t.ModifyDate,
		&t.HasReplicationFilter, &t.IsMemoryOptimized,
		&t.IsSystem, &t.IsFileTable, &t.IsExternal, &t.IsNode, &t.IsEdge); err != nil {
		return nil, err
	}
	return t, nil
}

func (d *Database) tablesWhere(ctx context.Context, where string, args []any) ([]*Table, error) {
	q := d.tableSelect() + `
WHERE  1 = 1 ` + where + `
ORDER  BY SCHEMA_NAME(t.schema_id), t.name`

	rows, err := d.query(ctx, q, args...)
	return scanRows(rows, err, fmt.Sprintf("list tables in %q", d.Name), func(scan func(...any) error) (*Table, error) {
		return scanTable(d, scan)
	})
}

// TableByName returns a single table by schema and name using a direct query.
//
// It finds a system table (is_ms_shipped = 1) as readily as a user one: the
// caller asked for a table by name, and msdb's own tables — which is most of
// what msdb has — are the ones a by-name lookup would otherwise never reach.
// Tables() still lists only the user tables; the predicate belongs to the
// listing, not to the lookup.
func (d *Database) TableByName(ctx context.Context, schema, name string) (*Table, error) {
	if err := requireSchema("table by name", schema, name); err != nil {
		return nil, err
	}
	q := d.tableSelect() + `
WHERE  SCHEMA_NAME(t.schema_id) COLLATE DATABASE_DEFAULT = @p1
  AND  t.name                                           = @p2`

	return readByName(ctx, d, scanTable, q, []any{schema, name},
		notFoundf("gosmo: table %s not found in %q", qualifiedName(schema, name), d.Name),
		fmt.Sprintf("find table %s in %q", qualifiedName(schema, name), d.Name))
}

// -- Drop and rename -----------------------------------------------------------

// Drop drops the database. With TerminationNone it fails while another
// session is connected. With TerminationRollbackImmediate those sessions are
// terminated first — by SET SINGLE_USER WITH ROLLBACK IMMEDIATE, or on a
// Managed Instance, which refuses that statement, by killing the database's
// sessions (see killDatabaseSessions). A forced drop that fails puts the
// database back to the user-access mode it had — MULTI_USER, or
// RESTRICTED_USER left as it was. A database that cannot be set SINGLE_USER
// and has nobody to terminate — RESTORING, RECOVERY_PENDING, OFFLINE — is
// dropped as it is, and one in STANDBY has its sessions killed instead (see
// exclusiveBatch). This Server's own idle sessions are released first either
// way (see ReleaseIdleConnections).
//
// SQL Server deletes only the files of a database it has open: dropping an
// OFFLINE database leaves its data and log files on disk, an EMERGENCY one its
// log file, and one in STANDBY its undo file (probed on 17).
//
// Dropping the database this Server's pool logs in to moves the pool to
// master (see InitialDatabase).
func (d *Database) Drop(ctx context.Context, term Termination) error {
	s, name := d.server, d.Name
	if name == "" {
		return invalidf("gosmo: drop database: name is required")
	}
	if _, err := term.withClause(); err != nil {
		return fmt.Errorf("gosmo: drop database %q: %w", name, err)
	}
	force := term == TerminationRollbackImmediate
	s.releaseIdle(ctx)
	drop := fmt.Sprintf("DROP DATABASE %s", quoteIdent(name))
	// Every form starts from master: see fromMaster.
	switch {
	case !force:
		// Nothing here set the access mode, so nothing is repaired: a
		// MULTI_USER on the way out would silently undo a RESTRICTED_USER or
		// SINGLE_USER the database was deliberately left in.
		if _, err := s.exec(ctx, fromMaster(drop)); err != nil {
			return fmt.Errorf("gosmo: drop database %q: %w", name, err)
		}
	case s.refusesSingleUser():
		// One batch for the same reason as exclusiveBatch: a session that
		// reconnects between the KILLs and the DROP makes the DROP fail.
		if _, err := s.exec(ctx, fromMaster(killDatabaseSessionsBatch(name)+";\n"+drop+";")); err != nil {
			return fmt.Errorf("gosmo: drop database %q: %w", name, err)
		}
	default:
		mode := s.accessModeBefore(ctx, name)
		if _, err := s.exec(ctx, fromMaster(exclusiveBatch(name, drop))); err != nil {
			// The drop can genuinely fail after the alter succeeded — the
			// database belongs to an availability group, the login may set
			// state but not drop — and the batch puts it back to the access
			// mode it had itself. Only a batch cut short needs the repair from here.
			// Best effort: the drop's own error is what the caller is told.
			if batchCutShort(err) {
				_ = s.restoreAccessMode(ctx, name, mode)
			}
			return fmt.Errorf("gosmo: drop database %q: %w", name, err)
		}
	}
	s.leftLoginDatabase(ctx, name, "master")
	return nil
}

// Rename renames the database (ALTER DATABASE ... MODIFY NAME). The
// server needs exclusive access to it, so any other connection to the
// database fails the statement outright rather than waiting.
//
// With TerminationRollbackImmediate the database is put into SINGLE_USER WITH ROLLBACK
// IMMEDIATE first — terminating those connections and rolling back their
// transactions — and back to the user-access mode it had afterwards,
// including when the rename itself fails, so a refused rename never leaves
// the database single-user and a RESTRICTED_USER one is not opened to
// everyone. A Managed Instance refuses SET SINGLE_USER, so there
// TerminationRollbackImmediate kills the database's sessions instead and
// changes no access mode. This Server's own idle sessions are released first
// either way (see ReleaseIdleConnections).
//
// The new name is mirrored onto the receiver (through setIfApplied, so not
// under WithScript), and onto the pool's login database when it is this one
// (see InitialDatabase).
func (d *Database) Rename(ctx context.Context, newName string, term Termination) error {
	if err := d.rename(ctx, newName, term); err != nil {
		return err
	}
	d.server.leftLoginDatabase(ctx, d.Name, newName)
	setIfApplied(ctx, &d.Name, newName)
	return nil
}

func (d *Database) rename(ctx context.Context, newName string, term Termination) error {
	s, oldName := d.server, d.Name
	if oldName == "" || newName == "" {
		return invalidf("gosmo: rename database: both names are required")
	}
	if _, err := term.withClause(); err != nil {
		return fmt.Errorf("gosmo: rename database %q: %w", oldName, err)
	}
	force := term == TerminationRollbackImmediate
	q := fmt.Sprintf("ALTER DATABASE %s MODIFY NAME = %s", quoteIdent(oldName), quoteIdent(newName))
	s.releaseIdle(ctx)
	switch {
	case !force:
	case s.refusesSingleUser():
		// Nothing to release afterwards: no access mode was changed. One
		// batch, as in Drop.
		q = killDatabaseSessionsBatch(oldName) + ";\n" + q + ";"
	default:
		mode := s.accessModeBefore(ctx, oldName)
		if _, err := s.exec(ctx, renameExclusiveBatch(oldName, newName)); err != nil {
			if batchCutShort(err) {
				_ = s.restoreAccessModeAfterRename(ctx, oldName, newName, mode)
			}
			return fmt.Errorf("gosmo: rename database %q to %q: %w", oldName, newName, err)
		}
		return nil
	}
	if _, err := s.exec(ctx, q); err != nil {
		return fmt.Errorf("gosmo: rename database %q to %q: %w", oldName, newName, err)
	}
	return nil
}
