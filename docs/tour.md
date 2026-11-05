# A tour of keel

This tour shows what keel does, one feature at a time, with a short example of each. It is the
quickest way to see the shape of the API before reading the [guide](guide.md), which covers every
feature by task and in full. Each section ends with links to the guide's sections on it.

The examples use the models of keel's own tests in
[fleet.go](../internal/testmodels/fleet/fleet.go) (`Customer`, `Shipment`, `Move`, `Stop`,
`Location`, `Tag`, `Worker`) and their generated package `fleetdb`, with
`s := fleetdb.Shipments` and `c := fleetdb.Customers`. `db` is the application's `*keel.DB` and
`ctx` a request context that carries a tenant. The snippets have compiled counterparts in
[example_test.go](../example_test.go), which keeps them in step with the API.

## Contents

- [Filters](#filters)
- [Relations and loads](#relations-and-loads)
- [Pagination](#pagination)
- [Batching reads](#batching-reads)
- [Projections and aggregates](#projections-and-aggregates)
- [CTEs and set operations](#ctes-and-set-operations)
- [Upserts and bulk writes](#upserts-and-bulk-writes)
- [Hooks](#hooks)
- [Result cache](#result-cache)
- [Running many tenants on one database](#running-many-tenants-on-one-database)
- [Dynamic queries](#dynamic-queries)
- [Replicas and data versions](#replicas-and-data-versions)
- [Migrations](#migrations)

## Filters

Each column has the operators of its type; `keel.When` makes a filter optional.

```go
rows, err := s.Where(
    keel.When(f.Search != "", s.ProNumber.ContainsFold(f.Search)),
    keel.When(len(f.Statuses) > 0, s.Status.In(f.Statuses...)),
    s.Weight.Between(10_000, 40_000),
    s.Labels.Has("expedite"),
    s.Attributes.Text("trailer", "type").Eq("reefer"),
    s.SearchVector.Match("dallas reefer"),
    keel.Or(s.BillToID.IsNull(), s.BillTo().Code.Ne("ACME")),
).Order(s.CreatedAt.Desc()).Limit(50).List(ctx, db)
```

Go deeper: [Operators](guide.md#operators), [Functions](guide.md#functions), [Optional filters](guide.md#optional-filters), [Full-text search](guide.md#full-text-search), [Raw SQL](guide.md#raw-sql).

## Relations and loads

To-one paths join; to-many paths filter with `Any` and `None` and load one statement per level, with
optional per-parent limits, all in the read's round trip: each level selects its parents' keys
with a subquery over the parent level's statement.

```go
m := s.Moves()
cu := s.Customer()

rows, err := s.
    Where(m.Any(m.Status.Eq(fleet.StatusInTransit), m.Loaded.IsTrue())).
    Load(
        cu.Pick(cu.Name, cu.Code),
        m.Where(m.Status.Ne(fleet.StatusCanceled)).Order(m.Sequence.Asc()).Load(m.Stops().Location()),
        s.Tags(),
    ).
    Limit(50).
    List(ctx, db)

latest, err := s.Load(m.Order(m.Sequence.Desc()).Limit(1)).Limit(50).List(ctx, db) // last move each
```

Go deeper: [Relation path filters, Any and None](guide.md#relation-path-filters-any-and-none), [Loading relations](guide.md#loading-relations), [One round trip](guide.md#one-round-trip), [Pick relations](guide.md#pick-relations).

## Pagination

Keyset pages with an opaque cursor and an optional total in the same round trip, exact up to a
limit and the planner's estimate above it with `Estimate()`; `Each` walks a large set at constant
memory.

```go
page, err := s.Where(s.Hazmat.IsTrue()).Order(s.CreatedAt.Desc()).
    Page(ctx, db, keel.After(cursor, 50).Total(10_000).Estimate())
// page.Items, page.Next ("" on the last page), page.Total, page.TotalExact

err = s.Where(s.Hazmat.IsTrue()).Each(ctx, db, 500, func(ctx context.Context, batch []*fleet.Shipment) error {
    return nil
})
```

Go deeper: [Keyset pages](guide.md#keyset-pages), [Estimated counts](guide.md#estimated-counts), [Each](guide.md#each).

## Batching reads

`keel.Batch` sends independent reads in one round trip and fills typed destinations only when every
read succeeded.

```go
var rows []*fleet.Shipment
var total int64
var cust *fleet.Customer
err = keel.Batch(ctx, db,
    keel.ListInto(s.Where(s.Hazmat.IsTrue()).Order(s.CreatedAt.Desc()).Limit(50), &rows),
    keel.CountInto(s.Where(s.Hazmat.IsTrue()), &total),
    keel.OneInto(c.Where(c.Code.Eq("ACME")), &cust),
)
```

Go deeper: [Batching reads](guide.md#batching-reads).

## Projections and aggregates

```go
type statusTotal struct {
    Status  fleet.Status
    Loads   int64
    Hazmat  int64
    Revenue fleet.Money
}
totals, err := keel.Into[statusTotal](ctx, db, s.
    Group(s.Status).
    Pick(s.Status, keel.Count(), keel.Count().Filter(s.Hazmat.IsTrue()), s.Total.Sum()).
    Having(keel.Count().Gt(10)))
```

Window functions (`keel.RowNumber().Over(...)`), `DistinctOn`, joins and self-joins with `As`,
lateral joins and correlated subqueries are covered in the guide.

Go deeper: [Projections and aggregates](guide.md#projections-and-aggregates), [Window functions](guide.md#window-functions), [Grouping sets, rollups and cubes](guide.md#grouping-sets-rollups-and-cubes), [Computed fields](guide.md#computed-fields).

## CTEs and set operations

```go
busy := keel.CTE("busy", []string{"customer_id", "loads"},
    s.Group(s.CustomerID).Pick(s.CustomerID, keel.Count()).Where(s.Status.Eq(fleet.StatusInTransit)))

customers, err := c.With(busy).
    Join(busy, busy.Col("customer_id").EqCol(c.ID)).
    Where(busy.Col("loads").Gte(10)).
    All(ctx, db)

codes, err := keel.Into[string](ctx, db, keel.UnionAll(
    s.Pick(s.ProNumber).Where(s.Hazmat.IsTrue()),
    c.Pick(c.Code).Where(c.Email.IsNull()),
))
```

`keel.RecursiveCTE` walks hierarchies; `keel.Union`, `Intersect` and `Except` combine queries.

Go deeper: [Common table expressions](guide.md#common-table-expressions), [Recursive CTEs](guide.md#recursive-ctes), [Set operations](guide.md#set-operations), [Lateral joins](guide.md#lateral-joins).

## Upserts and bulk writes

```go
err = fleetdb.Stops.CreateMany(ctx, db, stops) // one array per column through unnest, one round trip

n, err := s.Copy(ctx, db, imported) // binary COPY for large loads; IDs, tenant columns and hooks as in Create

inserted, err := c.Upsert(ctx, db, rows,
    keel.OnConflict(c.OrganizationID, c.BusinessUnitID, c.Code).Update(c.Name))

inserted, err = c.Upsert(ctx, db, rows, keel.OnConflict(c.OrganizationID, c.BusinessUnitID, c.Code).Set(
    c.Name.ToExpr(c.Name.Excluded()),                                         // the proposed name
    c.Email.ToExpr(keel.Raw("coalesce(?, ?)", c.Email, c.Email.Excluded())), // keep a stored email
))

n, err = c.CreateOrSkip(ctx, db, rows, c.OrganizationID, c.BusinessUnitID, c.Code)

n, err = s.Where(s.Status.Eq(fleet.StatusNew), s.Customer().Code.Eq("ACME")).
    Update(ctx, db, s.Status.To(fleet.StatusCanceled), s.Priority.Increment(1))
```

Go deeper: [Writing](guide.md#writing), [CreateMany](guide.md#createmany), [Copy](guide.md#copy), [Upsert](guide.md#upsert), [Bulk updates and deletes](guide.md#bulk-updates-and-deletes).

## Hooks

Model hooks are methods on the model (`BeforeSave`, `AfterLoad`, …). Table hooks run before a write,
after it inside the transaction, or after commit, and can ask for the previous values of columns:

```go
keel.OnUpdate(db, s, keel.AfterWrite, func(ctx context.Context, m *keel.Mutation[*fleet.Shipment]) error {
    if m.Old == nil || !m.Changed(s.Status) || m.Old.Status == m.Row.Status {
        return nil
    }
    log.Printf("shipment %s: %s -> %s", m.Row.ID, m.Old.Status, m.Row.Status)
    return nil
}, keel.Needs(s.Status))
```

Interceptors (`db.Intercept`) add predicates to the reads, bulk writes, `Save` and `DeleteRow` of a
table (never inserts), so a soft-delete or ownership filter holds for single-row writes too.
Observers (`Config.Observers`) see every statement; a span started in `BeforeQuery` travels on the
statement's context to its execution and to `AfterQuery`.

Go deeper: [Hooks, interceptors and observers](guide.md#hooks-interceptors-and-observers), [Which hooks run](guide.md#which-hooks-run), [Model hooks](guide.md#model-hooks).

## Result cache

A read that asks for the cache is served from memory, with no round trip, until a table it read
changes for the tenant. Validity never depends on the TTL: writes bump a per-tenant, per-table data
version, pushed to every process with `LISTEN/NOTIFY`. A shared tier on Redis lets processes share
entries.

```go
cfg.DataVersions = keel.DataVersionsConfig{Enabled: true}
cfg.Cache = &keel.CacheConfig{Local: keel.MemoryCache(256 << 20)}

open, err := s.Where(s.Status.Eq(fleet.StatusNew)).
    Load(s.Customer(), s.Moves()).
    Order(s.CreatedAt.Desc()).Limit(50).
    Cache(keel.TTL(5 * time.Minute)).
    List(ctx, db)
```

A hit costs about 20 µs, against about 390 µs for the same read from the database on loopback.
Concurrent misses of one entry wait for a single read.

Go deeper: [Result cache](guide.md#result-cache), [Data versions](guide.md#data-versions).

## Running many tenants on one database

```go
cfg.TenantConcurrency = 8                // at most 8 terminals or transactions per tenant at once
cfg.MaxWait = time.Minute                // no wait for a slot or connection outlasts this
cfg.SlowWaits = keel.SlowWaitConfig{     // report waits past 5s, with who holds the slots
    After:  5 * time.Second,
    Report: func(ctx context.Context, w *keel.SlowWait) { log.Printf("slow %s wait: %+v", w.Kind, w) },
}

ctx = keel.Budget(ctx, keel.MaxRoundTrips(20), keel.MaxRepeats(3)) // per request: catches N+1 loops
```

- **Tenant concurrency** stops one tenant's burst (an import, a dashboard of twenty widgets) from
  taking every connection. Work nested under a tenant's transaction shares its slot, so a bound of
  one cannot deadlock.
- **Slow waits** turn a hang into a log line while it happens: the waiter's stack and, for a slot,
  every holder with how long it has held it and the stack that took it.
- **`MaxWait`** ends a wait outright with `keel.ErrWaitLimit`, so work without a deadline (a
  background job) cannot wait forever behind a stuck request.
- **Budgets** count round trips, statements and repeated statement texts under a context, and fail
  the call that goes over.

Go deeper: [Tenancy](guide.md#tenancy), [Tenant concurrency](guide.md#tenant-concurrency), [Slow waits](guide.md#slow-waits), [Budgets and N+1 detection](guide.md#budgets-and-n1-detection).

## Dynamic queries

For queries whose columns are chosen at run time (a report builder, an export, an admin grid),
`keel.Registry` resolves tables, columns and relation paths by name, with the same tenancy,
interceptors and bind parameters as typed queries, and returns columnar results.

```go
q := reg.Query("shipments")
customer := q.Path("customer", "name")
q.Select(q.Path("pro_number"), customer, q.Path("status")).
    Where(q.Path("status").In([]string{"New", "Assigned"}), customer.ContainsFold(search)).
    Limit(100)
res, err := q.Run(ctx, db)
```

`keel/analytics` builds a full report engine on top of this: report definitions, a formula
language, pivots, charts, drill-through, Excel and CSV exports, and rollup tables
([docs/analytics.md](analytics.md)).

Go deeper: [Dynamic queries](guide.md#dynamic-queries), [Column paths from names](guide.md#column-paths-from-names) and [analytics.md](analytics.md).

## Replicas and data versions

Reads on a context marked with `keel.OnReplica` run on the least-loaded replica within
`Config.MaxReplicaLag`, tracked once a second, and still see the context's own writes: a write
reads its commit position in the `COMMIT` round trip. With `Config.DataVersions` on, keel keeps a
version per tenant and table, bumped after commit by a debounced background writer, for caches
keyed on the data they read.

```go
ctx = keel.OnReplica(ctx)
page, err := s.Where(s.Status.Eq(fleet.StatusNew)).Order(s.CreatedAt.Desc()).Page(ctx, db, keel.After("", 50))

versions, err := db.DataVersions(ctx, db.TenantKey(ctx), "shipments", "customers")
```

Go deeper: [Read replicas](guide.md#read-replicas), [Data versions](guide.md#data-versions).

## Migrations

`<version>_<name>.sql` files and Go functions, applied in order under an advisory lock, each in its
own transaction with a 5s lock timeout retried on contention; `--keel:notx` files run outside a
transaction for `CREATE INDEX CONCURRENTLY`, and `R_<name>.sql` files re-run when they change.

```sh
keel migrate create add_shipment_index -kind notx
keel migrate plan -dsn "$KEEL_DSN"
keel up -dsn "$KEEL_DSN"                 # a live view in a terminal, one line per event elsewhere
keel migrate status -dsn "$KEEL_DSN"
keel migrate squash -yes -dump "docker exec pg pg_dump -U app -d {db}"
keel migrate stamp -out schema/version_gen.go
keel                                     # the console, in a terminal
```

An application's database binary is one `keelcli.App` naming its migrations, seeds and database
configuration ([keelcli](../keelcli/README.md)).

From Go:

```go
m, err := migrate.Open(ctx, dsn, migrate.Sources(migrate.Source{Name: "core", FS: dir}),
    migrate.Options{After: []string{"SELECT trenova_rls.reconcile()"}})
res, err := m.Up(ctx, migrate.UpOptions{})
```

and at server startup, `migrate.RequireWait(ctx, pool, schema.Version, time.Second)`. Tests copy a
migrated database from a template in milliseconds with `migrate.Template`.

Go deeper: [Migrations](guide.md#migrations), [Seeds](guide.md#seeds) and [migrations-and-seeds.md](migrations-and-seeds.md).

## Where to go next

- [guide.md](guide.md): every feature by task, with its rules and edge cases.
- [design.md](design.md): the design contract and how each part works inside.
- [performance.md](performance.md): what keel costs and how to keep an application fast.
- [why-keel.md](why-keel.md): what keel is for, and when to choose something else.
