# Open threads

Open work and settled decisions — nothing else. Close an item by deleting it;
add one whenever something is knowingly left undone. `CLAUDE.md` § Conventions
and `ARCHITECTURE.md` describe what *is*; this file is the only place that
records what is knowingly **not** done, and what must be watched.

It exists because gosmo had nowhere to put either: `PLAN.md` was deleted in
`916c114` as no longer relevant to the project's direction, and the tracking
lines it carried had no home afterwards.

## Version support: the policy, and how it is held

The target is **SQL Server 2016 SP1 and later**. SP1 rather than RTM because
`procedure.go` and `scripter_module.go` emit `CREATE OR ALTER` — as does gossms's
`internal/activity/block.go`, pinned there by its own test.

**The standing check is `TestLiveVersionSweep`** (`live_versionsweep_test.go`):
it calls every read gosmo exposes and reports what the server rejects. Run it
on the *oldest* instance available after any query change. A query naming a
column the instance lacks fails the whole read, and `go test ./...` says
nothing.

A sweep of 0 failures is not proof on its own — it passes just as happily if a
read was never reached. When re-verifying, confirm the reads were actually
*called*: the sweep's `call` helper takes a label, and logging it lists every
method swept. Refusals are gates working, not defects: every on-premises major
refuses the Azure-only DMV reads, and major 13 additionally refuses the 2017+
reads outright with `ErrUnsupportedVersion`.

The gates, recorded because the next audit will otherwise re-derive them:

| Column or construct | Held by |
|---|---|
| `STRING_AGG` (2017) — partition functions and schemes, server and database triggers, foreign keys, audit specifications, role members | `sql_agg.go` `jsonList` aggregates with `FOR JSON PATH` (2016, independent of compatibility level) and `encoding/json` decodes it, so no name is split on a separator. No raw `STRING_AGG` and no `FOR XML PATH` aggregate in non-test source. |
| Column Master Keys: `allow_enclave_computations`, `signature` (2019) | `always_encrypted.go`, `colSince(major, SQLServer2019, …)` |
| Query Store options: the 2017 and 2019 columns | `query_store.go`, `colSince` per column |
| `Table.Detail`'s `ledger_type_desc` (2022) | `table.go`, `colSince(…, SQLServer2022, …)` |
| `Statistic.Header`: DBCC returns 10 columns before 2019, 11 after | `statistics.go` binds **by column name**, with the failure named in the comment |

Which instances exist to sweep against, and the sweep's current result, are
environment facts rather than library facts: gossms's `docs/open-threads.md`
§ Version support carries them, and points here for the table above.

## Resource Governor affinity beyond processor group 0: unit-tested only

Since 2026-10-02 the scripter maps a mask in a later processor group through
`Server.Schedulers` (each group's visible-scheduler count; VIEW SERVER STATE,
refused with `ErrUnsupported` naming it). No instance in the estate has more
than one processor group, so that path is covered only by
`TestResourcePoolAffinityBeyondGroupZero`'s fake sizes. Two assumptions are
unconfirmed against a live multi-group host: that ids continue across groups
by visible-scheduler count (VISIBLE OFFLINE included), and that an external
pool's CPU ids number the same way. Replay on the first multi-group instance.

## azidentity: watch for the removal, not the deprecation

azidentity has **deprecated** `UsernamePasswordCredential` (it lacks MFA),
which `AuthEntraPassword` / ROPC uses at `entra.go` — both the options literal
and `NewUsernamePasswordCredential`. The method is **kept**: it is a supported
gosmo auth mode, verified live on Managed Instance, and both sites carry
`//lint:ignore SA1019` with that reason.

**The thread to watch is azidentity *removing* the type**, at which point ROPC
needs a replacement or the mode goes. Nothing warns of that but a failed build
after a dependency bump.

## Backup and restore `TO URL` have never executed

The statements are built and validated, and no execution has ever been
observed: executing one needs a shared access signature credential on the
container, and none exists on the Managed Instance available for testing.

