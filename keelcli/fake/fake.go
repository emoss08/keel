// Package fake is an in-memory migration engine, seed engine and database that implements every
// keelcli port with simulated timing.
//
// It backs keelcli's tests and its demo: migrations that take time, a notx migration that reports
// its statements, a migration lock held by another deploy, a migration that waits on a long report
// query and retries, per-tenant seeds advancing through thousands of tenants, and failure paths.
// State changes as commands run, so a console refresh after an apply shows the new state.
package fake

import (
	"context"
	"sync"
	"time"

	"github.com/emoss08/keel/keelcli/factsport"
	"github.com/emoss08/keel/keelcli/migrateport"
	"github.com/emoss08/keel/keelcli/seedport"
)

// Options shapes the simulation.
type Options struct {
	Speed         float64
	Now           func() time.Time
	Environment   seedport.Environment
	Tenants       int
	Drift         bool
	FailMigration bool
	FailSeed      bool
	NoLockWait    bool
}

// Engine is the fake's shared state; Migrations, Seeds and Facts implement the ports over it and
// are safe for concurrent use.
type Engine struct {
	opts    Options
	created time.Time

	mu         sync.Mutex
	migrations []*migration
	seeds      []*seed
	orphans    []seedport.Status
	templates  []migrateport.Template
	env        seedport.Environment
	stamped    string
}

// Migrations is the fake's migration engine.
type Migrations struct{ e *Engine }

// Seeds is the fake's seed engine.
type Seeds struct{ e *Engine }

// Facts is the fake's database facts.
type Facts struct{ e *Engine }

var (
	_ migrateport.Engine = Migrations{}
	_ seedport.Engine    = Seeds{}
	_ factsport.Facts    = Facts{}
)

// Migrations returns the migration engine.
func (e *Engine) Migrations() Migrations { return Migrations{e} }

// Seeds returns the seed engine.
func (e *Engine) Seeds() Seeds { return Seeds{e} }

// Facts returns the database facts.
func (e *Engine) Facts() Facts { return Facts{e} }

// New returns a fake with the demo data set. Speed scales simulated time: 1 is real time, 0 runs
// instantly with the same reported durations.
func New(opts Options) *Engine {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Tenants == 0 {
		opts.Tenants = 9000
	}
	if opts.Environment == "" {
		opts.Environment = "development"
	}
	e := &Engine{opts: opts, created: opts.Now(), env: opts.Environment}
	e.migrations = demoMigrations(e.created, opts)
	e.seeds = demoSeeds(e.created, opts)
	e.orphans = demoOrphans(e.created)
	e.templates = demoTemplates(e.created)
	for _, m := range e.migrations {
		e.stamped = max(e.stamped, m.m.Version)
	}
	return e
}

func (e *Engine) sleep(ctx context.Context, d time.Duration) error {
	if e.opts.Speed <= 0 || d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(time.Duration(float64(d) * e.opts.Speed))
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
