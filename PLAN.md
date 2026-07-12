# gosmo Plan

## Working style

This is a spare-time project — no deadlines, no sprints, no committed
velocity. The date below is a target, not a promise. Work happens in
whatever order priorities and available time allow; this document tracks
*what's next*, not *when*.

## Release target

**First usable version: July 2026.**

gosmo's main consumer today is [goSSMS](https://github.com/radix29/gossms)
(see that repo's own `PLAN.md`), which is targeting the same date — the
two releases are effectively coupled: gosmo needs to cover whatever
goSSMS's v1 feature set actually calls into.

## Ongoing practices (no end date)

These continue for the life of the project, release or not:

- Bug fixing, optimizing, and refactoring as issues turn up.
- Triage incoming issues and re-prioritize implementation work as they
  land.
- Keep the `README.md` feature map (`Server`, `Database`, `Table`,
  `Index`, `Login`, dependencies/search/permissions/execution plans,
  `Scripter`, Backup & Restore, Agent Jobs) in sync with the code as
  methods are added — it's the API surface consumers actually read.
- New work follows the conventions already documented in gossms's
  `CLAUDE.md`: one file per SMO object family at the repo root, every
  DB-hitting method as a `Foo`/`FooContext` pair, a matching `FooSeq` in
  `iter.go` for any new collection-returning method, and errors wrapped
  `"gosmo: <verb phrase>: %w"`.

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
  execution-plan work (see its `PLAN.md`) needs a capability gosmo
  doesn't expose yet, add it here rather than working around the gap in
  the TUI layer; that's the intended way the two repos evolve together
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

## Contributing note

Per the existing README: the API is still moving as gossms's real-world
usage shakes out what's actually needed, so PRs aren't being accepted yet
— issues are the right channel until the project reaches a released,
stable state.
