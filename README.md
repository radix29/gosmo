# gosmo

A Go library that mimics **Microsoft SQL Server Management Objects (SMO)** — without WMI, COM, or Windows-only dependencies.

```
go get github.com/radix29/gosmo
```

> **Go version note:** The module requires Go 1.26

---

## Architecture

```mermaid
classDiagram
    %% =========================================================
    %% Top-level entry point
    %% =========================================================
    class ConnectionOptions {
        +Server string
        +Database string
        +Auth AuthMethod
        +User string
        +Password string
        +TenantID string
        +ClientID string
        +ClientCertPath string
        +AccessToken string
        +AccessTokenProvider func
        +ServerSPN string
        +Kerberos KerberosOptions
        +ConnectTimeout Duration
        +ApplicationName string
        +MaxOpenConns int
        +MaxIdleConns int
        +ConnMaxLifetime Duration
        +SessionInitSQL string
        +TrustServerCertificate bool
        +Encrypt string
    }

    class Server {
        -db *sql.DB
        -info *ServerInfo
        +Connect(opts) *Server
        +ConnectContext(ctx, opts) *Server
        +ParseServerAddress(server) string
        +Close() error
        +DB() *sql.DB
        +Info() *ServerInfo
        +Name() string
        +CurrentDatabase() string
        +Databases() []*Database
        +DatabaseByName(name) *Database
        +CreateDatabase(name, opts) error
        +DropDatabase(name, force) error
        +Logins() []*Login
        +LoginByName(name) *Login
        +CreateLogin(name, password, opts) error
        +DropLogin(name) error
        +ServerRoles() []*ServerRole
        +LinkedServers() []*LinkedServer
        +Configurations() []*Configuration
        +Jobs() []*AgentJob
        +ActiveSessions(sys) []*Session
        +KillSession(id) error
        +ReadErrorLog(n) []*ErrorLogEntry
        +MailProfiles() []*MailProfile
        +SendMail(opts) error
        +Backup(opts) error
        +Restore(opts) error
        +SecurityInfo() *ServerSecurityInfo
        +ServerPermissions() []*ServerPermissionEntry
        +GrantServerPermission(perm, principal) error
        +DenyServerPermission(perm, principal) error
        +RevokeServerPermission(perm, principal) error
        +Credentials() []*Credential
        +MemoryStats() *ServerMemoryStats
        +Languages() []*Language
    }

    class ServerInfo {
        +Name string
        +Edition string
        +ProductVersion string
        +ProductLevel string
        +Collation string
        +IsClustered bool
        +IsHADREnabled bool
        +OSVersion string
        +PhysicalMemoryMB int64
        +LogicalCPUCount int
        +DefaultDataPath string
        +DefaultLogPath string
        +DefaultBackupPath string
        +VersionMajor int
        +VersionMinor int
        +VersionBuild int
    }

    %% =========================================================
    %% Authentication
    %% =========================================================
    class AuthMethod {
        <<enumeration>>
        AuthSQLServer
        AuthWindows
        AuthEntraMSI
        AuthEntraServicePrincipal
        AuthEntraPassword
        AuthEntraInteractive
        AuthEntraDeviceCode
        AuthEntraDefault
        AuthEntraAzCLI
        AuthEntraAzurePipelines
        AuthEntraServicePrincipalAccessToken
        AuthEntraOnBehalfOf
    }

    class KerberosOptions {
        +ConfigFile string
        +CredCacheFile string
        +KeytabFile string
        +Realm string
        +DNSLookupKDC *bool
        +UDPPreferenceLimit int
        Native SSPI on Windows, unless set.
        Every other platform authenticates
        AuthWindows via Kerberos, using this or
        the ambient kinit cache when it is the
        zero value.
    }

    %% =========================================================
    %% Server security, permissions, memory, languages
    %% =========================================================
    class ServerSecurityInfo {
        +AuthenticationMode string
    }

    class ServerPermissionEntry {
        +Principal string
        +PrincipalType string
        +Grantor string
        +Permission string
        +State string
    }

    class Credential {
        +Name string
        +Identity string
        +CreateDate time.Time
        +ModifyDate time.Time
    }

    class ServerMemoryStats {
        +PhysicalMemoryMB int64
        +AvailableMemoryMB int64
        +TargetServerMemoryMB int64
        +TotalServerMemoryMB int64
    }

    class Language {
        +LangID int
        +Name string
        +Alias string
    }

    %% =========================================================
    %% Login
    %% =========================================================
    class Login {
        +Name string
        +SID []byte
        +LoginType string
        +IsDisabled bool
        +DefaultDatabase string
        +CreateDate time.Time
        +ModifyDate time.Time
        +Enable() error
        +Disable() error
        +ChangePassword(newPassword) error
        +ChangePasswordWithOptions(pw, mustChange, unlock) error
        +AddServerRoleMember(role) error
        +RemoveServerRoleMember(role) error
        +Drop() error
        +Details() *LoginDetails
        +Rename(newName) error
        +SetDefaultDatabase(name) error
        +SetDefaultLanguage(lang) error
        +SetPasswordPolicy(checkPolicy, checkExpiration) error
        +MapCredential(credential) error
        +UnmapCredential(credential) error
        +UserMappings() []*LoginUserMapping
        +MapToDatabase(dbName, user, schema) error
        +UnmapFromDatabase(dbName) error
    }

    class LoginDetails {
        +IsLocked bool
        +IsExpired bool
        +MustChangePassword bool
        +IsPolicyChecked bool
        +IsExpirationChecked bool
        +PasswordLastSet time.Time
        +LastLogin time.Time
        +BadPasswordCount int
        +DefaultLanguage string
        +CredentialName string
        +ConnectSQLState string
    }

    class LoginUserMapping {
        +Database string
        +User string
        +DefaultSchema string
        +Roles []string
    }

    class nStringLiteral {
        <<internal helper>>
        Quotes a password as an N'...'
        T-SQL string literal, escaping
        any embedded quote.
        Used by CreateLogin and ChangePassword —
        HASHED is never used, since it tells
        SQL Server the value is already one of
        its own password-hash formats, not
        cleartext.
    }

    %% =========================================================
    %% Database
    %% =========================================================
    class Database {
        -server *Server
        -name string
        -id int
        -state string
        -recoveryModel RecoveryModel
        -compatLevel CompatibilityLevel
        -collation string
        -isReadOnly bool
        -createDate time.Time
        +Name() string
        +ID() int
        +State() string
        +IsSystem() bool
        +RecoveryModel() RecoveryModel
        +CompatibilityLevel() CompatibilityLevel
        +Tables() []*Table
        +TablesBySchema(schema) []*Table
        +TableByName(schema, name) *Table
        +CreateTable(req) error
        +DropTable(schema, name, cascade) error
        +Views() []*View
        +StoredProcedures() []*StoredProcedure
        +CreateStoredProcedure(schema, name, body) error
        +DropStoredProcedure(schema, name) error
        +UserDefinedFunctions() []*UserDefinedFunction
        +Schemas() []*Schema
        +CreateSchema(name, owner) error
        +DropSchema(name) error
        +Users() []*User
        +CreateUser(user, login, schema) error
        +DropUser(name) error
        +DatabaseRoles() []*DatabaseRole
        +AddRoleMember(role, member) error
        +RemoveRoleMember(role, member) error
        +FileGroups() []*FileGroup
        +Triggers() []*Trigger
        +Sequences() []*Sequence
        +Synonyms() []*Synonym
        +PartitionFunctions() []*PartitionFunction
        +PartitionSchemes() []*PartitionScheme
        +ExtendedProperties(level) []*ExtendedProperty
        +AddExtendedProperty(name, value, level) error
        +SetExtendedProperty(name, value, level) error
        +DropExtendedProperty(name, level) error
        +ColumnMasterKeys() []*ColumnMasterKey
        +ColumnEncryptionKeys() []*ColumnEncryptionKey
        +SecurityPolicies() []*SecurityPolicy
        +SpaceUsed() SpaceInfo
        +SetRecoveryModel(model) error
        +SetCompatibilityLevel(level) error
        +SetReadOnly(bool) error
        +Options() *DatabaseOptions
        +SetDatabaseOption(opt, value) error
        +SetOwner(principal) error
        +Files() []*DatabaseFileInfo
        +AddFile(spec) error
        +AlterFile(name, m) error
        +RemoveFile(name) error
        +AddFileGroup(name) error
        +RemoveFileGroup(name) error
        +SetDefaultFileGroup(name) error
        +SetFileGroupReadOnly(name, ro) error
        +ChangeTracking() *ChangeTrackingInfo
        +SetChangeTracking(info) error
        +TableChangeTracking() []*TableChangeTracking
        +SetTableChangeTracking(schema, name, enable, cols) error
        +Dependencies(schema, name) []*Dependency
        +Dependents(schema, name) []*Dependency
        +Search(pattern) []*SearchResult
        +Permissions(schema, name) []*PermissionEntry
        +GrantPermission(schema, name, perm, principal) error
        +DenyPermission(schema, name, perm, principal) error
        +RevokePermission(schema, name, perm, principal) error
        +DatabasePermissions() []*DatabasePermissionEntry
        +GrantDatabasePermission(perm, principal) error
        +DenyDatabasePermission(perm, principal) error
        +RevokeDatabasePermission(perm, principal) error
        +EstimatedPlan(sql) *ExecutionPlan
        +ActualPlan(sql) *ExecutionPlan
        +ExecProc(schema, name, params) ProcResult
        +BulkInsert(bc, rows) int64
    }

    %% =========================================================
    %% Connection helpers (internal)
    %% =========================================================
    class withConn {
        <<internal helper>>
        Acquires *sql.Conn from pool.
        Executes USE db.
        Runs callback fn(*sql.Conn).
        Releases conn via defer.
        Used by exec, scanRow, queryRow.
    }

    class rowsWithConn {
        <<internal type>>
        -Rows *sql.Rows
        -conn *sql.Conn
        +Close() error
        Closes Rows then conn atomically.
        Prevents conn leaks on early
        iteration exits or error returns.
        Returned by query().
    }

    class scanRow {
        <<internal helper>>
        Callback-style single-row query.
        conn lifetime fully internal —
        no release() needed by caller.
        Used by SpaceUsed, TableByName.
    }

    class queryRow {
        <<internal helper>>
        Returns (*sql.Row, func(), error).
        Caller MUST defer release().
        conn is held until release() fires.
        Used by scripter.go, table.go.
    }

    class withRetry {
        <<internal helper>>
        Generic retry wrapper for idempotent
        reads only. 3 attempts, linear backoff
        of attempt times 50ms. Used by
        Database.query and Database.queryRow.
    }

    class IsRetryable {
        <<package function>>
        True for the driver's RetryableError or
        a dropped pooled connection (ErrBadConn),
        including wrapped errors. Exported so
        callers running their own statements can
        make the same retry decision.
    }

    %% =========================================================
    %% Quoting (shared identifier / literal escaping)
    %% =========================================================
    class Quoting {
        <<package functions>>
        +QuoteName(name) string
        +QuoteLiteral(s) string
        Backed by the driver's own TSQLQuoter,
        so gosmo, its callers, and gossms share
        one quoting implementation. The internal
        quoteIdent helper delegates to QuoteName.
    }

    %% =========================================================
    %% Errors
    %% =========================================================
    class SQLError {
        +Number int32
        +State uint8
        +Class uint8
        +Message string
        +ServerName string
        +ProcName string
        +LineNo int32
        +All []SQLError
        +AsSQLError(err) *SQLError
        +Header() string
        +Error() string
        +IsError() bool
    }

    %% =========================================================
    %% Scripting pending writes (dry-run)
    %% =========================================================
    class ScriptCollector {
        +Statements []string
        +WithScript(ctx) *ScriptCollector
        Captures the exact statement(s) a set of
        pending write calls would run, without
        running them. Every write funnels through
        Server.execContext or Database.exec, the
        two chokepoints WithScript intercepts.
    }

    %% =========================================================
    %% Database files, filegroups, and options
    %% =========================================================
    class DatabaseFileInfo {
        +FileID int
        +Name string
        +PhysicalName string
        +Type string
        +FileGroup string
        +State string
        +SizeKB int64
        +MaxSizeKB int64
        +GrowthKB int64
        +GrowthPercent int
        +IsPercentGrowth bool
    }

    class DatabaseFileSpec {
        +Name string
        +FileGroup string
        +Type string
        +Path string
        +SizeKB int64
        +GrowthKB int64
        +GrowthPercent int
        +MaxSizeKB int64
    }

    class FileModify {
        +NewName string
        +SizeKB int64
        +GrowthKB int64
        +GrowthPercent int
        +MaxSizeKB int64
    }

    class DatabaseOptions {
        +Owner string
        +PageVerify string
        +UserAccess string
        +Containment string
        +DefaultCursor string
        +SnapshotIsolation string
        +AutoClose bool
        +AutoShrink bool
        +AutoCreateStats bool
        +AutoUpdateStats bool
        +AutoUpdateStatsAsync bool
        +ANSINullDefault bool
        +ANSINulls bool
        +ANSIPadding bool
        +ANSIWarnings bool
        +ArithAbort bool
        +ConcatNullYieldsNull bool
        +NumericRoundAbort bool
        +QuotedIdentifier bool
        +RecursiveTriggers bool
        +CursorCloseOnCommit bool
        +ReadCommittedSnapshot bool
        +IsTrustworthy bool
        +IsBrokerEnabled bool
    }

    %% =========================================================
    %% Change tracking
    %% =========================================================
    class ChangeTrackingInfo {
        +Enabled bool
        +AutoCleanup bool
        +RetentionPeriod int
        +RetentionUnit string
    }

    class TableChangeTracking {
        +Schema string
        +Name string
        +Enabled bool
        +TrackColumnsUpdated bool
    }

    %% =========================================================
    %% Dependencies and object search
    %% =========================================================
    class Dependency {
        +Schema string
        +Name string
        +TypeDesc string
        +IsSchemaBound bool
    }

    class SearchResult {
        +Schema string
        +Name string
        +TypeDesc string
    }

    %% =========================================================
    %% Execution plans
    %% =========================================================
    class ExecutionPlan {
        +XML string
    }

    %% =========================================================
    %% Object and database-scoped permissions
    %% =========================================================
    class PermissionEntry {
        +Principal string
        +PrincipalType string
        +Permission ObjectPermission
        +State PermissionState
    }

    class DatabasePermissionEntry {
        +Principal string
        +PrincipalType string
        +Grantor string
        +Permission string
        +State string
    }

    %% =========================================================
    %% Bulk copy (fast import — bcp / SSMS "Import Data")
    %% =========================================================
    class BulkCopy {
        +Schema string
        +Table string
        +Columns []string
        +Options BulkOptions
        +SliceRows(rows) iter.Seq2
    }

    class BulkOptions {
        +CheckConstraints bool
        +FireTriggers bool
        +KeepNulls bool
        +TableLock bool
        +RowsPerBatch int
        +KilobytesPerBatch int
        +Order []string
    }

    %% =========================================================
    %% Stored-procedure execution
    %% =========================================================
    class ProcParam {
        +In(name, value) ProcParam
        +Out(name, dest) ProcParam
        +InOut(name, dest) ProcParam
    }

    class ProcResult {
        +ReturnStatus int32
    }

    %% =========================================================
    %% Table and its children
    %% =========================================================
    class Table {
        +ObjectID int
        +Schema string
        +Name string
        +CreateDate time.Time
        +ModifyDate time.Time
        +HasReplicationFilter bool
        +IsMemoryOptimized bool
        +FullName() string
        +Columns() []*Column
        +Indexes() []*Index
        +ForeignKeys() []*ForeignKey
        +CheckConstraints() []*CheckConstraint
        +Statistics() []*Statistic
        +Partitions() []*Partition
        +Triggers() []*Trigger
        +RowCount() int64
        +TruncateTable() error
        +FragmentationStats(mode) []*FragStat
        +RebuildAllIndexes(fillFactor) error
        +UpdateAllStatistics(samplePct) error
        +CreateIndex(req) error
        +CreateStatistic(name, cols, pct) error
        +AddColumn(col) error
        +AlterColumn(col) error
        +DropColumn(name) error
    }

    class Column {
        +Name string
        +OrdinalPosition int
        +DataType DataType
        +MaxLength int
        +Precision int
        +Scale int
        +IsNullable bool
        +IsIdentity bool
        +IdentitySeed int64
        +IdentityIncrement int64
        +IsComputed bool
        +ComputedText string
        +DefaultValue *ColumnDefault
        +IsRowGUID bool
        +IsPrimaryKey bool
        +Collation string
    }

    class Index {
        +Name string
        +IndexID int
        +Type IndexType
        +IsClustered bool
        +IsUnique bool
        +IsPrimaryKey bool
        +IsDisabled bool
        +FillFactor int
        +KeyColumns []IndexColumn
        +IncludedColumns []IndexColumn
        +FilterDefinition string
        +Rebuild(t, fillFactor) error
        +Reorganize(t) error
        +Disable(t) error
        +Enable(t) error
        +Drop(t) error
    }

    class ForeignKey {
        +Name string
        +Columns []string
        +ReferencedTable string
        +ReferencedSchema string
        +ReferencedColumns []string
        +DeleteAction string
        +UpdateAction string
        +IsDisabled bool
    }

    class CheckConstraint {
        +Name string
        +Definition string
        +IsDisabled bool
        +Column string
    }

    class Statistic {
        +Name string
        +StatID int
        +IsAutoCreated bool
        +IsUserCreated bool
        +HasFilter bool
        +FilterDef string
        +LastUpdated time.Time
        +RowsSampled int64
        +TotalRows int64
        +Steps int
        +Update(samplePct) error
        +Drop() error
    }

    %% =========================================================
    %% Scripter (generates CREATE DDL for existing objects — distinct
    %% from ScriptCollector, which captures pending write statements)
    %% =========================================================
    class Scripter {
        -db *Database
        -opts ScriptOptions
        +NewScripter(db, opts) *Scripter
        +ScriptTable(schema, name) string
        +ScriptView(schema, name) string
        +ScriptStoredProcedure(schema, name) string
        +ScriptFunction(schema, name) string
        +ScriptDatabase() string
    }

    class ScriptOptions {
        +IncludeHeaders bool
        +IncludeIfNotExists bool
        +ScriptDrops bool
        +SchemaQualify bool
        +AnsiPadding bool
    }

    %% =========================================================
    %% Database objects
    %% =========================================================
    class Schema {
        +Name string
        +ID int
        +Owner string
    }

    class View {
        +ObjectID int
        +Schema string
        +Name string
        +Definition string
        +CreateDate time.Time
        +ModifyDate time.Time
    }

    class StoredProcedure {
        +ObjectID int
        +Schema string
        +Name string
        +Definition string
        +CreateDate time.Time
        +ModifyDate time.Time
    }

    class UserDefinedFunction {
        +ObjectID int
        +Schema string
        +Name string
        +FuncType string
        +Definition string
        +CreateDate time.Time
        +ModifyDate time.Time
    }

    class User {
        +Name string
        +ID int
        +UserType string
        +DefaultSchema string
        +AuthType string
        +CreateDate time.Time
        +ModifyDate time.Time
    }

    class DatabaseRole {
        +Name string
        +ID int
        +IsFixedRole bool
        +Owner string
        +Members []string
    }

    class FileGroup {
        +Name string
        +IsDefault bool
        +Files []DatabaseFile
    }

    class Trigger {
        +Name string
        +TableName string
        +Schema string
        +IsEnabled bool
        +Events []string
        +Definition string
    }

    class ServerRole {
        +Name string
        +IsFixedRole bool
        +Members []string
    }

    class LinkedServer {
        +Name string
        +Product string
        +Provider string
        +DataSource string
        +IsRemote bool
    }

    %% =========================================================
    %% Backup / Restore
    %% =========================================================
    class BackupOptions {
        +Database string
        +Devices []string
        +BackupType BackupType
        +CopyOnly bool
        +Compression bool
        +Checksum bool
        +Description string
        +Name string
        +MediaName string
        +Expiry time.Time
        +RetainDays int
        +BlockSize int
        +BufferCount int
        +MaxTransferSize int
        +Stats int
        +Init bool
        +Format bool
        +Progress func
        +BuildBackupStatement(opts) string
    }

    class RestoreOptions {
        +Database string
        +Devices []string
        +RestoreType RestoreType
        +RelocateFiles []RelocateFile
        +Recovery bool
        +Replace bool
        +Checksum bool
        +Stats int
        +StopAt time.Time
        +StopAtMarkName string
        +FileNumber int
    }

    %% =========================================================
    %% Agent Jobs
    %% =========================================================
    class AgentJob {
        +JobID string
        +Name string
        +Enabled bool
        +Description string
        +Steps []*JobStep
        +Schedules []*JobSchedule
        +AddStep(req) error
        +AddSchedule(req) error
        +Start(stepName) error
        +Stop() error
        +Drop() error
    }

    %% =========================================================
    %% Relationships
    %% =========================================================
    ConnectionOptions --> AuthMethod : uses
    ConnectionOptions --> KerberosOptions : configures AuthWindows via
    Server --> ConnectionOptions : created from
    Server --> ServerInfo : has
    Server "1" --> "*" Database : owns
    Server "1" --> "*" Login : owns
    Server "1" --> "*" ServerRole : owns
    Server "1" --> "*" LinkedServer : owns
    Server "1" --> "*" AgentJob : owns
    Server --> BackupOptions : accepts
    Server --> RestoreOptions : accepts
    Server --> ServerSecurityInfo : has
    Server "1" --> "*" ServerPermissionEntry : grants
    Server "1" --> "*" Credential : owns
    Server --> ServerMemoryStats : has
    Server "1" --> "*" Language : lists

    Login ..> nStringLiteral : password quoted by
    Login --> LoginDetails : has
    Login "1" --> "*" LoginUserMapping : mapped via

    Database "1" --> "*" Table : contains
    Database "1" --> "*" View : contains
    Database "1" --> "*" StoredProcedure : contains
    Database "1" --> "*" UserDefinedFunction : contains
    Database "1" --> "*" Schema : contains
    Database "1" --> "*" User : contains
    Database "1" --> "*" DatabaseRole : contains
    Database "1" --> "*" FileGroup : contains
    Database "1" --> "*" Trigger : contains
    Database "1" --> "*" DatabaseFileInfo : contains
    Database --> DatabaseOptions : has
    Database --> ChangeTrackingInfo : has
    Database "1" --> "*" TableChangeTracking : tracks
    Database "1" --> "*" Dependency : dependencies of
    Database "1" --> "*" SearchResult : search() returns
    Database --> ExecutionPlan : produces
    Database "1" --> "*" PermissionEntry : grants
    Database "1" --> "*" DatabasePermissionEntry : grants
    Database ..> BulkCopy : bulk-loads via
    Database ..> ProcParam : executes procs with
    Database --> ProcResult : returns

    Database ..> withConn : uses internally
    Database ..> rowsWithConn : query() returns
    Database ..> scanRow : single-row callback
    Database ..> queryRow : single-row with release()
    Database ..> withRetry : reads retried via

    withConn <.. rowsWithConn : conn acquired by
    withConn <.. scanRow : delegates to
    withConn <.. queryRow : delegates to
    withRetry <.. IsRetryable : same failure test as

    Table "1" --> "*" Column : has
    Table "1" --> "*" Index : has
    Table "1" --> "*" ForeignKey : has
    Table "1" --> "*" CheckConstraint : has
    Table "1" --> "*" Statistic : has
    Table "1" --> "*" Trigger : has

    Scripter --> Database : scripts objects from
    Scripter --> ScriptOptions : configured by

    ScriptCollector ..> Server : captures writes from
    ScriptCollector ..> Database : captures writes from
```

