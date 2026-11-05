# Models and code generation

## A model

A model is a plain struct that embeds `keel.Table`, whose tag names the table. Every stored field
carries a `keel` tag; a field without one, or tagged `keel:"-"`, is not stored. Stored fields must
be exported. keel reads only its own tag key, so `json`, `db` or `bun` tags can stay.

```go
// Shipment is a load moved for a customer.
type Shipment struct {
    keel.Table `keel:"shipments,report=Operations"`

    ID             ids.ID  `keel:"id,pk,autoid:shp_"`
    OrganizationID ids.ID  `keel:"organization_id,pk,tenant"`
    BusinessUnitID ids.ID  `keel:"business_unit_id,pk,tenant"`
    CustomerID     ids.ID  `keel:"customer_id"`
    BillToID       *ids.ID `keel:"bill_to_id"`              // pointer: nullable
    // ProNumber is the carrier's progressive number, unique per tenant.
    ProNumber    string         `keel:"pro_number,label=PRO Number"`
    Status       Status         `keel:"status"`             // named type with constants: an enum
    Weight       *int64         `keel:"weight,format=weight"`
    Total        Money          `keel:"total,type=decimal"` // sql.Scanner type
    Labels       []string       `keel:"labels"`             // text[]
    Attributes   map[string]any `keel:"attributes,jsonb"`
    SearchVector string         `keel:"search_vector,noselect,readonly,tsvector=english"`
    Version      int64          `keel:"version,version"`
    CreatedAt    int64          `keel:"created_at,autocreate"` // int64 = Unix seconds; time.Time also works
    UpdatedAt    int64          `keel:"updated_at,autoupdate"`

    Customer        *Customer `keel:"belongs:customer_id"`
    BillTo          *Customer `keel:"belongs:bill_to_id,label=Bill-to Customer"`
    Moves           []*Move   `keel:"many:shipment_id"`
    Tags            []*Tag    `keel:"m2m:shipment_tags.shipment_id.tag_id"`
    OriginStop      *Stop     `keel:"pick:moves.stops,first=moves.sequence|stops.sequence"`
    DestinationStop *Stop     `keel:"pick:moves.stops,last=moves.sequence|stops.sequence"`
}
```

Table and column names are lower-case identifiers (`[a-z_][a-z0-9_]*`), never schema-qualified.
Doc comments become descriptions in the generated schema; write them.

## Column tag options

