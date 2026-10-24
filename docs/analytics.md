# Analytics

Status: design, ready to build. This document describes `keel/analytics`, a semantic layer that lets an
application's users build their own reports — choose an entity, pick fields across relations, filter,
group, aggregate, pivot, calculate, chart — and have keel turn that into safe, fast, tenant-scoped SQL. It
is written for Trenova's report builder first and for any keel application second.

## What Trenova has today

Three discovery passes over Trenova's reporting (about 50,000 lines across the server, the client builder
and the agent tools) found a capable product built on hand-made plumbing.

**The pieces**

| Piece | Where | Size |
|---|---|---|
| Report definition ("IR"), JSON in `report_definitions.definition` | `workspace/domain/report` | 2.4k lines |
| Catalog: 81 entities, 1,950 fields, 128 edges, generated from Bun tags (parsed from the AST) plus a curated YAML manifest | `pkg/reportcatalog`, `database/reportcatalog` | 24k generated + 1.7k YAML |
| Compiler: validate → authorize → plan joins → emit SQL by string concatenation | `reporting/compiler` | 4.2k |
| Executor: separate Bun pool over `database/sql`, text protocol, `[]any` rows | `reporting/executor` | 0.3k |
| Result cache: Redis, keyed by SQL, args and per-table data versions fed by CDC | `reporting/resultcache` | 0.4k |
| Formatting: display specs resolved and formatted by a Rust/WASM module shared with the browser | `pkg/reportfmt`, `crates/rules` | 1k + Rust |
| Renderers: CSV, JSON, XLSX (excelize), PDF (Gotenberg) | `reporting/render` | 1.5k |
| Runs, schedules, alerts, delivery on Temporal; dashboards; drill-through; run diffs; agent tools | `reportjobs`, `app/reporting`, `pkg/reportdiff`, agent packages | 15k+ |

**What users can express:**
- **Columns:** dimensions, measures (count, count distinct, sum, avg, min, max), measures with their own
  filter (`FILTER (WHERE …)`), and binary arithmetic between measures.
- **Shaping:** date buckets (day to year), numeric bands, transforms (round, abs, upper, …), nested
  AND/OR filters with relative dates, HAVING.
- **Layout:** one static pivot with listed values, a grand-total row, sort and limit.
- **Parameters:** they fill filter values only.
- **Presentation:** charts (bar to map), conditional formatting and drill-through.

**What it cannot express:**
- window functions: running totals, percent of total, rank, moving averages;
- period-over-period comparison in SQL;
- top-N per group;
- subtotals and rollups;
- row-level formulas (CASE, concatenation, date difference) and nested calculations;
- count distinct across a to-many path;
- pivots whose values are discovered when the report runs;
- parameters that drive anything but filter values;
- server-side paging of results.

**Structural problems the design removes**
1. **SQL by concatenation.**
   - Text and arguments are threaded together by hand. AVG with a filter appends its arguments twice,
     pivot "Other" duplicates its expression's arguments, and constants are inlined into the SQL to
     avoid desyncing the list.
   - It only works because Bun formats values into the text on the client. Statements are never
     prepared, plans are never reused, and the same bucket expression in SELECT and GROUP BY matches
     only because the inlined text is identical.
2. **Tenancy by hand.**
   - `scopeConds` is called at six join sites: the base table, each LEFT JOIN, each lateral, EXISTS,
     each pick hop and the m2m through table.
   - Every new join shape must remember it.
3. **Three sources of truth for the schema:**
   - Bun struct tags;
   - an AST parser (`structparse`) that reads them;
   - a 1,700-line YAML file of labels, formats, enum labels, exclusions and pick edges.

   On top of that, the client hard-codes the server's rules in TypeScript, with comments saying it
   mirrors them: operators per type, display styles, transforms, chart rules and output column naming.
4. **Correlated subqueries for every to-many measure.** Each one is a `LEFT JOIN LATERAL` evaluated once
   per base row.
5. **Totals in a second scan.** The grand total runs as a separate query in another transaction, so it
   can disagree with the rows under concurrent writes.
