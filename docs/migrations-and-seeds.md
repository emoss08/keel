# Migrations and seeds

Status: design, ready to build. This document describes the schema and data tooling keel ships, built so
an application's server binary carries none of it, and how Trenova moves onto it.

## The problem, measured in Trenova

- The production image builds one binary, `./cmd/cli`, and runs it as `trenova api`, `trenova worker` and
  `trenova db migrate` (deploy/Dockerfile.tms). `go list -deps ./cmd/cli` shows it links
  `postgres/migrations` (1,375 files, 6.9 MB of SQL embedded verbatim), `database/migrator`,
  `database/seeder`, `seeds/base`, `seeds/development`, `volumeseed` and `pkg/seedhelpers`. The server
  that answers requests carries every migration ever written and every development fixture.
- The image also copies `seeds/base/data` (1 MB of YAML) and `seeds/development/data` into `/app`, because
  seeds read their data files from disk at run time.
- Seeds re-run through three mechanisms: a version string bumped by hand, `Repeatable()` (three seeds
  re-run on every invocation because their data comes from code registries), and `--force`. A forgotten
  version bump ships stale reference data; a repeatable seed costs its full work on every deploy.
- Seed rollback is a subsystem of its own (`seed_rollbacks`, a persistent tracker of every created entity,
  `Down` on every seed, `seed-check`, `seed-clean`, `seed-sync`), used only to undo development data that a
  database reset removes faster.
- Every tenant-table migration must remember to end with `SELECT trenova_rls.reconcile();`.
- Test databases come from a template keyed by a fingerprint of the embedded migrations, built by
  Trenova's own harness (`testutil/seedtest`).

## Goals

1. The server binary links no migration, no seed and no seed data. It knows the schema version it needs,
   as one generated constant, and refuses to serve a database that is behind it.
2. One small database binary per application applies migrations and the reference data production needs,
   as a deploy step (Compose service, Kubernetes Job, Helm pre-upgrade hook).
3. Development fixtures live in a binary no image ever builds.
4. Reference data re-applies exactly when what it is made from changes: its files, its code, its
   version. Never on a schedule, never by hand.
5. Migrations are safe on a busy database: they never queue an exclusive lock behind a long query and
   stall every request, and a merge that reorders migrations fails CI instead of production.
6. A development or test database is ready in under a second, from a template.
7. An application wires all of it in about thirty lines.

## Shape

Three binaries, built from the application's code:

| Binary | Links | Ships | Runs |
|---|---|---|---|
| server (`trenova api`, `trenova worker`) | the app, `schema.Version` (one constant) | yes, image `trenova` | always |
| `trenova-db` (`cmd/db`) | migrations, reference seeds, their data | yes, image `trenova-db` | before each rollout |
| `trenova-devdb` (`cmd/devdb`) | everything `cmd/db` links, fixtures, volume data | never | by developers and CI |

keel provides the three building blocks: `keel/migrate` (migrations), `keel/seed` (seeds) and
`keel/keelcli` (the command line both database binaries are made of). The generic `keel` command keeps
`gen` and gains every `migrate` command for applications that have no seeds.

```
services/tms/
  schema/
    migrations/                 *.sql, embedded only by cmd/db
    version_gen.go              generated: Version, Fingerprint
  cmd/
    cli/                        api, worker and ops commands; no migrations, no seeds
    db/main.go                  keelcli.Main: migrations + reference seeds
    devdb/main.go               keelcli.Main: + fixtures + volume
  internal/seeds/
    reference/                  production data: states, hazmat, IFTA, roles, permission sync, …
    fixtures/                   development data: test organizations, workers, shipments, …
```

## keel/migrate

### Files

A migration is one file, `<version>_<name>.sql`, with the version a UTC timestamp (`20261023143000`).
Directives at the top of the file replace file-name suffixes:

```sql
--keel:notx
--keel:lock_timeout 2s
CREATE INDEX CONCURRENTLY shipments_status_idx ON shipments (organization_id, status);
--keel:split
CREATE INDEX CONCURRENTLY stops_move_idx ON stops (move_id);
```

