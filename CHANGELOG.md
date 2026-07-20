# Changelog

All notable changes to gosmo are documented here, newest first. This file
starts tracking detail from `v0.0.4` onward; `RELEASE.md` covers the
high-level shape of every release, including the ones before this file
existed.

## v0.0.5

### Added

- **Database catalog snapshot** (`catalog.go`): `Database.Catalog`/`CatalogContext` bulk-loads every user table/view and its columns in two queries instead of one object at a time — for callers like a SQL editor's autocomplete that need to inventory a whole database up front. `Database.SystemCatalog`/`SystemCatalogContext` does the same for the built-in `sys` schema, reading `sys.all_objects`/`sys.all_columns` (unlike `sys.objects`/`sys.columns`, which despite the generic names never surface `is_ms_shipped` rows). New types: `Catalog`, `CatalogObject`, `CatalogColumn`, `CatalogObjectType` (`CatalogTable`/`CatalogView`).
- **System object folders** (`database.go`): `Database.SystemViews`/`SystemStoredProcedures`/`SystemFunctions` (+ `Context` variants) list the shipped `sys.*` views/procs/functions, same `all_*`-view reasoning as `SystemCatalog` — backs Object Explorer's System Views/Procedures/Functions folders.
- **Query Store** (`query_store.go`): `Database.QueryStore`/`QueryStoreContext` reads `sys.database_query_store_options` into `QueryStoreInfo`; `SetQueryStoreOptions`/Context turns it on (`QueryStoreOptions`) or off via `ALTER DATABASE ... SET QUERY_STORE`; `FlushQueryStore`/Context and `ClearQueryStore`/Context — SSMS's Database Properties > Query Store page.
- **Database Scoped Configuration** (`database_scoped_config.go`): `Database.DatabaseScopedConfigs`/Context reads `sys.database_scoped_configurations` (`DatabaseScopedConfig`; boolean options render as `"0"`/`"1"` text, not `"OFF"`/`"ON"` — verified live); `SetDatabaseScopedConfig`/Context issues `ALTER DATABASE SCOPED CONFIGURATION SET name = value [FOR SECONDARY]`, validated by a new `isSimpleIdentifier` token-shape check (an allowlist would go stale — SQL Server adds new scoped-config options every release) — SSMS's Database Properties > Database Scoped Configuration page.
- **Permissions for a principal, and schema-scoped permissions** (`security.go`): `Database.PermissionsForPrincipal`/Context (new `PrincipalSecurable` type) reports every securable one principal holds a grant on, across database/schema/table/view scope — the inverse of the existing securable-centric `Permissions`; excludes stored-proc/function securables for now. `SchemaPermissions`/Context and `GrantSchemaPermission`/`DenySchemaPermission`/`RevokeSchemaPermission` (+ Context) add GRANT/DENY/REVOKE at schema scope, backed by a new allowlist that includes `EXECUTE` (schema-only, verified live) alongside the object-level set — SSMS's Schema Properties > Permissions and Database Role Properties > Securables pages. `PermissionEntry` gained a `Grantor` field, populated by both.
- **Permission-name catalogs for pickers**: `ObjectPermissionNames()`, `SchemaPermissionNames()`, `DatabasePermissionNames()` (`security.go`) and `ServerPermissionNames()` (`server_security.go`) return every valid permission name at that scope, sorted — for building a Permissions-page dropdown. New `ObjectPermission` constants: `PermAlter`, `PermReferences`, `PermTakeOwnership`, `PermViewChangeTracking`.
- **Backup/restore diagnostics** (`backup.go`): `Server.VerifyBackup`/Context (`RESTORE VERIFYONLY`), `BackupHeaders`/Context (`RESTORE HEADERONLY` → new `BackupHeader`), `BackupFileList`/Context (`RESTORE FILELISTONLY` → new `BackupFile`) — SSMS's Restore Database dialog's backup-set/file picker. Standalone `BuildRestoreStatement(opts) (string, error)` joins the existing `BuildBackupStatement` as a side-effect-free statement builder. **Differential backups**: new `BackupActionDifferential` constant (`BuildBackupStatement` now emits `WITH DIFFERENTIAL`; `BackupHistoryContext` now maps `sys.backupset.type = 'I'` to it). `RestoreOptions.Progress func(pct int, message string)` — restore gets the same live progress-notice callback backup already had; `Stats` defaults to 10 when `Progress` is set and `Stats` is left at 0.
- **Disk volumes and processor info** (`server_config.go`): `Server.DiskVolumes`/Context → `DiskVolumeInfo` (mount point, volume name, sample file path, total/available MB) from `sys.dm_os_volume_stats`. `Server.ProcessorInfo`/Context → `ProcessorInfo` (CPU count, hyperthread ratio, NUMA node count, per-CPU NUMA map) from `sys.dm_os_sys_info`/`sys.dm_os_schedulers` — SSMS's Server Properties > Processors page.
- **Lightweight `Server.Database`/`Server.Login` handles** (`server.go`): return a no-I/O handle (name only) for issuing further calls against an object the caller already knows exists — needed because a `WithScript`-scripted `CreateDatabase`/`CreateLogin` has no real row to look up afterward.
- **Database/role/user single-item lookups and administration** (`database.go`, `schema_user.go`): `UserByName`/Context (includes `SID` and the matching server `LoginName`/`LoginDisabled` — omitted from the tree-listing `Users`); `RoleByName`/Context (`SID`, `CreateDate`, `ModifyDate` — new `DatabaseRole` fields); `DatabaseRole.Rename`/`ChangeOwner` (+ Context); `RoleMembers`/Context (new `RoleMember` type: name + principal type, vs. `DatabaseRole.Members`'s name-only list); `SetUserAccess`/Context (`MULTI_USER`/`SINGLE_USER`/`RESTRICTED_USER` — SSMS Database Properties > Options "Restrict access"); `SetOffline`/`SetOnline` (+ Context — Object Explorer "Take/Bring Database Offline/Online"); `User.Rename`/`SetDefaultSchema`/`SetLogin` (+ Context); `Schema.ObjectCount`/Context (SSMS's Owned Schemas "Object count", loaded lazily).
- **CREATE DATABASE file placement**: `CreateDatabaseOptions.PrimaryFile`/`LogFile *DatabaseFileSpec` render `CREATE DATABASE ... ON PRIMARY (...) LOG ON (...)` — previously `CreateDatabase` could only set a collation and always took the server's default file path/size.
- **Table diagnostics** (`table.go`, `partition.go`): `Table.Detail`/Context → new `TableDetail` (schema owner, lock escalation, ANSI_NULLS, replication/CDC flags, temporal type, memory-optimized durability, ledger type, PK name, data space) — SSMS Table Properties > General "Object details". `Table.SpaceUsed`/Context → new `TableSpaceInfo` (Reserved/Data/Index/LOB/Unused KB, filegroup), mirroring `sp_spaceused` — SSMS Table Properties > Storage page.
- **Server/database info**: `ServerInfo.IsSingleUser`/`EngineEdition` (raw `SERVERPROPERTY('EngineEdition')`); `DatabaseOptions.IsEncrypted` (TDE status); `SpaceInfo.AvailLogMB` (the log-file counterpart of the existing `UnallocatedMB`); `FileGroup.IsReadOnly`; `LoginDetails.BadPasswordTime`; `CompatLevel2025`.
- **`ColumnTypeString(col *Column) string`** (`scripter.go`): the previously-unexported `scriptColType` is now exported, for rendering a column's T-SQL type outside of scripting (e.g. a Table Properties > Columns grid).
- **14 new `*Seq()` iterators** (`iter.go`): `DiskVolumeSeq`, `BackupHeaderSeq`, `BackupFileSeq`, `SystemViewSeq`, `SystemStoredProcedureSeq`, `SystemFunctionSeq`, `SynonymSeq`, `PartitionFunctionSeq`, `PartitionSchemeSeq`, `DatabaseExtendedPropertySeq`, `ColumnMasterKeySeq`, `ColumnEncryptionKeySeq`, `SecurityPolicySeq`, `PartitionSeq` (on `*Table`).
- **`Context` variants added throughout**, closing the last gaps in the `Foo`/`FooContext` pairing convention: every previously context-less write/read method in `security.go` (column master/encryption keys, security policies), `partition.go` (partition functions/schemes, split/merge/drop), `sequence_synonym.go` (sequence/synonym CRUD, `NextValue`), and `extended_properties.go` (`DatabaseExtendedPropertiesContext`/`ExtendedPropertiesContext`).
- `version/version.go`: `Version` is now resolved automatically too, the same way `Commit`/`Date` already were — from `-ldflags -X`, then `debug.BuildInfo` (the main module's own version when gosmo is built standalone, or the consuming module's pinned version when gosmo is embedded as a dependency), and only then the literal `"(devel)"` default. No more hand-edited version string to remember to bump on each release.

