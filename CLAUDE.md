# CLAUDE.md

Context for Claude Code sessions on **gosmo**.

## What this is

gosmo is a Go library that mimics Microsoft SQL Server Management Objects
(SMO) without WMI, COM, or any Windows-only dependency. It is pure Go, no
CGO, and talks to SQL Server through `github.com/microsoft/go-mssqldb`.

- Module: `github.com/radix29/gosmo` — https://github.com/radix29/gosmo
- `README.md` carries the full API map (as Mermaid class diagrams).
- Requires Go 1.26.

## This is a library, not gossms's back end

The author also writes **goSSMS** (https://github.com/radix29/gossms), a
terminal SSMS reimplementation that is gosmo's main consumer and is developed
in the same sitting — the sibling layout is `~/go/gosmo` and `~/go/gossms`,
and gossms's `go.mod` normally has an active `replace
github.com/radix29/gosmo => ../gosmo`.

**That does not make gossms the definition of gosmo's API.** gosmo is a
published, general-purpose library with users beyond gossms.

- **Never remove or narrow a capability because gossms doesn't call it.**
  "No callers in gossms" is not evidence of dead code. This covers whole
  files, exported methods, exported types and their fields, and struct
  fields only some paths populate. The `*Seq` iterators in `iter.go` are the
  standing example: 75 exported methods, zero gossms callers, all
  deliberately kept.
- When an audit turns up something unused, the allowed moves are: make it
  faster, make its doc comment accurate about what it actually does, or add
  a test that pins it. Removal, or replacing a general form with the narrow
  one gossms happens to need, is not one of them — raise it instead.
- Optimisation must be behaviour-preserving at the API surface: same
  signature, same results, same errors. A faster implementation that changes
  what a caller observes is a breaking change wearing a performance hat.
- Adding capability for gossms is encouraged — that's the intended
  direction. Design it as a library feature, not as a gossms shim.

## Build & verify

```
go build ./...    # build
go test ./...     # test
gofmt -w .        # format in place
go vet ./...      # vet
```

Plain `go` toolchain only, no Makefile. `go test ./...` runs against fakes
(`captureConn`, `fakeQueryConn`) and needs no server. Anything touching real
SQL behaviour should also be exercised against a live instance — connection
details are deliberately not in the repo; ask for them. Create throwaway
databases/logins, exercise the write path, drop them; never mutate
pre-existing objects.

Build and test **inside this repo** before relying on a change from gossms —
a gossms-side build only compiles the packages it imports.

## Conventions

- **Method pairs.** Every method that touches the database comes in two
  forms: `Foo(...)` delegating to `FooContext(ctx, ...)`. Accessors that
  only read already-fetched struct state, and the `*Seq` iterators (which
  take a `ctx` directly), are the exceptions.
- **Errors** wrap with `%w` and are prefixed `gosmo: ` plus what was being
  attempted — `fmt.Errorf("gosmo: drop statistic %q: %w", st.Name, err)`.
- **`rows.Err()` is always checked**, and every `query` is followed by
  `defer rows.Close()`.
- **Quoting.** See `quoting.go`'s doc comments, which are the authority:
  `QuoteName`/`qualifiedName` bracket-quote an *identifier*; `QuoteLiteral`
  produces a whole string literal; the unexported `escapeSingle`
  (`helpers.go`) escapes for a literal whose quotes are already in the
  caller's format string — the common shape here. An identifier that ends up
  *inside* a string literal (`OBJECT_ID`, `DBCC SHOW_STATISTICS`,
  `fn_listextendedproperty`) needs bracket-quoting first and `escapeSingle`
  on top: `escapeSingle(t.FullName())`. Getting this wrong is not cosmetic —
  a name containing `.` resolves to the wrong object or to NULL, and a NULL
  `object_id` means "every object in the database" to
  `sys.dm_db_index_physical_stats`, so the wrong form returns plausible
  stats for the wrong tables instead of failing. `identifier_quoting_test.go`
  pins it. Prefer a query parameter over any of this where the server
  accepts one.
- **One file per subject area** (`table.go`, `index.go`, `security.go`, …),
  with `helpers.go` for cross-file helpers and `types.go` for shared enums.
- **Script mode.** `WithScript` collects statements instead of executing
  them. A code path that *reads* to decide what to write does not work under
  it — see gossms's `docs/open-threads.md` for the standing example
  (`AttachSchedule` resolving a schedule by name that `CreateSchedule` only
  collected).
- `Server.Database(name)` returns a lightweight handle that queries nothing
  (`State`/`RecoveryModel`/`Collation`/`CompatibilityLevel` stay
  zero-valued); `Server.DatabaseByName(name)` queries `sys.databases` and
  populates them. Both exist on purpose. The lightweight one is the only one
  that works under a `WithScript`-derived context.

## Release

`RELEASE.md` and `CHANGELOG.md` cover the release process; don't edit either
as part of a feature or fix unless asked. Note that gossms cannot cut a
release while gosmo's `HEAD` is untagged — tag and push here first.
