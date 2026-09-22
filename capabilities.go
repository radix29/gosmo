package gosmo

// capabilities.go is what the connected login may do: the three-way answer
// HAS_PERMS_BY_NAME gives, and the lists of roles and permissions each probe
// asks about. The server's answers are in capabilities_server.go, a database's
// in capabilities_database.go, the SQL that asks in capabilities_query.go and
// the scan that fills them in capabilities_scan.go.

// CapabilityState is the answer to "does the connected login hold this
// permission?" — the three-way answer HAS_PERMS_BY_NAME actually gives.
//
// The third state is not padding. HAS_PERMS_BY_NAME returns NULL, without
// raising an error, for a permission name the instance does not define, so a
// permission introduced in a later version reads as CapabilityUnknown on an
// older one rather than as denied. A caller that folds Unknown into Denied
// hides a feature on every instance that has it under a different name.
type CapabilityState int

const (
	// CapabilityUnknown means the answer is not available: the permission is
	// not one this instance defines, or it was never probed, or the probe
	// itself failed. It is not a denial — see Capabilities.Allows.
	CapabilityUnknown CapabilityState = iota

	// CapabilityGranted means the login holds the permission, whether
	// directly, through a role, or through a wider permission that implies
	// it.
	CapabilityGranted

	// CapabilityDenied means the instance was asked and said no.
	CapabilityDenied
)

func (s CapabilityState) String() string {
	switch s {
	case CapabilityGranted:
		return "granted"
	case CapabilityDenied:
		return "denied"
	default:
		return "unknown"
	}
}

// ProbedServerRoles are the fixed server roles Capabilities probes.
//
// Membership in sysadmin does not imply membership in any other role, so a
// caller testing for a role must test for sysadmin too — see
// Capabilities.InServerRole.
var ProbedServerRoles = []string{
	"sysadmin",
	"serveradmin",
	"securityadmin",
	"processadmin",
	"setupadmin",
	"bulkadmin",
	"diskadmin",
	"dbcreator",
	"public",
}

// ProbedServerPermissions are the server-scope permissions Capabilities
// probes: the handful the application layer gates on, not the whole grantable
// catalog ServerPermissionNames returns. Every name here was checked against a
// live instance; a name with a typo would report CapabilityUnknown forever
// rather than failing.
//
// VIEW SERVER PERFORMANCE STATE and VIEW SERVER SECURITY STATE are the two
// narrower rights SQL Server 2022 split VIEW SERVER STATE into, and are what a
// modern instance names in its denial. All three are probed because holding
// the wide one grants both narrow ones, but not the reverse.
var ProbedServerPermissions = []string{
	"CONTROL SERVER",
	"ALTER SETTINGS",
	"SHUTDOWN",
	"ALTER TRACE",
	"ADMINISTER BULK OPERATIONS",
	"VIEW ANY DATABASE",
	"VIEW ANY DEFINITION",
	"VIEW SERVER STATE",
	"VIEW SERVER PERFORMANCE STATE",
	"VIEW SERVER SECURITY STATE",
	"CREATE ANY DATABASE",
	"ALTER ANY DATABASE",
	"ALTER ANY LOGIN",
	"ALTER ANY SERVER ROLE",
	"ALTER ANY CONNECTION",
	"ALTER ANY CREDENTIAL",
	"ALTER ANY SERVER AUDIT",
	"ALTER ANY ENDPOINT",
	"ALTER ANY LINKED SERVER",
	"ALTER ANY EVENT SESSION",
	"ALTER ANY AVAILABILITY GROUP",
}

// ProbedServerSecurablePermissions are the permissions Capabilities reads
// explicit server-scope DENY rows for, once per securable the login has one
// recorded on: SERVER_PRINCIPAL (class 101 — logins and server roles alike)
// and ENDPOINT (class 105).
//
// One name is enough for the reason it is at schema and class-4 scope:
// CONTROL is matched alongside it in the query, and nothing narrower than
// ALTER is what these gates ask about.
//
// Only the DENY direction is read, and that is a fact about SQL Server rather
// than a choice — probed live on majors 13 and 17 (2026-09-04, identical on
// both). HAS_PERMS_BY_NAME cannot answer here at all: it reads 0 for a denied
// ALTER *and* for one never granted, which is the ordinary state of a login
// working through the server-wide ALTER ANY LOGIN, so only the catalog can say
// a DENY row exists. See Capabilities.ExplicitServerPermissions for what each
// class's DENY actually withholds — the two do not agree, and a caller that
// treats them alike is wrong about one of them.
var ProbedServerSecurablePermissions = []string{
	"ALTER",
}