### Changed

- **Password handling no longer sends `HASHED` (bug fix).** `CreateLogin`/`ChangePassword`/`ChangePasswordWithOptions` previously encoded the password as a UTF-16LE hex literal sent as `WITH PASSWORD = 0x... HASHED` — `HASHED` tells SQL Server the value is already one of its own password-hash formats, so passing a hex encoding of cleartext under it either failed outright or silently created a login nothing could ever authenticate as. Passwords are now quoted as an `N'...'` string literal via the new `nStringLiteral` helper (same escaping every other string literal in the package uses); `passwordHexLiteral` is gone.
- **`MUST_CHANGE`/`UNLOCK` no longer produce invalid SQL (bug fix).** They're password-clause modifiers that must follow `PASSWORD = '...'` space-separated, not comma-separated `<set_option>` items — the old comma-joined form (`PASSWORD = '...', UNLOCK`) was rejected by SQL Server outright ("Incorrect syntax near 'UNLOCK'"). `MUST_CHANGE` now also adds `CHECK_EXPIRATION = ON`, which SQL Server requires alongside it.
- **`AgentJob`/`JobByName`'s last-run and running-state fields now report real values (bug fix).** `LastRunOutcome`/`LastRunDuration` previously read from `msdb.dbo.sysjobactivity`, which doesn't carry them for a completed run, so they silently stayed at their `ISNULL` defaults; they now read from `msdb.dbo.sysjobservers`. `JobState`'s "is running" flag previously read a `sysjobactivity.job_state` column that doesn't exist (`ISNULL` again always won), and is now computed from `start_execution_date`/`stop_execution_date`. Any caller/UI rendering these fields will show different (correct) data after upgrading.
- `sql.ErrNoRows` comparisons across `login.go`, `server.go`, `server_config.go`, `database.go`, `database_options.go`, `agent_job.go`, `change_tracking.go`, and `scripter.go` now use `errors.Is` instead of `==`, so a wrapped not-found error is still recognized.

