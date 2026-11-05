# Dynamic queries and analytics

Use typed queries whenever tables and columns are known at compile time. Use `keel.DynQuery` only
when they are chosen at run time (report builders, user-configured grids, generic exports). It keeps
the same tenancy, interceptors, bind parameters and prepared statements, and returns columnar results.

## Registry and paths

```go
reg, err := keel.NewRegistry(keeltables.All()...) // build once, share
q := reg.Query("shipments")                        // unknown name → terminals fail

pro := q.Path("pro_number")                        // SQL or Go name
customer := q.Path("customer", "name")             // to-one relations: LEFT JOIN with tenant condition
q.Select(pro, customer, q.Path("status"), q.Path("total").As("amount")).
    Where(q.Path("status").In([]string{"New", "Assigned"}), customer.ContainsFold(search)).
    Order(pro.Desc()).
    Limit(100)
res, err := q.Run(ctx, db)
```

`keel.DynFrom(table)` starts from a `*schema.Table`. A path through a to-many relation is refused:
use `q.Rel(...)`.

## Expressions

`keel.DynExpr` is an expression and a predicate: comparisons, `In(slice)` (one array param), text
(`ContainsFold`, `HasPrefix`, `Like`), arithmetic, `Coalesce`, `NullIf`, `keel.DynLit(v)`,
`Cast(keel.TypeNumeric)`, `keel.DynCase()`, `keel.DynRow`, `keel.DynArray`, `keel.DynConst`
(written into SQL; only to match indexes), aggregates (`Sum`, `Avg`, `Count`, `keel.DynCount()`,
`Filter`, `Over`), `keel.ArrayAgg`, `StringAgg`, `PercentileCont`, full-text `Match`/`Rank`, and
`keel.DynFunc(name, args...)` from a fixed allow-list. Unknown names/functions produce an expression
whose `Err()` reports the problem; validate before running.

Assign a grouped expression to a variable and reuse it in `Select`, `Group` and `Order` so its
parameters bind once and the clauses match.

## To-many relations

```go
moves := q.Rel("moves")
q.Where(moves.Any(moves.Col("status").Eq("New")))
pa := moves.PreAggregate(keel.PreAggCTE)            // reports; keel.PreAggLateral for few parents
legs := pa.Measure(keel.DynCount().As("legs"))
q.Select(q.Path("customer", "code"), legs.Sum().As("legs")).Group(q.Path("customer", "code"))
```

Pre-aggregations never fan out parent rows. Filter on measures with `Having`, not `Where`.

## Results

| Terminal | Result |
|---|---|
| `Run` | `*keel.DynResult`: `Columns []DynColumn` (Name, Type, typed vector, `IsNull(i)`), `Rows` |
| `Count`, `CountEstimate`, `Exists` | counts |
| `Page(ctx, db, keel.After(cursor, n).Total(cap))` | keyset page; `res.Next`, `res.CursorAt(i)` |
| `Stream(ctx, db, size)` | `iter.Seq2[*DynResult, error]` batches at constant memory (exports) |
| `keel.DynInto`, `DynCountInto`, `DynPageInto`, … | `Batch` items |

```go
for batch, err := range q.Stream(ctx, db, 5000) {
    if err != nil {
        return err
    }
    if err := writeCSV(w, batch); err != nil {
        return err
    }
}
```

Numeric columns come back as `keel.Decimal` (exact text); use `.Float64()` only for charts.
Inside a typed query, use `keel.ResolvePath(handle, ...)` instead of a dynamic query's `Path`.

## Safety

Identifiers only come from the schema; values, including user search text, are always parameters;
every referenced table gets the tenant predicate; system scopes need `AllTenants()`.

## keel/analytics

A semantic layer for user-built reports: `analytics/ir` (report definitions as JSON/Go values,
validation, versions), `analytics/formula` (calculated columns), a compiler to tenant-scoped SQL,
formatting and exports (CSV, Excel), result caches and rollup tables. Report metadata comes from the
models: tag the table `report[=Category]` and use `dimension`, `measure`, `agg=`, `format=`, `type=`,
`label=`, `noreport` on columns. Doc comments become descriptions.

Before writing analytics code, read the repository docs: `docs/guide.md` sections "Analytics: …"
and `docs/analytics.md` (the design). Definitions are data; never build SQL from a definition
yourself.
