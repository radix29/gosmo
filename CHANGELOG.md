# Changelog

All notable changes to gosmo are documented here, newest first. This file
starts tracking detail from `v0.0.4` onward; `RELEASE.md` covers the
high-level shape of every release, including the ones before this file
existed.

## v0.0.9

### Added

- **Always On availability groups** (`availability_group.go`, ~1,960 lines): the whole SSMS Always On node, read and write. `Server.AvailabilityGroups`, `AvailabilityGroup(name)` (a no-I/O handle) and `AvailabilityGroupByName` read `sys.availability_groups` joined to `sys.dm_hadr_availability_group_states`; `AvailabilityGroup.Replicas`, `.Databases` and `.Listeners` read the replicas, the per-database synchronization state (queue sizes, rates, `SecondaryLagSeconds`, the four last-*-time stamps) and the listeners with their IP configurations. Writes cover the group (`SetAutomatedBackupPreference`, `SetFailureConditionLevel`, `SetHealthCheckTimeout`, `SetDBFailover`, `SetDTCSupport`, `SetRequiredSynchronizedSecondariesToCommit`), the replicas (`SetAvailabilityMode`, `SetFailoverMode`, `SetSeedingMode`, `SetPrimaryRoleAllowConnections`, `SetSecondaryRoleAllowConnections`, `SetSessionTimeout`, `SetBackupPriority`, `SetReadOnlyRoutingURL`, `SetReadOnlyRoutingList`), membership (`AddReplica`/`RemoveReplica`, `AddDatabase`/`RemoveDatabase`, `JoinDatabase`/`UnjoinDatabase`, `SuspendDatabase`/`ResumeDatabase`), listeners (`AddListener`, `AddListenerIP`, `SetListenerPort`, `RemoveListener`), lifecycle (`CreateAvailabilityGroup`, `Join`, `Drop`, `GrantCreateAnyDatabase`/`DenyCreateAnyDatabase`) and failover (`Failover`, `ForceFailoverAllowDataLoss`). **A cluster-wide catalog view and a `sys.dm_hadr_*` DMV do not mean the same thing**: the catalog agrees on every replica, the DMVs describe only what the local instance can see, so a secondary reports no queue sizes or commit times for databases it does not host. `PrimaryReplicaServerName` and `IsLocalPrimary` exist so a caller can tell where it is and follow the primary; an empty `PrimaryReplicaServerName` means "unknown from here", not "no primary".
- **Certificates and the database master key** (`certificate.go`): `Database.Certificates`, `CertificateByName`, `CreateCertificate(CertificateSpec)`, `Certificate.Drop`, `HasMasterKey` and `CreateMasterKey`. `Certificate.Encoded` (`CERTENCODED`) and `CertificateSpec.FromBinary` (`CREATE CERTIFICATE ... FROM BINARY`) are the pair that moves a certificate between instances **without filesystem access on either host** — the documented `BACKUP CERTIFICATE`-to-file route needs it and a client library has none. Only the public certificate crosses the wire, which is what makes it safe over an ordinary connection and enough for mirroring endpoints, where each instance keeps its own private key. `FROM BINARY` requires SQL Server 2022 or later. `CertificateByName` reports absence as `(nil, nil)` rather than an error, because its callers branch on absence as the ordinary case.
- **The database mirroring endpoint** (`endpoint.go`): `Server.DatabaseMirroringEndpoint`, `CreateDatabaseMirroringEndpoint(EndpointSpec)`, `Start`, `Stop`, `Drop`, `GrantConnect(login)` and `URL()`. This is Always On's transport: a replica cannot join a group until every instance has one started and has granted the others' service accounts `CONNECT` on it. **An instance can have at most one**, whatever it is called — a server rule, not a convention — so a second availability group over the same pair of instances reuses the first one's endpoint and port, and setup code reads before it considers creating.
- **Error log reading** (`error_log.go`): `Server.EnumErrorLogs(logType)` lists the archived log files with their sizes and last-written times, and `ReadLog(logType, logNumber)` reads one. `ErrorLogType` is `ErrorLogSQLServer` or `ErrorLogAgent` — the log-type argument `xp_readerrorlog`/`sp_enumerrorlogs` themselves take, so it passes straight through — which makes the Agent log readable through the same two methods rather than a second pair. `ReadErrorLog(n)` remains as the SQL Server-log shorthand, and `CycleErrorLog` starts a new one.
- **Server-side filesystem enumeration** (`filesystem.go`): `Server.EnumFileSystem(path)`, `FixedDrives()` and `FileSystemExists(path)` — SMO's `EnumDirectories`/`EnumFiles` equivalent. Every path is interpreted by the **server**, which is routinely a different machine with different path conventions from the process calling gosmo; that is the whole reason these exist rather than a caller using `os.ReadDir`. Each has a modern and a legacy implementation (`sys.dm_os_enumerate_filesystem` / `sys.dm_os_enumerate_fixed_drives` against `xp_dirtree` / `xp_fixeddrives`) chosen by instance version.
- **`ErrNotFound`** (`errors.go`): the sentinel every by-name lookup that reports absence as an error now wraps, so a caller can tell "this object does not exist" from "the lookup itself failed" with `errors.Is` instead of matching message text. A caller that reads any error as absence goes on to create an object it never established was missing, and then reports the creation's failure instead of the permission or connection error that actually stopped it. Message text is unchanged: `notFoundError` carries the caller-facing string and reaches the sentinel through `Unwrap() []error`. Three conventions coexist deliberately and `ErrNotFound`'s doc comment names all three — most lookups wrap it, `CertificateByName` returns `(nil, nil)`, and `AgentStatus` reports an unreachable Agent as a populated value. `AvailabilityGroupByName` additionally still satisfies `errors.Is(err, sql.ErrNoRows)`, which it promised before the sentinel existed. Pinned live against a real server by `live_notfound_test.go`.
- **Drop and rename, filled in** across the object families that had one but not the other: `Database.DropView`, `DropFunction`, `DropTrigger`, `DropSequence`, `DropSynonym`, `DropDatabaseRole` and `RenameObject`; `Server.DropServerRole` and `RenameDatabase(old, new, force)`; `Table.DropConstraint`; `Statistic.Rename`; and `Drop` methods on `DatabaseRole` and `ServerRole` to match the by-name forms. `RenameObject` covers every schema-scoped object `sp_rename`'s default `'OBJECT'` type takes — view, procedure, function, sequence, synonym, trigger — and documents that `newName` is a bare name, because `sp_rename` refuses a qualified one and a rename does not move an object between schemas.
- **`Database.Table(schema, name)`** (`table.go`): the lightweight no-I/O `Table` handle, matching `Server.Database`/`Server.Login`/the four Agent handles — the form that works under a `WithScript`-derived context, where `TableByName`'s catalog read has nothing to find.
- **`ServerInfo.Platform`** (`types.go`, `server.go`): the host OS family, `"Windows"` or `"Linux"`, derived from `@@VERSION` rather than `sys.dm_os_host_info` so it is populated on pre-2017 instances too. Empty when `@@VERSION` names neither.
- **Seven new `*Seq` iterators** (`iter.go`): `AvailabilityGroupSeq`, `ReplicaSeq`, `DatabaseSeq`, `ListenerSeq`, `CertificateSeq`, `ReadLogSeq` and `EnumErrorLogSeq` — one for every collection-returning method added here, per the standing convention.
- **`script_*_write_test.go`** (`script_write_common_test.go` plus agent, extended-properties, files, maintenance and security suites): statement-pinning coverage using `WithScript` as a serverless harness, asserting the **whole** statement rather than a substring. Every case feeds a quote-hostile value — `o'brien`, `a]b`, `Sales.Archive` — through each parameter that reaches the statement text, so a name that loses its brackets or a literal whose apostrophe stops being doubled shows up as a diff here instead of as a live failure months later.
- **Live test suites** (`-tags livedb`): `live_availability_group_test.go` verifies the Always On layer against a real two-node cluster, including the catalog-versus-DMV asymmetry above and a create/drop cycle behind its own flag; `live_notfound_test.go` pins the `ErrNotFound` classification against what SQL Server actually returns for each lookup; `live_filesystem_test.go` pins that the `xp_dirtree` fallback describes the same directory the DMV does, entry for entry.
- **`drop_rename_test.go`** and **`error_log_test.go`, `errors_test.go`, `certificate_test.go`, `endpoint_test.go`, `filesystem_test.go`, `availability_group_test.go`**: offline statement and parsing coverage for everything above.

