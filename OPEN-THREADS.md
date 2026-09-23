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

## Closing connections leaves a STANDBY database's readers alone

`RestoreOptions.CloseExistingConnections` sets SINGLE_USER only on a database
that is online and not in standby: a STANDBY database refuses the ALTER, and a
refusal in the batch would abort the RESTORE behind it (a RESTORING one refuses
it too, Msg 5052, verified live 2026-09-22). So a log restore over a STANDBY
database with readers connected still fails with "Exclusive access could not
be obtained" — the log-shipping secondary case. `killDatabaseSessionsBatch`
would close them, and is what a Managed Instance already gets; it was not
switched on for standby here without a live run with a parked reader. The
MI KILL form itself has not executed either, since a Managed Instance restores
`FROM URL` only (see above).

## Scripter fidelity: what `ScriptTable` still does not recreate

The 2026-09-22 pass (gossms review plan Q2) made `ScriptTable` keep every
feature listed in `ARCHITECTURE.md` § Scripter. Knowingly still missing, each
of which recreates a *different* table rather than failing:

- **Always Encrypted columns** — no `ENCRYPTED WITH (…)`; the column is
  recreated in plaintext.
- **FILESTREAM** — no `FILESTREAM` column attribute and no `FILESTREAM_ON`;
  `TEXTIMAGE_ON` is not emitted either.
- **Ledger tables** (2022+) and the `generated_always_type` values above 2
  (transaction-id / sequence-number columns): not emitted; `LEDGER = ON` is
  not read.
- **Memory-optimized tables**: no `MEMORY_OPTIMIZED`/`DURABILITY`, and hash
  indexes script as B-trees.
- **Per-partition compression**: an index's compression is its first
  partition's.
- **XML and spatial indexes** — skipped with a comment, as before.
- A disabled **clustered** index is recreated and then disabled, as the
  source is — which takes the replayed table offline, faithfully.

`Parameter.TypeString` shares the `datetime2(0)` fix but not the alias-type
qualification: `sys.parameters`' type is still rendered unqualified.
`PartitionFunction.Boundaries` is split on `,`, so a string boundary
containing one is mis-read.

## Keys `FROM PROVIDER` have never executed

`AsymmetricKeySpec.FromProvider` and `SymmetricKeySpec.FromProvider`
(`cryptographic_provider.go`) build `CREATE … FROM PROVIDER` from the
documented grammar and are pinned by unit tests only. Executing one needs an
EKM provider DLL registered with `CREATE CRYPTOGRAPHIC PROVIDER` and `EKM
provider enabled` switched on, and no test instance has one (win10cli: zero
rows in `sys.cryptographic_providers`, the option at 0, 2026-09-22).

What to check when one exists: whether `OPEN_EXISTING` really accepts no
`ALGORITHM` (the builder omits it when the caller leaves it empty), and
whether a provider symmetric key accepts `IDENTITY_VALUE` — refused here
until seen.

## `ConnectTimeout` does not bound the TCP dial

`ConnectTimeout` is written as the driver's `connection timeout`, which
go-mssqldb applies to the connection's I/O *after* the dial (`tds.go`,
`newTimeoutConn`). The dial itself runs on the driver's `dial timeout`,
15 s per protocol by default, which gosmo does not set. Probed 2026-09-23:
`ConnectTimeout: 500ms` against an unroutable address failed after 15 s. The
field's doc ("the maximum time to wait for the initial connection") promises
more than that. The 2026-09-23 review's S10 fixed only the rounding — a
sub-second value used to become `connection timeout=0`, which the driver
reads as none. Candidate fix: also write `dial timeout` from the same value
unless `ExtraParams` sets it; `ctx` on `Connect` already bounds the whole
call for a caller who needs it now.

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

## `live_keycaps_test.go` leaves its login behind

Seen 2026-09-23: after a full `-run TestLive` on win10cli the login
`gosmo_live_keycaps` was still there. The test's cleanup is an unchecked
`defer db.ExecContext(…, "DROP LOGIN …")`, registered after the scratch
database's drop and so run before it; a DROP LOGIN refused (most likely
because the login still has a session in that database) fails silently. Not
investigated further. The next run's pre-drop removes it, so it only
accumulates as one stray login, but a live run should leave nothing: check
the DROP's error, and kill the login's sessions first.

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
