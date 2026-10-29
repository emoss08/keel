# keel design

keel is a typed, Postgres-only data layer for Go. It is built for one large application
(Trenova: hundreds of thousands of users across thousands of tenants) and optimised in this
order: correctness and tenant isolation, runtime performance, developer experience, build time.

This document is the contract. Code that disagrees with it is a bug in one of the two; fix the one
that is wrong in the same change. [guide.md](guide.md) shows how to use keel;
[benchmarks.md](benchmarks.md) records how it performs.

## Goals

- **Typed end to end.** Columns, relations, values, assignments and results are checked by the
  compiler. Raw SQL exists, is explicit (`keel.Raw`) and is greppable.
- **Little server work per statement.** Values are bind parameters, so a statement shape has one
  text; it is prepared once per connection and Postgres skips parsing and, usually, planning.
- **One round trip where Postgres allows it.** Transactions start with their first statement, a
  lone statement runs as one pipelined `BEGIN; …; COMMIT`, independent statements of one read
  share a round trip, and a read's relation tree loads in the same round trip as the read.
- **No reflection on the hot path.** `keel gen` writes field accessors for every model; rows decode
  straight from pgx's buffers into struct fields through pgx scan plans cached per connection and
  statement.
- **Tenancy by construction.** The tenant comes from the context and is applied to every statement
  on a tenant table: reads, joins, relation loads, subqueries, updates, deletes and inserts.
- **Bounded by default.** `List` needs a limit, keyset pagination is built in, and bulk inserts
  send one array per column through `unnest`, in chunks that share one statement text and one round
  trip.
- **Cheap to compile.** Generated code is static data plus small non-generic functions. Generic
  types appear only where a typed result needs them (`Query[T]`, `Handle[T]`) and are reachable only
  from data-access packages.

Non-goals: other databases, schema migrations generated from models (migrations are SQL files),
soft delete, an identity map or a session cache.

## Layers

| Package | Role |
|---|---|
| `keel` | the public API: `DB`, transactions, scope, `Handle[T]`, `Query[T]`, expressions, columns, writes, hooks, errors, dynamic queries (`DynQuery`, `DynResult`) |
| `keel/schema` | runtime table metadata, written as static literals by the generator: storage facts, labels, docs, enum values, report facts; the `keel` tag parser |
| `keel/gen`, `cmd/keel` | the generator: loads packages with `go/packages`, finds models, writes one `<pkg>db` package per model package |
| `keel/migrate` | the SQL-file migration runner |
| `bench` | a separate module comparing keel with Bun and raw pgx |

The wire layer is pgx v5 (`pgxpool`, `pgx.Batch`, pgx scan plans). keel never uses `database/sql`.

## Models

A model is a plain struct that embeds `keel.Table`, an empty marker whose `keel` tag names the
table. Every stored field carries a `keel` tag. keel owns the tag key, so it validates every option:
an unknown option or a malformed value fails `keel gen` with the field's position. Tags under other
keys are not validated; `keel gen` reads only the `json` tag's name, into the schema (Derived
metadata).

```go
// Shipment is a customer's freight moving from origin to destination.
type Shipment struct {
    keel.Table `keel:"shipments,report=Operations"`

    ID             pulid.ID `keel:"id,pk,autoid:shp_"`
    OrganizationID pulid.ID `keel:"organization_id,pk,tenant"`
    BusinessUnitID pulid.ID `keel:"business_unit_id,pk,tenant"`
    CustomerID     pulid.ID `keel:"customer_id"`
    // ProNumber is the carrier's progressive number.
    ProNumber    string       `keel:"pro_number"`
    Status       Status       `keel:"status"`
    TotalCharge  money.Amount `keel:"total_charge_amount"`
    SearchVector string       `keel:"search_vector,noselect,readonly,tsvector"`
    Version      int64        `keel:"version,version"`
    CreatedAt    int64        `keel:"created_at,autocreate"`
    UpdatedAt    int64        `keel:"updated_at,autoupdate"`

    Customer   *customer.Customer `keel:"belongs:customer_id"`
    Moves      []*ShipmentMove    `keel:"many:shipment_id"`
    OriginStop *Stop              `keel:"pick:moves.stops,first=moves.sequence|stops.sequence"`
}
```

Table aliases are keel's business: each table in a statement gets a short alias derived from its
name (`shipments` → `s`, `shipment_moves` → `sm`) or, on a relation path, from the relation's name
(`BillTo` → `bt`), numbered when a statement uses it twice (`s2`). Aliases depend only on the
statement's shape, so equal shapes render equal text. A handle's `As("name")` gives a second
reference to a table, under that alias, for self-joins.

### Column options

| Option | Meaning |
|---|---|
| `pk` | part of the primary key |
| `tenant` | a tenant column: filled from the scope on insert, compared with the scope on every statement, added to every relation join |
| `autoid:<prefix>` | on insert, an empty value is replaced by `Config.NewID(prefix)`; the field must be a string type |
| `autocreate` / `autoupdate` | stamped on insert / on insert and every update, bulk updates included; `int64` means Unix seconds, `time.Time` a timestamp |
| `version` | optimistic locking: `Save` compares and increments it, bulk updates and upserts increment it; must be `int64` |
| `readonly` | never written (generated or trigger-maintained columns); read back after writes |
| `noselect` | not selected by default (a `tsvector`); usable in predicates. A generated `noselect` column must also be tagged `readonly` |
| `jsonb` | the value is encoded as JSON; a nil map or slice in a non-pointer field is written as `{}` or `[]` |
| `jsonnumber` | with `jsonb` only (`schema.FlagJSONNumber`): reads decode the JSON with `JSONCodec.UnmarshalNumber` (default `sonic.ConfigStd` with `UseNumber`), so numbers in interface values are `json.Number` |
| `tsvector` / `tsvector=<config>` | the column gets the full-text operators; `<config>` (`Column.TSConfig`) is the text search configuration its searches use by default |
| `nullzero` | a non-pointer field's zero value is stored as NULL and NULL reads back as the zero value (see Nullzero columns) |
| `computed` | not a column of the table: a field queries fill with `Compute(field, expr)` (see Computed fields) |
| `label=` | the column's label, instead of the one derived from the field name |
| `noreport`, `report`, `dimension`, `measure`, `agg=`, `format=`, `type=epoch\|date\|decimal` | report facts that differ from the derived ones (see Derived metadata); only on models tagged `report` |

The table tag takes `report[=Category]`, `label=` and `plural=`. Options that exclude each other
(`readonly` and the write options, `jsonb` and `tsvector`, `report` and `noreport`, `dimension` and
`measure`) are refused, and so is `jsonnumber` without `jsonb`.

A `jsonnumber` column's scan plan (`jsonNumberPlan`) calls the number-preserving decoder on the
column's bytes (after the jsonb version byte in the binary format) and stores the zero value for
NULL. The pool's `AfterConnect` puts `Config.JSON.UnmarshalNumber` in the connection's
`CustomData`, where the plan finds it when it is built, once per connection and statement like
every scan plan; a connection keel did not set up (`JoinTx`) uses the default.

A string column with enum constants (`Column.Enum`) gets an enum scan plan (`enumScanPlan` for a
`*string` field, `enumPtrScanPlan` for `**string`; generated accessors point a named string field at
its string) when its values arrive as their own text: a text type, the text format, or a Postgres
enum (`EnumCodec`), whose binary form is the label. The plan compares the raw bytes with each
constant (no allocation) and stores the generated constant's string, so every row shares it;
NULL and any other value go to the plan the column would have had (nullzero, nullable or pgx's).

There is no `default:` or `notnull`, and `type=` names a report value type, not a SQL type. The SQL
migrations own the schema; nullability follows the Go type (a pointer, or a `sql.Scanner` named
`Null…`). The value in the struct is the value written: keel never substitutes `DEFAULT` for a zero
value. A nil slice in a non-pointer array field is written as an empty array.

Column kinds follow the Go type: strings, `int64`, `int32`, `int16`, `float64`, `float32` (a
`Float64Col`), `bool`, `[]byte`, `time.Time`, `[]string`, `[]int64`, named types with those
underlying types (enums, IDs), and any other type implementing `sql.Scanner` (a decimal). A named
type over a basic type is that basic kind even when it implements `sql.Scanner` and
`driver.Valuer` (`pulid.ID`): keel accesses the underlying value directly and never calls those
methods. `int` is the `int64` kind, read and written through `*int64`; the generated package
carries a constant that fails the build where `int` is narrower than 64 bits. A custom type whose `driver.Valuer` has
a pointer receiver is bound through a pointer by the generated `Value` accessor and wrappers. Bulk
inserts send custom columns as text cast to the column's type, except an extension base type
(OID past `FirstNormalObjectId`) whose codec and array codec the connections register: its column
is one array of values the codec encodes (`columnType.array`, read with the catalog types).

### Nullzero columns

`nullzero` (`schema.FlagNullZero`) makes NULL the stored form of a non-pointer field's zero value.

- The generated `Value` accessor returns nil for the zero value, tested without reflection (a
  literal for basic kinds, nil for maps and slices, `IsZero()` when the type has it, the composite
  zero value of a comparable struct otherwise). Every path that binds through `Value` (VALUES
  inserts, `Copy`, `Save`, upserts) stores NULL. Bulk inserts build the column's array from
  pointers into the rows' fields, nil for zero values, using the nullable array types. `To(zero)`
  binds NULL in bulk updates and `Conflict.Set`. `InsertFrom` reads its source through
  `FROM (src) AS s(c1, …)` and writes `nullif(s.cN, zero)` for string, numeric, bool and time
  columns.
- Reads wrap the column's pgx scan plan in a plan that stores the zero value for NULL (typed per
  basic kind, so no reflection or allocation; `reflect.Value.SetZero` for other types); it is
  cached per connection and statement like every scan plan.