### Changed

- **A `DROP` method no longer emits `IF EXISTS`.** Half of them carried it and half did not, so the same gesture in a caller's UI reported two different things about the same situation: deleting an already-gone view said "deleted", deleting an already-gone sequence said the server refused. Dropping something that isn't there is now uniformly the server's own "Cannot drop ... because it does not exist". A caller that wants the idempotent form ignores the error — a decision it can make and this package cannot make for it. The generated **scripts** keep `IF EXISTS`: `Scripter`'s DROP-and-CREATE output exists to be re-run, which is the opposite requirement.
- **Errors are wrapped with what was being attempted, everywhere** (~35 files). `rows.Err()` and every `rows.Scan` now wrap with the *same* message their function's query error uses, per the convention in `CLAUDE.md`: a failure mid-iteration was otherwise indistinguishable from any other and reached the caller as a naked `context deadline exceeded` naming nothing. The shared scan helpers still return bare errors on purpose — only their callers know which operation to name.
- **`BuildRestoreStatement` lays the statement out over several lines** (`backup.go`) — target, devices, then one `WITH` option per line. A restore that relocates files carries a `MOVE` clause per database file, each holding two full paths; on one line that runs to several hundred columns, and a caller scripting it for review sees every `MOVE` off the right edge of the editor, which reads as the clauses being missing. Whitespace is not significant to SQL Server here, so the executed statement is unchanged.
- **`EnumFileSystemContext`'s version gate now fails toward `xp_dirtree`** (`filesystem.go`). It gated negatively — `xp_dirtree` only on a *known* pre-2017 instance, so an unknown version (no `ServerInfo` loaded, or a major of 0) fell through to `sys.dm_os_enumerate_filesystem`. The DMV does not exist before SQL Server 2017 and `xp_dirtree` exists everywhere, so the guess was aimed at the branch that can fail outright. The gate is now positive: the DMV only on a known 2017-or-later instance, everything else on `xp_dirtree`. **Visible to callers only in the unknown-version case**, which now returns entries with `Size` and `LastModified` zero rather than an error — a caller browsing for a path needs the names and the directory flag and can live without the other two. Every known version is unchanged.
- **`escapeLikePattern` is gone; `securable_search.go` uses the existing `likeEscape`** (`helpers.go`). `v0.0.8` shipped a second, identical LIKE escaper rather than reaching for the one already there. `likeEscape`'s doc comment absorbed the reasoning and `helpers_test.go` the cases; behaviour is identical.
- `Database.CreateUser` refuses an empty login name instead of emitting `FOR LOGIN []`, which the server rejects with a message naming an empty login the caller never typed. A user with no login is `CREATE USER ... WITHOUT LOGIN`, a different statement, so this refuses rather than picking one.