// ProbedAvailabilityGroupPermissions are the AVAILABILITY GROUP-scope (class
// 108) permissions Capabilities probes, once per availability group on the
// instance.
//
// This scope is asked with HAS_PERMS_BY_NAME and not read out of the catalog,
// which is the opposite of every other explicit-permission block here, and the
// reason is that the catalog cannot answer: class 108's major_id is an
// internal availability-group id that **no supported view maps back to a
// name** — sys.availability_groups exposes only the group_id GUID, and the
// internal table behind it exposes nothing more. A catalog read at this class
// produces rows nothing can be matched to.
//
// HAS_PERMS_BY_NAME can be asked per group because there are single digits of
// them, where there are hundreds of logins, and it answers the question this
// scope actually needs: a login holding the server-wide
// ALTER ANY AVAILABILITY GROUP reads 1 on each group and 0 on one carrying
// DENY ALTER — verified live on the two-node cluster, 2026-09-05. That is the
// distinction sys.server_permissions had to be read for at class 101, and here
// the probe makes it directly.
var ProbedAvailabilityGroupPermissions = []string{
	"ALTER",
}

// ServerSecurableKind is the kind of server securable
// Capabilities.ExplicitServerPermissions is keyed by. Its values are the
// securable words SQL Server itself uses in DENY ... ON <kind>::<name>.
//
// Logins and server roles are told apart by the *principal's* type_desc rather
// than by class: both are class 101 SERVER_PRINCIPAL, and there is no class
// 110. A caller probing a separate class for server roles finds nothing.
type ServerSecurableKind string

const (
	// ServerSecurableLogin is a login — class 101 with a type_desc of
	// SQL_LOGIN, WINDOWS_LOGIN, WINDOWS_GROUP, EXTERNAL_LOGIN,
	// EXTERNAL_GROUP, CERTIFICATE_MAPPED_LOGIN or
	// ASYMMETRIC_KEY_MAPPED_LOGIN.
	ServerSecurableLogin ServerSecurableKind = "LOGIN"

	// ServerSecurableServerRole is a server role — class 101 with a type_desc
	// of SERVER_ROLE.
	ServerSecurableServerRole ServerSecurableKind = "SERVER ROLE"

	// ServerSecurableEndpoint is an endpoint — class 105.
	ServerSecurableEndpoint ServerSecurableKind = "ENDPOINT"
)

// ServerSecurableKey is the key ExplicitServerPermissions is indexed by: the
// securable kind and its name joined with "::", exactly as the probe records
// them and as the DENY statement spells them.
//
// The kind is part of the key rather than a separate map because the answer
// differs by kind — a class-101 DENY withholds everything on a login and only
// membership edits on a server role — and a caller must not be able to reach
// one kind's answer while asking about another's.
func ServerSecurableKey(kind ServerSecurableKind, name string) string {
	return string(kind) + "::" + name
}

// ProbedDatabaseRoles are the fixed database roles DatabaseCapabilities probes.
//
// The three SQLAgent* roles exist only in msdb; elsewhere IS_ROLEMEMBER
// returns NULL for them and they read as false. They are probed for every
// database rather than only msdb because doing so costs nothing and keeps one
// code path.
var ProbedDatabaseRoles = []string{
	"db_owner",
	"db_securityadmin",
	"db_accessadmin",
	"db_backupoperator",
	"db_ddladmin",
	"db_datawriter",
	"db_datareader",
	"db_denydatawriter",
	"db_denydatareader",
	"SQLAgentUserRole",
	"SQLAgentReaderRole",
	"SQLAgentOperatorRole",
}

