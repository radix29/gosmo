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
type Database struct {
	server        *Server
	name          string
	id            int
	state         string
	recoveryModel RecoveryModel
	compatLevel   CompatibilityLevel
	collation     string
	isReadOnly    bool
	createDate    time.Time
}

// systemDatabaseMaxID is the highest database_id SQL Server permanently
// reserves for its own system databases: master=1, tempdb=2, model=3,
// msdb=4. Every user database gets an id above this range.
const systemDatabaseMaxID = 4

// Name returns the database name.
func (d *Database) Name() string { return d.name }

// ID returns the database_id from sys.databases.
func (d *Database) ID() int { return d.id }

// IsSystem reports whether this is one of SQL Server's four built-in
// system databases (master, tempdb, model, msdb), identified by their
// permanently reserved database_id (1-4) rather than by name.
func (d *Database) IsSystem() bool { return d.id > 0 && d.id <= systemDatabaseMaxID }

// State returns the state_desc (ONLINE, OFFLINE, RESTORING ...).
func (d *Database) State() string { return d.state }

// RecoveryModel returns the database recovery model.
func (d *Database) RecoveryModel() RecoveryModel { return d.recoveryModel }

// CompatibilityLevel returns the database compatibility level.
func (d *Database) CompatibilityLevel() CompatibilityLevel { return d.compatLevel }

// Collation returns the database collation name.
func (d *Database) Collation() string { return d.collation }

// IsReadOnly reports whether the database is set to read-only.
func (d *Database) IsReadOnly() bool { return d.isReadOnly }

// CreateDate returns the date the database was created.
func (d *Database) CreateDate() time.Time { return d.createDate }