- `Column.MayBeNull()` is true for nullable and nullzero columns; keyset pages, the dynamic layer
  and rollups use it where they need to know whether a column can hold NULL.
- A value compared with a nullzero column means the field's value: `= zero` is `IS NULL`, `<> zero`
  `IS NOT NULL`, `<> v` `IS DISTINCT FROM v`, `= ANY(list)` with zero in the list adds
  `OR col IS NULL`, `<> ALL(list)` without it adds `OR col IS NULL`.
- Relation keys read from a nullzero column skip the zero value.

### Relations

| Tag | Meaning |
|---|---|
| `belongs:<fk>` | this table's `<fk>` references the target's primary key |
| `one:<fk>` | the target's `<fk>` references this table's primary key, at most one row |
| `many:<fk>` | the target's `<fk>` references this table's primary key |
| `belongs:<fk>\|<fk>[=<key>]…` (and `one:`, `many:`) | a key of several columns; each item is a foreign-key column and, after `=`, the referenced column (default: the primary key for the first, the same name for the others) |
| `m2m:<join_table>.<local_fk>.<remote_fk>` | rows linked through a join table |
| `pick:<rel>.<rel>…,first=<seg>.<col>\|…` (or `last=`) | one row along a path that crosses a to-many relation, the first (last) in a total order |

Relation tags take `label=` and `noreport`; `first=` and `last=` only on `pick`.

`Relation.Local` and `Relation.Remote` hold one column per key pair, and every renderer walks all
pairs: join conditions (`relCond`, `relCondNoTenant`), relation loads (`= ANY($1)` for one column,
`(cols) IN (SELECT * FROM unnest($1, …))` for several, unnested keys for `Limit` loads and picks,
keyed statements returning one key column per pair), picks, dynamic paths and pre-aggregation keys.
A key of several columns indexes parents by its values joined with NUL (`joinKey`), which text
cannot hold; single-column keys stay a string or an integer, without allocation. With one column
and no `=`, a relation references the primary key column besides the tenant columns, or the one
named `id` when there are several; the keys of to-many and pick relations must be string or integer
columns; many-to-many relations take one column per side; a tenant column both sides carry is
refused in a key. When both sides have tenant columns,
keel adds them to the join: as constants from the scope under a tenant scope, as column equalities
under a system scope. A many-to-many join table between two tenant tables carries the tenant
columns they share, and keel joins it to the target on them too, so a join table row links rows of
its own tenant. `schema.SharedTenant(parent, child)` names those columns, and
`Table.JoinTableOf(rel)` describes the join table as a tenant table (its two keys and the shared
tenant columns, flagged `tenant`) for code that reads it directly: the analytics fan-out reads it in a
CTE under the scope's tenant predicate, and `keel.Verify` checks its columns.

### Pick relations

A pick is a to-one relation whose row is the first (or last) row of a to-many path in an order:
`schema.Relation{Kind: schema.Pick, Path, Order, Last}`, where `Path` holds one relation index per
hop and `Order` one `schema.PickKey{Hop, Column}` per key. `keel gen` checks that the path walks
real relations, crosses a to-many relation, ends at the field's model and does not walk through
another pick, and makes the order total by appending the target's primary key; the parent key the
path starts from must be a string or integer column. At run time the pick is a subquery that joins
the path's tables hop by hop, each with its tenant predicate (constants under a tenant scope,
equalities with the previous hop under a system scope) and its table's interceptors, ordered by the
keys and limited to one row. It is a `LEFT JOIN LATERAL` when the statement filters through it or
reads at most two rows without an order, a keyed `unnest(keys) CROSS JOIN LATERAL` statement per
level otherwise, and the `FROM` item of `EXISTS` for `Any` and `None`. Bulk updates and deletes
cannot join it.

### Derived metadata

`keel gen` derives, and writes into the static schema, everything about a model that the source
already says, so nothing is declared twice:

- `Table.Label`, `Table.Plural`, `Column.Label`, `Relation.Label`: from the names, split at case
  changes and digits, with an acronym list (`-acronyms`, default ID, URL, API, JSON, UUID).
- `Doc` on tables, columns, relations and enum values: from doc comments.
- `Column.JSON`, `Relation.JSON`: the name encoding/json gives the field (the `json` tag's name, the
  field name for a tag that names none, empty without a tag or for `-`), so an API resolves its
  field names to columns and relations from the static schema, without reflection.
- `Column.Enum []schema.EnumValue{Value, Label, Doc}`: the constants of a column's named string or
  integer type, from any package, in declaration order.
- `Table.Report` and `Column.Report *schema.ReportInfo{Included, Kind, Type, Aggs, Format}` for
  models tagged `report`: inclusion, value type (epochs by the `_at`, `_date`, `_time` suffix on
  `int64`), measure or dimension, aggregations and format (`KeelFormat() string` on the type),
  resolved by the generator so readers only read them.

Shared values (enum value arrays, each distinct `ReportInfo`) are package-level variables of the
generated file, referenced by address or slice, so the schema remains static data.

### Model hooks

Methods on the model with standard-library signatures. The generator detects them and writes direct
calls; there is no runtime type assertion.

```go
func (s *Shipment) BeforeCreate(ctx context.Context) error
func (s *Shipment) BeforeUpdate(ctx context.Context) error
func (s *Shipment) BeforeSave(ctx context.Context) error   // create and update
func (s *Shipment) BeforeDelete(ctx context.Context) error // DeleteRow
func (s *Shipment) AfterLoad(ctx context.Context) error    // every loaded row, relations included
```

## Generated code

`keel gen ./internal/...` writes, for each package that holds models, a package named
`<name>db` (`-out` sets the path template). For each model it writes:

- the `schema.Table` as a static literal: columns, flags, relations, precomputed column sets, and
  accessor functions (`New`, `Ptr` for scanning, `Value` for binding, `Attach` for relations) plus
  hook trampolines;
- a handle type `ShipmentsTable` embedding `keel.Handle[*shipment.Shipment]`, with one field per
  column, one method per relation, `As`, and a typed `Get` by primary key when the key, besides its
  tenant columns, is one non-pointer column;
- a package variable `Shipments` built statically, `BuildShipments`, which relation and alias
  methods use, and `ShipmentsSchema`, which returns the table;
- a column type per named Go type (`StatusCol`), so `Status.In(...)` accepts only
  `shipment.Status`. Types declared outside model packages (`pulid.ID`) get their column type in a
  shared package (`-kinds`).

Relation targets are linked by a small generated `init` (pointer assignments only), because two
tables of one package that reference each other would be an initialisation cycle as static data.
Handles reached through relations (`s.Customer()`) are built on first use and memoised. The
memo keeps a relation path's (or an alias's) `Ref` apart from the handles built over it, which are
keyed by their Go type too: every handle type built for one path shares its `Ref`, so a column
resolved by name (`keel.ResolvePath`) and the generated column of the same path are one join.
`keel gen -check` fails when generated files are out of date.

Packages that take columns in declarations (rollups, for one) read a handle with
`keel.ColumnOf(e) (table, index, ok)`: the table and column index of a generated column of a
table's own handle or alias, and false for anything else, including a column reached through a
relation, which belongs to a related row. `db.TenantColumns()` returns `Config.TenantColumns`, the
order of the values in a tenant and in a tenant key, so such packages never ask for it again.

## Queries

A query starts from a handle and ends with a terminal method that takes `(ctx, db)`.

```go
s := shipmentdb.Shipments
m := s.Moves()

page, err := s.
    Where(
        s.Status.In(shipment.StatusNew, shipment.StatusAssigned),
        keel.When(search != "", s.Customer().Name.Contains(search)),
        m.Any(m.Status.Eq(shipment.MoveStatusAssigned)),
    ).
    Load(s.Customer(), m.Order(m.Sequence.Asc()), m.Stops().Location()).
    Order(s.CreatedAt.Desc()).
    Page(ctx, db, keel.After(cursor, 50).Total(10_000))
```

### Builder methods

`Where`, `Pick`, `Omit`, `Compute`, `Load`, `Order`, `Group`, `GroupingSets`, `Rollup`, `Cube`, `Having`, `Window`,
`Limit`, `Offset`, `Lock`, `Join`, `LeftJoin`, `JoinLateral`, `LeftJoinLateral`, `DistinctOn`,
`With`, `AllTenants`, `Timeout`, `CustomPlan`; `Any` and `None` turn a query into a predicate.
Builders are mutable and chainable; `Clone` copies one. `ToSQL` renders the main statement and
`Explain` returns its plan.

