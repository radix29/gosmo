# Changelog

All notable changes to gosmo are documented here, newest first. This file
starts tracking detail from `v0.0.4` onward; `RELEASE.md` covers the
high-level shape of every release, including the ones before this file
existed.

## v0.0.6

### Added

- **SQL Server Agent alerts** (`agent_alert.go`, new): `Server.Alerts`/`AlertByName`/`CreateAlert` (+ `Context` variants) and the new `Alert` type, with a full write surface — `Enable`/`Disable`/`Drop`/`Rename`/`SetTrigger`/`SetDatabase`/`SetDelay`/`SetNotificationMessage`/`SetJobResponse`/`SetCategory` — plus operator notification links: `Alert.Notifications`, `Notify`, `RemoveNotify`. New supporting types: `CreateAlertRequest`, `AlertNotification`, `NotificationMethod` (msdb's Email/Pager/NetSend bitmask, with a `String` that renders e.g. `"Email, Pager"`) and its `NotifyMethodEmail`/`NotifyMethodPager`/`NotifyMethodNetSend` constants. `Alert.IsEventAlert` and `Server.EventAlerts`/`EventAlertsContext` narrow the list to plain SQL Server event alerts — the subset implementable without WMI or Windows performance counters, matching the exclusions in the README's "Features intentionally excluded". Classification reads `sysalerts.event_source`, not `wmi_query`/`wmi_namespace`: those two columns aren't present on every build (absent on Linux SQL Server, live-confirmed), so selecting them would fail the whole alert list.
- **SQL Server Agent operators** (`agent_operator.go`, new): `Server.Operators`/`OperatorByName`/`CreateOperator` (+ `Context`) and the `Operator` type with `Enable`/`Disable`/`Drop`/`Rename`/`SetEmailAddress`/`SetCategory`. `Operator.NotifyingAlerts` and `NotifyingJobs` are the "referenced by" direction — every alert and job configured to notify this operator (new `AlertNotificationRef`, `JobNotificationRef`). New `CreateOperatorRequest`.
- **SQL Server Agent shared schedules** (`agent_schedule.go`, new): `Server.Schedules`/`ScheduleByName`/`CreateSchedule` (+ `Context`) and the `Schedule` type with `Enable`/`Disable`/`Drop`/`Rename`/`SetOwner`/`SetFrequency`/`SetActiveRange`. `Schedule.Jobs` lists the jobs a shared schedule is attached to, and `Job.AttachSchedule`/`DetachSchedule`/`Schedules` manage that attachment without creating or deleting the schedule itself (unlike the pre-existing `Job.AddSchedule`, which creates one and attaches it in a single step). `Schedule.Description` renders the frequency and active-date range as the one-line English sentence SSMS's Schedule Properties dialog shows. New types and constants: `ScheduleFrequency`, `ScheduleFreqType` (`FreqOnce`/`FreqDaily`/`FreqWeekly`/`FreqMonthly`/`FreqMonthlyRelative`/`FreqAutoStart`/`FreqOnIdle`), `ScheduleSubdayType` (`SubdayOnce`/`SubdaySeconds`/`SubdayMinutes`/`SubdayHours`), `CreateScheduleRequest`, the `Weekday*` bitmask (for a `FreqWeekly` schedule's `FreqInterval`), and the `RelativeDay*`/`Relative*` values a `FreqMonthlyRelative` schedule uses instead.
- **SQL Server Agent categories** (`agent_category.go`, new): `Server.Categories(class)`/`CreateCategory`/`DeleteCategory` (+ `Context`), the `Category` type, and `CategoryClass` (`CategoryClassJob`/`CategoryClassAlert`/`CategoryClassOperator`) — the job/alert/operator category pickers in every Agent properties dialog. `sp_add_category`'s `@type` is derived per class rather than fixed: `LOCAL` for JOB, `NONE` for ALERT and OPERATOR, the only value those two accept ("The specified '@type' is invalid (valid values are: NONE)", live-verified). Clearing a category — `Alert.SetCategory("")`, `Operator.SetCategory("")` — sends the real `[Uncategorized]` category rather than an empty name, which `sp_update_alert`/`sp_update_operator` reject outright ("The specified '@category_name' does not exist"); jobs differ here, using `[Uncategorized (Local)]`.
- **Agent run state**: `Server.AgentInfo`/`AgentInfoContext` → new `AgentStatus` (running flag, status text, last startup time) from `sys.dm_server_services` (whose column is `last_startup_time`, not `startup_time`), so a caller can label SQL Server Agent as stopped rather than reporting an error when it isn't running.
- **Agent job administration** (`agent_job.go`): `Job.Rename`, `SetDescription`, `SetCategory`, `SetOwner`, `SetStartStep`, `SetDeleteLevel`, `SetEmailNotify` (+ `Context` for each) round out the job-properties write surface. New `NotifyLevel` type (`NotifyNever`/`NotifyOnSuccess`/`NotifyOnFailure`/`NotifyOnComplete`) and new `Job` fields `DeleteLevel`, `NotifyLevelEmail`, `NotifyEmailOperatorName`. `JobStep.Update` replaces a step's definition in place (`sp_update_jobstep`) and `JobStep.Delete` removes it; `JobStep` gained `Database`, `OnSuccessStepID`, `OnFailStepID`, `OutputFileName`, and the raw `Flags` bitmask, with `JobStepRequest` gaining the matching `OnSuccessStepID`/`OnFailStepID`/`OutputFileName`. `Server.JobHistory(limit)`/`JobHistoryContext` reads job-level history across every job (the pre-existing `Job.History` is per-job), and `JobHistoryEntry` gained `JobName` for it.
- **Server role administration** (`server.go`): `Server.ServerRoleByName` (principal detail — SID, create/modify dates — that the tree-listing `ServerRoles` leaves out), `ServerRoleMembers` (each member's name *and* principal type, vs. `ServerRoles`'s concatenated name list), `AddServerRoleMember`/`RemoveServerRoleMember`, and `ServerRole.Rename`/`ChangeOwner` (+ `Context` for each) — SSMS's Server Role Properties dialog. `ServerRole` gained `ID`, `Owner`, `SID`, `CreateDate`, `ModifyDate`.
- **Index administration and diagnostics** (`index.go`): `Index.SetOptions` (`ALTER INDEX ... SET`) and `SetLockOptions` (the lock-granularity subset only — SQL Server rejects `IGNORE_DUP_KEY` outright on an index backing a PRIMARY KEY or UNIQUE constraint, live-verified); `RebuildWithOptions` (fill factor, pad index, data compression — none of which is a plain `SET` option); `SetIncludedColumns`, which reissues a full `CREATE INDEX ... WITH (DROP_EXISTING = ON)` since changing the `INCLUDE` list isn't an `ALTER`; `Rename` (via `sp_rename` — also how a PRIMARY KEY or UNIQUE constraint gets renamed, its name being the backing index's); `UpdateStatistics`; `StorageInfo` → new `IndexStorageInfo`/`IndexAllocationUnit` (filegroup/partitioning plus the `IN_ROW_DATA`/`LOB_DATA`/`ROW_OVERFLOW_DATA` breakdown); and `Fragmentation`, the single-index analog of `Table.FragmentationStats`. `Index` gained `IsPadded`, `IgnoreDupKey`, `AllowRowLocks`, `AllowPageLocks`, `DataCompression`; `IndexFragmentation` gained `AvgPageSpaceUsedPct` (populated only in `SAMPLED`/`DETAILED` mode, matching the DMV).
- **Statistics detail** (`statistics.go`): `Statistic.Columns` (in stat-column order), and the three `DBCC SHOW_STATISTICS` result sets as first-class types — `Header` → `StatisticHeader`, `DensityVector` → `StatisticDensity`, `Histogram` → `StatisticHistogramStep` (+ `Context` variants) — SSMS's Statistics Properties > Details page. `Statistic` gained `NoRecompute`, `IsIncremental`, `ModificationCounter`.
- **Filtered-predicate helpers** (`table.go`): `Table.CountWhere(predicate)` counts the rows a filtered index or statistic's predicate would qualify (SSMS's "Estimate Rows"), and `Table.CheckWhereSyntax(predicate)` validates one without scanning any data ("Check Syntax"), both with `Context` variants.
- **`Server.CurrentLogin`/`CurrentLoginContext`** (`server.go`): the login the connection is actually authenticated as (`SUSER_NAME()`) — which for Windows/Entra auth differs from whatever was passed as `ConnectionOptions.User`, often empty for those methods.
- **`ConnectionOptions.ConnMaxIdleTime`** (`server.go`), defaulting to 5 minutes when zero: caps how long a pooled connection may sit idle before it's evicted rather than handed out again. Without it, a connection silently dropped while idle — by a firewall/NAT idle timeout, a load balancer, or the server closing a long-idle session — sits in the pool looking usable until something tries it and fails.
- **`Database.FileGroups`/`FileGroupsContext`** (`database_files.go`) now reports `FileGroup.IsReadOnly` and each member file's `IsPrimaryFile`/`FileGroupName` alongside the existing size/growth columns.
- **40 new `*Seq()` iterators** (`iter.go`), completing the `Foo`/`FooContext`/`FooSeq` trio for every collection method that was missing one: on `*Database` — `DatabaseRoleSeq`, `DatabaseScopedConfigSeq`, `DependencySeq`, `DependentSeq`, `ExtendedPropertySeq`, `FileGroupSeq`, `PermissionSeq`, `PermissionsForPrincipalSeq`, `RoleMemberSeq`, `SchemaPermissionSeq`, `SearchSeq`, `TablesBySchemaSeq`, `TriggerSeq`, `UserDefinedFunctionSeq`; on `*Server` — `ActiveSessionSeq`, `AlertSeq`, `BackupHistorySeq`, `CategorySeq`, `ConfigurationSeq`, `EventAlertSeq`, `JobHistorySeq`, `LinkedServerSeq`, `MailProfileSeq`, `OperatorSeq`, `ReadErrorLogSeq`, `ScheduleSeq`, `ServerRoleMemberSeq`, `ServerRoleSeq`; and `Alert.NotificationSeq`, `Operator.NotifyingAlertSeq`/`NotifyingJobSeq`, `Schedule.JobSeq`, `Job.HistorySeq`/`ScheduleSeq`/`StepSeq`, `Statistic.ColumnSeq`/`DensityVectorSeq`/`HistogramSeq`, `Table.CheckConstraintSeq`/`FragmentationStatsSeq`.
- **Nine missing `Context` twins** filled in, for methods that had shipped without one: `Login.Drop`, `Schema.Drop`, `User.Drop`, `User.AddToRole`, `User.RemoveFromRole`, `Index.Enable`, `Job.Enable`, `Job.Disable`, `Server.BackupHistory`.

