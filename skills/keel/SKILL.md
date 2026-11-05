---
name: keel
description: Writes idiomatic, correct and fast Go code for keel (github.com/emoss08/keel), the typed multi-tenant data layer for PostgreSQL built on pgx v5. Use whenever code imports github.com/emoss08/keel or a keel-generated `…db` package, defines structs that embed keel.Table or carry `keel:"…"` tags, runs `keel gen`, writes queries, relation loads, pagination, writes, transactions, hooks, interceptors, tenancy or system scopes, or works on keel migrations (keel/migrate), seeds (keel/seed), keelcli binaries, keeltest, dynamic queries or keel/analytics reports. Also use when moving code from Bun, GORM, ent, sqlx or raw pgx to keel.
license: MIT
---

# Writing code with keel

keel is a typed, multi-tenant data layer for Go and PostgreSQL. You write plain structs tagged
`keel:"…"`; `keel gen` writes a typed package for them (`fleet` → `fleet/fleetdb`). Every query,
filter, relation load and write goes through that generated package, so the compiler checks columns
and values, and every statement is scoped to the caller's tenant before it reaches Postgres.

The examples use keel's own test models (`fleet.Shipment`, `fleet.Customer`, `fleet.Move`,
`fleet.Stop`, …) and their generated package `fleetdb`. In an application, substitute its own model
package and `<name>db` package. `db` is the application's `*keel.DB`, `ctx` a request context that
carries a tenant.

## The mental model

- **Models are plain structs; the schema lives in SQL migrations.** keel never generates DDL from
  structs. A model change is: edit the struct → write a migration → run `keel gen` → run
  `keel.Verify` in a test.
- **Handles and columns are generated values.** `s := fleetdb.Shipments` is the table handle;
  `s.Status` is a typed column; `s.Customer()` is a relation path. Queries are values of the
  interface `keel.Query[*fleet.Shipment]`. Never write column names as strings.
- **The context carries everything ambient:** the tenant (via `Config.Tenant`), a system scope (via
  `Config.System`), the transaction (`keel.Write`/`keel.Read`), a replica mark, a budget. Pass
  `ctx` through; never pass a `tx` object.
- **Every terminal takes `(ctx, db)`.** Builders (`Where`, `Load`, `Order`, `Limit`, …) only build;
  terminals (`One`, `List`, `Page`, `Create`, `Update`, …) run.
- **Round trips are the unit of cost.** keel pipelines aggressively: a read and its whole relation
  tree, a page and its total, a `keel.Batch` of reads, `BEGIN`+`OnBegin`+statement+`COMMIT` are each
  one round trip. Write code that lets it do that.

## Non-negotiable rules

1. **Use the generated package for every query and write.** `fleetdb.Shipments.Where(...)`, never
   `keel.QueryOf` on an application model and never hand-built SQL for something the typed API
   covers. Only data-access packages should import generated packages.
2. **Never put values into SQL text.** Values are bind parameters. In `keel.Raw("length(?) > ?",
   s.ProNumber, 8)` every `?` is an argument (`??` is a literal `?`). Never `fmt.Sprintf` SQL.
3. **Never add tenant filters by hand.** keel adds `organization_id = $1 AND …` to every read, join,
   load, subquery and write on a tenant table, and fills tenant columns on insert. Don't filter on
   tenant columns, don't set them on new rows, don't list them in relation keys.
4. **Crossing tenants is explicit.** Put a system scope on the context (the application's own
   helper that `Config.System` reads, with a reason) *and* call `.AllTenants()` on the query (and on
   each subquery). Never fake a tenant to read another tenant's data.
5. **`List` needs `Limit`.** Use `Page` (keyset) for anything a person scrolls, `Each` to walk a
   large set, `All` only for sets known to be small. Avoid `Offset`.
6. **Bulk `Update`/`Delete` need a `Where`.** Use `Where(keel.True())` to mean every row of the tenant.
7. **Load relations with `Load`, never in a loop.** No lazy loading exists; a `Get` per row is an N+1
   that `keel.Budget(ctx, keel.MaxRepeats(n))` will fail.
