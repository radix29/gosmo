// Command security demonstrates gosmo's principal and permission surface:
// creating a login, mapping it into a database as a user, role membership at
// both levels, granting and denying object/schema/database/server
// permissions, and reading the permission state back from either direction.
//
// Everything it creates is disposable and dropped at the end — it never
// touches an existing login or database.
//
//	MSSQL_SERVER=localhost:1433 MSSQL_USER=sa MSSQL_PASSWORD=YourPw go run ./examples/security
package main

import (
	"fmt"
	"strings"

	gosmo "github.com/radix29/gosmo"
	"github.com/radix29/gosmo/examples/internal/demo"
)

const (
	dbName    = "GoSMOSecurityDemo"
	loginName = "gosmo_demo_login"
	userName  = "gosmo_demo_user"
)

func main() {
	// First, so it runs after the cleanup deferred below it.
	defer demo.Exit()

	srv := demo.Connect()
	defer srv.Close()

	db, drop := demo.TempDatabase(srv, dbName)
	defer drop()

	demo.Must(db.CreateSchema("Reporting", "dbo"))
	demo.Must(db.CreateTable(gosmo.CreateTableRequest{
		Schema: "Reporting",
		Name:   "Revenue",
		Columns: []gosmo.ColumnDefinition{
			{Name: "Period", DataType: gosmo.DataTypeDate, IsNullable: false, IsPrimaryKey: true},
			{Name: "Amount", DataType: gosmo.DataTypeDecimal, Precision: 18, Scale: 2, IsNullable: false},
		},
	}))

	// -- Server-level: the login ------------------------------------------
	demo.Section("Login")
	_ = srv.DropLogin(loginName) // in case a previous run died mid-way
	demo.Must(srv.CreateLogin(loginName, "S0me-Str0ng-Pa55!", &gosmo.CreateLoginOptions{
		DefaultDatabase: dbName,
	}))
	defer func() {
		if err := srv.DropLogin(loginName); err == nil {
			fmt.Printf("Dropped login [%s]\n", loginName)
		}
	}()

	login := demo.Value(srv.LoginByName(loginName))
	fmt.Printf("  %s  type=%s  default_db=%s  disabled=%t\n",
		login.Name, login.LoginType, login.DefaultDatabase, login.IsDisabled)

	// Passwords are always escaped into an N'...' literal, never spliced in
	// raw, so any password content is safe.
	demo.Must(login.ChangePasswordWithOptions("An0ther-Str0ng-Pa55!", false, false))
	demo.Must(login.SetPasswordPolicy(true, false))
	demo.Must(login.SetDefaultLanguage("us_english"))

	// -- Server roles and permissions -------------------------------------
	demo.Section("Server role membership and permissions")
	demo.Must(login.AddServerRoleMember("dbcreator"))
	for _, m := range demo.Value(srv.ServerRoleMembers("dbcreator")) {
		fmt.Printf("  dbcreator member: %s (%s)\n", m.Name, m.Type)
	}

	// VIEW SERVER STATE is what a monitoring login needs before
	// Server.Info() or the session DMVs work for it.
	demo.Must(srv.GrantServerPermission("VIEW SERVER STATE", loginName))
	demo.Must(srv.DenyServerPermission("ALTER ANY LINKED SERVER", loginName))
	for _, p := range demo.Value(srv.ServerPermissions()) {
		if strings.EqualFold(p.Principal, loginName) {
			fmt.Printf("  %-6s %-28s to %s\n", p.State, p.Permission, p.Principal)
		}
	}
	// ServerPermissionNames (and its Database/Schema/Object siblings) is the
	// allowlist gosmo validates against — the source for a permissions grid.
	fmt.Printf("  %d server permissions are grantable in total\n", len(gosmo.ServerPermissionNames()))

	// -- Database-level: the user ------------------------------------------
	demo.Section("Database user")
	demo.Must(db.CreateUser(userName, loginName, "dbo"))
	user := demo.Value(db.UserByName(userName))
	fmt.Printf("  %s  type=%s  login=%s  default_schema=%s  auth=%s\n",
		user.Name, user.UserType, user.LoginName, user.DefaultSchema, user.AuthType)

	// An orphaned user — one whose login was dropped — keeps AuthType
	// "INSTANCE" with an empty LoginName. A user created WITHOUT LOGIN
	// reports AuthType "NONE" instead; the two look alike without AuthType.
	demo.Must(user.SetDefaultSchema("Reporting"))

	// -- Database roles ----------------------------------------------------
	demo.Section("Database role membership")
	demo.Must(db.AddRoleMember("db_datareader", userName))
	demo.Must(user.AddToRole("db_denydatawriter"))
	for _, r := range demo.Value(db.DatabaseRoles()) {
		if contains(r.Members, userName) {
			fmt.Printf("  %s is a member of %s\n", userName, r.Name)
		}
	}
	// RoleMembers reads the same relationship from the role's side.
	for _, m := range demo.Value(db.RoleMembers("db_datareader")) {
		fmt.Printf("  db_datareader member: %s (%s)\n", m.Name, m.Type)
	}

	// -- Permissions on a securable ---------------------------------------
	//
	// Grant to a genuine non-owner principal. GRANT to the object's owner
	// (dbo, or whoever owns the schema) succeeds and writes no row —
	// ownership already implies control — so testing against dbo makes a
	// working grant look broken.
	demo.Section("Object, schema and database permissions")
	demo.Must(db.GrantPermission("Reporting", "Revenue", gosmo.PermSelect, userName))
	demo.Must(db.DenyPermission("Reporting", "Revenue", gosmo.PermUpdate, userName))
	demo.Must(db.GrantSchemaPermission("Reporting", gosmo.PermView, userName))
	demo.Must(db.GrantDatabasePermission("VIEW DATABASE STATE", userName))

	fmt.Println("  one securable, every principal — Permissions():")
	for _, p := range demo.Value(db.Permissions("Reporting", "Revenue")) {
		fmt.Printf("    %-5s %-16s to %-20s (by %s)\n", p.State, p.Permission, p.Principal, p.Grantor)
	}

	fmt.Println("  one principal, every securable — PermissionsForPrincipal():")
	for _, p := range demo.Value(db.PermissionsForPrincipal(userName)) {
		name := p.Name
		if p.Schema != "" {
			name = p.Schema + "." + p.Name
		}
		fmt.Printf("    %-5s %-22s on %-10s %s\n", p.State, p.Permission, p.SecurableType, name)
	}

	// -- Revoking ----------------------------------------------------------
	demo.Section("Revoke")
	demo.Must(db.RevokePermission("Reporting", "Revenue", gosmo.PermUpdate, userName))
	fmt.Printf("  after revoke, %d entries remain on Reporting.Revenue\n",
		len(demo.Value(db.Permissions("Reporting", "Revenue"))))

	// -- Where is this login used? ----------------------------------------
	demo.Section("User mappings for the login")
	for _, m := range demo.Value(login.UserMappings()) {
		fmt.Printf("  [%s] as %s (schema %s) roles=%s\n",
			m.Database, m.User, m.DefaultSchema, strings.Join(m.Roles, ","))
	}

	// -- Server security settings -----------------------------------------
	demo.Section("Server security")
	fmt.Printf("  authentication mode: %s\n", demo.Value(srv.SecurityInfo()).AuthenticationMode)

	// -- Cleanup -----------------------------------------------------------
	// Drop the user explicitly: DROP LOGIN fails while a database user is
	// still mapped to it, and the deferred login drop runs before the
	// deferred database drop.
	demo.Must(user.Drop())
	demo.Must(srv.RevokeServerPermission("VIEW SERVER STATE", loginName))
	demo.Must(login.RemoveServerRoleMember("dbcreator"))
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if strings.EqualFold(s, needle) {
			return true
		}
	}
	return false
}
