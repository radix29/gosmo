# Release notes

The current release, in brief. Detail and history are in
[CHANGELOG.md](CHANGELOG.md).

## v0.0.11

Six server-object families arrive whole, plus detach/attach, the Query
Store report views, and permissions answered per securable. Underneath:
every catalog column added after the SQL Server 2016 SP1 floor is now
version-gated instead of assumed.

### New

- Server audits and audit specifications — create, alter, enable/disable,
  drop, running status.
- Logical backup devices, and a `BackupTarget` so the RESTORE-side reads
  take a device as well as a path.
- Credentials as a full family, with cryptographic providers. The secret
  is write-only.
- Endpoints of every protocol, with state changes and on-demand mirroring
  or Service Broker detail.
- Server-scope DDL and logon triggers.
- Asymmetric keys (read only).
- Detach and attach, including the file list read out of a detached
  primary data file.
- Query Store reports: SSMS's seven views, plan list and plan XML,
  force/unforce.
- Column master key rotation for a column encryption key.
- Permission answers at schema, object and column scope.
- `ConnectionOptions.Dialer`, for a proxy or an SSH tunnel.
- `Server.DatabaseFiles(name)` — paths for a database in any state, from
  the server catalog.
- Six more scripting verbs, five more `*Seq` iterators.
- A stated version floor, `MinimumServerVersion`, and the gating layer
  behind it.

### Fixes

- A running Agent job reported idle, and an idle one running.
- Six catalog reads failed outright on an older instance, each naming a
  column it does not have: AG listeners, Query Store options, table
  detail, column master keys, scoped configurations, statistics header.
- Five more used `STRING_AGG`, which is 2017, and so failed on the 2016
  floor.
- A named instance with no port often failed to connect on a dual-stack
  host, reported as "no instance matching".
- Extended properties came back empty for a level the caller left unset.
- A failed forced drop left a database in single-user mode.
- A failed job step reorder could lose a step.
- `Search` missed rows on a case-sensitive collation.
- The default backup path was empty on 2017 and older.
- Scripting a database panicked on a `Server` built without `NewServer`.

### Changes

- **`JobState`'s values are Agent's real encoding** — the constants are
  renumbered, so code comparing against a literal now compares against the
  wrong thing. `JobStateCancelling` and `JobStateRunning` are deprecated
  and go at the next breaking tag.
- `Credential` moved to its own file and carries a server reference; a
  struct literal panics on `Alter` or `Drop`.
- Scripted parameter substitution skips string literals, quoted
  identifiers and comments.
- Job step reordering is one transactional batch.
- The single-user repair after a rename, drop or detach runs on its own
  context.
- `ServerInfo.OSVersion` is documented as `@@VERSION` verbatim, not an OS
  version string.
- Dependencies: `go-mssqldb` v1.11.0, and the Azure identity chain and
  `golang.org/x/crypto` with it.
