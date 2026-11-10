# Release notes

The current release, in brief. Detail and history are in
[CHANGELOG.md](CHANGELOG.md).

## v0.2.0

### New

- Whole-database scripting: `ScriptDatabaseObjects` and `ScriptDatabaseObjectList`, dependency-ordered, with ledger tables, extended properties and `ScriptApplicationRole`.
- Space and maintenance: `SpaceReport`, `TableSizes`, `LogSpace`, `LogShrinkFloors`, `CheckDB`, database-wide `FragmentationStats` and `UpdateStatistics`.
- Indexes and statistics: `RebuildAllIndexesWith`, `ReorganizeAllIndexes`, `IndexUsage`, `UpdateWith`, `UpdateStatisticsWith`; indexed views gain `FragmentationStats`.
- Instance reads: `DatabaseSizes`, `InstanceFiles`, `FileVolumes`, `DatabaseAccess`, `StartTime`, `CurrentTime`, `CollationCodePage`, `LastBackups`.
- `TopQueries` and `CachedPlan` for the plan cache; `DecodeDeadlockReport` and `XEvents.ReadFileEvents`.
- `SearchDefinitions`, `SearchColumns`, `ObjectByName`, `ObjectsNamed`, `ReturnType`.
- Agent: `Job.Alerts`, `SearchJobHistory`. Server: `CreateServerRole`, `TraceFlags`, `SetTraceFlag`, configuration `Default`.
- `gosmotest` package: test code that calls gosmo without a server.
- Bulk loads check each value against its column (`BulkColumn.Converter`, `BulkValueError`).
- `ErrDefinitionHidden`, `EntryError`, `Database.HasPermission`, `Capabilities.MasterPermission`, `ScriptCollector.Add`.

### Fixes

- Bulk loads no longer wrap, truncate or mis-encode values silently.
- `ScriptDatabase` could omit `SET CHANGE_TRACKING`.
- Table scripts placed foreign keys before the tables they reference.
- Scripts and dependency order for a login without database-wide `VIEW DEFINITION`.
- Agent reads failed for logins holding only an Agent role.
- DBCC permission denial (Msg 2571) is `PermissionDenied`.

### Changes

- **Breaking:** sequence values (`StartValue`, `Increment`, `MinValue`, `MaxValue`, `RestartWith`, `Restart`, `NextValue`) are `string`, not `int64`.
- A hidden definition now refuses the script (`ErrDefinitionHidden`) instead of omitting it.
- Agent reads use `sysjobs_view` and `sp_help_*` for non-sysadmin logins.
- `ReadLog` falls back to `sp_readerrorlog`.
- `golang.org/x/text` v0.44.0, `x/sync` v0.25.0, `x/sys` v0.50.0.
