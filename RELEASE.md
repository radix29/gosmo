# Release notes

The current release, in brief. Detail and history are in
[CHANGELOG.md](CHANGELOG.md).

## v0.1.2

### New

- `Database.CreateRole` with `CreateRoleRequest`.
- Application roles: `ApplicationRoles`, `ApplicationRoleByName`, `ApplicationRoleRef`, `CreateApplicationRole`, and `Rename`, `SetDefaultSchema`, `ChangePassword`, `Drop` on the handle.
- `Sequence.Alter` with `SequenceChanges`; `CreateSequenceRequest.Precision` for decimal and numeric.
- Capability probes for `CREATE ROLE`, `CREATE SYNONYM`, `CREATE SEQUENCE` and `ALTER ANY APPLICATION ROLE`.

### Fixes

- Nothing user-visible.

### Changes

- `ScriptDatabaseSecurity` reads application roles through `ApplicationRoles`.
- Database and server trigger scripts share one builder.
- Rule and default scans share their row scanners.
- `azcore` v1.24.0, `go.yaml.in/yaml` v3.0.6.
