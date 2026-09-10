# gosmo

SQLServer Management Objects Library
A Go library that mimics **Microsoft SQL Server Management Objects (SMO)** — without WMI, COM, or Windows-only dependencies.

```
go get github.com/radix29/gosmo
```

> **Go version note:** The module requires Go 1.27

## Supported SQL Server versions

**SQL Server 2016 SP1 (13.0.4001) and later**, on Windows and on Linux.
`gosmo.MinimumServerVersion` states the floor in code.

The floor is SP1 rather than 2016 RTM for one reason: `CREATE OR ALTER`
arrived in 13.0.4001, and `Database.CreateStoredProcedure` emits it
unconditionally (`procedure.go`) — the only statement gosmo builds that
2016 RTM cannot parse. Every catalog read gosmo makes would run on RTM.
Supporting it means rewriting that one statement as
`IF EXISTS … ALTER … ELSE CREATE`; that is a deliberate decision, not an
oversight, and `TestOnlyKnownSitesEmitCreateOrAlter` fails if a second
such statement appears without one.

`Scripter`'s module scripting is not a second site: `alterModuleDefinition`
recognises a `CREATE OR ALTER` that the *server's own* stored definition
already contains and passes it through unchanged. That text is the
author's, not gosmo's.

Azure SQL Managed Instance is supported and is version-gated separately.
It reports a frozen `ProductVersion` — 12.0.2000.8, SQL Server 2014 — while
running an 18.x engine that has every catalog column gosmo gates on 2016
through 2022, so `ServerInfo.IsAzure()` routes it past `VersionMajor` and
into the "treat as newest" branch below. `Info().VersionMajor` keeps the 12
the server actually said, for callers that display a version.

Columns and syntax added after the floor are gated rather than assumed —
`colSince` substitutes a typed literal for a column the instance lacks, so
a caller gets a zero value instead of a failed read, and a statement an
older parser rejects is refused before it is sent (see
`Database.EnclaveComputationsSupported`, `Database.QueryStoreWaitStatsSupported`),
wrapping `ErrUnsupportedVersion`. An instance whose version was never read
is treated as newest.

Every gate is pinned three ways, because a wrong one is invisible on the
instance it was written against: `testdata/version_gates/*.sql` holds each
gated `SELECT` list rendered at majors 13 through 17 (regenerate with
`go test -run TestGatedQueries -updategolden`, then read the diff — regenerating is
not review), a hand-written inventory is kept in step with the call sites,
and the live-instance sweep asks a real server whether each gate decided
correctly for its own major.

---

## Architecture

The class map is split across five diagrams, one per area. It is one map,
not five — a single Mermaid diagram stops rendering above 50,000 characters
and the whole thing is well past that, so an edge that crosses areas is drawn
in the diagram of the area it points *into* (`Server --> AvailabilityGroup`
is in the high-availability one, where `Server` shows up as a bare box).
`gosmo.mermaid` holds the same five diagrams, concatenated in order, and is
kept byte-identical to what is below.

### Connecting, the server, and logins