6. **Guards on one path only.**
   - The EXPLAIN cost preflight and the per-organization limits apply to Temporal runs only.
   - Previews, dashboards, drill-through, dashboard export and a 5M-row synchronous CSV stream have
     no cost check and no organization limit.
   - The preview limit (100 rows) is not pushed into SQL: the database produces up to 100,000 rows and
     the reader keeps 100.
7. **Freshness is the TTL.** The result cache keys on per-table data versions bumped from the
   `reporting:cdc` stream, but nothing writes that stream, so every version is -1.
8. **Everything on the primary.** There is no read replica, and no rollups or pre-aggregation.
9. **Hand-mapped definitions.** The IR is mapped twice: 1,064 lines of GraphQL mapping and a separate JSON
   normaliser for the AI agent. Dashboard filter semantics are written in both Go and TypeScript.

Defects found along the way are listed at the end. Some are security- or correctness-relevant and should
be fixed whatever happens to this design.

## The split: what keel owns

keel owns everything from the schema to the result bytes; the application owns the product around it.

| keel/analytics | The application (Trenova) |
|---|---|
| The catalog, generated by `keel gen` from the models | One word per reportable model, and tag options where a derived default is wrong |
| The report definition (IR): types, JSON encoding, JSON Schema, versioning and upgrades, TypeScript types | Storing definitions, revisions, sharing, visibility, canned reports |
| Validation with clear, user-facing errors and suggestions | Translating the error codes it shows |
| Access: a policy port consulted by the catalog view and the compiler alike | The policy itself: Trenova's resources, field groups, reference views, sensitivity |
| Compilation to bound, prepared, tenant-scoped SQL; drill-through; dashboard filter merging | Dashboards' storage and layout |
| Execution on a replica, streaming, typed columnar results, totals and subtotals in the same statement | Temporal workflows for long runs, artifacts in object storage |
| Guards: complexity, EXPLAIN cost, row caps pushed into SQL, per-tenant concurrency, supersede | The limits it configures |
| Freshness: data versions maintained by keel's write path; a cache port | The cache backend (Redis, or keel's Postgres implementation) |
| Rollups: declared aggregate tables, incremental refresh, automatic routing | Scheduling the refresher |
| Display specs, their resolution and a Go formatter; CSV, streaming XLSX, JSON and Arrow writers; run diffs | PDF (Gotenberg), email, notifications, schedules, alerts |

Delivery, schedules and dashboards stay in the application because they're product behaviour bound to its
users, notifications and storage. Everything they need from the data side becomes one call into keel.

## Model metadata: a keel tag and typed declarations

### The tag

Models move from `db:"…"` to `keel:"…"`. The `db` tag is shared with pgx's `RowToStructByName`, sqlx and
others, so keel can't reject an option it doesn't know without breaking them. Under its own key, keel
validates every option and fails `keel gen` on a typo.

```go
type Shipment struct {
    keel.Table `keel:"shipments"`

    ID             ids.ID `keel:"id,pk,autoid:shp_"`
    OrganizationID ids.ID `keel:"organization_id,pk,tenant"`
    BusinessUnitID ids.ID `keel:"business_unit_id,pk,tenant"`

    // ProNumber is the carrier's progressive number, unique per tenant.
    ProNumber string `keel:"pro_number"`
    // TotalCharge is the shipment's billed total.
    TotalCharge money.Amount `keel:"total_charge_amount"`
    Status      Status       `keel:"status"`
    CustomerID  ids.ID       `keel:"customer_id"`
    Customer    *Customer    `keel:"belongs:customer_id"`
    Moves       []*Move      `keel:"many:shipment_id"`
}
```

The tag carries storage facts (column, keys, tenant, version, generated columns, relations) and, only
where a derived default is wrong, short report overrides (`label=`, `format=`, `noreport`, `type=`,
`agg=`). Everything longer comes from the source itself: descriptions from doc comments, enum labels from
constants, formats from a `KeelFormat()` method on the field's type. The guide's tag reference lists every
option; `keel gen` rejects anything else with the field's position.

### What keel gen reads from the source

