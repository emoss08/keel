package seed

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"runtime/debug"
	"strings"
	"time"

	"github.com/emoss08/keel"
	"golang.org/x/sync/semaphore"
)

// ErrRefused is returned when a run would apply Fixture seeds to a database marked production, or
// with Options.Environment set to Production. Nothing runs.
var ErrRefused = errors.New("seed: fixtures refused on a production database")

// ErrAborted is the error of a seed a run did not start because another seed failed.
var ErrAborted = errors.New("seed: not run because another seed failed")

// Tenant is one tenant a PerTenant seed runs for.
type Tenant struct {
	// Key identifies the tenant in keel_seed_tenants; it must be unique and stable.
	Key string
	// Name is the tenant's name, for output.
	Name string
	// Context returns ctx with the tenant's scope, the one keel.Config.Tenant reads.
	Context func(ctx context.Context) context.Context
}

// Options configures Apply and Plan.
type Options struct {
	// Environment is the environment the caller expects. A run against a database marked with
	// another one fails with ErrEnvironment; an unmarked database is marked with it. Empty skips
	// the check, but fixtures are still refused on a database marked Production.
	Environment Env
	// Tenants lists the tenants PerTenant seeds run for. It is called with the System context at
	// the start of each PerTenant seed, so tenants an earlier seed created are included.
	Tenants func(ctx context.Context, db *keel.DB) iter.Seq2[Tenant, error]
	// System returns ctx with the application's system scope, the one keel.Config.System reads.
	// Seeds that are not PerTenant run under it, and so does the bookkeeping.
	System func(ctx context.Context) context.Context
	// Parallel is the number of transactions that may run at once: independent seeds, and tenants
	// of PerTenant seeds. Defaults to 1. Concurrency is opt-in because seeds that touch the same
	// tables can deadlock or conflict when run together, Run functions may share Go state, and the
	// pool needs Parallel + 1 connections (one holds the seed lock).
	Parallel int
	// Only limits the run to the seeds named and the seeds they run After.
	Only []string
	// Force runs the seeds named in Only, or every seed without Only, whatever their state, except
	// fixtures on a production database. A forced PerTenant seed runs for every tenant.
	Force bool
	// DryRun plans the run and reports it without taking the lock or writing anything.
	DryRun bool
	// Observer receives progress events. It is called from the goroutine running the seed, so with
	// Parallel above 1 from several at once: it must be safe for concurrent use and must not block.
	Observer func(Event)
	// Transact runs each seed transaction (a seed with its bookkeeping row, one tenant of a
	// PerTenant seed, a PerTenant seed's final bookkeeping row) instead of keel.Write: it begins
	// a transaction, calls fn with a context that runs keel in it and commits when fn returns
	// nil. An application that also writes through another data layer begins that layer's
	// transaction, joins it with keel.JoinTx, calls fn and commits, so a seed's rows and its
	// bookkeeping commit or roll back together with the other layer's writes. ctx carries the
	// seed's scope (the system scope, or the tenant's). Nil uses keel.Write. The seed lock is a
	// session lock on a connection of its own and needs no transaction.
	Transact func(ctx context.Context, fn func(ctx context.Context) error) error
}

// Result is what a run did with one seed.
type Result struct {
	// Seed is the registered seed.
	Seed *Seed
	// Name is the seed's name.
	Name string
	// Reason is why the seed ran or did not.
	Reason Reason
	// Ran reports whether the seed ran to completion and was recorded.
	Ran bool
	// Tenants is the number of tenants a PerTenant seed applied in this run.
	Tenants int
	// Rows is the number of rows reported through Run.Count.
	Rows int64
	// Duration is how long the seed ran.
	Duration time.Duration
	// Err is why the seed failed, or ErrAborted when another seed's failure stopped the run first.
	Err error
}

// Report is what a run did.
type Report struct {
	// Environment is the database's environment after the run; empty when it is not marked.
	Environment Env
	// Results lists every registered seed in dependency order.
	Results []Result
	// Duration is how long the run took, including waiting for the lock.
	Duration time.Duration
}

// Apply runs the seeds whose plan says they run, in After order, each in one transaction together
// with its bookkeeping row. It holds the seed advisory lock for the whole run, so two processes
// never seed at once; the second waits, reporting LockWait events. The bookkeeping tables are
// created on first use.
//
// A failing seed rolls back its writes and its bookkeeping. Seeds already running finish, no other
// seed starts, and Apply returns every seed's error joined. The report is filled as far as the run
// got, even when Apply returns an error.
func (g *Registry) Apply(ctx context.Context, db *keel.DB, opts Options) (Report, error) {
	start := time.Now()
	a := newApplier(db, opts)
	rep, err := a.apply(ctx, g.list())
	rep.Duration = time.Since(start)
	a.emit(Event{Kind: RunEnd, Duration: rep.Duration, Err: err})
	return rep, err
}

// applier is one Apply call.
type applier struct {
	db       *keel.DB
	opts     Options
	parallel int
	slots    *semaphore.Weighted
	values   *values
	env      Env
}

func newApplier(db *keel.DB, opts Options) *applier {
	parallel := max(opts.Parallel, 1)
	return &applier{
		db:       db,
		opts:     opts,
		parallel: parallel,
		slots:    semaphore.NewWeighted(int64(parallel)),
		values:   &values{m: map[string]any{}},
	}
}