// Server returns the parent Server.
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
		if _, err := conn.ExecContext(ctx, "USE "+quoteIdent(d.name)); err != nil {
			conn.Close()
			return nil, fmt.Errorf("gosmo: USE %s: %w", d.name, err)
		}
		return conn, nil
	})
	if err != nil {
		return err
	}
	defer conn.Close()
	return fn(conn)
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
		c.append("USE " + quoteIdent(d.name) + ";\n" + bound)
		return scriptResult{}, nil
	}
	var res sql.Result
	err := d.withConn(ctx, func(c *sql.Conn) error {
		var e error
		res, e = c.ExecContext(ctx, q, args...)
		return e
	})
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
		if _, err := conn.ExecContext(ctx, "USE "+quoteIdent(d.name)); err != nil {
			conn.Close()
			return nil, fmt.Errorf("gosmo: USE %s: %w", d.name, err)
		}
		rows, err := conn.QueryContext(ctx, q, args...)
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
func (d *Database) queryRow(ctx context.Context, scan func(*sql.Row) error, q string, args ...any) error {
	_, err := withRetry(ctx, func() (struct{}, error) {
		conn, err := d.server.db.Conn(ctx)
		if err != nil {
			return struct{}{}, fmt.Errorf("gosmo: acquire connection: %w", err)
		}
		defer conn.Close()
		if _, err := conn.ExecContext(ctx, "USE "+quoteIdent(d.name)); err != nil {
			return struct{}{}, fmt.Errorf("gosmo: USE %s: %w", d.name, err)
		}
		return struct{}{}, scan(conn.QueryRowContext(ctx, q, args...))
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
func (d *Database) SpaceUsed() (SpaceInfo, error) {
	return d.SpaceUsedContext(context.Background())
}

// SpaceUsedContext is the context-aware variant of SpaceUsed.
func (d *Database) SpaceUsedContext(ctx context.Context) (SpaceInfo, error) {
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

// -- Schemas -------------------------------------------------------------------

// Schemas returns all schemas in the database.
func (d *Database) Schemas() ([]*Schema, error) {
	return d.SchemasContext(context.Background())
}

// SchemasContext is the context-aware variant of Schemas.
func (d *Database) SchemasContext(ctx context.Context) ([]*Schema, error) {
	const q = `
SELECT s.name, s.schema_id, p.name AS owner
FROM   sys.schemas s
JOIN   sys.database_principals p ON p.principal_id = s.principal_id
ORDER  BY s.name`

	rows, err := d.query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("gosmo: list schemas in %q: %w", d.name, err)
	}
	defer rows.Close()

	var schemas []*Schema
	for rows.Next() {
		sc := &Schema{db: d}
		if err := rows.Scan(&sc.Name, &sc.ID, &sc.Owner); err != nil {
			return nil, fmt.Errorf("gosmo: list schemas in %q: %w", d.name, err)
		}
		schemas = append(schemas, sc)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("gosmo: list schemas in %q: %w", d.name, err)
	}
	return schemas, nil
}

// CreateSchema creates a new schema in the database.
func (d *Database) CreateSchema(name, owner string) error {
	return d.CreateSchemaContext(context.Background(), name, owner)
}

// CreateSchemaContext is the context-aware variant of CreateSchema.
func (d *Database) CreateSchemaContext(ctx context.Context, name, owner string) error {
	if name == "" {
		return fmt.Errorf("gosmo: create schema: name is required")
	}
	q := "CREATE SCHEMA " + quoteIdent(name)
	if owner != "" {
		q += " AUTHORIZATION " + quoteIdent(owner)
	}
	if _, err := d.exec(ctx, q); err != nil {
		return fmt.Errorf("gosmo: create schema %q: %w", name, err)
	}
	return nil
}

// DropSchema drops a schema from the database.
func (d *Database) DropSchema(name string) error {
	return d.DropSchemaContext(context.Background(), name)
}

// DropSchemaContext is the context-aware variant of DropSchema.
func (d *Database) DropSchemaContext(ctx context.Context, name string) error {
	if _, err := d.exec(ctx, "DROP SCHEMA "+quoteIdent(name)); err != nil {
		return fmt.Errorf("gosmo: drop schema %q: %w", name, err)
	}
	return nil
}

// -- Tables --------------------------------------------------------------------

// Tables returns all user tables in the database.
func (d *Database) Tables() ([]*Table, error) {
	return d.TablesContext(context.Background())
}

// TablesContext is the context-aware variant of Tables.
func (d *Database) TablesContext(ctx context.Context) ([]*Table, error) {
	return d.tablesWhere(ctx, "", nil)
}

// TablesBySchema returns all tables in a specific schema.
func (d *Database) TablesBySchema(schema string) ([]*Table, error) {
	return d.TablesBySchemaContext(context.Background(), schema)
}

// TablesBySchemaContext is the context-aware variant of TablesBySchema.
func (d *Database) TablesBySchemaContext(ctx context.Context, schema string) ([]*Table, error) {
	return d.tablesWhere(ctx, "AND SCHEMA_NAME(t.schema_id) = @p1", []any{schema})
}

func (d *Database) tablesWhere(ctx context.Context, where string, args []any) ([]*Table, error) {
	q := `
SELECT t.object_id, SCHEMA_NAME(t.schema_id), t.name,
       t.create_date, t.modify_date,
       t.has_replication_filter, t.is_memory_optimized
FROM   sys.tables t
WHERE  t.is_ms_shipped = 0 ` + where + `
ORDER  BY SCHEMA_NAME(t.schema_id), t.name`

	rows, err := d.query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("gosmo: list tables in %q: %w", d.name, err)
	}
	defer rows.Close()

	var tables []*Table
	for rows.Next() {
		t := &Table{db: d}
		if err := rows.Scan(&t.ObjectID, &t.Schema, &t.Name,
			&t.CreateDate, &t.ModifyDate,
			&t.HasReplicationFilter, &t.IsMemoryOptimized); err != nil {
			return nil, fmt.Errorf("gosmo: list tables in %q: %w", d.name, err)
		}
		tables = append(tables, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("gosmo: list tables in %q: %w", d.name, err)
	}
	return tables, nil
}

// TableByName returns a single table by schema and name using a direct query.
func (d *Database) TableByName(schema, name string) (*Table, error) {
	return d.TableByNameContext(context.Background(), schema, name)
}

// TableByNameContext is the context-aware variant of TableByName.
func (d *Database) TableByNameContext(ctx context.Context, schema, name string) (*Table, error) {
	const q = `
SELECT t.object_id, SCHEMA_NAME(t.schema_id), t.name,
       t.create_date, t.modify_date,
       t.has_replication_filter, t.is_memory_optimized
FROM   sys.tables t
WHERE  t.is_ms_shipped = 0
  AND  SCHEMA_NAME(t.schema_id) = @p1
  AND  t.name                   = @p2`

	t := &Table{db: d}
	err := d.queryRow(ctx, func(row *sql.Row) error {
		return row.Scan(&t.ObjectID, &t.Schema, &t.Name,
			&t.CreateDate, &t.ModifyDate,
			&t.HasReplicationFilter, &t.IsMemoryOptimized)
	}, q, schema, name)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, notFoundf("gosmo: table [%s].[%s] not found in %q", schema, name, d.name)
		}
		return nil, err
	}
	return t, nil
}

// -- Database users ------------------------------------------------------------

// Users returns all database users.
func (d *Database) Users() ([]*User, error) {
	return d.UsersContext(context.Background())
}

// UsersContext is the context-aware variant of Users.
func (d *Database) UsersContext(ctx context.Context) ([]*User, error) {
	const q = `
SELECT name, principal_id, type_desc, default_schema_name,
       create_date, modify_date, authentication_type_desc
FROM   sys.database_principals
WHERE  type IN ('S','U','G')
ORDER  BY name`

	rows, err := d.query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("gosmo: list users in %q: %w", d.name, err)
	}
	defer rows.Close()

	var users []*User
	for rows.Next() {
		u := &User{db: d}
		var defSchema, authType sql.NullString
		if err := rows.Scan(&u.Name, &u.ID, &u.UserType, &defSchema,
			&u.CreateDate, &u.ModifyDate, &authType); err != nil {
			return nil, fmt.Errorf("gosmo: list users in %q: %w", d.name, err)
		}
		u.DefaultSchema = defSchema.String
		u.AuthType = authType.String
		users = append(users, u)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("gosmo: list users in %q: %w", d.name, err)
	}
	return users, nil
}

