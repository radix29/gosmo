package gosmo

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
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
	State              string // state_desc: ONLINE, OFFLINE, RESTORING, ...
	RecoveryModel      RecoveryModel
	CompatibilityLevel CompatibilityLevel
	Collation          string
	IsReadOnly         bool
	CreateDate         time.Time

	// SourceDatabaseID is sys.databases.source_database_id: the database a
	// snapshot was taken of, and 0 on every database that is not one. It
	// backs IsSnapshot, which is how a caller building a tree keeps a
	// snapshot out of the user-database list — Server.Databases returns it
	// like any other row, because the catalog does.
	SourceDatabaseID int
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

// -- Connection helpers --------------------------------------------------------
// These acquire a dedicated connection from the pool, switch to the correct
// database via USE, run the statement, then return the connection to the pool.
// This is safe under connection pooling because we hold the *sql.Conn for the
// entire duration of the call.

// withConn acquires a connection and switches it to d's database (USE) —
// both idempotent and safe to retry against a fresh connection on a
// transient failure (a dropped pooled connection, etc.), same as
// query/queryRow's own acquire step below — before handing it to fn, which
// is not retried: fn is the caller's actual write, and blindly re-running
// it on a fresh connection after a partial failure could re-apply side
// effects that already took hold.
func (d *Database) withConn(ctx context.Context, fn func(*sql.Conn) error) error {
	conn, err := withRetry(ctx, func() (*sql.Conn, error) {
		conn, err := d.server.db.Conn(ctx)
		if err != nil {
			return nil, fmt.Errorf("gosmo: acquire connection: %w", err)
		}
		if err := d.use(ctx, conn); err != nil {
			conn.Close()
			return nil, err
		}
		return conn, nil
	})
	if err != nil {
		return err
	}
	defer conn.Close()
	return withAllMessages(fn(conn))
}

// scriptResult is the sql.Result stand-in returned to callers of exec when
// a ScriptCollector is capturing statements instead of running them — its
// value is never inspected by exec's own callers, which either discard the
// result or (as with the extended-property writers) issue a single
// unambiguous statement per call rather than branching on RowsAffected.
type scriptResult struct{}

func (scriptResult) LastInsertId() (int64, error) { return 0, nil }
func (scriptResult) RowsAffected() (int64, error) { return 0, nil }

func (d *Database) exec(ctx context.Context, q string, args ...any) (sql.Result, error) {
	if c, ok := scriptFrom(ctx); ok {
		// Parameters are substituted into the text, not dropped: a captured
		// statement is run by hand in a query editor, where nothing binds
		// @p1 — see bindScriptArgs.
		bound, err := bindScriptArgs(q, args)
		if err != nil {
			return nil, err
		}
		// The real path below always runs q after a USE — captured
		// statements need that made explicit, since the script may be
		// handed to a session scoped to a different database (or none).
		// The entry records the database rather than prefixing a USE:
		// ScriptCollector renders it, with the GO a statement that must
		// open its batch needs after it.
		c.append(ScriptEntry{Server: scriptServerName(ctx, d.server), Database: d.Name, SQL: bound})
		return scriptResult{}, nil
	}
	var res sql.Result
	err := d.withConn(ctx, func(c *sql.Conn) error {
		var e error
		res, e = c.ExecContext(ctx, q, args...)
		return e
	})
	if err == nil && observerFrom(ctx) != nil {
		sent, berr := bindScriptArgs(q, args)
		if berr != nil {
			sent = q
		}
		observe(ctx, ScriptEntry{Server: scriptServerName(ctx, d.server), Database: d.Name, SQL: sent})
	}
	return res, err
}

// dbRows wraps a *sql.Rows obtained from a *sql.Conn pinned specifically for
// it (see Database.query), so that closing the rows also returns the pinned
// connection to the pool. *sql.Rows.Close alone only releases the query's
// own resources — a *sql.Conn stays checked out from the pool until its own
// Close is called, and nothing does that automatically.
type dbRows struct {
	*sql.Rows
	conn *sql.Conn
}

func (r *dbRows) Close() error {
	err := r.Rows.Close()
	if cerr := r.conn.Close(); err == nil {
		err = cerr
	}
	return err
}

// use switches conn to d's database on its own, with the error every
// database-scoped call has always reported for a database it cannot enter.
func (d *Database) use(ctx context.Context, conn *sql.Conn) error {
	if _, err := conn.ExecContext(ctx, "USE "+quoteIdent(d.Name)); err != nil {
		return fmt.Errorf("gosmo: USE %s: %w", d.Name, err)
	}
	return nil
}

