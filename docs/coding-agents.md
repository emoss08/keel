# Using keel with coding agents

keel ships a skill for coding agents such as Claude Code, and an [`llms.txt`](../llms.txt) for any
tool that reads one. Both teach an agent to write keel code the way the [guide](guide.md)
describes it, instead of the habits it brings from other ORMs: string column names, hand-written
tenant filters, a query per row, offset paging, unbounded lists.

## Contents

- [What the skill teaches](#what-the-skill-teaches)
- [Install it as a Claude Code plugin](#install-it-as-a-claude-code-plugin)
- [Install it into a project with keel skill](#install-it-into-a-project-with-keel-skill)
- [Which to choose](#which-to-choose)
- [Other agents and llms.txt](#other-agents-and-llmstxt)
- [Changing the skill](#changing-the-skill)

## What the skill teaches

The skill is a directory, [`skills/keel`](../skills/keel/SKILL.md). Its `SKILL.md` is what the agent
reads whenever it works on keel code (code that imports keel or a generated `…db` package, models
with `keel` tags, migrations, seeds). It holds:

- **How keel works:** models are plain structs and the schema lives in migrations; generated handles
  and typed columns; the context carries the tenant, the transaction, a replica mark and a budget;
  every terminal takes `(ctx, db)`; round trips are the unit of cost.
- **Rules the agent must not break:** query and write through the generated package; never put values
  into SQL text; never add tenant filters or set tenant columns by hand; cross tenants only with a
  system scope and `AllTenants()`; give `List` a `Limit` and use `Page` for scrolling lists; load
  relations with `Load`, never in a loop; write transactions as `keel.Write`/`keel.Read` closures that
  use their own `ctx`; compare errors with `errors.Is`/`errors.As`; never edit generated files.
- **Patterns and choices:** the canonical shape of each kind of read and write, a table of which call
  fits which need, and a table of common mistakes with their fixes.
- **A finishing checklist:** generated code current, a migration for every model change, bounded and
  batched queries, tests and `keel.Verify` passing.

Detail the agent reads only when a task needs it lives in `references/`:

| Reference | Covers |
|---|---|
| [models-and-gen.md](../skills/keel/references/models-and-gen.md) | tags, Go types, relations, picks, `nullzero`, computed fields, model hooks, constraint messages, `keel gen` |
| [reading.md](../skills/keel/references/reading.md) | terminals, operators, typed expressions, raw SQL, paths, joins, subqueries, CTEs, aggregates, windows, locks, search, `ToSQL`, `Explain` |
| [loading-pagination-batch.md](../skills/keel/references/loading-pagination-batch.md) | `Load` and its one-round-trip rules, per-parent limits, keyset pages and cursors, counts, `Each`, `Batch` |
| [writing.md](../skills/keel/references/writing.md) | `Create`, `CreateMany`, `Copy`, `Save`, `DeleteRow`, bulk writes, `UpdateReturning`, `Unnest`, `Upsert`, `CreateOrSkip`, `InsertFrom`, `SetLinks` |
| [transactions-and-tenancy.md](../skills/keel/references/transactions-and-tenancy.md) | transactions and their options, scopes, tenant concurrency, budgets, replicas, data versions, the result cache |
| [hooks-errors-observability.md](../skills/keel/references/hooks-errors-observability.md) | table hooks, interceptors, read filters, observers, `keellog`, errors |
| [config-migrations-seeds.md](../skills/keel/references/config-migrations-seeds.md) | `keel.Config`, type codecs, migrations, seeds, `keelcli` |
| [testing-and-performance.md](../skills/keel/references/testing-and-performance.md) | test setup, `keel.Verify`, `keeltest`, the plan guard, performance rules |
| [dynamic-and-analytics.md](../skills/keel/references/dynamic-and-analytics.md) | `keel.Registry`, `DynQuery`, results and streaming, `keel/analytics` |
| [migrating-from-other-libraries.md](../skills/keel/references/migrating-from-other-libraries.md) | Bun, GORM, sqlx and pgx idioms mapped to keel; running beside another layer |

## Install it as a Claude Code plugin

keel's repository is a Claude Code plugin marketplace. In Claude Code:

```text
/plugin marketplace add emoss08/keel
/plugin install keel@keel
```

On Claude Code 2.1.275 or later, one command does both:

```text
/plugin install keel --marketplace emoss08/keel
```

From a shell, the same with the `claude` command:

```bash
claude plugin marketplace add emoss08/keel
```

```bash
claude plugin install keel@keel
```

- **Scope.** A plugin installs for your user by default. `claude plugin install keel@keel --scope project`
  records it in the project's settings, so everyone who opens the project in Claude Code is offered
  it.
- **Updates.** The plugin is versioned by the repository's commit, so it follows keel's default
  branch. Refresh it with `claude plugin marketplace update keel` (or from `/plugin`).
- **Removing it.** `claude plugin uninstall keel@keel`, then `claude plugin marketplace remove keel`.

## Install it into a project with keel skill

The skill's files are embedded in the `keel` command, so a project can write the skill that matches
the keel version in its own `go.mod`:

```bash
go run github.com/emoss08/keel/cmd/keel skill install
```

Run it inside the project: `go run` uses the keel version the module requires, and the skill lands
in `.claude/skills/keel` at the module root (the nearest directory with a `go.mod`). Commit that
directory and everyone who works on the project, and every agent session in it, gets the skill. A
project that pins the generator as a tool (`go get -tool github.com/emoss08/keel/cmd/keel`) runs
`go tool keel skill install` instead. The `skill` command is in keel releases after v0.9.0.

| Flag | Effect |
|---|---|
| (none) | install into `.claude/skills/keel` at the module root |
| `-user` | install into `~/.claude/skills/keel`, for every project you open |
| `-to dir` | install into `dir/keel`, for any skills directory |
| `-force` | write over a `keel` directory that is not a keel skill |

- **Upgrading.** After upgrading keel, run `keel skill install` again: it replaces the installed
  skill completely, so files an older version had and the new one dropped do not linger. Make it part
  of the same change that bumps keel in `go.mod`.
- **Safety.** `install` replaces a directory only when its `SKILL.md` names the keel skill. Any other
  non-empty `keel` directory is refused; with `-force` the skill's files are written over it and
  nothing else in it is removed.
- **Reading it.** `keel skill print` writes `SKILL.md` to standard output, and
  `keel skill print references/writing.md` any other file of the skill.

## Which to choose

- **The plugin** is the quickest way to try the skill and to use it across many projects. It follows
  keel's default branch, so it can describe API that is newer than the keel version a project uses.
- **`keel skill install`** pins the skill to the project's keel version, keeps it in the repository
  for every contributor and every agent session (local, CI or cloud), and changes only when the
  project upgrades keel. Prefer it for a team project.

Install one or the other in a project, not both: two copies of the skill would compete.

## Other agents and llms.txt

The skill follows the Agent Skills layout (a directory holding `SKILL.md` with YAML front matter and
the files it links), so agents other than Claude Code that read skills can use it:
`keel skill install -to <that agent's skills directory>`. An agent that reads one instruction file
can be pointed at the installed `SKILL.md` from it, or given its text with `keel skill print`.

[`llms.txt`](../llms.txt), at the repository root, follows the [llms.txt](https://llmstxt.org)
format: a summary of keel, the core rules, how to install the skill, and links to the
documentation and the skill's references. Give its URL to a tool that fetches documentation:

```text
https://raw.githubusercontent.com/emoss08/keel/master/llms.txt
```

## Changing the skill

The skill is part of keel's source, in [`skills/keel`](../skills/keel), and the Go package
[`skills`](../skills/skills.go) embeds it for the `keel skill` command. When a change to keel's API
changes how code should be written, update the skill in the same change:

- Keep `SKILL.md` short (the test fails past 500 lines) and the rules in it the ones an agent must
  not break; put detail in a reference file and link it from `SKILL.md`'s reference list.
- Write examples against the fleet test models, so they can be checked by compiling them against
  `fleetdb`.
- `go test ./skills` checks the front matter, that every link resolves and that every reference is
  linked; `claude plugin validate ./` checks the plugin and marketplace manifests.
