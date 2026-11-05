# Configuration, migrations, seeds and keelcli

## keel.Open

Open one `*keel.DB` per database at startup (safe for concurrent use), `defer db.Close()`, and call
`db.Ping(ctx)` to fail fast (the pool connects lazily).

```go
db, err := keel.Open(ctx, keel.Config{
    DSN:           os.Getenv("DATABASE_URL"),
    TenantColumns: []string{"organization_id", "business_unit_id"},
    Tenant: func(ctx context.Context) ([]any, bool) {
        sc, ok := ctx.Value(scopeKey{}).(scope)
        if !ok || sc.SystemReason != "" {
            return nil, false
        }
        return []any{sc.OrganizationID, sc.BusinessUnitID}, true
    },
    System: func(ctx context.Context) (string, bool) {
        sc, ok := ctx.Value(scopeKey{}).(scope)
        return sc.SystemReason, ok && sc.SystemReason != ""
    },
    OnBegin: func(ctx context.Context, tx keel.TxInfo) ([]keel.Statement, error) { // RLS, pipelined: no extra round trip
        sc, _ := ctx.Value(scopeKey{}).(scope)
        return []keel.Statement{{SQL: "SELECT set_config('app.organization_id', $1, true)",
            Args: []any{string(sc.OrganizationID)}}}, nil
    },
    NewID:        newID,               // func(prefix string) string; required for autoid
    AfterConnect: numeric.Register,    // binary shopspring/decimal codec (keel/numeric)
    Tables:       keeltables.All(),    // readable constraint errors
    Observers:    []keel.Observer{tracing{}},
})
```

| Field | Purpose |
|---|---|
| `DSN` / `Pool` | connection string, or a parsed `*pgxpool.Config` to size the pool |
| `TenantColumns`, `Tenant`, `System` | tenancy (see the transactions reference) |
| `OnBegin` | statements at the start of every transaction; makes every terminal run in an implicit tx. It should only read its context |
| `NewID`, `Now` | key generator, clock |
| `Observers`, `ObserveArgs` | statement observers; bound values (dev only) |
| `AfterConnect`, `LoadTypes`, `SkipEnums` | codecs, domains/composites; enums are registered automatically |
| `JSON` | `*keel.JSONCodec` (default sonic, encoding/json compatible) |
| `PlanCache`, `StatementCacheCapacity` | `PlanCacheAuto` (default), `PlanCacheCustom`, `PlanCacheOff` (old PgBouncer) |
| `Replicas`, `MaxReplicaLag` | read replicas |
| `DataVersions`, `Cache` | change counters and the result cache |
| `LoadStrategy`, `ReadIsolation`, `AwaitReadCommit`, `DeadlineTimeouts` | load and transaction behaviour |
| `TenantConcurrency`, `SlowWaits`, `MaxWait` | multi-tenant protection |
| `Tables` | generated tables for constraint error resolution |

- PgBouncer transaction mode: 1.21+ with `max_prepared_statements`, else `PlanCacheOff`; add
  `plan_cache_mode` to `ignore_startup_parameters`; run migrations and seeds against Postgres directly.
- `keel.Pool(db)` gives the `*pgxpool.Pool` for `LISTEN`/foreign `COPY` only; it bypasses scopes,
  interceptors and hooks.
- Types: decimals via `keel/numeric`; PostGIS via `keel/postgis` (`AfterConnect: postgis.Register`,
  `*postgis.Point` fields, `z.Center.DWithin(postgis.Pt(lon, lat), 2000)`); pgvector via
  `pgxvec.RegisterTypes` + `keel/vector` (`vector.CosineDistance`, `vector.EFSearch(100)` as a
  `Read` option). Chain several registrations in one `AfterConnect` func.
- `jsonb` with big integers in `map[string]any`: tag `jsonnumber`.

## Migrations (keel/migrate)

keel never generates migrations from structs. Write SQL.

