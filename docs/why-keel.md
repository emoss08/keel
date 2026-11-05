# Why keel

keel is the data layer of one kind of application: a backend that serves many organizations from
one PostgreSQL database. This document says what keel believes, what it is good at, and when another
library will serve you better. Read it before you adopt keel; the [tour](tour.md) shows the API and
the [design](design.md) is the full contract.

## Contents

- [The name](#the-name)
- [Principles](#principles)
- [Is keel right for you?](#is-keel-right-for-you)
- [How keel compares](#how-keel-compares)

## The name

A keel is the beam along the bottom of a ship's hull. Everything else is built on it, and it is
what keeps the ship upright and stops it drifting sideways. keel does that job under an
application's data: it is the part every query rests on, and it keeps each tenant's work from
drifting into another's, so the application stays on an even keel as it grows.

## Principles

- **Typed end to end.** Columns, relations, values, assignments and results are checked by the
  compiler. `s.Status.In(...)` accepts only `fleet.Status` values; a misspelled column does not
  compile. Raw SQL exists, but it is explicit (`keel.Raw`) and greppable.
- **Tenancy by construction.** The tenant comes from the context and is applied to every statement
  on a tenant table: reads, joins, relation loads, subqueries, updates, deletes and inserts. A
  statement without a scope fails. Crossing tenants takes a system scope with a reason and an
  explicit `AllTenants()`.
- **Fewer round trips.** Transactions begin with their first statement. A lone statement that needs
  a transaction (a row-level security `set_config`) is one pipelined `BEGIN; ...; COMMIT`. A page and
  its total share a round trip, so do the independent reads of a `keel.Batch`, and a relation tree
  loads inside the read's own round trip wherever the read sees one snapshot.
- **Prepared statements, always.** Values are bind parameters, so a statement's text depends only on
  its shape and Postgres skips parsing and, usually, planning. `In` binds one array: 3 IDs and 3,000
  IDs are the same statement.
- **Built for many tenants on one database.** Per-tenant concurrency limits, query budgets that catch
  N+1 loops, a result cache invalidated by data versions, read replicas with lag tracking, and
  reports of slow waits that name who is holding what.
- **Bounded by default.** `List` needs a limit, pagination is keyset-based, bulk inserts send one
  array per column, and every wait for a slot or connection can be capped.
- **No reflection on the hot path.** Generated accessors decode rows straight from pgx's buffers
  into struct fields.

keel is Postgres-only and uses pgx v5, never `database/sql`.

## Is keel right for you?

keel is not a general-purpose ORM. It is the data layer of one kind of application, built with
strong opinions, and it carries machinery many projects will never want. Read this before the
quick start.

### What it is for

A backend that serves **many organizations from one PostgreSQL database**: every tenant table
carries tenant columns, the same code serves every tenant, and the data is relational enough that
most screens read a record with several levels of related rows. Think of a transportation
management system (keel is built alongside [Trenova](https://github.com/emoss08/Trenova)), a CRM,
a helpdesk, an ERP or a marketplace back office: B2B SaaS with a large schema, deep detail pages,
long lists, bulk imports and reports.

### What it does very well

- **Keeping tenants apart.** You cannot forget the tenant filter: keel adds it to every read, join,
  relation load, subquery and write, and fails a statement that has no scope. Work across tenants
  is written down, with a reason:

  ```go
  ctx := withSystem(ctx, "purge canceled shipments") // your Config.System reads this
  n, err := s.AllTenants().
      Where(s.Status.Eq(fleet.StatusCanceled), s.CanceledAt.Lt(cutoff)).
      Delete(ctx, db)
  ```

- **Detail pages and lists in one round trip.** A shipment with its customer, moves, stops and
  locations, or a page of fifty with a total count, is one exchange with Postgres outside a
  transaction. The saving grows with the distance to the database.
- **Dashboards.** Independent reads of a screen go in one round trip with `keel.Batch`, and reads
  that ask for the result cache come back from memory until a table they read changes:

  ```go
  err = keel.Batch(ctx, db,
      keel.ListInto(s.Where(s.Hazmat.IsTrue()).Order(s.CreatedAt.Desc()).Limit(50).Cache(), &recent),
      keel.CountInto(s.Where(s.Status.Eq(fleet.StatusNew)).Cache(), &unassigned),
      keel.OneInto(c.Where(c.Code.Eq("ACME")).Cache(), &cust),
  )
  ```

- **Imports and bulk changes.** `CreateMany` sends one array per column, `Copy` streams binary
  `COPY` with IDs, tenant columns and hooks applied, and upserts and bulk updates are one
  statement each.
- **Staying up when one tenant misbehaves.** Per-tenant concurrency limits, query budgets that fail
  an N+1 loop, a ceiling on waits, and slow-wait reports that name the code holding the slots.
- **Reports users build themselves.** `keel/analytics` turns a report definition (columns across
  relations, filters, grouping, pivots, formulas) into tenant-scoped SQL, with exports and rollups.
- **Living with a large schema.** Typed columns and relations, labels and descriptions derived from
  the models, `keel.Verify` to catch drift between models and migrations, golden SQL snapshots and
  index assertions in tests.

### When to choose something else

- **You need another database, or database portability.** keel is PostgreSQL only and leans on it:
  arrays, `unnest`, `COPY`, pipelining, `LISTEN/NOTIFY`, advisory locks, row-level security.
- **Your application is single-tenant and small.** keel works without tenant columns, but you would
  pay for code generation, a large API and opinionated runtime machinery to get a query builder.
  [sqlc](https://sqlc.dev) or plain [pgx](https://github.com/jackc/pgx) will serve you better.
- **You want to write SQL first.** keel builds SQL from typed Go. If you would rather keep queries in
  `.sql` files and generate Go from them, use sqlc.
- **You want the schema generated from your models.** keel never writes migrations from structs; the
  schema lives in SQL migrations you write. GORM's `AutoMigrate` and ent's schema migrations do the
  opposite.
- **You expect classic ORM behaviour.** There is no lazy loading, no identity map, no unit of work,
  no built-in soft delete and no polymorphic associations. Every load is asked for explicitly
  (an interceptor can add a soft-delete filter if you want one).
- **You need `database/sql`.** keel runs on pgx v5 and nothing else, so libraries that want a
  `*sql.DB` cannot share its connections.
- **You want a small dependency graph.** keel is one module with its CLI, migrations, seeds and
  analytics. Go compiles only the packages you import, but `go.mod` lists their dependencies
  (Charm's terminal libraries, excelize, Arrow) all the same.
- **You need a stable API today.** keel is pre-1.0 and shaped by one application's needs; minor
  versions can still change the API.

[docs/design.md](design.md) has the full design and its non-goals.

## How keel compares

Every library below is a good choice for some application; this is where each stands on the
questions keel was built around.

| | keel | sqlc | GORM / Bun | ent |
|---|---|---|---|---|
| Queries are written as | typed Go, from a generated package | SQL files, Go generated from them | Go builders with string fragments | typed Go, from a generated package |
| Schema comes from | your SQL migrations | your SQL migrations | your migrations, or the models (`AutoMigrate`) | the schema definitions |
| Tenant isolation | on every statement, from the context; unscoped statements fail | written by hand in each query | written by hand, or a plugin/scope | privacy rules and interceptors |
| Relation loading | explicit `Load`, every level in the read's round trip | written by hand | one query per level (`Preload`, `Relation`) | one query per level (`With…`) |
| Databases | PostgreSQL only (pgx v5) | PostgreSQL, MySQL, SQLite | many, through `database/sql` | many, through `database/sql` |
| Bulk writes | arrays through `unnest`, binary `COPY` | `:copyfrom` queries (pgx), or by hand | multi-row `VALUES` | multi-row `VALUES` |

[performance.md](performance.md) measures keel against these libraries, each written the way its own
documentation writes it.
