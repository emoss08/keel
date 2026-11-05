# Writing

## Create

```go
sh := &fleet.Shipment{CustomerID: "cus_01", ProNumber: "PRO-1001", Status: fleet.StatusNew}
if err := fleetdb.Shipments.Create(ctx, db, sh); err != nil {
    return err
}
// sh.ID (autoid), sh.OrganizationID/BusinessUnitID (scope), CreatedAt/UpdatedAt and readonly columns are set
```

- Fills an empty `autoid` (`Config.NewID` required), tenant columns from the scope,
  `autocreate`/`autoupdate`; runs `BeforeCreate`, `BeforeSave`, `BeforeWrite` hooks; reads back
  `readonly` columns. A non-empty ID is kept.
- A row whose tenant columns differ from the scope fails with `keel.ErrTenantMismatch`. Under a
  system scope, rows must carry their tenant columns.

## CreateMany and Copy

```go
err := fleetdb.Stops.CreateMany(ctx, db, stops) // unnest of one array per column; 10k rows per chunk; one round trip
n, err := fleetdb.Shipments.Copy(ctx, db, rows) // binary COPY: imports/backfills of many thousands of rows
```

- Never loop `Create` for many rows.
- `Copy` prepares rows like `Create` and runs create hooks, but reads nothing back, and a duplicate
  key fails the whole COPY (use `CreateOrSkip`/`Upsert` when rows may conflict). It needs codecs
  registered for custom types.
- After a migration changes a table while the process runs: `db.ForgetColumnTypes()`.

## Save (optimistic locking)

```go
cust, err := c.Get(ctx, db, id)
if err != nil {
    return err
}
cust.Name = "Acme Freight"
err = c.Save(ctx, db, cust)                   // all updatable columns
err = s.Save(ctx, db, sh, s.Status, s.CanceledAt) // only these (+ autoupdate)
err = conns.SaveExcept(ctx, db, conn, conns.AccessToken) // all but these

var conflict *keel.ConflictError
switch {
case errors.As(err, &conflict): // conflict.Table, conflict.Version: reload and retry, or 409
case errors.Is(err, keel.ErrNotFound): // no version column and no row matched
case err != nil:
    return err
}
```

- With a `version` column, `Save` compares and increments it; a deleted row also reports
  `ErrConflict`. Without one, a missing row is `ErrNotFound`.
- Named columns must be updatable: not PK, tenant, `autocreate`, `version`, `readonly`, `computed`.
- Interceptors' predicates join the PK in `WHERE`: an excluded row reads as missing/conflicting.

## DeleteRow

```go
if err := fleetdb.Stops.DeleteRow(ctx, db, &fleet.Stop{ID: id}); errors.Is(err, keel.ErrNotFound) {
    // already deleted, or excluded by an interceptor
}
```

Only the PK is needed; tenant columns are filled from the scope.

## Bulk Update and Delete

```go
n, err := s.Where(s.Status.Eq(fleet.StatusNew), s.Customer().Code.Eq("ACME")).
    Update(ctx, db, s.Status.To(fleet.StatusCanceled), s.CanceledAt.To(now), s.Priority.Increment(1))
n, err = st.Where(st.MoveID.Eq("mov_01"), st.ArrivedAt.IsNull()).Delete(ctx, db)
n, err = s.Where(keel.True()).Update(ctx, db, s.Priority.To(0)) // every row of the tenant: explicit
```

| Assignment | SQL |
|---|---|
| `col.To(v)` | `col = $n` |
| `col.Increment(v)` | `col = col + $n` |
| `col.ToNull()`, `col.ToDefault()` | `NULL`, `DEFAULT` |
| `col.ToExpr(e)` | `col = e` (typed arithmetic, `col.Excluded()` in upserts, `keel.Raw`) |

- A query without a non-nil `Where` is refused.
- Relation path filters become `FROM`/`USING`. Inner `Join`s and `With` CTEs work; `LeftJoin` and
  laterals are refused; pick paths can't filter bulk writes.