| Directive | Effect |
|---|---|
| `--keel:notx` | runs outside a transaction, statement by statement, split at `--keel:split` lines |
| `--keel:lock_timeout <d>` | overrides the default lock timeout for this migration |
| `--keel:statement_timeout <d>` | sets a statement timeout (none by default) |
| `--keel:no_retry` | does not retry on lock timeout |

There are no down migrations. Production only moves forward. A bad migration is fixed by the next one,
and a development database is rebuilt from its template in under a second, which is faster and more
reliable than a down file nobody runs until the day it is wrong. Trenova's 690 down files are deleted.

### Sources

An application's migrations can come from several file systems, merged into one version line:

```go
migrate.Sources(
    migrate.Source{Name: "core", FS: schema.Migrations},
    migrate.Source{Name: "cloud", FS: cloudschema.Migrations},
)
```

A version that appears in two sources is an error. This replaces Trenova's edition merging
(`migrator.PostgresMigrations`, `edition.PostgresMigrations`).

### Applying

`Up` takes the migration advisory lock (polling `pg_try_advisory_lock`, as today), then applies each
pending migration in its own transaction together with its bookkeeping row. Each transactional migration
runs with `SET LOCAL lock_timeout = '5s'` unless it says otherwise. When it fails with `lock_not_available`
(55P03) it is retried with backoff, up to five times. A migration that needs an `ACCESS EXCLUSIVE` lock
therefore never waits behind a long report query while every request queues behind it, which is the
classic way a migration takes an application down.

`Options.After` lists statements run once, in the last migration's transaction, after any run that
applied something:

```go
migrate.Options{After: []string{"SELECT trenova_rls.reconcile()"}}
```

Trenova's rule that every tenant-table migration ends with `reconcile()` becomes automatic, so it can't
be forgotten.

**Ordering.** A pending migration whose version is lower than the newest applied one is an error, unless
`Options.AllowOutOfOrder` is set. That catches the branch that merged with an older timestamp, before
production does. `keel migrate check` reports it in CI against the target database's history, or against
the newest version on the base branch.

**Bookkeeping.** `keel_migrations` stores `version`, `name`, `source`, `checksum`, `applied_at` and
`duration_ms`. A changed applied file is `ErrChanged`, as today.

### Everything else a migrator needs

| Feature | How |
|---|---|
| Plan and dry run | `Plan` returns what `Up` would do (pending, baseline marks, After statements); `up -dry-run` prints the SQL |
| Target | `up -to <version>` stops after that version |
| Repeatable migrations | `R_<name>.sql` files (functions, views, RLS policy helpers, grants) re-run after the versioned ones whenever their checksum changes, in name order |
| Go migrations | `migrate.Func{Version, Name, Up func(ctx, pgx.Tx) error}` for data changes SQL cannot express, ordered with the SQL files |
| Progress events | `Options.Observer` receives lock wait, lock acquired, migration start, statement (notx), retry, migration end, After, run end; the CLI and TUI render from them |
| Who is blocking | on lock waits and 55P03 retries the event names the blocking backends from `pg_locks`/`pg_stat_activity`: pid, application, state, age, query |
| Baseline an existing database | `migrate baseline <version>` records every migration up to version as applied without running it |
| Repair | `migrate repair <version>` accepts the current checksum of an applied file after a comment-only edit, with the old and new checksums in the output |
| History | `migrate history` lists applied migrations with source, duration and time |
| Bookkeeping schema | `Options.Schema` puts `keel_migrations` in a schema other than `public` |
| Machine output | every command takes `-json` |

### Squash

`keel migrate squash` applies every migration to a scratch database, dumps the schema with
`pg_dump --schema-only --no-owner --no-privileges`, and replaces the migrations up to and including the
newest with one baseline file, `<version>_baseline.sql`, marked `--keel:baseline`. On a database whose
history already contains every squashed version, the baseline is recorded as applied without running.
A fresh database runs it as one file.

Trenova has not launched, so its 1,375 migrations become one baseline now. Fresh databases, CI and test
templates get faster, and `schema/migrations` starts clean. Later, squash runs whenever history grows
long, up to the oldest version every live database has applied.

### The server's view: Version and Require