// ProbedDatabasePermissions are the database-scope permissions
// DatabaseCapabilities probes — again a working subset, not the grantable
// catalog DatabasePermissionNames returns.
//
// These are checked against the DATABASE securable class, not the server:
// HAS_PERMS_BY_NAME(NULL, NULL, 'ALTER') asks about the *server* and answers
// NULL, which is how a database-scope name mistakenly probed at server scope
// disappears without an error.
var ProbedDatabasePermissions = []string{
	"CONTROL",
	"ALTER",
	"VIEW DEFINITION",
	"VIEW DATABASE STATE",
	"BACKUP DATABASE",
	"BACKUP LOG",
	"CREATE TABLE",
	"CREATE VIEW",
	"CREATE PROCEDURE",
	"CREATE FUNCTION",
	"CREATE SCHEMA",
	"ALTER ANY USER",
	"ALTER ANY ROLE",
	"ALTER ANY SCHEMA",
	"ALTER ANY DATASPACE",
	"ALTER ANY COLUMN MASTER KEY",
	"ALTER ANY COLUMN ENCRYPTION KEY",
	"ALTER ANY SECURITY POLICY",
	"ALTER ANY DATABASE AUDIT",
	"ALTER ANY DATABASE DDL TRIGGER",
	"ALTER ANY ASSEMBLY",
	"ALTER ANY EXTERNAL DATA SOURCE",
	"ALTER ANY EXTERNAL FILE FORMAT",
	// Service Broker. Each of these five is enough on its own to ALTER and
	// DROP its family — probed with WITHOUT LOGIN users on majors 13, 14 and
	// 17, which answered identically — so none of them is paired with ALTER
	// on the database. There is deliberately no broker-priority name here:
	// SQL Server enforces a CREATE/ALTER BROKER PRIORITY permission but does
	// not publish one, so HAS_PERMS_BY_NAME answers NULL for every spelling
	// of it and the entry would read CapabilityUnknown forever. A broker
	// priority is gated on ALTER on the database instead.
	"ALTER ANY MESSAGE TYPE",
	"ALTER ANY CONTRACT",
	"ALTER ANY SERVICE",
	"ALTER ANY ROUTE",
	"ALTER ANY REMOTE SERVICE BINDING",
	// 2017 and later. On 2016 HAS_PERMS_BY_NAME answers NULL for a name it
	// does not know, which reads as CapabilityUnknown — there is no external
	// library on that version to be gated anyway.
	"ALTER ANY EXTERNAL LIBRARY",
	// Certificates, asymmetric keys and symmetric keys, probed with
	// WITHOUT LOGIN users on majors 13, 14 and 17 and on a Managed Instance
	// (2026-09-22, identical on all four). ALTER ANY <family> permits CREATE,
	// DROP and, for a symmetric key, ADD/DROP ENCRYPTION BY a password, and
	// makes every row of the family visible. CREATE <family> permits the
	// CREATE alone: its holder owns what it creates and can drop exactly
	// that, which SecurablePermissions answers for, and sees no one else's
	// rows. ALTER and CONTROL on the database cover both; db_ddladmin does
	// too, and db_securityadmin does not.
	"CREATE CERTIFICATE",
	"CREATE ASYMMETRIC KEY",
	"CREATE SYMMETRIC KEY",
	"ALTER ANY CERTIFICATE",
	"ALTER ANY ASYMMETRIC KEY",
	"ALTER ANY SYMMETRIC KEY",
	"SELECT",
	"INSERT",
	"UPDATE",
	"DELETE",
	"EXECUTE",
	"SHOWPLAN",
}

// ProbedSchemaPermissions are the SCHEMA-scope permissions
// DatabaseCapabilities probes, once per schema in the database.
//
// One name is enough: HAS_PERMS_BY_NAME folds in the permissions that imply
// the one it is asked about, so a principal holding CONTROL on the schema, or
// ALTER ANY SCHEMA, or db_owner, answers 1 for ALTER without any of them being
// asked separately.
var ProbedSchemaPermissions = []string{
	"ALTER",
}

