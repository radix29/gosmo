# Changelog

All notable changes to gosmo are documented here, newest first. This file
starts tracking detail from `v0.0.4` onward; `RELEASE.md` covers the
high-level shape of every release, including the ones before this file
existed.

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
