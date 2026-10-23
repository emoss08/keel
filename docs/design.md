# keel design

keel is a typed, Postgres-only data layer for Go. It is built for one large application
(Trenova: hundreds of thousands of users across thousands of tenants) and optimised in this
order: correctness and tenant isolation, runtime performance, developer experience, build time.

This document is the contract. Code that disagrees with it is a bug in one of the two; fix the one
that is wrong in the same change. [guide.md](guide.md) shows how to use keel;
[benchmarks.md](benchmarks.md) records how it performs.

## Goals

- **Typed end to end.** Columns, relations, values, assignments and results are checked by the
  compiler. Raw SQL exists, is explicit (`keel.Raw`) and is greppable.
- **Little server work per statement.** Values are bind parameters, so a statement shape has one
  text; it is prepared once per connection and Postgres skips parsing and, usually, planning.
- **One round trip where Postgres allows it.** Transactions start with their first statement, a
  lone statement runs as one pipelined `BEGIN; …; COMMIT`, and independent statements of one read
  share a round trip.
- **No reflection on the hot path.** `keel gen` writes field accessors for every model; rows decode
  straight from pgx's buffers into struct fields through pgx scan plans chosen once per result.
- **Tenancy by construction.** The tenant comes from the context and is applied to every statement
  on a tenant table: reads, joins, relation loads, subqueries, updates, deletes and inserts.
- **Bounded by default.** `List` needs a limit, keyset pagination is built in, bulk writes stay
  under Postgres's parameter limit and share a round trip.
- **Cheap to compile.** Generated code is static data plus small non-generic functions. Generic
  types appear only where a typed result needs them (`Query[T]`, `Handle[T]`) and are reachable only
  from data-access packages.

Non-goals: other databases, schema migrations generated from models (migrations are SQL files),
soft delete, an identity map or a session cache.

## Layers

| Package | Role |
|---|---|
| `keel` | the public API: `DB`, transactions, scope, `Handle[T]`, `Query[T]`, expressions, columns, writes, hooks, errors |
| `keel/schema` | runtime table metadata, written as static literals by the generator; the `db` tag parser |
| `keel/gen`, `cmd/keel` | the generator: loads packages with `go/packages`, finds models, writes one `<pkg>db` package per model package |
| `keel/migrate` | the SQL-file migration runner |
| `bench` | a separate module comparing keel with Bun and raw pgx |

The wire layer is pgx v5 (`pgxpool`, `pgx.Batch`, pgx scan plans). keel never uses `database/sql`.

## Models

A model is a plain struct that embeds `keel.Table`, an empty marker whose `db` tag names the table.
Every stored field carries a `db` tag.

```go
type Shipment struct {
    keel.Table `db:"shipments"`

    ID             pulid.ID  `db:"id,pk,autoid:shp_"`
    OrganizationID pulid.ID  `db:"organization_id,pk,tenant"`
    BusinessUnitID pulid.ID  `db:"business_unit_id,pk,tenant"`
    CustomerID     pulid.ID  `db:"customer_id"`
    Status         Status    `db:"status"`
    SearchVector   string    `db:"search_vector,noselect,readonly,tsvector"`
    Version        int64     `db:"version,version"`
    CreatedAt      int64     `db:"created_at,autocreate"`
    UpdatedAt      int64     `db:"updated_at,autoupdate"`

    Customer *customer.Customer `db:"belongs:customer_id"`
    Moves    []*ShipmentMove    `db:"many:shipment_id"`
}
```

Table aliases are keel's business: every table in a statement is named `t0`, `t1`, …; a handle's
`As("name")` gives a second reference to a table for self-joins.

### Column options

| Option | Meaning |
|---|---|
| `pk` | part of the primary key |
| `tenant` | a tenant column: filled from the scope on insert, compared with the scope on every statement, added to every relation join |
| `autoid:<prefix>` | on insert, an empty value is replaced by `Config.NewID(prefix)`; the field must be a string type |
| `autocreate` / `autoupdate` | stamped on insert / on insert and every update, bulk updates included; `int64` means Unix seconds, `time.Time` a timestamp |
| `version` | optimistic locking: `Save` compares and increments it, bulk updates and upserts increment it; must be `int64` |
| `readonly` | never written (generated or trigger-maintained columns); read back after writes |
| `noselect` | not selected by default (a `tsvector`); usable in predicates. A generated `noselect` column is also `readonly` |
| `jsonb` | the value is encoded as JSON; a nil map or slice in a non-pointer field is written as `{}` or `[]` |
| `tsvector` | the column gets the full-text operators |

