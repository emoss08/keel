package seed

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/emoss08/keel"
)

// job is one seed a run executes.
type job struct {
	item    PlanItem
	prep    *prepared
	index   int
	total   int
	deps    int
	next    []*job
	started bool
	res     *Result
}

func (j *job) event(kind EventKind) Event {
	return Event{Kind: kind, Seed: j.prep.seed, Reason: j.item.Reason, Index: j.index, Total: j.total}
}

// execute runs the plan's seeds, filling their results.
func (a *applier) execute(ctx context.Context, gr *graph, plan Plan, results []Result) error {
	bySeed := map[*Seed]*job{}
	var jobs []*job
	for i, it := range plan.Seeds {
		if !it.Run {
			continue
		}
		j := &job{item: it, prep: gr.bySeed[it.Seed], index: len(jobs), res: &results[i]}
		bySeed[it.Seed] = j
		jobs = append(jobs, j)
	}
	for _, j := range jobs {
		j.total = len(jobs)
		for _, s := range j.item.Seed.After {
			if dep, ok := bySeed[s]; ok {
				j.deps++
				dep.next = append(dep.next, j)
			}
		}
	}
	return a.schedule(ctx, jobs)
}

// schedule starts each job once the jobs it runs after have finished, up to Parallel at once and
// lowest index first, and starts nothing more once one fails.
func (a *applier) schedule(ctx context.Context, jobs []*job) error {
	done := make(chan *job)
	running := 0
	var errs []error
	for {
		for running < cap(a.slots) && len(errs) == 0 {
			j := firstReady(jobs)
			if j == nil {
				break
			}
			j.started = true
			running++
			go func() {
				a.runSeed(ctx, j)
				done <- j
			}()
		}
		if running == 0 {
			break
		}
		j := <-done
		running--
		if j.res.Err != nil {
			errs = append(errs, j.res.Err)
			continue
		}
		for _, n := range j.next {
			n.deps--
		}
	}
	for _, j := range jobs {
		if !j.started {
			j.res.Err = ErrAborted
		}
	}
	return errors.Join(errs...)
}

func firstReady(jobs []*job) *job {
	for _, j := range jobs {
		if !j.started && j.deps == 0 {
			return j
		}
	}
	return nil
}

func (a *applier) runSeed(ctx context.Context, j *job) {
	start := time.Now()
	if j.prep.seed.PerTenant {
		a.runPerTenant(ctx, j, start)
		return
	}
	if err := a.acquire(ctx); err != nil {
		a.emit(j.event(SeedStart))
		a.finish(j, start, 0, 0, 0, err)
		return
	}
	defer a.release()
	a.emit(j.event(SeedStart))
	r := a.newRun(j.prep, Tenant{})
	err := a.transact(a.system(ctx), r, func(ctx context.Context) error {
		return writeSeed(ctx, a.db, j.prep, time.Since(start), r.Rows())
	})
	var rows int64
	if err == nil {
		rows = r.Rows()
	}
	a.finish(j, start, rows, 0, 0, err)
}

// finish records a seed's result and reports its end. rows counts committed rows only.
func (a *applier) finish(j *job, start time.Time, rows int64, tenants, applied int, err error) {
	if err != nil {
		err = fmt.Errorf("seed: %s: %w", j.prep.seed.Name, err)
	}
	d := time.Since(start)
	*j.res = Result{Seed: j.prep.seed, Name: j.prep.seed.Name, Reason: j.item.Reason, Ran: err == nil,
		Tenants: applied, Rows: rows, Duration: d, Err: err}
	e := j.event(SeedEnd)
	e.Tenants, e.TenantsDone, e.Rows, e.Duration, e.Err = tenants, applied, rows, d, err
	a.emit(e)
}

// runPerTenant runs a PerTenant seed for each tenant not yet applied with its fingerprint, then
// records the seed once every tenant succeeded.
func (a *applier) runPerTenant(ctx context.Context, j *job, start time.Time) {
	sys := a.system(ctx)
	pending, err := a.pendingTenants(ctx, sys, j)
	if err != nil {
		a.emit(j.event(SeedStart))
		a.finish(j, start, 0, 0, 0, err)
		return
	}
	e := j.event(SeedStart)
	e.Tenants = len(pending)
	a.emit(e)
	tr := &tenantRun{a: a, j: j, tenants: pending}
	err = tr.run(ctx)
	rows, applied := tr.rows.Load(), int(tr.applied.Load())
	if err == nil {
		err = a.record(sys, j.prep, time.Since(start), rows)
	}
	a.finish(j, start, rows, len(pending), applied, err)
}

