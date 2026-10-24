// Package seedport declares what keelcli needs from a seed engine.
//
// The types mirror package keel/seed field for field, so an adapter from the engine to these ports
// is a set of plain conversions. Each interface covers one use; Engine joins them for App.
package seedport

import (
	"context"
	"time"
)

// Kind is what a seed provides.
type Kind uint8

// The seed kinds.
const (
	Reference Kind = iota
	Fixture
)

// String names the kind.
func (k Kind) String() string {
	if k == Fixture {
		return "fixture"
	}
	return "reference"
}

// Seed is one registered seed. After names the seeds that run before it.
type Seed struct {
	Name        string
	Kind        Kind
	Description string
	After       []string
	PerTenant   bool
}

// ReasonKind is why a seed runs or does not.
type ReasonKind uint8

// The reasons a plan gives.
const (
	New ReasonKind = iota
	InputChanged
	VersionChanged
	CodeChanged
	Forced
	UpToDate
	FixtureApplied
	Excluded
	Refused
)

// String names the reason.
func (k ReasonKind) String() string {
	switch k {
	case InputChanged:
		return "input changed"
	case VersionChanged:
		return "version changed"
	case CodeChanged:
		return "code changed"
	case Forced:
		return "forced"
	case UpToDate:
		return "up to date"
	case FixtureApplied:
		return "fixture applied"
	case Excluded:
		return "excluded"
	case Refused:
		return "refused"
	default:
		return "new"
	}
}

// Reason is why a seed runs or does not; Inputs names the changed input files.
type Reason struct {
	Kind   ReasonKind
	Inputs []string
}

// Runs reports whether the reason makes the seed run.
func (r Reason) Runs() bool {
	switch r.Kind {
	case New, InputChanged, VersionChanged, CodeChanged, Forced:
		return true
	default:
		return false
	}
}

// State is where a seed stands in the database.
type State uint8

// The seed states.
const (
	Applied State = iota
	Due
	Partial
	Orphaned
)

// String names the state.
func (s State) String() string {
	switch s {
	case Due:
		return "due"
	case Partial:
		return "partial"
	case Orphaned:
		return "orphaned"
	default:
		return "applied"
	}
}

// Status is a seed with its state. Partial is a per-tenant seed that stopped part way; Orphaned is
// a recorded seed that is no longer registered, which Prune deletes.
type Status struct {
	Seed      Seed
	State     State
	Reason    Reason
	AppliedAt time.Time
	Duration  time.Duration
	Rows      int64
	Tenants   int
}

// PlanItem is one seed of a plan with the reason it runs or does not.
type PlanItem struct {
	Seed   Seed
	Reason Reason
}

// PlanOptions narrows a plan to Only the named seeds and their dependencies, and Force runs them
// whatever their fingerprint says.
type PlanOptions struct {
	Only  []string
	Force bool
}

// ApplyOptions narrows a run like PlanOptions. A dry run is a Plan.
type ApplyOptions struct {
	Only  []string
	Force bool
}

// EventKind is what an Event reports.
type EventKind uint8

// The event kinds. A run reports waiting for the seed lock first and its plan once it holds the
// lock, so a run that never gets the lock reports no plan.
const (
	Planned EventKind = iota
	LockWait
	LockAcquired
	SeedStart
	TenantStart
	TenantEnd
	SeedEnd
	RunEnd
)

// String names the event kind.
func (k EventKind) String() string {
	switch k {
	case LockWait:
		return "lock_wait"
	case LockAcquired:
		return "lock_acquired"
	case SeedStart:
		return "seed_start"
	case TenantStart:
		return "tenant_start"
	case TenantEnd:
		return "tenant_end"
	case SeedEnd:
		return "seed_end"
	case RunEnd:
		return "run_end"
	default:
		return "planned"
	}
}

// Event is one step of a run.
//
// Planned: Seed, Reason, Index (1-based) of Total, once per seed. LockWait: Duration waited so
// far. LockAcquired: Duration waited, zero when the lock was free. SeedStart: Seed, Index of Total, Tenants for a per-tenant seed. TenantStart
// and TenantEnd: Tenant, TenantsDone of Tenants, Rows so far, Err. SeedEnd: Rows, Duration, Err.
// RunEnd: Total seeds applied, Duration, Err.
type Event struct {
	Kind        EventKind
	Seed        *Seed
	Reason      Reason
	Index       int
	Total       int
	Tenant      string
	Tenants     int
	TenantsDone int
	Rows        int64
	Duration    time.Duration
	Err         error
}

// Observer receives a run's events on the engine's goroutines; it must be safe for concurrent
// use when seeds run in parallel.
type Observer func(Event)

// Environment is the mark a database carries in keel_meta. The empty Environment is unmarked.
type Environment string

// Production is the environment fixtures and destructive commands refuse.
const Production Environment = "production"

// Grapher lists the registered seeds in dependency order.
type Grapher interface {
	Graph() []Seed
}

// Reader reads the seeds with their state.
type Reader interface {
	Status(ctx context.Context) ([]Status, error)
}

// Planner reports what a run would do and why.
type Planner interface {
	Plan(ctx context.Context, opts PlanOptions) ([]PlanItem, error)
}

// Applier runs the seeds a plan selects, reporting each step to observe.
type Applier interface {
	Apply(ctx context.Context, opts ApplyOptions, observe Observer) error
}

// Pruner deletes the records of seeds that are no longer registered.
type Pruner interface {
	Prune(ctx context.Context) ([]Status, error)
}

// Environments reads and records the database's environment mark.
type Environments interface {
	Environment(ctx context.Context) (Environment, error)
	MarkEnvironment(ctx context.Context, env Environment) error
}

// Engine is every seed port.
type Engine interface {
	Grapher
	Reader
	Planner
	Applier
	Pruner
	Environments
}
