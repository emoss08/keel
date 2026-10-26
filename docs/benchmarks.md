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
- Every library decodes the same thing into the same shape: `jsonb` into a map through sonic
  (keel through `Config.JSON`, pgx and sqlc through pgx's `jsonb` codec, the rest through a
  shared `sql.Scanner`, and Jet, whose generator types the column as text, right after its scan),
  `numeric` into a string or, for keel, a binary-decoded money value.
- ent keys an entity by one column, so its models use `id` alone where the tables have a
  composite primary key. sqlc, sqlx and raw pgx build no statements, so they have no
  statement-building number, and ent cannot render a typed query without running it.
- The generated code of ent, sqlc, Bob and Jet is checked in under `bench/` with its schema,
  queries and generator configuration.

### Through 1 ms of added latency

A round trip took 1.73 ms through the proxy against 0.23 ms direct. Fastest in bold.

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

### With Postgres on the same host

| Operation | keel | sqlc | sqlx | pgx | Bob | ent | GORM | Jet | Bun |
|---|---|---|---|---|---|---|---|---|---|
| get one row by key | **345 µs** | 345 µs | 346 µs | 349 µs | 402 µs | 424 µs | 397 µs | 412 µs | 510 µs |
| list 100 rows | **742 µs** | 1.04 ms | 1.24 ms | 969 µs | 1.01 ms | 1.13 ms | 1.61 ms | 1.79 ms | 1.28 ms |
| list 1,000 rows | **4.09 ms** | 5.15 ms | 6.26 ms | 4.75 ms | 5.84 ms | 5.90 ms | 10.00 ms | 12.73 ms | 6.47 ms |
| 50 rows with a to-one and a to-many relation | **1.35 ms** | 2.26 ms | 2.52 ms | – | 3.90 ms | 2.55 ms | 4.36 ms | 5.64 ms | 3.94 ms |
| read in a transaction with a `set_config` | **408 µs** | 965 µs | 1.10 ms | – | 1.15 ms | 1.26 ms | 1.17 ms | 1.33 ms | 1.36 ms |
| keyset page of 50 with its total | **1.11 ms** | 1.43 ms | 1.40 ms | 1.29 ms | 1.49 ms | 1.83 ms | 1.68 ms | 1.82 ms | 1.63 ms |
| insert one row | 564 µs | 571 µs | 577 µs | – | 627 µs | **562 µs** | 564 µs | 593 µs | 592 µs |
| insert 1,000 rows | 17.63 ms (18.30 ms with `Copy`) | **17.34 ms** | 18.90 ms | – | 24.22 ms | 21.71 ms | 21.94 ms | 26.36 ms | 28.98 ms |
| save with optimistic locking | 693 µs | 692 µs | 798 µs | – | 744 µs | 736 µs | **649 µs** | 718 µs | 745 µs |
| build a statement, no database | 4.6 µs | – | – | – | 13.4 µs | – | 8.9 µs | 23.4 µs | **4.1 µs** |

### Allocations

Allocations per operation with Postgres on the same host, bytes in parentheses; fewest in bold.

| Operation | keel | sqlc | sqlx | pgx | Bob | ent | GORM | Jet | Bun |
|---|---|---|---|---|---|---|---|---|---|
| get one row by key | 61 (5.4 KB) | 40 (3.2 KB) | 93 (5.1 KB) | **39 (3.2 KB)** | 157 (13 KB) | 289 (13 KB) | 177 (11 KB) | 465 (29 KB) | 55 (7.6 KB) |
| list 1,000 rows | **14,051 (912 KB)** | 22,030 (1,967 KB) | 36,814 (1,873 KB) | 19,025 (1,133 KB) | 33,885 (1,189 KB) | 52,008 (1,790 KB) | 51,891 (2,268 KB) | 73,171 (4,645 KB) | 33,784 (1,194 KB) |
| 50 rows with a to-one and a to-many relation | **1,512 (104 KB)** | 2,656 (237 KB) | 4,284 (230 KB) | – | 5,897 (252 KB) | 6,881 (255 KB) | 8,396 (416 KB) | 22,024 (1,303 KB) | 6,596 (202 KB) |
| keyset page of 50 with its total | **835 (55 KB)** | 1,135 (93 KB) | 1,931 (91 KB) | 985 (59 KB) | 1,969 (79 KB) | 3,026 (110 KB) | 2,782 (117 KB) | 4,170 (256 KB) | 1,761 (68 KB) |

### What it shows

- **Round trips decide most of it, and keel saves them.** keel sends a relation tree and its root
  in one round trip; every other library sends one per level, so 50 shipments with their customer
  and moves take keel one round trip and the others three. A scoped read is `BEGIN`, the
  `set_config`, the read and `COMMIT` pipelined in one round trip, without waiting for the
  `COMMIT`'s reply; every other library makes four. A page and its count share a round trip.
  Through 1 ms of latency that makes keel 2.1 to 2.9 times as fast as every other library on
  relation loads, 3.5 to 3.7 times on scoped reads and 1.6 to 1.9 times on pages, and the gap
  grows with the distance to the database.
- **keel decodes rows fastest.** It is the fastest list on the same host and through the proxy:
  the next fastest list of 100, raw pgx's, takes 1.3 times keel's time on the same host, and
  sqlc's 1.4 times. keel allocates 14 times a row on a list of 1,000, raw pgx 19 and sqlc 22.
- **Decoding is the work, not the generated code.** An earlier version of this run left sqlc's
  `jsonb` as bytes and decoded keel's `numeric` through its text form; sqlc's list of 1,000 then
  took 3.93 ms on the same host against keel's 4.66 ms. Decoding the same map costs sqlc 1.2 ms,
  and reading `numeric` in binary and `jsonb` through sonic saves keel 0.6 ms.
- **Where an operation is one round trip, the network evens everyone out.** A get by key, an
  insert and a save land within 12% of each other through the proxy, and a get by key is a
  tie on the same host, although keel's tenant scope and statement building cost it 61
  allocations against sqlc's and raw pgx's 40.
- **The ORMs pay in allocations.** Bob, Bun, ent, GORM and Jet allocate 2.4 to 5.2 times what
  keel does on a list of 1,000 and 3.9 to 15 times on a relation load, which shows on the same
  host: GORM's list of 1,000 takes 2.4 times keel's time and Jet's 3.1 times.
- **Bun builds statements fastest**, about 13% ahead of keel; GORM takes 1.9 to 2.0 times keel's
  time, Bob 2.9 to 3.1 times and Jet 5.1 to 5.4 times.
- **Bulk inserts:** keel's `CreateMany` and `Copy` and sqlc's `COPY` lead, within about 6%
  of each other; multi-row `INSERT`s, with one parameter per value, trail them by 9 to 67%.

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
