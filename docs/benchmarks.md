# Benchmarks

`bench/` runs the same operations through keel, GORM, ent, sqlc, Bob, Jet, sqlx, Bun (Trenova's
fork at `2580fd2`) and raw pgx on one schema and data set (2,000 shipments, 600 moves, 10 customers
in one tenant).

```sh
cd bench && go test -run xxx -bench . -benchmem -count 6 > new.txt
go run golang.org/x/perf/cmd/benchstat@latest -col /impl new.txt
```

## 2026-10-26: keel against other Go libraries

The suite now runs every operation through **GORM, ent, sqlc, Bob, Jet and sqlx** as well as Bun
and raw pgx, on the same schema and data (2,000 shipments, 600 moves and 10 customers in one
tenant), twice: with Postgres on the same host, and through a proxy that adds a millisecond to
every round trip, as a database in the same cloud region would. Medians of 6 runs on the 4-vCPU
Xeon VM, Postgres 18 in Docker. Raw output:
[bench/results/2026-10-26-libraries-loopback.txt](../bench/results/2026-10-26-libraries-loopback.txt)
and [bench/results/2026-10-26-libraries-latency.txt](../bench/results/2026-10-26-libraries-latency.txt).

**How each library is written.** Every operation is written the way the library's own
documentation writes it, and each checks its result (row counts, loaded relations, a changed
version):

- No other library scopes queries to a tenant, so each filters on the tenant columns by hand, on
  its relation loads too. keel adds the same filter itself.
- Relation loads use each library's own mechanism: GORM's `Preload`, ent's `With…`, Bob's
  `Preload` and `ThenLoad`, one join query for Jet (the 50 shipments limited in a subquery
  first), and for sqlc and sqlx the usual three queries stitched together by key.
- The scoped read is a read-only transaction that runs the `set_config` a row-level security
  policy needs, then the read.
- The page is a keyset page of 50 after a cursor plus a count of the tenant's shipments. keel's
  `Page(…, keel.After(cursor, 50).Total(10_000))` caps the count at 10,000, which on 2,000 rows
  counts them all.
- The 1,000-row insert is each library's bulk path: one multi-row `INSERT` for GORM, ent, Bob,
  Jet, sqlx and Bun, `COPY` for sqlc (`:copyfrom`), and for keel `CreateMany` (one array per
  column through `unnest`) with `Copy` alongside.
- Every library on `database/sql` runs on pgx's stdlib driver, which prepares and caches
  statements as pgx does, with as many pool connections as pgxpool's default; sqlc runs on
  pgxpool itself. Each write benchmark deletes the rows it inserted when it ends, so no library
  writes into, or reads, a table another one grew.
- ent keys an entity by one column, so its models use `id` alone where the tables have a
  composite primary key. Rows decode into each library's natural types: `jsonb` into a map where
  the library decodes JSON itself (keel, Bun, GORM, ent) and into bytes elsewhere, `numeric` into
  a decimal or a string. sqlc, sqlx and raw pgx build no statements, so they have no
  statement-building number, and ent cannot render a typed query without running it.
- The generated code of ent, sqlc, Bob and Jet is checked in under `bench/` with its schema,
  queries and generator configuration.

### Through 1 ms of added latency

A round trip took 1.73 ms through the proxy against 0.23 ms direct. Fastest in bold.

