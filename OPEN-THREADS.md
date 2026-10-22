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
nothing. Its reflective half (`versionsweep_reflect_test.go`) runs each call
under `WithScript`, so a ctx-only write missing from `sweepSkip` fails as
"wrote under the sweep" instead of running against the instance.

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
EXEC admits every literal, so a false positive is possible. Literals joined by
`+` are folded first (2026-10-13), so a type name split across them is found;
one split around a variable or function call is not.

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
DIRECTORY_NAME), LEDGER = ON`, then the recovery model, compatibility level,
empty and read-only filegroups, **every** `SET` option explicitly whatever its
value, as SSMS does (2026-10-13, so the replaying instance's `model` no longer
leaks in), change tracking when on, Query Store, then under `USE [db]` the TDE
key and the scoped configurations away from their default, `SET ENCRYPTION
ON`, the owner, and last the user access and `READ_ONLY`/`READ_WRITE`. `ACCELERATED_DATABASE_RECOVERY` is
written for a 2019+ source only, with `PERSISTENT_VERSION_STORE_FILEGROUP`
when ADR is on and its version store is off `PRIMARY` (read from
`sys.dm_tran_persistent_version_store_stats` inside the database; a refused
read leaves a comment). A snapshot scripts as `CREATE DATABASE … ON
(…) AS SNAPSHOT OF [source]` and nothing more. The options are read from
inside the database: one `AUTO_CLOSE` has closed reports NULL at server
scope for its collation, `TARGET_RECOVERY_TIME`, `DELAYED_DURABILITY` and
ADR — `TestLiveScriptDatabaseReadsAClosedDatabaseFromInside` calls both
reads on a closed database and fails at server scope.
`TestLiveScriptDatabaseFileLayoutRoundTrips`,
`TestLiveScriptDatabaseOptionsRoundTrip`,
`TestLiveScriptDatabaseSnapshotRoundTrips`,
`TestLiveScriptDatabaseContainmentRoundTrips` and
`TestLiveScriptDatabaseInDatabaseSettingsRoundTrip` and
`TestLiveScriptDatabaseKeyLeftByEncryptionOff` replay the script and
compare the catalog (green on 13, 14, 17; FILESTREAM, ADR and LEDGER only on
17). The containment test turns "contained database authentication" on and
back off; the TDE one leaves tempdb encrypted until the instance restarts.
Knowingly left out, each of which a replay silently gets wrong rather than
refuses:

- **TDE**: the key is read whether encryption is on or not — a key left
  behind by `SET ENCRYPTION OFF` is created without `SET ENCRYPTION ON`. Its
  DMV needs VIEW SERVER STATE; a refusal is silent for an unencrypted
  database, so a key left behind is not scripted without it. The protecting certificate or
  asymmetric key is named, not scripted — it must exist, with its private
  key, where the script runs. Without VIEW SERVER STATE the script says so in
  a comment and leaves encryption out. `TRIPLE_DES_3KEY` and an asymmetric-key
  protector are unit-tested only.
- **Scoped configurations** are written only where they differ from the
  default (`is_value_default`; on 2016, which lacks it, the four documented
  defaults — any other row is skipped). A value that is not a number, 0/1 or
  a bare keyword (Azure's `LEDGER_DIGEST_STORAGE_ENDPOINT` URL) gets a
  comment, not a statement. The value's sql_variant base type decides its
  spelling (`bit` → OFF/ON, an integer type → the number), the same on 2016.
- **ADR's version store filegroup on 2019 and 2022 is untested**: no such
  instance exists here, and the DMV's permission differs by version (VIEW
  SERVER STATE before 2022, VIEW DATABASE PERFORMANCE STATE after).
- **`ScriptDatabase` once left out `SET CHANGE_TRACKING` on 14** although the
  source had it on — `TestLiveScriptDatabaseOptionsRoundTrip`, 1 in 30 runs
  (1 in 18 earlier, undiagnosed); the next 60 were green. Ruled out: the
  server-scope `sys.change_tracking_databases` read missing a closed
  `AUTO_CLOSE` database's row, or missing it while the database opens,
  closes or turns `READ_ONLY` (3,000 reads, none missed). Diagnostics in
  place: the test now names each differing field, and when the script lacks
  change tracking it re-reads `ChangeTracking` and logs the database's state.
- **The paths and FILESTREAM directory name are the source's own**: replayed
  on the same instance under the same name after a DROP that is right; as a
  copy, the caller edits them.

## Scripter: extended properties beyond tables and modules

`ScriptOptions.ExtendedProperties` restates the properties of every object
a single-object script creates in a table's, a module's, a database's, a
schema's, a user's, a role's, an alias or table type's (and its columns'),
a sequence's, a synonym's, an XML schema collection's, a partition
function's and scheme's and a DDL trigger's script, and of the one index or
constraint `ScriptIndex`, `ScriptCheckConstraint` and `ScriptForeignKey`
create (2026-10-14).

Still left out, not yet asked for: a CLR type's, rule's, default's,
assembly's, plan guide's, Service Broker object's, filegroup's and file's.
Each is an `extendedPropertiesAt` call at its class and level (a CLR type is
class 6 under SCHEMA/TYPE, as an alias type is).

By design: a table script leaves out its triggers' properties (it does not
create the triggers — `ScriptTrigger` carries them), and a FileTable's
system-defined constraints' and indexes' (AS FILETABLE names those afresh).
A `sql_variant` base type without a case in `extPropType` is a comment, not
a statement — none known to exist.

## `ScriptTable` is 8–11 round trips — considered, not batched

gossms review plan G15 (2026-10-08) batched `LocalPublications`,
`LocalSubscriptions` and `UserMappingsIn` into one round trip after the
database list (`perDatabaseBatch`). `ScriptTable` was looked at in the same
pass and left alone: it reads through the public `TableByName`,
`scriptOptions`, `Columns`, `Indexes` (two queries), `ForeignKeys`,
`CheckConstraints`, `DataSpace` and, by kind, `EdgeConstraints` and the
schema-bound/referencing reads a DROP needs — plus, since 2026-10-13, one
for the extended properties. Batching means splitting each into
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

## Idle pooled connections camp in the last database read — not investigated

`Database.query`/`queryRow` send `USE [db]; …` on a pooled connection
(`useBatch`), and the connection goes back to the pool still in `db`: the
driver resets it only on the next checkout. Until then it holds a shared
database lock, so *someone else's* `RESTORE`, `SET OFFLINE` (no ROLLBACK) or
`SET SINGLE_USER` fails "Exclusive access could not be obtained", and an
`AUTO_CLOSE` database never closes. `ReleaseIdleConnections` covers only this
`Server`'s own exclusive writes.

Mitigated, not fixed: gossms sets `ConnMaxIdleTime` to 60 s (gossms
`internal/db/connection.go`, `connMaxIdleTime`), against the 5-minute default
here; the idle connection is gone 60 s after the read (measured on SQL2017,
2026-10-22) at ~25 ms of re-login on the next read.

The fix would be to run database-scoped reads as `EXEC @proc` with `@proc =
N'[db].sys.sp_executesql'`, so the session never leaves its initial database —
and with it most of `releaseIdle`'s reason to exist. Before anyone tries it,
prove it against three rules in `CLAUDE.md` § Conventions:

- the Msg 924 compile-time resolution of a literal `[db].sys.sp_executesql`
  (hence the variable, as `perDatabaseExec` does);
- the plan-cache database-id trap ("Read another database's catalog under
  `USE [db]`") — check whether a cached plan under `[db].sys.sp_executesql`
  keeps its database id across a drop/recreate the way an ad-hoc `[db].sys.…`
  plan does;
- parameter collation: inside `[db].sys.sp_executesql` parameters bind in the
  target's collation, which may make the `COLLATE DATABASE_DEFAULT` rule
  unnecessary there, or break a comparison that relies on it.

It touches every database-scoped read, so `TestLiveVersionSweep` on 13 and
the collation tests are the minimum gate.

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
`SetEnabled` block below each differs only in the scope the statement targets
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