### Fixed

- **An empty schema made a statement address the wrong object.** `qualifiedName` emitted `[].[name]`, which `OBJECT_ID` resolves to NULL; it now returns `[name]` (`helpers.go`). Most callers take `schema` straight from an exported method's parameter, so an empty one reached the statement and the caller got an empty result set rather than an error. Pinned by `helpers_test.go`.
- **`Table.Indexes` could exhaust a connection pool** (`table.go`). It now reads every index's columns in one query instead of one per index. The columns were fetched inside the loop over the indexes, and `Database.query` pins its own pooled connection and issues its own `USE`, so a table with 20 indexes cost 42 round trips across 21 connections — with the outer connection held throughout, which is the shape that exhausts a pool rather than merely being slow. Now 2. `indexListContext` drains and closes its rows before the column query opens, so the two never hold two connections at once.
- **`Login.UserMappings` reported a short list as success.** Its skip — there to pass over a database it cannot read — also swallowed a failure that struck partway through a database's rows, whose earlier rows were already in the result. The skip is now narrowed to a database whose query never opened (`login.go`); once rows are being read, a failure ends the scan with an error. The per-database loop is also documented as serial **on purpose** — fanning it across a worker pool was tried and measured slower against a 46-database instance, because `Database.query` pins a pooled connection of its own and each worker pays a full TCP+TLS+login handshake on a pool with nothing idle.