// record writes a PerTenant seed's bookkeeping row in its own transaction.
func (a *applier) record(ctx context.Context, p *prepared, d time.Duration, rows int64) error {
	if err := a.acquire(ctx); err != nil {
		return err
	}
	defer a.release()
	return keel.Write(ctx, a.db, func(ctx context.Context) error {
		return writeSeed(ctx, a.db, p, d, rows)
	})
}

// pendingTenants lists the tenants a PerTenant seed runs for: every tenant for a forced run,
// otherwise those not yet applied with the seed's fingerprint.
func (a *applier) pendingTenants(ctx, sys context.Context, j *job) ([]Tenant, error) {
	done := map[string]bool{}
	if j.item.Reason != Forced {
		var err error
		if done, err = doneTenants(ctx, a.db.Pool(), j.prep); err != nil {
			return nil, err
		}
	}
	seen := map[string]bool{}
	var pending []Tenant
	var errs []error
	for t, err := range a.opts.Tenants(sys, a.db) {
		if err != nil {
			return nil, fmt.Errorf("list tenants: %w", err)
		}
		switch {
		case t.Key == "":
			errs = append(errs, fmt.Errorf("tenant %q has no Key", t.Name))
		case seen[t.Key]:
			errs = append(errs, fmt.Errorf("tenant %q is listed twice", t.Key))
		case t.Context == nil:
			errs = append(errs, fmt.Errorf("tenant %q has no Context", t.Key))
		case !done[t.Key]:
			pending = append(pending, t)
		}
		seen[t.Key] = true
	}
	return pending, errors.Join(errs...)
}

// tenantRun runs one PerTenant seed across its tenants, up to Parallel at once, and dispenses no
// tenant after one fails.
type tenantRun struct {
	a       *applier
	j       *job
	tenants []Tenant
	next    atomic.Int64
	done    atomic.Int64
	applied atomic.Int64
	rows    atomic.Int64
	failed  atomic.Bool
	mu      sync.Mutex
	errs    []error
}

func (t *tenantRun) run(ctx context.Context) error {
	var wg sync.WaitGroup
	for range min(cap(t.a.slots), len(t.tenants)) {
		wg.Go(func() { t.work(ctx) })
	}
	wg.Wait()
	return errors.Join(t.errs...)
}

func (t *tenantRun) work(ctx context.Context) {
	for !t.failed.Load() {
		i := int(t.next.Add(1)) - 1
		if i >= len(t.tenants) {
			return
		}
		if err := t.one(ctx, t.tenants[i]); err != nil {
			t.failed.Store(true)
			t.mu.Lock()
			t.errs = append(t.errs, err)
			t.mu.Unlock()
		}
	}
}

// one runs the seed for one tenant, in the tenant's scope, recording the tenant in the same
// transaction.
func (t *tenantRun) one(ctx context.Context, tn Tenant) error {
	a := t.a
	if err := a.acquire(ctx); err != nil {
		return fmt.Errorf("tenant %q: %w", tn.Key, err)
	}
	defer a.release()
	e := t.j.event(TenantStart)
	e.Tenant, e.Tenants, e.TenantsDone = tn.Key, len(t.tenants), int(t.done.Load())
	a.emit(e)
	start := time.Now()
	r := a.newRun(t.j.prep, tn)
	err := a.transact(tn.Context(ctx), r, func(ctx context.Context) error {
		return writeTenant(ctx, a.db, t.j.prep, tn.Key)
	})
	var rows int64
	if err != nil {
		err = fmt.Errorf("tenant %q: %w", tn.Key, err)
	} else {
		rows = r.Rows()
		t.rows.Add(rows)
		t.applied.Add(1)
	}
	e = t.j.event(TenantEnd)
	e.Tenant, e.Tenants, e.TenantsDone = tn.Key, len(t.tenants), int(t.done.Add(1))
	e.Rows, e.Duration, e.Err = rows, time.Since(start), err
	a.emit(e)
	return err
}