---

## Security

- **Passwords are never interpolated into SQL strings.** `CreateLogin` and `ChangePassword` encode the password as a UTF-16LE binary literal (`0x...`), making them injection-proof regardless of password content.
- **Connection lifetimes are correctly scoped.** Every `query()` call returns a `rowsWithConn` that holds the underlying `*sql.Conn` and releases it atomically on `Close()`, preventing silent connection leaks on early iteration exits.
- **One shared quoting implementation.** `QuoteName` and `QuoteLiteral` wrap the driver's own `TSQLQuoter`, so gosmo's internal identifier/literal escaping — and any caller or downstream consumer (e.g. gossms) building its own DDL — go through the same tested implementation rather than a hand-rolled one.
- **Permission and SET-option names are allowlisted, not interpolated.** `GRANT`/`DENY`/`REVOKE` and `ALTER DATABASE ... SET` are DDL and can't parameterize their keyword arguments; every method that accepts one (`GrantServerPermission`, `GrantPermission`, `GrantDatabasePermission`, `SetDatabaseOption`, ...) rejects any name not on its allowlist instead of splicing caller input directly into the statement.

---

## Packages

| Path        | Purpose                 |
| ----------- | ----------------------- |
| `/`         | All SMO types and logic |
| `examples/` | Full end-to-end demo    |

