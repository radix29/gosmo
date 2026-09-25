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
(`live_script_facets_test.go`). They are read and scripted only:
`CreateTable`'s `ColumnDefinition` still cannot declare a typed xml or a
`vector` column, and `CreateIndexRequest` cannot declare `ORDER (…)`. The
ordered-columnstore gate (major 16) has no instance here and is argued from
the documentation. A `float16` vector needs `PREVIEW_FEATURES = ON` in the
database the script is replayed into, which the table script does not set.

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
  `DISTRIBUTION` (elastic query) are unit-tested only, and
  `sys.external_tables`' 2022+ `rejected_row_location` and `table_options`
  are not read. The data source's database-scoped credential is named, not
  scripted.
- **FileTables** keep the names of their primary key and two unique
  constraints; the defaults, checks and foreign key `AS FILETABLE` adds get
  new generated names.
- **Always Encrypted**: the column master and column encryption keys are
  referenced by name and must exist where the script runs.

Knowingly still missing, each of which recreates a *different* table rather
than failing:

- **Selective XML indexes** (`xml_index_type` 2 and 3) — skipped with a
  comment. Primary and secondary XML indexes and spatial indexes are
  scripted; a selective index's paths (`sys.selective_xml_index_paths`) are
  not read.
- **Natively compiled modules**: a natively compiled module's dependence on
  a memory-optimized table is not scripted.
- A disabled **clustered** index is recreated and then disabled, as the
  source is — which takes the replayed table offline, faithfully.

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
