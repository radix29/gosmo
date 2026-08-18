# Release history

High-level, release-to-release summary of what gosmo does at each tag —
what changed in spirit, not the full diff. For the itemized, per-symbol
detail behind each release from `v0.0.4` onward, see `CHANGELOG.md`.

## v0.0.9

An Always On release, and a sweep over how failures are reported. gosmo
could see everything about a SQL Server instance except its high-availability
configuration; availability groups, their replicas, databases and listeners
are now readable and writable, along with the two things setting one up
needs and gosmo also lacked — certificates that can move between instances
without touching either host's filesystem, and the database mirroring
endpoint replicas ship log through. Alongside that: the error log is
readable, the server's own filesystem is browsable, absence is now a
sentinel a caller can test for rather than a message to match on, and the
`Drop`/`Rename` gaps across the object families are filled in.

### New

- **Always On availability groups**, end to end. Read the group, its
  replicas, the per-database synchronization state (queue sizes, rates,
  secondary lag, last sent/received/hardened/redone/commit times) and the
  listeners with their IP configurations; write every group and replica
  setting SSMS exposes, add and remove replicas, databases and listeners,
  create and drop a group, join a replica to one, and fail over — planned or
  forced with data loss. A cluster-wide catalog view and a `sys.dm_hadr_*`
  DMV do not say the same thing: the catalog agrees on every replica, the
  DMVs describe only what the local instance can see, so a secondary reports
  nothing about databases it does not host. `PrimaryReplicaServerName` and
  `IsLocalPrimary` are there so a caller can tell where it is and follow the
  primary.
- **Certificates and the database master key**, including the pair that
  moves a certificate between instances with **no filesystem access on
  either host** — `Certificate.Encoded` and `CreateCertificate`'s
  `FromBinary`. The documented route is `BACKUP CERTIFICATE` to a file,
  which a client library cannot do. Only the public certificate crosses the
  wire, which is what makes it safe over an ordinary connection and enough
  for mirroring endpoints.
- **The database mirroring endpoint** — read, create, start, stop, drop, and
  grant `CONNECT` on it. It is Always On's transport, and an instance can
  have at most one however many groups use it, so a second group over the
  same pair of instances reuses the first one's.
- **The error log**, both families: enumerate the archived files with their
  sizes and last-written times, and read one. `ErrorLogType` is the argument
  `xp_readerrorlog` itself takes, so the Agent log goes through the same two
  methods rather than a second pair. Cycling the log is here too.
- **The server's own filesystem** — directory listing, fixed drives, and an
  existence check. Every path is interpreted by the *server*, which is
  routinely a different machine with different path conventions from the
  process calling gosmo; that is why these exist rather than a caller using
  `os.ReadDir`.
- **`ErrNotFound`.** Every by-name lookup that reports absence as an error
  now wraps it, so "this object does not exist" is `errors.Is`-testable
  rather than a message to match on — the distinction a caller needs before
  it creates an object it never established was missing. No message text
  changed. The two lookups that report absence differently on purpose are
  named in the sentinel's own doc comment.
- **`Drop` and `Rename`, filled in** where an object family had one but not
  the other: views, functions, triggers, sequences, synonyms, database and
  server roles, table constraints, statistics, and renaming a database or
  any `sp_rename`-able schema-scoped object.
- A lightweight `db.Table(schema, name)` handle, matching the ones
  `Database`, `Login` and the Agent families already had — the form that
  works under `WithScript`.
- `ServerInfo.Platform` ("Windows" or "Linux"), read from `@@VERSION` so it
  is populated on pre-2017 instances too.
- Seven new `*Seq` iterators, one per collection added here; live suites for
  Always On, for the not-found classification and for the filesystem
  fallback; and statement-pinning suites that assert the *whole* T-SQL every
  write emits, each case feeding a quote-hostile name through every
  parameter that reaches the statement text.

### Fixes

- **`Table.Indexes` could exhaust a connection pool.** It fetched each
  index's columns inside the loop over the indexes, and every one of those
  queries pins a pooled connection of its own — a table with 20 indexes cost
  42 round trips across 21 connections, with the outer one held throughout.
  Now 2.
