# Coming from Bun, GORM, sqlx or raw pgx

## Idiom map

| Elsewhere | keel |
|---|---|
| `bun.BaseModel \`bun:"table:shipments,alias:sp"\``, GORM `TableName()` | `keel.Table \`keel:"shipments"\``; aliases are derived |
| ID/timestamp setting in `BeforeAppendModel` / GORM hooks | `autoid:<prefix>`, `autocreate`, `autoupdate` tags |
| tenant helper scopes, `WhereGroup(... ScopeTenant ...)` | `tenant` tag + `Config.Tenant`; nothing per query |
| `version` column + `Where("version = ?")` + rows-affected check | `keel:"version,version"` + `Save` → `keel.ErrConflict` |
| validation/audit hooks | table hooks: `keel.OnCreate(db, s, keel.BeforeWrite, …)`, `keel.OnUpdate(…, keel.AfterWrite, …, keel.Needs(…))` |
| `AfterScanRow`, `AfterFind` | `AfterLoad` model hook |
| `type:jsonb`, GORM `serializer:json` | `jsonb` tag option |
| `bun:",array"`, `pq.StringArray` | nothing: `[]string`/`[]int64` are arrays |
| `bun:",nullzero"` | `nullzero` (on pointers drop it: nil is NULL) |
| `scanonly` generated column | `readonly` (+ `noselect`) |
| `scanonly` computed value, `ColumnExpr("… AS rank")` | `computed` field + `Compute(field, expr)` |
| `default:` tags, `AutoMigrate` | nothing: SQL migrations own the schema |
| `soft_delete`, GORM `DeletedAt` | an interceptor adding `deleted_at IS NULL` |
| `rel:belongs-to,join:customer_id=id` | `belongs:customer_id` |
| `rel:has-one` / `rel:has-many` | `one:<fk>` / `many:<fk>` |
| composite joins incl. tenant columns | `belongs:a\|b` without the tenant columns |
| `m2m:shipment_tags,join:Shipment=Tag` | `m2m:shipment_tags.shipment_id.tag_id` |
| delete + re-insert join rows | `keel.SetLinks(ctx, db, s.Tags(), id, tagIDs)` |
| hand-written lateral "first stop" | `pick:moves.stops,first=moves.sequence\|stops.sequence` |
| `NewSelect().Model(&sh).WherePK().Scan(ctx)`, `db.First(&sh, id)` | `s.Get(ctx, db, id)` |
| `.Where("sp.status = ?", st).Scan(ctx)` | `s.Where(s.Status.Eq(st)).Limit(n).List(ctx, db)` |
| `.WhereOr(...)` | `keel.Or(...)` |
| `ILIKE '%'||?||'%'` | `col.ContainsFold(q)` (escaped) |
| `bun.In(ids)`, `IN (?)` | `col.In(ids...)` (one array param) |
| `payload->'a'->>'b' = ?` | `s.Payload.Text("a", "b").Eq(v)` |
| `.Relation("Customer").Relation("Moves.Stops")`, GORM `Preload` | `.Load(s.Customer(), s.Moves().Stops())` |
| `.Relation("Moves", func(q) { q.Order("sequence") })` | `.Load(m.Order(m.Sequence.Asc()))` |
| `.Join("JOIN customers …")` to filter | `s.Customer().Code.Eq(...)` or `Join(c, c.ID.EqCol(s.CustomerID))` |
| `EXISTS (SELECT 1 FROM moves …)` | `m.Any(...)`, `keel.Exists(q)` |
| `.Limit(n).Offset(m)` + `ScanAndCount` | `Page(ctx, db, keel.After(cursor, n).Total(cap))` |
| `ColumnExpr("count(*)").Group("status")` into a struct | `keel.Into[R](ctx, db, s.Group(s.Status).Pick(s.Status, keel.Count()))` |
| `count(*) FILTER (WHERE …)` | `keel.Count().Filter(pred)` |
| `.For("UPDATE NOWAIT")` | `.Lock(keel.ForUpdate, keel.NoWait)` |
| `NewInsert().Model(sh)` / `Model(&rows)` | `s.Create` / `s.CreateMany` |
| `pgx.CopyFrom` | `s.Copy(ctx, db, rows)` (tenant, IDs, timestamps, hooks applied) |
| `.Returning("*")` | nothing: readonly columns and version are read back; `UpdateReturning` for bulk |
| `NewUpdate().Model(sh).WherePK()`, `.Column("status")` | `s.Save(ctx, db, sh)`, `s.Save(ctx, db, sh, s.Status)` |
| `ExcludeColumn("token")` on update / select | `SaveExcept(..., s.Token)` / `Omit(s.Token)` |
| `NewUpdate().Set("status = ?", v).Where(...)` | `s.Where(...).Update(ctx, db, s.Status.To(v))` |
| `NewDelete().Model(sh).WherePK()` / `.Where(...)` | `s.DeleteRow(ctx, db, sh)` / `s.Where(...).Delete(ctx, db)` |
| `On("CONFLICT (...) DO UPDATE")` | `s.Upsert(ctx, db, rows, keel.OnConflict(...).Update(cols...))` |
| `On("CONFLICT DO NOTHING")` | `s.CreateOrSkip(ctx, db, rows[, targets...])` |
| `UPDATE … FROM unnest(?::text[], …)` | `keel.Unnest(...)` + `With(..).Join(..).Update(..)` |
| `RunInTx(ctx, nil, func(ctx, tx bun.Tx) error)`, `db.Transaction(func(tx *gorm.DB) error)` | `keel.Write(ctx, db, func(ctx context.Context) error {...})` |
| `bun.IDB` / `*gorm.DB` / `sqlx.Ext` parameters | none: the transaction rides on `ctx` |
| raw fragments, `bun.Ident`, `?TableAlias` | `keel.Raw("…?…", col, value)`; statements: `keel.Exec`, `keel.QueryAll`, `keel.QueryValue` (unscoped) |
| `sql.ErrNoRows`, `gorm.ErrRecordNotFound` | `keel.ErrNotFound` |
| `pgdriver.Error.IntegrityViolation()` | `keel.IsUnique`, `IsForeignKey`, `IsCheck`, `*keel.ConstraintError` |
| `bun.QueryHook`, GORM callbacks for logging | `keel.Observer`, `keellog` |

