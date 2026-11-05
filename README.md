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

## Why keel

- **Typed end to end.** Columns, relations, values and results are checked by the compiler; raw SQL
  is explicit (`keel.Raw`) and greppable.
- **Tenancy by construction.** The tenant comes from the context and is applied to every statement;
  a statement without a scope fails, and crossing tenants is written down with a reason.
- **Fewer round trips.** A read and its relation tree, a page and its total, a batch of reads, and a
  transaction's setup and statement each take one round trip: 2 to 3 times as fast as other Go
  libraries on relation loads ([performance](docs/performance.md)).
- **Built for many tenants on one database.** Per-tenant concurrency limits, budgets that catch N+1
  loops, a result cache invalidated by data versions, read replicas, and slow-wait reports.
- **Bounded and prepared by default.** `List` needs a limit, pages are keyset-based, bulk inserts
  send one array per column, and every value is a bind parameter.

keel is for B2B backends that serve many organizations from one PostgreSQL database. It is
PostgreSQL-only, runs on pgx v5 (never `database/sql`), never generates migrations from structs and
has no lazy loading. [Why keel](docs/why-keel.md) says when it fits and when to choose something
else.

## Install

```sh
go get github.com/emoss08/keel
```

keel needs Go 1.26 and PostgreSQL (tested on 18).

Using Claude Code or another coding agent? Install the keel skill so it writes keel code the way
these docs describe ([details](docs/coding-agents.md)):

```text
/plugin marketplace add emoss08/keel
/plugin install keel@keel
```

## Quick start

The examples use the models of keel's own tests, in `internal/testmodels/fleet`; every snippet in
the docs has a compiled counterpart in [example_test.go](example_test.go).

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
first stop of the first move. The schema itself lives in SQL migrations you write; see the
[tag reference](docs/guide.md#tag-reference).

### 2. Generate

```sh
go run github.com/emoss08/keel/cmd/keel gen ./internal/...
```

This writes a package next to each model package (`fleet` → `fleet/fleetdb`) with a handle per table
(`fleetdb.Shipments`), a typed field per column (`.Status`) and a method per relation
(`.Customer()`). Run it with `-check` in CI.

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

Next, take the [tour](docs/tour.md) for every feature in short examples, then keep the
[guide](docs/guide.md) open while you build.

## Documentation

| Document | Read it for |
|---|---|
| [Why keel](docs/why-keel.md) | what keel is for, and when to choose something else |
| [Tour](docs/tour.md) | a short example of every feature |
| [Guide](docs/guide.md) | how to use keel, by task |
| [Design](docs/design.md) | the design contract and how each part works inside |
| [Migrations and seeds](docs/migrations-and-seeds.md) | migrations, seeds and an application's database binary |
| [Analytics](docs/analytics.md) | the user-built report engine |
| [Performance](docs/performance.md) | measurements, why keel is fast, keeping an application fast |
| [Coding agents](docs/coding-agents.md) | the keel skill for Claude Code and other agents, and [llms.txt](llms.txt) |

[docs/](docs/README.md) lists every document, including the [benchmarks](docs/benchmarks.md).

## Packages

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
| `numeric` | `shopspring/decimal` values read and bound in binary, 2 to 3 times as fast as through their text |
| `analytics` | a semantic layer for user-built reports: definitions, formulas, a compiler to tenant-scoped SQL, exports, rollups |
| `skills` | the skill that teaches coding agents (Claude Code) to write keel code; `keel skill install` writes it |

## Status

keel is pre-1.0. It is built alongside [Trenova](https://github.com/emoss08/Trenova), a
transportation management system designed for thousands of tenants on one database, and every
Trenova query runs through it. Releases are tagged
`v0.x.y`; until 1.0 a minor version may change the API, and its release notes say how.

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

## License

keel is released under the [MIT License](LICENSE).
