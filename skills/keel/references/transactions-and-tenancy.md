# Transactions, tenancy and request-level controls

## Read and Write

```go
err := keel.Write(ctx, db, func(ctx context.Context) error { // use THIS ctx inside
    sh, err := s.Where(s.ID.Eq(id)).Lock(keel.ForUpdate, keel.NoWait).One(ctx, db)
    if err != nil {
        return err // rolls back
    }
    sh.Status = fleet.StatusAssigned
    if err := s.Save(ctx, db, sh, s.Status); err != nil {
        return err
    }
    keel.AfterCommit(ctx, func(ctx context.Context) { events.Publish(sh.ID) })
    return nil
}, keel.LockTimeout(2*time.Second))

sh, err := keel.ReadValue(ctx, db, func(ctx context.Context) (*fleet.Shipment, error) {
    return s.Where(s.ProNumber.Eq("PRO-1001")).Load(s.Customer(), s.Moves()).One(ctx, db)
}, keel.RepeatableRead)

id, err := keel.WriteValue(ctx, db, func(ctx context.Context) (ids.ID, error) { … }, keel.Serializable, keel.Retry(3))
```

- The transaction lives on the context; every terminal called with it joins. Never pass a `tx`.
- `BEGIN`, options and `OnBegin` statements ride with the first statement. A transaction that runs
  nothing costs nothing.
- It holds one connection: use its context from one goroutine at a time. Don't fan out goroutines
  inside a transaction.
- Nested `Read`/`Write` in a `Write` join it; a `Write` (or any write) inside a `Read` returns
  `keel.ErrReadOnly`. Options of joined calls are ignored.
- A panic rolls back and releases the connection.
- A transaction keeps its starting scope; work under another scope returns `keel.ErrScopeMismatch`
  (use `keel.Detached()`).
- `keel.InTx(ctx, db)` reports whether the context carries a transaction.

| Option | Effect |
|---|---|
| `keel.LockTimeout(d)` | `lock_timeout` for the transaction |
| `keel.Setting(name, value)` | transaction-local `set_config` (e.g. `hnsw.ef_search`) |
| `keel.ReadCommitted`, `keel.RepeatableRead`, `keel.Serializable` | isolation level |
| `keel.Retry(n)` | rerun a top-level transaction after `40001`/`40P01`; `fn` must be repeatable |
| `keel.Detached()` | a separate transaction even inside another |
| `keel.ReplicaTx()` | run a `Read` on a replica (one snapshot), for reports/exports |

Use `keel.RepeatableRead` for read transactions whose counts and lists must agree and whose
relation trees should pipeline in one round trip; it costs nothing for read-only work.

`Read` returns as soon as `COMMIT` is written (read-only has nothing to make durable); writes and
`Serializable` reads wait.

## Implicit transactions

Outside `Read`/`Write`, a terminal runs in an implicit transaction when `Config.OnBegin` is set,
the table has `AfterWrite` hooks for the write, for `CustomPlan`, for a write on a replica-marked
context, or when relation loads pipeline. Each is still one round trip for a single statement.

## Detached, savepoints, advisory locks, AfterCommit

```go
num, err := keel.WriteValue(ctx, db, nextInvoiceNumber, keel.Detached(), keel.Retry(3)) // commits now
rates, err := keel.ReadValue(withSystem(ctx, "global rate table"), db, loadRates, keel.Detached())

err = keel.Savepoint(ctx, db, func(ctx context.Context) error { // error rolls back to the savepoint only
    return c.Create(ctx, db, cust)
})
if keel.IsUnique(err, "uq_customers_code") { /* skip */ }

err = keel.LockKey(ctx, db, "invoice-run:"+orgID)       // inside Write; released at tx end
got, err := keel.TryLockKey(ctx, db, "rate-refresh")
keel.AfterCommit(ctx, fn) // after commit; dropped on rollback; immediate outside a tx
```

Detached holds a second connection (size the pool) and must never wait on a lock the outer
transaction holds. Namespace advisory-lock keys.

## Tenancy

`Config.TenantColumns` names the tenant columns; `Config.Tenant(ctx)` returns their values;
`Config.System(ctx)` reports a system scope with a reason (checked first). The application owns the
context helpers (`withTenant`, `withSystem`, …) that put these values on the context; find and use
the project's existing ones.

| Scope | Behaviour on tenant tables |
|---|---|
| tenant | every read, join, load, subquery, update and delete compares tenant columns with the scope; inserts fill them and refuse other tenants |
| system | statements must say `AllTenants()`; joins compare tenant columns between tables; inserts must carry tenant columns |
| none | `keel.ErrNoScope` (tables without tenant columns need no scope) |