### Dependencies

- `github.com/Azure/azure-sdk-for-go/sdk/azcore` 1.22.0 → 1.23.0, `github.com/AzureAD/microsoft-authentication-library-for-go` 1.7.2 → 1.8.0, `golang.org/x/crypto` 0.54.0 → 0.55.0, `golang.org/x/net` 0.57.0 → 0.58.0, `golang.org/x/text` 0.40.0 → 0.41.0, new indirect `golang.org/x/sync` 0.22.0; toolchain go1.26.5 → go1.26.6. All indirect, all through the driver's own dependency set.

## v0.0.8

### Added

- **`PermissionOptions` and a `...WithOptions` form of every GRANT/DENY/REVOKE** (`permission_options.go`): `WithGrantOption`, `Cascade`, and `GrantOptionOnly` (`REVOKE GRANT OPTION FOR`) — the modifiers the plain trios had no way to express, at every scope: object, column, schema, database, and server. Fifteen new method pairs, all rendering through one `permissionStmt.render`. A modifier the verb has no form for is an error rather than a silently plain statement: `WITH GRANT OPTION` on a `DENY`, `CASCADE` on a `GRANT`, `GRANT OPTION FOR` on anything but a `REVOKE`. The zero value renders exactly what the plain method renders, which is what makes the delegation below safe.
- **Column-level permissions** (`column_permission.go`): `Database.ColumnPermissions(schema, name)` and `ColumnPermissionsForPrincipal(principal)` read the `OBJECT_OR_COLUMN` rows of `sys.database_permissions` with a non-zero `minor_id` — the grants `Permissions` does not report, and genuinely separate ones (a column `DENY` overrides an object `GRANT`). `Grant|Deny|RevokeColumnPermission(schema, name, perm, cols, principal)` and their `WithOptions` forms write them, one statement covering all named columns. `ColumnPermissionNames()` is the catalog a column-permissions grid enumerates: only `SELECT`, `UPDATE` and `REFERENCES` have a column-level form at all, since `GRANT DELETE (col)` is a syntax error. `ColumnPermissionEntry.ObjectType` distinguishes a view's grants from a table's, which `sys.database_permissions` records identically.
- **Effective permissions** (`effective_permission.go`): `Database.EffectivePermissions(principal)`, `EffectiveObjectPermissions(schema, name, principal)`, `EffectiveSchemaPermissions(schema, principal)` and `Server.EffectiveServerPermissions(login)` report what a principal can actually do — SSMS's Effective tab — with role membership, ownership, inherited scopes and `DENY` already resolved by the server rather than re-derived here. Asking about an object reports its column-level permissions too, as rows carrying `EffectivePermission.Subentity`. The principal must be a user or login: resolution works by impersonating it, and SQL Server refuses to impersonate a role (Msg 15517), while `fn_my_permissions` has no principal argument to use instead.
- **Securable search** (`securable_search.go`): `Database.FindSecurables(SecurableSearch{Name, Limit})` → `[]SecurableRef` returns the schemas, tables and views matching what a user typed, in one query, ordered schemas-then-tables-then-views so a capped search returns a stable prefix. It exists for a permissions picker on a database with thousands of tables, where "list the whole catalog and filter in the client" is both slow to open and useless as a picker. Matching is case-insensitive and substring, against the qualified name, with LIKE wildcards in the input escaped (`escapeLikePattern`) so a typed `%` matches a literal `%`.
- **`Database.ObjectColumns(schema, name)`** (`table.go`): the columns of a table *or a view*, in ordinal order. `Table.Columns` covers tables only and a view has no handle type carrying an `object_id`, so this was the missing way to reach a view's columns — which do carry permissions, and so do appear on a Securables page. The joins that supply identity, computed text, defaults and primary key don't match for a view, so those come back zero-valued; everything else is real. An `OBJECT_ID` that resolves to nothing is reported as an error rather than as an empty column list.
- **`Server.BackupFileListForSet(device, fileNumber)`** (`backup.go`): `RESTORE FILELISTONLY WITH FILE = n`, for a device backups were appended to. Such a device holds one set per backup and their file lists differ, so building `RESTORE`'s `MOVE` clauses from the wrong set names logical files the restored set doesn't contain — which SQL Server rejects outright. `fileNumber` is the same 1-based value as `BackupHeader.Position` and `RestoreOptions.FileNumber`.
- **`JobStep.LastRunDate` and `JobStep.LastRunElapsed`** (`agent_job.go`): when the step last ran, and its last run's duration as a `time.Duration`. `LastRunDuration` remains msdb's raw `HHMMSS` integer (`10230` is 1h 02m 30s, not 10230 seconds) — `LastRunElapsed` is that value decoded, and is what display code should use. `LastRunDate` is zero for a step that has never run, so `IsZero()` is the test, not a sentinel date.
- **Seven new `*Seq` iterators** (`iter.go`): `ColumnPermissionSeq`, `ColumnPermissionsForPrincipalSeq`, `EffectivePermissionSeq`, `EffectiveObjectPermissionSeq`, `EffectiveSchemaPermissionSeq`, `EffectiveServerPermissionSeq`, and `ObjectColumnSeq` — one for every collection-returning method added here, per the standing convention.
- **Live verification tests** (`live_execproc_script_test.go`, `live_setifapplied_test.go`): a scripted `ExecProc` is now checked by running the script it generates against a real server for every output-parameter destination type, and the `setIfApplied` setters by asserting the server's own state after a scripted call. Both cover behaviour the fakes cannot: what SQL Server accepts, and what it did not change.