- `autoupdate` is stamped and `version` incremented unless assigned.
- Bulk writes run no model hooks and no `BeforeWrite` hooks; `AfterWrite`/`Committed` hooks run per
  row (returned through `RETURNING`, memory proportional to rows).

### Per-row values in one statement

```go
out := keel.Unnest("outcome", []string{"id", "phone", "score"}, ids, phones, scores)
n, err := c.With(out).Join(out, out.Col("id").EqCol(c.ID)).Where(c.VIP.IsFalse()).
    Update(ctx, db, c.Phone.ToExpr(out.Col("phone")), c.Score.ToExpr(out.Col("score")))
```

### UpdateReturning

```go
rows, err := s.Where(s.ID.In(ids...), s.Status.Eq(fleet.StatusNew)).
    Load(s.Customer(), s.Moves()).Order(s.ProNumber.Asc()).
    UpdateReturning(ctx, db, s.Status.To(fleet.StatusCanceled))
```

Returns the rows after the update in one statement; refuses `Limit`, `Offset`, `Lock`, `Group`,
`DistinctOn`.

## Upsert

```go
inserted, err := c.Upsert(ctx, db, rows,
    keel.OnConflict(c.OrganizationID, c.BusinessUnitID, c.Code).Update(c.Name)) // []bool per row

inserted, err = c.Upsert(ctx, db, rows, keel.OnConflict(c.OrganizationID, c.BusinessUnitID, c.Code).Set(
    c.Name.ToExpr(c.Name.Excluded()),
    c.Email.ToExpr(keel.Raw("coalesce(?, ?)", c.Email, c.Email.Excluded())),
))
```

- `OnConflict(targets...)` is required and names the unique index's columns, including tenant
  columns when the index has them (or expressions: `keel.Raw("md5(?)", p.Endpoint)`, binding no values).
- `Update(cols...)` takes the proposed values; `Set(assignments...)` explicit ones (`col` reads the
  stored row, `col.Excluded()` the proposed). Neither: every updatable column.
- `Where(preds...)` matches a partial unique index (constants only, e.g. `c.Email.IsNull()`).
- Conflicting rows keep the PK they were sent with (a fresh autoid), not the stored one: read back
  by the unique key if needed.
- Interceptors don't see upserts. Inserted rows run create hooks, conflicting rows update hooks.

## CreateOrSkip

```go
n, err := c.CreateOrSkip(ctx, db, rows, c.OrganizationID, c.BusinessUnitID, c.Code) // ON CONFLICT (…) DO NOTHING
n, err = c.CreateOrSkip(ctx, db, rows)                                             // any unique violation
```

## InsertFrom and UpsertFrom

```go
n, err := tg.InsertFrom(ctx, db,
    c.Pick(keel.Text("tag_").Cat(c.ID), c.Name).Where(c.Email.IsNotNull()),
    tg.ID, tg.Name)

n, err = d.UpsertFrom(ctx, db,
    l.Pick(l.Provider, l.Endpoint, keel.Count(), l.Units.Sum()).Group(l.Provider, l.Endpoint),
    keel.OnConflict(d.OrganizationID, d.BusinessUnitID, d.Provider, d.Endpoint),
    d.Provider, d.Endpoint, d.Calls, d.Units)
```

- One picked expression per listed column, in order. Under a tenant scope, tenant columns are
  filled and must not be listed; under a system scope, list and pick them.
- No `autoid`/`autocreate`/`autoupdate` filling and no hooks: pick those values yourself.
- `Conflict.UpdateWhere(ps...)` updates only when `ps` hold (capped counters in one statement).

## Many-to-many links

```go
err := keel.SetLinks(ctx, db, s.Tags(), sh.ID, tagIDs) // nil/empty unlinks all; needs a tenant scope
```

## What is read back

Only what Postgres may change: `readonly` columns and the new `version`. IDs, tenant columns and
timestamps keel filled are already in the struct. `Update` reads nothing back.

## Hooks and implicit transactions

A table with `AfterWrite` hooks for an operation makes those writes run in an implicit transaction
outside `Write`, so a hook error rolls the statement back. Group related writes in `keel.Write`
yourself; don't rely on hooks for atomicity across tables.
