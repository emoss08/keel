# keelcli

`keelcli` turns an application's migrations and seeds into the command line its database binaries
are made of: `trenova-db`, the deploy job that migrates and applies reference data, and
`trenova-devdb`, the developer tool that adds fixtures. It is the third building block of
[migrations and seeds](../docs/migrations-and-seeds.md), next to `keel/migrate` and `keel/seed`.

Every command works two ways:

- **In a terminal** it is interactive: runs draw a live view, destructive commands ask before they
  act, and the binary with no command opens a full-screen console.
- **Anywhere else** (CI, a Kubernetes Job, Compose, a pipe), or with `-plain`, it prints one line per
  event and plain tables; with `-json` it prints JSON lines for events and one JSON document per
  result.

The terminal interface is built on [Bubble Tea v2](https://charm.land/bubbletea), Bubbles, Lip
Gloss and [Huh](https://charm.land/huh). Those dependencies live only in `keelcli`: a server that
imports `keel` or `keel/migrate` never links them.

Try it without a database:

```sh
go run ./keelcli/cmd/demo            # the console
go run ./keelcli/cmd/demo up         # the deploy step's live view
go run ./keelcli/cmd/demo up -plain  # the same run as log lines
KEEL_DEMO=fail-seed go run ./keelcli/cmd/demo up
```

`KEEL_DEMO_SPEED` scales the simulated time (`0` is instant) and `KEEL_DEMO` takes comma-separated
scenario switches: `drift`, `fail-migration`, `fail-seed`, `production`, `no-lock-wait`.

## Wiring an application

An application describes its database in a `keelcli.App` and calls `Main`; keelcli builds the
engines over `keel/migrate` and `keel/seed` and opens the database the first time a command needs
it, so `help`, `migrate create` and `migrate stamp` work without one:

```go
// services/tms/cmd/db/main.go
func main() {
	keelcli.Main(keelcli.App{
		Name:       "trenova-db",
		Version:    buildinfo.Version,
		Config:     dbconfig.FromEnv, // func() (keel.Config, error)
		Migrations: migrate.Sources(migrate.Source{Name: "core", FS: schema.Migrations}),
		Migrate:    migrate.Options{After: []string{"SELECT trenova_rls.reconcile()"}},
		Seeds:      reference.Seeds,  // *seed.Registry; nil for none
		Tenants:    tenants.All,      // func(ctx, *keel.DB) iter.Seq2[seed.Tenant, error]
		System:     sysctx.With,      // func(ctx) ctx: the system scope seeds run under
		Extensions: []string{"pg_trgm", "btree_gin", "pgcrypto"},
	})
}
```

`trenova-devdb` is the same with `seed.Merge(reference.Seeds, fixtures.Seeds)`, the source
directories for the commands that write files, and its own commands:

```go
// services/tms/cmd/devdb/main.go
keelcli.Main(keelcli.App{
	Name:       "trenova-devdb",
	Config:     dbconfig.FromEnv,
	Migrations: migrate.Sources(migrate.Source{Name: "core", FS: schema.Migrations}),
	Migrate:    migrate.Options{After: []string{"SELECT trenova_rls.reconcile()"}},
	Dirs:       []migrate.Dir{{Name: "core", Path: "schema/migrations"}}, // create, squash
	Stamp:      "schema/version_gen.go",                                 // stamp; the default
	Dump:       []string{"docker", "exec", "trenova-db-1", "pg_dump", "-U", "trenova", "-d", "{db}"},
	Lint:       []migrate.LintRule{tenantRLS},
	Seeds:      seed.Merge(reference.Seeds, fixtures.Seeds),
	Tenants:    tenants.All,
	System:     sysctx.With,
	Parallel:   8,
	Commands:   []keelcli.Command{volume.Command},
})
```

| Field | Is |
|---|---|
| `Name`, `Description`, `Version` | the binary in help and `version`; `Name` is also the `application_name` of its connections, so a lock holder shows who it is |
| `Config` | the database configuration, called on first use |
| `Migrations` | the migrations; when it is the zero `Set` they are read from `Dirs`, again on every call |
| `Migrate` | the migrator's options: `After`, `LockTimeout`, `Retries`, `Schema`, `AllowOutOfOrder` |
| `Dirs` | the sources' directories on disk: `migrate create` writes into the first (or `-source name`), `migrate squash` rewrites them |
| `Stamp` | the schema version file `migrate stamp` writes, default `schema/version_gen.go`; its package is the one the Go files beside it declare, or the directory's name |
| `Dump` | the pg_dump command `migrate squash` runs, default `pg_dump`; `{db}` and `{dsn}` name the scratch database, so `docker exec <container> pg_dump -U <user> -d {db}` uses the server's own pg_dump, which must match its major version |
| `Lint` | the application's lint rules, run after the built-in ones |
| `Seeds`, `Tenants`, `System`, `Parallel` | the seeds and the `seed.Options` every run uses; without `Seeds` there are no seed commands and the console has no Seeds tab |
| `Extensions` | the extensions `doctor` requires |
| `Tables` | the application's generated tables (`keeltables.All`), which `verify` compares with the database; without it there is no `verify` |
| `Commands` | the application's own commands |
| `Flags`, `Setup` | global flags of the application and the hook that finishes the `App` from them once the command line is parsed (the `keel` command uses them for `-dsn` and `-dir`) |
| `Engines` | replaces any engine keelcli would build, as the tests and the demo do with package `fake` |

Templates and `reset` connect to the server's `postgres` database with the application's
credentials; `-admin dsn` on those commands, or `KEEL_ADMIN_DSN`, names another (a role with
CREATEDB). A template belongs to the application whose name starts its key, so `template` lists
and `template prune` drops only the application's own templates, never another application's or
those a test harness built with keys of its own.

An application command gets the same streams, output mode, guard and database:

```go
var Command = keelcli.Command{
	Name:    "volume",
	Usage:   "[-orgs n]",
	Summary: "seed volume data for load tests",
	Run: func(ctx context.Context, env *keelcli.Env, args []string) error {
		fs := env.FlagSet("volume") // the global flags work after the command name too
		orgs := fs.Int("orgs", 10, "organizations to create")
		if err := env.Parse(fs, args); err != nil {
			return err
		}
		if err := env.Confirm(ctx, "seed volume data", "Seed volume data into this database?"); err != nil {
			return err
		}
		db, err := env.DB(ctx) // opened on first use, closed when the command returns
		if err != nil {
			return err
		}
		return Seed(ctx, db, *orgs)
	},
}
```

An application command returns `keelcli.Usagef(...)` for a usage error (exit 2) and an error that
wraps `keelcli.ErrProblems` when a check found problems (exit 3); `Env.Confirm` applies the same
production guard and prompt as the built-in destructive commands. A `Config` that has no connection
to offer returns an error wrapping `keelcli.ErrNoDatabase`, which commands that can work without one
honour (`migrate lint` then lints every migration).

`App.Run(ctx, args, stdin, stdout, stderr)` returns the exit code instead of exiting, for tests and
embedding. `Main` cancels the running command on SIGINT or SIGTERM, so a Kubernetes Job that is
stopped mid-run lets the engine roll back the current migration and reports what was left undone; a
second signal kills the process.

### The generic `keel` command

`keel` is a keelcli binary too, for applications without seeds: every command but `gen` is
keelcli's, over the migrations its flags name. `keel` with no arguments in a terminal opens the
console with migrations, templates, lint and doctor:

```sh
export KEEL_DSN=postgres://app:secret@localhost:5432/app
keel migrate create add_holds -dir core=migrations
keel up -dir core=migrations -after "SELECT reconcile()"
keel migrate check -base-dir core=../main/migrations   # in CI, without a database
keel                                                    # the console
```

Its global flags are `-dsn` (default `$KEEL_DSN`), `-dir [name=]path` (repeatable, default
`migrations`), `-schema`, `-lock-timeout` (`0` for none), `-retries` (`0` for none),
`-allow-out-of-order` and `-after statement` (repeatable), before or after the command, next to
keelcli's own.

### The ports

The commands, the live views and the console reach the database only through three ports, which
`internal/adapt` implements over the real engines and package `fake` over in-memory state. Each
port is split by use, so a command depends only on what it calls (`migrateport.Runner` for a run,
`migrateport.Repairer` for `baseline` and `repair`, and so on); `Engine` and `Facts` join them.

```go
type migrateport.Engine interface {
	Status(ctx) ([]Status, error)                      // Reader
	History(ctx) ([]Record, error)
	Plan(ctx, PlanOptions) (Plan, error)               // Planner
	Up(ctx, UpOptions, Observer) error                 // Runner
	Create(ctx, CreateRequest) (Created, error)        // Author
	Squash(ctx, SquashOptions) (Squashed, error)
	Stamp(ctx, StampOptions) (Stamp, error)
	Check(ctx, CheckOptions) ([]Problem, error)        // Checker
	Lint(ctx, LintOptions) ([]Finding, error)
	Baseline(ctx, version) (Baselined, error)          // Repairer
	Repair(ctx, version) (Repaired, error)
	Templates(ctx) ([]Template, error)                 // Templater
	BuildTemplate(ctx) (Template, error)
	PruneTemplates(ctx) ([]Template, error)
	Reset(ctx) (Reset, error)
}

type seedport.Engine interface {
	Graph() []Seed                                     // Grapher
	Status(ctx) ([]Status, error)                      // Reader
	Plan(ctx, PlanOptions) ([]PlanItem, error)         // Planner
	Apply(ctx, ApplyOptions, Observer) error           // Applier
	Prune(ctx) ([]Status, error)                       // Pruner
	Environment(ctx) (Environment, error)              // Environments
	MarkEnvironment(ctx, Environment) error
}

type factsport.Facts interface {
	Connection(ctx) (Connection, error)                // Connector
	Settings(ctx, names...) ([]Setting, error)         // Inspector
	Extensions(ctx) ([]Extension, error)
	Activity(ctx, longer) (Activity, error)            // Monitor
	Environment(ctx) (seedport.Environment, error)     // EnvironmentReader
}
```

The event types are the views' contract, and the engines do not emit them as they are, so the
adapters translate (each rule is tested in `internal/adapt`):

| Port semantics | keel/migrate and keel/seed emit |
|---|---|
| `Index` counts from 1 over the migrations the run executes | from 0, counting baselines it marks |
| migration `RunEnd.Total` is the migrations applied | the size of the plan |
| `Retry.Attempt` is the attempt that failed | the attempt about to run |
| `LockWait.Attempt` numbers the reports | no attempt |
| `LockAcquired.Duration` is zero when the lock was free | the time spent taking it |
| `After` reports a statement once it finished, with its duration | before each statement, with its SQL |
| `Retry` with a nil `Migration` retries the After statements | the same |
| one `Planned` per seed, numbered from 1 | one event holding the plan |
| `TenantStart`/`TenantEnd` carry the seed's rows and tenants applied so far | the tenant's own rows, failures counted as done |
| seed `RunEnd.Total` is the seeds applied | unset |

A seed run reports waiting for its lock before its plan, since it plans once it holds the lock.

## Commands

Global flags go before or after the command: `-json`, `-plain`, `-no-color` (`NO_COLOR` works too),
`-yes` (answer confirmations), `-yes-production` (allow destructive commands on a production
database), and the application's own. A command's flags may follow its arguments too
(`migrate create add_holds -kind notx`).

| Command | Does |
|---|---|
| (none) | opens the console in a terminal; prints help and exits 2 otherwise |
| `up [-to version] [-dry-run] [-env name]` | migrate, then apply seeds: the deploy step. `-env` (production, staging, development or test) marks an unmarked database and refuses one marked otherwise; `-to` stops after a version and skips seeds; `-dry-run` prints the plan with its SQL and the seed plan |
| `migrate up [-to version] [-dry-run]` | apply pending migrations |
| `migrate plan [-to version]` | what `migrate up` would do |
| `migrate status` | every migration with its state: applied, pending, changed, repeatable-due, missing |
| `migrate history` | applied migrations with source, time and duration |
| `migrate create [-kind tx\|notx\|repeatable] [-source name] [name]` | write a migration file; in a terminal without a name, asks for the name and kind |
| `migrate squash [-to version] [-dump command] [-admin dsn]` | replace applied migrations with one baseline (confirms) |
| `migrate stamp [-check] [-out path]` | write the schema version file; `-check` exits 3 when it is stale |
| `migrate check [-base-dir [name=]path]...` | ordering and checksum problems against the database, or the base branch's directories without one; exits 3 when there are any |
| `migrate lint [-all]` | lint pending migrations, or all of them (also without a database); exits 3 on an error finding |
| `migrate baseline <version>` | record migrations up to version as applied (confirms) |
| `migrate repair <version>` | accept the current checksum of a changed migration (confirms) |
| `seed apply [-force] [-dry-run] [names...]` | apply seeds that changed, or the named ones with their dependencies |
| `seed plan [-force] [names...]` | which seeds would run and why |
| `seed status` | seeds with state, reason, rows and tenants |
| `seed prune` | delete records of seeds no longer registered (confirms) |
| `reset [-admin dsn]` | recreate the database from the current template, building it first, keeping the environment mark (confirms; never on production) |
| `template` / `template build` / `template prune` | list, build (migrations and reference seeds), or drop the application's stale templates (prune confirms); each takes `-admin` |
| `doctor` | connection, server, `plan_cache_mode`, extensions, migrations, templates, environment, lock holders and long queries; exits 3 when a check fails |
| `verify` | compare the models (`App.Tables`) with the database through `keel.Verify`: missing tables and columns, primary keys, nullability, generated columns, types, join tables and relation keys, each with a fix; exits 3 on drift |
| `plans -shapes file [-seq-scan-rows n] [-max-cost c] [-sort-limit]` | the plan guard: `EXPLAIN (GENERIC_PLAN, VERBOSE, FORMAT JSON)` (PostgreSQL 16) of every statement shape in the file `keel.RecordShapesFile` wrote (`-` reads stdin), on one connection of its own outside transactions; flags sequential scans of relations whose `reltuples` exceed `-seq-scan-rows` (default 10000, `-1` flags every one), plans whose total cost exceeds `-max-cost` (default 0, no limit) and, with `-sort-limit`, a `Sort` under a `Limit` over a relation above the same `-seq-scan-rows` threshold; a shape EXPLAIN cannot plan is listed with its error and does not fail the run; exits 3 on any finding. Present whenever the App has a `Config` |
| `version` | the binary's name and version |
| `help [command]` | help for a command |

Exit codes: `0` ok, `1` failure, `2` usage error or refusal (a missing flag would lift it), `3` a
check found problems.

### Confirmation and production

Destructive commands name the database and its environment mark and ask:

```
┃ Drop 2 stale templates?
┃ Database: trenova on localhost:5432
┃ Environment: development
┃ Templates: keel_tpl_51ab07e3, keel_tpl_c7d2e9aa
┃
┃            Yes, proceed     Cancel

←/→ toggle • enter submit • y Yes, proceed • n Cancel
```

A database marked production is refused unless `-yes-production` is given; `reset` refuses it
whatever the flags say. `-yes` skips the prompt. Without a terminal, a command that would ask is a
usage error, never a hang:

```
$ trenova-db template prune < /dev/null
keelcli: prune templates needs confirmation and there is no terminal to ask on: pass -yes
$ KEEL_DEMO=production trenova-devdb reset -yes -yes-production
keelcli: refusing to reset the database: trenova on localhost:5432 is marked production, where it never runs
```

## Live view

`up`, `migrate up` and `seed apply` in a terminal, 80 columns wide. While another deploy holds the
migration lock:

```
Migrations                                                            0/4 · 1.1s
  ·  20261025110000  edi_partner_index       core · notx · lock_timeout 2s
  ·  20261025120000  rate_tables             core
  ·  20261026080000  backfill_rate_currency  core · go
  ·  R               tenant_rls_helpers      core · repeatable

╭──────────────────────────────────────────────────────────────────────────────╮
│ ⧗ Waiting for the migration lock · attempt 2 · 1.1s                          │
│   PID  APPLICATION                AGE  QUERY                                 │
│ 51203  trenova-db (deploy 418)  41.7s  SELECT pg_advisory_lock(737263183645… │
╰──────────────────────────────────────────────────────────────────────────────╯

ctrl+c cancel
```

A migration whose lock timed out behind a report query, retrying:

```
Migrations                                                            1/4 · 8.3s
  ✓  20261025110000  edi_partner_index       2 statements              4s
  ⠋  20261025120000  rate_tables             lock timeout · retry 1  2.2s
  ·  20261026080000  backfill_rate_currency
  ·  R               tenant_rls_helpers
  migration lock acquired after 2.1s

╭──────────────────────────────────────────────────────────────────────────────╮
│ ⧗ rate_tables waits on a lock · lock timeout · retry 1 · 2.2s                │
│   PID  APPLICATION      AGE  QUERY                                           │
│ 48121  report-runner  4m13s  SELECT s.pro_number, sum(m.distance) FROM ship… │
│ 48377  trenova-api    39.2s  UPDATE shipments SET status = $1 WHERE id = $2  │
╰──────────────────────────────────────────────────────────────────────────────╯
```

A per-tenant seed:

```
Seeds                                                                 2/7 · 3.4s
  ✓  hazmat_references              reference            3,121 rows        1.4s
  ✓  organization_roles             reference            24 rows          310ms
  ⠸  organization_role_permissions  reference · tenants  39,576 rows       1.3s
    ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━ 1,649 / 9,000 tenants
  ·  document_template_starters     reference            version changed
  ·  test_organizations             fixture              new
  ·  workers                        fixture              new
  ·  shipments                      fixture              new
  – 3 up to date: us_states, ifta_jurisdictions, system_agent_definitions
  seed lock acquired after 400ms
```

The end of a run that failed at a tenant:

```
Seeds                                                                 2/7 · 5.4s
  ✓  hazmat_references    reference            3,121 rows                   1.4s
  ✓  organization_roles   reference            24 rows                     310ms
  ✗  organization_role_…  reference · tenants  failed at tenant org_04231   3.3s
      tenant org_04231: ERROR: duplicate key value violates unique constraint
      "role_permissions_role_id_resource_key" (SQLSTATE 23505)
  ⊘  document_template_…  reference            not run
  ⊘  test_organizations   fixture              not run
  ⊘  workers              fixture              not run
  ⊘  shipments            fixture              not run
  – 3 up to date: us_states, ifta_jurisdictions, system_agent_definitions

Summary
  ✓  Migrations  4 migrations applied  11.9s
  ✗  Seeds       failed after 2 seeds   5.4s
  tenant org_04231: ERROR: duplicate key value violates unique constraint
  "role_permissions_role_id_resource_key" (SQLSTATE 23505)

Left undone
  seeds: document_template_starters, test_organizations, workers, shipments
```

Ctrl+C cancels the run's context; the view waits for the engine to stop the current step, then
lists what was left undone (a second Ctrl+C quits at once):

```
Migrations                                                            0/4 · 4.4s
  ⊘  20261025110000  edi_partner_index       cancelled               2.3s
  ⊘  20261025120000  rate_tables             not run
  ⊘  20261026080000  backfill_rate_currency  not run
  ⊘  R               tenant_rls_helpers      not run

Summary
  ⊘  Migrations  cancelled after 0 migrations  4.4s
  ⊘  Seeds       not started

Left undone
  migrations: 20261025110000_edi_partner_index, 20261025120000_rate_tables,
  20261026080000_backfill_rate_currency, R_tenant_rls_helpers
  seeds: not started
```

Without a terminal the same run prints one line per event, per-tenant progress once per tenth:

```
lock    waiting for the migration lock (attempt 1)
          blocked by pid 51203 trenova-db (deploy 418), active for 41s, advisory: SELECT pg_advisory_lock(7372631836457123681)
lock    migration lock acquired after 2.1s
migrate [1/4] 20261025110000_edi_partner_index  core · notx · lock_timeout 2s
migrate [1/4] 20261025110000_edi_partner_index  statement 1/2
migrate [2/4] 20261025120000_rate_tables  lock timeout, retry 1: ERROR: canceling statement due to lock timeout (SQLSTATE 55P03)
          blocked by pid 48121 report-runner, active for 4m13s, AccessShareLock: SELECT s.pro_number, sum(m.distance) FROM …
seed    [3/7] organization_role_permissions  tenants 900/9,000
seed    done: 7 seeds applied in 13.2s
```

## Console

`trenova-devdb` with no arguments in a terminal. The header shows the binary, the environment mark,
the schema version (the newest recorded migration), the redacted DSN and the server. Captures at
100×32 with the `drift` scenario:

**Migrations**: state glyphs, source, directives, the SQL highlighted below; `u` applies all, `U`
applies up to the selection, `p` shows the plan, `c` creates from a form, `x` repairs a changed
checksum, `/` filters.

```
 trenova-devdb  development · v20261024150000 · postgres://trenova:***@localhost:5432/trenova?sslmo…
 1 Migrations (4)   2 Seeds (7)   3 Templates   4 Lint (3)   5 Doctor (!)
────────────────────────────────────────────────────────────────────────────────────────────────────
     VERSION         NAME                    SOURCE  STATE           APPLIED           DETAIL
  ?  20261020100000  legacy_fuel_codes       core    missing         2026-10-19 00:00  no file…
  ✓  20261023143000  baseline                core    applied         2026-10-20 14:31  baseline
  ✓  20261024091500  shipment_holds          core    applied         2026-10-21 09:16
▌ !  20261024150000  driver_qualifications   cloud   changed         2026-10-21 15:02  checksu…
  •  20261025110000  edi_partner_index       core    pending                           notx · …
  •  20261025120000  rate_tables             core    pending
  •  20261026080000  backfill_rate_currency  core    pending                           go
  ↻  R               tenant_rls_helpers      core    repeatable-due  2026-10-21 09:16  checksu…

────────────────────────────────────────────────────────────────────────────────────────────────────
20261024150000_driver_qualifications · cloud · changed · recorded c23c2dbdc2ff, file f20badfef869
1 │CREATE TABLE driver_qualifications (
2 │    id text PRIMARY KEY,
3 │    organization_id text NOT NULL,
4 │    worker_id text NOT NULL,
5 │    kind text NOT NULL,
6 │    expires_on date NOT NULL
7 │);
────────────────────────────────────────────────────────────────────────────────────────────────────

u apply all • U apply to selected • p plan • c create • x repair checksum • enter view SQL …
```

**Seeds**: the dependency graph as a tree, with kind, state and the reason each would run; `space`
selects, `a` applies the selection with its dependencies (or everything due), `f` forces, `p` plans.

```
          SEED                              STATE     REASON                               ROWS
  [ ]  ✓  us_states                         applied   up to date                             56
  [ ]  ✓  └─ ifta_jurisdictions             applied   up to date                             68
  [ ]  •  hazmat_references                 due       input changed (data/hazmat.yaml)    3,104
  [ ]  •  organization_roles                due       new
▌ [ ]  •  ├─ organization_role_permissions  due       code changed                      211,000
  [ ]  •  ├─ document_template_starters     due       version changed                        12
  [ ]  •  └─ test_organizations             due       new
  [ ]  •     └─ workers                     due       new
  [ ]  •        └─ shipments                due       new
  [ ]  ✓  system_agent_definitions          applied   up to date                              9
       ?  legacy_rate_codes                 orphaned  excluded                              412
────────────────────────────────────────────────────────────────────────────────────────────────────
organization_role_permissions · reference · tenants · due
Permission sync for every role
after organization_roles
reason: code changed
applied 2026-10-22 21:51 in 7s · 211,000 rows
tenants recorded: 9,000
```

**Templates**: `b` builds the current template, `x` prunes stale ones, `R` resets the database.

```
     TEMPLATE           STATE     SIZE  AGE  CREATED
▌ ✓  keel_tpl_9f2c41d0  current  48 MB   2h  2026-10-23 21:51
  –  keel_tpl_51ab07e3  stale    46 MB   3d  2026-10-20 23:51
  –  keel_tpl_c7d2e9aa  stale    45 MB  19d  2026-10-04 23:51

Each test and reset copies the current template, so a fresh database takes milliseconds;
stale templates belong to migrations that have since changed.
```

**Lint**: findings grouped by rule; `enter` opens the migration with the line marked.

```
tenant-rls
▌ ! warning 20261025120000_rate_tables:1  tenant table: reconcile() enables row-level security afte…
not-null-default
  ✗ error   20261025120000_rate_tables:9  ADD COLUMN … NOT NULL without a default fails on a table …
index-concurrently
  ✗ error   20261025120000_rate_tables:10  CREATE INDEX on existing table shipments without CONCURR…
```

**Doctor**: the doctor checks, refreshed every three seconds while the tab is open.

```
   CHECK            RESULT  DETAIL
✓  connection       pass    localhost:5432/trenova as trenova in <1ms
✓  server           pass    PostgreSQL 18.1
✓  plan_cache_mode  pass    auto: generic plans when they are no worse (keel's default)
✓  template copy    pass    file_copy_method = clone: templates copy by reflink
✓  extensions       pass    plpgsql, pg_trgm, btree_gin, pgcrypto, pg_stat_statements
✗  migrations       fail    2 changed or missing (migrate check) · 4 pending (up)
i  templates        info    keel_tpl_9f2c41d0, 48 MB, 2h old · 2 stale (template prune)
✓  environment      pass    development
!  locks            warn    2 backends hold locks others wait for; a migration would queue behind t…
!  long queries     warn    1 query running longer than 1m

Lock holders
  PID  APPLICATION      AGE  QUERY
48121  report-runner  4m15s  SELECT s.pro_number, sum(m.distance) FROM shipments s JOIN moves m ON …
48377  trenova-api    41.1s  UPDATE shipments SET status = $1 WHERE id = $2

checked 23:51:49 · refreshes every 3s
```

Everywhere: `tab`/`shift+tab` or `1`–`5` switch tabs, `↑`/`↓` or `j`/`k` move, `r` refreshes, `?`
shows every key, the mouse wheel scrolls, `q` quits. Applying runs the live view inside the console;
Ctrl+C cancels it and `enter` returns. The console calls the same ports as the commands and confirms
destructive actions with the same guard, so everything it does is also scriptable.

## Design notes

- **Command parsing** is the standard `flag` package with a small router. It matches keel's own
  `keel gen` flags (`-to`, `-dry-run`), has no dependency, and every command's flag set also binds
  the global flags, so `trenova-db -json migrate status` and `trenova-db migrate status -json` both
  work. A framework such as Cobra would add a dependency tree and GNU-style flags for no feature this
  needs.
- **SQL highlighting** is a small lexer (`internal/ui/sqlview`): migration SQL needs only keywords,
  strings, dollar quotes, numbers and comments; the colours must come from keelcli's theme; and
  `--keel:` directives get their own colour, which a general highlighter does not know. Chroma would
  link over 200 lexers into every database binary for that.
- **One theme** (`internal/ui/theme`) defines every colour for dark and light backgrounds; views
  restyle when Bubble Tea reports the background, and Huh forms take their styles from it.
- **Engine calls run off the UI goroutine.** A run's events cross a channel and reach the model in
  batches, so a seed over 100,000 tenants costs one render per batch, not per tenant.
- **Plain output is complete and stable**: no colour, no truncation, deterministic durations from the
  engine, golden-tested in `testdata/`.