### Changed

- **BREAKING: `Table.AddColumn` and `Table.DropColumn` are gone** (`table.go`), along with their `Context` variants. `AlterColumn` remains; its doc comment no longer points at the removed pair for the identity/default cases.
- **The twelve plain `Grant|Deny|Revoke...Permission` methods are now one-line delegations** to their `WithOptions` counterparts passing `PermissionOptions{}` (`security.go`, `server_security.go`), instead of each rendering its own `fmt.Sprintf`. Behaviour is unchanged, deliberately and verifiably: `legacy_permission_delegation_test.go` pins the exact statement and the exact validation error every one of them produced before the change. The point is that there is now one renderer and one set of error strings rather than two that have to be kept in step as modifiers are added.
- **`ConfigurationOption.SetValue` and the five `Database` state setters go through `setIfApplied`** (`server_config.go`, `database.go`): `SetRecoveryModel`, `SetCompatibilityLevel`, `SetReadOnly`, `SetOffline` and `SetOnline` were the remaining direct assignments `v0.0.7`'s sweep missed, so under `WithScript` each left its object claiming state the server never took.
- **`Server.BackupFileList` documents that it reads the *first* set** and delegates to `BackupFileListForSetContext(ctx, device, 0)`; the statement is built by a testable `backupFileListQuery` rather than inline.
- `Table.Columns` and `Database.ObjectColumns` share one `columnSelect` and one `scanColumns` — the two differ only in the `WHERE`, because a `Table` already holds an `object_id` while `ObjectColumns` has a name to resolve.
- `JobStepRequest.Database` and `.OutputFileName` document that they read an empty value differently, because msdb does — see the fix below.

### Fixed

- **A scripted `ExecProc` declared `SQL_VARIANT` for output parameters that cannot receive one.** `scriptDeclType`'s kind switch sent every `sql.Null*` destination (a struct), `mssql.UniqueIdentifier` (a `[16]byte` array) and `mssql.NullUniqueIdentifier` to `SQL_VARIANT`, so the generated script failed on execution with "Implicit conversion from data type sql_variant to int is not allowed" — and `sql.Null*` is *the* way to receive a nullable output parameter, so this hit the ordinary case. A `declTypeByName` map now answers for those types ahead of the kind switch, verified live against every entry (`script.go`).
- **`IsRetryable` reported a caller's own expired deadline as transient** (`retry.go`). `context.DeadlineExceeded` implements `net.Error`, so it passed the `net.Error` timeout test and every read wrapped in `WithRetry` waited out three attempts on a deadline that had already passed. Both context errors are now rejected explicitly, named together so they can't drift apart.
- **Blanking a job step's output file silently kept the old path.** `JobStep.UpdateContext` omitted `@output_file_name` when the request's value was empty, so a caller's form clearing the field changed nothing on the server while the local `JobStep` reported it as cleared. `@output_file_name` does honour `N''` — it nulls the column — so it is now always sent. `@database_name` is the opposite case, verified against SQL Server 2025: `sp_update_jobstep` accepts `N''` and changes nothing, so an empty `Database` is now read as "leave it alone" and, crucially, is no longer mirrored onto the local `JobStep` either — which had been the only reason the object claimed a database the server had never stopped using.
- **`Job.Steps` never reported when a step last ran.** `sysjobsteps.last_run_time` wasn't selected, so the date half had no time to pair with and the field didn't exist at all. Both columns are now read; `last_run_date = 0` (never ran) is left as a zero `time.Time` rather than being decoded into a year-zero date.