- **An empty schema addressed the wrong object.** `[].[name]` is what the
  naive form emits, and `OBJECT_ID` resolves it to NULL, so the caller got
  an empty result set rather than an error.
- **`Login.UserMappings` reported a short list as success.** Its skip exists
  to pass over a database it cannot read, but it also swallowed a failure
  that struck partway through one whose rows were already in the result.
- `CreateUser` refuses an empty login name rather than emitting
  `FOR LOGIN []`, which the server rejects naming a login the caller never
  typed.

### Changes

- **A `DROP` method no longer emits `IF EXISTS`.** Half of them carried it
  and half did not, so the same gesture in a caller's UI reported two
  different things about the same situation. Dropping something that isn't
  there is now uniformly the server's own error; a caller wanting the
  idempotent form ignores it, which is a decision this package cannot make
  for it. The generated *scripts* keep `IF EXISTS` — they exist to be
  re-run.
- **Errors say what was being attempted, everywhere.** `rows.Err()` and
  every `rows.Scan` now wrap with the same message their function's query
  error uses, across some 35 files. A failure mid-iteration used to reach
  the caller as a naked `context deadline exceeded` naming nothing.
- **A restore statement is laid out over several lines.** One that relocates
  files carries a `MOVE` clause per database file, each holding two full
  paths; on one line, a caller scripting it for review sees every `MOVE` off
  the right edge of the editor, which reads as the clauses being missing.
- **The filesystem enumeration's version gate now fails toward
  `xp_dirtree`**, which exists everywhere, instead of toward the DMV, which
  does not exist before SQL Server 2017. Visible only on an instance whose
  version is unknown, which now gets entries without sizes or timestamps
  rather than an error.
- `v0.0.8`'s second, identical LIKE escaper is gone; there is one again.

## v0.0.8

A permissions release. gosmo could grant, deny and revoke, but only in the
plainest form of each: no `WITH GRANT OPTION`, no `CASCADE`, nothing
column-level, and no way to answer the question a permissions dialog is
really for — *what can this principal actually do?* All of that is here,
along with the securable search and view-column read a picker built on it
needs. The rest of the release is msdb and scripting accuracy: a job step
that couldn't report when it last ran or clear its output file, and a
scripted `EXEC` whose output parameters were declared as a type SQL Server
refuses to convert.

### New

- The full GRANT/DENY/REVOKE grammar, via a `PermissionOptions` and a
  `...WithOptions` form of every permission method at every scope:
  `WITH GRANT OPTION`, `CASCADE`, and `REVOKE GRANT OPTION FOR` — the
  downgrade back to a plain grant. A modifier a verb has no form for is
  refused, not silently dropped.
- Column-level permissions: read them per object or per principal, and
  grant, deny or revoke them across several columns in one statement.
  `ColumnPermissionNames()` is the (short) catalog of what SQL Server
  actually accepts on a column.
- Effective permissions — SSMS's Effective tab — for a database, an object,
  a schema, or the server, with role membership, ownership, inheritance
  and `DENY` resolved by the server rather than re-derived here.
- `db.FindSecurables(...)` searches schemas, tables and views in one query,
  for a permissions picker on a database too big to list whole.
- `db.ObjectColumns(schema, name)` reads the columns of a table *or a
  view*; `Table.Columns` reaches tables only, and a view's columns carry
  permissions of their own.
- `srv.BackupFileListForSet(device, n)` reads the file list of one
  particular backup set, for a device that backups were appended to.
- `JobStep` reports when it last ran (`LastRunDate`) and how long that took
  as a real duration (`LastRunElapsed`) — the raw field is msdb's `HHMMSS`
  integer, which is not a count of seconds.
- Seven new `*Seq` iterators, one per collection added here, and live
  verification tests for scripted `EXEC` and for the scripted-write state
  guard.

### Fixes

