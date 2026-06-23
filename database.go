package gosmo

import (
	"context"
	"database/sql"
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

// Name returns the database name.
func (d *Database) Name() string { return d.name }

// ID returns the database_id from sys.databases.
func (d *Database) ID() int { return d.id }

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

func (d *Database) withConn(ctx context.Context, fn func(*sql.Conn) error) error {
	conn, err := d.server.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("gosmo: acquire connection: %w", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "USE "+quoteIdent(d.name)); err != nil {
		return fmt.Errorf("gosmo: USE %s: %w", d.name, err)
	}
	return fn(conn)
}

func (d *Database) exec(ctx context.Context, q string, args ...any) (sql.Result, error) {
	var res sql.Result
	err := d.withConn(ctx, func(c *sql.Conn) error {
		var e error
		res, e = c.ExecContext(ctx, q, args...)
		return e
	})
	return res, err
}

func (d *Database) query(ctx context.Context, q string, args ...any) (*sql.Rows, error) {
	// For queries that return rows we cannot use withConn (the conn would be
	// released before the caller finishes iterating). Instead we acquire a
	// dedicated conn, switch DB, run the query, and return the rows.  The
	// caller must close the rows, which releases the underlying conn automatically.
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
	return rows, nil
}

func (d *Database) queryRow(ctx context.Context, q string, args ...any) (*sql.Row, func(), error) {
	conn, err := d.server.db.Conn(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("gosmo: acquire connection: %w", err)
	}
	if _, err := conn.ExecContext(ctx, "USE "+quoteIdent(d.name)); err != nil {
		conn.Close()
		return nil, nil, fmt.Errorf("gosmo: USE %s: %w", d.name, err)
	}
	return conn.QueryRowContext(ctx, q, args...), func() { conn.Close() }, nil
}

// -- Size / space --------------------------------------------------------------

// SpaceInfo holds space usage information for a database.
type SpaceInfo struct {
	TotalMB float64
	DataMB  float64
	LogMB   float64
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
    SUM(CASE WHEN type_desc =  'LOG' THEN size ELSE 0 END) * 8.0 / 1024     AS log_mb
FROM sys.database_files`

	row, release, err := d.queryRow(ctx, q)
	if err != nil {
		return SpaceInfo{}, err
	}
	defer release()

	var si SpaceInfo
	if err := row.Scan(&si.TotalMB, &si.DataMB, &si.LogMB); err != nil {
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
			return nil, err
		}
		schemas = append(schemas, sc)
	}
	return schemas, rows.Err()
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
			return nil, err
		}
		tables = append(tables, t)
	}
	return tables, rows.Err()
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

	row, release, err := d.queryRow(ctx, q, schema, name)
	if err != nil {
		return nil, err
	}
	defer release()

	t := &Table{db: d}
	if err := row.Scan(&t.ObjectID, &t.Schema, &t.Name,
		&t.CreateDate, &t.ModifyDate,
		&t.HasReplicationFilter, &t.IsMemoryOptimized); err != nil {
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("gosmo: table [%s].[%s] not found in %q", schema, name, d.name)
		}
		return nil, err
	}
	return t, nil
}

// -- Views ---------------------------------------------------------------------

// View represents a database view.
type View struct {
	ObjectID   int
	Schema     string
	Name       string
	Definition string
	CreateDate time.Time
	ModifyDate time.Time
}

// Views returns all views in the database.
func (d *Database) Views() ([]*View, error) {
	return d.ViewsContext(context.Background())
}

// ViewsContext is the context-aware variant of Views.
func (d *Database) ViewsContext(ctx context.Context) ([]*View, error) {
	const q = `
SELECT v.object_id, SCHEMA_NAME(v.schema_id), v.name,
       ISNULL(m.definition,''), v.create_date, v.modify_date
FROM   sys.views v
JOIN   sys.sql_modules m ON m.object_id = v.object_id
WHERE  v.is_ms_shipped = 0
ORDER  BY SCHEMA_NAME(v.schema_id), v.name`

	rows, err := d.query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("gosmo: list views in %q: %w", d.name, err)
	}
	defer rows.Close()

	var views []*View
	for rows.Next() {
		v := &View{}
		if err := rows.Scan(&v.ObjectID, &v.Schema, &v.Name,
			&v.Definition, &v.CreateDate, &v.ModifyDate); err != nil {
			return nil, err
		}
		views = append(views, v)
	}
	return views, rows.Err()
}

// -- Stored procedures ---------------------------------------------------------

// StoredProcedure represents a stored procedure.
type StoredProcedure struct {
	ObjectID   int
	Schema     string
	Name       string
	Definition string
	CreateDate time.Time
	ModifyDate time.Time
}

// StoredProcedures returns all stored procedures in the database.
func (d *Database) StoredProcedures() ([]*StoredProcedure, error) {
	return d.StoredProceduresContext(context.Background())
}

// StoredProceduresContext is the context-aware variant of StoredProcedures.
func (d *Database) StoredProceduresContext(ctx context.Context) ([]*StoredProcedure, error) {
	const q = `
SELECT p.object_id, SCHEMA_NAME(p.schema_id), p.name,
       ISNULL(m.definition,''), p.create_date, p.modify_date
FROM   sys.procedures p
JOIN   sys.sql_modules m ON m.object_id = p.object_id
WHERE  p.is_ms_shipped = 0
ORDER  BY SCHEMA_NAME(p.schema_id), p.name`

	rows, err := d.query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("gosmo: list stored procs in %q: %w", d.name, err)
	}
	defer rows.Close()

	var procs []*StoredProcedure
	for rows.Next() {
		p := &StoredProcedure{}
		if err := rows.Scan(&p.ObjectID, &p.Schema, &p.Name,
			&p.Definition, &p.CreateDate, &p.ModifyDate); err != nil {
			return nil, err
		}
		procs = append(procs, p)
	}
	return procs, rows.Err()
}

// CreateStoredProcedure creates (or replaces) a stored procedure.
// schema may be empty (defaults to dbo). body is the raw T-SQL after AS.
func (d *Database) CreateStoredProcedure(schema, name, body string) error {
	return d.CreateStoredProcedureContext(context.Background(), schema, name, body)
}

// CreateStoredProcedureContext is the context-aware variant.
func (d *Database) CreateStoredProcedureContext(ctx context.Context, schema, name, body string) error {
	if name == "" {
		return fmt.Errorf("gosmo: create stored procedure: name is required")
	}
	if schema == "" {
		schema = "dbo"
	}
	q := fmt.Sprintf("CREATE OR ALTER PROCEDURE %s\nAS\n%s", qualifiedName(schema, name), body)
	if _, err := d.exec(ctx, q); err != nil {
		return fmt.Errorf("gosmo: create stored procedure [%s].[%s]: %w", schema, name, err)
	}
	return nil
}

// DropStoredProcedure drops a stored procedure.
func (d *Database) DropStoredProcedure(schema, name string) error {
	return d.DropStoredProcedureContext(context.Background(), schema, name)
}

// DropStoredProcedureContext is the context-aware variant.
func (d *Database) DropStoredProcedureContext(ctx context.Context, schema, name string) error {
	if schema == "" {
		schema = "dbo"
	}
	if _, err := d.exec(ctx, "DROP PROCEDURE IF EXISTS "+qualifiedName(schema, name)); err != nil {
		return fmt.Errorf("gosmo: drop stored procedure [%s].[%s]: %w", schema, name, err)
	}
	return nil
}

// -- User-defined functions -----------------------------------------------------

// UserDefinedFunction represents a UDF.
type UserDefinedFunction struct {
	ObjectID   int
	Schema     string
	Name       string
	FuncType   string // "FN" scalar, "TF" multi-statement table-valued, "IF" inline table-valued
	Definition string
	CreateDate time.Time
	ModifyDate time.Time
}

// UserDefinedFunctions returns all UDFs in the database.
func (d *Database) UserDefinedFunctions() ([]*UserDefinedFunction, error) {
	return d.UserDefinedFunctionsContext(context.Background())
}

// UserDefinedFunctionsContext is the context-aware variant.
func (d *Database) UserDefinedFunctionsContext(ctx context.Context) ([]*UserDefinedFunction, error) {
	const q = `
SELECT o.object_id, SCHEMA_NAME(o.schema_id), o.name, o.type,
       ISNULL(m.definition,''), o.create_date, o.modify_date
FROM   sys.objects o
JOIN   sys.sql_modules m ON m.object_id = o.object_id
WHERE  o.type IN ('FN','TF','IF') AND o.is_ms_shipped = 0
ORDER  BY SCHEMA_NAME(o.schema_id), o.name`

	rows, err := d.query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("gosmo: list UDFs in %q: %w", d.name, err)
	}
	defer rows.Close()

	var funcs []*UserDefinedFunction
	for rows.Next() {
		f := &UserDefinedFunction{}
		if err := rows.Scan(&f.ObjectID, &f.Schema, &f.Name, &f.FuncType,
			&f.Definition, &f.CreateDate, &f.ModifyDate); err != nil {
			return nil, err
		}
		f.FuncType = strings.TrimSpace(f.FuncType)
		funcs = append(funcs, f)
	}
	return funcs, rows.Err()
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
			return nil, err
		}
		u.DefaultSchema = defSchema.String
		u.AuthType = authType.String
		users = append(users, u)
	}
	return users, rows.Err()
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

// -- Database roles ------------------------------------------------------------

// DatabaseRole represents a database-level role.
type DatabaseRole struct {
	db          *Database
	Name        string
	ID          int
	IsFixedRole bool
	Owner       string
	Members     []string
}

// DatabaseRoles returns all roles defined in the database.
func (d *Database) DatabaseRoles() ([]*DatabaseRole, error) {
	return d.DatabaseRolesContext(context.Background())
}

// DatabaseRolesContext is the context-aware variant of DatabaseRoles.
func (d *Database) DatabaseRolesContext(ctx context.Context) ([]*DatabaseRole, error) {
	const q = `
SELECT r.name, r.principal_id, r.is_fixed_role, p.name AS owner,
       STUFF((SELECT ', ' + m.name
              FROM   sys.database_role_members rm
              JOIN   sys.database_principals m ON m.principal_id = rm.member_principal_id
              WHERE  rm.role_principal_id = r.principal_id
              FOR XML PATH(''), TYPE).value('.','NVARCHAR(MAX)'), 1, 2, '') AS members
FROM   sys.database_principals r
JOIN   sys.database_principals p ON p.principal_id = r.owning_principal_id
WHERE  r.type = 'R'
ORDER  BY r.name`

	rows, err := d.query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("gosmo: list database roles in %q: %w", d.name, err)
	}
	defer rows.Close()

	var roles []*DatabaseRole
	for rows.Next() {
		r := &DatabaseRole{db: d}
		var members sql.NullString
		if err := rows.Scan(&r.Name, &r.ID, &r.IsFixedRole, &r.Owner, &members); err != nil {
			return nil, err
		}
		if members.Valid && members.String != "" {
			r.Members = strings.Split(members.String, ", ")
		}
		roles = append(roles, r)
	}
	return roles, rows.Err()
}

// AddRoleMember adds a user to a database role.
func (d *Database) AddRoleMember(roleName, memberName string) error {
	return d.AddRoleMemberContext(context.Background(), roleName, memberName)
}

// AddRoleMemberContext is the context-aware variant.
func (d *Database) AddRoleMemberContext(ctx context.Context, roleName, memberName string) error {
	if _, err := d.exec(ctx,
		fmt.Sprintf("ALTER ROLE %s ADD MEMBER %s", quoteIdent(roleName), quoteIdent(memberName)),
	); err != nil {
		return fmt.Errorf("gosmo: add %q to role %q: %w", memberName, roleName, err)
	}
	return nil
}

// RemoveRoleMember removes a user from a database role.
func (d *Database) RemoveRoleMember(roleName, memberName string) error {
	return d.RemoveRoleMemberContext(context.Background(), roleName, memberName)
}

// RemoveRoleMemberContext is the context-aware variant.
func (d *Database) RemoveRoleMemberContext(ctx context.Context, roleName, memberName string) error {
	if _, err := d.exec(ctx,
		fmt.Sprintf("ALTER ROLE %s DROP MEMBER %s", quoteIdent(roleName), quoteIdent(memberName)),
	); err != nil {
		return fmt.Errorf("gosmo: remove %q from role %q: %w", memberName, roleName, err)
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
	if _, err := d.server.db.ExecContext(ctx,
		fmt.Sprintf("ALTER DATABASE %s SET RECOVERY %s", quoteIdent(d.name), model),
	); err != nil {
		return fmt.Errorf("gosmo: set recovery model: %w", err)
	}
	d.recoveryModel = model
	return nil
}

// SetCompatibilityLevel changes the database compatibility level.
func (d *Database) SetCompatibilityLevel(level CompatibilityLevel) error {
	return d.SetCompatibilityLevelContext(context.Background(), level)
}

// SetCompatibilityLevelContext is the context-aware variant.
func (d *Database) SetCompatibilityLevelContext(ctx context.Context, level CompatibilityLevel) error {
	if _, err := d.server.db.ExecContext(ctx,
		fmt.Sprintf("ALTER DATABASE %s SET COMPATIBILITY_LEVEL = %d", quoteIdent(d.name), level),
	); err != nil {
		return fmt.Errorf("gosmo: set compatibility level: %w", err)
	}
	d.compatLevel = level
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
	if _, err := d.server.db.ExecContext(ctx,
		fmt.Sprintf("ALTER DATABASE %s SET %s", quoteIdent(d.name), mode),
	); err != nil {
		return fmt.Errorf("gosmo: set read-only %v: %w", readOnly, err)
	}
	d.isReadOnly = readOnly
	return nil
}

// -- Filegroups ----------------------------------------------------------------

// FileGroups returns all filegroups and their files.
func (d *Database) FileGroups() ([]*FileGroup, error) {
	return d.FileGroupsContext(context.Background())
}

// FileGroupsContext is the context-aware variant of FileGroups.
func (d *Database) FileGroupsContext(ctx context.Context) ([]*FileGroup, error) {
	const q = `
SELECT fg.name, fg.is_default,
       df.name, df.physical_name, df.size * 8, df.max_size, df.growth,
       df.is_percent_growth,
       CASE WHEN df.file_id = 1 THEN 1 ELSE 0 END AS is_primary
FROM   sys.filegroups fg
JOIN   sys.database_files df ON df.data_space_id = fg.data_space_id
ORDER  BY fg.name, df.file_id`

	rows, err := d.query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("gosmo: list filegroups: %w", err)
	}
	defer rows.Close()

	fgMap := make(map[string]*FileGroup)
	var order []string
	for rows.Next() {
		var fgName string
		var fgDefault, isPctGrowth, isPrimary bool
		f := DatabaseFile{}
		if err := rows.Scan(&fgName, &fgDefault,
			&f.Name, &f.PhysicalName, &f.Size, &f.MaxSize, &f.Growth,
			&isPctGrowth, &isPrimary); err != nil {
			return nil, err
		}
		if isPctGrowth {
			f.GrowthType = "PERCENT"
		} else {
			f.GrowthType = "KB"
		}
		f.IsPrimaryFile = isPrimary
		f.FileGroupName = fgName

		fg, ok := fgMap[fgName]
		if !ok {
			fg = &FileGroup{Name: fgName, IsDefault: fgDefault}
			fgMap[fgName] = fg
			order = append(order, fgName)
		}
		fg.Files = append(fg.Files, f)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	fgs := make([]*FileGroup, 0, len(order))
	for _, n := range order {
		fgs = append(fgs, fgMap[n])
	}
	return fgs, nil
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
	const q = `
SELECT tr.name, OBJECT_NAME(tr.parent_id), SCHEMA_NAME(o.schema_id),
       tr.is_disabled,
       (SELECT STRING_AGG(te.type_desc, ',')
        FROM   sys.trigger_events te
        WHERE  te.object_id = tr.object_id) AS events,
       m.definition
FROM   sys.triggers tr
JOIN   sys.objects o   ON o.object_id  = tr.parent_id
JOIN   sys.sql_modules m ON m.object_id = tr.object_id
WHERE  tr.is_ms_shipped = 0 AND tr.parent_class = 1
ORDER  BY tr.name`

	rows, err := d.query(ctx, q)
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
			return nil, err
		}
		t.IsEnabled = !isDisabled
		if events.Valid && events.String != "" {
			t.Events = strings.Split(events.String, ",")
		}
		triggers = append(triggers, t)
	}
	return triggers, rows.Err()
}