### Dependencies

No dependency changes.

## v0.0.7

### Added

- **`Scripting(ctx) bool`** (`script.go`): reports whether `ctx` came from `WithScript`, i.e. whether a write invoked with it records its statement instead of running it. A caller that mirrors a write into its own state needs to know — under `WithScript` a write returns success without the server ever seeing it, so state derived from "it worked" is wrong. The case it exists for is a rename: an editor that renames an object and then re-reads it by the new name finds nothing. Reads are unaffected by `WithScript` either way.
- **Lightweight Agent handles** (`agent_alert.go`, `agent_job.go`, `agent_operator.go`, `agent_schedule.go`): `Server.Alert(name)`, `Server.Job(name)`, `Server.Operator(name)`, `Server.Schedule(name)` return a no-I/O handle carrying only the name — the Agent-side counterparts of `Server.Database`/`Server.Login`. Every write method on those four types addresses its object by name, so a handle is enough to keep operating on one the caller already knows exists, and it is the only usable form under `WithScript`, where the `...ByName` lookup is a real read and an object whose `sp_add_*` was merely collected isn't there to find. `CreateAlertContext`/`CreateJobContext`/`CreateOperatorContext`/`CreateScheduleContext` now return such a handle under `WithScript` rather than failing their read-back.
- **Whole-database space and row-count reads** (`partition.go`, `table.go`): `Database.TableSpaceUsedAll`/`Context` → `map[int]*TableSpaceInfo` and `Database.TableRowCounts`/`Context` → `map[int]int64`, both keyed by `object_id` — the per-table `Table.SpaceUsed`/`Table.RowCount` for every user table in one round trip instead of one query (and one pooled connection) per table, which is what a grid listing a few hundred tables was costing. Same aggregates and filters as the per-table forms, so the numbers are identical. A table with no row in `sys.partitions` is absent from the map rather than present as zero.
- **File and filegroup backup/restore** (`backup.go`): `BackupOptions.Files`/`FileGroups` and `RestoreOptions.Files`/`FileGroups` render the `FILE = N'...'` / `FILEGROUP = N'...'` clauses a `BackupActionFiles` operation needs. At least one of the two is required for that action — with neither, the statement would render as a plain full `BACKUP DATABASE`, quietly doing far more work than the caller asked for.
- **`RestoreOptions.FileNumber`** (`backup.go`): `RESTORE ... WITH FILE = n`, selecting which backup set on the device to restore (1-based, as reported by `BackupHeader.Position`). Zero leaves the clause off, which SQL Server reads as the first set — so a device holding an appended differential or log needs this set explicitly.
- **`IndexTypeClusteredColumnStore`** and **`IndexType.IsColumnStore()`** (`types.go`): the sixth `sys.indexes.type_desc` value, plus the predicate covering both columnstore forms. `Table.Indexes` now maps `CLUSTERED COLUMNSTORE` to it (and sets `IsClustered`), and carries any *other* unrecognized `type_desc` — `NONCLUSTERED HASH`, or whatever a newer SQL Server adds — through as the server's own text rather than leaving `Type` empty.
- **Sampling-percentage validation** (`statistics.go`): `Statistic.Update`, `Table.UpdateAllStatistics`, and `Table.CreateStatistic` (and their `Context` variants) now reject a `samplePct` outside 0-100 with a gosmo error rather than letting it reach the server as a `SAMPLE n PERCENT` syntax error. 0 keeps its existing meaning of "not a percentage" — FULLSCAN, or the server's own default sampling.
- **Eight runnable examples** (`examples/`): the single guided-tour program is now backed by `backup/`, `bulkcopy/`, `diagnostic/`, `iterators/`, `jobs/`, `maintain/`, `scripting/`, and `security/`, each going deep on one subject, plus `examples/README.md` and a shared `examples/internal/demo` harness. Each creates its own throwaway database and drops it afterwards; nothing already on the instance is modified.

