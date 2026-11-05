# Testing and performance

## Testing an application

- Get one database per test binary from `migrate.Template` (built once per migrations + `Key`, copied in milliseconds),
  with `Prepare` loading reference seeds; open the app's `keel.DB` with its real `Config`; call the
  returned drop function at the end.
- Give **each test its own random tenant** on its context. Tenancy keeps tests apart, so they can
  run with `t.Parallel()` against one database and need no cleanup.
- Test system jobs under a system scope; assert scope errors with `errors.Is(err, keel.ErrNoScope)` etc.
- Assert SQL without a server: `ToSQL` on a `DB` opened on an address nothing listens on (the pool
  connects lazily), or `keeltest.Snapshot(t, sql, args)` golden files under
  `testdata/keel/<test>.sql` (`KEEL_UPDATE=1 go test` rewrites them).
- Assert round trips with a recording observer (statements of one round trip share `Start`) or a
  `keel.Budget(ctx, keel.MaxRoundTrips(n))`.
- Assert an index serves a query: `keeltest.UsesIndex(t, ctx, db, q, "idx_name")` (generic plan,
  seq scans disabled).
- Exercise `keel.Retry` by returning `&pgconn.PgError{Code: "40001"}` from the transaction function.

```go
func TestMain(m *testing.M) {
    ctx := context.Background()
    dsn, drop, err := migrate.Template(ctx, migrate.TemplateOptions{
        Admin:   adminDSN,
        Sources: schema.Sources,
        Key:     referenceDataKey,    // a string that changes when what Prepare loads changes
        Prepare: applyReferenceSeeds, // must close its connections before returning
    })
    if err != nil {
        log.Fatal(err)
    }
    testDB = openDB(ctx, dsn)
    code := m.Run()
    testDB.Close()
    _ = drop(ctx)
    os.Exit(code)
}
```

(Adapt to the project's existing harness; look for one before writing a new one.)

## Schema drift: keel.Verify

```go
func TestSchemaMatchesModels(t *testing.T) {
    drifts, err := keel.Verify(context.Background(), testDB, keeltables.All())
    if err != nil {
        t.Fatal(err)
    }
    for _, d := range drifts {
        t.Error(d) // missing_table, missing_column, primary_key, nullable, generated, json, type, join_table, relation_key
    }
}
```

`keel.VerifyFuncs(ctx, db, decls...)` checks declared DB functions. `keel gen -check` keeps generated
code in step with models; `Verify` keeps models in step with migrations. Run both in CI.

## Plan guard

Record every statement shape a test run sends, then explain them against realistic statistics:

```go
obs, closeShapes, err := keel.RecordShapesFile(os.Getenv("KEEL_SHAPES")) // add obs to Config.Observers
```

```sh
KEEL_SHAPES=/tmp/shapes.jsonl go test ./...
keel plans -dsn "$STAGING_DSN" -shapes /tmp/shapes.jsonl -sort-limit
```

It flags sequential scans of large tables, plans over a cost, and pages sorted in full.

## Performance rules

**Round trips**

- One read + its whole relation tree = one round trip outside a transaction (and in
  `RepeatableRead` transactions). Give limited queries an `Order` so loads pipeline.
- A page and its total share a round trip; `keel.Batch` puts independent reads in one.
- With `OnBegin`, a lone terminal is still one round trip.
- Inside `Read`/`Write` each terminal is a round trip: batch reads there too.
- To-one loads are joins: free. Prefer them to separate queries.

**Statements**

- Values are always bound, so each query shape is one prepared statement. Keep shapes few: build
  `Raw` text from constants, use `In` (one array) rather than generated `OR` chains.
- `StatementCacheCapacity` (default 512) should exceed the distinct shapes a connection runs.
- Tenant-size-skewed queries: `CustomPlan()` on that query, or `PlanCacheCustom` globally.
- Enum constants are written into SQL so generic plans can use partial indexes; use `keel.Const*`
  for expressions that must match an expression index or partial-index predicate.

**Writes**

- `CreateMany`/`Upsert`/`CreateOrSkip`: one array per column, 10k rows per statement, one round trip.
  `Copy` for very large loads. Never a loop of `Create`.
- Bulk `Update`/`Delete` are one statement unless hooks need the rows.
- `keel.Unnest` for per-row values in a single `UPDATE`.

**Indexes**

- Lead every index on a tenant table with the tenant columns.
- Keyset page `Order(a.Desc())` → index `(tenant cols, a DESC, pk DESC)`; keep page orders on
  non-nullable columns sorted one way.
- To-many loads → `(tenant cols, fk[, load order col])`; m2m join tables → `(local_fk, tenant cols)`.
- Prefer `Exists`, `CountUpTo`, `CountEstimate`, `Total(cap)` over exact `Count` on large tables.

**Build time**

- `keel.Query[T]` and `keel.Handle[T]` are interfaces on purpose: don't wrap generated tables in
  your own generic structs with methods (that reintroduces per-package recompilation). Keep
  generated packages imported only by data-access packages.