---

## Quick start

```go
import "github.com/radix29/gosmo"

srv, err := gosmo.Connect(gosmo.ConnectionOptions{
    Server:                 "localhost:1433",
    User:                   "sa",
    Password:               "YourPassword",
    TrustServerCertificate: true,
})
if err != nil { log.Fatal(err) }
defer srv.Close()

fmt.Println(srv.Info().ProductVersion)
```

---

## Feature map

### Server

| SMO equivalent          | gosmo                                      |
| ----------------------- | ------------------------------------------ |
| `Server.Databases`      | `srv.Databases()`                          |
| Current database         | `srv.CurrentDatabase()`                    |
| `Server.Logins`         | `srv.Logins()` / `srv.LoginByName(name)`   |
| `Server.Roles`          | `srv.ServerRoles()`                        |
| `Server.LinkedServers`  | `srv.LinkedServers()`                      |
| `Server.Configuration`  | `srv.Configurations()`                     |
| `Server.JobServer.Jobs` | `srv.Jobs()`                               |
| Active sessions         | `srv.ActiveSessions(includeSystem)`        |
| Kill session            | `srv.KillSession(id)`                      |
| Error log               | `srv.ReadErrorLog(n)`                      |
| Database Mail           | `srv.MailProfiles()` / `srv.SendMail(...)` |
| Create login (safe)     | `srv.CreateLogin(name, password, opts)`    |
| Authentication mode     | `srv.SecurityInfo()`                       |
| Server-level permissions | `srv.ServerPermissions()` / `srv.Grant\|Deny\|RevokeServerPermission(...)` |
| Credentials              | `srv.Credentials()`                        |
| Live memory stats        | `srv.MemoryStats()`                        |
| Languages                | `srv.Languages()`                          |

