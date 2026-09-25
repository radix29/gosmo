# CLAUDE.md

Context for Claude Code sessions on **gosmo**.

## What this is

gosmo is a pure-Go (no CGO) library mimicking SQL Server Management Objects
(SMO) without WMI/COM, over `github.com/microsoft/go-mssqldb`. Go 1.27.

- Module `github.com/radix29/gosmo` — https://github.com/radix29/gosmo
- `ARCHITECTURE.md`: API map (Mermaid), feature map, errors, authentication,
  connection internals. Diagrams live in `diagram/*.mmd`; only
  `diagram/00-map.mmd` is inlined. Read § Maintaining this document before
  touching a diagram.
- Read only what the task touches: `quoting.go`'s doc comments for anything
  building an identifier or literal; `server.go`/`login.go`'s for the
  handle-vs-lookup pairs.

## This is a library, not gossms's back end

**goSSMS** (`~/go/gossms`, https://github.com/radix29/gossms) is the main
consumer, developed in the same sitting with an active `replace
github.com/radix29/gosmo => ../gosmo`. **It is not the definition of gosmo's
API** — gosmo has other users.

- **Never remove or narrow a capability because gossms doesn't call it** —
  files, exported methods/types/fields, or fields only some paths populate.
  (The 2026-09-22 removal of the context-free `Foo` delegates and `*Seq`
  iterators was a one-off author waiver, not a precedent.)
- For unused surface the allowed moves are: make it faster, make its doc
  accurate, or pin it with a test. Removal or narrowing — raise it instead.
- Optimisation must preserve the API: same signature, results, errors.
- Adding capability for gossms is encouraged — as a library feature, not a shim.

## Build & verify

```
go build ./...  &&  go test ./...  &&  gofmt -w .  &&  go vet ./...
go vet -tags livedb ./...   # after any rename, and before a tag
```

Plain `go`, no Makefile. `go test ./...` uses fakes (`captureConn`,
`fakeQueryConn`) and needs no server. Real SQL behaviour must also run against
a live instance (details not in the repo; ask): create throwaway objects,
exercise the write, drop them; never mutate pre-existing objects.

- **`go vet ./...` does not compile the `livedb` tests** — the 2026-09-17 `Ref`
  rename silently broke 17 of them. Hence the `-tags livedb` line above.
- **A DSN test asserts what the driver parses**, not only what gosmo writes
  (four Entra methods shipped unable to connect). Go through `buildConnector`
  and assert on `msdsn.Parse(dsn).Parameters`; `auth_test.go`'s `driverParams`
  does both. A known unfixed bug is pinned with a `knownBroken` marker.
- **A live test closes its connection with `t.Cleanup`, never `defer`,** when
  it drops objects in a `t.Cleanup` — a deferred close runs first and every
  drop fails silently.
- Build and test **here** before relying on a change from gossms — a gossms
  build compiles only the packages it imports.

## Conventions

- **One form, context first.** Every database-touching method takes `ctx` first
  and has no other form — no `Foo`/`FooContext` pair. Accessors over fetched
  state take no context. Do not reintroduce a context-free form (the old pairs
  were 707 untested delegates, one miswired to a sibling).
- **Errors** wrap with `%w`, prefixed `gosmo: ` + the attempted operation:
  `fmt.Errorf("gosmo: drop statistic %q: %w", st.Name, err)`.
- **Row iteration.** Every `query` gets `defer rows.Close()` and a checked
  `rows.Err()`; it and every `rows.Scan` wrap with the *same* message as the
  query error (else a mid-iteration failure surfaces as a bare `context
  deadline exceeded`). List reads use `scanRows` (`helpers.go`); by-name reads
  end in `foundRow` (`sql.ErrNoRows` → `notFoundf`). Hand-roll only for shapes
  they don't fit. Shared scan helpers (`scanColumns`, `scanExtProps`,
  `scanEffectivePermissions`, `securityPredicates`, `indexColumns`,
  `execWithProgress`) return bare errors on purpose; their callers wrap.
- **Quoting** — `quoting.go`'s doc comments are the authority. `QuoteName`/
  `qualifiedName` bracket an identifier; `QuoteLiteral` builds a whole literal;
  `escapeSingle` (`helpers.go`) escapes inside quotes already in the format
  string. An identifier *inside* a literal (`OBJECT_ID`, `DBCC
  SHOW_STATISTICS`, `fn_listextendedproperty`) needs both:
  `escapeSingle(t.FullName())`. Wrong forms resolve to the wrong object or to
  NULL — and a NULL `object_id` means "every object" to
  `sys.dm_db_index_physical_stats`. `identifier_quoting_test.go` pins it.
  Prefer a query parameter where the server accepts one.
- **Never query inside a `rows.Next()` loop.** `Database.query` pins its own
  pooled connection and `USE` (`Database.useBatch`), so per-row lookups hold
  the outer connection while acquiring more — pool exhaustion. Fetch children
  in one query ordered by parent id and group in Go (`Table.Indexes`: 42 round
  trips → 2).
