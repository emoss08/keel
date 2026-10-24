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
  - [Pick relations](#pick-relations)
  - [Derived metadata](#derived-metadata)
- [Generating code](#generating-code)
- [Reading](#reading)
- [Loading relations](#loading-relations)
- [Pagination](#pagination)
- [Batching reads](#batching-reads)
- [Writing](#writing)
- [Transactions](#transactions)
- [Tenancy](#tenancy)
- [Hooks, interceptors and observers](#hooks-interceptors-and-observers)
- [Errors](#errors)
- [Migrations](#migrations)
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
| `github.com/emoss08/keel/cmd/keel` | the command: `keel gen` and `keel migrate` |
| `github.com/emoss08/keel/migrate` | migrations: apply, plan, squash, lint, templates, `Require` |
| `github.com/emoss08/keel/migrate/migratecli` | the `keel migrate` commands, importable by an application's CLI |
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
| `Observers` | `[]keel.Observer` | see every statement: tracing, metrics, slow-query logs |
| `AfterConnect` | `func(ctx, *pgx.Conn) error` | runs on every new connection after keel registers its types; register codecs here |
| `LoadTypes` | `[]string` | user-defined types (domains, composites) and their array types to register on every connection |
| `SkipEnums` | `bool` | stops keel registering every enum type of the database on each new connection |
| `JSON` | `*keel.JSONCodec` | the JSON implementation for `json` and `jsonb` values; defaults to `encoding/json` |
| `PlanCache` | `keel.PlanCache` | how statements are prepared: `PlanCacheAuto` (default), `PlanCacheCustom`, `PlanCacheOff` |
| `StatementCacheCapacity` | `int` | prepared statements (or, with `PlanCacheOff`, statement descriptions) kept per connection; defaults to 512 |

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

### Enums, domains and decimals

- **Enums** are registered on every new connection without configuration: keel lists the
  database's enum types and their array types and loads them, so an enum compared with `In` binds
  as one array parameter, and enum values decode into one shared string per label, without an
  allocation per row. `SkipEnums: true` turns this off; then list each enum compared with `In`, and
  its array type (`_name`), in `LoadTypes`.
- **Domains and composite types** must be listed in `LoadTypes`, each with its array type.
- **Decimals:** register a binary codec in `AfterConnect`, for example
  `github.com/jackc/pgx-shopspring-decimal`, so numeric values travel in binary instead of being
  parsed from text:

```go
AfterConnect: func(ctx context.Context, conn *pgx.Conn) error {
    pgxdecimal.Register(conn.TypeMap())
    return nil
},
```

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
| `autocreate` | stamped on insert; `int64` means Unix seconds, `time.Time` a timestamp | `keel:"created_at,autocreate"` |
| `autoupdate` | stamped on insert and on every update, bulk updates and upserts included | `keel:"updated_at,autoupdate"` |
| `version` | optimistic locking: `Save` compares and increments it, bulk updates and upserts increment it; must be `int64` | `keel:"version,version"` |
| `readonly` | never written (generated or trigger-maintained columns); read back after inserts and saves | `keel:"stage_rank,readonly"` |
| `noselect` | not selected by default; usable in predicates. Mark a generated `noselect` column `readonly` too | `keel:"search_vector,noselect,readonly,tsvector"` |
| `jsonb` | the value is encoded as JSON; a nil map or slice in a non-pointer field is written as `{}` or `[]` | `keel:"attributes,jsonb"` |
| `tsvector` | the column gets the full-text operators | see `noselect` |
| `label=<text>` | the column's label instead of the derived one | `keel:"pro_number,label=PRO Number"` |
| `noreport` | reports do not offer the column | `keel:"internal_key,noreport"` |
| `report` | reports offer a column they leave out by default (a tenant, version, `noselect`, bytea or tsvector column, or an exposed foreign key) | `keel:"customer_id,report"` |
| `dimension` / `measure` | reports group by / aggregate the column instead of what keel derives; `measure` needs a numeric column | `keel:"sequence,dimension"` |
| `agg=<a>\|<b>…` | the aggregations reports offer, from `count`, `count_distinct`, `sum`, `avg`, `min`, `max`; each must apply to the value type | `keel:"sequence,agg=min\|max"` |
| `format=<name>` | how reports display values (`[A-Za-z0-9_.:-]+`), instead of the type's `KeelFormat` | `keel:"weight,format=weight"` |
| `type=epoch` / `type=date` / `type=decimal` | the value type reports read: an `int64` of Unix seconds, a `time.Time` holding a date, a scanner or string holding a decimal | `keel:"total,type=decimal"` |

A model has at most one `version`, `autoid`, `autocreate` and `autoupdate` column. `readonly`
cannot be combined with `pk`, `tenant`, `version`, `autoid`, `autocreate` or `autoupdate`; `jsonb`
and `tsvector`, `report` and `noreport`, `dimension` and `measure` exclude each other. The report
options (`noreport`, `report`, `dimension`, `measure`, `agg=`, `format=`, `type=`) need `report` on
the model's table tag.

There is no `default:` or `notnull`: the SQL migrations own the schema. The value in the struct is
the value written; keel never substitutes `DEFAULT` for a zero value. A nil slice in a non-pointer
array field is written as an empty array.

**On a relation**

| Head or option | Meaning | Example |
|---|---|---|
| `belongs:<fk>` | this table's `<fk>` column references the target's primary key | `Customer *Customer \`keel:"belongs:customer_id"\`` |
| `one:<fk>` | the target's `<fk>` references this table's primary key; at most one row | `Profile *DriverProfile \`keel:"one:worker_id"\`` |
| `many:<fk>` | the target's `<fk>` references this table's primary key | `Moves []*Move \`keel:"many:shipment_id"\`` |
| `m2m:<join_table>.<local_fk>.<remote_fk>` | rows linked through a join table: `<local_fk>` references this table, `<remote_fk>` the target | `Tags []*Tag \`keel:"m2m:shipment_tags.shipment_id.tag_id"\`` |
| `pick:<relation>.<relation>…` | one row picked along a path that crosses a to-many relation (see [Pick relations](#pick-relations)) | `OriginStop *Stop \`keel:"pick:moves.stops,first=moves.sequence\|stops.sequence"\`` |
| `first=<segment>.<column>\|…` / `last=…` | a pick's order: the first or the last row by these columns; exactly one is required on `pick` and allowed nowhere else | see `pick` |
| `label=<text>` | the relation's label instead of the derived one | `keel:"belongs:bill_to_id,label=Bill-to Customer"` |
| `noreport` | reports do not offer the relation as an edge | `keel:"m2m:shipment_tags.shipment_id.tag_id,noreport"` |

### Go types

| Go field type | Column type in the generated handle |
|---|---|
| `string` | `keel.StringCol` |
| `int64`, `int32`, `int16` | `keel.Int64Col`, `keel.Int32Col`, `keel.Int16Col` |
| `float64`, `float32` | `keel.Float64Col` |
| `bool` | `keel.BoolCol` |
| `[]byte` | `keel.BytesCol` |
| `time.Time` | `keel.TimeCol` (timestamp, timestamptz or date) |
| `[]string` | `keel.StringArrayCol` |
| `[]int64` | `keel.Int64ArrayCol` |
| `string` with `tsvector` | `keel.TSVectorCol` |
| any type with `jsonb` | `keel.JSONBCol` |
| a named type over one of the above (`Status`, `ids.ID`) | a generated wrapper (`StatusCol`, `IdsIDCol`) whose methods take the named type |
| a type implementing `sql.Scanner` (`Money`, a decimal) | a generated wrapper over `keel.AnyCol` (`MoneyCol`) |

A pointer field (`*int64`, `*ids.ID`) or a `sql.Scanner` whose name starts with `Null` is nullable
and has the same column type. Plain `int` is refused, because its width is not portable; use
`int64`. Structs, maps and slices of other element types need the `jsonb` option.

### Relations

A relation field is a pointer (to-one: `belongs`, `one`, `pick`) or a slice (to-many: `many`,
`m2m`) of another model. A model may relate to itself (`Worker.Manager *Worker
\`keel:"belongs:manager_id"\``) and to the same target twice (`Customer` and `BillTo`).

Rules:

- The referenced primary key, besides its tenant columns, must be a single column. Tenant columns
  are not part of the relation key: when both sides have them, keel adds them to every join, as
  constants from the scope under a tenant scope and as column equalities under a system scope.
- Keys of to-many relations must be string or integer columns.
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
| Enum values | `Column.Enum` | the constants of the column's named string or integer type, in declaration order, from any package; labels from the constant names without the type name (`StatusInTransit` → "In Transit") | — |
| Reportable entity | `Table.Report` | `report[=Category]` on the table tag | — |
| Included | `Column.Report.Included` | every column except tenant, version, `noselect`, bytea and tsvector columns and foreign keys a reported `belongs` relation exposes | `noreport`, `report` |
| Value type | `Column.Report.Type` | the Go type: strings, enums, integers, floats, decimals (`shopspring/decimal`, `pgtype.Numeric`), bools, times, JSON, arrays, bytea, tsvector; `int64` columns named `*_at`, `*_date` or `*_time` are epochs | `type=` |
| Measure or dimension | `Column.Report.Kind` | numeric columns that are neither keys (primary keys, foreign keys of `belongs`, `one` and `many` relations) nor enums are measures; everything else is a dimension | `dimension`, `measure` |
| Aggregations | `Column.Report.Aggs` | numbers: count, count distinct, sum, avg, min, max; strings, times, dates and epochs: count, count distinct, min, max; everything else: count, count distinct | `agg=` |
| Format | `Column.Report.Format` | a `KeelFormat() string` method on the column's type, value or pointer receiver, returning a string literal: `func (Money) KeelFormat() string { return "money" }` | `format=` |

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
command that writes the files.

### What a generated package contains

The generated file is `keel_gen.go` in the `-out` directory. For each model (here `fleet.Shipment`)
it holds:

| Declaration | What it is |
|---|---|
| `type ShipmentsTable struct` | the handle type: embeds `keel.Handle[fleet.Shipment]` and has one field per column (`ID`, `ProNumber`, `Status`, …) |
| `var Shipments` | the base handle, built statically; queries and writes start here |
| `func (t *ShipmentsTable) Customer() *CustomersTable` | one method per relation, returning the handle of the related table on that path |
| `func (t *ShipmentsTable) As(alias string) *ShipmentsTable` | a second reference to the table for self-joins and correlated subqueries |
| `func (t *ShipmentsTable) Get(ctx, db, key ids.ID) (*fleet.Shipment, error)` | fetch by primary key; generated when the key, besides tenant columns, is one non-pointer column |
| `func BuildShipments(*keel.Ref) *ShipmentsTable` | builds a handle for a reference; relation and alias methods call it |
| `func ShipmentsSchema() *schema.Table` | the table metadata, written as a static literal with field accessors, hook trampolines, labels, docs, enum values and report facts |

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
exists, err := open.Exists(ctx, db)
```

Rows come back as `*T`. `Pick` with columns of the table narrows what is read: the other fields
stay zero, and keel adds the primary key columns. `Pick` with anything else (an aggregate, a
column of another table) makes the query a projection, which needs `keel.Into`.

```go
sh, err := s.Pick(s.ProNumber, s.Status).Where(s.ID.Eq(id)).One(ctx, db) // sh.Total is zero
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
    s.SearchVector.Matches("dallas reefer"),
    keel.Or(s.CanceledAt.IsNull(), s.CanceledAt.Gt(since.Unix())),
    keel.Not(s.Customer().Name.ContainsFold("test")),
).Count(ctx, db)

st := fleetdb.Stops
late, err := st.Where(st.ArrivedAt.Gte(since), st.Kind.Eq("delivery")).Count(ctx, db)
```

| Column type | Operators |
|---|---|
| every column | `IsNull`, `IsNotNull`, `EqCol`, `NeCol`, `InQuery`, `NotInQuery`, `Asc`, `Desc`, `Count`, `CountDistinct`, `Min`, `Max`, `Coalesce(v)`, `ToNull`, `ToDefault`, `ToExpr(e)` |
| `StringCol` | `Eq`, `Ne`, `Gt`, `Gte`, `Lt`, `Lte`, `Between`, `IsDistinctFrom`, `In`, `NotIn`, `To`; `Contains`, `ContainsFold`, `HasPrefix`, `HasPrefixFold`, `HasSuffix` (their argument is escaped, so `%` and `_` match themselves); `Like`, `ILike` (patterns as given); `Match`, `MatchFold` (POSIX regular expressions); `Lower` |
| `Int64Col`, `Int32Col` | `Eq`, `Ne`, `Gt`, `Gte`, `Lt`, `Lte`, `Between`, `IsDistinctFrom`, `In`, `NotIn`, `To`, `Add`, `Sum`, `Avg` |
| `Int16Col` | as `Int32Col`, without `Avg` |
| `Float64Col` | `Eq`, `Ne`, `Gt`, `Gte`, `Lt`, `Lte`, `Between`, `IsDistinctFrom`, `To`, `Add`, `Sum`, `Avg` |
| `BoolCol` | `Eq`, `IsTrue`, `IsFalse`, `To` |
| `BytesCol` | `Eq`, `To` |
| `TimeCol` | `Eq`, `Ne`, `Gt`, `Gte`, `Lt`, `Lte`, `Between`, `To` |
| `AnyCol` and its wrappers (`MoneyCol`) | `Eq`, `Ne`, `Gt`, `Gte`, `Lt`, `Lte`, `Between`, `IsDistinctFrom`, `In`, `NotIn`, `To`, `Add`, `Sum`, `Avg` |
| `JSONBCol` | `Contains(v)` (`@>`, `v` encoded as JSON), `HasKey`, `HasAnyKey`, `HasAllKeys`, `Text(path...)` (`#>>`, a `StringExpr`), `To` |
| `StringArrayCol` | `Has`, `Overlaps`, `ContainsAll`, `ContainedBy`, `Len`, `To` |
| `Int64ArrayCol` | `Has`, `Overlaps`, `ContainsAll`, `ContainedBy`, `To` |
| `TSVectorCol` | `Matches(q)` (`@@ websearch_to_tsquery(q)`), `MatchesConfig(config, q)`, `Rank(q)` |

Generated wrappers (`StatusCol`, `IdsIDCol`) take the named type in `Eq`, `Ne`, `Gt`, `Gte`, `Lt`,
`Lte`, `IsDistinctFrom`, `Between`, `To`, and for string, integer and scanner kinds `In` and
`NotIn`; the base type's other methods stay available. `In` binds one array parameter
(`= ANY($1)`), so the statement text does not depend on the number of values. `Lower()` and
`Text(...)` return a `keel.StringExpr` with `Eq`, `Ne`, `In`, `Like`, `Contains`, `IsNull`, `Asc`
and `Desc`. `EqCol` and `NeCol` compare with another expression, for joins and correlated
subqueries. `Col.Excluded()` is the value an upsert proposed for the column (`EXCLUDED.col`); it is
valid only inside `Conflict.Set` (see [Upsert](#upsert)) and fails the statement anywhere else.

Combinators: `keel.And(ps...)` and `keel.Or(ps...)` skip nil predicates (`And` of nothing is
`TRUE`, `Or` of nothing `FALSE`), `keel.Not(p)`, `keel.True()`. `Where` joins its arguments with
`AND`.

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

### Common table expressions

`keel.CTE(name, columns, body)` declares `WITH name (columns) AS (body)`. Declare it on the query
with `With`, then join it, filter on its columns with `Col(name)` (an `AnyCol`), or read it through
`Query()`, which returns a `*keel.Query[keel.Row]` to read with `keel.Into`. A query over the CTE
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
    w.Pick(w.ID, keel.Raw("0")).Where(w.ID.Eq("wrk_dispatch_lead")))
sub := w.As("sub")
chain.Recurse(sub.
    Pick(sub.ID, keel.Raw("? + 1", chain.Col("depth"))).
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

Aggregates and functions are `keel.Agg` values: `keel.Count()` (`count(*)`), `keel.Sum(expr)`,
`keel.RowNumber()`, and the column methods `Count`, `CountDistinct`, `Min`, `Max`, `Sum`, `Avg`,
`Len` and `Rank`. An `Agg` compares to values (`Eq`, `Ne`, `Gt`, `Gte`, `Lt`, `Lte`), orders (`Asc`,
`Desc`), restricts its rows with `Filter(pred)` (`FILTER (WHERE …)`), and becomes a window function
with `Over(partition, order...)`. Use a pointer field for an aggregate that can be NULL (`max` over
no rows).

### Window functions

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

`Over(nil, order...)` windows over every row, as a running count does: `keel.Count().Over(nil,
s.CreatedAt.Asc())`.

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
n, err := s.Where(s.SearchVector.Matches("reefer")).Timeout(2*time.Second).Count(ctx, db)
if errors.Is(err, context.DeadlineExceeded) {
    // the search took too long
}
```

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
- **Has-many and many-to-many** relations run one statement per relation per level, filtered by
  `= ANY($1)` over the parents' keys plus the scope's tenant columns. The statements of one level
  share a round trip, so `Load(s.Customer(), s.Moves().Stops())` costs three round trips: the
  shipments with their customers, the moves, the stops.
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

## Pagination

### Keyset pages

`Page(ctx, db, keel.After(cursor, size))` returns the page of `size` rows after `cursor`; an empty
cursor asks for the first page. `Total(limit)` also counts the matching rows, up to `limit`, in the
same round trip.

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
| `Total` | the number of matching rows when the request asked for it, capped at its limit |
| `TotalCapped` | `Total` stopped at the limit ("10,000+") |

Rules:

- The order may use columns of the query's table only. keel appends every primary key column the
  order does not already name, in the direction of the order's last term, so each row has a unique
  position. A query without an order pages by primary key.
- The cursor is opaque (base64url) and encodes the order values of the page's last row. Pass it only
  to the query that produced it; a cursor whose number of values does not match the order fails.
- `size` must be positive.

When every order term sorts the same way, none uses `NullsFirst` or `NullsLast` and no column is
nullable, keel compares the position as one row, `(created_at, id, …) < ($3, $4, …)`, which a
matching composite index serves. Otherwise it expands an `OR` chain that places NULLs as the order
does. See [Performance notes](#performance-notes) for the index.

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
inside `keel.Read(…, keel.RepeatableRead())` to read one snapshot, at the cost of holding a
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
| `keel.ExistsInto(q, &ok)` | `Exists` |
| `keel.IntoSlice[R](q, &rs)`, `keel.IntoValue[R](q, &r)` | `keel.Into`, `keel.IntoOne` |

How a batch runs:

- The main statements of every item (a page's count included) are sent together: in the
  transaction on the context; else, when `Config.OnBegin` is set, in an implicit transaction whose
  `BEGIN`, `OnBegin` statements and, when no relation loads follow, `COMMIT` ride in the same round
  trip; else on one pooled connection outside a transaction.
- The to-many relation loads of all items follow, one round trip per level for every item together.
- Each item keeps its terminal's checks: scope errors, interceptors, `AfterLoad` hooks,
  `ErrUnbounded` for a list without a `Limit`, `ErrNotFound` and `ErrNotSingular` for single-row
  items.
- `Batch` returns the first error and fills the destinations only when every item succeeded, so a
  failed batch leaves them untouched.
- The shortest `Timeout` among the items bounds the whole batch.
- An item holds no result, so it can be built once and run in more than one batch. `Batch` with no
  items does nothing.

The terminals are one-item batches, so a batch costs nothing beyond the statements it sends.

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
columns.

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
passed needs only its primary key and tenant columns; delete hooks that declared `Needs` get the
stored values of those columns in `Mutation.Old`.

### Bulk updates and deletes

`Update` and `Delete` on a query write every row it matches and return the number of rows. A query
without a non-nil `Where` predicate is refused; write `Where(keel.True())` to mean every row (of the
tenant). Relation path filters work: they become `FROM` in an update and `USING` in a delete.

```go
now := time.Now().Unix()
n, err := s.
    Where(s.Status.Eq(fleet.StatusNew), s.Customer().Code.Eq("ACME")).
    Update(ctx, db, s.Status.To(fleet.StatusCanceled), s.CanceledAt.To(now), s.Priority.Add(1))

st := fleetdb.Stops
n, err = st.Where(st.MoveID.Eq("mov_01"), st.ArrivedAt.IsNull()).Delete(ctx, db)
```

Assignments:

| Assignment | SQL |
|---|---|
| `col.To(v)` | `col = $n` |
| `col.Add(v)` (numeric columns) | `col = s.col + $n`, the stored value plus `v` |
| `col.ToNull()` | `col = NULL` |
| `col.ToDefault()` | `col = DEFAULT` |
| `col.ToExpr(e)` | `col = e`, for example `s.Weight.ToExpr(keel.Raw("? * 2", s.Weight))` |

The `autoupdate` column is stamped and the `version` column incremented unless an assignment names
them. Assignments must be columns of the query's table. Bulk writes run no model hooks and no
`BeforeWrite` hooks; see [which hooks run](#which-hooks-run).

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

- `keel.OnConflict(cols...)` names the unique columns; it is required.
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
        c.Version.Add(10),
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

`CreateOrSkip` inserts rows, skips those that conflict on the given columns (`ON CONFLICT DO
NOTHING`), and returns how many it inserted:

```go
n, err := c.CreateOrSkip(ctx, db, rows, c.OrganizationID, c.BusinessUnitID, c.Code)
```

### InsertFrom

`InsertFrom` inserts the rows of a query: `INSERT INTO t (cols) SELECT …`. The query must pick one
expression per listed column, in order. Under a tenant scope the tenant columns are filled from the
scope and must not be listed; under a system scope, list and pick them.

```go
tg, c := fleetdb.Tags, fleetdb.Customers
n, err := tg.InsertFrom(ctx, db,
    c.Pick(keel.Raw("'tag_' || ?", c.ID), c.Name).Where(c.Email.IsNotNull()),
    tg.ID, tg.Name)
```

`InsertFrom` does not fill `autoid`, `autocreate` or `autoupdate` columns and runs no hooks: pick
those values in the query.

### What is read back

After a write keel reads back only what Postgres may have changed: `readonly` columns, and the
version where it changed it. The values keel filled (IDs, tenant columns, timestamps) are already in
the struct.

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
}, keel.Serializable(), keel.Retry(3))
```

How transactions run:

- `BEGIN`, the transaction options and the `OnBegin` statements are sent with the first statement,
  in one round trip. A transaction that runs no statement costs nothing.
- The transaction holds one connection from its first statement to its end. Use its context from
  one goroutine at a time.
- A `Read` or `Write` inside a `Write` joins it; a `Read` inside a `Read` joins it; a `Write` inside
  a `Read`, and any write inside a `Read`, returns `ErrReadOnly`. Options of a joined call are
  ignored.
- `keel.InTx(ctx, db)` reports whether the context carries a transaction of `db`.

### Implicit transactions

Outside `Read` and `Write`, a terminal runs on a pooled connection. It runs in an implicit
transaction when `Config.OnBegin` is set, when the table has `AfterWrite` hooks for the write, or
for `CustomPlan`. A terminal with one statement then sends `BEGIN`, the `OnBegin` statements, the
statement and `COMMIT` in one round trip; with relation loads, `BEGIN` goes with the first batch and
`COMMIT` with the last.

### Options

| Option | Effect |
|---|---|
| `keel.LockTimeout(d)` | any statement of the transaction waits at most `d` for a lock (`set_config('lock_timeout', …, true)`, sent with `BEGIN`) |
| `keel.Serializable()` | `ISOLATION LEVEL SERIALIZABLE` |
| `keel.RepeatableRead()` | `ISOLATION LEVEL REPEATABLE READ` |
| `keel.Retry(n)` | reruns a top-level transaction up to `n` more times after a serialization failure or deadlock (`keel.IsRetryable`); `fn` must be safe to repeat |

```go
var open, late int64
err := keel.Read(ctx, db, func(ctx context.Context) error {
    var err error
    if open, err = s.Where(s.Status.Ne(fleet.StatusDelivered)).Count(ctx, db); err != nil {
        return err
    }
    late, err = s.Where(s.Status.Eq(fleet.StatusInTransit), s.Priority.Gte(2)).Count(ctx, db)
    return err
}, keel.RepeatableRead())
```

`Retry` starts each attempt as a new transaction; `AfterCommit` functions of failed attempts are
dropped. Nested calls never retry on their own.

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

keel.OnCreate(db, s, keel.BeforeWrite, func(ctx context.Context, m *keel.Mutation[fleet.Shipment]) error {
    if m.Row.ProNumber == "" {
        return errors.New("shipment needs a PRO number")
    }
    if m.Row.Attributes == nil {
        m.Row.Attributes = map[string]any{}
    }
    return nil
})

keel.OnUpdate(db, s, keel.AfterWrite, func(ctx context.Context, m *keel.Mutation[fleet.Shipment]) error {
    if m.Old == nil || !m.Changed(s.Status) || m.Old.Status == m.Row.Status {
        return nil
    }
    log.Printf("shipment %s: %s -> %s", m.Row.ID, m.Old.Status, m.Row.Status)
    return nil
}, keel.Needs(s.Status))

keel.OnWrite(db, fleetdb.Customers, keel.Committed, func(ctx context.Context, m *keel.Mutation[fleet.Customer]) error {
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
| `InsertFrom` | none | none | none |

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
| `Start`, `Duration` | when the round trip started and how long it took; statements sent in one round trip share both |
| `Rows` | rows affected or returned |
| `Err` | the error of the round trip |

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

## Errors

| Error | Returned by |
|---|---|
| `keel.ErrNotFound` | `Get`, `One`, `First`, `IntoOne` with no row; `DeleteRow` that deleted nothing; `Save` without a version column that matched nothing |
| `keel.ErrNotSingular` | `One` and `IntoOne` with more than one row |
| `keel.ErrConflict` | `Save` that found another version; the error is a `*keel.ConflictError` with `Table` and `Version` |
| `keel.ErrNoScope`, `ErrAllTenantsRequired`, `ErrSystemScopeRequired`, `ErrTenantMismatch` | scope violations; see [Scope errors](#scope-errors) |
| `keel.ErrUnbounded` | `List` without `Limit` |
| `keel.ErrReadOnly` | a write, a `Write` or a `Savepoint` inside a `Read` |
| `migrate.ErrChanged` | `Up` and `Plan` when an applied migration's file changed |
| `migrate.ErrOutOfOrder` | `Up` and `Plan` when a pending migration is older than the newest applied one |
| `migrate.ErrBaselineGap` | `Up` and `Plan` when the database's history ends before a baseline |
| `migrate.ErrBehind` | `Require` and `RequireWait` when the database has not applied a version; the error is a `*migrate.BehindError` with `Want` and `Have` |

Test them with `errors.Is` (and `errors.As` for `*keel.ConflictError`): keel wraps them with the
table name. Misuse (an `Update` without `Where`, an `Into` whose struct does not match the picks) is
reported with plain errors.

Postgres errors pass through as `*pgconn.PgError`. Helpers:

| Helper | Reports |
|---|---|
| `keel.IsUnique(err, constraints...)` | a unique violation (`23505`), optionally on one of the named constraints |
| `keel.IsForeignKey(err, constraints...)` | a foreign key violation (`23503`) |
| `keel.IsCheck(err, constraints...)` | a check violation (`23514`) |
| `keel.IsExclusion(err, constraints...)` | an exclusion violation (`23P01`) |
| `keel.IsNotNull(err)` | a not-null violation (`23502`) |
| `keel.IsRetryable(err)` | a serialization failure (`40001`) or deadlock (`40P01`) |
| `keel.Code(err)` | the SQLSTATE, or `""` |
| `keel.Constraint(err)` | the constraint name, or `""` |

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
generate them from models. `keel/migrate` applies them; `keel migrate` is its command line.
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
- A pending migration older than the newest applied one is `ErrOutOfOrder`, unless
  `Options.AllowOutOfOrder`: that is the branch merged with an old timestamp.
- keel stores each migration's checksum. An applied file that changed is `ErrChanged` before
  anything runs: add a new migration instead. After a comment-only edit, `migrate repair <version>`
  accepts the new checksum. Repeatable migrations re-run instead.
- `keel_migrations` holds `version`, `name`, `source`, `kind` (versioned, repeatable, go,
  baseline), `checksum`, `applied_at` and `duration_ms`, in `public` or `Options.Schema`.

### Command line

```sh
export KEEL_DSN=postgres://localhost:5432/app
keel migrate create add_shipment_holds            # migrations/20261023143000_add_shipment_holds.sql
keel migrate create shipment_status_idx -notx     # starts with --keel:notx
keel migrate create shipment_views -repeatable    # migrations/R_shipment_views.sql
keel migrate plan                                 # what up would do
keel migrate up                                   # apply; one line per event
keel migrate up -to 20261023143000 -dry-run       # print the SQL up to a version
keel migrate status                               # applied, pending, changed, repeatable-due, missing
keel migrate history                              # applied migrations, oldest first
keel migrate check                                # exit 1 on out-of-order, changed or missing migrations
keel migrate check -base-dir ../main/migrations   # the same against the base branch, without a database
keel migrate lint                                 # lint pending migrations; -all for every one
keel migrate baseline 20261023143000              # adopt an existing database
keel migrate repair 20261023143000                # accept an applied file's new checksum
keel migrate squash -dump "docker exec pg pg_dump -U app -d {db}"
keel migrate stamp -package schema -out schema/version_gen.go [-check]
keel migrate templates [prune]
```

Every command takes `-dsn` (default `$KEEL_DSN`), `-dir [name=]path` (repeatable; default
`migrations`), `-schema`, `-lock-timeout` (`0` for none), `-retries` (`0` for none),
`-allow-out-of-order`, `-after SQL` (repeatable) and `-json`. With `-json`, `up` writes one JSON
object per event and the other commands one JSON document. `check`, `lint` (on an error finding)
and `stamp -check` exit 1 when they find problems.

The same commands are importable from `keel/migrate/migratecli`, as functions that take an
`io.Writer`, for an application's own database binary.

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
which report query held the table. `migratecli.Printer(w, json)` renders events as lines or JSON
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
  `-admin` names the server for scratch databases (default `-dsn`); `-to` squashes up to a version.
- From Go: `migrate.Squash(ctx, migrate.SquashOptions{Admin, Dirs, To, Dump, Options})`.

### The server's view

The server links one generated file instead of its migrations:

```sh
keel migrate stamp -package schema -out schema/version_gen.go   # const Version, const Fingerprint
keel migrate stamp -out schema/version_gen.go -check            # in CI: fail when stale
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
the others (`keel migrate templates prune` keeps the current one), and
`migrate.ResetFromTemplate(ctx, admin, dbname, template)` recreates a database from a template.

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

- A read is one round trip, plus one per level of to-many loads. A page and its total share one.
- Independent reads of one request share a round trip with [`keel.Batch`](#batching-reads), and
  their relation loads share one per level.
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
  `m.Order(m.Sequence.Asc())`. Many-to-many loads filter the join table on its local key.
- `CountUpTo` and `Total(limit)` stop counting at the limit; prefer them to an exact `Count` on
  large tables.

### Decoding

Rows decode straight from pgx's buffers into struct fields through generated accessors, with no
reflection. Rows of one statement are allocated in slabs, scan plans are cached per connection and
statement, and enum values and tenant strings are shared instead of allocated per row.

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

### Testing an application

The same pattern works for an application:

- In `TestMain` or a `sync.Once`, get a database per test binary from `migrate.Template` (see
  [Templates](#templates)), with `Prepare` loading reference data, and open the application's
  `keel.DB` with its real `Config`. Call the returned drop function when the run ends.
- Give each test its own tenant context. Tenancy keeps tests apart, so they can run with
  `t.Parallel()` against one database.
- Test system jobs with a system scope, and assert scope errors with `errors.Is`.
- Assert SQL with `ToSQL` on a `DB` that never connects, and assert round trips with a recording
  observer.
- To exercise `Retry`, return `&pgconn.PgError{Code: "40001"}` from the transaction function.

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
| `bun:",nullzero"` | a pointer field; keel writes the value in the struct |
| `bun:",scanonly"` (generated column) | `keel:"stage_rank,readonly"`, plus `noselect` when it should not be read |
| `bun:",default:..."` | nothing; set the value in Go |
| `bun:"-"` | `keel:"-"` |
| `bun:",soft_delete"` | not built in; an interceptor that adds `deleted_at IS NULL` |
| `bun:"rel:belongs-to,join:customer_id=id"` | `keel:"belongs:customer_id"` |
| `bun:"rel:has-one,join:id=worker_id"` | `keel:"one:worker_id"` |
| `bun:"rel:has-many,join:id=shipment_id"` | `keel:"many:shipment_id"` |
| `bun:"m2m:shipment_tags,join:Shipment=Tag"` | `keel:"m2m:shipment_tags.shipment_id.tag_id"` |
| a hand-written `LEFT JOIN LATERAL (… ORDER BY … LIMIT 1)` for a shipment's first stop | `keel:"pick:moves.stops,first=moves.sequence\|stops.sequence"` and `Load(s.OriginStop())` |
| labels, descriptions and enum values curated in a report catalog | derived by `keel gen` from names, doc comments and constants ([Derived metadata](#derived-metadata)) |
| `db.NewSelect().Model(&sh).WherePK().Scan(ctx)` | `s.Get(ctx, db, id)` |
| `db.NewSelect().Model(&rows).Where("sp.status = ?", st).Scan(ctx)` | `s.Where(s.Status.Eq(st)).Limit(n).List(ctx, db)` |
| `WhereGroup(" AND ", func(q) {... ScopeTenant ...})` | nothing; the scope applies the tenant |
| `.WhereOr(...)` | `keel.Or(...)` |
| `.Where("sp.pro_number ILIKE ?", "%"+q+"%")` | `s.ProNumber.ContainsFold(q)` (escaped) |
| `bun.In(ids)` | `col.In(ids...)` |
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
| `.Union(q)`, `.UnionAll(q)` | `keel.Union(q1, q2)`, `keel.UnionAll(q1, q2)` |
| `.For("UPDATE NOWAIT")` | `.Lock(keel.ForUpdate, keel.NoWait)` |
| `db.NewInsert().Model(sh).Exec(ctx)` | `s.Create(ctx, db, sh)` |
| `db.NewInsert().Model(&rows).Exec(ctx)` | `s.CreateMany(ctx, db, rows)` |
| `.Returning("*")` | nothing; keel reads back `readonly` columns and the version |
| `db.NewUpdate().Model(sh).WherePK().Exec(ctx)` | `s.Save(ctx, db, sh)` |
| `db.NewUpdate().Model(sh).Column("status").WherePK()` | `s.Save(ctx, db, sh, s.Status)` |
| `db.NewUpdate().Model((*T)(nil)).Set("status = ?", v).Where(...)` | `s.Where(...).Update(ctx, db, s.Status.To(v))` |
| `db.NewDelete().Model(sh).WherePK().Exec(ctx)` | `s.DeleteRow(ctx, db, sh)` |
| `db.NewDelete().Model((*T)(nil)).Where(...)` | `s.Where(...).Delete(ctx, db)` |
| `.On("CONFLICT (...) DO UPDATE").Set("x = EXCLUDED.x")` | `s.Upsert(ctx, db, rows, keel.OnConflict(...).Update(s.X))` |
| `.Set("hits = sp.hits + 1, name = EXCLUDED.name")` on a conflict | `keel.OnConflict(...).Set(s.Hits.Add(1), s.Name.ToExpr(s.Name.Excluded()))` |
| pgx `CopyFrom` on the underlying pool for large loads | `s.Copy(ctx, db, rows)`, with tenant columns, IDs, timestamps and hooks |
| `.On("CONFLICT DO NOTHING")` | `s.CreateOrSkip(ctx, db, rows, cols...)` |
| `INSERT … SELECT` through `NewInsert().Table(...)` | `s.InsertFrom(ctx, db, q, cols...)` |
| `db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {...})` | `keel.Write(ctx, db, func(ctx context.Context) error {...})`; pass `ctx`, not a `tx` |
| `bun.IDB` parameters to share a transaction | nothing; the transaction travels on the context |
| `NewRaw` / string fragments | `keel.Raw("…?…", col, value)`; `db.Pool()` for what keel cannot express (unscoped) |
| `bun.Ident`, `bun.Safe` | an expression argument of `keel.Raw` |
| `sql.ErrNoRows` | `keel.ErrNotFound` |
| `pgdriver.Error` with `IntegrityViolation()` | `keel.IsUnique`, `IsForeignKey`, `IsCheck`, `Constraint` |
| `bun.QueryHook` | `keel.Observer` |
