# gosmo

SQLServer Management Objects Library
A Go library that mimics **Microsoft SQL Server Management Objects (SMO)** — without WMI, COM, or Windows-only dependencies.

```
go get github.com/radix29/gosmo
```

> **Go version note:** The module requires Go 1.27

## Supported SQL Server versions

**SQL Server 2016 SP1 (13.0.4001) and later**, on Windows and on Linux.
`gosmo.MinimumServerVersion` states the floor in code.

The floor is SP1 rather than 2016 RTM for one reason: `CREATE OR ALTER`
arrived in 13.0.4001, and `Database.CreateStoredProcedure` emits it
unconditionally (`procedure.go`) — the only statement gosmo builds that
2016 RTM cannot parse. Every catalog read gosmo makes would run on RTM.
Supporting it means rewriting that one statement as
`IF EXISTS … ALTER … ELSE CREATE`; that is a deliberate decision, not an
oversight, and `TestOnlyKnownSitesEmitCreateOrAlter` fails if a second
such statement appears without one.

`Scripter`'s module scripting is not a second site: `alterModuleDefinition`
recognises a `CREATE OR ALTER` that the *server's own* stored definition
already contains and passes it through unchanged. That text is the
author's, not gosmo's.

Azure SQL Managed Instance is supported and is version-gated separately.
It reports a frozen `ProductVersion` — 12.0.2000.8, SQL Server 2014 — while
running an 18.x engine that has every catalog column gosmo gates on 2016
through 2022, so `ServerInfo.IsAzure()` routes it past `VersionMajor` and
into the "treat as newest" branch. `Info().VersionMajor` keeps the 12 the
server actually said, for callers that display a version.

