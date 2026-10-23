# Using keel

## Setup

```go
db, err := keel.Open(ctx, keel.Config{
    DSN:           cfg.DSN(),
    TenantColumns: []string{"organization_id", "business_unit_id"},
    Tenant: func(ctx context.Context) ([]any, bool) {
        t, ok := dbscope.TenantFrom(ctx)
        if !ok {
            return nil, false
        }
        return []any{t.OrganizationID, t.BusinessUnitID}, true
    },
    System: func(ctx context.Context) (string, bool) {
        s := dbscope.From(ctx)
        return s.Reason(), s.Kind() == dbscope.KindSystem
    },
    OnBegin: func(ctx context.Context, tx keel.TxInfo) ([]keel.Statement, error) {
        token, err := signer.Token(ctx) // row-level security scope
        if err != nil {
            return nil, err
        }
        return []keel.Statement{{SQL: "SELECT set_config('app.scope', $1, true)", Args: []any{token}}}, nil
    },
    NewID:     func(prefix string) string { return pulid.MustNew(prefix).String() },
    LoadTypes: []string{"shipment_status", "_shipment_status"}, // enums whose arrays are bound
    AfterConnect: func(ctx context.Context, c *pgx.Conn) error {
        pgxdecimal.Register(c.TypeMap()) // shopspring decimal in binary
        return nil
    },
    Observers: []keel.Observer{tracing, slowLog},
})
```

- **Enums in arrays:** an enum compared with `In` is bound as an array of the enum type, so list the
  enum and its array type (`_name`) in `LoadTypes`. A single enum value binds from a Go string without
  this.
- **Decimals:** register a decimal codec (`github.com/jackc/pgx-shopspring-decimal`) so numeric values
  travel in binary instead of being parsed from text.
- **PgBouncer:** in transaction mode, prepared statements need PgBouncer 1.21 or later with
  `max_prepared_statements` set; otherwise use `PlanCache: keel.PlanCacheOff`.

## Models and generation

