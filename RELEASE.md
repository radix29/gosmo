# Release notes

The current release, in brief. Detail and history are in
[CHANGELOG.md](CHANGELOG.md).

## v0.3.0

### New

- `Database.Principals`: users, database roles and application roles in one round trip.
- `JobState.Running`, `SecurableClass.Permission`, `Schema.IsSystem`.

### Fixes

- Connection retargeting compares database names under the server collation, not case-blind.
- Columnstore `ORDER` columns must match their key column's case exactly.
- `ScriptDatabaseObjects` scripts database DDL triggers after the permissions, and its warnings are comment-safe.

### Changes

- **Breaking:** full-text calls move to `d.FullText()` (`Catalogs`, `Stoplists`, `SearchPropertyLists`, `Indexes`, `Create…`, `…ByName`, `…Ref`).
- **Breaking:** Service Broker calls move to `d.ServiceBroker()` (`Queues`, `Services`, `Priorities`, `MessageTypes`, `Contracts`, `Routes`, `RemoteServiceBindings`, `QueueMessageCounts`, `QueueMonitors`).
- `SearchDefinitions` compares ordinally after case folding: about 5× faster; no longer accent-insensitive.
- Go 1.27.4; `golang.org/x/crypto` v0.59.0.
