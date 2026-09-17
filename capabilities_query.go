package gosmo

// capabilities_query.go builds the probe statements: one query per scope, each
// a VALUES list of names joined to HAS_PERMS_BY_NAME or to the explicit-DENY
// catalogue views, so a whole scope costs one round trip whatever its list
// holds. The results are scanned in capabilities_scan.go.

import (
	"fmt"
	"strings"
)

// capabilityQuery builds the two-part probe: one row per name, tagged 'R' or
// 'P', with each name passed as a parameter through a VALUES constructor
// rather than pasted into the text.
//
// The name is selected back alongside its answer so the caller reads the
// result by name. Scanning N booleans positionally instead would make adding a
// name to one of the lists silently shift every answer after it.
func capabilityQuery(roleSelect string, roles []string, permSelect string, perms []string) (string, []any) {
	args := make([]any, 0, len(roles)+len(perms))
	for _, n := range roles {
		args = append(args, n)
	}
	for _, n := range perms {
		args = append(args, n)
	}

	var b strings.Builder
	b.WriteString(roleSelect)
	b.WriteString(valuesClause(1, len(roles)))
	b.WriteString("\nUNION ALL\n")
	b.WriteString(permSelect)
	b.WriteString(valuesClause(1+len(roles), len(perms)))
	return b.String(), args
}

// schemaCapabilityQuery builds the third block of the database probe: one row
// per schema per probed permission, asked of every schema in the database in
// one pass rather than a query per schema.
//
// The permission travels in the *kind* column and the schema in the name
// column, not the other way round: a permission name is ours and fixed, a
// schema name is user data, and a schema called "P" would otherwise be read
// back as a database-scope permission answer.
func schemaCapabilityQuery(first int, perms []string) (string, []any) {
	args := make([]any, len(perms))
	for i, n := range perms {
		args[i] = n
	}
	return "SELECT CONCAT('S:', n.v), s.name, HAS_PERMS_BY_NAME(QUOTENAME(s.name), 'SCHEMA', n.v)" +
		" FROM sys.schemas AS s CROSS JOIN (VALUES " + valuesList(first, len(perms)) + ") AS n(v)", args
}

// objectCapabilityQuery builds the OBJECT-scope block: one row per object the
// login has an explicit permission on or owns, tagged "O:<permission>" with
// the object as "schema.object" and 1 for held, 0 for denied.
//
// It is a catalog read rather than a HAS_PERMS_BY_NAME probe because that
// function answers for one securable per call — a query per object, which is
// what kept this scope unprobed. Four details are load-bearing, each of them a
// wrong answer if dropped:
//
//   - The recursive CTE walks role membership. A permission granted to a role
//     the login reaches only through another role is held just as fully as one
//     granted directly, and stopping at the first level misses it.
//   - public is in the principal set, so the permission_name filter has to
//     stay: without it every catalog view's SELECT grant to public comes back,
//     235 rows on a stock database against the 3 that matter.
//   - minor_id splits the two scopes rather than filtering one away. A
//     column-scope row shares class 1 with the object-level ones, so folding
//     it in reports a grant on one column as a grant on the table and a DENY
//     on one column as a DENY on all of them; dropping it instead leaves the
//     wider grant to answer for a column SQL Server refuses. Column rows are
//     tagged "C:" and keyed "schema.object.column" — see
//     DatabaseCapabilities.ColumnPermissions.
//   - The sys.objects half is not redundant with the permissions half: an
//     object's owner holds implicit CONTROL and has *no* permission row at
//     all, so ownership is invisible to the catalog's permission list.
//
// An object the login can see nothing of comes back with a NULL name — a DENY
// leaves no permission behind, and metadata visibility then hides the object —
// and is dropped. Such an object is equally invisible in any listing built
// from the same catalog, so there is nothing for the answer to gate.
func objectCapabilityQuery(first int, perms []string) (string, []any) {
	args := make([]any, len(perms))
	for i, n := range perms {
		args[i] = n
	}
	return `SELECT CONCAT('O:', n.v), CONCAT(SCHEMA_NAME(o.schema_id), '.', o.name), 1
	FROM sys.objects AS o CROSS JOIN (VALUES ` + valuesList(first, len(perms)) + `) AS n(v)
	WHERE o.principal_id IN (SELECT id FROM cap_me)
UNION ALL
	SELECT CONCAT('O:', n.v), CONCAT(OBJECT_SCHEMA_NAME(p.major_id), '.', OBJECT_NAME(p.major_id)),
	       CASE WHEN p.state IN ('D') THEN 0 ELSE 1 END
	FROM sys.database_permissions AS p CROSS JOIN (VALUES ` + valuesList(first, len(perms)) + `) AS n(v)
	WHERE p.class = 1 AND p.minor_id = 0
	  AND p.grantee_principal_id IN (SELECT id FROM cap_me)
	  AND p.permission_name IN (n.v, 'CONTROL')
	  AND OBJECT_NAME(p.major_id) IS NOT NULL
UNION ALL
	SELECT CONCAT('C:', n.v), CONCAT(OBJECT_SCHEMA_NAME(p.major_id), '.', OBJECT_NAME(p.major_id),
	                                 '.', COL_NAME(p.major_id, p.minor_id)),
	       CASE WHEN p.state IN ('D') THEN 0 ELSE 1 END
	FROM sys.database_permissions AS p CROSS JOIN (VALUES ` + valuesList(first, len(perms)) + `) AS n(v)
	WHERE p.class = 1 AND p.minor_id > 0
	  AND p.grantee_principal_id IN (SELECT id FROM cap_me)
	  AND p.permission_name IN (n.v, 'CONTROL')
	  AND OBJECT_NAME(p.major_id) IS NOT NULL
	  AND COL_NAME(p.major_id, p.minor_id) IS NOT NULL`, args
}

