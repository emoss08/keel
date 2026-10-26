# Benchmarks

`bench/` runs the same operations through keel, Bun (Trenova's fork at `2580fd2`) and raw pgx on one
schema and data set (2,000 shipments, 600 moves, 10 customers in one tenant).

```sh
cd bench && go test -run xxx -bench . -benchmem -count 6 > new.txt
go run golang.org/x/perf/cmd/benchstat@latest -col /impl new.txt
```

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
