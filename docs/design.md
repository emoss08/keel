# keel design

keel is a typed, Postgres-only data layer for Go. It is built for one large application
(Trenova: hundreds of thousands of users across thousands of tenants) and optimised in this
order: correctness and tenant isolation, runtime performance, developer experience, build time.

This document is the contract. Code that disagrees with it is a bug in one of the two; fix the one
that is wrong in the same change.

## Goals

- **Typed end to end.** Columns, relations, values, assignments and results are checked by the
  compiler. Raw SQL exists, is explicit (`keel.Raw`) and is greppable.
- **One round trip where Postgres allows it.** Transactions start with their first statement,
  a lone statement runs as a single pipelined `BEGIN; …; COMMIT`, and independent statements of
  one read share a round trip.
- **No reflection on the hot path.** `keel gen` writes field accessors for every model; rows decode
  straight from pgx's buffers into struct fields through pgx scan plans chosen once per result.
- **Tenancy by construction.** The tenant comes from the context and is applied to every statement
  on a tenant table: reads, joins, relation loads, subqueries, updates, deletes and inserts.
- **Bounded by default.** `List` needs a limit, keyset pagination is built in, bulk writes keep a
  fixed statement shape.
- **Cheap to compile.** Generated code is static data plus small non-generic functions. Generic types
  appear only where a typed result needs them (`Query[T]`, `Handle[T]`) and are reachable only from
  data-access packages.

Non-goals: other databases, schema migrations generated from models (migrations are SQL files),
soft delete, an identity map or a session cache.

## Layers

| Package | Role |
|---|---|
| `keel` | the public API: `DB`, transactions, scope, `Handle[T]`, `Query[T]`, expressions, columns, writes, hooks, errors |
| `keel/schema` | runtime table metadata, written as static literals by the generator; the `db` tag parser |
| `keel/gen`, `cmd/keel` | the generator: loads packages with `go/packages`, finds models, writes one `<pkg>db` package per model package |
| `keel/migrate` | the SQL-file migration runner |
| `keel/keeltest` | test helpers: a scoped context, a database per test from a template |
| `keel/internal/sqlw` | the SQL writer: buffer, placeholders, arguments |

The wire layer is pgx v5 (`pgxpool`, `pgx.Batch`, pgx scan plans). keel never uses `database/sql`.

## Models

A model is a plain struct that embeds `keel.Table`. The marker carries the table name and
table-level options; every stored field carries a `db` tag.

```go
type Shipment struct {
    keel.Table `db:"shipments"`

    ID             pulid.ID  `db:"id,pk,autoid:shp_"`
    OrganizationID pulid.ID  `db:"organization_id,pk,tenant"`
    BusinessUnitID pulid.ID  `db:"business_unit_id,pk,tenant"`
    CustomerID     pulid.ID  `db:"customer_id"`
    Status         Status    `db:"status"`
    Version        int64     `db:"version,version"`
    CreatedAt      int64     `db:"created_at,autocreate"`
    UpdatedAt      int64     `db:"updated_at,autoupdate"`

    Customer *customer.Customer `db:"belongs:customer_id"`
    Moves    []*ShipmentMove    `db:"many:shipment_id"`
}
```

### Column options

| Option | Meaning |
|---|---|
| `pk` | part of the primary key |
| `tenant` | a tenant column: filled from the scope on insert, compared to the scope on every statement, added to every relation join |
| `autoid:<prefix>` | on insert, a zero value is replaced by `Config.NewID(prefix)` |
| `autocreate` / `autoupdate` | stamped on insert / on insert and every update (bulk updates included); `int64` means Unix seconds, `time.Time` means a timestamp |
| `version` | optimistic locking: `Save` compares and increments it, bulk updates increment it |
| `readonly` | selected but never written (generated or trigger-maintained columns); returned after writes |
| `noselect` | never selected by default (for example a `tsvector`); usable in predicates |
| `jsonb` | the value is encoded as JSON |

There is no `default:`, `type:` or `notnull`. The SQL migrations own the schema; nullability follows
the Go type (a pointer, `sql.Null*` or another `sql.Scanner` that accepts NULL). The value in the
struct is the value written: keel never substitutes `DEFAULT` for a zero value.

### Relations

| Tag | Meaning |
|---|---|
| `belongs:<fk>` | this table's `<fk>` references the target's primary key |
| `one:<fk>` | the target's `<fk>` references this table's primary key, at most one row |
| `many:<fk>` | the target's `<fk>` references this table's primary key |
| `m2m:<join_table>.<local_fk>.<remote_fk>` | rows linked through a join table |