The library-side question to settle when one does: **`WITH INIT` on a URL
device.** Block-blob backup to URL overwrites through `WITH FORMAT`, not
`INIT`, so a server may refuse the statement `backup.go` builds from
`BackupOptions.Init`. It has not been changed blind, because `INIT` is right
for every disk and tape device and a change would alter those too. gossms has
settled its own side the same way — it keeps `Init: true` on a URL device
rather than special-casing one device kind — so a change here waits on a real
execution, not on a preference.

Restore's URL-side cases, and the dialog behaviour around backup history, are
gossms's: `docs/decisions.md` § Azure SQL Managed Instance.

## Scripter fidelity: what `ScriptTable` still does not recreate

The 2026-09-22 pass (gossms review plan Q2) made `ScriptTable` keep every
feature listed in `ARCHITECTURE.md` § Scripter, and the 2026-09-24 pass
(review plan T7) added graph tables and edge constraints, memory-optimized
tables and hash indexes, FILESTREAM and `TEXTIMAGE_ON`.

The 2026-09-24 review pass (gossms review plan W1) added typed xml columns' schema
collections, `vector(n[, float16])` columns, ordered columnstore indexes,
`HISTORY_RETENTION_PERIOD` and a non-default `LOCK_ESCALATION`
(`live_script_facets_test.go`). `ColumnDefinition` declares the typed xml and
`vector` columns and `CreateIndexRequest.ColumnstoreOrder` the `ORDER (…)`
(2026-10-01). The ordered-columnstore gates (clustered major 16,
nonclustered 17) have no major-16 instance here: 16 itself is argued from
the documentation, and only the refusal on 13/14 and acceptance on 17 are
run live. A `float16` vector needs `PREVIEW_FEATURES = ON` in the
database the script is replayed into (Msg 195 otherwise): the table script
names it in a leading comment and deliberately does not set it
(`live_script_float16_test.go`, 2026-10-01). A procedure or function with a
`float16` parameter gets the same comment, naming the parameters from
`sys.parameters` (2026-10-02). A module that uses the type only in its body —
a local variable, a table variable's or `RETURNS` table's column, a `CAST` in
a view — gets it too, unnamed: a lexical scan of the definition that skips
comments, strings and quoted identifiers (`usesFloat16Vector`, 2026-10-02).
A module that spells it only in **dynamic SQL** gets a note of its own
(`buildsFloat16Vector`, 2026-10-08): its CREATE replays with the setting off,
and running it fails with Msg 195 (`TestLiveFloat16DynamicSQLScript`). Any
EXEC admits every literal, so a false positive is possible; a type name split
across concatenated literals is not found.

The 2026-09-24 fix-plan pass (G7–G10) added Always Encrypted columns, ledger
tables, FileTables and external tables. **Refused, not scripted** (an
`ErrUnsupported` error and no script, for every verb), by design rather than
as open work: a ledger history table and a dropped ledger table, which only
the ledger creates. A column dropped from a ledger table is left out of the
script, and the history table and ledger view of the recreated table do not
have it.

What those four kinds still leave out:

- **External tables** are verified live only on Azure SQL Managed Instance —
  no on-premises test instance has PolyBase. `SCHEMA_NAME`, `OBJECT_NAME` and
  `DISTRIBUTION` (elastic query) are unit-tested only, and so are
  `REJECTED_ROW_LOCATION` and `TABLE_OPTIONS` (`sys.external_tables`, read
  from 2022; 2026-10-02) — replay them on the Managed Instance or the first
  PolyBase instance. The data source's database-scoped credential is named, not
  scripted.
- **Always Encrypted**: the column master and column encryption keys are
  referenced by name and must exist where the script runs.

Knowingly still missing, each of which recreates a *different* table rather
than failing:

- A disabled **clustered** index is recreated and then disabled, as the
  source is — which takes the replayed table offline, faithfully.

