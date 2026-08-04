# gosmo examples

Nine runnable programs. Each is a `package main` you run directly:

```sh
export MSSQL_SERVER="localhost:1433"
export MSSQL_USER="sa"
export MSSQL_PASSWORD="YourPassword"
export MSSQL_TRUST_CERT="true"      # self-signed dev cert

go run ./examples            # the guided tour
go run ./examples/backup     # one subject at a time
```

| Program | Covers |
| --- | --- |
| [`.`](main.go) | The guided tour: connect, inspect the instance, build a database with schemas, tables, columns, indexes, a sequence, a synonym and a procedure, script it, read its space usage, drop it |
| [`backup/`](backup/main.go) | `BACKUP`/`RESTORE`, full + differential + log to one device, progress callbacks, `RESTORE HEADERONLY`/`FILELISTONLY`/`VERIFYONLY`, msdb backup history, relocating files on restore |
| [`bulkcopy/`](bulkcopy/main.go) | `Database.BulkInsert` from a slice, a generator, and a streaming CSV; `SliceRows`; batch/lock/order options; what happens when the source fails mid-load |
| [`diagnostic/`](diagnostic/main.go) | `AsSQLError`, `IsRetryable`, `ExecProc` with In/Out/InOut parameters and the return status, estimated vs actual execution plans, object search, dependency graphs, the bulk catalog snapshot, memory/CPU/session/error-log reads |
| [`iterators/`](iterators/main.go) | The `*Seq` API — what it does (deferred fetch, whole-collection) and what it does not (streaming), plus cancellation behaviour |
| [`jobs/`](jobs/main.go) | SQL Server Agent: categories, operators, a job with branching steps, job-owned and shared schedules, running it, job history, event alerts |
| [`maintain/`](maintain/main.go) | Files and filegroups, index fragmentation, rebuild/reorganize, statistics with header/histogram/density vector, space usage, database options and scoped configurations, change tracking, Query Store |
| [`scripting/`](scripting/main.go) | `Scripter` and its options; `WithScript`, which collects the statements a set of writes *would* run instead of executing them |
| [`security/`](security/main.go) | Logins, database users, server and database roles, object/schema/database/server permissions read from both directions |

## Environment

Every program reads the same variables, handled in
[`internal/demo`](internal/demo/demo.go):

| Variable | Meaning |
| --- | --- |
| `MSSQL_SERVER` | `host[:port]` or `host\instance` (default `localhost:1433`) |
| `MSSQL_DATABASE` | initial database (default `master`) |
| `MSSQL_AUTH` | `sql` (default), `windows`, `msi`, `msi-user`, `sp`, `sp-cert`, `token`, `default`, `azcli`, `azd`, `password`, `interactive`, `devicecode` |
| `MSSQL_USER` | login, Windows UPN, or client ID |
| `MSSQL_PASSWORD` | password or client secret |
| `MSSQL_ENCRYPT` | `""`, `true`, `false`, or `strict` |
| `MSSQL_TRUST_CERT` | `true` skips TLS certificate validation |

Azure methods take the usual `AZURE_TENANT_ID` / `AZURE_CLIENT_ID` /
`AZURE_CLIENT_SECRET` / `AZURE_CLIENT_CERT_PATH` variables; `demo.Connect`'s
doc comment lists which each one needs.

## What they touch

Each program creates its own throwaway database — `GoSMOBackupDemo`,
`GoSMOSecurityDemo`, … — and drops it at the end, along with any login, job,
operator, category, schedule or alert it made. Nothing that already exists on
the instance is modified; a program that fails partway still runs its cleanup,
because `demo.Must` panics rather than exiting and every `main` defers
`demo.Exit()` first.

Two things they cannot clean up: `backup/` leaves its `.bak` file behind (SQL
Server has no "delete file" verb), and `jobs/` needs SQL Server Agent running
for the run/history section — it says so and skips it otherwise.

## test_db.sql

[`test_db.sql`](test_db.sql) is unrelated to the programs above: it builds
three realistic sample databases (`RetailShop`, `HealthClinic`,
`HRManagement`) with views, procedures, logins and users, for poking at gosmo
against something more interesting than an empty instance. Run it with
`sqlcmd` or SSMS, not with `go run`.