`keel migrate stamp` writes a tiny generated file:

```go
// Code generated by keel migrate stamp. DO NOT EDIT.
package schema

const (
    Version     = "20261023143000"
    Fingerprint = "9f2c41d0…"
)
```

The server links this file and nothing else from the schema. At startup:

```go
if err := migrate.Require(ctx, pool, schema.Version); err != nil {
    return err // migrate.ErrBehind: the database has not applied Version yet
}
```

`Require` reads one row from `keel_migrations`. A database ahead of the server passes, which is what a
rolling deploy needs (expand the schema, then roll the servers). `RequireWait` polls until the database
catches up, for a server that starts alongside its database job. `keel migrate stamp -check` fails CI when
the stamp does not match the migrations, like `keel gen -check`.

### Templates

```go
dsn, err := migrate.Template(ctx, migrate.TemplateOptions{
    Admin:   adminDSN,
    Sources: sources,
    Prepare: func(ctx context.Context, dsn string) error { return seeds.Run(ctx, dsn, …) },
})
```

`Template` builds `keel_tpl_<fingerprint>` once, under an advisory lock so parallel test packages build it
once between them. The fingerprint covers the migrations and, when `Prepare` is set, a key the caller
supplies (the seed fingerprints). Each call then returns a fresh database made with
`CREATE DATABASE … TEMPLATE keel_tpl_… STRATEGY FILE_COPY`; on Postgres 18 with
`file_copy_method = clone` on a copy-on-write file system, that is a reflink copy in milliseconds.
`keel migrate templates prune` drops templates whose fingerprint is no longer current. This replaces
Trenova's `seedtest` template harness and makes `db reset` instant.

### Lint

`keel migrate lint` checks pending migrations with built-in rules, and the application can add its own:

| Rule | Flags |
|---|---|
| `index-concurrently` | `CREATE INDEX` without `CONCURRENTLY` on a table that existed before the migration |
| `not-null-default` | `ADD COLUMN … NOT NULL` without a default on an existing table |
| `notx-mixed` | `CONCURRENTLY` or `ALTER TYPE … ADD VALUE` in a transactional migration |
| `lock-heavy` | `ALTER TABLE … SET DATA TYPE`, `VALIDATE` without a prior `NOT VALID` |

```go
migrate.LintRule{Name: "tenant-rls", Check: func(m migrate.Migration) []migrate.Finding { … }}
```

## keel/seed

### Two kinds of seed

| Kind | What | Where it runs | Re-runs when |
|---|---|---|---|
| `seed.Reference` | data production needs: states, hazmat references, IFTA jurisdictions, roles, permission sync, document template starters, system agent definitions | `cmd/db` on every deploy, `cmd/devdb`, tests | its fingerprint changes |
| `seed.Fixture` | development data: test organizations, workers, shipments, … | `cmd/devdb`, tests | never: reset the database |

A database records its environment in `keel_meta` the first time `cmd/db` runs (`-env production`).
Fixtures refuse to run against a database marked production, whatever binary runs them.

### Declaring a seed

```go
package reference

//go:embed data
var data embed.FS

var Seeds = seed.NewRegistry()

var USStates = Seeds.Add(seed.Seed{
    Name:   "us_states",
    Kind:   seed.Reference,
    Inputs: seed.Files(data, "data/us_states.yaml"),
    Run: func(ctx context.Context, r *seed.Run) error {
        states, err := seed.YAML[[]statedb.Row](r, "data/us_states.yaml")
        if err != nil {
            return err
        }
        _, err = statedb.States.Upsert(ctx, r.DB, states, keel.OnConflict(statedb.States.Abbreviation))
        return err
    },
})

var PermissionSync = Seeds.Add(seed.Seed{
    Name:        "organization_role_permissions",
    Kind:        seed.Reference,
    After:       []*seed.Seed{OrganizationRoles},
    PerTenant:   true,
    Fingerprint: permission.Registry.Fingerprint,
    Run: func(ctx context.Context, r *seed.Run) error {
        return rolesync.Sync(ctx, r.DB) // ctx carries r.Tenant's scope
    },
})
```