- Files: `migrations/<14-digit UTC version>_<name>.sql` (create with `keel migrate create <name>`),
  repeatable `R_<name>.sql` (views, functions, grants; `CREATE OR REPLACE`). No down migrations: fix
  forward. Never edit an applied migration (`migrate.ErrChanged`).
- Directives at the top: `--keel:notx` (with `--keel:split` between statements) for
  `CREATE INDEX CONCURRENTLY` etc.; `--keel:lock_timeout 2s`; `--keel:statement_timeout 1m`;
  `--keel:no_retry`; `--keel:lint_ignore rule`.
- Each migration runs with a 5s lock timeout and is retried on contention.
- On tenant tables: lead every index with the tenant columns; build indexes on existing tables
  `CONCURRENTLY` in a notx file with `IF NOT EXISTS`; add `NOT NULL` columns with a `DEFAULT`.
  `keel migrate lint` flags these.

```sql
--keel:notx
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_shipments_status
    ON shipments (organization_id, business_unit_id, status);
--keel:split
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_moves_shipment
    ON moves (organization_id, business_unit_id, shipment_id, sequence);
```

```sh
keel migrate create add_shipment_holds          # -kind notx | repeatable
keel migrate plan                                # what up would do
keel up                                          # apply (the deploy step)
keel migrate status | history | check | lint
keel migrate stamp -out schema/version_gen.go    # server links Version; -check in CI
```

From Go: `m, err := migrate.Open(ctx, dsn, migrate.Sources(migrate.Source{Name: "core", FS: dir}), migrate.Options{After: []string{...}})`,
then `m.Up(ctx, migrate.UpOptions{})`. Go migrations: `migrate.Func{Version, Name, Up: func(ctx, tx pgx.Tx) error}`.
At server start: `migrate.RequireWait(ctx, keel.Pool(db), schema.Version, time.Second)` (apply
migrations from a deploy step, not from the server). Tests: `migrate.Template(ctx, migrate.TemplateOptions{...})`.

## Seeds (keel/seed)

```go
var Seeds = seed.NewRegistry()

var Plants = Seeds.Add(seed.Seed{
    Name:   "plants",
    Kind:   seed.Reference,                 // seed.Fixture for dev/test data (refused on production)
    Inputs: seed.Files(data, "data/plants.yaml"),
    Run: func(ctx context.Context, r *seed.Run) error {
        plants, err := seed.YAML[[]*fleet.Plant](r, "data/plants.yaml")
        if err != nil {
            return err
        }
        p := fleetdb.Plants
        _, err = p.Upsert(ctx, r.DB, plants, keel.OnConflict(p.Code)) // idempotent
        r.Count(int64(len(plants)))
        return err
    },
})
```

- Reference seeds re-run when their fingerprint (name, `Version`, input files, `Fingerprint()`)
  changes: write them as idempotent upserts / `CreateOrSkip`. Fixtures run once.
- `PerTenant: true` runs once per tenant under that tenant's scope (needs `Options.Tenants`).
- `After: []*seed.Seed{Plants}` orders seeds. Readers (`seed.YAML`, `JSON`, `CSV`, `CSVEach`) only
  read declared `Inputs`. `r.Rand()` is deterministic.
- Apply: `Seeds.Apply(ctx, db, seed.Options{Environment: seed.Production, System: withSystem, Tenants: tenants.All})`.

## keelcli

An application's database binary is one `keelcli.App`:

```go
func main() {
    keelcli.Main(keelcli.App{
        Name:       "app-db",
        Config:     dbconfig.FromEnv, // func() (keel.Config, error)
        Migrations: migrate.Sources(migrate.Source{Name: "core", FS: schema.Migrations}),
        Seeds:      reference.Seeds,
        Tenants:    tenants.All,
        System:     sysctx.With,
        Tables:     keeltables.All, // enables `verify`
    })
}
```

It provides `up`, `migrate …`, `seed …`, `template`, `reset`, `doctor`, `verify`, `plans` and an
interactive console. Destructive commands need `-yes` (and `-yes-production` on production).
