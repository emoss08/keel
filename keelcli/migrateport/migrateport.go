// Package migrateport declares what keelcli needs from a migration engine.
//
// The types mirror package keel/migrate field for field, so an adapter from the engine to these
// ports is a set of plain conversions. Each interface covers one use, so a command depends only on
// the calls it makes; Engine joins them for App.
package migrateport

import (
	"context"
	"time"
)

// Kind is what a migration file is.
type Kind uint8

// The migration kinds.
const (
	Versioned Kind = iota
	Repeatable
	Go
	Baseline
)

// String names the kind.
func (k Kind) String() string {
	switch k {
	case Repeatable:
		return "repeatable"
	case Go:
		return "go"
	case Baseline:
		return "baseline"
	default:
		return "versioned"
	}
}

// Directives are the --keel: lines at the top of a migration file. LockTimeout is zero for the
// default and negative when the file disables it with --keel:lock_timeout 0.
type Directives struct {
	NoTx             bool
	LockTimeout      time.Duration
	StatementTimeout time.Duration
	NoRetry          bool
}

// Migration is one migration from the application's sources.
type Migration struct {
	Version    string
	Name       string
	Source     string
	Kind       Kind
	Directives Directives
	Checksum   string
	SQL        string
}

// ID is the migration's file name without the extension: version and name, or R_ and the name
// of a repeatable migration.
func (m Migration) ID() string {
	if m.Version == "" {
		return "R_" + m.Name
	}
	return m.Version + "_" + m.Name
}

// State is where a migration stands in the database.
type State uint8

// The migration states.
const (
	Applied State = iota
	Pending
	Changed
	RepeatableDue
	Missing
)

// String names the state.
func (s State) String() string {
	switch s {
	case Pending:
		return "pending"
	case Changed:
		return "changed"
	case RepeatableDue:
		return "repeatable-due"
	case Missing:
		return "missing"
	default:
		return "applied"
	}
}

// Status is a migration with its state. A Missing migration has a bookkeeping row and no file, so
// only its Version, Name, Source and Checksum are set.
type Status struct {
	Migration       Migration
	State           State
	AppliedAt       time.Time
	Duration        time.Duration
	AppliedChecksum string
}

// Recorded reports whether the bookkeeping table has a row for the migration: applied, changed
// since, or missing from the sources.
func (s Status) Recorded() bool {
	return s.State == Applied || s.State == Changed || s.State == Missing
}

// Record is one row of the bookkeeping table.
type Record struct {
	Version   string
	Name      string
	Source    string
	Checksum  string
	AppliedAt time.Time
	Duration  time.Duration
}

// Plan is what Up would do: the migrations it would run in order, the baseline migrations it
// would record without running, and the After statements it would run at the end.
type Plan struct {
	Migrations []Migration
	Baseline   []Migration
	After      []string
}

// Empty reports whether the plan does nothing.
func (p Plan) Empty() bool { return len(p.Migrations) == 0 && len(p.Baseline) == 0 }

// PlanOptions narrows a plan.
type PlanOptions struct {
	To string
}

// UpOptions narrows a run. A dry run is a Plan, whose migrations carry their SQL.
type UpOptions struct {
	To string
}

// EventKind is what an Event reports.
type EventKind uint8

// The event kinds, in the order a run emits them.
const (
	LockWait EventKind = iota
	LockAcquired
	MigrationStart
	Statement
	Retry
	MigrationEnd
	BaselineMarked
	After
	RunEnd
)

// String names the event kind.
func (k EventKind) String() string {
	switch k {
	case LockAcquired:
		return "lock_acquired"
	case MigrationStart:
		return "migration_start"
	case Statement:
		return "statement"
	case Retry:
		return "retry"
	case MigrationEnd:
		return "migration_end"
	case BaselineMarked:
		return "baseline_marked"
	case After:
		return "after"
	case RunEnd:
		return "run_end"
	default:
		return "lock_wait"
	}
}

// Event is one step of a run.
//
// LockWait: Attempt (1-based count of the reports), Duration waited so far, Blockers holding the
// migration lock. LockAcquired: Duration waited, zero when the lock was free. MigrationStart:
// Migration, Index (1-based) of Total, the migrations the run executes; baselines recorded
// without running are not counted. Statement: a notx migration starting statement Statement of
// Statements. Retry: Attempt that failed with a lock timeout, Err, Blockers; Migration is nil
// when the After statements are retried. MigrationEnd: Duration, Err. BaselineMarked: Migration.
// After: Statement of Statements finished, Duration. RunEnd: Total migrations applied,
// Duration, Err.
type Event struct {
	Kind       EventKind
	Migration  *Migration
	Index      int
	Total      int
	Statement  int
	Statements int
	Attempt    int
	Blockers   []Blocker
	Duration   time.Duration
	Err        error
}

// Blocker is a backend holding a lock a migration waits for.
type Blocker struct {
	PID         int32
	Application string
	State       string
	Mode        string
	Age         time.Duration
	Query       string
}

// Observer receives a run's events on the engine's goroutine.
type Observer func(Event)