There is no `default:`, `type:` or `notnull`. The SQL migrations own the schema; nullability follows
the Go type (a pointer, or a `sql.Scanner` named `Null…`). The value in the struct is the value
written: keel never substitutes `DEFAULT` for a zero value. A nil slice in a non-pointer array field
is written as an empty array.

Column kinds follow the Go type: strings, `int64`, `int32`, `int16`, `float64`, `bool`, `[]byte`,
`time.Time`, `[]string`, `[]int64`, named types with those underlying types (enums, IDs), and any
type implementing `sql.Scanner` (a decimal). Plain `int` is refused: its width is not portable.

### Relations

| Tag | Meaning |
|---|---|
| `belongs:<fk>` | this table's `<fk>` references the target's primary key |
| `one:<fk>` | the target's `<fk>` references this table's primary key, at most one row |
| `many:<fk>` | the target's `<fk>` references this table's primary key |
| `m2m:<join_table>.<local_fk>.<remote_fk>` | rows linked through a join table |

The primary key of a related table, besides its tenant columns, must be a single column. When both
sides have tenant columns, keel adds them to the join: as constants from the scope under a tenant
scope, as column equalities under a system scope.

### Model hooks

Methods on the model with standard-library signatures. The generator detects them and writes direct
calls; there is no runtime type assertion.

```go
func (s *Shipment) BeforeCreate(ctx context.Context) error
func (s *Shipment) BeforeUpdate(ctx context.Context) error
func (s *Shipment) BeforeSave(ctx context.Context) error   // create and update
func (s *Shipment) BeforeDelete(ctx context.Context) error // DeleteRow
func (s *Shipment) AfterLoad(ctx context.Context) error    // every loaded row, relations included
```

## Generated code

`keel gen ./internal/...` writes, for each package that holds models, a package named
`<name>db` (`-out` sets the path template). For each model it writes:

- the `schema.Table` as a static literal: columns, flags, relations, precomputed column sets, and
  accessor functions (`New`, `Ptr` for scanning, `Value` for binding, `Attach` for relations) plus
  hook trampolines;
- a handle type `ShipmentsTable` embedding `keel.Handle[shipment.Shipment]`, with one field per
  column, one method per relation, `As`, and a typed `Get` by primary key;
- a package variable `Shipments` built statically, and `BuildShipments`, which relation and alias
  methods use;
- a column type per named Go type (`StatusCol`), so `Status.In(...)` accepts only
  `shipment.Status`. Types declared outside model packages (`pulid.ID`) get their column type in a
  shared package (`-kinds`).

Relation targets are linked by a small generated `init` (pointer assignments only), because two
tables of one package that reference each other would be an initialisation cycle as static data.
Handles reached through relations (`s.Customer()`) are built on first use and memoised.
`keel gen -check` fails when generated files are out of date.

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
    Order(s.CreatedAt.Desc()).
    Page(ctx, db, keel.After(cursor, 50).Total(10_000))