// explicitSchemaCapabilityQuery builds the SCHEMA-scope catalog block: one row
// per schema the login has an explicit permission recorded on, tagged
// "E:<permission>" with the schema as the name and 1 for held, 0 for denied.
//
// It is a catalog read beside the HAS_PERMS_BY_NAME block schemaCapabilityQuery
// already asks, not a replacement for it, and the two answer different
// questions: HAS_PERMS_BY_NAME returns 0 for a permission never granted just as
// it does for one explicitly denied, so only the catalog can say a DENY row
// exists. That distinction is the whole reason for this block — a schema-scope
// DENY overrides a database-wide GRANT, so a gate reading the wider grant alone
// offers a write the server then refuses with Msg 297.
//
// class 3 is SCHEMA, and major_id is the schema_id. As in the object block the
// principal set is the recursive cap_me CTE, CONTROL is matched alongside the
// permission asked about because it implies it, and a schema hidden by
// metadata visibility comes back with a NULL name and is dropped.
func explicitSchemaCapabilityQuery(first int, perms []string) (string, []any) {
	args := make([]any, len(perms))
	for i, n := range perms {
		args[i] = n
	}
	return `SELECT CONCAT('E:', n.v), SCHEMA_NAME(p.major_id),
	       CASE WHEN p.state IN ('D') THEN 0 ELSE 1 END
	FROM sys.database_permissions AS p CROSS JOIN (VALUES ` + valuesList(first, len(perms)) + `) AS n(v)
	WHERE p.class = 3
	  AND p.grantee_principal_id IN (SELECT id FROM cap_me)
	  AND p.permission_name IN (n.v, 'CONTROL')
	  AND SCHEMA_NAME(p.major_id) IS NOT NULL`, args
}