```go
ctx := withSystem(context.Background(), "purge canceled shipments") // the app's helper Config.System reads
n, err := s.AllTenants().
    Where(s.Status.Eq(fleet.StatusCanceled), s.CanceledAt.Lt(cutoff)).
    Delete(ctx, db)
```

- `AllTenants()` covers the query's joins, loads, `Any`/`None`. Each subquery (`Exists`, `InQuery`,
  lateral, CTE, set operation) is its own query and needs `AllTenants()` too.
- `Save`/`DeleteRow` under a system scope write by PK without `AllTenants`.

| Error | When |
|---|---|
| `keel.ErrNoScope` | tenant table, no scope |
| `keel.ErrAllTenantsRequired` | system scope without `AllTenants()` |
| `keel.ErrSystemScopeRequired` | `AllTenants()` without a system scope |
| `keel.ErrTenantMismatch` | a row's tenant columns differ from the scope |

Row-level rules beside tenancy: `db.Intercept` (base table, also guards `Save`/`DeleteRow`) or
`keel.WithReadFilter(ctx, f)` (every table reference of reads). See the hooks reference.

## Tenant concurrency and waits

```go
cfg.TenantConcurrency = 8 // terminals/transactions per tenant at once; nested work shares the slot
cfg.MaxWait = time.Minute // any slot/connection wait fails with keel.ErrWaitLimit after this
cfg.SlowWaits = keel.SlowWaitConfig{After: 5 * time.Second, Report: func(ctx context.Context, w *keel.SlowWait) {
    log.Warn("database wait", "kind", w.Kind, "tenant", w.Tenant, "waited", w.Waited, "holders", w.Holders)
}}
```

Don't run statements from `SlowWaits.Report`. A goroutine that drops the transaction from its
context needs its own slot; keep the bound above the nesting the app does.

## Budgets (N+1 detection)

```go
ctx = keel.Budget(ctx, keel.MaxRoundTrips(20), keel.MaxRepeats(3)) // in request middleware
// OnExceeded(fn) logs instead of failing; errors.Is(err, keel.ErrBudgetExceeded) otherwise
if r, ok := keel.BudgetReportFrom(ctx); ok && len(r.Repeats) > 0 { /* log r.Repeats[i].SQL, CallSites */ }
```

`MaxRoundTrips(0)` asserts a path (a cache hit) touches no database. Use budgets in tests to pin
round-trip counts of hot endpoints.

## Replicas

```go
cfg.Replicas = []keel.ReplicaConfig{{DSN: os.Getenv("REPLICA_URL")}}
cfg.MaxReplicaLag = 2 * time.Second

ctx := keel.OnReplica(r.Context()) // mark the whole request, not single queries
page, err := s.Where(...).Order(...).Page(ctx, db, keel.After("", 50)) // replica
err = s.Create(ctx, db, sh)                                           // primary
got, err := s.Get(ctx, db, sh.ID)                                     // sees its own write

cookie := keel.CommitPosition(ctx, db).String()                       // carry across requests
pos, err := keel.ParsePosition(cookie)
next := keel.OnReplicaAfter(r.Context(), db, pos)
```

Writes, `keel.Exec` and explicit transactions use the primary. `keel.ReplicaTx()` runs a long
`Read` on one replica.

## Data versions and the result cache

```go
cfg.DataVersions = keel.DataVersionsConfig{Enabled: true}
cfg.Cache = &keel.CacheConfig{
    Local: keel.MemoryCache(256 << 20),
    Key:   func(ctx context.Context) string { return rlsSettingsOf(ctx) }, // required when OnBegin is set
}

open, err := s.Where(s.Status.Eq(fleet.StatusNew)).Load(s.Customer()).
    Order(s.CreatedAt.Desc()).Limit(50).
    Cache(keel.TTL(5 * time.Minute)).
    List(ctx, db)
```

- An entry is served until any table the read touched changes for the tenant (TTL only evicts
  unused entries). `Cache` works on `One`, `First`, `List`, `All`, `Count*`, `Exists`, `Page` and
  `Batch` items.
- Bypassed inside `Write`, joined transactions, snapshot `Read`s after their first statement,
  system scopes, and `Lock` queries.
- `Key(ctx)` must return everything else rows depend on (e.g. the RLS settings `OnBegin` applies).
- Writes through `keel.Exec`, `keel.Pool(db)`, triggers or another data layer are invisible: call
  `keel.MarkChanged(ctx, db, tables...)`. Tables read only by `keel.Raw`: `keel.AlsoReads(...)`.
- Shared tier: `Shared: rediscache.New(client, "")` from `keel/cache/rediscache`.
