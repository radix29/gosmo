# gosmo Plan

## Working style

This is a spare-time project — no deadlines, no sprints, no committed
velocity. Work happens in whatever order priorities and available time
allow; this document tracks *what's next*, not *when*.

gosmo's main consumer is [goSSMS](https://github.com/radix29/gossms), and the
two evolve together: gosmo needs to cover whatever goSSMS actually calls into.
What goSSMS still has open is in that repo's `docs/open-threads.md`.

## Ongoing practices (no end date)

These continue for the life of the project, release or not:

- Bug fixing, optimizing, and refactoring as issues turn up.
- Triage incoming issues and re-prioritize implementation work as they
  land.
- Keep the `README.md` feature map (`Server`, `Database`, `Table`,
  `Index`, Statistics, `Login`, dependencies/search/permissions/execution
  plans, `Scripter`, Backup & Restore, SQL Server Agent, Always On,
  certificates, endpoints, error log, server filesystem) in sync with the
  code as methods are added — it's the API surface consumers actually
  read. The class diagrams in the same file need the same treatment, and
  `gosmo.mermaid` is those diagrams concatenated in order, kept
  byte-identical to them.
  - **The class map is five diagrams, not one, and has to stay that way.**
    Mermaid refuses to render a diagram whose text (comments stripped)
    exceeds 50,000 characters — it draws an error box in place of the whole
    thing, on GitHub included. The single diagram reached 52,533 during
    `v0.0.9` and had to be split. When a new area is added, give it its own
    diagram rather than growing one past ~45,000; an edge that crosses areas
    is drawn in the diagram of the area it points *into*.
- Keep the nine programs under `examples/` compiling and honest as the API
  moves — they're the only place the library is exercised end to end, and
  `examples/README.md` indexes what each one covers.
- New work follows the conventions in this repo's own `CLAUDE.md`
  § Conventions: one file per SMO object family at the repo root, every
  DB-hitting method as a `Foo`/`FooContext` pair, a matching `FooSeq` in
  `iter.go` for any new collection-returning method (taking a
  `context.Context`, since `v0.0.7`), and errors wrapped
  `"gosmo: <verb phrase>: %w"`. A new write method that mirrors its change
  onto the receiver goes through `setIfApplied`, and a new `Create*` returns
  a name-only handle under `Scripting(ctx)` rather than reading its object
  back. A new GRANT/DENY/REVOKE method takes a `PermissionOptions` and
  renders through `permissionStmt` (`permission_options.go`), with any plain
  form of it delegating there rather than building a second statement of its
  own.

## Next up

- **Missing SMO surface area** — go through real SQL Server Management
  Objects (SMO) API coverage and add what's genuinely missing from
  gosmo's equivalent object families (`table.go`, `index.go`,
  `security.go`, `server.go`, etc.), matching the existing
  method-pair/iterator conventions rather than inventing new shapes.
- **New functionality beyond SMO** — where SMO itself is awkward or
  incomplete, add capabilities that make gosmo genuinely easier to use
  than SMO, not just a port of it.
- **Driven by goSSMS's needs** — when goSSMS's property-dialog and
  execution-plan work needs a capability gosmo
  doesn't expose yet, add it here rather than working around the gap in
  the TUI layer; that's the intended way the two repos evolve together.
  `v0.0.9`'s Always On work is the worked example, and the largest so far:
  goSSMS wanted an Always On node, which needed the availability-group
  layer, the mirroring endpoint underneath it and the certificate exchange
  that authenticates that — three subject areas, all added here rather than
  assembled out of raw queries in the TUI
  (`replace github.com/radix29/gosmo => ../gosmo` in gossms's `go.mod`
  for local dev against unreleased changes, tag-and-bump once merged).

## Non-goals

Carried from `README.md`'s "Features intentionally excluded" section —
these require WMI/COM/Windows-only APIs and are permanently out of scope
for a cross-platform, pure-Go library:

- Hardware enumeration (disk, NIC, CPU details beyond
  `sys.dm_os_sys_info`).
- SQL Server service start/stop/restart.
- Performance counters via Windows PDH.
- SQL Server Browser service interaction.
- Windows Event Log reading.
- Registry reads for SQL Server configuration outside
  `sys.configurations`.
- Creating or editing WMI and performance-condition SQL Server Agent
  alerts (they're still listed; `Alert.IsEventAlert` marks the subset
  gosmo can manage).
- Multi-server Agent administration (master/target servers).

## Contributing note

Per the existing README: the API is still moving as gossms's real-world
usage shakes out what's actually needed, so PRs aren't being accepted yet
— issues are the right channel until the project reaches a released,
stable state.
