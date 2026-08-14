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
  `defer rows.Close()`. Both it and every `rows.Scan` wrap with the *same*
  message the function's query error uses — a failure mid-iteration is
  otherwise indistinguishable from any other, and comes back to the caller as
  a naked `context deadline exceeded` naming nothing. The rule is per exported
  entry point, not per statement: the shared scan helpers (`scanColumns`,
  `scanExtProps`, `scanEffectivePermissions`, `securityPredicates`,
  `indexColumnsContext`, `execWithProgress`) return bare errors on purpose,
  because only their callers know which operation to name, and each caller
  wraps what they return.
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
- **Never query inside a `rows.Next()` loop.** `Database.query` pins its own
  pooled connection and issues its own `USE`, so a per-row lookup costs two
  round trips and an acquisition per row *while the outer connection is still
  held* — the shape that exhausts a pool, not merely a slow one. Fetch the
  child rows for the whole object in one query with no parent-id predicate,
  ordered by the parent id first, and group them in Go.
  `Table.IndexesContext` is the worked example (2026-08-14: 42 round trips
  across 21 connections for a 20-index table, now 2).
- **One file per subject area** (`table.go`, `index.go`, `security.go`, …),
  with `helpers.go` for cross-file helpers and `types.go` for shared enums.
- **Script mode.** `WithScript` collects statements instead of executing
  them, and `Scripting(ctx)` reports whether a context is one. A code path
  that *reads* to decide what to write does not work under it — the standing
  shape is a `Create*` reading its own object back by name after an `EXEC`
  that was only collected. Every such method returns a name-only handle
  under `Scripting(ctx)` instead; a new one must do the same.
  - A write that mirrors its change back onto the receiver (`Rename` setting
    `.Name`, `Enable` setting `.IsEnabled`) must go through `setIfApplied`,
    never a direct assignment. Under `WithScript` nothing ran, so a direct
    assignment leaves the object claiming state the server doesn't have and
    the next call built from it targets an object that doesn't exist.
  - A statement captured with bound parameters is substituted to literals
    (`bindScriptArgs`) — a captured statement is pasted into a query editor,
    where nothing binds `@p1`.
- `Server.Database(name)` and `Server.DatabaseByName(name)` both exist on
  purpose and are not interchangeable — the lightweight handle is the only
  one that works under a `WithScript`-derived context. The same pairing
  exists for logins and for all four Agent object families (`Server.Job` vs
  `JobByName`, and Alert/Operator/Schedule alike). Their doc comments in
  `server.go` are the authority; `go doc gosmo.Server.Database`.

## Release

`RELEASE.md` and `CHANGELOG.md` cover the release process; don't edit either
as part of a feature or fix unless asked. Note that gossms cannot cut a
release while gosmo's `HEAD` is untagged — tag and push here first.