Columns and syntax added after the floor are gated rather than assumed, and
every gate is pinned three ways — see
[`ARCHITECTURE.md`](ARCHITECTURE.md#version-gating).

---

## Quick start

```go
import "github.com/radix29/gosmo"

srv, err := gosmo.Connect(gosmo.ConnectionOptions{
    Server:                 "localhost:1433",
    User:                   "sa",
    Password:               "YourPassword",
    TrustServerCertificate: true,
})
if err != nil { log.Fatal(err) }
defer srv.Close()

fmt.Println(srv.Info().ProductVersion)
```

`Connect` opens the pool itself. Where the pool is not gosmo's to open — one
shared with the rest of an application, a driver wrapped for tracing or
retries, or a fake driver in a test — `gosmo.NewServer(ctx, db)` wraps an
existing `*sql.DB` and loads the same metadata. It is the inverse of
`srv.DB()`, and ownership passes to the `Server`, whose `Close` closes the
pool.

---

## What it covers

Every object family SSMS shows, read and — where it makes sense — written:

- **Server** — info, configuration, logins and server roles, permissions and
  the connected login's capabilities, sessions, Database Mail, linked
  servers, the error log, the server filesystem, memory and processor DMVs.
- **Database** — files and filegroups, `ALTER DATABASE` options, scoped
  configurations, space and disk usage, change tracking, users, roles,
  schemas, permissions down to column scope, detach/attach, snapshots.
- **Objects** — tables (with system, FileTable, external and graph tables as
  their own listings), columns, indexes, keys, constraints, statistics,
  partitions, views, procedures, functions, sequences, synonyms, triggers at
  all three scopes.
- **Programmability** — user-defined types, XML schema collections, rules,
  defaults, CLR assemblies, plan guides, external data sources, file formats
  and libraries.
- **Backup & restore** — to disk, to a logical device, or to Azure Storage,
  with headers, history, file lists and progress callbacks.
- **SQL Server Agent** — jobs, steps, schedules, alerts, operators,
  categories.
- **Always On** — availability groups, replicas, databases, listeners, the
  mirroring endpoints beneath them and the certificates that authenticate
  them.
- **Security** — audits and audit specifications at both scopes, credentials
  at both scopes, endpoints, certificates, asymmetric keys, column
  encryption.
- **Query Store** — its options and all seven report views.
- **Azure SQL Managed Instance** — supported, not merely reachable: its own
  version gating, resource history and governance limits at instance and
  database scope, and backup to URL.
- **Scripting** — a `Scripter` that generates CREATE/ALTER/DROP DDL for any
  of the above, and `WithScript`, which collects the statements a write
  *would* run instead of running them.

Every collection method has a `FooSeq(ctx)` iterator beside it, and every
method that touches the database comes as a `Foo`/`FooContext` pair.

The full API map — nineteen Mermaid class diagrams in [`diagram/`](diagram/)
under a master map, and a feature map giving gosmo's name for each SMO one —
is in [`ARCHITECTURE.md`](ARCHITECTURE.md).

---

## Authentication

`ConnectionOptions.Auth` selects the method: SQL Server logins, Windows and
Kerberos, and thirteen Microsoft Entra ID flows — managed identity, service
principal, the Azure and Azure Developer CLI credentials, interactive,
device code, on-behalf-of and Azure Pipelines OIDC among them. gosmo builds
the Entra credential itself rather than leaving it to the driver, which
would sign in once per *physical connection*; an `EntraCache` signs in once
per identity and a whole pool shares it.

Required fields are checked before anything is dialled, and the error names
the gosmo field rather than a driver parameter the caller never wrote. Each
method's fields, and the Kerberos, proxy-dialer and connection-string
options beside them, are in
[`ARCHITECTURE.md`](ARCHITECTURE.md#authentication).

---

## Security

Passwords are escaped and never spliced in raw; every keyword or literal DDL
cannot parameterize is validated by shape or allowlist before it reaches a
statement; identifier and literal quoting goes through one implementation
wrapping the driver's own. The detail is in
[`ARCHITECTURE.md`](ARCHITECTURE.md#security).

---

## Documentation

| Document | |
| --- | --- |
| [`ARCHITECTURE.md`](ARCHITECTURE.md) | The API map, the feature map, errors, authentication and the connection internals |
| [`diagram/`](diagram/) | The class map — `00-map.mmd` plus nineteen Mermaid class diagrams, one per group of types |
| [`RELEASE.md`](RELEASE.md) | What changed in the current release |
| [`CHANGELOG.md`](CHANGELOG.md) | The full history, from `v0.0.4` |
| [`examples/README.md`](examples/README.md) | What each runnable example covers |

---

## Packages

| Path        | Purpose                                                    |
| ----------- | ---------------------------------------------------------- |
| `/`         | All SMO types and logic                                    |
| `examples/` | Nine runnable programs — see [`examples/README.md`](examples/README.md) |

---

## Running the examples

```
export MSSQL_SERVER="localhost:1433"
export MSSQL_USER="sa"
export MSSQL_PASSWORD="YourPassword"
export MSSQL_TRUST_CERT="true"     # self-signed dev cert

go run ./examples                  # guided tour of the whole library
```

Eight more programs go deeper on one subject each:

| Program | Covers |
| --- | --- |
| `go run ./examples/backup` | `BACKUP`/`RESTORE`, backup headers and history, progress callbacks, relocating files |
| `go run ./examples/bulkcopy` | `BulkInsert` from a slice, a generator, and a streaming CSV |
| `go run ./examples/diagnostic` | `AsSQLError`, `IsRetryable`, `ExecProc`, execution plans, search, dependencies, DMV reads |
| `go run ./examples/iterators` | The `*Seq` API and what its deferred-fetch semantics do and don't buy you |
| `go run ./examples/jobs` | SQL Server Agent jobs, steps, schedules, operators, alerts |
| `go run ./examples/maintain` | Files, fragmentation, index rebuilds, statistics, Query Store, change tracking |
| `go run ./examples/scripting` | The `Scripter`, and `WithScript`'s collect-instead-of-execute mode |
| `go run ./examples/security` | Logins, users, roles, and permissions from both directions |

Each creates its own throwaway database and drops it afterwards; nothing
already on the instance is modified. Authentication and the full environment
variable list are documented in [`examples/README.md`](examples/README.md).

---

## Features intentionally excluded (require WMI / COM / OS APIs)

- Hardware enumeration (disk, NIC, CPU details beyond what `sys.dm_os_sys_info` provides)
- SQL Server service start/stop/restart
- Performance counters via Windows PDH
- SQL Server Browser service interaction — enumerating instances, reading
  its configuration. The dual-stack Browser *dialer* is not an exception: it
  fixes how the driver's own port lookup reaches the service, and asks it
  nothing gosmo does not already need in order to connect.
- Windows Event Log reading
- Registry reads through a Windows API. `xp_instance_regread`, which the
  server itself runs, is not one — it is how `Info().DefaultBackupPath` is
  recovered on instances where `SERVERPROPERTY` does not report it — but
  nothing here opens a registry from the client side.
- WMI and performance-condition SQL Server Agent alerts — these are listed
  by `srv.Alerts()` but not creatable or editable, since they depend on a
  WMI provider or Windows performance counters. `Alert.IsEventAlert()` and
  `srv.EventAlerts()` identify the manageable subset.
- Multi-server Agent administration (master/target servers) — jobs are
  created as `LOCAL`, enlisted on `(local)`

All of the above require WMI or Windows-only APIs and are out of scope for a cross-platform Go library.

---

## Contributing

The codebase is currently unstable and going through regular refactoring,
so I'm not accepting pull requests at this time — please open an issue
instead. I'll start accepting PRs once the project reaches a released,
more stable state. In the near future I'm planning to update the project
regularly.


