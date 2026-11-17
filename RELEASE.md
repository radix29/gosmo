# Release notes

The current release, in brief. Detail and history are in
[CHANGELOG.md](CHANGELOG.md).

## v0.3.0

### New

- `Database.Principals`: users, roles and application roles in one round trip.
- `Database.BackupRefusal`: why SQL Server would refuse a backup.
- `DiscardConn` and `RepairError` for sessions and disable windows left in doubt.
- `JobState.Running`, `SecurableClass.Permission`, `Schema.IsSystem`.
- Fabric Warehouse: `ExternalFunctions`, `CREATE TABLE` scripting, statistics reads.
- Table and view scripts include user-created statistics.

### Fixes

- Connection retargeting compares database names under the server collation.
- Failed connections are discarded, not pooled; failed repairs are reported.
- Columnstore `ORDER` columns must match their key column's case exactly.
- `ScriptDatabaseObjects` scripts DDL triggers last; its warnings are comment-safe.
- Bulk loads round `float` to `decimal` ties away from zero, as `CAST` does.
- Fabric key constraints no longer emit `ASC`/`DESC`; `Dependents` works on Fabric.

### Changes

- **Breaking:** full-text calls move to `d.FullText()`, Service Broker calls to `d.ServiceBroker()`.
- **Breaking:** `Enable`/`Disable` removed (except `Index`, `ResourceGovernor`); use `SetEnabled(ctx, on)`.
- **Breaking:** `AddToServerRole`, `RemoveFromServerRole`, `AddToRole`, `RemoveFromRole` removed; use the role's `AddMember`/`RemoveMember`.
- **Breaking:** statistics updates take `StatisticsUpdateOptions`; `UpdateWith` variants removed.
- `PlanRestore` follows the live recovery fork.
- `SearchDefinitions` compares ordinally: about 5× faster, no longer accent-insensitive.
- Whole-database scripting reads extended properties in bulk.
- Go 1.27.4; `golang.org/x/crypto` v0.59.0.