- **Zoneless server clocks are `time.UTC`, never `time.Local`** — go-mssqldb
  returns `datetime` in UTC, so hand-decoded values (`parseSQLAgentDate`,
  error-log lines) must match or they are off by the client's offset.
- **Files.** One per subject area (`table.go`, `index.go`, …), `helpers.go` for
  cross-file helpers, `types.go` for shared enums. Over ~900 lines is a prompt
  to split along existing section banners: extract by exact line range, diff
  byte-for-byte, then delete the source.
- **Script mode.** `WithScript` collects statements instead of executing;
  `Scripting(ctx)` detects it. A path that *reads* to decide what to write
  breaks under it:
  - A `Create*` reading its object back returns a name-only handle under
    `Scripting(ctx)` via `createdObject` (`helpers.go`); a new one must too.
  - A write mirroring onto the receiver (`Rename` → `.Name`, `Enable` →
    `.IsEnabled`) goes through `setIfApplied`, never direct assignment.
  - Bound parameters in a captured statement are substituted to literals
    (`bindScriptArgs`) — nothing binds `@p1` in a query editor.
- **Every `Create*` is `CreateX(ctx, CreateXRequest) (*X, error)`.** The
  request is a value; the result comes back through `createdObject` (the `XRef`
  handle under `Scripting(ctx)` or when the new row isn't visible). A type keeps
  a `Spec` name only if a non-create method takes it too (`ServerAuditSpec`).
- **A write on an existing object is a method on its handle** —
  `db.ViewRef(s, n).Drop(ctx)`, `t.Rename(ctx, n)` — never
  `Parent.VerbX(ctx, name, …)`. A new family gets its `Ref` and writes together.
- **Catalog state is an exported field, not an accessor** (`Login.SID`,
  `Table.Name`, `Database.State`). Only derivations (`Database.IsSystem`,
  `IsSnapshot`) and back-pointers stay methods. Don't add an accessor over a
  scanned field.
- **Every type holding a parent exposes it** as `Database() *Database` or
  `Server() *Server` — one line, no context, never another name (`Server.DB()`
  is the `*sql.DB` pool). `parent_accessor_wiring_test.go` enforces it;
  deliberate omissions go in `parentAccessorExceptions` with a reason.
- **The `Ref` suffix marks a lookup-free handle** — this bullet is the
  authority (gossms's `CLAUDE.md`, `docs/decisions.md` and `dbOf` in
  `internal/tui/explorer_object_ops.go` point here). `Server.DatabaseRef(name)`
  carries only the name, no query; `Server.DatabaseByName(name)` reads the
  catalog. Not interchangeable: only the populated one answers field questions,
  and only the handle works with nothing to read — no connection, or an object
  a script is about to *create* (a New-X dialog's Script Changes). `WithScript`
  alone is not that case: it intercepts writes only, so by-name reads still hit
  the server.
  - Fifty-five families pair this way: `DatabaseRef`, `LoginRef`, `TableRef`,
    the four Agent ones, the audit/credential/trigger/snapshot/plan-guide/
    backup-device/AG families, `ServerRoleRef`, `UserRef`, `StatisticRef`,
    `IndexRef`, `ConfigurationRef`, `CertificateRef`, `AsymmetricKeyRef`,
    `SymmetricKeyRef`, the schema, sequence, synonym, rule, default, three type,
    XML schema collection, partition, Always Encrypted key, assembly, `RoleRef`,
    external-resource and Service Broker families, and `ViewRef`,
    `StoredProcedureRef`, `UserDefinedFunctionRef`, `TriggerRef`. Every other
    by-name lookup is `*ByName` with no handle.
  - A schema-scoped handle takes its schema as given and refuses an empty one
    on write (`ErrSchemaRequired` via `requireSchema`).
  - `Endpoint` deliberately has no handle: `IsSystem` derives from a scanned
    id, so a name-only handle would refuse every write (see `endpoint.go` above
    `ErrSystemEndpoint`).
  - **A new handle method takes the `Ref` suffix** — it is what stops
    `s.Database("x").State()` compiling into a silent zero value. Doc comments
    in `server.go`/`login.go` are authoritative; `go doc
    gosmo.Server.DatabaseRef`.

## Open threads

`OPEN-THREADS.md` holds work knowingly left undone and what to watch
(azidentity's deprecated ROPC credential, the version-support gate table,
backup `TO URL`). Deferred work, or a lesson that belongs in neither this file
nor `ARCHITECTURE.md`, goes there — not into a commit message.

## Release

`RELEASE.md` is the current release, `CHANGELOG.md` the history; don't edit
either in a feature or fix unless asked. gossms cannot release while gosmo's
`HEAD` is untagged — tag and push here first.
