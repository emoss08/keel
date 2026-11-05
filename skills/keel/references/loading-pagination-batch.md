# Loading relations, pagination and batching

## Load

`Load` takes relation paths and fills the relation fields of the returned rows. A path loads every
relation on it (`m.Stops().Location()` loads moves, stops and locations).

```go
s := fleetdb.Shipments
m := s.Moves()
cu := s.Customer()

rows, err := s.
    Where(s.Status.Eq(fleet.StatusInTransit)).
    Load(
        cu.Pick(cu.Name, cu.Code),                       // narrow columns (keys added automatically)
        s.BillTo(),                                      // nil when bill_to_id is NULL
        m.Where(m.Status.Ne(fleet.StatusCanceled)).      // filter the related rows
            Order(m.Sequence.Asc()).                     // order each parent's slice
            Load(m.Stops().Location()),                  // nested loads
        s.Tags(),                                        // many-to-many
        s.OriginStop().Location(),                       // pick relation
    ).
    Order(s.CreatedAt.Desc()).
    Limit(50).
    List(ctx, db)
```

| Method on the relation handle | To-many load | To-one load |
|---|---|---|
| `Where` | filters related rows | joins on it: non-matching → nil field, parent still returned |
| `Order` | orders each parent's slice | ignored |
| `Pick` / `Omit` | narrows columns | same |
| `Limit(n)` | at most n per parent (one lateral statement for all parents) | ignored |
| `Load` | nested loads | same |
| `Compute` | fills computed fields | same |

Rules:

- A load path must start at the query's table: `s.Load(s.Moves().Stops())`, never
  `s.Load(fleetdb.Moves.Stops())`.
- To-one relations (`belongs`, `one`) are LEFT JOINed into the parent statement: free.
- To-many and many-to-many: one statement per relation per level, filtered on the parents' keys.
- Picks join into a statement that reads at most two rows without an order (`One`, `Get`);
  otherwise one lateral statement per pick for all parents.
- Many-to-many loads take no `Limit`; pick loads take `Where` but no `Limit`.
- Interceptors and read filters apply to load statements; `AfterLoad` hooks run on every loaded row.
- Under a system scope, loads cross tenants only when the query (or the load) says `AllTenants()`.

### One round trip

With the default `keel.LoadPipelined`, a read and its whole tree go in one round trip: each level
selects its parents' keys with a subquery repeating the parent statement, inside an implicit
`REPEATABLE READ READ ONLY` transaction. keel falls back to one round trip per level when:

- the read is in a `Read`/`Write` at read committed (the default), or in a `JoinTx` transaction;
- a parent level has `Limit`/`Offset` without `Order` (`First`, or `Limit` without `Order`), locks,
  uses `DISTINCT ON`, groups, or is a per-parent `Limit` load without `Order`.

So: give limited queries an `Order` (keel appends the PK as a tie-break), and run multi-statement
read transactions with `keel.RepeatableRead` (or set `Config.ReadIsolation = keel.RepeatableRead`)
when they load trees. A query using a volatile function or an unordered `Limit` inside a CTE/lateral
needs `LoadStrategy(keel.LoadPerLevel)`.

### Per-parent limits

```go
rows, err := s.Load(m.Order(m.Sequence.Desc()).Limit(1)). // each shipment's last move
    Order(s.CreatedAt.Desc()).Limit(50).List(ctx, db)
```

For a fixed "first/last related row" used in many places, prefer a `pick:` relation on the model.

## Keyset pages

```go
page, err := s.
    Where(s.Status.In(fleet.StatusNew, fleet.StatusAssigned)).
    Load(s.Customer()).
    Order(s.CreatedAt.Desc()).
    Page(ctx, db, keel.After(cursor, 50).Total(10_000)) // "" cursor = first page
```

| `keel.Page[T]` | Meaning |
|---|---|
| `Items` | `[]*T` with loads |
| `Next` | cursor of the next page; `""` on the last page |
| `Total` | matching rows when asked: exact up to the cap, else the cap ("10,000+"), or with `.Estimate()` the planner's estimate ("about 1.2M") |
| `TotalExact` | `Total` is exact |