When both sides have tenant columns of the same name, keel adds them to the join condition. A
composite tenant key never needs to be spelled out.

### Model hooks

Methods on the model with standard-library signatures. The generator detects them and writes
direct calls; there is no runtime type assertion.

```go
func (s *Shipment) BeforeCreate(ctx context.Context) error
func (s *Shipment) BeforeUpdate(ctx context.Context) error
func (s *Shipment) BeforeSave(ctx context.Context) error   // create and update
func (s *Shipment) BeforeDelete(ctx context.Context) error
func (s *Shipment) AfterLoad(ctx context.Context) error
```

## Generated code

`keel gen ./internal/...` writes, for each package that holds models, a package named
`<name>db` (the output path is configurable). For each model it writes:

- the `schema.Table` as a static literal: columns, flags, relations, precomputed column sets, and
  three accessor functions (`New`, `Ptr` for scanning, `Value` for binding) plus relation attach and
  hook trampolines;
- a handle type `ShipmentsTable` embedding `keel.Handle[shipment.Shipment]`, with one field per
  column and one method per relation, and a package variable `Shipments` built statically;
- a column type per named non-basic type (`StatusCol`), so `Status.In(...)` accepts only
  `shipment.Status`. Wrappers for types from other packages (`pulid.ID`) are written once into a
  shared kinds package.

Nothing runs at program start. Handles reached through relations (`s.Customer()`) are built on
first use and memoised.

## Queries

A query starts from a handle and ends with a terminal method that takes `(ctx, db)`.

```go
s := shipmentdb.Shipments
m := s.Moves()

page, err := s.
    Where(
        s.Status.In(shipment.StatusNew, shipment.StatusAssigned),
        keel.When(search != "", s.Customer().Name.Contains(search)),
        m.Any(m.Status.Eq(shipment.MoveStatusAssigned)),
    ).
    Load(s.Customer(), m.Order(m.Sequence.Asc()), m.Stops().Location()).
    Order(s.CreatedAt.Desc(), s.ID.Desc()).
    Page(ctx, db, keel.After(cursor, 50).Total(10_000))
```

### Builder methods

`Where`, `Pick`, `Load`, `Order`, `Group`, `Having`, `Limit`, `Offset`, `Lock`, `Join`,
`LeftJoin`, `LeftJoinLateral`, `DistinctOn`, `AllTenants`, `Timeout`. Builders are mutable and
chainable; `Clone` copies one.

### Terminal methods

| Method | Result |
|---|---|
| `One` | exactly one row, `ErrNotFound` when none, `ErrNotSingular` when more |
| `First` | the first row by the query's order, `ErrNotFound` when none |
| `List` | rows; requires `Limit` |
| `All` | every row; for sets known to be small |
| `Page` | a keyset page with an opaque cursor and an optional capped total |
| `Count`, `CountUpTo`, `Exists` | scalars |
| `Each` | keyset batches at constant memory |
| `Update`, `Delete` | bulk writes over the query's filter; rows affected |
| `keel.Into[R]` | projections: the picked expressions fill `R`'s fields in order |

### Expressions

Every clause takes expressions, never strings. Columns are typed by their Go kind:

| Kind | Operators |
|---|---|
| every column | `Eq`, `Ne`, `In`, `NotIn`, `IsNull`, `IsNotNull`, `IsDistinctFrom`, `EqCol`, `InQuery`, `Asc`, `Desc`, `To`, `ToNull`, `Count`, `CountDistinct`, `Min`, `Max` |
| ordered | `Gt`, `Gte`, `Lt`, `Lte`, `Between`, `Sum`, `Avg`, `Add` |
| string | `Contains`, `HasPrefix`, `HasSuffix` (escaped), `ContainsFold`, `HasPrefixFold`, `Like`, `ILike`, `Lower`, `Match` (regex) |
| bool | `IsTrue`, `IsFalse` |
| jsonb | `Contains`, `HasKey`, `HasAnyKey`, `HasAllKeys`, `Text(path...)` |
| array | `Has`, `Overlaps`, `ContainsAll`, `ContainedBy`, `Len` |
| tsvector | `Matches`, `Rank` |