### Database

| SMO equivalent                  | gosmo                                       |
| ------------------------------- | ------------------------------------------- |
| Is a system database             | `db.IsSystem()`                             |
| `Database.Tables`               | `db.Tables()` / `db.TablesBySchema(schema)` |
| `Database.Views`                | `db.Views()`                                |
| `Database.StoredProcedures`     | `db.StoredProcedures()`                     |
| `Database.UserDefinedFunctions` | `db.UserDefinedFunctions()`                 |
| `Database.Schemas`              | `db.Schemas()`                              |
| `Database.Users`                | `db.Users()`                                |
| `Database.Roles`                | `db.DatabaseRoles()`                        |
| `Database.FileGroups`           | `db.FileGroups()`                           |
| `Database.Triggers`             | `db.Triggers()`                             |
| `Database.Sequences`            | `db.Sequences()`                            |
| `Database.Synonyms`             | `db.Synonyms()`                             |
| Partition functions             | `db.PartitionFunctions()`                   |
| Partition schemes               | `db.PartitionSchemes()`                     |
| Extended properties             | `db.ExtendedProperties(level)` / `db.AddExtendedProperty(...)` / `db.SetExtendedProperty(...)` / `db.DropExtendedProperty(...)` |
| Column master keys              | `db.ColumnMasterKeys()`                     |
| Column encryption keys          | `db.ColumnEncryptionKeys()`                 |
| Security policies (RLS)         | `db.SecurityPolicies()`                     |
| `Database.RecoveryModel`        | `db.SetRecoveryModel(model)`                |
| `Database.CompatibilityLevel`   | `db.SetCompatibilityLevel(level)`           |
| Space used                      | `db.SpaceUsed()`                            |
| ALTER DATABASE SET options      | `db.Options()` / `db.SetDatabaseOption(opt, value)` |
| Change ownership                | `db.SetOwner(principal)`                    |
| Every file, incl. log           | `db.Files()`                                |
| Add / alter / remove file       | `db.AddFile(spec)` / `db.AlterFile(name, m)` / `db.RemoveFile(name)` |
| Add / remove filegroup          | `db.AddFileGroup(name)` / `db.RemoveFileGroup(name)` |
| Filegroup default / read-only   | `db.SetDefaultFileGroup(name)` / `db.SetFileGroupReadOnly(name, ro)` |
| Change tracking                 | `db.ChangeTracking()` / `db.SetChangeTracking(info)` |
| Table change tracking           | `db.TableChangeTracking()` / `db.SetTableChangeTracking(...)` |
| Database-level permissions      | `db.DatabasePermissions()` / `db.Grant\|Deny\|RevokeDatabasePermission(...)` |

