# Release notes

The current release, in brief. Detail and history are in
[CHANGELOG.md](CHANGELOG.md).

## v0.0.13

The Programmability and External Resources folders arrive whole — types,
rules, defaults, assemblies, plan guides, external data sources, file formats
and libraries — along with database snapshots and the table sub-folders.
Underneath them, Entra sign-in and the Browser probe stopped repeating
themselves once per connection, and a `Database` read costs one round trip
instead of two.

### New

- Types: alias, table, CLR and system, plus XML schema collections — read,
  drop, transfer, rename.
- Rules and defaults — the two deprecated families, read-only.
- CLR assemblies, with their files and modules.
- Plan guides — read, enable, disable, drop.
- External data sources, external file formats and external libraries.
- Database snapshots at server scope: create, restore, drop, and every
  snapshot of a given source.
- Table kinds — System, FileTable, External and Graph as their own listings,
  with `TableKindsPresent()` to say which exist at all.
- `Database.DiskUsage()` — the SSMS Disk Usage report's numbers.
- Azure per-database resource stats and governance limits, the database-scoped
  twins of `v0.0.12`'s instance ones.
- Per-securable database capabilities for assemblies, types and XML schema
  collections.
- Eleven more scripting verbs, covering every new family above.
- `ConnectionOptions.ExtraParams` — driver parameters with no field of their
  own; one a field controls is refused, never merged.
- `ConnectionOptions.ConnectionString(maskSecrets)` — the DSN, without
  dialling.
- Fourteen more `*Seq` iterators (112 now), and `AuthMethod.String()`.

### Fixes

- `CreateDatabase` failed outright when given a log file and no data file.
- A cancelled or failed write left auditing switched off, the one thing the
  disable window exists to prevent.
- An interrupted `EXECUTE AS` read left a pooled connection impersonating, so
  the next caller to get it failed with Msg 596.
- A forced drop or rename could not work on a Managed Instance, which rejects
  `SET SINGLE_USER`.
- `ParseServerAddress` misread IPv6 literals, taking the last group for a port.
- An Entra `User` already carrying a tenant had `TenantID` appended again.
- `StopAt` silently discarded sub-second precision from a point-in-time
  restore.

### Changes

- **Entra credentials are built by gosmo, once per identity, not once per
  connection** — one browser sign-in or device code for a whole pool. Share
  one via `ConnectionOptions.EntraCache`, warm it with `Warm`, and put the
  device code where a user can see it with `DeviceCodePrompt`.
- **A `Database` read is one round trip**, the `USE` batched with the query —
  41 ms per read against a Managed Instance.
- **A Browser reply is cached for two minutes**, rather than re-probed for
  every new pooled connection; a failed connection evicts it.
- Under `WithScript`, a nil `[]byte` scripts as `NULL` and an empty one as
  `0x`; both were `0x00`.
- `AuthEntraIntegrated` is documented as what it is — the
  `AuthEntraDefault` chain, not Windows SSO — and `AuthEntraPassword` as
  unable to satisfy MFA.
- Four new version gates, checked against real instances: the graph columns at
  2017, the PolyBase v2 and file-format columns at 2019.
- `azcore` and `azidentity` are direct dependencies now.
- Docs split: `README.md` is a short summary, the API map and reference moved
  to `ARCHITECTURE.md`, and `PLAN.md` is gone.