// ProbedObjectPermissions are the OBJECT-scope permissions
// DatabaseCapabilities probes, for every object the login has been granted one
// on or owns outright.
//
// This block is read out of the catalog rather than asked with
// HAS_PERMS_BY_NAME, which answers for one securable per call and so would
// cost a query per object. The consequence is that it reports only what is
// *explicit*: an object carrying no grant and no distinct owner has no row at
// all, which is why the answer is additive — see HasOnObject.
//
// Each name is probed at column scope as well, into ColumnPermissions. Only a
// column-grantable permission can produce a row there — ALTER and CONTROL are
// not among them — so the block is empty for this list as it stands, and
// correct the moment SELECT, UPDATE or REFERENCES joins it.
// CONTROL is asked for as well as folded into ALTER, and the two answer
// different questions. The query matches CONTROL alongside whatever name it
// is given, so "ALTER" already reads 1 for a principal holding either — which
// is right for a rename or a drop, both of which ALTER alone permits. A
// transfer is the statement where they part: ALTER SCHEMA ... TRANSFER needs
// CONTROL on the object and is refused to ALTER. Probed live 2026-09-16 on
// major 17 with a WITHOUT LOGIN user per right, transferring a service queue
// between schemas with ALTER on the target schema held throughout: CONTROL on
// the object, its ownership, CONTROL on the source schema and CONTROL on the
// database went through, while ALTER on the object, ALTER on the source
// schema, ALTER ANY SCHEMA, db_ddladmin and ALTER on the database were all
// refused Msg 15151. Only the first two of the four leave a row in this block,
// which is what makes it the one scope that can tell them apart.
var ProbedObjectPermissions = []string{
	"ALTER",
	"CONTROL",
}

// ProbedPrincipalPermissions are the DATABASE_PRINCIPAL-scope (class 4)
// permissions DatabaseCapabilities probes, for every user or database role the
// login has one explicitly recorded on.
//
// Only the DENY direction is worth reading here, and that is a fact about SQL
// Server rather than a choice — verified live on majors 13, 14 and 17
// (2026-09-04, identical on all three):
//
//   - GRANT ALTER ON USER::x answers HAS_PERMS_BY_NAME 1 on the user and still
//     permits nothing: both ALTER USER ... WITH NAME and DROP USER are refused.
//     Those statements require ALTER ANY USER at database scope, so unlike an
//     object- or schema-scope grant there is no narrow grant for a wider map to
//     miss.
//   - DENY ALTER ON USER::x *does* withhold both, over a database-wide
//     ALTER ANY USER, so only the catalog can say what a gate needs to know.
//
// One name is enough for the same reason it is at schema scope, and CONTROL is
// matched alongside it in the query: DENY CONTROL ON USER::x withholds the
// same two statements and is recorded under its own permission_name.
var ProbedPrincipalPermissions = []string{
	"ALTER",
}

