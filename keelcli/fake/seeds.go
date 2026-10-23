package fake

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/emoss08/keel/keelcli/seedport"
)

const tenantBatch = 50

// Graph lists the registered seeds in dependency order.
func (x Seeds) Graph() []seedport.Seed {
	e := x.e
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]seedport.Seed, 0, len(e.seeds))
	for _, s := range e.seeds {
		out = append(out, s.s)
	}
	return out
}

func (e *Engine) seedStatusLocked() []seedport.Status {
	out := make([]seedport.Status, 0, len(e.seeds)+len(e.orphans))
	for _, s := range e.seeds {
		reason := e.reasonLocked(s, false)
		st := seedport.Status{
			Seed:      s.s,
			Reason:    reason,
			AppliedAt: s.appliedAt,
			Duration:  s.duration,
			Rows:      s.rows,
			Tenants:   s.tenantsDone,
		}
		switch {
		case reason.Runs() && s.s.PerTenant && s.tenantsDone > 0 && s.tenantsDone < s.tenants:
			st.State = seedport.Partial
		case reason.Runs() || reason.Kind == seedport.Refused:
			st.State = seedport.Due
		default:
			st.State = seedport.Applied
		}
		out = append(out, st)
	}
	return append(out, e.orphans...)
}

func (e *Engine) reasonLocked(s *seed, force bool) seedport.Reason {
	if s.s.Kind == seedport.Fixture {
		switch {
		case e.env == seedport.Production:
			return seedport.Reason{Kind: seedport.Refused}
		case force:
			return seedport.Reason{Kind: seedport.Forced}
		case s.applied:
			return seedport.Reason{Kind: seedport.FixtureApplied}
		default:
			return seedport.Reason{Kind: seedport.New}
		}
	}
	if force {
		return seedport.Reason{Kind: seedport.Forced}
	}
	return s.reason
}

// Plan lists every seed with the reason it runs or does not; seeds outside opts.Only and their
// dependencies are excluded.
func (x Seeds) Plan(_ context.Context, opts seedport.PlanOptions) ([]seedport.PlanItem, error) {
	e := x.e
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.seedPlanLocked(opts.Only, opts.Force)
}

func (e *Engine) seedPlanLocked(only []string, force bool) ([]seedport.PlanItem, error) {
	selected, err := e.closureLocked(only)
	if err != nil {
		return nil, err
	}
	out := make([]seedport.PlanItem, 0, len(e.seeds))
	for _, s := range e.seeds {
		reason := seedport.Reason{Kind: seedport.Excluded}
		if selected == nil || selected[s.s.Name] {
			reason = e.reasonLocked(s, force && (selected == nil || slices.Contains(only, s.s.Name)))
		}
		out = append(out, seedport.PlanItem{Seed: s.s, Reason: reason})
	}
	return out, nil
}

func (e *Engine) closureLocked(only []string) (map[string]bool, error) {
	if len(only) == 0 {
		return nil, nil
	}
	byName := map[string]*seed{}
	for _, s := range e.seeds {
		byName[s.s.Name] = s
	}
	selected := map[string]bool{}
	var visit func(string) error
	visit = func(name string) error {
		s, ok := byName[name]
		if !ok {
			return fmt.Errorf("fake: unknown seed %q", name)
		}
		if selected[name] {
			return nil
		}
		selected[name] = true
		for _, dep := range s.s.After {
			if err := visit(dep); err != nil {
				return err
			}
		}
		return nil
	}
	for _, name := range only {
		if err := visit(name); err != nil {
			return nil, err
		}
	}
	return selected, nil
}

// Status lists every registered seed with its state, then the recorded seeds no longer registered.
func (x Seeds) Status(context.Context) ([]seedport.Status, error) {
	e := x.e
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.seedStatusLocked(), nil
}

// Apply runs the seeds the plan selects, one tenant transaction at a time for per-tenant seeds,
// resuming a per-tenant seed where its last run stopped.
func (x Seeds) Apply(ctx context.Context, opts seedport.ApplyOptions, observe seedport.Observer) error {
	e := x.e
	e.mu.Lock()
	plan, err := e.seedPlanLocked(opts.Only, opts.Force)
	e.mu.Unlock()
	if err != nil {
		return err
	}
	var run []*seed
	for i, p := range plan {
		s := p.Seed
		observe(seedport.Event{Kind: seedport.Planned, Seed: &s, Reason: p.Reason, Index: i + 1, Total: len(plan)})
		if p.Reason.Runs() {
			run = append(run, e.seedNamed(s.Name))
		}
	}
	wait := 400 * time.Millisecond
	observe(seedport.Event{Kind: seedport.LockWait})
	if err := e.sleep(ctx, wait); err != nil {
		observe(seedport.Event{Kind: seedport.RunEnd, Err: err})
		return err
	}
	observe(seedport.Event{Kind: seedport.LockAcquired, Duration: wait})
	total := wait
	for i, s := range run {
		d, err := e.runSeed(ctx, s, plan, i+1, len(run), observe)
		total += d
		if err != nil {
			observe(seedport.Event{Kind: seedport.RunEnd, Total: i, Duration: total, Err: err})
			return err
		}
	}
	observe(seedport.Event{Kind: seedport.RunEnd, Total: len(run), Duration: total})
	return nil
}