func (a *applier) emit(e Event) {
	if a.opts.Observer != nil {
		a.opts.Observer(e)
	}
}

func (a *applier) system(ctx context.Context) context.Context {
	if a.opts.System != nil {
		return a.opts.System(ctx)
	}
	return ctx
}

func (a *applier) apply(ctx context.Context, seeds []*Seed) (Report, error) {
	gr, err := prepare(seeds)
	if err != nil {
		return Report{}, err
	}
	if a.opts.DryRun {
		return a.dryRun(ctx, gr)
	}
	if a.db.Pool().Config().MaxConns < 2 {
		return Report{}, errors.New("seed: the pool needs at least 2 connections: one holds the seed lock")
	}
	l, err := lock(ctx, a.db.Pool(), a.emit)
	if err != nil {
		return Report{}, err
	}
	defer l.unlock(ctx)
	plan, err := a.begin(ctx, gr, l)
	if err != nil {
		return Report{}, err
	}
	a.emit(Event{Kind: Planned, Plan: &plan})
	rep := newReport(plan)
	if err := a.check(plan); err != nil {
		return rep, err
	}
	return rep, a.execute(ctx, gr, plan, rep.Results)
}

// begin creates the bookkeeping tables, checks and records the environment, and plans the run.
func (a *applier) begin(ctx context.Context, gr *graph, l *sessionLock) (Plan, error) {
	sys := a.system(ctx)
	st, err := loadState(ctx, l.conn)
	if err != nil {
		return Plan{}, err
	}
	if !st.ready {
		if err := createTables(sys, a.db); err != nil {
			return Plan{}, err
		}
	}
	if err := checkEnv(a.opts.Environment, st.env); err != nil {
		return Plan{}, err
	}
	if st.env == "" && a.opts.Environment != "" {
		if _, err := keel.Exec(sys, a.db, insertMeta, envKey, string(a.opts.Environment)); err != nil {
			return Plan{}, fmt.Errorf("seed: record the environment: %w", err)
		}
		st.env = a.opts.Environment
	}
	return a.plan(gr, st)
}

func (a *applier) plan(gr *graph, st *state) (Plan, error) {
	a.env = st.env
	if a.opts.Environment != "" {
		a.env = a.opts.Environment
	}
	return gr.plan(st, a.opts)
}

func (a *applier) dryRun(ctx context.Context, gr *graph) (Report, error) {
	st, err := loadState(ctx, a.db.Pool())
	if err != nil {
		return Report{}, err
	}
	plan, err := a.plan(gr, st)
	if err != nil {
		return Report{}, err
	}
	a.emit(Event{Kind: Planned, Plan: &plan})
	return newReport(plan), a.check(plan)
}

// check refuses a plan that runs fixtures on production, or PerTenant seeds without tenants.
func (a *applier) check(plan Plan) error {
	var refused, perTenant []string
	for _, it := range plan.Seeds {
		if it.Reason == Refused {
			refused = append(refused, it.Seed.Name)
		}
		if it.Run && it.Seed.PerTenant && a.opts.Tenants == nil {
			perTenant = append(perTenant, it.Seed.Name)
		}
	}
	var errs []error
	if len(refused) > 0 {
		errs = append(errs, fmt.Errorf("%w: %s", ErrRefused, strings.Join(refused, ", ")))
	}
	if len(perTenant) > 0 {
		errs = append(errs, fmt.Errorf("seed: Options.Tenants is not set, but PerTenant seeds run: %s",
			strings.Join(perTenant, ", ")))
	}
	return errors.Join(errs...)
}

func newReport(plan Plan) Report {
	rep := Report{Environment: plan.Environment, Results: make([]Result, len(plan.Seeds))}
	for i, it := range plan.Seeds {
		rep.Results[i] = Result{Seed: it.Seed, Name: it.Seed.Name, Reason: it.Reason}
	}
	return rep
}

// acquire takes one of the run's Parallel transaction slots.
func (a *applier) acquire(ctx context.Context) error { return a.slots.Acquire(ctx, 1) }

func (a *applier) release() { a.slots.Release(1) }

func (a *applier) newRun(p *prepared, t Tenant) *Run {
	return &Run{DB: a.db, Seed: p.seed, Tenant: t, Environment: a.env, prep: p, values: a.values}
}

// tx runs fn in one seed transaction: through Options.Transact, else keel.Write.
func (a *applier) tx(ctx context.Context, fn func(ctx context.Context) error) error {
	if a.opts.Transact != nil {
		return a.opts.Transact(ctx, fn)
	}
	return keel.Write(ctx, a.db, fn)
}

// transact runs a seed's Run in one seed transaction bounded by its Timeout, with record writing
// the bookkeeping in the same transaction. The caller holds a slot.
func (a *applier) transact(ctx context.Context, r *Run, record func(ctx context.Context) error) error {
	timeout := r.Seed.Timeout
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	err := a.tx(ctx, func(ctx context.Context) error {
		if err := call(ctx, r); err != nil {
			return err
		}
		return record(ctx)
	})
	if err != nil && timeout > 0 && errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("timed out after %s: %w", timeout, err)
	}
	return err
}

// call runs the seed's Run function, turning a panic into an error so the transaction rolls back.
func call(ctx context.Context, r *Run) (err error) {
	defer func() {
		if v := recover(); v != nil {
			err = fmt.Errorf("panic: %v\n%s", v, debug.Stack())
		}
	}()
	return r.Seed.Run(ctx, r)
}
