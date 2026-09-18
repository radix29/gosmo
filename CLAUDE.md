# CLAUDE.md

Context for Claude Code sessions on **gosmo**.

## What this is

gosmo is a Go library that mimics Microsoft SQL Server Management Objects
(SMO) without WMI, COM, or any Windows-only dependency. It is pure Go, no
CGO, and talks to SQL Server through `github.com/microsoft/go-mssqldb`.

- Module: `github.com/radix29/gosmo` — https://github.com/radix29/gosmo
- `ARCHITECTURE.md` carries the full API map (as Mermaid class diagrams), the
  feature map, the error and authentication reference, and the connection
  internals. `README.md` is a short summary and points there.
- Requires Go 1.27.

Read what the task touches: `ARCHITECTURE.md` for the API map and the feature
map, `quoting.go`'s doc comments for anything that builds an identifier or
literal, `server.go`/`login.go`'s doc comments for the handle-vs-lookup
method pairs. A one-file fix needs none of it. `ARCHITECTURE.md` § Maintaining
this document is the authority on editing the class map — read it before
touching a diagram. The diagrams themselves are `diagram/*.mmd`; `ARCHITECTURE.md`
inlines only `diagram/00-map.mmd` and links the rest.

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
  standing example: 112 exported methods, zero gossms callers, all
  deliberately kept.
- When an audit turns up something unused, the allowed moves are: make it
  faster, make its doc comment accurate about what it actually does, or add
  a test that pins it. Removal, or replacing a general form with the narrow
  one gossms happens to need, is not one of them — raise it instead.
- Optimisation must be behaviour-preserving at the API surface: same
  signature, same results, same errors.
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

**A DSN test asserts what the driver parses, not only what gosmo writes.**
Four Entra methods shipped unable to connect because every test checked the
query string `buildDSN` produced and none ran it through go-mssqldb. Go
through `buildConnector` (for Entra it runs the azuread parser and validator
without dialling) and assert on `msdsn.Parse(dsn).Parameters`;
`auth_test.go`'s `driverParams` does both. A known, unfixed bug is pinned
there with a `knownBroken` marker, which fails once the fix lands.

Build and test **inside this repo** before relying on a change from gossms —
a gossms-side build only compiles the packages it imports.

**A plain `go vet ./...` does not compile the `livedb` tests.** An API rename
can leave every one of them uncompilable and nothing says so: the 2026-09-17
`Ref` rename did exactly that to 17 files, found only when the next breaking
change swept the same call sites. Run `go vet -tags livedb ./...` too after any
rename, and before a tag.

## Conventions

- **Method pairs.** Every method that touches the database comes in two
  forms: `Foo(...)` delegating to `FooContext(ctx, ...)`. Accessors that
  only read already-fetched struct state, and the `*Seq` iterators (which
  take a `ctx` directly), are the exceptions.
  `method_pair_wiring_test.go` pins the 676 delegates by reading the source:
  each must call its *own* `Foo`+`Context` with `context.Background()` and the
  declared parameters in order. Nothing executes them otherwise — their
  coverage is 0.0% — and a delegate wired to a same-signature sibling
  (`AddRoleMember`/`RemoveRoleMember`) compiles and passes every other test.
  A deliberate non-delegate goes in that file's `delegateExceptions` with a
  reason.
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
  pooled connection and issues its own `USE` (batched with the query — see
  `Database.useBatch`), so a per-row lookup costs a round trip and an
  acquisition per row *while the outer connection is still held* — the shape
  that exhausts a pool, not merely a slow one. Fetch the
  child rows for the whole object in one query with no parent-id predicate,
  ordered by the parent id first, and group them in Go.
  `Table.IndexesContext` is the worked example (2026-08-14: 42 round trips
  across 21 connections for a 20-index table, now 2).
- **A zoneless server clock is stamped `time.UTC`, never `time.Local`.**
  go-mssqldb already hands `datetime` columns back in UTC, so a value decoded
  by hand (msdb's YYYYMMDD/HHMMSS integer pairs in `parseSQLAgentDate`, an
  error-log line) must match. `time.Local` renders the same digits and so
  looks right, but is off by the client's UTC offset the moment it is
  compared with or subtracted from a `datetime`-derived value beside it.
- **Over ~900 lines is the prompt to split** a file, along the lines its own
  section banners already draw — a prompt to look, not a defect on its own.
  Extract by exact line range, diff the extracted text byte-for-byte against
  the original, and only then delete the source.
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
- **Catalog state is an exported field, not an accessor.** A type scanned
  from a catalog row exposes what it scanned as exported fields — `Login.SID`,
  `Table.Name`, `Job.IsEnabled`. `Database` was the last holdout, hiding nine
  behind `Name()`/`State()`/… until 2026-09-18, and the shape was unguessable:
  a caller could not tell from the type which form it would get, and
  `DatabaseRef("master").IsSystem()` compiling to `false` was the trap it
  produced. Only a *derivation* stays a method (`Database.IsSystem`,
  `Database.IsSnapshot`, computed from `ID`/`SourceDatabaseID`), as does a
  back-pointer (`Database.Server`, `Table.DB`). Adding an accessor over a
  scanned field re-creates the holdout.
- **The `Ref` suffix marks a lookup-free handle.** `Server.DatabaseRef(name)`
  returns a `*Database` carrying only its name — no query, every other field
  at its zero value — while `Server.DatabaseByName(name)` reads the catalog.
  They are not interchangeable: the handle is the only form that works under
  a `WithScript`-derived context, and the populated one is the only form
  whose accessors answer anything. Twenty-two families pair this way
  (`DatabaseRef`, `LoginRef`, `TableRef`, the four Agent ones, the
  audit/credential/trigger/snapshot/plan-guide/backup-device/AG families, and
  `ServerRoleRef`, `UserRef`, `StatisticRef`, `ConfigurationRef`); every
  other by-name lookup in the library is `*ByName` with no handle beside it.
  `Endpoint` is the one family deliberately left without one — `IsSystem` is
  derived from a scanned id, so a name-only handle would carry id 0 and
  refuse every write on itself; see `endpoint.go`'s comment above
  `ErrSystemEndpoint`. **A new handle method takes the `Ref` suffix** — the
  suffix is what stops `s.Database("x").State()` from compiling into a silent
  zero value, which is what the un-suffixed name allowed. Their doc
  comments in `server.go` and `login.go` are the authority;
  `go doc gosmo.Server.DatabaseRef`.

## Open threads

`OPEN-THREADS.md` holds open work and settled decisions — what is knowingly
left undone, and what must be watched (azidentity's deprecated ROPC credential,
the version-support gate table, backup `TO URL`). Anything knowingly deferred,
or any rule learned from a mistake that does not belong in this file or
`ARCHITECTURE.md`, goes there rather than into a commit message.

## Release

`RELEASE.md` carries the current release and `CHANGELOG.md` the history;
don't edit either as part of a feature or fix unless asked. Note that gossms cannot cut a
release while gosmo's `HEAD` is untagged — tag and push here first.