## Scripter fidelity: what `ScriptDatabase` still leaves out

`ScriptDatabase` emits `CREATE DATABASE [name]` with its containment, file
layout, collation and `WITH FILESTREAM (NON_TRANSACTED_ACCESS,
DIRECTORY_NAME)`, then the recovery model, compatibility level, empty and
read-only filegroups, the options that differ from a new database's
(`AUTO_CLOSE`/`AUTO_SHRINK`/`AUTO_CREATE_STATISTICS`/`AUTO_UPDATE_STATISTICS
[_ASYNC]`, `PAGE_VERIFY`, `TRUSTWORTHY`, `READ_COMMITTED_SNAPSHOT`,
`ALLOW_SNAPSHOT_ISOLATION`, change tracking), and always Query Store (its
default depends on the replaying instance) and the owner. The layout
(2026-10-08, gossms fix plan 1) is every filegroup's files with name,
`FILENAME`, `SIZE`, `MAXSIZE` and `FILEGROWTH`; the options (2026-10-08, fix
plan 2) are read with `Options`, `ChangeTracking` and `QueryStore`.
`TestLiveScriptDatabaseFileLayoutRoundTrips` and
`TestLiveScriptDatabaseOptionsRoundTrip` replay the script under a new name
and compare the catalog (green on 13, 14, 17; FILESTREAM only on 17).
Knowingly left out, each of which a replay silently gets wrong rather than
refuses:

- **`CONTAINMENT = PARTIAL` has never run live**: "contained database
  authentication" is off on all three instances, and the test skips it rather
  than change server configuration. Unit-tested only.
- **The rest of `sys.databases`**: the ANSI/`ARITHABORT`/`QUOTED_IDENTIFIER`
  family, `CURSOR_DEFAULT`, `RECURSIVE_TRIGGERS`, `DB_CHAINING`, `ENABLE_BROKER`,
  `PARAMETERIZATION`, `DELAYED_DURABILITY`, `TARGET_RECOVERY_TIME`,
  accelerated database recovery, ledger, database-scoped configurations,
  `user_access`, read-only and encryption (TDE). Each is one more `onOff`
  line in `writeDatabaseSettings` where `DatabaseOptions` already reads it.
- **Defaults are as shipped**, not the source's `model`: a replay on an
  instance whose `model` was changed gets that `model`'s value for every
  option at its shipped default.
- **A database snapshot** scripts as an ordinary `CREATE DATABASE` over its
  sparse files' paths, not `AS SNAPSHOT OF`.
- **The paths and FILESTREAM directory name are the source's own**: replayed
  on the same instance under the same name after a DROP that is right; as a
  copy, the caller edits them.

## `ScriptTable` is 8–11 round trips — considered, not batched

gossms review plan G15 (2026-10-08) batched `LocalPublications`,
`LocalSubscriptions` and `UserMappingsIn` into one round trip after the
database list (`perDatabaseBatch`). `ScriptTable` was looked at in the same
pass and left alone: it reads through the public `TableByName`,
`scriptOptions`, `Columns`, `Indexes` (two queries), `ForeignKeys`,
`CheckConstraints`, `DataSpace` and, by kind, `EdgeConstraints` and the
schema-bound/referencing reads a DROP needs. Batching means splitting each into
a select list and a scanner and reading them as one multi-result-set batch, as
`Database.catalog` does — every family's scan code moves, for one
user-triggered script per table. Worth doing only if a whole-database script
(every table in a loop) is built; then batch per database, not per table.

## Keys `FROM PROVIDER` have never executed

`CreateAsymmetricKeyRequest.FromProvider` and `CreateSymmetricKeyRequest.FromProvider`
(`cryptographic_provider.go`) build `CREATE … FROM PROVIDER` from the
documented grammar and are pinned by unit tests only. Executing one needs an
EKM provider DLL registered with `CREATE CRYPTOGRAPHIC PROVIDER` and `EKM
provider enabled` switched on, and no test instance has one (win10cli: zero
rows in `sys.cryptographic_providers`, the option at 0, 2026-09-22).