// useBatch is q preceded by the switch to d's database, as one batch — a read
// then costs one round trip rather than two, which is 41 ms per read against
// an Azure SQL Managed Instance and 1.3 ms on a LAN.
//
// The guard is what makes one batch safe. A USE that fails (a missing,
// offline or inaccessible database: 911, 942, 916) raises its error and the
// batch goes on, so without it q would run in whatever database the pooled
// session was in and return that database's rows as this one's. @@ERROR
// rather than a DB_NAME() comparison, which would depend on the collation of
// the database compared in and on how the caller cased the name.
//
// The prefix shares q's first line, so every line number an error in q
// reports is the one q alone reported. Statements after a USE are compiled in
// the database it switched to; verified on 2016, 2017, 2025 and Managed
// Instance, parameterised and not.
func (d *Database) useBatch(q string) string {
	return "USE " + quoteIdent(d.Name) + "; IF @@ERROR <> 0 RETURN; " + q
}

func (d *Database) query(ctx context.Context, q string, args ...any) (*dbRows, error) {
	// For queries that return rows we cannot use withConn (the conn would be
	// released before the caller finishes iterating). Instead we acquire a
	// dedicated conn, switch DB, run the query, and return the rows wrapped
	// with that conn — the caller's defer rows.Close() releases both.
	// A single read is idempotent, so a transient failure (dropped pooled
	// connection, etc.) is retried on a fresh connection.
	return withRetry(ctx, func() (*dbRows, error) {
		conn, err := d.server.db.Conn(ctx)
		if err != nil {
			return nil, fmt.Errorf("gosmo: acquire connection: %w", err)
		}
		rows, err := conn.QueryContext(ctx, d.useBatch(q), args...)
		if err != nil && ctx.Err() == nil {
			// Either half of the batch may have failed, and which one decides
			// the error — a USE failure is reported as one — so the failure
			// is reproduced the way it always was, one statement at a time.
			// Only a failing read pays for this.
			if err := d.use(ctx, conn); err != nil {
				conn.Close()
				return nil, err
			}
			rows, err = conn.QueryContext(ctx, q, args...)
		}
		if err != nil {
			conn.Close()
			return nil, err
		}
		return &dbRows{Rows: rows, conn: conn}, nil
	})
}

// queryRow acquires a connection, switches it to d's database (USE), runs
// q, and hands the resulting row to scan — retrying the whole acquire+USE+
// scan sequence as one unit on a transient connection failure, same as
// Server.queryRow and for the same reason: QueryRowContext itself never
// returns an error, only Scan does, so scan has to run inside the retried
// closure to be covered by it at all. Handing the caller a live *sql.Row
// to scan later would let withRetry see a nil error and return before the
// failure that only surfaces at Scan time, silently skipping the retry.
//
// The USE and q go as one batch (see useBatch). A failed USE surfaces in
// Row.Err before scan sees the row, and that is where the batch falls back to
// the two statements, as query does — scan never sees a USE failure, which it
// could otherwise wrap or map to something else.
func (d *Database) queryRow(ctx context.Context, scan func(*sql.Row) error, q string, args ...any) error {
	_, err := withRetry(ctx, func() (struct{}, error) {
		conn, err := d.server.db.Conn(ctx)
		if err != nil {
			return struct{}{}, fmt.Errorf("gosmo: acquire connection: %w", err)
		}
		defer conn.Close()
		row := conn.QueryRowContext(ctx, d.useBatch(q), args...)
		if row.Err() != nil && ctx.Err() == nil {
			if err := d.use(ctx, conn); err != nil {
				return struct{}{}, err
			}
			row = conn.QueryRowContext(ctx, q, args...)
		}
		return struct{}{}, scan(row)
	})
	return err
}

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
    SUM(size) * 8.0 / 1024                                                   AS total_mb,
    SUM(CASE WHEN type_desc <> 'LOG' THEN size ELSE 0 END) * 8.0 / 1024     AS data_mb,
    SUM(CASE WHEN type_desc =  'LOG' THEN size ELSE 0 END) * 8.0 / 1024     AS log_mb,
    SUM(CASE WHEN type_desc <> 'LOG'
             THEN size - CAST(FILEPROPERTY(name, 'SpaceUsed') AS INT)
             ELSE 0 END) * 8.0 / 1024                                       AS unallocated_mb,
    SUM(CASE WHEN type_desc = 'LOG'
             THEN size - CAST(FILEPROPERTY(name, 'SpaceUsed') AS INT)
             ELSE 0 END) * 8.0 / 1024                                       AS avail_log_mb
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
        SUM(CASE WHEN type_desc <> 'LOG' THEN size ELSE 0 END) * 8.0 / 1024 AS data_files_mb,
        SUM(CASE WHEN type_desc =  'LOG' THEN size ELSE 0 END) * 8.0 / 1024 AS log_files_mb,
        SUM(CASE WHEN type_desc <> 'LOG'
                 THEN size - CAST(FILEPROPERTY(name, 'SpaceUsed') AS INT)
                 ELSE 0 END) * 8.0 / 1024                                   AS unallocated_mb,
        SUM(CASE WHEN type_desc = 'LOG'
                 THEN size - CAST(FILEPROPERTY(name, 'SpaceUsed') AS INT)
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
	var sc *Schema
	err := d.queryRow(ctx, func(row *sql.Row) error {
		var err error
		sc, err = scanSchema(d, row.Scan)
		return err
	}, schemaSelect+`
WHERE  s.name = @p1`, name)
	return foundRow(sc, err, notFoundf("gosmo: schema %q not found in %q", name, d.Name), fmt.Sprintf("find schema %q in %q", name, d.Name))
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
	if req.Name == "" {
		return nil, fmt.Errorf("gosmo: create schema: name is required")
	}
	q := "CREATE SCHEMA " + quoteIdent(req.Name)
	if req.Owner != "" {
		q += " AUTHORIZATION " + quoteIdent(req.Owner)
	}
	if _, err := d.exec(ctx, q); err != nil {
		return nil, fmt.Errorf("gosmo: create schema %q: %w", req.Name, err)
	}
	return createdObject(ctx, d.SchemaRef(req.Name), func() (*Schema, error) {
		return d.SchemaByName(ctx, req.Name)
	})
}