### Changed

- **Server-scoped reads now retry transient connection failures.** `Server.query`/`queryRow`/`queryRowScan` are new internal chokepoints mirroring `Database.query`/`queryRow`, and roughly every server-level read in the package (`server.go`, `server_config.go`, `server_security.go`, `login.go`, `backup.go`, `agent_*.go`) now goes through them instead of calling `s.db.QueryContext`/`QueryRowContext` directly. Retry previously only covered `Database`-scoped reads.
- **`Database.queryRow` now takes a scan callback** — `queryRow(ctx, func(row *sql.Row) error, q, args...) error` — instead of returning `(*sql.Row, func(), error)` for the caller to scan later. `QueryRowContext` itself never returns an error; the failure only surfaces at `Scan` time, so a `Scan` that ran *after* the retry wrapper returned was never covered by it. This is internal, but it changes the shape of nearly every single-row read in the package (`scripter.go`, `query_store.go`, `table.go`, …).
- **`IsRetryable` recognizes more transient failures**: in addition to the driver's `RetryableError` and a wrapped `driver.ErrBadConn`, it now reports true for the raw connection-level failures the driver itself treats as fatal to a connection — a `net.Error`, a corrupted TDS stream (`mssql.StreamError`), a connection-severing `mssql.ServerError`, or `io.EOF`. Those surface unwrapped whenever the driver decided retrying the exact in-flight call wasn't safe; that restriction is about re-running the *same* call, not about whether the connection is salvageable, so a caller retrying its own idempotent operation on a fresh connection still is.
- **`ScriptCollector.Statements` is now guarded by a mutex.** Nothing stops a caller from reusing one `WithScript` context across write calls issued from several goroutines, which previously raced on the slice append.
- **Input validation on values that can't be parameterized.** New shape/allowlist checks reject unrecognized input before it reaches a DDL string: `validRecoveryModel`, `validDataType`, and `validBackupAction` (`types.go`); `validPartitionBoundary`, a literal-shape check for partition function boundary values (`partition.go`); `ALTER DATABASE SET QUERY_STORE`'s `OPERATION_MODE`/`QUERY_CAPTURE_MODE`/`SIZE_BASED_CLEANUP_MODE`/`WAIT_STATS_CAPTURE_MODE` keywords (`query_store.go`); and `Index.RebuildWithOptions`'s `dataCompression` argument (`index.go`).
- **`database.go` split by object family**, matching the one-file-per-family convention: views moved to `view.go`, stored procedures to `procedure.go`, user-defined functions to `function.go`, database roles and role membership to `database_role.go`, and filegroup listing to `database_files.go` alongside the file methods already there. No API change.
- `Alert`/`Operator`/`Schedule` reads share `scanAlert`/`scanOperator`/`scanSchedule` helpers so the list and single-item queries can't drift apart.