// explicitDatabaseCapabilityQuery builds the DATABASE-scope catalog block: one
// row per probed permission the login has an explicit DENY recorded for at
// class 0, tagged "D:<permission>" with the database as the name and 0 for
// denied.
//
// Only DENY rows are selected, which is what separates it from the schema
// block it is otherwise modelled on. The grant direction is already answered
// by the HAS_PERMS_BY_NAME block, which folds in role membership and covering
// permissions; the catalog can add nothing there. What only the catalog can
// say is that a DENY row exists, and a database-scope DENY overrides an
// object- or schema-scope GRANT — see
// DatabaseCapabilities.ExplicitDatabasePermissions.
//
// class 0 is DATABASE, where major_id is 0 and names nothing, so DB_NAME()
// stands in as the securable. CONTROL is deliberately not matched alongside
// the permission: DENY CONTROL at this scope denies CONNECT with it, so the
// login cannot open the database and never reaches this map.
func explicitDatabaseCapabilityQuery(first int, perms []string) (string, []any) {
	args := make([]any, len(perms))
	for i, n := range perms {
		args[i] = n
	}
	return `SELECT CONCAT('D:', n.v), DB_NAME(), 0
	FROM sys.database_permissions AS p CROSS JOIN (VALUES ` + valuesList(first, len(perms)) + `) AS n(v)
	WHERE p.class = 0
	  AND p.state = 'D'
	  AND p.grantee_principal_id IN (SELECT id FROM cap_me)
	  AND p.permission_name = n.v`, args
}

// explicitPrincipalCapabilityQuery builds the DATABASE_PRINCIPAL-scope (class 4)
// catalog block: one row per principal the login has an explicit DENY recorded
// on, tagged "N:<permission>" with the principal's name and 0 for denied.
//
// Only DENY rows are selected, as in the database block, and here the grant
// direction does not merely add nothing — it exists nowhere. GRANT ALTER ON
// USER::x reads HAS_PERMS_BY_NAME 1 and still permits neither the rename nor
// the drop, because both statements require ALTER ANY USER at database scope;
// see ProbedPrincipalPermissions for the live result.
//
// class 4 is DATABASE_PRINCIPAL and major_id is the principal_id. USER_NAME
// resolves it for a database role as well as for a user — verified live on
// majors 13, 14 and 17 — so both kinds land in one map, which is what
// DatabaseCapabilities.ExplicitPrincipalPermissions warns its callers about.
//
// CONTROL *is* matched alongside the permission asked about, unlike the
// database block: DENY CONTROL ON USER::x is recorded under its own
// permission_name and withholds the same two statements, and denying CONTROL
// on one principal does not lock the login out of the database the way DENY
// CONTROL at class 0 does.
//
// A principal hidden by metadata visibility would come back with a NULL name
// and is dropped, as in the schema and object blocks. Class 4 does not in fact
// suppress visibility — a user carrying a DENY stays listed, verified live —
// but the guard costs nothing and the block should not depend on that.
func explicitPrincipalCapabilityQuery(first int, perms []string) (string, []any) {
	args := make([]any, len(perms))
	for i, n := range perms {
		args[i] = n
	}
	return `SELECT CONCAT('N:', n.v), USER_NAME(p.major_id), 0
	FROM sys.database_permissions AS p CROSS JOIN (VALUES ` + valuesList(first, len(perms)) + `) AS n(v)
	WHERE p.class = 4
	  AND p.state = 'D'
	  AND p.grantee_principal_id IN (SELECT id FROM cap_me)
	  AND p.permission_name IN (n.v, 'CONTROL')
	  AND USER_NAME(p.major_id) IS NOT NULL`, args
}