### Fixed

- **`CreateColumnMasterKey`'s key name was interpolated unquoted** into `CREATE COLUMN MASTER KEY %s WITH (...)` — a name containing `]`, whitespace, or a reserved word could produce broken or unintended SQL. Now quoted via `quoteIdent`, like every other identifier in the package.
- **`CreatePartitionScheme`'s filegroup names were hand-wrapped as `"[%s]"`** with no escaping of an embedded `]`; now uses `quoteIdent`.
- **Sequence/synonym schema-qualified names were hand-built as `"[%s].[%s]"`** with no escaping, across `Create`/`Drop`/`Restart`/`NextValue` (Sequence) and `Create`/`Drop` (Synonym); now built via the shared `qualifiedName` helper.
- **`Sequence.NextValue` silently discarded a failed query** (`row, _, _ := ...`) and proceeded to scan it anyway; the error is now checked and wrapped.
- **Object-level `GrantPermission`/`DenyPermission`/`RevokePermission` had no permission-name validation.** A new `objectPermissionNames` allowlist (mirroring the database-scoped variant's, which already validated) closes the gap.
- **`databasePermissionNames` dropped `"ADMINISTER DATABASE BULK OPERATIONS"`**, verified live to be rejected at database scope ("not supported... use the server level 'ADMINISTER BULK OPERATIONS' permission", which `serverPermissionNames` already lists) — previously accepted client-side only to fail server-side with a confusing error.
- **`ActiveSessionsContext` could fail to scan a background/system session** with a `NULL` `login_name`/`host_name`/`program_name`; those columns are now `ISNULL`-coalesced.
- **The package doc comment said `Package smo`**, not `Package gosmo` — a real `go doc`-visible mismatch with the actual `package gosmo` clause, now corrected.
- `executionplan.go`: one error message was missing the `"gosmo: "` prefix every other error in the package uses.

### Dependencies

No dependency version changes — `go.mod` only gained a `toolchain go1.26.5` directive; `go.sum` is unchanged.

## v0.0.4

### Added

- **Bulk copy** (`bulkcopy.go`): `Database.BulkInsert`/`BulkInsertContext`
  stream rows into a table over the TDS bulk-copy protocol — the same
  fast path `bcp` and SSMS's "Import Data" use. `BulkCopy`/`BulkOptions`
  describe the destination and `WITH` tuning (constraints, triggers,
  nulls, table lock, batch size, sort order); `SliceRows` adapts an
  in-memory `[][]any` to the `iter.Seq2[[]any, error]` the loader
  consumes, for callers that already hold every row in memory.
- **Stored-procedure execution** (`procedure.go`): `Database.ExecProc`/
  `ExecProcContext` run a procedure as an RPC, so `OUTPUT` parameters and
  the return status come back to the caller. `In`/`Out`/`InOut` build
  `ProcParam` values; `ProcResult.ReturnStatus` carries the status code.
- **Structured SQL errors** (`errors.go`): `AsSQLError` unwraps a driver
  error into `SQLError` (number, class, state, originating
  server/procedure/line, and the full `All` list for a batch that raised
  more than one), with `Header()`/`Error()`/`IsError()` for SSMS-style
  formatting and severity checks — without callers needing to import the
  underlying driver package.
- **Execution plans** (`executionplan.go`): `Database.EstimatedPlan`
  (`SET SHOWPLAN_XML`, statement not run) and `Database.ActualPlan`
  (`SET STATISTICS XML`, statement runs) return the Showplan XML SSMS's
  graphical plan view parses.
- **Object dependencies** (`dependency.go`): `Database.Dependencies`
  (what an object references) and `Database.Dependents` (what references
  it), from `sys.sql_expression_dependencies`.
- **Object search** (`search.go`): `Database.Search(pattern)` finds
  tables/views/procs/functions/triggers by name, matching SSMS's Object
  Explorer Details search box.
- **Object-level and database-scoped permissions** (`security.go`):
  `Database.Permissions`/`GrantPermission`/`DenyPermission`/
  `RevokePermission` for object-level GRANT/DENY/REVOKE, and
  `Database.DatabasePermissions`/`GrantDatabasePermission`/
  `DenyDatabasePermission`/`RevokeDatabasePermission` for database-scoped
  ones (CONNECT, CREATE TABLE, ...). Permission names are allowlisted,
  not interpolated, since GRANT/DENY/REVOKE can't parameterize them.
- **Server security, permissions, and credentials** (`server_security.go`):
  `Server.SecurityInfo` (authentication mode), `Server.ServerPermissions`/
  `GrantServerPermission`/`DenyServerPermission`/`RevokeServerPermission`,
  and `Server.Credentials`.
- **Server memory and languages** (`server_config.go`): `Server.MemoryStats`
  (live figures from `sys.dm_os_sys_memory`/`sys.dm_os_performance_counters`
  — SSMS's Server Properties > Memory "Current values") and
  `Server.Languages` (from `sys.syslanguages`).
- **`Server.CurrentDatabase`/`CurrentDatabaseContext`**: returns the
  database the pooled connection is currently in.
- **`Server.LoginByName`/`LoginByNameContext`**: single-login lookup,
  alongside the existing bulk `Logins()`.
- **Database files and filegroups** (`database_files.go`):
  `Database.Files` lists every file including the log (unlike
  `FileGroups`, which only sees filegroup members); `AddFile`/`AlterFile`/
  `RemoveFile` and `AddFileGroup`/`RemoveFileGroup`/`SetDefaultFileGroup`/
  `SetFileGroupReadOnly` manage them — SSMS's Database Properties > Files
  page.
- **Database options** (`database_options.go`): `Database.Options` reads
  the `ALTER DATABASE SET` options and related flags (owner, page verify,
  user access, containment, ANSI/ARITHABORT settings, ...) from
  `sys.databases`; `SetDatabaseOption` changes one via an allowlisted
  option name; `SetOwner` transfers ownership — SSMS's Database
  Properties > Options page.
- **Change tracking** (`change_tracking.go`): `Database.ChangeTracking`/
  `SetChangeTracking` for database-level change tracking, and
  `Database.TableChangeTracking`/`SetTableChangeTracking` per table —
  SSMS's Database Properties > Change Tracking page.
- **`Database.IsSystem()`**: reports whether a database is one of the
  four built-in system databases (by `database_id`, not name).
- **`SpaceInfo.UnallocatedMB`**: free space within already-allocated data
  files (SSMS's "Space available"), alongside the existing space fields.
- **Kerberos support** (`kerberos.go`): `AuthWindows` now authenticates
  via Kerberos on every non-Windows platform (previously effectively
  unsupported cross-platform), using the pure-Go `gokrb5` client.
  `ConnectionOptions.Kerberos` (`KerberosOptions`) configures a config
  file, credential cache, keytab, realm, or DNS/UDP tuning; the zero
  value uses the ambient `kinit` cache. `ConnectionOptions.ServerSPN`
  overrides the derived SPN when needed.
- **`ConnectionOptions.AccessTokenProvider`**: a per-connection callback
  that mints a fresh bearer token for each new pooled connection, so
  tokens that expire mid-session (Entra tokens, ~1 hour) are refreshed
  automatically instead of embedded once and going stale. Takes
  precedence over `AccessToken` and `Auth`.
- **`ConnectionOptions.SessionInitSQL`**: T-SQL run on every pooled
  connection right after reset, before the first query — the equivalent
  of SSMS's Query Execution `SET` options.
- **`ParseServerAddress(server)`**: parses every address form SSMS's own
  "Server name" field accepts (`host`, `host:port`, `host,port`,
  `host\instance`, `host\instance,port`) into `(host, instance, port)`.
  Named-instance addresses (`host\instance`) now connect correctly —
  previously a literal backslash would have been mishandled in the DSN.
- **`QuoteName`/`QuoteLiteral`** (`quoting.go`): T-SQL identifier and
  string-literal quoting backed by the driver's own `TSQLQuoter`, shared
  by gosmo's internal escaping and available to callers building their
  own DDL.
- **`IsRetryable(err)`** (`retry.go`): reports whether an error is a
  driver-retryable failure or a dropped pooled connection. `Database.query`
  and `Database.queryRow` now retry transient failures automatically (3
  attempts, linear backoff) since reads are idempotent; `IsRetryable` is
  exported for callers making the same decision about their own
  statements.
- **`WithScript`/`ScriptCollector`** (`script.go`): `WithScript(ctx)`
  returns a context that causes every write funneled through
  `Server.execContext`/`Database.exec` to append its statement to
  `ScriptCollector.Statements` instead of executing — a dry-run/"generate
  script" mode covering the whole write API, not an allowlisted subset.
  Read methods are unaffected.
- **`Database.SetExtendedProperty`/`SetExtendedPropertyContext`**:
  explicit update-only variant of extended properties (`sp_updateextendedproperty`),
  restoring the update path removed from `AddExtendedProperty` (see
  Changed, below).
- **`Table.Triggers`/`TriggersContext`**: per-table trigger listing,
  alongside the existing database-wide `Database.Triggers`.
- **`Column.IsPrimaryKey`**: new field on `Table.Columns()` results.
- **`Login.Details`/`DetailsContext`**: `LoginDetails` — locked/expired/
  must-change-password/policy-checked flags, password-last-set, best-effort
  last login, bad password count, default language, mapped credential, and
  server CONNECT SQL state — SSMS's Login Properties > Status page.
- **`Login.Rename`/`RenameContext`**: renames a login (`ALTER LOGIN ...
  WITH NAME =`), updating `Login.Name` in place on success.
- **`Login.SetDefaultDatabase`/`SetDefaultLanguage`** (+ `Context` variants).
- **`Login.SetPasswordPolicy`/`SetPasswordPolicyContext`**: toggles
  `CHECK_POLICY`/`CHECK_EXPIRATION`.
- **`Login.ChangePasswordWithOptions`/`...Context`**: adds `MUST_CHANGE`/
  `UNLOCK` support beyond the existing `ChangePassword`.
- **`Login.MapCredential`/`UnmapCredential`** (+ `Context` variants).
- **`Login.UserMappings`/`UserMappingsContext`**: every database a login
  maps to, with default schema and role membership.
- **`Login.MapToDatabase`/`UnmapFromDatabase`** (+ `Context` variants):
  map/unmap a login to a database user in one call.
- New `*Seq()` iterators in `iter.go` for every collection added above:
  `ServerPermissionSeq`, `CredentialSeq`, `LanguageSeq`,
  `DatabasePermissionSeq`, `FileSeq`, `TableChangeTrackingSeq`,
  `UserMappingSeq`, `TriggerSeq` (on `*Table`).

### Changed

- **`Database.AddExtendedProperty` no longer upserts (breaking).**
  Previously it fell back to `sp_updateextendedproperty` if the property
  already existed; it now only calls `sp_addextendedproperty` and fails
  if the property is already set at that level. Callers that relied on
  the old upsert behavior should switch to the new
  `SetExtendedProperty`/`SetExtendedPropertyContext`.
- **`AuthWindows` on non-Windows platforms now actively attempts Kerberos
  authentication**, rather than being an effectively unsupported/no-op
  path. Existing non-Windows callers passing `AuthWindows` will hit this
  new code path; see the Kerberos section in the README's Authentication
  docs.
- `ConnectContext` now builds a driver `*mssql.Connector` via the new
  `buildConnector` and opens the pool with `sql.OpenDB`, instead of
  `sql.Open(driverName, dsn)` — what makes `AccessTokenProvider` and
  `SessionInitSQL` possible. No caller-visible signature change.
- Every write method across the package (`Server`, `Database`, `Login`,
  `AgentJob`, ...) now funnels through one of two chokepoints
  (`Server.execContext`, `Database.exec`) so `WithScript` can intercept
  it; no signature changes.
- `Database.query`/`queryRow` now retry transient failures automatically
  (see `IsRetryable`, above).
- `version/version.go`: `Commit`/`Date` are now populated automatically
  at `init()` time — from the Go toolchain's VCS stamp when gosmo is the
  main module, or decoded from gosmo's own pseudo-version string when
  it's embedded as a dependency (e.g. in gossms) — instead of staying
  `"unknown"` without explicit `-ldflags`. `-ldflags -X` overrides still
  work.

### Fixed

- `Server.BackupHistoryContext`: an unnamed backup set (NULL `name` in
  `sys.backupset`) no longer fails the scan — the column is coalesced to
  `""`.
- Named-instance server addresses (`host\instance[,port]`) now produce a
  correct DSN; previously the literal backslash could be mishandled by
  the URL-based DSN builder.

### Dependencies

- `github.com/microsoft/go-mssqldb` upgraded `v1.7.2` → `v1.10.0`.
- `github.com/golang-sql/sqlexp` promoted from indirect to direct
  (used by `backup.go` for BACKUP/RESTORE progress notices).
- New indirect dependency `github.com/jcmturner/gokrb5/v8` (and the rest
  of the `jcmturner/*` family) — the pure-Go Kerberos client backing
  `kerberos.go`.
- Routine indirect bumps pulled in by the go-mssqldb upgrade: the Azure
  SDK (`azcore`, `azidentity`) and MSAL packages, plus `golang.org/x/crypto`
  (→ `v0.54.0`), `golang.org/x/net` (→ `v0.57.0`), `golang.org/x/sys`
  (→ `v0.47.0`), and `golang.org/x/text` (→ `v0.40.0`).