### Table

| SMO equivalent        | gosmo                              |
| --------------------- | ---------------------------------- |
| `Table.Columns`       | `t.Columns()`                      |
| `Table.Indexes`       | `t.Indexes()`                      |
| `Table.ForeignKeys`   | `t.ForeignKeys()`                  |
| `Table.Checks`        | `t.CheckConstraints()`             |
| `Table.Statistics`    | `t.Statistics()`                   |
| `Table.Partitions`    | `t.Partitions()`                   |
| `Table.Triggers`      | `t.Triggers()`                     |
| `Table.RowCount`      | `t.RowCount()`                     |
| Truncate              | `t.TruncateTable()`                |
| Fragmentation         | `t.FragmentationStats(mode)`       |
| Rebuild all indexes   | `t.RebuildAllIndexes(fillFactor)`  |
| Update all statistics | `t.UpdateAllStatistics(samplePct)` |
| Create index          | `t.CreateIndex(req)`               |
| Add column            | `t.AddColumn(col)`                 |
| Alter column          | `t.AlterColumn(col)`               |
| Drop column           | `t.DropColumn(name)`               |

### Index

| gosmo                               |
| ----------------------------------- |
| `idx.Rebuild(t, fillFactor)`        |
| `idx.Reorganize(t)`                 |
| `idx.Disable(t)` / `idx.Enable(t)` |
| `idx.Drop(t)`                       |

