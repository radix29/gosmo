# Release history

High-level, release-to-release summary of what gosmo does at each tag —
what changed in spirit, not the full diff. For the itemized, per-symbol
detail behind each release from `v0.0.4` onward, see `CHANGELOG.md`.

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