Combinators: `keel.And`, `keel.Or`, `keel.Not`, `keel.When`, `keel.Exists`, `keel.NotExists`,
`keel.Raw`. Aggregates: `keel.Count()`, `.Filter(pred)`, `.Over(...)`. Relation paths:
belongs-to and has-one columns join automatically; `Any` and `None` on a has-many path compile to
`EXISTS` and `NOT EXISTS`.

Values are bind parameters. `In` binds one array parameter (`= ANY($1)`), so the statement text
does not depend on the number of values.

### Aliases

keel names every table in a statement (`t0`, `t1`, …). A handle reached through `As("name")`
gives a second, independent reference to the same table for self-joins and correlated
subqueries. A reference used in a nested query resolves to the innermost query that contains it.

## Relation loading

`Load` takes relation paths. Belongs-to and has-one segments are joined into the parent's
statement; has-many and many-to-many segments run one statement per relation per level, filtered
by `= ANY($1)` over the parent keys plus the scope's tenant columns. Statements of the same level
share a round trip. A has-many load with a `Limit` loads at most that many rows per parent through
`LATERAL`.

## Writes

```go
err := s.Create(ctx, db, sh)                       // autoid, timestamps, tenant columns filled
err = s.CreateMany(ctx, db, rows)                  // one statement per 65,535 parameters
err = s.Save(ctx, db, sh)                          // compare-and-increment version, ErrConflict on mismatch
err = s.Save(ctx, db, sh, s.Status, s.CanceledAt)  // only these columns
n, err := s.Where(...).Update(ctx, db, s.Status.To(shipment.StatusCanceled))
n, err = s.Where(...).Delete(ctx, db)
ins, err := s.Upsert(ctx, db, rows, keel.OnConflict(s.ID, ...).Update(s.Status))
n, err = s.InsertFrom(ctx, db, query)
```

After a write keel reads back only what Postgres may have changed: `readonly` columns, the version
and `updated_at`.

## Transactions and scope

```go
err := keel.Write(ctx, db, func(ctx context.Context) error { ... }, keel.LockTimeout(2*time.Second))
v, err := keel.ReadValue(ctx, db, func(ctx context.Context) (T, error) { ... })
```

- The transaction lives on the context. Terminals find it there; nested `Read`/`Write` calls join it.
- `BEGIN` and the statements from `Config.OnBegin` (for example a row-level security
  `set_config`) are sent with the first statement. A transaction that runs no statement costs
  nothing.
- Outside a transaction a terminal runs its statements in an implicit transaction when
  `OnBegin` is set; a terminal with a single statement sends `BEGIN`, the `OnBegin` statements,
  the statement and `COMMIT` in one round trip.
- `Config.Tenant` returns the tenant values from the context, in the order of
  `Config.TenantColumns`. `Config.System` reports a system scope and its reason. A statement on a
  tenant table with neither is refused (`ErrNoScope`). Under a system scope a statement on a tenant
  table must say `AllTenants()`.
- `keel.AfterCommit(ctx, fn)` runs `fn` after the transaction on the context commits, or at once
  outside one.
- `keel.Retry(n)` retries serialization failures and deadlocks for a top-level transaction.

## Hooks

| Kind | Registration | Use |
|---|---|---|
| model hooks | methods on the model | normalisation and derived fields |
| table hooks | `keel.OnCreate/OnUpdate/OnDelete[T](db, handle, phase, fn, keel.Needs(cols...))` | validation, audit, side effects; phases `Before`, `After`, `AfterCommit`; bulk writes fire `After` and `AfterCommit` per row from `RETURNING`, with `OLD` values for the columns in `Needs` |
| interceptors | `db.Intercept(keel.ForTable(handle, fn))` | add predicates, enforce access rules |
| observers | `Config.Observers` | tracing, metrics, slow-query log, N+1 detection |

Hooks are resolved per table when registered; a table without hooks pays nothing.

## Errors

`ErrNotFound`, `ErrNotSingular`, `ErrConflict` (`*ConflictError`), `ErrNoScope`,
`ErrTenantMismatch`, `ErrAllTenantsRequired`, `ErrUnbounded`, `ErrReadOnly`; constraint helpers
`IsUnique`, `IsForeignKey`, `IsCheck`, `IsExclusion`, `Code`, `Constraint`.

## Performance rules

- One allocation budget per query shape is tracked by benchmarks in `bench/`; a change that raises
  allocations or time on a benchmark explains why in its commit.
- No `sync.Map` and no generic type with methods in any type reachable from `schema.Table`.
- Statement text never contains values, so pgx's description cache and Postgres's
  `pg_stat_statements` see one entry per shape.
