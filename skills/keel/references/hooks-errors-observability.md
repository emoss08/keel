# Hooks, interceptors, observers and errors

| Kind | Registration | Use for |
|---|---|---|
| model hooks | methods on the model | normalisation, derived fields |
| table hooks | `keel.OnCreate`, `OnUpdate`, `OnDelete`, `OnWrite` | validation needing the DB, audit, side effects |
| interceptors | `db.Intercept(handle, fn)`, `db.InterceptAll(fn)` | predicates on the base table (soft delete, ownership) |
| read filters | `keel.WithReadFilter(ctx, f)` | predicates on every table a read references |
| observers | `Config.Observers` | tracing, metrics, slow-query logs |

## Table hooks

Register per `DB` and table at startup.

| Phase | When | An error |
|---|---|---|
| `keel.BeforeWrite` | in the transaction, before an entity write; may change the row | aborts the write |
| `keel.AfterWrite` | in the transaction, after entity and bulk writes | rolls the transaction back |
| `keel.Committed` | after commit | is dropped (report it yourself) |

```go
s := fleetdb.Shipments

keel.OnCreate(db, s, keel.BeforeWrite, func(ctx context.Context, m *keel.Mutation[*fleet.Shipment]) error {
    if m.Row.ProNumber == "" {
        return errors.New("shipment needs a PRO number")
    }
    return nil
})

keel.OnUpdate(db, s, keel.AfterWrite, func(ctx context.Context, m *keel.Mutation[*fleet.Shipment]) error {
    if m.Old == nil || !m.Changed(s.Status) || m.Old.Status == m.Row.Status {
        return nil
    }
    return audit.Record(ctx, m.Row.ID, m.Old.Status, m.Row.Status)
}, keel.Needs(s.Status)) // previous values via RETURNING old.… (PostgreSQL 18)

keel.OnWrite(db, fleetdb.Customers, keel.Committed, func(ctx context.Context, m *keel.Mutation[*fleet.Customer]) error {
    search.Reindex(m.Row.ID, m.Op)
    return nil
})
```

`Mutation`: `Op` (`OpCreate`/`OpUpdate`/`OpDelete`), `Row`, `Old` (only `Needs` columns, updates and
deletes), `Changed(col)`.

| Write | Model hooks | BeforeWrite | AfterWrite / Committed |
|---|---|---|---|
| `Create`, `CreateMany`, `Copy` | BeforeCreate, BeforeSave | create | create |
| `Upsert` | BeforeCreate, BeforeSave | create | create (inserted), update with `Old` (conflicting) |
| `CreateOrSkip` | BeforeCreate, BeforeSave | create | create (inserted) |
| `Save` | BeforeUpdate, BeforeSave | update | update with `Old` |
| `DeleteRow` | BeforeDelete | delete | delete with `Old` |
| bulk `Update` / `Delete` | none | none | per row, with `Old` |
| `InsertFrom`, `UpsertFrom` | none | none | none |

Side effects outside the database (events, emails, queues) belong in `Committed` hooks or
`keel.AfterCommit`, never in `BeforeWrite`/`AfterWrite`.

## Interceptors

```go
db.Intercept(s, func(ctx context.Context, q *keel.Intercepted) error {
    customers, ok := ctx.Value(portalKey{}).([]ids.ID)
    if !ok {
        return nil
    }
    if q.Op == keel.OpDelete {
        return errors.New("portal users cannot delete shipments")
    }
    q.Where(s.CustomerID.In(customers...))
    return nil
})
```

Interceptors see every statement with a `WHERE` over their table: reads, loads, subqueries,
`Any`/`None`, `Save`, `DeleteRow`, bulk writes. They never see inserts or the update half of an
upsert, nor relations joined into a statement. keel has no built-in soft delete: implement it as an
interceptor adding `deleted_at IS NULL`, plus a partial unique index matched by `OnConflict(...).Where(...)`.

## Read filters

```go
ctx = keel.WithReadFilter(ctx, func(ctx context.Context, t *schema.Table) ([]keel.Pred, error) {
    if t.Name != "shipments" {
        return nil, nil
    }
    return []keel.Pred{shipmentdb.Shipments.OwnerID.Eq(userID(ctx))}, nil
})
```

Applied to every table reference of a read (base, joins, picks, EXISTS, CTEs); a joined row it
excludes reads as absent. Return quickly (cache built predicates).

## Observers and the query log

```go
type slowQueryLog struct{ threshold time.Duration }

func (slowQueryLog) BeforeQuery(ctx context.Context, _ *keel.QueryEvent) context.Context { return ctx }
func (l slowQueryLog) AfterQuery(_ context.Context, e *keel.QueryEvent) {
    if e.Duration >= l.threshold {
        log.Printf("slow %s on %s: %v: %s", e.Op, e.Table, e.Duration, e.SQL)
    }
}
```

`QueryEvent`: `Op`, `Table`, `SQL` (never values), `Args`, `Values` (only with
`Config.ObserveArgs`), `Replica`, `Tx`, `Start`, `Duration`, `Rows`, `Err`. Start spans in
`BeforeQuery` (return the new ctx) and end them in `AfterQuery`.

Development log: `Observers: []keel.Observer{keellog.New(keellog.Options{Mode: keellog.All})}` with
`ObserveArgs: true` (never in production: values are data).

## Errors

| Error | From |
|---|---|
| `keel.ErrNotFound` | `Get`, `One`, `First`, `IntoOne` without a row; `DeleteRow` deleting nothing; `Save` without version matching nothing |
| `keel.ErrNotSingular` | `One`, `IntoOne` with several rows |
| `keel.ErrConflict` (`*keel.ConflictError{Table, Version}`) | `Save` that lost an optimistic lock |
| `keel.ErrNoScope`, `ErrAllTenantsRequired`, `ErrSystemScopeRequired`, `ErrTenantMismatch` | scope violations |
| `keel.ErrUnbounded` | `List` without `Limit` |
| `keel.ErrReadOnly` | a write inside `Read` |
| `keel.ErrScopeMismatch` | joining a transaction under another scope |
| `keel.ErrBudgetExceeded` (`*keel.BudgetError`) | a budget limit crossed |
| `keel.ErrWaitLimit` | a wait past `Config.MaxWait` |

Errors are wrapped with the table name: always `errors.Is`/`errors.As`, never string comparison.

Constraint violations are `*keel.ConstraintError` (`Kind`, `Table`, `Constraint`, `Columns`, and
with `Config.Tables: keeltables.All()` also `Model`, `Fields`, `Labels`, `Message`):

```go
err := fleetdb.Shipments.Create(ctx, db, sh)
var ce *keel.ConstraintError
switch {
case keel.IsUnique(err, "uq_shipments_pro"):
    return ErrDuplicatePRO
case keel.IsForeignKey(err, "fk_shipments_customer"):
    return ErrUnknownCustomer
case errors.As(err, &ce):
    return fieldError(ce.Fields, ce.Message)
case err != nil:
    return fmt.Errorf("create shipment (SQLSTATE %s): %w", keel.Code(err), err)
}
```

Helpers: `keel.IsUnique`, `IsForeignKey`, `IsCheck`, `IsExclusion` (each optionally with
constraint names), `IsNotNull`, `IsRetryable` (`40001`/`40P01`), `Code(err)`, `ConstraintName(err)`.
Map `ErrNotFound` to 404, `ErrConflict` to 409, unique violations to field errors at the API edge.