`keel gen` already loads packages with go/types. It now also records:
- **Doc comments** of fields and models, which become catalog descriptions. The comment a developer
  writes for the code is the description a report author reads.
- **Enum values:** the constants of a named type used as a column, with their doc comments as
  descriptions and a derived label (`InTransit` → "In Transit"). This replaces the manifest's
  `enumValues`.

Both land in `schema.Column` (`Doc`, `Enum []EnumValue`), available at run time to every keel feature, not
only analytics.

### The catalog is generated, not declared

Nothing about a field is written twice. `keel gen` derives the whole catalog from the models; a developer
adds one word to make a model reportable and a tag option only where a default is wrong.

```go
// Shipment is a customer's freight moving from origin to destination.
type Shipment struct {
    keel.Table `keel:"shipments,report=Operations"`

    ID             ids.ID       `keel:"id,pk,autoid:shp_"`
    OrganizationID ids.ID       `keel:"organization_id,pk,tenant"`
    BusinessUnitID ids.ID       `keel:"business_unit_id,pk,tenant"`
    // ProNumber is the carrier's progressive number.
    ProNumber   string       `keel:"pro_number"`
    TotalCharge money.Amount `keel:"total_charge_amount"`
    Weight      int64        `keel:"weight,format=weight"`
    ShippedAt   int64        `keel:"shipped_at"`
    Status      Status       `keel:"status"`
    InternalKey string       `keel:"internal_key,noreport"`
    Customer    *Customer    `keel:"belongs:customer_id"`
    Moves       []*Move      `keel:"many:shipment_id"`
    OriginStop  *Stop        `keel:"pick:moves.stops,first=moves.sequence|stops.sequence"`
}
```

What `keel gen` derives, and the option that overrides it when needed:

| Catalog fact | Derived from | Override |
|---|---|---|
| Reportable entity, category | `report[=Category]` on the table tag | — |
| Label | the field or model name, split on case with an acronym list given to `keel gen -acronyms PRO,BOL,SCAC,DOT,MC` (`ProNumber` → "PRO Number") | `label=` |
| Description | the field's and model's doc comments | — |
| Included fields | every column except tenant, version, `noselect`, bytea, tsvector and the foreign keys a relation already exposes | `noreport` to exclude; `report` to include an excluded kind |
| Value type | the Go and SQL type; `int64` columns named `*_at`, `*_date`, `*_time` are epochs | `type=epoch` |
| Measure or dimension | numeric non-key, non-enum columns are measures (and bandable dimensions); everything else is a dimension | `dimension`, `measure` |
| Aggregations | the value type (sum/avg for numbers, min/max for ordered types, count/count distinct for all) | `agg=sum\|avg` |
| Format | a `KeelFormat() string` method on the field's type, written once per type (`func (Amount) KeelFormat() string { return "money" }`) | `format=` |
| Enum values and labels | the named type's constants and their doc comments | — |
| Edges | the model's relations, with derived labels | `noreport` on the relation |
| Pick edges | a `pick:` relation, which keel itself can also load (a shipment's origin stop) | — |

`analytics.Build(keelkinds.Tables...)` turns the generated schema into a `*Catalog` at startup, checking the
graph once: picks walk real paths with a total order, edges point at reportable entities, tenant columns are
complete. Its `Version` is a hash of everything a definition depends on. `Label`s are translation keys
(`shipment.proNumber`) with the derived text as fallback; `keel analytics i18n` writes them as a message
catalog for the application's translation pipeline, and `analytics.Options.Translate` reads translations at
run time.

Access needs no annotation either. The policy receives entity and field keys and resolves them against the
application's own registry; Trenova's resources, field groups and reference views already exist there.

Trenova's 1,950 hand-curated catalog fields become what `keel gen` derives from its models, with options on
the few fields whose defaults are wrong. `reportcatalog.yml`, its generator and the 24,000-line literal are
deleted, and nothing is maintained by hand.

## The report definition

`analytics.Definition` is keel's IR. It keeps every shape Trenova's IR has, so the migration is mechanical,
and adds the missing ones. It has a JSON encoding, a published JSON Schema (used to validate GraphQL input
and to constrain the AI agent), and a version with upgrade functions, so stored definitions move forward
when the IR does.

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

**What the IR adds**

| Addition | Compiles to |
|---|---|
| **Calculations** in a small typed formula language: arithmetic, comparisons, `if`, `case`, `coalesce`, `nullif`, `round`/`floor`/`ceil`, `abs`, `concat`, `lower`/`upper`/`trim`, `date_diff`, `date_trunc`, `extract`, references to columns (`[revenue]`) and fields (`{customer.name}`) | typed SQL expressions with bound parameters; type-checked against the catalog before anything runs |
| **Row-level calculations** (no aggregate inside) usable as dimensions and in filters | the expression in SELECT, WHERE and GROUP BY |
| **Window columns**: running total, percent of total, rank, dense rank, moving average, change and percent change against an offset period, first/last in partition | `OVER (PARTITION BY … ORDER BY … ROWS …)` over the grouped result |
| **Top-N per group** | a ranked subquery filtered on rank |
| **Subtotals and total** | `GROUPING SETS` in the same statement, with `GROUPING()` marking each row's level: one scan, consistent with the rows, and compatible with HAVING |
| **Dynamic pivots**: values discovered at run time (top K by a measure, plus "Other") as well as listed ones | a first statement that picks the values, then the pivot; both in one round trip with `keel.Batch` |
| **Parameters everywhere**: filter values, limits, top-N, bucket, pivot values | bound values; never identifiers |
| **Count distinct across to-many paths** | a distinct count over the joined path, which a fan-out cannot inflate |
| **Paging** | keyset paging over the sorted output, so a preview can scroll a large result without re-running it from the start |

Trenova's structured "computed" spec (`add`/`subtract`/`multiply`/`divide`) becomes a formula. The builder
can keep its structured editor and produce a formula AST (the JSON encoding accepts both the text form and
the AST), and gain a formula field for power users.

**Operators and rules are data.** `catalog.Rules()` publishes the operator table per value type, the
transforms, buckets, bands, display styles per type, chart encodings and output column naming. The
generated TypeScript package (below) carries the same tables, so the client stops mirroring server code
by hand.

## Access: one policy, consulted everywhere

```go
type Policy interface {
    Entity(ctx context.Context, e *analytics.Entity) analytics.Access
    Field(ctx context.Context, f *analytics.Field, via analytics.Path) analytics.Access
    Edge(ctx context.Context, e *analytics.EdgeInfo, via analytics.Path) analytics.Access
    Rows(ctx context.Context, e *analytics.Entity) []keel.Pred
}
```

- **One rule for both sides.** `catalog.For(ctx, policy)` returns the caller's view: entities they can
  start from, fields and edges with an availability and a reason code (`analytics.Denied("needs_field_group")`).
  The compiler consults the same policy, so the builder offers exactly what the compiler accepts, which
  Trenova guarantees today by sharing `reportaccess` by hand.
- **Batched.** The policy is asked once per entity, field and edge per request. Trenova's batched
  permission load stays inside its policy implementation.
- **Row rules.** `Rows` returns typed predicates ("own records only": `shipments.owner_id = $user`) that
  keel applies on every table reference the statement makes: base, joins, picks, EXISTS, subqueries,
  pre-aggregations. It sits beside tenancy, which keel applies by construction exactly as for every
  other keel query.
- **Sensitivity propagates.** Each output column carries the highest sensitivity of the fields it reads,
  through calculations and windows.
- **Re-checking a definition.** `catalog.Check(def, policy)` re-validates a stored definition and returns
  diagnostics. Trenova's "needs attention" status, which nothing sets today, becomes a sweep over
  definitions whose `CatalogVersion` differs.

A keel application without permissions passes no policy; tenancy still applies.

## Compilation

The compiler is built on keel's renderer and a new dynamic layer in keel core (below), not on strings:

- **Bound, deduplicated parameters.** The renderer gives equal values the same `$n`, so an expression in
  SELECT and GROUP BY renders identically with real bind parameters. GROUP BY refers to output positions
  where Postgres allows it. Statements are prepared and their plans reused across runs of the same report
  shape.
- **Tenancy and row rules by construction.** Every table reference the compiler emits goes through
  keel's scope rendering; there is no join site that can forget it.
- **Readable, deterministic aliases**: `s` for shipments, `c` for its customer, `m_d` for a to-many path
  through moves. `ToSQL` output is something a developer or a support engineer can read.
- **Fan-out without correlated subqueries.** A measure across a to-many path is pre-aggregated in a
  grouped CTE keyed by the parent, filtered to the parents the report selects:

  ```sql
  WITH m AS (
    SELECT mv.shipment_id, sum(mv.distance) AS distance
      FROM moves mv
     WHERE mv.organization_id = $1 AND mv.business_unit_id = $2
       AND mv.shipment_id IN (SELECT s.id FROM shipments s WHERE …)
     GROUP BY mv.shipment_id)
  SELECT c.name, sum(s.total_charge_amount), sum(m.distance)
    FROM shipments s
    LEFT JOIN customers c ON …
    LEFT JOIN m ON m.shipment_id = s.id
   WHERE …
   GROUP BY 1
  ```

  This is one pass over each child table instead of one subquery per base row. Lateral joins remain for
  picks and per-group limits, where they are the right tool.
- **Totals and subtotals as grouping sets**, in the same statement and snapshot as the rows.
- **Preview limits in SQL.** A preview asks for `LIMIT n+1` and reports truncation, instead of letting
  the database produce 100,000 rows to keep 100.
- **Drill-through** is `analytics.Drill(def, row, column)`. It inverts buckets and bands into half-open
  ranges in the report's time zone, carries the clicked measure's filter and pivot scope, and returns a
  record-list definition.
- **Dashboard filters** are `analytics.ApplyFilters(def, filters, params)`, the one implementation of
  "replace the condition on the same field, otherwise AND on top; bind by explicit mapping, then by name".
  The generated TypeScript package exports the same function for the client.
- **Time zones.** A bucket is computed in the report's zone and returned as the instant it starts. A
  bucket is rendered from that instant, so the offset error found in Trenova (local wall-clock time read
  as a UTC epoch, then shifted again when formatted) cannot occur.

## Execution

- **Replicas.** keel core gains read replicas: `Config.Replicas` with a lag bound, and
  `keel.OnReplica(ctx)` to route reads. Analytics reads from a replica by default and falls back to the
  primary when every replica is lagging past the bound. Reporting load leaves the database that serves
  writes.
- **Read-only and bounded.** Statements run in a read-only transaction with a statement timeout set for
  that statement (keel's `Timeout`), not a pool-wide connection parameter.
- **Typed columnar results.** `Result` holds one typed vector per column (`[]int64`, `[]decimal`, `[]bool`,
  `[]string` with shared values, …) and a null bitmap, decoded with pgx scan plans in binary format. That
  is fewer allocations than `[]any` rows, and it maps directly to the JSON, CSV, XLSX and Arrow writers.
- **Streaming.** `Stream(ctx, …)` yields row batches for exports of any size, with the same guards.
- **One round trip** for a report and its dynamic pivot values or its row count, through `keel.Batch`.

## Guards, on every path

Every entry point (preview, run, drill, dashboard tile, export, stream, agent tool) goes through one
`analytics.Runner` with the same guards. "Only Temporal runs are checked" can't happen again.

| Guard | How |
|---|---|
| Complexity | joins, to-many paths, dimensions, pivot columns, path depth, formula depth, window count; configured limits with a score |
| Cost | `EXPLAIN (FORMAT JSON)` total cost and rows against limits, cached per statement shape for a short time so a busy dashboard pays it once |
| Rows and bytes | limits pushed into SQL; byte caps on writers |
| Per-tenant concurrency | a distributed semaphore built on Postgres advisory locks (N slots per tenant, `pg_try_advisory_lock(tenant_hash, slot)`), shared by every process with no extra infrastructure |
| Supersede | a statement can be tagged with a key (a user's preview); starting another with the same key cancels the first across processes, by finding it through `application_name` in `pg_stat_activity` and calling `pg_cancel_backend` |
| Timeouts | per entry point (preview seconds, run minutes) |

## Freshness and caching

The result cache is a port (`analytics.Cache`), with an in-memory implementation, a Postgres
implementation (an unlogged table) and room for Redis. Its key covers:
- the statement and its arguments;
- the policy's row rules;
- the output format;
- the data versions of every table the statement reads.

**Data versions come from keel's write path, not CDC.** Every write in a keel application goes through
keel, so keel knows which (tenant, table) pairs a transaction changed. After commit, keel bumps them in
`keel_data_versions`, batched and debounced (one upsert per pair per second at most), so write
transactions never contend on a version row. A report reading `shipments` and `customers` for tenant T is
fresh exactly until one of those changes for T.

The cache applies to every entry point, previews and dashboards included, which is where repeated reads
actually happen.

> **Status: built.** `analytics/cache` has the in-memory store (sharded LRU with TTLs), the Postgres
> store (`keel_analytics_cache`, unlogged, created on first use, batched expiry cleanup, capped
> values) and a Redis adapter over a three-method client interface, plus `cache.Loader`, which shares
> one load between concurrent misses of a key. The engine adapts `cache.Store` to `analytics.Cache`.
> See "Analytics: caching and rollups" in the guide.

## Rollups

For tenants with millions of rows, the fastest query is one that reads a small pre-aggregated table:

```go
var ShipmentsDaily = analytics.Rollup("shipments_daily", Shipments, analytics.RollupOptions{
    Grain:      analytics.Day(shipmentdb.Shipments.ShippedAt),
    Dimensions: []analytics.FieldRef{shipmentdb.Shipments.CustomerID, shipmentdb.Shipments.Status},
    Measures:   analytics.Sums(shipmentdb.Shipments.TotalCharge, shipmentdb.Shipments.Weight).Count(),
})
```

- **Storage.** A rollup is a tenant table keel creates and maintains, with a migration generated by
  `keel analytics rollups` for keel/migrate.
- **Incremental maintenance.** keel's write path records the (tenant, grain bucket) slices a
  transaction touched in `keel_rollup_dirty`. `analytics.Refresh(ctx, db, rollups...)` recomputes only
  the dirty slices, idempotently. The application schedules it (Trenova on Temporal; others with a
  ticker).
- **Routing.** The compiler answers a definition from a rollup when the rollup covers it: dimensions
  within the rollup's, filters on its dimensions or grain, measures composable (sum, count, min, max;
  avg as sum over count). Users and authors never choose; `ToSQL` shows which table answered. Count
  distinct is answered from a rollup only at the rollup's exact grain.
- **Freshness.** A rollup slice pending refresh is detected through the dirty table, and the compiler
  falls back to the base tables for that report.

> **Status: built, with one change.** `analytics/rollup` declares rollups with `rollup.MustDefine(name,
> table, rollup.Day(col), rollup.Dims(...), rollup.Measures(...))`, generates their migrations
> (`keel analytics rollups migration`), refreshes dirty slices concurrently with `SKIP LOCKED` and
> advisory locks, and routes resolved definitions through `rollup.Router`, which checks freshness
> against the dirty table. The change: slices are marked by statement triggers with transition
> tables, written by the rollup's migration, rather than by keel's write path. Triggers mark in the
> write's transaction with one statement per write statement, see writes keel cannot (`Exec`,
> `InsertFrom`, `COPY`, other services), and work in implicit transactions, where a table hook's
> context carries no transaction to write in. Marks are keyed by transaction id, so writers never
> contend; a refresh deletes only committed marks and reads the source afterwards, so it never
> misses a committed write.

## Formatting and exports

- **`analytics/format`.** Owns the display spec (Trenova's `reportfmt.Spec`: styles, decimals, currency,
  negative style, notation, prefixes, date and bool styles, durations, null text, conditional tones and
  bands), its resolution from field type, format hint and author overrides, and a Go formatter that uses
  CLDR data from `golang.org/x/text` for locale-correct numbers, currencies and dates.
- **Generated TypeScript package.** The client gets a formatter that implements the same spec. Trenova
  can keep its WASM module behind it or drop the WASM; either way, formatting rules have one definition.
- **`analytics/export`** has four writers:
  - **CSV:** streaming, with an optional BOM, and formula-injection escaping (cells starting with
    `=`, `+`, `-`, `@` are prefixed). Trenova's CSV currently has none.
  - **XLSX:** a true streaming writer that writes the zip and sheet XML as rows arrive, with typed cells,
    number formats, tones and sheet splitting at 1,048,575 rows. excelize's stream writer still
    finalises the workbook in memory, and Trenova builds the dashboard workbook fully in memory.
  - **JSON:** the raw-value envelope Trenova uses for its run sidecars.
  - **Arrow IPC:** for notebooks and the agent's pandas sandbox.

  PDF stays with the application's renderer.
- **Run diffs.** `analytics.Diff(a, b, keys)` is Trenova's `reportdiff`: rows matched by key, exact
  decimal comparison, added/removed/changed/duplicate, ranked. It becomes available to schedules and
  emails as well as the agent.

## Generated TypeScript

`keel analytics ts -out client/packages/analytics` writes a TypeScript package with:
- the IR types and the JSON Schema;
- `catalog.Rules()` as data: operators per type, transforms, buckets, bands, display styles, chart
  encodings and output column naming;
- `applyFilters` for dashboards;
- the display spec and a formatter;
- the formula grammar, for an editor with completion.

A CI check (`-check`) keeps it in step with the server. The client's hand-copied rule tables and its copy
of the dashboard semantics are deleted.

> **Status: generation built.** `keel analytics ts [-check] -out path` writes this module through
> `analytics/ir/tsgen` (`analytics/analyticscli`); `keel analytics i18n` writes the label message
> catalog.

## For the AI agent

- **`catalog.For(ctx, policy).Describe()`** returns a compact text description of the caller's catalog
  for a model's context.
- **The JSON Schema** constrains generated definitions, so the shape normaliser the agent needs today
  goes away.
- **Corrective errors.** Validation errors carry suggestions ("unknown field `customer.nam`; did you
  mean `customer.name`?"), which replaces `report_hints.go`.

> **Status: schema description built.** `keel analytics describe` writes a compact description of
> the reportable entities, fields, relations and rollups from the schema; the policy-aware
> `catalog.For(ctx, policy).Describe()` belongs to the engine.

## keel core additions this needs

| Addition | Also useful for |
|---|---|
| **Dynamic queries**: build a query over a `schema.Table` by column and relation name at run time, with keel's expressions, tenancy and binding; identifiers only ever come from the schema | data tables with user-chosen columns and filters, generic exports, admin tools |
| **Parameter deduplication** in the renderer | any query repeating a value |
| **`GROUPING SETS` / `ROLLUP` / `CUBE`** and full window frames in the typed API | dashboards and summaries in application code |
| **Read replicas** with lag bound and routing | every read-heavy page |
| **Doc comments and enum values** in the schema | API docs, admin UIs, validation messages |
| **The `keel` tag** | every model |
| **`Explain`** on any query | debugging, guards |
| **Data versions** maintained after commit | any cache keyed on table changes |

## Moving Trenova onto it

1. Rename tags to `keel:` (with the rest of the keel migration). Mark the manifest's 81 entities
   `report=<Category>`, add `KeelFormat` to the money, weight and distance types, move the three pick
   edges to `pick:` relations, and carry over only the labels and exclusions the derivation gets wrong
   (a script compares the derived catalog with the old one and prints the differences). Delete
   `reportcatalog.yml`, `reportcataloggen`, `structparse`'s report use and `catalog_gen.go`.
2. Implement `analytics.Policy` over `reportaccess`, keeping its decisions and reason codes.
3. Convert stored definitions with an upgrade function (IR v1 to v2: computed specs to formulas,
   `having` by ref+agg to column references, pivots unchanged). Run it as a migration over
   `report_definitions` and their revisions.
4. Replace the compiler, executor, result cache, `reportrows`, `reportdiff`, drill-through,
   `dashboard_resolve` and the dashboard export's query path with `analytics` calls. Previews,
   dashboards and the CSV stream go through the same `Runner` and guards as runs.
5. GraphQL takes the definition as a JSON scalar validated against keel's schema. That deletes the
   1,064-line mapping and the agent's normaliser.
6. The client imports the generated package and deletes its mirrored rules.
7. Configure a replica, the cache, the per-tenant semaphore and the first rollups (shipments, invoices
   and moves by day).

## Defects found in Trenova during discovery

Fix these regardless of this design:

| # | Defect | Where |
|---|---|---|
| 1 | Every GraphQL schedule update clears the schedule's alert and inline digest: the resolver rebuilds the input without `Alert` and `EmailInline` | `report.resolvers.go:219-235`, `reportmapping.go:502-547` |
| 2 | `reportSchedules` lists every schedule in the tenant with its email recipients, whatever the caller's ownership or the report's visibility | `schedules.go:160-185`, `report.resolvers.go:634` |
| 3 | CSV exports do not escape formula-leading cells (`=`, `+`, `-`, `@`), a spreadsheet injection vector | `render/csv.go` |
| 4 | Previews, dashboard tiles, drill-through, dashboard export and the 5M-row synchronous CSV stream skip the cost preflight and the per-org limit | `preview.go`, `dashboardexport.go`, `stream.go` |
| 5 | The result cache's CDC stream `reporting:cdc` has no producer; freshness is the 15-minute TTL | `resultcache/consumer.go`, `deploy/gtc/sinks.yaml` |
| 6 | Date buckets likely render one period early for organizations west of UTC (local wall time encoded as a UTC epoch, then formatted in the org zone) | `compiler/outputs.go:162-165`, `reportfmt/format.go:101` (not confirmed by running) |
| 7 | `ListDefinitions` filters visibility after LIMIT/OFFSET, so pages come back short | `service.go:282-304` |
| 8 | Table export uses the first return of `ResolvePath` (the base entity) as the field's entity, so nested columns are skipped or wrongly accepted | `tableexportservice/definition.go:57` |
| 9 | `needs_attention` is never set: `UpdateStatus` has no callers | `reportrepository` |
| 10 | Totals run in a separate transaction and can disagree with the rows | `executor.go:156-193` |
| 11 | The PDF row-cap error is retryable, so Temporal re-runs the whole query | `render/pdf.go:142-147` |
| 12 | In-app delivery notifies recipients about a run they cannot open; the email links to a generic page | `runs.go:207-217`, `delivery.go:78-81` |
| 13 | Parameters print in map order on the XLSX info sheet and in PDFs | `xlsx.go:291`, `pdf.go:134` |
| 14 | Preview SQL carries `LIMIT 100000` while the reader keeps 100 rows | `preview.go:128`, `emit.go:107-112` |

## Decisions and trade-offs

- **A semantic layer, not a SQL editor.** Users never write SQL. A typed IR over a declared catalog
  keeps tenancy, access and cost enforceable; a SQL box makes all three guesswork.
- **Derived, not declared.** The catalog comes from the models: names, doc comments, types, relations
  and constants already say almost everything. Tag options cover the rest. A hand-written manifest or
  declaration file is a second copy of the schema that drifts; there is none.
- **Opt-in models, opt-out fields.** One word makes a model reportable; fields follow by rule, with
  keys, tenant and internal columns excluded automatically.
- **Pre-aggregated CTEs over laterals.** One pass per child table scales with data size, not with
  base-row count times subqueries. Laterals stay for picks and per-group limits.
- **Data versions from the write path, not CDC.** keel sees every write, so it can maintain versions
  without another pipeline. Writes made outside keel (manual SQL, other services) are invisible to
  it; the TTL remains the backstop for those.
- **Rollups owned by keel.** Aggregate awareness has to live in the compiler to be automatic, and
  maintenance has to see writes; both are keel. Scheduling stays with the application.
- **Product features stay in the application.** Schedules, delivery, dashboards and sharing differ
  between products; the data path does not.