// CreateKind is the kind of file Create writes.
type CreateKind uint8

// The kinds of migration Create writes.
const (
	Transactional CreateKind = iota
	NoTx
	RepeatableFile
)

// String names the create kind as the -kind flag spells it.
func (k CreateKind) String() string {
	switch k {
	case NoTx:
		return "notx"
	case RepeatableFile:
		return "repeatable"
	default:
		return "tx"
	}
}

// CreateRequest names a new migration. Source names the source whose directory receives the
// file; empty is the first.
type CreateRequest struct {
	Name   string
	Kind   CreateKind
	Source string
}

// Created is the file Create wrote.
type Created struct {
	Path      string
	Migration Migration
}

// Severity grades a lint finding.
type Severity uint8

// The finding severities.
const (
	Warning Severity = iota
	Error
)

// String names the severity.
func (s Severity) String() string {
	if s == Error {
		return "error"
	}
	return "warning"
}

// Finding is one lint result.
type Finding struct {
	Rule      string
	Migration Migration
	Line      int
	Message   string
	Severity  Severity
}

// ProblemKind is what Check found.
type ProblemKind uint8

// The problems Check reports.
const (
	OutOfOrder ProblemKind = iota
	ChangedFile
	MissingFile
	BaselineGap
)

// String names the problem kind.
func (k ProblemKind) String() string {
	switch k {
	case ChangedFile:
		return "changed"
	case MissingFile:
		return "missing"
	case BaselineGap:
		return "gap"
	default:
		return "out-of-order"
	}
}

// Problem is one result of Check.
type Problem struct {
	Kind      ProblemKind
	Migration Migration
	Message   string
}

// Dir is a directory of migration files on disk, named as its source.
type Dir struct {
	Name string
	Path string
}

// CheckOptions selects what Check compares against: the database's history, or Base, the
// migration directories of the branch being merged into, when it is set.
type CheckOptions struct {
	Base []Dir
}

// LintOptions selects what Lint checks: the pending migrations of the database, or every
// migration with All. Without a database, Lint checks every migration.
type LintOptions struct {
	All bool
}

// SquashOptions narrows Squash: To is the newest version squashed, default the newest of all;
// Dump replaces the engine's pg_dump command when set.
type SquashOptions struct {
	To   string
	Dump []string
}

// Baselined is what Baseline recorded.
type Baselined struct {
	Marked []Migration
}

// Repaired is what Repair accepted.
type Repaired struct {
	Migration   Migration
	OldChecksum string
	NewChecksum string
}

// Squashed is what Squash wrote.
type Squashed struct {
	Path     string
	Version  string
	Replaced int
}

// Stamp is the schema version file. With Check set, Stamp writes nothing and Current reports
// whether the file matches the migrations.
type Stamp struct {
	Path        string
	Version     string
	Fingerprint string
	Current     bool
}

// StampOptions controls Stamp. Out replaces the engine's path of the file when set.
type StampOptions struct {
	Check bool
	Out   string
}

// Template is one template database.
type Template struct {
	Name        string
	Fingerprint string
	Current     bool
	Size        int64
	CreatedAt   time.Time
}

// Reset is what Reset did: the database it recreated, the template it copied, whether it built
// the template first and how long it took.
type Reset struct {
	Database string
	Template string
	Built    bool
	Duration time.Duration
}

// Reader reads the migrations and the bookkeeping table.
type Reader interface {
	Status(ctx context.Context) ([]Status, error)
	History(ctx context.Context) ([]Record, error)
}

// Planner reports what a run would do.
type Planner interface {
	Plan(ctx context.Context, opts PlanOptions) (Plan, error)
}

// Runner applies pending migrations, reporting each step to observe.
type Runner interface {
	Up(ctx context.Context, opts UpOptions, observe Observer) error
}

// Author writes migration files: new ones, the squashed baseline and the version stamp.
type Author interface {
	Create(ctx context.Context, req CreateRequest) (Created, error)
	Squash(ctx context.Context, opts SquashOptions) (Squashed, error)
	Stamp(ctx context.Context, opts StampOptions) (Stamp, error)
}

// Checker finds problems before they reach production: ordering and drift, and lint findings in
// pending migrations.
type Checker interface {
	Check(ctx context.Context, opts CheckOptions) ([]Problem, error)
	Lint(ctx context.Context, opts LintOptions) ([]Finding, error)
}

// Repairer edits the bookkeeping table without running migrations.
type Repairer interface {
	Baseline(ctx context.Context, version string) (Baselined, error)
	Repair(ctx context.Context, version string) (Repaired, error)
}

// Templater manages template databases and resets the database from the current one.
type Templater interface {
	Templates(ctx context.Context) ([]Template, error)
	BuildTemplate(ctx context.Context) (Template, error)
	PruneTemplates(ctx context.Context) ([]Template, error)
	Reset(ctx context.Context) (Reset, error)
}

// Engine is every migration port.
type Engine interface {
	Reader
	Planner
	Runner
	Author
	Checker
	Repairer
	Templater
}