// UserByName returns a single database user by name, with its SID and
// matching server login (if any) filled in — UsersContext leaves these
// out since Object Explorer's tree listing never needs them.
func (d *Database) UserByName(name string) (*User, error) {
	return d.UserByNameContext(context.Background(), name)
}

// UserByNameContext is the context-aware variant of UserByName.
func (d *Database) UserByNameContext(ctx context.Context, name string) (*User, error) {
	const q = `
SELECT dp.principal_id, dp.type_desc, dp.default_schema_name,
       dp.create_date, dp.modify_date, dp.authentication_type_desc, dp.sid,
       sp.name, sp.is_disabled
FROM   sys.database_principals dp
LEFT   JOIN sys.server_principals sp ON sp.sid = dp.sid
WHERE  dp.type IN ('S','U','G') AND dp.name = @p1`

	u := &User{db: d, Name: name}
	var defSchema, authType, loginName sql.NullString
	var loginDisabled sql.NullBool
	err := d.queryRow(ctx, func(row *sql.Row) error {
		return row.Scan(&u.ID, &u.UserType, &defSchema, &u.CreateDate, &u.ModifyDate,
			&authType, &u.SID, &loginName, &loginDisabled)
	}, q, name)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, notFoundf("gosmo: database user %q not found in %q", name, d.name)
		}
		return nil, fmt.Errorf("gosmo: find database user %q in %q: %w", name, d.name, err)
	}
	u.DefaultSchema = defSchema.String
	u.AuthType = authType.String
	u.LoginName = loginName.String
	u.LoginDisabled = loginDisabled.Bool
	return u, nil
}

// CreateUser creates a database user mapped to a login.
func (d *Database) CreateUser(userName, loginName, defaultSchema string) error {
	return d.CreateUserContext(context.Background(), userName, loginName, defaultSchema)
}

// CreateUserContext is the context-aware variant of CreateUser.
func (d *Database) CreateUserContext(ctx context.Context, userName, loginName, defaultSchema string) error {
	if userName == "" {
		return fmt.Errorf("gosmo: create user: user name is required")
	}
	q := fmt.Sprintf("CREATE USER %s FOR LOGIN %s", quoteIdent(userName), quoteIdent(loginName))
	if defaultSchema != "" {
		q += " WITH DEFAULT_SCHEMA = " + quoteIdent(defaultSchema)
	}
	if _, err := d.exec(ctx, q); err != nil {
		return fmt.Errorf("gosmo: create user %q: %w", userName, err)
	}
	return nil
}

// DropUser drops a database user.
func (d *Database) DropUser(name string) error {
	return d.DropUserContext(context.Background(), name)
}

