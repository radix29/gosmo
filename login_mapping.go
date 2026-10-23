package gosmo

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// -- User mapping --------------------------------------------------------------

// LoginUserMapping describes one database this login is mapped into —
// SSMS's Login Properties > User Mapping page.
type LoginUserMapping struct {
	Database      string
	User          string
	DefaultSchema string
	Roles         []string
}

// UserMappings returns every database this login has a mapped user in.
// Only mapped databases are included — combine with Server.Databases to
// build a full "all databases, mapped or not" view. Databases that are
// offline, or that the login can't currently reach, are skipped rather
// than failing the whole scan (SSMS's own User Mapping page behaves the
// same way); any other error in a database fails it, so a broken read is not
// shown as "unmapped". A cancelled context is not skipped — it ends the scan and is
// returned.
//
// It is two round trips whatever the database count: the database list,
// then one batch that reads every ONLINE database in turn, each inside its
// own TRY/CATCH — the per-database skip, made server-side. The read runs
// as dynamic SQL in the database (its sp_executesql, named through a
// variable — perDatabaseExec says why), so a database that cannot be
// entered fails at that EXEC, inside the TRY, rather than at the batch's
// compile. A per-database query each was the
// shape until 2026-10-03; fanning those across a worker pool was tried
// and measured slower (2026-08-14), since every worker on a cold pool
// pays a full login handshake.
//
// A failure reading the batch's rows is not skipped: rows already read are
// in the result, so skipping would return a short list and call it success.
//
// On a LoginRef the SID is looked up by name first.
func (l *Login) UserMappings(ctx context.Context) ([]*LoginUserMapping, error) {
	sid, _, err := l.identity(ctx, fmt.Sprintf("user mappings for login %q", l.Name))
	if err != nil {
		return nil, err
	}
	dbs, err := l.server.Databases(ctx)
	if err != nil {
		return nil, err
	}
	return l.userMappingsBatch(ctx, userMappingsQuery, sid, dbs)
}

// UserMappingsIn is UserMappings over databases the caller has already
// listed — a User Mapping page that shows every database reads the list once
// for both, so the mappings are one round trip (two on a LoginRef, whose SID
// is looked up first). A database in dbs that is not ONLINE, or that cannot
// be entered, is skipped as UserMappings skips it.
func (l *Login) UserMappingsIn(ctx context.Context, dbs []*Database) ([]*LoginUserMapping, error) {
	sid, _, err := l.identity(ctx, fmt.Sprintf("user mappings for login %q", l.Name))
	if err != nil {
		return nil, err
	}
	return l.userMappingsBatch(ctx, userMappingsQuery, sid, dbs)
}

// userMappingsBatch is the batch UserMappings and UserMappingsIn share: q is
// userMappingsQuery, a parameter only so a live test can break it.
func (l *Login) userMappingsBatch(ctx context.Context, q string, sid []byte, dbs []*Database) ([]*LoginUserMapping, error) {
	var sb strings.Builder
	sb.WriteString("SET NOCOUNT ON;\nDECLARE @proc nvarchar(300), @q nvarchar(max) = " + QuoteLiteral(q) + ";\n")
	n := 0
	for _, db := range dbs {
		if db.State != DatabaseOnline {
			continue
		}
		sb.WriteString(perDatabaseExec(db.Name, ", N'@p1 varbinary(85)', @p1 = @sid"))
		n++
	}
	if n == 0 {
		return nil, nil
	}
	// The SID goes in as @sid, renamed for the batch: @p1 is the name each
	// database's query binds it under.
	batch := "DECLARE @sid varbinary(85) = @p1;\n" + sb.String()
	rows, err := l.server.query(ctx, batch, sid)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("gosmo: user mappings for login %q: %w", l.Name, err)
	}
	defer rows.Close()

	var out []*LoginUserMapping
	for {
		ms, err := scanUserMappings(rows)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, fmt.Errorf("gosmo: user mappings for login %q: %w", l.Name, err)
		}
		out = append(out, ms...)
		if !rows.NextResultSet() {
			break
		}
	}
	if err := rows.Err(); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("gosmo: user mappings for login %q: %w", l.Name, err)
	}
	return out, nil
}

