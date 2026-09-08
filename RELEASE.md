# Release notes

The current release, in brief. Detail and history are in
[CHANGELOG.md](CHANGELOG.md).

## v0.0.12

Azure SQL Managed Instance is supported properly rather than incidentally,
and the database half of `v0.0.11`'s Security folder work arrives: DDL
triggers, audit specifications and scoped credentials, all at database
scope. Capabilities can now withhold on a recorded DENY.

### New

- Database-scope DDL triggers — list, enable, disable, drop, script.
- Database audit specifications, including per-securable actions.
- Database-scoped credentials. The secret is write-only.
- `Database.ObjectTriggers(schema, name)` — a view's INSTEAD OF triggers,
  which nothing could read before.
- Azure instance resources: the 15-second resource history, the resource
  governor's fixed limits, and the engine's OS job object.
- `EngineEdition` constants, with `ServerInfo.IsAzure()`.
- Backup and restore to Azure Storage: `TO URL`/`FROM URL` chosen per
  device, `URLTarget`, `IsBackupURL`, and a `Credential` option.
- Explicit-DENY capability blocks: `DeniedOnLogin`, `DeniedOnServerRole`,
  `DeniedOnEndpoint`, `DeniedOnDatabase`, `DeniedOnPrincipal`.
- Availability-group scope capabilities.
- `FileGroup.Type` and `IsFileStream()`, which decide what a file added to
  the group becomes.
- Three more scripting verbs, two more `*Seq` iterators (98 now).

### Fixes

- A NULL in msdb's backup history killed the whole read — the entire
  Database Properties General page on a Managed Instance.
- A scripted column encryption key rotation mutated the handle, so a
  pre-flight check read the wrong value count.
- The server filesystem reads took the pre-2017 path on Azure, losing
  size and modification time for no reason.

### Changes

- **An Azure engine edition is gated as newest, not as the version it
  reports.** A Managed Instance says 12.0.2000.8 while running an 18.x
  engine, which put it below every version gate — silently, returning
  nothing for columns it has. `ServerInfo.VersionMajor` is unchanged.
- `Server.AgentInfo` answers on Azure, where `sys.dm_server_services` is
  empty, from Agent's own sessions and `msdb.dbo.syssessions`.
- `EndpointSpec.EncryptionAlgorithm` is validated against the sub-clause's
  grammar; a value the server would reject now fails client-side.
- **Removed:** `JobStateCancelling` and `JobStateRunning`, deprecated in
  `v0.0.11`. Use `JobStateExecuting` and
  `JobStatePerformingCompletionActions`.
- Dependencies: the `golang.org/x` chain; toolchain go1.27.1.