8. **Transactions are `keel.Write` / `keel.Read` closures** that take and use the inner `ctx`.
   Return an error to roll back. Side effects that must not happen on rollback go in
   `keel.AfterCommit` or a `keel.Committed` table hook.
9. **Errors are sentinels; compare with `errors.Is`/`errors.As`.** `keel.ErrNotFound` (not
   `sql.ErrNoRows`/`pgx.ErrNoRows`), `keel.ErrConflict` / `*keel.ConflictError`, scope errors,
   `keel.IsUnique(err, "constraint")`, `*keel.ConstraintError`.
10. **pgx v5 only.** keel never uses `database/sql`. Raw pool access is `keel.Pool(db)` and bypasses
    scopes, interceptors and hooks: avoid it.
11. **Don't edit generated files** (`keel_gen.go`, the `keelkinds` and `keeltables` packages). Change
    the model and rerun `keel gen`; CI runs `keel gen -check`.

## Canonical patterns

```go
s, c := fleetdb.Shipments, fleetdb.Customers
m := s.Moves()

// One row by primary key / by a unique filter.
sh, err := s.Get(ctx, db, id)                               // keel.ErrNotFound when missing
sh, err = s.Where(s.ProNumber.Eq("PRO-1001")).One(ctx, db)  // ErrNotFound / ErrNotSingular

// A bounded list with optional filters, relation filters and relation loads.
rows, err := s.Where(
    keel.When(f.Search != "", s.ProNumber.ContainsFold(f.Search)), // nil predicates are skipped
    keel.When(len(f.Statuses) > 0, s.Status.In(f.Statuses...)),    // In binds ONE array param
    s.Customer().Code.Eq("ACME"),                                  // to-one path: LEFT JOIN
    m.Any(m.Status.Eq(fleet.StatusInTransit)),                     // to-many path: EXISTS
).
    Load(s.Customer(), m.Order(m.Sequence.Asc()).Load(m.Stops().Location())).
    Order(s.CreatedAt.Desc()).
    Limit(50).
    List(ctx, db)

// A keyset page with its total, in one round trip.
page, err := s.Where(s.Hazmat.IsTrue()).Order(s.CreatedAt.Desc()).
    Page(ctx, db, keel.After(cursor, 50).Total(10_000)) // page.Items, page.Next, page.Total, page.TotalExact

// Independent reads of one screen: one round trip.
var recent []*fleet.Shipment
var open int64
var cust *fleet.Customer
err = keel.Batch(ctx, db,
    keel.ListInto(s.Where(s.Status.Eq(fleet.StatusNew)).Order(s.CreatedAt.Desc()).Limit(50), &recent),
    keel.CountInto(s.Where(s.Status.Ne(fleet.StatusDelivered)), &open),
    keel.OneInto(c.Where(c.Code.Eq("ACME")), &cust),
)

// Projections and aggregates decode into a struct whose fields match the picks in order.
type statusTotal struct {
    Status fleet.Status
    Loads  int64
}
totals, err := keel.Into[statusTotal](ctx, db, s.Group(s.Status).Pick(s.Status, keel.Count()))

// Writes: IDs (autoid), tenant columns and timestamps are filled for you.
err = c.Create(ctx, db, &fleet.Customer{Code: "ACME", Name: "Acme Freight"})
err = fleetdb.Stops.CreateMany(ctx, db, stops)                 // one array per column, one round trip
sh.Status = fleet.StatusAssigned
err = s.Save(ctx, db, sh, s.Status)                             // optimistic lock if the model has `version`
if errors.Is(err, keel.ErrConflict) { /* reload and retry, or tell the user */ }
n, err := s.Where(s.Status.Eq(fleet.StatusNew), s.Customer().Code.Eq("ACME")).
    Update(ctx, db, s.Status.To(fleet.StatusCanceled), s.Priority.Increment(1))
inserted, err := c.Upsert(ctx, db, custs,
    keel.OnConflict(c.OrganizationID, c.BusinessUnitID, c.Code).Update(c.Name))

// A transaction travels on the context.
err = keel.Write(ctx, db, func(ctx context.Context) error {
    sh, err := s.Where(s.ID.Eq(id)).Lock(keel.ForUpdate, keel.NoWait).One(ctx, db)
    if err != nil {
        return err
    }
    sh.Status = fleet.StatusAssigned
    if err := s.Save(ctx, db, sh, s.Status); err != nil {
        return err
    }
    keel.AfterCommit(ctx, func(ctx context.Context) { publish(sh.ID) })
    return nil
}, keel.LockTimeout(2*time.Second))
```