// userMappingsQuery reads a login's user in the current database with one row
// per role it is a member of — none when it is in no role, hence the LEFT
// JOINs — ordered so scanUserMappings can group the rows in Go. The database
// is a column, so rows from several databases group apart however they are
// split into result sets.
//
// The roles were a comma-joined string (STUFF ... FOR XML PATH) split back
// apart on ", " until 2026-09-23, which split a role whose name held a comma
// into two roles that do not exist.
const userMappingsQuery = `
SELECT DB_NAME(), dp.principal_id, dp.name, ISNULL(dp.default_schema_name, ''), r.name
FROM   sys.database_principals dp
LEFT   JOIN sys.database_role_members rm ON rm.member_principal_id = dp.principal_id
LEFT   JOIN sys.database_principals r ON r.principal_id = rm.role_principal_id
WHERE  dp.sid = @p1
ORDER  BY dp.principal_id, r.name`

// scanUserMappings drains the current result set of userMappingsQuery rows,
// grouping a user's role rows into one mapping. It leaves rows open for a
// next result set and returns bare errors; its callers wrap.
func scanUserMappings(rows rowSource) ([]*LoginUserMapping, error) {
	var out []*LoginUserMapping
	var last *LoginUserMapping
	lastID := -1
	for rows.Next() {
		var dbName, user, schema string
		var id int
		var role sql.NullString
		if err := rows.Scan(&dbName, &id, &user, &schema, &role); err != nil {
			return nil, err
		}
		if last == nil || id != lastID || dbName != last.Database {
			last = &LoginUserMapping{Database: dbName, User: user, DefaultSchema: schema}
			lastID = id
			out = append(out, last)
		}
		if role.Valid {
			last.Roles = append(last.Roles, role.String)
		}
	}
	return out, rows.Err()
}

// userMappingsIn reads one database's mapping for l, for a caller asking
// about that database by name: a failure is returned, not skipped.
func (l *Login) userMappingsIn(ctx context.Context, db *Database) ([]*LoginUserMapping, error) {
	sid, _, err := l.identity(ctx, fmt.Sprintf("user mappings for login %q in %q", l.Name, db.Name))
	if err != nil {
		return nil, err
	}
	rows, err := db.query(ctx, userMappingsQuery, sid)
	if err != nil {
		return nil, fmt.Errorf("gosmo: user mappings for login %q in %q: %w", l.Name, db.Name, err)
	}
	defer rows.Close()
	out, err := scanUserMappings(rows)
	if err != nil {
		return nil, fmt.Errorf("gosmo: user mappings for login %q in %q: %w", l.Name, db.Name, err)
	}
	return out, nil
}

// MapToDatabase creates userName in the named database for this login
// (CREATE USER ... FOR LOGIN) and returns nothing of it: read the user back
// with Database.UserByName if the caller needs it. The database is read by
// name first (DatabaseByName), so a missing or inaccessible one fails before
// anything is written. defaultSchema "" omits DEFAULT_SCHEMA, leaving the
// server's default (dbo).
func (l *Login) MapToDatabase(ctx context.Context, dbName, userName, defaultSchema string) error {
	d, err := l.server.DatabaseByName(ctx, dbName)
	if err != nil {
		return err
	}
	_, err = d.CreateUser(ctx, CreateUserRequest{Name: userName, Login: l.Name, DefaultSchema: defaultSchema})
	return err
}

// UnmapFromDatabase drops this login's mapped user in the named database.
// It reads that database's mapping alone: UserMappings would query every
// database on the server to find the one whose name it was already given.
// On a LoginRef the SID is looked up by name first.
func (l *Login) UnmapFromDatabase(ctx context.Context, dbName string) error {
	d, err := l.server.DatabaseByName(ctx, dbName)
	if err != nil {
		return err
	}
	mappings, err := l.userMappingsIn(ctx, d)
	if err != nil {
		return err
	}
	if len(mappings) == 0 {
		return notFoundf("gosmo: login %q is not mapped to database %q", l.Name, dbName)
	}
	return d.UserRef(mappings[0].User).Drop(ctx)
}