The entry point, authentication, server-level security and the login
object — plus the internal connection helpers every read and write goes
through, and the `WithScript` collector that can capture a write instead of
running it.

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
        +ApplicationClientID string
        +EntraCache *EntraCache
        +DeviceCodePrompt func
        +ServerSPN string
        +Kerberos KerberosOptions
        +ConnectTimeout Duration
        +ApplicationName string
        +MaxOpenConns int
        +MaxIdleConns int
        +ConnMaxLifetime Duration
        +ConnMaxIdleTime Duration
        +SessionInitSQL string
        +Dialer mssql.Dialer
        +TrustServerCertificate bool
        +Encrypt string
        +HostNameInCertificate string
        +ExtraParams url.Values
        +ConnectionString(maskSecrets) string
    }

    class Server {
        -db *sql.DB
        -info *ServerInfo
        +Connect(opts) *Server
        +ConnectContext(ctx, opts) *Server
        +NewServer(ctx, db) *Server
        +ParseServerAddress(server) string
        +Close() error
        +DB() *sql.DB
        +Info() *ServerInfo
        +Name() string
        +CurrentDatabase() string
        +CurrentLogin() string
        +Databases() []*Database
        +DatabaseByName(name) *Database
        +Database(name) *Database
        +CreateDatabase(name, opts) error
        +DropDatabase(name, force) error
        +RenameDatabase(oldName, newName, force) error
        +DetachDatabase(name, opts) error
        +AttachDatabase(spec) error
        +DetachedDatabaseInfo(primaryFilePath) *DetachedDatabase
        +Logins() []*Login
        +LoginByName(name) *Login
        +Login(name) *Login
        +CreateLogin(name, password, opts) error
        +DropLogin(name) error
        +ServerRoles() []*ServerRole
        +ServerRoleByName(name) *ServerRole
        +DropServerRole(name) error
        +ServerRoleMembers(role) []*RoleMember
        +AddServerRoleMember(role, member) error
        +RemoveServerRoleMember(role, member) error
        +LinkedServers() []*LinkedServer
        +Configurations() []*Configuration
        +AgentInfo() *AgentStatus
        +Jobs() []*Job
        +JobByName(name) *Job
        +Job(name) *Job
        +CreateJob(req) *Job
        +JobHistory(limit) []*JobHistoryEntry
        +Alerts() []*Alert
        +EventAlerts() []*Alert
        +AlertByName(name) *Alert
        +Alert(name) *Alert
        +CreateAlert(req) *Alert
        +Operators() []*Operator
        +OperatorByName(name) *Operator
        +Operator(name) *Operator
        +CreateOperator(req) *Operator
        +Schedules() []*Schedule
        +ScheduleByName(name) *Schedule
        +Schedule(name) *Schedule
        +CreateSchedule(req) *Schedule
        +Categories(class) []*Category
        +CreateCategory(class, name) error
        +DeleteCategory(class, name) error
        +ActiveSessions(sys) []*Session
        +KillSession(id) error
        +EnumErrorLogs(logType) []*ErrorLogFile
        +ReadLog(logType, n) []*ErrorLogEntry
        +ReadLogFiltered(logType, n, search) []*ErrorLogEntry
        +ReadErrorLog(n) []*ErrorLogEntry
        +CycleLog(logType) error
        +CycleErrorLog() error
        +EnumFileSystem(path) []*FileSystemEntry
        +EnumFileSystemIsLegacy() bool
        +FixedDrives() []*FixedDrive
        +FileSystemExists(path) bool
        +MailProfiles() []*MailProfile
        +SendMail(opts) error
        +Backup(opts) error
        +Restore(opts) error
        +VerifyBackup(device) error
        +BackupHeaders(device) []*BackupHeader
        +BackupFileList(device) []*BackupFile
        +BackupFileListForSet(device, fileNumber) []*BackupFile
        +VerifyBackupFrom(target) error
        +BackupHeadersFrom(target) []*BackupHeader
        +BackupFileListForSetFrom(target, fileNumber) []*BackupFile
        +BackupDevices() []*BackupDevice
        +BackupDeviceByName(name) *BackupDevice
        +BackupDevice(name) *BackupDevice
        +CreateBackupDevice(name, devType, physicalName) *BackupDevice
        +DatabaseFiles(database) []*DatabaseFileInfo
        +DatabaseRecoveryStatuses() []*DatabaseRecoveryStatus
        +Capabilities() *Capabilities
        +SecurityInfo() *ServerSecurityInfo
        +ServerPermissions() []*ServerPermissionEntry
        +GrantServerPermissionWithOptions(perm, principal, opts) error
        +DenyServerPermissionWithOptions(perm, principal, opts) error
        +RevokeServerPermissionWithOptions(perm, principal, opts) error
        +EffectiveServerPermissions(login) []*EffectivePermission
        +GrantServerPermission(perm, principal) error
        +DenyServerPermission(perm, principal) error
        +RevokeServerPermission(perm, principal) error
        +ServerPermissionNames() []string
        +Credentials() []*Credential
        +CredentialByName(name) *Credential
        +Credential(name) *Credential
        +CreateCredential(spec) *Credential
        +CryptographicProviders() []*CryptographicProvider
        +ServerAudits() []*ServerAudit
        +ServerAuditByName(name) *ServerAudit
        +ServerAudit(name) *ServerAudit
        +CreateServerAudit(spec) *ServerAudit
        +ServerAuditSpecifications() []*ServerAuditSpecification
        +ServerAuditSpecificationByName(name) *ServerAuditSpecification
        +ServerAuditSpecification(name) *ServerAuditSpecification
        +CreateServerAuditSpecification(spec) *ServerAuditSpecification
        +AuditActionGroups() []string
        +DatabaseAuditActionGroups() []string
        +DatabaseAuditActions() []string
        +ServerResourceStats(max) []*ServerResourceStat
        +LatestServerResourceStats() *ServerResourceStat
        +InstanceResourceGovernance() *InstanceResourceGovernance
        +OSJobObject() *OSJobObject
        +ServerTriggers() []*ServerTrigger
        +ServerTriggerByName(name) *ServerTrigger
        +ServerTrigger(name) *ServerTrigger
        +Endpoints() []*Endpoint
        +EndpointByName(name) *Endpoint
        +MemoryStats() *ServerMemoryStats
        +Languages() []*Language
        +ProcessorInfo() *ProcessorInfo
        +DiskVolumes() []DiskVolumeInfo
        +AvailabilityGroups() []*AvailabilityGroup
        +AvailabilityGroupByName(name) *AvailabilityGroup
        +AvailabilityGroup(name) *AvailabilityGroup
        +CreateAvailabilityGroup(req) *AvailabilityGroup
        +DatabaseMirroringEndpoint() *DatabaseMirroringEndpoint
        +CreateDatabaseMirroringEndpoint(spec) *DatabaseMirroringEndpoint
    }

    class ServerInfo {
        +Name string
        +Edition string
        +ProductVersion string
        +ProductLevel string
        +Collation string
        +IsClustered bool
        +IsHADREnabled bool
        +IsSingleUser bool
        +EngineEdition int
        +OSVersion string
        +Platform string
        +PhysicalMemoryMB int64
        +LogicalCPUCount int
        +SysInfoUnavailable bool
        +DefaultDataPath string
        +DefaultLogPath string
        +DefaultBackupPath string
        +VersionMajor int
        +VersionMinor int
        +VersionBuild int
        +IsAzure() bool
        EngineEdition is SERVERPROPERTY('EngineEdition');
        IsAzure folds the five Azure-hosted ones.
        ProductVersion is frozen on Azure — a Managed
        Instance answers 12.0.2000.8 while running an
        18.x engine, so no version gate may believe it.
    }

    class EngineEdition {
        <<enumeration>>
        EnginePersonal
        EngineStandard
        EngineEnterprise
        EngineExpress
        EngineAzureSQLDatabase
        EngineAzureSynapse
        EngineAzureManagedInst
        EngineAzureSQLEdge
        EngineAzureSynapseSrvls
        +IsAzure() bool
    }

    %% =========================================================
    %% Authentication
    %% =========================================================
    class AuthMethod {
        <<enumeration>>
        AuthSQLServer
        AuthWindows
        AuthEntraDefault
        AuthEntraPassword
        AuthEntraMSI
        AuthEntraServicePrincipal
        AuthEntraServicePrincipalAccessToken
        AuthEntraIntegrated
        AuthEntraInteractive
        AuthEntraDeviceCode
        AuthEntraAzCLI
        AuthEntraAzureDeveloperCLI
        AuthEntraAzurePipelines
        AuthEntraOnBehalfOf
        +String() string
    }

    class EntraCache {
        +NewEntraCache() *EntraCache
        +Warm(ctx, opts) error
        +Clear()
        One sign-in per identity, shared by
        every connection and Server given it.
        In memory only; never keyed by server.
    }

    class DeviceCodeMessage {
        +UserCode string
        +VerificationURL string
        +Message string
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
        -server *Server
        +CredentialID int
        +Name string
        +Identity string
        +TargetType string
        +CryptographicProvider string
        +CreateDate time.Time
        +ModifyDate time.Time
        +Alter(identity, secret) error
        +Drop() error
        The secret is write-only: sys.credentials
        never exposes it, so Alter takes a *string —
        and nil clears the stored secret, because
        ALTER CREDENTIAL resets both halves.
    }

    class CredentialSpec {
        +Name string
        +Identity string
        +Secret string
        +CryptographicProvider string
    }

    class DatabaseScopedCredential {
        -db *Database
        +CredentialID int
        +Name string
        +Identity string
        +CreateDate time.Time
        +ModifyDate time.Time
        +Alter(identity, secret) error
        +Drop() error
        The database-scope family, a separate
        securable with its own DDL. The secret
        is write-only here for the same reason.
    }

    class DatabaseScopedCredentialSpec {
        +Name string
        +Identity string
        +Secret string
    }

    class CryptographicProvider {
        +ProviderID int
        +Name string
        +GUID string
        +Version string
        +DLLPath string
        +IsEnabled bool
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

    class ProcessorInfo {
        +CPUCount int
        +HyperthreadRatio int
        +NUMANodeCount int
        +CPUNUMANode []int
    }

    class DiskVolumeInfo {
        +MountPoint string
        +VolumeName string
        +SamplePath string
        +TotalMB float64
        +AvailableMB float64
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
        +MappedObject string
        +ResolveMapping() error
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
        +BadPasswordTime time.Time
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
        +Table(schema, name) *Table
        +CreateTable(req) error
        +DropTable(schema, name, cascade) error
        +RenameTable(schema, oldName, newName) error
        +RenameObject(schema, oldName, newName) error
        +Catalog() *Catalog
        +SystemCatalog() *Catalog
        +TablesFiltered(filter) []*Table
        +TransferObject(targetSchema, schema, name) error
        +Views() []*View
        +ViewsFiltered(filter) []*View
        +DropView(schema, name) error
        +StoredProcedures() []*StoredProcedure
        +StoredProceduresFiltered(filter) []*StoredProcedure
        +CreateStoredProcedure(schema, name, body) error
        +DropStoredProcedure(schema, name) error
        +UserDefinedFunctions() []*UserDefinedFunction
        +UserDefinedFunctionsFiltered(filter) []*UserDefinedFunction
        +DropFunction(schema, name) error
        +Parameters(schema, name) []*Parameter
        +SystemViews() []*View
        +SystemViewsFiltered(filter) []*View
        +SystemStoredProcedures() []*StoredProcedure
        +SystemStoredProceduresFiltered(filter) []*StoredProcedure
        +SystemFunctions() []*UserDefinedFunction
        +SystemFunctionsFiltered(filter) []*UserDefinedFunction
        +Schemas() []*Schema
        +SchemaByName(name) *Schema
        +CreateSchema(name, owner) error
        +DropSchema(name) error
        +Users() []*User
        +UserByName(name) *User
        +CreateUser(user, login, schema) error
        +DropUser(name) error
        +DatabaseAuditSpecifications() []*DatabaseAuditSpecification
        +DatabaseAuditSpecificationByName(name) *DatabaseAuditSpecification
        +DatabaseAuditSpecification(name) *DatabaseAuditSpecification
        +CreateDatabaseAuditSpecification(spec) *DatabaseAuditSpecification
        +DatabaseRoles() []*DatabaseRole
        +RoleByName(name) *DatabaseRole
        +DropDatabaseRole(name) error
        +RoleMembers(role) []*RoleMember
        +AddRoleMember(role, member) error
        +RemoveRoleMember(role, member) error
        +FileGroups() []*FileGroup
        +Triggers() []*Trigger
        +ObjectTriggers(schema, name) []*Trigger
        +DropTrigger(schema, name) error
        +DatabaseTriggers() []*DatabaseTrigger
        +DatabaseTriggerByName(name) *DatabaseTrigger
        +DatabaseTrigger(name) *DatabaseTrigger
        +Sequences() []*Sequence
        +DropSequence(schema, name) error
        +Synonyms() []*Synonym
        +DropSynonym(schema, name) error
        +PartitionFunctions() []*PartitionFunction
        +PartitionFunctionByName(name) *PartitionFunction
        +PartitionSchemes() []*PartitionScheme
        +PartitionSchemeByName(name) *PartitionScheme
        +ExtendedProperties(level) []*ExtendedProperty
        +AddExtendedProperty(name, value, level) error
        +SetExtendedProperty(name, value, level) error
        +DropExtendedProperty(name, level) error
        +Certificates() []*Certificate
        +CertificateByName(name) *Certificate
        +CreateCertificate(spec) error
        +AsymmetricKeys() []*AsymmetricKey
        +AsymmetricKeyByName(name) *AsymmetricKey
        +HasMasterKey() bool
        +CreateMasterKey(password) error
        +ColumnMasterKeys() []*ColumnMasterKey
        +ColumnMasterKeyByName(name) *ColumnMasterKey
        +CreateColumnMasterKeyWithSignature(name, provider, path, sig) error
        +ColumnEncryptionKeys() []*ColumnEncryptionKey
        +ColumnEncryptionKeyByName(name) *ColumnEncryptionKey
        +CreateColumnEncryptionKey(name, values) error
        +SecurityPolicies() []*SecurityPolicy
        +SecurityPolicyByName(schema, name) *SecurityPolicy
        +DatabaseScopedCredentials() []*DatabaseScopedCredential
        +DatabaseScopedCredentialByName(name) *DatabaseScopedCredential
        +DatabaseScopedCredential(name) *DatabaseScopedCredential
        +CreateDatabaseScopedCredential(spec) *DatabaseScopedCredential
        +Capabilities() *DatabaseCapabilities
        +RecoveryStatus() *DatabaseRecoveryStatus
        +SpaceUsed() SpaceInfo
        +TableRowCounts() map~int,int64~
        +TableSpaceUsedAll() map~int,TableSpaceInfo~
        +SetRecoveryModel(model) error
        +SetCompatibilityLevel(level) error
        +SetReadOnly(bool) error
        +SetUserAccess(mode) error
        +SetOffline() error
        +SetOnline() error
        +Options() *DatabaseOptions
        +SetDatabaseOption(opt, value) error
        +SetOwner(principal) error
        +DatabaseScopedConfigs() []*DatabaseScopedConfig
        +SetDatabaseScopedConfig(name, value, forSecondary) error
        +QueryStore() *QueryStoreInfo
        +SetQueryStoreOptions(opts) error
        +FlushQueryStore() error
        +ClearQueryStore() error
        +QueryStoreTopResourceQueries(opts) []*QSQueryStat
        +QueryStoreRegressedQueries(opts) []*QSQueryStat
        +QueryStoreHighVariationQueries(opts) []*QSQueryStat
        +QueryStoreForcedPlanQueries(opts) []*QSQueryStat
        +QueryStoreOverallConsumption(opts) []*QSIntervalStat
        +QueryStoreTrackedQuery(queryID, opts) []*QSPlanIntervalStat
        +QueryStoreWaitCategories(opts) []*QSWaitStat
        +QueryStoreWaitingQueries(category, opts) []*QSQueryStat
        +QueryStorePlans(queryID, opts) []*QSPlan
        +QueryStoreQueryText(queryID) (string, string)
        +QueryStoreForcePlan(queryID, planID) error
        +QueryStoreUnforcePlan(queryID, planID) error
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
        +TableChangeTrackingFor(schema, name) *TableChangeTracking
        +SetTableChangeTracking(schema, name, enable, cols) error
        +Dependencies(schema, name) []*Dependency
        +Dependents(schema, name) []*Dependency
        +Search(pattern) []*SearchResult
        +FindSecurables(search) []SecurableRef
        +ObjectColumns(schema, name) []*Column
        +Permissions(schema, name) []*PermissionEntry
        +GrantPermission(schema, name, perm, principal) error
        +DenyPermission(schema, name, perm, principal) error
        +RevokePermission(schema, name, perm, principal) error
        +PermissionsForPrincipal(principal) []*PrincipalSecurable
        +GrantPermissionWithOptions(schema, name, perm, principal, opts) error
        +DenyPermissionWithOptions(schema, name, perm, principal, opts) error
        +RevokePermissionWithOptions(schema, name, perm, principal, opts) error
        +ColumnPermissions(schema, name) []*ColumnPermissionEntry
        +ColumnPermissionsForPrincipal(principal) []*ColumnPermissionEntry
        +GrantColumnPermission(schema, name, perm, cols, principal) error
        +DenyColumnPermission(schema, name, perm, cols, principal) error
        +RevokeColumnPermission(schema, name, perm, cols, principal) error
        +GrantColumnPermissionWithOptions(schema, name, perm, cols, principal, opts) error
        +DenyColumnPermissionWithOptions(schema, name, perm, cols, principal, opts) error
        +RevokeColumnPermissionWithOptions(schema, name, perm, cols, principal, opts) error
        +EffectivePermissions(principal) []*EffectivePermission
        +EffectiveObjectPermissions(schema, name, principal) []*EffectivePermission
        +EffectiveSchemaPermissions(schema, principal) []*EffectivePermission
        +SchemaPermissions(schema) []*PermissionEntry
        +GrantSchemaPermission(schema, perm, principal) error
        +DenySchemaPermission(schema, perm, principal) error
        +RevokeSchemaPermission(schema, perm, principal) error
        +GrantSchemaPermissionWithOptions(schema, perm, principal, opts) error
        +DenySchemaPermissionWithOptions(schema, perm, principal, opts) error
        +RevokeSchemaPermissionWithOptions(schema, perm, principal, opts) error
        +DatabasePermissions() []*DatabasePermissionEntry
        +GrantDatabasePermission(perm, principal) error
        +DenyDatabasePermission(perm, principal) error
        +RevokeDatabasePermission(perm, principal) error
        +GrantDatabasePermissionWithOptions(perm, principal, opts) error
        +DenyDatabasePermissionWithOptions(perm, principal, opts) error
        +RevokeDatabasePermissionWithOptions(perm, principal, opts) error
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
        Executes USE db, retried on a
        transient failure. Runs callback
        fn(*sql.Conn) — NOT retried, since
        fn is the caller's actual write.
        Releases conn via defer.
        Used by Database.exec.
    }

    class dbRows {
        <<internal type>>
        -Rows *sql.Rows
        -conn *sql.Conn
        +Close() error
        Closes Rows then the conn pinned
        for them. *sql.Rows.Close alone
        leaves the conn checked out of the
        pool forever. Returned by
        Database.query().
    }

    class DatabaseQueryRow {
        <<internal helper>>
        Database.queryRow(ctx, scan, q, args)
        Acquires conn, runs USE, hands the row
        to scan — all inside one retry unit.
        scan must run inside it: QueryRowContext
        never errors, only Scan does.
    }

    class ServerQuery {
        <<internal helpers>>
        Server.query(ctx, q, args)
        Server.queryRow(ctx, scan, q, args)
        Server.queryRowScan(ctx, q, args, dest)
        Server-scoped counterparts, with no USE
        to redo. queryRowScan is the bare-Scan
        convenience over queryRow.
    }

    class withRetry {
        <<internal helper>>
        Generic retry wrapper for idempotent
        reads only. 3 attempts, linear backoff
        of attempt times 50ms. Used by every
        query/queryRow helper above.
    }

    class IsRetryable {
        <<package function>>
        True for the driver's RetryableError, a
        dropped pooled connection (ErrBadConn),
        a net.Error, a corrupted TDS stream, a
        connection-severing ServerError, or EOF
        — including wrapped errors. Exported so
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
    class ErrNotFound {
        <<sentinel>>
        The error every by-name lookup that
        reports absence wraps, so errors.Is
        separates "does not exist" from
        "the lookup itself failed".
        CertificateByName returns (nil, nil)
        instead, and AgentStatus reports an
        unreachable Agent as a value.
    }

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
    %% What the connected login may do
    %% =========================================================
    class Capabilities {
        +ServerRoles map~string,bool~
        +ServerPermissions map~string,CapabilityState~
        +ExplicitServerPermissions map~string,map~
        +AvailabilityGroupPermissions map~string,map~
        +Has(name) bool
        +Allows(name) bool
        +Permission(name) CapabilityState
        +InServerRole(name) bool
        +IsSysadmin() bool
        +Probed() bool
        +DeniedOnServerSecurable(kind, name, permission) bool
        +DeniedOnLogin(name, permission) bool
        +DeniedOnServerRole(name, permission) bool
        +DeniedOnEndpoint(name, permission) bool
        +AvailabilityGroupPermission(group, name) CapabilityState
        +HasOnAvailabilityGroup(group, name) bool
        +PermitsOnAvailabilityGroup(group, name) bool
        Has is the test for offering something,
        Allows the test for withholding it —
        deliberately not opposites, because
        withholding must fail open. Every
        method is nil-safe.
        ExplicitServerPermissions is a
        sys.server_permissions read of DENY rows
        only, keyed by ServerSecurableKey, so
        DeniedOn* is its one sound question.
    }

    class ServerSecurableKind {
        <<enumeration>>
        ServerSecurableLogin
        ServerSecurableServerRole
        ServerSecurableEndpoint
        +ServerSecurableKey(kind, name) string
    }

    class CapabilityState {
        <<enumeration>>
        CapabilityUnknown
        CapabilityGranted
        CapabilityDenied
        HAS_PERMS_BY_NAME returns NULL without
        raising for a permission the instance
        does not define, so Unknown is not a
        denial.
    }

    class LoginSource {
        <<enumeration>>
        LoginSourceAuto
        LoginSourceSQL
        LoginSourceWindows
        LoginSourceExternalProvider
        LoginSourceCertificate
        LoginSourceAsymmetricKey
    }

    %% =========================================================
    %% Scripting pending writes (dry-run)
    %% =========================================================
    class ScriptCollector {
        -mu sync.Mutex
        +Statements []string
        +WithScript(ctx) *ScriptCollector
        +Scripting(ctx) bool
        Captures the exact statement(s) a set of
        pending write calls would run, without
        running them. Every write funnels through
        Server.execContext or Database.exec, the
        two chokepoints WithScript intercepts.
        Bound parameters are substituted as
        literals so a captured statement runs
        standalone; ExecProc is captured as its
        EXEC form. Scripting(ctx) reports whether
        a context is one of these, so a caller
        does not mirror a write the server never
        saw into its own state.
        Statements is mutex-guarded: one collector
        may be shared across goroutines.
    }

    %% =========================================================
    %% Relationships
    %% =========================================================
    ConnectionOptions --> AuthMethod : uses
    ConnectionOptions --> KerberosOptions : configures AuthWindows via
    ConnectionOptions --> EntraCache : shares Entra sign-ins through
    ConnectionOptions ..> DeviceCodeMessage : DeviceCodePrompt receives
    Server --> ConnectionOptions : created from
    Server --> ServerInfo : has
    ServerInfo ..> EngineEdition : EngineEdition is one of
    Server "1" --> "*" Database : owns
    Server "1" --> "*" Login : owns
    Server "1" --> "*" ServerRole : owns
    Server "1" --> "*" RoleMember : ServerRoleMembers() returns
    Server "1" --> "*" LinkedServer : owns
    Server --> ServerSecurityInfo : has
    Server "1" --> "*" ServerPermissionEntry : grants
    Server "1" --> "*" Credential : owns
    Server --> CredentialSpec : CreateCredential() takes
    Server "1" --> "*" CryptographicProvider : lists
    Credential ..> CryptographicProvider : may be bound to
    Database "1" --> "*" DatabaseScopedCredential : owns
    Database --> DatabaseScopedCredentialSpec : CreateDatabaseScopedCredential() takes
    Server --> ServerMemoryStats : has
    Server "1" --> "*" Language : lists
    Server --> ProcessorInfo : has
    Server "1" --> "*" DiskVolumeInfo : lists
    Server --> Capabilities : Capabilities() returns
    Capabilities ..> ServerSecurableKind : ExplicitServerPermissions keyed by
    Capabilities --> CapabilityState : answers with
    DatabaseCapabilities --> CapabilityState : answers with

    Login ..> nStringLiteral : password quoted by
    Login --> LoginDetails : has
    Login "1" --> "*" LoginUserMapping : mapped via
    Login ..> LoginSource : created from

    Database ..> withConn : writes run via
    Database ..> dbRows : query() returns
    Database ..> DatabaseQueryRow : single-row reads via
    Database ..> withRetry : reads retried via
    Server ..> ServerQuery : reads run via
    Server ..> withRetry : reads retried via

    withRetry <.. withConn : acquire+USE retried by
    withRetry <.. dbRows : acquire+USE+query retried by
    withRetry <.. DatabaseQueryRow : whole scan retried by
    withRetry <.. ServerQuery : whole scan retried by
    withRetry <.. IsRetryable : same failure test as

    ServerScripter ..> Server : scripts logins and server roles of
    ScriptCollector ..> Server : captures writes from
    ScriptCollector ..> Database : captures writes from
```

### A database: files, options, catalog, and permissions

What `Database` exposes about itself — its files and filegroups, its
`ALTER DATABASE` options, change tracking, the bulk catalog snapshot, Query
Store, dependencies and search, and the whole permissions surface, including
column-level and effective permissions.

```mermaid
classDiagram
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
        +DisableGrowth bool
        +MaxSizeKB int64
    }

    class FileModify {
        +NewName string
        +SizeKB int64
        +GrowthKB int64
        +GrowthPercent int
        +DisableGrowth bool
        +MaxSizeKB int64
        DisableGrowth is FILEGROWTH = 0. Zero
        cannot say it: GrowthKB = 0 means
        "leave this property alone".
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
    %% Catalog snapshot (bulk table/view + column inventory)
    %% =========================================================
    class Catalog {
        +Schemas []string
        +Objects []CatalogObject
    }

    class CatalogObject {
        +ObjectID int
        +Schema string
        +Name string
        +Type CatalogObjectType
        +Columns []CatalogColumn
    }

    class CatalogColumn {
        +Name string
        +DataType DataType
        +MaxLength int
        +Precision int
        +Scale int
        +IsNullable bool
    }

    %% =========================================================
    %% Query Store and Database Scoped Configuration
    %% =========================================================
    class QueryStoreInfo {
        +DesiredState string
        +ActualState string
        +ReadOnlyReason int
        +CurrentStorageMB int64
        +MaxStorageMB int64
        +CaptureMode string
        +SizeCleanupMode string
        +StaleThresholdDays int
        +WaitStatsCaptureMode string
    }

    class QueryStoreReportOptions {
        +Metric QSMetric
        +Statistic QSStatistic
        +From time.Time
        +To time.Time
        +BaselineFrom time.Time
        +BaselineTo time.Time
        +Top int
        +MinExecCount int64
        +QueryIDs []int64
        +MinRegressionPct float64
        +IncludeInternal bool
    }

    class QSMetric {
        <<enumeration>>
        QSMetricDuration
        QSMetricCPUTime
        QSMetricLogicalReads
        QSMetricLogicalWrites
        QSMetricPhysicalReads
        QSMetricCLRTime
        QSMetricDOP
        QSMetricMemory
        QSMetricRowCount
        QSMetricLogMemory
        QSMetricTempDBMemory
    }

    class QSStatistic {
        <<enumeration>>
        QSStatAvg
        QSStatMin
        QSStatMax
        QSStatTotal
        QSStatStdDev
    }

    class QSUnit {
        <<enumeration>>
        QSUnitCount
        QSUnitMicroseconds
        QSUnitPages
        QSUnitBytes
        QSUnitMilliseconds
        Query Store stores raw engine units.
        QSMetricUnit(m) reports which.
    }

    class QSQueryStat {
        +QueryID int64
        +QueryText string
        +ObjectName string
        +ExecCount int64
        +PlanCount int
        +ForcedPlanID int64
        +LastExecutionTime time.Time
        +Value float64
        +BaselineValue float64
        +Regression float64
        +Variation float64
    }

    class QSPlan {
        +PlanID int64
        +QueryID int64
        +IsForced bool
        +ForceFailureCount int64
        +LastForceFailureReason string
        +QueryPlanXML string
        +ExecCount int64
        +Value float64
    }

    class QSIntervalStat {
        +StartTime time.Time
        +EndTime time.Time
        +ExecCount int64
        +Value float64
    }

    class QSPlanIntervalStat {
        +PlanID int64
        +StartTime time.Time
        +EndTime time.Time
        +ExecCount int64
        +Value float64
    }

    class QSWaitStat {
        +Category string
        +ExecCount int64
        +Value float64
    }

    %% =========================================================
    %% Detach / Attach
    %% =========================================================
    class DetachOptions {
        +DropConnections bool
        +UpdateStatistics bool
        +DropFullTextIndexFile bool
        Named for what they do, not for
        sp_detach_db's parameters, whose two
        flags are the inverse of the question
        a user is asked.
    }

    class AttachSpec {
        +Name string
        +Files []string
        +Owner string
        +RebuildLog bool
    }

    class DetachedDatabase {
        +Name string
        +Version string
        +Collation string
        +Files []*DetachedFile
        +DataFiles() []*DetachedFile
        +LogFiles() []*DetachedFile
    }

    class DetachedFile {
        +FileID int
        +Name string
        +PhysicalName string
        +IsLog bool
    }

    class DatabaseScopedConfig {
        +ID int
        +Name string
        +Value string
        +ValueForSecondary string
        +IsValueDefault bool
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
    %% Narrowing a listing at the server
    %% =========================================================
    class ObjectFilter {
        +Name []TextCriterion
        +Schema []TextCriterion
        +Created []DateCriterion
        +MemoryOptimized *bool
        +Empty() bool
        Criteria are AND-ed; a zero filter
        narrows nothing. Matching lowercases
        both sides, because a bare LIKE follows
        the database collation, and the pattern
        is escaped, because %, _ and [ are legal
        in an identifier.
    }

    class TextCriterion {
        +Op TextOp
        +Value string
    }

    class DateCriterion {
        +Op DateOp
        +Day time.Time
    }

    class TextOp {
        <<enumeration>>
        TextContains
        TextNotContains
        TextEquals
        TextNotEquals
    }

    class DateOp {
        <<enumeration>>
        DateOn
        DateBefore
        DateAfter
        All three compare whole calendar days.
    }

    %% =========================================================
    %% Procedure and function parameters
    %% =========================================================
    class Parameter {
        +Name string
        +Ordinal int
        +DataType DataType
        +MaxLength int
        +Precision int
        +Scale int
        +IsOutput bool
        +HasDefault bool
        +TypeString() string
        sys.parameters for one procedure or
        function. A scalar function's return
        value, stored there as parameter_id 0,
        is not a parameter and is not returned.
    }

    %% =========================================================
    %% What the connected login may do here
    %% =========================================================
    class DatabaseCapabilities {
        +Accessible bool
        +Roles map~string,bool~
        +Permissions map~string,CapabilityState~
        +ExplicitDatabasePermissions map~string,CapabilityState~
        +ExplicitPrincipalPermissions map~string,map~
        +Has(name) bool
        +Allows(name) bool
        +Permits(name) bool
        +DeniedOnDatabase(name) bool
        +DeniedOnPrincipal(principal, name) bool
        +Permission(name) CapabilityState
        +InRole(name) bool
        +Probed() bool
        +SchemaPermissions map~string,map~
        +ExplicitSchemaPermissions map~string,map~
        +ObjectPermissions map~string,map~
        +ColumnPermissions map~string,map~
        +SchemaPermission(schema, name) CapabilityState
        +HasOnSchema(schema, name) bool
        +AllowsOnSchema(schema, name) bool
        +PermitsOnSchema(schema, name) bool
        +DeniedOnSchema(schema, name) bool
        +ObjectPermission(schema, object, name) CapabilityState
        +HasOnObject(schema, object, name) bool
        +DeniedOnObject(schema, object, name) bool
        +ColumnPermission(schema, object, column, name) CapabilityState
        +HasOnColumn(schema, object, column, name) bool
        +DeniedOnColumn(schema, object, column, name) bool
        +DeniedOnAnyColumn(schema, object, name) bool
        Accessible is HAS_DBACCESS: check it
        before expanding a database at all.
        Permits is Allows plus that, since an
        inaccessible database answers Unknown
        to every permission.
        Schema scope is probed with
        HAS_PERMS_BY_NAME and answers three ways.
        Object and column scope are read from
        sys.database_permissions and record only
        explicit grants and denials, so HasOn*
        adds permission and only DeniedOn*
        withholds it.
        DeniedOnDatabase and DeniedOnPrincipal
        read recorded DENY rows, which a
        HAS_PERMS_BY_NAME grant cannot reveal.
    }

    class DatabaseRecoveryStatus {
        +DatabaseName string
        +LastLogBackupLSN string
        +LogBackupChainStarted bool
        Whether the log backup chain has been
        started at all — what SQL Server tests
        before a database may join an
        availability group.
    }

    %% =========================================================
    %% Execution plans
    %% =========================================================
    class ExecutionPlan {
        +XML string
        +All []string
        A batch of several statements yields
        several plan documents. XML is the last
        of them; All holds every one.
    }

    %% =========================================================
    %% Object and database-scoped permissions
    %% =========================================================
    class PermissionEntry {
        +Principal string
        +PrincipalType string
        +Grantor string
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

    class PrincipalSecurable {
        +SecurableType string
        +Schema string
        +Name string
        +Permission string
        +State string
    }

    class ColumnPermissionEntry {
        +Principal string
        +PrincipalType string
        +Grantor string
        +Schema string
        +Object string
        +ObjectType string
        +Column string
        +Permission ObjectPermission
        +State PermissionState
    }

    class EffectivePermission {
        +Entity string
        +Subentity string
        +Permission string
    }

    class PermissionOptions {
        +WithGrantOption bool
        +Cascade bool
        +GrantOptionOnly bool
    }

    class SecurableSearch {
        +Name string
        +Limit int
    }

    class SecurableRef {
        +Type string
        +Schema string
        +Name string
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
    %% Relationships
    %% =========================================================
    Database "1" --> "*" DatabaseFileInfo : contains
    Database "1" --> "*" FileGroup : contains
    Database --> DatabaseOptions : has
    Database --> ChangeTrackingInfo : has
    Database "1" --> "*" TableChangeTracking : tracks
    Database --> Catalog : Catalog()/SystemCatalog() returns
    Catalog "1" --> "*" CatalogObject : contains
    CatalogObject "1" --> "*" CatalogColumn : has
    Database --> QueryStoreInfo : has
    Database --> QSQueryStat : reports
    Database --> QSPlan : reports
    Database --> QSIntervalStat : reports
    Database --> QSPlanIntervalStat : reports
    Database --> QSWaitStat : reports
    QueryStoreReportOptions --> QSMetric : ranks by
    QueryStoreReportOptions --> QSStatistic : aggregated with
    QSMetric ..> QSUnit : measured in
    Server --> DetachOptions : DetachDatabase() takes
    Server --> AttachSpec : AttachDatabase() takes
    Server --> DetachedDatabase : DetachedDatabaseInfo() returns
    DetachedDatabase "1" --> "*" DetachedFile : contains
    Database "1" --> "*" DatabaseScopedConfig : lists
    Database "1" --> "*" Dependency : dependencies of
    Database "1" --> "*" SearchResult : search() returns
    Database "1" --> "*" Parameter : Parameters() returns
    Database --> DatabaseRecoveryStatus : RecoveryStatus() returns
    Database --> DatabaseCapabilities : Capabilities() returns
    Database ..> ObjectFilter : ...Filtered() listings narrowed by
    ObjectFilter --> TextCriterion : matches names and schemas with
    ObjectFilter --> DateCriterion : matches creation dates with
    TextCriterion --> TextOp : uses
    DateCriterion --> DateOp : uses
    Database --> ExecutionPlan : produces
    Database "1" --> "*" PermissionEntry : grants
    Database "1" --> "*" DatabasePermissionEntry : grants
    Database "1" --> "*" PrincipalSecurable : PermissionsForPrincipal() returns
    Database "1" --> "*" ColumnPermissionEntry : ColumnPermissions() returns
    Database "1" --> "*" EffectivePermission : EffectivePermissions() returns
    Database "1" --> "*" SecurableRef : FindSecurables() returns
    SecurableSearch ..> SecurableRef : narrows FindSecurables()
    Database ..> BulkCopy : bulk-loads via
    Database ..> ProcParam : executes procs with
    Database --> ProcResult : returns
```

### Tables, their children, and the object families

`Table` and everything hanging off it (columns, indexes, foreign keys,
constraints, statistics, partitions), the other object families a database
contains, and the `Scripter` that generates CREATE DDL for any of them.

```mermaid
classDiagram
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
        +IndexByName(name) *Index
        +XMLIndexes() []*XMLIndex
        +ForeignKeys() []*ForeignKey
        +ForeignKeyByName(name) *ForeignKey
        +CheckConstraints() []*CheckConstraint
        +Statistics() []*Statistic
        +StatisticByName(name) *Statistic
        +Partitions() []*Partition
        +Triggers() []*Trigger
        +DataSpace() DataSpace
        +RowCount() int64
        +CountWhere(predicate) int64
        +CheckWhereSyntax(predicate) error
        +Detail() *TableDetail
        +SpaceUsed() *TableSpaceInfo
        +TruncateTable() error
        +FragmentationStats(mode) []*IndexFragmentation
        +RebuildAllIndexes(fillFactor) error
        +UpdateAllStatistics(samplePct) error
        +CreateIndex(req) error
        +CreateStatistic(name, cols, pct) error
        +CreateStatisticWithOptions(req) error
        +AlterColumn(col) error
        +DropColumn(name) error
        +RenameColumn(name, newName) error
        +DropConstraint(name) error
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
        +IsPadded bool
        +IgnoreDupKey bool
        +AllowRowLocks bool
        +AllowPageLocks bool
        +DataCompression string
        +KeyColumns []IndexColumn
        +IncludedColumns []IndexColumn
        +FilterDefinition string
        +DataSpace DataSpace
        +Rebuild(t, fillFactor) error
        +RebuildWithOptions(t, fillFactor, padIndex, compression) error
        +Reorganize(t) error
        +Disable(t) error
        +Enable(t) error
        +Rename(t, newName) error
        +SetOptions(t, ignoreDupKey, rowLocks, pageLocks) error
        +SetLockOptions(t, rowLocks, pageLocks) error
        +SetIncludedColumns(t, columns) error
        +UpdateStatistics(t) error
        +StorageInfo(t) *IndexStorageInfo
        +Fragmentation(t, mode) *IndexFragmentation
        +Drop(t) error
        +Type.IsColumnStore() bool
        Type is a sys.indexes type_desc value; an
        unrecognized one is carried through as the
        server's own text. IsColumnStore covers
        both columnstore forms, neither of which
        SetIncludedColumns supports.
    }

    class IndexStorageInfo {
        +FileGroup string
        +PartitionScheme string
        +PartitionColumn string
        +RowCount int64
        +UsedKB int64
        +ReservedKB int64
        +AvgRecordSize float64
        +Allocations []IndexAllocationUnit
    }

    class IndexAllocationUnit {
        +Type string
        +Pages int64
        +UsedKB int64
    }

    class IndexFragmentation {
        +IndexName string
        +IndexID int
        +AvgFragmentationPct float64
        +PageCount int64
        +FragmentCount int64
        +AvgPageSpaceUsedPct float64
    }

    class CreateIndexRequest {
        +Name string
        +Type IndexType
        +IsUnique bool
        +KeyColumns []IndexColumnDef
        +IncludedColumns []string
        +FilterDefinition string
        +FillFactor int
        +PadIndex bool
        +Online bool
        +SortInTempDB bool
        +DropExisting bool
        +DataCompression string
        +CompressionDelay int
        +FileGroup string
        +PartitionScheme string
        +PartitionColumns []string
        +IsPrimaryXML bool
        +PrimaryXMLIndex string
        +SecondaryXMLType XMLSecondaryIndexType
        +Tessellation SpatialTessellation
        +BoundingBox *SpatialBoundingBox
        +GridLevels SpatialGridLevels
        +CellsPerObject int
        Which fields apply depends on Type.
        A combination the server would reject
        is refused before anything runs, with
        an error naming the field.
    }

    class XMLIndex {
        +Name string
        +IndexID int
        +IsPrimary bool
        +SecondaryType XMLSecondaryIndexType
        +ColumnName string
        +PrimaryIndexName string
        What sys.indexes cannot say: primary or
        secondary, which form, and over which
        primary index it is built.
    }

    class XMLSecondaryIndexType {
        <<enumeration>>
        XMLSecondaryPath
        XMLSecondaryValue
        XMLSecondaryProperty
    }

    class SpatialTessellation {
        <<enumeration>>
        SpatialGeometryGrid
        SpatialGeometryAutoGrid
        SpatialGeographyGrid
        SpatialGeographyAutoGrid
        +IsGeometry() bool
        +IsAutoGrid() bool
    }

    class SpatialBoundingBox {
        +XMin float64
        +YMin float64
        +XMax float64
        +YMax float64
        Belongs around the data, not around the
        coordinate system: anything outside it
        lands in the single top-level cell.
    }

    class SpatialGridLevels {
        +Level1 SpatialGridDensity
        +Level2 SpatialGridDensity
        +Level3 SpatialGridDensity
        +Level4 SpatialGridDensity
    }

    class SpatialGridDensity {
        <<enumeration>>
        SpatialGridLow
        SpatialGridMedium
        SpatialGridHigh
    }

    class DataSpace {
        +Name string
        +IsPartitionScheme bool
        +IsDefaultFileGroup bool
        +PartitionColumn string
        Where a table or index keeps its rows —
        CREATE TABLE / CREATE INDEX's ON clause.
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
        +UnfilteredRows int64
        +NoRecompute bool
        +IsIncremental bool
        +ModificationCounter int64
        +Columns() []string
        +Header() *StatisticHeader
        +DensityVector() []*StatisticDensity
        +Histogram() []*StatisticHistogramStep
        +Update(samplePct) error
        +Drop() error
        +Rename(newName) error
    }

    class CreateStatisticRequest {
        +Name string
        +Columns []string
        +SamplePercent int
        +FullScan bool
        +FilterDefinition string
        +NoRecompute bool
        +Incremental bool
    }

    class StatisticHeader {
        +Updated string
        +Rows int64
        +RowsSampled int64
        +Steps int
        +Density float64
        +AverageKeyLength float64
        +StringIndex string
        +FilterExpression string
        +UnfilteredRows int64
        +PersistedSamplePercent float64
    }

    class StatisticDensity {
        +AllDensity float64
        +AverageLength float64
        +Columns string
    }

    class StatisticHistogramStep {
        +RangeHighKey string
        +RangeRows float64
        +EqRows float64
        +DistinctRangeRows int64
        +AvgRangeRows float64
    }

    class TableDetail {
        +SchemaOwner string
        +LockEscalation string
        +UsesAnsiNulls bool
        +IsReplicated bool
        +IsTrackedByCDC bool
        +TemporalType string
        +Durability string
        +LedgerType string
        +PrimaryKeyName string
        +DataSpace string
    }

    class TableSpaceInfo {
        +ReservedKB int64
        +DataKB int64
        +IndexKB int64
        +LOBKB int64
        +UnusedKB int64
        +FileGroup string
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
        +ScriptTrigger(schema, name) string
        +ScriptIndex(schema, table, name) string
        +ScriptCheckConstraint(schema, table, name) string
        +ScriptForeignKey(schema, table, name) string
        +ScriptSequence(schema, name) string
        +ScriptSynonym(schema, name) string
        +ScriptSchema(name) string
        +ScriptUser(name) string
        +ScriptDatabaseRole(name) string
        +ScriptDatabaseAuditSpecification(name) string
        +ScriptDatabaseScopedCredential(name) string
        +ScriptDatabaseTrigger(name) string
        +ScriptPartitionFunction(name) string
        +ScriptPartitionScheme(name) string
        +ScriptSecurityPolicy(schema, name) string
        +ScriptColumnMasterKey(name) string
        +ScriptColumnEncryptionKey(name) string
        +ScriptDatabase() string
        +ScriptSelect(schema, name) string
        +ScriptInsert(schema, name) string
        +ScriptUpdate(schema, name) string
        +ScriptDelete(schema, name) string
        +ScriptExecute(schema, name) string
        +ScriptFunctionCall(schema, name, funcType) string
    }

    class ServerScripter {
        -server *Server
        -opts ScriptOptions
        +NewServerScripter(server, opts) *ServerScripter
        +ScriptLogin(name) string
        +ScriptServerRole(name) string
        +ScriptEndpoint(name) string
        +ScriptCredential(name) string
        +ScriptBackupDevice(name) string
        +ScriptServerAudit(name) string
        +ScriptServerAuditSpecification(name) string
        +ScriptServerTrigger(name) string
        A scripted credential carries an
        "insert secret here" placeholder: the
        secret is not readable from the catalog.
    }

    class ScriptVerb {
        <<enumeration>>
        ScriptCreate
        ScriptDrop
        ScriptDropAndCreate
        ScriptAlter
        ScriptAlter applies to module objects —
        view, procedure, function, trigger —
        and everything else falls back to CREATE.
    }

    class ScriptOptions {
        +Verb ScriptVerb
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
        +ObjectCount() int
        +ObjectCountsByType() SchemaObjectCounts
        +ChangeOwner(newOwner) error
        +Drop() error
    }

    class SchemaObjectCounts {
        +Tables int
        +Views int
        +StoredProcedures int
        +Functions int
        +Synonyms int
        +Sequences int
        Each count reproduces the predicate of
        the listing it stands in for, which is
        why it does not add up to ObjectCount's
        single COUNT over sys.objects.
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
        +SID []byte
        +LoginName string
        +LoginDisabled bool
        +Rename(newName) error
        +SetDefaultSchema(schemaName) error
        +SetLogin(loginName) error
    }

    class DatabaseRole {
        +Name string
        +ID int
        +IsFixedRole bool
        +Owner string
        +Members []string
        +SID []byte
        +CreateDate time.Time
        +ModifyDate time.Time
        +Rename(newName) error
        +Drop() error
        +ChangeOwner(newOwner) error
    }

    class RoleMember {
        +Name string
        +Type string
    }

    class FileGroup {
        +Name string
        +Type string
        +IsDefault bool
        +IsReadOnly bool
        +Files []DatabaseFile
        +IsFileStream() bool
        Type is sys.filegroups.type_desc —
        ROWS_FILEGROUP, FILESTREAM_DATA_FILEGROUP or
        MEMORY_OPTIMIZED_DATA_FILEGROUP. ADD FILE has
        no file-type keyword, so the group decides what
        a file added to it becomes. IsDefault is per
        type, not per database.
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
        +ID int
        +Name string
        +IsFixedRole bool
        +Owner string
        +SID []byte
        +CreateDate time.Time
        +ModifyDate time.Time
        +Members []string
        +Rename(newName) error
        +Drop() error
        +ChangeOwner(newOwner) error
    }

    class LinkedServer {
        +Name string
        +Product string
        +Provider string
        +DataSource string
        +IsRemote bool
    }

    %% =========================================================
    %% Relationships
    %% =========================================================
    Database "1" --> "*" Table : contains
    Database "1" --> "*" View : contains
    Database "1" --> "*" StoredProcedure : contains
    Database "1" --> "*" UserDefinedFunction : contains
    Database "1" --> "*" Schema : contains
    Database "1" --> "*" User : contains
    Database "1" --> "*" DatabaseRole : contains
    Database "1" --> "*" Trigger : contains
    Database "1" --> "*" RoleMember : RoleMembers() returns
    Database "1" --> "*" Column : ObjectColumns() returns (table or view)

    Table "1" --> "*" Column : has
    Table "1" --> "*" Index : has
    Table "1" --> "*" ForeignKey : has
    Table "1" --> "*" CheckConstraint : has
    Table "1" --> "*" Statistic : has
    Table "1" --> "*" Trigger : has
    Table --> TableDetail : has
    Table --> TableSpaceInfo : has
    Table "1" --> "*" IndexFragmentation : FragmentationStats() returns
    Table "1" --> "*" XMLIndex : XMLIndexes() returns
    Table --> DataSpace : DataSpace() returns
    Table ..> CreateIndexRequest : CreateIndex() takes
    Table ..> CreateStatisticRequest : CreateStatisticWithOptions() takes

    Index --> IndexStorageInfo : StorageInfo() returns
    IndexStorageInfo "1" --> "*" IndexAllocationUnit : breaks down into
    Index --> IndexFragmentation : Fragmentation() returns
    Index --> DataSpace : read with the index

    CreateIndexRequest --> XMLSecondaryIndexType : XML form selected by
    CreateIndexRequest --> SpatialTessellation : spatial scheme selected by
    CreateIndexRequest --> SpatialBoundingBox : geometry schemes bounded by
    CreateIndexRequest --> SpatialGridLevels : grid density set by
    SpatialGridLevels --> SpatialGridDensity : per level
    XMLIndex --> XMLSecondaryIndexType : secondary form

    Schema --> SchemaObjectCounts : ObjectCountsByType() returns

    Statistic --> StatisticHeader : Header() returns
    Statistic "1" --> "*" StatisticDensity : DensityVector() returns
    Statistic "1" --> "*" StatisticHistogramStep : Histogram() returns

    Scripter --> Database : scripts objects from
    Scripter --> ScriptOptions : configured by
    ServerScripter --> ScriptOptions : configured by
    ScriptOptions --> ScriptVerb : statement form selected by
```

### Backup, restore, and SQL Server Agent

Backup and restore options and the metadata read back off a device, and
the whole Agent node — jobs and their steps and history, alerts, operators,
shared schedules, and categories.

```mermaid
classDiagram
    %% =========================================================
    %% Backup / Restore
    %% =========================================================
    class BackupOptions {
        +Database string
        +Devices []string
        +Action BackupAction
        +Files []string
        +FileGroups []string
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
        +Credential string
        +Progress func
        +BuildBackupStatement(opts) string
    }

    class RestoreOptions {
        +Database string
        +Devices []string
        +Action BackupAction
        +Files []string
        +FileGroups []string
        +RelocateFiles []RelocateFile
        +Recovery bool
        +Replace bool
        +Checksum bool
        +Stats int
        +StopAt time.Time
        +StopAtMarkName string
        +FileNumber int
        +Credential string
        +Progress func
        +BuildRestoreStatement(opts) string
    }

    class BackupHeader {
        +BackupName string
        +Description string
        +BackupType BackupAction
        +Position int
        +DatabaseName string
        +BackupStart time.Time
        +BackupFinish time.Time
        +BackupSize int64
        +Compressed bool
        +HasChecksums bool
        +IsCopyOnly bool
        +RecoveryModel string
    }

    class BackupFile {
        +LogicalName string
        +PhysicalName string
        +Type string
        +FileGroupName string
        +Size int64
        +MaxSize int64
    }

    class BackupTarget {
        -name string
        -logical bool
        -url bool
        +DiskTarget(path) BackupTarget
        +URLTarget(url) BackupTarget
        +DeviceTarget(name) BackupTarget
        +IsBackupURL(device) bool
        +String() string
        A logical device is named bare, a path
        as DISK = N'...'. Passing a device name
        as a path reads a file of that name in
        the default backup directory instead.
        An http/https device is URL = N'...' —
        Managed Instance refuses DISK outright.
    }

    class BackupDevice {
        -server *Server
        +Name string
        +Type string
        +PhysicalName string
        +Drop(deleteFile) error
        +Headers() []*BackupHeader
        +Target() BackupTarget
        No Alter: sp_addumpdevice and
        sp_dropdevice are the whole write
        surface, so name, type and path are
        fixed at creation.
    }

    class BackupDeviceType {
        <<enumeration>>
        BackupDeviceDisk
        BackupDeviceTape
    }

    %% =========================================================
    %% SQL Server Agent
    %% =========================================================
    class AgentStatus {
        +Running bool
        +StatusText string
        +LastStartupTime time.Time
        Read from sys.dm_server_services, which
        returns no rows on an Azure edition —
        there Agent's own sessions and
        msdb.dbo.syssessions stand in.
    }

    class Job {
        +JobID string
        +Name string
        +Description string
        +IsEnabled bool
        +Category string
        +OwnerLoginName string
        +DateCreated time.Time
        +DateModified time.Time
        +StartStepID int
        +DeleteLevel NotifyLevel
        +NotifyLevelEmail NotifyLevel
        +NotifyEmailOperatorName string
        +LastRunDate time.Time
        +LastRunOutcome JobOutcome
        +LastRunDuration Duration
        +NextRunDate time.Time
        +CurrentState JobState
        +Steps() []*JobStep
        +AddStep(req) error
        +InsertStep(req, stepID) error
        +MoveStep(stepID, newStepID) error
        +ReorderSteps(order) error
        +Schedules() []*Schedule
        +AddSchedule(req) error
        +AttachSchedule(name) error
        +DetachSchedule(name) error
        +History(limit) []*JobHistoryEntry
        +Start(stepName) error
        +Stop() error
        +Enable() error
        +Disable() error
        +Rename(newName) error
        +SetDescription(desc) error
        +SetCategory(category) error
        +SetOwner(login) error
        +SetStartStep(stepID) error
        +SetDeleteLevel(level) error
        +SetEmailNotify(operator, level) error
        +Drop() error
    }

    class JobStep {
        +StepID int
        +Name string
        +Subsystem string
        +Command string
        +Database string
        +OnSuccessAction int
        +OnSuccessStepID int
        +OnFailAction int
        +OnFailStepID int
        +LastRunOutcome JobOutcome
        +LastRunDate time.Time
        +LastRunDuration int
        +LastRunElapsed Duration
        +RetryAttempts int
        +RetryInterval int
        +OutputFileName string
        +Flags int
        +ProxyName string
        +AdditionalParameters string
        +Server string
        +DatabaseUserName string
        +CmdExecSuccessCode int
        +OSRunPriority int
        +Update(req) error
        +SetFlow(onSuccess, successStep, onFail, failStep) error
        +Delete() error
        The whole definition is carried because
        a move is a delete and a re-insert —
        msdb has no procedure that renumbers a
        step in place.
    }

    class JobHistoryEntry {
        +JobName string
        +RunDate time.Time
        +Duration Duration
        +Outcome JobOutcome
        +Message string
        +StepID int
        +StepName string
    }

    class Alert {
        +ID int
        +Name string
        +Enabled bool
        +EventSource string
        +ErrorNumber int
        +Severity int
        +DatabaseName string
        +DelayBetweenResponses Duration
        +NotificationMessage string
        +IncludeEventDescriptionIn int
        +Category string
        +JobName string
        +PerformanceCondition string
        +OccurrenceCount int
        +LastOccurrence time.Time
        +LastResponse time.Time
        +IsEventAlert() bool
        +Enable() error
        +Disable() error
        +Rename(newName) error
        +SetTrigger(errorNumber, severity) error
        +SetDatabase(dbName) error
        +SetDelay(d) error
        +SetNotificationMessage(msg) error
        +SetJobResponse(jobName) error
        +SetCategory(category) error
        +Notifications() []*AlertNotification
        +Notify(operator, method) error
        +RemoveNotify(operator) error
        +Drop() error
    }

    class AlertNotification {
        +OperatorName string
        +Method NotificationMethod
    }

    class Operator {
        +ID int
        +Name string
        +Enabled bool
        +EmailAddress string
        +PagerAddress string
        +NetSendAddress string
        +Category string
        +LastEmailDate time.Time
        +LastPagerDate time.Time
        +LastNetSendDate time.Time
        +Enable() error
        +Disable() error
        +Rename(newName) error
        +SetEmailAddress(addr) error
        +SetCategory(category) error
        +NotifyingAlerts() []*AlertNotificationRef
        +NotifyingJobs() []*JobNotificationRef
        +Drop() error
    }

    class AlertNotificationRef {
        +AlertName string
        +Method NotificationMethod
    }

    class JobNotificationRef {
        +JobName string
        +Level NotifyLevel
    }

    class Schedule {
        +ID int
        +Name string
        +Enabled bool
        +FreqType ScheduleFreqType
        +FreqInterval int
        +FreqSubdayType ScheduleSubdayType
        +FreqSubdayInterval int
        +FreqRelativeInterval int
        +FreqRecurrenceFactor int
        +ActiveStartDate time.Time
        +ActiveEndDate time.Time
        +ActiveStartTime int
        +ActiveEndTime int
        +OwnerLoginName string
        +CreateDate time.Time
        +ModifyDate time.Time
        +Description() string
        +Enable() error
        +Disable() error
        +Rename(newName) error
        +SetOwner(login) error
        +SetFrequency(f) error
        +SetActiveRange(startDate, endDate, startTime, endTime) error
        +Jobs() []*Job
        +Drop() error
    }

    class ScheduleFrequency {
        +FreqType ScheduleFreqType
        +FreqInterval int
        +FreqSubdayType ScheduleSubdayType
        +FreqSubdayInterval int
        +FreqRelativeInterval int
        +FreqRecurrenceFactor int
    }

    class Category {
        +ID int
        +Class CategoryClass
        +Name string
    }

    class JobState {
        <<enumeration>>
        JobStateUnknown
        JobStateExecuting
        JobStateWaitingForWorker
        JobStateBetweenRetries
        JobStateIdle
        JobStateSuspended
        JobStateWaitingForStepToFinish
        JobStatePerformingCompletionActions
        Agent's own job_state encoding, read
        from xp_sqlagent_enum_jobs. With Agent
        stopped only Executing and Idle can be
        told apart.
    }

    class ScheduleFreqType {
        <<enumeration>>
        FreqOnce
        FreqDaily
        FreqWeekly
        FreqMonthly
        FreqMonthlyRelative
        FreqAutoStart
        FreqOnIdle
    }

    class ScheduleSubdayType {
        <<enumeration>>
        SubdayOnce
        SubdaySeconds
        SubdayMinutes
        SubdayHours
    }

    class NotificationMethod {
        <<enumeration>>
        NotifyMethodEmail
        NotifyMethodPager
        NotifyMethodNetSend
        +String() string
    }

    class NotifyLevel {
        <<enumeration>>
        NotifyNever
        NotifyOnSuccess
        NotifyOnFailure
        NotifyOnComplete
    }

    class CategoryClass {
        <<enumeration>>
        CategoryClassJob
        CategoryClassAlert
        CategoryClassOperator
    }

    %% =========================================================
    %% Relationships
    %% =========================================================
    Server --> BackupOptions : accepts
    Server --> RestoreOptions : accepts
    Server "1" --> "*" BackupHeader : BackupHeaders() returns
    Server "1" --> "*" BackupFile : BackupFileList() returns
    Server "1" --> "*" BackupDevice : owns
    Server --> BackupTarget : the ...From() reads take
    BackupDevice --> BackupTarget : Target() returns
    BackupDevice --> BackupDeviceType : created as

    Server --> AgentStatus : AgentInfo() returns
    Server "1" --> "*" Job : owns
    Server "1" --> "*" Alert : owns
    Server "1" --> "*" Operator : owns
    Server "1" --> "*" Schedule : owns
    Server "1" --> "*" Category : owns
    Server "1" --> "*" JobHistoryEntry : JobHistory() returns

    Job "1" --> "*" JobStep : has
    Job "1" --> "*" Schedule : attached to
    Job "1" --> "*" JobHistoryEntry : History() returns
    Job --> JobState : CurrentState is
    Job --> NotifyLevel : notified per
    Schedule --> ScheduleFrequency : SetFrequency() accepts
    Schedule --> ScheduleFreqType : recurs per
    Schedule --> ScheduleSubdayType : repeats per
    Alert "1" --> "*" AlertNotification : notifies via
    Alert --> Job : responds by running
    Operator "1" --> "*" AlertNotificationRef : notified by
    Operator "1" --> "*" JobNotificationRef : emailed by
    AlertNotification --> NotificationMethod : delivered by
    Category --> CategoryClass : classified by
```

### High availability and instance-level services

Always On availability groups with their replicas, databases and listeners;
the database mirroring endpoint they ship log through; the certificates that
authenticate it; and the two host-facing reads — the error log and the
server's own filesystem.

```mermaid
classDiagram
    %% =========================================================
    %% Always On availability groups
    %% =========================================================
    class AvailabilityGroup {
        -server *Server
        +ID string
        +Name string
        +ResourceID string
        +ResourceGroupID string
        +ClusterType string
        +AutomatedBackupPreference string
        +FailureConditionLevel int
        +HealthCheckTimeout int
        +Version int
        +BasicFeatures bool
        +DTCSupport bool
        +DBFailover bool
        +IsDistributed bool
        +IsContained bool
        +RequiredSynchronizedSecondariesToCommit int
        +PrimaryReplicaServerName string
        +PrimaryRecoveryHealth string
        +SynchronizationHealth string
        +Server() *Server
        +IsLocalPrimary() bool
        +Replicas() []*AvailabilityReplica
        +Databases() []*AvailabilityDatabase
        +Listeners() []*AvailabilityGroupListener
        +SetAutomatedBackupPreference(pref) error
        +SetFailureConditionLevel(level) error
        +SetHealthCheckTimeout(ms) error
        +SetDBFailover(on) error
        +SetDTCSupport(perDB) error
        +SetRequiredSynchronizedSecondariesToCommit(n) error
        +AddDatabase(name) error
        +RemoveDatabase(name) error
        +JoinDatabase(name) error
        +UnjoinDatabase(name) error
        +SuspendDatabase(name) error
        +ResumeDatabase(name) error
        +AddReplica(spec) error
        +RemoveReplica(serverName) error
        +AddListener(spec) error
        +AddListenerIP(dnsName, ip) error
        +SetListenerPort(dnsName, port) error
        +RemoveListener(dnsName) error
        +Join(clusterType) error
        +GrantCreateAnyDatabase() error
        +DenyCreateAnyDatabase() error
        +Failover() error
        +ForceFailoverAllowDataLoss() error
        +Drop() error
    }

    class AvailabilityReplica {
        +GroupID string
        +GroupName string
        +ReplicaID string
        +ReplicaServerName string
        +EndpointURL string
        +AvailabilityMode string
        +FailoverMode string
        +SeedingMode string
        +SessionTimeout int
        +PrimaryRoleAllowConnections string
        +SecondaryRoleAllowConnections string
        +BackupPriority int
        +ReadOnlyRoutingURL string
        +IsLocal bool
        +Role string
        +OperationalState string
        +ConnectedState string
        +RecoveryHealth string
        +SynchronizationHealth string
        +LastConnectErrorNumber int
        +LastConnectErrorDescription string
        +LastConnectErrorTimestamp time.Time
        +CreateDate time.Time
        +ModifyDate time.Time
        +ReadOnlyRoutingList() [][]string
        +SetAvailabilityMode(mode) error
        +SetFailoverMode(mode) error
        +SetSeedingMode(mode) error
        +SetPrimaryRoleAllowConnections(mode) error
        +SetSecondaryRoleAllowConnections(mode) error
        +SetSessionTimeout(seconds) error
        +SetBackupPriority(priority) error
        +SetReadOnlyRoutingURL(url) error
        +SetReadOnlyRoutingList(list) error
        +Drop() error
    }

    class AvailabilityDatabase {
        +GroupID string
        +ReplicaID string
        +ReplicaServerName string
        +DatabaseName string
        +GroupDatabaseID string
        +IsLocal bool
        +IsPrimaryReplica bool
        +SynchronizationState string
        +SynchronizationHealth string
        +DatabaseState string
        +IsSuspended bool
        +SuspendReason string
        +LogSendQueueKB int64
        +LogSendRateKBps int64
        +RedoQueueKB int64
        +RedoRateKBps int64
        +SecondaryLagSeconds int64
        +LastSentTime time.Time
        +LastReceivedTime time.Time
        +LastHardenedTime time.Time
        +LastRedoneTime time.Time
        +LastCommitTime time.Time
    }

    class AvailabilityGroupListener {
        +GroupID string
        +ListenerID string
        +DNSName string
        +Port int
        +IsConformant bool
        +IPConfigurationString string
        +IsDistributedNetworkName bool
        +IPAddresses []AvailabilityListenerIP
    }

    class AvailabilityListenerIP {
        +IPAddress string
        +SubnetMask string
        +IsDHCP bool
        +State string
    }

    class CreateAvailabilityGroupRequest {
        +Name string
        +ClusterType string
        +AutomatedBackupPreference string
        +FailureConditionLevel int
        +HealthCheckTimeout int
        +DBFailover bool
        +DTCSupport bool
        +RequiredSynchronizedSecondariesToCommit int
        +Databases []string
        +Replicas []AvailabilityReplicaSpec
        +Listener *AvailabilityListenerSpec
    }

    class AvailabilityReplicaSpec {
        +ServerName string
        +EndpointURL string
        +AvailabilityMode string
        +FailoverMode string
        +SeedingMode string
        +SessionTimeout int
        +BackupPriority int
        +PrimaryRoleAllowConnections string
        +SecondaryRoleAllowConnections string
        +ReadOnlyRoutingURL string
    }

    class AvailabilityListenerSpec {
        +DNSName string
        +Port int
        +DHCP bool
        +DHCPSubnet string
        +IPAddresses []AvailabilityListenerIPSpec
    }

    class AvailabilityListenerIPSpec {
        +IPAddress string
        +SubnetMask string
    }

    %% =========================================================
    %% Database mirroring endpoint (Always On's transport)
    %% =========================================================
    class DatabaseMirroringEndpoint {
        -server *Server
        +Name string
        +Port int
        +State string
        +Role string
        +IsEncryptionEnabled bool
        +EncryptionAlgorithm string
        +ConnectionAuth string
        +Owner string
        +Server() *Server
        +URL() string
        +Start() error
        +Stop() error
        +Drop() error
        +GrantConnect(login) error
    }

    class EndpointSpec {
        +Name string
        +Port int
        +Role string
        +Authentication string
        +Encryption string
        +EncryptionAlgorithm string
    }

    class Endpoint {
        -server *Server
        +EndpointID int
        +Name string
        +Owner string
        +Protocol string
        +Type string
        +State string
        +IsAdmin bool
        +Port int
        +IsSystem bool
        +SetState(state) error
        +Drop() error
        +MirroringDetail() *DatabaseMirroringEndpoint
        +ServiceBrokerDetail() *ServiceBrokerEndpointDetail
        Type-specific detail is read on demand,
        so listing every endpoint costs one
        query. The five built-in endpoints
        refuse writes with ErrSystemEndpoint.
    }

    class EndpointState {
        <<enumeration>>
        EndpointStarted
        EndpointStopped
        EndpointDisabled
    }

    class ServiceBrokerEndpointDetail {
        +IsMessageForwardingEnabled bool
        +MessageForwardingSize int
        +ConnectionAuth string
        +EncryptionAlgorithm string
        +CertificateName string
    }

    %% =========================================================
    %% Server audits, audit specifications, server triggers
    %% =========================================================
    class ServerAudit {
        -server *Server
        +AuditID int
        +Name string
        +GUID string
        +Type string
        +OnFailure string
        +QueueDelay int
        +Predicate string
        +IsEnabled bool
        +LogFilePath string
        +LogFileName string
        +MaxFileSize int64
        +MaxRolloverFiles int
        +MaxFiles int
        +ReserveDiskSpace bool
        +Alter(spec) error
        +Rename(newName) error
        +SetState(on) error
        +Drop() error
        +Status() *ServerAuditStatus
        +WithDisabled(ctx, fn) error
        Every write but SetState needs the audit
        disabled; each one disables, applies and
        re-enables, and only if it disabled.
        MaxFileSize uses 0 for UNLIMITED,
        MaxRolloverFiles uses AuditUnlimited.
    }

    class ServerAuditSpec {
        +Name string
        +Type string
        +QueueDelay int
        +OnFailure string
        +Predicate string
        +FilePath string
        +MaxFileSize int64
        +MaxRolloverFiles int
        +MaxFiles int
        +ReserveDiskSpace bool
    }

    class ServerAuditStatus {
        +Status string
        +StatusTime time.Time
        +AuditFilePath string
        +AuditFileSize int64
    }

    class ServerAuditSpecification {
        -server *Server
        +SpecificationID int
        +Name string
        +AuditGUID string
        +AuditName string
        +IsEnabled bool
        +ActionGroups []string
        +AddActionGroups(groups) error
        +DropActionGroups(groups) error
        +SetAudit(auditName) error
        +SetState(on) error
        +Drop() error
        +WithDisabled(ctx, fn) error
        AuditName is empty for an orphaned
        specification: dropping the audit it
        references succeeds and leaves the
        audit_guid pointing at nothing.
    }

    class ServerAuditSpecificationSpec {
        +Name string
        +AuditName string
        +ActionGroups []string
        +Enabled bool
    }

    class DatabaseAuditSpecification {
        -db *Database
        +SpecificationID int
        +Name string
        +AuditGUID string
        +AuditName string
        +IsEnabled bool
        +ActionGroups []string
        +Actions []DatabaseAuditAction
        +AddActions(groups, actions) error
        +DropActions(groups, actions) error
        +SetAudit(auditName) error
        +SetState(on) error
        +Drop() error
        +WithDisabled(ctx, fn) error
        Unlike the server half it also records
        individual actions on securables, not
        action groups alone.
    }

    class DatabaseAuditAction {
        +ActionName string
        +ClassDesc string
        +SchemaName string
        +ObjectName string
        +Principal string
        +AuditedResult string
        +FullName() string
    }

    class DatabaseAuditSpecificationSpec {
        +Name string
        +AuditName string
        +ActionGroups []string
        +Actions []DatabaseAuditAction
        +Enabled bool
    }

    class ServerTrigger {
        -server *Server
        +Name string
        +IsEnabled bool
        +CreateDate time.Time
        +ModifyDate time.Time
        +Events []string
        +Definition string
        +Enable() error
        +Disable() error
        +Drop() error
        A different family from Database.Triggers,
        which reads DML triggers on a table, and from
        DatabaseTrigger below, which reads DDL triggers
        scoped to one database.
    }

    class DatabaseTrigger {
        -db *Database
        +Name string
        +IsEnabled bool
        +CreateDate time.Time
        +ModifyDate time.Time
        +Events []string
        +Definition string
        +Database() *Database
        +Enable() error
        +Disable() error
        +Drop() error
        The parent_class = 0 family: DDL triggers on
        the database itself. DISABLE TRIGGER takes
        ON DATABASE here, not ON ALL SERVER.
    }

    %% =========================================================
    %% Certificates and the database master key
    %% =========================================================
    class AsymmetricKey {
        +Name string
        +KeyID int
        +PrincipalID int
        +Algorithm string
        +KeyLength int
        +PvtKeyEncryptionType string
        +Thumbprint []byte
        +HasPrivateKey() bool
    }

    class Certificate {
        -db *Database
        +Name string
        +CertificateID int
        +PrincipalID int
        +Subject string
        +PvtKeyEncryptionType string
        +StartDate time.Time
        +ExpiryDate time.Time
        +Thumbprint []byte
        +HasPrivateKey() bool
        +Encoded() []byte
        +Drop() error
    }

    class CertificateSpec {
        +Name string
        +Authorization string
        +Subject string
        +StartDate time.Time
        +ExpiryDate time.Time
        +EncryptionPassword string
        +FromBinary []byte
    }

    %% =========================================================
    %% Error log
    %% =========================================================
    class ErrorLogType {
        <<enumeration>>
        ErrorLogSQLServer
        ErrorLogAgent
    }

    class ErrorLogFile {
        +Number int
        +Date string
        +LastWritten time.Time
        +SizeBytes int64
    }

    class ErrorLogEntry {
        +LogDate string
        +Process string
        +Text string
        +Date time.Time
        +ErrorLevel int
        +Source() string
    }

    class LogSearch {
        +Text1 string
        +Text2 string
        +From time.Time
        +To time.Time
        xp_readerrorlog's own arguments 3-6:
        Text1 and Text2 are case-insensitive
        substrings AND-ed together, not two
        alternatives. A zero LogSearch reads
        the whole file. Filtering at the server
        is what makes the current log usable on
        a busy instance.
    }

    %% =========================================================
    %% Server filesystem (paths the SERVER resolves, not the client)
    %% =========================================================
    class FileSystemEntry {
        +Name string
        +FullPath string
        +IsDirectory bool
        +Size int64
        +LastModified time.Time
    }

    class FixedDrive {
        +Name string
        +Type string
        +FreeSpaceBytes int64
    }

    %% =========================================================
    %% Relationships
    %% =========================================================
    Server "1" --> "*" AvailabilityGroup : AvailabilityGroups() returns
    Server --> CreateAvailabilityGroupRequest : accepts
    Server "1" --> "1" DatabaseMirroringEndpoint : has at most one
    Server --> EndpointSpec : accepts
    Server "1" --> "*" Endpoint : Endpoints() returns
    Endpoint --> EndpointState : SetState() takes
    Endpoint ..> DatabaseMirroringEndpoint : MirroringDetail() returns
    Endpoint ..> ServiceBrokerEndpointDetail : ServiceBrokerDetail() returns
    Server "1" --> "*" ServerAudit : owns
    Server --> ServerAuditSpec : CreateServerAudit() takes
    ServerAudit --> ServerAuditStatus : Status() returns
    Server "1" --> "*" ServerAuditSpecification : owns
    Server --> ServerAuditSpecificationSpec : CreateServerAuditSpecification() takes
    ServerAuditSpecification "*" --> "1" ServerAudit : writes to
    Database "1" --> "*" DatabaseAuditSpecification : owns
    Database --> DatabaseAuditSpecificationSpec : CreateDatabaseAuditSpecification() takes
    DatabaseAuditSpecification "1" --> "*" DatabaseAuditAction : records
    DatabaseAuditSpecification "*" --> "1" ServerAudit : writes to
    Server "1" --> "*" ServerTrigger : owns
    Database "1" --> "*" DatabaseTrigger : owns
    Server "1" --> "*" ErrorLogFile : EnumErrorLogs() returns
    Server "1" --> "*" ErrorLogEntry : ReadLog() returns
    Server "1" --> "*" FileSystemEntry : EnumFileSystem() returns
    Server "1" --> "*" FixedDrive : FixedDrives() returns
    Server ..> ErrorLogType : selects log family with
    Server ..> LogSearch : ReadLogFiltered() narrowed by

    AvailabilityGroup "1" --> "*" AvailabilityReplica : has
    AvailabilityGroup "1" --> "*" AvailabilityDatabase : has
    AvailabilityGroup "1" --> "*" AvailabilityGroupListener : has
    AvailabilityGroup --> AvailabilityReplicaSpec : AddReplica() accepts
    AvailabilityGroup --> AvailabilityListenerSpec : AddListener() accepts
    AvailabilityGroupListener "1" --> "*" AvailabilityListenerIP : has
    AvailabilityListenerSpec "1" --> "*" AvailabilityListenerIPSpec : has
    CreateAvailabilityGroupRequest "1" --> "*" AvailabilityReplicaSpec : has
    CreateAvailabilityGroupRequest --> AvailabilityListenerSpec : may have
    AvailabilityReplica ..> DatabaseMirroringEndpoint : ships log through

    Database "1" --> "*" Certificate : contains
    Database "1" --> "*" AsymmetricKey : contains
    Database --> CertificateSpec : CreateCertificate() accepts
```

### Azure instance resources

The three instance-scoped views an Azure SQL Managed Instance exposes and an
on-premises instance has no analogue for: its own 15-second resource history,
the resource governor's fixed limits, and the Windows job object the engine
process runs inside. All three refuse with `ErrUnsupportedVersion` on a
non-Azure engine edition — see `azure_resources.go`.

```mermaid
classDiagram
    class ServerResourceStat {
        +StartTime time.Time
        +EndTime time.Time
        +ResourceType string
        +ResourceName string
        +SKU string
        +HardwareGeneration string
        +VirtualCoreCount int
        +AvgCPUPercent float64
        +ReservedStorageMB int64
        +StorageSpaceUsedMB float64
        +IORequests int64
        +IOBytesRead int64
        +IOBytesWritten int64
        One row per 15-second window, ~14 days
        retained. Pre-aggregated: plot it, never
        run it through a per-second delta.
    }

    class InstanceResourceGovernance {
        +ServerName string
        +CapCPU int
        +MaxLogRate int64
        +MaxWorkerThreads int
        +LocalIOPS int
        +ManagedXStoreIOPS int
        +ExternalXStoreIOPS int
        +LocalMaxOutstandingIO int
        +TempDBLogFileNumber int
        +DataDirectoryQuotaMB int
        +DataDirectoryUsageMB int
        +BufferPoolExtensionSizeGB int
        Fixed ceilings, not readings — the scale a
        ServerResourceStat history is read against.
    }

    class OSJobObject {
        +CPURate int
        +CPUAffinityMask int64
        +MemoryLimitMB int64
        +ProcessMemoryLimitMB int64
        +WorkingSetLimitMB int64
        +PeakJobMemoryUsedMB int64
        +TotalUserTime int64
        +TotalKernelTime int64
        +ReadOperationCount int64
        +WriteOperationCount int64
        The host's limits on SQL Server, below the
        governor's limits on itself.
    }

    Server --> ServerResourceStat : ServerResourceStats() / LatestServerResourceStats()
    Server --> InstanceResourceGovernance : InstanceResourceGovernance()
    Server --> OSJobObject : OSJobObject()
```

---

## Security

- **Passwords are escaped, never spliced in raw.** `CreateLogin`, `ChangePassword`, and `ChangePasswordWithOptions` quote the password as an `N'...'` literal through the same `nStringLiteral` escaping every other string literal in the package uses, so it's injection-proof regardless of password content.
- **Connection lifetimes are correctly scoped.** `Database.query` returns a `*dbRows` that owns both the `*sql.Rows` and the `*sql.Conn` pinned to run its `USE`, closing both together — `*sql.Rows.Close` on its own would leave that connection checked out of the pool for good.
- **Values that can't be parameterized are validated by shape or allowlist.** DDL can't parameterize keyword or literal arguments, so anything spliced into one is checked first: recovery models, data types, and backup actions against their known sets; partition function boundary values against the shape of a well-formed SQL Server literal; Query Store mode keywords and index data-compression settings against their allowlists.
- **One shared quoting implementation.** `QuoteName` and `QuoteLiteral` wrap the driver's own `TSQLQuoter`, so gosmo's internal identifier/literal escaping — and any caller or downstream consumer (e.g. gossms) building its own DDL — go through the same tested implementation rather than a hand-rolled one.
- **Permission and SET-option names are allowlisted, not interpolated.** `GRANT`/`DENY`/`REVOKE` and `ALTER DATABASE ... SET` are DDL and can't parameterize their keyword arguments; every method that accepts one (`GrantServerPermission`, `GrantPermission`, `GrantDatabasePermission`, `SetDatabaseOption`, ...) rejects any name not on its allowlist instead of splicing caller input directly into the statement.

---

## Packages

| Path        | Purpose                                                    |
| ----------- | ---------------------------------------------------------- |
| `/`         | All SMO types and logic                                    |
| `examples/` | Nine runnable programs — see [`examples/README.md`](examples/README.md) |

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

`Connect` opens the pool itself. Where the pool is not gosmo's to open — one
shared with the rest of an application, a driver wrapped for tracing or
retries, or a fake driver in a test — `gosmo.NewServer(ctx, db)` wraps an
existing `*sql.DB` and loads the same metadata. It is the inverse of
`srv.DB()`, and ownership passes to the `Server`, whose `Close` closes the
pool.

---

## Feature map

### Server

| SMO equivalent          | gosmo                                      |
| ----------------------- | ------------------------------------------ |
| `Server.Databases`      | `srv.Databases()` / `srv.Database(name)` (no-I/O handle) |
| Current database         | `srv.CurrentDatabase()`                    |
| Current login (`SUSER_NAME()`) | `srv.CurrentLogin()`                 |
| `Server.Logins`         | `srv.Logins()` / `srv.LoginByName(name)` / `srv.Login(name)` (no-I/O handle) |
| `Server.Roles`          | `srv.ServerRoles()` / `srv.ServerRoleByName(name)` / `srv.ServerRoleMembers(role)` |
| Server role administration | `role.Rename(newName)` / `role.ChangeOwner(owner)` / `srv.Add\|RemoveServerRoleMember(role, member)` |
| Drop a server role      | `srv.DropServerRole(name)` / `role.Drop()`  |
| Rename a database       | `srv.RenameDatabase(old, new, force)` — `force` puts it in single-user mode first |
| Detach a database       | `srv.DetachDatabase(name, gosmo.DetachOptions{...})` — leaves the files on disk; a detach that fails after `DropConnections` is put back to MULTI_USER |
| Attach a database       | `srv.AttachDatabase(gosmo.AttachSpec{Name, Files, Owner, RebuildLog})` — the name need not be the one it was detached under |
| Read a detached file    | `srv.DetachedDatabaseInfo(primaryFilePath)` → `*DetachedDatabase` (`.Name`, `.Files`, `.DataFiles()`, `.LogFiles()`) — the only way to learn a detached database's other files |
| `Server.LinkedServers`  | `srv.LinkedServers()`                      |
| `Server.Configuration`  | `srv.Configurations()`                     |
| `Server.JobServer` (Agent) | see [SQL Server Agent](#sql-server-agent) below |
| Active sessions         | `srv.ActiveSessions(includeSystem)`        |
| Kill session            | `srv.KillSession(id)`                      |
| Error log               | `srv.ReadLog(logType, n)` / `srv.ReadLogFiltered(logType, n, search)` / `srv.EnumErrorLogs(logType)` / `srv.CycleLog(logType)` — see [Error log](#error-log) |
| Database Mail           | `srv.MailProfiles()` / `srv.SendMail(...)` |
| Create login (safe)     | `srv.CreateLogin(name, password, opts)` — SQL, Windows, external provider, certificate or asymmetric key |
| Authentication mode     | `srv.SecurityInfo()`                       |
| Server-level permissions | `srv.ServerPermissions()` / `srv.Grant\|Deny\|RevokeServerPermission(...)` / `srv.ServerPermissionNames()` |
| Server permissions with modifiers | `srv.Grant\|Deny\|RevokeServerPermissionWithOptions(perm, principal, opts)` — `WITH GRANT OPTION`, `CASCADE`, `GRANT OPTION FOR` |
| Effective server permissions | `srv.EffectiveServerPermissions(login)` (`EXECUTE AS LOGIN` + `fn_my_permissions`) |
| Credentials              | `srv.Credentials()` / `srv.CredentialByName(name)` / `srv.Credential(name)` (no-I/O handle) / `srv.CreateCredential(spec)` / `cred.Alter(identity, secret)` / `cred.Drop()` — see [Credentials](#credentials) |
| Cryptographic providers  | `srv.CryptographicProviders()`             |
| Server audits            | `srv.ServerAudits()` / `srv.ServerAuditByName(name)` / `srv.ServerAudit(name)` (no-I/O handle) / `srv.CreateServerAudit(spec)` — see [Audits](#audits-and-audit-specifications) |
| Server audit specifications | `srv.ServerAuditSpecifications()` / `...ByName(name)` / `srv.ServerAuditSpecification(name)` (no-I/O handle) / `srv.CreateServerAuditSpecification(spec)` |
| Audit action groups      | `srv.AuditActionGroups()` / `srv.DatabaseAuditActionGroups()` / `srv.DatabaseAuditActions()` |
| Backup devices           | `srv.BackupDevices()` / `srv.BackupDeviceByName(name)` / `srv.BackupDevice(name)` (no-I/O handle) / `srv.CreateBackupDevice(name, type, physicalName)` / `dev.Drop(deleteFile)` / `dev.Headers()` |
| Endpoints (all protocols) | `srv.Endpoints()` / `srv.EndpointByName(name)` / `ep.SetState(state)` / `ep.Drop()` / `ep.MirroringDetail()` / `ep.ServiceBrokerDetail()` — see [Endpoints](#endpoints) |
| Server DDL / logon triggers | `srv.ServerTriggers()` / `srv.ServerTriggerByName(name)` / `srv.ServerTrigger(name)` (no-I/O handle) / `tr.Enable()` / `tr.Disable()` / `tr.Drop()` |
| Azure engine edition             | `srv.Info().IsAzure()` — the test every version gate asks before it believes `VersionMajor`, which Azure freezes |
| Azure resource history           | `srv.ServerResourceStats(max)` / `srv.LatestServerResourceStats()` — `sys.server_resource_stats`, one row per 15-second window |
| Azure resource limits            | `srv.InstanceResourceGovernance()` / `srv.OSJobObject()` — see [Azure instance resources](#azure-instance-resources) |
| Files of one database, in any state | `srv.DatabaseFiles(name)` — reads `sys.master_files`, so it answers for an OFFLINE / RECOVERY_PENDING / SUSPECT database that `db.Files()` cannot `USE` |
| Live memory stats        | `srv.MemoryStats()`                        |
| Languages                | `srv.Languages()`                          |
| Processors / NUMA topology | `srv.ProcessorInfo()`                    |
| Disk volumes              | `srv.DiskVolumes()`                        |
| `Server.EnumDirectories` / `EnumFiles` | `srv.EnumFileSystem(path)` / `srv.FixedDrives()` / `srv.FileSystemExists(path)` — see [Server filesystem](#server-filesystem) |
| Host OS family            | `srv.Info().Platform` (`"Windows"` / `"Linux"`, from `@@VERSION`) |
| `Server.AvailabilityGroups` | `srv.AvailabilityGroups()` / `srv.AvailabilityGroup(name)` (no-I/O handle) / `srv.AvailabilityGroupByName(name)` — see [Always On](#always-on-availability-groups) |
| Database mirroring endpoint | `srv.DatabaseMirroringEndpoint()` / `srv.CreateDatabaseMirroringEndpoint(spec)` |
| Verify / inspect a backup device | `srv.VerifyBackup(path)` / `srv.BackupHeaders(path)` / `srv.BackupFileList(path)` |
| ... from a path, a blob URL or a logical device | `srv.VerifyBackupFrom(t)` / `srv.BackupHeadersFrom(t)` / `srv.BackupFileListForSetFrom(t, n)`, with `t` = `gosmo.DiskTarget(path)`, `gosmo.URLTarget(url)` or `gosmo.DeviceTarget(name)` |
| Is this device a blob?    | `gosmo.IsBackupURL(device)` — decides `TO URL` vs `TO DISK`; a Managed Instance refuses DISK outright |
| Log backup chain state    | `srv.DatabaseRecoveryStatuses()` / `db.RecoveryStatus()` → `*DatabaseRecoveryStatus` |
| What may this login do?   | `srv.Capabilities()` → `*Capabilities` — see [Capabilities](#capabilities-of-the-connected-login) |
| Wrap a `*sql.DB` you already have | `gosmo.NewServer(ctx, db)` — the inverse of `srv.DB()` |

### Database

| SMO equivalent                  | gosmo                                       |
| ------------------------------- | ------------------------------------------- |
| Is a system database             | `db.IsSystem()`                             |
| `Database.Tables`               | `db.Tables()` / `db.TablesBySchema(schema)` |
| Bulk table/view + column snapshot | `db.Catalog()` (user objects) / `db.SystemCatalog()` (`sys` schema) |
| `Database.Views`                | `db.Views()` / `db.DropView(schema, name)`  |
| `Database.StoredProcedures`     | `db.StoredProcedures()`                     |
| `Database.UserDefinedFunctions` | `db.UserDefinedFunctions()` / `db.DropFunction(schema, name)` |
| System Views/Procedures/Functions | `db.SystemViews()` / `db.SystemStoredProcedures()` / `db.SystemFunctions()` |
| `Database.Schemas`              | `db.Schemas()` / `db.SchemaByName(name)` / `schema.ObjectCount()` / `schema.ObjectCountsByType()` |
| `Database.Users`                | `db.Users()` / `db.UserByName(name)`        |
| Database user administration    | `user.Rename(newName)` / `user.SetDefaultSchema(schemaName)` / `user.SetLogin(loginName)` |
| `Database.AuditSpecifications`  | `db.DatabaseAuditSpecifications()` / `...ByName(name)` / `db.DatabaseAuditSpecification(name)` (no-I/O handle) / `db.CreateDatabaseAuditSpecification(spec)` |
| `Database.Roles`                | `db.DatabaseRoles()` / `db.RoleByName(name)` / `db.RoleMembers(roleName)` |
| Database role administration    | `role.Rename(newName)` / `role.ChangeOwner(newOwner)` / `role.Drop()` / `db.DropDatabaseRole(name)` |
| `Database.FileGroups`           | `db.FileGroups()` — `fg.Type` is the `type_desc` (ROWS / FILESTREAM / MEMORY_OPTIMIZED), `fg.IsFileStream()` the common test |
| `Database.Triggers`             | `db.Triggers()` / `db.ObjectTriggers(schema, name)` (one table or view, by name) / `db.DropTrigger(schema, name)` |
| Database-scope DDL triggers     | `db.DatabaseTriggers()` / `db.DatabaseTriggerByName(name)` / `db.DatabaseTrigger(name)` (no-I/O handle) / `tr.Enable()` / `tr.Disable()` / `tr.Drop()` — see [Database DDL triggers](#database-ddl-triggers) |
| `Database.Sequences`            | `db.Sequences()` / `db.DropSequence(schema, name)` |
| `Database.Synonyms`             | `db.Synonyms()` / `db.DropSynonym(schema, name)` |
| Rename any `sp_rename`-able object | `db.RenameObject(schema, oldName, newName)` — view, procedure, function, sequence, synonym, trigger |
| Move an object to another schema | `db.TransferObject(targetSchema, schema, name)` — `ALTER SCHEMA ... TRANSFER`, which `sp_rename` cannot do |
| Parameters of a procedure or function | `db.Parameters(schema, name)` → `[]*Parameter` |
| Filtered listings                | `db.TablesFiltered(f)` / `ViewsFiltered` / `StoredProceduresFiltered` / `UserDefinedFunctionsFiltered` (and the `System...` forms) — see [Filtering a listing](#filtering-a-listing) |
| What may this login do here?     | `db.Capabilities()` → `*DatabaseCapabilities` |
| Partition functions             | `db.PartitionFunctions()` / `db.PartitionFunctionByName(name)` |
| Partition schemes               | `db.PartitionSchemes()` / `db.PartitionSchemeByName(name)` |
| Extended properties             | `db.ExtendedProperties(level)` / `db.AddExtendedProperty(...)` / `db.SetExtendedProperty(...)` / `db.DropExtendedProperty(...)` |
| `Database.Certificates`         | `db.Certificates()` / `db.CertificateByName(name)` / `db.CreateCertificate(spec)` / `cert.Drop()` — see [Certificates](#certificates-and-the-database-master-key) |
| `Database.AsymmetricKeys`       | `db.AsymmetricKeys()` / `db.AsymmetricKeyByName(name)` — read only; CREATE ASYMMETRIC KEY imports from the server's own filesystem |
| Database master key             | `db.HasMasterKey()` / `db.CreateMasterKey(password)` |
| Column master keys              | `db.ColumnMasterKeys()` / `db.ColumnMasterKeyByName(name)` / `db.CreateColumnMasterKey(...)` / `...WithSignature(...)` |
| Column encryption keys          | `db.ColumnEncryptionKeys()` / `db.ColumnEncryptionKeyByName(name)` / `db.CreateColumnEncryptionKey(name, values)` / `cek.AddValue(value)` / `cek.DropValue(masterKeyName)` — the two halves of a master-key rotation |
| Security policies (RLS)         | `db.SecurityPolicies()` / `db.SecurityPolicyByName(schema, name)` |
| Database scoped credentials     | `db.DatabaseScopedCredentials()` / `db.DatabaseScopedCredentialByName(name)` / `db.DatabaseScopedCredential(name)` (no-I/O handle) / `db.CreateDatabaseScopedCredential(spec)` — see [Credentials](#credentials) |
| `Database.RecoveryModel`        | `db.SetRecoveryModel(model)`                |
| `Database.CompatibilityLevel`   | `db.SetCompatibilityLevel(level)`           |
| Space used                      | `db.SpaceUsed()`                            |
| Every table's row count / space used, in one query | `db.TableRowCounts()` / `db.TableSpaceUsedAll()` (keyed by `object_id`) |
| ALTER DATABASE SET options      | `db.Options()` / `db.SetDatabaseOption(opt, value)` |
| Restrict access (single/multi/restricted user) | `db.SetUserAccess(mode)`     |
| Take offline / bring online     | `db.SetOffline()` / `db.SetOnline()`        |
| Change ownership                | `db.SetOwner(principal)`                    |
| Database Scoped Configuration   | `db.DatabaseScopedConfigs()` / `db.SetDatabaseScopedConfig(name, value, forSecondary)` |
| Query Store                     | `db.QueryStore()` / `db.SetQueryStoreOptions(opts)` / `db.FlushQueryStore()` / `db.ClearQueryStore()` |
| Query Store reports (SSMS's seven views) | `db.QueryStoreTopResourceQueries(opts)` / `.QueryStoreRegressedQueries(opts)` / `.QueryStoreHighVariationQueries(opts)` / `.QueryStoreForcedPlanQueries(opts)` / `.QueryStoreOverallConsumption(opts)` / `.QueryStoreTrackedQuery(queryID, opts)` / `.QueryStoreWaitCategories(opts)` + `.QueryStoreWaitingQueries(category, opts)` |
| Query Store plans and plan XML  | `db.QueryStorePlans(queryID, opts)` / `db.QueryStoreQueryText(queryID)` |
| Force / unforce a plan          | `db.QueryStoreForcePlan(queryID, planID)` / `db.QueryStoreUnforcePlan(queryID, planID)` |
| What a report can rank by       | `db.QueryStoreMetrics()` / `gosmo.QSStatistics()` / `gosmo.QSMetricUnit(m)` — metrics are version-gated, and `db.QueryStoreWaitStatsSupported()` gates the two wait reports (2017+) |
| Every file, incl. log           | `db.Files()`                                |
| Add / alter / remove file       | `db.AddFile(spec)` / `db.AlterFile(name, m)` / `db.RemoveFile(name)` |
| Add / remove filegroup          | `db.AddFileGroup(name)` / `db.RemoveFileGroup(name)` |
| Filegroup default / read-only   | `db.SetDefaultFileGroup(name)` / `db.SetFileGroupReadOnly(name, ro)` |
| CREATE DATABASE file placement  | `CreateDatabaseOptions.PrimaryFile` / `.LogFile` (`*DatabaseFileSpec`) |
| Change tracking                 | `db.ChangeTracking()` / `db.SetChangeTracking(info)` |
| Table change tracking           | `db.TableChangeTracking()` / `db.TableChangeTrackingFor(schema, name)` / `db.SetTableChangeTracking(...)` |
| Database-level permissions      | `db.DatabasePermissions()` / `db.Grant\|Deny\|RevokeDatabasePermission(...)` |

### Table

| SMO equivalent        | gosmo                              |
| --------------------- | ---------------------------------- |
| `Database.Tables` (no-I/O handle) | `db.Table(schema, name)` — works under `WithScript`, where `TableByName`'s catalog read has nothing to find |
| `Table.Columns`       | `t.Columns()`                      |
| `Table.Indexes`       | `t.Indexes()` / `t.IndexByName(name)` |
| XML indexes           | `t.XMLIndexes()` → `[]*XMLIndex` (primary/secondary, and which primary) |
| `Table.ForeignKeys`   | `t.ForeignKeys()` / `t.ForeignKeyByName(name)` |
| `Table.Checks`        | `t.CheckConstraints()`             |
| `Table.Statistics`    | `t.Statistics()` / `t.StatisticByName(name)` |
| `Table.Partitions`    | `t.Partitions()`                   |
| `Table.Triggers`      | `t.Triggers()`                     |
| `Table.RowCount`      | `t.RowCount()` (all tables at once: `db.TableRowCounts()`) |
| Rows matching a filter predicate | `t.CountWhere(predicate)`  |
| Validate a filter predicate | `t.CheckWhereSyntax(predicate)` |
| Object details (lock escalation, ANSI_NULLS, CDC, temporal, ledger, ...) | `t.Detail()` |
| Space used (`sp_spaceused`-style) | `t.SpaceUsed()` (all tables at once: `db.TableSpaceUsedAll()`) |
| Truncate              | `t.TruncateTable()`                |
| Fragmentation         | `t.FragmentationStats(mode)`       |
| Rebuild all indexes   | `t.RebuildAllIndexes(fillFactor)`  |
| Update all statistics | `t.UpdateAllStatistics(samplePct)` |
| Create index          | `t.CreateIndex(req)` — every index type, see below |
| Alter column          | `t.AlterColumn(col)`               |
| Drop / rename a column | `t.DropColumn(name)` / `t.RenameColumn(name, newName)` |
| Drop a constraint     | `t.DropConstraint(name)`           |
| Where the rows live (`ON` clause) | `t.DataSpace()` → `DataSpace` (filegroup or partition scheme) |
| Columns of a table *or view* | `db.ObjectColumns(schema, name)` — `Table.Columns` reaches tables only |

### Index

| gosmo                               |
| ----------------------------------- |
| `idx.Rebuild(t, fillFactor)`        |
| `idx.RebuildWithOptions(t, fillFactor, padIndex, dataCompression)` |
| `idx.Reorganize(t)`                 |
| `idx.Disable(t)` / `idx.Enable(t)` |
| `idx.Rename(t, newName)` — also renames a PK/UNIQUE constraint |
| `idx.SetOptions(t, ignoreDupKey, allowRowLocks, allowPageLocks)` |
| `idx.SetLockOptions(t, allowRowLocks, allowPageLocks)` — no `IGNORE_DUP_KEY`, which a PK/UNIQUE-backing index rejects |
| `idx.SetIncludedColumns(t, columns)` — via `CREATE INDEX ... DROP_EXISTING` |
| `idx.UpdateStatistics(t)`           |
| `idx.StorageInfo(t)` — filegroup, partitioning, allocation-unit space |
| `idx.Fragmentation(t, mode)` — one index (`t.FragmentationStats(mode)` does all) |
| `idx.DataSpace` — the filegroup or partition scheme it is on, read with the index |
| `idx.Drop(t)`                       |

`Index.Type` is a `sys.indexes.type_desc` value — `IndexTypeClustered`,
`IndexTypeNonClustered`, `IndexTypeXML`, `IndexTypeSpatial`,
`IndexTypeColumnStore`, `IndexTypeClusteredColumnStore`, or the server's own
text for a type gosmo has no constant for (e.g. `NONCLUSTERED HASH`), so it
is never empty for an index that exists. `idx.Type.IsColumnStore()` covers
both columnstore forms — neither has an `INCLUDE` list, so
`SetIncludedColumns` rejects them rather than silently producing a rowstore
index.

`CreateIndexRequest` creates any of them, and which of its fields apply
depends on `Type`:

| Type                          | Statement                                    | Its own fields |
| ----------------------------- | -------------------------------------------- | -------------- |
| `IndexTypeClustered` / `IndexTypeNonClustered` (and the zero value) | `CREATE [UNIQUE] CLUSTERED\|NONCLUSTERED INDEX` | `IsUnique`, `IncludedColumns` and `FilterDefinition` (nonclustered only) |
| `IndexTypeColumnStore`        | `CREATE NONCLUSTERED COLUMNSTORE INDEX`      | `FilterDefinition`, `CompressionDelay` |
| `IndexTypeClusteredColumnStore` | `CREATE CLUSTERED COLUMNSTORE INDEX`       | takes no key columns at all — it covers every column |
| `IndexTypeXML`                | `CREATE [PRIMARY] XML INDEX`                 | `IsPrimaryXML`, or `PrimaryXMLIndex` + `SecondaryXMLType` |
| `IndexTypeSpatial`            | `CREATE SPATIAL INDEX ... USING`             | `Tessellation`, `BoundingBox` (the `GEOMETRY_` schemes), `GridLevels`, `CellsPerObject` |

A combination the server would reject is refused before anything is
executed — a unique columnstore index, an ordered columnstore column list, a
fill factor on a columnstore index, a geography index with a bounding box, a
secondary XML index naming no primary — with an error naming the field
rather than a parse error naming a column number.

### Statistics

| SSMS equivalent                | gosmo                                     |
| ------------------------------ | ----------------------------------------- |
| Statistics of a table          | `t.Statistics()` / `t.CreateStatistic(name, cols, pct)` |
| ... with a filter, `FULLSCAN`, `NORECOMPUTE`, `INCREMENTAL` | `t.CreateStatisticWithOptions(req)` |
| Statistic's key columns        | `st.Columns()`                            |
| `DBCC SHOW_STATISTICS` header  | `st.Header()` → `*StatisticHeader`        |
| ... density vector             | `st.DensityVector()` → `[]*StatisticDensity` |
| ... histogram                  | `st.Histogram()` → `[]*StatisticHistogramStep` |
| One statistic by name          | `t.StatisticByName(name)`                 |
| Update / drop                  | `st.Update(samplePct)` / `st.Drop()`      |
| Rename                         | `st.Rename(newName)`                      |

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
| `login.ResolveMapping()` — fills `login.MappedObject` for a certificate- or asymmetric-key-mapped login |
| `login.UserMappings()` / `login.MapToDatabase(...)` / `login.UnmapFromDatabase(db)` |

### Capabilities of the connected login

What the login on this connection may actually do — its fixed-role
memberships and its permission states — in one round trip per scope, so a
caller can gate its UI up front instead of discovering permissions from
failed calls.

```go
caps, _ := srv.Capabilities()
if caps.Has("ALTER ANY LOGIN") { /* offer New Login */ }
if !caps.Allows("SHUTDOWN")    { /* grey out Shutdown */ }

dcaps, _ := db.Capabilities()
if !dcaps.Accessible            { /* don't expand this database at all */ }
if dcaps.Permits("BACKUP DATABASE") { /* offer Back Up */ }
```

`ProbedServerRoles`, `ProbedServerPermissions`, `ProbedDatabaseRoles` and
`ProbedDatabasePermissions` are the names that get asked about — a working
subset chosen for what an application actually gates on, not the grantable
catalogs `ServerPermissionNames()`/`DatabasePermissionNames()` return.

**The answer is three-way, and that is the point.** `HAS_PERMS_BY_NAME`
returns NULL *without raising* for a permission the instance does not define,
so a permission introduced in a later version reads as `CapabilityUnknown` on
an older one rather than as denied, and a caller that folds unknown into
denied hides a feature on every instance that names it differently.

| Question                       | Method                                       |
| ------------------------------ | -------------------------------------------- |
| Should I *offer* this?         | `Has(name)` — known to be held                |
| Should I *withhold* this?      | `Allows(name)` (server) / `Permits(name)` (database) — not known to be denied |
| Exact state                    | `Permission(name)` → `CapabilityGranted` / `CapabilityDenied` / `CapabilityUnknown` |
| Role membership                | `InServerRole(name)` / `InRole(name)`, `IsSysadmin()` |
| Did the probe run at all?      | `Probed()`                                   |

`Has` and `Allows` are deliberately not opposites: withholding must **fail
open**, because the server remains the authority and refusing something a
sysadmin may well be allowed to do is the worse error. `sysadmin` is not
folded into `InServerRole` because SQL Server does not fold it in either —
`IS_SRVROLEMEMBER('SQLAgentUserRole')` is 0 for `sa` — and a role test cannot
fail open on its own, which is what `Probed()` is for: `InServerRole` answers
false for a role never asked about exactly as for one the login is not in.

At database scope, `Accessible` is `HAS_DBACCESS` and is the one field to
check before expanding a database or opening its properties, since every
folder under an inaccessible one fails separately and identically. A database
the login cannot open answers unknown to every permission — there was nothing
inside it to ask — so `Allows` alone would report "not known to be denied"
for exactly the databases the login has no business writing to. `Permits` is
`Allows` plus that accessibility, and is the database-scope test for
withholding. Every method is nil-safe; a nil `*DatabaseCapabilities` is
"nothing known" and fails open, but the **zero value** is not — its
`Accessible` is false, which reads as a measured "cannot open this".

#### Schema, object and column scope

A database probe also answers for the securables inside it, so an action can
be gated on the thing it actually touches rather than on database-wide
permission.

| Scope  | Offer                          | Withhold                                        |
| ------ | ------------------------------ | ----------------------------------------------- |
| Schema | `HasOnSchema(schema, name)`    | `AllowsOnSchema` / `PermitsOnSchema` / `DeniedOnSchema` |
| Object | `HasOnObject(schema, obj, name)` | `DeniedOnObject(schema, obj, name)`           |
| Column | `HasOnColumn(schema, obj, col, name)` | `DeniedOnColumn(...)` / `DeniedOnAnyColumn(schema, obj, name)` |

`ProbedSchemaPermissions` and `ProbedObjectPermissions` name what is asked
about; `ObjectKey(schema, object)` and `ColumnKey(schema, object, column)`
build the map keys of the raw `SchemaPermissions`, `ObjectPermissions` and
`ColumnPermissions` blocks. `SchemaPermission`, `ObjectPermission` and
`ColumnPermission` return the exact `CapabilityState`.

**Schema scope works like database scope; object and column scope do not.**
The schema block is asked with `HAS_PERMS_BY_NAME`, once per schema, so it
answers three ways and folds in whatever implies the permission — a principal
holding `CONTROL` on the schema, or `ALTER ANY SCHEMA`, or `db_owner`,
answers granted for `ALTER` without any of those being asked separately. The
object and column blocks are read straight out of `sys.database_permissions`
instead, one query for the whole database rather than one per object, and so
report only what is **explicit**: an object with no grant, no deny and no
distinct owner has no row at all. That makes `HasOnObject` an *additional*
reason to permit something — alongside the database- and schema-scope answers
— and never a reason to withhold it. The withholding reads are the `Denied*`
ones, which ask for a state that was recorded rather than for the absence of
one, and a principal denied on the object cannot write it however wide its
other grants are: SQL Server resolves DENY over GRANT across scopes.

Two exceptions belong to the caller. A member of `sysadmin` bypasses the
check entirely and must be asked about first — the probe reads permissions
through `public`, and a DENY to `public` is recorded for the one login the
server never applies it to. And a database that was never probed records
nothing, which `Probed()` reports.

`DeniedOnAnyColumn` is a separate question from `DeniedOnObject` because
column rows live in their own block: an action that touches the whole object
is withheld by a denial on any single column, but recording that denial on
the table would make it a denial of every column.

#### Explicit denials, and per-securable server scope

`HAS_PERMS_BY_NAME` answers what the login *effectively* has, which is why a
DENY it does not currently lose to is invisible in it. The `Explicit*` blocks
are catalog reads of the recorded DENY rows beside it, so an action can be
withheld on the grounds the server will actually refuse it:

| Scope                             | Withhold                                     |
| --------------------------------- | -------------------------------------------- |
| A named database (server scope)   | `caps.DeniedOnDatabase(name)`                 |
| A login, server role or endpoint  | `caps.DeniedOnLogin` / `DeniedOnServerRole` / `DeniedOnEndpoint`, or `DeniedOnServerSecurable(kind, name, permission)` |
| A database user or role           | `dcaps.DeniedOnPrincipal(principal, name)`    |
| An availability group             | `caps.AvailabilityGroupPermission(group, name)` / `HasOnAvailabilityGroup` / `PermitsOnAvailabilityGroup` |

`ExplicitServerPermissions` is keyed by `ServerSecurableKey(kind, name)`, so
a login, a server role and an endpoint of the same name stay apart;
`ServerSecurableKind` is `ServerSecurableLogin`, `ServerSecurableServerRole`
or `ServerSecurableEndpoint`. `ProbedServerSecurablePermissions` and
`ProbedAvailabilityGroupPermissions` name what is asked about.

Availability-group scope is the exception in this group: it is a
`HAS_PERMS_BY_NAME` probe like schema scope, not a catalog read, so it answers
three ways and has both an offering and a withholding form. Everything else
here reads DENY rows only, and so answers exactly one question — is this
recorded as denied — never "is it granted".

The same two caveats as above still belong to the caller: `sysadmin` bypasses
every check, and an unprobed scope records nothing.

### Filtering a listing

An `ObjectFilter` narrows a catalog listing at the server rather than in the
caller — the SSMS Object Explorer "Filter Settings" dialog.

```go
tables, _ := db.TablesFiltered(gosmo.ObjectFilter{
    Name:    []gosmo.TextCriterion{{Op: gosmo.TextContains, Value: "order"}},
    Schema:  []gosmo.TextCriterion{{Op: gosmo.TextNotEquals, Value: "staging"}},
    Created: []gosmo.DateCriterion{{Op: gosmo.DateAfter, Day: cutoff}},
})
```

| Family                 | Method                                    |
| ---------------------- | ----------------------------------------- |
| Tables                 | `db.TablesFiltered(f)`                    |
| Views                  | `db.ViewsFiltered(f)` / `db.SystemViewsFiltered(f)` |
| Stored procedures      | `db.StoredProceduresFiltered(f)` / `db.SystemStoredProceduresFiltered(f)` |
| Functions              | `db.UserDefinedFunctionsFiltered(f)` / `db.SystemFunctionsFiltered(f)` |

Criteria are AND-ed, never OR-ed, and a zero `ObjectFilter` narrows nothing —
the unfiltered listing is the same call with an empty one (`f.Empty()`
reports which). `TextOp` is `TextContains`, `TextNotContains`, `TextEquals`
or `TextNotEquals`; `DateOp` is `DateOn`, `DateBefore` or `DateAfter`, all
over whole calendar days, since a creation date is a timestamp and "created
on the 20th" means the day. `MemoryOptimized` applies only to the table
listing, whose catalog view is the only one with such a column; elsewhere it
is ignored rather than failing, because a filter describes what the caller
wants and not every family can express all of it.

Matching is case-insensitive **regardless of the database's collation** — the
comparison lowercases both sides, because a bare `LIKE` follows the collation
and would drop rows on a case-sensitive instance. The pattern is escaped with
an `ESCAPE` clause, because `%`, `_` and `[` are legal in an identifier and
an unescaped filter for `pct_1` also matches `pct1100`.

### Dependencies, search, permissions, and execution plans

| SMO / SSMS equivalent      | gosmo                                                     |
| --------------------------- | ---------------------------------------------------------- |
| Object dependencies (uses)  | `db.Dependencies(schema, name)`                            |
| Object dependencies (used by) | `db.Dependents(schema, name)`                            |
| Object search                | `db.Search(pattern)`                                      |
| Securable search (for a permissions picker) | `db.FindSecurables(gosmo.SecurableSearch{Name: ..., Limit: ...})` → `[]SecurableRef` (schemas, tables, views) |
| Object permissions           | `db.Permissions(schema, name)`                            |
| Grant / deny / revoke        | `db.GrantPermission(...)` / `db.DenyPermission(...)` / `db.RevokePermission(...)` |
| Schema permissions            | `db.SchemaPermissions(schema)`                            |
| Grant / deny / revoke (schema) | `db.GrantSchemaPermission(...)` / `db.DenySchemaPermission(...)` / `db.RevokeSchemaPermission(...)` |
| Every securable one principal holds | `db.PermissionsForPrincipal(principal)`             |
| Permissions with modifiers   | `db.Grant\|Deny\|RevokePermissionWithOptions(...)` / `...SchemaPermissionWithOptions(...)` / `...DatabasePermissionWithOptions(...)` |
| Column permissions           | `db.ColumnPermissions(schema, name)` / `db.ColumnPermissionsForPrincipal(principal)` |
| Grant / deny / revoke (column) | `db.Grant\|Deny\|RevokeColumnPermission(schema, name, perm, cols, principal)` |
| Effective permissions        | `db.EffectivePermissions(principal)` / `db.EffectiveObjectPermissions(schema, name, principal)` / `db.EffectiveSchemaPermissions(schema, principal)` |
| Permission-name catalogs (for pickers) | `gosmo.ObjectPermissionNames()` / `SchemaPermissionNames()` / `DatabasePermissionNames()` / `ServerPermissionNames()` / `ColumnPermissionNames()` |
| Estimated execution plan     | `db.EstimatedPlan(sql)` (`SET SHOWPLAN_XML`, statement not run) |
| Actual execution plan        | `db.ActualPlan(sql)` (`SET STATISTICS XML`, statement runs)|
| Every plan a multi-statement batch produced | `plan.All` (`plan.XML` is the last of them) |

Every `Grant|Deny|Revoke...` method has a `...WithOptions` counterpart taking
a `PermissionOptions`, at all four scopes (object, column, schema, database,
server). The zero value renders exactly the statement the plain method
renders — the plain methods *are* one-line delegations to the `WithOptions`
form, so there is one renderer and one set of error strings rather than two
that have to be kept in step.

```go
// WITH GRANT OPTION, and the CASCADE that taking such a grant back requires.
db.GrantPermissionWithOptions("dbo", "Orders", gosmo.PermSelect, "app_reader",
    gosmo.PermissionOptions{WithGrantOption: true})
db.RevokePermissionWithOptions("dbo", "Orders", gosmo.PermSelect, "app_reader",
    gosmo.PermissionOptions{Cascade: true})

// Downgrade WITH GRANT OPTION back to a plain GRANT (REVOKE GRANT OPTION FOR).
db.RevokePermissionWithOptions("dbo", "Orders", gosmo.PermSelect, "app_reader",
    gosmo.PermissionOptions{GrantOptionOnly: true})
```

A modifier the verb has no form for is rejected rather than quietly dropped —
`WithGrantOption` on a `DENY`, `Cascade` on a `GRANT`. Column permissions are
their own grants, separate from the object-level ones (`Permissions` reports
those), and only `SELECT`, `UPDATE` and `REFERENCES` have a column-level form
at all; `ColumnPermissionNames()` is that catalog.

`Effective*Permissions` answers "what can this principal actually do", with
role membership, inherited scopes, ownership and `DENY` already resolved —
SSMS's Effective tab. It resolves by impersonating the principal, so the
argument must be a database *user* (or, for `srv.EffectiveServerPermissions`,
a login): SQL Server refuses to impersonate a role, and `fn_my_permissions`
has no principal argument to use instead.

### Query Store reports

SSMS's seven Query Store views, plus the plan list and the forcing behind
Force/Unforce Plan.

```go
opts := gosmo.QueryStoreReportOptions{
    Metric:    gosmo.QSMetricDuration,
    Statistic: gosmo.QSStatAvg,
    From:      time.Now().Add(-24 * time.Hour),
    Top:       25,
}
rows, _ := db.QueryStoreTopResourceQueries(opts)
plans, _ := db.QueryStorePlans(rows[0].QueryID, opts)
db.QueryStoreForcePlan(rows[0].QueryID, plans[0].PlanID)
```

| SSMS view                    | gosmo                                              |
| ---------------------------- | -------------------------------------------------- |
| Top Resource Consuming Queries | `db.QueryStoreTopResourceQueries(opts)` → `[]*QSQueryStat` |
| Regressed Queries            | `db.QueryStoreRegressedQueries(opts)`               |
| Queries With High Variation  | `db.QueryStoreHighVariationQueries(opts)`           |
| Queries With Forced Plans    | `db.QueryStoreForcedPlanQueries(opts)`              |
| Overall Resource Consumption | `db.QueryStoreOverallConsumption(opts)` → `[]*QSIntervalStat` |
| Tracked Queries              | `db.QueryStoreTrackedQuery(queryID, opts)` → `[]*QSPlanIntervalStat` |
| Query Wait Statistics        | `db.QueryStoreWaitCategories(opts)` → `[]*QSWaitStat`, then `db.QueryStoreWaitingQueries(category, opts)` |
| The plans of one query       | `db.QueryStorePlans(queryID, opts)` → `[]*QSPlan` (`.QueryPlanXML`) |
| The statement behind a row   | `db.QueryStoreQueryText(queryID)`                   |
| Force / unforce a plan       | `db.QueryStoreForcePlan(queryID, planID)` / `db.QueryStoreUnforcePlan(queryID, planID)` |

`Metric` and `Statistic` pick what rows are ranked by; empty means average
duration. `From`/`To` bound the window half-open, a zero `To` meaning now and
a zero `From` an hour before it. `BaselineFrom`/`BaselineTo` and
`MinRegressionPct` apply to the Regressed Queries report alone, `QueryIDs` to
the four per-query reports, and `Top` caps every one of them.

**Query Store stores raw engine units, not display ones** — durations in
microseconds, I/O and memory in 8-KB pages, waits in milliseconds — so
`gosmo.QSMetricUnit(m)` reports which, and a caller formatting a value has to
ask. `db.QueryStoreMetrics()` returns the metrics the connected instance
supports and `db.QueryStoreWaitStatsSupported()` gates the two wait reports,
which need SQL Server 2017. A database with Query Store turned off is not an
error: the catalog views exist and are empty, so every report returns no
rows.

### Detach and attach

| SSMS equivalent                    | gosmo                                              |
| ---------------------------------- | -------------------------------------------------- |
| Tasks → Detach                     | `srv.DetachDatabase(name, gosmo.DetachOptions{...})` |
| Databases → Attach                 | `srv.AttachDatabase(gosmo.AttachSpec{Name, Files, Owner, RebuildLog})` |
| The Attach dialog's file list      | `srv.DetachedDatabaseInfo(primaryFilePath)` → `*DetachedDatabase` |
| The paths to detach from           | `srv.DatabaseFiles(name)` — reads `sys.master_files`, so it answers for a database `db.Files()` cannot `USE` |

`DetachOptions`' three fields are named for what they **do**, not for
`sp_detach_db`'s parameters, whose senses are inverted — so the zero value
skips the statistics update and keeps the full-text index files, which is
both what SSMS offers unchecked and what a large database wants.
`DropConnections` rolls back and disconnects everything using the database
first; a detach that then fails puts it back to `MULTI_USER`, so a refusal
never leaves the database single-user.

`DetachedDatabaseInfo` reads the file list held *inside* a detached primary
data file (the undocumented `DBCC CHECKPRIMARYFILE` that SMO — and so SSMS —
uses). It is the only way to learn a detached database's other files, and so
the only way an Attach dialog can be built: `.DataFiles()` and `.LogFiles()`
split what it returns.

### Scripter

```go
sc := gosmo.NewScripter(db, gosmo.DefaultScriptOptions())
ddl, _ := sc.ScriptTable("dbo", "MyTable")
ddl, _ := sc.ScriptView("dbo", "MyView")
ddl, _ := sc.ScriptStoredProcedure("dbo", "MyProc")
ddl, _ := sc.ScriptFunction("dbo", "MyFunc")
ddl, _ := sc.ScriptTrigger("dbo", "MyTrigger")
ddl, _ := sc.ScriptIndex("dbo", "MyTable", "IX_MyTable_a")
ddl, _ := sc.ScriptCheckConstraint("dbo", "MyTable", "CK_MyTable_a")
ddl, _ := sc.ScriptForeignKey("dbo", "MyTable", "FK_MyTable_Other")
ddl, _ := sc.ScriptSequence("dbo", "MySeq")
ddl, _ := sc.ScriptSynonym("dbo", "MySyn")
ddl, _ := sc.ScriptSchema("sales")
ddl, _ := sc.ScriptUser("app_user")
ddl, _ := sc.ScriptDatabaseRole("app_rw")
ddl, _ := sc.ScriptPartitionFunction("pfMonthly")
ddl, _ := sc.ScriptPartitionScheme("psMonthly")
ddl, _ := sc.ScriptSecurityPolicy("sec", "TenantFilter")
ddl, _ := sc.ScriptColumnMasterKey("CMK1")
ddl, _ := sc.ScriptColumnEncryptionKey("CEK1")
ddl, _ := sc.ScriptDatabase()

// Logins and server roles belong to no database, so they have their own
// scripter.
ssc := gosmo.NewServerScripter(srv, gosmo.DefaultScriptOptions())
ddl, _ := ssc.ScriptLogin("app_login")
ddl, _ := ssc.ScriptServerRole("ops")
ddl, _ := ssc.ScriptEndpoint("Hadr_endpoint")
ddl, _ := ssc.ScriptCredential("AzureBlob")
ddl, _ := ssc.ScriptBackupDevice("NightlyFull")
ddl, _ := ssc.ScriptServerAudit("Audit-Logins")
ddl, _ := ssc.ScriptServerAuditSpecification("Spec-Logins")
ddl, _ := ssc.ScriptServerTrigger("trg_ddl_guard")
```

A scripted credential carries a `<insert secret here>` placeholder: the
secret is not readable from the catalog, and emitting nothing there would
produce a statement that runs and creates a credential that cannot
authenticate.

`ScriptOptions.Verb` selects the statement form: `ScriptCreate` (the
default), `ScriptDrop`, `ScriptDropAndCreate` — the DROP and the CREATE, in
that order and in separate batches — or `ScriptAlter`, which applies to
module objects (view, procedure, function, trigger) and rewrites the leading
`CREATE` of the stored definition, leaving a `CREATE OR ALTER` definition
alone. The older `ScriptDrops bool` is still honoured, and means
`Verb = ScriptDrop` while `Verb` is left at its zero value.

There are also DML templates, in the shape SSMS's "SELECT To"/"INSERT To"
produce — `ScriptSelect`, `ScriptInsert`, `ScriptUpdate`, `ScriptDelete`,
`ScriptExecute` and `ScriptFunctionCall`. They carry `<name, type,>`
placeholders wherever the operator has to supply a value and are
deliberately not runnable as they stand; `ScriptInsert`/`ScriptUpdate` leave
out identity and computed columns, which reject an explicit value. The
parameter metadata behind `ScriptExecute` is `Database.Parameters(schema,
name)`, which reads `sys.parameters` for a procedure or function.

`ScriptOptions.IncludeHeaders` and `IncludeIfNotExists` apply to
`ScriptTable` and `ScriptDatabase` only — the view/procedure/function/trigger
methods return the module's definition verbatim from `sys.sql_modules` and
synthesize no DDL to guard. The existence check is per statement, never a
block spanning several: `GO` is a client-side batch break, so a `BEGIN`
block containing one is split across batches and the script can't parse.
`ScriptTable` emits a unique constraint as the `ALTER TABLE ... ADD
CONSTRAINT` it really is rather than as a `CREATE INDEX`, and skips XML and
spatial indexes with a comment naming what was left out, their DDL having no
generic form here.

### Iterators (`*Seq`)

Every collection method has a `FooSeq(ctx, ...)` counterpart in `iter.go`
returning an `iter.Seq2[T, error]`, for ranging over a collection without
materializing the slice at the call site:

```go
for t, err := range db.TableSeq(ctx) {
    if err != nil { return err }
    fmt.Println(t.FullName())
}
```

The fetch is deferred until the iterator is ranged over — an iterator built
and never ranged queries nothing — but it is **not streaming**: the
underlying `FooContext` method runs to completion first, and the loop then
yields from the slice it returned. So `ctx` cancels the fetch, an error
arrives as a single `(zero, err)` yield in place of any items rather than
partway through, and breaking out early saves no query work and no memory.
These exist for range-over-func ergonomics, not to bound memory or stop the
server mid-scan; where that matters, use the `...Context` method with a
bounded query.

**Breaking, since `v0.0.7`:** these took no `context.Context` before — they
wrapped the non-`Context` collection method, i.e. `context.Background()`.
`db.TableSeq()` becomes `db.TableSeq(ctx)`, for all 75 that existed then
(98 now).

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

Bound parameters are substituted into the captured text as literals, since
a script pasted into a query editor has nothing to bind `@p1` to, and
`ExecProc` is captured as the `EXEC` form it would run — inputs as literals,
`OUTPUT` parameters as a `DECLARE`d variable — rather than as the bare
procedure name the driver sends over RPC.

`gosmo.Scripting(ctx)` reports whether a context is one of these. It matters
to any caller mirroring a write into its own state: under `WithScript` the
write returns success without the server ever seeing it, so a rename
followed by a re-read *by the new name* finds nothing. gosmo honours this
for its own cached state too — a scripted `Rename`/`Enable`/`SetOwner`
leaves the object it was called on unchanged. The lookup-free handles
(`srv.Database(name)`, `srv.Login(name)`, `srv.Alert(name)`, `srv.Job(name)`,
`srv.Operator(name)`, `srv.Schedule(name)`) exist for the same reason: an
object whose `CREATE` was only collected can't be found by a `...ByName`
query, and the `Create*` methods return one of these handles under
`WithScript`.

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
    // Optional: which backup set on the device to restore (WITH FILE = n,
    // 1-based, as reported by BackupHeader.Position). Left at 0, SQL Server
    // restores the first set — so an appended differential or log needs this.
    FileNumber: 1,
    // Optional: same progress callback as Backup, above.
    Progress: func(pct int, message string) { fmt.Println(pct, message) },
})

// File / filegroup backup and restore. There is no BACKUP FILES verb in
// T-SQL — these render as a BACKUP/RESTORE DATABASE carrying FILE = /
// FILEGROUP = clauses — and at least one file or filegroup is required.
srv.Backup(gosmo.BackupOptions{
    Database:   "MyDB",
    Action:     gosmo.BackupActionFiles,
    FileGroups: []string{"FG_Archive"},
    Devices:    []string{`C:\Backups\MyDB_FG.bak`},
})

// Inspect a backup device before restoring — SSMS's Restore Database
// dialog's backup-set/file picker.
headers, _ := srv.BackupHeaders(`C:\Backups\MyDB.bak`)
files, _ := srv.BackupFileList(`C:\Backups\MyDB.bak`) // first set on the device
err := srv.VerifyBackup(`C:\Backups\MyDB.bak`)

// A device backups were appended to holds one set per backup, and their file
// lists differ. Pass the same 1-based set number to the file list and to the
// restore, or the MOVE clauses name logical files the restored set doesn't
// contain and SQL Server rejects the statement.
files, _ = srv.BackupFileListForSet(`C:\Backups\MyDB.bak`, headers[1].Position)
```

#### Backing up to Azure Storage

A device that is an `http`/`https` URL is a blob, and renders as `TO URL` /
`FROM URL` rather than `TO DISK`. `BackupOptions.Devices` and
`RestoreOptions.Devices` are plain strings, so the shape decides it —
`gosmo.IsBackupURL(device)` is the same test a caller can ask itself, and
`gosmo.URLTarget(url)` states it outright on the RESTORE-side reads.

It is not a cosmetic difference on Azure SQL Managed Instance, which refuses
every `DISK` device with *"SQL Database Managed Instance supports database
restore from URI backup device only"* — and whose
`SERVERPROPERTY('InstanceDefaultBackupPath')` is itself a container URL, so a
caller building a default destination out of it arrives holding a URL without
having decided to.

`BackupOptions.Credential` / `RestoreOptions.Credential` name the SQL Server
credential the statement authenticates with (`WITH CREDENTIAL = N'...'`), the
storage-account-key form. Leave it empty for the shared access signature
form, which is what Managed Instance uses: there the credential's *name* is
the container URL and SQL Server finds it itself, so naming it is not merely
unnecessary but wrong.

#### Logical backup devices

A backup device is a named alias for a physical location — SSMS's Server
Objects → Backup Devices — usable anywhere `BACKUP` or `RESTORE` takes one.

| SSMS equivalent               | gosmo                                                    |
| ----------------------------- | -------------------------------------------------------- |
| Server Objects → Backup Devices | `srv.BackupDevices()` / `srv.BackupDeviceByName(name)` / `srv.BackupDevice(name)` (no-I/O handle) |
| New backup device             | `srv.CreateBackupDevice(name, gosmo.BackupDeviceDisk, path)` |
| Delete (optionally the file)  | `dev.Drop(deleteFile)`                                    |
| Contents                      | `dev.Headers()` → `[]*BackupHeader`                       |

There is no `Alter`, deliberately: `sp_addumpdevice` and `sp_dropdevice` are
the whole write surface, and a device's name, type and physical path are
fixed at creation.

The three RESTORE-side reads take a `BackupTarget` in their `…From` form, so
they can read a device as well as a path:

```go
t := gosmo.DeviceTarget("NightlyFull")     // or gosmo.DiskTarget(path)
headers, _ := srv.BackupHeadersFrom(t)
files, _   := srv.BackupFileListForSetFrom(t, headers[0].Position)
err := srv.VerifyBackupFrom(t)
```

The two forms are not interchangeable, which is why they are separate
constructors rather than one string: a logical device is named bare (`FROM
[NightlyFull]`), and passing its name as a path yields `FROM DISK =
N'NightlyFull'`, which SQL Server reads as a *file* of that name in the
server's default backup directory.

### SQL Server Agent

Everything under SSMS's SQL Server Agent node that a SQL-only client can
reach: jobs and their steps, shared schedules, alerts, operators, and the
categories they're filed under. WMI alerts and performance-condition
alerts are visible but not manageable — see
[Features intentionally excluded](#features-intentionally-excluded-require-wmi--com--os-apis).

```go
// Is Agent even running? (Reported, not inferred from a failed call.)
status, _ := srv.AgentInfo()
fmt.Println(status.Running, status.StatusText, status.LastStartupTime)
```

On an Azure engine edition there is no Windows service to report and
`sys.dm_server_services` is empty, so the read falls back to two SQL-visible
facts that keep the no-WMI contract: a session under `program_name LIKE
N'SQLAgent%'` means Agent is up right now, and `msdb.dbo.syssessions`'
newest `agent_start_date` is its last startup. A login without `VIEW SERVER
STATE` sees no sessions and reads as stopped, which is why the startup time
is still reported alongside.

`srv.Job(name)`, `srv.Alert(name)`, `srv.Operator(name)` and
`srv.Schedule(name)` return a no-I/O handle carrying only the name — the
Agent counterparts of `srv.Database`/`srv.Login`. Every write method on
those types addresses its object by name, so a handle is enough to keep
operating on one you already know exists; the `...ByName` form is what
queries `msdb` and populates the cached fields. Under `WithScript` the
handle is the only usable form, and is what `CreateJob`/`CreateAlert`/
`CreateOperator`/`CreateSchedule` return there.

#### Jobs and steps

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
job.SetEmailNotify("DBA on call", gosmo.NotifyOnFailure)
job.Start("")

// Edit or remove a step in place.
steps, _ := job.Steps()
steps[0].Update(gosmo.JobStepRequest{ /* ... */ })
steps[0].Delete()

// Reorder them: insert at a position, move one step, or reorder the lot.
job.InsertStep(gosmo.JobStepRequest{ /* ... */ }, 2)
job.MoveStep(3, 1)
job.ReorderSteps(func(n int) []int { return []int{3, 1, 2} })

// History, per job or across every job at once.
entries, _ := job.History(50)
recent, _ := srv.JobHistory(200)
```

`JobStep.LastRunDate` is when the step last ran (zero — test with `IsZero()` —
for a step that never has), and `LastRunElapsed` is `LastRunDuration` decoded:
the raw field is msdb's `HHMMSS` integer, so `10230` is 1h 02m 30s, not 10230
seconds. Display code should use `LastRunElapsed`.

msdb has no procedure that renumbers a step in place, so a move is a delete
followed by an insert at the target position — which is why `JobStep` carries
the step's whole definition (proxy, additional parameters, CmdExec success
code, target server, run-as user, OS priority) and not just the fields a UI
shows: all of it has to survive the round trip. `sp_add_jobstep` renumbers
every later step *and* follows their "go to step N" references, but
`sp_delete_jobstep` clears such a reference instead of following it, so
`ReorderSteps` rewrites every reference itself afterwards, through
`JobStep.SetFlow`. `ReorderSteps` needs a job read with `JobByName` — the step
listing is by `job_id`, which a bare `srv.Job(name)` handle does not carry.

The whole reorder goes to the server as **one transactional batch**, so a job
is either in the requested order or in the order it started in, and never in
the state between a step's delete and its re-insert — where the step exists
nowhere but in gosmo's memory. The step listing that decides the order is
read outside that transaction, so a concurrent edit of the same job is still
last-writer-wins: the batch makes the reorder atomic, not serializable.

`Job.CurrentState` is Agent's own `job_state`, read from
`xp_sqlagent_enum_jobs` — the value SSMS's Job Activity Monitor displays.
Agent keeps it in memory and msdb has no column for it, so a job the read
does not cover falls back to a `sysjobactivity`-derived value where only
`JobStateExecuting` and `JobStateIdle` can be told apart. That happens with
Agent stopped, and for a login with neither `sysadmin` nor
`SQLAgentReaderRole`; a job listing has to survive both rather than fail.
`JobStateUnknown` is what a multi-server job Agent does not run itself
reports.

`JobStepRequest`'s two string fields read an empty value differently, because
msdb does: an empty `Database` means "leave the step's database alone"
(`sp_update_jobstep` accepts `N''` for `@database_name` and changes nothing),
while an empty `OutputFileName` is sent and *does* clear the step's output
file. There is no way to null the step's database through msdb at all.

#### Shared schedules

A schedule is an object in its own right, shared by any number of jobs —
`Job.AddSchedule` creates one and attaches it in a single step, while
`AttachSchedule`/`DetachSchedule` wire up (or unwire) one that already
exists without creating or deleting it.

```go
sched, _ := srv.CreateSchedule(gosmo.CreateScheduleRequest{
    Name:            "Weeknights at 2am",
    Enabled:         true,
    FreqType:        gosmo.FreqWeekly,
    FreqInterval:    gosmo.WeekdayMonday | gosmo.WeekdayTuesday | gosmo.WeekdayWednesday |
                     gosmo.WeekdayThursday | gosmo.WeekdayFriday,
    FreqSubdayType:  gosmo.SubdayOnce,
    ActiveStartTime: 20000, // HHMMSS — 02:00:00
})

job.AttachSchedule(sched.Name)
// "Occurs every week on Monday, Tuesday, Wednesday, Thursday, Friday at
// 02:00:00. Schedule is active from 2026-07-28."
fmt.Println(sched.Description())

jobs, _ := sched.Jobs() // which jobs this schedule drives
```

#### Alerts and operators

```go
op, _ := srv.CreateOperator(gosmo.CreateOperatorRequest{
    Name:         "DBA on call",
    Enabled:      true,
    EmailAddress: "dba@example.com",
})

alert, _ := srv.CreateAlert(gosmo.CreateAlertRequest{
    Name:     "Severity 17+",
    Enabled:  true,
    Severity: 17,
})
alert.Notify(op.Name, gosmo.NotifyMethodEmail)
alert.SetJobResponse("NightlyBackup") // run a job in response

// The "referenced by" direction, for an operator's properties page.
alerts, _ := op.NotifyingAlerts()
notified, _ := op.NotifyingJobs()

// Only the alerts gosmo can fully manage (no WMI, no perf counters).
manageable, _ := srv.EventAlerts()
```

#### Categories

```go
cats, _ := srv.Categories(gosmo.CategoryClassJob)
srv.CreateCategory(gosmo.CategoryClassAlert, "Storage")
srv.DeleteCategory(gosmo.CategoryClassAlert, "Storage")
```

### Always On availability groups

The whole SSMS Always On node — the group, its replicas, the per-database
synchronization state, and the listeners clients connect through.

| SSMS equivalent                   | gosmo                                                        |
| --------------------------------- | ------------------------------------------------------------ |
| Availability Groups node          | `srv.AvailabilityGroups()` / `srv.AvailabilityGroup(name)` (no-I/O handle) / `srv.AvailabilityGroupByName(name)` |
| Availability Replicas node        | `ag.Replicas()` → `[]*AvailabilityReplica`                    |
| Availability Databases node       | `ag.Databases()` → `[]*AvailabilityDatabase` (queue sizes, rates, `SecondaryLagSeconds`, last sent/received/hardened/redone/commit times) |
| Availability Group Listeners node | `ag.Listeners()` → `[]*AvailabilityGroupListener` (with their IP configurations) |
| Group properties                  | `ag.SetAutomatedBackupPreference(p)` / `SetFailureConditionLevel(n)` / `SetHealthCheckTimeout(ms)` / `SetDBFailover(on)` / `SetDTCSupport(perDB)` / `SetRequiredSynchronizedSecondariesToCommit(n)` |
| Replica properties                | `r.SetAvailabilityMode(m)` / `SetFailoverMode(m)` / `SetSeedingMode(m)` / `SetSessionTimeout(s)` / `SetBackupPriority(n)` / `SetPrimaryRoleAllowConnections(m)` / `SetSecondaryRoleAllowConnections(m)` |
| Read-only routing                 | `r.SetReadOnlyRoutingURL(url)` / `r.SetReadOnlyRoutingList(list)` / `r.ReadOnlyRoutingList()` |
| Add / remove a replica            | `ag.AddReplica(spec)` / `ag.RemoveReplica(serverName)` / `r.Drop()` |
| Add / remove a database           | `ag.AddDatabase(name)` / `ag.RemoveDatabase(name)`            |
| Join / unjoin on a secondary      | `ag.JoinDatabase(name)` / `ag.UnjoinDatabase(name)`           |
| Suspend / resume data movement    | `ag.SuspendDatabase(name)` / `ag.ResumeDatabase(name)`        |
| Listeners                         | `ag.AddListener(spec)` / `ag.AddListenerIP(dns, ip)` / `ag.SetListenerPort(dns, port)` / `ag.RemoveListener(dns)` |
| New Availability Group wizard     | `srv.CreateAvailabilityGroup(req)` / `ag.Join(clusterType)` / `ag.GrantCreateAnyDatabase()` / `ag.DenyCreateAnyDatabase()` |
| Failover / forced failover        | `ag.Failover()` / `ag.ForceFailoverAllowDataLoss()`           |
| Drop                              | `ag.Drop()`                                                   |

**Read from the primary when the answer has to be complete.**
`sys.availability_groups` and `sys.availability_replicas` are cluster-wide
metadata and agree on every replica, but the `sys.dm_hadr_*` DMVs describe
only what *this* instance can currently see — most visibly, per-database
queue sizes and commit times are populated only for databases the local
instance actually hosts. `ag.PrimaryReplicaServerName` and
`ag.IsLocalPrimary()` are there so a caller can tell where it is and follow
the primary; an empty `PrimaryReplicaServerName` means "unknown from here",
not "no primary exists".

Setting a group up needs two things below it, both also new here: every
instance must have a **database mirroring endpoint** started with `CONNECT`
granted to the other instances' service accounts, and — for certificate
authentication — each instance needs the others' public **certificates**.

### Certificates and the database master key

| SSMS equivalent                  | gosmo                                                    |
| -------------------------------- | -------------------------------------------------------- |
| Security → Certificates          | `db.Certificates()` / `db.CertificateByName(name)`        |
| Security → Asymmetric Keys       | `db.AsymmetricKeys()` / `db.AsymmetricKeyByName(name)`    |
| New / drop certificate           | `db.CreateCertificate(gosmo.CertificateSpec{...})` / `cert.Drop()` |
| Database master key              | `db.HasMasterKey()` / `db.CreateMasterKey(password)`      |
| Export the public certificate    | `cert.Encoded()` → `[]byte` (`CERTENCODED`)               |
| Import it on another instance    | `CertificateSpec.FromBinary` (`CREATE CERTIFICATE ... FROM BINARY`, SQL Server 2022+) |

`Encoded` and `FromBinary` are the pair that moves a certificate between
instances **without filesystem access on either host**. The documented route
is `BACKUP CERTIFICATE` to a file, copy the file, `CREATE CERTIFICATE FROM
FILE` — which a client library cannot do. Only the ASN.1-encoded *public*
certificate crosses the wire, which is what makes this safe over an ordinary
connection, and it is enough for database mirroring endpoints, where each
instance keeps its own key pair and holds only its peers' public
certificates.

`CertificateByName` reports a certificate that isn't there as `(nil, nil)`,
not an error — its callers branch on absence as the ordinary case. See
[Errors](#errors) for the package's three not-found conventions.

### Database mirroring endpoints

| SSMS equivalent                | gosmo                                                     |
| ------------------------------ | --------------------------------------------------------- |
| Server Objects → Endpoints     | `srv.DatabaseMirroringEndpoint()` → `*DatabaseMirroringEndpoint` |
| New endpoint                   | `srv.CreateDatabaseMirroringEndpoint(gosmo.EndpointSpec{...})` |
| Start / stop / drop            | `e.Start()` / `e.Stop()` / `e.Drop()`                      |
| Grant CONNECT to a peer's login | `e.GrantConnect(login)`                                   |
| The `TCP://host:port` form     | `e.URL()`                                                  |

An instance can have **at most one** database mirroring endpoint, whatever
it is called and however many availability groups use it — a server rule,
not a convention. A second availability group over the same pair of
instances reuses the first one's endpoint and port, so code that sets a
group up should read the endpoint before considering creating one. An
endpoint left `STOPPED` is the usual reason a replica that looks correctly
configured never synchronizes.

### Endpoints

`DatabaseMirroringEndpoint` above is one endpoint of one kind. `Endpoint` is
any row of `sys.endpoints`, whatever its protocol and payload — SSMS's Server
Objects → Endpoints folder in full.

| SSMS equivalent                 | gosmo                                                     |
| ------------------------------- | --------------------------------------------------------- |
| Server Objects → Endpoints      | `srv.Endpoints()` / `srv.EndpointByName(name)` / `srv.EndpointSeq(ctx)` |
| Start / stop / disable          | `ep.SetState(gosmo.EndpointStarted \| gosmo.EndpointStopped \| gosmo.EndpointDisabled)` |
| Drop                            | `ep.Drop()`                                                |
| Mirroring detail                | `ep.MirroringDetail()` → `*DatabaseMirroringEndpoint`      |
| Service Broker detail           | `ep.ServiceBrokerDetail()` → `*ServiceBrokerEndpointDetail` |

Type-specific detail is read on demand rather than in the listing, so
enumerating every endpoint costs one query rather than three.

The five built-in endpoints — the Dedicated Admin Connection, TSQL Local
Machine, TSQL Named Pipes, TSQL Default TCP and TSQL Default VIA, all with
`endpoint_id` below 65536 — cannot be altered or dropped. `SetState` and
`Drop` return `ErrSystemEndpoint` for them, because SQL Server's own refusal
names neither the endpoint nor the reason. There is deliberately no
`srv.Endpoint(name)` no-I/O handle for this family: `IsSystem` is derived
from the scanned id, so such a handle would carry id 0 and refuse every write
on itself.

### Audits and audit specifications

SSMS's Security → Audits and Security → Server Audit Specifications. An audit
is the destination — a file, the Windows Application log or the Security log
— and a specification names the action groups written to it.

| SSMS equivalent                | gosmo                                                      |
| ------------------------------ | ---------------------------------------------------------- |
| Security → Audits              | `srv.ServerAudits()` / `srv.ServerAuditByName(name)` / `srv.ServerAudit(name)` (no-I/O handle) |
| New audit                      | `srv.CreateServerAudit(gosmo.ServerAuditSpec{...})`         |
| Alter / rename / drop          | `a.Alter(spec)` / `a.Rename(newName)` / `a.Drop()`          |
| Enable / disable               | `a.SetState(true \| false)`                                 |
| Is it running, and to which file | `a.Status()` → `*ServerAuditStatus`                       |
| Security → Server Audit Specifications | `srv.ServerAuditSpecifications()` / `...ByName(name)` / `srv.ServerAuditSpecification(name)` |
| New specification              | `srv.CreateServerAuditSpecification(gosmo.ServerAuditSpecificationSpec{...})` |
| Add / drop action groups       | `spec.AddActionGroups(g...)` / `spec.DropActionGroups(g...)` |
| Point it at another audit      | `spec.SetAudit(auditName)`                                  |
| Enable / disable / drop        | `spec.SetState(on)` / `spec.Drop()`                         |
| What can be audited            | `srv.AuditActionGroups()`                                   |
| Database → Security → Database Audit Specifications | `db.DatabaseAuditSpecifications()` / `...ByName(name)` / `db.DatabaseAuditSpecification(name)` |
| New database specification     | `db.CreateDatabaseAuditSpecification(gosmo.DatabaseAuditSpecificationSpec{...})` |
| Add / drop groups and actions  | `spec.AddActions(groups, actions)` / `spec.DropActions(groups, actions)` |
| What can be audited in a database | `srv.DatabaseAuditActionGroups()` / `srv.DatabaseAuditActions()` |

**Every write but the state toggle needs the object disabled**, and both
types handle that themselves: SQL Server refuses `ALTER` and `DROP` on an
enabled audit ("This command requires audit to be disabled") and on an
enabled specification. Each write disables, applies, re-enables — and only if
it was the one that disabled — and restores the state on the failure path
too. `a.WithDisabled(ctx, fn)` and `spec.WithDisabled(ctx, fn)` expose that
for a caller making several changes at once.

Two details do not follow from the catalog. The constants
(`AuditToApplicationLog`, `AuditFailureShutdown`, …) are `type_desc` values,
not T-SQL keywords — `APPLICATION LOG` is written `TO APPLICATION_LOG` and
`SHUTDOWN SERVER INSTANCE` as `ON_FAILURE = SHUTDOWN` — so never build a
statement out of one. And the two size limits disagree on their sentinel:
`MaxFileSize` uses 0 for UNLIMITED, `MaxRolloverFiles` uses
`gosmo.AuditUnlimited`.

`ServerAuditSpecification.AuditName` is empty for an orphaned specification:
dropping an audit a specification still references succeeds and leaves the
`audit_guid` pointing at nothing. The same holds for
`DatabaseAuditSpecification.AuditName`.

A **database** audit specification is not the server one with a different
keyword. It records individual actions on securables as well as action
groups, so a clause is either `ADD (SCHEMA_OBJECT_ACCESS_GROUP)` or
`ADD (SELECT ON OBJECT::[dbo].[T] BY [public])` — hence
`DatabaseAuditAction` and the paired `AddActions`/`DropActions` in place of
the server half's group-only `AddActionGroups`/`DropActionGroups`. The two
halves of an action clause quote in opposite ways: the action name and the
securable class are keywords and are charset-checked, the securable and the
principal are identifiers and are bracket-quoted.

### Credentials

SSMS's Security → Credentials, and the identity a login can be mapped to.

| SSMS equivalent           | gosmo                                                |
| ------------------------- | ---------------------------------------------------- |
| Security → Credentials    | `srv.Credentials()` / `srv.CredentialByName(name)` / `srv.Credential(name)` (no-I/O handle) |
| New credential            | `srv.CreateCredential(gosmo.CredentialSpec{Name, Identity, Secret, CryptographicProvider})` |
| Change identity or secret | `cred.Alter(identity, secret)` — `secret` is a `*string`, and nil **clears** the stored secret: `ALTER CREDENTIAL` resets both halves, so there is no form that changes the identity and keeps the secret |
| Drop                      | `cred.Drop()`                                        |
| Cryptographic providers   | `srv.CryptographicProviders()`                       |

**The secret is write-only.** `sys.credentials` never exposes it and there is
no read that does, which is why `Alter` takes a pointer rather than a string
and why a scripted credential carries a `<insert secret here>` placeholder
instead of a value it cannot know.

Database-scoped credentials are a separate securable with their own DDL, and
live on `*Database` rather than `*Server` — SSMS's *database* → Security →
Database Scoped Credentials. There is no `FOR CRYPTOGRAPHIC PROVIDER` form of
one, so `DatabaseScopedCredentialSpec` has no provider field.

| SSMS equivalent                       | gosmo                                          |
| ------------------------------------- | ---------------------------------------------- |
| *db* → Security → Database Scoped Credentials | `db.DatabaseScopedCredentials()` / `db.DatabaseScopedCredentialByName(name)` / `db.DatabaseScopedCredential(name)` (no-I/O handle) |
| New database scoped credential        | `db.CreateDatabaseScopedCredential(gosmo.DatabaseScopedCredentialSpec{Name, Identity, Secret})` |
| Change identity or secret             | `dsc.Alter(identity, secret)` — same `*string`, same meaning |
| Drop                                  | `dsc.Drop()`                                   |

### Server triggers

Server-scope DDL and LOGON triggers — SSMS's Server Objects → Triggers.

| SSMS equivalent            | gosmo                                              |
| -------------------------- | -------------------------------------------------- |
| Server Objects → Triggers  | `srv.ServerTriggers()` / `srv.ServerTriggerByName(name)` / `srv.ServerTrigger(name)` |
| Enable / disable / drop    | `tr.Enable()` / `tr.Disable()` / `tr.Drop()`        |

A different family from `db.Triggers()`, which reads DML triggers on a table.
A trigger declared `FOR` a whole event group lists that group's individual
events in `Events`, which is what the catalog records. `Definition` is empty
for an encrypted trigger and for a CLR one, which has no row in
`sys.server_sql_modules` at all.

### Database DDL triggers

The database-scope half of the same family — SSMS's *db* → Programmability →
Database Triggers. A third family from the two above: `db.Triggers()` reads
DML triggers on a table (`parent_class = 1`), `srv.ServerTriggers()` reads the
server-scope ones (`parent_class = 100`), and these are `parent_class = 0`.

| SSMS equivalent                        | gosmo                                        |
| -------------------------------------- | -------------------------------------------- |
| *db* → Programmability → Database Triggers | `db.DatabaseTriggers()` / `db.DatabaseTriggerByName(name)` / `db.DatabaseTrigger(name)` (no-I/O handle) |
| Enable / disable / drop                | `tr.Enable()` / `tr.Disable()` / `tr.Drop()` |
| Script one                             | `sc.ScriptDatabaseTrigger(name)`             |

The scope keyword differs from the server family's and is not
interchangeable: `ENABLE`/`DISABLE`/`DROP TRIGGER ... ON DATABASE`, not `ON
ALL SERVER`. `Events` lists a declared event group's individual events, as the
catalog records them, and `Definition` is empty for an encrypted or CLR
trigger.

`db.ObjectTriggers(schema, name)` belongs to the DML family rather than this
one: it is `Table.Triggers` addressed by name, and the reader a view's INSTEAD
OF triggers previously had none of, `View` being a plain row struct with no
back-pointer to its database.

### Error log

| SSMS equivalent                  | gosmo                                        |
| -------------------------------- | -------------------------------------------- |
| Management → SQL Server Logs     | `srv.EnumErrorLogs(gosmo.ErrorLogSQLServer)` → `[]*ErrorLogFile` |
| Agent → Error Logs               | `srv.EnumErrorLogs(gosmo.ErrorLogAgent)`      |
| Open a log                       | `srv.ReadLog(logType, n)` → `[]*ErrorLogEntry` |
| ... filtered at the server        | `srv.ReadLogFiltered(logType, n, gosmo.LogSearch{Text1: ..., From: ..., To: ...})` |
| ... the SQL Server log, shorthand | `srv.ReadErrorLog(n)`                        |
| Recycle the log                  | `srv.CycleLog(logType)` (`srv.CycleErrorLog()` is the SQL Server-log shorthand) |

`ErrorLogType` is the log-type argument `xp_readerrorlog` and
`sp_enumerrorlogs` themselves take, so it passes straight through — which is
why the Agent log is readable through the same two methods rather than a
second pair of them. Log number 0 is the current log, 1 the most recent
archive, and so on.

`LogSearch` is `xp_readerrorlog`'s own arguments 3-6, with its semantics:
`Text1` and `Text2` are case-insensitive substrings **AND-ed together**, not
two alternatives, and `From`/`To` bound the entry timestamp. Filtering at the
server rather than in the caller is what makes the current log usable on a
busy instance, where it runs to tens of thousands of entries. A zero
`LogSearch` reads the whole file, which is what `ReadLog` does.

### Server filesystem

SMO's `Server.EnumDirectories`/`EnumFiles`, and the fixed-drive list behind
SSMS's file-browse dialogs.

| SSMS equivalent               | gosmo                                       |
| ----------------------------- | ------------------------------------------- |
| Browse a server-side folder   | `srv.EnumFileSystem(path)` → `[]*FileSystemEntry` |
| Drive list in a browse dialog | `srv.FixedDrives()` → `[]*FixedDrive`        |
| Does this path exist?         | `srv.FileSystemExists(path)` → `(exists, isDirectory bool, err error)` |

Every path here is interpreted by the **server**, not by the process calling
gosmo — routinely two different machines with different path conventions,
which is the whole reason these exist rather than a caller using
`os.ReadDir`. Each reads `sys.dm_os_enumerate_filesystem` /
`sys.dm_os_enumerate_fixed_drives` on SQL Server 2017 and later, and falls
back to `xp_dirtree` / `xp_fixeddrives` otherwise. The fallback reports no
`Size` and no `LastModified`; an instance whose version gosmo has not
established takes it, since `xp_dirtree` exists everywhere and the DMV does
not.

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

Every error is wrapped `gosmo: <what was being attempted>: %w`, including
the ones raised partway through reading a result set — so a cancellation
mid-scan names the operation it interrupted rather than arriving as a bare
`context deadline exceeded`.

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

### `ErrNotFound`

Every by-name lookup that reports absence as an error wraps `ErrNotFound`,
so "this object does not exist" is testable without matching on message
text:

```go
db, err := srv.DatabaseByName("Sales")
switch {
case errors.Is(err, gosmo.ErrNotFound):
    // create it
case err != nil:
    return err // permission, connection, timeout — not absence
}
```

The distinction matters: a caller that reads *any* error as absence goes on
to create an object it never established was missing, and then reports the
creation's failure instead of the permission or connection error that
actually stopped it.

Three conventions coexist, deliberately:

- Most by-name lookups — `LoginByName`, `DatabaseByName`, `TableByName`,
  `UserByName`, `RoleByName`, `AgentJobByName`, `AlertByName`,
  `OperatorByName`, `ScheduleByName`, `ServerRoleByName`,
  `ConfigurationByName`, `AvailabilityGroupByName`, and the Scripter's
  view/procedure/function lookups — return an error wrapping `ErrNotFound`.
- `CertificateByName` returns `(nil, nil)`, because its callers branch on
  absence as the ordinary case rather than the exceptional one.
- `AgentStatus` reports an unreachable Agent as a populated value
  (`StatusText` "Unknown"), not an error.

`AvailabilityGroupByName`'s not-found error additionally still satisfies
`errors.Is(err, sql.ErrNoRows)`, which it promised before `ErrNotFound`
existed.

A `Drop` method does **not** use `IF EXISTS`: dropping something that isn't
there comes back as the server's own "Cannot drop ... because it does not
exist", uniformly across every object family. A caller that wants the
idempotent form ignores the error — a decision it can make and this package
cannot make for it. The DDL that `Scripter` *generates* does keep
`IF EXISTS`, since that output exists to be re-run.

### `ErrUnsupportedVersion`

A call gosmo refuses because the connected instance is older than the
feature it names wraps `ErrUnsupportedVersion` — decided here, before any
statement is sent, because the server's own answer would be a parse error
naming syntax the caller never wrote
(`CreateColumnMasterKeyWithSignature` below SQL Server 2019 is the standing
case). It is only for that: a missing *column* is gated instead, so the read
returns a zero value rather than an error. Where a caller can ask in
advance, it should — `db.EnclaveComputationsSupported()`,
`db.QueryStoreWaitStatsSupported()`, `db.QueryStoreMetrics()` — and hide the
option rather than let it fail on submit.

---

## Authentication

`ConnectionOptions.Auth` selects the authentication method:

| Constant                               | When to use                                        | Fields / notes |
| -------------------------------------- | -------------------------------------------------- | -------------- |
| `AuthSQLServer` (default)              | SQL Server login + password                        | `User`, `Password` |
| `AuthWindows`                          | Windows / Kerberos (domain-joined host)            | see below |
| `AuthEntraMSI`                         | Azure Managed Identity (system- or user-assigned)  | `ClientID` for user-assigned |
| `AuthEntraServicePrincipal`            | Service principal with secret or certificate       | `User` = client ID, `TenantID`, `Password` or `ClientCertPath` |
| `AuthEntraServicePrincipalAccessToken` | A bearer token you already hold                    | `AccessToken`; prefer `AccessTokenProvider` |
| `AuthEntraPassword`                    | Entra ID user + password (non-interactive)         | `User`, `Password`, optional `ApplicationClientID`. No MFA; deprecated by azidentity |
| `AuthEntraInteractive`                 | Browser-based interactive login                    | optional `User` (login hint), `ApplicationClientID` |
| `AuthEntraDeviceCode`                  | Device code flow                                   | optional `ApplicationClientID`; the code goes to `DeviceCodePrompt`, else stdout |
| `AuthEntraDefault`                     | Default credential chain (env → MSI → AzCLI)       | — |
| `AuthEntraIntegrated`                  | Same chain as `AuthEntraDefault`                   | not Windows SSO: go-mssqldb has no such credential |
| `AuthEntraAzCLI`                       | `az login` credential                              | — |
| `AuthEntraAzureDeveloperCLI`           | `azd auth login` credential                        | — |
| `AuthEntraAzurePipelines`              | Azure DevOps pipeline OIDC                         | `User` = client ID, `TenantID` (or `AZURESUBSCRIPTION_*` env); `serviceconnectionid` / `systemtoken` via `ExtraParams` or env |
| `AuthEntraOnBehalfOf`                  | Middle-tier on-behalf-of exchange                  | `User` = client ID, `AccessToken` = user assertion, `Password` or `ClientCertPath` |

Required fields are checked before anything is dialled, and the error names
the gosmo field (`gosmo: AuthEntraServicePrincipal requires User (the
application's client ID)`); `AuthMethod.String()` gives the constant's name. `TenantID` is
honoured by every Entra method except `AuthEntraMSI` and
`AuthEntraServicePrincipalAccessToken`. Left empty, service principal,
password, on-behalf-of, Azure Pipelines, interactive and device code sign in
to the tenant the server announces at login, as SSMS does — not
azidentity's `organizations` default, which refuses a personal Microsoft
account that is a member of the server's tenant.

gosmo builds the Entra credential itself rather than leaving it to
go-mssqldb, which would construct a new one for every physical connection —
a browser sign-in, or a new device code, per pooled connection. Each
credential lives in an `EntraCache` keyed by identity (method, tenant,
client and application IDs, user or login hint, certificate path,
authority, and a digest of the secrets — never the server), and its tokens
are reused until five minutes before they expire, so a pool opening twenty
connections at once signs in once. Nothing is written to disk.

- **`ConnectionOptions.EntraCache`** — share one `NewEntraCache()` across
  every `Connect` that should share a sign-in, typically one per process.
  Nil gives each `Server` a private cache: its own connections share a
  sign-in, other `Server`s' do not. `Clear()` forgets every credential and
  token, for switching accounts.
- **`EntraCache.Warm(ctx, opts)`** signs in before dialling, so a human
  sign-in runs under a context of the caller's choosing (long, cancellable)
  instead of the connect timeout inside the TDS login handshake. The scope,
  authority and tenant are the server's to announce, part-way through a
  login, so `Warm` first opens one and abandons it as soon as the server has
  named them — before any token is sent — and the cache remembers the answer
  per server. The probe is bounded by `ConnectTimeout`, and its failure (an
  unreachable server, one without Entra support) is `Warm`'s error. Azure
  SQL logs each probe as Error 33155. `Warm` is a no-op for non-Entra
  methods, `AccessTokenProvider` and `AuthEntraServicePrincipalAccessToken`.
- **`ConnectionOptions.DeviceCodePrompt`** receives a `DeviceCodeMessage`
  (code, URL and Microsoft's one-line instruction) in place of azidentity's
  default of printing to stdout, which a terminal UI, GUI or service cannot
  show. A credential shared through an `EntraCache` uses the prompt of the
  most recent connection that set one, so pass one process-wide prompt.

Every Entra method is checked against go-mssqldb's own parser in the test
suite. Verified end to end against Azure SQL Managed Instance, with
`TenantID` blank: `AuthEntraInteractive`, `AuthEntraDeviceCode`,
`AuthEntraPassword`, `AuthEntraServicePrincipal` (client secret),
`AuthEntraAzCLI` and `AuthEntraDefault` (reaching the Azure CLI) — each
signing in once for many pooled connections. The other methods have not yet
been driven against a live tenant.

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

`ConnectionOptions.Dialer` replaces the dialer used for every network
operation — the TDS connection and the SQL Server Browser probe alike —
for routing through a proxy or an SSH tunnel, or for controlling address
selection. Left nil, gosmo uses the driver's own dialer, except when
`Server` names an instance with no port: that connection needs a Browser
probe (a single UDP datagram to port 1434), and gosmo substitutes a
dialer that sends it to *every* resolved address and takes the first
reply. On a dual-stack host whose Browser answers on only one family, the
driver's own dialer probes whichever address the resolver returned first
and, half the time, reports the timeout as `no instance matching
'<name>'` — a network fault that reads like a misspelled instance.

`gosmo.ParseServerAddress(server)` parses any address form SSMS's own
"Server name" field accepts — `host`, `host:port`, `host,port`,
`host\instance`, `host\instance,port` — into `(host, instance, port)`.
Exported so a caller building its own connection-address UI can reuse the
same parsing `Connect`/`ConnectContext` rely on internally.

---

## Connection helpers (internal)

A `Database`-scoped call has to run `USE <db>` on the same connection as
its statement, so it can't use the pool directly — it pins a `*sql.Conn`
for the duration. `Server`-scoped calls have no `USE` to redo and go
straight to the pool.

| Helper                 | Purpose                                                                                     |
| ---------------------- | ------------------------------------------------------------------------------------------- |
| `Database.withConn`    | Acquires a `*sql.Conn` and runs `USE <db>` (retried), then hands it to a callback (not retried — the callback is the caller's write), releasing the conn on return. |
| `Database.query`       | Returns `*dbRows`, whose `Close()` closes the rows **and** the conn pinned for them. `*sql.Rows.Close` alone would leak that conn out of the pool permanently. |
| `Database.queryRow`    | Takes a `func(*sql.Row) error` scan callback and runs acquire + `USE` + scan as one retried unit. The scan has to be inside it: `QueryRowContext` never returns an error, so a scan run afterwards would never be retried. |
| `Database.exec`        | Thin wrapper over `withConn` for non-SELECT statements; also where `WithScript` intercepts database-scoped writes. |
| `Server.query`         | Server-scoped rows-returning read, retried — no `USE`, so a plain `*sql.Rows` is enough.    |
| `Server.queryRow`      | Server-scoped single-row read, same scan-callback shape and reason as `Database.queryRow`.   |
| `Server.queryRowScan`  | `queryRow` convenience for a plain `row.Scan(dest...)`, sparing the caller a closure.        |
| `withRetry`            | Retries each of the read helpers above up to 3 times (linear backoff) on a transient/dropped-connection failure — reads only, since retrying is only safe when the operation is idempotent. |

`gosmo.IsRetryable(err)` exposes the same transient-failure test
`withRetry` uses, for callers running their own statements outside
gosmo's query helpers.

---

## Running the examples

```
export MSSQL_SERVER="localhost:1433"
export MSSQL_USER="sa"
export MSSQL_PASSWORD="YourPassword"
export MSSQL_TRUST_CERT="true"     # self-signed dev cert

go run ./examples                  # guided tour of the whole library
```

Eight more programs go deeper on one subject each:

| Program | Covers |
| --- | --- |
| `go run ./examples/backup` | `BACKUP`/`RESTORE`, backup headers and history, progress callbacks, relocating files |
| `go run ./examples/bulkcopy` | `BulkInsert` from a slice, a generator, and a streaming CSV |
| `go run ./examples/diagnostic` | `AsSQLError`, `IsRetryable`, `ExecProc`, execution plans, search, dependencies, DMV reads |
| `go run ./examples/iterators` | The `*Seq` API and what its deferred-fetch semantics do and don't buy you |
| `go run ./examples/jobs` | SQL Server Agent jobs, steps, schedules, operators, alerts |
| `go run ./examples/maintain` | Files, fragmentation, index rebuilds, statistics, Query Store, change tracking |
| `go run ./examples/scripting` | The `Scripter`, and `WithScript`'s collect-instead-of-execute mode |
| `go run ./examples/security` | Logins, users, roles, and permissions from both directions |

Each creates its own throwaway database and drops it afterwards; nothing
already on the instance is modified. Authentication and the full environment
variable list are documented in [`examples/README.md`](examples/README.md).

---

## Features intentionally excluded (require WMI / COM / OS APIs)

- Hardware enumeration (disk, NIC, CPU details beyond what `sys.dm_os_sys_info` provides)
- SQL Server service start/stop/restart
- Performance counters via Windows PDH
- SQL Server Browser service interaction — enumerating instances, reading
  its configuration. The dual-stack Browser *dialer* is not an exception: it
  fixes how the driver's own port lookup reaches the service, and asks it
  nothing gosmo does not already need in order to connect.
- Windows Event Log reading
- Registry reads through a Windows API. `xp_instance_regread`, which the
  server itself runs, is not one — it is how `Info().DefaultBackupPath` is
  recovered on instances where `SERVERPROPERTY` does not report it — but
  nothing here opens a registry from the client side.
- WMI and performance-condition SQL Server Agent alerts — these are listed
  by `srv.Alerts()` but not creatable or editable, since they depend on a
  WMI provider or Windows performance counters. `Alert.IsEventAlert()` and
  `srv.EventAlerts()` identify the manageable subset.
- Multi-server Agent administration (master/target servers) — jobs are
  created as `LOCAL`, enlisted on `(local)`

All of the above require WMI or Windows-only APIs and are out of scope for a cross-platform Go library.

---

## Contributing

The codebase is currently unstable and going through regular refactoring,
so I'm not accepting pull requests at this time — please open an issue
instead. I'll start accepting PRs once the project reaches a released,
more stable state. In the near future I'm planning to update the project
regularly.