Embed `keel.Table` and tag every stored field (see [design.md](design.md#models) for every option).
Then generate:

```sh
go run github.com/emoss08/keel/cmd/keel gen -out 'pkg/dbq/{name}db' -kinds pkg/dbq/dbkind ./internal/modules/...
go run github.com/emoss08/keel/cmd/keel gen -check ...   # in CI
```

Only data-access packages should import the generated packages.

## Reading

```go
s := shipmentdb.Shipments

sh, err := s.Get(ctx, db, id)                                   // by primary key
sh, err := s.Where(s.ProNumber.Eq(pro)).One(ctx, db)            // exactly one
rows, err := s.Where(s.Status.In(a, b)).Order(s.ID.Asc()).Limit(50).List(ctx, db)
n, err := s.Where(s.Hazmat.IsTrue()).Count(ctx, db)
```

Optional filters: `keel.When(req.Search != "", s.ProNumber.Contains(req.Search))`; `Where` skips
nil predicates.

### Relations

```go
m := s.Moves()
rows, err := s.
    Where(s.Customer().Name.ContainsFold(q)).              // joins customers
    Where(m.Any(m.Status.Eq(shipment.MoveStatusAssigned))). // EXISTS over moves
    Load(
        s.Customer().Pick(s.Customer().Name),               // only these columns
        m.Where(m.Loaded.IsTrue()).Order(m.Sequence.Asc()),
        m.Stops().Location(),
        s.Comments().Order(cmt.CreatedAt.Desc()).Limit(3),  // at most 3 per shipment
    ).
    Limit(50).List(ctx, db)
```

### Pages

```go
page, err := s.Where(...).Order(s.CreatedAt.Desc()).
    Page(ctx, db, keel.After(req.Cursor, req.Size).Total(10_000))
// page.Items, page.Next ("" on the last page), page.Total, page.TotalCapped
```

The order may use columns of the table only; keel appends the primary key so positions are unique,
and uses a row comparison when it can, which a matching composite index serves.

### Projections

```go
type laneTotal struct {
    Lane    string
    Loads   int64
    Revenue decimal.Decimal
}
rows, err := keel.Into[laneTotal](ctx, db, s.
    Group(s.Lane).
    Pick(s.Lane, keel.Count(), s.Total.Sum()).
    Having(keel.Count().Gt(10)))
```

### Joins and subqueries

```go
mgr := workerdb.Workers.As("mgr")
w := workerdb.Workers
reports, err := w.Join(mgr, mgr.ID.EqCol(w.ManagerID)).Where(mgr.LastName.Eq("Hopper")).All(ctx, db)

c := customerdb.Customers
idle, err := c.Where(keel.NotExists(s.Where(s.CustomerID.EqCol(c.ID), s.CreatedAt.Gte(cutoff)))).All(ctx, db)

last := stopdb.Stops.As("last_stop")
rows, err := keel.Into[moveStop](ctx, db, mv.
    LeftJoinLateral(last.Pick(last.LocationID).Where(last.MoveID.EqCol(mv.ID)).Order(last.Sequence.Desc()).Limit(1)).
    Pick(mv.ID, last.LocationID))
```

## Writing

```go
err := s.Create(ctx, db, sh)
err = s.CreateMany(ctx, db, rows)
err = s.Save(ctx, db, sh)                   // errors.Is(err, keel.ErrConflict) on a stale version
err = s.Save(ctx, db, sh, s.Status)         // narrow
err = s.DeleteRow(ctx, db, sh)
n, err := s.Where(s.ID.In(ids...)).Update(ctx, db, s.Status.To(shipment.StatusCanceled))
n, err = s.Where(s.Status.Eq(shipment.StatusCanceled)).Delete(ctx, db)
ins, err := s.Upsert(ctx, db, rows, keel.OnConflict(s.OrganizationID, s.BusinessUnitID, s.ProNumber).Update(s.Status))
```

## Transactions

```go
err := keel.Write(ctx, db, func(ctx context.Context) error {
    sh, err := s.Where(s.ID.Eq(id)).Lock(keel.ForUpdate, keel.NoWait).One(ctx, db)
    if err != nil {
        return err
    }
    sh.Status = shipment.StatusAssigned
    if err := s.Save(ctx, db, sh); err != nil {
        return err
    }
    keel.AfterCommit(ctx, func(ctx context.Context) { events.Publish(ctx, sh.ID) })
    return nil
}, keel.LockTimeout(2*time.Second), keel.Retry(2))
```

## Hooks

```go
keel.OnUpdate(db, s, keel.AfterWrite, func(ctx context.Context, m *keel.Mutation[shipment.Shipment]) error {
    if m.Changed(s.Status) {
        return audit.Record(ctx, m.Row.ID, "status", m.Old.Status, m.Row.Status)
    }
    return nil
}, keel.Needs(s.Status))

db.Intercept(s, func(ctx context.Context, q *keel.Intercepted) error {
    if scope, ok := readscope.From(ctx); ok && q.Read {
        q.Where(s.CustomerID.In(scope.CustomerIDs...))
    }
    return nil
})
```

## Migrations

```sh
keel migrate -dsn "$DATABASE_URL" -dir migrations create add_shipment_index notx
keel migrate -dsn "$DATABASE_URL" -dir migrations up
keel migrate -dsn "$DATABASE_URL" -dir migrations status
```

Or from Go: `migrate.Open(ctx, dsn, embeddedFS)` then `Up`.

## Coming from Bun

| Bun | keel |
|---|---|
| `bun.BaseModel \`bun:"table:shipments,alias:sp"\`` | `keel.Table \`db:"shipments"\`` |
| `bun:"id,pk,type:VARCHAR(100),notnull"` | `db:"id,pk,autoid:shp_"` |
| `bun:"organization_id,pk"` + tenant helpers | `db:"organization_id,pk,tenant"` |
| `bun:"version,type:BIGINT"` + `Version++` / `Where(version = ?)` / `CheckRowsAffected` | `db:"version,version"` + `Save` |
| `BeforeAppendModel` setting ID and timestamps | `autoid`, `autocreate`, `autoupdate` |
| `bun:"rel:belongs-to,join:customer_id=id"` | `db:"belongs:customer_id"` |
| `bun:"rel:has-many,join:id=shipment_id"` | `db:"many:shipment_id"` |
| `bun:"rel:m2m:shipment_tags,join:Shipment=Tag"` | `db:"m2m:shipment_tags.shipment_id.tag_id"` |
| `bun:",scanonly"` (generated column) | `db:",readonly"` (and `noselect` when it should not be read) |
| `bun:",default:..."` | nothing; set the value in Go |
| `db.NewSelect().Model(&rows).Where("sp.status = ?", s)` | `s.Where(s.Status.Eq(st)).List(ctx, db)` |
| `WhereGroup(" AND ", func(q) {... ScopeTenant ...})` | nothing; the scope applies the tenant |
| `.Relation("Customer").Relation("Moves.Stops")` | `.Load(s.Customer(), s.Moves().Stops())` |
| `.Relation("Moves", func(q) { return q.Order("sm.sequence") })` | `.Load(m.Order(m.Sequence.Asc()))` |
| `db.NewUpdate().Model((*T)(nil)).Set("status = ?", v).Where(...)` | `s.Where(...).Update(ctx, db, s.Status.To(v))` |
| `.On("CONFLICT (...) DO UPDATE").Set("x = EXCLUDED.x")` | `s.Upsert(ctx, db, rows, keel.OnConflict(...).Update(s.X))` |
| `ScanAndCount` | `Page(...Total(n))` (one round trip) |
| `bun.In(ids)` | `col.In(ids...)` |
| `.For("UPDATE NOWAIT")` | `.Lock(keel.ForUpdate, keel.NoWait)` |
| `ColumnExpr("count(*) FILTER (WHERE ...)")` | `keel.Count().Filter(pred)` |
| `NewRaw` / string fragments | `keel.Raw("…?…", col, value)` |