| Operation | keel | sqlc | sqlx | pgx | Bob | ent | GORM | Jet | Bun |
|---|---|---|---|---|---|---|---|---|---|
| get one row by key | 1.87 ms | 1.89 ms | 1.87 ms | **1.82 ms** | 1.91 ms | 1.99 ms | 1.91 ms | 2.02 ms | 2.04 ms |
| list 100 rows | 2.61 ms | **2.53 ms** | 2.75 ms | 2.92 ms | 2.72 ms | 2.78 ms | 3.68 ms | 3.39 ms | 3.19 ms |
| list 1,000 rows | 6.74 ms | **5.88 ms** | 7.48 ms | 7.15 ms | 8.06 ms | 8.65 ms | 12.32 ms | 13.50 ms | 8.39 ms |
| 50 rows with a to-one and a to-many relation | **3.42 ms** | 6.98 ms | 7.03 ms | – | 9.57 ms | 7.49 ms | 9.37 ms | 6.99 ms | 7.42 ms |
| read in a transaction with a `set_config` | **1.96 ms** | 7.08 ms | 7.50 ms | – | 7.21 ms | 7.29 ms | 7.48 ms | 7.58 ms | 7.44 ms |
| keyset page of 50 with its total | **2.84 ms** | 4.34 ms | 4.54 ms | 4.52 ms | 4.85 ms | 4.82 ms | 5.05 ms | 5.12 ms | 4.70 ms |
| insert one row | 2.48 ms | **2.33 ms** | 2.53 ms | – | 2.49 ms | 2.38 ms | 2.35 ms | 2.50 ms | 2.51 ms |
| insert 1,000 rows | 18.33 ms (16.96 ms with `Copy`) | **16.99 ms** | 20.09 ms | – | 22.92 ms | 23.69 ms | 19.84 ms | 27.11 ms | 25.85 ms |
| save with optimistic locking | 2.25 ms | 2.20 ms | 2.23 ms | – | 2.31 ms | 2.20 ms | **2.19 ms** | 2.27 ms | 2.35 ms |
| build a statement, no database | 4.5 µs | – | – | – | 13.1 µs | – | 8.4 µs | 23.5 µs | **3.9 µs** |

### With Postgres on the same host

| Operation | keel | sqlc | sqlx | pgx | Bob | ent | GORM | Jet | Bun |
|---|---|---|---|---|---|---|---|---|---|
| get one row by key | 351 µs | **323 µs** | 350 µs | 331 µs | 371 µs | 379 µs | 381 µs | 431 µs | 480 µs |
| list 100 rows | 966 µs | **846 µs** | 1.05 ms | 1.21 ms | 1.07 ms | 1.11 ms | 1.62 ms | 1.80 ms | 1.40 ms |
| list 1,000 rows | 4.66 ms | **3.93 ms** | 5.30 ms | 5.39 ms | 5.87 ms | 6.62 ms | 9.88 ms | 11.53 ms | 6.97 ms |
| 50 rows with a to-one and a to-many relation | **1.34 ms** | 1.96 ms | 2.12 ms | – | 3.80 ms | 2.54 ms | 4.29 ms | 4.79 ms | 4.00 ms |
| read in a transaction with a `set_config` | **377 µs** | 892 µs | 1.07 ms | – | 1.15 ms | 1.11 ms | 1.12 ms | 1.21 ms | 1.36 ms |
| keyset page of 50 with its total | **1.12 ms** | 1.33 ms | 1.62 ms | 1.55 ms | 1.62 ms | 1.89 ms | 1.76 ms | 2.02 ms | 1.77 ms |
| insert one row | 761 µs | 767 µs | **730 µs** | – | 865 µs | 921 µs | 818 µs | 797 µs | 769 µs |
| insert 1,000 rows | **15.80 ms** (15.58 ms with `Copy`) | 16.10 ms | 19.48 ms | – | 23.57 ms | 22.08 ms | 18.86 ms | 26.57 ms | 25.55 ms |
| save with optimistic locking | **755 µs** | 761 µs | 823 µs | – | 844 µs | 808 µs | 788 µs | 794 µs | 862 µs |
| build a statement, no database | 5.0 µs | – | – | – | 12.7 µs | – | 8.8 µs | 23.1 µs | **4.2 µs** |

### What it shows

- **Round trips decide most of it, and keel saves them.** keel sends a relation tree and its root
  in one round trip; every other library sends one per level, so 50 shipments with their customer
  and moves take keel one round trip and the others three. A scoped read is `BEGIN`, the
  `set_config`, the read and `COMMIT` pipelined in one round trip, without waiting for the
  `COMMIT`'s reply; every other library makes four. A page and its count share a round trip.
  Through 1 ms of latency that makes keel 2.0 to 2.8 times as fast as every other library on
  relation loads, 3.6 to 3.9 times on scoped reads and 1.5 to 1.8 times on pages, and the gap
  grows with the distance to the database.
- **Where an operation is one round trip, the network evens everyone out.** A get by key, an
  insert and a save land within about 10% of each other through the proxy, keel among them.
- **sqlc decodes faster.** Its generated scan code beats keel's on plain lists, by 12 to 16% on
  the same host and 3 to 13% through the proxy, and it is the fastest single-row read on the same
  host. keel is second on every list, ahead of raw pgx's generic row mapping and every ORM.
