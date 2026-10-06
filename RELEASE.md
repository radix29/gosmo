# Release notes

The current release, in brief. Detail and history are in
[CHANGELOG.md](CHANGELOG.md).

## v0.0.16

### New

- Extended Events: sessions, targets, live reads, templates, scripting.
- Resource Governor: pools, workload groups, external pools, classifier.
- Database Mail: accounts, profiles, security, queues, log, `SendMail`.
- `Server.InTransaction` — several writes as one transaction.
- Restore planning: set numbers, file relocation, `RestoreOptions.FromHeader`.
- Server activity counters and tempdb usage reads.
- CLR procedures, functions and triggers script from the catalog.
- Table-valued functions and aggregates in `Catalog`.
- `ScheduleByID`, `AgentMailSettings`, linked-server catalog reads.
- `ClassifyRefusal`, `IsPermissionDenied`, `IsAlreadyExists`, `ErrAmbiguous`.
- Collation-aware `SameName` / `NameKey`; more quoting helpers.

### Fixes

- Case-sensitive databases raised collation conflicts and returned the wrong object.
- `DROP TABLE` did not name the schema-bound modules in its way.
- Restore moved files to names the server could not create.
- Captured scripts could keep a password containing a quote.
- Writes that read a value back could be retried.
- Several statements of one write could half-apply.

### Changes

- **Breaking:** file, filegroup and role-member writes are methods on their handles.
- **Breaking:** Agent step, category, alert and schedule writes follow the handle shape; attach by `*Schedule`.
- **Breaking:** `Credential.Alter` takes `CredentialOptions`.
- **Breaking:** `BuildBackupStatement` is a `Server` method.
- **Breaking:** `ObjectKey` joins with a NUL byte.
- By-name lookups return the catalog's spelling.
- `Close` cancels every statement in flight.
- Dependencies: `shopspring/decimal` v1.5.0.
