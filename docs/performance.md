# Performance

keel is fast where a data layer can be: it sends fewer round trips than other Go libraries, decodes
rows without reflection, and keeps every statement a prepared shape. This document gives the
measurements, explains where the time goes, and lists what an application does to stay fast.
[benchmarks.md](benchmarks.md) has the full method, the raw output and how to rerun everything.

## Contents

- [Measurements](#measurements)
- [Why keel is fast](#why-keel-is-fast)
- [Keeping an application fast](#keeping-an-application-fast)
- [Build time](#build-time)

## Measurements

`bench/` runs the same operations through keel, sqlc, sqlx, raw pgx, Bob, ent, GORM, Jet and Bun,
each written the way its documentation writes it, with the tenant filter added by hand where the
library has no tenancy. Medians of 6 runs on a 4-vCPU Xeon VM, Postgres 18 in Docker, through a
proxy that adds 1 ms to every round trip, as a database in the same cloud region would (recorded
on 2026-10-26; fastest in bold):

| Operation | keel | sqlc | sqlx | pgx | Bob | ent | GORM | Jet | Bun |
|---|---|---|---|---|---|---|---|---|---|
| get one row by key | 1.88 ms | 1.87 ms | 1.88 ms | **1.80 ms** | 1.95 ms | 1.93 ms | 1.89 ms | 1.99 ms | 2.00 ms |
| list 100 rows | **2.48 ms** | 2.76 ms | 2.79 ms | 2.89 ms | 2.71 ms | 2.76 ms | 3.45 ms | 3.59 ms | 2.93 ms |
| list 1,000 rows | **5.62 ms** | 6.42 ms | 7.54 ms | 5.96 ms | 7.10 ms | 7.74 ms | 10.77 ms | 12.96 ms | 8.30 ms |
| 50 rows with a to-one and a to-many relation | **3.02 ms** | 6.47 ms | 6.83 ms | – | 8.40 ms | 6.96 ms | 8.71 ms | 7.25 ms | 6.95 ms |
| read in a transaction with a `set_config` | **1.98 ms** | 6.99 ms | 7.00 ms | – | 7.31 ms | 7.22 ms | 7.07 ms | 7.41 ms | 7.34 ms |
| keyset page of 50 with its total | **2.71 ms** | 4.47 ms | 4.66 ms | 4.69 ms | 4.42 ms | 4.72 ms | 4.84 ms | 5.05 ms | 4.86 ms |
| insert one row | 2.24 ms | **2.22 ms** | 2.36 ms | – | 2.36 ms | 2.39 ms | 2.28 ms | 2.41 ms | 2.37 ms |
| insert 1,000 rows | **15.97 ms** (16.75 ms with `Copy`) | 16.97 ms | 20.02 ms | – | 22.70 ms | 22.69 ms | 19.01 ms | 26.40 ms | 24.72 ms |
| save with optimistic locking | **2.19 ms** | 2.24 ms | 2.23 ms | – | 2.43 ms | 2.22 ms | 2.32 ms | 2.37 ms | 2.46 ms |
| build a statement, no database | 4.1 µs | – | – | – | 12.6 µs | – | 8.2 µs | 22.2 µs | **3.6 µs** |

- **keel wins where it saves round trips.** A relation tree loads in the read's own round trip (the
  others take one per level), a scoped read pipelines `BEGIN`, the `set_config`, the read and
  `COMMIT` into one round trip (the others take four), and a page shares its round trip with its
  count. That makes keel 2.1 to 2.9 times as fast as every other library on relation loads, 3.5 to
  3.7 times on scoped reads and 1.6 to 1.9 times on pages, and the gap grows with the distance to
  the database.
- **keel decodes rows fastest.** Its lists lead here and with the database on the same host,
  where the next fastest list of 100 takes 1.3 times keel's time. It allocates 14 times a row on
  a list of 1,000, against 19 for raw pgx, 22 for sqlc and 34 to 73 for the ORMs.
- **Where an operation is one round trip, everyone is close.** Gets, inserts and saves land within
  12% of each other. Bun builds statements about 13% faster than keel.

[docs/benchmarks.md](benchmarks.md) has the same table with the database on the same host,
how each library is written, allocations, the raw output and how to rerun everything.

## Why keel is fast

- **Round trips.** Across a network, each round trip costs the latency to the database, and most
  of a request's time is spent waiting on them. keel pipelines what it can into one: a read and
  its whole relation tree (each level selects its parents' keys with a subquery over the level
  above, inside one snapshot), a keyset page and its total, the independent reads of a
  `keel.Batch`, and `BEGIN`, the `OnBegin` statements (row-level security), the statement and
  `COMMIT` of a lone terminal. See [One round trip](guide.md#one-round-trip) and
  [Round trips](guide.md#round-trips).
- **Prepared statements.** Values are always bind parameters, so a statement's text depends only on
  its shape: Postgres parses each shape once per connection and, with `plan_cache_mode = auto`,
  usually plans it once too. `In` binds one array, so 3 IDs and 3,000 are the same statement. See
  [Statement preparation](guide.md#statement-preparation).
- **Decoding.** Generated accessors decode rows straight from pgx's buffers into struct fields,
  without reflection. Rows of a statement are allocated in slabs, scan plans are cached per
  connection, and enum values and tenant strings are shared instead of allocated per row. See
  [Decoding](guide.md#decoding).
- **Bulk writes.** `CreateMany`, `Upsert` and `CreateOrSkip` send one array per column through
  `unnest`, 10,000 rows per statement and every chunk in one round trip; `Copy` streams binary
  `COPY`. See [Bulk writes](guide.md#bulk-writes).
- **Nothing for what you do not use.** Replicas, data versions and the result cache start no
  goroutine and add no statement while they are off.

## Keeping an application fast

keel makes the fast path the default, but the shape of an application's queries still decides
its speed. The [performance notes](guide.md#performance-notes) in the guide have the detail.

**Round trips**

- Load relations in the read that needs them (`Load(s.Customer(), s.Moves().Stops())`), never with a
  query per row. A `keel.Budget(ctx, keel.MaxRepeats(n))` in request middleware fails an N+1 loop.
- Give a limited query an `Order`: without one, the levels below it load one round trip each.
- Ask a page for its total in the same call (`keel.After(cursor, n).Total(cap)`), and put a
  screen's independent reads in one `keel.Batch`.
- Inside `Read` and `Write` every terminal is a round trip. Run read transactions that load trees
  at `keel.RepeatableRead` (or set `Config.ReadIsolation`), so their loads pipeline.
- Mark whole requests with `keel.OnReplica` when replicas serve reads, and use `.Cache()` for reads
  a dashboard repeats.

**Statements and plans**

- Keep the number of statement shapes small: build `keel.Raw` text from constants and pass values
  as arguments. `Config.StatementCacheCapacity` (default 512) should exceed the shapes a connection
  runs.
- When one statement serves tenants of very different sizes, plan it for its values with
  `CustomPlan()`, or use `PlanCacheCustom` for the whole database.
- Use `keel.Const*` where an expression must match an expression index or a partial index
  predicate; enum constants are written into the statement for the same reason.

**Counting**

- Prefer `Exists`, `CountUpTo`, `CountEstimate` and `Total(cap)` to an exact `Count` on large
  tables: they stop at the limit.

**Writes**

- Insert many rows with `CreateMany` (or `Copy` for tens of thousands), never a loop of `Create`.
- Change many rows with one bulk `Update` or `Delete`, and give each row its own values with
  `keel.Unnest`.

**Indexes**

- Lead every index on a tenant table with its tenant columns: every statement compares them with
  constants.
- For a keyset page ordered by `created_at DESC`, index
  `(organization_id, business_unit_id, created_at DESC, id DESC)`, and keep page orders on
  non-nullable columns sorted one way.
- Index each to-many relation's foreign key after the tenant columns, with the load's order column
  when it has one; index a many-to-many join table on `(local key, tenant columns)`.
- Check that indexes serve your queries: `keeltest.UsesIndex` in tests, and the
  [plan guard](guide.md#plan-guard) over every statement a test run sends.

## Build time

`keel.Query[T]` and `keel.Handle[T]` are generic interfaces, not generic structs, and the query
engine (`keel.QueryCore`) is not generic. The reason is a rule of the Go compiler: a package that
uses a generic type with methods from another package compiles every one of those methods again.
With one handle and one query type per table, and relation methods that let almost every package
see almost every table, that made an application recompile keel's methods for every table in
nearly every package: in Trenova (197 tables) it was 56% of all compile time, and one 728-line
package compiled 18,814 functions, 18,660 of them copies of keel's methods. An interface has no
method bodies, so using it costs nothing; keel's engine compiles once in keel, and the typed
wrappers `keel gen` writes compile once in the generated package. A 60-table prototype went from
1,892 functions compiled in an importing package to 1. See
[Why queries are interfaces](guide.md#why-queries-are-interfaces).