### Changed

- **BREAKING: every `*Seq()` iterator now takes a `context.Context`** (`iter.go`) — `db.TableSeq()` becomes `db.TableSeq(ctx)`, and so on for all 75 of them. They previously wrapped the plain (non-`Context`) collection method, i.e. `context.Background()`, so ranging over one was uncancellable and unbounded — the one part of the public API `v0.0.6`'s context-everywhere work didn't reach. Each now runs on the matching `...Context` method, so an iterator is cancellable and can carry a deadline like any other read here. The fetch is still deferred until the iterator is actually ranged over, so `ctx` is evaluated then, not at the call that builds it. No iterator was added or removed; they now share one `seqFrom` helper rather than each repeating the yield loop.
- **A scripted write no longer mirrors its change onto the receiver.** New internal `setIfApplied` (`script.go`) replaces every direct assignment a write method made back onto its object — `Rename` updating `.Name`, `Enable` updating `.IsEnabled`, and so on across `agent_alert.go`, `agent_job.go`, `database_role.go`, `index.go`, `login.go`, `schema_user.go`, `security.go`, `server.go`. Under `WithScript` the write only recorded a statement and the server still holds the old value, so the object was left claiming state the server didn't have and the next call built from it (a second rename, a delete by name) targeted an object that doesn't exist.
- **`Table.CreateIndex` rejects `IndexTypeClusteredColumnStore`** rather than quietly emitting a plain `NONCLUSTERED` index: `CREATE CLUSTERED COLUMNSTORE INDEX` takes no key columns at all and doesn't fit the statement that method builds. Likewise `Index.SetIncludedColumns` now refuses a columnstore index — it has no `INCLUDE` list, and the `CREATE ... WITH (DROP_EXISTING = ON)` behind that method would have replaced it with a rowstore index of a different kind.
- **`BulkInsert` discards its connection when a load is abandoned** (`bulkcopy.go`). Returning early — a row-iterator error, a wrong-width row, a failed row exec, a failed flush — left the connection mid-bulk-copy with the server still waiting for rows, so the next statement run on it failed with "Bulk load data was expected but not sent" in the hands of an unrelated caller. The connection is now poisoned (via `driver.ErrBadConn`) so the pool discards it instead of handing it on.
- **A cancelled context ends `Login.UserMappings`'s per-database scan** (`login.go`): the loop deliberately skips a database it can't read, but a cancellation isn't one of those — every remaining database would fail the same way. It now checks `ctx.Err()` and returns, rather than issuing a doomed query per database.
- `ScriptOptions.IncludeHeaders` and `IncludeIfNotExists` document that they apply to `ScriptTable`/`ScriptDatabase` only — `ScriptView`/`StoredProcedure`/`Function` return the module definition verbatim from `sys.sql_modules` and synthesize no DDL to guard.
- `QuoteLiteral` (`quoting.go`) and `escapeSingle` (`helpers.go`) now document which to reach for, and that neither quotes an *identifier*: one going inside a string literal — `OBJECT_ID`, `DBCC SHOW_STATISTICS`, `fn_listextendedproperty` — needs `QuoteName`/`qualifiedName` applied first, with `escapeSingle` on top. `identifier_quoting_test.go` pins it.
- Doc comments corrected or filled in across `index.go`, `login.go`, `query_store.go`, `schema_user.go`, `security.go`, `server_security.go`, `statistics.go`, `table.go` and `server_config.go`, including `Context`-variant comments on methods that had shipped without one, and `Server.DatabaseByName` now states outright how it differs from `Server.Database` and that the two are not interchangeable.