- The order may use the table's columns, to-one relation columns (`s.Customer().Name.Asc()`),
  `keel.ResolvePath(...)` and expressions. To-many columns are refused.
- keel appends every PK column the order lacks, so positions are unique; no order pages by PK.
- The cursor is opaque base64url; pass it only to the query that produced it.
- Fast path: all terms one direction, no `NullsFirst/Last`, non-nullable table columns → a row
  comparison an index on `(tenant cols, order cols…, pk)` serves. Otherwise an `OR` chain.
- `keel.CursorAt(page, i)` is the cursor after row i (GraphQL edge cursors), without a statement.

```go
page, err := s.Order(s.CreatedAt.Desc()).Page(ctx, db, keel.After(after, first))
edges := make([]Edge, len(page.Items))
for i, sh := range page.Items {
    edges[i] = Edge{Node: sh, Cursor: keel.CursorAt(page, i)}
}
info := PageInfo{HasNextPage: page.Next != ""}
```

Index for `Order(s.CreatedAt.Desc())` on a table keyed `(id, organization_id, business_unit_id)`:

```sql
CREATE INDEX idx_shipments_created ON shipments (organization_id, business_unit_id, created_at DESC, id DESC);
```

## Counting

- `Exists`: stops at the first row.
- `CountUpTo(ctx, db, n)` / `Total(n)`: stop counting at n.
- `CountEstimate(ctx, db, exactUpTo)` / `Total(n).Estimate()`: exact up to n, planner estimate above
  (display only, never logic). `Estimate()` without `Total` fails.
- `Count`: exact; reads every matching row.

## Each

```go
err := s.Where(s.Hazmat.IsTrue()).Each(ctx, db, 500, func(ctx context.Context, batch []*fleet.Shipment) error {
    return process(ctx, batch)
})
```

Keyset batches at constant memory; each batch is its own statement. Wrap in
`keel.Read(…, keel.RepeatableRead)` for one snapshot (holds a connection). `Offset(n)` exists but
avoid it for anything a person scrolls.

## Batch

`keel.Batch` sends independent reads in one round trip and fills the destinations only if every
item succeeded.

```go
open := s.Where(s.Status.In(fleet.StatusNew, fleet.StatusAssigned))

var rows []*fleet.Shipment
var total int64
var byStatus []statusCount
var cust *fleet.Customer
err := keel.Batch(ctx, db,
    keel.ListInto(open.Clone().Order(s.CreatedAt.Desc()).Limit(50), &rows),
    keel.CountInto(open.Clone(), &total),
    keel.IntoSlice(s.Group(s.Status).Pick(s.Status, keel.Count()), &byStatus),
    keel.OneInto(c.Where(c.ID.Eq("cus_01")), &cust),
)
```

| Item | Like |
|---|---|
| `keel.ListInto(q, &rows)` | `List` (needs `Limit`) |
| `keel.AllInto`, `OneInto`, `FirstInto` | `All`, `One`, `First` |
| `keel.PageInto(q, req, &page)` | `Page` |
| `keel.CountInto`, `CountUpToInto(q, n, &n)`, `CountEstimateInto(q, n, &c)` | counts |
| `keel.ExistsInto(q, &ok)` | `Exists` |
| `keel.IntoSlice[R](q, &rs)`, `keel.IntoValue[R](q, &r)` | `keel.Into`, `keel.IntoOne` |
| `keel.DynInto`, `DynCountInto`, `DynPageInto`, … | dynamic queries |

Note `open.Clone()`: queries are mutable builders, so clone a shared base before extending it.
Each item keeps its terminal's checks (`ErrUnbounded`, `ErrNotFound`, scope errors). The shortest
`Timeout` bounds the batch. Relation loads of all items share the round trip where they pipeline.
Use `.Cache()` on every item to serve the whole batch from the result cache.