## Running alongside another data layer

During a migration the old layer can keep writing in the same transaction, if it runs on pgx:

```go
conn, err := pool.Acquire(ctx) // the pgxpool both layers share
defer conn.Release()
tx, err := conn.Begin(ctx)
// … the other layer writes …
kctx, done := keel.JoinTx(ctx, db, conn.Conn())
err = keel.Write(kctx, db, func(ctx context.Context) error {
    return shipmentdb.Shipments.Create(ctx, db, sh)
})
if err != nil {
    _ = tx.Rollback(ctx)
    done(false)
    return err
}
err = tx.Commit(ctx)
done(err == nil) // runs Committed hooks / AfterCommit, moves data versions
```

keel sends no `BEGIN`/`COMMIT`/`OnBegin` in a joined transaction; scope checks, tenancy,
interceptors and hooks still apply. Writes the other layer makes are invisible to data versions:
`keel.MarkChanged(ctx, db, tables...)`.

## Migration order that works

1. Write models for existing tables (tags from the table above), run `keel gen`, and add a
   `keel.Verify` test against the migrated test database until it reports no drift.
2. Configure `keel.Config` tenancy to read the same context values the old layer's scope helpers use.
3. Move reads first (they're side-effect free), endpoint by endpoint; then writes, using `JoinTx`
   where a transaction spans both layers.
4. Delete the old layer's tenant helpers once nothing calls them.