```

### Builder methods

`Where`, `Pick`, `Load`, `Order`, `Group`, `Having`, `Limit`, `Offset`, `Lock`, `Join`,
`LeftJoin`, `JoinLateral`, `LeftJoinLateral`, `DistinctOn`, `AllTenants`, `Timeout`, `CustomPlan`.
Builders are mutable and chainable; `Clone` copies one. `ToSQL` renders the main statement.

### Terminal methods

| Method | Result |
|---|---|
| `One` | exactly one row, `ErrNotFound` when none, `ErrNotSingular` when more |
| `First` | the first row by the query's order, `ErrNotFound` when none |
| `List` | rows; returns `ErrUnbounded` without `Limit` |
| `All` | every row; for sets known to be small |
| `Page` | a keyset page with an opaque cursor and an optional capped total, counted in the same round trip |
| `Count`, `CountUpTo`, `Exists` | scalars |
| `Each` | keyset batches at constant memory |
| `Update`, `Delete` | bulk writes over the query's filter; rows affected. A query without `Where` is refused |
| `keel.Into[R]`, `keel.IntoOne[R]` | projections: the picked expressions fill `R`'s exported fields in order |

### Expressions

Every clause takes expressions, never strings. Columns are typed by their Go kind:

| Kind | Operators |
|---|---|
| every column | `IsNull`, `IsNotNull`, `EqCol`, `NeCol`, `InQuery`, `NotInQuery`, `Asc`, `Desc`, `ToNull`, `ToDefault`, `ToExpr`, `Excluded`, `Count`, `CountDistinct`, `Min`, `Max`, `Coalesce` |
| typed values | `Eq`, `Ne`, `Gt`, `Gte`, `Lt`, `Lte`, `Between`, `IsDistinctFrom`, `In`, `NotIn`, `To` |
| numbers | `Add`, `Sum`, `Avg` |
| string | `Contains`, `HasPrefix`, `HasSuffix` (escaped), `ContainsFold`, `HasPrefixFold`, `Like`, `ILike`, `Match`, `MatchFold`, `Lower` |
| bool | `IsTrue`, `IsFalse` |
| jsonb | `Contains`, `HasKey`, `HasAnyKey`, `HasAllKeys`, `Text(path...)` |
| array | `Has`, `Overlaps`, `ContainsAll`, `ContainedBy`, `Len` |
| tsvector | `Matches`, `MatchesConfig`, `Rank` |

Combinators: `keel.And`, `keel.Or`, `keel.Not`, `keel.When`, `keel.Exists`, `keel.NotExists`,
`keel.True`, `keel.Raw`, `keel.Val`. Aggregates: `keel.Count()`, `keel.Sum`, `keel.RowNumber`,
`.Filter(pred)`, `.Over(partition, order...)`. Relation paths: belongs-to and has-one columns join
automatically (`LEFT JOIN` in reads, `FROM`/`USING` in bulk writes); `Any` and `None` on a to-many
path compile to `EXISTS` and `NOT EXISTS`.

Values are bind parameters. `In` binds one array parameter (`= ANY($1)`), so the statement text does
not depend on the number of values. A column resolves to the innermost statement that references
its table; a column of a table's base handle also resolves inside a statement built on a relation
path to that table.

## Relation loading

`Load` takes relation paths, optionally narrowed with `Where`, `Order`, `Pick` and `Limit`.
Belongs-to and has-one segments are joined into the parent's statement. Has-many and many-to-many
segments run one statement per relation per level, filtered by `= ANY($1)` over the parent keys
plus the scope's tenant columns; statements of one level share a round trip. A to-many load with a
`Limit` loads at most that many rows per parent through `unnest … CROSS JOIN LATERAL`.

## Writes

```go
err := s.Create(ctx, db, sh)                       // autoid, timestamps, tenant columns filled
err = s.CreateMany(ctx, db, rows)                  // statements under 65,535 parameters, one round trip
err = s.Save(ctx, db, sh)                          // compare-and-increment version, ErrConflict on mismatch
err = s.Save(ctx, db, sh, s.Status, s.CanceledAt)  // only these columns (and the autoupdate column)
err = s.DeleteRow(ctx, db, sh)                     // ErrNotFound when nothing was deleted
n, err := s.Where(...).Update(ctx, db, s.Status.To(shipment.StatusCanceled))
n, err = s.Where(...).Delete(ctx, db)
ins, err := s.Upsert(ctx, db, rows, keel.OnConflict(s.OrganizationID, s.BusinessUnitID, s.ProNumber).Update(s.Status))
n, err = s.CreateOrSkip(ctx, db, rows, s.OrganizationID, s.BusinessUnitID, s.ProNumber)
n, err = s.InsertFrom(ctx, db, src.Pick(...), s.Name, s.Code)
```

After a write keel reads back only what Postgres may have changed: `readonly` columns, and the
version where it changed it. `Upsert` reports per row whether it inserted (`xmax = 0`).

## Transactions and scope

```go
err := keel.Write(ctx, db, func(ctx context.Context) error { ... }, keel.LockTimeout(2*time.Second))
v, err := keel.ReadValue(ctx, db, func(ctx context.Context) (T, error) { ... })
```

- The transaction lives on the context. Terminals find it there; nested `Read`/`Write` calls join
  it; a `Write` inside a `Read` returns `ErrReadOnly`.
- `BEGIN`, the transaction options and the statements from `Config.OnBegin` (for example a
  row-level security `set_config`) are sent with the first statement. A transaction that runs no
  statement costs nothing.
- Outside a transaction a terminal runs its statements in an implicit transaction when `OnBegin` is
  set or a hook needs one; a terminal with a single statement sends `BEGIN`, the `OnBegin`
  statements, the statement and `COMMIT` in one round trip.
- `Config.Tenant` returns the tenant values from the context, in the order of
  `Config.TenantColumns`. `Config.System` reports a system scope and its reason. A statement on a
  tenant table with neither is refused (`ErrNoScope`). Under a system scope a statement on a tenant
  table must say `AllTenants()` (`ErrAllTenantsRequired`), and `AllTenants()` without a system
  scope is refused (`ErrSystemScopeRequired`). Inserts under a system scope must carry their tenant
  columns; a row of another tenant is refused (`ErrTenantMismatch`).
- `keel.AfterCommit(ctx, fn)` runs `fn` after the transaction on the context commits, or at once
  outside one.
- `keel.Retry(n)` retries serialization failures and deadlocks for a top-level transaction.
  `keel.Serializable()` and `keel.RepeatableRead()` set the isolation level.

### Statement preparation

`Config.PlanCache` chooses how statements are prepared:

| Mode | Behaviour |
|---|---|
| `PlanCacheAuto` (default) | each statement shape is prepared once per connection (pgx's statement cache, `StatementCacheCapacity`, default 512); `plan_cache_mode = auto` lets Postgres reuse a generic plan when it is no worse than the custom plans of the first executions |
| `PlanCacheCustom` | prepared, but every execution is planned for its values (`force_custom_plan`) |
| `PlanCacheOff` | nothing prepared; for poolers that cannot track prepared statements (PgBouncer before 1.21) |

`Query.CustomPlan()` plans one query for its values: keel wraps it in transaction-local
`set_config('plan_cache_mode', …)` calls inside the same round trip.

## Hooks

| Kind | Registration | Use |
|---|---|---|
| model hooks | methods on the model | normalisation and derived fields |
| table hooks | `keel.OnCreate/OnUpdate/OnDelete/OnWrite[T](db, handle, phase, fn, keel.Needs(cols...))` | validation, audit, side effects |
| interceptors | `db.Intercept(handle, fn)`, `db.InterceptAll(fn)` | add predicates to every statement whose base is a table: queries, loads, subqueries, bulk writes |
| observers | `Config.Observers` | tracing, metrics, slow-query log, N+1 detection |

Table hook phases:

- `BeforeWrite` runs inside the transaction before an entity write (`Create`, `CreateMany`,
  `Save`, `Upsert`, `DeleteRow`) and may change the row or abort.
- `AfterWrite` runs inside the transaction after the statement, for entity and bulk writes; an
  error rolls back. A table with `AfterWrite` hooks makes its writes transactional.
- `Committed` runs after the transaction commits.

Bulk writes on a table with `AfterWrite` or `Committed` hooks return their rows through `RETURNING`,
with the previous values of the columns hooks declared with `Needs` through `RETURNING old.…`
(PostgreSQL 18). Hooks are resolved per table when registered; a table without hooks pays nothing.

Observers receive the statement text (never values), the number of bound arguments, duration, rows
and error; statements sent in one round trip share a start time and a duration.

## Errors

`ErrNotFound`, `ErrNotSingular`, `ErrConflict` (`*ConflictError`), `ErrNoScope`,
`ErrAllTenantsRequired`, `ErrSystemScopeRequired`, `ErrTenantMismatch`, `ErrUnbounded`,
`ErrReadOnly`; Postgres helpers `IsUnique`, `IsForeignKey`, `IsCheck`, `IsExclusion`, `IsNotNull`,
`IsRetryable`, `Code`, `Constraint`.

## Migrations

`keel/migrate` applies `<version>_<name>.up.sql` / `.down.sql` pairs in version order. An up file
runs in one transaction with its bookkeeping row; `*.notx.up.sql` runs statement by statement
outside a transaction, split at `--keel:split` lines. Runs serialise on an advisory lock taken by
polling (a waiting statement would hold a transaction that `CREATE INDEX CONCURRENTLY` waits on);
an applied file whose content changed stops `Up` with `ErrChanged`.

## Performance rules

- `bench/` tracks time and allocations per operation against Bun and raw pgx; a change that makes a
  benchmark worse explains why in its commit.
- Renders and their buffers are pooled; a statement copies its text and arguments out of the
  render.
- No `sync.Map` and no generic type with methods in any type reachable from `schema.Table`.
- Statement text never contains values.