// DropUserContext is the context-aware variant of DropUser.
func (d *Database) DropUserContext(ctx context.Context, name string) error {
	if _, err := d.exec(ctx, "DROP USER "+quoteIdent(name)); err != nil {
		return fmt.Errorf("gosmo: drop user %q: %w", name, err)
	}
	return nil
}

// -- Settings ------------------------------------------------------------------

// SetRecoveryModel changes the database recovery model.
func (d *Database) SetRecoveryModel(model RecoveryModel) error {
	return d.SetRecoveryModelContext(context.Background(), model)
}

// SetRecoveryModelContext is the context-aware variant.
func (d *Database) SetRecoveryModelContext(ctx context.Context, model RecoveryModel) error {
	if !validRecoveryModel(model) {
		return fmt.Errorf("gosmo: set recovery model: unrecognized recovery model %q", model)
	}
	if err := d.server.execContext(ctx,
		fmt.Sprintf("ALTER DATABASE %s SET RECOVERY %s", quoteIdent(d.name), model),
	); err != nil {
		return fmt.Errorf("gosmo: set recovery model: %w", err)
	}
	setIfApplied(ctx, &d.recoveryModel, model)
	return nil
}

// SetCompatibilityLevel changes the database compatibility level.
func (d *Database) SetCompatibilityLevel(level CompatibilityLevel) error {
	return d.SetCompatibilityLevelContext(context.Background(), level)
}

// SetCompatibilityLevelContext is the context-aware variant.
func (d *Database) SetCompatibilityLevelContext(ctx context.Context, level CompatibilityLevel) error {
	if err := d.server.execContext(ctx,
		fmt.Sprintf("ALTER DATABASE %s SET COMPATIBILITY_LEVEL = %d", quoteIdent(d.name), level),
	); err != nil {
		return fmt.Errorf("gosmo: set compatibility level: %w", err)
	}
	setIfApplied(ctx, &d.compatLevel, level)
	return nil
}

// SetReadOnly sets the database to read-only or read-write.
func (d *Database) SetReadOnly(readOnly bool) error {
	return d.SetReadOnlyContext(context.Background(), readOnly)
}

// SetReadOnlyContext is the context-aware variant.
func (d *Database) SetReadOnlyContext(ctx context.Context, readOnly bool) error {
	mode := "READ_WRITE"
	if readOnly {
		mode = "READ_ONLY"
	}
	if err := d.server.execContext(ctx,
		fmt.Sprintf("ALTER DATABASE %s SET %s", quoteIdent(d.name), mode),
	); err != nil {
		return fmt.Errorf("gosmo: set read-only %v: %w", readOnly, err)
	}
	setIfApplied(ctx, &d.isReadOnly, readOnly)
	return nil
}

// userAccessModes allowlists the ALTER DATABASE SET user-access keywords —
// can't be identifier-quoted or parameterised (ALTER DATABASE is DDL).
var userAccessModes = map[string]bool{
	"MULTI_USER": true, "SINGLE_USER": true, "RESTRICTED_USER": true,
}

// SetUserAccess changes the database's user-access mode (MULTI_USER,
// SINGLE_USER, or RESTRICTED_USER — SSMS's Database Properties > Options
// "Restrict access" setting). Existing connections that would violate the
// new mode are rolled back immediately, matching SSMS's own behavior.
func (d *Database) SetUserAccess(mode string) error {
	return d.SetUserAccessContext(context.Background(), mode)
}

// SetUserAccessContext is the context-aware variant of SetUserAccess.
func (d *Database) SetUserAccessContext(ctx context.Context, mode string) error {
	if !userAccessModes[mode] {
		return fmt.Errorf("gosmo: set user access: unrecognized mode %q", mode)
	}
	if err := d.server.execContext(ctx,
		fmt.Sprintf("ALTER DATABASE %s SET %s WITH ROLLBACK IMMEDIATE", quoteIdent(d.name), mode),
	); err != nil {
		return fmt.Errorf("gosmo: set user access %s: %w", mode, err)
	}
	return nil
}

// SetOffline takes the database offline.
func (d *Database) SetOffline() error {
	return d.SetOfflineContext(context.Background())
}

