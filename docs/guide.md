# Using keel

This guide shows how to use keel, organised by task. [design.md](design.md) is the contract the
code follows; [benchmarks.md](benchmarks.md) records how it performs.

The examples use the models keel's own tests run against, in
[`internal/testmodels/fleet`](../internal/testmodels/fleet/fleet.go): `Customer`, `Shipment`,
`Move`, `Stop`, `Location`, `Tag`, `Worker` and `Plant`. `keel gen` writes their data-access package,
`fleetdb`. The examples have compiled counterparts in [`example_test.go`](../example_test.go), so
the snippets here stay in step with the API.

In the snippets, `db` is the application's `*keel.DB` and `ctx` is a request context that carries a
tenant (see [Tenancy](#tenancy)).

## Contents

- [Install](#install)
- [Configuration](#configuration)
- [Models](#models)
  - [Tag reference](#tag-reference)
  - [Nullzero columns](#nullzero-columns)
  - [Pick relations](#pick-relations)
  - [Derived metadata](#derived-metadata)
- [Generating code](#generating-code)
- [Reading](#reading)
- [Loading relations](#loading-relations)
- [Pagination](#pagination)
- [Batching reads](#batching-reads)
- [Dynamic queries](#dynamic-queries)
- [Writing](#writing)
- [Transactions](#transactions)
- [Tenancy](#tenancy)
- [Hooks, interceptors and observers](#hooks-interceptors-and-observers)
- [Errors](#errors)
- [Migrations](#migrations)
- [Seeds](#seeds)
- [Performance notes](#performance-notes)
- [Testing](#testing)
- [Coming from Bun](#coming-from-bun)

## Install

```sh
go get github.com/emoss08/keel
```

keel needs Go 1.26 and PostgreSQL; its tests run against PostgreSQL 18, which hooks that declare
`Needs` require (they read previous values with `RETURNING old.…`). keel talks to Postgres through
pgx v5 and never uses `database/sql`.

| Package | Role |
|---|---|
| `github.com/emoss08/keel` | the API: `DB`, transactions, queries, expressions, writes, hooks, errors |
| `github.com/emoss08/keel/cmd/keel` | the command: `keel gen`, the migration commands and the console |
| `github.com/emoss08/keel/migrate` | migrations: apply, plan, squash, lint, templates, `Require` |
| `github.com/emoss08/keel/seed` | reference data and fixtures, re-applied when their inputs change |
| `github.com/emoss08/keel/keelcli` | an application's database binary: migrations and seeds as commands, live views and a console |
| `github.com/emoss08/keel/schema` | table metadata the generator writes; applications do not use it directly |

## Configuration

`keel.Open` builds the connection pool and returns a `*keel.DB`, which is safe for concurrent use.
Open one per database at startup and close it at shutdown. The pool connects on first use, so call
`db.Ping(ctx)` at startup to fail fast on a bad DSN or an unreachable server.

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
    OnBegin: func(ctx context.Context, tx keel.TxInfo) ([]keel.Statement, error) {
        sc, _ := ctx.Value(scopeKey{}).(scope)
        return []keel.Statement{{
            SQL:  "SELECT set_config('app.organization_id', $1, true)",
            Args: []any{string(sc.OrganizationID)},
        }}, nil
    },
    NewID:     newID, // Trenova: func(p string) string { return pulid.MustNew(p).String() }
    Observers: []keel.Observer{slowQueryLog{threshold: 200 * time.Millisecond}},
})
if err != nil {
    return err
}
defer db.Close()
```

`scope` and `scopeKey` are the application's own: whatever its authentication middleware puts on
the request context.

### Every field

| Field | Type | What it does |
|---|---|---|
| `DSN` | `string` | connection string; required unless `Pool` is set |
| `Pool` | `*pgxpool.Config` | a parsed pool configuration, used instead of `DSN`; keel sets its query mode and statement cache and wraps its `AfterConnect` |
| `TenantColumns` | `[]string` | names of the tenant columns, in the order `Tenant` returns their values |
| `Tenant` | `func(ctx) ([]any, bool)` | the tenant of the context; a result with a different number of values than `TenantColumns` counts as no tenant |
| `System` | `func(ctx) (reason string, ok bool)` | reports a system scope; checked before `Tenant` |
| `OnBegin` | `func(ctx, keel.TxInfo) ([]keel.Statement, error)` | statements sent at the start of every transaction, explicit or implicit; when set, every terminal outside a transaction runs in an implicit one |
| `NewID` | `func(prefix string) string` | new primary keys for `autoid` columns; a write that needs one fails without it |
| `Now` | `func() time.Time` | the time stamped into `autocreate` and `autoupdate` columns; defaults to `time.Now` |
| `Observers` | `[]keel.Observer` | see every statement: tracing, metrics, slow-query logs, [the query log](#query-log) |
| `ObserveArgs` | `bool` | hands observers each statement's bound values (`QueryEvent.Values`); for development logs only |
| `AfterConnect` | `func(ctx, *pgx.Conn) error` | runs on every new connection after keel registers its types; register codecs here |
| `LoadTypes` | `[]string` | user-defined types (domains, composites) and their array types to register on every connection |
| `SkipEnums` | `bool` | stops keel registering every enum type of the database on each new connection |
| `JSON` | `*keel.JSONCodec` | the JSON implementation for `json` and `jsonb` values (`Marshal`, `Unmarshal`, and `UnmarshalNumber` for `jsonnumber` columns); defaults to `encoding/json` |
| `PlanCache` | `keel.PlanCache` | how statements are prepared: `PlanCacheAuto` (default), `PlanCacheCustom`, `PlanCacheOff` |
| `StatementCacheCapacity` | `int` | prepared statements (or, with `PlanCacheOff`, statement descriptions) kept per connection; defaults to 512 |
| `Replicas` | `[]keel.ReplicaConfig` | streaming replicas (`DSN` or `Pool`, and a `Weight`) that reads marked with `keel.OnReplica` run on; see [Read replicas](#read-replicas) |
| `MaxReplicaLag` | `time.Duration` | how far behind the primary a replica may be and still serve reads; defaults to 2s |
| `DataVersions` | `keel.DataVersionsConfig` | `Enabled`, `Interval` (default 1s) and `OnError` of the per-tenant, per-table change counters; off by default; see [Data versions](#data-versions) |
| `Cache` | `*keel.CacheConfig` | the result cache: `Local`, `Shared`, `Key`, `MaxEntryBytes` and `OnError`; needs `DataVersions`; nil (the default) turns it off; see [Result cache](#result-cache) |
| `LoadStrategy` | `keel.LoadStrategy` | how relation trees load: `keel.LoadPipelined` (default, every level in the read's round trip) or `keel.LoadPerLevel`; see [One round trip](#one-round-trip) |
| `ReadIsolation` | `keel.Isolation` | the isolation level of explicit `Read` transactions that pass none: zero leaves the server's default (read committed), `keel.RepeatableRead` gives every statement of a `Read` one snapshot; see [Isolation](#isolation) |
| `AwaitReadCommit` | `bool` | makes read-only transactions wait for the reply to their `COMMIT`, as writes do; see [Read-only transactions end without waiting](#read-only-transactions-end-without-waiting) |
| `DeadlineTimeouts` | `bool` | every transaction keel begins sets `statement_timeout` to the time left before its context's deadline; see [Plans and timeouts](#plans-and-timeouts) |
| `TenantConcurrency` | `int` | at most this many terminals and transactions of one tenant run at once; zero (the default) is no bound; see [Tenant concurrency](#tenant-concurrency) |

`keel.TxInfo` tells `OnBegin` whether the transaction is `ReadOnly` and whether it is `Implicit`
(opened by keel for a single terminal). A `keel.Statement` is `SQL` plus `Args`.

### A multi-tenant application

For an application like Trenova, where every tenant table carries `organization_id` and
`business_unit_id`:

- Set `TenantColumns` to those names and `Tenant` to read them from the request context. Every
  statement on a table with `tenant` columns is then scoped (see [Tenancy](#tenancy)).
- Set `System` for work that is not done for one tenant (sweeps, reports, migrations), and give
  each system scope a reason so logs say why a statement crossed tenants.
- Set `OnBegin` when Postgres row-level security backs the application's own checks. The
  `set_config(…, true)` call is transaction-local, and keel pipelines `BEGIN`, the `OnBegin`
  statements, the statement and `COMMIT` in one round trip, so the extra safety costs no extra
  round trip (see [Transactions](#transactions)).
- Set `NewID` to the application's ID generator.
- Set `JSON` to a fast codec and register a binary decimal codec in `AfterConnect`.

### Connection pool

To size the pool, parse the DSN yourself and pass the pool configuration:

```go
pc, err := pgxpool.ParseConfig(os.Getenv("DATABASE_URL"))
if err != nil {
    return err
}
pc.MaxConns = 32
pc.MaxConnIdleTime = 5 * time.Minute

cfg := appConfig("")
cfg.Pool = pc
cfg.PlanCache = keel.PlanCacheCustom
cfg.StatementCacheCapacity = 1024
db, err := keel.Open(ctx, cfg)
```

`db.Pool()` returns the underlying `*pgxpool.Pool` for work keel does not cover (`LISTEN`, `COPY`
of foreign data). Statements sent through it bypass scope checks, interceptors and hooks.

### Statement preparation

Values are always bind parameters, so a statement's text depends only on its shape. keel prepares
each shape once per connection and Postgres skips parsing it again.

| `PlanCache` | Behaviour |
|---|---|
| `keel.PlanCacheAuto` (default) | prepared once per connection; `plan_cache_mode = auto` lets Postgres reuse a generic plan when it is no worse than the custom plans of the first executions, so it usually skips planning too |
| `keel.PlanCacheCustom` | prepared, but every execution is planned for its values (`force_custom_plan`); use it when tenants of very different sizes make parameter values change plans a lot |
| `keel.PlanCacheOff` | nothing prepared; every execution is parsed and planned. For poolers that cannot track prepared statements |

keel sets `plan_cache_mode` as a connection parameter. `Query.CustomPlan()` plans one query for its
values without changing the mode for the rest (see [Plans and timeouts](#plans-and-timeouts)).
`StatementCacheCapacity` bounds the statements kept per connection; the least recently used is
deallocated beyond it.

### PgBouncer

- In transaction mode, prepared statements need PgBouncer 1.21 or later with
  `max_prepared_statements` set. With an older PgBouncer, or without that setting, use
  `PlanCache: keel.PlanCacheOff`.
- With `PlanCacheAuto` or `PlanCacheCustom`, keel sends `plan_cache_mode` as a startup parameter.
  PgBouncer refuses startup parameters it does not track unless they are listed in
  `ignore_startup_parameters`; list `plan_cache_mode` there, and set the mode on the server (for
  example `ALTER ROLE app SET plan_cache_mode = force_custom_plan`) if you need a non-default one.
- keel's transaction-scoped features work in transaction mode: `OnBegin`'s `set_config(…, true)`,
  `CustomPlan`, `LockKey` and `LockTimeout` all live and die with the transaction.
- Run migrations against Postgres directly: the migration runner holds a session-level advisory
  lock.

### JSON

`jsonb` fields are encoded and decoded with `encoding/json` unless `Config.JSON` names another
implementation with the same signatures:

```go
JSON: &keel.JSONCodec{Marshal: sonic.Marshal, Unmarshal: sonic.Unmarshal},
```

The codec is registered for `json` and `jsonb` on every connection and also encodes `jsonb` values
in bulk inserts.

A `jsonb` column read into `map[string]any`, `[]any` or `any` decodes its numbers as `float64`,
which rounds an integer past 2^53 (an ID, an amount in minor units, a tool call's argument).
Tag the column `jsonnumber` and keel decodes it with `JSONCodec.UnmarshalNumber` instead: numbers
in interface values become `json.Number`, holding the literal, and every codec writes a
`json.Number` back as the same literal, so a value round-trips digit for digit. Typed fields
(`int64`, a struct field) decode as before.

```go
type AuditEntry struct {
    keel.Table `keel:"audit_entries"`

    Arguments map[string]any `keel:"arguments,jsonb,jsonnumber"`
}

// The default decodes with encoding/json's Decoder and UseNumber (keel.UnmarshalJSONNumber).
// With sonic, give its number-preserving configuration:
JSON: &keel.JSONCodec{Marshal: sonic.Marshal, Unmarshal: sonic.Unmarshal,
    UnmarshalNumber: sonic.Config{UseNumber: true}.Froze().Unmarshal},
```

`jsonnumber` applies wherever keel decodes the model's rows (queries, relation loads, values
read back after writes) on connections keel opened; on a connection joined with `JoinTx`, which
keel did not set up, it uses `keel.UnmarshalJSONNumber`. `keel.Into` projections and dynamic
results are not models: a projection decodes with `Unmarshal`, and `DynResult` keeps the JSON
text. `DB.JSON()` returns the codec with `UnmarshalNumber` filled in, for packages that decode
JSON themselves.

### Enums, domains and decimals

- **Enums** are registered on every new connection without configuration: keel reads every enum
  type of the database and its array type from the catalog in one query and registers their codecs
  itself, so connection setup costs one round trip whether the schema has ten enums or a thousand
  (about 5 ms for 700 enums, where pgx's `LoadTypes` takes over a second). An enum compared with
  `In` binds as one binary array parameter, and enum values decode into one shared string per
  label, without an allocation per row. Each enum is registered under its name and its
  schema-qualified name (`status`, `ops.status`, and `_status`, `ops._status` for the array).
  `SkipEnums: true` turns this off; then list each enum compared with `In`, and its array type
  (`_name`), in `LoadTypes`.
- **Enum values are constants.** A value compared with a column whose Go type is an enum (a named
  type with declared constants, `Column.Enum`) is written into the statement when it is one of
  those constants: `s.Status.Eq(fleet.StatusNew)` is `s."status" = 'New'`, and `In` of constants
  is `IN ('New', 'Assigned')`. The constants are a closed set, so the number of distinct statements
  stays bounded, and a generic plan can prove a partial index predicate such as
  `WHERE status = 'Active'`, which it never can from a parameter. Any other value is bound.
- **Domains and composite types** must be listed in `LoadTypes`, each with its array type, by
  their unquoted names (`ops.label`). They load after the enums, through pgx's `LoadTypes`.
- **Decimals:** register a binary codec in `AfterConnect`, for example
  `github.com/jackc/pgx-shopspring-decimal`, so numeric values travel in binary instead of being
  parsed from text:

```go
AfterConnect: func(ctx context.Context, conn *pgx.Conn) error {
    pgxdecimal.Register(conn.TypeMap())
    return nil
},
```

- **Extension types (PostGIS, pgvector):** a column of a Go type that implements `sql.Scanner`
  and `driver.Valuer` (on the value or on the pointer) is a custom column (`KindOther`). Register
  the extension's types and their array types in `AfterConnect` so values travel in binary through
  a codec; pgvector-go's `pgxvec.RegisterTypes` does it for vectors, and `keel/postgis` does it for
  PostGIS (below).
- **pgvector expressions:** `keel/vector` has the distance operators (`CosineDistance` `<=>`,
  `L2Distance` `<->`, `NegativeInnerProduct` `<#>`, `L1Distance` `<+>`), the casts an expression
  index over a column of mixed dimensions declares (`HalfVec(e, 1536)` is `(e)::halfvec(1536)`,
  with the dimensions written into the statement so it matches the index), and the HNSW settings
  `EFSearch(n)` and `IterativeScan(mode)`, `TxOption`s for `Read`. Order by the distance and limit
  the rows, and the HNSW index serves the query:

```go
distance := vector.CosineDistance(vector.HalfVec(e.Embedding, 1536), vector.HalfVec(query, 1536))
err := keel.Read(ctx, db, func(ctx context.Context) error {
    rows, err = e.Pick(e.SourceID, distance).Order(distance.Asc()).Limit(40).All(ctx, db)
    return err
}, vector.EFSearch(100))
```

  With the codecs registered, every write path binds the type's `Value` (WKB) and every read
  passes the bytes PostGIS sends (EWKB) to `Scan`; bulk inserts send the column as one array of
  values that the codec encodes, since keel sees the type and its array registered when it reads
  the table's column types. Without a codec, reads still work (the driver passes the text form to
  `Scan`), and writes need a `Value` that returns the type's text input, as hex EWKB is for
  PostGIS. `Copy` always needs the codec.

#### PostGIS: keel/postgis

`github.com/emoss08/keel/postgis` maps geometry and geography columns and builds spatial SQL:

- `postgis.Point` (a `geometry(Point)` or `geography(Point)`) and `postgis.Geometry` (any shape)
  scan EWKB and its hex form and write EWKB with SRID 4326. Fields are `*postgis.Point` and
  `*postgis.Geometry`; nil is NULL.
- `postgis.Register` registers the types and their arrays: `Config{AfterConnect: postgis.Register}`.
- keel gen gives such fields `postgis.PointCol` and `postgis.GeometryCol`, with spatial methods:
  `DWithin(other, meters)`, `Distance(other)`, `Within(g)`, `Contains(g)`, `Intersects(g)`,
  `Area()`, `Centroid()`, `AsGeoJSON()`, and `SetExpr(e)` to assign a built value. Any library can
  do the same: when a field's type comes from a package that also exports `<Type>Col` embedding
  keel's base column kind, keel gen uses that kind.
- Builders, each a `keel.DynExpr` whose operands are expressions or values bound as parameters:
  `Pt(lon, lat)`, `FromGeoJSON(json)`, `Buffer(g, meters)`, `ToGeography(g)`, `ToGeometry(g)`,
  `DWithin(a, b, meters)`, `Distance(a, b)`, `DistanceSphere(a, b)`, `Contains`, `Within`,
  `Intersects`, `Area`, `Centroid`, `AsGeoJSON`. Distances, radii and areas are in meters, measured
  through geography, so a GiST index on a geography column serves `DWithin`.

```go
z := zonedb.Zones
near, err := z.Where(z.Center.DWithin(postgis.Pt(lon, lat), 2000)).All(ctx, db)
_, err = z.Where(z.ID.Eq(id)).Update(ctx, db, z.Area.SetExpr(postgis.Buffer(postgis.Pt(lon, lat), 250)))
_, err = z.Where(z.ID.Eq(id)).Update(ctx, db, z.Area.SetExpr(postgis.FromGeoJSON(polygonJSON)))
```

### Read replicas

`Config.Replicas` lists streaming replicas of the primary. keel opens a pool per replica with the
same setup as the primary's (query mode, statement cache, `plan_cache_mode`, enum and array
registration, the JSON codec, `LoadTypes` and `AfterConnect`), so every query that works on the
primary works on a replica.

```go
cfg := appConfig(os.Getenv("DATABASE_URL"))
cfg.Replicas = []keel.ReplicaConfig{
    {DSN: os.Getenv("REPLICA_A_URL")},
    {DSN: os.Getenv("REPLICA_B_URL"), Weight: 2},
}
cfg.MaxReplicaLag = 2 * time.Second
db, err := keel.Open(ctx, cfg)
```

Reads opt in. `keel.OnReplica(ctx)` marks a context, and every read terminal called with it outside
a transaction (`One`, `First`, `List`, `All`, `Page`, `Count`, `CountUpTo`, `Exists`, `Each`,
`keel.Into`, `keel.IntoOne`, `keel.Batch`) runs on a replica, its relation loads included. Writes,
`keel.Exec` and explicit transactions use the primary.

```go
ctx := keel.OnReplica(r.Context())
page, err := s.Where(s.Status.Eq(fleet.StatusNew)).
    Order(s.CreatedAt.Desc()).
    Page(ctx, db, keel.After("", 50))

sh := &fleet.Shipment{CustomerID: "cus_01", ProNumber: "PRO-3001", Status: fleet.StatusNew}
err = s.Create(ctx, db, sh)       // the primary
got, err := s.Get(ctx, db, sh.ID) // sees the shipment it just created
```

**Routing.** A replica serves a read when its last check found it healthy and in recovery, no more
than `MaxReplicaLag` behind and checked within the last 3 seconds. Among those keel takes the one
with the fewest reads in flight for its `Weight`, then the fewest reads so far for its weight, so
weights split sequential traffic and in-flight counts balance concurrent traffic. When no replica
qualifies the read runs on the primary and `db.ReplicaFallbacks()` counts it.

**Lag tracking.** One goroutine per `DB` checks every replica once a second, stopped by `Close`.
Each check reads the primary's WAL insert position, then each replica's `pg_is_in_recovery()` and
`pg_last_wal_replay_lsn()`, concurrently. A replica's lag is the age, on keel's clock, of the newest
primary position it has replayed: it needs no clock agreement between servers and stays near zero
on an idle primary, where `now() - pg_last_xact_replay_timestamp()` would grow without bound. When
the primary cannot be reached, keel falls back to that timestamp (zero when the replica has replayed
all it received). A server that is not in recovery, such as a promoted replica, is never used.

**Read-your-writes.** A write made with a marked context records the primary's WAL position after
its commit, read by a `SELECT pg_current_wal_insert_lsn()` sent after `COMMIT` in the commit's own
round trip, and later replica reads on that context only run where that position has been
replayed. Reading the position before `COMMIT` would not do: the commit record comes after it, so a
replica could reach the position without the commit. Outside `Write`, a write on a marked context
runs in an implicit transaction (`BEGIN`, the statement, `COMMIT` and the position in one round
trip), as it does with `OnBegin`.

- A replica the lag tracker has seen replay the position serves the read as usual.
- A replica it has not seen yet (the tracker polls once a second) serves it when the write is
  older than the replica's lag: the read's round trip starts with a check of the replica's replay
  position, which aborts the rest of the pipeline when the replica is behind, and the read then
  runs on the primary. A read right after its write goes to the primary.
- When the position after a commit cannot be read, the context's reads stay on the primary.

To carry read-your-writes across requests, send `keel.CommitPosition(ctx, db)` to the client (a
cookie, a header) and start the next request with `keel.OnReplicaAfter`:

```go
cookie := keel.CommitPosition(ctx, db).String()

pos, err := keel.ParsePosition(cookie)
next := keel.OnReplicaAfter(r.Context(), db, pos)
n, err := s.Where(s.Status.Eq(fleet.StatusNew)).Count(next, db)
```

**Long read-only work.** `keel.ReplicaTx()` runs a `Read` on a replica: one replica connection and
snapshot for every terminal in it, for reports and exports. It takes a replica the tracker has seen
past the context's position, falls back to the primary, and is refused on a `Write` and with
`Serializable`, which a hot standby does not run.

```go
err := keel.Read(ctx, db, func(ctx context.Context) error {
    return s.Where(s.Status.Eq(fleet.StatusDelivered)).Each(ctx, db, 1000, exportBatch)
}, keel.ReplicaTx(), keel.RepeatableRead)
```

**Watching replicas.** `db.Replicas()` returns, per replica, its `Name` (`host:port/database`),
`Weight`, `Healthy`, `Lag`, replayed `Position`, `CheckedAt`, reads `InFlight`, `Reads` routed so
far and the last check's `Err`. Observers see where each statement ran in `QueryEvent.Replica`.

A long query on a replica can be cancelled by replay conflicts; set `hot_standby_feedback = on` on
the replicas, or a `max_standby_streaming_delay` long enough for the reports they serve.

### Data versions

With `Config.DataVersions.Enabled`, keel keeps a version counter per tenant and table in
`keel_data_versions`, bumped after a write to the table commits. A cache keyed on the versions of
the tables it read is fresh exactly until one of them changes for the tenant.

```go
cfg.DataVersions = keel.DataVersionsConfig{
    Enabled:  true,
    Interval: time.Second,
    OnError:  func(err error) { log.Printf("data versions: %v", err) },
}
```

```go
tenant := db.TenantKey(ctx)
versions, err := db.DataVersions(ctx, tenant, "shipments", "customers")
cacheKey := fmt.Sprintf("open-shipments:%s:%d:%d", tenant, versions["shipments"], versions["customers"])
```

How versions move:

- Every write terminal records the (tenant, table) pairs it changed: `Create`, `CreateMany`,
  `Copy`, `Save`, `DeleteRow`, `Upsert`, `CreateOrSkip`, `InsertFrom`, `UpsertFrom`, bulk `Update` and `Delete`.
  Inside a transaction the pairs wait for the commit, and a rollback drops them; outside one they
  are handed over after the statement.
- A goroutine per `DB` writes the pairs every `Interval`, at most once per pair, in one
  `INSERT … SELECT FROM unnest(…) ON CONFLICT DO UPDATE SET version = version + 1` per 5,000
  pairs, rows locked in key order. It runs outside every write's transaction on its own
  connection, so writes never wait on a version row, and a pair written a thousand times in a
  second costs one row update. `db.FlushDataVersions(ctx)` writes the pending pairs now; `Close`
  writes them too. A failed write is retried at the next interval and reported to `OnError`.
- The tenant key is the tenant's values joined with `/` (`keel.TenantKeyOf`, `db.TenantKey(ctx)`),
  taken from the scope. Under a system scope keel records each written row's own tenant, read from
  the rows (a system bulk write returns its distinct tenants through `RETURNING` for this). A table
  without tenant columns, and a change keel cannot attribute to one tenant, is recorded for no
  tenant (`""`). A table that has only the first tenant columns is keyed by those.
- `DataVersions` returns, per table, the sum of the versions for the key, its shorter prefixes and
  `""`, so a change to a shared table, or one recorded for every tenant, moves every tenant's
  version. Each pair is bumped at most `Interval` after its commit: cache entries may outlive a
  change by that long.
- `keel.Exec`, statements sent through `db.Pool()` and triggers are invisible: call
  `keel.MarkChanged(ctx, db, tables...)` next to them. Under a tenant scope it records the tenant,
  else `""`.
- A version can move without a change (a write that matched no row, a statement that failed after
  it may have committed, a rolled-back savepoint); it never stays put across a committed change.
- On a context marked with `OnReplica`, `DataVersions` also raises the context's position to the
  primary's current one, so data read from a replica afterwards is at least as new as the versions
  and a cache never stores old rows under a new version.

keel creates `keel_data_versions` on first use, under an advisory lock, when it does not exist. To
create it in a migration instead (a role without `CREATE`), use the same definition:

```sql
CREATE TABLE keel_data_versions (
    tenant_key text COLLATE "C" NOT NULL,
    table_name text COLLATE "C" NOT NULL,
    version bigint NOT NULL,
    changed_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_key, table_name)
) WITH (fillfactor = 70);
```

The fill factor keeps a bump, which changes no indexed column, a heap-only update.

The versioner also notifies the versions it writes on the channel `keel_data_versions`, in the
same statement, so the [result cache](#result-cache) of every process learns about a change
without asking.

### Result cache

`Config.Cache` keeps query results in memory (and, optionally, in a store every process shares)
and serves a query that asks for it without a round trip while no table it read has changed for
the tenant. It is built on data versions, which must be on.

```go
cfg.DataVersions = keel.DataVersionsConfig{Enabled: true}
cfg.Cache = &keel.CacheConfig{
    Local:  keel.LRU(256 << 20),                     // default keel.LRU(64 MiB)
    Shared: rediscache.New(redisClient{rdb}, ""),     // optional: keel/cache/rediscache
    Key:    func(ctx context.Context) string { return readScopeOf(ctx) },
    OnError: func(err error) { log.Printf("result cache: %v", err) },
}
```

```go
s := fleetdb.Shipments
open, err := s.Where(s.Status.Eq(fleet.StatusNew)).
    Load(s.Customer(), s.Moves()).
    Order(s.CreatedAt.Desc()).Limit(50).
    Cache(keel.TTL(5 * time.Minute)).
    List(ctx, db)
```

- `Cache` works on `One`, `First`, `List`, `All`, `Count`, `CountUpTo`, `CountEstimate`, `Exists`
  and `Page`, and on their `Batch` items: a batch is served from the cache when every item asks,
  for the shortest TTL. `keel.TTL` (default one minute) only bounds how long an unused entry
  stays; an entry is never served after a table it read changed.
- The entry is keyed by the statements, their arguments, the tenant and `Key(ctx)`, and checked
  against the version of every table the read touched: the base table, joins, relation loads,
  subqueries, `Any` and `None`, CTEs. A table only a `keel.Raw` fragment reads is invisible: name
  it with `keel.AlsoReads("table")`.
- `Key` returns what else the rows depend on that the scope does not capture. With `OnBegin`
  setting row-level security from a user's permissions, two users of one tenant may see different
  rows, so `Open` refuses a cache without `Key` when `OnBegin` is set. Return the encoded settings
  OnBegin applies.
- A write of this process is visible to its next cached read at once, before the versions are
  flushed. A write of another process is visible after its `DataVersions.Interval` and the
  notification's delivery (milliseconds).
- Entries hold the raw result Postgres sent, relation loads included, and every hit decodes it
  into new rows: callers never share structs, and `AfterLoad` hooks run on each hit.
- The cache is bypassed inside a `Write`, in a joined transaction, inside a `Read` at
  `RepeatableRead` or `Serializable` once it has sent a statement (its snapshot is taken), under a
  system scope, for a query with `Lock`, and while the listener is reconnecting. A read-committed
  `Read` uses it, and so does a snapshot `Read` until its first statement: a hit sends nothing and
  the transaction begins with the first statement that reaches the database, and a miss begins it
  and stores its result. A miss read on a replica is served but not stored.
- Reads of versions run on the terminal's own connection (its transaction's, or the one it is
  about to send on), so the cache never needs a second pool connection, even with a pool of one.
- `Shared` takes any store with `Get(ctx, key) ([]byte, bool, error)` and `Set(ctx, key, value,
  ttl) error`: `keel/cache/rediscache` wraps a Redis client through the same three-method adapter
  (`Get`, `Set`, `Del`) as `keel/analytics/cache`, and the stores of `keel/analytics/cache` fit
  as they are. A local miss reads it; an entry read while this process had unflushed writes to its
  tables stays local. Store errors are misses, reported to `OnError`.
- The listener is one dedicated connection per `DB` that runs `LISTEN keel_data_versions`, with
  the pool's settings unless `CacheConfig.Listen` (a `*pgx.ConnConfig` from `pgx.ParseConfig`)
  names another: behind a transaction-mode pooler such as PgBouncer, point `Listen` at Postgres
  directly. Hits decode on up to `GOMAXPROCS`
  decoder connections, each opened once (with `AfterConnect`) and closed, so a hit never takes a
  pooled connection.
- `db.CacheStats()` returns `Hits`, `Misses`, `Bypasses`, `Errors`, `Bytes` (local), `Versions`
  (pairs held) and `Listening`.

## Models

A model is a plain struct that embeds `keel.Table`, an empty marker whose `keel` tag names the
table. Every stored field carries a `keel` tag. These are the shipment and customer models of the
examples:

```go
// Customer is a tenant's customer.
type Customer struct {
    keel.Table `keel:"customers,report=Sales"`

    ID             ids.ID `keel:"id,pk,autoid:cus_"`
    OrganizationID ids.ID `keel:"organization_id,pk,tenant"`
    BusinessUnitID ids.ID `keel:"business_unit_id,pk,tenant"`
    // Name is the customer's trading name.
    Name      string  `keel:"name"`
    Code      string  `keel:"code"`
    Email     *string `keel:"email"`
    Version   int64   `keel:"version,version"`
    CreatedAt int64   `keel:"created_at,autocreate"`
    UpdatedAt int64   `keel:"updated_at,autoupdate"`
}

// Shipment is a load moved for a customer.
type Shipment struct {
    keel.Table `keel:"shipments,report=Operations"`

    ID             ids.ID  `keel:"id,pk,autoid:shp_"`
    OrganizationID ids.ID  `keel:"organization_id,pk,tenant"`
    BusinessUnitID ids.ID  `keel:"business_unit_id,pk,tenant"`
    CustomerID     ids.ID  `keel:"customer_id"`
    BillToID       *ids.ID `keel:"bill_to_id"`
    // ProNumber is the carrier's progressive number, unique per tenant.
    ProNumber string   `keel:"pro_number,label=PRO Number"`
    Status    Status   `keel:"status"`
    Priority  Priority `keel:"priority"`
    // Weight is the shipment's gross weight in pounds.
    Weight       *int64         `keel:"weight,format=weight"`
    Total        Money          `keel:"total,type=decimal"`
    Hazmat       bool           `keel:"hazmat"`
    Labels       []string       `keel:"labels"`
    Attributes   map[string]any `keel:"attributes,jsonb,noreport"`
    SearchVector string         `keel:"search_vector,noselect,readonly,tsvector"`
    StageRank    int16          `keel:"stage_rank,readonly,dimension"`
    CanceledAt   *int64         `keel:"canceled_at"`
    Version      int64          `keel:"version,version"`
    CreatedAt    int64          `keel:"created_at,autocreate"`
    UpdatedAt    int64          `keel:"updated_at,autoupdate"`

    Customer *Customer `keel:"belongs:customer_id"`
    BillTo   *Customer `keel:"belongs:bill_to_id,label=Bill-to Customer"`
    Moves    []*Move   `keel:"many:shipment_id"`
    Tags     []*Tag    `keel:"m2m:shipment_tags.shipment_id.tag_id,noreport"`
    // OriginStop is the first stop of the shipment's first move.
    OriginStop *Stop `keel:"pick:moves.stops,first=moves.sequence|stops.sequence"`
    // DestinationStop is the last stop of the shipment's last move.
    DestinationStop *Stop `keel:"pick:moves.stops,last=moves.sequence|stops.sequence"`
}
```

`ids.ID` is `type ID string`, `Status` a string enum, `Priority` an `int16` enum, and `Money` a
`sql.Scanner` stored as `numeric` whose `KeelFormat` method says it is displayed as money.

Table and column names are lower-case identifiers (`[a-z_][a-z0-9_]*`); schema-qualified names are
not accepted. A field tagged `keel:"-"`, or without a `keel` tag, is not stored (the `Worker`
model's `Initials` is derived in a hook). Stored fields must be exported. keel reads only its own
tag key, so `db`, `json` or `bun` tags on the same struct are left to their libraries.

### Tag reference

A tag is a head (the table or column name, or a relation) followed by comma-separated options.
Options are plain words or `key=value`; `autoid` keeps its `autoid:<prefix>` form. keel validates
every option: an unknown option, a repeated option, a value on a plain word, an empty value or a
malformed one fails `keel gen` with the field's position (`models/shipment.go:48:2:
Shipment.Weight: keel tag "weight,fromat=weight": unknown option "fromat"`).

**On the `keel.Table` marker**

| Option | Meaning | Example |
|---|---|---|
| `<name>` | the table name, first | `keel:"shipments"` |
| `report` / `report=<Category>` | the model is a report's starting entity, optionally grouped under a category | `keel:"shipments,report=Operations"` |
| `label=<text>` | the model's label instead of the derived one | `keel:"bols,label=Bill of Lading"` |
| `plural=<text>` | the plural label instead of the derived one | `keel:"people,label=Person,plural=People"` |

**On a column**

| Option | Meaning | Example |
|---|---|---|
| `<name>` | the column name, first | `keel:"pro_number"` |
| `pk` | part of the primary key | `keel:"id,pk"` |
| `tenant` | a tenant column: filled from the scope on insert, compared with the scope on every statement, added to every relation join | `keel:"organization_id,pk,tenant"` |
| `autoid:<prefix>` | on insert, an empty value is replaced by `Config.NewID(prefix)`; the field must be a string type | `keel:"id,pk,autoid:shp_"` |
| `autocreate` | on insert, a zero value is stamped with the time and a value the caller set (an imported row's) is kept; `int64` means Unix seconds, `time.Time` a timestamp | `keel:"created_at,autocreate"` |
| `autoupdate` | stamped on insert and on every update, bulk updates and upserts included | `keel:"updated_at,autoupdate"` |
| `version` | optimistic locking: `Save` compares and increments it, bulk updates and upserts increment it; must be `int64` | `keel:"version,version"` |
| `readonly` | never written (generated or trigger-maintained columns); read back after inserts and saves | `keel:"stage_rank,readonly"` |
| `noselect` | not selected by default; usable in predicates. Mark a generated `noselect` column `readonly` too | `keel:"search_vector,noselect,readonly,tsvector"` |
| `jsonb` | the value is encoded as JSON; a nil map or slice in a non-pointer field is written as `{}` or `[]` | `keel:"attributes,jsonb"` |
| `jsonnumber` | with `jsonb` only: reads decode numbers held in interface values (`map[string]any`, `[]any`, `any`) as `json.Number`, which keeps an integer past 2^53 exact, instead of `float64` (see [JSON](#json)) | `keel:"arguments,jsonb,jsonnumber"` |
| `sqltype=<name>` | the column's SQL type, for a type an expression cannot be assigned to without a cast, such as an enum: `ToExpr` writes `CAST(e AS <name>)`, and `Verify` reports a column whose type differs; an identifier, optionally schema-qualified, with `[]` for an array | `keel:"billing_queue_status,sqltype=billing_queue_status"` |
| `tsvector` / `tsvector=<config>` | the column gets the full-text operators (see [Full-text search](#full-text-search)); `<config>` is the text search configuration its searches use by default, an identifier, optionally schema-qualified | `keel:"search_vector,noselect,readonly,tsvector=english"` |
| `nullzero` | the field's zero value is stored as NULL and NULL reads back as the zero value; see [Nullzero columns](#nullzero-columns) | `keel:"customer_id,nullzero"` |
| `default` | an insert that leaves the field unset (a nil pointer, a NULL `Null…` value, a `nullzero` zero value) takes the column's database default instead of NULL: `DEFAULT` in a single-row insert, `coalesce(value, default)` in a bulk one, with the default read from the catalog, and the inserted value is read back into the field; `Copy` refuses an unset value, since `COPY` cannot apply a default. The field must be able to be unset, and the column must have a default | `keel:"threshold,default"` |
| `computed` | not a column of the table: never inserted, updated or selected by default; a query fills it with `Compute`; see [Computed fields](#computed-fields). The name is how dynamic queries address it | `keel:"rank,computed"` |
| `label=<text>` | the column's label instead of the derived one | `keel:"pro_number,label=PRO Number"` |
| `noreport` | reports do not offer the column | `keel:"internal_key,noreport"` |
| `report` | reports offer a column they leave out by default (a tenant, version, `noselect`, bytea or tsvector column, or an exposed foreign key) | `keel:"customer_id,report"` |
| `dimension` / `measure` | reports group by / aggregate the column instead of what keel derives; `measure` needs a numeric column | `keel:"sequence,dimension"` |
| `agg=<a>\|<b>…` | the aggregations reports offer, from `count`, `count_distinct`, `sum`, `avg`, `min`, `max`; each must apply to the value type | `keel:"sequence,agg=min\|max"` |
| `format=<name>` | how reports display values (`[A-Za-z0-9_.:-]+`), instead of the type's `KeelFormat` | `keel:"weight,format=weight"` |
| `type=epoch` / `type=date` / `type=decimal` | the value type reports read: an `int64` of Unix seconds, a `time.Time` holding a date, a scanner or string holding a decimal | `keel:"total,type=decimal"` |

A model has at most one `version`, `autoid`, `autocreate` and `autoupdate` column. `readonly`
cannot be combined with `pk`, `tenant`, `version`, `autoid`, `autocreate` or `autoupdate`, and
neither can `nullzero`; `computed` excludes those, `readonly` and `noselect`; `jsonb` and `tsvector`, `report` and `noreport`, `dimension` and `measure`
exclude each other. The report
options (`noreport`, `report`, `dimension`, `measure`, `agg=`, `format=`, `type=`) need `report` on
the model's table tag.

There is no `default:` or `notnull`: the SQL migrations own the schema. The value in the struct is
the value written; keel never substitutes `DEFAULT` for a zero value. A nil slice in a non-pointer
array field is written as an empty array, unless the column is `nullzero`.

**On a relation**

| Head or option | Meaning | Example |
|---|---|---|
| `belongs:<fk>` | this table's `<fk>` column references the target's primary key | `Customer *Customer \`keel:"belongs:customer_id"\`` |
| `one:<fk>` | the target's `<fk>` references this table's primary key; at most one row | `Profile *DriverProfile \`keel:"one:worker_id"\`` |
| `many:<fk>` | the target's `<fk>` references this table's primary key | `Moves []*Move \`keel:"many:shipment_id"\`` |
| `belongs:<fk>\|<fk>[=<key>]…` (and `one:`, `many:`) | a key of several columns: every pair joins; see [Relations](#relations) | `Source *Leave \`keel:"belongs:source_leave_id\|worker_id"\`` |
| `m2m:<join_table>.<local_fk>.<remote_fk>` | rows linked through a join table: `<local_fk>` references this table, `<remote_fk>` the target; between two tenant tables the join table also carries the tenant columns they share (see [Relations](#relations)) | `Tags []*Tag \`keel:"m2m:shipment_tags.shipment_id.tag_id"\`` |
| `pick:<relation>.<relation>…` | one row picked along a path that crosses a to-many relation (see [Pick relations](#pick-relations)) | `OriginStop *Stop \`keel:"pick:moves.stops,first=moves.sequence\|stops.sequence"\`` |
| `first=<segment>.<column>\|…` / `last=…` | a pick's order: the first or the last row by these columns; exactly one is required on `pick` and allowed nowhere else | see `pick` |
| `where=<segment>.<column>:<value>[;<value>…]\|…` | a pick chooses only among rows whose column holds one of the values; only on `pick` | `keel:"pick:moves.stops,first=moves.sequence\|stops.sequence,where=stops.type:Pickup"` |
| `label=<text>` | the relation's label instead of the derived one | `keel:"belongs:bill_to_id,label=Bill-to Customer"` |
| `noreport` | reports do not offer the relation as an edge | `keel:"m2m:shipment_tags.shipment_id.tag_id,noreport"` |

### Go types

| Go field type | Column type in the generated handle |
|---|---|
| `string` | `keel.StringCol` |
| `int64` or `int`, `int32`, `int16` | `keel.Int64Col`, `keel.Int32Col`, `keel.Int16Col` |
| `float64`, `float32` | `keel.Float64Col` |
| `bool` | `keel.BoolCol` |
| `[]byte` | `keel.BytesCol` |
| `time.Time` | `keel.TimeCol` (timestamp, timestamptz or date) |
| `[]string` | `keel.StringArrayCol` |
| `[]int64` | `keel.Int64ArrayCol` |
| `[]int`, `[]int32`, `[]int16`, `[]float64`, `[]float32`, `[]bool` | `keel.AnyCol`; pgx encodes and scans the slice, and a nil slice is written as an empty array |
| `string` with `tsvector` | `keel.TSVectorCol` |
| any type with `jsonb` | `keel.JSONBCol` |
| a named type over one of the above (`Status`, `ids.ID`) | a generated wrapper (`StatusCol`, `IdsIDCol`) whose methods take the named type |
| a type implementing `sql.Scanner` (`Money`, a decimal, a PostGIS point) | a generated wrapper over `keel.AnyCol` (`MoneyCol`) whose methods take the type; when only the pointer implements `driver.Valuer`, the wrapper and the generated `Value` accessor bind a pointer |

A pointer field (`*int64`, `*ids.ID`) or a `sql.Scanner` whose name starts with `Null` is nullable
and has the same column type. An `int` field is a `bigint` value read and written as `int64`; the
generated package fails to build where `int` is narrower than 64 bits. `version`, `autocreate` and
`autoupdate` columns still need `int64`. Structs, maps and slices of other element types need the
`jsonb` option.

A named type over a basic type is stored as that basic type even when it implements `sql.Scanner`
and `driver.Valuer`, as `ids.ID` here and Trenova's `pulid.ID` do: keel reads and writes the
underlying string or number straight through the field, so the column keeps its typed operators,
can key relations and travels in typed bulk arrays, and the type's `Scan` and `Value` methods are
not called. A type whose methods store its zero value as NULL gets that behaviour from `nullzero`.
Only struct and other non-basic types use their `Scan` and `Value` methods.

### Nullzero columns

`nullzero` is for a non-pointer field whose column is nullable: the zero value of the field (`""`,
`0`, `false`, the zero `time.Time`, a nil slice or map, a struct's zero value) is stored as NULL,
and NULL reads back as the zero value. Optional foreign keys are the common case: an empty
`CustomerID` must be NULL, not `''`, for the foreign key and partial unique indexes to hold.

```go
type Contact struct {
    keel.Table `keel:"contacts"`

    ID         ids.ID    `keel:"id,pk,autoid:con_"`
    // …tenant columns…
    CustomerID ids.ID    `keel:"customer_id,nullzero"`
    Phone      string    `keel:"phone,nullzero"`
    LastSeenAt time.Time `keel:"last_seen_at,nullzero"`
    Balance    Money     `keel:"balance,nullzero"`
    Notes      []string  `keel:"notes,nullzero"`

    Customer *Customer `keel:"belongs:customer_id"`
}
```

- **Writes.** Every write stores a zero value as NULL: `Create`, `CreateMany` (the column's typed
  array carries NULL elements), `Copy`, `Upsert` and `CreateOrSkip`, `Save` with or without named
  columns, bulk `Update` and `Conflict.Set` (`c.Phone.To("")` binds NULL), and `InsertFrom`, which
  wraps a picked value of a string, number, bool or time column in `nullif(…, zero)`.
- **Reads.** NULL decodes into the zero value through a scan plan cached per connection, without
  reflection or an allocation per value for the basic kinds and `time.Time`.
- **Predicates.** A value compared with a nullzero column follows the field's meaning: `Eq(zero)` is
  `IS NULL`, `Ne(zero)` and `IsDistinctFrom(zero)` are `IS NOT NULL`, `Ne(v)` is `IS DISTINCT FROM v`
  so rows holding the zero value match, `In(…)` with the zero value in the list adds `OR col IS
  NULL`, and `NotIn(…)` without it keeps the NULL rows. The dynamic layer's `Eq`, `Ne`, `In` and
  `NotIn` on a nullzero column do the same. Ordering comparisons (`Gt`, `Lt`, `Between`) see NULL as
  SQL does: it never matches.
- **Ordering and pages.** NULL sorts last ascending and first descending, as in Postgres; keyset
  pages over a nullzero column place its zero values that way.
- **Relations.** A nullzero foreign key holding the zero value has no related row: `Load` leaves the
  relation nil, and key lists for relation loads skip it.

The generated `Value` accessor tests the zero value (`r.Phone == ""`, `r.LastSeenAt.IsZero()`,
`r.Balance == (Money{})`): a struct type needs an `IsZero() bool` method or must be comparable.
`nullzero` on a pointer field or a `Null…` scanner is refused, since those store NULL already, and
so is `nullzero` on a `pk`, `tenant`, `version`, `autoid`, `autocreate` or `autoupdate` column.

### Relations

A relation field is a pointer (to-one: `belongs`, `one`, `pick`) or a slice (to-many: `many`,
`m2m`) of another model. A model may relate to itself (`Worker.Manager *Worker
\`keel:"belongs:manager_id"\``) and to the same target twice (`Customer` and `BillTo`).

A many-to-many relation between two tenant tables reads its join table as a table of the same
tenants: the join table must carry the tenant columns the two tables share, under the same names
(`shipment_tags (shipment_id, tag_id, organization_id, business_unit_id)`), and keel joins it to
the target on them as well as on `<remote_fk>`. A join table row then links a parent and a target
of its own tenant even when keys repeat across tenants. Code that reads a join table itself gets its
description from `parent.JoinTableOf(&parent.Relations[i])`: a `*schema.Table` with the two keys and
the shared tenant columns (`schema.SharedTenant`) flagged `tenant`, so `keel.DynFrom` filters it by
the scope's tenant like any tenant table.

A relation can join on several columns: list them with `|`. Each item names a foreign-key column
(this table's for `belongs`, the target's for `one` and `many`) and, after `=`, the column it
references on the other side; without `=`, the first item references the primary key and the
others the column of the same name. A ledger entry that draws on a leave of the same worker:

```go
type Leave struct {
    keel.Table `keel:"leaves"`
    ID       ids.ID `keel:"id,pk,autoid:lv_"`
    // …tenant columns…
    WorkerID ids.ID `keel:"worker_id"`

    Entries   []*LeaveEntry `keel:"many:source_leave_id|worker_id"`
    LastEntry *LeaveEntry   `keel:"pick:entries,last=entries.posted_at"`
}

type LeaveEntry struct {
    keel.Table `keel:"leave_entries"`
    ID            ids.ID `keel:"id,pk,autoid:lve_"`
    // …tenant columns…
    WorkerID      ids.ID `keel:"worker_id"`
    SourceLeaveID ids.ID `keel:"source_leave_id,nullzero"`
    PostedAt      int64  `keel:"posted_at"`

    Source *Leave `keel:"belongs:source_leave_id|worker_id"` // = leaves.id AND = leaves.worker_id
}
```

Every use honours every pair: joins and relation path filters (`e.Source().Days.Gt(1)`), loads
(to-many loads filter by `(fk1, fk2) IN (SELECT * FROM unnest($1, $2))` and attach by the whole key,
`Limit` loads and picks unnest one array per key column), `Any` and `None`, picks whose first hop
has several columns, bulk updates and deletes through the relation, the dynamic layer's paths,
`Rel`, `Any`, `None` and pre-aggregations, and the analytics edges built on them.

Rules:

- A key column given without `=`, other than the first, must exist on the other side under the
  same name; name it with `=` otherwise. Paired columns must hold the same kind of value (any two
  integer widths pair). The referenced columns should be unique on their table (the primary key,
  or a unique constraint as `(id, worker_id)` above).
- With one column and no `=`, the relation references the primary key besides its tenant columns:
  its single column, or, when it has several (`(id, customer_id, organization_id, …)`), the one
  named `id`. Many-to-many relations reference the same column on each side.
- A tenant column both sides carry is not part of the relation key and is refused in it; one that
  references a table without it (`belongs:organization_id` to `organizations.id`) is an ordinary
  key. When both sides have
  them, keel adds them to every join, as constants from the scope under a tenant scope and as
  column equalities under a system scope.
- Keys of to-many and pick relations must be string or integer columns. Many-to-many relations
  take one column per side.
- The target must be a model of the same `keel gen` run.

### Pick relations

A pick relation is a to-one relation that picks one row along a to-many path in a total order: a
shipment's origin stop is the first stop of its first move.

```go
OriginStop      *Stop `keel:"pick:moves.stops,first=moves.sequence|stops.sequence"`
DestinationStop *Stop `keel:"pick:moves.stops,last=moves.sequence|stops.sequence"`
```

- The path names relations hop by hop, each by its field name in snake case (`Moves` → `moves`,
  `BillTo` → `bill_to`), starting from the model's own relations. It must cross at least one
  to-many relation, may not walk through another pick, and must end at the field's model.
- `first=` and `last=` list order keys as `<segment>.<column>`: a column of the table that path
  segment reaches. `last=` orders every key descending. `keel gen` appends the target's primary
  key when the order does not end with it, so the order is total and the pick is deterministic;
  keys after the target's key are refused because they never apply.
- `where=` narrows the rows the pick chooses from, as `<segment>.<column>:<value>[;<value>…]`
  conditions joined by `|`: the row that segment reaches must hold one of the values. Values are
  constants written into the subquery as SQL string literals, so they cannot hold a comma, `|` or
  `;`, and the column must accept a text literal (text, enum or number columns all do).

  ```go
  FirstPickup *Stop `keel:"pick:moves.stops,first=moves.sequence|stops.sequence,where=stops.type:Pickup;SplitPickup"`
  ```
- The model's key the first hop starts from must be a string or integer column.

`Load(s.OriginStop())` fills the field, and the pick's own relations load like any other
(`s.OriginStop().Location()`). A statement that reads at most two rows in no particular order
(`One`, `Get`, `Limit(1)` without `Order`) joins the pick into itself as `LEFT JOIN LATERAL (… ORDER
BY … LIMIT 1) os ON TRUE`, so the lateral subquery runs at most twice. Any other read loads the pick
after its statement, in the next round trip with the level's other loads, as one statement per
pick for all the parents' keys: `unnest($1) k CROSS JOIN LATERAL (… WHERE m.shipment_id = k.k …
LIMIT 1)`, so the work is bounded by the rows returned, not by the rows the filter scanned. A pick
beneath a to-many load (`s.Moves().FirstStop()`) loads the same way, one lateral per move.

```go
s := fleetdb.Shipments
origin := s.OriginStop()

rows, err := s.
    Where(origin.Location().City.Eq("Austin")).
    Load(origin.Location(), s.DestinationStop().Location()).
    Order(s.CreatedAt.Desc()).
    Limit(50).
    List(ctx, db)
```

Filtering through a pick (`origin.Location().City.Eq(...)`) joins the lateral subquery into the
statement; `origin.Any(...)` and `origin.None()` compile to `EXISTS` over it. Every hop of the
subquery is scoped to the tenant (constants under a tenant scope, equalities with the previous
table under a system scope, which needs `AllTenants()`) and filtered by the interceptors of its
table. A pick load takes `Where` (a picked row that does not match leaves the field nil) but no
`Limit`, and bulk updates and deletes cannot filter through a pick: `UPDATE … FROM` cannot reference
the updated table from a lateral subquery.

### Derived metadata

`keel gen` writes what it can derive from the source into the generated schema, so nothing about a
model is written twice. `fleetdb.ShipmentsSchema()` returns it at run time for any feature to read
(the analytics catalog, forms, exports, error messages):

| Fact | Where | Derived from | Override |
|---|---|---|---|
| Label | `Table.Label`, `Column.Label`, `Relation.Label` | the model or field name, split at case changes, digits and underscores; acronyms in capitals (`ProNumber` → "PRO Number" with `-acronyms PRO`, `CustomerID` → "Customer ID", `Address2` → "Address 2") | `label=` |
| Plural label | `Table.Plural` | the label with its last word in the plural ("Shipments", "Categories") | `plural=` |
| Description | `Table.Doc`, `Column.Doc`, `Relation.Doc`, `EnumValue.Doc` | the doc comment (a field's line comment when it has no doc comment), paragraphs kept, lines joined | — |
| JSON name | `Column.JSON`, `Relation.JSON` | the field's `json` tag: its name (`json:"proNumber,omitempty"` → "proNumber"), the field name when the tag names none (`json:",omitempty"`), empty without a `json` tag or for `json:"-"` | — |
| Enum values | `Column.Enum` | the constants of the column's named string or integer type, in declaration order, from any package; labels from the constant names without the type name (`StatusInTransit` → "In Transit") | — |
| Reportable entity | `Table.Report` | `report[=Category]` on the table tag | — |
| Included | `Column.Report.Included` | every column except tenant, version, `noselect`, bytea and tsvector columns and foreign keys a reported `belongs` relation exposes | `noreport`, `report` |
| Value type | `Column.Report.Type` | the Go type: strings, enums, integers, floats, decimals (`shopspring/decimal`, `pgtype.Numeric`), bools, times, JSON, arrays, bytea, tsvector; `int64` columns named `*_at`, `*_date` or `*_time` are epochs | `type=` |
| Measure or dimension | `Column.Report.Kind` | numeric columns that are neither keys (primary keys, foreign keys of `belongs`, `one` and `many` relations) nor enums are measures; everything else is a dimension | `dimension`, `measure` |
| Aggregations | `Column.Report.Aggs` | numbers: count, count distinct, sum, avg, min, max; strings, times, dates and epochs: count, count distinct, min, max; everything else: count, count distinct | `agg=` |
| Format | `Column.Report.Format` | a `KeelFormat() string` method on the column's type, value or pointer receiver, returning a string literal: `func (Money) KeelFormat() string { return "money" }` | `format=` |

An API that takes field names in filters and sorts (`?sort=customer.name`) maps them to columns
and relations with the JSON names, then builds the query with
[`keel.ResolvePath`](#column-paths-from-names): no reflection over the models, no cache of their tags.

The default acronyms are ID, URL, API, JSON and UUID; `keel gen -acronyms PRO,BOL,SCAC,DOT,MC,ID,URL`
(or `gen.Options.Acronyms`) replaces the list. Words already in capitals in the source stay in
capitals. Report facts are written only for models tagged `report`, and the report options are
refused elsewhere. Labels are the fallback text of translation keys; the analytics layer translates
them.

The generated file declares each enum's values once, as an array that every column of the type
slices, and each distinct set of report facts once (`reportDimEpoch`, `reportMeasureDecimalMoney`),
so the schema stays static data that compiles without init-time work.

### Model hooks

Methods on the model with these signatures. The generator detects them and writes direct calls;
there is no runtime type assertion.

```go
func (s *Shipment) BeforeCreate(ctx context.Context) error // Create, CreateMany, Upsert, CreateOrSkip
func (s *Shipment) BeforeUpdate(ctx context.Context) error // Save
func (s *Shipment) BeforeSave(ctx context.Context) error   // after BeforeCreate or BeforeUpdate
func (s *Shipment) BeforeDelete(ctx context.Context) error // DeleteRow
func (s *Shipment) AfterLoad(ctx context.Context) error    // every loaded row, relations included
```

A method with one of these names and another signature is a generation error. The `Worker` model
trims names before every write and derives initials after every load:

```go
func (w *Worker) BeforeSave(_ context.Context) error {
    w.FirstName = strings.TrimSpace(w.FirstName)
    w.LastName = strings.TrimSpace(w.LastName)
    return nil
}

func (w *Worker) AfterLoad(_ context.Context) error {
    if w.FirstName != "" && w.LastName != "" {
        w.Initials = w.FirstName[:1] + w.LastName[:1]
    }
    return nil
}
```

Model hooks are for normalisation and derived fields. Use [table hooks](#table-hooks) for
validation that needs the database, audit and side effects.

## Generating code

`keel gen` loads the model packages with `go/packages` and writes one data-access package per
package that holds models. It validates every tag, links relations, resolves picks and derives the
[metadata](#derived-metadata); every error names the file, line and column of the field at fault.

```sh
go run github.com/emoss08/keel/cmd/keel gen ./internal/...
```

| Flag | Default | Meaning |
|---|---|---|
| `-out` | `{dir}/{name}db` | directory of each generated package, relative to the module root. `{dir}` is the model package's directory relative to the module root, `{name}` its package name |
| `-kinds` | `keelkinds` | directory, relative to the module root, of the shared package that holds column types for named types declared outside model packages |
| `-tables` | `keeltables` | directory, relative to the module root, of the package whose `All()` lists every generated table in name order, for code that works over all of them: `keel.NewRegistry(keeltables.All()...)`, `analytics.Build`, `analyticscli.Config.Tables`; and whose `Catalog()` finds a table by name (`Table`) or by model (`Model`, false when two packages share the model name) |
| `-tags` | none | comma-separated build tags used to load the packages |
| `-check` | `false` | fail, listing the stale files, when generated files are out of date, instead of writing them |
| `-acronyms` | `ID,URL,API,JSON,UUID` | comma-separated words that labels write in capitals (`PRO,BOL,SCAC,DOT,MC,ID,URL`); replaces the default list |

Without package patterns it loads `./...`. Pass every model package in one run: a relation's
target must be a model the same run generates. Errors in previously generated packages are ignored,
so removing or renaming a model field never stops the run that rewrites them. keel's own Makefile
runs:

```sh
go run ./cmd/keel gen -kinds internal/testmodels/keelkinds ./internal/testmodels/...
```

which writes `internal/testmodels/fleet/fleetdb/keel_gen.go` and
`internal/testmodels/keelkinds/keel_gen.go`. An `-out` template without `{dir}` puts every package
under one directory, for example `-out 'internal/dbq/{name}db'`; model packages must then have
distinct names.

Pin the generator in `go.mod` with a tool directive (`go get -tool github.com/emoss08/keel/cmd/keel`,
then `go tool keel gen …`), or use `go run` with a version, or a `//go:generate` line in one place.

### In CI

```sh
go run github.com/emoss08/keel/cmd/keel gen -check -kinds internal/dbq/keelkinds ./internal/...
```

`-check` exits non-zero and prints the files to regenerate. Run it with the same flags as the
command that writes the files. It keeps the generated code in step with the models; to keep the
models in step with the migrations, run `keel.Verify` in an integration test (see
[Verifying the schema](#verifying-the-schema)).

### What a generated package contains

The generated file is `keel_gen.go` in the `-out` directory. For each model (here `fleet.Shipment`)
it holds:

| Declaration | What it is |
|---|---|
| `type ShipmentsTable struct` | the handle type: embeds `keel.Handle[*fleet.Shipment]` and has one field per column (`ID`, `ProNumber`, `Status`, …) |
| `var Shipments` | the base handle, built statically; queries and writes start here |
| `func (t *ShipmentsTable) Customer() *CustomersTable` | one method per relation, returning the handle of the related table on that path |
| `func (t *ShipmentsTable) As(alias string) *ShipmentsTable` | a second reference to the table for self-joins and correlated subqueries |
| `func (t *ShipmentsTable) Get(ctx, db, key ids.ID) (*fleet.Shipment, error)` | fetch by primary key; generated when the key, besides tenant columns, is one non-pointer column |
| `func BuildShipments(*keel.Ref) *ShipmentsTable` | builds a handle for a reference; relation and alias methods call it |
| `func ShipmentsSchema() *schema.Table` | the table metadata, written as a static literal with field accessors, hook trampolines, labels, docs, enum values and report facts |

keel's generic types (`Handle`, `Query`, `Page`, `Mutation`, `ListInto` and the other batch items)
take the model's pointer type: `keel.Query[*fleet.Shipment]`, whose terminals return
`*fleet.Shipment` and `[]*fleet.Shipment`. Every pointer type shares one shape, so a package that
can see a hundred handles compiles keel's generic methods once instead of a hundred times; a struct
type argument panics on first use.

The handle name is the model name in the plural: `Shipment` → `Shipments`, `Address` →
`Addresses`, `Company` → `Companies`. Named column types declared in a model package get their
wrapper in that package's generated package (`fleetdb.StatusCol`, `fleetdb.PriorityCol`,
`fleetdb.MoneyCol`); named types declared elsewhere get theirs in the `-kinds` package, prefixed
with their package name (`keelkinds.IdsIDCol` for `ids.ID`; `pulid.ID` would be `PulidIDCol`).

Values many columns share are declared once per file: `enumStatus`, an array of
`schema.EnumValue` that every `Status` column slices, and one `schema.ReportInfo` per distinct set
of report facts (`reportDimEpoch`, `reportMeasureDecimalMoney`), named by its content.

Only data-access packages should import generated packages. The generic types they mention
(`keel.Handle[T]`, `keel.Query[T]`) then stay out of the rest of the build.

## Reading

### Handles and queries

A query starts from a handle and ends with a terminal method that takes `(ctx, db)`:

```go
s := fleetdb.Shipments
rows, err := s.Where(s.Status.Eq(fleet.StatusNew)).Order(s.CreatedAt.Desc()).Limit(50).List(ctx, db)
```

A handle starts a query with `Query`, `Where`, `Pick`, `Load`, `Order`, `Limit`, `Group`, `Join`,
`LeftJoin`, `JoinLateral`, `LeftJoinLateral`, `Lock`, `DistinctOn`, `With` or `AllTenants`.
`Having`, `Offset`, `CustomPlan` and `Timeout` exist on queries only: `s.Query().Timeout(d)`.

Handles are immutable and safe to share. Queries are builders: each method changes the query and
returns it. Build a query per call, and `Clone` one to branch:

```go
base := s.Where(s.CustomerID.Eq(customerID))
open := base.Clone().Where(s.Status.Ne(fleet.StatusDelivered))
```

### Terminals

| Method | Result |
|---|---|
| `Get(ctx, db, key)` | the row with this primary key (generated on the handle); `ErrNotFound` when none |
| `One` | exactly one row: `ErrNotFound` when none, `ErrNotSingular` when more |
| `First` | the first row in the query's order, `ErrNotFound` when none |
| `List` | the rows; returns `ErrUnbounded` without `Limit` |
| `All` | every row; for sets known to be small |
| `Count` | the number of rows (of groups, for a grouped query) |
| `CountUpTo(ctx, db, limit)` | the number of rows, counting no further than `limit` |
| `CountEstimate(ctx, db, exactUpTo)` | a `keel.RowCount{N, Exact}`: exact up to `exactUpTo`, the planner's estimate above it; see [Estimated counts](#estimated-counts) |
| `Exists` | whether any row matches |
| `Page(ctx, db, req)` | a keyset page; see [Pagination](#pagination) |
| `Each(ctx, db, size, fn)` | every row in keyset batches at constant memory |
| `Update(ctx, db, sets...)`, `Delete(ctx, db)` | bulk writes; see [Writing](#writing) |
| `keel.Into[R](ctx, db, q)`, `keel.IntoOne[R]` | projections into a struct or scalar; see [Projections](#projections-and-aggregates) |
| `ToSQL(ctx, db)` | the rendered statement, without running it |

```go
sh, err := s.Get(ctx, db, "shp_01")
sh, err = s.Where(s.ProNumber.Eq("PRO-1001")).One(ctx, db)
newest, err := s.Where(s.CustomerID.Eq(sh.CustomerID)).Order(s.CreatedAt.Desc()).First(ctx, db)
recent, err := s.Where(s.Status.Eq(fleet.StatusNew)).Order(s.CreatedAt.Desc()).Limit(50).List(ctx, db)
tags, err := fleetdb.Tags.Query().All(ctx, db)

open := s.Where(s.Status.In(fleet.StatusNew, fleet.StatusAssigned))
n, err := open.Count(ctx, db)
upTo, err := open.CountUpTo(ctx, db, 1000)
about, err := open.CountEstimate(ctx, db, 1000) // {N: 1204, Exact: true} or {N: 1180000, Exact: false}
exists, err := open.Exists(ctx, db)
```

Rows come back as `*T`. `Pick` with columns of the table narrows what is read: the other fields
stay zero, and keel adds the primary key columns. `Pick` with anything else (an aggregate, a
column of another table) makes the query a projection, which needs `keel.Into`.

```go
sh, err := s.Pick(s.ProNumber, s.Status).Where(s.ID.Eq(id)).One(ctx, db) // sh.Total is zero
```

`Omit` is the other way round: the default columns except the ones named, for a heavy or secret
column (a transcript, a file's contents, a token). The primary key and the columns relation loads
need are read anyway, and loads take it too:

```go
runs, err := r.Omit(r.Transcript).Where(r.ThreadID.Eq(id)).Limit(50).List(ctx, db)
rows, err := s.Load(s.Customer().Omit(s.Customer().Email)).Limit(50).List(ctx, db)
```

### Operators

Every clause takes expressions, never strings, and each column offers the operators of its Go
type. Values are bind parameters.

```go
since := time.Now().Add(-24 * time.Hour)
n, err := s.Where(
    s.Status.In(fleet.StatusNew, fleet.StatusAssigned),
    s.Priority.Gte(2),
    s.Weight.Between(10_000, 40_000),
    s.BillToID.IsNull(),
    s.ProNumber.HasPrefix("PRO-"),
    s.Hazmat.IsFalse(),
    s.Total.Gt(fleet.Money{Cents: 50_000}),
    s.Labels.Has("expedite"),
    s.Attributes.Contains(map[string]any{"temperature": "frozen"}),
    s.Attributes.Text("trailer", "type").Eq("reefer"),
    s.SearchVector.Match("dallas reefer"),
    keel.Or(s.CanceledAt.IsNull(), s.CanceledAt.Gt(since.Unix())),
    keel.Not(s.Customer().Name.ContainsFold("test")),
).Count(ctx, db)

st := fleetdb.Stops
late, err := st.Where(st.ArrivedAt.Gte(since), st.Kind.Eq("delivery")).Count(ctx, db)
```

| Column type | Operators |
|---|---|
| every column | `IsNull`, `IsNotNull`, `EqCol`, `NeCol`, `InQuery`, `NotInQuery`, `Asc`, `Desc`, `Count`, `CountDistinct`, `Min`, `Max`, `Coalesce(v)`, `ToNull`, `ToDefault`, `ToExpr(e)` |
| `StringCol` | `Eq`, `Ne`, `Gt`, `Gte`, `Lt`, `Lte`, `Between`, `IsDistinctFrom`, `IsNotDistinctFrom`, `In`, `NotIn`, `To`; `Contains`, `ContainsFold`, `HasPrefix`, `HasPrefixFold`, `HasSuffix` (their argument is escaped, so `%` and `_` match themselves); `Like`, `ILike` (patterns as given); `Match`, `MatchFold` (POSIX regular expressions); `Cat` (`\|\|`), `Collate`, `InArray` and the string functions of [Functions](#functions) |
| `Int64Col`, `Int32Col` | `Eq`, `Ne`, `Gt`, `Gte`, `Lt`, `Lte`, `Between`, `IsDistinctFrom`, `IsNotDistinctFrom`, `In`, `NotIn`, `To`, `Increment`, `Sum`, `Avg`; `Add`, `Sub`, `Mul`, `Div`, `Mod`, `Neg`, `Abs`, `NullIf`, `Greatest`, `Least`, `InArray` |
| `Int16Col` | as `Int32Col`, without `Avg`, the arithmetic and the functions (`keel.CastInt64(col)` first) |
| `Float64Col` | `Eq`, `Ne`, `Gt`, `Gte`, `Lt`, `Lte`, `Between`, `IsDistinctFrom`, `IsNotDistinctFrom`, `To`, `Increment`, `Sum`, `Avg`; `Add`, `Sub`, `Mul`, `Div`, `Neg`, `Abs`, `Round`, `Floor`, `Ceil`, `Ln`, `Exp`, `NullIf`, `Greatest`, `Least`, `InArray` |
| `BoolCol` | `Eq`, `IsDistinctFrom`, `IsNotDistinctFrom`, `IsTrue`, `IsFalse`, `To` |
| `BytesCol` | `Eq`, `To` |
| `TimeCol` | `Eq`, `Ne`, `Gt`, `Gte`, `Lt`, `Lte`, `Between`, `IsDistinctFrom`, `IsNotDistinctFrom`, `To`; `Add(interval)`, `Sub(interval)`, `Since(t)`, `ToChar`, `AtTimeZone`, `Greatest`, `Least`, `InArray` |
| `AnyCol` and its wrappers (`MoneyCol`) | `Eq`, `Ne`, `Gt`, `Gte`, `Lt`, `Lte`, `Between`, `IsDistinctFrom`, `IsNotDistinctFrom`, `In`, `NotIn`, `To`, `Increment`, `Sum`, `Avg`; numeric `Add`, `Sub`, `Mul`, `Div`, `Mod`, `Neg`, `InArray` |
| `JSONBCol` | `Contains(v)` (`@>`, `v` encoded as JSON), `HasKey`, `HasAnyKey`, `HasAllKeys`, `Text(path...)` (`#>>`, a `StringExpr`), `To`; `TypeOf`, `Set`, `Concat`, `ArrayElementsText` |
| `StringArrayCol` | `Has`, `Overlaps`, `ContainsAll`, `ContainedBy`, `Len` (an `Int32Expr`), `Unnest`, `To`, `Append`, `Remove`; `At(i)`, `Without(v)`, `ContainsAllExpr`, `ContainedByExpr`, `OverlapsExpr` (see [Typed arrays](#typed-arrays)) |
| `Int64ArrayCol` | `Has`, `Overlaps`, `ContainsAll`, `ContainedBy`, `Len`, `Unnest`, `To`, `Append`, `Remove`; `At(i)`, `Without(v)`, `ContainsAllExpr`, `ContainedByExpr`, `OverlapsExpr` |
| `TSVectorCol` | `Match(q, opts...)` (`@@ websearch_to_tsquery(…)`), `Rank(q, opts...)` (`ts_rank`), `RankCD(q, opts...)` (`ts_rank_cd`); see [Full-text search](#full-text-search) |

Generated wrappers (`StatusCol`, `IdsIDCol`) take the named type in `Eq`, `Ne`, `Gt`, `Gte`, `Lt`,
`Lte`, `IsDistinctFrom`, `Between`, `To`, and for string, integer and scanner kinds `In` and
`NotIn`; the base type's other methods stay available. `In` binds one array parameter
(`= ANY($1)`), so the statement text does not depend on the number of values. `Coalesce(v)` of a
string, integer, float, bool, time or jsonb column returns the typed expression of its kind (a
`keel.StringExpr`, `keel.Int64Expr`, …), and `JSONBCol.Text(...)` a `StringExpr`; see
[Functions](#functions). `EqCol` and `NeCol` compare with another expression, for joins and
correlated subqueries. `Col.Excluded()` is the value an upsert proposed for the column (`EXCLUDED.col`); it is
valid only inside `Conflict.Set` (see [Upsert](#upsert)) and fails the statement anywhere else.

Combinators: `keel.And(ps...)` and `keel.Or(ps...)` skip nil predicates (`And` of nothing is
`TRUE`, `Or` of nothing `FALSE`), `keel.Not(p)`, `keel.True()`. `Where` joins its arguments with
`AND`.

### Functions

SQL functions are typed like columns. Each value kind has an expression type that compares to Go
values of its kind, as its columns do: `keel.StringExpr`, `Int64Expr`, `Int32Expr`, `Float64Expr`,
`NumericExpr`, `BoolExpr` (a predicate), `TimeExpr` and `JSONBExpr`. Functions return the expression
type of their result, so `s.ProNumber.Lower().Eq(1)` does not compile, and a function's arguments
are value interfaces (`keel.StringValue`, `Int64Value`, `Int32Value`, `Float64Value`,
`NumericValue`, `NumberValue`, `BoolValue`, `TimeValue`, `JSONBValue`) that the kind's columns and
expressions implement.

```go
u := tenantdb.Users
taken, err := u.AllTenants().Where(u.EmailAddress.Lower().EqExpr(keel.Text(email).Lower())).Exists(ctx, db)

w := workerdb.Workers
fullName := keel.ConcatWS(" ", w.FirstName, w.LastName)
rows, err := w.Pick(w.ID, fullName).Where(fullName.ContainsFold(term)).All(ctx, db)

th := conversationdb.Threads
lastActive := th.LastMessageAt.Greatest(th.CreatedAt) // an Int64Expr
live := keel.Or(ura.ExpiresAt.IsNull(), keel.NowEpoch().LtExpr(ura.ExpiresAt))
day := keel.ToTimestamp(f.CreatedAt).AtTimeZone(keel.Text(zone)).ToChar("YYYY-MM-DD")
```

Every expression type has `Eq`, `Ne`, `Gt`, `Gte`, `Lt`, `Lte`, `Between`, `IsDistinctFrom`,
`IsNotDistinctFrom` with a Go value (`In` and `NotIn` too for text and integers), `EqExpr` …
`LteExpr`, `IsDistinctFromExpr` and `IsNotDistinctFromExpr` against another value of its kind,
`IsNull`, `IsNotNull`, `Asc`, `Desc`, `Coalesce(v)` (a value or an expression), and `Min`, `Max`
(`Sum`, `Avg` for numbers) as aggregates; the typed columns have the same `IsNotDistinctFrom` and
`…Expr` tests. Go values are bound; a value whose type nothing in the statement fixes is bound
under a cast. `keel.Text(s)`, `Int64(n)`, `Int32(n)`, `Float64(f)`, `Numeric(v)`, `Bool(b)`,
`Time(t)`, `JSONB(v)` and `Interval("1 day")`, `Duration(d)` bind a value as an expression of its
kind (`$1::text`, `$1::bigint`, …, `JSONB` encoded with the configured codec), where a function
takes a value interface. `keel.StringOf(e)`, `Int64Of`, `Int32Of`, `Float64Of`, `NumericOf`,
`BoolOf`, `TimeOf`, `JSONBOf` and `IntervalOf` type an expression keel cannot type itself (a `Raw`,
a `Scalar`, a `DynExpr`, a `Rows` or CTE column) without changing the SQL; `keel.CastText(e)`,
`CastInt64(e)`, `CastInt32(e)`, `CastFloat64(e)`, `CastNumeric(e)`, `CastBool(e)`, `CastTime(e)`,
`CastInterval(e)` and `CastJSONB(e)` convert any expression with `CAST(e AS type)`: a number of
another kind, text that holds a number, a boolean or a document (`keel.CastBool(v.Snapshot.Text("enabled"))`),
json as jsonb.

| Kind | Functions |
|---|---|
| text (string columns, `StringExpr`) | `Lower`, `Upper`, `Trim` (`btrim`), `TrimChars(chars)`, `LTrim`, `LTrimChars`, `RTrim`, `RTrimChars`, `Length` (an `Int32Expr`), `Left(n)`, `Right(n)`, `Substr(start, count)`, `SubstrFrom(start)`, `Replace(from, to)`, `SplitPart(delim, n)`, `MD5`, `RegexpReplace(pattern, replacement, flags)`, `NullIf(v)`, `Greatest`, `Least`; a column that holds an enum, or declares a SQL type that is not text, is cast to text inside these, since Postgres's text functions do not take a Postgres enum |
| bigint, integer | `Abs`, `NullIf(v)`, `Greatest`, `Least` |
| double precision | `Abs`, `Round` (to a whole number), `Floor`, `Ceil`, `Ln`, `Exp`, `NullIf`, `Greatest`, `Least` |
| numeric | as double precision, and `RoundTo(places)`: `keel.CastNumeric(miles).RoundTo(2)` |
| time | `ToChar(format)`, `AtTimeZone(zone)` (`timezone(zone, t)`), `Greatest`, `Least`; `Add(interval)`, `Sub(interval)`, `Since(t)` (see [Typed operators](#typed-operators)) |
| jsonb | `TypeOf` (`jsonb_typeof`), `Set(path, v)` (`jsonb_set`), `Concat(o)` (`||`), `ArrayElementsText`, and on `JSONBExpr` `Contains`, `ContainsExpr`, `HasKey`, `Text(path...)` |
| arrays | `Len` (`cardinality`), `Unnest`, `At(i)`, `Without(v)` (`array_remove`), the assignments `Append(v)` and `Remove(v)`; see [Typed arrays](#typed-arrays) |

Package functions, whose result kind does not follow one argument:

| Function | SQL |
|---|---|
| `keel.Concat(args...)`, `keel.ConcatWS(sep, args...)` | `concat`, `concat_ws`; an argument is any expression or a Go value bound under the cast its type implies |
| `keel.Now()`, `keel.NowEpoch()`, `keel.Epoch(t)` | `now()`; `CAST(extract(epoch FROM t) AS bigint)`, Unix seconds for models that store times as epochs |
| `keel.ToTimestamp(seconds)` | `to_timestamp(seconds)` |
| `keel.DatePart(field, t)`, `keel.DateTrunc(field, t)` | `date_part('hour', t)` (a `Float64Expr`), `date_trunc('month', t)`; the field is checked against Postgres's and written as a constant, so an expression index on it matches |
| `keel.BoolOr(p)`, `keel.BoolAnd(p)`, `keel.JSONBAgg(e)` | the aggregates, as `keel.Agg` values with `Filter`, `OrderBy` and `Over` |
| `keel.JSONBuildObject(keel.Pair(key, v), ...)`, `keel.JSONBuildArray(vs...)` | `jsonb_build_object($1::text, v, …)`, `jsonb_build_array(…)` |
| `keel.ArrayPosition(list, v)` | `array_position($1::text[], v)`: `Order(keel.ArrayPosition(ids, q.ID).Asc())` keeps a caller's order |
| `keel.WidthBucket(v, bounds)`, `keel.WidthBucketRange(v, lo, hi, n)` | `width_bucket(v, $1::bigint[])`, `width_bucket(v, lo, hi, n)` |
| `keel.GenerateSeries(start, stop)`, `GenerateSeriesBy(start, stop, step)`, `GenerateTimeSeries(start, stop, "1 day")` | `generate_series`, for `keel.Rows` |
| `keel.Int8Range(lo, hi)` | `int8range(lo, hi)`, with `Covers(v)` (`@>`) and `Overlaps` (`&&`) |
| `keel.GenRandomUUID()` | `CAST(gen_random_uuid() AS text)` |

An expression written in two places of a statement (in `Pick` and `Group`) binds its values once,
so Postgres sees one expression: keep it in a variable (`day := …; q.Pick(day, keel.Count()).Group(day)`).
`keel.DynFunc` stays for dynamic queries, whose functions are chosen at run time.

#### Typed operators

Arithmetic, concatenation and collation are methods of the columns and expression types, take the
value interface of their kind and return the expression type of their result. They render
parenthesised, as dynamic expressions do, so they nest at any precedence:

```go
inv := invoicedb.Invoices
open := inv.TotalAmountMinor.Sub(inv.AppliedAmountMinor)              // ("total_amount_minor" - "applied_amount_minor")
daysPastDue := keel.Int64(asOf).Sub(inv.DueDate).Div(keel.Int64(86400)) // (($1::bigint - "due_date") / $2::bigint)
q.Where(inv.TotalAmount.Sub(expected).Abs().Gt(tolerance))               // numeric columns (AnyCol) compute as NumericExpr
deadline := item.FundedOn.Add(keel.CastInt64(agr.ChargebackWindowDays).Mul(keel.Int64(86400)))
stale := keel.Now().Since(c.LastSeenAt).GtExpr(keel.Interval("30 days"))
label := keel.Text("MC ").Cat(c.MCNumber.NullIf(""))                   // NULL when the number is empty
q.Order(st.ID.Collate("C").Asc())
```

| Operator | Kinds | SQL |
|---|---|---|
| `Add(v)`, `Sub(v)`, `Mul(v)`, `Div(v)`, `Neg()` | bigint, integer, double precision, numeric (columns and expressions) | `(a + b)`, `(a - b)`, `(a * b)`, `(a / b)`, `(-a)` |
| `Mod(v)` | bigint, integer, numeric | `(a % b)` |
| `Add(i)`, `Sub(i)` | time, with an `IntervalValue` | `(t + i)`, `(t - i)` |
| `Since(u)` | time | `(t - u)`, an `IntervalExpr` |
| `Add`, `Sub`, `Mul(n)`, `Div(n)`, `Neg`, `Seconds()` | interval | interval arithmetic; `Seconds` is `date_part('epoch', i)`, a `Float64Expr` |
| `Cat(vs...)` | text | `((a \|\| b) \|\| c)` |
| `Collate(name)` | text | `(a COLLATE "name")`; the name is quoted as one identifier and must be non-empty, at most 63 bytes, without double quotes or control characters |
| `InArray(a)`, `NotInArray(a)` | text, bigint, integer, double precision, numeric, time | `(e = ANY(a))`, `(e <> ALL(a))` over an array value |

Kinds do not mix, as Postgres's operators would silently widen them: an integer meets a bigint
through `keel.CastInt64(col)`, any number meets a numeric through `keel.CastNumeric(v)` (or
`keel.NumericOf(e)` when the SQL already yields numeric), and an integer meets a double through
`keel.CastFloat64`. Integer division truncates; `Div` of a value that may be zero takes
`NullIf(0)` on the divisor. On a column, `Add` is the expression; the assignment `col = col + v` is
`col.Increment(v)`, and `col.ToExpr(col.Mul(keel.Int64(2)))` assigns any arithmetic.

`Cat` is SQL's `||`: a NULL operand makes the whole result NULL. `keel.Concat` and `keel.ConcatWS`
call `concat()` and `concat_ws()`, which skip NULL arguments; pick `Cat` where an absent part should
blank the result and `Concat` where it should drop out. jsonb's `||` is `JSONBExpr.Concat`.

#### Typed CASE

`keel.CaseString()`, `CaseInt64()`, `CaseInt32()`, `CaseFloat64()`, `CaseNumeric()`, `CaseBool()`,
`CaseTime()`, `CaseJSONB()` and `CaseInterval()` start a CASE whose result kind is fixed: `When(cond,
v)` takes a value of that kind, and `Else(v)` or `End()` returns the kind's expression type, so the
CASE compares, sums and nests like any value of it.

```go
bucket := keel.CaseString().
    When(days.Lte(30), keel.Text("current")).
    When(days.Lte(60), keel.Text("31-60")).
    Else(keel.Text("over 60"))
miles := keel.CaseNumeric().
    When(units.Eq(shipment.UnitsKilometers), distance.Div(keel.Numeric(kilometersPerMile))).
    Else(distance)
q.Pick(bucket, miles.Sum()).Group(bucket)
```

The SQL is `CASE WHEN … THEN … [ELSE …] END`. A nil condition skips its `When`, as `Where` skips a
nil predicate (`keel.When(flag, p)`), and a CASE with no `When` left is its `Else` value (or NULL);
a nil value is NULL. Each builder is a small value: `When` copies, so a builder shared by two code
paths stays as it was.

#### Row comparisons

`keel.Tuple(vs...)` is the row value `ROW(a, b, …)`: an element is an expression, nil (NULL) or a Go
value bound as a parameter typed by the element it meets. `Eq`, `Ne`, `Gt`, `Gte`, `Lt`, `Lte`,
`IsDistinctFrom` and `IsNotDistinctFrom` compare two rows element by element, which is the keyset
condition an index on the same columns serves as one range scan:

```go
q.Where(keel.Tuple(e.OccurredAt, e.ID).Gt(keel.Tuple(after.OccurredAt, after.ID))). // ascending page
    Order(e.OccurredAt.Asc(), e.ID.Asc())
q.Where(keel.Tuple(fp.PurchasedAt, fp.ID).Lt(keel.Tuple(cur.PurchasedAt, cur.ID))) // descending
up.UpdateWhere(keel.Tuple(stored...).IsDistinctFrom(keel.Tuple(proposed...)))   // skip unchanged rows
q.Where(keel.Tuple(c.ItemKey, c.ContentHash).InUnnest(keel.TextArray(keys), keel.TextArray(hashes)))
```

`InUnnest(arrays...)` is `(row IN (SELECT * FROM unnest($1::text[], $2::text[])))`: the arrays zip
into rows, one parameter each, so the statement keeps one shape whatever the number of rows. A row
of fewer than two values fails the statement.

#### Typed arrays

Each element kind has an array value interface (`StringArrayValue`, `Int64ArrayValue`,
`Int32ArrayValue`, `Float64ArrayValue`, `NumericArrayValue`, `BoolArrayValue`, `TimeArrayValue`)
and an expression type (`StringArrayExpr`, …); `StringArrayCol` and `Int64ArrayCol` implement
theirs. Functions of Go values are named after the SQL type, functions of typed expressions after
the kind:

| Form | SQL |
|---|---|
| `keel.TextArray(ids)`, `Int64Array`, `Int32Array`, `Float64Array`, `NumericArray`, `BoolArray`, `TimeArray` | one parameter, `$1::text[]`, `$1::bigint[]`, …; a nil slice binds the empty array |
| `keel.ArrayOfString(es...)`, `ArrayOfInt64`, `ArrayOfInt32`, `ArrayOfFloat64`, `ArrayOfNumeric`, `ArrayOfBool`, `ArrayOfTime` | `ARRAY[e1, e2]`; no elements is `ARRAY[]::text[]` (`keel.ArrayOf(q)` is `ARRAY(subquery)`) |
| `keel.StringArrayOf(e)`, `Int64ArrayOf`, … | `e` typed as an array, as a CTE's array column: `parent.ID.InArray(keel.StringArrayOf(closure.Col("parent_role_ids")))` |
| `a.At(i)` | `(a)[i]`, from 1, NULL past either end, of the element's kind |
| `a.Len()`, `a.Unnest()` | `cardinality(a)` (an `Int32Expr`), the set-returning `unnest(a)` for `keel.Rows` |
| `a.Without(v)` | `array_remove(a, v)`, an expression (the column assignment stays `Remove(v)`) |
| `a.ContainsAllExpr(o)`, `ContainedByExpr(o)`, `OverlapsExpr(o)` | `(a @> o)`, `(a <@ o)`, `(a && o)` |
| `a.Coalesce(o)`, `IsNull`, `IsNotNull` | `coalesce(a, o)`, the NULL tests |
| `e.InArray(a)`, `e.NotInArray(a)` | `(e = ANY(a))`, `(e <> ALL(a))` |

```go
counts := keel.ArrayOfInt64(keel.Int64Of(keel.Count().Filter(late)), keel.Int64Of(keel.Count().Filter(held)))
q.Pick(counts) // one bigint[] of filtered totals in one row
```

#### Typed NULLs, nullable values and constants

| Form | SQL |
|---|---|
| `keel.NullText()`, `NullInt64()`, `NullInt32()`, `NullFloat64()`, `NullNumeric()`, `NullBool()`, `NullTime()`, `NullJSONB()`, `NullInterval()` | `NULL::text`, `NULL::bigint`, …: a NULL whose type nothing else fixes (a select list output, a union arm, a CASE branch) |
| `keel.TextPtr(p)`, `Int64Ptr`, `Int32Ptr`, `Float64Ptr`, `BoolPtr`, `TimePtr` | `$1::bigint` binding `*p`, or NULL when `p` is nil |
| `keel.ConstText(s)`, `ConstInt64(n)`, `ConstInt32(n)`, `ConstBool(b)` | the value written into the SQL: `'it''s'` (single quotes doubled, a NUL byte fails), `768`, `(-1)`, `TRUE` |
| `keel.ConstFloat64(f)`, `ConstNumeric("0.035")` | `'0.5'::double precision`, `'0.035'::numeric` (a non-finite float or a string that is not a decimal fails) |

A constant is for an expression that must match an expression index, a partial index predicate or
an `ON CONFLICT` target, which Postgres proves only against constants it sees: `coalesce(col, '')`
in an index needs `col.Coalesce(keel.ConstText(""))`, and `dimensions = 768` in a partial index
needs `e.Dimensions.EqCol(keel.ConstInt64(768))`. A text constant is of unknown type until it meets
a value, as one in an index definition is, and an integer constant is `integer` when it fits, so
both resolve as the index's own. Each distinct constant is a distinct statement: values from users
are bound.

#### From dynamic expressions to typed ones

Every `keel.Dyn*` form used inside a typed query has a typed counterpart that renders the same SQL
(the same operators, casts and NULL handling; a typed comparison leaves out the parentheses a
dynamic one adds):

| Dynamic | Typed |
|---|---|
| `keel.DynOf(a).Add(b)`, `.Sub`, `.Mul`, `.Div`, `.Mod` | `a.Add(b)`, … with `b` a column, an expression or `keel.Int64(n)` (`keel.DynOf(a).Div(86400)` is `a.Div(keel.Int64(86400))`) |
| `keel.DynLit(asOf).Sub(inv.DueDate)` | `keel.Int64(asOf).Sub(inv.DueDate)` |
| `keel.DynOf(a).Neg()` | `a.Neg()` |
| `keel.DynOf(n).Cast(keel.TypeInt64).Mul(86400)` | `keel.CastInt64(n).Mul(keel.Int64(86400))` |
| `keel.DynOf(x).Cast(keel.TypeNumeric)`, `TypeFloat64`, `TypeBool`, `TypeJSON`, `TypeString` | `keel.CastNumeric(x)`, `CastFloat64`, `CastBool`, `CastJSONB`, `CastText` |
| `keel.DynLit(v).Cast(keel.TypeString)`, `TypeInt64` | `keel.Text(v)`, `keel.Int64(v)` |
| `keel.DynLit(nil).Cast(keel.TypeString)` | `keel.NullText()` |
| `keel.DynLit(p)` with `p` a `*int64` | `keel.Int64Ptr(p)` |
| `keel.DynLit([]string{})` | `keel.TextArray(nil)` |
| `keel.DynLit("MC ").Concat(x)` | `keel.Text("MC ").Cat(x)` |
| `keel.DynOf(id).Collate("C")` | `id.Collate("C")` |
| `keel.DynCase().When(p, v).Else(w)` | `keel.CaseString()` (or the kind of `v`) `.When(p, v).Else(w)` |
| `keel.DynRow(a, b).Gt(keel.DynRow(x, y))`, `.IsDistinctFrom`, `.InUnnest(xs, ys)` | `keel.Tuple(a, b).Gt(keel.Tuple(x, y))`, `.IsDistinctFrom`, `.InUnnest(keel.TextArray(xs), keel.Int64Array(ys))` |
| `keel.DynArray(e1, e2)` | `keel.ArrayOfInt64(e1, e2)` (the kind of the elements) |
| `keel.DynOf(id).In(cte.Col("ids"))` | `id.InArray(keel.StringArrayOf(cte.Col("ids")))` |
| `keel.DynOf(a).Covers(b)`, `CoveredBy(b)` on arrays | `a.ContainsAllExpr(b)`, `ContainedByExpr(b)` |
| `keel.DynOf(a).At(1)` | `a.At(1)` |
| `keel.DynOf(a).IsDistinctFrom(b)` | `a.IsDistinctFromExpr(b)` |
| `keel.DynConst("")`, `DynConst(768)`, `DynConst(true)`; `col.EqCol(keel.DynConst(v))` | `keel.ConstText("")`, `ConstInt64(768)`, `ConstBool(true)`; `col.EqCol(keel.ConstText(v))` |
| `keel.DynFunc("array_remove", a, v)` | `a.Without(v)` |

#### Declared database functions

A function of the application's schema is declared once, with its argument and result types, and
called like keel's own:

```go
var (
    slugify    = keel.DeclareFunc1[keel.StringExpr, keel.StringValue]("public.slugify")
    nextNumber = keel.DeclareFunc2[keel.Int64Expr, keel.StringValue, keel.Int32Value]("billing.next_number")
)

c.Where(slugify.Call(c.Name).Eq("acme-freight"))
c.Pick(nextNumber.Call(keel.Text("INV"), keel.Int32(6)))
```

`DeclareFunc0[R]` … `DeclareFunc3[R, A, B, C]` take the name, `fn` or `schema.fn` (each part
quoted as given), and panic on a malformed one, since declarations are package variables. `R` is
one of the expression types; each argument type is a value interface or an expression type.
`keel.VerifyFuncs(ctx, db, decls...)` checks the declarations against the catalog (see
[Verifying the schema](#verifying-the-schema)).

### Full-text search

A `tsvector` column (`TSVectorCol`) is searched with `Match` and ranked with `Rank` and `RankCD`. The
search string is always a bound parameter, so user input goes in as it is typed:

```go
n := fleetdb.Notes // SearchVector string `keel:"search_vector,noselect,readonly,tsvector=english"`
q := "dallas reefer"
rows, err := n.
    Compute(n.Rank, n.SearchVector.Rank(q)).
    Where(n.SearchVector.Match(q)).
    Order(n.Rank.Desc()).
    Limit(20).
    List(ctx, db)
// … WHERE n."search_vector" @@ websearch_to_tsquery('english'::regconfig, $3)
//   ORDER BY (ts_rank(n."search_vector", websearch_to_tsquery('english'::regconfig, $3))) DESC
```

Options shape the query:

| Option | Query |
|---|---|
| none | `websearch_to_tsquery(config, q)`: words ANDed, `"quoted phrase"`, `or`, `-excluded` |
| `keel.SearchModes(keel.Plain)` | `plainto_tsquery(config, q)`: the words ANDed, operators ignored |
| `keel.SearchModes(keel.Phrase)` | `phraseto_tsquery(config, q)`: the words next to each other, in order |
| `keel.SearchModes(keel.Prefix)` | `to_tsquery(config, 'dal':* & 'ree':*)`: every word as a prefix, for search as you type |
| `keel.SearchModes(keel.AnyWord)` | `to_tsquery(config, 'dallas' \| 'reefer')`: any of the words |
| `keel.SearchModes(keel.AnyPrefix)` | `to_tsquery(config, 'dal':* \| 'ree':*)`: any word as a prefix |
| `keel.SearchModes(keel.AnyWebSearch)` | `to_tsquery(config, ('dallas' \| ('dry' <-> 'van')) & !'frozen')`: any word or `"quoted phrase"`, without any `-excluded` word |
| `keel.SearchModes(keel.TSQuery)` | `to_tsquery(config, q)`: `q` is tsquery syntax the code built from cleaned words; malformed syntax fails the statement, so never pass user input |
| `keel.SearchAlso(mode, text)` | another term, `text` read with `mode`, ORed into the query: a websearch of the whole string plus a `TSQuery` of the prefixes the code chose |
| `keel.SearchModes(a, b, …)` | the queries of each mode ORed: `(websearch_to_tsquery(…) \|\| to_tsquery(…))` |
| `keel.SearchConfig("simple", "english")` | the queries built with each configuration ORed, so a word matches stemmed or not |
| `keel.RankNormalization(32)` | `ts_rank(vector, query, 32)`: the normalization bit mask of `Rank` and `RankCD` |

The configuration is the column's `tsvector=<config>` unless `SearchConfig` names others, and the
database's `default_text_search_config` when neither does. Names must be identifiers (optionally
`schema.name`); they are written into the statement as `'name'::regconfig`, and Postgres refuses a
configuration it does not have. The word modes split the string into runs of letters and digits,
so punctuation and tsquery operators in user input are only separators; a string without words
makes a query that matches nothing (`NULL::tsquery`). `Rank` and `RankCD` return a `real`, usable
in `Order`, in `Pick` and to fill a computed field; a search string written twice in one statement
(in `Match` and `Rank`) binds once. A GIN index on the vector serves every mode.

The dynamic layer has the same methods on `DynExpr`: `q.Path("search_vector").Match(q, opts...)`,
`.Rank(q, opts...)` and `.RankCD(q, opts...)`.

Trigram similarity (`pg_trgm`'s `similarity()` and `%`) has no typed operator: a trigram GIN index
serves `ContainsFold` and `ILike`, and `keel.Raw("similarity(?, ?)", col, text)` reaches the
functions where a ranking needs them.

### Optional filters

`keel.When(cond, p)` returns `p` when `cond` holds and nil otherwise. `Where` and the combinators
skip nil, so each optional filter is one expression:

```go
rows, err := s.Where(
    keel.When(f.Search != "", s.ProNumber.ContainsFold(f.Search)),
    keel.When(len(f.Statuses) > 0, s.Status.In(f.Statuses...)),
    keel.When(f.CustomerID != "", s.CustomerID.Eq(f.CustomerID)),
    keel.When(f.HazmatOnly, s.Hazmat.IsTrue()),
).Order(s.CreatedAt.Desc()).Limit(50).List(ctx, db)
```

The predicate is built even when `cond` is false; building one only allocates.

### Raw SQL

Build computed values with the expressions of [Expressions](#expressions) first: `keel.DynOf(e)`
wraps a typed column or aggregate, and its methods, `keel.DynFunc`, `keel.DynCase`, `keel.DynRow`
and `Cast` cover coalesce, arithmetic, comparisons between expressions, casts, CASE, row
comparisons and the common functions in typed queries as well as dynamic ones:

```go
n, err := s.Where(
    keel.DynFunc("length", s.ProNumber).Gt(8),
    keel.DynOf(s.Weight).Coalesce(0).Add(500).Lt(20000),
).Count(ctx, db)
```

`keel.Raw(sql, args...)` is SQL text keel does not understand. Each `?` is replaced by the next
argument: an expression (a column, a subquery) is written in place with its alias resolved, any
other value is bound as a parameter. `??` is a literal question mark. The result is both an
expression and a predicate.

```go
n, err := s.Where(
    keel.Raw("length(?) > ?", s.ProNumber, 8),
    keel.Raw("? ?? 'trailer'", s.Attributes), // jsonb ? operator
).Count(ctx, db)
```

Never build the SQL text from values: pass them as arguments, so the statement stays one prepared
shape and cannot be injected. `keel.Val(v)` binds a value where an expression is expected.

### Relation path filters, Any and None

A relation method returns the handle of the related table on that path. Columns of a belongs-to or
has-one path join the relation automatically (`LEFT JOIN` in reads, `FROM`/`USING` in bulk writes,
with the tenant columns in the join). On a to-many path, `Any` and `None` compile to `EXISTS` and
`NOT EXISTS`; a to-many column outside them is an error.

```go
m := s.Moves()
st := m.Stops()

rows, err := s.Where(
    s.Customer().Code.Eq("ACME"),
    keel.Or(s.BillToID.IsNull(), s.BillTo().Code.Ne("ACME")),
    m.Any(m.Status.Eq(fleet.StatusInTransit), m.Loaded.IsTrue()),
    m.Any(st.Any(st.Kind.Eq("delivery"), st.Location().City.Eq("Dallas"))),
    s.Tags().None(s.Tags().Name.Eq("on-hold")),
).Limit(100).List(ctx, db)
```

Each path gets its own join: `s.Customer()` and `s.BillTo()` both read `customers`, under the
aliases `c` and `bt`. A to-many path under another to-many path is filtered inside the outer
`Any`, as `st` is above. `Any` and `None` with no predicates test whether any row exists. A query
started from a to-many path has `Any` and `None` too, so `m.Where(m.Loaded.IsTrue()).Any()` works.

Use a path handle (`s.Customer().Code`), not the base handle (`fleetdb.Customers.Code`), to filter
on a relation: a base handle column must belong to a table of the statement.

### Column paths from names

An API that lets clients filter and sort by field names (`?sort=customer.name&status=New`) builds
a typed query from names chosen at run time. `keel.ResolvePath(handle, segments...)` resolves them
on a handle, a query or an alias: the last segment names a column (its SQL name or Go field name),
the ones before it to-one relations (their Go name or its snake_case form), exactly as a dynamic
query's `Path` reads them.

```go
name := keel.ResolvePath(s, "customer", "name") // the column s.Customer().Name
rows, err := s.Where(name.ContainsFold(search), keel.ResolvePath(s, "status").In(statuses)).
    Order(keel.ResolvePath(s, sortField...).Desc()).
    Load(s.Customer()).
    Page(ctx, db, keel.After(cursor, 50))
```

The result is a `DynExpr`, so it has the dynamic operators (`Eq`, `In`, `ContainsFold`, `IsNull`,
`Asc`, …), and it is a column of the same relation path the generated handles build: it and
`s.Customer().Name`, or a `Load(s.Customer())`, share one join, with its tenant condition. It works
in `Where`, `Order` (keyset pages included), `Pick`, and inside `Any` and `None` when resolved on
the to-many handle (`keel.ResolvePath(m, "sequence")` with `m := s.Moves()`). A name that does not
resolve, or a path through a to-many relation, makes an expression that carries the error
(`Err()`), which the query's terminal returns. Do not use a dynamic query's `Path` inside a typed
query: its columns belong to the dynamic query's own table reference.

### Joins and self-joins

`Join` and `LeftJoin` add a table on predicates; the joined table's tenant columns are compared
with the scope in the `ON` clause. `As(name)` gives a second reference to a table, for self-joins
and correlated subqueries. Alias names must match `[a-z_][a-z0-9_]*` and are memoised, so use
constants.

```go
w := fleetdb.Workers
mgr := w.As("mgr")
reports, err := w.
    Join(mgr, mgr.ID.EqCol(w.ManagerID)).
    Where(mgr.LastName.Eq("Hopper")).
    Order(w.LastName.Asc()).
    All(ctx, db)

s, c := fleetdb.Shipments, fleetdb.Customers
rows, err := s.
    LeftJoin(c, c.ID.EqCol(s.BillToID)).
    Where(keel.Or(c.ID.IsNull(), c.Email.IsNotNull())).
    Limit(100).
    List(ctx, db)
```

Rows still decode into the query's model; to read the joined table's columns, pick them and use
`keel.Into`. Every table in a statement gets a short alias derived from its name (`shipments` →
`s`, `shipment_tags` → `st`) or its relation (`BillTo` → `bt`), numbered when a statement uses it
twice (`s2`).

### Subqueries

Any query is a subquery. `keel.Exists` and `keel.NotExists` wrap one; `InQuery` and `NotInQuery`
compare a column with a query that picks one column. A subquery that references the outer table's
columns is correlated.

```go
c, s := fleetdb.Customers, fleetdb.Shipments
cutoff := time.Now().AddDate(0, -3, 0).Unix()

idle, err := c.Where(keel.NotExists(
    s.Where(s.CustomerID.EqCol(c.ID), s.CreatedAt.Gte(cutoff)),
)).All(ctx, db)

hazmatShippers, err := c.Where(
    c.ID.InQuery(s.Pick(s.CustomerID).Where(s.Hazmat.IsTrue())),
).Count(ctx, db)
```

Subqueries are scoped like any statement: the tenant predicate is added to each.

`keel.Scalar(q)` is the value of a query that picks one expression and returns at most one row,
`(SELECT …)`, usable wherever an expression is: in `Pick`, `Where`, `Order`, and to fill a computed
field (below).

```go
loads := keel.Scalar(s.Pick(keel.Count()).Where(s.CustomerID.EqCol(c.ID)))
busy, err := c.Where(keel.DynOf(loads).Gt(10)).All(ctx, db)
```

### Lateral joins

`LeftJoinLateral(sub)` adds `LEFT JOIN LATERAL (sub) ON TRUE`; `JoinLateral(sub)` adds
`CROSS JOIN LATERAL (sub)`. Columns of the subquery's table, referenced outside it, read the
subquery's output. Use them for "the latest row per parent" without a window function:

```go
m := fleetdb.Moves
last := fleetdb.Stops.As("last_stop")

type moveEnd struct {
    Move     ids.ID
    Location ids.ID
    Sequence int32
}
rows, err := keel.Into[moveEnd](ctx, db, m.
    LeftJoinLateral(last.
        Pick(last.LocationID, last.Sequence).
        Where(last.MoveID.EqCol(m.ID)).
        Order(last.Sequence.Desc()).
        Limit(1)).
    Pick(m.ID, last.LocationID, last.Sequence).
    Where(m.ShipmentID.Eq("shp_01")))
```

`keel.Rows(name, columns, exprs...)` is a lateral source computed by expressions, usually the
set-returning `unnest` or `generate_series`: `(SELECT expr AS "col", …) name`. Several set-returning
functions zip, row i of each making row i. Each expression may read the row it joins:

```go
s := fleetdb.Shipments
lbl := keel.Rows("lbl", []string{"label"}, keel.DynFunc("unnest", s.Labels))
rows, err := keel.Into[labelRow](ctx, db, s.LeftJoinLateral(lbl).Pick(s.ProNumber, lbl.Col("label")))
```

### Common table expressions

`keel.CTE(name, columns, body)` declares `WITH name (columns) AS (body)`. Declare it on the query
with `With`, then join it, filter on its columns with `Col(name)` (an `AnyCol`), or read it through
`Query()`, which returns a `*keel.Query[*keel.Row]` to read with `keel.Into`. A query over the CTE
declares it too.

```go
s, c := fleetdb.Shipments, fleetdb.Customers

busy := keel.CTE("busy", []string{"customer_id", "loads"},
    s.Group(s.CustomerID).Pick(s.CustomerID, keel.Count()).Where(s.Status.Eq(fleet.StatusInTransit)))

customers, err := c.With(busy).
    Join(busy, busy.Col("customer_id").EqCol(c.ID)).
    Where(busy.Col("loads").Gte(10)).
    Order(c.Name.Asc()).
    All(ctx, db)

type load struct {
    Customer ids.ID
    Loads    int64
}
top, err := keel.Into[load](ctx, db, busy.Query().With(busy).
    Pick(busy.Col("customer_id"), busy.Col("loads")).
    Order(busy.Col("loads").Desc()).
    Limit(5))
```

`Col` panics for a name that is not one of the CTE's columns.

### Recursive CTEs

`keel.RecursiveCTE(name, columns, base)` declares `WITH RECURSIVE name (columns) AS (base UNION ALL
step)`; `Recurse(step)` sets the step, which joins the CTE itself to walk a hierarchy. `Distinct()`
makes it `UNION`, so repeated rows end the recursion.

```go
w := fleetdb.Workers

chain := keel.RecursiveCTE("chain", []string{"id", "depth"},
    w.Pick(w.ID, keel.DynLit(0).Cast(keel.TypeInt64)).Where(w.ID.Eq("wrk_dispatch_lead")))
sub := w.As("sub")
chain.Recurse(sub.
    Pick(sub.ID, keel.DynOf(chain.Col("depth")).Add(1)).
    Join(chain, chain.Col("id").EqCol(sub.ManagerID)))

type node struct {
    ID    ids.ID
    Depth int32
}
tree, err := keel.Into[node](ctx, db, chain.Query().With(chain).
    Pick(chain.Col("id"), chain.Col("depth")).
    Order(chain.Col("depth").Asc()))

team, err := w.With(chain).Where(w.ID.InQuery(chain.Query().Pick(chain.Col("id")))).All(ctx, db)
```

### Set operations

`keel.Union`, `keel.UnionAll`, `keel.Intersect` and `keel.Except` combine queries that pick the same
number of expressions into a `keel.Compound`. Read it with `keel.Into`, or use it as a subquery or a
CTE body:

```go
codes, err := keel.Into[string](ctx, db, keel.UnionAll(
    s.Pick(s.ProNumber).Where(s.Hazmat.IsTrue()),
    c.Pick(c.Code).Where(c.Email.IsNull()),
))

flagged := keel.CTE("flagged", []string{"customer_id"}, keel.Union(
    s.Pick(s.CustomerID).Where(s.Hazmat.IsTrue()),
    s.Pick(s.CustomerID).Where(s.Status.Eq(fleet.StatusCanceled)),
))
n, err := c.With(flagged).
    Where(c.ID.InQuery(flagged.Query().Pick(flagged.Col("customer_id")))).
    Count(ctx, db)
```

### Projections and aggregates

`keel.Into[R](ctx, db, q)` runs a query that picks expressions and decodes each row into `R`: the
picked expressions fill `R`'s exported fields in order, so the counts must match. `R` may be a
scalar type when one expression is picked. `keel.IntoOne[R]` expects exactly one row
(`ErrNotFound`, `ErrNotSingular` otherwise).

```go
type statusTotal struct {
    Status  fleet.Status
    Loads   int64
    Hazmat  int64
    Revenue fleet.Money
}
rows, err := keel.Into[statusTotal](ctx, db, s.
    Group(s.Status).
    Pick(s.Status, keel.Count(), keel.Count().Filter(s.Hazmat.IsTrue()), s.Total.Sum()).
    Having(keel.Count().Gt(10)).
    Order(keel.Count().Desc()))

heaviest, err := keel.IntoOne[*int64](ctx, db, s.Pick(s.Weight.Max()).Where(s.Status.Eq(fleet.StatusNew)))
```

Aggregates and functions are `keel.Agg` values: `keel.Count()` (`count(*)`), `keel.CountOf(expr)`,
`keel.Sum(expr)`, `keel.Avg(expr)`, `keel.Min(expr)`, `keel.Max(expr)`, the window functions below,
and the column methods `Count`, `CountDistinct`, `Min`, `Max`, `Sum`, `Avg`, `Len` and `Rank`. An
`Agg` compares to values (`Eq`, `Ne`, `Gt`, `Gte`, `Lt`, `Lte`), orders (`Asc`, `Desc`), restricts its
rows with `Filter(pred)` (`FILTER (WHERE …)`), and becomes a window function with `Over` or
`OverWindow`. Use a pointer field for an aggregate that can be NULL (`max` over no rows).

### Computed fields

A field tagged `computed` is not a column of the table: a search rank, a count of related rows, a
name from a joined table. Writes never insert or update it, queries do not select it by default,
the bulk-insert column catalog skips it, and reports do not offer it. A query fills it with
`Compute(field, expr)`, which selects the expression into the field for the rows it returns:

```go
type Contact struct {
    keel.Table `keel:"contacts"`
    // …
    CustomerName string `keel:"customer_name,computed,nullzero"`
    Loads        int64  `keel:"loads,computed"`
}

c, s := fleetdb.Contacts, fleetdb.Shipments
rows, err := c.
    Compute(c.CustomerName, c.Customer().Name).
    Compute(c.Loads, keel.Scalar(s.Pick(keel.Count()).Where(s.CustomerID.EqCol(c.CustomerID)))).
    Where(c.Loads.Gt(0)).
    Order(c.Loads.Desc()).
    Limit(50).
    List(ctx, db)
```

- The field is filled only by queries that compute it; elsewhere it keeps its zero value. A field
  that can read NULL (a `LEFT JOIN`ed name, `max` over no rows) needs a pointer type or `nullzero`.
- Inside the query, the field stands for its expression: `Where`, `Order`, `Pick`, keyset `Page`s
  and `keel.Into` projections write the expression where the field is used. Using a computed field
  in a query that does not compute it fails the query.
- The expression's type must fit the field: keel knows the type of columns, string expressions,
  `Scalar` over one typed expression, counts and ranks (`bigint`), `ts_rank` (`real`), `sum` and
  `avg` (by their argument), `min` and `max` (their argument's type), and refuses an expression
  that does not fit (a `bigint` field takes integers, a `double precision` field any number, a
  scanner field such as a decimal any number). Expressions whose type keel cannot tell (`Raw`) are
  checked by the driver when rows decode.
- `Compute` works on loads too (`Load(m.Compute(m.Rank, …))`) and on the dynamic layer:
  `q.Compute("loads", expr)` makes `q.Path("loads")` read the expression, and a query without
  `Select` returns the field after the table's default columns.
- `Save`, bulk `Update`, `OnConflict` and `InsertFrom` refuse a computed field, and `keel.Needs`
  panics on one: it has no stored value.

### Grouping sets, rollups and cubes

Subtotals and grand totals come from the same statement, and the same snapshot, as the detail rows.
`Rollup(a, b)` groups by `(a, b)`, then `(a)`, then nothing; `Cube(a, b)` by every subset;
`GroupingSets(sets...)` by exactly the sets given, `nil` being the grand total. They mix with
`Group`: `Group(s.Hazmat).Rollup(s.Status)` groups by hazmat in every set. In a row of a set that
does not group by an expression, that expression is NULL.

`keel.Grouping(es...)` is `GROUPING(…)`: a bit mask with a 1 for each of `es` the row's set does not
group by, the leftmost expression being the highest bit. It tells a detail row (0) from a subtotal
or the total, in `Pick`, `Having` and `Order`. Pass the expressions exactly as they are grouped.

```go
code := s.Customer().Code
level := keel.Grouping(code, s.Status)
type line struct {
    Customer *string       // NULL on the grand total
    Status   *fleet.Status // NULL on subtotals and the total
    Loads    int64
    Revenue  fleet.Money
    Level    int32 // 0 detail, 1 customer subtotal, 3 grand total
}
rows, err := keel.Into[line](ctx, db, s.
    Pick(code, s.Status, keel.Count(), s.Total.Sum(), level).
    Rollup(code, s.Status).
    Order(level.Asc(), code.Asc(), s.Status.Asc()))

totals, err := keel.Into[line](ctx, db, s.
    Pick(code, s.Status, keel.Count(), s.Total.Sum(), level).
    GroupingSets([]keel.Expr{code}, []keel.Expr{s.Status}, nil).
    Having(level.Gt(0)))
```

`Count` on a grouped query counts the rows it returns, subtotals included.


### Window functions

A window function computes a value for each row from the rows around it, without collapsing them.
`Agg.Over(partition, order...)` is the short form; `Agg.OverWindow(w)` takes a `keel.Window` with a
partition, an order and a frame. Build one with `keel.PartitionBy(es...)`, `keel.OrderBy(os...)` or
`keel.NamedWindow(name)`; its methods return a changed copy, so a base window can be shared. The
zero `keel.Window{}` is `OVER ()`: every row of the result.

```go
type ranked struct {
    Customer ids.ID
    Pro      string
    Rank     int64
}
rows, err := keel.Into[ranked](ctx, db, s.
    Pick(s.CustomerID, s.ProNumber,
        keel.RowNumber().Over([]keel.Expr{s.CustomerID}, s.CreatedAt.Desc())).
    Where(s.Status.Eq(fleet.StatusDelivered)).
    Limit(500))
```

Frames bound the rows a function sees, relative to the current row:

| Frame | Offsets count |
|---|---|
| `w.Rows(start, end)` | rows |
| `w.Range(start, end)` | distance in the value of the single order expression (a number, or a `time.Duration` for a timestamp) |
| `w.Groups(start, end)` | peer groups (rows that tie in the order) |

Bounds are `keel.UnboundedPreceding`, `keel.Preceding(n)`, `keel.CurrentRow`, `keel.Following(n)`
and `keel.UnboundedFollowing`; offsets are bound as parameters. `w.Exclude(x)` removes
`keel.ExcludeCurrentRow`, `keel.ExcludeGroup` (the row and its peers) or `keel.ExcludeTies` (its
peers) from the frame.

```go
byDay := keel.PartitionBy(s.CustomerID).OrderBy(s.CreatedAt.Asc())
type trend struct {
    Pro     string
    Running fleet.Money  // running total per customer
    Moving  float64      // average of this and the two previous shipments
    Share   float64      // percent of all revenue
    Rank    int64        // rank by revenue within the customer
    Change  *fleet.Money // change from the previous shipment; NULL on the first
}
rows, err := keel.Into[trend](ctx, db, s.Pick(
    s.ProNumber,
    keel.Sum(s.Total).OverWindow(byDay.Rows(keel.UnboundedPreceding, keel.CurrentRow)),
    keel.Avg(s.Total).OverWindow(byDay.Rows(keel.Preceding(2), keel.CurrentRow)),
    keel.Raw("? * 100.0 / ?", s.Total, keel.Sum(s.Total).OverWindow(keel.Window{})),
    keel.Rank().OverWindow(keel.PartitionBy(s.CustomerID).OrderBy(s.Total.Desc())),
    keel.Raw("? - ?", s.Total, keel.Lag(s.Total, 1, nil).OverWindow(byDay)),
).Order(s.CustomerID.Asc(), s.CreatedAt.Asc()).Limit(1000))
```

Window functions: `RowNumber`, `Rank`, `DenseRank`, `PercentRank`, `CumeDist`, `Ntile(n)`,
`Lag(e, offset, default)`, `Lead(e, offset, default)`, `FirstValue(e)`, `LastValue(e)` and
`NthValue(e, n)`; every aggregate (`Sum`, `Avg`, `Min`, `Max`, `Count`, `CountOf`, the column
methods, with or without `Filter`) is a running or moving aggregate over a window. `LastValue` reads
the frame's last row, which by default is the current row's last peer: give it a frame that reaches
`UnboundedFollowing` to read the partition's last row. Windows apply after grouping, so a window
over an aggregate gives each group's share of the total:
`keel.Sum(s.Total.Sum()).OverWindow(keel.Window{})`.

`Query.Window(name, w)` declares a named window (`WINDOW name AS (…)`) that functions use with
`keel.NamedWindow(name)`; a function may add an order, when the named window has none, and a frame:

```go
q := s.Pick(s.ProNumber,
    keel.RowNumber().OverWindow(keel.NamedWindow("per_customer")),
    keel.Sum(s.Total).OverWindow(keel.NamedWindow("per_customer").Rows(keel.UnboundedPreceding, keel.CurrentRow))).
    Window("per_customer", keel.PartitionBy(s.CustomerID).OrderBy(s.CreatedAt.Asc()))
```

Window names match `[a-z_][a-z0-9_]*`.

### DISTINCT ON

`DistinctOn(es...)` keeps the first row of each group, in the query's order, which must start with
the same expressions:

```go
latest, err := s.
    DistinctOn(s.CustomerID).
    Order(s.CustomerID.Asc(), s.CreatedAt.Desc()).
    All(ctx, db)
```

### Ordering

`col.Asc()` and `col.Desc()` order by a column; `keel.Asc(expr)` and `keel.Desc(expr)` by any
expression, as do `Agg.Asc` and `StringExpr.Asc` (`s.ProNumber.Lower().Asc()`). `NullsFirst()` and
`NullsLast()` place NULLs: `s.Weight.Desc().NullsLast()`.

### Locks

`Lock(strength, wait...)` locks the selected rows of the query's own table (`FOR UPDATE OF s`);
joined relations are not locked. Strengths: `keel.ForUpdate`, `keel.ForNoKeyUpdate`,
`keel.ForShare`, `keel.ForKeyShare`. Waits: `keel.Wait` (the default), `keel.NoWait`,
`keel.SkipLocked`. Locks need a transaction to be useful:

```go
err := keel.Write(ctx, db, func(ctx context.Context) error {
    batch, err := s.
        Where(s.Status.Eq(fleet.StatusNew)).
        Order(s.CreatedAt.Asc()).
        Limit(20).
        Lock(keel.ForUpdate, keel.SkipLocked).
        List(ctx, db)
    if err != nil {
        return err
    }
    for _, sh := range batch {
        sh.Status = fleet.StatusAssigned
        if err := s.Save(ctx, db, sh, s.Status); err != nil {
            return err
        }
    }
    return nil
})
```

`NoWait` fails with SQLSTATE `55P03` (`keel.Code(err) == "55P03"`) when a row is locked.

### Plans and timeouts

`CustomPlan()` plans one query for its parameter values on every execution, even when the
connection would reuse a generic plan. Use it for queries whose best plan depends on the values,
such as a filter whose selectivity differs widely between tenants. keel wraps the statement in
transaction-local `set_config('plan_cache_mode', …)` calls in the same round trip; outside a
transaction it opens an implicit one for this.

```go
rows, err := s.
    Where(s.Status.Eq(fleet.StatusDelivered), s.CustomerID.Eq("cus_01")).
    CustomPlan().
    Order(s.CreatedAt.Desc()).
    Limit(100).
    List(ctx, db)
```

`Timeout(d)` bounds the terminal with a context deadline, relation loads included. When it passes,
pgx cancels the statement and the error matches `context.DeadlineExceeded`. Inside a transaction a
canceled statement aborts the transaction.

```go
n, err := s.Where(s.SearchVector.Match("reefer")).Timeout(2*time.Second).Count(ctx, db)
if errors.Is(err, context.DeadlineExceeded) {
    // the search took too long
}
```

pgx cancels a statement by sending a cancel request over a second connection, which can be lost or
arrive late. With `Config.DeadlineTimeouts` the server enforces the deadline too: every
transaction keel begins, explicit or implicit, sends
`SELECT set_config('statement_timeout', $1, true)` in its `BEGIN` round trip, with the time left
before the deadline (at least 1ms). An explicit transaction takes the deadline of the context given
to `Read` or `Write`; an implicit one the deadline of its terminal's context, `Timeout` included. A
request context with a deadline is therefore bounded on the server for every terminal when
`OnBegin` is set, since every terminal then runs in an implicit transaction. A statement outside any
transaction relies on the cancel request alone: `set_config(…, true)` lasts only for a
transaction. A statement the server stops fails with SQLSTATE `57014` (`query_canceled`).

### Debugging with ToSQL

`ToSQL(ctx, db)` renders the query's main statement as it would run under `ctx`'s scope, with its
arguments, for logs, tests and `EXPLAIN`. Relation loads that run as separate statements are not
included, nor is a page's keyset predicate.

```go
sql, args, err := s.
    Pick(s.ID, s.ProNumber).
    Where(s.Status.In(fleet.StatusNew, fleet.StatusAssigned), s.Customer().Code.Eq("ACME")).
    Order(s.CreatedAt.Desc()).
    Limit(20).
    ToSQL(ctx, db)
```

```sql
SELECT s."id", s."pro_number", s."organization_id", s."business_unit_id" FROM "shipments" s
LEFT JOIN "customers" c ON c."id" = s."customer_id" AND c."organization_id" = $1 AND c."business_unit_id" = $2
WHERE s."organization_id" = $1 AND s."business_unit_id" = $2 AND s."status" = ANY($3) AND c."code" = $4
ORDER BY s."created_at" DESC LIMIT 20
-- args: [org_acme bu_dallas [New Assigned] ACME]
```

(The statement is one line; it is wrapped here.) An observer sees every statement keel sends; see
[Observers](#observers).

### Shared parameters

An expression written twice in a statement binds its values once, so it renders identically
wherever it appears. Postgres matches GROUP BY, GROUPING and ORDER BY expressions by their text, and
`date_trunc($1, …)` in SELECT does not match `date_trunc($2, …)` in GROUP BY even when both
parameters hold `'month'`:

```go
month := keel.Raw("date_trunc(?, to_timestamp(?))", "month", s.CreatedAt)
rows, err := keel.Into[monthly](ctx, db, s.
    Pick(month, s.Total.Sum()).
    Group(month).
    Order(keel.Asc(month)))
```

```sql
SELECT date_trunc($3, to_timestamp(s."created_at")), sum(s."total") FROM "shipments" s
WHERE s."organization_id" = $1 AND s."business_unit_id" = $2
GROUP BY date_trunc($3, to_timestamp(s."created_at")) ORDER BY date_trunc($3, to_timestamp(s."created_at")) ASC
```

keel shares a parameter only between binds it knows get the same type, because Postgres infers a
parameter's type from its first use: one `$1` compared with a `date` column and a `timestamptz`
column fails with "inconsistent types deduced for parameter", and one compared with an enum and a
text column with "operator does not exist". So two binds share a placeholder when they are:

- the same value of the same expression value: one `Raw`, `Val`, JSON path, window offset or
  dynamic literal written in several places (`Pick(month).Group(month)`; two separately built `Raw`
  expressions keep their own placeholders, so build a repeated expression once and reuse it);
- equal values (strings, integers, booleans, times, named types of those, non-zero floats) compared
  with the same column through the same operator (`s.Status.Eq(x)` twice);
- equal values under the same explicit cast (`keel.DynLit(v).Cast(keel.TypeDate)` twice).

Writes keep one parameter per value. The lookup is a scan of at most 16 entries, then a map, both
reused across statements, so sharing costs no allocation.

### Explain

`q.Explain(ctx, db, opts)` returns the plan of a typed or dynamic query's main statement, rendered
as `ToSQL` renders it and run with its parameters, so the plan is the one Postgres picks for these
values: `TotalCost` (in the planner's cost units), `Rows` (the estimated row count) and `JSON`, the
whole `EXPLAIN (FORMAT JSON)` output. A guard can refuse a report whose cost or row estimate is over
a limit before running it.

```go
plan, err := s.Where(s.Status.Eq(fleet.StatusNew)).Order(s.CreatedAt.Desc()).Limit(50).
    Explain(ctx, db, keel.ExplainOptions{})
if err == nil && plan.TotalCost > 100_000 {
    // too expensive for an interactive request
}

analyzed, err := s.Where(s.Status.Eq(fleet.StatusNew)).Limit(50).
    Explain(ctx, db, keel.ExplainOptions{Analyze: true})
// analyzed.ActualRows, ActualTime, PlanningTime, ExecutionTime; buffer counts in analyzed.JSON
```

`Analyze` runs `EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON)`, which executes the statement, inside a
transaction that is rolled back: an implicit read-only one with the `OnBegin` statements, or,
when the context carries a transaction, a savepoint rolled back to in the same round trip, so the
statement sees the transaction's own writes and a failure leaves the transaction usable.

`GenericPlan` runs `EXPLAIN (GENERIC_PLAN)` (PostgreSQL 16) with the parameters left unbound: the
plan a prepared statement settles on after its first runs, which cannot use a value to reach a
partial index or an expression index. A predicate that needs `DynConst` to reach its index shows
here as the scan it really gets. `plan.Indexes()` and `plan.SeqScans()` list the indexes the plan
reads and the tables it scans sequentially; [`keeltest.UsesIndex`](#testing-an-application) asserts
on them, and the [plan guard](#plan-guard) checks the generic plan of every statement a test run
sent.

## Loading relations

`Load` takes relation paths and fills the relation fields of the returned rows:

```go
s := fleetdb.Shipments
m := s.Moves()
cu := s.Customer()

rows, err := s.
    Where(s.Status.Eq(fleet.StatusInTransit)).
    Load(
        cu.Pick(cu.Name, cu.Code),
        s.BillTo(),
        m.Where(m.Status.Ne(fleet.StatusCanceled)).
            Order(m.Sequence.Asc()).
            Load(m.Stops().Location()),
        s.Tags(),
    ).
    Order(s.CreatedAt.Desc()).
    Limit(50).
    List(ctx, db)
// rows[i].Customer, rows[i].BillTo (nil when bill_to_id is NULL), rows[i].Moves[j].Stops[k].Location, rows[i].Tags
```

How loads run:

- **Belongs-to and has-one** relations are joined into the parent's statement with `LEFT JOIN`.
  A nested to-one path (`m.Shipment().Customer()`) joins each level.
- **Has-many and many-to-many** relations run one statement per relation per level, filtered on
  the parents' keys plus the scope's tenant columns (by the tenant columns and keys together when
  the load crosses tenants; see [Rules](#rules)). By default every level goes in the read's own
  round trip, so `Load(s.Customer(), s.Moves().Stops())` costs one round trip for three
  statements: the shipments with their customers, the moves, the stops (see
  [One round trip](#one-round-trip)).
- **Pick** relations join into a statement that reads at most two rows without an order and
  otherwise load with the level's to-many relations, one lateral statement per pick for all
  parents' keys (see [Pick relations](#pick-relations)).
- A path loads every relation on it: `m.Stops().Location()` loads moves, their stops and the stops'
  locations.

Narrow a load by starting a query from the relation handle:

| Method | On a to-many load | On a to-one load |
|---|---|---|
| `Where` | filters the related rows | becomes part of the join condition: a row that does not match leaves the field nil; the parent is still returned |
| `Order` | orders each parent's slice | ignored |
| `Pick` | reads only these columns (keel adds the keys it needs) | same |
| `Limit(n)` | at most `n` rows per parent | ignored |
| `Load` | loads relations of the related rows | same |

A column of the related table's base handle also works inside a load:
`m.Where(fleetdb.Moves.Loaded.IsTrue())` is the same as `m.Where(m.Loaded.IsTrue())`.

### One round trip

With the default strategy, `keel.LoadPipelined`, a read and its whole relation tree go to Postgres
in one round trip. A level does not wait for its parents' keys: it selects them with a subquery
that repeats the parent level's statement (its filters, joins, order, limit, offset and keyset),
nested for deeper levels, and keel groups the rows under their parents as they arrive:

```sql
SELECT s."id", … FROM "shipments" s WHERE s."organization_id" = $1 AND s."business_unit_id" = $2
  AND s."customer_id" = $3 ORDER BY s."created_at" DESC, s."id" LIMIT 50;
SELECT m."id", … FROM "moves" m WHERE m."organization_id" = $1 AND m."business_unit_id" = $2
  AND m."shipment_id" IN (SELECT s."id" FROM "shipments" s WHERE s."organization_id" = $1
  AND s."business_unit_id" = $2 AND s."customer_id" = $3 ORDER BY s."created_at" DESC, s."id" LIMIT 50)
  ORDER BY m."sequence" ASC;
```

Each level reads its parent level again, so every statement must see the same snapshot. keel
pipelines only where it does:

| Where the read runs | How it loads |
|---|---|
| outside a transaction, with or without `OnBegin` | pipelined, in an implicit `REPEATABLE READ READ ONLY` transaction whose `BEGIN` and `COMMIT` ride in the same round trip |
| `keel.Read` or `keel.Write` with `keel.RepeatableRead` or `keel.Serializable`, or a `Read` under `Config.ReadIsolation = keel.RepeatableRead` | pipelined |
| a `Read` or `Write` at read committed (the default) | per level |
| a transaction joined with `JoinTx` | per level: keel cannot know its isolation |

A level also loads per level when its parent level could return other rows the second time it
runs, and only from there down to the next level that pipelines again:

- the parent statement has a `Limit` or `Offset` and no `Order` (`First`, or `Limit` without
  `Order`). With an order, keel appends the primary key columns the order lacks as a tie-break
  (`ORDER BY s."created_at" DESC, s."id"` above), so the order is total; `One` needs no order,
  since it fails unless the statement returned every row it matched;
- the parent statement locks (`Lock`; `SKIP LOCKED` would skip other rows), uses `DISTINCT ON`,
  groups, or is limited and `Distinct`;
- the parent is a per-parent `Limit` load without an `Order`.

Many-to-many loads, per-parent `Limit` loads and picks pipeline too: their key list is
`(SELECT DISTINCT * FROM (subquery) kq) AS k(k)` in place of `unnest($1)`. Under a system scope the
subqueries select the tenant columns with the keys (`(m."organization_id", m."business_unit_id",
m."shipment_id") IN (SELECT s."organization_id", s."business_unit_id", s."id" FROM …)`).
`UpdateReturning` loads per level.

keel assumes a statement returns the same rows on the same snapshot. A volatile function
(`random()`), or a `Limit` without a total order inside a CTE or an explicit lateral join, breaks
that; give such a query `LoadStrategy(keel.LoadPerLevel)`:

```go
rows, err := s.LoadStrategy(keel.LoadPerLevel).Load(s.Moves()).List(ctx, db) // one round trip per level
rows, err = s.Load(m.LoadStrategy(keel.LoadPerLevel).Load(m.Stops())).All(ctx, db) // a load chooses for its subtree
```

`keel.LoadPerLevel` reads each level's keys from the rows of the level before and binds them
(`m."shipment_id" = ANY($3)`), one round trip per level, the statements of one level sharing it.
`Config.LoadStrategy = keel.LoadPerLevel` makes it the default; `LoadStrategy(keel.LoadPipelined)`
then opts a query in. Pipelining trades round trips for server work: each level re-runs its
ancestors' filters as a semi-join, which an index on the foreign key and on the parent's filter
serves; on a slow link or a deep tree the saved round trips dominate.

### Per-parent limits

A to-many load with `Limit(n)` loads at most `n` rows per parent through
`unnest($1) … CROSS JOIN LATERAL (… LIMIT n)`, still one statement for all parents:

```go
rows, err := s.
    Load(m.Order(m.Sequence.Desc()).Limit(1)). // each shipment's last move
    Order(s.CreatedAt.Desc()).
    Limit(50).
    List(ctx, db)
```

Many-to-many loads do not take a `Limit`.

### Rules

- A load path must start at the query's table: `s.Load(fleetdb.Moves.Stops())` fails with
  `load moves.Stops does not start at shipments`; use `s.Load(s.Moves().Stops())`.
- Interceptors apply to to-many load statements, as to any statement on their table (see
  [Interceptors](#interceptors)).
- `AfterLoad` hooks run for every loaded row of every table, once all levels are loaded.
- Under a system scope, to-many loads cross tenants when the query they load into says
  `AllTenants`, or when the load itself does (`Load(m.AllTenants())`); see [Tenancy](#tenancy).
  Parents of different tenants may then hold the same key (keys repeat across tenants when a
  table's primary key includes its tenant columns), so a load between two tenant tables matches on
  the tenant columns they share as well as the relation's keys, in its statement
  (`(c.organization_id, c.business_unit_id, c.shipment_id) IN (SELECT * FROM unnest($1, $2, $3))`)
  and when it attaches rows to parents. `Limit` loads and picks unnest the same columns; a
  many-to-many load matches the target's tenant columns. Under a tenant scope every row is pinned
  to the scope's tenant, so the keys alone match and the statement stays `= ANY($1)`.

## Pagination

### Keyset pages

`Page(ctx, db, keel.After(cursor, size))` returns the page of `size` rows after `cursor`; an empty
cursor asks for the first page. `Total(limit)` also counts the matching rows, exactly up to
`limit`, in the same round trip; `Total(limit).Estimate()` makes a total past the limit the
planner's estimate instead of the limit (see [Estimated counts](#estimated-counts)).

```go
page, err := s.
    Where(s.Status.In(fleet.StatusNew, fleet.StatusAssigned)).
    Load(s.Customer()).
    Order(s.CreatedAt.Desc()).
    Page(ctx, db, keel.After(cursor, 50).Total(10_000))
```

| `keel.Page[T]` field | Meaning |
|---|---|
| `Items` | the rows, `[]*T`, with their loads |
| `Next` | the cursor of the following page; empty on the last page |
| `Total` | the number of matching rows when the request asked for it: exact up to its limit; past it the limit ("10,000+"), or with `Estimate()` the planner's estimate, at least the limit plus one ("about 1.2M") |
| `TotalExact` | `Total` is the exact number of matching rows; false when no total was asked for or more rows matched than the limit |
| `CursorAt(i)` | the cursor that resumes after `Items[i]`: a GraphQL connection's edge cursor |

`CursorAt(i)` encodes row `i`'s order values in the format of `Next` (which is the cursor of the
last item when a page follows), without a statement: `After(page.CursorAt(i), n)` reads the `n`
rows after that row. A connection gives every edge its cursor and takes `endCursor` from the last
one, so a client can resume after any row it saw:

```go
page, err := s.Order(s.CreatedAt.Desc()).Page(ctx, db, keel.After(after, first))
edges := make([]Edge, len(page.Items))
for i, sh := range page.Items {
    edges[i] = Edge{Node: sh, Cursor: page.CursorAt(i)}
}
info := PageInfo{HasNextPage: page.Next != ""}
if n := len(edges); n > 0 {
    info.EndCursor = edges[n-1].Cursor
}
```

Rules:

- The order may use columns of the query's table, columns of to-one relations (belongs, one and
  pick paths: `s.Customer().Name.Asc()`, `keel.ResolvePath(s, "customer", "name").Desc()`) and
  expressions (`keel.DynOf(s.Weight).Coalesce(0).Desc()`, `s.Customer().Name.Lower().Asc()`).
  A relation in the order is LEFT JOINed with its tenant condition, the same join a `Load` or a
  filter on that path uses. A column of a to-many relation is refused.
- keel appends every primary key column the order does not already name, in the direction of the
  order's last term, so each row has a unique position. A query without an order pages by primary
  key.
- A column of the table is read from the row for the cursor. A relation column or an expression is
  selected once more, as a hidden value after the row's columns, and decoded into the cursor's
  value: a string, an integer, a float, a bool, a time, bytes or NULL (a uuid as its text). An
  expression of another type (an interval, an array) fails the page.
- NULLs: a relation column is NULL when the related row is missing, and an expression may be NULL,
  so both are treated as nullable. `NullsFirst` and `NullsLast` (default: last ascending, first
  descending, as Postgres sorts) place NULLs the same way in the `ORDER BY` and in the predicate that
  resumes after a cursor holding NULL.
- The cursor is opaque (base64url) and encodes the order values of one row. Pass it only to the
  query that produced it; a cursor whose number of values does not match the order fails.
- `size` must be positive.

When every order term sorts the same way, none uses `NullsFirst` or `NullsLast` and none can be
NULL (columns of the table that are not nullable), keel compares the position as one row,
`(created_at, id, …) < ($3, $4, …)`, which a matching composite index serves. Otherwise it expands
an `OR` chain that places NULLs as the order does, writing an expression in parentheses. See
[Performance notes](#performance-notes) for the index; a page ordered by a relation's column is
served by the relation's index only as a join, so order a large table by its own columns (a
denormalised copy of the related value, kept by a trigger) when it must page fast at any depth.

### Estimated counts

An exact count of a large set reads every matching row. A list header rarely needs it: "1,204
shipments" for a small set and "about 1.2M" for a large one is what a person reads.
`CountEstimate(ctx, db, exactUpTo)` and `Total(limit).Estimate()` give that in one round trip:

```go
c, err := s.Where(s.Status.In(fleet.StatusNew, fleet.StatusAssigned)).CountEstimate(ctx, db, 1000)
// c.Exact: c.N rows matched, counted; else c.N is the planner's estimate, at least 1001

page, err := s.Order(s.CreatedAt.Desc()).
    Page(ctx, db, keel.After(cursor, 50).Total(1000).Estimate())
// page.TotalExact, page.Total as above
```

| `keel.RowCount` field | Meaning |
|---|---|
| `N` | the number of rows: counted when `Exact`, else the planner's estimate, never less than `exactUpTo+1` |
| `Exact` | no more than `exactUpTo` rows matched, so `N` was counted |

How it runs: keel renders the query's rows once, as `SELECT 1 FROM … WHERE …` (with its joins,
groups and tenant predicates, without its order, `Limit` and `Offset`, as `CountUpTo` counts),
and sends two statements in the same round trip with the same text and bound arguments:

- `SELECT count(*) FROM (SELECT 1 … LIMIT exactUpTo+1) c`, which stops after `exactUpTo+1` rows;
- `EXPLAIN (FORMAT JSON) SELECT 1 …`, which plans the same rows without running them. The plan
  is built for the bound values (a prepared `EXPLAIN` plans its query on every run with the
  parameters it is given), and keel reads the top plan node's `Plan Rows`.

When the count reached `exactUpTo+1`, the result is the estimate, raised to `exactUpTo+1` when the
statistics put it lower. The estimate is as good as the table's statistics (`ANALYZE`, autovacuum)
and the planner's selectivity for the filter: close for a filter on indexed or analysed columns,
rough for correlated predicates or expressions. Use it for display, never for logic. A grouped
query estimates its number of groups. `CountEstimateInto` is the `Batch` item; on a dynamic query
`CountEstimate` and `DynCountEstimateInto` wrap the query (`SELECT count(*) FROM (SELECT 1 FROM
(query) d LIMIT exactUpTo+1) c`), so its `Limit` bounds both the count and the estimate.
`Estimate()` without `Total` fails the page.

### Each

`Each(ctx, db, size, fn)` runs the query page by page in keyset order and calls `fn` with each batch
of at most `size` rows, holding one batch in memory. An error from `fn` stops it.

```go
ctx := withSystem(context.Background(), "nightly hazmat audit")
err := s.AllTenants().Where(s.Hazmat.IsTrue()).Each(ctx, db, 500,
    func(ctx context.Context, batch []*fleet.Shipment) error {
        for _, sh := range batch {
            fmt.Println(sh.OrganizationID, sh.ProNumber)
        }
        return nil
    })
```

Each batch is a separate statement; rows written while `Each` runs may or may not be seen. Run it
inside `keel.Read(…, keel.RepeatableRead)` to read one snapshot, at the cost of holding a
connection for the whole run.

### Offsets

`Offset(n)` exists for the rare list that needs it. Prefer `Page` for anything a person scrolls:
an offset makes Postgres read and discard every skipped row.

## Batching reads

`keel.Batch` runs independent reads in one round trip. Each item pairs a query with a typed
destination; a page that shows a list, its total and a header record costs one round trip instead
of three:

```go
type statusCount struct {
    Status fleet.Status
    Loads  int64
}

s, c := fleetdb.Shipments, fleetdb.Customers
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

| Item | Reads what this terminal returns |
|---|---|
| `keel.ListInto(q, &rows)` | `List`; the query needs a `Limit` |
| `keel.AllInto(q, &rows)` | `All` |
| `keel.OneInto(q, &row)` | `One` |
| `keel.FirstInto(q, &row)` | `First` |
| `keel.PageInto(q, req, &page)` | `Page`, with its total when `req` asks for one |
| `keel.CountInto(q, &n)`, `keel.CountUpToInto(q, limit, &n)` | `Count`, `CountUpTo` |
| `keel.CountEstimateInto(q, exactUpTo, &c)`, `keel.DynCountEstimateInto(dq, exactUpTo, &c)` | `CountEstimate`, `DynQuery.CountEstimate` |
| `keel.ExistsInto(q, &ok)` | `Exists` |
| `keel.IntoSlice[R](q, &rs)`, `keel.IntoValue[R](q, &r)` | `keel.Into`, `keel.IntoOne` |

How a batch runs:

- The main statements of every item (a page's count included) are sent together: in the
  transaction on the context; else, when `Config.OnBegin` is set, in an implicit transaction whose
  `BEGIN`, `OnBegin` statements and, when no relation loads follow, `COMMIT` ride in the same round
  trip; else on one pooled connection outside a transaction.
- The to-many relation loads of all items ride in the same round trip where they pipeline (see
  [One round trip](#one-round-trip)); levels that load per level follow, one round trip per level
  for every item together.
- Each item keeps its terminal's checks: scope errors, interceptors, `AfterLoad` hooks,
  `ErrUnbounded` for a list without a `Limit`, `ErrNotFound` and `ErrNotSingular` for single-row
  items.
- `Batch` returns the first error and fills the destinations only when every item succeeded, so a
  failed batch leaves them untouched.
- The shortest `Timeout` among the items bounds the whole batch.
- An item holds no result, so it can be built once and run in more than one batch. `Batch` with no
  items does nothing.

The terminals are one-item batches, so a batch costs nothing beyond the statements it sends.

## Dynamic queries

Typed queries cover code that knows its tables at compile time. Report builders, data tables with
user-chosen columns and filters, and generic exports choose tables, columns, relations and
expressions at run time; they use `keel.DynQuery`. It is non-generic, resolves every identifier
against the generated schema, binds every value, and renders through the same machinery as typed
queries, so tenancy, interceptors, parameter sharing and statement preparation work the same way.
Its result is a typed columnar `DynResult`.

### Starting a dynamic query

A `keel.Registry` finds tables by their SQL name. Build it once from the generated tables and share
it; `DynFrom(table)` starts from a `*schema.Table` directly.

```go
reg, err := keel.NewRegistry(fleetdb.ShipmentsSchema(), fleetdb.CustomersSchema(),
    fleetdb.MovesSchema(), fleetdb.StopsSchema(), fleetdb.TagsSchema(), fleetdb.LocationsSchema())

q := reg.Query("shipments") // an unknown name makes a query whose terminals fail
```

### Columns and paths

`q.Path(segments...)` resolves a column (`keel.ResolvePath` is its counterpart for typed queries;
see [Column paths from names](#column-paths-from-names)): the last segment names the column (its SQL name or Go
field name), the segments before it to-one relations followed from the base table (the relation's
Go name or its snake_case form). The relations are LEFT JOINed with their tenant condition when the
expression is used, exactly as typed relation paths are. A path through a to-many relation is
refused; use `Rel` (below).

```go
pro := q.Path("pro_number")
customer := q.Path("customer", "name") // LEFT JOIN customers c ON … tenant …
q.Select(pro, customer, q.Path("status"), q.Path("total")).
    Where(q.Path("status").In([]string{"New", "Assigned"}), customer.ContainsFold(search)).
    Order(pro.Desc()).
    Limit(100)
res, err := q.Run(ctx, db)
```

Each selected expression becomes a result column named by its label: the path
(`"customer.name"`), or the name given with `As(label)`. `q.Omit(names...)` leaves columns out of
the default columns a query without `Select` returns. Labels stay in Go and never reach the SQL,
where result columns are `c1`, `c2`, …; a label selected twice is an error. Without `Select`, a
table query returns the table's default columns.

### Expressions

A `keel.DynExpr` is an expression, and a predicate, so `Where`, `Having`, `keel.And`, `keel.Or` and
`Filter` take it, in a dynamic query and in a typed one alike: `Pick`, `Where`, `Order`, `Group`
and `ToExpr` of a generated table take it too. Inside a typed query, prefer the typed forms (see
[From dynamic expressions to typed ones](#from-dynamic-expressions-to-typed-ones)): they render the
same SQL and the compiler checks their operands. Operands are other expressions or values, which are always bound as parameters:

| Kind | Methods and functions |
|---|---|
| comparisons | `Eq`, `Ne`, `Gt`, `Gte`, `Lt`, `Lte`, `IsDistinctFrom`, `Between`, `In(slice)`, `NotIn(slice)` (one array parameter), `IsNull`, `IsNotNull`, `IsTrue`, `IsFalse` |
| text | `Like`, `ILike`, `Contains`, `ContainsFold`, `HasPrefix`, `HasSuffix` (the last four escape `%` and `_`), `Concat` |
| arithmetic | `Add`, `Sub`, `Mul`, `Div`, `Mod`, `Neg`, `Coalesce`, `NullIf` |
| logic | `And`, `Or`, `Not` |
| literals | `keel.DynLit(v)`, `Cast(keel.TypeNumeric)` and the other value types |
| CASE | `keel.DynCase().When(cond, then).Else(v)` or `.End()` |
| rows | `keel.DynRow(vs...)`: `ROW(a, b)`, compared element by element; `keel.DynRow(a, b).Gt(keel.DynRow(x, y))` is a keyset condition |
| sets | `InUnnest(slices...)`: `(a, b) IN (SELECT * FROM unnest($1::text[], $2::bigint[]))`, one parameter per slice whatever its length |
| arrays, ranges, jsonb | `keel.DynArray(vs...)` (`ARRAY[…]`), `At(i)` (`d[i]`), `Covers(v)` (`@>`), `CoveredBy(v)` (`<@`); on a jsonb column `Path(keys...)` (`#>`, jsonb) beside `Text(keys...)` (`#>>`, text); on a text[] column `Remove(v)` assigns `array_remove(col, v)` |
| collation | `Collate("C")`: `d COLLATE "C"` |
| constants | `keel.DynConst(v)`: an integer, float, bool or string written into the SQL text, for an expression that must match an expression index or a partial index predicate; everything else is a bound `DynLit` |
| distinct and ordered aggregates | `Agg.Distinct()` and `DynExpr.Distinct()` (`array_agg(DISTINCT e)`), `DynExpr.OrderBy(orders...)` on any aggregate `DynFunc` (`jsonb_agg(e ORDER BY …)`) |
| array of a subquery | `keel.ArrayOf(q)`: `ARRAY(SELECT …)`, an empty array when no row matches; `Query.Distinct()` makes it distinct |
| ordered aggregates | `keel.ArrayAgg(e)`, `keel.StringAgg(e, sep)` with `OrderBy(orders...)`; `keel.PercentileCont(f, order)`, `keel.PercentileDisc(f, order)`, `keel.Mode(order)` (`WITHIN GROUP`); all take `Filter` |
| functions | `keel.DynFunc(name, args...)` from an allow-list |
| aggregates | `Sum`, `Avg`, `Min`, `Max`, `Count`, `CountDistinct`, `keel.DynCount()`, `Filter(pred)`, `Over(window)` |
| full-text search | `Match(q, opts...)`, `Rank(q, opts...)`, `RankCD(q, opts...)` on a `tsvector` column or expression, as on `TSVectorCol` |
| typed expressions | `keel.DynOf(e)`: a generated column, `keel.Rank()`, `keel.Lag(…)`, `keel.Grouping(…)`, a `Raw` |

`DynFunc` calls only functions of keel's allow-list: `abs`, `ceil`, `floor`, `round`, `trunc`,
`sign`, `sqrt`, `power`, `mod`, `greatest`, `least`, `coalesce`, `nullif`, `lower`, `upper`,
`initcap`, `trim`, `ltrim`, `rtrim`, `btrim`, `length`, `substr`, `left`, `right`, `replace`,
`concat`, `concat_ws`, `strpos`, `split_part`, `lpad`, `rpad`, `md5`, `to_char`, `date_trunc`,
`date_part`, `date_bin`, `age`, `to_timestamp`, `timezone`, `make_date`, `now`,
`jsonb_extract_path_text`, `jsonb_typeof`, `jsonb_array_length`, `cardinality`,
`array_to_string`, `array_append`, `array_remove`, `array_position`, `regexp_replace`,
`jsonb_build_array`, `jsonb_build_object`, `jsonb_set`, `width_bucket`, `gen_random_uuid`,
`int4range`, `int8range`, `numrange`, `tstzrange`, and the aggregates `sum`, `avg`, `min`, `max`,
`count`, `bool_and`, `bool_or`, `string_agg`, `array_agg`, `jsonb_agg`, `jsonb_object_agg`,
`stddev`, `stddev_samp`, `stddev_pop`, `variance`, `var_samp` and `var_pop`. `Cast` takes a
`keel.ValueType`, never a type name. An unresolved name, an unknown function or a `Filter`
on something that is not an aggregate makes an expression that carries an error: `Err()` reports it
at once, and a query that uses it fails with it, so a compiler can validate a definition before
running it.

A literal compared with, added to or passed alongside a typed expression takes that expression's
type, as Postgres infers it. Where nothing gives it one, keel casts it to the type of its Go value:
an output of the select list (`Pick(keel.DynLit(0))` is `$1::bigint`), the results of a CASE whose
results are all literals, the elements of an `ARRAY` that are all literals, and the arguments of the
functions declared over `"any"` (`concat`, `concat_ws`, `jsonb_build_object`,
`jsonb_build_array`). An integer is `bigint`, a `uint64` `numeric`, a float `double precision`, a
bool `boolean`, a string `text`, a `time.Time` `timestamptz`, a `[]byte` `bytea` and a slice of one
of them its array; a type with methods (a `Valuer`, a `time.Duration`, a decimal) is left to
Postgres. `Cast` sets any other type. Integer division truncates: cast one side to
`keel.TypeNumeric` or `keel.TypeFloat64`, and guard a divisor that can be zero with `NullIf(0)`.

`Group` and `Order` name an expression the select list already holds by its position: assign the
expression to a variable, pass the same value to `Pick` and to `Group` or `Order`, and the
statement reads `GROUP BY 2` instead of repeating it, so the two always match. A bare column is
written by name.

```go
revenue := q.Path("total").Sum()
miles := q.Path("weight").Sum()
rpm := keel.DynFunc("round", revenue.Div(miles.Cast(keel.TypeNumeric).NullIf(0)), 2).As("rpm")
bucket := keel.DynCase().
    When(q.Path("priority").Gte(2), "urgent").
    When(q.Path("priority").Eq(1), "normal").
    Else("low").As("priority")
month := keel.DynFunc("date_trunc", "month", keel.DynFunc("to_timestamp", q.Path("created_at"))).As("month")
```

### Grouping, windows and subtotals

Dynamic queries have `Group`, `GroupingSets`, `Rollup`, `Cube`, `Having`, `Window` and `Distinct`
with the typed meaning. Write a grouped expression once and reuse the value in `Select`, `Group` and
`Order`: its parameters are bound once, so the clauses match.

```go
code := q.Path("customer", "code")
level := keel.DynOf(keel.Grouping(code, month))
q.Select(code, month, revenue.As("revenue"),
    keel.DynCount().Filter(q.Path("hazmat").IsTrue()).As("hazmat loads"),
    revenue.Sum().Over(keel.Window{}).As("all revenue"),
    keel.DynOf(keel.Rank().OverWindow(keel.OrderBy(revenue.Desc()))).As("rank"),
    level.As("level")).
    Rollup(code, month).
    Having(revenue.Gt(1000)).
    Order(level.Asc(), code.Asc(), month.Asc())
```

### To-many relations

`q.Rel(path...)` resolves a relation path, to-one or to-many. Measures over a to-many relation must
not fan the query's rows out, so the caller chooses how to read it:

- **`Any(preds...)` and `None(preds...)`** are semi- and anti-joins: `EXISTS` correlated with the
  row, with the related tables' tenant predicates and interceptors, one nested `EXISTS` per to-many
  hop. `rel.Col(path...)` resolves the related table's columns, through further to-one relations.
- **`PreAggregate(strategy)`** aggregates the related rows per parent row. `Measure(agg)` adds an
  aggregate and returns the column that holds it for each parent; select it, filter on it in
  `Having`, or aggregate it again in a grouped query. `Where` filters the related rows first.

```go
moves := q.Rel("moves")
stops := q.Rel("moves", "stops")
q.Where(moves.Any(moves.Col("status").Eq("New")),
    stops.None(stops.Col("location", "city").Eq("Dallas")))

pa := moves.PreAggregate(keel.PreAggCTE)
legs := pa.Measure(keel.DynCount().As("legs"))
loaded := pa.Measure(keel.DynCount().Filter(moves.Col("loaded").IsTrue()).As("loaded legs"))
q.Select(code, revenue.As("revenue"), legs.Sum().As("legs"), loaded.Sum().As("loaded legs")).
    Group(code)
```

| Strategy | SQL | Use when |
|---|---|---|
| `keel.PreAggCTE` | a grouped CTE keyed by the parent (`WITH keel_moves (k1, …, c1) AS (SELECT m.shipment_id, …, count(*) FROM moves m WHERE tenant AND (keys) IN (SELECT s.id, … FROM shipments s WHERE <the query's filter>) GROUP BY …)`), LEFT JOINed on the key | reports: one pass over the related table whatever the number of parents |
| `keel.PreAggLateral` | `LEFT JOIN LATERAL (SELECT count(*) AS c1 FROM moves m WHERE tenant AND m.shipment_id = s.id HAVING count(*) > 0) … ON TRUE` | few parents (a page, a top N): one index probe per parent |

Both read NULL for a parent without related rows (wrap a count in `Coalesce(0)` for zero). The first
to-many relation of the path is the level rows are aggregated to; to-many relations after it are
joined inside the aggregation (`q.Rel("moves", "stops")` counts stops per shipment), many-to-many
relations through their join table. The CTE's parent filter repeats the query's `Where`, so a
`Where` that refers to a pre-aggregated measure is refused; filter measures with `Having`, or use
the lateral strategy. The keys include the tenant columns, so a pre-aggregation is exact under a
system scope too.

### CTEs, derived tables and set operations

```go
sub := reg.Query("shipments")
sub.Select(sub.Path("customer_id").As("customer"), sub.Path("total").Sum().As("revenue")).
    Group(sub.Path("customer_id"))

c := reg.Query("customers")
totals := c.With(sub) // WITH keel_cte_1 (c1, c2) AS (…)
c.Join(totals, totals.Col("customer").Eq(c.Path("id"))).
    Select(c.Path("code"), totals.Col("revenue")).
    Where(totals.Col("revenue").Gt(10_000))

outer := keel.DynFromQuery(sub) // a query over sub's rows, columns by label
outer.Select(keel.DynCount().As("customers"), outer.Path("revenue").Avg().As("average"))

names := keel.DynUnionAll(customerNames, locationNames) // also DynUnion, DynIntersect, DynExcept
names.Order(names.Path("name").Asc())
```

`LeftJoin` joins a CTE with LEFT JOIN. The parts of a set operation select the same number of
expressions, and the result's columns take the first part's labels. A query used in `With`,
`DynFromQuery` or a set operation declares `AllTenants` itself, as typed subqueries do. A
`DynQuery` is also a subquery for typed code: `keel.Exists(dq)`, `col.InQuery(dq)`.

### Reading results

`Run` returns every row; `Count` counts them; `CountEstimate(ctx, db, exactUpTo)` counts them
exactly up to `exactUpTo` and returns the planner's estimate above it, as the typed one does (the
count wraps the query, so its `Limit` bounds both); `Exists` reports whether there is any
(`SELECT EXISTS (query)`, which stops at the first row: the check behind a "does this filter match
anything" answer, without reading a row or counting); `Page` reads a keyset page, and with
`keel.After(cursor, n).Total(limit)` (and `.Estimate()`) sets the result's `Total` and
`TotalExact` as the typed `Page` does, in the same round trip; `Stream` yields batches; `DynInto`,
`DynCountInto`, `DynCountEstimateInto`, `DynExistsInto` and `DynPageInto` are `Batch` items, so a
report, its row count and the values of a dynamic pivot share one round trip. Every terminal
applies the query's scope, tenant predicates and interceptors.

A `DynResult` has one `DynColumn` per selected expression and `Rows`. A column has its `Name`
(label), its `Type`, its Postgres `OID`, a null bitmap (`IsNull(i)`) and one typed vector, the one
its type selects, with a zero value at each NULL row:

| Type | Postgres types | Vector |
|---|---|---|
| `TypeString` | text, varchar, char, name, enums, domains over text, uuid, types read as text | `String []string` |
| `TypeInt64` | smallint, integer, bigint, oid | `Int64 []int64` |
| `TypeFloat64` | real, double precision | `Float64 []float64` |
| `TypeNumeric` | numeric | `Numeric []keel.Decimal` |
| `TypeBool` | boolean | `Bool []bool` |
| `TypeTime` | timestamptz, timestamp (UTC) | `Time []time.Time` |
| `TypeDate` | date (midnight UTC) | `Time []time.Time` |
| `TypeBytes` | bytea | `Bytes [][]byte` |
| `TypeJSON` | json, jsonb, and every other type (arrays, ranges, intervals) as JSON | `JSON [][]byte` |

```go
res, err := q.Run(ctx, db)
revenue := res.Column("revenue") // TypeNumeric
for i := range res.Rows {
    if revenue.IsNull(i) {
        continue
    }
    fmt.Println(res.Columns[0].String[i], revenue.Numeric[i])
}
```

`keel.Decimal` is numeric's exact text form (`"-1234.5600"`, `"NaN"`, `"Infinity"`): every digit and
the scale kept, written by keel from the binary format without a `big.Int` per value, bound back as
a numeric parameter, and what CSV, JSON and spreadsheet writers emit; `Float64()` gives an
approximation for charts. `Value(i)` returns one value boxed, for code that handles a few values
generically.

Decoding reads pgx's raw values through a decoder chosen once per column: no per-row map and no
per-value allocation. Strings, decimals, bytes and JSON point into chunks of memory that are never
reused, so they stay valid for the life of the result. A 1000-row read of six columns allocates
82 times; the same projection through `keel.Into` allocates 14,047 times.

### Pages

`Page` reads by keyset in the query's order. Each order term must be a selected expression, or an
expression keel adds as a hidden column; on a table query that neither groups nor is DISTINCT, keel
appends the primary key, so every row has a unique position. A grouped or derived query must order
by expressions that are unique together. `Next` is the cursor of the following page, empty on the
last one; `Limit` and `Offset` do not mix with `Page`.

```go
res, err := q.Order(pro.Desc()).Page(ctx, db, keel.After(cursor, 50))
next := res.Next
rowCursor := res.CursorAt(3) // resumes after row 3
```

`res.CursorAt(i)` is the cursor of row `i` in the format of `Next`, made from the row's order
values, hidden order columns included; it is empty on a result `Page` did not read. An order term
on a bytea or JSON column fails the page.

The page wraps the query (`SELECT k.c1, … FROM (query) k WHERE (k.c1, k.c2) < ($n, $m) ORDER BY …
LIMIT 51`); Postgres flattens the wrapper of an ungrouped query, so an index on the order columns
still serves it.

### Streaming

`Stream(ctx, db, size)` runs the query once and yields its rows in batches of `size`, decoding each
batch as rows arrive, so an export of any size holds one batch in memory. Each batch is a separate
`DynResult` the consumer may keep. A query without rows yields one empty batch, so the columns are
known. Breaking out of the loop stops the query: outside a transaction keel cancels it; inside one
the driver reads and discards the remaining rows. Do not run statements on the same transaction in
the loop body.

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

### Tenancy, interceptors and safety

- Every table a dynamic statement references gets the scope's tenant predicate: the base, joined
  relations, `Any` and `None`, pre-aggregations and their parent filters, CTE bodies. Without a
  scope a statement fails with `ErrNoScope`; under a system scope it needs `AllTenants()`
  (`ErrAllTenantsRequired`), which covers the query's joins, semi-joins and pre-aggregations.
- Interceptors apply to every statement whose base is a table, as for typed reads: the query, its
  pre-aggregation bodies and parent filters, its `EXISTS` subqueries.
- Identifiers only come from the schema: tables, columns and relations are resolved by name and
  written from the schema's quoted names; result columns, CTEs and pre-aggregations get generated
  names; functions and cast types come from fixed lists; window names must match
  `[a-z_][a-z0-9_]*`. A name that does not resolve is an error, never SQL. Values, user search text
  included, are always parameters.

## Writing

### Create

`Create` inserts one row. It fills the `autoid` column when empty, the tenant columns from the
scope, and the `autocreate` and `autoupdate` columns, runs the model hooks and `BeforeWrite` table
hooks, and reads back `readonly` columns.

```go
sh := &fleet.Shipment{
    CustomerID: "cus_01",
    ProNumber:  "PRO-1001",
    Status:     fleet.StatusNew,
    Total:      fleet.Money{Cents: 125_000},
    Labels:     []string{"expedite"},
    Attributes: map[string]any{"trailer": map[string]any{"type": "reefer"}},
}
if err := fleetdb.Shipments.Create(ctx, db, sh); err != nil {
    return err
}
// sh.ID, sh.OrganizationID, sh.CreatedAt and sh.StageRank (generated) are set
```

A non-empty `autoid` value is kept, so rows can carry IDs generated earlier. A row whose tenant
columns differ from the scope's is refused with `ErrTenantMismatch`.

### CreateMany

`CreateMany` inserts many rows in one round trip:

```go
stops := []*fleet.Stop{
    {MoveID: "mov_01", LocationID: "loc_chicago", Sequence: 1, Kind: "pickup"},
    {MoveID: "mov_01", LocationID: "loc_dallas", Sequence: 2, Kind: "delivery"},
}
err := fleetdb.Stops.CreateMany(ctx, db, stops)
```

One row is inserted with `VALUES`. More rows are inserted with `INSERT … SELECT … FROM unnest($1,
$2, …)`, binding one array per column, in chunks of 10,000 rows that share one statement text, all
sent in one round trip. keel reads the table's column types from the catalog on the first bulk write
of a table and caches them per `DB`; call `db.ForgetColumnTypes()` after a migration changes a table
while the process runs. An empty slice does nothing.

### Copy

`Copy` inserts rows through Postgres's binary `COPY` protocol and returns how many it inserted. It
streams the rows instead of building arrays, so it beats `CreateMany` for loads of many thousands of
rows (imports, backfills, seeding):

```go
n, err := fleetdb.Shipments.Copy(ctx, db, rows)
```

- Rows are prepared as `Create` prepares them: the `autoid`, tenant, `autocreate` and `autoupdate`
  columns are filled, a row of another tenant is refused, and the `BeforeCreate`, `BeforeSave` and
  `BeforeWrite` hooks run.
- `COPY` returns nothing, so `readonly` columns are not read back, and `AfterWrite` and `Committed`
  hooks see the rows as written.
- `Copy` joins the transaction on the context. Outside one it runs in an implicit transaction when
  `Config.OnBegin` is set (after the `OnBegin` statements, which `COPY` cannot share a round trip
  with) or when the table has create `AfterWrite` hooks; otherwise it runs on one pooled connection.
- Enum columns need their types registered on the connection, which keel does unless
  `Config.SkipEnums` is set.
- A duplicate key fails the whole `COPY`; use `CreateOrSkip` or `Upsert` when rows may conflict.
- Observers see one `copy` statement whose text is `COPY "table" (…) FROM STDIN (FORMAT binary)`.

### Save and optimistic locking

`Save` updates a row by its primary key. With a `version` column it compares the stored version,
increments it, and returns a `*keel.ConflictError` (matching `keel.ErrConflict`) when another writer
got there first:

```go
c := fleetdb.Customers
cust, err := c.Get(ctx, db, "cus_01")
if err != nil {
    return err
}
cust.Name = "Acme Freight"
err = c.Save(ctx, db, cust)
var conflict *keel.ConflictError
switch {
case errors.As(err, &conflict):
    // conflict.Table changed since conflict.Version was read: reload and retry, or tell the user
case err != nil:
    return err
}
// cust.Version is now the stored version
```

Without a `version` column, `Save` returns `ErrNotFound` when no row matched. With one, a row that
was deleted also reports `ErrConflict`. `Save` stamps the `autoupdate` column, runs `BeforeUpdate`,
`BeforeSave` and `BeforeWrite` hooks, and reads back `readonly` columns and the new version.

Pass columns to write only those (and the `autoupdate` column):

```go
now := time.Now().Unix()
sh.Status = fleet.StatusCanceled
sh.CanceledAt = &now
err := s.Save(ctx, db, sh, s.Status, s.CanceledAt)
```

Only updatable columns can be named: not primary key, tenant, `autocreate`, `version` or `readonly`
columns. `SaveExcept` writes every updatable column but the named ones, for columns a row read
earlier must never overwrite (tokens another process refreshes, counters a trigger maintains):

```go
err := conns.SaveExcept(ctx, db, conn, conns.AccessToken, conns.RefreshToken)
```

[Interceptors](#interceptors) apply to `Save`: their predicates join the primary key in the
`WHERE` clause (with `FROM` for the relations they reference), so a row they exclude is not
written. `Save` reports such a row as it reports a missing one: `ErrConflict` on a table with a
`version` column, `ErrNotFound` otherwise.

### DeleteRow

`DeleteRow` deletes a row by its primary key and returns `ErrNotFound` when nothing was deleted:

```go
if err := fleetdb.Stops.DeleteRow(ctx, db, st); errors.Is(err, keel.ErrNotFound) {
    // already deleted, or excluded by an interceptor
}
```

Interceptors apply as they do to `Save` (with `USING` for the relations they reference). The row
passed needs only its primary key: under a tenant scope keel fills its empty tenant columns from
the scope, as `Create` does, and refuses a row of another tenant, so `DeleteRow(ctx, db, &Stop{ID:
id})` is enough. Delete hooks that declared `Needs` get the stored values of those columns in
`Mutation.Old`.

### Bulk updates and deletes

`Update` and `Delete` on a query write every row it matches and return the number of rows. A query
without a non-nil `Where` predicate is refused; write `Where(keel.True())` to mean every row (of the
tenant). Relation path filters work: they become `FROM` in an update and `USING` in a delete.

```go
now := time.Now().Unix()
n, err := s.
    Where(s.Status.Eq(fleet.StatusNew), s.Customer().Code.Eq("ACME")).
    Update(ctx, db, s.Status.To(fleet.StatusCanceled), s.CanceledAt.To(now), s.Priority.Increment(1))

st := fleetdb.Stops
n, err = st.Where(st.MoveID.Eq("mov_01"), st.ArrivedAt.IsNull()).Delete(ctx, db)
```

Assignments:

| Assignment | SQL |
|---|---|
| `col.To(v)` | `col = $n` |
| `col.Increment(v)` (numeric columns) | `col = s.col + $n`, the stored value plus `v`; `col.ToExpr(e)` assigns any typed arithmetic (`c.Score.ToExpr(c.Score.Mul(keel.Int64(2)))`) |
| `col.ToNull()` | `col = NULL` |
| `col.ToDefault()` | `col = DEFAULT` |
| `col.ToExpr(e)` | `col = e`, for example `s.Weight.ToExpr(keel.Raw("? * 2", s.Weight))` |

The `autoupdate` column is stamped and the `version` column incremented unless an assignment names
them. Assignments must be columns of the query's table.

A bulk update or delete takes `With` and inner `Join`s of tables and CTEs, which become `FROM`
(`USING`) items, their `ON` predicates and tenant predicates joining the `WHERE` clause. With
`keel.Unnest`, a CTE over Go slices bound as one array each, every row gets its own values in one
statement, whatever the number of rows:

```go
c := fleetdb.Contacts
out := keel.Unnest("outcome", []string{"id", "phone", "score"}, ids, phones, scores)
n, err := c.With(out).
    Join(out, out.Col("id").EqCol(c.ID)).
    Where(c.VIP.IsFalse()).
    Update(ctx, db, c.Phone.ToExpr(out.Col("phone")), c.Score.ToExpr(out.Col("score")))
// WITH "outcome" ("id", "phone", "score") AS (SELECT * FROM unnest($1::text[], $2::text[], $3::int8[]))
// UPDATE "contacts" c SET … FROM "outcome" o WHERE … AND o."id" = c."id"
```

`Unnest` takes slices of strings, integers, floats, bools, times or byte slices (named types
included) of equal length; it is also a CTE for reads. `LeftJoin` and lateral joins are refused:
an `UPDATE`'s `FROM` list cannot outer-join the updated table. Bulk writes run no model hooks and no
`BeforeWrite` hooks; see [which hooks run](#which-hooks-run).


`UpdateReturning` is `Update` that returns the updated rows, read in the same statement, instead of
a count: an API that answers a bulk change with the changed records needs no second read.

```go
rows, err := s.
    Where(s.ID.In(ids...), s.Status.Eq(fleet.StatusNew)).
    Load(s.Customer(), s.Moves()).
    Order(s.ProNumber.Asc()).
    UpdateReturning(ctx, db, s.Status.To(fleet.StatusCanceled))
// WITH keel_upd AS (UPDATE "shipments" s SET … WHERE … RETURNING s.*)
// SELECT s."id", …, c."id", … FROM keel_upd s LEFT JOIN "customers" c ON … ORDER BY s."pro_number" ASC
```

- The rows hold the values after the update: the assignments, the `autoupdate` stamp, the
  incremented `version` and the `readonly` columns Postgres computed (generated columns, values set
  by `BEFORE` triggers).
- The update chooses the rows exactly as `Update` does: its filter, the tenant predicate, the
  update interceptors, `With` and inner `Join`s. Read interceptors and read filters do not apply to
  the returned rows; they are the rows the update changed.
- `Load` works: to-one relations are joined into the statement, to-many relations load after it in
  the same transaction, one round trip per level. They read the database as of the statement's
  snapshot, before the update, which matters only when a relation reads the table being updated.
  `AfterLoad` hooks run on the rows.
- `Order` orders the rows. `Pick` (columns of the table) and `Omit` narrow what is read back; the
  primary key and the tenant columns are always read, and every default column when the table has
  update hooks, which receive the same rows (with `Mutation.Old` from `RETURNING old.…` for the
  columns they declared with `Needs`).
- `Limit`, `Offset`, `Lock`, `Group` and `DistinctOn` are refused: an `UPDATE` changes every row it
  matches. A query without `Where` is refused as `Update` refuses it.

### Many-to-many links

`keel.SetLinks` makes a parent's rows in a many-to-many join table exactly the given targets, in
one statement: it deletes the join rows to every other target and inserts the missing ones
(`ON CONFLICT DO NOTHING`, so the join table needs a primary key over its columns).

```go
s := shipmentdb.Shipments
err := keel.SetLinks(ctx, db, s.Tags(), sh.ID, tagIDs) // nil or empty unlinks every tag
```

Between two tenant tables the join rows take the scope's tenant columns and the delete is limited
to them, so `SetLinks` needs a tenant scope. The join table has no model: no hook or interceptor
runs, and its data version moves for the scope's tenant.

### Upsert

`Upsert` inserts rows and updates the existing row on conflict. It reports, per row, whether the row
was inserted (`true`) or updated (`false`):

```go
c := fleetdb.Customers
rows := []*fleet.Customer{
    {Code: "ACME", Name: "Acme Freight"},
    {Code: "GLOBEX", Name: "Globex Logistics"},
}
inserted, err := c.Upsert(ctx, db, rows,
    keel.OnConflict(c.OrganizationID, c.BusinessUnitID, c.Code).Update(c.Name))
```

- `keel.OnConflict(targets...)` names what the unique index covers; it is required. Targets are
  columns, or expressions for an index on expressions: `keel.OnConflict(p.OrganizationID,
  p.BusinessUnitID, keel.Raw("md5(?)", p.Endpoint))` is `ON CONFLICT (organization_id,
  business_unit_id, (md5(endpoint)))`. An expression must not bind values.
- `Update(cols...)` names the columns a conflicting row takes from the proposed row
  (`col = EXCLUDED.col`).
- `Set(assignments...)` gives a conflicting row explicit assignments, the same `Assignment` values
  bulk `Update` takes. Inside them the table's columns read the stored row and `col.Excluded()`
  reads the proposed row; values bind as parameters.
- Without `Update` or `Set`, a conflicting row takes every updatable column. `Update` and `Set`
  combine; a column assigned twice is refused. Unless `Set` assigns them, the `autoupdate` column
  takes the proposed row's stamp and the `version` column is incremented.
- `Where(preds...)` matches a partial unique index: `ON CONFLICT (…) WHERE …`. Its predicates must
  not bind values; use constants, as `c.Email.IsNull()` or `keel.Raw("deleted_at IS NULL")` do.
- Rows that conflict get the stored version and `readonly` columns back. Their primary key keeps the
  value the row was sent with (a fresh `autoid` value), not the stored one; read the row back by its
  unique key when you need it.

```go
c := fleetdb.Customers
inserted, err := c.Upsert(ctx, db, rows,
    keel.OnConflict(c.OrganizationID, c.BusinessUnitID, c.Code).Set(
        c.Name.ToExpr(c.Name.Excluded()),
        c.Email.ToExpr(keel.Raw("coalesce(?, ?)", c.Email, c.Email.Excluded())),
        c.Version.Increment(10),
    ))
// ON CONFLICT (…) DO UPDATE SET "name" = EXCLUDED."name",
//   "email" = coalesce(c."email", EXCLUDED."email"), "version" = c."version" + $n,
//   "updated_at" = EXCLUDED."updated_at"
```

Interceptors do not see upserts: the conflicting row is updated even when an interceptor would hide
it from reads. Give a soft-deleted or otherwise hidden row a partial unique index and match it with
`Where`, or check for it first. Inserted rows run the create hooks and conflicting rows the update
hooks; outside a transaction an upsert runs in an implicit one when the table has `AfterWrite` hooks
for either, so a failing hook rolls every row back.

Many rows go through `unnest`, as in `CreateMany`.

### CreateOrSkip

`CreateOrSkip` inserts rows, skips those that conflict on the given targets (`ON CONFLICT (…) DO
NOTHING`), and returns how many it inserted. Targets are columns or index expressions, as for
`OnConflict`; without any, a row that violates any unique constraint is skipped (`ON CONFLICT DO
NOTHING`):

```go
n, err := c.CreateOrSkip(ctx, db, rows, c.OrganizationID, c.BusinessUnitID, c.Code)
n, err = c.CreateOrSkip(ctx, db, rows) // any unique violation
```

### InsertFrom

`InsertFrom` inserts the rows of a query: `INSERT INTO t (cols) SELECT …`. The query must pick one
expression per listed column, in order. Under a tenant scope the tenant columns are filled from the
scope and must not be listed; under a system scope, list and pick them.

```go
tg, c := fleetdb.Tags, fleetdb.Customers
n, err := tg.InsertFrom(ctx, db,
    c.Pick(keel.DynConst("tag_").Concat(c.ID), c.Name).Where(c.Email.IsNotNull()),
    tg.ID, tg.Name)
```

`InsertFrom` does not fill `autoid`, `autocreate` or `autoupdate` columns and runs no hooks: pick
those values in the query.

`InsertFromSQL` and `UpsertFromSQL` render the statement without running it, for a test.

`UpsertFrom` is `InsertFrom` with a conflict clause, `INSERT INTO t AS alias (cols) SELECT … ON
CONFLICT …`, for rollups that fold a query's rows into a table keyed by a unique index. It takes the
same `keel.OnConflict(...)` an `Upsert` takes. Without `Update`, `Set` or `DoNothing`, a conflicting
row takes the listed columns that are updatable; the `autoupdate` column of an updated row is
stamped with the statement's time unless `Set` assigns it, and the version is incremented. It
returns the rows inserted or updated, and runs no hooks.

```go
d, l := usagedb.Daily, usagedb.Ledger
n, err := d.UpsertFrom(ctx, db,
    l.Pick(l.Provider, l.Endpoint, keel.Count(), l.Units.Sum()).Group(l.Provider, l.Endpoint),
    keel.OnConflict(d.OrganizationID, d.BusinessUnitID, d.Provider, d.Endpoint),
    d.Provider, d.Endpoint, d.Calls, d.Units)
```

`Conflict.UpdateWhere(ps...)` updates a conflicting row only when `ps` hold (`DO UPDATE SET … WHERE
…`; the table's columns read the stored row, `Excluded` the proposed one). A row it rejects is
neither inserted nor updated, so `UpsertFrom` does not count it: a capped counter is one statement.
A `keel.Rows` of values is a one-row source; its literals take the types of their Go values.
`Upsert` refuses `UpdateWhere`, since it reports every row as inserted or updated.

```go
src := keel.Rows("req", []string{"extension_type", "day", "requests"},
    keel.DynLit(kind), keel.DynLit(day), keel.DynLit(1))
reserved, err := u.UpsertFrom(ctx, db, src,
    keel.OnConflict(u.OrganizationID, u.BusinessUnitID, u.ExtensionType, u.Day).
        Set(u.Requests.Increment(1)).
        UpdateWhere(u.Requests.Lt(limit)),
    u.ExtensionType, u.Day, u.Requests)
// reserved == 0: the day's cap is reached
```

### What is read back

After a write keel reads back only what Postgres may have changed: `readonly` columns, and the
version where it changed it. The values keel filled (IDs, tenant columns, timestamps) are already in
the struct. `Update` reads nothing back; `UpdateReturning` reads the updated rows whole.

## Transactions

### Read and Write

`keel.Write` runs a function in a read-write transaction, `keel.Read` in a read-only one. The
transaction lives on the context: every terminal called with that context joins it. Return an error
to roll back.

```go
err := keel.Write(ctx, db, func(ctx context.Context) error {
    sh, err := s.Where(s.ID.Eq("shp_01")).Lock(keel.ForUpdate, keel.NoWait).One(ctx, db)
    if err != nil {
        return err
    }
    sh.Status = fleet.StatusAssigned
    if err := s.Save(ctx, db, sh, s.Status); err != nil {
        return err
    }
    mv := &fleet.Move{ShipmentID: sh.ID, Sequence: 1, Status: fleet.StatusAssigned}
    if err := fleetdb.Moves.Create(ctx, db, mv); err != nil {
        return err
    }
    keel.AfterCommit(ctx, func(ctx context.Context) {
        log.Printf("shipment %s assigned", sh.ID)
    })
    return nil
}, keel.LockTimeout(2*time.Second))
```

`keel.ReadValue` and `keel.WriteValue` return a value:

```go
sh, err := keel.ReadValue(ctx, db, func(ctx context.Context) (*fleet.Shipment, error) {
    return s.Where(s.ProNumber.Eq("PRO-1001")).Load(s.Customer(), s.Moves()).One(ctx, db)
})

id, err := keel.WriteValue(ctx, db, func(ctx context.Context) (ids.ID, error) {
    c := &fleet.Customer{Code: "UMBRELLA", Name: "Umbrella Transport"}
    if err := fleetdb.Customers.Create(ctx, db, c); err != nil {
        return "", err
    }
    sh := &fleet.Shipment{CustomerID: c.ID, ProNumber: "PRO-2001", Status: fleet.StatusNew}
    if err := fleetdb.Shipments.Create(ctx, db, sh); err != nil {
        return "", err
    }
    return sh.ID, nil
}, keel.Serializable, keel.Retry(3))
```

How transactions run:

- `BEGIN`, the transaction options and the `OnBegin` statements are sent with the first statement,
  in one round trip. A transaction that runs no statement costs nothing.
- The transaction holds one connection from its first statement to its end. Use its context from
  one goroutine at a time.
- A panic in the function rolls the transaction back and returns its connection to the pool before
  it goes on up the stack, so a recovered panic leaks nothing.
- A `Read` returns as soon as its `COMMIT` is written; see
  [Read-only transactions end without waiting](#read-only-transactions-end-without-waiting).
- A `Read` or `Write` inside a `Write` joins it; a `Read` inside a `Read` joins it; a `Write` inside
  a `Read`, and any write inside a `Read`, returns `ErrReadOnly`. Options of a joined call are
  ignored. `keel.Detached()` starts a transaction of its own instead; see
  [Detached transactions](#detached-transactions).
- A transaction keeps the scope it began with: its `OnBegin` statements were made for it. A
  statement, `Read`, `Write`, `Savepoint` or `LockKey` whose context carries another scope (a system
  scope inside a tenant's transaction, another tenant) returns `ErrScopeMismatch` instead of joining
  it. Two system scopes are the same scope whatever their reasons; a tenant is compared by its
  values, so a context rebuilt with the same tenant joins.
- `keel.InTx(ctx, db)` reports whether the context carries a transaction of `db`.

### Implicit transactions

Outside `Read` and `Write`, a terminal runs on a pooled connection, a replica's for a read on a
context marked with [`OnReplica`](#read-replicas). It runs in an implicit transaction when
`Config.OnBegin` is set, when the table has `AfterWrite` hooks for the write, for `CustomPlan`, or
for a write on a marked context of a `DB` with replicas, which reads its commit position after
`COMMIT`. A terminal with one statement then sends `BEGIN`, the `OnBegin` statements, the
statement and `COMMIT` in one round trip; with relation loads, `BEGIN` goes with the first batch and
`COMMIT` with the last. A read whose relation loads pipeline (see [One round trip](#one-round-trip))
runs in an implicit `REPEATABLE READ READ ONLY` transaction even without `OnBegin`, so its levels
read one snapshot; its `BEGIN` and `COMMIT` share the round trip.

### Options

| Option | Effect |
|---|---|
| `keel.LockTimeout(d)` | any statement of the transaction waits at most `d` for a lock (`set_config('lock_timeout', …, true)`, sent with `BEGIN`) |
| `keel.Setting(name, value)` | sets a run-time parameter for the transaction (`set_config(name, value, true)`, sent with `BEGIN`), such as `hnsw.ef_search`; a `Read` or `Write` that joins a transaction sets it for `fn` and restores the joined transaction's value after |
| `keel.ReadCommitted`, `keel.RepeatableRead`, `keel.Serializable` | `ISOLATION LEVEL …`; the levels are options themselves (constants of `keel.Isolation`), and a `Read` without one runs at `Config.ReadIsolation` |
| `keel.Retry(n)` | reruns a top-level transaction up to `n` more times after a serialization failure or deadlock (`keel.IsRetryable`); `fn` must be safe to repeat |
| `keel.Detached()` | begins a transaction of its own even inside another; see [Detached transactions](#detached-transactions) |

```go
var open, late int64
err := keel.Read(ctx, db, func(ctx context.Context) error {
    var err error
    if open, err = s.Where(s.Status.Ne(fleet.StatusDelivered)).Count(ctx, db); err != nil {
        return err
    }
    late, err = s.Where(s.Status.Eq(fleet.StatusInTransit), s.Priority.Gte(2)).Count(ctx, db)
    return err
}, keel.RepeatableRead)
```

`Retry` starts each attempt as a new transaction; `AfterCommit` functions of failed attempts are
dropped. Nested calls never retry on their own.

### Isolation

`keel.ReadCommitted`, `keel.RepeatableRead` and `keel.Serializable` are constants of
`keel.Isolation` and options of `Read` and `Write`. `Config.ReadIsolation` sets the level of every
explicit `Read` that passes none; `Write` and the implicit transactions of single terminals keep the
server's default unless an option says otherwise.

```go
cfg.ReadIsolation = keel.RepeatableRead // every Read reads one snapshot

err := keel.Read(ctx, db, func(ctx context.Context) error { … }, keel.ReadCommitted) // this one does not
```

A read-only `REPEATABLE READ` transaction gives every statement the snapshot its first statement
took, so the counts and lists of one request agree with each other, and pipelined relation loads
read parents and children from one state. It takes no locks and cannot fail a serialization check,
so it costs nothing over `READ COMMITTED` but a snapshot held for the transaction's length. A
`ReplicaTx` read runs `REPEATABLE READ` where `ReadIsolation` says `Serializable`, which a hot
standby refuses; an explicit `keel.Serializable` with `ReplicaTx` is refused.

### Read-only transactions end without waiting

When the function of a `Read` returns, keel writes `COMMIT` (`ROLLBACK` when it returned an error)
and returns at once. A read-only transaction has nothing to make durable, so the caller has nothing
to wait for. A goroutine reads the reply and only then releases the connection to the pool, which
would destroy a connection still owed a reply; the next transaction that needs the connection waits
for that, not the caller. A reply that fails (a broken connection) closes the connection instead of
returning it, and `db.ReadCommitFailures()` counts those. The reply is read under a context of its
own, so cancelling the caller's context after the `Read` returns, as an HTTP server does when the
handler finishes, changes nothing. `db.Close` waits for the replies still in flight.

The same holds for an implicit read-only transaction whose `COMMIT` would cost a round trip of its
own (relation loads whose last level had no rows to load). When `COMMIT` rides in the round trip of
the last statements, as it does for most terminals, keel reads it with them: Postgres sends the
statements' results only once it has processed the whole pipeline, `COMMIT` included, so there is
nothing to gain.

Writes always wait, and so do `Serializable` reads, whose `COMMIT` can report a serialization
failure that `Retry` must see, transactions joined with `JoinTx`, whose owner commits them, and every
read when `Config.AwaitReadCommit` is set. `AfterCommit` callbacks of a read run when the `Read`
returns, as before. An advisory lock a `Read` took (`LockKey`) is released when the server processes
the `COMMIT`, a moment after `Read` returns.

### Detached transactions

`keel.Detached()` makes a `Read` or `Write` begin a new transaction on a pool connection of its own
even when the context carries one. It commits or rolls back on its own: the outer transaction rolling
back does not undo it, and it failing does not fail the outer one unless the caller returns its
error. Use it for:

- **Work under another scope.** The transaction begins with the context's scope, so `OnBegin` sets
  up that scope (row-level security) rather than the outer transaction's. A system-scoped read
  inside a tenant's transaction, or work for another tenant, needs it; joining returns
  `ErrScopeMismatch`.
- **Work that must commit whatever the caller does**: allocating a number from a sequence table, an
  audit row for an attempt that may fail.

```go
err := keel.Write(ctx, db, func(ctx context.Context) error {
    num, err := keel.WriteValue(ctx, db, func(ctx context.Context) (int64, error) {
        return nextInvoiceNumber(ctx) // commits now; a rolled-back invoice leaves a gap
    }, keel.Detached(), keel.Retry(3))
    if err != nil {
        return err
    }
    rates, err := keel.ReadValue(withSystem(ctx, "global rate table"), db,
        func(ctx context.Context) ([]*Rate, error) {
            return r.AllTenants().Where(r.Lane.Eq(lane)).All(ctx, db)
        }, keel.Detached())
    if err != nil {
        return err
    }
    return createInvoice(ctx, num, rates)
})
```

- For the callback's duration the detached transaction hides the outer one: terminals,
  `Savepoint`, `LockKey` and `AfterCommit` inside it use the detached transaction. Its `AfterCommit`
  callbacks and `Committed` hooks run when it commits, with the caller's context, which still
  carries the outer transaction. Its options (`Serializable`, `LockTimeout`, `Retry`, `ReplicaTx`)
  apply as to a top-level transaction.
- It is a separate transaction: it sees what other transactions committed, not the outer
  transaction's uncommitted writes, and the outer one sees its writes once it commits (under READ
  COMMITTED).
- It holds a second connection while the outer transaction holds its first. Size the pool for
  the requests that do this at once (each needs two connections), or they can wait on each other
  for connections forever.
- It must not wait on a lock the outer transaction holds (a row the outer transaction updated, an
  advisory lock it took): the outer transaction waits for the detached one to return, so neither
  ends. Give it a `LockTimeout` where that can happen.
- It composes with `keel.JoinTx`: inside a joined transaction a detached one runs on keel's own
  pool, outside the other layer's transaction.
- Outside a transaction `Detached` changes nothing.

### Savepoints

`keel.Savepoint` runs a function inside a savepoint of the transaction on the context. An error rolls
back to the savepoint and is returned, leaving the transaction usable. Outside a transaction it runs
the function in a new `Write`; inside a `Read` it returns `ErrReadOnly`.

```go
err := keel.Write(ctx, db, func(ctx context.Context) error {
    for _, code := range []string{"ACME", "GLOBEX", "ACME"} {
        err := keel.Savepoint(ctx, db, func(ctx context.Context) error {
            return c.Create(ctx, db, &fleet.Customer{Code: code, Name: code})
        })
        if keel.IsUnique(err, "uq_customers_code") {
            continue
        }
        if err != nil {
            return err
        }
    }
    return nil
})
```

### Advisory locks

`keel.LockKey(ctx, db, key)` takes a transaction-scoped advisory lock on a string key, waiting for
it, so work without a row to lock is serialised across processes. `keel.TryLockKey` does not wait
and reports whether it got the lock. Both need a transaction on the context; the lock is released
when it ends.

```go
err := keel.Write(ctx, db, func(ctx context.Context) error {
    if err := keel.LockKey(ctx, db, "invoice-run:org_acme"); err != nil {
        return err
    }
    // only one invoice run per organization at a time
    return nil
})

err = keel.Write(ctx, db, func(ctx context.Context) error {
    got, err := keel.TryLockKey(ctx, db, "rate-refresh")
    if err != nil {
        return err
    }
    if !got {
        return nil // another process is refreshing rates
    }
    return refreshRates(ctx)
})
```

Keys are hashed with `hashtextextended`; namespace them (`"invoice-run:" + orgID`).

### AfterCommit

`keel.AfterCommit(ctx, fn)` runs `fn` after the transaction on the context commits and drops it if
the transaction rolls back. Outside a transaction `fn` runs at once. Use it to publish events and
enqueue work that must not happen for a rolled-back write.

## Tenancy

### Scopes

Every statement keel renders runs under the scope of its context:

- **Tenant scope**: `Config.Tenant` returned values for every column of `Config.TenantColumns`.
  Every statement on a table with `tenant` columns compares them with the scope: reads, joins,
  relation loads, subqueries, bulk updates and deletes, `Save` and `DeleteRow`. Inserts fill empty
  tenant columns from the scope and refuse a row of another tenant. Relation joins pin the target's
  tenant columns to the same constants.
- **System scope**: `Config.System` returned true, with a reason. Statements on tenant tables must
  say `AllTenants()`; relation joins compare tenant columns between the two tables. Inserts must
  carry their tenant columns.
- **No scope**: statements on tenant tables fail with `ErrNoScope`. Tables without tenant columns
  (`Plant`) need no scope.

`System` is checked first, so a context with a system scope is never treated as a tenant's.

### Working across tenants

```go
ctx := withSystem(context.Background(), "purge canceled shipments")
s := fleetdb.Shipments
m := s.Moves()
cutoff := time.Now().AddDate(-1, 0, 0).Unix()

stale, err := s.AllTenants().
    Where(s.Status.Eq(fleet.StatusCanceled), s.CanceledAt.Lt(cutoff), m.None(m.Loaded.IsTrue())).
    Load(s.Customer(), m.Stops()).
    Limit(1000).
    List(ctx, db)

n, err := s.AllTenants().
    Where(s.Status.Eq(fleet.StatusCanceled), s.CanceledAt.Lt(cutoff)).
    Delete(ctx, db)
```

A query that says `AllTenants` crosses tenants with everything it contains: to-one relations
joined into it, its to-many relation loads at every level, and its `Any` and `None` predicates.
A query used as a subquery (`keel.Exists`, `InQuery`, a lateral join, a CTE, a set operation) is a
query of its own and says `AllTenants` itself; without it the statement fails with
`ErrAllTenantsRequired`. `Save` and `DeleteRow` under a system scope write by primary key without
`AllTenants`.

### Scope errors

| Error | When |
|---|---|
| `keel.ErrNoScope` | a statement on a tenant table without a tenant or system scope |
| `keel.ErrAllTenantsRequired` | a statement on a tenant table under a system scope that did not say `AllTenants()` |
| `keel.ErrSystemScopeRequired` | `AllTenants()` without a system scope |
| `keel.ErrTenantMismatch` | a row's tenant columns differ from the tenant scope (`Create`, `Save`, `DeleteRow`, …) |

The errors are wrapped with the table name; test them with `errors.Is`. An insert under a system
scope with empty tenant columns fails with a plain error naming the column.

```go
_, err := s.Query().Limit(10).List(context.Background(), db)           // ErrNoScope
_, err = s.Query().Limit(10).List(withSystem(ctx, "report"), db)       // ErrAllTenantsRequired
_, err = s.AllTenants().Limit(10).List(requestContext(), db)           // ErrSystemScopeRequired
err = fleetdb.Customers.Create(requestContext(), db,
    &fleet.Customer{OrganizationID: "org_other", BusinessUnitID: "bu_other", Code: "X", Name: "X"}) // ErrTenantMismatch
plant, err := fleetdb.Plants.Get(context.Background(), db, "DAL1")     // no scope needed
```

### Tenant concurrency

One tenant's burst (an import, a dashboard of twenty widgets, a runaway client) can take every
connection of the pool and stall all other tenants. `Config.TenantConcurrency` bounds how much of the
`DB` one tenant uses at once:

```go
cfg.TenantConcurrency = 8 // at most 8 terminals or transactions per tenant at a time
```

- Each terminal outside a transaction and each top-level `Read` or `Write` takes a slot of its
  context's tenant (the values of `Config.Tenant`, as `TenantKey` writes them) before it acquires a
  connection, and gives it back when it ends. Work past the bound waits, first come first served,
  until a slot frees or its context ends; the error then wraps the context's error.
- A transaction holds one slot from its start to its end; its statements take none. Work started
  under a context that carries a slot of the same tenant shares it: a `Detached` transaction inside
  the tenant's transaction, and the queries a write's hooks run outside a transaction. Without that,
  a bound of 1 would deadlock on them.
- System scopes and unscoped work take no slot.
- The slot is given back before `AfterCommit` callbacks and `Committed` hooks run, so work they start
  takes a slot of its own.
- `db.TenantWaits()` counts the terminals and transactions that had to wait: a rising count names a
  bound too low or a tenant that needs looking at.

Slots live in a map split into 64 independently locked shards, holding only the tenants with work
running or waiting, so its size follows the number of active tenants, not of all tenants. Work
started under a fresh context while the tenant's transaction is open (a goroutine that drops the
transaction from its context) needs a slot of its own: keep the bound above the nesting the
application does.

## Hooks, interceptors and observers

| Kind | Registration | Use |
|---|---|---|
| model hooks | methods on the model | normalisation and derived fields |
| table hooks | `keel.OnCreate`, `OnUpdate`, `OnDelete`, `OnWrite` | validation, audit, side effects |
| interceptors | `db.Intercept(handle, fn)`, `db.InterceptAll(fn)` | add predicates to statements |
| observers | `Config.Observers` | tracing, metrics, slow-query logs |

### Table hooks

Table hooks are registered per `DB` and table, usually at startup, and run in one of three phases:

| Phase | When | An error |
|---|---|---|
| `keel.BeforeWrite` | inside the transaction, before an entity write; may change the row | aborts the write |
| `keel.AfterWrite` | inside the transaction, after the statement, for entity and bulk writes | rolls the transaction back |
| `keel.Committed` | after the transaction commits | is dropped; report it from the hook |

```go
s := fleetdb.Shipments

keel.OnCreate(db, s, keel.BeforeWrite, func(ctx context.Context, m *keel.Mutation[*fleet.Shipment]) error {
    if m.Row.ProNumber == "" {
        return errors.New("shipment needs a PRO number")
    }
    if m.Row.Attributes == nil {
        m.Row.Attributes = map[string]any{}
    }
    return nil
})

keel.OnUpdate(db, s, keel.AfterWrite, func(ctx context.Context, m *keel.Mutation[*fleet.Shipment]) error {
    if m.Old == nil || !m.Changed(s.Status) || m.Old.Status == m.Row.Status {
        return nil
    }
    log.Printf("shipment %s: %s -> %s", m.Row.ID, m.Old.Status, m.Row.Status)
    return nil
}, keel.Needs(s.Status))

keel.OnWrite(db, fleetdb.Customers, keel.Committed, func(ctx context.Context, m *keel.Mutation[*fleet.Customer]) error {
    log.Printf("customer %s %s; refresh the search index", m.Row.ID, m.Op)
    return nil
})
```

`OnCreate`, `OnUpdate` and `OnDelete` register for one operation, `OnWrite` for all three. A hook
receives a `*keel.Mutation[T]`:

| Field or method | Meaning |
|---|---|
| `Op` | `keel.OpCreate`, `keel.OpUpdate` or `keel.OpDelete` (`String()` gives `create`, `update`, `delete`) |
| `Row` | the row as written: the entity passed to an entity write, or a row returned by a bulk write (for `DeleteRow`, the entity passed, which may hold only its key) |
| `Old` | the stored values, before the write, of the columns the table's hooks declared with `Needs`, for every update (`Save`, an upsert's conflicting rows, bulk updates) and every delete (`DeleteRow`, bulk deletes); other fields are zero. Nil for creates and when no hook declared `Needs` |
| `Changed(col)` | whether the write set the column: always true for creates; the written columns for `Save` and bulk updates; the columns the `Conflict` assigns for an upsert's conflicting rows; false for deletes |

`keel.Needs(cols...)` reads the previous values in the same statement (`RETURNING old.…`, which
needs PostgreSQL 18). It is only paid for when the table has `AfterWrite` or `Committed` hooks for
the write's operation.

A table with `AfterWrite` hooks for an operation makes those writes transactional: outside `Write`
they run in an implicit transaction, so a hook error rolls the statement back. An `Upsert` opens it
for create or update hooks, since it may do either. A table without hooks pays nothing.

Bulk writes on a table with `AfterWrite` or `Committed` hooks for their operation return every
written row through `RETURNING` and call the hooks once per row, which costs memory proportional to
the rows written.

### Which hooks run

| Write | Model hooks | `BeforeWrite` | `AfterWrite` and `Committed` |
|---|---|---|---|
| `Create`, `CreateMany` | `BeforeCreate`, `BeforeSave` | create | create |
| `Copy` | `BeforeCreate`, `BeforeSave` | create | create, with rows as written (nothing read back) |
| `Upsert` | `BeforeCreate`, `BeforeSave` on every row | create, on every row | create for inserted rows, update with `Old` for conflicting rows |
| `CreateOrSkip` | `BeforeCreate`, `BeforeSave` on every row | create, on every row | create for inserted rows |
| `Save` | `BeforeUpdate`, `BeforeSave` | update | update, with `Old` |
| `DeleteRow` | `BeforeDelete` | delete | delete, with `Old` |
| `Query.Update` | none | none | update per row, with `Old` |
| `Query.Delete` | none | none | delete per row, with `Old` |
| `InsertFrom`, `UpsertFrom` | none | none | none |

On inserts, keel fills the `autoid`, tenant and timestamp columns before the hooks run, so hooks see
them. `AfterLoad` runs after reads.

### Interceptors

An interceptor sees a statement before it is rendered and may add predicates to its `WHERE`
clause. `db.Intercept` registers one for the statements whose base is a table; `db.InterceptAll`
for those of every table (it runs before the per-table ones).

```go
s := fleetdb.Shipments
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

`keel.Intercepted` carries `Op` (`keel.OpRead`, `keel.OpUpdate` or `keel.OpDelete`), `Read`, and
`Table` (the `*schema.Table`); `Where(preds...)` adds predicates, joined with `AND`. An error aborts
the statement.

Interceptors see exactly the statements that have a `WHERE` clause over their table:

| Statement | `Op` |
|---|---|
| queries: every terminal and `Batch` item, `Page`'s total, `keel.Into`, `ToSQL` | `OpRead` |
| to-many relation loads, and subqueries: `Exists`, `InQuery`, `Any`, `None`, CTE bodies, lateral joins, the source query of `InsertFrom` | `OpRead` |
| `Save`, and bulk `Update` | `OpUpdate` |
| `DeleteRow`, and bulk `Delete` | `OpDelete` |

On `Save` and `DeleteRow` the predicates join the primary key, so a row they exclude is not
written: a soft-delete or ownership interceptor cannot be bypassed by a single-row write. Relation
path predicates (`s.Customer().Code.Ne(…)`) become `FROM` or `USING` joins, as in bulk writes.

Interceptors never see inserts (`Create`, `CreateMany`, `Copy`, `Upsert`, `CreateOrSkip`, the
`INSERT` of `InsertFrom`), which have no `WHERE` clause, and that includes the update an `Upsert`
applies to a conflicting row. They do not see relations joined into a statement or tables added with
`Join` either. Columns of the table's base handle resolve inside statements built on a relation path
to the table, so one interceptor covers loads and `Any`.

### Observers

An observer sees every statement keel sends:

```go
type slowQueryLog struct{ threshold time.Duration }

func (slowQueryLog) BeforeQuery(ctx context.Context, _ *keel.QueryEvent) context.Context {
    return ctx
}

func (l slowQueryLog) AfterQuery(_ context.Context, e *keel.QueryEvent) {
    if e.Duration >= l.threshold {
        log.Printf("slow %s on %s (%d args, %d rows): %v: %s",
            e.Op, e.Table, e.Args, e.Rows, e.Duration, e.SQL)
    }
}
```

| `keel.QueryEvent` field | Meaning |
|---|---|
| `Op` | `select`, `count`, `insert`, `update`, `delete`, `copy`, `savepoint` or `lock` |
| `Table` | the statement's table |
| `SQL` | the statement text, which never contains values |
| `Args` | the number of bound arguments |
| `Values` | the bound arguments, only when `Config.ObserveArgs` is set |
| `Replica` | the replica the statement ran on (`ReplicaStats.Name`); empty for the primary |
| `Tx` | the statement ran in an explicit transaction (`Write`, `Read`, `Tx`), not an implicit one |
| `Start`, `Duration` | when the round trip started and how long it took; statements sent in one round trip share both |
| `Rows` | rows affected or returned |
| `Err` | the error the terminal returns: a constraint violation is already a `*keel.ConstraintError`, and a missing row is `keel.ErrNotFound` |

For each statement keel calls every observer's `BeforeQuery` before the round trip, in order: the
first gets the terminal's context, each later one the context the previous one returned. The
context the last one returns is the statement's context:

- `AfterQuery` gets it after the round trip, observers in reverse order, so an observer can start a
  span in `BeforeQuery` and end it in `AfterQuery`.
- A round trip that carries one statement (most terminals, `Save`, `Copy`, with or without the
  `BEGIN`, `OnBegin` statements and `COMMIT` of an implicit transaction) runs under that context,
  so a deadline, a cancellation or a value an observer adds reaches the execution and pgx's own
  tracer.
- Statements that share a round trip (a `Batch`, one level of relation loads, the chunks of a large
  `CreateMany`, a page and its total) each start from the terminal's context, so their contexts are
  siblings and one statement's span never contains another's. The round trip itself runs under the
  terminal's context.

```go
type traceKey struct{}

type statementSpan struct {
    name  string
    start time.Time
}

type tracingObserver struct{}

func (tracingObserver) BeforeQuery(ctx context.Context, e *keel.QueryEvent) context.Context {
    return context.WithValue(ctx, traceKey{}, &statementSpan{name: e.Op + " " + e.Table, start: time.Now()})
}

func (tracingObserver) AfterQuery(ctx context.Context, e *keel.QueryEvent) {
    sp, ok := ctx.Value(traceKey{}).(*statementSpan)
    if !ok {
        return
    }
    log.Printf("span %s: %v, %d rows, error %v", sp.name, time.Since(sp.start), e.Rows, e.Err)
}
```

The `*QueryEvent` is the same pointer in both calls. `BEGIN`, `COMMIT`, `OnBegin` statements and
plan-mode switches are not reported.

`keel.RecordShapes(w)` is a ready-made observer that writes each distinct statement text once, for
the [plan guard](#plan-guard).

### Query log

`keel/keellog` prints statements for a developer reading a terminal: a header line per statement
(operation, table, `tx` inside an explicit transaction, duration, rows or the outcome, and the file
and line that issued it), then the SQL broken before its clauses, with the bound values written in
when `Config.ObserveArgs` is set.

```go
db, err := keel.Open(ctx, keel.Config{
    DSN:         dsn,
    ObserveArgs: true,
    Observers:   []keel.Observer{keellog.New(keellog.Options{Mode: keellog.All})},
})
```

```text
keel SELECT shipments                    2.3ms  25 rows     shipmentrepository/list.go:142
     SELECT s.id, s.pro_number, s.status
     FROM shipments s
     WHERE s.organization_id = 'org_01J8…' AND s.status = ANY('{New,InTransit}')
     ORDER BY s.created_at DESC LIMIT 25
keel SELECT workers                      0.3ms  not found   workerrepository/get.go:40
keel INSERT customers             tx     0.4ms  ERROR       customerrepository/create.go:51
     INSERT INTO customers (id, code, name)
     VALUES ('cus_01J…', 'ACME', 'Acme')
     23505 unique violation uq_customers_code → code
```

| `keellog.Options` field | Meaning |
|---|---|
| `Mode` | `keellog.Errors` (the default) prints failed and slow statements; `keellog.All` prints every one. `ParseMode` reads `"errors"` or `"all"` |
| `Slow` | the duration from which a statement is marked slow and printed in `Errors` mode; defaults to 100ms |
| `Out` | defaults to `os.Stderr` |
| `Color` | ANSI colors; defaults to on when `Out` is a terminal and `NO_COLOR` is unset |
| `Skip` | function name prefixes never reported as the caller, besides keel's own: a repository helper every query passes through |
| `MaxValue`, `MaxElems`, `MaxSQL` | how much of a bound string (64), a bound slice (8) and the SQL (4000) is printed |

`keel.ErrNotFound` is an outcome, not a failure: it is printed dim in `All` mode and never in
`Errors` mode. `keellog.Format` and `keellog.Inline` are the formatter and the value writer on their
own. Values are the data itself, so set `ObserveArgs` only where a log may hold it; production
logging belongs in an observer that writes structured fields to the application's logger.

### Budgets and N+1 detection

`keel.Budget(ctx, opts...)` returns a context that counts the work keel does under it: round trips,
statements and statement texts sent again. Put one on a request's context in middleware and every
terminal, transaction and goroutine of the request counts against it.

```go
ctx = keel.Budget(ctx, keel.MaxRoundTrips(20), keel.MaxRepeats(3))
rows, err := s.Where(…).Limit(50).List(ctx, db)
for _, sh := range rows {
    c, err := fleetdb.Customers.Get(ctx, db, sh.CustomerID) // the fourth Get fails
    …
}
```

| Option | Limit |
|---|---|
| `keel.MaxRoundTrips(n)` | at most `n` round trips; `MaxRoundTrips(0)` asserts that a path (a cache hit) touches no database |
| `keel.MaxStatements(n)` | at most `n` statements |
| `keel.MaxRepeats(n)` | no statement text sent in more than `n` round trips: the shape of an N+1 read, one query per row of an earlier result |
| `keel.OnExceeded(fn)` | call `fn(ctx, report)` once, when the first limit is crossed, instead of failing |

Without `OnExceeded`, the terminal whose round trip crosses a limit returns a `*keel.BudgetError`
(`errors.Is(err, keel.ErrBudgetExceeded)`, with `Limit`, `Max` and the `Report`) after its
statements ran, and every later round trip under the budget is refused before it is sent. A write
outside a transaction has committed when it returns the error; inside one, returning the error rolls
the transaction back. With `OnExceeded` the work goes on: log the report in production, fail the
test in development.

`keel.BudgetReportFrom(ctx)` returns the innermost budget's account, for a log line or a metric at
the end of the request:

```go
if r, ok := keel.BudgetReportFrom(ctx); ok && len(r.Repeats) > 0 {
    for _, rep := range r.Repeats {
        log.Printf("%d× %s at %v", rep.Count, rep.SQL, rep.CallSites)
    }
}
```

- `RoundTrips` counts the round trips that carried statements (a replica read rerun on the primary
  counts twice), plus the `BEGIN` keel sends on its own before a `COPY`; the `COMMIT` that ends a
  transaction is not counted.
- `Statements` counts what observers see: queries, relation loads, writes, `COPY`, savepoints,
  advisory locks and raw SQL; not `BEGIN`, the `OnBegin` statements or `COMMIT`.
- `Repeats` lists the texts sent in more than one round trip, the most repeated first. A text counts
  once per round trip, so the chunks of one `CreateMany` or the items of one `Batch` that share a
  text are not repeats. `CallSites` holds the distinct `file:line` places, outside keel and the
  generated `…db` packages, that sent it again (up to 8). keel captures a stack only when a text
  repeats, so the statements that do not cost nothing beyond a map lookup.
- A budget inside another counts against both; one budget is safe to share between goroutines.
- A result the cache serves sends nothing and counts nothing.

## Errors

| Error | Returned by |
|---|---|
| `keel.ErrNotFound` | `Get`, `One`, `First`, `IntoOne` with no row; `DeleteRow` that deleted nothing; `Save` without a version column that matched nothing |
| `keel.ErrNotSingular` | `One` and `IntoOne` with more than one row |
| `keel.ErrConflict` | `Save` that found another version; the error is a `*keel.ConflictError` with `Table` and `Version` |
| `keel.ErrNoScope`, `ErrAllTenantsRequired`, `ErrSystemScopeRequired`, `ErrTenantMismatch` | scope violations; see [Scope errors](#scope-errors) |
| `keel.ErrUnbounded` | `List` without `Limit` |
| `keel.ErrReadOnly` | a write, a `Write` or a `Savepoint` inside a `Read` |
| `keel.ErrBudgetExceeded` | matched by a `*keel.BudgetError`: the terminal whose round trip took a `keel.Budget` past a limit, and every later round trip under it; see [Budgets and N+1 detection](#budgets-and-n1-detection) |
| `keel.ErrScopeMismatch` | a statement, `Read`, `Write`, `Savepoint` or `LockKey` that would join the context's transaction under another scope than the one it began with; use `keel.Detached()` |
| `migrate.ErrChanged` | `Up` and `Plan` when an applied migration's file changed |
| `migrate.ErrOutOfOrder` | `Up` and `Plan` when a pending migration is older than the newest applied one |
| `migrate.ErrBaselineGap` | `Up` and `Plan` when the database's history ends before a baseline |
| `migrate.ErrBehind` | `Require` and `RequireWait` when the database has not applied a version; the error is a `*migrate.BehindError` with `Want` and `Have` |

Test them with `errors.Is` (and `errors.As` for `*keel.ConflictError`): keel wraps them with the
table name. Misuse (an `Update` without `Where`, an `Into` whose struct does not match the picks) is
reported with plain errors.

A constraint violation (unique, foreign key, check, not-null, exclusion) comes back as a
`*keel.ConstraintError` wrapping the `*pgconn.PgError`: its `Kind`, `Table`, `Constraint`, the
constraint's `Columns` without the tenant columns, and, when `Config.Tables` lists the table (pass
`keeltables.All()`), the model (`Model`), the columns' JSON names (`Fields`) and labels (`Labels`),
and the `Message` the model declares for the constraint. `Open` reads the constraint columns of
every table `Config.Tables` lists in one catalog query, so a violation never needs a connection,
however busy the pool; a table it does not list, or one created after `Open`, is read the first time
one of its constraints is violated, on a pool connection of its own, and cached. The columns are
the key columns in order, then the columns an index's expressions and predicate read, so
`lower(code)` reports `code` and `WHERE is_default` reports `is_default`. A `Field` on the declared
constraint replaces the resolved fields, for a key led by a reference column or a column hidden from
JSON.

A model declares the constraints whose violation should read well with a `Constraints` method;
keel gen records them in the generated table and `keel.Verify` reports a name the table lacks, so a
renamed constraint fails CI instead of quietly losing its message:

```go
func (Location) Constraints() []keel.Constraint {
    return []keel.Constraint{
        {Name: "idx_locations_code", Message: "A location with this code already exists"},
        {Name: "fk_locations_location_category", Message: "The location category does not exist"},
    }
}

var ce *keel.ConstraintError
if errors.As(err, &ce) {
    return fieldError(ce.Fields, ce.Message) // "code", "A location with this code already exists"
}
```

Other Postgres errors pass through as `*pgconn.PgError`. Helpers, which see through a
`ConstraintError`:

| Helper | Reports |
|---|---|
| `keel.IsUnique(err, constraints...)` | a unique violation (`23505`), optionally on one of the named constraints |
| `keel.IsForeignKey(err, constraints...)` | a foreign key violation (`23503`) |
| `keel.IsCheck(err, constraints...)` | a check violation (`23514`) |
| `keel.IsExclusion(err, constraints...)` | an exclusion violation (`23P01`) |
| `keel.IsNotNull(err)` | a not-null violation (`23502`) |
| `keel.IsRetryable(err)` | a serialization failure (`40001`) or deadlock (`40P01`) |
| `keel.Code(err)` | the SQLSTATE, or `""` |
| `keel.ConstraintName(err)` | the constraint name, or `""` |

```go
err := fleetdb.Shipments.Create(ctx, db, sh)
switch {
case keel.IsUnique(err, "uq_shipments_pro"):
    // PRO number already used
case keel.IsForeignKey(err, "fk_shipments_customer"):
    // no such customer
case err != nil:
    return fmt.Errorf("create shipment (SQLSTATE %s): %w", keel.Code(err), err)
}
```

## Migrations

Migrations are SQL files, plus Go functions for data changes SQL cannot express; keel does not
generate them from models. `keel/migrate` applies them; `keel` is its command line.
[migrations-and-seeds.md](migrations-and-seeds.md) is the design and how it fits deploys.

### Files

```
migrations/
  20260101000000_init.sql
  20260102000000_shipment_status_index.sql
  R_shipment_views.sql
```

- A migration is `<version>_<name>.sql`. The version is a 14-digit UTC timestamp
  (`20261023143000`); `keel migrate create` takes it from the clock. Versions must be unique across
  every source. There are no down migrations: fix forward with a new migration, and rebuild a
  development database from its template (see [Templates](#templates)).
- `R_<name>.sql` is repeatable: it runs after the versioned migrations whenever its checksum
  changes, in name order. Use it for views, functions, RLS helpers and grants, written with
  `CREATE OR REPLACE` so they re-apply.
- A transactional migration runs in one transaction together with its row in `keel_migrations`.
- Directives at the top of a file, before any statement, change how it runs. An unknown or
  misplaced directive is an error.

| Directive | Effect |
|---|---|
| `--keel:notx` | runs outside a transaction, statement by statement, split at `--keel:split` lines |
| `--keel:lock_timeout 2s` | overrides the lock timeout (default 5s); `0` disables it |
| `--keel:statement_timeout 1m` | bounds each statement (none by default) |
| `--keel:no_retry` | fails on the first lock timeout instead of retrying |
| `--keel:baseline` | a squashed schema (see [Squash](#squash)) |
| `--keel:lint_ignore a,b` | skips the named lint rules for this file |

```sql
--keel:notx
--keel:lock_timeout 2s
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_shipments_status ON shipments (organization_id, business_unit_id, status);
--keel:split
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_workers_manager ON workers (organization_id, business_unit_id, manager_id);
```

Use `--keel:notx` for work Postgres refuses inside a transaction (`CREATE INDEX CONCURRENTLY`,
`DROP INDEX CONCURRENTLY`, `REINDEX CONCURRENTLY`, `VACUUM`). Put each such statement in its own
`--keel:split` part: Postgres runs several statements sent together as one implicit transaction.
Make notx statements idempotent (`IF NOT EXISTS`): a failure part-way leaves the earlier ones
applied. A `CREATE INDEX CONCURRENTLY` that fails leaves an invalid index behind, which
`IF NOT EXISTS` then skips; drop it before retrying.

### How Up applies

- Runs serialise across processes on an advisory lock, taken by polling: a waiting statement would
  hold a transaction open, and `CREATE INDEX CONCURRENTLY` in the run that holds the lock waits for
  every open transaction. Two replicas or a retried deploy job never apply a migration twice.
- Every migration runs with `lock_timeout` 5s (`SET LOCAL` in its transaction, or set for the
  session around a notx migration's statements and reset after). When a statement times out
  waiting for a lock (SQLSTATE 55P03), the migration (or the notx statement) is retried with
  backoff, five times by default. A migration that needs an `ACCESS EXCLUSIVE` lock therefore
  never queues behind a long report query while every request queues behind it.
- `Options.After` statements run once, in the last migration's transaction, after a run that
  applied anything: Trenova's `SELECT trenova_rls.reconcile()` becomes automatic. When the last
  migration is notx, or a run fails after applying some migrations, they run in a transaction of
  their own.
- `Options.AfterFunc(ctx, applied)` runs once after a run that applied anything, outside every
  transaction and after the migration lock is released, also when the run failed part-way (its
  error joins the run's). It is for work SQL cannot do: migration SQL is invisible to data
  versions, so an application with a result cache records the migrated tables as changed there
  (`keel.MarkChanged` under a system scope on a `DB` with data versions, then `Close`, which
  flushes and notifies every process).
- A pending migration older than the newest applied one is `ErrOutOfOrder`, unless
  `Options.AllowOutOfOrder`: that is the branch merged with an old timestamp.
- keel stores each migration's checksum. An applied file that changed is `ErrChanged` before
  anything runs: add a new migration instead. After a comment-only edit, `migrate repair <version>`
  accepts the new checksum. Repeatable migrations re-run instead.
- `keel_migrations` holds `version`, `name`, `source`, `kind` (versioned, repeatable, go,
  baseline), `checksum`, `applied_at` and `duration_ms`, in `public` or `Options.Schema`.

### Command line

`keel` runs every migration command; in a terminal runs draw a live view, destructive commands ask
first and `keel` alone opens a console, and anywhere else (CI, a Job, a pipe) or with `-plain` each
event is one line:

```sh
export KEEL_DSN=postgres://localhost:5432/app
keel migrate create add_shipment_holds                 # migrations/20261023143000_add_shipment_holds.sql
keel migrate create shipment_status_idx -kind notx     # starts with --keel:notx
keel migrate create shipment_views -kind repeatable    # migrations/R_shipment_views.sql
keel migrate plan                                      # what up would do
keel up                                                # apply: the deploy step
keel migrate up -to 20261023143000 -dry-run            # print the SQL up to a version
keel migrate status                                    # applied, pending, changed, repeatable-due, missing
keel migrate history                                   # applied migrations, oldest first
keel migrate check                                     # exit 3 on out-of-order, changed or missing migrations
keel migrate check -base-dir ../main/migrations        # the same against the base branch, without a database
keel migrate lint                                      # lint pending migrations; -all (or no database) for every one
keel migrate baseline 20261023143000 -yes              # adopt an existing database
keel migrate repair 20261023143000 -yes                # accept an applied file's new checksum
keel migrate squash -yes -dump "docker exec pg pg_dump -U app -d {db}"
keel migrate stamp -out schema/version_gen.go [-check]
keel template [build | prune]                          # template databases
keel reset -yes                                        # recreate the database from the current template
keel doctor                                            # connection, settings, extensions, locks, long queries
keel                                                   # the console, in a terminal
```

Global flags go before or after the command: `-dsn` (default `$KEEL_DSN`), `-dir [name=]path`
(repeatable; default `migrations`), `-schema`, `-lock-timeout` (`0` for none), `-retries` (`0` for
none), `-allow-out-of-order`, `-after statement` (repeatable), `-json`, `-plain`, `-yes` and
`-yes-production`. With `-json`, runs write one JSON object per event and the other commands one
JSON document. Exit codes: `1` failure, `2` usage error or refusal, `3` a check found problems
(`check`, `lint` on an error finding, `stamp -check`, `doctor`). Destructive commands (`squash`,
`baseline`, `repair`, `template prune`, `reset`) need `-yes` without a terminal and refuse a
database marked production unless `-yes-production` is given; `reset` refuses it always.

The same commands, with seeds, make an application's own database binary: one
[`keelcli.App`](../keelcli/README.md) names the migrations, the seeds and the database
configuration, and keelcli builds the rest:

```go
func main() {
    keelcli.Main(keelcli.App{
        Name:       "trenova-db",
        Config:     dbconfig.FromEnv, // func() (keel.Config, error), called on first use
        Migrations: migrate.Sources(migrate.Source{Name: "core", FS: schema.Migrations}),
        Migrate:    migrate.Options{After: []string{"SELECT trenova_rls.reconcile()"}},
        Seeds:      reference.Seeds,
        Tenants:    tenants.All,
        System:     sysctx.With,
    })
}
```

### Go API

Embed the files and apply them from a deploy step, not from the server (see
[The server's view](#the-servers-view)):

```go
//go:embed migrations/*.sql
var migrationFiles embed.FS

dir, err := fs.Sub(migrationFiles, "migrations")
if err != nil {
    return err
}
sources := migrate.Sources(
    migrate.Source{Name: "core", FS: dir, Funcs: []migrate.Func{{
        Version: "20261024091500",
        Name:    "backfill_shipment_holds",
        Up: func(ctx context.Context, tx pgx.Tx) error {
            _, err := tx.Exec(ctx, "UPDATE shipments SET hold = false WHERE hold IS NULL")
            return err
        },
    }}},
    migrate.Source{Name: "cloud", FS: cloudDir},
)
m, err := migrate.Open(ctx, os.Getenv("KEEL_DSN"), sources, migrate.Options{
    After:    []string{"SELECT trenova_rls.reconcile()"},
    Observer: func(e migrate.Event) { log.Println(e.Kind, e.Index, e.Total) },
})
if err != nil {
    return err
}
defer m.Close()

res, err := m.Up(ctx, migrate.UpOptions{})
switch {
case errors.Is(err, migrate.ErrChanged):
    return errors.New("an applied migration was edited; add a new migration instead")
case errors.Is(err, migrate.ErrOutOfOrder):
    return errors.New("a migration is older than the newest applied one; give it a newer version")
case err != nil:
    return err
}
log.Printf("applied %d migrations in %s", len(res.Applied), res.Duration)
```

- `migrate.Sources(...)` returns a `Set`; a read error, a duplicate version or a migration older
  than a baseline travels with it and is returned by `Open`. `Set.Version()` is the newest version
  and `Set.Fingerprint()` hashes every migration.
- A `Func` sets `Up` (runs in the migration's transaction) or `UpNoTx` (gets the `*pgx.Conn`).
- `migrate.New(conn, sources, opts)` uses a `*pgx.Conn` you own.
- `m.Plan(ctx, migrate.UpOptions{To: v})` returns the baselines Up would mark, the pending and
  repeatable migrations it would run and the After statements, or the error Up would return;
  `UpOptions{DryRun: true}` does the same through `Up`.
- `m.Status(ctx)` returns each migration's `State`, applied time and duration; `m.History(ctx)` the
  rows of `keel_migrations`; `m.Check(ctx)` and `migrate.CheckAgainst(set, base)` a `CheckReport`.
- `m.Baseline(ctx, v)` records every migration up to `v` as applied without running it;
  `m.Repair(ctx, v)` returns the old and new checksums.
- `Options.LockTimeout`, `Retries` and `RetryBackoff` tune lock handling (negative disables the
  first two); `Options.Schema` moves `keel_migrations`.

### Progress events

`Options.Observer` receives an `Event` for each step of a run, synchronously on the goroutine
running `Up`, so it must not block:

| Kind | When | Fields |
|---|---|---|
| `LockWait` | about every second while another run holds the lock | `Duration` waited, `Blockers` |
| `LockAcquired` | the run holds the lock | `Duration` waited |
| `MigrationStart`, `MigrationEnd` | around each migration | `Migration`, `Index`, `Total`; end: `Duration`, `Err` |
| `Statement` | before each statement of a notx migration | `Statement` of `Statements`, `SQL` |
| `Retry` | before a retry after a lock timeout | `Attempt`, `Blockers`, `Err` |
| `BaselineMarked` | a baseline recorded without running | `Migration` |
| `After` | before each After statement | `SQL` |
| `RunEnd` | Up returns | `Duration`, `Err` |

A `Blocker` names a backend holding the lock the run waits for: `PID`, `Application`, `State`,
`Mode`, `Age` and `Query`, from `pg_locks`, `pg_blocking_pids()` and `pg_stat_activity`. On a
lock timeout keel finds them from a second connection while the statement waits, so the log says
which report query held the table. keelcli renders the events as a live view, as lines or as JSON
lines.

### Squash

`keel migrate squash` applies the migrations to a scratch database, dumps its schema with
`pg_dump --schema-only --no-owner --no-privileges`, strips what does not re-apply in a migration
(psql meta-commands such as `\restrict`, session settings, extension comments, version comments),
verifies the result by applying it to a second scratch database and comparing the dumps, and
replaces the migrations with one file, `<newest version>_baseline.sql`, marked `--keel:baseline`.
Repeatable migrations stay.

- A database whose history reaches the baseline's version records it as applied without running
  it (`BaselineMarked`); an empty database runs it. A database whose history ends before it is
  `ErrBaselineGap`: apply the squashed migrations from an earlier release first.
- `-dump` is the pg_dump command, which must match the server's major version; `{db}` and `{dsn}`
  are replaced by the scratch database, as in `-dump "docker exec pg pg_dump -U app -d {db}"`.
  `-admin` names the server for scratch databases (default `KEEL_ADMIN_DSN`, or the database's DSN
  on the `postgres` database); `-to` squashes up to a version.
- From Go: `migrate.Squash(ctx, migrate.SquashOptions{Admin, Dirs, To, Dump, Options})`.

### The server's view

The server links one generated file instead of its migrations:

```sh
keel migrate stamp -out schema/version_gen.go          # const Version, const Fingerprint
keel migrate stamp -out schema/version_gen.go -check   # in CI: exit 3 when stale
```

```go
if err := migrate.RequireWait(ctx, pool, schema.Version, time.Second); err != nil {
    return err // errors.Is(err, migrate.ErrBehind): the database has not applied Version
}
```

`migrate.Require(ctx, q, version)` reads `keel_migrations` once: a database that has applied
`version`, or is ahead of it, passes, which is what a rolling deploy needs; a database behind it, or
without the table, returns a `*migrate.BehindError` (wrapping `ErrBehind`) with the newest version
it has. `RequireWait` polls until the deploy job catches up or `ctx` ends. `q` is a
`*pgxpool.Pool`, `*pgx.Conn` or `pgx.Tx`; the table is found through the search path.

### Templates

```go
dsn, drop, err := migrate.Template(ctx, migrate.TemplateOptions{
    Admin:   adminDSN, // any database on the server; the role needs CREATEDB
    Sources: sources,
    Key:     seeds.Fingerprint(), // joins the template's identity when Prepare is set
    Prepare: func(ctx context.Context, dsn string) error { return seed(ctx, dsn) },
})
defer drop(ctx)
```

The first call for a set and key builds `keel_tpl_<hash>` under an advisory lock, so parallel test
packages build it once between them: it migrates, runs `Prepare`, and marks the database a
template that takes no connections. Every call returns a fresh database copied from it with
`CREATE DATABASE … TEMPLATE … STRATEGY FILE_COPY`, in milliseconds. `migrate.Templates(ctx, admin)`
lists templates with their size and creation time, `migrate.PruneTemplates(ctx, admin, keep)` drops
the others, `migrate.DropTemplate(ctx, admin, name)` drops one, and
`migrate.ResetFromTemplate(ctx, admin, dbname, template)` recreates a database from a template.

`keel template build` builds the current template of the migrations (an application's binary adds
its reference seeds), `keel reset` recreates the database from it, and `keel template prune` drops
the stale ones. A template's key starts with the binary's name, so a binary lists and prunes only
its own templates, never another application's or those a test harness built with keys of its
own.

### Lint

`keel migrate lint` and `migrate.Lint(migrations, rules...)` check SQL lexically, with comments and
literals removed; a table not created earlier in the same migration counts as existing.

| Rule | Flags |
|---|---|
| `index-concurrently` | `CREATE INDEX` without `CONCURRENTLY` on an existing table |
| `not-null-default` | `ADD COLUMN … NOT NULL` without a `DEFAULT` on an existing table (error) |
| `notx-mixed` | `CONCURRENTLY` or `VACUUM` in a transaction, a notx statement sharing a split part (errors); `ALTER TYPE … ADD VALUE` beside other statements |
| `lock-heavy` | `ALTER COLUMN … TYPE`, `SET NOT NULL`, `ADD CONSTRAINT` that validates or builds an index under the table lock, `VALIDATE` in the migration that added it `NOT VALID` |

Add rules with `migrate.LintRule{Name: "tenant-rls", Check: func(m migrate.Migration) []migrate.Finding { … }}`,
and skip one for a file with `--keel:lint_ignore`.

## Seeds

Package `github.com/emoss08/keel/seed` applies the data a database needs: **reference data**
production needs (states, hazmat references, roles) and **fixtures** for development (test
organizations, workers, shipments). [migrations-and-seeds.md](migrations-and-seeds.md) explains
the design and how the binaries that run seeds are laid out.

| Kind | Runs | Re-runs |
|---|---|---|
| `seed.Reference` | everywhere, on every deploy | when its fingerprint changes: its name, `Version`, input files or `Fingerprint()` result |
| `seed.Fixture` | development and test databases; refused on a database marked production | never: reset the database |

### Declaring seeds

Seeds live in a `seed.Registry`, usually a package variable filled at initialisation. `Add` copies
the seed and returns a pointer that other seeds name in `After`:

```go
//go:embed data
var data embed.FS

var Seeds = seed.NewRegistry()

var Plants = Seeds.Add(seed.Seed{
    Name:   "plants",
    Kind:   seed.Reference,
    Inputs: seed.Files(data, "data/plants.yaml"),
    Run: func(ctx context.Context, r *seed.Run) error {
        plants, err := seed.YAML[[]*fleet.Plant](r, "data/plants.yaml")
        if err != nil {
            return err
        }
        p := fleetdb.Plants
        if _, err := p.Upsert(ctx, r.DB, plants, keel.OnConflict(p.Code)); err != nil {
            return err
        }
        r.Count(int64(len(plants)))
        return nil
    },
})

var CustomerDefaults = Seeds.Add(seed.Seed{
    Name:        "customer_defaults",
    Kind:        seed.Reference,
    After:       []*seed.Seed{Plants},
    PerTenant:   true,
    Fingerprint: defaults.Fingerprint, // func() string over the code-built defaults
    Timeout:     30 * time.Second,
    Run: func(ctx context.Context, r *seed.Run) error {
        _, err := fleetdb.Customers.CreateOrSkip(ctx, r.DB, defaults.Customers(),
            fleetdb.Customers.OrganizationID, fleetdb.Customers.BusinessUnitID, fleetdb.Customers.Code)
        return err // ctx carries r.Tenant's scope
    },
})
```

| Field | Meaning |
|---|---|
| `Name` | stable identity, the key of the seed's bookkeeping row |
| `Kind` | `seed.Reference` or `seed.Fixture`; required |
| `Description` | what the seed writes, for status output |
| `After` | seeds that run first; they must be in the same (merged) registry |
| `Inputs` | files the seed reads: `seed.Files(fsys, paths...)`; a path may be a glob (`data/*.csv`) |
| `Fingerprint` | `func() string` whose result joins the fingerprint, for data built from code |
| `Version` | joins the fingerprint; bump it when a change to `Run` changes what it writes |
| `PerTenant` | `Run` runs once per tenant, each in its own transaction with the tenant's scope |
| `Timeout` | bounds one run (one tenant of a `PerTenant` seed) |
| `Run` | the work; idempotent upserts for reference data, plain creates for fixtures |

`seed.Merge(reference.Seeds, fixtures.Seeds)` combines registries, for a development binary that
applies both. `Registry.Validate`, `Plan` and `Apply` report every problem at once (`errors.Join`):
missing or duplicate names, a missing `Kind` or `Run`, `After` seeds outside the registry, cycles,
a reference seed after a fixture, and unreadable inputs. `Registry.Seeds()` returns the seeds in
dependency order, each with its `After`, for a tool that draws the graph.

### Applying

```go
report, err := reference.Seeds.Apply(ctx, db, seed.Options{
    Environment: seed.Production,
    System:      withSystem, // func(ctx) ctx: adds the application's system scope
    Tenants:     tenants.All, // func(ctx, *keel.DB) iter.Seq2[seed.Tenant, error]
    Parallel:    8,
    Observer:    progress, // func(seed.Event)
})
```

- **What runs.** A reference seed runs when its fingerprint differs from the stored one; a fixture
  runs when it has never run. An unchanged seed costs one hash and one read.
- **Transactions.** A seed runs in one `keel.Write` under the `System` context, bounded by its
  `Timeout`, and its row in `keel_seeds` is written in the same transaction: it applies and is
  recorded, or leaves no trace. A panic in `Run` is an error that rolls back.
- **Order.** Seeds start once the seeds they run `After` have finished. With `Parallel` above 1,
  independent seeds run at once; `Parallel` bounds the transactions in flight, seeds and tenants
  together. It defaults to 1 because seeds that touch the same tables can deadlock or conflict,
  `Run` functions may share Go state, and the pool needs `Parallel + 1` connections.
- **Failure.** A failing seed stops the run from starting more seeds; seeds already running finish.
  `Apply` returns every failure joined, and the report marks seeds that never started with
  `seed.ErrAborted`.
- **Tenants.** A `PerTenant` seed lists tenants through `Options.Tenants` (called under the system
  scope when the seed starts, so tenants an earlier seed created are included) and runs one
  transaction per tenant under `Tenant.Context`, up to `Parallel` at once. Each tenant's row in
  `keel_seed_tenants` commits with its data; the seed's `keel_seeds` row is written once every
  tenant succeeded. A run that fails at tenant 4,000 of 9,000 stops there, and the next run skips
  the tenants already applied with the current fingerprint. A forced run reruns every tenant.
- **Locking.** A run holds a session advisory lock, polled so a waiting process holds no
  transaction open; a second process waits and reports `LockWait` events about every second,
  naming the backend that holds the lock. Run seeds against Postgres directly, not through a
  transaction-mode pooler.
- **Bookkeeping.** `Apply` creates `keel_seeds`, `keel_seed_tenants` and `keel_meta` on first use.
  They hold no tenant data and are not tenant tables.

`seed.Tenant` is `{Key, Name, Context}`: `Key` is stable and unique, `Context` returns the context
with the tenant's scope, the one `keel.Config.Tenant` reads:

```go
func All(ctx context.Context, db *keel.DB) iter.Seq2[seed.Tenant, error] {
    return func(yield func(seed.Tenant, error) bool) {
        orgs, err := orgdb.Organizations.AllTenants().Limit(100_000).List(ctx, db)
        if err != nil {
            yield(seed.Tenant{}, err)
            return
        }
        for _, o := range orgs {
            t := seed.Tenant{Key: string(o.ID), Name: o.Name, Context: func(ctx context.Context) context.Context {
                return withTenant(ctx, o.ID, o.BusinessUnitID)
            }}
            if !yield(t, nil) {
                return
            }
        }
    }
}
```

| Option | Effect |
|---|---|
| `Environment` | the environment expected; a database marked otherwise fails with `seed.ErrEnvironment`, an unmarked one is marked |
| `System` | adds the system scope to the context of non-tenant seeds and the bookkeeping |
| `Tenants` | lists the tenants of `PerTenant` seeds; required when one runs |
| `Parallel` | transactions in flight at once; default 1 |
| `Only` | the seeds named and the seeds they run `After`; the rest are `Excluded` |
| `Force` | runs the seeds named in `Only` (or all) whatever their state; never fixtures on production |
| `DryRun` | plans and reports without the lock or any write |
| `Observer` | receives progress events |

The `Report` lists every registered seed in dependency order with its `Reason`, whether it `Ran`,
the tenants it applied, its rows, duration and error. It is filled as far as the run got even
when `Apply` returns an error.

### Plans and status

`Registry.Plan(ctx, db, opts)` returns what `Apply` would do, without the lock or writes: each seed
with whether it runs and why.

| Reason | Runs | Meaning |
|---|---|---|
| `seed.New` | yes | never applied |
| `seed.InputChanged` | yes | an input file was added, removed or edited; `PlanItem.Changed` names the paths |
| `seed.VersionChanged` | yes | `Version` differs |
| `seed.CodeChanged` | yes | `Fingerprint()` returns a different value |
| `seed.Forced` | yes | `Options.Force` |
| `seed.UpToDate` | no | applied with the current fingerprint |
| `seed.FixtureApplied` | no | a fixture that already ran |
| `seed.Excluded` | no | not selected by `Only` |
| `seed.Refused` | no | a fixture on a production database |

`keel_seeds` stores each input's hash, so a plan names the file that changed, and
`PlanItem.Detail` explains every change found. `Registry.Status(ctx, db)` returns each registered
seed's state (`seed.Applied`, `seed.Pending`, `seed.Changed` with its reason) with when it was
applied, how long it took, its rows and the tenants applied, followed by `seed.Removed` entries for
seeds the database records that are no longer registered. `Registry.Prune(ctx, db)` deletes those.

### Environments

```go
err := seed.MarkEnvironment(ctx, db, seed.Production, false) // once, at the first deploy
env, err := seed.Environment(ctx, db)                         // "" when unmarked
```

A database records its environment (`seed.Production`, `seed.Staging`, `seed.Development`,
`seed.Test`) in `keel_meta` once; marking it again with another environment is
`seed.ErrEnvironment` unless forced. `Apply` with `Options.Environment` marks an unmarked database.
A run that would apply a fixture to a database marked production, or with `Environment:
seed.Production`, fails with `seed.ErrRefused` before anything runs, whichever binary runs it.
`seed.ParseEnv` reads a command-line flag.

### Inside Run

| Call | Does |
|---|---|
| `r.DB`, `r.Seed`, `r.Tenant`, `r.Environment` | the database, the seed, the tenant (`PerTenant`), the run's environment |
| `r.Rand()` | a PCG generator seeded from the seed's name, fingerprint and tenant key: the same data on every machine |
| `r.Count(n)` | adds to the rows the report shows; feed it `Copy`, `CreateOrSkip`, `Upsert` results |
| `seed.YAML[T](r, path)` | decodes YAML with `github.com/goccy/go-yaml`; a duplicated key is an error |
| `seed.JSON[T](r, path)` | decodes JSON with `keel.Config.JSON`, or `encoding/json` |
| `seed.CSV[T](r, path)` | decodes every CSV record into a struct `T` |
| `seed.CSVEach(r, path, fn)` | streams records one at a time, for files too large to hold |
| `r.Open(path)`, `r.ReadFile(path)`, `r.Inputs()` | raw access to the declared inputs |

The readers read only the seed's declared `Inputs`; any other path is `seed.ErrUndeclaredInput`,
so every file that shapes the data is in the fingerprint.

CSV maps the header to exported fields by `csv:"name"` tag or the field name in snake case
(`ProNumber` reads `pro_number`; `csv:"-"` skips a field). A column with no field is an error, so a
misspelled header cannot drop data. Fields may be strings, booleans, integers, floats,
`time.Duration`, `time.Time` (RFC 3339 or `2006-01-02`), `encoding.TextUnmarshaler`
implementations, or pointers to them; an empty cell leaves a field zero and a pointer nil. Large
files stream into `Copy` in batches:

```go
batch := make([]*fleet.Plant, 0, 5000)
flush := func() error {
    n, err := fleetdb.Plants.Copy(ctx, r.DB, batch)
    r.Count(n)
    batch = batch[:0]
    return err
}
err := seed.CSVEach(r, "data/plants.csv", func(p fleet.Plant) error {
    batch = append(batch, &p)
    if len(batch) == cap(batch) {
        return flush()
    }
    return nil
})
if err == nil && len(batch) > 0 {
    err = flush()
}
```

### Values between seeds

A typed key passes a value from one seed to those that run after it, within one `Apply`:

```go
var DevOrg = seed.Key[*tenant.Organization]("dev-org")

seed.Put(r, DevOrg, org)    // in the seed that creates it
org := seed.Must(r, DevOrg) // in a seed that runs After it
org, ok := seed.Get(r, DevOrg)
```

`Must` panics, failing the seed with a message that names the key and the seed, when no seed put
the value in this run. Values do not outlive the `Apply`, and a seed can run without the seed that
puts a value (that one is up to date, or a fixture already applied), so a seed that may run alone
reads with `Get` and loads the value from the database when it is absent.

### Progress events

`Options.Observer` receives a `seed.Event` for each step: `LockWait` (with the time waited and the
holding backend), `LockAcquired`, `Planned` (with the plan), then per seed `SeedStart` (with its
`Reason`, `Index` of `Total`, and `Tenants` to run), `TenantStart`/`TenantEnd` (with the tenant's
key, `TenantsDone`, rows, duration and error), `SeedEnd` (rows, duration, error), and finally
`RunEnd`, always last. The observer is called from the goroutine running the seed, so with
`Parallel` above 1 from several at once: make it safe for concurrent use, and do not block in it.

### Command line

An application's database binary runs its seeds through [keelcli](../keelcli/README.md): with
`Seeds` set on the `keelcli.App`, `up` migrates and then applies the seeds, and the seed commands
appear:

```sh
trenova-db up -env production       # deploy: migrate, mark the database, apply reference seeds
trenova-devdb seed plan             # which seeds would run and why (input changed, version changed, ...)
trenova-devdb seed apply            # apply what changed, with a live view in a terminal
trenova-devdb seed apply -force us_states
trenova-devdb seed status           # state, reason, rows and tenants; orphaned records last
trenova-devdb seed prune -yes       # delete the records of seeds no longer registered
trenova-devdb reset -yes            # back to migrations and reference seeds, no fixtures
```

`-env` marks an unmarked database and refuses one marked otherwise. A fixture against a database
marked production fails the run before anything applies, and the destructive commands refuse it
without `-yes-production`. Per-tenant seeds show their progress per tenth of the tenants in plain
output and as a progress bar in a terminal.

## Performance notes

### Prepared statements

- Statement text never contains values, so each query shape is one prepared statement per connection.
  `In` binds one array, so a list of 3 or 3,000 IDs is the same statement. Keep shapes few: build
  `Raw` text from constants and pass values as arguments.
- With `PlanCacheAuto`, Postgres reuses a generic plan when it is no worse than the custom plans of
  the first executions. When one statement serves tenants of very different sizes, use
  `CustomPlan()` on that query, or `PlanCacheCustom` for the whole DB.
- `StatementCacheCapacity` (default 512) should exceed the number of distinct statement shapes a
  connection runs, or statements are re-prepared.

### Round trips

- A read is one round trip, its to-many loads included (see [One round trip](#one-round-trip)),
  plus one per level that loads per level. A page and its total share one.
- Independent reads of one request share a round trip with [`keel.Batch`](#batching-reads), their
  relation loads included.
- With `OnBegin` set, a lone terminal is still one round trip: `BEGIN`, the `OnBegin` statements,
  the statement and `COMMIT` are pipelined. Across a network each saved round trip is worth its
  latency; see [benchmarks.md](benchmarks.md).
- Inside `Read` or `Write`, each terminal is a round trip; `BEGIN` rides on the first.
- Belongs-to and has-one loads are joins and cost nothing extra; prefer them to separate queries.

### Bulk writes

- `CreateMany`, `Upsert` and `CreateOrSkip` with many rows send one array per column through
  `unnest`, 10,000 rows per statement, one statement text for any row count, all chunks in one round
  trip. A loop of `Create` calls is a round trip per row.
- `Copy` streams rows through binary `COPY` and beats `CreateMany` from many thousands of rows, at
  the price of reading nothing back (see [Copy](#copy)).
- Bulk `Update` and `Delete` are one statement whatever the row count, unless hooks need the rows
  (see [Table hooks](#table-hooks)).

### Replicas and data versions

- Both are free when off: no goroutine, no extra statement, no allocation on a terminal.
- A read on a marked context costs a context lookup and a scan of the replicas' atomics. It is
  still one round trip; a read checked against its context's commit position carries the check in
  the same round trip, and costs a second one, on the primary, only when the replica is behind.
- A write on a marked context reads its commit position in the `COMMIT` round trip, so a lone
  write sends `BEGIN` and `COMMIT` with its statement. `Copy` begins its implicit transaction in a
  round trip of its own.
- Mark the contexts of whole requests and reports with `OnReplica`, not single queries: the commit
  positions live on the context, so one mark covers every read after every write of the request.
- The lag tracker costs one query per replica and one on the primary per second per `DB`, which
  keeps a connection of each pool open.
- Data versions cost one upsert per changed (tenant, table) pair per `Interval`, whatever the number
  of writes, in statements of at most 5,000 pairs. With thousands of tenants writing every second,
  that is thousands of row updates per second on a small table: raise `Interval` if the caches
  that read the versions can be that stale.
- Recording a pair costs a map insert per write terminal under a tenant scope; under a system
  scope, a key per written row (or a `RETURNING` of the distinct tenants, for bulk writes).

### Indexes

- Lead every index on a tenant table with its tenant columns: every statement compares them with
  constants.
- **Keyset pages:** keel orders by the requested columns, then the primary key columns, and compares
  the position as a row when it can. For `Order(s.CreatedAt.Desc())` on a table whose key is
  `(id, organization_id, business_unit_id)`, an index on
  `(organization_id, business_unit_id, created_at DESC, id DESC)` serves both the filter and the
  order:

```sql
CREATE INDEX idx_shipments_created ON shipments (organization_id, business_unit_id, created_at DESC, id DESC);
```

  Keep page orders on non-nullable columns sorted one way, or the keyset expands to an `OR` chain.
- **To-many loads** filter on the foreign key: index `(tenant columns, fk)`, and add the load's
  order column when it has one: `(organization_id, business_unit_id, shipment_id, sequence)` for
  `m.Order(m.Sequence.Asc())`. Many-to-many loads filter the join table on its local key and join
  it to the target on the remote key and the tenant columns: index the join table
  `(local_fk, tenant columns)`.
- `CountUpTo`, `CountEstimate` and `Total(limit)` stop counting at the limit; prefer them to an
  exact `Count` on large tables. `CountEstimate` and `Total(limit).Estimate()` add an `EXPLAIN` to
  the same round trip (planning, no execution), so "about 1.2M" costs a capped count and a plan.

### Decoding

Rows decode straight from pgx's buffers into struct fields through generated accessors, with no
reflection. Rows of one statement are allocated in slabs, scan plans are cached per connection and
statement, and enum values and tenant strings are shared instead of allocated per row. A column
whose Go type is an enum (`Column.Enum`) matches each value's bytes against the generated constants
and stores the constant's string, whether the column is a Postgres enum or text; a value that is no
constant decodes as any string does, and a nullable enum field allocates only its pointer.

## Testing

### How keel's tests run

keel's integration tests ([main_test.go](../main_test.go)) need a Postgres server:

```sh
docker run -d --name keelpg -e POSTGRES_USER=keel -e POSTGRES_PASSWORD=keel -e POSTGRES_DB=keel -p 55432:5432 postgres:18-alpine
go test ./...                      # integration tests skip when Postgres is unreachable
KEEL_TEST_REQUIRE=1 go test ./...  # fail instead of skipping
```

- `KEEL_TEST_DSN` names the server (default `postgres://keel:keel@localhost:55432/keel`). On first
  use the tests copy a database from a template of `testdata/migrations` with `migrate.Template`
  (built once, then reused while the migrations are unchanged) and open a `keel.DB` on it;
  `TestMain` drops it.
- Each test works in a fresh random tenant (`newTenant()`), so tests never see each other's rows and
  need no cleanup between them.
- SQL golden tests ([render_test.go](../render_test.go)) open a `DB` on an address nothing listens
  on: pgxpool connects lazily, so `ToSQL` renders statements without a server.
- Round-trip tests count statements and round trips with an observer: statements of one round trip
  share a `Start`.
- Replica tests need a streaming replica of the test server and skip unless
  `KEEL_TEST_REPLICA_DSN` names it. `make replica` (`testdata/replica/replica.sh up`) creates one
  in Docker, `keelpg-replica` on port 55433, from a `pg_basebackup` of `keelpg` streaming from a
  replication slot, and prints its DSN; `make test-replica` starts it and runs the replica tests;
  `make replica-down` removes it and its slot. The tests open the replica on the test database
  (created on the primary, so it replicates) and pause replay with `pg_wal_replay_pause()` to
  test lag.

### Testing an application

The same pattern works for an application:

- In `TestMain` or a `sync.Once`, get a database per test binary from `migrate.Template` (see
  [Templates](#templates)), with `Prepare` loading reference data, and open the application's
  `keel.DB` with its real `Config`. Call the returned drop function when the run ends.
- Give each test its own tenant context. Tenancy keeps tests apart, so they can run with
  `t.Parallel()` against one database.
- Test system jobs with a system scope, and assert scope errors with `errors.Is`.
- Assert SQL with `ToSQL` on a `DB` that never connects, and assert round trips with a recording
  observer. `keeltest.Snapshot(t, sql, args)` compares a rendered statement, formatted by clause
  with its values listed, with `testdata/keel/<test name>.sql`; `KEEL_UPDATE=1 go test` (or the
  test binary's own `-update` flag) writes the snapshots, so a change in how keel renders is one
  reviewed diff of snapshots instead of edits to string assertions.
- Assert that an index serves a query with `keeltest.UsesIndex(t, ctx, db, q, "idx_name")`: it
  explains the generic plan with sequential scans switched off, so neither a small test table nor a
  value that only a custom plan sees can hide a query no index serves.
- To exercise `Retry`, return `&pgconn.PgError{Code: "40001"}` from the transaction function.

### Verifying the schema

Models and migrations are written separately, so they can drift: a column renamed in a migration,
a field added without one, a column made nullable under a non-pointer field. `keel.Verify` compares
generated tables with the live catalog and returns every difference as a `keel.Drift`:

```go
func Verify(ctx context.Context, db *keel.DB, tables []*schema.Table) ([]keel.Drift, error)

type Drift struct {
    Kind   keel.DriftKind // what was found
    Table  string         // the model's table, a join table or a relation's target
    Column string         // empty for a whole table
    Field  string         // Shipment.Weight, Shipment.Tags; the model alone for a whole table
    Detail string         // what differs
    Fix    string         // how to make them agree
}
```

| Kind | Reported when |
|---|---|
| `missing_table` | the model's table or view does not exist |
| `missing_column` | a stored field's column does not exist (the fix carries an `ALTER TABLE … ADD COLUMN`) |
| `primary_key` | the model's `pk` columns are not the table's primary key |
| `nullable` | a nullable column is read into a field that cannot hold NULL: not a pointer, a `Null…` scanner, `nullzero`, `jsonb`, `[]byte`, an array or a custom type |
| `generated` | a generated (or `GENERATED ALWAYS` identity) column's field is not `readonly` |
| `json` | a `jsonb` field's column is neither `json` nor `jsonb` |
| `type` | the field's Go kind cannot hold the column's type (`int16` on `bigint`, `string` on `integer`, `tsvector` on `text`), domains resolved to their base type |
| `join_table` | a many-to-many join table is missing, or lacks its key columns or the tenant columns its two tables share |
| `relation_key` | a relation's key column or target table does not exist, or its target was never linked |

Computed fields are skipped; `noselect` columns are not checked for NULL; views are checked for
their columns and types only, since Postgres records no primary key or nullability for them; a
custom (`sql.Scanner`) type is not type-checked. `Verify` reads the catalog in one statement on a
pool connection of its own, outside any transaction or scope.

The primary use is an integration test on the database the other tests use, built from the
migrations, so CI fails when models and migrations disagree:

```go
func TestSchemaMatchesModels(t *testing.T) {
    drifts, err := keel.Verify(context.Background(), testDB, keeltables.All())
    if err != nil {
        t.Fatal(err)
    }
    for _, d := range drifts {
        t.Error(d) // shipments.weight (Shipment.Weight): the column is nullable but …; fix
    }
}
```

`keelcli` exposes the same check as `verify` for an `App` that sets `Tables: keeltables.All`, with
plain or `-json` output and exit code 3 on drift, to run against a deployed database.

`keel.VerifyFuncs(ctx, db, decls...)` does the same for [declared database
functions](#declared-database-functions): each must exist as a plain function (not a procedure or
an aggregate), found through the search_path when its name carries no schema, with an overload that
takes the declared number of arguments (defaults and a variadic last argument counted) and returns
a type the declared result kind decodes, by `Verify`'s type rules (polymorphic results are not
checked). Drifts carry the function's name in `Table`:

| Kind | Reported when |
|---|---|
| `missing_function` | no plain function of that name exists |
| `function_arity` | no overload takes the declared number of arguments |
| `function_result` | the overloads of that arity return a type the result kind cannot hold (`text` declared as `Int64Expr`) |

### Plan guard

`keeltest.UsesIndex` asserts on the queries a test names; the plan guard checks every statement
the application really sends. Record the distinct statement texts while the test suite (or a
staging soak) runs, then explain each one against a database with realistic statistics:

```go
func TestMain(m *testing.M) {
    var observers []keel.Observer
    var closeShapes func() error
    if path := os.Getenv("KEEL_SHAPES"); path != "" {
        obs, closeFn, err := keel.RecordShapesFile(path)
        if err != nil {
            log.Fatal(err)
        }
        observers, closeShapes = append(observers, obs), closeFn
    }
    testDB = openTestDB(observers) // Config.Observers: observers
    code := m.Run()
    if closeShapes != nil {
        if err := closeShapes(); err != nil {
            log.Print(err)
            code = 1
        }
    }
    os.Exit(code)
}
```

```sh
rm -f /tmp/shapes.jsonl
KEEL_SHAPES=/tmp/shapes.jsonl go test ./...
keel plans -dsn "$STAGING_DSN" -shapes /tmp/shapes.jsonl -sort-limit
```

```text
findings select shipments (cost 48213.50, 1,204 rows)
  SELECT s.id, s.pro_number FROM shipments s WHERE s.organization_id = $1 AND s.bol = $2 …
  seq-scan sequential scan of public.shipments (about 2,400,000 rows)
findings select stops (cost 912.40, 25 rows)
  SELECT st.id FROM stops st WHERE st.organization_id = $1 ORDER BY st.planned_at LIMIT $2
  sort-under-limit sort by st.planned_at under a limit over public.stops (about 380,000 rows): the page is sorted in full, no index gives its order
unplanned select tmp_import
  SELECT id FROM tmp_import WHERE batch = $1
  relation "tmp_import" does not exist (SQLSTATE 42P01)
2 findings in 2 of 311 shapes planned; 1 shape could not be planned.
keelcli: plans found 2 findings
```

- `keel.RecordShapes(w)` is the same observer over any `io.Writer`; `keel.RecordShapesFile(path)`
  appends, so every test binary of `go test ./...` can write to one file, and its close function
  returns the first write error. Each line is a `keel.Shape`:
  `{"sql": "...", "op": "select", "table": "shipments", "args": 2}`. A text seen before costs a
  read-locked map lookup; only statements EXPLAIN can plan are written (no savepoints, `COPY`,
  DDL through `Exec`, advisory locks or `Explain`'s own statements). `keel.ReadShapes(r)` reads a
  file back with each distinct text once.
- `keel plans` runs `EXPLAIN (GENERIC_PLAN, VERBOSE, FORMAT JSON)` (PostgreSQL 16 or later) for
  every shape on one connection, outside transactions, with the placeholders unbound: the plan a
  prepared statement settles on, which a value-dependent partial index cannot rescue. A shape
  that holds more than one statement is never sent.
- `-seq-scan-rows N` (default 10000) flags a sequential scan of a relation whose
  `pg_class.reltuples` exceeds N; a table never analyzed counts as empty, so run `ANALYZE` (or use
  a database restored from production statistics) first, and `-seq-scan-rows -1` flags every
  sequential scan. `-max-cost C` flags a plan whose total cost exceeds C (default 0: no limit).
  `-sort-limit` flags a `Sort` under a `Limit`, directly or through `Gather Merge`, `Subquery
  Scan`, `Unique`, `Materialize`, `Result` or `Merge Append`: a page sorted in full because no
  index gives its order. It uses the same threshold: the sort is flagged only when the largest
  relation it reads (by any scan, index scans included) has more than `-seq-scan-rows` rows, so a
  settings or reference table sorted under a limit passes, and `-seq-scan-rows -1` flags every
  sort under a limit. An `Incremental Sort` passes.
- A shape EXPLAIN cannot plan (a temporary table, a parameter whose type Postgres cannot infer) is
  listed with its error and does not fail the run. Any finding exits 3, keelcli's "a check found
  problems"; `-json` writes one document with `shapes`, `findings`, `unplanned` and a report per
  shape (`sql`, `op`, `table`, `args`, `cost`, `rows`, `findings`, `error`).
- `plans` is a keelcli command, so an application's database binary has it too whenever its `App`
  sets `Config`, with the application's connection settings.

## Coming from Bun

| Bun | keel |
|---|---|
| `bun.BaseModel \`bun:"table:shipments,alias:sp"\`` | `keel.Table \`keel:"shipments"\``; aliases are derived |
| `bun:"id,pk,type:VARCHAR(100),notnull"` | `keel:"id,pk,autoid:shp_"` |
| `bun:"organization_id,pk"` plus tenant helpers | `keel:"organization_id,pk,tenant"` |
| `bun:"version,type:BIGINT"` plus `Version++`, `Where("version = ?")`, `CheckRowsAffected` | `keel:"version,version"` plus `Save` |
| `BeforeAppendModel` setting IDs and timestamps | `autoid`, `autocreate`, `autoupdate` |
| `BeforeAppendModel` validation, `AfterUpdate` audit | table hooks: `keel.OnCreate(db, s, keel.BeforeWrite, …)`, `keel.OnUpdate(…, keel.AfterWrite, …, keel.Needs(…))` |
| `AfterScanRow` | `AfterLoad` |
| `bun:"type:jsonb"` | `keel:"attributes,jsonb"` |
| `bun:",array"` | nothing; `[]string` and `[]int64` are arrays |
| `bun:",nullzero"` on a non-pointer field | `keel:"customer_id,nullzero"`; on a pointer field drop it, a nil pointer is NULL |
| `bun:",scanonly"` (generated column) | `keel:"stage_rank,readonly"`, plus `noselect` when it should not be read |
| `bun:",scanonly"` (a search rank, an aggregate or a joined value scanned onto the model) | `keel:"rank,computed"` plus `q.Compute(s.Rank, expr)` |
| `ColumnExpr("(SELECT count(*) …) AS loads")` | `Compute(c.Loads, keel.Scalar(s.Pick(keel.Count()).Where(…)))` |
| `bun:",default:..."` | nothing; set the value in Go |
| `bun:"-"` | `keel:"-"` |
| `bun:",soft_delete"` | not built in; an interceptor that adds `deleted_at IS NULL` |
| `bun:"rel:belongs-to,join:customer_id=id"` | `keel:"belongs:customer_id"` |
| `bun:"rel:has-one,join:id=worker_id"` | `keel:"one:worker_id"` |
| `bun:"rel:has-many,join:id=shipment_id"` | `keel:"many:shipment_id"` |
| `bun:"rel:belongs-to,join:source_pto_id=id,join:worker_id=worker_id"` | `keel:"belongs:source_pto_id\|worker_id"`; drop the `organization_id` and `business_unit_id` pairs, the tenancy adds them |
| `bun:"m2m:shipment_tags,join:Shipment=Tag"` | `keel:"m2m:shipment_tags.shipment_id.tag_id"` |
| deleting and re-inserting a parent's join rows | `keel.SetLinks(ctx, db, s.Tags(), sh.ID, tagIDs)` |
| a hand-written `LEFT JOIN LATERAL (… ORDER BY … LIMIT 1)` for a shipment's first stop | `keel:"pick:moves.stops,first=moves.sequence\|stops.sequence"` and `Load(s.OriginStop())` |
| labels, descriptions and enum values curated in a report catalog | derived by `keel gen` from names, doc comments and constants ([Derived metadata](#derived-metadata)) |
| `db.NewSelect().Model(&sh).WherePK().Scan(ctx)` | `s.Get(ctx, db, id)` |
| `db.NewSelect().Model(&rows).Where("sp.status = ?", st).Scan(ctx)` | `s.Where(s.Status.Eq(st)).Limit(n).List(ctx, db)` |
| `WhereGroup(" AND ", func(q) {... ScopeTenant ...})` | nothing; the scope applies the tenant |
| `.WhereOr(...)` | `keel.Or(...)` |
| `.Where("sp.pro_number ILIKE ?", "%"+q+"%")` | `s.ProNumber.ContainsFold(q)` (escaped) |
| `Where("sv @@ websearch_to_tsquery('simple', ?) \|\| websearch_to_tsquery('english', ?)", q, q)` | `s.SearchVector.Match(q, keel.SearchConfig("simple", "english"))` |
| `ColumnExpr("ts_rank(sv, …) AS rank")` | `Compute(s.Rank, s.SearchVector.Rank(q))` |
| `to_tsquery('simple', 'word:*')` built by hand for search as you type | `keel.SearchModes(keel.WebSearch, keel.Prefix)` |
| `bun.In(ids)` | `col.In(ids...)` |
| `bun.List(ids)` in `IN (?)` | `col.In(ids...)`, one array parameter |
| `?TableAlias`, `bun.Ident(col)` in raw fragments | typed columns, or expression arguments of `keel.Raw`; aliases are derived |
| `payload->'a'->>'b' = ?` | `s.Payload.Text("a", "b").Eq(v)` |
| `.For("UPDATE OF inv")` | `.Lock(keel.ForUpdate)`, which locks the base table (`FOR UPDATE OF` its alias) |
| a `*postgis.Point` (`sql.Scanner` and `driver.Valuer`) with `type:geography` | the same field; register the PostGIS codecs in `Config.AfterConnect` ([Enums, domains and decimals](#enums-domains-and-decimals)) |
| `.Value("geom", "ST_GeomFromGeoJSON(?)", js)` on an insert | a `Value` method that encodes the type, or `Update` with `ToExpr(keel.Raw("ST_GeomFromGeoJSON(?)", js))` in the same transaction |
| `.Order("sp.created_at DESC")` | `.Order(s.CreatedAt.Desc())` |
| `.Limit(n).Offset(m)`, `ScanAndCount` | `Page(ctx, db, keel.After(cursor, n).Total(cap))` (one round trip) |
| `.Count(ctx)`, `.Exists(ctx)` | `Count`, `CountUpTo`, `Exists` |
| `.Join("JOIN customers AS c ON c.id = sp.customer_id")` | a relation path filter (`s.Customer().Code.Eq(…)`) or `Join(c, c.ID.EqCol(s.CustomerID))` |
| `.Relation("Customer").Relation("Moves.Stops")` | `.Load(s.Customer(), s.Moves().Stops())` |
| `.Relation("Moves", func(q) { return q.Order("sm.sequence") })` | `.Load(m.Order(m.Sequence.Asc()))` |
| `.Relation("Customer", func(q) { return q.Column("name") })` | `.Load(cu.Pick(cu.Name))` |
| `.Where("EXISTS (SELECT 1 FROM moves …)")` | `m.Any(…)`, `keel.Exists(q)` |
| `.ColumnExpr("count(*)").Group("status")` scanned into a struct | `keel.Into[R](ctx, db, s.Group(s.Status).Pick(s.Status, keel.Count()))` |
| `ColumnExpr("count(*) FILTER (WHERE ...)")` | `keel.Count().Filter(pred)` |
| `.DistinctOn("customer_id")` | `.DistinctOn(s.CustomerID)` |
| `.With("busy", q).Table("busy")` | `keel.CTE("busy", cols, q)` plus `With(busy)` |
| `NewUpdate().With("_data", db.NewValues(&rows)).TableExpr("_data").Set(...)`, or `UPDATE … FROM unnest(?::text[], …)` | `keel.Unnest("data", cols, slices...)` plus `With(data).Join(data, …).Update(…)` |
| `.Union(q)`, `.UnionAll(q)` | `keel.Union(q1, q2)`, `keel.UnionAll(q1, q2)` |
| `.For("UPDATE NOWAIT")` | `.Lock(keel.ForUpdate, keel.NoWait)` |
| `db.NewInsert().Model(sh).Exec(ctx)` | `s.Create(ctx, db, sh)` |
| `db.NewInsert().Model(&rows).Exec(ctx)` | `s.CreateMany(ctx, db, rows)` |
| `.Returning("*")` | nothing; keel reads back `readonly` columns and the version |
| `db.NewUpdate().Model(sh).WherePK().Exec(ctx)` | `s.Save(ctx, db, sh)` |
| `db.NewUpdate().Model(sh).Column("status").WherePK()` | `s.Save(ctx, db, sh, s.Status)` |
| `db.NewUpdate().Model(sh).WherePK().ExcludeColumn("token")` | `s.SaveExcept(ctx, db, sh, s.Token)`; `created_at` needs nothing, `Save` never writes `autocreate` |
| `db.NewSelect().Model(&rows).ExcludeColumn("transcript")` | `s.Omit(s.Transcript)` |
| `db.NewUpdate().Model((*T)(nil)).Set("status = ?", v).Where(...)` | `s.Where(...).Update(ctx, db, s.Status.To(v))` |
| `db.NewDelete().Model(sh).WherePK().Exec(ctx)` | `s.DeleteRow(ctx, db, sh)` |
| `db.NewDelete().Model((*T)(nil)).Where(...)` | `s.Where(...).Delete(ctx, db)` |
| `.On("CONFLICT (...) DO UPDATE").Set("x = EXCLUDED.x")` | `s.Upsert(ctx, db, rows, keel.OnConflict(...).Update(s.X))` |
| `.Set("hits = sp.hits + 1, name = EXCLUDED.name")` on a conflict | `keel.OnConflict(...).Set(s.Hits.Increment(1), s.Name.ToExpr(s.Name.Excluded()))` |
| pgx `CopyFrom` on the underlying pool for large loads | `s.Copy(ctx, db, rows)`, with tenant columns, IDs, timestamps and hooks |
| `.On("CONFLICT (cols) DO NOTHING")` | `s.CreateOrSkip(ctx, db, rows, cols...)` |
| `.On("CONFLICT DO NOTHING")` | `s.CreateOrSkip(ctx, db, rows)` |
| `.On("CONFLICT (md5(endpoint)) DO UPDATE")` | `keel.OnConflict(keel.Raw("md5(?)", s.Endpoint)).Update(…)` |
| `INSERT … SELECT` through `NewInsert().Table(...)` | `s.InsertFrom(ctx, db, q, cols...)` |
| `db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {...})` | `keel.Write(ctx, db, func(ctx context.Context) error {...})`; pass `ctx`, not a `tx` |
| `bun.IDB` parameters to share a transaction | nothing; the transaction travels on the context |
| `NewRaw` / string fragments | `keel.Raw("…?…", col, value)` inside a query; whole statements through `keel.Exec`, `keel.QueryRows`, `keel.QueryAll` or `keel.QueryValue`, which join the context's transaction (unscoped) |
| `bun.Ident`, `bun.Safe` | an expression argument of `keel.Raw` |
| `sql.ErrNoRows` | `keel.ErrNotFound` |
| `pgdriver.Error` with `IntegrityViolation()` | `keel.IsUnique`, `IsForeignKey`, `IsCheck`, `Constraint` |
| `bun.QueryHook` | `keel.Observer` |

## Running alongside another data layer

An application moving to keel can keep writing through its old data layer while it moves, with
both in one transaction. The old layer must run on pgx (Bun runs on pgx through its stdlib driver,
`pgdriver` cannot): it begins the transaction on a pgx connection, and `keel.JoinTx` makes keel run
inside it.

```go
conn, err := pool.Acquire(ctx) // the pgxpool both layers share
defer conn.Release()
tx, err := conn.Begin(ctx)     // or the other layer's BeginTx on this connection
// … the other layer sets its scope (row-level security) and writes …

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
done(err == nil)
```

- Every keel operation on `kctx` runs on that connection, in the owner's transaction. keel sends no
  `BEGIN`, `COMMIT` or `Config.OnBegin` statements: the owner began the transaction and set its
  scope. `keel.Read` and `keel.Write` join it, `keel.Savepoint` nests in it, and replicas are not
  used.
- Scope checks, tenancy, interceptors, read filters, hooks and observers apply as in a keel
  transaction. A failing `AfterWrite` hook returns its error; rolling back is the owner's call.
- The joined transaction's scope is the scope of the context passed to `JoinTx`; keel work on
  `kctx` under another scope returns `ErrScopeMismatch`. `keel.Detached()` runs work in a
  transaction of keel's own instead, outside the owner's.
- `done` tells keel how the transaction ended. `done(true)` after `COMMIT` runs the `Committed`
  hooks and `AfterCommit` callbacks and moves the data versions of the tables keel wrote;
  `done(false)` after `ROLLBACK` drops them. Only the first call counts, and keel refuses to run on
  `kctx` afterwards instead of running outside the transaction.
- Writes the other layer makes are invisible to keel's data versions; call `keel.MarkChanged` with
  their tables.
- The connection belongs to one goroutine at a time, as any transaction does.

Seeds run the same way: `seed.Options.Transact` replaces the `keel.Write` around each seed (and
each tenant of a `PerTenant` seed). It begins the other layer's transaction, joins it, calls `fn`
and commits, so a seed's rows, the other layer's writes and the seed's bookkeeping row commit or
roll back together:

```go
opts.Transact = func(ctx context.Context, fn func(ctx context.Context) error) error {
    tx, err := bunDB.BeginTx(ctx, nil)
    if err != nil {
        return err
    }
    kctx, done := keel.JoinTx(ctx, db, pgxConnOf(tx)) // the pgx connection under Bun's stdlib driver
    if err := fn(kctx); err != nil {
        _ = tx.Rollback()
        done(false)
        return err
    }
    err = tx.Commit()
    done(err == nil)
    return err
}
```

The seed lock is a session lock on a connection of its own and needs no transaction.

## Analytics: report definitions and formulas

`keel/analytics` lets an application's users build their own reports. Two packages hold what a user
writes; neither runs SQL or touches the database:

- [`analytics/ir`](../analytics/ir) is the report definition: its types, JSON encoding, JSON Schema,
  versions and upgrades, validation, dashboard filter merging and drill-through.
- [`analytics/formula`](../analytics/formula) is the formula language of calculated columns and
  conditions.

The compiler turns a validated definition into SQL. [analytics.md](analytics.md) is the design.

### A definition

A definition is stored as JSON, and the JSON reads the way the report does:

```json
{
  "version": 2,
  "entity": "shipment",
  "columns": [
    {"id": "customer", "dimension": {"field": "customer.name"}},
    {"id": "month", "dimension": {"field": "shippedAt", "bucket": "month"}},
    {"id": "revenue", "measure": {"field": "totalCharge", "agg": "sum"}},
    {"id": "miles", "measure": {"field": "moves.distance", "agg": "sum"}},
    {"id": "rpm", "calc": "round([revenue] / nullif([miles], 0), 2)", "format": "money"},
    {"id": "share", "window": {"of": "revenue", "fn": "percent_of_total", "partition": ["month"]}},
    {"id": "mom", "window": {"of": "revenue", "fn": "change", "over": "month", "offset": 1}}
  ],
  "filters": {"and": [
    {"field": "shippedAt", "op": "last_n_days", "param": "window"},
    {"field": "status", "op": "not_in", "value": ["Canceled"]}
  ]},
  "having": {"and": [{"column": "revenue", "op": "gt", "value": 1000}]},
  "groupings": {"subtotals": ["customer"], "total": true},
  "top": {"n": 5, "by": "revenue", "per": ["month"]},
  "sort": [{"column": "revenue", "desc": true}],
  "limit": 500,
  "parameters": [{"name": "window", "type": "int", "default": 30}]
}
```

`ir.Decode` reads stored JSON of any supported version, rejects unknown keys and keeps numbers exact
(`json.Number`); `ir.Encode` writes it back in this shape. In Go the same report is a plain value:

```go
def := &ir.Definition{
    Version: ir.Version,
    Entity:  "shipment",
    Columns: []ir.Column{
        {ID: "customer", Dimension: &ir.Dimension{Field: "customer.name"}},
        {ID: "month", Dimension: &ir.Dimension{Field: "shippedAt", Bucket: ir.Every(ir.BucketMonth)}},
        {ID: "revenue", Measure: &ir.Measure{Field: "totalCharge", Agg: ir.AggSum}},
        {ID: "loads", Measure: &ir.Measure{Agg: ir.AggCount}},
        {ID: "per_load", Calc: formula.Of("[revenue] / [loads]")},
    },
    Filters: ir.And(
        ir.WhereParam("shippedAt", ir.OpLastNDays, "window"),
        ir.Where("status", ir.OpNotIn, []string{"Canceled"}),
    ),
    Top:        &ir.Top{N: ir.CountParam("n"), By: "revenue"},
    Parameters: []ir.Parameter{{Name: "window", Type: formula.Int, Default: 30}, {Name: "n", Type: formula.Int, Default: 10}},
}
```

**Columns.** Each column has an id (letters, digits, underscores) and exactly one kind:

| Kind | What it is | Options |
|---|---|---|
| `dimension` | groups by a field, given as a dotted path across relations | `bucket` (hour to year, or `{"param": …}`), `band` (`width`, or ascending `edges` with an open top range) |
| `measure` | aggregates a field: `count`, `count_distinct`, `sum`, `avg`, `min`, `max` | `filter`: narrows only this measure (`FILTER (WHERE …)`); `count` without a field counts rows |
| `calc` | a formula, as text or as an AST | row-level calcs group like dimensions; calcs over measures are aggregates |
| `window` | a value across the grouped rows from another column (`of`) | `running_total`, `percent_of_total`, `rank`, `dense_rank`, `moving_avg` (`size`), `change` and `percent_change` (`offset`), `first`, `last`; `over` orders, `partition` restarts |

Any column can carry a `transform` (round, ceil, floor, truncate, abs, scale, upper, lower, title, trim),
a `format` hint, a `display` spec (an object owned by `analytics/format`) and `hidden`, which computes
a column for other columns, `having` or `sort` without returning it.

**Filters** are trees of `{"and": […]}` and `{"or": […]}` groups. A condition compares a `field`, a
`calc` or a `column` with an `op` and a `value` or a `param`; a `calc` with no `op` is a condition when
it is true. `ir.OpsFor(t)` lists the operators of a value type, and the same table is in the generated
TypeScript. `between` is half-open: from the first value, up to the second. `having` compares output
columns (or aggregate calcs) after grouping.

**Layout.** `groupings` adds subtotal rows (each named dimension and the ones before it) and a grand
total, computed in the same statement as the rows. `top` keeps the first N rows per group. `pivot`
spreads measure columns across a field's values: listed (`values`, or `{"param": …}`), or discovered
when the report runs (`top: {"n", "by"}`), with `other` for the rest; a pivot cell is addressed as
`revenue:Completed` (and `revenue:__other__`) in `sort` and charts. `limit` and `page` (`size`, `after`)
bound the output.

**Parameters** have a type, `required`, a `default`, `multi`, `allowed` values and, for `ref`, the
`entity` they point at. They supply filter values, limits, top-N counts, buckets and pivot values,
always as bound values, never as identifiers.

**Charts** (bar, hbar, line, area, pie, donut, scatter, kpi, map) are presentation: they name output
columns and never change what is queried. `ir.ChartRules()` says what each type needs.

### Validation and errors

`ir.Validate(def, limits)` checks everything that needs no catalog: structure, ids, references between
columns, calculation cycles, formula syntax and levels, parameter binding, pivot and sort cells, charts
and the limits in `ir.Limits` (zero fields take `ir.DefaultLimits()`). `ir.Resolve(def, catalog, limits)`
does the same and checks the definition against the catalog: entities, relations and fields exist,
aggregations, operators, buckets, bands and transforms suit the field types, values fit their fields
(including enum values), formulas type-check, dimensions do not cross a to-many relation, and a to-many
measure's filter stays on its own rows. It returns an `*ir.Resolved` with each output column's type and
level, the resolved fields and the checked formulas the compiler renders.

The catalog is an interface, so the compiler implements it and tests use a fake:

```go
type Catalog interface {
    HasEntity(key string) bool
    EntityKeys() []string
    Field(entity, key string) (ir.FieldInfo, bool)
    FieldKeys(entity string) []string
    Edge(entity, name string) (ir.EdgeInfo, bool)
    EdgeNames(entity string) []string
}
```

Every error is an `*ir.Error` in an `ir.Errors` list: a code to translate, a message for a person, a
JSON pointer to the offending part and suggestions for unknown names, found by Damerau-Levenshtein
distance. A formula error also carries its line, column and span:

```text
/columns/2/dimension/field: unknown field "nam" on customer; did you mean "customer.name"?
/filters/and/1/value/0: unknown value "Canceld"; did you mean "Canceled"?
/columns/4/calc: 1:1: unknown column [revnue]; did you mean [revenue]?
```

### Formulas

A formula is a typed expression over the report's columns, the catalog's fields and the report's
parameters:

```text
round([revenue] / nullif([miles], 0), 2)
case when date_diff('day', {pickedUpAt}, {deliveredAt}) > 2 then 'late' else 'on time' end
concat({customer.name}, ' (', {customer.code}, ')')
sum({totalCharge}) / count()
```

- `[id]` is an output column, `{path.field}` a catalog field, `@name` a parameter.
- Literals: exact decimal numbers (`0.1` stays 0.1), `'text'` with `''` for a quote, `true`, `false`,
  `null`, `date '2026-01-31'` and `timestamp '2026-01-31T08:00:00Z'`.
- Arithmetic promotes int to decimal to float; `int / int` is decimal and dividing by zero gives null.
  Comparisons need comparable types, and `= null` is refused in favour of `is null`.
- Dates and times are computed in the report time zone: `date_trunc`, `date_diff`, `extract` and
  `today()` all bind the zone as a parameter.

The grammar, loosest binding first:

```text
expr     = or
or       = and { "or" and }
and      = not { "and" not }
not      = "not" not | compare
compare  = concat [ ("=" | "!=" | "<>" | "<" | "<=" | ">" | ">=") concat | "is" ["not"] "null" ]
concat   = additive { "||" additive }
additive = term { ("+" | "-") term }
term     = unary { ("*" | "/" | "%") unary }
unary    = ("-" | "+") unary | primary
primary  = number | string | "true" | "false" | "null" | ("date" | "timestamp") string
         | "[" column "]" | "{" path "}" | "@" name | name "(" [ expr { "," expr } ] ")"
         | "case" [ expr ] "when" expr "then" expr { "when" expr "then" expr } [ "else" expr ] "end"
         | "(" expr ")"
```

`formula.Parse` builds the AST, `formula.Check` types it against a `formula.Env`, and `formula.Format`
prints the canonical text, which parses back to the same tree. Each formula has a level: constant, row
(usable as a dimension and in filters), aggregate (over measures) or window. Mixing a per-row field with
per-group values outside an aggregate is an error that says which field to aggregate. A structured
editor can send the AST as JSON instead of text, as in
`{"op": "/", "args": [{"column": "revenue"}, {"column": "miles"}]}`; a definition stores whichever form
it was given. Formulas are capped in length, depth and size (`formula.Limits`).

Every function carries its signature, its result rule, whether it aggregates, and how it renders: a
SQL template with argument positions (`round({0}, {1})`), never text a user typed.
`formula.Coercion(from, to)` gives the conversion the checker asked for, such as `to_timestamp({0})`
for an epoch used as a timestamp.

### Formula reference

The tables are generated from the registry by `go generate ./analytics/ir`.

<!-- keel:formula-reference:start -->

| Operator | Precedence | Meaning |
|---|---|---|
| `a or b` | 10 | True when either side is true. |
| `a and b` | 20 | True when both sides are true. |
| `not a` | 25 | True when the operand is false. |
| `a = b` | 30 | Equal. |
| `a != b` | 30 | Not equal; <> is accepted too. |
| `a < b` | 30 | Less than. |
| `a <= b` | 30 | Less than or equal. |
| `a > b` | 30 | Greater than. |
| `a >= b` | 30 | Greater than or equal. |
| `a is null` | 30 | True when the operand is null. |
| `a is not null` | 30 | True when the operand is not null. |
| `a \|\| b` | 40 | Both sides as text, joined; null when either is null. |
| `a + b` | 50 | Addition. |
| `a - b` | 50 | Subtraction; between two dates or times, the seconds from the second to the first: an int for two epochs, a decimal otherwise. |
| `a * b` | 60 | Multiplication. |
| `a / b` | 60 | Division; int / int is decimal, and dividing by zero gives null. |
| `a % b` | 60 | Remainder of int or decimal division; by zero gives null. |
| `-a` | 70 | Negation. |

| Function | Returns | Kind | Meaning | Example |
|---|---|---|---|---|
| `if(condition: bool, then: any, else: any)` | the common type of its values | row | The second argument when the condition is true, else the third. | `if([miles] > 0, [revenue] / [miles], 0)` |
| `case [subject] when condition then result … [else result] end` | the common type of its values | row | The result of the first branch whose condition holds (or whose value equals the subject), else the else branch or null. | `case when [margin] < 0 then 'loss' when [margin] < 0.1 then 'thin' else 'healthy' end` |
| `coalesce(value: any, ...fallback: any)` | the common type of its values | row | The first argument that is not null. | `coalesce({customer.name}, 'Unknown')` |
| `nullif(value: any, other: any)` | the common type of its values | row | Null when both arguments are equal, else the first. | `[revenue] / nullif([miles], 0)` |
| `round(number: number, digits?: digits)` | decimal | row | The number rounded half away from zero to the given decimal places (0 when omitted). | `round([revenue] / [miles], 2)` |
| `trunc(number: number, digits?: digits)` | decimal | row | The number truncated toward zero to the given decimal places (0 when omitted). | `trunc({weight} / 1000, 1)` |
| `floor(number: number)` | decimal (float for a float) | row | The largest whole number not greater than the argument. | `floor([miles] / 100)` |
| `ceil(number: number)` | decimal (float for a float) | row | The smallest whole number not less than the argument. | `ceil({weight} / 1000)` |
| `abs(number: number)` | the type of number | row | The absolute value. | `abs([variance])` |
| `greatest(value: ordered, ...other: ordered)` | the common type of its values | row | The largest of the arguments, ignoring nulls. | `greatest([billed], [quoted])` |
| `least(value: ordered, ...other: ordered)` | the common type of its values | row | The smallest of the arguments, ignoring nulls. | `least([billed], [quoted])` |
| `concat(...value: any)` | string | row | The arguments as text joined together; nulls are skipped. | `concat({firstName}, ' ', {lastName})` |
| `lower(text: text)` | string | row | The text in lower case. | `lower({customer.code})` |
| `upper(text: text)` | string | row | The text in upper case. | `upper({customer.code})` |
| `trim(text: text)` | string | row | The text without leading and trailing spaces. | `trim({reference})` |
| `length(text: text)` | int | row | The number of characters in the text. | `length({proNumber})` |
| `date_trunc(unit: 'hour' \| 'day' \| 'week' \| 'month' \| 'quarter' \| 'year', value: temporal)` | timestamp | row | The instant the period containing the value starts, in the report time zone. | `date_trunc('month', {shippedAt})` |
| `date_diff(unit: 'second' \| 'minute' \| 'hour' \| 'day' \| 'week' \| 'month' \| 'quarter' \| 'year', start: temporal, end: temporal)` | int | row | Whole units from start to end: elapsed time for seconds, minutes and hours, calendar boundaries crossed in the report time zone for days and longer. | `date_diff('day', {pickedUpAt}, {deliveredAt})` |
| `extract(part: 'year' \| 'quarter' \| 'month' \| 'week' \| 'day' \| 'dow' \| 'isodow' \| 'doy' \| 'hour' \| 'minute', value: temporal)` | int | row | A part of the value's local date or time in the report time zone; dow counts from 0 on Sunday, isodow from 1 on Monday. | `extract('dow', {shippedAt})` |
| `now()` | timestamp | row | The instant the statement started. | `date_diff('day', {dueAt}, now())` |
| `today()` | date | row | Today's date in the report time zone. | `date_diff('day', {expiresOn}, today())` |
| `count(value?: any)` | int | aggregate | The number of rows in the group, or of rows where the value is not null. | `count({moves.id})` |
| `count_distinct(value: any)` | int | aggregate | The number of distinct non-null values in the group. | `count_distinct({customer.id})` |
| `sum(number: number)` | the type of number | aggregate | The total of the values in the group. | `sum({totalCharge}) / sum({moves.distance})` |
| `avg(value: mean)` | the type of a date or time, else decimal (float for a float) | aggregate | The mean of the values in the group; the mean of dates or times is the date or time at their average (to the second for an epoch, to the day for a date). | `avg({weight})` |
| `min(value: ordered)` | the type of value | aggregate | The smallest value in the group. | `min({shippedAt})` |
| `max(value: ordered)` | the type of value | aggregate | The largest value in the group. | `max({deliveredAt})` |

<!-- keel:formula-reference:end -->

### Dashboards

`ir.ApplyFilters(def, filters, params)` folds a dashboard's filters and parameters into one tile's
report. A filter applies to reports of its entity when it has a value; a condition of the report on the
same field (not bound to a parameter) takes the filter's operator and value; every other filter is ANDed
on top with the report's own tree nested intact. Report parameters take the dashboard value named by the
tile's binding, then the value of the same name. The stored definition is never modified.

```go
tile, params := ir.ApplyFilters(def, []ir.FilterValue{
    {Filter: ir.DashboardFilter{ID: "status", Entity: "shipment", Field: "status", Op: ir.OpEq}, Value: "Completed"},
}, ir.Params{Values: dashboardValues, Bindings: map[string]string{"window": "lookback"}})
```

The generated TypeScript exports `applyFilters` with the same behaviour, and a test runs both on the
same cases.

### Drill-through

`ir.Drill(def, req, types)` rewrites an aggregate report into the records behind a clicked cell: the
report's filters, the row's grouping values (a bucket inverted into the half-open range it covered in
`req.Location`, a band into its range, a transformed dimension compared through its transform, a
row-level calc compared as a calc), and the clicked measure's own filter and pivot scope. The caller
chooses the record columns the reader may see; the report's dimensions across relations are appended.
`types` gives field types so ranges hold the field's own values (`ir.CatalogTypes(catalog, entity)`
adapts a catalog).

### Versions, schema and TypeScript

`ir.Version` is 2. `ir.Decode` upgrades older definitions; `ir.UpgradeFromTrenovaV1` converts Trenova's
stored IR: field references become paths, computed specs become formulas, `having` by field and
aggregation becomes a condition on a measure column (adding a hidden one when the report has none),
totals become a total grouping, and pivots, parameters, display specs and charts carry over.

`ir.Schema()` is the JSON Schema of a definition, generated from the Go types and their doc comments.
`go generate ./analytics/ir` regenerates it, the TypeScript module (`tsgen.Generate`, which the keel CLI
calls) and the formula reference above; a test fails when any of them is stale. The TypeScript module
holds the definition types, the schema, the operator table per value type, chart and transform rules,
the formula function registry and `applyFilters`, so a client builder imports the server's rules
instead of copying them.

## Analytics: formatting and exports

Four packages turn an analytics result into what people and programs read. They are designed in
[analytics.md](analytics.md#formatting-and-exports):

| Package | What it does |
|---|---|
| `analytics/value` | the value types a column holds (`value.Decimal`, `value.Epoch`, …), the column vector, exact decimal helpers |
| `analytics/table` | the result every writer reads: a schema of columns and a stream of typed column batches |
| `analytics/format` | the display spec, its resolution, a locale- and time-zone-aware formatter, Excel number formats |
| `analytics/export` | streaming CSV, JSON envelope, XLSX and Arrow writers, and the envelope reader |
| `analytics/diff` | run diffs: what appeared, disappeared or moved between two runs |

### The result: `table.Reader`

A result is a `table.Reader`: its `Columns()` and a stream of `*table.Batch` from `Next(ctx)`,
ending with `io.EOF`, then `Truncated()` says whether a row limit cut it.

```go
type Reader interface {
    Columns() []table.Column
    Next(ctx context.Context) (*table.Batch, error)
    Truncated() bool
}
```

A `table.Column` has an `ID`, a translated `Label`, a `value.Type`, its resolved `Display` and a
`Sensitivity` (public, internal, restricted, confidential). A `table.Batch` holds `Len` rows as one
`value.Vector` per column and, when the result has subtotals or a grand total, a `Levels` slice:
`table.Detail`, `table.Subtotal(n)` or `table.Total`.

A vector keeps one typed slice per column, chosen by the type, and a null bitmap:

| Type | Slice | Postgres |
|---|---|---|
| `value.String`, `value.Enum`, `value.Ref` | `String []string` | text, enums, keys |
| `value.Int` | `Int64 []int64` | smallint, integer, bigint |
| `value.Epoch` | `Int64 []int64` (seconds) | bigint columns named `*_at`, `*_date`, `*_time` |
| `value.Decimal` | `Numeric []keel.Decimal` (exact text, as Postgres writes it) | numeric |
| `value.Float` | `Float64 []float64` | real, double precision |
| `value.Bool` | `Bool []bool` | boolean |
| `value.Time` | `Time []time.Time` | timestamptz, and a date bucket's start |
| `value.JSON` | `JSON [][]byte` | json, jsonb |

The analytics runner decodes each column's binary values (pgx's `RawValues`) straight into these
slices, so nothing is boxed or copied on the way to a writer; a batch is valid until the next call
to `Next`. For tests, stored runs and application code, `table.NewBuilder` builds a `*table.Table`
from Go values and `table.Collect` reads any reader into one:

```go
b := table.NewBuilder([]table.Column{
    {ID: "customer", Label: "Customer", Type: value.String},
    {ID: "revenue", Label: "Revenue", Type: value.Decimal,
        Display: format.Resolve(value.Decimal, format.Hint{Kind: format.Money}, nil)},
})
_ = b.Append("ACME", "2840.125")                   // decimals as exact text, keel.Decimal or numbers
_ = b.AppendLevel(table.Total, nil, "2840.125")    // a grand total
r := b.Table().Reader()
```

### Display specs

A column's display is resolved once, from its value type, a `format.Hint` and the author's
`format.Spec`:

```go
d := format.Resolve(value.Decimal, format.Hint{Kind: format.Money}, &format.Spec{Currency: "eur"})
// d.Style == "currency", d.Decimals == 2, d.Currency == "EUR", d.Negative == "parentheses"
```

- **The value type** sets the baseline: numbers are `number` (decimals and floats with two places),
  epochs and times are `date` (`dateTime`), booleans `bool` (`yesNo`), everything else `text`.
- **The hint** is what the model and the definition imply: the format `Kind` from the field type's
  `KeelFormat()` method or the `format=` tag option (`money`, `weight`, `percent`, `duration`,
  `distance`, `count`), the date style a date `Bucket` implies, a `Band`, and enum `Labels` from
  the type's constants.
- **The spec** overrides any field. Restyling resets what the new style implies first: a currency
  takes its currency's places (2 for USD, 0 for JPY) and parentheses for negatives, a number drops
  a derived suffix, and the spec's other fields apply after.

`Spec.Validate(t)` reports the first option that cannot apply to a column of type `t` as a
`*format.Error` with the JSON path of the field (`rules[1].upper`) and a stable code to translate
(`format.CodeStyleNotAllowed`). `format.Rules()` returns the vocabulary as data, the styles each
value type allows with its default, every enum and every limit, for the catalog to publish and the
client to offer.

Rules give numeric cells a tone (`positive`, `warning`, `negative`, `neutral`), first match wins;
values and thresholds compare as exact decimals, and a percent column compares on the scale it
shows (a threshold of 12 means 12%). A band shows a grouped lower bound as its range:
`"$0.00 – $1,000.00"`, `"4:00+"`.

### Formatting values

`format.New` returns a formatter for one column in a locale and a time zone:

```go
berlin, _ := time.LoadLocation("Europe/Berlin")
f := format.New(value.Decimal, d, format.Options{Locale: language.German, Location: berlin})
text, tone := f.Format("-1234.5") // "(1.234,50 €)", ""
```

- **Numbers are exact.** Decimals round half away from zero on their digits (`1.005` with two
  places is `1.01`); floats are read from their shortest decimal form; compact notation picks K, M,
  B or T on the unrounded magnitude.
- **Locale data comes from CLDR.** Decimal and grouping separators, group sizes (`1,23,45,678.9` in
  `en-IN`), native digits, the minus and percent signs and currency symbols come from
  `golang.org/x/text`; currency placement comes from CLDR's currency patterns, which `go generate
  ./analytics/format` writes from the CLDR release x/text uses. `€1,234.50` in English is
  `1.234,50 €` in German and `€ 1.234,50` in Dutch.
- **Dates are written in the given time zone** with the same layouts in every locale (`date`,
  `dateLong`, `dateTime`, `time`, `monthYear`, `quarter`, `year`, `weekday`, `iso`). A date bucket is
  an instant, so it renders as the period it starts in the report's zone.
- **Batches cost nothing per value.** `f.Column(&cells, vector, n)` renders a whole vector into a
  reusable `format.Cells`; once its buffers have grown it allocates nothing (numbers too large for
  an int64 excepted). A formatter keeps scratch buffers, so use one per goroutine.

`f.Excel()` returns the number format code a spreadsheet cell carries (`"$"#,##0.00;("$"#,##0.00)`)
and whether the column's values are native numbers or dates.

Compared with Trenova's `reportfmt` and its Rust formatter, the behaviour is the same except:

| Here | Trenova |
|---|---|
| A negative that rounds to zero shows no sign: `$0.00` | `($0.00)`, `-0.00` |
| Currency symbols and placement follow the locale: `CA$` in `en-US`, `$` in `en-CA`; `CHF 5.00` with a no-break space | a fixed table: `C$` everywhere |
| A currency's default places are its own: JPY 0, BHD 3 | always 2 |
| A JSON value shows as its text | empty |
| A raw time is RFC 3339 in UTC | RFC 3339 in the value's zone |
| In XLSX, a raw number is a native number and a quarter is its text | text; a quarter showed only its year |
| Decimal hours are rounded once | rounded to 16 places, then to the display's |

### Exports

Each writer streams a `table.Reader` to an `io.Writer` and returns `export.Stats`: detail rows
written, bytes written, and whether and why the output was truncated.

```go
stats, err := export.XLSX(ctx, w, result, export.Options{
    Meta:     export.Meta{Title: "Revenue by lane", GeneratedAt: start, RequestedBy: user,
        Params: []export.Param{{Name: "window", Value: 30}}},
    Locale:   language.AmericanEnglish,
    Location: orgZone,
    MaxBytes: 50 << 20,
})
```

| Writer | For | What it writes |
|---|---|---|
| `export.CSV` | people (default) or programs (`Raw`) | formatted cells under the labels, labelled subtotal and total rows, a truncation notice; `Raw` writes column IDs, exact values and detail rows only. `Delimiter` and a UTF-8 `BOM` are options |
| `export.JSON` | stored runs, diffs, agents | the rows envelope `{meta, schema, rows, levels, totals, summary}` with raw values: decimals as exact strings, instants as RFC 3339 UTC strings, epochs as seconds |
| `export.XLSX` | spreadsheets | native numbers, dates and booleans with the column's number format; text for displays Excel cannot express; bold header and summary rows; tones as fills; an info sheet with the title and parameters in declared order |
| `export.Arrow` | notebooks and dataframes | an Arrow IPC stream: `utf8`, `int64`, `float64`, `bool`, `decimal128(38, s)`, `timestamp[us, UTC]` for times and `timestamp[s, UTC]` for epochs, detail rows only |

- **Formula injection.** CSV cells that a spreadsheet would run (starting with `=`, `+`, `@`, a tab,
  a carriage return, or `-` followed by anything but a number) are prefixed with `'`. Exact numbers
  in a raw CSV are never touched.
- **XLSX streams for real.** The writer emits the zip entries and sheet XML itself as rows arrive,
  with inline strings and no shared-string table, estimates column widths from the first 100 rows,
  and continues on `Data (2)`, `Data (3)`, … after 1,048,575 rows. Writing two million rows keeps
  the live heap flat (about 2.4 MiB before and after); `TestXLSXStreamsInFlatMemory` checks it.
- **Arrow costs no dependency.** The IPC stream is written by keel's own encoder; keel tests it by
  reading it back with apache/arrow-go, which is a test-only dependency.
- **Byte limits.** With `MaxBytes`, a writer stops reading rows once its output reaches the limit,
  writes the truncation notice (CSV, XLSX), the summary (JSON) or a final record batch with
  `keel.truncated` in its metadata (Arrow), and closes the file, which is always complete. The limit
  is checked between rows (every 512 rows for XLSX, every batch for Arrow).
- **Words.** `Options.Text` carries the words a writer puts in a file ("Total", the notices, sheet
  names) for the application to translate.

`export.ReadJSON` reads an envelope back as an `*export.Envelope`: its meta, the time zone, the
summary and a `*table.Table` whose last row is the grand total. It refuses an envelope whose rows do
not match its schema.

### Run diffs

`diff.Compare` reads two runs and reports what changed:

```go
before, _ := export.ReadJSON(lastWeek)
after, _ := export.ReadJSON(thisWeek)
res, err := diff.Compare(ctx, before.Table.Reader(), after.Table.Reader(), diff.Options{})
for _, ch := range res.Changes {
    fmt.Println(ch.Kind, ch.Key, ch.Measures)
}
```

- Rows are matched by key: by default every string, enum, reference, boolean and instant column;
  `Options.Keys` chooses others. Measures default to the numeric columns (`Options.Measures`).
- Measures compare as exact decimals; a null measure counts as zero. A null key cell is not the
  empty string.
- A key that repeats within either run is reported once as `duplicate` and never compared.
- Added and removed rows state their measures against zero, so they rank by size with the rest.
  Changes are ordered duplicates, removed, added, changed, each biggest movement first, and capped
  at `Options.Limit` (200 by default; negative for all). The summary always counts every row.
- `Totals` sums each measure over the detail rows present on each side; `Truncated` says that a
  run hit its row limit, so a removed row may only be past it.

### Performance

On a 4-core cloud VM (`go test -bench . ./analytics/format ./analytics/export`):

| Work | Time |
|---|---|
| Format 100,000 values of one column (text, counts, dates in Chicago, money in German, 20-digit quotients) | 1.4–14.6 ms (14–146 ns a value); no allocations once warm, quotients included |
| Write 100,000 rows of six columns: CSV / raw CSV / JSON / XLSX / Arrow | 73 / 38 / 20 / 181 / 6.0 ms |
| Write 2,000,000 rows as XLSX | 4.5 s, live heap flat |

## Analytics: caching and rollups

Three packages make repeated and large reports cheap. None of them needs the others:

- [`analytics/cache`](../analytics/cache) holds result caches: in memory, in Postgres, or in Redis
  through the application's own client.
- [`analytics/rollup`](../analytics/rollup) maintains pre-aggregated tables, refreshes them
  incrementally and routes reports to them.
- [`analytics/analyticscli`](../analytics/analyticscli) adds the `analytics` commands to a keelcli
  binary: TypeScript, message catalog, model description, rollup migrations and refresh.

[analytics.md](analytics.md) is the design.

### Result caches

A store keeps byte values by key, each with a time to live:

```go
type Store interface {
    Get(ctx context.Context, key string) ([]byte, bool, error)
    Set(ctx context.Context, key string, value []byte, ttl time.Duration) error
}
```

| Store | Where values live | Notes |
|---|---|---|
| `cache.NewMemory(cache.MemoryOptions{MaxBytes: 256 << 20})` | the process | 64 shards by default, each an LRU list under its own lock; entries expire at their TTL and the least recently used entry of a full shard is evicted. `Get` returns a shared slice: do not modify it |
| `cache.NewPostgres(db.Pool(), cache.PostgresOptions{})` | `keel_analytics_cache`, an `UNLOGGED` table every process shares | created on first use under an advisory lock; expiry follows the database's clock; expired rows are deleted in batches of `CleanupBatch` in the background, at most every `CleanupEvery` per process, with `SKIP LOCKED`; `Cleanup` runs it on demand |
| `cache.NewRedis(client, cache.RedisOptions{})` | Redis, Valkey, Dragonfly, … | keel has no Redis dependency: `client` is three methods (`Get`, `Set`, `Del`) the application implements over its own client; the `RedisClient` comment shows go-redis |

A value larger than the store takes (a shard's share of `MaxBytes`, or `MaxValueBytes`, 4 MiB by
default) is `cache.ErrTooLarge`, and the key's previous value is removed so it is never served
stale. Keys longer than 200 bytes are stored as their SHA-256 in Postgres and Redis. A TTL of zero
or less keeps a value until it is evicted.

`cache.Loader` puts a store in front of a computation:

```go
loader := cache.NewLoader(store, cache.LoaderOptions{OnError: logCacheError})

body, hit, err := loader.Load(ctx, key, 5*time.Minute, func(ctx context.Context) ([]byte, error) {
    return runReport(ctx, def)
})
```

- Concurrent misses of one key in a process share one load: the first caller runs it, the others
  wait for its value. In-flight loads are spread over sharded locks.
- The load runs with the first caller's context values but not its cancellation; it is cancelled
  when every waiting caller has given up, and a caller arriving after that starts a fresh one.
- A store failure is a miss (`Get`) or is ignored (`Set`) and goes to `OnError`: a cache outage
  slows reports down without failing them. A load error reaches every waiter and nothing is stored.
  A panicking load becomes an error.

The key must cover everything the value depends on: the statement and its arguments (the tenant
among them), the row rules, the output format, and the data versions of every table the statement
reads (`db.DataVersions`). A report answered from a rollup is keyed on its source tables' versions,
since the rollup changes only when they do.

### Rollups

A rollup is a tenant table keel keeps aggregated by a time bucket and some dimensions. It is
declared once, from the generated handles:

```go
s := fleetdb.Shipments

var ShipmentsDaily = rollup.MustDefine("shipments_daily", fleetdb.ShipmentsSchema(),
    rollup.Day(s.CreatedAt),
    rollup.Dims(s.CustomerID, s.Status),
    rollup.Measures(rollup.Count(), rollup.Sum(s.Total), rollup.Avg(s.Weight), rollup.Max(s.Weight)),
)
```

- **Grain**: `rollup.Hour`, `Day`, `Week` (ISO, from Monday), `Month`, `Quarter` or `Year` of a
  `timestamptz`, `date` or epoch column. Buckets are computed in UTC, or in
  `rollup.Zone("America/Chicago")`.
- **Dimensions**: columns of the same table; tenant columns are always kept.
- **Measures**: `Count()` (rows), `CountOf(c)` (non-null values), `Sum`, `Avg`, `Min`, `Max` and
  `CountDistinct`. Sums, counts, minimums and maximums compose, so a report over coarser buckets or
  fewer dimensions re-aggregates them; an average is stored as a sum and a count; a distinct count
  answers only a report at the rollup's exact grain and dimensions.
- `Define` returns an error, and `MustDefine` panics, when a column belongs to another table or was
  taken through a relation; the grain is not a time; a dimension is a tenant column, the grain, a
  repeat or a tsvector; a sum or average reads non-numbers, a minimum or maximum unordered values;
  or two derived column names collide.

The rollup table holds the tenant columns, `bucket` (the start of each bucket, a `timestamptz`), the
dimensions, and one column per stored aggregate: `row_count`, `<column>_sum`, `<column>_count`
(only for a nullable column; otherwise the row count serves), `<column>_min`, `<column>_max`,
`<column>_distinct`. `Rollup.Table()` is its `schema.Table`, with the source's belongs-to relations
whose keys are dimensions, so a report can group by `customer.name` through it.

#### Migrations

```sh
fleet-db analytics rollups migration -out migrations         # new and changed rollups only
fleet-db analytics rollups migration -check -out migrations  # CI
```

`rollup.WriteMigrations(dir, rollups...)` does the same from Go, `rollup.Migration(r)` returns the
SQL, `rollup.Func(version, r)` wraps it as a Go migration, and `rollup.DropMigration(name, table)`
removes a rollup that is no longer declared. A rollup migration is a `--keel:notx` file whose parts
are each safe to run again:

1. `CREATE INDEX CONCURRENTLY` on the source's tenant and grain columns, which refresh reads
   through (left out with `rollup.WithoutSourceIndex()` when an index with those leading columns
   exists).
2. One transaction: the shared `keel_rollup_dirty` and `keel_rollup_state` tables; the rollup table,
   dropped first when an earlier definition made it, created with `CREATE TABLE … AS … WITH NO DATA`
   so Postgres takes every column type from the source, and its unique key; the trigger function and
   three statement triggers on the source.
3. Marking every existing tenant for a rebuild and recording the rollup as ready. The source's
   trigger lock is held only for the DDL of part 2, so writes wait briefly.

Row-level security and grants stay with the application: its migrate `After` statements (Trenova's
`SELECT trenova_rls.reconcile()`) see the rollup table as any other tenant table. Rollups need
PostgreSQL 15 or later.

#### Dirty tracking

Every write to the source records the (tenant, bucket) slices it touched in `keel_rollup_dirty`,
inside the write's transaction, through `AFTER … FOR EACH STATEMENT` triggers with transition
tables: one `INSERT … SELECT DISTINCT … ON CONFLICT DO NOTHING` per statement, however many rows it
wrote. An update marks only rows whose rollup columns changed, both their old and new slices; a
delete marks the rows' old slices.

- Marks are keyed by the writing transaction's id, so a transaction marks a slice once and two
  writers never contend for a row.
- Triggers see every write, including `keel.Exec`, `InsertFrom`, `COPY`, other services and manual
  SQL, and work in implicit transactions, where a keel table hook could not write in the same
  transaction.
- The cost is one statement per write statement and rollup: on the test VM a one-row create with
  two rollups takes 1.24 ms instead of 1.06 ms.
- The application's database role needs `INSERT` on `keel_rollup_dirty`.

#### Refresh

The application schedules refresh: a Temporal schedule, a ticker, or the command line.

```go
rep, err := rollup.Refresh(ctx, db, ShipmentsDaily, ShipmentsMonthly)

rep, err = rollup.RefreshWith(ctx, db, rollup.RefreshOptions{
    BatchSize:  500,
    MaxBatches: 20,
    OnBegin: func(ctx context.Context, tx pgx.Tx) error {
        _, err := tx.Exec(ctx, "SELECT set_config('app.bypass_rls', 'on', true)")
        return err
    },
}, ShipmentsDaily)
```

Each transaction claims up to `BatchSize` dirty rows with `FOR UPDATE SKIP LOCKED`, takes an advisory
lock per slice without waiting (exclusive per tenant for a whole-tenant rebuild, shared per tenant
and exclusive per slice otherwise), deletes the committed dirty rows of the slices it locked, then
deletes and re-inserts those slices of the rollup table from the source in one round trip per
rollup. Workers on many processes refresh at once without waiting on each other; a slice another
worker holds is left to it.

A refresh never misses a committed write: the write's mark commits with it, the refresh deletes only
marks committed before it, and it reads the source in a later statement, so it sees every write
whose mark it deleted. Marks written while it runs stay for the next refresh.

| Function | Does |
|---|---|
| `Refresh`, `RefreshWith` | recompute dirty slices until none is claimable, or `MaxBatches`; `StatementTimeout` bounds each statement; `OnBegin` runs first in every transaction, for row-level security |
| `Rebuild(ctx, db, opts, r, org, bu)` | recompute one tenant now, waiting for workers on it: a backfill tenant by tenant, or a repair |
| `MarkAll(ctx, db, rollups...)` | mark every tenant for a rebuild by the next refresh, after writing with the triggers disabled |
| `Pending(ctx, db, rollups...)` | dirty rows waiting per rollup, for metrics and alerts |

`Report` counts transactions, slices, whole tenants, rows written, slices left to other workers and
wall time, in total and per rollup. In a zone with clock changes, a slice is read through its
wall-clock range widened by three hours and filtered by bucket, so a repeated or skipped local time
never drops a row.

#### Routing

```go
router, err := rollup.NewRouter(db, rollup.RouterOptions{
    TenantColumns: cfg.TenantColumns,
    Location:      reportZone,
}, ShipmentsDaily, ShipmentsMonthly)

if plan, ok := router.Route(ctx, resolved); ok {
    // compile the report over plan.Table
}
```

`Route` takes a resolved definition (`ir.Resolve`) and tries the entity's rollups, coarser grains and
fewer dimensions first. A rollup covers a report when:

| Part | Covered when |
|---|---|
| The report | it aggregates, and runs in the rollup's time zone |
| Dimensions | rollup dimensions, the key of a related row (`customer.id` is the rollup's `customer_id`), fields of a belongs-to relation whose key is a dimension (`customer.name`), or the grain field bucketed to the grain or a unit made of whole grain buckets (a day rollup answers days, weeks, months, quarters and years; a week rollup only weeks) |
| Measures | stored: sum, min, max and count re-aggregate; avg divides a summed sum by a summed count; count distinct only when the report groups by exactly the grain bucket and every dimension, without subtotals or a measure filter |
| Filters and measure filters | conditions on dimensions and related fields of any shape, row-level calculations over them, and conditions on the grain field that select whole buckets: `gte`, `lt` and `between` on bucket starts, `today` to `last_year` when their period is whole buckets, `is_null`; never a parameter, `last_n_days` or a bound inside a bucket |
| Calculations, windows, having, sort, top, pivot, subtotals | computed above the rollup; a pivot field must be a dimension; an aggregate calculation must not read fields |

`Route` then checks freshness: the rollup has been built and the report's tenant (every tenant for a
system-scope report) has no dirty slice in the bucket range its filters read. Otherwise it answers
false and the report reads its source tables. `Match` does the coverage alone, and its error says
why each rollup refused.

The `Plan` gives the compiler `Table` (the rollup's `schema.Table`, tenant columns included),
`Fields` (each path's column, or relation and target column; `Grain` marks the bucket column, whose
values are instants), `Dimensions` (with `Bucket` when the bucket must be truncated further;
`BucketSQL` renders it), `Measures` (`MeasurePlan.SQL(alias, filter)` renders the re-aggregation,
counts as `coalesce(sum(…), 0)::int8`) and the bucket range `[From, To)`. `Route` checks freshness on
the primary; a report read from a replica runs `Plan.Freshness()` in its own transaction instead, so
the check and the read share a snapshot.

### Analytics commands

`analyticscli.Command` adds one command, `analytics`, to a keelcli binary:

```go
keelcli.Main(keelcli.App{
    Name:       "fleet-db",
    Config:     dbConfig,
    Migrations: migrate.Sources(migrate.Source{Name: "app", FS: migrations}),
    Commands: []keelcli.Command{analyticscli.Command(analyticscli.Config{
        Tables:  []*schema.Table{fleetdb.ShipmentsSchema(), fleetdb.CustomersSchema()},
        Rollups: []*rollup.Rollup{ShipmentsDaily},
    })},
})
```

`rollups refresh` and `rollups pending` use the App's database; run without keelcli they open one
with `Config.Open`.

| Command | Does |
|---|---|
| `analytics ts [-check] -out path` | writes the TypeScript module of `analytics/ir` (`tsgen.Write`), or fails when it is out of date |
| `analytics i18n [-check] -out path` | writes the message catalog below, or fails when it is out of date |
| `analytics describe [-out path]` | describes the reportable entities, their fields, relations and rollups as compact text for a language model's context |
| `analytics rollups migration [-check] -out dir` | writes migrations for new and changed rollups, or fails when one is missing or stale |
| `analytics rollups refresh [-rollup a,b] [-batch n] [-max-batches n] [-rebuild]` | refreshes dirty slices; `-rebuild` marks every tenant first; `-json` prints the `Report` |
| `analytics rollups pending [-rollup a,b]` | dirty rows waiting per rollup |

A failed check exits 3, a usage error 2. `analyticscli.Run(ctx, cfg, args, out)` runs the same
commands without keelcli, and `analyticscli.Main(cfg)` wraps it for a `go:generate` program:

```go
//go:generate go run ./cmd/analyticsgen ts -out ../client/packages/analytics/index.ts
//go:generate go run ./cmd/analyticsgen i18n -out ../i18n/analytics.en.json
```

The message catalog is one JSON object of translation key to fallback text, sorted by key, for the
application's translation pipeline (`analyticscli.Messages` returns it as a map):

| Key | Fallback |
|---|---|
| `shipment`, `shipment@plural`, `shipment@description` | the entity's label, plural and doc comment |
| `shipment.proNumber`, `shipment.proNumber@description` | a field's label and doc comment |
| `shipment.status.InTransit` | an enum value's label |
| `shipment.customer`, `shipment.customer@description` | a relation's label and doc comment |
| `@category.Operations` | a report category |

Keys use the catalog's entity, field and edge keys, which `rollup.DefaultKeys` derives in lower
camel case (`CustomerID` → `customerId`); `Config.Keys` replaces them. Hidden fields, relations
marked `noreport` and relations to tables that are not reportable are left out, as are keys with
no text.

### Performance

On a 4-core cloud VM with Postgres in Docker (`go test -bench . ./analytics/cache ./analytics/rollup`):

| Work | Time |
|---|---|
| Memory store `Get`, 4 goroutines | 135 ns, no allocation |
| Memory store `Set` of 2 KiB, 4 goroutines | 1.1 µs, one allocation (the copy) |
| `Loader.Load` hit, 4 goroutines | 120 ns |
| Postgres store `Get` of 2 KiB | 0.23 ms (a round trip) |
| One-row create with two rollups marking slices / without triggers | 1.24 / 1.06 ms |

## Analytics: the engine

The [`analytics`](../analytics) package turns a definition into tenant-scoped SQL and runs it. It has
four parts: the **catalog** of what can be reported on, derived from the generated schema; the
**policy** that decides what one caller may see; the **compiler**, which turns a definition into keel
dynamic queries; and the **runner**, which runs them behind one set of guards and returns
`table.Reader`s that the formatter and the exporters read directly.

```go
cat, err := analytics.Build([]*schema.Table{
    shipmentdb.ShipmentsSchema(), customerdb.CustomersSchema(), movedb.MovesSchema(), // …
}, analytics.Options{Translate: translator.Text})
runner, err := analytics.NewRunner(db, cat, analytics.RunnerOptions{
    Slots: 4, SlotWait: 2 * time.Second, MaxCost: 5e6, Cache: resultCache,
})

res, err := runner.Preview(ctx, analytics.Request{
    Definition: def, Params: params, Policy: policy,
    Location: userZone, Key: "preview:" + userID,
})
_, err = export.CSV(ctx, w, res, export.CSVOptions{})
```

### The catalog

`analytics.Build(tables, opts)` takes generated tables, keeps those marked reportable (so an
application can pass every table it has) and checks the graph: every edge leads to a reportable entity,
every pick walks real relations and orders by a total order, and joined tables share their tenant
columns. A problem is an error with code `catalog`.

- **Labels.** Each entity, field, edge, enum value and category has a derived label and a translation
  key (`shipment`, `shipment.proNumber`, `shipment.status.InTransit`, `category.Operations`).
  `Options.Translate(ctx, key, fallback)` returns the text in the request's language; `nil` keeps the
  derived labels.
- **`Version()`** hashes everything a stored definition depends on (keys, types, aggregations, edges).
  Store it with a definition; a sweep over the definitions whose version differs, calling
  `cat.Check(ctx, def, policy)`, lists their problems as `Diagnostic`s (definition errors with
  suggestions, and denials with the policy's reason).
- **`For(ctx, policy)`** returns the caller's `View`: every entity, field and edge with an `Access`
  (allowed, or denied with a reason code) and translated labels. `View.Field(entity, path)` resolves a
  dotted path with the access the compiler checks for it. `View.Describe()` is a compact text form of
  what the caller may use, for a language model's context.
- **`Rules()`** is one JSON value with the operators per type, the transforms, buckets, bands, windows,
  output naming and limits, so a builder offers exactly what the compiler accepts.
- The catalog implements `ir.Catalog`, so `ir.Resolve` and `ir.Drill` validate against it.

### Policies

```go
type Policy interface {
    Entity(ctx context.Context, e *analytics.Entity) analytics.Access
    Field(ctx context.Context, f *analytics.Field, via analytics.Path) analytics.Access
    Edge(ctx context.Context, e *analytics.Edge, via analytics.Path) analytics.Access
    Rows(ctx context.Context, e *analytics.Entity) []keel.Pred
}
```

- `Field` and `Edge` receive the path that reaches them, so "a customer's name, but not through
  `billTo`" is expressible. A denial returns `analytics.Denied("needs_billing")`; an allowed field can
  carry a sensitivity, `analytics.Allowed().WithSensitivity(table.Confidential)`, which every output
  column reading it inherits, through calculations and windows.
- Each question is asked once per request, whatever the number of columns, filters and pivot cells that
  ask it, so an implementation can load the caller's permissions once.
- `Rows` returns row rules over the entity's own columns
  (`shipmentdb.Shipments.OwnerID.Eq(user)`, or `keel.DynFrom(e.Table).Path("region").Eq(r)` for a
  dynamic table). They apply to every reference a statement makes to the table, through
  `keel.WithReadFilter`: the base table, the join `ON` clauses (a customer the rule excludes reads as
  absent), picks and each table a pick walks, EXISTS filters and pre-aggregations. A report reading a
  table with row rules is never answered from a rollup.
- A nil policy allows everything. Tenancy applies whatever the policy says.

### Compiling

`analytics.Compile(ctx, cat, policy, def, params, opts)` validates the definition, checks the policy,
binds the parameters and returns a `Compiled`: the main `Query` (a `*keel.DynQuery`), the `Discovery`
query of a dynamic pivot, the output `Columns`, the `Tables` read and a `Complexity` count.
`compiled.ToSQL(ctx, db)` renders the statement as it runs. Every value is a bound parameter and
every identifier comes from the schema; equal values share a `$n`, so an expression in SELECT, GROUP
BY and ORDER BY renders identically.

| Definition | Compiles to |
|---|---|
| Bucket (`day` … `year`, `hour_of_day`, `day_of_week`, …) | `date_trunc` or `extract` in the report's time zone (`CompileOptions.Location`), returned as the instant the bucket starts |
| Band | a `CASE` over the band edges, half-open, with a band for values past the last edge |
| Transform | the expression inside the aggregate (`round`, `lower`, `abs`, …) |
| Measure filter | `agg(x) FILTER (WHERE …)` |
| Measure over a to-many path | a pre-aggregated CTE per parent, left-joined on the parent key (see below) |
| Count distinct over a to-many path | a second grouped statement over the chain of child rows, joined back on the groups |
| Calculation | the formula registry's SQL template for each function, with literals as parameters; aggregates over the grouped rows are hoisted into the grouped statement |
| Window | `OVER (PARTITION BY … ORDER BY … ROWS …)` over the grouped result |
| Subtotals and total | `GROUPING SETS` with `GROUPING()` as a hidden level column; each subtotal row sorts after the rows it covers and the total last |
| Top N | `row_number()` over the grouped result, filtered on the rank, per group and per level |
| HAVING | on the grouped statement, or as a WHERE over the grouped result when it reads a window or a calculation over it; subtotal and total rows are kept |
| Static pivot | one `FILTER (WHERE field = $n)` aggregate per value and per pivoted column, plus `Other` |
| Dynamic pivot | a discovery statement (top K values by a measure, or a parameter), then the pivot with the values bound |
| Limit | `LIMIT n+1` for a preview, so truncation is known without counting |
| Page | keyset paging over the sort columns and a tiebreak; the cursor carries the last row's sort values |
| Pick | a lateral join that picks one row (the first stop of a shipment, its last move) |

**To-many measures.** A measure over `moves.stops.id` cannot be joined directly without multiplying the
shipment's own measures. The compiler pre-aggregates the child rows per parent in a CTE and left-joins
it on the parent key. `CompileOptions.Strategy` chooses how:

- `StrategyCTE` restricts the CTE to the parents the report's filters select (a semi-join);
- `StrategyCTEAll` aggregates every parent in one pass, cheaper when the filters keep most parents;
- `StrategyLateral` reads each parent's rows in a lateral subquery, best for a handful of parents;
- `StrategyAuto` (the default) reads laterally when the filters pin at most `LateralParents` keys, and
  otherwise lets the runner EXPLAIN the restricted and unrestricted forms and remember the cheaper
  one per tenant and statement shape.

**Drill-through.** `analytics.Drill(ctx, cat, def, row, column, opts)` returns the record-list
definition behind one cell: the report's filters, each grouping value of the row (buckets and bands
inverted into half-open ranges in the report's zone) and the clicked measure's filter and pivot scope.
`runner.Drill(ctx, req, row, column, opts)` runs it through the guards.

**Semantics to know.** Relative dates (`today`, `last_n_days`, `this_month`) are whole days in the
report's zone, so a statement is stable for a day and caches. `ne` and `not_in` keep rows whose value
is NULL. `between` is half-open: `[from, to)`.

### Running

`analytics.NewRunner(db, cat, opts)` returns a `Runner`, safe for concurrent use. Every entry point takes
a `Request` (definition, parameters, policy, time zone, `Now`, supersede key, output format):

| Entry point | Returns | Default timeout |
|---|---|---|
| `Preview(ctx, req)` | `*Result`: `PreviewRows` rows (100) and whether there were more | 30 s |
| `Run(ctx, req)` | `*Stream`: every row up to `MaxRows`, one batch at a time; close it, or read to `io.EOF` | 10 min |
| `Count(ctx, req)` | the report's row count | 30 s |
| `Page(ctx, req, cursor, size)` | `*Result` with the next page's cursor | 30 s |
| `Drill(ctx, req, row, column, opts)` | `*Result` with the records behind a cell | 30 s |
| `Compile(ctx, req)` | `*Compiled` with the runner's options, for `ToSQL` | |

`Result` and `Stream` implement `table.Reader`. A `Result` reads the batches the dynamic query layer
decoded without copying them. A `Stream` decodes each batch on the run's connection as it is read, so
an export of any size holds one batch.

Errors are `*analytics.Error` values with a `Code` the application translates: `invalid_definition`
(with the definition errors in `Details`), `parameter`, `denied` (with the policy's `Reason`),
`not_allowed`, `too_complex`, `too_costly`, `busy`, `superseded`, `timeout`, `cursor` and `catalog`.
`analytics.ErrorCode(err)` reads the code through wrapping, and `errors.Is` matches two errors with
the same code.

### Guards

Every entry point passes the same guards, in this order:

| Guard | How |
|---|---|
| Definition limits | `ir.Limits` (columns, dimensions, pivot values, windows, path depth, …) |
| Complexity | `ComplexityLimits` on the joins, to-many paths, pre-aggregations and windows, and a weighted `Score` |
| Supersede | a request with a `Key` cancels the previous run with the same tenant and key: in this process through its context, in others with `pg_cancel_backend` on every server (`keel.ExecAll`), found by an `application_name` that hashes the tenant and key |
| Statement timeout | `set_config('statement_timeout', …, true)` in the run's transaction, per entry point, with the context deadline to match |
| Replica | a read-only transaction on a replica (`keel.OnReplica`), or the primary when every replica lags or `Primary` is set |
| Concurrency | `Slots` statements per tenant at once, shared by every process: a transaction-scoped advisory lock taken from a random slot (`pg_try_advisory_xact_lock(tenant, slot)`), retried until `SlotWait`, then `busy`. The lock is released with the transaction whatever happens to the process |
| Cost | `EXPLAIN (FORMAT JSON)` total cost and rows against `MaxCost` and `MaxPlanRows`, in the run's transaction; the estimates are cached per tenant and statement shape for `ExplainTTL` (30 s), so a busy dashboard pays for EXPLAIN once |
| Rows and bytes | the limit in SQL, `MaxRows` for runs, and `MaxBytes` on the values a result holds; a cut result says it is truncated |

The slots and supersede work on hot standbys too. The advisory locks live on the server that runs the
statement, so with replicas a tenant can run `Slots` statements on each server.

### Caching

`RunnerOptions.Cache` is a port:

```go
type Cache interface {
    Get(ctx context.Context, key string) ([]byte, bool, error)
    Set(ctx context.Context, key string, value []byte, ttl time.Duration) error
}
```

The key (`CacheKey`) covers the statement and its arguments (which carry the tenant's values and the
policy's row rules), the entry point, the output format and schema, and the data version of every
table the statement reads for the tenant. A write through keel to any of those tables moves its version,
so the next run misses; `CacheTTL` (15 min) is the backstop for writes keel cannot see. The cache needs
data versions on the DB. A streamed run is cached when it has at most `MaxCachedRows` rows (50,000).
Cache failures go to `OnError` and count as misses. `Result.Cached()` and `Stream.Cached()` report
that the rows came from the cache rather than a statement.

### Rollups

`RunnerOptions.Rollups` (or `CompileOptions.Rollups`) is a `RollupRouter`:

```go
type RollupRouter interface {
    Route(ctx context.Context, resolved *ir.Resolved) (analytics.RollupPlan, bool)
}
```

For an aggregated report that reads no table with row rules and has no fan-out distinct count, the
compiler asks the router. A `RollupPlan` names the rollup table, the column holding each field the
report reads, and how each measure re-aggregates (`ReSum`, `ReMin`, `ReMax`, `ReAvg` over a partial sum
and count). When the plan lacks something the report needs, the compiler falls back to the base tables.
`Compiled.Rollup` and `Result.Rollup()` name the rollup that answered.

### Decimals

A numeric column is `keel.Decimal` from the database to the writer: the exact text Postgres sends, held
in the batch's arena. A `keel.DynColumn` has the layout of a `value.Vector`, so the runner hands batches
to readers without converting them. The formatter and the writers read the digits directly (money and
wide quotients format at about 80 and 100 ns a value, with no allocations), and the diff and total
arithmetic (`value.AddDecimal`, `value.CompareDecimal`) takes an int64 fast path with a `big.Int` fallback.

### What the compiler writes

A grouped report with a monthly bucket in Chicago, a filter, a running total per customer, subtotals per
customer and a total, previewed (`Probe` asks for the 101st row):

```json
{"entity": "shipment", "columns": [
  {"id": "customer", "dimension": {"field": "customer.name"}},
  {"id": "month", "dimension": {"field": "createdAt", "bucket": "month"}},
  {"id": "revenue", "measure": {"field": "total", "agg": "sum"}},
  {"id": "running", "window": {"of": "revenue", "fn": "running_total", "over": "month", "partition": ["customer"]}}
], "filters": {"field": "status", "op": "ne", "value": "Canceled"},
"groupings": {"subtotals": ["customer"], "total": true}, "sort": [{"column": "customer"}, {"column": "month"}]}
```

```sql
WITH "keel_sub" ("c1", "c2", "c3", "c4") AS (
  SELECT c."name" AS c1,
         CAST(extract(epoch from date_trunc($5::text, to_timestamp(s."created_at"), $6::text)) AS bigint) AS c2,
         sum(s."total") AS c3,
         GROUPING(c."name", CAST(extract(epoch from date_trunc($5::text, to_timestamp(s."created_at"), $6::text)) AS bigint)) AS c4
    FROM "shipments" s
    LEFT JOIN "customers" c ON c."id" = s."customer_id"
         AND c."organization_id" = $2 AND c."business_unit_id" = $3
   WHERE s."organization_id" = $2 AND s."business_unit_id" = $3
     AND ((s."status" IS NULL) OR (s."status" <> $4))
   GROUP BY GROUPING SETS (
         (c."name", CAST(extract(epoch from date_trunc($5::text, to_timestamp(s."created_at"), $6::text)) AS bigint)),
         (c."name"), ()))
SELECT ks."c1" AS c1, ks."c2" AS c2, ks."c3" AS c3,
       sum(ks."c3") OVER (PARTITION BY ks."c4", ks."c1" ORDER BY ks."c2" ASC
                          ROWS BETWEEN UNBOUNDED PRECEDING AND CURRENT ROW) AS c4,
       ks."c4" AS c5
  FROM "keel_sub" ks
 ORDER BY ((ks."c4" >> $1::integer) & 1) ASC, ks."c1" ASC, ks."c4" ASC, ks."c2" ASC
 LIMIT 101
-- $1 = 1, $2 $3 = the tenant, $4 = 'Canceled', $5 = 'month', $6 = 'America/Chicago'
```

The window partitions by the level too, so subtotal rows never mix with detail rows. The first ORDER BY
term puts each customer's subtotal after its months and the total last.

A sum across a to-many path and a distinct count across a deeper one, under a policy whose row rules
hide shipment `P01` and customers with code `G`:

```json
{"entity": "shipment", "columns": [
  {"id": "customer", "dimension": {"field": "customer.name"}},
  {"id": "revenue", "measure": {"field": "total", "agg": "sum"}},
  {"id": "stops", "measure": {"field": "moves.stops.id", "agg": "count"}},
  {"id": "cities", "measure": {"field": "moves.stops.location.city", "agg": "count_distinct"}}
], "sort": [{"column": "revenue", "desc": true}]}
```

```sql
WITH "keel_sub" ("c1", "c2", "c3") AS (
  WITH "keel_moves_stops" ("k1", "k2", "k3", "c1") AS (
    SELECT m."shipment_id", m."organization_id", m."business_unit_id", count(s2."id")
      FROM "moves" m
      JOIN "stops" s2 ON s2."organization_id" = $2 AND s2."business_unit_id" = $3 AND s2."move_id" = m."id"
     WHERE m."organization_id" = $2 AND m."business_unit_id" = $3
     GROUP BY m."shipment_id", m."organization_id", m."business_unit_id")
  SELECT c."name" AS c1, sum(s."total") AS c2, coalesce(CAST(sum(kms."c1") AS bigint), $1::bigint) AS c3
    FROM "shipments" s
    LEFT JOIN "customers" c ON c."id" = s."customer_id"
         AND c."organization_id" = $2 AND c."business_unit_id" = $3 AND (c."code" <> $5)
    LEFT JOIN "keel_moves_stops" kms ON kms."k1" = s."id"
         AND kms."k2" = s."organization_id" AND kms."k3" = s."business_unit_id"
   WHERE s."organization_id" = $2 AND s."business_unit_id" = $3 AND s."pro_number" <> $4
   GROUP BY c."name"),
"keel_cte" ("c1", "c2") AS (
  WITH "keel_cte_2" ("c1", "c2", "c3", "c4") AS (
         SELECT m2."shipment_id" AS c1, m2."organization_id" AS c2, m2."business_unit_id" AS c3,
                m2."id" AS c4
           FROM "moves" m2
          WHERE m2."organization_id" = $2 AND m2."business_unit_id" = $3),
       "keel_cte_3" ("c1", "c2", "c3", "c4") AS (
         SELECT s4."move_id" AS c1, s4."organization_id" AS c2, s4."business_unit_id" AS c3,
                l."city" AS c4
           FROM "stops" s4
           LEFT JOIN "locations" l ON l."id" = s4."location_id"
                AND l."organization_id" = $2 AND l."business_unit_id" = $3
          WHERE s4."organization_id" = $2 AND s4."business_unit_id" = $3)
  SELECT c2."name" AS c1, count(DISTINCT kc3."c4") AS c2
    FROM "shipments" s3
    LEFT JOIN "customers" c2 ON c2."id" = s3."customer_id"
         AND c2."organization_id" = $2 AND c2."business_unit_id" = $3 AND (c2."code" <> $5)
    JOIN "keel_cte_2" kc2 ON (kc2."c1" = s3."id")
         AND (kc2."c2" = s3."organization_id") AND (kc2."c3" = s3."business_unit_id")
    JOIN "keel_cte_3" kc3 ON (kc3."c1" = kc2."c4")
         AND (kc3."c2" = kc2."c2") AND (kc3."c3" = kc2."c3")
   WHERE s3."organization_id" = $2 AND s3."business_unit_id" = $3 AND s3."pro_number" <> $4
   GROUP BY c2."name")
SELECT ks."c1" AS c1, ks."c2" AS c2, ks."c3" AS c3, coalesce(kc."c2", $1::bigint) AS c4
  FROM "keel_sub" ks
  LEFT JOIN "keel_cte" kc ON (NOT (ks."c1" IS DISTINCT FROM kc."c1"))
 ORDER BY ks."c2" DESC NULLS LAST, ks."c1" ASC
 LIMIT 101
-- $1 = 0, $2 $3 = the tenant, $4 = 'P01', $5 = 'G'
```

The row rules appear on every reference to their tables, the join `ON` clauses included. The count of
stops is pre-aggregated per shipment over every parent (no filter selects parents here), and the
distinct count runs over the chain of child rows and is joined back on the customer, so neither can
inflate the revenue. Each level of the chain joins its parent on the relation's keys and the tenant
columns the two tables share; under a tenant scope those equalities are implied by the tenant
predicates (Postgres folds them into constants), and across tenants they keep a row of one tenant from
joining rows of another whose keys repeat. A many-to-many level reads its join table in a CTE of its
own, as a tenant table (`schema.Table.JoinTableOf`): filtered by the scope's tenant and joined on its
keys and tenant columns. A statement names its CTEs in the order it uses them, across every query
nested in it, so no name shadows another.

A pivot of revenue by the two statuses with the most revenue, plus `Other`: the discovery statement
runs first, then the pivot with the values it found bound as parameters, in one repeatable-read
transaction:

```sql
SELECT CAST(s."status" AS text) AS c1, sum(s."total") AS c2
  FROM "shipments" s
 WHERE s."organization_id" = $1 AND s."business_unit_id" = $2 AND (s."status" IS NOT NULL)
 GROUP BY s."status"
 ORDER BY sum(s."total") DESC NULLS LAST, CAST(s."status" AS text) ASC
 LIMIT 2

SELECT c."name" AS c1,
       sum(s."total") FILTER (WHERE (s."status" = $3)) AS c2,
       sum(s."total") FILTER (WHERE (s."status" = $4)) AS c3,
       sum(s."total") FILTER (WHERE ((s."status" IS NULL) OR (s."status" <> ALL($5)))) AS c4
  FROM "shipments" s
  LEFT JOIN "customers" c ON c."id" = s."customer_id"
       AND c."organization_id" = $1 AND c."business_unit_id" = $2
 WHERE s."organization_id" = $1 AND s."business_unit_id" = $2
 GROUP BY c."name"
 ORDER BY c."name" ASC
 LIMIT 100
-- $3 = 'New', $4 = 'Delivered', $5 = {New,Delivered}
```

Two statements are needed because the second one's columns depend on the first one's values. Binding
the values, instead of writing them into the SQL, lets Postgres reuse the pivot's plan whichever values
come back.

### keel core hooks the engine added

- **`keel.WithReadFilter(ctx, f)`** applies `f`'s predicates to every table reference a read statement
  makes: the base table's WHERE, each joined relation's `ON` clause, picks and each table they walk,
  EXISTS, subqueries, CTE bodies and pre-aggregations. Interceptors only cover a statement's base table.
  Any keel application can use it for row-level rules.
- **`keel.ExecAll(ctx, db, sql, args...)`** runs one statement on the primary and every replica, for
  statements about the servers rather than the data, such as cancelling tagged backends.
- **`DynPreAgg.AllParents()`** makes a CTE pre-aggregation read every parent in scope instead of only
  the parents the query's filters select.

### Performance

On a 4-core cloud VM (`go test -bench . ./analytics`, local PostgreSQL 18):

| Work | Time |
|---|---|
| Compile a typical report (two dimensions, four measures including a filtered count and a to-many count, a calculation, a window, a total) | 76 µs, 677 allocations; 96 µs with rendering the SQL |
| Preview a grouped report over 100,000 shipments and 200,000 moves (customer × month, sum, count, to-many count) | 221 ms through the runner, against 230 ms for the hand-written statement through pgx (within noise of each other across runs) |
| The typical report above over the same data, with the cost guard and its estimates cached | 302 ms |
| The typical report answered from the cache | 2.0 ms |

Trenova's 43 canned reports, ported to an equivalent catalog, compile and run unchanged, except three
that average instants (`avg` takes numbers), which are refused with a clear error.
