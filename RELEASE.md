# Release notes

The current release, in brief. Detail and history are in
[CHANGELOG.md](CHANGELOG.md).

## v0.0.15

### New

- Symmetric keys: list, create, add/drop encryptions, change owner, drop.
- Database master key as an object: regenerate, encryptions, backup, drop.
- Module signatures: add, drop and list `ADD [COUNTER] SIGNATURE`.
- Certificate backup, `RemovePrivateKey` and `SetOwner` on certificates and
  asymmetric keys; `CreateAsymmetricKey`; EKM `FROM PROVIDER` keys.
- `ScriptCertificate`, `ScriptAsymmetricKey`, `ScriptSymmetricKey`,
  `ScriptStatistic`.
- `ScriptTable` keeps every table facet it used to drop, and scripts graph,
  memory-optimized, FILESTREAM, Always Encrypted, ledger, FileTable and
  external tables, and XML and spatial indexes.
- `CreateUserRequest` for every kind of database user.
- `Rename` and `Transfer` on every schema-scoped handle;
  `Table.RenameConstraint`.
- 34 more `Ref` handles (56 in all).
- `Server.ApplyConfiguration`, `ReleaseIdleConnections`, `DefaultPaths`,
  `AgentCounts`.
- `WithStatementObserver`, `WithScriptServer`, and `ScriptCollector.String()`.
- `RestoreOptions.CloseExistingConnections`.

### Fixes

- A statement captured under `WithScript` kept its `@p1` placeholders and
  would not run.
- `Table.Drop(cascade)` could drop the foreign keys and keep the table.
- `Index.SetIncludedColumns` lost the index's options and filegroup.
- Non-ASCII file paths and names were mangled by non-`N` literals.
- A role name containing a comma split into two roles.
- `Index.StorageInfo` over-counted rows on tables with LOB data.
- Mixed per-partition compression scripted as uniform.
- `Index.UpdateStatistics` sampled differently from `Statistic.Update`.
- Detach and other exclusive-access writes were blocked by gosmo's own
  idle connection.
- `BackupHistory` listed a striped backup once per stripe.
- A `decimal(38,0)` sequence or identity failed its listing.
- `CreateTable` turned `datetime2(0)` into `datetime2(7)`.
- Sequences and partition functions scripted without precision or length.

### Changes

- **Breaking:** one form per method — `FooContext` is now `Foo(ctx, …)`.
- **Breaking:** the `*Seq` iterators are removed.
- **Breaking:** every `Create*` takes a `CreateXRequest` and returns the
  object.
- **Breaking:** writes on an existing object are methods on its handle —
  `db.DropView(ctx, s, n)` is `db.ViewRef(s, n).Drop(ctx)`.
- **Breaking:** string modes and permission names are typed constants.
- **Breaking:** the `...WithOptions` permission twins are merged.
- **Breaking:** option structs replace positional flags (index rebuild and
  options, change password, restore recovery, `Termination`).
- **Breaking:** backup locations are `BackupTarget`s everywhere.
- **Breaking:** `ChangeOwner` → `SetOwner`, `SetState` → `Enable`/`Disable`,
  `ColumnTypeString` → `Column.TypeString`.
- **Breaking:** sequence and identity values are strings.
- An empty schema is refused with `ErrSchemaRequired`.
- `CertificateByName`/`AsymmetricKeyByName` return `ErrNotFound`.
- `ConnectTimeout` now covers the TCP dial.
- Dependencies: `azcore` v1.23.2, MSAL for Go v1.10.1.