## Choosing the right tool

| Need | Use |
|---|---|
| row by key | generated `Get(ctx, db, key)` |
| exactly one row | `One`; first in an order: `First` |
| bounded list | `Limit(n).List`; small known set: `All` |
| scrolling list / API page | `Page(ctx, db, keel.After(cursor, n).Total(cap))`; GraphQL edge cursors: `keel.CursorAt(page, i)` |
| walk everything | `Each(ctx, db, size, fn)` |
| counts | `Exists` > `CountUpTo(limit)` > `CountEstimate(exactUpTo)` > `Count` on large tables |
| several independent reads | `keel.Batch` with `ListInto`, `CountInto`, `OneInto`, `PageInto`, `IntoSlice`, … |
| related rows | `Load(...)`; filter by them: path columns (to-one) or `Any`/`None` (to-many) |
| latest/first related row | a `pick:` relation, or a per-parent `Limit` load: `Load(m.Order(m.Sequence.Desc()).Limit(1))` |
| aggregates, columns of other tables | `Pick(...)` + `keel.Into[R]` / `keel.IntoOne[R]` |
| value computed onto the model | a `computed` field + `Compute(field, expr)` |
| many inserts | `CreateMany`; tens of thousands+: `Copy`; may conflict: `Upsert`/`CreateOrSkip` |
| per-row values in one UPDATE | `keel.Unnest(name, cols, slices...)` + `With(..).Join(..).Update(..)` |
| changed rows back from an update | `UpdateReturning` |
| replace a many-to-many set | `keel.SetLinks(ctx, db, s.Tags(), sh.ID, tagIDs)` |
| optional filter | `keel.When(cond, pred)` |
| filter/sort by a client field name | `keel.ResolvePath(handle, "customer", "name")` |
| columns chosen at run time | `keel.Registry` / `keel.DynQuery` (see dynamic reference) |
| SQL keel can't express | typed functions first, then `keel.Raw` with `?` args; whole statements `keel.Exec`/`keel.QueryAll` |

## Anti-patterns to fix on sight

| Don't | Do |
|---|---|
| `Where("status = ?", st)`, string column names | `s.Status.Eq(st)` |
| `s.OrganizationID.Eq(org)` in every query | nothing: the scope adds it |
| `for _, sh := range rows { c.Get(ctx, db, sh.CustomerID) }` | `Load(s.Customer())` on the original read |
| `List` without `Limit`, `Offset` paging | `Limit(n)`, `Page` with a cursor |
| `Count` then `List` as two calls | `Page(… .Total(cap))` or `keel.Batch` |
| `for _, r := range rows { Create(r) }` | `CreateMany(rows)` / `Copy(rows)` |
| read-modify-write without a version column or lock | `version` column + `Save` (`ErrConflict`), or `Lock(keel.ForUpdate)` inside `Write` |
| `keel.Write(ctx, …, func(_ context.Context) error { s.Create(ctx, …) })` | use the closure's `ctx` |
| `fleetdb.Customers.Code.Eq(..)` to filter shipments by customer | the path handle: `s.Customer().Code.Eq(..)` |
| `s.Load(fleetdb.Moves.Stops())` | `s.Load(s.Moves().Stops())`: a load path starts at the query's table |
| `keel.DynFunc`/`keel.DynOf` inside typed queries | typed forms: `a.Add(b)`, `keel.CastNumeric(x)`, `keel.CaseString()…` |
| publishing events inside `Write` | `keel.AfterCommit` / `keel.Committed` hook |
| setting `ID`, `CreatedAt`, tenant columns on new rows | leave them zero: `autoid`, `autocreate`, `tenant` fill them |
| `errors.Is(err, sql.ErrNoRows)`, matching error strings | `errors.Is(err, keel.ErrNotFound)`, `keel.IsUnique(err, "uq_…")` |
| a `*pgx.Tx`/`bun.IDB` parameter on repository methods | take `ctx`; the transaction rides on it |
| `default:`/`notnull` tags, expecting zero → DEFAULT | the migration owns defaults; tag `default` only to take the DB default for an unset field |
| empty-string foreign keys | pointer field or `nullzero` so empty is NULL |

