# Release notes

The current release, in brief. Detail and history are in
[CHANGELOG.md](CHANGELOG.md).

## v0.0.14

### New

- Service Broker: message types, contracts, queues, services, routes, remote
  service bindings and broker priorities — read, script, drop.
- `ALTER QUEUE` and `ALTER ROUTE`, plus queue message counts and activation
  monitors.
- Batched Agent writes: `Job`/`Alert`/`Operator`/`Schedule.Alter` send one
  `sp_update_*` call; `gosmo.Ptr` builds the fields.
- `AcquireConn` — a pinned connection that retries a dead one from the pool.
- `ServerRoleRef`, `ConfigurationRef`, `UserRef`, `StatisticRef` handles.
- `Server()`/`Database()` back-pointer on every type that has a parent.
- `Sequence.DataTypeSchema` and the exported `ShowplanColumn`.
- Sixteen more `*Seq` iterators (128 now).

### Fixes

- `Alert.SetTrigger`, `Schedule.SetFrequency` and `Schedule.SetActiveRange`
  changed the receiver under `WithScript`.
- A scripted sequence over an alias type lost the type's schema.

### Changes

- **Breaking:** `Database` fields are exported — `db.Name()` is `db.Name`.
- **Breaking:** lookup-free handles take the `Ref` suffix —
  `srv.Database(name)` is `srv.DatabaseRef(name)`, and so on for all 18.
- **Breaking:** `Table.DB()` is `Table.Database()`.
- **Breaking:** `Xml` is `XML` in every identifier
  (`XMLSchemaCollection`, …).
- Large files split along their section banners; no behaviour change.
- Every write path now has an offline test of its exact statement.
- New `OPEN-THREADS.md` for open work and settled decisions.