- **A scripted `EXEC` declared output parameters SQL Server refuses.**
  Every `sql.Null*` destination and both `UniqueIdentifier` forms were
  declared `SQL_VARIANT`, so running the generated script failed with
  "Implicit conversion from data type sql_variant to int is not allowed" —
  and `sql.Null*` is the ordinary way to receive a nullable output value.
- **A retry waited out an expired deadline.** `context.DeadlineExceeded`
  implements `net.Error`, so `IsRetryable` read a caller's own timeout as a
  transient network failure and tried twice more.
- **Blanking a job step's output file did nothing** while the `JobStep`
  reported it as cleared: the parameter was omitted when empty, and msdb
  reads an omitted parameter as "keep". Its database field is the opposite
  case and is now treated as such.
- **A job step never reported its last run date** — the time half of msdb's
  date/time pair wasn't being read, so there was nothing to pair the date
  with.

### Changes

- **Breaking:** `Table.AddColumn` and `Table.DropColumn` are removed.
  `AlterColumn` remains.
- The twelve plain grant/deny/revoke methods are now one-line delegations to
  their `WithOptions` counterparts, so one renderer and one set of error
  strings serve both. A test written against the pre-delegation code pins
  the exact statement and error each of them produced, so this is a
  refactor with no behaviour change by construction.
- `Database`'s five state setters and `ConfigurationOption.SetValue` go
  through `setIfApplied` — the last direct assignments `v0.0.7`'s sweep
  missed, each of which left its object claiming state a scripted write
  never applied.
- `srv.BackupFileList` now says it reads the first set on the device, and
  is the zero-`fileNumber` case of the new method.

## v0.0.7

Finishes `v0.0.6`'s context-everywhere work on the one part of the public
API it missed — the iterators — and puts both scripting paths (`Scripter`
and `WithScript`) through the scrutiny they hadn't had: between them they
were producing scripts that couldn't parse, couldn't run, or silently
recreated the wrong kind of object.

### New

- `Scripting(ctx)` reports whether a context came from `WithScript`, so a
  caller mirroring writes into its own state can tell "recorded" from
  "actually applied".
- No-I/O handles for Agent objects — `srv.Alert/Job/Operator/Schedule(name)`
  — matching the existing `srv.Database`/`srv.Login` ones. These are the
  only usable form under `WithScript`, where a lookup by name would go to
  the server for an object that was never created.
- Whole-database space and row counts in one round trip:
  `db.TableSpaceUsedAll()` and `db.TableRowCounts()`, keyed by object id,
  instead of a query per table.
- File and filegroup backups and restores (`Files`/`FileGroups`), and
  restoring a specific backup set from a device (`FileNumber`).
- Clustered columnstore indexes are a recognized index type, with
  `IndexType.IsColumnStore()` covering both columnstore forms; an index
  type gosmo has no constant for now reports the server's own text
  instead of nothing.
- Statistics sampling percentages are validated before they reach the
  server.
- Eight subject-specific runnable examples alongside the guided tour —
  backup, bulk copy, diagnostics, iterators, Agent jobs, maintenance,
  scripting, and security — each on its own throwaway database.

### Fixes

- **Scripted writes produced scripts that couldn't be run.** A
  parameterised write was captured with the driver's `@p1` placeholders
  still in it; `ExecProc` was captured as a bare procedure name with no
  `EXEC` and no parameters. Both now render real, runnable T-SQL.
- **`ScriptTable`, four ways.** Its `IF NOT EXISTS` wrapper opened a
  `BEGIN` block spanning `GO` separators, so the script it produced could
  not parse at all; unique constraints were scripted as `CREATE INDEX`,
  leaving the constraint missing; columnstore, XML and spatial indexes
  were pasted into the B-tree `CREATE INDEX` form, which SQL Server
  rejects for all three; and its existence checks used an unquoted
  two-part name.
- **Fragmentation reads returned numbers for the wrong tables.** A table
  name containing a `.` reached `OBJECT_ID` unquoted and resolved to
  NULL — which `sys.dm_db_index_physical_stats` reads as "every object in
  the database", so the call succeeded with plausible, wrong results.