// securableCapabilityQuery builds the class 5/6/10 block: one row per
// assembly, user-defined type and XML schema collection per probed permission,
// tagged "K:<permission>" with the securable as DatabaseSecurableKey spells it.
//
// It asks HAS_PERMS_BY_NAME where the object block reads the catalog, and has
// to — see ProbedSecurablePermissions: the answer that matters is the
// effective one, which folds in schema ownership and CONTROL on the database,
// and no permission row records either. The permission rides in the kind
// column and the securable in the name column, schemaCapabilityQuery's
// arrangement and for its reason.
//
// Three details are load-bearing:
//
//   - The HAS_PERMS_BY_NAME class word is the kind itself. A type asked about
//     as 'OBJECT' reads NULL — it is not in sys.objects — and NULL is
//     unknown, which gates nothing.
//   - The name is QUOTENAMEd part by part. "[s].[t]" is what HAS_PERMS_BY_NAME
//     parses; a dot or a bracket inside a bare name asks about a different
//     securable, or none.
//   - System rows are skipped: the built-in types, Microsoft.SqlServer.Types
//     and the sys schema's collection offer nothing to gate, and there are
//     thirty-odd built-in types per database.
func securableCapabilityQuery(first int, perms []string) (string, []any) {
	args := make([]any, len(perms))
	for i, n := range perms {
		args[i] = n
	}
	vals := valuesList(first, len(perms))
	return `SELECT CONCAT('K:', n.v), CONCAT('ASSEMBLY::', a.name),
	       HAS_PERMS_BY_NAME(QUOTENAME(a.name), 'ASSEMBLY', n.v)
	FROM sys.assemblies AS a CROSS JOIN (VALUES ` + vals + `) AS n(v)
	WHERE a.is_user_defined = 1
UNION ALL
	SELECT CONCAT('K:', n.v), CONCAT('TYPE::', SCHEMA_NAME(t.schema_id), '.', t.name),
	       HAS_PERMS_BY_NAME(QUOTENAME(SCHEMA_NAME(t.schema_id)) + '.' + QUOTENAME(t.name), 'TYPE', n.v)
	FROM sys.types AS t CROSS JOIN (VALUES ` + vals + `) AS n(v)
	WHERE t.is_user_defined = 1
UNION ALL
	SELECT CONCAT('K:', n.v), CONCAT('XML SCHEMA COLLECTION::', SCHEMA_NAME(x.schema_id), '.', x.name),
	       HAS_PERMS_BY_NAME(QUOTENAME(SCHEMA_NAME(x.schema_id)) + '.' + QUOTENAME(x.name), 'XML SCHEMA COLLECTION', n.v)
	FROM sys.xml_schema_collections AS x CROSS JOIN (VALUES ` + vals + `) AS n(v)
	WHERE x.schema_id <> SCHEMA_ID('sys')`, args
}

// explicitServerCapabilityQuery builds the server-scope catalog block: one row
// per server securable the login has an explicit DENY recorded on, tagged
// "V:<permission>" with the securable as "<kind>::<name>" and 0 for denied.
//
// It is the only block of the server probe that is not a HAS_PERMS_BY_NAME
// question, and it has to be, for explicitSchemaCapabilityQuery's reason one
// scope wider: HAS_PERMS_BY_NAME reads 0 for an ALTER denied on a login and
// for one never granted alike — and never granted is the ordinary state of a
// login working through the server-wide ALTER ANY LOGIN — so only the catalog
// can say a DENY row exists.
//
// Only DENY rows are selected, as in the database and class-4 blocks. The
// grant direction is already answered by the HAS_PERMS_BY_NAME block, which
// folds in role membership and covering permissions.
//
// Two classes, one block. **There is no class 110**: logins and server roles
// are both class 101 SERVER_PRINCIPAL, told apart here by the principal's
// type_desc, exactly as class 4 tells a user from a database role. major_id is
// the principal_id at class 101 and the endpoint_id at class 105, which is why
// the two halves join to different catalog views and cannot be merged.
//
// CONTROL is matched alongside the permission asked about, as at class 4 and
// unlike the database block: DENY CONTROL on a login withholds the same
// statements and is recorded under its own permission_name, and denying
// CONTROL on one securable does not lock the login out of the server.
//
// The joins are inner, which drops a securable hidden by metadata visibility
// the way the NULL-name guards do in the database blocks. Such a securable is
// equally invisible in any listing built from the same catalog, so there is
// nothing for the answer to gate.
func explicitServerCapabilityQuery(first int, perms []string) (string, []any) {
	args := make([]any, len(perms))
	for i, n := range perms {
		args[i] = n
	}
	return `SELECT CONCAT('V:', n.v),
	       CONCAT(CASE sp.type_desc WHEN 'SERVER_ROLE' THEN 'SERVER ROLE' ELSE 'LOGIN' END, '::', sp.name), 0
	FROM sys.server_permissions AS p
	JOIN sys.server_principals AS sp ON sp.principal_id = p.major_id
	CROSS JOIN (VALUES ` + valuesList(first, len(perms)) + `) AS n(v)
	WHERE p.class = 101
	  AND p.state = 'D'
	  AND p.grantee_principal_id IN (SELECT id FROM cap_srv_me)
	  AND p.permission_name IN (n.v, 'CONTROL')
UNION ALL
	SELECT CONCAT('V:', n.v), CONCAT('ENDPOINT::', e.name), 0
	FROM sys.server_permissions AS p
	JOIN sys.endpoints AS e ON e.endpoint_id = p.major_id
	CROSS JOIN (VALUES ` + valuesList(first, len(perms)) + `) AS n(v)
	WHERE p.class = 105
	  AND p.state = 'D'
	  AND p.grantee_principal_id IN (SELECT id FROM cap_srv_me)
	  AND p.permission_name IN (n.v, 'CONTROL')`, args
}