- **The ORMs pay in allocations.** GORM, ent, Bob and Jet allocate two to four times what keel
  does on lists and relation loads, which shows on the same host: Jet's list of 1,000 takes 2.5
  times keel's time and GORM's 2.1 times.
- **Bun builds statements fastest**, about 15% ahead of keel; GORM takes 1.8 times keel's time,
  Bob 2.5 to 2.9 times and Jet 4.6 to 5.2 times.
- **Bulk inserts:** keel's `CreateMany` and `Copy` and sqlc's `COPY` lead; multi-row `INSERT`s,
  with one parameter per value, trail them by 20 to 70%.

### Rerunning

```sh
cd bench && KEEL_TEST_DSN=postgres://keel:keel@localhost:55432/keel?sslmode=disable \
    go test -run xxx -bench . -benchmem -count 6 -timeout 3h > loopback.txt
```

For the latency run, start [Toxiproxy](https://github.com/Shopify/toxiproxy) in front of the
server and point `KEEL_TEST_DSN` at it. It runs as a process of its own, so its work does not
reach the benchmark's allocation counts:

```sh
go install github.com/Shopify/toxiproxy/v2/cmd/server@latest && server &
curl -X POST localhost:8474/proxies -d '{"name":"pg","listen":"127.0.0.1:55435","upstream":"127.0.0.1:55432"}'
curl -X POST localhost:8474/proxies/pg/toxics -d '{"type":"latency","stream":"downstream","attributes":{"latency":1}}'
cd bench && KEEL_TEST_DSN=postgres://keel:keel@127.0.0.1:55435/keel?sslmode=disable \
    go test -run xxx -bench . -benchmem -count 6 -timeout 4h > latency.txt
```

After a schema change, regenerate each library's code: `sqlc generate` in `bench/sqlc`,
`go generate ./ent` in `bench`, `bobgen-psql -c bobgen.yaml` in `bench/bob`, and Jet's
`jet -dsn=… -tables=customers,shipments,moves -path=./jetgen` in `bench`, then move
`jetgen/<database>/public` to `bench/jet`.

## 2026-10-26

The performance work since 2026-10-23 (pipelined relation trees, the version-checked result cache,
read transactions that end without waiting for `COMMIT`, estimated totals), measured the only way
this VM allows: keel's previous `master` and this branch alternating, two runs each, three rounds,
so the host's noise falls on both. Raw output:
[bench/results/2026-10-26-ab-master.txt](../bench/results/2026-10-26-ab-master.txt) and
[bench/results/2026-10-26-ab-branch.txt](../bench/results/2026-10-26-ab-branch.txt); the full
suite on the branch is [bench/results/2026-10-26.txt](../bench/results/2026-10-26.txt).

| Operation | before | after | change |
|---|---|---|---|
| 50 rows with a to-one and a to-many relation | 1.82 ms, 2,153 allocs | 1.56 ms, 2,002 allocs | 14% faster, 7% fewer allocs |
| the same inside a snapshot (`OnBegin` read transaction) | two round trips | 1.58 ms, one round trip | the load rides the read's round trip |
| a cached read, served from the in-process memory cache | 391 µs (the database read) | 20.6 µs | 19× faster |
| list 100 rows | 1.11 ms, 2,536 allocs | 1.12 ms, 2,437 allocs | even, 4% fewer allocs |
| get one row by key | 408 µs | 387 µs | even |
| read in a transaction with a `set_config` | 416 µs | 414 µs | even here; see below |
| build a statement, no database | 5.4 µs | 5.5 µs | even |
| dynamic report and 1,000-row dynamic read | 5.2 ms, 1.65 ms | 4.6 ms, 1.65 ms | even |

Everything else is within noise: no operation got slower. What the table cannot show on a
loopback connection:

- **Read transactions end without waiting for `COMMIT`.** A scoped read used to wait for the
  pipelined `COMMIT`'s reply before returning; it now returns when the rows arrive and the
  connection finishes the commit before its next use. On loopback the reply is microseconds; across
  a network it is one round trip off every read transaction, which is most of a primary-key read's
  latency.
- **Pipelined relation trees** save a round trip per level beyond the first when the read sees one
  snapshot (`Config.ReadIsolation = keel.RepeatableRead`, or an `OnBegin` read transaction); the
  14% here is the saved round trip on loopback plus the per-level decoding the pipeline removed.
- **Estimated totals** replace `count(*)` over every matching row by a count capped at the limit
  and the planner's estimate, both in the page's round trip; on the 2,000-row bench table the two
  cost the same, and the gap grows with the table (a full count reads every row).
- **The result cache's hit** costs a version check against an in-memory map pushed by
  `LISTEN/NOTIFY` and a decode of the stored rows; it never reads the database, so its 20 µs is the
  same on a remote server, where the miss costs a network round trip more.

Trenova's row-level security policy verified the signed tenant scope twice per table reference
(`trenova_rls.org_id()` and `trenova_rls.bu_id()`, two InitPlans each calling a plpgsql HMAC).
Comparing the two against one InitPlan reading both claims (`trenova_rls.tenant()`), on
PostgreSQL 18 with pgbench, the policy's cost per statement fell from about 117 µs to about 77 µs
on one table and from about 255 µs to about 142 µs on a two-table join, 35 to 45% less, on every
tenant read Trenova sends.

## 2026-10-23

Medians of 6 runs on a 4-vCPU Xeon VM, Postgres 18 in Docker on the same host. Raw output:
[bench/results/2026-10-23.txt](../bench/results/2026-10-23.txt).

| Operation | keel | Bun | pgx | keel vs Bun |
|---|---|---|---|---|
| get one row by key | 328 µs, 94 allocs | 441 µs, 56 allocs | 257 µs, 47 allocs | 26% faster |
| list 100 rows | 812 µs, 2,953 allocs | 1,253 µs, 3,623 allocs | 1,177 µs, 2,908 allocs | 35% faster |
| list 1,000 rows | 3.95 ms, 29,050 allocs | 5.50 ms, 36,770 allocs | 4.54 ms, 29,010 allocs | 28% faster |
| 50 rows with a to-one and a to-many relation | 1.52 ms, 3,377 allocs | 3.34 ms, 6,735 allocs | — | 55% faster |
| read in a transaction with a `set_config` (row-level security) | 326 µs, 127 allocs | 1,077 µs, 73 allocs | — | 70% faster |
| insert one row | 901 µs, 45 allocs | 911 µs, 16 allocs | — | even |
| insert 1,000 rows | 19.6 ms, 3.3 MiB | 28.0 ms, 0.7 MiB | — | 30% faster, 4.6× the memory |
| save with optimistic locking | 1.02 ms, 44 allocs | 1.20 ms, 29 allocs | — | 15% faster |
| build a statement, no database | 3.8 µs, 2.7 KiB | 3.3 µs, 3.0 KiB | — | 13% slower |

Where the time goes:

- **Prepared statements.** keel binds values, so each statement shape is prepared once per
  connection and Postgres skips parsing and, usually, planning. On this server an unnamed
  primary-key read costs ~720 µs; prepared with `plan_cache_mode = auto` it costs ~265 µs, and with
  `force_custom_plan` ~450 µs. Bun writes values into the text, so it can never reuse a plan. The
  pgx column uses pgx's own statement cache and `RowToStructByPos`, which decodes through
  reflection; keel decodes through generated accessors.
- **Round trips.** The scoped read is one round trip in keel (`BEGIN`, `set_config`, the query and
  `COMMIT` pipelined) and four in Bun. This server is on the same host, so a round trip costs tens of
  microseconds; across a network the gap grows by roughly 3 × the round-trip time per read.
- **Relation loads.** keel joins the to-one relation into the main statement and loads the to-many
  relation with one `= ANY($1)` statement; Bun runs a statement per relation and decodes through
  reflection.

Known costs:

- **Allocations per statement** are higher than Bun's on single-row operations (94 against 56 on a
  read): closures for predicates, the runner, the decoding plan and pgx's batch. They cost far less
  than the server work prepared statements save, and pooled renders already removed the render's
  own allocations.
- **Bulk inserts** box every value into an `any` and the test schema's `numeric` column is encoded
  by parsing a decimal string; Trenova should register a binary decimal codec. A columnar insert
  through `unnest` of per-column arrays would remove the boxing and keep one prepared statement for
  any row count.