## Before you finish

- Generated code is current: `go run github.com/emoss08/keel/cmd/keel gen ./…` (or `go tool keel gen`)
  with the project's usual flags, after any model change. Look at how the project already invokes it
  (Makefile, `//go:generate`, CI) and use exactly that.
- A migration exists for every model change; indexes on tenant tables lead with the tenant columns.
- New query shapes have a `Limit`/`Page`, load relations in the same read, and batch independent reads.
- `go vet ./...` and the tests pass; `keel.Verify` (if the project has the test) reports no drift.

## Reference

Read the file that matches the task; each is self-contained.

- [references/models-and-gen.md](references/models-and-gen.md): tags, Go types, relations, picks,
  nullzero, computed fields, model hooks, constraint messages, `keel gen` flags and output.
- [references/reading.md](references/reading.md): terminals, operators per column type, typed
  functions/CASE/arrays/constants, raw SQL, paths, joins, subqueries, lateral, CTEs, set operations,
  aggregates, windows, grouping sets, locks, full-text search, `ToSQL`, `Explain`.
- [references/loading-pagination-batch.md](references/loading-pagination-batch.md): `Load`
  semantics and its one-round-trip rules, per-parent limits, keyset pages and cursors, estimates,
  `Each`, `Batch`.
- [references/writing.md](references/writing.md): `Create`, `CreateMany`, `Copy`, `Save`,
  `SaveExcept`, `DeleteRow`, bulk `Update`/`Delete`, `UpdateReturning`, `Unnest`, `Upsert`,
  `CreateOrSkip`, `InsertFrom`/`UpsertFrom`, `SetLinks`.
- [references/transactions-and-tenancy.md](references/transactions-and-tenancy.md): `Read`/`Write`,
  options, isolation, `Detached`, savepoints, advisory locks, `AfterCommit`, scopes and
  `AllTenants`, tenant concurrency, waits, budgets, replicas, data versions, the result cache.
- [references/hooks-errors-observability.md](references/hooks-errors-observability.md): table hooks
  and `Mutation`, which hooks run, interceptors, read filters, observers, `keellog`, errors and
  constraint helpers.
- [references/config-migrations-seeds.md](references/config-migrations-seeds.md): `keel.Config`,
  pool and statement preparation, PgBouncer, JSON/enums/decimals/PostGIS/pgvector, migrations,
  seeds, `keelcli`.
- [references/testing-and-performance.md](references/testing-and-performance.md): test setup with
  templates and per-test tenants, `keel.Verify`, `keeltest`, the plan guard, indexes and
  performance rules.
- [references/dynamic-and-analytics.md](references/dynamic-and-analytics.md): `keel.Registry`,
  `DynQuery`, `DynResult`, streaming, and pointers into `keel/analytics`.
- [references/migrating-from-other-libraries.md](references/migrating-from-other-libraries.md): Bun
  (and GORM/sqlx) idioms mapped to keel, running alongside another layer with `keel.JoinTx`.

The full documentation lives in the keel repository: `docs/guide.md` (by task), `docs/design.md`
(the contract), `example_test.go` (compiled examples). When this skill and the installed keel
version disagree, the module's source in the Go module cache is authoritative
(`go list -m -f '{{.Dir}}' github.com/emoss08/keel`).