// SetOfflineContext is the context-aware variant of SetOffline. Existing
// connections are rolled back immediately, matching SSMS's Object Explorer
// "Take Database Offline" behavior.
func (d *Database) SetOfflineContext(ctx context.Context) error {
	if err := d.server.execContext(ctx,
		fmt.Sprintf("ALTER DATABASE %s SET OFFLINE WITH ROLLBACK IMMEDIATE", quoteIdent(d.name)),
	); err != nil {
		return fmt.Errorf("gosmo: set offline: %w", err)
	}
	setIfApplied(ctx, &d.state, "OFFLINE")
	return nil
}

// SetOnline brings an offline database back online.
func (d *Database) SetOnline() error {
	return d.SetOnlineContext(context.Background())
}

// SetOnlineContext is the context-aware variant of SetOnline.
func (d *Database) SetOnlineContext(ctx context.Context) error {
	if err := d.server.execContext(ctx,
		fmt.Sprintf("ALTER DATABASE %s SET ONLINE", quoteIdent(d.name)),
	); err != nil {
		return fmt.Errorf("gosmo: set online: %w", err)
	}
	setIfApplied(ctx, &d.state, "ONLINE")
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
func (d *Database) Triggers() ([]*Trigger, error) {
	return d.TriggersContext(context.Background())
}

// TriggersContext is the context-aware variant of Triggers.
func (d *Database) TriggersContext(ctx context.Context) ([]*Trigger, error) {
	return d.triggersWhere(ctx, "", nil)
}

func (d *Database) triggersWhere(ctx context.Context, where string, args []any) ([]*Trigger, error) {
	q := `
SELECT tr.name, OBJECT_NAME(tr.parent_id), SCHEMA_NAME(o.schema_id),
       tr.is_disabled,
       (SELECT STRING_AGG(te.type_desc, ',')
        FROM   sys.trigger_events te
        WHERE  te.object_id = tr.object_id) AS events,
       m.definition
FROM   sys.triggers tr
JOIN   sys.objects o   ON o.object_id  = tr.parent_id
JOIN   sys.sql_modules m ON m.object_id = tr.object_id
WHERE  tr.is_ms_shipped = 0 AND tr.parent_class = 1 ` + where + `
ORDER  BY tr.name`

	rows, err := d.query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("gosmo: list triggers in %q: %w", d.name, err)
	}
	defer rows.Close()

	var triggers []*Trigger
	for rows.Next() {
		t := &Trigger{}
		var events sql.NullString
		var isDisabled bool
		if err := rows.Scan(&t.Name, &t.TableName, &t.Schema, &isDisabled,
			&events, &t.Definition); err != nil {
			return nil, fmt.Errorf("gosmo: list triggers in %q: %w", d.name, err)
		}
		t.IsEnabled = !isDisabled
		if events.Valid && events.String != "" {
			t.Events = strings.Split(events.String, ",")
		}
		triggers = append(triggers, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("gosmo: list triggers in %q: %w", d.name, err)
	}
	return triggers, nil
}

// DropTrigger drops a DML trigger. schema is the trigger's own schema —
// the schema of the table it is defined on. A trigger that isn't there is the
// server's error, not a silent success — see the note on Database.DropTable.
func (d *Database) DropTrigger(schema, name string) error {
	return d.DropTriggerContext(context.Background(), schema, name)
}

// DropTriggerContext is the context-aware variant of DropTrigger.
func (d *Database) DropTriggerContext(ctx context.Context, schema, name string) error {
	if schema == "" {
		schema = "dbo"
	}
	if _, err := d.exec(ctx, "DROP TRIGGER "+qualifiedName(schema, name)); err != nil {
		return fmt.Errorf("gosmo: drop trigger [%s].[%s]: %w", schema, name, err)
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
func (d *Database) RenameObject(schema, oldName, newName string) error {
	return d.RenameObjectContext(context.Background(), schema, oldName, newName)
}

// RenameObjectContext is the context-aware variant of RenameObject.
func (d *Database) RenameObjectContext(ctx context.Context, schema, oldName, newName string) error {
	if schema == "" {
		schema = "dbo"
	}
	if _, err := d.exec(ctx,
		"EXEC sp_rename @objname = @p1, @newname = @p2, @objtype = N'OBJECT'",
		qualifiedName(schema, oldName), newName,
	); err != nil {
		return fmt.Errorf("gosmo: rename %s -> %q: %w", qualifiedName(schema, oldName), newName, err)
	}
	return nil
}