### Fixed

- **A scripted parameterised write produced a script that couldn't run.** `Database.exec` recorded the statement text with the driver's `@p1`/`@p2` placeholders still in it, so a captured statement pasted into a query editor failed with "Must declare the scalar variable '@p1'". Arguments are now substituted as T-SQL literals (`bindScriptArgs`/`scriptLiteral`, `script.go`). This hit `Index.Rename`, `Database.RenameTable`, `Table.DropColumn`, and `Database.DropTable(cascade=true)` — every parameterised write method, all four reachable from a "Script Changes" action.
- **`ExecProc` scripted as a bare object name.** The driver runs a procedure call as an RPC, so the statement text handed to the chokepoint is just the procedure name — no `EXEC`, no parameters. Under `WithScript` it now renders the real `EXEC` form (`scriptExecProc`, `procedure.go`): inputs as literals, output and in/out parameters as a `DECLARE`d variable followed by `OUTPUT`, typed from what the caller intends to read the value into.
- **`ScriptTable`'s `IncludeIfNotExists` emitted a script that could not parse.** The existence check opened a `BEGIN` block spanning the `CREATE TABLE`, its indexes, and its foreign keys — with `GO` separators inside it. `GO` is a client-side batch break, so the block was split across batches, leaving an unclosed `BEGIN` in the first and a bare `END` in the last. Each statement now carries its own single-statement guard, which is also what SSMS emits.
- **`ScriptTable` scripted a unique constraint as a `CREATE INDEX`**, leaving the constraint itself missing from the generated script. A unique constraint is backed by an index in `sys.indexes` but belongs to the table; it now scripts as the `ALTER TABLE ... ADD CONSTRAINT ... UNIQUE` it really is.
- **`ScriptTable` pasted an index's `type_desc` into the B-tree `CREATE INDEX` form**, producing DDL SQL Server rejects for anything that isn't a plain rowstore index. Clustered and nonclustered columnstore indexes now get their own grammar (no column list, and no `ASC`/`DESC` respectively), and XML/spatial indexes are emitted as a comment naming what was skipped rather than as a statement that cannot run.
- **`ScriptTable`'s existence checks used an unquoted two-part name** (`OBJECT_ID(N'schema.name')`), so a schema or table name containing a `.` resolved to the wrong object or to NULL. Both now go through `qualifiedName`.
- **`Table.FragmentationStats` and `Index.Fragmentation` passed an unquoted name to `OBJECT_ID`** for the same reason — and there the failure is silent and worse: a NULL `object_id` means "every object in the database" to `sys.dm_db_index_physical_stats`, so a table whose name contains a `.` returned plausible fragmentation numbers for the wrong tables instead of an error.
- **`SetDatabaseScopedConfig(..., forSecondary: true)` was a syntax error, always.** `FOR SECONDARY` precedes `SET` — `ALTER DATABASE SCOPED CONFIGURATION FOR SECONDARY SET MAXDOP = PRIMARY` — and the clause was being appended after the assignment instead, which made the `forSecondary` argument unusable outright. Statement building is now split into `buildScopedConfigStatement` so the clause order is asserted without a server.
- **`SetQueryStoreOptions` was rejected outright by SQL Server.** `STALE_QUERY_THRESHOLD_DAYS` is not a top-level `SET QUERY_STORE` option — it is only accepted inside `CLEANUP_POLICY = (...)` — and its presence failed the whole statement, taking every other Query Store option with it.
- **`BackupActionFiles` emitted `BACKUP FILES [db]`**, which is not T-SQL: there is no `FILES` backup verb, only a `BACKUP DATABASE` carrying `FILE =`/`FILEGROUP =` clauses. Both `BuildBackupStatement` and `BuildRestoreStatement` now render the real form, and `BuildRestoreStatement` also maps `BackupActionDifferential` down to `DATABASE` the way the backup side already did.
- **`Table.CheckWhereSyntax` discarded the error it exists to report.** It returned `rows.Close()` bare, so a predicate rejected by the server came back as the driver's own unwrapped error instead of gosmo's `check syntax for <table>` wrapping, unlike every other failure path in the method.

### Dependencies

No dependency changes.

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