### Login

| gosmo                                   |
| --------------------------------------- |
| `srv.CreateLogin(name, password, opts)` |
| `login.ChangePassword(newPassword)`     |
| `login.Enable()` / `login.Disable()`   |
| `login.AddServerRoleMember(role)`       |
| `login.RemoveServerRoleMember(role)`    |
| `login.Drop()`                          |
| `login.Rename(newName)`                 |
| `login.SetDefaultDatabase(name)` / `login.SetDefaultLanguage(name)` |
| `login.SetPasswordPolicy(checkPolicy, checkExpiration)` |
| `login.ChangePasswordWithOptions(pw, mustChange, unlock)` |
| `login.MapCredential(name)` / `login.UnmapCredential(name)` |
| `login.Details()` — locked/expired/policy/last-login status |
| `login.UserMappings()` / `login.MapToDatabase(...)` / `login.UnmapFromDatabase(db)` |

### Dependencies, search, permissions, and execution plans

| SMO / SSMS equivalent      | gosmo                                                     |
| --------------------------- | ---------------------------------------------------------- |
| Object dependencies (uses)  | `db.Dependencies(schema, name)`                            |
| Object dependencies (used by) | `db.Dependents(schema, name)`                            |
| Object search                | `db.Search(pattern)`                                      |
| Object permissions           | `db.Permissions(schema, name)`                            |
| Grant / deny / revoke        | `db.GrantPermission(...)` / `db.DenyPermission(...)` / `db.RevokePermission(...)` |
| Estimated execution plan     | `db.EstimatedPlan(sql)` (`SET SHOWPLAN_XML`, statement not run) |
| Actual execution plan        | `db.ActualPlan(sql)` (`SET STATISTICS XML`, statement runs)|

