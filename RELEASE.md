# Release notes

The current release, in brief. Detail and history are in
[CHANGELOG.md](CHANGELOG.md).

## v0.4.0

### New

- Fabric Warehouse is detected at connect; `ConnectionOptions.FabricDW` overrides it.
- SQL database in Fabric: `EngineFabricSQLDatabase`; edition helpers `IsDatabaseScoped`, `IsManagedInstance`, `IsFabricSQLDatabase`.
- `ServerInfo.AtLeast` / `AtLeastBuild`: gosmo's version gate, exported.
- `Catalog.Types`: user-defined alias, CLR and table types.
- `Table.ReferencingForeignKeys`; `ForeignKey.Schema` / `TableName`.
- `Database.EnsureMasterKey`, `RebuildAllIndexesScript` (table, view), `QueryStoreReportOptions.Effective`.

### Fixes

- Whole-database scripting no longer fails on a user mapped to a certificate or asymmetric key.
- Whole-database scripting prefetch is about a third faster.
- `BackupRefusal` recognises `tempdb`/`master` by server collation.
- Truncated error messages cut at a rune boundary.

### Changes

- **Breaking:** `Server.IsFabricDW` is a method and can no longer be set.
- **Breaking:** `ServerInfo.EngineEdition` is typed `EngineEdition`, not `int`.
- **Breaking:** `MasterKey`, `DatabaseMirroringEndpoint`, `MirroringDetail`, `ServiceBrokerDetail` return `ErrNotFound`, not `(nil, nil)`.
- **Breaking:** `ObjectByName`, `ObjectsNamed`, `ViewByName` and column reads resolve shipped objects (`sys.objects`).
- **Breaking:** `SystemViews` / `SystemCatalog` include INFORMATION_SCHEMA; system listings sort by schema, then name.
- **Breaking:** `ExtraParams` refuses `Authentication`; use `AuthMethod`.
- `SameName` / `NameKey` follow each collation's case pairing.
- SQL database in Fabric: no longer version-gated as SQL Server 2014; per-index `ALTER INDEX`.
- go-mssqldb v1.12.0 (Kerberos via `go-krb5/krb5`).
