# Release history

High-level, release-to-release summary of what gosmo does at each tag —
what changed in spirit, not the full diff. For the itemized, per-symbol
detail behind each release from `v0.0.4` onward, see `CHANGELOG.md`.

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