### Scripter

```go
sc := gosmo.NewScripter(db, gosmo.DefaultScriptOptions())
ddl, _ := sc.ScriptTable("dbo", "MyTable")
ddl, _ := sc.ScriptView("dbo", "MyView")
ddl, _ := sc.ScriptStoredProcedure("dbo", "MyProc")
ddl, _ := sc.ScriptFunction("dbo", "MyFunc")
ddl, _ := sc.ScriptDatabase()
```

### Scripting pending writes (`WithScript`)

Distinct from the Scripter above (which generates CREATE DDL for objects
that already exist): `WithScript` captures the exact statement(s) a set of
*pending* write calls would run, without running them — for an editor-style
"preview the SQL" or "script my changes instead of applying them" action.

```go
ctx, script := gosmo.WithScript(context.Background())

srv.GrantServerPermissionContext(ctx, "CONNECT SQL", "app_user")
db.SetDatabaseOptionContext(ctx, gosmo.DBOptAutoShrink, "ON")

for _, stmt := range script.Statements {
    fmt.Println(stmt) // never executed against the server
}
```

Every write method in the package funnels through one of two chokepoints
(`Server.execContext`, `Database.exec`); `WithScript` intercepts there, so
this works for any write call, not just an allowlisted subset. Database-
scoped statements carry their own `USE [db];` prefix, since the caller may
run the resulting script against a session scoped to a different database
(or none) than the one that produced it. Read methods are unaffected —
only the two exec chokepoints consult the collector.

### Backup & Restore

```go
srv.Backup(gosmo.BackupOptions{
    Database: "MyDB",
    Devices:  []string{`C:\Backups\MyDB.bak`},
    CopyOnly: true,
    // Optional: receive "N percent processed" notices as the backup runs
    // (Stats defaults to 10 automatically once Progress is set).
    Progress: func(pct int, message string) { fmt.Println(pct, message) },
})

srv.Restore(gosmo.RestoreOptions{
    Database: "MyDB_Restored",
    Devices:  []string{`C:\Backups\MyDB.bak`},
    RelocateFiles: []gosmo.RelocateFile{
        {LogicalName: "MyDB",     PhysicalName: `C:\Data\MyDB.mdf`},
        {LogicalName: "MyDB_log", PhysicalName: `C:\Data\MyDB.ldf`},
    },
    Recovery: true,
    Replace:  true,
})
```

### Agent Jobs

```go
job, _ := srv.CreateJob(gosmo.CreateJobRequest{Name: "NightlyBackup", Enabled: true})
job.AddStep(gosmo.JobStepRequest{
    Name:            "Run backup",
    Subsystem:       "TSQL",
    Command:         "EXEC dbo.RunNightlyBackup",
    Database:        "MyDB",
    OnSuccessAction: 1,
    OnFailAction:    2,
})
job.AddSchedule(gosmo.JobScheduleRequest{
    Name:            "Every night at 2am",
    Enabled:         true,
    FreqType:        4,     // daily
    FreqInterval:    1,
    FreqSubdayType:  1,     // once
    ActiveStartTime: 20000, // 02:00:00
})
job.Start("")
```

### Bulk copy

Streams rows into a table over the TDS bulk-copy protocol — the same fast
path `bcp` and SSMS's "Import Data" use, far faster than row-by-row
`INSERT`s.

```go
n, err := db.BulkInsert(gosmo.BulkCopy{
    Table:   "Orders",
    Columns: []string{"OrderID", "CustomerID", "OrderDate"},
    Options: gosmo.BulkOptions{TableLock: true},
}, gosmo.SliceRows(rows)) // or your own iter.Seq2[[]any, error], e.g. a CSV reader
```