| Option | Meaning |
|---|---|
| `pk` | part of the primary key (tenant columns usually are too) |
| `tenant` | filled from the scope on insert, compared with the scope on every statement, added to every relation join |
| `autoid:<prefix>` | an empty value on insert becomes `Config.NewID(prefix)`; string field |
| `autocreate` | stamped on insert when zero (an imported row's value is kept) |
| `autoupdate` | stamped on insert and every update, bulk updates and upserts included |
| `version` | optimistic locking (`int64`): `Save` compares and increments; bulk updates and upserts increment |
| `readonly` | never written (generated/trigger columns); read back after inserts and saves |
| `noselect` | not selected by default; still usable in predicates (pair with `readonly` for generated columns) |
| `jsonb` | JSON-encoded; nil map/slice in a non-pointer field writes `{}`/`[]` |
| `jsonnumber` | with `jsonb`: numbers in `any`/`map[string]any` decode as `json.Number` (exact past 2^53) |
| `sqltype=<name>` | the column's SQL type for casts (`ToExpr` writes `CAST(e AS name)`), e.g. an enum |
| `tsvector[=config]` | full-text operators (`Match`, `Rank`, `RankCD`) with a default config |
| `nullzero` | zero value ⇄ NULL for a non-pointer field (optional FKs: empty must be NULL) |
| `default` | an unset field (nil pointer, NULL `Null…`, nullzero zero) takes the column's DB default, read back |
| `computed` | not a column: filled only by `Compute(field, expr)`; never written |
| `label=`, `noreport`, `report`, `dimension`, `measure`, `agg=`, `format=`, `type=` | metadata for reports (report options need `report` on the table tag) |

Rules keel gen enforces: at most one `version`, `autoid`, `autocreate`, `autoupdate` per model;
`readonly` and `nullzero` cannot combine with `pk`, `tenant`, `version`, `autoid`, `autocreate`,
`autoupdate`; `computed` excludes those and `readonly`/`noselect`; unknown or repeated options fail
with the field's file:line:col.

**There is no `default:` value or `notnull`.** The SQL migrations own the schema. The value in the
struct is the value written; keel never substitutes `DEFAULT` for a zero value (except with the
`default` option on a field that can be unset).

## Go types → generated column types

| Go type | Column type |
|---|---|
| `string` | `keel.StringCol` |
| `int64`/`int`, `int32`, `int16` | `keel.Int64Col`, `keel.Int32Col`, `keel.Int16Col` |
| `float64`, `float32` | `keel.Float64Col` |
| `bool` | `keel.BoolCol` |
| `[]byte` | `keel.BytesCol` |
| `time.Time` | `keel.TimeCol` |
| `[]string`, `[]int64` | `keel.StringArrayCol`, `keel.Int64ArrayCol` |
| other basic slices | `keel.AnyCol` |
| `string` + `tsvector` | `keel.TSVectorCol` |
| anything + `jsonb` | `keel.JSONBCol` |
| named type over a basic type (`Status`, `ids.ID`) | generated wrapper (`StatusCol`, `IdsIDCol`) whose methods take the named type |
| `sql.Scanner` type (`Money`, `decimal.Decimal`, PostGIS) | generated wrapper over `keel.AnyCol` |

Pointers and `Null…` scanners are nullable. Structs, maps and other slices need `jsonb`. A named
type over a basic type is stored as the basic type even if it implements `Scanner`/`Valuer` (its
methods aren't called). Enum values: the constants of a named string/int type become
`Column.Enum`; comparing with a constant writes it into the SQL (so partial indexes match), other
values are bound.

## Relations

| Tag | Meaning |
|---|---|
| `belongs:<fk>` | this table's `fk` → target's primary key (to-one, pointer field) |
| `one:<fk>` | target's `fk` → this table's key; at most one row (pointer) |
| `many:<fk>` | target's `fk` → this table's key (slice) |
| `belongs:<fk>\|<fk2>[=<col>]` | composite keys; every pair joins |
| `m2m:<join_table>.<local_fk>.<remote_fk>` | through a join table, which must carry the shared tenant columns and should have a PK over its columns |
| `pick:<rel>.<rel>,first=<seg>.<col>\|…` (or `last=`), optional `where=<seg>.<col>:<v>[;<v>]` | one row chosen along a to-many path in a total order |

- Never list tenant columns in a relation key: keel adds them to every join.
- Keys of to-many and pick relations must be string or integer columns.
- The target must be a model in the same `keel gen` run; a model may relate to itself.
- A pick path names relations by snake-case field name (`Moves` → `moves`), must cross a to-many
  relation, and may not go through another pick. keel appends the target's PK to the order.

## nullzero

For a non-pointer field whose column is nullable: zero ⇄ NULL on every write and read, and
predicates follow the field's meaning (`Eq(zero)` is `IS NULL`, `Ne(v)` is `IS DISTINCT FROM v`).
A nullzero FK holding zero has no related row. Not allowed on pointers, `Null…` scanners, or key /
tenant / version / auto columns. A struct type needs `IsZero() bool` or must be comparable.

## Computed fields

```go
type Contact struct {
    keel.Table `keel:"contacts"`
    // …
    CustomerName string `keel:"customer_name,computed,nullzero"` // LEFT JOIN may be NULL
    Loads        int64  `keel:"loads,computed"`
}

rows, err := c.
    Compute(c.CustomerName, c.Customer().Name).
    Compute(c.Loads, keel.Scalar(s.Pick(keel.Count()).Where(s.CustomerID.EqCol(c.CustomerID)))).
    Where(c.Loads.Gt(0)).Order(c.Loads.Desc()).Limit(50).List(ctx, db)
```

Inside a query that computes it, the field stands for its expression in `Where`, `Order`, `Pick`
and pages. Using it in a query that doesn't compute it fails. Writes refuse it.

## Model hooks

Methods the generator detects (wrong signatures are generation errors):

```go
func (s *Shipment) BeforeCreate(ctx context.Context) error // Create, CreateMany, Copy, Upsert, CreateOrSkip
func (s *Shipment) BeforeUpdate(ctx context.Context) error // Save
func (s *Shipment) BeforeSave(ctx context.Context) error   // after BeforeCreate/BeforeUpdate
func (s *Shipment) BeforeDelete(ctx context.Context) error // DeleteRow
func (s *Shipment) AfterLoad(ctx context.Context) error    // every loaded row, relations and cache hits included
```

Use model hooks only for normalisation and derived fields. Validation that needs the database,
audit and side effects belong in table hooks (`keel.OnCreate` etc.). Bulk `Update`/`Delete`,
`InsertFrom` and `UpsertFrom` run no model hooks.

## Constraint messages

```go
func (Location) Constraints() []keel.Constraint {
    return []keel.Constraint{
        {Name: "idx_locations_code", Message: "A location with this code already exists"},
    }
}
```

keel gen records them; `keel.Verify` reports a name the table lacks; a violation comes back as a
`*keel.ConstraintError` with `Message` and `Fields` (JSON names) when `Config.Tables` lists the
table (pass `keeltables.All()`).

## keel gen

```sh
go run github.com/emoss08/keel/cmd/keel gen ./internal/...
go tool keel gen ./internal/...            # after: go get -tool github.com/emoss08/keel/cmd/keel
go run github.com/emoss08/keel/cmd/keel gen -check ./internal/...   # CI: fail when stale
```

| Flag | Default | Meaning |
|---|---|---|
| `-out` | `{dir}/{name}db` | generated package directory (relative to module root) |
| `-kinds` | `keelkinds` | shared package for column types of named types declared outside model packages |
| `-tables` | `keeltables` | package whose `All()` lists every table and `Catalog()` finds them |
| `-tags` | | build tags for loading packages |
| `-acronyms` | `ID,URL,API,JSON,UUID` | words labels write in capitals (replaces the list) |
| `-check` | false | fail listing stale files instead of writing |

Pass every model package in one run (relation targets must be in the run). Use the same flags for
`-check` as for writing. Always reuse the project's existing invocation (Makefile target,
`//go:generate`, CI step).

Per model the generated package holds: `ShipmentsTable` (the handle type: a field per column,
methods to start queries and write rows), `ShipmentsQuery`, `var Shipments`, a method per relation
(`Customer() *CustomersTable`), `As(alias)`, `Get(ctx, db, key)` (when the key besides tenant
columns is one column), `ShipmentsSchema()`. A column or relation named like a handle method
(`Order`, `Group`, `Query`, `Copy`, `Get`, …) gets a suffixed member: `OrderCol`, `OrderRel()`.

Functions that hold a query take the interface type:

```go
func openShipments(s *fleetdb.ShipmentsTable, customer ids.ID) keel.Query[*fleet.Shipment] {
    return s.Where(s.CustomerID.Eq(customer), s.Status.Ne(fleet.StatusDelivered))
}
```

Generic keel types always take the model's pointer type (`keel.Query[*fleet.Shipment]`,
`keel.Page[*fleet.Shipment]`); a struct type argument panics.