| Field | Meaning |
|---|---|
| `Name` | stable identity, the key in `keel_seeds` |
| `Kind` | `seed.Reference` or `seed.Fixture` |
| `After` | seeds that must have run first; the registry rejects cycles and unknown seeds when built |
| `Inputs` | embedded files hashed into the fingerprint and readable through `seed.YAML`, `seed.JSON`, `seed.CSV` |
| `Fingerprint` | a function whose result joins the fingerprint, for data built from code (registries, starters) |
| `Version` | a string that joins the fingerprint, for a change to `Run` itself |
| `PerTenant` | `Run` runs once per tenant, with that tenant's scope on the context |
| `Run` | the work: idempotent upserts for reference data, plain creates for fixtures |

### Running

```go
report, err := reference.Seeds.Apply(ctx, db, seed.Options{
    Tenants:     tenants.All, // func(ctx, *keel.DB) iter.Seq2[seed.Tenant, error]
    Parallel:    8,
    Environment: seed.Production,
})
```

- **Plan.** Every seed's fingerprint is `sha256(name, version, input files, Fingerprint())`. A reference
  seed runs when its fingerprint differs from the one stored in `keel_seeds`; a fixture runs when it has
  no row. `Repeatable` and hand-bumped versions are gone, and an unchanged seed costs one hash and one
  read.
- **Order.** Seeds run in `After` order. Independent seeds at the same depth run concurrently, up to
  `Parallel`.
- **Atomicity.** A seed runs in one `keel.Write` with a system scope, and its `keel_seeds` row is written
  in the same transaction, so it either applies and is recorded, or leaves no trace.
- **Tenants.** A `PerTenant` seed runs one transaction per tenant, with the tenant's scope on the context
  so keel fills tenant columns and row-level security applies. Up to `Parallel` tenants run at once, and
  progress is recorded per tenant in `keel_seed_tenants`, so a run that fails at tenant 4,000 of 9,000
  resumes there. New tenants get their data from the application's own tenant bootstrap when they are
  created; the per-tenant seed reconciles existing ones when its fingerprint changes.
- **Locking.** A run holds the seed advisory lock, so two deploys never seed at once.
- **Data.** Reference data is embedded in `cmd/db`, so the Dockerfile copies no data directory. Large
  sets read as CSV stream straight into `Copy`.
- **Values between seeds.** Typed keys replace `SeedContext.Set`/`Get` and their `any` values:

  ```go
  var DevOrg = seed.Key[*tenant.Organization]("dev-org")

  seed.Put(r, DevOrg, org)         // in TestOrganizations
  org := seed.Must(r, DevOrg)      // in a seed that runs After it
  ```

- **Reporting.** `seed.Options.Observer` receives each seed's start, end, rows and duration; the CLI
  prints them as a table.

### Everything else a seeder needs

| Feature | How |
|---|---|
| Plan and dry run | `Plan` returns each seed with why it will run (new, input changed, version changed, code fingerprint changed, forced) or why not |
| Change explanation | `keel_seeds` stores per-input hashes, so status names the file or fingerprint that changed |
| Deterministic data | `r.Rand()` is a PCG generator seeded from the seed's name and fingerprint: fixtures and volume data are the same on every machine |
| Timeouts | `Seed.Timeout` bounds one seed (per tenant for PerTenant seeds) |
| Progress events | `Options.Observer` receives plan, lock wait, seed start, tenant progress (n of N), rows, seed end, run end |
| Rows | `r.Count(n)` adds to the rows the report shows; `Copy`, `CreateMany` and `Upsert` results feed it |
| Machine output | every command takes `-json` |

There is no rollback. Reference data is idempotent and forward-only, like migrations; fixtures go away
with a database reset, which takes under a second from a template. That deletes Trenova's rollback
tracker, persistent entity tracker, `seed_rollbacks`, `Down` methods and the `seed-check`, `seed-clean`,
`seed-sync` and `seed-rollback` commands. `keel_seeds` rows of seeds no longer registered show in
`seed status`, and `seed prune` deletes them.

## keel/keelcli