### Execute stored procedures

Runs a stored procedure as an RPC, so `OUTPUT` parameters and the return
status come back to the caller — unlike a plain `db.Exec`-style call.

```go
var rowsAffected int
result, err := db.ExecProc("dbo", "usp_UpdateStock",
    gosmo.In("ProductID", 42),
    gosmo.Out("RowsAffected", &rowsAffected),
)
fmt.Println(result.ReturnStatus, rowsAffected)
```

---

## Errors

`AsSQLError` unwraps a driver error into a structured `SQLError` — number,
severity class, state, originating procedure/line, and (for a batch that
raised more than one) the full `All` list — without callers needing to
import the underlying driver package themselves.

```go
if _, err := db.CreateTable(req); err != nil {
    if sqlErr, ok := gosmo.AsSQLError(err); ok {
        fmt.Println(sqlErr.Header()) // "Msg 2714, Level 16, State 6, Line 1"
    }
}
```

---

## Authentication

`ConnectionOptions.Auth` selects the authentication method:

| Constant                           | When to use                                       |
| ---------------------------------- | ------------------------------------------------- |
| `AuthSQLServer` (default)          | SQL Server login + password                       |
| `AuthWindows`                      | Windows / Kerberos (domain-joined host)           |
| `AuthEntraMSI`                     | Azure Managed Identity (system- or user-assigned) |
| `AuthEntraServicePrincipal`        | Service principal with secret or certificate      |
| `AuthEntraPassword`                | Entra ID user + password (non-interactive)        |
| `AuthEntraInteractive`             | Browser-based interactive login                   |
| `AuthEntraDeviceCode`              | Device code flow                                  |
| `AuthEntraDefault`                 | Default credential chain (env → MSI → AzCLI)     |
| `AuthEntraAzCLI`                   | `az login` credential                             |
| `AuthEntraAzurePipelines`          | Azure DevOps pipeline OIDC                        |

`AuthWindows` uses native SSPI on Windows. On every other platform it
authenticates via Kerberos instead — run `kinit` first for ambient
single sign-on, or set `ConnectionOptions.Kerberos` (`KerberosOptions`)
for a keytab, realm, credential cache, or custom `krb5.conf`.
`ConnectionOptions.ServerSPN` overrides the target SPN when the driver's
own derivation from the address doesn't match (e.g. a load balancer or
CNAME in front of the instance).

`ConnectionOptions.AccessTokenProvider`, when set, is called to obtain a
bearer token for each new pooled connection — use it instead of the
static `AccessToken` field for tokens that expire during the connection's
lifetime (Entra tokens are good for roughly an hour). It takes precedence
over both `AccessToken` and `Auth`.

`ConnectionOptions.SessionInitSQL` runs on every pooled connection right
after it is reset, before the first query — the equivalent of SSMS's
Query Execution `SET` options (e.g. `"SET ARITHABORT ON; SET ANSI_NULLS ON"`).

`gosmo.ParseServerAddress(server)` parses any address form SSMS's own
"Server name" field accepts — `host`, `host:port`, `host,port`,
`host\instance`, `host\instance,port` — into `(host, instance, port)`.
Exported so a caller building its own connection-address UI can reuse the
same parsing `Connect`/`ConnectContext` rely on internally.

---

## Connection helpers (internal)

| Helper         | Purpose                                                                                   |
| -------------- | ----------------------------------------------------------------------------------------- |
| `withConn`     | Acquires a `*sql.Conn`, runs `USE <db>`, executes a callback, then releases the conn.    |
| `query`        | Returns `*rowsWithConn`; `Close()` releases both rows **and** the underlying connection. |
| `queryRow`     | Returns `(*sql.Row, func(), error)`; always `defer release()` before scanning.           |
| `scanRow`      | Callback-style single-row helper; conn is fully internal, no release needed.             |
| `exec`         | Thin wrapper over `withConn` for non-SELECT statements.                                   |
| `withRetry`    | Retries `Database.query`/`queryRow` up to 3 times (linear backoff) on a transient/dropped-connection failure — reads only, since retrying is only safe when the operation is idempotent. |

`gosmo.IsRetryable(err)` exposes the same retryable/dropped-connection
test `withRetry` uses, for callers running their own statements outside
gosmo's query helpers.

---

## Running the example

```
export MSSQL_SERVER="localhost:1433"
export MSSQL_USER="sa"
export MSSQL_PASSWORD="YourPassword"
go run ./examples/main.go
```

---

## Features intentionally excluded (require WMI / COM / OS APIs)

- Hardware enumeration (disk, NIC, CPU details beyond what `sys.dm_os_sys_info` provides)
- SQL Server service start/stop/restart
- Performance counters via Windows PDH
- SQL Server Browser service interaction
- Windows Event Log reading
- Registry reads for SQL Server configuration outside `sys.configurations`

All of the above require WMI or Windows-only APIs and are out of scope for a cross-platform Go library.

---

## Contributing

The codebase is currently unstable and going through regular refactoring,
so I'm not accepting pull requests at this time — please open an issue
instead. I'll start accepting PRs once the project reaches a released,
more stable state. In the near future I'm planning to update the project
regularly.