### Terminal methods

| Method | Result |
|---|---|
| `One` | exactly one row, `ErrNotFound` when none, `ErrNotSingular` when more |
| `First` | the first row by the query's order, `ErrNotFound` when none |
| `List` | rows; returns `ErrUnbounded` without `Limit` |
| `All` | every row; for sets known to be small |
| `Page` | a keyset page with an opaque cursor, a cursor per row (`CursorAt`) and an optional total in the same round trip: exact up to its limit (`TotalExact`), and above it the limit or, with `Estimate()`, the planner's estimate |
| `Count`, `CountUpTo`, `Exists` | scalars |
| `CountEstimate(ctx, db, exactUpTo)` | a `keel.RowCount{N, Exact}`: exact up to `exactUpTo` (`SELECT count(*) FROM (SELECT 1 … LIMIT exactUpTo+1)`), above it the top plan node's row estimate from `EXPLAIN (FORMAT JSON)` of the same rows, sent in the same round trip with the same text and bound arguments |
| `Each` | keyset batches at constant memory |
| `Update`, `Delete` | bulk writes over the query's filter; rows affected. A query without `Where` is refused |
| `UpdateReturning` | a bulk update that returns the updated rows, with their loads, from one statement |
| `keel.Into[R]`, `keel.IntoOne[R]` | projections: the picked expressions fill `R`'s exported fields in order |
| `keel.Batch(ctx, db, items...)` | independent reads in one round trip: `ListInto`, `AllInto`, `OneInto`, `FirstInto`, `PageInto`, `CountInto`, `CountUpToInto`, `CountEstimateInto`, `ExistsInto`, `IntoSlice`, `IntoValue` each pair a query with a typed destination and keep their terminal's checks; the relation loads of all items share that round trip where they pipeline, and one round trip per level otherwise; destinations are filled only when every item succeeded. The terminals above are one-item batches |
| `Explain(ctx, db, opts)` | the main statement's plan: total cost, estimated rows and EXPLAIN's JSON; with `Analyze`, actual rows and timings from `EXPLAIN (ANALYZE, BUFFERS)` run in a transaction (or savepoint) that is rolled back |

### Expressions

Every clause takes expressions, never strings. Columns are typed by their Go kind:

| Kind | Operators |
|---|---|
| every column | `IsNull`, `IsNotNull`, `EqCol`, `NeCol`, `InQuery`, `NotInQuery`, `Asc`, `Desc`, `ToNull`, `ToDefault`, `ToExpr`, `Excluded` (only in `Conflict.Set`), `Count`, `CountDistinct`, `Min`, `Max`, `Coalesce` |
| string, integer, scanner (`AnyCol`) | `Eq`, `Ne`, `Gt`, `Gte`, `Lt`, `Lte`, `Between`, `IsDistinctFrom`, `In`, `NotIn`, `To` |
| float | as above without `In` and `NotIn` |
| time | `Eq`, `Ne`, `Gt`, `Gte`, `Lt`, `Lte`, `Between`, `To` |
| numbers | `Increment`, `Sum`, and `Avg` except on `smallint`; the arithmetic of the typed operators |
| string | `Contains`, `HasPrefix`, `HasSuffix` (escaped), `ContainsFold`, `HasPrefixFold`, `Like`, `ILike`, `Match`, `MatchFold`, and the string functions below |
| bool | `Eq`, `IsTrue`, `IsFalse`, `To` |
| bytea | `Eq`, `To` |
| jsonb | `Contains`, `HasKey`, `HasAnyKey`, `HasAllKeys`, `Text(path...)`, `To` |
| array | `Has`, `Overlaps`, `ContainsAll`, `ContainedBy`, `To`, `Len`, `Unnest`, `Append`, `Remove` |
| tsvector | `Match(q, opts...)`, `Rank(q, opts...)`, `RankCD(q, opts...)`: the search string bound once, the tsquery built by `websearch_to_tsquery` (default), `plainto_tsquery`, `phraseto_tsquery` or `to_tsquery` over the string's words as lexemes or prefixes, for each configuration (`SearchConfig`, else the column's `TSConfig`, else the database default) and each mode (`SearchModes`), ORed |

Combinators: `keel.And`, `keel.Or`, `keel.Not`, `keel.When`, `keel.Exists`, `keel.NotExists`,
`keel.True`, `keel.Raw`, `keel.Val`. Aggregates: `keel.Count()`, `keel.CountOf`, `keel.Sum`,
`keel.Avg`, `keel.Min`, `keel.Max`, `keel.Grouping` (`GROUPING()` of `GroupingSets`, `Rollup` and
`Cube`), `.Filter(pred)`. Window functions: `RowNumber`, `Rank`, `DenseRank`, `PercentRank`,
`CumeDist`, `Ntile`, `Lag`, `Lead`, `FirstValue`, `LastValue`, `NthValue`, and every aggregate with
`.Over(partition, order...)` or `.OverWindow(w)`, where a `Window` has a partition, an order, a
`ROWS`, `RANGE` or `GROUPS` frame between bound offsets, an exclusion, or the name of a window the
query declares. Relation paths: belongs-to and has-one columns join automatically (`LEFT JOIN` in
reads, `FROM`/`USING` in bulk writes); `Any` and `None` on a to-many path compile to `EXISTS` and
`NOT EXISTS`.

Typed functions (`funcs.go`). Each value kind has an expression type, `struct{ e Expr }`, with the
comparisons of its columns: `StringExpr`, `Int64Expr`, `Int32Expr`, `Float64Expr`, `NumericExpr`,
`BoolExpr` (a `Pred`), `TimeExpr`, `JSONBExpr`. Each kind also has a value interface (`StringValue`,
`Int64Value`, …, `NumberValue` for any number) whose unexported marker method its column kinds and
its expression type implement, so generated wrappers satisfy it through embedding and a function's
arguments are checked by the compiler. A function whose result kind follows its receiver is a
method of the column kinds and expression types (`Lower`, `Trim`, `Length`, `Substr`, `SplitPart`,
`RegexpReplace`, `Abs`, `Round`, `RoundTo`, `Floor`, `Ceil`, `Ln`, `Greatest`, `Least`, `Coalesce`,
`NullIf`, `ToChar`, `AtTimeZone`, `TypeOf`, `Set`, `Unnest`, …); one whose kind is fixed is a package
function (`Concat`, `ConcatWS`, `Now`, `NowEpoch`, `Epoch`, `ToTimestamp`, `DatePart`, `DateTrunc`,
`BoolOr`, `BoolAnd`, `JSONBAgg`, `JSONBuildObject`, `JSONBuildArray`, `ArrayPosition`,
`WidthBucket`, `GenerateSeries`, `Int8Range`, `GenRandomUUID`, `Cast…`). Package-level
`Greatest(a, b)` with the result typed by its arguments would need a generic function inferring
its result from a method set, which Go cannot do, so kind-preserving functions are methods. Calls
render as `name(args)` (`sqlCall`); Go values are `*litExpr` binds (one placeholder per literal
wherever the expression repeats), cast (`$n::text`) where the parameter's type is "any" or
ambiguous; a date field is checked against Postgres's list and written as a constant. A string
column holding an enum or declaring a non-text `sqltype` is cast to text inside text functions.
`Text`, `Int64`, … bind a Go value as an expression of the kind; `StringOf`, … type any expression
without changing the SQL. `exprKind` reads the kind of every typed expression through an unexported
`kind()` method, so `Compute` checks them. `DeclareFunc0[R]` … `DeclareFunc3[R, A, B, C]` declare a
database function: a non-generic `*FuncDecl` (name, quoted name, arity, result kind) inside small
generic `FuncN` values whose `Call` converts the call to `R` (every expression type shares the
underlying struct). `FuncN` are user-level values, never reachable from `schema.Table`.

Typed operators (`operators.go`, `typedcase.go`, `typedrow.go`, `typedarray.go`, `literals.go`) give
the typed kinds what only `DynExpr` had, rendering the same SQL so a query moves from one to the
other without a plan change. Arithmetic (`Add`, `Sub`, `Mul`, `Div`, `Mod`, `Neg`) is a method of
each numeric column kind and expression type taking the kind's value interface and returning its
own kind, rendered `(a op b)` through `binOp`; kinds never mix implicitly (a `Cast…` crosses them),
so the Go type of a result is the Postgres type of its value. Times take an `IntervalValue`
(`IntervalExpr`: `Interval`, `Duration`, `Since`). Because `Int64Col.Add` is now the expression,
the assignment `col = col + v` is `Increment`. `Cat` is `||` (NULL-propagating, unlike `concat()`),
`Collate` quotes a validated name, `IsDistinctFromExpr`/`IsNotDistinctFromExpr` compare two values of
a kind, and a nullzero column's `IsNotDistinctFrom(zero)` is `IS NULL`. CASE has one exported
builder per result kind (`StringCase`, `Int64Case`, …) over one unexported non-generic `caseWhens`
core, so no exported signature mentions a generic type; a nil condition skips its `WHEN`. `Tuple(vs...)`
is `ROW(…)` with `Eq` … `Lte`, `IsDistinctFrom` and `InUnnest`, rendered as `DynRow`'s comparisons.
Typed arrays have a value interface and expression type per element kind, literals that bind one
`$n::type[]` parameter (a nil slice as the empty array), `ARRAY[…]` constructors (`ARRAY[]::type[]`
when empty), views, and `At`, `Len`, `Unnest`, `Without`, `@>`/`<@`/`&&` and `InArray` on the
element side. `Null…()` are `NULL::type` constants; `…Ptr(p)` bind the pointed-to value under the
kind's cast, or NULL. `Const…` write a value into the text through `constText` (DynConst's
rendering; negatives parenthesised; floats and numerics as quoted literals under their cast), for
statements that must match an expression index, a partial index predicate or a conflict target.
`CastInt64`, `CastFloat64` and `CastNumeric` take any expression, text included, beside the new
`CastInt32`, `CastBool`, `CastTime`, `CastInterval` and `CastJSONB`.

