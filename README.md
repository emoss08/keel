# keel

[![ci](https://github.com/emoss08/keel/actions/workflows/ci.yml/badge.svg)](https://github.com/emoss08/keel/actions/workflows/ci.yml)

**A typed, multi-tenant data layer for Go and PostgreSQL.**

You write plain structs. `keel gen` writes a typed package for them. Every query, filter, relation
load and write goes through that package, so the compiler checks your columns and values, and
every statement is scoped to the caller's tenant before it reaches Postgres.

```go
s := fleetdb.Shipments
m := s.Moves()

page, err := s.
    Where(
        s.Status.In(fleet.StatusNew, fleet.StatusAssigned),
        keel.When(search != "", s.Customer().Name.ContainsFold(search)),
        m.Any(m.Status.Eq(fleet.StatusInTransit)),
    ).
    Load(s.Customer(), m.Order(m.Sequence.Asc()), m.Stops().Location()).
    Order(s.CreatedAt.Desc()).
    Page(ctx, db, keel.After(cursor, 50).Total(10_000))
```

Outside a transaction that is one round trip: the page, its total, the joined customer and every
level of moves, stops and locations, all filtered to the tenant on the context.

> **Why "keel"?** A keel is the beam along the bottom of a ship's hull. Everything else is built on
> it, and it is what keeps the ship upright and stops it drifting sideways. keel does that job under
> an application's data: it is the part every query rests on, and it keeps each tenant's work from
> drifting into another's, so the application stays on an even keel as it grows.

## Why keel

- **Typed end to end.** Columns, relations, values, assignments and results are checked by the
  compiler. `s.Status.In(...)` accepts only `fleet.Status` values; a misspelled column does not
  compile. Raw SQL exists, but it is explicit (`keel.Raw`) and greppable.
- **Tenancy by construction.** The tenant comes from the context and is applied to every statement
  on a tenant table: reads, joins, relation loads, subqueries, updates, deletes and inserts. A
  statement without a scope fails. Crossing tenants takes a system scope with a reason and an
  explicit `AllTenants()`.
- **Fewer round trips.** Transactions begin with their first statement. A lone statement that needs
  a transaction (a row-level security `set_config`) is one pipelined `BEGIN; ...; COMMIT`. A page and
  its total share a round trip, so do the independent reads of a `keel.Batch`, and a relation tree
  loads inside the read's own round trip wherever the read sees one snapshot.
- **Prepared statements, always.** Values are bind parameters, so a statement's text depends only on
  its shape and Postgres skips parsing and, usually, planning. `In` binds one array: 3 IDs and 3,000
  IDs are the same statement.
- **Built for many tenants on one database.** Per-tenant concurrency limits, query budgets that catch
  N+1 loops, a result cache invalidated by data versions, read replicas with lag tracking, and
  reports of slow waits that name who is holding what.
- **Bounded by default.** `List` needs a limit, pagination is keyset-based, bulk inserts send one
  array per column, and every wait for a slot or connection can be capped.
- **No reflection on the hot path.** Generated accessors decode rows straight from pgx's buffers
  into struct fields.

keel is Postgres-only and uses pgx v5, never `database/sql`.

## What's in the box

| Package | What it does |
|---|---|
| `keel` | queries, relation loads, pagination, writes, transactions, tenancy, hooks, the result cache |
| `cmd/keel`, `gen` | `keel gen`: the typed package for each model package, and `-check` for CI |
| `migrate` | SQL and Go migrations under an advisory lock, with lock-timeout retries, squash and templates for tests |
| `seed` | reference data and fixtures as declared seeds that re-run when their inputs change |
| `keelcli` | the command line an application's database binary is made of: `up`, `migrate`, `seed`, a live console |
| `keeltest` | golden snapshots of rendered SQL and assertions that an index serves a query |
| `keellog` | a readable statement log for development, with values, caller and outcome |
| `cache/rediscache` | the shared tier of the result cache on Redis, Valkey, KeyDB or Dragonfly |
| `vector`, `postgis` | pgvector distances and HNSW settings; PostGIS points, geometries and spatial SQL |
| `analytics` | a semantic layer for user-built reports: definitions, formulas, a compiler to tenant-scoped SQL, exports, rollups |

## Install

```sh
go get github.com/emoss08/keel
```

keel needs Go 1.26 and PostgreSQL (tested on 18).

## Quick start

The examples use the models of keel's own tests, in `internal/testmodels/fleet`. The snippets in
this README and in [docs/guide.md](docs/guide.md) have compiled counterparts in
[example_test.go](example_test.go), which keeps them in step with the API.

### 1. Write a model

A model embeds `keel.Table` and tags every stored field with `keel`. Abridged from
[fleet.go](internal/testmodels/fleet/fleet.go):

```go
package fleet

// Customer is a tenant's customer.
type Customer struct {
    keel.Table `keel:"customers,report=Sales"`

    ID             ids.ID `keel:"id,pk,autoid:cus_"`
    OrganizationID ids.ID `keel:"organization_id,pk,tenant"`
    BusinessUnitID ids.ID `keel:"business_unit_id,pk,tenant"`
    // Name is the customer's trading name.
    Name      string `keel:"name"`
    Code      string `keel:"code"`
    Version   int64  `keel:"version,version"`
    CreatedAt int64  `keel:"created_at,autocreate"`
    UpdatedAt int64  `keel:"updated_at,autoupdate"`
}

// Shipment is a load moved for a customer.
type Shipment struct {
    keel.Table `keel:"shipments,report=Operations"`

    ID             ids.ID `keel:"id,pk,autoid:shp_"`
    OrganizationID ids.ID `keel:"organization_id,pk,tenant"`
    BusinessUnitID ids.ID `keel:"business_unit_id,pk,tenant"`
    CustomerID     ids.ID `keel:"customer_id"`
    // ProNumber is the carrier's progressive number, unique per tenant.
    ProNumber string `keel:"pro_number"`
    Status    Status `keel:"status"`
    Weight    *int64 `keel:"weight,format=weight"`
    Version   int64  `keel:"version,version"`
    CreatedAt int64  `keel:"created_at,autocreate"`
    UpdatedAt int64  `keel:"updated_at,autoupdate"`

    Customer   *Customer `keel:"belongs:customer_id"`
    Moves      []*Move   `keel:"many:shipment_id"`
    OriginStop *Stop     `keel:"pick:moves.stops,first=moves.sequence|stops.sequence"`
}
```

`autoid` fills the key from your ID generator, `tenant` columns come from the scope, `autocreate`
and `autoupdate` are stamped, and `version` gives `Save` optimistic locking. `OriginStop` picks the
first stop of the first move, and keel loads and filters it like any to-one relation. The schema
itself lives in SQL migrations. Every tag option is checked by `keel gen`, which also derives labels
(`CustomerID` → "Customer ID", `ProNumber` → "PRO Number" with `-acronyms PRO`), descriptions from doc comments, `Status`'s enum values and, for models tagged
`report`, how reports present each column; see the
[tag reference](docs/guide.md#tag-reference).

### 2. Generate

```sh
go run github.com/emoss08/keel/cmd/keel gen ./internal/...
```

For each package with models, this writes a package next to it (`fleet` → `fleet/fleetdb`) with a
handle per table (`fleetdb.Shipments`), a typed field per column (`fleetdb.Shipments.Status`), a
method per relation (`fleetdb.Shipments.Customer()`) and `Get` by primary key. Run it with `-check`
in CI.

### 3. Open a database

```go
type tenantCtxKey struct{}

db, err := keel.Open(ctx, keel.Config{
    DSN:           os.Getenv("DATABASE_URL"),
    TenantColumns: []string{"organization_id", "business_unit_id"},
    Tenant: func(ctx context.Context) ([]any, bool) {
        t, ok := ctx.Value(tenantCtxKey{}).([2]ids.ID)
        if !ok {
            return nil, false
        }
        return []any{t[0], t[1]}, true
    },
    NewID: newID, // your generator: func(prefix string) string
})
if err != nil {
    return err
}
defer db.Close()

ctx = context.WithValue(ctx, tenantCtxKey{}, [2]ids.ID{"org_acme", "bu_dallas"})
```

### 4. Create, query, update

```go
c, s := fleetdb.Customers, fleetdb.Shipments

cust := &fleet.Customer{Code: "ACME", Name: "Acme Freight"}
if err := c.Create(ctx, db, cust); err != nil { // ID, tenant columns and timestamps filled
    return err
}
sh := &fleet.Shipment{CustomerID: cust.ID, ProNumber: "PRO-1001", Status: fleet.StatusNew}
if err := s.Create(ctx, db, sh); err != nil {
    return err
}

open, err := s.
    Where(s.Status.In(fleet.StatusNew, fleet.StatusAssigned), s.Customer().Code.Eq("ACME")).
    Load(s.Customer()).
    Order(s.CreatedAt.Desc()).
    Limit(50).
    List(ctx, db)

sh.Status = fleet.StatusAssigned
if err := s.Save(ctx, db, sh); errors.Is(err, keel.ErrConflict) {
    // someone else saved this shipment first
}
```

### 5. Use a transaction

The transaction travels on the context; every call made with that context joins it.

```go
err = keel.Write(ctx, db, func(ctx context.Context) error {
    mv := &fleet.Move{ShipmentID: sh.ID, Sequence: 1, Status: fleet.StatusAssigned}
    if err := fleetdb.Moves.Create(ctx, db, mv); err != nil {
        return err
    }
    _, err := s.Where(s.ID.Eq(sh.ID)).Update(ctx, db, s.Status.To(fleet.StatusInTransit))
    return err
})
```

## Feature tour

The tour uses the full models in [fleet.go](internal/testmodels/fleet/fleet.go), with
`s := fleetdb.Shipments` and `c := fleetdb.Customers`.

### Filters

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

### Relations and loads

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

### Pagination

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

### Batching reads

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

### Projections and aggregates

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

### CTEs and set operations

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

### Upserts and bulk writes

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

### Hooks

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

### Result cache

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

### Running many tenants on one database

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

### Dynamic queries

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
([docs/analytics.md](docs/analytics.md)).

### Replicas and data versions

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

### Migrations

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
configuration ([keelcli](keelcli/README.md)).

From Go:

```go
m, err := migrate.Open(ctx, dsn, migrate.Sources(migrate.Source{Name: "core", FS: dir}),
    migrate.Options{After: []string{"SELECT trenova_rls.reconcile()"}})
res, err := m.Up(ctx, migrate.UpOptions{})
```

and at server startup, `migrate.RequireWait(ctx, pool, schema.Version, time.Second)`. Tests copy a
migrated database from a template in milliseconds with `migrate.Template`.

## What keel does not do

- **Migrations from models.** The schema lives in SQL migrations. `keel.Verify` reports where models
  and the live catalog disagree, so drift fails a test instead of production.
- **Soft deletes, identity maps or lazy loading.** Loads are explicit. An interceptor adds a
  soft-delete or ownership filter to every read and write of a table if you want one.
- **Other databases.** keel uses Postgres features directly: arrays, `unnest`, `COPY`, pipelining,
  `LISTEN/NOTIFY`, advisory locks and row-level security.

[docs/design.md](docs/design.md) has the full design and its non-goals.

## Performance

`bench/` runs the same operations through keel, Bun and raw pgx. Medians of 6 runs on a 4-vCPU Xeon
VM with Postgres 18 in Docker on the same host, recorded on 2026-10-23:

| Operation | keel | Bun | keel vs Bun |
|---|---|---|---|
| get one row by key | 328 µs, 94 allocs | 441 µs, 56 allocs | 26% faster |
| list 100 rows | 812 µs, 2,953 allocs | 1,253 µs, 3,623 allocs | 35% faster |
| 50 rows with a to-one and a to-many relation | 1.52 ms, 3,377 allocs | 3.34 ms, 6,735 allocs | 55% faster |
| read in a transaction with a `set_config` (row-level security) | 326 µs, 127 allocs | 1,077 µs, 73 allocs | 70% faster |
| save with optimistic locking | 1.02 ms, 44 allocs | 1.20 ms, 29 allocs | 15% faster |
| build a statement, no database | 3.8 µs, 2.7 KiB | 3.3 µs, 3.0 KiB | 13% slower |

Most of the gain is server work: prepared statements skip parsing and planning, and pipelining
saves round trips, a gap that grows with network latency. keel allocates more than Bun per
single-row statement. Later work (pipelined relation trees, the result cache, read transactions
that end without waiting for `COMMIT`) is measured in [docs/benchmarks.md](docs/benchmarks.md),
with the raw output and how to rerun everything.

## Status

keel is pre-1.0. It is built alongside [Trenova](https://github.com/emoss08/Trenova), a
transportation management system designed for thousands of tenants on one database, and every
Trenova query runs through it. Releases are tagged
`v0.x.y`; until 1.0 a minor version may change the API, and its release notes say how.

## Documentation

- [docs/guide.md](docs/guide.md): how to use keel, by task: configuration, models, generation,
  reading, loading, pagination, writing, transactions, tenancy, hooks, errors, migrations, seeds,
  performance, testing, coming from Bun, and analytics.
- [docs/design.md](docs/design.md): the design contract and how each part works inside.
- [docs/migrations-and-seeds.md](docs/migrations-and-seeds.md): migrations, seeds and `keelcli`.
- [docs/analytics.md](docs/analytics.md): the report engine's design.
- [docs/benchmarks.md](docs/benchmarks.md): measurements against Bun and pgx.
- [example_test.go](example_test.go): compiled examples for the guide.

## Development

```sh
docker run -d --name keelpg -e POSTGRES_USER=keel -e POSTGRES_PASSWORD=keel -e POSTGRES_DB=keel -p 55432:5432 postgres:18-alpine
go test ./...                      # integration tests skip when Postgres is unreachable
KEEL_TEST_REQUIRE=1 go test ./...  # fail instead of skipping
make gen                           # regenerate the test models' packages
make check                         # gofmt, vet, and generated code up to date
make lint                          # golangci-lint (v2, .golangci.yml) and actionlint
make test-replica                  # start a streaming replica of keelpg and run the replica tests
```