func (e *Engine) seedNamed(name string) *seed {
	for _, s := range e.seeds {
		if s.s.Name == name {
			return s
		}
	}
	return nil
}

func (e *Engine) runSeed(ctx context.Context, s *seed, plan []seedport.PlanItem, index, total int, observe seedport.Observer) (time.Duration, error) {
	def := s.s
	reason := seedport.Reason{}
	for _, p := range plan {
		if p.Seed.Name == def.Name {
			reason = p.Reason
		}
	}
	tenants := 0
	if def.PerTenant {
		tenants = e.opts.Tenants
	}
	observe(seedport.Event{Kind: seedport.SeedStart, Seed: &def, Reason: reason, Index: index, Total: total, Tenants: tenants})
	var d time.Duration
	var rows int64
	var err error
	if def.PerTenant {
		d, rows, err = e.runTenants(ctx, s, &def, index, total, observe)
	} else {
		d, rows = s.script.duration, s.script.rows
		err = e.sleep(ctx, d)
	}
	observe(seedport.Event{Kind: seedport.SeedEnd, Seed: &def, Index: index, Total: total, Tenants: tenants, Rows: rows, Duration: d, Err: err})
	if err != nil {
		return d, err
	}
	e.mu.Lock()
	s.applied, s.appliedAt, s.duration, s.rows = true, e.opts.Now(), d, rows
	if s.s.Kind == seedport.Reference {
		s.reason = seedport.Reason{Kind: seedport.UpToDate}
	}
	e.mu.Unlock()
	return d, nil
}

func (e *Engine) runTenants(ctx context.Context, s *seed, def *seedport.Seed, index, total int, observe seedport.Observer) (time.Duration, int64, error) {
	e.mu.Lock()
	n := e.opts.Tenants
	start := 0
	if s.tenantsDone > 0 && s.tenantsDone < n {
		start = s.tenantsDone
	}
	s.tenants = n
	e.mu.Unlock()
	per := s.script.duration / time.Duration(n)
	var rows int64
	var elapsed time.Duration
	for t := start; t < n; t++ {
		tenant := fmt.Sprintf("org_%05d", t+1)
		observe(seedport.Event{Kind: seedport.TenantStart, Seed: def, Index: index, Total: total, Tenant: tenant, Tenants: n, TenantsDone: t, Rows: rows})
		if (t+1)%tenantBatch == 0 {
			if err := e.sleep(ctx, per*tenantBatch); err != nil {
				e.recordTenants(s, t)
				return elapsed, rows, err
			}
		}
		elapsed += per
		if s.script.failAt == t+1 {
			err := fmt.Errorf("tenant %s: %w", tenant, errDuplicate)
			observe(seedport.Event{Kind: seedport.TenantEnd, Seed: def, Index: index, Total: total, Tenant: tenant, Tenants: n, TenantsDone: t, Rows: rows, Err: err})
			e.recordTenants(s, t)
			e.mu.Lock()
			s.script.failAt = 0
			e.mu.Unlock()
			return elapsed, rows, err
		}
		rows += s.script.rows
		observe(seedport.Event{Kind: seedport.TenantEnd, Seed: def, Index: index, Total: total, Tenant: tenant, Tenants: n, TenantsDone: t + 1, Rows: rows})
	}
	e.recordTenants(s, n)
	return elapsed, rows, nil
}

var errDuplicate = errors.New(`ERROR: duplicate key value violates unique constraint "role_permissions_role_id_resource_key" (SQLSTATE 23505)`)

func (e *Engine) recordTenants(s *seed, done int) {
	e.mu.Lock()
	s.tenantsDone = done
	e.mu.Unlock()
}

// Prune deletes the records of seeds that are no longer registered.
func (x Seeds) Prune(context.Context) ([]seedport.Status, error) {
	e := x.e
	e.mu.Lock()
	defer e.mu.Unlock()
	pruned := e.orphans
	e.orphans = nil
	return pruned, nil
}

// Environment reads the database's environment mark.
func (x Seeds) Environment(context.Context) (seedport.Environment, error) {
	e := x.e
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.env, nil
}

// MarkEnvironment records the database's environment mark.
func (x Seeds) MarkEnvironment(_ context.Context, env seedport.Environment) error {
	e := x.e
	e.mu.Lock()
	defer e.mu.Unlock()
	e.env = env
	return nil
}

func (e *Engine) resetSeedsLocked(now time.Time) {
	for _, s := range e.seeds {
		if s.s.Kind == seedport.Fixture {
			s.applied, s.appliedAt, s.duration, s.rows = false, time.Time{}, 0, 0
			continue
		}
		s.applied, s.appliedAt = true, now
		s.reason = seedport.Reason{Kind: seedport.UpToDate}
		if s.s.PerTenant {
			s.tenants, s.tenantsDone = e.opts.Tenants, e.opts.Tenants
		}
	}
}
