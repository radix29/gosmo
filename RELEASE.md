# Release notes

The current release, in brief. Detail and history are in
[CHANGELOG.md](CHANGELOG.md).

## v0.1.1

### New

- `Database.Shrink` and `DatabaseFileInfo.Shrink` with `ShrinkResult`.
- `PlanRestore`: the full, differential and log sets for a point in time.
- `Scripter.ScriptDatabaseSecurity`; indexed-view indexes and statistics on `View`.
- `XML_COMPRESSION` and `STATISTICS_INCREMENTAL` in index scripts.
- `IsTransient`; `Trigger.Enable`/`Disable`; `TriggerByName`, `ViewByName`, `UserDefinedFunctionByName`, `SessionByID`.
- Typed enums for database, file and principal states and types, and availability-group states.

### Fixes

- The pool follows a database that is dropped, detached or renamed.
- Pooled reads and writes no longer hold a lock that blocks `RESTORE` or `SET OFFLINE`.
- Deadlocks and Azure transient errors are retried; `NextValue` is not.
- A session returns to its home database after `InTransaction`.
- Agent and `Mail.Send` values are bound as parameters.
- A refused database no longer stops a login-to-user mapping scan.

### Changes

- **Breaking:** `Database.QueryStore()`, `Server.Mail()` and `XEvents()` handles replace flat methods.
- **Breaking:** one listing per family taking `ObjectFilter` or `TableQuery`; `…Filtered` methods removed.
- **Breaking:** `…Spec`, `…Settings`, `…Options` request types renamed `…Definition` or `…Changes`.
- **Breaking:** agent per-property setters and `Ptr` removed; `ReadLog` takes `LogSearch`.
- Documentation counts and class diagrams brought up to date.