// availabilityGroupCapabilityQuery builds the AVAILABILITY GROUP-scope (class
// 108) block: one row per availability group on the instance per probed
// permission, asked of every group in one pass rather than a query per group.
//
// Alone among the server-scope blocks this asks HAS_PERMS_BY_NAME rather than
// reading sys.server_permissions, and it has to — see
// ProbedAvailabilityGroupPermissions, where the reason and the live result are
// recorded.
//
// The permission travels in the *kind* column and the group in the name
// column, schemaCapabilityQuery's arrangement and for its reason: a permission
// name is ours and fixed, a group name is user data, and a group called "P"
// would otherwise be read back as a server-scope permission answer.
//
// sys.availability_groups exists on every edition and is empty when HADR is
// off, so this block costs one empty scan on an instance with no groups rather
// than needing a version or feature gate.
func availabilityGroupCapabilityQuery(first int, perms []string) (string, []any) {
	args := make([]any, len(perms))
	for i, n := range perms {
		args[i] = n
	}
	return "SELECT CONCAT('G:', n.v), ag.name, HAS_PERMS_BY_NAME(QUOTENAME(ag.name), 'AVAILABILITY GROUP', n.v)" +
		" FROM sys.availability_groups AS ag CROSS JOIN (VALUES " + valuesList(first, len(perms)) + ") AS n(v)", args
}

// capabilityServerPrincipalCTE is capabilityPrincipalCTE at server scope: the
// login itself, public, and every server role reachable through role
// membership at any depth.
//
// It is a separate CTE rather than the same one parameterised because every
// name in it differs — SUSER_ID for DATABASE_PRINCIPAL_ID,
// sys.server_role_members for sys.database_role_members — and because public
// has no SUSER_ID-style accessor at server scope and has to be looked up by
// name.
const capabilityServerPrincipalCTE = `WITH cap_srv_me AS (
	SELECT SUSER_ID() AS id
	UNION ALL SELECT principal_id FROM sys.server_principals WHERE name = 'public'
	UNION ALL
	SELECT rm.role_principal_id FROM sys.server_role_members AS rm
	JOIN cap_srv_me ON rm.member_principal_id = cap_srv_me.id
)
`

// capabilityPrincipalCTE is the "every principal the login's permissions can
// arrive through" set the object block selects from: the user itself, public,
// and every role reachable through role membership at any depth.
const capabilityPrincipalCTE = `WITH cap_me AS (
	SELECT DATABASE_PRINCIPAL_ID() AS id
	UNION ALL SELECT DATABASE_PRINCIPAL_ID('public')
	UNION ALL
	SELECT rm.role_principal_id FROM sys.database_role_members AS rm
	JOIN cap_me ON rm.member_principal_id = cap_me.id
)
`

// valuesList renders "(@pN),(@pN+1),..." for count parameters starting at
// first — the placeholder list both probe blocks bind their names through.
func valuesList(first, count int) string {
	var b strings.Builder
	for i := range count {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, "(@p%d)", first+i)
	}
	return b.String()
}

// valuesClause renders " FROM (VALUES (@pN),(@pN+1),...) AS n(v)" for count
// parameters starting at first.
func valuesClause(first, count int) string {
	var b strings.Builder
	b.WriteString(" FROM (VALUES ")
	b.WriteString(valuesList(first, count))
	b.WriteString(") AS n(v)")
	return b.String()
}