- **`SetDatabaseScopedConfig`'s `forSecondary` argument never worked** —
  `FOR SECONDARY` precedes `SET`, and appending it after the assignment
  is a syntax error.
- **`SetQueryStoreOptions` was rejected outright**, taking every Query
  Store option with it: `STALE_QUERY_THRESHOLD_DAYS` is only accepted
  inside `CLEANUP_POLICY`, not as a top-level option.
- **File/filegroup backups emitted `BACKUP FILES`**, which is not T-SQL —
  there is no such verb, only `BACKUP DATABASE` with `FILE =`/`FILEGROUP
  =` clauses.
- `Table.CheckWhereSyntax` now wraps the error it exists to report, like
  every other failure in the package.

### Changes

- **Breaking:** every `*Seq()` iterator now takes a `context.Context`
  (`db.TableSeq(ctx)`), running on the matching `FooContext` method.
  They previously used `context.Background()`, leaving the whole
  iterator API uncancellable.
- A write made under `WithScript` no longer mirrors its change back onto
  the object it was called on — nothing ran, so the server still holds
  the old value, and the next call built from that object targeted
  something that doesn't exist.
- An abandoned `BulkInsert` discards its connection instead of returning
  it to the pool mid-load, where the next unrelated caller inherited the
  failure.
- `CreateIndex` refuses a clustered columnstore index (it takes no key
  columns, so it doesn't fit that method's statement) and
  `SetIncludedColumns` refuses either columnstore form, rather than each
  quietly producing a rowstore index instead.
- A cancelled context ends `Login.UserMappings`'s per-database scan
  instead of being treated as one more unreachable database.
- Documentation: `QuoteLiteral`/`escapeSingle` now say which to reach for
  and that neither quotes an identifier; `ScriptOptions` says which
  script methods its flags apply to; `DatabaseByName` says outright how
  it differs from `Database`.

## v0.0.6

Two themes: SQL Server Agent grows from "jobs only" into the whole Agent
node SSMS shows — alerts, operators, shared schedules, and categories —
and the connection layer underneath every read in the package gets
rebuilt after a pooled-connection leak turned out to be able to wedge a
long-running application outright.

- SQL Server Agent, completed: alerts (create/edit/enable/drop, error-
  number or severity triggers, database scoping, response job, operator
  notifications), operators (create/edit/enable/drop, plus the
  "referenced by" direction — every alert and job that notifies one),
  shared schedules (create/edit/enable/drop, attach to and detach from
  jobs, and a one-line English description of a frequency), job/alert/
  operator categories, and Agent's own run state so a caller can say
  "Agent is stopped" rather than surfacing an error.
- Agent jobs, rounded out: rename, description, category, owner, start
  step, auto-delete condition, and completion-email operator are all
  writable now; a job step can be edited in place or deleted; and job
  history is readable across every job at once, not just per job.
- **A connection leak fixed.** Every rows-returning database read
  permanently consumed one connection from the pool — the connection each
  read pins to run its `USE` was never returned. An application that
  caps `MaxOpenConns` stopped working entirely after that many reads;
  one that doesn't grew its connection count without bound. Alongside it:
  single-row reads were retrying transient failures in a way that could
  never actually catch one, server-level reads weren't retrying at all,
  and `IsRetryable` now recognizes the raw network/TDS-stream failures
  the driver itself treats as fatal to a connection. Connections also
  no longer sit in the pool indefinitely once idle (new
  `ConnMaxIdleTime`, defaulting to 5 minutes), which is what let a
  connection dropped by a firewall or load balancer look usable until
  something tried it.
- Server-scope `GRANT`/`DENY`/`REVOKE` worked only from a connection that
  happened to be sitting in `master` — a real engine restriction gosmo
  now handles itself rather than failing silently.
- Index and statistics administration to match SSMS's own property
  pages: index options, lock granularity, rename, rebuild with fill
  factor/compression, included-column changes, storage and fragmentation
  detail; and the full `DBCC SHOW_STATISTICS` output (header, density
  vector, histogram) as typed results.