Values are bind parameters. `In` binds one array parameter (`= ANY($1)`), so the statement text does
not depend on the number of values. Two binds share a placeholder only when they provably get the
same parameter type, since Postgres infers it from the first use: the same value of the same
expression written twice (an expression in `Pick` and `Group` renders identically), equal
comparable values compared with the same column through the same operator, and equal values under
the same explicit cast. Writes never share. A column resolves to the innermost statement that references
its table; a column of a table's base handle also resolves inside a statement built on a relation
path to that table.

### Computed fields

A `computed` field (`schema.FlagComputed`, `Column.Stored()` false) stays in `Table.Columns`, so it
has a column index, a `Ptr` and a `Value` accessor, but `Derive` leaves it out of `Select`,
`Insert`, `Update` and `Returning`; the bulk-insert catalog lookup skips it, reports never include
it, and writes that name it are refused.

- `Compute(field, expr)` records `(column, expr)` on the query, after checking that the field is a
  computed field of the query's table, computed once, and that `expr`'s value kind, when keel
  knows it (columns, `StringExpr`, `Scalar` of one typed expression, aggregates and window
  functions by their function and argument), fits the field's kind.
- When a statement renders, each node's computes are registered on its render scope. A column
  reference to a computed field (`r.col`) writes `(expr)` from the innermost scope that computes
  it, so the field can be selected, filtered, ordered, paged by keyset and projected; with no
  compute in scope the statement fails. The select list appends computed fields after a node's
  columns, and the scan plan decodes them into the field like any column.
- `DynQuery.Compute(name, expr)` is the dynamic counterpart: `Path(name)` resolves the field, which
  renders the expression.
- `keel.Scalar(q)` is `(SELECT …)` as an expression, typically correlated, to fill a computed field
  with an aggregate over related rows.

### Keyset pages