### Fixed

- **`Database.query` leaked a pooled connection on every call.** It acquired a dedicated `*sql.Conn` (needed to run `USE` before the query) and returned the bare `*sql.Rows`, on the assumption that closing the rows would release the connection — it doesn't: `*sql.Rows.Close` only frees the query's own resources, and the `*sql.Conn` stays checked out until *its* `Close` runs, which nothing did. Every rows-returning `Database` read permanently consumed one pool slot: an application that set `ConnectionOptions.MaxOpenConns` wedged completely after that many reads, every later call blocking forever waiting for a connection that could never come back, and one that left it unlimited grew its connection count without bound instead. `query` now returns a `*dbRows` wrapper whose `Close` closes the rows *and* the connection.
- **`Database.queryRow`'s retry never covered the failure it existed for** — see the signature change above.
- **Server-scope `GRANT`/`DENY`/`REVOKE` silently did nothing outside a `master` session.** SQL Server rejects permission changes at server scope unless the session's current database is `master` ("Permissions at the server scope can only be granted when the current database is master") — a real engine restriction, live-verified, so `Server.GrantServerPermission` and its `Deny`/`Revoke` siblings only worked from a connection that happened to be in `master`. Each statement now carries a `USE master` prefix in the same batch (`server_security.go`).
- **`CreateJob` created jobs SQL Server Agent then refused to start.** A job that isn't enlisted on a target server via `sp_add_jobserver` fails at `sp_start_job` ("does not have a target server"); `CreateJobContext` now enlists it on `(local)` as part of creation.
- **A job's `NextRunDate` reported the year 1900 instead of "never".** The query coalesced a NULL `next_scheduled_run_date` to the `'19000101'` sentinel; it now scans through `sql.NullTime` and leaves `NextRunDate` the zero `time.Time`.
- **`Job.JobID` came back corrupted.** `sysjobs.job_id` is a `uniqueidentifier`, and scanning it straight into a `string` yields the raw 16 bytes rather than the GUID text; the query now selects `CONVERT(varchar(36), j.job_id)`.
- **Partition boundary values were spliced into DDL unvalidated** (`partition.go`) — `CREATE PARTITION FUNCTION ... FOR VALUES`, `SPLIT RANGE`, and `MERGE RANGE` take literals that can't be parameterized, so each value is now checked against the shape of a well-formed SQL Server literal (signed integer/decimal, hex, a properly escaped quoted string/date, or `NULL`) before use.
- **`Sequence.NextValue` ran through the retried read path**, but `NEXT VALUE FOR` advances the sequence — a write, not an idempotent read, and re-running it on a transient failure consumes an extra value. It now uses `withConn`, which doesn't retry the caller's statement.
- **`Database.EstimatedPlan`/`ActualPlan`'s cleanup used `context.Background()`**, which can never time out, so an unresponsive connection could block the deferred `SET SHOWPLAN_XML OFF`/`SET STATISTICS XML OFF` forever. Cleanup now runs on `context.WithTimeout(context.WithoutCancel(ctx), 5s)` — still runs when the caller's context is already canceled, but bounded.
- `Login.Drop`, `Schema.Drop`, and `User.Drop` ignored any `WithScript` collector or cancellation because they delegated to the `Server`/`Database`-level method with a fresh `context.Background()`; each now routes through its own `DropContext`.
- `formatColumnType` (`table.go`) documents that it trusts `col.DataType` — callers validate via `validDataType` first — replacing a stale comment claiming it was the package's single canonical implementation (`ColumnTypeString` in `scripter.go` is its `*Column` counterpart).

### Dependencies

No dependency changes — `go.mod` and `go.sum` are byte-for-byte identical to `v0.0.5`.

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