- Server roles gained the administration surface database roles already
  had — rename, change owner, typed membership, add/remove members.
- Validation added wherever a value has to be spliced into DDL rather
  than parameterized (recovery models, data types, backup actions,
  partition boundary literals, Query Store mode keywords, index data
  compression).
- 40 new `*Seq()` iterators and 9 previously missing `FooContext` twins
  close out the `Foo`/`FooContext`/`FooSeq` convention across the
  package, and `database.go` was split by object family (`view.go`,
  `procedure.go`, `function.go`, `database_role.go`).

## v0.0.5

Closes out remaining gaps in the SMO surface's write/administration
coverage — role and user management, schema-scoped permissions, database
scoped configuration, Query Store, and explicit CREATE DATABASE file
placement — alongside new read-only diagnostics (table/database catalog
snapshots, backup-set inspection, disk/processor topology), and two real
bug fixes that had gone unreleased since v0.0.4:

- Object/role/user administration: rename and change owner for database
  roles, rename/remap login/default schema for database users, role
  membership with principal type, restrict database access
  (multi/single/restricted user), take a database offline/online, and
  place `CREATE DATABASE`'s primary/log files explicitly instead of
  always at the server default.
- Security: schema-scoped GRANT/DENY/REVOKE, "every securable one
  principal holds" lookups, sorted permission-name catalogs for every
  scope (object/schema/database/server) to back a Permissions-page
  picker, and database scoped configuration read/write.
- Diagnostics: bulk table/view catalog snapshots (user and `sys` schema)
  for consumers like IntelliSense, per-table space usage and metadata
  detail, Query Store configuration and state, backup-set/file-list
  inspection and verification (`RESTORE HEADERONLY`/`FILELISTONLY`/
  `VERIFYONLY`), and disk volume / CPU-NUMA topology.
- Two real bug fixes: `CreateLogin`/`ChangePassword` no longer send a
  cleartext password mislabeled `HASHED` (previously either rejected
  outright or silently created an unusable login); SQL Server Agent job
  "last run outcome/duration" and running-state now read from the right
  catalog view instead of always reporting their defaults.
- Differential backup support, and a live restore progress callback
  matching the one backup already had.
- Every previously context-less write/read method across the package now
  has its `FooContext` twin, and 14 new `*Seq()` iterators round out the
  collection methods added since v0.0.4.
- gosmo's own `Version` is no longer hand-edited — it now resolves
  automatically the same way `Commit`/`Date` already did.

## v0.0.4

Rounds out the SMO surface with the object management, security, and
diagnostics pages SSMS exposes that gosmo didn't cover yet, plus two new
cross-cutting capabilities:

- Database administration: files/filegroups, `ALTER DATABASE` options,
  change tracking, ownership.
- Security: object- and database-scoped permissions, server-level
  permissions and credentials, login status/rename/password-policy/
  user-mapping management, cross-platform Kerberos for `AuthWindows`.
- Diagnostics: estimated/actual execution plans, object dependencies,
  object search, structured SQL errors, server memory stats.
- Two new cross-cutting mechanisms: `WithScript` (capture pending writes
  as SQL instead of running them) and automatic retry of idempotent
  reads.
- Bulk copy and stored-procedure execution (with output parameters and
  return status).
- Connection handling: named-instance addresses, per-connection
  access-token refresh, session-init SQL.

## v0.0.1 – v0.0.3

Earlier releases predate this file and the detailed `CHANGELOG.md`; the
project's git history was consolidated before `v0.0.4`'s work began, so
those releases aren't itemized individually here. At a high level, by
`v0.0.3` gosmo covered the core SMO object model this library is built
around: connecting and authenticating (SQL Server, Windows, and the
Entra ID methods), enumerating and managing servers, databases, tables
and their children (columns, indexes, foreign keys, checks, statistics,
partitions), logins and roles, views/procedures/functions, backup and
restore, SQL Agent jobs, and DDL scripting via `Scripter` — the baseline
`v0.0.4` builds on.