// -- Tables --------------------------------------------------------------------

// Tables returns all user tables in the database.
func (d *Database) Tables(ctx context.Context) ([]*Table, error) {
	return d.tablesWhere(ctx, userTablesClause, nil)
}

// userTablesClause is the "not SQL Server's own" test every listing that is
// not asked for a specific TableKind applies. The kind listings supply their
// own is_ms_shipped test instead — see kindClause.
const userTablesClause = "AND t.is_ms_shipped = 0"

// TablesFiltered returns the user tables an ObjectFilter matches, narrowed by
// the server rather than by the caller. An empty filter is Tables.
func (d *Database) TablesFiltered(ctx context.Context, filter ObjectFilter) ([]*Table, error) {
	where, args := filter.clause(tableFilterColumns, 1)
	return d.tablesWhere(ctx, userTablesClause+" "+where, args)
}

// tableFilterColumns maps an ObjectFilter onto sys.tables as tablesWhere
// aliases it.
var tableFilterColumns = filterColumns{
	name:            "t.name",
	schema:          "SCHEMA_NAME(t.schema_id)",
	created:         "t.create_date",
	memoryOptimized: "t.is_memory_optimized",
}

// TablesBySchema returns all tables in a specific schema.
func (d *Database) TablesBySchema(ctx context.Context, schema string) ([]*Table, error) {
	if err := requireSchema("tables by schema", schema, schema); err != nil {
		return nil, err
	}
	return d.tablesWhere(ctx, userTablesClause+" AND SCHEMA_NAME(t.schema_id) = @p1", []any{schema})
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
       ` + colSince(major, SQLServer2017, "t.is_node", "CAST(0 AS bit)") + `,
       ` + colSince(major, SQLServer2017, "t.is_edge", "CAST(0 AS bit)") + `
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
WHERE  SCHEMA_NAME(t.schema_id) = @p1
  AND  t.name                   = @p2`

	var t *Table
	err := d.queryRow(ctx, func(row *sql.Row) error {
		var err error
		t, err = scanTable(d, row.Scan)
		return err
	}, q, schema, name)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, notFoundf("gosmo: table [%s].[%s] not found in %q", schema, name, d.Name)
		}
		return nil, err
	}
	return t, nil
}

// -- Database users ------------------------------------------------------------

// userTypes is every sys.database_principals type that is a user rather than
// a role: SQL, Windows user and group, Entra user and group ('E','X'), and
// the certificate- and asymmetric-key-mapped users ('C','K'). Listing only
// the first three left the rest out of the Users folder and made Script as
// report them not found — every FROM EXTERNAL PROVIDER user on Managed
// Instance among them.
const userTypes = `('S','U','G','E','X','C','K')`

// Users returns all database users.
func (d *Database) Users(ctx context.Context) ([]*User, error) {
	const q = `
SELECT name, principal_id, type_desc, default_schema_name,
       create_date, modify_date, authentication_type_desc
FROM   sys.database_principals
WHERE  type IN ` + userTypes + `
ORDER  BY name`

	rows, err := d.query(ctx, q)
	return scanRows(rows, err, fmt.Sprintf("list users in %q", d.Name), func(scan func(...any) error) (*User, error) {
		u := &User{db: d}
		var defSchema, authType sql.NullString
		if err := scan(&u.Name, &u.ID, &u.UserType, &defSchema,
			&u.CreateDate, &u.ModifyDate, &authType); err != nil {
			return nil, err
		}
		u.DefaultSchema = defSchema.String
		u.AuthType = authType.String
		return u, nil
	})
}

// UserByName returns a single database user by name, with its SID, matching
// server login (if any) and mapped certificate or asymmetric key filled in —
// Users leaves these out since Object Explorer's tree listing never needs
// them.
func (d *Database) UserByName(ctx context.Context, name string) (*User, error) {
	const q = `
SELECT dp.principal_id, dp.type_desc, dp.default_schema_name,
       dp.create_date, dp.modify_date, dp.authentication_type_desc, dp.sid,
       sp.name, sp.is_disabled,
       CASE dp.type WHEN 'C' THEN (SELECT TOP 1 c.name  FROM sys.certificates    c  WHERE c.sid  = dp.sid)
                    WHEN 'K' THEN (SELECT TOP 1 ak.name FROM sys.asymmetric_keys ak WHERE ak.sid = dp.sid)
       END
FROM   sys.database_principals dp
LEFT   JOIN sys.server_principals sp ON sp.sid = dp.sid
WHERE  dp.type IN ` + userTypes + ` AND dp.name = @p1`

	u := &User{db: d, Name: name}
	var defSchema, authType, loginName, mapped sql.NullString
	var loginDisabled sql.NullBool
	err := d.queryRow(ctx, func(row *sql.Row) error {
		return row.Scan(&u.ID, &u.UserType, &defSchema, &u.CreateDate, &u.ModifyDate,
			&authType, &u.SID, &loginName, &loginDisabled, &mapped)
	}, q, name)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, notFoundf("gosmo: database user %q not found in %q", name, d.Name)
		}
		return nil, fmt.Errorf("gosmo: find database user %q in %q: %w", name, d.Name, err)
	}
	u.DefaultSchema = defSchema.String
	u.AuthType = authType.String
	u.LoginName = loginName.String
	u.LoginDisabled = loginDisabled.Bool
	u.MappedObject = mapped.String
	return u, nil
}

// UserRef returns a lightweight handle for name without querying the server
// at all — unlike UserByName, it doesn't verify the user
// exists or populate ID/UserType/DefaultSchema/AuthType/SID/LoginName/etc.
// (they stay at their zero value). Every write method on *User (Drop,
// Rename, SetDefaultSchema, SetLogin, AddToRole,
// Grant, ...) only ever needs the user's name, never those cached
// fields, so this is sufficient for issuing further ALTER-style calls against
// a user the caller already knows exists — most commonly one it just created
// in the same operation. See Server.DatabaseRef's doc comment for why this
// also matters under a WithScript-derived context.
func (d *Database) UserRef(name string) *User {
	return &User{db: d, Name: name}
}

// UserKind selects the CREATE USER form a CreateUserRequest issues.
type UserKind int

const (
	// UserForLogin is a user mapped to a server login (FOR LOGIN). It is the
	// zero value.
	UserForLogin UserKind = iota
	// UserWithoutLogin is a user nobody connects as (WITHOUT LOGIN) — for
	// impersonation, or to own objects and hold permissions.
	UserWithoutLogin
	// UserWithPassword is a contained database user that authenticates with
	// its own password (WITH PASSWORD). The database must be partially
	// contained, except on Azure SQL Database, where every database is.
	UserWithPassword
	// UserWindows is a Windows user or group, named DOMAIN\name. With a
	// Login it is FOR LOGIN; without one it is the bare statement, a
	// contained database's Windows user.
	UserWindows
	// UserFromCertificate maps the user to a certificate in the database
	// (FROM CERTIFICATE), to hold permissions for code signed by it.
	UserFromCertificate
	// UserFromAsymmetricKey is UserFromCertificate for an asymmetric key.
	UserFromAsymmetricKey
	// UserFromExternalProvider is a Microsoft Entra user or group
	// (FROM EXTERNAL PROVIDER) — Azure SQL Database, Managed Instance and
	// SQL Server 2022 and later.
	UserFromExternalProvider
)

// String renders the kind as the words used in error messages.
func (k UserKind) String() string {
	switch k {
	case UserForLogin:
		return "login-mapped"
	case UserWithoutLogin:
		return "login-less"
	case UserWithPassword:
		return "contained"
	case UserWindows:
		return "Windows"
	case UserFromCertificate:
		return "certificate-mapped"
	case UserFromAsymmetricKey:
		return "asymmetric-key-mapped"
	case UserFromExternalProvider:
		return "external provider"
	}
	return fmt.Sprintf("UserKind(%d)", int(k))
}

// CreateUserRequest describes a new database user. Kind selects the form, and
// each of the other fields is accepted only by the kinds that use it — a field
// set for a kind that has no clause for it is refused rather than dropped.
type CreateUserRequest struct {
	Name string
	Kind UserKind
	// Login is required for UserForLogin and optional for UserWindows.
	Login string
	// Password is required for UserWithPassword, and refused otherwise.
	Password string
	// Certificate / AsymmetricKey name the object a mapped user maps to.
	Certificate   string
	AsymmetricKey string
	// ObjectID is the Entra object id, for UserFromExternalProvider only.
	ObjectID string
	// DefaultSchema is refused for the certificate- and key-mapped kinds,
	// which SQL Server refuses it for too.
	DefaultSchema string
}

// CreateUser creates a database user of any kind — see UserKind.
//
// A contained user's password goes through QuoteLiteral, as CreateLogin's
// does. Asking for one in a database that is not partially contained is
// refused here, after a read of sys.databases: SQL Server's own refusal
// (Msg 33233) says "only in a contained database" without naming the setting
// or the database.
func (d *Database) CreateUser(ctx context.Context, req CreateUserRequest) (*User, error) {
	stmt, err := createUserStatement(req)
	if err != nil {
		if req.Name == "" {
			return nil, fmt.Errorf("gosmo: create user: %w", err)
		}
		return nil, fmt.Errorf("gosmo: create user %q: %w", req.Name, err)
	}
	if req.Kind == UserWithPassword && !d.server.everyDatabaseContained() {
		var containment string
		if err := d.server.queryRowScan(ctx,
			"SELECT containment_desc FROM sys.databases WHERE name = @p1",
			[]any{d.Name}, &containment); err != nil {
			return nil, fmt.Errorf("gosmo: create user %q: read containment of %q: %w", req.Name, d.Name, err)
		}
		if containment != "PARTIAL" {
			return nil, fmt.Errorf("gosmo: create user %q: a user with a password needs a contained database, and %q has CONTAINMENT = %s",
				req.Name, d.Name, containment)
		}
	}
	if _, err := d.exec(ctx, stmt); err != nil {
		return nil, fmt.Errorf("gosmo: create user %q: %w", req.Name, err)
	}
	return createdObject(ctx, d.UserRef(req.Name), func() (*User, error) {
		return d.UserByName(ctx, req.Name)
	})
}

// everyDatabaseContained reports whether the engine takes contained users in
// any database without CONTAINMENT = PARTIAL — Azure SQL Database does, and
// reports containment NONE for all of them.
func (s *Server) everyDatabaseContained() bool {
	return s.info != nil && EngineEdition(s.info.EngineEdition) == EngineAzureSQLDatabase
}

// createUserStatement validates req and builds its CREATE USER statement.
func createUserStatement(req CreateUserRequest) (string, error) {
	if req.Name == "" {
		return "", fmt.Errorf("user name is required")
	}
	k := req.Kind
	// Each optional field against the kinds that have a clause for it.
	switch {
	case req.Login != "" && k != UserForLogin && k != UserWindows:
		return "", fmt.Errorf("a %s user takes no login", k)
	case req.Password != "" && k != UserWithPassword:
		return "", fmt.Errorf("a %s user takes no password", k)
	case req.Certificate != "" && k != UserFromCertificate:
		return "", fmt.Errorf("a %s user maps to no certificate", k)
	case req.AsymmetricKey != "" && k != UserFromAsymmetricKey:
		return "", fmt.Errorf("a %s user maps to no asymmetric key", k)
	case req.ObjectID != "" && k != UserFromExternalProvider:
		return "", fmt.Errorf("ObjectID applies to an external provider user only, not a %s user", k)
	case req.DefaultSchema != "" && (k == UserFromCertificate || k == UserFromAsymmetricKey):
		return "", fmt.Errorf("a %s user cannot have a default schema", k)
	}

	var opts []string
	stmt := "CREATE USER " + quoteIdent(req.Name)
	switch k {
	case UserForLogin:
		// Without this, quoteIdent("") turns an empty login into "FOR LOGIN
		// []" — a statement the server rejects with a message naming an empty
		// login the caller never typed.
		if req.Login == "" {
			return "", fmt.Errorf("login name is required")
		}
		stmt += " FOR LOGIN " + quoteIdent(req.Login)
	case UserWithoutLogin:
		stmt += " WITHOUT LOGIN"
	case UserWithPassword:
		if req.Password == "" {
			return "", fmt.Errorf("a contained user requires a password")
		}
		opts = append(opts, "PASSWORD = "+QuoteLiteral(req.Password))
	case UserWindows:
		if req.Login != "" {
			stmt += " FOR LOGIN " + quoteIdent(req.Login)
		}
	case UserFromCertificate:
		if req.Certificate == "" {
			return "", fmt.Errorf("a certificate-mapped user requires Certificate")
		}
		stmt += " FROM CERTIFICATE " + quoteIdent(req.Certificate)
	case UserFromAsymmetricKey:
		if req.AsymmetricKey == "" {
			return "", fmt.Errorf("an asymmetric-key-mapped user requires AsymmetricKey")
		}
		stmt += " FROM ASYMMETRIC KEY " + quoteIdent(req.AsymmetricKey)
	case UserFromExternalProvider:
		stmt += " FROM EXTERNAL PROVIDER"
		if req.ObjectID != "" {
			opts = append(opts, "OBJECT_ID = "+QuoteLiteral(req.ObjectID))
		}
	default:
		return "", fmt.Errorf("unknown user kind %s", k)
	}
	if req.DefaultSchema != "" {
		opts = append(opts, "DEFAULT_SCHEMA = "+quoteIdent(req.DefaultSchema))
	}
	if len(opts) > 0 {
		stmt += " WITH " + strings.Join(opts, ", ")
	}
	return stmt, nil
}

// -- Settings ------------------------------------------------------------------

// SetRecoveryModel changes the database recovery model.
func (d *Database) SetRecoveryModel(ctx context.Context, model RecoveryModel) error {
	if !validRecoveryModel(model) {
		return fmt.Errorf("gosmo: set recovery model: unrecognized recovery model %q", model)
	}
	if err := d.server.exec(ctx,
		fmt.Sprintf("ALTER DATABASE %s SET RECOVERY %s", quoteIdent(d.Name), model),
	); err != nil {
		return fmt.Errorf("gosmo: set recovery model: %w", err)
	}
	setIfApplied(ctx, &d.RecoveryModel, model)
	return nil
}

// SetCompatibilityLevel changes the database compatibility level.
func (d *Database) SetCompatibilityLevel(ctx context.Context, level CompatibilityLevel) error {
	if err := d.server.exec(ctx,
		fmt.Sprintf("ALTER DATABASE %s SET COMPATIBILITY_LEVEL = %d", quoteIdent(d.Name), level),
	); err != nil {
		return fmt.Errorf("gosmo: set compatibility level: %w", err)
	}
	setIfApplied(ctx, &d.CompatibilityLevel, level)
	return nil
}

// Termination says what an ALTER DATABASE needing exclusive access does about
// the other sessions in the database — the WITH <termination> clause of ALTER
// DATABASE SET.
type Termination int

const (
	// TerminationNone waits for the other sessions to leave, as the bare
	// statement does. Nothing bounds the wait but the caller's context: WITH
	// NO_WAIT was probed on 17.0 and still waited, so it is not offered.
	TerminationNone Termination = iota

	// TerminationRollbackImmediate disconnects every other session in the
	// database and rolls back its open transaction, so the statement finishes
	// now. Those sessions' uncommitted work is lost.
	TerminationRollbackImmediate
)

// withClause is t as the suffix of an ALTER DATABASE SET statement.
func (t Termination) withClause() (string, error) {
	switch t {
	case TerminationNone:
		return "", nil
	case TerminationRollbackImmediate:
		return " WITH ROLLBACK IMMEDIATE", nil
	}
	return "", fmt.Errorf("unrecognized termination %d", t)
}

// SetReadOnly sets the database to read-only or read-write. Either needs
// exclusive access to the database, so term says what happens to the other
// sessions in it; this Server's own idle sessions are released first either
// way (see Server.ReleaseIdleConnections).
func (d *Database) SetReadOnly(ctx context.Context, readOnly bool, term Termination) error {
	mode := "READ_WRITE"
	if readOnly {
		mode = "READ_ONLY"
	}
	with, err := term.withClause()
	if err != nil {
		return fmt.Errorf("gosmo: set read-only %v: %w", readOnly, err)
	}
	d.server.releaseIdle(ctx)
	if err := d.server.exec(ctx,
		fmt.Sprintf("ALTER DATABASE %s SET %s%s", quoteIdent(d.Name), mode, with),
	); err != nil {
		return fmt.Errorf("gosmo: set read-only %v: %w", readOnly, err)
	}
	setIfApplied(ctx, &d.IsReadOnly, readOnly)
	return nil
}

// UserAccess is a database's user-access mode, spelled as ALTER DATABASE SET
// takes it and sys.databases.user_access_desc reports it.
type UserAccess string

const (
	UserAccessMulti      UserAccess = "MULTI_USER"
	UserAccessSingle     UserAccess = "SINGLE_USER"
	UserAccessRestricted UserAccess = "RESTRICTED_USER"
)

// userAccessModes is UserAccess's validity check. The keyword can't be
// identifier-quoted or parameterised (ALTER DATABASE is DDL), so a value
// outside the constants — a conversion from an arbitrary string — is refused
// here rather than spliced in.
var userAccessModes = map[UserAccess]bool{
	UserAccessMulti: true, UserAccessSingle: true, UserAccessRestricted: true,
}

// SetUserAccess changes the database's user-access mode (MULTI_USER,
// SINGLE_USER, or RESTRICTED_USER — SSMS's Database Properties > Options
// "Restrict access" setting). Existing connections that would violate the
// new mode are rolled back immediately, matching SSMS's own behavior.
func (d *Database) SetUserAccess(ctx context.Context, mode UserAccess) error {
	if !userAccessModes[mode] {
		return fmt.Errorf("gosmo: set user access: unrecognized mode %q", mode)
	}
	if err := d.server.exec(ctx,
		fmt.Sprintf("ALTER DATABASE %s SET %s WITH ROLLBACK IMMEDIATE", quoteIdent(d.Name), mode),
	); err != nil {
		return fmt.Errorf("gosmo: set user access %s: %w", mode, err)
	}
	return nil
}

// SetOffline takes the database offline.
//
// Existing connections are rolled back immediately, matching SSMS's Object
// Explorer "Take Database Offline" behavior.
func (d *Database) SetOffline(ctx context.Context) error {
	if err := d.server.exec(ctx,
		fmt.Sprintf("ALTER DATABASE %s SET OFFLINE WITH ROLLBACK IMMEDIATE", quoteIdent(d.Name)),
	); err != nil {
		return fmt.Errorf("gosmo: set offline: %w", err)
	}
	setIfApplied(ctx, &d.State, "OFFLINE")
	return nil
}

// SetOnline brings an offline database back online.
func (d *Database) SetOnline(ctx context.Context) error {
	if err := d.server.exec(ctx,
		fmt.Sprintf("ALTER DATABASE %s SET ONLINE", quoteIdent(d.Name)),
	); err != nil {
		return fmt.Errorf("gosmo: set online: %w", err)
	}
	setIfApplied(ctx, &d.State, "ONLINE")
	return nil
}

// -- Triggers ------------------------------------------------------------------

// Trigger represents a DML trigger attached to a table.
type Trigger struct {
	Name       string
	TableName  string
	Schema     string
	IsEnabled  bool
	Events     []string
	Definition string
}

// Triggers returns all DML triggers in the database.
func (d *Database) Triggers(ctx context.Context) ([]*Trigger, error) {
	return d.triggersWhere(ctx, "", nil)
}

func (d *Database) triggersWhere(ctx context.Context, where string, args []any) ([]*Trigger, error) {
	q := `
SELECT tr.name, OBJECT_NAME(tr.parent_id), SCHEMA_NAME(o.schema_id),
       tr.is_disabled,
       ` + jsonList("te.type_desc", `
        FROM   sys.trigger_events te
        WHERE  te.object_id = tr.object_id`, "") + ` AS events,
       ISNULL(m.definition, '')
FROM   sys.triggers tr
JOIN   sys.objects o   ON o.object_id  = tr.parent_id
JOIN   sys.sql_modules m ON m.object_id = tr.object_id
WHERE  tr.is_ms_shipped = 0 AND tr.parent_class = 1 ` + where + `
ORDER  BY tr.name`

	rows, err := d.query(ctx, q, args...)
	return scanRows(rows, err, fmt.Sprintf("list triggers in %q", d.Name), func(scan func(...any) error) (*Trigger, error) {
		t := &Trigger{}
		var events sql.NullString
		var isDisabled bool
		if err := scan(&t.Name, &t.TableName, &t.Schema, &isDisabled,
			&events, &t.Definition); err != nil {
			return nil, err
		}
		t.IsEnabled = !isDisabled
		var err error
		if t.Events, err = decodeJSONList(events); err != nil {
			return nil, err
		}
		return t, nil
	})
}

// DropTrigger drops a DML trigger. schema is the trigger's own schema —
// the schema of the table it is defined on. A trigger that isn't there is the
// server's error, not a silent success — see the note on Database.DropTable.
func (d *Database) DropTrigger(ctx context.Context, schema, name string) error {
	if err := requireSchema("drop trigger", schema, name); err != nil {
		return err
	}
	if _, err := d.exec(ctx, "DROP TRIGGER "+qualifiedName(schema, name)); err != nil {
		return fmt.Errorf("gosmo: drop trigger [%s].[%s]: %w", schema, name, err)
	}
	return nil
}

// TransferObject moves a schema-scoped object into another schema
// (ALTER SCHEMA ... TRANSFER), which is the operation sp_rename cannot do —
// a rename takes a bare name and never crosses schemas.
//
// The object keeps its name and its object_id; permissions granted on it
// directly are dropped by the server, which is the documented behaviour of
// ALTER SCHEMA TRANSFER and the reason it is not a cosmetic change. An empty
// schema means dbo, as everywhere else here.
//
// This is sp_rename's default 'OBJECT' class: tables, views, procedures,
// functions, sequences and synonyms. A type or an XML schema collection needs
// TRANSFER's own class prefix and is not covered.
func (d *Database) TransferObject(ctx context.Context, targetSchema, schema, name string) error {
	if err := requireSchema("transfer object", schema, name); err != nil {
		return err
	}
	if err := requireSchema("transfer object", targetSchema, name); err != nil {
		return err
	}
	if targetSchema == "" {
		return fmt.Errorf("gosmo: transfer %s: target schema is required", qualifiedName(schema, name))
	}
	if err := d.refuseSameSchemaTransfer(ctx, targetSchema, schema, name); err != nil {
		return err
	}
	if _, err := d.exec(ctx, fmt.Sprintf("ALTER SCHEMA %s TRANSFER %s",
		quoteIdent(targetSchema), qualifiedName(schema, name))); err != nil {
		return fmt.Errorf("gosmo: transfer %s to schema [%s]: %w", qualifiedName(schema, name), targetSchema, err)
	}
	return nil
}

// refuseSameSchemaTransfer is the refusal TransferObject and
// transferWithClass share. A same-schema transfer is not a no-op at the
// server — it still drops the permissions granted directly on the object —
// so it is refused rather than sent.
//
// Names that differ only in case are one schema under a case-insensitive
// collation and two under a case-sensitive one, so only the server can say
// which; that case alone costs a round trip. Comparing case-blind here
// refused a legitimate [sales] → [Sales] transfer in a _CS_ database.
func (d *Database) refuseSameSchemaTransfer(ctx context.Context, targetSchema, schema, name string) error {
	same := targetSchema == schema
	if !same && strings.EqualFold(targetSchema, schema) {
		err := d.queryRow(ctx, func(row *sql.Row) error { return row.Scan(&same) },
			`SELECT CAST(CASE WHEN SCHEMA_ID(@p1) = SCHEMA_ID(@p2) THEN 1 ELSE 0 END AS bit)`, targetSchema, schema)
		if err != nil {
			return fmt.Errorf("gosmo: transfer %s to schema [%s]: %w", qualifiedName(schema, name), targetSchema, err)
		}
	}
	if same {
		return fmt.Errorf("gosmo: transfer %s: it is already in schema [%s]", qualifiedName(schema, name), schema)
	}
	return nil
}

// RenameObject renames any schema-scoped object sp_rename's default
// 'OBJECT' type covers — a view, procedure, function, sequence, synonym, or
// trigger. A table is the same statement with its own wording; see
// RenameTable. An index, statistic, or column each needs its own @objtype
// and has its own method.
//
// newName is a bare name: sp_rename refuses a qualified one, and renaming
// does not move the object between schemas (ALTER SCHEMA ... TRANSFER does).
func (d *Database) RenameObject(ctx context.Context, schema, oldName, newName string) error {
	if err := requireSchema("rename object", schema, oldName); err != nil {
		return err
	}
	if _, err := d.exec(ctx,
		"EXEC sp_rename @objname = @p1, @newname = @p2, @objtype = N'OBJECT'",
		qualifiedName(schema, oldName), newName,
	); err != nil {
		return fmt.Errorf("gosmo: rename %s -> %q: %w", qualifiedName(schema, oldName), newName, err)
	}
	return nil
}