`Page` turns the query's order into keyset terms (`pageKeys`) and appends the primary key columns it
does not name. A term on a column of the table reads the cursor value from the row (`Table.Value`,
the read's extra columns make sure it is selected). Any other term, a to-one relation's column (its
path is checked to cross no to-many relation, and the render joins it as every read does) or an
expression, is selected as a hidden value after the rows' columns (`selectOpts.tail`); the scan plan
decodes those values per row (`selectPlan.tailScan`) through pgx into cursor values, refusing types
a cursor cannot hold, and keeps them beside the rows. Such terms count as nullable. `CursorAt(i)`
encodes row `i`'s values on demand, so a page costs no encoding for rows nobody asks about, and
`Next` is the cursor of the last kept row. The keyset predicate is a row comparison when every term
sorts one way and none can be NULL, an `OR` chain that places NULLs as `NULLS FIRST`/`LAST` (or
Postgres's default) does otherwise, with expressions in parentheses. A dynamic page keeps its order
columns (hidden ones included) beside its result for `DynResult.CursorAt`.

## Relation loading

`Load` takes relation paths, optionally narrowed with `Where`, `Order`, `Pick` and `Limit`.
Belongs-to and has-one segments are joined into the parent's statement, and so are picks when the
statement reads at most two rows without an order; other picks load with the level's to-many
relations, one keyed lateral statement per pick. Has-many and many-to-many segments run one
statement per relation per level, filtered on the parent keys plus the scope's tenant columns.
Under a system scope parents of several tenants can share a key, so a load between two tenant
tables keys on (shared tenant columns, relation keys) in its statement, its key lists and the
grouping of children under parents (`crossTenant`); under a tenant scope the tenant columns are
constants and the keys alone match. A to-many load with a `Limit` loads at most that many rows per
parent through `… CROSS JOIN LATERAL (… LIMIT n)`; many-to-many loads take no `Limit`. On a to-one
load, `Where` becomes part of the join condition, so a row that does not match leaves the relation
nil without filtering the parent. `AfterLoad` hooks run once every level is loaded, deeper levels
first.

### Load strategies

`Config.LoadStrategy` (default `LoadPipelined`) and `Query.LoadStrategy` (on a query or a load,
for its subtree; the nearest choice wins) choose how levels reach the server.

- **`LoadPipelined`**: the read's statement and every level's statement go in one round trip. A
  level's filter is a subquery over its parent level's own statement rendered again in key mode
  (`selectOpts.keysOf`): the same CTEs, joins (the loaded to-one joins included, with their load
  `Where`), filters, interceptors, read filters and keyset, selecting only the parent node's key
  columns (after the shared tenant columns under a system scope, as `crossTenant` keys them), with
  its order, limit and offset kept when it is limited and dropped otherwise, and no lock or
  `DISTINCT`. Deeper levels nest the subqueries. A filter is `fk IN (subquery)` or
  `(tenant…, fk…) IN (subquery)`; a `Limit` load's and a pick's key list is
  `(SELECT DISTINCT * FROM (subquery) kq) AS k(k…)`, joined laterally as the unnested arrays are.
  Children are grouped under parents as before: a load's scanner indexes the parent level's rows,
  which the pipeline has already decoded, by key before it reads its own.
- **Snapshot.** The subqueries re-read the parent level, so pipelining runs only where every
  statement reads one snapshot (`runner.oneSnapshot`): in a transaction at `RepeatableRead` or
  `Serializable` (`txState.level`, including `Config.ReadIsolation`), or where keel owns the
  transaction: an implicit read-only transaction not yet begun is begun `REPEATABLE READ READ ONLY`,
  and a read outside any transaction opens one (`runner.holdSnapshot`), whose `BEGIN` and `COMMIT`
  ride in the round trip. A read in a `READ COMMITTED` transaction, a `Write` at the default level,
  and a `JoinTx` transaction (whose level keel cannot know) load per level. A replica guard failure
  re-runs the round trip on the primary with the same options.
- **Stable parents.** A level pipelines only when its parent level's statement selects the same
  rows every time it runs on that snapshot (`query.stableRows`, `stableOrder`). Without a limit or
  offset it does. With one it does when its order is non-empty: keel appends the primary key
  columns the order lacks (`selectOpts.tieBreak`; under a tenant scope the key's tenant columns are
  constants and are not appended) to the statement and to its key subquery; or when the read fails
  unless it returned every matching row (`One`, limited to two rows). It does not with a limit or
  offset and no order (`First` or `Limit` without `Order`), a lock (`SKIP LOCKED` skips rows a
  second read may not), `DISTINCT ON`, `GROUP BY`/`HAVING`, a limited `DISTINCT`, or a per-parent
  `Limit` load without an order. Such a level's children load per level after the round trip, and
  the children of that per-level statement pipeline again below it (its keys bound as arrays, its
  descendants filtered by subqueries over it), so each unstable boundary costs one round trip.
  Statements are assumed deterministic otherwise: a volatile function or an unordered limit inside
  a CTE or an explicit lateral join can make a subquery select other rows; such queries say
  `LoadPerLevel`.
- Many-to-many loads pipeline with the same key tuple they bind per level (target tenant columns,
  join table key), and picks and per-parent `Limit` loads take the distinct key subquery in place of
  `unnest`; all of them can parent further pipelined levels. `UpdateReturning` loads per level: its
  parent statement is the update.
- **`LoadPerLevel`**: one round trip per level, each filtered by `= ANY($1)` (or
  `(cols) IN (SELECT * FROM unnest($1, …))`) over the keys read back from the level before;
  statements of one level share a round trip.

## Dynamic queries

`keel.DynQuery` is the runtime-typed counterpart of `Query[T]` for code that chooses tables,
columns, relations and expressions at run time: report compilers, data tables with user-chosen
columns, exports. It starts from a `*schema.Table` (`DynFrom`) or a name in a `Registry` of
generated tables and is not generic.

- **One renderer.** A dynamic query holds the same `query` a typed one does and renders through
  `renderSelect` and the render scope. Tenancy, interceptors, aliases, parameter sharing and
  statement preparation are the typed ones; nothing is reimplemented.
- **Typed queries resolve names too.** `keel.ResolvePath(handle, segments...)` follows relation
  names from a typed handle through the memoised path `Ref`s (`pathRef`) and returns a `DynExpr`
  over that path's column, so typed queries filter and sort by names chosen at run time without a
  dynamic query's own references, which would resolve only through the same-table fallback.
- **Identifiers from the schema only.** Columns and relations are resolved by name against the
  schema and written from its quoted names. Result columns are `c1…cN` in SQL and keep their labels
  in Go; CTEs and pre-aggregations get generated names; `DynFunc` calls an allow-list; `Cast` takes
  a `ValueType`. A name that does not resolve makes an expression that carries an error.
- **CTE names per statement.** The CTEs keel names (`With`'s `keel_cte`, derived tables'
  `keel_sub`, set operations' `keel_set`) are named when a statement renders, across every query
  nested in it: the first use of a base keeps it, later ones are `base_2`, `base_3`, …, in the
  order the statement uses them. Nested statements never shadow each other's names, and the names
  depend only on the statement's shape, so equal shapes render equal text.
- **To-many relations are read as the caller chooses**: `Any`/`None` (`EXISTS`, nested per to-many
  hop), or `PreAggregate`: a grouped CTE keyed by the parent and filtered to the parents the query
  selects (`PreAggCTE`: one pass over the child table), or a `LEFT JOIN LATERAL` (`PreAggLateral`:
  one probe per parent). Keys include the tenant columns; a parent without rows reads NULL with
  both.
- **Columnar results.** `DynResult` holds one typed vector per column and a null bitmap, decoded
  from pgx's raw values by a decoder chosen once per column: no per-row map, no boxing.
  Variable-length values are slices of arenas that are never reused. numeric is `keel.Decimal`, its
  exact text, formatted by keel from the binary format. Types without a vector decode through
  their pgx codec into JSON.
- **Scalars.** `Count` is `SELECT count(*) FROM (query) c`; `Exists` is `SELECT EXISTS (query)`, one
  statement that stops at the first row. Both render the query through `writeSelect`, so scope,
  tenancy and interceptors are the query's.
- **Pages and streams.** `Page` wraps the query and applies the keyset to its output columns, adding
  hidden order columns and, on an ungrouped table query, the primary key. `Stream` decodes batches
  while the rows arrive and cancels the query when the consumer stops outside a transaction.

## Writes

```go
err := s.Create(ctx, db, sh)                       // autoid, timestamps, tenant columns filled
err = s.CreateMany(ctx, db, rows)                  // unnest of one array per column, 10,000 rows per statement, one round trip
err = s.Save(ctx, db, sh)                          // compare-and-increment version, ErrConflict on mismatch
err = s.Save(ctx, db, sh, s.Status, s.CanceledAt)  // only these columns (and the autoupdate column)
err = s.SaveExcept(ctx, db, sh, s.Token)           // every updatable column but these
err = s.DeleteRow(ctx, db, sh)                     // ErrNotFound when nothing was deleted
n, err := s.Where(...).Update(ctx, db, s.Status.To(shipment.StatusCanceled))
rows, err := s.Where(...).Load(s.Customer()).UpdateReturning(ctx, db, s.Status.To(shipment.StatusCanceled))
out := keel.Unnest("outcome", []string{"id", "status"}, ids, statuses) // a CTE over Go slices
n, err = s.With(out).Join(out, out.Col("id").EqCol(s.ID)).Where(...).Update(ctx, db, s.Status.ToExpr(out.Col("status")))
n, err = s.Where(...).Delete(ctx, db)
ins, err := s.Upsert(ctx, db, rows, keel.OnConflict(s.OrganizationID, s.BusinessUnitID, s.ProNumber).Update(s.Status))
ins, err = s.Upsert(ctx, db, rows, keel.OnConflict(...).Set(s.Status.ToExpr(s.Status.Excluded()), s.Priority.Increment(1)))
n, err = s.CreateOrSkip(ctx, db, rows, s.OrganizationID, s.BusinessUnitID, s.ProNumber) // no targets: any unique violation
n, err = s.InsertFrom(ctx, db, src.Pick(...), s.Name, s.Code)
n, err = s.Copy(ctx, db, rows)                     // binary COPY; prepared as Create, nothing read back
```

`UpdateReturning` renders the same `UPDATE` as `Update` (`writeUpdate`) inside a data-modifying
CTE, `WITH keel_upd AS (UPDATE … RETURNING alias.*[, old.c AS keel_old_N]) SELECT … FROM keel_upd
alias`, and reads it with the select renderer: the outer `SELECT` is the query without its filter,
joins and CTEs, over the CTE as a prefiltered `FROM` item, so its to-one loads join, its order
applies, its scan plan decodes the rows (the previous values of hooked columns as hidden values
after them) and its to-many loads follow in the write's transaction. The rows feed the update
hooks and the data versions (`touchRows`), so a system-scope write needs no `returnTenants`
wrapper.

After a write keel reads back only what Postgres may have changed: `readonly` columns, and the
version where it changed it. `Upsert` reports per row whether it inserted (`xmax = 0`); a row that
conflicted keeps the primary key it was sent with. `OnConflict` targets are columns or index
expressions (written in parentheses, without aliases, refusing binds). `Conflict.Update` takes columns from the proposed
row (`col = EXCLUDED.col`); `Conflict.Set` takes `Assignment`s, in which the table's columns read
the stored row and `Col.Excluded()` the proposed row, with values bound as parameters; a column
assigned twice is refused, and the `autoupdate` and `version` columns are maintained unless `Set`
assigns them. The `ON CONFLICT` clause is rendered with each statement, after its values. An
`OnConflict(...).Where` predicate must not bind values. One row is inserted with `VALUES`; more rows
through `INSERT … SELECT … FROM unnest(…)` with one typed array per column (column types read from
the catalog once per `DB`), 10,000 rows per statement, every statement sharing one text and one
round trip.

`Copy` streams rows through the binary `COPY` protocol (`pgx.CopyFrom`) for loads of many thousands
of rows. Rows are prepared as `Create` prepares them (autoid, tenant and timestamp columns, model and
`BeforeWrite` hooks, `ErrTenantMismatch`); `COPY` returns nothing, so `readonly` columns are not read
back and `AfterWrite` and `Committed` hooks see the rows as written. `COPY` cannot share a pipeline,
so an implicit transaction (for `OnBegin` or create `AfterWrite` hooks) is begun in a round trip of
its own first.

`Save` and `DeleteRow` apply interceptors: their predicates join the primary key in the `WHERE`
clause (relation paths become `FROM`/`USING` joins), so a row they exclude is not written and
reports `ErrConflict` (versioned `Save`) or `ErrNotFound`.

## Transactions and scope

```go
err := keel.Write(ctx, db, func(ctx context.Context) error { ... }, keel.LockTimeout(2*time.Second))
v, err := keel.ReadValue(ctx, db, func(ctx context.Context) (T, error) { ... })
```

- The transaction lives on the context. Terminals find it there; nested `Read`/`Write` calls join
  it; a `Write` inside a `Read` returns `ErrReadOnly`.
- A transaction keeps the scope it began with, since its `OnBegin` statements were made for that
  scope. Joining it under another scope (a statement, `Read`, `Write`, `Savepoint` or `LockKey` whose
  context carries a system scope inside a tenant's transaction, or another tenant) returns
  `ErrScopeMismatch`. keel does not start a new transaction on its own there: that would quietly
  split one unit of work into two commits and read past the outer transaction's writes. The caller
  says so with `keel.Detached()`.
- `keel.Detached()` begins a `Read` or `Write` on a pool connection of its own even inside another
  transaction, with the context's scope (and its `OnBegin` statements). Its context hides the outer
  transaction until it ends; it commits, runs its `AfterCommit` callbacks and records its data
  versions on its own. It composes with `JoinTx` and `Savepoint` like a top-level transaction.
- `BEGIN`, the transaction options and the statements from `Config.OnBegin` (for example a
  row-level security `set_config`) are sent with the first statement. A transaction that runs no
  statement costs nothing.
- Outside a transaction a terminal runs its statements in an implicit transaction when `OnBegin` is
  set, an `AfterWrite` hook needs one, or the query uses `CustomPlan`; a terminal with a single statement sends `BEGIN`, the `OnBegin`
  statements, the statement and `COMMIT` in one round trip.
- `Config.Tenant` returns the tenant values from the context, in the order of
  `Config.TenantColumns`. `Config.System` reports a system scope and its reason. A statement on a
  tenant table with neither is refused (`ErrNoScope`). Under a system scope a statement on a tenant
  table must say `AllTenants()` (`ErrAllTenantsRequired`), and `AllTenants()` without a system
  scope is refused (`ErrSystemScopeRequired`). A query that says `AllTenants()` covers its joined
  relations, its to-many loads at every level and its `Any`/`None` predicates; a query used as a
  subquery (`Exists`, `InQuery`, lateral joins, CTEs, set operations) declares its own. Inserts under a system scope must carry their tenant
  columns; a row of another tenant is refused (`ErrTenantMismatch`).
- `keel.AfterCommit(ctx, fn)` runs `fn` after the transaction on the context commits, or at once
  outside one.
- `keel.Retry(n)` retries serialization failures and deadlocks for a top-level transaction.
  `keel.Isolation` levels (`ReadCommitted`, `RepeatableRead`, `Serializable`) are `TxOption`s
  themselves (`TxOption` is an interface with an unexported method); `Config.ReadIsolation` is the
  level of an explicit `Read` without one (zero: the server's default; on a `ReplicaTx`,
  `Serializable` becomes `RepeatableRead`). Implicit transactions and `Write` keep the server's
  default unless keel or an option sets one. BEGIN texts are precomputed per level and access mode
  (`beginSQL`). `txState.level()` is the effective level and `snapshot()` reports `REPEATABLE READ`
  or above; a `JoinTx` transaction reports zero.
- `keel.Savepoint` runs a function inside a savepoint; an error rolls back to it and drops the
  `Committed` hooks and `AfterCommit` callbacks queued inside it. `keel.LockKey` and
  `keel.TryLockKey` take transaction-scoped advisory locks on string keys.
- Raw SQL joins the transaction on the context like any terminal: `Exec` (rows affected),
  `ExecBatch` (several statements in one round trip), `QueryRows` (a callback per row),
  `QueryAll[T]` (rows through a pgx row function) and `QueryValue[T]` (one value, `ErrNotFound`
  without a row). Outside a transaction they run on the primary, in an implicit transaction when
  `OnBegin` is set; inside a `Read` with `ReplicaTx` they run on its replica. Observers see them
  (`exec`, `query`); scope checks, interceptors, read filters, hooks and data versions do not apply.

### Ending read-only transactions

A read-only transaction keel began on a connection of its own pools ends without waiting for the
reply to its `COMMIT` (or `ROLLBACK`): `finish` writes it with the connection's `PgConn().Exec`
under a context of its own (`drainTimeout`, 30s), returns, and a goroutine counted in `DB.drains`
reads the reply and then releases the connection. pgxpool destroys a connection that is busy or in
a transaction on `Release`, so the release waits for the reply; a failed reply closes the
connection (`DB.ReadCommitFailures`), and a connection pgx already closed is released at once.
`Close` waits for the goroutines before closing the pools. The caller's context does not reach the
reply: a request context cancelled after `Read` returns would otherwise cancel the drain and
destroy the connection.

Writes wait, as do `SERIALIZABLE` reads (their `COMMIT` can fail a serialization check that `Retry`
must see), `JoinTx` transactions (keel does not own the connection) and every read under
`Config.AwaitReadCommit`. A `COMMIT` that rides in the pipeline of an implicit transaction's last
statements is read with them: Postgres flushes the statements' results only at the pipeline's
`Sync`, after `COMMIT`, so not reading it saves nothing and would leave the pipeline open under the
caller's context. `AfterCommit` callbacks run when the transaction returns.

### Deadlines and statement_timeout

With `Config.DeadlineTimeouts`, `queueBegin` adds `SELECT set_config('statement_timeout', $1,
true)` with the milliseconds left before a deadline (at least 1): the deadline of `Read`'s or
`Write`'s context for an explicit transaction (a statement's own shorter deadline would wrongly bind
the later statements), of the beginning round trip's context for an implicit one, which carries
`Query.Timeout`. The text is constant; the value is a parameter. Statements outside transactions
are bounded by pgx's cancel request only, since a transaction-local setting outside a transaction
does nothing and a session setting would outlive the statement on a pooled connection.

### Tenant concurrency

`Config.TenantConcurrency` (`tenantLimiter`, `tenantlimit.go`) bounds the runners and transactions
of one tenant key (`TenantKeyOf` of the scope's values) per `DB`. The limiter is 64 shards, each a
mutex and a map from key to a `tenantSlot` (holder count and a FIFO of one-element channels for
waiters); a releasing holder hands its slot to the first waiter, and a slot with no holder and no
waiter leaves the map, so the map's size is the number of tenants active now. A waiter whose context
ends removes itself, or, if it was handed the slot in that instant, passes it on.

- `runnerFor` takes a slot for a runner outside a transaction and `runner.close` gives it back after
  ending the implicit transaction and before its `AfterCommit` callbacks; `runTx` takes one per
  attempt of a top-level transaction and `txState.run` gives it back after ending it. Statements of
  a transaction take none.
- A held slot rides on the transaction's function context and on a write's hook context
  (`slotKey`); work started under a context carrying a slot of the same key shares it rather than
  wait (a `Detached` transaction, a hook's query), which would deadlock at a bound of one.
- System scopes and unscoped work are exempt; `DB.TenantWaits` counts acquisitions that waited.
- A runner's or transaction's part in a slot is a `slotHold` embedded in it: the slot, whether it
  owns it, and, with `Config.SlowWaits`, when it took the slot and its stack's program counters.
  The slot keeps its owners' holds in a map, so a waiter that crosses the threshold reports who
  holds the tenant's slots (`slowwait.go`). Connection acquisitions go through `DB.acquire`, which
  arms a timer per acquisition, and cache fill waits through `DB.waitFill`; frames are symbolised
  only when a report is made.
- `Config.MaxWait` bounds a slot wait, `DB.acquire` and `DB.waitFill` with a context ended by
  `context.WithTimeoutCause` and the cause `errWaitLimit` (`maxwait.go`); a wait whose cause is it
  returns `ErrWaitLimit`, one the caller's context ends first keeps that context's error. A
  connection or fill wait names its tenant through `waitTenant`, read only when the wait is
  reported: the runner's scope, the version read's tenant, or the scope of the context of a catalog
  read.
- A cached read that waits for another goroutine's fill gives its owned slot back
  (`runner.yieldSlot`) and takes one again (`runner.reclaimSlot`, which also points its implicit
  transaction at the new slot) before it looks again; a shared slot is kept.

### Budgets

`keel.Budget(ctx, opts...)` puts a `*budget` on the context (chained to an enclosing one). The
runner counts in `batch`, around `exchange` (the old batch body: `direct`, `send` and the replica
retry, which reports two round trips), so every terminal is covered without per-terminal code; `COPY`
and the `BEGIN` round trip of `txState.begin` count themselves. Before a round trip, `refuseBudgets`
fails it when a budget without `OnExceeded` is already over a limit; after it, `chargeBudgets` adds
the round trips, the statements and one count per distinct text of the round trip to every budget in
the chain, under each budget's mutex. The round trip that crosses a limit returns a `*BudgetError`
after its statements ran, or calls `OnExceeded` once. A call site is captured (`runtime.Callers`,
the first frame outside package keel and outside packages whose name ends in `db`) only for a text
already seen, at most once per round trip and 8 sites per text, so statements that never repeat cost
a map update. A cache hit returns before the budget code and counts nothing.

### Joining another data layer's transaction

`keel.JoinTx(ctx, db, conn) (context.Context, func(committed bool))` runs keel inside a transaction
another library began on a pgx connection, so an application can move to keel while it still writes
through, say, Bun on pgx's stdlib driver, with both in one transaction.

- The returned context carries a transaction state that owns nothing: it uses `conn`, is marked as
  begun, and has no replica or commit-position route. keel sends no `BEGIN`, `COMMIT`, `ROLLBACK`
  or `OnBegin` statements; the owner began the transaction and set its scope. `Read` and `Write`
  join it, `Savepoint` nests in it, and every terminal runs on `conn`, so scope checks, tenancy,
  interceptors, read filters, hooks and observers behave as in a transaction keel began.
- The state queues what keel queues for any transaction: `Committed` hooks, `AfterCommit`
  callbacks and the (tenant, table) pairs its writes changed. The owner calls the returned function
  after `COMMIT` (`true`: the pairs go to the versioner and the queued work runs) or `ROLLBACK`
  (`false`: it is dropped). Only the first call counts; afterwards a terminal on the context is
  refused rather than run outside the transaction.
- `seed.Options.Transact` lets the application run every seed transaction this way, so a seed's
  rows and its bookkeeping row commit or roll back with the application's own writes.

### Statement preparation

`Config.PlanCache` chooses how statements are prepared:

| Mode | Behaviour |
|---|---|
| `PlanCacheAuto` (default) | each statement shape is prepared once per connection (pgx's statement cache, `StatementCacheCapacity`, default 512); `plan_cache_mode = auto` lets Postgres reuse a generic plan when it is no worse than the custom plans of the first executions |
| `PlanCacheCustom` | prepared, but every execution is planned for its values (`force_custom_plan`) |
| `PlanCacheOff` | nothing prepared; for poolers that cannot track prepared statements (PgBouncer before 1.21) |

`Query.CustomPlan()` plans one query for its values: keel wraps it in transaction-local
`set_config('plan_cache_mode', …)` calls inside the same round trip.

## Replicas

```go
cfg.Replicas = []keel.ReplicaConfig{{DSN: replicaA}, {DSN: replicaB, Weight: 2}}
cfg.MaxReplicaLag = 2 * time.Second

ctx = keel.OnReplica(ctx)                           // reads on ctx go to a replica
err = keel.Read(ctx, db, report, keel.ReplicaTx())  // one replica snapshot for long read-only work
pos := keel.CommitPosition(ctx, db)                 // for keel.OnReplicaAfter in a later request
```

- Every replica pool gets the primary's setup through one function (`Config.configure`): query
  mode, statement cache, `plan_cache_mode`, the `AfterConnect` that registers enums, arrays, JSON
  and `LoadTypes`.
- Routing is opt-in per context. Read terminals on a marked context, outside a transaction, run on
  a replica; writes, `Exec` and explicit transactions use the primary, unless a `Read` asks for
  `ReplicaTx`. The read terminals share one runner, so every read, present and future, routes the
  same way.
- A replica qualifies when its last check (at most 3 seconds old) found it in recovery and within
  `MaxReplicaLag`. keel picks the qualifying replica with the fewest reads in flight per unit of
  weight, then the fewest reads so far per unit of weight, and falls back to the primary, counting
  the fallback.
- One goroutine per `DB` checks the replicas every second; `Close` stops it. Lag is the age, on
  keel's clock, of the newest primary WAL insert position (sampled before the replicas are read,
  normalised past page headers) that the replica has replayed: no clock agreement between servers
  is needed and an idle primary shows no lag. Without a primary sample it falls back to
  `now() - pg_last_xact_replay_timestamp()`.
- Read-your-writes is on for every marked context. A write's commit position is read by a
  `SELECT pg_current_wal_insert_lsn()` queued after `COMMIT` in the commit's round trip; the
  position read before `COMMIT` would precede the commit record and is not enough. Writes outside
  `Write` on a marked context therefore run in an implicit transaction. The position lives on the
  context (per `DB`); replica reads on it use a replica the tracker saw replay it, or one it has not
  seen yet when the write is older than that replica's lag, with a guard statement
  (`SELECT 1 / (replay_lsn >= $1)::int`) first in the read's pipeline. A failing guard aborts the
  pipeline on the replica, so it does no work, and the read reruns on the primary under the same
  observer events. A position that could not be read pins the context to the primary.

## Data versions

- Off by default (`Config.DataVersions.Enabled`): each changed (tenant, table) pair costs a row
  upsert per `Interval` (default 1s).
- Every write terminal records its pairs on the runner: in the transaction state, delivered when
  `COMMIT` is sent (also when it fails, since it may have committed) and dropped on rollback; or,
  outside a transaction, delivered when the runner closes, even after an error.
- A per-`DB` goroutine writes the pending set every interval in one statement per 5,000 pairs
  (`INSERT … SELECT FROM unnest($1, $2, $3) ORDER BY … ON CONFLICT DO UPDATE SET version = version + 1
  RETURNING …`), on its own connection and never inside a write's transaction, so writes never
  contend on version rows. Key order prevents deadlocks between processes. Failed pairs return to
  the set. `Close` and `FlushDataVersions` write the set at once.
- The same statement `pg_notify`s the versions it wrote on channel `keel_data_versions`, delivered
  at commit: one payload per group of pairs (`$3`, computed in Go so each payload stays under
  7,000 bytes of the 8,000 limit), a `jsonb` array of `[tenant key, table, version]`; a pair too
  large for a payload notifies `*` (reset everything). Every process notifies whether or not it
  caches, since the result caches of other processes depend on it.
- Tenant keys are the tenant values of the longest prefix of `TenantColumns` the table has,
  escaped and joined with `/`. A system-scope write records each row's tenant (entity writes read
  the rows; bulk writes and `InsertFrom` wrap the statement in
  `WITH w AS (… RETURNING tenant columns) SELECT tenant columns, count(*) FROM w GROUP BY …`).
  Tables without tenant columns and unattributable changes record `""`. `DataVersions` sums the
  key, its prefixes and `""`, so over-invalidation is possible and staleness is not.
- `keel_data_versions` is created on first use under a transaction-scoped advisory lock when
  `to_regclass` does not find it; a migration can create it instead with the definition in the
  guide. keel/migrate is not used, so the feature works whichever migration tool an application
  runs.
- `Exec` cannot see its tables: `MarkChanged` records them.

## Result cache

`Query.Cache(keel.TTL(d), keel.AlsoReads(tables...))` serves `One`, `First`, `List`, `All`,
`Count`, `CountUpTo`, `CountEstimate`, `Exists` and `Page` and their `Batch` items from
`Config.Cache` (`Local keel.LocalCache`, default `keel.MemoryCache(64 MiB)`; `Shared keel.SharedCache`;
`Key func(ctx) string`; `MaxEntryBytes`, default 1 MiB; `OnError`). `Open` refuses a cache
without `DataVersions.Enabled`, and one without `Key` when `OnBegin` is set (the rows OnBegin's
settings expose are invisible to the key otherwise). `Key` and `OnBegin` run on a callback context: the caller's
context without the transaction of this DB and with the tenant slot of the work they serve. A
statement they send can neither join the transaction they compute for (and begin it, re-entering
`OnBegin`) nor wait for a second slot of the tenant, which at `TenantConcurrency: 1` deadlocks. `DB.CacheStats()` reports hits, misses,
bypasses, errors, local bytes, pairs held and whether the listener is up.

- **Granularity.** One entry per terminal call (`runReads`): every item of a `Batch` must ask,
  the TTL is the shortest; a mixed batch bypasses. The key is SHA-256 over the format version,
  the tenant key, `Key(ctx)`, which reads load levels after the first round trip, and the text and
  arguments of every statement of the first round trip (pipelined loads included). Arguments key
  by value and type (a `driver.Valuer` by its value, other types by reflection); a function or
  channel argument bypasses.
- **Validity.** Renders on the terminal's context add every table they read (each query level's
  base: the query, subqueries, `Any`/`None`, CTE bodies, loads; joined relations; tables named in
  `FROM`/`JOIN`) to a read set; `AlsoReads` adds tables only raw SQL reads. Before each round trip
  of a miss the runner hook stamps the tables added since the last one: for each table and each
  prefix of the tenant key, the pair's version and, while this process has writes to the pair its
  version does not show, its local epoch. Stamps are taken before the statement runs, so data is
  never older than its stamps. The entry holds the tables and stamps; a lookup decodes the entry
  and compares its stamps with the current ones, so an entry stops matching the moment a table it
  read changes. Versions only grow; truncating `keel_data_versions` under a running cache would
  revalidate old entries.
- **Versions in memory.** A version bus per `DB` holds the pairs the process read, in 64 locked
  shards of at most 2^20 pairs in all (evicting pairs without local writes). An unknown pair is
  read from `keel_data_versions` in one round trip on the terminal's own connection: its
  transaction's (begun, or about to begin with the terminal's round trip, before `BEGIN`), else the
  pool connection the runner then sends on, so the cache never waits for a second pool connection
  while its caller holds one (a runner on a replica reads them on a primary connection). Until the
  table has been seen it is looked up with `to_regclass` first and never created there, so the
  read cannot fail the caller's transaction; a missing table is version 0. A notification updates a
  pair the bus holds (and any pair while a read is in flight, so a bump during the read is not
  lost); versions take the maximum. A hit on known pairs costs no round trip.
- **Local epochs.** `versioner.add` (every committed write of this process) bumps the epoch of its
  pairs under the versioner's lock; a flush marks the epochs it took and, once written, marks them
  clean and takes the returned versions. A stamp with an epoch is never written to the shared
  store, so a process never serves its own stale data and never shares data keyed by its local
  state.
- **Listener.** A dedicated connection (`CacheConfig.Listen` as given, else a copy of the primary
  pool's settings after its `BeforeConnect`) runs
  `LISTEN keel_data_versions`; `Open` waits for the first connection and retries in the background
  when it fails. After 30s without a notification it pings. On loss, and again on reconnect, the
  bus forgets every version (keeping dirty epochs) and the cache is bypassed until it is back;
  reconnects back off from 100ms to 5s. Entries survive: a refilled version that still matches
  revalidates them. It needs a session connection: behind a transaction-mode pooler set `Listen`
  to a direct connection.
- **Entries** hold the raw wire result of every round trip the terminal sent, loads included:
  per statement the hash of its text, its command tag, its field descriptions and each row's raw
  values. On a miss the runner hook wraps each statement's scanner so the rows it reads are
  copied as it reads them. On a hit the runner hook feeds each round trip, in order, from the
  entry (`replayRows` implements `pgx.Rows`), so the same scanners, plans, relation grouping,
  `AfterLoad` hooks and cardinality checks run and every caller gets fresh rows. A round trip
  whose statement count or text hash differs, or a scanner error, fails the hit with
  `errCacheReplay`; the terminal reruns against the database and refills.
- **Decoders.** Hits decode on decoder connections: connected with the primary pool's settings,
  `AfterConnect` run (enums, `LoadTypes`, JSON, the application's codecs), then closed, keeping
  their type map and plan cache. At most `GOMAXPROCS`, each replaced after the pool's
  `MaxConnLifetime`, so a hit never takes a pooled connection.
- **Bypass.** A write transaction, a joined transaction, an explicit `REPEATABLE READ` or
  `SERIALIZABLE` transaction that has begun (its snapshot is older than the current stamps), a
  system scope, `Lock`, and a down listener bypass. An explicit read-committed `Read` uses the
  cache. A snapshot transaction that has not begun does too: transactions begin with their first
  statement, so a hit sends nothing and leaves it unbegun (its snapshot is taken by its first
  statement that reaches the database, later than the hit's data), and a miss begins it in its own
  round trip, after the stamps, and is stored. In a snapshot transaction that has begun, a later
  round trip of a miss (a level loaded per level) that reads a table not stamped yet makes the
  result unstorable, since its snapshot predates any stamp taken now. An implicit transaction keel
  owns (OnBegin, pipelined loads) begins after the lookup, so it uses the cache. A miss read on a
  replica is not stored (the versions come from the primary).
- **Decoders and the pool.** Decoder connections are opened with `pgx.ConnectConfig`, never taken
  from the application's pool.
- **Stores.** A local miss reads the shared store; a shared hit is validated the same way and
  copied into the local store. Concurrent misses of one key in a process wait for the first and
  look again; a waiter first returns a connection its lookup took to read versions (its own, or
  that of a transaction that has not begun) and the tenant slot it took, so waiters hold neither a
  pool connection nor a slot, and takes the slot again before it looks. `Config.MaxWait` bounds
  the wait; past it the waiter reads the database itself. Shared-store errors are misses, reported to `OnError`.
- Not covered: `DynQuery` and `Into` projections; data read by raw SQL without `AlsoReads`;
  writes keel cannot see without `MarkChanged`. Changes by other processes are visible after their
  flush interval plus the notification's delivery.

## Hooks

| Kind | Registration | Use |
|---|---|---|
| model hooks | methods on the model | normalisation and derived fields |
| table hooks | `keel.OnCreate/OnUpdate/OnDelete/OnWrite[T](db, handle, phase, fn, keel.Needs(cols...))` | validation, audit, side effects |
| interceptors | `db.Intercept(handle, fn)`, `db.InterceptAll(fn)` | add `WHERE` predicates to every statement whose base is a table and that has one: queries, loads, subqueries, bulk updates and deletes, `Save`, `DeleteRow`. Never inserts, including an upsert's conflict update |
| observers | `Config.Observers` | tracing, metrics, slow-query log, N+1 detection, recording statement shapes for the plan guard (`keel.RecordShapes`) |

Table hook phases:

- `BeforeWrite` runs inside the transaction before an entity write (`Create`, `CreateMany`,
  `Copy`, `Save`, `Upsert`, `CreateOrSkip`, `DeleteRow`) and may change the row or abort.
- `AfterWrite` runs inside the transaction after the statement, for entity and bulk writes; an
  error rolls back. A table with `AfterWrite` hooks for any operation a write can perform makes
  that write transactional (an `Upsert` for create or update hooks), and the hooks of a write in
  such an implicit transaction (`BeforeWrite` and `AfterWrite`) get a context that carries it, so
  what they run through keel joins the write.
- `Committed` runs after the transaction commits.

Bulk writes on a table with `AfterWrite` or `Committed` hooks return their rows through `RETURNING`.
For every update and delete (`Save`, an upsert's conflicting rows, `DeleteRow`, bulk updates and
deletes) the previous values of the columns hooks declared with `Needs` are read in the same
statement through `RETURNING old.…` (PostgreSQL 18) into `Mutation.Old`. Hooks are resolved per
table when registered; a table without hooks pays nothing.

Observers receive the statement text (never values), the number of bound arguments (and the values
themselves only with `Config.ObserveArgs`), the replica it ran on, whether it ran in an explicit
transaction, duration, rows and the error the terminal returns (a constraint violation already
resolved to a `*ConstraintError`); statements sent in one round trip share a start time and a duration. Each statement's
observers chain their `BeforeQuery` contexts from the terminal's context; `AfterQuery` gets the
statement's final context, and a round trip that carries one statement runs under it. Statements
of one round trip get sibling contexts and the round trip runs under the terminal's context.

## Schema verification

`keel.Verify(ctx, db, tables) ([]Drift, error)` checks that the database can serve the models: the
migrations own the schema and the models describe it, so nothing else keeps them in step. It reads
every table, view, join table and relation target the tables name in one catalog statement
(`to_regclass` of each quoted name, columns with domains resolved to their base type, array element
types, `attnotnull` and the domain's `NOT NULL`, `attgenerated`, `attidentity`, the primary key's
columns) on a pool connection, outside transactions and scopes, and compares in Go. A drift names
the table, column and field and carries a fix; drifts come per table in the order given. The check
rules are the decoding rules: a kind holds the types pgx scans it from (`int16` only `smallint`,
`int64` any integer, `string` text types, enums, `uuid` and `json`), a field holds NULL when it is
nullable, nullzero, jsonb, `[]byte`, an array or a custom type, and a join table carries
`Table.JoinTableOf`'s columns. `keelcli`'s `verify` runs it for `App.Tables`.

`keel.VerifyFuncs(ctx, db, decls...)` checks declared functions the same way: one statement over
`pg_proc` (plain functions only, `pg_function_is_visible` for an unqualified name), then in Go an
overload must take the declared arity (`pronargs - pronargdefaults` up to `pronargs`, any count from
there for a variadic one) and return a type the result kind holds by `kindHolds`, domains resolved
and pseudo-type results skipped. Drifts are `missing_function`, `function_arity` and
`function_result`, with the function's name in `Table`.

## Plan guard

A query that scans a large table sequentially, costs too much or sorts a page in full is caught
before it ships, from the statements the application really sends:

- `keel.RecordShapes(w io.Writer) Observer` writes each distinct statement text once, in
  `BeforeQuery`, as a JSON line of `keel.Shape{SQL, Op, Table, Args}`
  (`{"sql": "...", "op": "...", "table": "...", "args": n}`). It keeps the texts it has seen in a
  map behind an `RWMutex`, so a repeated statement costs a read-locked lookup and each new line is
  one `Write` under the lock; it skips what EXPLAIN cannot plan (texts not starting with `SELECT`,
  `WITH`, `INSERT`, `UPDATE`, `DELETE`, `MERGE`, `VALUES` or `TABLE`, and advisory locks).
  Statement text never contains values, so the set is bounded by the code, not the data.
- `keel.RecordShapesFile(path) (Observer, func() error, error)` appends to a file (one write per
  line), so the test binaries of one `go test ./...` share it; the close function returns the
  first write error. `keel.ReadShapes(r)` reads the lines back, each distinct text once in first-seen
  order.
- `keel plans` (keelcli, so `cmd/keel` and every application binary have it whenever the App has a
  `Config`) explains every shape with `EXPLAIN (GENERIC_PLAN, VERBOSE, FORMAT JSON)` over the
  simple query protocol on one connection of its own, outside transactions: placeholders stay
  unbound, the plan is the generic one a prepared statement settles on, and `VERBOSE` names each
  relation's schema. A shape that is not a single statement (checked by a lexer that skips
  literals, quoted identifiers, dollar quotes and comments) is never sent. The `reltuples` of every
  relation scanned sequentially, or read under a sort under a limit, are then read in one catalog
  statement. Findings: a `Seq Scan`
  (parallel included) of a relation with more than `-seq-scan-rows` rows (default 10000; never
  analyzed counts as empty; `-1` flags every one); a total cost over `-max-cost`; with
  `-sort-limit`, a `Sort` a `Limit` reads through `Gather`, `Gather Merge`, `Result`,
  `Subquery Scan`, `Materialize`, `Unique` or `Merge Append` only, when the largest relation the
  sort reads has more than `-seq-scan-rows` rows, reading no further down than an `Aggregate` or
  `Group`, whose returned rows count instead (an `Incremental Sort`, a sort feeding an aggregate,
  a sort of a few grouped rows, or a sort of a small reference table is not one). A shape EXPLAIN cannot plan is reported with its error
  and does not fail the run; any finding exits 3, keelcli's "a check found problems".
  PostgreSQL 16 or later is required.

## Errors

`ErrNotFound`, `ErrNotSingular`, `ErrConflict` (`*ConflictError`), `ErrNoScope`,
`ErrAllTenantsRequired`, `ErrSystemScopeRequired`, `ErrTenantMismatch`, `ErrUnbounded`,
`ErrReadOnly`, `ErrBudgetExceeded` (`*BudgetError`); Postgres helpers `IsUnique`, `IsForeignKey`, `IsCheck`, `IsExclusion`, `IsNotNull`,
`IsRetryable`, `Code`, `Constraint`.

## Migrations

`keel/migrate` applies `<version>_<name>.up.sql` / `.down.sql` pairs in version order. An up file
runs in one transaction with its bookkeeping row; `*.notx.up.sql` runs statement by statement
outside a transaction, split at `--keel:split` lines. Runs serialise on an advisory lock taken by
polling (a waiting statement would hold a transaction that `CREATE INDEX CONCURRENTLY` waits on);
an applied file whose content changed stops `Up` with `ErrChanged`. `Options.AfterFunc` runs once after a run that applied anything, outside every transaction and after the lock, for work SQL cannot do (marking migrated tables changed for data versions).

## Performance rules

- `bench/` tracks time and allocations per operation against Bun and raw pgx; a change that makes a
  benchmark worse explains why in its commit.
- Renders and their buffers are pooled; a statement copies its text and arguments out of the
  render.
- No `sync.Map` and no generic type with methods in any type reachable from `schema.Table`.
- Statement text never contains values.
- Dynamic results allocate per batch and per column, never per row or per value.