What to check when one exists: whether `OPEN_EXISTING` really accepts no
`ALGORITHM` (the builder omits it when the caller leaves it empty), and
whether a provider symmetric key accepts `IDENTITY_VALUE` — refused here
until seen.

## Entra users and Entra login defaults have never executed

`CreateUserRequest{Kind: UserFromExternalProvider}` (with `ObjectID` and
`DEFAULT_SCHEMA` in one `WITH` list), and the external-login half of
`buildLoginScript` (`DEFAULT_DATABASE` and `DEFAULT_LANGUAGE` in one following
`ALTER LOGIN`), are pinned by unit tests only. Every other user kind and the
SQL/Windows login script were replayed on 13 and 17 (2026-09-24,
`live_user_kinds_test.go`); these need a real directory principal, which none
of the test instances has — `t-qmi-01` is reachable with SQL auth, but no Entra
user or group to name.

What to check when one exists: that `FROM EXTERNAL PROVIDER WITH OBJECT_ID =
…, DEFAULT_SCHEMA = …` parses in that order, and that an Entra login takes
`DEFAULT_LANGUAGE` through `ALTER LOGIN` as it does `DEFAULT_DATABASE`.

## Two login writes have no offline test, and cannot have one

Every write path in the library now has a `WithScript` test pinning the exact
statement it emits — the 2026-09-17 sweep took the zero-coverage count from 86
to 0 — with two exceptions, both in `login.go`:
`Login.MapToDatabase` and `Login.UnmapFromDatabase`. Each reads
the catalog before it writes (`DatabaseByName`, and for the unmap that
database's user mapping on top), and a read is exactly what `WithScript` cannot
serve: nothing ran, so there is nothing to read back. They stay live-only, and
`live_*` is where a regression in them will show — `live_api_pass_test.go`
drives the unmap.

This is the shape `CLAUDE.md` § Script mode already names — a path that reads
to decide what to write does not work under a scripting context. The two here
are not broken by it, because they are not scripted paths; they are simply the
two writes whose statement a test cannot see without a server.

When adding a write, add its case to the matching `script_*_write_test.go`
table (or `drop_rename_test.go` for a one-statement drop) in the same change,
and mutation-check it: swap a parameter name or drop a `dbo` default in the
source and confirm the new case fails. A case built from the same constant the
code uses proves nothing.

## Agent schedules are addressed by id — settled, do not re-raise

Schedule names are not unique in msdb (`ARCHITECTURE.md` § Shared schedules).
`ScheduleByName` refuses a shared name with `ErrAmbiguous`; every write sends
`@schedule_id` when the handle has one, and only a `ScheduleRef` falls back
to the name. Do not "simplify" a schedule write back to `@schedule_name` —
`TestScheduleProceduresAddressByKey` fails on it, and msdb answers Msg 14371
as soon as two schedules share the name.

## The two DDL-trigger files stay near-identical — settled, do not re-raise

`database_trigger.go` and `server_trigger.go` duplicate roughly thirty lines:
`scanDatabaseTrigger`/`scanServerTrigger` are twelve identical lines apart from
the receiver, and the `Enable`/`Disable`/
`setEnabled` block below each differs only in the scope the statement targets
(`ON DATABASE` versus `ON ALL SERVER`) and in which exec helper it reaches
(`db.exec` versus `server.exec`).

Reviewed 2026-09-18 and **deliberately left duplicated.** Unifying it needs
either generics over two receivers with different `db`/`server` fields, or a
shared struct both embed — and the second changes the shape of two exported
types for no caller's benefit. Neither pays for itself against thirty lines
that have not drifted.

What was done instead: each `scanX` now carries a comment naming the other as
its twin, so a change to one prompts a look at the other. Keep those two
comments in step if either file moves. A future duplicate-code scan will find
this pair again; this entry is the answer.
