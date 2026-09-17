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
`procedure.go` and `scripter.go` emit `CREATE OR ALTER` — as does gossms's
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
| `STRING_AGG` (2017) — partition functions and schemes, server and database triggers, foreign keys, audit specifications | `sql_agg.go` `commaList` renders the `FOR XML PATH`/`STUFF` form, valid from 2008. No raw `STRING_AGG` in non-test source. |
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