// ProbedSecurablePermissions are the permissions DatabaseCapabilities probes on
// every assembly (class 5), user-defined type (class 6), XML schema collection
// (class 10), symmetric key (class 24), certificate (class 25) and asymmetric
// key (class 26) in the database, once per securable.
//
// This block is asked with HAS_PERMS_BY_NAME, as class 108 is, rather than
// read out of the catalog, and the answer it gives is the one no catalog row
// can: the *effective* permission, folding in ownership of the securable,
// ownership of or CONTROL on its schema, and CONTROL on the database. These
// families hold a handful of rows each, so one call per securable costs
// little.
//
// CONTROL is the name that answers for these, and what it answers was probed live on
// majors 13, 14 and 17 (2026-09-11, identical on all three) with a
// WITHOUT LOGIN user per case:
//
//   - ALTER SCHEMA ... TRANSFER of a type or a collection goes through exactly
//     when CONTROL reads 1 — under CONTROL on the securable, its ownership,
//     CONTROL on or ownership of the source schema, or CONTROL on the
//     database. ALTER on the database, ALTER ANY SCHEMA, db_ddladmin and ALTER
//     on the source schema all read 0 and are all refused (Msg 15151), with
//     ALTER on the target schema held throughout.
//   - DROP goes through when CONTROL reads 1, *and* under the wider rights
//     that read 0 here: ALTER ANY ASSEMBLY for an assembly, ALTER on the
//     schema for a type or a collection, ALTER on the database for all three.
//     So for a drop this is an additional reason to permit, never the whole
//     test.
//   - ALTER answers neither. GRANT ALTER on the securable alone reads 1
//     for ALTER and permits neither statement, and DENY ALTER reads 0 while
//     the drop goes through. It is asked for the symmetric key's sake (below)
//     and answers nothing for these three.
//
// For certificates, asymmetric keys and symmetric keys CONTROL answers for the
// drop, again in the permitting direction only (2026-09-22, majors 13, 14 and
// 17 and a Managed Instance): the owner and a CONTROL grantee can drop with no
// database-scope right at all, which is the case a CREATE CERTIFICATE-only
// user hits on the first certificate it makes; ALTER ANY <family>, ALTER on
// the database and db_ddladmin drop too, and read 0 here. ALTER on the key
// alone permits no drop, and VIEW DEFINITION on it permits nothing but seeing
// it. CONTROL on a certificate or asymmetric key is also what a symmetric key
// needs to be opened with it, or to DROP ENCRYPTION BY it.
//
// ALTER on a symmetric key is the exact answer for ALTER SYMMETRIC KEY ... ADD
// / DROP ENCRYPTION (2026-09-22, majors 13 and 17, identical): the statement
// goes through exactly when the effective ALTER reads 1 — under ALTER on the
// key alone, ALTER ANY SYMMETRIC KEY, or ownership — and is refused (Msg
// 15151) when it reads 0, which includes CONTROL on the key with ALTER denied
// and VIEW DEFINITION alone. CONTROL is not a stand-in for it in either
// direction. ALTER is asked of all six classes, since the block binds one list;
// on the other five it is read by nothing.
//
// There is no catalog block for the DENY direction, and that is SQL Server's
// doing rather than an omission: DENY CONTROL on any of the six — to the
// user or to public — withholds VIEW DEFINITION with it, and the securable
// disappears from sys.assemblies, sys.types, sys.xml_schema_collections,
// sys.certificates, sys.asymmetric_keys or sys.symmetric_keys for that
// principal (verified on the same three majors; for certificates on
// 2026-09-22). A listing built from those views never shows it, so there is
// nothing for a gate to withhold.
var ProbedSecurablePermissions = []string{
	"CONTROL",
	"ALTER",
}

// DatabaseSecurableKind is the kind of database securable
// DatabaseCapabilities.SecurablePermissions is keyed by — ServerSecurableKind's
// database-scope twin. Its values are the class words SQL Server itself uses,
// in HAS_PERMS_BY_NAME and in GRANT ... ON <kind>::<name>.
type DatabaseSecurableKind string

const (
	// DatabaseSecurableAssembly is an assembly — class 5, schemaless.
	DatabaseSecurableAssembly DatabaseSecurableKind = "ASSEMBLY"

	// DatabaseSecurableType is a user-defined type — class 6, covering alias,
	// table and CLR types alike.
	DatabaseSecurableType DatabaseSecurableKind = "TYPE"

	// DatabaseSecurableXMLSchemaCollection is an XML schema collection —
	// class 10.
	DatabaseSecurableXMLSchemaCollection DatabaseSecurableKind = "XML SCHEMA COLLECTION"

	// DatabaseSecurableSymmetricKey is a symmetric key — class 24,
	// schemaless. The database master key is not asked about.
	DatabaseSecurableSymmetricKey DatabaseSecurableKind = "SYMMETRIC KEY"

	// DatabaseSecurableCertificate is a certificate — class 25, schemaless.
	DatabaseSecurableCertificate DatabaseSecurableKind = "CERTIFICATE"

	// DatabaseSecurableAsymmetricKey is an asymmetric key — class 26,
	// schemaless.
	DatabaseSecurableAsymmetricKey DatabaseSecurableKind = "ASYMMETRIC KEY"
)

// DatabaseSecurableKey is the key SecurablePermissions is indexed by: the kind
// and the securable joined with "::", the securable being "schema.name" for a
// type or a collection and the bare name for an assembly, a certificate or a
// key, whose schema is "".
//
// The kind is part of the key for ServerSecurableKey's reason: types and XML
// schema collections live in separate namespaces, so dbo.x can be both, and
// the two answers must not be reachable through each other.
func DatabaseSecurableKey(kind DatabaseSecurableKind, schema, name string) string {
	if schema == "" {
		return string(kind) + "::" + name
	}
	return string(kind) + "::" + schema + "." + name
}