`keelcli` turns an application's migrations and seeds into a command line. Every command works two ways:
plain, scriptable output when stdout is not a terminal or `-json`/`-plain` is given (CI, Kubernetes
Jobs, Compose), and an interactive interface built on Bubble Tea v2, Bubbles, Lip Gloss and Huh when it
is a terminal. The TUI dependencies live only in `keelcli`; a server that imports `keel` or
`keel/migrate` never links them.

```go
// services/tms/cmd/db/main.go
func main() {
    keelcli.Main(keelcli.App{
        Name:       "trenova-db",
        Config:     dbconfig.FromEnv, // func() (keel.Config, error)
        Migrations: migrate.Sources(migrate.Source{Name: "core", FS: schema.Migrations}),
        Migrate:    migrate.Options{After: []string{"SELECT trenova_rls.reconcile()"}},
        Seeds:      reference.Seeds,
        Tenants:    tenants.All,
    })
}
```

```go
// services/tms/cmd/devdb/main.go
func main() {
    keelcli.Main(keelcli.App{
        Name:       "trenova-devdb",
        Config:     dbconfig.FromEnv,
        Migrations: migrate.Sources(migrate.Source{Name: "core", FS: schema.Migrations}),
        Migrate:    migrate.Options{After: []string{"SELECT trenova_rls.reconcile()"}},
        Seeds:      seed.Merge(reference.Seeds, fixtures.Seeds),
        Tenants:    tenants.All,
        Commands:   []keelcli.Command{volume.Command},
    })
}
```

### Commands

| Command | Does |
|---|---|
| (none, in a terminal) | opens the console, below |
| `up [-to v] [-dry-run]` | migrate, then apply seeds: the one deploy step |
| `migrate up / plan / status / history / create / squash / stamp / check / lint / baseline / repair` | migrations |
| `seed apply [names…] / plan / status / prune` | seeds |
| `reset` | development only: recreate the database from the current template, building it first when needed |
| `template [build / prune]` | templates |
| `doctor` | connectivity, server version, `plan_cache_mode`, extensions, lock holders, template state, environment mark |

Destructive commands (`reset`, `prune`, `repair`, `baseline`, `squash`) confirm with a Huh prompt that
names the database and its environment mark, and refuse against a production-marked database unless
`-yes-production` is given. `create` without a name asks for one and for transactional or notx.

### Live output

In a terminal, `up`, `migrate up` and `seed apply` render a live view instead of log lines: one row per
migration or seed with a spinner, elapsed time, retries and rows, a progress bar for per-tenant seeds
(tenant 4,213 of 9,000), and a panel that appears while waiting on a lock showing the blocking backend's
pid, application, age and query. The run ends with a summary table. Without a terminal the same events
print as one line each, or as JSON lines with `-json`.

### The console

`trenova-devdb` (or any keelcli binary) with no arguments in a terminal opens a full-screen console:

```
┌ trenova-devdb ─ postgres://localhost:5432/trenova ─ development ─ v20261023143000 ───────────────┐
│ Migrations  Seeds  Templates  Lint  Doctor                                                       │
├──────────────────────────────────────────────────────────────────────────────────────────────────┤
│ ✓ 20261023143000 baseline              core   applied 2026-10-23 14:31   1.8s                    │
│ ✓ 20261024091500 shipment_holds        core   applied 2026-10-24 09:16   42ms                    │
│ • 20261025110000 edi_partner_index     core   pending  notx                                      │
│ ! 20261025120000 rate_tables           core   changed  checksum differs                          │
├──────────────────────────────────────────────────────────────────────────────────────────────────┤
│ --keel:notx                                                                                      │
│ CREATE INDEX CONCURRENTLY edi_partner_idx ON edi_partners (organization_id, scac);               │
├──────────────────────────────────────────────────────────────────────────────────────────────────┤
│ u apply  p plan  c create  / filter  enter view SQL  r refresh  ? help  q quit                   │
└──────────────────────────────────────────────────────────────────────────────────────────────────┘
```

- **Migrations**: every migration with its state (applied, pending, changed, repeatable stale), source,
  directives and duration; a preview pane with the SQL highlighted; apply all or up to the selected one
  with the live view; create a migration from a form.
- **Seeds**: the seed graph as an indented tree in dependency order, each seed's kind, state and the
  reason it would run; per-tenant progress; apply the selection (with its dependencies) or force it.
- **Templates**: current and stale templates with size and age; build, prune, reset the database from a
  template.
- **Lint**: findings for pending migrations, grouped by rule, jumping to the line in the preview.
- **Doctor**: the doctor checks, refreshed live, with the current lock holders and long-running
  queries.

The console uses the same engine calls as the commands; it adds no behaviour of its own, so everything
it does is also scriptable.

## Deploying

Compose (selfhost), replacing the `until trenova db migrate` loop:

```yaml
trenova-db:
  image: ghcr.io/emoss08/trenova-db:${TRENOVA_VERSION}
  command: ["up", "-env", "production"]
  restart: on-failure
api:
  image: ghcr.io/emoss08/trenova:${TRENOVA_VERSION}
  depends_on:
    trenova-db: { condition: service_completed_successfully }
```

Kubernetes: `trenova-db up` as a Job (a Helm pre-upgrade hook), with servers using `RequireWait` so a pod
that starts first waits instead of crash-looping.

## Moving Trenova onto it

One change, as with the rest of keel:

1. Squash `postgres/migrations` into `schema/migrations/<version>_baseline.sql`; delete every down file;
   generate `schema/version_gen.go`.
2. Split `seeds/base` into `internal/seeds/reference` (states, hazmat, IFTA, roles, permission sync,
   template starters, system agent definitions, GL accounts and system accounts as per-tenant
   reconcilers over `tenantbootstrap`) and `seeds/development` into `internal/seeds/fixtures`. Convert
   each to a `seed.Seed`, replacing `SeedContext` lookups with typed keys and `bun.Tx` with keel.
3. Add `cmd/db` and `cmd/devdb`; remove `db migrate`, `db seed*`, `db create-seed` and `db bootstrap`
   from `cmd/cli`, which keeps its ops commands (repairs, encryption, redis, desk).
4. Delete `database/migrator`, `database/seeder`, `seeds/register_gen.go` and its generator,
   `pkg/seedhelpers`, `postgres/migrations` and `testutil/seedtest`'s template code, which becomes
   `migrate.Template`.
5. Dockerfile: build `cmd/cli` into `trenova` without the data `COPY` lines; add a `trenova-db` target.
   Selfhost compose gains the `trenova-db` service.
6. Taskfile: `db-migrate`, `db-seed`, `db-reset`, `db-seed-volume` call `trenova-devdb`; the `db-seed-*`
   housekeeping tasks go.
7. Server startup calls `migrate.RequireWait(ctx, pool, schema.Version)`.
8. Docs: `generated-artifacts.md` and the backend migration pattern file describe the new layout, the
   automatic `reconcile()` and the stamp check; `AGENTS.md`'s commands table points at the new tasks.

## Decisions and trade-offs

- **Separate binaries, not build tags.** A build tag keeps one main package but every tool (gopls, lint,
  tests) must run twice to see both builds, and a missing tag silently ships the wrong binary. Separate
  `main` packages are explicit, and `go list -deps ./cmd/cli` proves the server is clean.
- **No down migrations.** They double the files, are rarely tested, and are never run in production.
  Templates make development resets faster than a down run. The cost is losing "undo the last migration"
  during development; `reset` replaces it.
- **No seed rollback.** Same reasoning: forward-only reference data, disposable fixtures.
- **Fingerprints, not versions.** Re-running when inputs change removes both failure modes: stale data
  from a forgotten bump, and wasted work from always-on seeds. The cost is that a change inside `Run`
  that changes its output needs `Version` bumped; code-derived data declares its `Fingerprint` so that
  case is rare.
- **pg_dump for squash.** Writing a schema dumper is a project of its own and would be wrong in corner
  cases pg_dump already handles. `squash` is a development command, so needing `pg_dump` (or the
  Postgres container) there is acceptable.
- **The server checks the version, it does not migrate.** Migrating from the server races between
  replicas and couples request serving to DDL. One job per deploy, with the server waiting on a version
  check, keeps both simple.
